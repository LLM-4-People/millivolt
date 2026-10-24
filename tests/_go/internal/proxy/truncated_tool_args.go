package proxy

// Round 12 NEW-3 regression: a Chat stream that truncates mid-tool-arguments is
// an upstream truncation, not a translator failure. FlushBlocks closes the open
// block without validating the incomplete value because the canonical truncated
// error already refuses a clean terminal. The record must be
// upstream_error/truncated, not stream_read_error.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const truncatedChatToolArgsStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}

`

func TestTruncatedChatToolArgumentsKeepTruncationClass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		request  string
		wantCode bool
	}{
		{name: "anthropic target", path: "/v1/messages",
			request: `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{name: "responses target", path: "/v1/responses",
			request: `{"model":"m","input":"hi","stream":true}`, wantCode: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, truncatedChatToolArgsStream)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, "openai", tc.request)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want the committed 200 (body %s)", w.Code, w.Body.String())
			}
			got := w.Body.String()
			if !strings.Contains(got, "event: error") {
				t.Fatalf("client received no in-band truncation error: %s", got)
			}
			if strings.Contains(got, "[DONE]") {
				t.Fatalf("truncated Chat source leaked a [DONE] to the target: %s", got)
			}
			if tc.wantCode {
				if strings.Contains(got, "event: response.completed") {
					t.Fatalf("truncated arguments produced a completed Responses turn: %s", got)
				}
				if !strings.Contains(got, "event: response.failed") ||
					!strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
					t.Fatalf("Responses client lost the canonical truncated pair: %s", got)
				}
			} else if strings.Contains(got, "event: message_stop") && !strings.Contains(got, "event: error") {
				t.Fatalf("Anthropic client received a clean terminal without an error: %s", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != "upstream_error" || rec.ErrorCode != metrics.CodeTruncated || !rec.IsError() {
				t.Fatalf("record = type %q code %q error %v, want upstream_error/truncated",
					rec.ErrorType, rec.ErrorCode, rec.IsError())
			}
			if up.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
			}
		})
	}
}
