package proxy

// Round 7 NEW-1 regression: an incremental Responses reasoning_text delta
// stream reaches the proxy client in full. The upstream and record stay
// healthy while every delta is visible, so a cumulative-suffix regression
// cannot silently drop thinking text.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const responsesReasoningDeltaStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"step "}

event: response.reasoning_text.delta
data: {"type":"response.reasoning_text.delta","output_index":0,"content_index":0,"delta":"one"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`

func TestTranslatedReasoningTextDeltasReachClient(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		marker string
	}{
		{
			name:   "anthropic client",
			path:   "/v1/messages",
			body:   `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			marker: `"thinking"`,
		},
		{
			name:   "chat client",
			path:   "/v1/chat/completions",
			body:   `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			marker: `"reasoning_content"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, responsesReasoningDeltaStream)
			buf := metrics.NewBuffer(8)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, "openai-responses", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			got := w.Body.String()
			for _, want := range []string{
				tc.marker + `:"step "`,
				tc.marker + `:"one"`,
			} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in the client stream: %s", want, got)
				}
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != "" || rec.ErrorCode != "" || rec.IsError() {
				t.Fatalf("record = type %q code %q error %v, want healthy", rec.ErrorType, rec.ErrorCode, rec.IsError())
			}
			if rec.FinishReason != "stop" {
				t.Fatalf("record finish = %q, want stop", rec.FinishReason)
			}
			if rec.HadAnswerContent {
				t.Fatalf("reasoning-only stream marked answer content: %+v", rec)
			}
		})
	}
}
