package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// TestParseRetryAfterClamped pins the hostile-hint contract of
// parseRetryAfter: every parsed path must store a value in
// [0, 24h] (scheduler.MaxRetryHint). Before the clamp, the seconds path
// silently wrapped (a hint >= 9223372037s stored a garbage negative that
// evaded the scheduler's positive-only pathological-hint clamp), the float
// fallback converted NaN/Inf/huge values with implementation-defined
// results, and a past HTTP-date stored a negative delay. Stored hints land
// in the record's RetryAfterMs (the dashboard drawer renders them) and feed
// BackoffFor, so garbage must never get stored. The 24h ceiling is pinned
// literally: silent drift of scheduler.MaxRetryHint must fail here.
func TestParseRetryAfterClamped(t *testing.T) {
	past := time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat)
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{"seconds product wraps negative pre-clamp", map[string]string{"Retry-After": "9223372037"}, 24 * time.Hour},
		{"int64-max seconds stores -1s pre-clamp", map[string]string{"Retry-After": "9223372036854775807"}, 24 * time.Hour},
		{"huge float hint caps to the ceiling", map[string]string{"X-Ratelimit-Reset-Requests": "1e30"}, 24 * time.Hour},
		{"NaN hint is malformed, so no hint", map[string]string{"X-Ratelimit-Reset-Requests": "NaN"}, 0},
		{"past HTTP-date floors to retry-now", map[string]string{"Retry-After": past}, 0},
		{"one second past the ceiling caps to it", map[string]string{"Retry-After": "86401"}, 24 * time.Hour},
		{"exactly the ceiling stays exact", map[string]string{"Retry-After": "86400"}, 24 * time.Hour},
		{"plain seconds parse exactly", map[string]string{"Retry-After": "2"}, 2 * time.Second},
		{"duration form parses exactly", map[string]string{"X-Ratelimit-Reset-Requests": "500ms"}, 500 * time.Millisecond},
		{"malformed value means no hint", map[string]string{"Retry-After": "abc"}, 0},
		{"Retry-After wins over a garbage reset header", map[string]string{"Retry-After": "2", "X-Ratelimit-Reset-Requests": "1e30"}, 2 * time.Second},
	}
	for _, tc := range cases {
		resp := &http.Response{Header: http.Header{}}
		for k, v := range tc.headers {
			resp.Header.Set(k, v)
		}
		if got := parseRetryAfter(resp); got != tc.want {
			t.Errorf("%s: parseRetryAfter = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestMaxRequestOutputTokensBound pins the trust-boundary rejection of a
// hostile client token cap. The decoded max_tokens feeds estimateTokens at
// Acquire time with no range check anywhere: MaxInt64 wraps the estimate
// negative so the scheduler's need <= 0 branch skips the reservation, and a
// huge finite value exceeds any configured capacity so the need > capacity
// branch admits the request and debits the provider-wide token bucket by
// the attacker-chosen amount, token-blocking every other client for the
// in-flight window. max_tokens and max_completion_tokens both decode into
// the same ReqMaxTokens (preferring max_completion_tokens, like the
// Anthropic translator), so one bound rejects both spellings - including a
// both-fields body whose max_completion_tokens the translator would have
// carried into the translated re-decode past a max_tokens-only check.
// The adversarial rows pin the split-decode bypass: a decoy type error in
// a field the strict parameter decode rejects (n as a string) leaves the
// original record's ReqMaxTokens nil (parseLLMRequest's split-decode early
// return), while the Anthropic translator's struct ignores the unknown
// field and still copies the hostile cap into the translated body, which
// re-decodes through parseLLMRequest into a fresh record. Before the
// second check, that fresh record reached estimateTokens and the upstream
// with no bound re-applied, and every row below returned 200.
func TestMaxRequestOutputTokensBound(t *testing.T) {
	var calls atomic.Int32
	var upstreamBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		upstreamBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	post := func(payload, format string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer sk-test-key")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("Content-Type", "application/json")
		if format != "" {
			req.Header.Set("X-Proxy-Format", format)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}

	for _, tc := range []struct {
		payload string
		format  string
	}{
		{`{"model":"m","max_tokens":1000001}`, ""},
		{`{"model":"m","max_completion_tokens":1000001}`, ""},
		{`{"model":"m","max_tokens":1000000,"max_completion_tokens":1000001}`, "anthropic"},
		// Split-decode bypass shape: the decoy "n":"x" type error suppresses
		// the original record's ReqMaxTokens while the Anthropic translator
		// tolerates it and carries the hostile cap into the re-decoded body.
		{`{"model":"m","max_tokens":2000000000,"n":"x"}`, "anthropic"},
		{`{"model":"m","max_completion_tokens":9999999999999,"n":"x"}`, "anthropic"},
	} {
		status, respBody := post(tc.payload, tc.format)
		if status != http.StatusBadRequest {
			t.Errorf("%s (format %q): status = %d, want 400", tc.payload, tc.format, status)
		}
		if !strings.Contains(respBody, "invalid_request_error") {
			t.Errorf("%s (format %q): response %q lacks invalid_request_error", tc.payload, tc.format, respBody)
		}
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0 (the hostile cap must be rejected at the boundary)", got)
	}

	// Control: exactly the ceiling proxies with the body unchanged.
	payload := `{"model":"m","max_tokens":1000000}`
	if status, _ := post(payload, ""); status != http.StatusOK {
		t.Errorf("in-range cap: status = %d, want 200", status)
	}
	if upstreamBody != payload {
		t.Errorf("in-range body mutated:\n got %q\nwant %q", upstreamBody, payload)
	}
}

// TestAnthropicDefaultMaxTokensCeilingMatchesProxyBound pins the documented
// mirror between internal/config's anthropic_default_max_tokens validation
// ceiling and maxRequestOutputTokens (the request.go constant's comment
// claims they are the same ceiling). The load-bearing direction: the
// Anthropic translator injects the configured default into translated
// bodies, which re-decode into a fresh record the proxy bound checks, so a
// config ceiling above maxRequestOutputTokens would let a validated setting
// produce requests the trust boundary then rejects. The equality is asserted
// through the validation seam (config.Default plus Validate), the same path
// every config load and save goes through.
func TestAnthropicDefaultMaxTokensCeilingMatchesProxyBound(t *testing.T) {
	cfg := config.Default()
	cfg.AnthropicDefaultMaxTokens = maxRequestOutputTokens
	if err := cfg.Validate(); err != nil {
		t.Fatalf("anthropic_default_max_tokens = %d must validate: %v", maxRequestOutputTokens, err)
	}
	cfg.AnthropicDefaultMaxTokens = maxRequestOutputTokens + 1
	if err := cfg.Validate(); err == nil {
		t.Fatalf("anthropic_default_max_tokens = %d validates above the proxy bound (%d)",
			cfg.AnthropicDefaultMaxTokens, maxRequestOutputTokens)
	}
}

// TestMaxLimitWindowMirrorsSchedulerRetryHint pins the throttle.go comment
// that maxLimitWindow is the same ceiling as scheduler.MaxRetryHint (a
// daily quota window). Both bounds are deliberately in their owning
// packages (the proxy caps header/UI-supplied limits, the scheduler clamps
// Retry-After hints), so this in-package test is the one place that can
// pin their equality; silent drift of either constant must fail here.
func TestMaxLimitWindowMirrorsSchedulerRetryHint(t *testing.T) {
	if maxLimitWindow != scheduler.MaxRetryHint {
		t.Fatalf("maxLimitWindow = %v, want scheduler.MaxRetryHint (%v)",
			maxLimitWindow, scheduler.MaxRetryHint)
	}
}
