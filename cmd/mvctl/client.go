package main

// Operator-plane HTTP client. Standard library only: these files are shared
// verbatim with the small upstream `cmd/mvctl` contribution.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20 // bootstrap + a 200-row log page can exceed 500 KiB

type Global struct {
	BaseURL string
	Token   string
	JSON    bool
	Color   bool
	Verbose bool
	Yes     bool
	Timeout time.Duration
	Theme   string
	Mode    string
	Profile string
	Args    []string
	Flags   map[string]string
}

type Client struct {
	base    string
	token   string
	timeout time.Duration
	hc      *http.Client
}

func NewClient(g Global) *Client {
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		base:    strings.TrimRight(g.BaseURL, "/"),
		token:   g.Token,
		timeout: timeout,
		hc:      &http.Client{},
	}
}

// do never puts the credential into an error message.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, accept string) ([]byte, int, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(encoded)
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, u, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("millivolt unreachable: %w", err)
	}
	defer resp.Body.Close()
	// Read one byte past the cap so a truncated body is an error, never a
	// "successful" partial response.
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}
	if len(data) > maxResponseBytes {
		return data[:maxResponseBytes], resp.StatusCode,
			fmt.Errorf("millivolt response exceeded %d bytes; use a streaming command for large bodies", maxResponseBytes)
	}
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, fmt.Errorf("millivolt returned %s: %s", resp.Status, c.scrub(detailText(data)))
	}
	return data, resp.StatusCode, nil
}

// detailText truncates by rune and scrub removes the credential in case a
// proxy echoes request headers back in an error body.
func detailText(data []byte) string {
	runes := []rune(strings.TrimSpace(string(data)))
	if len(runes) > 300 {
		return string(runes[:300]) + "…"
	}
	return string(runes)
}

func (c *Client) scrub(s string) string {
	if c.token == "" {
		return s
	}
	s = strings.ReplaceAll(s, c.token, "[redacted]")
	s = strings.ReplaceAll(s, "Bearer "+c.token, "Bearer [redacted]")
	return s
}

func decodeObject(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{"ok": true}, nil
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, fmt.Errorf("millivolt returned malformed JSON: %w", err)
	}
	if m, ok := generic.(map[string]any); ok {
		return m, nil
	}
	// A JSON array or scalar is valid; wrap it so callers keep a map.
	return map[string]any{"value": generic}, nil
}

func (c *Client) Get(ctx context.Context, path string, q url.Values) (map[string]any, error) {
	data, _, err := c.do(ctx, http.MethodGet, path, q, nil, "application/json")
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

func (c *Client) Post(ctx context.Context, path string, q url.Values, body any) (map[string]any, error) {
	data, _, err := c.do(ctx, http.MethodPost, path, q, body, "application/json")
	if err != nil {
		return nil, err
	}
	return decodeObject(data)
}

// Download streams a large binary response to a file with no size cap. It
// writes to a temporary file first and only renames on success, so a failed
// download never corrupts an existing archive.
func (c *Client) Download(ctx context.Context, path string, q url.Values, out string) (int64, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("millivolt unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return 0, fmt.Errorf("millivolt returned %s: %s", resp.Status, c.scrub(detailText(data)))
	}
	tmp := out + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	syncErr := f.Sync()
	closeErr := f.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		if copyErr != nil {
			return 0, copyErr
		}
		if syncErr != nil {
			return 0, syncErr
		}
		return 0, closeErr
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

func (c *Client) PostRaw(ctx context.Context, path string, q url.Values, contentType string, body []byte) (map[string]any, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("millivolt returned %s: %s", resp.Status, c.scrub(detailText(data)))
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading response: %w", readErr)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("millivolt response exceeded %d bytes", maxResponseBytes)
	}
	return decodeObject(data)
}

func (c *Client) Text(ctx context.Context, path string, q url.Values) (string, error) {
	data, _, err := c.do(ctx, http.MethodGet, path, q, nil, "")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Client) Bytes(ctx context.Context, path string, q url.Values) ([]byte, error) {
	data, _, err := c.do(ctx, http.MethodGet, path, q, nil, "application/octet-stream")
	return data, err
}

func (c *Client) Health(ctx context.Context) bool {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c *Client) Bootstrap(ctx context.Context) (Snapshot, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/metrics/bootstrap", nil, nil, "application/json")
	if err != nil {
		return Snapshot{}, err
	}
	return DecodeSnapshot(data)
}

func (c *Client) Overview(ctx context.Context) (Snapshot, error) {
	return c.Bootstrap(ctx)
}

func (c *Client) Requests(ctx context.Context, limit int, beforeMs float64, beforeID string) (RequestPage, error) {
	if limit < 10 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}
	q := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	if beforeMs > 0 && beforeID != "" {
		q.Set("before_ms", fmt.Sprintf("%.0f", beforeMs))
		q.Set("before_id", beforeID)
	}
	data, _, err := c.do(ctx, http.MethodGet, "/metrics/agg/log", q, nil, "application/json")
	if err != nil {
		return RequestPage{}, err
	}
	return DecodeRequestPage(data)
}

func (c *Client) Explorer(ctx context.Context, dim string, filters url.Values) (Explorer, error) {
	q := url.Values{"dim": {dim}}
	for k, v := range filters {
		q[k] = v
	}
	data, _, err := c.do(ctx, http.MethodGet, "/metrics/agg/explorer", q, nil, "application/json")
	if err != nil {
		return Explorer{}, err
	}
	var out Explorer
	err = json.Unmarshal(data, &out)
	return out, err
}

func (c *Client) Chart(ctx context.Context, filters url.Values) (Chart, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/metrics/agg/chart", filters, nil, "application/json")
	if err != nil {
		return Chart{}, err
	}
	var out Chart
	err = json.Unmarshal(data, &out)
	return out, err
}

func (c *Client) Config(ctx context.Context) (ConfigDoc, error) {
	data, _, err := c.do(ctx, http.MethodGet, "/admin/config", nil, nil, "application/json")
	if err != nil {
		return ConfigDoc{}, err
	}
	var out ConfigDoc
	err = json.Unmarshal(data, &out)
	return out, err
}

func (c *Client) SetConfig(ctx context.Context, revision string, values map[string]any) (map[string]any, error) {
	return c.Post(ctx, "/admin/config", nil, map[string]any{"revision": revision, "values": values})
}

func (c *Client) Query(ctx context.Context, sql string) (any, error) {
	q := url.Values{"q": {sql}}
	data, _, err := c.do(ctx, http.MethodGet, "/metrics/query", q, nil, "application/json")
	if err != nil {
		return nil, err
	}
	var rows any
	if err := json.Unmarshal(data, &rows); err != nil {
		return string(data), nil
	}
	return rows, nil
}
