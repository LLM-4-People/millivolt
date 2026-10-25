package web

import (
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestKPIStoredPlusRingMerge(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("a", now.Add(-2*time.Hour), 200, "", nil, 500, 1000, 100, 50, 10, 0.02))
	s.Record(mkRec("b", now.Add(-time.Hour), 502, "api_error", nil, 0, 800, 200, 0, 0, 0.01))
	s.Record(mkRec("c", now.Add(-time.Minute), 200, "", []metrics.RetryAttempt{{StatusCode: 503}}, 300, 900, 150, 80, 150, 0.03))
	waitTotals(t, s, 3)

	buf := metrics.NewBuffer(16)
	// "d" lives only in the ring (never recorded to the store).
	buf.Record(mkRec("d", now, 200, "", nil, 100, 500, 50, 30, 0, 0.001))
	// "e" is in BOTH ring and store - must be counted exactly once.
	ee := mkRec("e", now.Add(-3*time.Hour), 200, "", nil, 600, 1200, 300, 200, 300, 0.05)
	// Production ownership order: the ring applies retention in place before
	// the durable writer can read the same record.
	buf.Record(ee)
	s.Record(ee)
	waitTotals(t, s, 4)

	api := NewAggAPI(buf, s, time.Second)
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	// Stored 4 (a,b,c,e) + ring-extra d; e deduped via the written set.
	if p["requests"] != float64(5) {
		t.Fatalf("requests = %v, want 5", p["requests"])
	}
	// Errors: b (502) + c (absorbed 503) - canonical IsError semantics.
	if p["errors"] != float64(2) {
		t.Fatalf("errors = %v, want 2", p["errors"])
	}
	if p["input_tokens"] != float64(800) { // 100+200+150+300+50
		t.Fatalf("input = %v, want 800", p["input_tokens"])
	}
	if p["output_tokens"] != float64(360) { // 50+0+80+30+200
		t.Fatalf("output = %v, want 360", p["output_tokens"])
	}
	if p["cache_read_tokens"] != float64(460) { // 10+0+150+0+300
		t.Fatalf("cache = %v, want 460", p["cache_read_tokens"])
	}
	if p["avg_ttft_ms"] != float64(375) { // 500+300+100+600 = 1500/4 (b has 0 → skipped)
		t.Fatalf("avg_ttft = %v, want 375", p["avg_ttft_ms"])
	}
	if p["in_flight"] != float64(0) {
		t.Fatalf("in_flight = %v, want 0", p["in_flight"])
	}
	// All 5 records reported cost → blended denominators = all requests and
	// their tokens: 0.111 over 5 reqs / 1160 in+out tokens.
	if got := p["cost_per_req"].(float64); math.Abs(got-0.111/5) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want %v", got, 0.111/5)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.111/1160*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.111/1160*1e6)
	}
}

// The durable fold (Totals.add via the writer, and the computeTotals rebuild
// it shares) must exclude 0-cost records from the blended denominators exactly
// like the ring fold does - an unpriced provider's tokens never dilute.
func TestKPIBlendedCostIgnoresUnpricedOnDurablePath(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	s.Record(mkRec("p1", now.Add(-time.Hour), 200, "", nil, 10, 100, 100, 50, 0, 0.02))
	s.Record(mkRec("u1", now.Add(-time.Minute), 200, "", nil, 10, 100, 1000, 50, 0, 0))
	waitTotals(t, s, 2)

	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, s, time.Second)
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.02) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want 0.02 (unpriced request must not enter the denominator)", got)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.02/150*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v (unpriced tokens must not enter the denominator)", got, 0.02/150*1e6)
	}
	// Raw totals still absorbed everything.
	if p["requests"] != float64(2) || p["input_tokens"] != float64(1100) {
		t.Fatalf("requests/input = %v/%v, want 2/1100", p["requests"], p["input_tokens"])
	}
}

func TestKPIInFlightLive(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	buf.PublishLive("begin", &metrics.Record{ID: "first"})
	buf.PublishLive("begin", &metrics.Record{ID: "second"})
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if p["in_flight"] != float64(2) {
		t.Fatalf("in_flight = %v, want 2", p["in_flight"])
	}
}

// The KPI band's unit-economics fields are server-derived from the folded
// totals: blended $/req and $/Mtok over the tokens of cost-REPORTING requests
// only - a 0-cost record (provider reported no price) must never dilute the
// rate. Null ("-") on a zero denominator, never a fake 0.
func TestKPICostUnitEconomics(t *testing.T) {
	buf := metrics.NewBuffer(16)
	api := NewAggAPI(buf, nil, time.Second)
	now := time.Now()

	// Empty history: both denominators are 0 → null/null.
	p := getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if p["cost_per_req"] != nil || p["cost_per_mtok"] != nil {
		t.Fatalf("empty cost ratios = %v/%v, want null/null", p["cost_per_req"], p["cost_per_mtok"])
	}

	// $0.03 over 2 requests and 350 in+out tokens.
	buf.Record(mkRec("c1", now, 200, "", nil, 10, 100, 100, 50, 0, 0.02))
	buf.Record(mkRec("c2", now, 200, "", nil, 10, 100, 200, 0, 0, 0.01))
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.015) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want 0.015", got)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.03/350*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.03/350*1e6)
	}

	// A zero-token record adds cost but no tokens: cost_per_req re-blends,
	// cost_per_mtok keeps the 350-token denominator.
	buf.Record(mkRec("c3", now, 500, "boom", nil, 0, 0, 0, 0, 0, 0.5))
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.53/3) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want %v", got, 0.53/3)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.53/350*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v", got, 0.53/350*1e6)
	}

	// A zero-COST record (unpriced provider) adds requests and tokens but no
	// cost: it must stay out of BOTH blended denominators - $/req stays
	// 0.53/3 over the three cost-reporting requests, $/Mtok keeps their
	// 350-token denominator despite 1050 more tokens flowing through.
	buf.Record(mkRec("c4", now, 200, "", nil, 10, 100, 1000, 50, 0, 0))
	p = getKPI(t, http.HandlerFunc(api.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.53/3) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want %v (0-cost record must not enter the denominator)", got, 0.53/3)
	}
	if got := p["cost_per_mtok"].(float64); math.Abs(got-0.53/350*1e6) > 1e-6 {
		t.Fatalf("cost_per_mtok = %v, want %v (0-cost tokens must not enter the denominator)", got, 0.53/350*1e6)
	}

	// All-history sanity: the raw totals DID absorb c4 (its 1050 tokens).
	if p["input_tokens"] != float64(100+200+1000) {
		t.Fatalf("input_tokens = %v, want 1300 (0-cost tokens still counted in raw totals)", p["input_tokens"])
	}

	// A history with ZERO tokens in total (single zero-token record, cost
	// still counts) keeps cost_per_req but nulls cost_per_mtok.
	buf2 := metrics.NewBuffer(16)
	api2 := NewAggAPI(buf2, nil, time.Second)
	buf2.Record(mkRec("z1", now, 500, "boom", nil, 0, 0, 0, 0, 0, 0.5))
	p = getKPI(t, http.HandlerFunc(api2.HandleBootstrap), "/metrics/bootstrap")
	if got := p["cost_per_req"].(float64); math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("cost_per_req = %v, want 0.5", got)
	}
	if p["cost_per_mtok"] != nil {
		t.Fatalf("cost_per_mtok = %v, want null (zero in+out tokens)", p["cost_per_mtok"])
	}
}
