package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// fixtureUpstream captures the path and body of one upstream call and replies
// with a fixed content type and body.
type fixtureUpstream struct {
	server      *httptest.Server
	path        atomic.Value
	escapedPath atomic.Value
	rawQuery    atomic.Value
	body        atomic.Value
	contentType string
	status      int
	response    string
	calls       atomic.Int64
}

func newFixtureUpstream(t *testing.T, contentType string, status int, response string) *fixtureUpstream {
	t.Helper()
	f := &fixtureUpstream{contentType: contentType, status: status, response: response}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		f.path.Store(r.URL.Path)
		f.escapedPath.Store(r.URL.EscapedPath())
		f.rawQuery.Store(r.URL.RawQuery)
		f.body.Store(body)
		w.Header().Set("Content-Type", f.contentType)
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.response)
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fixtureUpstream) lastPath() string        { return f.path.Load().(string) }
func (f *fixtureUpstream) lastEscapedPath() string { return f.escapedPath.Load().(string) }
func (f *fixtureUpstream) lastRawQuery() string    { return f.rawQuery.Load().(string) }
func (f *fixtureUpstream) lastBody() []byte        { return f.body.Load().([]byte) }

func proxyRequest(t *testing.T, s *Server, path, format, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://proxy.example"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	if format != "" {
		r.Header.Set("X-Proxy-Format", format)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// TestAnthropicClientToChatUpstream pins the full inbound Anthropic path: the
// client's /v1/messages request reaches the Chat endpoint, and the Chat
// completion returns in Anthropic shape.
func TestAnthropicClientToChatUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	cfg := config.Default()
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":64,"system":"be nice","messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := up.lastPath(); got != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
		t.Fatal(err)
	}
	msgs := sent["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "be nice" {
		t.Fatalf("translated messages = %v", msgs)
	}
	if sent["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %v", sent["max_tokens"])
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("response = %v", out)
	}
	content := out["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != "hello" {
		t.Fatalf("content = %v", content)
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", out["stop_reason"])
	}
}

// TestResponsesClientToChatUpstream pins the Responses input-item decode and
// the Chat upstream path.
func TestResponsesClientToChatUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","instructions":"be brief","input":"hi","max_output_tokens":64}`
	w := proxyRequest(t, s, "/v1/responses", "openai", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := up.lastPath(); got != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q, want /v1/chat/completions", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
		t.Fatal(err)
	}
	msgs := sent["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "be brief" {
		t.Fatalf("translated messages = %v", msgs)
	}
	if sent["max_tokens"] != float64(64) {
		t.Fatalf("max_tokens = %v", sent["max_tokens"])
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("response = %v", out)
	}
	output := out["output"].([]any)
	msg := output[0].(map[string]any)
	part := msg["content"].([]any)[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "hello" {
		t.Fatalf("output = %v", output)
	}
}

// TestChatClientToResponsesUpstream pins the new outbound protocol.
func TestChatClientToResponsesUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := up.lastPath(); got != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["max_output_tokens"] != float64(64) {
		t.Fatalf("max_output_tokens = %v", sent["max_output_tokens"])
	}
	input := sent["input"].([]any)
	if len(input) != 1 || input[0].(map[string]any)["role"] != "user" {
		t.Fatalf("input = %v", input)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["object"] != "chat.completion" {
		t.Fatalf("response = %v", out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "hello" {
		t.Fatalf("choice = %v", choice)
	}
}

// TestSameProtocolPassthroughIsByteIdentical pins the no-translation contract:
// an explicit matching format relays the client body and the upstream body
// unchanged.
func TestSameProtocolPassthroughIsByteIdentical(t *testing.T) {
	response := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`
	up := newFixtureUpstream(t, "application/json", 200, response)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "anthropic", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if !bytes.Equal(up.lastBody(), []byte(body)) {
		t.Fatalf("upstream body changed:\n got %s\nwant %s", up.lastBody(), body)
	}
	if w.Body.String() != response {
		t.Fatalf("client body changed:\n got %s\nwant %s", w.Body.String(), response)
	}
}

// TestAnthropicClientStreamingFromChatUpstream pins the SSE direction.
func TestAnthropicClientStreamingFromChatUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "text/event-stream", 200, strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		``,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	out := w.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"type":"text_delta"`,
		`"text":"Hi"`,
		`"stop_reason":"end_turn"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestResponsesClientStreamingFromAnthropicUpstream pins the other SSE
// direction.
func TestResponsesClientStreamingFromAnthropicUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "text/event-stream", 200, strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3,"output_tokens":0}}}`,
		``,
		`event: content_block_start`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		``,
		`event: content_block_stop`,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"claude","input":"hi","stream":true,"max_output_tokens":64}`
	w := proxyRequest(t, s, "/v1/responses", "anthropic", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	out := w.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		`"delta":"Hi"`,
		"event: response.completed",
		`"total_tokens":5`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}

// TestTranslatedUpstreamErrorUsesClientProtocol pins error-envelope
// translation.
func TestTranslatedUpstreamErrorUsesClientProtocol(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 429,
		`{"error":{"message":"rate limited","type":"rate_limit_error","code":"slow"}}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai", body)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (body %s)", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "error" {
		t.Fatalf("response is not an Anthropic error envelope: %v", out)
	}
	e := out["error"].(map[string]any)
	if e["type"] != "rate_limit_error" || e["message"] != "rate limited" {
		t.Fatalf("error = %v", e)
	}
}

// TestTranslationLossPolicy pins warn and reject modes.
func TestTranslationLossPolicy(t *testing.T) {
	t.Run("warn", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7}`
		w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(got, "seed") {
			t.Fatalf("warnings = %q, want seed", got)
		}
		if up.calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
		}
	})

	t.Run("reject", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200, `{}`)
		cfg := config.Default()
		cfg.TranslationLoss = config.TranslationLossReject
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"seed":7}`
		w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "seed") {
			t.Fatalf("rejection does not name the field: %s", w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
		}
	})
}

// TestAnthropicDefaultMaxTokensInjection pins the configured default boundary:
// the value is injected only when the client sent no output-limit spelling,
// and the Responses max_output_tokens spelling suppresses it like the two Chat
// spellings do.
func TestAnthropicDefaultMaxTokensInjection(t *testing.T) {
	const anthropicResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	cases := []struct {
		name       string
		path       string
		defaultMax int
		body       string
		want       int
	}{
		{"chat no cap", "/v1/chat/completions", 4096,
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 4096},
		{"chat configured default", "/v1/chat/completions", 1234,
			`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, 1234},
		{"chat max_tokens", "/v1/chat/completions", 4096,
			`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, 64},
		{"chat max_completion_tokens", "/v1/chat/completions", 4096,
			`{"model":"m","max_completion_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, 32},
		{"responses max_output_tokens", "/v1/responses", 4096,
			`{"model":"m","input":"hi","max_output_tokens":48}`, 48},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", 200, anthropicResponse)
			cfg := config.Default()
			cfg.AnthropicDefaultMaxTokens = tc.defaultMax
			s := New(cfg, metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, "anthropic", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
			}
			if got := up.lastPath(); got != "/v1/messages" {
				t.Fatalf("upstream path = %q, want /v1/messages", got)
			}
			var sent map[string]any
			if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
				t.Fatal(err)
			}
			if sent["max_tokens"] != float64(tc.want) {
				t.Fatalf("upstream max_tokens = %v, want %d (body %s)", sent["max_tokens"], tc.want, up.lastBody())
			}
		})
	}
}

// TestResponseSideTranslationWarningsReachTheHeader pins the non-streaming
// response-side path: a loss recorded while encoding the translated response
// reaches X-Proxy-Warnings, and an upstream header of the same name cannot
// spoof or mask it.
func TestResponseSideTranslationWarningsReachTheHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Proxy-Warnings", "upstream:spoofed")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"stop_sequence","stop_sequence":"END","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(up.Close)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.URL}})

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":["END"]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	got := w.Header().Get("X-Proxy-Warnings")
	if !strings.Contains(got, "stop_sequence") {
		t.Fatalf("response-side warning missing: %q", got)
	}
	if strings.Contains(got, "upstream:spoofed") {
		t.Fatalf("untrusted upstream warning reached the client: %q", got)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("translated response = %v", out)
	}
	message := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "done" {
		t.Fatalf("translated content = %v", message["content"])
	}
}

// TestTerminalFrameForOnlyReadsTheEventLine pins the terminal detector against
// provider-controlled data: a text delta that happens to contain a terminal
// event name must not suppress the real terminal frame after a failure.
func TestTerminalFrameForOnlyReadsTheEventLine(t *testing.T) {
	fake := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"event: message_stop\"}}\n\n")
	if terminalFrameFor(ir.Anthropic)(fake) {
		t.Fatal("content containing message_stop was treated as the terminal event")
	}
	if !terminalFrameFor(ir.Anthropic)([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")) {
		t.Fatal("real message_stop was not detected")
	}
	fakeResponses := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"event: response.completed\"}\n\n")
	if terminalFrameFor(ir.Responses)(fakeResponses) {
		t.Fatal("content containing response.completed was treated as terminal")
	}
	if !terminalFrameFor(ir.Responses)([]byte("event: response.completed\ndata: {}\n\n")) {
		t.Fatal("real response.completed was not detected")
	}
}

// TestTranslatedStreamWriterDetectsForwardedErrors pins the failure-closing
// gate: a provider error event that already reached the client must suppress
// the synthetic translation_error frame. Anthropic and Responses frames put
// the event name before the data line, so error detection must read the
// frame's data line rather than only a bare data frame.
func TestTranslatedStreamWriterDetectsForwardedErrors(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame string
		want  bool
	}{
		{"chat error data", "data: {\"error\":{\"message\":\"boom\"}}\n\n", true},
		{"anthropic error event", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n", true},
		{"responses error event", "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n", true},
		{"anthropic text delta", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"error\"}}\n\n", false},
		{"chat content", "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := &translatedStreamWriter{dst: &buf, terminal: func([]byte) bool { return false }}
			if _, err := w.Write([]byte(tc.frame)); err != nil {
				t.Fatal(err)
			}
			if w.errorFrame != tc.want {
				t.Fatalf("errorFrame = %v, want %v for %q", w.errorFrame, tc.want, tc.frame)
			}
		})
	}
}

func TestWarningsHeaderMergesAndSanitizes(t *testing.T) {
	h := http.Header{}
	setWarningsHeader(h, ir.Warnings{{Kind: ir.WarningDropped, Field: "seed"}})
	setWarningsHeader(h, ir.Warnings{{Kind: ir.WarningDropped, Field: "output.bad\nname"}})
	got := h.Get("X-Proxy-Warnings")
	if !strings.Contains(got, "seed") || !strings.Contains(got, "output.bad?name") {
		t.Fatalf("merged warnings = %q", got)
	}
	if strings.ContainsAny(got, "\r\n") {
		t.Fatalf("control byte reached the header: %q", got)
	}

	long := http.Header{}
	setWarningsHeader(long, ir.Warnings{{Kind: ir.WarningDropped, Field: strings.Repeat("x", 3000)}})
	value := long.Get("X-Proxy-Warnings")
	if len(value) > warningsHeaderMax || !strings.HasSuffix(value, "; truncated") {
		t.Fatalf("long warning value = %d bytes, suffix %q", len(value), value[max(0, len(value)-20):])
	}
}

// TestAnthropicPassthroughKeepsDegenerateTurns pins the no-translation
// contract for the new Anthropic client path: an empty end_turn and a
// thinking-only end_turn are legitimate Anthropic turns, so the OpenAI
// degenerate substitution and reasoning rescue must not rewrite them or
// re-send the request.
func TestAnthropicPassthroughKeepsDegenerateTurns(t *testing.T) {
	streams := map[string]string{
		"empty end_turn": strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3,"output_tokens":0}}}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":0}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
			``,
		}, "\n"),
		"thinking only": strings.Join([]string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3,"output_tokens":0}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
			``,
			`event: content_block_delta`,
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
			``,
		}, "\n"),
	}
	for name, stream := range streams {
		t.Run(name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", 200, stream)
			buf := metrics.NewBuffer(8)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			body := `{"model":"claude","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
			w := proxyRequest(t, s, "/v1/messages", "", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
			}
			if got := w.Body.String(); got != stream {
				t.Fatalf("passthrough body changed:\n got %q\nwant %q", got, stream)
			}
			if up.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1 (no degenerate re-send)", up.calls.Load())
			}
			if strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"choices"`) {
				t.Fatalf("Anthropic passthrough gained OpenAI frames: %s", w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.StatusCode != http.StatusOK || rec.ErrorType != "" || rec.ErrorCode != "" || rec.ErrorMsg != "" {
				t.Fatalf("legitimate turn was recorded as a failure: %+v", rec)
			}
			if rec.FinishReason != "stop" {
				t.Fatalf("finish reason = %q, want stop", rec.FinishReason)
			}
			if rec.HadAnswerContent {
				t.Fatalf("degenerate turn gained answer content: %+v", rec)
			}
			if want := rec.Usage.OutputTokens - rec.Usage.ReasoningTokens; rec.AnswerTokens != want {
				t.Fatalf("answer tokens = %d, want output minus reasoning = %d", rec.AnswerTokens, want)
			}
			if rec.Usage.InputTokens != 3 {
				t.Fatalf("input tokens = %d, want 3", rec.Usage.InputTokens)
			}
		})
	}
}

// TestTruncatedAnthropicPassthroughRecordsTheFailure proves a byte-transparent
// Anthropic stream that ends without message_stop or a stop_reason is recorded
// as truncated while the client wire stays byte-identical: the OpenAI
// degenerate substitution must not write an OpenAI error chunk into it.
func TestTruncatedAnthropicPassthroughRecordsTheFailure(t *testing.T) {
	stream := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"claude","usage":{"input_tokens":3,"output_tokens":0}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`,
		``,
		``,
	}, "\n")
	up := newFixtureUpstream(t, "text/event-stream", 200, stream)
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"claude","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != stream {
		t.Fatalf("passthrough body changed:\n got %q\nwant %q", got, stream)
	}
	if strings.Contains(w.Body.String(), "[DONE]") || strings.Contains(w.Body.String(), `"choices"`) {
		t.Fatalf("OpenAI frames were substituted into an Anthropic stream: %s", w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].ErrorCode != "truncated" || recs[0].ErrorType == "" {
		t.Fatalf("record does not carry the truncation: %+v", recs[0])
	}
}

// TestCursorTargetRejectsNonChatClient pins the documented bridge limitation:
// a Responses or Anthropic client targeting Cursor is rejected at the routing
// boundary instead of receiving a Chat-shaped envelope.
func TestCursorTargetRejectsNonChatClient(t *testing.T) {
	for _, tc := range []struct {
		path string
		body string
	}{
		{"/v1/messages", `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses", `{"model":"m","input":"hi"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", 200, `{}`)
			s := New(config.Default(), metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			w := proxyRequest(t, s, tc.path, "cursor", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "OpenAI Chat client") {
				t.Fatalf("rejection is not explicit: %s", w.Body.String())
			}
			if up.calls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
			}
		})
	}
}

// TestTranslatedClientRecordKeepsCompletionStructure pins record fidelity for
// an Anthropic client receiving a translated Chat completion: usage, finish
// reason, answer presence, tool count and tool names survive even though the
// client-facing body no longer has Chat choices.
func TestTranslatedClientRecordKeepsCompletionStructure(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"up-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	snap := waitForRecord(t, buf, 1)
	if len(snap) != 1 {
		t.Fatalf("records = %d, want 1", len(snap))
	}
	rec := snap[0]
	if rec.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", rec.FinishReason)
	}
	if rec.ToolCalls != 1 || len(rec.ToolNames) != 1 || rec.ToolNames[0] != "lookup" {
		t.Errorf("tool accounting = %d/%v", rec.ToolCalls, rec.ToolNames)
	}
	if !rec.HadAnswerContent {
		t.Error("HadAnswerContent = false, want true")
	}
	if rec.Usage.InputTokens != 3 || rec.Usage.OutputTokens != 2 || rec.Usage.TotalTokens != 5 {
		t.Errorf("usage = %+v", rec.Usage)
	}
	if rec.ProviderModel != "up-model" || rec.ProviderRequestID != "c1" {
		t.Errorf("provider identity = %q/%q", rec.ProviderModel, rec.ProviderRequestID)
	}
}

// TestInFormatOverrideOwnsTheWireShape pins the client-protocol override: it
// changes which body shape the proxy decodes (and therefore the canonical
// upstream path), is stripped before the upstream request, and rejects an
// unknown spelling at the routing boundary.
func TestInFormatOverrideOwnsTheWireShape(t *testing.T) {
	t.Run("same protocol passthrough uses the declared path", func(t *testing.T) {
		response := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		up := newFixtureUpstream(t, "application/json", 200, response)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
		r.Header.Set("X-Proxy-In-Format", "anthropic")
		r.Header.Set("X-Proxy-Format", "anthropic")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		if got := up.lastPath(); got != "/v1/messages" {
			t.Fatalf("upstream path = %q, want /v1/messages", got)
		}
		if got := string(up.lastBody()); got != body {
			t.Fatalf("passthrough body changed:\n got %s\nwant %s", got, body)
		}
		if w.Body.String() != response {
			t.Fatalf("client body changed:\n got %s\nwant %s", w.Body.String(), response)
		}
	})

	t.Run("openai-chat alias selects the Chat endpoint", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/responses", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
		r.Header.Set("X-Proxy-In-Format", "openai-chat")
		r.Header.Set("X-Proxy-Format", "openai")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		if got := up.lastPath(); got != "/v1/chat/completions" {
			t.Fatalf("upstream path = %q, want /v1/chat/completions", got)
		}
	})

	t.Run("unknown override is rejected", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200, `{}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		r.Header.Set("Authorization", "Bearer sk-test")
		r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
		r.Header.Set("X-Proxy-In-Format", "gemini")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
		}
	})

	t.Run("reject opaque provider artifact", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		cfg := config.Default()
		cfg.TranslationLoss = config.TranslationLossReject
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","max_tokens":8,"messages":[` +
			`{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque"}]},` +
			`{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "reasoning") {
			t.Fatalf("rejection does not name the artifact: %s", w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
		}
	})
}

// TestClientBodyRewriteUsesTheInboundShape pins the rewrite gate: token
// overrides and ensured Chat stubs edit the CLIENT body, so they must follow
// the inbound protocol, not the upstream target. A Chat client targeting
// Responses still receives an override (translated to max_output_tokens), a
// Responses client never receives Chat cap fields, and an Anthropic body never
// receives Chat-shaped ensured tool stubs.
func TestClientBodyRewriteUsesTheInboundShape(t *testing.T) {
	cap := func(v int) *int { return &v }

	t.Run("chat client override reaches a responses target", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[],"logprobs":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
		cfg := config.Default()
		cfg.RequestOverrides = []config.RequestOverride{{Model: "m", Body: &config.OverrideBody{MaxTokens: cap(77)}}}
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
		w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		var sent map[string]any
		if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
			t.Fatal(err)
		}
		if sent["max_output_tokens"] != float64(77) {
			t.Fatalf("max_output_tokens = %v, want the override 77", sent["max_output_tokens"])
		}
		if _, ok := sent["max_tokens"]; ok {
			t.Fatalf("Responses body carries Chat max_tokens: %v", sent)
		}
	})

	t.Run("anthropic body maps a completion-token override to max_tokens", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		cfg := config.Default()
		cfg.RequestOverrides = []config.RequestOverride{{Model: "m", Body: &config.OverrideBody{MaxCompletionTokens: cap(88)}}}
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		var sent map[string]any
		if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
			t.Fatal(err)
		}
		if sent["max_tokens"] != float64(88) {
			t.Fatalf("max_tokens = %v, want the override 88", sent["max_tokens"])
		}
		if _, ok := sent["max_completion_tokens"]; ok {
			t.Fatalf("Chat body carries max_completion_tokens from an Anthropic-body override: %v", sent)
		}
	})

	t.Run("responses client override is not stamped as a chat cap", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		cfg := config.Default()
		cfg.RequestOverrides = []config.RequestOverride{{Model: "m", Body: &config.OverrideBody{MaxTokens: cap(77)}}}
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","input":"hi","max_output_tokens":64}`
		w := proxyRequest(t, s, "/v1/responses", "openai", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		var sent map[string]any
		if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
			t.Fatal(err)
		}
		if sent["max_tokens"] != float64(64) {
			t.Fatalf("max_tokens = %v, want the client's 64 (not the 77 override)", sent["max_tokens"])
		}
	})

	t.Run("chat control still receives ensured tool stubs", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		cfg := config.Default()
		cfg.Providers = map[string]config.ProviderOverride{"fixture.example": {EnsureTools: []string{"bash"}}}
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/chat/completions", "", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		var sent map[string]any
		if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
			t.Fatal(err)
		}
		tools, ok := sent["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("ensured tools missing from the Chat control: %v", sent["tools"])
		}
	})

	t.Run("anthropic body gets no chat tool stubs", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		cfg := config.Default()
		cfg.Providers = map[string]config.ProviderOverride{"fixture.example": {EnsureTools: []string{"bash"}}}
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
		}
		var sent map[string]any
		if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
			t.Fatal(err)
		}
		if tools, ok := sent["tools"]; ok {
			t.Fatalf("Chat-shaped ensured tools leaked into the translated body: %v", tools)
		}
	})
}

// TestResponsesPassthroughBoundsOutputCap pins the Responses spelling on the
// hostile-cap trust boundary: max_output_tokens is decoded onto the record for a
// byte-transparent /v1/responses request, so an over-ceiling value is rejected
// before any upstream send and the exact ceiling still relays unchanged.
func TestResponsesPassthroughBoundsOutputCap(t *testing.T) {
	body := `{"model":"m","input":"hi","max_output_tokens":1000001}`
	up := newFixtureUpstream(t, "application/json", 200, `{}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	w := proxyRequest(t, s, "/v1/responses", "", body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
	}

	inRange := `{"model":"m","input":"hi","max_output_tokens":1000000}`
	w = proxyRequest(t, s, "/v1/responses", "", inRange)
	if w.Code != http.StatusOK {
		t.Fatalf("in-range status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := string(up.lastBody()); got != inRange {
		t.Fatalf("passthrough body mutated:\n got %s\nwant %s", got, inRange)
	}
}

// rewriteTransport sends every request to the fixture server while preserving
// the request path.
type rewriteTransport struct{ target string }

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	u, err := url.Parse(t.target)
	if err != nil {
		return nil, err
	}
	u.Path = r.URL.Path
	u.RawPath = r.URL.RawPath
	u.RawQuery = r.URL.RawQuery
	clone.URL = u
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// TestTranslationWarningsSurviveUpstreamHeader proves the proxy-owned warning
// header survives an upstream header of the same name on a translated
// response: the provider value is untrusted and must not erase or extend the
// proxy's own translation record.
func TestTranslationWarningsSurviveUpstreamHeader(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Proxy-Warnings", "upstream:spoofed")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(up.Close)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.URL}})

	body := `{"model":"m","max_tokens":8,"top_k":5,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/messages", "openai", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
	}
	got := w.Header().Get("X-Proxy-Warnings")
	if !strings.Contains(got, "top_k") {
		t.Fatalf("translation warning missing: %q", got)
	}
	if strings.Contains(got, "upstream:spoofed") {
		t.Fatalf("untrusted upstream warning reached the client: %q", got)
	}

	// A byte-transparent request never produces translation warnings, and the
	// provider's header of the same name is still not forwarded.
	w = proxyRequest(t, s, "/v1/chat/completions", "", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("passthrough status = %d (body %s)", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Proxy-Warnings"); got != "" {
		t.Fatalf("provider warning header reached a passthrough client: %q", got)
	}
}

// TestPreCommitErrorsUseTheClientProtocol pins the failure-shape contract on a
// translated Anthropic or Responses client path: a failure before the status
// line is committed must answer in the client's own error envelope, not the
// OpenAI envelope the historical Chat-only proxy always wrote.
func TestPreCommitErrorsUseTheClientProtocol(t *testing.T) {
	anthropicErrorType := func(t *testing.T, body string) (string, string) {
		t.Helper()
		var doc struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("decode anthropic error %q: %v", body, err)
		}
		return doc.Type, doc.Error.Type
	}
	openAIErrorType := func(t *testing.T, body string) string {
		t.Helper()
		var doc struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("decode openai error %q: %v", body, err)
		}
		return doc.Error.Type
	}

	t.Run("anthropic decode failure", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200, `{}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","max_tokens":8,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"f","input":[1,2]}]}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		typ, inner := anthropicErrorType(t, w.Body.String())
		if typ != "error" || inner != "invalid_request_error" {
			t.Fatalf("envelope = type %q inner %q (body %s)", typ, inner, w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
		}
	})

	t.Run("responses rejected loss", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200, `{}`)
		cfg := config.Default()
		cfg.TranslationLoss = config.TranslationLossReject
		s := New(cfg, metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","input":"hi","include":["reasoning.encrypted_content"]}`
		w := proxyRequest(t, s, "/v1/responses", "anthropic", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if got := openAIErrorType(t, w.Body.String()); got != "invalid_request_error" {
			t.Fatalf("error type = %q (body %s)", got, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "include") {
			t.Fatalf("rejection does not name the lossy field: %s", w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
		}
	})

	t.Run("anthropic hostile token cap", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200, `{}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","max_tokens":1000001,"messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
		}
		if typ, inner := anthropicErrorType(t, w.Body.String()); typ != "error" || inner != "invalid_request_error" {
			t.Fatalf("envelope = type %q inner %q (body %s)", typ, inner, w.Body.String())
		}
		if up.calls.Load() != 0 {
			t.Fatalf("upstream calls = %d, want 0", up.calls.Load())
		}
	})

	t.Run("anthropic translated transform failure", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"not json"}}]},"finish_reason":"tool_calls"}]}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
		}
		if typ, inner := anthropicErrorType(t, w.Body.String()); typ != "error" || inner != "api_error" {
			t.Fatalf("envelope = type %q inner %q (body %s)", typ, inner, w.Body.String())
		}
		if up.calls.Load() != 1 {
			t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
		}
	})
}

// TestTranslatedInBandErrorIsAFailure pins the client status for a provider
// error envelope that arrived on HTTP 200 and had to be translated: the
// client-facing status becomes 502, and the error body keeps the provider's
// type and message in the client protocol. A byte-transparent response keeps
// the historical 200 (metrics.go pins that separately).
func TestTranslatedInBandErrorIsAFailure(t *testing.T) {
	t.Run("chat upstream to anthropic client", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"insufficient_quota"}}`)
		buf := metrics.NewBuffer(8)
		s := New(config.Default(), buf)
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/messages", "openai", body)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
		}
		var doc struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode body %q: %v", w.Body.String(), err)
		}
		if doc.Type != "error" || doc.Error.Type != "billing_error" || doc.Error.Message != "quota exhausted" {
			t.Fatalf("error envelope = %+v (body %s)", doc, w.Body.String())
		}
		recs := waitForRecord(t, buf, 1)
		if len(recs) != 1 || recs[0].StatusCode != http.StatusBadGateway || recs[0].ErrorType != "insufficient_quota" {
			t.Fatalf("record = %+v", recs)
		}
	})

	t.Run("responses upstream to chat client", func(t *testing.T) {
		up := newFixtureUpstream(t, "application/json", 200,
			`{"id":"resp_1","object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}`)
		s := New(config.Default(), metrics.Noop{})
		s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
		}
		var doc struct {
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Code    string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
			t.Fatalf("decode body %q: %v", w.Body.String(), err)
		}
		// The Responses error object carries only a code, so the canonical type
		// has no source spelling and the OpenAI envelope uses api_error while
		// preserving the code.
		if doc.Error.Type != "api_error" || doc.Error.Message != "boom" || doc.Error.Code != "server_error" {
			t.Fatalf("error envelope = %+v (body %s)", doc, w.Body.String())
		}
	})
}

// TestTranslatedFrameBufCoversEncoderCaps pins TC8-9: the relay's derived
// scanner bound must cover every frame the format encoders can legally emit.
func TestTranslatedFrameBufCoversEncoderCaps(t *testing.T) {
	for name, cap := range map[string]int{
		"native stream":          providerformat.NativeStreamFrameMaxBytes,
		"responses stream":       providerformat.ResponsesStreamFrameMaxBytes,
		"responses stream + pad": providerformat.ResponsesStreamFrameMaxBytes + 2*1024,
	} {
		if translatedFrameBufMax < cap {
			t.Fatalf("translatedFrameBufMax = %d, want at least %s (%d)", translatedFrameBufMax, name, cap)
		}
	}
}

// TestUnknownPathInFormatTranslationEndToEnd pins TC4-4: an unknown path with
// X-Proxy-In-Format selects the inbound decoder, translates the request to the
// target protocol and translates the response back.
func TestUnknownPathInFormatTranslationEndToEnd(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"msg_1","type":"message","role":"assistant","model":"m",`+
			`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":3,"output_tokens":2}}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	r := httptest.NewRequest(http.MethodPost, "http://proxy.example/openai/deployments/foo/chat/completions",
		strings.NewReader(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-test")
	r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
	r.Header.Set("X-Proxy-In-Format", "openai")
	r.Header.Set("X-Proxy-Format", "anthropic")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if got := up.lastPath(); got != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", got)
	}
	if body := string(up.lastBody()); !strings.Contains(body, `"messages"`) || !strings.Contains(body, `"max_tokens":16`) {
		t.Fatalf("upstream body is not the translated Anthropic request: %s", body)
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("response is not a Chat completion: %v (%s)", err, w.Body.String())
	}
	if len(doc.Choices) != 1 || doc.Choices[0].Message.Content != "hello" {
		t.Fatalf("translated response = %s, want a Chat completion with hello", w.Body.String())
	}
}

// TestChatClientUsageChunkFollowsRequest pins TC3-6 end-to-end: a Chat client
// receives the extra usage chunk only when it asked for stream_options
// include_usage, even though the upstream Responses body reports usage.
func TestChatClientUsageChunkFollowsRequest(t *testing.T) {
	up := newFixtureUpstream(t, "text/event-stream", 200,
		`event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hi"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}

`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{
			name: "not requested",
			body: `{"model":"m","stream":true,"input":"hi"}`,
			want: false,
		},
		{
			name: "requested",
			body: `{"model":"m","stream":true,"input":"hi","stream_options":{"include_usage":true}}`,
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d (body %s)", w.Code, w.Body.String())
			}
			out := w.Body.String()
			if got := strings.Contains(out, `"usage":{`); got != tc.want {
				t.Fatalf("usage chunk present = %v, want %v:\n%s", got, tc.want, out)
			}
			if !strings.Contains(out, `"content":"Hi"`) || !strings.Contains(out, "data: [DONE]") {
				t.Fatalf("translated stream is incomplete:\n%s", out)
			}
		})
	}
}

// TestAnthropicAllEmptyRequestRejectedBeforeUpstream pins NEW-4 at the proxy
// boundary: when every Chat input part is empty, translation produces no
// Anthropic message, so the request fails with 400 before any upstream send.
func TestAnthropicAllEmptyRequestRejectedBeforeUpstream(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic",
		`{"model":"m","messages":[{"role":"user","content":""}]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "at least one non-empty message") {
		t.Fatalf("rejection does not name the empty-message rule: %s", w.Body.String())
	}
	if up.calls.Load() != 0 {
		t.Fatalf("upstream was called %d times for an all-empty request", up.calls.Load())
	}
}
