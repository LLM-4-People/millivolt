package web

// Exact last-result memoization and one-slot in-flight coalescing for the two
// full-history dashboard scans. A hit avoids the scan, decoding, and percentile
// sorting without retaining history or accepting stale data for an arbitrary
// TTL. Two last-result slots bound memory independently of the number of
// scopes/operators that have visited the page.
//
// A cold caller claims its memo's one flight slot; concurrent callers for the
// SAME key wait for that call instead of repeating the fold. A different key
// never waits and never displaces the tracked call, so memory and waiter state
// stay constant.

import (
	"context"
	"net/url"
	"sort"
	"strconv"
	"sync"
)

type aggregateKey struct {
	params, canon      string
	database           int64
	finalized, pending uint64
}

type aggregateEntry[T any] struct {
	key   aggregateKey
	value T
}

type aggregateMemo[T any] struct {
	mu     sync.Mutex
	entry  *aggregateEntry[T]
	flight *aggregateFlight
}

// aggregateFlight is the one cold computation a memo may share. Same-key
// callers wait on done; a different key never waits behind it.
type aggregateFlight struct {
	key  aggregateKey
	done chan struct{}
}

// aggregateTestHooks are nil in production. They mark the cold-fold start and
// a caller joining an in-flight fold so package tests can sequence leader and
// follower deterministically.
type aggregateTestHooks struct {
	cold func()
	wait func()
}

func (a *AggAPI) testColdHook() {
	if h := a.testHooks; h != nil && h.cold != nil {
		h.cold()
	}
}

func (a *AggAPI) testWaitHook() {
	if h := a.testHooks; h != nil && h.wait != nil {
		h.wait()
	}
}

func (m *aggregateMemo[T]) get(key aggregateKey) *aggregateEntry[T] {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entry != nil && m.entry.key == key {
		return m.entry
	}
	return nil
}

func (m *aggregateMemo[T]) put(key aggregateKey, value T) {
	m.mu.Lock()
	m.entry = &aggregateEntry[T]{key: key, value: value}
	m.mu.Unlock()
}

// begin reports the exact last result, the completion signal of a same-key
// in-flight call, or a new flight the caller must finish. A nil entry, wait
// and flight means another key owns the slot: the caller computes
// independently without waiting or disturbing that call. The flight slot is a
// single bounded field, not a per-key waiter map.
func (m *aggregateMemo[T]) begin(key aggregateKey) (*aggregateEntry[T], <-chan struct{}, *aggregateFlight) {
	if cached := m.get(key); cached != nil {
		return cached, nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Re-check under the lock: a publication between get and this acquisition
	// still wins over a new fold.
	if m.entry != nil && m.entry.key == key {
		return m.entry, nil, nil
	}
	if m.flight != nil {
		if m.flight.key == key {
			return nil, m.flight.done, nil
		}
		return nil, nil, nil
	}
	f := &aggregateFlight{key: key, done: make(chan struct{})}
	m.flight = f
	return nil, nil, f
}

// finish releases the caller's flight and wakes every waiter on every path,
// including errors, cancellation, and panics. Clearing under the memo lock
// before closing guarantees each waiter's re-check observes the final state.
func (m *aggregateMemo[T]) finish(f *aggregateFlight) {
	m.mu.Lock()
	if m.flight == f {
		m.flight = nil
		close(f.done)
	}
	m.mu.Unlock()
}

// Validated scope values form the key, not a raw URL (parameter ordering,
// unknown query fields, and order-independent filters cannot fragment it).
func aggregateParams(view string, fs []scopeFilter, status string) string {
	q := url.Values{"view": {view}, "s": {status}}
	for _, f := range fs {
		q.Add("f", f.dim+":"+f.id)
	}
	sort.Strings(q["f"])
	return q.Encode()
}

func (a *AggAPI) aggregateKey(ctx context.Context, params, canon string, includePending bool) (aggregateKey, bool) {
	if ctx.Err() != nil {
		return aggregateKey{}, false
	}
	finalized, pending := a.buf.Revisions()
	if !includePending {
		pending = 0 // chart sums are finalized-only
	}
	key := aggregateKey{params: params, canon: canon, finalized: finalized, pending: pending}
	if a.store != nil {
		var err error
		key.database, err = a.store.DataVersion(ctx)
		if err != nil {
			return aggregateKey{}, false // never reuse old data after a failed check
		}
	}
	return key, true
}

// A mutation during the scan is ordinary snapshot skew, but its result must
// not become a reusable entry under either the old or the new revision.
func (a *AggAPI) aggregateUnchanged(ctx context.Context, key aggregateKey, includePending bool) bool {
	now, ok := a.aggregateKey(ctx, key.params, modelCanonIdentity(a.modelCanon()), includePending)
	return ok && now == key
}

type chartMemoPayload struct {
	payload chartPayload
	rawFrom int64 // All's scoped minimum, reused only at the same data revision
	empty   bool  // an empty All window starts at the current clock, not the last visit
}

func (a *AggAPI) chart(ctx context.Context, winMin int, fs []scopeFilter, status string, now int64) (chartPayload, error) {
	mcz := a.canonizer() // key, minimum lookup, and fold use ONE rules snapshot
	key, cacheable := a.aggregateKey(ctx, aggregateParams(strconv.Itoa(winMin), fs, status), mcz.identity, false)
	if !cacheable {
		return a.chartCold(ctx, mcz, key, false, nil, winMin, fs, status, now)
	}
	for {
		cached, wait, flight := a.chartMemo.begin(key)
		if cached != nil {
			if p, ok := chartHit(cached, winMin, now); ok {
				return p, nil
			}
			// A result published for another clock's bucket geometry cannot
			// answer this caller; fold its own payload as an uncached call.
			return a.chartCold(ctx, mcz, key, true, cached, winMin, fs, status, now)
		}
		if flight != nil {
			defer a.chartMemo.finish(flight)
			return a.chartCold(ctx, mcz, key, true, nil, winMin, fs, status, now)
		}
		if wait == nil {
			// Another key owns the one flight slot: never wait behind it.
			return a.chartCold(ctx, mcz, key, true, nil, winMin, fs, status, now)
		}
		a.testWaitHook()
		select {
		case <-wait:
			// Re-check the memo: the leader may have published, failed, or
			// been canceled. Its error never becomes ours.
			continue
		case <-ctx.Done():
			return chartPayload{}, ctx.Err()
		}
	}
}

// chartHit reuses a memoized chart only when the caller's clock produces the
// exact bucket geometry. NowMs is request metadata within identical geometry;
// cached slices remain immutable.
func chartHit(cached *aggregateEntry[chartMemoPayload], winMin int, now int64) (chartPayload, bool) {
	rawFrom := now - int64(winMin)*60000
	if winMin == 0 {
		if cached.value.empty {
			rawFrom = now
		} else {
			rawFrom = cached.value.rawFrom
		}
	}
	from, step, count := chartWindowEdges(now, rawFrom)
	p := cached.value.payload
	if p.FromMs != from || p.BucketMs != step || len(p.Buckets) != count {
		return chartPayload{}, false
	}
	p.NowMs = now
	return p, true
}

// chartCold runs one uncached chart computation and publishes its result only
// under the unchanged-revision discipline. cached is the exact-key entry that
// failed this caller's geometry check, or nil for a cold All-window fold.
func (a *AggAPI) chartCold(ctx context.Context, mcz *modelCanonizer, key aggregateKey, cacheable bool, cached *aggregateEntry[chartMemoPayload], winMin int, fs []scopeFilter, status string, now int64) (chartPayload, error) {
	a.testColdHook()
	// An uncached All window owns its minimum and fold under one lease. A
	// cached entry already pinned the minimum at the same data revision, so
	// the normal single-lease fold below is enough.
	if winMin == 0 && cached == nil {
		var rawFrom int64
		var fold *chartFold
		if err := a.streamAllChart(ctx, mcz, status, fs, now, func(minStart, _, _ int64, _ int, f *chartFold) {
			rawFrom, fold = minStart, f
		}); err != nil {
			return chartPayload{}, err
		}
		return a.storeChartResult(ctx, key, cacheable, winMin, rawFrom, now, fold.payload(now))
	}
	rawFrom := now - int64(winMin)*60000
	if winMin == 0 {
		if cached.value.empty {
			rawFrom = now
		} else {
			rawFrom = cached.value.rawFrom
		}
	}
	from, step, count := chartWindowEdges(now, rawFrom)
	fold := newChartFold(from, step, count, status, fs)
	freeze := func() error { fold.freezeOrder(); return nil }
	if err := a.streamWindow(ctx, from, mcz, func(p *projectionData) { fold.prepare(p, mcz) }, fold.fold, freeze); err != nil {
		return chartPayload{}, err
	}
	return a.storeChartResult(ctx, key, cacheable, winMin, rawFrom, now, fold.payload(now))
}

// storeChartResult publishes a completed chart only when the data revision is
// unchanged, then returns the payload. The caller computes the payload outside
// the projection lease.
func (a *AggAPI) storeChartResult(ctx context.Context, key aggregateKey, cacheable bool, winMin int, rawFrom, now int64, p chartPayload) (chartPayload, error) {
	empty := true
	for _, b := range p.Buckets {
		if b.Req > 0 {
			empty = false
			break
		}
	}
	// An All minimum clamped to now despite matching future-dated rows may
	// change as the clock catches them. Such unusual windows are not memoized.
	if cacheable && (winMin != 0 || rawFrom < now || empty) && a.aggregateUnchanged(ctx, key, false) {
		a.chartMemo.put(key, chartMemoPayload{payload: p, rawFrom: rawFrom, empty: empty})
	}
	if err := ctx.Err(); err != nil {
		return chartPayload{}, err
	}
	return p, nil
}

func (a *AggAPI) explorer(ctx context.Context, dim string, fs []scopeFilter, status string) (explorerPayload, error) {
	mcz := a.canonizer()
	key, cacheable := a.aggregateKey(ctx, aggregateParams(dim, fs, status), mcz.identity, true)
	if !cacheable {
		return a.explorerCold(ctx, mcz, key, false, dim, fs, status)
	}
	for {
		cached, wait, flight := a.explorerMemo.begin(key)
		if cached != nil {
			return cached.value, nil
		}
		if flight != nil {
			defer a.explorerMemo.finish(flight)
			return a.explorerCold(ctx, mcz, key, true, dim, fs, status)
		}
		if wait == nil {
			// Another key owns the one flight slot: never wait behind it.
			return a.explorerCold(ctx, mcz, key, true, dim, fs, status)
		}
		a.testWaitHook()
		select {
		case <-wait:
			continue
		case <-ctx.Done():
			return explorerPayload{}, ctx.Err()
		}
	}
}

// explorerCold runs one uncached explorer fold. A caller that loses the flight
// race, or whose leader published nothing, recomputes here instead of
// inheriting another request's error.
func (a *AggAPI) explorerCold(ctx context.Context, mcz *modelCanonizer, key aggregateKey, cacheable bool, dim string, fs []scopeFilter, status string) (explorerPayload, error) {
	a.testColdHook()
	e := newExplorerFold(dim, status, fs)
	pending := a.buf.PendingRecords()
	e.pendingIDs = make(map[string]struct{}, len(pending))
	for _, r := range pending {
		if r != nil {
			e.pendingIDs[r.ID] = struct{}{}
		}
	}
	var p explorerPayload
	if err := a.streamWindow(ctx, 0, mcz, func(p *projectionData) { e.prepare(p, mcz) }, e.fold, func() error {
		if err := eachPending(pending, e.seen, mcz, e.fold); err != nil {
			return err
		}
		// The payload reads the borrowed conversation topology. Build it while
		// streamWindow still holds the projection read lease; it must not
		// escape the callback and read a concurrent append.
		p = e.payload()
		return nil
	}); err != nil {
		return explorerPayload{}, err
	}
	if cacheable && a.aggregateUnchanged(ctx, key, true) {
		a.explorerMemo.put(key, p)
	}
	if err := ctx.Err(); err != nil {
		return explorerPayload{}, err
	}
	return p, nil
}
