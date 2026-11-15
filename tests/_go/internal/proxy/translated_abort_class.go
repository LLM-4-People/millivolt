package proxy

// R1-3 sibling: the translated relay must share the abort-class owner the
// native stream uses. An abort on a translated stream - a failed client write
// or a scanner error after a canceled context - is the same 499 plus the
// truncation class; a plain 499 without it reads as healthy.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

func translatedContentFrame(text string) []byte {
	return []byte(`data: {"id":"1","choices":[{"delta":{"content":"` + text + `"}}]}` + "\n\n")
}

// translatedAbortRecord relays body through streamBodyTranslated on a canceled
// client context and returns the finalized record.
func translatedAbortRecord(t *testing.T, body io.Reader, w http.ResponseWriter) *metrics.Record {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatal("writer does not flush")
	}
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{StatusCode: http.StatusOK}
	s.streamBodyTranslated(ctx, w, body, rec, flusher, ir.Chat, 1<<20)
	return rec
}

func TestTranslatedStreamAbortKeepsTheTruncationClass(t *testing.T) {
	arms := []struct {
		name string
		body func() io.Reader
		// writer decides the shape: a socket that dies on the second write
		// (a write-detected abort) or one that accepts everything while the
		// transformer's pipe closes with an error.
		writer func() http.ResponseWriter
	}{
		{
			name: "write-abort",
			body: func() io.Reader {
				return &splitChunkReader{chunks: [][]byte{translatedContentFrame("hello"), translatedContentFrame("world")}}
			},
			writer: func() http.ResponseWriter { return &failAfterStreamWriter{remaining: 1} },
		},
		{
			name: "scanner-error",
			body: func() io.Reader {
				return &errAfterChunks{chunks: [][]byte{translatedContentFrame("hello")}, err: errors.New("connection reset")}
			},
			writer: func() http.ResponseWriter { return httptest.NewRecorder() },
		},
	}
	for _, arm := range arms {
		t.Run(arm.name, func(t *testing.T) {
			rec := translatedAbortRecord(t, arm.body(), arm.writer())
			if rec.StatusCode != metrics.StatusClientClosedRequest || !rec.ClientDisconnected {
				t.Fatalf("record = status %d disconnected %v, want the 499 disconnect", rec.StatusCode, rec.ClientDisconnected)
			}
			if !rec.IsError() || rec.ErrorType != sse.TypeUpstreamError || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record = status %d type %q code %q isError %v, want the truncation class the native path shares",
					rec.StatusCode, rec.ErrorType, rec.ErrorCode, rec.IsError())
			}
			if rec.ErrorMsg == "" {
				t.Fatalf("record lost the truncation message: %+v", rec)
			}
			// The delivered chunk still books its answer content: the abort
			// must not erase what the client already saw.
			if !rec.HadAnswerContent {
				t.Fatalf("record lost the delivered answer content: %+v", rec)
			}
		})
	}
}
