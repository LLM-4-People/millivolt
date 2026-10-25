package proxy

// Round 3 batch 5 regression (D1): translation may add structured content
// blocks beyond the inbound content-part count; the proxy's own generated body
// must not be re-rejected by the client trust-boundary aggregate.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func translatedGrowthBody() string {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[`)
	first := true
	for i := 0; i < 8_192; i++ {
		if !first {
			b.WriteString(",")
		}
		first = false
		b.WriteString(`{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}`)
	}
	for i := 0; i < 1_024; i++ {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, `{"role":"assistant","content":null,"tool_calls":[{"id":"call_%d","type":"function","function":{"name":"lookup","arguments":"{}"}}]}`, i)
	}
	b.WriteString(`]}`)
	return b.String()
}

func TestTranslatedRequestMergedContentNotRejected(t *testing.T) {
	body := translatedGrowthBody()
	up := newFixtureUpstream(t, "application/json", 200,
		`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	s := New(config.Default(), metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls.Load())
	}
	var sent struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
		t.Fatal(err)
	}
	blocks := 0
	toolUses := 0
	for _, message := range sent.Messages {
		blocks += len(message.Content)
		for _, block := range message.Content {
			if block["type"] == "tool_use" {
				toolUses++
			}
		}
	}
	if blocks != 17_408 || toolUses != 1_024 {
		t.Fatalf("translated blocks = %d (tool_use %d), want 17,408 (1,024)", blocks, toolUses)
	}
	if strings.Contains(w.Body.String(), "request content parts exceed") {
		t.Fatalf("generated body was rejected by the client aggregate: %s", w.Body.String())
	}
}
