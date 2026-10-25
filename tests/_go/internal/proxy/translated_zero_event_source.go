package proxy

// Round 8 F2 regression: the translated source analyzer was created without its
// wire, so a source that sent no data lines (comments or pings only) produced no
// outcome. The translator still synthesized a truncation, but the record kept
// the client-wire protocol class instead of upstream_error/truncated.

import (
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const commentOnlySource = ": ping\n\n"

func TestTranslatedZeroEventSourceRecordsTruncated(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		format     string
		body       string
		upstream   string
		wantType   string
		wantCode   string
		wantStatus int
	}{
		{
			name:       "anthropic inbound",
			path:       "/v1/messages",
			format:     "openai",
			body:       `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`,
			upstream:   commentOnlySource,
			wantType:   "upstream_error",
			wantCode:   metrics.CodeTruncated,
			wantStatus: http.StatusOK,
		},
		{
			name:       "responses inbound",
			path:       "/v1/responses",
			format:     "openai",
			body:       `{"model":"m","input":"hi","stream":true}`,
			upstream:   commentOnlySource,
			wantType:   "upstream_error",
			wantCode:   metrics.CodeTruncated,
			wantStatus: http.StatusOK,
		},
		{
			name:       "chat inbound",
			path:       "/v1/chat/completions",
			format:     "anthropic",
			body:       `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			upstream:   commentOnlySource,
			wantType:   "upstream_error",
			wantCode:   metrics.CodeTruncated,
			wantStatus: http.StatusOK,
		},
		{
			name:       "provider error keeps its class",
			path:       "/v1/responses",
			format:     "openai",
			body:       `{"model":"m","input":"hi","stream":true}`,
			upstream:   chatSourceErrorStream,
			wantType:   "server_error",
			wantCode:   "overloaded",
			wantStatus: http.StatusOK,
		},
		{
			name:       "healthy source unchanged",
			path:       "/v1/responses",
			format:     "openai",
			body:       `{"model":"m","input":"hi","stream":true}`,
			upstream:   freshAnswerFrames,
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, tc.upstream)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != tc.wantType || rec.ErrorCode != tc.wantCode {
				t.Fatalf("record error = %q/%q, want %q/%q (%+v)",
					rec.ErrorType, rec.ErrorCode, tc.wantType, tc.wantCode, rec)
			}
			if tc.wantType == "" && rec.IsError() {
				t.Fatalf("healthy source recorded an error: %+v", rec)
			}
		})
	}
}
