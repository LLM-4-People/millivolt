package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandlePrometheus(t *testing.T) {
	buf := NewBuffer(100)
	buf.Record(&Record{
		Provider:     "alpha",
		Model:        "model-a",
		StatusCode:   200,
		Start:        time.Now(),
		End:          time.Now().Add(time.Second),
		FirstTokenAt: time.Now().Add(100 * time.Millisecond),
		LastTokenAt:  time.Now().Add(900 * time.Millisecond),
		Usage: Usage{
			InputTokens:     10,
			OutputTokens:    20,
			TotalTokens:     30,
			CacheReadTokens: 5,
		},
		ToolCalls: 1,
	})
	// A genuine upstream failure (500) counts as an error. (A 429 would be flow
	// control and must NOT increment errors_total.)
	buf.Record(&Record{Provider: "beta", Model: "llama", StatusCode: 500, ErrorType: "provider_error"})

	req := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	HandlePrometheus(w, req, buf)

	body := w.Body.String()
	for _, want := range []string{
		"llm_proxy_requests_total 2",
		"llm_proxy_input_tokens_total 10",
		"llm_proxy_output_tokens_total 20",
		"llm_proxy_cache_read_tokens_total 5",
		"llm_proxy_errors_total 1",
		"llm_proxy_tool_calls_total 1",
		`provider="alpha"`,
		`provider="beta"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prometheus output missing %q\n%s", want, body)
		}
	}

	post := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	pw := httptest.NewRecorder()
	HandlePrometheus(pw, post, buf)
	if pw.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics → %d, want 405", pw.Code)
	}
	if pw.Header().Get("Allow") != metricsAllow {
		t.Fatalf("POST /metrics Allow = %q, want %q", pw.Header().Get("Allow"), metricsAllow)
	}
}
