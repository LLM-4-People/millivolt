package main

import (
	"os"
	"strconv"
	"strings"
)

// cgroupMemoryMax reads the container's cgroup v2 (or v1) memory ceiling.
// Docker with a memory limit exposes it at the cgroup root even when the
// process runs in a nested namespace; no /proc/self/cgroup traversal is
// needed for that common case. "max" and sentinel-sized values mean
// unlimited. Returns false when no finite limit is readable.
func cgroupMemoryMax() (int64, bool) {
	for _, path := range []string{
		"/sys/fs/cgroup/memory.max",
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		v := strings.TrimSpace(string(raw))
		if v == "" || v == "max" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		// v1 represents "unlimited" with a page-aligned near-MaxInt value.
		if n >= 1<<62 {
			continue
		}
		return n, true
	}
	return 0, false
}

// effectiveMemoryLimit is the startup memory policy owner. Explicit config
// wins (documented); otherwise an operator GOMEMLIMIT environment value is
// left untouched; otherwise a finite cgroup ceiling becomes a soft Go limit at
// 80% of the container's memory, leaving headroom for SQLite's page caches and
// non-Go process memory. 0 means leave the runtime unbounded.
func effectiveMemoryLimit(configured int64, env string, cgroup int64, hasCgroup bool) int64 {
	if configured > 0 {
		return configured
	}
	if strings.TrimSpace(env) != "" {
		return 0
	}
	if !hasCgroup || cgroup <= 0 {
		return 0
	}
	limit := cgroup * 8 / 10
	const minMemLimit = 64 << 20
	if limit < minMemLimit {
		return 0
	}
	return limit
}
