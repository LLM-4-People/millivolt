package proxy

// Round 7 verifier NEW-5 regression: an accepted integral cap spelling must not
// take the split-decode recovery. The parameter block used to type the three
// caps as *int, so 1e6 or 1000000.0 failed the merged metadata decode and the
// recovery returned after stamping only the cap. The record then lost
// temperature, tools, turns and character composition, and estimateTokens
// reserved only the output cap. The wire body was always correct, so only these
// record invariants catch the defect.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const capMetadataPromptChars = 4000

func capMetadataBody(capLiteral string) string {
	return fmt.Sprintf(
		`{"model":"m","temperature":0.7,"max_tokens":%s,"messages":[`+
			`{"role":"system","content":"sys"},`+
			`{"role":"user","content":%q}],`+
			`"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`,
		capLiteral, strings.Repeat("x", capMetadataPromptChars))
}

func TestTokenCapSpellingKeepsRequestMetadata(t *testing.T) {
	for _, target := range []struct {
		name         string
		format       string
		contentType  string
		response     string
		wantCapField string
	}{
		{
			name:         "passthrough",
			contentType:  "application/json",
			response:     translatedChatCompletion,
			wantCapField: "max_tokens",
		},
		{
			name:         "chat to responses",
			format:       "openai-responses",
			contentType:  "application/json",
			response:     responsesCompletedAnswer,
			wantCapField: "max_output_tokens",
		},
	} {
		t.Run(target.name, func(t *testing.T) {
			for _, literal := range []string{"1000000", "1e6", "1000000.0", "1E+6"} {
				t.Run(literal, func(t *testing.T) {
					up := newFixtureUpstream(t, target.contentType, http.StatusOK, target.response)
					buf := metrics.NewBuffer(8)
					cfg := config.Default()
					cfg.QualityRetries = 0
					cfg.ThinkingRetries = 0
					s := New(cfg, buf)
					s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

					w := proxyRequest(t, s, "/v1/chat/completions", target.format, capMetadataBody(literal))
					if w.Code != http.StatusOK {
						t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
					}
					wire := up.lastBody()
					var sent map[string]any
					if err := json.Unmarshal(wire, &sent); err != nil {
						t.Fatalf("upstream body is not JSON: %v (%s)", err, wire)
					}
					if got, ok := sent[target.wantCapField].(float64); !ok || got != 1000000 {
						t.Fatalf("upstream %s = %v, want 1000000 (%s)", target.wantCapField, sent[target.wantCapField], wire)
					}

					recs := waitForRecord(t, buf, 1)
					if len(recs) != 1 {
						t.Fatalf("records = %d, want 1", len(recs))
					}
					rec := recs[0]
					if rec.ReqMaxTokens == nil || *rec.ReqMaxTokens != 1000000 {
						t.Fatalf("record cap = %v, want 1000000", rec.ReqMaxTokens)
					}
					if rec.ReqTemperature == nil || *rec.ReqTemperature != 0.7 {
						t.Fatalf("record temperature = %v, want 0.7 (split-decode recovery dropped it)", rec.ReqTemperature)
					}
					if rec.ReqToolsCount != 1 {
						t.Fatalf("record tools count = %d, want 1 (split-decode recovery dropped it)", rec.ReqToolsCount)
					}
					if rec.TurnsUser != 1 || rec.TurnsAssistant != 0 || rec.TurnsTool != 0 {
						t.Fatalf("record turns = %d/%d/%d, want 1/0/0 (split-decode recovery dropped them)",
							rec.TurnsUser, rec.TurnsAssistant, rec.TurnsTool)
					}
					if rec.CharsSystem != 3 || rec.CharsUser != capMetadataPromptChars {
						t.Fatalf("record chars = system %d user %d, want 3/%d (split-decode recovery dropped the composition)",
							rec.CharsSystem, rec.CharsUser, capMetadataPromptChars)
					}
					if got, want := estimateTokens(rec), int64(capMetadataPromptChars/4+1000000); got != want {
						t.Fatalf("estimateTokens = %d, want %d (prompt share and cap reservation)", got, want)
					}
				})
			}

			// The same block still rejects a malformed cap before any upstream
			// call; keeping the members raw through the decode must not turn a
			// fractional value into a recorded request.
			for _, literal := range []string{"1.5", "1000001", `"1000000"`} {
				t.Run("rejects "+literal, func(t *testing.T) {
					up := newFixtureUpstream(t, target.contentType, http.StatusOK, target.response)
					s := New(config.Default(), metrics.Noop{})
					s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
					w := proxyRequest(t, s, "/v1/chat/completions", target.format, capMetadataBody(literal))
					if w.Code != http.StatusBadRequest {
						t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
					}
					if got := up.calls.Load(); got != 0 {
						t.Fatalf("upstream calls = %d, want 0", got)
					}
				})
			}
		})
	}
}
