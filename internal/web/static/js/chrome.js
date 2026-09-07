// Storm detection and recovery belong to the server. Accepted bootstrap
// snapshots replace this bounded status surface and its open detail dialog.
// Incident buttons retain their identity across updates for keyboard focus.
let stormSnapshot = {valid: false, enabled: false, storms: []};
let stormDialogKey = '';
const stormIncidentKey = s => JSON.stringify([s.provider, s.scope, s.model]);
const stormPercent = value => value.toLocaleString(undefined, {maximumFractionDigits: 1});
function applyStormState(st) {
  const banner = $('storm-banner');
  if (!banner) return;
  const valid = s => s && typeof s.provider === 'string' && s.provider.length > 0 &&
    typeof s.model === 'string' && (s.scope === 'provider' && !s.model || s.scope === 'model') &&
    ['open', 'half_open'].includes(s.state) && typeof s.reason === 'string' &&
    Number.isFinite(s.error_percent) && s.error_percent >= 0 && s.error_percent <= 100 &&
    (s.scope !== 'provider' || Number.isSafeInteger(s.active_models) && s.active_models >= 0 &&
      Number.isSafeInteger(s.affected_models) && s.affected_models >= 0 && s.affected_models <= s.active_models &&
      Number.isFinite(s.affected_model_percent) && s.affected_model_percent >= 0 && s.affected_model_percent <= 100) &&
    ['error_requests', 'failures', 'samples', 'window_ms', 'queued', 'recovery_successes', 'recovery_required'].every(key =>
      Number.isSafeInteger(s[key]) && s[key] >= 0) && s.failures <= s.samples && s.window_ms > 0 &&
    s.error_requests <= s.failures &&
    s.recovery_required > 0 && s.recovery_successes <= s.recovery_required &&
    typeof s.retry_at === 'string' && Number.isFinite(Date.parse(s.retry_at));
  const keys = new Set();
  const accepted = typeof st?.enabled === 'boolean' && typeof st.banner_enabled === 'boolean' &&
    Array.isArray(st.storms) && st.storms.every(s => {
      if (!valid(s)) return false;
      const key = stormIncidentKey(s);
      if (keys.has(key)) return false;
      keys.add(key);
      return true;
    });
  stormSnapshot = {valid: accepted, enabled: accepted && st.enabled, storms: accepted ? st.storms : []};
  const storms = accepted && st.enabled && st.banner_enabled ? st.storms : [];
  const existing = new Map([...banner.children].map(row => [row.dataset.value, row]));
  storms.forEach((s, index) => {
    const key = stormIncidentKey(s);
    const scope = s.provider + ' · ' + (s.scope === 'provider' ? 'all models' : s.model || '(unspecified model)');
    const html = `<span class="storm-heading"><strong>${escapeHtml(scope)}</strong><span class="storm-state">${s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'}</span><span class="storm-open-hint">View details ›</span></span><span class="storm-detail"><span class="storm-reason">${escapeHtml(s.reason)}</span><span>${escapeHtml(stormPercent(s.error_percent))}% failed attempts</span><span>${fmt(s.error_requests)} requests affected</span><span>${fmt(s.queued)} queued</span></span>`;
    let row = existing.get(key);
    existing.delete(key);
    if (!row) {
      row = document.createElement('button');
      row.type = 'button';
      row.className = 'storm-row';
      row.dataset.operator = 'storm-open';
      row.dataset.value = key;
      row.setAttribute('aria-haspopup', 'dialog');
      row.setAttribute('aria-controls', 'storm-dialog');
    }
    if (row.stormHTML !== html) { row.innerHTML = html; row.stormHTML = html; }
    if (banner.children[index] !== row) banner.insertBefore(row, banner.children[index] || null);
  });
  for (const row of existing.values()) row.remove();
  banner.hidden = storms.length === 0;
  renderStormDetails();
}

function openStormDetails(key) {
  if (!stormSnapshot.enabled || !stormSnapshot.storms.some(s => stormIncidentKey(s) === key)) return;
  let dialog = $('storm-dialog');
  if (!dialog) {
    dialog = document.createElement('section');
    dialog.id = 'storm-dialog';
    dialog.className = 'storm-dialog';
    dialog.hidden = true;
    dialog.tabIndex = -1;
    dialog.setAttribute('inert', '');
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    dialog.setAttribute('aria-labelledby', 'storm-dialog-title');
    dialog.innerHTML = `<div class="storm-dialog-panel"><div class="drawer-hd"><h3 id="storm-dialog-title">Error storm details</h3><button class="btn" type="button" data-operator="storm-close" aria-label="Close error storm details">Close</button></div><div class="storm-dialog-body" id="storm-dialog-body"></div></div>`;
    dialog.addEventListener('click', e => { if (e.target === dialog) closeStormDetails(); });
    dialog.addEventListener('keydown', e => {
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeStormDetails(); }
    });
    document.body.appendChild(dialog);
  }
  closeHeaderMenus();
  closeNavMenu();
  closeDimMenu();
  stormDialogKey = key;
  renderStormDetails();
  openModal(dialog);
}

function closeStormDetails() {
  const dialog = $('storm-dialog');
  if (!dialog || dialog.hidden) return;
  const returnTo = modalReturnFocus;
  closeModal(dialog);
  stormDialogKey = '';
  if (!returnTo?.isConnected || returnTo.closest('[hidden], [inert]')) {
    const banner = $('storm-banner');
    (banner && !banner.hidden ? banner : visibleHeaderControl())?.focus();
  }
}

function renderStormDetails() {
  if (!stormDialogKey || !$('storm-dialog-body')) return;
  const incident = stormSnapshot.storms.find(s => stormIncidentKey(s) === stormDialogKey);
  if (!stormSnapshot.valid || !stormSnapshot.enabled || !incident) {
    const message = !stormSnapshot.valid ? 'Current incident details are unavailable.' :
      !stormSnapshot.enabled ? 'Error storm protection is disabled.' : 'This incident is no longer active.';
    updateSection('storm-dialog-body', `<p class="storm-resolution" role="status">${message}</p>`);
    return;
  }
  const s = incident;
  const entries = [
    ['Provider', s.provider], ['Model scope', s.scope === 'provider' ? 'All models' : s.model || '(unspecified model)'],
    ['State', s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'], ['Error', s.reason],
    ['Requests affected', fmt(s.error_requests)], ['Failed upstream attempts', fmt(s.failures)],
    ['Sampled upstream attempts', fmt(s.samples)], ['Upstream attempt failure rate', stormPercent(s.error_percent) + '%'],
    ['Detection window', fmtDur(s.window_ms)], ['Queued requests', fmt(s.queued)],
    ['Next retry', new Date(s.retry_at).toLocaleString()],
    ['Successful recovery probes', s.recovery_successes + ' / ' + s.recovery_required],
  ];
  if (s.scope === 'provider') entries.push(['Active models', fmt(s.active_models)],
    ['Affected active models', fmt(s.affected_models) + ' / ' + fmt(s.active_models) + ' (' + stormPercent(s.affected_model_percent) + '%)']);
  const rows = entries.map(([label, value]) => `<div><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(value)}</dd></div>`).join('');
  updateSection('storm-dialog-body', `<dl class="storm-facts">${rows}</dl><p class="storm-explanation">Counts cover the rolling detection window. Each request with a selected failure in this window is counted once; retries count as separate upstream attempts. Recovery checks use queued requests, so the next retry may start after the scheduled time.</p>`);
}

// Operator credential owner for the gated plane. The server never issues or
// returns the credential; gated calls attach it as a Bearer header and one
// 401 opens the prompt below. sessionStorage keeps the value tab-scoped: it
// never outlives the browser session, and it is dropped as soon as the
// server rejects it.
const OPERATOR_KEY = 'millivolt.operatorToken';
let operatorCredential = '';
try { operatorCredential = sessionStorage.getItem(OPERATOR_KEY) || ''; } catch (e) { operatorCredential = ''; }
let operatorPrompt = null;

function operatorToken() { return operatorCredential; }

// One owner for gated-plane calls: attach the stored credential, and on 401
// prompt once, remember the entered value and retry the identical request
// exactly once. A 403 means the server has no credential configured
// (MILLIVOLT_OPERATOR_TOKEN); prompting cannot help, so the response is
// returned for the caller's error surface.
async function operatorFetch(url, options = {}) {
  const attempt = () => {
    const headers = Object.assign({}, options.headers || {});
    if (operatorCredential) headers['Authorization'] = 'Bearer ' + operatorCredential;
    return fetch(url, Object.assign({}, options, { headers }));
  };
  let response = await attempt();
  if (response.status !== 401) return response;
  const token = await askOperatorToken();
  if (!token) return response;
  operatorCredential = token;
  try { sessionStorage.setItem(OPERATOR_KEY, token); } catch (e) { /* storage denied: tab-only credential */ }
  response = await attempt();
  if (response.status === 401) {
    operatorCredential = '';
    try { sessionStorage.removeItem(OPERATOR_KEY); } catch (e) { /* already tab-only */ }
  }
  return response;
}

// Single-flight credential prompt: concurrent gated calls share one dialog
// and one resolution. Resolves '' on cancel or dismiss.
function askOperatorToken() {
  if (operatorPrompt) return operatorPrompt;
  operatorPrompt = new Promise(resolve => {
    let dialog = $('operator-dialog');
    if (!dialog) {
      dialog = document.createElement('section');
      dialog.id = 'operator-dialog';
      dialog.className = 'operator-dialog';
      dialog.hidden = true;
      dialog.tabIndex = -1;
      dialog.setAttribute('inert', '');
      dialog.setAttribute('role', 'dialog');
      dialog.setAttribute('aria-modal', 'true');
      dialog.setAttribute('aria-labelledby', 'operator-dialog-title');
      dialog.innerHTML = `<div class="operator-dialog-panel"><div class="operator-dialog-brand"><div class="brand-logo" aria-hidden="true"><svg width="18" height="18" viewBox="0 0 18 18" fill="none"><g stroke="var(--scale-tick)" stroke-width="1" opacity=".55" stroke-linecap="round"><path d="M2 13.5v1.6M5.5 13.5v1.6M9 13.5v1.6M12.5 13.5v1.6M16 13.5v1.6"/></g><path d="M1.5 12.8H16.5" stroke="var(--scale-base)" stroke-width="1" opacity=".5" stroke-linecap="round"/><path d="M1.5 9.5 4 9.5 5.6 4.6 8 13.2 10.4 6.8 12.4 9.5 16.5 9.5" stroke="url(#mv-login)" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"/><defs><linearGradient id="mv-login" x1="1.5" y1="9" x2="16.5" y2="9" gradientUnits="userSpaceOnUse"><stop stop-color="var(--accent)"/><stop offset="1" stop-color="var(--accent2)"/></linearGradient></defs></svg></div><h3 id="operator-dialog-title">millivolt</h3></div><p class="operator-dialog-note">This dashboard is protected. Enter the MILLIVOLT_OPERATOR_TOKEN value. It stays in this browser tab for the session.</p><form id="operator-dialog-form"><input id="operator-dialog-input" type="password" autocomplete="current-password" spellcheck="false" aria-label="Operator token" placeholder="operator token"><div class="operator-dialog-actions"><button class="btn" type="button" data-operator-auth="cancel">Cancel</button><button class="btn btn-accent" type="submit">Sign in</button></div></form></div>`;
      document.body.appendChild(dialog);
    }
    const input = $('operator-dialog-input');
    const finish = value => {
      input.value = '';
      closeModal(dialog);
      resolve(value);
    };
    dialog.onclick = e => { if (e.target === dialog) finish(''); };
    dialog.onkeydown = e => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); finish(''); } };
    dialog.querySelector('[data-operator-auth="cancel"]').onclick = () => finish('');
    $('operator-dialog-form').onsubmit = e => {
      e.preventDefault();
      finish(input.value.trim());
    };
    closeHeaderMenus();
    closeNavMenu();
    closeDimMenu();
    openModal(dialog);
    input.focus();
  }).finally(() => { operatorPrompt = null; });
}

// All operator mutations share confirmation, busy and issue-order gates.
const operatorState = {
  pause: { revision: 0, busy: false, menu: 'pause-menu', count: 'pause-count', valid: st => st?.ok === true && Array.isArray(st.holds), apply: applyPauseState, sync: syncPauseMenuState },
  debug: { revision: 0, busy: false, menu: 'debug-menu', count: 'debug-count', valid: st => st?.ok === true && Array.isArray(st.sessions), apply: applyDebugState, sync: syncDebugMenuState },
  throttle: { revision: 0, busy: false, menu: 'limits-menu', count: 'lim-count', valid: st => st?.ok === true && Array.isArray(st.throttles), apply: applyThrottleState, sync: syncLimitsMenuState },
};
document.addEventListener('click', e => {
  const button = e.target.closest('[data-operator]');
  if (!button || button.disabled) return;
  const actions = { 'pause-edit': editPauseHold, 'pause-resume': resumePauseHold,
    'debug-edit': editDebugSession, 'debug-stop': stopDebugSession, 'limit-edit': editLimitProvider,
    'storm-open': openStormDetails, 'storm-close': closeStormDetails };
  const action = actions[button.dataset.operator];
  if (action) { e.stopPropagation(); action(button.dataset.value); }
});
function captureOperatorRevisions() {
  return Object.fromEntries(Object.entries(operatorState).map(([key, st]) => [key, st.revision]));
}
function acceptOperator(kind, st, revision) {
  const gate = operatorState[kind];
  return !gate.busy && revision === gate.revision && gate.valid(st);
}
function operatorError(kind, message) {
  const el = $(operatorState[kind].count);
  if (el) { el.textContent = message; el.dataset.err = '1'; }
}
async function mutateOperator(kind, body, after) {
  const gate = operatorState[kind];
  if (gate.busy) return;
  gate.busy = true;
  ++gate.revision;
  const count = $(gate.count);
  if (count) delete count.dataset.err;
  gate.sync();
  let confirmed;
  try {
    const response = await operatorFetch('/admin/' + kind, {
      method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify(body),
    });
    const st = await response.json();
    if (!response.ok || !gate.valid(st)) throw new Error(st?.error || 'could not change ' + kind);
    confirmed = st;
  } catch (err) {
    operatorError(kind, String(err.message || 'could not change ' + kind));
  } finally {
    gate.busy = false;
    ++gate.revision;
    if (confirmed) {
      gate.apply(confirmed);
      if (after) after();
      if (confirmed.warning) {
        operatorError(kind, confirmed.warning);
        $(gate.menu).hidden = false;
        syncHdrMenuExpanded();
      }
    }
    gate.sync();
    refreshFooterState();
  }
}

function applyPauseState(st, revision = operatorState.pause.revision) {
  if (!acceptOperator('pause', st, revision)) return;
  pauseState = {
    paused: !!st.paused,
    clients: Array.isArray(st.clients) ? st.clients : [],
    providers: Array.isArray(st.providers) ? st.providers : [],
    holds: Array.isArray(st.holds) ? st.holds : [],
    known_clients: Array.isArray(st.known_clients) ? st.known_clients : [],
    known_providers: Array.isArray(st.known_providers) ? st.known_providers : [],
    until: st.until || null,
    queued: typeof st.queued === 'number' ? st.queued : 0,
    default_max_queued: typeof st.default_max_queued === 'number' ? st.default_max_queued : 0,
  };
  refreshFooterState();
  const menu = $('pause-menu');
  if (menu && !menu.hidden) syncPauseMenuState();
  if (lastData) scheduleRenderLive();
}

function applyThrottleState(st, revision = operatorState.throttle.revision) {
  if (!acceptOperator('throttle', st, revision)) return;
  throttleState = {
    throttles: Array.isArray(st.throttles) ? st.throttles : [],
    known_providers: Array.isArray(st.known_providers) ? st.known_providers : [],
    active: !!st.active,
  };
  const btn = $('btn-limits');
  if (btn) btn.setAttribute('aria-pressed', throttleState.active ? 'true' : 'false');
  const menu = $('limits-menu');
  if (menu && !menu.hidden) syncLimitsMenuState();
  if (lastData) scheduleRenderLive();
}

const HEADER_MENUS = { 'btn-pause': 'pause-menu', 'btn-debug': 'debug-menu', 'btn-limits': 'limits-menu', 'btn-logs': 'logs-menu', 'btn-clear': 'clear-menu', 'btn-restart': 'restart-menu' };
function closeHeaderMenus(except, restoreFocus = false) {
  if (except && except !== 'settings') closeSettings(true);
  let closed = false;
  for (const [button, id] of Object.entries(HEADER_MENUS)) {
    const el = $(id);
    if (el && !el.hidden && id !== except) {
      el.hidden = true;
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

function closeNavMenu(restoreFocus = false) {
  const nav = $('hdr-actions'), btn = $('btn-nav');
  if (!nav?.classList.contains('is-open')) return false;
  nav.classList.remove('is-open');
  btn?.setAttribute('aria-expanded', 'false');
  if (restoreFocus) btn?.focus();
  return true;
}
function toggleNavMenu(e) {
  e.stopPropagation();
  const nav = $('hdr-actions'), btn = $('btn-nav');
  if (!nav || !btn) return;
  const open = !nav.classList.contains('is-open');
  if (open) closeHeaderMenus();
  nav.classList.toggle('is-open', open);
  btn.setAttribute('aria-expanded', open ? 'true' : 'false');
}
function hideHdrMenu(id) {
  const el = $(id);
  if (el) el.hidden = true;
  syncHdrMenuExpanded();
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
  if (v.history_size != null) dashCfg.history_size = +v.history_size;
  if (v.dash_log_rows != null) dashCfg.log_rows = +v.dash_log_rows;
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
  // Re-arm only on an actual cadence change - the 5s tick calls this with an
  // unchanged config, and clearing/setting the interval every tick would
  // starve it (a tick younger than the interval never fires).
  if (typeof armDashboardTicks === 'function' && cadence() !== before) armDashboardTicks();
}

let settingsCat = '';
let settingsSnap = '';
let settingsQ = '';
let settingsReq = 0; // newest settings request owns state; saves invalidate older GETs
let backupConfigMode = 'replace';
let backupDatabaseMode = 'replace';
let backupInspect = null; // {file, data} after a validated inspect, before apply

const SETTINGS_CAT_REF = {
  server:       { dim: 'status' },
  request:      { dim: 'request' },
  upstream:     { dim: 'provider' },
  queue:        { dim: null, icon: '↻', color: 'var(--warn)' },
  storm:        { dim: null, icon: '⚠', color: 'var(--warn)' },
  conversation: { dim: 'conversation' },
  format:       { dim: 'model' },
  storage:    { dim: 'key' },
  backup:     { dim: null, icon: '⬇', color: 'var(--accent)' },
  dashboard:  { dim: 'time' },
  models:     { dim: 'model' },
  providers:  { dim: 'provider' },
};

function settingsCatVisual(id) {
  const r = SETTINGS_CAT_REF[id] || SETTINGS_CAT_REF.server;
  if (r.dim && ENTITY_TYPES[r.dim]) {
    return { icon: ENTITY_TYPES[r.dim].icon, color: ENTITY_TYPES[r.dim].color };
  }
  return { icon: r.icon, color: r.color || ENTITY_TYPES.status.color };
}

function settingsIsOpen() {
  const s = $('settings-sheet');
  return !!(s && !s.hidden);
}

function settingsIsDirty() {
  return !!(settingsDoc && settingsDoc.writable && settingsSnap && settingsFingerprint() !== settingsSnap);
}

function settingsDirtyCount() {
  if (!settingsSnap) return 0;
  try {
    const a = JSON.parse(settingsSnap);
    const b = collectSettingsValues();
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    let n = 0;
    keys.forEach(k => {
      if (JSON.stringify(a[k]) !== JSON.stringify(b[k])) n++;
    });
    return n;
  } catch (e) { return settingsIsDirty() ? 1 : 0; }
}

function settingsStatus(text, kind) {
  const el = $('settings-count');
  if (!el) return;
  el.textContent = text || '';
  el.classList.toggle('ok', kind === 'ok');
}

function settingsRestartNotice(doc) {
  const keys = doc?.restart_required || [];
  return keys.length ? 'restart needed for: ' + keys.join(', ') : '';
}

function closeSettings(force) {
  if (!force && settingsIsOpen() && settingsIsDirty()) {
    const n = settingsDirtyCount();
    settingsStatus((n === 1 ? '1 unsaved' : n + ' unsaved') + ' - apply or revert first');
    const btn = $('btn-settings-apply');
    if (btn && !btn.disabled) btn.focus();
    return;
  }
  const s = $('settings-sheet');
  const v = $('settings-veil');
  const b = $('btn-settings');
  if (s) { s.classList.remove('open'); closeModal(s); }
  if (v) v.classList.remove('open');
  if (b) b.setAttribute('aria-pressed', 'false');
}

function toggleSettings(e) {
  if (e) e.stopPropagation();
  if (settingsIsOpen()) { closeSettings(); return; }
  closeHeaderMenus('settings');
  closeNavMenu();
  if (typeof closeDrawer === 'function') closeDrawer();
  const s = $('settings-sheet');
  const v = $('settings-veil');
  const b = $('btn-settings');
  if (!s) return;
  openModal(s);
  if (v) v.classList.add('open');
  if (b) b.setAttribute('aria-pressed', 'true');
  requestAnimationFrame(() => s.classList.add('open'));
  const q = $('settings-q');
  if (q) q.value = '';
  settingsQ = '';
  fetchSettings().then(() => {
    const focus = $('settings-q');
    if (focus && settingsIsOpen()) focus.focus();
  });
}

function fetchSettings(discard = false) {
  const req = ++settingsReq;
  const draft = settingsFingerprint();
  return operatorFetch('/admin/config')
    .then(async r => {
      const doc = await r.json();
      if (!r.ok || !Array.isArray(doc.fields) || !doc.values || !doc.revision) throw new Error(doc.error || 'could not load config');
      return doc;
    })
    .then(doc => {
      if (req !== settingsReq) return null;
      applyDashValues(doc.effective || doc.values);
      const saving = $('btn-settings-apply')?.dataset.busy;
      // Restart/background refreshes never replace a draft. Explicit Revert
      // may discard only the draft it started with, not subsequent typing.
      if (!saving && (!settingsIsDirty() || (discard && draft === settingsFingerprint()))) {
        settingsDoc = doc;
        if (settingsIsOpen()) fillSettingsForm(doc);
      }
      return doc;
    })
    .catch(err => {
      if (req === settingsReq && settingsIsOpen()) settingsStatus(String(err.message || err));
      return null;
    });
}

function fillSettingsForm(doc) {
  const box = $('settings-fields');
  const rail = $('settings-rail');
  const hd = $('settings-pane-hd');
  if (!box || !rail) return;
  if (!doc || !doc.fields) {
    box.innerHTML = '';
    rail.innerHTML = '';
    if (hd) hd.innerHTML = '';
    fillSettingsLive(null);
    settingsStatus('could not load config');
    updateSettingsActions();
    return;
  }
  settingsDoc = doc;
  const values = doc.values || {};
  const defaults = doc.defaults || {};
  const overrides = doc.overrides || {};
  const cats = (doc.categories || []).filter(c => doc.fields.some(f => f.category === c.id));
  if (!settingsCat || !cats.some(c => c.id === settingsCat)) settingsCat = (cats[0] && cats[0].id) || '';
  rail.innerHTML = cats.map(c => {
    const n = doc.fields.filter(f => f.category === c.id).length;
    const vis = settingsCatVisual(c.id);
    const on = c.id === settingsCat ? ' active' : '';
    const ref = SETTINGS_CAT_REF[c.id] || SETTINGS_CAT_REF.server;
    const badge = ref.dim
      ? entityBadge(ref.dim, '', c.label)
      : `<span class="ent" style="--ent:${vis.color}"><span class="ent-ic" aria-hidden="true">${vis.icon}</span><span class="ent-lb">${escapeHtml(c.label)}</span></span>`;
    return `<button type="button" class="st-rail-item${on}" style="--ent:${vis.color}" data-st-cat="${escapeHtml(c.id)}" aria-current="${c.id === settingsCat ? 'page' : 'false'}">${badge}<span class="rail-n">${n}</span></button>`;
  }).join('');
  box.innerHTML = doc.fields.map(f => settingsFieldHTML(f, values[f.key], defaults[f.key], overrides[f.key])).join('') + settingsBackupHTML(doc);
  const backupFile = $('backup-file');
  if (backupFile) backupFile.addEventListener('change', runBackupRestore);
  syncProvMenuList(box.querySelector('.st-row[data-key="providers"]'));
  // Initial paint for every rules editor: validation states, preview bench,
  // rule count - the same pass the delegated events run on every edit.
  box.querySelectorAll('.mr-wrap').forEach(wrap => { validateModelRulesDraft(wrap); mrPreview(wrap); });
  fillSettingsLive(doc);
  settingsStatus(doc.writable ? settingsRestartNotice(doc) : 'in-memory only - start with -config to persist');
  settingsSnap = settingsFingerprint();
  showSettingsCat();
  updateSettingsActions();
}

function backupIncludeOn(id, enabled) {
  if (!enabled) return false;
  const el = $(id);
  return el ? el.checked : true;
}

function settingsBackupHTML(doc) {
  const b = doc.backup || {};
  const canCfg = !!b.config;
  const canDb = !!b.database;
  const restoring = !!(backupInspect && backupInspect.data);
  const cfgOn = backupIncludeOn('backup-include-config', canCfg);
  const dbOn = backupIncludeOn('backup-include-database', canDb);
  const pending = b.pending_database
    ? '<span class="st-hint">database restore is staged; restart to apply</span>'
    : '';
  const members =
    `<label class="st-check"><input type="checkbox" id="backup-include-config"${canCfg ? '' : ' disabled'}${cfgOn ? ' checked' : ''}> config</label>` +
    `<label class="st-check"><input type="checkbox" id="backup-include-database"${canDb ? '' : ' disabled'}${dbOn ? ' checked' : ''}> database</label>`;
  const file = `<input type="file" id="backup-file" accept=".mvb,application/octet-stream" hidden>`;
  let extra;
  if (restoring) {
    extra = backupRestoreControlsHTML() + backupPreviewHTML(doc) +
      `<div class="st-backup-actions">` +
      `<button type="button" class="btn btn-accent" id="btn-backup-apply">Apply</button>` +
      `<button type="button" class="btn" id="btn-backup-cancel">Cancel</button></div>` + file;
  } else {
    extra =
      `<div class="st-backup-actions">` +
      `<button type="button" class="btn" id="btn-backup-download">Download</button>` +
      `<button type="button" class="btn" id="btn-backup-restore">Restore</button></div>` +
      file +
      `<span class="st-hint">restore validates the archive before it is applied.</span>`;
  }
  return `<div class="st-row st-block st-backup" data-cat="backup" data-label="backup download restore config database archive merge replace" data-help="download or restore a self-checked archive of the saved config and sqlite history">
    <div class="st-name">Archive</div>
    <div class="st-ctl">${members}${pending}${extra}</div>
  </div>`;
}

function backupRestoreControlsHTML() {
  const j = backupInspect && backupInspect.data || {};
  const cfg = j.config || {};
  const db = j.database || {};
  const wantCfg = backupWantConfig();
  const wantDb = backupWantDatabase();
  const cfgReplace = backupConfigMode !== 'merge' ? ' checked' : '';
  const cfgMerge = backupConfigMode === 'merge' ? ' checked' : '';
  const dbReplace = backupDatabaseMode !== 'merge' ? ' checked' : '';
  const dbMerge = backupDatabaseMode === 'merge' ? ' checked' : '';
  let html = '';
  if (wantCfg && cfg.present) {
    html += `<span class="st-hint">config</span>` +
      `<label class="st-check"><input type="radio" name="backup-config-mode" value="replace"${cfgReplace}> replace all</label>` +
      `<label class="st-check"><input type="radio" name="backup-config-mode" value="merge"${cfgMerge}> merge modified</label>`;
  }
  if (wantDb && db.present) {
    html += `<span class="st-hint">database</span>` +
      `<label class="st-check"><input type="radio" name="backup-database-mode" value="replace"${dbReplace}> replace (restart)</label>` +
      `<label class="st-check"><input type="radio" name="backup-database-mode" value="merge"${dbMerge}> merge new ids</label>`;
  }
  return html;
}

function backupWantConfig() {
  return !!($('backup-include-config') && $('backup-include-config').checked);
}

function backupWantDatabase() {
  return !!($('backup-include-database') && $('backup-include-database').checked);
}

function backupQuery(forRestore = false) {
  const q = new URLSearchParams();
  if (backupWantConfig()) q.set('config', '1');
  if (backupWantDatabase()) q.set('database', '1');
  if (forRestore) {
    if (backupWantConfig()) q.set('config_mode', backupConfigMode === 'merge' ? 'merge' : 'replace');
    if (backupWantDatabase()) q.set('database_mode', backupDatabaseMode === 'merge' ? 'merge' : 'replace');
  }
  return q;
}

function backupScalar(v) {
  if (v == null) return '—';
  if (typeof v === 'boolean' || typeof v === 'number') return String(v);
  if (typeof v === 'string') return v;
  if (Array.isArray(v)) return v.length ? v.length + ' items' : '[]';
  if (typeof v === 'object') {
    const n = Object.keys(v).length;
    return n ? n + ' entries' : '{}';
  }
  return String(v);
}

function backupPreviewHTML(doc) {
  if (!backupInspect || !backupInspect.data) return '';
  const j = backupInspect.data;
  const fields = (doc && doc.fields) || (settingsDoc && settingsDoc.fields) || [];
  const byKey = {};
  for (const f of fields) byKey[f.key] = f;
  const defaults = (doc && doc.defaults) || (settingsDoc && settingsDoc.defaults) || {};
  const live = (doc && doc.values) || (settingsDoc && settingsDoc.values) || {};
  const cfg = j.config || {};
  const db = j.database || {};
  const wantCfg = backupWantConfig();
  const wantDb = backupWantDatabase();
  let body = '';
  if (wantCfg && cfg.present) {
    const modified = Array.isArray(cfg.modified) ? cfg.modified : [];
    const vsLive = new Set(Array.isArray(cfg.vs_live) ? cfg.vs_live : []);
    const values = cfg.values || {};
    if (!modified.length) {
      body += '<span class="st-hint">every setting in this backup is still the built-in default</span>';
    } else {
      for (const key of modified) {
        const label = (byKey[key] && byKey[key].label) || key;
        let hint = 'default ' + backupScalar(defaults[key]);
        hint += vsLive.has(key) ? ' · live ' + backupScalar(live[key]) : ' · same as live';
        body += `<div class="st-live-row"><span class="k">${escapeHtml(label)}</span><span class="v" title="${escapeHtml(backupScalar(values[key]))}">${escapeHtml(backupScalar(values[key]))}</span></div>` +
          `<span class="st-hint">${escapeHtml(hint)}</span>`;
      }
    }
  } else if (wantCfg) {
    body += '<span class="st-hint">archive has no config member</span>';
  }
  if (wantDb && db.present) {
    const overlap = db.overlap == null ? null : Number(db.overlap);
    const reqs = Number(db.requests) || 0;
    let extra = reqs + ' requests';
    if (overlap != null) extra += ' · ' + overlap + ' already on this store';
    if (backupDatabaseMode === 'merge' && overlap != null) extra += ' · merge would add ' + Math.max(0, reqs - overlap);
    else extra += ' · replace waits for restart';
    body += `<span class="st-hint">database · ${escapeHtml(extra)}</span>`;
  } else if (wantDb) {
    body += '<span class="st-hint">archive has no database member</span>';
  }
  return body ? `<div id="backup-preview">${body}</div>` : '';
}

function runBackupDownload() {
  const q = backupQuery();
  if (![...q.keys()].length) { settingsStatus('select config, database, or both'); return; }
  settingsStatus('building backup');
  operatorFetch('/admin/backup?' + q).then(async r => {
    if (!r.ok) {
      let msg = 'backup failed';
      try { msg = (await r.json()).error || msg; } catch (e) {}
      throw new Error(msg);
    }
    const blob = await r.blob();
    const dispo = r.headers.get('Content-Disposition') || '';
    const m = /filename="([^"]+)"/.exec(dispo);
    const a = document.createElement('a');
    a.href = URL.createObjectURL(blob);
    a.download = (m && m[1]) || 'millivolt-backup.mvb';
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(a.href);
    settingsStatus('backup downloaded', 'ok');
  }).catch(err => settingsStatus(String(err.message || err)));
}

function runBackupRestore(ev) {
  const input = ev && ev.target && ev.target.files ? ev.target : $('backup-file');
  const file = input && input.files && input.files[0];
  if (!file) return;
  const q = backupQuery(true);
  if (!q.get('config') && !q.get('database')) { settingsStatus('select config, database, or both'); input.value = ''; return; }
  q.set('inspect', '1');
  settingsStatus('checking backup');
  operatorFetch('/admin/restore?' + q, {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: file,
  }).then(async r => {
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || 'restore failed');
    backupInspect = { file, data: j };
    paintBackupPane();
    settingsStatus('archive checked — review then apply', 'ok');
  }).catch(err => settingsStatus(String(err.message || err)))
    .finally(() => { if (input) input.value = ''; });
}

function paintBackupPane() {
  const row = document.querySelector('.st-backup');
  if (!row || !settingsDoc) return;
  const wrap = document.createElement('div');
  wrap.innerHTML = settingsBackupHTML(settingsDoc);
  const next = wrap.firstElementChild;
  if (!next) return;
  row.replaceWith(next);
  const backupFile = $('backup-file');
  if (backupFile) backupFile.addEventListener('change', runBackupRestore);
}

function runBackupApply() {
  if (!backupInspect || !backupInspect.file) return;
  const q = backupQuery(true);
  if (!q.get('config') && !q.get('database')) { settingsStatus('select config, database, or both'); return; }
  settingsStatus('restoring');
  operatorFetch('/admin/restore?' + q, {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: backupInspect.file,
  }).then(async r => {
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || 'restore failed');
    backupInspect = null;
    const restart = Array.isArray(j.restart_required) && j.restart_required.length
      ? 'restart needed for: ' + j.restart_required.join(', ')
      : '';
    settingsStatus(restart ? 'restored · ' + restart : 'restored', restart ? '' : 'ok');
    return fetchSettings(true);
  }).catch(err => settingsStatus(String(err.message || err)));
}

function runBackupCancel() {
  backupInspect = null;
  paintBackupPane();
  settingsStatus('');
}

function fillSettingsLive(doc) {
  const el = $('settings-live');
  if (!el) return;
  if (!doc) { el.innerHTML = ''; return; }
  const eff = doc.effective || doc.values || {};
  const ovr = doc.overrides || {};
  const listen = ovr.listen || eff.listen || '';
  const db = ovr.db_path || eff.db_path || '';
  const persist = doc.writable ? 'yaml' : 'memory';
  el.innerHTML =
    `<div class="st-live-row"><span class="k">listen</span><span class="v" title="${escapeHtml(String(listen))}">${escapeHtml(String(listen))}</span></div>` +
    `<div class="st-live-row"><span class="k">store</span><span class="v" title="${escapeHtml(String(db))}">${escapeHtml(String(db || 'ring'))}</span></div>` +
    `<div class="st-live-row"><span class="k">write</span><span class="v">${escapeHtml(persist)}</span></div>`;
}

function settingsFieldHTML(f, val, def, override) {
  const locked = override != null && String(override) !== '';
  const dis = (locked ? ' disabled' : '') + ` aria-label="${escapeHtml(f.label)}"`;
  const pills = locked ? '<span class="st-pill lock">flag</span>' : '';
  let hint = '';
  if (f.zero_means && (val === 0 || val === '0' || (f.zero_token && val === f.zero_token))) hint = `<span class="st-hint">${escapeHtml(f.zero_means)}</span>`;
  else if (def != null && def !== '' && f.kind !== 'providers' && f.kind !== 'aliases' && f.kind !== 'strings' && String(val) === String(def)) {
    hint = `<span class="st-hint">default</span>`;
  } else if (def != null && def !== '' && f.kind !== 'providers' && f.kind !== 'aliases' && f.kind !== 'strings') {
    hint = `<span class="st-hint">default ${escapeHtml(String(def))}</span>`;
  }
  if (locked) hint += `<span class="st-hint">via flag · ${escapeHtml(String(override))}</span>`;
  let control = '';
  const unit = f.unit ? `<span class="st-unit">${escapeHtml(f.unit)}</span>` : '';
  const nameExtra = f.kind === 'providers' ? providersAddHTML() : '';
  if (f.kind === 'bool') {
    const on = val === true || val === 'true';
    control = `<input type="checkbox" class="st-switch" data-key="${escapeHtml(f.key)}"${on ? ' checked' : ''}${dis}>`;
  } else if (f.kind === 'strings') {
    const text = Array.isArray(val) ? val.join('\n') : (val || '');
    control = `<textarea data-key="${escapeHtml(f.key)}" rows="3" placeholder="one per line"${dis}>${escapeHtml(text)}</textarea>`;
  } else if (f.kind === 'providers') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="providers">${providersEditorHTML(val)}</div>`;
  } else if (f.kind === 'aliases') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="aliases">${aliasesEditorHTML(val)}</div>`;
  } else if (f.kind === 'model_rules') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="model_rules">${modelRulesEditorHTML(val)}</div>`;
  } else if (f.kind === 'int') {
    const min = f.min != null ? ` min="${f.min}"` : '';
    const max = f.max != null ? ` max="${f.max}"` : '';
    control = `<div class="st-ctl-line"><input type="number" data-key="${escapeHtml(f.key)}" value="${escapeHtml(String(val ?? ''))}" step="1" required${min}${max}${dis}>${unit}</div>`;
  } else {
    control = `<div class="st-ctl-line"><input type="text" data-key="${escapeHtml(f.key)}" value="${escapeHtml(val == null ? '' : String(val))}"${dis}>${unit}</div>`;
  }
  const block = (f.kind === 'providers' || f.kind === 'aliases' || f.kind === 'strings' || f.kind === 'model_rules') ? ' st-block' : '';
  const ttl = escapeHtml((f.help || '') + (f.help && f.key ? ' · ' : '') + (f.key || ''));
  const hot = f.hot_reload ? '1' : '0';
  return `<div class="st-row${block}" data-cat="${escapeHtml(f.category)}" data-key="${escapeHtml(f.key)}" data-label="${escapeHtml(f.label)}" data-help="${escapeHtml(f.help || '')}" data-hot="${hot}" title="${ttl}"><div class="st-name">${escapeHtml(f.label)}${pills}${nameExtra}</div><div class="st-ctl">${control}${hint}</div></div>`;
}

// ---- providers editor ----
// Field-name maps per provider: cost keys are JSON paths from the response
// root (chips), usage keys map a canonical token field to a custom path
// (canonical dropdown - the list is served by /admin/config usage_fields,
// owned by metrics.CanonicalUsageFields). Provider labels are the registrable
// domain of the upstream base URL; known labels from the pause API seed the
// suggestions. All events are delegated on #settings-fields (the sheet's
// innerHTML is rebuilt on every open/refetch, so per-element listeners would
// not survive a re-render).

// Dotted JSON paths only: what digJSON can actually resolve (object segments,
// numeric array segments). Deny anything else at the input.
const PROV_PATH_RE = /^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$/;
// RFC 7230 header token - the same shape the server's config validation
// enforces on providers.<label>.headers names.
const PROV_HEADER_RE = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
const USAGE_FIELDS_FALLBACK = ['input_tokens', 'output_tokens', 'total_tokens', 'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens'];
const MODEL_FIELDS_FALLBACK = ['context_length', 'context_window', 'max_output_tokens', 'input_modalities', 'output_modalities'];

function canonicalUsageFields() {
  const f = settingsDoc && settingsDoc.usage_fields;
  return Array.isArray(f) && f.length ? f : USAGE_FIELDS_FALLBACK;
}

function canonicalModelFields() {
  const f = settingsDoc && settingsDoc.model_fields;
  return Array.isArray(f) && f.length ? f : MODEL_FIELDS_FALLBACK;
}

function costChipHTML(path) {
  return `<span class="prov-chip" data-cost="${escapeHtml(path)}"><span class="prov-chip-p">${escapeHtml(path)}</span><button type="button" class="prov-x" data-prov-chip-rm aria-label="remove ${escapeHtml(path)}">✕</button></span>`;
}

const PROV_TRASH_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.2 5.4h9.6"/><path d="M6.1 5.4V4.3A1.2 1.2 0 0 1 7.3 3.1h1.4A1.2 1.2 0 0 1 9.9 4.3v1.1"/><path d="M4.2 5.4l.7 7.4a1 1 0 0 0 1 .8h5.2a1 1 0 0 0 1-.8l.7-7.4"/></svg>';
const PROV_CHEV_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.2 8 10.2 12 6.2"/></svg>';

function usageRowHTML(field, path, fields) {
  const opts = settingsFieldOptions(fields, field);
  return `<div class="prov-urow"><select class="sp-ufield" aria-label="canonical token field">${opts}</select><span class="prov-arrow" aria-hidden="true">→</span><input class="sp-upath" value="${escapeHtml(path == null ? '' : String(path))}" placeholder="usage-relative path" aria-label="usage key path"><button type="button" class="prov-x" data-prov-urow-rm aria-label="remove mapping">✕</button></div>`;
}

function modelRowHTML(field, path, fields) {
  const opts = settingsFieldOptions(fields, field);
  return `<div class="prov-urow m-row"><select class="sp-mfield" aria-label="canonical model field">${opts}</select><span class="prov-arrow" aria-hidden="true">→</span><input class="sp-mkey" value="${escapeHtml(path == null ? '' : String(path))}" placeholder="path inside the enrichment entry" aria-label="model metadata path"><button type="button" class="prov-x" data-prov-mrow-rm aria-label="remove mapping">✕</button></div>`;
}

function settingsFieldOptions(fields, selected) {
  // Models enrichment can carry custom output keys. Existing names must
  // round-trip even when they are not offered as canonical new-row choices.
  const choices = fields.includes(selected) ? fields : fields.concat(selected);
  return choices.map(f => `<option value="${escapeHtml(f)}"${f === selected ? ' selected' : ''}>${escapeHtml(f)}</option>`).join('');
}

function providerCardHTML(label, p) {
  p = p || {};
  const costs = Array.isArray(p.cost_keys) ? p.cost_keys : [];
  const usage = p.usage_keys && typeof p.usage_keys === 'object' && !Array.isArray(p.usage_keys) ? p.usage_keys : {};
  const modelsPath = p.models_path == null ? '' : String(p.models_path);
  const modelsKeys = p.models_keys && typeof p.models_keys === 'object' && !Array.isArray(p.models_keys) ? p.models_keys : {};
  const headers = p.headers && typeof p.headers === 'object' && !Array.isArray(p.headers) ? p.headers : {};
  const fields = canonicalUsageFields();
  const used = new Set(Object.keys(usage));
  const free = fields.filter(f => !used.has(f));
  const mfields = canonicalModelFields();
  const mused = new Set(Object.keys(modelsKeys));
  const mfree = mfields.filter(f => !mused.has(f));
  // Section order is fixed: cost keys, then usage keys, then models
  // enrichment, then upstream headers last. Every section opens and closes
  // inside the card - one missing close here cascades the following sections
  // into each other (the models section once rendered nested inside usage
  // keys).
  return `<div class="st-prov">` +
    `<div class="prov-hd"><span class="prov-ic" aria-hidden="true">☁</span><input class="sp-label" value="${escapeHtml(label)}" placeholder="provider label - registrable domain of the base URL, e.g. nano-gpt.com" aria-label="provider label"><button type="button" class="prov-chev" data-prov-collapse aria-expanded="true" aria-label="collapse or expand ${escapeHtml(label)}" title="collapse / expand">${PROV_CHEV_SVG}</button><button type="button" class="prov-x" data-prov-rm aria-label="remove provider" title="remove provider">${PROV_TRASH_SVG}</button></div>` +
    `<div class="prov-body">` +
    `<div class="prov-sec"><div class="prov-lb"><span>cost keys</span><span class="prov-sub">dotted JSON path from the response root - e.g. usage.cost</span></div>` +
    `<div class="prov-chips">${costs.map(costChipHTML).join('')}</div>` +
    `<div class="prov-add"><input class="sp-cost-in" placeholder="add cost key path… ↵" aria-label="add cost key"><button type="button" class="btn prov-addbtn" data-prov-add-cost aria-label="add cost key">+</button></div></div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>usage keys</span><span class="prov-sub">canonical token field → custom path</span></div>` +
    `<div class="prov-umap">${Object.entries(usage).map(([f, path]) => usageRowHTML(f, path, fields)).join('')}</div>` +
    (free.length ? `<div class="prov-add prov-add-u"><select class="sp-ufield-new" aria-label="canonical token field">${free.map(f => `<option value="${escapeHtml(f)}">${escapeHtml(f)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-usage aria-label="add usage mapping">+ map</button></div>` : '') +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>models enrichment</span><span class="prov-sub">metadata endpoint merged into this provider's /v1/models list</span></div>` +
    `<div class="prov-add prov-add-mp"><input class="sp-mpath" value="${escapeHtml(modelsPath)}" placeholder="metadata path - e.g. /language-models" aria-label="models metadata path"></div>` +
    `<div class="prov-mmap">${Object.entries(modelsKeys).map(([f, path]) => modelRowHTML(f, path, mfields)).join('')}</div>` +
    (mfree.length ? `<div class="prov-add prov-add-m"><select class="sp-mfield-new" aria-label="canonical model field">${mfree.map(f => `<option value="${escapeHtml(f)}">${escapeHtml(f)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-model aria-label="add model mapping">+ map</button></div>` : '') +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>upstream headers</span><span class="prov-sub">extra headers sent upstream - {{uuid4}} / {{platform}} expand per request</span></div>` +
    `<div class="prov-hmap">${Object.entries(headers).map(([n, v]) => headerRowHTML(n, v)).join('')}</div>` +
    `<div class="prov-add prov-add-h"><input class="sp-hname" placeholder="header name… ↵" aria-label="upstream header name"><input class="sp-hval" placeholder="header value… ↵" aria-label="upstream header value"><button type="button" class="btn prov-addbtn" data-prov-add-header aria-label="add header">+</button></div>` +
    `</div>` +
    `</div>` +
    `</div>`;
}

function headerRowHTML(name, value) {
  return `<div class="prov-urow prov-hrow"><input class="sp-hname" value="${escapeHtml(name == null ? '' : String(name))}" placeholder="Header-Name" aria-label="upstream header name"><input class="sp-hval" value="${escapeHtml(value == null ? '' : String(value))}" placeholder="value - {{uuid4}} / {{platform}} expand" aria-label="upstream header value"><button type="button" class="prov-x" data-prov-hrow-rm aria-label="remove header">✕</button></div>`;
}

function providersEditorHTML(val) {
  const m = val && typeof val === 'object' && !Array.isArray(val) ? val : {};
  return Object.keys(m).map(k => providerCardHTML(k, m[k])).join('');
}

// ---- aliases editor ----
// provider_aliases: rows of [old label] → [canonical label]. Reuses the
// usage-row layout (.prov-urow) so both structured maps share one visual
// grammar. All events ride the same delegated #settings-fields wiring.
function aliasRowHTML(from, to) {
  return `<div class="prov-urow al-row"><input class="al-from" value="${escapeHtml(from)}" placeholder="old label - e.g. host.example.com" aria-label="old provider label"><span class="prov-arrow" aria-hidden="true">→</span><input class="al-to" value="${escapeHtml(to == null ? '' : String(to))}" placeholder="canonical label - e.g. example.com" aria-label="canonical provider label"><button type="button" class="prov-x" data-al-rm aria-label="remove alias">✕</button></div>`;
}

function aliasesEditorHTML(val) {
  const m = val && typeof val === 'object' && !Array.isArray(val) ? val : {};
  const rows = Object.keys(m).sort().map(k => aliasRowHTML(k, m[k])).join('');
  // .prov-sec wrapper imports the provider-editor visual system for the
  // inputs (mono, rail background, 7px radius, accent focus, dashed
  // add-row affordance) - without it the bare inputs fall back to browser
  // defaults and look nothing like the rest of the sheet.
  return `<div class="prov-sec">` +
    `<div class="al-rows">${rows}</div>` +
    `<div class="prov-add"><input class="al-new-from" placeholder="old label… ↵" aria-label="old provider label"><span class="prov-arrow" aria-hidden="true">→</span><input class="al-new-to" placeholder="canonical label… ↵" aria-label="canonical provider label"><button type="button" class="btn prov-addbtn" data-al-add aria-label="add alias">+</button></div>` +
    `</div>`;
}

// ---- model rules editor ----
// model_rules: an ordered rewrite pipeline (exact | pattern | lower steps)
// merging spelling variants of the same model in every grouped surface.
// One visual grammar with the other structured editors (.prov-urow rows,
// dashed add-row). Affordances, each researched from how mature tools do
// rule builders (regex101 / Cloudflare / Grafana / Stripe):
//   - live per-keystroke validation with consequence-phrased inline errors
//     (validateMrRow below runs on every input; Apply is blocked while any
//     row is red - nothing invalid can reach the POST).
//   - an RE2-compatibility lint (re2Lint): the server compiles patterns
//     with Go's RE2, whose syntax differs from JavaScript RegExp -
//     lookarounds/backreferences/\u pass `new RegExp` but fail the server.
//     The lint flags those before the save, and marks server-only-but-
//     Go-valid syntax as info rows the browser preview skips honestly.
//   - a preview bench: the typed spelling's final result, a per-rule step
//     trace, and a before→after table over the real model spellings from
//     the request log (the backtest pattern - synthetic examples lie).
//   - templates + restore-defaults (the shipped pipeline as one click).
//   - ◉/◌ per-rule parking: keep a rule without applying it (A/B the
//     pipeline; parked rows ride the wire as {disabled: true}, skipped by
//     ValidateModelRules' body checks and CompileModelRules).
const MODEL_RULE_MODES = [['exact', 'exact'], ['pattern', 'pattern'], ['lower', 'lower']];
const MR_MODE_TITLES = { exact: 'exact - merge one whole spelling', pattern: 'pattern - regex replace-all (RE2 syntax, $1 refs)', lower: 'lower - lowercase fold' };
// Mirrors config.ModelRulesMax (64) - the count line and add-gate use it.
const MODEL_RULES_MAX = 64;
// The shipped pipeline, offered back as one-click templates + as the
// "default pipeline" restore (pure data - identical to DefaultModelRules).
const MR_DEFAULTS = [
  { mode: 'lower' },
  { mode: 'pattern', from: '^[a-z0-9][a-z0-9._-]*/', to: '' },
  { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' },
  { mode: 'pattern', from: '-(?:[a-z]{0,2}fp\\d+|bf\\d+|int\\d+|nf\\d+|[a-z]?q\\d+(?:_[0-9a-z]+)*)$', to: '' },
  { mode: 'pattern', from: '(\\d)\\.(\\d)', to: '$1-$2' },
];
const MR_TEMPLATES = [
  ['lowercase fold', { mode: 'lower' }],
  ['strip vendor/ prefix', { mode: 'pattern', from: '^[a-z0-9][a-z0-9._-]*/', to: '' }],
  ['strip :tag suffix', { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' }],
  ['strip architecture/quant suffix (fp4, nvfp4, int8, q4_k_m…)', { mode: 'pattern', from: '-(?:[a-z]{0,2}fp\\d+|bf\\d+|int\\d+|nf\\d+|[a-z]?q\\d+(?:_[0-9a-z]+)*)$', to: '' }],
  ['unify . and - between digits', { mode: 'pattern', from: '(\\d)\\.(\\d)', to: '$1-$2' }],
  ['exact merge…', { mode: 'exact' }],
  ['custom pattern…', { mode: 'pattern' }],
];

function mrModeSelectHTML(sel) {
  return `<select class="mr-mode" title="${MR_MODE_TITLES[sel] || MR_MODE_TITLES.exact}" aria-label="rule mode">` +
    MODEL_RULE_MODES.map(([v, l]) => `<option value="${l}"${v === sel ? ' selected' : ''}>${l}</option>`).join('') +
    `</select>`;
}

// re2Lint classifies one pattern against the server's regex engine (Go
// regexp - RE2 syntax, which is not JavaScript RegExp): 'error' means the
// server would reject the pattern (blocks Apply), 'info' means it is valid
// for the server but the browser cannot run it faithfully (the preview
// skips the row and says so), 'warn' means valid everywhere but with
// different semantics (drift the preview would otherwise hide). Token
// tables verified against Go's regexp/syntax parser and the RE2 syntax
// reference; only constructs that are certain are flagged - anything
// uncertain is left to the server's own Validate (its error text is
// surfaced on the POST).
function re2Lint(p) {
  // RE2 caps counted repeats at 1000; JS allows astronomically more.
  const rep = /\{(\d+)(?:,(\d*))?\}/.exec(p);
  if (rep && (+rep[1] > 1000 || (rep[2] && +rep[2] > 1000)))
    return { level: 'error', msg: 'RE2 caps repeats at 1000 - "' + rep[0] + '" would fail to load on the server' };
  const SRV_ONLY = 'server-only syntax - the browser preview skips this rule; it still applies once saved';
  let cls = false;
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === '\\') {
      const e = p[i + 1] || '';
      if (!e) break;
      // JS RegExp backreference - RE2 has none. Multi-digit octal forms
      // (\17) are escapes in both, so only flag when the next char cannot
      // continue an octal escape.
      if ('123456789'.includes(e) && !(p[i + 2] && '01234567'.includes(p[i + 2])))
        return { level: 'error', msg: 'backreferences (\\' + e + ') are not supported by the server\'s regex engine (RE2) - rewrite the rule without them' };
      // \h \k \R \X \K \G \e \N \Z: identity escapes in JS, hard errors in RE2.
      if ('hkRXKGeNZ'.includes(e))
        return { level: 'error', msg: '\\' + e + ' is not supported by the server\'s regex engine (RE2)' };
      if (e === 'c' && p[i + 2] && /[A-Za-z]/.test(p[i + 2]))
        return { level: 'error', msg: '\\c control escapes are not supported by RE2 - use \\xNN' };
      if (e === 'b' && cls)
        return { level: 'error', msg: 'RE2 has no \\b inside a character class - use \\x08 for a backspace byte' };
      if (e === 'u')
        return { level: 'error', msg: 'RE2 does not support \\u escapes - write the character itself or use \\xNN' };
      if (e === 'p' || e === 'P')
        return { level: 'info', msg: 'unicode classes are ' + SRV_ONLY };
      if (e === 'A' || e === 'z')
        return { level: 'info', msg: 'text anchors \\A/\\z are ' + SRV_ONLY + ' (the browser would read them as literal letters)' };
      if (e === 'Q')
        return { level: 'info', msg: '\\Q…\\E literal quoting is ' + SRV_ONLY };
      if (e === 'x' && p[i + 2] === '{')
        return { level: 'info', msg: '\\x{…} hex escapes are ' + SRV_ONLY };
      i++; // consume the escape pair atomically
      continue;
    }
    if (c === '[') {
      if (p.startsWith('[[:', i))
        return { level: 'info', msg: 'POSIX classes like [[:alpha:]] are ' + SRV_ONLY };
      cls = true;
      continue;
    }
    if (c === ']') { cls = false; continue; }
    if (c === '(' && p[i + 1] === '?') {
      if (p.startsWith('(?=', i) || p.startsWith('(?!', i))
        return { level: 'error', msg: 'lookahead is not supported by the server\'s regex engine (RE2) - rewrite with character classes and anchors' };
      if (p.startsWith('(?<=', i) || p.startsWith('(?<!', i))
        return { level: 'error', msg: 'lookbehind is not supported by RE2 - rewrite with character classes and anchors' };
      if (p.startsWith('(?P<', i))
        return { level: 'info', msg: 'named groups (?P<…>) are ' + SRV_ONLY };
      const fl = /^\(\?([imsU-]+)\)/.exec(p.slice(i));
      if (fl)
        return { level: 'info', msg: 'inline flags like ' + fl[0] + ' are ' + SRV_ONLY };
    }
  }
  // Valid in both engines, but with different character sets: the browser
  // preview would show matches the server never makes.
  if (/\\s/.test(p))
    return { level: 'warn', msg: '\\s matches more characters in the browser than on the server (RE2: ASCII [ \\t\\n\\f\\r ] only) - prefer an explicit class' };
  return null;
}

// validateMrRow is the live per-row gate: renders the inline state (error
// text under the row, red border on the offending input) and returns the
// level. `seen` (a Set of earlier enabled rows' mode+from keys) powers
// the never-runs duplicate warning; pass null to skip it.
function validateMrRow(row, seen) {
  const mode = row.querySelector('.mr-mode').value;
  const fromIn = row.querySelector('.mr-from');
  const toIn = row.querySelector('.mr-to');
  const err = row.querySelector('.mr-err');
  const from = String((fromIn || {}).value || '');
  let level = 'ok', msg = '';
  if (row.dataset.mrOff === '1') {
    level = 'off';
  } else if (mode === 'lower') {
    level = 'ok';
  } else if (!from.trim()) {
    level = 'error';
    msg = (mode === 'exact' ? 'needs a from spelling' : 'needs a from pattern') + ' - fill it in, or remove the row';
  } else if (mode === 'exact') {
    if (from !== from.trim()) { level = 'error'; msg = 'the from spelling has leading/trailing spaces - the rule compares the whole string and would never fire'; }
  } else {
    const lint = re2Lint(from);
    let jsErr = null;
    try { new RegExp(from); } catch (e) { jsErr = e; }
    if (lint && lint.level === 'error') { level = 'error'; msg = lint.msg; }
    else if (jsErr && lint && lint.level === 'info') { level = 'info'; msg = lint.msg; }
    else if (jsErr) { level = 'error'; msg = String(jsErr.message || jsErr) + ' - the server would reject this pattern too'; }
    else if (lint) { level = lint.level; msg = lint.msg; }
  }
  if (level === 'ok' && seen) {
    const key = mode === 'lower' ? '\u0000lower' : mode + '\u0000' + from;
    if (seen.has(key)) { level = 'warn'; msg = 'an earlier rule already does this - this one never runs'; }
    else seen.add(key);
  }
  row.dataset.mrState = level;
  if (err) { err.textContent = msg; err.dataset.level = (level === 'error' || level === 'warn') ? level : ''; }
  if (fromIn) fromIn.classList.toggle('prov-bad', level === 'error');
  return level;
}

// validateModelRulesDraft walks every row in order and returns the first
// hard error (Apply blocks on it; the button flow focuses and flashes it).
function validateModelRulesDraft(wrap) {
  const seen = new Set();
  let firstBad = null;
  wrap.querySelectorAll('.mr-row').forEach(row => {
    if (validateMrRow(row, seen) === 'error' && !firstBad) firstBad = row;
  });
  return { ok: !firstBad, firstBad };
}

function modelRuleRowHTML(r) {
  r = r || {};
  const mode = MODEL_RULE_MODES.some(([v]) => v === r.mode) ? r.mode : 'exact';
  const lower = mode === 'lower';
  const off = !!r.disabled;
  const dead = lower || off ? ' disabled' : '';
  return `<div class="prov-urow mr-row"${off ? ' data-mr-off="1"' : ''}>` +
    mrModeSelectHTML(mode).replace('<select ', `<select${off ? ' disabled' : ''} `) +
    `<input class="mr-from" value="${escapeHtml(r.from || '')}" placeholder="from - spelling (exact) or regex (pattern)"${dead} aria-label="from">` +
    `<span class="prov-arrow" aria-hidden="true">→</span>` +
    `<input class="mr-to" value="${escapeHtml(r.to || '')}" placeholder="to - empty removes the match"${dead} aria-label="to">` +
    `<span class="mr-move"><button type="button" class="mr-vis" data-mr-vis aria-pressed="${off}" title="${off ? 'un-park this rule' : 'park this rule - keep it without applying it'}">${off ? '◌' : '◉'}</button><button type="button" data-mr-up aria-label="move rule up" title="move up">▲</button><button type="button" data-mr-dn aria-label="move rule down" title="move down">▼</button></span>` +
    `<button type="button" class="prov-x" data-mr-rm aria-label="remove rule" title="remove">✕</button>` +
    `<div class="mr-err" aria-live="polite"></div>` +
    `</div>`;
}

function modelRulesEditorHTML(val) {
  const rules = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap">` +
    `<div class="mr-hint">rules apply top→bottom to each stored model name · exact merges a whole spelling · pattern is a regex replace-all (RE2, $1 refs) · lower folds case · ◉ parks a rule</div>` +
    `<div class="mr-tools">` +
    `<select class="mr-tpl" aria-label="add a rule from a template"><option value="">add from template…</option>${MR_TEMPLATES.map((t, i) => `<option value="${i}">${escapeHtml(t[0])}</option>`).join('')}</select>` +
    `<button type="button" class="mr-restore" data-mr-restore title="replace the draft with the shipped four-rule pipeline">↺ default pipeline</button>` +
    `<span class="mr-count muted"></span>` +
    `</div>` +
    `<div class="prov-urow mr-preview">` +
    `<span class="mr-what" title="Approximate exact/lower preview only; patterns execute on the server after saving.">preview*</span>` +
    `<input class="mr-test" placeholder="type any spelling - e.g. moonshotai/kimi-k3:nube" aria-label="test spelling">` +
    `<span class="prov-arrow" aria-hidden="true">→</span>` +
    `<span class="mr-out muted">…</span>` +
    `<span></span><span></span>` +
    `</div>` +
    `<div class="mr-trace" hidden></div>` +
    `<div class="mr-examples"></div>` +
    `<div class="mr-rows">${rules.map(modelRuleRowHTML).join('')}</div>` +
    `<div class="prov-add mr-add">` +
    mrModeSelectHTML('exact') +
    `<input class="mr-new-from" placeholder="from… (↵ to add)" aria-label="new rule from">` +
    `<span class="prov-arrow" aria-hidden="true">→</span>` +
    `<input class="mr-new-to" placeholder="to…" aria-label="new rule to">` +
    `<button type="button" class="btn prov-addbtn" data-mr-add aria-label="add rule">+</button>` +
    `</div>` +
    `</div>`;
}

// collectModelRules reads the editor's rows into the wire shape. Parked
// rows ride along as {disabled: true} (with their from/to kept, so
// un-parking restores them); incomplete enabled rows are dropped here for
// preview robustness - but the save gate (validateModelRulesDraft) blocks
// Apply on them first, so a dropped row can never silently vanish from a
// saved config. The server re-validates everything (mode vocabulary,
// pattern compilability, cap) - deny by default on both sides.
function collectModelRules(wrap) {
  const out = [];
  wrap.querySelectorAll('.mr-row').forEach(row => {
    const mode = row.querySelector('.mr-mode').value;
    const off = row.dataset.mrOff === '1';
    const from = String((row.querySelector('.mr-from') || {}).value || '');
    const to = String((row.querySelector('.mr-to') || {}).value || '');
    if (!from.trim() && !off && mode !== 'lower') return;
    const r = { mode };
    if (from || mode !== 'lower') r.from = from;
    if (to || mode !== 'lower') r.to = to;
    if (off) r.disabled = true;
    out.push(r);
  });
  return out;
}

// mrTrafficExamples gathers the top raw model spellings from the request
// log (live ring data - the real backtest the preview demonstrates on).
function mrTrafficExamples() {
  if (typeof lastData !== 'object' || !lastData || !Array.isArray(lastData.records)) return [];
  const counts = new Map();
  for (const r of lastData.records) {
    const m = String((r && r.model) || '').trim();
    if (m) counts.set(m, (counts.get(m) || 0) + 1);
  }
  return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
}

// mrDraftSteps builds the preview's compiled pipeline from the current
// editor rows: parked rows, rows the browser cannot run (info state), and
// rows that could never be saved (error state - the preview must not
// demonstrate behavior no valid config can have) are skipped - the preview
// is honestly partial when they exist.
function mrDraftSteps(wrap) {
  const steps = [];
  wrap.querySelectorAll('.mr-row').forEach(row => {
    const st = row.dataset.mrState;
    if (row.dataset.mrOff === '1' || st === 'info' || st === 'error') return;
    const mode = row.querySelector('.mr-mode').value;
    if (mode === 'lower') { steps.push({ label: 'lower', run: s => s.toLowerCase() }); return; }
    const from = String((row.querySelector('.mr-from') || {}).value || '');
    const to = String((row.querySelector('.mr-to') || {}).value || '');
    if (!from.trim()) return;
    if (mode === 'exact') { const f = from, t = to; steps.push({ label: f, run: s => s === f ? t : s }); return; }
    // Never execute RE2 patterns in a backtracking JavaScript engine.
    // Draft pattern outcomes are server-only; saved names use observer metadata.
  });
  return steps;
}

// mrRunSteps folds one spelling through the draft steps with the same
// empty-result fallback for the explicitly partial exact/lower preview.
function mrRunSteps(steps, s) {
  let cur = s;
  for (const st of steps) {
    const nxt = String(st.run(cur));
    if (!nxt) return { out: s, stripped: true };
    cur = nxt;
  }
  return { out: cur, stripped: false };
}

// mrSyncCount keeps the "N / 64 rules" line honest and gates the add
// controls at the cap (mirrors config.ModelRulesMax on the server).
function mrSyncCount(wrap) {
  const n = wrap.querySelectorAll('.mr-row').length;
  const count = wrap.querySelector('.mr-count');
  if (count) count.textContent = n + ' / ' + MODEL_RULES_MAX + ' rules';
  const cap = n >= MODEL_RULES_MAX;
  const add = wrap.querySelector('[data-mr-add]');
  if (add) add.disabled = cap;
  const tpl = wrap.querySelector('.mr-tpl');
  if (tpl) tpl.disabled = cap;
}

// mrPreview is the live bench: the typed spelling's final result, a
// per-rule step trace showing what each rule did, and the before→after
// table over the request log's real spellings.
function mrPreview(wrap) {
  const test = wrap.querySelector('.mr-test');
  const out = wrap.querySelector('.mr-out');
  if (!test || !out) return;
  const steps = mrDraftSteps(wrap);
  const s = String(test.value || '').trim();
  out.classList.toggle('muted', !s);
  if (!s) {
    out.textContent = '…';
    const trace = wrap.querySelector('.mr-trace');
    if (trace) trace.hidden = true;
  } else {
    const r = mrRunSteps(steps, s);
    out.textContent = r.out;
    const trace = wrap.querySelector('.mr-trace');
    if (trace) {
      let cur = s;
      const parts = [`<span class="mr-step-in">${escapeHtml(s)}</span>`];
      for (const st of steps) {
        const nxt = String(st.run(cur));
        if (!nxt) { parts.push(`<em class="mr-step-rule">stripped everything - original kept</em>`); cur = s; break; }
        if (nxt !== cur) {
          const lbl = st.label.length > 20 ? st.label.slice(0, 19) + '…' : st.label;
          parts.push(`<em class="mr-step-rule">${escapeHtml(lbl)}</em><span class="mr-step">${escapeHtml(nxt)}</span>`);
        }
        cur = nxt;
      }
      trace.innerHTML = parts.join('');
      trace.hidden = false;
    }
  }
  // real-traffic examples
  const exBox = wrap.querySelector('.mr-examples');
  if (exBox) {
    const exs = mrTrafficExamples();
    if (!exs.length) {
      exBox.innerHTML = `<div class="mr-ex-hd muted">no model names in the request log yet - type a test spelling above</div>`;
    } else {
      let changed = 0;
      const rows = exs.map(([m, n]) => {
        const r = mrRunSteps(steps, m);
        if (r.out !== m) changed++;
        return `<div class="mr-ex${r.out !== m ? ' ch' : ''}"><span class="mr-ex-in">${escapeHtml(m)}</span><span class="prov-arrow" aria-hidden="true">→</span><span class="mr-ex-out">${escapeHtml(r.out)}</span><span class="mr-ex-n muted">×${n}</span></div>`;
      }).join('');
      const partial = wrap.querySelector('.mr-mode option[value="pattern"]:checked') ? ' · partial - pattern rules execute on the server after saving' : '';
      exBox.innerHTML = `<div class="mr-ex-hd muted">your traffic - top spellings · ${changed}/${exs.length} change${partial}</div>` + rows;
    }
  }
  mrSyncCount(wrap);
}

// mrValidateInputs pre-checks the add-row inputs (deny by default: an
// invalid pattern or empty from flashes instead of adding a row that could
// never load). Server-only syntax (info level) is accepted - it saves and
// applies fine; the browser preview just skips it.
function mrValidateInputs(mode, fromIn, toIn) {
  const from = String((fromIn || {}).value || '');
  const to = String((toIn || {}).value || '');
  if (mode === 'lower') return { mode: 'lower' };
  if (!from.trim()) { flashBadInput(fromIn); return null; }
  if (mode === 'exact') {
    if (from !== from.trim()) { flashBadInput(fromIn); return null; }
    return { mode, from, to };
  }
  // Same gates the server applies at load - pre-check here so the row
  // never enters a save the POST would reject.
  const lint = re2Lint(from);
  if (lint && lint.level === 'error') { flashBadInput(fromIn); return null; }
  if (lint && lint.level === 'info') return { mode, from, to };
  try { new RegExp(from); } catch { flashBadInput(fromIn); return null; }
  return { mode, from, to };
}

// addModelRuleRow appends one rule from the add-row inputs.
function addModelRuleRow(addRow) {
  const wrap = addRow.closest('.mr-wrap');
  const mode = addRow.querySelector('.mr-mode').value;
  const rule = mrValidateInputs(mode, addRow.querySelector('.mr-new-from'), addRow.querySelector('.mr-new-to'));
  if (!rule) return;
  wrap.querySelector('.mr-rows').insertAdjacentHTML('beforeend', modelRuleRowHTML(rule));
  addRow.querySelector('.mr-new-from').value = '';
  addRow.querySelector('.mr-new-to').value = '';
  markSettingsDirty();
  validateModelRulesDraft(wrap);
  mrPreview(wrap);
}

// mrApplyTemplate appends one pre-filled rule from the templates select -
// the common cases are one click (Cloudflare-style rule templates).
function mrApplyTemplate(sel) {
  const idx = parseInt(sel.value, 10);
  sel.value = '';
  if (!Number.isInteger(idx) || !MR_TEMPLATES[idx]) return;
  const wrap = sel.closest('.mr-wrap');
  if (wrap.querySelectorAll('.mr-row').length >= MODEL_RULES_MAX) return;
  const t = MR_TEMPLATES[idx][1];
  wrap.querySelector('.mr-rows').insertAdjacentHTML('beforeend', modelRuleRowHTML(t));
  const rows = wrap.querySelectorAll('.mr-row');
  const row = rows[rows.length - 1];
  validateMrRow(row, null);
  markSettingsDirty();
  mrPreview(wrap);
  const from = row.querySelector('.mr-from');
  if (from && !from.value) from.focus();
}

// mrRestoreDefaults replaces the draft with the shipped pipeline - cheap
// to try: nothing is committed until Apply.
function mrRestoreDefaults(wrap) {
  wrap.querySelector('.mr-rows').innerHTML = MR_DEFAULTS.map(modelRuleRowHTML).join('');
  markSettingsDirty();
  validateModelRulesDraft(wrap);
  mrPreview(wrap);
}

// addAliasRow appends one mapping row from the add inputs. Deny by default:
// either side empty, a self-mapping, or a duplicate old label flashes the
// offending input instead of adding a row that could never apply.
function addAliasRow(row) {
  const fromIn = row.querySelector('.al-new-from');
  const toIn = row.querySelector('.al-new-to');
  const from = String((fromIn || {}).value || '');
  const to = String((toIn || {}).value || '');
  const wrap = row.closest('[data-kind="aliases"]');
  if (!from) { flashBadInput(fromIn); return; }
  if (!to) { flashBadInput(toIn); return; }
  if (from === to) { flashBadInput(toIn); return; }
  if (wrap && [...wrap.querySelectorAll('.al-row .al-from')].some(l => l.value.trim() === from)) { flashBadInput(fromIn); return; }
  (wrap ? wrap.querySelector('.al-rows') : row).insertAdjacentHTML('beforeend', aliasRowHTML(from, to));
  if (fromIn) fromIn.value = '';
  if (toIn) toIn.value = '';
  markSettingsDirty();
}

// providersAddHTML renders the "+ add provider" control that lives in the
// field's title row (top right, aligned with "Provider field maps"), in the
// same .btn / dropdown idiom as the header menus. The seen-provider picker
// list is filled by syncProvMenuList, which excludes providers already added.
function providersAddHTML() {
  return `<span class="prov-top"><button type="button" class="btn" data-prov-menu-toggle aria-haspopup="menu" aria-expanded="false">+ add provider</button>` +
    `<span class="prov-menu" hidden role="menu">` +
    `<span class="prov-menu-list"></span>` +
    `<span class="prov-menu-custom"><input class="prov-new-label" placeholder="custom label - e.g. nano-gpt.com" aria-label="custom provider label"><button type="button" class="btn prov-addbtn" data-prov-add aria-label="add provider">+</button></span>` +
    `</span></span>`;
}

// syncProvMenuList rebuilds the picker: only seen providers not already
// mapped are offered (deny by default - an already-added label can only be
// re-added through the custom input, which the duplicate guard refuses).
function syncProvMenuList(row) {
  const list = row && row.querySelector('.prov-menu-list');
  if (!list) return;
  const taken = new Set([...row.querySelectorAll('.st-prov .sp-label')].map(l => l.value.trim()));
  const known = (typeof pauseState === 'object' && pauseState && Array.isArray(pauseState.known_providers)) ? pauseState.known_providers : [];
  list.innerHTML = known.filter(p => !taken.has(p)).map(p => `<button type="button" class="prov-menu-item" data-prov-pick="${escapeHtml(p)}" role="menuitem"><span class="prov-ic" aria-hidden="true">☁</span>${escapeHtml(p)}</button>`).join('');
}

function flashBadInput(el) {
  if (!el) return;
  el.classList.add('prov-bad');
  setTimeout(() => el.classList.remove('prov-bad'), 1200);
}

function addCostKey(sec) {
  const input = sec && sec.querySelector('.sp-cost-in');
  const chips = sec && sec.querySelector('.prov-chips');
  if (!input || !chips) return;
  const path = String(input.value || '').trim();
  if (!path || !PROV_PATH_RE.test(path)) { flashBadInput(input); return; }
  if ([...chips.querySelectorAll('.prov-chip')].some(c => c.dataset.cost === path)) { flashBadInput(input); return; }
  chips.insertAdjacentHTML('beforeend', costChipHTML(path));
  input.value = '';
  markSettingsDirty();
}

// The same free-field owner is called after add, remove and field changes.
function syncMappingPicker(sec, kind) {
  const usage = kind === 'usage', letter = usage ? 'u' : 'm';
  const fields = usage ? canonicalUsageFields() : canonicalModelFields();
  const selected = sec.querySelector('.sp-' + letter + 'field-new')?.value;
  const taken = new Set([...sec.querySelectorAll('.sp-' + letter + 'field')].map(el => el.value));
  const free = fields.filter(field => !taken.has(field));
  sec.querySelector('.prov-add-' + letter)?.remove();
  if (!free.length) return;
  sec.insertAdjacentHTML('beforeend', `<div class="prov-add prov-add-${letter}"><select class="sp-${letter}field-new" aria-label="canonical ${usage ? 'token' : 'model'} field">${free.map(field => `<option value="${escapeHtml(field)}">${escapeHtml(field)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-${kind} aria-label="add ${kind} mapping">+ map</button></div>`);
  if (free.includes(selected)) sec.querySelector('.sp-' + letter + 'field-new').value = selected;
}
function addMappingRow(sec, kind) {
  const usage = kind === 'usage', letter = usage ? 'u' : 'm';
  const map = sec?.querySelector('.prov-' + letter + 'map');
  const sel = sec?.querySelector('.sp-' + letter + 'field-new');
  if (!map || !sel?.value) return;
  map.insertAdjacentHTML('beforeend', usage ? usageRowHTML(sel.value, '', canonicalUsageFields()) : modelRowHTML(sel.value, '', canonicalModelFields()));
  syncMappingPicker(sec, kind);
  markSettingsDirty();
}
function addUsageRow(sec) { addMappingRow(sec, 'usage'); }
function addModelRow(sec) { addMappingRow(sec, 'model'); }

// addHeaderRow appends one name/value row from the add inputs. Deny by
// default: an empty name or value, or a duplicate name (one value per
// header), flashes the offending input instead of adding a row the server
// would refuse.
function addHeaderRow(sec) {
  const map = sec && sec.querySelector('.prov-hmap');
  const nameIn = sec && sec.querySelector('.prov-add-h .sp-hname');
  const valIn = sec && sec.querySelector('.prov-add-h .sp-hval');
  if (!map || !nameIn || !valIn) return;
  const name = String(nameIn.value || '').trim();
  const value = String(valIn.value || '').trim();
  if (!name || !PROV_HEADER_RE.test(name)) { flashBadInput(nameIn); return; }
  if (!value) { flashBadInput(valIn); return; }
  if ([...map.querySelectorAll('.prov-hrow .sp-hname')].some(n => n.value.trim() === name)) { flashBadInput(nameIn); return; }
  map.insertAdjacentHTML('beforeend', headerRowHTML(name, value));
  nameIn.value = '';
  valIn.value = '';
  markSettingsDirty();
}

// provWrapFrom resolves the cards container for anything inside the
// providers row (the picker lives in the title row, outside [data-kind]).
function provWrapFrom(node) {
  const row = node && node.closest('.st-row');
  return row ? row.querySelector('[data-kind="providers"]') : null;
}

function addProviderLabel(label, node) {
  const wrap = provWrapFrom(node);
  if (!wrap) return;
  label = String(label || '').trim();
  const menuInput = wrap.closest('.st-row').querySelector('.prov-new-label');
  if (!label || !/^[A-Za-z0-9._-]+$/.test(label)) { flashBadInput(menuInput); return; }
  if ([...wrap.querySelectorAll('.sp-label')].some(l => l.value.trim() === label)) { flashBadInput(menuInput); return; }
  wrap.insertAdjacentHTML('beforeend', providerCardHTML(label, { cost_keys: [], usage_keys: {} }));
  if (menuInput) menuInput.value = '';
  closeProvMenus();
  const card = [...wrap.querySelectorAll('.st-prov')].pop();
  if (card && card.scrollIntoView) card.scrollIntoView({ block: 'nearest' });
  markSettingsDirty();
}

function addProviderCard(trigger) {
  const menu = trigger.closest('.prov-menu');
  const input = menu && menu.querySelector('.prov-new-label');
  addProviderLabel(input ? input.value : '', trigger);
}

// Collapse/expand is view state only - it changes no data, so it must not
// arm the apply button (the fingerprint is unchanged: collect reads the DOM
// whether or not the body is displayed).
function toggleProvCollapse(card) {
  if (!card) return;
  const collapsed = card.classList.toggle('collapsed');
  const chev = card.querySelector('[data-prov-collapse]');
  if (chev) chev.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
}

function closeProvMenus(scope) {
  (scope || document).querySelectorAll('.prov-menu:not([hidden])').forEach(m => {
    m.hidden = true;
    const btn = m.parentElement && m.parentElement.querySelector('[data-prov-menu-toggle]');
    if (btn) btn.setAttribute('aria-expanded', 'false');
  });
}

function providersEditorClick(e) {
  const t = e.target;
  if (t.closest('[data-al-rm]')) { t.closest('.al-row').remove(); markSettingsDirty(); return; }
  if (t.closest('[data-al-add]')) { addAliasRow(t.closest('.prov-add')); return; }
  if (t.closest('[data-mr-rm]')) { const wrap = t.closest('.mr-wrap'); t.closest('.mr-row').remove(); markSettingsDirty(); validateModelRulesDraft(wrap); mrPreview(wrap); return; }
  if (t.closest('[data-mr-add]')) { addModelRuleRow(t.closest('.mr-add')); return; }
  if (t.closest('[data-mr-up]')) { const row = t.closest('.mr-row'); const prev = row.previousElementSibling; const wrap = t.closest('.mr-wrap'); if (prev) row.parentNode.insertBefore(row, prev); markSettingsDirty(); validateModelRulesDraft(wrap); mrPreview(wrap); return; }
  if (t.closest('[data-mr-dn]')) { const row = t.closest('.mr-row'); const next = row.nextElementSibling; const wrap = t.closest('.mr-wrap'); if (next) row.parentNode.insertBefore(next, row); markSettingsDirty(); validateModelRulesDraft(wrap); mrPreview(wrap); return; }
  if (t.closest('[data-mr-vis]')) {
    const btn = t.closest('[data-mr-vis]');
    const row = btn.closest('.mr-row');
    const wrap = btn.closest('.mr-wrap');
    const off = row.dataset.mrOff === '1';
    if (off) delete row.dataset.mrOff; else row.dataset.mrOff = '1';
    const mode = row.querySelector('.mr-mode');
    const lower = mode.value === 'lower';
    btn.textContent = off ? '◉' : '◌';
    btn.setAttribute('aria-pressed', String(!off));
    btn.title = off ? 'un-park this rule' : 'park this rule - keep it without applying it';
    mode.disabled = !off;
    row.querySelector('.mr-from').disabled = !off || lower;
    row.querySelector('.mr-to').disabled = !off || lower;
    markSettingsDirty();
    validateModelRulesDraft(wrap);
    mrPreview(wrap);
    return;
  }
  if (t.closest('[data-mr-restore]')) { mrRestoreDefaults(t.closest('.mr-wrap')); return; }
  if (t.closest('[data-prov-rm]')) { t.closest('.st-prov').remove(); markSettingsDirty(); return; }
  if (t.closest('[data-prov-collapse]')) { toggleProvCollapse(t.closest('.st-prov')); return; }
  // The header band itself toggles collapse - except its interactive parts.
  const hd = t.closest('.prov-hd');
  if (hd && !t.closest('.sp-label') && !t.closest('[data-prov-rm]')) { toggleProvCollapse(hd.closest('.st-prov')); return; }
  if (t.closest('[data-prov-menu-toggle]')) {
    const menu = t.closest('.prov-top') && t.closest('.prov-top').querySelector('.prov-menu');
    if (menu) {
      // Re-sync the picker at open time: providers added or removed since the
      // last render must be reflected (already-added ones never offered).
      if (menu.hidden) syncProvMenuList(t.closest('.st-row'));
      menu.hidden = !menu.hidden;
      t.closest('[data-prov-menu-toggle]').setAttribute('aria-expanded', menu.hidden ? 'false' : 'true');
    }
    return;
  }
  if (t.closest('.prov-menu')) {
    const pick = t.closest('[data-prov-pick]');
    if (pick) { addProviderLabel(pick.dataset.provPick, pick); return; }
    if (t.closest('[data-prov-add]')) { addProviderCard(t.closest('[data-prov-add]')); return; }
    return; // clicks inside the open menu close nothing but themselves
  }
  if (t.closest('[data-prov-chip-rm]')) { t.closest('.prov-chip').remove(); markSettingsDirty(); return; }
  if (t.closest('[data-prov-urow-rm]')) {
    const sec = t.closest('.prov-sec');
    t.closest('.prov-urow').remove();
    syncMappingPicker(sec, 'usage');
    markSettingsDirty();
    return;
  }
  if (t.closest('[data-prov-add-cost]')) { addCostKey(t.closest('.prov-sec')); return; }
  if (t.closest('[data-prov-add-usage]')) { addUsageRow(t.closest('.prov-sec')); return; }
  if (t.closest('[data-prov-mrow-rm]')) {
    const sec = t.closest('.prov-sec');
    t.closest('.prov-urow').remove();
    syncMappingPicker(sec, 'model');
    markSettingsDirty();
    return;
  }
  if (t.closest('[data-prov-add-model]')) { addModelRow(t.closest('.prov-sec')); return; }
  if (t.closest('[data-prov-hrow-rm]')) { t.closest('.prov-urow').remove(); markSettingsDirty(); return; }
  if (t.closest('[data-prov-add-header]')) { addHeaderRow(t.closest('.prov-sec')); return; }
  // Any click elsewhere in the editor closes a stray open add-menu.
  closeProvMenus(t.closest('[data-kind="providers"]'));
}

// One delegated wiring for the settings form (dirty tracking + provider
// editor actions) - replaces the per-element listeners that innerHTML
// rebuilds would drop.
function wireSettingsDelegation() {
  const box = $('settings-fields');
  if (!box || box.dataset.provWired) return;
  box.dataset.provWired = '1';
  const dirty = e => {
    if (e.target.closest && e.target.closest('.st-backup')) return;
    if (e.target.closest && e.target.closest('.st-row')) markSettingsDirty();
  };
  box.addEventListener('input', e => {
    if (e.target.closest && e.target.closest('.mr-wrap')) {
      const wrap = e.target.closest('.mr-wrap');
      // live per-keystroke validation (regex101/NN(g)-style: inline, at
      // the field, the moment it goes wrong) + the preview bench refresh.
      validateModelRulesDraft(wrap);
      mrPreview(wrap);
    }
    if (e.target.closest && e.target.closest('.st-backup')) return;
    if (e.target.closest && e.target.closest('.st-row')) markSettingsDirty();
  });
  box.addEventListener('change', e => {
    if (e.target.closest && e.target.closest('.st-backup')) {
      if (e.target.name === 'backup-config-mode') backupConfigMode = e.target.value;
      if (e.target.name === 'backup-database-mode') backupDatabaseMode = e.target.value;
      paintBackupPane();
      return;
    }
    if (e.target.matches('.sp-ufield, .sp-mfield')) syncMappingPicker(e.target.closest('.prov-sec'), e.target.matches('.sp-ufield') ? 'usage' : 'model');
    if (e.target.classList && (e.target.classList.contains('sp-upath') || e.target.classList.contains('sp-cost-in') || e.target.classList.contains('sp-label') || e.target.classList.contains('sp-mkey') || e.target.classList.contains('sp-mpath') || e.target.classList.contains('sp-hname') || e.target.classList.contains('sp-hval'))) {
      const v = String(e.target.value || '').trim();
      let bad = false;
      if (v !== '') {
        if (e.target.classList.contains('sp-mpath')) bad = !v.startsWith('/');
        else if (e.target.classList.contains('sp-mkey') || e.target.classList.contains('sp-upath')) bad = !PROV_PATH_RE.test(v);
        else if (e.target.classList.contains('sp-hname')) bad = !PROV_HEADER_RE.test(v);
      }
      e.target.classList.toggle('prov-bad', bad);
    }
    const mrMode = e.target.classList && e.target.classList.contains('mr-mode') ? e.target.closest('.mr-row') : null;
    if (mrMode) {
      const lower = e.target.value === 'lower';
      mrMode.querySelector('.mr-from').disabled = lower;
      mrMode.querySelector('.mr-to').disabled = lower;
      markSettingsDirty();
      validateModelRulesDraft(e.target.closest('.mr-wrap'));
      mrPreview(e.target.closest('.mr-wrap'));
    }
    if (e.target.classList && e.target.classList.contains('mr-tpl')) mrApplyTemplate(e.target);
    dirty(e);
  });
  box.addEventListener('keydown', e => {
    if (e.key !== 'Enter') return;
    if (e.target.classList.contains('sp-cost-in')) { e.preventDefault(); addCostKey(e.target.closest('.prov-sec')); }
    else if (e.target.classList.contains('prov-new-label')) { e.preventDefault(); addProviderCard(e.target); }
    else if (e.target.classList.contains('al-new-from') || e.target.classList.contains('al-new-to')) { e.preventDefault(); addAliasRow(e.target.closest('.prov-add')); }
    else if (e.target.classList.contains('mr-new-from') || e.target.classList.contains('mr-new-to')) { e.preventDefault(); addModelRuleRow(e.target.closest('.mr-add')); }
    else if (e.target.classList.contains('sp-hname') || e.target.classList.contains('sp-hval')) { e.preventDefault(); addHeaderRow(e.target.closest('.prov-sec')); }
  });
  box.addEventListener('click', e => {
    if (e.target && e.target.id === 'btn-backup-download') { runBackupDownload(); return; }
    if (e.target && e.target.id === 'btn-backup-restore') { const f = $('backup-file'); if (f) f.click(); return; }
    if (e.target && e.target.id === 'btn-backup-apply') { runBackupApply(); return; }
    if (e.target && e.target.id === 'btn-backup-cancel') { runBackupCancel(); return; }
    providersEditorClick(e);
  });
  // Clicks outside the add-menu close it (the menu lives in the sheet, but
  // the veil/other panels are outside #settings-fields).
  document.addEventListener('click', e => {
    if (e.target.closest && (e.target.closest('.prov-menu') || e.target.closest('[data-prov-menu-toggle]'))) return;
    closeProvMenus();
  });
}
wireSettingsDelegation();

function collectSettingsValues(validate = false) {
  const out = Object.create(null);
  const box = $('settings-fields');
  if (!box) return out;
  const put = (map, key, value, input) => {
    if (validate && (!key || Object.hasOwn(map, key))) {
      throw Object.assign(new Error(key ? 'duplicate mapping: ' + key : 'mapping name is required'), { input });
    }
    map[key] = value;
  };
  box.querySelectorAll('[data-key]').forEach(el => {
    if (el.disabled) return;
    if (el.classList.contains('st-row')) return;
    const k = el.dataset.key;
    if (!k) return;
    if (el.dataset.kind === 'providers') {
      const m = Object.create(null);
      el.querySelectorAll('.st-prov').forEach(card => {
        const label = String((card.querySelector('.sp-label') || {}).value || '').trim();
        const cost = [...new Set([...card.querySelectorAll('.prov-chip')].map(c => String(c.dataset.cost || '').trim()).filter(Boolean))];
        const usage = Object.create(null);
        card.querySelectorAll('.prov-umap .prov-urow').forEach(row => {
          const f = (row.querySelector('.sp-ufield') || {}).value;
          const p = String((row.querySelector('.sp-upath') || {}).value || '').trim();
          put(usage, f, p, row.querySelector('.sp-ufield'));
        });
        const modelsKeys = Object.create(null);
        card.querySelectorAll('.prov-mmap .prov-urow').forEach(row => {
          const f = (row.querySelector('.sp-mfield') || {}).value;
          const p = String((row.querySelector('.sp-mkey') || {}).value || '').trim();
          put(modelsKeys, f, p, row.querySelector('.sp-mfield'));
        });
        const headers = Object.create(null);
        card.querySelectorAll('.prov-hmap .prov-hrow').forEach(row => {
          const n = String((row.querySelector('.sp-hname') || {}).value || '').trim();
          const v = String((row.querySelector('.sp-hval') || {}).value || '');
          put(headers, n, v, row.querySelector('.sp-hname'));
        });
        put(m, label, {
          cost_keys: cost,
          usage_keys: usage,
          models_path: String((card.querySelector('.sp-mpath') || {}).value || '').trim(),
          models_keys: modelsKeys,
          headers: headers,
        }, card.querySelector('.sp-label'));
      });
      out[k] = m;
      return;
    }
    if (el.dataset.kind === 'model_rules') {
      out[k] = collectModelRules(el);
      return;
    }
    if (el.dataset.kind === 'aliases') {
      const m = Object.create(null);
      el.querySelectorAll('.al-row').forEach(row => {
        const from = String((row.querySelector('.al-from') || {}).value || '').trim();
        const to = String((row.querySelector('.al-to') || {}).value || '').trim();
        // Keep incomplete/self mappings in the draft; the shared server
        // validator rejects them rather than silently deleting a saved row.
        put(m, from, to, row.querySelector('.al-from'));
      });
      out[k] = m;
      return;
    }
    if (el.type === 'checkbox') { out[k] = !!el.checked; return; }
    if (el.tagName === 'TEXTAREA') {
      out[k] = el.value.split('\n').map(s => s.trim()).filter(Boolean);
      return;
    }
    if (el.type === 'number') {
      const n = Number(el.value);
      if (validate && (!el.checkValidity() || !Number.isSafeInteger(n))) {
        throw Object.assign(new Error(k + ': enter a valid integer'), { input: el });
      }
      out[k] = el.value.trim() && Number.isFinite(n) ? n : el.value;
      return;
    }
    out[k] = el.value;
  });
  return out;
}

function settingsFingerprint() {
  return JSON.stringify(collectSettingsValues());
}

function markSettingsDirty() {
  if (settingsDoc && settingsDoc.writable) {
    const n = settingsDirtyCount();
    if (n) settingsStatus(n === 1 ? '1 unsaved' : n + ' unsaved');
    else settingsStatus(settingsRestartNotice(settingsDoc));
  }
  updateSettingsActions();
}

function updateSettingsActions() {
  const dirty = !!(settingsSnap && settingsFingerprint() !== settingsSnap);
  const apply = $('btn-settings-apply');
  const revert = $('btn-settings-revert');
  const busy = !!(apply?.dataset.busy || revert?.dataset.busy);
  if (apply) apply.disabled = !(dirty && settingsDoc && settingsDoc.writable) || busy;
  if (revert) revert.disabled = !dirty || busy;
}

function showSettingsCat() {
  const box = $('settings-fields');
  const hd = $('settings-pane-hd');
  const rail = $('settings-rail');
  if (!box) return;
  const q = (settingsQ || '').trim().toLowerCase();
  const cats = (settingsDoc && settingsDoc.categories) || [];
  const cat = cats.find(c => c.id === settingsCat) || cats[0];
  let shown = 0, restarts = 0;
  box.querySelectorAll('.st-row').forEach(row => {
    const hay = ((row.dataset.label || '') + ' ' + (row.dataset.key || '') + ' ' + (row.dataset.help || '')).toLowerCase();
    const hit = !q || hay.includes(q);
    const inCat = !q && row.dataset.cat === (cat && cat.id);
    row.hidden = q ? !hit : !inCat;
    if (!row.hidden) {
      shown++;
      if (row.dataset.hot === '0') restarts++;
    }
  });
  if (hd) {
    const banner = (!q && restarts) ? '<div class="st-banner">takes a process restart</div>' : '';
    if (q) hd.innerHTML = `<h4>${shown} match${shown === 1 ? '' : 'es'}</h4><p>${escapeHtml(q)}</p>`;
    else if (cat) hd.innerHTML = `<h4>${escapeHtml(cat.label)}</h4><p>${escapeHtml(cat.help || '')}</p>${banner}`;
    else hd.innerHTML = '';
  }
  if (rail) {
    rail.querySelectorAll('.st-rail-item').forEach(btn => {
      const on = !q && btn.dataset.stCat === settingsCat;
      btn.classList.toggle('active', on);
      btn.setAttribute('aria-current', on ? 'page' : 'false');
      const id = btn.dataset.stCat;
      const n = [...box.querySelectorAll('.st-row')].filter(r => {
        if (r.dataset.cat !== id) return false;
        if (!q) return true;
        const hay = ((r.dataset.label || '') + ' ' + (r.dataset.key || '') + ' ' + (r.dataset.help || '')).toLowerCase();
        return hay.includes(q);
      }).length;
      const badge = btn.querySelector('.rail-n');
      if (badge) badge.textContent = n;
      btn.classList.toggle('has-hit', !!q && n > 0);
      btn.classList.toggle('is-miss', !!q && n === 0);
    });
  }
}

function filterSettings() {
  const q = $('settings-q');
  settingsQ = q ? q.value : '';
  showSettingsCat();
}

function applySettings() {
  const btn = $('btn-settings-apply');
  if (!btn || btn.disabled || btn.dataset.busy) return;
  let values;
  try { values = collectSettingsValues(true); }
  catch (err) {
    settingsStatus(String(err.message || err));
    if (err.input) { err.input.focus(); flashBadInput(err.input); }
    return;
  }
  const submitted = JSON.stringify(values);
  btn.dataset.busy = '1';
  updateSettingsActions();
  // Model-rules save gate: nothing invalid reaches the POST (the row is
  // already marked red inline - focus + flash the first offender).
  const mrW = $('settings-fields') && $('settings-fields').querySelector('[data-kind="model_rules"]');
  if (mrW) {
    const gate = validateModelRulesDraft(mrW);
    if (!gate.ok) {
      delete btn.dataset.busy;
      updateSettingsActions();
      settingsStatus('model rules - fix the highlighted rule first');
      const f = gate.firstBad.querySelector('.mr-from');
      if (f) { f.focus(); flashBadInput(f); }
      gate.firstBad.scrollIntoView({ block: 'center' });
      return;
    }
  }
  ++settingsReq;
  return operatorFetch('/admin/config', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ values, revision: settingsDoc.revision }),
  }).then(async r => {
    const j = await r.json();
    ++settingsReq; // GETs issued during this save may still describe pre-save state
    if (r.ok || j.saved) {
      const unchanged = settingsFingerprint() === submitted;
      settingsDoc = { ...settingsDoc, values: j.values, effective: j.effective, revision: j.revision, restart_required: j.restart_required };
      applyDashValues(j.effective || j.values);
      if (unchanged) fillSettingsForm(settingsDoc);
      else settingsSnap = submitted; // typing during the save remains unsaved
    }
    if (!r.ok) throw new Error(j.error || r.statusText);
    const restart = settingsRestartNotice(settingsDoc);
    settingsStatus(restart ? 'saved · ' + restart : 'saved', restart ? '' : 'ok');
    if (settingsIsDirty()) settingsStatus('saved · ' + settingsDirtyCount() + ' unsaved changes');
    if (lastData) renderAll(lastData);
  }).catch(err => {
    settingsStatus(String(err.message || err));
  }).finally(() => { if (btn) delete btn.dataset.busy; updateSettingsActions(); });
}

function revertSettings() {
  const btn = $('btn-settings-revert');
  if (btn && btn.disabled) return;
  if (btn) btn.dataset.busy = '1';
  updateSettingsActions();
  return fetchSettings(true).finally(() => {
    if (btn) delete btn.dataset.busy;
    updateSettingsActions();
  });
}

document.getElementById('settings-rail')?.addEventListener('click', e => {
  const b = e.target.closest('.st-rail-item[data-st-cat]');
  if (!b) return;
  settingsCat = b.dataset.stCat;
  settingsQ = '';
  const q = $('settings-q');
  if (q) q.value = '';
  showSettingsCat();
});

function applyDebugState(st, revision = operatorState.debug.revision) {
  if (!acceptOperator('debug', st, revision) || !applyModelCanon(st.model_canon)) return;
  debugState = {
    enabled: !!st.enabled,
    sessions: Array.isArray(st.sessions) ? st.sessions : [],
    known_clients: Array.isArray(st.known_clients) ? st.known_clients : [],
    known_providers: Array.isArray(st.known_providers) ? st.known_providers : [],
    known_models: Array.isArray(st.known_models) ? st.known_models : [],
    until: st.until || null,
    ttl: st.ttl || '',
    max_bytes: st.max_bytes == null || st.max_bytes === '' ? '' : String(st.max_bytes),
  };
  refreshFooterState();
  const menu = $('debug-menu');
  if (menu && !menu.hidden) syncDebugMenuState();
  if (lastData) scheduleRenderLive();
}

function togglePauseMenu(e) {
  e.stopPropagation();
  const m = $('pause-menu');
  if (m.hidden) { closeHeaderMenus('pause-menu'); resetPauseMenuForm(); syncPauseMenuState(); m.hidden = false; }
  else m.hidden = true;
  syncHdrMenuExpanded();
}

function holdLive(h) {
  if (!h) return false;
  if (!h.until) return true;
  const ms = new Date(h.until).getTime() - Date.now();
  return Number.isFinite(ms) && ms > 0;
}

function liveHolds() {
  return (pauseState.holds || []).filter(holdLive);
}

function pauseActive() {
  return liveHolds().length > 0;
}

function pauseMenuScope() {
  const el = $('pf-scope');
  return el ? el.value : '';
}

function onPauseScopeChange() {
  const blanket = pauseMenuScope() === 'all';
  const box = $('pf-clients');
  const pbox = $('pf-providers');
  if (box) box.classList.toggle('pause-disabled', blanket);
  if (pbox) pbox.classList.toggle('pause-disabled', blanket);
  const disable = (root) => {
    if (!root) return;
    root.querySelectorAll('input[type=checkbox]').forEach(c => {
      c.disabled = operatorState.pause.busy || blanket;
      if (blanket) c.checked = false;
    });
  };
  disable(box);
  disable(pbox);
  updatePauseApplyEnabled();
}

function selectedPauseChecks(boxId) {
  return [...document.querySelectorAll('#' + boxId + ' input[type=checkbox]:checked')].map(c => c.value);
}

function pauseMenuDuration() {
  const el = $('pf-dur');
  return el ? el.value : '';
}

function pauseMenuCap() {
  const el = $('pf-cap');
  if (!el) return null;
  if (!el.validity.valid) return NaN;
  if (el.value === '') return null;
  const n = Number(el.value);
  return Number.isSafeInteger(n) && n >= 0 ? n : NaN;
}

function updatePauseApplyEnabled() {
  const btn = $('btn-pause-apply');
  if (!btn) return;
  const scope = pauseMenuScope();
  btn.disabled = operatorState.pause.busy || (scope !== 'all' && scope !== 'new' && selectedPauseChecks('pf-clients').length === 0 && selectedPauseChecks('pf-providers').length === 0);
  btn.textContent = pauseEditID ? 'Update' : 'Pause';
}

function fillPauseChecks(box, names, picked) {
  if (!box) return;
  const keep = picked instanceof Set ? picked : new Set();
  box.innerHTML = (names || []).map(c => {
    // Overlap is a client×provider predicate, owned by the scheduler. Greying
    // individual names here prevents valid disjoint combinations and makes
    // polling silently erase the operator's selected draft.
    return `<label class="pause-check"><input type="checkbox" value="${escapeHtml(c)}" ${keep.has(c)?'checked':''}> ${escapeHtml(c)}</label>`;
  }).join('');
  box.querySelectorAll('input').forEach(el => { el.onchange = updatePauseApplyEnabled; });
}

function holdScopeLabel(h) {
  if (!h) return '';
  if (h.all) return pauseScopeText('all');
  const bits = [];
  if (h.new) bits.push(pauseScopeText('new'));
  if (Array.isArray(h.clients) && h.clients.length) bits.push(h.clients.join(', '));
  if (Array.isArray(h.providers) && h.providers.length) bits.push(h.providers.join(', '));
  return bits.join(' + ') || 'hold';
}

function renderPauseHolds() {
  const box = $('pf-holds');
  if (!box) return;
  const holds = liveHolds();
  if (!holds.length) { box.innerHTML = ''; return; }
  box.innerHTML = '<div class="pause-known-hd">Active holds</div>' + holds.map(h => {
    const left = [holdScopeLabel(h)];
    const until = pauseUntilLabel(h.until);
    if (until) left.push(until);
    else if (h.duration) left.push(h.duration);
    if (h.max_queued > 0) left.push('cap ' + h.max_queued);
    const q = typeof h.queued === 'number' && h.queued > 0 ? ' · q ' + h.queued : '';
    const editing = pauseEditID === h.id ? ' editing' : '';
    return `<div class="pause-hold${editing}"><button class="hold-edit" data-operator="pause-edit" data-value="${escapeHtml(h.id)}" ${operatorState.pause.busy ? 'disabled' : ''}>${escapeHtml(left.join(' · '))}${q}</button>` +
      `<button class="btn" ${operatorState.pause.busy ? 'disabled' : ''} data-operator="pause-resume" data-value="${escapeHtml(h.id)}">Resume</button></div>`;
  }).join('');
}

function refreshPauseKnownLists() {
  const pickedC = new Set(selectedPauseChecks('pf-clients'));
  const pickedP = new Set(selectedPauseChecks('pf-providers'));
  const knownC = [...new Set([...(pauseState.known_clients || []), ...(pauseState.clients || []), ...pickedC])].sort();
  const knownP = [...new Set([...(pauseState.known_providers || []), ...(pauseState.providers || []), ...pickedP])].sort();
  fillPauseChecks($('pf-clients'), knownC, pickedC);
  fillPauseChecks($('pf-providers'), knownP, pickedP);
  onPauseScopeChange();
}

function syncPauseMenuState() {
  fillPauseControls();
  if (pauseEditID && !liveHolds().some(h => h.id === pauseEditID)) {
    pauseEditID = '';
    resetPauseMenuForm();
  }
  refreshPauseKnownLists();
  const cap = $('pf-cap');
  if (cap && document.activeElement !== cap) {
    cap.placeholder = pauseState.default_max_queued > 0 ? String(pauseState.default_max_queued) : 'unlimited';
  }
  const resume = $('btn-pause-resume');
  if (resume) resume.disabled = operatorState.pause.busy || !pauseActive();
  ['pf-scope', 'pf-dur', 'pf-cap'].forEach(id => { if ($(id)) $(id).disabled = operatorState.pause.busy; });
  const hd = document.querySelector('#pause-menu .clear-menu-hd');
  if (hd) {
    hd.textContent = pauseEditID ? 'Edit hold…' : (pauseActive() ? 'Holding requests…' : 'Hold requests…');
  }
  const count = $('pause-count');
  if (count && !count.dataset.err) {
    if (pauseActive()) {
      const bits = ['paused'];
      if (pauseState.queued > 0) bits.push('queued ' + pauseState.queued);
      const left = pauseUntilLabel(pauseState.until);
      if (left) bits.push(left);
      count.textContent = bits.join(' · ');
    } else {
      count.textContent = '';
    }
  }
  renderPauseHolds();
  updatePauseApplyEnabled();
}

function resetPauseMenuForm() {
  pauseEditID = '';
  fillPauseControls();
  if ($('pf-scope')) $('pf-scope').value = '';
  if ($('pf-dur')) $('pf-dur').value = '';
  const cap = $('pf-cap');
  if (cap) cap.value = '';
  const count = $('pause-count');
  if (count) { count.textContent = ''; delete count.dataset.err; }
  fillPauseChecks($('pf-clients'), [...new Set([...(pauseState.known_clients || []), ...(pauseState.clients || [])])].sort(), new Set());
  fillPauseChecks($('pf-providers'), [...new Set([...(pauseState.known_providers || []), ...(pauseState.providers || [])])].sort(), new Set());
  onPauseScopeChange();
}

function holdMatchesRecord(h, r) {
  if (!h || !r) return false;
  if (h.all) return true;
  const clients = h.clients || [];
  const providers = h.providers || [];
  const known = h.known_at_new || [];
  const client = r.client || '';
  const provider = r.provider || '';
  let clientOK = false;
  if (clients.length && clients.includes(client)) clientOK = true;
  else if (h.new) clientOK = !known.includes(client);
  else if (clients.length) clientOK = false;
  else clientOK = providers.length > 0;
  if (!clientOK) return false;
  if (!providers.length) return true;
  return !!provider && providers.includes(provider);
}

function editHoldForRecord(r) {
  const holds = liveHolds();
  if (!holds.length || !r) return;
  const named = holds.find(h => !h.all && holdMatchesRecord(h, r));
  const h = named || holds.find(h => h.all);
  if (!h) return;
  closeHeaderMenus('pause-menu');
  const m = $('pause-menu');
  if (m) m.hidden = false;
  syncHdrMenuExpanded();
  editPauseHold(h.id);
}

function recordById(id) {
  const hit = (lastData?.records || []).find(r => r && r.id === id);
  if (hit) return hit;
  return logArchive.find(r => r && r.id === id);
}

function editPauseHold(id) {
  if (!id || operatorState.pause.busy) return;
  if (pauseEditID === id) {
    resetPauseMenuForm();
    syncPauseMenuState();
    return;
  }
  const h = liveHolds().find(x => x.id === id);
  if (!h) return;
  pauseEditID = id;
  fillPauseControls();
  if ($('pf-scope')) $('pf-scope').value = h.all ? 'all' : (h.new ? 'new' : '');
  fillSelectPairs($('pf-dur'), PAUSE_DURS, h.duration || '');
  const cap = $('pf-cap');
  if (cap) cap.value = typeof h.max_queued === 'number' ? String(h.max_queued) : '';
  const count = $('pause-count');
  if (count) { count.textContent = ''; delete count.dataset.err; }
  const knownC = [...new Set([...(pauseState.known_clients || []), ...(h.clients || [])])].sort();
  const knownP = [...new Set([...(pauseState.known_providers || []), ...(h.providers || [])])].sort();
  fillPauseChecks($('pf-clients'), knownC, new Set(h.clients || []));
  fillPauseChecks($('pf-providers'), knownP, new Set(h.providers || []));
  onPauseScopeChange();
  syncPauseMenuState();
}

function applyPauseMenu() {
  if (operatorState.pause.busy) return;
  const scope = pauseMenuScope();
  const all = scope === 'all';
  const neu = scope === 'new';
  const clients = all ? [] : selectedPauseChecks('pf-clients');
  const providers = all ? [] : selectedPauseChecks('pf-providers');
  if (!all && !neu && !clients.length && !providers.length) return;
  const body = { paused: true, all, new: !all && neu, clients, providers, duration: pauseMenuDuration() };
  if (pauseEditID) body.id = pauseEditID;
  const cap = pauseMenuCap();
  if (Number.isNaN(cap)) {
    pauseMutationError('queue cap must be a non-negative whole number');
    return;
  }
  if (cap !== null) body.max_queued = cap;
  return mutateOperator('pause', body, resetPauseMenuForm);
}

function resumePauseHold(id) {
  if (!id || !pauseActive()) return;
  return mutateOperator('pause', { paused: false, id });
}

function resumePauseMenu() {
  if (!pauseActive()) return;
  return mutateOperator('pause', { paused: false }, () => hideHdrMenu('pause-menu'));
}

function pauseMutationError(message) {
  const count = $('pause-count');
  if (count) { count.textContent = message; count.dataset.err = '1'; }
}

function toggleLimitsMenu(e) {
  e.stopPropagation();
  const m = $('limits-menu');
  if (m.hidden) { closeHeaderMenus('limits-menu'); syncLimitsMenuState(); onLimitProviderChange(); m.hidden = false; }
  else m.hidden = true;
  syncHdrMenuExpanded();
}

function limitByProvider(name) {
  return (throttleState.throttles || []).find(t => t && t.provider === name) || null;
}

function fillSelectPairs(sel, pairs, value) {
  if (!sel || !pairs) return;
  sel.innerHTML = pairs.map(([val, lab]) => `<option value="${escapeHtml(String(val))}">${escapeHtml(lab)}</option>`).join('');
  const v = value == null ? '' : String(value);
  // Curated choices are suggestions, not permission to erase a saved value.
  if (!pairs.some(([val]) => String(val) === v)) sel.add(new Option(v || 'off', v));
  sel.value = v;
}

const PAUSE_SCOPES = [
  ['', 'named only'],
  ['all', 'all requests'],
  ['new', 'new clients'],
];
const PAUSE_DURS = [
  ['', 'I resume'],
  ['15m', '15 min'],
  ['1h', '1 hour'],
  ['6h', '6 hours'],
  ['12h', '12 hours'],
  ['24h', '24 hours'],
];
function pauseScopeText(val) {
  const hit = PAUSE_SCOPES.find(([v]) => v === val);
  return hit ? hit[1] : '';
}
function fillPauseControls() {
  fillSelectPairs($('pf-scope'), PAUSE_SCOPES, pauseMenuScope());
  fillSelectPairs($('pf-dur'), PAUSE_DURS, pauseMenuDuration());
}

const DEBUG_DURS = [['', 'I stop']].concat(PAUSE_DURS.slice(1));

function toggleDebugMenu(e) {
  e.stopPropagation();
  const m = $('debug-menu');
  if (m.hidden) { closeHeaderMenus('debug-menu'); resetDebugMenuForm(); syncDebugMenuState(); m.hidden = false; }
  else m.hidden = true;
  syncHdrMenuExpanded();
}

function liveDebugSessions() {
  return (debugState.sessions || []).filter(holdLive);
}

function debugActive() {
  return !!(debugState.enabled || liveDebugSessions().length);
}

function otherDebugSessions() {
  return liveDebugSessions().filter(h => h.id !== debugEditID);
}

function debugNameTaken(kind, name) {
  for (const h of otherDebugSessions()) {
    if (kind === 'client' && (h.clients || []).includes(name) && !(h.providers || []).length && !(h.models || []).length) return true;
    if (kind === 'provider' && (h.providers || []).includes(name) && !(h.clients || []).length && !(h.models || []).length) return true;
    if (kind === 'model' && (h.models || []).includes(name) && !(h.clients || []).length && !(h.providers || []).length) return true;
  }
  return false;
}

function selectedDebugChecks(boxId) {
  return [...document.querySelectorAll('#' + boxId + ' input[type=checkbox]:checked')].map(c => c.value);
}

// Debug-checklist model grouping. Clients and providers spell the same model
// differently (glm-5.3 vs glm-5-3, moonshotai/kimi-k3:nube vs kimi-k3), so the
// raw known-models list reads as duplicates. The checklist therefore groups
// spelling variants for display using the canonicalization rules from config
// (canonicalModel in explorer.js - observer-authored name lookup;
// rules editable in Settings). A debug session still stores
// and matches the exact raw spellings (Record.Model is the match key);
// checking a group selects every variant of it, and editing a session re-checks
// the group holding any of its raw models.
let dbgModelGroups = [];
// dbgGroupedModels: raw (deduped, sorted) names → ordered [group] objects
// {key, display, variants}; a group's display name is its shortest variant
// (the bare spelling), ties broken lexicographically.
function dbgGroupedModels(names) {
  const byGroup = new Map();
  for (const n of [...new Set(names || [])]) {
    const g = canonicalModel(n);
    if (!byGroup.has(g)) byGroup.set(g, []);
    byGroup.get(g).push(n);
  }
  const groups = [...byGroup.entries()].map(([key, variants]) => ({
    key,
    display: [...variants].sort((a, b) => a.length - b.length || (a < b ? -1 : 1))[0],
    variants: variants.sort(),
  }));
  return groups.sort((a, b) => (a.display < b.display ? -1 : a.display > b.display ? 1 : 0));
}
// selectedDebugModels expands the checked model groups back into the exact raw
// spellings the debug session matches on.
function selectedDebugModels() {
  const sel = new Set(selectedDebugChecks('df-models'));
  const out = [];
  for (const g of dbgModelGroups) if (sel.has(g.display)) out.push(...g.variants);
  return out;
}

function debugMenuDuration() {
  const el = $('df-dur');
  return el ? el.value : '';
}

function updateDebugApplyEnabled() {
  const btn = $('btn-debug-apply');
  if (!btn || btn.dataset.busy) return;
  btn.disabled = operatorState.debug.busy || selectedDebugChecks('df-clients').length === 0 && selectedDebugChecks('df-providers').length === 0 && selectedDebugChecks('df-models').length === 0;
  btn.textContent = debugEditID ? 'Update' : 'Start';
}

function fillDebugChecks(box, names, picked, kind) {
  if (!box) return;
  const keep = picked instanceof Set ? picked : new Set();
  if (kind === 'model') {
    // Grouped display (see debugModelGroupOf): one checkbox per spelling
    // group, value = the group's display name; the raw variants are carried
    // in dbgModelGroups and expanded by selectedDebugModels when the session
    // is applied. `keep` may hold raw names (edit path) or display names
    // (refresh path) - either marks the group checked.
    dbgModelGroups = dbgGroupedModels(names);
    box.innerHTML = dbgModelGroups.map(g => {
      const taken = g.variants.some(v => debugNameTaken('model', v));
      const on = !taken && (keep.has(g.display) || g.variants.some(v => keep.has(v)));
      const extra = g.variants.length > 1
        ? ` <span style="color:var(--muted)" aria-hidden="true">+${g.variants.length - 1}</span>` : '';
      return `<label class="pause-check${taken?' taken':''}" title="${escapeHtml(g.variants.join(' · '))}"><input type="checkbox" value="${escapeHtml(g.display)}" ${on?'checked':''} ${taken || operatorState.debug.busy ? 'disabled' : ''}> ${escapeHtml(g.display)}${extra}</label>`;
    }).join('');
  } else {
    box.innerHTML = (names || []).map(c => {
      const taken = debugNameTaken(kind, c);
      const on = keep.has(c) && !taken;
      return `<label class="pause-check${taken?' taken':''}"><input type="checkbox" value="${escapeHtml(c)}" ${on?'checked':''} ${taken || operatorState.debug.busy ? 'disabled' : ''}> ${escapeHtml(c)}</label>`;
    }).join('');
  }
  box.querySelectorAll('input').forEach(el => { el.onchange = updateDebugApplyEnabled; });
}

function debugScopeLabel(h) {
  if (!h) return '';
  const bits = [];
  if (Array.isArray(h.clients) && h.clients.length) bits.push(h.clients.join(', '));
  if (Array.isArray(h.providers) && h.providers.length) bits.push(h.providers.join(', '));
  if (Array.isArray(h.models) && h.models.length) bits.push(h.models.join(', '));
  return bits.join(' + ') || 'session';
}

function debugRanLabel(h) {
  const ms = typeof h.ran_ms === 'number' ? h.ran_ms : 0;
  if (ms <= 0) return '';
  const m = Math.max(1, Math.round(ms / 60000));
  if (m < 60) return m + 'm';
  const hr = Math.floor(m / 60), rem = m % 60;
  return rem ? hr + 'h ' + rem + 'm' : hr + 'h';
}

function renderDebugSessions() {
  const box = $('df-holds');
  if (!box) return;
  const holds = liveDebugSessions();
  if (!holds.length) { box.innerHTML = ''; return; }
  box.innerHTML = '<div class="pause-known-hd">Active sessions</div>' + holds.map(h => {
    const left = [debugScopeLabel(h)];
    const until = pauseUntilLabel(h.until);
    if (until) left.push(until);
    else if (h.duration) left.push(h.duration);
    else left.push('until stop');
    const ran = debugRanLabel(h);
    if (ran) left.push('ran ' + ran);
    const n = typeof h.captures === 'number' ? h.captures : 0;
    left.push(n + ' req');
    const editing = debugEditID === h.id ? ' editing' : '';
    return `<div class="pause-hold${editing}"><button class="hold-edit" data-operator="debug-edit" data-value="${escapeHtml(h.id)}" ${operatorState.debug.busy ? 'disabled' : ''}>${escapeHtml(left.join(' · '))}</button>` +
      `<button class="btn" data-operator="debug-stop" data-value="${escapeHtml(h.id)}" ${operatorState.debug.busy ? 'disabled' : ''}>Stop</button></div>`;
  }).join('');
}

function refreshDebugKnownLists() {
  const pickedC = new Set(selectedDebugChecks('df-clients'));
  const pickedP = new Set(selectedDebugChecks('df-providers'));
  const pickedM = new Set(selectedDebugChecks('df-models'));
  const knownC = [...new Set([...(debugState.known_clients || []), ...pickedC])].sort();
  const knownP = [...new Set([...(debugState.known_providers || []), ...pickedP])].sort();
  const knownM = [...new Set([...(debugState.known_models || []), ...pickedM])].sort();
  fillDebugChecks($('df-clients'), knownC, pickedC, 'client');
  fillDebugChecks($('df-providers'), knownP, pickedP, 'provider');
  fillDebugChecks($('df-models'), knownM, pickedM, 'model');
  updateDebugApplyEnabled();
}

function syncDebugMenuState() {
  fillSelectPairs($('df-dur'), DEBUG_DURS, debugMenuDuration());
  if (debugEditID && !liveDebugSessions().some(h => h.id === debugEditID)) {
    debugEditID = '';
    resetDebugMenuForm();
  }
  refreshDebugKnownLists();
  const stop = $('btn-debug-stop');
  if (stop) stop.disabled = operatorState.debug.busy || !debugActive();
  if ($('df-dur')) $('df-dur').disabled = operatorState.debug.busy;
  const hd = document.querySelector('#debug-menu .clear-menu-hd');
  if (hd) {
    hd.textContent = debugEditID ? 'Edit session…' : (debugActive() ? 'Capturing debug…' : 'Capture debug…');
  }
  const count = $('debug-count');
  if (count && !count.dataset.err) {
    if (debugActive()) {
      const bits = ['debug'];
      const live = liveDebugSessions();
      if (live.length === 1) bits.push(debugScopeLabel(live[0]));
      else if (live.length > 1) bits.push(live.length + ' sessions');
      const left = pauseUntilLabel(debugState.until);
      if (left) bits.push(left);
      count.textContent = bits.join(' · ');
    } else {
      count.textContent = '';
    }
  }
  renderDebugSessions();
  updateDebugApplyEnabled();
}

function resetDebugMenuForm() {
  debugEditID = '';
  fillSelectPairs($('df-dur'), DEBUG_DURS, '');
  if ($('df-dur')) $('df-dur').value = '';
  const count = $('debug-count');
  if (count) { count.textContent = ''; delete count.dataset.err; }
  fillDebugChecks($('df-clients'), [...new Set(debugState.known_clients || [])].sort(), new Set(), 'client');
  fillDebugChecks($('df-providers'), [...new Set(debugState.known_providers || [])].sort(), new Set(), 'provider');
  fillDebugChecks($('df-models'), [...new Set(debugState.known_models || [])].sort(), new Set(), 'model');
  updateDebugApplyEnabled();
}

function debugMatchesRecord(h, r) {
  if (!h || !r) return false;
  if ((h.clients || []).length && !(h.clients || []).includes(r.client || '')) return false;
  if ((h.providers || []).length && !(h.providers || []).includes(r.provider || '')) return false;
  if ((h.models || []).length && !(h.models || []).includes(r.model || '')) return false;
  return !!(h.clients || []).length || !!(h.providers || []).length || !!(h.models || []).length;
}

function editDebugForRecord(r) {
  const holds = liveDebugSessions();
  if (!holds.length || !r) return;
  const h = holds.find(x => debugMatchesRecord(x, r)) || (r.debug_session_id && holds.find(x => x.id === r.debug_session_id));
  if (!h) return;
  closeHeaderMenus('debug-menu');
  const m = $('debug-menu');
  if (m) m.hidden = false;
  syncHdrMenuExpanded();
  editDebugSession(h.id);
}

function editDebugSession(id) {
  if (!id || operatorState.debug.busy) return;
  if (debugEditID === id) {
    resetDebugMenuForm();
    syncDebugMenuState();
    return;
  }
  const h = liveDebugSessions().find(x => x.id === id);
  if (!h) return;
  debugEditID = id;
  fillSelectPairs($('df-dur'), DEBUG_DURS, h.duration || '');
  const count = $('debug-count');
  if (count) { count.textContent = ''; delete count.dataset.err; }
  fillDebugChecks($('df-clients'), [...new Set([...(debugState.known_clients || []), ...(h.clients || [])])].sort(), new Set(h.clients || []), 'client');
  fillDebugChecks($('df-providers'), [...new Set([...(debugState.known_providers || []), ...(h.providers || [])])].sort(), new Set(h.providers || []), 'provider');
  fillDebugChecks($('df-models'), [...new Set([...(debugState.known_models || []), ...(h.models || [])])].sort(), new Set(h.models || []), 'model');
  syncDebugMenuState();
}

function applyDebugMenu() {
  const clients = selectedDebugChecks('df-clients');
  const providers = selectedDebugChecks('df-providers');
  const models = selectedDebugModels();
  if (!clients.length && !providers.length && !models.length) return;
  const body = { enabled: true, clients, providers, models, duration: debugMenuDuration() };
  if (debugEditID) body.id = debugEditID;
  return mutateOperator('debug', body, resetDebugMenuForm);
}
function stopDebugSession(id) {
  if (!id || !debugActive()) return;
  return mutateOperator('debug', { enabled: false, id });
}
function stopDebugMenu() {
  if (!debugActive()) return;
  return mutateOperator('debug', { enabled: false }, () => { debugEditID = ''; hideHdrMenu('debug-menu'); });
}

// LIMIT_WINDOWS is the single owner of the Limits ▾ request/token window
// dropdown. Both selects are filled from this list (never a second HTML copy).
const LIMIT_WINDOWS = [
  ['1s', '1 second'], ['10s', '10 seconds'], ['30s', '30 seconds'],
  ['1m', '1 minute'], ['5m', '5 minutes'], ['15m', '15 minutes'],
  ['1h', '1 hour'], ['6h', '6 hours'], ['24h', '24 hours'],
];
function fillLimitWindows(sel, value) {
  fillSelectPairs(sel, LIMIT_WINDOWS, value || '1m');
}

function syncLimitsMenuState() {
  document.querySelectorAll('#limits-menu input, #limits-menu select').forEach(el => { el.disabled = operatorState.throttle.busy; });
  const known = [...new Set([
    ...(throttleState.known_providers || []),
    ...(throttleState.throttles || []).map(t => t.provider).filter(Boolean),
  ])].sort();
  const sel = $('lim-provider');
  const keep = sel ? sel.value : '';
  if (sel) {
    const empty = known.length ? '' : '<option value="">none yet - they appear as requests arrive</option>';
    sel.innerHTML = empty + known.map(p => `<option value="${escapeHtml(p)}">${escapeHtml(p)}</option>`).join('');
    if (keep && known.includes(keep)) sel.value = keep;
  }
  updateLimitCount();
  renderLimitHolds();
  updateLimitsApplyEnabled();
}

function onLimitProviderChange() {
  const name = $('lim-provider') ? $('lim-provider').value : '';
  const t = limitByProvider(name);
  const conc = $('lim-conc'), req = $('lim-req'), tok = $('lim-tok');
  if (conc) conc.value = t && t.concurrency ? String(t.concurrency) : '';
  if (req) req.value = t && t.requests ? String(t.requests) : '';
  if (tok) tok.value = t && t.tokens ? String(t.tokens) : '';
  fillLimitWindows($('lim-reqw'), t && t.request_window);
  fillLimitWindows($('lim-tokw'), t && t.token_window);
  const count = $('lim-count');
  if (count) delete count.dataset.err;
  updateLimitCount();
  updateLimitsApplyEnabled();
}

function updateLimitCount() {
  const count = $('lim-count');
  if (!count || count.dataset.err) return;
  const name = $('lim-provider') ? $('lim-provider').value : '';
  const t = limitByProvider(name);
  if (t) {
    const bits = [];
    if (t.source) bits.push(t.source === 'header' ? 'set by header' + (t.updated_by ? ' · ' + t.updated_by : '') : 'set in UI');
    if (t.in_flight) bits.push('in-flight ' + t.in_flight);
    if (t.queued) bits.push('queued ' + t.queued);
    if (t.requests && t.requests_capacity) bits.push(Math.max(0, Math.round(t.requests_remaining)) + '/' + Math.round(t.requests_capacity) + ' req');
    if (t.tokens && t.tokens_capacity) bits.push(Math.max(0, Math.round(t.tokens_remaining)) + '/' + Math.round(t.tokens_capacity) + ' tok');
    count.textContent = bits.join(' · ');
  } else {
    count.textContent = '';
  }
}

function updateLimitsApplyEnabled() {
  const hasProv = !!( $('lim-provider') && $('lim-provider').value );
  const apply = $('btn-limits-apply');
  const clear = $('btn-limits-clear');
  if (apply) apply.disabled = operatorState.throttle.busy || !hasProv;
  if (clear) clear.disabled = operatorState.throttle.busy || !hasProv || !limitByProvider($('lim-provider').value);
}

function limitNum(id) {
  const el = $(id);
  if (!el || el.value === '') return 0;
  const n = Number(el.value);
  return el.validity.valid && Number.isSafeInteger(n) && n >= 0 ? n : NaN;
}

function applyLimitsMenu() {
  const provider = $('lim-provider') ? $('lim-provider').value : '';
  if (!provider) return;
  const conc = limitNum('lim-conc');
  const req = limitNum('lim-req');
  const tok = limitNum('lim-tok');
  if ([conc, req, tok].some(Number.isNaN)) { operatorError('throttle', 'limits must be non-negative whole numbers'); return; }
  const body = {
    provider,
    concurrency: conc,
    requests: req,
    request_window: req > 0 ? ($('lim-reqw') ? $('lim-reqw').value : '1m') : '',
    tokens: tok,
    token_window: tok > 0 ? ($('lim-tokw') ? $('lim-tokw').value : '1m') : '',
  };
  return mutateOperator('throttle', body);
}

function clearLimitsMenu() {
  const provider = $('lim-provider')?.value;
  if (!provider) return;
  return mutateOperator('throttle', { provider, clear: true }, onLimitProviderChange);
}

function renderLimitHolds() {
  const box = $('lim-holds');
  if (!box) return;
  const rows = throttleState.throttles || [];
  if (!rows.length) { box.innerHTML = ''; return; }
  box.innerHTML = '<div class="pause-known-hd">Active limits</div>' + rows.map(t => {
    const bits = [];
    if (t.concurrency) bits.push('×' + t.concurrency);
    if (t.requests) bits.push(t.requests + '/' + (t.request_window || ''));
    if (t.tokens) bits.push(fmt(t.tokens) + ' tok/' + (t.token_window || ''));
    const src = t.source === 'header' ? 'header' : 'ui';
    const editing = ($('lim-provider') && $('lim-provider').value === t.provider) ? ' editing' : '';
    return `<div class="pause-hold${editing}"><button class="hold-edit" data-operator="limit-edit" data-value="${escapeHtml(t.provider)}" ${operatorState.throttle.busy ? 'disabled' : ''}>${escapeHtml(t.provider)} · ${escapeHtml(bits.join(' · '))}</button>` +
      `<span class="lim-src ${src}">${escapeHtml(src)}</span></div>`;
  }).join('');
}

function editLimitProvider(name) {
  if (!name || operatorState.throttle.busy) return;
  const sel = $('lim-provider');
  if (sel) {
    if (![...sel.options].some(o => o.value === name)) {
      sel.insertAdjacentHTML('beforeend', `<option value="${escapeHtml(name)}">${escapeHtml(name)}</option>`);
    }
    sel.value = name;
  }
  const count = $('lim-count');
  if (count) delete count.dataset.err;
  onLimitProviderChange();
  renderLimitHolds();
}

function editLimitForRecord(r) {
  if (!r || !r.provider) return;
  closeHeaderMenus('limits-menu');
  const m = $('limits-menu');
  if (m) m.hidden = false;
  syncHdrMenuExpanded();
  syncLimitsMenuState();
  editLimitProvider(r.provider);
}

function pauseUntilLabel(until) {
  if (!until) return '';
  const ms = new Date(until).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return '';
  const m = Math.ceil(ms / 60000);
  if (m < 60) return m + 'm left';
  const h = Math.floor(m / 60), rem = m % 60;
  return rem ? h + 'h ' + rem + 'm left' : h + 'h left';
}

// setFooterLive mirrors SSE connectivity. refreshFooterState combines that
// with the operator pause so the footer can say "paused" for a hold
// and "offline" when the live stream drops.
function setFooterLive(live) {
  streamLive = !!live;
  refreshFooterState();
}

function refreshFooterState() {
  const dot = $('f-live'), state = $('f-state');
  const holding = pauseActive();
  const debugging = debugActive();
  if (dot) dot.classList.toggle('paused', holding || debugging || !streamLive);
  const btn = $('btn-pause');
  if (btn) {
    btn.setAttribute('aria-pressed', holding ? 'true' : 'false');
    btn.setAttribute('aria-label', holding ? 'Paused' : 'Pause');
    btn.title = holding ? 'Paused' : 'Pause';
  }
  const dbtn = $('btn-debug');
  if (dbtn) {
    dbtn.setAttribute('aria-pressed', debugging ? 'true' : 'false');
    dbtn.setAttribute('aria-label', debugging ? 'Debugging' : 'Debug');
    dbtn.title = debugging ? 'Debugging' : 'Debug';
  }
  const resume = $('btn-pause-resume');
  if (resume) resume.disabled = operatorState.pause.busy || !holding;
  const dstop = $('btn-debug-stop');
  if (dstop) dstop.disabled = operatorState.debug.busy || !debugging;
  if (state) {
    if (holding) {
      const bits = ['paused'];
      const live = liveHolds();
      if (live.length === 1) bits.push(holdScopeLabel(live[0]));
      else if (live.length > 1) bits.push(live.length + ' holds');
      if (pauseState.queued > 0) bits.push('queued ' + pauseState.queued);
      const left = pauseUntilLabel(pauseState.until);
      if (left) bits.push(left);
      state.textContent = bits.join(' · ');
    } else if (debugging) {
      const bits = ['debug'];
      const live = liveDebugSessions();
      if (live.length === 1) bits.push(debugScopeLabel(live[0]));
      else if (live.length > 1) bits.push(live.length + ' sessions');
      const left = pauseUntilLabel(debugState.until);
      if (left) bits.push(left);
      state.textContent = bits.join(' · ');
    } else {
      state.textContent = streamLive ? 'live' : 'offline';
    }
    const dropped = storageState?.enabled ? storageState.dropped : 0;
    state.classList.toggle('storage-warning', dropped > 0);
    state.title = dropped > 0 ? 'Records not saved durably since this process started (queue overflow, closed store, or write failure). Some may remain in the live ring.' : '';
    if (dropped > 0) state.textContent += ' · ' + fmt(dropped) + ' storage drops';
  }
}

function doFilter() {
  filters.status = $('f-status').value;
  storage.set('dash.filters', JSON.stringify(filters)); // persist across reloads
  renderAll(lastData);
  fetchChart();      // chart is scoped to status + explorer filters
  // The shared request gate cancels an obsolete scan immediately and
  // dedupes repeated selections; no delay or second debounce owner.
  fetchExplorer();
}

// ---------- safe delete (Clear ▾ menu) ----------
// The Clear button opens a menu to delete all records or a filtered subset
// (by provider / model / client / status / errors / age), with a live count
// preview before anything is deleted. Every request is one self-contained row
// (retries live in the row's attempts JSON), so a filtered delete can never
// leave orphan entries. The Logs ▾ menu reuses the same filter options (and
// the same backend count endpoint) for choosing exactly what to download.

function toggleFilterMenu(e, menuId, prefix, refresh) {
  e.stopPropagation();
  const m = $(menuId);
  if (m.hidden) { closeHeaderMenus(menuId); populateFilterMenu(prefix); m.hidden = false; refresh(); }
  else m.hidden = true;
  syncHdrMenuExpanded();
}
function toggleClearMenu(e) { toggleFilterMenu(e, 'clear-menu', 'cf', updateClearCount); }
// Use the dispatch path: a delegated action may replace/detach its target
// before this later document listener runs. Live ancestry then lies about
// where the click originated; the composed event path remains stable.
document.addEventListener('click', e => {
  const path = e.composedPath();
  const inside = selector => path.some(node => node.matches?.(selector));
  if (!inside('.menu-wrap')) closeHeaderMenus();
  if (!inside('#hdr-actions') && !inside('#btn-nav')) closeNavMenu();
  if (!inside('.xp-rail-col')) closeDimMenu();
});

// populateFilterMenu fills a filter menu's dropdowns from the current record
// set (shared by the Clear and Logs menus - same options, same values).
const FILTER_ERRORS = [['', 'any'], ['1', 'only errors']];
const FILTER_DEBUG = [['', 'any'], ['1', 'only debug']];
const FILTER_AGES = [['', '-'], ['1h', '1 hour'], ['24h', '1 day'], ['168h', '1 week']];

function populateFilterMenu(prefix) {
  const recs = lastData?.records || [];
  const fill = (id, vals) => {
    const el = $(id);
    const cur = el.value;
    fillSelectPairs(el, [['', 'any']].concat(vals.map(v => [v, String(v)])), cur);
  };
  fill(prefix+'-provider', [...new Set(recs.map(r => r.provider).filter(Boolean))].sort());
  fill(prefix+'-client', [...new Set(recs.map(r => r.client).filter(Boolean))].sort());
  fill(prefix+'-status', [...new Set(recs.map(r => r.status_code))].sort((a,b)=>a-b).map(String));
  // The model select groups spelling variants under their canonical group
  // (an <optgroup> per multi-variant group) so the same model clusters
  // visually - but each <option> keeps its raw value: Clear/Logs filter on
  // the exact stored spelling, and the grouping never widens a delete.
  {
    const el = $(prefix+'-model');
    const cur = el.value;
    const raws = [...new Set(recs.map(r => r.model).filter(Boolean))].sort();
    const byGroup = new Map();
    for (const m of raws) {
      const g = canonicalModel(m);
      if (!byGroup.has(g)) byGroup.set(g, []);
      byGroup.get(g).push(m);
    }
    const groups = [...byGroup.entries()].sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
    let html = '<option value="">any</option>';
    let curPresent = cur === '';
    for (const [g, variants] of groups) {
      const opts = variants.map(m => {
        if (m === cur) curPresent = true;
        return `<option value="${escapeHtml(m)}">${escapeHtml(m)}</option>`;
      }).join('');
      html += (variants.length > 1)
        ? `<optgroup label="${escapeHtml(g)} (${variants.length})">${opts}</optgroup>`
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
  const onCh = prefix === 'cf' ? updateClearCount : updateLogsCount;
  [prefix+'-provider', prefix+'-model', prefix+'-client', prefix+'-status', prefix+'-errors', prefix+'-debug', prefix+'-age'].forEach(id => { $(id).onchange = onCh; });
}

// filterFromUI reads a menu (by id prefix: cf = clear, lf = logs) into a
// filter object matching the backend PurgeFilter JSON shape.
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

// updateFilterCount POSTs /admin/purge/count for a Clear/Logs menu and
// writes the preview into elId / enables btnId. phrase(count) is the label.
// Each menu owns its latest preview, including the exact older-than instant.
// Empty selections invalidate it too. Actions reuse this predicate, never a
// newly calculated Date.now() that would widen the confirmed deletion.
const _filterPreviews = new Map();
let _purgeBusy = false;
function filterSelection(prefix) { return JSON.stringify(filterFromUI(prefix, 0)); }
function previewedFilter(prefix) {
  const preview = _filterPreviews.get(prefix);
  return preview && preview.count > 0 && preview.selection === filterSelection(prefix) ? preview : null;
}
async function updateFilterCount(prefix, elId, btnId, phrase) {
  const f = filterFromUI(prefix);
  const el = $(elId);
  const preview = {filter: f, selection: filterSelection(prefix), count: null};
  _filterPreviews.set(prefix, preview);
  $(btnId).disabled = true;
  if (!clearFilterActive(f)) { el.textContent = ''; return; }
  el.textContent = 'counting…';
  try {
    const res = await operatorFetch('/admin/purge/count', { method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(f) });
    const d = await res.json();
    if (_filterPreviews.get(prefix) !== preview || preview.selection !== filterSelection(prefix)) return;
    if (!res.ok || !Number.isSafeInteger(d.count) || d.count < 0) throw new Error(d.error || 'count unavailable');
    preview.count = d.count;
    el.textContent = phrase(d.count);
    $(btnId).disabled = d.count === 0 || prefix === 'cf' && _purgeBusy;
  } catch (e) {
    if (_filterPreviews.get(prefix) === preview && preview.selection === filterSelection(prefix)) {
      el.textContent = String(e.message || 'count unavailable');
    }
  }
}
function recPhrase(n) { return fmt(n) + ' record' + (n === 1 ? '' : 's'); }
function updateClearCount() {
  return updateFilterCount('cf', 'clear-count', 'btn-clear-filtered', n => 'will delete ' + recPhrase(n));
}

// clearFiltered deletes only the records matching the chosen filter.
async function clearFiltered() {
  if (_purgeBusy) return;
  const preview = previewedFilter('cf');
  if (!preview) return updateClearCount();
  if (!confirm(`Delete ${recPhrase(preview.count)} matching the selected filter?\n\nThis cannot be undone.`)) return;
  return purgeMetrics(preview.filter);
}

// clearAll wipes everything (the original Clear behavior).
async function clearAll() {
  if (_purgeBusy) return;
  const n = (lastData?.records || []).length;
  if (!confirm(`Permanently delete ALL metrics history?\n\n${fmt(n)} records in view + the entire database will be wiped. This cannot be undone.`)) return;
  return purgeMetrics(null);
}

// Confirmed success is the only path that clears UI state. The same gate
// handles full/filtered deletion, disables repeat submits, and reports failures.
async function purgeMetrics(filter) {
  if (_purgeBusy) return;
  _purgeBusy = true;
  $('btn-clear-filtered').disabled = true;
  $('btn-clear-all').disabled = true;
  document.querySelectorAll('#clear-menu select').forEach(el => { el.disabled = true; });
  try {
    const options = {method: 'POST'};
    if (filter) { options.headers = {'Content-Type': 'application/json'}; options.body = JSON.stringify(filter); }
    const response = await operatorFetch('/admin/purge', options);
    const result = await response.json();
    if (!response.ok || result.ok !== true) throw new Error(result.error || 'could not delete records');
    _filterPreviews.clear();
    $('btn-logs-filtered').disabled = true;
    hideHdrMenu('clear-menu');
    closeDrawer(); lastRender = {};
    lastSeq = 0;
    // No removal event exists: the forced full bootstrap is the post-purge
    // render, superseding any stale in-flight tick. No optimistic local wipe.
    refreshAggregates();
    fetchBootstrap('full');
  } catch (err) {
    $('clear-count').textContent = String(err.message || 'could not delete records');
  } finally {
    _purgeBusy = false;
    $('btn-clear-all').disabled = false;
    $('btn-clear-filtered').disabled = !previewedFilter('cf');
    document.querySelectorAll('#clear-menu select').forEach(el => { el.disabled = false; });
  }
}

// refreshAggregates pulls the since-inception numeric surfaces now so a
// purge (or any wipe) is visible before the next tick. The KPI aggregate
// rides the bootstrap payload; explorer includes the scoped footer counts.
function refreshAggregates() {
  fetchChart('invalidate'); fetchExplorer('invalidate');
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
  fetchSettings();
}

// ---------- log export (Logs ▾ menu) ----------
// Pick exactly what to download: the same filter dimensions as Clear (provider
// / model / client / status / errors / age), applied and previewed against
// the full database via the shared count endpoint - the export then streams
// the same rows the preview announced. "Download all" exports everything.

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
  const a = document.createElement('a');
  a.href = logsExportURL(preview.filter);
  a.download = '';
  document.body.appendChild(a); a.click(); a.remove();
  hideHdrMenu('logs-menu');
}

function downloadLogsAll() {
  const a = document.createElement('a');
  a.href = '/metrics/export';
  a.download = '';
  document.body.appendChild(a); a.click(); a.remove();
  hideHdrMenu('logs-menu');
}

// ---- Restart (rebuild & graceful handoff) ----
// POST /admin/restart rebuilds from source and hands the listening socket to
// a fresh process. In-flight streams finish on the old process; new requests
// queue in the kernel backlog until the child serves - nothing is refused.
// The client polls GET /admin/restart until `started_at` changes, which can
// only come from the fresh process.

let restartBusy = false;
let restartFailed = false;
let restartServerState = {};

function restartInProgress() {
  return restartBusy || !!(restartServerState.phase && restartServerState.phase !== 'idle');
}

// The local lock covers preflight/POST/polling; server state also covers a
// restart started in another tab. Status refreshes cannot unlock a local run.
function syncRestartControls() {
  const active = restartInProgress();
  const btn = $('btn-restart-now');
  if (btn) {
    btn.disabled = active || restartServerState.available === false;
    btn.textContent = active ? 'Restarting…' : 'Restart now';
    btn.setAttribute('aria-busy', String(active));
  }
  const trigger = $('btn-restart');
  if (trigger) {
    // Keep the menu accessible so progress can be reopened during a restart.
    if (active) trigger.dataset.busy = '1'; else delete trigger.dataset.busy;
    trigger.title = active ? 'Restarting - view progress' : 'Restart';
    trigger.setAttribute('aria-label', trigger.title);
  }
}

// RESTART_STEPS is the visible choreography. `rank` matches the server's
// phaseRank: a reported phase marks every step before it done, itself active
// - the list is rendered the moment each step actually starts (the watch
// stream), never pre-claimed.
const RESTART_STEPS = [
  { rank: 0, label: 'rebuild from source' },
  { rank: 1, label: 'finish in-flight streams' },
  { rank: 2, label: 'flush metrics to disk' },
  { rank: 3, label: 'start the new process' },
];

// renderRestartSteps paints the step list from a status document
// {rank, phase, drain_elapsed_ms, drain_timeout_ms, error}. Steps below rank
// are done, the ranked one is active (with the live drain timer), the rest
// pending; an error marks the active step failed.
function renderRestartSteps(st) {
  const ol = $('restart-steps');
  if (!ol) return;
  if (!st || st.rank == null || st.rank < 0) { ol.hidden = true; ol.innerHTML = ''; return; }
  ol.hidden = false;
  const fail = !!(st.error);
  ol.innerHTML = RESTART_STEPS.map(s => {
    const state = s.rank < st.rank ? 'done' : (s.rank === st.rank ? (fail ? 'failed' : 'active') : 'pending');
    let sub = '';
    if (state === 'active' && s.rank === 1 && st.drain_timeout_ms > 0) {
      sub = `<span class="rs-sub">${fmtDur(Math.round((st.drain_elapsed_ms || 0) / 1000) * 1000)} / ${fmtDur(st.drain_timeout_ms)}</span>`;
    }
    return `<li class="rs-step ${state}"><span class="rs-dot" aria-hidden="true"></span><span class="rs-label">${s.label}</span>${sub}</li>`;
  }).join('');
}

function toggleRestartMenu(e) {
  e.stopPropagation();
  const m = $('restart-menu');
  if (m.hidden) { closeHeaderMenus('restart-menu'); m.hidden = false; fetchRestartStatus(); }
  else m.hidden = true;
  syncHdrMenuExpanded();
}

function setRestartStatus(text, err) {
  const el = $('restart-count');
  if (!el) return;
  el.textContent = text || '';
  if (err) el.dataset.err = '1'; else delete el.dataset.err;
}

function fetchRestartStatus() {
  return operatorFetch('/admin/restart')
    .then(r => r.json())
    .then(st => {
      applyRestartEvent(st);
      if (st && st.available === false) {
        setRestartStatus(st.reason || 'restart unavailable', true);
      }
      return st || {};
    })
    .catch(() => { setRestartStatus('could not check restart status', true); return {}; });
}

// watchRestart opens the NDJSON step stream (GET /admin/restart?watch=1) and
// calls onEvent per status document. It resolves true when the stream was
// established (hijacked server-side, so it survives the drain that hangs
// ordinary polls); false when streaming is unsupported - the caller then
// falls back to polling. The stream ends when the old process exits (success)
// or the registry closes it (failure) - the caller detects both by the
// reader finishing.
async function watchRestart(onEvent, signal) {
  let reader;
  try {
    const r = await operatorFetch('/admin/restart?watch=1', {signal});
    if (!r.ok || !r.body || !r.body.getReader) return false;
    const dec = new TextDecoder();
    let buf = '';
    reader = r.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return true;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try { onEvent(JSON.parse(line)); } catch (e) { /* malformed line: skip */ }
      }
    }
  } catch (e) {
    return false;
  } finally {
    if (reader) await reader.cancel().catch(() => {});
  }
}

async function restartProxy() {
  if (restartInProgress()) return;
  const beforeFeed = feedId;
  const watcher = new AbortController();
  // Lock before the first await: rapid clicks must not start two preflights.
  restartBusy = true;
  syncRestartControls();
  try {
    const before = await fetchRestartStatus();
    if (before.available === false || before.phase !== 'idle') return;
    if (!confirm('Rebuild from source and restart the proxy?\n\nIn-flight streams finish; new requests queue until the fresh process takes over.')) return;
    restartFailed = false;
    setRestartStatus('');
    renderRestartSteps(null);
    // Open the step stream before the POST: after the commit point the old
    // process stops accepting, so polls hang in the backlog - the hijacked
    // watch connection is already accepted and streams every real transition.
    // Apply each NDJSON line as it arrives (do not wait for the stream to
    // end - that hid draining behind "building" for the whole drain).
    watchRestart(st => { if (!watcher.signal.aborted) applyRestartEvent(st); }, watcher.signal);
    const r = await operatorFetch('/admin/restart', { method: 'POST' });
    const doc = await r.json().catch(() => ({}));
    if (!r.ok) { setRestartStatus(doc.error || 'restart failed (' + r.status + ')', true); return; }
    if (restartFailed) return;
    applyRestartEvent(doc);
    // A fast failed build can already be idle in the POST response, before
    // this client has observed its active phase on the watch connection.
    if (doc.phase === 'idle' && doc.error) return;
    // Poll in parallel with the watch: streaming drives the step list live
    // (including drain); polls detect the fresh process via started_at and
    // are the fallback when the client cannot stream. Drain polls hang, so
    // each is raced against 5s.
    const deadline = Date.now() + 300000;
    const pollDone = (async () => {
      while (Date.now() < deadline) {
        await new Promise(res => setTimeout(res, 750));
        if (restartFailed) return null;
        const poll = new AbortController();
        const timeout = setTimeout(() => poll.abort(), 5000);
        const st = await operatorFetch('/admin/restart', {signal: poll.signal}).then(x => x.json()).catch(() => null).finally(() => clearTimeout(timeout));
        if (restartFailed) return null;
        if (st && st.started_at && before.started_at && st.started_at !== before.started_at) {
          return st;
        }
        if (st && st.phase) applyRestartEvent(st);
      }
      return null;
    })();
    const st = await pollDone;
    if (restartFailed) return;
    if (st && st.started_at) {
      applyRestartEvent(st);
      renderRestartSteps({ rank: RESTART_STEPS.length, error: '' });
      setRestartStatus('restarted ✓ (pid ' + st.pid + ')');
      // SSE normally detects the new feed first. If it is disconnected,
      // check fresh assets/state immediately through the same bootstrap gate.
      if (feedId === beforeFeed) fetchBootstrap('resume');
      setTimeout(() => { hideHdrMenu('restart-menu'); setRestartStatus(''); const ol = $('restart-steps'); if (ol) { ol.hidden = true; ol.innerHTML = ''; } }, 2500);
      return;
    }
    setRestartStatus('restart did not complete in time', true);
  } catch (err) {
    setRestartStatus('restart request failed - ' + err.message, true);
  } finally {
    watcher.abort();
    restartBusy = false;
    syncRestartControls();
  }
}

// applyRestartEvent folds one status document into the step list. An idle
// phase with an error is a failed run (the watch stream just ended); idle
// without error during the wait is the fresh child answering - completion
// is handled by started_at, never by phase. Phase names live on the
// checklist, never the yellow count line.
function applyRestartEvent(st) {
  if (!st) return;
  const wasActive = restartServerState.phase && restartServerState.phase !== 'idle';
  restartServerState = { ...restartServerState, ...st };
  syncRestartControls();
  if (!st.phase) return;
  if (st.phase === 'idle') {
    if (st.error) {
      // The watch's initial idle snapshot may carry a previous build error.
      // Only an active → idle transition can fail the run now being watched.
      if (wasActive) restartFailed = true;
      renderRestartSteps(st);
      setRestartStatus('restart failed - ' + st.error, true);
    } else if (!restartBusy) {
      renderRestartSteps(null);
      setRestartStatus('');
    }
    return;
  }
  setRestartStatus('');
  renderRestartSteps(st);
}
