package format

// Round 5 B3 regression: an Anthropic web_search_result_location carries a URL
// but no character span. Chat and Responses require both span fields, so the
// 0..0 range must be reported as an approximation while the annotation is kept.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func citationResponse(c ir.Citation) *ir.Response {
	return &ir.Response{
		ID: "resp_1", Model: "m", Finish: ir.FinishStop,
		Blocks: []ir.Block{{Type: ir.BlockText, Text: "answer", Citations: []ir.Citation{c}}},
	}
}

func TestAnthropicCitationSpanIsReportedApproximated(t *testing.T) {
	web := ir.Citation{Type: "web_search_result_location", URL: "https://example.com", Title: "Example"}
	native := ir.Citation{Type: "url_citation", URL: "https://example.com", Title: "Example", StartIndex: 1, EndIndex: 4}
	noURL := ir.Citation{Type: "char_location", CitedText: "x", StartIndex: 0, EndIndex: 1}

	t.Run("chat web location", func(t *testing.T) {
		resp := citationResponse(web)
		body, err := EncodeChatResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Choices []struct {
				Message struct {
					Annotations []struct {
						Type        string `json:"type"`
						URLCitation struct {
							Start int `json:"start_index"`
							End   int `json:"end_index"`
						} `json:"url_citation"`
					} `json:"annotations"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Choices) != 1 || len(doc.Choices[0].Message.Annotations) != 1 ||
			doc.Choices[0].Message.Annotations[0].URLCitation.Start != 0 ||
			doc.Choices[0].Message.Annotations[0].URLCitation.End != 0 {
			t.Fatalf("chat annotation lost or span changed: %s", body)
		}
		if warningCount(resp.Warnings, ir.WarningApproximated, "citations") != 1 {
			t.Fatalf("chat span approximation not reported: %+v", resp.Warnings)
		}
	})

	t.Run("responses web location", func(t *testing.T) {
		resp := citationResponse(web)
		body, err := EncodeResponsesResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Output []struct {
				Content []struct {
					Annotations []struct {
						Type  string `json:"type"`
						Start int    `json:"start_index"`
						End   int    `json:"end_index"`
					} `json:"annotations"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Output) != 1 || len(doc.Output[0].Content) != 1 ||
			len(doc.Output[0].Content[0].Annotations) != 1 {
			t.Fatalf("responses annotation lost: %s", body)
		}
		ann := doc.Output[0].Content[0].Annotations[0]
		if ann.Type != "url_citation" || ann.Start != 0 || ann.End != 0 {
			t.Fatalf("responses annotation span/type = %s/%d..%d, want url_citation/0..0: %s",
				ann.Type, ann.Start, ann.End, body)
		}
		if warningCount(resp.Warnings, ir.WarningApproximated, "citations") != 1 {
			t.Fatalf("responses span approximation not reported: %+v", resp.Warnings)
		}
	})

	// A zero-span citation on a later text part is emitted at that part's start
	// offset, so the approximation warning must name the same span.
	t.Run("responses zero span later part", func(t *testing.T) {
		resp := &ir.Response{
			ID: "resp_1", Model: "m", Finish: ir.FinishStop,
			Blocks: []ir.Block{
				{Type: ir.BlockText, Text: "foo"},
				{Type: ir.BlockText, Text: "bar", Citations: []ir.Citation{web}},
			},
		}
		body, err := EncodeResponsesResponse(resp)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Output []struct {
				Content []struct {
					Text        string `json:"text"`
					Annotations []struct {
						Start int `json:"start_index"`
						End   int `json:"end_index"`
					} `json:"annotations"`
				} `json:"content"`
			} `json:"output"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatal(err)
		}
		if len(doc.Output) != 1 || len(doc.Output[0].Content) != 2 {
			t.Fatalf("responses parts = %s", body)
		}
		part := doc.Output[0].Content[1]
		if part.Text != "bar" || len(part.Annotations) != 1 ||
			part.Annotations[0].Start != 3 || part.Annotations[0].End != 3 {
			t.Fatalf("later-part annotation = %+v, want bar 3..3: %s", part, body)
		}
		want := "source citation has no character span; target span is 3..3"
		if warningCount(resp.Warnings, ir.WarningApproximated, "citations") != 1 {
			t.Fatalf("later-part span approximation not reported: %+v", resp.Warnings)
		}
		found := false
		for _, warning := range resp.Warnings {
			if warning.Kind == ir.WarningApproximated && warning.Field == "citations" && warning.Detail == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("span warning = %+v, want %q", resp.Warnings, want)
		}
	})

	for _, tc := range []struct {
		name string
		c    ir.Citation
		kind ir.WarningKind
	}{
		{name: "native url citation", c: native},
		{name: "no url", c: noURL, kind: ir.WarningDropped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chat := citationResponse(tc.c)
			if _, err := EncodeChatResponse(chat); err != nil {
				t.Fatal(err)
			}
			responses := citationResponse(tc.c)
			if _, err := EncodeResponsesResponse(responses); err != nil {
				t.Fatal(err)
			}
			for label, warnings := range map[string]ir.Warnings{"chat": chat.Warnings, "responses": responses.Warnings} {
				if warningCount(warnings, ir.WarningApproximated, "citations") != 0 {
					t.Fatalf("%s approximated a real or spanless-without-url citation: %+v", label, warnings)
				}
				want := 0
				if tc.kind != "" {
					want = 1
				}
				if warningCount(warnings, tc.kind, "citations") != want {
					t.Fatalf("%s warning count = %+v, want %d %s", label, warnings, want, tc.kind)
				}
			}
		})
	}
}
