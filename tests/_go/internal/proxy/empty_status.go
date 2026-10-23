package proxy

// N1 regression: an accepted 202 and the status-only 204/205 are not
// completions. They relay verbatim on both the native and translated paths
// instead of entering completion classification or inventing an empty body.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAcceptedAndEmptyStatusesRelayVerbatim(t *testing.T) {
	routes := []struct {
		name   string
		path   string
		format string
		body   string
	}{
		{"native chat", "/v1/chat/completions", "", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"native responses", "/v1/responses", "", `{"model":"m","input":"hi"}`},
		{"translated chat", "/v1/chat/completions", "anthropic", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"translated responses", "/v1/responses", "anthropic", `{"model":"m","input":"hi"}`},
	}
	for _, route := range routes {
		for _, status := range []int{http.StatusAccepted, http.StatusNoContent, http.StatusResetContent} {
			t.Run(route.name+"/"+http.StatusText(status), func(t *testing.T) {
				response := ""
				if status == http.StatusAccepted {
					response = `{"status":"accepted","note":"async"}`
				}
				up := newFixtureUpstream(t, "application/json", status, response)
				buf := metrics.NewBuffer(8)
				s := New(config.Default(), buf)
				s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

				w := proxyRequest(t, s, route.path, route.format, route.body)
				if w.Code != status {
					t.Fatalf("status = %d, want %d (body %s)", w.Code, status, w.Body.String())
				}
				if got := w.Body.String(); got != response {
					t.Fatalf("body = %q, want %q", got, response)
				}
				if strings.Contains(w.Body.String(), "empty_completion") || strings.Contains(w.Body.String(), `"choices"`) {
					t.Fatalf("status %d was classified as a completion: %s", status, w.Body.String())
				}
				recs := waitForRecord(t, buf, 1)
				if len(recs) != 1 {
					t.Fatalf("records = %d, want 1", len(recs))
				}
				rec := recs[0]
				if rec.StatusCode != status || rec.ErrorType != "" || rec.ErrorCode != "" {
					t.Fatalf("record = %+v, want status %d with no error", rec, status)
				}
			})
		}
	}
}

// TestTranslatedStreamingNonCompletionKeepsUpstreamType pins N12: a streaming
// request with a differing target format must not relabel a 202/204/205 as
// text/event-stream. These statuses are not completions, so their real
// content type and body relay before the translated SSE header override.
func TestTranslatedStreamingNonCompletionKeepsUpstreamType(t *testing.T) {
	cases := []struct {
		name        string
		status      int
		contentType string
		body        string
	}{
		{"202 json", http.StatusAccepted, "application/json", `{"status":"accepted","note":"async"}`},
		{"202 sse", http.StatusAccepted, "text/event-stream", "event: accepted\ndata: {\"ok\":true}\n\n"},
		{"204", http.StatusNoContent, "application/json", ""},
		{"205", http.StatusResetContent, "application/json", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, tc.contentType, tc.status, tc.body)
			buf := metrics.NewBuffer(8)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			body := `{"model":"m","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`
			w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.status, w.Body.String())
			}
			if got := w.Header().Get("Content-Type"); got != tc.contentType {
				t.Fatalf("Content-Type = %q, want upstream %q", got, tc.contentType)
			}
			if got := w.Body.String(); got != tc.body {
				t.Fatalf("body = %q, want %q", got, tc.body)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].StatusCode != tc.status ||
				recs[0].ErrorType != "" || recs[0].ErrorCode != "" {
				t.Fatalf("record = %+v, want status %d with no error", recs, tc.status)
			}
		})
	}
}
