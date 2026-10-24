package format

// Round 11 B1 regression: a Responses url_citation span is message-relative,
// while the IR citation is relative to its own text block. The decoder attached
// the source span unchanged, so a multi-part message shifted every Chat span by
// the preceding text.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

type responsesCitationChatDoc struct {
	Choices []struct {
		Message struct {
			Content     string `json:"content"`
			Annotations []struct {
				URLCitation struct {
					StartIndex int `json:"start_index"`
					EndIndex   int `json:"end_index"`
				} `json:"url_citation"`
			} `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
}

func responsesCitationsBody(parts string) string {
	return `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[` + parts + `]}]}`
}

func chatCitationSpans(t *testing.T, body string) (string, []int, ir.Warnings) {
	t.Helper()
	out, warns, err := TranslateResponseBetween(ir.Chat, ir.Responses, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var doc responsesCitationChatDoc
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Choices) == 0 {
		t.Fatalf("no choice: %s", out)
	}
	var spans []int
	for _, a := range doc.Choices[0].Message.Annotations {
		spans = append(spans, a.URLCitation.StartIndex, a.URLCitation.EndIndex)
	}
	return doc.Choices[0].Message.Content, spans, warns
}

func TestResponsesCitationSpanMessageRelative(t *testing.T) {
	annotation := `{"type":"url_citation","url":"https://e","title":"t","start_index":5,"end_index":6}`

	body := responsesCitationsBody(
		`{"type":"output_text","text":"foo"},` +
			`{"type":"output_text","text":"bar","annotations":[` + annotation + `]}`)
	// The IR citation is block-relative: 5..6 in "foobar" is 2..3 in "bar".
	resp, err := DecodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 2 || len(resp.Blocks[1].Citations) != 1 {
		t.Fatalf("IR blocks = %+v, want the citation on the second part", resp.Blocks)
	}
	if c := resp.Blocks[1].Citations[0]; c.StartIndex != 2 || c.EndIndex != 3 {
		t.Fatalf("IR span = %d..%d, want block-relative 2..3", c.StartIndex, c.EndIndex)
	}
	// The Chat wire span is message-relative again: the encoder adds the
	// preceding text back, so 5..6 addresses the same characters in "foobar".
	content, spans, _ := chatCitationSpans(t, body)
	if content != "foobar" {
		t.Fatalf("content = %q, want foobar", content)
	}
	if len(spans) != 2 || spans[0] != 5 || spans[1] != 6 {
		t.Fatalf("two-part Chat span = %v, want 5..6", spans)
	}

	// A single part makes the message and block offsets coincide.
	content, spans, _ = chatCitationSpans(t, responsesCitationsBody(
		`{"type":"output_text","text":"foobar","annotations":[{"type":"url_citation","url":"https://e","title":"t","start_index":0,"end_index":2}]}`))
	if content != "foobar" || len(spans) != 2 || spans[0] != 0 || spans[1] != 2 {
		t.Fatalf("single-part control = %q %v, want foobar 0..2", content, spans)
	}

	// A span inside the next part but attached to this one is outside its own
	// block after subtraction: drop it with a warning instead of emitting a
	// negative or out-of-range span.
	_, spans, warns := chatCitationSpans(t, responsesCitationsBody(
		`{"type":"output_text","text":"foo","annotations":[`+annotation+`]},`+
			`{"type":"output_text","text":"bar"}`))
	if len(spans) != 0 {
		t.Fatalf("out-of-part span survived: %v", spans)
	}
	found := false
	for _, w := range warns {
		if w.Field == "annotations.span" {
			found = true
		}
	}
	if !found {
		t.Fatalf("out-of-part span did not warn: %+v", warns)
	}
	if _, _, err := TranslateResponseBetween(ir.Chat, ir.Responses, []byte(responsesCitationsBody(
		`{"type":"output_text","text":"foo"},{"type":"output_text","text":"bar"}`))); err != nil {
		t.Fatalf("warn path failed the translation: %v", err)
	}
}
