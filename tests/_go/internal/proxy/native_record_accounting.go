package proxy

// Round 4 batch 4 regressions (F1, F4, H1): native Responses/Anthropic
// records never charge unrepresentable or partially folded usage, and a
// Responses status:"failed" body is recorded as an error.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func TestNativeAnthropicUnrepresentableUsageInvalid(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":5000000000000000000,"cache_read_input_tokens":5000000000000000000,"output_tokens":1}}`
	up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/messages", "", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if w.Body.String() != body {
		t.Fatalf("native body was not relayed verbatim: %s", w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if !rec.UsageInvalid || rec.Usage != (metrics.Usage{}) {
		t.Fatalf("unrepresentable usage = %+v invalid=%v, want an invalidated zero usage", rec.Usage, rec.UsageInvalid)
	}
	if rec.ErrorType != metrics.CodeInvalidUsage || rec.ErrorCode != metrics.CodeInvalidUsage {
		t.Fatalf("error = type %q code %q, want %s/%s", rec.ErrorType, rec.ErrorCode, metrics.CodeInvalidUsage, metrics.CodeInvalidUsage)
	}
	if got := settleTokens(rec); got != scheduler.TokensUnavailable {
		t.Fatalf("settled tokens = %d, want unavailable", got)
	}
}

func TestNativeAnthropicZeroOutputUsageKeepsFoldedCounts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		usage string
		want  metrics.Usage
	}{
		{
			name:  "zero output keeps the cache folding",
			usage: `"usage":{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":20,"output_tokens":0}`,
			want:  metrics.Usage{InputTokens: 170, OutputTokens: 0, TotalTokens: 170, CacheReadTokens: 50, CacheWrite: 20},
		},
		{
			name:  "all-cached input stays representable",
			usage: `"usage":{"input_tokens":0,"cache_read_input_tokens":50,"cache_creation_input_tokens":20,"output_tokens":0}`,
			want:  metrics.Usage{InputTokens: 70, OutputTokens: 0, TotalTokens: 70, CacheReadTokens: 50, CacheWrite: 20},
		},
		{
			name:  "output-bearing body is unchanged",
			usage: `"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":7}`,
			want:  metrics.Usage{InputTokens: 15, OutputTokens: 7, TotalTokens: 22, CacheReadTokens: 5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","model":"m",`+
				`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",%s}`, tc.usage)
			up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/messages", "", `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.UsageInvalid {
				t.Fatalf("representable usage was marked invalid: %+v", rec.Usage)
			}
			if rec.Usage != tc.want {
				t.Fatalf("usage = %+v, want %+v", rec.Usage, tc.want)
			}
			if got := settleTokens(rec); got != tc.want.TotalTokens {
				t.Fatalf("settled tokens = %d, want %d", got, tc.want.TotalTokens)
			}
		})
	}
}

func TestNativeResponsesFailedStatusRecordsError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		wantType  string
		wantCode  string
		wantError bool
	}{
		{
			name:      "failed with null error",
			body:      `{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","error":null,"output":[]}`,
			wantType:  "upstream_error",
			wantError: true,
		},
		{
			name:      "failed with provider code",
			body:      `{"id":"resp_1","object":"response","created_at":1,"status":"failed","model":"m","error":{"code":"server_error","message":"boom"},"output":[]}`,
			wantType:  "provider_error",
			wantCode:  "server_error",
			wantError: true,
		},
		{
			name: "healthy body is unchanged",
			body: `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m",` +
				`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],` +
				`"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}`,
			wantType: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, tc.body)
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/responses", "", `{"model":"m","input":"hi"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if w.Body.String() != tc.body {
				t.Fatalf("native body was not relayed verbatim: %s", w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.ErrorType != tc.wantType || rec.ErrorCode != tc.wantCode {
				t.Fatalf("error = type %q code %q msg %q, want type %q code %q", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg, tc.wantType, tc.wantCode)
			}
			if rec.IsError() != tc.wantError {
				t.Fatalf("IsError = %v, want %v (record %+v)", rec.IsError(), tc.wantError, rec)
			}
			if tc.wantError && tc.wantType == "upstream_error" && rec.FinishReason != string(ir.FinishError) {
				t.Fatalf("finish = %q, want %q", rec.FinishReason, ir.FinishError)
			}
			if !tc.wantError && rec.FinishReason != string(ir.FinishStop) {
				t.Fatalf("healthy finish = %q, want %q", rec.FinishReason, ir.FinishStop)
			}
		})
	}
}
