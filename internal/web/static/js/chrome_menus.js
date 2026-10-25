// Header menu registry and lifecycle, the nav/dimension class menus, duration
// parsing, dash value application, and the shared safe-delete and log-export
// filter menus. The duration vocabulary lives in chrome_lists.js, which loads
// before this module because FILTER_AGES reads it at initialization.

const HEADER_MENUS = { 'btn-pause': 'pause-menu', 'btn-debug': 'debug-menu', 'btn-limits': 'limits-menu', 'btn-logs': 'logs-menu', 'btn-restart': 'restart-menu', 'btn-theme': 'theme-menu' };
function closeHeaderMenus(except, restoreFocus = false) {
  if (except && except !== 'settings') closeSettings(true);
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

function parseGoDuration(s) {
  if (s == null || s === '') return 0;
  s = String(s).trim();
  if (!s) return 0;
  const re = /(-?\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)/g;
  let m, total = 0, any = false;
  while ((m = re.exec(s))) {
    any = true;
    const n = parseFloat(m[1]);
    const mul = {ns:1e-6, us:0.001, 'µs':0.001, 'μs':0.001, ms:1, s:1000, m:60000, h:3600000}[m[2]];
    total += n * (mul || 0);
  }
  return any ? total : 0;
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
// filtered delete can never leave orphan entries. The Logs ▾ menu reuses the
// same filter options (and the same backend count endpoint) for choosing
// exactly what to download.

function toggleFilterMenu(e, menuId, prefix, refresh) {
  toggleHdrMenu(e, menuId, () => populateFilterMenu(prefix, refresh), refresh);
}
// Use the dispatch path: a delegated action may replace/detach its target
// before this later document listener runs. Live ancestry then lies about
// where the click originated; the composed event path remains stable.
document.addEventListener('click', e => {
  const path = e.composedPath();
  const inside = selector => path.some(node => node.matches?.(selector));
  if (!inside('.menu-wrap')) closeHeaderMenus();
  if (!inside('#hdr-actions') && !inside('#btn-nav')) closeNavMenu();
  if (!inside('.xp-rail-col')) closeDimMenu();
  if (!inside('#chart-metrics')) setMetricsMenuOpen(false);
});

// populateFilterMenu fills a filter surface's dropdowns from the current
// record set (shared by the Logs menu and the settings sheet's
// delete-matching block - same options, same values). onChange wires the
// per-select count refresh for surfaces without their own delegated
// listener (the logs menu); the settings block passes none because its
// #settings-fields change delegation owns the refresh.
const FILTER_ERRORS = [['', 'any'], ['1', 'only errors']];
const FILTER_DEBUG = [['', 'any'], ['1', 'only debug']];
// Filter ages reuse the shared hour wording for '1h'; the longer spans are
// deliberately day/week wording in the age context, not clock arithmetic.
const FILTER_AGES = [['', '-'], ['1h', DURATION_LABELS['1h']], ['24h', '1 day'], ['168h', '1 week']];

function populateFilterMenu(prefix, onChange) {
  const recs = lastData?.records || [];
  const fill = (id, vals, label = v => v) => {
    const el = $(id);
    const cur = el.value;
    fillSelectPairs(el, [['', 'any']].concat(vals.map(v => [v, label(v)])), cur);
  };
  fill(prefix+'-provider', [...new Set(recs.map(r => r.provider).filter(Boolean))].sort());
  fill(prefix+'-client', [...new Set(recs.map(r => r.client).filter(Boolean))].sort(), clientLabel);
  fill(prefix+'-status', [...new Set(recs.map(r => r.status_code))].sort((a,b)=>a-b).map(String));
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

// filterFromUI reads a filter surface (by id prefix: del = the settings
// sheet's delete-matching block, lf = the logs menu) into a filter object
// matching the backend PurgeFilter JSON shape.
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

// updateFilterCount POSTs /admin/purge/count for the Logs menu or the
// delete-matching block and writes the preview into elId / enables btnId.
// phrase(count) is the label; the optional busy predicate lets a surface
// disable its action while its own mutation is in flight. Each surface owns
// its latest preview, including the exact older-than instant. Empty
// selections invalidate it too. Actions reuse this predicate, never a newly
// calculated Date.now() that would widen the confirmed deletion.
const _filterPreviews = new Map();
function filterSelection(prefix) { return JSON.stringify(filterFromUI(prefix, 0)); }
function previewedFilter(prefix) {
  const preview = _filterPreviews.get(prefix);
  return preview && preview.count > 0 && preview.selection === filterSelection(prefix) ? preview : null;
}
async function updateFilterCount(prefix, elId, btnId, phrase, busy) {
  const fail = 'count unavailable';
  const f = filterFromUI(prefix);
  const el = $(elId);
  const preview = {filter: f, selection: filterSelection(prefix), count: null};
  _filterPreviews.set(prefix, preview);
  $(btnId).disabled = true;
  if (!clearFilterActive(f)) { el.textContent = ''; return; }
  el.textContent = 'counting…';
  try {
    const d = await operatorJsonBody('/admin/purge/count', { method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(f) }, fail);
    if (_filterPreviews.get(prefix) !== preview || preview.selection !== filterSelection(prefix)) return;
    if (!Number.isSafeInteger(d.count) || d.count < 0) throw new Error(d.error || fail);
    preview.count = d.count;
    el.textContent = phrase(d.count);
    $(btnId).disabled = d.count === 0 || !!(busy && busy());
  } catch (e) {
    if (_filterPreviews.get(prefix) === preview && preview.selection === filterSelection(prefix)) {
      el.textContent = String(e.message || fail);
    }
  }
}
function recPhrase(n) { return fmt(n) + ' record' + (n === 1 ? '' : 's'); }
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

// ---------- log export (Logs ▾ menu) ----------
// Pick exactly what to download: the same filter dimensions as the
// delete-matching block (provider / model / client / status / errors /
// debug / age), applied and previewed against the full database via the
// shared count endpoint - the export then streams the same rows the
// preview announced. "Download all" exports everything.

function toggleLogsMenu(e) { toggleFilterMenu(e, 'logs-menu', 'lf', updateLogsCount); }

// logsExportURL turns a filter into the export endpoint query (GET-only,
// read-only, streams the whole DB - the preview count and the download can
// never disagree: both use the same predicate).
function logsExportURL(f) {
  const p = new URLSearchParams();
  if (f.provider) p.set('provider', f.provider);
  if (f.model) p.set('model', f.model);
  if (f.client) p.set('client', f.client);
  if (f.status_code) p.set('status_code', String(f.status_code));
  if (f.has_error) p.set('has_error', '1');
  if (f.debug) p.set('debug', '1');
  if (f.before_ms) p.set('before_ms', String(f.before_ms));
  const qs = p.toString();
  return '/metrics/export' + (qs ? '?' + qs : '');
}

function updateLogsCount() {
  return updateFilterCount('lf', 'logs-count', 'btn-logs-filtered', n => 'download ' + recPhrase(n) + ' from the full database');
}

function downloadLogsFiltered() {
  const preview = previewedFilter('lf');
  if (!preview) return updateLogsCount();
  triggerDownload(logsExportURL(preview.filter), '');
  hideHdrMenu('logs-menu');
}

function downloadLogsAll() {
  triggerDownload('/metrics/export', '');
  hideHdrMenu('logs-menu');
}
