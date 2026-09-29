package mcp

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestHealthPredicatesMatchTheRecordTruthTable is the parity guard for the most
// consequential unguarded fact this server publishes. describe hands the model
// the exact SQL health predicates, and the same health meaning is implemented
// by metrics.Record.IsError and HasRateLimit. If the SQL and the Go methods
// drift, a model's "errors" filter and the dashboard's own error count answer
// differently about the same history, and neither side is obviously wrong.
//
// The truth table runs every combination of final status, structured error type
// and absorbed-attempt set through both sides on a real SQLite database. It is
// the same shape as storage's TestPurgeFilterMatchRecordParity.
func TestHealthPredicatesMatchTheRecordTruthTable(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE requests (
		id TEXT PRIMARY KEY,
		status_code INTEGER NOT NULL,
		error_type TEXT NOT NULL,
		attempts TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	insert, err := db.Prepare(`INSERT INTO requests(id, status_code, error_type, attempts) VALUES (?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	defer insert.Close()

	statuses := []int{0, 199, 200, 302, 400, 429, 499, 500, 502}
	errorTypes := []string{"", "upstream_timeout"}
	attemptSets := map[string][]metrics.RetryAttempt{
		"none":         nil,
		"attempt429":   {{StatusCode: 429}},
		"attempt500":   {{StatusCode: 500}},
		"attempt200":   {{StatusCode: 200}},
		"retried5xxOn": {{StatusCode: 500}, {StatusCode: 200}},
	}
	isErrorSQL := "SELECT (" + predicates.IsError + ") = 1 FROM requests WHERE id = ?"
	rateLimitedSQL := "SELECT (" + predicates.RateLimited + ") = 1 FROM requests WHERE id = ?"

	for _, status := range statuses {
		for _, errorType := range errorTypes {
			for name, attempts := range attemptSets {
				encoded, err := json.Marshal(attempts)
				if err != nil {
					t.Fatal(err)
				}
				record := &metrics.Record{StatusCode: status, ErrorType: errorType, Attempts: attempts}
				id := strconv.Itoa(status) + "/" + errorType + "/" + name
				if _, err := insert.Exec(id, status, errorType, string(encoded)); err != nil {
					t.Fatal(err)
				}
				var gotError, gotLimited int
				if err := db.QueryRow(isErrorSQL, id).Scan(&gotError); err != nil {
					t.Fatalf("is_error SQL for %s: %v", id, err)
				}
				if err := db.QueryRow(rateLimitedSQL, id).Scan(&gotLimited); err != nil {
					t.Fatalf("rate_limited SQL for %s: %v", id, err)
				}
				if want := record.IsError(); (gotError == 1) != want {
					t.Errorf("is_error(%s) = %v, metrics.Record.IsError = %v", id, gotError == 1, want)
				}
				if want := record.HasRateLimit(); (gotLimited == 1) != want {
					t.Errorf("rate_limited(%s) = %v, metrics.Record.HasRateLimit = %v", id, gotLimited == 1, want)
				}
			}
		}
	}
}
