package format

// Batch 6 regressions: every decoded collection has an explicit bound at the
// codec owner, and the proxy/codec limits agree. A Responses input list, a
// reasoning summary aggregate, an Anthropic citation list and Chat nested maps
// all materialize typed values, so each is preflighted before decode.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func repeatJSONArray(item string, n int) string {
	if n <= 0 {
		return "[]"
	}
	return "[" + strings.Repeat(item+",", n-1) + item + "]"
}

func repeatJSONObject(member func(int) string, n int) string {
	if n <= 0 {
		return "{}"
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = member(i)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// TestChatResponseChoiceBeyondFirstNotMaterialized pins B1: only the first
// choice is decoded. A malformed tool_calls value in choice 1 would fail a
// whole-body typed decode, so a successful translation proves it was skipped.
func TestChatResponseChoiceBeyondFirstNotMaterialized(t *testing.T) {
	body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"first"},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":"second","tool_calls":"not-an-array"},"finish_reason":"stop"}]}`
	resp, err := DecodeChatResponse([]byte(body))
	if err != nil {
		t.Fatalf("choice 1 was decoded: %v", err)
	}
	if len(resp.Blocks) == 0 || resp.Blocks[0].Text != "first" {
		t.Fatalf("first choice not translated: %+v", resp.Blocks)
	}
	// The first choice's own tool_calls array is still bounded.
	over := `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant",` +
		`"tool_calls":` + repeatJSONArray(`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}`, metrics.ContentPartMax+1) +
		`}}]}`
	if _, err := DecodeChatResponse([]byte(over)); err == nil || !strings.Contains(err.Error(), "tool_calls") {
		t.Fatalf("first-choice tool_calls over-bound error = %v", err)
	}
}

// TestResponsesSummaryAggregateBound pins B2/H6: summaries share one aggregate
// budget across output items, not only a per-item ceiling.
func TestResponsesSummaryAggregateBound(t *testing.T) {
	item := func(n int) string {
		return `{"type":"reasoning","summary":` + repeatJSONArray(`{"type":"summary_text","text":"s"}`, n) + `}`
	}
	atLimit := 3
	perItem := WireNestedCollectionMax / atLimit
	bodyAt := `{"id":"r1","status":"completed","output":[` + item(perItem) + `,` + item(perItem) + `,` + item(WireNestedCollectionMax-2*perItem) + `]}`
	if _, err := DecodeResponsesResponse([]byte(bodyAt)); err != nil {
		t.Fatalf("aggregate at the limit was rejected: %v", err)
	}
	bodyOver := `{"id":"r1","status":"completed","output":[` + item(WireNestedCollectionMax+1) + `]}`
	if _, err := DecodeResponsesResponse([]byte(bodyOver)); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("aggregate over-bound error = %v", err)
	}
}

// TestAnthropicCitationBound pins the restored per-array citation limit
// (WireNestedCollectionMax) and the shared aggregate on both the request and
// response paths.
func TestAnthropicCitationBound(t *testing.T) {
	citations := func(n int) string {
		return repeatJSONArray(`{"type":"char_location","cited_text":"x"}`, n)
	}
	blocks := func(counts ...int) string {
		var b strings.Builder
		for i, n := range counts {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(`{"type":"text","text":"hi","citations":` + citations(n) + `}`)
		}
		return b.String()
	}
	request := func(content string) string {
		return `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` + content + `]}]}`
	}
	if _, err := DecodeAnthropicRequest([]byte(request(blocks(WireNestedCollectionMax)))); err != nil {
		t.Fatalf("request citations at the per-array limit rejected: %v", err)
	}
	if _, err := DecodeAnthropicRequest([]byte(request(blocks(WireNestedCollectionMax + 1)))); err == nil || !strings.Contains(err.Error(), "citations") {
		t.Fatalf("request citation over-bound error = %v", err)
	}
	if _, err := DecodeAnthropicRequest([]byte(request(blocks(5_000, 5_000)))); err != nil {
		t.Fatalf("request citations at the aggregate limit rejected: %v", err)
	}
	if _, err := DecodeAnthropicRequest([]byte(request(blocks(5_000, 5_001)))); err == nil || !strings.Contains(err.Error(), "citations") {
		t.Fatalf("request citation aggregate over-bound error = %v", err)
	}
	response := func(content string) string {
		return `{"id":"m1","type":"message","role":"assistant","model":"m","content":[` + content + `],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	}
	if _, err := DecodeAnthropicResponse([]byte(response(blocks(WireNestedCollectionMax)))); err != nil {
		t.Fatalf("response citations at the per-array limit rejected: %v", err)
	}
	if _, err := DecodeAnthropicResponse([]byte(response(blocks(WireNestedCollectionMax + 1)))); err == nil || !strings.Contains(err.Error(), "citations") {
		t.Fatalf("response citation over-bound error = %v", err)
	}
	if _, err := DecodeAnthropicResponse([]byte(response(blocks(5_000, 5_000)))); err != nil {
		t.Fatalf("response citations at the aggregate limit rejected: %v", err)
	}
	if _, err := DecodeAnthropicResponse([]byte(response(blocks(5_000, 5_001)))); err == nil || !strings.Contains(err.Error(), "citations") {
		t.Fatalf("response citation aggregate over-bound error = %v", err)
	}
}

func TestAnthropicDocumentCitationsConfigDecoded(t *testing.T) {
	body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":[` +
		`{"type":"document","title":"doc","citations":{"enabled":true},` +
		`"source":{"type":"text","media_type":"text/plain","data":"hello"}}]}]}`
	req, err := DecodeAnthropicRequest([]byte(body))
	if err != nil {
		t.Fatalf("document citations configuration rejected: %v", err)
	}
	if !warningField(req.Warnings, "document.citations") {
		t.Fatalf("missing document.citations drop warning: %+v", req.Warnings)
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if len(block.Citations) > 0 {
				t.Fatalf("document citation configuration became text citations: %+v", block.Citations)
			}
		}
	}
}

// TestResponsesInputAtWireCollectionMax pins A14/H6: the Responses top-level
// input uses the shared WireCollectionMax on both sides of the boundary.
func TestResponsesInputAtWireCollectionMax(t *testing.T) {
	input := func(n int) string {
		return `{"model":"m","input":` + repeatJSONArray(`{"type":"message","role":"user","content":"hi"}`, n) + `}`
	}
	if _, err := DecodeResponsesRequest([]byte(input(WireCollectionMax))); err != nil {
		t.Fatalf("input at the limit rejected: %v", err)
	}
	if _, err := DecodeResponsesRequest([]byte(input(WireCollectionMax + 1))); err == nil || !strings.Contains(err.Error(), "exceed") {
		t.Fatalf("input over-bound error = %v", err)
	}
}

// TestChatNestedCollectionsBounded pins H8: metadata and logit_bias maps and
// the aggregate per-message tool_calls array are bounded at the codec.
func TestChatNestedCollectionsBounded(t *testing.T) {
	toolCalls := func(n int) string {
		return repeatJSONArray(`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}`, n)
	}
	atLimit := `{"model":"m","messages":[{"role":"user","content":"hi","tool_calls":` + toolCalls(WireToolCallsMax) + `}]}`
	if _, err := DecodeChatRequest([]byte(atLimit)); err != nil {
		t.Fatalf("tool calls at the limit rejected: %v", err)
	}
	overTools := `{"model":"m","messages":[{"role":"user","content":"hi","tool_calls":` + toolCalls(WireToolCallsMax+1) + `}]}`
	if _, err := DecodeChatRequest([]byte(overTools)); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("tool calls over-bound error = %v", err)
	}
	metadata := func(n int) string {
		return repeatJSONObject(func(i int) string { return fmt.Sprintf(`"k%d":"v"`, i) }, n)
	}
	if _, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":` + metadata(WireCollectionMax) + `}`)); err != nil {
		t.Fatalf("metadata at the limit rejected: %v", err)
	}
	if _, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":` + metadata(WireCollectionMax+1) + `}`)); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("metadata over-bound error = %v", err)
	}
	logit := func(n int) string {
		return repeatJSONObject(func(i int) string { return fmt.Sprintf(`"%d":0.5`, i) }, n)
	}
	if _, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"logit_bias":` + logit(WireCollectionMax) + `}`)); err != nil {
		t.Fatalf("logit_bias at the limit rejected: %v", err)
	}
	if _, err := DecodeChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"logit_bias":` + logit(WireCollectionMax+1) + `}`)); err == nil || !strings.Contains(err.Error(), "logit_bias") {
		t.Fatalf("logit_bias over-bound error = %v", err)
	}
}

// TestChatModalitiesBounded pins the pre-decode bound for Chat modalities. A
// slice of 10,001 empty strings otherwise expands into a large []string.
func TestChatModalitiesBounded(t *testing.T) {
	modalities := func(n int) string {
		return repeatJSONArray(`"audio"`, n)
	}
	atLimit := `{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":` + modalities(WireCollectionMax) + `}`
	req, err := DecodeChatRequest([]byte(atLimit))
	if err != nil {
		t.Fatalf("modalities at the limit rejected: %v", err)
	}
	if got := warningCount(req.Warnings, ir.WarningDropped, "modalities"); got != 1 {
		t.Fatalf("modalities warnings = %d, want one (%+v)", got, req.Warnings)
	}
	over := `{"model":"m","messages":[{"role":"user","content":"hi"}],"modalities":` + modalities(WireCollectionMax+1) + `}`
	if _, err := DecodeChatRequest([]byte(over)); err == nil || !strings.Contains(err.Error(), "modalities") {
		t.Fatalf("modalities over-bound error = %v", err)
	}
}
