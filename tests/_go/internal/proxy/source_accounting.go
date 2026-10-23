package proxy

// Batch 14 regressions: translated accounting is read from the source
// document/wire with the provider's keys, never from the normalized client
// wire; native Anthropic streams apply the same overrides; and an invalid
// usage keeps a provider error's own code.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func translatedMessageDeltaUsage(t *testing.T, body string) map[string]any {
	t.Helper()
	for _, frame := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(frame, "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(data), &event) != nil {
				continue
			}
			if event["type"] == "message_delta" {
				usage, _ := event["usage"].(map[string]any)
				return usage
			}
		}
	}
	t.Fatalf("no message_delta frame in %s", body)
	return nil
}

func TestTranslatedNonStreamUsesSourceUsageKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{"up.example": {UsageKeys: map[string]string{
		"input_tokens": "prompt_toks", "output_tokens": "completion_toks",
	}}}
	s := New(cfg, metrics.NewBuffer(8))
	rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
	w := httptest.NewRecorder()
	body := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_toks":100,"completion_toks":50,"total_tokens":150}}`
	resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
	s.serveNonStreaming(t.Context(), w, resp, httptest.NewRequest("POST", "http://proxy.example/", nil), target, "", nil, rec, "", scheduler.WaiterHooks{})

	want := metrics.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	if rec.Usage != want {
		t.Fatalf("source usage = %+v, want %+v (body %s)", rec.Usage, want, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["input_tokens"] != float64(100) || usage["output_tokens"] != float64(50) {
		t.Fatalf("client usage = %v", usage)
	}
}

func TestTranslatedStreamUsesSourceUsageAndCache(t *testing.T) {
	s := New(config.Default(), metrics.NewBuffer(8))
	rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
	w := httptest.NewRecorder()
	body := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`,
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":5,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":3,"cache_write_tokens":2},"completion_tokens_details":{"reasoning_tokens":1}}}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
	s.transformResponse(t.Context(), w, resp, rec, target, true)

	want := metrics.Usage{
		InputTokens: 15, OutputTokens: 5, TotalTokens: 20,
		CacheReadTokens: 3, CacheWrite: 2, ReasoningTokens: 1,
	}
	if rec.Usage != want {
		t.Fatalf("source usage = %+v, want %+v (body %s)", rec.Usage, want, w.Body.String())
	}
	usage := translatedMessageDeltaUsage(t, w.Body.String())
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(5) {
		t.Fatalf("message_delta input/output = %v", usage)
	}
	if usage["cache_read_input_tokens"] != float64(3) || usage["cache_creation_input_tokens"] != float64(2) {
		t.Fatalf("message_delta cache usage = %v", usage)
	}
	details, _ := usage["output_tokens_details"].(map[string]any)
	if details["thinking_tokens"] != float64(1) {
		t.Fatalf("message_delta reasoning = %v", usage)
	}
}

func TestTranslatedNoUsageFallsBackToChunks(t *testing.T) {
	s := New(config.Default(), metrics.NewBuffer(8))
	rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
	w := httptest.NewRecorder()
	body := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n") + "\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
	target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
	s.transformResponse(t.Context(), w, resp, rec, target, true)

	// The translated Anthropic wire carries a zero usage object; the chunk
	// fallback must survive it.
	if rec.Usage.OutputTokens != 1 || rec.Usage.InputTokens != 0 || rec.Usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v, want the one-chunk output fallback", rec.Usage)
	}
}

func TestNativeAnthropicStreamCostKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{"up.example": {
		UsageKeys: map[string]string{"input_tokens": "meta.in", "output_tokens": "meta.out"},
		CostKeys:  []string{"billing.amount"},
	}}
	s := New(cfg, metrics.NewBuffer(8))
	rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
	w := httptest.NewRecorder()
	body := strings.Join([]string{
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10}},"meta":{"in":100}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5},"meta":{"out":50},"billing":{"amount":0.02}}`,
		`data: {"type":"message_stop"}`,
	}, "\n\n") + "\n\n"
	s.streamBody(t.Context(), w, strings.NewReader(body), rec, false, false, &upstreamErrorRescue{}, false)

	want := metrics.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	if rec.Usage != want {
		t.Fatalf("custom source usage = %+v, want %+v", rec.Usage, want)
	}
	if rec.Cost != 0.02 {
		t.Fatalf("cost = %v, want 0.02", rec.Cost)
	}
}

func TestTranslatedWireDoesNotApplyProviderUsageKeys(t *testing.T) {
	cfg := config.Default()
	// The override matches a path that exists in the translated Anthropic
	// usage object but not in the Chat source. Applying it to the client wire
	// would report the source output count as the input count.
	cfg.Providers = map[string]config.ProviderOverride{"up.example": {UsageKeys: map[string]string{
		"input_tokens": "output_tokens",
	}}}
	s := New(cfg, metrics.NewBuffer(8))

	t.Run("stream", func(t *testing.T) {
		rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
		w := httptest.NewRecorder()
		body := strings.Join([]string{
			`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			`data: [DONE]`,
		}, "\n\n") + "\n\n"
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}
		target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
		s.transformResponse(t.Context(), w, resp, rec, target, true)
		if rec.Usage.InputTokens != 10 || rec.Usage.OutputTokens != 5 || rec.Usage.TotalTokens != 15 {
			t.Fatalf("translated stream usage = %+v, want the source counts", rec.Usage)
		}
	})

	t.Run("non-stream", func(t *testing.T) {
		rec := &metrics.Record{StatusCode: 200, Provider: "up.example"}
		w := httptest.NewRecorder()
		body := `{"id":"c1","object":"chat.completion","created":1,"model":"m",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
		resp := &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
		target := &target{format: "openai", inbound: ir.Anthropic, outbound: ir.Chat, translate: true}
		s.serveNonStreaming(t.Context(), w, resp, httptest.NewRequest("POST", "http://proxy.example/", nil), target, "", nil, rec, "", scheduler.WaiterHooks{})
		if rec.Usage.InputTokens != 10 || rec.Usage.OutputTokens != 5 || rec.Usage.TotalTokens != 15 {
			t.Fatalf("translated non-stream usage = %+v, want the source counts", rec.Usage)
		}
	})
}

func TestInvalidUsageKeepsProviderErrorCode(t *testing.T) {
	rec := &metrics.Record{
		ErrorType: "rate_limit_error", ErrorCode: "slow_down", ErrorMsg: "provider asked to slow down",
	}
	invalidateUsage(rec, metrics.ErrMetricRange)
	if rec.ErrorType != "rate_limit_error" || rec.ErrorCode != "slow_down" || rec.ErrorMsg != "provider asked to slow down" {
		t.Fatalf("provider classification changed: %+v", rec)
	}
	if rec.Usage != (metrics.Usage{}) {
		t.Fatalf("usage was not invalidated: %+v", rec.Usage)
	}

	plain := &metrics.Record{}
	invalidateUsage(plain, metrics.ErrMetricRange)
	if plain.ErrorType != metrics.CodeInvalidUsage || plain.ErrorCode != metrics.CodeInvalidUsage {
		t.Fatalf("missing invalid-usage classification: %+v", plain)
	}
}
