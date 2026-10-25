package web

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// ---------- traffic chart endpoint ----------

// chartBucketJSON is one bucket's aggregate row.
type chartBucketJSON struct {
	T      int64      `json:"t"`
	Req    int64      `json:"req"`
	Err    int64      `json:"err"` // affected requests Record.IsError counts (final 4xx/5xx or an absorbed 5xx); 429 alone is not one
	RL     int64      `json:"rl"`  // affected requests with final-or-retried HTTP 429 (Record.HasRateLimit); distinct from Err
	In     int64      `json:"in"`
	Out    int64      `json:"out"`
	Cache  int64      `json:"cache"` // cached prompt tokens (subset of In)
	Reason int64      `json:"reason"`
	Cost   float64    `json:"cost"`
	TTFT   []*float64 `json:"ttft"` // [p50, p95, p99]; nil entries below pctMinSamples samples
	TPS    []*float64 `json:"tps"`
	Dec    []*float64 `json:"dec"` // decode window (last_token_at - first_token_at), same triple shape
}

type chartPayload struct {
	NowMs      int64             `json:"now_ms"`
	FromMs     int64             `json:"from_ms"`   // CLOCK-ALIGNED window start (floored onto a ladder step)
	BucketMs   int64             `json:"bucket_ms"` // exact integer bucket width - single source of truth
	TTFTP      []*float64        `json:"ttft_p"`    // period-wide [p50, p95, p99] (the totals strip)
	TPSP       []*float64        `json:"tps_p"`
	DecP       []*float64        `json:"dec_p"`
	TTFTStat   []*float64        `json:"ttft_stat"` // period-wide [avg, min, max] over every captured sample
	TPSStat    []*float64        `json:"tps_stat"`
	CostPerMTk *float64          `json:"cost_per_mtok"` // period blended price; SAME rule as the KPI band (cost-reporting requests only)
	Buckets    []chartBucketJSON `json:"buckets"`
}

type bAcc struct {
	t, req, err, rl, in, out, cache, reason int64
	cost                                    float64
	ttftN, tpsN, decN                       int
}

// chartFold accumulates one scoped chart series over a stream of
// contributions. The stream is window-bounded (streamWindow's since), so
// every contribution lands in a bucket.
type chartFold struct {
	from, step int64
	count      int
	buckets    []bAcc
	cost       float64 // period totals behind cost_per_mtok (blended price)
	costInOut  float64 // in+out of cost-REPORTING requests only
	extraTTFT  []int64 // only unflushed/ring values; durable samples stay in the shared order
	extraTPS   []float64
	extraDec   []int64
	ttftBucket []uint8
	tpsBucket  []uint8
	decBucket  []uint8
	membership []uint8
	order      metricOrderSource
	// orderSource is the projection's metric view, retained only while the
	// read lease is held. freezeOrder selects either source after the fold
	// knows whether the recent tail can answer the window.
	orderSource *projectionMetrics
	// recent is the selected tail source for bounded windows; recentFrom is
	// its first covered projection row (0-based). minMemberRow is the earliest
	// in-window durable row, so the tail is safe only when it covers that row.
	recent       metricOrderSource
	recentFrom   int
	minMemberRow uint32
	hasMember    bool
	statusCode   string
	fs           []scopeFilter
}

func newChartFold(from, step int64, count int, statusCode string, fs []scopeFilter) *chartFold {
	f := &chartFold{from: from, step: step, count: count, statusCode: statusCode, fs: fs}
	f.buckets = make([]bAcc, count)
	for i := range f.buckets {
		f.buckets[i].t = from + int64(i)*step // bucket START edge
	}
	return f
}

// Projection metric orders are immutable snapshots. The per-view state is
// only one byte of bucket membership per row, not copies of all samples.
// Both durable and ring-only modes use the same exact rank reader.
func (f *chartFold) prepare(p *projectionData, mcz *modelCanonizer) {
	f.fs = bindScope(f.fs, p, mcz)
	f.prepareBound(p)
}

// prepareBound is prepare for callers that already bound their filters under
// the same projection lease.
func (f *chartFold) prepareBound(p *projectionData) {
	if p == nil {
		return
	}
	f.membership = make([]uint8, len(p.rows))
	f.orderSource = &p.metrics
	f.recentFrom = p.metrics.recentFrom
}

// recentCovers reports whether the retained tail covers every in-window
// durable row. Row indices are 1-based; recentFrom is a 0-based slice start.
func (f *chartFold) recentCovers() bool {
	return f.hasMember && f.recentFrom > 0 && int(f.minMemberRow) > f.recentFrom
}

// freezeOrder selects the exact durable order once the fold knows whether the
// retained tail covers every member. It must run under the projection lease.
// The fast path skips the tail entirely; neither path materializes a merge, so
// a fresh view allocates nothing history-sized.
func (f *chartFold) freezeOrder() {
	source := f.orderSource
	f.orderSource = nil
	if source == nil {
		return
	}
	if f.recentCovers() {
		f.recent = source.recentOrderSource()
		return
	}
	if !f.hasMember {
		return
	}
	f.order = source.allOrderSource()
}

func (f *chartFold) fold(c *contrib) error {
	if !scopedMatch(c, f.statusCode, f.fs) {
		return nil
	}
	idx := int((c.start - f.from) / f.step)
	if idx < 0 {
		idx = 0
	}
	if idx > f.count-1 {
		idx = f.count - 1
	}
	b := &f.buckets[idx]
	b.req++
	if c.isErr {
		b.err++
	}
	if c.has429 {
		b.rl++
	}
	// Counts and cost are admitted per contribution here; AddCount/AddValue
	// check the step's overflow exactly, so the fold never re-validates an
	// accumulator that is finite and non-negative by induction.
	if err := metrics.AddCount(&b.in, c.in); err != nil {
		return err
	}
	if err := metrics.AddCount(&b.out, c.out); err != nil {
		return err
	}
	if err := metrics.AddCount(&b.cache, c.cacheR); err != nil {
		return err
	}
	if err := metrics.AddCount(&b.reason, c.reason); err != nil {
		return err
	}
	// Cost feeds two accumulators (bucket and period), each admitted once.
	if err := metrics.AddValue(&b.cost, c.cost); err != nil {
		return err
	}
	if err := metrics.AddValue(&f.cost, c.cost); err != nil {
		return err
	}
	// Blended-price denominator: only cost-REPORTING requests contribute
	// tokens - the SAME rule the explorer/KPI aggregates apply, so the tile
	// and the KPI band can never quote different figures for one period.
	if c.cost > 0 {
		if err := metrics.AddValue(&f.costInOut, float64(c.in)); err != nil {
			return err
		}
		if err := metrics.AddValue(&f.costInOut, float64(c.out)); err != nil {
			return err
		}
	}
	if c.rowIndex > 0 {
		f.membership[c.rowIndex-1] = uint8(idx + 1)
		if !f.hasMember || c.rowIndex < f.minMemberRow {
			f.minMemberRow, f.hasMember = c.rowIndex, true
		}
	}
	if validMetricSample(c.ttft) {
		b.ttftN++
		if c.rowIndex == 0 {
			f.extraTTFT = append(f.extraTTFT, c.ttft)
			f.ttftBucket = append(f.ttftBucket, uint8(idx+1))
		}
	}
	if validMetricSample(c.tps) {
		b.tpsN++
		if c.rowIndex == 0 {
			f.extraTPS = append(f.extraTPS, c.tps)
			f.tpsBucket = append(f.tpsBucket, uint8(idx+1))
		}
	}
	if validMetricSample(c.dec) {
		b.decN++
		if c.rowIndex == 0 {
			f.extraDec = append(f.extraDec, c.dec)
			f.decBucket = append(f.decBucket, uint8(idx+1))
		}
	}
	return nil
}

func (f *chartFold) payload(now int64) chartPayload {
	out := make([]chartBucketJSON, f.count)
	ttftCounts, tpsCounts, decCounts := make([]int, f.count), make([]int, f.count), make([]int, f.count)
	for i, b := range f.buckets {
		ttftCounts[i], tpsCounts[i], decCounts[i] = b.ttftN, b.tpsN, b.decN
	}
	order := f.order
	// Every durable row in this window is inside the retained tail, so the
	// exact ranks can read the short sorted order instead of all history.
	if f.recentCovers() {
		order = f.recent
	}
	ttft, periodTTFT, ttftStat := rankPercentilesStatsMerge(order.base.ttft, order.pending.ttft, f.membership, ttftCounts, f.extraTTFT, f.ttftBucket)
	tps, periodTPS, tpsStat := rankPercentilesStatsMerge(order.base.tps, order.pending.tps, f.membership, tpsCounts, f.extraTPS, f.tpsBucket)
	dec, periodDec := rankPercentilesMerge(order.base.dec, order.pending.dec, f.membership, decCounts, f.extraDec, f.decBucket)
	for i, b := range f.buckets {
		o := chartBucketJSON{T: b.t, Req: b.req, Err: b.err, RL: b.rl, In: b.in, Out: b.out,
			Cache: b.cache, Reason: b.reason, Cost: b.cost}
		o.TTFT = ttft[i]
		o.TPS = tps[i]
		o.Dec = dec[i]
		out[i] = o
	}
	return chartPayload{NowMs: now, FromMs: f.from, BucketMs: f.step,
		TTFTP: periodTTFT, TPSP: periodTPS, DecP: periodDec, TTFTStat: ttftStat, TPSStat: tpsStat,
		CostPerMTk: metrics.ScaledRatio(f.cost, f.costInOut, 1e6), Buckets: out}
}

// chartWindowEdges floors the raw window start onto a clock-aligned ladder
// step (HandleAggChart's bucket geometry).
func chartWindowEdges(now, rawFrom int64) (from, step int64, count int) {
	// Clock alignment: floor the raw window start onto a ladder step so the
	// edges land on clock marks; flooring can add one partial bucket
	// (count ≤ chartMaxBuckets+1, even for multi-year history).
	step = chartStepMs(now - rawFrom)
	from = rawFrom - rawFrom%step
	if rawFrom < 0 && rawFrom%step != 0 {
		from -= step // Go's remainder truncates toward zero; we need floor.
	}
	// The adaptive step bounds count to cap+1 before allocation.
	count = int((now - from + step - 1) / step)
	if count < 1 {
		count = 1 // now==from on a boundary must still render one bucket
	}
	return from, step, count
}

// HandleAggChart serves the traffic time series over the selected window,
// SCOPED by the request log's filter set (f=dim:id explorer filters + s=
// exact status code) - computed over ALL matching history, never the ring.
// window is positive integer minutes (all = first in-scope request → now).
// Bucket width is the smallest nice-ladder step that fits chartMaxBuckets
// and the window start is floored onto a step boundary, so bucket t values
// are clock-aligned START edges (t = from_ms + i*bucket_ms).
func (a *AggAPI) HandleAggChart(w http.ResponseWriter, r *http.Request) {
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "window", "s"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	winMin, ok := windowParam(q)
	if !ok {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid window")
		return
	}
	fs, statusCode, ok := parseScope(q)
	if !ok {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid filter")
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	payload, err := a.chart(ctx, winMin, fs, statusCode, time.Now().UnixMilli())
	if err != nil {
		log.Printf("agg chart: %v", err)
		adminjson.WriteError(w, http.StatusInternalServerError, "aggregate failed")
		return
	}
	writeJSON(w, payload)
}

// windowParam owns the API's time-window domain: all history or positive
// integral minutes bounded to safe duration arithmetic. CHART_WINDOWS owns
// the UI's curated choices, not a second whitelist that could drift here.
// q is the handler's one strict-parsed query (adminjson.StrictQuery); no
// route parses the request twice.
func windowParam(q url.Values) (int, bool) {
	v := q.Get("window")
	if v == "" || v == "all" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > chartMaxWindowMinutes || strconv.FormatInt(n, 10) != v {
		return 0, false
	}
	return int(n), true
}

// streamAllChart computes the All-window scoped minimum and folds the chart
// under ONE ring snapshot and ONE projection lease. Splitting them let a row
// committed between the two leases contribute an older start than the one the
// window was built from, omitting it for that response. The minimum pass and
// fold still see the same immutable contributions.
func (a *AggAPI) streamAllChart(ctx context.Context, mcz *modelCanonizer, statusCode string, fs []scopeFilter, now int64, ready func(minStart, from, step int64, count int, fold *chartFold)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scratch, ring := a.ringSnapshot(0)
	defer releaseRing(scratch, ring)
	return a.withProjection(ctx, func(p *projectionData) error {
		if p != nil {
			fs = bindScope(fs, p, mcz)
		}
		min := now
		upd := func(c *contrib) {
			if scopedMatch(c, statusCode, fs) && c.start < min {
				min = c.start
			}
		}
		if p != nil {
			if len(fs) == 0 && statusCode == "" {
				if p.hasStart && p.minStart < min {
					min = p.minStart
				}
			} else {
				for i := range p.rows {
					if err := checkFoldContext(ctx, i); err != nil {
						return err
					}
					upd(&p.rows[i])
				}
			}
		}
		for i, r := range ring {
			if err := checkFoldContext(ctx, i); err != nil {
				return err
			}
			if p != nil {
				if _, duplicate := p.ids[r.ID]; duplicate {
					continue
				}
			}
			c := fromRecord(r, mcz)
			upd(&c)
		}
		from, step, count := chartWindowEdges(now, min)
		fold := newChartFold(from, step, count, statusCode, fs)
		fold.prepareBound(p)
		if p != nil {
			if err := p.visitSince(ctx, from, fold.fold); err != nil {
				return err
			}
		}
		for i, r := range ring {
			if err := checkFoldContext(ctx, i); err != nil {
				return err
			}
			if p != nil {
				if _, duplicate := p.ids[r.ID]; duplicate {
					continue
				}
			}
			c := fromRecord(r, mcz)
			if err := fold.fold(&c); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		fold.freezeOrder()
		ready(min, from, step, count, fold)
		return nil
	})
}
