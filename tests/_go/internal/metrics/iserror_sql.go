package metrics

// metrics.IsErrorSQL is the published SQL twin of Record.IsError: the MCP
// describe document and the durable purge filter render it, so an operator's
// filter must delete exactly the rows the dashboard counts as errors. The two
// are written in different languages and nothing else executes both against
// the same row, so this file pins them row-for-row on a real SQLite database.
// The same shape as the web suite's percentile contract: a Go/SQL pair that
// could drift silently gets one test that fails when either side moves.

import (
	"database/sql"
	"encoding/json"
	"testing"

	_ "modernc.org/sqlite"
)

// isErrorSQLRow is one fixture stored in both representations: the Go record
// the dashboard predicates read, and the durable columns the SQL predicate
// reads. wantErr is the contract both sides must answer; assertAgrees also
// re-derives it from Record.IsError so neither predicate can drift while the
// table stays green.
type isErrorSQLRow struct {
	name    string
	status  int
	errType string
	errCode string
	attempt []RetryAttempt
	wantErr bool
}

var isErrorSQLRows = []isErrorSQLRow{
	{
		// The 429 arm's absorbed-failure clause: the final status is flow
		// control, but an earlier attempt saw a genuine upstream 5xx.
		name: "429 after an absorbed 5xx is an error", status: 429,
		errType: "rate_limit_error", errCode: "rate_limit_exceeded",
		attempt: []RetryAttempt{{StatusCode: 502}},
		wantErr: true,
	},
	{
		// A bare 429 is flow control, not a failure. HasRateLimit answers the
		// other question.
		name: "bare 429 is not an error", status: 429,
		errType: "rate_limit_error",
		wantErr: false,
	},
	{
		// A 499 is the local client's own abort, unless it carries a
		// structured error observed before the client left.
		name: "499 with a typed abort is an error", status: StatusClientClosedRequest,
		errType: "api_error", errCode: "internal_error",
		wantErr: true,
	},
	{
		name: "plain 500 is an error", status: 500,
		wantErr: true,
	},
	{
		// A recovered request still counts: the upstream failed even though a
		// later attempt succeeded, and an attempt entry carries no "current"
		// marker the predicate could use to forget it.
		name: "2xx with a stale 5xx attempt is an error", status: 200,
		attempt: []RetryAttempt{{StatusCode: 503}},
		wantErr: true,
	},
	{
		// The 500 boundary and its negative: an absorbed 429 is flow control,
		// never an absorbed server failure.
		name: "2xx after only an absorbed 429 is not an error", status: 200,
		attempt: []RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit_error"}},
		wantErr: false,
	},
}

// isErrorSQLDatabase creates the durable column shape the predicate is written
// against. Only the four referenced columns exist: an extra column here would
// suggest the predicate reads something it does not.
func isErrorSQLDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	// :memory: is per-connection; one connection keeps every statement on the
	// database the fixtures were inserted into.
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE requests (
		status_code INTEGER NOT NULL,
		error_type TEXT NOT NULL,
		error_code TEXT NOT NULL,
		attempts TEXT NOT NULL DEFAULT 'null'
	)`); err != nil {
		t.Fatalf("create requests table: %v", err)
	}
	return db
}

// attemptsColumn renders the attempts value exactly as the durable writer
// does: JSON null for an empty list, the marshaled slice otherwise.
func attemptsColumn(t *testing.T, attempts []RetryAttempt) string {
	t.Helper()
	if len(attempts) == 0 {
		return "null"
	}
	raw, err := json.Marshal(attempts)
	if err != nil {
		t.Fatalf("marshal attempts: %v", err)
	}
	return string(raw)
}

// TestIsErrorSQLMatchesRecordIsError executes the published predicate against a
// real database for every fixture and compares it with the Go predicate over
// the same outcome. A row that disagrees fails in both directions: the SQL arm
// names the predicate text it evaluated, so a hand-edited predicate is
// traceable from the failure.
func TestIsErrorSQLMatchesRecordIsError(t *testing.T) {
	query := "SELECT " + IsErrorSQL() + " FROM requests"
	for _, row := range isErrorSQLRows {
		t.Run(row.name, func(t *testing.T) {
			rec := Record{StatusCode: row.status, ErrorType: row.errType, ErrorCode: row.errCode, Attempts: row.attempt}
			goErr := rec.IsError()
			if goErr != row.wantErr {
				t.Fatalf("Record.IsError() = %v, want %v (the fixture and the Go predicate disagree)", goErr, row.wantErr)
			}

			db := isErrorSQLDatabase(t)
			if _, err := db.Exec(
				`INSERT INTO requests (status_code, error_type, error_code, attempts) VALUES (?, ?, ?, ?)`,
				row.status, row.errType, row.errCode, attemptsColumn(t, row.attempt),
			); err != nil {
				t.Fatalf("insert fixture: %v", err)
			}
			var sqlErr bool
			if err := db.QueryRow(query).Scan(&sqlErr); err != nil {
				t.Fatalf("evaluate IsErrorSQL: %v\npredicate: %s", err, IsErrorSQL())
			}
			if sqlErr != goErr {
				t.Fatalf("IsErrorSQL = %v but Record.IsError() = %v for (status %d, type %q, code %q, %d attempts)\npredicate: %s",
					sqlErr, goErr, row.status, row.errType, row.errCode, len(row.attempt), IsErrorSQL())
			}
		})
	}
}
