package format

// Round 3 batch 14 regressions (A2, A3, A5, A6, A7, A8): the request codecs
// report every semantic they cannot carry and keep the target wire valid.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesAssistantMediaWarns(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleAssistant,
			Content: []ir.Block{
				ir.TextBlock("visible"),
				{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
				{Type: ir.BlockFile, File: &ir.File{FileID: "file_1"}},
				{Type: ir.BlockAudio, Audio: &ir.Audio{Data: "AAAA", Format: "wav"}},
			},
		}, {
			Role:    ir.RoleUser,
			Content: []ir.Block{ir.TextBlock("hi")},
		}},
	}
	body, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"assistant.image", "assistant.file", "assistant.audio"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("missing %s drop warning: %+v", field, req.Warnings)
		}
	}
	doc := decodeJSONMap(t, body)
	input, _ := doc["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input = %v, want the assistant text message and the user message", input)
	}
	content, _ := input[0].(map[string]any)["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "visible" {
		t.Fatalf("assistant content = %v, want only the text part", content)
	}
	for _, part := range content {
		if typ, _ := part.(map[string]any)["type"].(string); typ != "output_text" {
			t.Fatalf("assistant media leaked as %q", typ)
		}
	}
}

func TestResponsesMaxOutputTokensClamped(t *testing.T) {
	for _, tc := range []struct {
		in     int
		want   int
		warned bool
	}{
		{0, 16, true},
		{1, 16, true},
		{15, 16, true},
		{16, 16, false},
		{17, 17, false},
	} {
		limit := tc.in
		req := &ir.Request{
			Model:           "m",
			MaxOutputTokens: &limit,
			Messages:        []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
		}
		body, err := EncodeResponsesRequest(req)
		if err != nil {
			t.Fatalf("%d: %v", tc.in, err)
		}
		got, _ := decodeJSONMap(t, body)["max_output_tokens"].(float64)
		if int(got) != tc.want {
			t.Fatalf("max_output_tokens %d -> %d, want %d", tc.in, int(got), tc.want)
		}
		if warned := warningField(req.Warnings, "max_output_tokens"); warned != tc.warned {
			t.Fatalf("max_output_tokens %d warned = %v, want %v (%+v)", tc.in, warned, tc.warned, req.Warnings)
		}
	}
}

func TestAnthropicResponseFormatLosesDescriptionAndStrictWarns(t *testing.T) {
	strictTrue := true
	strictFalse := false
	for _, tc := range []struct {
		name       string
		format     *ir.ResponseFormat
		wantDesc   bool
		wantStrict bool
	}{
		{
			name: "description only",
			format: &ir.ResponseFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`),
				Description: "use this shape"},
			wantDesc: true,
		},
		{
			name: "strict true",
			format: &ir.ResponseFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`),
				Strict: &strictTrue},
			wantStrict: true,
		},
		{
			name: "strict false is the target default",
			format: &ir.ResponseFormat{Type: "json_schema", Schema: json.RawMessage(`{"type":"object"}`),
				Strict: &strictFalse},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model:           "m",
				MaxOutputTokens: intPtr(64),
				ResponseFormat:  tc.format,
				Messages:        []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
			}
			if _, err := EncodeAnthropicRequest(req, 4096, ""); err != nil {
				t.Fatal(err)
			}
			if got := warningField(req.Warnings, "response_format.description"); got != tc.wantDesc {
				t.Fatalf("description warning = %v, want %v (%+v)", got, tc.wantDesc, req.Warnings)
			}
			if got := warningField(req.Warnings, "response_format.strict"); got != tc.wantStrict {
				t.Fatalf("strict warning = %v, want %v (%+v)", got, tc.wantStrict, req.Warnings)
			}
		})
	}
}

func TestChatRequestAnnotationsPreserved(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"answer","annotations":[{"type":"url_citation",` +
		`"url_citation":{"url":"https://example.com/a","title":"A","start_index":0,"end_index":6}}]}]}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(req.Messages))
	}
	citations := 0
	for _, block := range req.Messages[1].Content {
		citations += len(block.Citations)
	}
	if citations != 1 {
		t.Fatalf("assistant annotations became %d citations, want 1", citations)
	}

	// The IR now owns the replayed citation, so every target is explicit: a
	// request message on these wires has no annotations field, and the
	// encoder reports the drop instead of silently ignoring the extension.
	for _, target := range []string{"responses", "anthropic", "chat"} {
		req.Warnings = nil
		switch target {
		case "responses":
			if _, err := EncodeResponsesRequest(req); err != nil {
				t.Fatal(err)
			}
		case "anthropic":
			if _, err := EncodeAnthropicRequest(req, 4096, ""); err != nil {
				t.Fatal(err)
			}
		case "chat":
			if _, err := EncodeChatRequest(req); err != nil {
				t.Fatal(err)
			}
		}
		if !warningField(req.Warnings, "citations") {
			t.Fatalf("%s target lost the replayed citation without a warning: %+v", target, req.Warnings)
		}
	}
}

func TestAnthropicStatefulFieldsWarn(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"container":"c1","diagnostics":{"previous_message_id":"msg_0"},"inference_geo":"us"}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"container", "diagnostics", "inference_geo"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("missing %s drop warning: %+v", field, req.Warnings)
		}
	}
}

func TestChatMessageAudioWarns(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","content":"hi","audio":{"id":"audio_1"}}]}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(req.Warnings, "messages.audio") {
		t.Fatalf("message audio drop warning missing: %+v", req.Warnings)
	}
}

func TestPartCacheBreakpointWarns(t *testing.T) {
	chatBody := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hi","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`
	chatReq, err := DecodeChatRequest([]byte(chatBody))
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(chatReq.Warnings, "prompt_cache_breakpoint") {
		t.Fatalf("Chat source breakpoint drop warning missing: %+v", chatReq.Warnings)
	}

	responsesBody := `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"hi","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`
	responsesReq, err := DecodeResponsesRequest([]byte(responsesBody))
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(responsesReq.Warnings, "prompt_cache_breakpoint") {
		t.Fatalf("Responses source breakpoint drop warning missing: %+v", responsesReq.Warnings)
	}

	// The target wording must describe the unmapped field, not claim the
	// target has no cache breakpoints at all.
	for _, target := range []string{"chat", "responses"} {
		req := &ir.Request{
			Model: "m",
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{{
				Type: ir.BlockText, Text: "hi",
				CacheControl: &ir.CacheControl{Type: "ephemeral"},
			}}}},
		}
		switch target {
		case "chat":
			if _, err := EncodeChatRequest(req); err != nil {
				t.Fatal(err)
			}
		case "responses":
			if _, err := EncodeResponsesRequest(req); err != nil {
				t.Fatal(err)
			}
		}
		found := false
		for _, warning := range req.Warnings {
			if warning.Field == "cache_control" && warning.Detail == "per-part prompt_cache_breakpoint is not mapped" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s target warning does not name the unmapped field: %+v", target, req.Warnings)
		}
	}
}

func intPtr(v int) *int { return &v }
