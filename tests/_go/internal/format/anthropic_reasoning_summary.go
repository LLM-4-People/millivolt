package format

// Round 5 B1 regression: an Anthropic target cannot carry a separate reasoning
// summary. A foreign summary-only block is preserved as visible text; a block
// with raw text (or a native Anthropic thinking block) keeps its text and
// reports the summary loss explicitly instead of dropping it silently.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicReasoningBody(t *testing.T, blocks []ir.Block) (string, ir.Warnings) {
	t.Helper()
	resp := &ir.Response{ID: "msg_1", Model: "m", Finish: ir.FinishStop, Blocks: blocks}
	body, err := EncodeAnthropicResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, block := range doc.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "thinking":
			text.WriteString(block.Thinking)
		}
	}
	return text.String(), resp.Warnings
}

func warningCount(warnings ir.Warnings, kind ir.WarningKind, field string) int {
	count := 0
	for _, warning := range warnings {
		if warning.Kind == kind && warning.Field == field {
			count++
		}
	}
	return count
}

func TestAnthropicReasoningSummaryReportsLoss(t *testing.T) {
	t.Run("text and summary", func(t *testing.T) {
		text, warnings := anthropicReasoningBody(t, []ir.Block{{
			Type:      ir.BlockReasoning,
			Reasoning: &ir.Reasoning{Text: "the raw chain", Summary: []string{"the summary"}},
		}})
		if text != "the raw chain" || strings.Contains(text, "the summary") {
			t.Fatalf("visible text = %q, want only the raw chain", text)
		}
		if warningCount(warnings, ir.WarningApproximated, "reasoning") != 1 {
			t.Fatalf("approximated reasoning warnings = %+v, want one", warnings)
		}
		if warningCount(warnings, ir.WarningDropped, "reasoning.summary") != 1 {
			t.Fatalf("summary loss not reported: %+v", warnings)
		}
	})

	t.Run("summary only", func(t *testing.T) {
		text, warnings := anthropicReasoningBody(t, []ir.Block{{
			Type:      ir.BlockReasoning,
			Reasoning: &ir.Reasoning{Summary: []string{"the summary"}},
		}})
		if text != "the summary" {
			t.Fatalf("visible text = %q, want the summary preserved", text)
		}
		if warningCount(warnings, ir.WarningApproximated, "reasoning") != 1 {
			t.Fatalf("summary-only approximation not reported: %+v", warnings)
		}
		if warningCount(warnings, ir.WarningDropped, "reasoning.summary") != 0 {
			t.Fatalf("preserved summary reported as dropped: %+v", warnings)
		}
	})

	t.Run("native thinking", func(t *testing.T) {
		text, warnings := anthropicReasoningBody(t, []ir.Block{{
			Type: ir.BlockReasoning,
			Reasoning: &ir.Reasoning{
				Provider: ir.Anthropic, Signature: "sig", Text: "the raw chain", Summary: []string{"the summary"},
			},
		}})
		if text != "the raw chain" {
			t.Fatalf("native thinking text = %q, want the raw chain", text)
		}
		if warningCount(warnings, ir.WarningDropped, "reasoning.summary") != 1 {
			t.Fatalf("native summary loss not reported: %+v", warnings)
		}
		if warningCount(warnings, ir.WarningApproximated, "reasoning") != 0 {
			t.Fatalf("native thinking reported as approximated: %+v", warnings)
		}
	})
}

func TestAnthropicReasoningSummaryAndEncryptedReportsBoth(t *testing.T) {
	text, warnings := anthropicReasoningBody(t, []ir.Block{{
		Type: ir.BlockReasoning,
		Reasoning: &ir.Reasoning{
			Summary:   []string{"the summary"},
			Encrypted: "enc",
		},
	}})
	if text != "the summary" {
		t.Fatalf("visible text = %q, want the summary preserved", text)
	}
	if warningCount(warnings, ir.WarningApproximated, "reasoning") != 1 {
		t.Fatalf("summary approximation not reported: %+v", warnings)
	}
	if warningCount(warnings, ir.WarningOpaque, "reasoning") != 1 {
		t.Fatalf("encrypted artifact loss not reported: %+v", warnings)
	}
	if warningCount(warnings, ir.WarningDropped, "reasoning") != 0 {
		t.Fatalf("encrypted summary reported as a generic drop: %+v", warnings)
	}
}
