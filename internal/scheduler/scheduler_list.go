package scheduler

// listNode is a value's membership record for one linkedList. A value that can
// belong to several collections carries one node per collection. owner is the
// single membership authority: a node is in a list exactly when owner points at
// it, and remove clears that pointer before the value is reachable again.
type listNode[T any] struct {
	value *T
	prev  *listNode[T]
	next  *listNode[T]
	owner *linkedList[T]
}

// linkedList is the scheduler's single doubly linked FIFO implementation. It
// owns links, order and the live count, so no collection keeps its own link
// code or a nil sentinel that a reader could mistake for a member. The zero
// value is empty. The owning collection's mutex guards every call; the list
// itself does not lock.
type linkedList[T any] struct {
	head *listNode[T]
	tail *listNode[T]
	live int
}

// push appends v at the tail through n. A node already linked into any list is
// an internal invariant violation, never a runtime input condition.
func (l *linkedList[T]) push(n *listNode[T], v *T) {
	if n.owner != nil {
		panic("scheduler: list node already linked")
	}
	n.value = v
	n.owner = l
	n.prev = l.tail
	n.next = nil
	if l.tail != nil {
		l.tail.next = n
	} else {
		l.head = n
	}
	l.tail = n
	l.live++
}

// remove unlinks n and reports whether it belonged to this list. A value that
// was already popped or transferred sees false, which is the queue's grant
// versus cancellation distinction.
func (l *linkedList[T]) remove(n *listNode[T]) bool {
	if n == nil || n.owner != l {
		return false
	}
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	n.value = nil
	n.prev, n.next, n.owner = nil, nil, nil
	l.live--
	return true
}

// first returns the head value, or nil when the list is empty.
func (l *linkedList[T]) first() *T {
	if l.head == nil {
		return nil
	}
	return l.head.value
}

// forEach calls fn for every live value in FIFO order. It is the only
// read-only walk, so consumers never see a removed or nil value.
func (l *linkedList[T]) forEach(fn func(*T)) {
	for n := l.head; n != nil; n = n.next {
		fn(n.value)
	}
}

// appendTo appends every live value to dst in FIFO order and returns it.
func (l *linkedList[T]) appendTo(dst []*T) []*T {
	for n := l.head; n != nil; n = n.next {
		dst = append(dst, n.value)
	}
	return dst
}
