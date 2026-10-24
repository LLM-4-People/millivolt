package format

// Round 6 batch 13 regressions for the Anthropic request/response encoder:
// A2 keeps a mid-conversation system turn, A5 rejects an overlong
// metadata.user_id, A6 maps the priority tier onto Anthropic fast speed, and
// B2 reports dropped response metadata.

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicMessagesOf(t *testing.T, body []byte) []any {
	t.Helper()
	root := decodeJSONMap(t, body)
	messages, _ := root["messages"].([]any)
	return messages
}

func TestAnthropicKeepsMidConversationSystem(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"system","content":"lead rule"},` +
		`{"role":"user","content":"u1"},` +
		`{"role":"system","content":"mid rule"},` +
		`{"role":"assistant","content":"a2"}]}`
	out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	root := decodeJSONMap(t, out)
	if root["system"] != "lead rule" {
		t.Fatalf("top-level system = %v, want only the leading rule: %s", root["system"], out)
	}
	messages := anthropicMessagesOf(t, out)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want user/system/assistant: %s", len(messages), out)
	}
	mid := messages[1].(map[string]any)
	if mid["role"] != "system" || mid["content"] != "mid rule" {
		t.Fatalf("messages[1] = %v, want the mid system turn in place: %s", mid, out)
	}
	if warningField(warnings, "system") {
		t.Fatalf("mid system turn was reported as moved: %+v", warnings)
	}

	// A native Anthropic round trip preserves the position.
	round, _, err := TranslateRequestBetween(ir.Anthropic, ir.Anthropic, out, 16, "")
	if err != nil {
		t.Fatal(err)
	}
	roundMessages := anthropicMessagesOf(t, round)
	if len(roundMessages) != 3 || roundMessages[1].(map[string]any)["role"] != "system" {
		t.Fatalf("native round trip lost the mid system position: %s", round)
	}
	if decodeJSONMap(t, round)["system"] != "lead rule" {
		t.Fatalf("native round trip changed the top-level system: %s", round)
	}
}

func TestAnthropicLiftsDeveloperWithWarning(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[` +
		`{"role":"user","content":"u1"},` +
		`{"role":"developer","content":"dev rule"},` +
		`{"role":"user","content":"u2"}]}`
	out, warnings, err := TranslateRequestBetween(ir.Chat, ir.Anthropic, []byte(body), 16, "")
	if err != nil {
		t.Fatal(err)
	}
	if decodeJSONMap(t, out)["system"] != "dev rule" {
		t.Fatalf("mid developer was not lifted: %s", out)
	}
	found := false
	for _, w := range warnings {
		if w.Field == "system" && w.Kind == ir.WarningApproximated {
			found = true
		}
	}
	if !found {
		t.Fatalf("mid developer lift produced no approximation warning: %+v", warnings)
	}
}

func TestAnthropicRejectsOverlongMetadataUserID(t *testing.T) {
	encode := func(user string) ([]byte, error) {
		return EncodeAnthropicRequest(&ir.Request{
			Model: "m", MaxOutputTokens: intPtr(16), User: user,
			Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
		}, 16, "")
	}
	for _, tc := range []struct {
		name    string
		user    string
		wantErr bool
	}{
		{name: "512 ascii", user: strings.Repeat("a", 512)},
		{name: "513 ascii", user: strings.Repeat("a", 513), wantErr: true},
		{name: "512 multibyte runes", user: strings.Repeat("é", 512)},
		{name: "513 multibyte runes", user: strings.Repeat("é", 513), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := encode(tc.user)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "metadata.user_id") {
					t.Fatalf("error = %v, want the metadata.user_id bound (body %s)", err, out)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			metadata := decodeJSONMap(t, out)["metadata"].(map[string]any)
			if metadata["user_id"] != tc.user {
				t.Fatalf("metadata.user_id was not preserved: %s", out)
			}
			if utf8.RuneCountInString(tc.user) != 512 {
				t.Fatalf("test value has %d runes, want 512", utf8.RuneCountInString(tc.user))
			}
		})
	}
}

func TestAnthropicPriorityRequestsFastSpeed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tier       string
		wantSpeed  string
		wantTier   string
		wantWarn   bool
		warnIsDrop bool
	}{
		{name: "priority", tier: "priority", wantSpeed: "fast", wantWarn: true},
		{name: "fast", tier: "fast", wantSpeed: "fast"},
		{name: "unknown", tier: "turbo", wantWarn: true, warnIsDrop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &ir.Request{
				Model: "m", MaxOutputTokens: intPtr(16), ServiceTier: tc.tier,
				Messages: []ir.Message{{Role: ir.RoleUser, Content: []ir.Block{ir.TextBlock("hi")}}},
			}
			out, err := EncodeAnthropicRequest(req, 16, "")
			if err != nil {
				t.Fatal(err)
			}
			root := decodeJSONMap(t, out)
			if tc.wantSpeed == "" {
				if _, ok := root["speed"]; ok {
					t.Fatalf("speed present for tier %q: %s", tc.tier, out)
				}
			} else if root["speed"] != tc.wantSpeed {
				t.Fatalf("speed = %v, want %q: %s", root["speed"], tc.wantSpeed, out)
			}
			if root["service_tier"] != nil {
				t.Fatalf("service_tier = %v, want absent: %s", root["service_tier"], out)
			}
			var matched *ir.Warning
			for i := range req.Warnings {
				if req.Warnings[i].Field == "service_tier" {
					matched = &req.Warnings[i]
				}
			}
			if tc.wantWarn {
				if matched == nil {
					t.Fatalf("tier %q produced no warning: %+v", tc.tier, req.Warnings)
				}
				wantKind := ir.WarningApproximated
				if tc.warnIsDrop {
					wantKind = ir.WarningDropped
				}
				if matched.Kind != wantKind {
					t.Fatalf("warning = %+v, want kind %q", *matched, wantKind)
				}
			} else if matched != nil {
				t.Fatalf("fast tier produced a warning: %+v", req.Warnings)
			}
		})
	}
}

func TestAnthropicResponseReportsDroppedMetadata(t *testing.T) {
	resp := &ir.Response{
		ID: "msg_1", Model: "m", Finish: ir.FinishStop,
		Blocks:   []ir.Block{ir.TextBlock("hi")},
		Metadata: map[string]string{"k": "v"},
	}
	out, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"metadata"`) {
		t.Fatalf("Anthropic response carried a non-schema metadata field: %s", out)
	}
	if !warningField(resp.Warnings, "metadata") {
		t.Fatalf("dropped response metadata produced no warning: %+v", resp.Warnings)
	}

	clean := &ir.Response{
		ID: "msg_1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{ir.TextBlock("hi")},
	}
	if _, err := EncodeAnthropicResponse(clean); err != nil {
		t.Fatal(err)
	}
	if warningField(clean.Warnings, "metadata") {
		t.Fatalf("metadata-free response warned: %+v", clean.Warnings)
	}
}
