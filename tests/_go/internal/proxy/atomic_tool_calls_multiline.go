package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Regression cases for the two detection holes found in the third review. Both
// are multi-line SSE shapes that a per-line scan handles wrongly, and both were
// reproduced with the exact wire the review used.

// TestToolCallValueOnNextDataLineIsWithheld covers the soundness hole: a
// "tool_calls" key whose value begins on the NEXT data line. JSONKey returns
// nil for it (indistinguishable from "no such key") and the line carrying the
// array body has no key in it, so before the fix the region never opened and
// the UNCOMMITTED tool call reached the client - and the append rescue then
// appended a second call with the same index and id, which is exactly the
// merge/spurious-execution hazard the mechanism exists to prevent.
func TestToolCallValueOnNextDataLineIsWithheld(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.WriteHeader(200)
			// One event, two data lines: the key ends line 1, the array is line 2.
			w.Write([]byte(atomicRoleFrame + atomicTextFrame +
				`data: {"id":"A","choices":[{"delta":{"tool_calls":` + "\n" +
				`data: [{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]}}]}` + "\n\n"))
			w.(http.Flusher).Flush()
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame + atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	// The load-bearing assertion: the split, uncommitted call was never written,
	// so the client cannot end up with two calls at index 0 with the same id.
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("client saw %d tool-call deltas, want 1 (the uncommitted split call must never be relayed): %s", n, got)
	}
	if strings.Count(got, `"id":"call_1"`) != 1 {
		t.Fatalf("the tool-call id must appear exactly once, not merged across attempts: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (rescued)", n)
	}
}

// TestEmptyToolCallArraySplitAcrossLinesLosesNoBytes is the other direction: an
// EMPTY tool_calls array pretty-printed across two data lines opens the
// withhold region ambiguously, but no tool call exists. A truncation must not
// throw away the ordinary answer text behind it - the withheld bytes are given
// back verbatim, and no rescue is spent.
func TestEmptyToolCallArraySplitAcrossLinesLosesNoBytes(t *testing.T) {
	tail := `data: {"id":"A","choices":[{"delta":{"content":"important tail"}}]}` + "\n\n"
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame))
		w.(http.Flusher).Flush()
		// An empty array split across two data lines: no tool call anywhere.
		w.Write([]byte(`data: {"id":"A","choices":[{"delta":{"tool_calls":[` + "\n" +
			`data: ]}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		// Real answer text after it, which an ambiguous region must not eat.
		w.Write([]byte(tail))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, "important tail") {
		t.Fatalf("an ambiguous withhold region must not discard answer text: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no tool call, so no rescue is warranted)", n)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must still surface in-band: %s", got)
	}
}

// TestToolCallInsideOversizedHoldIsNotRescued kills the mutation where the
// hold-bound overflow stops recording toolBytesOnWire. Many complete tool-call
// delta lines totalling more than toolCallHoldMax overflow the region into a
// verbatim commit, which puts tool-call bytes on the wire; the append rescue
// must then refuse, or a re-send merges behind already-relayed arguments.
func TestToolCallInsideOversizedHoldIsNotRescued(t *testing.T) {
	// Enough complete tool-call delta lines to blow the hold bound.
	var b strings.Builder
	b.WriteString(atomicRoleFrame)
	for i := 0; b.Len() < toolCallHoldMax+4096; i++ {
		b.WriteString(`data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"` +
			strings.Repeat("z", 512) + `"}}]}}]}` + "\n\n")
	}
	oversized := b.String()

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(oversized))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1: the overflow put tool-call bytes on the wire, so no re-send may follow", n)
	}
	// The fixture's deltas carry no id or function name, so count the deltas
	// themselves rather than the named-function marker.
	if n := strings.Count(got, `"tool_calls":[{"index"`); n == 0 {
		t.Fatalf("the overflow must commit the region verbatim: %d bytes of wire", len(got))
	}
}

// TestReadErrorAppendRescue covers the read-error twin of the append class,
// which no test executed: a connection dropped after answer content is the
// same rescuable truncation as a clean EOF. It also exercises the guards the
// review found untested - the per-send deadline and a canceled context must
// both refuse the re-send.
func TestReadErrorAppendRescue(t *testing.T) {
	newUpstream := func(calls *atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if calls.Add(1) == 1 {
				w.Write([]byte(atomicRoleFrame))
				w.(http.Flusher).Flush()
				w.Write([]byte(atomicTextFrame))
				w.(http.Flusher).Flush()
				// Drop the connection: the proxy sees a read error, not EOF.
				panic("simulated upstream crash after content")
			}
			w.Write([]byte(atomicRoleFrame))
			w.(http.Flusher).Flush()
			w.Write([]byte(atomicTextFrame))
			w.(http.Flusher).Flush()
			w.Write([]byte(atomicStopFrame))
			w.(http.Flusher).Flush()
			w.Write([]byte(atomicDoneFrame))
			w.(http.Flusher).Flush()
		}))
	}

	var calls atomic.Int32
	upstream := newUpstream(&calls)
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	recs := waitForRecord(t, buf, 1)
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (a read error after content is the same rescuable truncation)", n)
	}
	if !strings.Contains(got, atomicStopFrame) {
		t.Fatalf("the recovered stream's terminator must reach the client: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the reset must be absorbed by the rescue, not surfaced: %s", got)
	}
	if recs[0].IsError() {
		t.Fatalf("a rescued request must not be an error: %+v", recs[0])
	}
	if recs[0].Retries != 1 || len(recs[0].Attempts) != 1 ||
		recs[0].Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("the absorbed reset must be logged: retries=%d attempts=%+v", recs[0].Retries, recs[0].Attempts)
	}
}

// stagedReader yields one chunk, runs a hook, then ends with a fixed error
// (io.EOF for a clean finish). It reproduces the state relay.go documents as
// reachable: the upstream's FIN beats the transport's own cancellation, so the
// read loop sees a CLEAN EOF (or a reset) for an abort. Both append-rescue
// sites must refuse it, because a re-send would mint a fresh billed send for a
// client that is already gone.
type stagedReader struct {
	data []byte
	hook func()
	err  error
	done bool
}

func (r *stagedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	if r.hook != nil {
		r.hook()
	}
	return copy(p, r.data), nil
}

// TestAppendRescueRefusesCanceledCaller covers both append-rescue sites with a
// caller that is already gone. The clean-EOF site and the read-error site are
// checked separately because they carry independent guards, and both must
// refuse: allowAppendRetry is deliberately true, so only the own-context guard
// can stop the re-send.
func TestAppendRescueRefusesCanceledCaller(t *testing.T) {
	frame := atomicRoleFrame + atomicTextFrame
	cases := []struct {
		name string
		err  error
	}{
		{"clean EOF after cancel", io.EOF},
		{"read error after cancel", errors.New("simulated connection reset")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			rec := &metrics.Record{}
			s := New(config.Default(), metrics.NewBuffer(10))
			body := &stagedReader{data: []byte(frame), hook: cancel, err: c.err}
			got := s.streamBody(ctx, httptest.NewRecorder(), body, rec, false, false, true, true, &upstreamErrorRescue{})
			if got != rescueNone {
				t.Fatalf("rescue = %v, want rescueNone: a re-send must never be minted for a canceled caller", got)
			}
		})
	}
}
