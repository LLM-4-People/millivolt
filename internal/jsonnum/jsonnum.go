// Package jsonnum owns exact decoding of JSON number literals that must be
// integers. encoding/json rejects exponent and fraction spellings for Go int
// fields even when the value is mathematically integral, while the OpenAI and
// Anthropic schemas accept any JSON number whose value is an integer for count
// fields such as max_tokens. One owner decides what an integral JSON number is
// so the proxy trust boundary and every wire decoder agree.
package jsonnum

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	// ErrNotNumber reports a literal that is not a JSON number.
	ErrNotNumber = errors.New("must be an integer number")
	// ErrNotInteger reports a fractional value.
	ErrNotInteger = errors.New("must be an integer")
	// ErrOutOfRange reports an integral value that does not fit int.
	ErrOutOfRange = errors.New("is outside the supported integer range")
)

// exponentClamp bounds the decimal exponent accepted by decimalParts. Only a
// small scale can change the integral or overflow verdict for a body that fits
// the request limit, so a hostile exponent is clamped instead of being
// materialized as a huge integer.
const exponentClamp = 1 << 30

// Parse decodes one JSON number literal into an exact int. It accepts the full
// JSON number grammar and every spelling whose mathematical value is integral
// and fits int: 1000000, 1e6, 1000000.0 and 1E+6 are the same value. A
// fractional value, an overflow, or a non-number literal fails with one of the
// exported reasons.
func Parse(raw []byte) (int, error) {
	lit := strings.TrimSpace(string(raw))
	if lit == "" {
		return 0, ErrNotNumber
	}
	digits, scale, negative, ok := decimalParts(lit)
	if !ok {
		return 0, ErrNotNumber
	}
	sig := strings.TrimLeft(digits, "0")
	if sig == "" {
		// Every spelling of zero is integral, including 0.0 and 0e999.
		return 0, nil
	}
	switch {
	case scale < 0:
		drop := -scale
		if drop >= len(sig) {
			return 0, ErrNotInteger
		}
		for _, c := range sig[len(sig)-drop:] {
			if c != '0' {
				return 0, ErrNotInteger
			}
		}
		sig = sig[:len(sig)-drop]
	case scale > 0:
		if len(sig)+scale > 19 {
			return 0, ErrOutOfRange
		}
		sig += strings.Repeat("0", scale)
	}
	n, err := strconv.ParseInt(sig, 10, 0)
	if err != nil {
		return 0, ErrOutOfRange
	}
	if negative {
		n = -n
	}
	return int(n), nil
}

// OptionalInt decodes one optional integer member. A nil or JSON null raw
// member means absent and returns nil. A present member is parsed exactly and
// errors name the field, so a decoder can report which wire member failed.
func OptionalInt(raw json.RawMessage, field string) (*int, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}
	n, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %s", field, err)
	}
	return &n, nil
}

// decimalParts splits one JSON number literal into its digits and a base-10
// scale, so the exact value is digits * 10^scale. It accepts the full JSON
// number grammar and rejects anything else.
func decimalParts(lit string) (digits string, scale int, negative, ok bool) {
	if lit == "" {
		return "", 0, false, false
	}
	i := 0
	if lit[i] == '-' {
		negative = true
		i++
	}
	start := i
	for i < len(lit) && lit[i] >= '0' && lit[i] <= '9' {
		i++
	}
	if i == start {
		return "", 0, false, false
	}
	digits = lit[start:i]
	if i < len(lit) && lit[i] == '.' {
		i++
		fracStart := i
		for i < len(lit) && lit[i] >= '0' && lit[i] <= '9' {
			i++
		}
		if i == fracStart {
			return "", 0, false, false
		}
		digits += lit[fracStart:i]
		scale = -(i - fracStart)
	}
	if i < len(lit) && (lit[i] == 'e' || lit[i] == 'E') {
		i++
		expNegative := false
		if i < len(lit) && (lit[i] == '+' || lit[i] == '-') {
			expNegative = lit[i] == '-'
			i++
		}
		expStart := i
		for i < len(lit) && lit[i] >= '0' && lit[i] <= '9' {
			i++
		}
		if i == expStart {
			return "", 0, false, false
		}
		exp, err := strconv.Atoi(lit[expStart:i])
		if err != nil {
			// The exponent exceeds an int; either direction is decisive for
			// a non-zero significand, so clamp rather than materialize it.
			exp = exponentClamp
		}
		if exp > exponentClamp {
			exp = exponentClamp
		}
		if exp < -exponentClamp {
			exp = -exponentClamp
		}
		if expNegative {
			exp = -exp
		}
		scale += exp
	}
	if i != len(lit) {
		return "", 0, false, false
	}
	return digits, scale, negative, true
}
