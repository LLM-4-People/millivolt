package metrics

import (
	"strings"
	"testing"
)

// TestAnalyzeNonStreamBodyOutcome pins the quality classification derived from
// the single analysis decode. The rows mirror the legacy ClassifyNonStreamBody
// contract, including reasoning-only, tool mismatch, null/absent content and
// provider-signed finish reasons.
func TestAnalyzeNonStreamBodyOutcome(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		want     string
		fieldsOK bool
	}{
		{
			"content string",
			`{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}]}`,
			"", true,
		},
		{
			"multimodal parts",
			`{"choices":[{"finish_reason":"stop","message":{"content":[{"type":"text","text":"y"},{"type":"image_url","image_url":{"url":"x"}}]}}]}`,
			"", true,
		},
		{
			"content null",
			`{"choices":[{"finish_reason":"stop","message":{"content":null}}]}`,
			CodeEmptyCompletion, true,
		},
		{
			"content absent",
			`{"choices":[{"finish_reason":"stop","message":{}}]}`,
			CodeEmptyCompletion, true,
		},
		{
			"empty content array",
			`{"choices":[{"finish_reason":"stop","message":{"content":[]}}]}`,
			CodeEmptyCompletion, true,
		},
		{
			"object content part still satisfies the outcome",
			`{"choices":[{"finish_reason":"stop","message":{"content":{"text":"x"}}}]}`,
			"", true,
		},
		{
			"tool mismatch",
			`{"choices":[{"finish_reason":"tool_calls","message":{"content":null}}]}`,
			CodeEmptyToolCall, true,
		},
		{
			"real tool call",
			`{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":"1","function":{"name":"x"}}]}}]}`,
			"", true,
		},
		{
			"reasoning content only",
			`{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":"thought"}}]}`,
			CodeReasoningOnly, true,
		},
		{
			"bare reasoning only",
			`{"choices":[{"finish_reason":"stop","message":{"content":"","reasoning":"thought"}}]}`,
			CodeReasoningOnly, true,
		},
		{
			"reasoning details only",
			`{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_details":[{"type":"thinking"}]}}]}`,
			CodeReasoningOnly, true,
		},
		{
			"reasoning text only",
			`{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_text":"thought"}}]}`,
			CodeReasoningOnly, true,
		},
		{
			"empty reasoning decoy",
			`{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":""}}]}`,
			CodeEmptyCompletion, true,
		},
		{
			"null reasoning decoy",
			`{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":null}}]}`,
			CodeEmptyCompletion, true,
		},
		{
			"reasoning and answer",
			`{"choices":[{"finish_reason":"stop","message":{"content":"hi","reasoning_content":"thought"}}]}`,
			"", true,
		},
		{
			"reasoning only with length is provider signed",
			`{"choices":[{"finish_reason":"length","message":{"content":null,"reasoning_content":"ran out"}}]}`,
			"", true,
		},
		{
			"reasoning only with interruption finish is rescuable",
			`{"choices":[{"finish_reason":"insufficient_system_resource","message":{"content":null,"reasoning_content":"thought"}}]}`,
			CodeReasoningOnly, true,
		},
		{
			"reasoning does not rescue a lying tool finish",
			`{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"reasoning_content":"thought","tool_calls":[]}}]}`,
			CodeEmptyToolCall, true,
		},
		{
			"usage only body is not chat shaped",
			`{"choices":[],"usage":{"prompt_tokens":1}}`,
			"", true,
		},
		{
			"responses api shape",
			`{"object":"response","output":[]}`,
			"", true,
		},
		{
			"malformed json",
			`not json`,
			"", false,
		},
		{
			"malformed finish reason type",
			`{"choices":[{"finish_reason":7,"message":{"content":"hi"}}]}`,
			"", false,
		},
		{
			"malformed usage type keeps classification",
			`{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":"bad"}`,
			"", false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AnalyzeNonStreamBody([]byte(c.body), nil, nil, false)
			if got.Outcome != c.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, c.want)
			}
			if got.RecordFieldsOK != c.fieldsOK {
				t.Fatalf("RecordFieldsOK = %v, want %v", got.RecordFieldsOK, c.fieldsOK)
			}
		})
	}
}

// TestAnalyzeNonStreamBodyFields pins the record-facing extraction from the
// same decode: usage vocabularies and overrides, model/id, finish, answer and
// tool accounting, and the gated preview.
func TestAnalyzeNonStreamBodyFields(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		usageKeys map[string]string
		preview   bool
		wantUsage Usage
		hasUsage  bool
		model     string
		id        string
		finish    string
		answer    bool
		tools     int
		names     []string
		wantPrev  string
		fieldsOK  bool
	}{
		{
			name:      "openai content and usage",
			body:      `{"id":"chat-1","model":"gpt-x","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			preview:   true,
			wantUsage: Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			hasUsage:  true,
			model:     "gpt-x",
			id:        "chat-1",
			finish:    "stop",
			answer:    true,
			wantPrev:  "hello",
			fieldsOK:  true,
		},
		{
			name:     "multimodal parts join text",
			body:     `{"choices":[{"finish_reason":"stop","message":{"content":[{"type":"text","text":"hel"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"lo"}]}}]}`,
			preview:  true,
			finish:   "stop",
			answer:   true,
			wantPrev: "hello",
			fieldsOK: true,
		},
		{
			name:     "all non text parts carry no answer",
			body:     `{"choices":[{"finish_reason":"stop","message":{"content":[{"type":"image_url","image_url":{"url":"x"}}]}}]}`,
			preview:  true,
			finish:   "stop",
			fieldsOK: true,
		},
		{
			name:     "content null with tool calls",
			body:     `{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":"c1","function":{"name":"lookup"}},{"id":"c2","function":{"name":"read"}}]}}]}`,
			finish:   "tool_calls",
			tools:    2,
			names:    []string{"lookup", "read"},
			fieldsOK: true,
		},
		{
			name:     "reasoning only has no answer",
			body:     `{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":"thought"}}]}`,
			preview:  true,
			finish:   "stop",
			fieldsOK: true,
		},
		{
			name:      "anthropic usage",
			body:      `{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}`,
			wantUsage: Usage{InputTokens: 50, OutputTokens: 20, CacheReadTokens: 10, CacheWrite: 5},
			hasUsage:  true,
			finish:    "stop",
			answer:    true,
			fieldsOK:  true,
		},
		{
			name:      "provider usage override",
			body:      `{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":{"x":{"in":7},"completion_tokens":3}}`,
			usageKeys: map[string]string{"input_tokens": "x.in"},
			wantUsage: Usage{InputTokens: 7, OutputTokens: 3},
			hasUsage:  true,
			finish:    "stop",
			answer:    true,
			fieldsOK:  true,
		},
		{
			name:     "usage null leaves usage absent",
			body:     `{"id":"chat-1","choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":null}`,
			id:       "chat-1",
			finish:   "stop",
			answer:   true,
			fieldsOK: true,
		},
		{
			name:      "usage only body still extracts usage",
			body:      `{"id":"chat-1","model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			wantUsage: Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			hasUsage:  true,
			model:     "m",
			id:        "chat-1",
			fieldsOK:  true,
		},
		{
			name:     "usage wrong type suppresses every record field",
			body:     `{"id":"chat-1","model":"m","choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":"bad"}`,
			model:    "m",
			id:       "chat-1",
			finish:   "stop",
			answer:   true,
			fieldsOK: false,
		},
		{
			name:     "model wrong type suppresses every record field",
			body:     `{"model":7,"choices":[{"finish_reason":"stop","message":{"content":"hi"}}]}`,
			finish:   "stop",
			answer:   true,
			fieldsOK: false,
		},
		{
			name:     "tool detail wrong type suppresses every record field",
			body:     `{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":7,"function":{"name":"lookup"}}]}}]}`,
			finish:   "tool_calls",
			tools:    1,
			fieldsOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AnalyzeNonStreamBody([]byte(c.body), c.usageKeys, nil, c.preview)
			if got.HasUsage != c.hasUsage || got.Usage != c.wantUsage {
				t.Fatalf("usage = %+v (has=%v), want %+v (has=%v)", got.Usage, got.HasUsage, c.wantUsage, c.hasUsage)
			}
			if got.Model != c.model || got.ID != c.id || got.FinishReason != c.finish {
				t.Fatalf("model/id/finish = %q/%q/%q, want %q/%q/%q", got.Model, got.ID, got.FinishReason, c.model, c.id, c.finish)
			}
			if got.HadAnswerContent != c.answer {
				t.Fatalf("HadAnswerContent = %v, want %v", got.HadAnswerContent, c.answer)
			}
			if got.ToolCalls != c.tools {
				t.Fatalf("ToolCalls = %d, want %d", got.ToolCalls, c.tools)
			}
			if len(c.names) > 0 || got.ToolNames != nil {
				if len(got.ToolNames) != len(c.names) {
					t.Fatalf("ToolNames = %v, want %v", got.ToolNames, c.names)
				}
				for i := range c.names {
					if got.ToolNames[i] != c.names[i] {
						t.Fatalf("ToolNames = %v, want %v", got.ToolNames, c.names)
					}
				}
			}
			if got.Preview != c.wantPrev {
				t.Fatalf("Preview = %q, want %q", got.Preview, c.wantPrev)
			}
			if got.RecordFieldsOK != c.fieldsOK {
				t.Fatalf("RecordFieldsOK = %v, want %v", got.RecordFieldsOK, c.fieldsOK)
			}
		})
	}
}

// TestAnalyzeNonStreamBodyErrorEnvelope pins the canonical envelope rules on
// the top-level error value captured by the same decode: object/string/numeric
// code forms, null and empty-object decoys, and nested or string-literal
// "error" keys that are not envelopes.
func TestAnalyzeNonStreamBodyErrorEnvelope(t *testing.T) {
	cases := []struct {
		name string
		body string
		typ  string
		code string
		msg  string
	}{
		{
			"object envelope",
			`{"error":{"type":"server_error","code":"server_error","message":"boom"}}`,
			"server_error", "server_error", "boom",
		},
		{
			"string envelope",
			`{"error":"failed hard"}`,
			"provider_error", "", "failed hard",
		},
		{
			"numeric code",
			`{"error":{"code":429,"message":"slow down"}}`,
			"provider_error", "429", "slow down",
		},
		{
			"type only",
			`{"error":{"type":"overloaded_error"}}`,
			"overloaded_error", "", "",
		},
		{
			"null decoy",
			`{"error":null,"choices":[{"finish_reason":"stop","message":{"content":""}}]}`,
			"", "", "",
		},
		{
			"empty object decoy",
			`{"error":{},"choices":[{"finish_reason":"stop","message":{"content":""}}]}`,
			"", "", "",
		},
		{
			"empty string decoy",
			`{"error":""}`,
			"", "", "",
		},
		{
			"numeric error value decoy",
			`{"error":7}`,
			"", "", "",
		},
		{
			"nested error is not an envelope",
			`{"outer":{"error":{"type":"server_error","message":"nested"}}}`,
			"", "", "",
		},
		{
			"escaped error key is an envelope",
			`{"err\u006fr":{"type":"server_error","message":"boom"}}`,
			"server_error", "", "boom",
		},
		{
			"string literal error is not an envelope",
			`{"choices":[{"finish_reason":"stop","message":{"content":"say \"error\": now"}}]}`,
			"", "", "",
		},
		{
			"malformed json",
			`{"error":{"type":"server_error"`,
			"", "", "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AnalyzeNonStreamBody([]byte(c.body), nil, nil, false)
			if got.ErrorType != c.typ || got.ErrorCode != c.code || got.ErrorMsg != c.msg {
				t.Fatalf("error = %q/%q/%q, want %q/%q/%q", got.ErrorType, got.ErrorCode, got.ErrorMsg, c.typ, c.code, c.msg)
			}
		})
	}
}

// TestAnalyzeNonStreamBodyErrorBounds pins the shared TruncatePreview bound on
// an arbitrary provider message.
func TestAnalyzeNonStreamBodyErrorBounds(t *testing.T) {
	msg := strings.Repeat("x", PreviewMaxBytes+40)
	body := `{"error":{"type":"server_error","message":"` + msg + `"}}`
	got := AnalyzeNonStreamBody([]byte(body), nil, nil, false)
	if got.ErrorMsg != TruncatePreview(msg) {
		t.Fatalf("error message = %q, want the bounded %q", got.ErrorMsg, TruncatePreview(msg))
	}
	if !strings.HasSuffix(got.ErrorMsg, "…") || len(got.ErrorMsg) > PreviewMaxBytes+len("…") {
		t.Fatalf("error message is not bounded at the rune-safe preview limit: %q", got.ErrorMsg)
	}
}

// TestAnalyzeNonStreamBodyCost pins the existing dynamic cost extractor:
// built-in nested/scalar/tick keys, absence, and a provider override.
func TestAnalyzeNonStreamBodyCost(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		costKeys []string
		want     float64
		hasCost  bool
	}{
		{
			"openrouter usage cost",
			`{"choices":[{"message":{"content":"hi"}}],"usage":{"cost":0.00123,"prompt_tokens":1}}`,
			nil, 0.00123, true,
		},
		{
			"hyper nested cost",
			`{"choices":[{"message":{"content":"hi"}}],"usage":{"cost":{"usd":0.001038,"hypercredits":0.02}}}`,
			nil, 0.001038, true,
		},
		{
			"top level cost",
			`{"choices":[{"message":{"content":"hi"}}],"cost":0.007}`,
			nil, 0.007, true,
		},
		{
			"xai ticks",
			`{"choices":[{"message":{"content":"hi"}}],"usage":{"cost_in_usd_ticks":1421200}}`,
			nil, 0.00014212, true,
		},
		{
			"absent",
			`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":10}}`,
			nil, 0, false,
		},
		{
			"provider override",
			`{"choices":[{"message":{"content":"hi"}}],"usage":{"custom_charge":0.009}}`,
			[]string{"usage.custom_charge"}, 0.009, true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AnalyzeNonStreamBody([]byte(c.body), nil, c.costKeys, false)
			if got.HasCost != c.hasCost || got.Cost != c.want {
				t.Fatalf("cost = %v (has=%v), want %v (has=%v)", got.Cost, got.HasCost, c.want, c.hasCost)
			}
		})
	}
}

// TestAnalyzeNonStreamBodyPreview pins the preview gates and bound: on/off,
// empty content, joined parts, and rune-safe truncation.
func TestAnalyzeNonStreamBodyPreview(t *testing.T) {
	long := strings.Repeat("a", PreviewMaxBytes+50)
	multi := strings.Repeat("é", PreviewMaxBytes)
	body := func(content string) []byte {
		return []byte(`{"choices":[{"finish_reason":"stop","message":{"content":"` + content + `"}}]}`)
	}
	if got := AnalyzeNonStreamBody(body("hello"), nil, nil, true); got.Preview != "hello" {
		t.Fatalf("preview = %q, want hello", got.Preview)
	}
	if got := AnalyzeNonStreamBody(body("hello"), nil, nil, false); got.Preview != "" {
		t.Fatalf("preview with capture off = %q, want empty", got.Preview)
	}
	if got := AnalyzeNonStreamBody(body(long), nil, nil, true); got.Preview != TruncatePreview(long) {
		t.Fatalf("long preview = %q, want %q", got.Preview, TruncatePreview(long))
	}
	if got := AnalyzeNonStreamBody(body(multi), nil, nil, true); got.Preview != TruncatePreview(multi) || !strings.HasSuffix(got.Preview, "…") {
		t.Fatalf("multibyte preview = %q, want rune-safe %q", got.Preview, TruncatePreview(multi))
	}
	empty := []byte(`{"choices":[{"finish_reason":"stop","message":{"content":""}}]}`)
	if got := AnalyzeNonStreamBody(empty, nil, nil, true); got.Preview != "" {
		t.Fatalf("empty-content preview = %q, want empty", got.Preview)
	}
	parts := []byte(`{"choices":[{"finish_reason":"stop","message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`)
	if got := AnalyzeNonStreamBody(parts, nil, nil, true); got.Preview != "ab" {
		t.Fatalf("parts preview = %q, want ab", got.Preview)
	}
}
