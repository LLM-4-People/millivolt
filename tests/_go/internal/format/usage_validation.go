package format

// Batch 13 regressions: impossible provider usage never reaches the IR, the
// client wire or the record, every fold is checked, and the Responses detail
// paths are parsed by the shared usage owner.

import (
	"math"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatAndResponsesUsageValidation(t *testing.T) {
	const chatChoices = `"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]`
	cases := []struct {
		name  string
		body  string
		parse func([]byte) (*ir.Response, error)
	}{
		{
			name: "chat negative",
			body: `{"id":"c1","object":"chat.completion","created":1,"model":"m",` + chatChoices +
				`,"usage":{"prompt_tokens":-5,"completion_tokens":-1,"total_tokens":-6}}`,
			parse: func(b []byte) (*ir.Response, error) { return DecodeChatResponse(b) },
		},
		{
			name: "chat cache exceeds input",
			body: `{"id":"c1","object":"chat.completion","created":1,"model":"m",` + chatChoices +
				`,"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6,` +
				`"prompt_tokens_details":{"cached_tokens":9}}}`,
			parse: func(b []byte) (*ir.Response, error) { return DecodeChatResponse(b) },
		},
		{
			name: "responses negative",
			body: `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[],` +
				`"usage":{"input_tokens":-1,"output_tokens":0,"total_tokens":-1}}`,
			parse: func(b []byte) (*ir.Response, error) { return DecodeResponsesResponse(b) },
		},
		{
			name: "responses cache exceeds input",
			body: `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[],` +
				`"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,` +
				`"input_tokens_details":{"cached_tokens":2}}}`,
			parse: func(b []byte) (*ir.Response, error) { return DecodeResponsesResponse(b) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.parse([]byte(tc.body)); err == nil {
				t.Fatalf("accepted impossible usage: %s", tc.body)
			}
		})
	}

	// A valid document still decodes.
	okChat := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` + chatChoices +
		`,"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`
	if _, err := DecodeChatResponse([]byte(okChat)); err != nil {
		t.Fatalf("valid Chat usage rejected: %v", err)
	}
	okResponses := `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[],` +
		`"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}`
	if _, err := DecodeResponsesResponse([]byte(okResponses)); err != nil {
		t.Fatalf("valid Responses usage rejected: %v", err)
	}

	// The encoders reject impossible usage even when the IR was built
	// directly rather than decoded.
	bad := &ir.Response{
		ID: "r1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{ir.TextBlock("ok")},
		Usage:  ir.Usage{InputTokens: -1, OutputTokens: 1},
	}
	if _, err := EncodeChatResponse(bad); err == nil {
		t.Fatal("Chat encoder accepted negative usage")
	}
	if _, err := EncodeResponsesResponse(bad); err == nil {
		t.Fatal("Responses encoder accepted negative usage")
	}
	if _, err := EncodeAnthropicResponse(bad); err == nil {
		t.Fatal("Anthropic encoder accepted negative usage")
	}
}

func TestUsageArithmeticDoesNotWrap(t *testing.T) {
	cases := []string{
		`{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}`,
		`{"input_tokens":9223372036854775807,"output_tokens":1}`,
		`{"input_tokens":0,"output_tokens":9223372036854775807,"cache_creation_input_tokens":1}`,
	}
	for _, usage := range cases {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":` + usage + `}`
		if _, err := DecodeAnthropicResponse([]byte(body)); err == nil {
			t.Fatalf("Anthropic usage overflow accepted: %s", body)
		}
	}

	// The total fallback and the Anthropic subtraction cannot wrap either.
	overflow := &ir.Response{
		ID: "r1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{ir.TextBlock("ok")},
		Usage:  ir.Usage{InputTokens: math.MaxInt64, OutputTokens: 1},
	}
	if _, err := EncodeChatResponse(overflow); err == nil {
		t.Fatal("Chat total fallback accepted an overflowing sum")
	}
	if _, err := EncodeResponsesResponse(overflow); err == nil {
		t.Fatal("Responses total fallback accepted an overflowing sum")
	}
	if _, err := EncodeAnthropicResponse(overflow); err == nil {
		t.Fatal("Anthropic encoder accepted an overflowing sum")
	}

	// The streaming Anthropic fold reports the overflow before any usage
	// document is installed.
	stream := "data: " + `{"type":"message_start","message":{"id":"msg_1","model":"m",` +
		`"usage":{"input_tokens":9223372036854775807,"cache_read_input_tokens":1}}}` + "\n\n"
	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err == nil {
		t.Fatal("Anthropic stream fold accepted wrapped usage")
	}
}

func TestChatStreamUsageValidation(t *testing.T) {
	for name, usage := range map[string]string{
		"negative": `{"prompt_tokens":-5,"completion_tokens":1,"total_tokens":-4}`,
		"overflow": `{"prompt_tokens":9223372036854775807,"completion_tokens":1,"total_tokens":0}`,
	} {
		t.Run("chat "+name, func(t *testing.T) {
			stream := "data: " + `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
				`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n" +
				"data: " + `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
				`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":` + usage + `}` + "\n\n" +
				"data: [DONE]\n\n"
			var out strings.Builder
			if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &out, strings.NewReader(stream), nil, nil, StreamOptions{}); err == nil {
				t.Fatalf("Chat stream accepted impossible usage: %s", usage)
			}
		})
	}

	for name, usage := range map[string]string{
		"negative": `{"input_tokens":-1,"output_tokens":0,"total_tokens":-1}`,
		"overflow": `{"input_tokens":9223372036854775807,"output_tokens":1,"total_tokens":0}`,
	} {
		t.Run("responses "+name, func(t *testing.T) {
			stream := "data: " + `{"type":"response.created","response":{"id":"r1","object":"response",` +
				`"created_at":1,"status":"in_progress","model":"m","output":[]}}` + "\n\n" +
				"data: " + `{"type":"response.completed","response":{"id":"r1","object":"response",` +
				`"created_at":1,"status":"completed","model":"m","output":[],"usage":` + usage + `}}` + "\n\n"
			var out strings.Builder
			if err := TranslateStreamBetween(ir.Chat, ir.Responses, &out, strings.NewReader(stream), nil, nil, StreamOptions{}); err == nil {
				t.Fatalf("Responses stream accepted impossible usage: %s", usage)
			}
		})
	}

	// Valid streams still translate.
	chat := "data: " + `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m",` +
		`"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}` + "\n\n" +
		"data: [DONE]\n\n"
	var chatOut strings.Builder
	if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &chatOut, strings.NewReader(chat), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("valid Chat stream rejected: %v", err)
	}

	responses := "data: " + `{"type":"response.created","response":{"id":"r1","object":"response",` +
		`"created_at":1,"status":"in_progress","model":"m","output":[]}}` + "\n\n" +
		"data: " + `{"type":"response.completed","response":{"id":"r1","object":"response",` +
		`"created_at":1,"status":"completed","model":"m","output":[],` +
		`"usage":{"input_tokens":5,"output_tokens":1,"total_tokens":6}}}` + "\n\n"
	var responsesOut strings.Builder
	if err := TranslateStreamBetween(ir.Chat, ir.Responses, &responsesOut, strings.NewReader(responses), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("valid Responses stream rejected: %v", err)
	}
}
