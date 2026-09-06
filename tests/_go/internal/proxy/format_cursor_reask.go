package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// cursorReaskUpstream serves a sequence of agent.v1 Run exchanges: the first
// call ends with an EMPTY turn (the void - no deltas, turn_ended immediately),
// and subsequent calls answer with a text delta. matchAction, when non-nil,
// runs on each run_request payload so a test can pin the action encoding.
func cursorReaskUpstream(t *testing.T, matchAction func(payload []byte)) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)

		// 1. Read the run_request envelope.
		_, payload := readFrame(t, body)
		if matchAction != nil {
			matchAction(payload)
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

		if n == 1 {
			// The void: turn ends immediately with no tokens and no deltas.
			w.Write(cframe(cmsg(1, cmsg(14, nil))))
			w.Write(cend("{}"))
			fl.Flush()
			return
		}
		// A real answer.
		w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "finally an answer")))))
		w.Write(cframe(cmsg(1, cmsg(8, cvint(1, 3)))))
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	t.Cleanup(upstream.Close)
	return upstream
}

func cursorVoidRequest(srv *httptest.Server, upstream *httptest.Server) *http.Request {
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[
			{"role":"user","content":"Plan the tweet."},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"plan","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call-1","name":"plan","content":"the sky is blue"}
		],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	return req
}

// TestCursorVoidReaskFreshUser: a resume-action continuation that voids is
// transparently re-driven ONCE as a fresh user turn (user_message_action) on a
// new run; the client sees only the re-ask's answer on the same stream -
// never an empty_turn error, never a [DONE] between the attempts.
func TestCursorVoidReaskFreshUser(t *testing.T) {
	upstream := cursorReaskUpstream(t, nil)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)

	if !strings.Contains(got, `"content":"finally an answer"`) {
		t.Fatalf("re-ask answer missing: %s", got)
	}
	if strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("a recovered re-ask must not surface empty_turn: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] across the transparent re-ask: %s", got)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered turn must not be an error: %+v", rec)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != "empty_turn" {
		t.Fatalf("attempts = %+v, want one absorbed empty_turn", rec.Attempts)
	}
	if rec.Retries != 1 {
		t.Fatalf("retries = %d, want 1", rec.Retries)
	}
	if rec.Usage.OutputTokens != 3 {
		t.Fatalf("output tokens = %d, want the re-ask's 3", rec.Usage.OutputTokens)
	}
}

// TestCursorVoidReaskStillVoidSurfaces: when the re-ask ALSO returns an empty
// turn, the void is surfaced exactly like today - in-band empty_turn error.
// (The upstream answers empty on every call.)
func TestCursorVoidReaskStillVoidSurfaces(t *testing.T) {
	h2s := &http2.Server{}
	var calls atomic.Int32
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, payload := readFrame(t, body)
		if !bytes.Contains(payload, []byte("claude-sonnet-5")) {
			t.Errorf("model id not in run_request: %x", payload)
		}
		kvGet := cmsg(4, append(cvint(1, 7), cmsg(2, cstr(1, "\x01\x02\x03"))...))
		w.Write(cframe(kvGet))
		fl.Flush()
		readFrame(t, body)
		execReq := cmsg(2, append(cvint(1, 9), cmsg(10, nil)...))
		w.Write(cframe(execReq))
		fl.Flush()
		readFrame(t, body)
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)

	if !strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("double void must surface empty_turn: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream Run calls = %d, want 2 (attempt + one re-ask)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != "empty_turn" {
		t.Fatalf("record must flag the void: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
	if len(rec.Attempts) != 1 {
		t.Fatalf("attempts = %+v, want the one absorbed void", rec.Attempts)
	}
}

// TestCursorReaskDisabledByConfig: quality_retries 0 turns the void back into
// today's behavior - in-band empty_turn on the FIRST void, one upstream call.
func TestCursorReaskDisabledByConfig(t *testing.T) {
	var calls atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		readFrame(t, body)
		kvGet := cmsg(4, append(cvint(1, 7), cmsg(2, cstr(1, "\x01\x02\x03"))...))
		w.Write(cframe(kvGet))
		fl.Flush()
		readFrame(t, body)
		execReq := cmsg(2, append(cvint(1, 9), cmsg(10, nil)...))
		w.Write(cframe(execReq))
		fl.Flush()
		readFrame(t, body)
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(0), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)

	if !strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("void must surface empty_turn when re-ask is disabled: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream Run calls = %d, want 1 (re-ask disabled)", n)
	}
}
