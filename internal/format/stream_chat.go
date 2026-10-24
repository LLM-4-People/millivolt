package format

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// Chat Completions streaming codec.

type chatWireChunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string  `json:"role"`
			Content          *string `json:"content"`
			ReasoningContent string  `json:"reasoning_content"`
			Reasoning        string  `json:"reasoning"`
			Refusal          *string `json:"refusal"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatWireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// ChatStreamDecoder converts Chat Completions SSE chunks into canonical
// events.
type ChatStreamDecoder struct {
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

	nextIndex      int
	textIndex      int
	reasoningIndex int
	refusalIndex   int
	toolBlocks     map[int]int
	open           []int

	finishSeen    bool
	pendingFinish *ir.Finish
	usage         ir.Usage
	// usageReported distinguishes a source that reported usage (even all
	// zeros) from one that never did. Only a reported usage is forwarded.
	usageReported bool
}

// NewChatStreamDecoder builds a decoder over one upstream body.
func NewChatStreamDecoder(r io.Reader) *ChatStreamDecoder {
	return &ChatStreamDecoder{
		r:              newSSEReader(r),
		textIndex:      -1,
		reasoningIndex: -1,
		refusalIndex:   -1,
		toolBlocks:     map[int]int{},
	}
}

// Next returns the next canonical event.
func (d *ChatStreamDecoder) Next() (*ir.Event, error) {
	for {
		if ev, ok := d.q.pop(); ok {
			return ev, nil
		}
		if d.terminal {
			return nil, io.EOF
		}
		raw, err := d.r.Next()
		if err == io.EOF {
			if d.terminal {
				return nil, io.EOF
			}
			// A finish_reason is the documented end of a Chat completion. A
			// provider may close the connection without the optional [DONE]
			// sentinel; that is a complete stream, not a truncation.
			if d.finishSeen {
				d.closeAll()
				d.flushFinish()
				d.terminal = true
				d.q.push(&ir.Event{Type: ir.EventDone})
				continue
			}
			return nil, ErrTruncatedStream
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
		if strings.TrimSpace(string(raw.Data)) == "[DONE]" {
			d.closeAll()
			d.flushFinish()
			d.terminal = true
			d.q.push(&ir.Event{Type: ir.EventDone})
			continue
		}
		var chunk chatWireChunk
		if err := json.Unmarshal(raw.Data, &chunk); err != nil {
			return nil, err
		}
		if chunk.Error != nil {
			d.closeAll()
			d.terminal = true
			d.q.push(&ir.Event{Type: ir.EventError, Error: &ir.Error{
				Type: chunk.Error.Type, Code: stringifyCode(chunk.Error.Code), Message: chunk.Error.Message,
			}})
			continue
		}
		if !d.started {
			if err := checkNativeIdentity("id", chunk.ID); err != nil {
				return nil, err
			}
			if err := checkNativeIdentity("model", chunk.Model); err != nil {
				return nil, err
			}
			d.started = true
			d.id, d.model, d.created = chunk.ID, chunk.Model, chunk.Created
			if d.created == 0 {
				d.created = time.Now().Unix()
			}
			d.q.push(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
				ID: d.id, Model: d.model, Created: d.created,
			}})
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			delta := choice.Delta
			// Reasoning is thinking-first even when one provider chunk carries
			// both fields: opening text first would close it again immediately
			// and reverse the canonical block order.
			reasoning := delta.ReasoningContent
			if reasoning == "" {
				reasoning = delta.Reasoning
			}
			if reasoning != "" {
				d.ensureReasoning()
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: d.reasoningIndex, Delta: &ir.Delta{Reasoning: reasoning}})
			}
			if delta.Content != nil && *delta.Content != "" {
				d.ensureText()
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: d.textIndex, Delta: &ir.Delta{Text: *delta.Content}})
			}
			if delta.Refusal != nil && *delta.Refusal != "" {
				d.ensureRefusal()
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: d.refusalIndex, Delta: &ir.Delta{Text: *delta.Refusal}})
			}
			for _, tc := range delta.ToolCalls {
				if err := d.toolCall(tc.Index, tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments); err != nil {
					return nil, err
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				d.closeAll()
				d.finishSeen = true
				reason, _ := irFinishReason(*choice.FinishReason)
				d.pendingFinish = &ir.Finish{Reason: reason}
			}
		}
		if chunk.Usage != nil {
			usage := decodeChatUsage(chunk.Usage)
			if err := validateUsage(usage); err != nil {
				return nil, err
			}
			d.usage = usage
			d.usageReported = true
		}
	}
}

// flushFinish emits the single terminal MessageDelta. Finish and usage may
// arrive in separate chunks (opaque providers do not guarantee their order),
// so both are held until the stream's terminal marker and delivered once.
func (d *ChatStreamDecoder) flushFinish() {
	if d.pendingFinish == nil && !d.usageReported {
		return
	}
	ev := &ir.Event{Type: ir.EventMessageDelta, Finish: d.pendingFinish}
	if d.usageReported {
		u := d.usage
		ev.Usage = &u
	}
	d.pendingFinish = nil
	d.q.push(ev)
}

func stringifyCode(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case nil:
		return ""
	default:
		b, _ := json.Marshal(c)
		return string(b)
	}
}

func (d *ChatStreamDecoder) ensureText() {
	if d.textIndex >= 0 {
		return
	}
	d.closeReasoning()
	d.closeRefusal()
	d.textIndex = d.nextIndex
	d.nextIndex++
	d.open = append(d.open, d.textIndex)
	d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: d.textIndex, Block: &ir.Block{Type: ir.BlockText}})
}

func (d *ChatStreamDecoder) ensureReasoning() {
	if d.reasoningIndex >= 0 {
		return
	}
	d.closeText()
	d.closeRefusal()
	d.reasoningIndex = d.nextIndex
	d.nextIndex++
	d.open = append(d.open, d.reasoningIndex)
	d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: d.reasoningIndex, Block: &ir.Block{
		Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Provider: ir.Chat},
	}})
}

func (d *ChatStreamDecoder) ensureRefusal() {
	if d.refusalIndex >= 0 {
		return
	}
	d.closeText()
	d.closeReasoning()
	d.refusalIndex = d.nextIndex
	d.nextIndex++
	d.open = append(d.open, d.refusalIndex)
	d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: d.refusalIndex, Block: &ir.Block{Type: ir.BlockRefusal}})
}

func (d *ChatStreamDecoder) toolCall(chatIndex int, id, typ, name, args string) error {
	irIndex, ok := d.toolBlocks[chatIndex]
	if !ok {
		if err := checkNativeIdentity("tool id", id); err != nil {
			return err
		}
		if err := checkNativeIdentity("tool name", name); err != nil {
			return err
		}
		d.closeText()
		d.closeReasoning()
		d.closeRefusal()
		irIndex = d.nextIndex
		d.nextIndex++
		d.toolBlocks[chatIndex] = irIndex
		d.open = append(d.open, irIndex)
		d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: irIndex, Block: &ir.Block{
			Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)},
		}})
	}
	if args != "" {
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: irIndex, Delta: &ir.Delta{Arguments: args}})
	}
	return nil
}

func (d *ChatStreamDecoder) closeText() {
	if d.textIndex >= 0 {
		idx := d.textIndex
		d.textIndex = -1
		d.closeOpen(idx)
	}
}

func (d *ChatStreamDecoder) closeReasoning() {
	if d.reasoningIndex >= 0 {
		idx := d.reasoningIndex
		d.reasoningIndex = -1
		d.closeOpen(idx)
	}
}

func (d *ChatStreamDecoder) closeRefusal() {
	if d.refusalIndex >= 0 {
		idx := d.refusalIndex
		d.refusalIndex = -1
		d.closeOpen(idx)
	}
}

// closeOpen emits one BlockEnd for an index this decoder opened and removes it
// from the open set, so closeAll can never emit a second stop for that index.
func (d *ChatStreamDecoder) closeOpen(idx int) {
	d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
	for i, open := range d.open {
		if open == idx {
			d.open = append(d.open[:i], d.open[i+1:]...)
			return
		}
	}
}

func (d *ChatStreamDecoder) closeAll() {
	d.closeText()
	d.closeReasoning()
	d.closeRefusal()
	for _, idx := range d.open {
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
	}
	d.open = nil
	d.toolBlocks = map[int]int{}
}

// FlushBlocks closes every block this decoder still holds open. The
// translation facade calls it when an upstream stream fails so a client never
// receives an error or terminal event with an unclosed block.
func (d *ChatStreamDecoder) FlushBlocks() []*ir.Event {
	d.closeAll()
	return drainQueue(&d.q)
}

// ChatStreamEncoder renders canonical events as Chat Completions SSE.
type ChatStreamEncoder struct {
	w       io.Writer
	flusher http.Flusher

	id      string
	model   string
	created int64

	roleSent    bool
	toolIndex   map[int]int
	blockTypes  map[int]ir.BlockType
	blockArgs   map[int]json.RawMessage
	argsEmitted map[int]bool
	nextTool    int
	finishSent  bool
	usage       *ir.Usage
	usageSent   bool
	// includeUsage is the client's stream_options.include_usage. Official
	// Chat streaming sends a usage chunk only when the client asked for one.
	includeUsage bool
	doneSent     bool
}

// NewChatStreamEncoder builds an encoder over one client response writer.
// includeUsage mirrors the client's stream_options.include_usage: the extra
// usage chunk is part of the requested contract, not an unconditional frame.
func NewChatStreamEncoder(w io.Writer, flusher http.Flusher, includeUsage bool) *ChatStreamEncoder {
	return &ChatStreamEncoder{
		w: w, flusher: flusher, includeUsage: includeUsage,
		toolIndex: map[int]int{}, blockTypes: map[int]ir.BlockType{},
		blockArgs: map[int]json.RawMessage{}, argsEmitted: map[int]bool{},
	}
}

// Write renders one canonical event.
func (e *ChatStreamEncoder) Write(ev *ir.Event) error {
	switch ev.Type {
	case ir.EventStart:
		if ev.Response != nil {
			e.id, e.model, e.created = ev.Response.ID, ev.Response.Model, ev.Response.Created
		}
		if e.created == 0 {
			e.created = time.Now().Unix()
		}
		if !e.roleSent {
			e.roleSent = true
			return e.emit(map[string]any{"role": "assistant"}, nil, nil)
		}
	case ir.EventBlockStart:
		if ev.Block == nil {
			return nil
		}
		e.blockTypes[ev.Index] = ev.Block.Type
		if ev.Block.Type == ir.BlockToolCall && ev.Block.ToolCall != nil {
			idx := e.nextTool
			e.nextTool++
			e.toolIndex[ev.Index] = idx
			tc := map[string]any{
				"index": idx, "id": ev.Block.ToolCall.ID, "type": "function",
				"function": map[string]any{"name": ev.Block.ToolCall.Name, "arguments": ""},
			}
			if args := ev.Block.ToolCall.Arguments; len(args) > 0 && string(args) != "{}" {
				e.blockArgs[ev.Index] = args
			}
			return e.emit(map[string]any{"tool_calls": []any{tc}}, nil, nil)
		}
		return e.emitInitialBlock(ev.Block)
	case ir.EventBlockDelta:
		if ev.Delta == nil {
			return nil
		}
		switch {
		case ev.Delta.Text != "":
			if e.blockTypes[ev.Index] == ir.BlockRefusal {
				return e.emit(map[string]any{"refusal": ev.Delta.Text}, nil, nil)
			}
			return e.emit(map[string]any{"content": ev.Delta.Text}, nil, nil)
		case ev.Delta.Reasoning != "":
			return e.emit(map[string]any{"reasoning_content": ev.Delta.Reasoning}, nil, nil)
		case ev.Delta.Arguments != "":
			idx, ok := e.toolIndex[ev.Index]
			if !ok {
				return nil
			}
			e.argsEmitted[ev.Index] = true
			return e.emit(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "function": map[string]any{"arguments": ev.Delta.Arguments},
			}}}, nil, nil)
		}
	case ir.EventBlockEnd:
		if e.blockTypes[ev.Index] == ir.BlockToolCall && !e.argsEmitted[ev.Index] {
			if idx, ok := e.toolIndex[ev.Index]; ok {
				args := string(e.blockArgs[ev.Index])
				if args == "" {
					args = "{}"
				}
				e.releaseBlock(ev.Index)
				return e.emit(map[string]any{"tool_calls": []any{map[string]any{
					"index": idx, "function": map[string]any{"arguments": args},
				}}}, nil, nil)
			}
		}
		e.releaseBlock(ev.Index)
	case ir.EventMessageDelta:
		if ev.Finish != nil && !e.finishSent {
			e.finishSent = true
			if err := e.emit(map[string]any{}, ChatFinishReason(ev.Finish.Reason), nil); err != nil {
				return err
			}
		}
		if ev.Usage != nil {
			u := *ev.Usage
			if err := validateUsage(u); err != nil {
				return err
			}
			e.usage = &u
			if e.finishSent {
				return e.emitUsage()
			}
		}
	case ir.EventDone:
		if e.usage != nil && !e.usageSent {
			if err := e.emitUsage(); err != nil {
				return err
			}
		}
		return e.done()
	case ir.EventError:
		if ev.Error != nil {
			obj := map[string]any{"error": map[string]any{
				"message": ev.Error.Message,
				"type":    firstNonEmptyString(ev.Error.Type, sse.TypeUpstreamError),
			}}
			if ev.Error.Code != "" {
				obj["error"] = sse.ErrorObject(
					firstNonEmptyString(ev.Error.Type, sse.TypeUpstreamError), ev.Error.Code, ev.Error.Message)
			}
			if e.id != "" {
				obj["id"] = e.id
			}
			frame, err := sse.MarshalWireJSON(obj)
			if err != nil {
				return err
			}
			if _, err := e.w.Write(sse.DataFrame(frame)); err != nil {
				return err
			}
			if e.flusher != nil {
				e.flusher.Flush()
			}
		}
		return e.done()
	}
	return nil
}

// emitInitialBlock emits the payload a BlockStart already carries. Decoders
// may deliver complete text, a refusal or visible reasoning at start time
// (Responses content parts and some native presets); without this path the
// value is lost whenever no matching delta follows. A later delta is a
// separate increment, so this never duplicates it.
func (e *ChatStreamEncoder) emitInitialBlock(block *ir.Block) error {
	switch block.Type {
	case ir.BlockText:
		if block.Text != "" {
			return e.emit(map[string]any{"content": block.Text}, nil, nil)
		}
	case ir.BlockRefusal:
		if block.Refusal != "" {
			return e.emit(map[string]any{"refusal": block.Refusal}, nil, nil)
		}
	case ir.BlockReasoning:
		if text := chatReasoningText(block.Reasoning); text != "" {
			return e.emit(map[string]any{"reasoning_content": text}, nil, nil)
		}
	}
	return nil
}

// releaseBlock drops every per-block entry once the block is closed: a long
// stream of tool blocks must not grow encoder state for the whole request.
func (e *ChatStreamEncoder) releaseBlock(index int) {
	delete(e.toolIndex, index)
	delete(e.blockTypes, index)
	delete(e.blockArgs, index)
	delete(e.argsEmitted, index)
}

func (e *ChatStreamEncoder) emit(delta map[string]any, finish any, extra map[string]any) error {
	obj := sse.Chunk(e.id, e.created, e.model, []map[string]any{sse.DeltaChoice(delta, finish)}, extra)
	return sse.EmitFrame(e.w, e.flusher, obj)
}

func (e *ChatStreamEncoder) emitUsage() error {
	if !e.includeUsage || e.usage == nil || e.usageSent {
		return nil
	}
	e.usageSent = true
	usage, err := encodeChatUsage(*e.usage)
	if err != nil {
		return err
	}
	obj := sse.Chunk(e.id, e.created, e.model, []map[string]any{}, map[string]any{
		"usage": usage,
	})
	return sse.EmitFrame(e.w, e.flusher, obj)
}

func (e *ChatStreamEncoder) done() error {
	if e.doneSent {
		return nil
	}
	e.doneSent = true
	if _, err := io.WriteString(e.w, sse.DoneFrame); err != nil {
		return err
	}
	if e.flusher != nil {
		e.flusher.Flush()
	}
	return nil
}

// Close finishes the stream if the caller did not deliver EventDone.
func (e *ChatStreamEncoder) Close() error { return e.done() }

// Abort ends a failed translation. Chat Completions has no block close frame,
// so the chunks already written remain valid; only retained per-block state is
// dropped.
func (e *ChatStreamEncoder) Abort() error {
	e.toolIndex = nil
	e.blockTypes = nil
	e.blockArgs = nil
	e.argsEmitted = nil
	return nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
