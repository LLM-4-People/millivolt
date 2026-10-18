package web

import (
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
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
		{ttft: 40, tps: 4, dec: 400}, {ttft: 10, tps: 1, dec: 100}, {ttft: 0, tps: math.NaN(), dec: 0},
		{ttft: -3, tps: math.Inf(1), dec: -3}, {ttft: 30, tps: 3, dec: 0}, {ttft: 20, tps: 2, dec: 200},
		{ttft: 10, tps: 0, dec: 10}, {ttft: 0, tps: math.Inf(-1), dec: 1000},
	}
	var m projectionMetrics
	m.append(rows[:4], 0)
	first := m
	beforeWait, beforeSpeed, beforeDec := slices.Clone(m.all.ttft), slices.Clone(m.all.tps), slices.Clone(m.all.dec)
	m.append(rows, 4)
	if !slices.Equal(first.all.ttft, beforeWait) || !slices.Equal(first.all.tps, beforeSpeed) || !slices.Equal(first.all.dec, beforeDec) {
		t.Fatal("append mutated a previously published order")
	}
	if len(m.all.ttft) != 5 || len(m.all.tps) != 4 || len(m.all.dec) != 5 {
		t.Fatalf("invalid sample accepted: ttft=%v tps=%v dec=%v", m.all.ttft, m.all.tps, m.all.dec)
	}
	check := func(n int, row uint32, value float64, expected float64) {
		t.Helper()
		if int(row) >= len(rows) || value != expected {
			t.Fatalf("sample %d lost row/value association: row=%d value=%v want=%v", n, row, value, expected)
		}
	}
	for i, sample := range m.all.ttft {
		check(i, sample.row, float64(sample.value), float64(rows[sample.row].ttft))
		if i > 0 && m.all.ttft[i-1].value > sample.value {
			t.Fatal("TTFT merge is not sorted")
		}
	}
	for i, sample := range m.all.tps {
		check(i, sample.row, sample.value, rows[sample.row].tps)
		if i > 0 && m.all.tps[i-1].value > sample.value {
			t.Fatal("TPS merge is not sorted")
		}
	}
	for i, sample := range m.all.dec {
		check(i, sample.row, float64(sample.value), float64(rows[sample.row].dec))
		if i > 0 && m.all.dec[i-1].value > sample.value {
			t.Fatal("decode merge is not sorted")
		}
	}
	previous := m
	m.append(rows, len(rows))
	if &m.all.ttft[0] != &previous.all.ttft[0] || &m.all.tps[0] != &previous.all.tps[0] || &m.all.dec[0] != &previous.all.dec[0] {
		t.Fatal("empty catch-up needlessly copied orders")
	}
	mask, counts := []uint8{1, 1, 0, 0, 1, 1, 1, 0}, []int{5}
	buckets, period := rankPercentiles(m.all.ttft, mask, counts, nil, nil)
	want := sortedSampleOracle([]int64{10, 10, 20, 30, 40}, 50, 95, 99)
	if !reflect.DeepEqual(period, want) || !reflect.DeepEqual(buckets[0], want) {
		t.Fatal("merged order produced incorrect exact percentiles")
	}
	// The decode order ranks its own sample population: rows 0,1,5,6 pass
	// both the validity gate and the mask (row 4's zero window is invalid,
	// row 7's 1000ms window is masked out), so counts differ from ttft's.
	decBuckets, decPeriod := rankPercentiles(m.all.dec, mask, []int{4}, nil, nil)
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
		allTTFT, allPeriodTTFT, allStatTTFT := rankPercentilesStats(m.all.ttft, mask, counts, nil, nil)
		recentTTFT, recentPeriodTTFT, recentStatTTFT := rankPercentilesStats(m.recent.ttft, mask, counts, nil, nil)
		if !reflect.DeepEqual(allTTFT, recentTTFT) || !reflect.DeepEqual(allPeriodTTFT, recentPeriodTTFT) ||
			!reflect.DeepEqual(allStatTTFT, recentStatTTFT) {
			t.Fatalf("TTFT tail ranks differ from all-history ranks (from=%d)", from)
		}
		allTPS, allPeriodTPS, allStatTPS := rankPercentilesStats(m.all.tps, mask, counts, nil, nil)
		recentTPS, recentPeriodTPS, recentStatTPS := rankPercentilesStats(m.recent.tps, mask, counts, nil, nil)
		if !reflect.DeepEqual(allTPS, recentTPS) || !reflect.DeepEqual(allPeriodTPS, recentPeriodTPS) ||
			!reflect.DeepEqual(allStatTPS, recentStatTPS) {
			t.Fatalf("TPS tail ranks differ from all-history ranks (from=%d)", from)
		}
		allDec, allPeriodDec := rankPercentiles(m.all.dec, mask, counts, nil, nil)
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
		return f.payload(now)
	}
	fast, slow := fold(false), fold(true)
	if !reflect.DeepEqual(fast, slow) {
		t.Fatal("tail-selected chart payload differs from all-history payload")
	}
}
