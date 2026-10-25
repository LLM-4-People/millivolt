package proxy

// Round 5 A2 end-to-end regression: a translated stream to a Chat upstream
// asks for stream_options.include_usage, so the usage chunk reaches the
// Anthropic and Responses clients instead of an omitted or all-zero terminal.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const chatUsageStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

func TestTranslatedStreamRequestsUsageEndToEnd(t *testing.T) {
	var requested atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		include := strings.Contains(string(body), `"include_usage":true`)
		requested.Store(include)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if include {
			_, _ = io.WriteString(w, chatUsageStream)
			return
		}
		_, _ = io.WriteString(w, strings.ReplaceAll(chatUsageStream,
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n", ""))
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name     string
		path     string
		body     string
		contains []string
		absent   []string
	}{
		{
			name: "anthropic client",
			path: "/v1/messages",
			body: `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			contains: []string{
				`"input_tokens":10`,
				`"output_tokens":5`,
			},
		},
		{
			name: "responses client",
			path: "/v1/responses",
			body: `{"model":"m","input":"hi","stream":true}`,
			contains: []string{
				`"usage":{"input_tokens":10,"input_tokens_details":`,
				`"output_tokens":5`,
			},
			absent: []string{`"input_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens":0`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			s := New(cfg, metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})
			w := proxyRequest(t, s, tc.path, "openai", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if !requested.Load() {
				t.Fatalf("upstream request did not ask for usage: %s", w.Body.String())
			}
			got := w.Body.String()
			for _, want := range tc.contains {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q in the client stream: %s", want, got)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(got, absent) {
					t.Fatalf("client stream still carries zero usage %q: %s", absent, got)
				}
			}
		})
	}
}
