package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func TestInvalidTotalDoesNotBecomeQuotaRefund(t *testing.T) {
	n := int64(1) << 62
	rec := &metrics.Record{Usage: metrics.Usage{InputTokens: n, OutputTokens: n}}
	if got := settleTokens(rec); got != scheduler.TokensUnavailable {
		t.Fatalf("invalid total settled as %d, want unavailable", got)
	}
	if rec.ErrorCode != metrics.CodeInvalidUsage || rec.Usage != (metrics.Usage{}) {
		t.Fatalf("invalid usage was not explicitly unavailable: %+v", rec)
	}
}

func TestRunRenderNeverEmitsWrappedTotals(t *testing.T) {
	for _, stream := range []bool{false, true} {
		run := providerformat.NewCursorRun(io.Discard, bytes.NewReader(nil), nil, func() {}, time.Hour)
		t.Cleanup(run.Close)
		result := providerformat.TurnResult{Outcome: providerformat.TurnFinished, Prompt: int64(1) << 62, Output: int64(1) << 62}
		s := New(config.Default(), metrics.Noop{})
		w := httptest.NewRecorder()
		rec := &metrics.Record{StatusCode: 200}
		render := cursorTurnRender{model: "model-a", includeUsage: true}
		if stream {
			s.finishRunTurn(w, run, result, func(map[string]any, any) error { return nil }, rec, "req-a", render, false)
		} else {
			s.writeRunJSON(w, run, result, "hello", nil, rec, "req-a", render, false)
		}
		if rec.ErrorCode != metrics.CodeInvalidUsage || rec.Usage != (metrics.Usage{}) || strings.Contains(w.Body.String(), `"total_tokens"`) {
			t.Fatalf("stream=%v invalid usage leaked: rec=%+v wire=%s", stream, rec, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"error"`) {
			t.Fatalf("stream=%v invalid counts silently succeeded: %s", stream, w.Body.String())
		}
	}
}

func TestUsageChunkRejectsOverflowBeforeWriting(t *testing.T) {
	w := httptest.NewRecorder()
	err := usageChunk(w, "req-a", "model-a", true, int64(1)<<62, int64(1)<<62, 0)
	if !errors.Is(err, metrics.ErrMetricRange) || w.Body.Len() != 0 {
		t.Fatalf("invalid usage write: err=%v body=%s", err, w.Body.String())
	}
}

func TestValidUsageSettlementPreservesReportedFields(t *testing.T) {
	for _, tt := range []struct {
		usage metrics.Usage
		want  int64
	}{
		{metrics.Usage{}, 0},
		{metrics.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CacheWrite: 2, ReasoningTokens: 1}, 15},
		{metrics.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 17, CacheReadTokens: 3, CacheWrite: 2, ReasoningTokens: 1}, 17},
	} {
		rec := &metrics.Record{Usage: tt.usage}
		if got := settleTokens(rec); got != tt.want || rec.Usage != tt.usage || rec.ErrorCode != "" {
			t.Fatalf("valid usage changed: got=%d record=%+v", got, rec)
		}
	}
}

func TestNativeInvalidUsageCannotRefundReservation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		s := New(config.Default(), metrics.Noop{})
		rec := &metrics.Record{StatusCode: 200}
		w := httptest.NewRecorder()
		body := `{"content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":9223372036854775807,"output_tokens":1}}`
		if stream {
			body = "data: " + `{"type":"message_start","message":{"usage":{"input_tokens":9223372036854775807}}}` + "\n\n" +
				"data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hello"}}` + "\n\n" +
				"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n"
		}
		resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
		if stream {
			s.transformResponse(t.Context(), w, resp, rec, "anthropic", true)
		} else {
			s.serveNonStreaming(t.Context(), w, resp, httptest.NewRequest("POST", "http://proxy.example/", nil), &target{format: "anthropic"}, "", nil, rec, "", scheduler.WaiterHooks{})
		}
		if rec.ErrorCode != metrics.CodeInvalidUsage || rec.Usage != (metrics.Usage{}) || rec.GenTokens != 0 || settleTokens(rec) != scheduler.TokensUnavailable {
			t.Fatalf("stream=%v invalid usage not unavailable: %+v", stream, rec)
		}
	}
}
