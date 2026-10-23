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

// TestResponsesTerminalOnlyOutputRelays pins N9 on the native path: answer,
// refusal and mixed text/tool output that appears only in the terminal
// document is real output, not an empty completion. Reasoning-only output is
// classified as reasoning_only (the canonical auxiliary-output class), never
// empty_completion.
func TestResponsesTerminalOnlyOutputRelays(t *testing.T) {
	cases := []struct {
		name        string
		output      string
		wantBody    string
		wantFinish  string
		wantPreview string
		wantTools   int
		wantCode    string
	}{
		{
			name:        "text",
			output:      `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]`,
			wantBody:    `"text":"hello"`,
			wantFinish:  "stop",
			wantPreview: "hello",
		},
		{
			name:        "refusal",
			output:      `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"refusal","refusal":"no"}]}]`,
			wantBody:    `"refusal":"no"`,
			wantFinish:  "stop",
			wantPreview: "no",
		},
		{
			name: "mixed text and tool",
			output: `[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]},` +
				`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]`,
			wantBody:    `"text":"hello"`,
			wantFinish:  "tool_calls",
			wantPreview: "hello",
			wantTools:   1,
		},
		{
			name:       "reasoning",
			output:     `[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}]`,
			wantFinish: "stop",
			wantCode:   "reasoning_only",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := streamUpstream(t,
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":"+tc.output+"}}\n\n",
			)
			defer upstream.Close()
			cfg := config.Default()
			cfg.CaptureBodyPreview = true
			buf := metrics.NewBuffer(8)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
				`{"model":"m","stream":true,"input":"hi"}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d (body %s)", status, got)
			}
			if tc.wantCode == "" {
				if !strings.Contains(got, "event: response.completed") || !strings.Contains(got, tc.wantBody) {
					t.Fatalf("terminal output was not relayed: %s", got)
				}
				if strings.Contains(got, "response.failed") || strings.Contains(got, "empty_completion") {
					t.Fatalf("legitimate terminal output was replaced with a failure: %s", got)
				}
			} else {
				if !strings.Contains(got, `"code":"`+tc.wantCode+`"`) {
					t.Fatalf("terminal output class = %s, want %s", got, tc.wantCode)
				}
				if strings.Contains(got, "empty_completion") {
					t.Fatalf("terminal output was classified empty_completion: %s", got)
				}
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.FinishReason != tc.wantFinish {
				t.Fatalf("finish = %q, want %q", rec.FinishReason, tc.wantFinish)
			}
			if rec.ResponsePreview != tc.wantPreview {
				t.Fatalf("preview = %q, want %q", rec.ResponsePreview, tc.wantPreview)
			}
			if rec.ToolCalls != tc.wantTools {
				t.Fatalf("tool calls = %d, want %d", rec.ToolCalls, tc.wantTools)
			}
			if tc.wantCode == "" && (rec.ErrorType != "" || rec.ErrorCode != "") {
				t.Fatalf("record error = %q/%q, want none", rec.ErrorType, rec.ErrorCode)
			}
			if tc.wantCode != "" && rec.ErrorCode != tc.wantCode {
				t.Fatalf("record error code = %q, want %q", rec.ErrorCode, tc.wantCode)
			}
		})
	}
}

// TestResponsesStaleTerminalNameNotRelayed pins N9's wire half: a terminal
// event name line with no data must not survive ahead of the synthetic failure
// that replaces the incomplete terminal region.
func TestResponsesStaleTerminalNameNotRelayed(t *testing.T) {
	cases := []struct {
		name string
		tail string
	}{
		{"name then blank", "event: response.completed\n\n"},
		{"name then eof", "event: response.completed\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := streamUpstream(t,
				"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n",
				tc.tail,
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
			if strings.Contains(got, "event: response.completed") {
				t.Fatalf("stale terminal event name survived substitution: %s", got)
			}
			if !strings.Contains(got, "event: response.failed") || !strings.Contains(got, `"code":"truncated"`) {
				t.Fatalf("missing synthetic truncation failure: %s", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].ErrorCode != "truncated" {
				t.Fatalf("records = %+v, want one truncated error", recs)
			}
		})
	}
}
