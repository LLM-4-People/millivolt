package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// waitForRecord polls the buffer until it holds at least n records or the
// timeout elapses. The proxy finalizes and records metrics in a deferred call
// that runs after the client observes EOF, so a fixed sleep races under
// parallel test load; polling removes that flake deterministically.
func waitForRecord(t *testing.T, buf *metrics.Buffer, n int) []*metrics.Record {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap := buf.Snapshot(); len(snap) >= n {
			return snap
		}
		time.Sleep(time.Millisecond)
	}
	return buf.Snapshot()
}

func TestStreamingMetricsPopulated(t *testing.T) {
	// Upstream emits a realistic OpenAI stream with usage chunk.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		chunks := []string{
			"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n",
			"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n",
			"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n",
			"data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
			"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n",
			"data: [DONE]\n\n",
		}
		for _, c := range chunks {
			w.Write([]byte(c))
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"model-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Wait for the deferred Record to land (see waitForRecord).
	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("recorded %d records, want 1", len(snap))
	}
	rec := snap[0]
	if rec.Stream != true {
		t.Errorf("Stream = %v, want true", rec.Stream)
	}
	if rec.Usage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", rec.Usage.InputTokens)
	}
	if rec.Usage.OutputTokens != 2 {
		t.Errorf("OutputTokens = %d, want 2", rec.Usage.OutputTokens)
	}
	if rec.Usage.CacheReadTokens != 4 {
		t.Errorf("CacheReadTokens = %d, want 4", rec.Usage.CacheReadTokens)
	}
	if rec.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", rec.FinishReason)
	}
	if rec.TTFT() == 0 {
		t.Errorf("TTFT not captured")
	}
	if rec.FirstTokenAt.IsZero() {
		t.Errorf("FirstTokenAt not captured")
	}
	if rec.KeyHash == "" {
		t.Errorf("KeyHash empty; expected sha256 of key")
	}
	if strings.Contains(rec.KeyHash, "sk-key") {
		t.Errorf("KeyHash leaked raw key")
	}
}

// TestLiveBeginEndEmitted pins the live-request lifecycle: a "begin" event is
// published to SSE subscribers the moment a request starts (so the dashboard
// shows it streaming), and an "end" event lands on completion - while the ring
// buffer (and thus the durable store) still sees exactly one finalized record.
func TestLiveBeginEndEmitted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		// Slow stream so we can observe the begin event before completion.
		for i := 0; i < 3; i++ {
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			fl.Flush()
			time.Sleep(120 * time.Millisecond)
		}
		w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	live := buf.SubscribeLive()
	defer buf.UnsubscribeLive(live)

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Key", "sk-test")

	respCh := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		close(respCh)
	}()

	// The begin event must arrive BEFORE the request completes.
	var sawBegin, sawEnd bool
	var beginRec *metrics.Record
	var beginInFlight, endInFlight int64
	deadline := time.After(3 * time.Second)
	for !(sawBegin && sawEnd) {
		select {
		case ev := <-live:
			if ev.Phase == "begin" {
				sawBegin = true
				beginRec = ev.Record
				beginInFlight = ev.InFlight
			}
			if ev.Phase == "end" {
				sawEnd = true
				endInFlight = ev.InFlight
			}
		case <-respCh:
			// Response finished; keep draining events until we see end or timeout.
		case <-deadline:
			t.Fatalf("timed out waiting for live events: begin=%v end=%v", sawBegin, sawEnd)
		}
	}
	if !sawBegin {
		t.Error("no begin event published at request start")
	}
	if !sawEnd {
		t.Error("no end event published at request completion")
	}
	if beginRec != nil && beginRec.StatusCode != 0 {
		t.Errorf("begin record should be unfinalized (status 0), got %d", beginRec.StatusCode)
	}
	// The in-flight gauge rides every lifecycle event: this test runs exactly
	// one request, so begin must report 1 and end must report 0. (The KPI band
	// renders this value directly - it must never sit stale at the snapshot.)
	if beginInFlight != 1 {
		t.Errorf("begin event InFlight = %d, want 1", beginInFlight)
	}
	if endInFlight != 0 {
		t.Errorf("end event InFlight = %d, want 0", endInFlight)
	}

	// The ring buffer (and hence the store fan-out) must hold exactly ONE
	// finalized record - lifecycle events must not duplicate it.
	waitForRecord(t, buf, 1)
	if n := len(buf.Snapshot()); n != 1 {
		t.Errorf("ring holds %d records, want exactly 1 (no duplicate from lifecycle events)", n)
	}
}

// A provider can signal a failure IN-BAND: HTTP 200, then an error payload in
// the SSE stream ({"error":{...}}), then EOF. The client receives the failure
// verbatim, so the record must be flagged as an error too - previously the
// bytes passed through untouched, the analyzer ignored the error key, and the
// dashboard showed a clean, empty 200 (live coralbricks incident: the client
// displayed "provider overloaded", the proxy logged success).
func TestInBandStreamErrorRecordedAndRelayedVerbatim(t *testing.T) {
	errChunk := "data: {\"error\":{\"message\":\"provider overloaded\",\"type\":\"server_error\",\"code\":\"overloaded\"}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(errChunk))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"model-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Byte-transparent relay: the client gets EXACTLY the upstream payload.
	if string(body) != errChunk {
		t.Errorf("client body = %q, want verbatim %q", body, errChunk)
	}
	if resp.StatusCode != 200 {
		t.Errorf("client status = %d, want 200 (in-band errors keep the upstream status)", resp.StatusCode)
	}

	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("recorded %d records, want 1", len(snap))
	}
	rec := snap[0]
	if rec.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", rec.StatusCode)
	}
	if rec.ErrorType != "server_error" {
		t.Errorf("ErrorType = %q, want server_error", rec.ErrorType)
	}
	if rec.ErrorMsg != "provider overloaded" {
		t.Errorf("ErrorMsg = %q, want %q", rec.ErrorMsg, "provider overloaded")
	}
	if !rec.IsError() {
		t.Error("record must be flagged as an error")
	}
}

// testMidStreamAbortUpstream serves a two-part body with a long pause between
// the parts, so a test client can abort while the proxy is blocked reading.
func testMidStreamAbortUpstream(t *testing.T, contentType string, parts []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(200)
		f := w.(http.Flusher)
		w.Write([]byte(parts[0]))
		f.Flush()
		time.Sleep(1500 * time.Millisecond) // proxy is blocked on the body read here
		for _, p := range parts[1:] {
			w.Write([]byte(p))
		}
	}))
}

// TestMidStreamClientDisconnectRecords499 pins the documented 499 contract
// in docs/operations.md: a local client that disconnects
// mid-stream is recorded with client_disconnected=true and status_code 499 -
// the `cancel` bucket - never as the committed upstream 200. Cursor streams
// keep their own documented exception (upstream 200 honored in cursor_bidi);
// this covers the passthrough paths whose relay dies in markStreamErr.
func TestMidStreamClientDisconnectRecords499(t *testing.T) {
	upstream := testMidStreamAbortUpstream(t, "text/event-stream", []string{
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
		"data: [DONE]\n\n",
	})
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 256)
	if _, err := resp.Body.Read(b); err != nil {
		t.Fatalf("first chunk read: %v", err)
	}
	resp.Body.Close() // the LOCAL client aborts mid-stream

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Errorf("StatusCode = %d, want %d (client closed request)", rec.StatusCode, metrics.StatusClientClosedRequest)
	}
	if !rec.ClientDisconnected {
		t.Error("ClientDisconnected = false, want true")
	}
	if rec.IsError() {
		t.Errorf("a client cancellation must not be an error: %+v", rec)
	}
	if rec.ErrorType != "" || rec.ErrorMsg != "" {
		t.Errorf("disconnect must carry no error fields: type=%q msg=%q", rec.ErrorType, rec.ErrorMsg)
	}
}

// Non-stream variant: the same abort while the proxy is blocked spooling a
// slow JSON body - serveNonStreaming's spool read error classifies through
// the same markStreamErr choke point. (The abort travels on the request
// context: a non-streaming client receives nothing until the spool completes,
// so there is no body for it to close early.)
func TestMidStreamClientDisconnectNonStreamRecords499(t *testing.T) {
	upstream := testMidStreamAbortUpstream(t, "application/json", []string{
		`{"id":"1","choices":[{"message":`,
		`{"role":"assistant","content":"hi"}}]}`,
	})
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	go func() {
		// Result discarded on purpose: the LOCAL client aborting mid-body IS
		// the scenario under test.
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond) // the proxy is blocked spooling the slow body
	cancel()                           // the LOCAL client aborts

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Errorf("StatusCode = %d, want %d (client closed request)", rec.StatusCode, metrics.StatusClientClosedRequest)
	}
	if !rec.ClientDisconnected {
		t.Error("ClientDisconnected = false, want true")
	}
	if rec.IsError() {
		t.Errorf("a client cancellation must not be an error: %+v", rec)
	}
}

// TestClientDisconnectMidErrorBodyKeepsDecidedStatus pins the decided-outcome
// half of the 499 contract: the upstream HAD decided a final error (500 with
// max_retries=0) and the LOCAL client disconnected while that error body was
// still relaying. The record keeps the decided status and stays classifiable
// as the genuine upstream failure it is ("a genuine 5xx counts even when the
// client then cancelled") with ClientDisconnected also true. Regression: the
// unconditional 499 stamp in markClientGone flipped the decided 500 to a
// non-error 499 (IsError's 429/499 branch), erasing the failure from the
// error rate, the error explorer, and the errors-only purge.
func TestClientDisconnectMidErrorBodyKeepsDecidedStatus(t *testing.T) {
	// The error body is written past the 64 KiB capture cap in one burst, so
	// captureErrorFromResponse completes deterministically (its bounded read
	// returns at the cap, never blocking mid-body) and the decided outcome -
	// status 500 + a classified error type - is on the record BEFORE the
	// relay pauses and the client aborts.
	errBody := `{"error":{"type":"server_error","message":"boom","code":"internal"},"pad":"` +
		strings.Repeat("x", 70*1024) + `"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		f := w.(http.Flusher)
		w.Write([]byte(errBody))
		f.Flush()
		time.Sleep(1500 * time.Millisecond) // the proxy is mid-relay of the 500 body here
		w.Write([]byte(`{"tail":true}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0 // the 500 is final - no absorbed retry muddies the assertion
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	go func() {
		// Result discarded on purpose: the LOCAL client aborting mid error
		// body IS the scenario under test.
		if resp, err := http.DefaultClient.Do(req); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(200 * time.Millisecond) // the proxy is blocked relaying the slow 500 body
	cancel()                           // the LOCAL client aborts mid-body

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500 (a decided error outcome is never overwritten by the disconnect)", rec.StatusCode)
	}
	if !rec.ClientDisconnected {
		t.Error("ClientDisconnected = false, want true")
	}
	if !rec.IsError() {
		t.Errorf("a decided 5xx with the client then cancelling IS an error: %+v", rec)
	}
	if rec.ErrorType == "" {
		t.Error("ErrorType = empty, want the decided error detail to survive the disconnect")
	}
}

// Anthropic shape on a byte-transparent stream: event:error + data line with
// {"type":"error","error":{"type":...}} must be captured the same way.
func TestInBandStreamErrorAnthropicShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"model-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("recorded %d records, want 1", len(snap))
	}
	rec := snap[0]
	if rec.ErrorType != "overloaded_error" {
		t.Errorf("ErrorType = %q, want overloaded_error", rec.ErrorType)
	}
	if rec.ErrorMsg != "Overloaded" {
		t.Errorf("ErrorMsg = %q, want Overloaded", rec.ErrorMsg)
	}
	if !rec.IsError() {
		t.Error("record must be flagged as an error")
	}
}
