package web

// Regression tests for the chart's window-relative membership index. A bounded
// window used to allocate one membership byte per retained history row; the
// fold now indexes from its first accepted row, so the list is the window's own
// span. These tests keep the offset path reading the same ranks as the absolute
// index it replaced.

import (
	"context"
	"testing"
)

// chartMembershipFixture builds n oldest-first rows one second apart with
// distinct metric values, so a wrong row translation shows in the ranks.
func chartMembershipFixture(n int, now int64) []contrib {
	rows := make([]contrib, n)
	for i := range rows {
		rows[i] = contrib{
			start:  now - int64(n-1-i)*1000,
			status: 200,
			in:     10,
			out:    5,
			ttft:   int64(1 + i),
			tps:    float64(1 + i),
			dec:    int64(1 + i),
			labels: &contribLabels{prov: "alpha"},
		}
	}
	return rows
}

// compareTriples fails on the first rank, statistic or presence difference.
func compareTriples(t *testing.T, name string, got, want []*float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s len = %d, want %d", name, len(got), len(want))
	}
	for i := range want {
		if (got[i] == nil) != (want[i] == nil) {
			t.Fatalf("%s[%d] = %v, want %v", name, i, got[i], want[i])
		}
		if got[i] != nil && *got[i] != *want[i] {
			t.Fatalf("%s[%d] = %v, want %v", name, i, *got[i], *want[i])
		}
	}
}

// TestChartWindowMembershipMatchesAbsoluteIndex pins that a bounded window's
// window-relative membership yields exactly the payload the absolute per-row
// index produced: bucket ranks, the period ranks and the period statistics. It
// also pins the anchored span, so a future change cannot quietly return to
// sizing the list by all retained history.
func TestChartWindowMembershipMatchesAbsoluteIndex(t *testing.T) {
	now := int64(1_700_000_000_000)
	const rows, windowRows = 4000, 500
	cs := chartMembershipFixture(rows, now)
	p := projectionFor(cs)
	since := now - int64(windowRows-1)*1000
	from, step, count := chartWindowEdges(now, since)

	fold := newChartFold(from, step, count, "", nil)
	fold.prepare(p, nil)
	if err := p.visitSince(context.Background(), since, func(c *contrib) error { return fold.fold(c) }); err != nil {
		t.Fatal(err)
	}
	fold.freezeOrder()
	got := fold.payload(now)

	// The anchored span: the window's rows, never all retained history.
	first := rows - windowRows
	if len(fold.membership) == 0 || len(fold.membership) > rows-first {
		t.Fatalf("membership len = %d, want 1..%d (the window span)", len(fold.membership), rows-first)
	}
	if int(fold.membershipBase) != first {
		t.Fatalf("membershipBase = %d, want the first in-window row %d", fold.membershipBase, first)
	}

	// Independent oracle: the absolute per-row mask the previous index used,
	// read by the same rank owner the chart calls.
	mask := make([]uint8, rows)
	counts := make([]int, count)
	for i := first; i < rows; i++ {
		idx := int((cs[i].start - from) / step)
		idx = min(max(idx, 0), count-1)
		mask[i] = uint8(idx + 1)
		counts[idx]++
	}
	order := p.metrics.allOrderSource()
	wantTTFT, wantPeriodTTFT, wantStatTTFT := rankPercentilesStats(order.base.ttft, mask, counts, nil, nil)
	wantTPS, wantPeriodTPS, wantStatTPS := rankPercentilesStats(order.base.tps, mask, counts, nil, nil)
	wantDecBuckets, wantPeriodDec := rankPercentilesVisit(order.base.dec, counts, nil, nil, func(row uint32, emit func(uint8)) {
		emit(mask[row])
	})

	for i := range got.Buckets {
		compareTriples(t, "bucket ttft", got.Buckets[i].TTFT, wantTTFT[i])
		compareTriples(t, "bucket tps", got.Buckets[i].TPS, wantTPS[i])
		compareTriples(t, "bucket dec", got.Buckets[i].Dec, wantDecBuckets[i])
	}
	compareTriples(t, "period ttft", got.TTFTP, wantPeriodTTFT)
	compareTriples(t, "period tps", got.TPSP, wantPeriodTPS)
	compareTriples(t, "period dec", got.DecP, wantPeriodDec)
	compareTriples(t, "ttft stat", got.TTFTStat, wantStatTTFT)
	compareTriples(t, "tps stat", got.TPSStat, wantStatTPS)
	if n := len(got.Buckets) - 1; got.Buckets[n].Req == 0 {
		t.Fatalf("last bucket req = 0, want the newest row")
	}
}

// TestChartAllWindowMembershipIsHistorySized keeps the All path on the
// historical allocation: with no block skip the anchor is row 0 and the list
// covers the retained history.
func TestChartAllWindowMembershipIsHistorySized(t *testing.T) {
	now := int64(1_700_000_000_000)
	cs := chartMembershipFixture(2000, now)
	p := projectionFor(cs)
	from, step, count := chartWindowEdges(now, cs[0].start)
	fold := newChartFold(from, step, count, "", nil)
	fold.prepare(p, nil)
	for i := range cs {
		if err := fold.fold(&cs[i]); err != nil {
			t.Fatal(err)
		}
	}
	if len(fold.membership) != len(cs) || fold.membershipBase != 0 {
		t.Fatalf("all-window membership = %d from %d, want %d from 0", len(fold.membership), fold.membershipBase, len(cs))
	}
	payload := fold.payload(now)
	if n := len(payload.Buckets) - 1; payload.Buckets[n].Req == 0 {
		t.Fatal("last bucket holds none of the retained rows")
	}
}
