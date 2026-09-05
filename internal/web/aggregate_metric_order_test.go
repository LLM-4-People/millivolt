package web

import (
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
)

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
	order = mergeMetricOrder(nil, order)
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
		{ttft: 40, tps: 4}, {ttft: 10, tps: 1}, {ttft: 0, tps: math.NaN()},
		{ttft: -3, tps: math.Inf(1)}, {ttft: 30, tps: 3}, {ttft: 20, tps: 2},
		{ttft: 10, tps: 0}, {ttft: 0, tps: math.Inf(-1)},
	}
	var m projectionMetrics
	m.append(rows[:4], 0)
	first := m
	beforeWait, beforeSpeed := slices.Clone(m.ttft), slices.Clone(m.tps)
	m.append(rows, 4)
	if !slices.Equal(first.ttft, beforeWait) || !slices.Equal(first.tps, beforeSpeed) {
		t.Fatal("append mutated a previously published order")
	}
	if len(m.ttft) != 5 || len(m.tps) != 4 {
		t.Fatalf("invalid sample accepted: ttft=%v tps=%v", m.ttft, m.tps)
	}
	check := func(n int, row uint32, value float64, expected float64) {
		t.Helper()
		if int(row) >= len(rows) || value != expected {
			t.Fatalf("sample %d lost row/value association: row=%d value=%v want=%v", n, row, value, expected)
		}
	}
	for i, sample := range m.ttft {
		check(i, sample.row, float64(sample.value), float64(rows[sample.row].ttft))
		if i > 0 && m.ttft[i-1].value > sample.value {
			t.Fatal("TTFT merge is not sorted")
		}
	}
	for i, sample := range m.tps {
		check(i, sample.row, sample.value, rows[sample.row].tps)
		if i > 0 && m.tps[i-1].value > sample.value {
			t.Fatal("TPS merge is not sorted")
		}
	}
	previous := m
	m.append(rows, len(rows))
	if &m.ttft[0] != &previous.ttft[0] || &m.tps[0] != &previous.tps[0] {
		t.Fatal("empty catch-up needlessly copied orders")
	}
	mask, counts := []uint8{1, 1, 0, 0, 1, 1, 1, 0}, []int{5}
	buckets, period := rankPercentiles(m.ttft, mask, counts, nil, nil)
	want := sortedSampleOracle([]int64{10, 10, 20, 30, 40}, 50, 95, 99)
	if !reflect.DeepEqual(period, want) || !reflect.DeepEqual(buckets[0], want) {
		t.Fatal("merged order produced incorrect exact percentiles")
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
	order = mergeMetricOrder(nil, order)
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
	order = mergeMetricOrder(nil, order)
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
	order = mergeMetricOrder(nil, order)
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
