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
	"strconv"
	"strings"
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
// answers with text/plain carrying a JSON body. A body that is not that shape
// degrades to a bounded, whitespace-collapsed excerpt instead of raw bytes.
//
// The credential is redacted from the RESULT, not only from the transport
// error: an endpoint that reflects the Authorization header back into its own
// failure body would otherwise hand the credential to the model verbatim, and
// everything this returns is logged and pasted elsewhere. The redaction runs on
// the full text BEFORE the bound, so a token straddling the bound cannot lose
// its matching half and survive as a fragment.
func newAPIError(resp *http.Response, body []byte, token string) *APIError {
	message := strings.TrimSpace(string(body))
	var flat struct {
		Error string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &flat) == nil && flat.Error != "" {
		message = flat.Error
	}
	if message != "" {
		// Redact the FULL, untruncated message before collapsing whitespace and
		// applying the bound. Bounding first would cut a credential straddling
		// the boundary in half, and the surviving fragment no longer matches
		// the whole token, so it would ride into the tool message. Both shapes
		// get the same treatment: a flat {"error": "<huge>"} is exactly as able
		// to fill a model context as an unexpected HTML page, and the proxy's
		// own text can carry newlines.
		message = redactCredential(message, token)
		message = strings.Join(strings.Fields(message), " ")
		if len(message) > maxErrorBodyBytes {
			message = message[:maxErrorBodyBytes] + " [truncated]"
		}
	}
	return &APIError{
		Status:     resp.StatusCode,
		Message:    errorMessage(message, resp.StatusCode),
		RetryAfter: sanitizeRetryAfter(resp.Header.Get("Retry-After")),
		Challenge:  resp.Header.Get("WWW-Authenticate"),
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
		if _, err := strconv.Atoi(trimmed); err == nil {
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

// redactCredential removes the operator token from any text that can reach a
// model or a log. It covers the transport error, where net/http does not echo
// request headers, and the failure body, where a reflecting endpoint could put
// the credential back verbatim.
func redactCredential(message, token string) string {
	if token == "" {
		return message
	}
	return strings.ReplaceAll(message, token, "[redacted]")
}

// redact is the one entry point for a caller that decoded a response body
// itself (the config patch path, where a committed document arrives on a 500).
// Every model-visible string from such a body goes through it, because the
// fatal reload text can quote the request the proxy just refused.
func (c *Client) redact(text string) string {
	return redactCredential(text, c.token)
}
