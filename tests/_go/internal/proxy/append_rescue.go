package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The append class (config.AppendRetries) is the only rescue whose wire is
// NOT clean: a truncation that died after answer content, tool-call deltas or
// a refusal reached the client, re-sent with the fresh attempt APPENDING
// behind the relayed prefix because a delivered SSE delta cannot be retracted.
// These tests pin the class boundary (deny by default, disjoint from the
// quality and thinking budgets) and the accepted duplicate-prefix cost.

// The exact production shape that motivated the class: answer content and a
// tool call relayed, then the upstream stops with no completion marker.
const appendPrefixFrames = `data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
	`data: {"id":"A","choices":[{"delta":{"content":"running it"}}]}` + "\n\n" +
	`data: {"id":"A","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{\"command\":\"ls\""}}]}}]}` + "\n\n"

// TestStreamTruncatedAfterToolCallNotRescuedByDefault is the deny-by-default
// proof for the class that matters most: with the shipped default
// (append_retries 0) a stream that dies truncated after a tool-call delta is
// NOT re-sent. The client keeps the provider's single attempt and gets the
// in-band truncation error, exactly as before this class existed.
func TestStreamTruncatedAfterToolCallNotRescuedByDefault(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(appendPrefixFrames))
		w.(http.Flusher).Flush()
		// Clean EOF, no finish_reason, no [DONE]: the stream was truncated
		// after its tool call was already relayed.
	}))
	defer upstream.Close()

	if got := config.Default().AppendRetries; got != 0 {
		t.Fatalf("default AppendRetries = %d, want 0 (the class is opt-in)", got)
	}
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"function":{"name":"bash"`) {
		t.Fatalf("attempt-1 tool call must stay on the wire: %s", got)
	}
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("truncation must surface in-band when the class is off: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (append_retries defaults to 0)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation class: %+v", recs[0])
	}
	if recs[0].Retries != 0 {
		t.Fatalf("retries = %d, want 0", recs[0].Retries)
	}
}

// TestStreamTruncatedAfterContentAppendRescued: the opt-in path. A stream
// truncated after answer content and a tool-call delta is absorbed and
// re-sent; the fresh attempt's frames append behind the relayed prefix, the
// client sees one [DONE] and no truncation error, the record is a success
// carrying only the re-send's outcome, and the absorbed attempt is logged
// like every other retry.
func TestStreamTruncatedAfterContentAppendRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.Header().Set("X-Request-Id", "req-first")
			w.WriteHeader(200)
			w.Write([]byte(appendPrefixFrames))
			w.(http.Flusher).Flush()
			return
		}
		w.Header().Set("X-Request-Id", "req-second")
		w.WriteHeader(200)
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200", status)
	}
	// The documented cost: the relayed prefix is NOT retracted.
	if !strings.Contains(got, `"content":"running it"`) {
		t.Fatalf("attempt-1 content must remain on the wire: %s", got)
	}
	if !strings.Contains(got, `"content":"hello"`) || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("client must receive the fresh stream's answer: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("recovered request must not surface the truncation: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] expected: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (truncated absorbed + append rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 ||
		rec.Attempts[0].ErrorType != "upstream_error" ||
		rec.Attempts[0].ErrorCode != metrics.CodeTruncated ||
		rec.Attempts[0].StatusCode != 200 {
		t.Fatalf("absorbed attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if rec.FinishReason != "stop" || !rec.HadAnswerContent {
		t.Fatalf("record must reflect the fresh stream: finish=%q content=%v", rec.FinishReason, rec.HadAnswerContent)
	}
	if rec.ProviderRequestID != "req-second" {
		t.Fatalf("record must carry the re-send's provider metadata, got %q", rec.ProviderRequestID)
	}
}

// TestAppendRetriesExhaustedSurfacesInBand: a spent budget surfaces the
// truncation in-band again, so the class can never loop. The absorbed and
// final attempts are both visible on the record.
func TestAppendRetriesExhaustedSurfacesInBand(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(appendPrefixFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("exhausted budget must surface the truncation in-band: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one rescue, then exhausted)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != metrics.CodeTruncated {
		t.Fatalf("record must carry the truncation class: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 {
		t.Fatalf("exhausted budget must still log the absorbed attempt: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
}

// TestAppendRetriesDoesNotStealThinkingBudget pins that enabling the append
// class does not change any existing rescue: a reasoning-only truncation is
// still absorbed once by the thinking class, and the client outcome is
// identical. The budget disjointness itself (which class claims a stream) is
// pinned at the analyzer in TestAnalyzerRetryableAppendTruncation.
func TestAppendRetriesDoesNotStealThinkingBudget(t *testing.T) {
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

	cfg := config.Default()
	cfg.AppendRetries = 3
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (one thinking rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("rescued request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("absorbed attempt not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if !strings.Contains(got, `"reasoning_content":"thinking hard"`) {
		t.Fatalf("attempt-1 reasoning must stay on the wire: %s", got)
	}
}

// TestAppendRetriesIgnoresFinishedStream: a cleanly terminated stream is not a
// truncation, so the class never applies. A well-formed completion that
// happened to relay a tool call is relayed verbatim, never re-sent.
func TestAppendRetriesIgnoresFinishedStream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(`data: {"id":"1","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"bash","arguments":"{}"}}]}}]}` + "\n\n" +
			`data: {"id":"1","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
			"data: [DONE]\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 3
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatalf("completed stream must relay verbatim: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a terminator is never a truncation)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].IsError() {
		t.Fatalf("completed stream must not be an error: %+v", recs[0])
	}
}

// TestAppendRetriesIgnoresProviderInBandError: a provider in-band error is
// its own class (rescueUpstreamError, gated on the retryable vocabulary). The
// append class must not rescue it, and a non-retryable provider error stays
// verbatim even with append budget available.
func TestAppendRetriesIgnoresProviderInBandError(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(`data: {"id":"1","choices":[{"delta":{"content":"partial"}}]}` + "\n\n"))
		w.(http.Flusher).Flush()
		w.Write([]byte(`data: {"error":{"message":"bad key","type":"authentication_error","code":"invalid_api_key"}}` + "\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 3
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, "invalid_api_key") {
		t.Fatalf("the provider's own error must relay verbatim: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a provider error is not this class)", n)
	}
}

// TestStreamTruncatedAfterContentResetAppendRescued covers the read-error
// twin: a connection dropped after answer content is the same truncation as a
// clean EOF, so the class covers both. The absorbed reset must not be stamped
// as the transport's own error, and the recovery must be identical.
func TestStreamTruncatedAfterContentResetAppendRescued(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			w.WriteHeader(200)
			w.Write([]byte(appendPrefixFrames))
			w.(http.Flusher).Flush()
			// Drop the connection after the content/tool call reached the
			// client: the proxy sees a read error, not a clean EOF.
			panic("simulated upstream crash after content")
		}
		w.WriteHeader(200)
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 1
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, got := streamRequest(t, srv, upstream)
	if status != 200 {
		t.Fatalf("stream status = %d, want 200", status)
	}
	if !strings.Contains(got, `"content":"running it"`) {
		t.Fatalf("attempt-1 content must remain on the wire: %s", got)
	}
	if !strings.Contains(got, `"content":"hello"`) || !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("client must receive the fresh stream's answer: %s", got)
	}
	if strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("the reset must be absorbed by the rescue, not surfaced: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (reset absorbed + append rescue)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("rescued request must not be an error: %+v", rec)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorCode != metrics.CodeTruncated {
		t.Fatalf("absorbed reset not logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if rec.FinishReason != "stop" || !rec.HadAnswerContent {
		t.Fatalf("record must reflect the fresh stream: finish=%q content=%v", rec.FinishReason, rec.HadAnswerContent)
	}
}

// TestAppendRetriesZeroBudgetKeepsLegacyBehavior: an explicit 0 is the
// documented disable switch and must behave exactly like the shipped default,
// with the truncation surfaced in-band after a single attempt.
func TestAppendRetriesZeroBudgetKeepsLegacyBehavior(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(appendPrefixFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeTruncated+`"`) {
		t.Fatalf("append_retries 0 must keep the legacy in-band truncation: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (append_retries 0 disables the class)", n)
	}
}

// TestAppendRetriesRefusesClientDisconnect: a client that is already gone must
// never trigger a re-send, exactly like every other rescue class.
func TestAppendRetriesRefusesClientDisconnect(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(appendPrefixFrames))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.AppendRetries = 3
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	scratch := make([]byte, 512)
	resp.Body.Read(scratch)
	cancel()
	resp.Body.Close()
	waitForRecord(t, buf, 1)
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (a gone client is never rescued)", n)
	}
}
