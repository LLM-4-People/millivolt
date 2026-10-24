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
	Refusal        string          `json:"refusal"`
	Arguments      string          `json:"arguments"`
	Code           string          `json:"code"`
	Message        string          `json:"message"`
	Param          string          `json:"param"`
}

// ResponsesStreamDecoder converts Responses SSE events into canonical events.
// toolArgumentCoverage is the bounded prefix of one tool call's emitted
// argument bytes. partial means more bytes reached the target than this
// decoder retains, so a later full value cannot be reconciled safely.
type toolArgumentCoverage struct {
	emitted strings.Builder
	partial bool
}

type ResponsesStreamDecoder struct {
	r        *sseReader
	q        eventQueue
	started  bool
	terminal bool

	// observe receives every raw source event name and data payload before
	// it is parsed. The translation facade uses it for source-only accounting
	// (cost keys the target wire cannot carry); decoding itself never depends
	// on it.
	observe func(string, []byte)

	id      string
	model   string
	created int64

	nextIndex int
	// usageReported distinguishes a source that reported usage (even all
	// zeros) from one that never did. Only a reported usage is forwarded.
	usageReported bool
	itemBlock     map[int]int
	partBlock     map[[2]int]int
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
	// argsEmitted retains the argument bytes already emitted for one item
	// identity so a later authoritative done/terminal value can append only a
	// strict extension. A provider that emits more than the bounded coverage
	// keeps its streamed bytes and stops reconciling; the aggregate stays
	// bounded by toolInitialBytesMax.
	argsEmitted      map[string]*toolArgumentCoverage
	argsEmittedBytes int
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
	// partSeed holds an announced content part whose start payload was empty.
	// Its target block is deferred until the first delta, done event or
	// terminal document, so a later announced part does not force an empty
	// block ahead of an earlier terminal-only part. partPosition is the
	// announced wire position used when the deferred block finally starts.
	partSeed     map[string]ir.Block
	partPosition map[string][2]int
	// emittedText records the bytes already emitted for one announced channel
	// (content part or reasoning summary part). The terminal document is
	// authoritative, so a longer value contributes only its missing suffix;
	// an identical or shorter value is never re-emitted. Keyed by the part
	// identity (and summary index), never by wire position. A builder makes
	// incremental delta appends amortized instead of copying the whole channel
	// on every token; retainedText bounds the aggregate.
	emittedText  map[string]*strings.Builder
	retainedText int
	// partOwner maps a content or summary part key to the logical item it
	// belongs to, and partKind maps a content part key to its target channel.
	// Terminal reconciliation compares the whole emitted channel with the
	// whole terminal channel, so a provider that merges or splits parts cannot
	// duplicate streamed bytes.
	partOwner map[string]string
	partKind  map[string]partChannel
	// warnings records streaming approximations that cannot be represented on
	// the committed target wire.
	warnings ir.Warnings
	// terminalPending holds authoritative done-event values whose target block
	// already ended or was never opened. They are emitted after every streamed
	// block closes (closeAll), so target lifecycles never nest.
	terminalPending []terminalEmission
	open            map[int]bool

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
		argsEmitted:      map[string]*toolArgumentCoverage{},
		announcedOutputs: map[int]bool{},
		announcedItems:   map[string]bool{},
		announcedParts:   map[string]bool{},
		itemBlockByKey:   map[string]int{},
		partBlockByKey:   map[string]int{},
		keyByOutput:      map[int]string{},
		keyByPart:        map[[2]int]string{},
		itemIDByOutput:   map[int]string{},
		partSeed:         map[string]ir.Block{},
		partPosition:     map[string][2]int{},
		emittedText:      map[string]*strings.Builder{},
		partOwner:        map[string]string{},
		partKind:         map[string]partChannel{},
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
			d.observe(raw.Name, raw.Data)
		}
		var ev responsesStreamEvent
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			return nil, err
		}
		// A terminal document can omit its top-level type; the SSE event name
		// is then the only classifier. Resolve it before the switch so a
		// type-less response.completed is terminal rather than debris.
		if ev.Type == "" {
			ev.Type = raw.Name
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
				d.usageReported = true
			}
			d.started = true
			u := d.usage
			d.q.push(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
				ID: d.id, Model: d.model, Created: d.created, Usage: u,
				Metadata: resp.Metadata, ServiceTier: resp.ServiceTier,
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
				// An announced reasoning item opens its target block now so
				// summary/content deltas have a stable index. A provider that
				// announces an empty item, closes it, then delivers every
				// summary part only in the terminal document leaves one empty
				// Anthropic thinking block. Suppressing it would require
				// deferring the start or buffering a start already sent, which
				// risks ordering and signature loss for a cosmetic residual;
				// TestResponsesEmptyAnnouncedReasoningPinsThinkingBlock pins
				// the accepted behavior.
				d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &ir.Block{
					Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
						ID: item.ID, Encrypted: item.EncryptedContent, Provider: ir.Responses,
					},
				}})
				// The added item may already carry summary text. Emit each
				// part in order and record its coverage so a later delta,
				// done event or terminal copy adds only the missing suffix.
				for summaryIndex, part := range item.Summary {
					if part.Text == "" {
						continue
					}
					partKey := summaryPartKey(key, summaryIndex)
					if _, err := d.announcePart(partKey); err != nil {
						return nil, err
					}
					d.partOwner[partKey] = key
					d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: part.Text}})
					if err := d.setEmittedText(partKey, part.Text); err != nil {
						return nil, err
					}
				}
			case "message":
				// Content parts arrive in response.content_part.added. The
				// item id is still announced so a terminal document that
				// repeats the item at another array position is recognized.
				if err := checkNativeIdentity("message id", item.ID); err != nil {
					return nil, err
				}
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
			block, supported := responsesPartBlock(part.Type, part.Text, part.Refusal)
			if !supported {
				// Allocate the block only after the part type is accepted: an
				// unsupported part must not consume an index or leave an open
				// block whose stop would reach the client without a start.
				// Streaming cannot report the loss, so it is dropped silently.
				continue
			}
			if err := checkNativeIdentity("item id", ev.ItemID); err != nil {
				return nil, err
			}
			itemID := ev.ItemID
			if itemID == "" {
				itemID = d.itemIDByOutput[ev.OutputIndex]
			}
			if err := checkNativeIdentity("item id", itemID); err != nil {
				return nil, err
			}
			key := contentPartKey(itemID, ev.OutputIndex, ev.ContentIndex)
			admitted, err := d.announcePart(key)
			if err != nil {
				return nil, err
			}
			if !admitted {
				continue
			}
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			d.keyByPart[pos] = key
			d.partPosition[key] = pos
			d.itemIDByOutput[ev.OutputIndex] = itemID
			d.partOwner[key] = streamItemKey(itemID, ev.OutputIndex)
			d.partKind[key] = partChannelForBlock(block.Type)
			start := ""
			switch block.Type {
			case ir.BlockText:
				start = block.Text
			case ir.BlockRefusal:
				start = block.Refusal
			case ir.BlockReasoning:
				if block.Reasoning != nil {
					start = block.Reasoning.Text
				}
			}
			if start == "" {
				// Defer the empty block until it has content or a boundary.
				// This keeps terminal ordering possible when an earlier
				// terminal-only part was never announced.
				d.partSeed[key] = block
				continue
			}
			idx := d.newBlock()
			d.partBlock[pos] = idx
			d.partBlockByKey[key] = idx
			d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &block})
			// A non-empty start payload is already on the target wire as part
			// of the block start. Record it as emitted so a delta or terminal
			// document that repeats the prefix contributes only new bytes.
			if err := d.setEmittedText(key, start); err != nil {
				return nil, err
			}
		case "response.output_text.delta", "response.refusal.delta":
			// An empty delta carries no bytes and must not materialize a
			// deferred part: doing so would fix its target position before any
			// content exists, forcing an earlier terminal-only part behind it.
			if ev.Delta == "" {
				continue
			}
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			key := d.keyByPart[pos]
			if key == "" {
				continue
			}
			idx, ok := d.startPartBlock(key)
			if !ok {
				continue
			}
			if err := d.appendEmittedText(key, ev.Delta); err != nil {
				return nil, err
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Text: ev.Delta}})
		case "response.output_text.done", "response.refusal.done":
			// The done payload is the authoritative full part value. Emit it
			// only when no delta carried bytes; repeating streamed text would
			// duplicate the answer.
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			itemID := d.itemIDByOutput[ev.OutputIndex]
			key, ok := d.keyByPart[pos]
			if !ok {
				key = contentPartKey(itemID, ev.OutputIndex, ev.ContentIndex)
			}
			text := ev.Text
			block := ir.Block{Type: ir.BlockText}
			if ev.Type == "response.refusal.done" {
				text = ev.Refusal
				block = ir.Block{Type: ir.BlockRefusal}
			}
			emit := pendingValueSuffix(d.emittedValue(key), text)
			if emit == "" {
				continue
			}
			if _, announced := d.announcedParts[key]; !announced {
				admitted, err := d.announcePart(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
			}
			if _, owned := d.partOwner[key]; !owned {
				d.partOwner[key] = streamItemKey(itemID, ev.OutputIndex)
			}
			if _, kind := d.partKind[key]; !kind {
				d.partKind[key] = partChannelForBlock(block.Type)
			}
			if idx, ok := d.startPartBlock(key); ok {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Text: emit}})
			} else {
				d.terminalPending = append(d.terminalPending, terminalEmission{
					block: block,
					delta: &ir.Delta{Text: emit},
				})
			}
			if err := d.setEmittedText(key, text); err != nil {
				return nil, err
			}
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
			key := d.keyByOutput[ev.OutputIndex]
			if ev.Delta != "" && key != "" {
				partKey := summaryPartKey(key, ev.SummaryIndex)
				if _, err := d.announcePart(partKey); err != nil {
					return nil, err
				}
				d.partOwner[partKey] = key
				if err := d.appendEmittedText(partKey, ev.Delta); err != nil {
					return nil, err
				}
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: ev.Delta}})
			}
		case "response.reasoning_summary_part.added":
			// Some providers put the summary text in the part-start payload
			// and stream no delta for it.
			var part struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(ev.Part, &part); err != nil {
				return nil, err
			}
			key := d.keyByOutput[ev.OutputIndex]
			idx, ok := d.itemBlock[ev.OutputIndex]
			if part.Text == "" || !ok || key == "" {
				continue
			}
			partKey := summaryPartKey(key, ev.SummaryIndex)
			if _, err := d.announcePart(partKey); err != nil {
				return nil, err
			}
			d.partOwner[partKey] = key
			emit := pendingValueSuffix(d.emittedValue(partKey), part.Text)
			if emit == "" {
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			if err := d.setEmittedText(partKey, part.Text); err != nil {
				return nil, err
			}
		case "response.reasoning_summary_text.done":
			// The done payload is authoritative per summary part. Emit only
			// the bytes no delta or start payload already covered.
			key := d.keyByOutput[ev.OutputIndex]
			idx, ok := d.itemBlock[ev.OutputIndex]
			if ev.Text == "" || !ok || key == "" {
				continue
			}
			partKey := summaryPartKey(key, ev.SummaryIndex)
			if _, err := d.announcePart(partKey); err != nil {
				return nil, err
			}
			d.partOwner[partKey] = key
			emit := pendingValueSuffix(d.emittedValue(partKey), ev.Text)
			if emit == "" {
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			if err := d.setEmittedText(partKey, ev.Text); err != nil {
				return nil, err
			}
		case "response.reasoning_text.delta":
			// reasoning_text is a content part of the reasoning item, so its
			// deltas belong to the part block the added event opened (C12).
			// reasoning_summary_text has no content part and stays on the item
			// block. An empty delta must not materialize a deferred part. A
			// provider is not required to announce the part first: derive the
			// identity exactly like the done fallback and emit on the open
			// reasoning item block when the part block was never seeded.
			if ev.Delta == "" {
				continue
			}
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			itemID := d.itemIDByOutput[ev.OutputIndex]
			key := d.keyByPart[pos]
			if key == "" {
				key = contentPartKey(itemID, ev.OutputIndex, ev.ContentIndex)
			}
			if _, announced := d.announcedParts[key]; !announced {
				admitted, err := d.announcePart(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
			}
			if _, owned := d.partOwner[key]; !owned {
				d.partOwner[key] = streamItemKey(itemID, ev.OutputIndex)
			}
			if _, kind := d.partKind[key]; !kind {
				d.partKind[key] = partChannelReasoning
			}
			// The wire delta is incremental, exactly like output_text.delta:
			// append it verbatim. Only the done payload is cumulative, and its
			// own handler reconciles against the recorded coverage.
			delta := ev.Delta
			if err := d.appendEmittedText(key, delta); err != nil {
				return nil, err
			}
			if idx, ok := d.startPartBlock(key); ok {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: delta}})
			} else if idx, ok := d.itemBlock[ev.OutputIndex]; ok && d.open[idx] {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: delta}})
			} else {
				d.terminalPending = append(d.terminalPending, terminalEmission{
					block: ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses}},
					delta: &ir.Delta{Reasoning: delta},
				})
			}
		case "response.reasoning_text.done":
			// The done payload is authoritative. Emit it only when no delta
			// carried the part's bytes. A provider may omit the part-start
			// event entirely, so the reasoning item block is the fallback
			// channel; the part identity is still recorded so the terminal
			// document cannot repeat the value.
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			itemID := d.itemIDByOutput[ev.OutputIndex]
			key, ok := d.keyByPart[pos]
			if !ok {
				key = contentPartKey(itemID, ev.OutputIndex, ev.ContentIndex)
			}
			if _, announced := d.announcedParts[key]; !announced {
				admitted, err := d.announcePart(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
			}
			if _, owned := d.partOwner[key]; !owned {
				d.partOwner[key] = streamItemKey(itemID, ev.OutputIndex)
			}
			if _, kind := d.partKind[key]; !kind {
				d.partKind[key] = partChannelReasoning
			}
			emit := pendingValueSuffix(d.emittedValue(key), ev.Text)
			if emit == "" {
				continue
			}
			if idx, ok := d.startPartBlock(key); ok {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			} else if idx, ok := d.itemBlock[ev.OutputIndex]; ok && d.open[idx] {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			} else {
				d.terminalPending = append(d.terminalPending, terminalEmission{
					block: ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses}},
					delta: &ir.Delta{Reasoning: emit},
				})
			}
			if err := d.setEmittedText(key, ev.Text); err != nil {
				return nil, err
			}
		case "response.content_part.done":
			pos := [2]int{ev.OutputIndex, ev.ContentIndex}
			if err := checkNativeIdentity("item id", ev.ItemID); err != nil {
				return nil, err
			}
			itemID := ev.ItemID
			if itemID == "" {
				itemID = d.itemIDByOutput[ev.OutputIndex]
			}
			key := d.keyByPart[pos]
			if key == "" {
				key = contentPartKey(itemID, ev.OutputIndex, ev.ContentIndex)
			}
			var part struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			}
			trimmed := bytes.TrimSpace(ev.Part)
			hasPart := len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
			if hasPart {
				if err := json.Unmarshal(trimmed, &part); err != nil {
					return nil, err
				}
			}
			// Map the done payload through the same owner as the added
			// event. A reasoning_text part must stay on the reasoning
			// channel: once emitted as text it becomes visible answer
			// content and, on Anthropic, a text delta inside a thinking
			// block. A type-less done payload reuses the channel the added
			// event announced.
			var block ir.Block
			var supported bool
			if hasPart && part.Type != "" {
				block, supported = responsesPartBlock(part.Type, part.Text, part.Refusal)
			} else {
				typ := "text"
				switch d.partKind[key] {
				case partChannelRefusal:
					typ = "refusal"
				case partChannelReasoning:
					typ = "reasoning_text"
				}
				block, supported = responsesPartBlock(typ, part.Text, part.Refusal)
			}
			value := ""
			if supported {
				value = responsesPartValue(block)
			}
			if value != "" {
				if _, announced := d.announcedParts[key]; !announced {
					admitted, err := d.announcePart(key)
					if err != nil {
						return nil, err
					}
					if !admitted {
						continue
					}
				}
				if _, owned := d.partOwner[key]; !owned {
					d.partOwner[key] = streamItemKey(itemID, ev.OutputIndex)
				}
				if _, kind := d.partKind[key]; !kind {
					d.partKind[key] = partChannelForBlock(block.Type)
				}
				emit := pendingValueSuffix(d.emittedValue(key), value)
				if emit != "" {
					// A queued block is a type-only start template: the
					// value travels in the delta, so a block that also
					// carried it would emit the same text twice.
					d.appendContentValue(channelPart{
						key: key, value: emit, channel: partChannelForBlock(block.Type),
						block: responsesPartBlockStart(block),
					}, &d.terminalPending)
					if err := d.setEmittedText(key, value); err != nil {
						return nil, err
					}
				}
			}
			// A deferred part whose payload is absent or empty never starts a
			// target block; a started part is closed here because done ends it.
			delete(d.partSeed, key)
			if idx, started := d.partBlockByKey[key]; started && d.open[idx] {
				delete(d.partBlock, pos)
				delete(d.open, idx)
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			}
		case "response.output_item.done":
			var item responsesWireItem
			if err := json.Unmarshal(ev.Item, &item); err != nil {
				return nil, err
			}
			typ := item.Type
			if typ == "" && item.Role != "" {
				typ = "message"
			}
			if typ == "function_call" {
				idx, ok := d.itemBlock[ev.OutputIndex]
				if !ok {
					continue
				}
				key := d.keyByOutput[ev.OutputIndex]
				if err := d.flushToolArguments(key, idx, item.Arguments); err != nil {
					return nil, err
				}
				delete(d.itemBlock, ev.OutputIndex)
				delete(d.open, idx)
				d.clearToolState(key)
				d.openToolItems--
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if typ == "reasoning" {
				idx, ok := d.itemBlock[ev.OutputIndex]
				if !ok {
					continue
				}
				if err := d.applyReasoningContent(item, d.keyByOutput[ev.OutputIndex], ev.OutputIndex, &d.terminalPending); err != nil {
					return nil, err
				}
				delete(d.itemBlock, ev.OutputIndex)
				delete(d.open, idx)
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if typ == "message" {
				if err := d.applyMessageContent(item, ev.OutputIndex, &d.terminalPending); err != nil {
					return nil, err
				}
				// Some providers skip content_part.done; close any part block
				// that is still open for this output item. A deferred empty part
				// never reached the target, so discard its seed rather than
				// opening and immediately closing an empty block.
				for pos, key := range d.keyByPart {
					if pos[0] != ev.OutputIndex {
						continue
					}
					idx, started := d.partBlockByKey[key]
					if !started || !d.open[idx] {
						delete(d.partSeed, key)
						continue
					}
					delete(d.partBlock, pos)
					delete(d.open, idx)
					d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
				}
			}
		case "response.function_call_arguments.done":
			idx, ok := d.itemBlock[ev.OutputIndex]
			if !ok {
				continue
			}
			if err := d.flushToolArguments(d.keyByOutput[ev.OutputIndex], idx, ev.Arguments); err != nil {
				return nil, err
			}
		case "response.reasoning_summary_part.done":
			var part struct {
				Text string `json:"text"`
			}
			trimmed := bytes.TrimSpace(ev.Part)
			if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) {
				if err := json.Unmarshal(trimmed, &part); err != nil {
					return nil, err
				}
			}
			text := part.Text
			if text == "" {
				text = ev.Text
			}
			key := d.keyByOutput[ev.OutputIndex]
			idx, ok := d.itemBlock[ev.OutputIndex]
			if text == "" || !ok || key == "" {
				continue
			}
			partKey := summaryPartKey(key, ev.SummaryIndex)
			if _, err := d.announcePart(partKey); err != nil {
				return nil, err
			}
			d.partOwner[partKey] = key
			emit := pendingValueSuffix(d.emittedValue(partKey), text)
			if emit != "" {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			}
			if err := d.setEmittedText(partKey, text); err != nil {
				return nil, err
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
				d.usageReported = true
			}
			d.terminal = true
			if err := d.closeAll(resp.Output); err != nil {
				return nil, err
			}
			if ev.Type == "response.failed" {
				e := &ir.Error{Message: "upstream response failed"}
				if typ, code, msg := metrics.ParseErrorComponents(resp.Error); typ != "" || code != "" || msg != "" {
					e.Type, e.Code = typ, code
					if msg != "" {
						e.Message = msg
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
			delta := &ir.Event{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: finish}}
			if d.usageReported {
				u := d.usage
				delta.Usage = &u
			}
			d.q.push(delta)
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
	// Apply the authoritative terminal payload in terminal document order.
	// Announced blocks that already carry bytes are filled in place; every
	// block that still has to start (an unannounced item/part, or a deferred
	// empty announcement) is queued and started only after all streamed blocks
	// close. Done-event payloads queued earlier share that tail.
	pending := d.terminalPending
	d.terminalPending = nil
	more, err := d.collectTerminalOutput(output)
	if err != nil {
		return err
	}
	pending = append(pending, more...)
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
			if err := d.flushToolArguments(key, idx, ""); err != nil {
				return err
			}
		}
	}
	// Close every block the source actually opened before starting queued
	// terminal blocks, so a target block sequence never nests two lifecycles.
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
	clear(d.partSeed)
	clear(d.partPosition)
	clear(d.emittedText)
	d.retainedText = 0
	d.openToolItems = 0
	for _, block := range pending {
		d.syntheticBlock(block.block, block.delta)
	}
	return nil
}

// partChannel is one independently concatenated content channel of an item.
// Terminal reconciliation never compares text bytes with refusal or reasoning
// bytes, and reasoning summary parts form their own channel.
type partChannel uint8

const (
	partChannelText partChannel = iota
	partChannelRefusal
	partChannelReasoning
)

// responsesPartBlock maps one Responses content-part payload to its canonical
// block. It reports false for a provider part with no target representation.
// The added and done events share this owner so a part's channel cannot change
// between them.
func responsesPartBlock(typ, text, refusal string) (ir.Block, bool) {
	switch typ {
	case "output_text", "input_text", "text":
		return ir.Block{Type: ir.BlockText, Text: text}, true
	case "refusal":
		return ir.Block{Type: ir.BlockRefusal, Refusal: refusal}, true
	case "reasoning_text":
		return ir.Block{
			Type: ir.BlockReasoning,
			Reasoning: &ir.Reasoning{
				Text: text, Provider: ir.Responses,
			},
		}, true
	}
	return ir.Block{}, false
}

// responsesPartBlockStart returns the type-only block a queued done payload
// starts. The payload's value is emitted as a delta after the start, so the
// block must not repeat it in its start payload.
func responsesPartBlockStart(block ir.Block) ir.Block {
	switch block.Type {
	case ir.BlockReasoning:
		return ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses}}
	case ir.BlockRefusal:
		return ir.Block{Type: ir.BlockRefusal}
	}
	return ir.Block{Type: ir.BlockText}
}

// responsesPartValue returns the text a mapped part carries on its channel.
func responsesPartValue(block ir.Block) string {
	switch block.Type {
	case ir.BlockRefusal:
		return block.Refusal
	case ir.BlockReasoning:
		if block.Reasoning != nil {
			return block.Reasoning.Text
		}
		return ""
	}
	return block.Text
}

func partChannelForBlock(t ir.BlockType) partChannel {
	switch t {
	case ir.BlockRefusal:
		return partChannelRefusal
	case ir.BlockReasoning:
		return partChannelReasoning
	}
	return partChannelText
}

// channelPart is one part of a concatenated channel. key is the target part
// identity (content part key or summary part key), index is its source order,
// and order is its terminal-document position.
type channelPart struct {
	key     string
	index   int
	value   string
	channel partChannel
	block   ir.Block
	order   int
}

// channelReconciliation is the terminal channel tail that still has to be
// emitted. approximated reports that the terminal partition or text differs
// from what was streamed, so the target wire is a best-effort representation.
type channelReconciliation struct {
	emit         []channelPart
	approximated bool
}

// reconcileChannel compares the concatenated bytes already emitted for one
// channel with the concatenated terminal bytes. A terminal document that only
// re-partitions the same text contributes nothing; a longer terminal
// contributes only the missing tail once, preserving the terminal partition
// for the new bytes. A shorter or diverged terminal never repeats a visible
// prefix; only a strict continuation of the same part or a genuinely new part
// contributes bytes, and the mismatch is recorded as an approximation.
func reconcileChannel(emitted, terminal []channelPart) channelReconciliation {
	var res channelReconciliation
	if len(terminal) == 0 {
		return res
	}
	var emittedBuilder, terminalBuilder strings.Builder
	for _, part := range emitted {
		emittedBuilder.WriteString(part.value)
	}
	for _, part := range terminal {
		terminalBuilder.WriteString(part.value)
	}
	emittedTotal, terminalTotal := emittedBuilder.String(), terminalBuilder.String()
	if emittedTotal == terminalTotal {
		return res
	}
	if strings.HasPrefix(terminalTotal, emittedTotal) {
		covered := len(emittedTotal)
		// A terminal part that straddles the end of the streamed bytes is the
		// continuation of the LAST emitted part, whatever terminal index it
		// carries. Keeping its terminal key would resolve to an earlier open
		// block and append the tail there (the C1 text-order corruption).
		// Fully new tail parts keep their own keys and order.
		lastKey := ""
		if len(emitted) > 0 {
			lastKey = emitted[len(emitted)-1].key
		}
		offset := 0
		for _, part := range terminal {
			start := offset
			offset += len(part.value)
			if offset <= covered {
				continue
			}
			if start < covered {
				part.value = part.value[covered-start:]
				part.key = lastKey
			}
			if part.value != "" {
				res.emit = append(res.emit, part)
			}
		}
		return res
	}
	res.approximated = true
	// A partition-only difference in whitespace is already represented by the
	// streamed words; emitting the terminal form would repeat them.
	if strings.Join(strings.Fields(emittedTotal), "") == strings.Join(strings.Fields(terminalTotal), "") {
		return res
	}
	emittedByIndex := make(map[int]string, len(emitted))
	for _, part := range emitted {
		emittedByIndex[part.index] = part.value
	}
	for _, part := range terminal {
		if part.value == "" {
			continue
		}
		if emittedValue, ok := emittedByIndex[part.index]; ok {
			// Only a strict continuation of the same streamed part is a
			// genuinely new tail. A shorter, reordered or diverged value is
			// not representable without repeating visible bytes.
			if strings.HasPrefix(part.value, emittedValue) {
				suffix := part.value[len(emittedValue):]
				if suffix != "" && !strings.Contains(emittedTotal, suffix) {
					part.value = suffix
					res.emit = append(res.emit, part)
				}
			}
			continue
		}
		if strings.Contains(emittedTotal, part.value) {
			continue
		}
		res.emit = append(res.emit, part)
	}
	return res
}

// partKeyIndex parses the trailing integer identity from a content or summary
// part key. The key owner appends it last, so an item id containing colons is
// harmless.
func partKeyIndex(key string) (int, bool) {
	i := strings.LastIndexByte(key, ':')
	if i < 0 || i == len(key)-1 {
		return 0, false
	}
	index, err := strconv.Atoi(key[i+1:])
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// emittedContentParts returns the non-empty streamed bytes for one item and
// content channel, ordered by the part index the client saw.
func (d *ResponsesStreamDecoder) emittedContentParts(itemKey string, channel partChannel) []channelPart {
	var out []channelPart
	for key, builder := range d.emittedText {
		value := builder.String()
		if value == "" || d.partOwner[key] != itemKey || d.partKind[key] != channel {
			continue
		}
		index, ok := partKeyIndex(key)
		if !ok {
			continue
		}
		out = append(out, channelPart{key: key, index: index, value: value, channel: channel, block: d.partSeed[key]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].index != out[j].index {
			return out[i].index < out[j].index
		}
		return out[i].key < out[j].key
	})
	return out
}

// emittedSummaryParts returns the non-empty streamed summary bytes for one
// reasoning item, ordered by summary index.
func (d *ResponsesStreamDecoder) emittedSummaryParts(itemKey string) []channelPart {
	var out []channelPart
	for key, builder := range d.emittedText {
		value := builder.String()
		if value == "" || d.partOwner[key] != itemKey {
			continue
		}
		index, ok := partKeyIndex(key)
		if !ok {
			continue
		}
		out = append(out, channelPart{key: key, index: index, value: value, channel: partChannelReasoning})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].index < out[j].index })
	return out
}

// hasEmittedParts reports whether any part of an item already reached the
// target wire. A terminal item that was announced with only empty parts still
// needs the announced identity, so this only gates the aggregate owner key.
func (d *ResponsesStreamDecoder) hasEmittedParts(itemKey string) bool {
	for key := range d.emittedText {
		if d.partOwner[key] == itemKey {
			return true
		}
	}
	return false
}

// terminalContentItemKey resolves the aggregate item identity of a terminal
// content item. The item id is stable across renumbering; when one side has no
// id, the announced output position identifies the same logical item.
func (d *ResponsesStreamDecoder) terminalContentItemKey(itemID string, outputIndex int) string {
	primary := streamItemKey(itemID, outputIndex)
	if d.hasEmittedParts(primary) {
		return primary
	}
	announcedID := d.itemIDByOutput[outputIndex]
	if itemID == "" || announcedID == "" || announcedID == itemID {
		positional := streamItemKey(announcedID, outputIndex)
		if d.hasEmittedParts(positional) {
			return positional
		}
	}
	return primary
}

// warnApproximated records one streaming approximation per field. The decoder
// runs before the target status line is committed for non-streaming bodies,
// but a streamed status is already sent, so this is decoder-owned diagnostic
// state rather than a header.
func (d *ResponsesStreamDecoder) warnApproximated(field, detail string) {
	for _, warning := range d.warnings {
		if warning.Field == field && warning.Detail == detail {
			return
		}
	}
	d.warnings.Add(ir.WarningApproximated, field, detail)
}

func (d *ResponsesStreamDecoder) reconcileChannel(field string, emitted, terminal []channelPart) channelReconciliation {
	res := reconcileChannel(emitted, terminal)
	if res.approximated {
		d.warnApproximated(field, "terminal partition differs from the streamed parts; emitted only non-overlapping bytes")
	}
	return res
}

func contentChannelDelta(channel partChannel, value string) *ir.Delta {
	if channel == partChannelReasoning {
		return &ir.Delta{Reasoning: value}
	}
	return &ir.Delta{Text: value}
}

// appendContentValue emits one reconciled content tail. It prefers the open
// block the client already has. A part that never reached the target wire,
// including a deferred announcement, is queued so it starts after every
// streamed block has closed; starting it here would fix its position ahead of
// an earlier terminal-only part.
func (d *ResponsesStreamDecoder) appendContentValue(part channelPart, pending *[]terminalEmission) {
	if part.value == "" {
		return
	}
	if idx, ok := d.partBlockByKey[part.key]; ok && d.open[idx] {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: contentChannelDelta(part.channel, part.value)})
		return
	}
	*pending = append(*pending, terminalEmission{
		block: part.block,
		delta: contentChannelDelta(part.channel, part.value),
	})
}

// appendReasoningValue emits one reconciled summary or reasoning-content tail
// on the open item block, or queues a fresh reasoning block after it closes.
func (d *ResponsesStreamDecoder) appendReasoningValue(item responsesWireItem, itemKey, value string, pending *[]terminalEmission) {
	if value == "" {
		return
	}
	if idx, ok := d.itemBlockByKey[itemKey]; ok && d.open[idx] {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: value}})
		return
	}
	*pending = append(*pending, terminalEmission{block: reasoningTerminalBlock(item), delta: &ir.Delta{Reasoning: value}})
}

// collectTerminalOutput walks the terminal document in its own order. It
// fills announced blocks that are still open, queues every block that must
// start fresh (unannounced parts/items and deferred empty announcements), and
// keeps each queued value's event position. The caller closes streamed blocks
// before emitting the queue; perfect ordering is impossible only when a later
// announced block already streamed bytes, because its target index is fixed.
func (d *ResponsesStreamDecoder) collectTerminalOutput(output []responsesWireItem) ([]terminalEmission, error) {
	var pending []terminalEmission
	for i, item := range output {
		typ := item.Type
		if typ == "" && item.Role != "" {
			typ = "message"
		}
		switch typ {
		case "function_call":
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
				if err := d.flushToolArguments(key, idx, item.Arguments); err != nil {
					return nil, err
				}
				continue
			}
			if d.terminalItemAnnounced(key, i, item.ID) {
				continue
			}
			admitted, err := d.announceItem(key)
			if err != nil {
				return nil, err
			}
			if !admitted {
				continue
			}
			d.sawToolCall = true
			emission, ok, err := functionCallTerminalEmission(item)
			if err != nil {
				return nil, err
			}
			if ok {
				pending = append(pending, emission)
			}
		case "message":
			if err := d.applyMessageContent(item, i, &pending); err != nil {
				return nil, err
			}
		case "reasoning":
			key, announced := d.terminalAnnouncedItemKey(i, item.ID)
			if !announced {
				admitted, err := d.announceItem(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
				emission, ok, err := reasoningTerminalEmission(item)
				if err != nil {
					return nil, err
				}
				if ok {
					pending = append(pending, emission)
				}
				continue
			}
			if err := d.applyReasoningContent(item, key, i, &pending); err != nil {
				return nil, err
			}
		}
	}
	return pending, nil
}

// applyMessageContent reconciles one message item's content, whether it
// arrived in output_item.done or in the terminal document. Open blocks receive
// only the missing bytes; parts that never reached the target wire are queued
// after every streamed block closes.
func (d *ResponsesStreamDecoder) applyMessageContent(item responsesWireItem, outputIndex int, pending *[]terminalEmission) error {
	if err := checkNativeIdentity("message id", item.ID); err != nil {
		return err
	}
	parts, err := terminalMessageParts(item.Content)
	if err != nil {
		return err
	}
	itemKey := d.terminalContentItemKey(item.ID, outputIndex)
	var channels []partChannel
	grouped := map[partChannel][]channelPart{}
	for order, part := range parts {
		channel := partChannelForBlock(part.block.Type)
		if _, ok := grouped[channel]; !ok {
			channels = append(channels, channel)
		}
		grouped[channel] = append(grouped[channel], channelPart{
			key:     d.terminalPartKey(item.ID, outputIndex, part.contentIndex),
			index:   part.contentIndex,
			value:   part.delta.Text,
			channel: channel,
			block:   part.block,
			order:   order,
		})
	}
	var emit []channelPart
	for _, channel := range channels {
		rec := d.reconcileChannel("content_partition", d.emittedContentParts(itemKey, channel), grouped[channel])
		emit = append(emit, rec.emit...)
	}
	sort.SliceStable(emit, func(a, b int) bool { return emit[a].order < emit[b].order })
	for _, part := range emit {
		if !d.announcedParts[part.key] {
			admitted, err := d.announcePart(part.key)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
		}
		if _, owned := d.partOwner[part.key]; !owned {
			d.partOwner[part.key] = itemKey
		}
		if _, kind := d.partKind[part.key]; !kind {
			d.partKind[part.key] = part.channel
		}
		before := d.emittedValue(part.key)
		d.appendContentValue(part, pending)
		if err := d.setEmittedText(part.key, before+part.value); err != nil {
			return err
		}
	}
	return nil
}

// applyReasoningContent reconciles one reasoning item's summary and content
// channels, whether the payload arrived in output_item.done or in the terminal
// document. The open item block receives only the missing bytes.
func (d *ResponsesStreamDecoder) applyReasoningContent(item responsesWireItem, key string, outputIndex int, pending *[]terminalEmission) error {
	// A reasoning item's id is used to build content-part keys. Validate it at
	// this shared owner so output_item.done and the terminal document cannot
	// retain an over-bound identity after the added event was accepted.
	if err := checkNativeIdentity("reasoning id", item.ID); err != nil {
		return err
	}
	// Summary parts are one concatenated reasoning channel; content
	// parts are another. Reconcile each as a whole before emitting so
	// a terminal re-partition cannot repeat streamed bytes.
	var summaryTerminal []channelPart
	for summaryIndex, summary := range item.Summary {
		if summary.Text == "" {
			continue
		}
		summaryTerminal = append(summaryTerminal, channelPart{
			key:     summaryPartKey(key, summaryIndex),
			index:   summaryIndex,
			value:   summary.Text,
			channel: partChannelReasoning,
			order:   summaryIndex,
		})
	}
	summaryRec := d.reconcileChannel("summary_partition", d.emittedSummaryParts(key), summaryTerminal)
	for _, part := range summaryRec.emit {
		if _, err := d.announcePart(part.key); err != nil {
			return err
		}
		before := d.emittedValue(part.key)
		d.appendReasoningValue(item, key, part.value, pending)
		if err := d.setEmittedText(part.key, before+part.value); err != nil {
			return err
		}
	}
	parts, err := terminalMessageParts(item.Content)
	if err != nil {
		return err
	}
	var contentTerminal []channelPart
	for order, part := range parts {
		if part.block.Type != ir.BlockText {
			continue
		}
		contentTerminal = append(contentTerminal, channelPart{
			key:     d.terminalPartKey(item.ID, outputIndex, part.contentIndex),
			index:   part.contentIndex,
			value:   part.delta.Text,
			channel: partChannelReasoning,
			block:   reasoningTerminalBlock(item),
			order:   order,
		})
	}
	contentRec := d.reconcileChannel("reasoning_content_partition", d.emittedContentParts(key, partChannelReasoning), contentTerminal)
	for _, part := range contentRec.emit {
		if !d.announcedParts[part.key] {
			admitted, err := d.announcePart(part.key)
			if err != nil {
				return err
			}
			if !admitted {
				continue
			}
		}
		if _, owned := d.partOwner[part.key]; !owned {
			d.partOwner[part.key] = key
		}
		if _, kind := d.partKind[part.key]; !kind {
			d.partKind[part.key] = partChannelReasoning
		}
		before := d.emittedValue(part.key)
		d.appendContentValue(part, pending)
		if err := d.setEmittedText(part.key, before+part.value); err != nil {
			return err
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

// terminalEmission is a terminal value that belongs to a block which has
// already ended, or to a terminal-only channel. It is emitted after every
// streamed block has closed so target lifecycles never nest.
type terminalEmission struct {
	block ir.Block
	delta *ir.Delta
}

// terminalAnnouncedItemKey resolves the announcement identity of a terminal
// reasoning item. The output position is a fallback only when both sides agree
// on the item id (or one side has none).
func (d *ResponsesStreamDecoder) terminalAnnouncedItemKey(outputIndex int, itemID string) (string, bool) {
	key := reasoningItemKey(itemID, outputIndex)
	if _, ok := d.itemBlockByKey[key]; ok {
		return key, true
	}
	fallback, ok := d.keyByOutput[outputIndex]
	if !ok {
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
func functionCallTerminalEmission(item responsesWireItem) (terminalEmission, bool, error) {
	if err := checkNativeIdentity("tool id", item.CallID); err != nil {
		return terminalEmission{}, false, err
	}
	if err := checkNativeIdentity("tool name", item.Name); err != nil {
		return terminalEmission{}, false, err
	}
	args, err := parseToolArguments(item.Arguments)
	if err != nil {
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
			ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(`{}`),
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

// toolArgumentsDelta records one incremental argument delta. The buffered
// output_item.added payload is emitted first, then the delta is appended
// verbatim: OpenAI's document says each delta is added to the previous value,
// so a prefix heuristic would corrupt a legitimate first delta that happens to
// start with the inline prefix.
func (d *ResponsesStreamDecoder) toolArgumentsDelta(key string, blockIndex int, delta string) {
	if delta == "" || key == "" {
		return
	}
	if d.argsEmitted[key] == nil {
		if initial := d.initialArgs[key]; initial != "" {
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: initial}})
			d.recordEmittedArguments(key, initial)
		}
		d.dropInitialArgs(key)
	}
	d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: delta}})
	d.recordEmittedArguments(key, delta)
}

// flushToolArguments reconciles the authoritative done/terminal arguments
// with the bytes already streamed. A full value that strictly extends the
// emitted prefix appends only the missing suffix; a divergent value keeps the
// already-committed bytes. A complete retained value must be valid JSON. Once
// coverage is partial, the retained bytes are only a truncated prefix of what
// the client already received: they cannot be reconciled or validated as the
// final value, so the streamed bytes stand and reconciliation stops.
func (d *ResponsesStreamDecoder) flushToolArguments(key string, blockIndex int, full string) error {
	if key == "" {
		return nil
	}
	if d.argsEmitted[key] != nil {
		emitted, complete := d.emittedArguments(key)
		if !complete {
			return nil
		}
		if full != "" && len(full) > len(emitted) && strings.HasPrefix(full, emitted) {
			suffix := full[len(emitted):]
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: suffix}})
			d.recordEmittedArguments(key, suffix)
			emitted += suffix
		}
		if _, err := parseToolArguments(emitted); err != nil {
			return err
		}
		return nil
	}

	args := full
	if args == "" {
		args = d.initialArgs[key]
	}
	d.dropInitialArgs(key)
	if d.argsEmitted[key] == nil {
		d.argsEmitted[key] = &toolArgumentCoverage{}
	}
	normalized, err := parseToolArguments(args)
	if err != nil {
		return err
	}
	args = string(normalized)
	if args != "{}" {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: blockIndex, Delta: &ir.Delta{Arguments: args}})
	}
	d.recordEmittedArguments(key, args)
	return nil
}

// recordEmittedArguments adds argument bytes to the bounded coverage for one
// item identity. Coverage past toolInitialBytesMax is not retained.
func (d *ResponsesStreamDecoder) recordEmittedArguments(key, value string) {
	if key == "" || value == "" {
		return
	}
	cov := d.argsEmitted[key]
	if cov == nil {
		cov = &toolArgumentCoverage{}
		d.argsEmitted[key] = cov
	}
	if cov.partial {
		return
	}
	remaining := toolInitialBytesMax - d.argsEmittedBytes
	if len(value) > remaining {
		if remaining > 0 {
			cov.emitted.WriteString(value[:remaining])
			d.argsEmittedBytes += remaining
		}
		cov.partial = true
		return
	}
	cov.emitted.WriteString(value)
	d.argsEmittedBytes += len(value)
}

// emittedArguments returns the retained coverage and whether it is complete.
func (d *ResponsesStreamDecoder) emittedArguments(key string) (string, bool) {
	cov := d.argsEmitted[key]
	if cov == nil {
		return "", true
	}
	return cov.emitted.String(), !cov.partial
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
	if cov := d.argsEmitted[key]; cov != nil {
		d.argsEmittedBytes -= cov.emitted.Len()
		delete(d.argsEmitted, key)
	}
}

// startPartBlock starts the target block for an announced content part when
// it has not started yet. A part announced with an empty start payload is
// deferred; the first delta or done event starts it from the stored seed.
func (d *ResponsesStreamDecoder) startPartBlock(key string) (int, bool) {
	if idx, ok := d.partBlockByKey[key]; ok {
		return idx, d.open[idx]
	}
	block, ok := d.partSeed[key]
	if !ok {
		return 0, false
	}
	pos, ok := d.partPosition[key]
	if !ok {
		return 0, false
	}
	idx := d.newBlock()
	d.partBlock[pos] = idx
	d.partBlockByKey[key] = idx
	d.keyByPart[pos] = key
	delete(d.partSeed, key)
	d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &block})
	return idx, true
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

// responsesSummaryText joins the non-empty text parts of a Responses
// reasoning summary. The value may arrive in output_item.added or in a
// summary part event; both paths consume it once.
func responsesSummaryText(parts []struct {
	Type string `json:"type"`
	Text string `json:"text"`
}) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString(part.Text)
	}
	return b.String()
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
	err := d.closeAll(nil)
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
	// fallbackModel names the model when the source omitted its start event,
	// matching the Chat encoder's StreamOptions.FallbackModel contract.
	fallbackModel string

	started     bool
	outputIndex int
	items       map[int]*responsesItemState
	open        map[int]bool
	order       []int
	finish      ir.FinishReason
	usage       ir.Usage
	metadata    map[string]string
	serviceTier string
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
	if e.model == "" {
		e.model = e.fallbackModel
	}
	if e.created == 0 {
		e.created = time.Now().Unix()
	}
	if err := e.retain(len(e.id)+len(e.model), "response identity"); err != nil {
		return err
	}
	response, err := e.responseObject("in_progress", "", nil)
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
			// response.created/response.in_progress may already be on the
			// wire (a source stream can deliver its start event late, or not
			// at all). Identity is immutable once committed, exactly like the
			// Chat encoder's !started guard; only late-arriving data that the
			// target still needs is merged.
			if !e.started {
				e.id, e.model, e.created = ev.Response.ID, ev.Response.Model, ev.Response.Created
			}
			e.usage = ev.Response.Usage
			e.metadata = ev.Response.Metadata
			e.serviceTier = ev.Response.ServiceTier
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
	status, incompleteReason, _ := responsesTerminalStatus(e.finish, "")
	if e.failed != nil {
		status = "failed"
	}
	response, err := e.responseObject(status, incompleteReason, e.outputItems())
	if err != nil {
		return err
	}
	return e.emit("response."+status, map[string]any{
		"type": "response." + status, "sequence_number": e.nextSeq(),
		"response": response,
	})
}

func (e *ResponsesStreamEncoder) responseObject(status, incompleteReason string, output []map[string]any) (map[string]any, error) {
	return NewResponsesEnvelope(ResponsesEnvelope{
		ID: e.id, Model: e.model, Created: e.created, Status: status,
		IncompleteReason: incompleteReason,
		Output:           responsesAnyItems(output),
		Error:            e.failed, Usage: e.usage,
		Metadata: e.metadata, ServiceTier: e.serviceTier,
	})
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
	if e.items[index] != nil {
		return nil, fmt.Errorf("responses stream item index %d already started", index)
	}
	if len(e.order) >= WireCollectionMax {
		return nil, fmt.Errorf("responses stream exceeds %d items", WireCollectionMax)
	}
	if err := e.retain(responsesStreamItemEnvelopeBytes, "item envelope"); err != nil {
		return nil, err
	}
	item := &responsesItemState{
		id:          newResponsesItemID(kind),
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

// newResponsesItemID builds one Responses output item identity. The stream
// encoder and the non-stream encoder share it so every item carries the
// type-appropriate required id.
func newResponsesItemID(kind ir.BlockType) string {
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
