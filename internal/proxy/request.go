package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/publicsuffix"
)

// requestReadHintMax bounds speculative allocation from an untrusted
// Content-Length. Larger/unknown bodies still grow through the limited reader.
// This is an allocation guardrail, not a second request-size limit.
const requestReadHintMax = 1 << 20

// A decimal integer with fewer than 309 digits cannot overflow float64.
// Exponents are checked separately by the standard decoder.
const requestFloatDigits = 309

func (s *Server) buildUpstreamRequest(ctx context.Context, r *http.Request, t *target, key string, body []byte) (*http.Request, error) {
	path := t.path
	if path == "" {
		path = r.URL.Path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	targetURL := joinUpstreamURL(t.baseURL, path)
	u, err := url.Parse(targetURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", t.baseURL, err)
	}
	// Query override takes precedence; otherwise pass the client's query along.
	if t.query != "" {
		u.RawQuery = t.query
	} else {
		u.RawQuery = r.URL.RawQuery
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	// Forward client headers, dropping proxy control, hop-by-hop, and the
	// upstream auth header (re-added below from the extracted key). Drop
	// Accept-Encoding so the transport adds its own gzip preference and
	// auto-decompresses upstream responses (letting us analyze the plaintext
	// and relay readable bytes downstream).
	//
	// RFC 7230 §6.1: a header nominated by the Connection header is itself
	// hop-by-hop and must not be forwarded. The static isHopByHop list alone
	// would let a client smuggle an arbitrary header upstream by nominating it
	// (e.g. "Connection: X-Secret" + "X-Secret: …"), so strip nominated tokens too.
	nominated := connectionTokens(r.Header)
	for k, vs := range r.Header {
		if isProxyControlHeader(k) || isHopByHop(k) || nominated[strings.ToLower(k)] || strings.EqualFold(k, "Accept-Encoding") {
			continue
		}
		req.Header[k] = append([]string(nil), vs...)
	}

	// Place the upstream key into the provider's auth header.
	req.Header.Del("Authorization")
	req.Header.Del(t.authHeader)
	if key != "" {
		req.Header.Set(t.authHeader, t.authPrefix+key)
	}

	// Provider-configured headers (e.g. mimicking a first-party client's wire
	// fingerprint) override client-forwarded values; the explicit per-request
	// X-Proxy-Headers injection map below still wins over them.
	s.applyProviderHeaders(req.Header, t.provider)

	// Extra headers the app asked us to add (or override), filtered.
	for k, vs := range t.extraHeader {
		if isHopByHop(k) || isProxyControlHeader(k) {
			continue
		}
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	return req, nil
}

// applyProviderHeaders sets the provider's configured upstream headers
// (config providers.<label>.headers) on an upstream request. Template
// placeholders in values expand per request: {{uuid4}} mints a fresh UUID v4
// (the cursor x-request-id idiom) and {{platform}} renders the rust-style
// "os; arch" pair of the machine the proxy runs on. Every configured header
// is set, never deleted: empty values are rejected at config load
// (validHeaderValue), so there is no delete feature - deny by default.
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
// two documented placeholders are special.
func expandHeaderTemplates(v string) string {
	if !strings.Contains(v, "{{") {
		return v
	}
	v = strings.ReplaceAll(v, "{{uuid4}}", providerformat.UUID4())
	v = strings.ReplaceAll(v, "{{platform}}", rustPlatform())
	return v
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

// joinUpstreamURL joins a base URL and a request path without duplicating a
// shared leading path segment. Clients typically request "/v1/chat/completions"
// while operators variously set the base to "https://host" or
// "https://host/v1" - naive concatenation would yield ".../v1/v1/..." and 404.
// If the base's last path segment equals the request path's first segment, the
// duplicate is dropped. This is purely a path-join convenience; it never strips
// segments that differ.
func joinUpstreamURL(baseURL, path string) string {
	base := strings.TrimRight(baseURL, "/")
	bu, err := url.Parse(base)
	if err != nil {
		return base + path
	}
	baseSegs := strings.Split(strings.Trim(bu.Path, "/"), "/")
	pathSegs := strings.Split(strings.Trim(path, "/"), "/")
	// Drop the request path's first segment when it duplicates the base's last
	// non-empty segment (e.g. base ".../v1" + path "/v1/chat/completions").
	if len(baseSegs) > 0 && baseSegs[0] != "" && len(pathSegs) > 0 &&
		baseSegs[len(baseSegs)-1] == pathSegs[0] {
		path = "/" + strings.Join(pathSegs[1:], "/")
	}
	return base + path
}

// resolveTarget parses routing headers into an upstream target. Only
// X-Proxy-Base-URL is mandatory.
func resolveTarget(r *http.Request, cfg *config.Config) (*target, error) {
	baseURL := strings.TrimSpace(r.Header.Get(hdrBaseURL))
	if baseURL == "" {
		return nil, fmt.Errorf("missing %s header", hdrBaseURL)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid %s %q", hdrBaseURL, baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported %s scheme %q", hdrBaseURL, parsed.Scheme)
	}
	// The base must be exactly scheme://host[:port][/path] - no userinfo
	// (credentials would ride to the upstream and leak into error echoes), no
	// query or fragment (they collide with the path/query join). Deny by default.
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("invalid %s %q: userinfo, query, and fragment are not allowed in a base URL", hdrBaseURL, baseURL)
	}

	t := &target{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: "Authorization",
		authPrefix: "Bearer ",
		// The provider label is derived from the base URL, never supplied by
		// the client, so a given upstream always maps to the same identity in
		// the dashboard regardless of what each client calls it.
		provider: providerFromURL(parsed),
	}
	if h := strings.TrimSpace(r.Header.Get(hdrAuthHeader)); h != "" {
		if !isValidAuthHeader(h) {
			return nil, fmt.Errorf("invalid %s header %q", hdrAuthHeader, h)
		}
		t.authHeader = h
	}
	if p := r.Header.Get(hdrAuthPrefix); p != "" || headerPresent(r, hdrAuthPrefix) {
		// Explicitly allow an empty prefix (e.g. raw key with no "Bearer ").
		// The prefix is concatenated into the upstream auth header value, so it
		// must be a valid header value - reject invalid bytes at the boundary
		// rather than fail later as an opaque transport error.
		if !isValidHeaderValue(p) {
			return nil, fmt.Errorf("invalid %s value", hdrAuthPrefix)
		}
		t.authPrefix = p
	}
	if p := strings.TrimSpace(r.Header.Get(hdrPath)); p != "" {
		t.path = p
	}
	if q := strings.TrimSpace(r.Header.Get(hdrQuery)); q != "" {
		t.query = q
	}
	if f := strings.TrimSpace(r.Header.Get(hdrFormat)); f != "" {
		// Validate at the boundary so an unsupported format is rejected
		// consistently (400), not only when a translation happens to run.
		if f != "openai" && f != "anthropic" && f != "cursor" {
			return nil, fmt.Errorf("unsupported %s %q", hdrFormat, f)
		}
		t.format = f
	}
	if ms := r.Header.Get(hdrTimeout); ms != "" {
		d, err := time.ParseDuration(ms + "ms")
		if err != nil || d <= 0 {
			return nil, fmt.Errorf("invalid %s %q", hdrTimeout, ms)
		}
		t.timeout = d
	}
	if raw := r.Header.Get(hdrHeaders); raw != "" {
		var m map[string][]string
		if err := adminjson.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("invalid %s header: %v", hdrHeaders, err)
		}
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		if err := config.ValidateHeaderNames(names); err != nil {
			return nil, fmt.Errorf("invalid %s header: %w", hdrHeaders, err)
		}
		// Validate names/values at the boundary so a malformed header is a
		// clean 400 here, not a late transport write failure surfaced as a 502.
		for k, vs := range m {
			if !isValidAuthHeader(k) {
				return nil, fmt.Errorf("invalid %s header name %q", hdrHeaders, k)
			}
			for _, v := range vs {
				if !isValidHeaderValue(v) {
					return nil, fmt.Errorf("invalid %s value for header %q", hdrHeaders, k)
				}
			}
		}
		t.extraHeader = http.Header(m)
	}

	if len(cfg.AllowedBaseURLs) > 0 && !allowed(t.baseURL, cfg.AllowedBaseURLs) {
		return nil, fmt.Errorf("base URL %q not in allowed list", t.baseURL)
	}
	// provider_aliases re-key a derived label the operator has merged into a
	// canonical one. Applied here - the single choke point every
	// provider-scoped consumer flows through (record stamp, throttles, pause
	// holds, per-provider field maps, dashboard grouping) - so a merged label
	// is one entity everywhere at once.
	if canon := strings.TrimSpace(cfg.ProviderAliases[t.provider]); canon != "" {
		t.provider = canon
	}
	return t, nil
}

// providerFromURL derives the canonical provider label from the upstream base
// URL: the registrable domain (effective TLD + 1 label per the public suffix
// list). EVERY subdomain collapses to the root domain - api.openai.com →
// "openai.com"; api2.cursor.sh / api5.cursor.sh → "cursor.sh";
// inference.coralbricks.ai → "coralbricks.ai"; hyper.charm.land → "charm.land"
// - so all of a provider's gateway hosts group under one label. IPs (v4/v6)
// keep the authority with port so distinct self-hosted endpoints stay
// distinguishable; hosts without a derivable registrable domain (localhost,
// intranet names, bare suffixes) keep their hostname.
func providerFromURL(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "unknown"
	}
	if ip := net.ParseIP(host); ip != nil {
		// IPs have no meaningful service name; keep the authority (with port)
		// so distinct self-hosted endpoints stay distinguishable.
		return u.Host
	}
	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return domain
	}
	return host
}

func allowed(base string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(base, p) && (len(base) == len(p) ||
			base[len(p)] == '/' || base[len(p)] == '?' || base[len(p)] == ':') {
			return true
		}
	}
	return false
}

// isValidAuthHeader rejects header names that could be used to inject
// dangerous headers into upstream requests. Must match header-name grammar.
func isValidAuthHeader(h string) bool {
	if !config.ValidHeaderName(h) {
		return false
	}
	if isHopByHop(h) || isProxyControlHeader(h) || strings.EqualFold(h, "host") {
		return false
	}
	return true
}

// isValidHeaderValue reports whether s is a legal HTTP header field value
// (RFC 7230: visible ASCII, horizontal tab, space, and obs-text; no NUL/CR/LF
// or other control bytes). Used to validate client-supplied header material
// (auth prefix, X-Proxy-Headers values) at the routing boundary.
func isValidHeaderValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		// Allow HTAB(9), SP(32), VCHAR(33-126), and obs-text(128-255).
		if c == 9 || (c >= 32 && c != 127) {
			continue
		}
		return false
	}
	return true
}

// classifyClient derives a friendly client/app name from the request headers.
// It prefers an explicit X-Proxy-Client header, then x-stainless-lang (OpenAI
// SDKs), then the User-Agent.

// remoteIP returns the client's IP (host part of RemoteAddr), honoring
// X-Forwarded-For when the proxy sits behind another proxy.
func remoteIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.Index(xff, ","); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// clientLang extracts the SDK language/runtime from x-stainless-* headers or
// the user agent. No hardcoded list of known languages: it derives from
// whatever headers are present.
func clientLang(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Stainless-Lang")); v != "" {
		return v
	}
	if v := strings.TrimSpace(r.Header.Get("X-Stainless-Runtime")); v != "" {
		return v
	}
	// Fall back to the user-agent's product token (e.g. "openai-python/1.55" →
	// "openai-python").
	ua := r.UserAgent()
	if i := strings.Index(ua, "/"); i > 0 {
		return ua[:i]
	}
	if i := strings.Index(ua, " "); i > 0 {
		return ua[:i]
	}
	return ua
}

func classifyClient(r *http.Request) string {
	// Explicit override from the app.
	if v := strings.TrimSpace(r.Header.Get(hdrClient)); v != "" {
		return v
	}
	// OpenAI-compatible SDKs set x-stainless-lang (e.g. "python", "js", "go").
	if v := strings.TrimSpace(r.Header.Get("X-Stainless-Lang")); v != "" {
		if pkg := strings.TrimSpace(r.Header.Get("X-Stainless-Package-Version")); pkg != "" {
			return v + " " + pkg
		}
		return v
	}
	// Otherwise use the user-agent, normalized to its leading product token.
	ua := r.UserAgent()
	if ua == "" {
		return "unknown"
	}
	if i := strings.Index(ua, " "); i > 0 {
		ua = ua[:i]
	}
	return ua
}

// clientMetaMaxLen caps a captured client-meta token. Safety guardrail: these
// are short SDK identity strings, never payloads.
const clientMetaMaxLen = 64

func clipClientMeta(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if len(s) > clientMetaMaxLen {
		return s[:clientMetaMaxLen]
	}
	return s
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
		if t.format != "" && t.format != "openai" {
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

// isProxyControlHeader covers every client-consumed routing header so none of
// them can leak upstream - neither via header passthrough nor via an
// X-Proxy-Headers injection map. ("x-proxy-provider" is not read; it is
// stripped defensively so a client can never spoof a provider identity.
// "x-proxy-access-token" is proxy-ISSUED - it names a response signal, never
// an inbound one - and is stripped defensively so a stale or echoed fresh
// token can never ride upstream.)
func isProxyControlHeader(h string) bool {
	switch strings.ToLower(h) {
	case "authorization", "x-proxy-base-url", "x-proxy-auth-header",
		"x-proxy-auth-prefix", "x-proxy-path", "x-proxy-query", "x-proxy-headers",
		"x-proxy-provider", "x-proxy-key", "x-proxy-refresh-token",
		"x-proxy-access-token", "x-proxy-timeout-ms", "x-proxy-format",
		"x-proxy-max-concurrency", "x-proxy-session", "x-proxy-parent-session", "x-proxy-client",
		"x-proxy-limit-concurrency", "x-proxy-limit-requests", "x-proxy-limit-tokens":
		return true
	}
	return false
}

func copyResponseHeaders(dst, src http.Header) {
	nominated := connectionTokens(src)
	for k, vs := range src {
		// Drop hop-by-hop headers, Content-Length (so the client gets chunked
		// encoding), and Content-Encoding (we decompress upstream bodies, so
		// forwarding it would cause clients to double-decompress).
		if isHopByHop(k) || nominated[strings.ToLower(k)] || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func isHopByHop(h string) bool {
	switch strings.ToLower(h) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// connectionTokens returns the lowercased header names nominated by the
// request's Connection header. Per RFC 7230 §6.1 those nominated headers are
// themselves hop-by-hop and must not be forwarded upstream.
func connectionTokens(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, v := range h["Connection"] {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out[strings.ToLower(tok)] = true
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
func requestID() string { return rand.Text() }

func hashKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// headerIntInto parses the first non-empty of the named headers as an integer
// into dst. Single canonical owner for "read a provider's integer header into a
// metric field" (processing-time and rate-limit headers share it).
func headerIntInto(h http.Header, dst *int, names ...string) {
	for _, name := range names {
		if v := h.Get(name); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
			return
		}
	}
}

// disconnectWriter wraps a downstream ResponseWriter so a write error records
// the client as gone (markClientGone: client_disconnected + 499 only while no
// error outcome is decided - matching the accounting the analyzer streaming
// paths do) and lets io.Copy abort on the failed write.
type disconnectWriter struct {
	w   http.ResponseWriter
	rec *metrics.Record
}

func (d disconnectWriter) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
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

// translateRequest converts an OpenAI Chat Completions body into the target
// upstream format. Currently only "anthropic" is supported. defaultMaxTokens
// (from config) is injected when the client sent no token cap.
func translateRequest(format string, body []byte, defaultMaxTokens int) ([]byte, error) {
	switch format {
	case "anthropic":
		return providerformat.TranslateRequest(body, defaultMaxTokens)
	default:
		// Note: "cursor" is intentionally not here - serveCursorBidi translates
		// the body itself (it needs the history blobs for the KV channel and a
		// bare, unframed run_request).
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

// translateResponse converts an upstream response body from the provider
// format back to OpenAI Chat Completions.
func translateResponse(format string, body []byte) ([]byte, error) {
	switch format {
	case "anthropic":
		return providerformat.TranslateResponse(body)
	default:
		// "cursor" is served by serveCursorBidi, never translateResponse.
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

// readRequest preserves the exact body for forwarding/retries and extracts
// routing + observability metadata once. Content-Length is only a capped
// allocation hint: short/long actual bodies still follow the same byte limit.
func readRequest(r *http.Request, maxBytes int64, preview bool) (body []byte, rec *metrics.Record, err error) {
	rec = new(metrics.Record)
	if r.Body == nil {
		rec.Start = time.Now()
		return nil, rec, nil
	}
	defer r.Body.Close()
	reader := io.LimitReader(r.Body, maxBytes+1)
	if hint := min(r.ContentLength, maxBytes+1, requestReadHintMax); hint > 0 {
		body = make([]byte, int(hint))
		n := 0
		for n < len(body) {
			var got int
			got, err = reader.Read(body[n:])
			n += got
			if err != nil {
				break
			}
		}
		body = body[:n]
		if err != nil && err != io.EOF {
			return nil, nil, err
		}
		if err == nil {
			var rest []byte
			rest, err = io.ReadAll(reader)
			body = append(body, rest...)
		} else {
			err = nil
		}
	} else {
		body, err = io.ReadAll(reader)
	}
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, fmt.Errorf("request body exceeds %d bytes", maxBytes)
	}
	// Request timing excludes upload, but includes the unified metadata decode
	// and any later format translation. This is the sole Start owner.
	rec.Start = time.Now()
	parseLLMRequest(body, rec, preview)
	return body, rec, nil
}

// discardedJSON accepts every JSON shape like any without materializing
// nested schemas/metadata. Syntax is validated by the enclosing JSON decoder.
type discardedJSON struct{}

func (*discardedJSON) UnmarshalJSON(raw []byte) error {
	if requestNumberNeedsDecode(raw) {
		var discarded any
		return json.Unmarshal(raw, &discarded)
	}
	return nil
}

// This is only a conservative range-check trigger, not a JSON parser. The
// enclosing standard decoder owns syntax; unusual numeric candidates use its
// original any/float64 semantics. Matching inside strings is harmless.
func requestNumberNeedsDecode(raw []byte) bool {
	digits := 0
	for i, c := range raw {
		if c >= '0' && c <= '9' {
			digits++
			if digits >= requestFloatDigits {
				return true
			}
			continue
		}
		if digits > 0 && (c == 'e' || c == 'E') {
			next := i + 1
			if next < len(raw) && (raw[next] == '+' || raw[next] == '-') {
				next++
			}
			if next < len(raw) && raw[next] >= '0' && raw[next] <= '9' {
				return true
			}
		}
		digits = 0
	}
	return false
}

type requestRouting struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type requestMessage[C any] struct {
	Role    string `json:"role"`
	Content C      `json:"content"`
}

type requestParameters struct {
	MaxTokens      *int            `json:"max_tokens"`
	MaxCompTokens  *int            `json:"max_completion_tokens"`
	Temperature    *float64        `json:"temperature"`
	TopP           *float64        `json:"top_p"`
	Tools          []discardedJSON `json:"tools"`
	ToolChoice     json.RawMessage `json:"tool_choice"`
	StreamOptions  *struct{}       `json:"stream_options"`
	N              *int            `json:"n"`
	Stop           json.RawMessage `json:"stop"`
	Logprobs       bool            `json:"logprobs"`
	PresencePen    *float64        `json:"presence_penalty"`
	FrequencyPen   *float64        `json:"frequency_penalty"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
	Seed            *int64                   `json:"seed"`
	ParallelTools   *bool                    `json:"parallel_tool_calls"`
	LogitBias       map[string]discardedJSON `json:"logit_bias"`
	TopLogprobs     *int                     `json:"top_logprobs"`
	ServiceTier     string                   `json:"service_tier"`
	Thinking        json.RawMessage          `json:"thinking"`
	Metadata        map[string]discardedJSON `json:"metadata"`
	ReasoningEffort string                   `json:"reasoning_effort"`
	Verbosity       string                   `json:"verbosity"`
	Reasoning       json.RawMessage          `json:"reasoning"`
	// System carries the system prompt for Anthropic-shaped bodies (OpenAI
	// puts it in a role:"system"/"developer" message instead). Content only.
	System   requestContent                   `json:"system"`
	Messages []requestMessage[requestContent] `json:"messages"`
}

// parseLLMRequest is the single document decode for routing and metadata.
// Keep the two independent trust boundaries on type errors: a malformed
// metadata-only field must not erase valid model/stream routing, and vice
// versa. Only that exceptional path needs independent decoding; malformed
// JSON still passes through unchanged with no partially extracted metadata.
func parseLLMRequest(body []byte, rec *metrics.Record, preview bool) {
	var req struct {
		requestRouting
		requestParameters
	}
	if err := json.Unmarshal(body, &req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return
		}
		req.requestRouting = requestRouting{}
		if json.Unmarshal(body, &req.requestRouting) != nil {
			req.requestRouting = requestRouting{}
		}
		req.requestParameters = requestParameters{}
		if json.Unmarshal(body, &req.requestParameters) != nil {
			rec.Model, rec.Stream = req.Model, req.Stream
			return
		}
	}
	rec.Model, rec.Stream = req.Model, req.Stream
	if req.MaxTokens != nil {
		rec.ReqMaxTokens = req.MaxTokens
	} else if req.MaxCompTokens != nil {
		rec.ReqMaxTokens = req.MaxCompTokens
	}
	rec.ReqTemperature = req.Temperature
	rec.ReqTopP = req.TopP
	rec.ReqToolsCount = len(req.Tools)
	rec.ReqN = req.N
	rec.ReqPresencePen = req.PresencePen
	rec.ReqFrequencyPen = req.FrequencyPen
	rec.ReqLogprobs = req.Logprobs
	if req.StreamOptions != nil {
		rec.ReqStreamOpts = true
	}
	if len(req.ToolChoice) > 0 {
		var tcStr string
		if json.Unmarshal(req.ToolChoice, &tcStr) == nil {
			rec.ReqToolChoice = tcStr
		} else {
			var tcObj map[string]any
			if json.Unmarshal(req.ToolChoice, &tcObj) == nil {
				if fn, ok := tcObj["function"].(map[string]any); ok {
					if name, ok := fn["name"].(string); ok {
						rec.ReqToolChoice = "tool:" + name
					}
				}
			}
		}
	}
	if len(req.Stop) > 0 {
		var s string
		var arr []string
		if json.Unmarshal(req.Stop, &s) == nil {
			rec.ReqStop = 1
		} else if json.Unmarshal(req.Stop, &arr) == nil {
			rec.ReqStop = len(arr)
		}
	}
	// Additional non-content params: capture shape only, never values that
	// could carry content (logit_bias keys are token ids, metadata is counted
	// not copied).
	if req.ResponseFormat != nil {
		rec.ReqResponseFormat = req.ResponseFormat.Type
	}
	rec.ReqSeed = req.Seed
	rec.ReqParallelTools = req.ParallelTools
	rec.ReqTopLogprobs = req.TopLogprobs
	rec.ReqServiceTier = req.ServiceTier
	rec.ReqLogitBias = len(req.LogitBias)
	rec.ReqMetadataKeys = len(req.Metadata)
	if len(req.Thinking) > 0 {
		// Any non-null "thinking" object means extended thinking was requested.
		rec.ReqThinking = string(req.Thinking) != "null"
	}
	if tok := configToken(req.ReasoningEffort); tok != "" {
		rec.ReqReasoningEffort = tok
	} else if len(req.Reasoning) > 0 {
		var obj struct {
			Effort string `json:"effort"`
		}
		if json.Unmarshal(req.Reasoning, &obj) == nil {
			rec.ReqReasoningEffort = configToken(obj.Effort)
		}
	}
	rec.ReqVerbosity = configToken(req.Verbosity)
	// Prompt composition: per-role character sizes, multimodal counts, and the
	// role of the last turn (what the model is actually responding to). Sizes
	// count text characters only - never the text itself, and never inline
	// base64 payloads (an image part contributes its text siblings, not its
	// data). System size comes from a role:"system"/"developer" message
	// (OpenAI) and/or a top-level "system" field (Anthropic).
	rec.CharsSystem = req.System.chars
	rec.LastTurnRole = ""
	for _, m := range req.Messages {
		n := m.Content.chars
		switch m.Role {
		case "user":
			rec.TurnsUser++
			rec.CharsUser += n
		case "assistant":
			rec.TurnsAssistant++
			rec.CharsAssistant += n
		case "tool", "function":
			rec.TurnsTool++
			rec.CharsTool += n
		case "system", "developer":
			rec.CharsSystem += n
		}
		rec.Images += m.Content.images
		rec.Attachments += m.Content.attachments
		if m.Role != "" {
			rec.LastTurnRole = m.Role
		}
	}
	if preview && len(req.Messages) > 0 {
		// Content is never retained on the default path. Opt-in previews use
		// the same message shape with a bounded string-only content decoder.
		var captured struct {
			Messages []requestMessage[requestPreview] `json:"messages"`
		}
		if json.Unmarshal(body, &captured) != nil {
			return
		}
		for i := len(captured.Messages) - 1; i >= 0; i-- {
			if captured.Messages[i].Role == "user" {
				rec.PromptPreview = string(captured.Messages[i].Content)
				break
			}
		}
	}
}

// configToken accepts a short alphanumeric request-config token (effort,
// verbosity, …). Deny by default: anything with spaces, punctuation other
// than _/-, or a long payload is dropped rather than stored.
func configToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return s
}

// requestContent measures one message content field, retaining only its text
// character count plus the number of image and non-image attachment parts.
// Content may be a plain string or an array of typed parts; the part "type"
// discriminator is matched by substring so both OpenAI ("image_url",
// "input_image", "input_audio", "file") and Anthropic ("image", "document",
// "tool_result") shapes - and future input_* variants - bucket consistently.
// Text is not retained, and base64 payloads are never decoded or retained.
type requestContent struct {
	chars, images, attachments int
}

func (c *requestContent) UnmarshalJSON(content []byte) error {
	*c = requestContent{}
	if len(content) > 0 && content[0] == '"' {
		var size requestStringSize
		if size.UnmarshalJSON(content) == nil {
			c.chars = int(size)
		}
		return nil
	}
	var parts []struct {
		Type string            `json:"type"`
		Text requestStringSize `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return nil // content is best-effort, never a routing/type failure
	}
	for _, p := range parts {
		switch {
		case strings.Contains(p.Type, "image"):
			c.images++
		case p.Type == "text" || strings.HasPrefix(p.Type, "input_text"):
			c.chars += int(p.Text)
		case p.Type != "" && p.Type != "thinking" && p.Type != "redacted_thinking" && p.Type != "tool_use":
			// Any other non-text part is an attachment: file/document/audio/
			// video or an embedded tool result. Model-authored reasoning and
			// tool-call blocks are excluded - they are not user attachments.
			c.attachments++
		}
	}
	return nil
}

// requestStringBytes only borrows bytes during UnmarshalJSON. The enclosing
// encoding/json call already validated quoting/control bytes. Invalid UTF-8
// stays on the standard decoder's Unicode replacement path.
func requestStringBytes(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return nil, false
	}
	s := raw[1 : len(raw)-1]
	return s, utf8.Valid(s)
}

type requestStringSize int

func (n *requestStringSize) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return nil // a repeated null leaves a string field unchanged
	}
	if s, ok := requestStringBytes(raw); ok {
		size := len(s)
		for {
			i := bytes.IndexByte(s, '\\')
			if i < 0 {
				*n = requestStringSize(size)
				return nil
			}
			if s[i+1] == 'u' {
				break // the standard decoder owns Unicode/surrogate semantics
			}
			// Every other validated JSON escape is two bytes representing one
			// ASCII byte (newline, quote, slash, ...); no string allocation.
			size--
			s = s[i+2:]
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	*n = requestStringSize(len(s))
	return nil
}

type requestPreview string

func (p *requestPreview) UnmarshalJSON(raw []byte) error {
	*p = ""
	if s, ok := requestStringBytes(raw); ok && bytes.IndexByte(s, '\\') < 0 {
		// A few bytes beyond the cap let TruncatePreview own rune boundaries
		// and the ellipsis without allocating an entire large prompt string.
		s = s[:min(len(s), metrics.PreviewMaxBytes+utf8.UTFMax)]
		*p = requestPreview(metrics.TruncatePreview(string(s)))
	} else if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			*p = requestPreview(metrics.TruncatePreview(s))
		}
	}
	return nil
}
