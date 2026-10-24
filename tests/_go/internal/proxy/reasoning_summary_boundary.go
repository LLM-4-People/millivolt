package proxy

// Round 7 NEW-2 regression: a pretty-printed reasoning summary part is
// classified at the SSE event boundary, so the relay's degenerate replacement
// names reasoning_only instead of empty_completion.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

const prettyPrintedSummaryPartStream = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.reasoning_summary_part.added
data: {
data:   "type": "response.reasoning_summary_part.added",
data:   "output_index": 0,
data:   "summary_index": 0,
data:   "part": {
data:     "type": "summary_text",
data:     "text": "step"
data:   }
data: }

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`

func TestPrettyPrintedReasoningSummaryPartIsReasoningOnly(t *testing.T) {
	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, prettyPrintedSummaryPartStream)
	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","stream":true,"input":"hi"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200 (body %s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("missing reasoning_only failure: %s", body)
	}
	if strings.Contains(body, `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("pretty-printed summary part classified as empty: %s", body)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ErrorType != sse.TypeUpstreamError || rec.ErrorCode != metrics.CodeReasoningOnly || !rec.IsError() {
		t.Fatalf("record = type %q code %q error %v, want upstream_error/reasoning_only", rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
}

// The degenerate Responses substitution is proxy-minted. Its record must say
// upstream_error, matching the Chat substitution, rather than attributing the
// proxy's verdict to the provider; a provider-sent standalone error keeps its
// provider_error classification (pinned by the C1 partial-argument test).
func TestResponsesDegenerateSubstitutionRecordsUpstreamError(t *testing.T) {
	rec := &metrics.Record{ID: "req_1"}
	var a sse.Analyzer
	w := httptest.NewRecorder()
	emitDegenerateResponses(w, &a, time.Now(), metrics.CodeReasoningOnly, rec)
	a.Fill(rec)
	if rec.ErrorType != sse.TypeUpstreamError || rec.ErrorCode != metrics.CodeReasoningOnly || !rec.IsError() {
		t.Fatalf("record = type %q code %q error %v, want upstream_error/reasoning_only",
			rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
}
