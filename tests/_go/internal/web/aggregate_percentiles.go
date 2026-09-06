package web

import (
	"math"
	"reflect"
	"testing"
)

// sortedSampleOracle deliberately retains the independent fully sorted R7
// formula. Metric-order tests compare every selected rank against this oracle.
func sortedSampleOracle[T ~int64 | ~float64](samples []T, ps ...float64) []*float64 {
	out := make([]*float64, len(ps))
	if len(samples) < pctMinSamples {
		return out
	}
	for i, p := range ps {
		if p < 0 || p > 100 || math.IsNaN(p) {
			continue
		}
		idx := (p / 100) * float64(len(samples)-1)
		lo, hi := int(math.Floor(idx)), int(math.Ceil(idx))
		v := float64(samples[lo])
		if lo != hi {
			v += (float64(samples[hi]) - v) * (idx - float64(lo))
		}
		out[i] = &v
	}
	return out
}

func TestPercentilePositionBoundaries(t *testing.T) {
	ps := []float64{99, 0, 50, 95, 100, 50, 1, 12.345, -1, 101, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, n := range []int{-1, 0, 1, 2, 3, 4, 5, 6, 99, 100, 100_000} {
		for _, p := range ps {
			lo, hi, fraction, ok := percentilePosition(n, p)
			wantOK := n >= pctMinSamples && p >= 0 && p <= 100
			if ok != wantOK {
				t.Fatalf("n=%d p=%v valid=%v, want %v", n, p, ok, wantOK)
			}
			if !ok {
				continue
			}
			idx := (p / 100) * float64(n-1)
			if lo != int(math.Floor(idx)) || hi != int(math.Ceil(idx)) || fraction != idx-float64(lo) {
				t.Fatalf("n=%d p=%v position=(%d,%d,%v), index=%v", n, p, lo, hi, fraction, idx)
			}
		}
	}
}

func TestPercentileInterpolationBoundaries(t *testing.T) {
	for _, tc := range []struct{ lo, hi, fraction, want float64 }{
		{10, 20, 0, 10}, {10, 20, 1, 20}, {10, 20, .5, 15},
		{3, 4, .8499999999999996, 3.8499999999999996},
		{1e-200, 1e200, .5, 5e199},
		{float64(math.MaxInt64 - 1), float64(math.MaxInt64), .5, float64(math.MaxInt64)},
	} {
		if got := percentileInterpolate(tc.lo, tc.hi, tc.fraction); got != tc.want {
			t.Fatalf("interpolate(%v,%v,%v)=%v, want %v", tc.lo, tc.hi, tc.fraction, got, tc.want)
		}
	}
}

func TestRankPercentilesSharedConsumers(t *testing.T) {
	for _, n := range []int{0, 3, 4, 5, 20} {
		order, mask := make([]metricSample[float64], n), make([]uint8, n)
		for i := range order {
			order[i], mask[i] = metricSample[float64]{row: uint32(i), value: float64(i+1) / 10}, 1
		}
		chart, chartPeriod := rankPercentiles(order, mask, []int{n}, nil, nil)
		explorer, explorerPeriod := rankPercentilesVisit(order, []int{n}, nil, nil, func(row uint32, emit func(uint8)) {
			emit(mask[row])
		})
		if !reflect.DeepEqual(chart, explorer) || !reflect.DeepEqual(chartPeriod, explorerPeriod) {
			t.Fatalf("n=%d chart membership and explorer visitor disagree", n)
		}
	}
}
