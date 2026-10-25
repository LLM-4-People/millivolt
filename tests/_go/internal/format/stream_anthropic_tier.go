package format

// Round 13 C2 regression: an Anthropic stream's message_start carries the
// service tier mapped from the source start event, exactly like the non-stream
// encoder. A tier that arrives only after the stream opened is not rewritten,
// and message_delta's narrower usage shape never gains the tier.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func anthropicStartUsage(t *testing.T, out string) map[string]any {
	t.Helper()
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "message_start" {
			continue
		}
		message, _ := payload["message"].(map[string]any)
		usage, _ := message["usage"].(map[string]any)
		if usage == nil {
			t.Fatalf("message_start has no usage object: %s", out)
		}
		return usage
	}
	t.Fatalf("no message_start in output: %s", out)
	return nil
}

func anthropicDeltaUsageIsNarrow(t *testing.T, out string) {
	t.Helper()
	found := false
	for _, payload := range mustAnthropicPayloads(t, out) {
		if payload["type"] != "message_delta" {
			continue
		}
		found = true
		usage, _ := payload["usage"].(map[string]any)
		for _, field := range []string{"service_tier", "speed"} {
			if _, ok := usage[field]; ok {
				t.Fatalf("message_delta usage gained %q: %s", field, out)
			}
		}
	}
	if !found {
		t.Fatalf("no message_delta in output: %s", out)
	}
}

func TestAnthropicStreamServiceTier(t *testing.T) {
	cases := []struct {
		tier  string
		want  any
		speed any
	}{
		{"priority", "priority", nil},
		{"fast", nil, "fast"},
		{"default", "standard", nil},
		{"weird", nil, nil},
		{"", nil, nil},
	}
	for _, tc := range cases {
		t.Run("responses/"+tc.tier, func(t *testing.T) {
			created := `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"`
			if tc.tier != "" {
				created += `,"service_tier":"` + tc.tier + `"`
			}
			created += `}}

event: response.completed
data: {"type":"response.completed","response":{"id":"r1","model":"m","status":"completed","output":[]}}

`
			var buf bytes.Buffer
			if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, strings.NewReader(created), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			usage := anthropicStartUsage(t, buf.String())
			if usage["service_tier"] != tc.want || usage["speed"] != tc.speed {
				t.Fatalf("message_start usage tier/speed = %v/%v, want %v/%v: %s",
					usage["service_tier"], usage["speed"], tc.want, tc.speed, buf.String())
			}
			anthropicDeltaUsageIsNarrow(t, buf.String())
		})
		t.Run("chat/"+tc.tier, func(t *testing.T) {
			chunk := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m"`
			if tc.tier != "" {
				chunk += `,"service_tier":"` + tc.tier + `"`
			}
			chunk += `,"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
			var buf bytes.Buffer
			if err := TranslateStreamBetween(ir.Anthropic, ir.Chat, &buf, strings.NewReader(chunk), nil, nil, StreamOptions{}); err != nil {
				t.Fatal(err)
			}
			usage := anthropicStartUsage(t, buf.String())
			if usage["service_tier"] != tc.want || usage["speed"] != tc.speed {
				t.Fatalf("message_start usage tier/speed = %v/%v, want %v/%v: %s",
					usage["service_tier"], usage["speed"], tc.want, tc.speed, buf.String())
			}
			anthropicDeltaUsageIsNarrow(t, buf.String())
		})
	}

	// The non-stream owner is the one warning channel for an unknown tier.
	_, _, warns := anthropicResponseUsageTier("weird")
	if len(warns) != 1 || warns[0].Kind != ir.WarningDropped || warns[0].Field != "service_tier" {
		t.Fatalf("unknown tier warnings = %+v, want one dropped service_tier", warns)
	}
}
