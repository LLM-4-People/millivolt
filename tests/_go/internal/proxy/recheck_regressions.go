package proxy

import (
	"bufio"
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func readToDone(t *testing.T, body io.Reader) {
	t.Helper()
	br := bufio.NewReader(body)
	deadline := time.Now().Add(3 * time.Second)
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, "[DONE]") {
			return
		}
		if err != nil {
			t.Fatalf("stream ended before [DONE]: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for [DONE]")
		}
	}
}

// TestPassthroughTerminalFinalizesAtDone pins ADV-A: the relay resolves at
// [DONE] and stops reading, so a client close after the marker is a normal
// completion even while the upstream holds the connection open.
func TestPassthroughTerminalFinalizesAtDone(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(16)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()
	// Close the upstream hold before srv.Close waits on its handler; a failing
	// assertion must not deadlock the test process.
	defer close(release)
	srv.Client().Timeout = 5 * time.Second
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readToDone(t, resp.Body)
	// Keep our side open first: the record must already be finalized at [DONE].
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("record not finalized at [DONE] while the client kept the body open: %d", len(recs))
	}
	resp.Body.Close()
	rec := recs[0]
	if rec.StatusCode != http.StatusOK || rec.ClientDisconnected || rec.ErrorType != "" {
		t.Fatalf("completed stream recorded status=%d disconnected=%v error=%q", rec.StatusCode, rec.ClientDisconnected, rec.ErrorType)
	}
}

// TestTranslatedTerminalFinalizesAtDone is the translated twin of ADV-A.
func TestTranslatedTerminalFinalizesAtDone(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"type":"message_start","message":{"id":"m","model":"m"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(16)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()
	// Close the upstream hold before srv.Close waits on its handler; a failing
	// assertion must not deadlock the test process.
	defer close(release)
	srv.Client().Timeout = 5 * time.Second
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Auth-Header", "x-api-key")
	req.Header.Set("X-Proxy-Auth-Prefix", "")
	req.Header.Set("X-Proxy-Path", "/messages")
	req.Header.Set("X-Proxy-Format", "anthropic")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	readToDone(t, resp.Body)
	resp.Body.Close()
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("translated record not finalized: %d", len(recs))
	}
	rec := recs[0]
	if rec.StatusCode != http.StatusOK || rec.ClientDisconnected || rec.ErrorType != "" {
		t.Fatalf("completed translated stream recorded status=%d disconnected=%v error=%q", rec.StatusCode, rec.ClientDisconnected, rec.ErrorType)
	}
}

// TestResponsesFailedRelayedVerbatim pins PRO-2's relay half: the provider's
// own terminal failure reaches the client unchanged and its nested error is
// recorded, never replaced by a synthesized empty-completion envelope.
func TestResponsesFailedRelayedVerbatim(t *testing.T) {
	chunks := []string{
		"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"},\"sequence_number\":0}\n\n",
		"data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream exploded\"}},\"sequence_number\":1}\n\n",
	}
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()
	buf := metrics.NewBuffer(16)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()
	_, got := streamRequestTo(t, srv, upstream, "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`)
	if !strings.Contains(got, "response.failed") || !strings.Contains(got, "upstream exploded") {
		t.Fatalf("terminal failure was not relayed verbatim: %q", got)
	}
	if strings.Contains(got, metrics.CodeEmptyCompletion) || strings.Contains(got, "empty completion") {
		t.Fatalf("relay fabricated an empty completion over the provider error: %q", got)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	rec := recs[0]
	// The nested object carries no "type", so the canonical parser classifies
	// it as provider_error while preserving the provider's own code and message.
	if rec.ErrorType != "provider_error" || rec.ErrorCode != "server_error" || rec.ErrorMsg != "upstream exploded" {
		t.Fatalf("record error = type=%q code=%q msg=%q", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
}

// TestResponsesTruncationNotRetried pins PRO-3: a Responses feed that already
// relayed response.created is never transparently re-sent; chat truncation
// remains retryable elsewhere.
func TestResponsesTruncationNotRetried(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		if n == 1 {
			_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"},\"sequence_number\":0}\n\n")
			flusher.Flush()
			return
		}
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_2\"},\"sequence_number\":0}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\",\"sequence_number\":1}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_2\"},\"sequence_number\":2}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()
	buf := metrics.NewBuffer(16)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()
	_, got := streamRequestTo(t, srv, upstream, "/v1/responses", `{"model":"m","stream":true,"input":"hi"}`)
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no Responses re-send)", calls.Load())
	}
	if n := strings.Count(got, "response.created"); n != 1 {
		t.Fatalf("response.created frames = %d, want 1: %q", n, got)
	}
	waitForRecord(t, buf, 1)
}

// TestTranslatedPostTerminalDebrisStaysSuccess pins ADV-C: debris after the
// translated terminal marker never rewrites a completed record as an error.
func TestTranslatedPostTerminalDebrisStaysSuccess(t *testing.T) {
	debris := "data: " + strings.Repeat("x", sseScannerBufMax+1024) + "\n\n"
	native := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg-post","model":"claude-test","usage":{"input_tokens":4}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"before"}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n" + debris
	upstream, srv, buf := translatedSSEFixture(t, native)
	status, body := translatedSSERequest(t, srv, upstream)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if n := strings.Count(body, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] frames = %d, want 1", n)
	}
	if !strings.Contains(body, `"content":"before"`) {
		t.Fatalf("pre-terminal content lost: %q", body)
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.ErrorType != "" || rec.IsError() {
		t.Fatalf("completed stream was rewritten as an error: type=%q msg=%q", rec.ErrorType, rec.ErrorMsg)
	}
}

func auditMcpArgsFrame(t *testing.T) []byte {
	t.Helper()
	mcpArgs := append(cstr(mcpArgsName, "get_time"), cstr(mcpArgsToolCallID, "call-idle")...)
	exec := cmsg(asmExecServerMessage, append(cvint(execMessageID, 1), cmsg(execServerMcpArgs, mcpArgs)...))
	return cframe(exec)
}

func pauseStateHold(t *testing.T, state map[string]any, id string) map[string]any {
	t.Helper()
	holds, _ := state["holds"].([]any)
	for _, raw := range holds {
		hold, _ := raw.(map[string]any)
		if hold["id"] == id {
			return hold
		}
	}
	t.Fatalf("hold %q missing from %v", id, state["holds"])
	return nil
}

func pauseStateNames(t *testing.T, hold map[string]any, field string) []string {
	t.Helper()
	raw, ok := hold[field]
	if !ok || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("%s = %T %v, want an array", field, raw, raw)
	}
	out := make([]string, 0, len(list))
	for _, value := range list {
		name, _ := value.(string)
		out = append(out, name)
	}
	return out
}

// TestPausePartialEditPreservesOmittedScope pins MCP-4 at the pause boundary:
// an edit that names only clients keeps the hold's providers, and an explicit
// empty providers array is the only way to clear that dimension.
func TestPausePartialEditPreservesOmittedScope(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.pause.providers.add("alpha.example")
	p.pause.clients.add("client-a")

	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"providers":["alpha.example"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.Bytes())
	}
	created := pauseJSON(t, rr)
	holds, _ := created["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("create holds = %v", created["holds"])
	}
	id, _ := holds[0].(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("create hold has no id: %v", holds[0])
	}

	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`","clients":["client-a"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("client edit = %d %s", rr.Code, rr.Body.Bytes())
	}
	edited := pauseStateHold(t, pauseJSON(t, rr), id)
	if names := pauseStateNames(t, edited, "providers"); len(names) != 1 || names[0] != "alpha.example" {
		t.Fatalf("omitted providers were dropped by a client-only edit: %v", edited)
	}
	if names := pauseStateNames(t, edited, "clients"); len(names) != 1 || names[0] != "client-a" {
		t.Fatalf("edited clients = %v", edited)
	}

	// A no-scope edit preserves both dimensions.
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`"}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("no-scope edit = %d %s", rr.Code, rr.Body.Bytes())
	}
	preserved := pauseStateHold(t, pauseJSON(t, rr), id)
	if names := pauseStateNames(t, preserved, "providers"); len(names) != 1 || names[0] != "alpha.example" {
		t.Fatalf("no-scope edit dropped providers: %v", preserved)
	}
	if names := pauseStateNames(t, preserved, "clients"); len(names) != 1 || names[0] != "client-a" {
		t.Fatalf("no-scope edit dropped clients: %v", preserved)
	}

	// Explicit empty clears only the named dimension.
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`","providers":[]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("empty-provider edit = %d %s", rr.Code, rr.Body.Bytes())
	}
	cleared := pauseStateHold(t, pauseJSON(t, rr), id)
	if names := pauseStateNames(t, cleared, "providers"); len(names) != 0 {
		t.Fatalf("explicit empty providers did not clear: %v", cleared)
	}
	if names := pauseStateNames(t, cleared, "clients"); len(names) != 1 || names[0] != "client-a" {
		t.Fatalf("empty-provider edit dropped clients: %v", cleared)
	}
}

// TestDebugStopFailureLogsFailure pins the stop error branch: a persistence
// failure reports the warning without logging a successful stop.
func TestDebugStopFailureLogsFailure(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "debug-stop-log.db"))
	p := New(config.Default(), metrics.Noop{})
	p.AttachPausePersist(store)

	rr := httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"]}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d %s", rr.Code, rr.Body.Bytes())
	}
	sessions, _ := p.DebugSnapshot()["sessions"].([]map[string]any)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", sessions)
	}
	id, _ := sessions[0]["id"].(string)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	rr = httptest.NewRecorder()
	p.HandleDebug(rr, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":false,"id":"`+id+`"}`)))
	log.SetOutput(previous)
	if rr.Code != http.StatusOK {
		t.Fatalf("stop = %d %s", rr.Code, rr.Body.Bytes())
	}
	if !strings.Contains(rr.Body.String(), "warning") {
		t.Fatalf("persist failure did not report a warning: %s", rr.Body.Bytes())
	}
	text := logged.String()
	if !strings.Contains(text, "stop failed") {
		t.Fatalf("stop failure was not logged as a failure: %q", text)
	}
	if strings.Contains(text, "debug session "+id+" stopped") {
		t.Fatalf("stop failure logged success: %q", text)
	}
}

// TestCursorParkedRunOutlivesGenericIdleGuard pins PRO-5: a legitimately parked
// run pauses the transport idle bound; an unparked stalled body still fails at
// that generic bound.
func TestCursorParkedRunOutlivesGenericIdleGuard(t *testing.T) {
	pr, pw := io.Pipe()
	body := newIdleCancelBody(pr, func() {
		pw.Close()
		pr.Close()
	}, time.Second)
	run := providerformat.NewCursorRun(io.Discard, body, nil, func() {
		pw.Close()
		pr.Close()
	}, time.Hour)
	run.Start()
	release := make(chan struct{})
	t.Cleanup(func() {
		close(release)
		run.Close()
	})
	go func() {
		_, _ = pw.Write(auditMcpArgsFrame(t))
		<-release
	}()

	result := run.RunTurn(t.Context(), func(map[string]any) error { return nil })
	if result.Outcome != providerformat.TurnParked {
		t.Fatalf("park outcome = %v err=%v", result.Outcome, result.Err)
	}
	time.Sleep(1500 * time.Millisecond)
	if run.Closed() {
		t.Fatal("the generic idle guard closed a parked run before its park TTL")
	}
}

// TestCursorUnparkedStalledBodyFailsAtIdleGuard pins the control half of PRO-5.
func TestCursorUnparkedStalledBodyFailsAtIdleGuard(t *testing.T) {
	pr, pw := io.Pipe()
	body := newIdleCancelBody(pr, func() {
		pw.Close()
		pr.Close()
	}, 150*time.Millisecond)
	run := providerformat.NewCursorRun(io.Discard, body, nil, func() {
		pw.Close()
		pr.Close()
	}, time.Hour)
	run.Start()
	t.Cleanup(run.Close)
	result := make(chan providerformat.TurnResult, 1)
	go func() {
		result <- run.RunTurn(t.Context(), func(map[string]any) error { return nil })
	}()
	select {
	case got := <-result:
		if got.Outcome != providerformat.TurnErrored || got.Err == nil {
			t.Fatalf("idle guard result = %v err=%v, want TurnErrored", got.Outcome, got.Err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unparked stalled body did not fail at the generic idle bound")
	}
	// RunTurn can consume the terminal event before the pump runs its deferred
	// Close. The pump owns asynchronous teardown; wait for it rather than
	// asserting an unsynchronized Closed postcondition.
	waitCursorClosed(t, run)
}
