package proxy

// Round 6 F2 regression: a translated Responses stream carrying only a
// provider-executed tool item records a tool finish and does not bill the
// usage output as answer or generation tokens.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const providerToolItem = `{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"shell","input":"x"}`

const providerToolStreamBody = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":` + providerToolItem + `}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":` + providerToolItem + `}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[` + providerToolItem + `],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}

`

func TestTranslatedResponsesProviderToolCallRecordsToolFinish(t *testing.T) {
	for _, tc := range []struct {
		name         string
		path         string
		body         string
		wantFinish   string
		forbidFinish string
	}{
		{
			name:         "chat target",
			path:         "/v1/chat/completions",
			body:         `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantFinish:   `"finish_reason":"tool_calls"`,
			forbidFinish: `"finish_reason":"stop"`,
		},
		{
			name:         "anthropic target",
			path:         "/v1/messages",
			body:         `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			wantFinish:   `"stop_reason":"tool_use"`,
			forbidFinish: `"stop_reason":"end_turn"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, providerToolStreamBody)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, "openai-responses", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantFinish) {
				t.Fatalf("client stream does not finish as a tool turn: %s", w.Body.String())
			}
			if strings.Contains(w.Body.String(), tc.forbidFinish) {
				t.Fatalf("client stream finished cleanly: %s", w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.FinishReason != "tool_calls" {
				t.Fatalf("finish = %q, want tool_calls", rec.FinishReason)
			}
			if rec.AnswerTokens != 0 || rec.GenTokens != 0 {
				t.Fatalf("tool-only turn billed answer/gen %d/%d, want 0/0", rec.AnswerTokens, rec.GenTokens)
			}
		})
	}
}
