package proxy

import "sync/atomic"

// inflightLimiter is the process-wide admission bound. It is lock-free: a
// request increments, checks the current limit, and backs out when over, so a
// reload can change the bound without rebuilding a channel or stopping
// traffic. The counter is always decremented on the same request's exit.
type inflightLimiter struct {
	limit     atomic.Int64
	cur       atomic.Int64
	byteLimit atomic.Int64
	bytes     atomic.Int64
}

func (l *inflightLimiter) set(limit int, bodyBytes int64) {
	if limit < 0 {
		limit = 0
	}
	if bodyBytes < 0 {
		bodyBytes = 0
	}
	l.limit.Store(int64(limit))
	l.byteLimit.Store(bodyBytes)
}

// acquire reserves one request slot and its expected body bytes. A refusal
// backs both counters out, so a rejected request never leaks capacity.
func (l *inflightLimiter) acquire(bodyBytes int64) bool {
	if bodyBytes < 0 {
		bodyBytes = 0
	}
	n := l.cur.Add(1)
	if limit := l.limit.Load(); limit > 0 && n > limit {
		l.cur.Add(-1)
		return false
	}
	if bodyBytes > 0 {
		b := l.bytes.Add(bodyBytes)
		if limit := l.byteLimit.Load(); limit > 0 && b > limit {
			l.bytes.Add(-bodyBytes)
			l.cur.Add(-1)
			return false
		}
	}
	return true
}

func (l *inflightLimiter) release(bodyBytes int64) {
	if bodyBytes > 0 {
		l.bytes.Add(-bodyBytes)
	}
	l.cur.Add(-1)
}
