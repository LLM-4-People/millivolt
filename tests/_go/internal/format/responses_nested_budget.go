package format

// Round 2 correction NEW-1 regressions: every Responses content-part array is
// charged to one aggregate budget, including function-call output and the
// response output side, so many per-array-limit lists cannot multiply into an
// unbounded typed decode or warning list.

import (
	"strings"
	"testing"
)

func responsesInputPartsTestBody(items []int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[`)
	for i, parts := range items {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"message","role":"user","content":[`)
		for j := 0; j < parts; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"input_text","text":"x"}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func responsesOutputPartsTestBody(items []int) []byte {
	var b strings.Builder
	b.WriteString(`{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","output":[`)
	for i, parts := range items {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"message","id":"msg_1","role":"assistant","content":[`)
		for j := 0; j < parts; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"output_text","text":"x"}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

func responsesAnnotationTestBody(parts []int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","input":[{"type":"function_call_output","call_id":"c1","output":[`)
	for i, annotations := range parts {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"input_text","text":"hi","annotations":[`)
		for j := 0; j < annotations; j++ {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"url_citation","url":"https://example.com","title":"t","start_index":0,"end_index":1}`)
		}
		b.WriteString(`]}`)
	}
	b.WriteString(`]}]}`)
	return []byte(b.String())
}

func TestResponsesInputContentPartsAggregateBounded(t *testing.T) {
	atLimit := []int{4096, 4096, WireNestedCollectionMax - 2*4096}
	if _, err := DecodeResponsesRequest(responsesInputPartsTestBody(atLimit)); err != nil {
		t.Fatalf("at-limit input rejected: %v", err)
	}
	over := []int{4096, 4096, WireNestedCollectionMax - 2*4096 + 1}
	_, err := DecodeResponsesRequest(responsesInputPartsTestBody(over))
	if err == nil || !strings.Contains(err.Error(), "nested content parts exceed") {
		t.Fatalf("over-limit input error = %v", err)
	}
}

func TestResponsesFunctionCallOutputAnnotationsBounded(t *testing.T) {
	// Per-array limit: one output part carries one annotation past the cap.
	_, err := DecodeResponsesRequest(responsesAnnotationTestBody([]int{4097}))
	if err == nil || !strings.Contains(err.Error(), "annotations exceeds 4096 items") {
		t.Fatalf("over-array output annotations error = %v", err)
	}

	// Aggregate limit: every array is within its per-array cap, but the total
	// exceeds the shared budget.
	over := []int{4096, 4096, WireNestedCollectionMax - 2*4096 + 1}
	_, err = DecodeResponsesRequest(responsesAnnotationTestBody(over))
	if err == nil || !strings.Contains(err.Error(), "annotations exceeds") {
		t.Fatalf("over-aggregate output annotations error = %v", err)
	}

	atLimit := []int{4096, 4096, WireNestedCollectionMax - 2*4096}
	if _, err := DecodeResponsesRequest(responsesAnnotationTestBody(atLimit)); err != nil {
		t.Fatalf("at-limit output annotations rejected: %v", err)
	}
}

func TestResponsesOutputContentPartsAggregateBounded(t *testing.T) {
	atLimit := []int{4096, 4096, WireNestedCollectionMax - 2*4096}
	if _, err := DecodeResponsesResponse(responsesOutputPartsTestBody(atLimit)); err != nil {
		t.Fatalf("at-limit response output rejected: %v", err)
	}
	over := []int{4096, 4096, WireNestedCollectionMax - 2*4096 + 1}
	_, err := DecodeResponsesResponse(responsesOutputPartsTestBody(over))
	if err == nil || !strings.Contains(err.Error(), "nested content parts exceed") {
		t.Fatalf("over-limit response output error = %v", err)
	}
}

// TestResponsesNestedBudgetStopsBeforeMaterializing is the r8i/r8k shape: the
// rejection happens in the lexical aggregate pass, so the typed part and
// citation decode never runs even though the body stays under the request cap.
func TestResponsesNestedBudgetStopsBeforeMaterializing(t *testing.T) {
	const items, parts = 30, 4096
	counts := make([]int, items)
	for i := range counts {
		counts[i] = parts
	}
	body := responsesInputPartsTestBody(counts)
	if len(body) > 32<<20 {
		t.Fatalf("fixture is %d bytes, want under the request cap", len(body))
	}
	_, err := DecodeResponsesRequest(body)
	if err == nil || !strings.Contains(err.Error(), "nested content parts exceed") {
		t.Fatalf("large input error = %v", err)
	}
}

func TestResponsesFunctionCallOutputAnnotationsRetainedBounded(t *testing.T) {
	// A small supported annotation array still reaches the IR.
	body := responsesAnnotationTestBody([]int{2})
	req, err := DecodeResponsesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	content := req.Messages[0].Content
	if len(content) != 1 || content[0].ToolResult == nil || len(content[0].ToolResult.Content) != 1 {
		t.Fatalf("tool result content = %+v", content)
	}
	if got := len(content[0].ToolResult.Content[0].Citations); got != 2 {
		t.Fatalf("citations = %d, want 2", got)
	}
}
