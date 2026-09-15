package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

const voidBody = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
const realBody = `{"id":"2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}]}`

func newConfigWithQuality(n int) *config.Config {
	cfg := config.Default()
	cfg.QualityRetries = n
	return cfg
}

// TestQualityRetryNonStreamingVoid reproduces the degenerate-200 class on the
// non-streaming path: the first upstream 200 carries an empty completion, the
// proxy absorbs it and transparently retries BEFORE writing anything, and the
// client sees only the healthy second response (established behavior: a plain
// 0-in/0-out non-streaming response can poison client sessions).
func TestQualityRetryNonStreamingVoid(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Each attempt carries its own request id: the record must describe
		// the attempt whose body the client received (captureUpstreamHeaders
		// re-captures after every successful re-send).
		if n == 1 {
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			w.Write([]byte(voidBody))
			return
		}
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
}

type closeCountBody struct {
	io.ReadCloser
	n *atomic.Int32
}

func (c *closeCountBody) Close() error {
	c.n.Add(1)
	return c.ReadCloser.Close()
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

func streamUpstream(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, c := range chunks {
			w.Write([]byte(c))
			w.(http.Flusher).Flush()
		}
	}))
}

func streamRequest(t *testing.T, srv *httptest.Server, upstream *httptest.Server) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

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
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			// Truncation before any generation content: role chunk, clean EOF.
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			return
		}
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
	if got != freshAnswerFrames {
		t.Fatalf("the fresh attempt's frames must relay verbatim: %q", got)
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

// TestStreamRetryClientCancelWhileResendParksIsDisconnect pins
// classifyResendFailure's clientGone arm on the streaming re-send site: the
// LOCAL client cancelling while the quality re-send is parked in WaitSend
// (here: on an operator hold, armed from inside the absorbed attempt's
// handler before the truncating EOF can exist, so the park is guaranteed)
// keeps disconnect semantics - 499, ClientDisconnected, no error marks, the
// absorbed attempt logged - never a 502 upstream_unreachable.
func TestStreamRetryClientCancelWhileResendParksIsDisconnect(t *testing.T) {
	buf := metrics.NewBuffer(100)
	p := New(config.Default(), buf)
	holdArmed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		// Truncated-empty stream: role chunk, then the connection stays
		// open while the handler arms the hold, so the proxy cannot see
		// the truncating EOF before the hold exists.
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		if err := p.scheduler.AddHold(scheduler.Hold{All: true}); err != nil {
			t.Errorf("AddHold: %v", err)
		}
		close(holdArmed)
	}))
	defer upstream.Close()
	srv := httptest.NewServer(p)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-holdArmed
	cancel()
	resp.Body.Close()

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.ClientDisconnected {
		t.Errorf("ClientDisconnected = false, want true (the client cancelled the parked re-send)")
	}
	if rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Errorf("StatusCode = %d, want %d (committed 200 re-stamped by markClientGone)", rec.StatusCode, metrics.StatusClientClosedRequest)
	}
	if rec.IsError() || rec.ErrorType != "" || rec.ErrorCode != "" || rec.ErrorMsg != "" {
		t.Errorf("client cancel must leave no error marks: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Errorf("absorbed truncated attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestNonStreamRetryTransportFailureIs502APIError pins
// classifyResendFailure's transport default on the non-streaming re-send
// site: a re-send that dies on transport after a degenerate 200 was absorbed
// answers the still-open status line with 502 + the api_error JSON envelope,
// and the record carries the 502 + upstream_unreachable stamps.
func TestNonStreamRetryTransportFailureIs502APIError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(voidBody))
			return
		}
		// The re-send dies at the transport level: accept the connection
		// and close it without a response.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.MaxRetries = 0 // the transport failure must surface immediately, not after the retry ladder
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a re-send that died on transport", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"type":"api_error"`) {
		t.Fatalf("502 body must carry the api_error envelope: %s", body)
	}
	if !strings.Contains(string(body), "upstream error: ") {
		t.Fatalf("502 body must carry the transport wrap: %s", body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (void absorbed + failed re-send)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() {
		t.Fatalf("record must flag the failed re-send: %+v", rec)
	}
	if rec.ErrorType != typeUpstreamUnreachable || rec.StatusCode != http.StatusBadGateway {
		t.Fatalf("record stamps = type %q status %d, want upstream_unreachable/502", rec.ErrorType, rec.StatusCode)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != metrics.CodeEmptyCompletion {
		t.Fatalf("absorbed void attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestStreamRetryTransportFailureSurfacesInBand: a re-send that fails at the
// transport level must answer the committed socket IN-BAND, never with a raw
// HTTP error body after event-stream bytes - including the corner where the
// client never asked for streaming and the writer carries no pacer (the
// status line was already committed by applyUpstream before the retry loop).
func TestStreamRetryTransportFailureSurfacesInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
			return
		}
		// The re-send dies at the transport level: accept the connection
		// and close it without a response.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.MaxRetries = 0 // the transport failure must surface immediately, not after the retry ladder
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	// A NON-streaming request whose upstream answered event-stream: the same
	// committed-socket corner with no pacer on the client writer.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(body), "data: {\"error\"") {
		t.Fatalf("relayed socket must get the in-band error envelope, got: %q", string(body))
	}
	if !strings.Contains(string(body), `"type":"upstream_unreachable"`) {
		t.Fatalf("in-band envelope must carry the unreachable type: %q", string(body))
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one absorbed truncation + failed re-send)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorType != "upstream_unreachable" || recs[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("record must flag the failed re-send: %+v", recs[0])
	}
}

// TestStreamHealthyTerminalReleasedVerbatim: the held terminal region (finish
// chunk + usage chunk + [DONE]) is released byte-for-byte on a healthy stream.
func TestStreamHealthyTerminalReleasedVerbatim(t *testing.T) {
	chunks := []string{
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n",
		"data: [DONE]\n\n",
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("healthy stream must relay verbatim:\n got %q\nwant %q", got, want)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].IsError() {
		t.Fatalf("healthy stream flagged as error: %+v", recs[0])
	}
}

// TestStreamProviderErrorNotDoubleSignaled: a provider-sent in-band error (the
// OpenRouter shape: top-level error + finish_reason error) is released
// verbatim - the proxy must not stack its own degenerate signal on top.
func TestStreamProviderErrorNotDoubleSignaled(t *testing.T) {
	errChunk := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"error\"}],\"error\":{\"code\":\"server_error\",\"message\":\"provider died\"}}\n\n"
	upstream := streamUpstream(t,
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		errChunk,
	)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"+errChunk {
		t.Fatalf("provider error stream must relay verbatim: %q", got)
	}
	if strings.Contains(got, `"code":"truncated"`) {
		t.Fatalf("proxy must not double-signal on a provider error: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	// The provider's in-band error → error record; the class must come from
	// the provider's own envelope, not ours.
	if !recs[0].IsError() || recs[0].ErrorCode == metrics.CodeTruncated {
		t.Fatalf("record should carry the provider's error, got code=%q", recs[0].ErrorCode)
	}
}

// TestResponsesAPIStreamHealthy: a Responses-API stream (no [DONE], terminal
// response.completed event) is never misclassified as a void.
func TestResponsesAPIStreamHealthy(t *testing.T) {
	chunks := []string{
		"data: {\"type\":\"response.created\",\"response\":{}}\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hey\"}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":{}}\n\n",
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("responses-API stream must relay verbatim:\n got %q\nwant %q", got, want)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].IsError() {
		t.Fatalf("healthy responses stream flagged as error: %+v", recs[0])
	}
}

// TestStreamTerminalHoldOverflowCommit: a terminal region grown past the
// terminalHoldMax guardrail commits to verbatim relay - the hold is abandoned
// (no degenerate stream has a 64KiB+ "terminal" region), so every byte
// reaches the client exactly.
func TestStreamTerminalHoldOverflowCommit(t *testing.T) {
	bigComment := ":" + strings.Repeat("x", 70*1024) + "\n"
	chunks := []string{
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		bigComment,
		"data: [DONE]\n\n",
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("overflow-abandoned hold must relay verbatim:\n got %d bytes\nwant %d bytes", len(got), len(want))
	}
}

// TestStreamReadErrorMidHoldReleasesVerbatim: a real transport read failure
// after the finish chunk releases the held bytes verbatim and classifies as
// stream_read_error - never substituted with a synthetic degenerate error.
func TestStreamReadErrorMidHoldReleasesVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // abort the connection: the proxy sees a non-EOF read error
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	want := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	if got != want {
		t.Fatalf("read-error path must release the hold verbatim:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, "empty_completion") {
		t.Fatalf("no synthetic degenerate error on a transport failure: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].ErrorType != "stream_read_error" {
		t.Fatalf("record error type = %q, want stream_read_error", recs[0].ErrorType)
	}
}

// TestQualityRetryEmptyBody: a chat request answered with a completely empty
// 200 body is the degenerate class in its purest form - retried transparently.
func TestQualityRetryEmptyBody(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			return // empty body
		}
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

	if resp.StatusCode != 200 || string(body) != realBody {
		t.Fatalf("empty-body retry failed: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs[0].Attempts) != 1 || recs[0].Attempts[0].ErrorType != metrics.CodeEmptyCompletion {
		t.Fatalf("attempts = %+v, want one absorbed empty_completion", recs[0].Attempts)
	}
}

// TestStreamResolvesAtDoneWithoutEOF: the terminal hold resolves the moment
// the [DONE] line arrives - the client's completion is not withheld behind an
// upstream that keeps the connection open after [DONE].
func TestStreamResolvesAtDoneWithoutEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		keepOpen := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			f.Flush()
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
			f.Flush()
			w.Write([]byte("data: [DONE]\n\n"))
			f.Flush()
			<-keepOpen // hold the upstream connection open past [DONE]
		}()
		<-r.Context().Done()
		close(keepOpen) // client closed: release the writer and finish the request
		<-writerDone
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read incrementally with an overall deadline: [DONE] and the finish chunk
	// must arrive while the upstream is still open - well before any EOF.
	ch := make(chan []byte, 32)
	go func() {
		b := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(b)
			if n > 0 {
				ch <- append([]byte(nil), b[:n]...)
			}
			if rerr != nil {
				close(ch)
				return
			}
		}
	}()
	var acc []byte
	deadline := time.After(3 * time.Second)
	satisfied := false
	for !satisfied {
		select {
		case p, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before [DONE] arrived: %q", acc)
			}
			acc = append(acc, p...)
			satisfied = bytes.Contains(acc, []byte("data: [DONE]")) &&
				bytes.Contains(acc, []byte(`"finish_reason":"stop"`))
		case <-deadline:
			t.Fatalf("completion withheld despite [DONE] arriving: %q", acc)
		}
	}
}

// A decoy "error" key (null/empty value - metrics.ParseErrorEnvelope returns
// "" for those, exactly like the SSE analyzer treats them) must NOT ride the
// verbatim in-band branch of the non-streaming 200 path: a void completion
// carrying a sibling "error":null is still a degenerate 200 and must be
// quality-retried, never relayed as a silently-empty success.
func TestQualityRetryDecoyErrorKeyStillClassified(t *testing.T) {
	decoyVoid := `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"error":null}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			w.Write([]byte(decoyVoid))
			return
		}
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

	if resp.StatusCode != 200 || string(body) != realBody {
		t.Fatalf("decoy-key void must be quality-retried: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (decoy absorbed + retry)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered request must not be an error: %+v", rec)
	}
	if rec.ErrorType != "" || rec.ErrorCode != "" || rec.ErrorMsg != "" {
		t.Fatalf("decoy key must leave no error mark: type=%q code=%q msg=%q", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != metrics.CodeEmptyCompletion {
		t.Fatalf("attempts = %+v, want one absorbed %s", rec.Attempts, metrics.CodeEmptyCompletion)
	}
}

// Exhaustion variant: the still-void decoy body surfaces as a real 502 with
// the degenerate code - never a silently-empty 200.
func TestQualityRetryDecoyErrorKeyExhaustionFlags(t *testing.T) {
	decoyVoid := `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"error":null}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(decoyVoid))
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
		t.Fatalf("status = %d, want 502 for a still-degenerate decoy void", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("502 body must carry the degenerate code: %s", body)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorCode != metrics.CodeEmptyCompletion {
		t.Fatalf("record must flag the degenerate outcome: type=%q code=%q", recs[0].ErrorType, recs[0].ErrorCode)
	}
}

// TestExhausted429OversizedBodyRelayedVerbatim: an exhausted-budget 429 whose
// body overflows the error-peek cap (maxErrBodyBytes) must still relay the
// COMPLETE body byte-for-byte - prefix + live remainder - never a truncated
// prefix ("an exhausted budget surfaces the last upstream response verbatim").
func TestExhausted429OversizedBodyRelayedVerbatim(t *testing.T) {
	// Deterministic payload comfortably past the 64 KiB peek cap.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64/16*1024+64) // 64 KiB + 1 KiB
	if len(payload) <= 64*1024 {
		t.Fatalf("test payload must exceed the peek cap: %d", len(payload))
	}
	wantSum := sha256.Sum256(payload)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(payload)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0 // exhaust immediately: the 429 is the final response
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

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 relayed verbatim", resp.StatusCode)
	}
	if len(body) != len(payload) {
		t.Fatalf("relayed body length = %d, want the complete %d bytes (truncated prefix)", len(body), len(payload))
	}
	gotSum := sha256.Sum256(body)
	if gotSum != wantSum {
		t.Fatal("relayed body checksum mismatch: the 429 body was altered/truncated in transit")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (max_retries=0)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].StatusCode != http.StatusTooManyRequests {
		t.Fatalf("record status = %d, want 429", recs[0].StatusCode)
	}
}

// failingSSEWriter is a ResponseWriter whose socket is already dead: every
// write fails, like a LOCAL client that disconnected mid-stream.
type failingSSEWriter struct{}

func (failingSSEWriter) Header() http.Header       { return http.Header{} }
func (failingSSEWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }
func (failingSSEWriter) WriteHeader(int)           {}

// TestEmitErrorSSEWriteFailureMarksCursorDisconnect pins the accounting for
// in-band cursor error emission onto a dead socket: the writes report their
// failure and the record is marked client-disconnected - WITHOUT the
// passthrough paths' 499 (cursor's documented exception: the committed
// upstream 200 stays the status) and WITHOUT clearing the upstream error
// fields (the failure itself is still recorded). Regression: emitErrorSSE
// discarded its write errors, so a client that disconnected during in-band
// error emission was accounted as if it had received the failure.
func TestEmitErrorSSEWriteFailureMarksCursorDisconnect(t *testing.T) {
	// The emitter itself must report the dead socket.
	if err := emitErrorSSE(failingSSEWriter{}, "id-1", "stream_read_error", "boom"); err == nil {
		t.Fatal("emitErrorSSE must return the failed client write")
	}

	// The full turn path: a TurnErrored (non-abort) result rendered to a
	// dead socket marks the disconnect and keeps both the 200 and the
	// upstream error fields.
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	res := providerformat.TurnResult{
		Outcome: providerformat.TurnErrored,
		Err:     fmt.Errorf("upstream died mid-turn"),
		Output:  3,
	}
	s.finishRunTurn(failingSSEWriter{}, run, res, func(map[string]any, any) error { return nil }, rec, "id-1", cursorTurnRender{est: 10}, false)

	if !rec.ClientDisconnected {
		t.Error("ClientDisconnected = false, want true (the in-band error hit a dead socket)")
	}
	if rec.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (cursor's documented exception: never markClientGone's 499)", rec.StatusCode)
	}
	if rec.ErrorType != "stream_read_error" {
		t.Errorf("ErrorType = %q, want stream_read_error (the upstream failure is still recorded)", rec.ErrorType)
	}
}

// TestWriteRunJSONErrorWriteFailureMarksCursorDisconnect is the non-streaming
// twin of the in-band SSE case above: writeRunJSON surfaces a failed turn and
// the void as HTTP 502 error bodies, and those writes must detect a dead
// client socket. Regression: the error paths went through http.Error, which
// discards its write error, so a client that disconnected just as the error
// JSON was written was never marked ClientDisconnected - accounted as if it
// had received the failure. The disconnect keeps cursor's documented
// exception (the committed upstream 200 stays the status) and keeps the
// error fields (the failure itself is still recorded).
func TestWriteRunJSONErrorWriteFailureMarksCursorDisconnect(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	newRun := func() *providerformat.CursorRun {
		pr, pw := io.Pipe()
		return providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	}

	// Transform-failure branch (a turn that errored, not a client abort):
	// writeRunJSON stamps the error and writes a 502 error body.
	rec := &metrics.Record{StatusCode: 200}
	res := providerformat.TurnResult{
		Outcome: providerformat.TurnErrored,
		Err:     fmt.Errorf("transform boom"),
		Output:  3,
	}
	s.writeRunJSON(failingSSEWriter{}, newRun(), res, "", nil, rec, "id-1", cursorTurnRender{est: 10}, false)
	if !rec.ClientDisconnected {
		t.Error("ClientDisconnected = false, want true (the 502 error body hit a dead socket)")
	}
	if rec.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (cursor's documented exception: never markClientGone's 499)", rec.StatusCode)
	}
	if rec.ErrorType != "transform_error" {
		t.Errorf("ErrorType = %q, want transform_error (the failure is still recorded)", rec.ErrorType)
	}

	// Void branch (an empty resume-action continuation surfaces a 502).
	rec2 := &metrics.Record{StatusCode: 200}
	res2 := providerformat.TurnResult{Outcome: providerformat.TurnFinished}
	s.writeRunJSON(failingSSEWriter{}, newRun(), res2, "", nil, rec2, "id-2", cursorTurnRender{est: 10}, true)
	if !rec2.ClientDisconnected {
		t.Error("ClientDisconnected = false, want true (the void 502 body hit a dead socket)")
	}
	if rec2.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (cursor's documented exception)", rec2.StatusCode)
	}
	if rec2.ErrorType != "empty_turn" {
		t.Errorf("ErrorType = %q, want empty_turn (the void is still recorded)", rec2.ErrorType)
	}
}

// TestWriteRunJSONVoidBodyBytes pins the void's non-streaming wire contract
// byte-exact: a void (an empty resume-action continuation that finished
// with zero output tokens) must surface a real 502 whose body is the
// canonical empty_turn error JSON - the same message text the record and
// the in-band SSE path carry - followed by emitHTTPError's newline, with
// emitHTTPError's error headers (plain text, nosniff). The disconnect
// twin above drives this arm through a dead socket, so nothing pinned the
// body bytes a live non-streaming client actually receives.
func TestWriteRunJSONVoidBodyBytes(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	rec := &metrics.Record{StatusCode: 200}
	res := providerformat.TurnResult{Outcome: providerformat.TurnFinished}
	w := httptest.NewRecorder()
	s.writeRunJSON(w, run, res, "", nil, rec, "id-1", cursorTurnRender{est: 10}, true)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the void must surface a real failure)", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8 (emitHTTPError's plain-text error header)", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff (emitHTTPError's nosniff guard)", got)
	}
	want := `{"error":{"message":"cursor returned an empty turn for a resume-action continuation (0 output tokens, finish stop) - there was nothing to resume upstream","type":"empty_turn"}}` + "\n"
	if got := w.Body.String(); got != want {
		t.Fatalf("void 502 body = %q, want the canonical empty_turn error JSON", got)
	}
}

// Thinking-rescue fixtures: the reasoning-only delta frame providers stream
// (DeepSeek reasoning_content and its spellings), the degenerate reasoning-only
// stop, and a healthy fresh stream the rescue re-send appends.
const thinkingRoleFrame = `data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
const thinkingFrame = `data: {"id":"A","choices":[{"delta":{"reasoning_content":"thinking hard"}}]}` + "\n\n"
const thinkingStopFrames = `data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"
const freshAnswerFrames = `data: {"id":"B","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
	`data: {"id":"B","choices":[{"delta":{"content":"hello"}}]}` + "\n\n" +
	`data: {"id":"B","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"

const thinkingOnlyBody = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"thought hard"},"finish_reason":"stop"}]}`

// TestStreamThinkingTruncatedRescued: a stream that dies mid-thinking (clean
// EOF, no terminal marker, only reasoning relayed) is transparently re-sent
// on the thinking_retries budget: the client keeps the attempt-1 reasoning
// (auxiliary display text) and receives the fresh stream's answer appended
// behind it, the record stays a success, and the absorbed attempt is logged
// as the dashboard's correction flag with the provider-error view.
func TestStreamThinkingTruncatedRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(thinkingRoleFrame))
			w.(http.Flusher).Flush()
			w.Write([]byte(thinkingFrame))
			w.(http.Flusher).Flush()
			return
		}
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200", status)
	}
	if !strings.Contains(got, `"reasoning_content":"thinking hard"`) {
		t.Fatalf("attempt-1 reasoning must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"content":"hello"`) || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("client must receive the fresh stream's answer: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("rescued request must not surface the truncation: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (mid-thinking truncation absorbed + rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("rescued request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 ||
		rec.Attempts[0].ErrorType != "upstream_error" || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("absorbed attempt not logged as the provider-error correction: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if rec.FinishReason != "stop" || !rec.HadAnswerContent {
		t.Fatalf("final record must reflect the fresh stream: finish=%q content=%v", rec.FinishReason, rec.HadAnswerContent)
	}
}

// TestStreamThinkingStopRescued: a cleanly finished reasoning-only stream (the
// model thought, stopped, never answered) is rescued while its terminal region
// is still withheld - the client never sees attempt 1's finish chunk, the
// re-send's own terminal region takes its place, and the fresh answer lands
// behind the already-relayed reasoning.
func TestStreamThinkingStopRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(thinkingRoleFrame + thinkingFrame + thinkingStopFrames))
			w.(http.Flusher).Flush()
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
	if !strings.Contains(got, `"reasoning_content":"thinking hard"`) {
		t.Fatalf("attempt-1 reasoning must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("client must receive the fresh stream's answer: %s", got)
	}
	// The withheld void must never surface: exactly one finish chunk (the
	// fresh attempt's) and exactly one [DONE].
	if strings.Count(got, `"finish_reason":"stop"`) != 1 {
		t.Fatalf("exactly one finish chunk expected (the void's was withheld): %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("rescued request must not surface the degenerate error: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (reasoning-only stop absorbed + rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("rescued request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 ||
		rec.Attempts[0].ErrorType != "upstream_error" || rec.Attempts[0].ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("absorbed attempt not logged as the provider-error correction: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestStreamThinkingStopExhaustedSurfacesProviderError: with the thinking
// budget disabled (thinking_retries: 0), a reasoning-only stop surfaces the
// withheld terminal region's replacement in-band, and the record classifies
// the outcome as a provider error (upstream_error / reasoning_only) so the
// health counts and the error explorer group it with provider failures.
func TestStreamThinkingStopExhaustedSurfacesProviderError(t *testing.T) {
	upstream := streamUpstream(t, thinkingRoleFrame+thinkingFrame+thinkingStopFrames)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.ThinkingRetries = 0
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200 (error must be in-band)", status)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("in-band reasoning_only error missing: %s", got)
	}
	if strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("the void's finish chunk must be replaced, not relayed: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() {
		t.Fatalf("an exhausted reasoning-only rescue is a genuine provider failure: %+v", rec)
	}
	if rec.ErrorType != "upstream_error" || rec.ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("record must classify as provider error: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
}

// TestStreamThinkingLengthNeverRescued: finish_reason length is the client's
// own token cap - a reasoning-only length finish relays verbatim, is never
// re-sent, and never classifies as degenerate.
func TestStreamThinkingLengthNeverRescued(t *testing.T) {
	upstream := streamUpstream(t,
		thinkingRoleFrame+thinkingFrame+
			`data: {"id":"A","choices":[{"delta":{},"finish_reason":"length"}]}`+"\n\n"+
			`data: [DONE]`+"\n\n")
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"finish_reason":"length"`) {
		t.Fatalf("the length finish must relay verbatim: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("a length finish is never a degenerate class: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.FinishReason != "length" {
		t.Fatalf("length is a clean provider signal: %+v", rec)
	}
}

// TestStreamThinkingWithAnswerNeverRescued: once answer content reached the
// client, no rescue may append a second generation - a truncation after the
// answer began stays client-retryable (in-band error), even when reasoning
// preceded the answer.
func TestStreamThinkingWithAnswerNeverRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(thinkingRoleFrame + thinkingFrame +
			`data: {"id":"A","choices":[{"delta":{"content":"half "}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("in-band truncated error missing: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a rescue after answer content is forbidden)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record error code = %q, want truncated", recs[0].ErrorCode)
	}
}

// TestStreamThinkingResetRescued: a mid-thinking connection reset (the
// upstream dropping the socket, not our own deadline) is the same rescuable
// death as a clean EOF - "dies off mid-thinking for whatever reason".
func TestStreamThinkingResetRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.WriteHeader(200)
			w.Write([]byte(thinkingRoleFrame))
			w.(http.Flusher).Flush()
			w.Write([]byte(thinkingFrame))
			w.(http.Flusher).Flush()
			// Drop the connection mid-thinking: the proxy sees a read error,
			// not a clean EOF.
			panic("simulated upstream crash mid-thinking")
		}
		w.WriteHeader(200)
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200", status)
	}
	if !strings.Contains(got, `"reasoning_content":"thinking hard"`) {
		t.Fatalf("attempt-1 reasoning must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("client must receive the fresh stream's answer: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the reset must be absorbed by the rescue, not surfaced: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (mid-thinking reset absorbed + rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("rescued request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("absorbed reset not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestNonStreamReasoningOnlyRescued: the non-streaming twin - a
// reasoning-only 200 body (DeepSeek message.reasoning_content, no answer)
// is absorbed and re-run on the thinking budget before the status line, and
// the client sees only the healthy second body.
func TestNonStreamReasoningOnlyRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(thinkingOnlyBody))
			return
		}
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

	if resp.StatusCode != 200 || string(body) != realBody {
		t.Fatalf("client must see only the healthy body: status=%d body=%q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (reasoning-only absorbed + rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("rescued request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 ||
		rec.Attempts[0].ErrorType != "upstream_error" || rec.Attempts[0].ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("absorbed attempt not logged as the provider-error correction: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestNonStreamReasoningOnlyExhaustedSurfacesProviderError: past the thinking
// budget the reasoning-only body surfaces as a real 502 carrying the
// reasoning_only code, and the record classifies it as a provider error
// (upstream_error / reasoning_only), matching the streaming twin.
func TestNonStreamReasoningOnlyExhaustedSurfacesProviderError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(thinkingOnlyBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.ThinkingRetries = 1 // one rescue, then the second reasoning-only body exhausts it
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

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (a reasoning-only body is a failure, never a silent success)", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("502 body must carry the reasoning_only code: %s", body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (absorbed rescue + exhausted re-run)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() {
		t.Fatalf("exhausted reasoning-only rescue is a genuine provider failure: %+v", rec)
	}
	if rec.ErrorType != "upstream_error" || rec.ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("record must classify as provider error: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 ||
		rec.Attempts[0].ErrorType != "upstream_error" || rec.Attempts[0].ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("absorbed rescue not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestStreamThinkingDeadlineMidThinkingNotRescued: the per-send deadline
// (X-Proxy-Timeout-Ms) lives on a child context, so the outer request context
// stays clean when it kills the body read - the rescue must still refuse: a
// re-send would mint a fresh deadline the client explicitly did not ask for.
// The stalled read surfaces as stream_read_error, one upstream call.
func TestStreamThinkingDeadlineMidThinkingNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(thinkingRoleFrame + thinkingFrame))
		w.(http.Flusher).Flush()
		// Stall mid-thinking until the proxy's per-send deadline kills us.
		<-r.Context().Done()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Timeout-Ms", "300")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if !strings.Contains(string(got), `"reasoning_content":"thinking hard"`) {
		t.Fatalf("attempt-1 reasoning must stay on the wire: %s", got)
	}
	if strings.Contains(string(got), `"content":"hello"`) {
		t.Fatalf("a deadline-killed read must never be rescued: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (deadline death is never re-sent)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ErrorType != "stream_read_error" || rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Fatalf("deadline death must surface as the transport's own failure: %+v", rec)
	}
}

// TestStreamThinkingClientGoneNotRescued: a client abort mid-thinking
// cancels the outer request context, and the rescue must refuse - the
// re-send would go to a socket nobody reads.
func TestStreamThinkingClientGoneNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(thinkingRoleFrame + thinkingFrame))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read until the reasoning frame lands, then abort.
	r := bufio.NewReader(resp.Body)
	for {
		line, rerr := r.ReadString('\n')
		if rerr != nil {
			break
		}
		if strings.Contains(line, "thinking hard") {
			break
		}
	}
	cancel()
	resp.Body.Close()

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (client-gone death is never rescued)", n)
	}
	if !rec.ClientDisconnected || rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Fatalf("client abort must book as client_disconnected with no rescue: %+v", rec)
	}
	if rec.IsError() {
		t.Fatalf("the local client's own cancellation is never an error: %+v", rec)
	}
}

// TestStreamThinkingStopNoDoneRescued: the stop rescue also fires when the
// finish chunk arrives but the upstream closes without ever sending [DONE] -
// the withheld terminal region is resolved as a rescue at EOF.
func TestStreamThinkingStopNoDoneRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(thinkingRoleFrame + thinkingFrame +
				`data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
			w.(http.Flusher).Flush()
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
	if !strings.Contains(got, `"reasoning_content":"thinking hard"`) {
		t.Fatalf("attempt-1 reasoning must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("client must receive the fresh stream's answer: %s", got)
	}
	if strings.Count(got, `"finish_reason":"stop"`) != 1 || strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one finish chunk and one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (withheld stop absorbed + rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 ||
		len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("rescued no-[DONE] stop not booked correctly: %+v", rec)
	}
}

// TestStreamThinkingRefusalThenStopNotRescued: a refusal delta is answer-shaped
// output the client saw - even after reasoning, the reasoning-only stop is
// never re-sent; it surfaces the in-band provider error instead.
func TestStreamThinkingRefusalThenStopNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(thinkingRoleFrame + thinkingFrame +
			`data: {"id":"A","choices":[{"delta":{"refusal":"I cannot help with that"}}]}` + "\n\n" +
			thinkingStopFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"refusal":"I cannot help with that"`) {
		t.Fatalf("the refusal must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("the reasoning-only stop must surface in-band: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (never rescued after a refusal)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorType != "upstream_error" || rec.ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("record must classify as the surfaced provider error: %+v", rec)
	}
}

// TestStreamThinkingResponsesShapeNotRescued: a Responses-API-shaped stream
// (typed response.* events carrying per-response sequence numbers) is never
// appended to, even when it classifies reasoning_only - the withheld terminal
// event surfaces in-band instead, and the client sees exactly one
// response.created.
func TestStreamThinkingResponsesShapeNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(
			`data: {"type":"response.created","response":{"id":"r1"},"sequence_number":1}` + "\n\n" +
				`data: {"type":"response.reasoning_text.delta","delta":"thinking","sequence_number":2}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"r1"},"sequence_number":3}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if strings.Count(got, `"response.created"`) != 1 {
		t.Fatalf("exactly one response.created expected (no appended restart): %s", got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("the withheld terminal event must surface in-band: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (Responses-shaped feeds are never rescued)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != metrics.CodeReasoningOnly {
		t.Fatalf("record must classify the surfaced reasoning_only error: %+v", rec)
	}
}

// TestStreamPostDoneFinishMutationNotRescued: once a finishHold released the
// terminal region, no later mutation (a rogue post-[DONE] finish chunk) can
// trigger a rescue - the outcome was already signed on the wire.
func TestStreamPostDoneFinishMutationNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(thinkingRoleFrame + thinkingFrame +
			`data: {"id":"A","choices":[{"delta":{},"finish_reason":"length"}]}` + "\n\n" +
			`data: [DONE]` + "\n\n" +
			`data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"finish_reason":"length"`) {
		t.Fatalf("the released length finish must stay on the wire: %s", got)
	}
	if strings.Contains(got, `"content":"hello"`) {
		t.Fatalf("no second generation may ever be appended: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (never rescued after the hold was released)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Fatalf("no rescue may book for a released hold: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestStreamRefusalOnlyTruncationNotRescued: the empty-truncation rescue
// (quality budget) equally refuses to append behind answer-shaped bytes - a
// refusal-only stream that dies truncated stays client-retryable.
func TestStreamRefusalOnlyTruncationNotRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(
			`data: {"id":"A","choices":[{"delta":{"refusal":"I cannot help with that"}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"refusal":"I cannot help with that"`) {
		t.Fatalf("the refusal must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("in-band truncated error missing: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (refusal bytes are never appended to)", n)
	}
}

// TestStreamInBandErrorAfterReasoningRescuedOnThinkingBudget: a retryable
// in-band provider error arriving after reasoning-only output is rescued on
// the thinking budget - reasoning frames are auxiliary display text the
// fresh attempt appends behind, and the error frame itself never reached the
// wire (the drop happens before its write, so the client sees only the
// reasoning followed by the fresh attempt's answer).
func TestStreamInBandErrorAfterReasoningRescuedOnThinkingBudget(t *testing.T) {
	var calls atomic.Int32
	errChunk := `data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(thinkingFrame + errChunk))
			w.(http.Flusher).Flush()
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
	if want := thinkingFrame + freshAnswerFrames; got != want {
		t.Fatalf("reasoning then fresh answer must relay, error frame dropped: got %q want %q", got, want)
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
	if at.ErrorType != "overloaded_error" || at.ErrorCode != "overloaded" {
		t.Fatalf("absorbed attempt must carry the provider's envelope: %+v", at)
	}
}

// TestStreamInBandErrorAfterReasoningZeroBudgetVerbatim: with the thinking
// budget spent (0) the same stream relays the provider's error verbatim after
// the reasoning - never substituted on top of, and the record carries it.
func TestStreamInBandErrorAfterReasoningZeroBudgetVerbatim(t *testing.T) {
	errChunk := `data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}` + "\n\n"
	upstream := streamUpstream(t, thinkingFrame+errChunk)
	defer upstream.Close()

	cfg := config.Default()
	cfg.ThinkingRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if want := thinkingFrame + errChunk; got != want {
		t.Fatalf("zero budget must relay the provider error verbatim: got %q want %q", got, want)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorType != "overloaded_error" || rec.ErrorCode != "overloaded" || rec.Retries != 0 {
		t.Fatalf("record must carry the provider's own error, unrescued: %+v", rec)
	}
}

// TestStreamTerminalHoldOverflowReasoningOnlyCommitsVerbatim: past the hold
// bound the stream commits to verbatim relay - a reasoning-only stream with
// an oversized terminal region is never rescued and never substituted; the
// finish chunk itself reaches the client.
func TestStreamTerminalHoldOverflowReasoningOnlyCommitsVerbatim(t *testing.T) {
	chunks := []string{
		thinkingFrame,
		`data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: ` + `{"pad":"` + strings.Repeat("x", 70*1024) + `"}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("overflowed reasoning-only stream must relay byte-for-byte")
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Fatalf("commit-to-verbatim stream must stay a clean record: %+v", rec)
	}
}

// TestStreamMixedBudgetRescues: the two rescue budgets are independent pools -
// a request may consume the thinking budget first and still be rescued by
// the quality budget afterwards, in either order, and the fresh attempt
// finally completes the stream.
func TestStreamMixedBudgetRescues(t *testing.T) {
	for _, order := range []string{"thinking-first", "quality-first"} {
		t.Run(order, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				n := calls.Add(1)
				// Attempt 3 always completes; attempts 1 and 2 are the two
				// truncation flavors in the order under test.
				if n == 3 {
					w.Write([]byte(freshAnswerFrames))
					w.(http.Flusher).Flush()
					return
				}
				thinkingDeath := n == 1 && order == "thinking-first" || n == 2 && order == "quality-first"
				if thinkingDeath {
					w.Write([]byte(thinkingFrame))
					w.(http.Flusher).Flush()
					return
				}
				w.Write([]byte(`data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			_, got := streamRequest(t, srv, upstream)
			if !strings.Contains(got, `"content":"hello"`) {
				t.Fatalf("client must receive the fresh stream's answer: %s", got)
			}
			if strings.Contains(got, `"code":"`) {
				t.Fatalf("a fully rescued request must surface no error: %s", got)
			}
			if n := calls.Load(); n != 3 {
				t.Fatalf("upstream calls = %d, want 3 (two absorbed rescues + final)", n)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.IsError() || rec.Retries != 2 || len(rec.Attempts) != 2 {
				t.Fatalf("both absorbed rescues must book: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
			}
			wantCodes := []string{metrics.CodeTruncated, metrics.CodeTruncated}
			for i, a := range rec.Attempts {
				if a.ErrorCode != wantCodes[i] || a.ErrorType != "upstream_error" {
					t.Fatalf("attempt %d not booked as provider error: %+v", i, a)
				}
			}
		})
	}
}

// TestNonStreamMixedBudgetRescues: the non-streaming twin - the two budgets
// are independent pools in either order, and the client sees only the final
// healthy body.
func TestNonStreamMixedBudgetRescues(t *testing.T) {
	for _, order := range []string{"thinking-first", "quality-first"} {
		t.Run(order, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				n := calls.Add(1)
				if n == 3 {
					w.Write([]byte(realBody))
					return
				}
				if (n == 1) == (order == "thinking-first") {
					w.Write([]byte(thinkingOnlyBody))
					return
				}
				w.Write([]byte(voidBody))
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

			if resp.StatusCode != 200 || string(body) != realBody {
				t.Fatalf("client must see only the healthy body: status=%d body=%q", resp.StatusCode, body)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.IsError() || rec.Retries != 2 || len(rec.Attempts) != 2 {
				t.Fatalf("both absorbed attempts must book: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
			}
			first, second := rec.Attempts[0], rec.Attempts[1]
			if order == "quality-first" {
				first, second = second, first
			}
			if first.ErrorCode != metrics.CodeReasoningOnly || first.ErrorType != "upstream_error" ||
				second.ErrorType != metrics.CodeEmptyCompletion {
				t.Fatalf("attempts not booked per class in %s order: %+v", order, rec.Attempts)
			}
		})
	}
}

// TestNonStreamReasoningOnlyThinkingZeroSurfaces502: with the thinking budget
// disabled but quality retries enabled, a reasoning-only body must NOT be
// re-run on the quality budget - the class owns the thinking budget alone.
func TestNonStreamReasoningOnlyThinkingZeroSurfaces502(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(thinkingOnlyBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.ThinkingRetries = 0
	cfg.QualityRetries = 1
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

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (thinking budget disabled)", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("502 body must carry the reasoning_only code: %s", body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (quality budget must not rescue the thinking class)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ErrorType != "upstream_error" || rec.ErrorCode != metrics.CodeReasoningOnly || rec.Retries != 0 {
		t.Fatalf("record must classify as the surfaced provider error, unretried: %+v", rec)
	}
}
