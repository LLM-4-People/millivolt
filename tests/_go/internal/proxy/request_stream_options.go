package proxy

// A1 regression: the request preflight bounds Responses stream_options before
// the single metadata decode and before the rewrite engine can materialize it.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func streamOptionsObject(n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"k%d":1`, i)
	}
	b.WriteString("}")
	return b.String()
}

func streamOptionsRequest(n int) string {
	return `{"model":"m","stream":true,"input":"hi","stream_options":` + streamOptionsObject(n) + `}`
}

func TestRequestStreamOptionsMetadataBound(t *testing.T) {
	const responsesBody = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	up := newFixtureUpstream(t, "application/json", 200, responsesBody)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "", streamOptionsRequest(10_001))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("10,001-member stream_options status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "request stream_options exceeds 10000 members") {
		t.Fatalf("rejection does not name the stream_options bound: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("over-bound stream_options called upstream %d times, want 0", up.calls.Load())
	}

	up.calls.Store(0)
	w = proxyRequest(t, s, "/v1/responses", "", streamOptionsRequest(10_000))
	if w.Code != http.StatusOK {
		t.Fatalf("10,000-member stream_options status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("at-limit stream_options upstream calls = %d, want 1", up.calls.Load())
	}
}
