package format

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// ResponsesStreamRetainedMaxBytes bounds the total text, arguments, reasoning,
// refusal, provider identity and per-item envelope bytes the encoder retains
// across ALL synthesized Responses items for their done and terminal events.
// The wire requires those events to carry the full value, so the encoder
// buffers it; the relay sizes its translated-frame scanner from this one
// aggregate bound and the maximum JSON escape expansion. A per-item value is
// subordinate: the total can never exceed this. Internal safety guardrail, not
// user-tunable.
const ResponsesStreamRetainedMaxBytes = 8 << 20

// responsesStreamItemEnvelopeBytes is the conservative fixed cost charged for
// one synthesized item's terminal envelope: its type, generated id, status,
// role and map keys. Provider-controlled call ids and names are charged
// separately at their actual length. Internal safety guardrail, not
// user-tunable.
const responsesStreamItemEnvelopeBytes = 512

// WireJSONEscapeExpansion is the largest number of bytes MarshalWireJSON emits
// per retained byte: a control byte becomes the six-byte \u00XX escape. The
// relay's translated-frame bound must cover it.
const WireJSONEscapeExpansion = 6

// ResponsesStreamFrameMaxBytes bounds one frame the Responses stream encoder
// can emit from its enforced aggregate retention: the retained bytes after the
// maximum JSON escape expansion plus the fixed frame envelope. The proxy sizes
// its translated relay scanner from this bound.
const ResponsesStreamFrameMaxBytes = WireJSONEscapeExpansion*ResponsesStreamRetainedMaxBytes + nativeStreamFrameEnvelopeMax

// OpenAI Responses streaming codec.

type responsesStreamEvent struct {
	Type           string          `json:"type"`
	SequenceNumber int64           `json:"sequence_number"`
	Response       json.RawMessage `json:"response"`
	Item           json.RawMessage `json:"item"`
	Part           json.RawMessage `json:"part"`
	OutputIndex    int             `json:"output_index"`
	ContentIndex   int             `json:"content_index"`
	SummaryIndex   int             `json:"summary_index"`
	ItemID         string          `json:"item_id"`
	Delta          string          `json:"delta"`
	Text           string          `json:"text"`
	Arguments      string          `json:"arguments"`
	Code           string          `json:"code"`
	Message        string          `json:"message"`
	Param          string          `json:"param"`
}

// ResponsesStreamDecoder converts Responses SSE events into canonical events.
type ResponsesStreamDecoder struct {
	r        *sseReader
	q        eventQueue
	started  bool
	terminal bool

	// observe receives every raw source data payload before it is parsed.
	// The translation facade uses it for source-only accounting (cost keys
	// the target wire cannot carry); decoding itself never depends on it.
	observe func([]byte)

	id      string
	model   string
	created int64

	nextIndex int
	itemBlock map[int]int
	partBlock map[[2]int]int
	// initialArgs holds output_item.added item.arguments that have not been
	// emitted yet. A partial prefix must not reach the target before the
	// first delta or the full done/terminal value decides the continuation.
	// It is keyed by the item's stable identity, not its wire position.
	initialArgs map[string]string
	// initialArgsBytes bounds the total buffered initial arguments retained
	// across open tool blocks, and openToolItems bounds how many such blocks
	// can be open at once.
	initialArgsBytes int
	openToolItems    int
	// argsEmitted marks that at least one argument payload was emitted for an
	// item identity, so a done/terminal full value must not repeat it.
	argsEmitted map[string]bool
	// Announcement identity. A provider's terminal output array is not
	// required to preserve output_index positions, so dedupe and terminal
	// completion use stable identities (call id, item id, content index) and
	// fall back to a position only when the source supplied no identity.
	announcedOutputs map[int]bool
	announcedItems   map[string]bool
	announcedParts   map[string]bool
	itemBlockByKey   map[string]int
	partBlockByKey   map[string]int
	keyByOutput      map[int]string
	keyByPart        map[[2]int]string
	itemIDByOutput   map[int]string
	open             map[int]bool

	usage       ir.Usage
	finish      ir.FinishReason
	sawToolCall bool
}

// NewResponsesStreamDecoder builds a decoder over one upstream body.
func NewResponsesStreamDecoder(r io.Reader) *ResponsesStreamDecoder {
	return &ResponsesStreamDecoder{
		r:                newSSEReader(r),
		itemBlock:        map[int]int{},
		partBlock:        map[[2]int]int{},
		initialArgs:      map[string]string{},
		argsEmitted:      map[string]bool{},
		announcedOutputs: map[int]bool{},
		announcedItems:   map[string]bool{},
		announcedParts:   map[string]bool{},
		itemBlockByKey:   map[string]int{},
		partBlockByKey:   map[string]int{},
		keyByOutput:      map[int]string{},
		keyByPart:        map[[2]int]string{},
		itemIDByOutput:   map[int]string{},
		open:             map[int]bool{},
	}
}

// Next returns the next canonical event.
func (d *ResponsesStreamDecoder) Next() (*ir.Event, error) {
	for {
		if ev, ok := d.q.pop(); ok {
			return ev, nil
		}
		if d.terminal {
			return nil, io.EOF
		}
		raw, err := d.r.Next()
		if err == io.EOF {
			if !d.terminal {
				return nil, ErrTruncatedStream
			}
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		if len(raw.Data) == 0 {
			continue
		}
		if d.observe != nil {
			d.observe(raw.Data)
		}
		var ev responsesStreamEvent
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			return nil, err
		}
		switch ev.Type {
		case "response.created", "response.in_progress":
			if d.started {
				continue
			}
			var resp responsesWireResponse
			if err := json.Unmarshal(ev.Response, &resp); err != nil {
				return nil, err
			}
			if err := checkNativeIdentity("id", resp.ID); err != nil {
				return nil, err
			}
			if err := checkNativeIdentity("model", resp.Model); err != nil {
				return nil, err
			}
			d.id, d.model, d.created = resp.ID, resp.Model, resp.CreatedAt
			if d.created == 0 {
				d.created = time.Now().Unix()
			}
			if resp.Usage != nil {
				usage := responsesUsageToIR(resp.Usage)
				if err := validateUsage(usage); err != nil {
					return nil, err
				}
				d.usage = usage
			}
			d.started = true
			u := d.usage
			d.q.push(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
				ID: d.id, Model: d.model, Created: d.created, Usage: u,
			}})
		case "response.output_item.added":
			var item responsesWireItem
			if err := json.Unmarshal(ev.Item, &item); err != nil {
				return nil, err
			}
			admitted, err := d.announceOutput(ev.OutputIndex)
			if err != nil {
				return nil, err
			}
			if !admitted {
				// A duplicate announcement for one output index must not
				// allocate a second target block.
				continue
			}
			switch item.Type {
			case "function_call":
				if err := checkNativeIdentity("tool id", item.CallID); err != nil {
					return nil, err
				}
				if err := checkNativeIdentity("tool name", item.Name); err != nil {
					return nil, err
				}
				key := functionCallKey(item.CallID, item.ID, ev.OutputIndex)
				admitted, err := d.announceItem(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
				d.sawToolCall = true
				d.openToolItems++
				if d.openToolItems > toolBlocksMax {
					return nil, fmt.Errorf("responses stream exceeds %d concurrent tool calls", toolBlocksMax)
				}
				idx := d.newBlock()
				d.itemBlock[ev.OutputIndex] = idx
				d.itemBlockByKey[key] = idx
				d.keyByOutput[ev.OutputIndex] = key
				d.itemIDByOutput[ev.OutputIndex] = item.ID
				d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &ir.Block{
					Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
						ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(`{}`),
					},
				}})
				// output_item.added may carry a prefix of the arguments, or the
				// whole object. Buffer it: a partial prefix emitted alone would
				// be invalid JSON when no delta follows. The first delta or the
				// done/terminal value decides the continuation.
				if args := item.Arguments; args != "" && args != "{}" {
					if len(args) > toolInitialBytesMax-d.initialArgsBytes {
						return nil, fmt.Errorf("responses stream exceeds %d retained initial tool bytes", toolInitialBytesMax)
					}
					d.initialArgs[key] = args
					d.initialArgsBytes += len(args)
				}
			case "reasoning":
				if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
					return nil, err
				}
				key := reasoningItemKey(item.ID, ev.OutputIndex)
				admitted, err := d.announceItem(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
				idx := d.newBlock()
				d.itemBlock[ev.OutputIndex] = idx
				d.itemBlockByKey[key] = idx
				d.keyByOutput[ev.OutputIndex] = key
				d.itemIDByOutput[ev.OutputIndex] = item.ID
				d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &ir.Block{
					Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
						ID: item.ID, Encrypted: item.EncryptedContent, Provider: ir.Responses,
					},
				}})
			case "message":
				// Content parts arrive in response.content_part.added. The
				// item id is still announced so a terminal document that
				// repeats the item at another array position is recognized.
				if _, err := d.announceItem(messageItemKey(item.ID, ev.OutputIndex)); err != nil {
					return nil, err
				}
				d.itemIDByOutput[ev.OutputIndex] = item.ID
			default:
				// A provider-specific item has no cross-protocol equivalent.
				// Streaming cannot report the loss (the status line is already
				// committed), so it is dropped without a record.
			}
		case "response.content_part.added":
			var part struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			}
			if err := json.Unmarshal(ev.Part, &part); err != nil {
				return nil, err
			}
			var block ir.Block
			switch part.Type {
			case "output_text", "input_text", "text":
				block = ir.Block{Type: ir.BlockText, Text: part.Text}
			case "refusal":
				block = ir.Block{Type: ir.BlockRefusal, Refusal: part.Refusal}
			case "reasoning_text":
				block = ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: part.Text, Provider: ir.Responses}}
			default:
				// Allocate the block only after the part type is accepted: an
				// unsupported part must not consume an index or leave an open
				// block whose stop would reach the client without a start.
				// Streaming cannot report the loss, so it is dropped silently.
				continue
			}
			itemID := ev.ItemID
			if itemID == "" {
				itemID = d.itemIDByOutput[ev.OutputIndex]
			}
			key := contentPartKey(itemID, ev.OutputIndex, ev.ContentIndex)
			admitted, err := d.announcePart(key)
			if err != nil {
				return nil, err
			}
			if !admitted {
				continue
			}
			idx := d.newBlock()
			d.partBlock[[2]int{ev.OutputIndex, ev.ContentIndex}] = idx
			d.partBlockByKey[key] = idx
			d.keyByPart[[2]int{ev.OutputIndex, ev.ContentIndex}] = key
			d.itemIDByOutput[ev.OutputIndex] = itemID
			d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &block})
		case "response.output_text.delta", "response.refusal.delta":
			idx, ok := d.partBlock[[2]int{ev.OutputIndex, ev.ContentIndex}]
			if !ok {
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Text: ev.Delta}})
		case "response.function_call_arguments.delta":
			idx, ok := d.itemBlock[ev.OutputIndex]
			if !ok {
				continue
			}
			d.toolArgumentsDelta(d.keyByOutput[ev.OutputIndex], idx, ev.Delta)
		case "response.reasoning_summary_text.delta":
			idx, ok := d.itemBlock[ev.OutputIndex]
			if !ok {
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: ev.Delta}})
		case "response.reasoning_text.delta":
			// reasoning_text is a content part of the reasoning item, so its
			// deltas belong to the part block the added event opened (C12).
			// reasoning_summary_text has no content part and stays on the item
			// block.
			idx, ok := d.partBlock[[2]int{ev.OutputIndex, ev.ContentIndex}]
			if !ok {
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: ev.Delta}})
		case "response.content_part.done":
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			idx, ok := d.partBlock[pos]
			if !ok {
				continue
			}
			delete(d.partBlock, pos)
			delete(d.open, idx)
			if key, ok := d.keyByPart[pos]; ok {
				delete(d.partBlockByKey, key)
				delete(d.keyByPart, pos)
			}
			d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
		case "response.output_item.done":
			var item responsesWireItem
			if err := json.Unmarshal(ev.Item, &item); err != nil {
				return nil, err
			}
			if item.Type == "function_call" {
				idx, ok := d.itemBlock[ev.OutputIndex]
				if !ok {
					continue
				}
				key := d.keyByOutput[ev.OutputIndex]
				d.flushToolArguments(key, idx, item.Arguments)
				delete(d.itemBlock, ev.OutputIndex)
				delete(d.open, idx)
				d.clearToolState(key)
				d.openToolItems--
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if item.Type == "reasoning" {
				idx, ok := d.itemBlock[ev.OutputIndex]
				if !ok {
					continue
				}
				delete(d.itemBlock, ev.OutputIndex)
				delete(d.open, idx)
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if item.Type == "message" {
				// Some providers skip content_part.done; close any part
				// block that is still open for this output item.
				for key, idx := range d.partBlock {
					if key[0] == ev.OutputIndex {
						delete(d.partBlock, key)
						delete(d.open, idx)
						d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
					}
				}
			}
		case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
			var resp responsesWireResponse
			if err := json.Unmarshal(ev.Response, &resp); err != nil {
				return nil, err
			}
			if resp.Usage != nil {
				usage := responsesUsageToIR(resp.Usage)
				if err := validateUsage(usage); err != nil {
					return nil, err
				}
				d.usage = usage
			}
			d.terminal = true
			if err := d.closeAll(resp.Output); err != nil {
				return nil, err
			}
			if ev.Type == "response.failed" {
				e := &ir.Error{Message: "upstream response failed"}
				if resp.Error != nil {
					e.Code, e.Type = resp.Error.Code, resp.Error.Type
					if resp.Error.Message != "" {
						e.Message = resp.Error.Message
					}
				}
				d.q.push(&ir.Event{Type: ir.EventError, Error: e})
				continue
			}
			finish := ir.FinishStop
			switch ev.Type {
			case "response.cancelled":
				finish = ir.FinishCancelled
			case "response.incomplete":
				finish = ir.FinishOther
				if resp.IncompleteDetails != nil {
					switch resp.IncompleteDetails.Reason {
					case "max_output_tokens":
						finish = ir.FinishLength
					case "content_filter":
						finish = ir.FinishContentFilter
					}
				}
			default:
				if d.sawToolCall || hasToolCall(d.blocksFromItems(resp.Output)) {
					finish = ir.FinishToolCalls
				}
			}
			u := d.usage
			d.q.push(&ir.Event{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: finish}, Usage: &u})
			d.q.push(&ir.Event{Type: ir.EventDone})
		case "error":
			d.terminal = true
			if err := d.closeAll(nil); err != nil {
				return nil, err
			}
			d.q.push(&ir.Event{Type: ir.EventError, Error: &ir.Error{
				Type: "api_error", Code: ev.Code, Message: ev.Message,
			}})
		default:
			// response.output_text.done, function_call_arguments.done,
			// reasoning_summary_* and provider-specific events are already
			// represented by the deltas above.
			continue
		}
	}
}

// closeAll closes every block still open at a terminal event and synthesizes
// items that exist only in the terminal document. A provider may stream no
// per-item events at all and place the whole answer, refusal, reasoning or
// function call in response.completed.output; a translated client must still
// receive those blocks exactly once.
func (d *ResponsesStreamDecoder) closeAll(output []responsesWireItem) error {
	// First complete the arguments of announced calls whose final value
	// arrived only in the terminal document. The terminal array position is
	// not an identity: a shortened output array, or one that starts above
	// zero, must still match the announced call by call id/item id.
	for i, item := range output {
		if item.Type != "function_call" {
			continue
		}
		key := functionCallKey(item.CallID, item.ID, i)
		idx, ok := d.itemBlockByKey[key]
		if !ok && item.CallID == "" && item.ID == "" {
			// Positional fallback only when the terminal item carries no
			// identity to match against.
			if fallback, ok := d.keyByOutput[i]; ok {
				key, idx, ok = fallback, d.itemBlockByKey[fallback], true
			}
		}
		if ok && d.open[idx] {
			d.flushToolArguments(key, idx, item.Arguments)
		}
	}
	// A truncated stream can end before a terminal document supplies the full
	// value. The buffered added payload is then the only argument bytes the
	// source produced, so flush it before the block closes. The identities are
	// sorted so the emitted delta order is deterministic.
	toolKeys := make([]string, 0, len(d.initialArgs))
	for key := range d.initialArgs {
		toolKeys = append(toolKeys, key)
	}
	sort.Strings(toolKeys)
	for _, key := range toolKeys {
		if idx, ok := d.itemBlockByKey[key]; ok && d.open[idx] {
			d.flushToolArguments(key, idx, "")
		}
	}
	// Close every block the source actually opened before starting synthetic
	// terminal items, so a target block sequence never nests two lifecycles.
	for _, idx := range sortedKeys(d.open) {
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
		delete(d.open, idx)
	}
	clear(d.partBlock)
	clear(d.itemBlock)
	clear(d.initialArgs)
	d.initialArgsBytes = 0
	clear(d.argsEmitted)
	clear(d.keyByOutput)
	clear(d.keyByPart)
	d.openToolItems = 0
	// Synthesize terminal-only items in document order. Identity keys keep a
	// repeated or renumbered terminal entry from allocating a second block.
	for i, item := range output {
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch typ {
		case "function_call":
			key := functionCallKey(item.CallID, item.ID, i)
			if d.terminalItemAnnounced(key, i, item.ID) {
				continue
			}
			admitted, err := d.announceItem(key)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
			if err := d.synthesizeFunctionCall(item); err != nil {
				return err
			}
		case "message":
			if err := d.synthesizeMessage(i, item); err != nil {
				return err
			}
		case "reasoning":
			key := reasoningItemKey(item.ID, i)
			if d.terminalItemAnnounced(key, i, item.ID) {
				continue
			}
			admitted, err := d.announceItem(key)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
			if err := d.synthesizeReasoning(item); err != nil {
				return err
			}
		}
	}
	return nil
}

// terminalItemAnnounced reports whether a terminal item's stable identity was
// already announced by a streamed item event. A positional fallback is used
// only when the stream and terminal item share an item id (or one side has no
// id), never to relabel a different item that happens to occupy the position.
func (d *ResponsesStreamDecoder) terminalItemAnnounced(key string, outputIndex int, itemID string) bool {
	if d.announcedItems[key] {
		return true
	}
	fallback, ok := d.keyByOutput[outputIndex]
	if !ok || !d.announcedItems[fallback] {
		return false
	}
	announcedID := d.itemIDByOutput[outputIndex]
	return itemID == "" || announcedID == "" || announcedID == itemID
}

// terminalPartKey resolves the announcement identity of one terminal content
// part. The content index is the part identity; the output position is used
// only when both sides agree on the item id (or one side has none).
func (d *ResponsesStreamDecoder) terminalPartKey(itemID string, outputIndex, contentIndex int) string {
	key := contentPartKey(itemID, outputIndex, contentIndex)
	if d.announcedParts[key] {
		return key
	}
	posKey, ok := d.keyByPart[[2]int{outputIndex, contentIndex}]
	if !ok {
		return key
	}
	announcedID := d.itemIDByOutput[outputIndex]
	if itemID == "" || announcedID == "" || announcedID == itemID {
		return posKey
	}
	return key
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

// synthesizeFunctionCall emits one function_call that appeared only in the
// terminal document.
func (d *ResponsesStreamDecoder) synthesizeFunctionCall(item responsesWireItem) error {
	if err := checkNativeIdentity("tool id", item.CallID); err != nil {
		return err
	}
	if err := checkNativeIdentity("tool name", item.Name); err != nil {
		return err
	}
	d.sawToolCall = true
	var delta *ir.Delta
	if item.Arguments != "" && item.Arguments != "{}" {
		delta = &ir.Delta{Arguments: item.Arguments}
	}
	d.syntheticBlock(ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
		ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(`{}`),
	}}, delta)
	return nil
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

// synthesizeMessage emits the content parts of one message item that appeared
// only in the terminal document. Parts already announced by content_part.added
// are skipped by their stable identity.
func (d *ResponsesStreamDecoder) synthesizeMessage(outputIndex int, item responsesWireItem) error {
	parts, err := terminalMessageParts(item.Content)
	if err != nil {
		return err
	}
	for _, part := range parts {
		key := d.terminalPartKey(item.ID, outputIndex, part.contentIndex)
		if d.announcedParts[key] {
			continue
		}
		admitted, err := d.announcePart(key)
		if err != nil {
			return err
		}
		if !admitted {
			continue
		}
		d.syntheticBlock(part.block, part.delta)
	}
	return nil
}

// synthesizeReasoning emits one reasoning item that appeared only in the
// terminal document, combining its reasoning_text content and summary parts.
func (d *ResponsesStreamDecoder) synthesizeReasoning(item responsesWireItem) error {
	if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
		return err
	}
	var text strings.Builder
	if len(item.Content) > 0 {
		blocks, _, err := decodeResponsesContent(item.Content, ir.RoleAssistant)
		if err != nil {
			return err
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
		return nil
	}
	block := ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
		ID: item.ID, Encrypted: item.EncryptedContent, Provider: ir.Responses,
	}}
	var delta *ir.Delta
	if text.Len() > 0 {
		delta = &ir.Delta{Reasoning: text.String()}
	}
	d.syntheticBlock(block, delta)
	return nil
}

// toolArgumentsDelta applies the first streamed arguments delta for one item
// identity. The buffered output_item.added payload is emitted first; when the
// delta already repeats that prefix only its suffix is emitted. No delta means
// no emission, so the done or terminal full value can still replace the
// initial payload.
func (d *ResponsesStreamDecoder) toolArgumentsDelta(key string, blockIndex int, delta string) {
	if delta == "" || key == "" {
		return
	}
	if !d.argsEmitted[key] {
		if initial := d.initialArgs[key]; initial != "" {
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: initial}})
			if strings.HasPrefix(delta, initial) {
				delta = delta[len(initial):]
			}
		}
		d.dropInitialArgs(key)
		d.argsEmitted[key] = true
	}
	if delta != "" {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: delta}})
	}
}

// flushToolArguments emits the full done/terminal arguments for an item
// identity that never streamed a delta. When the completion carries no value
// at all, the buffered output_item.added payload is the only available value.
func (d *ResponsesStreamDecoder) flushToolArguments(key string, blockIndex int, full string) {
	if key == "" || d.argsEmitted[key] {
		return
	}
	args := full
	if args == "" {
		args = d.initialArgs[key]
	}
	d.argsEmitted[key] = true
	d.dropInitialArgs(key)
	if args != "" && args != "{}" {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: args}})
	}
}

// dropInitialArgs releases the buffered output_item.added arguments for one
// item identity and its share of the initial-arguments bound.
func (d *ResponsesStreamDecoder) dropInitialArgs(key string) {
	if args, ok := d.initialArgs[key]; ok {
		d.initialArgsBytes -= len(args)
		delete(d.initialArgs, key)
	}
}

// clearToolState releases every per-call entry once a function call closes.
func (d *ResponsesStreamDecoder) clearToolState(key string) {
	d.dropInitialArgs(key)
	delete(d.argsEmitted, key)
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

func reasoningItemKey(itemID string, outputIndex int) string {
	if itemID != "" {
		return "item:" + itemID
	}
	return "output:" + strconv.Itoa(outputIndex)
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
func (d *ResponsesStreamDecoder) FlushBlocks() []*ir.Event {
	_ = d.closeAll(nil)
	return drainQueue(&d.q)
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
		if item.Type == "function_call" {
			out = append(out, ir.Block{Type: ir.BlockToolCall})
		}
	}
	return out
}

// ResponsesStreamEncoder renders canonical events as Responses SSE.
type ResponsesStreamEncoder struct {
	w       io.Writer
	flusher http.Flusher

	seq     int64
	id      string
	model   string
	created int64

	started     bool
	outputIndex int
	items       map[int]*responsesItemState
	open        map[int]bool
	order       []int
	finish      ir.FinishReason
	usage       ir.Usage
	failed      *ir.Error
	retained    int
	doneSent    bool
}

type responsesItemState struct {
	id           string
	outputIndex  int
	contentIndex int
	kind         string
	partType     string
	name         string
	callID       string
	encrypted    string
	text         strings.Builder
	args         strings.Builder
	reasoning    strings.Builder
	refusal      strings.Builder
	partOpen     bool
	summaryOpen  bool
}

// NewResponsesStreamEncoder builds an encoder over one client response writer.
func NewResponsesStreamEncoder(w io.Writer, flusher http.Flusher) *ResponsesStreamEncoder {
	return &ResponsesStreamEncoder{
		w: w, flusher: flusher,
		items: map[int]*responsesItemState{},
		open:  map[int]bool{},
	}
}

// startStream emits the mandatory response.created/response.in_progress pair
// once, synthesizing identity for a source stream that never delivered its own
// start event.
func (e *ResponsesStreamEncoder) startStream() error {
	if e.started {
		return nil
	}
	e.started = true
	if e.id == "" {
		e.id = "resp_" + randomID()
	}
	if e.created == 0 {
		e.created = time.Now().Unix()
	}
	if err := e.retain(len(e.id)+len(e.model), "response identity"); err != nil {
		return err
	}
	response, err := e.responseObject("in_progress", nil)
	if err != nil {
		return err
	}
	if err := e.emit("response.created", map[string]any{
		"type": "response.created", "sequence_number": e.nextSeq(),
		"response": response,
	}); err != nil {
		return err
	}
	return e.emit("response.in_progress", map[string]any{
		"type": "response.in_progress", "sequence_number": e.nextSeq(),
		"response": response,
	})
}

// Write renders one canonical event.
func (e *ResponsesStreamEncoder) Write(ev *ir.Event) error {
	if ev.Type != ir.EventStart {
		if err := e.startStream(); err != nil {
			return err
		}
	}
	switch ev.Type {
	case ir.EventStart:
		if ev.Response != nil {
			if err := validateUsage(ev.Response.Usage); err != nil {
				return err
			}
			e.id, e.model, e.created = ev.Response.ID, ev.Response.Model, ev.Response.Created
			e.usage = ev.Response.Usage
		}
		return e.startStream()
	case ir.EventBlockStart:
		if ev.Block == nil {
			return nil
		}
		switch ev.Block.Type {
		case ir.BlockText, ir.BlockRefusal:
			item, err := e.startItem(ev.Index, ev.Block.Type)
			if err != nil {
				return err
			}
			item.kind = "message"
			item.partType = "output_text"
			if ev.Block.Type == ir.BlockRefusal {
				item.partType = "refusal"
			}
			if err := e.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item": map[string]any{
					"type": "message", "id": item.id, "status": "in_progress",
					"role": "assistant", "content": []any{},
				},
			}); err != nil {
				return err
			}
			part := map[string]any{"type": item.partType, "text": "", "annotations": []any{}, "logprobs": []any{}}
			if item.partType == "refusal" {
				part = map[string]any{"type": "refusal", "refusal": ""}
			}
			if err := e.emit("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
				"part": part,
			}); err != nil {
				return err
			}
			item.partOpen = true
			initial := ev.Block.Text
			if item.partType == "refusal" {
				initial = ev.Block.Refusal
			}
			return e.appendText(item, initial)
		case ir.BlockToolCall:
			tc := ev.Block.ToolCall
			item, err := e.startItem(ev.Index, ev.Block.Type)
			if err != nil {
				return err
			}
			item.kind = "function_call"
			if tc != nil {
				item.name = tc.Name
				item.callID = tc.ID
				if err := e.retain(len(tc.ID)+len(tc.Name), "function identity"); err != nil {
					return err
				}
				if len(tc.Arguments) > 0 && string(tc.Arguments) != "{}" {
					if err := e.retain(len(tc.Arguments), "function arguments"); err != nil {
						return err
					}
					item.args.Write(tc.Arguments)
				}
			}
			return e.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": item.id, "call_id": item.callID,
					"name": item.name, "arguments": "", "status": "in_progress",
				},
			})
		case ir.BlockReasoning, ir.BlockRedactedReasoning:
			item, err := e.startItem(ev.Index, ev.Block.Type)
			if err != nil {
				return err
			}
			item.kind = "reasoning"
			initial := ""
			if ev.Block.Reasoning != nil {
				item.encrypted = ev.Block.Reasoning.Encrypted
				if err := e.retain(len(item.encrypted), "reasoning artifact"); err != nil {
					return err
				}
				initial = ev.Block.Reasoning.Text
				if initial == "" && len(ev.Block.Reasoning.Summary) > 0 {
					initial = strings.Join(ev.Block.Reasoning.Summary, "\n")
				}
			}
			if err := e.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item": map[string]any{
					"type": "reasoning", "id": item.id, "summary": []any{}, "status": "in_progress",
				},
			}); err != nil {
				return err
			}
			return e.appendReasoning(item, initial)
		default:
			// No Responses item shape exists for this assistant block. Do not
			// allocate an output index: a skipped item would leave a gap in
			// the client's contiguous output_index sequence.
			return nil
		}
	case ir.EventBlockDelta:
		item := e.items[ev.Index]
		if item == nil || ev.Delta == nil {
			return nil
		}
		switch {
		case ev.Delta.Text != "":
			if item.kind == "message" {
				return e.appendText(item, ev.Delta.Text)
			}
		case ev.Delta.Arguments != "":
			if item.kind == "function_call" {
				if err := e.retain(len(ev.Delta.Arguments), "function arguments"); err != nil {
					return err
				}
				item.args.WriteString(ev.Delta.Arguments)
				return e.emit("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "sequence_number": e.nextSeq(),
					"item_id": item.id, "output_index": item.outputIndex, "delta": ev.Delta.Arguments,
				})
			}
		case ev.Delta.Reasoning != "":
			if item.kind == "reasoning" {
				return e.appendReasoning(item, ev.Delta.Reasoning)
			}
		}
	case ir.EventBlockEnd:
		item := e.items[ev.Index]
		if item == nil {
			return nil
		}
		if err := e.closeItem(item); err != nil {
			return err
		}
		delete(e.open, ev.Index)
		return nil
	case ir.EventMessageDelta:
		if ev.Finish != nil {
			e.finish = ev.Finish.Reason
		}
		if ev.Usage != nil {
			if err := validateUsage(*ev.Usage); err != nil {
				return err
			}
			e.usage = *ev.Usage
		}
	case ir.EventDone:
		return e.finishResponse()
	case ir.EventError:
		if ev.Error != nil {
			e.failed = ev.Error
			if err := e.emit("error", map[string]any{
				"type": "error", "sequence_number": e.nextSeq(),
				"code": ev.Error.Code, "message": ev.Error.Message, "param": nil,
			}); err != nil {
				return err
			}
		}
		return e.finishResponse()
	}
	return nil
}

func (e *ResponsesStreamEncoder) finishResponse() error {
	if e.doneSent {
		return nil
	}
	if err := e.startStream(); err != nil {
		return err
	}
	e.doneSent = true
	status := "completed"
	switch {
	case e.failed != nil:
		status = "failed"
	case e.finish == ir.FinishCancelled:
		status = "cancelled"
	case e.finish == ir.FinishLength || e.finish == ir.FinishContentFilter || e.finish == ir.FinishOther:
		status = "incomplete"
	}
	response, err := e.responseObject(status, e.outputItems())
	if err != nil {
		return err
	}
	return e.emit("response."+status, map[string]any{
		"type": "response." + status, "sequence_number": e.nextSeq(),
		"response": response,
	})
}

func (e *ResponsesStreamEncoder) responseObject(status string, output []map[string]any) (map[string]any, error) {
	if output == nil {
		output = []map[string]any{}
	}
	obj := map[string]any{
		"id": e.id, "object": "response", "created_at": e.created, "status": status,
		"error": nil, "incomplete_details": nil, "instructions": nil,
		"max_output_tokens": nil, "model": e.model, "output": output,
		"parallel_tool_calls": true, "previous_response_id": nil, "reasoning": nil,
		"store": false, "temperature": nil,
		"text":        map[string]any{"format": map[string]any{"type": "text"}},
		"tool_choice": "auto", "tools": []any{}, "top_p": nil, "truncation": "disabled",
	}
	if status == "incomplete" {
		reason := ""
		switch e.finish {
		case ir.FinishContentFilter:
			reason = "content_filter"
		case ir.FinishLength:
			reason = "max_output_tokens"
		}
		if reason != "" {
			obj["incomplete_details"] = map[string]any{"reason": reason}
		}
	}
	if status == "failed" && e.failed != nil {
		code := e.failed.Code
		if code == "" {
			code = "server_error"
		}
		obj["error"] = map[string]any{"code": code, "message": e.failed.Message}
	}
	if e.usage != (ir.Usage{}) {
		usage, err := encodeResponsesUsage(e.usage)
		if err != nil {
			return nil, err
		}
		obj["usage"] = usage
	}
	return obj, nil
}

func (e *ResponsesStreamEncoder) outputItems() []map[string]any {
	var out []map[string]any
	for _, idx := range e.order {
		item := e.items[idx]
		if item == nil {
			continue
		}
		switch item.kind {
		case "message":
			part := map[string]any{"type": "output_text", "text": item.text.String(), "annotations": []any{}, "logprobs": []any{}}
			if item.partType == "refusal" {
				part = map[string]any{"type": "refusal", "refusal": item.refusal.String()}
			}
			out = append(out, map[string]any{
				"type": "message", "id": item.id, "status": "completed", "role": "assistant",
				"content": []any{part},
			})
		case "function_call":
			args := item.args.String()
			if args == "" {
				args = "{}"
			}
			out = append(out, map[string]any{
				"type": "function_call", "id": item.id, "call_id": item.callID,
				"name": item.name, "arguments": args, "status": "completed",
			})
		case "reasoning":
			out = append(out, reasoningItem(item, item.reasoning.String()))
		}
	}
	return out
}

// closeItem emits the normal completion frames for one open Responses item.
// The caller removes the item from the open set only after every frame lands.
func (e *ResponsesStreamEncoder) closeItem(item *responsesItemState) error {
	switch item.kind {
	case "message":
		if item.partType == "refusal" {
			text := item.refusal.String()
			if err := e.emit("response.refusal.done", map[string]any{
				"type": "response.refusal.done", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
				"refusal": text,
			}); err != nil {
				return err
			}
			if err := e.emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
				"part": map[string]any{"type": "refusal", "refusal": text},
			}); err != nil {
				return err
			}
			item.partOpen = false
			return e.emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item": map[string]any{
					"type": "message", "id": item.id, "status": "completed",
					"role":    "assistant",
					"content": []any{map[string]any{"type": "refusal", "refusal": text}},
				},
			})
		}
		text := item.text.String()
		if err := e.emit("response.output_text.done", map[string]any{
			"type": "response.output_text.done", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
			"text": text, "logprobs": []any{},
		}); err != nil {
			return err
		}
		if err := e.emit("response.content_part.done", map[string]any{
			"type": "response.content_part.done", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}},
		}); err != nil {
			return err
		}
		item.partOpen = false
		return e.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.nextSeq(),
			"output_index": item.outputIndex,
			"item": map[string]any{
				"type": "message", "id": item.id, "status": "completed",
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{},
				}},
			},
		})
	case "function_call":
		args := item.args.String()
		if args == "" {
			args = "{}"
		}
		if err := e.emit("response.function_call_arguments.done", map[string]any{
			"type": "response.function_call_arguments.done", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "arguments": args,
		}); err != nil {
			return err
		}
		return e.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.nextSeq(),
			"output_index": item.outputIndex,
			"item": map[string]any{
				"type": "function_call", "id": item.id, "call_id": item.callID,
				"name": item.name, "arguments": args, "status": "completed",
			},
		})
	case "reasoning":
		text := item.reasoning.String()
		if item.summaryOpen {
			if err := e.emit("response.reasoning_summary_text.done", map[string]any{
				"type": "response.reasoning_summary_text.done", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
				"text": text,
			}); err != nil {
				return err
			}
			if err := e.emit("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": text},
			}); err != nil {
				return err
			}
			item.summaryOpen = false
		}
		return e.emit("response.output_item.done", map[string]any{
			"type": "response.output_item.done", "sequence_number": e.nextSeq(),
			"output_index": item.outputIndex,
			"item":         reasoningItem(item, text),
		})
	}
	return nil
}

// abortItem closes one Responses item after a target-side failure. It emits an
// empty close for a content part or reasoning summary the encoder opened, then
// the item done frame, and never flushes retained text, arguments or reasoning.
func (e *ResponsesStreamEncoder) abortItem(item *responsesItemState) error {
	switch item.kind {
	case "message":
		if item.partOpen {
			part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
			if item.partType == "refusal" {
				part = map[string]any{"type": "refusal", "refusal": ""}
			}
			if err := e.emit("response.content_part.done", map[string]any{
				"type": "response.content_part.done", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
				"part": part,
			}); err != nil {
				return err
			}
			item.partOpen = false
		}
	case "reasoning":
		if item.summaryOpen {
			if err := e.emit("response.reasoning_summary_part.done", map[string]any{
				"type": "response.reasoning_summary_part.done", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}); err != nil {
				return err
			}
			item.summaryOpen = false
		}
	}
	closed := map[string]any{"type": item.kind, "id": item.id, "status": "incomplete"}
	switch item.kind {
	case "message":
		closed["role"] = "assistant"
		closed["content"] = []any{}
	case "function_call":
		closed["call_id"] = item.callID
		closed["name"] = item.name
		closed["arguments"] = "{}"
	case "reasoning":
		closed["summary"] = []any{}
	}
	return e.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": e.nextSeq(),
		"output_index": item.outputIndex, "item": closed,
	})
}

// Abort closes every item this encoder opened after a target-side failure,
// without flushing retained payload. Chat completions need no close frame;
// Anthropic emits a content_block_stop; Responses emits an empty part done
// (when the part was opened) followed by an incomplete item done.
func (e *ResponsesStreamEncoder) Abort() error {
	for _, idx := range e.order {
		if !e.open[idx] {
			continue
		}
		item := e.items[idx]
		if item == nil {
			continue
		}
		if err := e.abortItem(item); err != nil {
			return err
		}
		delete(e.open, idx)
	}
	return nil
}

// reasoningItem renders one completed Responses reasoning item. An opaque
// encrypted artifact the source provided (Anthropic redacted_thinking or a
// Responses encrypted reasoning item) is preserved where the target accepts it.
func reasoningItem(item *responsesItemState, text string) map[string]any {
	summary := []any{}
	if text != "" {
		summary = []any{map[string]any{"type": "summary_text", "text": text}}
	}
	out := map[string]any{
		"type": "reasoning", "id": item.id, "status": "completed", "summary": summary,
	}
	if item.encrypted != "" {
		out["encrypted_content"] = item.encrypted
	}
	return out
}

// startItem allocates the next contiguous Responses output index for one
// canonical block. Only a block whose target shape exists may call it. The
// shared item bound and the per-item envelope charge keep the terminal
// document within ResponsesStreamFrameMaxBytes even when the source opens
// thousands of empty items.
func (e *ResponsesStreamEncoder) startItem(index int, kind ir.BlockType) (*responsesItemState, error) {
	if len(e.order) >= WireCollectionMax {
		return nil, fmt.Errorf("responses stream exceeds %d items", WireCollectionMax)
	}
	if err := e.retain(responsesStreamItemEnvelopeBytes, "item envelope"); err != nil {
		return nil, err
	}
	item := &responsesItemState{
		id:          e.newItemID(kind),
		outputIndex: e.outputIndex,
		kind:        string(kind),
	}
	e.outputIndex++
	e.items[index] = item
	e.open[index] = true
	e.order = append(e.order, index)
	return item, nil
}

// retain books bytes the terminal frame must carry. One aggregate budget
// covers every item, so the terminal document cannot exceed the relay's
// derived frame bound however many items the source opened.
func (e *ResponsesStreamEncoder) retain(n int, field string) error {
	if n <= 0 {
		return nil
	}
	if e.retained+n > ResponsesStreamRetainedMaxBytes {
		return fmt.Errorf("responses %s exceeds %d retained bytes", field, ResponsesStreamRetainedMaxBytes)
	}
	e.retained += n
	return nil
}

// appendText retains and emits one message text or refusal delta. A block-start
// payload and later deltas share the same path, so an initial value is never
// dropped when no delta follows.
func (e *ResponsesStreamEncoder) appendText(item *responsesItemState, delta string) error {
	if delta == "" {
		return nil
	}
	if item.partType == "refusal" {
		if err := e.retain(len(delta), "refusal"); err != nil {
			return err
		}
		item.refusal.WriteString(delta)
		return e.emit("response.refusal.delta", map[string]any{
			"type": "response.refusal.delta", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
			"delta": delta,
		})
	}
	if err := e.retain(len(delta), "output text"); err != nil {
		return err
	}
	item.text.WriteString(delta)
	return e.emit("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "sequence_number": e.nextSeq(),
		"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
		"delta": delta, "logprobs": []any{},
	})
}

// appendReasoning retains and emits one reasoning summary delta.
func (e *ResponsesStreamEncoder) appendReasoning(item *responsesItemState, delta string) error {
	if delta == "" {
		return nil
	}
	if !item.summaryOpen {
		if err := e.emit("response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}); err != nil {
			return err
		}
		item.summaryOpen = true
	}
	if err := e.retain(len(delta), "reasoning"); err != nil {
		return err
	}
	item.reasoning.WriteString(delta)
	return e.emit("response.reasoning_summary_text.delta", map[string]any{
		"type": "response.reasoning_summary_text.delta", "sequence_number": e.nextSeq(),
		"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
		"delta": delta,
	})
}

func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (e *ResponsesStreamEncoder) newItemID(kind ir.BlockType) string {
	switch kind {
	case ir.BlockToolCall:
		return "fc_" + randomID()
	case ir.BlockReasoning, ir.BlockRedactedReasoning:
		return "rs_" + randomID()
	default:
		return "msg_" + randomID()
	}
}

func (e *ResponsesStreamEncoder) nextSeq() int64 {
	n := e.seq
	e.seq++
	return n
}

func (e *ResponsesStreamEncoder) emit(name string, obj any) error {
	payload, err := sse.MarshalWireJSON(obj)
	if err != nil {
		return err
	}
	frame := make([]byte, 0, len(name)+len(payload)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, '\n')
	frame = append(frame, "data: "...)
	frame = append(frame, payload...)
	frame = append(frame, '\n', '\n')
	if len(frame) > ResponsesStreamFrameMaxBytes {
		return fmt.Errorf("responses frame exceeds %d bytes", ResponsesStreamFrameMaxBytes)
	}
	if _, err := e.w.Write(frame); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

// Close finishes the stream if the caller did not deliver EventDone.
func (e *ResponsesStreamEncoder) Close() error { return e.finishResponse() }
