package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
)

// requestReadHintMax bounds speculative allocation from an untrusted
// Content-Length. Larger/unknown bodies still grow through the limited reader.
// This is an allocation guardrail, not a second request-size limit.
const requestReadHintMax = 1 << 20

// setResolvedAuth stamps the extracted upstream key into the target's
// configured auth header (name + prefix); an empty key stamps nothing. The
// one owner shared by the generic upstream request builder, the metadata
// fetchers, and the cursor identity headers. t.authHeader always carries the
// resolved header (Authorization unless the client names one), so there is no
// protocol-specific fallback, and buildUpstreamRequest additionally strips the
// client's forwarded credential before stamping.
func setResolvedAuth(h http.Header, t *target, key string) {
	if key != "" {
		h.Set(t.authHeader, t.authPrefix+key)
	}
}

func (s *Server) buildUpstreamRequest(ctx context.Context, r *http.Request, t *target, key string, body []byte) (*http.Request, error) {
	path := t.path
	if path == "" {
		// EscapedPath preserves the client's percent-encoding: Path alone
		// would turn %2F, %3F and %23 into path separators or query/fragment
		// delimiters before the upstream sees them.
		path = r.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := upstreamURL(t.baseURL, path, t.query, r.URL.RawQuery)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// NewRequest re-parses its URL string, where a literal "#" in RawQuery
	// becomes a fragment and truncates the query. Keep the structurally built
	// URL the transport will serialize instead.
	req.URL = u

	// Forward client headers, dropping proxy control, hop-by-hop, and the
	// upstream auth header (re-added below from the extracted key). Transport
	// mode headers (Accept-Encoding, Range) never pass through: the shared
	// transport must own gzip negotiation and auto-decompression, and the relay
	// always forwards a complete body, so a caller-supplied value would
	// desynchronize the framing. The final assembly strips them again after
	// every configured source.
	//
	// RFC 7230 §6.1: a header nominated by the Connection header is itself
	// hop-by-hop and must not be forwarded. The static isHopByHop list alone
	// would let a client smuggle an arbitrary header upstream by nominating it
	// (e.g. "Connection: X-Secret" + "X-Secret: …"), so strip nominated tokens too.
	nominated := connectionTokens(r.Header)
	for k, vs := range r.Header {
		// Inbound names are already canonical, so the direct predicates avoid
		// three lowercasing allocations per forwarded header.
		if isProxyControlHeaderCanonical(k) || isHopByHopCanonical(k) || nominated[k] || isTransportModeHeader(k) {
			continue
		}
		req.Header[k] = append([]string(nil), vs...)
	}

	// Place the upstream key into the provider's auth header. The strip is
	// passthrough-specific: forwarded client headers may carry a stale
	// credential, while the metadata and cursor builders start from fresh
	// header maps.
	req.Header.Del("Authorization")
	req.Header.Del(t.authHeader)
	setResolvedAuth(req.Header, t, key)

	// Provider-configured headers (e.g. mimicking a first-party client's wire
	// fingerprint) override client-forwarded values; the explicit per-request
	// X-Proxy-Headers injection map below still wins over them, and the
	// resolved request_overrides merge applies LAST of all - the operator's
	// config is the point of an override, so it wins over every other source.
	s.applyProviderHeaders(req.Header, t.provider)

	// Extra headers the app asked us to add (or override), filtered.
	for k, vs := range t.extraHeader {
		if isHopByHop(k) || isProxyControlHeader(k) || isTransportModeHeader(k) {
			continue
		}
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	// Scoped request overrides: the resolved merge for this request closes
	// the chain (its set/replace entries, then its removals), with the
	// credential safety belt inside applyOverrideHeaders.
	applyOverrideHeaders(req.Header, t.override, t.authHeader)
	// Transport-mode headers are invariant regardless of which source tried to
	// set them: the shared transport's own Accept-Encoding enables
	// auto-decompression, and Range would request a partial body the relay
	// cannot forward as a completion.
	stripTransportModeHeaders(req.Header)

	return req, nil
}

// applyProviderHeaders sets the provider's configured upstream headers
// (config providers.<label>.headers) on an upstream request. Template
// placeholders in values expand per request: {{uuid4}} mints a fresh UUID v4
// (the cursor x-request-id idiom), {{platform}} renders the rust-style
// "os; arch" pair of the machine the proxy runs on, and
// {{opencode-msg-id}}/{{opencode-ses-id}} mint fresh identifiers in the
// opencode CLI's wire format (the zen idiom). Every configured header
// is set, never deleted: empty values are rejected at config load
// (config.ValidHeaderValue plus the mapping's non-empty rule), so there is no
// delete feature - deny by default.
func (s *Server) applyProviderHeaders(h http.Header, provider string) {
	ov, ok := s.cfg().Providers[provider]
	if !ok || len(ov.Headers) == 0 {
		return
	}
	for name, value := range ov.Headers {
		h.Set(name, expandHeaderTemplates(value))
	}
}

// expandHeaderTemplates expands the per-request placeholders in a configured
// header value. Unknown {{...}} sequences pass through verbatim - only the
// documented placeholders are special.
func expandHeaderTemplates(v string) string {
	if !strings.Contains(v, "{{") {
		return v
	}
	v = strings.ReplaceAll(v, "{{uuid4}}", providerformat.UUID4())
	v = strings.ReplaceAll(v, "{{platform}}", rustPlatform())
	v = strings.ReplaceAll(v, "{{opencode-msg-id}}", opencodeID("msg_"))
	v = strings.ReplaceAll(v, "{{opencode-ses-id}}", opencodeID("ses_"))
	return v
}

// opencodeIDChars is the opencode CLI's identifier alphabet: 62 characters
// the CLI picks its random tail from (byte % 62 over the same set).
const opencodeIDChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	opencodeIDmu      sync.Mutex
	opencodeIDlastMS  int64
	opencodeIDcounter int64
)

// opencodeID mints an identifier in the opencode CLI's wire format: prefix
// plus 26 characters - 12 lowercase hex encoding the low 48 bits of
// (timestamp_ms * 4096 + counter), then 14 base62 characters of
// cryptographically random bytes. This ports the CLI's identifier generator
// (packages/schema identifier.ts) faithfully; upstream gates have been
// observed to validate the shape (12 hex + 14 base62), not the timestamp.
// The counter shares the CLI's same-millisecond monotonicity, so ids minted
// within one process never collide.
func opencodeID(prefix string) string {
	opencodeIDmu.Lock()
	ms := time.Now().UnixMilli()
	if ms != opencodeIDlastMS {
		opencodeIDlastMS = ms
		opencodeIDcounter = 0
	}
	opencodeIDcounter++
	value := uint64(ms)*0x1000 + uint64(opencodeIDcounter)
	opencodeIDmu.Unlock()
	tail := make([]byte, 14)
	// crypto/rand.Read cannot fail on the supported platforms; a zero tail
	// still matches the validated shape if it ever did.
	_, _ = rand.Read(tail)
	chars := make([]byte, 14)
	for i, b := range tail {
		chars[i] = opencodeIDChars[int(b)%62]
	}
	return prefix + fmt.Sprintf("%012x", value&(1<<48-1)) + string(chars)
}

// rustPlatform renders the platform pair the grok-build CLI puts in its
// User-Agent ("({os}; {arch})", rust std::env::consts spellings) for THIS
// process. Go's GOARCH names differ from rust's for two common platforms and
// are mapped so the string reads exactly like the CLI's.
func rustPlatform() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return runtime.GOOS + "; " + arch
}
