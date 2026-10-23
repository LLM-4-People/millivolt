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

	id      string
	model   string
	created int64

	nextIndex int
	itemType  map[int]string
	itemBlock map[int]int
	partBlock map[[2]int]int
	argsSeen  map[int]bool
	open      map[int]bool

	usage    ir.Usage
	warnings ir.Warnings
	finish   ir.FinishReason
}

// NewResponsesStreamDecoder builds a decoder over one upstream body.
func NewResponsesStreamDecoder(r io.Reader) *ResponsesStreamDecoder {
	return &ResponsesStreamDecoder{
		r:         newSSEReader(r),
		itemType:  map[int]string{},
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
			d.itemType[ev.OutputIndex] = item.Type
			switch item.Type {
			case "function_call":
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
			idx := d.newBlock()
			d.partBlock[[2]int{ev.OutputIndex, ev.ContentIndex}] = idx
			var block ir.Block
			switch part.Type {
			case "output_text", "input_text", "text":
				block = ir.Block{Type: ir.BlockText, Text: part.Text}
			case "refusal":
				block = ir.Block{Type: ir.BlockRefusal, Refusal: part.Refusal}
			case "reasoning_text":
				block = ir.Block{Type: ir.BlockReasoning, Reasoning: &ir.Reasoning{Text: part.Text, Provider: ir.Responses}}
			default:
				d.warnings.Add(ir.WarningDropped, "content."+part.Type, "unsupported content part")
				continue
			}
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
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			idx, ok := d.itemBlock[ev.OutputIndex]
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
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if item.Type == "reasoning" {
				idx, ok := d.itemBlock[ev.OutputIndex]
				if !ok {
					continue
				}
				delete(d.itemBlock, ev.OutputIndex)
				d.q.push(&ir.Event{Type: ir.EventBlockEnd, Index: idx})
			} else if item.Type == "message" {
				// Some providers skip content_part.done; close any part
				// block that is still open for this output item.
				for key, idx := range d.partBlock {
					if key[0] == ev.OutputIndex {
						delete(d.partBlock, key)
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
			} else if hasToolCall(d.blocksFromItems(resp.Output)) {
				finish = ir.FinishToolCalls
			}
			u := d.usage
			d.q.push(&ir.Event{Type: ir.EventMessageDelta, Finish: &ir.Finish{Reason: finish}, Usage: &u})
			d.q.push(&ir.Event{Type: ir.EventDone})
		case "error":
			d.terminal = true
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

// Write renders one canonical event.
func (e *ResponsesStreamEncoder) Write(ev *ir.Event) error {
	switch ev.Type {
	case ir.EventStart:
		if ev.Response != nil {
			e.id, e.model, e.created = ev.Response.ID, ev.Response.Model, ev.Response.Created
			e.usage = ev.Response.Usage
		}
		if e.created == 0 {
			e.created = time.Now().Unix()
		}
		e.started = true
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
	case ir.EventBlockStart:
		if ev.Block == nil {
			return nil
		}
		item := &responsesItemState{
			id:          e.newItemID(ev.Block.Type),
			outputIndex: e.outputIndex,
			kind:        string(ev.Block.Type),
		}
		e.outputIndex++
		e.items[ev.Index] = item
		e.order = append(e.order, ev.Index)
		switch ev.Block.Type {
		case ir.BlockText, ir.BlockRefusal:
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
			return e.emit("response.content_part.added", map[string]any{
				"type": "response.content_part.added", "sequence_number": e.nextSeq(),
				"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
				"part": part,
			})
		case ir.BlockToolCall:
			tc := ev.Block.ToolCall
			item.kind = "function_call"
			item.name = tc.Name
			item.callID = tc.ID
			return e.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item": map[string]any{
					"type": "function_call", "id": item.id, "call_id": tc.ID,
					"name": tc.Name, "arguments": "", "status": "in_progress",
				},
			})
		case ir.BlockReasoning:
			item.kind = "reasoning"
			return e.emit("response.output_item.added", map[string]any{
				"type": "response.output_item.added", "sequence_number": e.nextSeq(),
				"output_index": item.outputIndex,
				"item": map[string]any{
					"type": "reasoning", "id": item.id, "summary": []any{}, "status": "in_progress",
				},
			})
		default:
			item.kind = "skipped"
			return nil
		}
	case ir.EventBlockDelta:
		item := e.items[ev.Index]
		if item == nil || item.kind == "skipped" || ev.Delta == nil {
			return nil
		}
		switch {
		case ev.Delta.Text != "":
			if item.kind == "message" {
				if item.partType == "refusal" {
					item.refusal.WriteString(ev.Delta.Text)
					return e.emit("response.refusal.delta", map[string]any{
						"type": "response.refusal.delta", "sequence_number": e.nextSeq(),
						"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
						"delta": ev.Delta.Text,
					})
				}
				item.text.WriteString(ev.Delta.Text)
				return e.emit("response.output_text.delta", map[string]any{
					"type": "response.output_text.delta", "sequence_number": e.nextSeq(),
					"item_id": item.id, "output_index": item.outputIndex, "content_index": 0,
					"delta": ev.Delta.Text, "logprobs": []any{},
				})
			}
		case ev.Delta.Arguments != "":
			if item.kind == "function_call" {
				item.args.WriteString(ev.Delta.Arguments)
				return e.emit("response.function_call_arguments.delta", map[string]any{
					"type": "response.function_call_arguments.delta", "sequence_number": e.nextSeq(),
					"item_id": item.id, "output_index": item.outputIndex, "delta": ev.Delta.Arguments,
				})
			}
		case ev.Delta.Reasoning != "":
			if item.kind == "reasoning" {
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
				item.reasoning.WriteString(ev.Delta.Reasoning)
				return e.emit("response.reasoning_summary_text.delta", map[string]any{
					"type": "response.reasoning_summary_text.delta", "sequence_number": e.nextSeq(),
					"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
					"delta": ev.Delta.Reasoning,
				})
			}
		}
	case ir.EventBlockEnd:
		item := e.items[ev.Index]
		if item == nil || item.kind == "skipped" {
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
				"item": map[string]any{
					"type": "reasoning", "id": item.id, "summary": []any{
						map[string]any{"type": "summary_text", "text": text},
					}, "status": "completed",
				},
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
		if item == nil || item.kind == "skipped" {
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
			out = append(out, map[string]any{
				"type": "reasoning", "id": item.id, "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": item.reasoning.String()}},
			})
		}
	}
	return out
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
	case ir.BlockReasoning:
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
