package format

// Round 2 correction NEW-2 regressions: a content_part.done payload whose part
// is reasoning_text must stay on the reasoning channel, whether the part was
// deferred, partially streamed or type-less.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesDoneReasoningStream(addedPart, donePart string) string {
	added := `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

`
	if addedPart != "" {
		added += `event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":` + addedPart + `}

`
	}
	return terminalStreamCreated + added + `event: response.content_part.done
data: {"type":"response.content_part.done","output_index":0,"content_index":0,"part":` + donePart + `}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
}

func TestResponsesDoneReasoningTextStaysHidden(t *testing.T) {
	cases := []struct {
		name      string
		addedPart string
		donePart  string
		want      string
	}{
		{
			name:      "deferred part",
			addedPart: `{"type":"reasoning_text","text":""}`,
			donePart:  `{"type":"reasoning_text","text":"deep"}`,
			want:      "deep",
		},
		{
			name:      "partially streamed part",
			addedPart: `{"type":"reasoning_text","text":"de"}`,
			donePart:  `{"type":"reasoning_text","text":"deep"}`,
			want:      "deep",
		},
		{
			name:      "type-less done reuses the announced channel",
			addedPart: `{"type":"reasoning_text","text":""}`,
			donePart:  `{"text":"deep"}`,
			want:      "deep",
		},
		{
			name:     "no added part",
			donePart: `{"type":"reasoning_text","text":"deep"}`,
			want:     "deep",
		},
	}
	for _, tc := range cases {
		for _, target := range terminalTargets {
			t.Run(tc.name+"/"+target.name, func(t *testing.T) {
				var buf bytes.Buffer
				if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(
					responsesDoneReasoningStream(tc.addedPart, tc.donePart)), nil, nil, StreamOptions{}); err != nil {
					t.Fatal(err)
				}
				out := buf.String()
				if got := strings.Join(reasoningEmissionOrder(t, target.inbound, out), ""); got != tc.want {
					t.Fatalf("reasoning = %q, want %q: %s", got, tc.want, out)
				}
				if got := textDeltaCount(t, target.inbound, out, tc.want); got != 0 {
					t.Fatalf("reasoning value leaked into visible text %d times: %s", got, out)
				}
				if target.inbound == ir.Responses {
					for _, payload := range mustResponsePayloads(t, out) {
						if payload["type"] != "response.output_item.added" {
							continue
						}
						item, _ := payload["item"].(map[string]any)
						if item["type"] == "message" {
							t.Fatalf("reasoning done payload created a visible message item: %s", out)
						}
					}
				}
			})
		}
	}
}

func TestResponsesDoneReasoningTextEmitsOncePerChannel(t *testing.T) {
	source := responsesDoneReasoningStream(`{"type":"reasoning_text","text":""}`, `{"type":"reasoning_text","text":"deep"}`)
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Anthropic, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := reasoningDeltaCount(t, ir.Anthropic, buf.String(), "deep"); got != 1 {
		t.Fatalf("thinking deltas = %d, want 1: %s", got, buf.String())
	}
	if strings.Contains(buf.String(), `"type":"text_delta"`) {
		t.Fatalf("Anthropic stream mixed a text delta into the thinking channel: %s", buf.String())
	}
}
