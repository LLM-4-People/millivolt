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
  // Shares the single error-identity derivation (forEachErrorEntry) with the
  // gallery grouping, so filtering and counting never disagree; the visitor
  // form avoids allocating an entry object per occurrence.
  let hit = false;
  forEachErrorEntry(r, (type, code, msg) => {
    if (!hit && errorKeyMatches(key, type, code, msg)) hit = true;
  });
  return hit;
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

// XP_RAIL_DIMS is the dimension rail order (nine dims).
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
  // the by marker is view state, never a filter.
  let breakdown = XP_LANDING_DIM;
  const filterMap = new Map();
  for (const s of segments) {
    if (s.dim === 'by' && XP_RAIL_DIMS.includes(s.id)) { breakdown = s.id; continue; }
    if (XP_RAIL_DIMS.includes(s.dim)) filterMap.set(s.dim, s.id);
  }
  const filters = [...filterMap.entries()].map(([dim, id]) => ({ dim, id }));
  return { segments, filters, activeDim: breakdown };
}

// explorerFilterKey canonicalizes a filter set to its order-independent
// identity. The by/<dim> breakdown is view state, so explorerState has
// already split it off; callers pass only the filter set. Each dim/id pair is
// JSON-encoded so user values cannot collide with a delimiter.
function explorerFilterKey(filters) {
  const pairs = (filters || []).map(f => [f.dim, f.id]);
  pairs.sort((a, b) => a[0] === b[0]
    ? (a[1] < b[1] ? -1 : a[1] > b[1] ? 1 : 0)
    : (a[0] < b[0] ? -1 : 1));
  return JSON.stringify(pairs);
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
  for (const name of debugState[KNOWN_MODELS_KEY] || []) keep.add(name);
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
  if (dim === 'client') return clientLabel(key);
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
//
// Every repaint replaces the chips, so removal destroys the focused control.
// A chip's dimension position parks a stable successor identity: the surviving
// chip at the same slot, else the previous chip, else "clear all"/the root
// crumb. Focus is only claimed when the rewrite actually dropped it to BODY.
let _scopeFocus = null;
function scopeFocusToken(el) {
  const scope = $('xp-scope');
  if (!scope || !el || !scope.contains(el)) return null;
  const chip = el.closest('.chip-x[data-chip-dim]');
  if (chip) {
    const chips = [...scope.querySelectorAll('.chip-x[data-chip-dim]')];
    return {owner: scope, chipIndex: chips.indexOf(chip)};
  }
  if (el.closest('[data-xp-clear]')) return {owner: scope, clear: true};
  if (el.closest('.bc-root')) return {owner: scope, root: true};
  return null;
}
function restoreScopeFocus(token) {
  _scopeFocus = null;
  if (!token || !token.owner.isConnected) return false;
  const active = document.activeElement;
  // Never steal focus from an operator who moved it elsewhere; a removed
  // element (disconnected) and BODY are the drop cases this owner repairs.
  if (active && active !== document.body && active.isConnected) return false;
  const scope = token.owner;
  let next = null;
  if (token.chipIndex != null) {
    const chips = [...scope.querySelectorAll('.chip-x[data-chip-dim]')];
    if (chips.length) next = chips[Math.min(token.chipIndex, chips.length - 1)];
    if (!next) next = scope.querySelector('[data-xp-clear]');
  } else if (token.clear) {
    next = scope.querySelector('.bc-root');
  } else if (token.root) {
    next = scope.querySelector('.bc-root');
  }
  if (!next) next = scope.querySelector('.bc-root');
  if (!next) return false;
  next.focus();
  return document.activeElement === next;
}
function renderScopeBar(st) {
  const scope = $('xp-scope');
  const active = document.activeElement;
  let focus = null;
  if (scope && active && active !== document.body && active.isConnected && scope.contains(active)) {
    focus = scopeFocusToken(active);
  } else if (scope && (!active || active === document.body || !active.isConnected)) {
    focus = _scopeFocus; // the click-time park survives a blur before hashchange
  }
  _scopeFocus = null;
  const parts = [`<a href="#/" class="bc-root">all traffic</a>`];
  if (st.filters.length) parts.push('<span class="bc-sep">·</span><span class="htext">filtered to</span>');
  st.filters.forEach(f => {
    // Chip tooltip uses the obfuscated display form (shortId), never the raw id.
    parts.push(`<span class="xp-chip" style="--ent:${entityType(f.dim).color}" title="${escapeHtml(entityType(f.dim).label)}: ${escapeHtml(shortId(f.dim, f.id))}">${entityBadge(f.dim, f.id)}<button type="button" class="chip-x" data-chip-dim="${escapeHtml(f.dim)}" aria-label="remove ${escapeHtml(entityType(f.dim).label)} filter">✕</button></span>`);
  });
  if (st.filters.length) parts.push('<button type="button" class="btn xp-clear" data-xp-clear>clear all</button>');
  if (updateSection('xp-scope', parts.join(''))) restoreScopeFocus(focus);
}

// renderDimRail: one item per dimension - entityBadge + distinct-value count.
// Clicking sets the breakdown dimension (by/<dim>), not a filter. Active = the
// dimension the gallery is grouping by.
function explorerRailCount(dim, payload) {
  if (dim === 'conversation') {
    const summary = payload?.conversation_summary;
    if (!summary) return '-';
    return fmt(summary.main) + ' main + ' + fmt(summary.sub) + ' sub' +
      (summary.unresolved ? ' + ' + fmt(summary.unresolved) + ' unresolved' : '');
  }
  // A missing payload or count was never measured: render the shared unknown
  // marker, never a fabricated 0. A measured zero stays a real 0.
  const n = payload?.rail?.[dim];
  return Number.isFinite(n) ? fmt(n) : '-';
}
// The shared dimension trigger sits outside the rail zone's updateSection
// cache, so its presentation is keyed separately on the active dimension and
// rendered count. The key also lives on the node: a replaced trigger has no
// expando and must be repainted even while module state still matches.
let _dimTriggerKey = null;
function renderDimRail(st) {
  // The rail is rebuilt through updateSection, so a count-only repaint
  // replaces the focused button. The shared helper restores it only when the
  // rewrite dropped focus to BODY.
  const rail = $('xp-rail');
  const focus = captureFocus(rail, el => {
    const dim = el.closest('.xp-rail-item[data-xp-dim]')?.dataset.xpDim;
    return dim == null ? null : `.xp-rail-item[data-xp-dim="${CSS.escape(dim)}"]`;
  });
  // distinct-value counts come from the server aggregate (since inception),
  // for every dimension in one payload - no client-side grouping.
  const html = XP_RAIL_DIMS.map(dim => {
    const on = dim === st.activeDim;
    // A rail item is a dimension, not a value: show its distinct-value count.
    // (A sparkline here would be meaningless - every dimension shares the same
    // scope's shape. Share-of-total lives on the node-cards, where it differs.)
    return `<button type="button" class="xp-rail-item${on ? ' active' : ''}" style="--ent:${entityType(dim).color}" data-xp-dim="${dim}" aria-pressed="${on}">${entityBadge(dim, '', entityType(dim).label)}<span class="rail-n">${explorerRailCount(dim, explorerAgg)}</span></button>`;
  }).join('');
  if (updateSection('xp-rail', html)) restoreFocus(focus);
  const trig = $('xp-dim-trigger');
  if (trig) {
    const dim = st.activeDim;
    const count = explorerRailCount(dim, explorerAgg);
    const key = JSON.stringify([dim, count]);
    if (_dimTriggerKey !== key || trig._xpDimKey !== key) {
      _dimTriggerKey = key;
      trig._xpDimKey = key;
      const label = entityType(dim).label;
      trig.style.setProperty('--ent', entityType(dim).color);
      trig.innerHTML = `<span class="xp-dim-by">by</span>${entityBadge(dim, '', label)}<span class="rail-n">${count}</span><svg class="hdr-ico xp-dim-caret" viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.5 8 10.5 12 6.5"/></svg>`;
      trig.setAttribute('aria-label', 'break down by ' + label);
      trig.title = 'break down by ' + label;
    }
  }
}

function dimMenuUsed() {
  const t = $('xp-dim-trigger');
  return !!(t && getComputedStyle(t).display !== 'none');
}
// The dimension rail menu shares chrome.js's class-based menu lifecycle
// (DIM_CLASS_MENU): same open class + aria-expanded contract as the header
// nav flyout. The use-once guard is explorer-specific and stays here.
function closeDimMenu() {
  return classMenuClose(DIM_CLASS_MENU);
}
function toggleDimMenu() {
  if (!dimMenuUsed()) return;
  classMenuToggle(DIM_CLASS_MENU);
}

// renderGallery: one node-card per value of the active dimension, from the
// server's since-inception breakdown (pre-sorted, pre-capped server-side).
// Card = entityBadge + request count + KPI row + sparkline with error overlay
// + error tally. Clicking a card toggles that value as a log filter.
//
// A scope change or invalidation repaints the gallery twice: first the loading
// marker (the old cards are unusable), then the matching payload. The first
// repaint destroys the focused card, but its replacement does not exist until
// the second one. Park that focus identity across the gap and consume it on the
// payload that resolves it; a terminal empty/failure paint clears it so focus
// is never restored into an unrelated later render.
let _galleryFocus = null;
function renderGallery(st) {
  // Scope identity, not innerHTML replacement, gates the cards: a payload for
  // another filter/status set (or another breakdown dimension) is unusable.
  // The matching payload carries groups pre-sorted and pre-capped by the
  // server; the client applies no cuts or math.
  const gallery = $('xp-gallery');
  const focus = captureFocus(gallery, el => {
    const key = el.closest('.xp-node[data-xp-key]')?.dataset.xpKey;
    return key == null ? null : `.xp-node[data-xp-key="${CSS.escape(key)}"]`;
  });
  if (focus) _galleryFocus = focus;
  const scopeKey = scopeQS();
  const payload = explorerAgg && explorerAgg.dim === st.activeDim &&
    explorerAgg.scopeKey === scopeKey ? explorerAgg : null;
  if (!payload) {
    const failed = explorerFailure && explorerFailure.scopeKey === scopeKey &&
      explorerFailure.dim === st.activeDim;
    if (failed) _galleryFocus = null;
    if (updateSection('xp-gallery', `<div class="empty">${failed ? 'breakdown unavailable' : 'loading breakdown…'}</div>`)) restoreFocus(focus);
    return;
  }
  if (!payload.groups || !payload.groups.length) {
    _galleryFocus = null;
    if (updateSection('xp-gallery', `<div class="empty">no ${entityType(st.activeDim).label.toLowerCase()} values yet</div>`)) restoreFocus(focus);
    return;
  }
  const cards = payload.groups.map(e => xpNodeCard(st, e, payload)).join('');
  // The payload resolves any token parked by the loading repaint; a fresh
  // capture covers an ordinary card-list repaint. Consume it in both cases so
  // a card that vanished cannot hold focus for a later unrelated render.
  const restore = _galleryFocus || focus;
  _galleryFocus = null;
  // Only a changed gallery needs the post-layout sizing pass; an identical
  // payload leaves both the DOM and the layout untouched.
  if (updateSection('xp-gallery', cards)) {
    restoreFocus(restore);
    sizeGallery();
  }
}

// sizeGallery sizes the gallery once per render. In the locked viewport layout
// the explorer box has a fixed height and the gallery fills it via CSS (flex);
// otherwise (scrolling page) the gallery compresses to ONE whole row of node
// cards - the same compact band the locked layout's 220px explorer shows -
// with internal scrolling for the remaining rows. The actual sizing is applied
// by applyGallerySize, which runs after layout (rAF) and re-runs on any
// content/box resize (ResizeObserver) - so the first paint, which used to
// measure before the grid settled and stay wrong until a refresh, now
// self-corrects on the next frame.
function sizeGallery() {
  const gal = $('xp-gallery');
  if (!gal) return;
  requestAnimationFrame(() => applyGallerySize(gal));
}

// The row's height is MEASURED from the laid-out cards - card height is
// content-driven, so a nominal estimate could never win - and an empty gallery
// never collapses. The gallery viewport is at least ONE whole row: end-of-scroll
// breathing lives in scroll-padding (not box padding), so nothing of the next
// row peeks into the band.

// applyGallerySize caps the scrolling-page explorer at ONE whole row of node
// cards by sizing the BAND (the .xp-body grid box), not the gallery: the tall
// dimension rail would otherwise stretch the body. Side-by-side bodies align
// the gallery with the band top. Stacked bodies lead with the dimension
// trigger, so the cap adds the measured body-to-gallery lead to the row before
// rounding up; this reserves the trigger and row without a second breakpoint.
// A definite body height clamps its grid row like the locked layout's flexed
// band, while the gallery keeps its natural height and internal scrolling.
function applyGallerySize(gal) {
  const body = gal.closest('.xp-body');
  if (!body) return;
  const locked = getComputedStyle(document.documentElement).getPropertyValue('--gallery-locked').trim() === '1';
  const setH = (el, want) => { if (el.style.height !== want) el.style.height = want; };
  if (locked) { setH(body, ''); setH(gal, ''); return; } // locked: CSS flex fills the box
  const first = gal.firstElementChild;
  if (!first) { setH(body, ''); setH(gal, ''); return; }
  const top = first.offsetTop;
  let row = 0;
  for (const c of gal.children) {
    if (c.offsetTop !== top) break; // the first row ends where the next begins
    row = Math.max(row, c.getBoundingClientRect().height);
  }
  const lead = gal.getBoundingClientRect().top - body.getBoundingClientRect().top;
  // The gallery viewport is ONE row, always - never conditional on the
  // gallery's own scrollHeight: a capped box collapses scrollHeight to the cap,
  // so a "fits: go natural" predicate would flip to clearing the height, the
  // box would regrow (the tall rail stretches it), the observer would re-cap,
  // and the band would oscillate forever - the set-clear-set loop behind the
  // layout jitter. A stable cap is a no-op on re-apply, so the band settles in
  // one frame; the rail and the remaining gallery rows scroll inside it.
  const cap = Math.ceil(row + lead) + 'px';
  setH(body, cap);
  setH(gal, '');
}

// Re-apply sizing whenever the gallery's box changes size (first paint,
// streaming updates, zoom, and the locked/unlocked layout flip: crossing the
// viewport breakpoints changes the box without changing the content, so a
// content-hash guard would miss it). Safe by construction: applyGallerySize
// only writes when the height differs, so a re-apply that changes nothing
// cannot re-fire the observer.
let _galleryRO = null;
function watchGallery() {
  const gal = $('xp-gallery');
  if (!gal || _galleryRO) return;
  _galleryRO = new ResizeObserver(() => applyGallerySize(gal));
  _galleryRO.observe(gal);
}

// xpNodeCard renders one gallery node-card from the server's entity aggregate
// (jsonEnt): share of all requests (error dim: of all error-affected requests)
// since inception + the per-dimension KPI set (`nodeKpis`). No client-side
// math beyond formatting - percentiles arrive precomputed.
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
function kpiTps(e) { return kpiTail('tps', e.tps_p50, e.tps_p95, v => Number(v).toFixed(0), 'p50 / p95 generation speed (tok/s, first to last token)'); }
function kpiCache(e) { return kpiNum('cache', e.in ? pct2(e.cache, e.in) : '-', 'prompt-cache hit rate'); }
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
      // Session economics, volume and wait. Cache/tps average away across
      // mixed turns; blended per-Mtok is derivable from cost+tok and its
      // slot is what keeps the KPI row on one line - a second line makes
      // lineage-bearing cards taller than the one-band gallery (browser
      // contract: rowFullyVisible). Tool calls count invocations, never
      // requests: a chat-only conversation keeps its real 0 and only a
      // missing field renders the shared '-' placeholder.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiTok(e)
        + kpiNum('calls', fmt(e.tools)) + kpiTtft(e);
    case 'model':
      // Token-family slots together: in/out and the cached-prompt share.
      // Cache renders like client/provider - an unreported hit rate shows
      // 0.0%, not a fabricated gap.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTok(e) + kpiCache(e) + kpiTtft(e) + kpiTps(e);
    default:
      // client / provider: routing peers - spend, tokens, prompt-cache, and
      // the latency/throughput the user feels.
      return kpiNum('cost', fmtMoney(e.cost)) + kpiBlend(e) + kpiTok(e) + kpiCache(e) + kpiTtft(e) + kpiTps(e);
  }
}

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
  const synthetic = e.synthetic === true;
  // Both footer signals count affected requests, never retry occurrences.
  // A request may have received 429 and a genuine failure; 429 alone is not
  // an error. Hide zero counts; separators only join visible signals.
  // Error-dimension headlines count failure events (final + absorbed), while
  // card N, the share numerator and the share denominator all stay in
  // affected-request units so the two can never mix.
  const signals = [
    e.err_final > 0 ? `<span class="xp-node-err" title="Requests with a genuine failure, including recovered failures; 429 is not an error">${fmt(e.err_final)} err</span>` : '',
    e.rate_limit_requests > 0 ? `<span class="xp-node-rate" title="${escapeHtml(X429_COUNTING_RULE)}">${fmt(e.rate_limit_requests)} ×429</span>` : '',
  ].filter(Boolean).join('<span aria-hidden="true"> · </span>');
  const headN = dim === 'error' ? (e.err_events || 0) : e.n;
  const headUnit = dim === 'error' ? 'err' : 'req';
  // Share of the dimension's whole. A zero denominator means the share was
  // never measured: no share row at all, never a fabricated 0%.
  let shareFrac = null, shareLabel = '';
  if (dim === 'error') {
    // error_total is the union of affected requests represented by the error
    // cards (final plus absorbed/recovered), exactly like card N. Using either
    // failure events or only Record.IsError requests made the share disappear
    // or exceed 100%.
    if (payload.error_total) { shareFrac = e.n / payload.error_total; shareLabel = 'of error requests'; }
  } else if (payload.total) {
    shareFrac = e.n / payload.total;
    shareLabel = 'of requests';
  }
  const selected = !synthetic && st.filters.some(f => f.dim === dim && f.id === key);
  const kpis = nodeKpis(dim, e);
  // The synthetic overflow card is informational: it carries an explicit
  // marker instead of a filter key, so a real value sharing its display name
  // stays independently selectable.
  const clientAttr = !synthetic && dim === 'client' ? ` data-client="${escapeHtml(key)}"` : '';
  const nodeTitle = synthetic
    ? ' title="aggregated remainder beyond the exact group budget; not filterable"'
    : dim === 'client' ? '' : ` title="${selected ? 'remove' : 'filter to'} ${escapeHtml(entityType(dim).label)}: ${escapeHtml(shortId(dim, key))}"`;
  const keyAttr = synthetic
    ? ' data-xp-synthetic="1" role="group" aria-disabled="true"'
    : ` data-xp-key="${escapeHtml(key)}" aria-pressed="${selected}"`;
  const tag = synthetic ? 'div' : 'button';
  const typeAttr = synthetic ? '' : ' type="button"';
  const label = synthetic ? key + ' (grouped)' : key;
  const info = dim === 'conversation' ? e.conversation : null;
  const role = info?.role === 'main' || info?.role === 'sub' ? info.role : 'unresolved';
  const roleTitle = role === 'main' ? 'No parent declared in observed history' : role === 'sub' ?
    'Client-declared parent conversation' : info?.issue?.replaceAll('_', ' ') || 'Unresolved relationship';
  const roleBadge = dim === 'conversation' ? `<small class="xp-conversation-role" title="${escapeHtml(roleTitle)}">${role}</small>` : '';
  const card = `<${tag}${typeAttr} class="xp-node${selected ? ' selected' : ''}${synthetic ? ' xp-node-synthetic' : ''}" style="--ent:${entityType(dim).color}${synthetic ? ';cursor:default' : ''}"${keyAttr}${clientAttr}${nodeTitle}>
    <span class="xp-node-hd">${entityBadge(dim, synthetic ? '' : key, label)}${roleBadge}<span class="xp-node-n">${fmt(headN)}<small>${headUnit}</small></span></span>
    ${shareFrac != null ? `<span class="xp-node-share" title="${shareLabel}">${shareBar(shareFrac, entityType(dim).color)}</span>` : ''}
    <span class="xp-node-kpis">${kpis}</span>
    ${signals ? `<span class="xp-node-foot"><span class="xp-node-signals">${signals}</span></span>` : ''}
  </${tag}>`;
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
  if (clear) { _scopeFocus = scopeFocusToken(clear); navigateTo([]); return; }
  const chipX = e.target.closest('.chip-x[data-chip-dim]');
  if (chipX) {
    _scopeFocus = scopeFocusToken(chipX);
    navigateTo(buildExplorerHash(st.filters.filter(f => f.dim !== chipX.dataset.chipDim), st.activeDim));
    return;
  }
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
