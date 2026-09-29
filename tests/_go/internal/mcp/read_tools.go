package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/storage"
)

const (
	schemaPath    = "/metrics/query"
	explorerPath  = "/metrics/agg/explorer"
	chartPath     = "/metrics/agg/chart"
	logPath       = "/metrics/agg/log"
	bootstrapPath = "/metrics/bootstrap"
	prometheusPat = "/metrics/prometheus"
	debugPath     = "/admin/debug"
	capturePath   = "/admin/debug/capture"
	pausePath     = "/admin/pause"
	throttlePath  = "/admin/throttle"
	quotaPath     = "/admin/quota"
	configPath    = "/admin/config"
	reloadPath    = "/admin/reload"
	restartPath   = "/admin/restart"
	purgePath     = "/admin/purge"
	purgeCountPat = "/admin/purge/count"
)

// minimalBootstrap is the smallest /metrics/bootstrap body the tools accept.
const minimalBootstrap = `{"seq":9,"feed_id":"feed-a","counters":{"in_flight":1,"total_requests":12,"total_errors":3},` +
	`"storage":{"enabled":true,"dropped":0,"totals_degraded":false},"records":[]}`

const emptyDebugStatus = `{"ok":true,"enabled":false,"sessions":[],"known_clients":["dev-traffic"],` +
	`"known_providers":["local"],"known_models":["demo"],"ttl":"168h","max_bytes":"1MiB"}`

// TestReadToolsRequestShape pins, for every read tool, the exact method, path,
// query string and Authorization header that reaches the proxy.
func TestReadToolsRequestShape(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath, `[{"type":"table","name":"requests","tbl_name":"requests","sql":"CREATE TABLE requests (id TEXT)"}]`)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, explorerPath, `{"dim":"provider","total":4,"error_total":1,"groups":[{"name":"local","n":4}],`+
		`"rail":{"provider":1},"scope":{"matches":4,"errors":1},"conversation_summary":{"main":1,"sub":0,"unresolved":0}}`)
	proxy.json(http.MethodGet, chartPath, `{"now_ms":2000,"from_ms":1000,"bucket_ms":1000,"ttft_p":null,"tps_p":null,`+
		`"ttft_stat":null,"tps_stat":null,"cost_per_mtok":null,"buckets":[{"t":1000,"req":2,"err":0,"rl":1,"in":1,"out":1,"cache":0,"reason":0,"cost":0.1,"ttft":null,"tps":null}]}`)
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r1","status_code":200}],"model_canon":{"revision":"rev-1","names":{"demo":"demo","demo-2":"demo"}},`+
		`"more":false,"cursor_ms":1500,"cursor_id":"r1"}`)
	proxy.respond(http.MethodGet, prometheusPat, cannedResponse{
		Status: http.StatusOK, ContentType: "text/plain; version=0.0.4",
		Body: "# HELP x x\n# TYPE x gauge\nx 1\n",
	})

	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	if _, err := service.query(ctx, QueryInput{SQL: "SELECT id FROM requests", MaxRows: 5}); err != nil {
		t.Fatalf("query: %v", err)
	}
	request := proxy.requestsFor(http.MethodGet, schemaPath)[0]
	assertAuth(t, request)
	assertQuery(t, request, "q", "SELECT id FROM requests")

	if _, err := service.explore(ctx, ExploreInput{
		Dim:   "provider",
		Scope: Scope{Filters: []string{"client:dev-traffic", "model:demo"}, Status: "200"},
	}); err != nil {
		t.Fatalf("explore: %v", err)
	}
	request = proxy.requestsFor(http.MethodGet, explorerPath)[0]
	assertAuth(t, request)
	assertQuery(t, request, "dim", "provider")
	assertQuery(t, request, "s", "200")
	if got := request.Query["f"]; len(got) != 2 || got[0] != "client:dev-traffic" || got[1] != "model:demo" {
		t.Fatalf("filters = %q, want both repeated f values in order", got)
	}

	if _, err := service.chart(ctx, ChartInput{Window: "60", Scope: Scope{Status: "streaming"}}); err != nil {
		t.Fatalf("chart: %v", err)
	}
	request = proxy.requestsFor(http.MethodGet, chartPath)[0]
	assertAuth(t, request)
	assertQuery(t, request, "window", "60")
	assertQuery(t, request, "s", "streaming")

	if _, err := service.records(ctx, RecordsInput{BeforeMs: 1500, BeforeID: "r1", Limit: 10}); err != nil {
		t.Fatalf("records: %v", err)
	}
	request = proxy.requestsFor(http.MethodGet, logPath)[0]
	assertAuth(t, request)
	assertQuery(t, request, "before_ms", "1500")
	assertQuery(t, request, "before_id", "r1")
	assertQuery(t, request, "limit", "10")

	if _, err := service.snapshot(ctx, SnapshotInput{MaxRecords: 3}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	request = proxy.requestsFor(http.MethodGet, bootstrapPath)[0]
	assertAuth(t, request)
	assertNoQuery(t, request, "since")

	if _, err := service.prometheus(ctx, PrometheusInput{}); err != nil {
		t.Fatalf("prometheus: %v", err)
	}
	request = proxy.requestsFor(http.MethodGet, prometheusPat)[0]
	assertAuth(t, request)
	assertNoQuery(t, request, "q")

	// Every read tool presented the credential, and none of them sent a body.
	for _, request := range proxy.requests() {
		assertAuth(t, request)
		assertNoToken(t, request.Path+" query", request.Query.Encode())
		assertNoToken(t, request.Path+" body", request.Body)
	}
}

// TestDescribeRequestShapeAndOutput pins the schema probe, the storage probe
// and the capture-vocabulary probe, and the mapping of each onto the output.
func TestDescribeRequestShapeAndOutput(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, `{"ok":true,"enabled":true,"sessions":[{"id":"s1","clients":["dev"],"ran_ms":5,"captures":2}],`+
		`"known_clients":["dev"],"known_providers":["local"],"known_models":["demo"],"ttl":"168h","max_bytes":"1MiB"}`)
	proxy.json(http.MethodGet, schemaPath,
		`[{"type":"table","name":"requests","tbl_name":"requests","sql":"CREATE TABLE requests (id TEXT)"},`+
			`{"type":"index","name":"idx_requests_log","tbl_name":"requests","sql":"CREATE INDEX ..."}]`)

	service := newTestService(t, proxy, Limits{})
	out, err := service.describe(context.Background(), DescribeInput{})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	probe := proxy.requestsFor(http.MethodGet, schemaPath)[0]
	assertAuth(t, probe)
	assertQuery(t, probe, "q", schemaStatement)
	if len(out.Schema) != 2 || out.Schema[0].Name != "requests" || out.Schema[1].Type != "index" {
		t.Fatalf("schema mapping = %+v", out.Schema)
	}
	if !out.Storage.Enabled || out.KnownProviders[0] != "local" {
		t.Fatalf("storage/vocabulary mapping = %+v %+v", out.Storage, out.KnownProviders)
	}
	if out.Limits.QueryMaxRows != DefaultLimits().QueryMaxRows || out.Limits.LogPageMax != logPageMax {
		t.Fatalf("limits mapping = %+v", out.Limits)
	}
	// The reference must carry the health predicates and the unit facts, or a
	// model cannot write correct SQL.
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"Unix MILLISECONDS", "429 alone is NOT an error", "cost is USD", "keyset pagination", "idx_requests_log"} {
		if !strings.Contains(string(body), needle) {
			t.Fatalf("describe output is missing %q", needle)
		}
	}
	assertNoToken(t, "describe output", string(body))
}

// TestDescribeWithoutDurableStorage pins that a disabled store degrades the
// schema probe to a note instead of failing the whole reference.
func TestDescribeWithoutDurableStorage(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":false},"records":[]}`)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.fail(http.MethodGet, schemaPath, http.StatusServiceUnavailable, "durable storage is disabled", "")

	service := newTestService(t, proxy, Limits{})
	out, err := service.describe(context.Background(), DescribeInput{})
	if err != nil {
		t.Fatalf("describe with storage off must still answer: %v", err)
	}
	if out.Storage.Enabled {
		t.Fatal("storage.enabled must be reported false")
	}
	if len(out.Schema) != 0 {
		t.Fatalf("no schema can be read with storage off: %+v", out.Schema)
	}
	if !strings.Contains(strings.Join(out.Notes, " "), "durable storage") {
		t.Fatalf("notes must explain the missing schema: %q", out.Notes)
	}
	// The column reference is still available, because it is not read from the
	// database.
	if len(out.Tables) == 0 {
		t.Fatal("the curated column reference must survive a disabled store")
	}
}

// TestCannedResponsesMapOntoToolOutput pins that each read tool's typed output
// is filled from the proxy's document rather than from a pass-through blob.
func TestCannedResponsesMapOntoToolOutput(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, explorerPath, `{"dim":"model","total":9,"error_total":2,"groups":[{"name":"m1","n":9,"cost":1.5,"err_final":2}],`+
		`"rail":{"model":1},"scope":{"matches":9,"errors":2},"conversation_summary":{"main":3,"sub":1,"unresolved":0}}`)
	proxy.json(http.MethodGet, chartPath, `{"now_ms":3000,"from_ms":1000,"bucket_ms":2000,"ttft_p":[10.5,20.5,30.5],"tps_p":null,`+
		`"ttft_stat":[11,10,12],"tps_stat":null,"cost_per_mtok":0.25,"buckets":[{"t":1000,"req":3,"err":1,"rl":2,"in":10,"out":5,"cache":2,"reason":1,"cost":0.3,"ttft":[9,10,11],"tps":[4,5,6]}]}`)
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r9"},{"id":"r8"}],"model_canon":{"a":"b"},"more":true,"cursor_ms":1400,"cursor_id":"r8"}`)
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":42,"feed_id":"feed-z","counters":{"in_flight":2,"total_requests":99,"total_errors":7},`+
		`"storage":{"enabled":true,"dropped":3,"totals_degraded":true},"records":[{"id":"n1"},{"id":"n0"}]}`)
	proxy.json(http.MethodGet, prometheusPat, "")

	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	explorer, err := service.explore(ctx, ExploreInput{Dim: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if explorer.Dim != "model" || explorer.Total != 9 || explorer.Errors != 2 || len(explorer.Groups) != 1 {
		t.Fatalf("explorer output = %+v", explorer)
	}
	if explorer.ConversationSummary["main"] != 3 || explorer.Scope["matches"] != 9 {
		t.Fatalf("explorer nested documents = %+v %+v", explorer.ConversationSummary, explorer.Scope)
	}

	chart, err := service.chart(ctx, ChartInput{Window: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if chart.BucketMs != 2000 || len(chart.TTFTp) != 3 || chart.CostPerMTok == nil || *chart.CostPerMTok != 0.25 {
		t.Fatalf("chart output = %+v", chart)
	}
	if len(chart.Buckets) != 1 || chart.Buckets[0]["req"].(float64) != 3 {
		t.Fatalf("chart buckets = %+v", chart.Buckets)
	}

	records, err := service.records(ctx, RecordsInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if records.Returned != 2 || !records.More || records.Exhausted {
		t.Fatalf("records output = %+v", records)
	}
	if records.NextBeforeID != "r8" || records.NextBeforeMs != 1400 {
		t.Fatalf("records cursor = %+v", records)
	}

	snapshot, err := service.snapshot(ctx, SnapshotInput{MaxRecords: 10})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Seq != 42 || snapshot.FeedID != "feed-z" || snapshot.InFlight != 2 ||
		snapshot.TotalRequests != 99 || snapshot.TotalErrors != 7 {
		t.Fatalf("snapshot output = %+v", snapshot)
	}
	if !snapshot.Storage.TotalsDegraded || snapshot.Storage.Dropped != 3 || len(snapshot.Records) != 2 {
		t.Fatalf("snapshot storage/records = %+v %+v", snapshot.Storage, snapshot.Records)
	}

	// An empty exposition must be an empty result, not one blank line.
	prom, err := service.prometheus(ctx, PrometheusInput{})
	if err != nil {
		t.Fatal(err)
	}
	if prom.Lines != 0 || prom.Text != "" {
		t.Fatalf("empty exposition = %+v", prom)
	}
}

// TestWindowDefaultsToAllHistory pins that an unset chart window is stated
// explicitly rather than left to the proxy's default.
func TestWindowDefaultsToAllHistory(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, chartPath, `{"now_ms":1,"from_ms":0,"bucket_ms":1,"buckets":[]}`)
	service := newTestService(t, proxy, Limits{})
	if _, err := service.chart(context.Background(), ChartInput{}); err != nil {
		t.Fatal(err)
	}
	assertQuery(t, proxy.last(t), "window", "all")
}

// TestChartRejectsNonCanonicalWindowPreFlight is a local fast-fail: the proxy
// is the authority, but the tool must not send a window it already knows is
// rejected.
func TestScopeValidationRejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope Scope
		want  string
	}{
		{"missing colon", Scope{Filters: []string{"provider"}}, "dim:id"},
		{"empty id", Scope{Filters: []string{"provider:"}}, "dim:id"},
		{"unknown dimension", Scope{Filters: []string{"colour:red"}}, "unknown"},
		{"unknown status", Scope{Status: "okay"}, "exact HTTP status code"},
		{"out of range status", Scope{Status: "10000"}, "exact HTTP status code"},
		{"too many filters", Scope{Filters: make([]string, maxScopeFilters+1)}, "at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.scope.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want an error mentioning %q", err, tc.want)
			}
		})
	}
	full := make([]string, maxScopeFilters)
	for i := range full {
		full[i] = "provider:x"
	}
	for _, ok := range []Scope{
		{},
		{Filters: []string{"status:2xx", "model:a:b"}},
		{Status: "0"},
		{Status: "9999"},
		{Status: "throttled"},
		{Filters: full},
	} {
		if err := ok.Validate(); err != nil {
			t.Fatalf("Validate(%+v) = %v, want nil", ok, err)
		}
	}
}

// TestBadScopeNeverReachesTheProxy pins deny-by-default: an invalid dimension,
// filter or status is refused locally instead of being sent and silently
// ignored or broadened by the proxy.
func TestBadScopeNeverReachesTheProxy(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, explorerPath, `{"dim":"provider","groups":[]}`)
	proxy.json(http.MethodGet, chartPath, `{"buckets":[]}`)
	proxy.json(http.MethodGet, logPath, `{"records":[],"more":false}`)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()

	if _, err := service.explore(ctx, ExploreInput{Dim: "colour", Scope: Scope{Status: "200"}}); err == nil {
		t.Fatal("an unknown dimension must be refused")
	}
	if _, err := service.explore(ctx, ExploreInput{Dim: "provider", Scope: Scope{Filters: []string{"provider"}}}); err == nil {
		t.Fatal("a malformed filter must be refused")
	}
	if _, err := service.chart(ctx, ChartInput{Scope: Scope{Status: "maybe"}}); err == nil {
		t.Fatal("an unknown status must be refused")
	}
	if _, err := service.records(ctx, RecordsInput{Scope: Scope{Filters: []string{"nope:x"}}}); err == nil {
		t.Fatal("an unknown filter dimension must be refused")
	}
	if len(proxy.requests()) != 0 {
		t.Fatalf("no invalid scope may reach the proxy, got %d requests", len(proxy.requests()))
	}
}

// TestRecordsCursorPairing pins the paired cursor rule in both directions.
func TestRecordsCursorPairing(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	service := newTestService(t, proxy, Limits{})
	ctx := context.Background()
	for _, in := range []RecordsInput{
		{BeforeMs: 100},
		{BeforeID: "r1"},
		{BeforeMs: 100, BeforeID: ""},
		{BeforeMs: 0, BeforeID: "r1"},
	} {
		if _, err := service.records(ctx, in); err == nil {
			t.Fatalf("an unpaired cursor must be refused: %+v", in)
		}
	}
	if len(proxy.requests()) != 0 {
		t.Fatal("an unpaired cursor must not reach the proxy")
	}
	for _, limit := range []int{0, 9, 501, -1} {
		in := RecordsInput{Limit: limit}
		if limit == 0 {
			in.Limit = 0
			proxy.json(http.MethodGet, logPath, `{"records":[],"more":false}`)
			if _, err := service.records(ctx, in); err != nil {
				t.Fatalf("limit 0 must fall back to the configured page size: %v", err)
			}
			continue
		}
		if _, err := service.records(ctx, in); err == nil {
			t.Fatalf("limit %d must be refused", limit)
		}
	}
}

// TestRecordsStopsOnANonAdvancingCursor is the pagination safety property: the
// ONLY thing that ends a walk is a cursor that stops moving, because that is
// what would otherwise make the next call repeat this page forever.
func TestRecordsStopsOnANonAdvancingCursor(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	// more=true, but the cursor is identical to the one that was sent.
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r1"}],"more":true,"cursor_ms":100,"cursor_id":"r1"}`)
	service := newTestService(t, proxy, Limits{})

	page, err := service.records(context.Background(), RecordsInput{BeforeMs: 100, BeforeID: "r1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Exhausted {
		t.Fatal("a page whose cursor does not advance must be reported exhausted even when more is true")
	}
	if !page.More {
		t.Fatal("the proxy's more flag must be reported as received, so the model can see why paging stopped")
	}
}

// TestRecordsContinuesPastAnEmptyPage is the regression for the scan budget. The
// proxy advances its cursor on every row it SCANNED, including rows the scope
// filter excluded, so an empty page with an advanced cursor and more true is
// the ordinary shape of "the budget ran out before a match" - not the end of
// history. Reporting it as exhaustion silently drops every match behind it.
//
// The fixture is the byte-exact page the proxy's own handler test pins as one
// that must be continued (tests/_go/internal/web/log.go
// TestLogPageScopeBudgetAdvancesAcrossTimestampTies): page one is zero records,
// more true, cursor row-0007; page two from that cursor returns seven rows with
// more false. Both are asserted here, because the tool is what turns those two
// pages into one answer.
func TestRecordsContinuesPastAnEmptyPage(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	// The proxy's page one: the scope budget ran out before any target row.
	proxy.json(http.MethodGet, logPath,
		`{"records":[],"model_canon":{"revision":"r","names":{}},"more":true,"cursor_ms":1800000000000,"cursor_id":"row-0007"}`)
	service := newTestService(t, proxy, Limits{})

	first, err := service.records(context.Background(), RecordsInput{Scope: Scope{Filters: []string{"provider:target"}}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if first.Returned != 0 || !first.More {
		t.Fatalf("page one = %+v, want zero records with more true", first)
	}
	if first.Exhausted {
		t.Fatal("an empty page with an advanced cursor is NOT exhaustion: the proxy's own test pins it as a page that must be continued")
	}
	if first.NextBeforeID != "row-0007" || first.NextBeforeMs != 1800000000000 {
		t.Fatalf("the advanced cursor must be handed back for the next page: %+v", first)
	}

	// Page two, from that cursor, carries the rows. Paging stops there because
	// the proxy says there is nothing older.
	rows := make([]string, 0, 7)
	for i := range 7 {
		rows = append(rows, `{"id":"target-`+string(rune('a'+i))+`"}`)
	}
	proxy.json(http.MethodGet, logPath,
		`{"records":[`+strings.Join(rows, ",")+`],"more":false,"cursor_ms":1799999999000,"cursor_id":"target-a"}`)
	second, err := service.records(context.Background(), RecordsInput{
		Scope:    Scope{Filters: []string{"provider:target"}},
		BeforeMs: first.NextBeforeMs, BeforeID: first.NextBeforeID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Returned != 7 {
		t.Fatalf("the continuation lost rows: %+v", second)
	}
	// Exhausted is true here for the OTHER reason: the proxy reported no older
	// rows. That is a different fact from a scan budget that ran out, and both
	// must end the walk.
	if !second.Exhausted || second.More {
		t.Fatalf("more=false must end the walk: %+v", second)
	}
	// Both pages carried the same filter, and the second carried the cursor.
	requests := proxy.requestsFor(http.MethodGet, logPath)
	if len(requests) != 2 {
		t.Fatalf("expected two page calls, got %d", len(requests))
	}
	for _, request := range requests {
		if got := request.Query["f"]; len(got) != 1 || got[0] != "provider:target" {
			t.Fatalf("every page must carry the same scope: %q", got)
		}
	}
	if requests[0].Query.Get("before_id") != "" {
		t.Fatal("the first page has no cursor")
	}
	if requests[1].Query.Get("before_id") != "row-0007" || requests[1].Query.Get("before_ms") != "1800000000000" {
		t.Fatalf("the continuation must send the cursor page one returned: %q", requests[1].Query.Encode())
	}
}

// TestRecordsAdvancesWhileTheCursorMoves pins the other half: a real cursor
// keeps paging possible until the proxy says otherwise.
func TestRecordsAdvancesWhileTheCursorMoves(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r2"}],"more":true,"cursor_ms":90,"cursor_id":"r2"}`)
	service := newTestService(t, proxy, Limits{})
	page, err := service.records(context.Background(), RecordsInput{BeforeMs: 100, BeforeID: "r1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Exhausted {
		t.Fatal("an advancing cursor must not be reported exhausted")
	}
	if page.NextBeforeMs != 90 || page.NextBeforeID != "r2" {
		t.Fatalf("next cursor = %+v", page)
	}
	// And the returned cursor round-trips as the next request's cursor.
	if _, err := service.records(context.Background(), RecordsInput{BeforeMs: page.NextBeforeMs, BeforeID: page.NextBeforeID, Limit: 10}); err != nil {
		t.Fatal(err)
	}
	second := proxy.requestsFor(http.MethodGet, logPath)[1]
	assertQuery(t, second, "before_ms", "90")
	assertQuery(t, second, "before_id", "r2")
}

// TestChartCarriesBothPeriodStatBands pins the two period-wide [avg, min, max]
// arrays the proxy sends beside the percentiles. They were part of the payload
// all along and had no field, so ttft_stat and tps_stat were dropped silently:
// a model comparing an average against a median had only the percentiles to
// work with, and had no way to know the average existed.
func TestChartCarriesBothPeriodStatBands(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, chartPath, `{"now_ms":3000,"from_ms":1000,"bucket_ms":2000,`+
		`"ttft_p":[10.5,20.5,30.5],"tps_p":[4,5,6],"ttft_stat":[11,10,12],"tps_stat":[4.5,4,5],`+
		`"cost_per_mtok":null,"buckets":[{"t":1000,"ttft":[9,10,11]}]}`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.chart(context.Background(), ChartInput{Window: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if got := float64Values(out.TTFTStat); !slices.Equal(got, []float64{11, 10, 12}) {
		t.Fatalf("ttft_stat = %v, want the period [avg, min, max]", got)
	}
	if got := float64Values(out.TPSStat); !slices.Equal(got, []float64{4.5, 4, 5}) {
		t.Fatalf("tps_stat = %v, want the period [avg, min, max]", got)
	}
	// The percentiles still come through, and the two bands are independent.
	if got := float64Values(out.TTFTp); !slices.Equal(got, []float64{10.5, 20.5, 30.5}) {
		t.Fatalf("ttft_p = %v", got)
	}
}

// TestRecordsCarriesModelCanonAndStorage pins the page-level model_canon map and
// the storage signal. Grouping by the raw model column splits one canonical
// family across every spelling, and with storage off the log route scans nothing
// while the ring still holds history - so an empty page is not the end of it.
func TestRecordsCarriesModelCanonAndStorage(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r1"}],"model_canon":{"revision":"rev-9","names":`+
		`{"demo-latest":"demo","demo-preview":"demo"}},"more":false,"cursor_ms":10,"cursor_id":"r1"}`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.records(context.Background(), RecordsInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if out.ModelCanon == nil {
		t.Fatal("model_canon must reach the tool output: it is what folds raw spellings into one family")
	}
	if out.ModelCanon.Revision != "rev-9" || out.ModelCanon.Names["demo-latest"] != "demo" {
		t.Fatalf("model_canon = %+v", out.ModelCanon)
	}
	if !out.Storage.Enabled {
		t.Fatal("the storage signal must be reported, so a caller knows what this page is backed by")
	}
	if !strings.Contains(out.Note, "DURABLE history") {
		t.Fatalf("note = %q", out.Note)
	}

	// With storage off the route answers an empty page with more false, which
	// would otherwise read as the end of history.
	proxy.json(http.MethodGet, bootstrapPath, storageOffBootstrap)
	proxy.json(http.MethodGet, logPath, `{"records":[],"more":false,"cursor_ms":0,"cursor_id":""}`)
	blind, err := service.records(context.Background(), RecordsInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if blind.Returned != 0 || !blind.Exhausted {
		t.Fatalf("a store-less page is still exhaustible: %+v", blind)
	}
	if blind.Storage.Enabled {
		t.Fatal("storage must be reported false")
	}
	for _, needle := range []string{"NOT the end of history", "snapshot"} {
		if !strings.Contains(blind.Note, needle) {
			t.Fatalf("the note must say %q, got %q", needle, blind.Note)
		}
	}
}

// TestDescribeVocabulariesAreTheProxyOnes pins the value vocabulary describe
// advertises. It used to list a 3xx status class the proxy never produces, so
// `f=status:3xx` was a silently empty filter, and it said nothing about the
// `time` dimension at all - which is a daypart bucket, so `f=time:24h` was a
// silently empty filter too.
func TestDescribeVocabulariesAreTheProxyOnes(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, schemaPath, `[]`)
	service := newTestService(t, proxy, Limits{})

	out, err := service.describe(context.Background(), DescribeInput{})
	if err != nil {
		t.Fatal(err)
	}
	byDimension := map[string]VocabularyDoc{}
	for _, vocabulary := range out.Vocabularies {
		byDimension[vocabulary.Dimension] = vocabulary
	}
	// Exactly the classes internal/web statusClass can return: a 3xx never
	// reaches a recorded row, 499 is its own class, and a status under 200 is
	// err. See tests/_go/internal/web/aggregate.go TestStatusClassAndBuckets for
	// the other side of this contract.
	want := []string{"2xx", "cancel", "4xx", "5xx", "err"}
	if got := byDimension["status"].Values; len(got) != len(want) {
		t.Fatalf("status classes = %v, want exactly %v", got, want)
	}
	for i, class := range want {
		if byDimension["status"].Values[i] != class {
			t.Fatalf("status classes = %v, want exactly %v", byDimension["status"].Values, want)
		}
	}
	if slices.Contains(byDimension["status"].Values, "3xx") {
		t.Fatal("there is no 3xx status class: advertising one makes f=status:3xx a silently empty filter")
	}
	if got, want := byDimension["time"].Values, TimeBuckets; !slices.Equal(got, want) {
		t.Fatalf("time values = %v, want the daypart buckets %v", got, want)
	}
	if !strings.Contains(byDimension["time"].Note, "NOT a duration") {
		t.Fatalf("the time vocabulary must say it is a bucket, not a duration: %q", byDimension["time"].Note)
	}
	if got, want := byDimension["error"].Values, ErrorFilterKeys; !slices.Equal(got, want) {
		t.Fatalf("error keys = %v, want %v", got, want)
	}
	if !strings.Contains(byDimension["error"].Note, "type|code|message") {
		t.Fatalf("the error grammar must be stated: %q", byDimension["error"].Note)
	}
	// The parts are NOT guaranteed non-empty: errorKey joins typ|code|msg, and
	// keys like server_error|500| and other|| exist.
	if !strings.Contains(byDimension["error"].Note, "CAN be empty") {
		t.Fatalf("the error vocabulary must say a part can be empty: %q", byDimension["error"].Note)
	}
	if strings.Contains(byDimension["error"].Note, "never has an empty part") {
		t.Fatalf("the error vocabulary still claims the false non-empty guarantee: %q", byDimension["error"].Note)
	}
	// A 3xx is not a class of its own; statusClass maps it into err.
	if !strings.Contains(byDimension["status"].Note, "3xx") || strings.Contains(byDimension["status"].Note, "below 200 is 'err'") {
		t.Fatalf("the status vocabulary must say 3xx and below 200 both fall into err: %q", byDimension["status"].Note)
	}
	if !strings.Contains(byDimension["live_statuses"].Note, "s= selector") {
		t.Fatalf("the s= vocabulary must be distinguished from the status dimension: %q", byDimension["live_statuses"].Note)
	}
	if !strings.Contains(byDimension["model"].Note, "CANONICAL") {
		t.Fatalf("the model vocabulary must say the scope filter takes the canonical spelling: %q", byDimension["model"].Note)
	}
	for _, dimension := range []string{"client", "provider", "model", "conversation", "key", "tool"} {
		if _, ok := byDimension[dimension]; !ok {
			t.Fatalf("describe must state the vocabulary of %q too", dimension)
		}
		if !strings.Contains(byDimension[dimension].Note, "values tool") {
			t.Fatalf("%q must point at the values tool: %q", dimension, byDimension[dimension].Note)
		}
	}
	// Every explorer dimension has a vocabulary entry, so none is undiscoverable.
	if len(out.Vocabularies) != len(Dimensions)+1 {
		t.Fatalf("vocabularies = %d, want one per dimension plus the live selector", len(out.Vocabularies))
	}
}

// TestDescribeDocumentsEveryTrackedRequestParam derives the presence-tracked
// columns from the durable owner - storage.RequestParamColumns(), the bit
// mapping in internal/storage/request_params.go whose DDL lives in store.go -
// rather than a second hand-written list. A column added to presence tracking
// therefore cannot be missing from the reference, and the old blanket claim
// that every req_* zero was indistinguishable from absent cannot come back.
func TestDescribeDocumentsEveryTrackedRequestParam(t *testing.T) {
	documented := map[string]string{}
	var notes []string
	for _, table := range tableDocs {
		if table.Table != "requests" {
			continue
		}
		for _, column := range table.Columns {
			documented[column.Name] = column.Meaning
		}
		notes = append(notes, table.Notes...)
	}
	note := strings.Join(notes, " ")
	if !strings.Contains(note, "req_param_presence") {
		t.Fatalf("the requests note must state the presence mask: %q", note)
	}
	if strings.Contains(note, "a 0 cannot be distinguished from an absent one") {
		t.Fatalf("the note still carries the false blanket zero claim: %q", note)
	}
	if !strings.Contains(note, "NEGATIVE") && !strings.Contains(note, "negative") {
		t.Fatalf("the note must state the legacy negative mask: %q", note)
	}
	tracked := storage.RequestParamColumns()
	if len(tracked) == 0 {
		t.Fatal("the storage owner lists no presence-tracked columns")
	}
	seen := map[string]bool{}
	for _, column := range tracked {
		if seen[column] {
			t.Fatalf("duplicate presence-tracked column %q", column)
		}
		seen[column] = true
		meaning, ok := documented[column]
		if !ok {
			t.Fatalf("describe is missing the presence-tracked column %q", column)
		}
		if !strings.Contains(meaning, "req_param_presence") {
			t.Fatalf("the meaning of %q must name the presence mask: %q", column, meaning)
		}
		if !strings.Contains(note, column) {
			t.Fatalf("the presence note must name the tracked column %q: %q", column, note)
		}
	}
	if _, ok := documented["req_param_presence"]; !ok {
		t.Fatal("describe must document the req_param_presence column itself")
	}
}

// TestDescribeStatesTheStructuralLimitsAndTheSQLIdiom pins the rules that send a
// model down the wrong path if it does not know them: this build has no unixnow,
// explore has no window and no cross-tabs, chart cannot group, and the
// server-side group and bucket caps are invisible to the tool's own truncation
// flag.
func TestDescribeStatesTheStructuralLimitsAndTheSQLIdiom(t *testing.T) {
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, bootstrapPath, minimalBootstrap)
	proxy.json(http.MethodGet, debugPath, emptyDebugStatus)
	proxy.json(http.MethodGet, schemaPath, `[]`)
	service := newTestService(t, proxy, Limits{})
	out, err := service.describe(context.Background(), DescribeInput{})
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(out.Notes, " ")
	for _, needle := range []string{
		"NO unixnow() function",
		"strftime('%s','now')*1000",
		"explore has NO time window",
		"cannot cross-tabulate",
		"chart is the only windowed tool and cannot group",
		"query is the only tool that can express a time range",
		"SERVER-SIDE",
		// The snapshot window pointer must name the tool, not a raw HTTP route
		// the model has no way to call.
		"use the records tool",
	} {
		if !strings.Contains(notes, needle) {
			t.Fatalf("describe notes must state %q, got %q", needle, notes)
		}
	}
	if strings.Contains(notes, "/metrics/agg/log") {
		t.Fatalf("describe must not send the model to a raw HTTP route: %q", notes)
	}
	// The index list is a planner reference, not prose: it names the two real
	// request_debug indexes and does not claim there is no index on started_at
	// alone (idx_requests_log leads on it).
	indexes := strings.Join(out.Indexes, " ")
	for _, needle := range []string{
		"idx_requests_log(started_at, id)",
		"idx_request_debug_expires(expires_at)",
		"idx_request_debug_session(session_id)",
		"NO index on status_code, error_type or cost",
	} {
		if !strings.Contains(indexes, needle) {
			t.Fatalf("the index reference must state %q, got %q", needle, indexes)
		}
	}
	for _, forbidden := range []string{"request_debug(payload)", "started_at alone"} {
		if strings.Contains(indexes, forbidden) {
			t.Fatalf("the index reference still contains %q: %q", forbidden, indexes)
		}
	}
	// Every advertised result limit is present, including the ones the tool
	// enforces itself rather than the proxy.
	if out.Limits.QueryMaxRowsCeiling != maxQueryRowsCeiling ||
		out.Limits.PrometheusMaxLines != prometheusMaxLines ||
		out.Limits.DefaultPageSize != DefaultLimits().PageSize ||
		out.Limits.CaptureMaxBytes != DefaultLimits().CaptureBytes {
		t.Fatalf("the limits must include the self-enforced caps: %+v", out.Limits)
	}
	if out.Limits.QueryMaxBytes != DefaultLimits().QueryMaxBytes {
		t.Fatalf("the encoded-size clamp must be in the limits: %+v", out.Limits)
	}
	// The complete column reference, so an analysis never has to guess a name.
	body, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{
		"cache_write_tokens", "turns_user", "chars_tool", "response_headers",
		"prompt_preview", "response_preview", "client_ip", "client_lang", "method",
		"rate_limit_remaining", "rate_limit_limit", "had_answer_content", "answer_tokens",
		"provider_request_id", "provider_server", "req_max_tokens", "req_top_p",
		"req_service_tier", "req_reasoning_effort",
		"first_token_at", "last_token_at", "first_answer_at",
	} {
		if !strings.Contains(string(body), column) {
			t.Fatalf("the column reference is missing %q", column)
		}
	}
	// Live tool names come from the records the snapshot already carried, at no
	// extra request.
	if len(out.KnownTools) != 0 {
		t.Fatalf("a snapshot with no records names no tools: %v", out.KnownTools)
	}
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":true},`+
		`"records":[{"id":"n1","tool_names":["read_file","grep"]},{"id":"n2","tool_names":["grep"]}]}`)
	tools, err := service.describe(context.Background(), DescribeInput{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(tools.KnownTools, []string{"grep", "read_file"}) {
		t.Fatalf("known_tools = %v, want the live names in a stable order", tools.KnownTools)
	}
}

// TestQueryClampsEncodedSize pins that a query result is bounded by VOLUME as
// well as by rows. A 200-row SELECT * of the requests table is well over 100k
// tokens in one result, and the row cap alone would hand all of it to a model.
func TestQueryClampsEncodedSize(t *testing.T) {
	rows := make([]string, 0, 10)
	for i := range 10 {
		rows = append(rows, `{"id":"r`+string(rune('a'+i))+`","blob":"`+strings.Repeat("x", 200)+`"}`)
	}
	proxy := newFakeProxy(t)
	proxy.json(http.MethodGet, schemaPath, `[`+strings.Join(rows, ",")+`]`)

	limits := DefaultLimits()
	limits.QueryMaxBytes = 500
	service := newTestService(t, proxy, limits)

	out, err := service.query(context.Background(), QueryInput{SQL: "SELECT * FROM requests"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowCount >= 10 {
		t.Fatalf("the byte clamp must cut a wide result: %d of 10 rows survived", out.RowCount)
	}
	if !out.Truncation.Truncated || out.Truncation.Total != 10 {
		t.Fatalf("truncation = %+v, want 10 produced", out.Truncation)
	}
	if !strings.Contains(out.Truncation.Marker, "byte result budget") || !strings.Contains(out.Truncation.Marker, "narrow the SELECT") {
		t.Fatalf("marker = %q, must name the byte budget and the way to continue", out.Truncation.Marker)
	}
	if out.Bytes <= 0 || out.Bytes > 500 {
		t.Fatalf("bytes = %d, want the clamped encoded size", out.Bytes)
	}
	// Every surviving row is a whole row: the result stays valid JSON.
	for _, row := range out.Rows {
		if _, err := json.Marshal(row); err != nil {
			t.Fatalf("a clamped row must stay encodable: %v", err)
		}
		if row["blob"] == nil {
			t.Fatal("a row was cut inside: the clamp must keep whole rows only")
		}
	}

	// A result inside the byte budget reports no truncation at all.
	roomy := newFakeProxy(t)
	roomy.json(http.MethodGet, schemaPath, `[`+strings.Join(rows, ",")+`]`)
	limits.QueryMaxBytes = 1 << 20
	fit, err := newTestService(t, roomy, limits).query(context.Background(), QueryInput{SQL: "SELECT * FROM requests"})
	if err != nil {
		t.Fatal(err)
	}
	if fit.Truncation.Truncated || fit.RowCount != 10 {
		t.Fatalf("a fitting result must not claim truncation: %+v", fit)
	}
}

// TestTruncationAdviceIsPerTool pins that each tool's marker names a continuation
// that tool actually has. The shared wording told a model to "page with the
// cursor fields" for tools that have no cursor, which sends it looking for
// arguments that do not exist.
func TestTruncationAdviceIsPerTool(t *testing.T) {
	proxy := newFakeProxy(t)
	groups := make([]string, 0, explorerMaxGroups+2)
	for i := range explorerMaxGroups + 2 {
		groups = append(groups, `{"name":"g`+string(rune('a'+i))+`"}`)
	}
	proxy.json(http.MethodGet, explorerPath, `{"dim":"provider","groups":[`+strings.Join(groups, ",")+`]}`)
	buckets := make([]string, 0, chartMaxBuckets+2)
	for i := range chartMaxBuckets + 2 {
		buckets = append(buckets, `{"t":`+strconv.Itoa(i)+`}`)
	}
	proxy.json(http.MethodGet, chartPath, `{"bucket_ms":1,"buckets":[`+strings.Join(buckets, ",")+`]}`)
	records := make([]string, 0, 12)
	for i := range 12 {
		records = append(records, `{"id":"r`+string(rune('a'+i))+`"}`)
	}
	proxy.json(http.MethodGet, bootstrapPath, `{"seq":1,"feed_id":"f","counters":{},"storage":{"enabled":true},`+
		`"records":[`+strings.Join(records, ",")+`]}`)
	proxy.json(http.MethodGet, logPath, `{"records":[`+strings.Join(records, ",")+`],"more":false,"cursor_ms":5,"cursor_id":"rl"}`)
	limits := DefaultLimits()
	limits.PageSize = 5
	service := newTestService(t, proxy, limits)
	ctx := context.Background()

	explorer, err := service.explore(ctx, ExploreInput{Dim: "provider"})
	if err != nil {
		t.Fatal(err)
	}
	chart, err := service.chart(ctx, ChartInput{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := service.records(ctx, RecordsInput{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.snapshot(ctx, SnapshotInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		tool   string
		marker string
		want   string
	}{
		{"explore", explorer.Truncation.Marker, "GROUP BY"},
		{"chart", chart.Truncation.Marker, "shorter window"},
		{"records", page.Truncation.Marker, "next_before_ms"},
		{"snapshot", snapshot.Truncation.Marker, "records tool"},
	} {
		if !strings.Contains(tc.marker, tc.want) {
			t.Fatalf("%s marker %q must name %q, its own continuation", tc.tool, tc.marker, tc.want)
		}
		if strings.Contains(tc.marker, "cursor fields") {
			t.Fatalf("%s marker %q must not use the shared cursor wording", tc.tool, tc.marker)
		}
	}
}

// float64Values unwraps a percentile band for comparison, so the assertion is
// about the numbers and not about pointer identity.
func float64Values(band []*float64) []float64 {
	out := make([]float64, 0, len(band))
	for _, value := range band {
		if value == nil {
			out = append(out, 0)
			continue
		}
		out = append(out, *value)
	}
	return out
}
