package proxy

// Round 6 E2 regression: a non-streaming request whose upstream returns an
// event-stream body creates the pacer late. The late pacer was never assigned
// back to the response writer and the early defer captured a nil receiver, so
// SSE writes never armed sse_write_progress_timeout.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestLatePacerArmsSSEWriteDeadline(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{
			name:        "event stream",
			contentType: "text/event-stream",
			body:        "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			want:        true,
		},
		{
			name:        "json",
			contentType: "application/json",
			body:        chatCompletionBody,
			want:        false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, tc.contentType, http.StatusOK, tc.body)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
				strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			r.Header.Set("Authorization", "Bearer sk-test")
			r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
			w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}

			start := time.Now()
			s.ServeHTTP(w, r)
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("request took %v", elapsed)
			}
			if got := !w.write.IsZero(); got != tc.want {
				t.Fatalf("write-progress deadline armed = %v, want %v (status %d, body %s)",
					got, tc.want, w.Code, w.Body.String())
			}
		})
	}
}
