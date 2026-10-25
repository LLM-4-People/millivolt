package format

// Round 7 A4 regression: current Anthropic block fields that the canonical
// model cannot carry must be reported once per content array instead of being
// silently discarded by the typed decode.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicBlockFieldKnobs = `{"model":"m","max_tokens":16,"messages":[` +
	`{"role":"user","content":[` +
	`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="},"transformations":{"oversized_image":"error"}},` +
	`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="},"transformations":{"oversized_image":"downsize"}},` +
	`{"type":"tool_result","tool_use_id":"toolu_1","content":"x","toolset_name":"ts_user"}]},` +
	`{"role":"assistant","content":[` +
	`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1},"caller":{"type":"server"},"toolset_name":"ts_model"}]}]}`

const anthropicBlockFieldClean = `{"model":"m","max_tokens":16,"messages":[` +
	`{"role":"user","content":[` +
	`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGk="}},` +
	`{"type":"tool_result","tool_use_id":"toolu_1","content":"x"}]},` +
	`{"role":"assistant","content":[` +
	`{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":1}}]}]}`

func TestAnthropicBlockFieldsWarnWhenDropped(t *testing.T) {
	req, err := DecodeAnthropicRequest([]byte(anthropicBlockFieldKnobs))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"image.transformations",
		"tool_use.caller",
		"tool_use.toolset_name",
		"tool_result.toolset_name",
	} {
		if got := warningCount(req.Warnings, ir.WarningDropped, field); got != 1 {
			t.Fatalf("%s warnings = %d, want 1 (%+v)", field, got, req.Warnings)
		}
	}

	clean, err := DecodeAnthropicRequest([]byte(anthropicBlockFieldClean))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"image.transformations",
		"tool_use.caller",
		"tool_use.toolset_name",
		"tool_result.toolset_name",
	} {
		if warningField(clean.Warnings, field) {
			t.Fatalf("clean request warned for %s: %+v", field, clean.Warnings)
		}
	}
}
