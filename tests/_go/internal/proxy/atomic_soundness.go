package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The soundness holes the seventh review measured. Every one of them is a path
// where the relay's view of the stream diverges from the bytes the client
// actually received - a tool call on the wire that no terminator committed, a
// rescue landing behind it, a degenerate class erased by a payload's size, or
// the DEFAULTS path quietly losing a stream's last line.

// toolCallKeyPad sizes the JSON string between a data line's `data: ` prefix
// and its `tool_calls` key so the key lands past the relay's 1 MiB newline-free
// guardrail (sseScannerBufMax). A line that long is delivered in FRAGMENTS, and
// every fragment after the first begins mid-JSON with no `data:` prefix - so an
// analyzer that only reads whole data lines can never see the call. That is the
// ordinary reach, not a synthetic one: a thinking model coalescing a long
// reasoning_content and the call into one chunk, a logprobs blob, or a gateway
// that hoists a long prompt_filter_results before choices all produce it.
const toolCallKeyPad = 2 * sseScannerBufMax

// toolCallKeyPastGuardrail is one data line whose `tool_calls` key sits
// toolCallKeyPad bytes in, carrying a complete, valid tool call, and which the
// upstream never terminates: the body ends inside that line. The final fragment
// is therefore a trailing partial line too, so this one fixture exercises both
// newline-free paths (the guardrail split and the trailing partial).
func toolCallKeyPastGuardrail() string {
	const head = `data: {"id":"A","choices":[{"delta":{"pad":"`
	const call = `","tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}`
	return head + strings.Repeat("p", toolCallKeyPad) + call
}

// TestGuardrailSplitToolCallIsNeverResent pins the newline-free path against the
// analyzer's blind spot. The guardrail splits the line, so only the first
// fragment carries a `data:` prefix and only the first fragment reaches the
// analyzer's line scan: a call whose key is 2 MiB in is invisible to it. The
// relay used to relay every fragment and then believe the client held nothing
// contract-shaped, so all three rescue budgets fired and a fresh attempt
// appended behind the bytes the client already had. Measured on the shipped
// setting: upstream attempts 2, TWO byte-identical tool-call frames both at
// index 0, and a record with Retries=1 and ToolCalls=0.
//
// The invariant is the analyzer's view must never be wider than the wire: a
// tool-call shape has to be detected in ANY byte run, and once tool-call bytes
// are on the wire for whatever reason (the setting is off, or this line's head
// went out before the call was revealed) that fact is recorded, because a
// re-send behind them merges both attempts' arguments for the same index - the
// one hazard the tool-call contract cannot survive.
func TestGuardrailSplitToolCallIsNeverResent(t *testing.T) {
	wire := atomicRoleFrame + toolCallKeyPastGuardrail()
	if len(wire) <= toolCallKeyPad {
		t.Fatalf("the fixture must put the key past the guardrail, got %d bytes", len(wire))
	}
	for _, c := range []struct {
		name        string
		atomicOn    bool
		wantOnWire  int
		wantMessage string
	}{
		// Withheld: the call never reaches the client, so no re-send is legal
		// and none happens.
		{name: "atomic tool calls on", atomicOn: true, wantOnWire: 0, wantMessage: "a withheld call must never be relayed"},
		// Shipped default: the call is relayed (that is what the setting trades
		// away), so the re-send behind it is the thing that must not happen.
		{name: "shipped default", atomicOn: false, wantOnWire: 1, wantMessage: "the default keeps relaying the call"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, calls, rec := streamWithAtomic(t, wire, c.atomicOn)
			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1: a re-send must never land behind tool-call bytes (%s)", calls, c.wantMessage)
			}
			if n := toolCallCount(got); n != c.wantOnWire {
				t.Fatalf("client saw %d tool-call deltas, want %d: %s", n, c.wantOnWire, c.wantMessage)
			}
			// The same-index assertion: a second attempt's frames would carry a
			// second `"index":0` entry, which is exactly how both attempts'
			// arguments merge into one call.
			if n := strings.Count(got, `"index":0`); n != c.wantOnWire {
				t.Fatalf("client saw %d index-0 tool-call entries, want %d: a re-send duplicated the call: %s",
					n, c.wantOnWire, truncateForMsg(got))
			}
			if rec.Retries != 0 {
				t.Fatalf("record Retries = %d, want 0: no attempt may be absorbed behind those bytes", rec.Retries)
			}
			if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record must carry the truncation, not be a silent success: IsError=%v ErrorCode=%q", rec.IsError(), rec.ErrorCode)
			}
			if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
				t.Fatalf("the truncation must surface in-band: %s", truncateForMsg(got))
			}
			// The record counts what the provider sent, so the call the
			// withheld region revealed is still accounted for.
			if rec.ToolCalls != 1 {
				t.Fatalf("record ToolCalls = %d, want 1: the split call is still a call the provider made", rec.ToolCalls)
			}
			if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "bash" {
				t.Fatalf("record ToolNames = %v, want [bash]", rec.ToolNames)
			}
		})
	}
}

// TestWithheldToolCallSurvivesATransportReadError pins the read-error epilogue
// against an uncommitted call. An upstream that TCP-RESETs mid-stream used to
// release the withheld region verbatim: the client received the tool call, no
// [DONE] and no in-band error, and the record said stream_read_error with
// ToolCalls=1 - flagged, but the client was still holding a call no terminator
// committed. A withheld region is withheld because the relay cannot prove the
// call finished, and a transport death proves nothing at all: the region is
// dropped and the failure is surfaced in its place.
func TestWithheldToolCallSurvivesATransportReadError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicToolFrame))
		w.(http.Flusher).Flush()
		// Abort the connection with a RST: the client sees a transport read
		// error, not a clean EOF, and no terminator ever arrives.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.SetLinger(0)
		}
		conn.Close()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	buf := metrics.NewBuffer(50)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("client saw %d tool-call deltas, want 0: a transport death must never release an uncommitted call: %s",
			n, truncateForMsg(got))
	}
	if !strings.Contains(got, `"type":"`+errStreamRead+`"`) {
		t.Fatalf("the transport failure must surface in-band where the region was dropped: %s", truncateForMsg(got))
	}
	// The substituted failure closes the stream exactly once, so the client's
	// SSE parser ends on a real terminator instead of a truncated body.
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %s", n, truncateForMsg(got))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream attempts = %d, want 1", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ErrorType != errStreamRead {
		t.Fatalf("record ErrorType = %q, want %q", rec.ErrorType, errStreamRead)
	}
	if !rec.IsError() {
		t.Fatalf("record must flag the dead stream: %+v", rec)
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1: the provider did call a tool", rec.ToolCalls)
	}
}

// stagedWireReader hands streamBody its bytes one stage per read and then a
// terminal error, so the two states a client-gone path can be observed in - a
// canceled request context and a transport read failure - are driven
// deterministically instead of through a timing race.
type stagedWireReader struct {
	stages []string
	err    error
}

func (r *stagedWireReader) Read(p []byte) (int, error) {
	for len(r.stages) > 0 && r.stages[0] == "" {
		r.stages = r.stages[1:]
	}
	if len(r.stages) == 0 {
		if r.err != nil {
			err := r.err
			r.err = nil
			return 0, err
		}
		return 0, io.EOF
	}
	n := copy(p, r.stages[0])
	r.stages[0] = r.stages[0][n:]
	return n, nil
}

// TestClientGoneWithAWithheldRegionReleasesNothing pins both halves of the
// client-gone epilogue. A withhold region open when the client disappeared used
// to be RELEASED verbatim - verified live against scripts/dev.sh with a SIGTERM:
// the client received role, content, a keepalive and then the withheld
// tool-call frame, with no [DONE] and no in-band error. The region is withheld
// precisely because the relay cannot prove the call finished; a socket that went
// away proves nothing, so nothing may be released into it.
//
// The second half is the record. That same live run recorded status 499 with an
// EMPTY error type: 499 means the local client closed the connection, and
// IsError() is false for a bare 499, so the dashboard showed a non-error
// request that made a tool call. A close the SERVER initiated is not the
// client's own abort and has to carry a real class; the client's own abort keeps
// the documented 499 flow event.
func TestClientGoneWithAWithheldRegionReleasesNothing(t *testing.T) {
	wire := atomicRoleFrame + atomicTextFrame + atomicToolFrame
	for _, c := range []struct {
		name          string
		serverStopped bool
		wantType      string
		wantStatus    int
		wantGone      bool
	}{
		{
			name:          "the server stopped serving",
			serverStopped: true,
			wantType:      errServerShutdown,
			wantStatus:    http.StatusServiceUnavailable,
		},
		{
			name:       "the local client left",
			wantStatus: metrics.StatusClientClosedRequest,
			wantGone:   true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.AtomicToolCalls = true
			s := New(cfg, nil)
			if c.serverStopped {
				s.MarkAborting()
			}
			// The process is aborting in-flight streams: the request context
			// is canceled and the body read fails with it. Nothing here races
			// a real socket - the client-gone classification is the fact under
			// test, not its detection.
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			body := &stagedWireReader{stages: []string{wire}, err: context.Canceled}
			rec := &metrics.Record{StatusCode: 200, Stream: true, Provider: "openai"}
			w := httptest.NewRecorder()
			rescue := s.streamBody(ctx, w, body, rec, true, true, true, &upstreamErrorRescue{}, true)
			if rescue != rescueNone {
				t.Fatalf("rescue = %d, want none: a client that is gone cannot be rescued", rescue)
			}
			if n := toolCallCount(w.Body.String()); n != 0 {
				t.Fatalf("client saw %d tool-call deltas, want 0: a client-gone path must release nothing: %s",
					n, truncateForMsg(w.Body.String()))
			}
			if rec.ErrorType != c.wantType {
				t.Fatalf("record ErrorType = %q, want %q", rec.ErrorType, c.wantType)
			}
			if rec.StatusCode != c.wantStatus {
				t.Fatalf("record StatusCode = %d, want %d", rec.StatusCode, c.wantStatus)
			}
			if rec.ClientDisconnected != c.wantGone {
				t.Fatalf("record ClientDisconnected = %v, want %v: only the local client's own abort is a disconnect",
					rec.ClientDisconnected, c.wantGone)
			}
			if wantError := c.wantType != ""; rec.IsError() != wantError {
				t.Fatalf("record IsError = %v, want %v (type %q)", rec.IsError(), wantError, rec.ErrorType)
			}
			// The already-relayed answer content is unaffected: withholding is
			// about the call, not about erasing the stream.
			if !strings.Contains(w.Body.String(), atomicTextFrame) {
				t.Fatalf("already-relayed content must still reach the client: %s", truncateForMsg(w.Body.String()))
			}
		})
	}
}

// TestCommittedRegionWithoutAToolCallKeepsItsClass pins the outcome rule where a
// payload's SIZE alone used to reach a silent success. The commit-to-verbatim
// rule exists because a region that overflowed its bound can carry tool-call
// bytes the analyzer never saw a whole call in, which makes every count-based
// class untrustworthy. A region that holds NO tool call at all has no such
// problem: what the analyzer says about it is exactly what the client received.
// Measured before the fix - atomic tool calls on, a region opened by a
// finish_reason chunk, no [DONE], and a 65,536-byte usage payload: the payload
// alone pushed the region past terminalHoldMax, the commit cleared the class,
// and a stream that produced literally nothing recorded IsError=false with an
// empty ErrorCode.
func TestCommittedRegionWithoutAToolCallKeepsItsClass(t *testing.T) {
	// The usage payload is newline-free, so it is the accumulating partial that
	// crosses the 64 KiB terminal bound - a committed region, verbatim.
	tail := `data: {"id":"A","choices":[],"usage":{"prompt_tokens":1,"prompt":"` + strings.Repeat("u", 2*terminalHoldMax)
	got, calls, rec := atomicStream(t, atomicRoleFrame+atomicStopOnlyFrame+tail)

	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", calls)
	}
	if !rec.IsError() {
		t.Fatalf("a stream that produced nothing must never record as a clean success: %+v", rec)
	}
	if rec.ErrorCode != metrics.CodeEmptyCompletion {
		t.Fatalf("record ErrorCode = %q, want %q: a committed region that holds no tool call cannot erase the class",
			rec.ErrorCode, metrics.CodeEmptyCompletion)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("the class must reach the client too, appended after the committed bytes: %s", truncateForMsg(got))
	}
	if rec.ToolCalls != 0 {
		t.Fatalf("record ToolCalls = %d, want 0: this stream has no tool call", rec.ToolCalls)
	}
}

// The DEFAULTS path. Neither of the two below involves a tool call, a setting
// or a rescue budget: they are the shipped behavior a1324c8 changed.

// TestDefaultsReleaseATrailingTerminalPartial pins the trailing-partial line at
// the shipped default. A stream whose last line lacks its trailing newline used
// to be staged verbatim and written by the terminal resolution; the withhold
// opener added to that path withheld it as a terminal region, and since the
// class was not reasoning_only the held bytes were never written at all - the
// client lost the stream's final line. Measured over the 52,112-row
// differential corpus: 19 cases / 132 rows, all of them a \r-only wire or a
// wire with no blank line at EOF.
//
// A trailing partial line is released EXACTLY as before. Withholding on that
// path is a tool-call decision and nothing else: a partial line can never be a
// terminal marker, so it can only ever OPEN a region, and the only opening
// worth having is a tool call's.
func TestDefaultsReleaseATrailingTerminalPartial(t *testing.T) {
	finish := `data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}`
	for _, c := range []struct {
		name string
		wire string
	}{
		{name: "last line has no trailing newline", wire: atomicRoleFrame + finish},
		{
			// A \r-only wire has no \n at all, so the WHOLE document is one
			// trailing partial line. Withheld, it was dropped and the relay
			// re-sent the same empty result.
			name: "carriage-return-only framing",
			wire: `data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\r" + finish,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, calls, rec := streamWithAtomic(t, c.wire, false)
			if calls != 1 {
				t.Fatalf("upstream attempts = %d, want 1", calls)
			}
			// Byte preservation: every byte the upstream sent, in order, before
			// anything the relay adds.
			if !strings.HasPrefix(got, c.wire) {
				t.Fatalf("the upstream's own bytes must be released verbatim and in order.\nwire: %q\ngot:  %q",
					truncateForMsg(c.wire), truncateForMsg(got))
			}
			if !strings.Contains(got, `"code":"`+metrics.CodeEmptyCompletion+`"`) {
				t.Fatalf("the void class must still surface in-band: %s", truncateForMsg(got))
			}
			if strings.Count(got, "data: [DONE]") != 1 {
				t.Fatalf("exactly one [DONE] expected: %s", truncateForMsg(got))
			}
			if !rec.IsError() || rec.ErrorCode != metrics.CodeEmptyCompletion {
				t.Fatalf("record must carry %s, got IsError=%v code=%q", metrics.CodeEmptyCompletion, rec.IsError(), rec.ErrorCode)
			}
		})
	}
}

// TestDefaultsKeepReleasingANonTerminalTrailingPartial is the contrast that
// keeps the test above honest: a trailing partial that is NOT terminal, at the
// default, is also released verbatim - the whole trailing-partial path is byte
// preservation again, not a special case for terminal lines.
func TestDefaultsKeepReleasingANonTerminalTrailingPartial(t *testing.T) {
	tail := `data: {"id":"A","choices":[{"delta":{"content":"tail"}}]}`
	got, calls, rec := streamWithAtomic(t, atomicRoleFrame+tail, false)
	if calls != 1 {
		t.Fatalf("upstream attempts = %d, want 1", calls)
	}
	if !strings.HasPrefix(got, atomicRoleFrame+tail) {
		t.Fatalf("the trailing partial must be released verbatim: %s", truncateForMsg(got))
	}
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("a body that ends mid-event is a truncation: IsError=%v code=%q", rec.IsError(), rec.ErrorCode)
	}
}

// TestTrailingPartialToolCallInsideAnOpenRegionIsNotReleased pins the same rule
// one step later than the test above: the tool call does not have to OPEN the
// withheld region to be in it. A finish_reason chunk opens a terminal region
// without resolving it (sse.TerminalLine matches only [DONE] and the Responses
// terminal events), and a tool call that then arrives in the trailing partial
// lands inside that region. Such a stream is out of contract either way, but a
// transport death means the terminator never came, so the call in the region was
// never committed and must not be released - the same fact, recorded the same
// way, whichever order the two arrived in.
func TestTrailingPartialToolCallInsideAnOpenRegionIsNotReleased(t *testing.T) {
	// The finish chunk completes, so the region opens; the body then ends inside
	// a tool call's data line and the transport dies.
	body := &stagedWireReader{
		stages: []string{atomicRoleFrame + atomicStopOnlyFrame + `data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}`},
		err:    errors.New("read tcp: connection reset by peer"),
	}
	cfg := config.Default()
	cfg.AtomicToolCalls = true
	s := New(cfg, nil)
	rec := &metrics.Record{StatusCode: 200, Stream: true, Provider: "openai"}
	w := httptest.NewRecorder()
	rescue := s.streamBody(context.Background(), w, body, rec, true, true, true, &upstreamErrorRescue{}, true)
	if rescue != rescueNone {
		t.Fatalf("rescue = %d, want none: a committed region is never rescued", rescue)
	}
	if n := toolCallCount(w.Body.String()); n != 0 {
		t.Fatalf("client saw %d tool-call deltas, want 0: the transport failure must not release the region: %s",
			n, truncateForMsg(w.Body.String()))
	}
	if !strings.Contains(w.Body.String(), `"type":"`+errStreamRead+`"`) {
		t.Fatalf("the transport failure must surface in the region's place: %s", truncateForMsg(w.Body.String()))
	}
	if rec.ErrorType != errStreamRead {
		t.Fatalf("record ErrorType = %q, want %q", rec.ErrorType, errStreamRead)
	}
	if rec.ToolCalls != 1 {
		t.Fatalf("record ToolCalls = %d, want 1: the provider did call a tool", rec.ToolCalls)
	}
}
