package mcp

// Exported entry points for the operator command in cmd/mvctl. The MCP tools
// use the package-private methods above; a command-line caller needs the same
// transport contract without importing a tool-shaped API. Keeping both on the
// one Client is deliberate: origin validation, the credential, redirect
// refusal, the response bound and credential redaction have a single owner.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// NewCommandClient builds the shared client for a command-line caller. It
// deliberately leaves the blanket http.Client.Timeout unset: a backup or
// restore streams for as long as the proxy needs, and every bounded command
// supplies its own context deadline. The command's SQL bound is a flag, so
// this client carries no unused query timeout of its own.
func NewCommandClient(proxyURL, token string) (*Client, error) {
	return NewClientWithTransport(proxyURL, token, Limits{}, nil)
}

// GetJSON performs one authenticated GET and decodes the JSON body into out.
// A non-2xx status becomes an *APIError carrying the proxy's redacted message.
func (c *Client) GetJSON(ctx context.Context, route Route, query url.Values, out any) error {
	return c.getJSON(ctx, route, query, out)
}

// PostJSON performs one authenticated POST with a JSON body and decodes the
// response into out. A nil tolerate map rejects every non-2xx status. A
// tolerated status is returned so the caller can distinguish a committed
// document from a failure.
func (c *Client) PostJSON(ctx context.Context, route Route, query url.Values, body any, out any, tolerate map[int]bool) (int, error) {
	return c.doJSONTolerating(ctx, http.MethodPost, route, query, body, out, tolerate)
}

// GetText performs one authenticated GET and returns the bounded body without
// decoding it.
func (c *Client) GetText(ctx context.Context, route Route, query url.Values) ([]byte, error) {
	return c.getText(ctx, route, query)
}

// GetStream performs one authenticated GET and returns the success body
// unread. The caller owns closing it. A failure body is bounded and becomes an
// *APIError exactly like the buffered paths.
func (c *Client) GetStream(ctx context.Context, route Route, query url.Values) (io.ReadCloser, error) {
	resp, err := c.openRaw(ctx, http.MethodGet, route, query)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// PostStream uploads body to one operator route and returns the bounded
// response body. It is the restore path: the archive is streamed from the
// caller's file instead of being materialized in memory.
func (c *Client) PostStream(ctx context.Context, route Route, query url.Values, contentType string, body io.Reader, size int64) ([]byte, error) {
	resp, err := c.sendStreamWith(c.http, ctx, http.MethodPost, route, query, contentType, size, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := readBody(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp, raw, c.token)
	}
	return raw, nil
}

// CheckKnownProvider rejects a throttle provider outside the operator's own
// vocabulary. Writing a limit for an unknown name permanently adds it to that
// vocabulary, so a typo would not fail; it would pollute every later
// validation and discovery list. The guard fails open when the vocabulary
// cannot be read or is empty, because refusing then would block a legitimate
// change on an unrelated failure.
func (c *Client) CheckKnownProvider(ctx context.Context, provider string) error {
	status, err := c.debug(ctx)
	if err != nil {
		return nil
	}
	known := list(status.KnownProviders)
	if len(known) == 0 || slices.Contains(known, provider) {
		return nil
	}
	return fmt.Errorf("provider %q is not one of the known providers, and writing a limit for it would add the name to the proxy's vocabulary permanently. Known providers: %s",
		provider, strings.Join(known, ", "))
}

// CheckKnownCaptureScope refuses a capture scope outside the vocabulary the
// proxy reports. A session scoped to a name that matches nothing reports
// enabled:true and captures nothing, so a typo would look like success. The
// same requireKnownScope owner backs the MCP audit tool.
func (c *Client) CheckKnownCaptureScope(ctx context.Context, clients, providers, models []string) error {
	status, err := c.debug(ctx)
	if err != nil {
		return fmt.Errorf("read the capture scope vocabulary: %v", err)
	}
	return requireKnownScope(clients, providers, models, status)
}

// Redact removes the operator credential from text that can reach a terminal,
// a log, or a shell pipeline. It is the same redactor the MCP tools trust.
func (c *Client) Redact(text string) string {
	return c.redact(text)
}
