package scheduler

import (
	"context"
	"math/rand"
	"slices"
	"testing"
)

// TestLinkedListTracksLiveMembership pins the shared membership contract:
// push/remove preserve FIFO order, remove unlinks by node in O(1), a removed
// value is not findable, and an emptied list keeps no reachable node. No scan
// API can observe a departed value because there is no tombstone slot.
func TestLinkedListTracksLiveMembership(t *testing.T) {
	var l linkedList[waiter]
	ws := make([]*waiter, 5)
	for i := range ws {
		ws[i] = &waiter{ch: make(chan struct{}, 1), ctx: context.Background()}
		l.push(&ws[i].queueNode, ws[i])
	}
	if l.live != len(ws) || l.head == nil || l.head.value != ws[0] || l.tail == nil || l.tail.value != ws[len(ws)-1] {
		t.Fatalf("list = %+v, want %d live waiters from head %p to tail %p", l, len(ws), ws[0], ws[len(ws)-1])
	}

	if !l.remove(&ws[2].queueNode) || ws[2].queueNode.owner != nil || ws[2].queueNode.prev != nil || ws[2].queueNode.next != nil {
		t.Fatal("removed waiter retained membership or links")
	}
	if l.remove(&ws[2].queueNode) {
		t.Fatal("second removal reported a live waiter")
	}
	if l.live != len(ws)-1 {
		t.Fatalf("live = %d after removal, want %d", l.live, len(ws)-1)
	}
	var got []*waiter
	l.forEach(func(w *waiter) { got = append(got, w) })
	if want := []*waiter{ws[0], ws[1], ws[3], ws[4]}; !slices.Equal(got, want) {
		t.Fatalf("FIFO order = %v, want %v", got, want)
	}

	for _, w := range []*waiter{ws[0], ws[1], ws[3], ws[4]} {
		if !l.remove(&w.queueNode) {
			t.Fatalf("waiter %p was not removable", w)
		}
	}
	if l.live != 0 || l.head != nil || l.tail != nil {
		t.Fatalf("emptied list retained state: %+v", l)
	}
	l.forEach(func(w *waiter) { t.Fatalf("empty list yielded waiter %p", w) })
}

// TestLinkedListMatchesModel drives push/remove against a slice model and
// checks the live count and FIFO view after every operation. It covers
// arbitrary interleavings without relying on a particular scheduler scenario.
func TestLinkedListMatchesModel(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var l linkedList[waiter]
	var model []*waiter
	for step := 0; step < 5000; step++ {
		if len(model) == 0 || rng.Intn(3) != 0 {
			w := &waiter{}
			l.push(&w.queueNode, w)
			model = append(model, w)
		} else {
			i := rng.Intn(len(model))
			if !l.remove(&model[i].queueNode) {
				t.Fatalf("step %d: queued waiter %p was not removable", step, model[i])
			}
			model = append(model[:i], model[i+1:]...)
		}
		if l.live != len(model) {
			t.Fatalf("step %d: live = %d, want %d", step, l.live, len(model))
		}
		var got []*waiter
		l.forEach(func(w *waiter) { got = append(got, w) })
		if !slices.Equal(got, model) {
			t.Fatalf("step %d: FIFO view = %v, want %v", step, got, model)
		}
	}
}
