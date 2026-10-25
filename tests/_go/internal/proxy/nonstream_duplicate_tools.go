package proxy

// Round 2 B4 proxy regression: a non-streaming translated response with two
// target tool calls sharing one id must fail with an error before the 200 is
// committed, never reach the client as a successful body with duplicate ids.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestNonStreamDuplicateTargetToolIDsFailBeforeCommit(t *testing.T) {
	const body = `{"id":"resp_1","model":"m","status":"completed","output":[` +
		`{"type":"function_call","id":"fc_1","call_id":"dup","name":"first","arguments":"{}"},` +
		`{"type":"function_call","id":"fc_2","call_id":"dup","name":"second","arguments":"{}"}]}`
	upstream := translatedJSONUpstream(t, body)
	req, err := http.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Auth-Header", "Authorization")
	req.Header.Set("X-Proxy-Auth-Prefix", "Bearer ")
	req.Header.Set("X-Proxy-Path", "/responses")
	req.Header.Set("X-Proxy-Format", "openai-responses")

	buf := metrics.NewBuffer(4)
	p := New(config.Default(), buf)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 before a duplicate-id success body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "duplicate tool call id") {
		t.Fatalf("failure body = %s, want the duplicate tool call id reason", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"id":"dup"`) {
		t.Fatalf("failure body retained a successful duplicate tool call: %s", rec.Body.String())
	}
}
