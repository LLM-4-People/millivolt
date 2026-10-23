package proxy

// Round 2 TC5-1/TC5-2 regressions: keepalive comments belong only to an
// event-stream body, and a translated non-200 error is JSON, not SSE.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func slowJSONUpstream(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		time.Sleep(70 * time.Millisecond)
		fmt.Fprint(w, body)
	}))
}

func slowJSONRequest(t *testing.T, srv *httptest.Server, upstream *httptest.Server, format string, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	if format != "" {
		req.Header.Set("X-Proxy-Format", format)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestSlowNonSSEBodyHasNoKeepalive(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{name: "201", status: http.StatusCreated, body: `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
		{name: "200", status: http.StatusOK, body: `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := slowJSONUpstream(t, tc.status, tc.body)
			defer upstream.Close()
			cfg := config.Default()
			cfg.SSEKeepaliveInterval = 20 * time.Millisecond
			srv := httptest.NewServer(New(cfg, metrics.Noop{}))
			defer srv.Close()
			resp, got := slowJSONRequest(t, srv, upstream, "", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", resp.StatusCode, tc.status, got)
			}
			if strings.Contains(got, ": keepalive") {
				t.Fatalf("JSON body received SSE keepalive comments: %q", got)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("content-type = %q, want application/json", ct)
			}
		})
	}
}

func TestTranslatedSlow201IsJSON(t *testing.T) {
	upstream := slowJSONUpstream(t, http.StatusCreated,
		`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	defer upstream.Close()
	cfg := config.Default()
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()
	resp, got := slowJSONRequest(t, srv, upstream, "anthropic", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", resp.StatusCode, got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") || strings.Contains(ct, "event-stream") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if strings.Contains(got, ": keepalive") || strings.Contains(got, "data: ") {
		t.Fatalf("translated 201 carried SSE bytes: %q", got)
	}
}

func TestTranslatedNon200ErrorIsJSON(t *testing.T) {
	upstream := slowJSONUpstream(t, http.StatusInternalServerError,
		`{"type":"error","error":{"type":"api_error","message":"boom"}}`)
	defer upstream.Close()
	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()
	resp, got := slowJSONRequest(t, srv, upstream, "anthropic", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body %s)", resp.StatusCode, got)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") || strings.Contains(ct, "event-stream") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if strings.Contains(got, ": keepalive") || strings.Contains(got, "data: ") {
		t.Fatalf("translated error carried SSE bytes: %q", got)
	}
	if !strings.Contains(got, "boom") {
		t.Fatalf("translated error body = %q, want the provider message", got)
	}
}
