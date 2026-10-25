package format

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

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
	// pendingDone defers a message item's final output_item.done until the
	// terminal status is known. The block closes before the canonical
	// MessageDelta, so emitting "completed" at BlockEnd would lie for a
	// length/content-filter/failed turn.
	pendingDone *responsesItemState
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
	// status records the lifecycle status the item reached when it closed:
	// completed for a normal close and incomplete for an aborted one. It stays
	// empty while the item is still open.
	status string
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
				if ev.Block.Reasoning.Encrypted != "" &&
					(ev.Block.Reasoning.Provider == "" || ev.Block.Reasoning.Provider == ir.Responses) {
					item.encrypted = ev.Block.Reasoning.Encrypted
					if err := e.retain(len(item.encrypted), "reasoning artifact"); err != nil {
						return err
					}
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
	if err := e.flushPendingDone(); err != nil {
		return err
	}
	response, err := e.responseObject(status, incompleteReason, e.outputItems(status))
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

func (e *ResponsesStreamEncoder) outputItems(status string) []map[string]any {
	var out []map[string]any
	for _, idx := range e.order {
		item := e.items[idx]
		if item == nil {
			continue
		}
		// A failed turn omits an open function call: its argument value is
		// incomplete and must not appear as an executable call. Other open
		// items keep the incomplete status their terminal gives them.
		if e.failed != nil && e.open[idx] && item.kind == "function_call" {
			continue
		}
		// Each item owns the status it reached when it closed. Only an item
		// still open at a non-completed terminal inherits the terminal status;
		// an item that closed normally stays completed.
		itemStatus := item.status
		if itemStatus == "" {
			itemStatus = "completed"
			if status != "completed" {
				itemStatus = "incomplete"
			}
		}
		switch item.kind {
		case "message":
			out = append(out, messageItemJSON(item, itemStatus))
		case "function_call":
			args := item.args.String()
			if args == "" {
				args = "{}"
			}
			out = append(out, map[string]any{
				"type": "function_call", "id": item.id, "call_id": item.callID,
				"name": item.name, "arguments": args, "status": itemStatus,
			})
		case "reasoning":
			out = append(out, reasoningItem(item, item.reasoning.String(), itemStatus))
		}
	}
	return out
}

// messageItemJSON renders one message item with the supplied lifecycle status.
func messageItemJSON(item *responsesItemState, status string) map[string]any {
	var part map[string]any
	if item.partType == "refusal" {
		part = map[string]any{"type": "refusal", "refusal": item.refusal.String()}
	} else {
		part = map[string]any{
			"type": "output_text", "text": item.text.String(), "annotations": []any{}, "logprobs": []any{},
		}
	}
	return map[string]any{
		"type": "message", "id": item.id, "status": status, "role": "assistant",
		"content": []any{part},
	}
}

// deferMessageDone holds a closed message item's final output_item.done until
// the next item starts or the stream ends. A preceding deferred item is
// flushed with its own close status first, so event order and the item count
// stay unchanged.
func (e *ResponsesStreamEncoder) deferMessageDone(item *responsesItemState) error {
	if err := e.flushPendingDone(); err != nil {
		return err
	}
	e.pendingDone = item
	return nil
}

// flushPendingDone emits the deferred message done frame with the status the
// item reached when it closed. A deferred item is always a normal close, so
// the terminal envelope cannot rewrite it.
func (e *ResponsesStreamEncoder) flushPendingDone() error {
	item := e.pendingDone
	if item == nil {
		return nil
	}
	e.pendingDone = nil
	status := item.status
	if status == "" {
		status = "completed"
	}
	return e.emit("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": e.nextSeq(),
		"output_index": item.outputIndex, "item": messageItemJSON(item, status),
	})
}

// closeItem emits the normal completion frames for one open Responses item.
// The caller removes the item from the open set only after every frame lands.
func (e *ResponsesStreamEncoder) closeItem(item *responsesItemState) error {
	item.status = "completed"
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
			return e.deferMessageDone(item)
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
		return e.deferMessageDone(item)
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
				"name": item.name, "arguments": args, "status": item.status,
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
			"item":         reasoningItem(item, text, item.status),
		})
	}
	return nil
}

// abortItem closes one non-tool Responses item after a target-side failure. It
// emits an empty close for a content part or reasoning summary the encoder
// opened, then the item done frame, and never flushes retained text or
// reasoning. Abort leaves an open function call for the proxy error frame, so
// this helper has no function-call shape.
func (e *ResponsesStreamEncoder) abortItem(item *responsesItemState) error {
	item.status = "incomplete"
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
	closed := map[string]any{"type": item.kind, "id": item.id, "status": item.status}
	switch item.kind {
	case "message":
		closed["role"] = "assistant"
		closed["content"] = []any{}
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
// (when the part was opened) followed by an incomplete item done. An open
// function_call is left without a done item because that item would carry
// arguments {}, an executable empty call; the proxy error frame terminates the
// stream instead.
func (e *ResponsesStreamEncoder) Abort() error {
	if err := e.flushPendingDone(); err != nil {
		return err
	}
	for _, idx := range e.order {
		if !e.open[idx] {
			continue
		}
		item := e.items[idx]
		if item == nil {
			continue
		}
		if item.kind == "function_call" {
			// An open function_call has no validated argument value. Its done
			// item would carry arguments {}, an executable empty call, so it
			// stays incomplete and the proxy's error frame terminates the
			// stream.
			delete(e.open, idx)
			continue
		}
		if err := e.abortItem(item); err != nil {
			return err
		}
		delete(e.open, idx)
	}
	return nil
}

// reasoningItem renders one Responses reasoning item with its close status. An
// opaque encrypted artifact the source provided (Anthropic redacted_thinking
// or a Responses encrypted reasoning item) is preserved where the target
// accepts it.
func reasoningItem(item *responsesItemState, text, status string) map[string]any {
	summary := []any{}
	if text != "" {
		summary = []any{map[string]any{"type": "summary_text", "text": text}}
	}
	out := map[string]any{
		"type": "reasoning", "id": item.id, "status": status, "summary": summary,
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
	if err := e.flushPendingDone(); err != nil {
		return nil, err
	}
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
