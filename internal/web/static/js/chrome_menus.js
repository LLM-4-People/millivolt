// Header menu registry and lifecycle, the nav/dimension class menus, duration
// parsing, dash value application, and the shared safe-delete filter menu.
// The operator console owns every policy surface; the theme tray is the one
// remaining header menu in this registry. The duration vocabulary lives in
// chrome_lists.js, which loads before this module because FILTER_AGES reads it
// at initialization.

const HEADER_MENUS = { 'btn-theme': 'theme-menu' };
function closeHeaderMenus(except, restoreFocus = false) {
  if (except && except !== 'settings') closeSettings(true);
  // The console is the operator plane's one open surface: opening another
  // header surface closes it, and its own open path closes them first.
  if (except !== 'operator-console' && typeof closeOperatorConsole === 'function') closeOperatorConsole();
  let closed = false;
  for (const [button, id] of Object.entries(HEADER_MENUS)) {
    if (id === except) continue;
    if (hideHdrMenu(id)) {
      closed = true;
      if (restoreFocus) $(button)?.focus();
    }
  }
  syncHdrMenuExpanded();
  return closed;
}

function syncHdrMenuExpanded() {
  for (const [bid, mid] of Object.entries(HEADER_MENUS)) {
    const b = $(bid), m = $(mid);
    if (b && m) b.setAttribute('aria-expanded', m.hidden ? 'false' : 'true');
  }
}

function visibleHeaderControl() {
  const nav = $('btn-nav');
  if (nav && nav.getClientRects().length) return nav;
  return $('btn-settings');
}

// Class-based menu lifecycle, shared by the header nav flyout (mobile) and
// the explorer dimension rail. One descriptor names the menu container, its
// trigger button, and the open class; the three helpers own the rest.
const NAV_CLASS_MENU = { id: 'hdr-actions', btn: 'btn-nav', cls: 'is-open' };
const DIM_CLASS_MENU = { id: 'xp-rail', btn: 'xp-dim-trigger', cls: 'open' };

// setClassMenuOpen flips the open class and the trigger's aria-expanded
// together: the two must never disagree. Deny by default - a missing
// container or trigger means no menu to act on.
function setClassMenuOpen(menu, open) {
  const el = $(menu.id), btn = $(menu.btn);
  if (!el || !btn) return;
  el.classList.toggle(menu.cls, open);
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}

// classMenuClose closes only if it was open and reports whether it did.
// afterClose runs after a real close (focus restoration lives there).
function classMenuClose(menu, afterClose) {
  const el = $(menu.id);
  if (!el || !el.classList.contains(menu.cls)) return false;
  setClassMenuOpen(menu, false);
  if (afterClose) afterClose();
  return true;
}

// classMenuToggle flips and reports the new state; onOpen runs before the
// open lands (close-competitive menus, use-once guards).
function classMenuToggle(menu, onOpen) {
  const el = $(menu.id), btn = $(menu.btn);
  if (!el || !btn) return false;
  const open = !el.classList.contains(menu.cls);
  if (open && onOpen) onOpen();
  setClassMenuOpen(menu, open);
  return open;
}

function closeNavMenu(restoreFocus = false) {
  return classMenuClose(NAV_CLASS_MENU, () => {
    if (restoreFocus) $(NAV_CLASS_MENU.btn)?.focus();
  });
}
function toggleNavMenu(e) {
  e.stopPropagation();
  // Opening the nav flyout closes the competing header menus first.
  classMenuToggle(NAV_CLASS_MENU, closeHeaderMenus);
}
// Header menus may own transient state that a close must discard however the
// close arrived (Escape, an outside click, another menu, a Close button). The
// lifecycle owns hiding; each module registers its cleanup once instead of
// duplicating key or click handlers.
const HEADER_MENU_CLOSE = new Map();
function onHeaderMenuClose(id, cleanup) { HEADER_MENU_CLOSE.set(id, cleanup); }

function hideHdrMenu(id) {
  const el = $(id);
  if (!el) return false;
  const wasOpen = !el.hidden;
  el.hidden = true;
  if (wasOpen) HEADER_MENU_CLOSE.get(id)?.();
  syncHdrMenuExpanded();
  return wasOpen;
}

// maxGoDurationMS is time.ParseDuration's int64 nanosecond ceiling expressed
// in milliseconds; the server rejects anything beyond it.
const maxGoDurationMS = 9223372036854775807 / 1e6;

// parseGoDurationValue is the strict owner: null means the text is not a Go
// duration at all. It mirrors time.ParseDuration, the server's authority:
// one optional leading sign, `0` by itself, then one or more number+unit
// groups where a number is `digits[.digits]` or `.digits`, and the unit is
// one of the Go spellings. The old unanchored scan accepted trailing garbage
// and misread `.5h` as 5h, which the settings Apply gate now treats as truth.
// parseGoDuration keeps the historic lenient 0 result for callers that treat
// garbage as "no interval".
function parseGoDurationValue(s) {
  if (s == null) return null;
  s = String(s).trim();
  if (!s) return null;
  let sign = 1;
  if (s[0] === '+' || s[0] === '-') {
    if (s[0] === '-') sign = -1;
    s = s.slice(1);
  }
  if (s === '0') return 0;
  if (!s) return null;
  const unitMS = {ns:1e-6, us:0.001, 'µs':0.001, 'μs':0.001, ms:1, s:1000, m:60000, h:3600000};
  const isDigit = ch => ch >= '0' && ch <= '9';
  let total = 0, i = 0, groups = 0;
  while (i < s.length) {
    const start = i;
    while (i < s.length && isDigit(s[i])) i++;
    const pre = i > start;
    let post = false;
    if (s[i] === '.') {
      i++;
      const frac = i;
      while (i < s.length && isDigit(s[i])) i++;
      post = i > frac;
    }
    if (!pre && !post) return null;
    const number = parseFloat(s.slice(start, i));
    const unitStart = i;
    while (i < s.length && s[i] !== '.' && !isDigit(s[i])) i++;
    if (i === unitStart) return null;
    const unit = unitMS[s.slice(unitStart, i)];
    if (unit == null) return null;
    total += number * unit;
    groups++;
  }
  if (!groups || !Number.isFinite(total) || Math.abs(total) > maxGoDurationMS) return null;
  return sign * total;
}
function parseGoDuration(s) {
  const value = parseGoDurationValue(s);
  return value == null ? 0 : value;
}

// parseGoByteSize mirrors internal/config parseByteSizeString: a bare integer
// is bytes, and the accepted suffixes are B/KiB/MiB/GiB (case-insensitive).
// null means the text is not a byte size.
function parseGoByteSize(s) {
  if (s == null) return null;
  const compact = String(s).trim().replace(/\s+/g, '');
  if (!compact) return null;
  const m = /^(\d+)(GiB|MiB|KiB|B)?$/i.exec(compact);
  if (!m) return null;
  const mult = {gib: 1 << 30, mib: 1 << 20, kib: 1 << 10, b: 1}[ (m[2] || 'b').toLowerCase() ];
  const n = Number(m[1]);
  if (!Number.isSafeInteger(n) || !mult) return null;
  const value = n * mult;
  return Number.isSafeInteger(value) ? value : null;
}

function applyDashValues(v) {
  if (!v) return;
  // Presence, not truthiness: after GET /admin/config the server is the owner.
  // core.js seeds are first-paint only (TestDashCfgSeedsMatchDefault).
  const cadence = () => `${dashCfg.poll_ms}|${dashCfg.chart_ms}|${dashCfg.explorer_stale_ms}`;
  const before = cadence();
  const logRowsBefore = dashCfg.log_rows;
  const wasBackgroundRefresh = dashCfg.background_refresh;
  if (v.history_size != null) dashCfg.history_size = +v.history_size;
  if (v.dash_log_rows != null) dashCfg.log_rows = +v.dash_log_rows;  if (v.dash_background_refresh != null) dashCfg.background_refresh = !!v.dash_background_refresh;
  // parseGoDuration returns 0 for garbage; a 0ms interval would tight-loop.
  // Fail closed (keep the first-paint seed). Validate already rejects 0.
  if (v.dash_poll_interval != null) {
    const poll = parseGoDuration(v.dash_poll_interval);
    if (poll > 0) dashCfg.poll_ms = poll;
  }
  if (v.dash_chart_refresh != null) {
    const chart = parseGoDuration(v.dash_chart_refresh);
    if (chart > 0) dashCfg.chart_ms = chart;
  }
  if (v.dash_explorer_stale != null) {
    const stale = parseGoDuration(v.dash_explorer_stale);
    if (stale > 0) dashCfg.explorer_stale_ms = stale;
  }
  // A page-size change reshapes the log window; repaint it explicitly instead
  // of relying on an unrelated KPI revision bump to invalidate the derivation.
  if (dashCfg.log_rows !== logRowsBefore && typeof scheduleRenderLive === 'function') scheduleRenderLive();
  // Re-arm only on an actual cadence change - the 5s tick calls this with an
  // unchanged config, and clearing/setting the interval every tick would
  // starve it (a tick younger than the interval never fires). Payloads can
  // still land while hidden (an in-flight resume fetch, the SSE reset path),
  // and the visibilitychange handler only runs on transitions, so a cadence
  // change must not re-arm a tick the hide transition stopped. A combined
  // cadence+toggle-off payload still ends stopped via the branch below.
  if (typeof armDashboardTicks === 'function' && cadence() !== before &&
      (!document.hidden || dashCfg.background_refresh)) armDashboardTicks();
  // A hot-reloaded toggle-off must release the background hold and stop the
  // tick even while the tab is hidden (the visibilitychange handler only
  // runs on the transition).
  if (wasBackgroundRefresh && !dashCfg.background_refresh) {
    if (typeof dashReleaseBackgroundLock === 'function') dashReleaseBackgroundLock();
    if (document.hidden && typeof dashStopDashboardTick === 'function') dashStopDashboardTick();
  }
  // Symmetric activation: a flag flipped on while already hidden (only
  // reachable via an out-of-band config PUT) must arm the tick and take the
  // hold now - no visibilitychange fires until the next transition.
  if (!wasBackgroundRefresh && dashCfg.background_refresh && document.hidden && typeof dashHoldBackgroundLock === 'function') {
    dashHoldBackgroundLock();
    if (typeof armDashboardTicks === 'function') armDashboardTicks();
  }
}

// ---------- safe delete (settings sheet, storage category) ----------
// Both database deletions live in the settings sheet's storage category:
// Delete matching removes a filtered subset (by provider / model / client /
// status / errors / debug / age) with a live count preview before anything is
// deleted, and Delete everything wipes the whole database. Every request is
// one self-contained row (retries live in the row's attempts JSON), so a
// filtered delete can never leave orphan entries.
// Use the dispatch path: a delegated action may replace/detach its target
// before this later document listener runs. Live ancestry then lies about
// where the click originated; the composed event path remains stable.
document.addEventListener('click', e => {
  const path = e.composedPath();
  const inside = selector => path.some(node => node.matches?.(selector));
  // A modal dialog is its own surface: a press inside the operator
  // confirmation or the storm details must not dismiss the panel that opened
  // it. Both render outside the header and the console.
  if (!inside('.menu-wrap') && !inside('#operator-console') && !inside('#btn-controls') &&
      !inside('.operator-dialog') && !inside('.storm-dialog')) closeHeaderMenus();
  if (!inside('#hdr-actions') && !inside('#btn-nav') && !inside('#operator-console')) closeNavMenu();
  if (!inside('.xp-rail-col')) closeDimMenu();
  if (!inside('#chart-metrics')) setMetricsMenuOpen(false);
});

// populateFilterMenu fills the settings sheet's delete-matching dropdowns
// from the current record set. onChange wires the per-select count refresh
// for a surface without its own delegated listener; the settings block
// passes none because its #settings-fields change delegation owns it.
const FILTER_ERRORS = [['', 'any'], ['1', 'only errors']];
const FILTER_DEBUG = [['', 'any'], ['1', 'only debug']];
// FILTER_AGES is the one history-window vocabulary: the age spans every
// surface offers, with the word each reads as in a delete filter. The tokens
// are canonical Go duration spellings because every consumer derives a
// `before_ms` from them through parseGoDuration, and a Go test round-trips
// each one through ParseDuration and FormatDuration.
//
// The tokens are shared; the prose is not. An export says what a window
// includes and a delete filter says what it matches, so operator_export.js
// derives its choices from this list while keeping its own wording.
const FILTER_AGES = [['', 'any'], ['1h', DURATION_LABELS['1h']], ['24h', '1 day'], ['168h', '1 week']];

// FILTER_AGE_TOKENS is the token column alone, for surfaces that supply their
// own words.
const FILTER_AGE_TOKENS = FILTER_AGES.map(([token]) => token);

function populateFilterMenu(prefix, onChange) {
  const recs = lastData?.records || [];
  const fill = (id, vals, label = v => v) => {
    const el = $(id);
    const cur = el.value;
    fillSelectPairs(el, [['', 'any']].concat(vals.map(v => [v, label(v)])), cur);
  };
  fill(prefix+'-provider', [...new Set(recs.map(r => r.provider).filter(Boolean))].sort());
  fill(prefix+'-client', [...new Set(recs.map(r => r.client).filter(Boolean))].sort(), clientLabel);
  // 0 is an in-flight row, not a response status: offering it would let a
  // filter that the backend treats as "ignore" look like a real match.
  fill(prefix+'-status', [...new Set(recs.map(r => r.status_code).filter(n => n > 0))].sort((a,b)=>a-b).map(String));
  // The model select groups spelling variants under their canonical group
  // (an <optgroup> per multi-variant group) so the same model clusters
  // visually - but each <option> keeps its raw value: delete/download
  // filters match the exact stored spelling, and the grouping never widens
  // a delete.
  {
    const el = $(prefix+'-model');
    const cur = el.value;
    const groups = groupByCanonical(recs.map(r => r.model).filter(Boolean))
      .sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
    let html = '<option value="">any</option>';
    let curPresent = cur === '';
    for (const { key, variants } of groups) {
      const opts = variants.map(m => {
        if (m === cur) curPresent = true;
        return `<option value="${escapeHtml(m)}">${escapeHtml(m)}</option>`;
      }).join('');
      html += (variants.length > 1)
        ? `<optgroup label="${escapeHtml(key)} (${variants.length})">${opts}</optgroup>`
        : opts;
    }
    el.innerHTML = html;
    el.value = curPresent ? cur : '';
  }
  const errCur = $(prefix+'-errors').value;
  const dbgCur = $(prefix+'-debug').value;
  const ageCur = $(prefix+'-age').value;
  fillSelectPairs($(prefix+'-errors'), FILTER_ERRORS, errCur);
  fillSelectPairs($(prefix+'-debug'), FILTER_DEBUG, dbgCur);
  fillSelectPairs($(prefix+'-age'), FILTER_AGES, ageCur);
  if (onChange) [prefix+'-provider', prefix+'-model', prefix+'-client', prefix+'-status', prefix+'-errors', prefix+'-debug', prefix+'-age'].forEach(id => { $(id).onchange = onChange; });
}

// filterFromUI reads the settings sheet's delete-matching block (id prefix
// del) into a filter object matching the backend PurgeFilter JSON shape.
function filterFromUI(prefix, now = Date.now()) {
  const age = parseGoDuration($(prefix+'-age').value);
  return {
    provider: $(prefix+'-provider').value || '',
    model: $(prefix+'-model').value || '',
    client: $(prefix+'-client').value || '',
    status_code: +($(prefix+'-status').value || 0),
    has_error: $(prefix+'-errors').value === '1',
    debug: $(prefix+'-debug').value === '1',
    before_ms: age ? now - age : 0,
  };
}
function clearFilterActive(f) {
  return f.provider || f.model || f.client || f.status_code || f.has_error || f.debug || f.before_ms;
}

// purgeCount is the one count reader for both purge surfaces: the console's
// export preview and the Settings sheet's delete-matching block. Both must
// preview with the exact predicate they act on, so the request, the fallback
// message and the count validation live here once. It lives in this module
// because this module loads first and owns the purge filter vocabulary; the
// export module reads it rather than keeping a second copy.
async function purgeCount(filter) {
  const doc = await operatorJsonBody('/admin/purge/count', {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(filter),
  }, PURGE_COUNT_UNAVAILABLE);
  if (!isCount(doc.count)) throw new Error(doc.error || PURGE_COUNT_UNAVAILABLE);
  return doc.count;
}

// The one sentence a failed or malformed count reports.
const PURGE_COUNT_UNAVAILABLE = 'count unavailable';

// updateFilterCount paints the delete-matching block's preview into elId and
// gates btnId on it. phrase(count) is the label; the optional busy predicate
// lets a surface disable its action while its own mutation is in flight. Each
// surface owns its latest preview, including the exact older-than instant.
// Empty selections invalidate it too. Actions reuse this predicate, never a
// newly calculated Date.now() that would widen the confirmed deletion.
const _filterPreviews = new Map();
function filterSelection(prefix) { return JSON.stringify(filterFromUI(prefix, 0)); }
function previewedFilter(prefix) {
  const preview = _filterPreviews.get(prefix);
  return preview && preview.count > 0 && preview.selection === filterSelection(prefix) ? preview : null;
}
async function updateFilterCount(prefix, elId, btnId, phrase, busy) {
  const f = filterFromUI(prefix);
  const el = $(elId);
  const preview = {filter: f, selection: filterSelection(prefix), count: null};
  _filterPreviews.set(prefix, preview);
  $(btnId).disabled = true;
  if (!clearFilterActive(f)) { el.textContent = ''; return; }
  el.textContent = 'counting…';
  try {
    const count = await purgeCount(f);
    if (_filterPreviews.get(prefix) !== preview || preview.selection !== filterSelection(prefix)) return;
    preview.count = count;
    el.textContent = phrase(count);
    $(btnId).disabled = count === 0 || !!(busy && busy());
  } catch (e) {
    // A reply that lands after the selection moved on says nothing about the
    // current one, so it writes neither the count nor the failure.
    if (_filterPreviews.get(prefix) === preview && preview.selection === filterSelection(prefix)) {
      el.textContent = String(e.message || PURGE_COUNT_UNAVAILABLE);
    }
  }
}
// One shape, one owner: the count-agreement rule and the record noun both
// live beside fmt, so this is a call rather than a second spelling.
function recPhrase(n) { return countPhrase(n, 'record'); }
// refreshAggregates invalidates and re-pulls the since-inception numeric
// surfaces now so a purge (or any wipe) is visible before the next tick. The
// invalidation clears the cached payloads and paints loading before each
// request; the returned promise settles when both forced requests finish.
function refreshAggregates() {
  return Promise.all([fetchChart('invalidate'), fetchExplorer('invalidate')]);
}

// refreshAfterRestart re-pulls every server-fed surface after the SSE
// reconnects to a new process (feed_id change): the bootstrap state surfaces
// (mode 'none' - the SSE full snapshot already rebuilt the records; applying
// another snapshot here would race the SSE replay) plus the chart/explorer
// scans (including footer counts), and config (applyDashValues re-arms the page's
// own cadences; an open Settings sheet refills in place). Bootstrap checks
// the frontend content version too; only changed assets require a reload.
function refreshAfterRestart(bootstrapApplied = false) {
  if (!bootstrapApplied) {
    // A changed frontend reloads at the bootstrap gate and skips this
    // callback, avoiding scans whose responses would be thrown away.
    fetchBootstrap('none', () => refreshAfterRestart(true));
    return;
  }
  refreshAggregates();
  // Settings is lazy: only refresh its document after the module has loaded.
  if (typeof fetchSettings === 'function') fetchSettings();
}
