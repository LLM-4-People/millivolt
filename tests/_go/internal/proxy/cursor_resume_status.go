package proxy

// Regression: a resumed Cursor turn never opened an upstream request, so its
// metrics record kept status_code 0 - and the dashboard renders any row
// without a status_code as an in-flight "streaming" row forever. Resumed
// turns must carry status 200 so the row finalizes.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// agent.v1 wire field numbers for the exec tool-call frame (mirror of the
// constants in format/cursor_bidi.go, not importable from package proxy).
const (
	asmExecServerMessage = 2 // AgentServerMessage.exec_server_message
	execMessageID        = 1 // ExecServerMessage.id
	execServerMcpArgs    = 11
	mcpArgsName          = 1 // McpArgs.name
	mcpArgsToolCallID    = 3 // McpArgs.tool_call_id
)

func TestCursorResumeRecordGetsStatus(t *testing.T) {
	// Fabricate a parked run: the pump processes one mcp_args exec frame
	// (surfacing it as a pending tool call) and then EOFs.
	serverIn, testIn := io.Pipe()
	run := format.NewCursorRun(io.Discard, serverIn, nil, func() {
		testIn.Close()
	}, 5*time.Second)
	run.Start()
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

	buf := metrics.NewBuffer(100)
	s := New(config.Default(), buf)
	scope := s.cursorScopeFor(&target{baseURL: "http://upstream.invalid", authHeader: "Authorization", authPrefix: "Bearer "}, "cursor-key", "cursor-alpha-1", "go")
	if !s.cursorRuns.park(scope, run) {
		t.Fatal("park failed")
	}
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"cursor-alpha-1","messages":[{"role":"tool","tool_call_id":"call-abc","name":"read","content":"ok"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Client", "go")
	req.Header.Set("X-Proxy-Base-URL", "http://upstream.invalid")
	req.Header.Set("X-Proxy-Format", "cursor")

	// Keep the upstream stream alive through the resume (a pump that already
	// exited marks the run closed and the proxy cold-starts instead); once the
	// resume consumed the pending id, end the stream so the turn finishes.
	type res struct {
		resp *http.Response
		err  error
	}
	resCh := make(chan res, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		resCh <- res{resp, err}
	}()
	deadline = time.Now().Add(2 * time.Second)
	for run.Parked() {
		if time.Now().After(deadline) {
			t.Fatal("resume did not consume the pending tool call")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := testIn.Write(cend("{}")); err != nil {
		t.Fatal(err)
	}
	testIn.Close()

	out := <-resCh
	if out.err != nil {
		t.Fatal(out.err)
	}
	body, _ := io.ReadAll(out.resp.Body)
	out.resp.Body.Close()
	if out.resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", out.resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"finish_reason":"stop"`) {
		t.Errorf("resumed turn should finish after its result is answered, body: %s", body)
	}

	recs := waitForRecord(t, buf, 1)
	if got := recs[0].StatusCode; got != 200 {
		t.Errorf("record status_code = %d, want 200 (row would stay 'streaming')", got)
	}
	if recs[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", recs[0].FinishReason)
	}
	// The parked turn ends before Cursor's usage summary lands; the record must
	// carry the payload-estimated input, never 0 - clients use the
	// reported prompt_tokens to track context and decide when to compact, and 0
	// made them think every tool turn was free.
	if recs[0].Usage.InputTokens == 0 {
		t.Errorf("parked-turn input tokens = 0, want the payload estimate (a 0 prompt hides context growth from the client)")
	}
}
func TestEstimateInputTokens(t *testing.T) {
	small := estimateInputTokens([]byte(`{"model":"g","messages":[{"role":"user","content":"hi"}]}`))
	if small <= cursorAgentContextBaseTokens {
		t.Fatalf("small estimate = %d, want > base %d", small, cursorAgentContextBaseTokens)
	}
	// 4k chars ≈ +1024 tokens over the base; ordering must hold as content grows.
	big := estimateInputTokens([]byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 4096) + `"}]}`))
	if big <= small {
		t.Fatalf("big estimate = %d, small = %d - estimate did not grow with content", big, small)
	}
	// Array-shaped (multipart) content counts too.
	arr := estimateInputTokens([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"` + strings.Repeat("y", 2048) + `"}]}]}`))
	if arr <= cursorAgentContextBaseTokens {
		t.Fatalf("array-content estimate = %d, want > base", arr)
	}
	// Unparsable body still yields the base (never 0).
	bad := estimateInputTokens([]byte(`not json`))
	if bad != cursorAgentContextBaseTokens {
		t.Fatalf("bad-body estimate = %d, want base %d", bad, cursorAgentContextBaseTokens)
	}
}
