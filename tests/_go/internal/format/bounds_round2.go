package format

// Round 2 DUP-1/TC1-2 regressions: every nested annotation, citation and
// content array is bounded at the codec before its typed decode, and the
// arrays share one aggregate budget.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func urlAnnotations(n int) string {
	return repeatJSONArray(`{"type":"url_citation","url_citation":{"url":"https://example.com","title":"t","start_index":0,"end_index":1}}`, n)
}

func charCitations(n int) string {
	return repeatJSONArray(`{"type":"char_location","cited_text":"x"}`, n)
}

func chatMessagesWithAnnotations(counts ...int) string {
	msgs := make([]string, len(counts))
	for i, n := range counts {
		msgs[i] = `{"role":"assistant","content":"hi","annotations":` + urlAnnotations(n) + `}`
	}
	return "[" + strings.Join(msgs, ",") + "]"
}

func TestChatResponseAnnotationsBounded(t *testing.T) {
	body := func(n int) string {
		return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"hi","annotations":` + urlAnnotations(n) + `},"finish_reason":"stop"}]}`
	}
	if _, err := DecodeChatResponse([]byte(body(metrics.ContentPartMax))); err != nil {
		t.Fatalf("annotations at the per-array limit rejected: %v", err)
	}
	if _, err := DecodeChatResponse([]byte(body(metrics.ContentPartMax + 1))); err == nil || !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("annotations over the per-array limit error = %v", err)
	}
}

func TestChatRequestAnnotationsAggregateBounded(t *testing.T) {
	body := func(counts ...int) string {
		return `{"model":"m","messages":` + chatMessagesWithAnnotations(counts...) + `}`
	}
	atLimit := []int{metrics.ContentPartMax - 96, metrics.ContentPartMax - 96, 2000}
	if _, err := DecodeChatRequest([]byte(body(atLimit...))); err != nil {
		t.Fatalf("annotations at the aggregate limit rejected: %v", err)
	}
	over := []int{metrics.ContentPartMax, metrics.ContentPartMax, 2000}
	if _, err := DecodeChatRequest([]byte(body(over...))); err == nil || !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("annotations over the aggregate limit error = %v", err)
	}
}

func responsesMessageWithAnnotations(n int) string {
	return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":` + urlAnnotations(n) + `}]}`
}

func TestResponsesInputAnnotationsAggregateBounded(t *testing.T) {
	body := func(coverage ...int) string {
		items := make([]string, len(coverage))
		for i, n := range coverage {
			items[i] = responsesMessageWithAnnotations(n)
		}
		return `{"model":"m","input":[` + strings.Join(items, ",") + `]}`
	}
	if _, err := DecodeResponsesRequest([]byte(body(4000, 4000, 2000))); err != nil {
		t.Fatalf("input annotations at the aggregate limit rejected: %v", err)
	}
	if _, err := DecodeResponsesRequest([]byte(body(metrics.ContentPartMax, metrics.ContentPartMax, metrics.ContentPartMax))); err == nil || !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("input annotations over the aggregate limit error = %v", err)
	}
}

func TestResponsesOutputAnnotationsAggregateBounded(t *testing.T) {
	body := func(coverage ...int) string {
		items := make([]string, len(coverage))
		for i, n := range coverage {
			items[i] = responsesMessageWithAnnotations(n)
		}
		return `{"id":"r1","status":"completed","output":[` + strings.Join(items, ",") + `]}`
	}
	if _, err := DecodeResponsesResponse([]byte(body(4000, 4000, 2000))); err != nil {
		t.Fatalf("output annotations at the aggregate limit rejected: %v", err)
	}
	if _, err := DecodeResponsesResponse([]byte(body(metrics.ContentPartMax, metrics.ContentPartMax, metrics.ContentPartMax))); err == nil || !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("output annotations over the aggregate limit error = %v", err)
	}
}

func anthropicSystem(citations int) string {
	return `{"model":"m","max_tokens":8,"system":[{"type":"text","text":"sys","citations":` + charCitations(citations) + `}],` +
		`"messages":[{"role":"user","content":"hi"}]}`
}

func TestAnthropicSystemCitationsBoundedAndPreserved(t *testing.T) {
	req, err := DecodeAnthropicRequest([]byte(anthropicSystem(2)))
	if err != nil {
		t.Fatalf("small system citations rejected: %v", err)
	}
	system := req.Messages[0]
	if len(system.Content) != 1 || len(system.Content[0].Citations) != 2 {
		t.Fatalf("system citations were not preserved: %+v", system.Content)
	}
	if _, err := DecodeAnthropicRequest([]byte(anthropicSystem(WireNestedCollectionMax + 1))); err == nil || !strings.Contains(err.Error(), "citations") {
		t.Fatalf("system citations over the per-array limit error = %v", err)
	}
}

func anthropicMessagesWithNested(counts ...int) string {
	msgs := make([]string, len(counts))
	for i, n := range counts {
		content := repeatJSONArray(`{"type":"text","text":"x"}`, n)
		msgs[i] = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":` + content + `}]}`
	}
	return "[" + strings.Join(msgs, ",") + "]"
}

func TestAnthropicNestedContentAggregateBounded(t *testing.T) {
	atLimit := `{"model":"m","max_tokens":8,"messages":` + anthropicMessagesWithNested(4000, 4000, 2000) + `}`
	if _, err := DecodeAnthropicRequest([]byte(atLimit)); err != nil {
		t.Fatalf("nested content at the aggregate limit rejected: %v", err)
	}
	over := `{"model":"m","max_tokens":8,"messages":` + anthropicMessagesWithNested(metrics.ContentPartMax, metrics.ContentPartMax, metrics.ContentPartMax) + `}`
	if _, err := DecodeAnthropicRequest([]byte(over)); err == nil || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested content over the aggregate limit error = %v", err)
	}
}
