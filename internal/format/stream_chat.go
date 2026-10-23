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
	warnings      ir.Warnings
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

// Warnings returns the decode-time translation warnings.
func (d *ChatStreamDecoder) Warnings() ir.Warnings { return d.warnings }

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
			d.started = true
			d.id, d.model, d.created = chunk.ID, chunk.Model, chunk.Created
			if d.created == 0 {
				d.created = time.Now().Unix()
			}
			d.q.push(&ir.Event{Type: ir.EventStart, Response: &ir.Response{
				ID: d.id, Model: d.model, Created: d.created,
			}})
		}
		if len(chunk.Choices) > 1 {
			d.warnings.Add(ir.WarningDropped, "choices", "only the first choice is translated")
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			delta := choice.Delta
			if delta.Content != nil && *delta.Content != "" {
				d.ensureText()
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: d.textIndex, Delta: &ir.Delta{Text: *delta.Content}})
			}
			reasoning := delta.ReasoningContent
			if reasoning == "" {
				reasoning = delta.Reasoning
			}
			if reasoning != "" {
				d.ensureReasoning()
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: d.reasoningIndex, Delta: &ir.Delta{Reasoning: reasoning}})
			}
			if delta.Refusal != nil && *delta.Refusal != "" {
				d.ensureRefusal()
				d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: d.refusalIndex, Delta: &ir.Delta{Text: *delta.Refusal}})
			}
			for _, tc := range delta.ToolCalls {
				d.toolCall(tc.Index, tc.ID, tc.Type, tc.Function.Name, tc.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				d.closeAll()
				d.finishSeen = true
				d.pendingFinish = &ir.Finish{Reason: irFinishReason(*choice.FinishReason)}
			}
		}
		if chunk.Usage != nil {
			d.usage = decodeChatUsage(chunk.Usage)
		}
	}
}

// flushFinish emits the single terminal MessageDelta. Finish and usage may
// arrive in separate chunks (opaque providers do not guarantee their order),
// so both are held until the stream's terminal marker and delivered once.
func (d *ChatStreamDecoder) flushFinish() {
	if d.pendingFinish == nil && d.usage == (ir.Usage{}) {
		return
	}
	ev := &ir.Event{Type: ir.EventMessageDelta, Finish: d.pendingFinish}
	if d.usage != (ir.Usage{}) {
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

func (d *ChatStreamDecoder) toolCall(chatIndex int, id, typ, name, args string) {
	irIndex, ok := d.toolBlocks[chatIndex]
	if !ok {
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
}

func (d *ChatStreamDecoder) closeText() {
	if d.textIndex >= 0 {
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: d.textIndex})
		d.textIndex = -1
	}
}

func (d *ChatStreamDecoder) closeReasoning() {
	if d.reasoningIndex >= 0 {
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: d.reasoningIndex})
		d.reasoningIndex = -1
	}
}

func (d *ChatStreamDecoder) closeRefusal() {
	if d.refusalIndex >= 0 {
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: d.refusalIndex})
		d.refusalIndex = -1
	}
}

func (d *ChatStreamDecoder) closeAll() {
	d.closeText()
	d.closeReasoning()
	d.closeRefusal()
	for _, idx := range d.open {
		if idx == d.textIndex || idx == d.reasoningIndex || idx == d.refusalIndex {
			continue
		}
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
	}
	d.open = nil
	d.toolBlocks = map[int]int{}
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
	doneSent    bool
}

// NewChatStreamEncoder builds an encoder over one client response writer.
func NewChatStreamEncoder(w io.Writer, flusher http.Flusher) *ChatStreamEncoder {
	return &ChatStreamEncoder{
		w: w, flusher: flusher,
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
				e.argsEmitted[ev.Index] = true
				return e.emit(map[string]any{"tool_calls": []any{map[string]any{
					"index": idx, "function": map[string]any{"arguments": args},
				}}}, nil, nil)
			}
		}
	case ir.EventMessageDelta:
		if ev.Finish != nil && !e.finishSent {
			e.finishSent = true
			if err := e.emit(map[string]any{}, ChatFinishReason(ev.Finish.Reason), nil); err != nil {
				return err
			}
		}
		if ev.Usage != nil {
			u := *ev.Usage
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

func (e *ChatStreamEncoder) emit(delta map[string]any, finish any, extra map[string]any) error {
	obj := sse.Chunk(e.id, e.created, e.model, []map[string]any{sse.DeltaChoice(delta, finish)}, extra)
	return sse.EmitFrame(e.w, e.flusher, obj)
}

func (e *ChatStreamEncoder) emitUsage() error {
	if e.usage == nil || e.usageSent {
		return nil
	}
	e.usageSent = true
	obj := sse.Chunk(e.id, e.created, e.model, []map[string]any{}, map[string]any{
		"usage": encodeChatUsage(*e.usage),
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

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
