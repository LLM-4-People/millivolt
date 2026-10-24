package proxy

// Round 3 batch 10 regressions (F7): a native Anthropic stream is classified
// from the routed wire before any frame arrives, so an empty or ping-only feed
// is a truncation instead of a healthy unknown-protocol relay.

import (
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func nativeAnthropicStream(t *testing.T, response string) (*fixtureUpstream, *Server, *metrics.Buffer) {
	t.Helper()
	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, response)
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	return up, s, buf
}

func TestNativeAnthropicEmptyStreamTruncated(t *testing.T) {
	_, s, buf := nativeAnthropicStream(t, "")
	w := proxyRequest(t, s, "/v1/messages", "anthropic", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want the committed 200 (body %s)", w.Code, w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ErrorCode != metrics.CodeTruncated || rec.ErrorType == "" || !rec.IsError() {
		t.Fatalf("empty anthropic stream = type %q code %q error %v, want a truncated failure", rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
	// A foreign wire keeps its own terminal contract: the empty upstream
	// body relays nothing, and the truncation lives on the record.
	if w.Body.Len() != 0 {
		t.Fatalf("empty upstream body fabricated client bytes: %q", w.Body.String())
	}
}

func TestNativeAnthropicPingOnlyTruncated(t *testing.T) {
	_, s, buf := nativeAnthropicStream(t, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	w := proxyRequest(t, s, "/v1/messages", "anthropic", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.ErrorCode != metrics.CodeTruncated || rec.ErrorType == "" || !rec.IsError() {
		t.Fatalf("ping-only anthropic stream = type %q code %q error %v, want a truncated failure", rec.ErrorType, rec.ErrorCode, rec.IsError())
	}
}

func TestNativeAnthropicMissingStopTruncated(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"
	_, s, buf := nativeAnthropicStream(t, stream)
	w := proxyRequest(t, s, "/v1/messages", "anthropic", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if rec := recs[0]; rec.ErrorCode != metrics.CodeTruncated || !rec.IsError() {
		t.Fatalf("missing message_stop = type %q code %q, want truncated", rec.ErrorType, rec.ErrorCode)
	}
}
