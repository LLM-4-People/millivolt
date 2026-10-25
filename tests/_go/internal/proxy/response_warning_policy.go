package proxy

// Round 5 NEW-12 regression: translation_loss reject is a request-side policy.
// Response-side warnings are never fatal, so a translated response that
// approximates a reasoning summary or a citation still reaches the client with
// its warning header.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponseWarningsStayNonFatalInRejectMode(t *testing.T) {
	const responsesReasoning = `{"id":"resp_1","object":"response","status":"completed","model":"m","output":[` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"enc"}]}`
	const anthropicCitation = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"answer","citations":[` +
		`{"type":"web_search_result_location","url":"https://example.com","title":"Example"}]}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	for _, tc := range []struct {
		name     string
		path     string
		format   string
		body     string
		upstream string
		contains string
		warning  string
	}{
		{
			name:     "reasoning summary to anthropic",
			path:     "/v1/messages",
			format:   "openai-responses",
			body:     `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			upstream: responsesReasoning,
			contains: "thinking",
			warning:  "reasoning",
		},
		{
			name:     "citation span to chat",
			path:     "/v1/chat/completions",
			format:   "anthropic",
			body:     `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			upstream: anthropicCitation,
			contains: "answer",
			warning:  "citations",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, tc.upstream)
			cfg := config.Default()
			cfg.TranslationLoss = config.TranslationLossReject
			s := New(cfg, metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Proxy-Warnings"); !strings.Contains(got, tc.warning) {
				t.Fatalf("warning header = %q, want %q", got, tc.warning)
			}
			if !strings.Contains(w.Body.String(), tc.contains) {
				t.Fatalf("client body lost %q: %s", tc.contains, w.Body.String())
			}
		})
	}
}
