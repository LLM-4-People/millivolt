// ---------- state ----------
// Dashboard tunables. Seeds match config.Default() only until GET
// /admin/config lands; after that Schema/Default on the server is the
// single source of truth (no second table of numbers here).
const dashCfg = {
  history_size: 10000,
  log_rows: 60,
  poll_ms: 5000,
  chart_ms: 15000,
  explorer_stale_ms: 15000,
  background_refresh: false,
};
// Live-log window: starts at dashCfg.log_rows (cheap first paint) and grows
// when the user scrolls near the bottom. logArchive holds durable pages absent
// from the ring; reset on navigation / full resync so scope can't leak.
let logLimit = 0;
let logArchive = [];
let logFetching = false;
let logExhausted = false;
let logCursor = null; // exact durable (started_at, id) keyset, independent of ring arrival order
// A failed older-page fetch is retried by one delayed owner, not a tight
// timer: logRetryTimer is that single pending retry, logFailures counts
// consecutive failures so the automatic budget stays bounded, and
// logFetchFailed keeps the visible "history unavailable" state until a
// success or a window reset. A user scroll may retry after the budget.
let logRetryTimer = null;
let logFailures = 0;
let logFetchFailed = false;
const LOG_RETRY_LIMIT = 3; // automatic retries after the initial attempt
const LOG_RETRY_MS = 2000; // delayed retry cadence; never a tight loop
// Bumped by resetLogWindow; a store-page fetch that resolves after a window
// reset belongs to the old scope and must be dropped, never merged into the
// new one (a stale page would paint rows the new scope excludes).
let logFetchGen = 0;
const logNearPx = 96; // load-more slack (a few row heights), not a tunable
const logNearSlackPx = 8; // fill-test tolerance: sub-pixel rounding never reads as scrollable room
function clearLogRetry() {
  if (logRetryTimer != null) {
    clearTimeout(logRetryTimer);
    logRetryTimer = null;
  }
}

// The provider-card collapse chevron is shared by the chart metrics picker
// and the lazily loaded settings sheet, so it belongs to the always-loaded
// core rather than either surface.
const PROV_CHEV_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.5 8 10.5 12 6.5"/></svg>';

// One focus/visibility owner for the two modal surfaces. Closed dialogs are
// hidden and inert; active dialogs contain keyboard focus and restore it.
let activeModal = null, modalReturnFocus = null, modalReturnSelector = null;
let modalPreempt = null;
let modalBackground = [];
// openModal preempts the current surface. Its owner teardown (veil classes,
// pressed flags, drawer selection) runs through onPreempt, so a competing
// dialog (the operator prompt) cannot leave a half-closed modal behind.
// returnSelector is the owner's stable focus identity for a control the
// surface may have replaced while it was open (a live request row).
function openModal(el, {returnSelector = null, onPreempt = null} = {}) {
  if (activeModal === el) return;
  if (activeModal) {
    const teardown = modalPreempt;
    closeModal(activeModal);
    if (teardown) teardown();
  }
  modalReturnFocus = document.activeElement;
  modalReturnSelector = returnSelector;
  modalPreempt = onPreempt;
  activeModal = el;
  el.hidden = false;
  el.removeAttribute('inert');
  modalBackground = [...document.body.children].filter(node =>
    node !== el && node.id !== (el.id === 'drawer' ? 'drawer-veil' : 'settings-veil') &&
    node.id !== 'toast' &&
    !['SCRIPT', 'STYLE', 'LINK'].includes(node.tagName)).map(node => [node, node.hasAttribute('inert')]);
  for (const [node] of modalBackground) node.setAttribute('inert', '');
  requestAnimationFrame(() => { if (activeModal === el) (modalFocusables(el)[0] || el).focus(); });
}
function closeModal(el) {
  el.hidden = true;
  el.setAttribute('inert', '');
  if (activeModal !== el) return;
  activeModal = null;
  modalPreempt = null;
  for (const [node, inert] of modalBackground) node.toggleAttribute('inert', inert);
  modalBackground = [];
  const back = modalReturnFocus;
  const selector = modalReturnSelector;
  modalReturnFocus = null;
  modalReturnSelector = null;
  const usable = node => node?.isConnected && !node.closest('[hidden], [inert]');
  let target = usable(back) ? back : null;
  if (!target && selector) {
    try {
      const next = document.querySelector(selector);
      if (usable(next)) target = next;
    } catch (e) {} // a stale/malformed selector never blocks the close
  }
  if (target) target.focus();
}
function modalFocusables(el) {
  return [...el.querySelectorAll('button, input, select, textarea, a[href], [tabindex]')]
    .filter(node => !node.disabled && node.tabIndex >= 0 && !node.closest('[hidden], [inert]') &&
      getComputedStyle(node).display !== 'none' && getComputedStyle(node).visibility !== 'hidden');
}
document.addEventListener('keydown', e => {
  if (e.key !== 'Tab' || !activeModal) return;
  const items = modalFocusables(activeModal);
  const first = items[0], last = items[items.length - 1];
  if (!first) { e.preventDefault(); activeModal.focus(); return; }
  if (!activeModal.contains(document.activeElement) || (e.shiftKey ? document.activeElement === first : document.activeElement === last)) {
    e.preventDefault(); (e.shiftKey ? last : first).focus();
  }
});
document.addEventListener('focusin', e => {
  if (activeModal && !activeModal.contains(e.target)) (modalFocusables(activeModal)[0] || activeModal).focus();
});

let storageState = null; // accepted bootstrap signal; never locally incremented
// Canvas colors resolve from :root tokens (chart series + grid). Entity
// colors live in ENTITY_TYPES - never a second palette here.
const COLORS = {};
// refreshChartColors re-reads the canvas palette from the document's current
// tokens. Boot calls it once; the theme picker calls it again after switching
// palettes, because a CSS variable change does not repaint an existing canvas.
function refreshChartColors() {
  ['accent', 'accent2', 'err', 'cyan', 'ok', 'warn', 'rl', 'muted', 'border2'].forEach(k => {
    COLORS[k] = getComputedStyle(document.documentElement).getPropertyValue('--' + k).trim();
  });
  // Faint warm gridline (translucent panel border) for chart axes. The alpha
  // is owned by the dashboard token --chart-grid-a; the literal is the
  // fallback when the token is unreadable (robustness: a failed or stripped
  // CSS load must not break the axes).
  const chartGridA = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--chart-grid-a'));
  COLORS.grid = hexA(COLORS.border2, Number.isFinite(chartGridA) ? chartGridA : 0.5);
}
// hexA blends a #rrggbb color into an rgba() string (chart bar fills and the
// gridline below). A value that is not exact 6-digit hex returns unchanged -
// a malformed CSS var must never become 'rgba(NaN,NaN,NaN,...)'.
function hexA(hex, a) {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex);
  return m ? `rgba(${parseInt(m[1], 16)},${parseInt(m[2], 16)},${parseInt(m[3], 16)},${a})` : hex;
}
refreshChartColors();

// localStorage throws in privacy mode - keep the dashboard alive regardless.
const storage = {
  get(k) { try { return localStorage.getItem(k); } catch(e) { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch(e) {} },
};

// streamLive is the SSE/poll connection; pauseState owns the operator holds
// (in-flight finish, new requests queue). They are independent - pausing
// the proxy must not freeze the dashboard (you'd miss in-flight completing).
// The known_* field names are the server's wire vocabulary for the
// seen-name lists every operator menu offers; one constant each so no
// access site can drift a letter. Same string values as before - the
// properties themselves are unchanged.
const KNOWN_CLIENTS_KEY = 'known_clients';
const KNOWN_PROVIDERS_KEY = 'known_providers';
const KNOWN_MODELS_KEY = 'known_models';
let streamLive = false;
let pauseState = { paused: false, clients: [], providers: [], holds: [], [KNOWN_CLIENTS_KEY]: [], [KNOWN_PROVIDERS_KEY]: [], until: null, queued: 0, default_max_queued: 0 };
let pauseEditID = '';
let throttleState = { throttles: [], [KNOWN_PROVIDERS_KEY]: [], active: false };
let debugState = { enabled: false, sessions: [], [KNOWN_CLIENTS_KEY]: [], [KNOWN_PROVIDERS_KEY]: [], [KNOWN_MODELS_KEY]: [], until: null, ttl: '', max_bytes: '' };
let debugEditID = '';
// localStorage key for the persisted status filter. One owner: the write
// path in chrome_lists.js (doFilter) references the same constant.
const FILTERS_STORAGE_KEY = 'dash.filters';
// Filter state: only status remains a dropdown filter (client/provider/model/
// conversation/error are explorer dimensions now). Persisted across reloads.
let filters = (() => {
  try { return { status: '', ...JSON.parse(storage.get(FILTERS_STORAGE_KEY) || '{}') }; }
  catch (e) { return { status: '' }; }
})();
let drawerId = null; // id of the record currently shown in the detail drawer
// The attempt selector beside it: when the drawer shows one absorbed attempt
// instead of the request, this is its index in drawerId's attempts array.
// Set only by the attempt open paths; closeDrawer, a plain openDrawer and
// drawerNav clear it. renderDrawer re-resolves the (id, index) pair at
// render time - never a captured attempt object - so a record replaced
// wholesale keeps the view coherent and a dangling index fails closed to
// the request view.
let drawerAttempt = null;
let source = null;
let lastRender = {}; // dirty-check hashes to avoid re-rendering unchanged sections

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    if (cancelSettingsOpen()) return;
    if (closeDimMenu()) return;
    const metricsMenu = $('chart-metrics-menu');
    if (metricsMenu && !metricsMenu.hidden) {
      setMetricsMenuOpen(false);
      $('chart-metrics-btn').focus();
      return;
    }
    if (closeHeaderMenus(undefined, true)) { e.preventDefault(); return; }
    if (closeNavMenu(true)) { e.preventDefault(); return; }
    if (drawerId) closeDrawer();
    return;
  }
  if (settingsIsOpen() && e.key === '/' && e.target && e.target.tagName !== 'INPUT' && e.target.tagName !== 'TEXTAREA') {
    e.preventDefault();
    const q = $('settings-q');
    if (q) q.focus();
  }
});

// ---------- helpers ----------
// Canonical provider badge. The server stores the provider as the registrable
// domain (eTLD+1) of the API base URL - every subdomain collapses to the root
// (api.openai.com → openai.com). The favicon is resolved from that same host
// (a provider's marketing site usually lives there), and the display label is
// its first segment. Hosts without a public favicon (IPs, localhost, *.internal)
// keep the type-icon sibling, as do icons that fail to load.
function providerOrigin(name) {
  if (typeof name !== 'string' || !name) return '';
  name = name.toLowerCase();
  if (/^\d{1,3}(\.\d{1,3}){3}(:\d+)?$/.test(name)) return '';          // IPv4
  if (name.includes(':') || name.startsWith('[')) return '';           // IPv6 / host:port
  if (name === 'localhost' || name.endsWith('.localhost') || name.endsWith('.local') || name.endsWith('.internal')) return '';
  // Only qualified DNS hostnames are candidates. Arbitrary provider labels
  // and private single-label hosts must not become guessed external lookups.
  if (name.length > 253 || !name.includes('.') || !name.split('.').every(label =>
    /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))) return '';
  return name;
}
function providerLabel(name) {
  if (!name) return '?';
  return name.includes('.') && !/^\d/.test(name) ? name.split('.')[0] : name;
}
// clientLabel renders a recorded client identity for display. The stored
// value IS the identity (the X-Proxy-Client label or the inferred user-agent
// identity), and no surface collapses or rewrites it: the request table
// shows exactly what the client reported.
function clientLabel(name) {
  return String(name || '').trim();
}
const _faviconBroken = new Set();
function faviconErr(img, origin) {
  // Swap the failed icon for the type-icon fallback sibling (one-shot; dead
  // origins are cached so re-renders don't hammer the favicon service).
  _faviconBroken.add(origin);
  img.style.display = 'none';
  const fb = img.nextElementSibling;
  if (fb) fb.style.display = 'inline-block';
}
// Delegated favicon error handling (capture phase: resource errors don't
// bubble) - avoids an inline onerror that would interpolate the origin.
document.addEventListener('error', e => {
  const img = e.target;
  if (img instanceof HTMLImageElement && img.classList.contains('favicon')) faviconErr(img, img.dataset.origin);
}, true);
// X429_COUNTING_RULE is the one counting-rule sentence every ×429 signal
// tooltip carries (the chart's rate-limited series and the explorer node
// badge): a request counts once whether the 429 was its final status or an
// absorbed retry. The chart's series title appends its doctrine clause on
// top of this shared sentence.
const X429_COUNTING_RULE = 'Requests with final or retried HTTP 429; each request counted once';

// MAGNITUDES is the one compact-scale ladder (divisor + suffix) every large
// number formatter walks down: 1e9 → B, 1e6 → M, 1e3 → K, largest first.
// compactMagnitude owns the shared scale walk and its 1000K → 1M rollover;
// fmt and fmtMoney only own their rounding and unit styling.
const MAGNITUDES = [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
// compactMagnitude returns {value, suffix} for one finite absolute value:
// below 1000 the value stays exact with no suffix; at or above 1000 the value
// is rounded to two decimals at the chosen scale. A rounded 1000 at K or M
// rolls into the next suffix so money and counts never print "1000K".
function compactMagnitude(n) {
  const a = Math.abs(Number(n));
  for (let i = 0; i < MAGNITUDES.length; i++) {
    const [div, suf] = MAGNITUDES[i];
    if (a < div) continue;
    let v = Math.round((a / div) * 100) / 100;
    if (v >= 1000 && i > 0) {
      v = Math.round((v / 1000) * 100) / 100;
      return {value: v, suffix: MAGNITUDES[i - 1][1]};
    }
    return {value: v, suffix: suf};
  }
  return {value: a, suffix: ''};
}
// fmt is the single compact number formatter for the whole dashboard: exact
// below 1000, then K/M/B with up to 2 decimals (698479531 → "698.48M"). Every
// surface uses it, so big counts never overflow a tile; vt() tooltips carry the
// full un-rounded value. Negative and non-finite inputs are safe.
const fmt = n => {
  if (n == null || !isFinite(n)) return '-';
  const neg = n < 0 ? '-' : '';
  const a = Math.abs(Number(n));
  if (a < 1000) return neg + a.toLocaleString();
  const m = compactMagnitude(a);
  return neg + m.value.toString() + m.suffix;
};
// fmtFull is the exact grouped form (698479531 → "698,479,531") for tooltips.
const fmtFull = n => n == null ? '-' : Number(n).toLocaleString();
// fmtBytes is the single byte-size formatter for captured wire sizes and
// long-text readouts. Exact below 1 KB, then the decimal KB/MB/GB ladder with
// at most two decimals (1234 → "1.23 KB"). Exact counts stay recoverable
// through vt() titles; money keeps its own fmtMoney owner.
const BYTE_UNITS = [[1e12, 'TB'], [1e9, 'GB'], [1e6, 'MB'], [1e3, 'KB']];
const fmtBytes = n => {
  if (n == null || !isFinite(n)) return '-';
  const neg = n < 0 ? '-' : '';
  const a = Math.abs(Number(n));
  if (a < 1e3) return neg + Math.round(a) + ' B';
  for (let i = 0; i < BYTE_UNITS.length; i++) {
    const [div, suf] = BYTE_UNITS[i];
    if (a < div) continue;
    let v = Math.round((a / div) * 100) / 100;
    // A rounded 1000 at KB/MB rolls into the next unit (never "1000 KB").
    if (v >= 1000 && i > 0) {
      v = Math.round((v / 1000) * 100) / 100;
      return neg + v + ' ' + BYTE_UNITS[i - 1][1];
    }
    return neg + v + ' ' + suf;
  }
  return neg + Math.round(a) + ' B';
};
// fmtDur is the single duration formatter for every ms value, compact and
// readable at any scale: ms below 1s, seconds below 1m, then minutes, then
// hours and days - always with the unit so it never overflows (450 → "450ms",
// 1260 → "1.26s", 90000 → "1.50m", 5400000 → "1.50h"). Use it for every ms
// duration. vt() tooltips carry the exact ms where precision matters.
const fmtDur = ms => {
  if (ms == null || !isFinite(ms)) return '-';
  const neg = ms < 0 ? '-' : '';
  const a = Math.abs(Number(ms));
  if (a < 1000) {
    const rounded = Math.round(a * 1000) / 1000;
    return neg + (rounded >= 1000 ? '1s' : (rounded || a) + 'ms');
  }
  const r2 = v => Math.round(v * 100) / 100;
  if (a < 60000) { const s = r2(a / 1000); return s >= 60 ? neg + '1m' : neg + s + 's'; }   // 1.26s
  if (a < 3600000) { const m = r2(a / 60000); return m >= 60 ? neg + '1h' : neg + m + 'm'; } // 1.50m
  if (a < 86400000) { const h = r2(a / 3600000); return h >= 24 ? neg + '1d' : neg + h + 'h'; }
  return neg + r2(a / 86400000) + 'd';
};
// fmtTTFT is TTFT's display owner. Server ttft_ms is 0 when no token arrived
// or the response did not stream (absent), never a measured zero: sub-ms
// positives are rounded up to 1ms.
// fmtDur(0) stays "0ms" for real durations (axes, elapsed time).
const fmtTTFT = ms => ms ? fmtDur(ms) : '-';
// TPS_UNIT is the one compact generation-speed unit for narrow surfaces (the
// KPI band and the summary timing tile). Chart axes, legends and tooltips keep
// the full "tok/s" label where there is room.
const TPS_UNIT = 't/s';
// A K/M/B magnitude suffix must not fuse with the compact unit ("1.23Kt/s"
// reads as kilotons per second). tpsUnitGap inserts the one separator both
// compact surfaces use; below 1000 the requested tight form stays
// ("148.239t/s"). tpsUnitAfter owns the range's shared trailing unit and
// fmtTPSCompact the complete value.
const tpsUnitGap = s => (/[KMB]$/.test(s) ? ' ' : '');
const tpsUnitAfter = s => tpsUnitGap(s) + TPS_UNIT;
const fmtTPSCompact = v => {
  if (v == null || !Number.isFinite(v)) return '-';
  const s = fmt(v);
  return s + tpsUnitAfter(s);
};
// fmtMoney owns every displayed cost. Values remain USD in data; magnitudes
// below $1 display as cents (including fractional cents), with at most three
// decimals in the displayed unit. Dollar amounts retain compact K/M/B units.
// The cents form is rounded at the same precision before the unit is chosen:
// a value whose displayed cents reach 100 promotes to the dollar form ($1),
// never the impossible "100¢".
const fmtMoney = x => {
  if (x == null || !isFinite(x)) return '-';
  const neg = x < 0 ? '-' : '';
  const a = Math.abs(Number(x));
  const trim = v => {
    let s = v.toFixed(3);
    if (s.includes('.')) s = s.replace(/\.?0+$/, '');
    return s;
  };
  if (a < 1) {
    const cents = Math.round(a * 100 * 1000) / 1000;
    return cents >= 100 ? neg + '$' + trim(a) : neg + trim(a * 100) + '¢';
  }
  if (a < 1000) return neg + '$' + trim(a);
  const m = compactMagnitude(a);
  return neg + '$' + trim(m.value) + m.suffix;
};
// pctValue is the one percentage-value formatter. toFixed rounds, so a
// strictly-sub-100 value like 99.99 would print 100.0 and falsely claim a
// perfect rate. Floor any value below 100 to the largest sub-100 value at
// this precision; only a value >= 100 may ever print 100. The `trim` flag
// keeps the chart/storm compact style (100%, 75%) while the shared pct/pct2
// ratio owner keeps its exact decimal width. A non-finite value was never
// measured: render '-'.
const pctValue = (v, d = 1, trim = false) => {
  if (!Number.isFinite(v)) return '-';
  let x = v;
  if (x < 100) x = Math.min(x, 100 - Math.pow(10, -d));
  let s = x.toFixed(d);
  if (trim && s.includes('.')) s = s.replace(/\.?0+$/, '');
  return s + '%';
};
const pctCap = (a, b, d) => {
  let v = (a / b) * 100;
  // Use the counts, not the rounded ratio, so a strict a < b always floors
  // even when floating division rounds the ratio to exactly 100.
  if (a < b) v = Math.min(v, 100 - Math.pow(10, -d));
  return pctValue(v, d);
};
const pct = (a, b) => b ? pctCap(a, b, 1) : '-';
// pct2 is the 2-decimal variant for per-request cache hit, where precision matters.
const pct2 = (a, b) => b ? pctCap(a, b, 2) : '-';
// vt wraps a display value in a span whose title carries the full text, so a
// value that ellipsizes (overflow: hidden + text-overflow: ellipsis) is always
// fully recoverable on hover. Use for any value that can exceed its container.
const vt = (display, full) => `<span class="vt" title="${escapeHtml(String(full ?? display))}">${display}</span>`;
const $ = id => document.getElementById(id);

// ---------- focus preservation across repaints ----------
// Redraws that replace innerHTML (request rows, gallery cards, chart legend,
// the explorer rail) remove the focused control and leave the browser on
// BODY. captureFocus records the focused descendant and its stable selector;
// restoreFocus puts focus back only when the repaint actually dropped it to
// BODY. If the operator moved focus elsewhere, it is never stolen.
function captureFocus(owner, keyOf) {
  if (!owner) return null;
  const active = document.activeElement;
  if (!active || active === owner || active === document.body || !owner.contains(active)) return null;
  const key = keyOf ? keyOf(active) : null;
  return key ? {owner, key} : null;
}
function restoreFocus(token) {
  if (!token || !token.owner.isConnected) return false;
  const active = document.activeElement;
  if (active && active !== document.body) return false;
  let next = null;
  try { next = token.owner.querySelector(token.key); } catch (e) { return false; }
  if (!next || next === token.owner || next === active) return false;
  // Focus restoration is a repaint concern, not navigation: a replaced row
  // must not scroll the log (or the page) to the restored control.
  next.focus({preventScroll: true});
  return document.activeElement === next;
}

// requestGate creates a deduplication/supersede wrapper for fetch calls.
// Returns a `run` function: run(key, fetchFn, applyFn, mode) where:
//   key     - a string identifying this request (query string, mode, etc.)
//   fetchFn - called with `accept(payload, ok)` and an AbortSignal; the
//             fetch implementation calls accept() when the response arrives.
//   applyFn - called only if this response won the supersede race.
//   mode    - 'reuse' dedupes the same key; 'refresh' replaces settled data
//             but lets in-flight work finish; 'invalidate' also cancels
//             same-key work (restart/purge/config made that result stale).
//   failFn  - optional; called only when the current request reports failure,
//             never for superseded or deduped work, so a failed scan can own
//             its visible error state without racing newer requests.
// Guarantees: (1) the newest issued request's response always applies; (2) an
// identical key can optionally dedupe (no duplicate in-flight fetches for the
// same key); (3) a fetch failure unpins the dedupe so the next trigger works;
// (4) superseded work is cancelled instead of delaying the new selection.
// run returns a promise settling true when the current request applied its
// payload, false on failure or supersede. A deduped call settles false
// immediately: the in-flight owner already reports its own outcome.
function requestGate(dedupe) {
  let seq = 0, pinned = null, controller = null, settlePending = null;
  return function run(key, fetchFn, applyFn, mode = 'reuse', failFn = null) {
    if (dedupe && key === pinned && (mode === 'reuse' || (controller && mode !== 'invalidate'))) {
      return Promise.resolve(false);
    }
    pinned = key;
    const my = ++seq;
    if (controller) {
      controller.abort();
      // An implementation may ignore the abort signal and never call accept.
      // Settle the superseded awaiter here so a forced resync cannot hang on
      // work this gate already replaced.
      if (settlePending) settlePending(false);
    }
    controller = new AbortController();
    let settle = null;
    const settled = new Promise(resolve => { settle = resolve; });
    settlePending = settle;
    let done = false;
    const finish = ok => { if (!done) { done = true; settle(ok); } };
    fetchFn((payload, ok) => {
      if (my !== seq) { finish(false); return false; } // superseded by a newer request
      controller = null;
      if (!ok) { pinned = null; finish(false); if (failFn) failFn(); return false; } // failure unpins the dedupe
      applyFn(payload);
      finish(true);
      return true;
    }, controller.signal);
    return settled;
  };
}

// setupCanvas sizes the chart box's blank-state canvas and returns a 2d
// context whose transform already accounts for devicePixelRatio. The blank
// canvas is created on demand and removed when uPlot mounts - uPlot always
// creates its own canvas inside the box (a <canvas> target is only a DOM
// container to it, never the drawing surface).
function setupCanvas(box) {
  let cv = box.querySelector('canvas.chart-blank');
  if (!cv) {
    cv = document.createElement('canvas');
    cv.className = 'chart-blank';
    box.appendChild(cv);
  }
  const dpr = window.devicePixelRatio || 1;
  cv.style.width = '100%';
  cv.style.height = '100%';
  const w = cv.offsetWidth;
  const h = cv.offsetHeight;
  // Assigning width/height reallocates and clears the backing store even when
  // the pixel size is unchanged; the empty chart re-renders on every refresh.
  const pw = Math.max(1, Math.round(w * dpr));
  const ph = Math.max(1, Math.round(h * dpr));
  if (cv.width !== pw) cv.width = pw;
  if (cv.height !== ph) cv.height = ph;
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { ctx, w, h };
}
