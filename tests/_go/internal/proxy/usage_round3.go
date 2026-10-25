package proxy

// Round 3 batch 9 proxy regressions: Anthropic thinking tokens reach the
// canonical and translated usage, and a translated Chat stream adopts the
// source's provider usage instead of keeping the chunk-derived count.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicThinkingTokensUsage(t *testing.T) {
	const body = `{"id":"msg_1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"thinking hard"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":100,"output_tokens":50,"output_tokens_details":{"thinking_tokens":20}}}`
	for _, tc := range []struct {
		name   string
		path   string
		format string
		body   string
	}{
		{
			// The default native passthrough carries no format header.
			name:   "native",
			path:   "/v1/messages",
			format: "",
			body:   `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "translated",
			path:   "/v1/chat/completions",
			format: "anthropic",
			body:   `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, body)
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			w := proxyRequest(t, s, tc.path, tc.format, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			if tc.name == "translated" && !strings.Contains(w.Body.String(), `"completion_tokens_details":{"reasoning_tokens":20}`) {
				t.Fatalf("translated Chat usage lost the thinking tokens: %s", w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.Usage.ReasoningTokens != 20 {
				t.Fatalf("reasoning tokens = %d, want 20 (usage %+v)", rec.Usage.ReasoningTokens, rec.Usage)
			}
			if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 50 || rec.Usage.TotalTokens != 150 {
				t.Fatalf("usage = %+v, want 100/50/150", rec.Usage)
			}
		})
	}
}

func TestTranslatedChatStreamUsageGenTokens(t *testing.T) {
	var b strings.Builder
	b.WriteString(`event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":100,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`)
	for i := 0; i < 5; i++ {
		b.WriteString(`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}

`)
	}
	b.WriteString(`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":50}}

event: message_stop
data: {"type":"message_stop"}

`)
	up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, b.String())

	for _, include := range []bool{false, true} {
		t.Run(map[bool]string{false: "no include_usage", true: "include_usage"}[include], func(t *testing.T) {
			buf := metrics.NewBuffer(10)
			s := New(config.Default(), buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
			body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]`
			if include {
				body += `,"stream_options":{"include_usage":true}`
			}
			body += `}`
			w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 50 || rec.Usage.TotalTokens != 150 {
				t.Fatalf("usage = %+v, want 100/50/150", rec.Usage)
			}
			if rec.GenTokens != 50 {
				t.Fatalf("GenTokens = %d, want 50 from the adopted provider usage", rec.GenTokens)
			}
			if rec.AnswerTokens != 50 {
				t.Fatalf("AnswerTokens = %d, want 50", rec.AnswerTokens)
			}
		})
	}
}

// TestAnthropicStreamUsageIntegralSpellings pins NEW-5: the live analyzer is
// the record's Anthropic usage owner and must accept the same integral JSON
// number spellings as the format decoders. *int64 rejected 5.0 and dropped the
// usage from the record while the client wire stayed correct.
func TestAnthropicStreamUsageIntegralSpellings(t *testing.T) {
	stream := func(input, cacheRead, output string) string {
		return `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":` + input + `,"output_tokens":0,"cache_read_input_tokens":` + cacheRead + `}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":` + output + `}}

event: message_stop
data: {"type":"message_stop"}

`
	}
	for _, shape := range []struct {
		name   string
		path   string
		format string
		body   string
	}{
		{
			name: "native",
			path: "/v1/messages",
			body: `{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name:   "translated",
			path:   "/v1/chat/completions",
			format: "anthropic",
			body:   `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		},
	} {
		t.Run(shape.name, func(t *testing.T) {
			run := func(fixture string) *metrics.Record {
				up := newFixtureUpstream(t, "text/event-stream", http.StatusOK, fixture)
				buf := metrics.NewBuffer(10)
				s := New(config.Default(), buf)
				s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})
				w := proxyRequest(t, s, shape.path, shape.format, shape.body)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
				}
				recs := waitForRecord(t, buf, 1)
				return recs[0]
			}
			plain := run(stream("5", "7", "1"))
			integral := run(stream("5.0", "7.0", "1.0"))
			if integral.Usage != plain.Usage {
				t.Fatalf("integral usage = %+v, want the plain-integer value %+v", integral.Usage, plain.Usage)
			}
			if integral.Usage.InputTokens != 12 || integral.Usage.CacheReadTokens != 7 ||
				integral.Usage.OutputTokens == 0 ||
				integral.Usage.TotalTokens != integral.Usage.InputTokens+integral.Usage.OutputTokens {
				t.Fatalf("integral usage = %+v, want input 12 read 7 and a complete total", integral.Usage)
			}
		})
	}
}
