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
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
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
	// explorerGroupBudget bounds the exact per-group accumulators one explorer
	// fold retains. Distinct values beyond it fold into the synthetic "other"
	// bucket, so the exact group table and its ranking work stay bounded by the
	// budget rather than an attacker-controlled distinct-value count. It is
	// much larger than xpNodeCap so ordinary dimensions keep exact top-card
	// ranking. This is an internal guardrail, not a tunable.
	explorerGroupBudget = 8192
	// explorerOtherID reserves a dictionary slot that no real dimension value
	// can occupy, so overflow memberships can still select the synthetic card
	// without colliding with an unset (zero) membership. The wire marks that
	// card with synthetic:true; its display name is not a filter value.
	// Internal only.
	explorerOtherID   = ^uint32(0)
	explorerOtherName = "other"
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
	kpiMemo      kpiMemo
	projection   historyProjection
	modelRules   modelRuleCache
	modelNames   modelNameMemo
	// testHooks is nil in production; package tests use it to sequence the
	// in-flight coalescing cases without timing assumptions.
	testHooks *aggregateTestHooks
	// Optional state providers wired by main.go so HandleBootstrap can carry
	// every first-paint surface in one payload. Nil = section omitted.
	Dash     func() any // effective dashboard config (config.Map)
	Pause    func() any // operator pause state (same shape as GET /admin/pause)
	Throttle func() any // provider limit state (same shape as GET /admin/throttle)
	Debug    func() any // operator debug sessions (same shape as GET /admin/debug)
	Storm    func() any // current automatic error-storm protection and recovery
	// Restart reports whether the graceful self-restart is ELIGIBLE on this
	// binary, so the dashboard can offer the action only when it can work. A
	// strict SUBSET of GET /admin/restart's status document, carrying its two
	// eligibility fields (available, reason) and none of the live phase, pid or
	// drain state: this is the boot-time verdict, immutable by construction.
	Restart func() any
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
	compiled := a.modelRules.get(rules)
	return &modelCanonizer{exec: compiled.exec, memo: map[string]string{}, identity: compiled.identity}
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
// multiple of dash_log_rows - the first log page plus one headroom page before
// the log pages older history from the durable store. Internal derivation, not
// a tunable (same shape as storage's WriteTrackCap). Kept small: every record
// carries its response headers, so an 8x window multiplied the bootstrap and
// reconnect payload for rows the first paint never renders.
const BootRingCapMul = 2

// queryCtx is the single choke point that bounds a dashboard aggregate
// SELECT by StorageQueryTimeout. Validate requires > 0; a 0 fail-closes
// (deadline already expired), matching storage.withQueryTimeout.
func (a *AggAPI) queryCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), a.timeout)
}

// ---------- record materialization (shared by every endpoint) ----------

// contribLabels owns the six single-valued dimension labels while a
// contribution is still live. Durable rows never build this sidecar: the scan
// returns the tuple by value for the one interning step (internLabels), so a
// projected row never pays 96 bytes of garbage for labels the projection
// dictionaries are about to own. Projected rows resolve labels from
// dimensionIDs instead; retaining six string headers on every durable row
// duplicated bytes the dictionaries already own.
type contribLabels struct {
	client, prov, model, key, conv, parentConv string
}

// contrib is one request's contribution to the aggregates: the subset of
// Record fields any dashboard metric reads, derived identically from a stored
// row or from a live ring record.
type contrib struct {
	id     string
	start  int64
	status int
	isErr  bool
	has429 bool
	cost   float64
	in     int64
	out    int64
	cacheR int64
	reason int64
	tools  int64
	ttft   int64
	tps    float64
	dec    int64
	// labels is non-nil only for ring/pending/unprojected rows. Projected
	// rows are interned into the projection dictionaries and then drop it.
	labels *contribLabels
	toolsL []string
	ent    []errEnt
	// Dictionary IDs belong to the projection that materialized this row.
	// Unprojected ring/pending records carry zero IDs and are interned by
	// the same query-dimension adapter at the aggregation boundary.
	dimensionIDs      [dimTool]uint32
	rowIndex          uint32 // one-based projected position; zero is a ring/pending extra
	parentConvID      uint32 // projection conversation dictionary slot of parentConv
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

// liveLabel reads one single-valued label from the live sidecar. Projected
// rows carry no sidecar; their labels live in the projection dictionaries and
// must be resolved from dimensionIDs.
func (c *contrib) liveLabel(dim string) string {
	if c.labels == nil {
		return ""
	}
	switch dim {
	case "client":
		return c.labels.client
	case "provider":
		return c.labels.prov
	case "model":
		return c.labels.model
	case "conversation":
		return c.labels.conv
	case "key":
		return c.labels.key
	}
	return ""
}

// parentLabel reads the declared parent conversation from the live sidecar.
func (c *contrib) parentLabel() string {
	if c.labels == nil {
		return ""
	}
	return c.labels.parentConv
}

// fromRecord maps a live ring record (Attempts already decoded) into a
// contrib. The model field is CANONICALIZED here - the single lowest choke
// point every grouping surface (explorer fold, scope filters, rail counts,
// node-cards) reads - while records, the log leaf, debug session matching,
// and purge/export keep the exact stored spelling.
func fromRecord(r *metrics.Record, mcz *modelCanonizer) contrib {
	c := contrib{
		id:     r.ID,
		start:  r.Start.UnixMilli(),
		status: r.StatusCode,
		isErr:  r.IsError(),
		has429: r.HasRateLimit(),
		cost:   r.Cost,
		in:     r.Usage.InputTokens,
		out:    r.Usage.OutputTokens,
		cacheR: r.Usage.CacheReadTokens,
		reason: r.Usage.ReasoningTokens,
		tools:  int64(r.ToolCalls),
		ttft:   r.TTFTMs,
		tps:    r.DecodeTPS,
		dec:    recordDecodeMs(r),
		labels: &contribLabels{
			client:     r.Client,
			prov:       r.Provider,
			model:      mcz.model(r.Model),
			key:        r.KeyHash,
			conv:       r.ConversationID,
			parentConv: r.ParentConversationID,
		},
		toolsL: r.ToolNames,
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

// recordDecodeMs is the decode window: last_token_at - first_token_at, the
// exact span GenThroughput measures the generation rate over. Both timestamps are truncated
// to milliseconds the same way storage's timeToMilli writes them, so a ring
// record and its stored row always agree. 0 (either timestamp absent, or a
// non-positive window) is an invalid sample - the same >0 gate validMetricSample
// applies to ttft/tps excludes it downstream.
func recordDecodeMs(r *metrics.Record) int64 {
	if r.FirstTokenAt.IsZero() || r.LastTokenAt.IsZero() {
		return 0
	}
	return r.LastTokenAt.UnixMilli() - r.FirstTokenAt.UnixMilli()
}

// rowColumns is the SELECT list the chart and explorer scans stream. It is
// the single owner of which columns dashboard aggregates may read; key order
// must stay in lockstep between rowColumns and scanContrib. The trailing
// decode_ms entry is a DERIVED expression over existing durable columns (the
// operator recipe's decode window), not a new schema column.
const rowColumns = `id, started_at, status_code, error_type, error_code, error_msg, attempts,
	client, provider, model, key_hash, conversation_id, parent_conversation_id,
	input_tokens, output_tokens, cache_read_tokens, reasoning_tokens,
	tool_calls, cost, ttft_ms, gen_tps, tool_names,
	(last_token_at - first_token_at) AS decode_ms`

// scanContrib decodes one scanned row into a contrib (rowColumns order) and
// returns its six single-valued labels separately. The caller interns the row
// into the projection dictionaries immediately, so building a live labels
// sidecar here would allocate one 96-byte header per durable row only for the
// intern step to drop it. The label tuple travels by value instead.
func scanContrib(rows *sql.Rows) (contrib, contribLabels, error) {
	var c contrib
	var labels contribLabels
	var startedAt, status, ttft int64
	var gen float64
	var errType, errCode, errMsg string
	var attemptsJSON, toolNamesJSON []byte
	var model string
	if err := rows.Scan(&c.id, &startedAt, &status, &errType, &errCode, &errMsg, &attemptsJSON,
		&labels.client, &labels.prov, &model, &labels.key, &labels.conv, &labels.parentConv,
		&c.in, &c.out, &c.cacheR, &c.reason,
		&c.tools, &c.cost, &ttft, &gen, &toolNamesJSON, &c.dec); err != nil {
		return c, labels, err
	}
	labels.model = model
	c.start = startedAt
	c.status = int(status)
	c.ttft = ttft
	c.tps = gen
	atts, attErr := storage.DecodeAttemptsColumn(attemptsJSON)
	if attErr != nil {
		log.Printf("aggregate: parse attempts for %s: %v", c.id, attErr)
	}
	toolsL, toolErr := storage.DecodeToolNamesColumn(toolNamesJSON)
	if toolErr != nil {
		log.Printf("aggregate: parse tool names for %s: %v", c.id, toolErr)
	}
	c.toolsL = toolsL
	r := metrics.Record{
		StatusCode: c.status, ErrorType: errType, ErrorCode: errCode, ErrorMsg: errMsg,
		Attempts: atts, Start: time.UnixMilli(c.start),
	}
	c.isErr = r.IsError()
	c.has429 = r.HasRateLimit()
	c.ent = errorEntries(&r)
	return c, labels, nil
}

// errorEntries is the Go mirror of the client's recordErrorEntries enumerator
// (final entry + absorbed-attempt entries, 429 flow control excluded) - the
// single owner of what counts as an error signature event.
func errorEntries(r *metrics.Record) []errEnt {
	var out []errEnt
	fin := r.StatusCode
	// A final 429 is flow control and a plain final 499 is the client's own
	// cancellation - neither produces an error-group entry. A 499 carrying a
	// structured error_type, however, observed the upstream's own failure
	// (an in-band stream error the client aborted around): it produces one
	// exactly like its 200 twin.
	if fin != 429 && !(fin == metrics.StatusClientClosedRequest && r.ErrorType == "") && (r.ErrorType != "" || fin >= 400) {
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
		if a.InBand {
			// An in-band entry records a status the committed 200 hid; the
			// final error entry already owns this failure.
			continue
		}
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

// ringScratch is the recycled destination for a ring copy. The aggregate
// readers copy the whole ring on every fold (the ring is the conservative
// source of "possibly not yet durable"), and at the one-million-record ceiling
// that copy was 8 MB of fresh garbage per read.
type ringScratch []*metrics.Record

// ringScratchPool owns the ring-copy buffers between reads. It is package
// level, not per AggAPI, so a process that rebuilds an API (or a benchmark
// that does) still reuses them; sync.Pool drops them at GC like any other
// pooled buffer, so a shrink or an idle period returns the memory. A Get miss
// allocates exactly the slice Snapshot always allocated: the pool is a reuse
// win, never a correctness dependency.
var ringScratchPool = sync.Pool{New: func() any { return new(ringScratch) }}

// ringIDScratch is the ring-sized id slice used only to ask the durable store
// which records are still unwritten. It is pooled separately from the record
// pointers because the store call consumes strings, not records; a fresh
// []string per read was about 16 MB at the one-million-slot ceiling.
type ringIDScratch []string

var ringIDPool = sync.Pool{New: func() any { return new(ringIDScratch) }}

func acquireRingIDs(size int) *ringIDScratch {
	ids := ringIDPool.Get().(*ringIDScratch)
	if cap(*ids) < size {
		*ids = make([]string, size)
	} else {
		*ids = (*ids)[:size]
	}
	return ids
}

func releaseRingIDs(ids *ringIDScratch) {
	*ids = (*ids)[:0]
	ringIDPool.Put(ids)
}

// acquireRing copies the ring into a pooled scratch, oldest to newest. The
// view aliases the scratch until releaseRing, so it must not outlive the read
// that holds it; both readers (the KPI fold and ringSnapshot) release it with a
// defer at their own scope.
func (a *AggAPI) acquireRing() (*ringScratch, []*metrics.Record) {
	buf := ringScratchPool.Get().(*ringScratch)
	return buf, a.buf.SnapshotInto(*buf)
}

// releaseRing returns a scratch acquired by acquireRing. view must be the
// slice acquireRing returned, not a filter of it: the scratch is reused from
// its capacity, and the original backing array is the owner. Every usable slot
// is cleared first: after Buffer.Reset or RemoveWhere the ring no longer
// references those records, so a pooled pointer would pin each record and its
// strings until the scratch is reused or collected. Clearing one million slots
// costs about 0.2 ms, against a fold that copies the same million pointers.
func releaseRing(buf *ringScratch, view []*metrics.Record) {
	clear(view[:cap(view)])
	*buf = view[:0]
	ringScratchPool.Put(buf)
}

// ringSnapshot captures potentially unflushed records with start >= since into
// a borrowed scratch view, oldest to newest. The caller must hand the scratch
// back with releaseRing(scratch, view) once it has finished reading the view
// (a defer at the caller is the intended shape): the view aliases the pooled
// buffer, and filtering happens inside it, so a steady-state read allocates
// nothing. When a durable store is attached, its written-id probe uses a
// separate pooled scratch instead of a fresh ring-sized []string.
//
// Filter BEFORE the durable snapshot: commits arriving afterward are still
// deduped by its exact ID set, while already-written rows that were deleted
// externally can never be resurrected from their obsolete ring copies.
func (a *AggAPI) ringSnapshot(since int64) (*ringScratch, []*metrics.Record) {
	scratch, view := a.acquireRing()
	out := view[:0]
	for _, r := range view {
		if r == nil {
			continue
		}
		if since > 0 && r.Start.UnixMilli() < since {
			continue
		}
		out = append(out, r)
	}
	if a.store == nil || len(out) == 0 {
		return scratch, out
	}
	ids := acquireRingIDs(len(out))
	defer releaseRingIDs(ids)
	for i, r := range out {
		(*ids)[i] = r.ID
	}
	unwritten := a.store.FilterUnwritten(*ids)
	if len(unwritten) == len(out) {
		return scratch, out
	}
	if len(unwritten) == 0 {
		// Every ring record is already durable. Return the borrowed view with
		// its capacity, never nil: releaseRing hands the scratch back to the
		// pool through this slice, so a nil view would drop the pooled buffer
		// and make the next read allocate a fresh ring-sized one.
		return scratch, out[:0]
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
	return scratch, out[:n]
}

// streamWindow is the shared analytical read boundary. Snapshot the ring
// BEFORE acquiring durable history, then dedupe against that exact projection
// while its read lease is held. A racing commit is counted from one side only.
// prepare borrows the same projection metadata used by the subsequent fold.
func (a *AggAPI) streamWindow(ctx context.Context, since int64, mcz *modelCanonizer, prepare func(*projectionData), fn func(*contrib) error, finish func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	scratch, ring := a.ringSnapshot(since)
	defer releaseRing(scratch, ring)
	return a.withProjection(ctx, func(p *projectionData) error {
		if prepare != nil {
			prepare(p)
		}
		if p != nil {
			if err := p.visitSince(ctx, since, fn); err != nil {
				return err
			}
		}
		for i, r := range ring {
			if err := checkFoldContext(ctx, i); err != nil {
				return err
			}
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
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := finish(); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
}
