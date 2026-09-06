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
};
// Live-log window: starts at dashCfg.log_rows (cheap first paint) and grows
// when the user scrolls near the bottom. logArchive holds durable pages absent
// from the ring; reset on navigation / full resync so scope can't leak.
let logLimit = 0;
let logArchive = [];
let logFetching = false;
let logExhausted = false;
let logCursor = null; // exact durable (started_at, id) keyset, independent of ring arrival order
// Bumped by resetLogWindow; a store-page fetch that resolves after a window
// reset belongs to the old scope and must be dropped, never merged into the
// new one (a stale page would paint rows the new scope excludes).
let logFetchGen = 0;
const logNearPx = 96; // load-more slack (a few row heights), not a tunable
let settingsDoc = null;

// One focus/visibility owner for the two modal surfaces. Closed dialogs are
// hidden and inert; active dialogs contain keyboard focus and restore it.
let activeModal = null, modalReturnFocus = null;
let modalBackground = [];
function openModal(el) {
  if (activeModal === el) return;
  if (activeModal) closeModal(activeModal);
  modalReturnFocus = document.activeElement;
  activeModal = el;
  el.hidden = false;
  el.removeAttribute('inert');
  modalBackground = [...document.body.children].filter(node =>
    node !== el && node.id !== (el.id === 'drawer' ? 'drawer-veil' : 'settings-veil') &&
    !['SCRIPT', 'STYLE', 'LINK'].includes(node.tagName)).map(node => [node, node.hasAttribute('inert')]);
  for (const [node] of modalBackground) node.setAttribute('inert', '');
  requestAnimationFrame(() => { if (activeModal === el) (modalFocusables(el)[0] || el).focus(); });
}
function closeModal(el) {
  el.hidden = true;
  el.setAttribute('inert', '');
  if (activeModal !== el) return;
  activeModal = null;
  for (const [node, inert] of modalBackground) node.toggleAttribute('inert', inert);
  modalBackground = [];
  const back = modalReturnFocus;
  modalReturnFocus = null;
  if (back?.isConnected && !back.closest('[hidden], [inert]')) back.focus();
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
['accent', 'accent2', 'err', 'cyan', 'ok', 'warn', 'muted', 'border2'].forEach(k => {
  COLORS[k] = getComputedStyle(document.documentElement).getPropertyValue('--' + k).trim();
});
// Faint warm gridline (translucent panel border) for chart axes.
COLORS.grid = (() => {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(COLORS.border2);
  return m ? `rgba(${parseInt(m[1],16)},${parseInt(m[2],16)},${parseInt(m[3],16)},.5)` : COLORS.border2;
})();

// localStorage throws in privacy mode - keep the dashboard alive regardless.
const storage = {
  get(k) { try { return localStorage.getItem(k); } catch(e) { return null; } },
  set(k, v) { try { localStorage.setItem(k, v); } catch(e) {} },
};

// streamLive is the SSE/poll connection; pauseState owns the operator holds
// (in-flight finish, new requests queue). They are independent - pausing
// the proxy must not freeze the dashboard (you'd miss in-flight completing).
let streamLive = false;
let pauseState = { paused: false, clients: [], providers: [], holds: [], known_clients: [], known_providers: [], until: null, queued: 0, default_max_queued: 0 };
let pauseEditID = '';
let throttleState = { throttles: [], known_providers: [], active: false };
let debugState = { enabled: false, sessions: [], known_clients: [], known_providers: [], known_models: [], until: null, ttl: '', max_bytes: 0 };
let debugEditID = '';
// Filter state: only status remains a dropdown filter (client/provider/model/
// conversation/error are explorer dimensions now). Persisted across reloads.
let filters = (() => {
  try { return { status: '', ...JSON.parse(storage.get('dash.filters') || '{}') }; }
  catch (e) { return { status: '' }; }
})();
let drawerId = null; // id of the record currently shown in the detail drawer
let source = null;
let lastRender = {}; // dirty-check hashes to avoid re-rendering unchanged sections

document.addEventListener('keydown', e => {
  if (e.key === 'Escape') {
    if (settingsIsOpen()) { closeSettings(); return; }
    if (closeDimMenu()) return;
    if (closeHeaderMenus(undefined, true)) { e.preventDefault(); return; }
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
// fmt is the single compact number formatter for the whole dashboard: exact
// below 1000, then K/M/B with up to 2 decimals (698479531 → "698.48M"). Every
// surface uses it, so big counts never overflow a tile; vt() tooltips carry the
// full un-rounded value. Negative and non-finite inputs are safe.
const fmt = n => {
  if (n == null || !isFinite(n)) return '-';
  const neg = n < 0 ? '-' : '';
  const a = Math.abs(Number(n));
  if (a < 1000) return neg + a.toLocaleString();
  const units = [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
  for (const [div, suf] of units) {
    if (a >= div) {
      let v = a / div;
      v = Math.round(v * 100) / 100; // 2 decimals
      if (v >= 1000 && div < 1e9) { v = Math.round((v / 1000) * 100) / 100; // roll 1000K → 1M
        return neg + v.toString() + units[units.findIndex(u => u[0] === div) - 1][1]; }
      return neg + v.toString() + suf;
    }
  }
  return neg + a.toLocaleString();
};
// fmtFull is the exact grouped form (698479531 → "698,479,531") for tooltips.
const fmtFull = n => n == null ? '-' : Number(n).toLocaleString();
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
// fmtMoney owns every displayed cost. Values remain USD in data; magnitudes
// below $1 display as cents (including fractional cents), with at most three
// decimals in the displayed unit. Dollar amounts retain compact K/M/B units.
const fmtMoney = x => {
  if (x == null || !isFinite(x)) return '-';
  const neg = x < 0 ? '-' : '';
  const a = Math.abs(Number(x));
  const cents = a < 1;
  const trim = v => {
    let s = v.toFixed(3);
    if (s.includes('.')) s = s.replace(/\.?0+$/, '');
    return cents ? neg + s + '¢' : neg + '$' + s;
  };
  if (cents) return trim(a * 100);
  if (a < 1000) return trim(a);
  const units = [[1e9, 'B'], [1e6, 'M'], [1e3, 'K']];
  for (const [div, suf] of units) {
    if (a >= div) return trim(Math.round((a / div) * 100) / 100) + suf;
  }
  return trim(a);
};
// Percentage formatting. toFixed rounds, so a strictly-sub-100 ratio like
// 99.996% would print as "100.0%" / "100.00%" and falsely claim a perfect rate
// (a provider at 99.xx% cache read displayed "100%"). Floor such ratios to the
// largest sub-100 value at this precision; only a >= b may ever show "100%".
const pctCap = (a, b, d) => {
  let v = (a / b) * 100;
  if (a < b) v = Math.min(v, 100 - Math.pow(10, -d));
  return v.toFixed(d) + '%';
};
const pct = (a, b) => b ? pctCap(a, b, 1) : '0%';
// pct2 is the 2-decimal variant for per-request cache hit, where precision matters.
const pct2 = (a, b) => b ? pctCap(a, b, 2) : '0%';
// vt wraps a display value in a span whose title carries the full text, so a
// value that ellipsizes (overflow: hidden + text-overflow: ellipsis) is always
// fully recoverable on hover. Use for any value that can exceed its container.
const vt = (display, full) => `<span class="vt" title="${escapeHtml(String(full ?? display))}">${display}</span>`;
const $ = id => document.getElementById(id);

// requestGate creates a deduplication/supersede wrapper for fetch calls.
// Returns a `run` function: run(key, fetchFn, applyFn, mode) where:
//   key     - a string identifying this request (query string, mode, etc.)
//   fetchFn - called with `accept(payload, ok)` and an AbortSignal; the
//             fetch implementation calls accept() when the response arrives.
//   applyFn - called only if this response won the supersede race.
//   mode    - 'reuse' dedupes the same key; 'refresh' replaces settled data
//             but lets in-flight work finish; 'invalidate' also cancels
//             same-key work (restart/purge/config made that result stale).
// Guarantees: (1) the newest issued request's response always applies; (2) an
// identical key can optionally dedupe (no duplicate in-flight fetches for the
// same key); (3) a fetch failure unpins the dedupe so the next trigger works;
// (4) superseded work is cancelled instead of delaying the new selection.
function requestGate(dedupe) {
  let seq = 0, pinned = null, controller = null;
  return function run(key, fetchFn, applyFn, mode = 'reuse') {
    if (dedupe && key === pinned && (mode === 'reuse' || (controller && mode !== 'invalidate'))) return;
    pinned = key;
    const my = ++seq;
    if (controller) controller.abort();
    controller = new AbortController();
    fetchFn((payload, ok) => {
      if (my !== seq) return false; // superseded by a newer request
      controller = null;
      if (!ok) { pinned = null; return false; } // failure unpins the dedupe
      applyFn(payload);
      return true;
    }, controller.signal);
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
  cv.width = Math.max(1, Math.round(w * dpr));
  cv.height = Math.max(1, Math.round(h * dpr));
  const ctx = cv.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  return { ctx, w, h };
}
