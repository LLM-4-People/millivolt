package proxy

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
