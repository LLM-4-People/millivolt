package format

// Round 6 C2 regression: Chat and Anthropic stream decoders forward tool
// argument fragments verbatim, so the completed value must still be a JSON
// object. A null, array, fragment, or invalid value fails the translation
// before the target receives a clean terminal. Empty and object values pass.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatToolArgumentStream(arguments string) string {
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":` + quoteJSON(arguments) + `}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
}

func quoteJSON(value string) string {
	var b bytes.Buffer
	_ = b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	_ = b.WriteByte('"')
	return b.String()
}

func anthropicToolInputStream(startInput, fragment string) string {
	start := `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}`
	if startInput != "" {
		start = `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":` + startInput + `}}`
	}
	stream := "data: " + `{"type":"message_start","message":{"id":"msg_1","model":"m"}}` + "\n\n" +
		"data: " + start + "\n\n"
	if fragment != "" {
		stream += "data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + quoteJSON(fragment) + `}}` + "\n\n"
	}
	return stream + "data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"data: " + `{"type":"message_stop"}` + "\n\n"
}

func TestStreamedToolArgumentsRequireJSONObject(t *testing.T) {
	for _, tc := range []struct {
		name      string
		arguments string
		wantErr   bool
		wantText  string
	}{
		{name: "object", arguments: `{"q":1}`, wantText: `"arguments":"{\"q\":1}"`},
		{name: "empty"},
		{name: "null string", arguments: "null", wantErr: true},
		{name: "array", arguments: "[]", wantErr: true},
		{name: "fragment", arguments: `{"q":`, wantErr: true},
	} {
		t.Run("chat "+tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader(chatToolArgumentStream(tc.arguments)), nil, nil, StreamOptions{})
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "tool call arguments") {
					t.Fatalf("error = %v, want a JSON-object rejection", err)
				}
				if strings.Contains(buf.String(), "[DONE]") {
					t.Fatalf("clean terminal followed invalid arguments: %s", buf.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tc.wantText) {
				t.Fatalf("missing %q in %s", tc.wantText, buf.String())
			}
		})
	}

	for _, tc := range []struct {
		name     string
		start    string
		fragment string
		wantErr  bool
		wantText string
	}{
		{name: "object start", start: `{"q":1}`, wantText: `"arguments":"{\"q\":1}"`},
		{name: "object fragment", fragment: `{"q":1}`, wantText: `"arguments":"{\"q\":1}"`},
		{name: "empty", wantText: `"arguments":""`},
		{name: "null start", start: `null`, wantErr: true},
		{name: "array start", start: `[]`, wantErr: true},
		{name: "array fragment", fragment: `[]`, wantErr: true},
		{name: "fragment", fragment: `{"q":`, wantErr: true},
	} {
		t.Run("anthropic "+tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := StreamToOpenAI(&buf, strings.NewReader(anthropicToolInputStream(tc.start, tc.fragment)), nil, nil)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "tool call arguments") {
					t.Fatalf("error = %v, want a JSON-object rejection", err)
				}
				if strings.Contains(buf.String(), "[DONE]") {
					t.Fatalf("clean terminal followed invalid arguments: %s", buf.String())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tc.wantText) {
				t.Fatalf("missing %q in %s", tc.wantText, buf.String())
			}
		})
	}
}

// anthropicToolInputOmittedStopStream omits content_block_stop, which the
// Anthropic decoder explicitly supports. The clean terminal closes open blocks
// through message_delta, message_stop, or EOF after a stop reason.
func anthropicToolInputOmittedStopStream(fragment, closing string) string {
	stream := "data: " + `{"type":"message_start","message":{"id":"msg_1","model":"m"}}` + "\n\n" +
		"data: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup"}}` + "\n\n"
	if fragment != "" {
		stream += "data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + quoteJSON(fragment) + `}}` + "\n\n"
	}
	switch closing {
	case "message-delta":
		stream += "data: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}` + "\n\n"
		stream += "data: " + `{"type":"message_stop"}` + "\n\n"
	case "message-stop":
		stream += "data: " + `{"type":"message_stop"}` + "\n\n"
	case "eof-after-stop":
		stream += "data: " + `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":1}}` + "\n\n"
	}
	return stream
}

func TestStreamedToolArgumentsValidatedWhenAnthropicStopIsOmitted(t *testing.T) {
	for _, target := range []ir.Protocol{ir.Chat, ir.Anthropic} {
		for _, closing := range []string{"message-delta", "message-stop", "eof-after-stop"} {
			for _, tc := range []struct {
				name     string
				fragment string
				wantErr  bool
			}{
				{name: "null", fragment: "null", wantErr: true},
				{name: "array", fragment: "[]", wantErr: true},
				{name: "fragment", fragment: `{"q":`, wantErr: true},
				{name: "object", fragment: `{"q":1}`},
			} {
				t.Run(string(target)+"/"+closing+"/"+tc.name, func(t *testing.T) {
					var buf bytes.Buffer
					err := TranslateStreamBetween(target, ir.Anthropic, &buf, strings.NewReader(anthropicToolInputOmittedStopStream(tc.fragment, closing)), nil, nil, StreamOptions{})
					terminal := "[DONE]"
					if target == ir.Anthropic {
						terminal = "event: message_stop"
					}
					if tc.wantErr {
						if err == nil || !strings.Contains(err.Error(), "tool call arguments") {
							t.Fatalf("error = %v, want a JSON-object rejection", err)
						}
						if strings.Contains(buf.String(), terminal) {
							t.Fatalf("clean terminal followed invalid arguments: %s", buf.String())
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(buf.String(), terminal) {
						t.Fatalf("valid arguments produced no clean terminal: %s", buf.String())
					}
					wantArgs := `"arguments":"{\"q\":1}"`
					if target == ir.Anthropic {
						wantArgs = `"partial_json":"{\"q\":1}"`
					}
					if !strings.Contains(buf.String(), wantArgs) {
						t.Fatalf("arguments missing from clean output: %s", buf.String())
					}
				})
			}
		}
	}
}
