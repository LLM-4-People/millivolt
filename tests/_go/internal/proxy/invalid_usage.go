package proxy

// Round 5 F3 regression: an Anthropic usage document that overflows must mark
// the stream's accounting invalid, not silently fall back to a chunk count. The
// same signal must survive the translated source-fill path.

import (
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

const anthropicOverflowStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnthropicUsageOverflowMarksInvalid(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		format string
		body   string
	}{
		{
			name: "native anthropic",
			path: "/v1/messages",
			body: `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "translated chat",
			path:   "/v1/chat/completions",
			format: "anthropic",
			body:   `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, anthropicOverflowStream)
			buf := metrics.NewBuffer(8)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if !rec.UsageInvalid {
				t.Fatalf("usage not invalidated: %+v", rec)
			}
			if rec.Usage != (metrics.Usage{}) {
				t.Fatalf("invalid usage retained counts: %+v", rec.Usage)
			}
			if rec.ErrorCode != metrics.CodeInvalidUsage || rec.ErrorType == "" {
				t.Fatalf("record = type %q code %q, want a non-empty type with invalid_usage", rec.ErrorType, rec.ErrorCode)
			}
			if !rec.IsError() {
				t.Fatalf("record is not an error: %+v", rec)
			}
			if got := settleTokens(rec); got != scheduler.TokensUnavailable {
				t.Fatalf("settled tokens = %d, want TokensUnavailable", got)
			}
			if up.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
			}
		})
	}
}
