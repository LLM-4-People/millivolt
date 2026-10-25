package storage

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// queryEndpointProbeStatement is a 500-row version of the endpoint benchmark
// fixture: an id, a short label and a 300-byte payload, so a per-row
// regression shows up as roughly 500x its own cost without paying the 5,000-row
// fixture in the race-enabled gate.
func queryEndpointProbeStatement() string {
	return `SELECT n AS id, printf('row-%05d', n) AS label, substr('` +
		strings.Repeat("x", 300) +
		`', 1, 300) AS payload FROM (WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<500) SELECT n FROM c)`
}

// TestQueryEndpointAllocationBounded is the WS10 budget probe. The endpoint
// encoded every row twice: once into a map and once into JSON. Streaming the
// row into the response buffer removed the map layer. Measured post-change
// plain (500 rows, 300-byte payload): 3,836 allocs/op and about 1,071,370 B/op.
// Each plain ceiling adds just over 20%: 4,604 allocs and 1,286,000 bytes.
// Under -race the detector's shadow memory and instrumentation inflate both
// counts (about 5,158 allocs/op), so race builds use 6,400 allocs and 2 MiB.
// Both ceilings still reject a regression that re-materializes the map layer,
// which roughly doubles the alloc count.
func TestQueryEndpointAllocationBounded(t *testing.T) {
	budgetAllocs, budgetBytes := int64(4604), int64(1_286_000)
	if raceInstrumented {
		budgetAllocs, budgetBytes = 6400, 2<<20
	}
	s, err := Open(filepath.Join(t.TempDir(), "query-probe.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	statement := queryEndpointProbeStatement()
	req := httptest.NewRequest(http.MethodGet, "/metrics/query", nil)
	req.URL.RawQuery = url.Values{"q": {statement}}.Encode()
	// Warm every lazily created buffer and the encoder's pooled state before
	// the measured run.
	first := httptest.NewRecorder()
	s.HandleQuery(first, req)
	if first.Code != http.StatusOK {
		t.Fatalf("warm-up status=%d body=%s", first.Code, first.Body.String())
	}
	if got := strings.Count(first.Body.String(), `"id":`); got != 500 {
		t.Fatalf("fixture returned %d rows, want 500", got)
	}
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			rec := httptest.NewRecorder()
			s.HandleQuery(rec, req)
			if rec.Code != http.StatusOK {
				b.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		}
	})
	allocs, bytes := res.AllocsPerOp(), res.AllocedBytesPerOp()
	if allocs > budgetAllocs {
		t.Fatalf("Query endpoint allocates %d allocs/op for 500 rows, budget %d", allocs, budgetAllocs)
	}
	if bytes > budgetBytes {
		t.Fatalf("Query endpoint allocates %d B/op for 500 rows, budget %d", bytes, budgetBytes)
	}
	t.Logf("Query endpoint: %d allocs/op %d B/op for 500 rows", allocs, bytes)
}
