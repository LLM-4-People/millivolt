package proxy

// Round 7 C1 regression: a Responses stream that carries a partial function
// call in output_item.added and then ends without a clean terminal must keep
// its provider/truncation classification. The buffered initial arguments are
// incomplete on response.failed, a standalone error event, EOF and the
// truncation flush, so they must be dropped without parsing or emitting them.
// A clean response.completed still flushes a complete buffered object.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const responsesPartialInitialPrefix = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":"}}

`

const responsesCompleteInitialPrefix = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}}

`

const responsesCompletedTerminal = `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`

func TestResponsesPartialInitialArgumentsKeepTerminalClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		source   string
		wantType string
		wantCode string
		wantMsg  string
		frame    string
	}{
		{
			name:     "eof is a truncation",
			source:   responsesPartialInitialPrefix,
			wantType: "upstream_error",
			wantCode: metrics.CodeTruncated,
			wantMsg:  metrics.DegenerateMessage(metrics.CodeTruncated),
			frame:    `"code":"` + metrics.CodeTruncated + `"`,
		},
		{
			name: "response.failed keeps the provider error",
			source: responsesPartialInitialPrefix + `event: response.failed
data: {"type":"response.failed","response":{"id":"resp_1","model":"m","status":"failed","error":{"type":"server_error","code":"overloaded","message":"boom"}}}

`,
			wantType: "server_error",
			wantCode: "overloaded",
			wantMsg:  "boom",
			frame:    `"code":"overloaded"`,
		},
		{
			name: "standalone error keeps the provider error",
			source: responsesPartialInitialPrefix + `event: error
data: {"type":"error","code":"overloaded","message":"busy"}

`,
			wantType: "provider_error",
			wantCode: "overloaded",
			wantMsg:  "busy",
			frame:    `"code":"overloaded"`,
		},
		{
			name:     "clean terminal flushes a complete object",
			source:   responsesCompleteInitialPrefix + responsesCompletedTerminal,
			frame:    `"arguments":"{\"q\":1}"`,
			wantType: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, tc.source)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want the committed 200 (body %s)", w.Code, w.Body.String())
			}
			got := w.Body.String()
			if strings.Contains(got, "translation_error") {
				t.Fatalf("partial initial arguments became a translator failure: %s", got)
			}
			if !strings.Contains(got, "[DONE]") {
				t.Fatalf("client stream has no terminal sentinel: %s", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != tc.wantType || rec.ErrorCode != tc.wantCode || rec.ErrorMsg != tc.wantMsg {
				t.Fatalf("record = type %q code %q msg %q, want %q/%q/%q",
					rec.ErrorType, rec.ErrorCode, rec.ErrorMsg, tc.wantType, tc.wantCode, tc.wantMsg)
			}
			if tc.wantType != "" {
				if !rec.IsError() {
					t.Fatalf("record is not an error: %+v", rec)
				}
				if !strings.Contains(got, `"error"`) {
					t.Fatalf("client received no in-band error: %s", got)
				}
				if !strings.Contains(got, tc.frame) {
					t.Fatalf("client error frame lacks %s: %s", tc.frame, got)
				}
			}
			if tc.name == "clean terminal flushes a complete object" {
				if !strings.Contains(got, tc.frame) {
					t.Fatalf("clean terminal dropped the buffered arguments: %s", got)
				}
				if rec.IsError() {
					t.Fatalf("clean terminal recorded an error: %+v", rec)
				}
			}
			if up.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
			}
		})
	}
}
