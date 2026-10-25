package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

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

// TestStreamThinkingRefusalThenStopDeliveredNotRescued: a refusal delta is
// answer-shaped output the client saw. Even after reasoning it is delivered
// contract output, so the stop is healthy (never reasoning_only), the terminal
// region relays verbatim, and nothing is re-sent behind the refusal.
func TestStreamThinkingRefusalThenStopDeliveredNotRescued(t *testing.T) {
	var calls atomic.Int32
	chunks := []string{
		thinkingRoleFrame,
		thinkingFrame,
		`data: {"id":"A","choices":[{"delta":{"refusal":"I cannot help with that"}}]}` + "\n\n",
		thinkingStopFrames,
	}
	want := strings.Join(chunks, "")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(want))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("reasoning + refusal + stop must relay verbatim:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, `"error"`) {
		t.Fatalf("a delivered refusal must not be rewritten as an error: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (never rescued after a refusal)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.ErrorType != "" || rec.FinishReason != "stop" {
		t.Fatalf("record must carry the delivered refusal, not an error: %+v", rec)
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
