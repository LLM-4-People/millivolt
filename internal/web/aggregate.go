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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
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
// projected row never pays 112 bytes of garbage for labels the projection
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
func scanContrib(rows *sql.Rows, mcz *modelCanonizer) (contrib, contribLabels, error) {
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
	labels.model = mcz.model(model)
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
// its capacity, and the original backing array is the owner.
func releaseRing(buf *ringScratch, view []*metrics.Record) {
	*buf = view[:0]
	ringScratchPool.Put(buf)
}

// ringSnapshot captures potentially unflushed records with start >= since into
// a borrowed scratch view, oldest to newest. The caller must hand the scratch
// back with releaseRing(scratch, view) once it has finished reading the view
// (a defer at the caller is the intended shape): the view aliases the pooled
// buffer, and filtering happens inside it, so a read allocates nothing.
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
	ids := make([]string, len(out))
	for i, r := range out {
		ids[i] = r.ID
	}
	unwritten := a.store.FilterUnwritten(ids)
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
	Storm            any           `json:"storm,omitempty"`
	Restart          any           `json:"restart,omitempty"`
	Storage          storageSignal `json:"storage"`
}

type storageSignal struct {
	Enabled bool   `json:"enabled"`
	Dropped uint64 `json:"dropped"`
	// TotalsDegraded reports a failed boot scan: the since-inception KPI
	// totals count only rows committed since that scan. The status footer
	// renders it as the totals-degraded footnote and tooltip.
	TotalsDegraded bool `json:"totals_degraded"`
}

// bootstrapLivePayload is the fields=live view: the ring snapshot and storage
// signal without KPI/operator state. Snapshot and describe need the records.
type bootstrapLivePayload struct {
	metrics.ObservedSnapshot
	Storage storageSignal `json:"storage"`
}

// bootstrapStoragePayload is the fields=storage view: three booleans, no ring
// snapshot or dashboard state.
type bootstrapStoragePayload struct {
	Storage storageSignal `json:"storage"`
}

// bootstrapModelPayload is the fields=model_canon view: the effective rules
// without records or dashboard state.
type bootstrapModelPayload struct {
	ModelCanon any `json:"model_canon"`
}

func (a *AggAPI) storageSignal() storageSignal {
	if a.store == nil {
		return storageSignal{}
	}
	return storageSignal{Enabled: true, Dropped: a.store.Dropped(), TotalsDegraded: a.store.TotalsDegraded()}
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
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	// The two cursor keys this surface consumes go through the strict parse:
	// a malformed pair must never drop a feed pin and leave a stale cursor
	// trusted, and a repeated feed or since is ambiguous.
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "feed", "since", "fields"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	// fields is a closed, additive view used by in-process probes. The default
	// remains the complete dashboard document.
	fields := q.Get("fields")
	switch fields {
	case "":
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, a.bootstrap(a.buf.SnapshotRequest(q, r.Header)))
	case "storage":
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, bootstrapStoragePayload{Storage: a.storageSignal()})
	case "model_canon":
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, bootstrapModelPayload{ModelCanon: a.ObserveModels(true, nil)})
	case "live":
		w.Header().Set("Cache-Control", "no-store")
		snapshot := a.buf.SnapshotRequest(q, r.Header)
		writeJSON(w, bootstrapLivePayload{
			ObservedSnapshot: metrics.ObserveSnapshot(snapshot, a.ObserveModels(true, snapshot.Records, snapshot.InFlightRecords)),
			Storage:          a.storageSignal(),
		})
	default:
		adminjson.WriteError(w, http.StatusBadRequest, "fields must be storage, live, or model_canon")
	}
}

// bootstrap owns the initial HTML and live endpoint payload alike. A new
// document always passes a full snapshot; endpoint resumes use the
// buffer's existing feed/cursor trust boundary. No durable history scan.
func (a *AggAPI) bootstrap(snapshot metrics.Snapshot) bootstrapPayload {
	observed := metrics.ObserveSnapshot(snapshot, a.ObserveModels(true, snapshot.Records, snapshot.InFlightRecords))
	payload := bootstrapPayload{ObservedSnapshot: observed, KPI: a.kpi(), DashboardVersion: dashboardContentVersion()}
	payload.Storage = a.storageSignal()
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
	if a.Storm != nil {
		payload.Storm = a.Storm()
	}
	if a.Restart != nil {
		payload.Restart = a.Restart()
	}
	return payload
}

// ---------- request scoping (chart, explorer, footer, log) ----------

// scopeFilter is one explorer filter (dim:value) - the same dims the client's
// request log is scoped by; the count endpoint matches them server-side over
// ALL history so "scope X of Y" is a since-inception number on both sides.
type scopeFilter struct {
	dim, id string
	// bit is the dimension's bit in the scope grouping mask, derived from the
	// canonical dimensionNames order at parse time. Zero means a directly
	// constructed fixture; scopedMatch computes it then.
	bit uint32
	// ids is the query-bound projection dictionary membership: the exact
	// label slot for client/provider/conversation/key, or every raw model
	// slot whose canonical spelling equals id. A non-nil empty slice means
	// the projection has no matching label (projected rows cannot match);
	// nil leaves the filter unbound for live-only matching.
	ids []uint32
}

// matches reports whether one contribution satisfies the filter; the Go
// mirror of the client's recordMatchesDim (same derivations: statusClass,
// timeBucket, multi-valued tools, error signatures from errorEntries).
func (f scopeFilter) matches(c *contrib) bool {
	switch f.dim {
	case "error":
		for _, e := range c.ent {
			if errorKey(e) == f.id {
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
	case "client", "provider", "model", "conversation", "key":
		if c.rowIndex > 0 {
			dim, _ := singleValuedDim(f.dim)
			id := c.dimensionIDs[dim]
			if len(f.ids) == 1 {
				return id == f.ids[0]
			}
			// Model membership is appended in raw-dictionary order, so the
			// multi-spelling canonical case stays O(log n) per row.
			_, found := slices.BinarySearch(f.ids, id)
			return found
		}
		return c.liveLabel(f.dim) == f.id
	}
	return false
}

func parseScopeFilters(q url.Values) ([]scopeFilter, bool) {
	vals, ok := q["f"]
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
		out = append(out, scopeFilter{dim: dim, id: id, bit: scopeDimBit(dim)})
	}
	return out, true
}

// liveStatusClasses are the named in-flight pills the Requests-card status
// dropdown can send as s= (streaming / paused / throttled / pending). Distinct
// from explorer status CLASSES (2xx/4xx/…) which travel as f=status:2xx. The
// slice is the owner the MCP scope mirror is checked against.
var liveStatusClasses = []string{"paused", "throttled", "streaming", "pending"}

func liveStatusFilter(s string) bool {
	return slices.Contains(liveStatusClasses, s)
}

// parseScope parses a scoping request: explorer filters (f=dim:id, repeated -
// same dims the log is scoped by) plus the Requests-card status dropdown
// (s=200: an exact HTTP code, OR s=streaming/paused/…: a live in-flight
// class). Unknown s= is rejected (deny by default) - never silently ignored.
// q is the handler's one strict-parsed query (adminjson.StrictQuery); the
// repeated f= list key keeps its multi-value grammar.
func parseScope(q url.Values) (fs []scopeFilter, statusCode string, ok bool) {
	fs, ok = parseScopeFilters(q)
	if !ok {
		return nil, "", false
	}
	if s := q.Get("s"); s != "" {
		var valid bool
		statusCode, valid = canonicalStatusSelector(s)
		if !valid {
			return nil, "", false
		}
	}
	return fs, statusCode, true
}

// canonicalStatusSelector is the one status-spelling owner for every web
// surface: a live in-flight class stays verbatim, and an exact numeric code is
// rendered in canonical decimal form. "0200" and "+200" therefore match the
// same rows as "200" in chart, explorer and log instead of being accepted by
// validation and silently matching zero in the byte-for-byte Go matcher.
func canonicalStatusSelector(s string) (string, bool) {
	if liveStatusFilter(s) {
		return s, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 9999 {
		return "", false
	}
	return strconv.Itoa(n), true
}

// scopedMatch reports whether a contribution satisfies the full log scope
// (Requests-card status filter + explorer filters). Filters are AND-ed across
// dimensions and OR-ed within one dimension: repeated f=model:a&f=model:b are
// alternatives, exactly the grammar the MCP scope and the public docs state.
// The first filter of each dimension evaluates the whole group, later members
// are skipped, so each group matches at most once.
func scopedMatch(c *contrib, statusCode string, fs []scopeFilter) bool {
	if statusCode != "" {
		if liveStatusFilter(statusCode) {
			if contribStatusClass(c) != statusCode {
				return false
			}
		} else if !statusCodeEquals(c.status, statusCode) {
			return false
		}
	}
	// One pass with two dimension masks preserves OR-within and AND-across:
	// seen records every dimension that must match, matched records the
	// dimensions already satisfied. After one member of a dimension matches,
	// later members are skipped without rescanning the remaining slice.
	var seen, matched uint32
	for i := range fs {
		bit := fs[i].bit
		if bit == 0 {
			bit = scopeDimBit(fs[i].dim)
		}
		if bit == 0 {
			return false
		}
		seen |= bit
		if matched&bit != 0 {
			continue
		}
		if fs[i].matches(c) {
			matched |= bit
		}
	}
	return matched == seen
}

// statusCodeEquals compares a contribution's numeric status with the validated
// exact-code scope without formatting the number into a new string per row
// (strconv.Itoa allocated once per match attempt on every fold).
func statusCodeEquals(code int, text string) bool {
	if code < 0 {
		return false
	}
	var buf [20]byte
	i := len(buf)
	n := code
	for {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	digits := buf[i:]
	if len(digits) != len(text) {
		return false
	}
	for j := range digits {
		if digits[j] != text[j] {
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
	N                 int64             `json:"n"` // affected requests for error/tool cards, occurrences otherwise
	Cost              float64           `json:"cost"`
	In                int64             `json:"in"`
	Out               int64             `json:"out"`
	Cache             int64             `json:"cache"`
	ErrFinal          int64             `json:"err_final"`
	RateLimitRequests int64             `json:"rate_limit_requests"`
	Tools             int64             `json:"tools"` // ToolCalls summed once per affected request: calls, not requests
	// Blended $/Mtok over this entity's COST-REPORTING in+out tokens
	// (Totals' CostInOut rule, mirrored per entity) - nil when the
	// denominator is 0. An unpriced provider's tokens never dilute it.
	CostPerMTok *float64 `json:"cost_per_mtok"`
	TTFTP50     *float64 `json:"ttft_p50"`
	TTFTP95     *float64 `json:"ttft_p95"`
	TPSP50      *float64 `json:"tps_p50"`
	TPSP95      *float64 `json:"tps_p95"`
	ErrEvents   int64    `json:"err_events"` // error-dimension failure events (final plus absorbed)
	Code        string   `json:"code"`
	LastMs      int64    `json:"last_ms"`
	// Synthetic marks the overflow card. Its Name is a display label, not a
	// filter value; clients must not select it or confuse it with a real value
	// that happens to share the label.
	Synthetic bool `json:"synthetic,omitempty"`
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
	tools             int64
	errFin            int
	rateLimitRequests int
	lastRequest       string // first membership in the current row; no history-ID set
	ttftS             []int64
	tpsS              []float64
	ttftN             int
	tpsN              int
	// cost-REPORTING in+out tokens: the blended-$ denominator (unpriced
	// records add tokens but not cost, so they must not dilute).
	costInOut float64
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

// scopeDimBits derives each filter dimension's grouping bit from the canonical
// dimensionNames order, so the scope matcher and planner cannot drift from the
// dimension vocabulary.
var scopeDimBits = func() map[string]uint32 {
	bits := make(map[string]uint32, len(dimensionNames))
	for i, name := range dimensionNames {
		bits[name] = 1 << i
	}
	return bits
}()

func scopeDimBit(dim string) uint32 { return scopeDimBits[dim] }

// A row may belong to multiple groups, including repeated tool/error
// occurrences. Spans index one flat membership list without per-row slices.
type explorerMemberSpan struct{ start, count uint32 }

// explorerSelectedGroup pairs one returned card's dictionary id with its
// percentile bucket (rank+1). A zero bucket marks an empty open-addressed slot.
type explorerSelectedGroup struct {
	id     uint32
	bucket uint8
}

// explorerDenseSelection bounds the direct-index selected-group table. Common
// small-cardinality dimensions (provider, status, client, key) assign ids below
// it, so their samples keep a single array load per occurrence exactly like the
// old maxGroupID-sized slice. Low selected ids always populate this table, even
// when another selected id is history-scale. This is an internal guardrail, not
// a tunable.
const explorerDenseSelection = 256

// explorerHighSelection is the fixed slot count for the high-id table. It is a
// power of two and far above xpNodeCap (24), so at most xpNodeCap inserts can
// never fill it and linear probing always reaches an empty slot. The bounded
// probe loops below still guard the invariant instead of looping forever if a
// future cap outgrows it. Internal implementation constants, not tunables.
const (
	explorerHighSelection = 128
	explorerHighHash      = 2654435761 // Knuth multiplicative hash
)

// explorerHighSlot is the deterministic initial slot for one high id.
func explorerHighSlot(id uint32) int {
	return int((id * explorerHighHash) & (explorerHighSelection - 1))
}

// explorerSelectedGroups is one payload request's compact selected-group
// lookup, built from the returned top cards only. Dictionary ids are
// dimension-wide positions that can sit far above the card count, so the
// previous dense []uint8 sized by maxGroupID allocated kilobytes to megabytes
// per request to represent at most xpNodeCap entries. Low ids stay in the fixed
// dense table; high ids use one fixed-capacity open-addressed table, so both
// lookups are constant-time probes without allocation or maxGroupID scaling.
// Selections are never carried across requests.
type explorerSelectedGroups struct {
	dense [explorerDenseSelection]uint8
	high  [explorerHighSelection]explorerSelectedGroup
}

// newExplorerSelectedGroups ranks no groups of its own: the payload has already
// capped top at xpNodeCap, and duplicate ids cannot occur because top holds one
// entry per group map key. Every selected id populates exactly one table.
func newExplorerSelectedGroups(top []*entAcc) explorerSelectedGroups {
	var s explorerSelectedGroups
	for i, g := range top {
		bucket := uint8(i + 1)
		if g.id < explorerDenseSelection {
			s.dense[g.id] = bucket
			continue
		}
		slot := explorerHighSlot(g.id)
		for probes := 0; probes < explorerHighSelection; probes++ {
			entry := &s.high[slot]
			if entry.bucket == 0 {
				*entry = explorerSelectedGroup{id: g.id, bucket: bucket}
				break
			}
			if entry.id == g.id {
				entry.bucket = bucket
				break
			}
			slot = (slot + 1) & (explorerHighSelection - 1)
		}
	}
	return s
}

// bucket returns the group's percentile bucket, or zero when the id is not one
// of the returned cards. Low ids are one array load; high ids probe the fixed
// table until their exact id or an empty slot. The loop is bounded, so a missing
// id can never spin even if the capacity invariant were broken.
func (s *explorerSelectedGroups) bucket(id uint32) uint8 {
	if id < explorerDenseSelection {
		return s.dense[id]
	}
	slot := explorerHighSlot(id)
	for probes := 0; probes < explorerHighSelection; probes++ {
		entry := &s.high[slot]
		if entry.bucket == 0 {
			return 0
		}
		if entry.id == id {
			return entry.bucket
		}
		slot = (slot + 1) & (explorerHighSelection - 1)
	}
	return 0
}

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
	// other aggregates every distinct value beyond explorerGroupBudget. It
	// participates in ranking like a card but owns no per-value map: an error
	// row's affected-request count comes from lastRequest, which is exact
	// because one row's occurrences arrive contiguously.
	other         entAcc
	otherAffected int
	overflow      bool
	order         metricOrderSource
	rowCount      int
	// Single-valued dimensions store one group id per projected row (4 bytes).
	// Only tool/error need the flat multi-membership list + spans.
	singleMember []uint32
	multiMember  bool
	memberSpans  []explorerMemberSpan
	members      []uint32
	pendingIDs   map[string]struct{}
	seen         map[string]struct{}
	total        int64
	errTot       int64
	// errAffected is the error dimension's share denominator: the union of
	// affected requests represented by its cards. Every contrib is one request
	// folded exactly once, so counting rows with at least one error entry is
	// the union without a second history-scale ID set. It is deliberately
	// broader than errTot (Record.IsError): an absorbed quality failure is an
	// error card membership even when the final status is 200.
	errAffected int64
	scope       scopePayload
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
		other:  entAcc{id: explorerOtherID, name: explorerOtherName},
		seen:   make(map[string]struct{}),
	}
	// Unprepared folds (direct fixtures) still need a names source for the
	// lineage payload; prepare borrows the same query dictionary owner.
	e.lineage.names = &e.dims
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
	e.lineage.prepare(p, &e.dims)
	if p != nil {
		e.order = p.metrics.allOrderSource()
		// The unfiltered rail is exactly the shared dictionary's occupied
		// memberships. Only unprojected ring/pending names can add anything;
		// scoped rails continue to follow the canonical per-row predicate.
		e.projectedRail = e.statusCode == "" && len(e.allFS) == 0
		if e.projectedRail {
			for dim, dictionary := range e.dims.base {
				if dim == dimModel {
					// Canonical model IDs share the projection's raw membership
					// counts; the canonical dictionary intentionally carries no
					// counts of its own (they change on every append).
					continue
				}
				for id, n := range dictionary.counts {
					if id > 0 && n > 0 {
						e.markRail(dim, uint32(id))
					}
				}
			}
			rawCounts := p.dimensions.dict[dimModel].counts
			for rawID, n := range rawCounts {
				if n > 0 && rawID < len(e.dims.modelIDs) {
					e.markRail(dimModel, e.dims.modelIDs[rawID])
				}
			}
		}
		e.rowCount = len(p.rows)
		e.multiMember = e.dimIndex == dimTool || e.dimIndex == dimError
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
	// Only ring-only rows can be pending here: prepare already marked every
	// pending ID the projection contains, so hashing the ID for projected rows
	// is a redundant map probe per row.
	if c.rowIndex == 0 {
		if _, pending := e.pendingIDs[c.id]; pending {
			e.seen[c.id] = struct{}{}
		}
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
	ids := e.dims.singles(c)
	parent := c.parentConvID
	if c.rowIndex == 0 {
		if name := c.parentLabel(); name != "" {
			parent = e.dims.intern(dimConversation, name)
		}
	}
	e.lineage.observeRow(c, ids, parent, inRail, inGroup && e.dimIndex == dimConversation)
	if inGroup {
		e.total++
		if c.isErr {
			e.errTot++
		}
		// The error gallery denominator is the union of the requests its cards
		// represent. Each contribution is one request, so this counts every
		// affected request exactly once regardless of how many failure events
		// or error signatures it carries.
		if e.dimIndex == dimError && len(c.ent) > 0 {
			e.errAffected++
		}
	}
	if !inRail && !inGroup {
		return nil
	}
	needsRail := inRail && !(e.projectedRail && c.rowIndex > 0)
	for dim, id := range ids {
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
			if _, err := e.foldGroup(id, c); err != nil {
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
				if _, err := e.foldGroup(id, c); err != nil {
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
				g, err := e.foldGroup(id, c)
				if err != nil {
					return err
				}
				if ent.at > g.lastMs {
					g.lastMs = ent.at
				}
				// Mixed overflow errors have no single representative code;
				// leave it empty instead of naming one arbitrary failure.
				if g != &e.other && g.code == "" {
					g.code = ent.code
				}
			}
		}
	}
	return nil
}

func (e *explorerFold) foldGroup(dimKey uint32, c *contrib) (*entAcc, error) {
	g := e.groups[dimKey]
	if g == nil {
		if len(e.groups) >= explorerGroupBudget {
			// Stop allocating exact groups at the budget. The synthetic card
			// aggregates every later distinct value; repeated occurrences of
			// an already tracked value still reach it through the map above.
			e.overflow = true
			g = &e.other
		} else {
			g = &entAcc{id: dimKey, name: e.dims.name(e.dimIndex, dimKey)}
			e.groups[dimKey] = g
		}
	}
	// Health counts are affected requests. Error cards keep occurrence
	// multiplicity for ErrEvents, samples, cost and tokens, while their N is
	// the distinct affected-request count. Tool cards fold one contribution
	// per request per tool name (a nonstream response repeats a name once per
	// call), so their N, share and request-level accumulators are
	// request-based while the calls KPI still sums ToolCalls. Error groups need
	// the affected-ID set for their headline; the overflow bucket only needs
	// the last ID because one row's visits are contiguous. Tool duplicates are
	// contiguous too, so the same last-ID guard dedupes them.
	var firstRequest bool
	if e.dimIndex == dimError && g != &e.other {
		if g.affected == nil {
			g.affected = make(map[string]struct{})
		}
		_, duplicate := g.affected[c.id]
		firstRequest = !duplicate
		if firstRequest {
			g.affected[c.id] = struct{}{}
		}
	} else {
		firstRequest = g.n == 0 || g.lastRequest != c.id
		g.lastRequest = c.id
		if e.dimIndex == dimError && firstRequest {
			e.otherAffected++
		}
	}
	if e.dimIndex == dimTool && !firstRequest {
		return g, nil
	}
	g.n++
	// Same accumulator contract as the chart fold: every contribution is
	// admitted, every step's overflow is checked, nothing re-validates the
	// accumulator itself.
	if err := metrics.AddValue(&g.cost, c.cost); err != nil {
		return nil, err
	}
	if c.cost > 0 {
		if err := metrics.AddValue(&g.costInOut, float64(c.in)); err != nil {
			return nil, err
		}
		if err := metrics.AddValue(&g.costInOut, float64(c.out)); err != nil {
			return nil, err
		}
	}
	if err := metrics.AddCount(&g.in, c.in); err != nil {
		return nil, err
	}
	if err := metrics.AddCount(&g.out, c.out); err != nil {
		return nil, err
	}
	if err := metrics.AddCount(&g.cacheR, c.cacheR); err != nil {
		return nil, err
	}
	if err := metrics.AddCount(&g.tools, c.tools); err != nil {
		return nil, err
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
		if !e.multiMember {
			// One group per row for every single-valued dimension (client,
			// provider, model, key, status, time, conversation). Previously
			// every explorer request allocated a span and a membership slot
			// per projected row even for these.
			if e.singleMember == nil {
				e.singleMember = make([]uint32, e.rowCount)
			}
			e.singleMember[c.rowIndex-1] = g.id
		} else {
			if e.memberSpans == nil {
				e.memberSpans = make([]explorerMemberSpan, e.rowCount)
			}
			span := &e.memberSpans[c.rowIndex-1]
			if span.count == 0 {
				span.start = uint32(len(e.members))
			}
			span.count++
			e.members = append(e.members, g.id)
		}
	}
	// Durable samples stay in immutable metric order. Only raw ring/pending
	// extras need per-group values; error counts keep occurrence multiplicity
	// while every other dimension contributes one sample per affected request.
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
	return g, nil
}

// groupCount is a card's headline count: affected requests for error and tool
// cards, occurrences for every other dimension. Error keeps event occurrences
// separately in ErrEvents. Ranking and the wire N field read the same owner.
func (e *explorerFold) groupCount(g *entAcc) int {
	if e.dimIndex == dimError {
		if g == &e.other {
			return e.otherAffected
		}
		return len(g.affected)
	}
	return g.n
}

// rankedGroups selects the returned top cards before sample derivation:
// discarded groups never allocate response arrays or perform sample selection.
// Counts follow groupCount; names break ties deterministically, independently
// of dictionary insertion or map order. The overflow bucket ranks by its
// aggregate count but allocates nothing per excluded value.
func (e *explorerFold) rankedGroups() []*entAcc {
	capacity := len(e.groups)
	if e.overflow {
		capacity++
	}
	top := make([]*entAcc, 0, min(capacity, xpNodeCap))
	insert := func(g *entAcc) {
		n := e.groupCount(g)
		at := sort.Search(len(top), func(i int) bool {
			other := e.groupCount(top[i])
			return n > other || n == other && (g.name < top[i].name ||
				g.name == top[i].name && g.id < top[i].id)
		})
		if at == xpNodeCap {
			return
		}
		if len(top) < xpNodeCap {
			top = append(top, nil)
		}
		copy(top[at+1:], top[at:])
		top[at] = g
	}
	for _, g := range e.groups {
		insert(g)
	}
	if e.overflow {
		insert(&e.other)
	}
	return top
}

// selectedVisitor maps projection rows to compact selected-group buckets for
// the metric rank visitors. Multi-valued dimensions walk the row's occurrence
// span and emit one bucket per selected occurrence in source order, preserving
// duplicate tool/error multiplicity; single-valued dimensions keep one group id
// per row. Unselected ids emit nothing.
func (e *explorerFold) selectedVisitor(selected *explorerSelectedGroups) func(row uint32, emit func(uint8)) {
	if e.multiMember {
		return func(row uint32, emit func(uint8)) {
			if uint64(row) >= uint64(len(e.memberSpans)) {
				return
			}
			span := e.memberSpans[row]
			for _, id := range e.members[span.start : span.start+span.count] {
				if bucket := selected.bucket(id); bucket != 0 {
					emit(bucket)
				}
			}
		}
	}
	return func(row uint32, emit func(uint8)) {
		if uint64(row) < uint64(len(e.singleMember)) {
			if bucket := selected.bucket(e.singleMember[row]); bucket != 0 {
				emit(bucket)
			}
		}
	}
}

// errorTotal is the explorer's error_total field. The error breakdown uses the
// union of affected requests represented by its cards, the same unit as card N
// and share; every other dimension keeps the canonical Record.IsError count.
// Keeping the units identical is what guarantees an error card's share is at
// most 100% without a client-side clamp hiding a denominator mismatch.
func (e *explorerFold) errorTotal() int64 {
	if e.dimIndex == dimError {
		return e.errAffected
	}
	return e.errTot
}

func (e *explorerFold) payload() explorerPayload {
	// Every lineage read below happens before the pooled resolution workspace
	// is returned; a later payload build on this view re-resolves.
	defer e.lineage.releaseScratch()
	conversations := e.lineage.summary()
	top := e.rankedGroups()
	// Resolve compact selected-group membership once, then both metrics read
	// the same row spans. Emitting every occurrence preserves duplicate tools
	// and error events while error-card N remains affected-request based.
	selected := newExplorerSelectedGroups(top)
	ttftCounts, tpsCounts := make([]int, len(top)), make([]int, len(top))
	var extraTTFT []int64
	var extraTPS []float64
	var ttftBuckets, tpsBuckets []uint8
	for i, g := range top {
		bucket := uint8(i + 1)
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
	visit := e.selectedVisitor(&selected)
	wait, _ := rankPercentilesVisitMerge(e.order.base.ttft, e.order.pending.ttft, ttftCounts, extraTTFT, ttftBuckets, visit)
	speed, _ := rankPercentilesVisitMerge(e.order.base.tps, e.order.pending.tps, tpsCounts, extraTPS, tpsBuckets, visit)
	ents := make([]jsonEnt, 0, len(top))
	for i, g := range top {
		synthetic := g == &e.other
		ent := jsonEnt{
			Name: g.name, N: int64(e.groupCount(g)), Cost: g.cost, In: g.in, Out: g.out,
			Cache: g.cacheR, ErrFinal: int64(g.errFin),
			RateLimitRequests: int64(g.rateLimitRequests),
			Tools:             g.tools, Code: g.code, LastMs: g.lastMs,
			Synthetic: synthetic,
		}
		if e.dimIndex == dimConversation {
			// Resolve lineage for the selected cards only. Materializing every
			// in-scope conversation group allocated a map proportional to the
			// distinct-value count just to look up at most xpNodeCap names.
			if !synthetic {
				ent.Conversation = e.lineage.info(e.lineage.group(g.name))
			}
		}
		ent.CostPerMTok = metrics.ScaledRatio(g.cost, g.costInOut, 1e6)
		if e.dimIndex == dimError {
			ent.ErrEvents = int64(g.n) // fold counted failure EVENTS per group
		}
		ent.TTFTP50, ent.TTFTP95, ent.TPSP50, ent.TPSP95 =
			wait[i][0], wait[i][1], speed[i][0], speed[i][1]
		ents = append(ents, ent)
	}
	rail := make(map[string]int, dimCount)
	for dim, name := range dimensionNames {
		rail[name] = e.railCount[dim]
	}
	return explorerPayload{Dim: e.dim, Total: e.total, ErrorTot: e.errorTotal(), Rail: rail, Groups: ents, Scope: e.scope, Conversations: conversations}
}

// HandleAggExplorer serves the per-dimension breakdown over ALL history. The
// optional f=dim:id filters (everything EXCEPT the active dimension's own
// value - the cross-filter rule: the gallery shows X's distribution over
// records filtered by all OTHER dimensions) scope both the breakdown and its
// share denominator; the rail distinct-value counts and footer scope totals
// follow the FULL filter set. Every count is
// since-inception - computed server-side over the durable history.
func (a *AggAPI) HandleAggExplorer(w http.ResponseWriter, r *http.Request) {
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "dim", "s"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	dim := q.Get("dim")
	if !validDims[dim] {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid dim")
		return
	}
	allFS, statusCode, ok := parseScope(q)
	if !ok {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid filter")
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	payload, err := a.explorer(ctx, dim, allFS, statusCode)
	if err != nil {
		log.Printf("agg explorer: %v", err)
		adminjson.WriteError(w, http.StatusInternalServerError, "aggregate failed")
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

// jsonBufferPool reuses the encoder's scratch buffer: chart bundles and the
// bootstrap payload run to hundreds of kilobytes, and a fresh right-sized
// slice per poll is pure garbage. Oversized buffers are dropped instead of
// pinned in the pool.
var jsonBufferPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

const jsonBufferRetain = 1 << 20

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Every aggregate carries live request metadata; never let a browser or
	// intermediary retain a stale copy of request previews or filter results.
	w.Header().Set("Cache-Control", "no-store")
	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	enc := json.NewEncoder(buf)
	// application/json is never parsed as HTML by a browser: escaping <, > and
	// & only inflates URLs, query strings and previews with \u003c sequences.
	enc.SetEscapeHTML(false)
	err := enc.Encode(v)
	if err != nil {
		buf.Reset()
		jsonBufferPool.Put(buf)
		log.Printf("agg encode: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write(adminjson.ErrorBody("aggregate cannot be represented"))
		return
	}
	data := buf.Bytes()
	if _, err := w.Write(data); err != nil {
		// The response is already streaming; nothing to repair.
		_ = err
	}
	if buf.Cap() > jsonBufferRetain {
		return
	}
	buf.Reset()
	jsonBufferPool.Put(buf)
}

// Live-log page: GET /metrics/agg/log?before_ms=&limit=&s=&f=…
// Returns older-than-cursor records matching the SAME scope the request
// log uses (status + explorer filters), newest-first. Safety guardrails
// (not tunables): page size is config.DashLogRowsMin..Max, scan at most logScanMax rows per call.
const (
	logScanBatch = 512
	logScanMax   = 4096
)

// logPagePlan is the single classification owner for the live-log scope:
// exact stored predicates SQL enforces before decode, plus the residual
// Go-owned scope the authoritative matcher must still evaluate. allPushed
// reports that every active predicate is represented exactly, so each
// candidate is expected to be an output row.
type logPagePlan struct {
	filter    storage.LogFilter
	goFilters []scopeFilter
	goStatus  string
	allPushed bool
}

// planLogPage derives one request's pushdown and residual scope. Exact stored
// dimensions become SQL predicates; everything the projection derives (status
// classes, errors, tools, time buckets, canonical models) stays in the
// authoritative Go matcher and is matched against the narrow candidate read.
func planLogPage(fs []scopeFilter, statusCode string) logPagePlan {
	plan := logPagePlan{allPushed: true}
	// Group by dimension first: OR within one dimension's values, AND across
	// groups. A group that cannot be represented exactly in SQL (canonical
	// models, status classes, tools, errors, time buckets) stays wholly
	// Go-owned; pushing a subset as AND would narrow the candidate set below
	// the group's union.
	var done uint32
	for i := range fs {
		bit := fs[i].bit
		if bit == 0 {
			bit = scopeDimBit(fs[i].dim)
		}
		if done&bit != 0 {
			continue
		}
		done |= bit
		group := make([]scopeFilter, 0, 1)
		for j := i; j < len(fs); j++ {
			if fs[j].dim == fs[i].dim {
				group = append(group, fs[j])
			}
		}
		var field storage.LogField
		pushable := true
		switch fs[i].dim {
		case "client":
			field = storage.LogClient
		case "provider":
			field = storage.LogProvider
		case "key":
			field = storage.LogKeyHash
		case "conversation":
			field = storage.LogConversation
		case "model":
			// The canonical matcher trims the stored spelling even with no
			// rules, so raw SQL equality would silently drop a stored
			// " gpt-4 " that canonically equals "gpt-4". Keep every model
			// filter Go-owned; a growing scan is the correct cost.
			pushable = false
		default:
			pushable = false
		}
		if !pushable {
			plan.goFilters = append(plan.goFilters, group...)
			plan.allPushed = false
			continue
		}
		if len(group) == 1 {
			plan.filter.All = append(plan.filter.All, storage.LogPredicate{Field: field, Value: group[0].id})
			continue
		}
		// One dimension's repeated values stay one alternative group. Separate
		// dimensions must remain separate groups so the SQL planner intersects
		// them instead of flattening every value into one broad OR.
		predicates := make([]storage.LogPredicate, 0, len(group))
		for _, f := range group {
			predicates = append(predicates, storage.LogPredicate{Field: field, Value: f.id})
		}
		plan.filter.Any = append(plan.filter.Any, predicates)
	}
	if statusCode != "" {
		if canonical, ok := canonicalStatusSelector(statusCode); ok && !liveStatusFilter(canonical) {
			plan.filter.All = append(plan.filter.All, storage.LogPredicate{Field: storage.LogStatus, Value: canonical})
		} else {
			plan.goStatus = statusCode
			plan.allPushed = false
		}
	}
	return plan
}

// logPageQuery preserves the original classification contract for callers and
// tests that only need the SQL filter and the all-pushed verdict.
func logPageQuery(fs []scopeFilter, statusCode string) (filter storage.LogFilter, allPushed bool) {
	plan := planLogPage(fs, statusCode)
	return plan.filter, plan.allPushed
}

// logCandidateMatch evaluates the residual Go-owned scope against the narrow
// candidate projection. Exact predicates already enforced by the candidate
// query are deliberately absent from the plan, so every field this matcher
// reads comes from the scanned row itself.
func logCandidateMatch(c *storage.LogCandidate, goStatus string, fs []scopeFilter, mcz *modelCanonizer) bool {
	r := metrics.Record{
		StatusCode: c.Status, ErrorType: c.ErrorType, ErrorCode: c.ErrorCode,
		ErrorMsg: c.ErrorMsg, Attempts: c.Attempts,
		Start: time.UnixMilli(c.Cursor.StartMs),
	}
	cc := contrib{
		id: c.Cursor.ID, start: c.Cursor.StartMs, status: c.Status,
		labels: &contribLabels{model: mcz.model(c.Model)},
		toolsL: c.ToolNames,
		ent:    errorEntries(&r),
	}
	return scopedMatch(&cc, goStatus, fs)
}

// HandleLogPage starts at durable newest, then pages below an exact (time,id)
// cursor. The ring is arrival-ordered and cannot supply a safe history cutoff.
func (a *AggAPI) HandleLogPage(w http.ResponseWriter, r *http.Request) {
	if !metrics.RejectUnlessGet(w, r) {
		return
	}
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	// s is this surface's only single-occurrence scope key; the repeated f=
	// list keeps its grammar, and the before_ms/before_id/limit duplicates
	// stay behind their existing pairing gates below.
	if err := adminjson.DuplicateQueryKey(q, "s"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	fs, statusCode, ok := parseScope(q)
	if !ok {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid scope")
		return
	}
	mcz := a.canonizer()
	var cursor *storage.LogCursor
	ms, hasMs := q["before_ms"]
	ids, hasID := q["before_id"]
	if hasMs != hasID || (hasMs && (len(ms) != 1 || len(ids) != 1)) {
		adminjson.WriteError(w, http.StatusBadRequest, "before_ms and before_id must appear together once")
		return
	}
	if hasMs {
		n, err := strconv.ParseInt(ms[0], 10, 64)
		if err != nil {
			adminjson.WriteError(w, http.StatusBadRequest, "invalid before_ms")
			return
		}
		cursor = &storage.LogCursor{StartMs: n, ID: ids[0]}
	}
	v := q.Get("limit")
	if v == "" || len(q["limit"]) != 1 {
		adminjson.WriteError(w, http.StatusBadRequest, "limit required")
		return
	}
	limit, err := strconv.Atoi(v)
	if err != nil || limit < config.DashLogRowsMin || limit > config.DashLogRowsMax {
		adminjson.WriteError(w, http.StatusBadRequest, fmt.Sprintf("limit must be %d..%d", config.DashLogRowsMin, config.DashLogRowsMax))
		return
	}
	ctx, cancel := a.queryCtx(r)
	defer cancel()

	// A live-only status class can never match durable history (those classes
	// describe the in-memory pending set). Return the empty page without
	// decoding up to logScanMax rows that the Go matcher would reject.
	if liveStatusFilter(statusCode) {
		writeJSON(w, map[string]any{
			"records":     []metrics.ObservedRecord{},
			"model_canon": a.ObserveModels(false, nil),
			"more":        false,
			"cursor_ms":   int64(0),
			"cursor_id":   "",
		})
		return
	}
	// Push exact, SQL-safe dimensions into the page query. Model equality,
	// status classes, errors, tools and time buckets stay in the authoritative
	// Go matcher, which reads the narrow candidate projection instead of full
	// records.
	plan := planLogPage(fs, statusCode)
	accepted := make([]storage.LogCursor, 0, limit)
	scanned := 0
	hitEnd := a.store == nil
	batch := min(limit, logScanBatch)
	if a.store != nil {
		for scanned < logScanMax && len(accepted) < limit {
			if plan.allPushed {
				// Every scanned row is an expected output row, so ask for
				// exactly what the page still needs.
				batch = limit - len(accepted)
			}
			if remain := logScanMax - scanned; remain < batch {
				batch = remain
			}
			if batch <= 0 {
				break
			}
			candidates, err := a.store.QueryBeforeCandidates(ctx, cursor, &plan.filter, batch)
			if err != nil {
				log.Printf("agg log page: %v", err)
				adminjson.WriteError(w, http.StatusInternalServerError, "log page failed")
				return
			}
			if len(candidates) == 0 {
				hitEnd = true
				break
			}
			scanned += len(candidates)
			for i := range candidates {
				cand := &candidates[i]
				cursor = &storage.LogCursor{StartMs: cand.Cursor.StartMs, ID: cand.Cursor.ID}
				if !plan.allPushed && !logCandidateMatch(cand, plan.goStatus, plan.goFilters, mcz) {
					continue
				}
				accepted = append(accepted, cand.Cursor)
				if len(accepted) >= limit {
					break
				}
			}
			if len(accepted) >= limit {
				break
			}
			if len(candidates) < batch {
				hitEnd = true
				break
			}
			if !plan.allPushed {
				// Go-only predicates may reject most rows: grow from the page
				// size towards the per-call batch/scan caps instead of decoding
				// a full batch on the first request.
				batch = min(batch*2, logScanBatch)
			}
		}
	}
	// The full response rows are materialized once, for the accepted page
	// only: a rejected candidate scan never pays for prompts, previews,
	// headers, or request parameters. The storage call is bounded by the page
	// size and preserves the scan order.
	out := make([]*metrics.Record, 0, limit)
	if len(accepted) > 0 {
		out, err = a.store.LogPageRecords(ctx, accepted)
		if err != nil {
			log.Printf("agg log page: %v", err)
			adminjson.WriteError(w, http.StatusInternalServerError, "log page failed")
			return
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
