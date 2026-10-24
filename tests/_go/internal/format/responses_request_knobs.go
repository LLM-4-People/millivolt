package format

// Round 4 batch 6 regressions (A1, A2, A5): Responses request fields with no
// target representation warn instead of disappearing, invalid audio parts are
// not emitted, and provider-only tool knobs fail closed when dropping them
// would widen invocation.

import (
	"strings"
	"testing"
)

func TestResponsesRequestDropsAudioWithWarning(t *testing.T) {
	chat := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}},` +
		`{"type":"text","text":"listen"}]}]}`
	req, err := DecodeChatRequest([]byte(chat))
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "input_audio") {
		t.Fatalf("Responses body carries an input_audio part: %s", body)
	}
	input, _ := decodeJSONMap(t, body)["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v, want one message", input)
	}
	content, _ := input[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %v, want the text part only", content)
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "listen" {
		t.Fatalf("kept part = %v, want the text", part)
	}
	count := 0
	for _, warning := range req.Warnings {
		if warning.Field == "content.input_audio" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("content.input_audio warnings = %d, want one: %+v", count, req.Warnings)
	}
}

func TestResponsesToolKnobsWarn(t *testing.T) {
	body := `{"model":"m","input":"hi","tools":[` +
		`{"type":"function","name":"lookup","parameters":{"type":"object"},` +
		`"allowed_callers":["direct"],"async":true,"defer_loading":true,"output_schema":{"type":"object"}},` +
		`{"type":"function","name":"other","parameters":{"type":"object"},` +
		`"async":false,"defer_loading":false,"output_schema":null}]}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"tool.allowed_callers", "tool.async", "tool.defer_loading", "tool.output_schema",
	} {
		count := 0
		for _, warning := range req.Warnings {
			if warning.Field == field {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s warnings = %d, want one: %+v", field, count, req.Warnings)
		}
	}
	if len(req.Tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(req.Tools))
	}
}

func TestResponsesProgrammaticOnlyToolRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "programmatic only", raw: `["programmatic"]`},
		{name: "empty list", raw: `[]`},
		{name: "malformed", raw: `{"caller":"direct"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"m","input":"hi","tools":[` +
				`{"type":"function","name":"lookup","parameters":{"type":"object"},"allowed_callers":` + tc.raw + `}]}`
			if _, err := DecodeResponsesRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "allowed_callers") {
				t.Fatalf("decode error = %v, want an allowed_callers rejection", err)
			}
		})
	}

	t.Run("direct is accepted", func(t *testing.T) {
		body := `{"model":"m","input":"hi","tools":[` +
			`{"type":"function","name":"lookup","parameters":{"type":"object"},"allowed_callers":["direct","programmatic"]}]}`
		req, err := DecodeResponsesRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if !warningField(req.Warnings, "tool.allowed_callers") {
			t.Fatalf("direct-callable tool recorded no drop warning: %+v", req.Warnings)
		}
	})
}

func TestResponsesAccessProgramsAndObfuscationWarn(t *testing.T) {
	body := `{"model":"m","input":"hi","access_programs":{"cyber":true},` +
		`"stream_options":{"include_obfuscation":false}}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"access_programs", "stream_options.include_obfuscation"} {
		count := 0
		for _, warning := range req.Warnings {
			if warning.Field == field {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%s warnings = %d, want one: %+v", field, count, req.Warnings)
		}
	}

	control := `{"model":"m","input":"hi","access_programs":null,"stream_options":{}}`
	req, err = DecodeResponsesRequest([]byte(control))
	if err != nil {
		t.Fatal(err)
	}
	if warningField(req.Warnings, "access_programs") ||
		warningField(req.Warnings, "stream_options.include_obfuscation") {
		t.Fatalf("absent knobs warned: %+v", req.Warnings)
	}
}

func TestResponsesMalformedStreamOptionsFailsClosed(t *testing.T) {
	for _, raw := range []string{
		`5`,
		`"x"`,
		`[true]`,
		`true`,
		`{"include_obfuscation":"yes"}`,
		`{"include_obfuscation":1}`,
	} {
		body := `{"model":"m","input":"hi","stream_options":` + raw + `}`
		if _, err := DecodeResponsesRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "stream_options") {
			t.Fatalf("stream_options %s error = %v, want a stream_options rejection", raw, err)
		}
	}
}

func TestResponsesUnknownStreamOptionWarns(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","stream_options":{"include_usage":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(req.Warnings, "stream_options.include_usage") {
		t.Fatalf("unknown stream option recorded no warning: %+v", req.Warnings)
	}
	if warningField(req.Warnings, "stream_options.include_obfuscation") {
		t.Fatalf("absent include_obfuscation warned: %+v", req.Warnings)
	}

	for _, control := range []string{
		`{"model":"m","input":"hi","stream_options":{"include_usage":null}}`,
		`{"model":"m","input":"hi","stream_options":{}}`,
	} {
		req, err := DecodeResponsesRequest([]byte(control))
		if err != nil {
			t.Fatal(err)
		}
		if warningField(req.Warnings, "stream_options.include_usage") {
			t.Fatalf("null/absent member warned: %+v", req.Warnings)
		}
	}
}

func TestResponsesStreamOptionWarningsAreSorted(t *testing.T) {
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":"hi","stream_options":{"zeta":true,"alpha":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for _, warning := range req.Warnings {
		if strings.HasPrefix(warning.Field, "stream_options.") {
			fields = append(fields, warning.Field)
		}
	}
	want := []string{"stream_options.alpha", "stream_options.zeta"}
	if len(fields) != len(want) || fields[0] != want[0] || fields[1] != want[1] {
		t.Fatalf("stream_options warnings = %v, want %v", fields, want)
	}
}
