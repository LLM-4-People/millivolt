package format

// Batch 8 regressions: a target-invalid request field is never emitted, and
// documented approximations are explicit. Structured-output names, reasoning
// effort vocabularies, service tiers and the Chat stop-list ceiling all have
// one owner.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func decodeJSONObject(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	return out
}

func TestAnthropicJSONSchemaNameSynthesized(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	chatDoc := decodeJSONObject(t, chat)
	rf, _ := chatDoc["response_format"].(map[string]any)
	js, _ := rf["json_schema"].(map[string]any)
	if js["name"] != defaultResponseFormatName {
		t.Fatalf("Chat json_schema name = %v, want %q: %s", js["name"], defaultResponseFormatName, chat)
	}
	responses, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := decodeJSONObject(t, responses)["text"].(map[string]any)
	format, _ := text["format"].(map[string]any)
	if format["name"] != defaultResponseFormatName {
		t.Fatalf("Responses json_schema name = %v, want %q: %s", format["name"], defaultResponseFormatName, responses)
	}
}

func TestReasoningEffortNoneOmittedForAnthropic(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeAnthropicRequest(req, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := decodeJSONObject(t, out)
	config, _ := doc["output_config"].(map[string]any)
	if _, present := config["effort"]; present {
		t.Fatalf("Anthropic body carries effort:none: %s", out)
	}
	if !warned(req.Warnings, "reasoning.effort") {
		t.Fatalf("none effort recorded no approximation warning: %+v", req.Warnings)
	}
}

func TestThinkingBudgetMapsToChatEffort(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":6000}}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONObject(t, out)["reasoning_effort"]; got != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium: %s", got, out)
	}
	if !warned(req.Warnings, "reasoning.budget_tokens") {
		t.Fatalf("budget mapping recorded no warning: %+v", req.Warnings)
	}
}

func TestThinkingDisabledMapsToNone(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"disabled"}}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	chat, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeJSONObject(t, chat)["reasoning_effort"]; got != "none" {
		t.Fatalf("Chat reasoning_effort = %v, want none: %s", got, chat)
	}
	responses, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	reasoning, _ := decodeJSONObject(t, responses)["reasoning"].(map[string]any)
	if reasoning["effort"] != "none" {
		t.Fatalf("Responses reasoning.effort = %v, want none: %s", reasoning["effort"], responses)
	}
}

func TestAdaptiveThinkingWarns(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"adaptive"}}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !warned(req.Warnings, "thinking.type") {
		t.Fatalf("adaptive thinking recorded no warning: %+v", req.Warnings)
	}
}

func TestServiceTierCrossVocabulary(t *testing.T) {
	cases := []struct {
		name     string
		target   ir.Protocol
		source   string
		want     string
		wantWarn bool
	}{
		{"auto passes", ir.Anthropic, "auto", "auto", false},
		{"chat default to standard_only", ir.Anthropic, "default", "standard_only", true},
		{"standard_only to chat default", ir.Chat, "standard_only", "default", true},
		{"standard_only to responses default", ir.Responses, "standard_only", "default", true},
		{"chat flex stays", ir.Chat, "flex", "flex", false},
		{"chat flex to anthropic dropped", ir.Anthropic, "flex", "", true},
		{"responses ultrafast to chat dropped", ir.Chat, "ultrafast", "", true},
		{"chat scale to responses passes", ir.Responses, "scale", "scale", false},
		{"anthropic unknown dropped", ir.Anthropic, "bogus", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warns := mapServiceTier(tc.target, tc.source)
			if got != tc.want {
				t.Fatalf("mapped %q -> %q, want %q", tc.source, got, tc.want)
			}
			if tc.wantWarn != (len(warns) > 0) {
				t.Fatalf("warnings = %+v, wantWarn %v", warns, tc.wantWarn)
			}
		})
	}
}

func TestChatStopSequencesClamped(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"stop":["a","b","c","d","e"]}`
	req, err := DecodeChatRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := EncodeChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	stops, _ := decodeJSONObject(t, out)["stop"].([]any)
	if len(stops) != ChatMaxStopSequences {
		t.Fatalf("stop count = %d, want %d: %s", len(stops), ChatMaxStopSequences, out)
	}
	if !warned(req.Warnings, "stop") {
		t.Fatalf("clamped stop recorded no warning: %+v", req.Warnings)
	}
}

func TestResponsesReasoningFieldsDecoded(t *testing.T) {
	body := `{"model":"m","input":"hi","reasoning":{"effort":"high","generate_summary":"auto",` +
		`"context":"current_turn","mode":"standard"}}`
	req, err := DecodeResponsesRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if req.Reasoning == nil || req.Reasoning.Summary != "auto" ||
		req.Reasoning.Context != "current_turn" || req.Reasoning.Mode != "standard" {
		t.Fatalf("reasoning = %+v, want summary/context/mode decoded", req.Reasoning)
	}
	out, err := EncodeResponsesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	reasoning, _ := decodeJSONObject(t, out)["reasoning"].(map[string]any)
	if reasoning["summary"] != "auto" || reasoning["context"] != "current_turn" || reasoning["mode"] != "standard" {
		t.Fatalf("reasoning round trip = %v: %s", reasoning, out)
	}
	// The summary has no Anthropic spelling and must be reported.
	if _, err := EncodeAnthropicRequest(req, 0, ""); err != nil {
		t.Fatal(err)
	}
	if !warned(req.Warnings, "reasoning.summary") || !warned(req.Warnings, "reasoning.context") {
		t.Fatalf("cross-protocol reasoning losses not reported: %+v", req.Warnings)
	}
}

// TestChatScaleTierReachesResponses pins TC1-3: Responses accepts the scale
// tier, so a Chat scale request must pass through instead of being dropped.
func TestChatScaleTierReachesResponses(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"service_tier":"scale"}`
	out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Responses, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONObject(t, out)
	if root["service_tier"] != "scale" {
		t.Fatalf("service_tier = %v, want scale: %s", root["service_tier"], out)
	}
	if warned(warnings, "service_tier") {
		t.Fatalf("scale recorded a tier warning: %+v", warnings)
	}
}

// TestResponsesReasoningBudgetWarnsWithEffort pins TC1-5: Responses has no
// token budget, so a budget beside an explicit effort must be reported (and a
// disabled request must report both knobs).
func TestResponsesReasoningBudgetWarnsWithEffort(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],` +
		`"thinking":{"type":"enabled","budget_tokens":6000},"output_config":{"effort":"high"}}`
	out, warnings, err := TranslateRequestBetween(ir.Anthropic, ir.Responses, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if !warned(warnings, "reasoning.budget_tokens") {
		t.Fatalf("budget beside effort recorded no warning: %+v", warnings)
	}
	root := decodeJSONObject(t, out)
	reasoning, _ := root["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %v, want effort high: %s", reasoning, out)
	}

	enabled := false
	budget := 6000
	maxTokens := 16
	req := &ir.Request{
		Model: "m", MaxOutputTokens: &maxTokens,
		Messages:  []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
		Reasoning: &ir.ReasoningConfig{Enabled: &enabled, BudgetTokens: &budget, Effort: "high"},
	}
	if _, err := EncodeResponsesRequest(req); err != nil {
		t.Fatal(err)
	}
	if !warned(req.Warnings, "reasoning.budget_tokens") {
		t.Fatalf("disabled budget recorded no warning: %+v", req.Warnings)
	}
}

// TestChatTargetEmitsStore pins TC1-6: Chat Completions accepts store, so a
// translated store flag is carried instead of rejected. Anthropic still
// rejects store=true because it has no stateful storage.
func TestChatTargetEmitsStore(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`{"model":"m","input":"hi","store":true}`, true},
		{`{"model":"m","input":"hi","store":false}`, false},
	} {
		out, _, err := TranslateRequestBetween(ir.Responses, ir.Chat, []byte(tc.body), 16, "")
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		root := decodeJSONObject(t, out)
		if got, ok := root["store"].(bool); !ok || got != tc.want {
			t.Fatalf("chat store = %v, want %v: %s", root["store"], tc.want, out)
		}
	}
	if _, _, err := TranslateRequestBetween(ir.Responses, ir.Anthropic,
		[]byte(`{"model":"m","input":"hi","store":true}`), 16, ""); err == nil {
		t.Fatal("anthropic target accepted store=true")
	}
}
