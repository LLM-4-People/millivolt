package proxy

// Round 6 A4 proxy regression: an over-bound Responses history call id is
// rejected by the translation owner before any upstream send.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesOverlongToolCallIDProxyRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		want int
	}{
		{name: "64 bytes", id: strings.Repeat("a", 64), want: http.StatusOK},
		{name: "65 bytes", id: strings.Repeat("a", 65), want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK,
				`{"id":"resp_1","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
			s := New(config.Default(), metrics.Noop{})
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			body := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"` + tc.id + `","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"` + tc.id + `","content":"ok"}]}`
			w := proxyRequest(t, s, "/v1/chat/completions", "openai-responses", body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusBadRequest && up.calls.Load() != 0 {
				t.Fatalf("rejected request reached the upstream %d times", up.calls.Load())
			}
			if tc.want == http.StatusOK && up.calls.Load() != 1 {
				t.Fatalf("accepted request upstream calls = %d, want 1", up.calls.Load())
			}
		})
	}
}
