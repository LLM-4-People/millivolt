package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// slowResponseWriter blocks every body write, simulating a slow client.
type slowResponseWriter struct {
	header http.Header
	delay  time.Duration
}

func (w *slowResponseWriter) Header() http.Header         { return w.header }
func (w *slowResponseWriter) WriteHeader(int)             {}
func (w *slowResponseWriter) Flush()                      {}
func (w *slowResponseWriter) Write(p []byte) (int, error) { time.Sleep(w.delay); return len(p), nil }

// TestTranslatedStreamTimestampsBeforeClientWrite pins the timing anchor for
// the translated SSE path: the analyzer timestamps a frame before the client
// write. A slow client must not stretch TTFT or the decode window.
func TestTranslatedStreamTimestampsBeforeClientWrite(t *testing.T) {
	const clientDelay = 250 * time.Millisecond
	body := strings.NewReader(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"!\"}}]}\n\n" +
			"data: [DONE]\n\n")
	w := &slowResponseWriter{header: make(http.Header), delay: clientDelay}
	s := New(config.Default(), metrics.NewBuffer(1))

	start := time.Now()
	rec := &metrics.Record{Start: start, FinalAttemptAt: start, End: start.Add(time.Second)}
	s.streamBodyTranslated(context.Background(), w, body, rec, w)
	rec.End = time.Now()
	metrics.FinalizeRecord(rec)

	if rec.TTFTMs >= int64(clientDelay/time.Millisecond)/2 {
		t.Fatalf("TTFTMs = %d, want well below the %v client write delay (timestamp must be captured before Write)",
			rec.TTFTMs, clientDelay)
	}
	if rec.DecodeTPS <= 0 {
		t.Fatalf("DecodeTPS = %v, want a positive generation rate for the two content chunks", rec.DecodeTPS)
	}
}
