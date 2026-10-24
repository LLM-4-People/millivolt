package format

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Anthropic Messages streaming codec.

type anthropicStreamEvent struct {
	Type         string                `json:"type"`
	Index        int                   `json:"index"`
	Message      json.RawMessage       `json:"message"`
	ContentBlock anthropicWireBlock    `json:"content_block"`
	Delta        json.RawMessage       `json:"delta"`
	Usage        *anthropicStreamUsage `json:"usage"`
	Error        *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicStreamUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	// ServiceTier and Speed exist only on the message_start usage; a
	// message_delta repeats the token fields without them.
	ServiceTier         string `json:"service_tier"`
	Speed               string `json:"speed"`
	OutputTokensDetails *struct {
		ThinkingTokens int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type anthropicStreamMessage struct {
	ID      string                `json:"id"`
	Model   string                `json:"model"`
	Usage   *anthropicStreamUsage `json:"usage"`
	Content []anthropicWireBlock  `json:"content"`
}

// AnthropicStreamDecoder converts Anthropic SSE events into canonical events.
type AnthropicStreamDecoder struct {
	r        *sseReader
	q        eventQueue
	terminal bool

	id      string
	model   string
	created int64
	// started records that the first message_start already committed this
	// stream's identity. A later message_start may merge usage but must never
	// rewrite the id/model/created the client already saw.
	started bool

	usage          ir.Usage
	usageBaseInput int64
	// usageReported distinguishes a source that reported usage (even all
	// zeros) from one that never did. Only a reported usage is forwarded.
	usageReported bool
	skipped       map[int]bool
	observe       func(string, []byte)
	sawStopReason bool
	// pendingStopReason and pendingStopSequence hold the last non-empty
	// message_delta terminal values. Anthropic permits multiple message_delta
	// events (usage is cumulative), so the decoder emits one canonical
	// MessageDelta after the final wire event and never lets an interim empty
	// delta send a premature finish to the target.
	pendingStopReason   string
	pendingStopSequence *string
	messageDeltaSent    bool

	openToolBlocks   int
	toolInitialBytes int
	toolInitial      map[int]int
	openBlocks       map[int]bool
	// errorSeen records an explicit error event. A terminal stop reason of
	// "error" without one still needs a failure event; with one, the explicit
	// provider error already owns the terminal.
	errorSeen bool
	// seenBlocks is a lifetime set of content-block indexes. Anthropic's
	// stream contract assigns each block index to one position in the final
	// content array; reusing a closed index would make the target terminal
	// repeat the same position and could amplify a bounded event into a huge
	// terminal.
	seenBlocks map[int]struct{}
}

// NewAnthropicStreamDecoder builds a decoder over one upstream body. observe,
// when non-nil, receives every raw data payload synchronously.
func NewAnthropicStreamDecoder(r io.Reader, observe func(string, []byte)) *AnthropicStreamDecoder {
	return &AnthropicStreamDecoder{
		r:           newSSEReader(r),
		skipped:     map[int]bool{},
		observe:     observe,
		toolInitial: map[int]int{},
		openBlocks:  map[int]bool{},
		seenBlocks:  map[int]struct{}{},
	}
}

// Next returns the next canonical event.
func (d *AnthropicStreamDecoder) Next() (*ir.Event, error) {
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
				// A stop_reason delivered without message_stop is the server
				// closing the turn; only a stream with neither is truncated.
				if d.sawStopReason {
					d.terminal = true
					d.closeOpenBlocks()
					d.flushMessageDelta()
					d.pushTerminal()
					continue
				}
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
		var ev anthropicStreamEvent
		if err := json.Unmarshal(raw.Data, &ev); err != nil {
			return nil, err
		}
		typ := ev.Type
		if typ == "" {
			typ = raw.Name
		}
		switch typ {
		case "message_start":
			var msg anthropicStreamMessage
			if err := json.Unmarshal(ev.Message, &msg); err != nil {
				return nil, err
			}
			if err := checkNativeIdentity("id", msg.ID); err != nil {
				return nil, err
			}
			if err := checkNativeIdentity("model", msg.Model); err != nil {
				return nil, err
			}
			tier := ""
			if msg.Usage != nil {
				if err := d.mergeUsage(msg.Usage); err != nil {
					return nil, err
				}
				d.usageReported = true
				tier, _ = anthropicUsageServiceTier(msg.Usage.ServiceTier, msg.Usage.Speed)
			}
			if d.started {
				// Identity already reached the wire: keep the first start's
				// id/model/created and its start event. Only the cumulative
				// usage above may still move.
				continue
			}
			d.started = true
			d.id, d.model = msg.ID, msg.Model
			d.created = time.Now().Unix()
			u := d.usage
			d.q.push(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
				ID: d.id, Model: d.model, Created: d.created, Usage: u, ServiceTier: tier,
			}})
		case "content_block_start":
			if d.openBlocks[ev.Index] {
				return nil, fmt.Errorf("duplicate native tool index %d", ev.Index)
			}
			if _, seen := d.seenBlocks[ev.Index]; seen {
				return nil, fmt.Errorf("reused native content block index %d", ev.Index)
			}
			if len(d.seenBlocks) >= WireCollectionMax {
				return nil, fmt.Errorf("anthropic stream exceeds %d content block indexes", WireCollectionMax)
			}
			d.seenBlocks[ev.Index] = struct{}{}
			d.openBlocks[ev.Index] = true
			if len(ev.ContentBlock.Input) > toolInitialBytesMax-d.toolInitialBytes {
				return nil, errNativeToolInitialBytes
			}
			block := ev.ContentBlock
			if block.Type == "tool_use" {
				if block.ID == "" {
					// A streamed tool call without its required id still
					// needs one on the target wire; the index keeps it
					// unique within the response. Streaming cannot report
					// the approximation in a header, so it is synthesized
					// silently.
					block.ID = syntheticToolID(ev.Index)
				}
				if err := checkNativeIdentity("tool id", block.ID); err != nil {
					return nil, err
				}
				if err := checkNativeIdentity("tool name", block.Name); err != nil {
					return nil, err
				}
				d.openToolBlocks++
				if d.openToolBlocks > toolBlocksMax {
					return nil, errNativeToolBlocks
				}
				d.toolInitial[ev.Index] = len(block.Input)
				d.toolInitialBytes += len(block.Input)
				if d.toolInitialBytes > toolInitialBytesMax {
					return nil, errNativeToolInitialBytes
				}
			}
			irBlock, ok := anthropicStreamBlock(block)
			if !ok {
				// A provider-executed block has no cross-protocol equivalent.
				// Streaming cannot report the loss (the status line is already
				// committed), so it is skipped without a record.
				d.skipped[ev.Index] = true
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: ev.Index, Block: &irBlock})
		case "content_block_delta":
			if d.skipped[ev.Index] {
				continue
			}
			var delta struct {
				Type        string       `json:"type"`
				Text        string       `json:"text"`
				PartialJSON string       `json:"partial_json"`
				Thinking    string       `json:"thinking"`
				Signature   string       `json:"signature"`
				Citation    *ir.Citation `json:"citation"`
			}
			if err := json.Unmarshal(ev.Delta, &delta); err != nil {
				return nil, err
			}
			out := &ir.Delta{}
			switch delta.Type {
			case "text_delta":
				out.Text = delta.Text
			case "input_json_delta":
				out.Arguments = delta.PartialJSON
			case "thinking_delta":
				out.Reasoning = delta.Thinking
			case "signature_delta":
				out.Signature = delta.Signature
			case "citations_delta":
				out.Citation = delta.Citation
			default:
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: ev.Index, Delta: out})
		case "content_block_stop":
			open := d.openBlocks[ev.Index]
			delete(d.openBlocks, ev.Index)
			if n, ok := d.toolInitial[ev.Index]; ok {
				d.openToolBlocks--
				d.toolInitialBytes -= n
				delete(d.toolInitial, ev.Index)
			}
			if d.skipped[ev.Index] {
				delete(d.skipped, ev.Index)
				continue
			}
			if !open {
				// A terminal event already closed this block; a late stop must
				// not emit a second BlockEnd for an index the client saw closed.
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: ev.Index})
		case "message_delta":
			var delta struct {
				StopReason   string  `json:"stop_reason"`
				StopSequence *string `json:"stop_sequence"`
			}
			if len(ev.Delta) > 0 {
				if err := json.Unmarshal(ev.Delta, &delta); err != nil {
					return nil, err
				}
			}
			if ev.Usage != nil {
				if err := d.mergeUsage(ev.Usage); err != nil {
					return nil, err
				}
				d.usageReported = true
			}
			if delta.StopReason != "" {
				d.sawStopReason = true
				d.pendingStopReason = delta.StopReason
			}
			if delta.StopSequence != nil {
				d.pendingStopSequence = delta.StopSequence
			}
			d.closeOpenBlocks()
			// The canonical MessageDelta is emitted once at message_stop so an
			// interim usage-only delta cannot send a premature empty finish.
		case "message_stop":
			d.terminal = true
			d.closeOpenBlocks()
			d.flushMessageDelta()
			d.pushTerminal()
		case "ping":
			continue
		case "error":
			d.terminal = true
			d.errorSeen = true
			d.closeOpenBlocks()
			if ev.Error == nil || (ev.Error.Type == "" && ev.Error.Message == "") {
				// An empty error object is not a provider statement; treat it
				// exactly like a missing completion marker.
				d.q.push(&ir.Event{Type: ir.EventError, Error: &ir.Error{
					Type: sse.TypeUpstreamError, Code: metrics.CodeTruncated,
					Message: metrics.DegenerateMessage(metrics.CodeTruncated),
				}})
				continue
			}
			d.q.push(&ir.Event{Type: ir.EventError, Error: &ir.Error{
				Type: ev.Error.Type, Message: ev.Error.Message,
			}})
		default:
			continue
		}
	}
}

// flushMessageDelta emits the single canonical terminal delta. The finish
// reason is the last non-empty stop_reason the source reported; a source that
// never reported one keeps the non-clean FinishOther. Usage is the merged
// cumulative value at the terminal event.
func (d *AnthropicStreamDecoder) flushMessageDelta() {
	if d.messageDeltaSent {
		return
	}
	d.messageDeltaSent = true
	// The non-streaming decoder records a stop_reason approximation warning
	// for model_context_window_exceeded. This path committed the target status
	// line before message_delta arrived, and TranslateStreamBetween has no
	// warning channel, so the same length finish is applied silently.
	finish := &ir.Finish{Reason: anthropicFinishReason(d.pendingStopReason)}
	if d.pendingStopSequence != nil {
		finish.StopSequence = *d.pendingStopSequence
	}
	ev := &ir.Event{Type: ir.EventMessageDelta, Finish: finish}
	if d.usageReported {
		u := d.usage
		ev.Usage = &u
	}
	d.q.push(ev)
}

// pushTerminal closes the canonical event stream after the message delta. A
// terminal stop reason of "error" without an explicit error event synthesizes
// one, so a translated terminal carries a non-null failure instead of a clean
// end. An explicit provider error already terminated the stream and keeps its
// own type.
func (d *AnthropicStreamDecoder) pushTerminal() {
	if !d.errorSeen && anthropicFinishReason(d.pendingStopReason) == ir.FinishError {
		d.q.push(&ir.Event{Type: ir.EventError, Error: syntheticAnthropicFailure()})
		return
	}
	d.q.push(&ir.Event{Type: ir.EventDone})
}

// closeOpenBlocks emits one BlockEnd for every block still open when a
// terminal event arrives, in index order. Anthropic normally sends an explicit
// content_block_stop, but a provider may omit it before message_delta or
// message_stop; every target encoder requires the block lifecycle to close.
func (d *AnthropicStreamDecoder) closeOpenBlocks() {
	indices := make([]int, 0, len(d.openBlocks))
	for idx := range d.openBlocks {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	for _, idx := range indices {
		// A skipped provider block never announced a target block, so it must
		// not receive a stop when the terminal event closes open blocks.
		if d.skipped[idx] {
			continue
		}
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
	}
	clear(d.openBlocks)
	clear(d.skipped)
	clear(d.toolInitial)
	d.openToolBlocks = 0
	d.toolInitialBytes = 0
}

// FlushBlocks closes every block this decoder still holds open. The
// translation facade calls it when an upstream stream fails so a client never
// receives an error or terminal event with an unclosed block.
func (d *AnthropicStreamDecoder) FlushBlocks() ([]*ir.Event, error) {
	d.closeOpenBlocks()
	return drainQueue(&d.q), nil
}

// mergeUsage folds one Anthropic usage object into the stream total. The wire
// reports non-cached input_tokens separately, so the decoder retains the base
// count and the cache totals independently and recomputes the full prompt
// count after every event. A message_delta that repeats only input_tokens must
// update the base without erasing the cache totals established at
// message_start; otherwise the cached subsets appear to exceed the input and
// validation aborts the translation.
func (d *AnthropicStreamDecoder) mergeUsage(u *anthropicStreamUsage) error {
	if u.InputTokens != nil {
		d.usageBaseInput = *u.InputTokens
	}
	usage := d.usage
	if u.OutputTokens != nil {
		usage.OutputTokens = *u.OutputTokens
	}
	if u.CacheReadInputTokens != nil {
		usage.CacheReadTokens = *u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens != nil {
		usage.CacheWriteTokens = *u.CacheCreationInputTokens
	}
	if u.OutputTokensDetails != nil {
		usage.ReasoningTokens = u.OutputTokensDetails.ThinkingTokens
	}
	input, err := metrics.SumCounts(d.usageBaseInput, usage.CacheReadTokens, usage.CacheWriteTokens)
	if err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	usage.InputTokens = input
	total, err := metrics.SumCounts(usage.InputTokens, usage.OutputTokens)
	if err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	usage.TotalTokens = total
	if err := validateUsage(usage); err != nil {
		return err
	}
	d.usage = usage
	return nil
}

func anthropicStreamBlock(b anthropicWireBlock) (ir.Block, bool) {
	switch b.Type {
	case "text":
		return ir.Block{Type: ir.BlockText, Text: b.Text}, true
	case "tool_use":
		args := b.Input
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage(`{}`)
		}
		return ir.Block{Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
			ID: b.ID, Name: b.Name, Arguments: args,
		}}, true
	case "thinking":
		return ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
			Text: b.Thinking, Signature: b.Signature, Provider: ir.Anthropic,
		}}, true
	case "redacted_thinking":
		return ir.Block{Type: ir.BlockRedactedReasoning, Reasoning: &ir.Reasoning{
			Encrypted: b.Data, Provider: ir.Anthropic,
		}}, true
	default:
		return ir.Block{}, false
	}
}

// AnthropicStreamEncoder renders canonical events as Anthropic SSE.
type AnthropicStreamEncoder struct {
	w       io.Writer
	flusher http.Flusher

	id      string
	model   string
	created int64

	started    bool
	open       map[int]bool
	skipped    map[int]bool
	unopened   map[int]bool
	blockArgs  map[int]json.RawMessage
	argSent    map[int]bool
	finishSent bool
	usage      ir.Usage
	doneSent   bool
}

// NewAnthropicStreamEncoder builds an encoder over one client response writer.
func NewAnthropicStreamEncoder(w io.Writer, flusher http.Flusher) *AnthropicStreamEncoder {
	return &AnthropicStreamEncoder{
		w: w, flusher: flusher,
		open:      map[int]bool{},
		skipped:   map[int]bool{},
		unopened:  map[int]bool{},
		blockArgs: map[int]json.RawMessage{},
		argSent:   map[int]bool{},
	}
}

// anthropicStreamUsage renders the full cumulative usage a translated stream
// can carry. Anthropic reports non-cached input separately, so the cache
// counts travel beside it, and reasoning rides in output_tokens_details.
func anthropicUsageJSON(u ir.Usage) map[string]any {
	out := map[string]any{
		"input_tokens":                nonNegative(u.InputTokens - u.CacheReadTokens - u.CacheWriteTokens),
		"output_tokens":               u.OutputTokens,
		"cache_creation_input_tokens": u.CacheWriteTokens,
		"cache_read_input_tokens":     u.CacheReadTokens,
	}
	if u.ReasoningTokens != 0 {
		out["output_tokens_details"] = map[string]any{"thinking_tokens": u.ReasoningTokens}
	}
	return out
}

// startStream emits the mandatory message_start once, synthesizing identity for
// a source stream that never delivered its own start event.
func (e *AnthropicStreamEncoder) startStream() error {
	if e.started {
		return nil
	}
	e.started = true
	if e.id == "" {
		e.id = "msg_" + randomID()
	}
	if e.created == 0 {
		e.created = time.Now().Unix()
	}
	return e.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": e.id, "type": "message", "role": "assistant", "content": []any{},
			"model": e.model, "stop_reason": nil, "stop_sequence": nil,
			"usage": anthropicUsageJSON(e.usage),
		},
	})
}

// Write renders one canonical event.
func (e *AnthropicStreamEncoder) Write(ev *ir.Event) error {
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
			e.open[ev.Index] = true
			if err := e.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": ev.Index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return err
			}
			initial := ev.Block.Text
			if ev.Block.Type == ir.BlockRefusal {
				initial = ev.Block.Refusal
			}
			if initial != "" {
				return e.emitTextDelta(ev.Index, initial)
			}
			return nil
		case ir.BlockToolCall:
			tc := ev.Block.ToolCall
			if tc == nil {
				e.unopened[ev.Index] = true
				return nil
			}
			// Validate before any state changes or bytes reach the client.
			// The streaming target cannot repair an invalid id or name, so it
			// aborts instead of emitting a tool_use block Anthropic rejects.
			if err := validateAnthropicToolCall(tc); err != nil {
				return err
			}
			e.open[ev.Index] = true
			if len(tc.Arguments) > 0 && string(tc.Arguments) != "{}" {
				e.blockArgs[ev.Index] = tc.Arguments
			}
			return e.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": ev.Index,
				"content_block": map[string]any{
					"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{},
				},
			})
		case ir.BlockReasoning:
			e.open[ev.Index] = true
			if err := e.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": ev.Index,
				"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
			}); err != nil {
				return err
			}
			if ev.Block.Reasoning != nil {
				if ev.Block.Reasoning.Text != "" {
					if err := e.emitReasoningDelta(ev.Index, ev.Block.Reasoning.Text); err != nil {
						return err
					}
				}
				if ev.Block.Reasoning.Signature != "" {
					return e.emitSignatureDelta(ev.Index, ev.Block.Reasoning.Signature)
				}
			}
			return nil
		case ir.BlockRedactedReasoning:
			data := ""
			if ev.Block.Reasoning != nil {
				data = ev.Block.Reasoning.Encrypted
			}
			e.open[ev.Index] = true
			e.skipped[ev.Index] = true
			return e.emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": ev.Index,
				"content_block": map[string]any{"type": "redacted_thinking", "data": data},
			})
		default:
			// No Anthropic block shape exists for this assistant block. Do
			// not open one; the matching EventBlockEnd must not emit a stop
			// for an index the client never saw opened.
			e.unopened[ev.Index] = true
			return nil
		}
	case ir.EventBlockDelta:
		if ev.Delta == nil || e.skipped[ev.Index] {
			return nil
		}
		switch {
		case ev.Delta.Text != "":
			return e.emitTextDelta(ev.Index, ev.Delta.Text)
		case ev.Delta.Reasoning != "":
			return e.emitReasoningDelta(ev.Index, ev.Delta.Reasoning)
		case ev.Delta.Signature != "":
			return e.emitSignatureDelta(ev.Index, ev.Delta.Signature)
		case ev.Delta.Arguments != "":
			e.argSent[ev.Index] = true
			return e.emitJSONDelta(ev.Index, ev.Delta.Arguments)
		case ev.Delta.Citation != nil:
			return e.emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": ev.Index,
				"delta": map[string]any{"type": "citations_delta", "citation": ev.Delta.Citation},
			})
		}
	case ir.EventBlockEnd:
		if e.unopened[ev.Index] {
			delete(e.unopened, ev.Index)
			return nil
		}
		if args, ok := e.blockArgs[ev.Index]; ok && !e.argSent[ev.Index] {
			e.argSent[ev.Index] = true
			if err := e.emitJSONDelta(ev.Index, string(args)); err != nil {
				return err
			}
		}
		delete(e.blockArgs, ev.Index)
		delete(e.argSent, ev.Index)
		delete(e.skipped, ev.Index)
		delete(e.open, ev.Index)
		return e.emit("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": ev.Index,
		})
	case ir.EventMessageDelta:
		if ev.Usage != nil {
			if err := validateUsage(*ev.Usage); err != nil {
				return err
			}
			e.usage = *ev.Usage
		}
		stopReason := "end_turn"
		var stopSequence any
		if ev.Finish != nil {
			stopReason = anthropicStopReason(ev.Finish.Reason)
			if ev.Finish.StopSequence != "" {
				stopSequence = ev.Finish.StopSequence
			}
		}
		e.finishSent = true
		return e.emit("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": stopSequence},
			"usage": anthropicUsageJSON(e.usage),
		})
	case ir.EventDone:
		if !e.finishSent {
			e.finishSent = true
			if err := e.emit("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
				"usage": anthropicUsageJSON(e.usage),
			}); err != nil {
				return err
			}
		}
		return e.finish()
	case ir.EventError:
		if ev.Error != nil {
			typ := ev.Error.Type
			if typ == "" {
				typ = "api_error"
			}
			if err := e.emit("error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": typ, "message": ev.Error.Message},
			}); err != nil {
				return err
			}
		}
		return e.finish()
	}
	return nil
}

func (e *AnthropicStreamEncoder) emitTextDelta(index int, text string) error {
	return e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
}

func (e *AnthropicStreamEncoder) emitReasoningDelta(index int, thinking string) error {
	return e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": thinking},
	})
}

func (e *AnthropicStreamEncoder) emitSignatureDelta(index int, signature string) error {
	return e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "signature_delta", "signature": signature},
	})
}

func (e *AnthropicStreamEncoder) emitJSONDelta(index int, partial string) error {
	return e.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})
}

func (e *AnthropicStreamEncoder) emit(name string, obj any) error {
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
	if _, err := e.w.Write(frame); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

func (e *AnthropicStreamEncoder) finish() error {
	if e.doneSent {
		return nil
	}
	if err := e.startStream(); err != nil {
		return err
	}
	e.doneSent = true
	return e.emit("message_stop", map[string]any{"type": "message_stop"})
}

// Close finishes the stream if the caller did not deliver EventDone.
func (e *AnthropicStreamEncoder) Close() error { return e.finish() }

// Abort closes every content block this encoder opened without flushing any
// retained tool input. The proxy terminates the committed stream with its
// protocol-native error frame immediately afterwards.
func (e *AnthropicStreamEncoder) Abort() error {
	for _, idx := range sortedKeys(e.open) {
		if err := e.emit("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": idx,
		}); err != nil {
			return err
		}
		delete(e.open, idx)
	}
	clear(e.blockArgs)
	clear(e.argSent)
	clear(e.skipped)
	clear(e.unopened)
	return nil
}
