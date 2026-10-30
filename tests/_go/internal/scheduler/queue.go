package scheduler

import (
	"context"
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
