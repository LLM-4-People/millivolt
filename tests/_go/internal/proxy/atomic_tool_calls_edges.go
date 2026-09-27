package proxy

import (
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
// the withhold region a large tool call opens, and a Responses-API feed whose
// per-response sequence numbers a re-send must never restart.

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

// TestRescueNeverRestartsResponsesFeed pins the Responses-API exclusion where
// it is actually decided. Those events carry a per-response monotonic
// sequence_number used to order the stream, so appending a re-send's restarted
// sequence mid-stream would break documented ordering; the thinking budget -
// the one budget that DOES append behind relayed bytes - is therefore gated on
// !responsesStream. The contrast is what makes it non-vacuous: the same
// reasoning-only, unterminated, no-answer-content stream is re-sent on the
// chat-completions shape and must NOT be re-sent once it is a Responses feed.
// An assertion about the Responses run alone would pass for the wrong reason
// (a missing budget, or an answer delta the exclusion never sees), so the
// chat-completions run is asserted in the same test.
func TestRescueNeverRestartsResponsesFeed(t *testing.T) {
	run := func(t *testing.T, wire string) (string, int32) {
		t.Helper()
		var calls atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte(wire))
			w.(http.Flusher).Flush()
			// No terminal event: the feed truncated.
		}))
		defer upstream.Close()
		cfg := config.Default()
		// Both budgets available, so a refusal must be a deliberate gate and
		// not an absent budget.
		cfg.ThinkingRetries = 1
		cfg.QualityRetries = 1
		buf := metrics.NewBuffer(100)
		srv := httptest.NewServer(New(cfg, buf))
		defer srv.Close()
		_, got := streamRequest(t, srv, upstream)
		return got, calls.Load()
	}

	chat := `data: {"id":"1","choices":[{"delta":{"reasoning_content":"weighing"}}]}` + "\n\n"
	got, chatCalls := run(t, chat)
	if chatCalls != 2 {
		t.Fatalf("chat-completions reasoning-only truncation: upstream calls = %d, want 2 (the thinking budget must rescue it, or the contrast below proves nothing)", chatCalls)
	}
	if !strings.Contains(got, "weighing") {
		t.Fatalf("the relayed reasoning must reach the client: %s", got)
	}

	responses := `event: response.created` + "\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		`event: response.reasoning_text.delta` + "\n" +
		`data: {"type":"response.reasoning_text.delta","delta":"weighing"}` + "\n\n"
	_, responsesCalls := run(t, responses)
	if responsesCalls != 1 {
		t.Fatalf("upstream calls = %d, want 1: a Responses feed's per-response sequence numbers are documented ordering state and a re-send must never restart them mid-stream", responsesCalls)
	}
}
