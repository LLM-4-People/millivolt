package web

// Round 2 D1/D2 regressions: a pooled scratch outlives the request that filled
// it, so release must not keep that request's record pointers or bytes.
// Otherwise a record evicted by Buffer.Reset stays reachable through the ring
// scratch, and the previous dashboard JSON with request previews stays
// reachable through the encoder buffer, until the pool entry is reused or
// collected.

import (
	"bytes"
	"fmt"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func seedPooledRecords(t *testing.T, slots int) (*metrics.Buffer, *AggAPI) {
	t.Helper()
	buf := metrics.NewBuffer(slots)
	for i := 0; i < slots; i++ {
		buf.Record(&metrics.Record{
			ID:         fmt.Sprintf("pooled-%d", i),
			StatusCode: 200,
			Start:      time.UnixMilli(1_800_000_000_000 + int64(i)),
		})
	}
	return buf, NewAggAPI(buf, nil, time.Second)
}

func assertNoRetainedRecords(t *testing.T, scratch *ringScratch, label string) {
	t.Helper()
	for index, record := range (*scratch)[:cap(*scratch)] {
		if record != nil {
			t.Fatalf("%s: pooled scratch slot %d still pins %q", label, index, record.ID)
		}
	}
}

func TestRingScratchReleaseClearsPooledRecords(t *testing.T) {
	const slots = 64
	buf, api := seedPooledRecords(t, slots)
	scratch, view := api.ringSnapshot(0)
	if len(view) != slots {
		t.Fatalf("ringSnapshot = %d records, want %d", len(view), slots)
	}
	// Reset retires the ring's own references, as a purge does; the released
	// scratch is now the only possible owner of those records.
	buf.Reset()
	releaseRing(scratch, view)
	assertNoRetainedRecords(t, scratch, "full ring")
}

func TestRingScratchReleaseClearsFilteredTail(t *testing.T) {
	const slots = 64
	buf, api := seedPooledRecords(t, slots)
	scratch, view := api.ringSnapshot(0)
	// A since filter leaves a shorter view over the same backing array. The
	// slots the filter skipped must be cleared too, not just the visible tail.
	filtered := view[:0]
	for _, record := range view {
		if record.Start.UnixMilli() >= 1_800_000_000_000+60 {
			filtered = append(filtered, record)
		}
	}
	if len(filtered) != 4 {
		t.Fatalf("filtered view = %d records, want 4", len(filtered))
	}
	buf.Reset()
	releaseRing(scratch, filtered)
	assertNoRetainedRecords(t, scratch, "filtered ring")
}

func TestWriteJSONClearsPooledBuffer(t *testing.T) {
	const marker = "pooled-preview-marker-6f1c2b"
	// The pool drops entries at GC. Pin it so the just-released buffer is the
	// one this test inspects instead of a fresh allocation.
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)

	recorder := httptest.NewRecorder()
	writeJSON(recorder, map[string]string{"preview": marker})
	if !strings.Contains(recorder.Body.String(), marker) {
		t.Fatalf("response body lost the marker: %s", recorder.Body.String())
	}
	for index := 0; index < 8; index++ {
		buf := jsonBufferPool.Get().(*bytes.Buffer)
		data := buf.Bytes()
		if bytes.Contains(data[:cap(data)], []byte(marker)) {
			t.Fatalf("pooled JSON buffer %d still retains %q after release", index, marker)
		}
		jsonBufferPool.Put(buf)
	}
}
