package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingWriter keeps every Write's payload so a test can assert the feed's
// write-level framing: an SSE frame may be split across writes, but no single
// write may exceed the batcher's bound.
type recordingWriter struct {
	*httptest.ResponseRecorder
	mu     sync.Mutex
	writes []int
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes = append(w.writes, len(p))
	w.mu.Unlock()
	return w.ResponseRecorder.Write(p)
}

func (w *recordingWriter) sizes() []int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]int(nil), w.writes...)
}

// TestSnapshotDeltaStreamsThroughBatchBound proves a valid but very old cursor
// cannot produce one oversized SSE write. The delta must still arrive verbatim
// (the deliberate "a delta is never truncated" rule), in order, inside the one
// snapshot frame the browser consumes, while every individual write stays
// within the live feed's batch bound.
func TestSnapshotDeltaStreamsThroughBatchBound(t *testing.T) {
	const (
		records = 400
		// One retained error message is enough to make each record large
		// relative to the batch bound, so the encoded delta is several
		// batches wide.
		message = 400
	)
	b := NewBuffer(records + 1)
	for i := 0; i < records; i++ {
		r := record(fmt.Sprintf("record-%04d", i))
		r.ErrorMsg = strings.Repeat("x", message)
		b.Record(r) // seqs 1..records
	}
	// A cursor at the very beginning of the still-valid range: the delta is
	// every record but the first, far more than one batch.
	const cursor = 1

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", fmt.Sprint(cursor))
	w := &recordingWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		b.HandleStream(w, req)
		close(done)
	}()
	waitFor(t, func() bool {
		for _, size := range w.sizes() {
			if size > 0 {
				return true
			}
		}
		return false
	}, "snapshot body write")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("HandleStream did not return after cancel")
	}

	sizes := w.sizes()
	if len(sizes) < 2 {
		t.Fatalf("snapshot was written in %d write(s) (%v): a delta wider than one batch must stream through the batch bound", len(sizes), sizes)
	}
	for i, size := range sizes {
		if size > sseBatchBytes {
			t.Fatalf("write %d carried %d bytes, more than the %d-byte batch bound (all writes: %v)", i, size, sseBatchBytes, sizes)
		}
	}

	body := w.Body.String()
	if got := strings.Count(body, "event: snapshot\n"); got != 1 {
		t.Fatalf("snapshot frame count = %d, want exactly 1 (the browser consumes one snapshot per delivery)", got)
	}
	if !strings.HasSuffix(body, "\n\n") {
		t.Fatalf("snapshot frame is not terminated: %q", body[len(body)-16:])
	}
	const marker = "event: snapshot\ndata: "
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no snapshot frame in body: %q", body[:min(len(body), 120)])
	}
	data := body[i+len(marker):]
	data = strings.TrimSuffix(data, "\n\n")
	var p struct {
		Records     []*Record `json:"records"`
		Incremental bool      `json:"incremental"`
		Seq         int64     `json:"seq"`
	}
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatalf("snapshot payload unparseable after chunked framing: %v", err)
	}
	if !p.Incremental {
		t.Fatal("an old in-range cursor must still answer an incremental delta")
	}
	if len(p.Records) != records-1 {
		t.Fatalf("delta records = %d, want %d delivered verbatim (never truncated)", len(p.Records), records-1)
	}
	for i, rec := range p.Records {
		want := fmt.Sprintf("record-%04d", i+1)
		if rec == nil || rec.ID != want {
			t.Fatalf("delta record %d = %+v, want %s in order", i, rec, want)
		}
		if len(rec.ErrorMsg) != message {
			t.Fatalf("delta record %d lost its retained error message: %d bytes", i, len(rec.ErrorMsg))
		}
	}
	if p.Seq != records {
		t.Fatalf("delta seq = %d, want %d", p.Seq, records)
	}
}
