package format

// Round 12 H1 regression: a Responses url_citation span is message-relative on
// both encode and decode. The encoder used to write the block-relative IR span
// unchanged, so a multi-part source lost its citation on the next hop.

import (
	"encoding/json"
	"strings"
	"testing"
)

func responsesWireSpans(t *testing.T, body string) map[string][][2]int {
	t.Helper()
	var doc responsesAnnotationDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
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

func chatWireSpans(t *testing.T, body string) (string, [][2]int) {
	t.Helper()
	var doc struct {
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
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(doc.Choices))
	}
	var spans [][2]int
	for _, a := range doc.Choices[0].Message.Annotations {
		spans = append(spans, [2]int{a.URLCitation.StartIndex, a.URLCitation.EndIndex})
	}
	return doc.Choices[0].Message.Content, spans
}

func TestResponsesCitationSpansTwoPartRoundTrip(t *testing.T) {
	annotation := `{"type":"url_citation","url":"https://e","title":"t","start_index":5,"end_index":6}`
	body := responsesCitationsBody(
		`{"type":"output_text","text":"foo"},` +
			`{"type":"output_text","text":"bar","annotations":[` + annotation + `]}`)

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

	out, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if got := responsesWireSpans(t, string(out))["bar"]; len(got) != 1 || got[0] != [2]int{5, 6} {
		t.Fatalf("encoded Responses spans = %v, want message-relative bar 5..6", got)
	}
	if warningField(resp.Warnings, "annotations.span") {
		t.Fatalf("valid encode warned: %+v", resp.Warnings)
	}

	// The proxy re-decodes the same wire on the next hop.
	again, err := DecodeResponsesResponse(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Blocks) != 2 || len(again.Blocks[1].Citations) != 1 {
		t.Fatalf("round-trip lost the citation: %+v (%+v)", again.Blocks, again.Warnings)
	}
	if c := again.Blocks[1].Citations[0]; c.StartIndex != 2 || c.EndIndex != 3 {
		t.Fatalf("round-trip IR span = %d..%d, want 2..3", c.StartIndex, c.EndIndex)
	}

	// Single part keeps message and block offsets identical.
	single := responsesCitationsBody(
		`{"type":"output_text","text":"foobar","annotations":[{"type":"url_citation","url":"https://e","title":"t","start_index":0,"end_index":2}]}`)
	singleResp, err := DecodeResponsesResponse([]byte(single))
	if err != nil {
		t.Fatal(err)
	}
	singleOut, err := EncodeResponsesResponse(singleResp)
	if err != nil {
		t.Fatal(err)
	}
	if got := responsesWireSpans(t, string(singleOut))["foobar"]; len(got) != 1 || got[0] != [2]int{0, 2} {
		t.Fatalf("single-part span = %v, want 0..2", got)
	}

	// A span outside its own part still warns and drops on decode.
	outOfPart := responsesCitationsBody(
		`{"type":"output_text","text":"foo","annotations":[` + annotation + `]},` +
			`{"type":"output_text","text":"bar"}`)
	dropped, err := DecodeResponsesResponse([]byte(outOfPart))
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range dropped.Blocks {
		if len(block.Citations) != 0 {
			t.Fatalf("out-of-part span survived: %+v", dropped.Blocks)
		}
	}
	if !warningField(dropped.Warnings, "annotations.span") {
		t.Fatalf("out-of-part span did not warn: %+v", dropped.Warnings)
	}
}

func TestChatResponsesChatCitationRoundTrip(t *testing.T) {
	parts := `[{"type":"text","text":"foo"},{"type":"text","text":"bar"}]`
	chat, err := DecodeChatResponse([]byte(chatAnnotationBody(parts, "5", "6")))
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Blocks) != 2 || len(chat.Blocks[1].Citations) != 1 {
		t.Fatalf("Chat IR = %+v, want the citation on bar", chat.Blocks)
	}
	if c := chat.Blocks[1].Citations[0]; c.StartIndex != 2 || c.EndIndex != 3 {
		t.Fatalf("Chat IR span = %d..%d, want 2..3", c.StartIndex, c.EndIndex)
	}

	responses, err := EncodeResponsesResponse(chat)
	if err != nil {
		t.Fatal(err)
	}
	responsesSpans := responsesWireSpans(t, string(responses))
	if got := responsesSpans["bar"]; len(got) != 1 || got[0] != [2]int{5, 6} {
		t.Fatalf("first hop Responses spans = %v, want bar 5..6", responsesSpans)
	}

	again, err := DecodeResponsesResponse(responses)
	if err != nil {
		t.Fatal(err)
	}
	chatOut, err := EncodeChatResponse(again)
	if err != nil {
		t.Fatal(err)
	}
	content, spans := chatWireSpans(t, string(chatOut))
	if content != "foobar" {
		t.Fatalf("second hop Chat content = %q, want foobar", content)
	}
	if len(spans) != 1 || spans[0] != [2]int{5, 6} {
		t.Fatalf("second hop Chat spans = %v, want 5..6 (%s)", spans, strings.TrimSpace(string(chatOut)))
	}
}
