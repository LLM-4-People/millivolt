package proxy

// Round 8 A4 regression: the request record only decoded the Chat spellings of
// stop, tool choice, parallel tool use, reasoning effort, response format and
// service tier. A native Anthropic request therefore showed no stop count, no
// tool choice and no effort, and a Chat request translated to Anthropic lost the
// same metadata after the rewrite.

import (
	"testing"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func anthropicParameterBody(toolChoice string) string {
	return `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"stop_sequences":["END"],` +
		`"tool_choice":` + toolChoice + `,` +
		`"output_config":{"effort":"high","format":{"type":"json_schema"}},` +
		`"speed":"fast","metadata":{"user_id":"u1"}}`
}

func assertAnthropicParameters(t *testing.T, rec *metrics.Record) {
	t.Helper()
	if rec.ReqStop != 1 {
		t.Fatalf("ReqStop = %d, want 1", rec.ReqStop)
	}
	if rec.ReqToolChoice != "tool:f" {
		t.Fatalf("ReqToolChoice = %q, want tool:f", rec.ReqToolChoice)
	}
	if rec.ReqParallelTools == nil || *rec.ReqParallelTools {
		t.Fatalf("ReqParallelTools = %v, want false", rec.ReqParallelTools)
	}
	if rec.ReqReasoningEffort != "high" {
		t.Fatalf("ReqReasoningEffort = %q, want high", rec.ReqReasoningEffort)
	}
	if rec.ReqResponseFormat != "json_schema" {
		t.Fatalf("ReqResponseFormat = %q, want json_schema", rec.ReqResponseFormat)
	}
	if rec.ReqServiceTier != "fast" {
		t.Fatalf("ReqServiceTier = %q, want fast", rec.ReqServiceTier)
	}
	if rec.ReqMetadataKeys != 1 {
		t.Fatalf("ReqMetadataKeys = %d, want 1", rec.ReqMetadataKeys)
	}
}

func TestRequestParametersUnderstandAnthropicSpellings(t *testing.T) {
	t.Run("native anthropic", func(t *testing.T) {
		var rec metrics.Record
		body := anthropicParameterBody(`{"type":"tool","name":"f","disable_parallel_tool_use":true}`)
		if err := parseLLMRequest([]byte(body), &rec, false); err != nil {
			t.Fatal(err)
		}
		assertAnthropicParameters(t, &rec)
	})

	t.Run("chat translated to anthropic", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
			`"stop":["END"],"parallel_tool_calls":false,` +
			`"tool_choice":{"type":"function","function":{"name":"f"}},` +
			`"reasoning_effort":"high",` +
			`"response_format":{"type":"json_schema","json_schema":{"name":"x","schema":{"type":"object"}}},` +
			`"service_tier":"priority","user":"u1"}`
		translated, _, err := providerformat.TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
		if err != nil {
			t.Fatal(err)
		}
		var rec metrics.Record
		if err := parseGeneratedLLMRequest(translated, &rec, false); err != nil {
			t.Fatal(err)
		}
		assertAnthropicParameters(t, &rec)
	})

	t.Run("chat native unchanged", func(t *testing.T) {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
			`"stop":"END","tool_choice":{"type":"function","function":{"name":"f"}},` +
			`"parallel_tool_calls":false,"reasoning_effort":"high",` +
			`"response_format":{"type":"json_schema"},"service_tier":"priority","metadata":{"a":"b"}}`
		var rec metrics.Record
		if err := parseLLMRequest([]byte(body), &rec, false); err != nil {
			t.Fatal(err)
		}
		if rec.ReqStop != 1 || rec.ReqToolChoice != "tool:f" {
			t.Fatalf("stop/tool choice = %d/%q, want 1/tool:f", rec.ReqStop, rec.ReqToolChoice)
		}
		if rec.ReqParallelTools == nil || *rec.ReqParallelTools {
			t.Fatalf("ReqParallelTools = %v, want false", rec.ReqParallelTools)
		}
		if rec.ReqReasoningEffort != "high" || rec.ReqResponseFormat != "json_schema" ||
			rec.ReqServiceTier != "priority" {
			t.Fatalf("effort/format/tier = %q/%q/%q", rec.ReqReasoningEffort, rec.ReqResponseFormat, rec.ReqServiceTier)
		}
		if rec.ReqMetadataKeys != 1 {
			t.Fatalf("ReqMetadataKeys = %d, want 1", rec.ReqMetadataKeys)
		}
	})

	t.Run("anthropic any maps to required", func(t *testing.T) {
		var rec metrics.Record
		body := anthropicParameterBody(`{"type":"any","disable_parallel_tool_use":false}`)
		if err := parseLLMRequest([]byte(body), &rec, false); err != nil {
			t.Fatal(err)
		}
		if rec.ReqToolChoice != "required" {
			t.Fatalf("ReqToolChoice = %q, want required", rec.ReqToolChoice)
		}
		if rec.ReqParallelTools == nil || !*rec.ReqParallelTools {
			t.Fatalf("ReqParallelTools = %v, want true", rec.ReqParallelTools)
		}
	})

	t.Run("malformed tool choice records nothing", func(t *testing.T) {
		for _, choice := range []string{
			`{"type":"tool","name":7}`,
			`{"type":"tool"}`,
			`{"type":"any","disable_parallel_tool_use":"yes"}`,
			`[1,2]`,
		} {
			var rec metrics.Record
			body := anthropicParameterBody(choice)
			if err := parseLLMRequest([]byte(body), &rec, false); err != nil {
				t.Fatalf("malformed tool_choice %s failed the parse: %v", choice, err)
			}
			if rec.ReqToolChoice != "" || rec.ReqParallelTools != nil {
				t.Fatalf("malformed tool_choice %s recorded %q / %v", choice, rec.ReqToolChoice, rec.ReqParallelTools)
			}
		}
	})
}
