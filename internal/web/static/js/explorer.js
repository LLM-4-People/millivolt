// ---------- explorer: hash router (navigation state = URL) ----------
// Single source of truth for the faceted cross-filter explorer. The URL hash
// encodes an order-independent filter set plus a trailing by/<dim> breakdown
// (view state, not a filter): deep-linkable, bookmarkable, back/forward via
// hashchange. This module owns parsing/building/dispatch - render consumes
// explorerState() via onNavigate(). No DOM access beyond location/history.

const CONVERSATION_PARENT_DIMS = ['client', 'key', 'conversation'];

// EXPLORER_DIMS is the field map for hash parsing / record matching.
// Labels come from ENTITY_TYPES via entityType().label (single visual registry).
// Rail order is XP_RAIL_DIMS. `field` is the metrics.Record field used to
// match records of that dimension (error is derived, so it has no field).
const EXPLORER_DIMS = {
  client:       { field: 'client' },
  provider:     { field: 'provider' },
  model:        { field: 'model' },
  conversation: { field: 'conversation_id' },
  key:          { field: 'key_hash' }, // sha256 of the upstream key - displayed truncated, never the raw key
  status:       { field: null },   // derived: statusClass() → 2xx/4xx/5xx/err/cancel + live paused/streaming/throttled/pending
  time:         { field: null },   // derived: timeBucket() buckets start → four local-time windows (weekend/night/work/evening)
  tool:         { field: null },   // derived: multi-valued over tool_names[]
  request:      { field: 'id' },
  error:        { field: null },
  // 'by' is the breakdown-dimension marker (view state, not a filter): a
  // trailing `by/<dim>` hash segment sets which dimension the gallery groups by.
  by:           { field: null },
};

// parseHash reads location.hash → ordered [{dim, id}, …]. Segments alternate
// dim/id. Deny by default: stop at the first unknown dim or missing id, and
// never throw - a malformed hash yields [] (or the valid prefix).
function parseHash() {
  const raw = (location.hash || '').replace(/^#\/?/, '');
  if (!raw) return [];
  const parts = raw.split('/');
  const segs = [];
  for (let i = 0; i < parts.length; i += 2) {
    let dim, id;
    try {
      dim = decodeURIComponent(parts[i] || '');
      id  = decodeURIComponent(parts[i + 1] || '');
    } catch (e) { break; } // malformed escape - keep the valid prefix, stop
    if (!EXPLORER_DIMS[dim] || !id) break;
    segs.push({ dim, id });
  }
  return segs;
}

// buildHash is the exact inverse of parseHash: [{dim,id},…] → '#/dim/id/…'.
// Each part is encoded individually (never URLSearchParams - it mangles '+').
function buildHash(segments) {
  return '#/' + (segments || [])
    .filter(s => s && EXPLORER_DIMS[s.dim] && s.id != null && s.id !== '')
    .map(s => encodeURIComponent(s.dim) + '/' + encodeURIComponent(s.id))
    .join('/');
}

// hashFor(dim, id) - one-entity deep link, e.g. hashFor('provider', 'hyper').
function hashFor(dim, id) {
  return buildHash([{ dim, id }]);
}

// Router dispatch state: the registered render callback plus the last-rendered
// hash, so identical hashes never trigger a double render.
let _navCallback = null;
let _lastNavHash = null;
function _dispatchNav() {
  const h = location.hash || '';
  if (h === _lastNavHash) return; // same target - skip double-render
  _lastNavHash = h;
  if (_navCallback) _navCallback(parseHash());
}

// navigateTo(segments) - the only write path. Assigning location.hash creates
// a history entry and fires hashchange → _dispatchNav. (Assigning the current
// hash fires no hashchange, so repeat navigation to the same target is a no-op.)
function navigateTo(segments) {
  location.hash = buildHash(segments);
}

// onNavigate(cb) - register the render-layer callback. Fires on every real
// hash change (user edit, back/forward, navigateTo) and once immediately on
// registration (initial load / bookmark restore).
function onNavigate(cb) {
  _navCallback = cb;
  window.addEventListener('hashchange', _dispatchNav);
  _dispatchNav(); // initial state (bookmark / deep link restore)
}

// ---------- unified explorer (view engine) ----------
// The URL hash path carries the explorer state: a sequence of {dim, id} filter
// segments plus the trailing by/<dim> breakdown marker. The gallery/cards
// render the server's since-inception breakdown (/metrics/agg/explorer). The
// hash scopes the request log through the cached derive (live.js). All navigation goes
// through navigateTo(). Zones: scope bar (chips), dimension rail, breakdown
// gallery (node cards).

// recordHasError reports whether a record's final error or any absorbed attempt
// matches the given error key (type|code|msg).
function recordHasError(r, key) {
  // Shares the single error-identity derivation (recordErrorEntries) with the
  // gallery grouping, so filtering and counting never disagree.
  for (const e of recordErrorEntries(r)) {
    if (errorKey(e.type, e.code, e.msg) === key) return true;
  }
  return false;
}

// shortId shortens an entity id for chip/gallery display: conversation ids
// keep their short form; errors get their type; keys show only an obfuscated
// prefix (the raw key is never stored - id is a sha256 hex, so a short prefix
// is both safe and enough to tell keys apart); time buckets map to labels.
function shortId(dim, id) {
  if (id == null || id === '') return '';
  if (dim === 'conversation') return id.startsWith('s:') ? id.slice(2) : id;
  if (dim === 'error') return id.split('|')[0] || 'error';
  if (dim === 'key') return '…' + id.slice(0, 8);
  if (dim === 'time') return TIME_BUCKET_LABELS[id] || id;
  return id;
}

// TIME_BUCKET_LABELS renders the four time buckets (see timeBucket) as readable
// windows. Single source for the bucket → label mapping.
const TIME_BUCKET_LABELS = {
  night: 'night 00–08',
  work: 'work 08–16',
  evening: 'evening 16–24',
  weekend: 'weekend',
};

// XP_RAIL_DIMS is the dimension rail order (nine dims). The hash router's
// EXPLORER_DIMS also has a "request" dim (for leaf deep links).
const XP_RAIL_DIMS = ['provider', 'model', 'client', 'conversation', 'tool', 'time', 'status', 'error', 'key'];
const XP_LANDING_DIM = 'provider'; // gallery breakdown on the empty (global) path

// The explorer is a cross-filter (faceted) model, not a linear drill path.
// The URL hash encodes two independent things:
//   - a filter set: order-independent {dim: value} selections (and across dims),
//     encoded as dim/value path segments. Selecting a value in one dimension
//     never clears another dimension's selection - they stack.
//   - a breakdown dimension: which dimension the gallery groups by, encoded as a
//     trailing `by/<dim>` segment (view state, not a filter).
// explorerState derives the full state from the hash. Pure and cheap -
// record-set scoping lives in the one fused derive() pass (live.js);
// gallery counts are the server's.
function explorerState() {
  const segments = parseHash();
  // Split segments into the breakdown marker (trailing by/<dim>) and the filter
  // set (everything else). Dedup filters by dim (last one wins). Only the nine
  // Rail dims may scope (XP_RAIL_DIMS) - parity with the server's validDims:
  // the request leaf and the by marker are view state, never filters.
  let breakdown = XP_LANDING_DIM;
  const filterMap = new Map();
  for (const s of segments) {
    if (s.dim === 'by' && XP_RAIL_DIMS.includes(s.id)) { breakdown = s.id; continue; }
    if (XP_RAIL_DIMS.includes(s.dim)) filterMap.set(s.dim, s.id);
  }
  const filters = [...filterMap.entries()].map(([dim, id]) => ({ dim, id }));
  return { segments, filters, activeDim: breakdown };
}

// ---------- observer-authored model names ----------
// Go's rule executor is the only authority (RE2 and JavaScript differ).
// Exact raw keys travel with a content revision; only full bootstrap or the
// initial fallback SSE snapshot may authorize a different revision.
const MODEL_REVISION_RE = /^[a-f0-9]{64}$/;
let modelRevision = null;
let modelNames = new Map();
function canonicalModel(raw) {
  const key = String(raw || '');
  return modelNames.get(key) ?? key.trim();
}
function validModelCanon(mc, full = false) {
  return !!mc && MODEL_REVISION_RE.test(mc.revision) &&
    mc.names && typeof mc.names === 'object' && !Array.isArray(mc.names) &&
    Object.values(mc.names).every(v => typeof v === 'string') &&
    (!full || Array.isArray(mc.rules));
}
function applyModelCanon(mc, authoritative = false) {
  if (!validModelCanon(mc, authoritative) || (mc.revision !== modelRevision && !authoritative)) {
    fetchBootstrap('full');
    return false;
  }
  const changed = modelRevision !== null && modelRevision !== mc.revision;
  if (mc.revision !== modelRevision) modelNames = new Map();
  modelRevision = mc.revision;
  for (const [raw, name] of Object.entries(mc.names)) modelNames.set(raw, name);
  if (changed) { bumpData(); refreshAggregates(); }
  return true;
}
// Reuse full-replace/batch-eviction: retain only names needed by loaded rows,
// archive pages and debug selectors, not all models ever seen by this tab.
function pruneModelNames() {
  const keep = new Set();
  for (const records of [lastData?.records || [], logArchive]) for (const r of records) if (r) keep.add(r.model || '');
  for (const name of debugState.known_models || []) keep.add(name);
  for (const session of debugState.sessions || []) for (const name of session.models || []) keep.add(name);
  for (const raw of modelNames.keys()) if (!keep.has(raw)) modelNames.delete(raw);
}

// recordMatchesDim reports whether a record matches one filter (dim=value).
// error/status/time/tool are derived (no single stored field); the rest match
// their EXPLORER_DIMS field directly. The Go aggregate fold (dimValue /
// scopedMatch) mirrors these derivations - keep them in lockstep.
function recordMatchesDim(r, dim, id) {
  if (dim === 'error') return recordHasError(r, id);
  if (dim === 'status') return statusClass(r) === id;
  if (dim === 'time') return timeBucket(r) === id;
  if (dim === 'tool') return recordTools(r).includes(id);
  if (dim === 'model') return canonicalModel(r.model) === id;
  const field = EXPLORER_DIMS[dim]?.field;
  return field ? r[field] === id : false;
}

// dimLabel returns a value's display label for its dimension (providers show
// their short label, conversations their short id, errors their type).
function dimLabel(dim, key) {
  if (dim === 'provider') return providerLabel(key);
  return shortId(dim, key);
}

// renderExplorer renders the explorer (scope bar + rail + gallery) from
// current state, dirty-checked per zone via updateSection. Called by
// renderAll (live data) and onNavigate (hash changes). The request log is
// a sibling card, scoped through the cached derive in live.js.
function renderExplorer() {
  if (!$('xp-scope')) return;
  const st = explorerState();
  renderScopeBar(st);
  renderDimRail(st);
  renderGallery(st);
}

// renderScopeBar: the "all traffic" root crumb plus one removable chip per
// active filter (the cross-filter set - order-independent, one per dimension).
// Clicking a chip's ✕ removes just that filter (others stay); "clear all"
// empties the whole set. The chips are the scope - always visible, so global
// vs. scoped is never ambiguous.
function renderScopeBar(st) {
  const parts = [`<a href="#/" class="bc-root">all traffic</a>`];
  if (st.filters.length) parts.push('<span class="bc-sep">·</span><span class="htext">filtered to</span>');
  st.filters.forEach(f => {
    // Chip tooltip uses the obfuscated display form (shortId), never the raw id.
    parts.push(`<span class="xp-chip" style="--ent:${entityType(f.dim).color}" title="${escapeHtml(entityType(f.dim).label)}: ${escapeHtml(shortId(f.dim, f.id))}">${entityBadge(f.dim, f.id)}<button type="button" class="chip-x" data-chip-dim="${escapeHtml(f.dim)}" aria-label="remove ${escapeHtml(entityType(f.dim).label)} filter">✕</button></span>`);
  });
  if (st.filters.length) parts.push('<button type="button" class="btn xp-clear" data-xp-clear>clear all</button>');
  updateSection('xp-scope', parts.join(''));
}

// renderDimRail: one item per dimension - entityBadge + distinct-value count.
// Clicking sets the breakdown dimension (by/<dim>), not a filter. Active = the
// dimension the gallery is grouping by.
function explorerRailCount(dim, payload) {
  if (dim !== 'conversation') return fmt(payload?.rail?.[dim] ?? 0);
  const summary = payload?.conversation_summary;
  if (!summary) return '-';
  return fmt(summary.main) + ' main + ' + fmt(summary.sub) + ' sub' +
    (summary.unresolved ? ' + ' + fmt(summary.unresolved) + ' unresolved' : '');
}
function renderDimRail(st) {
  // distinct-value counts come from the server aggregate (since inception),
  // for every dimension in one payload - no client-side grouping.
  const html = XP_RAIL_DIMS.map(dim => {
    const on = dim === st.activeDim;
    // A rail item is a dimension, not a value: show its distinct-value count.
    // (A sparkline here would be meaningless - every dimension shares the same
    // scope's shape. Share-of-total lives on the node-cards, where it differs.)
    return `<button type="button" class="xp-rail-item${on ? ' active' : ''}" style="--ent:${entityType(dim).color}" data-xp-dim="${dim}" aria-pressed="${on}">${entityBadge(dim, '', entityType(dim).label)}<span class="rail-n">${explorerRailCount(dim, explorerAgg)}</span></button>`;
  }).join('');
  updateSection('xp-rail', html);
  const trig = $('xp-dim-trigger');
  if (trig) {
    const dim = st.activeDim;
    const label = entityType(dim).label;
    trig.style.setProperty('--ent', entityType(dim).color);
    trig.innerHTML = `<span class="xp-dim-by">by</span>${entityBadge(dim, '', label)}<span class="rail-n">${explorerRailCount(dim, explorerAgg)}</span><svg class="hdr-ico xp-dim-caret" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.5 8 10.5 12 6.5"/></svg>`;
    trig.setAttribute('aria-label', 'break down by ' + label);
    trig.title = 'break down by ' + label;
  }
}

function dimMenuUsed() {
  const t = $('xp-dim-trigger');
  return !!(t && getComputedStyle(t).display !== 'none');
}
function closeDimMenu() {
  const rail = $('xp-rail');
  const trig = $('xp-dim-trigger');
  const open = !!(rail && rail.classList.contains('open'));
  if (rail) rail.classList.remove('open');
  if (trig) trig.setAttribute('aria-expanded', 'false');
  return open;
}
function toggleDimMenu() {
  if (!dimMenuUsed()) return;
  const rail = $('xp-rail');
  const trig = $('xp-dim-trigger');
  if (!rail || !trig) return;
  const open = !rail.classList.contains('open');
  rail.classList.toggle('open', open);
  trig.setAttribute('aria-expanded', open ? 'true' : 'false');
}

// renderGallery: one node-card per value of the active dimension, from the
// server's since-inception breakdown (pre-sorted, pre-capped server-side).
// Card = entityBadge + request count + KPI row + sparkline with error overlay
// + error tally. Clicking a card toggles that value as a log filter.
function renderGallery(st) {
  // request is a pure leaf (no value breakdown); the request log already
  // lists the scoped rows.
  if (st.activeDim === 'request') {
    updateSection('xp-gallery', '');
    return;
  }
  // The gallery renders the server's since-inception breakdown for the active
  // dimension (groups pre-sorted desc, pre-capped - the server is the single
  // owner of both, the client applies no cuts or math).
  const payload = explorerAgg && explorerAgg.dim === st.activeDim ? explorerAgg : null;
  if (!payload) {
    updateSection('xp-gallery', `<div class="empty">loading breakdown…</div>`);
    return;
  }
  if (!payload.groups || !payload.groups.length) {
    updateSection('xp-gallery', `<div class="empty">no ${entityType(st.activeDim).label.toLowerCase()} values yet</div>`);
    return;
  }
  const cards = payload.groups.map(e => xpNodeCard(st, e, payload)).join('');
  updateSection('xp-gallery', cards);
  sizeGallery();
}

// sizeGallery sizes the gallery once per render. In the locked viewport layout
// the explorer box has a fixed height and the gallery fills it via CSS (flex);
// otherwise (scrolling page) it fits its content up to a generous cap, then
// scrolls whole rows. The actual sizing is applied by applyGallerySize, which
// runs after layout (rAF) and re-runs on any content/box resize (ResizeObserver)
// - so the first paint, which used to measure before the grid settled and stay
// wrong until a refresh, now self-corrects on the next frame.
function sizeGallery() {
  const gal = $('xp-gallery');
  if (!gal) return;
  requestAnimationFrame(() => applyGallerySize(gal));
}

function applyGallerySize(gal) {
  if (getComputedStyle(document.documentElement).getPropertyValue('--gallery-locked').trim() === '1') {
    if (gal.style.height !== '') gal.style.height = ''; // locked: CSS flex fills the box
    return;
  }
  const cap = Math.max(2 * 118 + 10, 340); // never below two full rows
  const want = Math.min(gal.scrollHeight, cap) + 'px';
  if (gal.style.height !== want) gal.style.height = want; // no-op if unchanged (avoids RO loop)
}

// Re-apply sizing whenever the gallery's content changes size (first paint,
// streaming updates, zoom, window resize). We observe the gallery's first child
// wrapper via the gallery's own scrollHeight changes; the no-op guard in
// applyGallerySize prevents a set-height → observe → set-height loop.
let _galleryRO = null;
function watchGallery() {
  const gal = $('xp-gallery');
  if (!gal || _galleryRO) return;
  let last = -1;
  _galleryRO = new ResizeObserver(() => {
    // Only re-apply when the content height actually changed.
    if (gal.scrollHeight !== last) { last = gal.scrollHeight; applyGallerySize(gal); }
  });
  _galleryRO.observe(gal);
}

// xpNodeCard renders one gallery node-card from the server's entity aggregate
// (jsonEnt): share of all requests (error dim: of all failure events) since
// inception + the entity's own trend sparkline + the per-dimension KPI set
// (`nodeKpis`). No client-side math beyond formatting - percentiles arrive
// precomputed.
function kpiCell(label, inner, title) {
  const t = title ? ` title="${escapeHtml(title)}"` : '';
  return `<span${t}>${escapeHtml(label)} ${inner}</span>`;
}
function kpiNum(label, value, title) {
  return kpiCell(label, `<b>${value}</b>`, title);
}
function kpiTail(label, p50, p95, format, title) {
  const inner = p50 != null
    ? `<b>${format(p50)}</b> <span class="xp-p95">p95 ${format(p95)}</span>`
    : `<b>-</b>`;
  return kpiCell(label, inner, title);
}
function kpiTok(e) { return kpiNum('tok', `${fmt(e.in)}/${fmt(e.out)}`); }
function kpiTtft(e) { return kpiTail('ttft', e.ttft_p50, e.ttft_p95, fmtDur, 'p50 / p95 time-to-first-token (the tail is what users feel)'); }
function kpiTps(e) { return kpiTail('tps', e.tps_p50, e.tps_p95, v => Number(v).toFixed(0), 'p50 / p95 throughput (tok/s)'); }
function kpiCache(e) { return kpiNum('cache', e.in ? pct2(e.cache, e.in) : '-', 'prompt-cache hit rate'); }
function kpiReason(e) { return kpiNum('reason', fmt(e.reasoning || 0), 'reasoning tokens (thinking load)'); }
function kpiErrRate(e) { return kpiNum('err rate', pct2(e.err_final || 0, e.n)); }
// Blended $/Mtok over this entity's cost-reporting tokens (server-computed,
// same rule as the KPI band) - a per-entity spend rate, never diluted by the
// unpriced traffic that also flows through the entity.
function kpiBlend(e) { return kpiNum('per Mtok', e.cost_per_mtok != null ? fmtMoney(e.cost_per_mtok) : '-', 'blended cost per Mtok of in+out tokens (cost-reporting requests only)'); }

// nodeKpis is the per-dimension diagnostic set (single owner). Rules:
//  1. Answer this gallery's question - never a generic perf wall.
//  2. Every slot must be populated for (almost) every card in that gallery
//     (`-` across a row means the KPI does not belong here).
//  3. Never repeat the headline count (n req / n err).
//  4. Same slots on every card in a gallery so layout cannot shift.
function nodeKpis(dim, e) {
  switch (dim) {
    case 'error':
      // Identity of a failure: HTTP status, how many requests it touched
      // (headline is failure events), when it last fired. Never perf -
      // ttft/tps are empty on fast-rejects and many timeouts.
      return kpiNum('status', e.code ? escapeHtml(e.code) : '-')
        + kpiNum('requests', fmt(e.n))
        + kpiNum('last seen', e.last_ms ? new Date(e.last_ms).toLocaleTimeString() : '-');
    case 'status':
      // Health bucket. ttft/tps/tok/cache are empty on 4xx/5xx/cancel;
      // duration lives in the live request table only.
      return kpiErrRate(e);
    case 'key':
      // A credential is a bill + a health check, not an upstream. Latency
      // belongs on provider/model (a key 1:1 with a provider would duplicate
      // that gallery).
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTok(e) + kpiErrRate(e);
    case 'time':
      // When is spend, when does it break, when does it feel slow/fast.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTtft(e) + kpiTps(e);
    case 'tool':
      // calls ≠ request count (one turn can invoke a tool many times).
      return kpiNum('calls', fmt(e.tools))
        + kpiNum('cost', fmtMoney(e.cost))
        + kpiBlend(e);
    case 'conversation':
      // Session economics and wait. Cache/tps average away across mixed turns;
      // volume is already the headline.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTok(e) + kpiTtft(e);
    case 'model':
      // Token-family slots together: in/out, cached-prompt share, reasoning
      // (thinking is model-inherent). Cache renders like client/provider -
      // an unreported hit rate shows 0.0%, not a fabricated gap.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTok(e) + kpiCache(e) + kpiReason(e) + kpiTtft(e) + kpiTps(e);
    default:
      // client / provider: routing peers - spend, tokens, prompt-cache, and
      // the latency/throughput the user feels.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTok(e) + kpiCache(e) + kpiTtft(e) + kpiTps(e);
  }
}

const SPARK_W = 96, SPARK_H = 22;
// The server resolves observed ancestry across the complete scoped history.
// These are exact leaf pivots, never implicit descendant/family filters.
function conversationParent(info) {
  if (!info?.parent_id) return '';
  const scope = info.parent_scope;
  const valid = Array.isArray(scope) && scope.length === CONVERSATION_PARENT_DIMS.length && CONVERSATION_PARENT_DIMS.every(dim =>
    scope.filter(f => f?.dim === dim && typeof f.id === 'string' && f.id !== '').length === 1) &&
    scope.find(f => f.dim === 'conversation')?.id === info.parent_id;
  const label = '↳ parent ' + shortId('conversation', info.parent_id) +
    (info.parent_observed === false ? ' · not observed' : info.parent_in_scope === false ? ' · outside view' : '');
  const title = valid ? 'Open the exact parent conversation; descendants are not included' :
    'Parent reported without a unique filterable namespace';
  return valid
    ? `<a class="xp-conversation-parent" href="${escapeHtml(buildHash([...scope, {dim:'by', id:'conversation'}]))}" title="${title}">${escapeHtml(label)}</a>`
    : `<span class="xp-conversation-parent" title="${title}">${escapeHtml(label)}</span>`;
}
function xpNodeCard(st, e, payload) {
  const dim = st.activeDim;
  const key = e.name;
  // Both footer signals count affected requests, never retry occurrences.
  // A request may have received 429 and a genuine failure; 429 alone is not
  // an error. Hide zero counts; separators only join visible signals.
  // Error-dimension headlines still count failure events.
  const signals = [
    e.err_final > 0 ? `<span class="xp-node-err" title="Requests with a genuine failure, including recovered failures; 429 is not an error">${fmt(e.err_final)} err</span>` : '',
    e.rate_limit_requests > 0 ? `<span class="xp-node-rate" title="Requests with final or retried HTTP 429; each request counted once">${fmt(e.rate_limit_requests)} ×429</span>` : '',
  ].filter(Boolean).join('<span aria-hidden="true"> · </span>');
  const headN = dim === 'error' ? (e.err_events || 0) : e.n;
  const headUnit = dim === 'error' ? 'err' : 'req';
  let shareFrac, shareLabel;
  if (dim === 'error') {
    shareFrac = payload.error_total ? headN / payload.error_total : 0;
    shareLabel = 'of errors';
  } else {
    shareFrac = payload.total ? e.n / payload.total : 0;
    shareLabel = 'of requests';
  }
  const selected = st.filters.some(f => f.dim === dim && f.id === key);
  const kpis = nodeKpis(dim, e);
  // Headline number matches the share's units so the two correlate: on the
  // error dimension both count failure events (final + absorbed).
  const clientAttr = dim === 'client' ? ` data-client="${escapeHtml(key)}"` : '';
  const nodeTitle = dim === 'client' ? '' : ` title="${selected ? 'remove' : 'filter to'} ${escapeHtml(entityType(dim).label)}: ${escapeHtml(shortId(dim, key))}"`;
  const info = dim === 'conversation' ? e.conversation : null;
  const role = info?.role === 'main' || info?.role === 'sub' ? info.role : 'unresolved';
  const roleTitle = role === 'main' ? 'No parent declared in observed history' : role === 'sub' ?
    'Client-declared parent conversation' : info?.issue?.replaceAll('_', ' ') || 'Unresolved relationship';
  const roleBadge = dim === 'conversation' ? `<small class="xp-conversation-role" title="${escapeHtml(roleTitle)}">${role}</small>` : '';
  const card = `<button type="button" class="xp-node${selected ? ' selected' : ''}" style="--ent:${entityType(dim).color}" data-xp-key="${escapeHtml(key)}" aria-pressed="${selected}"${clientAttr}${nodeTitle}>
    <span class="xp-node-hd">${entityBadge(dim, key)}${roleBadge}<span class="xp-node-n">${fmt(headN)}<small>${headUnit}</small></span></span>
    <span class="xp-node-share" title="${shareLabel}">${shareBar(shareFrac, entityType(dim).color)}</span>
    <span class="xp-node-kpis">${kpis}</span>
    <span class="xp-node-foot">${sparklineSVG(e.spark || [], e.spark_err || [], SPARK_W, SPARK_H, entityType(dim).color)}${signals ? `<span class="xp-node-signals">${signals}</span>` : ''}</span>
  </button>`;
  // Parent navigation is a sibling link, never nested inside the card button.
  return dim === 'conversation' ? `<div class="xp-conversation-node">${card}${conversationParent(info)}</div>` : card;
}

// Explorer interactions (delegated; no inline handlers). Cross-filter model:
// rail items set the breakdown dimension (by/<dim>); node cards toggle that
// value in the filter set (stacking across dimensions); chips remove one
// filter; clear-all empties the set. Everything navigates via navigateTo.
$('explorer').addEventListener('click', e => {
  const st = explorerState();
  const clear = e.target.closest('[data-xp-clear]');
  if (clear) { navigateTo([]); return; }
  const chipX = e.target.closest('.chip-x[data-chip-dim]');
  if (chipX) { navigateTo(buildExplorerHash(st.filters.filter(f => f.dim !== chipX.dataset.chipDim), st.activeDim)); return; }
  if (e.target.closest('#xp-dim-trigger')) { e.stopPropagation(); toggleDimMenu(); return; }
  const rail = e.target.closest('.xp-rail-item[data-xp-dim]');
  if (rail) {
    // Switch the breakdown dimension (view state) - filters are untouched.
    closeDimMenu();
    navigateTo(buildExplorerHash(st.filters, rail.dataset.xpDim));
    return;
  }
  const node = e.target.closest('.xp-node[data-xp-key]');
  if (node) {
    // Toggle this value in the filter set: if already selected, deselect it;
    // else add/replace it for this dimension. Other dimensions' filters stack.
    const key = node.dataset.xpKey;
    const without = st.filters.filter(f => f.dim !== st.activeDim);
    const already = st.filters.some(f => f.dim === st.activeDim && f.id === key);
    const next = already ? without : [...without, { dim: st.activeDim, id: key }];
    navigateTo(buildExplorerHash(next, st.activeDim));
    return;
  }
});

// buildExplorerHash encodes the cross-filter state: filter set (dim/value
// pairs, order-independent) + the breakdown dimension (trailing by/<dim>).
function buildExplorerHash(filters, breakdown) {
  const segs = (filters || []).map(f => ({ dim: f.dim, id: f.id }));
  if (breakdown && breakdown !== XP_LANDING_DIM) segs.push({ dim: 'by', id: breakdown });
  return segs;
}
