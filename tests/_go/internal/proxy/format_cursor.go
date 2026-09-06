package proxy

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// --- wire helpers (mirror the production agent.v1 wire format) ---

func cframe(payload []byte) []byte {
	var h [5]byte
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	return append(h[:], payload...)
}
func cend(jsonBody string) []byte {
	f := cframe([]byte(jsonBody))
	f[0] = 0x02
	return f
}
func cstr(field int, s string) []byte {
	b := []byte{byte(field<<3 | 2), byte(len(s))}
	return append(b, s...)
}
func cvint(field, v int) []byte { return []byte{byte(field << 3), byte(v)} }
func cmsg(field int, inner []byte) []byte {
	b := []byte{byte(field<<3 | 2), byte(len(inner))}
	return append(b, inner...)
}

// readFrame reads one Connect envelope from r.
func readFrame(t *testing.T, r io.Reader) (flags byte, payload []byte) {
	t.Helper()
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		t.Fatalf("read frame body: %v", err)
	}
	return hdr[0], payload
}

// TestCursorBidiTranslation drives the full-duplex path against a mock
// agent.v1 h2c upstream that performs the KV + request-context handshake, then
// streams a text delta. It asserts the proxy answered both callbacks on the
// open request stream, produced OpenAI SSE, and recorded the canonical base
// model id (thinking level stripped) even though the client sent a fused
// display id.
// TestClientCancelNotAnError pins the cancel contract for cursor's
// bidirectional path: when the turn's SSE emit fails because the LOCAL client
// disconnected (ClientAbort), the request is recorded as client-disconnected
// - upstream status kept (200), no error fields - never a stream_read_error.
// Regression: a cancelled cursor generation once showed error_type
// "stream_read_error" with the raw broken-pipe text.
func TestCursorClientAbortIsDisconnectNotError(t *testing.T) {
	s := &Server{cursorRuns: newCursorRunStore(time.Hour)}
	rec := &metrics.Record{StatusCode: 200}
	pr, pw := io.Pipe()
	run := providerformat.NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	res := providerformat.TurnResult{
		Outcome:     providerformat.TurnErrored,
		Err:         fmt.Errorf("write tcp 127.0.0.1:8080->127.0.0.1:45778: write: broken pipe"),
		ClientAbort: true,
		Output:      3,
	}
	w := httptest.NewRecorder()
	s.finishRunTurn(w, run, res, func(map[string]any, any) error { return nil }, rec, "id-1", cursorTurnRender{est: 10}, false)

	if !rec.ClientDisconnected {
		t.Errorf("ClientDisconnected = false, want true")
	}
	if rec.ErrorType != "" || rec.ErrorCode != "" || rec.ErrorMsg != "" {
		t.Errorf("error fields = %q/%q/%q, want all empty (a client abort is not an error)", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
	if rec.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want upstream 200 kept", rec.StatusCode)
	}
	if rec.IsError() {
		t.Errorf("IsError = true, want false for a client cancellation")
	}
}

// cursorTestCfg mirrors the shipped proxy.yaml providers.cursor.sh header
// mapping for a mock upstream whose provider label is its host:port - the
// cursor identity headers are CONFIG data under the new design, so a test
// that asserts them must configure the map.
func cursorTestCfg(upstreamURL string) *config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		strings.TrimPrefix(upstreamURL, "http://"): {Headers: map[string]string{
			"X-Cursor-Client-Version":      "cli-2026.08.11-0000000",
			"X-Cursor-Client-Type":         "cli",
			"X-Ghost-Mode":                 "true",
			"X-Cursor-Agent-Allowed-Tools": "mcp_tool_call",
			"X-Request-Id":                 "{{uuid4}}",
		}},
	}
	return cfg
}

func TestCursorBidiTranslation(t *testing.T) {
	// Server-side agent.v1 field numbers (mirror of the wire contract).
	// AgentServerMessage: interaction_update=1, exec_server_message=2, kv_server_message=4.
	// InteractionUpdate: text_delta=1 (text=1), token_delta=8 (tokens=1), turn_ended=14.
	var sawKvAnswer, sawCtxAnswer bool

	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent.v1.AgentService/Run" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.ProtoMajor != 2 {
			t.Errorf("proto major = %d, want 2 (h2c)", r.ProtoMajor)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/connect+proto" {
			t.Errorf("Content-Type = %q", ct)
		}
		if v := r.Header.Get("X-Cursor-Client-Version"); v == "" {
			t.Errorf("X-Cursor-Client-Version not injected")
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cursor-key" {
			t.Errorf("Authorization = %q", got)
		}

		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)

		// 1. Read the run_request envelope.
		_, payload := readFrame(t, body)
		if !bytes.Contains(payload, []byte("claude-sonnet-4-5")) {
			t.Errorf("model id not in run_request: %x", payload)
		}
		if !bytes.Contains(payload, []byte("hi")) {
			t.Errorf("prompt not in run_request: %x", payload)
		}

		// 2. Send a KV get_blob pull (id=7, blob_id = some digest) and flush.
		kvGet := cmsg(4, append(cvint(1, 7), cmsg(2, cstr(1, "\x01\x02\x03"))...)) // kv_server_message{ id:7, get_blob_args{ blob_id } }
		w.Write(cframe(kvGet))
		fl.Flush()

		// 3. Read the client's kv_client_message reply on the open stream.
		_, reply := readFrame(t, body)
		// AgentClientMessage field 3 = kv_client_message.
		if len(reply) == 0 || reply[0] != byte(3<<3|2) {
			t.Errorf("expected kv_client_message (field 3) reply, got %x", reply)
		} else {
			sawKvAnswer = true
		}

		// 4. Send the request_context handshake (exec_server_message id=9, field 10).
		execReq := cmsg(2, append(cvint(1, 9), cmsg(10, nil)...)) // exec_server_message{ id:9, request_context_args{} }
		w.Write(cframe(execReq))
		fl.Flush()

		// 5. Read the client's exec_client_message reply.
		_, reply2 := readFrame(t, body)
		if len(reply2) == 0 || reply2[0] != byte(2<<3|2) {
			t.Errorf("expected exec_client_message (field 2) reply, got %x", reply2)
		} else {
			sawCtxAnswer = true
		}

		// 6. Stream a text delta + token + turn end + end-stream.
		textDelta := cmsg(1, cmsg(1, cstr(1, "bonjour")))
		tokenDelta := cmsg(1, cmsg(8, cvint(1, 4)))
		turnEnded := cmsg(1, cmsg(14, nil))
		w.Write(cframe(textDelta))
		w.Write(cframe(tokenDelta))
		w.Write(cframe(turnEnded))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cursorTestCfg(upstream.URL), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-4.5-sonnet-thinking","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL) // h2c upstream (http://)
	req.Header.Set("X-Proxy-Format", "cursor")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !sawKvAnswer {
		t.Errorf("proxy did not answer the KV get_blob pull")
	}
	if !sawCtxAnswer {
		t.Errorf("proxy did not answer the request_context handshake")
	}
	if !strings.Contains(got, `"content":"bonjour"`) {
		t.Errorf("translated content missing: %s", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason missing: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one [DONE]: %s", got)
	}
	// The client's fused display id must be canonicalized everywhere the proxy
	// names the model: the SSE echo and the recorded/aggregated metrics name.
	if !strings.Contains(got, `"model":"claude-sonnet-4-5"`) {
		t.Errorf("SSE echo not canonicalized (want claude-sonnet-4-5): %s", got)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.Model != "claude-sonnet-4-5" {
		t.Errorf("record model = %q, want canonical base claude-sonnet-4-5", rec.Model)
	}
	if rec.Usage.OutputTokens != 4 {
		t.Errorf("output tokens = %d, want 4", rec.Usage.OutputTokens)
	}
	if rec.FirstTokenAt.IsZero() {
		t.Errorf("TTFT not captured from the translated stream")
	}
}

// TestCursorBidiVoidFlaggedToClient reproduces the "void": a cold-start
// continuation (empty resume_action - trailing tool results, no parked run)
// where Cursor finishes the turn with ZERO output tokens. The proxy must flag
// the record (error_type=empty_turn → dashboard error row/error dimension) and
// surface a real failure in-band on the stream (OpenAI error chunk with
// code=empty_turn + [DONE]) instead of a silently empty success.
func TestCursorBidiVoidFlaggedToClient(t *testing.T) {
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)

		// 1. Read the run_request envelope.
		_, payload := readFrame(t, body)
		if !bytes.Contains(payload, []byte("claude-sonnet-5")) {
			t.Errorf("model id not in run_request: %x", payload)
		}

		// 2. KV pull → answer; request-context handshake → answer.
		kvGet := cmsg(4, append(cvint(1, 7), cmsg(2, cstr(1, "\x01\x02\x03"))...))
		w.Write(cframe(kvGet))
		fl.Flush()
		readFrame(t, body)
		execReq := cmsg(2, append(cvint(1, 9), cmsg(10, nil)...))
		w.Write(cframe(execReq))
		fl.Flush()
		readFrame(t, body)

		// 3. The void: turn ends immediately with NO tokens and no deltas.
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cursorTestCfg(upstream.URL), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[
			{"role":"user","content":"Plan the tweet."},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"plan","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call-1","name":"plan","content":"the sky is blue"}
		],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	if !strings.Contains(got, `"code":"empty_turn"`) {
		t.Errorf("in-band void error missing: %s", got)
	}
	if !strings.Contains(got, `"type":"upstream_error"`) {
		t.Errorf("error chunk type missing: %s", got)
	}
	if strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("a void must not masquerade as a normal stop: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Errorf("expected exactly one [DONE]: %s", got)
	}
	if strings.Contains(got, `"usage":{`) {
		t.Errorf("void error must not carry a usage chunk: %s", got)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ErrorType != "empty_turn" || rec.ErrorCode != "empty_turn" {
		t.Errorf("record not flagged: error_type=%q error_code=%q", rec.ErrorType, rec.ErrorCode)
	}
	if rec.Usage.OutputTokens != 0 {
		t.Errorf("output tokens = %d, want 0", rec.Usage.OutputTokens)
	}
	if !rec.IsError() {
		t.Errorf("IsError must be true for a flagged void")
	}
}

func TestCursorHTTPErrorIsNotDrivenAsRun(t *testing.T) {
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"bad key"}}`))
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	srv := httptest.NewServer(New(cursorTestCfg(upstream.URL), metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (not a driven Connect run)", resp.StatusCode)
	}
	got := string(body)
	if !strings.Contains(got, "authentication_error") && !strings.Contains(got, "bad key") && !strings.Contains(got, "401") {
		t.Fatalf("body %q, want captured auth error", got)
	}
	if strings.Contains(got, `"role":"assistant"`) {
		t.Fatalf("body %q, must not emit a fake assistant role chunk", got)
	}
}

func TestCursorWaitsWhenGroupTripped(t *testing.T) {
	var hits atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
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

	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	groupKey := providerFromURL(u) + "|" + hashKey("cursor-key")
	// Trip(0) claims the send token without a nextAllowedAt window.
	// Acquire would grant immediately; only WaitSend can keep this off
	// the wire until EndSend.
	if !p.scheduler.Trip(groupKey, 0) {
		t.Fatal("expected to own")
	}

	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer cursor-key")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Format", "cursor")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- err
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- nil
	}()

	select {
	case err := <-done:
		t.Fatalf("cursor sent while group tripped: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("upstream hits = %d while tripped, want 0", got)
	}

	p.scheduler.EndSend(groupKey, false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cursor stuck after EndSend")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
}
