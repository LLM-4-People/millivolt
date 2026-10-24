package format

// Round 4 batch 11 regression (B3): an oversized Chat response content array
// fails the translation closed instead of becoming a 200 empty completion.

import (
	"encoding/json"
	"testing"
)

func chatCompletionWithParts(n int) []byte {
	parts := make([]map[string]any, n)
	for i := range parts {
		parts[i] = map[string]any{"type": "text", "text": "x"}
	}
	body, err := json.Marshal(map[string]any{
		"id": "c1", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": parts},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func TestChatResponseOversizedContentFailsClosed(t *testing.T) {
	resp, err := DecodeChatResponse(chatCompletionWithParts(4096))
	if err != nil {
		t.Fatalf("4096-part response rejected: %v", err)
	}
	if len(resp.Blocks) != 4096 {
		t.Fatalf("4096-part blocks = %d, want every part", len(resp.Blocks))
	}
	if _, err := DecodeChatResponse(chatCompletionWithParts(4097)); err == nil {
		t.Fatal("4097-part response decoded successfully")
	}
}
