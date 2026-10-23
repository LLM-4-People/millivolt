package format

// Batch 10 regressions: every content part a translator emits is valid for
// the target role and wire, target-specific file allowlists drop what the
// target cannot read with a warning, nested Anthropic document content keeps
// its images, and unrepresentable detail levels are reported.

import (
	"encoding/base64"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestChatToolResultContentIsTextOnly(t *testing.T) {
	req := &ir.Request{
		Model: "m",
		Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				ir.TextBlock("before"),
				{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
					CallID: "call_1",
					Content: []ir.Block{
						ir.TextBlock("sunny"),
						{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
						{Type: ir.BlockFile, File: &ir.File{FileData: "JVBER", MediaType: "application/pdf"}},
						{Type: ir.BlockAudio, Audio: &ir.Audio{Data: "AAAA", Format: "wav"}},
					},
				}},
				ir.TextBlock("after"),
			},
		}},
	}
	body, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeJSONMap(t, body)["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v, want a tool message and the remaining user message", messages)
	}
	tool := messages[0].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("tool message = %v", tool)
	}
	if content, ok := tool["content"].(string); !ok || content != "sunny" {
		t.Fatalf("tool content = %#v, want the text only", tool["content"])
	}
	user := messages[1].(map[string]any)
	if content, ok := user["content"].(string); !ok || content != "beforeafter" {
		t.Fatalf("user content = %#v", user["content"])
	}
	for _, field := range []string{"tool_result.image_url", "tool_result.file", "tool_result.audio"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("missing %s drop warning: %+v", field, req.Warnings)
		}
	}
}

func TestChatSystemAndAssistantPartsAreTextOnly(t *testing.T) {
	for _, role := range []ir.Role{ir.RoleSystem, ir.RoleDeveloper, ir.RoleAssistant} {
		t.Run(string(role), func(t *testing.T) {
			blocks := []ir.Block{
				ir.TextBlock("keep"),
				{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
				{Type: ir.BlockFile, File: &ir.File{FileData: "JVBER", MediaType: "application/pdf"}},
				{Type: ir.BlockAudio, Audio: &ir.Audio{Data: "AAAA", Format: "wav"}},
			}
			if role == ir.RoleAssistant {
				blocks = append(blocks, ir.Block{Type: ir.BlockRefusal, Refusal: "no"})
			}
			req := &ir.Request{Model: "m", Messages: []ir.Message{{Role: role, Content: blocks}}}
			body, err := EncodeChatRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			message := decodeJSONMap(t, body)["messages"].([]any)[0].(map[string]any)
			if content, ok := message["content"].(string); !ok || content != "keep" {
				t.Fatalf("%s content = %#v, want the text string", role, message["content"])
			}
			for _, field := range []string{"content.image_url", "content.file", "content.input_audio"} {
				if !warningField(req.Warnings, field) {
					t.Fatalf("%s missing %s drop warning: %+v", role, field, req.Warnings)
				}
			}
			if role == ir.RoleAssistant {
				if message["refusal"] != "no" {
					t.Fatalf("assistant refusal = %v", message["refusal"])
				}
			} else if _, present := message["refusal"]; present {
				t.Fatalf("%s message carries a refusal field: %v", role, message)
			}
		})
	}

	// The response encoder shares the role rules: an assistant response cannot
	// carry an image part on the Chat wire.
	resp := &ir.Response{
		ID:    "r1",
		Model: "m",
		Blocks: []ir.Block{
			ir.TextBlock("ok"),
			{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
		},
	}
	body, err := EncodeChatResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	message := decodeJSONMap(t, body)["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if content, ok := message["content"].(string); !ok || content != "ok" {
		t.Fatalf("response content = %#v, want the text string", message["content"])
	}
	if !warningField(resp.Warnings, "content.image_url") {
		t.Fatalf("missing response image drop warning: %+v", resp.Warnings)
	}
}

func TestAnthropicSystemDropsWarn(t *testing.T) {
	req := &ir.Request{Model: "m", Messages: []ir.Message{{
		Role: ir.RoleSystem,
		Content: []ir.Block{
			{Type: ir.BlockText, Text: "rules", Citations: []ir.Citation{{Type: "char_location", CitedText: "x"}}},
			{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png"}},
		},
	}}}
	body, err := EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONMap(t, body)["system"]; got != "rules" {
		t.Fatalf("system = %v, want the text only", got)
	}
	for _, field := range []string{"system.citations", "system.image"} {
		if !warningField(req.Warnings, field) {
			t.Fatalf("missing %s drop warning: %+v", field, req.Warnings)
		}
	}
}

func TestAnthropicDocumentContentKeepsImages(t *testing.T) {
	const image = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}}`
	for name, content := range map[string]string{
		"text and image": `[{"type":"text","text":"caption"},` + image + `]`,
		"image only":     `[` + image + `]`,
	} {
		t.Run(name, func(t *testing.T) {
			body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
				`{"type":"document","title":"scan","source":{"type":"content","content":` + content + `}}]}]}`
			req, err := DecodeAnthropicRequest([]byte(body))
			if err != nil {
				t.Fatalf("image-only document content was rejected: %v", err)
			}
			if !warningField(req.Warnings, "document.content") {
				t.Fatalf("missing document.content warning: %+v", req.Warnings)
			}
			var images, files int
			for _, block := range req.Messages[0].Content {
				switch block.Type {
				case ir.BlockImage:
					images++
					if block.Image == nil || block.Image.Data != "aGk=" || block.Image.MediaType != "image/png" {
						t.Fatalf("decoded image = %+v", block.Image)
					}
				case ir.BlockFile:
					files++
				}
			}
			if images != 1 {
				t.Fatalf("decoded images = %d, want 1: %+v", images, req.Messages[0].Content)
			}
			if files != 0 {
				t.Fatalf("content document became a file block: %+v", req.Messages[0].Content)
			}

			// The image survives the translation to a Chat user message.
			out, err := EncodeChatRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			parts := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
			var imageParts int
			for _, raw := range parts {
				if raw.(map[string]any)["type"] == "image_url" {
					imageParts++
				}
			}
			if imageParts != 1 {
				t.Fatalf("Chat image parts = %d, want 1: %v", imageParts, parts)
			}
		})
	}
}

func TestNonPDFFileTargetsWarnAndDrop(t *testing.T) {
	csvData := base64.StdEncoding.EncodeToString([]byte("a,b"))

	t.Run("anthropic coerces other text media types", func(t *testing.T) {
		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
			`{"type":"document","source":{"type":"base64","media_type":"text/csv","data":"` + csvData + `"}}]}]}`
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		out, err := EncodeAnthropicRequest(req, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		source := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
		if source["type"] != "text" || source["media_type"] != "text/plain" || source["data"] != "a,b" {
			t.Fatalf("Anthropic source = %v, want a plain-text document", source)
		}
		if !warningField(req.Warnings, "file.media_type") {
			t.Fatalf("missing text media coercion warning: %+v", req.Warnings)
		}

		// The same source translated to Chat cannot be a text/csv file part.
		chatReq, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		chatBody, err := EncodeChatRequest(chatReq)
		if err != nil {
			t.Fatal(err)
		}
		message := decodeJSONMap(t, chatBody)["messages"].([]any)[0].(map[string]any)
		if content, ok := message["content"].(string); !ok || content != "" {
			t.Fatalf("Chat content = %#v, want the non-PDF file dropped", message["content"])
		}
		if !warningField(chatReq.Warnings, "file.media_type") {
			t.Fatalf("missing Chat non-PDF warning: %+v", chatReq.Warnings)
		}
	})

	t.Run("anthropic drops unreadable documents", func(t *testing.T) {
		docx := base64.StdEncoding.EncodeToString([]byte("PK\x03\x04"))
		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
			`{"type":"text","text":"look"},` +
			`{"type":"document","source":{"type":"base64","media_type":"application/vnd.openxmlformats-officedocument.wordprocessingml.document","data":"` + docx + `"}}]}]}`
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		out, err := EncodeAnthropicRequest(req, 0, "")
		if err != nil {
			t.Fatal(err)
		}
		blocks := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
		if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "text" {
			t.Fatalf("Anthropic blocks = %v, want the unreadable document dropped", blocks)
		}
		if !warningField(req.Warnings, "file.media_type") {
			t.Fatalf("missing non-document drop warning: %+v", req.Warnings)
		}
	})

	t.Run("chat keeps PDF data and file ids only", func(t *testing.T) {
		csvReq := &ir.Request{Model: "m", Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				ir.TextBlock("look"),
				{Type: ir.BlockFile, File: &ir.File{FileData: csvData, MediaType: "text/csv", Filename: "a.csv"}},
			},
		}}}
		body, err := EncodeChatRequest(csvReq)
		if err != nil {
			t.Fatal(err)
		}
		message := decodeJSONMap(t, body)["messages"].([]any)[0].(map[string]any)
		if content, ok := message["content"].(string); !ok || content != "look" {
			t.Fatalf("Chat content = %#v, want the CSV file dropped", message["content"])
		}
		if !warningField(csvReq.Warnings, "file.media_type") {
			t.Fatalf("missing Chat CSV warning: %+v", csvReq.Warnings)
		}

		pdfReq := &ir.Request{Model: "m", Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{{
				Type: ir.BlockFile,
				File: &ir.File{FileData: "JVBER", MediaType: "application/pdf", Filename: "a.pdf"},
			}},
		}}}
		body, err = EncodeChatRequest(pdfReq)
		if err != nil {
			t.Fatal(err)
		}
		part := decodeJSONMap(t, body)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		file := part["file"].(map[string]any)
		if file["file_data"] != "data:application/pdf;base64,JVBER" {
			t.Fatalf("Chat PDF part = %v", file)
		}

		idReq := &ir.Request{Model: "m", Messages: []ir.Message{{
			Role:    ir.RoleUser,
			Content: []ir.Block{{Type: ir.BlockFile, File: &ir.File{FileID: "file_1"}}},
		}}}
		body, err = EncodeChatRequest(idReq)
		if err != nil {
			t.Fatal(err)
		}
		part = decodeJSONMap(t, body)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		if part["file"].(map[string]any)["file_id"] != "file_1" {
			t.Fatalf("Chat file id part = %v", part)
		}
	})

	t.Run("responses keeps declared media types", func(t *testing.T) {
		req := &ir.Request{Model: "m", Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{{
				Type: ir.BlockFile,
				File: &ir.File{FileData: csvData, MediaType: "text/csv", Filename: "a.csv"},
			}},
		}}}
		body, err := EncodeResponsesRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		part := decodeJSONMap(t, body)["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
		if part["type"] != "input_file" || part["file_data"] != "data:text/csv;base64,"+csvData {
			t.Fatalf("Responses file part = %v", part)
		}
	})
}

func TestChatRawBase64FileDefaultsToPDF(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"file","file":{"filename":"a.pdf","file_data":"JVBERi0="}}]}]}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatalf("a raw-base64 Chat PDF was rejected: %v", err)
	}
	file := req.Messages[0].Content[0].File
	if file == nil || file.FileData != "JVBERi0=" || file.MediaType != "application/pdf" {
		t.Fatalf("decoded file = %+v, want application/pdf", file)
	}
	out, err := EncodeAnthropicRequest(req, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	source := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if source["type"] != "base64" || source["media_type"] != "application/pdf" || source["data"] != "JVBERi0=" {
		t.Fatalf("Anthropic source = %v", source)
	}
}

func TestUnrepresentableDetailWarns(t *testing.T) {
	newRequest := func() *ir.Request {
		return &ir.Request{Model: "m", Messages: []ir.Message{{
			Role: ir.RoleUser,
			Content: []ir.Block{
				{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/a.png", Detail: "high"}},
				{Type: ir.BlockFile, File: &ir.File{FileData: "JVBER", MediaType: "application/pdf", Detail: "low"}},
			},
		}}}
	}

	anthropicReq := newRequest()
	out, err := EncodeAnthropicRequest(anthropicReq, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"image.detail", "file.detail"} {
		if !warningField(anthropicReq.Warnings, field) {
			t.Fatalf("missing %s warning: %+v", field, anthropicReq.Warnings)
		}
	}
	content := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	for _, raw := range content {
		block := raw.(map[string]any)
		if _, present := block["detail"]; present {
			t.Fatalf("Anthropic block carries detail: %v", block)
		}
	}

	chatReq := newRequest()
	out, err = EncodeChatRequest(chatReq)
	if err != nil {
		t.Fatal(err)
	}
	chatContent := decodeJSONMap(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	imagePart := chatContent[0].(map[string]any)
	if imagePart["image_url"].(map[string]any)["detail"] != "high" {
		t.Fatalf("Chat image detail = %v", imagePart)
	}
	if !warningField(chatReq.Warnings, "file.detail") {
		t.Fatalf("missing Chat file.detail warning: %+v", chatReq.Warnings)
	}

	responsesReq := newRequest()
	out, err = EncodeResponsesRequest(responsesReq)
	if err != nil {
		t.Fatal(err)
	}
	responsesContent := decodeJSONMap(t, out)["input"].([]any)[0].(map[string]any)["content"].([]any)
	if responsesContent[0].(map[string]any)["detail"] != "high" {
		t.Fatalf("Responses image detail = %v", responsesContent[0])
	}
	if responsesContent[1].(map[string]any)["detail"] != "low" {
		t.Fatalf("Responses file detail = %v", responsesContent[1])
	}
	if warningField(responsesReq.Warnings, "file.detail") || warningField(responsesReq.Warnings, "image.detail") {
		t.Fatalf("Responses reported a detail loss it can represent: %+v", responsesReq.Warnings)
	}
}
