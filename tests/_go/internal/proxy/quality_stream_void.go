package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestStreamVoidReplacedWithInBandError: a stream that ends cleanly but
// produced nothing has its TERMINAL FINISH CHUNK replaced by an in-band error
// (the error must arrive instead of the finish chunk - SDKs end the stream at
// the finish chunk and ignore anything after it). Not retried because this is
// a COMPLETED stream (the terminal marker was seen - empty_completion), not a
// truncation: only truncated streams are re-sent.
func TestStreamVoidReplacedWithInBandError(t *testing.T) {
	upstream := streamUpstream(t,
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: [DONE]\n\n",
	)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("in-band degenerate error missing: %s", got)
	}
	if strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("the void's finish chunk must be replaced, not relayed: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	// Byte ORDER: the role chunk (relayed before the terminal region) must
	// precede the substituted error chunk.
	if !strings.Contains(got, `"role":"assistant"`) ||
		strings.Index(got, `"role":"assistant"`) > strings.Index(got, `"code":"empty_completion"`) {
		t.Fatalf("role chunk must precede the in-band error: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != metrics.CodeEmptyCompletion {
		t.Fatalf("record must flag the void: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
}

// TestStreamToolCallsWithoutCallsReplaced: finish_reason tool_calls with zero
// tool-call deltas and no answer - the documented broken class.
func TestStreamToolCallsWithoutCallsReplaced(t *testing.T) {
	upstream := streamUpstream(t,
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
		"data: [DONE]\n\n",
	)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeEmptyToolCall+`"`) {
		t.Fatalf("in-band tool-mismatch error missing: %s", got)
	}
	if strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("the lying finish chunk must be replaced: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].ErrorCode != metrics.CodeEmptyToolCall {
		t.Fatalf("record error code = %q", recs[0].ErrorCode)
	}
}

// TestStreamTruncatedAppendsInBandError: a stream that dies at clean EOF
// without any terminator gets an in-band truncated error appended after the
// already-relayed bytes (content can't be un-relayed - the error tells the
// client the answer is incomplete). Content-bearing truncation is NEVER
// proxy-retried: the client already saw generation bytes.
func TestStreamTruncatedAppendsInBandError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"half \"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"content":"half "`) {
		t.Fatalf("relayed content missing: %s", got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("in-band truncated error missing: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (content-bearing truncation is never re-sent)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record error code = %q", recs[0].ErrorCode)
	}
}

// TestStreamTruncatedEmptyRetried: the streaming twin of the non-streaming
// quality retry. A stream that dies truncated BEFORE any content-bearing
// chunk (content, reasoning, tool call) reached the client is transparently
// re-sent on the same committed SSE connection; the client sees one complete
// fresh stream, the record stays a success, and the absorbed attempt is
// logged like every other retry.
func TestStreamTruncatedEmptyRetried(t *testing.T) {
	// Two competing timelines keep the ttfb bound below load-safe: the
	// re-send delays its headers ~60ms (a floor, so its own ttfb is
	// structurally >= 60ms), while the truncated first attempt sits ~400ms
	// out before its headers - any window that wrongly includes the first
	// attempt's timeline (a request-start anchor) lands >= 460ms. The bound
	// at 250ms is the midpoint: the re-send's own header wait passes with
	// ~190ms of scheduler slack, and a wrong anchor fails it by ~200ms.
	const firstAttemptDelay = 400 * time.Millisecond
	const reaskHeaderDelay = 60 * time.Millisecond
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			time.Sleep(firstAttemptDelay)
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			// Truncation before any generation content: role chunk, clean EOF.
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			return
		}
		time.Sleep(reaskHeaderDelay)
		w.Header().Set("X-Request-Id", "req-second")
		w.WriteHeader(200)
		w.Write([]byte(
			"data: {\"id\":\"2\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"data: {\"id\":\"2\",\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n" +
				"data: {\"id\":\"2\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"content":"hello"`) || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("client must see the complete fresh stream: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("recovered request must not surface the truncation error: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (truncated absorbed + retry)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 ||
		rec.Attempts[0].ErrorCode != metrics.CodeTruncated || rec.Attempts[0].StatusCode != 200 {
		t.Fatalf("absorbed attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if rec.FinishReason != "stop" || !rec.HadAnswerContent {
		t.Fatalf("final record must reflect the fresh stream: finish=%q content=%v", rec.FinishReason, rec.HadAnswerContent)
	}
	if rec.ProviderRequestID != "req-second" {
		t.Fatalf("record must carry the re-send's provider metadata, got request id %q", rec.ProviderRequestID)
	}
	// The rescue ACCUMULATES (never resets): the abandoned attempt's role
	// chunk genuinely reached the client and the fresh stream appended
	// behind it, so response_bytes is the whole wire content the client
	// received - not the adopted attempt's frames alone.
	if rec.ResponseBytes != int64(len(got)) {
		t.Errorf("ResponseBytes = %d, want %d (the full appended wire content across the rescue)", rec.ResponseBytes, len(got))
	}
	// The anchor pin: ttft_ms is measured from the re-send's SEND START
	// (FinalAttemptAt, stamped at its admission inside doWithRetry), so the
	// re-send's ~60ms header wait is INSIDE ttft. A headers-arrival stamp
	// would exclude it and leave ttft at the token-only residue (~0ms).
	if rec.TTFTMs < 30 {
		t.Fatalf("TTFTMs = %d, want >= 30 (the fresh attempt's ~60ms header wait is part of its TTFT; a headers-arrival FinalAttemptAt stamp strips it)", rec.TTFTMs)
	}
	if rec.UpstreamTTFBMs < 30 || rec.UpstreamTTFBMs >= 250 {
		t.Errorf("UpstreamTTFBMs = %d, want 30 <= ttfb < 250 (the re-send's own ~60ms header wait, the same anchor ttft_ms measures from; the first attempt's ~400ms timeline and any request-start anchor sit far above the bound)", rec.UpstreamTTFBMs)
	}
}

// TestStreamRetryUpstreamErrorSurfacesCaptured: a re-send that returns a real
// upstream error (>= 400) surfaces it in-band (nothing content-bearing was
// relayed), and the record describes THAT attempt: the provider request id,
// error type/message and rate-limit class all come from the re-send, not the
// absorbed first attempt.
func TestStreamRetryUpstreamErrorSurfacesCaptured(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Truncated-empty stream: absorbed by the quality loop.
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			return
		}
		// Re-send rejected by the provider with a real error.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-second")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.MaxRetries = 0 // the re-send's 429 surfaces instead of retrying
	cfg.QualityRetries = 1
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200 (error must be in-band)", status)
	}
	if !strings.Contains(got, `data: {"error"`) || !strings.Contains(got, "slow down") {
		t.Fatalf("re-send error must surface in-band: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (truncated absorbed + erroring re-send)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.StatusCode != 429 || !rec.HasRateLimit() || rec.IsError() {
		t.Fatalf("record must classify as rate-limited, not error: %+v", rec)
	}
	if rec.ErrorType != "rate_limit_error" || !strings.Contains(rec.ErrorMsg, "slow down") {
		t.Fatalf("record must capture the re-send's error body: %+v", rec)
	}
	if rec.ProviderRequestID != "req-second" {
		t.Fatalf("record must carry the re-send's provider metadata, got request id %q", rec.ProviderRequestID)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].StatusCode != 200 {
		t.Fatalf("absorbed attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestStreamRetryStormRejectionSurfacesInBand: a streaming re-send rejected by
// the storm queue (ErrStormMaxWait) surfaces the storm rejection in-band, and
// the record carries the decided status (429, rate-limit class) with the
// absorbed attempt preserved, identical to the non-streaming twin.
func TestStreamRetryStormRejectionSurfacesInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503) // trips the storm on request 1's single send
			return
		}
		// A truncated-empty stream: absorbed by the quality loop, which
		// then re-sends into the active storm.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"id\":\"2\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := stormTestConfig()
	cfg.MaxRetries = 0      // request 1 surfaces its 503 without any retry
	cfg.StormMaxRetries = 0 // no storm-budget retry either: the 503 surfaces, the storm stays tripped
	cfg.QualityRetries = 1
	// Request 2's first send waits one backoff (15ms <= 20ms max) and passes;
	// the absorb's quality-failure observation doubles the backoff (30ms), so
	// the QUALITY RE-SEND's wait is the one that exceeds the max wait.
	cfg.StormMaxWait = 20 * time.Millisecond
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	// Request 1 trips the storm (503) and surfaces it.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 || calls.Load() != 1 {
		t.Fatalf("storm trip request: status=%d calls=%d", resp.StatusCode, calls.Load())
	}

	// Request 2 (streaming): truncated-empty absorbed, re-send rejected by
	// the storm queue. The upstream serves the truncated stream on call 2;
	// the re-send never reaches it.
	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200 (rejection must be in-band)", status)
	}
	if !strings.Contains(got, `data: {"error"`) || !strings.Contains(got, "error storm protection") {
		t.Fatalf("storm rejection must surface in-band: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (truncated absorbed; re-send never sent)", n)
	}
	recs := waitForRecord(t, buf, 2)
	rej := recs[1]
	if rej.StatusCode != 429 || rej.ErrorType != "storm_queue_full" ||
		!rej.HasRateLimit() || rej.IsError() || rej.Retries != 1 || len(rej.Attempts) != 1 ||
		rej.Attempts[0].StatusCode != 200 {
		t.Fatalf("streaming storm rejection must record the decided 429 with the absorbed attempt: %+v", rej)
	}
}

// TestStreamTruncatedRetryExhaustedSurfacesInBand: with the quality budget
// spent, the truncated stream surfaces the in-band error envelope exactly as
// before (nothing content-bearing was relayed, so the error chunk + [DONE]
// still end the stream cleanly for the client).
func TestStreamTruncatedRetryExhaustedSurfacesInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		calls.Add(1)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(1), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("in-band truncated error missing after exhausted budget: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one absorbed retry + final failure)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated || rec.ErrorType != "upstream_error" {
		t.Fatalf("record must flag the final truncation: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("absorbed attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestStreamTruncatedZeroBudgetKeepsLegacyBehavior: quality_retries=0
// disables the streaming re-send entirely - the truncation surfaces in-band
// after the single attempt, exactly as before this retry existed.
func TestStreamTruncatedZeroBudgetKeepsLegacyBehavior(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		calls.Add(1)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(0), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("in-band truncated error missing: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (zero budget disables the re-send)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must flag the truncation: %+v", recs[0])
	}
}

// TestStreamProviderErrorOnEmptyStreamRetried: a provider-sent in-band error
// in a transient server-availability class (the coralbricks api_error shape)
// arriving before any content-bearing frame is dropped before the wire and
// the SAME request transparently re-sent on the quality budget - the OpenAI
// retry semantics ("please retry") applied at the only wire state where a
// re-send cannot duplicate anything. The client never sees the error frame;
// the attempt log carries the provider's own envelope.
func TestStreamProviderErrorOnEmptyStreamRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte("data: {\"error\":{\"message\":\"Coral Bricks is temporarily unavailable. Please retry.\",\"type\":\"api_error\",\"code\":\"internal_error\"}}\n\n"))
			return
		}
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if strings.Contains(got, "api_error") {
		t.Fatalf("the error frame must be dropped before the wire: %s", got)
	}
	// The fresh attempt's frames relay verbatim, behind the blank line that
	// closed the dropped event (the rescue's pending-event close).
	if got != "\n"+freshAnswerFrames {
		t.Fatalf("the fresh attempt's frames must relay verbatim behind the event close: %q", got)
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
	if at.ErrorType != "api_error" || at.ErrorCode != "internal_error" ||
		at.ErrorMsg != "Coral Bricks is temporarily unavailable. Please retry." {
		t.Fatalf("absorbed attempt must carry the provider's envelope: %+v", at)
	}
}

// TestStreamEngineOverloadedWrappedAsInvalidRequestRetried: a gateway that
// labels a transient capacity failure as invalid_request_error but names the
// real condition in the code (engine_overloaded, "Model busy, retry later")
// is retried through the normal in-band rescue gate. The generic client-fault
// wrapper must not suppress a specific transient capacity code, and the client
// never sees the provider's error frame.
func TestStreamEngineOverloadedWrappedAsInvalidRequestRetried(t *testing.T) {
	var calls atomic.Int32
	errChunk := `data: {"error":{"message":"Model busy, retry later","type":"invalid_request_error","code":"engine_overloaded"}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(errChunk))
			return
		}
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if strings.Contains(got, "engine_overloaded") || strings.Contains(got, "invalid_request_error") {
		t.Fatalf("the wrapped capacity error must be dropped before the wire: %s", got)
	}
	if got != "\n"+freshAnswerFrames {
		t.Fatalf("the fresh attempt's frames must relay verbatim behind the event close: %q", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one re-send)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 || len(rec.Attempts) != 1 {
		t.Fatalf("recovered record: retries=%d attempts=%d isError=%v", rec.Retries, len(rec.Attempts), rec.IsError())
	}
	at := rec.Attempts[0]
	if at.ErrorType != "invalid_request_error" || at.ErrorCode != "engine_overloaded" ||
		at.ErrorMsg != "Model busy, retry later" {
		t.Fatalf("absorbed attempt must carry the provider's envelope: %+v", at)
	}
}

// TestStreamOperatorErrorClassExtensionRetried: the retryable_error_classes
// setting extends the built-in vocabulary - a gateway reporting a transient
// failure under a custom spelling is rescued exactly like a built-in class,
// while the same spelling without the extension relays verbatim.
func TestStreamOperatorErrorClassExtensionRetried(t *testing.T) {
	var calls atomic.Int32
	errChunk := `data: {"error":{"message":"model warming up","type":"model_warming_up","code":"warming"}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(errChunk))
			return
		}
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.RetryableErrorClasses = []string{"model_warming_up"}
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if strings.Contains(got, "model_warming_up") {
		t.Fatalf("the extended-class error frame must be dropped before the wire: %s", got)
	}
	if got != "\n"+freshAnswerFrames {
		t.Fatalf("the fresh attempt's frames must relay verbatim behind the event close: %q", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 || rec.Attempts[0].ErrorType != "model_warming_up" {
		t.Fatalf("recovered record must carry the absorbed extension-class attempt: %+v", rec)
	}

	// Without the extension the same spelling is unknown: fail closed to
	// verbatim relay on the first attempt.
	calls2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		calls.Add(-1) // reuse the counter loosely; separate assertions below
		w.Write([]byte(errChunk))
		w.(http.Flusher).Flush()
	}))
	defer calls2.Close()
	buf2 := metrics.NewBuffer(100)
	srv2 := httptest.NewServer(New(config.Default(), buf2))
	defer srv2.Close()
	_, got2 := streamRequest(t, srv2, calls2)
	if got2 != errChunk {
		t.Fatalf("unknown class must relay verbatim without the extension: %q", got2)
	}
	recs2 := waitForRecord(t, buf2, 1)
	if recs2[0].ErrorType != "model_warming_up" || !recs2[0].IsError() || recs2[0].Retries != 0 {
		t.Fatalf("unknown-class record must carry the provider's error, unrescued: %+v", recs2[0])
	}
}

// TestStreamProviderErrorNonRetryableNotRetried: a provider in-band error in a
// class that is not transient (invalid_request_error) relays verbatim even on
// an empty stream - one call, no synthetic stacking, the record carries the
// provider's own envelope.
func TestStreamProviderErrorNonRetryableNotRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		calls.Add(1)
		w.Write([]byte("data: {\"error\":{\"message\":\"bad request\",\"type\":\"invalid_request_error\",\"code\":\"bad_request\"}}\n\n"))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"type":"invalid_request_error"`) {
		t.Fatalf("provider error must relay verbatim: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("proxy must not stack its own error on the provider's: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (non-retryable class is never re-sent)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorType != "invalid_request_error" || recs[0].Retries != 0 {
		t.Fatalf("record must carry the provider's envelope, unrescued: %+v", recs[0])
	}
}

// TestStreamInBandErrorAfterContentNotRetried: once answer content was
// relayed, a retryable in-band provider error is the client's to retry - a
// re-send would duplicate visible bytes, so the frame relays verbatim and the
// record carries the provider's statement.
func TestStreamInBandErrorAfterContentNotRetried(t *testing.T) {
	var calls atomic.Int32
	contentFrame := `data: {"id":"A","choices":[{"delta":{"content":"partial answer"}}]}` + "\n\n"
	errChunk := `data: {"error":{"message":"Coral Bricks is temporarily unavailable. Please retry.","type":"api_error","code":"internal_error"}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		calls.Add(1)
		w.Write([]byte(contentFrame + errChunk))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if want := contentFrame + errChunk; got != want {
		t.Fatalf("error after relayed content must relay verbatim: got %q want %q", got, want)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (never re-send behind relayed content)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorType != "api_error" || rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Fatalf("record must carry the provider's error, unrescued: %+v", rec)
	}
}
