package mcp

// Round 15 R41-1 pins: the in-band 429 attempt entry is one exception shared
// by the public protocol doc, the operator metric and retry SQL, the
// model-facing column reference, and the metrics type comment. The retry SQL
// is executed as documented so a filter change cannot silently turn a health
// entry into an apparent retry.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func repoText(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(raw)), " ")
}

func TestInBandAttemptContractDocs(t *testing.T) {
	protocol := repoText(t, "docs/protocol.md")
	for _, want := range []string{
		"Every retried or final attempt stores the same upstream-response information",
		"An in-band 429 is the one exception",
		"It carries no retry or error detail",
		"excluded from retry disclosures and error enumerations",
		"A final, retried or in-band 429 is a separate affected-request signal",
	} {
		if !strings.Contains(protocol, want) {
			t.Errorf("docs/protocol.md is missing the in-band attempt contract %q", want)
		}
	}

	operations := repoText(t, "docs/operations.md")
	for _, want := range []string{
		"HTTP 429 counts requests that encountered a final, retried or in-band 429",
		"AND COALESCE(json_extract(j.value,'$.in_band'),0)=0",
		"One row per absorbed retry, oldest first",
		"that entry is health evidence, not a retry",
	} {
		if !strings.Contains(operations, want) {
			t.Errorf("docs/operations.md is missing the in-band attempt contract %q", want)
		}
	}

	meaning := ""
	for _, table := range tableDocs {
		if table.Table != "requests" {
			continue
		}
		for _, column := range table.Columns {
			if column.Name == "attempts" {
				meaning = column.Meaning
			}
		}
	}
	for _, want := range []string{
		"absorbed retries plus at most one in-band 429",
		"in_band marks a 429 a committed 200 hid",
		"health evidence, not a retry",
	} {
		if !strings.Contains(meaning, want) {
			t.Errorf("describe.go attempts column is missing %q: %q", want, meaning)
		}
	}

	metricsSrc, err := os.ReadFile(filepath.Join("..", "metrics", "metrics.go"))
	if err != nil {
		t.Fatal(err)
	}
	var prose strings.Builder
	for _, line := range strings.Split(string(metricsSrc), "\n") {
		prose.WriteString(strings.TrimPrefix(strings.TrimSpace(line), "//"))
		prose.WriteByte(' ')
	}
	metricsText := strings.Join(strings.Fields(prose.String()), " ")
	for _, want := range []string{
		"every retried or final attempt stores the same information",
		"An in-band 429 entry is the exception",
		"it carries only the hidden status and time, no metadata",
	} {
		if !strings.Contains(metricsText, want) {
			t.Errorf("metrics.go RetryAttempt comment is missing %q", want)
		}
	}
}

// TestRetryQueryExcludesInBandAttempts runs the retry disclosure query exactly
// as published. The in-band 429 exists only for the health predicates and must
// not appear as an absorbed retry row.
func TestRetryQueryExcludesInBandAttempts(t *testing.T) {
	const marker = "**What retried before the client saw a response?**"
	doc := repoText(t, "docs/operations.md")
	at := strings.Index(doc, marker)
	if at < 0 {
		t.Fatal("docs/operations.md lost the retry query marker")
	}
	rest := doc[at:]
	open := strings.Index(rest, "```sql")
	if open < 0 {
		t.Fatal("docs/operations.md retry query has no sql fence")
	}
	body := rest[open+len("```sql"):]
	closeAt := strings.Index(body, "```")
	if closeAt < 0 {
		t.Fatal("docs/operations.md retry query fence is unterminated")
	}
	query := strings.TrimSpace(body[:closeAt])
	if !strings.Contains(query, "in_band") {
		t.Fatalf("retry query lost the in-band filter: %s", query)
	}

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE requests (
		id TEXT PRIMARY KEY,
		started_at INTEGER NOT NULL,
		retries INTEGER NOT NULL,
		status_code INTEGER NOT NULL,
		attempts TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, row := range []struct {
		id       string
		attempts string
	}{
		{"retry", `[{"status_code":503,"error_type":"server_error","at":"2026-10-25T00:00:00Z"}]`},
		{"inband", `[{"status_code":429,"in_band":true,"at":"2026-10-25T00:00:01Z"}]`},
		{"none", `null`},
	} {
		if _, err := db.Exec(
			`INSERT INTO requests(id, started_at, retries, status_code, attempts) VALUES (?, ?, 0, 200, ?)`,
			row.id, now, row.attempts); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("documented retry query failed: %v\n%s", err, query)
	}
	defer rows.Close()
	var ids, statuses []string
	for rows.Next() {
		var id string
		var started, retries, final int64
		var attemptStatus, attemptAt, retryAfter sql.NullString
		if err := rows.Scan(&started, &id, &retries, &final, &attemptStatus, &attemptAt, &retryAfter); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		statuses = append(statuses, attemptStatus.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "retry" || statuses[0] != "503" {
		t.Fatalf("documented retry query returned ids=%v statuses=%v, want only the 503 retry", ids, statuses)
	}
}
