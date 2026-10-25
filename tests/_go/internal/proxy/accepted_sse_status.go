package proxy

// Round 6 E1 regression: a native upstream 2xx-non-200 event-stream body is
// committed and relayed under its own status. It must never enter the buffered
// quality owner after that commit, where a retry's JSON completion would land
// under the already-sent status and SSE content type.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func Test201SSEBodyRelayedRaw(t *testing.T) {
	const event = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n"
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "event", body: event},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"c2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"retry answer"},"finish_reason":"stop"}]}`))
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 1
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})

			w := proxyRequest(t, s, "/v1/chat/completions", "",
				`{"model":"m","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (body %s)", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("content type = %q, want text/event-stream", got)
			}
			if got := w.Body.String(); got != tc.body {
				t.Fatalf("body = %q, want the raw SSE body %q", got, tc.body)
			}
			if strings.Contains(w.Body.String(), "retry answer") {
				t.Fatalf("retry JSON was injected into the 201 SSE body: %s", w.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1 (no buffered retry)", calls.Load())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].StatusCode != http.StatusCreated {
				t.Fatalf("record = %+v, want status 201", recs)
			}
		})
	}
}
