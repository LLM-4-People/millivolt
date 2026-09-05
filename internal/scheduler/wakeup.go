package scheduler

import (
	"sync"
	"time"
)

// wakeTimer owns one outstanding wakeup for a shared gate. Callers still
// re-check their predicates: Stop cannot wait for an AfterFunc callback that
// has already started. The generation check prevents such a callback from
// clearing or firing a replacement timer. fn must be installed before use.
type wakeTimer struct {
	mu         sync.Mutex
	timer      *time.Timer
	at         time.Time
	generation uint64
	fn         func()
}

// arm replaces the deadline, or keeps an earlier wakeup when earliest is true
// (several provider queues may need different token amounts). Equal deadlines
// are always a no-op. The group pacer passes false because its window extends.
func (w *wakeTimer) arm(at time.Time, earliest bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil && (w.at.Equal(at) || earliest && !at.Before(w.at)) {
		return
	}
	w.stopLocked()
	w.at = at
	gen := w.generation
	w.timer = time.AfterFunc(time.Until(at), func() {
		w.mu.Lock()
		if gen != w.generation {
			w.mu.Unlock()
			return
		}
		w.timer = nil
		w.at = time.Time{}
		w.mu.Unlock()
		w.fn()
	})
}

func (w *wakeTimer) stop() {
	w.mu.Lock()
	w.stopLocked()
	w.mu.Unlock()
}

func (w *wakeTimer) stopLocked() {
	w.generation++
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.at = time.Time{}
}
