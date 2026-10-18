package storage

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// queryBenchStatement is the 5,000-row endpoint fixture: an id, a short label
// and a 300-byte payload, about 340 encoded bytes per row, well inside the
// default query caps. It is a SELECT-leading statement so it passes
// validateQuery exactly like a client query.
func queryBenchStatement() string {
	return `SELECT n AS id, printf('row-%05d', n) AS label, substr('` +
		strings.Repeat("x", 300) +
		`', 1, 300) AS payload FROM (WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n<5000) SELECT n FROM c)`
}

// BenchmarkQueryEndpoint measures the bounded SELECT endpoint end to end:
// statement execution, per-row cap accounting and the response encoding. It
// also pins the fixture shape (row count and encoded row size) so the
// before/after cost comparison is over the same payload.
func BenchmarkQueryEndpoint(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "query-bench.db"), testOpts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Error(err)
		}
	})
	statement := queryBenchStatement()
	rows, err := s.Query(b.Context(), statement)
	if err != nil {
		b.Fatal(err)
	}
	if len(rows) != 5000 {
		b.Fatalf("fixture rows=%d, want 5000", len(rows))
	}
	rowJSON, err := json.Marshal(rows[0])
	if err != nil {
		b.Fatal(err)
	}
	want, err := json.Marshal(rows)
	if err != nil {
		b.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/metrics/query", nil)
	req.URL.RawQuery = url.Values{"q": {statement}}.Encode()

	b.ReportAllocs()
	b.ResetTimer()
	b.ReportMetric(float64(len(rowJSON)), "row-bytes")
	b.ReportMetric(5000, "rows")
	for range b.N {
		rec := httptest.NewRecorder()
		s.HandleQuery(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec.Body.Len() != len(want) {
			b.Fatalf("response bytes=%d, want %d", rec.Body.Len(), len(want))
		}
	}
}
