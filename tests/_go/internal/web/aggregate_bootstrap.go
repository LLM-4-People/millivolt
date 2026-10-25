package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"fmt"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestExplorerModelDimensionCanonicalized: the model dimension groups by the
// CANONICAL spelling (config.CanonicalModel applied at the contrib choke
// point), and a canonical model filter matches every raw variant - the
// server-side half of the contract the client mirrors in recordMatchesDim.
func TestExplorerModelDimensionCanonicalized(t *testing.T) {
	buf := metrics.NewBuffer(16)
	now := time.Now()
	// Three raw spellings of the same canonical model: glm-5.3 twice and
	// glm-5-3 once. Set the model before Record so the ring owns a finalized
	// record, never a value mutated after enqueue.
	models := []string{"glm-5.3", "glm-5.3", "glm-5-3"}
	for i, rec := range []*metrics.Record{
		mkRec("a", now.Add(-3*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
		mkRec("b", now.Add(-2*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
		mkRec("c", now.Add(-1*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0),
	} {
		rec.Model = models[i]
		buf.Record(rec)
	}

	agg := NewAggAPI(buf, nil, time.Second)
	agg.ModelCanon = func() config.ModelCanon { return config.Default().ModelCanon() }

	p := get(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model")
	groups := p["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("model groups = %d, want 1 (all three spellings canonicalize to glm-5-3)", len(groups))
	}
	g := groups[0].(map[string]any)
	if g["name"] != "glm-5-3" || g["n"] != float64(3) {
		t.Fatalf("group = %v, want name=glm-5-3 n=3", g)
	}
	if rail := p["rail"].(map[string]any); rail["model"] != float64(1) {
		t.Fatalf("rail model count = %v, want 1 canonical group", rail["model"])
	}

	// A canonical model filter matches every raw variant of the group.
	ps := getExplorerScope(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model&f=model:glm-5-3")
	if ps["matches"] != float64(3) {
		t.Fatalf("scoped matches = %v, want 3 (both raw spellings match the canonical filter)", ps["matches"])
	}
}

// TestBootstrapCarriesModelCanon: the bootstrap payload ships the effective
// rules and server-derived name dictionary so clients need no regex mirror.
// An absent provider explicitly supplies identity semantics.
func TestBootstrapCarriesModelCanon(t *testing.T) {
	buf := metrics.NewBuffer(4)
	agg := NewAggAPI(buf, nil, time.Second)
	agg.ModelCanon = func() config.ModelCanon {
		return config.ModelCanon{Rules: []config.ModelRule{{Mode: config.ModelRuleLower}}}
	}
	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	mc, ok := p["model_canon"].(map[string]any)
	if !ok {
		t.Fatalf("model_canon section = %v, want present", p["model_canon"])
	}
	rules, ok := mc["rules"].([]any)
	if !ok || len(rules) != 1 {
		t.Fatalf("model_canon rules = %v, want the provider's single lower rule", mc["rules"])
	}
	if rule := rules[0].(map[string]any); rule["mode"] != "lower" {
		t.Fatalf("model_canon rules = %v, want mode lower", rules)
	}

	agg2 := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	p2 := get(t, http.HandlerFunc(agg2.HandleBootstrap), "/metrics/bootstrap")
	if mc, ok := p2["model_canon"].(map[string]any); !ok || mc["revision"] == "" {
		t.Fatal("raw grouping requires explicit observer identity")
	}
}

// TestBootstrapCarriesRestartEligibility: the boot payload carries the
// restarter's eligibility so a fresh page knows whether to OFFER the Restart
// control before the menu is ever opened - no extra round trip, and a build
// whose only possible outcome is a refusal never shows the action. The section
// carries the two eligibility fields GET /admin/restart also reports and
// deliberately none of that document's live state (phase, pid, error, rank,
// drain timings), so it cannot drift into a second source of restart truth; and
// an unwired provider omits it entirely (the browser then keeps the current
// behaviour rather than hiding a working action).
func TestBootstrapCarriesRestartEligibility(t *testing.T) {
	const trimpath = "built with -trimpath, so the binary cannot locate its own source; " +
		"self-restart is unavailable (restart via your process manager)"
	for _, tc := range []struct {
		name      string
		doc       any
		available bool
		reason    string
	}{
		{name: "eligible", doc: map[string]any{"available": true, "reason": ""}, available: true},
		{name: "ineligible", doc: map[string]any{"available": false, "reason": trimpath}, reason: trimpath},
	} {
		agg := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
		agg.Restart = func() any { return tc.doc }
		p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
		section, ok := p["restart"].(map[string]any)
		if !ok {
			t.Fatalf("%s: restart section = %v, want the eligibility document", tc.name, p["restart"])
		}
		if len(section) != 2 || section["available"] != tc.available || section["reason"] != tc.reason {
			t.Errorf("%s: restart section = %v, want available=%v with the server's own reason", tc.name, section, tc.available)
		}
	}

	// Unwired (a build without the provider) omits the section rather than
	// claiming an eligibility nobody computed.
	agg := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	p := get(t, http.HandlerFunc(agg.HandleBootstrap), "/metrics/bootstrap")
	if _, ok := p["restart"]; ok {
		t.Errorf("bootstrap carried a restart section with no provider: %v", p["restart"])
	}
}

// TestCanonicalModelScopeAppliesEverywhere: the canonical model name is
// applied at the LOWEST server read choke point - contrib construction
// (fromRecord/fromPending/scanContrib all run the modelCanonizer) - so
// every grouped/matched surface downstream of it agrees: a scope filter
// carrying the CANONICAL spelling (what a node-card or saved hash sends)
// must match records stored under RAW spellings. This pins the
// STORE-backed paths (scanContrib): explorer grouping over the store,
// scope counts, chart buckets, and log paging - the ring path
// (fromRecord) is pinned by TestExplorerModelDimensionCanonicalized.
func TestCanonicalModelScopeAppliesEverywhere(t *testing.T) {
	s := testStore(t)
	buf := metrics.NewBuffer(16)
	// Scoping covers the whole requested window, not just the newest bucket.
	// Keep this fixture away from the current bucket deterministically.
	now := time.Now().Add(-5 * time.Minute)
	raw := []string{"glm-5.3", "glm-5-3", "GLM-5.3", "grok-4.6"}
	for i, m := range raw {
		rec := mkRec(fmt.Sprintf("r%d", i), now.Add(-time.Duration(i+1)*time.Second), 200, "", nil, 10, 100, 10, 5, 0, 0)
		rec.Model = m
		buf.Record(rec)
		s.Record(rec)
	}
	waitTotals(t, s, 4)
	agg := NewAggAPI(buf, s, time.Second)
	agg.ModelCanon = func() config.ModelCanon { return config.Default().ModelCanon() }

	// The store scan itself groups canonically: one glm-5-3 card, not three.
	p := get(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model")
	groups := p["groups"].([]any)
	if len(groups) != 2 || groups[0].(map[string]any)["name"] != "glm-5-3" {
		t.Fatalf("store explorer groups = %v, want 2 with glm-5-3 merged first", groups)
	}

	// Scope: canonical filter matches all three raw spellings (scanContrib).
	ps := getExplorerScope(t, http.HandlerFunc(agg.HandleAggExplorer), "/metrics/agg/explorer?dim=model&f=model:glm-5-3")
	if ps["matches"] != float64(3) {
		t.Fatalf("scoped matches = %v, want 3 over the store", ps["matches"])
	}

	// Chart: the scoped bucket count folds the raw-spelled rows.
	pc := get(t, http.HandlerFunc(agg.HandleAggChart), "/metrics/agg/chart?window=60&f=model:glm-5-3")
	buckets, ok := pc["buckets"].([]any)
	if !ok || len(buckets) == 0 {
		t.Fatalf("chart buckets missing: %v", pc["buckets"])
	}
	var req float64
	for _, bucket := range buckets {
		req += bucket.(map[string]any)["req"].(float64)
	}
	if req != 3 {
		t.Fatalf("scoped chart req = %v, want 3", req)
	}

	// Log paging: the scoped archive returns the raw-spelled rows.
	before := now.Add(time.Millisecond).UnixMilli()
	pl := get(t, http.HandlerFunc(agg.HandleLogPage), fmt.Sprintf("/metrics/agg/log?before_ms=%d&before_id=&limit=10&s=&f=model:glm-5-3", before))
	rows, ok := pl["records"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("scoped log rows = %d, want 3 (raw spellings matched by the canonical filter)", pl["records"])
	}
}

// TestBootstrapWireKeysStrict pins the dashboard bootstrap payload's exact
// wire key set with a DisallowUnknownFields decoder through the real
// handler and marshaling path. The W15 drop removed the KPI's answer_tokens
// and tool_calls and the snapshot's oldest_seq/buffer_size; the round-seven
// mutation round proved a re-added or renamed key went unnoticed. Nested
// records stay raw (their key set is the record contract, pinned by the
// storage/log suites); the envelope keys and the KPI section are the pinned
// surface. The kpi section is decoded raw and its key set asserted equal to
// kpiWireKeys (exact equality - stricter than DisallowUnknownFields, which
// alone would not notice a missing key), then typed for the value checks.
// The same decode also pins the two identity widths the browser
// regexes assert: dashboard_version is 16 lowercase hex (web.go etagHex,
// mirrored by live.js DASHBOARD_VERSION_RE) and the model_canon revision
// is 64 lowercase hex (aggregate_observer.go sha256, mirrored by
// explorer.js MODEL_REVISION_RE).
// TestBootstrapFieldsViews pins the closed probe views used by in-process MCP
// tools: each returns only its named section, while the default bootstrap
// remains the complete dashboard document.
func TestBootstrapFieldsViews(t *testing.T) {
	buf := metrics.NewBuffer(8)
	buf.Record(mkRec("b1", time.Now(), 200, "", nil, 10, 100, 10, 5, 0, 0.5))
	agg := NewAggAPI(buf, nil, time.Second)
	get := func(query string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		http.HandlerFunc(agg.HandleBootstrap).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("bootstrap%s status = %d body=%s", query, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode bootstrap%s: %v", query, err)
		}
		return out
	}
	storage := get("?fields=storage")
	if len(storage) != 1 || storage["storage"] == nil {
		t.Fatalf("fields=storage keys = %v", storage)
	}
	live := get("?fields=live")
	if live["records"] == nil || live["storage"] == nil {
		t.Fatalf("fields=live missing records/storage: %v", live)
	}
	for _, absent := range []string{"kpi", "dashboard_version", "dash", "pause", "debug", "storm", "restart"} {
		if _, ok := live[absent]; ok {
			t.Fatalf("fields=live unexpectedly carries %q", absent)
		}
	}
	canon := get("?fields=model_canon")
	if len(canon) != 1 || canon["model_canon"] == nil {
		t.Fatalf("fields=model_canon keys = %v", canon)
	}
	full := get("")
	if full["kpi"] == nil || full["dashboard_version"] == nil || full["records"] == nil {
		t.Fatalf("default bootstrap lost a dashboard section: %v", full)
	}
	for _, query := range []string{"?fields=bogus", "?fields=storage&fields=live"} {
		rec := httptest.NewRecorder()
		http.HandlerFunc(agg.HandleBootstrap).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap"+query, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bootstrap%s status = %d, want 400", query, rec.Code)
		}
	}
}

func TestBootstrapWireKeysStrict(t *testing.T) {
	buf := metrics.NewBuffer(8)
	now := time.Now()
	buf.Record(mkRec("b1", now, 200, "", nil, 10, 100, 10, 5, 0, 0.5))
	live := mkRec("b2", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	live.Stream = true
	buf.PublishLive("begin", live)
	agg := NewAggAPI(buf, nil, time.Second)

	rec := httptest.NewRecorder()
	http.HandlerFunc(agg.HandleBootstrap).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/bootstrap", nil))
	if rec.Code != 200 {
		t.Fatalf("bootstrap status = %d body=%s", rec.Code, rec.Body.String())
	}
	var wire struct {
		Records         json.RawMessage `json:"records"`
		InFlightRecords json.RawMessage `json:"in_flight_records"`
		PendingRevision uint64          `json:"pending_revision"`
		Counters        struct {
			InFlight int64 `json:"in_flight"`
			TotalReq int64 `json:"total_requests"`
			TotalErr int64 `json:"total_errors"`
		} `json:"counters"`
		Seq              int64           `json:"seq"`
		FeedID           string          `json:"feed_id"`
		Incremental      bool            `json:"incremental"`
		ModelCanon       json.RawMessage `json:"model_canon"`
		DashboardVersion string          `json:"dashboard_version"`
		KPI              json.RawMessage `json:"kpi"`
		Storage          struct {
			Enabled        bool   `json:"enabled"`
			Dropped        uint64 `json:"dropped"`
			TotalsDegraded bool   `json:"totals_degraded"`
		} `json:"storage"`
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		t.Fatalf("strict decode: %v\n%s", err, rec.Body.String())
	}
	var kpiObj map[string]json.RawMessage
	if err := json.Unmarshal(wire.KPI, &kpiObj); err != nil {
		t.Fatalf("kpi section is not a JSON object: %v (%s)", err, wire.KPI)
	}
	served := make([]string, 0, len(kpiObj))
	for k := range kpiObj {
		served = append(served, k)
	}
	slices.Sort(served)
	want := slices.Clone(kpiWireKeys)
	slices.Sort(want)
	if !slices.Equal(served, want) {
		t.Fatalf("bootstrap kpi keys = %v, want the pinned wire set %v", served, want)
	}
	var kpi struct {
		Requests    int64    `json:"requests"`
		Errors      int64    `json:"errors"`
		InFlight    int64    `json:"in_flight"`
		Cost        float64  `json:"cost"`
		CostPerReq  *float64 `json:"cost_per_req"`
		CostPerMTok *float64 `json:"cost_per_mtok"`
		InputTok    int64    `json:"input_tokens"`
		OutputTok   int64    `json:"output_tokens"`
		CacheRead   int64    `json:"cache_read_tokens"`
		Reasoning   int64    `json:"reasoning_tokens"`
		AvgTTFT     *float64 `json:"avg_ttft_ms"`
		AvgTPS      *float64 `json:"avg_tps"`
	}
	if err := json.Unmarshal(wire.KPI, &kpi); err != nil {
		t.Fatalf("kpi value decode: %v (%s)", err, wire.KPI)
	}
	if kpi.Requests != 1 || kpi.InputTok != 10 || kpi.OutputTok != 5 || kpi.Cost != 0.5 {
		t.Fatalf("kpi = %+v, want the folded ring record", kpi)
	}
	if kpi.CostPerReq == nil || *kpi.CostPerReq != 0.5 {
		t.Fatalf("kpi cost_per_req = %v, want the folded 0.5 blended rate", kpi.CostPerReq)
	}
	if kpi.AvgTTFT == nil || *kpi.AvgTTFT != 10 {
		t.Fatalf("kpi avg_ttft_ms = %v, want the folded 10ms mean", kpi.AvgTTFT)
	}
	if kpi.AvgTPS != nil {
		t.Fatalf("kpi avg_tps = %v, want nil with no captured throughput sample", kpi.AvgTPS)
	}
	if !hexRe16.MatchString(wire.DashboardVersion) {
		t.Fatalf("dashboard_version = %q, want 16 lowercase hex (web.go etagHex width; live.js DASHBOARD_VERSION_RE mirrors it)", wire.DashboardVersion)
	}
	var canon struct {
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(wire.ModelCanon, &canon); err != nil {
		t.Fatalf("model_canon: %v", err)
	}
	if !hexRe64.MatchString(canon.Revision) {
		t.Fatalf("model_canon revision = %q, want 64 lowercase hex (sha256; explorer.js MODEL_REVISION_RE mirrors it)", canon.Revision)
	}
}
