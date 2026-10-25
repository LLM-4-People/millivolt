package format

import (
	"fmt"

	"github.com/LLM-4-People/millivolt/internal/jsonnum"
)

// intFromJSONIndex narrows one exact wire index to the platform int. The round
// trip check makes a 32-bit build fail closed instead of aliasing a large
// provider index onto an existing map key; 64-bit builds accept every int64.
func intFromJSONIndex(field string, value jsonnum.Int64) (int, error) {
	n := int(value)
	if int64(n) != int64(value) {
		return 0, fmt.Errorf("%s %d is out of range", field, value)
	}
	return n, nil
}

// nonNegativeStreamIndex narrows one exact stream position and rejects a
// negative value. Positions address announced items and parts; a negative
// index has no announced entry, so accepting it emits bytes on a channel the
// part-coverage owner never records, and the terminal copy repeats them. Not a
// global replacement for intFromJSONIndex: citation spans narrow negatives and
// drop them with a warning instead.
func nonNegativeStreamIndex(field string, value jsonnum.Int64) (int, error) {
	if value < 0 {
		return 0, fmt.Errorf("%s %d is negative", field, value)
	}
	return intFromJSONIndex(field, value)
}
