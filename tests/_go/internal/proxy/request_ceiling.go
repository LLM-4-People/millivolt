package proxy

import (
	"runtime"
	"testing"
)

// TestProxyRequestAllocationCeiling mirrors the 1 KiB prompt with 100 streamed
// frames through the real ServeHTTP path: request parsing, SSE analysis/relay
// and bounded ring accounting, with no network or provider time. The 100-frame
// probe's plain-build steady state is about 34,960 B/op and 172 allocs/op; the
// 1,000-frame BenchmarkProxyRequest case is about 202 allocs/op and 62 KB/op.
// The 37,000 B ceiling leaves roughly 2 KB (5.8%) over the probe while still
// rejecting an added per-request 4 KiB temporary buffer (34,960 + 4,096 >
// 37,000). The default shard runs this test without the race detector, where
// that byte ceiling is meaningful; the race pass asserts allocations only.
// Under -race shadow memory inflates B/op and the observed count is 190-191,
// so the race allocation ceiling is 220 (at least 15% headroom). The plain
// allocation ceiling stays 200, about 16% over 172.
func TestProxyRequestAllocationCeiling(t *testing.T) {
	// sync.Pool shards per P. A benchmark goroutine that migrates between Ps
	// strands a warm entry and measures a cold request (+5.6 KB, +20
	// allocations), which made -benchtime=1x fail in about one run in eight
	// even after two warm-ups. Pin one P so the single-iteration measurement
	// reflects steady-state reuse; the ceiling is per request, not scheduler
	// placement. Restore the setting for the rest of the package.
	previousProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previousProcs)
	result := testing.Benchmark(func(b *testing.B) {
		s, payload, response := proxyRequestFixture(1024, 100)
		// Warm twice: the first request initializes lazy relay and analyzer
		// state, and the second re-primes the per-request pools after the
		// runtime GC that testing.Benchmark runs before this function.
		for range 2 {
			serveProxyBenchmarkRequest(b, s, payload, response)
		}
		b.ReportAllocs()
		for b.Loop() {
			serveProxyBenchmarkRequest(b, s, payload, response)
		}
	})
	t.Logf("ServeHTTP = %d B/op %d allocs/op (N=%d)", result.AllocedBytesPerOp(), result.AllocsPerOp(), result.N)
	if !raceInstrumented {
		if got := result.AllocedBytesPerOp(); got > 37_000 {
			t.Fatalf("ServeHTTP = %d B/op, want <= 37000", got)
		}
	}
	allocCeiling := int64(200)
	if raceInstrumented {
		allocCeiling = 220
	}
	if got := result.AllocsPerOp(); got > allocCeiling {
		t.Fatalf("ServeHTTP = %d allocs/op, want <= %d", got, allocCeiling)
	}
}
