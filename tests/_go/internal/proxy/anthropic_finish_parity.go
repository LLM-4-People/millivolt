package proxy

// Round 3 recheck NEW-2 regression: the record keeps the raw Anthropic finish
// reason on every surface. Chat's wire enum still approximates refusal to
// content_filter, but the record vocabulary must not depend on the transport.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicRefusalResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
	`"content":[{"type":"text","text":"no"}],"stop_reason":"refusal",` +
	`"usage":{"input_tokens":1,"output_tokens":1}}`

const anthropicRefusalStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"no"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func anthropicFinishServer(t *testing.T, stream bool) (*fixtureUpstream, *Server, *metrics.Buffer) {
	t.Helper()
	contentType, response := "application/json", anthropicRefusalResponse
	if stream {
		contentType, response = "text/event-stream", anthropicRefusalStream
	}
	up := newFixtureUpstream(t, contentType, http.StatusOK, response)
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	return up, s, buf
}

func TestAnthropicRefusalFinishParity(t *testing.T) {
	t.Run("native non-stream record", func(t *testing.T) {
		_, s, buf := anthropicFinishServer(t, false)
		w := proxyRequest(t, s, "/v1/messages", "", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		recs := waitForRecord(t, buf, 1)
		if len(recs) != 1 || recs[0].FinishReason != "refusal" {
			t.Fatalf("record = %v, want raw refusal", recordFinishReasons(recs))
		}
	})

	t.Run("native stream record", func(t *testing.T) {
		_, s, buf := anthropicFinishServer(t, true)
		w := proxyRequest(t, s, "/v1/messages", "", `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		recs := waitForRecord(t, buf, 1)
		if len(recs) != 1 || recs[0].FinishReason != "refusal" {
			t.Fatalf("record = %v, want raw refusal", recordFinishReasons(recs))
		}
	})

	t.Run("translated Chat wire approximates", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			_, s, buf := anthropicFinishServer(t, stream)
			body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
			if stream {
				body = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
			}
			w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
			if w.Code != http.StatusOK {
				t.Fatalf("stream %v status = %d, want 200 (body %s)", stream, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"finish_reason":"content_filter"`) {
				t.Fatalf("stream %v Chat wire did not approximate refusal: %s", stream, w.Body.String())
			}
			// A Chat client's record is read from its own wire, so it keeps
			// the valid Chat enum the client received.
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].FinishReason != "content_filter" {
				t.Fatalf("stream %v Chat record = %v, want the wire projection", stream, recordFinishReasons(recs))
			}
		}
	})
}

func recordFinishReasons(recs []*metrics.Record) []string {
	reasons := make([]string, 0, len(recs))
	for _, rec := range recs {
		reasons = append(reasons, rec.FinishReason)
	}
	return reasons
}
