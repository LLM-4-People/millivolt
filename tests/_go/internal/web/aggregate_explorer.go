package web

import (
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestExplorerBreakdown(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("p1", now.Add(-2*time.Hour), 200, "", nil, 100, 500, 10, 5, 0, 0.01))
	s.Record(mkRec("p2", now.Add(-time.Hour), 200, "", nil, 200, 600, 20, 10, 0, 0.02))
	s.Record(mkRec("p3", now.Add(-time.Minute), 500, "boom", nil, 0, 700, 30, 15, 0, 0.03))
	s.Record(mkRec("p4", now, 200, "", []metrics.RetryAttempt{{StatusCode: 502, ErrorType: "x"}}, 50, 800, 40, 20, 0, 0.04))
	waitTotals(t, s, 4)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")
	if p["total"] != float64(4) {
		t.Fatalf("total = %v, want 4", p["total"])
	}
	groups := p["groups"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["name"] != "p" {
		t.Fatalf("provider groups = %v", groups)
	}
	g := groups[0].(map[string]any)
	if g["n"] != float64(4) || g["err_final"] != float64(2) { // p3 final 500 + p4 absorbed 502
		t.Fatalf("provider agg = %v", g)
	}
	// Per-entity blended $/Mtok (cost-reporting tokens only; all 4 records
	// report cost): $0.10 over 150 in+out tokens.
	if got := g["cost_per_mtok"].(float64); math.Abs(got-0.10/150*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.10/150*1e6)
	}
	rail := p["rail"].(map[string]any)
	if rail["provider"] != float64(1) || rail["status"] != float64(2) { // 2xx + 5xx
		t.Fatalf("rail = %v", rail)
	}

	// Error dimension: p3's final boom + p4's absorbed "x" are two signatures.
	pe := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=error")
	egroups := pe["groups"].([]any)
	if len(egroups) != 2 {
		t.Fatalf("error groups = %d, want 2", len(egroups))
	}
	for _, e := range egroups {
		m := e.(map[string]any)
		if m["err_events"] != float64(1) || m["n"] != float64(1) {
			t.Fatalf("error group = %v", m)
		}
	}

	// Status dimension classes.
	ps := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status")
	names := map[string]float64{}
	for _, e := range ps["groups"].([]any) {
		m := e.(map[string]any)
		names[m["name"].(string)] = m["n"].(float64)
	}
	if names["2xx"] != 3 || names["5xx"] != 1 {
		t.Fatalf("status groups = %v", names)
	}

	// Cross-filter: selecting Status→2xx must NOT collapse the gallery.
	px := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status&f=status:2xx")
	if px["total"] != float64(4) {
		t.Fatalf("status self-filter total = %v, want 4 (all records)", px["total"])
	}
	xnames := map[string]float64{}
	for _, e := range px["groups"].([]any) {
		m := e.(map[string]any)
		xnames[m["name"].(string)] = m["n"].(float64)
	}
	if xnames["2xx"] != 3 || xnames["5xx"] != 1 {
		t.Fatalf("status self-filter groups = %v, want 2xx=3 5xx=1", xnames)
	}
	xrail := px["rail"].(map[string]any)
	if xrail["status"] != float64(1) {
		t.Fatalf("rail status after 2xx filter = %v, want 1 (full filter)", xrail)
	}
}

// TestExplorerErrorTotalAndCardNShareAffectedRequestUnit pins the error
// gallery's share denominator and numerator to the same unit: error_total and
// card N both count affected requests, while ErrEvents keeps counting failure
// occurrences (final plus absorbed). A request with several absorbed 500s must
// never make the card's share exceed the gallery total.
func TestExplorerErrorTotalAndCardNShareAffectedRequestUnit(t *testing.T) {
	s := testStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	s.Record(&metrics.Record{
		ID: "multi", Provider: "p", Model: "m", Start: base, StatusCode: 200,
		Attempts: []metrics.RetryAttempt{
			{StatusCode: 500, ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "multi", At: base.Add(-3 * time.Second)},
			{StatusCode: 500, ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "multi", At: base.Add(-2 * time.Second)},
			{StatusCode: 500, ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "multi", At: base.Add(-time.Second)},
		},
	})
	s.Record(&metrics.Record{
		ID: "single", Provider: "p", Model: "m", Start: base.Add(time.Second), StatusCode: 500,
		ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "single",
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(1), s, time.Second)
	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=error")
	if p["error_total"] != float64(2) {
		t.Fatalf("error_total = %v, want 2 affected requests", p["error_total"])
	}
	groups := p["groups"].([]any)
	if len(groups) != 2 {
		t.Fatalf("error groups = %d, want 2 error signatures", len(groups))
	}
	wantEvents := map[string]float64{"upstream|500|multi": 3, "upstream|500|single": 1}
	for _, raw := range groups {
		g := raw.(map[string]any)
		name := g["name"].(string)
		if g["n"] != float64(1) {
			t.Fatalf("%s affected N = %v, want 1", name, g["n"])
		}
		if g["err_events"] != wantEvents[name] {
			t.Fatalf("%s err_events = %v, want %v", name, g["err_events"], wantEvents[name])
		}
		if g["n"].(float64) > p["error_total"].(float64) {
			t.Fatalf("%s affected N %v exceeds error_total %v", name, g["n"], p["error_total"])
		}
	}
}

// TestExplorerErrorTotalCountsRecoveredQualityFailures pins the error
// gallery's denominator to the union of affected requests its cards represent,
// not the narrower Record.IsError set. An absorbed quality failure (final 200,
// attempt 200 with a structured error type) is an error event and a card
// membership, but IsError does not count it because no status or absorbed
// server failure occurred. The denominator must still include it exactly once,
// or a recovered card's N can exceed the denominator (or lose its share
// entirely when no final error exists).
func TestExplorerErrorTotalCountsRecoveredQualityFailures(t *testing.T) {
	base := time.UnixMilli(1_800_000_000_000)
	quality := func(id string, at time.Time, attempts int) *metrics.Record {
		r := &metrics.Record{ID: id, Provider: "p", Model: "m", Start: at, StatusCode: 200}
		for i := range attempts {
			r.Attempts = append(r.Attempts, metrics.RetryAttempt{
				StatusCode: 200, ErrorType: "empty_completion", ErrorMsg: "empty",
				At: at.Add(time.Duration(i) * time.Millisecond),
			})
		}
		return r
	}
	errorGroup := func(t *testing.T, p map[string]any, name string) map[string]any {
		t.Helper()
		for _, raw := range p["groups"].([]any) {
			g := raw.(map[string]any)
			if g["name"] == name {
				return g
			}
		}
		t.Fatalf("missing error card %q in %v", name, p["groups"])
		return nil
	}

	t.Run("recovered-only", func(t *testing.T) {
		s := testStore(t)
		s.Record(quality("recovered-1", base, 1))
		s.Record(quality("recovered-2", base.Add(time.Second), 2))
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		api := NewAggAPI(metrics.NewBuffer(1), s, time.Second)
		p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=error")
		if p["error_total"] != float64(2) {
			t.Fatalf("error_total = %v, want 2 affected recovered requests", p["error_total"])
		}
		g := errorGroup(t, p, "empty_completion|200|empty")
		if g["n"] != float64(2) || g["err_events"] != float64(3) {
			t.Fatalf("recovered card = %v, want N=2 err_events=3", g)
		}
		if g["n"].(float64) > p["error_total"].(float64) {
			t.Fatalf("recovered card N %v exceeds error_total %v", g["n"], p["error_total"])
		}
	})

	t.Run("mixed-final-and-absorbed", func(t *testing.T) {
		s := testStore(t)
		s.Record(quality("recovered-1", base, 1))
		s.Record(quality("recovered-2", base.Add(time.Second), 2))
		s.Record(&metrics.Record{
			ID: "final", Provider: "p", Model: "m", Start: base.Add(2 * time.Second),
			StatusCode: 500, ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "boom",
		})
		// One request carrying both a final failure and an absorbed quality
		// failure must join the union once even though it appears on two cards.
		s.Record(&metrics.Record{
			ID: "combined", Provider: "p", Model: "m", Start: base.Add(3 * time.Second),
			StatusCode: 500, ErrorType: "upstream", ErrorCode: "500", ErrorMsg: "boom",
			Attempts: []metrics.RetryAttempt{{
				StatusCode: 200, ErrorType: "empty_completion", ErrorMsg: "empty",
				At: base.Add(3 * time.Second),
			}},
		})
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
		api := NewAggAPI(metrics.NewBuffer(1), s, time.Second)
		p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=error")
		if p["error_total"] != float64(4) {
			t.Fatalf("error_total = %v, want 4 distinct affected requests (card Ns total 5, one shared)", p["error_total"])
		}
		recovered := errorGroup(t, p, "empty_completion|200|empty")
		final := errorGroup(t, p, "upstream|500|boom")
		if recovered["n"] != float64(3) || final["n"] != float64(2) {
			t.Fatalf("mixed cards = %v / %v, want N=3 and N=2", recovered, final)
		}
		if recovered["err_events"] != float64(4) || final["err_events"] != float64(2) {
			t.Fatalf("mixed events = %v / %v, want 4 and 2", recovered["err_events"], final["err_events"])
		}
		for name, g := range map[string]map[string]any{"recovered": recovered, "final": final} {
			if g["n"].(float64) > p["error_total"].(float64) {
				t.Fatalf("%s card N %v exceeds error_total %v", name, g["n"], p["error_total"])
			}
		}
	})
}

// TestExplorerToolCardsCountAffectedRequests pins tool cards to affected
// requests for N/share while keeping the calls KPI in call units. A nonstream
// response repeats a tool name once per call, so three calls to one tool in a
// single request must not produce N=3, a share above 100%, or a tripled calls
// KPI.
func TestExplorerToolCardsCountAffectedRequests(t *testing.T) {
	s := testStore(t)
	base := time.UnixMilli(1_800_000_000_000)
	s.Record(&metrics.Record{
		ID: "three-calls", Provider: "p", Model: "m", Start: base, StatusCode: 200,
		ToolCalls: 3, ToolNames: []string{"lookup", "lookup", "lookup"},
	})
	s.Record(&metrics.Record{
		ID: "one-call", Provider: "p", Model: "m", Start: base.Add(time.Second), StatusCode: 200,
		ToolCalls: 1, ToolNames: []string{"lookup"},
	})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	api := NewAggAPI(metrics.NewBuffer(1), s, time.Second)
	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=tool")
	if p["total"] != float64(2) {
		t.Fatalf("total = %v, want 2 requests", p["total"])
	}
	groups := p["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("tool groups = %v, want one lookup card", groups)
	}
	g := groups[0].(map[string]any)
	if g["name"] != "lookup" || g["n"] != float64(2) || g["tools"] != float64(4) {
		t.Fatalf("tool card = %v, want affected N=2 and calls=4", g)
	}
	if g["n"].(float64) > p["total"].(float64) {
		t.Fatalf("tool card N %v exceeds total %v", g["n"], p["total"])
	}
}

// The per-entity blended $/Mtok must exclude 0-cost records exactly like the
// KPI band's blended denominators do - an entity mixing priced and unpriced
// traffic (a client hitting both a priced provider and runinfra) must not
// have its rate diluted by the unpriced tokens.
func TestExplorerBlendedCostIgnoresUnpriced(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("pr", now.Add(-time.Hour), 200, "", nil, 10, 100, 100, 50, 0, 0.02))
	s.Record(mkRec("un", now.Add(-time.Minute), 200, "", nil, 10, 100, 1000, 50, 0, 0))
	waitTotals(t, s, 2)
	api := NewAggAPI(metrics.NewBuffer(16), s, time.Second)

	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=client")
	groups := p["groups"].([]any)
	if len(groups) != 1 { // mkRec stamps Client "cli" on both
		t.Fatalf("client groups = %d, want 1", len(groups))
	}
	g := groups[0].(map[string]any)
	if g["n"] != float64(2) || g["in"] != float64(1100) {
		t.Fatalf("client agg = %v (raw totals must absorb the unpriced tokens)", g)
	}
	// Denominator = the priced record's 150 tokens only.
	if got := g["cost_per_mtok"].(float64); math.Abs(got-0.02/150*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v (unpriced tokens must not dilute)", got, 0.02/150*1e6)
	}

	// An entity with NO cost-reporting requests at all → null, never a fake 0.
	s2 := testStore(t)
	s2.Record(mkRec("u1", now, 200, "", nil, 10, 100, 100, 50, 0, 0))
	waitTotals(t, s2, 1)
	api2 := NewAggAPI(metrics.NewBuffer(16), s2, time.Second)
	p2 := get(t, http.HandlerFunc(api2.HandleAggExplorer), "/metrics/agg/explorer?dim=provider")
	g2 := p2["groups"].([]any)[0].(map[string]any)
	if g2["cost_per_mtok"] != nil {
		t.Fatalf("cost_per_mtok = %v, want null (no cost-reporting requests)", g2["cost_per_mtok"])
	}
}

func TestExplorerRejectsBadDim(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(8), nil, time.Second)
	req := httptest.NewRequest(http.MethodGet, "/metrics/agg/explorer?dim=provider;DROP", nil)
	rec := httptest.NewRecorder()
	http.HandlerFunc(api.HandleAggExplorer).ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestStatusClassAndBuckets(t *testing.T) {
	cases := map[int]string{200: "2xx", 299: "2xx", 499: "cancel", 429: "4xx", 404: "4xx", 500: "5xx", 503: "5xx", 0: "err", 302: "err"}
	for status, want := range cases {
		if got := statusClass(status); got != want {
			t.Errorf("statusClass(%d) = %q, want %q", status, got, want)
		}
	}
	// The exported vocabulary and the classifier are one owner pair: every
	// published class must be produced by a case above, and no case may
	// produce a class the vocabulary does not publish.
	produced := map[string]bool{}
	for _, class := range cases {
		produced[class] = true
	}
	for status, class := range cases {
		if !slices.Contains(StatusClasses, class) {
			t.Errorf("statusClass(%d) produces %q, which StatusClasses does not publish", status, class)
		}
	}
	for _, class := range StatusClasses {
		if !produced[class] {
			t.Errorf("StatusClasses publishes %q, which statusClass never returns", class)
		}
	}
	sat := time.Date(2026, 8, 22, 12, 0, 0, 0, time.Local) // Saturday
	if got := metrics.TimeBucket(sat); got != "weekend" {
		t.Errorf("Saturday → %q, want weekend", got)
	}
	wed := time.Date(2026, 8, 12, 9, 0, 0, 0, time.Local)
	if got := metrics.TimeBucket(wed); got != "work" {
		t.Errorf("Wed 09:00 → %q, want work", got)
	}
	if got := metrics.TimeBucket(wed.Add(8 * time.Hour)); got != "evening" {
		t.Errorf("Wed 17:00 → %q, want evening", got)
	}
	if got := metrics.TimeBucket(wed.Add(-3 * time.Hour)); got != "night" {
		t.Errorf("Wed 06:00 → %q, want night", got)
	}
	if got := contribStatusClass(&contrib{live: true, paused: true, stream: true}); got != "paused" {
		t.Errorf("live paused = %q, want paused", got)
	}
	if got := contribStatusClass(&contrib{live: true, throttled: true, stream: true}); got != "throttled" {
		t.Errorf("live throttled = %q, want throttled (the pill precedence pauses > throttles > streams)", got)
	}
	if got := contribStatusClass(&contrib{live: true, stream: true}); got != "streaming" {
		t.Errorf("live stream = %q, want streaming", got)
	}
	if got := contribStatusClass(&contrib{live: true}); got != "pending" {
		t.Errorf("live pending = %q, want pending", got)
	}
	if got := contribStatusClass(&contrib{status: 0, stream: true}); got != "err" {
		t.Errorf("finalized stream status 0 = %q, want err (not streaming)", got)
	}
}

func TestExplorerLiveStatusClasses(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	now := time.Now()
	paused := mkRec("live-p", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	paused.Stream = true
	paused.Paused = true
	buf.PublishLive("begin", paused)
	stream := mkRec("live-s", now, 0, "", nil, 0, 0, 0, 0, 0, 0)
	stream.Stream = true
	buf.PublishLive("begin", stream)

	p := get(t, http.HandlerFunc(api.HandleAggExplorer), "/metrics/agg/explorer?dim=status")
	names := map[string]float64{}
	for _, e := range p["groups"].([]any) {
		m := e.(map[string]any)
		names[m["name"].(string)] = m["n"].(float64)
	}
	if names["paused"] != 1 || names["streaming"] != 1 {
		t.Fatalf("status groups = %v, want paused=1 streaming=1", names)
	}
	if p["error_total"] != float64(0) {
		t.Fatalf("error_total = %v, want 0 (live rows are not errors)", p["error_total"])
	}
}

func TestErrorEntriesSemantics(t *testing.T) {
	// 429 final = no error entry (flow control); a plain 499 = none (client
	// cancel).
	r := &metrics.Record{StatusCode: 429, ErrorType: "rate_limit_error", Start: time.Now()}
	if n := len(errorEntries(r)); n != 0 {
		t.Fatalf("429 → %d entries, want 0 (flow control is never an error event)", n)
	}
	r = &metrics.Record{StatusCode: 499, Start: time.Now()}
	if n := len(errorEntries(r)); n != 0 {
		t.Fatalf("499 → %d entries, want 0", n)
	}
	// A 499 carrying a structured error is the provider's in-band failure
	// the client aborted around - an entry exactly like its 200 twin.
	r = &metrics.Record{StatusCode: 499, ClientDisconnected: true, ErrorType: "api_error", ErrorCode: "internal_error", ErrorMsg: "temporarily unavailable", Start: time.Now()}
	ents := errorEntries(r)
	if len(ents) != 1 || ents[0].typ != "api_error" || ents[0].code != "internal_error" {
		t.Fatalf("499 with in-band error → %v", ents)
	}
	// A provider in-band error on 200 IS an entry.
	r = &metrics.Record{StatusCode: 200, ErrorType: "provider_overloaded", ErrorMsg: "x", Start: time.Now()}
	ents = errorEntries(r)
	if len(ents) != 1 || ents[0].typ != "provider_overloaded" {
		t.Fatalf("in-band → %v", ents)
	}
	// Absorbed 429 skipped; a typed transport attempt kept; a bare <400 attempt
	// skipped; a 401 and a typed 5xx kept (client recordErrorEntries parity).
	r = &metrics.Record{StatusCode: 200, Attempts: []metrics.RetryAttempt{
		{StatusCode: 429},
		{StatusCode: 502, ErrorType: "upstream"},
		{StatusCode: 0, ErrorType: "dial_failed"},
		{StatusCode: 401},
		{StatusCode: 302}, // bare <400 → skipped
	}, Start: time.Now()}
	ents = errorEntries(r)
	if len(ents) != 3 || !ents[0].absorbed {
		t.Fatalf("absorbed → %v", ents)
	}
	for i, wantType := range []string{"upstream", "dial_failed", "http_401"} {
		if ents[i].typ != wantType {
			t.Fatalf("ents[%d].typ = %q, want %q (%v)", i, ents[i].typ, wantType, ents)
		}
	}
	// An in-band attempt entry exists for the health predicates, not for error
	// disclosure: the final error_type already owns the failure, so it never
	// adds a duplicate entry. The synthetic in-band 5xx shape makes removing
	// the InBand guard fail here rather than pass through the 429 status skip.
	r = &metrics.Record{StatusCode: 200, ErrorType: "server_error", Start: time.Now(),
		Attempts: []metrics.RetryAttempt{
			{StatusCode: 502, ErrorType: "upstream"},
			{StatusCode: 429, InBand: true},
			{StatusCode: 500, InBand: true},
		}}
	ents = errorEntries(r)
	if len(ents) != 2 || ents[0].absorbed || ents[0].typ != "server_error" ||
		!ents[1].absorbed || ents[1].typ != "upstream" {
		t.Fatalf("in-band entries added an error disclosure → %v", ents)
	}
	// 5xx final status.
	r = &metrics.Record{StatusCode: 500, Start: time.Now()}
	ents = errorEntries(r)
	if len(ents) != 1 || ents[0].typ != "http_500" {
		t.Fatalf("final 5xx → %v", ents)
	}
}
