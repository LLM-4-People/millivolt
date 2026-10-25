package proxy

// Round 12 E2 regression: a translated non-200 must carry a protocol error
// envelope. HTTP forbids a body on 1xx/204/304, and writing one made the relay
// record a 499 client disconnect for an upstream 304.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTranslated304FailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		format string
		body   string
	}{
		{
			name:   "chat stream to anthropic",
			path:   "/v1/chat/completions",
			format: "anthropic",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
		{
			name:   "chat non-stream to anthropic",
			path:   "/v1/chat/completions",
			format: "anthropic",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "anthropic non-stream to chat",
			path:   "/v1/messages",
			format: "openai",
			body:   `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "anthropic stream to responses",
			path:   "/v1/messages",
			format: "openai-responses",
			body:   `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusNotModified, "")
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
			}
			got := w.Body.String()
			if !strings.Contains(got, `"error"`) || !strings.Contains(got, "304") {
				t.Fatalf("body = %s, want a protocol error envelope carrying the upstream status", got)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.StatusCode != http.StatusBadGateway {
				t.Fatalf("record status = %d, want 502", rec.StatusCode)
			}
			if rec.ErrorType != "upstream_error" {
				t.Fatalf("record error type = %q, want upstream_error", rec.ErrorType)
			}
			if rec.ClientDisconnected {
				t.Fatalf("record marks a client disconnect for an upstream 304: %+v", rec)
			}
			if rec.StatusCode == metrics.StatusClientClosedRequest {
				t.Fatalf("record status is the client-cancel 499: %+v", rec)
			}
			if !strings.Contains(rec.ErrorMsg, "304") {
				t.Fatalf("record message = %q, want the upstream status", rec.ErrorMsg)
			}
		})
	}
}
