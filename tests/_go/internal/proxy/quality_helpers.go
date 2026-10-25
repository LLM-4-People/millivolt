package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

const voidBody = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
const realBody = `{"id":"2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hello there"},"finish_reason":"stop"}]}`

func newConfigWithQuality(n int) *config.Config {
	cfg := config.Default()
	cfg.QualityRetries = n
	return cfg
}

type closeCountBody struct {
	io.ReadCloser
	n *atomic.Int32
}

func (c *closeCountBody) Close() error {
	c.n.Add(1)
	return c.ReadCloser.Close()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func streamUpstream(t *testing.T, chunks ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, c := range chunks {
			w.Write([]byte(c))
			w.(http.Flusher).Flush()
		}
	}))
}

func streamRequest(t *testing.T, srv *httptest.Server, upstream *httptest.Server) (int, string) {
	t.Helper()
	return streamRequestTo(t, srv, upstream, "/v1/chat/completions",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

// streamRequestTo is streamRequest with an explicit upstream path and body:
// the relay is path-transparent, so the same fixture drives chat completions
// and legacy prompt completions streams.
func streamRequestTo(t *testing.T, srv *httptest.Server, upstream *httptest.Server, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// failingSSEWriter is a ResponseWriter whose socket is already dead: every
// write fails, like a LOCAL client that disconnected mid-stream.
type failingSSEWriter struct{}

func (failingSSEWriter) Header() http.Header       { return http.Header{} }
func (failingSSEWriter) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }
func (failingSSEWriter) WriteHeader(int)           {}

// Thinking-rescue fixtures: the reasoning-only delta frame providers stream
// (DeepSeek reasoning_content and its spellings), the degenerate reasoning-only
// stop, and a healthy fresh stream the rescue re-send appends.
const thinkingRoleFrame = `data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"
const thinkingFrame = `data: {"id":"A","choices":[{"delta":{"reasoning_content":"thinking hard"}}]}` + "\n\n"
const thinkingStopFrames = `data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"
const freshAnswerFrames = `data: {"id":"B","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n" +
	`data: {"id":"B","choices":[{"delta":{"content":"hello"}}]}` + "\n\n" +
	`data: {"id":"B","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	`data: [DONE]` + "\n\n"

const thinkingOnlyBody = `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"thought hard"},"finish_reason":"stop"}]}`
