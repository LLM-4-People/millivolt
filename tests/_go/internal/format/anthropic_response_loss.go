package format

// Round 13 B2 regression: the five Anthropic response members that have no
// Chat or Responses equivalent are decoded as raw values and reported when
// non-null. Their normal null/absent spellings stay warning-free.

import (
	"bytes"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

const anthropicLossBase = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"container":null,"diagnostics":null,"usage":{"input_tokens":3,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"service_tier":"standard","speed":null,"cache_creation":null,"inference_geo":null,"server_tool_use":null,"output_tokens_details":null}}`

func TestAnthropicResponseOnlyMembersWarn(t *testing.T) {
	fields := []string{"container", "diagnostics", "usage.cache_creation", "usage.inference_geo", "usage.server_tool_use"}

	extras := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"stop_details":null,"container":{"id":"ctr_1","type":"workspace"},"diagnostics":{"reason":"gateway"},"usage":{"input_tokens":3,"output_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"service_tier":"standard","speed":null,"cache_creation":{"ephemeral_5m_input_tokens":4},"inference_geo":"us","server_tool_use":{"web_search_requests":1},"output_tokens_details":null}}`
	resp, err := DecodeAnthropicResponse([]byte(extras))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if got := warningCount(resp.Warnings, ir.WarningDropped, field); got != 1 {
			t.Fatalf("%s warnings = %d, want 1: %+v", field, got, resp.Warnings)
		}
	}
	dropped := 0
	for _, warning := range resp.Warnings {
		if warning.Kind == ir.WarningDropped {
			dropped++
		}
	}
	if dropped != len(fields) {
		t.Fatalf("dropped warnings = %d, want exactly the five response-only fields: %+v", dropped, resp.Warnings)
	}

	for name, body := range map[string]string{
		"all null": anthropicLossBase,
		"absent":   `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			clean, err := DecodeAnthropicResponse([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range fields {
				if got := warningCount(clean.Warnings, ir.WarningDropped, field); got != 0 {
					t.Fatalf("%s warned on the empty spelling: %+v", field, clean.Warnings)
				}
			}
		})
	}

	t.Run("stop details still warns once", func(t *testing.T) {
		body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_details":{"reason":"x"},"usage":{"input_tokens":3,"output_tokens":1}}`
		got, err := DecodeAnthropicResponse([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		if n := warningCount(got.Warnings, ir.WarningDropped, "stop_details"); n != 1 {
			t.Fatalf("stop_details warnings = %d, want 1: %+v", n, got.Warnings)
		}
	})

	t.Run("output bytes unchanged", func(t *testing.T) {
		withExtras, err := DecodeAnthropicResponse([]byte(extras))
		if err != nil {
			t.Fatal(err)
		}
		base, err := DecodeAnthropicResponse([]byte(anthropicLossBase))
		if err != nil {
			t.Fatal(err)
		}
		got, err := EncodeAnthropicResponse(withExtras)
		if err != nil {
			t.Fatal(err)
		}
		want, err := EncodeAnthropicResponse(base)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("response-only members changed the encoded bytes:\n got %s\nwant %s", got, want)
		}
	})
}
