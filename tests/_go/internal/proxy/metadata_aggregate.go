package proxy

// Round 3 batch 5 regression (A1): duplicate metadata objects merge in
// encoding/json, so the aggregate must count distinct keys across every
// occurrence instead of checking each object alone.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func metadataObject(prefix string, n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s%d":"v"`, prefix, i)
	}
	b.WriteString("}")
	return b.String()
}

func duplicateMetadataBody(objects ...string) string {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[{"role":"user","content":"hi"}]`)
	for _, object := range objects {
		b.WriteString(`,"metadata":`)
		b.WriteString(object)
	}
	b.WriteString("}")
	return b.String()
}

func TestChatDuplicateMetadataAggregateBounded(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	atLimit := duplicateMetadataBody(metadataObject("k", 10_000))
	w := proxyRequest(t, s, "/v1/chat/completions", "", atLimit)
	if w.Code != http.StatusOK {
		t.Fatalf("single 10,000-key metadata status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("at-limit upstream calls = %d, want 1", up.calls.Load())
	}

	up, s = nestedFixtureServer(t)
	over := duplicateMetadataBody(metadataObject("a", 6_000), metadataObject("b", 6_000))
	w = proxyRequest(t, s, "/v1/chat/completions", "", over)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("duplicate metadata status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "request metadata exceeds 10000 members") {
		t.Fatalf("rejection does not name the metadata aggregate: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("duplicate metadata called upstream %d times, want 0", up.calls.Load())
	}

	up, s = nestedFixtureServer(t)
	overOne := duplicateMetadataBody(metadataObject("c", 10_001))
	w = proxyRequest(t, s, "/v1/chat/completions", "", overOne)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("single 10,001-key metadata status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("single over-limit metadata called upstream %d times, want 0", up.calls.Load())
	}
}
