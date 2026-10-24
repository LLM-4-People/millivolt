package proxy

// Round 5 NEW-2 regression: a custom provider total below the restored output
// count must not lower the turn's usage below input+output. The same
// consistency rule covers the standard total and the provider usage_keys total.

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const truncatedAnthropicCustomTotalStream = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10,"output_tokens":0,"my_total":%s}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

`

func TestCustomUsageTotalBelowRestoredOutputRaised(t *testing.T) {
	for _, tc := range []struct {
		name      string
		custom    string
		wantTotal int64
	}{
		{name: "below restored sum", custom: "10", wantTotal: 11},
		{name: "above restored sum", custom: "50", wantTotal: 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(truncatedAnthropicCustomTotalStream, tc.custom)
			up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, body)
			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			cfg.Providers = map[string]config.ProviderOverride{
				"fixture.example": {UsageKeys: map[string]string{"total_tokens": "my_total"}},
			}
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, "/v1/messages", "",
				`{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			want := metrics.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: tc.wantTotal}
			if rec.Usage != want {
				t.Fatalf("usage = %+v, want %+v", rec.Usage, want)
			}
			if rec.UsageInvalid {
				t.Fatal("recoverable custom total marked usage invalid")
			}
		})
	}
}
