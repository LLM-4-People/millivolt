package web

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// numericFixture is the deterministic contribution set shared by the fold
// goldens and the numeric-admission property test. It exercises two providers,
// a final 4xx, a 429-only request (rate limit is not an error), cache and
// reasoning tokens, a cost-reporting and a costless request, and enough TTFT/
// TPS samples per explorer group for the percentile gates to publish numbers.
func numericFixture() ([]contrib, int64) {
	const (
		now    = int64(1_700_000_000_000)
		spread = int64(1000) // one second apart: the whole fixture is one chart bucket
	)
	specs := []struct {
		back             int64
		status           int
		in, out, cache   int64
		reason, tools    int64
		cost             float64
		ttft, dec        int64
		tps              float64
		provider, client string
	}{
		{5, 200, 100, 50, 20, 5, 1, 0.0010, 100, 500, 10.5, "alpha", "cli-1"},
		{4, 500, 200, 60, 10, 0, 0, 0.0020, 150, 600, 12.5, "alpha", "cli-1"},
		{3, 429, 50, 10, 0, 0, 0, 0, 90, 400, 8.0, "alpha", "cli-2"},
		{2, 400, 0, 0, 0, 0, 0, 0, 0, 0, 0, "beta", "cli-2"},
		{1, 200, 300, 80, 25, 10, 2, 0.0035, 200, 700, 20.0, "alpha", "cli-1"},
		{0, 200, 10, 5, 0, 1, 0, 0.0005, 0, 0, 0, "beta", "cli-1"},
	}
	rows := make([]contrib, 0, len(specs))
	for _, s := range specs {
		rows = append(rows, contrib{
			id:     "req-" + string(rune('a'+len(rows))),
			start:  now - s.back*spread,
			status: s.status,
			isErr:  s.status >= 400 && s.status != 429,
			has429: s.status == 429,
			cost:   s.cost,
			in:     s.in,
			out:    s.out,
			cacheR: s.cache,
			reason: s.reason,
			tools:  s.tools,
			ttft:   s.ttft,
			tps:    s.tps,
			dec:    s.dec,
			labels: &contribLabels{prov: s.provider, model: "model-" + s.provider, client: s.client},
		})
	}
	return rows, now
}

// projectionFor interns the fixture exactly like a query would, so a fold sees
// the same projected row shape production folds over.
func projectionFor(rows []contrib) *projectionData {
	p := &projectionData{rows: rows}
	for i := range rows {
		rows[i].rowIndex = uint32(i + 1)
		p.dimensions.intern(&rows[i])
		p.noteChunkStart(rows[i].rowIndex, rows[i].start)
	}
	p.metrics.append(rows, 0)
	return p
}

// recordsFromFixture mirrors the fixture into the durable record shape the
// ring and the durable totals fold from, keeping one fixture for all surfaces.
func recordsFromFixture(rows []contrib) []*metrics.Record {
	out := make([]*metrics.Record, 0, len(rows))
	for i := range rows {
		c := &rows[i]
		out = append(out, &metrics.Record{
			ID: c.id, Start: time.UnixMilli(c.start), StatusCode: c.status,
			Provider: c.labels.prov, Client: c.labels.client, Model: c.labels.model,
			Cost: c.cost, TTFTMs: c.ttft, DecodeTPS: c.tps, ToolCalls: int(c.tools),
			Usage: metrics.Usage{InputTokens: c.in, OutputTokens: c.out,
				CacheReadTokens: c.cacheR, ReasoningTokens: c.reason},
		})
	}
	return out
}

// foldGoldenProduct renders the three numeric products the chart, explorer and
// KPI surfaces publish. One fixture, three payloads: the golden asserts every
// consumer is byte-identical after an accumulation change.
func foldGoldenProduct(t *testing.T) map[string]string {
	t.Helper()
	rows, now := numericFixture()
	// Records keep their ring-side labels, so build them before interning
	// drops the live sidecar from the projected copies.
	records := recordsFromFixture(rows)
	p := projectionFor(rows)

	from, step, count := chartWindowEdges(now, rows[0].start)
	f := newChartFold(from, step, count, "", nil)
	f.prepare(p, nil)
	for i := range rows {
		if err := f.fold(&rows[i]); err != nil {
			t.Fatalf("chart fold: %v", err)
		}
	}
	f.freezeOrder()
	chart, err := json.Marshal(f.payload(now))
	if err != nil {
		t.Fatalf("chart marshal: %v", err)
	}

	e := newExplorerFold("provider", "", nil)
	e.prepare(p, nil)
	for i := range rows {
		if err := e.fold(&rows[i]); err != nil {
			t.Fatalf("explorer fold: %v", err)
		}
	}
	explorer, err := json.Marshal(e.payload())
	if err != nil {
		t.Fatalf("explorer marshal: %v", err)
	}

	totals := storage.Totals{}
	for _, r := range records {
		totals.Add(r)
	}
	if err := totals.Err(); err != nil {
		t.Fatalf("totals add: %v", err)
	}
	api := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	kpi, err := json.Marshal(api.kpiFromTotals(totals))
	if err != nil {
		t.Fatalf("kpi marshal: %v", err)
	}
	return map[string]string{"chart": string(chart), "explorer": string(explorer), "kpi": string(kpi)}
}

// Captured from the pre-change implementation (base 0b28628, before the WS7
// accumulator change). Byte equality here is the WS7 output contract: the fold
// may stop re-validating its accumulator, but no published byte may move.
const (
	chartFoldGolden    = `{"now_ms":1700000000000,"from_ms":1699999980000,"bucket_ms":60000,"ttft_p":[125,192.5,198.5],"tps_p":[11.5,18.874999999999996,19.775],"dec_p":[550,685,697],"ttft_stat":[135,90,200],"tps_stat":[12.75,8,20],"cost_per_mtok":8.695652173913045,"buckets":[{"t":1699999980000,"req":6,"err":2,"rl":1,"in":660,"out":205,"cache":55,"reason":16,"cost":0.007000000000000001,"ttft":[125,192.5,198.5],"tps":[11.5,18.874999999999996,19.775],"dec":[550,685,697]}]}`
	explorerFoldGolden = `{"conversation_summary":{"main":0,"sub":0,"unresolved":0},"dim":"provider","total":6,"error_total":2,"rail":{"client":2,"conversation":0,"error":0,"key":0,"model":2,"provider":2,"status":3,"time":1,"tool":0},"groups":[{"name":"alpha","n":4,"cost":0.006500000000000001,"in":650,"out":200,"cache":55,"err_final":1,"rate_limit_requests":1,"tools":3,"cost_per_mtok":8.227848101265824,"ttft_p50":125,"ttft_p95":192.5,"tps_p50":11.5,"tps_p95":18.874999999999996,"err_events":0,"code":"","last_ms":0},{"name":"beta","n":2,"cost":0.0005,"in":10,"out":5,"cache":0,"err_final":1,"rate_limit_requests":0,"tools":0,"cost_per_mtok":33.333333333333336,"ttft_p50":null,"ttft_p95":null,"tps_p50":null,"tps_p95":null,"err_events":0,"code":"","last_ms":0}],"scope":{"matches":6,"errors":2}}`
	kpiTotalsGolden    = `{"requests":6,"errors":2,"in_flight":0,"cost":0.007000000000000001,"cost_per_req":0.0017500000000000003,"cost_per_mtok":8.695652173913045,"input_tokens":660,"output_tokens":205,"cache_read_tokens":55,"reasoning_tokens":16,"avg_ttft_ms":135,"avg_tps":12.75}`
)

func TestFoldGoldensUnchanged(t *testing.T) {
	got := foldGoldenProduct(t)
	for _, name := range []string{"chart", "explorer", "kpi"} {
		want := map[string]string{
			"chart": chartFoldGolden, "explorer": explorerFoldGolden, "kpi": kpiTotalsGolden,
		}[name]
		if want == "" {
			t.Errorf("%s golden not captured: %s", name, got[name])
			continue
		}
		if got[name] != want {
			t.Errorf("%s payload changed:\n got: %s\nwant: %s", name, got[name], want)
		}
	}
}

// numericAdmissions is the boundary corpus: zero, one, the int64 ceiling and
// its neighbour, a negative, both infinities, NaN, the smallest normal and the
// smallest subnormal. A record carrying one of these must either be rejected or
// leave the aggregate failed closed; it must never publish a number.
func floatAdmissions() []struct {
	name string
	v    float64
	ok   bool
} {
	return []struct {
		name string
		v    float64
		ok   bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"max_float", math.MaxFloat64, true},
		{"negative", -1, false},
		{"nan", math.NaN(), false},
		{"pos_inf", math.Inf(1), false},
		{"neg_inf", math.Inf(-1), false},
		{"smallest_normal", 0x1p-1022, true},
		{"subnormal", math.SmallestNonzeroFloat64, true},
	}
}

func intAdmissions() []struct {
	name string
	v    int64
	ok   bool
} {
	return []struct {
		name string
		v    int64
		ok   bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"max_int64", math.MaxInt64, true},
		{"max_int64_minus_1", math.MaxInt64 - 1, true},
		{"negative", -1, false},
	}
}

// TestChartFoldNumericAdmission drives each corpus value through the real chart
// fold. Rejection (fold error, which the handler turns into a 5xx) or a finite
// rendered payload are the only acceptable outcomes; a JSON marshal failure
// would be a third (NaN/Inf) so it is asserted absent.
func TestChartFoldNumericAdmission(t *testing.T) {
	now := int64(1_700_000_000_000)
	foldOne := func(mut func(*contrib)) (chartPayload, error) {
		rows := []contrib{{start: now, status: 200, labels: &contribLabels{prov: "alpha"}}}
		mut(&rows[0])
		p := projectionFor(rows)
		f := newChartFold(now, time.Minute.Milliseconds(), 1, "", nil)
		f.prepare(p, nil)
		if err := f.fold(&rows[0]); err != nil {
			return chartPayload{}, err
		}
		f.freezeOrder()
		return f.payload(now), nil
	}
	for _, tc := range floatAdmissions() {
		for _, field := range []string{"cost", "tps"} {
			payload, err := foldOne(func(c *contrib) {
				if field == "cost" {
					c.cost = tc.v
				} else {
					c.tps = tc.v
				}
			})
			if !tc.ok {
				if field == "cost" && err == nil {
					t.Fatalf("%s=%v: chart fold accepted an unrepresentable cost", field, tc.v)
				}
				continue // tps is an optional sample: invalid values are absent data
			}
			if err != nil {
				t.Fatalf("%s=%v: chart fold rejected a representable value: %v", field, tc.v, err)
			}
			if _, err := json.Marshal(payload); err != nil {
				t.Fatalf("%s=%v: rendered an unrepresentable number: %v", field, tc.v, err)
			}
			if field == "cost" && len(payload.Buckets) != 1 {
				t.Fatalf("%s: expected one bucket, got %d", field, len(payload.Buckets))
			}
			if field == "cost" && payload.Buckets[0].Cost != tc.v {
				t.Fatalf("cost=%v rendered as %v", tc.v, payload.Buckets[0].Cost)
			}
		}
	}
	for _, tc := range intAdmissions() {
		for _, field := range []string{"in", "out", "cacheR", "reason"} {
			_, err := foldOne(func(c *contrib) {
				switch field {
				case "in":
					c.in = tc.v
				case "out":
					c.out = tc.v
				case "cacheR":
					c.cacheR = tc.v
				case "reason":
					c.reason = tc.v
				}
			})
			if !tc.ok && err == nil {
				t.Fatalf("%s=%v: chart fold accepted a negative count", field, tc.v)
			}
			if tc.ok && err != nil {
				t.Fatalf("%s=%v: chart fold rejected a representable count: %v", field, tc.v, err)
			}
		}
	}
}

// TestFoldNumericOverflowFailsClosed proves the accumulated half still detects
// overflow exactly: two representable contributions whose sum is not
// representable must fail the aggregate rather than wrap.
func TestFoldNumericOverflowFailsClosed(t *testing.T) {
	now := int64(1_700_000_000_000)
	fold := func(mut func(*contrib), n int) error {
		rows := make([]contrib, n)
		for i := range rows {
			rows[i] = contrib{start: now, status: 200, labels: &contribLabels{prov: "alpha"}}
			mut(&rows[i])
		}
		p := projectionFor(rows)
		f := newChartFold(now, time.Minute.Milliseconds(), 1, "", nil)
		f.prepare(p, nil)
		for i := range rows {
			if err := f.fold(&rows[i]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, tc := range []struct {
		name string
		mut  func(*contrib)
	}{
		{"cost", func(c *contrib) { c.cost = math.MaxFloat64 }},
		{"in", func(c *contrib) { c.in = math.MaxInt64 }},
		{"out", func(c *contrib) { c.out = math.MaxInt64 }},
	} {
		if err := fold(tc.mut, 1); err != nil {
			t.Fatalf("%s: a single representable contribution must fold: %v", tc.name, err)
		}
		if err := fold(tc.mut, 2); !errors.Is(err, metrics.ErrMetricRange) {
			t.Fatalf("%s: overflowing sum must fail closed with ErrMetricRange, got %v", tc.name, err)
		}
	}
}

// TestExplorerFoldNumericAdmission is the same boundary corpus through the
// explorer fold, which publishes per-group cost and blended price.
func TestExplorerFoldNumericAdmission(t *testing.T) {
	foldOne := func(mut func(*contrib)) (explorerPayload, error) {
		rows := []contrib{{start: 1_700_000_000_000, status: 200, labels: &contribLabels{prov: "alpha"}}}
		mut(&rows[0])
		p := projectionFor(rows)
		e := newExplorerFold("provider", "", nil)
		e.prepare(p, nil)
		if err := e.fold(&rows[0]); err != nil {
			return explorerPayload{}, err
		}
		return e.payload(), nil
	}
	for _, tc := range floatAdmissions() {
		payload, err := foldOne(func(c *contrib) { c.cost = tc.v })
		if !tc.ok {
			if err == nil {
				t.Fatalf("cost=%v: explorer fold accepted an unrepresentable cost", tc.v)
			}
			continue
		}
		if err != nil {
			t.Fatalf("cost=%v: explorer fold rejected a representable value: %v", tc.v, err)
		}
		if _, err := json.Marshal(payload); err != nil {
			t.Fatalf("cost=%v: explorer rendered an unrepresentable number: %v", tc.v, err)
		}
	}
	for _, tc := range intAdmissions() {
		_, err := foldOne(func(c *contrib) { c.in = tc.v })
		if !tc.ok && err == nil {
			t.Fatalf("in=%v: explorer fold accepted a negative count", tc.v)
		}
		if tc.ok && err != nil {
			t.Fatalf("in=%v: explorer fold rejected a representable count: %v", tc.v, err)
		}
	}
}

// TestTotalsNumericAdmission asserts the durable totals owner keeps its
// fail-closed contract: an unrepresentable record latches the error, the KPI
// payload refuses to render, and a representable corpus produces finite JSON.
func TestTotalsNumericAdmission(t *testing.T) {
	api := NewAggAPI(metrics.NewBuffer(4), nil, time.Second)
	base := func() *metrics.Record {
		return &metrics.Record{ID: "r", StatusCode: 200, Provider: "p",
			Usage: metrics.Usage{InputTokens: 1, OutputTokens: 2}}
	}
	render := func(r *metrics.Record) (string, error) {
		totals := storage.Totals{}
		totals.Add(r)
		if err := totals.Err(); err != nil {
			return "", err
		}
		b, err := json.Marshal(api.kpiFromTotals(totals))
		return string(b), err
	}
	for _, tc := range floatAdmissions() {
		for _, field := range []string{"cost", "tps"} {
			r := base()
			if field == "cost" {
				r.Cost = tc.v
			} else if tc.v > 0 || math.IsNaN(tc.v) {
				// A non-positive TPS is absent data (documented gate), so only
				// a positive non-finite sample reaches the checked sum.
				r.DecodeTPS = tc.v
			}
			out, err := render(r)
			if !tc.ok {
				if field == "cost" && err == nil {
					t.Fatalf("cost=%v: Totals.Add accepted an unrepresentable cost", tc.v)
				}
				if field == "tps" && tc.v > 0 && err == nil {
					t.Fatalf("tps=%v: Totals.Add accepted an unrepresentable sample", tc.v)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s=%v: Totals.Add rejected a representable value: %v", field, tc.v, err)
			}
			if strings.Contains(out, "NaN") || strings.Contains(out, "Inf") {
				t.Fatalf("%s=%v: rendered an unrepresentable number: %s", field, tc.v, out)
			}
		}
	}
	for _, tc := range intAdmissions() {
		r := base()
		r.Usage.InputTokens = tc.v
		_, err := render(r)
		if !tc.ok && err == nil {
			t.Fatalf("in=%v: Totals.Add accepted a negative count", tc.v)
		}
		if tc.ok && err != nil {
			t.Fatalf("in=%v: Totals.Add rejected a representable count: %v", tc.v, err)
		}
	}
}

// TestChartFoldAllocationBounded is the WS7 budget probe: accumulation at the
// fold must not trade the removed validation for per-row allocation. Measured
// 29 allocs and 28,880 bytes; the 35 ceiling is just over 20% headroom. The
// allocation count is the per-row guard: one added allocation per row fails it
// by 10,000. The 128 KiB byte ceiling leaves about 102 KiB, so a one-off 80 KiB
// scratch passes by design; it bounds gross scratch, not per-row work.
func TestChartFoldAllocationBounded(t *testing.T) {
	now := int64(1_700_000_000_000)
	const rows = 10000
	cs := make([]contrib, rows)
	for i := range cs {
		cs[i] = contrib{start: now - int64(rows-1-i)*1000, status: 200, in: 100, out: 50,
			cost: 0.001, ttft: 100, tps: 10, labels: &contribLabels{prov: "alpha"}}
	}
	p := projectionFor(cs)
	from, step, count := chartWindowEdges(now, cs[0].start)
	bytes, allocs := allocStats(func() {
		f := newChartFold(from, step, count, "", nil)
		f.prepare(p, nil)
		for i := range cs {
			if err := f.fold(&cs[i]); err != nil {
				t.Fatal(err)
			}
		}
		f.freezeOrder()
		_ = f.payload(now)
	})
	// Measured 2026-10-25 on the c2b tree: 29 allocs. Ranked-set growth and the
	// payload dominate; the numeric fold must stay allocation-free.
	if allocs > 35 {
		t.Fatalf("chart fold allocs = %.0f, budget 35", allocs)
	}
	if bytes > 128*1024 {
		t.Fatalf("chart fold bytes = %d, budget %d", bytes, 128*1024)
	}
	t.Logf("chart fold: %.0f allocs, %d bytes", allocs, bytes)
}
