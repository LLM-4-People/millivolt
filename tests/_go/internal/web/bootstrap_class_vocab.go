package web

// The dashboard classifies live rows against the rate-limit vocabulary in
// entities.js. When the operator installs rate_limit_error_classes extensions,
// the bootstrap payload publishes the EFFECTIVE vocabulary so the browser's
// pills and error groups agree with the server's aggregates; with no
// extensions the key stays absent and the payload keeps its pinned shape.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestBootstrapPublishesOperatorRateLimitVocabulary(t *testing.T) {
	buf := metrics.NewBuffer(8)
	agg := NewAggAPI(buf, nil, time.Second)

	// Built-ins alone: the key is absent, so the pinned payload shape (and the
	// browser's static mirror) stands.
	rec := httptest.NewRecorder()
	http.HandlerFunc(agg.HandleBootstrap).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "rate_limit_classes") {
		t.Fatalf("bootstrap with no extensions carries the key: %s", rec.Body.String())
	}

	// Extensions installed: the effective vocabulary is published.
	metrics.InstallClassExtensions(metrics.ClassExtensions{RateLimit: []string{"gateway_throttle"}})
	t.Cleanup(func() { metrics.InstallClassExtensions(metrics.ClassExtensions{}) })

	rec = httptest.NewRecorder()
	http.HandlerFunc(agg.HandleBootstrap).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap", nil))
	var wire struct {
		RateLimitClasses []string `json:"rate_limit_classes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatalf("bootstrap body is not JSON: %v", err)
	}
	want := metrics.RateLimitClassVocabulary()
	if !slices.Equal(wire.RateLimitClasses, want) {
		t.Fatalf("rate_limit_classes = %v, want the effective vocabulary %v", wire.RateLimitClasses, want)
	}
	if !slices.Contains(wire.RateLimitClasses, "gateway_throttle") {
		t.Fatalf("the extension spelling is missing from %v", wire.RateLimitClasses)
	}
}
