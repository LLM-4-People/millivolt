package storage

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// requestIndexExists reports whether the durable requests table owns one
// named index. The name is a test-owned schema identifier, never request data.
func requestIndexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists int
	if err := db.QueryRowContext(t.Context(),
		`SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = 'index' AND tbl_name = 'requests' AND name = ?)`,
		name).Scan(&exists); err != nil {
		t.Fatalf("inspect index %s: %v", name, err)
	}
	return exists == 1
}

// requestIndexColumns returns one index's key columns in key order.
func requestIndexColumns(t *testing.T, db *sql.DB, name string) []string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "PRAGMA index_info("+name+")")
	if err != nil {
		t.Fatalf("index_info(%s): %v", name, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var seq, cid int
		var column string
		if err := rows.Scan(&seq, &cid, &column); err != nil {
			t.Fatalf("scan index_info(%s): %v", name, err)
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("index_info(%s): %v", name, err)
	}
	return columns
}

// assertOrderedRequestLogIndexes pins the canonical log indexes and the
// absence of every superseded spelling. It is shared by the fresh-schema and
// reopen-after-migration contract tests.
func assertOrderedRequestLogIndexes(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, tc := range []struct {
		name    string
		columns []string
	}{
		{"idx_requests_log", []string{"started_at", "id"}},
		{"idx_requests_provider_log", []string{"provider", "started_at", "id"}},
		{"idx_requests_model_log", []string{"model", "started_at", "id"}},
		{"idx_requests_client_log", []string{"client", "started_at", "id"}},
		{"idx_requests_key_log", []string{"key_hash", "started_at", "id"}},
		{"idx_requests_conversation_log", []string{"conversation_id", "started_at", "id"}},
		{"idx_requests_status_log", []string{"status_code", "started_at", "id"}},
	} {
		if !requestIndexExists(t, db, tc.name) {
			t.Fatalf("canonical index %s is missing", tc.name)
		}
		if got := requestIndexColumns(t, db, tc.name); !reflect.DeepEqual(got, tc.columns) {
			t.Fatalf("index %s key columns = %v, want %v", tc.name, got, tc.columns)
		}
	}
	for _, name := range []string{"idx_requests_started_at", "idx_requests_provider", "idx_requests_model"} {
		if requestIndexExists(t, db, name) {
			t.Fatalf("superseded index %s still exists", name)
		}
	}
}

func TestRequestLogIndexesAreOrderedComposites(t *testing.T) {
	s := openTestStore(t)
	assertOrderedRequestLogIndexes(t, s.rdb)
}

// TestMigrateReplacesSupersededProviderAndModelIndexes creates a fixture with
// the old single-column indexes and history, then reopens it twice: the first
// open must create the ordered composites, drop the old spellings and keep
// every row; the second open proves the drop-and-create pass is idempotent and
// never removes the replacements.
func TestMigrateReplacesSupersededProviderAndModelIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-indexes.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(oldRequestsSchema + `
CREATE INDEX idx_requests_provider ON requests (provider);
CREATE INDEX idx_requests_model ON requests (model);
CREATE INDEX idx_requests_log ON requests (started_at, id);`); err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{"legacy-0", "legacy-1", "legacy-2"}
	for i, id := range wantIDs {
		insertLegacyRow(t, db, id, 200, 1_700_000_000_000+int64(i)*1_000)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("open legacy indexes: %v", err)
	}
	assertOrderedRequestLogIndexes(t, s.rdb)
	assertRecentIDs(t, s, wantIDs)
	epoch, highRowID := projectionState(t, s.rdb)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatalf("reopen migrated indexes: %v", err)
	}
	defer s2.Close()
	assertOrderedRequestLogIndexes(t, s2.rdb)
	assertRecentIDs(t, s2, wantIDs)
	// An index-only migration must not invalidate the analytical projection.
	if gotEpoch, gotHighRowID := projectionState(t, s2.rdb); gotEpoch != epoch || gotHighRowID != highRowID {
		t.Fatalf("projection state = (%d, %d), want unchanged (%d, %d)", gotEpoch, gotHighRowID, epoch, highRowID)
	}
}

func projectionState(t *testing.T, db *sql.DB) (epoch, highRowID int64) {
	t.Helper()
	if err := db.QueryRowContext(t.Context(),
		`SELECT epoch, high_rowid FROM request_projection_state WHERE singleton = 1`).Scan(&epoch, &highRowID); err != nil {
		t.Fatalf("read projection state: %v", err)
	}
	return epoch, highRowID
}

func assertRecentIDs(t *testing.T, s *Store, want []string) {
	t.Helper()
	records, err := s.LoadRecent(t.Context(), len(want)+10)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(records))
	for i, record := range records {
		got[i] = record.ID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored row ids = %v, want %v", got, want)
	}
}

// explainQueryPlan returns every detail line SQLite emits for one statement,
// with the statement's bound arguments so modernc prepares it exactly as the
// production read does.
func explainQueryPlan(t *testing.T, db *sql.DB, query string, args []any) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, aux int
		var detail string
		if err := rows.Scan(&id, &parent, &aux, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("query plan rows: %v", err)
	}
	return strings.Join(details, "\n")
}

// TestQueryBeforeEqualityUsesOrderedCompositeIndex pins the plan for the exact
// production QueryBefore statement: every LogFilter equality field chooses its
// ordered composite index and SQLite performs no sort, with and without the
// exclusive cursor bound. A future schema change that drops, renames or
// re-columns one of these indexes fails here instead of silently turning a
// filtered page into a whole-log scan.
func TestQueryBeforeEqualityUsesOrderedCompositeIndex(t *testing.T) {
	s := openTestStore(t)
	seedQueryBeforeRows(t, s, 5_000, 10, 10)

	cursor := &LogCursor{StartMs: queryBeforeFixtureStartMs + 2_500, ID: "row-002500"}
	for _, tc := range []struct {
		name   string
		before *LogCursor
		field  LogField
		value  string
		index  string
	}{
		{"provider", nil, LogProvider, "provider-03", "idx_requests_provider_log"},
		{"model", nil, LogModel, "model-03", "idx_requests_model_log"},
		{"client", nil, LogClient, "client-003", "idx_requests_client_log"},
		{"key_hash", nil, LogKeyHash, "key-000005", "idx_requests_key_log"},
		{"conversation", nil, LogConversation, "conv-000005", "idx_requests_conversation_log"},
		{"status", nil, LogStatus, "500", "idx_requests_status_log"},
		{"status_common", nil, LogStatus, "200", "idx_requests_status_log"},
		{"provider_cursor", cursor, LogProvider, "provider-03", "idx_requests_provider_log"},
		{"model_cursor", cursor, LogModel, "model-03", "idx_requests_model_log"},
		{"client_cursor", cursor, LogClient, "client-003", "idx_requests_client_log"},
		{"key_hash_cursor", cursor, LogKeyHash, "key-000005", "idx_requests_key_log"},
		{"conversation_cursor", cursor, LogConversation, "conv-000005", "idx_requests_conversation_log"},
		{"status_cursor", cursor, LogStatus, "500", "idx_requests_status_log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: tc.value}}}
			query, args, err := buildQueryBefore(tc.before, filter, 200)
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			plan := explainQueryPlan(t, s.rdb, query, args)
			if !strings.Contains(plan, "USING INDEX "+tc.index) && !strings.Contains(plan, "USING COVERING INDEX "+tc.index) {
				t.Fatalf("plan does not use %s:\n%s", tc.index, plan)
			}
			if strings.Contains(plan, "idx_requests_log") {
				t.Fatalf("equality page fell back to the bare log index:\n%s", plan)
			}
			if strings.Contains(plan, "TEMP B-TREE") {
				t.Fatalf("equality page sorts instead of scanning index order:\n%s", plan)
			}
			if !strings.Contains(plan, "SEARCH requests") {
				t.Fatalf("equality page is not an index search:\n%s", plan)
			}
		})
	}
}

// TestQueryBeforeEveryEqualityFieldPages verifies the SQL predicate and the
// fixture's Go expectation agree for every LogFilter equality field, with and
// without the exclusive cursor. Expected ids come from queryBeforeFixtureRow,
// the same owner that seeds the rows, so the test asserts page semantics rather
// than restating the fixture by hand.
func TestQueryBeforeEveryEqualityFieldPages(t *testing.T) {
	s := openTestStore(t)
	const rows = 2_000
	seedQueryBeforeRows(t, s, rows, 10, 10)
	cursorRow := queryBeforeFixtureRow(rows/2, 10, 10)
	cursor := &LogCursor{StartMs: cursorRow.Start.UnixMilli(), ID: cursorRow.ID}
	for _, tc := range []struct {
		name  string
		field LogField
		value string
	}{
		{"provider", LogProvider, "provider-03"},
		{"model", LogModel, "model-03"},
		{"client", LogClient, "client-003"},
		{"key_hash", LogKeyHash, "key-000005"},
		{"conversation", LogConversation, "conv-000005"},
		{"status", LogStatus, "500"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			filter := &LogFilter{All: []LogPredicate{{Field: tc.field, Value: tc.value}}}
			for _, before := range []*LogCursor{nil, cursor} {
				got, err := s.QueryBefore(t.Context(), before, filter, rows)
				if err != nil {
					t.Fatal(err)
				}
				ids := make([]string, len(got))
				for i, record := range got {
					ids[i] = record.ID
					if !queryBeforeFixtureMatches(record, tc.field, tc.value) {
						t.Fatalf("page leaked record %s for %s=%q", record.ID, tc.name, tc.value)
					}
				}
				stop := rows
				label := "no cursor"
				if before != nil {
					stop = rows / 2
					label = "cursor"
				}
				want := queryBeforeExpectedIDs(tc.field, tc.value, stop)
				if !reflect.DeepEqual(ids, want) {
					t.Fatalf("%s (%s): page ids = %v, want %v", tc.name, label, ids, want)
				}
			}
		})
	}
}

// queryBeforeFixtureMatches is the test-side mirror of the LogField to column
// mapping. It reads the fixture owner's records rather than trusting the SQL
// query under test.
func queryBeforeFixtureMatches(r *metrics.Record, field LogField, value string) bool {
	switch field {
	case LogProvider:
		return r.Provider == value
	case LogClient:
		return r.Client == value
	case LogKeyHash:
		return r.KeyHash == value
	case LogConversation:
		return r.ConversationID == value
	case LogModel:
		return r.Model == value
	case LogStatus:
		return strconv.Itoa(r.StatusCode) == value
	default:
		return false
	}
}

// queryBeforeExpectedIDs lists the fixture ids below stop in newest-first log
// order, matching one equality. The fixture's distinct millisecond timestamps
// make the id tie-break unnecessary in this expectation.
func queryBeforeExpectedIDs(field LogField, value string, stop int) []string {
	out := make([]string, 0)
	for i := stop - 1; i >= 0; i-- {
		record := queryBeforeFixtureRow(i, 10, 10)
		if queryBeforeFixtureMatches(record, field, value) {
			out = append(out, record.ID)
		}
	}
	return out
}

// TestQueryBeforeProviderAndModelPagesPreserveOrder pins the filtered page
// contract after the index change: newest-first order, raw model equality,
// the exclusive (started_at,id) cursor, and the requested limit. Records g/h
// and j/k share a timestamp so the descending id tie-break is exercised.
func TestQueryBeforeProviderAndModelPagesPreserveOrder(t *testing.T) {
	s := openTestStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	at := func(offset int) time.Time { return base.Add(time.Duration(offset) * time.Millisecond) }
	rows := []*metrics.Record{
		{ID: "a", Provider: "p0", Model: "m0", Start: at(0)},
		{ID: "b", Provider: "p0", Model: "m0", Start: at(1)},
		{ID: "c", Provider: "p1", Model: "m0", Start: at(2)},
		{ID: "d", Provider: "p0", Model: "m1", Start: at(3)},
		{ID: "e", Provider: "p0", Model: "m0", Start: at(4)},
		{ID: "f", Provider: "p1", Model: "m1", Start: at(5)},
		{ID: "g", Provider: "p0", Model: "m1", Start: at(6)},
		{ID: "h", Provider: "p0", Model: "m0", Start: at(6)},
		{ID: "i", Provider: "p1", Model: "m0", Start: at(7)},
		{ID: "j", Provider: "p0", Model: "m0", Start: at(8)},
		{ID: "k", Provider: "p0", Model: "m1", Start: at(8)},
		{ID: "l", Provider: "p0", Model: "m0", Start: at(9)},
	}
	for _, record := range rows {
		record.KeyHash, record.StatusCode = "k", 200
	}
	if err := s.insertBatch(rows); err != nil {
		t.Fatal(err)
	}

	provider := &LogFilter{All: []LogPredicate{{Field: LogProvider, Value: "p0"}}}
	modelM0 := &LogFilter{All: []LogPredicate{{Field: LogModel, Value: "m0"}}}
	modelM1 := &LogFilter{All: []LogPredicate{{Field: LogModel, Value: "m1"}}}
	both := &LogFilter{All: []LogPredicate{
		{Field: LogProvider, Value: "p0"},
		{Field: LogModel, Value: "m0"},
	}}
	for _, tc := range []struct {
		name   string
		before *LogCursor
		filter *LogFilter
		limit  int
		match  func(*metrics.Record) bool
		want   []string
	}{
		{"provider", nil, provider, 5, func(r *metrics.Record) bool { return r.Provider == "p0" }, []string{"l", "k", "j", "h", "g"}},
		{"provider_cursor", &LogCursor{StartMs: at(8).UnixMilli(), ID: "j"}, provider, 3, func(r *metrics.Record) bool { return r.Provider == "p0" }, []string{"h", "g", "e"}},
		{"model", nil, modelM0, 4, func(r *metrics.Record) bool { return r.Model == "m0" }, []string{"l", "j", "i", "h"}},
		{"model_cursor", &LogCursor{StartMs: at(6).UnixMilli(), ID: "g"}, modelM1, 2, func(r *metrics.Record) bool { return r.Model == "m1" }, []string{"f", "d"}},
		{"provider_and_model", nil, both, 10, func(r *metrics.Record) bool { return r.Provider == "p0" && r.Model == "m0" }, []string{"l", "j", "h", "e", "b", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.QueryBefore(t.Context(), tc.before, tc.filter, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]string, len(got))
			for i, record := range got {
				ids[i] = record.ID
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("page ids = %v, want %v", ids, tc.want)
			}
			if len(got) > tc.limit {
				t.Fatalf("page has %d rows, limit %d", len(got), tc.limit)
			}
			for _, record := range got {
				if !tc.match(record) {
					t.Fatalf("page leaked record %s (provider %q, model %q)", record.ID, record.Provider, record.Model)
				}
			}
		})
	}
}
