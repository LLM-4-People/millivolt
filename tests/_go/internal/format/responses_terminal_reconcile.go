package format

// F5-1 regressions: terminal content and reasoning-summary reconciliation is
// performed on the concatenated item/channel text, not per source index. A
// provider that merges or splits parts relative to the streamed announcements
// must not duplicate bytes that already reached the client.

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalContentPartition(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		wantTotal  string
		wantValues map[string]int
	}{
		{
			name: "two streamed parts one terminal part",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":1,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":1,"delta":" world"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello world"}]}]}}

`,
			wantTotal:  "hello world",
			wantValues: map[string]int{"hello": 1, " world": 1},
		},
		{
			name: "one streamed part two terminal parts",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello world"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"},{"type":"output_text","text":" world"}]}]}}

`,
			wantTotal:  "hello world",
			wantValues: map[string]int{"hello world": 1},
		},
		{
			// A provider may omit output_item.added and the part's item_id.
			// The terminal item then carries the only id, so reconciliation
			// must fall back to the announced output position instead of
			// treating the terminal item as a new one.
			name: "positional identity without item announcement",
			source: `event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}

`,
			wantTotal:  "hello",
			wantValues: map[string]int{"hello": 1},
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := strings.Join(textEmissionOrder(t, target.inbound, buf.String()), ""); got != tc.wantTotal {
					t.Fatalf("text = %q, want %q: %s", got, tc.wantTotal, buf.String())
				}
				for value, want := range tc.wantValues {
					if got := textDeltaCount(t, target.inbound, buf.String(), value); got != want {
						t.Fatalf("%q emitted %d times, want %d: %s", value, got, want, buf.String())
					}
				}
			})
		}
	}
}

func TestResponsesTerminalSummaryPartition(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		wantTotal  string
		wantValues map[string]int
	}{
		{
			name: "two streamed parts one terminal part",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"alpha"}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":" beta"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha beta"}]}]}}

`,
			wantTotal:  "alpha beta",
			wantValues: map[string]int{"alpha": 1, " beta": 1},
		},
		{
			name: "one streamed part two terminal parts",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"alpha beta"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"},{"type":"summary_text","text":" beta"}]}]}}

`,
			wantTotal:  "alpha beta",
			wantValues: map[string]int{"alpha beta": 1},
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(terminalStreamCreated+tc.source), nil, nil); err != nil {
					t.Fatalf("TranslateStreamBetween: %v", err)
				}
				if got := strings.Join(reasoningEmissionOrder(t, target.inbound, buf.String()), ""); got != tc.wantTotal {
					t.Fatalf("reasoning = %q, want %q: %s", got, tc.wantTotal, buf.String())
				}
				for value, want := range tc.wantValues {
					if got := reasoningDeltaCount(t, target.inbound, buf.String(), value); got != want {
						t.Fatalf("%q emitted %d times, want %d: %s", value, got, want, buf.String())
					}
				}
			})
		}
	}
}

// TestResponsesTerminalPartitionApproximation pins the divergent partition
// path: a terminal document that differs from the streamed bytes must not
// repeat the visible prefix, only a genuinely new trailing part is emitted,
// and the decoder records that the target wire is an approximation.
func TestResponsesTerminalPartitionApproximation(t *testing.T) {
	cases := []struct {
		name       string
		source     string
		wantText   string
		wantField  string
		reasoning  bool
		wantAbsent string
	}{
		{
			name: "content diverges then new tail",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"goodbye"},{"type":"output_text","text":" world"}]}]}}

`,
			wantText:   "hello world",
			wantField:  "content_partition",
			wantAbsent: "goodbye",
		},
		{
			name: "summary partition drops only whitespace",
			source: `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"alpha"}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":1,"part":{"type":"summary_text","text":"beta"}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha beta"}]}]}}

`,
			wantText:  "alphabeta",
			wantField: "summary_partition",
			reasoning: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := NewResponsesStreamDecoder(strings.NewReader(tc.source))
			for {
				if _, err := dec.Next(); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("Next: %v", err)
				}
			}
			warnings := dec.Warnings()
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want one approximation", warnings)
			}
			if warnings[0].Kind != ir.WarningApproximated || warnings[0].Field != tc.wantField {
				t.Fatalf("warning = %+v, want approximated/%s", warnings[0], tc.wantField)
			}
			for _, target := range terminalTargets {
				t.Run(target.name, func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(tc.source), nil, nil); err != nil {
						t.Fatalf("TranslateStreamBetween: %v", err)
					}
					var got string
					if tc.reasoning {
						got = strings.Join(reasoningEmissionOrder(t, target.inbound, buf.String()), "")
					} else {
						got = strings.Join(textEmissionOrder(t, target.inbound, buf.String()), "")
					}
					if got != tc.wantText {
						t.Fatalf("emitted = %q, want %q: %s", got, tc.wantText, buf.String())
					}
					if tc.wantAbsent != "" {
						for _, value := range []string{tc.wantAbsent} {
							if count := textDeltaCount(t, target.inbound, buf.String(), value); count != 0 {
								t.Fatalf("%q emitted %d times, want never: %s", value, count, buf.String())
							}
						}
					}
				})
			}
		})
	}
}

// reasoningEmissionOrder returns every reasoning delta the target emitted, in
// wire order.
func reasoningEmissionOrder(t *testing.T, protocol ir.Protocol, out string) []string {
	t.Helper()
	var order []string
	switch protocol {
	case ir.Responses:
		for _, payload := range mustResponsePayloads(t, out) {
			typ, _ := payload["type"].(string)
			if typ != "response.reasoning_summary_text.delta" && typ != "response.reasoning_text.delta" {
				continue
			}
			if text, _ := payload["delta"].(string); text != "" {
				order = append(order, text)
			}
		}
	case ir.Anthropic:
		for _, payload := range mustAnthropicPayloads(t, out) {
			delta, _ := payload["delta"].(map[string]any)
			if delta["type"] != "thinking_delta" {
				continue
			}
			if text, _ := delta["thinking"].(string); text != "" {
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
			if text, _ := delta["reasoning_content"].(string); text != "" {
				order = append(order, text)
			}
		}
	}
	return order
}
