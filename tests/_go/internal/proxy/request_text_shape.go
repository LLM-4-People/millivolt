package proxy

// Round 34 NEW-1/NEW-2 regressions: a malformed native Responses `text`
// member must not erase the rest of the request record, and an escaped include
// option decodes to the same logprob opt-in the codec sees.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// responsesTextShapeRequest is a native Responses body that exercises every
// recorded parameter the whole-record fallback used to lose. `text` is the only
// malformed member.
func responsesTextShapeRequest(text string) string {
	return `{"model":"m","stream":false,"max_output_tokens":64,"temperature":0.7,` +
		`"top_p":0.9,"top_logprobs":2,` +
		`"tools":[{"type":"function","name":"lookup","parameters":{}}],` +
		`"metadata":{"k":"v"},"text":` + text + `,` +
		`"input":[{"role":"system","content":[{"type":"input_text","text":"SYS"}]},` +
		`{"role":"user","content":[{"type":"input_text","text":"hello there"}]}]}`
}

func TestResponsesMalformedTextKeepsRecord(t *testing.T) {
	upstreamBody := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`

	shapes := []struct {
		name  string
		value string
	}{
		{name: "string", value: `"oops"`},
		{name: "number", value: `42`},
		{name: "array", value: `["oops"]`},
		{name: "object control", value: `{"format":{"type":"json_object"},"verbosity":"low"}`},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, upstreamBody)
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			body := responsesTextShapeRequest(shape.value)
			w := proxyRequest(t, s, "/v1/responses", "", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if w.Body.String() != upstreamBody {
				t.Fatalf("native body was not relayed verbatim: %s", w.Body.String())
			}
			rec := waitForRecord(t, buf, 1)[0]
			if rec.Model != "m" || rec.Stream {
				t.Fatalf("routing = model %q stream %v, want m/false", rec.Model, rec.Stream)
			}
			if rec.ReqTemperature == nil || *rec.ReqTemperature != 0.7 {
				t.Fatalf("temperature = %v, want 0.7", rec.ReqTemperature)
			}
			if rec.ReqTopP == nil || *rec.ReqTopP != 0.9 {
				t.Fatalf("top_p = %v, want 0.9", rec.ReqTopP)
			}
			if rec.ReqToolsCount != 1 || rec.ReqMetadataKeys != 1 {
				t.Fatalf("tools/metadata = %d/%d, want 1/1", rec.ReqToolsCount, rec.ReqMetadataKeys)
			}
			if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != 64 {
				t.Fatalf("max output tokens = %v, want 64", rec.ReqMaxTokens)
			}
			if rec.ReqTopLogprobs == nil || *rec.ReqTopLogprobs != 2 {
				t.Fatalf("top_logprobs = %v, want 2", rec.ReqTopLogprobs)
			}
			if got := estimateTokens(rec); got == 0 {
				t.Fatal("estimateTokens = 0; the record no longer reserves the output cap")
			}
			if shape.name == "object control" {
				if rec.ReqResponseFormat != "json_object" || rec.ReqVerbosity != "low" {
					t.Fatalf("control metadata = format %q verbosity %q", rec.ReqResponseFormat, rec.ReqVerbosity)
				}
			} else if rec.ReqResponseFormat != "" || rec.ReqVerbosity != "" {
				t.Fatalf("malformed text recorded metadata: format %q verbosity %q", rec.ReqResponseFormat, rec.ReqVerbosity)
			}
		})
	}

	// The Chat path relays the same unknown member and must record identically,
	// including the prompt composition the fallback also erased.
	chatBody := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	chatUp := newFixtureUpstream(t, "application/json", http.StatusOK, chatBody)
	chatBuf := metrics.NewBuffer(10)
	chatSrv := New(config.Default(), chatBuf)
	chatSrv.client.Store(&http.Client{Transport: rewriteTransport{target: chatUp.server.URL}})
	chatRequest := `{"model":"m","max_tokens":64,"temperature":0.7,` +
		`"tools":[{"type":"function","function":{"name":"lookup","parameters":{}}}],` +
		`"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"hello there"}],` +
		`"text":"oops"}`
	w := proxyRequest(t, chatSrv, "/v1/chat/completions", "", chatRequest)
	if w.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	rec := waitForRecord(t, chatBuf, 1)[0]
	if rec.ReqTemperature == nil || *rec.ReqTemperature != 0.7 || rec.ReqToolsCount != 1 {
		t.Fatalf("chat passthrough record = temperature %v tools %d", rec.ReqTemperature, rec.ReqToolsCount)
	}
	if rec.TurnsUser != 1 || rec.CharsSystem != len("SYS") || rec.CharsUser != len("hello there") {
		t.Fatalf("chat composition = turns %d system chars %d user chars %d, want 1/%d/%d",
			rec.TurnsUser, rec.CharsSystem, rec.CharsUser, len("SYS"), len("hello there"))
	}
	if got := estimateTokens(rec); got == 0 {
		t.Fatal("chat estimateTokens = 0; the record no longer reserves the prompt and output cap")
	}
}
