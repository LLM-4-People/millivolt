package proxy

// Round 8 A2 regression: a Chat stop value that is neither null nor a string
// nor an array of strings used to decode as no stop filter and reach the
// upstream. It is a malformed request, so a translated client now gets 400
// before any upstream send while native passthrough stays byte-transparent.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const anthropicStopFixtureResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func chatStopBody(raw string) string {
	return `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop":` + raw + `}`
}

func TestChatMalformedStopRejected(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, anthropicStopFixtureResponse)
	cfg := config.Default()
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	for _, raw := range []string{`5`, `{}`, `["a",1]`, `true`} {
		w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", chatStopBody(raw))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("stop %s status = %d, want 400 (body %s)", raw, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "stop") {
			t.Fatalf("stop %s rejection does not name stop: %s", raw, w.Body.String())
		}
	}
	if n := up.calls.Load(); n != 0 {
		t.Fatalf("malformed stop reached the upstream %d times", n)
	}

	for _, tc := range []struct {
		name string
		raw  string
		want []any
	}{
		{name: "string", raw: `"END"`, want: []any{"END"}},
		{name: "array", raw: `["a","b"]`, want: []any{"a", "b"}},
		{name: "null", raw: `null`},
		{name: "empty string", raw: `""`},
		{name: "empty array", raw: `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := up.calls.Load()
			w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", chatStopBody(tc.raw))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if got := up.calls.Load(); got != before+1 {
				t.Fatalf("upstream calls = %d, want %d", got, before+1)
			}
			var sent map[string]any
			if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
				t.Fatal(err)
			}
			got, present := sent["stop_sequences"]
			if tc.want == nil {
				if present {
					t.Fatalf("stop_sequences present for %s: %v", tc.raw, got)
				}
				return
			}
			values, ok := got.([]any)
			if !ok || len(values) != len(tc.want) {
				t.Fatalf("stop_sequences = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if values[i] != tc.want[i] {
					t.Fatalf("stop_sequences = %v, want %v", values, tc.want)
				}
			}
		})
	}
}

func TestChatMalformedStopNativePassthrough(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "", chatStopBody(`5`))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
	if !strings.Contains(string(up.lastBody()), `"stop":5`) {
		t.Fatalf("native passthrough changed the stop value: %s", up.lastBody())
	}
}
