package format

// Round 9 B1/B2 regression: Responses created_at is number/unixtime and the
// usage/count fields are JSON integers by value, so 1.5 and 5.0 are valid wire
// spellings. The shared jsonnum owner now decodes them exactly; fractional
// counts stay rejected.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesCreatedAtAcceptsSchemaNumbers(t *testing.T) {
	body := func(created string) string {
		return `{"id":"r1","object":"response","created_at":` + created + `,"status":"completed","model":"m","output":[]}`
	}
	for _, tc := range []struct {
		created string
		want    int64
	}{
		{"1.5", 1},
		{"1698982736.0", 1698982736},
		{"1", 1},
		{"1e3", 1000},
	} {
		t.Run(tc.created, func(t *testing.T) {
			resp, err := DecodeResponsesResponse([]byte(body(tc.created)))
			if err != nil {
				t.Fatalf("created_at %s rejected: %v", tc.created, err)
			}
			if resp.Created != tc.want {
				t.Fatalf("Created = %d, want %d", resp.Created, tc.want)
			}
		})
	}
	if _, err := DecodeResponsesResponse([]byte(body("-1"))); err == nil {
		t.Fatal("negative created_at accepted")
	}
}

func TestResponseIntegralNumbersAcrossProtocols(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		body := `{"id":"c1","object":"chat.completion","created":1.0,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":5.0,"completion_tokens":7.0,"total_tokens":12.0,` +
			`"prompt_tokens_details":{"cached_tokens":2.0,"cache_write_tokens":1.0},` +
			`"completion_tokens_details":{"reasoning_tokens":3.0}}}`
		resp, err := DecodeChatResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Created != 1 || resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 7 ||
			resp.Usage.TotalTokens != 12 || resp.Usage.CacheReadTokens != 2 ||
			resp.Usage.CacheWriteTokens != 1 || resp.Usage.ReasoningTokens != 3 {
			t.Fatalf("chat response numbers = created %d usage %+v", resp.Created, resp.Usage)
		}
		if _, err := DecodeChatResponse([]byte(strings.Replace(body, `"prompt_tokens":5.0`, `"prompt_tokens":5.5`, 1))); err == nil {
			t.Fatal("fractional chat prompt_tokens accepted")
		}
	})

	t.Run("responses", func(t *testing.T) {
		body := `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[],` +
			`"usage":{"input_tokens":5.0,"output_tokens":7.0,"total_tokens":12.0,` +
			`"input_tokens_details":{"cached_tokens":2.0,"cache_write_tokens":1.0},` +
			`"output_tokens_details":{"reasoning_tokens":3.0}}}`
		resp, err := DecodeResponsesResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Usage.InputTokens != 5 || resp.Usage.OutputTokens != 7 || resp.Usage.TotalTokens != 12 ||
			resp.Usage.CacheReadTokens != 2 || resp.Usage.CacheWriteTokens != 1 || resp.Usage.ReasoningTokens != 3 {
			t.Fatalf("responses usage = %+v", resp.Usage)
		}
		if _, err := DecodeResponsesResponse([]byte(strings.Replace(body, `"input_tokens":5.0`, `"input_tokens":5.5`, 1))); err == nil {
			t.Fatal("fractional responses input_tokens accepted")
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":5.0,"output_tokens":7.0,"cache_creation_input_tokens":1.0,` +
			`"cache_read_input_tokens":2.0,"output_tokens_details":{"thinking_tokens":3.0}}}`
		resp, err := DecodeAnthropicResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 7 || resp.Usage.TotalTokens != 15 ||
			resp.Usage.CacheReadTokens != 2 || resp.Usage.CacheWriteTokens != 1 || resp.Usage.ReasoningTokens != 3 {
			t.Fatalf("anthropic usage = %+v", resp.Usage)
		}
		if _, err := DecodeAnthropicResponse([]byte(strings.Replace(body, `"input_tokens":5.0`, `"input_tokens":5.5`, 1))); err == nil {
			t.Fatal("fractional anthropic input_tokens accepted")
		}
	})
}

func TestResponsesStreamCreatedAtAcceptsFractionalSeconds(t *testing.T) {
	source := `data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","created_at":1.5,"output":[]}}

data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","created_at":1.5,"output":[]}}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Responses, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "1.5") || !strings.Contains(out, `"created_at":1`) {
		t.Fatalf("stream created_at not truncated to 1: %s", out)
	}
}
