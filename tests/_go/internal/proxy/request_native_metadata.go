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
