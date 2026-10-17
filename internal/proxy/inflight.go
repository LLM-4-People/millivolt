package proxy

import "sync/atomic"

// inflightLimiter is the process-wide admission bound. It is lock-free: a
// request increments, checks the current limit, and backs out when over, so a
// reload can change the bound without rebuilding a channel or stopping
// traffic. The counter is always decremented on the same request's exit.
type inflightLimiter struct {
	limit atomic.Int64
	cur   atomic.Int64
}

func (l *inflightLimiter) set(limit int) {
	if limit < 0 {
		limit = 0
	}
	l.limit.Store(int64(limit))
}

func (l *inflightLimiter) acquire() bool {
	n := l.cur.Add(1)
	if limit := l.limit.Load(); limit > 0 && n > limit {
		l.cur.Add(-1)
		return false
	}
	return true
}

func (l *inflightLimiter) release() { l.cur.Add(-1) }
