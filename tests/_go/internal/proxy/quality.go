package proxy

import (
	"bytes"
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
		w.WriteHeader(200)
		if n == 1 {
			w.Write([]byte(voidBody))
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

// TestQualityRetryNeverRetriesInBandError: a 200 with a top-level error
// envelope is the provider's own failure statement - relayed verbatim, never
// quality-retried.
func TestQualityRetryNeverRetriesInBandError(t *testing.T) {
	var calls atomic.Int32
	errBody := `{"error":{"message":"overloaded","type":"overloaded_error","code":"overload"}}`
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
// the finish chunk and ignore anything after it). No transparent retry on the
// streaming path: bytes (role chunk) already reached the client.
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
// client the answer is incomplete).
func TestStreamTruncatedAppendsInBandError(t *testing.T) {
	upstream := streamUpstream(t,
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"half \"}}]}\n\n",
	)
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
	recs := waitForRecord(t, buf, 1)
	if recs[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record error code = %q", recs[0].ErrorCode)
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
