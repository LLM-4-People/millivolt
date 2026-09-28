package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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
	proxy.json(http.MethodGet, logPath, `{"records":[{"id":"r1","status_code":200}],"model_canon":{},"more":false,"cursor_ms":1500,"cursor_id":"r1"}`)
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
// proxy scans a bounded number of rows per call, so a page can be short or
// empty while more is still true. The tool must report exhaustion instead of
// inviting an endless loop.
func TestRecordsStopsOnANonAdvancingCursor(t *testing.T) {
	proxy := newFakeProxy(t)
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

	// A page that returns nothing is exhaustion too, whatever more claims.
	proxy.json(http.MethodGet, logPath, `{"records":[],"more":true,"cursor_ms":100,"cursor_id":"r1"}`)
	empty, err := service.records(context.Background(), RecordsInput{BeforeMs: 100, BeforeID: "r1", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Exhausted {
		t.Fatal("an empty page must be reported exhausted")
	}
}

// TestRecordsAdvancesWhileTheCursorMoves pins the other half: a real cursor
// keeps paging possible until the proxy says otherwise.
func TestRecordsAdvancesWhileTheCursorMoves(t *testing.T) {
	proxy := newFakeProxy(t)
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
