package proxy

// Round 4 batch 11 regression (B3): a translated client never receives a 200
// empty completion when the Chat source response's content array is oversized.

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func chatCompletionWithParts(parts int) []byte {
	content := make([]map[string]any, parts)
	for i := range content {
		content[i] = map[string]any{"type": "text", "text": "x"}
	}
	body, err := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func TestTranslatedChatResponseOversizedContentFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		parts int
		want  int
	}{
		{name: "4096 accepted", parts: 4096, want: 200},
		{name: "4097 fails closed", parts: 4097, want: 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", 200, string(chatCompletionWithParts(tc.parts)))
			buf := metrics.NewBuffer(8)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/messages", "openai", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if tc.want == 502 {
				if rec.ErrorType != "transform_error" || !rec.IsError() {
					t.Fatalf("record = type %q code %q error %v, want transform_error", rec.ErrorType, rec.ErrorCode, rec.IsError())
				}
				if len(w.Body.Bytes()) == 0 {
					t.Fatal("oversized response produced an empty 502 body")
				}
			}
		})
	}
}
