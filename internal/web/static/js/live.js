// ---------- server aggregation state ----------
// Every non-table metric surface (KPI band, traffic chart, explorer cards,
// footer totals) renders from server-computed aggregates over all requests
// since inception (/metrics/agg*, /metrics/bootstrap) - the client never
// derives numbers from the ring (the ring now feeds only the request log).
// kpiAgg is refreshed on a short cadence (the bootstrap tick); chartAgg per
// window + drift-refresh; explorerAgg per breakdown dimension +
// stale-refresh. Counters.in_flight stays event-live.
// The loaded document pins its embedded assets. This is a content identity,
// not a process ID: unchanged frontend files never need a page refresh.
const DASHBOARD_VERSION_RE = /^[a-f0-9]{16}$/;
const loadedDashboardVersion = document.querySelector('meta[name="dashboard-version"]')?.content || '';
let dashboardReloading = false;
let kpiAgg = null;
let chartAgg = null;
let explorerAgg = null;
let _pendingRevision = -1; // server lifecycle revision, scoped to the accepted feed

function kpiNow() {
  return kpiAgg || { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, answer_tokens: 0, avg_ttft_ms: null, avg_tps: null };
}
function kpiInFlight() {
  // 0 is a live value - do not fall through to a stale /metrics/agg gauge.
  if (lastData && lastData.counters && lastData.counters.in_flight != null) {
    return lastData.counters.in_flight;
  }
  return kpiNow().in_flight || 0;
}
function kpiTotal() {
  const k = kpiNow();
  return (k.requests || 0) + kpiInFlight();
}
// explorerFilters is the memoized explorer filter set for hot-path matching
// (derive/scopeQS): the hash + status pair is the key, so per-event
// code does one path parse instead of re-deriving the full explorer state per
// record. explorerState itself stays the canonical derivation.
let _filCache = null;
function explorerFilters() {
  const hash = location.hash;
  if (_filCache && _filCache.hash === hash && _filCache.status === filters.status) {
    return _filCache.filters;
  }
  const fs = explorerState().filters;
  _filCache = { hash, status: filters.status, filters: fs };
  return fs;
}
function scopeQS() {
  // the full log scope the chart/footer share: exact-status filter + explorer
  // filter set (order-independent), encoded once - single owner.
  const parts = [];
  if (filters.status) parts.push('s=' + encodeURIComponent(filters.status));
  for (const f of explorerFilters()) {
    parts.push('f=' + encodeURIComponent(f.dim + ':' + f.id));
  }
  return parts.join('&');
}
const chartRequest = requestGate(true);
function fetchChart(mode = 'reuse') {
  const win = chartView.window;
  const scope = scopeQS();
  const q = 'window=' + win + (scope ? '&' + scope : '');
  chartRequest(q, (accept, signal) => {
    _lastChartFetch = Date.now();
    fetch('/metrics/agg/chart?' + q, {signal})
      .then(r => r.ok ? r.json() : Promise.reject())
      .then(p => accept(p, true)).catch(() => accept(null, false));
  }, p => {
    chartAgg = p;
    renderChart();
  }, mode);
}
const explorerRequest = requestGate(true);

// explorerReq is the breakdown request key: active dim + the same s=/f=
// scopeQS the chart/footer use. The server strips the active dim for the
// cross-filter gallery but applies the full set to rail counts.
function explorerReq() {
  const dim = 'dim=' + encodeURIComponent(explorerState().activeDim);
  const rest = scopeQS();
  return rest ? dim + '&' + rest : dim;
}
function fetchExplorer(mode = 'reuse') {
  const q = explorerReq();
  const scope = scopeQS();
  // The breakdown shows the active dim's distribution over records matching
  // all other dims' filters - computed server-side over all history since
  // inception (the filter set is a scoping signal, never a client record set).
  explorerRequest(q, (accept, signal) => {
    fetch('/metrics/agg/explorer?' + q, {signal})
      .then(r => r.ok ? r.json() : Promise.reject())
      .then(p => accept(p, true)).catch(() => accept(null, false));
  }, p => {
    // Hash state can change before its navigation event dispatches.
    if (q !== explorerReq()) { fetchExplorer(); return; }
    explorerAgg = p;
    explorerAgg.fetchedAt = Date.now();
    explorerAgg.scopeKey = scope;
    renderExplorer();
    updateScopeLine();
  }, mode);
}
// refreshExplorerIfStale re-derives the breakdown from the server when the
// cached payload aged out; filter/dim changes refetch immediately (onNavigate).
function refreshExplorerIfStale() {
  if (!explorerAgg) fetchExplorer();
  else if (Date.now() - explorerAgg.fetchedAt > dashCfg.explorer_stale_ms) fetchExplorer('refresh');
}
function explorerWantsLiveStatus() {
  if (LIVE_STATUS_FILTERS[filters.status]) return true;
  const st = explorerState();
  if (st.activeDim === 'status') return true;
  return st.filters.some(f => f.dim === 'status' && LIVE_STATUS_FILTERS[f.id]);
}
let _xpLiveTimer = null;
function scheduleExplorerLiveRefresh() {
  if (!explorerWantsLiveStatus()) return;
  if (_xpLiveTimer) clearTimeout(_xpLiveTimer);
  _xpLiveTimer = setTimeout(() => {
    _xpLiveTimer = null;
    fetchExplorer('refresh');
  }, 250);
}

// ---------- main render ----------
let lastData = null;
// O(1) record lookup: maps each id to its index in lastData.records.
// Rebuilt on full snapshot replace; maintained in-place on incremental
// upserts and evictions. Makes the hot-path findIndex → O(1) Map.get.
let _recIdx = new Map();
function _rebuildRecIdx() {
  _recIdx.clear();
  const recs = lastData?.records;
  if (!recs) return;
  for (let i = 0; i < recs.length; i++) { if (recs[i]?.id) _recIdx.set(recs[i].id, i); }
  pruneModelNames();
}
// Incremental-feed cursor state: the resume position for
// /metrics/bootstrap?since= and EventSource Last-Event-ID replay. feedId
// pins the sequence space to one proxy process - a restart or instance
// swap invalidates the cursor and forces a full resync.
let feedId = null;
let lastSeq = 0;

// derive computes the canonical derived state shared by full renders, live
// event updates, and the log's windowing. Record-level scoping (the
// explorer filter set + status filter) applies only to the request log;
// every numeric surface is the server's since-inception aggregates.
// One fused O(N) pass over lastData.records per state revision, memoized:
// every mutation bumps lastData._rev (upsertRec/upsertPendingRec/counters
// writes) and every scope change swaps the filter-set identity, so a cached
// derivation can never go stale. All render paths share this one derivation.
let _der = null, _derData = null, _derRev = -1, _derFS = null;
function derive(data) {
  const fs = explorerFilters(); // memoized on (hash, status) - identity is the scope key
  const rev = data._rev || 0;
  if (_der && _derData === data && _derFS === fs && _derRev === rev) return _der;

  const recs = data.records || [];
  const ctr = data.counters || {};
  const liveBuckets = {};
  for (const name of Object.keys(LIVE_STATUS_FILTERS)) liveBuckets[name] = 0;
  const liveSeen = new Set();
  const codeSeen = new Set();
  const scopedRecs = [];
  for (const r of recs) {
    if (!r) continue;
    if (r.live) {
      const k = statusClass(r);
      if (k in liveBuckets) liveBuckets[k]++;
      liveSeen.add(k);
    }
    if (Number(r.status_code) > 0) codeSeen.add(r.status_code);
    if (recordMatchesStatusFilter(r, filters.status) && fs.every(f => recordMatchesDim(r, f.dim, f.id))) {
      scopedRecs.push(r);
    }
  }

  // Global KPI band (scope-independent), server-side since inception; the
  // in-flight tile stays event-live via the counters stream.
  const a = kpiNow();
  const inFlight = ctr.in_flight || 0;
  const flightBits = [];
  for (const name of Object.keys(LIVE_STATUS_FILTERS)) {
    if (liveBuckets[name]) flightBits.push(liveBuckets[name] + ' ' + name);
  }
  const flightSub = flightBits.length ? flightBits.join(' · ') : 'active streams';
  const totalCost = a.cost || 0;
  const cacheTotal = a.cache_read_tokens || 0;
  const inTotal = a.input_tokens || 0;
  const outTotal = a.output_tokens || 0;
  const cacheRatio = inTotal ? pct(cacheTotal, inTotal) : '0%';
  const errRate = a.requests ? pct(a.errors || 0, a.requests) : '0%';
  const reasonTotal = a.reasoning_tokens || 0;
  const costPerReq = Number.isFinite(a.cost_per_req) ? a.cost_per_req : null;
  const costPerMtok = Number.isFinite(a.cost_per_mtok) ? a.cost_per_mtok : null;

  const kpiHtml = `
    <div class="kpi"><h2>Requests</h2><div class="val">${vt(fmt(a.requests))}</div><div class="sub2">all tracked</div></div>
    <div class="kpi ${inFlight ? 'k-err' : ''}"><h2>In-flight</h2><div class="val">${vt(fmt(inFlight))}</div><div class="sub2">${flightSub}</div></div>
    <div class="kpi k-cost"><h2>Total cost</h2><div class="val">${vt(fmtMoney(totalCost), 'est USD · ' + fmtMoney(totalCost))}</div><div class="sub2">${costPerReq != null ? vt('~' + fmtMoney(costPerReq) + '/req') : '-'}</div></div>
    <div class="kpi k-cost"><h2>Blended cost</h2><div class="val">${costPerMtok != null ? vt(fmtMoney(costPerMtok)) + '<span class="unit">/Mtok</span>' : '-'}</div><div class="sub2">${costPerMtok != null ? vt('priced in+out', 'blended over cost-reporting requests only') : '-'}</div></div>
    <div class="kpi"><h2>Tokens in/out</h2><div class="val val-duo">${vt(fmt(inTotal), fmtFull(inTotal))}<span class="unit">/</span>${vt(fmt(outTotal), fmtFull(outTotal))}</div><div class="sub2">${reasonTotal > 0 ? vt('reasoning ' + fmt(reasonTotal) + ' tok', 'reasoning ' + fmtFull(reasonTotal) + ' tok') : 'no reasoning'}</div></div>
    <div class="kpi k-cache"><h2>Cache hit</h2><div class="val">${vt(cacheRatio)}</div><div class="sub2">${vt(fmt(cacheTotal) + ' / ' + fmt(inTotal) + ' in', fmtFull(cacheTotal) + ' / ' + fmtFull(inTotal) + ' in')}</div></div>
    <div class="kpi"><h2>Avg TTFT</h2><div class="val">${a.avg_ttft_ms != null ? vt(fmtDur(a.avg_ttft_ms)) : '-'}</div><div class="sub2">time to first token</div></div>
    <div class="kpi"><h2>Avg TPS</h2><div class="val">${a.avg_tps != null ? a.avg_tps.toFixed(1) + '<span class="unit">tok/s</span>' : '-'}</div><div class="sub2">throughput</div></div>
    <div class="kpi ${a.errors ? 'k-err' : ''}"><h2>Error rate</h2><div class="val">${vt(errRate)}</div><div class="sub2">${vt(fmt(a.errors || 0) + ' of ' + fmt(a.requests))}</div></div>
  `;

  // Status filter options. Live pills (streaming / paused) are always listed
  // - they are in-flight states, not HTTP codes, so they never appear in the
  // status_code set. Numeric codes come from finalized rows. The active
  // filter must always be a rendered option and the control's value, even
  // when no current record carries that status - otherwise a persisted
  // filter (dash.filters) that matches nothing right now would silently
  // filter the data while the dropdown misleadingly shows "all status".
  const liveOpts = [];
  for (const name of Object.keys(LIVE_STATUS_FILTERS)) {
    if (name === 'streaming' || name === 'paused' || liveSeen.has(name) || filters.status === name) {
      liveOpts.push(name);
    }
  }
  const codes = [...codeSeen].sort((x, y) => x - y);
  const statuses = liveOpts.concat(codes);
  if (filters.status && !statuses.some(s => String(s) === String(filters.status))) statuses.push(filters.status);
  const statusHtml = `<option value="">all status</option>` + statuses.map(s => `<option value="${s}" ${String(s)===filters.status?'selected':''}>${s}</option>`).join('');

  _der = { recs, scopedRecs, kpiHtml, statusHtml };
  _derData = data; _derRev = rev; _derFS = fs;
  return _der;
}

// bumpData invalidates the cached derivation after a mutation of
// lastData's records or counters. One owner: every mutator calls this.
function bumpData() { if (lastData) lastData._rev = (lastData._rev || 0) + 1; }

// Snapshots and lifecycle events share the same flat counter document. Only
// real changes invalidate derive; an empty SSE replay after the HTML seed is
// metadata, not another full-dashboard render. The server's existing pending
// revision also orders the gauge across independent SSE/HTTP deliveries. Only
// the gauge is revision-gated: an older event may still contain a missing row.
// Return the normalized document so full replacements use this same owner.
function setCounters(counters, pendingRevision) {
  if (!counters) return counters;
  const prev = lastData?.counters || {};
  if (!Number.isSafeInteger(pendingRevision) || pendingRevision < 0 || pendingRevision < _pendingRevision) {
    counters = {...counters, in_flight: prev.in_flight};
  } else {
    _pendingRevision = pendingRevision;
  }
  if (!lastData) return counters;
  const keys = Object.keys(counters);
  if (keys.length === Object.keys(prev).length && keys.every(k => counters[k] === prev[k])) return counters;
  lastData.counters = counters;
  bumpData();
  return counters;
}

// renderBand re-renders the KPI band alone (server-agg fetches + live
// in-flight counter merges). It reads the same lastData the full derive does.
function renderBand() {
  if (!lastData) return;
  updateSection('kpis', derive(lastData).kpiHtml);
}

// The explorer's full-scope fold also owns the footer count. Never perform
// another history scan or substitute the bounded ring for all-history truth.
function updateScopeLine() {
  if (!lastData) return;
  const total = kpiTotal();
  const scope = scopeQS();
  const matches = !scope ? total : (explorerAgg?.scopeKey === scope ? explorerAgg.scope?.matches : null);
  const setF = (id, v) => { const el = $(id); if (el) el.textContent = v; };
  setF('f-scope', (matches == null ? '…' : fmt(matches)) + ' of ' + fmt(total));
}

// renderLive re-renders the derived surfaces on a live event without touching
// the request log wholesale (rows update incrementally so the list never
// shifts under the cursor and text selection survives) and without re-rendering
// an open drawer or the explorer gallery on every event (both throttled to a
// human cadence instead). One derivation feeds every surface.
// renderLive is coalesced behind requestAnimationFrame: rapid bursts of SSE
// events (begin/update/end/record in one frame) run one pipeline pass instead
// of N, cutting per-frame DOM work to a constant. rAF naturally pauses in
// hidden tabs; on visibilitychange a flushed flag ensures the latest state
// paints immediately when the user returns.
let _renderQueued = false;
let _renderDirty = false;
function scheduleRenderLive() {
  _renderDirty = true;
  if (_renderQueued) return;
  _renderQueued = true;
  requestAnimationFrame(() => { _renderQueued = false; if (_renderDirty) { _renderDirty = false; renderLive(); } });
}
function renderLive() {
  if (!lastData) return;
  const d = derive(lastData);
  updateSection('kpis', d.kpiHtml);
  updateSection('f-status', d.statusHtml);
  if ($('f-status').value !== filters.status) $('f-status').value = filters.status;
  renderRequestsIncremental(d.scopedRecs);
  updateLogStats(d);
  scheduleExplorerRender();
  refreshFooterState();
  updateScopeLine();
}

function renderAll(data) {
  lastData = data;
  // Reset scroll against the old layout, before any DOM writes. Setting it
  // after inserting the table forces a complete synchronous table layout.
  resetLogWindow();
  const d = derive(data);
  updateSection('kpis', d.kpiHtml);

  // Chart + explorer render from server aggregates; the request log is the
  // only surface that renders the (explorer-scoped) ring records.
  renderChart();
  updateSection('f-status', d.statusHtml);
  if ($('f-status').value !== filters.status) $('f-status').value = filters.status;
  renderRequests(d.scopedRecs);
  updateLogStats(d);
  renderExplorer();

  // Keep the open drawer in sync as new records stream in.
  if (drawerId) renderDrawer();

  // Status-line footer: live state + scoped count + scoped error count/rate
  // (server-computed; in-flight stays in the KPI band only - each stat lives
  // in exactly one place).
  refreshFooterState();
  updateScopeLine();
}

// ---------- live updates via SSE ----------
// applySnapshotPayload consumes a live-feed snapshot payload (the
// /metrics/bootstrap response or the SSE "snapshot" event). Incremental
// payloads (server-side cursor resume) merge new records into the existing
// state; full payloads replace it. The feed_id guard invalidates the cursor
// across proxy restarts and deletions: a delta cannot communicate removals.
function applySnapshotPayload(data, bootstrapApplied = false) {
  const feedChanged = data.feed_id && feedId !== data.feed_id;
  if (data.feed_id === feedId && !data.incremental &&
      (data.seq < lastSeq || data.pending_revision < _pendingRevision)) {
    // An SSE reconnect snapshot can lag a newer HTTP delta/lifecycle event.
    // Re-capture behind the same full barrier before replacing either set.
    fetchBootstrap('full');
    return false;
  }
  if (!bootstrapApplied && !applyModelCanon(data.model_canon, modelRevision === null)) return false;
  if (feedChanged) _pendingRevision = -1;
  if (data.feed_id) {
    if (feedId && feedId !== data.feed_id) {
      // An SSE snapshot can win against an older in-flight bootstrap too.
      // Supersede it before issuing this epoch's state refresh.
      ++_bootReq;
      feedId = data.feed_id;
      lastSeq = 0;
      lastRender = {}; // force every section to rebuild against the new feed
      // A restart or deletion invalidated the prior replay epoch.
      // The snapshot below rebuilds the log/drawer from the new feed, and
      // refreshAfterRestart re-pulls every other server-fed surface (aggregates
      // - including the chart, which must not ride the dead process's
      // timeline - pause holds, provider limits, effective config - which
      // re-arms this page's own cadences) so the whole page reflects the new
      // process. A bootstrap already applied its state and checked the asset
      // version; only an SSE snapshot needs that additional state pull.
      refreshAfterRestart(bootstrapApplied);
    } else if (!feedId) {
      feedId = data.feed_id;
    }
  }
  lastSeq = Math.max(lastSeq, data.seq || 0);
  // In-flight view: the server includes current streaming requests
  // (in_flight_records) in every payload - a page that loaded mid-request
  // missed the ephemeral "begin" event, and the row must still appear (the
  // row renderer already draws records without status_code as streaming).
  // A pending copy never clobbers a finalized row with the same id (a
  // snapshot can race its own "end" publish - see upsertPendingRec).
  const pending = data.in_flight_records;
  const revision = lastData?._rev || 0;
  const counters = setCounters(data.counters, data.pending_revision);
  if (!feedChanged && data.incremental && lastData && Array.isArray(lastData.records)) {
    for (const r of (data.records || [])) {
      if (r) r.live = false;
      upsertRec(r);
    }
    if (Array.isArray(pending)) {
      for (const r of pending) upsertPendingRec(r, data.pending_revision);
    }
    lastData.seq = data.seq;
    lastData.oldest_seq = data.oldest_seq;
    if ((lastData._rev || 0) !== revision) scheduleRenderLive();
  } else {
    // Full replacement: fold the in-flight rows into the record set the
    // snapshot renders from. Finalized IDs win even for a replay from an
    // older process that captured final/pending in separate transitions.
    data.counters = counters;
    if (!Array.isArray(data.records)) data.records = [];
    for (const r of data.records) { if (r) r.live = false; }
    if (Array.isArray(pending) && pending.length) {
      const finalIds = new Set(data.records.map(r => r.id));
      for (const r of pending) {
        if (r && r.id && !finalIds.has(r.id)) {
          r.live = true; r._pendingRevision = data.pending_revision; data.records.push(r);
        }
      }
    }
    renderAll(data);
    _rebuildRecIdx();
    // A same-feed stale-cursor resync (ring eviction) does not refetch the
    // chart: server buckets still describe this process; the configured
    // chart cadence owns freshness. A feed change
    // handled the dead process's chart in the branch above.
  }
  // EventSource reconnects to its immutable original URL. Rebind it once to
  // the accepted epoch/cursor, or every later reconnect would replay full.
  // Closing alone does not protect queued callbacks; startStream also pins
  // every handler to the source instance and its replay epoch.
  if (feedChanged && source) startStream(lastSeq, feedId);
  return true;
}

// One request is exactly one row at every lifecycle stage: append-once,
// upsert-by-id afterward - shared by the ring broadcast ("record"), the
// lifecycle events ("begin"/"end"), and incremental snapshot merges, so they
// can never produce duplicates.
// _evict removes the oldest record from the ring when the cap is exceeded,
// keeping the Map index in sync without a full rebuild.
const RING_SLACK = 64; // internal constant - batches eviction to amortize O(N) reindex
function _evict(recs) {
  if (recs.length <= dashCfg.history_size + RING_SLACK) return;
  const cut = recs.length - dashCfg.history_size;
  recs.splice(0, cut);
  // Rebuild the index once per batch rather than decrementing every entry.
  _rebuildRecIdx();
}

// upsertRec folds one finalized record into lastData.records. Pending events
// use upsertPendingRec, whose monotonic gate cannot undo a finalized row.
// Returns true when the record genuinely arrived (inserted, or a pending row
// updated to final - a new durably-counted arrival) and false when the id was
// already a finalized row (a replayed/duplicate delivery). Callers that fold
// records into live aggregates must gate on the result - replay-tolerant
// delivery is by design, and a naive per-event fold double-counts replays.
// A pure-replay of a finalized row is a no-op: no bumpData, no render.
function upsertRec(rec) {
  const recs = lastData?.records || [];
  if (!rec || !rec.id) return false; // nothing arrived - never a foldable arrival
  let i = _recIdx.get(rec.id);
  if (i !== undefined && recs[i]?.id === rec.id) {
    const pendingBefore = !!recs[i].live;
    recs[i] = rec;
    _recIdx.set(rec.id, i);
    if (recs.length > dashCfg.history_size) { _evict(recs); }
    // A replay of an already-finalized row: same content, no visual change -
    // skip bumpData to avoid re-running the O(N) derive pass.
    if (!pendingBefore) return false;
    bumpData();
    return true; // pending → finalized: a real transition
  }
  // New record: append + index.
  recs.push(rec);
  _recIdx.set(rec.id, recs.length - 1);
  if (recs.length > dashCfg.history_size) { _evict(recs); }
  bumpData();
  return true;
}

// upsertPendingRec merges a server-provided in-flight (pending) record - the
// flip side of upsertRec for snapshot deliveries. A pending copy is stale by
// construction when a finalized record with the same id already exists (the
// snapshot was computed before that request's "end" publish): keep the
// finalized row instead of regressing it to a never-ending streaming row.
function upsertPendingRec(rec, pendingRevision) {
  const recs = lastData?.records || [];
  if (!rec || !rec.id) return recs;
  if (!Number.isSafeInteger(pendingRevision) || pendingRevision < 0) return recs;
  let i = _recIdx.get(rec.id);
  if (i !== undefined && recs[i]?.id === rec.id) {
    // The same request's watermark orders every pending field, including
    // pause/throttle changes that do not append retry attempts. Keep it on
    // the existing row, not another index or a global whole-event gate.
    if (!recs[i].live || pendingRevision < recs[i]._pendingRevision) return recs;
    rec.live = true; rec._pendingRevision = pendingRevision;
    recs[i] = rec; bumpData();
    return recs;
  }
  rec.live = true; rec._pendingRevision = pendingRevision;
  recs.push(rec);
  _recIdx.set(rec.id, recs.length - 1);
  if (recs.length > dashCfg.history_size) { _evict(recs); }
  bumpData();
  return recs;
}

// Explorer rebuilds are debounced during live-event bursts so the gallery
// cards don't jitter on every single request; navigation/full renders still
// rebuild it immediately.
let _explorerTimer = null;
function scheduleExplorerRender() {
  if (_explorerTimer) return;
  _explorerTimer = setTimeout(() => { _explorerTimer = null; renderExplorer(); }, 600);
}

// startStream opens the SSE feed. since/feed seed the initial snapshot's
// cursor (a page that just bootstrapped replays only the delta); a
// reconnect ignores them - the browser echoes Last-Event-ID, and a feed
// mismatch degrades to a full (capped) snapshot.
function startStream(since, feed) {
  if (source) source.close();
  const parts = [];
  if (feed) parts.push('feed=' + encodeURIComponent(feed));
  if (since > 0) parts.push('since=' + String(since));
  const stream = source = new EventSource('/metrics/live/stream' + (parts.length ? '?' + parts.join('&') : ''));
  const current = () => source === stream && (!feed || feed === feedId);
  stream.onopen = () => { if (current()) setFooterLive(true); };
  stream.onerror = () => { if (current()) setFooterLive(false); };
  stream.addEventListener('snapshot', (e) => {
    if (source !== stream) return;
    try { applySnapshotPayload(JSON.parse(e.data)); } catch(err) {}
  });
  stream.addEventListener('reset', (e) => {
    if (!current()) return;
    try {
      const next = JSON.parse(e.data).feed_id;
      if (typeof next !== 'string' || !next || next === feedId) return;
      // The full-bootstrap gate owns close, invalidation and cursor-safe reopen.
      fetchBootstrap('full');
    } catch(err) {}
  });
  // A finalized record from the ring. The event's Last-Event-ID (the id:
  // field the stream carries) is the resume cursor - on reconnect the browser
  // sends it back and the server replays only the newer records.
  stream.addEventListener('record', (e) => {
    if (!current()) return;
    try {
      const rec = JSON.parse(e.data);
      if (!applyModelCanon(rec?.model_canon)) return;
      delete rec.model_canon; // keep envelope metadata only in the shared dictionary
      rec.live = false;
      const seq = parseInt(e.lastEventId, 10);
      if (!isNaN(seq)) lastSeq = Math.max(lastSeq, seq);
      upsertRec(rec);
      scheduleRenderLive();
    } catch(err) {}
  });
  // Per-request lifecycle: a request appears the moment it starts ("begin") and
  // is finalized when it completes ("end"), so the log shows it streaming live
  // instead of only after it finishes. Upsert by id - begin/end share the
  // record's stable id; "end" carries the same authoritative record the ring
  // also persists, so we replace any pending row with the finalized one. Each
  // lifecycle event also carries the process-wide in-flight gauge
  // ("in_flight"): merge it into the counters so the global KPI band reacts
  // the moment a stream starts/ends - the bootstrap counters (snapshot)
  // otherwise only refresh on the tick cadence.
  const upsertLive = (e) => {
    if (!current()) return;
    try {
      const ev = JSON.parse(e.data);
      if (!ev.record || !ev.record.id || !applyModelCanon(ev.model_canon)) return;
      ev.record.live = e.type !== 'end';
      // The gauge merge is a real mutator: keep the fresh-copy pattern (other
      // code may hold the old counters object) and bump the derivation only
      // when in_flight actually changed. The bump matters because the ring
      // "record" event usually finalizes the row first - that end event is
      // then a replay that bumps nothing in upsertRec, so without this the
      // gauge correction would memo-hit and the In-flight tile would keep the
      // pre-end value. An unchanged gauge (an update re-carrying its begin's
      // value) must not re-run the O(N) derive pass.
      const counters = { ...(lastData?.counters || {}) };
      if (typeof ev.in_flight === 'number') counters.in_flight = ev.in_flight;
      setCounters(counters, ev.pending_revision);
      if (e.type === 'end') {
        upsertRec(ev.record);
      } else {
        upsertPendingRec(ev.record, ev.pending_revision);
      }
      scheduleRenderLive();
      scheduleExplorerLiveRefresh();
    } catch(err) {}
  };
  stream.addEventListener('begin', upsertLive);
  stream.addEventListener('end', upsertLive);
  // Mid-flight progress (an absorbed retry attempt): the record is still live
  // and not final, but we upsert it so the in-flight row reflects the retry as
  // it happens. Shares the record's stable id - upsert, never a duplicate.
  stream.addEventListener('update', upsertLive);
  // No poll timer: the 5s tick is the fallback - fetchBootstrap resumes via
  // the ?since=&feed= cursor, so a tick with SSE down still folds the records
  // missed while the stream was silent (and one timer, never stacked).
}

// Footer clock is managed by _armClock() below (visibility-aware: paused in
// hidden tabs, resumed on return). Resize is rAF-gated to avoid per-frame
// layout thrash.
window.addEventListener('resize', () => {
  if (_resizeRaf) return;
  _resizeRaf = requestAnimationFrame(() => { _resizeRaf = 0; if (lastData) renderChart(); });
});
let _resizeRaf = 0;

// Visibility handling: stop the 5s tick + 1s clock while the tab is hidden
// (SSE keeps state current; Chrome throttles background timers but doesn't
// stop them, wasting CPU + server round-trips). On return: re-arm + flush
// any state that changed while hidden, and scheduleRenderLive's rAF flag
// ensures the pending pipeline runs immediately.
let _clockTimer = null;
let _wasHidden = false;
function _armClock() {
  if (_clockTimer) return;
  _clockTimer = setInterval(() => {
    const fc = $('f-clock');
    if (fc) {
      const now = new Date();
      fc.textContent = now.toLocaleDateString(undefined, { weekday: 'long' }) + ' ' + now.toLocaleTimeString();
    }
    if (pauseActive() || (pauseState.holds || []).length || debugActive() || (debugState.sessions || []).length) refreshFooterState();
  }, 1000);
}
_armClock();
document.addEventListener('visibilitychange', () => {
  if (document.hidden) {
    _wasHidden = true;
    clearInterval(_clockTimer); _clockTimer = null;
    if (_dashTickTimer) { clearInterval(_dashTickTimer); _dashTickTimer = null; }
  } else if (_wasHidden) {
    _wasHidden = false;
    _armClock();
    armDashboardTicks();
    // Flush any state that changed while hidden (SSE may have delivered
    // new records while the aggregate payloads aged).
    fetchBootstrap('resume');
    scheduleRenderLive();
  }
});

// Filters: any change to a filter control re-scopes the request log (the only
// client-side surface the explorer/status filters still drive). Chart / footer
// / explorer refetch server aggregates with the same s=/f= set.
$('f-status').addEventListener('change', doFilter);

// Traffic chart controls: the preset dropdown (curated series combos -
// populated from CHART_PRESETS, the single owner), window + percentile
// selects. All restore their persisted values; the legend toggles series
// visibility (delegated on the container so updateSection's innerHTML swaps
// never drop the listener); the view persists in dash.chart.
fillChartControls();
$('chart-preset').addEventListener('change', e => setChartPreset(e.target.value));
$('chart-window').addEventListener('change', e => setChartWindow(e.target.value));
$('chart-pct').addEventListener('change', e => setChartPct(e.target.value));
$('traffic-legend').addEventListener('click', e => {
  const b = e.target.closest('.leg-item');
  if (b) toggleChartSeries(b.dataset.series);
});

// Server-state refresh cadence from dashCfg (the bootstrap payload's dash
// section; GET /admin/config re-pulls it after a restart): the tick polls
// /metrics/bootstrap at dash_poll_interval - KPI + operator state + the
// record delta since the cursor (which makes the tick the SSE fallback too:
// records missed while the stream was silent fold on the same resume rule);
// the chart refreshes server truth every dash_chart_refresh; the explorer
// breakdown refreshes when stale.
let _dashTickTimer = null;
let _lastChartFetch = 0;
function armDashboardTicks() {
  if (_dashTickTimer) clearInterval(_dashTickTimer);
  _dashTickTimer = setInterval(() => {
    fetchBootstrap('resume');
    const now = Date.now();
    if (now - _lastChartFetch >= dashCfg.chart_ms) {
      fetchChart('refresh');
    }
    refreshExplorerIfStale();
  }, dashCfg.poll_ms);
}
armDashboardTicks();

// Explorer: the URL hash drives the log's scope and the breakdown dimension.
// onNavigate fires on hash change and once immediately (deep-link restore).
// Dim/filter changes re-derive the breakdown server-side (cross-filter).
onNavigate(() => {
  if (lastData) renderAll(lastData);
  fetchExplorer();
  fetchChart(); // chart is SCOPED to the log's filter set (all-history server-side)
});

// ---------- boot: server-seeded state, one application gate ----------
// fetchBootstrap is the one server-state pull, shared by boot, the 5s tick,
// purge resyncs, and restart resyncs. GET /metrics/bootstrap carries
// everything the page renders before the slower chart/explorer scans land:
// the ring snapshot (full and capped on a cursor-less call, a delta on a
// cursor resume - never the full ring twice), the since-inception KPI
// aggregate, the effective dashboard config, and the operator pause/limit
// state. Initial HTML embeds this exact document; boot consumes it once
// through the same gate without another network request.
//   mode 'full'   forced full snapshot, no cursor (boot, purge re-sync)
//   mode 'resume' cursor resume - a tick delta; a foreign feed degrades
//                 server-side to a full snapshot, never a silent miss
//   mode 'none'   state surfaces only - records came from the SSE snapshot
// Overlapping requests are superseded by issue order (the newest request's
// response wins): a stale in-flight delta computed before a purge's full
// resync must never re-apply pre-purge rows after the fresh snapshot landed.
let _bootReq = 0;
let _bootFull = null; // mandatory replacement; ordinary polls join instead of superseding it
function refreshDashboardVersion(version) {
  if (dashboardReloading) return true;
  if (typeof version !== 'string' || !DASHBOARD_VERSION_RE.test(version) ||
      !DASHBOARD_VERSION_RE.test(loadedDashboardVersion) || version === loadedDashboardVersion) return false;
  dashboardReloading = true;
  // Normal reload revalidates the existing no-cache/ETag assets. No cache
  // busting URL or second asset watcher; persisted filters and chart survive.
  location.reload();
  return true;
}

// Consume the server's inert JSON block once. It is the same bootstrap
// document, not a second state/render path. Removing it releases the serialized
// copy; later full resyncs must read fresh state. Bad/missing seeds fall back
// to the existing endpoint, never leave the dashboard blank.
function takeEmbeddedBootstrap() {
  const node = $('dashboard-bootstrap');
  if (!node) return null;
  const raw = node.textContent;
  node.remove();
  try {
    const p = JSON.parse(raw);
    if (!p || typeof p !== 'object' || !Array.isArray(p.records) ||
        !p.counters || typeof p.counters !== 'object' || !p.kpi || typeof p.kpi !== 'object' ||
        typeof p.feed_id !== 'string' || !p.feed_id || !Number.isSafeInteger(p.seq) || p.seq < 0 ||
        !Number.isSafeInteger(p.pending_revision) || p.pending_revision < 0 ||
        p.incremental !== false || !DASHBOARD_VERSION_RE.test(p.dashboard_version)) return null;
    return p;
  } catch (_) { return null; }
}

function fetchBootstrap(mode, after) {
  if (dashboardReloading) return;
  if (mode !== 'full' && _bootFull) {
    return _bootFull.then(p => { if (after && !dashboardReloading) after(p); return p; });
  }
  const my = ++_bootReq;
  if (mode === 'full') {
    // A full response replaces rows. Stop live delivery before its snapshot
    // is captured, or an older response could erase a newer row while keeping
    // its advanced cursor. Source identity rejects callbacks already queued.
    if (source) source.close();
    source = null;
  }
  const operatorRevisions = captureOperatorRevisions();
  const q = [];
  if (mode === 'resume' && feedId && lastSeq > 0) {
    q.push('since=' + String(lastSeq), 'feed=' + encodeURIComponent(feedId));
  }
  const embedded = mode === 'full' ? takeEmbeddedBootstrap() : null;
  const response = embedded ? Promise.resolve(embedded)
    : fetch('/metrics/bootstrap' + (q.length ? '?' + q.join('&') : '')).then(r => r.ok ? r.json() : Promise.reject());
  const request = response.then(p => {
      if (dashboardReloading) return;
      if (my === _bootReq) {
        if (refreshDashboardVersion(p.dashboard_version)) return;
        // A stale/zero resume cursor can unexpectedly produce a full payload.
        // Re-capture it behind the same barrier; never replace rows under SSE.
        if (mode === 'resume' && !p.incremental && source) return fetchBootstrap('full');
        const previous = lastData;
        const revision = previous?._rev || 0;
        // Changed rules need a full set of names/rows; mixed revisions from a
        // reload during serialization must be re-captured before any adoption.
        if (!validModelCanon(p.model_canon, true)) throw new Error('invalid observer metadata');
        if ((p.debug?.model_canon && p.debug.model_canon.revision !== p.model_canon.revision) ||
            (modelRevision !== null && p.model_canon.revision !== modelRevision && mode !== 'full') ||
            (p.feed_id === feedId && !p.incremental && (p.seq < lastSeq || p.pending_revision < _pendingRevision))) {
          return fetchBootstrap('full');
        }
        applyBootstrapState(p, operatorRevisions);
        const stateChanged = previous && (previous._rev || 0) !== revision;
        if (mode !== 'none' && !applySnapshotPayload(p, true)) return _bootFull;
        // Config/model changes precede the snapshot's own revision gate and
        // must not depend on optional operator snapshots to refresh the log.
        // A full replacement already rendered; only in-place state needs this.
        if (stateChanged && lastData === previous) scheduleRenderLive();
        if (mode === 'full') startStream(lastSeq, feedId);
      }
      return p;
    })
    .catch(() => {
      // Failed boot/resync falls back to the stream's full snapshot. Only the
      // latest mandatory replacement may reopen; an older failure stays inert.
      if (mode === 'full' && _bootFull === request && !dashboardReloading) startStream(0, feedId);
      return null;
    })
    .finally(() => { if (_bootFull === request) _bootFull = null; })
    .then(p => { if (after && !dashboardReloading) after(p); return p; });
  if (mode === 'full') _bootFull = request;
  return request;
}
function applyBootstrapState(p, revisions = captureOperatorRevisions()) {
  applyDashValues(p.dash);
  applyModelCanon(p.model_canon, true);
  if (p.kpi) {
    kpiAgg = p.kpi;
    // kpiHtml lives inside the memoized derivation - a kpiAgg swap alone
    // (mode 'none': the SSE snapshot already delivered the records) must
    // invalidate it or renderBand would repaint the old aggregate.
    bumpData();
  }
  if (p.pause) applyPauseState(p.pause, revisions.pause);
  if (p.throttle) applyThrottleState(p.throttle, revisions.throttle);
  if (p.debug) applyDebugState(p.debug, revisions.debug);
  applyStormState(p.storm);
  if (p.storage && typeof p.storage.enabled === 'boolean' && Number.isSafeInteger(p.storage.dropped) && p.storage.dropped >= 0) {
    storageState = p.storage;
    refreshFooterState();
  }
  if (lastData) { renderBand(); updateScopeLine(); }
}
function bootDashboard() {
  fetchBootstrap('full');
}

watchGallery(); // self-correcting gallery sizing from the very first paint
bootDashboard();
