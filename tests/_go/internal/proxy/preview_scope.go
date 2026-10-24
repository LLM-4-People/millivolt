package proxy

// Round 4 batch 13 regression (G4): a translated Responses target keeps the
// inbound prompt preview because the generated item list is not re-decoded for
// preview, while the response preview still follows the client-facing content.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesTargetCaptureBodyPreviewKeepsInboundPrompt(t *testing.T) {
	const answer = "responses answer"
	const upstreamBody = `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant",` +
		`"content":[{"type":"output_text","text":"` + answer + `"}]}],` +
		`"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`
	up := newFixtureUpstream(t, "application/json", http.StatusOK, upstreamBody)
	buf := metrics.NewBuffer(8)
	cfg := config.Default()
	cfg.CaptureBodyPreview = true
	s := New(cfg, buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses",
		`{"model":"m","messages":[{"role":"user","content":"chat prompt"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	translated := string(up.lastBody())
	if !strings.Contains(translated, `"input"`) || !strings.Contains(translated, "chat prompt") {
		t.Fatalf("translated Responses body = %s, want the Chat prompt in input", translated)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.PromptPreview != "chat prompt" {
		t.Fatalf("prompt preview = %q, want the inbound Chat prompt", rec.PromptPreview)
	}
	if rec.ResponsePreview != answer {
		t.Fatalf("response preview = %q, want %q", rec.ResponsePreview, answer)
	}
}
