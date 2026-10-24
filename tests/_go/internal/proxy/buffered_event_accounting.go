package proxy

// Round 8 R8-1 regression: an oversized terminal-named event is abandoned to
// FeedBufferedEvent and later resolved at its real blank-line boundary. The
// single-line summary-part document is classified by the per-line path already,
// so the boundary pass must not record the same presence a second time. The
// wire stays byte-identical because the abandon path relays verbatim.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAbandonedTerminalNamedSummaryPartCountsOneChunk(t *testing.T) {
	text := strings.Repeat("a", 70*1024)
	stream := "event: response.completed\n" +
		`data: {"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":"` + text + `"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}` + "\n\n"

	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, stream)
	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","input":"hi","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	got := w.Body.String()
	if len(got) != len(stream) {
		t.Fatalf("client bytes = %d, want %d (verbatim relay)", len(got), len(stream))
	}
	for i := 0; i < len(got); i++ {
		if got[i] != stream[i] {
			end := i + 40
			if end > len(got) {
				end = len(got)
			}
			t.Fatalf("client bytes diverge at %d: got %q, want %q", i, got[i:end], stream[i:end])
		}
	}

	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Chunks != 1 {
		t.Fatalf("chunks = %d, want 1: the abandoned single-line summary part was counted twice", rec.Chunks)
	}
	if rec.FirstTokenAt.IsZero() || rec.LastTokenAt.IsZero() {
		t.Fatalf("token timestamps = first %v last %v, want one recorded presence", rec.FirstTokenAt, rec.LastTokenAt)
	}
	if rec.FinishReason != "stop" {
		t.Fatalf("finish = %q, want stop", rec.FinishReason)
	}
	if rec.IsError() || rec.ErrorType != "" || rec.ErrorCode != "" {
		t.Fatalf("record = type %q code %q error %v, want healthy", rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
}
