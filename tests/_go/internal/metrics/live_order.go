package metrics

import (
	"sync"
	"testing"
)

// TestPublishLiveUpdateNeverFollowsEnd pins the shared publication lock order:
// Record retires pending and publishes end under subsMu, so PublishLive must
// take subsMu before mu and hold it across its fan-out. Under the old
// mu-then-subsMu order, an update could register pending, release mu, then wait
// while Record retired the entry and delivered end; the delayed update then
// reached subscribers after end, leaving a dashboard streaming row for a
// finished request. The old order produced [begin end update] around trial 331
// in local runs; the bounded loop below makes that schedule observable without
// sleeps.
func TestPublishLiveUpdateNeverFollowsEnd(t *testing.T) {
	const trials = 5000
	for trial := range trials {
		b := NewBuffer(4)
		ch := b.SubscribeLive()
		rec := &Record{ID: "lifecycle", Stream: true}

		b.PublishLive("begin", rec)
		if ev := <-ch; ev.Phase != "begin" {
			t.Fatalf("trial %d: first event = %q, want begin", trial, ev.Phase)
		}

		// Record bounds its argument in place, while this test deliberately
		// races publication calls. Give completion its own copy of the same
		// logical record so the interleaving exercises the lock order without
		// racing test-owned memory; pending is keyed by ID, so the lifecycle
		// transition is identical.
		final := *rec
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			b.PublishLive("update", rec)
		}()
		go func() {
			defer wg.Done()
			<-start
			b.Record(&final)
		}()
		close(start)
		wg.Wait()

		// Both publishers have returned, so every fan-out send has completed
		// and the buffered channel can no longer grow.
		phases := make([]string, 0, 2)
		for range len(ch) {
			phases = append(phases, (<-ch).Phase)
		}
		switch {
		case len(phases) == 1 && phases[0] == "end":
			// The update lost the race and was dropped after completion.
		case len(phases) == 2 && phases[0] == "update" && phases[1] == "end":
			// The update registered before completion and was published first.
		default:
			t.Fatalf("trial %d: events after begin = %v, want [update end] or [end] (end must never precede update)", trial, phases)
		}
		b.UnsubscribeLive(ch)
	}
}
