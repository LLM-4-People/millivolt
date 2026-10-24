package format

// B2 regression: a Chat chunk body and a null first choice are rejected
// instead of decoding as an empty completed response.

import (
	"strings"
	"testing"
)

func TestChatResponseRejectsNonCompletion(t *testing.T) {
	invalid := []struct {
		name string
		body string
		want string
	}{
		{
			name: "chunk object",
			body: `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
			want: "is not a completion",
		},
		{
			name: "null first choice",
			body: `{"id":"c1","object":"chat.completion","model":"m","choices":[null]}`,
			want: "has no message object",
		},
		{
			name: "missing message",
			body: `{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop"}]}`,
			want: "has no message object",
		},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeChatResponse([]byte(tc.body)); err == nil {
				t.Fatal("non-completion body was accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}

	valid := []string{
		`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
		`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
		`{"id":"c1","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
	}
	for _, body := range valid {
		if _, err := DecodeChatResponse([]byte(body)); err != nil {
			t.Fatalf("valid completion rejected: %v (%s)", err, body)
		}
	}
}
