package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"

	"golang.org/x/net/publicsuffix"
)

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

// upstreamURL builds the request target structurally. The path is already an
// escaped path (client request or validated X-Proxy-Path) and is never
// re-parsed as part of a concatenated URL, so an encoded ? or # cannot become
// a query or fragment delimiter.
func upstreamURL(baseURL, escapedPath, queryOverride, clientQuery string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", clientEcho(baseURL), err)
	}
	joined := joinUpstreamEscapedPath(base.EscapedPath(), escapedPath)
	decoded, err := url.PathUnescape(joined)
	if err != nil {
		return nil, fmt.Errorf("invalid request path: %w", err)
	}
	u := *base
	u.Path = decoded
	u.RawPath = joined
	// Query override takes precedence; otherwise pass the client's query along.
	if queryOverride != "" {
		u.RawQuery = queryOverride
	} else {
		u.RawQuery = clientQuery
	}
	return &u, nil
}

// joinUpstreamEscapedPath joins two escaped paths and drops the request path's
// first segment when it duplicates the base's last non-empty segment. Segment
// comparison uses the decoded spelling so an encoded character still matches;
// joining uses the escaped spelling so %2F never becomes a separator. Exactly
// one leading separator is consumed: every remaining empty segment (a doubled
// slash at the start, inside or at the end) is part of the client's path shape
// and is preserved.
func joinUpstreamEscapedPath(basePath, escapedPath string) string {
	base := strings.TrimRight(basePath, "/")
	if !strings.HasPrefix(escapedPath, "/") {
		escapedPath = "/" + escapedPath
	}
	var baseSegs []string
	if base != "" {
		baseSegs = strings.Split(strings.Trim(base, "/"), "/")
	}
	pathSegs := strings.Split(escapedPath[1:], "/")
	if len(baseSegs) > 0 && len(pathSegs) > 0 && pathSegs[0] != "" &&
		decodedPathSegment(baseSegs[len(baseSegs)-1]) == decodedPathSegment(pathSegs[0]) {
		pathSegs = pathSegs[1:]
	}
	return "/" + strings.Join(append(baseSegs, pathSegs...), "/")
}

func decodedPathSegment(segment string) string {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return decoded
}

// maxBaseURLHostBytes is the DNS presentation limit for one hostname (RFC
// 1035, without a trailing dot). It bounds the authority a client can claim
// through X-Proxy-Base-URL before providerFromURL derives a retained label.
const maxBaseURLHostBytes = 253

// validProxyPathHeader accepts one escaped path override. Literal ? and #
// would be reinterpreted as query/fragment delimiters, and controls or spaces
// cannot travel in a request target; percent escapes must be well formed.
func validProxyPathHeader(p string) bool {
	if strings.IndexFunc(p, func(r rune) bool {
		return r <= 0x20 || r == 0x7f || r == '?' || r == '#'
	}) >= 0 {
		return false
	}
	_, err := url.PathUnescape(p)
	return err == nil
}

// validProxyQueryHeader accepts one raw query override. # would start a
// fragment and controls or spaces are invalid in a request target; a literal ?
// and well-formed percent escapes are query data.
func validProxyQueryHeader(q string) bool {
	return strings.IndexFunc(q, func(r rune) bool {
		return r <= 0x20 || r == 0x7f || r == '#'
	}) < 0
}

// clientEchoMax bounds a client-supplied value echoed inside a rejection
// message. Safety guardrail: a header-sized value must never turn a 400 into a
// multi-megabyte JSON body through quoting and HTML escaping. Normal routing
// values stay byte-identical.
const clientEchoMax = 256

// clientEcho prepares a client-supplied value for %q in a rejection message.
// The shared retained-text truncator cuts on a UTF-8 boundary and marks the
// cut with retainedTruncationSuffix.
func clientEcho(value string) string {
	return retainedText(value, clientEchoMax)
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
		return nil, errInvalidHeader(hdrBaseURL, baseURL)
	}
	// "http://:8080" parses with a non-empty Host but no hostname, and Go's
	// transport dials the local host for that authority. A base URL without a
	// host is malformed input, never a local-target shorthand; reject it here
	// rather than allowing a request to reach a loopback service.
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid %s %q: a hostname is required", hdrBaseURL, clientEcho(baseURL))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported %s scheme %q", hdrBaseURL, clientEcho(parsed.Scheme))
	}
	// The base must be exactly scheme://host[:port][/path] - no userinfo
	// (credentials would ride to the upstream and leak into error echoes), no
	// query or fragment (they collide with the path/query join). A bare
	// trailing "#" parses to an empty Fragment but is still a fragment marker;
	// a percent-encoded %23 remains path data. Deny by default.
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(baseURL, "#") {
		return nil, fmt.Errorf("invalid %s %q: userinfo, query, and fragment are not allowed in a base URL", hdrBaseURL, clientEcho(baseURL))
	}
	// A client controls this authority, and providerFromURL falls back to the
	// raw hostname for single-label and unknown-suffix hosts. Reject a
	// non-DNS-length hostname or an implausible port at the routing boundary so
	// neither the retained provider label nor an upstream dial ever carries
	// header-sized data. Normal domains, IPs, ports and operator aliases pass
	// unchanged.
	if len(parsed.Hostname()) > maxBaseURLHostBytes || !config.ValidURLPort(parsed.Port()) {
		return nil, errInvalidHeader(hdrBaseURL, baseURL)
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
		// The documented default is also a legal explicit selector. The
		// defensive strip set keeps its lowercase spelling because the raw
		// client header must never pass through; normalize the selector back
		// to the wire spelling the default uses.
		if strings.EqualFold(h, "Authorization") {
			h = "Authorization"
		} else if !isValidAuthHeader(h) {
			return nil, fmt.Errorf("invalid %s header %q", hdrAuthHeader, clientEcho(h))
		}
		t.authHeader = h
	}
	if p := r.Header.Get(hdrAuthPrefix); p != "" || headerPresent(r, hdrAuthPrefix) {
		// Explicitly allow an empty prefix (e.g. raw key with no "Bearer ").
		// The prefix is concatenated into the upstream auth header value, so it
		// must be a valid header value - reject invalid bytes at the boundary
		// rather than fail later as an opaque transport error.
		if !config.ValidHeaderValue(p) {
			return nil, fmt.Errorf("invalid %s value", hdrAuthPrefix)
		}
		t.authPrefix = p
	}
	if p := strings.TrimSpace(r.Header.Get(hdrPath)); p != "" {
		if !validProxyPathHeader(p) {
			return nil, fmt.Errorf("invalid %s header", hdrPath)
		}
		t.path = p
	}
	if q := strings.TrimSpace(r.Header.Get(hdrQuery)); q != "" {
		if !validProxyQueryHeader(q) {
			return nil, fmt.Errorf("invalid %s header", hdrQuery)
		}
		t.query = q
	}
	if f := strings.TrimSpace(r.Header.Get(hdrFormat)); f != "" {
		// Validate at the boundary so an unsupported format is rejected
		// consistently (400), not only when a translation happens to run.
		if !validOutboundFormat(f) {
			return nil, fmt.Errorf("unsupported %s %q", hdrFormat, clientEcho(f))
		}
		t.format = f
	}
	inbound, err := inboundProtocol(r)
	if err != nil {
		return nil, err
	}
	t.inbound = inbound
	t.outbound = outboundProtocol(t.format)
	t.translate = t.inbound != "" && t.outbound != "" && t.inbound != t.outbound
	if t.format == "cursor" {
		// Cursor is an adapter that consumes an OpenAI Chat body and speaks
		// its own Connect protocol. Any other declared client protocol would
		// need a response shape the bridge does not emit, so reject it at the
		// routing boundary instead of forwarding the wrong envelope.
		if t.inbound != "" && t.inbound != ir.Chat {
			return nil, fmt.Errorf("cursor target requires an OpenAI Chat client protocol")
		}
		t.translate = false
	}
	if t.inbound != "" && t.outbound != "" && t.path == "" && (t.translate || pathProtocol(routePath(r)) != "") {
		// A translated request reaches the target protocol's own endpoint: the
		// client's path names the inbound protocol, so forwarding it verbatim
		// would address the wrong wire. A same-protocol request on one of the
		// three known endpoints normalizes to the canonical spelling of that
		// same endpoint. An unknown path on a same-protocol passthrough is
		// preserved: the client chose an upstream route the proxy does not own.
		t.path = protocolPath(t.outbound)
	}
	if ms := r.Header.Get(hdrTimeout); ms != "" {
		d, err := time.ParseDuration(ms + "ms")
		if err != nil || d <= 0 {
			return nil, errInvalidHeader(hdrTimeout, ms)
		}
		t.timeout = d
	}
	if raw := r.Header.Get(hdrHeaders); raw != "" {
		var m map[string][]string
		if err := adminjson.Unmarshal([]byte(raw), &m); err != nil {
			// The strict decoder owns its diagnostics; this boundary bounds the
			// echo so a header-sized duplicate/invalid key cannot amplify the 400.
			return nil, fmt.Errorf("invalid %s header: %s", hdrHeaders, clientEcho(err.Error()))
		}
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		if err := config.ValidateHeaderNames(names); err != nil {
			// The shared validator owns the grammar and its diagnostic wording;
			// this boundary owns the echo bound, so a header-sized invalid name
			// cannot amplify the 400 body. Normal names keep the exact wording.
			return nil, fmt.Errorf("invalid %s header: %s", hdrHeaders, clientEcho(err.Error()))
		}
		// Validate names/values at the boundary so a malformed header is a
		// clean 400 here, not a late transport write failure surfaced as a 502.
		for k, vs := range m {
			if !isValidAuthHeader(k) {
				return nil, fmt.Errorf("invalid %s header name %q", hdrHeaders, clientEcho(k))
			}
			for _, v := range vs {
				if !config.ValidHeaderValue(v) {
					return nil, fmt.Errorf("invalid %s value for header %q", hdrHeaders, clientEcho(k))
				}
			}
		}
		t.extraHeader = http.Header(m)
	}

	if len(cfg.AllowedBaseURLs) > 0 && !allowed(t.baseURL, cfg.AllowedBaseURLs) {
		return nil, fmt.Errorf("base URL %q not in allowed list", clientEcho(t.baseURL))
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
	if isHopByHop(h) || isProxyControlHeader(h) || isTransportModeHeader(h) ||
		strings.EqualFold(h, "host") || strings.EqualFold(h, "content-length") {
		return false
	}
	return true
}

// isTransportModeHeader reports whether a header selects a transport-level
// response mode the proxy must own. Accept-Encoding controls gzip negotiation
// and net/http's automatic decompression; Range asks for a partial body. A
// supplied value from any source would make the relay forward bytes whose
// framing no longer matches the status and headers it emits.
func isTransportModeHeader(h string) bool {
	return strings.EqualFold(h, "Accept-Encoding") || strings.EqualFold(h, "Range")
}

// stripTransportModeHeaders removes the transport-mode headers after every
// header source has run. The shared transport then adds its own
// Accept-Encoding, auto-decompresses gzip upstream bodies, and never sends a
// partial-range request.
func stripTransportModeHeaders(h http.Header) {
	h.Del("Accept-Encoding")
	h.Del("Range")
}
