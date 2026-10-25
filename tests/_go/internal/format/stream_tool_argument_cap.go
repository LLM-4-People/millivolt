package format

// Round 7 H5 regression: both streaming decoders bound the tool-argument bytes
// they accumulate for validation with streamToolArgumentsMax. No test drove
// either cap, so deleting or mis-sizing it left the suite green. Every fragment
// is 900 KiB, below the 1 MiB SSE data-line cap.
//
// The pair of cases bounds the cap without restating its value: a legitimate
// 900 KiB argument must translate cleanly, and five fragments (4.5 MiB) must
// fail with the cap owner's own error instead of reaching a terminal.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const streamCapFragmentBytes = 900 << 10

func streamCapJSON(t *testing.T, value string) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func chatToolArgumentFragmentsStream(t *testing.T, fragments []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n")
	for i, fragment := range fragments {
		if i == 0 {
			fmt.Fprintf(&b, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":%s}}]},"finish_reason":null}]}`+"\n\n", streamCapJSON(t, fragment))
			continue
		}
		fmt.Fprintf(&b, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}]}`+"\n\n", streamCapJSON(t, fragment))
	}
	b.WriteString(`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func anthropicToolArgumentFragmentsStream(t *testing.T, fragments []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}

`)
	for _, fragment := range fragments {
		fmt.Fprintf(&b, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%s}}

`, streamCapJSON(t, fragment))
	}
	b.WriteString(`event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`)
	return b.String()
}

func TestStreamToolArgumentCapFailsClosed(t *testing.T) {
	overCap := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		overCap = append(overCap, strings.Repeat("a", streamCapFragmentBytes))
	}
	// Five 900 KiB fragments are 4.5 MiB, above the current 4 MiB cap. The
	// fixture is fixed rather than derived so a cap raised past 4.5 MiB is
	// caught by the distinct JSON-parse failure instead of a clean terminal.
	for _, tc := range []struct {
		name     string
		source   ir.Protocol
		target   ir.Protocol
		stream   string
		wantErr  string
		terminal string
	}{
		{
			name:     "chat source",
			source:   ir.Chat,
			target:   ir.Chat,
			stream:   chatToolArgumentFragmentsStream(t, overCap),
			wantErr:  fmt.Sprintf("chat stream exceeds %d accumulated tool-argument bytes", streamToolArgumentsMax),
			terminal: "data: [DONE]",
		},
		{
			name:     "anthropic source",
			source:   ir.Anthropic,
			target:   ir.Anthropic,
			stream:   anthropicToolArgumentFragmentsStream(t, overCap),
			wantErr:  fmt.Sprintf("anthropic stream exceeds %d accumulated tool-argument bytes", streamToolArgumentsMax),
			terminal: `"type":"message_stop"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(tc.target, tc.source, &buf, strings.NewReader(tc.stream), nil, nil, StreamOptions{})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
			if strings.Contains(buf.String(), tc.terminal) {
				t.Fatalf("over-cap stream reached a clean terminal: %s", buf.String())
			}
		})
	}
}

func TestStreamToolArgumentUnderCapTranslates(t *testing.T) {
	value := largeToolArguments("q", streamCapFragmentBytes)
	for _, tc := range []struct {
		name     string
		source   ir.Protocol
		target   ir.Protocol
		stream   string
		terminal string
	}{
		{
			name:     "chat source",
			source:   ir.Chat,
			target:   ir.Chat,
			stream:   chatToolArgumentFragmentsStream(t, []string{value}),
			terminal: "data: [DONE]",
		},
		{
			name:     "anthropic source",
			source:   ir.Anthropic,
			target:   ir.Anthropic,
			stream:   anthropicToolArgumentFragmentsStream(t, []string{value}),
			terminal: `"type":"message_stop"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(tc.target, tc.source, &buf, strings.NewReader(tc.stream), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("a %d-byte argument was rejected: %v", len(value), err)
			}
			if !strings.Contains(buf.String(), tc.terminal) {
				t.Fatalf("under-cap stream lost its terminal: %s", buf.String())
			}
		})
	}
}
