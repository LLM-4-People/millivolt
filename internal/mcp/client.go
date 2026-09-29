// Package mcp wraps millivolt's operator and observability HTTP surface as a
// stdio MCP server, so an LLM can run analytical queries against a running
// proxy and drive its safe operator controls.
//
// One owner per concern, deliberately: Client owns authentication, request
// building, response decoding and error extraction; Scope owns the explorer's
// filter grammar; the operator and purge files own the mutation calls. No tool
// builds a URL, presents the credential, or encodes a filter on its own.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Route paths. This is the closed set of operator-plane endpoints this server
// calls: a tool cannot name an arbitrary path, so no request can reach the
// proxy's transparent inference catch-all, and no restore-adjacent route is
// reachable.
const (
	routeQuery        = "/metrics/query"
	routeExplorer     = "/metrics/agg/explorer"
	routeChart        = "/metrics/agg/chart"
	routeLog          = "/metrics/agg/log"
	routeBootstrap    = "/metrics/bootstrap"
	routePrometheus   = "/metrics/prometheus"
	routePause        = "/admin/pause"
	routeThrottle     = "/admin/throttle"
	routeQuota        = "/admin/quota"
	routeDebug        = "/admin/debug"
	routeDebugCapture = "/admin/debug/capture"
	routeConfig       = "/admin/config"
	routeReload       = "/admin/reload"
	routeRestart      = "/admin/restart"
	routePurge        = "/admin/purge"
	routePurgeCount   = "/admin/purge/count"
)

// maxResponseBytes bounds one response body read. An internal guardrail against
// a runaway payload, not a user-tunable: the proxy already bounds its own
// surfaces (storage_query_max_bytes for SQL results, debug_capture_max_bytes
// for a capture document).
const maxResponseBytes = 64 << 20

// maxErrorBodyBytes bounds how much of a failure body is read back into a tool
// message. The proxy's flat {"error": "..."} body is far smaller; the rest of
// the budget keeps an unexpected HTML error page from filling a model context.
const maxErrorBodyBytes = 4 << 10

// errRedirectRefused is returned instead of following a redirect. No operator
// route redirects, so a redirect answer is a misconfiguration or a hostile
// endpoint - and following one is exactly the wrong thing to do: net/http
// compares only the HOSTNAME when deciding whether to keep the Authorization
// header, so a same-host different-port or subdomain 307/308 would replay both
// the credential and the full POST body to the redirect target. Refusing every
// redirect removes the question.
var errRedirectRefused = errors.New("millivolt answered a redirect; no operator route redirects, " +
	"and following one would replay the operator credential and the request body to another endpoint")

// refuseRedirect is the one CheckRedirect this client installs. It never follows.
func refuseRedirect(*http.Request, []*http.Request) error { return errRedirectRefused }

// Client is the one HTTP owner for the operator plane. Every request it builds
// carries the Bearer credential; no caller ever formats a header or a URL.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// Origin is the validated proxy origin the client calls. It never carries the
// credential.
func (c *Client) Origin() string { return c.base.String() }

// NewClient validates the setup parameters and returns the shared client.
// proxyURL must be an http/https origin with no path, query or fragment, since
// every operator route lives at the root; a trailing slash is normalized away.
func NewClient(proxyURL, token string, limits Limits) (*Client, error) {
	base, err := NormalizeProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	if err := ValidateOperatorToken(token); err != nil {
		return nil, err
	}
	return &Client{
		base:  base,
		token: token,
		http:  &http.Client{Timeout: limits.Timeout, CheckRedirect: refuseRedirect},
	}, nil
}

// NormalizeProxyURL parses and validates the proxy origin, returning it with
// any trailing slash removed so request paths concatenate unambiguously.
func NormalizeProxyURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("proxy URL is required: pass --proxy-url or MILLIVOLT_MCP_PROXY_URL")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// The raw value can carry userinfo, and this message reaches stderr and
		// the model: report the parser's own detail without restating the URL.
		// A *url.Error's Error() is `parse "<raw>": ...`, so unwrap it; the
		// inner cause (an invalid port, an invalid escape) never repeats it.
		var parseErr *url.Error
		if errors.As(err, &parseErr) && parseErr.Err != nil {
			return nil, fmt.Errorf("proxy URL is not a valid URL: %v", parseErr.Err)
		}
		return nil, errors.New("proxy URL is not a valid URL")
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return nil, errors.New("proxy URL must use http or https")
	case parsed.Host == "":
		return nil, errors.New("proxy URL must include a host, for example http://127.0.0.1:8081")
	case parsed.User != nil:
		return nil, errors.New("proxy URL must not carry credentials; use --operator-token")
	case parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "":
		return nil, errors.New("proxy URL must not carry a query or fragment")
	case parsed.Path != "" && parsed.Path != "/":
		return nil, errors.New("proxy URL must be an origin with no path; every millivolt operator route is at the root")
	}
	normalized := *parsed
	normalized.Path = ""
	return &normalized, nil
}

// APIError is one operator-plane failure, carrying the proxy's own message
// plus the status metadata a model can act on. It is a tool-level error, never
// a transport crash: handlers return it and the model reads it.
type APIError struct {
	Status     int
	Message    string
	RetryAfter string
	// Challenge is the WWW-Authenticate value on a 401, kept so the tool
	// message can name the expected scheme.
	Challenge string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "millivolt returned HTTP %d: %s", e.Status, e.Message)
	switch e.Status {
	case http.StatusUnauthorized:
		b.WriteString(" (the operator token is missing, wrong, or the session expired; check MILLIVOLT_OPERATOR_TOKEN)")
	case http.StatusForbidden:
		b.WriteString(" (the proxy's operator plane is unarmed: it is started without MILLIVOLT_OPERATOR_TOKEN)")
	case http.StatusTooManyRequests:
		if e.RetryAfter != "" {
			fmt.Fprintf(&b, " (retry after %s)", e.RetryAfter)
		} else {
			b.WriteString(" (repeated wrong credentials were rate limited; wait and check the token)")
		}
	}
	return b.String()
}

// newAPIError extracts the proxy's flat {"error": "..."} body REGARDLESS of
// the declared Content-Type: the operator routes are inconsistent, and some
// answers with text/plain carrying a JSON body. Any other JSON shape has every
// string field decoded and joined before matching, so a credential in a nested
// document gets the same unescaping the flat shape always got; a body that is
// not JSON at all degrades to a bounded, whitespace-collapsed excerpt instead
// of raw bytes.
//
// The credential is redacted from the RESULT, not only from the transport
// error: an endpoint that reflects the Authorization header back into its own
// failure body would otherwise hand the credential to the model verbatim, and
// everything this returns is logged and pasted elsewhere. The redaction runs on
// the full text BEFORE the bound, so a token straddling the bound cannot lose
// its matching half and survive as a fragment; redactForExcerpt owns that
// order and the scan bound.
func newAPIError(resp *http.Response, body []byte, token string) *APIError {
	message := strings.TrimSpace(string(body))
	var flat struct {
		Error string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &flat) == nil && flat.Error != "" {
		message = flat.Error
	} else if decoded, ok := structuredStrings(body); ok {
		message = decoded
	}
	if message != "" {
		message = redactForExcerpt(message, token)
	}
	return &APIError{
		Status:     resp.StatusCode,
		Message:    errorMessage(message, resp.StatusCode),
		RetryAfter: sanitizeRetryAfter(resp.Header.Get("Retry-After")),
		Challenge:  resp.Header.Get("WWW-Authenticate"),
	}
}

// structuredStrings decodes every string value in a JSON failure body and
// joins them. The flat {"error": "..."} document is handled before this; any
// other JSON shape used to reach the message as raw text, so a credential
// inside it arrived still JSON-escaped and no matcher could see it. Decoding
// the fields first gives the matcher the same text the flat shape gets.
func structuredStrings(body []byte) (string, bool) {
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return "", false
	}
	var values []string
	collectJSONStrings(document, &values)
	if len(values) == 0 {
		return "", false
	}
	return strings.Join(values, " "), true
}

// collectJSONStrings walks a decoded JSON document in order, collecting every
// non-empty string value. Object keys are field names, not values, so they are
// not collected.
func collectJSONStrings(value any, values *[]string) {
	switch typed := value.(type) {
	case string:
		if typed != "" {
			*values = append(*values, typed)
		}
	case []any:
		for _, item := range typed {
			collectJSONStrings(item, values)
		}
	case map[string]any:
		for _, item := range typed {
			collectJSONStrings(item, values)
		}
	}
}

// sanitizeRetryAfter keeps only the two shapes HTTP defines for Retry-After,
// a delay in seconds or an HTTP date, because the header is copied into a
// model-visible message verbatim and its value is endpoint-controlled. Any
// other text is dropped rather than printed: the generic rate-limit message
// is then shown instead, and no arbitrary content (or credential) can ride in.
func sanitizeRetryAfter(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 20 {
		// ParseUint accepts only non-negative ASCII digits: a sign prefix is
		// not permitted, so "-42" and "+42" are dropped instead of echoed.
		if _, err := strconv.ParseUint(trimmed, 10, 64); err == nil {
			return trimmed
		}
	}
	if when, err := http.ParseTime(trimmed); err == nil {
		return when.UTC().Format(http.TimeFormat)
	}
	return ""
}

// errorMessage is the one owner of "the proxy's own text, or the status text
// when it sent none". It is shared with the config patch path, where a failure
// status arrives in a body this server already decoded.
func errorMessage(message string, status int) string {
	if trimmed := strings.TrimSpace(message); trimmed != "" {
		return trimmed
	}
	return http.StatusText(status)
}

// getJSON performs one authenticated GET and decodes the JSON body into out.
// A non-2xx status becomes an *APIError carrying the proxy's message.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, query, nil, out)
}

// postJSON performs one authenticated POST with a JSON body and decodes the
// response into out. body is marshaled by the caller-owned request shapes; the
// proxy applies its single strict administrative decoder to what arrives.
func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	return c.doJSON(ctx, http.MethodPost, path, nil, body, out)
}

// getText performs one authenticated GET and returns a bounded body without
// decoding, for the Prometheus exposition and the capture document (which the
// tool sizes before decoding).
func (c *Client) getText(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.getRaw(ctx, http.MethodGet, path, query)
}

// getRaw is the one bounded, authenticated body reader for a response the tool
// does not decode itself.
func (c *Client) getRaw(ctx context.Context, method, path string, query url.Values) ([]byte, error) {
	resp, err := c.send(ctx, method, path, query, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp, body, c.token)
	}
	return body, nil
}

// doJSON is the single request/response owner: URL assembly, the Bearer
// header, the bounded read, the status check and the JSON decode. It discards
// the status; a caller that must reason about a non-2xx status uses
// doJSONTolerating.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	_, err := c.doJSONTolerating(ctx, method, path, query, body, out, nil)
	return err
}

// doJSONTolerating is doJSON with an explicit allowlist of non-2xx statuses
// whose body is still that route's success document, and it reports the status
// so the caller can tell a tolerated one from a real failure.
//
// Exactly one route needs it: POST /admin/config writes the file and THEN
// answers 500 when the reload failed (internal/config/admin.go servePost).
// Treating that as a transport failure reports an error for a mutation that
// SUCCEEDED and invites a retry that then fails with 409. No other status on
// any route is tolerated: a 409, a 401 and a 500 with no committed document all
// stay errors.
func (c *Client) doJSONTolerating(ctx context.Context, method, path string, query url.Values, body any, out any, tolerate map[int]bool) (int, error) {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}
	resp, err := c.send(ctx, method, path, query, payload)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	raw, err := readBody(resp.Body)
	if err != nil {
		return resp.StatusCode, err
	}
	if (resp.StatusCode < 200 || resp.StatusCode > 299) && !tolerate[resp.StatusCode] {
		return resp.StatusCode, newAPIError(resp, raw, c.token)
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return resp.StatusCode, fmt.Errorf("millivolt returned HTTP %d with a body this tool could not decode as JSON (%d bytes): %v",
			resp.StatusCode, len(raw), err)
	}
	return resp.StatusCode, nil
}

// send builds and performs the authenticated request. The credential appears
// only in the Authorization header: it is never placed in the URL, logged, or
// echoed into a tool result.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body io.Reader) (*http.Response, error) {
	endpoint := *c.base
	endpoint.Path = path
	if len(query) > 0 {
		endpoint.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	// The credential is not a cookie: the proxy also mints a session cookie on
	// a successful Bearer, and reusing that cookie would silently outlive a
	// rotated token. Every call presents the Bearer explicitly.
	resp, err := c.http.Do(request)
	if err != nil {
		// net/http wraps CheckRedirect's error in *url.Error; unwrap it so the
		// model reads the refusal itself rather than a request-line dump.
		if errors.Is(err, errRedirectRefused) {
			return nil, errRedirectRefused
		}
		return nil, fmt.Errorf("reach millivolt at %s: %v", c.base.String(), redactCredential(err.Error(), c.token))
	}
	return resp, nil
}

// readBody reads a bounded body and reports an explicit error instead of a
// truncated document when the budget is exceeded.
func readBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read millivolt response: %v", err)
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("millivolt response exceeded the %d byte read budget; narrow the request", maxResponseBytes)
	}
	return raw, nil
}

// redactionMarker replaces every credential form removed from model-visible
// text. It stays visible on purpose: silent removal leaves an operator
// wondering why the proxy's own message changed.
const redactionMarker = "[redacted]"

// minRedactionRun is the shortest contiguous credential fragment treated as
// the credential itself. An endpoint can split a reflected credential across
// JSON string fields, and neither half then matches the whole token; eight
// characters is long enough that such a fragment is a usable secret, and the
// min() rule keeps an unusually short credential covered without redacting
// single characters or ordinary prose.
const minRedactionRun = 8

// maxPercentLayers bounds the nested encodings the scanner decodes. Three
// applications is the chain a gateway produces by escaping an already escaped
// value; a credential hidden any deeper is outside the forms an operator
// faces, and the bound keeps the work finite. A message whose decoded text
// still changes after this many passes is not guessed at or published partly
// decoded: the whole excerpt is replaced by the redaction marker instead.
const maxPercentLayers = 3

// maxDecodePasses is how many decoding passes one view runs: the
// maxPercentLayers escape applications the bound allows, plus a final pass
// that must find nothing left to decode. That last pass is what proves the
// text is a fixed point; a view whose final pass still decodes something fails
// closed instead.
const maxDecodePasses = maxPercentLayers + 1

// credentialFormByteMax is the largest number of raw bytes one credential byte
// can occupy in a recognized encoded form. Each decode pass consumes at most
// six raw bytes for one decoded byte (a JSON \u escape of an ASCII byte), and
// percent-encoding multiplies the raw size by three, so the bound is six to
// the power of the number of escape applications.
const credentialFormByteMax = 6 * 6 * 6

// redactForExcerpt is the one redaction pipeline for a failure message that is
// published as a whitespace-collapsed excerpt: redact, collapse, redact again,
// then bound. Redacting before the collapse is what keeps a credential from
// being cut in half by the bound; redacting after it is what catches an echo
// whose whitespace differs from the credential (tabs where it has spaces),
// which the collapse itself turns back into the credential.
//
// The scan is bounded to the region that can still reach the excerpt. A
// credential byte can occupy at most credentialFormByteMax raw bytes, so any
// match that lands inside the first maxErrorBodyBytes collapsed bytes ends
// within maxErrorBodyBytes + credentialFormByteMax*len(token) collapsed bytes;
// excerptRegion finds that raw prefix exactly, even through a long whitespace
// run, because collapsing only ever shortens text.
func redactForExcerpt(message, token string) string {
	if message == "" {
		return message
	}
	region := excerptRegion(message, maxErrorBodyBytes+credentialFormByteMax*len(token))
	redacted := redactCredential(region, token)
	redacted = strings.Join(strings.Fields(redacted), " ")
	redacted = redactCredential(redacted, token)
	if len(redacted) > maxErrorBodyBytes {
		redacted = redacted[:maxErrorBodyBytes] + " [truncated]"
	}
	return redacted
}

// excerptRegion returns the shortest prefix of message whose whitespace
// collapse still reaches target bytes, or the whole message when it cannot.
// Collapsing only ever shortens text (a run of whitespace becomes one space,
// leading and trailing whitespace disappears), so the raw prefix that produces
// a given collapsed length is found by walking the collapse itself: a fixed
// raw byte cut would include unreachable text and, with a long whitespace run,
// exclude a credential the collapse pulls into the excerpt.
func excerptRegion(message string, target int) string {
	if target <= 0 || len(message) <= target {
		return message
	}
	collapsed := 0
	inWord := false
	for index := 0; index < len(message); {
		r, size := utf8.DecodeRuneInString(message[index:])
		if unicode.IsSpace(r) {
			inWord = false
			index += size
			continue
		}
		if !inWord {
			if collapsed > 0 {
				collapsed++ // the single space between two words
			}
			inWord = true
		}
		collapsed++
		index += size
		if collapsed >= target {
			return message[:index]
		}
	}
	return message
}

// rawSpan is the half-open byte range of one match in the original message.
type rawSpan struct{ start, end int }

// decodedByte is one byte of a decoded view with the raw range it came from. A
// multi-byte JSON escape produces several decoded bytes that share the whole
// escape's range, so mapping a match back to the message includes every raw
// byte the match consumed.
type decodedByte struct {
	b          byte
	start, end int
}

// credentialView is one decoding interpretation of the message. A reflection
// can encode the credential in more than one way at once, and an ambiguous
// sequence (`%25` is a literal percent or the first layer of `%2520`) decodes
// differently per interpretation, so every interpretation is scanned and the
// matches are unioned.
type credentialView struct {
	raw  string
	at   int
	last *decodeLayer
}

// maxLayerBuffer bounds one decode layer's input queue. One escape needs at
// most twelve input bytes (a surrogate pair), so the queue is fixed-size and
// the pipeline allocates nothing per byte.
const maxLayerBuffer = 12

// decodeLayer is one pass of a decoding pipeline. It pulls (byte, raw range)
// pairs from its source, decodes the escapes it recognizes, and emits pairs
// carrying the raw range of everything that produced them. The first layer
// reads the message; every later layer reads the previous layer's output, so a
// byte produced by one pass is decoded again by the next. The pipeline is what
// decodes full-byte nested forms like %25%36%62 (two passes to k), which
// peeling a single escape cannot see.
type decodeLayer struct {
	source    *decodeLayer
	raw       string
	at        int
	plusSpace bool
	exhausted bool
	in        [maxLayerBuffer]decodedByte
	inHead    int
	inLen     int
	out       [utf8.UTFMax]decodedByte
	outHead   int
	outLen    int
	decoded   bool
}

// fill pulls from the source until want bytes are buffered or it is exhausted.
func (l *decodeLayer) fill(want int) {
	for l.inLen < want && !l.exhausted {
		var db decodedByte
		if l.source == nil {
			if l.at >= len(l.raw) {
				l.exhausted = true
				return
			}
			db = decodedByte{b: l.raw[l.at], start: l.at, end: l.at + 1}
			l.at++
		} else {
			next, ok := l.source.next()
			if !ok {
				l.exhausted = true
				return
			}
			db = next
		}
		l.in[(l.inHead+l.inLen)%maxLayerBuffer] = db
		l.inLen++
	}
}

// peek returns the buffered input byte at index without consuming it.
func (l *decodeLayer) peek(index int) decodedByte {
	return l.in[(l.inHead+index)%maxLayerBuffer]
}

// drop consumes count buffered input bytes.
func (l *decodeLayer) drop(count int) {
	l.inHead = (l.inHead + count) % maxLayerBuffer
	l.inLen -= count
}

// next returns the next decoded byte with its raw range, or false at the end
// of the stream.
func (l *decodeLayer) next() (decodedByte, bool) {
	for {
		if l.outLen > 0 {
			out := l.out[l.outHead]
			l.outHead = (l.outHead + 1) % utf8.UTFMax
			l.outLen--
			return out, true
		}
		l.fill(1)
		if l.inLen == 0 {
			return decodedByte{}, false
		}
		head := l.peek(0)
		if head.b == '\\' {
			if r, consumed, ok := l.decodeJSON(); ok {
				span := rawSpan{start: head.start, end: l.peek(consumed - 1).end}
				l.drop(consumed)
				l.decoded = true
				var encoded [utf8.UTFMax]byte
				size := utf8.EncodeRune(encoded[:], r)
				for _, b := range encoded[:size] {
					l.out[(l.outHead+l.outLen)%utf8.UTFMax] = decodedByte{b: b, start: span.start, end: span.end}
					l.outLen++
				}
				continue
			}
		}
		if head.b == '%' {
			if value, ok := l.decodePercent(); ok {
				span := rawSpan{start: head.start, end: l.peek(2).end}
				l.drop(3)
				l.decoded = true
				l.out[(l.outHead+l.outLen)%utf8.UTFMax] = decodedByte{b: value, start: span.start, end: span.end}
				l.outLen++
				continue
			}
		}
		if l.plusSpace && head.b == '+' {
			l.drop(1)
			l.decoded = true
			l.out[(l.outHead+l.outLen)%utf8.UTFMax] = decodedByte{b: ' ', start: head.start, end: head.end}
			l.outLen++
			continue
		}
		l.drop(1)
		return head, true
	}
}

// decodeJSON recognizes one JSON escape at the head of the buffered input:
// the simple escapes plus \uXXXX, combining a surrogate pair when a second
// escape completes it. Hex digits are case-insensitive.
func (l *decodeLayer) decodeJSON() (rune, int, bool) {
	l.fill(2)
	if l.inLen < 2 || l.peek(0).b != '\\' {
		return 0, 0, false
	}
	switch l.peek(1).b {
	case '"', '\\', '/':
		return rune(l.peek(1).b), 2, true
	case 'b':
		return '\b', 2, true
	case 'f':
		return '\f', 2, true
	case 'n':
		return '\n', 2, true
	case 'r':
		return '\r', 2, true
	case 't':
		return '\t', 2, true
	case 'u':
		l.fill(6)
		if l.inLen < 6 {
			return 0, 0, false
		}
		first, ok := hex4(l.peek(2).b, l.peek(3).b, l.peek(4).b, l.peek(5).b)
		if !ok {
			return 0, 0, false
		}
		if utf16.IsSurrogate(rune(first)) {
			l.fill(12)
			if l.inLen >= 12 && l.peek(6).b == '\\' && l.peek(7).b == 'u' {
				if second, ok := hex4(l.peek(8).b, l.peek(9).b, l.peek(10).b, l.peek(11).b); ok {
					if combined := utf16.DecodeRune(rune(first), rune(second)); combined != utf8.RuneError {
						return combined, 12, true
					}
				}
			}
		}
		return rune(first), 6, true
	}
	return 0, 0, false
}

// decodePercent recognizes one percent escape at the head of the buffered
// input. It decodes exactly one layer: a produced percent is decoded again by
// the next pipeline pass, which is what makes nested forms work.
func (l *decodeLayer) decodePercent() (byte, bool) {
	l.fill(3)
	if l.inLen < 3 || l.peek(0).b != '%' {
		return 0, false
	}
	value, ok := hexByte(l.peek(1).b, l.peek(2).b)
	if !ok {
		return 0, false
	}
	return value, true
}

// hex4 reads four case-insensitive hex digits.
func hex4(a, b, c, d byte) (uint16, bool) {
	high, ok := hexByte(a, b)
	if !ok {
		return 0, false
	}
	low, ok := hexByte(c, d)
	if !ok {
		return 0, false
	}
	return uint16(high)<<8 | uint16(low), true
}

// credentialViews returns the interpretations the scanner runs: the raw bytes,
// then a decoding pipeline for each nesting depth, with and without the query
// `+` form. Every decoded view decodes JSON escapes, because a structured
// failure body can carry the credential as JSON text at any layer. The raw
// view covers literal forms (including url.PathEscape and url.QueryEscape
// output, which is literal text in the message).
func credentialViews(message string) []credentialView {
	views := make([]credentialView, 0, 2*maxPercentLayers+1)
	views = append(views, credentialView{raw: message})
	for layers := 1; layers <= maxPercentLayers; layers++ {
		views = append(views, decodedView(message, layers, false), decodedView(message, layers, true))
	}
	return views
}

// decodedView builds the pipeline for one interpretation: maxDecodePasses
// layers, with the query `+` form applied at the pass that matches the
// encoding depth it is meant to reverse. Every layer carries the raw range of
// the input it consumed, so a match in the final text maps back to the message.
func decodedView(message string, layers int, plusSpace bool) credentialView {
	var previous *decodeLayer
	for pass := 1; pass <= maxDecodePasses; pass++ {
		layer := &decodeLayer{source: previous}
		if previous == nil {
			layer.raw = message
		}
		layer.plusSpace = plusSpace && pass == layers
		previous = layer
	}
	return credentialView{raw: message, last: previous}
}

// next returns the next decoded byte of the view with its raw range.
func (v *credentialView) next() (decodedByte, bool) {
	if v.last == nil {
		if v.at >= len(v.raw) {
			return decodedByte{}, false
		}
		db := decodedByte{b: v.raw[v.at], start: v.at, end: v.at + 1}
		v.at++
		return db, true
	}
	return v.last.next()
}

// incomplete reports whether the view's final pass still decoded something:
// its text is not a fixed point, so no match in it can be trusted and the
// message fails closed instead.
func (v *credentialView) incomplete() bool {
	return v.last != nil && v.last.decoded
}

// hexByte decodes a pair of case-insensitive hex digits.
func hexByte(high, low byte) (byte, bool) {
	h, ok := hexDigit(high)
	if !ok {
		return 0, false
	}
	l, ok := hexDigit(low)
	if !ok {
		return 0, false
	}
	return h<<4 | l, true
}

// hexDigit decodes one case-insensitive hex digit.
func hexDigit(b byte) (byte, bool) {
	switch {
	case b >= '0' && b <= '9':
		return b - '0', true
	case b >= 'a' && b <= 'f':
		return b - 'a' + 10, true
	case b >= 'A' && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}

// credentialWindows maps every window-length byte run of the credential to one
// of its offsets. One witness per run is enough: the scan only extends a match,
// never chooses the longest of several.
func credentialWindows(token string) map[uint64]int {
	window := min(minRedactionRun, len(token))
	if window == 0 {
		return nil
	}
	windows := make(map[uint64]int, len(token)-window+1)
	for start := 0; start+window <= len(token); start++ {
		var key uint64
		for offset := 0; offset < window; offset++ {
			key = key<<8 | uint64(token[start+offset])
		}
		if _, seen := windows[key]; !seen {
			windows[key] = start
		}
	}
	return windows
}

// scanView walks one decoded view and appends the raw range of every match of
// a credential window or longer to spans. The window slides as one packed
// integer key, so the scan is linear in the message; a match extends byte by
// byte to the longest run the token allows, and the raw range comes from the
// decoded bytes' source spans.
func scanView(token string, windows map[uint64]int, view *credentialView, spans *[]rawSpan) {
	window := min(minRedactionRun, len(token))
	if window == 0 {
		return
	}
	mask := ^uint64(0) >> (64 - uint(8*window))
	var key uint64
	var starts [minRedactionRun]int
	filled, slot := 0, 0
	advance := func(db decodedByte) {
		key = (key<<8 | uint64(db.b)) & mask
		starts[slot] = db.start
		slot = (slot + 1) % window
		if filled < window {
			filled++
		}
	}
	for {
		db, ok := view.next()
		if !ok {
			return
		}
		advance(db)
		if filled < window {
			continue
		}
		offset, hit := windows[key]
		if !hit {
			continue
		}
		// The oldest byte of the window sits at slot.
		span := rawSpan{start: starts[slot], end: db.end}
		length := window
		var trailing *decodedByte
		for offset+length < len(token) {
			next, ok := view.next()
			if !ok {
				break
			}
			if next.b != token[offset+length] {
				trailing = &next
				break
			}
			length++
			span.end = next.end
		}
		// A window hit is itself a redactable fragment even when the run
		// stops there, so the partial extension is removed too.
		*spans = append(*spans, span)
		key, filled, slot = 0, 0, 0
		if trailing != nil {
			// The mismatching byte belongs to the next window.
			advance(*trailing)
		}
	}
}

// applySpans replaces every merged match range with the visible marker and
// returns the message byte-for-byte unchanged when there is none.
func applySpans(message string, spans []rawSpan) string {
	if len(spans) == 0 {
		return message
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})
	var out strings.Builder
	out.Grow(len(message))
	last := 0
	for _, span := range spans {
		if span.start < last {
			if span.end <= last {
				continue
			}
			span.start = last
		}
		out.WriteString(message[last:span.start])
		out.WriteString(redactionMarker)
		last = span.end
	}
	out.WriteString(message[last:])
	return out.String()
}

// redactCredential removes the operator token from any text that can reach a
// model or a log. It covers the transport error, where net/http does not echo
// request headers, and the failure body, where a reflecting endpoint could put
// the credential back verbatim. Every decoded view is scanned, so all of these
// are removed:
//
//   - the literal token, and any contiguous fragment of it at least
//     minRedactionRun bytes long, so a credential split across two JSON string
//     fields cannot leak a usable piece;
//   - percent-encoded forms at any hex case and up to maxPercentLayers escape
//     applications, including full-byte nested forms and the query `+` form of
//     a space;
//   - JSON-escaped forms, including backslash-doubled text and \u escapes.
//
// A message that still decodes after maxPercentLayers applications is not
// published partly decoded: the whole message is replaced by the redaction
// marker instead. That is the fail-closed path for nesting deeper than the
// bound, and it is why the coverage claim does not depend on guessing what an
// undecoded remainder might hide.
func redactCredential(message, token string) string {
	if token == "" || message == "" {
		return message
	}
	windows := credentialWindows(token)
	if len(windows) == 0 {
		return message
	}
	var spans []rawSpan
	for _, view := range credentialViews(message) {
		scanView(token, windows, &view, &spans)
		if view.incomplete() {
			return redactionMarker
		}
	}
	return applySpans(message, spans)
}

// redact is the one entry point for a caller that decoded a response body
// itself (the config patch path, where a committed document arrives on a 500).
// Every model-visible string from such a body goes through it, because the
// fatal reload text can quote the request the proxy just refused.
func (c *Client) redact(text string) string {
	return redactCredential(text, c.token)
}
