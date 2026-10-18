package web

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func observerExpected(t *testing.T, rules config.ModelCanon, names []string) (string, map[string]string) {
	t.Helper()
	var cache modelRuleCache
	compiled := cache.get(rules)
	want := make(map[string]string, len(names))
	for _, name := range names {
		if _, exists := want[name]; !exists {
			want[name] = config.ApplyModelRules(compiled.exec, name)
		}
	}
	return compiled.revision, want
}

func observerMemoSnapshot(t *testing.T, a *AggAPI) (string, map[string]string) {
	t.Helper()
	a.modelNames.mu.Lock()
	defer a.modelNames.mu.Unlock()
	return a.modelNames.revision, maps.Clone(a.modelNames.names)
}

func TestObserveModelNamesMemoMatchesApplyModelRules(t *testing.T) {
	names := []string{
		"", "   ", "MiXeD", "MiXeD", "  padded  ",
		"vendor.example/Model-1.5:tag-fp4", "OTHER/model", "special",
	}
	cases := []struct {
		name  string
		rules []config.ModelRule
	}{
		{name: "identity"},
		{name: "lower", rules: []config.ModelRule{{Mode: config.ModelRuleLower}}},
		{name: "exact", rules: []config.ModelRule{{Mode: config.ModelRuleExact, From: "MiXeD", To: "canonical"}}},
		{name: "pattern", rules: []config.ModelRule{
			{Mode: config.ModelRulePattern, From: `^[^/]+/`, To: ""},
			{Mode: config.ModelRulePattern, From: `:[^:]+$`, To: ""},
		}},
		{name: "mixed", rules: []config.ModelRule{
			{Mode: config.ModelRuleLower},
			{Mode: config.ModelRuleExact, From: "special", To: "merged"},
			{Mode: config.ModelRulePattern, From: `-\d+\.\d+$`, To: ""},
			{Mode: config.ModelRuleLower, Disabled: true, From: "parked"},
		}},
		{name: "default", rules: config.DefaultModelRules()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules := config.ModelCanon{Rules: tc.rules}
			a := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
			a.ModelCanon = func() config.ModelCanon { return rules }
			revision, want := observerExpected(t, rules, names)
			got := a.ObserveModelNames(names).(modelObserverState)
			if got.Revision != revision {
				t.Fatalf("revision = %q, want %q", got.Revision, revision)
			}
			if !maps.Equal(got.Names, want) {
				t.Fatalf("names = %v, want %v", got.Names, want)
			}
			records := make([]*metrics.Record, 0, len(names)+1)
			for _, name := range names {
				records = append(records, &metrics.Record{Model: name})
			}
			records = append(records, nil)
			viaModels := a.ObserveModels(false, records).(modelObserverState)
			if !maps.Equal(viaModels.Names, want) {
				t.Fatalf("ObserveModels names = %v, want %v", viaModels.Names, want)
			}
		})
	}
}

func TestObserveModelNamesMemoReusesAndStaysBounded(t *testing.T) {
	rules := config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	a := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	a.ModelCanon = func() config.ModelCanon { return rules }
	_, want := observerExpected(t, rules, []string{"A", "B", "C"})

	first := a.ObserveModelNames([]string{"A", "B", "C"}).(modelObserverState)
	if !maps.Equal(first.Names, want) {
		t.Fatalf("first names = %v, want %v", first.Names, want)
	}
	revision, stored := observerMemoSnapshot(t, a)
	if len(stored) != len(want) {
		t.Fatalf("memo size = %d, want %d", len(stored), len(want))
	}
	if !maps.Equal(stored, want) {
		t.Fatalf("memo = %v, want %v", stored, want)
	}

	second := a.ObserveModelNames([]string{"A", "B", "C"}).(modelObserverState)
	revisionAfter, storedAfter := observerMemoSnapshot(t, a)
	if revisionAfter != revision || !maps.Equal(storedAfter, stored) {
		t.Fatalf("repeated call rebuilt memo: revision %q -> %q, names %v -> %v",
			revision, revisionAfter, stored, storedAfter)
	}
	if !maps.Equal(second.Names, first.Names) {
		t.Fatalf("repeated names = %v, want %v", second.Names, first.Names)
	}

	flood := make([]string, modelNameMemoMax+257)
	for i := range flood {
		flood[i] = fmt.Sprintf("flood.example/Model-%d.5:tag", i)
	}
	flooded := a.ObserveModelNames(flood).(modelObserverState)
	_, stored = observerMemoSnapshot(t, a)
	if len(stored) != modelNameMemoMax {
		t.Fatalf("flooded memo size = %d, want bound %d", len(stored), modelNameMemoMax)
	}
	wantExec := config.CompileModelRules(rules.Rules)
	for _, name := range flood {
		want := config.ApplyModelRules(wantExec, name)
		if flooded.Names[name] != want {
			t.Fatalf("flooded %q = %q, want %q", name, flooded.Names[name], want)
		}
	}
	// The names beyond the bound are still answered correctly but are not
	// retained; replaying them must not grow the memo further.
	for _, name := range flood[modelNameMemoMax-len(first.Names):] {
		if _, ok := stored[name]; ok {
			t.Fatalf("overflow name %q retained past bound", name)
		}
	}
	replayed := a.ObserveModelNames(flood[modelNameMemoMax-len(first.Names):]).(modelObserverState)
	if _, stored = observerMemoSnapshot(t, a); len(stored) != modelNameMemoMax {
		t.Fatalf("replayed memo size = %d, want bound %d", len(stored), modelNameMemoMax)
	}
	for name, got := range replayed.Names {
		if want := config.ApplyModelRules(wantExec, name); got != want {
			t.Fatalf("replayed %q = %q, want %q", name, got, want)
		}
	}
}

func TestObserveModelNamesMemoResetsOnRevisionChange(t *testing.T) {
	a := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	lower := config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	a.ModelCanon = func() config.ModelCanon { return lower }
	oldRevision, oldWant := observerExpected(t, lower, []string{"MiXeD", "only-old"})
	first := a.ObserveModelNames([]string{"MiXeD", "only-old"}).(modelObserverState)
	if first.Revision != oldRevision || !maps.Equal(first.Names, oldWant) {
		t.Fatalf("lower observer = %+v, want %v", first, oldWant)
	}

	exact := config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleExact, From: "MiXeD", To: "replacement"}}}
	newRevision, newWant := observerExpected(t, exact, []string{"MiXeD"})
	a.ModelCanon = func() config.ModelCanon { return exact }
	empty := a.ObserveModelNames(nil).(modelObserverState)
	if empty.Revision != newRevision {
		t.Fatalf("empty observer revision = %q, want %q", empty.Revision, newRevision)
	}
	if revision, stored := observerMemoSnapshot(t, a); revision != newRevision || len(stored) != 0 {
		t.Fatalf("revision change retained old memo: revision=%q names=%v", revision, stored)
	}
	second := a.ObserveModelNames([]string{"MiXeD"}).(modelObserverState)
	if second.Revision != newRevision || second.Revision == oldRevision {
		t.Fatalf("new revision = %q, want %q distinct from %q", second.Revision, newRevision, oldRevision)
	}
	if second.Names["MiXeD"] != newWant["MiXeD"] {
		t.Fatalf("new canonical value = %q, want %q", second.Names["MiXeD"], newWant["MiXeD"])
	}
	revision, stored := observerMemoSnapshot(t, a)
	if revision != newRevision {
		t.Fatalf("memo revision = %q, want %q", revision, newRevision)
	}
	if _, ok := stored["only-old"]; ok || len(stored) != 1 {
		t.Fatalf("old memo survived revision change: %v", stored)
	}

	a.ModelCanon = func() config.ModelCanon { return lower }
	third := a.ObserveModelNames([]string{"MiXeD"}).(modelObserverState)
	if third.Revision != oldRevision || third.Names["MiXeD"] != oldWant["MiXeD"] {
		t.Fatalf("restored lower observer = %+v, want %v", third, oldWant)
	}
}

func TestObserveModelNamesMemoPreservesDuplicateAndEmptyOutput(t *testing.T) {
	rules := config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	a := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	a.ModelCanon = func() config.ModelCanon { return rules }
	names := []string{"", "", " ", "  ", "DUP", "DUP", "dup"}
	got := a.ObserveModelNames(names).(modelObserverState)
	wantExec := config.CompileModelRules(rules.Rules)
	unique := make(map[string]string)
	for _, name := range names {
		unique[name] = config.ApplyModelRules(wantExec, name)
	}
	if len(got.Names) != len(unique) || !maps.Equal(got.Names, unique) {
		t.Fatalf("names = %v, want %v", got.Names, unique)
	}
	if got.Names[""] != "" || got.Names[" "] != "" || got.Names["DUP"] != "dup" {
		t.Fatalf("duplicate/empty output drifted: %v", got.Names)
	}
	if _, stored := observerMemoSnapshot(t, a); len(stored) != len(unique) {
		t.Fatalf("memo size = %d, want %d", len(stored), len(unique))
	}
}

func TestObserveModelNamesMemoConcurrentRuleChanges(t *testing.T) {
	variants := []config.ModelCanon{
		{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}},
		{Rules: []config.ModelRule{
			{Mode: config.ModelRuleLower},
			{Mode: config.ModelRuleLower, Disabled: true, From: "parked"},
		}},
		{Rules: []config.ModelRule{{Mode: config.ModelRuleExact, From: "MiXeD", To: "exact-shape"}}},
		{Rules: config.DefaultModelRules()},
	}
	names := []string{"MiXeD", "vendor.example/Model-1.5:tag-fp4", "THIRD"}
	expected := make(map[string]map[string]string, len(variants))
	for _, rules := range variants {
		revision, want := observerExpected(t, rules, names)
		expected[revision] = want
	}

	var current atomic.Pointer[config.ModelCanon]
	current.Store(&variants[0])
	a := NewAggAPI(metrics.NewBuffer(1), nil, time.Second)
	a.ModelCanon = func() config.ModelCanon { return *current.Load() }

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				current.Store(&variants[(worker+i)%len(variants)])
				var observed modelObserverState
				if i%2 == 0 {
					observed = a.ObserveModelNames(names).(modelObserverState)
				} else {
					records := make([]*metrics.Record, len(names))
					for j, name := range names {
						records[j] = &metrics.Record{Model: name}
					}
					observed = a.ObserveModels(false, records).(modelObserverState)
				}
				want, ok := expected[observed.Revision]
				if !ok {
					t.Errorf("unexpected revision %q", observed.Revision)
					return
				}
				for _, name := range names {
					if observed.Names[name] != want[name] {
						t.Errorf("revision %q name %q = %q, want %q",
							observed.Revision, name, observed.Names[name], want[name])
						return
					}
				}
			}
		}(worker)
	}
	wg.Wait()

	revision, stored := observerMemoSnapshot(t, a)
	if revision == "" || len(stored) > modelNameMemoMax {
		t.Fatalf("memo revision %q size %d", revision, len(stored))
	}
	if !slices.Contains([]string{
		observerExpectedRevision(t, variants[0]),
		observerExpectedRevision(t, variants[1]),
		observerExpectedRevision(t, variants[2]),
		observerExpectedRevision(t, variants[3]),
	}, revision) {
		t.Fatalf("memo revision %q is not a live variant", revision)
	}
}

func observerExpectedRevision(t *testing.T, rules config.ModelCanon) string {
	t.Helper()
	revision, _ := observerExpected(t, rules, nil)
	return revision
}
