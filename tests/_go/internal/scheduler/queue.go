package scheduler

import (
	"context"
	"math/rand"
	"slices"
	"testing"
)

// TestWaiterQueueTracksLiveMembership pins the queue's ownership contract:
// push/remove preserve FIFO order, remove unlinks by back-pointer in O(1), a
// removed waiter is not findable, and an emptied queue keeps no reachable node.
// No scan API can observe a departed waiter because there is no tombstone slot.
func TestWaiterQueueTracksLiveMembership(t *testing.T) {
	var q waiterQueue
	ws := make([]*waiter, 5)
	for i := range ws {
		ws[i] = &waiter{ch: make(chan struct{}, 1), ctx: context.Background()}
		q.push(ws[i])
	}
	if q.live != len(ws) || q.head != ws[0] || q.tail != ws[len(ws)-1] {
		t.Fatalf("queue = %+v, want %d live waiters from head %p to tail %p", q, len(ws), ws[0], ws[len(ws)-1])
	}

	if !q.remove(ws[2]) || ws[2].queue != nil || ws[2].prev != nil || ws[2].next != nil {
		t.Fatal("removed waiter retained membership or links")
	}
	if q.remove(ws[2]) {
		t.Fatal("second removal reported a live waiter")
	}
	if q.live != len(ws)-1 {
		t.Fatalf("live = %d after removal, want %d", q.live, len(ws)-1)
	}
	var got []*waiter
	q.forEach(func(w *waiter) { got = append(got, w) })
	if want := []*waiter{ws[0], ws[1], ws[3], ws[4]}; !slices.Equal(got, want) {
		t.Fatalf("FIFO order = %v, want %v", got, want)
	}

	for _, w := range []*waiter{ws[0], ws[1], ws[3], ws[4]} {
		if !q.remove(w) {
			t.Fatalf("waiter %p was not removable", w)
		}
	}
	if q.live != 0 || q.head != nil || q.tail != nil {
		t.Fatalf("emptied queue retained state: %+v", q)
	}
	q.forEach(func(w *waiter) { t.Fatalf("empty queue yielded waiter %p", w) })
}

// TestWaiterQueueMatchesModel drives push/remove against a slice model and
// checks the live count and FIFO view after every operation. It covers
// arbitrary interleavings without relying on a particular scheduler scenario.
func TestWaiterQueueMatchesModel(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var q waiterQueue
	var model []*waiter
	for step := 0; step < 5000; step++ {
		if len(model) == 0 || rng.Intn(3) != 0 {
			w := &waiter{}
			q.push(w)
			model = append(model, w)
		} else {
			i := rng.Intn(len(model))
			if !q.remove(model[i]) {
				t.Fatalf("step %d: queued waiter %p was not removable", step, model[i])
			}
			model = append(model[:i], model[i+1:]...)
		}
		if q.live != len(model) {
			t.Fatalf("step %d: live = %d, want %d", step, q.live, len(model))
		}
		var got []*waiter
		q.forEach(func(w *waiter) { got = append(got, w) })
		if !slices.Equal(got, model) {
			t.Fatalf("step %d: FIFO view = %v, want %v", step, got, model)
		}
	}
}
