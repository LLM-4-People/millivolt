package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// TestPauseIgnoresProxyTimeoutUntilUnpause pins X-Proxy-Timeout-Ms as an
// upstream-header deadline, not a hold deadline: a 50ms timeout must not
// abort a request that is still parked.
func TestPauseIgnoresProxyTimeoutUntilUnpause(t *testing.T) {
	const proxyTimeout = 200 * time.Millisecond
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{{All: true}})
	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Timeout-Ms", "200")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	// Parked IS observable. A fixed sleep is not: under suite load the request
	// can still be in flight to the proxy when the sleep ends, which would
	// unpause before the hold ever applied and let the 200ms send deadline
	// govern the local upstream exchange. Poll the scheduler queue first.
	if !waitFor(t, func() bool { return p.scheduler.Stats().Queued >= 1 }) {
		t.Fatalf("request never parked (queued=%d, upstream hits=%d)", p.scheduler.Stats().Queued, hits.Load())
	}
	if hits.Load() != 0 {
		t.Fatal("paused request reached upstream")
	}
	// The window is the negative proof: the 200ms send budget expires while
	// the request is still parked, so a buggy arming would have fired by now.
	time.Sleep(proxyTimeout + 100*time.Millisecond)
	if hits.Load() != 0 {
		t.Fatal("X-Proxy-Timeout-Ms fired during pause")
	}
	p.applyHolds(nil)
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status = %d, want 200 after unpause", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after unpause")
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestRetryHoldWaitNotBoundedByProxyTimeout pins the retry-side half of the
// X-Proxy-Timeout-Ms contract: the header timeout is a PER-UPSTREAM-SEND
// deadline (headers + body), so a RETRY parked on an operator hold in WaitSend
// must not burn the budget - the retried send gets a fresh deadline when the
// hold releases. (The first attempt never waits on holds in WaitSend, which is
// why TestPauseIgnoresProxyTimeoutUntilUnpause passes either way.)
func TestRetryHoldWaitNotBoundedByProxyTimeout(t *testing.T) {
	var calls atomic.Int32
	firstAttempt := make(chan struct{})
	cfg := config.Default()
	cfg.MaxRetries = 2
	cfg.BaseBackoff = 10 * time.Millisecond
	cfg.MaxBackoff = 20 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Arm the hold DURING the first attempt: Acquire already granted
			// (in-flight slots finish) and attempt 0's WaitSend ignores holds,
			// so the hold deterministically parks the RETRY's WaitSend.
			p.applyHolds([]persistedPause{{All: true}})
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
			close(firstAttempt)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Timeout-Ms", "300")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()

	select {
	case <-firstAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("first attempt never reached upstream")
	}
	// Hold well past the 300ms header timeout, then release: the retry must
	// proceed with a fresh per-send budget, not a burned one.
	time.Sleep(600 * time.Millisecond)
	p.applyHolds(nil)

	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status = %d, want 200 after unpause (retry hold wait must not burn X-Proxy-Timeout-Ms)", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retried request did not finish after unpause")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
}

// TestQualityRetryHonorsOperatorHold pins the hold contract for the generic
// quality retry: the degenerate-200 re-send is a RETRY ("a retry is a new
// send and waits"), so a hold armed during the void attempt must park the
// re-send in WaitSend exactly like a 429/5xx retry - the operator hold is
// never bypassed (X-Proxy-Timeout-Ms is unset here; it must not bound the
// hold either). Pre-fix, doWithRetry's opening WaitSend ran with
// honorHold=false and the re-send fired while the hold was live.
func TestQualityRetryHonorsOperatorHold(t *testing.T) {
	var calls atomic.Int32
	p := New(newConfigWithQuality(1), metrics.Noop{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			// Arm the global hold DURING the degenerate attempt: Acquire
			// already granted (in-flight slots finish), so the hold
			// deterministically parks the quality RE-SEND's WaitSend.
			p.applyHolds([]persistedPause{{All: true}})
			w.Write([]byte(voidBody))
			return
		}
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan struct{})
	var gotStatus int
	var gotBody []byte
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		gotStatus = resp.StatusCode
		gotBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}()

	// Wait for the degenerate attempt to land (the hold is armed inside it).
	// The re-send may already have fired too (that is the bug under test) -
	// the window assertion below is the actual proof.
	if !waitFor(t, func() bool { return calls.Load() >= 1 }) {
		t.Fatalf("void attempt never reached upstream (calls=%d)", calls.Load())
	}
	// Negative-proof window: the re-send must NOT fire while the hold is
	// live. Pre-fix it went out within microseconds of the void attempt.
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("quality re-send hit upstream %d× while the operator hold was live, want 0 (a retry is a new send and waits)", n-1)
	}

	p.applyHolds(nil)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("re-send did not run after unpause")
	}
	if gotStatus != 200 || string(gotBody) != realBody {
		t.Fatalf("client got status=%d body=%q, want the healthy re-send response", gotStatus, gotBody)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (void absorbed + re-send after unpause)", n)
	}
}

// TestFiredSendTimeoutIsNotRetried pins the third half of the X-Proxy-Timeout
// contract: a FIRED per-send deadline must surface immediately as a 502 -
// never absorbed as a retryable transport timeout (context.DeadlineExceeded
// satisfies net.Error.Timeout(), so without the attempt-ctx check in
// doWithRetry's never-retry branch the fired budget would be re-sent up to
// max_retries times, multiplying the client's own timeout).
func TestFiredSendTimeoutIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Stall past any budget: hold the handler open until the proxy's
		// per-send deadline aborts the connection, with a bounded fallback
		// so upstream.Close() cannot hang either.
		select {
		case <-r.Context().Done():
		case <-time.After(750 * time.Millisecond):
		}
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Timeout-Ms", "50")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after the fired send deadline", resp.StatusCode)
	}
	if !strings.Contains(string(body), "upstream_unreachable") {
		t.Fatalf("body = %q, want upstream_unreachable", body)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want 1 (a fired per-send deadline must not be retried)", n)
	}
}

// TestCursorFiredSendTimeoutIsNotPaced is the cursor mirror of
// TestFiredSendTimeoutIsNotRetried: a FIRED per-send budget
// (X-Proxy-Timeout-Ms) on the bidi path must surface 502 with the canonical
// "context deadline exceeded" message - never the canceled request
// context's misleading "context canceled" - must NOT record a client
// disconnect (the local client is still there; its budget expired), and must
// NOT FailSend-pace the provider+key group: the client's own deadline is not
// an upstream health signal (in the doCursorUntil race window the surfaced
// error is context.DeadlineExceeded, which isRetryableTransportErr treats as
// a net timeout - pacing every sibling for a client-controlled expiry).
func TestCursorFiredSendTimeoutIsNotPaced(t *testing.T) {
	var hits atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// Stall without response headers past any budget: the proxy's
			// per-send deadline aborts the Do. Bounded fallback so
			// upstream.Close() cannot hang.
			select {
			case <-r.Context().Done():
			case <-time.After(750 * time.Millisecond):
			}
			return
		}
		// Follow-up requests get a complete minimal turn.
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, _ = readFrame(t, r.Body)
		w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "ok")))))
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	cfg := config.Default()
	cfg.BaseBackoff = time.Second // any FailSend pacing would be unmistakable
	cfg.MaxBackoff = 2 * time.Second
	buf := metrics.NewBuffer(10)
	p := New(cfg, buf)
	srv := httptest.NewServer(p)
	defer srv.Close()

	newReq := func() *http.Request {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer cursor-key")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Format", "cursor")
		req.Header.Set("X-Proxy-Timeout-Ms", "50")
		return req
	}

	resp, err := http.DefaultClient.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after the fired send deadline", resp.StatusCode)
	}
	if !strings.Contains(string(body), "context deadline exceeded") {
		t.Fatalf("502 body = %q, want the canonical 'context deadline exceeded' (the generic path's message), never a bare 'context canceled'", body)
	}
	// The doCursorUntil race window surfaces the RAW deadline error
	// (context.DeadlineExceeded - a retryable-class net timeout per
	// isRetryableTransportErr). The classifier must still refuse to pace it.
	if paced, _ := cursorSendFailure(context.DeadlineExceeded, context.DeadlineExceeded); paced {
		t.Error("cursorSendFailure must not pace a fired per-send budget even in the raw-DeadlineExceeded race shape")
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ClientDisconnected {
		t.Error("ClientDisconnected = true, want false (the local client is connected; its budget expired)")
	}
	if rec.StatusCode != http.StatusBadGateway || rec.ErrorType != "upstream_unreachable" || !rec.IsError() {
		t.Errorf("record = status %d type %q, want 502/upstream_unreachable error", rec.StatusCode, rec.ErrorType)
	}
	// The direct no-pacing proof is behavioral: the follow-up on the same
	// provider|key below must complete immediately - any FailSend pacing
	// (base_backoff is 1s here) would be unmistakable.

	// The follow-up on the same provider|key must complete immediately.
	start := time.Now()
	resp2, err := http.DefaultClient.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("follow-up took %v; the group was paced by the fired budget (base_backoff=%v)", elapsed, cfg.BaseBackoff)
	}
	if resp2.StatusCode != 200 || !strings.Contains(string(body2), `"content":"ok"`) {
		t.Fatalf("follow-up status = %d body = %q, want the completed turn", resp2.StatusCode, body2)
	}
}
