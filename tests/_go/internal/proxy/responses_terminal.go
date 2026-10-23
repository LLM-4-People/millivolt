package proxy

// N5 regression: a Responses turn whose only function_call appears in the
// terminal document is a real tool turn. It must relay the provider terminal
// event instead of a synthetic response.failed substitution.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesTerminalOnlyFunctionCallRelays(t *testing.T) {
	upstream := streamUpstream(t,
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"lookup\",\"arguments\":\"{}\"}]}}\n\n",
	)
	defer upstream.Close()
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
		`{"model":"m","stream":true,"input":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, got)
	}
	if !strings.Contains(got, "event: response.completed") || !strings.Contains(got, `"type":"function_call"`) {
		t.Fatalf("terminal function call was not relayed: %s", got)
	}
	if strings.Contains(got, "response.failed") || strings.Contains(got, "empty_completion") {
		t.Fatalf("legitimate tool turn was replaced with a failure: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.StatusCode != http.StatusOK || rec.ErrorType != "" || rec.ErrorCode != "" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.FinishReason != "tool_calls" || rec.ToolCalls != 1 {
		t.Fatalf("record finish/tools = %q/%d, want tool_calls/1", rec.FinishReason, rec.ToolCalls)
	}
	if len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
		t.Fatalf("record tool names = %v", rec.ToolNames)
	}
}
