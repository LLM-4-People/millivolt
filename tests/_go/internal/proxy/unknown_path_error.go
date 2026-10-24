package proxy

// Round 6 D3 regression: an unknown path has no inbound protocol. The
// pre-commit error sink fell back to errJSONCode because EncodeError rejected
// the empty protocol, and that fallback omits the schema-required param field.
// Chat and unknown paths must keep the full OpenAI envelope.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func openAIErrorFields(t *testing.T, body string) map[string]any {
	t.Helper()
	var envelope struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode error envelope %q: %v", body, err)
	}
	if envelope.Error == nil {
		t.Fatalf("body %q has no error object", body)
	}
	return envelope.Error
}

func TestUnknownPathErrorHasParam(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})

	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/embeddings",
		strings.NewReader(`{"model":"m","input":"hi"}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unknown path status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	fields := openAIErrorFields(t, w.Body.String())
	if param, ok := fields["param"]; !ok || param != nil {
		t.Fatalf("unknown-path error lacks param:null: %v", fields)
	}
	if code, ok := fields["code"]; !ok || code != nil {
		t.Fatalf("unknown-path error code = %v (present %v), want null", code, ok)
	}
}

func TestUnknownPathLimitErrorHasParam(t *testing.T) {
	w := httptest.NewRecorder()
	writeProtocolHTTPError(w, ir.Protocol(""), http.StatusServiceUnavailable,
		"rate_limit_error", "too many in-flight requests")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	fields := openAIErrorFields(t, w.Body.String())
	if param, ok := fields["param"]; !ok || param != nil {
		t.Fatalf("unknown-path limit error lacks param:null: %v", fields)
	}
}
