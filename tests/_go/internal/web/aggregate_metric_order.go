package web

import (
	"cmp"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"
)

func sortedMetricSamples[T ~int64 | ~float64](samples []metricSample[T]) []metricSample[T] {
	slices.SortFunc(samples, func(a, b metricSample[T]) int { return cmp.Compare(a.value, b.value) })
	return samples
}

func checkRankPercentiles[T ~int64 | ~float64](t *testing.T, values []T, membership []uint8, bucketCount int, extras []T, extraBuckets []uint8) {
	t.Helper()
	order := make([]metricSample[T], 0, len(values))
	bucketValues := make([][]T, bucketCount)
	var periodValues []T
	collect := func(value T, bucket uint8) {
		if !validMetricSample(value) || bucket == 0 || int(bucket) > bucketCount {
			return
		}
		bucketValues[bucket-1] = append(bucketValues[bucket-1], value)
		periodValues = append(periodValues, value)
	}
	for i, value := range values {
		if validMetricSample(value) {
			order = append(order, metricSample[T]{row: uint32(i), value: value})
		}
		collect(value, membership[i])
	}
	order = sortedMetricSamples(order)
	for i, value := range extras {
		collect(value, extraBuckets[i])
	}
	counts := make([]int, bucketCount)
	for i, vs := range bucketValues {
		counts[i] = len(vs)
	}
	beforeOrder, beforeMask := slices.Clone(order), slices.Clone(membership)
	beforeExtras, beforeExtraBuckets := slices.Clone(extras), slices.Clone(extraBuckets)
	gotBuckets, gotPeriod := rankPercentiles(order, membership, counts, extras, extraBuckets)
	check := func(label string, got []*float64, vs []T) {
		t.Helper()
		slices.Sort(vs)
		want := sortedSampleOracle(vs, 50, 95, 99)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s rank percentiles = %v, want %v (n=%d)", label, got, want, len(vs))
		}
	}
	check("period", gotPeriod, periodValues)
	if len(gotBuckets) != bucketCount {
		t.Fatalf("got %d buckets, want %d", len(gotBuckets), bucketCount)
	}
	for i, vs := range bucketValues {
		check("bucket", gotBuckets[i], vs)
	}
	if !slices.Equal(order, beforeOrder) || !slices.Equal(membership, beforeMask) ||
		!slices.EqualFunc(extras, beforeExtras, func(a, b T) bool { return a == b || math.IsNaN(float64(a)) && math.IsNaN(float64(b)) }) ||
		!slices.Equal(extraBuckets, beforeExtraBuckets) {
		t.Fatal("rank query mutated immutable inputs")
	}
}

func TestRankPercentilesSparseAndExtras(t *testing.T) {
	checkRankPercentiles[int64](t, nil, nil, 0, nil, nil)
	checkRankPercentiles[int64](t, nil, nil, 3, nil, nil)
	checkRankPercentiles(t, []int64{4, 1, 3, 2}, []uint8{1, 1, 1, 1}, 2, nil, nil)
	checkRankPercentiles(t, []int64{4, 1, 3}, []uint8{1, 1, 1}, 2, []int64{2, 8, 8, 8}, []uint8{1, 2, 2, 2})
	checkRankPercentiles(t, []int64{10, 20, 30, 40, 50}, []uint8{1, 0, 1, 2, 2}, 3,
		[]int64{15, 5, 1000, 0, -3, 60, 80}, []uint8{1, 1, 0, 1, 1, 2, 2})
	checkRankPercentiles[float64](t, nil, nil, 2, []float64{9, 1, 5, 3, math.NaN(), math.Inf(1), math.Inf(-1)}, []uint8{1, 1, 1, 1, 1, 1, 1})
	checkRankPercentiles(t, []int64{math.MaxInt64, math.MaxInt64 - 1, 1, 2, 3}, []uint8{1, 1, 1, 1, 1}, 1, nil, nil)
	checkRankPercentiles(t, []float64{1e-200, 1e-100, 1e100, 1e200}, []uint8{1, 1, 1, 1}, 1, nil, nil)
}

func TestRankPercentilesRandomized(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x219749, 0x18237))
	for trial := 0; trial < 200; trial++ {
		n, extraN, bucketCount := rng.IntN(3500), rng.IntN(50), 1+rng.IntN(31)
		ints, floats, mask := make([]int64, n), make([]float64, n), make([]uint8, n)
		for i := range ints {
			ints[i] = int64(rng.Uint64() >> 1)
			floats[i] = rng.Float64() * 1e9
			if trial%3 == 0 { // ties and missing samples
				ints[i] %= 5
				floats[i] = float64(ints[i])
			}
			mask[i] = uint8(rng.IntN(bucketCount + 1))
		}
		extraInts, extraFloats, extraMask := make([]int64, extraN), make([]float64, extraN), make([]uint8, extraN)
		for i := range extraInts {
			extraInts[i] = int64(rng.Uint64() >> 1)
			extraFloats[i] = rng.Float64() * 1e9
			extraMask[i] = uint8(rng.IntN(bucketCount + 1))
		}
		checkRankPercentiles(t, ints, mask, bucketCount, extraInts, extraMask)
		checkRankPercentiles(t, floats, mask, bucketCount, extraFloats, extraMask)
	}
}

func TestProjectionMetricAppendImmutable(t *testing.T) {
	rows := []contrib{
		{ttft: 40, tps: 4, dec: 400}, {ttft: 10, tps: 1, dec: 100}, {ttft: 0, tps: math.NaN(), dec: 0},
		{ttft: -3, tps: math.Inf(1), dec: -3}, {ttft: 30, tps: 3, dec: 0}, {ttft: 20, tps: 2, dec: 200},
		{ttft: 10, tps: 0, dec: 10}, {ttft: 0, tps: math.Inf(-1), dec: 1000},
	}
	var m projectionMetrics
	m.append(rows[:4], 0)
	first := m
	firstOrder := first.allOrder()
	beforeWait, beforeSpeed, beforeDec := slices.Clone(firstOrder.ttft), slices.Clone(firstOrder.tps), slices.Clone(firstOrder.dec)
	m.append(rows, 4)
	stale := first.allOrder()
	if !slices.Equal(stale.ttft, beforeWait) || !slices.Equal(stale.tps, beforeSpeed) || !slices.Equal(stale.dec, beforeDec) {
		t.Fatal("append mutated a previously published order")
	}
	order := m.allOrder()
	if len(order.ttft) != 5 || len(order.tps) != 4 || len(order.dec) != 5 {
		t.Fatalf("invalid sample accepted: ttft=%v tps=%v dec=%v", order.ttft, order.tps, order.dec)
	}
	check := func(n int, row uint32, value float64, expected float64) {
		t.Helper()
		if int(row) >= len(rows) || value != expected {
			t.Fatalf("sample %d lost row/value association: row=%d value=%v want=%v", n, row, value, expected)
		}
	}
	for i, sample := range order.ttft {
		check(i, sample.row, float64(sample.value), float64(rows[sample.row].ttft))
		if i > 0 && order.ttft[i-1].value > sample.value {
			t.Fatal("TTFT merge is not sorted")
		}
	}
	for i, sample := range order.tps {
		check(i, sample.row, sample.value, rows[sample.row].tps)
		if i > 0 && order.tps[i-1].value > sample.value {
			t.Fatal("TPS merge is not sorted")
		}
	}
	for i, sample := range order.dec {
		check(i, sample.row, float64(sample.value), float64(rows[sample.row].dec))
		if i > 0 && order.dec[i-1].value > sample.value {
			t.Fatal("decode merge is not sorted")
		}
	}
	previous := m
	m.append(rows, len(rows))
	again := m.allOrder()
	if &again.ttft[0] != &previous.allOrder().ttft[0] || &again.tps[0] != &previous.allOrder().tps[0] || &again.dec[0] != &previous.allOrder().dec[0] {
		t.Fatal("empty catch-up needlessly copied orders")
	}
	mask, counts := []uint8{1, 1, 0, 0, 1, 1, 1, 0}, []int{5}
	buckets, period := rankPercentiles(order.ttft, mask, counts, nil, nil)
	want := sortedSampleOracle([]int64{10, 10, 20, 30, 40}, 50, 95, 99)
	if !reflect.DeepEqual(period, want) || !reflect.DeepEqual(buckets[0], want) {
		t.Fatal("merged order produced incorrect exact percentiles")
	}
	// The decode order ranks its own sample population: rows 0,1,5,6 pass
	// both the validity gate and the mask (row 4's zero window is invalid,
	// row 7's 1000ms window is masked out), so counts differ from ttft's.
	decBuckets, decPeriod := rankPercentiles(order.dec, mask, []int{4}, nil, nil)
	decWant := sortedSampleOracle([]int64{10, 100, 200, 400}, 50, 95, 99)
	if !reflect.DeepEqual(decPeriod, decWant) || !reflect.DeepEqual(decBuckets[0], decWant) {
		t.Fatalf("decode order ranks = %v / %v, want %v", decPeriod, decBuckets[0], decWant)
	}
}

func TestRankPercentilesRejectsInconsistentCounts(t *testing.T) {
	order := []metricSample[int64]{{0, 1}, {1, 2}, {2, 3}, {3, 4}}
	for _, count := range []int{3, 5} {
		buckets, period := rankPercentiles(order, []uint8{1, 1, 1, 1}, []int{count}, nil, nil)
		if !reflect.DeepEqual(buckets[0], make([]*float64, 3)) || !reflect.DeepEqual(period, make([]*float64, 3)) {
			t.Fatal("mismatched count rendered invented ranks")
		}
	}
	buckets, period := rankPercentiles(order, []uint8{1, 1, 1, 1}, []int{4}, []int64{5}, nil)
	if !reflect.DeepEqual(buckets[0], make([]*float64, 3)) || !reflect.DeepEqual(period, make([]*float64, 3)) {
		t.Fatal("unpaired extras rendered partial ranks")
	}
}

func checkRankPercentilesVisit[T ~int64 | ~float64](t *testing.T, values []T, memberships [][]uint8, groupCount int, extras []T, extraGroups []uint8) {
	t.Helper()
	order := make([]metricSample[T], 0, len(values))
	groups := make([][]T, groupCount)
	var period []T
	collect := func(value T, group uint8) {
		if validMetricSample(value) && group > 0 && int(group) <= groupCount {
			groups[group-1] = append(groups[group-1], value)
			period = append(period, value)
		}
	}
	for row, value := range values {
		if validMetricSample(value) {
			order = append(order, metricSample[T]{row: uint32(row), value: value})
		}
		for _, group := range memberships[row] {
			collect(value, group)
		}
	}
	order = sortedMetricSamples(order)
	for i, value := range extras {
		collect(value, extraGroups[i])
	}
	counts := make([]int, groupCount)
	for i, samples := range groups {
		counts[i] = len(samples)
	}
	got, gotPeriod := rankPercentilesVisit(order, counts, extras, extraGroups, func(row uint32, emit func(uint8)) {
		for _, group := range memberships[row] {
			emit(group)
		}
	})
	for i, samples := range append(groups, period) {
		slices.Sort(samples)
		want := sortedSampleOracle(samples, 50, 95, 99)
		actual := gotPeriod
		if i < groupCount {
			actual = got[i]
		}
		if !reflect.DeepEqual(actual, want) {
			t.Fatalf("group %d exact occurrence ranks = %v, want %v (n=%d)", i, actual, want, len(samples))
		}
	}
}

func TestRankPercentilesVisitOccurrences(t *testing.T) {
	checkRankPercentilesVisit(t, []int64{40, 10, 30, 20, 1, 90},
		[][]uint8{{1, 1, 2}, {2, 1, 2}, nil, {1, 0}, {2, 2}, {0}}, 3,
		[]int64{4, 3, 2, 1, 9000}, []uint8{3, 3, 3, 3, 0})
	checkRankPercentilesVisit(t, []float64{4.2, .01, 3.7, 20.5},
		[][]uint8{{1, 1}, {1, 2}, {2, 2}, {2, 1}}, 2,
		[]float64{2.25, math.NaN(), math.Inf(1)}, []uint8{1, 1, 1})
	buckets, period := rankPercentilesVisit[int64](nil, []int{4}, []int64{4, 2, 3, 1}, []uint8{1, 1, 1, 1}, nil)
	want := sortedSampleOracle([]int64{1, 2, 3, 4}, 50, 95, 99)
	if !reflect.DeepEqual(buckets[0], want) || !reflect.DeepEqual(period, want) {
		t.Fatal("extras-only visitor failed")
	}
}

func TestRankPercentilesVisitRandomized(t *testing.T) {
	rng := rand.New(rand.NewPCG(0x82647, 0x913942))
	for trial := 0; trial < 100; trial++ {
		n, groupCount := rng.IntN(2000), 1+rng.IntN(24)
		ints, floats, memberships := make([]int64, n), make([]float64, n), make([][]uint8, n)
		for i := range ints {
			ints[i], floats[i] = int64(rng.IntN(500)), rng.Float64()*1e6
			for range rng.IntN(5) {
				group := uint8(rng.IntN(groupCount + 1))
				memberships[i] = append(memberships[i], group)
				if i%3 == 0 { // repeated tool/error occurrences in the same row
					memberships[i] = append(memberships[i], group)
				}
			}
		}
		extraInts, extraFloats, extraGroups := make([]int64, 20), make([]float64, 20), make([]uint8, 20)
		for i := range extraInts {
			extraInts[i], extraFloats[i] = int64(rng.IntN(500)), rng.Float64()*1e6
			extraGroups[i] = uint8(rng.IntN(groupCount + 1))
		}
		checkRankPercentilesVisit(t, ints, memberships, groupCount, extraInts, extraGroups)
		checkRankPercentilesVisit(t, floats, memberships, groupCount, extraFloats, extraGroups)
	}
}

var benchmarkRankBuckets [][]*float64
var benchmarkPercentileResult []*float64

func BenchmarkRankPercentiles100K(b *testing.B) {
	const n, bucketCount = 100_000, 30
	rng := rand.New(rand.NewPCG(0x238928, 0x891274))
	order := make([]metricSample[int64], n)
	for i := range order {
		order[i] = metricSample[int64]{row: uint32(i), value: 1 + int64(rng.Uint64()>>1)}
	}
	order = sortedMetricSamples(order)
	for _, scope := range []string{"all", "tenth"} {
		b.Run(scope, func(b *testing.B) {
			mask, counts := make([]uint8, n), make([]int, bucketCount)
			for i := range mask {
				if scope == "tenth" && i%10 != 0 {
					continue
				}
				mask[i] = uint8(i%bucketCount + 1)
				counts[i%bucketCount]++
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				benchmarkRankBuckets, benchmarkPercentileResult = rankPercentiles(order, mask, counts, nil, nil)
			}
		})
	}
}

func BenchmarkRankPercentilesVisit100K(b *testing.B) {
	const n, groupCount = 100_000, 24
	rng := rand.New(rand.NewPCG(0x238928, 0x891274))
	order := make([]metricSample[int64], n)
	offsets, members := make([]int, n+1), make([]uint8, 0, 2*n)
	counts := make([]int, groupCount)
	for i := range order {
		order[i] = metricSample[int64]{row: uint32(i), value: 1 + int64(rng.Uint64()>>1)}
		offsets[i] = len(members)
		if i%5 == 0 { // only selected/top-card groups receive memberships
			continue
		}
		for range 2 { // preserve repeated event memberships
			group := i % groupCount
			members = append(members, uint8(group+1))
			counts[group]++
		}
	}
	offsets[n] = len(members)
	order = sortedMetricSamples(order)
	visit := func(row uint32, emit func(uint8)) {
		for _, group := range members[offsets[row]:offsets[row+1]] {
			emit(group)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		benchmarkRankBuckets, benchmarkPercentileResult = rankPercentilesVisit(order, counts, nil, nil, visit)
	}
}

func metricSamplesByRow[T ~int64 | ~float64](samples []metricSample[T]) []metricSample[T] {
	out := slices.Clone(samples)
	slices.SortFunc(out, func(a, b metricSample[T]) int { return cmp.Compare(a.row, b.row) })
	return out
}

func checkMetricOrderSorted[T ~int64 | ~float64](t *testing.T, label string, samples []metricSample[T]) {
	t.Helper()
	for i := 1; i < len(samples); i++ {
		if samples[i-1].value > samples[i].value {
			t.Fatalf("%s order not sorted at %d: %v > %v", label, i, samples[i-1].value, samples[i].value)
		}
	}
}

func checkMetricOrdersEquivalent(t *testing.T, label string, got, want metricOrders) {
	t.Helper()
	if !reflect.DeepEqual(metricSamplesByRow(got.ttft), metricSamplesByRow(want.ttft)) ||
		!reflect.DeepEqual(metricSamplesByRow(got.tps), metricSamplesByRow(want.tps)) ||
		!reflect.DeepEqual(metricSamplesByRow(got.dec), metricSamplesByRow(want.dec)) {
		t.Fatalf("%s: incremental base+pending differs from a full rebuild", label)
	}
	checkMetricOrderSorted(t, label+" ttft", got.ttft)
	checkMetricOrderSorted(t, label+" tps", got.tps)
	checkMetricOrderSorted(t, label+" dec", got.dec)
}

func chartMembershipCounts[T ~int64 | ~float64](rows []contrib, mask []uint8, bucketCount int, value func(*contrib) T) []int {
	counts := make([]int, bucketCount)
	for i := range rows {
		if mask[i] == 0 || !validMetricSample(value(&rows[i])) {
			continue
		}
		if bucket := int(mask[i]); bucket <= bucketCount {
			counts[bucket-1]++
		}
	}
	return counts
}

// checkMetricRankEquivalence compares the exact chart and explorer readers over
// the incremental order with the same readers over a full rebuild.
func checkMetricRankEquivalence(t *testing.T, label string, order, rebuild metricOrders, rows []contrib, mask []uint8, bucketCount int, groups [][]uint8, groupCount int) {
	t.Helper()
	ttftCounts := chartMembershipCounts(rows, mask, bucketCount, func(c *contrib) int64 { return c.ttft })
	tpsCounts := chartMembershipCounts(rows, mask, bucketCount, func(c *contrib) float64 { return c.tps })
	decCounts := chartMembershipCounts(rows, mask, bucketCount, func(c *contrib) int64 { return c.dec })
	gotTTFT, gotPeriodTTFT, gotStatTTFT := rankPercentilesStats(order.ttft, mask, ttftCounts, nil, nil)
	wantTTFT, wantPeriodTTFT, wantStatTTFT := rankPercentilesStats(rebuild.ttft, mask, ttftCounts, nil, nil)
	if !reflect.DeepEqual(gotTTFT, wantTTFT) || !reflect.DeepEqual(gotPeriodTTFT, wantPeriodTTFT) || !reflect.DeepEqual(gotStatTTFT, wantStatTTFT) {
		t.Fatalf("%s: chart TTFT ranks/stats differ from a full rebuild", label)
	}
	gotTPS, gotPeriodTPS, gotStatTPS := rankPercentilesStats(order.tps, mask, tpsCounts, nil, nil)
	wantTPS, wantPeriodTPS, wantStatTPS := rankPercentilesStats(rebuild.tps, mask, tpsCounts, nil, nil)
	if !reflect.DeepEqual(gotTPS, wantTPS) || !reflect.DeepEqual(gotPeriodTPS, wantPeriodTPS) || !reflect.DeepEqual(gotStatTPS, wantStatTPS) {
		t.Fatalf("%s: chart TPS ranks/stats differ from a full rebuild", label)
	}
	gotDec, gotPeriodDec := rankPercentiles(order.dec, mask, decCounts, nil, nil)
	wantDec, wantPeriodDec := rankPercentiles(rebuild.dec, mask, decCounts, nil, nil)
	if !reflect.DeepEqual(gotDec, wantDec) || !reflect.DeepEqual(gotPeriodDec, wantPeriodDec) {
		t.Fatalf("%s: chart decode ranks differ from a full rebuild", label)
	}

	groupCounts := func(valid func(*contrib) bool) []int {
		counts := make([]int, groupCount)
		for i := range rows {
			if !valid(&rows[i]) {
				continue
			}
			for _, group := range groups[i] {
				if group > 0 && int(group) <= groupCount {
					counts[group-1]++
				}
			}
		}
		return counts
	}
	waitCounts := groupCounts(func(c *contrib) bool { return validMetricSample(c.ttft) })
	speedCounts := groupCounts(func(c *contrib) bool { return validMetricSample(c.tps) })
	visit := func(row uint32, emit func(uint8)) {
		for _, group := range groups[row] {
			emit(group)
		}
	}
	gotWait, gotWaitPeriod := rankPercentilesVisit(order.ttft, waitCounts, nil, nil, visit)
	wantWait, wantWaitPeriod := rankPercentilesVisit(rebuild.ttft, waitCounts, nil, nil, visit)
	if !reflect.DeepEqual(gotWait, wantWait) || !reflect.DeepEqual(gotWaitPeriod, wantWaitPeriod) {
		t.Fatalf("%s: explorer TTFT ranks differ from a full rebuild", label)
	}
	gotSpeed, gotSpeedPeriod := rankPercentilesVisit(order.tps, speedCounts, nil, nil, visit)
	wantSpeed, wantSpeedPeriod := rankPercentilesVisit(rebuild.tps, speedCounts, nil, nil, visit)
	if !reflect.DeepEqual(gotSpeed, wantSpeed) || !reflect.DeepEqual(gotSpeedPeriod, wantSpeedPeriod) {
		t.Fatalf("%s: explorer TPS ranks differ from a full rebuild", label)
	}
}

func TestProjectionMetricOrderMatchesFullRebuild(t *testing.T) {
	n := metricOrderPendingSamples*3 + 137
	rng := rand.New(rand.NewPCG(0x51627, 0x90813))
	rows := make([]contrib, n)
	for i := range rows {
		ttft := int64(1 + rng.IntN(5000))
		tps := float64(1 + rng.IntN(5000))
		dec := int64(1 + rng.IntN(5000))
		switch i % 23 {
		case 0:
			ttft = 0
		case 1:
			ttft = -1
		case 2:
			tps = math.NaN()
		case 3:
			tps = math.Inf(1)
		case 4:
			tps = 0
		case 5:
			dec = -5
		case 6:
			dec = 0
		case 7:
			ttft = math.MaxInt64
		}
		rows[i] = contrib{ttft: ttft, tps: tps, dec: dec}
	}
	const bucketCount, groupCount = 9, 7
	mask := make([]uint8, n)
	groups := make([][]uint8, n)
	for i := range rows {
		if rng.IntN(4) != 0 {
			mask[i] = uint8(rng.IntN(bucketCount) + 1)
		}
		for range rng.IntN(4) {
			groups[i] = append(groups[i], uint8(rng.IntN(groupCount+1)))
		}
	}

	var m projectionMetrics
	compactions := 0
	for from := 0; from < n; {
		to := min(from+1+rng.IntN(2500), n)
		pendingBefore := m.pending.sampleCount()
		m.append(rows[:to], from)
		if m.pending.sampleCount() < pendingBefore {
			compactions++
		}
		if got := m.pending.sampleCount(); got >= metricOrderPendingSamples {
			t.Fatalf("pending samples = %d at row %d, want below %d", got, to, metricOrderPendingSamples)
		}
		var rebuild projectionMetrics
		rebuild.append(rows[:to], 0)
		if got := rebuild.pending.sampleCount(); got != 0 {
			t.Fatalf("full rebuild retained %d pending samples", got)
		}
		checkMetricOrdersEquivalent(t, fmt.Sprintf("rows=%d", to), m.allOrder(), rebuild.allOrder())
		checkMetricRankEquivalence(t, fmt.Sprintf("rows=%d", to), m.allOrder(), rebuild.allOrder(),
			rows[:to], mask[:to], bucketCount, groups[:to], groupCount)
		from = to
	}
	if compactions < 2 {
		t.Fatalf("test crossed only %d compaction boundaries", compactions)
	}
}

func TestProjectionMetricOrderPendingBoundary(t *testing.T) {
	const baseRows = 10
	const threshold = metricOrderPendingSamples
	rows := make([]contrib, baseRows+threshold+2)
	for i := range rows {
		// Only TTFT is valid so one row is exactly one pending sample.
		rows[i] = contrib{ttft: int64(i + 1)}
	}
	newBase := func() projectionMetrics {
		var m projectionMetrics
		m.append(rows[:baseRows], 0)
		return m
	}

	below := newBase()
	below.append(rows[:baseRows+threshold-1], baseRows)
	if got := below.pending.sampleCount(); got != threshold-1 {
		t.Fatalf("below threshold: pending = %d, want %d", got, threshold-1)
	}
	if got := len(below.base.ttft); got != baseRows {
		t.Fatalf("below threshold compacted base to %d, want %d", got, baseRows)
	}
	if got := len(below.allOrder().ttft); got != baseRows+threshold-1 {
		t.Fatalf("below threshold order = %d, want %d", got, baseRows+threshold-1)
	}
	baseBefore, pendingBefore := slices.Clone(below.base.ttft), slices.Clone(below.pending.ttft)
	firstView := below.allOrder()
	secondView := below.allOrder()
	if &firstView.ttft[0] != &secondView.ttft[0] {
		t.Fatal("allOrder did not reuse its per-snapshot merge")
	}
	if !slices.Equal(below.base.ttft, baseBefore) || !slices.Equal(below.pending.ttft, pendingBefore) {
		t.Fatal("allOrder mutated base or pending")
	}

	at := below
	at.append(rows[:baseRows+threshold], baseRows+threshold-1)
	if got := at.pending.sampleCount(); got != 0 {
		t.Fatalf("at threshold: pending = %d, want 0 after compaction", got)
	}
	if got := len(at.base.ttft); got != baseRows+threshold {
		t.Fatalf("at threshold: base = %d, want %d", got, baseRows+threshold)
	}
	if got := len(below.pending.ttft); got != threshold-1 {
		t.Fatalf("compaction mutated the previous pending delta: %d", got)
	}
	if got := len(at.allOrder().ttft); got != baseRows+threshold {
		t.Fatalf("at threshold order = %d, want %d", got, baseRows+threshold)
	}

	above := newBase()
	above.append(rows[:baseRows+threshold+1], baseRows)
	if got := above.pending.sampleCount(); got != 0 {
		t.Fatalf("above threshold: pending = %d, want 0 after compaction", got)
	}
	if got := len(above.base.ttft); got != baseRows+threshold+1 {
		t.Fatalf("above threshold: base = %d, want %d", got, baseRows+threshold+1)
	}

	// Crossing the exact bound once leaves later samples pending again.
	at.append(rows[:baseRows+threshold+1], baseRows+threshold)
	if got := at.pending.sampleCount(); got != 1 {
		t.Fatalf("post-compaction delta: pending = %d, want 1", got)
	}
	if got := len(at.base.ttft); got != baseRows+threshold {
		t.Fatalf("post-compaction base = %d, want %d", got, baseRows+threshold)
	}

	const appended = baseRows + threshold + 1
	var full projectionMetrics
	full.append(rows[:appended], 0)
	if got := full.pending.sampleCount(); got != 0 {
		t.Fatalf("full rebuild retained %d pending samples", got)
	}
	if got := len(full.base.ttft); got != appended {
		t.Fatalf("full rebuild base = %d, want %d", got, appended)
	}
	checkMetricOrdersEquivalent(t, "full rebuild", at.allOrder(), full.allOrder())
}

// TestProjectionMetricOrderConcurrentLease exercises the shared projection
// RWMutex: readers hold the read lease and fold the current snapshot while the
// writer publishes base+pending snapshots. allOrder's cache must be safe for
// concurrent readers and invisible to later appends.
func TestProjectionMetricOrderConcurrentLease(t *testing.T) {
	const (
		initial = 20000
		batch   = 64
		batches = 220
	)
	rows := benchmarkMetricRows(initial + batch*batches)
	var p historyProjection
	p.data.metrics.append(rows[:initial], 0)
	done := make(chan struct{})
	errs := make(chan string, 8)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				p.mu.RLock()
				order := p.data.metrics.allOrder()
				if len(order.ttft) != len(order.tps) || len(order.ttft) != len(order.dec) {
					select {
					case errs <- "metric order lengths diverged":
					default:
					}
					p.mu.RUnlock()
					return
				}
				for i := 1; i < len(order.ttft); i++ {
					if order.ttft[i-1].value > order.ttft[i].value {
						select {
						case errs <- "concurrent TTFT order not sorted":
						default:
						}
						p.mu.RUnlock()
						return
					}
				}
				mask := make([]uint8, len(order.ttft))
				for i := range mask {
					mask[i] = 1
				}
				_, _, stat := rankPercentilesStats(order.ttft, mask, []int{len(mask)}, nil, nil)
				if stat == nil {
					select {
					case errs <- "concurrent rank fold returned no stats":
					default:
					}
					p.mu.RUnlock()
					return
				}
				p.mu.RUnlock()
			}
		}()
	}
	for i := range batches {
		from := initial + i*batch
		to := from + batch
		p.mu.Lock()
		data := p.data
		data.metrics.append(rows[:to], from)
		p.data = data
		p.mu.Unlock()
	}
	close(done)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func shuffledMetricRows(n int) []contrib {
	rows := make([]contrib, n)
	for i := range rows {
		// A non-monotone value order keeps the fixture from passing when a
		// merge forgets to sort.
		v := int64((i*7919)%n + 1)
		rows[i] = contrib{ttft: v, tps: float64(v), dec: v}
	}
	return rows
}

// TestProjectionRecentOrderMatchesAllForTailWindows pins the bounded-chart
// fast path: when every in-window row is inside the retained tail, ranks and
// occurrence-weighted stats from the short order are exactly the all-history
// answers. A sub-cadence delta must advance the tail without a rebuild; a
// larger delta must rebuild it, and the tail's own bound and immutability are
// asserted directly.
func TestProjectionRecentOrderMatchesAllForTailWindows(t *testing.T) {
	n := recentMetricRows + 5000
	rows := shuffledMetricRows(n)
	var m projectionMetrics
	m.append(rows, 0)
	if m.recentFrom != n-recentMetricRows {
		t.Fatalf("recentFrom = %d, want %d", m.recentFrom, n-recentMetricRows)
	}
	checkBound := func() {
		t.Helper()
		if got := len(m.recent.ttft); got != recentMetricRows {
			t.Fatalf("retained TTFT samples = %d, want %d", got, recentMetricRows)
		}
		if got := len(m.recent.tps); got != recentMetricRows {
			t.Fatalf("retained TPS samples = %d, want %d", got, recentMetricRows)
		}
		if got := len(m.recent.dec); got != recentMetricRows {
			t.Fatalf("retained decode samples = %d, want %d", got, recentMetricRows)
		}
		for _, s := range m.recent.ttft {
			if int(s.row) < m.recentFrom {
				t.Fatalf("recent TTFT sample row %d predates recentFrom %d", s.row, m.recentFrom)
			}
		}
	}
	checkBound()
	check := func(from int) {
		t.Helper()
		mask := make([]uint8, n)
		for i := from; i < n; i++ {
			mask[i] = 1
		}
		counts := []int{n - from}
		all := m.allOrder()
		allTTFT, allPeriodTTFT, allStatTTFT := rankPercentilesStats(all.ttft, mask, counts, nil, nil)
		recentTTFT, recentPeriodTTFT, recentStatTTFT := rankPercentilesStats(m.recent.ttft, mask, counts, nil, nil)
		if !reflect.DeepEqual(allTTFT, recentTTFT) || !reflect.DeepEqual(allPeriodTTFT, recentPeriodTTFT) ||
			!reflect.DeepEqual(allStatTTFT, recentStatTTFT) {
			t.Fatalf("TTFT tail ranks differ from all-history ranks (from=%d)", from)
		}
		allTPS, allPeriodTPS, allStatTPS := rankPercentilesStats(all.tps, mask, counts, nil, nil)
		recentTPS, recentPeriodTPS, recentStatTPS := rankPercentilesStats(m.recent.tps, mask, counts, nil, nil)
		if !reflect.DeepEqual(allTPS, recentTPS) || !reflect.DeepEqual(allPeriodTPS, recentPeriodTPS) ||
			!reflect.DeepEqual(allStatTPS, recentStatTPS) {
			t.Fatalf("TPS tail ranks differ from all-history ranks (from=%d)", from)
		}
		allDec, allPeriodDec := rankPercentiles(all.dec, mask, counts, nil, nil)
		recentDec, recentPeriodDec := rankPercentiles(m.recent.dec, mask, counts, nil, nil)
		if !reflect.DeepEqual(allDec, recentDec) || !reflect.DeepEqual(allPeriodDec, recentPeriodDec) {
			t.Fatalf("decode tail ranks differ from all-history ranks (from=%d)", from)
		}
	}
	check(m.recentFrom)

	// A delta below the rebuild cadence advances the window and merges only
	// new samples; the previously published arrays must stay untouched.
	oldBuiltAt := m.recentBuiltAt
	publishedTTFT := m.recent.ttft
	publishedTTFTCopy := slices.Clone(publishedTTFT)
	delta := make([]contrib, 1000)
	for i := range delta {
		v := int64(n + i + 1)
		delta[i] = contrib{ttft: v, tps: float64(v), dec: v}
	}
	rows = append(rows, delta...)
	m.append(rows, n)
	n = len(rows)
	if m.recentBuiltAt != oldBuiltAt {
		t.Fatalf("sub-cadence delta rebuilt at %d, want %d", m.recentBuiltAt, oldBuiltAt)
	}
	if m.recentFrom != n-recentMetricRows {
		t.Fatalf("advanced recentFrom = %d, want %d", m.recentFrom, n-recentMetricRows)
	}
	if !slices.Equal(publishedTTFT, publishedTTFTCopy) {
		t.Fatal("recent append mutated a previously published order")
	}
	check(m.recentFrom)

	// A batch at the rebuild cadence replaces the tail, bounds it exactly,
	// and still answers a window at the new boundary from the short order.
	delta = make([]contrib, 20000)
	for i := range delta {
		v := int64(n + i + 1)
		delta[i] = contrib{ttft: v, tps: float64(v), dec: v}
	}
	rows = append(rows, delta...)
	m.append(rows, n)
	n = len(rows)
	if m.recentFrom != n-recentMetricRows {
		t.Fatalf("advanced recentFrom = %d, want %d", m.recentFrom, n-recentMetricRows)
	}
	if m.recentBuiltAt != n {
		t.Fatalf("recentBuiltAt = %d, want a rebuild at %d", m.recentBuiltAt, n)
	}
	checkBound()
	check(m.recentFrom)
}

// TestChartRecentTailMatchesAllHistory exercises the real chartFold selector:
// the fast fold's all-history order is deliberately emptied after prepare, so
// a wrong or missing tail selection cannot silently pass. The same window
// folded with the all-history order must produce a byte-identical payload.
func TestChartRecentTailMatchesAllHistory(t *testing.T) {
	const n = recentMetricRows + 5000
	now := time.Now().UnixMilli()
	rows := make([]contrib, n)
	for i := range rows {
		start := now - int64(n-1-i)*1000
		v := int64((i*7919)%n + 1)
		rows[i] = contrib{start: start, rowIndex: uint32(i + 1), ttft: v, tps: float64(v)}
	}
	p := &projectionData{rows: rows}
	p.metrics.append(rows, 0)
	from, step, count := chartWindowEdges(now, now-15*60*1000)
	first := n - 1 - int((now-from)/1000)
	if first < 0 {
		first = 0
	}
	fold := func(forceAll bool) chartPayload {
		t.Helper()
		f := newChartFold(from, step, count, "", nil)
		f.prepare(p, nil)
		if forceAll {
			f.recentFrom = 0
		} else {
			// The fast path must select recent; if it falls back to order,
			// the empty all-history order yields empty ranks.
			f.order = metricOrders{}
		}
		for i := first; i < n; i++ {
			if err := f.fold(&rows[i]); err != nil {
				t.Fatal(err)
			}
			if !forceAll && f.recentFrom == 0 {
				t.Fatalf("recentFrom reset at row index %d (rowIndex=%d)", i, rows[i].rowIndex)
			}
		}
		if !forceAll && (!f.hasMember || f.recentFrom == 0 || int(f.minMemberRow) <= f.recentFrom) {
			t.Fatalf("fast fold did not select the tail: hasMember=%v recentFrom=%d minMemberRow=%d", f.hasMember, f.recentFrom, f.minMemberRow)
		}
		f.freezeOrder()
		if !forceAll && len(f.order.ttft) != 0 {
			t.Fatal("tail fast path materialized the all-history order")
		}
		return f.payload(now)
	}
	fast, slow := fold(false), fold(true)
	if !reflect.DeepEqual(fast, slow) {
		t.Fatal("tail-selected chart payload differs from all-history payload")
	}
}
