package proxy

// R1-3 regression: one aborted stream must be classified by its outcome, not
// by which write happened to fail. The same truncated stream with the same
// canceled client context delivered as a clean EOF, a failed client write or
// an upstream read error has one answer: the 499 disconnect plus the
// truncation class, never a healthy-looking plain 499 beside it.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// errAfterChunks serves its chunks, then returns err instead of io.EOF.
type errAfterChunks struct {
	chunks [][]byte
	err    error
}

func (r *errAfterChunks) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, r.err
	}
	n := copy(p, r.chunks[0])
	if n < len(r.chunks[0]) {
		r.chunks[0] = r.chunks[0][n:]
	} else {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

// abortShapeRecord relays one content chunk through streamBody on a canceled
// client context and returns the finalized record. `remaining` decides whether
// the content write reaches the wire: zero makes it the failed write.
func abortShapeRecord(t *testing.T, body io.Reader, remaining int) *metrics.Record {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{StatusCode: http.StatusOK}
	var uerr upstreamErrorRescue
	s.streamBody(ctx, &failAfterStreamWriter{remaining: remaining}, body, rec, false, false, &uerr, ir.Chat)
	return rec
}

func TestStreamAbortShapeDecidesFailureClassification(t *testing.T) {
	content := []byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
	shapes := []struct {
		name      string
		body      func() io.Reader
		remaining int
	}{
		// Clean EOF: the read loop resolves the hold with the client gone.
		{"clean-eof", func() io.Reader { return &splitChunkReader{chunks: [][]byte{content}} }, 1000},
		// Write-detected abort: the content write itself fails.
		{"write-abort", func() io.Reader { return &splitChunkReader{chunks: [][]byte{content}} }, 0},
		// Upstream read error: never an io.EOF.
		{"read-error", func() io.Reader {
			return &errAfterChunks{chunks: [][]byte{content}, err: errors.New("connection reset")}
		}, 1000},
	}
	for _, tc := range shapes {
		t.Run(tc.name, func(t *testing.T) {
			rec := abortShapeRecord(t, tc.body(), tc.remaining)
			if rec.StatusCode != metrics.StatusClientClosedRequest || !rec.ClientDisconnected {
				t.Fatalf("record = status %d disconnected %v, want the 499 disconnect", rec.StatusCode, rec.ClientDisconnected)
			}
			if !rec.IsError() || rec.ErrorType != sse.TypeUpstreamError || rec.ErrorCode != metrics.CodeTruncated {
				t.Fatalf("record = status %d type %q code %q isError %v, want the truncation class every shape shares",
					rec.StatusCode, rec.ErrorType, rec.ErrorCode, rec.IsError())
			}
			if rec.ErrorMsg == "" {
				t.Fatalf("record lost the truncation message: %+v", rec)
			}
		})
	}
}

// TestStreamAbortAfterTerminatorStaysFlowEvent is the boundary twin: an abort
// on a stream that already reached its terminator carries no truncation, so
// the 499 stays a plain flow event with no error type.
func TestStreamAbortAfterTerminatorStaysFlowEvent(t *testing.T) {
	body := &splitChunkReader{chunks: [][]byte{
		[]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"),
		[]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"),
		[]byte("data: [DONE]\n\n"),
	}}
	// One write carries the content chunk; the [DONE] write fails after the
	// terminator was already fed, so the outcome is decided.
	rec := abortShapeRecord(t, body, 1)
	if rec.StatusCode != metrics.StatusClientClosedRequest || !rec.ClientDisconnected {
		t.Fatalf("record = status %d disconnected %v, want the 499 disconnect", rec.StatusCode, rec.ClientDisconnected)
	}
	if rec.IsError() || rec.ErrorType != "" || rec.ErrorCode != "" {
		t.Fatalf("record = type %q code %q isError %v, want a plain 499 flow event", rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
}
