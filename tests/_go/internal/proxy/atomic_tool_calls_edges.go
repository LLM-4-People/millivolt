package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Edge cases the first round of review demanded and the original suite did not
// cover: the terminator-bounded contrast that actually proves withholding, the
// no-terminator provider shape that atomic withholding would otherwise regress,
// a tool call that begins inside an already-open terminal region, a tool call
// split across data lines, order across interleaved output, and a Responses-API
// feed.

// TestAtomicWithholdingIsBoundedByTheTerminator is the deterministic proof of
// withholding. The same byte sequence is sent twice and differs only in whether
// the terminator arrives: the truncated run must yield ZERO tool-call deltas and
// the complete run exactly one. A mid-flight timing assertion would be flaky;
// this contrast is not, and it pins the property the class depends on.
func TestAtomicWithholdingIsBoundedByTheTerminator(t *testing.T) {
	run := func(t *testing.T, complete bool) (string, int32) {
		t.Helper()
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
			w.(http.Flusher).Flush()
			if complete {
				w.Write([]byte(atomicStopFrame + atomicDoneFrame))
				w.(http.Flusher).Flush()
			}
		}))
		defer upstream.Close()

		cfg := config.Default()
		cfg.AtomicToolCalls = true
		buf := metrics.NewBuffer(100)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()
		_, got := streamRequest(t, srv, upstream)
		return got, calls.Load()
	}

	truncated, truncatedCalls := run(t, false)
	complete, completeCalls := run(t, true)

	if n := toolCallCount(truncated); n != 0 {
		t.Fatalf("truncated run relayed %d tool-call deltas, want 0: withholding is not working", n)
	}
	if n := toolCallCount(complete); n != 1 {
		t.Fatalf("complete run relayed %d tool-call deltas, want exactly 1: the terminator must release it", n)
	}
	// Same bytes, one terminator apart: the ONLY difference in the client's
	// tool-call bytes is the terminator. That is the property.
	if truncatedCalls != 1 || completeCalls != 1 {
		t.Fatalf("upstream calls truncated=%d complete=%d, want 1 each (no rescue configured)", truncatedCalls, completeCalls)
	}
}

// TestNoTerminatorProviderRegressionGuard pins the compatibility cost of atomic
// withholding, which is why it is opt-in. A provider that ends the body with
// neither finish_reason nor [DONE] is a real shape (see metrics.ClassifyOutcome).
// With atomic tool calls OFF - the shipped default - such a provider's completed
// tool call still reaches the client, exactly as before this feature existed.
// With them ON it is deliberately discarded; both behaviors are asserted so the
// trade can never change silently.
func TestNoTerminatorProviderRegressionGuard(t *testing.T) {
	run := func(t *testing.T, atomicOn bool) string {
		t.Helper()
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			// role, text and a COMPLETE tool call, then the body simply ends.
			w.Write([]byte(atomicRoleFrame + atomicTextFrame + atomicToolFrame))
			w.(http.Flusher).Flush()
		}))
		defer upstream.Close()
		cfg := config.Default()
		cfg.AtomicToolCalls = atomicOn
		buf := metrics.NewBuffer(100)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()
		_, got := streamRequest(t, srv, upstream)
		return got
	}

	off := run(t, false)
	if n := toolCallCount(off); n != 1 {
		t.Fatalf("default (atomic off) must keep relaying the tool call for a no-terminator provider, saw %d: %s", n, off)
	}
	if !strings.Contains(off, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must still surface in-band: %s", off)
	}

	on := run(t, true)
	if n := toolCallCount(on); n != 0 {
		t.Fatalf("atomic on must withhold and discard it for a no-terminator provider, saw %d: %s", n, on)
	}
	if !strings.Contains(on, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface either way: %s", on)
	}
}

// TestToolCallAfterTerminatorIsRelayedVerbatim pins the one shape withholding
// does NOT cover, so the guarantee is stated honestly rather than implied. A
// provider that emits a terminator and THEN a tool call has already ended the
// stream: the terminal region was released on that line, so the later tool-call
// line is relayed verbatim. Withholding cannot cover it - there is no
// terminator left to release a second region at, and the bytes have already
// been ordered behind the terminator. Such a stream is malformed regardless
// (a tool call after the end), and the append class never applies to it because
// the terminator is sticky. The provider frames its own disagreement, so this
// is recorded rather than papered over.
func TestToolCallAfterTerminatorIsRelayedVerbatim(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame))
		w.(http.Flusher).Flush()
		// The terminator arrives first and closes the stream's region.
		w.Write([]byte(atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
		// ...and a tool call follows it, out of contract.
		w.Write([]byte(atomicToolFrame))
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
		t.Fatalf("upstream calls = %d, want 1 (a terminator was seen, so this is not a truncation)", n)
	}
	if n := toolCallCount(got); n != 1 {
		t.Fatalf("the post-terminator tool call is relayed verbatim, saw %d deltas: %s", n, got)
	}
	// Order must still be preserved: the tool call is where it arrived.
	if i, j := strings.Index(got, atomicDoneFrame), strings.Index(got, atomicToolFrame); i < 0 || j < i {
		t.Fatalf("relay order broken: done=%d tool=%d\n%s", i, j, got)
	}
}

// TestToolCallSplitAcrossDataLinesIsWithheld covers the multi-line SSE event
// shape: the tool_calls key appears on the first data line, so the withhold
// region must open there - not one line later, after argument bytes are out.
func TestToolCallSplitAcrossDataLinesIsWithheld(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame))
		w.(http.Flusher).Flush()
		// One event, three data lines, no blank line until the end.
		w.Write([]byte(`data: {"id":"A","choices":[{"delta":{"tool_calls":[` + "\n" +
			`data: {"index":0,"id":"call_1","function":{"name":"bash",` + "\n" +
			`data: "arguments":"{}"}}]}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		// Truncated: no terminator.
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := toolCallCount(got); n != 0 {
		t.Fatalf("a split tool call must be withheld whole, saw %d deltas: %s", n, got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the truncation must surface in-band: %s", got)
	}
}

// TestAtomicOrderPreservedAcrossInterleavedOutput pins byte order when text, a
// tool call and more text share a stream: the withheld region must be released
// in arrival order, not appended at the end.
func TestAtomicOrderPreservedAcrossInterleavedOutput(t *testing.T) {
	lateText := `data: {"id":"A","choices":[{"delta":{"content":" tail"}}]}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(atomicRoleFrame + atomicTextFrame))
		w.(http.Flusher).Flush()
		w.Write([]byte(atomicToolFrame))
		w.(http.Flusher).Flush()
		w.Write([]byte(lateText))
		w.(http.Flusher).Flush()
		w.Write([]byte(atomicStopFrame + atomicDoneFrame))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AtomicToolCalls = true
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	iText := strings.Index(got, atomicTextFrame)
	iTool := strings.Index(got, atomicToolFrame)
	iLate := strings.Index(got, lateText)
	iStop := strings.Index(got, atomicStopFrame)
	if iText < 0 || iTool < 0 || iLate < 0 || iStop < 0 {
		t.Fatalf("every frame must be relayed: %s", got)
	}
	if !(iText < iTool && iTool < iLate && iLate < iStop) {
		t.Fatalf("relay order broken: text=%d tool=%d late=%d stop=%d\n%s", iText, iTool, iLate, iStop, got)
	}
	// Framing must survive too: every event a parser sees is one JSON value.
	for i, ev := range sseEvents(got) {
		if ev == "[DONE]" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(ev), &v); err != nil {
			t.Fatalf("SSE event %d is not one JSON value (%v): %q", i, err, ev)
		}
	}
}

// TestAppendRetriesNeverRescuesResponsesFeed pins the Responses-API exclusion
// end to end: its per-response sequence numbers are documented ordering state,
// so a re-send must never restart them mid-stream.
func TestAppendRetriesNeverRescuesResponsesFeed(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(`event: response.created` + "\n" +
			`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
			`event: response.output_text.delta` + "\n" +
			`data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"))
		w.(http.Flusher).Flush()
		// No response.completed: the feed truncated.
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
		t.Fatalf("upstream calls = %d, want 1 (a Responses feed is never append-rescued)", n)
	}
	if !strings.Contains(got, "partial") {
		t.Fatalf("the feed's own deltas must relay verbatim: %s", got)
	}
}
