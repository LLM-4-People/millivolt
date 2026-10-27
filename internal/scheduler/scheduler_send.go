package scheduler

import (
	"context"
	"time"
)

// Trip records a retryable failure (429, 5xx, or transport): extends the
// pacing window and claims the send token if nobody else has it. The
// caller who gets true owns every subsequent send until EndSend - siblings
// WaitSend until that request succeeds or exhausts.
func (s *Scheduler) Trip(key string, d time.Duration) bool {
	return s.extendWindow(key, d, true)
}

func (s *Scheduler) extendWindow(key string, d time.Duration, claim bool) bool {
	s.mu.Lock()
	g := s.groupForLocked(key, 0, false)
	g.mu.Lock()
	owned := false
	// A non-positive duration still trips below so siblings queue even when
	// the owner can retry immediately; the pacing window itself is guarded
	// by armPacingLaterLocked (fail closed).
	g.armPacingLaterLocked(d)
	if d > 0 || claim {
		g.tripped = true
		g.kickSendLocked()
	}
	if claim && !g.probing {
		g.probing = true
		owned = true
	}
	s.mu.Unlock()
	g.mu.Unlock()
	return owned
}

func (g *group) sendKickC() <-chan struct{} {
	g.mu.Lock()
	ch := g.sendKick
	g.mu.Unlock()
	return ch
}

func (g *group) kickSend() {
	g.mu.Lock()
	g.kickSendLocked()
	g.mu.Unlock()
}

func (g *group) kickSendLocked() {
	if g.sendKick == nil {
		g.sendKick = make(chan struct{})
		return
	}
	close(g.sendKick)
	g.sendKick = make(chan struct{})
}

// armPacingLaterLocked pushes the group's pacing window out by d when that
// lands after the current allowance, arming the pacer; an earlier existing
// window is never shrunk. A non-positive d is a no-op: it must never erase an
// existing pacing window (fail closed). Callers hold g.mu.
func (g *group) armPacingLaterLocked(d time.Duration) {
	if d > 0 {
		until := time.Now().Add(d)
		if until.After(g.nextAllowedAt) {
			g.nextAllowedAt = until
			g.pacer.arm(until, false)
		}
	}
}

// waitKickOrDone parks a WaitSend caller on its freshly snapshotted wake
// channels until a group kick, a send kick, or context cancellation; the
// cancellation is returned so the caller can return its retry token.
func waitKickOrDone(ctx context.Context, kick, sendKick <-chan struct{}) error {
	select {
	case <-kick:
	case <-sendKick:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// parkSendLocked counts this WaitSend call as one live send-gate waiter. It
// reports the call's first park - the only transition that publishes the
// queued edge - so a loop that re-parks never double counts or re-fires.
// Callers hold g.mu.
func (g *group) parkSendLocked(parked bool) bool {
	if parked {
		return false
	}
	g.sendWaiters++
	return true
}

// announceSendWait publishes the queued edge for a caller parked in the send
// gate: the live row reads as queued until the gate opens. It deliberately
// does not start the SSE pacer - a send-gate wait must not write a keepalive
// frame ahead of the upstream body, which would change the bytes a client
// receives on a retried stream. Called outside every scheduler lock.
func announceSendWait(hooks WaiterHooks) {
	if hooks.OnThrottle != nil {
		hooks.OnThrottle()
	}
}

// WaitSend blocks until this caller may hit upstream. honorHold parks on an
// operator pause (retries - a new send). First-attempt callers pass false so
// an already-acquired in-flight request still finishes. own is the token
// from Trip: the retrying request keeps sending; everyone else waits until
// EndSend. The bool is the token to pass back in (true if this caller now
// owns the retry). nextAllowedAt is absolute wall clock - pause does not
// reset it; after unpause the remaining time is waited.
//
// Every parked caller is counted in the group's sendWaiters and publishes one
// queued edge (OnThrottle) until it owns the gate or gives up, so a 429 pacing
// window is visible as queued work instead of an idle "streaming" row. The
// token owner parks the same way while it waits out its own Retry-After
// window; it is not sending during that time either.
func (s *Scheduler) WaitSend(ctx context.Context, key string, hooks WaiterHooks, honorHold, own bool) (bool, error) {
	g := s.groupFor(key, 0)
	holding := false
	parked := false
	defer func() {
		if parked {
			g.mu.Lock()
			if g.sendWaiters > 0 {
				g.sendWaiters--
			}
			g.mu.Unlock()
			if hooks.OnUnthrottle != nil {
				hooks.OnUnthrottle()
			}
		}
		if holding && hooks.OnUnhold != nil {
			hooks.OnUnhold()
		}
	}()
	for {
		// Snapshot both kick channels before the predicates (lost-wakeup).
		kick := s.kickC()
		sendKick := g.sendKickC()

		if honorHold {
			if h := s.matchingHold(hooks.Client, hooks.Provider); h != nil {
				if !holding {
					if hooks.OnWait != nil {
						hooks.OnWait()
					}
					if err := ctx.Err(); err != nil {
						return own, err
					}
					// A slow client write may outlive the hold. Do not publish
					// a stale paused edge after the operator resumed it.
					if s.matchingHold(hooks.Client, hooks.Provider) == nil {
						continue
					}
					holding = true
					if hooks.OnHold != nil {
						hooks.OnHold()
					}
				}
				var untilTimer *time.Timer
				var untilC <-chan time.Time
				if !h.until.IsZero() {
					d := time.Until(h.until)
					if d <= 0 {
						continue
					}
					untilTimer = time.NewTimer(d)
					untilC = untilTimer.C
				}
				select {
				case <-untilC:
				case <-kick:
				case <-sendKick:
				case <-ctx.Done():
					if untilTimer != nil {
						untilTimer.Stop()
					}
					return own, ctx.Err()
				}
				if untilTimer != nil {
					untilTimer.Stop()
				}
				continue
			}
		}
		if holding {
			holding = false
			if hooks.OnUnhold != nil {
				hooks.OnUnhold()
			}
		}

		g.mu.Lock()
		now := time.Now()
		// One park site for both send-gate waits: the pacing window and the
		// sibling waiting for the owner's probe token. An owner ignores
		// probing and sends as soon as its own window opens.
		if now.Before(g.nextAllowedAt) || (!own && g.tripped && g.probing) {
			first := g.parkSendLocked(parked)
			parked = true
			g.mu.Unlock()
			if first {
				announceSendWait(hooks)
			}
			if err := waitKickOrDone(ctx, kick, sendKick); err != nil {
				return own, err
			}
			continue
		}
		if own || !g.tripped {
			g.mu.Unlock()
			return own, nil
		}
		g.probing = true
		g.mu.Unlock()
		return true, nil
	}
}

// EndSend releases the retry token after a non-failure outcome (success
// or client abort). keepSerial (the request retried) leaves the group
// half-open: exactly one waiter may send next. A clean first-attempt
// success passes keepSerial=false and restores full concurrency.
// Consecutive-request backoff is reset so the next probe is immediate.
func (s *Scheduler) EndSend(key string, keepSerial bool) {
	s.mu.Lock()
	g, ok := s.groups[key]
	s.mu.Unlock()
	if !ok {
		return
	}
	g.mu.Lock()
	g.probing = false
	g.backoff = 0
	g.requestBackoff = 0
	g.nextAllowedAt = time.Time{}
	g.pacer.stop()
	if keepSerial {
		g.tripped = true
	} else {
		g.tripped = false
	}
	g.kickSendLocked()
	g.mu.Unlock()
	g.drainAdmission()
}

// FailSend records that this request's final outcome was a retryable
// failure (exhausted 429/5xx or retryable transport). It doubles the
// consecutive-request backoff - start at base_backoff, double each
// failed request, cap at max_backoff (same config pair as attempt
// backoff) - and paces the next probe. Retry-After already on the
// group is not shrunk. own releases the send token; concurrency
// stays at one.
func (s *Scheduler) FailSend(key string, own bool) {
	s.mu.Lock()
	g := s.groupForLocked(key, 0, false)
	opts := s.opts
	s.mu.Unlock()
	g.mu.Lock()
	if own {
		g.probing = false
		// Only the owner is done. A sibling first-attempt exhaust must not
		// wipe an in-flight retry's attempt streak (g.backoff is shared).
		g.backoff = 0
	}
	g.requestBackoff = growBackoff(g.requestBackoff, opts.BaseBackoff, opts.MaxBackoff)
	d := jitterBackoff(g.requestBackoff)
	g.armPacingLaterLocked(d)
	g.tripped = true
	g.kickSendLocked()
	g.mu.Unlock()
	g.drainAdmission()
}

func (g *group) kickC() <-chan struct{} {
	if g.sched == nil {
		return nil
	}
	return g.sched.kickC()
}
