package format

import (
	"bytes"
	"encoding/json"
	"fmt"

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

// WireNestedCollectionMax bounds an aggregate of nested collections that a
// single top-level bound cannot cover, such as every reasoning summary array in
// an output list or every citation array in a content list.
const WireNestedCollectionMax = 10_000

// WireToolCallsMax bounds the aggregate tool-call members over every message.
// The proxy's request preflight and the Chat codec share this one value so the
// translated wire cannot accept a document the passthrough path rejects.
const WireToolCallsMax = 1024

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

// checkAnthropicCitations bounds every citations array in one Anthropic
// content-block list before the typed decode materializes the citations. A
// block list is already bounded by ContentPartMax, but one text block can carry
// an unbounded citation list.
func checkAnthropicCitations(content json.RawMessage, field string) error {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var blocks []struct {
		Citations json.RawMessage `json:"citations"`
	}
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return fmt.Errorf("parse %s: %w", field, err)
	}
	total := 0
	for i, block := range blocks {
		if err := addWireArray(&total, block.Citations, fmt.Sprintf("%s block %d citations", field, i), WireNestedCollectionMax); err != nil {
			return err
		}
	}
	return nil
}
