package scheduler

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// groupCount returns the retained group count under the scheduler lock.
func groupCount(s *Scheduler) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.groups)
}

func groupPresent(s *Scheduler, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.groups[key]
	return ok
}

func schedulerGroup(s *Scheduler, key string) *group {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.groups[key]
}

// TestProductionGroupCap pins the production bound and its test seam: eviction
// exercises a small per-instance cap, never a lowered global bound.
func TestProductionGroupCap(t *testing.T) {
	if maxSchedulerGroups != 100_000 {
		t.Fatalf("maxSchedulerGroups = %d, want 100000", maxSchedulerGroups)
	}
	if got := New(Options{}).groupCap; got != maxSchedulerGroups {
		t.Fatalf("New groupCap = %d, want production bound %d", got, maxSchedulerGroups)
	}
}

// TestIdleGroupEvictionAdmitsNewKey is the H1 regression: completed acquisitions
// used to fill the group cap permanently. At the cap the least-recently-idle
// group must be evicted from the map and provider index so a brand-new key is
// admitted and retained memory stays bounded.
func TestIdleGroupEvictionAdmitsNewKey(t *testing.T) {
	s := New(Options{})
	s.groupCap = 4
	ctx := context.Background()
	const provider = "evict.example"
	oldest := provider + "|k0"

	for i := 0; i < s.groupCap; i++ {
		key := fmt.Sprintf("%s|k%d", provider, i)
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatalf("AcquireWith(%s) = %v", key, err)
		}
		release(0)
	}
	if got := groupCount(s); got != s.groupCap {
		t.Fatalf("retained groups = %d, want cap %d", got, s.groupCap)
	}

	release, err := s.AcquireWith(ctx, provider+"|new", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("brand-new key after %d completed groups: %v", s.groupCap, err)
	}
	release(0)

	if groupPresent(s, oldest) {
		t.Fatalf("least-recently-idle completed group %q was not evicted", oldest)
	}
	if got := groupCount(s); got > s.groupCap {
		t.Fatalf("retained groups = %d, want <= cap %d", got, s.groupCap)
	}
	groups := providerGroupsSnapshot(s, provider)
	if len(groups) != groupCount(s) {
		t.Fatalf("provider index retains %d groups, map retains %d", len(groups), groupCount(s))
	}
	for _, g := range groups {
		if g.key == oldest {
			t.Fatalf("provider index retained evicted group %q", oldest)
		}
	}
}

// TestIdleGroupEvictionNeverRemovesActiveWork pins the eviction precondition:
// a group with an in-flight slot, and a group with a queued waiter, must
// survive pressure that evicts every idle group.
func TestIdleGroupEvictionNeverRemovesActiveWork(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	s.groupCap = 4
	ctx := context.Background()
	const provider = "active.example"

	activeKey := provider + "|active"
	activeRelease, err := s.AcquireWith(ctx, activeKey, 1, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("active acquire: %v", err)
	}
	defer activeRelease(0)
	activeGroup := schedulerGroup(s, activeKey)
	if activeGroup == nil {
		t.Fatal("active group missing after acquire")
	}

	queuedKey := provider + "|queued"
	holderRelease, err := s.AcquireWith(ctx, queuedKey, 1, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("queued holder acquire: %v", err)
	}
	defer holderRelease(0)
	type result struct {
		release Release
		err     error
	}
	waiter := make(chan result, 1)
	go func() {
		release, err := s.AcquireWith(ctx, queuedKey, 1, WaiterHooks{Provider: provider})
		waiter <- result{release: release, err: err}
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 1 }, "queued waiter to park")
	queuedGroup := schedulerGroup(s, queuedKey)
	if queuedGroup == nil {
		t.Fatal("queued group missing after enqueue")
	}

	// Fill the remaining two slots with completed groups, then force
	// evictions with brand-new keys while the active and queued groups remain.
	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("%s|idle-%d", provider, i)
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatalf("idle fill acquire %s: %v", key, err)
		}
		release(0)
	}
	if got := groupCount(s); got != s.groupCap {
		t.Fatalf("retained groups = %d, want cap %d before eviction pressure", got, s.groupCap)
	}

	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("%s|new-%d", provider, i)
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatalf("new key %s under idle pressure: %v", key, err)
		}
		release(0)
	}

	if got := schedulerGroup(s, activeKey); got != activeGroup {
		t.Fatalf("in-flight group was evicted or replaced: got %p want %p", got, activeGroup)
	}
	if got := schedulerGroup(s, queuedKey); got != queuedGroup {
		t.Fatalf("queued group was evicted or replaced: got %p want %p", got, queuedGroup)
	}
	if st := s.Stats(); st.InFlight != 2 || st.Queued != 1 {
		t.Fatalf("stats after idle evictions = %+v, want in_flight=2 queued=1", st)
	}
	if got := groupCount(s); got > s.groupCap {
		t.Fatalf("retained groups = %d, want <= cap %d", got, s.groupCap)
	}

	// Drain the queued group to leave the scheduler quiescent.
	holderRelease(0)
	res := <-waiter
	if res.err != nil {
		t.Fatalf("queued acquire after pressure: %v", res.err)
	}
	res.release(0)
}

// TestGroupLimitStillRefusesWhenEveryGroupIsActive proves eviction did not
// weaken the hard bound: when every retained group has live work, a new key is
// still refused, and the same key is admitted once an idle slot exists.
func TestGroupLimitStillRefusesWhenEveryGroupIsActive(t *testing.T) {
	s := New(Options{})
	s.groupCap = 2
	ctx := context.Background()
	const provider = "limit.example"

	releaseA, err := s.AcquireWith(ctx, provider+"|a", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	defer releaseA(0)
	releaseB, err := s.AcquireWith(ctx, provider+"|b", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	defer releaseB(0)

	if _, err := s.AcquireWith(ctx, provider+"|c", 0, WaiterHooks{Provider: provider}); !errors.Is(err, ErrGroupLimit) {
		t.Fatalf("new key with every group active = %v, want ErrGroupLimit", err)
	}
	if got := groupCount(s); got != s.groupCap {
		t.Fatalf("retained groups = %d, want %d after refusal", got, s.groupCap)
	}

	releaseA(0)
	releaseC, err := s.AcquireWith(ctx, provider+"|c", 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("new key after an idle group appeared: %v", err)
	}
	releaseC(0)
	if groupPresent(s, provider+"|a") {
		t.Fatal("idle group a was not reclaimed once the cap needed room")
	}
}

// TestEvictionKeepsProviderAccountingCoherent checks that removal updates the
// provider index, its queued-group counter and the idle list without touching
// live occupancy.
func TestEvictionKeepsProviderAccountingCoherent(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	s.groupCap = 4
	ctx := context.Background()
	const provider = "account.example"

	holderKey := provider + "|holder"
	holderRelease, err := s.AcquireWith(ctx, holderKey, 1, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	defer holderRelease(0)

	queuedKey := provider + "|queued"
	queuedHolderRelease, err := s.AcquireWith(ctx, queuedKey, 1, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("queued holder acquire: %v", err)
	}
	defer queuedHolderRelease(0)
	type result struct {
		release Release
		err     error
	}
	waiter := make(chan result, 1)
	go func() {
		release, err := s.AcquireWith(ctx, queuedKey, 1, WaiterHooks{Provider: provider})
		waiter <- result{release: release, err: err}
	}()
	waitFor(t, func() bool { return s.Stats().Queued == 1 }, "queued waiter to park")
	if got := providerQueuedGroups(s, provider); got != 1 {
		t.Fatalf("provider queued groups = %d, want 1 before eviction", got)
	}

	// Drain the provider concurrently with eviction so the index unlink and
	// the queued-group snapshot race each other under -race.
	stopDrains := make(chan struct{})
	var drainers sync.WaitGroup
	for i := 0; i < 4; i++ {
		drainers.Add(1)
		go func() {
			defer drainers.Done()
			for {
				select {
				case <-stopDrains:
					return
				default:
					s.drainProvider(provider)
					runtime.Gosched()
				}
			}
		}()
	}
	t.Cleanup(func() {
		close(stopDrains)
		drainers.Wait()
	})

	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("%s|idle-%d", provider, i)
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatalf("idle fill acquire %s: %v", key, err)
		}
		release(0)
	}
	before := s.Stats()
	if got := groupCount(s); got != s.groupCap {
		t.Fatalf("retained groups = %d, want cap %d", got, s.groupCap)
	}

	for i := 0; i < 2; i++ {
		key := fmt.Sprintf("%s|new-%d", provider, i)
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatalf("new key %s: %v", key, err)
		}
		release(0)
		if got := providerQueuedGroups(s, provider); got != 1 {
			t.Fatalf("provider queued groups = %d after eviction %d, want 1", got, i)
		}
		if st := s.Stats(); st != before {
			t.Fatalf("stats after eviction %d = %+v, want unchanged %+v", i, st, before)
		}
	}

	groups := providerGroupsSnapshot(s, provider)
	if len(groups) != groupCount(s) {
		t.Fatalf("provider index retains %d groups, map retains %d", len(groups), groupCount(s))
	}
	seen := make(map[*group]bool, len(groups))
	queuedGroups := 0
	queuedWaiters := 0
	for _, g := range groups {
		if seen[g] {
			t.Fatalf("provider index contains duplicate group %p", g)
		}
		seen[g] = true
		g.mu.Lock()
		if g.inFlight < 0 || g.queue.live < 0 {
			t.Fatalf("group %q has negative counters: in_flight=%d queued=%d", g.key, g.inFlight, g.queue.live)
		}
		if g.hasQueued.Load() != (g.queue.live > 0) {
			t.Fatalf("group %q witness=%v with queued=%d", g.key, g.hasQueued.Load(), g.queue.live)
		}
		if g.queue.live > 0 {
			queuedGroups++
			queuedWaiters += g.queue.live
		}
		g.mu.Unlock()
	}
	if queuedGroups != 1 || queuedWaiters != 1 {
		t.Fatalf("provider index holds %d queued groups/%d waiters, want 1/1", queuedGroups, queuedWaiters)
	}

	// Release both holders; the queued waiter must be granted exactly once and
	// the provider counter must return to zero.
	holderRelease(0)
	queuedHolderRelease(0)
	res := <-waiter
	if res.err != nil {
		t.Fatalf("queued acquire: %v", res.err)
	}
	res.release(0)
	if got := providerQueuedGroups(s, provider); got != 0 {
		t.Fatalf("provider queued groups = %d after drain, want 0", got)
	}
	if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
		t.Fatalf("stats after drain = %+v, want zero", st)
	}
}

// TestEvictionDropsEmptyProviderIndex pins the provider-index side of
// eviction: the last group of a provider must remove its index entry, so
// rotating providers cannot retain one empty entry per provider.
func TestEvictionDropsEmptyProviderIndex(t *testing.T) {
	s := New(Options{})
	s.groupCap = 1
	ctx := context.Background()

	for _, provider := range []string{"first.example", "second.example"} {
		key := provider + "|key"
		release, err := s.AcquireWith(ctx, key, 0, WaiterHooks{Provider: provider})
		if err != nil {
			t.Fatalf("AcquireWith(%s) = %v", key, err)
		}
		release(0)
	}

	s.mu.Lock()
	_, firstLive := s.providerGroups["first.example"]
	_, secondLive := s.providerGroups["second.example"]
	s.mu.Unlock()
	if firstLive {
		t.Fatal("evicted provider retained an empty index entry")
	}
	if !secondLive {
		t.Fatal("live provider index entry was dropped")
	}
}

// TestPacingLRUIsBounded pins the explicit hot-state bound and access order:
// the oldest pacing state is evicted past maxSchedulerPacingStates.
func TestPacingLRUIsBounded(t *testing.T) {
	var lru pacingLRU
	total := maxSchedulerPacingStates + 8
	for i := 0; i < total; i++ {
		lru.put(fmt.Sprintf("k%d", i), pacingState{backoff: time.Duration(i)})
	}
	lru.mu.Lock()
	size := len(lru.states)
	_, oldestLive := lru.states["k0"]
	lru.mu.Unlock()
	if size != maxSchedulerPacingStates {
		t.Fatalf("pacing LRU size = %d, want bound %d", size, maxSchedulerPacingStates)
	}
	if oldestLive {
		t.Fatal("pacing LRU did not evict its oldest entry at the bound")
	}
}

// TestIdleEvictionPreservesRecentPacing pins the bounded pacing handoff: an
// idle group evicted as the LRU victim carries its next-allowed window into the
// replacement group, so a returning key keeps its recent backoff at the cap.
func TestIdleEvictionPreservesRecentPacing(t *testing.T) {
	s := New(Options{})
	s.groupCap = 2
	ctx := context.Background()
	const provider = "pacing.example"
	pacedKey := provider + "|paced"

	release, err := s.AcquireWith(ctx, pacedKey, 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("paced acquire: %v", err)
	}
	release(0)
	const window = 400 * time.Millisecond
	s.Trip(pacedKey, window)

	// Touch another key after the paced one so the paced group is the LRU
	// victim, then force an eviction with a third key.
	fillerKey := provider + "|filler"
	release, err = s.AcquireWith(ctx, fillerKey, 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("filler acquire: %v", err)
	}
	release(0)
	triggerKey := provider + "|trigger"
	release, err = s.AcquireWith(ctx, triggerKey, 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("trigger acquire: %v", err)
	}
	release(0)

	start := time.Now()
	release, err = s.AcquireWith(ctx, pacedKey, 0, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("paced re-acquire: %v", err)
	}
	elapsed := time.Since(start)
	release(0)
	if elapsed < window/2 {
		t.Fatalf("paced re-acquire returned in %v; the evicted group's %v window was lost", elapsed, window)
	}
}

// assertSchedulerQuiescent checks the post-stress invariants: no occupancy,
// bounded map, no duplicate provider links, and an idle list that covers every
// retained group with idle counters.
func assertSchedulerQuiescent(t *testing.T, s *Scheduler, provider string) {
	t.Helper()
	if st := s.Stats(); st.InFlight != 0 || st.Queued != 0 {
		t.Fatalf("stats after stress = %+v, want zero occupancy", st)
	}
	if got := groupCount(s); got > s.groupCap {
		t.Fatalf("retained groups = %d, want <= cap %d", got, s.groupCap)
	}
	groups := providerGroupsSnapshot(s, provider)
	if len(groups) != groupCount(s) {
		t.Fatalf("provider index retains %d groups, map retains %d", len(groups), groupCount(s))
	}
	seen := make(map[*group]bool, len(groups))
	for _, g := range groups {
		if seen[g] {
			t.Fatalf("provider index contains duplicate group %p", g)
		}
		seen[g] = true
	}
	if got := providerQueuedGroups(s, provider); got != 0 {
		t.Fatalf("provider queued groups = %d after stress, want 0", got)
	}

	s.idleMu.Lock()
	listed := make([]*group, 0, s.idle.live)
	s.idle.forEach(func(g *group) { listed = append(listed, g) })
	s.idleMu.Unlock()
	if len(listed) != len(groups) {
		t.Fatalf("idle list holds %d groups, map holds %d", len(listed), len(groups))
	}
	for _, g := range listed {
		if !seen[g] {
			t.Fatalf("idle list retains group %p absent from the map", g)
		}
		g.mu.Lock()
		if g.inFlight != 0 || g.queue.live != 0 || g.retired {
			t.Fatalf("idle group %q has in_flight=%d queued=%d retired=%v", g.key, g.inFlight, g.queue.live, g.retired)
		}
		g.mu.Unlock()
	}
}

// TestConcurrentGroupEvictionStress hammers a small cap with concurrent
// acquire/release so eviction, provider accounting and the idle LRU race each
// other. Every acquisition must eventually succeed while retained groups stay
// bounded, and completed groups must never leave a permanent ErrGroupLimit.
func TestConcurrentGroupEvictionStress(t *testing.T) {
	s := New(Options{MaxConcurrent: 1})
	s.groupCap = 8
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const provider = "stress.example"
	const workers = 6

	keys := make([]string, 32)
	for i := range keys {
		keys[i] = fmt.Sprintf("%s|k%d", provider, i)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < 300; i++ {
				key := keys[(w*7+i*13)%len(keys)]
				release, err := s.AcquireWith(ctx, key, 1, WaiterHooks{Provider: provider})
				if err != nil {
					t.Errorf("worker %d acquire %s at iteration %d: %v", w, key, i, err)
					return
				}
				release(0)
			}
		}(w)
	}
	close(start)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
monitor:
	for {
		select {
		case <-done:
			break monitor
		case <-ticker.C:
			if got := groupCount(s); got > s.groupCap {
				t.Fatalf("retained groups = %d under stress, want <= cap %d", got, s.groupCap)
			}
			st := s.Stats()
			if st.InFlight < 0 || st.Queued < 0 || st.InFlight > workers {
				t.Fatalf("invalid stats under stress: %+v", st)
			}
			if got := providerQueuedGroups(s, provider); got < 0 {
				t.Fatalf("negative provider queued-group counter: %d", got)
			}
		}
	}

	assertSchedulerQuiescent(t, s, provider)

	// A brand-new key must still be admitted after every completed group.
	release, err := s.AcquireWith(ctx, provider+"|final", 1, WaiterHooks{Provider: provider})
	if err != nil {
		t.Fatalf("new key after completed stress groups: %v", err)
	}
	release(0)
	assertSchedulerQuiescent(t, s, provider)
}
