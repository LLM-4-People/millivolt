package storage

import (
	"strings"
	"testing"
	"time"
)

// assertIndexRange pins one statement to an ordered request-log index range:
// the planner must search that index (plain or covering) and must not fall
// back to a whole-requests scan or a sort.
func assertIndexRange(t *testing.T, plan, index string) {
	t.Helper()
	if !strings.Contains(plan, "USING INDEX "+index) && !strings.Contains(plan, "USING COVERING INDEX "+index) {
		t.Fatalf("plan does not use %s:\n%s", index, plan)
	}
	if strings.Contains(plan, "SCAN requests") {
		t.Fatalf("plan scans requests instead of an index range:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("plan sorts instead of scanning index order:\n%s", plan)
	}
}

// assertLogIndexScan pins one plan to idx_requests_log, the only request-log
// index, without assuming SQLite spells the access as SCAN or SEARCH.
func assertLogIndexScan(t *testing.T, plan string) {
	t.Helper()
	if !strings.Contains(plan, "USING INDEX idx_requests_log") && !strings.Contains(plan, "USING COVERING INDEX idx_requests_log") {
		t.Fatalf("plan does not use idx_requests_log:\n%s", plan)
	}
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("plan sorts instead of using log order:\n%s", plan)
	}
}

// TestFilteredExportStreamsInLogOrder pins the durable export plan for every
// purge filter: its ORDER BY (started_at, id) is served by idx_requests_log,
// while equality predicates are rechecked per row. An unfiltered scan or a
// temp sort would mean the single-index schema broke streaming export.
func TestFilteredExportStreamsInLogOrder(t *testing.T) {
	s := openTestStore(t)
	seedQueryBeforeRows(t, s, 5_000, 10, 10)
	before := time.Now().UnixMilli() - 1_000
	for _, tc := range []struct {
		name   string
		filter PurgeFilter
	}{
		{"provider", PurgeFilter{Provider: "provider-03"}},
		{"model", PurgeFilter{Model: "model-03"}},
		{"client", PurgeFilter{Client: "client-003"}},
		{"conversation", PurgeFilter{ConversationID: "conv-000005"}},
		{"status", PurgeFilter{StatusCode: 500}},
		{"before", PurgeFilter{BeforeMs: before}},
		{"provider_and_status", PurgeFilter{Provider: "provider-03", StatusCode: 500}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exportSQL, exportArgs := buildExportQuery(tc.filter, time.Now().UnixMilli())
			assertLogIndexScan(t, explainQueryPlan(t, s.rdb, exportSQL, exportArgs))
		})
	}
}

// TestCountAndPurgePlanAgainstTheSingleLogIndex pins the cost of dropping the
// six per-dimension composites: an equality-only preview count or purge cannot
// seek an index, so it scans the requests table and rechecks the predicate. A
// time-bounded filter still gets an idx_requests_log range. Both purge-side
// statements come from the production builders, and the sidecar delete keeps
// its primary-key probe.
func TestCountAndPurgePlanAgainstTheSingleLogIndex(t *testing.T) {
	s := openTestStore(t)
	seedQueryBeforeRows(t, s, 5_000, 10, 10)
	before := time.Now().UnixMilli() - 1_000
	cases := []struct {
		name   string
		filter PurgeFilter
		index  string
	}{
		{"provider", PurgeFilter{Provider: "provider-03"}, ""},
		{"model", PurgeFilter{Model: "model-03"}, ""},
		{"client", PurgeFilter{Client: "client-003"}, ""},
		{"conversation", PurgeFilter{ConversationID: "conv-000005"}, ""},
		{"status", PurgeFilter{StatusCode: 500}, ""},
		{"before", PurgeFilter{BeforeMs: before}, "idx_requests_log"},
		{"provider_and_status", PurgeFilter{Provider: "provider-03", StatusCode: 500}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			countSQL, countArgs := buildCountQuery(tc.filter)
			countPlan := explainQueryPlan(t, s.rdb, countSQL, countArgs)
			deleteSQL, deleteArgs, err := buildPurgeRequestsDelete(&tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			deletePlan := explainQueryPlan(t, s.rdb, deleteSQL, deleteArgs)
			for _, plan := range []string{countPlan, deletePlan} {
				if tc.index == "" {
					if !strings.Contains(plan, "SCAN requests") || strings.Contains(plan, "USING INDEX") {
						t.Fatalf("equality predicate should be a plain table scan:\n%s", plan)
					}
					continue
				}
				assertIndexRange(t, plan, tc.index)
			}

			debugSQL, debugArgs, err := buildPurgeDebugDelete(&tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			plan := explainQueryPlan(t, s.rdb, debugSQL, debugArgs)
			if !strings.Contains(plan, "SEARCH request_debug") {
				t.Fatalf("sidecar delete does not probe its primary key:\n%s", plan)
			}
			if tc.index == "" {
				if !strings.Contains(plan, "SCAN requests") {
					t.Fatalf("sidecar subquery should scan requests for an equality predicate:\n%s", plan)
				}
			} else if !strings.Contains(plan, "USING INDEX "+tc.index) && !strings.Contains(plan, "USING COVERING INDEX "+tc.index) {
				t.Fatalf("sidecar subquery does not use %s:\n%s", tc.index, plan)
			}
		})
	}
}

// TestUnfilteredPathPlansAreDocumented covers the three statements that have
// no predicate to serve. The assertions pin the inherent shape rather than
// pretending an index range exists:
//   - the ordered export scans idx_requests_log to stream (started_at,id);
//   - COUNT(*) has no WHERE, so SQLite reads the smallest covering index;
//   - an unfiltered DELETE has no key to seek and must scan every row.
//
// The unfiltered sidecar delete still probes request_debug's primary key and
// the live requests primary key per capture, because a capture can precede
// its request row and must survive.
func TestUnfilteredPathPlansAreDocumented(t *testing.T) {
	s := openTestStore(t)
	seedQueryBeforeRows(t, s, 5_000, 10, 10)

	exportSQL, exportArgs := buildExportQuery(PurgeFilter{}, time.Now().UnixMilli())
	exportPlan := explainQueryPlan(t, s.rdb, exportSQL, exportArgs)
	if !strings.Contains(exportPlan, "SCAN requests USING INDEX idx_requests_log") {
		t.Fatalf("unfiltered export does not stream in log order:\n%s", exportPlan)
	}
	if strings.Contains(exportPlan, "TEMP B-TREE") {
		t.Fatalf("unfiltered export sorts instead of streaming the log index:\n%s", exportPlan)
	}

	countSQL, countArgs := buildCountQuery(PurgeFilter{})
	countPlan := explainQueryPlan(t, s.rdb, countSQL, countArgs)
	if !strings.Contains(countPlan, "SCAN requests") || strings.Contains(countPlan, "TEMP B-TREE") {
		t.Fatalf("unfiltered COUNT(*) plan = %s", countPlan)
	}

	deleteSQL, deleteArgs, err := buildPurgeRequestsDelete(nil)
	if err != nil {
		t.Fatal(err)
	}
	deletePlan := explainQueryPlan(t, s.rdb, deleteSQL, deleteArgs)
	if !strings.Contains(deletePlan, "SCAN requests") {
		t.Fatalf("unfiltered request delete is not a full scan:\n%s", deletePlan)
	}

	debugSQL, debugArgs, err := buildPurgeDebugDelete(nil)
	if err != nil {
		t.Fatal(err)
	}
	debugPlan := explainQueryPlan(t, s.rdb, debugSQL, debugArgs)
	if !strings.Contains(debugPlan, "SEARCH request_debug USING COVERING INDEX sqlite_autoindex_request_debug_1") ||
		!strings.Contains(debugPlan, "SEARCH r EXISTS USING COVERING INDEX sqlite_autoindex_requests_1") {
		t.Fatalf("unfiltered sidecar delete lost its primary-key probes:\n%s", debugPlan)
	}
}

// TestUnindexedPurgePredicatesAreDocumented covers the filters that genuinely
// cannot be an index range:
//   - HasError is an OR across status_code, error_type and
//     json_each(attempts); SQLite has no index for JSON array membership, so
//     the only correct plan is a requests scan with correlated json_each
//     probes (a time bound still selects the ordered log index first);
//   - ErrorType and HasDebug have no index by schema design (the published
//     index reference names error_type as unindexed), so both scan requests.
//
// These assertions pin the documented shape instead of demanding an index the
// schema does not promise.
func TestUnindexedPurgePredicatesAreDocumented(t *testing.T) {
	s := openTestStore(t)
	seedQueryBeforeRows(t, s, 5_000, 10, 10)
	for _, tc := range []struct {
		name     string
		filter   PurgeFilter
		index    string
		jsonEach bool
	}{
		{name: "error_only", filter: PurgeFilter{HasError: true}, jsonEach: true},
		{name: "error_before", filter: PurgeFilter{HasError: true, BeforeMs: time.Now().UnixMilli()}, index: "idx_requests_log", jsonEach: true},
		{name: "error_type", filter: PurgeFilter{ErrorType: "provider_error"}},
		{name: "debug_only", filter: PurgeFilter{HasDebug: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exportSQL, exportArgs := buildExportQuery(tc.filter, time.Now().UnixMilli())
			plan := explainQueryPlan(t, s.rdb, exportSQL, exportArgs)
			countSQL, countArgs := buildCountQuery(tc.filter)
			countPlan := explainQueryPlan(t, s.rdb, countSQL, countArgs)
			for _, got := range []string{plan, countPlan} {
				if tc.jsonEach && !strings.Contains(got, "SCAN json_each") {
					t.Fatalf("HasError plan does not inspect attempts:\n%s", got)
				}
				if tc.index != "" {
					assertIndexRange(t, got, tc.index)
				} else if !strings.Contains(got, "SCAN requests") {
					t.Fatalf("unindexed predicate plan is not a requests scan:\n%s", got)
				}
			}
		})
	}
}
