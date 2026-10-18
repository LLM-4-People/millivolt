package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func observerBenchmarkNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("vendor.example/Model-%d.5:tag-fp4", i)
	}
	return names
}

func BenchmarkObserveModelNames4096(b *testing.B) {
	api := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	rules := config.Default().ModelCanon()
	api.ModelCanon = func() config.ModelCanon { return rules }
	names := observerBenchmarkNames(4096)
	b.ReportAllocs()
	for b.Loop() {
		if got := len(api.ObserveModelNames(names).(modelObserverState).Names); got != len(names) {
			b.Fatalf("names = %d, want %d", got, len(names))
		}
	}
}
