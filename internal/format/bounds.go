package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Wire decoders materialize JSON arrays into typed slices, so one compact
// array of empty objects can expand into a large allocation. Every codec
// preflights the arrays it decodes with the shared lexical counter, which
// stops at the first member past the limit and never materializes a part.

// WireCollectionMax is the shared ceiling for a decoded top-level collection
// (messages, tools, input items). It matches the proxy's request preflight so
// a translation never rejects a document the passthrough path accepts; a
// single content-part array keeps the smaller ContentPartMax.
const WireCollectionMax = 10_000

// toolBlocksMax bounds the concurrent tool blocks a native stream decoder
// tracks while translating one response. Each open block may retain its
// initial JSON until its stop; real parallel tool calls are single-digit, so
// 1024 leaves ample headroom while failing a stream that opens unique blocks
// without ever closing them. Internal safety guardrail, not user-tunable.
const toolBlocksMax = 1024

// toolInitialBytesMax bounds the total initial tool JSON retained by the
// currently open tool blocks of one native stream. Each start may carry up to
// one SSE line, so the block count alone would allow a gigabyte of retained
// strings; normal starts carry {} or a small inline object and large arguments
// stream through deltas. Internal safety guardrail, not user-tunable.
const toolInitialBytesMax = 4 << 20

// streamToolArgumentsMax bounds the total tool-argument bytes a stream decoder
// accumulates to validate the completed JSON object. Arguments are forwarded
// as they arrive, so this is a validation buffer, not a wire cap: normal tool
// calls are measured in kilobytes, and a source that streams megabytes into a
// single call without finishing it fails closed before a clean terminal.
const streamToolArgumentsMax = 4 << 20

// toolArgumentsCanonicalBytesMax bounds one argument value whose JSON is
// canonicalized for an id-less repeat comparison. Canonicalization decodes and
// re-marshals the value, so a larger provider-controlled value would multiply
// transient memory even though it is already capped at streamToolArgumentsMax;
// the fingerprint then falls back to the raw-byte hash. Normal tool arguments
// are kilobytes. Internal safety guardrail, not user-tunable.
const toolArgumentsCanonicalBytesMax = 1 << 20

// toolArgumentsCanonicalSeparatorsMax bounds the aggregate array commas and
// object colons in one canonicalized value. It is a conservative proxy for the
// decoded member count: a compact array of hundreds of thousands of scalars
// expands into a generic tree during the canonical decode, while a value within
// this budget stays bounded. Internal safety guardrail, not user-tunable.
const toolArgumentsCanonicalSeparatorsMax = WireCollectionMax

// toolArgumentsCanonicalDepthMax bounds bracket nesting in one canonicalized
// value. Tool-argument schemas are shallow, and the generic decode recurses;
// a deeper value falls back to the raw-byte hash. Internal safety guardrail,
// not user-tunable.
const toolArgumentsCanonicalDepthMax = 64

// toolArgumentsWithinCanonicalBounds reports whether a complete JSON object
// may be decoded and re-marshaled for canonical hashing. The scan is lexical:
// it skips strings, counts array commas and object colons, tracks bracket
// depth, and stops at the first proof of overflow, so an over-bound value costs
// only a prefix pass and no decoded tree. Malformed, partial, non-object and
// over-bound values return false; the caller hashes their raw bytes.
func toolArgumentsWithinCanonicalBounds(data []byte) bool {
	if len(data) > toolArgumentsCanonicalBytesMax {
		return false
	}
	i := metrics.SkipSpace(data, 0)
	if i >= len(data) || data[i] != '{' {
		return false
	}
	depth := 0
	separators := 0
	for i < len(data) {
		switch data[i] {
		case '"':
			next := metrics.SkipJSONString(data, i)
			if next < 0 {
				return false
			}
			i = next
			continue
		case '{', '[':
			depth++
			if depth > toolArgumentsCanonicalDepthMax {
				return false
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return false
			}
			if depth == 0 {
				return metrics.SkipSpace(data, i+1) == len(data)
			}
		case ',', ':':
			separators++
			if separators > toolArgumentsCanonicalSeparatorsMax {
				return false
			}
		}
		i++
	}
	return false
}

// WireNestedCollectionMax bounds an aggregate of nested collections that a
// single top-level bound cannot cover, such as every reasoning summary array in
// an output list or every citation array in a content list.
const WireNestedCollectionMax = 10_000

// WireContentPartsMax bounds the aggregate content-part members over every
// message content array, the top-level system content and every nested
// Responses input content/output array. It is the proxy's request content-part
// ceiling too, so the preflight admits exactly the translated documents the
// codec accepts. The per-array metrics.ContentPartMax keeps one list bounded;
// this aggregate stops many lists at that cap from multiplying.
const WireContentPartsMax = 16_384

// WireToolCallsMax bounds the aggregate tool-call members over every message.
// The proxy's request preflight and the Chat codec share this one value so the
// translated wire cannot accept a document the passthrough path rejects.
const WireToolCallsMax = 1024

// boundedEchoMax bounds a body-controlled value echoed in an error or
// warning. A request-sized value must never become a request-sized
// diagnostic.
const boundedEchoMax = 128

// boundedEcho truncates value to boundedEchoMax bytes on a rune boundary and
// marks the cut.
func boundedEcho(value string) string {
	if len(value) <= boundedEchoMax {
		return value
	}
	cut := boundedEchoMax
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "..."
}

// checkWireArray rejects an over-bound JSON array before its typed decode.
// Absent, null and non-array values pass: the typed decode owns their shape
// errors. A malformed array is reported as over-bound here, matching every
// other caller of the shared counter; a document that does not even parse
// fails the probe unmarshal first and reports its syntax error.
func checkWireArray(raw json.RawMessage, field string, max int) error {
	if len(raw) == 0 || metrics.ArrayWithinLimit(raw, max) {
		return nil
	}
	return fmt.Errorf("%s exceeds %d items", field, max)
}

// addWireArray counts one nested array into an aggregate budget. Absent and
// null values contribute nothing; a malformed array fails closed.
func addWireArray(total *int, raw json.RawMessage, field string, max int) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	n, _, ok := metrics.CountJSONArrayBytes(raw, 0, max+1)
	if !ok {
		return fmt.Errorf("%s is malformed", field)
	}
	*total += n
	if *total > max {
		return fmt.Errorf("%s exceeds %d items", field, max)
	}
	return nil
}

// checkWireObject rejects an over-bound JSON object before its typed map
// decode. Absent, null and non-object values pass.
func checkWireObject(raw json.RawMessage, field string, max int) error {
	if len(raw) == 0 || metrics.ObjectWithinLimit(raw, max) {
		return nil
	}
	return fmt.Errorf("%s exceeds %d members", field, max)
}

// anthropicNestedDepthMax bounds the nesting of content arrays inside one
// Anthropic block. The schema's deepest representable path is a tool_result
// whose document source carries content (roughly three levels), so 16 is
// generous. The bound is required rather than cosmetic: the preflight
// re-unmarshals the remaining subtree at every level and the typed decode
// re-walks each nested content array, so an unbounded chain is cubic in the
// request body and can exhaust CPU and heap at the JSON depth limit.
const anthropicNestedDepthMax = 16

// checkAnthropicCitations bounds every citations array and nested content
// array in one Anthropic content-block list before the typed decode
// materializes them. A block list is already bounded by ContentPartMax, but
// one text block can carry an unbounded citation list and nested
// tool_result/document content can multiply block arrays.
func checkAnthropicCitations(content json.RawMessage, field string) error {
	budget := anthropicNestedBudget{}
	return checkAnthropicBlockList(content, field, 0, &budget)
}

// anthropicNestedBudget accumulates the citation members and nested content
// blocks of one Anthropic request or response. Top-level block lists keep
// their per-array ContentPartMax; the aggregate covers the arrays those
// elements can nest, which one bounded array cannot.
type anthropicNestedBudget struct {
	citations int
	blocks    int
}

// checkAnthropicBlockList walks one Anthropic content array without
// materializing its typed form. Every citations array keeps
// WireNestedCollectionMax and every nested content array contributes to one
// WireNestedCollectionMax aggregate. depth 0 is the caller's own block list;
// deeper arrays are the nested tool_result/document content that can multiply
// through their parents.
func checkAnthropicBlockList(raw json.RawMessage, field string, depth int, budget *anthropicNestedBudget) error {
	if depth > anthropicNestedDepthMax {
		return fmt.Errorf("%s exceeds %d nested content levels", field, anthropicNestedDepthMax)
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	if !metrics.ArrayWithinLimit(trimmed, metrics.ContentPartMax) {
		return fmt.Errorf("%s exceeds %d blocks", field, metrics.ContentPartMax)
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return fmt.Errorf("parse %s: %w", field, err)
	}
	if depth > 0 {
		budget.blocks += len(blocks)
		if budget.blocks > WireNestedCollectionMax {
			return fmt.Errorf("%s exceeds %d nested blocks", field, WireNestedCollectionMax)
		}
	}
	for i, rawBlock := range blocks {
		var block struct {
			Citations json.RawMessage `json:"citations"`
			Content   json.RawMessage `json:"content"`
			Source    *struct {
				Content json.RawMessage `json:"content"`
			} `json:"source"`
		}
		if err := json.Unmarshal(rawBlock, &block); err != nil {
			return fmt.Errorf("parse %s block %d: %w", field, i, err)
		}
		citationsField := fmt.Sprintf("%s block %d citations", field, i)
		if err := checkWireArray(block.Citations, citationsField, WireNestedCollectionMax); err != nil {
			return err
		}
		if err := addWireArray(&budget.citations, block.Citations, citationsField, WireNestedCollectionMax); err != nil {
			return err
		}
		if err := checkAnthropicBlockList(block.Content, fmt.Sprintf("%s block %d content", field, i), depth+1, budget); err != nil {
			return err
		}
		if block.Source != nil {
			if err := checkAnthropicBlockList(block.Source.Content, fmt.Sprintf("%s block %d source content", field, i), depth+1, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkAnthropicRequestCollections applies one aggregate nested budget across
// a system block list and every message content list before the typed request
// decode.
func checkAnthropicRequestCollections(system, messages json.RawMessage) error {
	var budget anthropicNestedBudget
	if err := checkAnthropicBlockList(system, "system", 0, &budget); err != nil {
		return err
	}
	if len(messages) == 0 || bytes.Equal(bytes.TrimSpace(messages), []byte("null")) {
		return nil
	}
	var items []struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(messages, &items); err != nil {
		return fmt.Errorf("parse messages: %w", err)
	}
	for i, item := range items {
		if err := checkAnthropicBlockList(item.Content, fmt.Sprintf("messages[%d].content", i), 0, &budget); err != nil {
			return err
		}
	}
	return nil
}

// checkChatMessageAnnotations bounds the message-level annotation arrays of
// one decoded Chat message array, per array and in aggregate.
func checkChatMessageAnnotations(messages json.RawMessage, field string) error {
	if len(messages) == 0 || bytes.Equal(bytes.TrimSpace(messages), []byte("null")) {
		return nil
	}
	var items []struct {
		Annotations json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(messages, &items); err != nil {
		return fmt.Errorf("parse %s: %w", field, err)
	}
	total := 0
	for i, item := range items {
		annotationField := fmt.Sprintf("%s[%d].annotations", field, i)
		if err := checkWireArray(item.Annotations, annotationField, metrics.ContentPartMax); err != nil {
			return err
		}
		if err := addWireArray(&total, item.Annotations, annotationField, WireNestedCollectionMax); err != nil {
			return err
		}
	}
	return nil
}

// responsesNestedBudget is the shared aggregate over every nested Responses
// content-part and annotation array in one document. Per-array limits alone
// let thousands of lists at that cap multiply into a typed slice and a warning
// list that are both far larger than the request.
type responsesNestedBudget struct {
	parts       int
	annotations int
}

// checkResponsesContent charges one Responses content-part array (message
// content, reasoning content, or function-call output) to the shared budgets.
// It enforces the per-array part limit, the aggregate part limit, every nested
// annotation array's per-array limit and the aggregate annotation limit before
// the typed decode materializes either. An absent, null or non-array value
// contributes nothing: function_call_output legitimately accepts a string.
func checkResponsesContent(budget *responsesNestedBudget, content json.RawMessage, field string) error {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	if !metrics.ArrayWithinLimit(trimmed, metrics.ContentPartMax) {
		return fmt.Errorf("%s exceeds %d parts", field, metrics.ContentPartMax)
	}
	var parts []struct {
		Annotations json.RawMessage `json:"annotations"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return fmt.Errorf("parse %s: %w", field, err)
	}
	if len(parts) > WireContentPartsMax-budget.parts {
		return fmt.Errorf("%s: nested content parts exceed %d", field, WireContentPartsMax)
	}
	budget.parts += len(parts)
	for i, part := range parts {
		annotationField := fmt.Sprintf("%s[%d].annotations", field, i)
		if err := checkWireArray(part.Annotations, annotationField, metrics.ContentPartMax); err != nil {
			return err
		}
		if err := addWireArray(&budget.annotations, part.Annotations, annotationField, WireNestedCollectionMax); err != nil {
			return err
		}
	}
	return nil
}
