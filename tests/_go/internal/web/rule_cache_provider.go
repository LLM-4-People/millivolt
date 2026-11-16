package web

import (
	"fmt"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func ruleCacheBenchRules(count int) []config.ModelRule {
	rules := make([]config.ModelRule, count)
	for i := range rules {
		rules[i] = config.ModelRule{Mode: config.ModelRuleLower}
	}
	return rules
}

// TestRuleCacheShortcutAnswersRepeatedSnapshotWithoutAllocating recreates the
// production provider shape: main.go derives the canon from the live *Config on
// every event. The pointer shortcut in modelRuleCache.get only skips the element
// compare when the caller's slice is the one the last get saw, so the provider
// must hand back the same backing array per live config; otherwise every SSE
// event pays a full 64-rule copy just to prove the rules did not change.
func TestRuleCacheShortcutAnswersRepeatedSnapshotWithoutAllocating(t *testing.T) {
	cfg := config.Default()
	cfg.ModelRules = ruleCacheBenchRules(64)
	var memo config.ModelCanonMemo
	var cache modelRuleCache
	warm := cache.get(memo.Canon(cfg))
	if len(warm.exec) != len(cfg.ModelRules) {
		t.Fatalf("compiled %d exec rules, want %d", len(warm.exec), len(cfg.ModelRules))
	}
	first, second := memo.Canon(cfg), memo.Canon(cfg)
	if &first.Rules[0] != &second.Rules[0] {
		t.Fatal("the provider copied the rule slice between two reads of one config")
	}
	if &first.Rules[0] == &cfg.ModelRules[0] {
		t.Fatal("the shared canon aliases the live config instead of its own copy")
	}
	if allocs := testing.AllocsPerRun(100, func() { cache.get(memo.Canon(cfg)) }); allocs != 0 {
		t.Fatalf("100 unchanged events allocated %v per event; the compiled-rule shortcut never fired on the production provider", allocs)
	}
}

// TestModelCanonMemoServesReloadedRules: the memo is keyed by pointer, not
// content, so a reload publishes its new rules even when the values are equal.
func TestModelCanonMemoServesReloadedRules(t *testing.T) {
	cfg := config.Default()
	cfg.ModelRules = []config.ModelRule{{Mode: config.ModelRuleExact, From: "a", To: "b"}}
	var memo config.ModelCanonMemo
	if got := config.ApplyModelRules(config.CompileModelRules(memo.Canon(cfg).Rules), "a"); got != "b" {
		t.Fatalf("canonicalized a to %q, want b", got)
	}
	same := cfg.Clone()
	reloaded := memo.Canon(same)
	if &reloaded.Rules[0] == &memo.Canon(cfg).Rules[0] {
		t.Fatal("a reloaded config reused the previous canon")
	}
	if got := config.ApplyModelRules(config.CompileModelRules(reloaded.Rules), "a"); got != "b" {
		t.Fatalf("reloaded canon canonicalized a to %q, want b", got)
	}
}

// TestRuleCacheShortcutSurvivesReload: the shortcut is a cache, never a rule
// rollback. A config swap with different content must still recompile, and
// swapping back must serve the restored content.
func TestRuleCacheShortcutSurvivesReload(t *testing.T) {
	lower := config.Default()
	lower.ModelRules = ruleCacheBenchRules(2)
	exact := lower.Clone()
	exact.ModelRules = []config.ModelRule{{Mode: config.ModelRuleExact, From: "a", To: "b"}}

	var memo config.ModelCanonMemo
	var cache modelRuleCache
	before := cache.get(memo.Canon(lower))
	changed := cache.get(memo.Canon(exact))
	if before.revision == changed.revision {
		t.Fatal("a reloaded rule set kept the previous revision")
	}
	if got := config.ApplyModelRules(changed.exec, "a"); got != "b" {
		t.Fatalf("reloaded rules canonicalized a to %q, want b", got)
	}
	if got := config.ApplyModelRules(changed.exec, "A"); got != "A" {
		t.Fatalf("reloaded rules applied the superseded lower rule: %q", got)
	}
	restored := cache.get(memo.Canon(exact.Clone()))
	if restored.revision != changed.revision {
		t.Fatalf("restoring equal content changed revision %q -> %q", changed.revision, restored.revision)
	}
}

// BenchmarkObserveModelNameEventWired is BenchmarkObserveModelNameEvent's shape
// wired like main.go: the provider derives the canon from the live config per
// event instead of closing over a fixed slice. The gap between the two
// benchmarks is the whole cost of a provider whose copies defeat the shortcut.
func BenchmarkObserveModelNameEventWired(b *testing.B) {
	rec := &metrics.Record{ID: "record", Model: "vendor.example/Model-1.5:tag-fp4"}
	group := []*metrics.Record{rec}
	for _, count := range []int{0, 64} {
		cfg := config.Default()
		cfg.ModelRules = ruleCacheBenchRules(count)
		var memo config.ModelCanonMemo
		api := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
		api.ModelCanon = func() config.ModelCanon { return memo.Canon(cfg) }
		b.Run(fmt.Sprintf("rules%d", count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = api.ObserveModels(false, group)
			}
		})
	}
}
