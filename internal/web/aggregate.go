package web

// Dashboard server-side aggregation: every non-table metric surface (KPI band,
// traffic chart, explorer breakdowns) is computed here against ALL requests
// since inception - the durable store plus any ring records a slow flush has
// not yet written (deduped by the store's exact written-id set). The live
// request log paints the in-memory ring first and pages older store rows via
// HandleLogPage; nothing numeric is derived client-side. This file mirrors
// the (removed) client derivations: statusClass,
// timeBucket, recordIsError (canonical metrics.Record.IsError),
// recordErrorEntries, percentile, and the xpNodeCard KPI sets. Time buckets
// are SERVER-local time.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

const (
	// chartMaxBuckets caps the traffic chart's bucket count. The ladder picks
	// the smallest step that fits the window in chartMaxBuckets buckets;
	// clock-aligning the window start can add ONE extra partial bucket
	// (≤ chartMaxBuckets+1 buckets total).
	chartMaxBuckets = 30
	// chartMaxWindowMinutes is the arithmetic safety boundary, not a curated
	// UI range or tunable: a minute window must fit Go's duration domain.
	chartMaxWindowMinutes = int64(math.MaxInt64) / int64(time.Minute)
	// xpNodeCap mirrors the explorer gallery's per-dimension node cap (the
	// single owner now - the client no longer applies its own cap).
	xpNodeCap = 24
	// sparkBuckets is the per-entity trend sparkline bucket count.
	sparkBuckets = 24
	// pctMinSamples suppresses percentile stats below this many samples
	// (tail-latency numbers from a handful of requests are noise).
	pctMinSamples = 4
)

// chartLadderMs is the nice clock-aligned ladder of candidate chart bucket
// widths, in MILLISECONDS (internal implementation detail - deliberately NOT
// config): the chart picks the smallest step with ceil(span/step) <=
// chartMaxBuckets so bucket edges land on clock marks instead of arbitrary
// span/30 fractions.
var chartLadderMs = []int64{
	time.Minute.Milliseconds(), (2 * time.Minute).Milliseconds(), (5 * time.Minute).Milliseconds(),
	(10 * time.Minute).Milliseconds(), (15 * time.Minute).Milliseconds(), (30 * time.Minute).Milliseconds(),
	time.Hour.Milliseconds(), (2 * time.Hour).Milliseconds(), (6 * time.Hour).Milliseconds(),
	(12 * time.Hour).Milliseconds(), (24 * time.Hour).Milliseconds(), (7 * 24 * time.Hour).Milliseconds(),
}

// chartStepMs returns the smallest ladder step whose ceil(span/step) fits
// chartMaxBuckets. Beyond the fixed ladder, whole-week steps double until
// they fit, keeping even decades of All history bounded to the same payload.
func chartStepMs(span int64) int64 {
	needed := span / chartMaxBuckets
	if span%chartMaxBuckets > 0 {
		needed++
	}
	for _, step := range chartLadderMs {
		if step >= needed {
			return step
		}
	}
	step := chartLadderMs[len(chartLadderMs)-1]
	for step < needed {
		step *= 2
	}
	return step
}

// AggAPI computes dashboard aggregates over the full durable history.
type AggAPI struct {
	buf     *metrics.Buffer
	store   *storage.Store // nil = no durable storage: aggregates over the ring only
	timeout time.Duration
	// One immutable last result per expensive surface. No retained history,
	// TTL, or per-scope map: memory stays bounded by the response shapes.
	chartMemo    aggregateMemo[chartMemoPayload]
	explorerMemo aggregateMemo[explorerPayload]
	projection   historyProjection
	modelRules   modelRuleCache
	// Optional state providers wired by main.go so HandleBootstrap can carry
	// every first-paint surface in one payload. Nil = section omitted.
	Dash     func() any // effective dashboard config (config.Map)
	Pause    func() any // operator pause state (same shape as GET /admin/pause)
	Throttle func() any // provider limit state (same shape as GET /admin/throttle)
	Debug    func() any // operator debug sessions (same shape as GET /admin/debug)
	// ModelCanon serves the effective model-canonicalization rules (single
	// owner config.CanonicalModel) for the bootstrap payload; the fold reads
	// it via canonizer() per request. Nil = no canonicalization (raw
	// spellings group as-is - the pre-canonicalization behavior).
	ModelCanon func() config.ModelCanon
}

// modelCanonizer applies the configured model-canonicalization pipeline to
// one raw model spelling, compiled once per request (patterns are never
// compiled per record) and memoized per raw name: distinct models are few,
// but a full-history store scan maps thousands of rows through the rules.
type modelCanonizer struct {
	exec     []config.ModelRuleExec
	memo     map[string]string
	identity string
}

func (m *modelCanonizer) model(name string) string {
	if m == nil {
		return name
	}
	if c, ok := m.memo[name]; ok {
		return c
	}
	c := config.ApplyModelRules(m.exec, name)
	m.memo[name] = c
	return c
}

// canonizer snapshots the effective model-canonicalization rules once. The
// provider reads the live config under the caller's lock (main.go liveMu);
// called a bounded number of times per request (one per fold helper entry).
func (a *AggAPI) canonizer() *modelCanonizer {
	rules := a.modelCanon()
	identity := modelCanonIdentity(rules)
	return &modelCanonizer{exec: a.modelRules.get(identity, rules).exec, memo: map[string]string{}, identity: identity}
}

func (a *AggAPI) modelCanon() config.ModelCanon {
	if a.ModelCanon == nil {
		return config.ModelCanon{}
	}
	return a.ModelCanon()
}

// The serialized rules identify the exact snapshot compiled for a fold.
// ModelCanon contains only strings/bools/slices, so marshaling cannot fail.
func modelCanonIdentity(rules config.ModelCanon) string {
	b, _ := json.Marshal(rules)
	return string(b)
}

// NewAggAPI binds the aggregate handlers to the ring and (optional) store.
func NewAggAPI(buf *metrics.Buffer, store *storage.Store, queryTimeout time.Duration) *AggAPI {
	return &AggAPI{buf: buf, store: store, timeout: queryTimeout}
}

// BootRingCapMul sizes the full-snapshot record cap (SetSnapshotLimit) as a
// multiple of dash_log_rows - the first log page plus scroll-through
// headroom before the log pages older history from the durable store.
// Internal derivation, not a tunable (same shape as storage's WriteTrackCap).
const BootRingCapMul = 8

// queryCtx is the single choke point that bounds a dashboard aggregate
// SELECT by StorageQueryTimeout. Validate requires > 0; a 0 fail-closes
// (deadline already expired), matching storage.withQueryTimeout.
func (a *AggAPI) queryCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), a.timeout)
}

// ---------- record materialization (shared by every endpoint) ----------

// contrib is one request's contribution to the aggregates: the subset of
// Record fields any dashboard metric reads, derived identically from a stored
// row or from a live ring record.
type contrib struct {
	id         string
	start      int64
	status     int
	isErr      bool
	has429     bool
	cost       float64
	in         int64
	out        int64
	cacheR     int64
	reason     int64
	answer     int64
	tools      int64
	ttft       int64
	tps        float64
	client     string
	prov       string
	model      string
	key        string
	conv       string
	parentConv string
	toolsL     []string
	ent        []errEnt
	// Dictionary IDs belong to the projection that materialized this row.
	// Unprojected ring/pending records carry zero IDs and are interned by
	// the same query-dimension adapter at the aggregation boundary.
	dimensionIDs      [dimTool]uint32
	rowIndex          uint32 // one-based projected position; zero is a ring/pending extra
	toolIDs, errorIDs []uint32
	// live marks an in-flight pending row (never a stored/finalized record).
	// paused/stream/throttled are only meaningful when live is set - they
	// drive the explorer status classes "paused", "throttled", and "streaming".
	live      bool
	paused    bool
	throttled bool
	stream    bool
}

type errEnt struct {
	typ      string
	code     string
	msg      string
	absorbed bool
	at       int64
}

// fromRecord maps a live ring record (Attempts already decoded) into a
// contrib. The model field is CANONICALIZED here - the single lowest choke
// point every grouping surface (explorer fold, scope filters, rail counts,
// node-cards) reads - while records, the log leaf, debug session matching,
// and purge/export keep the exact stored spelling.
func fromRecord(r *metrics.Record, mcz *modelCanonizer) contrib {
	c := contrib{
		id:         r.ID,
		start:      r.Start.UnixMilli(),
		status:     r.StatusCode,
		isErr:      r.IsError(),
		has429:     r.HasRateLimit(),
		cost:       r.Cost,
		in:         r.Usage.InputTokens,
		out:        r.Usage.OutputTokens,
		cacheR:     r.Usage.CacheReadTokens,
		reason:     r.Usage.ReasoningTokens,
		answer:     r.AnswerTokens,
		tools:      int64(r.ToolCalls),
		ttft:       r.TTFTMs,
		tps:        recordTPS(r),
		client:     r.Client,
		prov:       r.Provider,
		model:      mcz.model(r.Model),
		key:        r.KeyHash,
		conv:       r.ConversationID,
		parentConv: r.ParentConversationID,
		toolsL:     r.ToolNames,
	}
	c.ent = errorEntries(&metrics.Record{
		StatusCode: r.StatusCode, ErrorType: r.ErrorType, ErrorCode: r.ErrorCode,
		ErrorMsg: r.ErrorMsg, Attempts: r.Attempts, Start: r.Start,
	})
	return c
}

// fromPending maps an in-flight buffer snapshot. Live rows are never errors
// (status 0 is "not done", not a transport failure) and carry the live
// paused/stream flags the explorer status dimension groups on.
func fromPending(r *metrics.Record, mcz *modelCanonizer) contrib {
	c := fromRecord(r, mcz)
	c.live = true
	c.paused = r.Paused
	c.throttled = r.Throttled
	c.stream = r.Stream
	c.isErr = false
	c.ent = nil // mid-flight attempts are not explorer error events
	return c
}

// eachPending folds current in-flight rows (Buffer.pending). skip is the
// id set already counted by streamWindow (Record-then-end race). KPI/chart
// stay finalized-only (streamWindow).
func eachPending(records []*metrics.Record, skip map[string]struct{}, mcz *modelCanonizer, fn func(*contrib) error) error {
	for _, r := range records {
		if r == nil || r.ID == "" {
			continue
		}
		if skip != nil {
			if _, ok := skip[r.ID]; ok {
				continue
			}
		}
		c := fromPending(r, mcz)
		if err := fn(&c); err != nil {
			return err
		}
	}
	return nil
}

// recordTPS prefers the stored overall_tps, then decode-window gen_tps.
func recordTPS(r *metrics.Record) float64 {
	if r.OverallTPS > 0 {
		return r.OverallTPS
	}
	return r.DecodeTPS
}

// rowColumns is the SELECT list the chart and explorer scans stream. It is
// the single owner of which columns dashboard aggregates may read; key order
// must stay in lockstep between rowColumns and scanContrib.
const rowColumns = `id, started_at, status_code, error_type, error_code, error_msg, attempts,
	client, provider, model, key_hash, conversation_id, parent_conversation_id,
	input_tokens, output_tokens, cache_read_tokens, reasoning_tokens, answer_tokens,
	tool_calls, cost, ttft_ms, overall_tps, gen_tps, tool_names`

// scanContrib decodes one scanned row into a contrib (rowColumns order).
func scanContrib(rows *sql.Rows, mcz *modelCanonizer) (contrib, error) {
	var c contrib
	var startedAt, status, ttft int64
	var overall, gen float64
	var errType, errCode, errMsg string
	var attemptsJSON, toolNamesJSON []byte
	if err := rows.Scan(&c.id, &startedAt, &status, &errType, &errCode, &errMsg, &attemptsJSON,
		&c.client, &c.prov, &c.model, &c.key, &c.conv, &c.parentConv,
		&c.in, &c.out, &c.cacheR, &c.reason, &c.answer,
		&c.tools, &c.cost, &ttft, &overall, &gen, &toolNamesJSON); err != nil {
		return c, err
	}
	c.model = mcz.model(c.model)
	c.start = startedAt
	c.status = int(status)
	c.ttft = ttft
	if overall > 0 {
		c.tps = overall
	} else if gen > 0 {
		c.tps = gen
	}
	var atts []metrics.RetryAttempt
	if len(attemptsJSON) > 0 && string(attemptsJSON) != "null" {
		_ = json.Unmarshal(attemptsJSON, &atts)
	}
	if len(toolNamesJSON) > 0 && string(toolNamesJSON) != "null" {
		_ = json.Unmarshal(toolNamesJSON, &c.toolsL)
	}
	r := metrics.Record{
		StatusCode: c.status, ErrorType: errType, ErrorCode: errCode, ErrorMsg: errMsg,
		Attempts: atts, Start: time.UnixMilli(c.start),
	}
	c.isErr = r.IsError()
	c.has429 = r.HasRateLimit()
	c.ent = errorEntries(&r)
	return c, nil
}

// errorEntries is the Go mirror of the client's recordErrorEntries enumerator
// (final entry + absorbed-attempt entries, 429 flow control excluded) - the
// single owner of what counts as an error signature event.
func errorEntries(r *metrics.Record) []errEnt {
	var out []errEnt
	fin := r.StatusCode
	if fin != 429 && fin != 499 && (r.ErrorType != "" || fin >= 400) {
		typ := r.ErrorType
		if typ == "" {
			typ = "http_" + strconv.Itoa(fin)
		}
		code := r.ErrorCode
		if code == "" && fin != 0 {
			code = strconv.Itoa(fin)
		}
		out = append(out, errEnt{typ: typ, code: code, msg: r.ErrorMsg, at: r.Start.UnixMilli()})
	}
	for _, a := range r.Attempts {
		as := a.StatusCode
		if as == 429 {
			continue
		}
		typ := a.ErrorType
		if typ == "" {
			if as != 0 {
				typ = "http_" + strconv.Itoa(as)
			} else {
				typ = "transport"
			}
		}
		if as < 500 && (typ == "transport" || typ == "rate_limit") {
			continue
		}
		if as < 400 && a.ErrorType == "" {
			continue
		}
		code := a.ErrorCode
		if code == "" && as != 0 {
			code = strconv.Itoa(as)
		}
		at := a.At.UnixMilli()
		if at == 0 {
			at = r.Start.UnixMilli()
		}
		out = append(out, errEnt{typ: typ, code: code, msg: a.ErrorMsg, absorbed: true, at: at})
	}
	return out
}

// ringSnapshot captures potentially unflushed records with start >= since.
// Filter BEFORE the durable snapshot: commits arriving afterward are still
// deduped by its exact ID set, while already-written rows that were deleted
// externally can never be resurrected from their obsolete ring copies.
func (a *AggAPI) ringSnapshot(since int64) []*metrics.Record {
	out := make([]*metrics.Record, 0, 64)
	for _, r := range a.buf.Snapshot() {
		if r == nil {
			continue
		}
		if since > 0 && r.Start.UnixMilli() < since {
			continue
		}
		out = append(out, r)
	}
	if a.store == nil || len(out) == 0 {
		return out
	}
	ids := make([]string, len(out))
	for i, r := range out {
		ids[i] = r.ID
	}
	unwritten := a.store.FilterUnwritten(ids)
	if len(unwritten) == len(out) {
		return out
	}
	if len(unwritten) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(unwritten))
	for _, id := range unwritten {
		keep[id] = struct{}{}
	}
	n := 0
	for _, r := range out {
		if _, ok := keep[r.ID]; ok {
			out[n] = r
			n++
		}
	}
	return out[:n]
}

// streamWindow is the shared analytical read boundary. Snapshot the ring
// BEFORE acquiring durable history, then dedupe against that exact projection
// while its read lease is held. A racing commit is counted from one side only.
// prepare borrows the same projection metadata used by the subsequent fold.
func (a *AggAPI) streamWindow(ctx context.Context, since int64, mcz *modelCanonizer, prepare func(*projectionData), fn func(*contrib) error, finish func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ring := a.ringSnapshot(since)
	return a.withProjection(ctx, func(p *projectionData) error {
		if prepare != nil {
			prepare(p)
		}
		if p != nil {
			for i := range p.rows {
				c := &p.rows[i]
				if since > 0 && c.start < since {
					continue
				}
				if err := fn(c); err != nil {
					return err
				}
			}
		}
		for _, r := range ring {
			if p != nil {
				if _, dup := p.ids[r.ID]; dup {
					continue
				}
			}
			c := fromRecord(r, mcz)
			if err := fn(&c); err != nil {
				return err
			}
		}
		if finish != nil {
			if err := finish(); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
}

// ---------- status / time derivation (Go mirror of the removed client code) ----------

func statusClass(s int) string {
	switch {
	case s >= 200 && s < 300:
		return "2xx"
	case s == 499:
		return "cancel"
	case s >= 400 && s < 500:
		return "4xx"
	case s >= 500:
		return "5xx"
	default:
		return "err"
	}
}

// contribStatusClass is the Go mirror of the client's statusClass(r): finalized
// codes use statusClass(int); a live row is paused / throttled / streaming / pending.
// Never treat a stored Stream flag or status 0 as "currently streaming".
func contribStatusClass(c *contrib) string {
	if c != nil && c.live {
		if c.paused {
			return "paused"
		}
		if c.throttled {
			return "throttled"
		}
		if c.stream {
			return "streaming"
		}
		return "pending"
	}
	if c == nil {
		return "err"
	}
	return statusClass(c.status)
}

// ---------- KPI aggregate endpoint ----------

type kpiPayload struct {
	err      error
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	InFlight int64   `json:"in_flight"`
	Cost     float64 `json:"cost"`
	// Unit economics for the KPI band (server-derived, nil on a zero
	// denominator - the dashboard renders "-", never a fake 0):
	// blended $/req and $/Mtok over input+output tokens.
	CostPerReq  *float64 `json:"cost_per_req"`
	CostPerMTok *float64 `json:"cost_per_mtok"`
	InputTok    int64    `json:"input_tokens"`
	OutputTok   int64    `json:"output_tokens"`
	CacheRead   int64    `json:"cache_read_tokens"`
	Reasoning   int64    `json:"reasoning_tokens"`
	Answer      int64    `json:"answer_tokens"`
	ToolCalls   int64    `json:"tool_calls"`
	AvgTTFT     *float64 `json:"avg_ttft_ms"`
	AvgTPS      *float64 `json:"avg_tps"`
}

func (p kpiPayload) MarshalJSON() ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	type payload kpiPayload
	return json.Marshal(payload(p))
}

// kpi computes the global since-inception KPI aggregate (Totals + un-flushed
// ring records, exact written-id dedupe) plus the inline unit-economics
// fields. The ONE KPI surface - served inside /metrics/bootstrap.
func (a *AggAPI) kpi() kpiPayload {
	t := storage.Totals{}
	ring := a.buf.Snapshot()
	if a.store != nil {
		t, ring = a.store.TotalsAndUnwritten(ring)
	}
	for _, r := range ring {
		t.Add(r)
	}
	// Unit economics derive inline from the folded totals (no storage change);
	// nil when the denominator is 0. Both denominators count only
	// cost-REPORTING requests (Totals.CostReqs / CostInOut, maintained by the
	// shared Totals.Add), so unpriced providers never dilute the rate.
	return kpiPayload{
		err:         t.Err(),
		Requests:    t.Requests,
		Errors:      t.Errors,
		InFlight:    a.buf.Counters().InFlight,
		Cost:        t.Cost,
		CostPerReq:  metrics.ScaledRatio(t.Cost, float64(t.CostReqs), 1),
		CostPerMTok: metrics.ScaledRatio(t.Cost, t.CostInOut, 1e6),
		InputTok:    t.InputTok,
		OutputTok:   t.OutputTok,
		CacheRead:   t.CacheRead,
		Reasoning:   t.Reasoning,
		Answer:      t.Answer,
		ToolCalls:   t.ToolCalls,
		AvgTTFT:     t.AvgTTFT(),
		AvgTPS:      t.AvgTPS(),
	}
}

// ---------- traffic chart endpoint ----------

// chartBucketJSON is one bucket's aggregate row.
type chartBucketJSON struct {
	T      int64      `json:"t"` // bucket START edge (from_ms + i*bucket_ms); the client plots slot centers
	Req    int64      `json:"req"`
	Err    int64      `json:"err"`
	In     int64      `json:"in"`
	Out    int64      `json:"out"`
	Cache  int64      `json:"cache"` // cached prompt tokens (subset of In)
	Reason int64      `json:"reason"`
	Cost   float64    `json:"cost"`
	TTFT   []*float64 `json:"ttft"` // [p50, p95, p99]; nil entries below pctMinSamples samples
	TPS    []*float64 `json:"tps"`
}

type chartPayload struct {
	NowMs    int64             `json:"now_ms"`
	FromMs   int64             `json:"from_ms"`   // CLOCK-ALIGNED window start (floored onto a ladder step)
	BucketMs int64             `json:"bucket_ms"` // exact integer bucket width - single source of truth
	TTFTP    []*float64        `json:"ttft_p"`    // period-wide [p50, p95, p99] (the totals strip)
	TPSP     []*float64        `json:"tps_p"`
	Buckets  []chartBucketJSON `json:"buckets"`
}

type bAcc struct {
	t, req, err, in, out, cache, reason int64
	cost                                float64
	ttftN, tpsN                         int
}

// chartFold accumulates one scoped chart series over a stream of
// contributions. The stream is window-bounded (streamWindow's since), so
// every contribution lands in a bucket.
type chartFold struct {
	from, step int64
	count      int
	buckets    []bAcc
	extraTTFT  []int64 // only unflushed/ring values; durable samples stay in the shared order
	extraTPS   []float64
	ttftBucket []uint8
	tpsBucket  []uint8
	membership []uint8
	order      projectionMetrics
	statusCode string
	fs         []scopeFilter
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
	if p == nil {
		return
	}
	f.membership = make([]uint8, len(p.rows))
	f.order = p.metrics
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
	for _, term := range [...]struct {
		dst   *int64
		value int64
	}{
		{&b.in, c.in}, {&b.out, c.out}, {&b.cache, c.cacheR}, {&b.reason, c.reason},
	} {
		var err error
		*term.dst, err = metrics.SumCounts(*term.dst, term.value)
		if err != nil {
			return err
		}
	}
	var err error
	b.cost, err = metrics.SumValues(b.cost, c.cost)
	if err != nil {
		return err
	}
	if c.rowIndex > 0 {
		f.membership[c.rowIndex-1] = uint8(idx + 1)
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
	return nil
}

func (f *chartFold) payload(now int64) chartPayload {
	out := make([]chartBucketJSON, f.count)
	ttftCounts, tpsCounts := make([]int, f.count), make([]int, f.count)
	for i, b := range f.buckets {
		ttftCounts[i], tpsCounts[i] = b.ttftN, b.tpsN
	}
	ttft, periodTTFT := rankPercentiles(f.order.ttft, f.membership, ttftCounts, f.extraTTFT, f.ttftBucket)
	tps, periodTPS := rankPercentiles(f.order.tps, f.membership, tpsCounts, f.extraTPS, f.tpsBucket)
	for i, b := range f.buckets {
		o := chartBucketJSON{T: b.t, Req: b.req, Err: b.err, In: b.in, Out: b.out,
			Cache: b.cache, Reason: b.reason, Cost: b.cost}
		o.TTFT = ttft[i]
		o.TPS = tps[i]
		out[i] = o
	}
	return chartPayload{NowMs: now, FromMs: f.from, BucketMs: f.step,
		TTFTP: periodTTFT, TPSP: periodTPS, Buckets: out}
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
	if !rejectUnlessGet(w, r) {
		return
	}
	winMin, ok := windowParam(r)
	if !ok {
		http.Error(w, `{"error":"invalid window"}`, http.StatusBadRequest)
		return
	}
	fs, statusCode, ok := parseScope(r)
	if !ok {
		http.Error(w, `{"error":"invalid filter"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	payload, err := a.chart(ctx, winMin, fs, statusCode, time.Now().UnixMilli())
	if err != nil {
		http.Error(w, `{"error":"aggregate failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, payload)
}

// windowParam owns the API's time-window domain: all history or positive
// integral minutes bounded to safe duration arithmetic. CHART_WINDOWS owns
// the UI's curated choices, not a second whitelist that could drift here.
func windowParam(r *http.Request) (int, bool) {
	v := r.URL.Query().Get("window")
	if v == "" || v == "all" {
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 || n > chartMaxWindowMinutes || strconv.FormatInt(n, 10) != v {
		return 0, false
	}
	return int(n), true
}

// minStartScoped returns the oldest start among IN-SCOPE history (store +
// ring), or now when nothing matches yet (a degenerate one-bucket All
// chart). The unscoped durable minimum is maintained on projection updates;
// scoped minima read the same decoded contributions and canonical predicate
// as the chart fold, never a separate SQL matcher or full-history decode.
func minStartScoped(ctx context.Context, a *AggAPI, mcz *modelCanonizer, fs []scopeFilter, statusCode string, fallback int64) (int64, error) {
	min := fallback
	upd := func(c *contrib) {
		if scopedMatch(c, statusCode, fs) && c.start < min {
			min = c.start
		}
	}
	ring := a.ringSnapshot(0)
	err := a.withProjection(ctx, func(p *projectionData) error {
		if p != nil {
			fs = bindScope(fs, p, mcz)
			if len(fs) == 0 && statusCode == "" {
				if p.hasStart && p.minStart < min {
					min = p.minStart
				}
			} else {
				for i := range p.rows {
					upd(&p.rows[i])
				}
			}
		}
		for _, r := range ring {
			if p != nil {
				if _, duplicate := p.ids[r.ID]; duplicate {
					continue
				}
			}
			c := fromRecord(r, mcz)
			upd(&c)
		}
		return ctx.Err()
	})
	return min, err
}

// ---------- bootstrap: shared initial-page and live state ----------

// bootstrapPayload embeds the ring snapshot shape (records, in-flight
// pending registry, counters, cursor metadata) and adds every state surface
// a fresh page renders before the slower chart/explorer fetches land.
type bootstrapPayload struct {
	metrics.ObservedSnapshot
	DashboardVersion string        `json:"dashboard_version"`
	KPI              kpiPayload    `json:"kpi"`
	Dash             any           `json:"dash,omitempty"`
	Pause            any           `json:"pause,omitempty"`
	Throttle         any           `json:"throttle,omitempty"`
	Debug            any           `json:"debug,omitempty"`
	Storage          storageSignal `json:"storage"`
}

type storageSignal struct {
	Enabled bool   `json:"enabled"`
	Dropped uint64 `json:"dropped"`
}

// HandleBootstrap is THE dashboard's live endpoint - one round trip carrying
// everything the page renders outside the slow chart/explorer scans: the
// ring snapshot (full and capped on a fresh boot via SnapshotRequest;
// a ?since=&feed= delta on tick/poll resumes), the global since-inception
// KPI aggregate, the effective dashboard config, and the operator pause/
// limit state when wired. In-memory only - no history scan. The client
// filters the records at render (log scope is client-side), so the payload
// takes no scope parameters. The traffic chart and explorer breakdown keep
// their own endpoints (the slow full-history scans).
func (a *AggAPI) HandleBootstrap(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGet(w, r) {
		return
	}
	// State and the running asset version must come from the current process.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, a.bootstrap(a.buf.SnapshotRequest(r)))
}

// bootstrap owns the initial HTML and live endpoint payload alike. A new
// document always passes a full snapshot; endpoint resumes use the
// buffer's existing feed/cursor trust boundary. No durable history scan.
func (a *AggAPI) bootstrap(snapshot metrics.Snapshot) bootstrapPayload {
	observed := metrics.ObserveSnapshot(snapshot, a.ObserveModels(true, snapshot.Records, snapshot.InFlightRecords))
	payload := bootstrapPayload{ObservedSnapshot: observed, KPI: a.kpi(), DashboardVersion: dashboardVersion}
	if a.store != nil {
		payload.Storage = storageSignal{Enabled: true, Dropped: a.store.Dropped()}
	}
	if a.Dash != nil {
		payload.Dash = a.Dash()
	}
	if a.Pause != nil {
		payload.Pause = a.Pause()
	}
	if a.Throttle != nil {
		payload.Throttle = a.Throttle()
	}
	if a.Debug != nil {
		payload.Debug = a.Debug()
	}
	return payload
}

// ---------- request scoping (chart, explorer, footer, log) ----------

// scopeFilter is one explorer filter (dim:value) - the same dims the client's
// request log is scoped by; the count endpoint matches them server-side over
// ALL history so "scope X of Y" is a since-inception number on both sides.
type scopeFilter struct {
	dim, id  string
	modelIDs []bool // query-bound raw-model membership; never an API parameter
}

// matches reports whether one contribution satisfies the filter; the Go
// mirror of the client's recordMatchesDim (same derivations: statusClass,
// timeBucket, multi-valued tools, error signatures from errorEntries).
func (f scopeFilter) matches(c *contrib) bool {
	switch f.dim {
	case "error":
		for _, e := range c.ent {
			if strings.Join([]string{e.typ, e.code, e.msg}, "|") == f.id {
				return true
			}
		}
		return false
	case "status":
		return contribStatusClass(c) == f.id
	case "time":
		return metrics.TimeBucket(time.UnixMilli(c.start)) == f.id
	case "tool":
		for _, t := range c.toolsL {
			if t == "" {
				continue // same empty-name skip as the explorer fold below
			}
			if t == f.id {
				return true
			}
		}
		return false
	case "client":
		return c.client == f.id
	case "provider":
		return c.prov == f.id
	case "model":
		if id := c.dimensionIDs[dimModel]; c.dimensionIDs[dimStatus] != 0 && f.modelIDs != nil {
			return int(id) < len(f.modelIDs) && f.modelIDs[id]
		}
		return c.model == f.id
	case "conversation":
		return c.conv == f.id
	case "key":
		return c.key == f.id
	}
	return false
}

func parseScopeFilters(r *http.Request) ([]scopeFilter, bool) {
	vals, ok := r.URL.Query()["f"]
	if !ok {
		return nil, true
	}
	if len(vals) > 64 {
		return nil, false
	}
	out := make([]scopeFilter, 0, len(vals))
	for _, v := range vals {
		if v == "" || len(v) > 2048 {
			return nil, false
		}
		i := strings.IndexByte(v, ':')
		if i <= 0 {
			return nil, false
		}
		dim, id := v[:i], v[i+1:]
		if !validDims[dim] {
			return nil, false
		}
		out = append(out, scopeFilter{dim: dim, id: id})
	}
	return out, true
}

// liveStatusFilter is the named in-flight pills the Requests-card status
// dropdown can send as s= (streaming / paused / throttled / pending). Distinct
// from explorer status CLASSES (2xx/4xx/…) which travel as f=status:2xx.
func liveStatusFilter(s string) bool {
	switch s {
	case "paused", "throttled", "streaming", "pending":
		return true
	}
	return false
}

// parseScope parses a scoping request: explorer filters (f=dim:id, repeated -
// same dims the log is scoped by) plus the Requests-card status dropdown
// (s=200: an exact HTTP code, OR s=streaming/paused/…: a live in-flight
// class). Unknown s= is rejected (deny by default) - never silently ignored.
func parseScope(r *http.Request) (fs []scopeFilter, statusCode string, ok bool) {
	fs, ok = parseScopeFilters(r)
	if !ok {
		return nil, "", false
	}
	if s := r.URL.Query().Get("s"); s != "" {
		if liveStatusFilter(s) {
			statusCode = s
		} else {
			n, err := strconv.Atoi(s)
			if err != nil || n < 0 || n > 9999 {
				return nil, "", false
			}
			statusCode = s
		}
	}
	return fs, statusCode, true
}

// scopedMatch reports whether a contribution satisfies the full log scope
// (Requests-card status filter + explorer filters).
func scopedMatch(c *contrib, statusCode string, fs []scopeFilter) bool {
	if statusCode != "" {
		if liveStatusFilter(statusCode) {
			if contribStatusClass(c) != statusCode {
				return false
			}
		} else if strconv.Itoa(c.status) != statusCode {
			return false
		}
	}
	for _, f := range fs {
		if !f.matches(c) {
			return false
		}
	}
	return true
}

// ---------- explorer breakdown endpoint ----------

// jsonEnt is the per-entity aggregate a node-card renders - every field the
// per-dimension KPI sets need, precomputed server-side.
type jsonEnt struct {
	Conversation      *conversationInfo `json:"conversation,omitempty"`
	Name              string            `json:"name"`
	N                 int64             `json:"n"`
	Cost              float64           `json:"cost"`
	In                int64             `json:"in"`
	Out               int64             `json:"out"`
	Cache             int64             `json:"cache"`
	Reasoning         int64             `json:"reasoning"`
	ErrFinal          int64             `json:"err_final"`
	RateLimitRequests int64             `json:"rate_limit_requests"`
	Tools             int64             `json:"tools"`
	// Blended $/Mtok over this entity's COST-REPORTING in+out tokens
	// (Totals' CostInOut rule, mirrored per entity) - nil when the
	// denominator is 0. An unpriced provider's tokens never dilute it.
	CostPerMTok *float64 `json:"cost_per_mtok"`
	TTFTP50     *float64 `json:"ttft_p50"`
	TTFTP95     *float64 `json:"ttft_p95"`
	TPSP50      *float64 `json:"tps_p50"`
	TPSP95      *float64 `json:"tps_p95"`
	ErrEvents   int64    `json:"err_events"`
	Code        string   `json:"code"`
	LastMs      int64    `json:"last_ms"`
	Spark       []int    `json:"spark"`
	SparkErr    []int    `json:"spark_err"`
}

type explorerPayload struct {
	Conversations conversationSummary `json:"conversation_summary"`
	Dim           string              `json:"dim"`
	Total         int64               `json:"total"`
	ErrorTot      int64               `json:"error_total"`
	Rail          map[string]int      `json:"rail"`
	Groups        []jsonEnt           `json:"groups"`
	Scope         scopePayload        `json:"scope"`
}

// scopePayload follows every filter, including the active dimension's own
// filter. The gallery's Total/ErrorTot intentionally exclude that one filter.
type scopePayload struct {
	Matches int64 `json:"matches"`
	Errors  int64 `json:"errors"`
}

type entAcc struct {
	id                uint32
	name              string
	n                 int
	cost              float64
	in                int64
	out               int64
	cacheR            int64
	reason            int64
	tools             int64
	errFin            int
	rateLimitRequests int
	lastRequest       string // first membership in the current row; no history-ID set
	ttftS             []int64
	tpsS              []float64
	ttftN             int
	tpsN              int
	startCap          int
	// cost-REPORTING in+out tokens: the blended-$ denominator (unpriced
	// records add tokens but not cost, so they must not dilute).
	costInOut float64
	starts    []int64
	startEr   []bool
	// error dim only
	code     string
	lastMs   int64
	affected map[string]struct{}
}

var validDims = func() map[string]bool {
	dims := make(map[string]bool, len(dimensionNames))
	for _, name := range dimensionNames {
		dims[name] = true
	}
	return dims
}()

// A row may belong to multiple groups, including repeated tool/error
// occurrences. Spans index one flat membership list without per-row slices.
type explorerMemberSpan struct{ start, count uint32 }

// explorerFold accumulates the per-dimension breakdown (groups, rail
// distinct-value counts, full-scope footer counts, error-affected sets) over
// one stream of contributions. The footer never needs a second history scan.
type explorerFold struct {
	lineage       conversationLineage
	projectedRail bool
	dim           string
	dimIndex      int
	statusCode    string
	allFS         []scopeFilter
	grpFS         []scopeFilter
	dims          queryDimensions
	railSeen      [dimCount][]uint64
	railCount     [dimCount]int
	groups        map[uint32]*entAcc
	maxGroupID    uint32
	reserveStarts bool
	order         projectionMetrics
	memberSpans   []explorerMemberSpan
	members       []uint32
	pendingIDs    map[string]struct{}
	seen          map[string]struct{}
	total         int64
	errTot        int64
	scope         scopePayload
}

func newExplorerFold(dim, statusCode string, allFS []scopeFilter) *explorerFold {
	// Cross-filter: the breakdown ignores the active dim's own filter value.
	grpFS := allFS[:0:0]
	for _, f := range allFS {
		if f.dim != dim {
			grpFS = append(grpFS, f)
		}
	}
	e := &explorerFold{
		dim: dim, statusCode: statusCode, allFS: allFS, grpFS: grpFS,
		groups: make(map[uint32]*entAcc),
		seen:   make(map[string]struct{}),
	}
	for i, name := range dimensionNames {
		if name == dim {
			e.dimIndex = i
			break
		}
	}
	return e
}

func (e *explorerFold) prepare(p *projectionData, mcz *modelCanonizer) {
	e.dims.prepare(p, mcz)
	e.allFS = bindScope(e.allFS, p, mcz)
	e.grpFS = bindScope(e.grpFS, p, mcz)
	// Full galleries reserve exact projected start counts per group;
	// filtered views stay lazy instead of allocating for unseen history.
	e.reserveStarts = p != nil && e.statusCode == "" && len(e.grpFS) == 0
	if p != nil {
		e.order = p.metrics
		// The unfiltered rail is exactly the shared dictionary's occupied
		// memberships. Only unprojected ring/pending names can add anything;
		// scoped rails continue to follow the canonical per-row predicate.
		e.projectedRail = e.statusCode == "" && len(e.allFS) == 0
		if e.projectedRail {
			for dim, dictionary := range e.dims.base {
				for id, n := range dictionary.counts {
					if id > 0 && n > 0 {
						e.markRail(dim, uint32(id))
					}
				}
			}
		}
		e.memberSpans = make([]explorerMemberSpan, len(p.rows))
		e.members = make([]uint32, 0, len(p.rows))
		// Only pending candidates can be replayed by the captured live snapshot.
		// Never allocate a second full-history request-ID set for this query.
		for id := range e.pendingIDs {
			if _, finalized := p.ids[id]; finalized {
				e.seen[id] = struct{}{}
			}
		}
	}
}

func (e *explorerFold) markRail(dim int, id uint32) {
	if id == 0 {
		return
	}
	word, bit := int(id/64), uint64(1)<<(id%64)
	seen := e.railSeen[dim]
	if word >= len(seen) {
		seen = append(seen, make([]uint64, word+1-len(seen))...)
		e.railSeen[dim] = seen
	}
	if seen[word]&bit == 0 {
		seen[word] |= bit
		e.railCount[dim]++
	}
}

func (e *explorerFold) fold(c *contrib) error {
	if _, pending := e.pendingIDs[c.id]; pending {
		e.seen[c.id] = struct{}{}
	}
	inRail := scopedMatch(c, e.statusCode, e.allFS)
	if inRail {
		e.scope.Matches++
		if c.isErr {
			e.scope.Errors++
		}
	}
	// grpFS is allFS minus the active dim. Do not AND with inRail -
	// that made inGroup ≡ inRail and collapsed the gallery to 100%.
	inGroup := scopedMatch(c, e.statusCode, e.grpFS)
	e.lineage.observe(c, inRail, inGroup && e.dimIndex == dimConversation)
	if inGroup {
		e.total++
		if c.isErr {
			e.errTot++
		}
	}
	if !inRail && !inGroup {
		return nil
	}
	needsRail := inRail && !(e.projectedRail && c.rowIndex > 0)
	for dim, id := range e.dims.singles(c) {
		if !needsRail && dim != e.dimIndex {
			continue
		}
		if id == 0 {
			continue
		}
		if needsRail {
			e.markRail(dim, id)
		}
		if inGroup && dim == e.dimIndex {
			if _, err := e.foldGroup(id, c, true); err != nil {
				return err
			}
		}
	}
	if needsRail || e.dimIndex == dimTool {
		for i := range c.toolsL {
			id := e.dims.id(dimTool, c, i)
			if id == 0 {
				continue
			}
			if needsRail {
				e.markRail(dimTool, id)
			}
			if inGroup && e.dimIndex == dimTool {
				if _, err := e.foldGroup(id, c, true); err != nil {
					return err
				}
			}
		}
	}
	if needsRail || e.dimIndex == dimError {
		for i, ent := range c.ent {
			id := e.dims.id(dimError, c, i)
			if needsRail {
				e.markRail(dimError, id)
			}
			if inGroup && e.dimIndex == dimError {
				g, err := e.foldGroup(id, c, false)
				if err != nil {
					return err
				}
				if ent.at > g.lastMs {
					g.lastMs = ent.at
				}
				if g.code == "" {
					g.code = ent.code
				}
			}
		}
	}
	return nil
}

func (e *explorerFold) foldGroup(dimKey uint32, c *contrib, addStart bool) (*entAcc, error) {
	g := e.groups[dimKey]
	if g == nil {
		g = &entAcc{id: dimKey, name: e.dims.name(e.dimIndex, dimKey)}
		if dimKey > e.maxGroupID {
			e.maxGroupID = dimKey
		}
		if e.reserveStarts {
			g.startCap = e.dims.count(e.dimIndex, dimKey)
		}
		if g.startCap > 0 {
			g.starts = make([]int64, 0, g.startCap)
			g.startEr = make([]bool, 0, g.startCap)
		}
		e.groups[dimKey] = g
	}
	// Health counts are affected requests, while tool/error occurrences still
	// retain their existing event, sample, cost and token multiplicity. Error
	// groups already need the affected-ID set for their headline and sparks;
	// other groups only need the last ID because one row's visits are contiguous.
	var firstRequest bool
	if e.dimIndex == dimError {
		if g.affected == nil {
			g.affected = make(map[string]struct{})
		}
		_, duplicate := g.affected[c.id]
		firstRequest = !duplicate
		if firstRequest {
			g.affected[c.id] = struct{}{}
			addStart = true
		}
	} else {
		firstRequest = g.n == 0 || g.lastRequest != c.id
		g.lastRequest = c.id
	}
	g.n++
	var err error
	g.cost, err = metrics.SumValues(g.cost, c.cost)
	if err != nil {
		return nil, err
	}
	if c.cost > 0 {
		g.costInOut, err = metrics.SumValues(g.costInOut, float64(c.in), float64(c.out))
		if err != nil {
			return nil, err
		}
	}
	for _, term := range [...]struct {
		dst   *int64
		value int64
	}{
		{&g.in, c.in}, {&g.out, c.out}, {&g.cacheR, c.cacheR}, {&g.reason, c.reason}, {&g.tools, c.tools},
	} {
		*term.dst, err = metrics.SumCounts(*term.dst, term.value)
		if err != nil {
			return nil, err
		}
	}
	if firstRequest {
		if c.isErr {
			g.errFin++
		}
		if c.has429 {
			g.rateLimitRequests++
		}
	}
	if c.rowIndex > 0 {
		span := &e.memberSpans[c.rowIndex-1]
		if span.count == 0 {
			span.start = uint32(len(e.members))
		}
		span.count++
		e.members = append(e.members, dimKey)
	}
	// Durable samples stay in immutable metric order. Only raw ring/pending
	// extras need per-group values; counts preserve occurrence multiplicity.
	if validMetricSample(c.ttft) {
		g.ttftN++
		if c.rowIndex == 0 {
			g.ttftS = append(g.ttftS, c.ttft)
		}
	}
	if validMetricSample(c.tps) {
		g.tpsN++
		if c.rowIndex == 0 {
			g.tpsS = append(g.tpsS, c.tps)
		}
	}
	if addStart {
		g.starts = append(g.starts, c.start)
		g.startEr = append(g.startEr, c.isErr)
	}
	return g, nil
}

func (e *explorerFold) payload() explorerPayload {
	conversations, conversationGroups := e.lineage.resolve()
	// Rank before deriving percentiles and sparklines: discarded groups never
	// allocate response arrays or perform sample selection. Names break ties
	// deterministically, independently of dictionary insertion or map order.
	count := func(g *entAcc) int {
		if e.dimIndex == dimError {
			return len(g.affected)
		}
		return g.n
	}
	top := make([]*entAcc, 0, min(len(e.groups), xpNodeCap))
	for _, g := range e.groups {
		n := count(g)
		at := sort.Search(len(top), func(i int) bool {
			other := count(top[i])
			return n > other || n == other && g.name < top[i].name
		})
		if at == xpNodeCap {
			continue
		}
		if len(top) < xpNodeCap {
			top = append(top, nil)
		}
		copy(top[at+1:], top[at:])
		top[at] = g
	}
	// Resolve compact selected-group membership once, then both metrics read
	// the same row spans. Emitting every occurrence preserves duplicate tools
	// and error events while error-card N/sparks remain affected-request based.
	selected := make([]uint8, int(e.maxGroupID)+1)
	ttftCounts, tpsCounts := make([]int, len(top)), make([]int, len(top))
	var extraTTFT []int64
	var extraTPS []float64
	var ttftBuckets, tpsBuckets []uint8
	for i, g := range top {
		bucket := uint8(i + 1)
		selected[g.id] = bucket
		ttftCounts[i], tpsCounts[i] = g.ttftN, g.tpsN
		extraTTFT = append(extraTTFT, g.ttftS...)
		extraTPS = append(extraTPS, g.tpsS...)
		for range g.ttftS {
			ttftBuckets = append(ttftBuckets, bucket)
		}
		for range g.tpsS {
			tpsBuckets = append(tpsBuckets, bucket)
		}
	}
	membership := make([]uint8, len(e.members))
	for i, id := range e.members {
		membership[i] = selected[id]
	}
	visit := func(row uint32, emit func(uint8)) {
		if uint64(row) >= uint64(len(e.memberSpans)) {
			return
		}
		span := e.memberSpans[row]
		for _, bucket := range membership[span.start : span.start+span.count] {
			if bucket != 0 {
				emit(bucket)
			}
		}
	}
	wait, _ := rankPercentilesVisit(e.order.ttft, ttftCounts, extraTTFT, ttftBuckets, visit)
	speed, _ := rankPercentilesVisit(e.order.tps, tpsCounts, extraTPS, tpsBuckets, visit)
	ents := make([]jsonEnt, 0, len(top))
	for i, g := range top {
		ent := jsonEnt{
			Name: g.name, N: int64(count(g)), Cost: g.cost, In: g.in, Out: g.out,
			Cache: g.cacheR, Reasoning: g.reason, ErrFinal: int64(g.errFin),
			RateLimitRequests: int64(g.rateLimitRequests),
			Tools:             g.tools, Code: g.code, LastMs: g.lastMs,
		}
		if e.dimIndex == dimConversation {
			ent.Conversation = e.lineage.info(conversationGroups[g.name])
		}
		ent.CostPerMTok = metrics.ScaledRatio(g.cost, g.costInOut, 1e6)
		if e.dimIndex == dimError {
			ent.ErrEvents = int64(g.n) // fold counted failure EVENTS per group
		}
		ent.TTFTP50, ent.TTFTP95, ent.TPSP50, ent.TPSP95 =
			wait[i][0], wait[i][1], speed[i][0], speed[i][1]
		ent.Spark, ent.SparkErr = spark(g.starts, g.startEr)
		ents = append(ents, ent)
	}
	rail := make(map[string]int, dimCount)
	for dim, name := range dimensionNames {
		rail[name] = e.railCount[dim]
	}
	return explorerPayload{Dim: e.dim, Total: e.total, ErrorTot: e.errTot, Rail: rail, Groups: ents, Scope: e.scope, Conversations: conversations}
}

// HandleAggExplorer serves the per-dimension breakdown over ALL history. The
// optional f=dim:id filters (everything EXCEPT the active dimension's own
// value - the cross-filter rule: the gallery shows X's distribution over
// records filtered by all OTHER dimensions) scope both the breakdown and its
// share denominator; the rail distinct-value counts and footer scope totals
// follow the FULL filter set. Every count is
// since-inception - computed server-side over the durable history.
func (a *AggAPI) HandleAggExplorer(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGet(w, r) {
		return
	}
	dim := r.URL.Query().Get("dim")
	if !validDims[dim] {
		http.Error(w, `{"error":"invalid dim"}`, http.StatusBadRequest)
		return
	}
	allFS, statusCode, ok := parseScope(r)
	if !ok {
		http.Error(w, `{"error":"invalid filter"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	payload, err := a.explorer(ctx, dim, allFS, statusCode)
	if err != nil {
		http.Error(w, `{"error":"aggregate failed"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, payload)
}

// dimValue derives one record's value for a single-valued dimension and
// whether to count it (Go mirror of recordMatchesDim; empty keys never
// group, client parity).
func dimValue(dim string, c *contrib) (string, bool) {
	if field := dimField(dim, c); field != nil {
		return *field, *field != ""
	}
	switch dim {
	case "status":
		return contribStatusClass(c), true
	case "time":
		return metrics.TimeBucket(time.UnixMilli(c.start)), true
	}
	return "", false
}

// spark buckets a group's request counts into 24 equal spans over its OWN
// first→last window (Go mirror of the removed client bucketize(g.recs, 24)).
func spark(starts []int64, startEr []bool) (vol, errs []int) {
	vol = make([]int, sparkBuckets)
	errs = make([]int, sparkBuckets)
	if len(starts) == 0 {
		return vol, errs
	}
	t0, t1 := starts[0], starts[0]
	for _, t := range starts {
		if t < t0 {
			t0 = t
		}
		if t > t1 {
			t1 = t
		}
	}
	span := float64(t1 - t0)
	if span < 1 {
		span = 1
	}
	for i, t := range starts {
		idx := int(math.Floor(float64(t-t0) / span * sparkBuckets))
		if idx > sparkBuckets-1 {
			idx = sparkBuckets - 1
		}
		vol[idx]++
		if startEr[i] {
			errs[idx]++
		}
	}
	return vol, errs
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(v)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"aggregate cannot be represented"}`))
		return
	}
	w.Write(append(data, '\n'))
}

// Live-log page: GET /metrics/agg/log?before_ms=&limit=&s=&f=…
// Returns older-than-cursor records matching the SAME scope the request
// log uses (status + explorer filters), newest-first. Safety guardrails
// (not tunables): page size is config.DashLogRowsMin..Max, scan at most logScanMax rows per call.
const (
	logScanBatch = 512
	logScanMax   = 4096
)

// HandleLogPage starts at durable newest, then pages below an exact (time,id)
// cursor. The ring is arrival-ordered and cannot supply a safe history cutoff.
func (a *AggAPI) HandleLogPage(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGet(w, r) {
		return
	}
	fs, statusCode, ok := parseScope(r)
	if !ok {
		http.Error(w, `{"error":"invalid scope"}`, http.StatusBadRequest)
		return
	}
	mcz := a.canonizer()
	q := r.URL.Query()
	var cursor *storage.LogCursor
	ms, hasMs := q["before_ms"]
	ids, hasID := q["before_id"]
	if hasMs != hasID || (hasMs && (len(ms) != 1 || len(ids) != 1)) {
		http.Error(w, `{"error":"before_ms and before_id must appear together once"}`, http.StatusBadRequest)
		return
	}
	if hasMs {
		n, err := strconv.ParseInt(ms[0], 10, 64)
		if err != nil {
			http.Error(w, `{"error":"invalid before_ms"}`, http.StatusBadRequest)
			return
		}
		cursor = &storage.LogCursor{StartMs: n, ID: ids[0]}
	}
	v := r.URL.Query().Get("limit")
	if v == "" || len(q["limit"]) != 1 {
		http.Error(w, `{"error":"limit required"}`, http.StatusBadRequest)
		return
	}
	limit, err := strconv.Atoi(v)
	if err != nil || limit < config.DashLogRowsMin || limit > config.DashLogRowsMax {
		http.Error(w, fmt.Sprintf(`{"error":"limit must be %d..%d"}`, config.DashLogRowsMin, config.DashLogRowsMax), http.StatusBadRequest)
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	out := make([]*metrics.Record, 0, limit)
	scanned := 0
	hitEnd := a.store == nil
	if a.store != nil {
		for scanned < logScanMax && len(out) < limit {
			batch := logScanBatch
			if remain := logScanMax - scanned; remain < batch {
				batch = remain
			}
			if batch <= 0 {
				break
			}
			rows, err := a.store.QueryBefore(ctx, cursor, batch)
			if err != nil {
				http.Error(w, `{"error":"log page failed"}`, http.StatusInternalServerError)
				return
			}
			if len(rows) == 0 {
				hitEnd = true
				break
			}
			scanned += len(rows)
			for _, rec := range rows {
				if rec == nil {
					continue
				}
				cursor = &storage.LogCursor{StartMs: rec.Start.UnixMilli(), ID: rec.ID}
				c := fromRecord(rec, mcz)
				if !scopedMatch(&c, statusCode, fs) {
					continue
				}
				out = append(out, rec)
				if len(out) >= limit {
					break
				}
			}
			if len(out) >= limit {
				break
			}
			if len(rows) < batch {
				hitEnd = true
				break
			}
		}
	}
	var cursorMs int64
	var cursorID string
	if cursor != nil {
		cursorMs, cursorID = cursor.StartMs, cursor.ID
	}
	writeJSON(w, map[string]any{
		"records":     metrics.ObserveRecords(out),
		"model_canon": a.ObserveModels(false, out),
		"more":        !hitEnd && a.store != nil,
		"cursor_ms":   cursorMs,
		"cursor_id":   cursorID,
	})
}
