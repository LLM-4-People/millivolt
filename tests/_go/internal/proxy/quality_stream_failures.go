package proxy

import (
	"context"
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
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// TestStreamRetryClientCancelWhileResendParksIsDisconnect pins
// classifyResendFailure's clientGone arm on the streaming re-send site: the
// LOCAL client cancelling while the quality re-send is parked in WaitSend
// (here: on an operator hold, armed from inside the absorbed attempt's
// handler before the truncating EOF can exist, and the park observed via the
// scheduler queue before the cancel) keeps disconnect semantics - 499,
// ClientDisconnected, no error marks, the absorbed attempt logged - never a
// 502 upstream_unreachable.
func TestStreamRetryClientCancelWhileResendParksIsDisconnect(t *testing.T) {
	buf := metrics.NewBuffer(100)
	p := New(config.Default(), buf)
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
	}))
	defer upstream.Close()
	srv := httptest.NewServer(p)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	// Subscribe before the request: holdArmed says the upstream handler
	// armed the hold, not that the proxy consumed the truncating EOF and
	// parked the re-send. The park's one observable is the live paused edge
	// (the OnHold hook publishes the record with Paused set): cancel only
	// after it, so the cancel can never beat the EOF consumption and take
	// the valid plain-499 relay path with no absorbed attempt - the race
	// this poll removes.
	live := buf.SubscribeLive()
	defer buf.UnsubscribeLive(live)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var parked bool
		for !parked {
			select {
			case ev := <-live:
				parked = ev.Phase == "update" && ev.Record != nil && ev.Record.Paused
			default:
			}
			if parked || time.Now().After(deadline) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if parked {
			break
		}
		// Bail with the parked handler released first: srv.Close() waits for
		// it, and a parked re-send never wakes without the cancel.
		cancel()
		resp.Body.Close()
		t.Fatal("the re-send never published its paused edge (never parked in WaitSend)")
	}
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

// TestNonStreamSpoolReadErrorStamps502: an upstream 200 whose declared
// Content-Length is never fulfilled fails the spool read. The client is
// answered with the decided 502, and the record must carry that 502 - not the
// provisional upstream 200 stamped before the body spooled.
func TestNonStreamSpoolReadErrorStamps502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"1","choices":[`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // abort mid-body: the proxy sees an unexpected EOF
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
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
		t.Fatalf("status = %d, want 502 for a truncated upstream body", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"type":"stream_read_error"`) {
		t.Fatalf("502 body must carry the stream_read_error envelope: %s", body)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.StatusCode != http.StatusBadGateway || rec.ErrorType != "stream_read_error" {
		t.Fatalf("record = status %d type %q error=%v, want 502/stream_read_error",
			rec.StatusCode, rec.ErrorType, rec.IsError())
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
	if err := writeProtocolStreamError(failingSSEWriter{}, new(metrics.Record), ir.Chat, "id-1", "", "stream_read_error", "", "boom"); err == nil {
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
	s.finishRunTurn(failingSSEWriter{}, run, res, func(map[string]any, any) error { return nil }, rec, "id-1", cursorTurnRender{estimate: func() int64 { return 10 }}, false)

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
	s.writeRunJSON(failingSSEWriter{}, newRun(), res, "", nil, rec, "id-1", cursorTurnRender{estimate: func() int64 { return 10 }}, false)
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
	s.writeRunJSON(failingSSEWriter{}, newRun(), res2, "", nil, rec2, "id-2", cursorTurnRender{estimate: func() int64 { return 10 }}, true)
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
	s.writeRunJSON(w, run, res, "", nil, rec, "id-1", cursorTurnRender{estimate: func() int64 { return 10 }}, true)
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
