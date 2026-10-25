package proxy

import (
	"runtime"
	"testing"
)

// allocStats returns the bytes and objects allocated by one measured call
// after a warm-up. testing.Benchmark exists to measure time adaptively; these
// tests only assert allocation ceilings, so its timed loop adds a benchtime
// ramp to every plain test without changing what the allocation counters say.
// testing.AllocsPerRun keeps the warm-up-and-GC discipline the ceilings were
// calibrated against, and the runtime memory counters around one further call
// give the byte figure testing.Benchmark reported as AllocedBytesPerOp.
func allocStats(fn func()) (bytes int64, allocs float64) {
	allocs = testing.AllocsPerRun(1, fn)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return int64(after.TotalAlloc - before.TotalAlloc), allocs
}
