package format

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// ResponsesStreamRetainedMaxBytes bounds the text, arguments, reasoning and
// refusal one synthesized Responses item retains for its done and terminal
// events. The wire requires those events to carry the full value, so the
// encoder buffers it; the relay sizes its translated-frame scanner from this
// bound. Internal safety guardrail, not user-tunable.
const ResponsesStreamRetainedMaxBytes = 8 << 20

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
	argsSeen  map[int]bool
	open      map[int]bool

	usage       ir.Usage
	warnings    ir.Warnings
	finish      ir.FinishReason
	sawToolCall bool
}

// NewResponsesStreamDecoder builds a decoder over one upstream body.
func NewResponsesStreamDecoder(r io.Reader) *ResponsesStreamDecoder {
	return &ResponsesStreamDecoder{
		r:         newSSEReader(r),
		itemBlock: map[int]int{},
		partBlock: map[[2]int]int{},
		argsSeen:  map[int]bool{},
		open:      map[int]bool{},
	}
}

// Warnings returns the decode-time translation warnings.
func (d *ResponsesStreamDecoder) Warnings() ir.Warnings { return d.warnings }

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
			d.id, d.model, d.created = resp.ID, resp.Model, resp.CreatedAt
			if d.created == 0 {
				d.created = time.Now().Unix()
			}
			if resp.Usage != nil {
				d.usage = responsesUsageToIR(resp.Usage)
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
			switch item.Type {
			case "function_call":
				d.sawToolCall = true
				idx := d.newBlock()
				d.itemBlock[ev.OutputIndex] = idx
				args := item.Arguments
				if args == "" {
					args = "{}"
				}
				d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &ir.Block{
					Type: ir.BlockToolCall, ToolCall: &ir.ToolCall{
						ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(`{}`),
					},
				}})
			case "reasoning":
				idx := d.newBlock()
				d.itemBlock[ev.OutputIndex] = idx
				d.q.push(&ir.Event{Type: ir.EventBlockStart, Index: idx, Block: &ir.Block{
					Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{
						ID: item.ID, Encrypted: item.EncryptedContent, Provider: ir.Responses,
					},
				}})
			case "message":
				// Content parts arrive in response.content_part.added.
			default:
				d.warnings.Add(ir.WarningDropped, "output."+item.Type, "provider-specific item has no cross-protocol equivalent")
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
				d.warnings.Add(ir.WarningDropped, "content."+part.Type, "unsupported content part")
				continue
			}
			idx := d.newBlock()
			d.partBlock[[2]int{ev.OutputIndex, ev.ContentIndex}] = idx
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
			d.argsSeen[ev.OutputIndex] = true
			d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Arguments: ev.Delta}})
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
			idx, ok := d.partBlock[[2]int{ev.OutputIndex, ev.ContentIndex}]
			if !ok {
				continue
			}
			delete(d.partBlock, [2]int{ev.OutputIndex, ev.ContentIndex})
			delete(d.open, idx)
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
				if !d.argsSeen[ev.OutputIndex] && item.Arguments != "" {
					d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Arguments: item.Arguments}})
				}
				delete(d.itemBlock, ev.OutputIndex)
				delete(d.open, idx)
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
		case "response.completed", "response.incomplete", "response.failed":
			var resp responsesWireResponse
			if err := json.Unmarshal(ev.Response, &resp); err != nil {
				return nil, err
			}
			if resp.Usage != nil {
				d.usage = responsesUsageToIR(resp.Usage)
			}
			d.terminal = true
			d.closeAll(resp.Output)
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
			if ev.Type == "response.incomplete" {
				finish = ir.FinishLength
				if resp.IncompleteDetails != nil && resp.IncompleteDetails.Reason == "content_filter" {
					finish = ir.FinishContentFilter
				}
			} else if d.sawToolCall || hasToolCall(d.blocksFromItems(resp.Output)) {
				finish = ir.FinishToolCalls
			}
			u := d.usage
			d.q.push(&ir.Event{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: finish}, Usage: &u})
			d.q.push(&ir.Event{Type: ir.EventDone})
		case "error":
			d.terminal = true
			d.closeAll(nil)
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

// closeAll closes every block still open at a terminal event. The terminal
// response may be the only place a function call's arguments appear, so flush
// them before closing its block. Anthropic requires a content_block_stop for
// every opened block before message_delta.
func (d *ResponsesStreamDecoder) closeAll(output []responsesWireItem) {
	for i, item := range output {
		if item.Type != "function_call" || d.argsSeen[i] || item.Arguments == "" {
			continue
		}
		idx, ok := d.itemBlock[i]
		if !ok {
			continue
		}
		d.argsSeen[i] = true
		d.q.push(&ir.Event{Type: ir.EventBlockDelta, Index: idx, Delta: &ir.Delta{Arguments: item.Arguments}})
	}
	for _, idx := range sortedKeys(d.open) {
		d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
		delete(d.open, idx)
	}
	clear(d.partBlock)
	clear(d.itemBlock)
}

// FlushBlocks closes every block this decoder still holds open. The
// translation facade calls it when an upstream stream fails so a client never
// receives an error or terminal event with an unclosed block.
func (d *ResponsesStreamDecoder) FlushBlocks() []*ir.Event {
	d.closeAll(nil)
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
	order       []int
	finish      ir.FinishReason
	usage       ir.Usage
	failed      *ir.Error
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
	summaryOpen  bool
}

// NewResponsesStreamEncoder builds an encoder over one client response writer.
func NewResponsesStreamEncoder(w io.Writer, flusher http.Flusher) *ResponsesStreamEncoder {
	return &ResponsesStreamEncoder{w: w, flusher: flusher, items: map[int]*responsesItemState{}}
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
	if err := e.emit("response.created", map[string]any{
		"type": "response.created", "sequence_number": e.nextSeq(),
		"response": e.responseObject("in_progress", nil),
	}); err != nil {
		return err
	}
	return e.emit("response.in_progress", map[string]any{
		"type": "response.in_progress", "sequence_number": e.nextSeq(),
		"response": e.responseObject("in_progress", nil),
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
			item := e.startItem(ev.Index, ev.Block.Type)
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
			initial := ev.Block.Text
			if item.partType == "refusal" {
				initial = ev.Block.Refusal
			}
			return e.appendText(item, initial)
		case ir.BlockToolCall:
			tc := ev.Block.ToolCall
			item := e.startItem(ev.Index, ev.Block.Type)
			item.kind = "function_call"
			if tc != nil {
				item.name = tc.Name
				item.callID = tc.ID
				if len(tc.Arguments) > 0 && string(tc.Arguments) != "{}" {
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
			item := e.startItem(ev.Index, ev.Block.Type)
			item.kind = "reasoning"
			initial := ""
			if ev.Block.Reasoning != nil {
				item.encrypted = ev.Block.Reasoning.Encrypted
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
				if item.args.Len()+len(ev.Delta.Arguments) > ResponsesStreamRetainedMaxBytes {
					return fmt.Errorf("responses function arguments exceed %d bytes", ResponsesStreamRetainedMaxBytes)
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
			}
			return e.emit("response.output_item.done", map[string]any{
				"type": "response.output_item.done", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item":         reasoningItem(item, text),
			})
		}
	case ir.EventMessageDelta:
		if ev.Finish != nil {
			e.finish = ev.Finish.Reason
		}
		if ev.Usage != nil {
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
	if e.finish == ir.FinishLength || e.finish == ir.FinishContentFilter {
		status = "incomplete"
	}
	if e.failed != nil {
		status = "failed"
	}
	return e.emit("response."+status, map[string]any{
		"type": "response." + status, "sequence_number": e.nextSeq(),
		"response": e.responseObject(status, e.outputItems()),
	})
}

func (e *ResponsesStreamEncoder) responseObject(status string, output []map[string]any) map[string]any {
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
		reason := "max_output_tokens"
		if e.finish == ir.FinishContentFilter {
			reason = "content_filter"
		}
		obj["incomplete_details"] = map[string]any{"reason": reason}
	}
	if status == "failed" && e.failed != nil {
		code := e.failed.Code
		if code == "" {
			code = "server_error"
		}
		obj["error"] = map[string]any{"code": code, "message": e.failed.Message}
	}
	if e.usage != (ir.Usage{}) {
		obj["usage"] = encodeResponsesUsage(e.usage)
	}
	return obj
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
// canonical block. Only a block whose target shape exists may call it.
func (e *ResponsesStreamEncoder) startItem(index int, kind ir.BlockType) *responsesItemState {
	item := &responsesItemState{
		id:          e.newItemID(kind),
		outputIndex: e.outputIndex,
		kind:        string(kind),
	}
	e.outputIndex++
	e.items[index] = item
	e.order = append(e.order, index)
	return item
}

// appendText retains and emits one message text or refusal delta. A block-start
// payload and later deltas share the same path, so an initial value is never
// dropped when no delta follows.
func (e *ResponsesStreamEncoder) appendText(item *responsesItemState, delta string) error {
	if delta == "" {
		return nil
	}
	if item.partType == "refusal" {
		if item.refusal.Len()+len(delta) > ResponsesStreamRetainedMaxBytes {
			return fmt.Errorf("responses refusal exceeds %d bytes", ResponsesStreamRetainedMaxBytes)
		}
		item.refusal.WriteString(delta)
		return e.emit("response.refusal.delta", map[string]any{
			"type": "response.refusal.delta", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
			"delta": delta,
		})
	}
	if item.text.Len()+len(delta) > ResponsesStreamRetainedMaxBytes {
		return fmt.Errorf("responses output text exceeds %d bytes", ResponsesStreamRetainedMaxBytes)
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
		item.summaryOpen = true
		if err := e.emit("response.reasoning_summary_part.added", map[string]any{
			"type": "response.reasoning_summary_part.added", "sequence_number": e.nextSeq(),
			"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}); err != nil {
			return err
		}
	}
	if item.reasoning.Len()+len(delta) > ResponsesStreamRetainedMaxBytes {
		return fmt.Errorf("responses reasoning exceeds %d bytes", ResponsesStreamRetainedMaxBytes)
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
