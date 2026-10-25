package format

// Round 2 DUP-3/F6-2 regressions: done payloads are authoritative. A complete
// part, item, summary part or function-call argument value arriving in a done
// event must be emitted once, and an omitted part payload must not open an
// empty target block.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func translateDoneFixture(t *testing.T, source string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, target := range terminalTargets {
		var buf bytes.Buffer
		if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil, StreamOptions{}); err != nil {
			t.Fatalf("%s: TranslateStreamBetween: %v", target.name, err)
		}
		out[target.name] = buf.String()
	}
	return out
}

func TestResponsesContentPartDoneCarriesValue(t *testing.T) {
	t.Run("deferred part", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
		out := translateDoneFixture(t, source)
		for _, target := range terminalTargets {
			if got := strings.Join(textEmissionOrder(t, target.inbound, out[target.name]), ""); got != "hello" {
				t.Fatalf("%s text = %q, want hello", target.name, got)
			}
			if target.inbound == ir.Anthropic {
				if got := emptyAnthropicTextBlocks(t, out[target.name]); got != 0 {
					t.Fatalf("anthropic empty text blocks = %d, want 0", got)
				}
			}
		}
	})

	t.Run("started part receives only the suffix", func(t *testing.T) {
		source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hel"}}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
		out := translateDoneFixture(t, source)
		for _, target := range terminalTargets {
			if got := strings.Join(textEmissionOrder(t, target.inbound, out[target.name]), ""); got != "hello" {
				t.Fatalf("%s text = %q, want hello exactly once", target.name, got)
			}
		}
	})
}

func TestResponsesContentPartDoneOmitsPartOpensNoBlock(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	out := translateDoneFixture(t, source)
	for _, target := range terminalTargets {
		if got := textEmissionOrder(t, target.inbound, out[target.name]); len(got) != 0 {
			t.Fatalf("%s text = %v, want none", target.name, got)
		}
		if target.inbound == ir.Anthropic {
			if got := emptyAnthropicTextBlocks(t, out[target.name]); got != 0 {
				t.Fatalf("anthropic empty text blocks = %d, want 0", got)
			}
		}
	}
}

func TestResponsesOutputItemDoneCarriesContent(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	out := translateDoneFixture(t, source)
	for _, target := range terminalTargets {
		if got := strings.Join(textEmissionOrder(t, target.inbound, out[target.name]), ""); got != "hello" {
			t.Fatalf("%s text = %q, want hello", target.name, got)
		}
		if target.inbound == ir.Anthropic {
			if got := emptyAnthropicTextBlocks(t, out[target.name]); got != 0 {
				t.Fatalf("anthropic empty text blocks = %d, want 0", got)
			}
		}
	}
}

func TestResponsesReasoningSummaryPartDoneCarriesText(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}

event: response.reasoning_summary_part.done
data: {"type":"response.reasoning_summary_part.done","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"think"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	out := translateDoneFixture(t, source)
	for _, target := range terminalTargets {
		if got := strings.Join(reasoningEmissionOrder(t, target.inbound, out[target.name]), ""); got != "think" {
			t.Fatalf("%s reasoning = %q, want think", target.name, got)
		}
	}
}

func TestResponsesOutputItemDoneCarriesReasoningContent(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[],"content":[{"type":"reasoning_text","text":"deep"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	out := translateDoneFixture(t, source)
	for _, target := range terminalTargets {
		if got := strings.Join(reasoningEmissionOrder(t, target.inbound, out[target.name]), ""); got != "deep" {
			t.Fatalf("%s reasoning = %q, want deep", target.name, got)
		}
	}
}

// toolArgumentDeltas returns every argument delta the target emitted, so a
// duplicated done emission is visible as two entries instead of one joined
// value.
func toolArgumentDeltas(t *testing.T, protocol ir.Protocol, out string) []string {
	t.Helper()
	var deltas []string
	switch protocol {
	case ir.Chat:
		for _, frame := range strings.Split(out, "\n\n") {
			payload, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
			if !ok || payload == "[DONE]" {
				continue
			}
			var obj map[string]any
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				t.Fatalf("frame %q: %v", frame, err)
			}
			choices, _ := obj["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			calls, _ := delta["tool_calls"].([]any)
			for _, c := range calls {
				fn, _ := c.(map[string]any)["function"].(map[string]any)
				if v, ok := fn["arguments"].(string); ok && v != "" {
					deltas = append(deltas, v)
				}
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if partial, ok := delta["partial_json"].(string); ok && partial != "" {
				deltas = append(deltas, partial)
			}
		}
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] != "response.function_call_arguments.delta" {
				continue
			}
			if delta, _ := payload["delta"].(string); delta != "" {
				deltas = append(deltas, delta)
			}
		}
	}
	return deltas
}

func TestResponsesFunctionCallArgumentsDoneWithoutDelta(t *testing.T) {
	// The done mirror carries no arguments, so the complete value can only
	// arrive through function_call_arguments.done. Asserting the joined value
	// is not enough on its own: a duplicated full emission joins to an
	// invalid JSON string that still contains the expected substring, so the
	// delta count is asserted too.
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"q\":1}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	out := translateDoneFixture(t, source)
	for _, target := range terminalTargets {
		deltas := toolArgumentDeltas(t, target.inbound, out[target.name])
		got := strings.Join(deltas, "")
		if got != `{"q":1}` {
			t.Fatalf("%s arguments = %q, want exactly the done value", target.name, got)
		}
		if n := strings.Count(got, `"q":1`); n != 1 {
			t.Fatalf("%s emitted %d complete argument values, want 1", target.name, n)
		}
	}
}

func TestResponsesFunctionCallArgumentsDoneAfterDelta(t *testing.T) {
	// A streamed prefix followed by the authoritative done value contributes
	// only its missing suffix: the client must never receive the complete
	// arguments twice.
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"q\":"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","output_index":0,"arguments":"{\"q\":1}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	out := translateDoneFixture(t, source)
	for _, target := range terminalTargets {
		deltas := toolArgumentDeltas(t, target.inbound, out[target.name])
		got := strings.Join(deltas, "")
		if got != `{"q":1}` {
			t.Fatalf("%s arguments = %q, want the streamed prefix plus the recovered suffix", target.name, got)
		}
		if n := strings.Count(got, `"q":1`); n != 1 {
			t.Fatalf("%s emitted %d complete argument values, want 1", target.name, n)
		}
	}
}

// refusalEmissionOrder returns every refusal payload the target emitted.
// Anthropic has no refusal block, so its mapped text deltas are the refusal
// channel there.
func refusalEmissionOrder(t *testing.T, protocol ir.Protocol, out string) []string {
	t.Helper()
	var order []string
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] != "response.refusal.delta" {
				continue
			}
			if text, _ := payload["delta"].(string); text != "" {
				order = append(order, text)
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] != "text_delta" {
				continue
			}
			if text, _ := delta["text"].(string); text != "" {
				order = append(order, text)
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			choices, _ := payload["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			if text, _ := delta["refusal"].(string); text != "" {
				order = append(order, text)
			}
		}
	}
	return order
}

// TestResponsesRefusalPartDoneCarriesValue covers the refusal arm of the
// content_part.done matrix: deferred, partially streamed and type-less done
// payloads must stay on the refusal channel once, never as visible output text.
func TestResponsesRefusalPartDoneCarriesValue(t *testing.T) {
	cases := []struct {
		name  string
		added string
		done  string
	}{
		{name: "deferred part", added: `{"type":"refusal","refusal":""}`, done: `{"type":"refusal","refusal":"no"}`},
		{name: "started part", added: `{"type":"refusal","refusal":"n"}`, done: `{"type":"refusal","refusal":"no"}`},
		{name: "type-less done", added: `{"type":"refusal","refusal":""}`, done: `{"refusal":"no"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":` + tc.added + `}

event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":` + tc.done + `}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
			out := translateDoneFixture(t, source)
			for _, target := range terminalTargets {
				if got := strings.Join(refusalEmissionOrder(t, target.inbound, out[target.name]), ""); got != "no" {
					t.Fatalf("%s refusal = %q, want no: %s", target.name, got, out[target.name])
				}
				if target.inbound != ir.Anthropic {
					if got := strings.Join(textEmissionOrder(t, target.inbound, out[target.name]), ""); got != "" {
						t.Fatalf("%s refusal leaked into visible text %q", target.name, got)
					}
				}
			}
		})
	}
}
