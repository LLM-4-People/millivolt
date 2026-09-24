package proxy

// Provider-configured ensured tools (config providers.<label>.ensure_tools):
// the third actor of the single body-rewrite engine. Missing names are
// injected as inert chat/completions function stubs; a client that sent no
// tools of its own additionally gets tool_choice none so the stubs can never
// be invoked; a body that already satisfies the signature relays
// byte-identical, and non-chat bodies are never rewritten. The mechanism is
// label-blind like the provider-header tests: the profile keys whatever
// provider label the upstream URL derives.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// bodyCapture records every upstream request's body bytes.
type bodyCapture struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *bodyCapture) snapshot() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]byte, len(c.bodies))
	copy(out, c.bodies)
	return out
}

// captureBodies serves one chat completion per request and records bodies.
func captureBodies(t *testing.T) (*httptest.Server, *bodyCapture) {
	t.Helper()
	cap := &bodyCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, body)
		cap.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}]}`))
	}))
	return srv, cap
}

// postChatBody sends one POST with a caller-owned body through the proxy.
func postChatBody(t *testing.T, srvURL, upstreamURL, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", srvURL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

// ensuredCfg builds a label-blind config whose profile carries the two zen
// signature names plus the id templates, like the shipped opencode.ai entry.
func ensuredCfg(label string) *config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{label: {
		EnsureTools: []string{"bash", "read"},
		Headers: map[string]string{
			"x-opencode-request": "{{opencode-msg-id}}",
			"x-opencode-session": "{{opencode-ses-id}}",
		},
	}}
	return cfg
}

func toolNames(t *testing.T, body []byte) (names []string, toolChoice any, hasTools bool) {
	t.Helper()
	var doc struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		ToolChoice any `json:"tool_choice"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("upstream body does not decode: %v\n%s", err, body)
	}
	for _, tool := range doc.Tools {
		names = append(names, tool.Function.Name)
	}
	return names, doc.ToolChoice, doc.Tools != nil
}

// TestProviderEnsureToolsInjected covers the outside-client shape: a plain
// chat body gets both signature stubs injected and tool_choice none, so the
// model can never invoke them, and the id templates mint the CLI's wire
// format per request.
func TestProviderEnsureToolsInjected(t *testing.T) {
	upstream, cap := captureBodies(t)
	defer upstream.Close()
	srv := httptest.NewServer(New(ensuredCfg(upstreamLabel(upstream.URL)), metrics.Noop{}))
	defer srv.Close()

	postChatBody(t, srv.URL, upstream.URL, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	bodies := cap.snapshot()
	if len(bodies) != 1 {
		t.Fatalf("captured %d upstream requests, want 1", len(bodies))
	}
	names, choice, hasTools := toolNames(t, bodies[0])
	if !hasTools || len(names) != 2 || names[0] != "bash" || names[1] != "read" {
		t.Fatalf("upstream tool names = %v (hasTools=%v), want bash and read stubs", names, hasTools)
	}
	if choice != "none" {
		t.Fatalf("tool_choice = %v, want none for a client that sent no tools", choice)
	}
}

// TestProviderEnsureToolsAppendToClientTools covers the agentic-client shape:
// the client's own tools stay first and untouched, the missing signature
// stubs append, and the client's tool_choice survives verbatim.
func TestProviderEnsureToolsAppendToClientTools(t *testing.T) {
	upstream, cap := captureBodies(t)
	defer upstream.Close()
	srv := httptest.NewServer(New(ensuredCfg(upstreamLabel(upstream.URL)), metrics.Noop{}))
	defer srv.Close()

	postChatBody(t, srv.URL, upstream.URL,
		`{"model":"m","messages":[{"role":"user","content":"weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}],"tool_choice":"auto"}`)
	names, choice, _ := toolNames(t, cap.snapshot()[0])
	if len(names) != 3 || names[0] != "get_weather" || names[1] != "bash" || names[2] != "read" {
		t.Fatalf("upstream tool names = %v, want the client tool first then bash and read stubs", names)
	}
	if choice != "auto" {
		t.Fatalf("tool_choice = %v, want the client's auto preserved", choice)
	}
}

// TestProviderEnsureToolsSatisfiedBodyIsByteIdentical pins the passthrough
// invariant: a body already carrying every configured name relays with its
// original bytes, not a re-encode.
func TestProviderEnsureToolsSatisfiedBodyIsByteIdentical(t *testing.T) {
	upstream, cap := captureBodies(t)
	defer upstream.Close()
	srv := httptest.NewServer(New(ensuredCfg(upstreamLabel(upstream.URL)), metrics.Noop{}))
	defer srv.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"bash"}},{"type":"function","function":{"name":"read"}}]}`
	postChatBody(t, srv.URL, upstream.URL, body)
	got := cap.snapshot()[0]
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("satisfied signature must relay byte-identical:\n got: %s\nwant: %s", got, body)
	}
}

// TestProviderEnsureToolsSkipNonChatBody covers the wire guard: a body
// without a top-level messages array (responses-style, embeddings, garbage)
// is never rewritten, so the chat-shaped stubs cannot corrupt other request
// shapes.
func TestProviderEnsureToolsSkipNonChatBody(t *testing.T) {
	upstream, cap := captureBodies(t)
	defer upstream.Close()
	srv := httptest.NewServer(New(ensuredCfg(upstreamLabel(upstream.URL)), metrics.Noop{}))
	defer srv.Close()

	body := `{"model":"m","input":"hi","stream":false}`
	postChatBody(t, srv.URL, upstream.URL, body)
	got := cap.snapshot()[0]
	if !bytes.Equal(got, []byte(body)) {
		t.Fatalf("non-chat body must relay byte-identical:\n got: %s\nwant: %s", got, body)
	}
}

// TestProviderEnsureToolsSkipTranslatedAnthropic pins the wire guard at the
// relay level: ensured tools are chat/completions-shaped, so a
// translated-anthropic target must never receive them even though its
// provider carries ensure_tools (token ceilings keep their wider wire
// reach; the stubs do not).
func TestProviderEnsureToolsSkipTranslatedAnthropic(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamBody, _ = io.ReadAll(r.Body)
		// Anthropic-shaped non-streaming response, like the format tests.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","model":"claude-x","role":"assistant","content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(ensuredCfg(upstreamLabel(upstream.URL)), metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-x","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`))
	req.Header.Set("Authorization", "Bearer sk-test-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "anthropic")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("anthropic relay status = %d, want 200", resp.StatusCode)
	}
	var translated map[string]json.RawMessage
	if err := json.Unmarshal(upstreamBody, &translated); err != nil {
		t.Fatalf("upstream body does not decode: %v\n%s", err, upstreamBody)
	}
	if raw, ok := translated["tools"]; ok {
		if strings.Contains(string(raw), `"bash"`) || strings.Contains(string(raw), `"read"`) {
			t.Fatalf("ensured chat stubs leaked into a translated-anthropic body:\n%s", raw)
		}
	}
}

// TestOpencodeIDFormat pins the CLI's identifier shape and per-call
// uniqueness: prefix plus 12 lowercase hex and 14 base62 characters, and no
// collisions across a same-millisecond burst.
func TestOpencodeIDFormat(t *testing.T) {
	shape := regexp.MustCompile(`^msg_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := opencodeID("msg_")
		if !shape.MatchString(id) {
			t.Fatalf("opencodeID = %q, want the CLI's 26-character wire shape", id)
		}
		if seen[id] {
			t.Fatalf("opencodeID collided: %q", id)
		}
		seen[id] = true
	}
}
