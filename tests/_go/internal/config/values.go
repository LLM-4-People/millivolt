package config

import (
	"math"
	"strconv"
	"testing"
)

// TestAsIntGuardsPlatformWidth pins the int conversion at the platform
// boundary. int is 32 bits on some supported targets; before the guard, an
// accepted int64 beyond that range wrapped and silently changed a configured
// limit. The 32-bit rejection branch only runs on a 32-bit target, while both
// widths assert the int32 boundaries stay exact and 64-bit targets keep the
// full int64 range.
func TestAsIntGuardsPlatformWidth(t *testing.T) {
	for _, value := range []int64{math.MinInt32, math.MaxInt32} {
		got, err := asInt(value)
		if err != nil {
			t.Fatalf("asInt(%d) on %d-bit int: %v", value, strconv.IntSize, err)
		}
		if int64(got) != value {
			t.Fatalf("asInt(%d) on %d-bit int = %d", value, strconv.IntSize, got)
		}
	}
	if strconv.IntSize == 32 {
		for _, value := range []int64{math.MinInt32 - 1, math.MaxInt32 + 1} {
			if got, err := asInt(value); err == nil {
				t.Fatalf("asInt(%d) on 32-bit int = %d, want a range rejection", value, got)
			}
		}
		return
	}
	for _, value := range []int64{math.MinInt32 - 1, math.MaxInt32 + 1, math.MinInt64, math.MaxInt64} {
		got, err := asInt(value)
		if err != nil {
			t.Fatalf("asInt(%d) on 64-bit int: %v", value, err)
		}
		if int64(got) != value {
			t.Fatalf("asInt(%d) on 64-bit int = %d", value, got)
		}
	}
}
