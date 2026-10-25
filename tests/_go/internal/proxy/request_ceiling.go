package proxy

import (
	"testing"
)

// TestProxyRequestAllocationCeiling mirrors the 1 KiB prompt with 100 streamed
// frames through the real ServeHTTP path: request parsing, SSE analysis/relay
// and bounded ring accounting, with no network or provider time. The warm-up
// keeps one-time lazy setup out of the measured loop. The measured plain-build
// steady state is about 34,960 B/op and 172 allocs/op; the 37,000 B ceiling
// leaves roughly 2 KB (5.8%) of headroom while still rejecting an added
// per-request 4 KiB temporary buffer (34,960 + 4,096 > 37,000). The default
// shard runs this test without the race detector, where that byte ceiling is
// meaningful. Under -race shadow memory inflates B/op, so race builds assert
// allocations only; the 200-allocation ceiling leaves about 16% headroom.
func TestProxyRequestAllocationCeiling(t *testing.T) {
	for run := 0; run < 5; run++ {
		result := testing.Benchmark(func(b *testing.B) {
			s, payload, response := proxyRequestFixture(1024, 100)
			serveProxyBenchmarkRequest(b, s, payload, response)
			b.ReportAllocs()
			for b.Loop() {
				serveProxyBenchmarkRequest(b, s, payload, response)
			}
		})
		t.Logf("run %d: %d B/op %d allocs/op", run, result.AllocedBytesPerOp(), result.AllocsPerOp())
		if !raceInstrumented {
			if got := result.AllocedBytesPerOp(); got > 37_000 {
				t.Fatalf("run %d: ServeHTTP = %d B/op, want <= 37000", run, got)
			}
		}
		if got := result.AllocsPerOp(); got > 200 {
			t.Fatalf("run %d: ServeHTTP = %d allocs/op, want <= 200", run, got)
		}
	}
}
