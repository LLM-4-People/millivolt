package proxy

// Round 4 batch 13 regression (H2): a translated request skips the client-facing
// metadata preflight as a whole, so a valid Anthropic conversation with more
// tool calls than the Chat aggregate bound still reaches the upstream. The same
// bytes posted as a client Chat body are rejected, proving the bound still
// guards the client boundary.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func anthropicToolCallBurst(calls int) []byte {
	content := make([]map[string]any, calls)
	for i := range content {
		content[i] = map[string]any{
			"type":  "tool_use",
			"id":    fmt.Sprintf("toolu_%d", i),
			"name":  fmt.Sprintf("lookup_%d", i),
			"input": map[string]any{"q": i},
		}
	}
	body, err := json.Marshal(map[string]any{
		"model":      "m",
		"max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": content},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	if err != nil {
		panic(err)
	}
	return body
}

func TestTranslatedGeneratedBodySkipsAggregateToolCallBound(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusOK, chatCompletionBody)
	buf := metrics.NewBuffer(8)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	request := anthropicToolCallBurst(format.WireToolCallsMax + 1)
	w := proxyRequest(t, s, "/v1/messages", "openai", string(request))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	translated := up.lastBody()
	var sent struct {
		Messages []struct {
			ToolCalls []json.RawMessage `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(translated, &sent); err != nil {
		t.Fatalf("translated Chat body: %v", err)
	}
	total := 0
	for _, message := range sent.Messages {
		total += len(message.ToolCalls)
	}
	if total != format.WireToolCallsMax+1 {
		t.Fatalf("translated tool calls = %d, want %d", total, format.WireToolCallsMax+1)
	}

	// The exact generated document at the client boundary is still bounded:
	// the broad skip belongs to the proxy's own output, not to clients.
	control := New(config.Default(), metrics.Noop{})
	control.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
	r := proxyRequest(t, control, "/v1/chat/completions", "", string(translated))
	if r.Code != http.StatusBadRequest {
		t.Fatalf("client Chat control status = %d, want 400 (body %s)", r.Code, r.Body.String())
	}
	if !strings.Contains(r.Body.String(), "tool calls exceed") {
		t.Fatalf("client Chat control rejection does not name the tool-call bound: %s", r.Body.String())
	}
}
