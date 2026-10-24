package proxy

// Round 7 H4 regression: an Anthropic provider error that arrives while a tool
// block is open must keep the provider's error class. The decoder closes the
// open block without validating the incomplete input_json_delta, because the
// provider error is the terminal statement; validating there would turn
// overloaded_error into a translation failure.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicOpenToolErrorStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}

event: error
data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}

`

func TestAnthropicProviderErrorWithOpenToolBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat target",
			path: "/v1/chat/completions",
			body: `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses target",
			path: "/v1/responses",
			body: `{"model":"m","input":"hi","stream":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, anthropicOpenToolErrorStream)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.MaxRetries = 0
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, "anthropic", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			got := w.Body.String()
			if !strings.Contains(got, "overloaded_error") || !strings.Contains(got, "busy") {
				t.Fatalf("client lost the provider error: %s", got)
			}
			if strings.Contains(got, "translation_error") {
				t.Fatalf("open tool block turned the provider error into a translation failure: %s", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != "overloaded_error" || !rec.IsError() {
				t.Fatalf("record = type %q error %v, want the provider error class", rec.ErrorType, rec.IsError())
			}
			if rec.ErrorMsg != "busy" {
				t.Fatalf("record message = %q, want busy", rec.ErrorMsg)
			}
		})
	}
}
