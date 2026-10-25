package format

// Round 8 H2 regression: block-level AddOnce warnings restarted for every
// message because each message decoded into its own warning slice. Two messages
// carrying the same loss therefore produced two header entries, so one request
// could amplify the warning header with one copy per message.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicTransformedImage = `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"},"transformations":{"oversized_image":"downsize"}}`

func anthropicWarnCount(warns ir.Warnings, field string) int {
	count := 0
	for _, w := range warns {
		if w.Field == field {
			count++
		}
	}
	return count
}

func TestAnthropicBlockWarningsDedupePerRequest(t *testing.T) {
	t.Run("request spans messages", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[` +
			`{"role":"user","content":[` + anthropicTransformedImage + `]},` +
			`{"role":"assistant","content":[` + anthropicTransformedImage + `,` +
			`{"type":"tool_use","id":"toolu_1","name":"f","input":{},"caller":{"type":"direct"},"toolset_name":"ts1"}]},` +
			`{"role":"user","content":[{"type":"tool_use","id":"toolu_2","name":"g","input":{},"caller":{"type":"direct"},"toolset_name":"ts2"}]}]}`
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"image.transformations", "tool_use.caller", "tool_use.toolset_name"} {
			if got := anthropicWarnCount(req.Warnings, field); got != 1 {
				t.Fatalf("%s warnings = %d, want 1 (%+v)", field, got, req.Warnings)
			}
		}
	})

	t.Run("request without the fields warns none", func(t *testing.T) {
		body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":"ok"}]}`
		req, err := DecodeAnthropicRequest([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"image.transformations", "tool_use.caller", "tool_use.toolset_name"} {
			if got := anthropicWarnCount(req.Warnings, field); got != 0 {
				t.Fatalf("%s warnings = %d, want 0 (%+v)", field, got, req.Warnings)
			}
		}
	})

	t.Run("response spans content blocks", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[` +
			anthropicTransformedImage + `,` + anthropicTransformedImage + `,` +
			`{"type":"tool_use","id":"toolu_1","name":"f","input":{},"caller":{"type":"direct"}},` +
			`{"type":"tool_use","id":"toolu_2","name":"g","input":{},"caller":{"type":"direct"}}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		resp, err := DecodeAnthropicResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"image.transformations", "tool_use.caller"} {
			if got := anthropicWarnCount(resp.Warnings, field); got != 1 {
				t.Fatalf("%s warnings = %d, want 1 (%+v)", field, got, resp.Warnings)
			}
		}
	})
}
