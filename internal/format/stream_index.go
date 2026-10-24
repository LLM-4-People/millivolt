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
