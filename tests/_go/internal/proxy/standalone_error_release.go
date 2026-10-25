package proxy

// Round 6 E-U regression: a standalone error event opens the terminal hold on
// its data line, but the event is not a terminal-named document. At the blank
// boundary the hold used to stay because the stream was already terminated, so
// the data and blank were withheld until EOF. A provider that keeps the
// connection open after the error left the client with an event name and no
// data.

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

func upstreamWithDelay(t *testing.T, contentType, first, later string, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		fl, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, first)
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(delay)
		_, _ = io.WriteString(w, later)
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func timedProxyRead(t *testing.T, srv *httptest.Server, upstream *httptest.Server, path, format, body string) (string, time.Duration) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	if format != "" {
		req.Header.Set("X-Proxy-Format", format)
	}
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(data), time.Since(start)
}

func TestStandaloneErrorEventReleasedAtBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		format string
		body   string
		first  string
		want   string
	}{
		{
			name:   "anthropic",
			path:   "/v1/messages",
			format: "anthropic",
			body:   `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			first:  "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
			want:   `"busy"`,
		},
		{
			name:   "responses",
			path:   "/v1/responses",
			format: "openai-responses",
			body:   `{"model":"m","input":"hi","stream":true}`,
			first:  "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"boom\"}\n\n",
			want:   `"boom"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := upstreamWithDelay(t, "text/event-stream", tc.first, "", 500*time.Millisecond)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			srv := httptest.NewServer(New(cfg, metrics.Noop{}))
			defer srv.Close()

			got, elapsed := timedProxyRead(t, srv, upstream, tc.path, tc.format, tc.body)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("client body %q lacks the error data %s", got, tc.want)
			}
			if elapsed >= 400*time.Millisecond {
				t.Fatalf("error event released after %v, want its blank boundary (upstream closed at 500ms)", elapsed)
			}
		})
	}
}

// A Chat finish_reason also terminates the analyzer, but its hold deliberately
// waits for the usage chunk and [DONE]. The error-boundary release must not
// turn that into a premature finish.
func TestChatFinishHoldStillWaitsForUsage(t *testing.T) {
	first := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"
	later := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n"
	upstream := upstreamWithDelay(t, "text/event-stream", first, later, 300*time.Millisecond)
	cfg := config.Default()
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	got, elapsed := timedProxyRead(t, srv, upstream, "/v1/chat/completions", "",
		`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(got, `"usage"`) || !strings.Contains(got, "[DONE]") {
		t.Fatalf("client body %q lost the held usage or terminal", got)
	}
	if elapsed < 250*time.Millisecond {
		t.Fatalf("Chat finish hold released after %v before the late usage chunk", elapsed)
	}
}
