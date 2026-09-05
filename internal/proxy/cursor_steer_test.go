package proxy

// Regression: a user message typed mid-turn arrives bundled AFTER the tool
// results in the resume request. The resume path previously forwarded only the
// tool results to the parked stream and silently dropped the user's text, so
// the model resumed its prior task ignoring the message (the "message ignored
// mid-turn" bug). Cursor's real client steers the message into the live turn
// via conversation_action.user_message_action; the proxy must do the same.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestCursorResumeSteersTrailingUserMessage parks a run, then resumes it with a
// request whose messages are [tool result, user message]. It asserts the parked
// stream receives BOTH the mcp_result AND a user_message_action carrying the
// user's text - proving the mid-turn message is steered, not dropped.
func TestCursorResumeSteersTrailingUserMessage(t *testing.T) {
	// upstreamWrites captures everything the run writes to the (fake) upstream.
	var upstreamWrites bytes.Buffer
	sw := &syncWriter{w: &upstreamWrites}

	serverIn, testIn := io.Pipe()
	run := format.NewCursorRun(sw, serverIn, nil, func() { testIn.Close() }, 5*time.Second)
	run.Start()

	// Park: one mcp_args exec frame surfaces a pending tool call, then idle.
	mcpArgs := append(cstr(mcpArgsName, "read"), cstr(mcpArgsToolCallID, "call-abc")...)
	exec := cmsg(asmExecServerMessage, append(cvint(execMessageID, 1), cmsg(execServerMcpArgs, mcpArgs)...))
	if _, err := testIn.Write(cframe(exec)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !run.Parked() {
		if time.Now().After(deadline) {
			t.Fatal("pump did not register the pending tool call")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sw.reset() // ignore the frames written during parking; we assert on the resume writes

	buf := metrics.NewBuffer(100)
	s := New(config.Default(), buf)
	scope := s.cursorScopeFor(&target{baseURL: "http://upstream.invalid", authHeader: "Authorization", authPrefix: "Bearer "}, "cursor-key", "cursor-alpha-1", "go")
	if !s.cursorRuns.park(scope, run) {
		t.Fatal("park failed")
	}
	srv := httptest.NewServer(s)
	defer srv.Close()

	// Resume request: the tool result AND a trailing user message typed mid-turn.
	body := `{"model":"cursor-alpha-1","messages":[` +
		`{"role":"tool","tool_call_id":"call-abc","name":"read","content":"ok"},` +
		`{"role":"user","content":"actually, also fix the tests"}` +
		`],"stream":true}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Client", "go")
	req.Header.Set("X-Proxy-Base-URL", "http://upstream.invalid")
	req.Header.Set("X-Proxy-Format", "cursor")

	type res struct {
		resp *http.Response
		err  error
	}
	resCh := make(chan res, 1)
	go func() { resp, err := http.DefaultClient.Do(req); resCh <- res{resp, err} }()

	// Wait for the resume to consume the pending call (and write the steer).
	deadline = time.Now().Add(2 * time.Second)
	for run.Parked() {
		if time.Now().After(deadline) {
			t.Fatal("resume did not consume the pending tool call")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// End the upstream stream so the turn finishes.
	if _, err := testIn.Write(cend("{}")); err != nil {
		t.Fatal(err)
	}
	testIn.Close()

	out := <-resCh
	if out.err != nil {
		t.Fatal(out.err)
	}
	io.Copy(io.Discard, out.resp.Body)
	out.resp.Body.Close()
	if out.resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", out.resp.StatusCode)
	}

	// The decisive assertion: the stream must carry the user's steered text.
	// The user_message_action frame embeds the literal user text.
	got := sw.String()
	if !strings.Contains(got, "actually, also fix the tests") {
		t.Errorf("parked stream did NOT carry the user's mid-turn message - it was dropped.\nwritten bytes: %q", got)
	}
}

// syncWriter is a mutex-guarded bytes.Buffer (the pump and the resume write
// from different goroutines).
type syncWriter struct {
	m sync.Mutex
	w *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.m.Lock()
	defer s.m.Unlock()
	return s.w.Write(p)
}
func (s *syncWriter) String() string {
	s.m.Lock()
	defer s.m.Unlock()
	return s.w.String()
}
func (s *syncWriter) reset() {
	s.m.Lock()
	defer s.m.Unlock()
	s.w.Reset()
}
