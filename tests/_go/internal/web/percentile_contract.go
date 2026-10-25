package web

// Two percentile algorithms are published today, and they are intentionally
// different. This file pins both so neither can drift silently:
//
//   - /metrics/prometheus publishes metrics.Percentile: nearest-rank
//     (index = ceil(n*p/100)-1) over the ring's TTFT samples, with NO minimum
//     sample gate. One sample publishes a quantile.
//   - The dashboard chart/explorer cards and the MCP chart tool read the
//     server fold, which uses percentilePosition: exact R7 linear interpolation
//     between order statistics, gated by pctMinSamples = 4. Below four samples
//     the dashboard publishes null where Prometheus still publishes a number.
//
// For the same sorted samples [10 20 30 40]: p50 is 25 (R7) vs 20
// (nearest-rank), p95 is 38.5 vs 40, p99 is 39.7 vs 40. Neither surface may be
// "fixed" to match the other without an operator-visible contract change: the
// dashboard values are chart history (R7) and the Prometheus values are a
// published metric. Do not unify them; pin them.

import (
	"math"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// dashboardPercentile is the R7 read used by the server fold: the shared
// percentilePosition rank plus the shared interpolation, with the minimum
// sample gate applied. ok=false is the wire's null.
func dashboardPercentile(sorted []int64, p float64) (float64, bool) {
	lo, hi, fraction, ok := percentilePosition(len(sorted), p)
	if !ok {
		return 0, false
	}
	return percentileInterpolate(float64(sorted[lo]), float64(sorted[hi]), fraction), true
}

func TestPercentileContractPrometheusNearestRankVsDashboardR7(t *testing.T) {
	// The gate itself is a published contract: four samples is the dashboard's
	// minimum, not an incidental constant.
	if pctMinSamples != 4 {
		t.Fatalf("pctMinSamples = %d, want the published dashboard minimum of 4", pctMinSamples)
	}

	sorted := []int64{10, 20, 30, 40}
	cases := []struct {
		p          float64
		dashboard  float64
		prometheus float64
	}{
		{50, 25, 20},
		{95, 38.5, 40},
		{99, 39.7, 40},
	}
	for _, tc := range cases {
		dash, ok := dashboardPercentile(sorted, tc.p)
		if !ok {
			t.Fatalf("p%v: dashboard withheld a percentile at the minimum sample count", tc.p)
		}
		// R7 interpolates in float64, so p99 lands on the nearest representable
		// value of 39.7; the published number is that float, not a rounded one.
		if math.Abs(dash-tc.dashboard) > 1e-9 {
			t.Errorf("p%v dashboard R7 = %v, want %v", tc.p, dash, tc.dashboard)
		}
		if got := metrics.Percentile(sorted, tc.p); got != tc.prometheus {
			t.Errorf("p%v Prometheus nearest-rank = %v, want %v", tc.p, got, tc.prometheus)
		}
	}

	// Below the gate the dashboard publishes null while Prometheus keeps
	// publishing the nearest-rank sample: this is the visible difference a
	// single-request deployment sees when comparing Grafana with the dashboard.
	sparse := []int64{7, 50, 90}
	for _, p := range []float64{50, 95, 99} {
		if _, ok := dashboardPercentile(sparse, p); ok {
			t.Fatalf("p%v: dashboard published a percentile below pctMinSamples", p)
		}
		if got := metrics.Percentile(sparse, p); got == 0 {
			t.Fatalf("p%v: Prometheus has no minimum-sample gate, got the zero value", p)
		}
	}
	if got, want := metrics.Percentile([]int64{42}, 50), float64(42); got != want {
		t.Errorf("single-sample Prometheus p50 = %v, want %v", got, want)
	}
	if got := metrics.Percentile(nil, 50); got != 0 {
		t.Errorf("empty Prometheus p50 = %v, want the zero value", got)
	}
	if _, _, _, ok := percentilePosition(0, 50); ok {
		t.Error("R7 published a position for an empty sample set")
	}

	// The Prometheus exposition is the surface that publishes the nearest-rank
	// number; one sample is enough there under the same no-gate contract.
	if got := metrics.Percentile([]int64{1234}, 99); got != 1234 {
		t.Errorf("Prometheus single-sample p99 = %v, want 1234 (nearest-rank, no gate)", got)
	}
}
