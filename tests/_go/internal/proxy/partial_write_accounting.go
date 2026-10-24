package proxy

// E-U regression: a partial client write counts the bytes the writer accepted,
// not only a fully successful write.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/sse"
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

func TestCommitSpooledBodyCountsPartialWrite(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{StatusCode: http.StatusOK}
	w := &partialResponseWriter{n: 2, err: io.ErrClosedPipe}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	s.commitSpooledBody(w, resp, []byte("hello"), rec, metrics.NonStreamAnalysis{})
	if rec.ResponseBytes != 2 {
		t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
	}
	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("record = %+v, want a 499 client disconnect", rec)
	}
}

func TestTranslatedNonStreamCountsPartialWrite(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{StatusCode: http.StatusOK}
	w := &partialResponseWriter{n: 2, err: io.ErrClosedPipe}
	body := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
	req := httptest.NewRequest(http.MethodPost, "http://proxy.example/", nil)
	s.serveNonStreaming(t.Context(), w, resp, req, target, "", nil, rec, "", scheduler.WaiterHooks{})
	if rec.ResponseBytes != 2 {
		t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
	}
	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("record = %+v, want a 499 client disconnect", rec)
	}
}

func TestDegenerateEmittersCountPartialWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		emit func(http.ResponseWriter, *metrics.Record)
	}{
		{
			name: "chat",
			emit: func(w http.ResponseWriter, rec *metrics.Record) {
				var a sse.Analyzer
				emitDegenerateSSE(w, &a, time.Now(), metrics.CodeEmptyCompletion, rec)
			},
		},
		{
			name: "responses",
			emit: func(w http.ResponseWriter, rec *metrics.Record) {
				var a sse.Analyzer
				emitDegenerateResponses(w, &a, time.Now(), metrics.CodeEmptyCompletion, rec)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &metrics.Record{StatusCode: http.StatusOK}
			w := &partialResponseWriter{n: 2, err: io.ErrClosedPipe}
			tc.emit(w, rec)
			if rec.ResponseBytes != 2 {
				t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
			}
			if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
				t.Fatalf("record = %+v, want a 499 client disconnect", rec)
			}
		})
	}
}

// TestTranslatedUpstreamErrorCountsPartialWrite is the R31-NEW-1 regression:
// the translated non-2xx body is a relayed upstream error document and counts
// the bytes a client accepts before disconnecting, like every other client
// write.
func TestTranslatedUpstreamErrorCountsPartialWrite(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{StatusCode: http.StatusInternalServerError}
	w := &partialResponseWriter{n: 2, err: io.ErrClosedPipe}
	body := `{"error":{"message":"upstream exploded","type":"api_error"}}`
	resp := &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}
	target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
	req := httptest.NewRequest(http.MethodPost, "http://proxy.example/", nil)
	s.serveNonStreaming(t.Context(), w, resp, req, target, "", nil, rec, "", scheduler.WaiterHooks{})
	if rec.ResponseBytes != 2 {
		t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
	}
	if !rec.ClientDisconnected || rec.StatusCode != http.StatusInternalServerError {
		t.Fatalf("record = %+v, want a 500 client disconnect", rec)
	}
}

// partialStreamWriter accepts a fixed byte count and then fails, with the
// Flush method streamBody requires to enter its per-event writeOut path.
type partialStreamWriter struct {
	header http.Header
	n      int
	err    error
}

func (w *partialStreamWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *partialStreamWriter) WriteHeader(int)           {}
func (w *partialStreamWriter) Write([]byte) (int, error) { return w.n, w.err }
func (w *partialStreamWriter) Flush()                    {}

// TestStreamBodyCountsPartialWrite is the H4 regression: the SSE relay's
// writeOut is the primary client-write owner, and a partial (n, err) write
// must book the accepted bytes and classify the client as gone. A
// count-on-nil revert at that owner otherwise leaves the whole suite green.
func TestStreamBodyCountsPartialWrite(t *testing.T) {
	body := &splitChunkReader{chunks: [][]byte{
		[]byte(`data: {"id":"1","choices":[{"delta":{"content":"hello"}}]}` + "\n\n"),
	}}
	w := &partialStreamWriter{n: 2, err: io.ErrClosedPipe}
	rec := &metrics.Record{StatusCode: http.StatusOK}
	s := New(config.Default(), metrics.Noop{})
	var uerr upstreamErrorRescue
	if rescue := s.streamBody(context.Background(), w, body, rec, false, false, &uerr, ir.Chat); rescue != rescueNone {
		t.Fatalf("rescue = %v, want none", rescue)
	}
	if rec.ResponseBytes != 2 {
		t.Fatalf("ResponseBytes = %d, want the 2 delivered bytes", rec.ResponseBytes)
	}
	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("record = %+v, want a 499 client disconnect", rec)
	}
}
