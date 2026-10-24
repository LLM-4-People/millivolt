package proxy

// Round 7 D1 regression: the hostile token-cap boundary must inspect each cap
// member's raw value. A typed *int decode rejects 1e9, 1000000000.0, a
// 20-digit value and a string spelling, and then every member looks absent, so
// the body was relayed with no bound and no reservation. Integral values at or
// below the bound, including 1e6 and 1000000.0, are valid.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestHostileTokenCapValuesRejected(t *testing.T) {
	upstream := newFixtureUpstream(t, "application/json", http.StatusOK,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)

	cfg := config.Default()
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.server.URL}})

	rejected := []struct {
		payload string
		format  string
		field   string
	}{
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1e9}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1e9}`, "anthropic", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000000.0}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000000.0}`, "anthropic", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":99999999999999999999}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1.5}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1.5}`, "anthropic", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":"1000000000"}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1e9}`, "", "max_completion_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_output_tokens":1e9}`, "", "max_output_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_output_tokens":"1000000000"}`, "", "max_output_tokens"},
		// A later case variant must not replace the validated value: the
		// relay sends both members, so a first-wins or strict upstream would
		// still see the over-bound occurrence.
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000001,"MAX_TOKENS":10}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"MAX_TOKENS":1000001}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000001,"MAX_TOKENS":null}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":99999999999999999999,"MAX_TOKENS":10}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max_tokens":20}`, "", "max_tokens"},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max\u005ftokens":20}`, "", "max_tokens"},
	}
	for _, tc := range rejected {
		t.Run(tc.payload+tc.format, func(t *testing.T) {
			w := proxyRequest(t, s, "/v1/chat/completions", tc.format, tc.payload)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.field) || !strings.Contains(w.Body.String(), "invalid_request_error") {
				t.Fatalf("rejection does not name %s: %s", tc.field, w.Body.String())
			}
		})
	}
	if got := upstream.calls.Load(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0 for every rejected cap", got)
	}

	accepted := []struct {
		payload string
		format  string
	}{
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1e6}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000.0}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1E+6}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":-1}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":null}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}],"MAX_TOKENS":1000000}`, ""},
		{`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, ""},
	}
	for _, tc := range accepted {
		t.Run("accept "+tc.payload+tc.format, func(t *testing.T) {
			w := proxyRequest(t, s, "/v1/chat/completions", tc.format, tc.payload)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
		})
	}
	if got := upstream.calls.Load(); got != int64(len(accepted)) {
		t.Fatalf("upstream hits = %d, want %d accepted requests", got, len(accepted))
	}

	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"hostile responses cap mixed with chat cap", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_output_tokens":1e9,"max_tokens":100}`},
		{"chat cap mixed with responses spelling", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"max_output_tokens":200}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := proxyRequest(t, s, "/v1/chat/completions", "", tc.payload)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot be combined") {
				t.Fatalf("status = %d body = %s, want the mixed-spelling 400", w.Code, w.Body.String())
			}
		})
	}
}

// The raw spelling owner must also stamp the effective reservation ceiling for
// every accepted spelling, not only for a plain integer literal.
func TestApplyTokenCapsRawSpellings(t *testing.T) {
	for _, tc := range []struct {
		payload string
		want    *int
	}{
		{`{"max_tokens":1000000}`, intPtr(1000000)},
		{`{"max_tokens":1e6}`, intPtr(1000000)},
		{`{"max_tokens":1000000.0}`, intPtr(1000000)},
		{`{"max_tokens":1E+6}`, intPtr(1000000)},
		{`{"max_tokens":0}`, intPtr(0)},
		{`{"max_tokens":-1}`, intPtr(-1)},
		{`{"max_tokens":null}`, nil},
		{`{}`, nil},
		{`{"max_tokens":1.5}`, nil},
		{`{"max_tokens":"1000000"}`, nil},
		{`{"max_completion_tokens":1e6,"max_tokens":7}`, intPtr(1000000)},
		{`{"max_output_tokens":1e6}`, intPtr(1000000)},
		{`{"MAX_TOKENS":1e6}`, intPtr(1000000)},
		{`{"max_tokens":1000001,"MAX_TOKENS":10}`, nil},
		{`{"max_tokens":10,"MAX_TOKENS":10}`, nil},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			rec := &metrics.Record{}
			applyTokenCaps([]byte(tc.payload), rec)
			switch {
			case tc.want == nil && rec.ReqMaxTokens != nil:
				t.Fatalf("cap = %d, want absent", *rec.ReqMaxTokens)
			case tc.want != nil && rec.ReqMaxTokens == nil:
				t.Fatalf("cap = nil, want %d", *tc.want)
			case tc.want != nil && *rec.ReqMaxTokens != *tc.want:
				t.Fatalf("cap = %d, want %d", *rec.ReqMaxTokens, *tc.want)
			}
		})
	}
}

func intPtr(v int) *int { return &v }

// TestHostileTokenCapDuplicatesRejectedBeforeEnsureToolsRewrite pins D1: the
// ensured-tools rewrite re-marshals the body and collapses duplicate cap
// members, so the duplicate scan must run on the pre-rewrite bytes.
func TestHostileTokenCapDuplicatesRejectedBeforeEnsureToolsRewrite(t *testing.T) {
	upstream := newFixtureUpstream(t, "application/json", http.StatusOK,
		`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	cfg := ensuredCfg("fixture.example")
	cfg.QualityRetries = 0
	cfg.ThinkingRetries = 0
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.server.URL}})

	for _, payload := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max_tokens":20}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"max\u005ftokens":20}`,
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000001,"MAX_TOKENS":10}`,
	} {
		t.Run(payload, func(t *testing.T) {
			w := proxyRequest(t, s, "/v1/chat/completions", "", payload)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "max_tokens") {
				t.Fatalf("status = %d body = %s, want the duplicate-cap 400", w.Code, w.Body.String())
			}
		})
	}
	if got := upstream.calls.Load(); got != 0 {
		t.Fatalf("duplicate cap reached the upstream %d times", got)
	}

	control := `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1000000}`
	w := proxyRequest(t, s, "/v1/chat/completions", "", control)
	if w.Code != http.StatusOK {
		t.Fatalf("valid rewrite control status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if got := upstream.calls.Load(); got != 1 {
		t.Fatalf("valid rewrite control upstream hits = %d, want 1", got)
	}
	if !strings.Contains(string(upstream.lastBody()), `"bash"`) {
		t.Fatalf("ensure-tools rewrite did not run: %s", upstream.lastBody())
	}
}
