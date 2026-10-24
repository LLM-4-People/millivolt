package format

// Round 34 NEW-3 regression: a Chat message-level url_citation span is relative
// to the whole message, while an IR citation is relative to its own text block.
// The decoder used to append every citation to the last text block unchanged,
// so a multi-part message produced out-of-range spans and a negative span was
// accepted.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatAnnotationBody(content, start, end string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":` + content + `,` +
		`"annotations":[{"type":"url_citation","url_citation":{"url":"https://e","title":"t",` +
		`"start_index":` + start + `,"end_index":` + end + `}}]},` +
		`"finish_reason":"stop"}]}`
}

func decodeChatIR(t *testing.T, body string) (*ir.Response, ir.Warnings) {
	t.Helper()
	resp, err := DecodeChatResponse([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp, resp.Warnings
}

type responsesAnnotationDoc struct {
	Output []struct {
		Content []struct {
			Text        string `json:"text"`
			Annotations []struct {
				Type       string `json:"type"`
				StartIndex int    `json:"start_index"`
				EndIndex   int    `json:"end_index"`
			} `json:"annotations"`
		} `json:"content"`
	} `json:"output"`
}

func chatToResponsesAnnotations(t *testing.T, body string) map[string][][2]int {
	t.Helper()
	out, _, err := TranslateResponseBetween(ir.Responses, ir.Chat, []byte(body))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var doc responsesAnnotationDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	spans := map[string][][2]int{}
	for _, item := range doc.Output {
		for _, part := range item.Content {
			for _, a := range part.Annotations {
				spans[part.Text] = append(spans[part.Text], [2]int{a.StartIndex, a.EndIndex})
			}
		}
	}
	return spans
}

func hasChatSpanWarning(w ir.Warnings) bool {
	for _, warning := range w {
		if warning.Field == "annotations.span" {
			return true
		}
	}
	return false
}

func TestChatCitationMessageRelativeSpans(t *testing.T) {
	parts := `[{"type":"text","text":"foo"},{"type":"text","text":"bar"}]`

	resp, warns := decodeChatIR(t, chatAnnotationBody(parts, "5", "6"))
	if len(resp.Blocks) != 2 || len(resp.Blocks[1].Citations) != 1 {
		t.Fatalf("IR blocks = %+v, want the citation on the second part", resp.Blocks)
	}
	if c := resp.Blocks[1].Citations[0]; c.StartIndex != 2 || c.EndIndex != 3 {
		t.Fatalf("IR span = %d..%d, want block-relative 2..3", c.StartIndex, c.EndIndex)
	}
	if hasChatSpanWarning(warns) {
		t.Fatalf("valid span warned: %+v", warns)
	}
	spans := chatToResponsesAnnotations(t, chatAnnotationBody(parts, "5", "6"))
	if got := spans["bar"]; len(got) != 1 || got[0] != [2]int{5, 6} {
		t.Fatalf("Responses spans = %v, want the message-relative bar 5..6", spans)
	}
	if len(spans["foo"]) != 0 {
		t.Fatalf("citation attached to the wrong part: %v", spans)
	}

	// A single string content keeps the message and block offsets identical.
	single := chatAnnotationBody(`"foobar"`, "0", "2")
	resp, _ = decodeChatIR(t, single)
	if len(resp.Blocks) != 1 || len(resp.Blocks[0].Citations) != 1 {
		t.Fatalf("single-part IR blocks = %+v, want one citation", resp.Blocks)
	}
	if c := resp.Blocks[0].Citations[0]; c.StartIndex != 0 || c.EndIndex != 2 {
		t.Fatalf("single-part IR span = %d..%d, want 0..2", c.StartIndex, c.EndIndex)
	}
	spans = chatToResponsesAnnotations(t, single)
	if got := spans["foobar"]; len(got) != 1 || got[0] != [2]int{0, 2} {
		t.Fatalf("single-part Responses spans = %v, want 0..2", spans)
	}

	for _, tc := range []struct {
		name  string
		start string
		end   string
		want  string
	}{
		{name: "negative start", start: "-1", end: "5", want: "outside the message text"},
		{name: "end before start", start: "5", end: "4", want: "outside the message text"},
		{name: "end past the message", start: "5", end: "7", want: "outside the message text"},
		{name: "crosses a part boundary", start: "2", end: "4", want: "crosses a text block boundary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := chatAnnotationBody(parts, tc.start, tc.end)
			resp, warns := decodeChatIR(t, body)
			for _, block := range resp.Blocks {
				if len(block.Citations) != 0 {
					t.Fatalf("invalid span attached: %+v", resp.Blocks)
				}
			}
			found := false
			for _, warning := range warns {
				if warning.Field == "annotations.span" && strings.Contains(warning.Detail, tc.want) {
					found = true
				}
			}
			if !found {
				t.Fatalf("warning %q missing: %+v", tc.want, warns)
			}
			// The translation still succeeds and emits no out-of-range span.
			spans := chatToResponsesAnnotations(t, body)
			if len(spans) != 0 {
				t.Fatalf("invalid span reached the Responses wire: %v", spans)
			}
		})
	}
}
