package format

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
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
	SequenceNumber jsonnum.Int64   `json:"sequence_number"`
	Response       json.RawMessage `json:"response"`
	Item           json.RawMessage `json:"item"`
	Part           json.RawMessage `json:"part"`
	OutputIndex    jsonnum.Int64   `json:"output_index"`
	ContentIndex   jsonnum.Int64   `json:"content_index"`
	SummaryIndex   jsonnum.Int64   `json:"summary_index"`
	ItemID         string          `json:"item_id"`
	Delta          string          `json:"delta"`
	Text           string          `json:"text"`
	Refusal        string          `json:"refusal"`
	Arguments      string          `json:"arguments"`
	Code           json.RawMessage `json:"code"`
	Message        json.RawMessage `json:"message"`
	// Param is a provider hint with no canonical equivalent. It stays raw so
	// a wrong-typed value cannot abort the terminal before its valid code and
	// message siblings are classified.
	Param json.RawMessage `json:"param"`
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
	// argumentEvidence records that the source supplied argument bytes for a
	// call. An incomplete or cancelled terminal needs it to distinguish an
	// explicit empty object from a call that never carried arguments.
	argumentEvidence map[string]bool
	// openToolKeys names every announced function call whose target block may
	// still be open, including a call that never carried argument bytes.
	openToolKeys map[string]bool
	// toolIDs is a lifetime set of committed function-call ids. Two target
	// blocks with one id make the client's tool-result correlation ambiguous;
	// synthesis never shadows a source id silently.
	toolIDs map[string]struct{}
	// announcedToolKeys names every announced function call for the whole
	// stream, including one whose done event already closed its block. A
	// terminal document can repeat a closed call; that entry must not be
	// reassigned to a different open call by name or position.
	announcedToolKeys map[string]bool
	// toolAliases maps an alternate identity spelling of an announced call to
	// its canonical position-qualified key. A terminal entry can carry only
	// the item id after the announcement used the call id, or the reverse.
	// Aliases live for the whole stream: the terminal document still repeats a
	// call after its done event closed the block. An empty value marks a
	// spelling claimed by two announcements, where only the wire position can
	// disambiguate.
	toolAliases map[string]string
	// toolNames maps an announced call key to its function name for
	// identity-free terminal matching. Names stay available after the call
	// closes so a terminal repeat can be recognized.
	toolNames map[string]string
	// toolArgsHash records the canonical fingerprint of the emitted argument
	// value of an id-less announced call when it closes. A reordered terminal
	// repeat has no identity, so the proxy compares the function name and this
	// hash without retaining the argument bytes for the whole stream.
	toolArgsHash map[string][sha256.Size]byte
	// closedIdlessCalls counts closed id-less calls by function name and
	// emitted argument hash. It is the constant-time lookup for a terminal
	// repeat that no longer occupies its announcement position.
	closedIdlessCalls map[string]map[[sha256.Size]byte]int
	// Announcement identity. A provider's terminal output array is not
	// required to preserve output_index positions, so every item key carries
	// its announced position and stable identities (call id, item id, content
	// index) are retained as aliases for terminal matching. Two items that
	// share an id at different positions stay distinct.
	announcedOutputs map[int]bool
	announcedItems   map[string]bool
	announcedParts   map[string]bool
	itemBlockByKey   map[string]int
	partBlockByKey   map[string]int
	keyByOutput      map[int]string
	// itemKeyByKindID maps one item kind and stable provider id to its
	// announced positional key for a terminal document that renumbers items.
	// An empty value marks an id used by more than one announcement of that
	// kind, where identity alone cannot choose.
	itemKeyByKindID map[string]string
	// terminalItemClaims maps an announced message key to the terminal output
	// position that first claimed it during the current terminal walk. A
	// second terminal-only sibling that reuses the same provider id cannot
	// borrow the first sibling's key and must be synthesized instead.
	terminalItemClaims map[string]int
	// reasoningKeys names the announced key of every reasoning item.
	// itemBlock and keyByOutput are kind-agnostic position caches, so a
	// reasoning event must consult this set before routing its bytes onto an
	// announced tool or message block.
	reasoningKeys  map[string]bool
	keyByPart      map[[2]int]string
	itemIDByOutput map[int]string
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
	// ownerParts indexes the parts that have emitted bytes by their logical
	// item owner, so terminal reconciliation scans only the item's own parts
	// instead of every part in the stream.
	ownerParts map[string]map[string]struct{}
	// warnings records streaming approximations that cannot be represented on
	// the committed target wire.
	warnings ir.Warnings
	// terminalPending holds authoritative done-event values whose target block
	// already ended or was never opened. They are emitted after every streamed
	// block closes (closeAll), so target lifecycles never nest. Entries are
	// keyed by part identity and coalesced: repeated unannounced deltas for one
	// part append into the existing emission instead of growing one entry per
	// delta. terminalPendingByKey records the first-seen position for each key.
	terminalPending      []terminalEmission
	terminalPendingByKey map[string]int
	open                 map[int]bool

	usage       ir.Usage
	finish      ir.FinishReason
	sawToolCall bool
	// phaseSeen records that a streamed or terminal item carried the
	// Responses output phase hint. The target wire has no equivalent, so the
	// terminal reports it once.
	phaseSeen bool
}

// NewResponsesStreamDecoder builds a decoder over one upstream body.
func NewResponsesStreamDecoder(r io.Reader) *ResponsesStreamDecoder {
	return &ResponsesStreamDecoder{
		r:                    newSSEReader(r),
		itemBlock:            map[int]int{},
		partBlock:            map[[2]int]int{},
		initialArgs:          map[string]string{},
		argsEmitted:          map[string]*toolArgumentCoverage{},
		argumentEvidence:     map[string]bool{},
		openToolKeys:         map[string]bool{},
		toolIDs:              map[string]struct{}{},
		announcedToolKeys:    map[string]bool{},
		toolAliases:          map[string]string{},
		toolNames:            map[string]string{},
		toolArgsHash:         map[string][sha256.Size]byte{},
		closedIdlessCalls:    map[string]map[[sha256.Size]byte]int{},
		announcedOutputs:     map[int]bool{},
		announcedItems:       map[string]bool{},
		announcedParts:       map[string]bool{},
		itemBlockByKey:       map[string]int{},
		partBlockByKey:       map[string]int{},
		keyByOutput:          map[int]string{},
		itemKeyByKindID:      map[string]string{},
		terminalItemClaims:   map[string]int{},
		reasoningKeys:        map[string]bool{},
		keyByPart:            map[[2]int]string{},
		terminalPendingByKey: map[string]int{},
		itemIDByOutput:       map[int]string{},
		partSeed:             map[string]ir.Block{},
		partPosition:         map[string][2]int{},
		emittedText:          map[string]*strings.Builder{},
		partOwner:            map[string]string{},
		partKind:             map[string]partChannel{},
		ownerParts:           map[string]map[string]struct{}{},
		open:                 map[int]bool{},
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
		outputIndex, err := nonNegativeStreamIndex("output_index", ev.OutputIndex)
		if err != nil {
			return nil, err
		}
		contentIndex, err := nonNegativeStreamIndex("content_index", ev.ContentIndex)
		if err != nil {
			return nil, err
		}
		summaryIndex, err := nonNegativeStreamIndex("summary_index", ev.SummaryIndex)
		if err != nil {
			return nil, err
		}
		switch ev.Type {
		case "response.created", "response.in_progress":
			if d.started {
				continue
			}
			// The created payload can carry a full output array even though
			// this branch only needs identity. Preflight it with the same
			// bounds owner as the non-stream decoder before the typed decode.
			if err := checkResponsesResponseBounds(ev.Response); err != nil {
				return nil, err
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
			d.id, d.model, d.created = resp.ID, resp.Model, int64(resp.CreatedAt)
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
				Metadata: resp.Metadata.values, ServiceTier: resp.ServiceTier,
			}})
		case "response.output_item.added":
			var item responsesWireItem
			if err := json.Unmarshal(ev.Item, &item); err != nil {
				return nil, err
			}
			if item.Phase != "" {
				d.phaseSeen = true
			}
			admitted, err := d.announceOutput(outputIndex)
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
				toolID := responsesFunctionCallID(item.CallID, item.ID, outputIndex)
				if err := checkNativeIdentity("tool id", toolID); err != nil {
					return nil, err
				}
				if err := checkNativeIdentity("tool name", item.Name); err != nil {
					return nil, err
				}
				if err := checkToolCallName(item.Name); err != nil {
					return nil, err
				}
				key := functionCallKey(item.CallID, item.ID, outputIndex)
				admitted, err := d.announceItem(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
				if err := reserveToolID(d.toolIDs, toolID); err != nil {
					return nil, err
				}
				d.sawToolCall = true
				d.openToolKeys[key] = true
				d.announcedToolKeys[key] = true
				d.toolNames[key] = item.Name
				d.openToolItems++
				if d.openToolItems > toolBlocksMax {
					return nil, fmt.Errorf("responses stream exceeds %d concurrent tool calls", toolBlocksMax)
				}
				idx := d.newBlock()
				d.itemBlock[outputIndex] = idx
				d.itemBlockByKey[key] = idx
				d.keyByOutput[outputIndex] = key
				d.itemIDByOutput[outputIndex] = item.ID
				d.aliasToolKey(functionCallIdentityKey(item.CallID, item.ID), key)
				if item.CallID != "" && item.ID != "" {
					d.aliasToolKey(functionCallIdentityKey("", item.ID), key)
				}
				d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &ir.Block{
					Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
						ID: toolID, Name: item.Name, Arguments: json.RawMessage(`{}`),
					},
				}})
				if !toolArgumentsMissing(item.Arguments) {
					d.argumentEvidence[key] = true
				}
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
				key := reasoningItemKey(item.ID, outputIndex)
				admitted, err := d.announceItem(key)
				if err != nil {
					return nil, err
				}
				if !admitted {
					continue
				}
				idx := d.newBlock()
				d.itemBlock[outputIndex] = idx
				d.itemBlockByKey[key] = idx
				d.keyByOutput[outputIndex] = key
				d.reasoningKeys[key] = true
				d.itemIDByOutput[outputIndex] = item.ID
				d.registerItemIdentity(itemKindReasoning, item.ID, key)
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
					d.setPartOwner(partKey, key)
					d.partKind[partKey] = partChannelReasoning
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
				if _, err := d.announceItem(messageItemKey(item.ID, outputIndex)); err != nil {
					return nil, err
				}
				d.itemIDByOutput[outputIndex] = item.ID
				d.registerItemIdentity(itemKindMessage, item.ID, streamItemKey(item.ID, outputIndex))
			default:
				// A provider-specific item has no cross-protocol equivalent.
				// Streaming cannot report the loss (the status line is already
				// committed), so it is dropped without a record. It is still a
				// model tool invocation: the shared vocabulary keeps the turn's
				// finish and settlement aligned with the non-stream decoder.
				if sse.ResponsesToolCallItem(item.Type) {
					d.sawToolCall = true
				}
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
				itemID = d.itemIDByOutput[outputIndex]
			}
			if err := checkNativeIdentity("item id", itemID); err != nil {
				return nil, err
			}
			itemKey := streamItemKey(itemID, outputIndex)
			key := contentPartKey(itemKey, contentIndex)
			admitted, err := d.announcePart(key)
			if err != nil {
				return nil, err
			}
			if !admitted {
				continue
			}
			pos := [2]int{outputIndex, contentIndex}
			d.keyByPart[pos] = key
			d.partPosition[key] = pos
			d.itemIDByOutput[outputIndex] = itemID
			identityKind := itemKindMessage
			if block.Type == ir.BlockReasoning {
				identityKind = itemKindReasoning
			}
			d.registerItemIdentity(identityKind, itemID, itemKey)
			d.setPartOwner(key, itemKey)
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
			pos := [2]int{outputIndex, contentIndex}
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
			pos := [2]int{outputIndex, contentIndex}
			itemID := d.itemIDByOutput[outputIndex]
			key, ok := d.keyByPart[pos]
			if !ok {
				key = contentPartKey(streamItemKey(itemID, outputIndex), contentIndex)
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
				d.setPartOwner(key, streamItemKey(itemID, outputIndex))
			}
			if _, kind := d.partKind[key]; !kind {
				d.partKind[key] = partChannelForBlock(block.Type)
			}
			if idx, ok := d.startPartBlock(key); ok {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Text: emit}})
			} else if err := d.queueTerminal(key, block, &ir.Delta{Text: emit}); err != nil {
				return nil, err
			}
			if err := d.setEmittedText(key, text); err != nil {
				return nil, err
			}
		case "response.function_call_arguments.delta":
			idx, ok := d.itemBlock[outputIndex]
			if !ok {
				continue
			}
			key := d.keyByOutput[outputIndex]
			if !d.announcedToolKeys[key] || !d.openToolKeys[key] {
				// itemBlock is an index cache: a reasoning or message
				// announcement also sets it. Argument bytes belong to a tool
				// block that is still open; anything else is a stray event.
				continue
			}
			d.toolArgumentsDelta(key, idx, ev.Delta)
		case "response.reasoning_summary_text.delta":
			idx, ok := d.itemBlock[outputIndex]
			if !ok {
				continue
			}
			key := d.keyByOutput[outputIndex]
			if !d.reasoningKeys[key] {
				continue
			}
			if ev.Delta != "" && key != "" {
				partKey := summaryPartKey(key, summaryIndex)
				if _, err := d.announcePart(partKey); err != nil {
					return nil, err
				}
				d.setPartOwner(partKey, key)
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
			key := d.keyByOutput[outputIndex]
			idx, ok := d.itemBlock[outputIndex]
			if part.Text == "" || !ok || !d.reasoningKeys[key] {
				continue
			}
			partKey := summaryPartKey(key, summaryIndex)
			if _, err := d.announcePart(partKey); err != nil {
				return nil, err
			}
			d.setPartOwner(partKey, key)
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
			key := d.keyByOutput[outputIndex]
			idx, ok := d.itemBlock[outputIndex]
			if ev.Text == "" || !ok || !d.reasoningKeys[key] {
				continue
			}
			partKey := summaryPartKey(key, summaryIndex)
			if _, err := d.announcePart(partKey); err != nil {
				return nil, err
			}
			d.setPartOwner(partKey, key)
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
			if key := d.keyByOutput[outputIndex]; key != "" && !d.reasoningKeys[key] {
				// The output position is announced as a tool or message item;
				// its block is not a reasoning channel.
				continue
			}
			pos := [2]int{outputIndex, contentIndex}
			itemID := d.itemIDByOutput[outputIndex]
			key := d.keyByPart[pos]
			if key == "" {
				key = contentPartKey(streamItemKey(itemID, outputIndex), contentIndex)
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
				d.setPartOwner(key, streamItemKey(itemID, outputIndex))
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
			} else if idx, ok := d.itemBlock[outputIndex]; ok && d.open[idx] {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: delta}})
			} else if err := d.queueTerminal(key, ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses}}, &ir.Delta{Reasoning: delta}); err != nil {
				return nil, err
			}
		case "response.reasoning_text.done":
			// The done payload is authoritative. Emit it only when no delta
			// carried the part's bytes. A provider may omit the part-start
			// event entirely, so the reasoning item block is the fallback
			// channel; the part identity is still recorded so the terminal
			// document cannot repeat the value.
			if key := d.keyByOutput[outputIndex]; key != "" {
				if !d.reasoningKeys[key] {
					// The output position is announced as a tool item; its
					// block is not a reasoning channel.
					continue
				}
			} else if d.announcedItems[messageItemKey(d.itemIDByOutput[outputIndex], outputIndex)] {
				// A message item owns this output position but has no
				// keyByOutput entry. Do not turn its position into a fallback
				// reasoning part; only an unannounced position may use the
				// fallback below.
				continue
			}
			pos := [2]int{outputIndex, contentIndex}
			itemID := d.itemIDByOutput[outputIndex]
			key, ok := d.keyByPart[pos]
			if !ok {
				key = contentPartKey(streamItemKey(itemID, outputIndex), contentIndex)
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
				d.setPartOwner(key, streamItemKey(itemID, outputIndex))
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
			} else if idx, ok := d.itemBlock[outputIndex]; ok && d.open[idx] {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			} else if err := d.queueTerminal(key, ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Responses}}, &ir.Delta{Reasoning: emit}); err != nil {
				return nil, err
			}
			if err := d.setEmittedText(key, ev.Text); err != nil {
				return nil, err
			}
		case "response.content_part.done":
			pos := [2]int{outputIndex, contentIndex}
			if err := checkNativeIdentity("item id", ev.ItemID); err != nil {
				return nil, err
			}
			itemID := ev.ItemID
			if itemID == "" {
				itemID = d.itemIDByOutput[outputIndex]
			}
			key := d.keyByPart[pos]
			if key == "" {
				key = contentPartKey(streamItemKey(itemID, outputIndex), contentIndex)
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
					d.setPartOwner(key, streamItemKey(itemID, outputIndex))
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
			if item.Phase != "" {
				d.phaseSeen = true
			}
			typ := item.Type
			if typ == "" && item.Role != "" {
				typ = "message"
			}
			if typ == "function_call" {
				idx, ok := d.itemBlock[outputIndex]
				if !ok {
					continue
				}
				key := d.keyByOutput[outputIndex]
				if err := d.flushToolArguments(key, idx, item.Arguments, responsesToolArgumentsComplete); err != nil {
					return nil, err
				}
				delete(d.itemBlock, outputIndex)
				delete(d.open, idx)
				d.clearToolState(key)
				d.openToolItems--
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if typ == "reasoning" {
				idx, ok := d.itemBlock[outputIndex]
				if !ok {
					continue
				}
				if err := d.applyReasoningContent(item, d.keyByOutput[outputIndex], outputIndex, &d.terminalPending); err != nil {
					return nil, err
				}
				delete(d.itemBlock, outputIndex)
				delete(d.open, idx)
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if sse.ResponsesToolCallItem(typ) {
				d.sawToolCall = true
			} else if typ == "message" {
				if err := d.applyMessageContent(item, outputIndex, &d.terminalPending, false); err != nil {
					return nil, err
				}
				// Some providers skip content_part.done; close any part block
				// that is still open for this output item. A deferred empty part
				// never reached the target, so discard its seed rather than
				// opening and immediately closing an empty block.
				for pos, key := range d.keyByPart {
					if pos[0] != outputIndex {
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
			idx, ok := d.itemBlock[outputIndex]
			if !ok {
				continue
			}
			key := d.keyByOutput[outputIndex]
			if !d.announcedToolKeys[key] || !d.openToolKeys[key] {
				continue
			}
			if err := d.flushToolArguments(key, idx, ev.Arguments, responsesToolArgumentsComplete); err != nil {
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
			key := d.keyByOutput[outputIndex]
			idx, ok := d.itemBlock[outputIndex]
			if text == "" || !ok || !d.reasoningKeys[key] {
				continue
			}
			partKey := summaryPartKey(key, summaryIndex)
			if _, err := d.announcePart(partKey); err != nil {
				return nil, err
			}
			d.setPartOwner(partKey, key)
			emit := pendingValueSuffix(d.emittedValue(partKey), text)
			if emit != "" {
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Reasoning: emit}})
			}
			if err := d.setEmittedText(partKey, text); err != nil {
				return nil, err
			}
		case "response.completed", "response.incomplete", "response.failed", "response.cancelled":
			if err := checkResponsesResponseBounds(ev.Response); err != nil {
				return nil, err
			}
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
			for _, item := range resp.Output {
				if item.Phase != "" {
					d.phaseSeen = true
					break
				}
			}
			if d.phaseSeen {
				d.warnings.AddOnce(ir.WarningDropped, "output.phase",
					"Responses output phase has no cross-protocol equivalent")
			}
			// The terminal class owns the turn's outcome: completed,
			// incomplete and cancelled validate tool arguments strictly, while
			// failed, error and truncated drop the unparseable call.
			if err := d.closeAll(resp.Output, responsesTerminalClassFor(ev.Type)); err != nil {
				return nil, err
			}
			if ev.Type == "response.failed" {
				e := &ir.Error{Message: "upstream response failed"}
				if typ, code, msg := metrics.ParseErrorComponents(resp.Error); typ != "" || code != "" || msg != "" {
					bounded := boundedProviderError(typ, code, msg)
					e.Type, e.Code = bounded.Type, bounded.Code
					if msg != "" {
						e.Message = bounded.Message
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
			// The standalone error event is the terminal statement; buffered
			// initial argument bytes are incomplete by definition here.
			if err := d.closeAll(nil, responsesTerminalError); err != nil {
				return nil, err
			}
			_, code, msg := metrics.ParseErrorMembers(nil, ev.Code, ev.Message)
			if code == "" && msg == "" {
				msg = genericProviderErrorMessage
			}
			d.q.push(&ir.Event{Type: ir.EventError, Error: boundedProviderError(
				"api_error", code, msg)})
		default:
			// response.output_text.done, function_call_arguments.done,
			// reasoning_summary_* and provider-specific events are already
			// represented by the deltas above.
			continue
		}
	}
}
