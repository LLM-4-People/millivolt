package format

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// responsesFunctionCallID returns the target tool id for a streamed Responses
// function call: call_id first, then the item id, then a synthetic id for the
// output position. The non-stream decoder still rejects a call with both
// identities absent, but a stream must not close a target block with an empty
// id (Anthropic aborts and parallel Chat calls collide).
func responsesFunctionCallID(callID, itemID string, index int) string {
	switch {
	case callID != "":
		return callID
	case itemID != "":
		return itemID
	default:
		return "call_proxy_" + strconv.Itoa(index)
	}
}

// Item and content-part identities. A provider's terminal output array is not
// required to preserve output_index positions, so dedupe and terminal
// completion match on the wire identity the target correlates with: call id
// for function calls, item id for messages/reasoning, then content index for a
// part. A source that supplies no identity falls back to its wire position.
func functionCallKey(callID, itemID string, outputIndex int) string {
	switch {
	case callID != "":
		return "call:" + callID
	case itemID != "":
		return "item:" + itemID
	default:
		return "output:" + strconv.Itoa(outputIndex)
	}
}

// streamItemKey identifies one logical item across its announcement and the
// terminal document. Item ids are stable across renumbering; the output
// position is the fallback only when no id was supplied.
func streamItemKey(itemID string, outputIndex int) string {
	if itemID != "" {
		return "item:" + itemID
	}
	return "output:" + strconv.Itoa(outputIndex)
}

func reasoningItemKey(itemID string, outputIndex int) string {
	return streamItemKey(itemID, outputIndex)
}

func messageItemKey(itemID string, outputIndex int) string {
	if itemID != "" {
		return "message:" + itemID
	}
	return "output:" + strconv.Itoa(outputIndex)
}

func contentPartKey(itemID string, outputIndex, contentIndex int) string {
	if itemID != "" {
		return "part:" + itemID + ":" + strconv.Itoa(contentIndex)
	}
	return "output:" + strconv.Itoa(outputIndex) + ":part:" + strconv.Itoa(contentIndex)
}

// summaryPartKey identifies one reasoning summary part. Summary parts share a
// single item block but each part owns its coverage, so a later part or a
// longer terminal summary is not suppressed by the first part's bytes.
func summaryPartKey(itemKey string, summaryIndex int) string {
	return "summary:" + itemKey + ":" + strconv.Itoa(summaryIndex)
}

// emittedValue returns the bytes already emitted for one channel. Builder
// strings are safe to retain because appends never mutate existing bytes and
// a replacement abandons the old builder.
func (d *ResponsesStreamDecoder) emittedValue(key string) string {
	if b := d.emittedText[key]; b != nil {
		return b.String()
	}
	return ""
}

// appendEmittedText records incremental delta bytes for one channel without
// copying the existing coverage. The aggregate retained-text bound mirrors the
// encoder budget so a token-level stream cannot grow decoder memory without
// limit.
func (d *ResponsesStreamDecoder) appendEmittedText(key, value string) error {
	if value == "" {
		return nil
	}
	if d.retainedText+len(value) > ResponsesStreamRetainedMaxBytes {
		return fmt.Errorf("responses stream exceeds %d retained text bytes", ResponsesStreamRetainedMaxBytes)
	}
	b := d.emittedText[key]
	if b == nil {
		b = &strings.Builder{}
		d.emittedText[key] = b
	}
	b.WriteString(value)
	d.retainedText += len(value)
	return nil
}

// setEmittedText records a cumulative done/terminal value for one channel. A
// value that extends the current coverage appends only the suffix; a shorter
// or divergent authoritative value replaces the builder.
func (d *ResponsesStreamDecoder) setEmittedText(key, value string) error {
	old := d.emittedText[key]
	oldLen := 0
	if old != nil {
		oldLen = old.Len()
	}
	if value == "" {
		if old != nil {
			delete(d.emittedText, key)
			d.retainedText -= oldLen
		}
		return nil
	}
	if old != nil && strings.HasPrefix(value, old.String()) {
		return d.appendEmittedText(key, value[oldLen:])
	}
	if d.retainedText-oldLen+len(value) > ResponsesStreamRetainedMaxBytes {
		return fmt.Errorf("responses stream exceeds %d retained text bytes", ResponsesStreamRetainedMaxBytes)
	}
	if old != nil {
		delete(d.emittedText, key)
		d.retainedText -= oldLen
	}
	b := &strings.Builder{}
	b.WriteString(value)
	d.emittedText[key] = b
	d.retainedText += len(value)
	return nil
}

// pendingValueSuffix returns the bytes of a cumulative done/terminal value not
// already emitted for its channel. A value that is identical to or a prefix of
// the emitted bytes contributes nothing. A divergent value contributes nothing
// as well: re-emitting it would duplicate a visible prefix, which is worse
// than keeping the bytes the client already saw.
func pendingValueSuffix(emitted, value string) string {
	if value == "" {
		return ""
	}
	if emitted == "" {
		return value
	}
	if strings.HasPrefix(value, emitted) {
		return value[len(emitted):]
	}
	return ""
}

func (d *ResponsesStreamDecoder) announceOutput(outputIndex int) (bool, error) {
	if d.announcedOutputs[outputIndex] {
		return false, nil
	}
	if len(d.announcedOutputs) >= WireCollectionMax {
		return false, fmt.Errorf("responses stream exceeds %d announced output items", WireCollectionMax)
	}
	d.announcedOutputs[outputIndex] = true
	return true, nil
}

func (d *ResponsesStreamDecoder) announceItem(key string) (bool, error) {
	if d.announcedItems[key] {
		return false, nil
	}
	if len(d.announcedItems) >= WireCollectionMax {
		return false, fmt.Errorf("responses stream exceeds %d announced items", WireCollectionMax)
	}
	d.announcedItems[key] = true
	return true, nil
}

func (d *ResponsesStreamDecoder) announcePart(key string) (bool, error) {
	if d.announcedParts[key] {
		return false, nil
	}
	if len(d.announcedParts) >= WireNestedCollectionMax {
		return false, fmt.Errorf("responses stream exceeds %d announced content parts", WireNestedCollectionMax)
	}
	d.announcedParts[key] = true
	return true, nil
}

// FlushBlocks closes every block this decoder still holds open. The
// translation facade calls it when an upstream stream fails so a client never
// receives an error or terminal event with an unclosed block.
func (d *ResponsesStreamDecoder) FlushBlocks() ([]*ir.Event, error) {
	// FlushBlocks runs only when the upstream stream failed or truncated. The
	// buffered initial arguments are incomplete, so close the blocks without
	// parsing or emitting them.
	err := d.closeAll(nil, responsesTerminalTruncated)
	return drainQueue(&d.q), err
}

// Warnings returns the streaming approximations recorded so far. A streamed
// response cannot add X-Proxy-Warnings after its status line is committed, so
// this is decoder diagnostic state rather than a client header.
func (d *ResponsesStreamDecoder) Warnings() ir.Warnings {
	return append(ir.Warnings(nil), d.warnings...)
}

func (d *ResponsesStreamDecoder) newBlock() int {
	idx := d.nextIndex
	d.nextIndex++
	d.open[idx] = true
	return idx
}

func (d *ResponsesStreamDecoder) blocksFromItems(items []responsesWireItem) []ir.Block {
	var out []ir.Block
	for _, item := range items {
		// The shared vocabulary covers function_call plus provider-executed
		// custom, shell, MCP and computer calls. The target blocks are dropped
		// where unrepresentable, but the finish and token settlement must
		// still read as a tool turn.
		if sse.ResponsesToolCallItem(item.Type) {
			out = append(out, ir.Block{Type: ir.BlockToolCall})
		}
	}
	return out
}
