package web

import (
	"cmp"
	"math"
	"slices"
)

// metricSample pins a positive finite metric value to its immutable projection
// row. The sorted order belongs to projection catch-up, never a proxy request.
type metricSample[T ~int64 | ~float64] struct {
	row   uint32
	value T
}

type projectionMetrics struct {
	ttft []metricSample[int64]
	tps  []metricSample[float64]
}

func validMetricSample[T ~int64 | ~float64](value T) bool {
	return value > 0 && !math.IsInf(float64(value), 0)
}

// append sorts only new rows and merges into fresh immutable metric orders.
// Previously published slices are never changed, including their spare capacity.
// A full rebuild starts with an empty projectionMetrics and from == 0.
func (m *projectionMetrics) append(rows []contrib, from int) {
	if from >= len(rows) {
		return
	}
	wait := make([]metricSample[int64], 0, len(rows)-from)
	speed := make([]metricSample[float64], 0, len(rows)-from)
	for i := from; i < len(rows); i++ {
		c := &rows[i]
		if validMetricSample(c.ttft) {
			wait = append(wait, metricSample[int64]{row: uint32(i), value: c.ttft})
		}
		if validMetricSample(c.tps) {
			speed = append(speed, metricSample[float64]{row: uint32(i), value: c.tps})
		}
	}
	m.ttft = mergeMetricOrder(m.ttft, wait)
	m.tps = mergeMetricOrder(m.tps, speed)
}

func mergeMetricOrder[T ~int64 | ~float64](old, added []metricSample[T]) []metricSample[T] {
	if len(added) == 0 {
		return old
	}
	slices.SortFunc(added, func(a, b metricSample[T]) int { return cmp.Compare(a.value, b.value) })
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
	if total < pctMinSamples || len(extraValues) != len(extraBuckets) {
		return buckets, period
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
	emit := func(bucket uint8) { observeMetricRank(sets, bucket, currentValue) }
	extra := 0
	for _, sample := range order {
		for extra < len(extras) && extras[extra].value <= sample.value {
			v := extras[extra]
			observeMetricRank(sets, uint8(v.row), float64(v.value))
			extra++
		}
		if visit != nil {
			currentValue = float64(sample.value)
			visit(sample.row, emit)
		}
	}
	for _, v := range extras[extra:] {
		observeMetricRank(sets, uint8(v.row), float64(v.value))
	}
	for i := range sets {
		sets[i].result(out[3*i:3*(i+1)], values[3*i:3*(i+1)])
	}
	return buckets, period
}
