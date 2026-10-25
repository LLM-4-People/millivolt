package format

// Round 4 batch 7 regressions (B1, B2): every emitted service tier belongs to
// the target vocabulary, and Anthropic response usage tier/speed reaches the
// canonical response and the stream start event.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func serviceTierWarnings(warnings ir.Warnings) int {
	count := 0
	for _, warning := range warnings {
		if warning.Field == "service_tier" {
			count++
		}
	}
	return count
}

func TestChatResponseServiceTierMapped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tier    string
		want    string
		warning bool
	}{
		{name: "ultrafast is outside the Chat vocabulary", tier: "ultrafast", want: "", warning: true},
		{name: "priority passes", tier: "priority", want: "priority"},
		{name: "fast passes", tier: "fast", want: "fast"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &ir.Response{
				ID: "r1", Model: "m", Finish: ir.FinishStop,
				Blocks:      []ir.Block{ir.TextBlock("hi")},
				ServiceTier: tc.tier,
			}
			body, err := EncodeChatResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			got, present := out["service_tier"]
			if tc.want == "" {
				if present {
					t.Fatalf("Chat body carried service_tier %v, want it omitted", got)
				}
			} else if got != tc.want {
				t.Fatalf("Chat service_tier = %v, want %q", got, tc.want)
			}
			if count := serviceTierWarnings(resp.Warnings); (count == 1) != tc.warning {
				t.Fatalf("service_tier warnings = %d, want warning %v: %+v", count, tc.warning, resp.Warnings)
			}
		})
	}
}

func TestChatStreamServiceTierMapped(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier string
		want string
	}{
		{name: "ultrafast is dropped", tier: "ultrafast", want: ""},
		{name: "priority passes", tier: "priority", want: "priority"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			enc := NewChatStreamEncoder(&buf, nil, false)
			for _, ev := range []*ir.Event{
				{Type: ir.EventStart, Response: &ir.Response{ID: "c1", Model: "m", Created: 1, ServiceTier: tc.tier}},
				{Type: ir.EventBlockStart, Index: 0, Block: &ir.Block{Type: ir.BlockText}},
				{Type: ir.EventBlockDelta, Index: 0, Delta: &ir.Delta{Text: "hi"}},
				{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: ir.FinishStop}},
				{Type: ir.EventDone},
			} {
				if err := enc.Write(ev); err != nil {
					t.Fatal(err)
				}
			}
			if err := enc.Close(); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			if tc.want == "" {
				if strings.Contains(out, "service_tier") {
					t.Fatalf("unrepresentable tier reached a chunk: %s", out)
				}
				return
			}
			if !strings.Contains(out, `"service_tier":"`+tc.want+`"`) {
				t.Fatalf("mapped tier missing from chunks: %s", out)
			}
			if strings.Contains(out, "ultrafast") {
				t.Fatalf("foreign tier leaked into chunks: %s", out)
			}
		})
	}
}

func TestAnthropicUsageServiceTierSpeedPreserved(t *testing.T) {
	for _, tc := range []struct {
		name        string
		usage       string
		want        string
		wantWarning ir.WarningKind
	}{
		{
			name:  "priority wins over fast",
			usage: `"usage":{"input_tokens":1,"output_tokens":1,"service_tier":"priority","speed":"fast"}`,
			want:  "priority",
		},
		{
			name:  "fast speed maps to fast",
			usage: `"usage":{"input_tokens":1,"output_tokens":1,"speed":"fast"}`,
			want:  "fast",
		},
		{
			name:        "standard approximates default",
			usage:       `"usage":{"input_tokens":1,"output_tokens":1,"service_tier":"standard","speed":"standard"}`,
			want:        "default",
			wantWarning: ir.WarningApproximated,
		},
		{
			name:        "batch is dropped",
			usage:       `"usage":{"input_tokens":1,"output_tokens":1,"service_tier":"batch"}`,
			want:        "",
			wantWarning: ir.WarningDropped,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
				`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` + tc.usage + `}`
			resp, err := DecodeAnthropicResponse([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if resp.ServiceTier != tc.want {
				t.Fatalf("decoded tier = %q, want %q", resp.ServiceTier, tc.want)
			}
			if tc.want == "" && resp.Warnings == nil {
				t.Fatal("no warning for the dropped tier")
			}
			if tc.wantWarning != "" {
				found := false
				for _, warning := range resp.Warnings {
					if warning.Field == "service_tier" && warning.Kind == tc.wantWarning {
						found = true
					}
				}
				if !found {
					t.Fatalf("service_tier %s warning missing: %+v", tc.wantWarning, resp.Warnings)
				}
			}

			chatBody, err := EncodeChatResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			var chat map[string]any
			if err := json.Unmarshal(chatBody, &chat); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if _, present := chat["service_tier"]; present {
					t.Fatalf("dropped tier reached the Chat body: %s", chatBody)
				}
			} else if chat["service_tier"] != tc.want {
				t.Fatalf("Chat service_tier = %v, want %q", chat["service_tier"], tc.want)
			}

			responsesBody, err := EncodeResponsesResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			var responses map[string]any
			if err := json.Unmarshal(responsesBody, &responses); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if _, present := responses["service_tier"]; present {
					t.Fatalf("dropped tier reached the Responses body: %s", responsesBody)
				}
			} else if responses["service_tier"] != tc.want {
				t.Fatalf("Responses service_tier = %v, want %q", responses["service_tier"], tc.want)
			}
		})
	}
}

func TestAnthropicStreamMessageStartKeepsServiceTier(t *testing.T) {
	in := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0,"service_tier":"priority","speed":"fast"}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Anthropic, &buf, strings.NewReader(in), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"service_tier":"priority"`) {
		t.Fatalf("message_start tier did not reach the Chat chunks: %s", out)
	}
	if strings.Contains(out, "ultrafast") {
		t.Fatalf("foreign tier leaked: %s", out)
	}
}

// TestAnthropicResponseUsageServiceTierReverse pins the IR-to-Anthropic
// direction of the usage tier mapping: the canonical tier becomes the
// documented response usage spelling, and an unrepresentable tier is omitted
// with a dropped warning.
func TestAnthropicResponseUsageServiceTierReverse(t *testing.T) {
	for _, tc := range []struct {
		name        string
		tier        string
		usageTier   string
		speed       string
		wantWarning bool
	}{
		{name: "default becomes standard", tier: "default", usageTier: "standard"},
		{name: "priority passes", tier: "priority", usageTier: "priority"},
		{name: "fast becomes speed", tier: "fast", speed: "fast"},
		{name: "batch passes", tier: "batch", usageTier: "batch"},
		{name: "scale is dropped", tier: "scale", wantWarning: true},
		{name: "empty emits neither", tier: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &ir.Response{
				ID: "msg_1", Model: "m", Finish: ir.FinishStop,
				Blocks:      []ir.Block{ir.TextBlock("hi")},
				Usage:       ir.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2},
				ServiceTier: tc.tier,
			}
			body, err := EncodeAnthropicResponse(resp)
			if err != nil {
				t.Fatal(err)
			}
			var out struct {
				Usage map[string]any `json:"usage"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatal(err)
			}
			gotTier, _ := out.Usage["service_tier"].(string)
			gotSpeed, _ := out.Usage["speed"].(string)
			if gotTier != tc.usageTier || gotSpeed != tc.speed {
				t.Fatalf("usage tier/speed = %q/%q, want %q/%q (body %s)", gotTier, gotSpeed, tc.usageTier, tc.speed, body)
			}
			count := serviceTierWarnings(resp.Warnings)
			if (count > 0) != tc.wantWarning {
				t.Fatalf("service_tier warnings = %d, want warning %v: %+v", count, tc.wantWarning, resp.Warnings)
			}
			if tc.wantWarning {
				found := false
				for _, warning := range resp.Warnings {
					if warning.Field == "service_tier" && warning.Kind == ir.WarningDropped {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing dropped service_tier warning: %+v", resp.Warnings)
				}
			}
		})
	}
}
