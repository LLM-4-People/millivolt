package format

// Round 7 A1 regression: nested Anthropic content arrays are capped at 16
// levels. Without the cap the preflight re-unmarshals the remaining subtree at
// every level and the typed decode re-walks each nested content array, making a
// small body cubic in CPU and allocations (measured 23.3 s and 16.7 GiB at
// depth 800).

import (
	"strconv"
	"strings"
	"testing"
)

func anthropicNestedChain(levels int) string {
	content := `[{"type":"text","text":"x"}]`
	for i := 0; i < levels; i++ {
		content = `[{"type":"tool_result","tool_use_id":"t","content":` + content + `}]`
	}
	return content
}

func TestAnthropicNestedContentDepthIsBounded(t *testing.T) {
	for _, levels := range []int{0, 3, 15} {
		t.Run("request accepts "+strconv.Itoa(levels), func(t *testing.T) {
			body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":` + anthropicNestedChain(levels) + `}]}`
			if _, err := DecodeAnthropicRequest([]byte(body)); err != nil {
				t.Fatalf("depth %d request failed: %v", levels, err)
			}
		})
		t.Run("response accepts "+strconv.Itoa(levels), func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":` + anthropicNestedChain(levels) + `,"stop_reason":"end_turn"}`
			if _, err := DecodeAnthropicResponse([]byte(body)); err != nil {
				t.Fatalf("depth %d response failed: %v", levels, err)
			}
		})
	}

	for _, levels := range []int{16, 40} {
		t.Run("request rejects "+strconv.Itoa(levels), func(t *testing.T) {
			body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":` + anthropicNestedChain(levels) + `}]}`
			_, err := DecodeAnthropicRequest([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "nested content levels") {
				t.Fatalf("depth %d request error = %v, want the depth bound", levels, err)
			}
		})
		t.Run("response rejects "+strconv.Itoa(levels), func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":` + anthropicNestedChain(levels) + `,"stop_reason":"end_turn"}`
			_, err := DecodeAnthropicResponse([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "nested content levels") {
				t.Fatalf("depth %d response error = %v, want the depth bound", levels, err)
			}
		})
	}
}
