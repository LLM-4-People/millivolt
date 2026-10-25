package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestQualityRetryNonStreamingVoid reproduces the degenerate-200 class on the
// non-streaming path: the first upstream 200 carries an empty completion, the
// proxy absorbs it and transparently retries BEFORE writing anything, and the
// client sees only the healthy second response (established behavior: a plain
// 0-in/0-out non-streaming response can poison client sessions).
func TestQualityRetryNonStreamingVoid(t *testing.T) {
	// reaskWake is when the re-send's handler woke: FinalAttemptAt (the
	// re-send's send start, stamped at admission in doWithRetry) must sit at
	// or before it, while a headers-arrival stamp would sit one reaskDelay
	// after it - that is the anchor pin below.
	const reaskDelay = 60 * time.Millisecond
	var reaskWake time.Time
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Each attempt carries its own request id: the record must describe
		// the attempt whose body the client received (captureUpstreamHeaders
		// re-captures after every successful re-send).
		if n == 1 {
			// The void attempt is SLOW: its (empty) answer sits ~400ms out.
			// Sleeps are floors, so the void's own ttfb is structurally
			// >= 400ms while the re-send's is >= 60ms - the two competing
			// timelines are far apart, and the ttfb bound below sits at the
			// midpoint (250ms). An upstream_ttfb_ms anchored anywhere but the
			// re-send's own start would carry the void's delay and fail the
			// bound by ~150ms even under scheduler noise.
			time.Sleep(400 * time.Millisecond)
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			w.Write([]byte(voidBody))
			return
		}
		reaskWake = time.Now()
		// The re-send delays its headers (still under the ttfb bound below):
		// the delay must land inside upstream_ttfb_ms, never inside
		// FinalAttemptAt, or the anchor pin below fails.
		time.Sleep(reaskDelay)
		w.Header().Set("X-Request-Id", "req-second")
		w.WriteHeader(200)
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := string(body); got != realBody {
		t.Fatalf("client body = %q, want the healthy second response", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (void absorbed + retry)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered request must not be an error: %+v", rec)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != metrics.CodeEmptyCompletion {
		t.Fatalf("attempts = %+v, want one absorbed %s", rec.Attempts, metrics.CodeEmptyCompletion)
	}
	if rec.Retries != 1 {
		t.Fatalf("retries = %d, want 1", rec.Retries)
	}
	if rec.FinishReason != "stop" || !rec.HadAnswerContent {
		t.Fatalf("final record should reflect the healthy body: finish=%q content=%v", rec.FinishReason, rec.HadAnswerContent)
	}
	if rec.ProviderRequestID != "req-second" {
		t.Fatalf("record must carry the re-send's provider metadata, got request id %q", rec.ProviderRequestID)
	}
	// The adopted-retry byte count: only the ADOPTED attempt's body was ever
	// written (the void was absorbed pre-write), so response_bytes equals the
	// healthy body alone - never the union with the abandoned void body.
	if rec.ResponseBytes != int64(len(realBody)) {
		t.Errorf("ResponseBytes = %d, want %d (the adopted attempt's body only, not the void's too)", rec.ResponseBytes, len(realBody))
	}
	if rec.RequestBytes != int64(len(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)) {
		t.Errorf("RequestBytes = %d, want the sent request body length", rec.RequestBytes)
	}
	// The httptrace decomposition describes the RE-SEND (the adopted
	// attempt), like every other record-level fact above: upstream_ttfb_ms
	// measures from the re-send's own start, not from the request start, so
	// the slow void attempt's ~400ms header delay is absent (FinalAttemptAt -
	// Start proves that delay was really on the wire timeline). The void
	// attempt's fresh dial also died with its context: the re-send rode the
	// pooled connection, so connect is 0 and reused is set.
	if rec.UpstreamTTFBMs < 30 || rec.UpstreamTTFBMs >= 250 {
		t.Errorf("UpstreamTTFBMs = %d, want 30 <= ttfb < 250 (the re-send's own ~60ms header wait; the void's ~400ms timeline and any wrong anchor sit far above the bound)", rec.UpstreamTTFBMs)
	}
	if rec.FinalAttemptAt.Sub(rec.Start) < 350*time.Millisecond {
		t.Fatalf("FinalAttemptAt - Start = %v, want >= 350ms (the void's ~400ms delay is on the request timeline, so the ttfb bound above discriminates)", rec.FinalAttemptAt.Sub(rec.Start))
	}
	// The anchor pin: FinalAttemptAt must be the re-send's SEND START
	// (stamped at its admission inside doWithRetry), never its headers
	// arrival. The re-send's handler woke at reaskWake and delayed its
	// headers ~60ms after waking, so a headers-arrival stamp would sit
	// ~60ms AFTER reaskWake while the send-start stamp sits at or before
	// it - the re-send's own header wait belongs to upstream_ttfb_ms.
	if late := rec.FinalAttemptAt.Sub(reaskWake); late > 30*time.Millisecond {
		t.Fatalf("FinalAttemptAt sits %v after the re-send's handler woke, want <= 30ms (a headers-arrival stamp carries the ~60ms header wait and fails this)", late)
	}
	if rec.UpstreamConnectMs != 0 || !rec.UpstreamConnReused {
		t.Errorf("UpstreamConnectMs/ConnReused = %d/%v, want 0/true (the re-send rode the void attempt's pooled connection; the fresh dial died with it)", rec.UpstreamConnectMs, rec.UpstreamConnReused)
	}
	if rec.UpstreamTLSMs != 0 {
		t.Errorf("UpstreamTLSMs = %d, want 0 (plaintext upstream: the TLS hook never fired)", rec.UpstreamTLSMs)
	}
}

func TestQualityRetryClosesReplacementBody(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			w.Write([]byte(voidBody))
			return
		}
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	var voidClosed, realClosed atomic.Int32
	var seq atomic.Int32
	p := New(config.Default(), metrics.Noop{})
	base := p.httpClient()
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := rt.RoundTrip(r)
		if err != nil {
			return resp, err
		}
		n := seq.Add(1)
		dest := &realClosed
		if n == 1 {
			dest = &voidClosed
		}
		resp.Body = &closeCountBody{ReadCloser: resp.Body, n: dest}
		return resp, nil
	})})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if voidClosed.Load() < 1 {
		t.Fatal("void body was never closed")
	}
	if realClosed.Load() < 1 {
		t.Fatal("replacement body was never closed")
	}
}

// TestQualityRetryExhaustedSurfaces502: with the quality budget spent (or
// disabled), a still-degenerate 200 is surfaced as HTTP 502 + OpenAI error
// JSON - a failure the client retries, never a silently empty success.
func TestQualityRetryExhaustedSurfaces502(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(voidBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(0), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a still-degenerate 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("502 body must carry the degenerate code: %s", body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (quality disabled)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != metrics.CodeEmptyCompletion {
		t.Fatalf("record must flag the degenerate outcome: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
}

// TestQualityRetrySkipsLargeBody: a body past the spool cap is committed to
// verbatim relay without classification - big bodies are never degenerate.
func TestQualityRetrySkipsLargeBody(t *testing.T) {
	var calls atomic.Int32
	big := fmt.Sprintf(`{"id":"1","choices":[{"finish_reason":"stop","message":{"content":%q}}]}`, strings.Repeat("x", 1<<20+4096))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(big))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != big {
		t.Fatalf("large body must relay verbatim (status %d, len %d)", resp.StatusCode, len(body))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no quality retry on large bodies)", n)
	}
	// The overflow shape: past the spool cap the body commits to verbatim
	// relay through nonStreamBody (spooled prefix + live remainder), so the
	// full oversized body is accounted.
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want 1", len(recs))
	}
	if recs[0].ResponseBytes != int64(len(big)) {
		t.Errorf("ResponseBytes = %d, want %d (the overflowed body relayed verbatim)", recs[0].ResponseBytes, len(big))
	}
}

// TestQualityRetryNonRetryableInBandErrorVerbatim: a non-streaming 200 with a
// top-level error envelope in a class that is NOT transient
// (invalid_request_error) is the provider's own final statement - relayed
// verbatim, never quality-retried.
func TestQualityRetryNonRetryableInBandErrorVerbatim(t *testing.T) {
	var calls atomic.Int32
	errBody := `{"error":{"message":"bad request","type":"invalid_request_error","code":"bad_request"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(errBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != errBody {
		t.Fatalf("in-band error must relay verbatim: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() {
		t.Fatal("record must carry the provider's in-band error")
	}
}

// TestQualityRetryRetriesRetryableInBandError: a non-streaming 200 whose body
// is a top-level error envelope in a transient server-availability class
// (overloaded_error) is absorbed and the SAME request re-sent before any byte
// reaches the client - the OpenAI retry semantics applied to the in-band
// failure. The attempt log carries the provider's own envelope.
func TestQualityRetryRetriesRetryableInBandError(t *testing.T) {
	var calls atomic.Int32
	errBody := `{"error":{"message":"overloaded","type":"overloaded_error","code":"overload"}}`
	okBody := `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":"hi"}}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(errBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(okBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != okBody {
		t.Fatalf("re-send must deliver the healthy body verbatim: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 || len(rec.Attempts) != 1 {
		t.Fatalf("recovered record: retries=%d attempts=%d isError=%v", rec.Retries, len(rec.Attempts), rec.IsError())
	}
	at := rec.Attempts[0]
	if at.ErrorType != "overloaded_error" || at.ErrorCode != "overload" || at.ErrorMsg != "overloaded" {
		t.Fatalf("absorbed attempt must carry the provider's envelope: %+v", at)
	}
}

// TestQualityRetryRetryableInBandErrorZeroBudget: with quality_retries spent
// (0) even a transient in-band error class relays verbatim - the provider's
// statement is the client's to retry.
func TestQualityRetryRetryableInBandErrorZeroBudgetVerbatim(t *testing.T) {
	var calls atomic.Int32
	errBody := `{"error":{"message":"overloaded","type":"overloaded_error","code":"overload"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(errBody))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.QualityRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != errBody {
		t.Fatalf("zero budget must relay verbatim: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorType != "overloaded_error" {
		t.Fatalf("record must carry the provider's in-band error: %+v", recs[0])
	}
}
