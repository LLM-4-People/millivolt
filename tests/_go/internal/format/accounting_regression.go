package format

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNativeToolChoiceMapping(t *testing.T) {
	for _, tt := range []struct{ in, want string }{{"required", "any"}, {"none", "none"}, {"auto", "auto"}} {
		body := `{"model":"model-a","messages":[],"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}}],"tool_choice":"` + tt.in + `"}`
		out, err := TranslateRequest([]byte(body), 100)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			ToolChoice struct {
				Type string `json:"type"`
			} `json:"tool_choice"`
		}
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatal(err)
		}
		if got.ToolChoice.Type != tt.want {
			t.Fatalf("%s mapped to %q, want %q", tt.in, got.ToolChoice.Type, tt.want)
		}
	}
}

func TestNativeUsageIncludesAllInputClasses(t *testing.T) {
	out, err := TranslateResponse([]byte(`{"id":"req-a","model":"model-a","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":20,"cache_creation_input_tokens":30}}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	u := got.Usage
	d := u["prompt_tokens_details"].(map[string]any)
	if u["prompt_tokens"] != float64(60) || u["completion_tokens"] != float64(5) || u["total_tokens"] != float64(65) || d["cached_tokens"] != float64(20) || d["cache_write_tokens"] != float64(30) {
		t.Fatalf("usage=%v", u)
	}
}

func TestNativeUsageRejectsImpossibleTotals(t *testing.T) {
	for _, usage := range []string{`{"input_tokens":-1}`, `{"input_tokens":9223372036854775807,"output_tokens":1}`, `{"input_tokens":1,"cache_read_input_tokens":9223372036854775807}`} {
		if _, err := TranslateResponse([]byte(`{"content":[{"type":"text","text":"hello"}],"usage":` + usage + `}`)); err == nil {
			t.Fatalf("accepted impossible usage %s", usage)
		}
	}
}

func TestNativeToolDeltaIdentityAccumulatesOnce(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"req-a","model":"model-a"}}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call-a","name":"lookup"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"}"}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"content_block_start","index":4,"content_block":{"type":"tool_use","id":"call-b","name":"other"}}`,
		`data: {"type":"content_block_stop","index":4}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatal(err)
	}
	type call struct{ ID, Name, Arguments string }
	calls := map[int]call{}
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatal(err)
		}
		for _, choice := range chunk.Choices {
			for _, tc := range choice.Delta.ToolCalls {
				c := calls[tc.Index]
				c.ID += tc.ID
				c.Name += tc.Function.Name
				c.Arguments += tc.Function.Arguments
				calls[tc.Index] = c
			}
		}
	}
	if len(calls) != 2 || calls[0] != (call{"call-a", "lookup", "{}"}) || calls[1] != (call{"call-b", "other", "{}"}) {
		t.Fatalf("accumulated calls=%+v", calls)
	}
}

func TestNativeStreamRejectsImpossibleUsageBeforeTerminal(t *testing.T) {
	for _, usage := range []string{`{"input_tokens":-1}`, `{"input_tokens":9223372036854775808}`, `{"input_tokens":9223372036854775807}`} {
		stream := "data:" + `{"type":"message_start","message":{"id":"req-a","model":"model-a","usage":` + usage + `}}` + "\n\n" +
			"data:" + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n"
		var out strings.Builder
		if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err == nil {
			t.Fatalf("accepted impossible stream usage %s", usage)
		}
		if strings.Contains(out.String(), `"finish_reason":"stop"`) || strings.Contains(out.String(), "[DONE]") {
			t.Fatalf("committed success for impossible usage: %s", out.String())
		}
	}
}
