package format

// N14 regressions: an announced-but-empty content part or reasoning item must
// not suppress the authoritative value from the terminal document. Payload
// carried only by a start/done event must also reach every target once.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

func TestResponsesAnnouncedEmptyChannelsReceiveTerminalPayload(t *testing.T) {
	cases := []struct {
		name      string
		source    string
		wantText  string
		reasoning bool
	}{
		{
			name: "empty text part",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}

`,
			wantText: "hello",
		},
		{
			name: "empty reasoning item",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}]}}

`,
			wantText:  "think",
			reasoning: true,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				source := terminalStreamCreated + tc.source
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				got := textDeltaCount(t, target.inbound, buf.String(), tc.wantText)
				if tc.reasoning {
					got = reasoningDeltaCount(t, target.inbound, buf.String(), tc.wantText)
				}
				if got != 1 {
					t.Fatalf("%q emitted %d times, want once: %s", tc.wantText, got, buf.String())
				}
			})
		}
	}

	t.Run("record answer content", func(t *testing.T) {
		rec := responsesRecord(t, terminalStreamCreated+cases[0].source)
		if !rec.HadAnswerContent {
			t.Fatalf("record has no answer content: %+v", rec)
		}
	})
	t.Run("record reasoning presence", func(t *testing.T) {
		rec := responsesRecord(t, terminalStreamCreated+cases[1].source)
		if rec.FinishReason != "stop" || rec.HadAnswerContent {
			t.Fatalf("record = %+v, want a reasoning-only stop", rec)
		}
	})
}

func TestResponsesDonePayloadWithoutDeltas(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{
			name: "output_item.added summary",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`,
		},
		{
			name: "reasoning_summary_part.added payload",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"think"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`,
		},
		{
			name: "reasoning_summary_text.done payload",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_text.done
data: {"type":"response.reasoning_summary_text.done","output_index":0,"summary_index":0,"text":"think"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`,
		},
		{
			name: "reasoning_text.done payload",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_text.done
data: {"type":"response.reasoning_text.done","output_index":0,"content_index":0,"text":"think"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := reasoningDeltaCount(t, target.inbound, buf.String(), "think"); got != 1 {
					t.Fatalf("reasoning payload emitted %d times, want once: %s", got, buf.String())
				}
			})
		}
	}
}

func TestResponsesOutputTextDoneWithoutDelta(t *testing.T) {
	source := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.done
data: {"type":"response.output_text.done","output_index":0,"content_index":0,"text":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+source), nil, nil); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := textDeltaCount(t, target.inbound, buf.String(), "hello"); got != 1 {
				t.Fatalf("done text emitted %d times, want once: %s", got, buf.String())
			}
		})
	}
}

// responsesRecord analyzes the raw Responses source exactly like the proxy.
func responsesRecord(t *testing.T, source string) metrics.Record {
	t.Helper()
	now := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	var a sse.Analyzer
	a.CapturePreview = true
	for _, line := range strings.Split(source, "\n") {
		a.Feed([]byte(line), now)
	}
	var rec metrics.Record
	a.Fill(&rec)
	metrics.FinalizeRecord(&rec)
	return rec
}

// textDeltaCount counts target-protocol answer-text delta emissions equal to
// want. A duplicated block emits a second delta.
func textDeltaCount(t *testing.T, protocol ir.Protocol, out, want string) int {
	t.Helper()
	count := 0
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] == "response.output_text.delta" && payload["delta"] == want {
				count++
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "text_delta" && delta["text"] == want {
				count++
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			if content, _ := chatDeltaContent(payload); content == want {
				count++
			}
		}
	}
	return count
}

// reasoningDeltaCount counts target-protocol reasoning delta emissions equal to
// want.
func reasoningDeltaCount(t *testing.T, protocol ir.Protocol, out, want string) int {
	t.Helper()
	count := 0
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			typ, _ := payload["type"].(string)
			if (typ == "response.reasoning_summary_text.delta" || typ == "response.reasoning_text.delta") &&
				payload["delta"] == want {
				count++
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "thinking_delta" && delta["thinking"] == want {
				count++
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			choices, _ := payload["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			if delta["reasoning_content"] == want {
				count++
			}
		}
	}
	return count
}

// TestResponsesNonEmptyStartPayloadEmittedOnce pins R4-1: a non-empty
// content_part.added start payload is already on the target wire, so a
// repeated delta or terminal copy must add only the missing suffix.
func TestResponsesNonEmptyStartPayloadEmittedOnce(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		source    string
		countFunc func(*testing.T, ir.Protocol, string, string) int
	}{
		{
			name:  "text terminal repeat",
			value: "hello",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}

`,
			countFunc: textDeltaCount,
		},
		{
			name:  "text repeated delta",
			value: "hello",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}

`,
			countFunc: textDeltaCount,
		},
		{
			name:  "refusal",
			value: "nope",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"refusal","refusal":"nope"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"nope"}]}]}}

`,
			countFunc: refusalDeltaCount,
		},
		{
			name:  "reasoning content",
			value: "think",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"reasoning_text","text":"think"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"think"}]}]}}

`,
			countFunc: reasoningDeltaCount,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := tc.countFunc(t, target.inbound, buf.String(), tc.value); got != 1 {
					t.Fatalf("%q emitted %d times, want once: %s", tc.value, got, buf.String())
				}
			})
		}
	}

	rec := responsesRecord(t, terminalStreamCreated+cases[0].source)
	if !rec.HadAnswerContent || rec.Chunks != 1 || rec.AnswerTokens != 1 || rec.GenTokens != 1 {
		t.Fatalf("record = %+v, want one answer chunk without usage", rec)
	}
}

func refusalDeltaCount(t *testing.T, protocol ir.Protocol, out, want string) int {
	t.Helper()
	count := 0
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			if payload["type"] == "response.refusal.delta" && payload["delta"] == want {
				count++
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] == "text_delta" && delta["text"] == want {
				count++
			}
		}
	case ir.Chat:
		for _, payload := range chatPayloads(t, out) {
			choices, _ := payload["choices"].([]any)
			if len(choices) == 0 {
				continue
			}
			delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
			if delta["refusal"] == want {
				count++
			}
		}
	}
	return count
}

// TestResponsesReasoningSummaryPartCoverage pins R4-3: summary coverage is
// tracked per summary_index, so a later part or a longer terminal summary is
// not suppressed by the first part's bytes.
func TestResponsesReasoningSummaryPartCoverage(t *testing.T) {
	cases := []struct {
		name   string
		source string
	}{
		{
			name: "two summary parts",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"alpha"}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":"beta"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"},{"type":"summary_text","text":"beta"}]}]}}

`,
		},
		{
			name: "short added summary longer terminal",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"},{"type":"summary_text","text":"beta"}]}]}}

`,
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				for _, want := range []string{"alpha", "beta"} {
					if got := reasoningDeltaCount(t, target.inbound, buf.String(), want); got != 1 {
						t.Fatalf("%q emitted %d times, want once: %s", want, got, buf.String())
					}
				}
			})
		}
	}
}
