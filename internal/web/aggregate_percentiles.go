package web

import "math"

// percentilePosition is the shared zero-based R7 rank/interpolation owner.
// Chart buckets, explorer groups, and period samples use these exact positions.
func percentilePosition(n int, p float64) (lo, hi int, fraction float64, ok bool) {
	if n < pctMinSamples || p < 0 || p > 100 || math.IsNaN(p) {
		return 0, 0, 0, false
	}
	idx := (p / 100) * float64(n-1)
	lo, hi = int(math.Floor(idx)), int(math.Ceil(idx))
	return lo, hi, idx - float64(lo), true
}

func percentileInterpolate(lo, hi, fraction float64) float64 {
	if fraction == 0 {
		return lo
	}
	return lo + (hi-lo)*fraction
}
