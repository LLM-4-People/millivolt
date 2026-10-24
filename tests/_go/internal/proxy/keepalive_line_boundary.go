package proxy

// Round 7 E2 regression: the keepalive ticker must never split an SSE line.
// The pacer tracks the last body byte at its single write choke point and skips
// a comment while mid-line, including the force path.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestKeepaliveNeverSplitsSSELine(t *testing.T) {
	const stall = 120 * time.Millisecond
	for _, tc := range []struct {
		name     string
		status   int
		stream   bool
		first    string
		second   string
		wantLine string
		wantKeep bool
	}{
		{
			name:     "201 partial line",
			status:   http.StatusCreated,
			first:    `data: {"x":`,
			second:   "1}\n\n",
			wantLine: `data: {"x":1}` + "\n\n",
		},
		{
			name:     "201 complete line",
			status:   http.StatusCreated,
			first:    `data: {"x":1}` + "\n",
			second:   "\n",
			wantLine: `data: {"x":1}` + "\n",
			wantKeep: true,
		},
		{
			name:     "500 sse unchanged",
			status:   http.StatusInternalServerError,
			first:    `data: {"x":`,
			second:   "1}\n\n",
			wantLine: `data: {"x":1}` + "\n\n",
		},
		{
			name:     "200 stream partial line",
			status:   http.StatusOK,
			stream:   true,
			first:    `data: {"x":`,
			second:   "1}\n\n",
			wantLine: `data: {"x":1}` + "\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(tc.status)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				_, _ = io.WriteString(w, tc.first)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(stall)
				_, _ = io.WriteString(w, tc.second)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer upstream.Close()

			cfg := config.Default()
			cfg.MaxRetries = 0
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			cfg.SSEKeepaliveInterval = 25 * time.Millisecond
			srv := httptest.NewServer(New(cfg, metrics.Noop{}))
			defer srv.Close()

			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
			if tc.stream {
				body = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer sk-test")
			req.Header.Set("X-Proxy-Base-URL", upstream.URL)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			got := string(raw)

			if !strings.Contains(got, tc.wantLine) {
				t.Fatalf("SSE line was split by a keepalive: %q", got)
			}
			if tc.wantKeep {
				comment := strings.Index(got, ": keepalive")
				line := strings.Index(got, tc.wantLine)
				if comment < 0 {
					t.Fatalf("complete line got no own-line keepalive: %q", got)
				}
				if comment < line+len(tc.wantLine) {
					t.Fatalf("keepalive was not after the complete line: %q", got)
				}
			}
			if tc.status >= 400 && strings.Contains(got, ": keepalive") {
				t.Fatalf("error body got a keepalive: %q", got)
			}
		})
	}
}
