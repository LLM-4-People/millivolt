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

// BenchmarkObserveModelNameEvent measures the SSE per-event observation: one
// record, one name. The rule-bearing shape is the one a 64-rule maximum costs
// per event, where the compiled-rule cache must answer without re-comparing the
// whole rule slice.
func BenchmarkObserveModelNameEvent(b *testing.B) {
	rec := &metrics.Record{ID: "record", Model: "vendor.example/Model-1.5:tag-fp4"}
	group := []*metrics.Record{rec}
	for _, count := range []int{0, 64} {
		rules := make([]config.ModelRule, count)
		for i := range rules {
			rules[i] = config.ModelRule{Mode: config.ModelRuleLower}
		}
		api := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
		api.ModelCanon = func() config.ModelCanon { return config.ModelCanon{Rules: rules} }
		b.Run(fmt.Sprintf("rules%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = api.ObserveModels(false, group)
			}
		})
	}
}
