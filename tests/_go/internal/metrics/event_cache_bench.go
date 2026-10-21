package metrics

import (
	"sync/atomic"
	"testing"
	"time"
)

var benchmarkEventPayloadBytes int

// benchmarkEventRecord is a finalized record shaped like the dashboard's
// larger SSE payloads: provider/client identity, usage, retry attempts with
// response headers, and both bounded content previews. The shared cache must be
// measured against the same bytes the direct path encodes.
func benchmarkEventRecord() *Record {
	start := time.Date(2026, 10, 18, 10, 30, 0, 0, time.Local)
	return &Record{
		ID:             "bench-final-record",
		Provider:       "fixture-provider",
		Model:          "fixture-model-large",
		KeyHash:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		UserAgent:      "fixture-client/1.0",
		Client:         "openai-python",
		Stream:         true,
		ConversationID: "conversation-fixture",
		ClientIP:       "127.0.0.1",
		Path:           "/v1/chat/completions",
		Method:         "POST",
		StatusCode:     200,
		Start:          start,
		End:            start.Add(1500 * time.Millisecond),
		FirstTokenAt:   start.Add(250 * time.Millisecond),
		LastTokenAt:    start.Add(1400 * time.Millisecond),
		FinishReason:   "stop",
		TTFTMs:         250,
		DecodeTPS:      60.25,
		DurationMs:     1500,
		Usage: Usage{
			InputTokens:     2048,
			OutputTokens:    512,
			TotalTokens:     2560,
			CacheReadTokens: 1024,
		},
		Retries: 2,
		Attempts: []RetryAttempt{
			{
				StatusCode: 429, ErrorType: "rate_limit_error", ErrorMsg: "upstream rate limit",
				RetryAfterMs: 250, At: start.Add(100 * time.Millisecond),
				ProviderRequestID: "req-attempt-1", ProviderServer: "fixture-upstream",
				ProviderModel: "fixture-model-large", ProcessingMs: 12,
				RateLimitRemaining: 0, RateLimitLimit: 60,
				ResponseHeaders: map[string][]string{"retry-after": {"0.25"}},
			},
			{
				StatusCode: 503, ErrorType: "provider_error", ErrorMsg: "upstream temporarily unavailable",
				RetryAfterMs: 500, At: start.Add(400 * time.Millisecond),
				ProviderRequestID: "req-attempt-2", ProviderServer: "fixture-upstream",
				ProviderModel: "fixture-model-large", ProcessingMs: 48,
				ResponseHeaders: map[string][]string{"retry-after": {"0.5"}},
			},
		},
		PromptPreview:   "fixture prompt preview with representative request text",
		ResponsePreview: "fixture response preview with representative completion text",
	}
}

// BenchmarkSharedEventEncode compares the two-subscriber publication paths for
// one ring event: two direct observed-JSON encodes (the pre-cache behavior)
// versus one event cache plus a hit. The shared path pays one small cache
// allocation per event; the observer provider and json.Marshal run once.
// observer-calls/op is the measured marshal-path invocation count.
func BenchmarkSharedEventEncode(b *testing.B) {
	rec := benchmarkEventRecord()
	for _, shared := range []bool{false, true} {
		name := "direct/two_subscribers"
		if shared {
			name = "shared_cache/two_subscribers"
		}
		b.Run(name, func(b *testing.B) {
			buf := NewBuffer(1)
			var observes atomic.Int64
			buf.SetModelObserver(func(full bool, _ ...[]*Record) any {
				if full {
					return map[string]string{"revision": "bench"}
				}
				observes.Add(1)
				return map[string]any{"model": "canonical", "revision": "bench"}
			})

			b.ReportAllocs()
			b.ResetTimer()
			payload := 0
			for b.Loop() {
				ev := RingEvent{Record: rec}
				if shared {
					ev.cache = sharedEventCache(2)
				}
				if _, err := buf.encodeRingEvent(ev); err != nil {
					b.Fatal(err)
				}
				data, err := buf.encodeRingEvent(ev)
				if err != nil {
					b.Fatal(err)
				}
				payload = len(data)
			}
			benchmarkEventPayloadBytes = payload
			b.ReportMetric(float64(observes.Load())/float64(b.N), "observer-calls/op")
			b.ReportMetric(float64(payload), "payload-bytes")
		})
	}
}
