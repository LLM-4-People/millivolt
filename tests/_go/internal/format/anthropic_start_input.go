package format

// C2 regression: Anthropic's official content_block_start tool input is {}.
// A non-empty start carries a complete JSON object, so a following
// input_json_delta cannot be concatenated with it; the decoder fails closed
// instead of producing target-dependent arguments. Start-only and delta-only
// shapes keep working.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicStreamStart = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":%s}}

`

func TestAnthropicStreamNonEmptyStartPlusDeltaFailsClosed(t *testing.T) {
	source := anthropicStreamStart + `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"b\":2}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
	source = strings.Replace(source, "%s", `{"a":1}`, 1)
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
		t.Run(target.String(), func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(target, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
			if err == nil {
				t.Fatalf("non-empty start plus delta was accepted: %s", buf.String())
			}
			if strings.Contains(buf.String(), `{\"a\":1}{\"b\":2}`) {
				t.Fatalf("start and delta were concatenated: %s", buf.String())
			}
		})
	}
}

func TestAnthropicStreamToolInputShapes(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		deltas []string
		want   string
	}{
		{
			name:  "start only",
			input: `{"a":1}`,
			want:  `{"a":1}`,
		},
		{
			name:   "delta only from empty start",
			input:  `{}`,
			deltas: []string{`{"b":2}`},
			want:   `{"b":2}`,
		},
		{
			name:   "delta fragments from empty start",
			input:  `{}`,
			deltas: []string{`{"b":`, `2}`},
			want:   `{"b":2}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := strings.Replace(anthropicStreamStart, "%s", tc.input, 1)
			for _, delta := range tc.deltas {
				source += `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"` + strings.ReplaceAll(delta, `"`, `\"`) + `"}}

`
			}
			source += `event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}

`
			for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic, ir.Responses} {
				t.Run(target.String(), func(t *testing.T) {
					var buf bytes.Buffer
					if err := TranslateStreamBetween(target, ir.Anthropic, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
						t.Fatalf("TranslateStreamBetween: %v", err)
					}
					calls := streamedToolCalls(t, target, buf.String())
					if len(calls) != 1 || calls[0].name != "lookup" || calls[0].args.String() != tc.want {
						t.Fatalf("calls = %+v, want lookup/%s: %s", calls, tc.want, buf.String())
					}
				})
			}
		})
	}
}
