package proxy

// Helpers for the resumable Cursor Run path (cursor_bidi.go): request parsing
// (tool results) and the OpenAI SSE/JSON emitters that render a turn's deltas.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// cursorToolResult is one role:"tool" message from a client request.
type cursorToolResult struct {
	toolCallID string
	name       string
	content    string
	isError    bool
}

// extractToolResults pulls the role:"tool" messages out of an OpenAI request
// body (the client's answers to a prior tool_calls turn).
func extractToolResults(body []byte) []cursorToolResult {
	var in struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			Name       string          `json:"name"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &in) != nil {
		return nil
	}
	var out []cursorToolResult
	for _, m := range in.Messages {
		if m.Role != "tool" {
			continue
		}
		tr := cursorToolResult{toolCallID: m.ToolCallID, name: m.Name}
		// content may be a string or an array of {type:"text",text} parts.
		if s, ok := flattenContent(m.Content); ok {
			tr.content = s
		}
		if tr.toolCallID != "" {
			out = append(out, tr)
		}
	}
	return out
}

// flattenContent is the proxy-side name for the shared metrics content walk,
// kept so the tool-result reader, input-token estimate and non-stream relay
// all consume one implementation.
func flattenContent(raw json.RawMessage) (string, bool) {
	return metrics.FlattenContent(raw)
}

// toolResultIDs returns the tool_call_ids of a request's role:"tool" messages.
func toolResultIDs(results []cursorToolResult) []string {
	var ids []string
	for _, tr := range results {
		ids = append(ids, tr.toolCallID)
	}
	return ids
}

// cursorToolResultMap converts a request's decoded tool results into the
// ResumeTurn map keyed by tool_call_id. Shared by both resume drivers.
func cursorToolResultMap(results []cursorToolResult) map[string]providerformat.ToolResult {
	m := make(map[string]providerformat.ToolResult, len(results))
	for _, tr := range results {
		m[tr.toolCallID] = providerformat.ToolResult{Text: tr.content}
	}
	return m
}

// startCursorRun builds and starts the resumable run over a live bidi
// stream: closeFn cancels the upstream request and closes the body pipe
// (only when the run truly ends), and the heartbeat cadence comes from the
// live config snapshot (a reload applies to the next run) - the canonical
// default lives in config.Default(). Callers keep resp/pw/upstreamCancel
// ownership outside the run: its Close is the only trigger.
func (s *Server) startCursorRun(pw *io.PipeWriter, body io.ReadCloser, blobs *providerformat.KVBlobStore, upstreamCancel context.CancelFunc) *providerformat.CursorRun {
	run := providerformat.NewCursorRun(pw, body, blobs, func() {
		upstreamCancel()
		pw.Close()
		body.Close()
	}, s.cfg().CursorHeartbeatInterval)
	run.Start()
	return run
}

// writeSSEHeaders writes the SSE response headers + status for a cursor stream.
func writeSSEHeaders(w http.ResponseWriter, status int) {
	if status == 0 {
		status = http.StatusOK
	}
	hdr := make(http.Header, 3)
	metrics.SetStreamHeaders(hdr)
	if p := pacerOf(w); p != nil {
		p.applyUpstream(hdr)
		p.commitStatus(status)
		return
	}
	copyResponseHeaders(w.Header(), hdr)
	w.WriteHeader(status)
}

// sseEmitter returns a function that emits one OpenAI chunk (delta + optional
// finish_reason) as an SSE frame to w, flushing after each. model stamps the
// request model on every chunk (OpenAI echoes it; clients may log it); the
// created timestamp is stamped per chunk, the cursor bridge's policy - the
// Anthropic bridge pins one per stream instead.
func sseEmitter(w http.ResponseWriter, id, model string, flusher http.Flusher) func(delta map[string]any, finish any) error {
	return func(delta map[string]any, finish any) error {
		obj := sse.Chunk(id, time.Now().Unix(), model,
			[]map[string]any{sse.DeltaChoice(delta, finish)}, nil)
		return sse.EmitFrame(w, flusher, obj)
	}
}

func pacerOf(w http.ResponseWriter) *ssePacer {
	for w != nil {
		if p, ok := w.(*ssePacer); ok {
			return p
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			break
		}
		w = u.Unwrap()
	}
	return nil
}

// writeClientError writes a JSON HTTP error if headers are still open, or the
// inbound protocol's in-band error + terminal pair if the idle pacer already
// committed 200.
func writeClientError(w http.ResponseWriter, rec *metrics.Record, inbound ir.Protocol, typ, msg string, code int) {
	writeClientErrorHdr(w, rec, inbound, typ, msg, code, nil)
}

// retryAfterHeader builds the single extra-header shape shared by every 429
// the proxy writes for its own queueing: Retry-After in whole seconds from
// the operator-tuned base. The denial type and message stay with each
// caller, and so do the record stamps: the queue-admission arm stamps
// queue_full plus the AcquireWith error text before its write, and the
// storm arm stamps the decided 429 class through stormQueueErrorRecord
// (the envelope owner) - folding record semantics into a header helper
// would weld each arm's classification to a transport detail.
func (s *Server) retryAfterHeader() http.Header {
	h := http.Header{}
	h.Set("Retry-After", strconv.Itoa(s.cfg().RetryAfterSeconds()))
	return h
}

func writeClientErrorHdr(w http.ResponseWriter, rec *metrics.Record, inbound ir.Protocol, typ, msg string, code int, extra http.Header) {
	if rec != nil {
		if rec.StatusCode == 0 {
			rec.StatusCode = code
		}
		if rec.ErrorType == "" {
			rec.ErrorType = typ
		}
		if rec.ErrorMsg == "" {
			rec.ErrorMsg = msg
		}
	}
	id, model := "", ""
	if rec != nil {
		id, model = rec.ID, rec.Model
	}
	if p := pacerOf(w); p != nil {
		if p.commitError(extra, inbound, code, typ, msg) {
			if err := writeProtocolStreamError(w, rec, inbound, id, model, typ, "", msg); err != nil && rec != nil {
				// The in-band error hit a dead socket: the LOCAL client is
				// gone. Safe for every caller of this helper - the status is
				// the just-stamped error code (≥400), so markClientGone only
				// adds the disconnect flag, never a cursor-200.
				markClientGone(rec)
			}
		}
		return
	}
	writeProtocolHTTPErrorHdr(w, inbound, code, typ, msg, extra)
}

// cursorAgentContextBaseTokens is the fixed agent-context overhead Cursor's
// agent.v1 wrapper adds to every grok prompt (~2.7k tokens measured with a
// 5-token user message). It's folded into the parked-turn input estimate so
// the client's accounting stays close to what the upstream actually bills.
// Internal, measured protocol constant - not user-tunable.
const cursorAgentContextBaseTokens = 2700

// estimateInputTokens approximates the input tokens a cursor turn consumes,
// from the request's message payload alone. It's used ONLY when Cursor's exact
// usage summary hasn't arrived yet (a parked tool-call turn ends before the
// summary lands - the real number arrives on the resume turn's summary). A
// char/4 estimate keeps per-turn billing monotonic with what the client sent,
// so tools that drive context management off reported usage
// don't think every tool turn is free.
func estimateInputTokens(body []byte) int64 {
	var in struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &in) != nil {
		return cursorAgentContextBaseTokens
	}
	var chars int64
	for _, m := range in.Messages {
		if s, ok := flattenContent(m.Content); ok {
			chars += int64(len(s))
			continue
		}
	}
	return int64(len(in.Messages)) + chars/4 + cursorAgentContextBaseTokens
}

// Non-streaming Cursor completions buffer the whole answer before writing one
// JSON body. cursorJSONOutputBytesMax is the internal accumulated-output
// budget: it bounds content, reasoning text when present, tool-call arguments
// and a conservative encoder allowance, so `stream:false` cannot materialize an
// unbounded upstream turn. Sized above normal completions (an 8 MiB answer fits
// with room to spare) and far below a hostile 32 MiB-frame stream. Internal
// safety guardrail, not user-tunable.
const cursorJSONOutputBytesMax = 16 << 20 // 16 MiB

// cursorJSONEnvelopeBytes reserves room for the completion envelope (id,
// object, created, model, choices, usage) and JSON syntax outside the measured
// deltas.
const cursorJSONEnvelopeBytes = 4 << 10

// Per-delta/per-tool-call syntax allowances keep the accounting close to the
// encoder's real output without marshaling every delta twice.
const (
	cursorJSONDeltaOverhead    = 64
	cursorJSONToolCallOverhead = 96
)

// errCursorJSONOutputBytes is the stable fail-closed reason for exceeding the
// non-streaming accumulated-output budget.
var errCursorJSONOutputBytes = errors.New("cursor: non-streaming output byte limit exceeded")

// cursorJSONAccumulator is the single owner of non-streaming Cursor output:
// the content builder, tool-call slice and the byte budget are updated
// together, so a delta that would overflow never appends partial output.
type cursorJSONAccumulator struct {
	content   strings.Builder
	toolCalls []map[string]any
	bytes     int64
	max       int64
}

func newCursorJSONAccumulator() *cursorJSONAccumulator {
	return &cursorJSONAccumulator{bytes: cursorJSONEnvelopeBytes, max: cursorJSONOutputBytesMax}
}

// reset clears a voided turn's accumulation before a re-ask turn reuses it.
func (a *cursorJSONAccumulator) reset() {
	a.content.Reset()
	a.toolCalls = a.toolCalls[:0]
	a.bytes = cursorJSONEnvelopeBytes
}

// add folds one streamed delta into the non-streaming completion. The size is
// checked before either builder/slice is mutated, so an over-budget turn leaves
// the accumulated output unchanged and the driver can return a bounded error.
func (a *cursorJSONAccumulator) add(delta map[string]any) error {
	size := cursorJSONDeltaSize(delta)
	if size > a.max-a.bytes {
		return fmt.Errorf("%w (delta %d bytes, accumulated %d, limit %d)",
			errCursorJSONOutputBytes, size, a.bytes, a.max)
	}
	if c, ok := delta["content"].(string); ok {
		a.content.WriteString(c)
	}
	if tcs, ok := delta["tool_calls"].([]map[string]any); ok {
		a.toolCalls = append(a.toolCalls, tcs...)
	}
	a.bytes += size
	return nil
}

// cursorJSONDeltaSize estimates the encoded bytes a delta contributes. Reasoning
// text is counted even though the Cursor non-stream completion does not emit a
// reasoning body: if a delta ever carries it, it is part of the turn's output
// envelope. cursorJSONStringSize follows encoding/json's HTML-escaping policy, which
// the final Encoder uses.
func cursorJSONDeltaSize(delta map[string]any) int64 {
	n := int64(cursorJSONDeltaOverhead)
	if c, ok := delta["content"].(string); ok {
		n += cursorJSONStringSize(c)
	}
	if r, ok := delta["reasoning_content"].(string); ok {
		n += cursorJSONStringSize(r)
	}
	if tcs, ok := delta["tool_calls"].([]map[string]any); ok {
		for _, tc := range tcs {
			n += cursorJSONToolCallOverhead
			if id, ok := tc["id"].(string); ok {
				n += cursorJSONStringSize(id)
			}
			if fn, ok := tc["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					n += cursorJSONStringSize(name)
				}
				if args, ok := fn["arguments"].(string); ok {
					n += cursorJSONStringSize(args)
				}
			}
		}
	}
	return n
}

// cursorJSONStringSize returns the encoded length of s under encoding/json's default
// HTML escaping: the short escapes (quote, backslash, \b, \f, \n, \r, \t) take
// two bytes, other control bytes and <, >, & take six, invalid UTF-8 becomes
// U+FFFD, and U+2028/U+2029 are escaped. This keeps the budget aligned with the
// bytes json.Encoder writes without materializing a second copy of each string.
func cursorJSONStringSize(s string) int64 {
	var n int64
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"', '\\', '\b', '\f', '\n', '\r', '\t':
				n += 2
			case '<', '>', '&':
				n += 6
			default:
				if c < 0x20 {
					n += 6
				} else {
					n++
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			n += 3 // encoding/json writes U+FFFD for invalid UTF-8
			i++
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			n += 6
		} else {
			n += int64(size)
		}
		i += size
	}
	return n
}
