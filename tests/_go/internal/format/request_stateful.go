package format

// Round 2 TC1-4b regressions: stateful and safety request fields are either
// rejected, carried between Chat and Responses, or reported as dropped.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesRejectsContextManagement(t *testing.T) {
	body := `{"model":"m","input":"hi","context_management":[{"type":"compaction","compact_threshold":1000}]}`
	if _, err := DecodeResponsesRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "context_management") {
		t.Fatalf("context_management error = %v", err)
	}
	null := `{"model":"m","input":"hi","context_management":null}`
	if _, err := DecodeResponsesRequest([]byte(null)); err != nil {
		t.Fatalf("null context_management rejected: %v", err)
	}
}

func TestModerationCrossWire(t *testing.T) {
	const moderation = `{"model":"omni-moderation-latest","policy":{"input":{"mode":"block"}}}`
	chatBody := `{"model":"m","messages":[{"role":"user","content":"hi"}],"moderation":` + moderation + `}`
	out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Responses, []byte(chatBody), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONObject(t, out)
	got, _ := root["moderation"].(map[string]any)
	if got["model"] != "omni-moderation-latest" {
		t.Fatalf("responses moderation = %v: %s", root["moderation"], out)
	}
	if warningField(warnings, "moderation") {
		t.Fatalf("chat to responses reported moderation dropped: %+v", warnings)
	}

	responsesBody := `{"model":"m","input":"hi","moderation":` + moderation + `}`
	out, _, err = TranslateRequestBetween(ir.Responses, ir.Chat, []byte(responsesBody), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root = decodeJSONObject(t, out)
	got, _ = root["moderation"].(map[string]any)
	if got["model"] != "omni-moderation-latest" {
		t.Fatalf("chat moderation = %v: %s", root["moderation"], out)
	}

	out, warnings, err = TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(chatBody), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(warnings, "moderation") {
		t.Fatalf("anthropic did not report moderation dropped: %+v", warnings)
	}
	if strings.Contains(string(out), `"moderation"`) {
		t.Fatalf("anthropic body carried moderation: %s", out)
	}
}

func TestModerationRequiresModel(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"moderation":{"policy":{}}}`
	if _, err := DecodeChatRequest([]byte(body)); err == nil || !strings.Contains(err.Error(), "moderation.model") {
		t.Fatalf("missing moderation model error = %v", err)
	}
}

func TestResponsesInputPhaseWarns(t *testing.T) {
	body := `{"model":"m","input":[{"type":"message","role":"user","phase":"commentary","content":"hi"}]}`
	output, warnings, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(warnings, "input.phase") {
		t.Fatalf("phase recorded no warning: %+v", warnings)
	}
	if strings.Contains(string(output), "commentary") {
		t.Fatalf("phase leaked into the translated body: %s", output)
	}

	clean := `{"model":"m","input":[{"type":"message","role":"user","content":"hi"}]}`
	_, warnings, err = TranslateRequestBetween(ir.Responses, ir.Chat, []byte(clean), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if warningField(warnings, "input.phase") {
		t.Fatalf("phase warning without a phase: %+v", warnings)
	}
}

func TestChatIncludeObfuscationWarns(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"stream_options":{"include_usage":true,"include_obfuscation":true}}`
	output, warnings, err := TranslateRequestBetween(ir.Chat, ir.Responses, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(warnings, "stream_options.include_obfuscation") {
		t.Fatalf("include_obfuscation recorded no warning: %+v", warnings)
	}
	if strings.Contains(string(output), "obfuscation") {
		t.Fatalf("obfuscation leaked into the translated body: %s", output)
	}

	clean := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true,` +
		`"stream_options":{"include_usage":true}}`
	_, warnings, err = TranslateRequestBetween(ir.Chat, ir.Responses, []byte(clean), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if warningField(warnings, "stream_options.include_obfuscation") {
		t.Fatalf("obfuscation warning without the flag: %+v", warnings)
	}
}
