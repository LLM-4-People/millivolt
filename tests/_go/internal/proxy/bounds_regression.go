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
	"github.com/LLM-4-People/millivolt/internal/metrics"
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
		// Anthropic-shaped response: the translated rows need a body their
		// response translation accepts; passthrough rows relay it verbatim.
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
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
		// Tools-decoy bypass shape: an out-of-range number inside tool
		// parameters is carried as json.RawMessage by BOTH decoders, so the
		// type error suppresses the original record's ReqMaxTokens AND the
		// translated re-decode's (the early-return fires twice) - both
		// hostileTokenCap checks see nil while the hostile cap still
		// reaches the passthrough/translated upstream body.
		{`{"model":"m","max_tokens":2000000000,"tools":[{"type":"function","function":{"name":"f","parameters":1e400}}]}`, ""},
		{`{"model":"m","max_tokens":2000000000,"tools":[{"type":"function","function":{"name":"f","parameters":1e400}}]}`, "anthropic"},
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

	// Transparency control for the tools decoy: the decoy body is valid JSON
	// whose type error lives outside the cap fields, so a SANE cap still
	// proxies verbatim (translated upstream with format anthropic). The
	// invariant protects forwarding; a recoverable cap must not blind the
	// bound check, and an in-range one must not be rejected.
	decoy := `{"model":"m","max_tokens":100,"tools":[{"type":"function","function":{"name":"f","parameters":1e400}}]}`
	if status, _ := post(decoy, "anthropic"); status != http.StatusOK {
		t.Errorf("in-range cap with tools decoy: status = %d, want 200", status)
	}
}

// TestTranslatedResponseBodyBounded pins the byte cap on the translated
// non-streaming 200 read in serveNonStreaming. Every sibling read is bounded
// (request upload, error-body prefix, quality spool, SSE line scanner), but
// the translated path io.ReadAll'd the whole document with no ceiling: a
// client-chosen base URL (X-Proxy-Base-URL is client-supplied;
// allowed_base_urls only gates it when configured) could point at an upstream
// that streams gigabytes and exhaust proxy memory per request. Past the cap
// the document cannot be translated, and relaying it verbatim would break the
// translated-shape contract, so the read fails closed as 502 (nothing has been
// written to the client yet on that path - the status line is committed only
// after a successful translation). The 32 MiB ceiling (relay.go's
// maxTranslateBodyBytes, the request-side default scale) is pinned literally
// here: silent drift of that constant must fail this test.
func TestTranslatedResponseBodyBounded(t *testing.T) {
	const wantCapBytes = 32 << 20
	// A valid Anthropic 200 body whose single text block pads the framed
	// document past the cap (the JSON framing alone adds >1 byte).
	bigBody := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"` +
		strings.Repeat("x", wantCapBytes) +
		`"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	smallBody := `{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"m","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	var served atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if served.Add(1) == 1 {
			w.Write([]byte(bigBody))
			return
		}
		w.Write([]byte(smallBody))
	}))
	defer upstream.Close()
	buf := metrics.NewBuffer(4)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	post := func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test-key")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Format", "anthropic")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Oversized: fail closed 502, nothing translated or relayed.
	resp := post()
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("oversized translated body: status = %d, want 502 (body: %s)", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream response too large to translate") {
		t.Errorf("502 body = %q, want the too-large message", raw)
	}
	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("recorded %d records, want 1", len(snap))
	}
	rec := snap[0]
	if rec.StatusCode != http.StatusBadGateway {
		t.Errorf("record status = %d, want 502", rec.StatusCode)
	}
	if rec.ErrorType != "response_too_large" {
		t.Errorf("record error type = %q, want response_too_large", rec.ErrorType)
	}
	if want := config.FormatByteSize(wantCapBytes); !strings.Contains(rec.ErrorMsg, want) {
		t.Errorf("record error msg = %q, want it to carry the byte size %q", rec.ErrorMsg, want)
	}

	// Control: an in-range body still translates to a 200.
	resp2 := post()
	raw2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("in-range translated body: status = %d, want 200 (body: %s)", resp2.StatusCode, raw2)
	}
	if !strings.Contains(string(raw2), `"object":"chat.completion"`) {
		t.Errorf("in-range translated body = %q, want an OpenAI completion shape", raw2)
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
