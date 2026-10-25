// Package scheduler provides a provider-agnostic, FIFO-ordered request queue
// with per-group concurrency caps and adaptive rate-limit pacing. Requests are
// queued transparently when a provider returns 429/503 and retried when the
// provider signals it is ready (Retry-After / rate-limit-reset headers), so
// clients never see a rate-limit failure. While any request is retrying
// (429, 5xx, or transport), WaitSend lets only that owner send. After it
// succeeds or exhausts, exactly one sibling is the probe: a clean
// first-attempt success restores full concurrency; another retry keeps
// the group at one-at-a-time. Consecutive failed requests (exhausted
// retryable or transport) double a request-level backoff (base_backoff,
// cap max_backoff) so the next probe waits; a success resets it.
package scheduler

import (
	"sync"
	"sync/atomic"
	"time"
)

// Options configures the scheduler.
type Options struct {
	// MaxConcurrent is the default concurrency cap per group when the provider
	// does not advertise one. 0 means unlimited.
	MaxConcurrent int
	// MaxQueueSize is the maximum number of queued requests per group. 0 means
	// unlimited.
	MaxQueueSize int
	// MaxWait is the maximum time a request may wait in the queue before being
	// rejected with an error. 0 means unlimited.
	MaxWait time.Duration
	// BaseBackoff is the initial adaptive delay. It doubles each attempt
	// whether or not the provider sent a retry hint.
	BaseBackoff time.Duration
	// MaxBackoff caps adaptive exponential backoff. It does not clamp a
	// provider-supplied Retry-After / rate-limit-reset duration - a long
	// hint is still waited in full (see MaxRetryHint). A short hint is a
	// floor on the wait, not a replacement for this cap's doubling.
	MaxBackoff time.Duration
	Storm      StormOptions
}

// Scheduler manages ordered queues per group key.
//
// Lock order: s.mu -> g.mu, s.mu -> idleMu, s.mu -> providerIndex.mu,
// s.mu -> pacingLRU.mu, and g.mu -> idleMu. A path holding idleMu,
// providerIndex.mu or pacingLRU.mu never waits for s.mu or g.mu, and
// drainProvider releases providerIndex.mu before touching a group.
type Scheduler struct {
	mu     sync.Mutex // guards opts, groups, groupCap, providerGroups and group retirement
	opts   Options
	groups map[string]*group
	// groupCap bounds len(groups). It is maxSchedulerGroups in production;
	// package tests lower it to exercise eviction without creating 100k
	// groups. Guarded by mu.
	groupCap int
	// providerGroups indexes groups by their attributed provider, so a
	// provider throttle release drains only that provider's groups instead of
	// scanning every group. Guarded by mu. A group is linked exactly once, on
	// its first non-empty attribution, and eviction unlinks it under the same
	// lock and retires that group's counter pointer with the entry.
	providerGroups map[string]*providerIndex
	storms         stormState
	// idleMu guards the idle-group LRU (idleHead/idleTail and each group's
	// idle links). A group with no in-flight slot and no queued waiter sits on
	// this list; the head is the oldest eviction candidate. Transition owners
	// add and remove a group while holding its g.mu, and eviction re-verifies
	// idleness under that same g.mu before removing it.
	idleMu   sync.Mutex
	idleHead *group
	idleTail *group
	// pacing carries the pacing/backoff fields of recently evicted idle
	// groups, bounded by maxSchedulerPacingStates.
	pacing pacingLRU

	// policy is the operator hold set (all / new-unseen / named
	// clients / named providers). Read via atomic pointer so drain can
	// skip held waiters without taking pauseMu.
	policy      atomic.Pointer[policySnap]
	pauseMu     sync.Mutex
	kick        chan struct{} // closed and replaced on every policy edge
	holdCountMu sync.Mutex
	holdCount   map[string]int // waiters currently parked, keyed by hold id

	// gates is the per-provider throttle map (concurrency / requests-per-window /
	// tokens-per-window). Keyed by Record.Provider, NOT provider|key - one
	// client setting a header applies across every key of that provider.
	throttleMu sync.Mutex
	gates      map[string]*providerGate
	// gateSnap is the lock-free read view of gates. gate() must never take
	// throttleMu: admission calls it while holding a group's mutex, and the
	// throttle writers take throttleMu -> gate/group mutexes. Reading through
	// an immutable replacement removes that inversion entirely.
	gateSnap atomic.Pointer[map[string]*providerGate]
}

// New builds a Scheduler. All Options are taken as-is - defaults are owned by
// config.Default(), never re-derived here, so there is exactly one source of
// truth and no conflicting fallback values.
func New(opts Options) *Scheduler {
	s := &Scheduler{opts: opts, groups: make(map[string]*group), groupCap: maxSchedulerGroups, providerGroups: make(map[string]*providerIndex), kick: make(chan struct{}), holdCount: make(map[string]int), gates: make(map[string]*providerGate)}
	s.policy.Store(&policySnap{})
	s.publishGatesLocked()
	s.storms.configure(opts.Storm)
	return s
}

// Options returns the scheduler options.
func (s *Scheduler) Options() Options {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opts
}

// UpdateOptions hot-applies new options on a live scheduler (config reload).
// It updates the shared tunables (queue size/wait, backoff bounds) that
// acquire/drain read from s.opts. It deliberately does NOT touch live groups'
// maxConcurrent: that cap is also set per-request via X-Proxy-Max-Concurrency
// (a client override), and the next Acquire for a group passes the fresh config
// value anyway, so resetting here would only clobber a deliberate override.
func (s *Scheduler) UpdateOptions(opts Options) {
	s.mu.Lock()
	s.opts = opts
	s.storms.configure(opts.Storm)
	s.mu.Unlock()
}

// Stats is a point-in-time snapshot of scheduler occupancy.
type Stats struct {
	Paused   bool `json:"paused"`
	InFlight int  `json:"in_flight"`
	Queued   int  `json:"queued"`
}

// Stats sums in-flight slots and queued waiters across every group.
func (s *Scheduler) Stats() Stats {
	st := Stats{Paused: s.Paused()}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range s.groups {
		g.mu.Lock()
		st.InFlight += g.inFlight
		st.Queued += g.queued
		g.mu.Unlock()
	}
	return st
}

// snapshotGroups copies the current group set under s.mu and releases the
// lock before the caller walks it, so per-group work never nests the
// scheduler lock. Stats deliberately keeps its own nested-lock protocol: it
// sums in-flight and queue counters inside s.mu.
func (s *Scheduler) snapshotGroups() []*group {
	s.mu.Lock()
	groups := make([]*group, 0, len(s.groups))
	for _, g := range s.groups {
		groups = append(groups, g)
	}
	s.mu.Unlock()
	return groups
}

func (s *Scheduler) drainAll() {
	for _, g := range s.snapshotGroups() {
		g.drain()
	}
}

func (s *Scheduler) kickC() <-chan struct{} {
	s.pauseMu.Lock()
	ch := s.kick
	s.pauseMu.Unlock()
	return ch
}

var (
	// ErrQueueFull is returned when the group queue or a pause's MaxQueued
	// cap is exhausted. The proxy maps this to HTTP 429.
	ErrQueueFull = errorString("queue full")
	// ErrGroupLimit is returned when a brand-new group would exceed the
	// process-wide group bound. Existing keys are unaffected.
	ErrGroupLimit = errorString("scheduler group limit reached")
	// ErrOverlap is returned when AddHold / ReplaceHold would
	// park a target already covered by another pause.
	ErrOverlap = errorString("pause overlaps existing hold")
	// ErrHoldNotFound is returned when ReplaceHold targets an unknown id.
	ErrHoldNotFound = errorString("pause hold not found")
	errMaxWait      = errorString("queue wait exceeded")
	// errGroupRetired tells AcquireWith that its looked-up group was evicted
	// as idle before admission; the acquisition retries against the map.
	errGroupRetired = errorString("scheduler group retired")
)

type errorString string

func (e errorString) Error() string { return string(e) }
