package proxy

// Round 6 A5 proxy regression: an overlong metadata.user_id fails at the
// translation owner before any upstream send.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicOverlongUserIDRejectedBeforeSend(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK,
		`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	post := func(user string) *httptest.ResponseRecorder {
		body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"user":"` + user + `"}`
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
		r.Header.Set("X-Proxy-Format", "anthropic")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}

	if w := post(strings.Repeat("a", 513)); w.Code != http.StatusBadRequest {
		t.Fatalf("513-character user id status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("rejected user id reached the upstream %d times", up.calls.Load())
	}
	if w := post(strings.Repeat("a", 512)); w.Code != http.StatusOK {
		t.Fatalf("512-character user id status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("accepted user id upstream calls = %d, want 1", up.calls.Load())
	}
}
