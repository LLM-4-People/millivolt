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
	"strings"
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

// Client is the one HTTP owner for the operator plane. Every request it builds
// carries the Bearer credential; no caller ever formats a header or a URL.
type Client struct {
	base   *url.URL
	token  string
	http   *http.Client
	limits Limits
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
		base:   base,
		token:  token,
		http:   &http.Client{Timeout: limits.Timeout},
		limits: limits,
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
		return nil, fmt.Errorf("proxy URL is not a valid URL: %v", err)
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
func newAPIError(resp *http.Response, body []byte) *APIError {
	message := strings.TrimSpace(string(body))
	var flat struct {
		Error string `json:"error"`
	}
	if len(body) > 0 && json.Unmarshal(body, &flat) == nil && flat.Error != "" {
		message = flat.Error
	} else if message != "" {
		message = strings.Join(strings.Fields(message), " ")
		if len(message) > maxErrorBodyBytes {
			message = message[:maxErrorBodyBytes] + " [truncated]"
		}
	}
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return &APIError{
		Status:     resp.StatusCode,
		Message:    message,
		RetryAfter: resp.Header.Get("Retry-After"),
		Challenge:  resp.Header.Get("WWW-Authenticate"),
	}
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
// JSON decoding, for the Prometheus text exposition.
func (c *Client) getText(ctx context.Context, path string, query url.Values) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp, body)
	}
	return body, nil
}

// doJSON is the single request/response owner: URL assembly, the Bearer
// header, the bounded read, the status check and the JSON decode.
func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request body: %v", err)
		}
		payload = bytes.NewReader(encoded)
	}
	resp, err := c.send(ctx, method, path, query, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := readBody(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return newAPIError(resp, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("millivolt returned HTTP %d with a body this tool could not decode as JSON (%d bytes): %v",
			resp.StatusCode, len(raw), err)
	}
	return nil
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

// redactCredential removes the operator token from a transport error string.
// net/http does not echo request headers, so this is a belt-and-braces guard
// for any future error that carries the request URL or its headers.
func redactCredential(message, token string) string {
	if token == "" {
		return message
	}
	return strings.ReplaceAll(message, token, "[redacted]")
}
