package scheduler

// waiterQueue is a group's FIFO of parked Acquire callers. It owns membership
// and the live count: a cancelled waiter is unlinked in O(1), so there is no
// tombstone slot a reader could mistake for a live waiter and no compaction
// pass to defer. Every node reachable from head is a live waiter. The zero
// value is empty. All access is guarded by the owning group's mu.
type waiterQueue struct {
	head *waiter
	tail *waiter
	live int
}

// push appends w at the tail.
func (q *waiterQueue) push(w *waiter) {
	w.queue = q
	w.prev = q.tail
	w.next = nil
	if q.tail != nil {
		q.tail.next = w
	} else {
		q.head = w
	}
	q.tail = w
	q.live++
}

// remove unlinks w and reports whether it was still queued. A waiter granted
// by drain is unlinked at grant, so a later cancellation sees false and
// follows the phantom-slot path in removeWaiter.
func (q *waiterQueue) remove(w *waiter) bool {
	if w == nil || w.queue != q {
		return false
	}
	if w.prev != nil {
		w.prev.next = w.next
	} else {
		q.head = w.next
	}
	if w.next != nil {
		w.next.prev = w.prev
	} else {
		q.tail = w.prev
	}
	w.queue, w.prev, w.next = nil, nil, nil
	q.live--
	return true
}

// forEach calls fn for every live waiter in FIFO order. It is the only
// read-only walk of the queue, so consumers never see a removed waiter.
func (q *waiterQueue) forEach(fn func(*waiter)) {
	for w := q.head; w != nil; w = w.next {
		fn(w)
	}
}
