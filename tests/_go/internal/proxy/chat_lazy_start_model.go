package proxy

// Round 5 H6 regression: the relay passes the recorded request model as the
// translated Chat stream's fallback. An Anthropic source without message_start
// still names that model on every Chat chunk, and the role chunk appears once.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicLazyChatStream = `event: content_block_start
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

func TestTranslatedChatLazyStartUsesRequestModel(t *testing.T) {
	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, anthropicLazyChatStream)
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic",
		`{"model":"request-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	chunks := 0
	roles := 0
	for _, frame := range strings.Split(w.Body.String(), "\n\n") {
		data, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok || data == "[DONE]" || data == "" {
			continue
		}
		var payload struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Role string `json:"role"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("decode chunk %q: %v", data, err)
		}
		chunks++
		if payload.Model != "request-model" {
			t.Fatalf("chunk model = %q, want request-model: %s", payload.Model, w.Body.String())
		}
		for _, choice := range payload.Choices {
			if choice.Delta.Role == "assistant" {
				roles++
			}
		}
	}
	if chunks == 0 || roles != 1 {
		t.Fatalf("chunks = %d roles = %d, want a non-empty stream with one role chunk", chunks, roles)
	}
}
