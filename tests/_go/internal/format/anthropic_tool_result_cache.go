package format

// Round 3 R3-3 regression: a non-empty nested tool-result text block keeps its
// cache breakpoint. The plain string form cannot carry one, so a breakpoint
// forces the array form.

import (
	"testing"
)

func anthropicToolResultCacheRequest(t *testing.T, content string) []byte {
	t.Helper()
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"hi"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` + content + `}]}]}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func anthropicToolResultContent(t *testing.T, out []byte) any {
	t.Helper()
	root := decodeJSONObject(t, out)
	messages, _ := root["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %v, want three", messages)
	}
	wrappers, _ := messages[2].(map[string]any)["content"].([]any)
	if len(wrappers) != 1 {
		t.Fatalf("tool result wrapper = %v", wrappers)
	}
	return wrappers[0].(map[string]any)["content"]
}

func TestAnthropicToolResultTextCacheControlPreserved(t *testing.T) {
	out := anthropicToolResultCacheRequest(t,
		`[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}}]`)
	content := anthropicToolResultContent(t, out)
	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("tool-result content = %T (%v), want the array form", content, content)
	}
	if len(parts) != 1 {
		t.Fatalf("tool-result content = %v, want one text block", parts)
	}
	block, _ := parts[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "ok" {
		t.Fatalf("tool-result block = %v, want the text block", block)
	}
	cache, _ := block["cache_control"].(map[string]any)
	if cache["type"] != "ephemeral" {
		t.Fatalf("tool-result cache_control = %v, want ephemeral: %s", block["cache_control"], out)
	}
}

func TestAnthropicToolResultMixedTextCacheControlPreserved(t *testing.T) {
	out := anthropicToolResultCacheRequest(t,
		`[{"type":"text","text":"ok","cache_control":{"type":"ephemeral"}},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)
	content := anthropicToolResultContent(t, out)
	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("tool-result content = %T, want the array form", content)
	}
	if len(parts) != 2 {
		t.Fatalf("tool-result content = %v, want text plus image", parts)
	}
	text, _ := parts[0].(map[string]any)
	cache, _ := text["cache_control"].(map[string]any)
	if cache["type"] != "ephemeral" {
		t.Fatalf("mixed tool-result text cache_control = %v, want ephemeral: %s", text["cache_control"], out)
	}
	image, _ := parts[1].(map[string]any)
	if image["type"] != "image" {
		t.Fatalf("mixed tool-result image = %v", image)
	}
}

func TestAnthropicToolResultPlainTextStaysString(t *testing.T) {
	out := anthropicToolResultCacheRequest(t, `[{"type":"text","text":"ok"}]`)
	content := anthropicToolResultContent(t, out)
	if content != "ok" {
		t.Fatalf("tool-result content = %T (%v), want the plain string form", content, content)
	}
}

func TestAnthropicToolResultEmptyTextDropsBreakpoint(t *testing.T) {
	out := anthropicToolResultCacheRequest(t,
		`[{"type":"text","text":"","cache_control":{"type":"ephemeral"}},`+
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}]`)
	content := anthropicToolResultContent(t, out)
	parts, ok := content.([]any)
	if !ok {
		t.Fatalf("tool-result content = %T, want the image array", content)
	}
	if len(parts) != 1 {
		t.Fatalf("tool-result content = %v, want only the image", parts)
	}
	if image, _ := parts[0].(map[string]any); image["type"] != "image" {
		t.Fatalf("tool-result part = %v, want image", parts[0])
	}
}
