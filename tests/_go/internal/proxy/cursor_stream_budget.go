package proxy

// Aggregate retention budgets for parked Cursor runs: admission, growth of an
// already parked run, blob growth, and the normal park/release path.

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
)

// cursorStreamExecFrame builds AgentServerMessage{exec_server_message{id,
// mcp_args{name, args, tool_call_id}}} with an argument value of argsLen bytes.
func cursorStreamExecFrame(id uint64, callID string, argsLen int) []byte {
	value := cursorBudgetBytesField(3, []byte(strings.Repeat("x", argsLen)))
	entry := append(cursorBudgetBytesField(1, []byte("k")), value...)
	mcp := cursorBudgetBytesField(1, []byte("tool"))
	mcp = append(mcp, cursorBudgetBytesField(2, entry)...)
	mcp = append(mcp, cursorBudgetBytesField(3, []byte(callID))...)
	ex := cursorBudgetVarintField(1, id)
	ex = append(ex, cursorBudgetBytesField(11, mcp)...)
	return cursorBudgetFrame(cursorBudgetBytesField(2, ex))
}

func cursorSetBlobFrame(id uint64, blobID, data []byte) []byte {
	args := cursorBudgetBytesField(1, blobID)
	args = append(args, cursorBudgetBytesField(2, data)...)
	kv := cursorBudgetVarintField(1, id)
	kv = append(kv, cursorBudgetBytesField(3, args)...)
	return cursorBudgetFrame(cursorBudgetBytesField(4, kv))
}

// cursorPendingRun starts a run over a controllable upstream pipe and feeds one
// blocking tool call, so its pending byte accounting is populated by the real
// pump path.
func cursorPendingRun(t *testing.T, callID string, argsLen int) (*providerformat.CursorRun, *io.PipeWriter) {
	t.Helper()
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(io.Discard, pr, nil, func() { _ = pw.Close() }, 5*time.Second)
	run.Start()
	t.Cleanup(run.Close)
	if _, err := pw.Write(cursorStreamExecFrame(1, callID, argsLen)); err != nil {
		t.Fatalf("write tool-call frame: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !run.PendingToolCallIDs()[callID] {
		if time.Now().After(deadline) {
			t.Fatalf("pump did not register pending call %q", callID)
		}
		time.Sleep(time.Millisecond)
	}
	return run, pw
}

func waitCursorClosed(t *testing.T, run *providerformat.CursorRun) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !run.Closed() {
		if time.Now().After(deadline) {
			t.Fatal("run did not fail closed at the aggregate budget")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestParkAdmissionStopsAtAggregateStreamBudget(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	first, _ := cursorPendingRun(t, "call-first", 4<<10)
	second, _ := cursorPendingRun(t, "call-second", 4<<10)
	firstBytes := first.PendingBytes() + first.QueuedBytes()
	secondBytes := second.PendingBytes() + second.QueuedBytes()
	if firstBytes <= 0 || secondBytes <= 0 {
		t.Fatalf("fixture retained no stream bytes: first=%d second=%d", firstBytes, secondBytes)
	}
	s.streamReserveBytes = 0
	s.streamBytesMax = firstBytes + secondBytes - 1

	if !s.parkWith(cursorScope{}, first, []string{"call-first"}) {
		t.Fatal("below-budget first run was rejected")
	}
	if s.parkWith(cursorScope{}, second, []string{"call-second"}) {
		t.Fatal("candidate crossing the aggregate stream budget was parked")
	}
	if got := s.streamUsed(); got != firstBytes {
		t.Fatalf("aggregate stream bytes = %d, want %d", got, firstBytes)
	}

	// Dropping the admitted run releases its bytes, so the refused candidate
	// can park once there is room.
	s.drop(first)
	if got := s.streamUsed(); got != 0 {
		t.Fatalf("dropped run retained %d stream bytes, want 0", got)
	}
	if !s.parkWith(cursorScope{}, second, []string{"call-second"}) {
		t.Fatal("candidate was rejected after the aggregate budget was released")
	}
}

func TestParkedRunCannotGrowPastReservedAggregateShare(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	run, pw := cursorPendingRun(t, "call-first", 2<<10)
	before := run.PendingBytes()
	queued := run.QueuedBytes()
	s.streamReserveBytes = 64
	s.streamBytesMax = before + queued + s.streamReserveBytes + 1 // effective share: current + 1

	if !s.parkWith(cursorScope{}, run, []string{"call-first"}) {
		t.Fatal("first run was rejected below the aggregate budget")
	}
	// The pump is still reading; a second tool call would grow the run's
	// retained pending bytes past its reserved aggregate share.
	_, _ = pw.Write(cursorStreamExecFrame(2, "call-second", 2<<10))
	waitCursorClosed(t, run)
	if got := run.PendingBytes(); got != before {
		t.Fatalf("failed growth changed pending bytes: got %d, want %d", got, before)
	}
	if ids := run.PendingToolCallIDs(); len(ids) != 1 || !ids["call-first"] {
		t.Fatalf("failed growth changed pending calls: %v", ids)
	}
	if got := s.streamUsed(); got != before+queued {
		t.Fatalf("failed growth changed aggregate bytes: got %d, want %d", got, before+queued)
	}
	s.drop(run)
	if got := s.streamUsed(); got != 0 {
		t.Fatalf("dropped failed run retained %d stream bytes, want 0", got)
	}
}

func TestParkedRunBlobGrowthFailsAtAggregateBudget(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []map[string]any{
			{"role": "assistant", "content": strings.Repeat("x", 1<<20)},
			{"role": "user", "content": "next"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, blobs, _, err := providerformat.TranslateCursorRunRequestWithConversation(body, "")
	if err != nil {
		t.Fatalf("building history blobs: %v", err)
	}
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(io.Discard, pr, blobs, func() { _ = pw.Close() }, 5*time.Second)
	run.Start()
	t.Cleanup(run.Close)
	beforeBytes, beforeEntries := run.BlobBytes(), run.BlobEntries()

	s := newCursorRunStore(time.Hour)
	s.blobBytesMax = beforeBytes + 100
	if !s.parkWith(cursorScope{}, run, []string{"call-first"}) {
		t.Fatal("blob run was rejected below the aggregate budget")
	}
	_, _ = pw.Write(cursorSetBlobFrame(2, bytes.Repeat([]byte("i"), 32), bytes.Repeat([]byte("b"), 200)))
	waitCursorClosed(t, run)
	if got := run.BlobBytes(); got != beforeBytes {
		t.Fatalf("failed blob growth changed retained bytes: got %d, want %d", got, beforeBytes)
	}
	if got := run.BlobEntries(); got != beforeEntries {
		t.Fatalf("failed blob growth changed retained entries: got %d, want %d", got, beforeEntries)
	}
	if got := s.blobBytesUsed(); got != beforeBytes {
		t.Fatalf("failed blob growth changed aggregate bytes: got %d, want %d", got, beforeBytes)
	}
	s.drop(run)
	if got := s.blobBytesUsed(); got != 0 {
		t.Fatalf("dropped run retained %d blob bytes, want 0", got)
	}
	if got := s.blobEntriesUsed(); got != 0 {
		t.Fatalf("dropped run retained %d blob entries, want 0", got)
	}
}

// TestParkedStreamBudgetPreservesNormalResume is the normal-path regression:
// a small parked run is admitted, found by its tool tail, and releases its
// aggregate accounting on unpark.
func TestParkedStreamBudgetPreservesNormalResume(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	if got, want := s.streamBytesMax, int64(cursorParkedStreamBytesMax); got != want {
		t.Fatalf("default parked stream budget = %d, want %d", got, want)
	}
	if got, want := s.streamReserveBytes, providerformat.CursorRunRetainedBytesMax(); got != want {
		t.Fatalf("active-run reserve = %d, want the per-run cap %d", got, want)
	}
	run, _ := cursorPendingRun(t, "call-a", 32)
	if !s.parkWith(cursorScope{}, run, []string{"call-a"}) {
		t.Fatal("normal parked run was rejected")
	}
	if got := s.streamUsed(); got != run.PendingBytes()+run.QueuedBytes() {
		t.Fatalf("aggregate stream bytes = %d, want %d", got, run.PendingBytes()+run.QueuedBytes())
	}
	got, tail := s.findByToolTail(cursorScope{}, []string{"call-a"})
	if got != run || len(tail) != 1 || tail[0] != "call-a" {
		t.Fatalf("normal resume lookup = %v %v", got, tail)
	}
	if used := s.streamUsed(); used != 0 {
		t.Fatalf("unparked run retained %d stream bytes, want 0", used)
	}
}
