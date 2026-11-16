package metrics

import (
	"net/http/httptest"
	"testing"
	"time"
)

type sseDeadlineRecorder struct {
	*httptest.ResponseRecorder
	write time.Time
}

func (r *sseDeadlineRecorder) SetWriteDeadline(t time.Time) error { r.write = t; return nil }

// TestSSEStreamWriterArmsProgressDeadline pins the dashboard feed's write bound:
// a tab that stops reading fails its write instead of pinning the handler. The
// bound is armed by the one batched writer every frame and the snapshot share,
// so this covers the production path rather than a helper beside it.
func TestSSEStreamWriterArmsProgressDeadline(t *testing.T) {
	rec := &sseDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	stream := newSSEStreamWriter(rec, rec)
	if err := stream.frame("record", 7, true, []byte(`{"id":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := stream.flush(); err != nil {
		t.Fatal(err)
	}
	if rec.write.IsZero() || time.Until(rec.write) <= 0 || time.Until(rec.write) > sseWriteProgressDeadline+time.Second {
		t.Fatalf("SSE write deadline = %v, want a near-future progress bound", rec.write)
	}
	if got := rec.Body.String(); got != "id: 7\nevent: record\ndata: {\"id\":\"x\"}\n\n" {
		t.Fatalf("frame = %q", got)
	}
}
