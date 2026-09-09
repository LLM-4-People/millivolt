package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// mockRateLimited returns 429 with Retry-After for the first N requests, then
// 200. Used to verify the proxy transparently queues and retries.
func mockRateLimited(n int32) (*httptest.Server, *atomic.Int32) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := count.Add(1)
		if c <= n {
			w.Header().Set("Retry-After", "0") // immediate retry
			w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`))
	}))
	return srv, &count
}

func TestTransparentRateLimitRetry(t *testing.T) {
	upstream, count := mockRateLimited(2) // fail twice, then succeed
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (transparent retry). body: %s", resp.StatusCode, body)
	}
	if count.Load() != 3 {
		t.Errorf("upstream hits = %d, want 3 (2 failures + 1 success)", count.Load())
	}

	// Metrics should record 1 retry and rate_limited=true.
	snap := buf.Snapshot()
	if len(snap) == 0 {
		t.Fatal("no records")
	}
	rec := snap[len(snap)-1]
	if rec.Retries != 2 {
		t.Errorf("retries = %d, want 2", rec.Retries)
	}
	if !rec.RateLimited {
		t.Errorf("rate_limited = false, want true")
	}
	if rec.StatusCode != 200 {
		t.Errorf("final status = %d, want 200", rec.StatusCode)
	}
}

func TestRateLimitExhaustsRetries(t *testing.T) {
	upstream, _ := mockRateLimited(9999) // always fail
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 2 // fail fast for test
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// After MaxRetries (2) + initial attempt = 3 upstream calls, we surface the
	// last 429 to the client (can't retry forever).
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 after exhausting retries; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "rate limited") {
		t.Errorf("exhausted 429 body = %q, want the peeked rate-limit envelope re-wrapped", body)
	}
	_ = start
}

// TestRetryAfterNotClampedToMaxBackoff is the end-to-end regression for daily
// rate-limit 429s: the provider's retry hint must be waited in full even when
// it exceeds max_backoff. Previously BackoffFor clamped every hint to
// max_backoff (default 2m), so a 35m daily-limit Retry-After was retried after
// 2m and immediately 429'd again.
func TestRetryAfterNotClampedToMaxBackoff(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Sub-second hint via the OpenAI reset header so the test stays
			// fast; parseRetryAfter accepts duration strings here. MaxBackoff
			// below is 1ms - if the hint were clamped, we'd retry almost
			// immediately.
			w.Header().Set("X-Ratelimit-Reset-Requests", "80ms")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"daily limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxBackoff = time.Millisecond
	cfg.MaxRetries = 2
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start)

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 after honoring retry hint", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2", calls.Load())
	}
	if elapsed < 70*time.Millisecond {
		t.Errorf("retried in %v; 80ms retry hint was clamped to max_backoff=1ms", elapsed)
	}
}

// TestShortRetryAfterDoesNotResetAttemptBackoff is the 503 Retry-After: 1s
// case: HTTP Retry-After is integer seconds, so a "retry shortly" 503 often
// carries a 1s hint on every attempt. That hint is a floor, not a reset -
// adaptive doubling still grows. A 10ms reset header with 80ms base would
// finish in ~30ms if the hint replaced exponential; grown waits are ~80+160+320.
func TestShortRetryAfterDoesNotResetAttemptBackoff(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("X-Ratelimit-Reset-Requests", "10ms")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"type":"upstream_error","code":"upstream_error","message":"Hosted inference is temporarily unavailable. Please retry shortly."}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 3
	cfg.BaseBackoff = 80 * time.Millisecond
	cfg.MaxBackoff = 400 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 after exhausting retries", resp.StatusCode)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("upstream hits = %d, want 4 (1 initial + 3 retries)", got)
	}
	// Jitter floor 0.75×: 60+120+240ms = 420ms. Stay under that so a slow
	// host still fails the old 10ms×3 path (~30ms) without flaking.
	if elapsed < 300*time.Millisecond {
		t.Fatalf("retried in %v; short retry hint reset exponential (want ~80+160+320ms)", elapsed)
	}
}

func TestTransparent5xxRetry(t *testing.T) {
	// Upstream fails with 502 twice, then succeeds. The client must see ONLY
	// the final 200 - the 5xx errors are absorbed transparently.
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway) // 502
			w.Write([]byte(`{"error":{"type":"bad_gateway","message":"upstream overloaded"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.MaxRetries = 5
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	// Client sees only success; upstream absorbed the two 502s.
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (5xx absorbed transparently)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("upstream calls = %d, want 3 (2 failures + 1 success)", got)
	}

	// The record must reflect the retries and NOT be mislabeled as rate-limited
	// (502 is a transient upstream error, not a 429/503).
	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("recorded %d records, want 1", len(snap))
	}
	rec := snap[0]
	if rec.Retries != 2 {
		t.Errorf("Retries = %d, want 2", rec.Retries)
	}
	if rec.RateLimited {
		t.Errorf("RateLimited = true, want false for a 502 (not a rate limit)")
	}
	if rec.StatusCode != 200 {
		t.Errorf("recorded StatusCode = %d, want 200", rec.StatusCode)
	}
}

func Test5xxExhaustsSurfacesLastError(t *testing.T) {
	// Upstream always 500s. After exhausting retries, the client must receive
	// the real upstream error (500 + its body), not a masked/proxy error.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"type":"internal","message":"model crashed"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 2
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want 500 (last upstream error surfaced)", resp.StatusCode)
	}
	if !strings.Contains(string(body), "model crashed") {
		t.Errorf("body = %q, want upstream error body surfaced verbatim", body)
	}
}

func TestRetryAttemptsRecorded(t *testing.T) {
	// Two distinct 5xx failures, then success: both absorbed attempts must be
	// recorded with their status and provider error detail.
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"type":"bad_gateway","code":"bg1","message":"upstream overloaded"}}`))
			return
		}
		if n == 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"error":{"type":"service_unavailable","message":"try later"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.MaxRetries = 5
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("records = %d", len(snap))
	}
	rec := snap[0]
	if len(rec.Attempts) != 2 {
		t.Fatalf("Attempts = %d, want 2 (%+v)", len(rec.Attempts), rec.Attempts)
	}
	if rec.Attempts[0].StatusCode != 502 || rec.Attempts[0].ErrorType != "bad_gateway" || rec.Attempts[0].ErrorCode != "bg1" {
		t.Errorf("attempt[0] = %+v, want 502/bad_gateway/bg1", rec.Attempts[0])
	}
	if rec.Attempts[1].StatusCode != 503 || rec.Attempts[1].ErrorType != "service_unavailable" {
		t.Errorf("attempt[1] = %+v, want 503/service_unavailable", rec.Attempts[1])
	}
	if rec.Attempts[1].RetryAfterMs <= 0 {
		t.Errorf("attempt[1].RetryAfterMs = %d, want > 0 (Retry-After: 1 header)", rec.Attempts[1].RetryAfterMs)
	}
}

func TestRetryTTFTIsFinalAttempt(t *testing.T) {
	// First attempt fails fast with a 502; the winning attempt then delays
	// ~120ms before its first token. TTFT must be ~the winning attempt's TTFT
	// (~120ms), NOT the total time since the original request (which includes
	// the failed attempt + backoff). Duration must still cover everything.
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"type":"bad_gateway","message":"boom"}}`))
			return
		}
		// Winning attempt: small delay before first token.
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.MaxRetries = 3
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("records = %d", len(snap))
	}
	rec := snap[0]
	if rec.Retries != 1 {
		t.Fatalf("Retries = %d, want 1", rec.Retries)
	}
	// TTFT should be ~the winning attempt's delay (~120ms), clearly below the
	// total wall time. Backoff has jitter (375–625ms) plus the failed attempt,
	// so duration is reliably >= ~500ms while TTFT stays ~120ms.
	if rec.TTFTMs > 400 {
		t.Errorf("TTFTMs = %d, want ~120 (final attempt), not the retry-inclusive total", rec.TTFTMs)
	}
	// Duration must exceed TTFT by at least the minimum backoff (375ms),
	// proving it includes the retry delay TTFT excludes.
	if rec.DurationMs < rec.TTFTMs+350 {
		t.Errorf("DurationMs = %d, want >= TTFT(%d)+350 (includes retry backoff)", rec.DurationMs, rec.TTFTMs)
	}
}

func TestTransparentTransportErrorRetry(t *testing.T) {
	// First attempt: server accepts the connection then closes it without a
	// response (a transport error, not an HTTP status). Second attempt succeeds.
	// The client must see ONLY the 200 - the transport error is retried.
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			// Hijack and close abruptly -> client sees a transport error (EOF).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("not hijackable")
			}
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.MaxRetries = 3
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (transport error absorbed)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (1 transport failure + 1 success)", got)
	}
	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("records = %d", len(snap))
	}
	rec := snap[0]
	if rec.Retries != 1 {
		t.Errorf("Retries = %d, want 1", rec.Retries)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != "transport" {
		t.Errorf("Attempts = %+v, want 1 transport attempt", rec.Attempts)
	}
}

func TestTransportRetryPreservesSendDeadline(t *testing.T) {
	for _, mode := range []string{"transient", "deadline", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config.Default()
			cfg.BaseBackoff = time.Millisecond
			cfg.MaxBackoff = time.Millisecond
			buf := metrics.NewBuffer(8)
			p := New(cfg, buf)
			var calls int
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if mode == "deadline" {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				if calls == 1 {
					return nil, io.ErrUnexpectedEOF
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hello world\"}}]}\n\ndata: [DONE]\n\n"))}, nil
			})})
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"fixture","stream":true}`))
			req.Header.Set("Authorization", "Bearer fixture-key")
			req.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
			req.Header.Set("X-Proxy-Timeout-Ms", "1000")
			if mode == "deadline" {
				req.Header.Set("X-Proxy-Timeout-Ms", "1")
			}
			if mode == "canceled" {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, req)
			recs := buf.Snapshot()
			if len(recs) != 1 {
				t.Fatalf("records = %d, want one finalized outcome", len(recs))
			}
			if mode == "transient" {
				if calls != 2 || w.Code != http.StatusOK || recs[0].Retries != 1 {
					t.Fatalf("send cleanup bypassed retry: calls=%d status=%d retries=%d", calls, w.Code, recs[0].Retries)
				}
			} else if calls > 1 || recs[0].Retries != 0 {
				t.Fatalf("expired/canceled request retried: calls=%d retries=%d", calls, recs[0].Retries)
			}
			if mode == "deadline" && (calls != 1 || w.Code != http.StatusBadGateway) {
				t.Fatalf("deadline fixture did not exercise one failed send: calls=%d status=%d", calls, w.Code)
			}
		})
	}
}

func TestQuota429NotRetried(t *testing.T) {
	// A 429 carrying a DURABLE quota/billing error (insufficient_quota) can
	// never clear by waiting: the proxy must surface it to the client
	// immediately - no absorbed attempts, no retry loop, no group pacing -
	// while transient rate-limit 429s keep being retried (covered by
	// TestTransparentRateLimitRetry). Regression: quota exhaustion once burned
	// the full retry budget, delaying the client for seconds.
	const quotaBody = `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_credits"}}`
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(quotaBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf)) // default MaxRetries > 1
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 429 {
		t.Errorf("status = %d, want 429 surfaced immediately", resp.StatusCode)
	}
	if string(body) != quotaBody {
		t.Errorf("body relayed = %q, want upstream quota error verbatim", body)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1 (quota 429 must not be retried)", got)
	}

	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("records = %d", len(snap))
	}
	rec := snap[0]
	if rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Errorf("Retries/Attempts = %d/%d, want 0/0 (nothing absorbed)", rec.Retries, len(rec.Attempts))
	}
	if rec.StatusCode != 429 || rec.ErrorType != "insufficient_quota" || rec.ErrorCode != "insufficient_credits" {
		t.Errorf("record = %d/%q/%q, want 429/insufficient_quota/insufficient_credits", rec.StatusCode, rec.ErrorType, rec.ErrorCode)
	}
	if rec.RateLimited {
		t.Errorf("RateLimited = true, want false (quota is not a rate limit)")
	}
	if rec.IsError() {
		t.Errorf("IsError = true, want false (429 is flow control, never an error)")
	}
}

func TestQuota429EndsRetryLoop(t *testing.T) {
	// First attempt: transient rate-limit 429 (retried). Second attempt: a
	// durable quota 429 (must end the loop as the final outcome - no third
	// attempt, quota error surfaced verbatim with the transient attempt
	// logged as absorbed).
	var calls int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"message":"out of credits","type":"insufficient_quota","code":"insufficient_credits"}}`))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "out of credits") {
		t.Errorf("body = %q, want the quota error surfaced", body)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("upstream calls = %d, want 2 (rate limit absorbed, quota final)", got)
	}

	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("records = %d", len(snap))
	}
	rec := snap[0]
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != "rate_limit_error" {
		t.Errorf("Attempts = %+v, want 1 absorbed rate_limit_error", rec.Attempts)
	}
	if rec.ErrorType != "insufficient_quota" {
		t.Errorf("final ErrorType = %q, want insufficient_quota", rec.ErrorType)
	}

	start := time.Now()
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	req2.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("follow-up after quota-ended retry loop waited %v; must not inherit request backoff", elapsed)
	}
}

func TestQuota429OnLastAttemptDoesNotPace(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"insufficient_quota","code":"insufficient_credits","message":"no credits"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 1 // quota lands on the exhaust slot
	cfg.BaseBackoff = time.Second
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() int {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := do(); got != 429 {
		t.Fatalf("status = %d, want 429", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (rate-limit + last-attempt quota)", got)
	}
	start := time.Now()
	if got := do(); got != 429 {
		t.Fatalf("follow-up status = %d, want 429", got)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("follow-up after last-attempt quota waited %v; must not FailSend", elapsed)
	}
}

func TestClientCancelNotRetried(t *testing.T) {
	// If the CALLER cancels (client disconnect), the proxy must NOT retry - the
	// client is gone. The attempt should surface as a cancellation, not loop.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Simulate a slow/stalled upstream.
		time.Sleep(2 * time.Second)
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 5
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	go func() { time.Sleep(150 * time.Millisecond); cancel() }()
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		resp.Body.Close()
	}
	// Must return promptly on cancel - not retry for 5 attempts.
	if elapsed > 1*time.Second {
		t.Errorf("took %v after client cancel, want prompt abort (no retry loop)", elapsed)
	}
}

// TestRetrySerializesSameKey pins the send gate: after a 5xx (same as 429),
// siblings on the same provider+key wait until the retrying request succeeds
// or exhausts. Only then do they send.
func TestRetrySerializesSameKey(t *testing.T) {
	var calls, inflight, maxAfter atomic.Int32
	first := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		cur := inflight.Add(1)
		defer inflight.Add(-1)
		if n == 1 {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"type":"bad_gateway","message":"blip"}}`))
			close(first)
			return
		}
		for {
			old := maxAfter.Load()
			if cur <= old || maxAfter.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.BaseBackoff = 40 * time.Millisecond
	cfg.MaxBackoff = 40 * time.Millisecond
	cfg.MaxRetries = 3
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	done1 := make(chan struct{})
	go func() { do(); close(done1) }()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("first 502 never arrived")
	}
	// The group's tripped/probing state has no exported observable from this
	// package (scheduler internals), so a fixed window sequences the Trip
	// before the siblings arrive.
	time.Sleep(10 * time.Millisecond) // Trip + WaitSend

	done2 := make(chan struct{})
	done3 := make(chan struct{})
	go func() { do(); close(done2) }()
	go func() { do(); close(done3) }()
	// Detection window: a sibling parked inside WaitSend is not observable
	// from this package, so give them time to (wrongly) send if the send gate
	// were broken before asserting the negative.
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d during 5xx retry; siblings must wait", got)
	}

	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("owner did not finish")
	}
	select {
	case <-done2:
	case <-time.After(2 * time.Second):
		t.Fatal("sibling 2 stuck")
	}
	select {
	case <-done3:
	case <-time.After(2 * time.Second):
		t.Fatal("sibling 3 stuck")
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("upstream calls = %d, want 4 (1×502 + owner 200 + 2 siblings)", got)
	}
	if got := maxAfter.Load(); got > 1 {
		t.Fatalf("max overlapping sends after first 502 = %d, want 1 (half-open then clean probe)", got)
	}
}

// TestConsecutiveFailuresDoubleBackoff: exhausted requests on the same
// provider+key wait base, then 2×base, before the next first send.
func TestConsecutiveFailuresDoubleBackoff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"type":"bad_gateway","message":"down"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.BaseBackoff = 80 * time.Millisecond
	cfg.MaxBackoff = 400 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	do := func() time.Duration {
		start := time.Now()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Helper()
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 502 {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		return time.Since(start)
	}

	if elapsed := do(); elapsed > 50*time.Millisecond {
		t.Fatalf("first exhaust waited %v, want immediate", elapsed)
	}
	if elapsed := do(); elapsed < 50*time.Millisecond {
		t.Fatalf("second request in %v; first fail should pace ~80ms", elapsed)
	}
	if elapsed := do(); elapsed < 100*time.Millisecond {
		t.Fatalf("third request in %v; second fail should pace ~160ms", elapsed)
	}
}

func TestRecoveredRequestResetsBackoff(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"type":"bad_gateway","message":"down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.BaseBackoff = 80 * time.Millisecond
	cfg.MaxBackoff = 400 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() (int, time.Duration) {
		t.Helper()
		start := time.Now()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode, time.Since(start)
	}

	if code, elapsed := do(); code != 502 || elapsed > 50*time.Millisecond {
		t.Fatalf("first exhaust status=%d waited %v, want immediate 502", code, elapsed)
	}
	if code, elapsed := do(); code != 502 || elapsed < 50*time.Millisecond {
		t.Fatalf("second exhaust status=%d in %v; first FailSend should pace ~80ms", code, elapsed)
	}
	fail.Store(false)
	if code, elapsed := do(); code != 200 || elapsed < 100*time.Millisecond {
		t.Fatalf("recovery status=%d in %v; leftover request backoff should still pace ~160ms", code, elapsed)
	}
	if code, elapsed := do(); code != 200 || elapsed > 50*time.Millisecond {
		t.Fatalf("request after recovered 200 status=%d waited %v, want immediate", code, elapsed)
	}
}

func TestQuota429DoesNotPaceSibling(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			close(started)
			<-release
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"type":"insufficient_quota","code":"insufficient_credits","message":"no credits"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 2
	cfg.BaseBackoff = time.Second
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return 0
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}

	done1 := make(chan int, 1)
	go func() { done1 <- do() }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("quota request never started")
	}
	close(release)
	select {
	case code := <-done1:
		if code != 429 {
			t.Fatalf("quota status = %d, want 429", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("quota request stuck")
	}

	start := time.Now()
	if got := do(); got != 200 {
		t.Fatalf("sibling status = %d, want 200", got)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("sibling of quota 429 waited %v; group must not be tripped", elapsed)
	}
}

func TestQuota429MaxRetriesZeroDoesNotPace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"insufficient_quota","code":"insufficient_credits","message":"no credits"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.BaseBackoff = time.Second
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	do()
	start := time.Now()
	do()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("second quota 429 waited %v; MaxRetries=0 must not FailSend", elapsed)
	}
}

func TestConsecutiveExhausted429sDoubleBackoff(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.BaseBackoff = 80 * time.Millisecond
	cfg.MaxBackoff = 400 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() time.Duration {
		t.Helper()
		start := time.Now()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 429 {
			t.Fatalf("status = %d, want 429", resp.StatusCode)
		}
		return time.Since(start)
	}

	if elapsed := do(); elapsed > 50*time.Millisecond {
		t.Fatalf("first exhausted 429 waited %v, want immediate", elapsed)
	}
	if elapsed := do(); elapsed < 50*time.Millisecond {
		t.Fatalf("second 429 in %v; first fail should pace ~80ms", elapsed)
	}
	if elapsed := do(); elapsed < 100*time.Millisecond {
		t.Fatalf("third 429 in %v; second fail should pace ~160ms", elapsed)
	}
}

func TestExhaustedRetriesPaceNextFirstSend(t *testing.T) {
	var calls atomic.Int32
	var secondFirst time.Time
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 4 {
			secondFirst = time.Now()
		}
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"type":"bad_gateway","message":"down"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 2
	cfg.BaseBackoff = 200 * time.Millisecond
	cfg.MaxBackoff = 800 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	groupKey := providerFromURL(u) + "|" + hashKey("sk-test")

	do := func() {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	do()
	if got := calls.Load(); got != 3 {
		t.Fatalf("first request hits = %d, want 3", got)
	}
	if got := p.scheduler.Backoff(groupKey); got != 0 {
		t.Fatalf("attempt backoff after FailSend(own) = %v, want 0", got)
	}
	if got := p.scheduler.RequestBackoff(groupKey); got != 200*time.Millisecond {
		t.Fatalf("request backoff = %v, want 200ms (base, not leftover attempt 400ms)", got)
	}
	t0 := time.Now()
	do()
	if secondFirst.IsZero() {
		t.Fatal("second request never hit upstream")
	}
	// request base 200ms × 0.75–1.25 = 150–250ms. Leftover attempt 400ms
	// × 0.75 = 300ms - the 280ms cap sits in that gap.
	if elapsed := secondFirst.Sub(t0); elapsed < 120*time.Millisecond {
		t.Fatalf("next first send in %v; exhausted retryable must wait ~base request backoff", elapsed)
	}
	if elapsed := secondFirst.Sub(t0); elapsed > 280*time.Millisecond {
		t.Fatalf("next first send waited %v; must be request backoff (~200ms), not leftover attempt (400ms+)", elapsed)
	}
}

func TestClientErrorDoesNotFailSend(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"bad"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 2
	cfg.BaseBackoff = time.Second
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	do := func() {
		t.Helper()
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	do()
	start := time.Now()
	do()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("second 4xx waited %v; client errors must not FailSend", elapsed)
	}
}

func TestClientAbortDoesNotFailSend(t *testing.T) {
	unblock := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-unblock:
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 2
	cfg.BaseBackoff = time.Second
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
	}
	close(unblock)

	start := time.Now()
	req2, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req2.Header.Set("Authorization", "Bearer sk-test")
	req2.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("follow-up after client abort waited %v; abort must not FailSend", elapsed)
	}
}
