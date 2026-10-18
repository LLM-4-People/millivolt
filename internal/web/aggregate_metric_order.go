package web

import (
	"cmp"
	"math"
	"slices"
	"sync"
)

// metricSample pins a positive finite metric value to its immutable projection
// row. The sorted order belongs to projection catch-up, never a proxy request.
type metricSample[T ~int64 | ~float64] struct {
	row   uint32
	value T
}

// recentMetricRows bounds the duplicate tail order used by bounded chart
// windows. A window whose earliest durable row is inside this tail reads the
// tail order instead of walking all history; 65536 rows covers a 15-minute
// window at a high sustained rate while adding only a few MiB per process.
// Internal implementation constants, not tunables.
const (
	recentMetricRows        = 65536
	recentMetricRebuildRows = 16384
	// metricOrderPendingSamples bounds the uncompacted all-history and
	// recent-tail deltas. A sub-threshold append copies at most this many
	// samples however long the retained history is; reaching the bound folds
	// base+pending into one new immutable base. 16384 samples cap the extra
	// pending memory at roughly 256 KiB, while making the O(history) fold rare
	// instead of per append.
	metricOrderPendingSamples = 16384
)

// metricOrders is one immutable set of value-sorted samples. Rows are
// projection row indices (0-based); every slice is sorted ascending by value.
type metricOrders struct {
	ttft []metricSample[int64]
	tps  []metricSample[float64]
	dec  []metricSample[int64]
}

func (o metricOrders) sampleCount() int {
	return len(o.ttft) + len(o.tps) + len(o.dec)
}

// mergeSorted folds already sorted added samples into fresh immutable slices
// and never mutates its inputs. Ties keep the old samples first, exactly like
// the batch-by-batch merge it replaces.
func (o *metricOrders) mergeSorted(added metricOrders) {
	o.ttft = mergeSortedMetricOrder(o.ttft, added.ttft)
	o.tps = mergeSortedMetricOrder(o.tps, added.tps)
	o.dec = mergeSortedMetricOrder(o.dec, added.dec)
}

func (o *metricOrders) sort() {
	slices.SortFunc(o.ttft, func(a, b metricSample[int64]) int { return cmp.Compare(a.value, b.value) })
	slices.SortFunc(o.tps, func(a, b metricSample[float64]) int { return cmp.Compare(a.value, b.value) })
	slices.SortFunc(o.dec, func(a, b metricSample[int64]) int { return cmp.Compare(a.value, b.value) })
}

// metricOrderView memoizes the exact base+pending merge for one published
// projection snapshot. The holder is shared by snapshot copies; a new append
// installs a fresh holder, so no reader can observe later mutation.
type metricOrderView struct {
	once  sync.Once
	order metricOrders
}

type projectionMetrics struct {
	// base is the immutable compacted all-history order. pending is a value
	// sorted delta since base; it is copy-on-write, so published slices never
	// change. Readers see both as one order through allOrder.
	base    metricOrders
	pending metricOrders
	view    *metricOrderView
	// recentBase is the immutable compacted order over the last
	// recentMetricRows rows at the last rebuild. recentPending is the value
	// sorted delta since then; recentOrder merges both on demand. The base may
	// retain already-out-of-tail samples between rebuilds; consumers only use
	// the order when their earliest required row is >= recentFrom.
	recentBase    metricOrders
	recentPending metricOrders
	recentView    *metricOrderView
	recentFrom    int
	recentBuiltAt int
}

// allOrder returns the exact ascending all-history order of base+pending.
// Chart and explorer share this one reader path. The merge runs once per
// published snapshot and is cached; it never mutates base or pending.
func (m *projectionMetrics) allOrder() metricOrders {
	return mergeMetricOrderView(m.base, m.pending, m.view)
}

// recentOrder returns the exact ascending tail order of recentBase+pending.
// Bounded chart windows whose earliest required row is inside the tail share
// this one reader path. The merge runs once per published snapshot and is
// cached; it never mutates either input.
func (m *projectionMetrics) recentOrder() metricOrders {
	return mergeMetricOrderView(m.recentBase, m.recentPending, m.recentView)
}

// mergeMetricOrderView merges an immutable base with its value-sorted pending
// delta through one view holder. The holder is shared by snapshot copies; a
// new append installs a fresh holder, so no reader can observe later mutation.
func mergeMetricOrderView(base, pending metricOrders, view *metricOrderView) metricOrders {
	if pending.sampleCount() == 0 {
		return base
	}
	if view == nil {
		// A zero projectionMetrics has no published snapshot to cache on.
		return mergeMetricOrderSets(base, pending)
	}
	view.once.Do(func() {
		view.order = mergeMetricOrderSets(base, pending)
	})
	return view.order
}

func mergeMetricOrderSets(base, pending metricOrders) metricOrders {
	return metricOrders{
		ttft: mergeSortedMetricOrder(base.ttft, pending.ttft),
		tps:  mergeSortedMetricOrder(base.tps, pending.tps),
		dec:  mergeSortedMetricOrder(base.dec, pending.dec),
	}
}

func validMetricSample[T ~int64 | ~float64](value T) bool {
	return value > 0 && !math.IsInf(float64(value), 0)
}

// collectMetricOrders selects valid samples in rows[from:].
func collectMetricOrders(rows []contrib, from int) metricOrders {
	if from < 0 {
		from = 0
	}
	if from >= len(rows) {
		return metricOrders{}
	}
	n := len(rows) - from
	out := metricOrders{
		ttft: make([]metricSample[int64], 0, n),
		tps:  make([]metricSample[float64], 0, n),
		dec:  make([]metricSample[int64], 0, n),
	}
	for i := from; i < len(rows); i++ {
		c := &rows[i]
		if validMetricSample(c.ttft) {
			out.ttft = append(out.ttft, metricSample[int64]{row: uint32(i), value: c.ttft})
		}
		if validMetricSample(c.tps) {
			out.tps = append(out.tps, metricSample[float64]{row: uint32(i), value: c.tps})
		}
		if validMetricSample(c.dec) {
			out.dec = append(out.dec, metricSample[int64]{row: uint32(i), value: c.dec})
		}
	}
	return out
}

// append sorts only new rows and folds them into the bounded pending delta.
// Previously published slices are never changed, including their spare
// capacity. from == 0 is a full rebuild: it replaces the base with every
// sample and clears pending.
func (m *projectionMetrics) append(rows []contrib, from int) {
	if from >= len(rows) {
		return
	}
	added := collectMetricOrders(rows, from)
	added.sort()
	if from == 0 {
		m.base = added
		m.pending = metricOrders{}
		m.recentBase = metricOrders{}
		m.recentPending = metricOrders{}
		m.recentView = nil
		m.recentFrom = 0
		m.recentBuiltAt = 0
	} else {
		m.pending.mergeSorted(added)
		if m.pending.sampleCount() >= metricOrderPendingSamples {
			m.base.mergeSorted(m.pending)
			m.pending = metricOrders{}
		}
	}
	// Any base or pending change retires the cached merge. Snapshot copies
	// keep the old holder and its immutable slices.
	m.view = &metricOrderView{}
	if len(rows) <= recentMetricRows {
		// No tail is read until the history exceeds recentMetricRows; leave
		// the recent order empty instead of aliasing the all-history slices.
		m.recentBase = metricOrders{}
		m.recentPending = metricOrders{}
		m.recentView = nil
		m.recentFrom = 0
		m.recentBuiltAt = 0
		return
	}
	// Rebuild before merging a large catch-up: replaying a batch that is
	// about to be discarded only doubles the transient copy.
	if m.recentFrom == 0 || len(rows)-m.recentBuiltAt >= recentMetricRebuildRows {
		m.rebuildRecent(rows)
		return
	}
	// The recent order already covers every row >= recentFrom. A caller that
	// passes an earlier from (not the incremental append contract) must not
	// merge those rows again or its samples would be double-counted.
	if from >= m.recentFrom {
		m.recentPending.mergeSorted(added)
		if m.recentPending.sampleCount() >= metricOrderPendingSamples {
			m.recentBase.mergeSorted(m.recentPending)
			m.recentPending = metricOrders{}
		}
		// The tail order changed; retire its cached merge while snapshot
		// copies keep the old holder and its immutable slices.
		m.recentView = &metricOrderView{}
	}
	if next := len(rows) - recentMetricRows; next > m.recentFrom {
		m.recentFrom = next
	}
	// Advance the tail lazily: out-of-tail samples retained between rebuilds
	// are harmless (their membership byte is zero) and bounded by the rebuild
	// cadence.
}

// rebuildRecent recreates the tail base from the newest recentMetricRows rows,
// clears the pending delta and its view, and releases samples that have fallen
// out of the tail.
func (m *projectionMetrics) rebuildRecent(rows []contrib) {
	from := max(0, len(rows)-recentMetricRows)
	orders := collectMetricOrders(rows, from)
	orders.sort()
	m.recentBase = orders
	m.recentPending = metricOrders{}
	m.recentView = nil
	m.recentFrom = from
	m.recentBuiltAt = len(rows)
}

// mergeSortedMetricOrder merges two ascending orders without touching either
// input. Ties keep old samples first, preserving the incremental batch order.
func mergeSortedMetricOrder[T ~int64 | ~float64](old, added []metricSample[T]) []metricSample[T] {
	if len(added) == 0 {
		return old
	}
	if len(old) == 0 {
		return added
	}
	out := make([]metricSample[T], len(old)+len(added))
	i, j, n := 0, 0, 0
	for i < len(old) && j < len(added) {
		if old[i].value <= added[j].value {
			out[n] = old[i]
			i++
		} else {
			out[n] = added[j]
			j++
		}
		n++
	}
	n += copy(out[n:], old[i:])
	copy(out[n:], added[j:])
	return out
}

// Each bucket and the viewed period need only six sorted ranks. Positions
// can repeat for sparse (but nonsuppressed) samples, so observe consumes every
// matching position rather than discarding a repeated floor/ceiling rank.
type metricRankSet struct {
	positions [6]int
	values    [6]float64
	fractions [3]float64
	outputs   [6]uint8
	count     int
	seen      int
	next      int
}

func (s *metricRankSet) init(count int) {
	s.count = count
	if count < pctMinSamples {
		s.next = len(s.positions)
		return
	}
	for i, p := range [...]float64{50, 95, 99} {
		lo, hi, fraction, _ := percentilePosition(count, p)
		s.positions[2*i], s.positions[2*i+1] = lo, hi
		s.fractions[i] = fraction
	}
	// A later percentile's floor can precede an earlier one's ceiling
	// (p95 and p99 on four samples). Sort rank/destination pairs once.
	for i := range s.outputs {
		s.outputs[i] = uint8(i)
	}
	for i := 1; i < len(s.positions); i++ {
		for j := i; j > 0 && s.positions[j] < s.positions[j-1]; j-- {
			s.positions[j], s.positions[j-1] = s.positions[j-1], s.positions[j]
			s.outputs[j], s.outputs[j-1] = s.outputs[j-1], s.outputs[j]
		}
	}
}

func (s *metricRankSet) observe(value float64) {
	for s.next < len(s.positions) && s.positions[s.next] == s.seen {
		s.values[s.outputs[s.next]] = value
		s.next++
	}
	s.seen++
}

func (s *metricRankSet) result(out []*float64, values []float64) {
	// Counts come from the same filtered fold. Refuse inconsistent internal
	// inputs instead of interpolating uncaptured/default zero ranks.
	if s.count < pctMinSamples || s.seen != s.count || s.next != len(s.positions) {
		return
	}
	for i := range out {
		values[i] = percentileInterpolate(s.values[2*i], s.values[2*i+1], s.fractions[i])
		out[i] = &values[i]
	}
}

func observeMetricRank(sets []metricRankSet, bucket uint8, value float64) {
	if bucket == 0 || int(bucket) >= len(sets) {
		return
	}
	sets[bucket].observe(value)
	sets[0].observe(value) // one exact period-wide order, not bucket averages
}

// rankPercentiles adapts a chart's one-byte row membership to the shared
// multi-membership reader. Zero excludes a row; other values are bucket+1.
func rankPercentiles[T ~int64 | ~float64](order []metricSample[T], membership []uint8, counts []int, extraValues []T, extraBuckets []uint8) ([][]*float64, []*float64) {
	return rankPercentilesVisit(order, counts, extraValues, extraBuckets, func(row uint32, emit func(uint8)) {
		if uint64(row) < uint64(len(membership)) {
			emit(membership[row])
		}
	})
}

// rankPercentilesVisit reads preordered durable samples once, selecting exact
// p50/p95/p99 for each group and the whole occurrence-weighted period without
// history copies or query-time sorting. visit synchronously emits zero or more
// group+1 IDs for a row (0 excludes); repeated IDs preserve tool/error occurrence
// multiplicity. The callback must not retain emit. counts includes positive
// finite durable AND extra samples, including repeated memberships.
// Unsorted ring extras use the same group encoding and are copied locally;
// all inputs remain immutable. Work is O(history + memberships + extras log extras).
func rankPercentilesVisit[T ~int64 | ~float64](order []metricSample[T], counts []int, extraValues []T, extraBuckets []uint8, visit func(row uint32, emit func(bucket uint8))) ([][]*float64, []*float64) {
	buckets, period, _ := rankPercentilesCore(order, counts, extraValues, extraBuckets, visit, false)
	return buckets, period
}

// rankPercentilesStats is the chart's combined owner: one traversal yields the
// exact bucket/period ranks AND the period-wide occurrence-weighted
// avg/min/max. The separate metricStats pass walked the same immutable order a
// second time per metric for no new information.
func rankPercentilesStats[T ~int64 | ~float64](order []metricSample[T], membership []uint8, counts []int, extraValues []T, extraBuckets []uint8) ([][]*float64, []*float64, []*float64) {
	return rankPercentilesCore(order, counts, extraValues, extraBuckets, func(row uint32, emit func(uint8)) {
		if uint64(row) < uint64(len(membership)) {
			emit(membership[row])
		}
	}, true)
}

type metricAccumulator struct {
	sum float64
	n   int
	lo  float64
	hi  float64
}

func (a *metricAccumulator) add(v float64) {
	if a.n == 0 {
		a.lo, a.hi = v, v
	} else {
		if v < a.lo {
			a.lo = v
		}
		if v > a.hi {
			a.hi = v
		}
	}
	a.sum += v
	a.n++
}

func (a *metricAccumulator) result() []*float64 {
	if a.n == 0 {
		return nil
	}
	avg := a.sum / float64(a.n)
	return []*float64{&avg, &a.lo, &a.hi}
}

func rankPercentilesCore[T ~int64 | ~float64](order []metricSample[T], counts []int, extraValues []T, extraBuckets []uint8, visit func(row uint32, emit func(bucket uint8)), wantStats bool) ([][]*float64, []*float64, []*float64) {
	sets := make([]metricRankSet, len(counts)+1)
	buckets := make([][]*float64, len(counts))
	out := make([]*float64, 3*len(sets))
	values := make([]float64, len(out))
	total := 0
	for i, count := range counts {
		sets[i+1].init(count)
		total += count
		buckets[i] = out[3*(i+1) : 3*(i+2)]
	}
	sets[0].init(total)
	period := out[:3]
	if len(extraValues) != len(extraBuckets) {
		return buckets, period, nil
	}
	if total < pctMinSamples {
		// Ranks need the minimum sample count, but the avg/min/max tile is
		// valid for any positive count. Traverse for stats only when asked;
		// the rank sets are already disabled by init.
		if !wantStats || total == 0 {
			return buckets, period, nil
		}
	}
	extras := make([]metricSample[T], 0, len(extraValues))
	for i, value := range extraValues {
		bucket := extraBuckets[i]
		if bucket > 0 && int(bucket) < len(sets) && validMetricSample(value) {
			extras = append(extras, metricSample[T]{row: uint32(bucket), value: value})
		}
	}
	slices.SortFunc(extras, func(a, b metricSample[T]) int { return cmp.Compare(a.value, b.value) })
	// One callback for this traversal, never a closure allocation per row.
	var currentValue float64
	var stats metricAccumulator
	emit := func(bucket uint8) {
		observeMetricRank(sets, bucket, currentValue)
		if wantStats && bucket != 0 && int(bucket) < len(sets) {
			stats.add(currentValue)
		}
	}
	useEmit := emit
	if visit == nil {
		useEmit = func(bucket uint8) {
			observeMetricRank(sets, bucket, currentValue)
		}
	}
	extra := 0
	for _, sample := range order {
		for extra < len(extras) && extras[extra].value <= sample.value {
			v := extras[extra]
			observeMetricRank(sets, uint8(v.row), float64(v.value))
			if wantStats && v.row != 0 {
				stats.add(float64(v.value))
			}
			extra++
		}
		if visit != nil {
			currentValue = float64(sample.value)
			visit(sample.row, useEmit)
		}
	}
	for _, v := range extras[extra:] {
		observeMetricRank(sets, uint8(v.row), float64(v.value))
		if wantStats && v.row != 0 {
			stats.add(float64(v.value))
		}
	}
	for i := range sets {
		sets[i].result(out[3*i:3*(i+1)], values[3*i:3*(i+1)])
	}
	if !wantStats {
		return buckets, period, nil
	}
	return buckets, period, stats.result()
}
