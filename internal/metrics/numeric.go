package metrics

import (
	"errors"
	"math"
)

var ErrMetricRange = errors.New("metric value is outside its representable range")

// CodeInvalidUsage marks provider accounting that cannot be represented. It
// is unavailable data, never a zero-token response or a saturated count.
const CodeInvalidUsage = "invalid_usage"

// IEEE-754 normal/subnormal boundary, not a configurable metric floor.
const smallestNormalFloat = 0x1p-1022

// SumCounts rejects negative or overflowing counts rather than publishing a
// wrapped total. It is shared by usage adapters and aggregate accounting.
func SumCounts(values ...int64) (int64, error) {
	var sum int64
	for _, value := range values {
		if value < 0 || sum > math.MaxInt64-value {
			return 0, ErrMetricRange
		}
		sum += value
	}
	return sum, nil
}

// SumValues is the finite, nonnegative counterpart for costs and rates.
func SumValues(values ...float64) (float64, error) {
	var sum float64
	for _, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, ErrMetricRange
		}
		sum += value
		if math.IsInf(sum, 0) {
			return 0, ErrMetricRange
		}
	}
	return sum, nil
}

// ScaledRatio returns an unavailable value when the denominator is absent or
// the result cannot be represented. Exponent decomposition rescues extreme
// intermediate overflow/underflow; neither invented zero nor saturation is a ratio.
func ScaledRatio(numerator, denominator, scale float64) *float64 {
	if numerator < 0 || denominator <= 0 || scale <= 0 ||
		math.IsNaN(numerator) || math.IsNaN(denominator) || math.IsNaN(scale) ||
		math.IsInf(numerator, 0) || math.IsInf(denominator, 0) || math.IsInf(scale, 0) {
		return nil
	}
	quotient := numerator / denominator
	v := quotient * scale
	if numerator > 0 && (quotient < smallestNormalFloat || math.IsInf(v, 0)) {
		n, ne := math.Frexp(numerator)
		d, de := math.Frexp(denominator)
		s, se := math.Frexp(scale)
		v = math.Ldexp(n/d*s, ne-de+se)
	}
	if math.IsInf(v, 0) || (numerator > 0 && v == 0) {
		return nil
	}
	return &v
}
