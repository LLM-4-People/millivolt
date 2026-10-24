package proxy

// Round 5 B2 regression: an Anthropic stop_reason "error" without an error
// object is still a provider failure. The non-stream record must name
// upstream_error whether the body is relayed natively or translated; the
// translated error envelope is a failure status, never a clean stop.

import (
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicErrorFinishBody = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"partial"}],"stop_reason":"error","usage":{"input_tokens":1,"output_tokens":1}}`

func TestAnthropicStopReasonErrorRecordsFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		format string
		body   string
		status int
	}{
		{
			name:   "native anthropic",
			path:   "/v1/messages",
			body:   `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusOK,
		},
		{
			name:   "translated chat",
			path:   "/v1/chat/completions",
			format: "anthropic",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			status: http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, anthropicErrorFinishBody)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.status, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != "upstream_error" || !rec.IsError() {
				t.Fatalf("record = type %q code %q error %v, want upstream_error", rec.ErrorType, rec.ErrorCode, rec.IsError())
			}
		})
	}
}
