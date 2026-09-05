package web

// Exact last-result memoization for the two full-history dashboard scans.
// A hit avoids the scan, decoding, and percentile sorting without retaining
// history or accepting stale data for an arbitrary TTL. Two slots bound memory
// independently of the number of scopes/operators that have visited the page.

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
	mu    sync.Mutex
	entry *aggregateEntry[T]
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
	var cached *aggregateEntry[chartMemoPayload]
	if cacheable {
		cached = a.chartMemo.get(key)
	}
	rawFrom := now - int64(winMin)*60000
	if winMin == 0 {
		if cached != nil {
			if cached.value.empty {
				rawFrom = now
			} else {
				rawFrom = cached.value.rawFrom
			}
		} else {
			var err error
			rawFrom, err = minStartScoped(ctx, a, mcz, fs, status, now)
			if err != nil {
				return chartPayload{}, err
			}
		}
	}
	from, step, count := chartWindowEdges(now, rawFrom)
	if cached != nil {
		p := cached.value.payload
		if p.FromMs == from && p.BucketMs == step && len(p.Buckets) == count {
			// Current time is metadata, not an aggregation input within the
			// same geometry. Copy it; cached slices remain immutable.
			p.NowMs = now
			return p, nil
		}
	}
	fold := newChartFold(from, step, count, status, fs)
	if err := a.streamWindow(ctx, from, mcz, func(p *projectionData) { fold.prepare(p, mcz) }, fold.fold, nil); err != nil {
		return chartPayload{}, err
	}
	p := fold.payload(now)
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
	if cacheable {
		if cached := a.explorerMemo.get(key); cached != nil {
			return cached.value, nil
		}
	}
	e := newExplorerFold(dim, status, fs)
	pending := a.buf.PendingRecords()
	e.pendingIDs = make(map[string]struct{}, len(pending))
	for _, r := range pending {
		if r != nil {
			e.pendingIDs[r.ID] = struct{}{}
		}
	}
	if err := a.streamWindow(ctx, 0, mcz, func(p *projectionData) { e.prepare(p, mcz) }, e.fold, func() error {
		return eachPending(pending, e.seen, mcz, e.fold)
	}); err != nil {
		return explorerPayload{}, err
	}
	p := e.payload()
	if cacheable && a.aggregateUnchanged(ctx, key, true) {
		a.explorerMemo.put(key, p)
	}
	if err := ctx.Err(); err != nil {
		return explorerPayload{}, err
	}
	return p, nil
}
