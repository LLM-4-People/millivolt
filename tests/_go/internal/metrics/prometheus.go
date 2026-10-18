package metrics

import (
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
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

// TestHandlePrometheusExpositionParity pins the complete exposition byte for
// byte across the mixed shape the aggregate must fold in one pass: multiple
// providers, a 429 (flow control, not an error), a 500 (a real error),
// zero-TTFT records that still count toward provider totals, and a sparse
// TTFT set whose nearest-rank percentiles repeat a value.
func TestHandlePrometheusExpositionParity(t *testing.T) {
	buf := NewBuffer(16)
	for _, r := range []*Record{
		{
			Provider:   "alpha",
			StatusCode: 200,
			TTFTMs:     100,
			Usage:      Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 5},
			ToolCalls:  1,
		},
		{Provider: "alpha", StatusCode: 429, TTFTMs: 300, ErrorType: "rate_limit_error"},
		{Provider: "beta", StatusCode: 500, TTFTMs: 200, ErrorType: "provider_error"},
		{Provider: "gamma", StatusCode: 200, Usage: Usage{InputTokens: 7}, ToolCalls: 2},
		{Provider: "alpha", StatusCode: 200},
		{Provider: "beta", StatusCode: 200, TTFTMs: 50},
	} {
		buf.Record(r)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	w := httptest.NewRecorder()
	HandlePrometheus(w, req, buf)

	if got, want := w.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := w.Header().Get("Content-Type"), "text/plain; version=0.0.4"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	const want = `# HELP llm_proxy_requests_total Total LLM proxy requests (current ring).
# TYPE llm_proxy_requests_total gauge
llm_proxy_requests_total 6
# HELP llm_proxy_input_tokens_total Total input tokens (current ring).
# TYPE llm_proxy_input_tokens_total gauge
llm_proxy_input_tokens_total 17
# HELP llm_proxy_output_tokens_total Total output tokens (current ring).
# TYPE llm_proxy_output_tokens_total gauge
llm_proxy_output_tokens_total 20
# HELP llm_proxy_cache_read_tokens_total Total cache-read tokens (current ring).
# TYPE llm_proxy_cache_read_tokens_total gauge
llm_proxy_cache_read_tokens_total 5
# HELP llm_proxy_errors_total Total requests with an error (current ring).
# TYPE llm_proxy_errors_total gauge
llm_proxy_errors_total 1
# HELP llm_proxy_tool_calls_total Total tool calls observed (current ring).
# TYPE llm_proxy_tool_calls_total gauge
llm_proxy_tool_calls_total 3
# HELP llm_proxy_requests_by_provider Requests grouped by provider (current ring).
# TYPE llm_proxy_requests_by_provider gauge
llm_proxy_requests_by_provider{provider="alpha"} 3
llm_proxy_requests_by_provider{provider="beta"} 2
llm_proxy_requests_by_provider{provider="gamma"} 1
# HELP llm_proxy_ttft_seconds Time to first token in seconds.
# TYPE llm_proxy_ttft_seconds summary
llm_proxy_ttft_seconds{quantile="0.5"} 0.1
llm_proxy_ttft_seconds{quantile="0.95"} 0.3
llm_proxy_ttft_seconds{quantile="0.99"} 0.3
llm_proxy_ttft_seconds_sum 0.65
llm_proxy_ttft_seconds_count 4
`
	if got := w.Body.String(); got != want {
		t.Errorf("exposition drifted:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestHandlePrometheusTTFTSumOverflow requires the checked flat TTFT sum to
// fail the whole exposition: a wrapped total must never appear behind a 200,
// and the handler must not emit the metric preamble it builds before the sum.
func TestHandlePrometheusTTFTSumOverflow(t *testing.T) {
	buf := NewBuffer(4)
	buf.Record(&Record{Provider: "alpha", StatusCode: 200, TTFTMs: math.MaxInt64})
	buf.Record(&Record{Provider: "alpha", StatusCode: 200, TTFTMs: 2})

	// The aggregate must fail closed: the samples stay complete (the sum is
	// what overflowed), but the sum field never retains a wrapped or partial
	// value and the error is the shared range rejection.
	agg := Aggregate(buf.Snapshot())
	if !errors.Is(agg.Err, ErrMetricRange) {
		t.Fatalf("Aggregate.Err = %v, want ErrMetricRange", agg.Err)
	}
	if agg.TTFTSumMs != 0 {
		t.Errorf("TTFTSumMs = %d, want the fail-closed 0", agg.TTFTSumMs)
	}
	if got, want := agg.TTFTSamples, []int64{math.MaxInt64, 2}; !slices.Equal(got, want) {
		t.Errorf("TTFTSamples = %v, want %v", got, want)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics/prometheus", nil)
	w := httptest.NewRecorder()
	HandlePrometheus(w, req, buf)

	if got, want := w.Code, http.StatusInternalServerError; got != want {
		t.Fatalf("status = %d, want %d\n%s", got, want, w.Body.String())
	}
	if got, want := w.Body.String(), "metrics cannot be represented\n"; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if strings.Contains(w.Body.String(), "llm_proxy_ttft_seconds") {
		t.Errorf("overflow emitted a partial summary:\n%s", w.Body.String())
	}
	if got, want := w.Header().Get("Cache-Control"), "no-store"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}
