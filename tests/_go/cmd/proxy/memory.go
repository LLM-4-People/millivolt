package main

import "testing"

// TestEffectiveMemoryLimit pins the startup precedence: explicit config wins,
// an operator GOMEMLIMIT is never overridden, and a finite cgroup ceiling
// becomes a bounded soft limit with headroom for non-Go memory.
func TestEffectiveMemoryLimit(t *testing.T) {
	cases := []struct {
		name       string
		configured int64
		env        string
		cgroup     int64
		has        bool
		want       int64
	}{
		{"config wins", 256 << 20, "1GiB", 1 << 30, true, 256 << 20},
		{"env wins over cgroup", 0, "512MiB", 1 << 30, true, 0},
		{"cgroup 80 percent", 0, "", 1 << 30, true, (1 << 30) * 8 / 10},
		{"no cgroup", 0, "", 0, false, 0},
		{"tiny cgroup ignored", 0, "", 32 << 20, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMemoryLimit(tc.configured, tc.env, tc.cgroup, tc.has); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
		})
	}
}
