package web

import (
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
)

func clipMetricSamplesFrom[T ~int64 | ~float64](samples []metricSample[T], from int) []metricSample[T] {
	out := make([]metricSample[T], 0, len(samples))
	for _, sample := range samples {
		if int(sample.row) >= from {
			out = append(out, sample)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// clipMetricOrdersFrom returns the exact selection a bounded window may read
// from the retained tail: every sample at or after the 0-based recentFrom.
func clipMetricOrdersFrom(order metricOrders, from int) metricOrders {
	return metricOrders{
		ttft: clipMetricSamplesFrom(order.ttft, from),
		tps:  clipMetricSamplesFrom(order.tps, from),
		dec:  clipMetricSamplesFrom(order.dec, from),
	}
}

func clipMetricSamplesBefore[T ~int64 | ~float64](samples []metricSample[T], to int) []metricSample[T] {
	out := make([]metricSample[T], 0, len(samples))
	for _, sample := range samples {
		if int(sample.row) < to {
			out = append(out, sample)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func clipMetricOrdersBefore(order metricOrders, to int) metricOrders {
	return metricOrders{
		ttft: clipMetricSamplesBefore(order.ttft, to),
		tps:  clipMetricSamplesBefore(order.tps, to),
		dec:  clipMetricSamplesBefore(order.dec, to),
	}
}

// TestProjectionRetentionDocumentedBounds pins the documented projection
// retention sizes independently of the implementation constants: the retained
// chart tail is the last 65,536 rows, and each uncompacted pending delta is
// bounded at 16,384 samples. Changing either constant must move the
// architecture contract with it instead of leaving the docs behind.
func TestProjectionRetentionDocumentedBounds(t *testing.T) {
	const (
		documentedRecentMetricRows          = 65_536
		documentedMetricOrderPendingSamples = 16_384
	)
	if recentMetricRows != documentedRecentMetricRows {
		t.Errorf("recentMetricRows = %d, want the documented %d-row tail", recentMetricRows, documentedRecentMetricRows)
	}
	if metricOrderPendingSamples != documentedMetricOrderPendingSamples {
		t.Errorf("metricOrderPendingSamples = %d, want the documented %d-sample pending delta", metricOrderPendingSamples, documentedMetricOrderPendingSamples)
	}
}

// TestProjectionRecentOrderMatchesFullRebuild feeds randomized append batches
// through the incremental recent base+pending path and compares its exact tail
// selection, chart buckets and explorer ranks with a fresh full rebuild for the
// same rows: across sub-cadence deltas, pending compactions, rebuild boundaries
// and a destructive reset.
func TestProjectionRecentOrderMatchesFullRebuild(t *testing.T) {
	n := recentMetricRows + 2*recentMetricRebuildRows + 2500
	rng := rand.New(rand.NewPCG(0x51c27, 0x90813))
	rows := make([]contrib, n)
	for i := range rows {
		ttft := int64(1 + rng.IntN(3000))
		tps := float64(1 + rng.IntN(3000))
		dec := int64(1 + rng.IntN(3000))
		switch i % 31 {
		case 0:
			ttft = 0
		case 1:
			tps = math.NaN()
		case 2:
			dec = 0
		case 3:
			ttft = math.MaxInt64
		case 4:
			tps = math.Inf(1)
		}
		rows[i] = contrib{ttft: ttft, tps: tps, dec: dec}
	}

	const bucketCount, groupCount = 5, 3
	var m projectionMetrics
	compactions := 0
	for from := 0; from < n; {
		to := min(from+1+rng.IntN(6000), n)
		pendingBefore := m.recentPending.sampleCount()
		m.append(rows[:to], from)
		if m.recentPending.sampleCount() < pendingBefore {
			compactions++
		}
		if got := m.recentPending.sampleCount(); got >= metricOrderPendingSamples {
			t.Fatalf("recent pending samples = %d at row %d, want below %d", got, to, metricOrderPendingSamples)
		}
		label := fmt.Sprintf("incremental rows=%d", to)
		var rebuild projectionMetrics
		rebuild.append(rows[:to], 0)
		wantFrom := max(0, to-recentMetricRows)
		if m.recentFrom != wantFrom || rebuild.recentFrom != wantFrom {
			t.Fatalf("%s: recentFrom = %d / rebuild %d, want %d", label, m.recentFrom, rebuild.recentFrom, wantFrom)
		}
		got := clipMetricOrdersFrom(m.recentOrder(), m.recentFrom)
		want := rebuild.recentOrder()
		checkMetricOrdersEquivalent(t, label, got, want)
		if to > recentMetricRows && m.recentBuiltAt == to {
			// A rebuild discards every stale out-of-tail sample.
			checkMetricOrdersEquivalent(t, label+" rebuild", m.recentOrder(), want)
		}
		if to <= recentMetricRows {
			from = to
			continue
		}
		first := m.recentFrom + rng.IntN(to-m.recentFrom)
		mask := make([]uint8, to)
		groups := make([][]uint8, to)
		for i := first; i < to; i++ {
			mask[i] = uint8((i-first)%bucketCount + 1)
			for range rng.IntN(3) {
				groups[i] = append(groups[i], uint8(rng.IntN(groupCount+1)))
			}
		}
		checkMetricRankEquivalence(t, label+" tail", m.recentOrder(), rebuild.recentOrder(),
			rows[:to], mask, bucketCount, groups, groupCount)
		from = to
	}
	if compactions < 2 {
		t.Fatalf("test crossed only %d recent pending compaction boundaries", compactions)
	}

	// A destructive reset drops the tail entirely until history grows again.
	resetRows := recentMetricRows / 2
	m.append(rows[:resetRows], 0)
	if m.recentBase.sampleCount() != 0 || m.recentPending.sampleCount() != 0 || m.recentView != nil ||
		m.recentFrom != 0 || m.recentBuiltAt != 0 {
		t.Fatalf("destructive reset retained recent state: base=%d pending=%d view=%v from=%d builtAt=%d",
			m.recentBase.sampleCount(), m.recentPending.sampleCount(), m.recentView, m.recentFrom, m.recentBuiltAt)
	}
	if got := m.recentOrder().sampleCount(); got != 0 {
		t.Fatalf("destructive reset retained %d recent samples", got)
	}
	var resetRebuild projectionMetrics
	resetRebuild.append(rows[:resetRows], 0)
	checkMetricOrdersEquivalent(t, "destructive reset", m.recentOrder(), resetRebuild.recentOrder())

	// A catch-up past the tail rebuilds it; a later sub-cadence delta folds
	// only the new batch into the pending tail.
	to := resetRows + 40000
	m.append(rows[:to], resetRows)
	rebuild := projectionMetrics{}
	rebuild.append(rows[:to], 0)
	if m.recentFrom != to-recentMetricRows || m.recentBuiltAt != to {
		t.Fatalf("post-reset rebuild: from=%d builtAt=%d, want %d / %d", m.recentFrom, m.recentBuiltAt, to-recentMetricRows, to)
	}
	checkMetricOrdersEquivalent(t, "post-reset rebuild", m.recentOrder(), rebuild.recentOrder())

	deltaEnd := to + 1000
	m.append(rows[:deltaEnd], to)
	if m.recentBuiltAt != to {
		t.Fatalf("post-reset sub-cadence delta rebuilt at %d, want %d", m.recentBuiltAt, to)
	}
	rebuild = projectionMetrics{}
	rebuild.append(rows[:deltaEnd], 0)
	if m.recentFrom != deltaEnd-recentMetricRows {
		t.Fatalf("post-reset delta recentFrom = %d, want %d", m.recentFrom, deltaEnd-recentMetricRows)
	}
	checkMetricOrdersEquivalent(t, "post-reset delta", clipMetricOrdersFrom(m.recentOrder(), m.recentFrom), rebuild.recentOrder())
}

// TestProjectionRecentOrderKeepsOldSamplesFirstOnTies pins the merge's tie
// rule: batching old samples before equal-valued new ones, while the order
// remains ascending.
func TestProjectionRecentOrderKeepsOldSamplesFirstOnTies(t *testing.T) {
	const n = recentMetricRows + 3000
	const delta = 100
	rows := make([]contrib, n+delta)
	for i := range rows {
		rows[i] = contrib{ttft: 7, tps: 7, dec: 7}
	}
	var m projectionMetrics
	m.append(rows[:n], 0)
	if got := len(m.recentOrder().ttft); got != recentMetricRows {
		t.Fatalf("tail samples = %d, want %d", got, recentMetricRows)
	}
	m.append(rows, n)
	order := m.recentOrder()
	checkMetricOrderSorted(t, "tie ttft", order.ttft)
	checkMetricOrderSorted(t, "tie tps", order.tps)
	checkMetricOrderSorted(t, "tie dec", order.dec)
	seenNew := false
	for _, sample := range order.ttft {
		if int(sample.row) >= n {
			seenNew = true
		} else if seenNew {
			t.Fatal("equal-valued new sample sorted before an older sample")
		}
	}
	if !seenNew {
		t.Fatal("delta samples missing from the tail order")
	}
}

// TestChartRecentTailSelectionAndFallback exercises the real chart fold:
// a window inside the retained tail reads only rows >= recentFrom even while
// the base still retains older escaped samples, while a window needing older
// rows materializes the exact all-history order.
func TestChartRecentTailSelectionAndFallback(t *testing.T) {
	const delta = 500
	const total = recentMetricRows + recentMetricRebuildRows + 5000
	const initial = total - delta
	now := time.Now().UnixMilli()
	rows := make([]contrib, total)
	for i := range rows {
		start := now - int64(total-1-i)*1000
		v := int64((i*7919)%total + 1)
		rows[i] = contrib{start: start, rowIndex: uint32(i + 1), ttft: v, tps: float64(v), dec: v}
	}
	// Rows that escape the tail after the sub-cadence delta carry extreme
	// values: they remain in the retained base but must never rank.
	escapedFrom := initial - recentMetricRows
	escapedTo := total - recentMetricRows
	for i := escapedFrom; i < escapedTo; i++ {
		rows[i].ttft = math.MaxInt64
		rows[i].tps = math.MaxFloat64
		rows[i].dec = math.MaxInt64
	}
	var metrics projectionMetrics
	metrics.append(rows[:initial], 0)
	metrics.append(rows, initial)
	if metrics.recentFrom != escapedTo || metrics.recentBuiltAt != initial {
		t.Fatalf("expected a sub-cadence tail advance: from=%d builtAt=%d, want %d / %d",
			metrics.recentFrom, metrics.recentBuiltAt, escapedTo, initial)
	}
	if !slices.ContainsFunc(metrics.recentOrder().ttft, func(s metricSample[int64]) bool {
		return int(s.row) < metrics.recentFrom
	}) {
		t.Fatal("escaped samples were not retained for the bounded rebuild cadence")
	}

	first := metrics.recentFrom + 10
	mask := make([]uint8, total)
	const bucketCount = 3
	for i := first; i < total; i++ {
		mask[i] = uint8((i-first)%bucketCount + 1)
	}
	counts := chartMembershipCounts(rows, mask, bucketCount, func(c *contrib) int64 { return c.ttft })
	all := metrics.allOrder()
	recent := metrics.recentOrder()
	allBuckets, allPeriod, allStats := rankPercentilesStats(all.ttft, mask, counts, nil, nil)
	recentBuckets, recentPeriod, recentStats := rankPercentilesStats(recent.ttft, mask, counts, nil, nil)
	if !reflect.DeepEqual(allBuckets, recentBuckets) || !reflect.DeepEqual(allPeriod, recentPeriod) ||
		!reflect.DeepEqual(allStats, recentStats) {
		t.Fatal("escaped out-of-tail samples changed the bounded-window ranks")
	}

	p := &projectionData{rows: rows}
	p.metrics = metrics
	fold := func(first int, from, step int64, count int, forceAll bool) *chartFold {
		t.Helper()
		f := newChartFold(from, step, count, "", nil)
		f.prepare(p, nil)
		if forceAll {
			f.recentFrom = 0
		}
		for i := first; i < total; i++ {
			if err := f.fold(&rows[i]); err != nil {
				t.Fatal(err)
			}
		}
		f.freezeOrder()
		return f
	}

	fastFrom, fastStep, fastCount := chartWindowEdges(now, rows[total-1000].start)
	fast := fold(total-1000, fastFrom, fastStep, fastCount, false)
	if !fast.recentCovers() || fast.order.sampleCount() != 0 {
		t.Fatalf("fast fold did not select the tail: covers=%v allHistory=%d", fast.recentCovers(), fast.order.sampleCount())
	}
	if got, want := fast.recent, metrics.recentOrderSource(); !reflect.DeepEqual(got, want) {
		t.Fatal("fast fold did not select the exact tail source")
	}
	fastOracle := fold(total-1000, fastFrom, fastStep, fastCount, true)
	if !reflect.DeepEqual(fast.payload(now), fastOracle.payload(now)) {
		t.Fatal("tail-selected chart payload differs from the all-history payload")
	}

	allFrom, allStep, allCount := chartWindowEdges(now, rows[0].start)
	fallback := fold(metrics.recentFrom-100, allFrom, allStep, allCount, false)
	if fallback.recentCovers() || fallback.recent.sampleCount() != 0 {
		t.Fatalf("older window used the tail: covers=%v recent=%d", fallback.recentCovers(), fallback.recent.sampleCount())
	}
	if got, want := fallback.order.sampleCount(), metrics.allOrderSource().sampleCount(); got != want {
		t.Fatalf("fallback selected %d all-history samples, want %d", got, want)
	}
	fallbackOracle := fold(metrics.recentFrom-100, allFrom, allStep, allCount, true)
	if !reflect.DeepEqual(fallback.payload(now), fallbackOracle.payload(now)) {
		t.Fatal("all-history fallback payload differs from the all-history oracle")
	}
}

// TestProjectionRecentAppendAllocationBounded pins the bounded append: a small
// catch-up must allocate only its own batch, never a copy of the retained tail.
func TestProjectionRecentAppendAllocationBounded(t *testing.T) {
	const history = recentMetricRows + 127
	rows := benchmarkMetricRows(history + 16)
	var base projectionMetrics
	base.append(rows[:history], 0)
	if got := len(base.recentOrder().ttft); got != recentMetricRows {
		t.Fatalf("tail samples = %d, want %d", got, recentMetricRows)
	}
	alloc, _ := allocStats(func() {
		m := base
		m.append(rows, history)
		benchmarkProjectionMetrics = m
	})
	const limit = 64 << 10
	if alloc > limit {
		t.Fatalf("sub-threshold append allocated %d bytes/op, want at most %d (no tail-sized copy)", alloc, limit)
	}
}

// TestProjectionRecentPendingCompaction fills the recent delta to the shared
// pending bound, then crosses it: base+pending fold into one exact order,
// the old view stays immutable, and the next reader sees the new snapshot.
func TestProjectionRecentPendingCompaction(t *testing.T) {
	const history = recentMetricRows + 1000
	// 5460 fully populated rows hold 16380 samples; one TTFT-only row then
	// leaves the delta exactly one three-sample row below the shared bound.
	belowRows := (metricOrderPendingSamples - 4) / 3
	total := history + belowRows + 200
	rows := benchmarkMetricRows(total)
	rows[history+belowRows] = contrib{ttft: 42}
	var m projectionMetrics
	m.append(rows[:history], 0)
	if got := m.recentBase.sampleCount(); got != recentMetricRows*3 {
		t.Fatalf("rebuilt tail base samples = %d, want %d", got, recentMetricRows*3)
	}
	if got := m.recentPending.sampleCount(); got != 0 {
		t.Fatalf("full rebuild retained %d recent pending samples", got)
	}

	from := history
	m.append(rows[:from+belowRows], from)
	from += belowRows
	if got := m.recentPending.sampleCount(); got != belowRows*3 {
		t.Fatalf("pending samples = %d, want %d", got, belowRows*3)
	}
	m.append(rows[:from+1], from)
	from++
	belowPending := m.recentPending.sampleCount()
	if got := m.recentPending.sampleCount(); got != metricOrderPendingSamples-3 {
		t.Fatalf("pending below the bound = %d, want %d", got, metricOrderPendingSamples-3)
	}
	if got := m.recentBase.sampleCount(); got != recentMetricRows*3 {
		t.Fatalf("sub-threshold append compacted the base to %d", got)
	}
	oldView := m.recentView
	before := m.recentOrder()
	if oldView == nil || m.recentView != oldView {
		t.Fatal("recent append did not publish a fresh view holder")
	}
	if got := len(oldView.order.ttft); got != recentMetricRows+belowRows+1 {
		t.Fatalf("memoized view samples = %d, want %d", got, recentMetricRows+belowRows+1)
	}
	beforeCopy := metricOrders{
		ttft: slices.Clone(before.ttft),
		tps:  slices.Clone(before.tps),
		dec:  slices.Clone(before.dec),
	}

	// One more row (three samples) crosses the shared pending bound.
	compactedFrom := from
	m.append(rows[:from+1], from)
	from++
	if got := m.recentPending.sampleCount(); got != 0 {
		t.Fatalf("crossing the bound left %d recent pending samples", got)
	}
	if got := m.recentBase.sampleCount(); got != recentMetricRows*3+belowPending+3 {
		t.Fatalf("compacted base samples = %d, want %d", got, recentMetricRows*3+belowPending+3)
	}
	if m.recentBuiltAt != history {
		t.Fatalf("compaction triggered a rebuild at %d, want builtAt %d", m.recentBuiltAt, history)
	}
	if m.recentView == oldView {
		t.Fatal("compaction reused the stale view holder")
	}
	if !reflect.DeepEqual(oldView.order, beforeCopy) {
		t.Fatal("compaction mutated the previously published view")
	}
	after := m.recentOrder()
	checkMetricOrdersEquivalent(t, "pending compaction", clipMetricOrdersBefore(after, compactedFrom), beforeCopy)
	if &after.ttft[0] != &m.recentOrder().ttft[0] || &after.tps[0] != &m.recentOrder().tps[0] ||
		&after.dec[0] != &m.recentOrder().dec[0] {
		t.Fatal("recentOrder did not reuse its per-snapshot merge")
	}
	want := collectMetricOrders(rows[:from], history-recentMetricRows)
	want.sort()
	checkMetricOrdersEquivalent(t, "compacted contiguous tail", after, want)
	checkMetricOrderSorted(t, "compacted ttft", after.ttft)
	checkMetricOrderSorted(t, "compacted tps", after.tps)
	checkMetricOrderSorted(t, "compacted dec", after.dec)

	// A later sub-cadence append merges only its own batch into the pending
	// delta and still reconstructs the exact contiguous tail.
	m.append(rows[:from+100], from)
	from += 100
	if got := m.recentPending.sampleCount(); got != 300 {
		t.Fatalf("post-compaction pending samples = %d, want 300", got)
	}
	if m.recentFrom != from-recentMetricRows {
		t.Fatalf("post-compaction recentFrom = %d, want %d", m.recentFrom, from-recentMetricRows)
	}
	want = collectMetricOrders(rows[:from], history-recentMetricRows)
	want.sort()
	checkMetricOrdersEquivalent(t, "post-compaction append", m.recentOrder(), want)
}
