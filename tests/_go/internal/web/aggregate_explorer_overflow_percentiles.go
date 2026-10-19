package web

import (
	"fmt"
	"math"
	"slices"
	"testing"
)

// TestExplorerOverflowPercentilesUseOtherMembership pins the percentile
// membership of the synthetic overflow card. The card's N counts every
// excluded value, but its TTFT/TPS ranks come from the projection rows whose
// membership was written as the OTHER accumulator id (explorerOtherID), not
// the excluded dimension key. Writing dimKey there leaves the card's sample
// counts positive while the rank visitor selects no rows, so all four
// percentile fields disappear.
func TestExplorerOverflowPercentilesUseOtherMembership(t *testing.T) {
	const overflow = 100
	rows := make([]contrib, explorerGroupBudget+overflow)
	for i := range rows {
		rows[i] = contrib{
			id:     fmt.Sprintf("request-%05d", i),
			start:  int64(i),
			status: 200,
			labels: &contribLabels{client: fmt.Sprintf("client-%05d", i)},
			ttft:   int64(1_000 + i),
			tps:    10 + float64(i)*0.5,
		}
	}
	p := explorerProjection(rows)
	f := newExplorerFold("client", "", nil)
	f.prepare(p, nil)
	for i := range p.rows {
		if err := f.fold(&p.rows[i]); err != nil {
			t.Fatal(err)
		}
	}
	payload := f.payload()
	if len(payload.Groups) == 0 || payload.Groups[0].Name != explorerOtherName {
		t.Fatalf("first card = %+v, want the synthetic %q overflow card", payload.Groups, explorerOtherName)
	}
	card := payload.Groups[0]
	if card.N != overflow {
		t.Fatalf("overflow card N = %d, want %d excluded values", card.N, overflow)
	}

	var ttft []int64
	var tps []float64
	for i := explorerGroupBudget; i < len(p.rows); i++ {
		ttft = append(ttft, p.rows[i].ttft)
		tps = append(tps, p.rows[i].tps)
	}
	slices.Sort(ttft)
	slices.Sort(tps)
	wantTTFT := sortedSampleOracle(ttft, 50, 95)
	wantTPS := sortedSampleOracle(tps, 50, 95)
	check := func(field string, got, want *float64) {
		t.Helper()
		if got == nil {
			t.Fatalf("overflow card %s is nil; membership did not select the overflow rows", field)
		}
		if want == nil {
			t.Fatalf("oracle %s is nil for %d samples", field, overflow)
		}
		if math.Abs(*got-*want) > 1e-6 {
			t.Fatalf("overflow card %s = %v, want %v", field, *got, *want)
		}
	}
	check("ttft_p50", card.TTFTP50, wantTTFT[0])
	check("ttft_p95", card.TTFTP95, wantTTFT[1])
	check("tps_p50", card.TPSP50, wantTPS[0])
	check("tps_p95", card.TPSP95, wantTPS[1])
}
