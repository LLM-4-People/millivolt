package proxy

// Regression: clients send cumulative history, so a result-request
// carries every tool_call id ever seen. The parked-run lookup must match the
// TRAILING run of ids (the current turn's answers) and ignore consumed older
// ids - the all-ids exact match treated every such request as a miss and the
// proxy cold-started a fresh Run on 100% of resume attempts.

import (
	"io"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func fakeParkedRun(t *testing.T) *format.CursorRun {
	t.Helper()
	return format.NewCursorRun(io.Discard, emptyReader{}, nil, func() {}, 5*time.Second)
}

// emptyReader is an io.Reader that immediately returns EOF (the run's pump is
// never started in these tests).
type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

func parkWithIDs(t *testing.T, s *cursorRunStore, run *format.CursorRun, ids ...string) {
	t.Helper()
	if !s.parkWith(cursorScope{}, run, ids) {
		t.Fatal("park failed")
	}
}

func TestFindByToolTailCumulativeHistory(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A", "call-B", "call-C", "call-D")
	defer run.Close()

	// Requests carry consumed ids from prior turns before the current answers.
	got, tail := s.findByToolTail(cursorScope{}, []string{"call-old1", "call-old2", "call-A"})
	if got != run {
		t.Fatalf("expected parked run, got %v", got)
	}
	if len(tail) != 1 || tail[0] != "call-A" {
		t.Fatalf("tail = %v, want [call-A]", tail)
	}
}

func TestFindByToolTailFullTail(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A", "call-B")
	defer run.Close()

	got, tail := s.findByToolTail(cursorScope{}, []string{"call-old", "call-A", "call-B"})
	if got != run {
		t.Fatalf("expected parked run, got %v", got)
	}
	if len(tail) != 2 || tail[0] != "call-A" || tail[1] != "call-B" {
		t.Fatalf("tail = %v, want [call-A call-B]", tail)
	}
}

func TestFindByToolTailSingleResult(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A")
	defer run.Close()

	got, tail := s.findByToolTail(cursorScope{}, []string{"call-A"})
	if got != run || len(tail) != 1 || tail[0] != "call-A" {
		t.Fatalf("got %v tail %v", got, tail)
	}
}

func TestFindByToolTailUnparkedTrailingIDMisses(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A")
	defer run.Close()

	// A trailing id no parked run owns is NOT the current answer - miss.
	if got, tail := s.findByToolTail(cursorScope{}, []string{"call-A", "call-gone"}); got != nil || tail != nil {
		t.Fatalf("expected miss, got %v %v", got, tail)
	}
}

func TestFindByToolTailStopsAtRunBoundary(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	runA := fakeParkedRun(t)
	runB := fakeParkedRun(t)
	parkWithIDs(t, s, runA, "call-A")
	parkWithIDs(t, s, runB, "call-B")
	defer runA.Close()
	defer runB.Close()

	// History spanning two parked runs: resume only the trailing run's answers.
	got, tail := s.findByToolTail(cursorScope{}, []string{"call-A", "call-B"})
	if got != runB {
		t.Fatalf("expected runB, got %v", got)
	}
	if len(tail) != 1 || tail[0] != "call-B" {
		t.Fatalf("tail = %v, want [call-B]", tail)
	}
}

func TestFindByToolTailRemovesEntry(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A", "call-B")
	defer run.Close()

	if _, tail := s.findByToolTail(cursorScope{}, []string{"call-A"}); len(tail) == 0 {
		t.Fatal("expected a match")
	}
	s.mu.Lock()
	byCall, entries := len(s.byCall), len(s.entries)
	s.mu.Unlock()
	if byCall != 0 || entries != 0 {
		t.Fatalf("entry not removed: byCall=%v entries=%v", byCall, entries)
	}
	// A second request with the same ids must now miss (run already resumed).
	if got, _ := s.findByToolTail(cursorScope{}, []string{"call-A"}); got != nil {
		t.Fatal("resumed run should no longer be parked")
	}
}

func TestFindByToolTailNoIDs(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	if got, tail := s.findByToolTail(cursorScope{}, nil); got != nil || tail != nil {
		t.Fatalf("expected nil, got %v %v", got, tail)
	}
}

func TestParkTTLExpiry(t *testing.T) {
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A")
	defer run.Close()

	// Age the entry past the TTL; the next lookup must sweep it (close + miss).
	s.mu.Lock()
	for _, e := range s.entries {
		e.at = time.Now().Add(-2 * time.Minute)
	}
	s.mu.Unlock()
	if got, _ := s.findByToolTail(cursorScope{}, []string{"call-A"}); got != nil {
		t.Fatal("expired parked run should be swept and missed")
	}
}

func TestParkTTLUpdate(t *testing.T) {
	// Reload widens the TTL: an entry past the OLD ttl but within the new one
	// must survive.
	s := newCursorRunStore(time.Minute)
	run := fakeParkedRun(t)
	parkWithIDs(t, s, run, "call-A")
	defer run.Close()

	s.UpdateTTL(time.Hour)
	s.mu.Lock()
	for _, e := range s.entries {
		e.at = time.Now().Add(-2 * time.Minute)
	}
	s.mu.Unlock()
	if got, _ := s.findByToolTail(cursorScope{}, []string{"call-A"}); got != run {
		t.Fatal("updated TTL should keep the run parked")
	}

	// And a reload that NARROWS the TTL sweeps it on the next lookup.
	s.UpdateTTL(time.Second)
	if got, _ := s.findByToolTail(cursorScope{}, []string{"call-A"}); got != nil {
		t.Fatal("narrowed TTL should expire the aged run")
	}
}

// TestCursorRunPumpExitMarksClosed pins the zombie-run fix: when the upstream
// stream ends while a run is parked, the pump must mark the run closed so the
// store sweep drops it and the resume cold-starts - never writing a tool
// result into the transport-closed pipe ("io: read/write on closed pipe").
func TestCursorRunPumpExitMarksClosed(t *testing.T) {
	serverIn, testIn := io.Pipe()
	run := format.NewCursorRun(io.Discard, serverIn, nil, func() { _ = testIn.Close() }, 5*time.Second)
	run.Start()
	testIn.Close() // upstream stream dies while parked
	deadline := time.Now().Add(2 * time.Second)
	for !run.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("pump exit did not mark the run closed")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// A resume against the dead run fails cleanly instead of hitting the pipe.
	res := run.ResumeTurn(t.Context(), map[string]struct {
		Text    string
		IsError bool
	}{"call-x": {"ok", false}}, "", func(map[string]any) error { return nil })
	if res.Outcome != format.TurnErrored {
		t.Fatalf("resume on a dead run = %v, want TurnErrored", res.Outcome)
	}
}
func TestRecordCursorTools(t *testing.T) {
	rec := metrics.Record{ToolNames: []string{"read"}}
	calls := []format.CursorToolCall{
		{CallID: "a", Name: "read"},
		{CallID: "b", Name: "grep"},
		{CallID: "c", Name: "read"},
	}
	recordCursorTools(&rec, calls)
	if rec.ToolCalls != 3 {
		t.Errorf("ToolCalls = %d, want 3", rec.ToolCalls)
	}
	if len(rec.ToolNames) != 2 || rec.ToolNames[0] != "read" || rec.ToolNames[1] != "grep" {
		t.Errorf("ToolNames = %v, want [read grep] (deduped)", rec.ToolNames)
	}
	recordCursorTools(&rec, nil)
	if rec.ToolCalls != 3 || len(rec.ToolNames) != 2 {
		t.Error("empty call list mutated the record")
	}
}
