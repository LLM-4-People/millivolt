package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// responsesTerminalClass describes the outcome a Responses terminal carries.
// It decides whether an incomplete argument value may fail the translation.
type responsesTerminalClass uint8

const (
	responsesTerminalCompleted responsesTerminalClass = iota
	responsesTerminalFailed
	responsesTerminalIncomplete
	responsesTerminalCancelled
	responsesTerminalError
	responsesTerminalTruncated
)

// responsesToolArgumentPolicy tells the terminal output walker how much
// argument evidence a function call needs before it may reach the target.
type responsesToolArgumentPolicy uint8

const (
	// responsesToolArgumentsComplete requires a valid value but accepts an
	// empty one as the historical empty object.
	responsesToolArgumentsComplete responsesToolArgumentPolicy = iota
	// responsesToolArgumentsSkip discards a call whose value cannot be
	// validated. A failed, errored or truncated terminal owns the outcome.
	responsesToolArgumentsSkip
	// responsesToolArgumentsRequired rejects an empty or unvalidated value.
	// Incomplete and cancelled terminals are normal turn boundaries, so a
	// call the model did not finish must not close as an executable empty
	// object.
	responsesToolArgumentsRequired
)

// toolArgumentPolicy maps the terminal class to its argument evidence rule.
func (c responsesTerminalClass) toolArgumentPolicy() responsesToolArgumentPolicy {
	switch c {
	case responsesTerminalFailed, responsesTerminalError, responsesTerminalTruncated:
		return responsesToolArgumentsSkip
	case responsesTerminalIncomplete, responsesTerminalCancelled:
		return responsesToolArgumentsRequired
	default:
		return responsesToolArgumentsComplete
	}
}

// toolArgumentsMissing reports whether a raw value carries no evidence that
// the source supplied arguments. Empty and JSON null are not evidence; the
// explicit empty object is.
func toolArgumentsMissing(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	return trimmed == "" || trimmed == "null"
}

func responsesTerminalClassFor(eventType string) responsesTerminalClass {
	switch eventType {
	case "response.completed":
		return responsesTerminalCompleted
	case "response.failed":
		return responsesTerminalFailed
	case "response.incomplete":
		return responsesTerminalIncomplete
	case "response.cancelled":
		return responsesTerminalCancelled
	default:
		return responsesTerminalFailed
	}
}

// closeAll closes every block still open at a terminal event and synthesizes
// items that exist only in the terminal document. A provider may stream no
// per-item events at all and place the whole answer, refusal, reasoning or
// function call in response.completed.output; a translated client must still
// receive those blocks exactly once. The terminal class decides how an
// incomplete argument value is treated: a completed, incomplete or cancelled
// terminal validates it, while a failed, errored or truncated terminal drops
// the unparseable call and preserves the provider error or truncation.
func (d *ResponsesStreamDecoder) closeAll(output []responsesWireItem, class responsesTerminalClass) error {
	// Apply the authoritative terminal payload in terminal document order.
	// Announced blocks that already carry bytes are filled in place; every
	// block that still has to start (an unannounced item/part, or a deferred
	// empty announcement) is queued and started only after all streamed blocks
	// close. Done-event payloads queued earlier share that tail.
	pending := d.terminalPending
	d.terminalPending = nil
	clear(d.terminalPendingByKey)
	policy := class.toolArgumentPolicy()
	more, err := d.collectTerminalOutput(output, policy)
	if err != nil {
		return err
	}
	pending = append(pending, more...)
	for i := range pending {
		pending[i].materialize()
	}
	// A terminal that validates tool arguments can arrive before the streamed
	// value was complete: the buffered added payload is then the only argument
	// bytes the source produced, so flush it before the block closes. The
	// identities are sorted so the emitted delta order is deterministic. A
	// failed, errored or truncated terminal instead discards the buffer below
	// without parsing or emitting it: a partial value must neither reach the
	// target nor replace the provider error or truncation with a translator
	// failure.
	if policy != responsesToolArgumentsSkip {
		// Every open tool call is validated, including one that never carried
		// argument bytes: the required policy must reject the missing
		// evidence instead of letting the block close as an empty object.
		toolKeys := make([]string, 0, len(d.openToolKeys))
		for key := range d.openToolKeys {
			toolKeys = append(toolKeys, key)
		}
		sort.Strings(toolKeys)
		for _, key := range toolKeys {
			if idx, ok := d.itemBlockByKey[key]; ok && d.open[idx] {
				if err := d.flushToolArguments(key, idx, "", policy); err != nil {
					return err
				}
			}
		}
	}
	// Close every block the source actually opened before starting queued
	// terminal blocks, so a target block sequence never nests two lifecycles.
	// A failure policy leaves an open tool block unclosed: BlockEnd would
	// publish a call over partial argument bytes.
	skipTools := map[int]struct{}{}
	if policy == responsesToolArgumentsSkip {
		for key := range d.openToolKeys {
			if idx, ok := d.itemBlockByKey[key]; ok {
				skipTools[idx] = struct{}{}
			}
		}
	}
	for _, idx := range sortedKeys(d.open) {
		delete(d.open, idx)
		if _, skip := skipTools[idx]; skip {
			continue
		}
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
	}
	clear(d.partBlock)
	clear(d.itemBlock)
	clear(d.initialArgs)
	d.initialArgsBytes = 0
	clear(d.argsEmitted)
	clear(d.argumentEvidence)
	clear(d.openToolKeys)
	clear(d.announcedToolKeys)
	clear(d.toolAliases)
	clear(d.toolNames)
	clear(d.toolArgsHash)
	clear(d.closedIdlessCalls)
	clear(d.keyByOutput)
	clear(d.keyByPart)
	clear(d.partSeed)
	clear(d.partPosition)
	clear(d.emittedText)
	clear(d.ownerParts)
	d.retainedText = 0
	d.openToolItems = 0
	for _, block := range pending {
		d.syntheticBlock(block.block, block.delta)
	}
	return nil
}

// terminalAccumulator buffers repeated unannounced deltas for one part. A
// strings.Builder grows amortized O(1), so n one-byte deltas allocate O(n)
// rather than the O(n^2) of repeated string concatenation on the ir.Delta. The
// builders are pointer fields because terminalEmission values are copied; a
// strings.Builder must never be copied after use.
type terminalAccumulator struct {
	text      strings.Builder
	reasoning strings.Builder
	arguments strings.Builder
	citation  *ir.Citation
}

// terminalEmission is a terminal value that belongs to a block which has
// already ended, or to a terminal-only channel. It is emitted after every
// streamed block has closed so target lifecycles never nest. Entries created
// by the coalescing queue carry an accumulator that materializes into delta
// once, at emission time; direct terminal-document entries leave it nil.
type terminalEmission struct {
	block ir.Block
	delta *ir.Delta
	acc   *terminalAccumulator
}

func (e *terminalEmission) add(src *ir.Delta) {
	if src == nil {
		return
	}
	if e.acc == nil {
		e.acc = &terminalAccumulator{}
	}
	e.acc.text.WriteString(src.Text)
	e.acc.reasoning.WriteString(src.Reasoning)
	e.acc.arguments.WriteString(src.Arguments)
	if src.Citation != nil {
		e.acc.citation = src.Citation
	}
}

// materialize joins the buffered fragments into the emission delta exactly
// once. Builder.String returns its buffer without copying, and no write follows
// this call, so the buffer is never mutated again.
func (e *terminalEmission) materialize() {
	if e.acc == nil || e.delta == nil {
		return
	}
	e.delta.Text = e.acc.text.String()
	e.delta.Reasoning = e.acc.reasoning.String()
	e.delta.Arguments = e.acc.arguments.String()
	e.delta.Citation = e.acc.citation
	e.acc = nil
}

// terminalAnnouncedItemKey resolves the announcement identity of a terminal
// reasoning item. A unique announced item id wins over the terminal array
// position; the output position is a fallback only when both sides agree on
// the item id (or one side has none).
func (d *ResponsesStreamDecoder) terminalAnnouncedItemKey(outputIndex int, itemID string) (string, bool) {
	key := reasoningItemKey(itemID, outputIndex)
	if _, ok := d.itemBlockByKey[key]; ok && d.reasoningKeys[key] {
		return key, true
	}
	if announced, ok := d.announcedItemKey(itemKindReasoning, itemID); ok {
		if _, exists := d.itemBlockByKey[announced]; exists && d.reasoningKeys[announced] {
			return announced, true
		}
	}
	fallback, ok := d.keyByOutput[outputIndex]
	if !ok || !d.reasoningKeys[fallback] {
		return key, false
	}
	announcedID := d.itemIDByOutput[outputIndex]
	if itemID != "" && announcedID != "" && announcedID != itemID {
		return key, false
	}
	return fallback, true
}

// reasoningTerminalBlock builds the canonical reasoning block for a terminal
// reasoning item.
func reasoningTerminalBlock(item responsesWireItem) ir.Block {
	return ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
		ID: item.ID, Encrypted: item.EncryptedContent, Provider: ir.Responses,
	}}
}

// syntheticBlock emits a complete canonical block lifecycle for terminal-only
// output. Terminal synthesis is self-contained: the block is started, filled
// and closed before the next item, and no announcement state is recorded.
func (d *ResponsesStreamDecoder) syntheticBlock(block ir.Block, delta *ir.Delta) {
	idx := d.newBlock()
	delete(d.open, idx)
	d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &block})
	if delta != nil {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: delta})
	}
	d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
}

// functionCallTerminalEmission builds the complete block for a function call
// that appeared only in the terminal document.
func functionCallTerminalEmission(item responsesWireItem, index int, policy responsesToolArgumentPolicy) (terminalEmission, bool, error) {
	toolID := responsesFunctionCallID(item.CallID, item.ID, index)
	if err := checkNativeIdentity("tool id", toolID); err != nil {
		return terminalEmission{}, false, err
	}
	if err := checkNativeIdentity("tool name", item.Name); err != nil {
		return terminalEmission{}, false, err
	}
	if err := checkToolCallName(item.Name); err != nil {
		return terminalEmission{}, false, err
	}
	if policy == responsesToolArgumentsRequired && toolArgumentsMissing(item.Arguments) {
		return terminalEmission{}, false, errToolArgumentsMissing
	}
	args, err := parseToolArguments(item.Arguments)
	if err != nil {
		if policy == responsesToolArgumentsSkip {
			return terminalEmission{}, false, nil
		}
		return terminalEmission{}, false, err
	}
	var delta *ir.Delta
	if string(args) != "{}" {
		delta = &ir.Delta{Arguments: string(args)}
	}
	return terminalEmission{
		block: ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
			// The block is the type-only start template; the normalized
			// arguments travel in the delta so a target encoder cannot count
			// them twice.
			ID: toolID, Name: item.Name, Arguments: json.RawMessage(`{}`),
		}},
		delta: delta,
	}, true, nil
}

// terminalContentPart is one content part of a terminal message item with the
// content index that identifies it against the announced parts.
type terminalContentPart struct {
	contentIndex int
	block        ir.Block
	delta        *ir.Delta
}

// terminalMessageParts decodes a terminal message content value into the parts
// the target can carry, preserving each part's content index.
func terminalMessageParts(raw json.RawMessage) ([]terminalContentPart, error) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []terminalContentPart{{
			block: ir.Block{Type: ir.BlockText},
			delta: &ir.Delta{Text: text},
		}}, nil
	}
	if !metrics.ContentPartsWithinLimit(raw) {
		return nil, fmt.Errorf("responses terminal content exceeds %d parts", metrics.ContentPartMax)
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	out := make([]terminalContentPart, 0, len(parts))
	for contentIndex, part := range parts {
		wrapped := make([]byte, 0, len(part)+2)
		wrapped = append(wrapped, '[')
		wrapped = append(wrapped, part...)
		wrapped = append(wrapped, ']')
		blocks, _, err := decodeResponsesContent(wrapped, ir.RoleAssistant)
		if err != nil {
			return nil, err
		}
		for _, block := range blocks {
			switch block.Type {
			case ir.BlockText:
				if block.Text != "" {
					out = append(out, terminalContentPart{
						contentIndex: contentIndex,
						block:        ir.Block{Type: ir.BlockText},
						delta:        &ir.Delta{Text: block.Text},
					})
				}
			case ir.BlockRefusal:
				if block.Refusal != "" {
					out = append(out, terminalContentPart{
						contentIndex: contentIndex,
						block:        ir.Block{Type: ir.BlockRefusal},
						delta:        &ir.Delta{Text: block.Refusal},
					})
				}
			}
		}
	}
	return out, nil
}

// reasoningTerminalEmission builds the complete reasoning block for an item
// that appeared only in the terminal document, combining its reasoning_text
// content and summary parts.
func reasoningTerminalEmission(item responsesWireItem) (terminalEmission, bool, error) {
	if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
		return terminalEmission{}, false, err
	}
	var text strings.Builder
	if len(item.Content) > 0 {
		blocks, _, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
		if err != nil {
			return terminalEmission{}, false, err
		}
		for _, block := range blocks {
			if block.Type == ir.BlockText {
				text.WriteString(block.Text)
			}
		}
	}
	for _, summary := range item.Summary {
		text.WriteString(summary.Text)
	}
	if text.Len() == 0 && item.EncryptedContent == "" {
		return terminalEmission{}, false, nil
	}
	var delta *ir.Delta
	if text.Len() > 0 {
		delta = &ir.Delta{Reasoning: text.String()}
	}
	return terminalEmission{block: reasoningTerminalBlock(item), delta: delta}, true, nil
}
