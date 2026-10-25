package format

// Round 13 B-U regression: Chat reasoning_text is a compatibility fallback
// behind reasoning_content and reasoning. Structured reasoning_details are
// reported as dropped, never synthesized into visible or hidden text.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatReasoningResponse(t *testing.T, message string) *ir.Response {
	t.Helper()
	body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":` +
		message + `,"finish_reason":"stop"}]}`
	resp, err := DecodeChatResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func chatReasoningBlock(resp *ir.Response) *ir.Reasoning {
	for i := range resp.Blocks {
		if resp.Blocks[i].Type == ir.BlockReasoning {
			return resp.Blocks[i].Reasoning
		}
	}
	return nil
}

func TestChatReasoningTextFallback(t *testing.T) {
	t.Run("reasoning_text is one reasoning block", func(t *testing.T) {
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"","reasoning_text":"fallback chain"}`)
		reasoning := chatReasoningBlock(resp)
		if reasoning == nil || reasoning.Text != "fallback chain" {
			t.Fatalf("reasoning = %+v, want the reasoning_text value", reasoning)
		}
		if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_details"); got != 0 {
			t.Fatalf("plain reasoning_text warned: %+v", resp.Warnings)
		}
	})

	t.Run("reasoning_content wins", func(t *testing.T) {
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"","reasoning_content":"primary","reasoning_text":"fallback"}`)
		reasoning := chatReasoningBlock(resp)
		if reasoning == nil || reasoning.Text != "primary" {
			t.Fatalf("reasoning = %+v, want reasoning_content to win", reasoning)
		}
	})

	t.Run("reasoning wins over reasoning_text", func(t *testing.T) {
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"","reasoning":"middle","reasoning_text":"fallback"}`)
		reasoning := chatReasoningBlock(resp)
		if reasoning == nil || reasoning.Text != "middle" {
			t.Fatalf("reasoning = %+v, want reasoning to win", reasoning)
		}
	})

	t.Run("structured details are dropped, not invented", func(t *testing.T) {
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"ok","reasoning_details":[{"type":"reasoning.text","text":"structured"}]}`)
		if reasoning := chatReasoningBlock(resp); reasoning != nil {
			t.Fatalf("structured detail was invented into reasoning: %+v", reasoning)
		}
		if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_details"); got != 1 {
			t.Fatalf("reasoning_details warnings = %d, want 1: %+v", got, resp.Warnings)
		}
		for _, block := range resp.Blocks {
			if block.Type == ir.BlockText && strings.Contains(block.Text, "structured") {
				t.Fatalf("structured detail leaked into visible text: %+v", block)
			}
		}
	})

	t.Run("wrong-typed reasoning_text is reported, not fatal", func(t *testing.T) {
		for _, value := range []string{`42`, `{"detail":"x"}`, `[1]`, `true`} {
			resp := chatReasoningResponse(t, `{"role":"assistant","content":"ok","reasoning_text":`+value+`}`)
			if reasoning := chatReasoningBlock(resp); reasoning != nil {
				t.Fatalf("wrong-typed reasoning_text %s produced a block: %+v", value, reasoning)
			}
			if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_text"); got != 1 {
				t.Fatalf("wrong-typed reasoning_text %s warnings = %d, want 1: %+v", value, got, resp.Warnings)
			}
		}
		for _, value := range []string{`null`, `""`} {
			resp := chatReasoningResponse(t, `{"role":"assistant","content":"ok","reasoning_text":`+value+`}`)
			if reasoning := chatReasoningBlock(resp); reasoning != nil {
				t.Fatalf("empty reasoning_text %s produced a block: %+v", value, reasoning)
			}
			if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_text"); got != 0 {
				t.Fatalf("empty reasoning_text %s warned: %+v", value, resp.Warnings)
			}
		}

		// The fallback is consulted only after both primary spellings, so a
		// wrong-typed extension does not warn when reasoning_content wins.
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"ok","reasoning_content":"primary","reasoning_text":{"detail":"x"}}`)
		if reasoning := chatReasoningBlock(resp); reasoning == nil || reasoning.Text != "primary" {
			t.Fatalf("reasoning = %+v, want reasoning_content to win", reasoning)
		}
		if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_text"); got != 0 {
			t.Fatalf("hidden wrong-typed fallback warned: %+v", resp.Warnings)
		}

		// A client replaying such a message must decode exactly like the
		// provider response that produced it.
		body := `{"model":"m","messages":[{"role":"assistant","content":"ok","reasoning_text":{"detail":"x"}}]}`
		if _, err := DecodeChatRequest([]byte(body)); err != nil {
			t.Fatalf("request replay with a wrong-typed reasoning_text aborted: %v", err)
		}
	})

	t.Run("empty details carry no loss", func(t *testing.T) {
		for _, value := range []string{`[]`, `[ ]`, `{}`, `{ }`, `""`, `null`} {
			resp := chatReasoningResponse(t, `{"role":"assistant","content":"ok","reasoning_details":`+value+`}`)
			if reasoning := chatReasoningBlock(resp); reasoning != nil {
				t.Fatalf("empty reasoning_details %s produced a block: %+v", value, reasoning)
			}
			if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_details"); got != 0 {
				t.Fatalf("empty reasoning_details %s warned: %+v", value, resp.Warnings)
			}
		}
	})

	t.Run("plain text wins over structured details", func(t *testing.T) {
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"","reasoning_text":"plain","reasoning_details":[{"type":"reasoning.text","text":"structured"}]}`)
		reasoning := chatReasoningBlock(resp)
		if reasoning == nil || reasoning.Text != "plain" {
			t.Fatalf("reasoning = %+v, want the explicit plain text", reasoning)
		}
		if strings.Contains(reasoning.Text, "structured") {
			t.Fatalf("structured detail leaked into reasoning: %+v", reasoning)
		}
		if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_details"); got != 1 {
			t.Fatalf("reasoning_details warnings = %d, want 1: %+v", got, resp.Warnings)
		}
	})

	t.Run("absent fields stay silent", func(t *testing.T) {
		resp := chatReasoningResponse(t, `{"role":"assistant","content":"ok"}`)
		if reasoning := chatReasoningBlock(resp); reasoning != nil {
			t.Fatalf("absent reasoning fields produced a block: %+v", reasoning)
		}
		if got := warningCount(resp.Warnings, ir.WarningDropped, "reasoning_details"); got != 0 {
			t.Fatalf("absent reasoning_details warned: %+v", resp.Warnings)
		}
	})
}
