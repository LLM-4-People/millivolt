package proxy

// Aggregate retained-blob admission for parked Cursor runs.

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/format"
)

// parkedRunWithBlobBytes builds a run whose KV store retains at least the
// requested content bytes, using the real history translation path.
func parkedRunWithBlobBytes(t *testing.T, contentBytes int) *format.CursorRun {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []map[string]any{
			{"role": "assistant", "content": strings.Repeat("x", contentBytes)},
			{"role": "user", "content": "next"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, blobs, _, err := format.TranslateCursorRunRequestWithConversation(body, "")
	if err != nil {
		t.Fatalf("building history blobs: %v", err)
	}
	if blobs.RetainedBytes() < int64(contentBytes) {
		t.Fatalf("fixture retained %d blob bytes, want at least %d", blobs.RetainedBytes(), contentBytes)
	}
	return format.NewCursorRun(io.Discard, emptyReader{}, blobs, func() {}, 5*time.Second)
}

// parkedRunWithBlobEntries builds a run whose KV store retains exactly the
// requested number of unique history blobs via the real translation path.
func parkedRunWithBlobEntries(t *testing.T, entries int) *format.CursorRun {
	t.Helper()
	messages := make([]map[string]any, 0, entries+1)
	for i := 0; i < entries; i++ {
		messages = append(messages, map[string]any{"role": "assistant", "content": "h" + strconv.Itoa(i)})
	}
	messages = append(messages, map[string]any{"role": "user", "content": "next"})
	body, err := json.Marshal(map[string]any{"model": "m", "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	_, blobs, _, err := format.TranslateCursorRunRequestWithConversation(body, "")
	if err != nil {
		t.Fatalf("building history blobs: %v", err)
	}
	if got := blobs.RetainedEntries(); got != entries {
		t.Fatalf("fixture retained %d blob entries, want %d", got, entries)
	}
	return format.NewCursorRun(io.Discard, emptyReader{}, blobs, func() {}, 5*time.Second)
}

// TestParkRejectsAggregateBlobBudgetOverflow proves a candidate that would
// cross the aggregate budget is refused without evicting, mutating or
// indexing the rejected run, while the first parked run stays resumable.
func TestParkRejectsAggregateBlobBudgetOverflow(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	s.blobBytesMax = 8 << 20
	first := parkedRunWithBlobBytes(t, 5<<20)
	second := parkedRunWithBlobBytes(t, 5<<20)
	defer first.Close()
	defer second.Close()

	firstBytes := first.BlobBytes()
	if firstBytes >= s.blobBytesMax {
		t.Fatalf("fixture first run retained %d bytes, want below the %d budget", firstBytes, s.blobBytesMax)
	}
	if !s.parkWith(cursorScope{}, first, []string{"call-first"}) {
		t.Fatal("below-budget first run was rejected")
	}
	if s.parkWith(cursorScope{}, second, []string{"call-second"}) {
		t.Fatal("candidate crossing the aggregate blob budget was parked")
	}

	s.mu.Lock()
	_, firstStill := s.entries[first]
	_, secondParked := s.entries[second]
	parked := len(s.entries)
	secondIndexed := s.byCall[cursorCallKey{cursorScope{}, "call-second"}] != nil
	s.mu.Unlock()
	if !firstStill || secondParked || parked != 1 {
		t.Fatalf("rejected park changed entries: first=%v second=%v count=%d", firstStill, secondParked, parked)
	}
	if secondIndexed {
		t.Fatal("rejected park indexed the refused run")
	}
	if got := first.BlobBytes(); got != firstBytes {
		t.Fatalf("rejected park mutated the parked run's blobs: got %d, want %d", got, firstBytes)
	}
	if got, tail := s.findByToolTail(cursorScope{}, []string{"call-first"}); got != first || len(tail) != 1 || tail[0] != "call-first" {
		t.Fatal("rejected park evicted the admitted run")
	}
}

// TestParkAdmitsBelowAggregateBlobBudget is the normal-path regression: runs
// whose combined retained blobs fit the budget still park.
func TestParkAdmitsBelowAggregateBlobBudget(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	if got := s.blobBytesMax; got != cursorParkedBlobBytesMax {
		t.Fatalf("default parked blob budget = %d, want %d", got, cursorParkedBlobBytesMax)
	}
	s.blobBytesMax = 8 << 20
	a := parkedRunWithBlobBytes(t, 1<<20)
	b := parkedRunWithBlobBytes(t, 1<<20)
	defer a.Close()
	defer b.Close()

	parkWithIDs(t, s, a, "call-a")
	parkWithIDs(t, s, b, "call-b")
	if got, want := a.BlobBytes()+b.BlobBytes(), s.blobBytesMax; got > want {
		t.Fatalf("admitted runs retained %d bytes, want within %d", got, want)
	}
	s.mu.Lock()
	parked := len(s.entries)
	s.mu.Unlock()
	if parked != 2 {
		t.Fatalf("parked entries = %d, want 2", parked)
	}
}

// TestParkRejectsAggregateBlobEntryBudgetOverflow proves a candidate that would
// cross the aggregate entry budget is refused without evicting, mutating or
// indexing the rejected run, while the first parked run stays resumable.
func TestParkRejectsAggregateBlobEntryBudgetOverflow(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	if got := s.blobEntriesMax; got != cursorParkedBlobEntriesMax {
		t.Fatalf("default parked blob entry budget = %d, want %d", got, cursorParkedBlobEntriesMax)
	}
	s.blobEntriesMax = 5
	first := parkedRunWithBlobEntries(t, 3)
	second := parkedRunWithBlobEntries(t, 3)
	defer first.Close()
	defer second.Close()

	if got := first.BlobEntries(); got != 3 {
		t.Fatalf("fixture first run retained %d entries, want 3", got)
	}
	if got := second.BlobEntries(); got != 3 {
		t.Fatalf("fixture second run retained %d entries, want 3", got)
	}
	if first.BlobEntries()+second.BlobEntries() <= s.blobEntriesMax {
		t.Fatalf("fixture entries %d+%d do not cross the %d budget", first.BlobEntries(), second.BlobEntries(), s.blobEntriesMax)
	}
	if !s.parkWith(cursorScope{}, first, []string{"call-first"}) {
		t.Fatal("below-budget first run was rejected")
	}
	if s.parkWith(cursorScope{}, second, []string{"call-second"}) {
		t.Fatal("candidate crossing the aggregate entry budget was parked")
	}

	s.mu.Lock()
	_, firstStill := s.entries[first]
	_, secondParked := s.entries[second]
	parked := len(s.entries)
	secondIndexed := s.byCall[cursorCallKey{cursorScope{}, "call-second"}] != nil
	s.mu.Unlock()
	if !firstStill || secondParked || parked != 1 {
		t.Fatalf("rejected park changed entries: first=%v second=%v count=%d", firstStill, secondParked, parked)
	}
	if secondIndexed {
		t.Fatal("rejected park indexed the refused run")
	}
	if got, want := first.BlobEntries(), 3; got != want {
		t.Fatalf("admitted run retained %d entries, want exactly %d", got, want)
	}
	if got := second.BlobEntries(); got != 3 {
		t.Fatalf("rejected run mutated to %d entries, want 3", got)
	}
	if got, tail := s.findByToolTail(cursorScope{}, []string{"call-first"}); got != first || len(tail) != 1 || tail[0] != "call-first" {
		t.Fatal("rejected park evicted the admitted run")
	}
}

// TestParkAdmitsBelowAggregateBlobEntryBudget proves runs whose combined
// entries exactly meet the narrowed budget still park.
func TestParkAdmitsBelowAggregateBlobEntryBudget(t *testing.T) {
	s := newCursorRunStore(time.Hour)
	s.blobEntriesMax = 4
	a := parkedRunWithBlobEntries(t, 2)
	b := parkedRunWithBlobEntries(t, 2)
	defer a.Close()
	defer b.Close()

	parkWithIDs(t, s, a, "call-a")
	parkWithIDs(t, s, b, "call-b")
	if got, want := a.BlobEntries()+b.BlobEntries(), s.blobEntriesMax; got != want {
		t.Fatalf("admitted runs retained %d entries, want exactly %d", got, want)
	}
	s.mu.Lock()
	parked := len(s.entries)
	s.mu.Unlock()
	if parked != 2 {
		t.Fatalf("parked entries = %d, want 2", parked)
	}
}
