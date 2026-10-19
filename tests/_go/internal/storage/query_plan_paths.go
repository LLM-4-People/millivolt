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

// TestExportCountAndPurgeUseOrderedCompositeIndexes pins the plan for every
// production path the docs promise is an index range: the durable export,
// the delete-preview count and both purge deletes. The statements come from
// the same builders the production methods use, so a schema/index change or a
// predicate rewrite that reintroduces a whole-log scan fails here rather than
// silently changing query cost.
func TestExportCountAndPurgeUseOrderedCompositeIndexes(t *testing.T) {
	s := openTestStore(t)
	seedQueryBeforeRows(t, s, 5_000, 10, 10)
	before := time.Now().UnixMilli() - 1_000
	cases := []struct {
		name   string
		filter PurgeFilter
		index  string
	}{
		{"provider", PurgeFilter{Provider: "provider-03"}, "idx_requests_provider_log"},
		{"model", PurgeFilter{Model: "model-03"}, "idx_requests_model_log"},
		{"client", PurgeFilter{Client: "client-003"}, "idx_requests_client_log"},
		{"conversation", PurgeFilter{ConversationID: "conv-000005"}, "idx_requests_conversation_log"},
		{"status", PurgeFilter{StatusCode: 500}, "idx_requests_status_log"},
		{"before", PurgeFilter{BeforeMs: before}, "idx_requests_log"},
		{"provider_and_status", PurgeFilter{Provider: "provider-03", StatusCode: 500}, "idx_requests_status_log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exportSQL, exportArgs := buildExportQuery(tc.filter, time.Now().UnixMilli())
			assertIndexRange(t, explainQueryPlan(t, s.rdb, exportSQL, exportArgs), tc.index)

			countSQL, countArgs := buildCountQuery(tc.filter)
			assertIndexRange(t, explainQueryPlan(t, s.rdb, countSQL, countArgs), tc.index)

			deleteSQL, deleteArgs, err := buildPurgeRequestsDelete(&tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			assertIndexRange(t, explainQueryPlan(t, s.rdb, deleteSQL, deleteArgs), tc.index)

			debugSQL, debugArgs, err := buildPurgeDebugDelete(&tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			plan := explainQueryPlan(t, s.rdb, debugSQL, debugArgs)
			assertIndexRange(t, plan, tc.index)
			if !strings.Contains(plan, "SEARCH request_debug") {
				t.Fatalf("sidecar delete does not probe its primary key:\n%s", plan)
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
