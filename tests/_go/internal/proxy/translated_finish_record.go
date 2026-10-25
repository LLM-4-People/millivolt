package proxy

// Round 3 batch 3 regression (B1) and round 4 batch 14 strength (H-U1): the
// record keeps the canonical Anthropic reason even though the Chat wire mapper
// approximates it. ir.FinishPause is a synthetic mapper case: no decoder
// produces it. The parent failure was that mapper, while raw provider reasons
// (for example pause_turn) are echoed on every surface; a Chat client's record
// deliberately keeps the valid Chat enum its wire received. The end-to-end
// cases below cover pause_turn on the provider path and the Chat projection.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestTranslatedRecordKeepsRawFinish(t *testing.T) {
	for _, reason := range []ir.FinishReason{ir.FinishPause, "refusal", ir.FinishError} {
		resp := &ir.Response{ID: "r1", Model: "m", Finish: reason}
		rec := &metrics.Record{}
		applyTranslatedStructure(rec, resp, false)
		if rec.FinishReason != string(reason) {
			t.Fatalf("record finish = %q, want raw %q", rec.FinishReason, reason)
		}
	}
}

// TestTranslatedFinishSplit pins the two vocabularies together: the record
// keeps the source spelling while the Chat wire receives a valid enum.
func TestTranslatedFinishSplit(t *testing.T) {
	for _, tc := range []struct {
		reason ir.FinishReason
		wire   string
	}{
		{ir.FinishPause, "stop"},
		{"refusal", "content_filter"},
		{ir.FinishError, "stop"},
	} {
		rec := &metrics.Record{}
		applyTranslatedStructure(rec, &ir.Response{Finish: tc.reason}, false)
		if rec.FinishReason != string(tc.reason) {
			t.Fatalf("record finish = %q, want raw %q", rec.FinishReason, tc.reason)
		}
		if got := format.ChatFinishReason(tc.reason); got != tc.wire {
			t.Fatalf("Chat wire finish for %q = %q, want %q", tc.reason, got, tc.wire)
		}
	}
}

// TestAnthropicPauseTurnEndToEnd exercises the provider path: the raw
// pause_turn reaches a Responses client's record, while the Chat wire maps it to
// stop and the Chat client's record keeps that wire projection.
func TestAnthropicPauseTurnEndToEnd(t *testing.T) {
	const response = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"pause_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	for _, tc := range []struct {
		name       string
		path       string
		format     string
		body       string
		wantWire   string
		wantFinish string
	}{
		{
			name:       "chat client projects",
			path:       "/v1/chat/completions",
			format:     "anthropic",
			body:       `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			wantWire:   `"finish_reason":"stop"`,
			wantFinish: "stop",
		},
		{
			name:       "responses client keeps raw",
			path:       "/v1/responses",
			format:     "anthropic",
			body:       `{"model":"m","input":"hi"}`,
			wantWire:   `"status":"incomplete"`,
			wantFinish: "pause_turn",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, response)
			buf := metrics.NewBuffer(8)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.wantWire) {
				t.Fatalf("wire does not contain %s: %s", tc.wantWire, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].FinishReason != tc.wantFinish {
				t.Fatalf("record finish = %v, want %q", recordFinishReasons(recs), tc.wantFinish)
			}
		})
	}
}
