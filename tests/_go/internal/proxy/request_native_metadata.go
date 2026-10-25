package proxy

// Round 11 A2 regression: native Responses metadata uses text.format,
// text.verbosity, include logprobs and the function tool-choice spelling. The
// record decoder read only the Chat/Anthropic spellings, so a native request
// stored an empty format, verbosity and tool choice and req_logprobs=false.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func parseRequestRecord(t *testing.T, body string) metrics.Record {
	t.Helper()
	var rec metrics.Record
	if err := parseLLMRequest([]byte(body), &rec, false); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return rec
}

func TestResponsesNativeRequestMetadata(t *testing.T) {
	body := `{"model":"m","input":"hi",` +
		`"text":{"format":{"type":"json_object"},"verbosity":"low"},` +
		`"include":["message.output_text.logprobs"],` +
		`"tool_choice":{"type":"function","name":"lookup"},` +
		`"tools":[{"type":"function","name":"lookup","parameters":{}}]}`
	rec := parseRequestRecord(t, body)
	if rec.ReqResponseFormat != "json_object" {
		t.Fatalf("response format = %q, want text.format json_object", rec.ReqResponseFormat)
	}
	if rec.ReqVerbosity != "low" {
		t.Fatalf("verbosity = %q, want text.verbosity low", rec.ReqVerbosity)
	}
	if !rec.ReqLogprobs {
		t.Fatal("include message.output_text.logprobs did not record req_logprobs")
	}
	if rec.ReqToolChoice != "tool:lookup" {
		t.Fatalf("tool choice = %q, want the function name", rec.ReqToolChoice)
	}
	if rec.ReqToolsCount != 1 {
		t.Fatalf("tools = %d, want 1", rec.ReqToolsCount)
	}

	// The translated/Chat control keeps the existing spellings.
	chat := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_object"},"verbosity":"high",` +
		`"tool_choice":{"type":"function","function":{"name":"lookup"}}}`
	rec = parseRequestRecord(t, chat)
	if rec.ReqResponseFormat != "json_object" || rec.ReqVerbosity != "high" || rec.ReqToolChoice != "tool:lookup" {
		t.Fatalf("chat control = format %q verbosity %q choice %q", rec.ReqResponseFormat, rec.ReqVerbosity, rec.ReqToolChoice)
	}

	// An include array without the logprob option, including non-string
	// members, records false and does not erase the rest of the record.
	other := `{"model":"m","input":"hi","text":{"format":{"type":"text"},"verbosity":"medium"},` +
		`"include":["message.output_text.annotations",5,{"x":1}]}`
	rec = parseRequestRecord(t, other)
	if rec.ReqLogprobs {
		t.Fatal("an unrelated include option recorded req_logprobs")
	}
	if rec.ReqResponseFormat != "text" || rec.ReqVerbosity != "medium" {
		t.Fatalf("other include row lost metadata: format %q verbosity %q", rec.ReqResponseFormat, rec.ReqVerbosity)
	}
}

// TestAnthropicThinkingDisabledNotRequested pins what the record's
// req_thinking means: the client asked for a thinking budget. A disabled
// thinking object is schema-valid but requests nothing.
func TestAnthropicThinkingDisabledNotRequested(t *testing.T) {
	for _, tc := range []struct {
		name     string
		thinking string
		want     bool
	}{
		{name: "disabled", thinking: `{"type":"disabled"}`, want: false},
		{name: "enabled", thinking: `{"type":"enabled","budget_tokens":2048}`, want: true},
		{name: "adaptive", thinking: `{"type":"adaptive"}`, want: true},
		{name: "between tools", thinking: `{"type":"between_tools"}`, want: true},
		{name: "null", thinking: `null`, want: false},
		{name: "malformed", thinking: `"nope"`, want: false},
		{name: "absent", thinking: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}]`
			if tc.thinking != "" {
				body += `,"thinking":` + tc.thinking
			}
			body += `}`
			rec := parseRequestRecord(t, body)
			if rec.ReqThinking != tc.want {
				t.Fatalf("req_thinking = %v, want %v for %s", rec.ReqThinking, tc.want, tc.thinking)
			}
		})
	}
}

// TestChatLegacyFunctionsMetadata pins the deprecated Chat functions and
// function_call record: the codec still converts them, so the record must
// report the definitions offered and the requested policy.
func TestChatLegacyFunctionsMetadata(t *testing.T) {
	fn := `{"name":"lookup","parameters":{}}`

	rec := parseRequestRecord(t, `{"model":"m","functions":[`+fn+`],`+
		`"function_call":{"name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.ReqToolsCount != 1 {
		t.Fatalf("functions count = %d, want 1", rec.ReqToolsCount)
	}
	if rec.ReqToolChoice != "tool:lookup" {
		t.Fatalf("function_call choice = %q, want tool:lookup", rec.ReqToolChoice)
	}

	// Modern tools and legacy functions are both offered upstream.
	rec = parseRequestRecord(t, `{"model":"m","tools":[{"type":"function","function":{"name":"modern","parameters":{}}}],`+
		`"functions":[`+fn+`],"messages":[{"role":"user","content":"hi"}]}`)
	if rec.ReqToolsCount != 2 {
		t.Fatalf("tools+functions count = %d, want 2", rec.ReqToolsCount)
	}

	// tool_choice wins over function_call, matching the codec's precedence.
	rec = parseRequestRecord(t, `{"model":"m","functions":[`+fn+`],`+
		`"tool_choice":"auto","function_call":{"name":"lookup"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.ReqToolChoice != "auto" {
		t.Fatalf("tool_choice precedence = %q, want auto", rec.ReqToolChoice)
	}

	for _, tc := range []struct {
		call string
		want string
	}{
		{call: `"auto"`, want: "auto"},
		{call: `"none"`, want: "none"},
		{call: `"required"`, want: "required"},
		{call: `{"name":"lookup"}`, want: "tool:lookup"},
		{call: `"unsupported"`, want: ""},
		{call: `{"name":""}`, want: ""},
		{call: `42`, want: ""},
	} {
		rec := parseRequestRecord(t, `{"model":"m","function_call":`+tc.call+`,"messages":[{"role":"user","content":"hi"}]}`)
		if rec.ReqToolChoice != tc.want {
			t.Fatalf("function_call %s = %q, want %q", tc.call, rec.ReqToolChoice, tc.want)
		}
	}

	// A schema-invalid functions member records its lexical count instead of
	// erasing the rest of the record.
	rec = parseRequestRecord(t, `{"model":"m","temperature":0.5,"functions":[42],`+
		`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.ReqToolsCount != 1 || rec.ReqTemperature == nil || *rec.ReqTemperature != 0.5 {
		t.Fatalf("malformed functions record = tools %d temperature %v, want 1/0.5", rec.ReqToolsCount, rec.ReqTemperature)
	}
}
