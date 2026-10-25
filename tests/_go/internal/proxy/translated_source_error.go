package proxy

// Round 6 F3 regression: a translated stream record is filled from the source
// analyzer, but the source error class was not copied. The client-wire
// analyzer sees the target encoder's normalized type/code, so a Chat
// server_error became provider_error on Responses and api_error on Anthropic,
// and a truncated source lost the canonical upstream_error/truncated pair.

import (
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const chatSourceErrorStream = `data: {"error":{"type":"server_error","code":"overloaded","message":"boom"}}

`

const chatSourceTruncatedStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}

`

const chatNullToolArgsStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"null"}}]},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

func TestTranslatedStreamRecordKeepsSourceErrorClass(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		format     string
		body       string
		upstream   string
		wantType   string
		wantCode   string
		wantMsg    string
		wantStatus int
	}{
		{
			name:       "chat error to responses",
			path:       "/v1/responses",
			format:     "openai",
			body:       `{"model":"m","input":"hi","stream":true}`,
			upstream:   chatSourceErrorStream,
			wantType:   "server_error",
			wantCode:   "overloaded",
			wantMsg:    "boom",
			wantStatus: http.StatusOK,
		},
		{
			name:       "chat error to anthropic",
			path:       "/v1/messages",
			format:     "openai",
			body:       `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			upstream:   chatSourceErrorStream,
			wantType:   "server_error",
			wantCode:   "overloaded",
			wantMsg:    "boom",
			wantStatus: http.StatusOK,
		},
		{
			name:       "truncated chat to anthropic",
			path:       "/v1/messages",
			format:     "openai",
			body:       `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			upstream:   chatSourceTruncatedStream,
			wantType:   "upstream_error",
			wantCode:   "truncated",
			wantMsg:    metrics.DegenerateMessage(metrics.CodeTruncated),
			wantStatus: http.StatusOK,
		},
		{
			name:       "translator failure stays a translation failure",
			path:       "/v1/messages",
			format:     "openai",
			body:       `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			upstream:   chatNullToolArgsStream,
			wantType:   "stream_read_error",
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, tc.upstream)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != tc.wantType || rec.ErrorCode != tc.wantCode {
				t.Fatalf("record error = %q/%q, want %q/%q (record %+v)",
					rec.ErrorType, rec.ErrorCode, tc.wantType, tc.wantCode, rec)
			}
			if tc.wantMsg != "" && rec.ErrorMsg != tc.wantMsg {
				t.Fatalf("record message = %q, want %q", rec.ErrorMsg, tc.wantMsg)
			}
			if !rec.IsError() {
				t.Fatalf("record is not an error: %+v", rec)
			}
		})
	}
}
