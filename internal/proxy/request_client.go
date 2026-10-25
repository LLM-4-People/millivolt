package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// remoteIP returns the client's IP (host part of RemoteAddr), honoring
// X-Forwarded-For when the proxy sits behind another proxy. The returned value
// is bounded at this owner so every caller and consumer shares the same cap.
func remoteIP(r *http.Request) string {
	ip := ""
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip = strings.TrimSpace(xff)
		if i := strings.Index(xff, ","); i > 0 {
			ip = strings.TrimSpace(xff[:i])
		}
	} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	} else {
		ip = r.RemoteAddr
	}
	return retainedText(ip, retainedClientIPMax)
}

// clientLang extracts the SDK language/runtime from x-stainless-* headers or
// the user agent. No hardcoded list of known languages: it derives from
// whatever headers are present. The returned value is bounded at this owner so
// every caller and consumer shares the same cap.
func clientLang(r *http.Request) string {
	lang := ""
	if v := strings.TrimSpace(r.Header.Get("X-Stainless-Lang")); v != "" {
		lang = v
	} else if v := strings.TrimSpace(r.Header.Get("X-Stainless-Runtime")); v != "" {
		lang = v
	} else {
		// Fall back to the user-agent's product token (e.g.
		// "openai-python/1.55" → "openai-python").
		ua := r.UserAgent()
		if i := strings.Index(ua, "/"); i > 0 {
			lang = ua[:i]
		} else if i := strings.Index(ua, " "); i > 0 {
			lang = ua[:i]
		} else {
			lang = ua
		}
	}
	return retainedText(lang, retainedClientLangMax)
}

// classifyClient derives the recorded client identity from the request
// headers. An explicit X-Proxy-Client label wins, except when it merely
// repeats the user agent's product name while the agent reports more detail
// ("opencode" beside "opencode/latest/2.0.26/cli"): the fuller user-agent
// identity is then the true one. Without a label the order is x-stainless-lang
// (OpenAI SDKs), then the user agent's product token, else "unknown". The
// returned value is bounded at this owner so every caller and consumer shares
// the same cap.
func classifyClient(r *http.Request) string {
	ua := strings.TrimSpace(r.UserAgent())
	if i := strings.Index(ua, " "); i > 0 {
		ua = ua[:i]
	}
	client := ""
	// Explicit override from the app.
	if v := strings.TrimSpace(r.Header.Get(hdrClient)); v != "" {
		client = v
		if ua != "" && ua != v {
			name := ua
			if i := strings.Index(name, "/"); i > 0 {
				name = name[:i]
			}
			if v == name {
				client = ua
			}
		}
	} else if v := strings.TrimSpace(r.Header.Get("X-Stainless-Lang")); v != "" {
		// OpenAI-compatible SDKs set x-stainless-lang (e.g. "python", "js", "go").
		client = v
		if pkg := strings.TrimSpace(r.Header.Get("X-Stainless-Package-Version")); pkg != "" {
			client = v + " " + pkg
		}
	} else if ua != "" {
		client = ua
	} else {
		client = "unknown"
	}
	return retainedText(client, retainedClientMax)
}

// clientMetaMaxLen caps a captured client-meta token. Safety guardrail: these
// are short SDK identity strings, never payloads.
const clientMetaMaxLen = 64

// clipClientMeta bounds one captured client-meta token through the shared
// rune-safe truncator. Invalid UTF-8 is coerced first so the retained identity
// is always valid text; normal short values pass through byte-identically.
func clipClientMeta(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return retainedText(strings.ToValidUTF8(s, "\uFFFD"), clientMetaMaxLen)
}

// fillClientMeta copies allowlisted client identity (X-Stainless-*) and
// proxy-routing config (format / timeout / per-request max-concurrency)
// onto the record. Lowest choke point: one call from ServeHTTP after the
// Record exists. Never copies keys or message content.
func fillClientMeta(r *http.Request, t *target, rec *metrics.Record) {
	if rec == nil || r == nil {
		return
	}
	m := metrics.ClientMeta{
		OS:         clipClientMeta(r.Header.Get("X-Stainless-OS")),
		Arch:       clipClientMeta(r.Header.Get("X-Stainless-Arch")),
		Runtime:    clipClientMeta(r.Header.Get("X-Stainless-Runtime")),
		RuntimeVer: clipClientMeta(r.Header.Get("X-Stainless-Runtime-Version")),
		PkgVer:     clipClientMeta(r.Header.Get("X-Stainless-Package-Version")),
	}
	if t != nil {
		if !isOpenAIWire(t.format) {
			m.Format = clipClientMeta(t.format)
		}
		if t.timeout > 0 {
			m.TimeoutMs = int(t.timeout / time.Millisecond)
		}
	}
	if v := strings.TrimSpace(r.Header.Get(hdrMaxConcurrency)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			m.MaxConc = n
		}
	}
	rec.ClientMeta = m
}

// extractKey returns the upstream API key from the request, preferring an
// explicit X-Proxy-Key header and falling back to the Authorization header.
func extractKey(r *http.Request) string {
	if k := r.Header.Get(hdrKey); k != "" {
		return k
	}
	auth := r.Header.Get("Authorization")
	// Auth-scheme is case-insensitive per RFC 7235 §2.1.
	const bearer = "bearer "
	if len(auth) > len(bearer) && strings.EqualFold(auth[:len(bearer)], bearer) {
		return strings.TrimSpace(auth[len(bearer):])
	}
	return strings.TrimSpace(auth)
}

// headerPresent reports whether the header is set at all (even to "").
func headerPresent(r *http.Request, name string) bool {
	_, ok := r.Header[textproto.CanonicalMIMEHeaderKey(name)]
	return ok
}

// proxyControlHeaderNames is the authoritative strip set for the routing
// boundary: every client-consumed routing/control header name, built from
// the named constants plus the documented defensive entries. Two derived
// lookups share this one list: a canonical-name set for parsed inbound
// headers (allocation-free on the hot path) and a case-folded set for
// operator-supplied names. A new routing header is one named constant plus
// one line here; the routing-header drift test fails when the two halves
// diverge.
var proxyControlHeaderNames = []string{
	hdrBaseURL, hdrAuthHeader, hdrAuthPrefix, hdrPath, hdrQuery,
	hdrHeaders, hdrKey, hdrRefreshToken, hdrProvider, hdrAccessToken,
	hdrTimeout, hdrFormat, hdrInFormat, hdrMaxConcurrency, hdrSession,
	hdrParentSession, hdrClient,
	hdrLimitConcurrency, hdrLimitRequests, hdrLimitTokens,
	// authorization and cookie are defensive strips without routing
	// constants: the upstream key rides only the provider's auth header,
	// rebuilt from the extracted key in buildUpstreamRequest, so the
	// client's raw Authorization (which may carry the operator-plane
	// credential) must never pass through; a client's cookies are this
	// proxy origin's browser state, never provider credentials.
	"authorization", "cookie",
}

var (
	proxyControlHeaders       = canonicalHeaderSet(proxyControlHeaderNames)
	proxyControlHeadersFolded = foldedHeaderSet(proxyControlHeaderNames)
)

func canonicalHeaderSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, name := range names {
		m[http.CanonicalHeaderKey(name)] = true
	}
	return m
}

func foldedHeaderSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, name := range names {
		m[strings.ToLower(name)] = true
	}
	return m
}

func isProxyControlHeader(h string) bool {
	return proxyControlHeadersFolded[strings.ToLower(h)]
}

// isProxyControlHeaderCanonical is the allocation-free lookup for header maps
// net/http has already canonicalized.
func isProxyControlHeaderCanonical(h string) bool {
	return proxyControlHeaders[h]
}

func copyResponseHeaders(dst, src http.Header) {
	nominated := connectionTokens(src)
	for k, vs := range src {
		// Drop hop-by-hop headers, Content-Length (so the client gets chunked
		// encoding), Content-Encoding (we decompress upstream bodies, so
		// forwarding it would cause clients to double-decompress), and
		// Set-Cookie (an upstream cookie must never enter the browser's
		// millivolt-origin jar, where it could shadow operator-plane state).
		if isHopByHopCanonical(k) || nominated[k] || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		// X-Proxy-Warnings is proxy-owned: it describes translation losses.
		// Never let an upstream header of that name reach the client or
		// overwrite the warning the translation path already stamped.
		if strings.EqualFold(k, hdrWarnings) {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func isHopByHop(h string) bool {
	return isHopByHopCanonical(http.CanonicalHeaderKey(strings.ToLower(h)))
}

// isHopByHopCanonical is the allocation-free form for canonical header names.
func isHopByHopCanonical(h string) bool {
	switch h {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// connectionTokens returns the canonical header names nominated by the
// request's Connection header. Per RFC 7230 §6.1 those nominated headers are
// themselves hop-by-hop and must not be forwarded upstream. nil when the
// header is absent, which is the common HTTP/1 no-nomination and every
// HTTP/2 case; a nil map lookup is valid.
func connectionTokens(h http.Header) map[string]bool {
	values := h.Values("Connection")
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out[http.CanonicalHeaderKey(tok)] = true
			}
		}
	}
	return out
}

func isEventStream(ct string) bool {
	return strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
}

// The audit identity belongs to this proxy, not the client. Reused client
// request IDs must not collapse pending lifecycles or replace durable rows.
// The original X-Request-Id still forwards unchanged in buildUpstreamRequest.
// requestIDEncoding is base32 with an ASCII-ordered alphabet ("0".."9",
// "A".."V"). RFC 4648's alphabet places digits after letters, so its encoded
// strings are not lexicographically ordered; this one is, which is what makes
// the time prefix a right-edge B-tree append.
var requestIDEncoding = base32.NewEncoding("0123456789ABCDEFGHIJKLMNOPQRSTUV").WithPadding(base32.NoPadding)

// requestID returns a 26-character opaque identifier whose leading six bytes
// are the request time. SQLite's TEXT primary key is a B-tree: random IDs make
// every insert seek and dirty a different leaf, while a time prefix appends at
// the right edge and keeps the hot leaf cached. Ten random bytes retain 80 bits
// of uniqueness within a millisecond; the format and length are unchanged from
// the previous fully random value, and IDs stay opaque to every consumer.
func requestID() string {
	var b [16]byte
	ms := time.Now().UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand failure is a platform fault; fall back to the random
		// portion only so the request still gets an ID (uniqueness retained).
		for i := 6; i < len(b); i++ {
			b[i] = byte(time.Now().UnixNano() >> (i * 3))
		}
	}
	return requestIDEncoding.EncodeToString(b[:])
}

func hashKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// headerIntValue parses the first non-empty of the named headers as an
// integer. Single canonical owner for "read a provider's integer header"
// (processing-time and rate-limit headers share it): the first present name
// decides - an unparseable value does not fall through to later names. ok
// reports whether a value was read.
func headerIntValue(h http.Header, names ...string) (val int, ok bool) {
	for _, name := range names {
		if v := h.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err == nil {
				return n, true
			}
			return 0, false
		}
	}
	return 0, false
}

// disconnectWriter wraps a downstream ResponseWriter so a write error records
// the client as gone (markClientGone: client_disconnected + 499 only while no
// error outcome is decided - matching the accounting the analyzer streaming
// paths do) and lets io.Copy abort on the failed write. Its Write also
// accumulates the accepted bytes into the record's ResponseBytes, so the
// raw-copy relay fallbacks (the io.Copy sites in relay.go) report
// response_bytes exactly like the explicit write loops.
type disconnectWriter struct {
	w   http.ResponseWriter
	rec *metrics.Record
}

func (d disconnectWriter) Write(p []byte) (int, error) {
	n, err := writeCounted(d.w, d.rec, p)
	if err != nil {
		markClientGone(d.rec)
	}
	return n, err
}

// firstNonEmpty returns the first non-empty string from the args.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// typeInvalidRequestError is the OpenAI error type the request trust
// boundaries stamp on client-side rejections (routing headers, body decode,
// token caps). One constant owns the spelling across every 400 surface.
const typeInvalidRequestError = "invalid_request_error"

// typeAPIError is OpenAI's generic error type: the unclassified default for
// upstream failures that carried no structured error envelope, and the type
// the proxy's own 502 surfaces (translation, metadata, degenerate
// completions) stamp. One constant owns the spelling across every 502
// surface.
const typeAPIError = "api_error"

// typeUpstreamUnreachable is the OpenAI error type for a transport-level
// upstream failure: no HTTP response at all (connect refused/reset, DNS,
// timeout awaiting headers). Shared by the send-failure epilogues and both
// in-band relay variants so the classification cannot drift.
const typeUpstreamUnreachable = "upstream_unreachable"

// upstreamErrorFields resolves the error type/message to surface for an
// upstream 4xx/5xx whose captured envelope carried neither: the
// unclassified default is OpenAI's api_error type plus the bare HTTP status.
// One owner for the defaulting the three upstream-error surfaces (the cursor
// bidi 4xx/5xx, the committed-SSE in-band error, and the pacer's in-band
// 4xx/5xx) previously hand-rolled independently.
func upstreamErrorFields(rec *metrics.Record, status int) (typ, msg string) {
	typ, msg = rec.ErrorType, rec.ErrorMsg
	if typ == "" {
		typ = typeAPIError
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream HTTP %d", status)
	}
	return typ, msg
}

// errInvalidHeader is the shared rejection error for a client-supplied
// routing or limit header whose value failed its parse: "invalid <header>
// <value>". One owner of the wording and of the bounded echo across the
// routing-header and X-Proxy-Limit-* trust boundaries.
func errInvalidHeader(name, value string) error {
	return fmt.Errorf("invalid %s %q", name, clientEcho(value))
}

func errJSON(typ, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
	return string(b)
}

// errJSONCode is errJSON plus a structured code: the degenerate-outcome 502s
// carry the canonical class (empty_completion/…) so clients can branch and
// retry on it exactly like any provider error code.
func errJSONCode(typ, msg, code string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
	return string(b)
}
