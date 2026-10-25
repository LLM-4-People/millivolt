package proxy

// Round 7 verifier NEW-1 and round 8 H1 regression: a failed Responses terminal
// keeps its provider error when it carries partial tool arguments, because the
// unparseable call is dropped. An incomplete terminal is a normal turn boundary
// and now fails closed: the client receives translation_error and the record
// keeps the stream_read_error classification rather than an invented finish.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const responsesNonCleanCreated = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}

`

const responsesFailedPartialTerminal = responsesNonCleanCreated + `event: response.failed
data: {"type":"response.failed","response":{"id":"resp_1","model":"m","status":"failed","error":{"type":"server_error","code":"overloaded","message":"boom"},"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":"}]}}

`

const responsesIncompletePartialTerminal = responsesNonCleanCreated + `event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":"}]}}

`

const responsesIncompleteBufferedPartial = responsesNonCleanCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":"}}

event: response.incomplete
data: {"type":"response.incomplete","response":{"id":"resp_1","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}

`

func TestResponsesNonCleanTerminalPartialArgumentsRecord(t *testing.T) {
	for _, tc := range []struct {
		name        string
		source      string
		wantErrType string
		wantErrCode string
		wantErrMsg  string
		wantFinish  string
		wantError   bool
		// wantTranslationError marks the terminal classes that validate tool
		// arguments strictly and therefore fail the stream.
		wantTranslationError bool
	}{
		{
			name:        "failed terminal partial",
			source:      responsesFailedPartialTerminal,
			wantErrType: "server_error",
			wantErrCode: "overloaded",
			wantErrMsg:  "boom",
			wantError:   true,
		},
		{
			name:                 "incomplete terminal partial",
			source:               responsesIncompletePartialTerminal,
			wantErrType:          "stream_read_error",
			wantErrMsg:           "tool call arguments must be valid JSON",
			wantError:            true,
			wantTranslationError: true,
		},
		{
			name:                 "incomplete buffered added partial",
			source:               responsesIncompleteBufferedPartial,
			wantErrType:          "stream_read_error",
			wantErrMsg:           "tool call arguments must be valid JSON",
			wantError:            true,
			wantTranslationError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, tc.source)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.MaxRetries = 0
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			got := w.Body.String()
			if tc.wantTranslationError {
				if !strings.Contains(got, `"code":"translation_error"`) {
					t.Fatalf("strict terminal did not close with translation_error: %s", got)
				}
				if strings.Contains(got, `"arguments":"{}"`) {
					t.Fatalf("partial call closed with an executable empty object: %s", got)
				}
			} else if strings.Contains(got, "translation_error") {
				t.Fatalf("partial arguments became a translator failure: %s", got)
			}
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType == "stream_read_error" && !tc.wantTranslationError {
				t.Fatalf("record kept the translator failure class: %+v", rec)
			}
			if rec.ErrorType != tc.wantErrType || rec.ErrorCode != tc.wantErrCode ||
				(tc.wantErrMsg != "" && !strings.Contains(rec.ErrorMsg, tc.wantErrMsg)) {
				t.Fatalf("record = type %q code %q msg %q, want %q/%q/%q",
					rec.ErrorType, rec.ErrorCode, rec.ErrorMsg, tc.wantErrType, tc.wantErrCode, tc.wantErrMsg)
			}
			if rec.IsError() != tc.wantError {
				t.Fatalf("record IsError = %v, want %v (%+v)", rec.IsError(), tc.wantError, rec)
			}
			if rec.FinishReason != tc.wantFinish {
				t.Fatalf("record finish = %q, want %q", rec.FinishReason, tc.wantFinish)
			}
			if tc.wantFinish != "" && !strings.Contains(got, `"finish_reason":"`+tc.wantFinish+`"`) {
				t.Fatalf("client lost the %s finish: %s", tc.wantFinish, got)
			}
		})
	}
}
