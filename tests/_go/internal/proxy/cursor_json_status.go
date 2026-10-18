package proxy

// Cursor error-finalization status contract: a non-streaming 502 body is the
// response the client received, so the record must carry the same status; a
// committed stream keeps its 200, and a dead local socket keeps the documented
// 200 plus ClientDisconnected.

import (
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func cursorStatusRun(t *testing.T) *providerformat.CursorRun {
	t.Helper()
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	t.Cleanup(run.Close)
	return run
}

// TestWriteRunJSONErrorRecordsClientStatus pins the non-streaming failure: the
// live client receives 502 and the record describes that same 502. Streaming
// is the deliberate exception (headers were committed before the turn failed),
// so its record stays 200 with the stream error class.
func TestWriteRunJSONErrorRecordsClientStatus(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	res := providerformat.TurnResult{Outcome: providerformat.TurnErrored, Err: fmt.Errorf("transform boom")}

	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	s.writeRunJSON(w, cursorStatusRun(t), res, "", nil, rec, "id-json", cursorTurnRender{}, false)
	if w.Code != 502 || rec.StatusCode != 502 {
		t.Fatalf("non-streaming error status: client=%d record=%d, want 502/502", w.Code, rec.StatusCode)
	}
	if rec.ErrorType != "transform_error" {
		t.Fatalf("non-streaming error type = %q, want transform_error", rec.ErrorType)
	}

	streamRec := &metrics.Record{StatusCode: 200}
	streamW := httptest.NewRecorder()
	s.finishRunTurn(streamW, cursorStatusRun(t), res, func(map[string]any, any) error { return nil }, streamRec, "id-stream", cursorTurnRender{}, false)
	if streamRec.StatusCode != 200 {
		t.Fatalf("committed stream status = %d, want 200 (headers were already sent)", streamRec.StatusCode)
	}
	if streamRec.ErrorType != errStreamRead {
		t.Fatalf("committed stream error type = %q, want %s", streamRec.ErrorType, errStreamRead)
	}
}

// TestWriteRunJSONVoidRecordsClientStatus pins the void's non-streaming status
// on the live recorder; the existing dead-socket test keeps the failed-write
// exception at 200 plus ClientDisconnected.
func TestWriteRunJSONVoidRecordsClientStatus(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	res := providerformat.TurnResult{Outcome: providerformat.TurnFinished}
	s.writeRunJSON(w, cursorStatusRun(t), res, "", nil, rec, "id-void", cursorTurnRender{}, true)
	if w.Code != 502 || rec.StatusCode != 502 {
		t.Fatalf("void status: client=%d record=%d, want 502/502", w.Code, rec.StatusCode)
	}
	if rec.ErrorType != cursorEmptyTurn || rec.ErrorCode != cursorEmptyTurn {
		t.Fatalf("void classification = %q/%q, want empty_turn/empty_turn", rec.ErrorType, rec.ErrorCode)
	}
}

// TestWriteRunJSONInvalidUsagePairing pins the generic relay's ordering: the
// transform error owns the type while invalid usage owns the code.
func TestWriteRunJSONInvalidUsagePairing(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}
	w := httptest.NewRecorder()
	res := providerformat.TurnResult{Outcome: providerformat.TurnErrored, Err: metrics.ErrMetricRange}
	s.writeRunJSON(w, cursorStatusRun(t), res, "", nil, rec, "id-usage", cursorTurnRender{estimate: func() int64 { return 7 }}, false)
	if rec.ErrorType != "transform_error" || rec.ErrorCode != metrics.CodeInvalidUsage {
		t.Fatalf("invalid usage pairing = %q/%q, want transform_error/%s", rec.ErrorType, rec.ErrorCode, metrics.CodeInvalidUsage)
	}
	if w.Code != 502 || rec.StatusCode != 502 {
		t.Fatalf("invalid usage status: client=%d record=%d, want 502/502", w.Code, rec.StatusCode)
	}
}
