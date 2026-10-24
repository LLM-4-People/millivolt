package proxy

// E-U regression: a partial client write counts the bytes the writer accepted,
// not only a fully successful write.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type partialResponseWriter struct {
	header http.Header
	n      int
	err    error
}

func (w *partialResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *partialResponseWriter) WriteHeader(int) {}
func (w *partialResponseWriter) Write([]byte) (int, error) {
	return w.n, w.err
}

func TestDisconnectWriterCountsPartialWrite(t *testing.T) {
	rec := &metrics.Record{StatusCode: http.StatusOK}
	dw := disconnectWriter{w: &partialResponseWriter{n: 2, err: io.ErrClosedPipe}, rec: rec}
	n, err := dw.Write([]byte("hello"))
	if n != 2 || err == nil {
		t.Fatalf("Write = %d/%v, want the fake partial error", n, err)
	}
	if rec.ResponseBytes != 2 {
		t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
	}
	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("record = %+v, want a 499 client disconnect", rec)
	}
}

func TestNonStreamBodyCountsPartialWrite(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{StatusCode: http.StatusOK}
	w := &partialResponseWriter{n: 2, err: io.ErrClosedPipe}
	s.nonStreamBody(context.Background(), w, strings.NewReader("hello"), rec)
	if rec.ResponseBytes != 2 {
		t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
	}
	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("record = %+v, want a 499 client disconnect", rec)
	}
}
