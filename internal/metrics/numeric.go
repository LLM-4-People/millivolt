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

// sumCount and addCount are the two halves of the one checked-count owner.
// addCount is the fold half: it admits the CONTRIBUTION (negative is
// unavailable data) and rejects an overflowing step. An accumulator fed only
// by addCount starts at zero and stays non-negative by induction, and an
// overflow of two non-negative int64 values always wraps negative, so the sign
// of the result is a complete overflow test - the accumulator's own
// admissibility is not re-derived per contribution.
func addCount(sum, value int64) (int64, error) {
	if value < 0 {
		return 0, ErrMetricRange
	}
	next := sum + value
	if next < 0 {
		return 0, ErrMetricRange
	}
	return next, nil
}

// sumCount is the boundary half: it re-validates the running value too, for
// callers holding a total this package did not accumulate (a stored total, a
// decoded column, an adapter's own base count).
func sumCount(sum, value int64) (int64, error) {
	if sum < 0 {
		return 0, ErrMetricRange
	}
	return addCount(sum, value)
}

// SumCounts rejects negative or overflowing counts rather than publishing a
// wrapped total. It is shared by usage adapters and aggregate accounting. Hot
// folds use AddCount, which folds the same checked arithmetic into an
// accumulator that is non-negative by induction.
func SumCounts(values ...int64) (int64, error) {
	var sum int64
	for _, value := range values {
		next, err := sumCount(sum, value)
		if err != nil {
			return 0, err
		}
		sum = next
	}
	return sum, nil
}

// AddCount folds one count contribution into an accumulator that is a checked
// sum of admitted contributions (zero or positive). It rejects a negative
// contribution and an overflowing step, setting the cell to 0 so no caller can
// read a partial or wrapped value. A contribution is validated here, at the
// trust boundary, so the fold never re-validates its own accumulator.
func AddCount(dst *int64, value int64) error {
	next, err := addCount(*dst, value)
	if err != nil {
		*dst = 0
		return err
	}
	*dst = next
	return nil
}

// addValue folds one finite non-negative cost or rate contribution into an
// accumulator. The negated comparison admits NaN, both infinities and every
// negative magnitude in one test, and the sum of non-negative finite values can
// only leave the finite range upward, so the result check is the complete
// overflow test.
func addValue(sum, value float64) (float64, error) {
	if !(value >= 0 && value <= math.MaxFloat64) {
		return 0, ErrMetricRange
	}
	next := sum + value
	if !(next <= math.MaxFloat64) {
		return 0, ErrMetricRange
	}
	return next, nil
}

// sumValue is addValue's boundary half: it re-validates the running value too.
func sumValue(sum, value float64) (float64, error) {
	if !(sum >= 0 && sum <= math.MaxFloat64) {
		return 0, ErrMetricRange
	}
	return addValue(sum, value)
}

// AddValue is AddCount's finite counterpart for costs and rates. The
// accumulator contract is identical: contributions are validated per record at
// the fold, overflow is checked exactly, and the accumulator's own validity is
// established by induction rather than re-derived on every row.
func AddValue(dst *float64, value float64) error {
	next, err := addValue(*dst, value)
	if err != nil {
		*dst = 0
		return err
	}
	*dst = next
	return nil
}

// SumValues is the finite, nonnegative counterpart for costs and rates. It
// re-validates every input, so it is the boundary owner; hot folds use
// AddValue's accumulator contract instead.
func SumValues(values ...float64) (float64, error) {
	var sum float64
	for _, value := range values {
		next, err := sumValue(sum, value)
		if err != nil {
			return 0, err
		}
		sum = next
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
