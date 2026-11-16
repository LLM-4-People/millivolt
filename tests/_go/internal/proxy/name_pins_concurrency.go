package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// pinnedNames is the pinned subset of a vocabulary; list() also holds released
// names, so membership there cannot prove a pin.
func pinnedNames(n *nameSet) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Sorted(maps.Keys(n.pinned))
}

// TestNamePinsStaleDerivationCannotLandLast holds one pin derivation open
// between its read and its apply, commits a newer scope, and then lets the
// stale derivation apply. The last applier must not resurrect an older roster:
// a scope that exists now stays pinned.
func TestNamePinsStaleDerivationCannotLandLast(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	derived := make(chan struct{})
	release := make(chan struct{})
	// Only the first derivation blocks. sync.Once would hold the second
	// caller too, and the point is to let the newer roster land and apply.
	var armed atomic.Bool
	armed.Store(true)
	p.namePinHook = func() {
		if armed.CompareAndSwap(true, false) {
			close(derived)
			<-release
		}
	}
	stale := make(chan struct{})
	go func() {
		defer close(stale)
		p.syncNamePins()
	}()
	<-derived

	// A newer roster lands while the stale derivation is held open.
	if !p.scheduler.SetPolicy(scheduler.Throttle{
		Scope:     scheduler.LimitScope{Clients: []string{"policy-client"}},
		Limit:     scheduler.Limit{Requests: 1, ReqWindow: time.Minute},
		Source:    scheduler.ThrottleSourceUI,
		UpdatedBy: "test",
	}) {
		t.Fatal("SetPolicy reported no change")
	}
	fresh := make(chan struct{})
	go func() {
		defer close(fresh)
		p.syncNamePins()
	}()
	// An unfixed derivation applies while the stale one is still blocked; the
	// fixed one waits on the document locks, so give it a moment first.
	select {
	case <-fresh:
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-stale
	<-fresh

	if !slices.Contains(pinnedNames(&p.pause.clients), "policy-client") {
		t.Fatalf("stale derivation unpinned a live scope: %v", pinnedNames(&p.pause.clients))
	}
}

// TestHeaderLimitPinsItsProvider: a header limit is a live operator document
// scoped to its provider, so the provider must be pinned, not merely observed.
// An observed name is evictable from the bounded vocabulary, and losing it
// drops the provider from the pickers while the limit still scopes it.
func TestHeaderLimitPinsItsProvider(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	const provider = "hdr-provider"
	if err := p.updateThrottleFromHeaders(provider, func(cur scheduler.Throttle) scheduler.Throttle {
		cur.Limit.Concurrency = 2
		return cur
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pinnedNames(&p.pause.providers), provider) {
		t.Fatalf("the header limit did not pin its provider: %v", pinnedNames(&p.pause.providers))
	}
	for i := range nameSetMax + 16 {
		// The same call a finalized request makes for every new name.
		p.pause.providers.add(fmt.Sprintf("observed-%05d.example", i))
	}
	if !slices.Contains(p.pause.providers.list(), provider) {
		t.Fatal("a live limit's provider was evicted from the picker")
	}

	// An unchanged re-send of the same patch keeps the pin too.
	if err := p.updateThrottleFromHeaders(provider, func(cur scheduler.Throttle) scheduler.Throttle {
		cur.Limit.Concurrency = 2
		return cur
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(pinnedNames(&p.pause.providers), provider) {
		t.Fatalf("an unchanged header patch dropped the pin: %v", pinnedNames(&p.pause.providers))
	}
}

// staticThrottlePersist restores one fixed document.
type staticThrottlePersist struct{ raw []byte }

func (*staticThrottlePersist) SaveThrottle(context.Context, []byte) error { return nil }
func (p *staticThrottlePersist) LoadThrottle(context.Context) ([]byte, error) {
	return p.raw, nil
}

// TestRestoredThrottlePinsItsProvider is the boot twin: a policy restored from
// the store scopes its provider before any traffic, so its name must be pinned
// there too instead of waiting for the next operator write.
func TestRestoredThrottlePinsItsProvider(t *testing.T) {
	raw, err := json.Marshal(persistedThrottleDoc{Version: 1, Throttles: []persistedThrottle{
		{Provider: "restored-provider", Concurrency: 2},
	}})
	if err != nil {
		t.Fatal(err)
	}
	p := New(config.Default(), metrics.Noop{})
	p.restoreThrottlePolicies(&staticThrottlePersist{raw: raw})
	if !slices.Contains(pinnedNames(&p.pause.providers), "restored-provider") {
		t.Fatalf("a restored policy did not pin its provider: %v", pinnedNames(&p.pause.providers))
	}
}

// stale writer: every round's own sync is raced by a goroutine re-deriving
// pins from an older roster.
// TestNamePinsSurviveConcurrentRosterChurn runs remove/add rounds against a
// stale writer: every round's own sync is raced by a goroutine re-deriving
// pins from an older roster.
func TestNamePinsSurviveConcurrentRosterChurn(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	stop := make(chan struct{})
	var stale sync.WaitGroup
	stale.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				p.syncNamePins()
			}
		}
	})
	defer func() {
		close(stop)
		stale.Wait()
	}()
	for round := range 2000 {
		name := fmt.Sprintf("hold-%d", round)
		p.pause.mu.Lock()
		p.pause.holds = append(p.pause.holds, persistedPause{ID: name, Clients: []string{name}, MaxQueued: 1})
		p.pause.mu.Unlock()
		p.syncNamePins()
		if !slices.Contains(pinnedNames(&p.pause.clients), name) {
			t.Fatalf("round %d: added hold is not pinned", round)
		}
		p.pause.mu.Lock()
		p.pause.holds = slices.DeleteFunc(p.pause.holds, func(h persistedPause) bool { return h.ID == name })
		p.pause.mu.Unlock()
		p.syncNamePins()
		if slices.Contains(pinnedNames(&p.pause.clients), name) {
			t.Fatalf("round %d: removed hold is still pinned", round)
		}
	}
}
