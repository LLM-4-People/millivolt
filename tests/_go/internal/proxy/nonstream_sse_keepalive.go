package proxy

// Round 7 E3 regression: a non-streaming request that receives an event-stream
// body must arm the late pacer. Only Arm/Start create the ticker; without it a
// quiet SSE socket got no keepalive even though the interval help promises one
// for mid-stream gaps. A JSON body must stay silent.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestNonStreamingSSEKeepalive(t *testing.T) {
	const stall = 120 * time.Millisecond
	chatSSE := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	chatJSON := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		contentType := "application/json"
		payload := chatJSON
		if strings.Contains(string(body), `"stream":true`) || r.Header.Get("X-Fixture-SSE") == "1" {
			contentType = "text/event-stream"
			payload = chatSSE
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(stall)
		_, _ = io.WriteString(w, payload)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.SSEKeepaliveInterval = 25 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	post := func(stream bool, forceSSE bool) string {
		t.Helper()
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		if stream {
			body = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		}
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		if forceSSE {
			req.Header.Set("X-Fixture-SSE", "1")
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return string(raw)
	}

	// Non-streaming request, SSE response: the late pacer must produce at
	// least one keepalive while the upstream is quiet.
	got := post(false, true)
	first := strings.Index(got, ": keepalive")
	data := strings.Index(got, "data: ")
	if first < 0 {
		t.Fatalf("non-streaming SSE body got no keepalive: %q", got)
	}
	if data >= 0 && first > data {
		t.Fatalf("keepalive arrived after the data: %q", got)
	}
	if !strings.Contains(got, chatSSE) {
		t.Fatalf("SSE body was not relayed: %q", got)
	}

	// Non-SSE body: the armed pacer must stay silent.
	got = post(false, false)
	if strings.Contains(got, ": keepalive") {
		t.Fatalf("JSON body got an SSE keepalive: %q", got)
	}
	if strings.TrimSpace(got) != chatJSON {
		t.Fatalf("JSON body changed: %q", got)
	}

	// Streaming control keeps its existing keepalive behavior.
	got = post(true, false)
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("streaming SSE body lost its keepalive: %q", got)
	}
}
