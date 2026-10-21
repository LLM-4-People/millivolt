// Storm detection and recovery belong to the server. Accepted bootstrap
// snapshots replace this bounded status surface and its open detail dialog.
// Incident buttons retain their identity across updates for keyboard focus.
let stormSnapshot = {valid: false, enabled: false, storms: []};
let stormDialogKey = '';
let quotaResumeError = '';
const stormIncidentKey = s => JSON.stringify([s.provider, s.scope, s.model]);
function applyStormState(st) {
  const banner = $('storm-banner');
  if (!banner) return;
  const valid = s => s && typeof s.provider === 'string' && s.provider.length > 0 &&
    typeof s.model === 'string' && (s.scope === 'provider' && !s.model || s.scope === 'model') &&
    ['open', 'half_open'].includes(s.state) && typeof s.reason === 'string' &&
    typeof s.quota === 'boolean' && (!s.quota || s.scope === 'provider') &&
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
    const detail = s.quota
      ? `<span class="storm-reason">${escapeHtml(s.reason)}</span><span>Quota pause</span><span>${fmt(s.queued)} queued</span><span>${fmt(s.recovery_successes)} / ${fmt(s.recovery_required)} recovery probes</span>`
      : `<span class="storm-reason">${escapeHtml(s.reason)}</span><span>${escapeHtml(pctValue(s.error_percent, 1, true))} failed attempts</span><span>${fmt(s.error_requests)} requests affected</span><span>${fmt(s.queued)} queued</span>`;
    const html = `<span class="storm-heading"><strong>${escapeHtml(scope)}</strong><span class="storm-state">${s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'}</span><span class="storm-open-hint">View details ›</span></span><span class="storm-detail">${detail}</span>`;
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

// buildModalDialog is the one modal-frame constructor: every overlay
// dialog builds the same inert-while-hidden, focusable section and
// appends it to the body exactly once.
function buildModalDialog(id, className, labelledBy, innerHTML) {
  const dialog = document.createElement('section');
  dialog.id = id;
  dialog.className = className;
  dialog.hidden = true;
  dialog.tabIndex = -1;
  dialog.setAttribute('inert', '');
  dialog.setAttribute('role', 'dialog');
  dialog.setAttribute('aria-modal', 'true');
  dialog.setAttribute('aria-labelledby', labelledBy);
  dialog.innerHTML = innerHTML;
  document.body.appendChild(dialog);
  return dialog;
}

// wireDialogDismiss attaches the two dismiss paths every modal frame
// shares: a click on the frame itself and the Escape key. The dismiss
// callback owns what closing means for its dialog.
function wireDialogDismiss(dialog, dismiss) {
  dialog.addEventListener('click', e => { if (e.target === dialog) dismiss(e); });
  dialog.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); dismiss(e); }
  });
}

function openStormDetails(key) {
  if (!stormSnapshot.enabled || !stormSnapshot.storms.some(s => stormIncidentKey(s) === key)) return;
  let dialog = $('storm-dialog');
  if (!dialog) {
    dialog = buildModalDialog('storm-dialog', 'storm-dialog', 'storm-dialog-title', `<div class="storm-dialog-panel"><div class="drawer-hd"><h3 id="storm-dialog-title">Error storm details</h3><button class="btn" type="button" data-operator="storm-close" aria-label="Close error storm details">Close</button></div><div class="storm-dialog-body" id="storm-dialog-body"></div></div>`);
    wireDialogDismiss(dialog, () => closeStormDetails());
  }
  closeHeaderMenus();
  closeNavMenu();
  closeDimMenu();
  stormDialogKey = key;
  quotaResumeError = '';
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
  const title = $('storm-dialog-title');
  const incident = stormSnapshot.storms.find(s => stormIncidentKey(s) === stormDialogKey);
  if (!stormSnapshot.valid || !stormSnapshot.enabled || !incident) {
    const message = !stormSnapshot.valid ? 'Current incident details are unavailable.' :
      !stormSnapshot.enabled ? 'Error storm protection is disabled.' : 'This incident is no longer active.';
    if (title) title.textContent = 'Error storm details';
    updateSection('storm-dialog-body', `<p class="storm-resolution" role="status">${message}</p>`);
    return;
  }
  const s = incident;
  if (title) title.textContent = s.quota ? 'Provider quota pause' : 'Error storm details';
  const entries = s.quota ? [
    ['Provider', s.provider], ['Model scope', 'All models'],
    ['State', s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'], ['Error', s.reason],
    ['Queued requests', fmt(s.queued)], ['Next retry', new Date(s.retry_at).toLocaleString()],
    ['Successful recovery probes', s.recovery_successes + ' / ' + s.recovery_required],
  ] : [
    ['Provider', s.provider], ['Model scope', s.scope === 'provider' ? 'All models' : s.model || '(unspecified model)'],
    ['State', s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'], ['Error', s.reason],
    ['Requests affected', fmt(s.error_requests)], ['Failed upstream attempts', fmt(s.failures)],
    ['Sampled upstream attempts', fmt(s.samples)], ['Upstream attempt failure rate', pctValue(s.error_percent, 1, true)],
    ['Detection window', fmtDur(s.window_ms)], ['Queued requests', fmt(s.queued)],
    ['Next retry', new Date(s.retry_at).toLocaleString()],
    ['Successful recovery probes', s.recovery_successes + ' / ' + s.recovery_required],
  ];
  if (!s.quota && s.scope === 'provider') entries.push(['Active models', fmt(s.active_models)],
    ['Affected active models', fmt(s.affected_models) + ' / ' + fmt(s.active_models) + ' (' + pctValue(s.affected_model_percent, 1, true) + ')']);
  const rows = entries.map(([label, value]) => `<div><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(value)}</dd></div>`).join('');
  const explanation = s.quota
    ? 'This provider is paused after a durable quota/billing error (for example insufficient credits). Every new request parks here and one recovery probe is admitted per cooldown - a provider Retry-After on the quota response paces it like any other 429. The first successful probe releases the queue; Resume now closes the gate immediately.'
    : 'Counts cover the rolling detection window. Each request with a selected failure in this window is counted once; retries count as separate upstream attempts. Recovery checks use queued requests, so the next retry may start after the scheduled time.';
  const controls = s.quota
    ? `<div class="storm-quota-actions"><button class="btn" type="button" data-operator="quota-resume" data-value="${escapeHtml(s.provider)}">Resume now</button>${quotaResumeError ? `<span class="storm-resolution" role="alert">${escapeHtml(quotaResumeError)}</span>` : ''}</div>` : '';
  updateSection('storm-dialog-body', `<dl class="storm-facts">${rows}</dl><p class="storm-explanation">${explanation}</p>${controls}`);
}

// resumeQuotaProvider is the retry-mode quota gate's operator action: the
// confirmed response is the shared storm snapshot, so the banner and dialog
// repaint from the same authoritative state; a failure keeps the dialog open
// with the message beside the button.
async function resumeQuotaProvider(provider) {
  if (!provider) return;
  try {
    const st = await operatorJsonBody('/admin/quota', {
      method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({provider, resume: true}),
    }, 'could not resume ' + provider);
    if (!st || !Array.isArray(st.storms)) throw new Error('unexpected resume response');
    quotaResumeError = '';
    applyStormState(st);
  } catch (err) {
    quotaResumeError = String(err.message || 'resume failed');
  }
  renderStormDetails();
}

// Operator credential owner for the gated plane. The server never issues or
// returns the credential; gated calls attach it as a Bearer header and one
// 401 opens the prompt below. sessionStorage keeps the value tab-scoped: it
// never outlives the browser session, and it is dropped as soon as the
// server rejects it.
const OPERATOR_KEY = 'millivolt.operatorToken';
let operatorCredential = '';
try { operatorCredential = sessionStorage.getItem(OPERATOR_KEY) || ''; } catch (e) { operatorCredential = ''; }
// Bumped on every stored/cleared credential so an in-flight request can tell
// that another call's unlock landed while it was waiting for its response.
let operatorCredentialEpoch = 0;
// Set when the server rejected the presented credential: the next prompt
// says so instead of silently asking the same question again.
let operatorRejected = false;
let operatorPrompt = null;
// The operator dialog frame's dismiss paths dispatch through this hook;
// every prompt's executor re-points it at its own finish() so a reused
// frame always closes the prompt that opened it.
let operatorDismiss = () => {};

function storeOperatorCredential(value) {
  operatorCredential = value;
  ++operatorCredentialEpoch;
  try {
    if (value) sessionStorage.setItem(OPERATOR_KEY, value);
    else sessionStorage.removeItem(OPERATOR_KEY);
  } catch (e) { /* storage denied: tab-only credential */ }
}

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
  const epoch = operatorCredentialEpoch;
  let response = await attempt();
  if (response.status !== 401) return response;
  // A credential stored while this attempt was in flight (another gated
  // call's unlock, whose own retried response already validated it) is
  // reused silently: a slow chart/explorer/log scan whose 401 lands after
  // the dialog was answered must not open it again. If this retry is also
  // rejected the credential was wrong or rotated after all - drop it so
  // other in-flight callers stop retrying with it (each presented-but-wrong
  // request burns the server's throttle), flag the notice, and fall through
  // to the prompt.
  if (operatorCredential && operatorCredentialEpoch !== epoch) {
    const presented = operatorCredential;
    response = await attempt();
    if (response.status !== 401) {
      if (response.ok) operatorRejected = false; // an accepted response clears any racing rejection notice
      return response;
    }
    operatorRejected = true;
    // Wipe only the credential this retry presented: a 401 indicts the
    // presented value, never whatever a newer unlock stored meanwhile.
    if (operatorCredential === presented) storeOperatorCredential('');
  }
  const token = await askOperatorToken();
  if (!token) return response;
  storeOperatorCredential(token);
  response = await attempt();
  if (response.status === 401) {
    operatorRejected = true;
    // Same value guard: a later prompt generation may have stored a
    // different token while this retry was in flight.
    if (operatorCredential === token) storeOperatorCredential('');
  } else if (response.ok) {
    operatorRejected = false; // a successful unlock clears any racing rejection notice
  }
  return response;
}

// operatorJson is the one GET-and-parse owner for operator-state JSON
// fetches: it resolves the parsed document only for a 2xx response and
// rejects otherwise, so every caller shares one failure contract.
function operatorJson(url, options = {}) {
  return operatorFetch(url, options).then(r => r.ok ? r.json() : Promise.reject());
}

// operatorErrorBody is the shared failure read for an operator JSON call:
// it drains the response body and rejects with its error field, falling
// back to the caller's message. The fallback may be a function of the
// response when the message needs the status code. The rejection carries
// the operatorBody marker so a caller with its own transport handling (the
// restart control) can tell the server's answer from a network failure.
async function operatorErrorBody(r, fallback) {
  let msg = typeof fallback === 'function' ? fallback(r) : fallback;
  try { msg = (await r.json()).error || msg; } catch (e) {}
  const err = new Error(msg);
  err.operatorBody = true;
  throw err;
}

// operatorJsonBody is the fetch-and-parse owner for operator JSON calls
// that must read the failure body: it resolves the parsed document only
// for a 2xx response and otherwise rejects through operatorErrorBody
// (the server's error field, or the caller's fallback). A malformed 2xx
// body resolves to {} so the caller's shape checks reject it with their
// designed message instead of a parser's.
async function operatorJsonBody(url, options = {}, fallback = '') {
  const r = await operatorFetch(url, options);
  if (!r.ok) return operatorErrorBody(r, fallback);
  return r.json().catch(() => ({}));
}

// JSON_HEADERS is the one JSON content-type header pair for operator JSON
// POSTs. operatorFetch copies options.headers per call, so one shared
// object instance cannot be mutated by a caller.
const JSON_HEADERS = { 'Content-Type': 'application/json' };

// brandLogoSVG clones the header's brand mark for a second surface (the
// operator sign-in dialog): the mark's geometry stays authored once, in
// index.html. The clone's gradient gets a fresh id - two identical ids in
// one document would be invalid, and the trace stroke must reference the
// clone's own defs.
function brandLogoSVG(gradientId) {
  const svg = document.querySelector('header .brand-logo svg');
  if (!svg) return '';
  const clone = svg.cloneNode(true);
  const grad = clone.querySelector('linearGradient');
  if (grad && grad.id) {
    const old = grad.id;
    grad.id = gradientId;
    clone.querySelectorAll(`[stroke="url(#${old})"]`).forEach(el => el.setAttribute('stroke', `url(#${gradientId})`));
  }
  return clone.outerHTML;
}

// Single-flight credential prompt: concurrent gated calls share one dialog
// and one resolution. Resolves '' on cancel or dismiss. A prompt after a
// rejected attempt says so instead of repeating the intro line.
// OPERATOR_DIALOG_NOTE is the one intro sentence, spelled identically in the
// built dialog and the reset branch that restores it.
const OPERATOR_DIALOG_NOTE = 'This dashboard is protected. Enter the MILLIVOLT_OPERATOR_TOKEN value. It stays in this browser tab for the session.';
function askOperatorToken() {
  if (operatorPrompt) return operatorPrompt;
  operatorPrompt = new Promise(resolve => {
    let dialog = $('operator-dialog');
    if (!dialog) {
      dialog = buildModalDialog('operator-dialog', 'operator-dialog', 'operator-dialog-title', `<div class="operator-dialog-panel"><div class="operator-dialog-brand"><div class="brand-logo" aria-hidden="true">${brandLogoSVG('mv-login')}</div><h3 id="operator-dialog-title">millivolt</h3></div><p class="operator-dialog-note">${OPERATOR_DIALOG_NOTE}</p><form id="operator-dialog-form"><input id="operator-dialog-input" type="password" autocomplete="current-password" spellcheck="false" aria-label="Operator token" placeholder="operator token"><div class="operator-dialog-actions"><button class="btn" type="button" data-operator-auth="cancel">Cancel</button><button class="btn btn-accent" type="submit">Sign in</button></div></form></div>`);
      wireDialogDismiss(dialog, () => operatorDismiss(''));
    }
    const input = $('operator-dialog-input');
    const note = dialog.querySelector('.operator-dialog-note');
    if (operatorRejected) {
      operatorRejected = false;
      // Same lead sentence as the server login page's rejection notice.
      note.textContent = 'That token was rejected. Enter the current MILLIVOLT_OPERATOR_TOKEN value and try again.';
      note.setAttribute('data-err', '');
    } else {
      note.textContent = OPERATOR_DIALOG_NOTE;
      note.removeAttribute('data-err');
    }
    // The dialog owns its promise. If anything else closes it (another modal
    // opening calls closeModal directly and resolves nothing), resolve '' -
    // a pending prompt nobody owns would deadlock every future prompt.
    let settled = false;
    const observer = new MutationObserver(() => { if (dialog.hidden) finish(''); });
    const finish = value => {
      if (settled) return;
      settled = true;
      observer.disconnect();
      input.value = '';
      closeModal(dialog);
      resolve(value);
    };
    observer.observe(dialog, { attributes: true, attributeFilter: ['hidden'] });
    operatorDismiss = finish;
    dialog.querySelector('[data-operator-auth="cancel"]').onclick = () => finish('');
    $('operator-dialog-form').onsubmit = e => {
      e.preventDefault();
      // Verbatim, like every other presentation path (bearerToken and the
      // login form never trim): a credential with edge whitespace is valid.
      finish(input.value);
    };
    closeHeaderMenus();
    closeNavMenu();
    closeDimMenu();
    openModal(dialog);
    input.focus();
  }).finally(() => { operatorPrompt = null; });
  // The creating caller must await the same promise it opened: without this
  // return it resumed immediately with undefined and abandoned its 401 - the
  // answered dialog stored nothing and retried nothing, and the next gated
  // call asked all over again (the double sign-in after an idle period).
  return operatorPrompt;
}

// All operator mutations share confirmation, busy and issue-order gates.
const operatorState = {
  pause: { revision: 0, busy: false, menu: 'pause-menu', count: 'pause-count', valid: st => st?.ok === true && Array.isArray(st.holds), apply: applyPauseState, sync: syncPauseMenuState, refresh: refreshPauseMenuDisplay },
  debug: { revision: 0, busy: false, menu: 'debug-menu', count: 'debug-count', valid: st => st?.ok === true && Array.isArray(st.sessions), apply: applyDebugState, sync: syncDebugMenuState, refresh: refreshDebugMenuDisplay },
  throttle: { revision: 0, busy: false, menu: 'limits-menu', count: 'lim-count', valid: st => st?.ok === true && Array.isArray(st.throttles), apply: applyThrottleState, sync: syncLimitsMenuState, refresh: refreshLimitsMenuDisplay },
};
document.addEventListener('click', e => {
  const button = e.target.closest('[data-operator]');
  if (!button || button.disabled) return;
  const actions = { 'pause-edit': editPauseHold, 'pause-resume': resumePauseHold,
    'debug-edit': editDebugSession, 'debug-stop': stopDebugSession, 'limit-edit': editLimitProvider,
    'storm-open': openStormDetails, 'storm-close': closeStormDetails, 'quota-resume': resumeQuotaProvider };
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
// operatorCountClear is operatorError's reset counterpart: one owner for the
// count-line clear (text + error flag), keyed by kind. The limits sites'
// text clear alone would be behavior-neutral (updateLimitCount repaints the
// line immediately after them, and mutateOperator's gate.sync() repaints it
// for every kind), but the ERROR-FLAG clear is load-bearing:
// updateLimitCount early-returns while dataset.err is set, so without this
// clear a provider edit after a failed limits write would leave the stale
// error on the line instead of repainting the live facts (jsdom-pinned).
function operatorCountClear(kind) {
  const el = $(operatorState[kind].count);
  if (el) { el.textContent = ''; delete el.dataset.err; }
}
// resyncOperatorSurfaces is the shared apply-state epilogue: re-sync the
// kind's header menu when it is open, then schedule a live repaint when a
// snapshot exists. displayOnly polls (occupancy, elapsed and capture counts)
// take the kind's refresh path, which updates the affected text without
// touching the form or checklist nodes. The per-kind pre-work (footer
// refresh, pressed bits) stays at its apply site.
function resyncOperatorSurfaces(kind, displayOnly = false) {
  const menu = $(operatorState[kind].menu);
  if (menu && !menu.hidden) {
    if (displayOnly && operatorState[kind].refresh) operatorState[kind].refresh();
    else operatorState[kind].sync();
  }
  if (lastData) scheduleRenderLive();
}
async function mutateOperator(kind, body, after) {
  const gate = operatorState[kind];
  if (gate.busy) return;
  gate.busy = true;
  ++gate.revision;
  operatorCountClear(kind);
  gate.sync();
  let confirmed;
  const fail = 'could not change ' + kind;
  try {
    const st = await operatorJsonBody('/admin/' + kind, {
      method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(body),
    }, fail);
    if (!gate.valid(st)) throw new Error(st?.error || fail);
    confirmed = st;
  } catch (err) {
    operatorError(kind, String(err.message || fail));
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

let _pauseStateSig = '';
let _throttleStateSig = '';
let _debugStateSig = '';
// Structural menu signatures exclude display-only poll fields (queue
// occupancy, elapsed run time and capture counts). They are refreshed in
// place by each kind's refresh function; everything that changes rows,
// checklist vocabulary, selection or take-ability still runs the full sync.
let _pauseMenuSig = '';
let _debugMenuSig = '';
let _limitsMenuSig = '';
let _limitsProviderSig = '';
function applyPauseState(st, revision = operatorState.pause.revision) {
  if (!acceptOperator('pause', st, revision)) return;
  const sig = JSON.stringify(st);
  if (sig === _pauseStateSig) return;
  _pauseStateSig = sig;
  pauseState = {
    paused: !!st.paused,
    clients: Array.isArray(st.clients) ? st.clients : [],
    providers: Array.isArray(st.providers) ? st.providers : [],
    holds: Array.isArray(st.holds) ? st.holds : [],
    [KNOWN_CLIENTS_KEY]: Array.isArray(st[KNOWN_CLIENTS_KEY]) ? st[KNOWN_CLIENTS_KEY] : [],
    [KNOWN_PROVIDERS_KEY]: Array.isArray(st[KNOWN_PROVIDERS_KEY]) ? st[KNOWN_PROVIDERS_KEY] : [],
    until: st.until || null,
    queued: typeof st.queued === 'number' ? st.queued : 0,
    default_max_queued: typeof st.default_max_queued === 'number' ? st.default_max_queued : 0,
  };
  const displayOnly = pauseMenuStructuralSig() === _pauseMenuSig;
  refreshFooterState();
  resyncOperatorSurfaces('pause', displayOnly);
}

function applyThrottleState(st, revision = operatorState.throttle.revision) {
  if (!acceptOperator('throttle', st, revision)) return;
  const sig = JSON.stringify(st);
  if (sig === _throttleStateSig) return;
  _throttleStateSig = sig;
  throttleState = {
    throttles: Array.isArray(st.throttles) ? st.throttles : [],
    [KNOWN_PROVIDERS_KEY]: Array.isArray(st[KNOWN_PROVIDERS_KEY]) ? st[KNOWN_PROVIDERS_KEY] : [],
    active: !!st.active,
  };
  const displayOnly = limitsMenuStructuralSig() === _limitsMenuSig;
  const btn = $('btn-limits');
  if (btn) btn.setAttribute('aria-pressed', throttleState.active ? 'true' : 'false');
  resyncOperatorSurfaces('throttle', displayOnly);
}

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

// ---------- settings shell and lazy module ----------
// chrome.js keeps only the Settings shell: the header trigger, the one-shot
// versioned loader, the sheet's open-state probe, the shared status line and
// the single close entry point. The sheet's state, rendering, editors,
// apply/revert and backup/restore/purge controls live in settings.js and
// load on the first open. Until they arrive, close is a DOM-only no-op and
// Escape cannot touch a draft that does not exist.
let settingsLoadPromise = null;
let settingsLoading = false;
let settingsWantOpen = false;

function settingsIsOpen() {
  const s = $('settings-sheet');
  return !!(s && !s.hidden);
}

function settingsStatus(text, kind) {
  const el = $('settings-count');
  if (!el) return;
  el.textContent = text || '';
  el.classList.toggle('ok', kind === 'ok');
}

// The settings-load failure copy, shared by the lazy loader and the fetched
// form builder (both render the same unavailable-config state).
const SETTINGS_LOAD_FAIL = 'could not load config';

function settingsScriptURL() {
  const version = document.querySelector('meta[name="dashboard-version"]')?.content || '';
  return '/dash/js/settings.js?v=' + encodeURIComponent(version);
}

// loadSettingsModule is the single-flight loader: concurrent or repeated
// opens share one promise and one script request for the page's lifetime.
function loadSettingsModule() {
  if (window.settingsModule) return Promise.resolve(window.settingsModule);
  if (settingsLoadPromise) return settingsLoadPromise;
  settingsLoadPromise = new Promise((resolve, reject) => {
    const script = document.createElement('script');
    script.src = settingsScriptURL();
    script.async = true;
    script.onload = () => {
      const module = window.settingsModule;
      if (module && typeof module.open === 'function' && typeof module.close === 'function') resolve(module);
      else reject(new Error(SETTINGS_LOAD_FAIL));
    };
    script.onerror = () => reject(new Error(SETTINGS_LOAD_FAIL));
    document.head.appendChild(script);
  }).catch(err => {
    settingsStatus(SETTINGS_LOAD_FAIL);
    console.error('settings module failed to load:', err && err.message ? err.message : err);
    throw err;
  });
  return settingsLoadPromise;
}

// closeSettings is the one close entry point every surface calls. Before the
// module loads there is no draft state to guard, so the shell hides the
// sheet/veil directly; after load it delegates to the real owner.
function closeSettings(force) {
  settingsWantOpen = false;
  const module = window.settingsModule;
  if (module && typeof module.close === 'function') { module.close(force); return; }
  const s = $('settings-sheet');
  const v = $('settings-veil');
  const b = $('btn-settings');
  if (s) { s.classList.remove('open'); closeModal(s); }
  if (v) v.classList.remove('open');
  if (b) b.setAttribute('aria-pressed', 'false');
}

// toggleSettings loads the module once, then opens the sheet through its
// owner. A load failure leaves the sheet closed and surfaces the shared
// status copy plus a console error.
function toggleSettings(e) {
  if (e) e.stopPropagation();
  if (settingsIsOpen()) { closeSettings(); return; }
  settingsWantOpen = true;
  const module = window.settingsModule;
  if (module) { module.open(); return; }
  if (settingsLoading) return;
  settingsLoading = true;
  loadSettingsModule().then(mod => {
    settingsLoading = false;
    // A close (or another header menu) while the module was loading cancels
    // the pending open; reopening while loading keeps the intent true.
    if (settingsWantOpen && !settingsIsOpen()) mod.open();
  }, () => {
    settingsLoading = false;
    settingsWantOpen = false;
    settingsStatus(SETTINGS_LOAD_FAIL);
  });
}

// triggerDownload is the one anchor-click download path (log exports,
// backups and debug captures): one owner for the download attribute
// contract - an empty string defers the filename to the server's
// Content-Disposition - and for revoking a passed object URL once the click
// landed.
function triggerDownload(href, download, revoke) {
  const a = document.createElement('a');
  a.href = href;
  a.download = download;
  document.body.appendChild(a);
  a.click();
  a.remove();
  if (revoke) URL.revokeObjectURL(href);
}

// downloadArtifact is the one fetch-blob-filename-click owner for operator
// artifact downloads (the settings backup and the debug capture): through
// the operator gate, blobbed, named from the server's Content-Disposition
// with the caller's fallback, then handed to triggerDownload. It resolves
// the blob (callers report sizes) and rejects through operatorErrorBody
// with the caller's designed failure message.
async function downloadArtifact(url, fallback, failed) {
  const r = await operatorFetch(url);
  if (!r.ok) return operatorErrorBody(r, failed);
  const blob = await r.blob();
  const dispo = r.headers.get('Content-Disposition') || '';
  const m = /filename="([^"]+)"/.exec(dispo);
  triggerDownload(URL.createObjectURL(blob), (m && m[1]) || fallback, true);
  return blob;
}


function applyDebugState(st, revision = operatorState.debug.revision) {
  if (!acceptOperator('debug', st, revision) || !applyModelCanon(st.model_canon)) return;
  const sig = JSON.stringify(st);
  if (sig === _debugStateSig) return;
  _debugStateSig = sig;
  debugState = {
    enabled: !!st.enabled,
    sessions: Array.isArray(st.sessions) ? st.sessions : [],
    [KNOWN_CLIENTS_KEY]: Array.isArray(st[KNOWN_CLIENTS_KEY]) ? st[KNOWN_CLIENTS_KEY] : [],
    [KNOWN_PROVIDERS_KEY]: Array.isArray(st[KNOWN_PROVIDERS_KEY]) ? st[KNOWN_PROVIDERS_KEY] : [],
    [KNOWN_MODELS_KEY]: Array.isArray(st[KNOWN_MODELS_KEY]) ? st[KNOWN_MODELS_KEY] : [],
    until: st.until || null,
    ttl: st.ttl || '',
    max_bytes: st.max_bytes == null || st.max_bytes === '' ? '' : String(st.max_bytes),
  };
  const displayOnly = debugMenuStructuralSig() === _debugMenuSig;
  refreshFooterState();
  resyncOperatorSurfaces('debug', displayOnly);
}

// toggleHdrMenu is the one header-menu toggle branch: stop the event,
// close every other header menu, run the caller's pre-open and post-open
// syncs around the hidden flip, and publish the shared expanded state.
// The named wrappers stay as the onclick surface every button names.
function toggleHdrMenu(e, menuId, prepare, after) {
  e.stopPropagation();
  const m = $(menuId);
  if (m.hidden) {
    closeHeaderMenus(menuId);
    if (prepare) prepare();
    m.hidden = false;
    if (after) after();
  } else {
    hideHdrMenu(menuId);
  }
  syncHdrMenuExpanded();
}

function togglePauseMenu(e) {
  toggleHdrMenu(e, 'pause-menu', resetPauseMenuForm);
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

function selectedCheckboxValues(boxId) {
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
  btn.disabled = operatorState.pause.busy || (scope !== 'all' && scope !== 'new' && selectedCheckboxValues('pf-clients').length === 0 && selectedCheckboxValues('pf-providers').length === 0);
  btn.textContent = pauseEditID ? 'Update' : 'Pause';
}

// NONE_YET_NOTE is the one empty-state sentence for the operator menus'
// known-name surfaces: the pause/debug checklists render it when nothing is
// known yet, and the limits provider select carries it as its empty option.
// JS owns the text; the stylesheet only paints its muted look (.pause-none).
const NONE_YET_NOTE = 'none yet - they appear as requests arrive';
const noneYetNoteElement = () => {
  const note = document.createElement('div');
  note.className = 'pause-none';
  note.textContent = NONE_YET_NOTE;
  return note;
};

// The operator menus repaint from a five-second poll. These setters write
// only values that actually changed, and the reconcilers below keep every
// unchanged row and checkbox node in place: the operator's focus, checked
// state and scroll position survive an occupancy or elapsed-time update.
function setNodeText(el, text) {
  if (el && el.textContent !== text) el.textContent = text;
}
function setNodeDisabled(el, disabled) {
  const value = !!disabled;
  if (el && el.disabled !== value) el.disabled = value;
}
function setNodeClass(el, name, on) {
  const value = !!on;
  if (el && el.classList.contains(name) !== value) el.classList.toggle(name, value);
}
function setNodeData(el, key, value) {
  if (el && el.dataset[key] !== value) el.dataset[key] = value;
}

// takenNameSet is the one per-fill taken-set builder: the predicate runs
// once per name in this fill and callers test membership from the set.
function takenNameSet(names, taken) {
  const out = new Set();
  if (typeof taken !== 'function') return out;
  for (const name of names || []) if (taken(name)) out.add(name);
  return out;
}

// reconcileKeyedChildren is the shared in-place reconciler for checklist and
// hold rows. Every managed child carries its stable key in a data attribute,
// so rows that entered or left the vocabulary are the only ones appended or
// removed; existing rows are moved only when their order genuinely changed.
// A malformed skeleton (missing/duplicate keys, a foreign row) returns false
// before any mutation so the caller can fall back to a full rebuild.
function reconcileKeyedChildren(box, first, items, keyOf, attr, createRow, paintRow, validRow) {
  const keys = items.map(keyOf);
  if (new Set(keys).size !== keys.length) return false;
  const existing = new Map();
  for (let el = first ? first.nextElementSibling : box.firstElementChild; el; el = el.nextElementSibling) {
    const key = el.dataset[attr];
    if (key === undefined || existing.has(key) || (validRow && !validRow(el))) return false;
    existing.set(key, el);
  }
  let ref = first ? first.nextElementSibling : box.firstElementChild;
  for (let i = 0; i < items.length; i++) {
    const key = keys[i];
    let row = existing.get(key);
    if (row) existing.delete(key);
    else row = createRow(items[i], key);
    if (row !== ref) box.insertBefore(row, ref);
    else ref = ref.nextElementSibling;
    paintRow(row, items[i], key);
  }
  for (const row of existing.values()) row.remove();
  return true;
}

// rebuildKeyedChildren is the documented fallback: the same create/paint
// owners, run over a cleared managed range when in-place reconciliation is
// not provably safe.
function rebuildKeyedChildren(box, first, items, keyOf, attr, createRow, paintRow) {
  let node = first ? first.nextSibling : box.firstChild;
  while (node) {
    const next = node.nextSibling;
    node.remove();
    node = next;
  }
  for (const item of items) {
    const key = keyOf(item);
    const row = createRow(item, key);
    paintRow(row, item, key);
    box.append(row);
  }
}

const isCheckRow = el => el.tagName === 'LABEL' && el.classList.contains('pause-check') &&
  !!el.querySelector('input[type=checkbox]');

function createCheckRow() {
  const label = document.createElement('label');
  label.className = 'pause-check';
  const input = document.createElement('input');
  input.type = 'checkbox';
  label.append(input);
  return label;
}

function paintCheckRow(label, name, on, taken, policy) {
  setNodeData(label, 'checkKey', name);
  const input = label.querySelector('input[type=checkbox]');
  if (input.value !== name) input.value = name;
  if (input.checked !== on) input.checked = on;
  setNodeDisabled(input, taken || policy.busy);
  setNodeClass(label, 'taken', taken);
  if (input.onchange !== policy.onchange) input.onchange = policy.onchange;
  const text = ' ' + name;
  const node = input.nextSibling;
  if (!node || node.nodeType !== Node.TEXT_NODE) input.after(document.createTextNode(text));
  else if (node.nodeValue !== text) node.nodeValue = text;
}

// fillCheckList reconciles a checklist against its vocabulary, keeping the
// shared none-yet note when the list is empty.
function fillCheckList(box, items, keyOf, createRow, paintRow) {
  if (!box) return;
  if (!items.length) {
    const note = [...box.children].find(el => el.classList.contains('pause-none'));
    if (note && box.children.length === 1) setNodeText(note, NONE_YET_NOTE);
    else box.replaceChildren(note || noneYetNoteElement());
    return;
  }
  const note = [...box.children].find(el => el.classList.contains('pause-none'));
  if (note) note.remove();
  if (!reconcileKeyedChildren(box, null, items, keyOf, 'checkKey', createRow, paintRow, isCheckRow)) {
    box.replaceChildren();
    rebuildKeyedChildren(box, null, items, keyOf, 'checkKey', createRow, paintRow);
  }
}

function createHoldRow(editOperator, actionOperator, actionLabel) {
  const row = document.createElement('div');
  row.className = 'pause-hold';
  const edit = document.createElement('button');
  edit.className = 'hold-edit';
  edit.dataset.operator = editOperator;
  const action = document.createElement('button');
  action.className = 'btn';
  action.dataset.operator = actionOperator;
  action.textContent = actionLabel;
  row.append(edit, action);
  return row;
}

const isHoldRow = el => el.classList.contains('pause-hold');

// renderKeyedRows reconciles a header plus one row per stable item id. It
// rebuilds only when the container skeleton is not the expected
// header-and-rows shape.
function renderKeyedRows(box, header, items, keyOf, createRow, paintRow, validRow = isHoldRow) {
  if (!box) return;
  if (!items.length) {
    if (box.firstChild) box.replaceChildren();
    return;
  }
  let head = box.firstElementChild;
  if (!head || !head.classList.contains('pause-known-hd')) {
    head = document.createElement('div');
    head.className = 'pause-known-hd';
    box.replaceChildren(head);
  }
  setNodeText(head, header);
  if (!reconcileKeyedChildren(box, head, items, keyOf, 'rowKey', createRow, paintRow, validRow)) {
    rebuildKeyedChildren(box, head, items, keyOf, 'rowKey', createRow, paintRow);
  }
}

// fillCheckRows is the one builder for the operator checklist rows (pause
// clients/providers and debug clients/providers/models): one labeled
// checkbox per known name, checked from the caller's picked set, disabled
// when the name is already taken by another hold or session (policy.takenSet
// or policy.taken) or the surface is busy (policy.busy), with the caller's
// change hook. An empty list renders the shared none-yet note instead.
function fillCheckRows(box, names, picked, policy) {
  if (!box) return;
  const list = names || [];
  const keep = picked instanceof Set ? picked : new Set();
  const takenNames = policy.takenSet instanceof Set
    ? policy.takenSet : takenNameSet(list, policy.taken);
  fillCheckList(box, list, c => c, createCheckRow, (label, name) => {
    const isTaken = takenNames.has(name);
    paintCheckRow(label, name, keep.has(name) && !isTaken, isTaken, policy);
  });
}

function fillPauseChecks(box, names, picked) {
  // Pause holds never disable names - overlap is a client×provider
  // predicate, owned by the scheduler. Greying individual names here would
  // prevent valid disjoint combinations and make polling silently erase
  // the operator's selected draft.
  fillCheckRows(box, names, picked, { onchange: updatePauseApplyEnabled });
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

function pauseHoldLabel(h) {
  const left = [holdScopeLabel(h)];
  // The system label of an auto-created quota hold - the operator sees why
  // a hold they did not create is parking the provider.
  if (h.reason) left.push(h.reason);
  const until = pauseUntilLabel(h.until);
  if (until) left.push(until);
  else if (h.duration) left.push(h.duration);
  if (h.max_queued > 0) left.push('cap ' + h.max_queued);
  const q = typeof h.queued === 'number' && h.queued > 0 ? ' · q ' + h.queued : '';
  return left.join(' · ') + q;
}

function paintPauseHoldRow(row, h) {
  setNodeData(row, 'rowKey', h.id || '');
  const edit = row.querySelector('[data-operator="pause-edit"]');
  const resume = row.querySelector('[data-operator="pause-resume"]');
  setNodeData(edit, 'value', h.id || '');
  setNodeData(resume, 'value', h.id || '');
  setNodeText(edit, pauseHoldLabel(h));
  setNodeDisabled(edit, operatorState.pause.busy);
  setNodeDisabled(resume, operatorState.pause.busy);
  setNodeClass(row, 'editing', pauseEditID === h.id);
}

const isPauseHoldRow = el => isHoldRow(el) &&
  !!el.querySelector('[data-operator="pause-edit"]') && !!el.querySelector('[data-operator="pause-resume"]');

function renderPauseHolds() {
  renderKeyedRows($('pf-holds'), 'Active holds', liveHolds(), h => h.id || '',
    () => createHoldRow('pause-edit', 'pause-resume', 'Resume'), paintPauseHoldRow, isPauseHoldRow);
}

function pauseMenuStructuralSig() {
  const holds = (pauseState.holds || []).map(h => [
    h.id || '', !!h.all, !!h.new, h.clients || [], h.providers || [], h.known_at_new || [],
    h.duration || '', h.until || '', Number(h.max_queued) || 0, h.reason || '',
  ]);
  return JSON.stringify([holds, pauseState.clients || [], pauseState.providers || [],
    pauseState[KNOWN_CLIENTS_KEY] || [], pauseState[KNOWN_PROVIDERS_KEY] || [],
    pauseState.until || null, Number(pauseState.default_max_queued) || 0]);
}

// refreshPauseMenuDisplay owns the live half of the pause menu: the queued
// count line, the per-hold queue count, and the header/resume state that
// follows hold liveness. Checklist drafts, taken names and hold structure are
// untouched unless the apply gate saw a structural change.
function refreshPauseMenuDisplay() {
  const resume = $('btn-pause-resume');
  if (resume) resume.disabled = operatorState.pause.busy || !pauseActive();
  const hd = document.querySelector('#pause-menu .clear-menu-hd');
  if (hd) hd.textContent = pauseEditID ? 'Edit hold…' : (pauseActive() ? 'Holding requests…' : 'Hold requests…');
  const count = $('pause-count');
  if (count && !count.dataset.err) count.textContent = pauseActive() ? pauseStatusBits().join(' · ') : '';
  renderPauseHolds();
}

// knownNames is the one known-name derivation for the pause/debug/limits
// menus: the operator state's canonical list unioned with any extra name
// collections (an active hold's names, the operator's picked checkboxes),
// deduped and sorted once.
function knownNames(state, key, ...extra) {
  const out = new Set(state?.[key] || []);
  for (const names of extra) for (const n of names || []) out.add(n);
  return [...out].sort();
}

function refreshPauseKnownLists(picks) {
  const pickedC = picks && picks.clients instanceof Set
    ? picks.clients : new Set(selectedCheckboxValues('pf-clients'));
  const pickedP = picks && picks.providers instanceof Set
    ? picks.providers : new Set(selectedCheckboxValues('pf-providers'));
  const knownC = knownNames(pauseState, KNOWN_CLIENTS_KEY, pauseState.clients, pickedC);
  const knownP = knownNames(pauseState, KNOWN_PROVIDERS_KEY, pauseState.providers, pickedP);
  fillPauseChecks($('pf-clients'), knownC, pickedC);
  fillPauseChecks($('pf-providers'), knownP, pickedP);
  onPauseScopeChange();
}

function syncPauseMenuState(picks) {
  if (pauseEditID && !liveHolds().some(h => h.id === pauseEditID)) {
    pauseEditID = '';
    resetPauseMenuValues();
    picks = { clients: new Set(), providers: new Set() };
  }
  fillPauseControls();
  refreshPauseKnownLists(picks);
  const cap = $('pf-cap');
  if (cap && document.activeElement !== cap) {
    cap.placeholder = pauseState.default_max_queued > 0 ? String(pauseState.default_max_queued) : 'unlimited';
  }
  ['pf-scope', 'pf-dur', 'pf-cap'].forEach(id => { if ($(id)) $(id).disabled = operatorState.pause.busy; });
  refreshPauseMenuDisplay();
  updatePauseApplyEnabled();
  _pauseMenuSig = pauseMenuStructuralSig();
}

// resetPauseMenuValues clears the editable draft without painting: the one
// following sync owns the single list paint.
function resetPauseMenuValues() {
  pauseEditID = '';
  if ($('pf-scope')) $('pf-scope').value = '';
  if ($('pf-dur')) $('pf-dur').value = '';
  const cap = $('pf-cap');
  if (cap) cap.value = '';
  operatorCountClear('pause');
}

function resetPauseMenuForm() {
  resetPauseMenuValues();
  syncPauseMenuState({ clients: new Set(), providers: new Set() });
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

// openHdrMenuForEdit is the one open-for-edit sequence: record-driven
// edits open their header menu exactly like a toggle does, without
// stomping the edit-in-progress form.
function openHdrMenuForEdit(menuId) {
  closeHeaderMenus(menuId);
  const m = $(menuId);
  if (m) m.hidden = false;
  syncHdrMenuExpanded();
}

function editHoldForRecord(r) {
  const holds = liveHolds();
  if (!holds.length || !r) return;
  const named = holds.find(h => !h.all && holdMatchesRecord(h, r));
  const h = named || holds.find(h => h.all);
  if (!h) return;
  openHdrMenuForEdit('pause-menu');
  editPauseHold(h.id);
}

function recordById(id) {
  if (!id) return undefined;
  const recs = lastData?.records || [];
  const i = _recIdx.get(id);
  if (i !== undefined && recs[i]?.id === id) return recs[i];
  // A full replacement renders the drawer before _rebuildRecIdx runs; fall
  // back to a scan rather than drop the open drawer for one frame.
  return recs.find(r => r && r.id === id) || logArchive.find(r => r && r.id === id);
}

function editPauseHold(id) {
  if (!id || operatorState.pause.busy) return;
  if (pauseEditID === id) {
    resetPauseMenuForm();
    return;
  }
  const h = liveHolds().find(x => x.id === id);
  if (!h) return;
  pauseEditID = id;
  fillSelectPairs($('pf-scope'), PAUSE_SCOPES, h.all ? 'all' : (h.new ? 'new' : ''));
  fillSelectPairs($('pf-dur'), PAUSE_DURS, h.duration || '');
  const cap = $('pf-cap');
  if (cap) cap.value = typeof h.max_queued === 'number' ? String(h.max_queued) : '';
  operatorCountClear('pause');
  // The picked sets ride the single sync: it paints the checklists once with
  // the hold's exact scope while preserving the size controls just set.
  syncPauseMenuState({
    clients: new Set(h.clients || []),
    providers: new Set(h.providers || []),
  });
}

function applyPauseMenu() {
  if (operatorState.pause.busy) return;
  const scope = pauseMenuScope();
  const all = scope === 'all';
  const neu = scope === 'new';
  const clients = all ? [] : selectedCheckboxValues('pf-clients');
  const providers = all ? [] : selectedCheckboxValues('pf-providers');
  if (!all && !neu && !clients.length && !providers.length) return;
  const body = { paused: true, all, new: !all && neu, clients, providers, duration: pauseMenuDuration() };
  if (pauseEditID) body.id = pauseEditID;
  const cap = pauseMenuCap();
  if (Number.isNaN(cap)) {
    operatorError('pause', 'queue cap must be a non-negative whole number');
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

function toggleLimitsMenu(e) {
  toggleHdrMenu(e, 'limits-menu', () => { syncLimitsMenuState(); onLimitProviderChange(); });
}

function limitByProvider(name) {
  return (throttleState.throttles || []).find(t => t && t.provider === name) || null;
}

// selectPairSigs remembers the last option vocabulary per select. A poll
// whose pairs are unchanged must not replace the option list: that would
// close an open picker and drop the operator's selection position.
const selectPairSigs = new WeakMap();
// setSelectValue sets one select's value, keeping exactly one custom option
// for a saved value outside the curated pairs and removing it once the value
// is curated again. Curated choices are suggestions, not permission to erase
// a saved value.
function setSelectValue(sel, value) {
  const v = value == null ? '' : String(value);
  let custom = null;
  for (const option of sel.options) if (option.dataset.pairCustom === '1') { custom = option; break; }
  const curated = [...sel.options].some(o => o.value === v && o.dataset.pairCustom !== '1');
  if (curated) {
    if (custom) custom.remove();
  } else if (custom) {
    if (custom.value !== v) custom.value = v;
    const label = v || 'off';
    if (custom.textContent !== label) custom.textContent = label;
  } else {
    const option = new Option(v || 'off', v);
    option.dataset.pairCustom = '1';
    sel.add(option);
  }
  if (sel.value !== v) sel.value = v;
}

function fillSelectPairs(sel, pairs, value) {
  if (!sel || !pairs) return;
  const sig = JSON.stringify(pairs);
  if (selectPairSigs.get(sel) !== sig) {
    selectPairSigs.set(sel, sig);
    sel.innerHTML = pairs.map(([val, lab]) => `<option value="${escapeHtml(String(val))}">${escapeHtml(lab)}</option>`).join('');
  }
  setSelectValue(sel, value);
}

const PAUSE_SCOPES = [
  ['', 'named only'],
  ['all', 'all requests'],
  ['new', 'new clients'],
];
// DURATION_LABELS is the one label vocabulary for time-window dropdown
// values, so every surface spells the same span the same way ('15 minutes',
// never '15 min'). Surfaces keep their own value sets; only the wording
// is shared. Ages beyond a day stay context-specific ('1 day', '1 week').
// The chart's window register (CHART_WINDOWS in chart.js) is the documented
// exclusion: its values are minute counts ('15', '60'), not these duration
// keys, and the two files share no module - deriving its four matching
// sub-day wordings would need a cross-file load-order dependency plus a
// value-translation table, so those coincident spellings stay separate.
const DURATION_LABELS = {
  '15m': '15 minutes', '1h': '1 hour', '6h': '6 hours',
  '12h': '12 hours', '24h': '24 hours',
};
// durationPairs builds [value, label] select pairs from DURATION_LABELS.
const durationPairs = (...vals) => vals.map(v => [v, DURATION_LABELS[v]]);
const PAUSE_DURS = [['', 'I resume']].concat(durationPairs('15m', '1h', '6h', '12h', '24h'));
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
  toggleHdrMenu(e, 'debug-menu', resetDebugMenuForm);
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

// debugTakenNameSet is the one taken-name derivation for a debug checklist
// fill: the other sessions are scanned once and every name they exclusively
// scope is collected. A name shared by a session with another scoped
// dimension stays selectable (the scheduler matches dimensions AND).
function debugTakenNameSet(kind) {
  const key = kind === 'client' ? 'clients' : kind === 'provider' ? 'providers' : kind === 'model' ? 'models' : '';
  const out = new Set();
  if (!key) return out;
  for (const h of otherDebugSessions()) {
    if (kind === 'client' && ((h.providers || []).length || (h.models || []).length)) continue;
    if (kind === 'provider' && ((h.clients || []).length || (h.models || []).length)) continue;
    if (kind === 'model' && ((h.clients || []).length || (h.providers || []).length)) continue;
    for (const name of h[key] || []) out.add(name);
  }
  return out;
}


// Debug-checklist model grouping. Clients and providers spell the same model
// differently (glm-5.3 vs glm-5-3, moonshotai/kimi-k3:nube vs kimi-k3), so the
// raw known-models list reads as duplicates. The checklist therefore groups
// spelling variants for display using the canonicalization rules from config
// (canonicalModel in explorer.js - observer-authored name lookup;
// rules editable in Settings). A debug session stores the exact raw
// spellings, but the server never raw-matches them: debug.go's modelIn folds
// both the session's models and each request's model through the cursor
// base name. Checking a group selects every variant of it, and editing a
// session re-checks the group holding any of its raw models.
let dbgModelGroups = [];
// groupByCanonical is the one fold of raw model spellings into canonical
// groups: deduped input, one canonicalModel pass per name, each group's
// variants sorted. Both grouped surfaces build on it - the debug
// checklist (dbgGroupedModels below) and the delete/download model
// optgroups (populateFilterMenu) - while each keeps its own projection
// (display-name groups vs raw-value optgroups).
function groupByCanonical(names) {
  const byGroup = new Map();
  for (const n of [...new Set(names || [])]) {
    const g = canonicalModel(n);
    if (!byGroup.has(g)) byGroup.set(g, []);
    byGroup.get(g).push(n);
  }
  return [...byGroup.entries()].map(([key, variants]) => ({ key, variants: variants.sort() }));
}
// dbgGroupedModels: raw (deduped, sorted) names → ordered [group] objects
// {key, display, variants}; a group's display name is its shortest variant
// (the bare spelling), ties broken lexicographically.
function dbgGroupedModels(names) {
  return groupByCanonical(names).map(({ key, variants }) => ({
    key,
    display: [...variants].sort((a, b) => a.length - b.length || (a < b ? -1 : 1))[0],
    variants,
  })).sort((a, b) => (a.display < b.display ? -1 : a.display > b.display ? 1 : 0));
}
// selectedDebugModels expands the checked model groups back into the exact raw
// spellings the debug session matches on.
function selectedDebugModels() {
  const sel = new Set(selectedCheckboxValues('df-models'));
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
  btn.disabled = operatorState.debug.busy || selectedCheckboxValues('df-clients').length === 0 && selectedCheckboxValues('df-providers').length === 0 && selectedCheckboxValues('df-models').length === 0;
  btn.textContent = debugEditID ? 'Update' : 'Start';
}

function paintDebugModelRow(label, g, keep, takenNames) {
  setNodeData(label, 'checkKey', g.display);
  const title = g.variants.join(' · ');
  if (label.title !== title) label.title = title;
  const taken = g.variants.some(v => takenNames.has(v));
  const on = !taken && (keep.has(g.display) || g.variants.some(v => keep.has(v)));
  const input = label.querySelector('input[type=checkbox]');
  if (input.value !== g.display) input.value = g.display;
  if (input.checked !== on) input.checked = on;
  setNodeDisabled(input, taken || operatorState.debug.busy);
  setNodeClass(label, 'taken', taken);
  if (input.onchange !== updateDebugApplyEnabled) input.onchange = updateDebugApplyEnabled;
  const text = ' ' + g.display;
  const node = input.nextSibling;
  if (!node || node.nodeType !== Node.TEXT_NODE) input.after(document.createTextNode(text));
  else if (node.nodeValue !== text) node.nodeValue = text;
  // The multi-variant count is display-only; keep its span so a poll cannot
  // replace the label the operator is reading.
  const extra = g.variants.length > 1 ? '+' + (g.variants.length - 1) : '';
  let count = [...label.children].find(el => el.dataset.pauseExtra === '1');
  if (extra) {
    if (!count) {
      label.append(document.createTextNode(' '));
      count = document.createElement('span');
      count.dataset.pauseExtra = '1';
      count.setAttribute('aria-hidden', 'true');
      count.style.color = 'var(--muted)';
      label.append(count);
    }
    if (count.textContent !== extra) count.textContent = extra;
  } else if (count) {
    const space = count.previousSibling;
    if (space && space.nodeType === Node.TEXT_NODE && space.nodeValue === ' ') space.remove();
    count.remove();
  }
}

function fillDebugChecks(box, names, picked, kind) {
  if (!box) return;
  const keep = picked instanceof Set ? picked : new Set();
  const takenNames = debugTakenNameSet(kind);
  if (kind === 'model') {
    // Grouped display (dbgGroupedModels builds the groups; dbgModelGroups
    // carries them): one checkbox per spelling group, value = the group's
    // display name; the raw variants are expanded by selectedDebugModels
    // when the session is applied. `keep` may hold raw names (edit path) or
    // display names (refresh path) - either marks the group checked.
    dbgModelGroups = dbgGroupedModels(names);
    fillCheckList(box, dbgModelGroups, g => g.display, createCheckRow,
      (label, g) => paintDebugModelRow(label, g, keep, takenNames));
  } else {
    fillCheckRows(box, names, picked, {
      takenSet: takenNames, busy: operatorState.debug.busy, onchange: updateDebugApplyEnabled,
    });
  }
}

function debugScopeLabel(h) {
  if (!h) return '';
  const bits = [];
  if (Array.isArray(h.clients) && h.clients.length) bits.push(h.clients.join(', '));
  if (Array.isArray(h.providers) && h.providers.length) bits.push(h.providers.join(', '));
  if (Array.isArray(h.models) && h.models.length) bits.push(h.models.join(', '));
  return bits.join(' + ') || 'session';
}

// hmBreakdown is the one compact H/M breakdown for remaining-time labels:
// whole minutes below an hour, hours plus remaining minutes above. Callers
// own the rounding of their minute count and any suffix - pause holds say
// "N left" from a ceil, debug sessions report whole minutes from a round.
function hmBreakdown(m) {
  if (m < 60) return m + 'm';
  const hr = Math.floor(m / 60), rem = m % 60;
  return rem ? hr + 'h ' + rem + 'm' : hr + 'h';
}

function debugRanLabel(h) {
  const ms = typeof h.ran_ms === 'number' ? h.ran_ms : 0;
  if (ms <= 0) return '';
  return hmBreakdown(Math.max(1, Math.round(ms / 60000)));
}

function debugSessionLabel(h) {
  const left = [debugScopeLabel(h)];
  const until = pauseUntilLabel(h.until);
  if (until) left.push(until);
  else if (h.duration) left.push(h.duration);
  else left.push('until stop');
  const ran = debugRanLabel(h);
  if (ran) left.push('ran ' + ran);
  const n = typeof h.captures === 'number' ? h.captures : 0;
  left.push(n + ' req');
  return left.join(' · ');
}

function paintDebugSessionRow(row, h) {
  setNodeData(row, 'rowKey', h.id || '');
  const edit = row.querySelector('[data-operator="debug-edit"]');
  const stop = row.querySelector('[data-operator="debug-stop"]');
  setNodeData(edit, 'value', h.id || '');
  setNodeData(stop, 'value', h.id || '');
  setNodeText(edit, debugSessionLabel(h));
  setNodeDisabled(edit, operatorState.debug.busy);
  setNodeDisabled(stop, operatorState.debug.busy);
  setNodeClass(row, 'editing', debugEditID === h.id);
}

const isDebugSessionRow = el => isHoldRow(el) &&
  !!el.querySelector('[data-operator="debug-edit"]') && !!el.querySelector('[data-operator="debug-stop"]');

function renderDebugSessions() {
  renderKeyedRows($('df-holds'), 'Active sessions', liveDebugSessions(), h => h.id || '',
    () => createHoldRow('debug-edit', 'debug-stop', 'Stop'), paintDebugSessionRow, isDebugSessionRow);
}

function debugMenuStructuralSig() {
  const sessions = (debugState.sessions || []).map(h => [
    h.id || '', h.clients || [], h.providers || [], h.models || [],
    h.duration || '', h.until || '',
  ]);
  // Expiry changes taken-name and row membership while every raw session
  // field stays put. The live id set is the exact liveness witness: when a
  // session expires (or arrives live) between polls, the full sync must run.
  const live = liveDebugSessions().map(h => h.id || '');
  return JSON.stringify([!!debugState.enabled, live, sessions,
    debugState[KNOWN_CLIENTS_KEY] || [], debugState[KNOWN_PROVIDERS_KEY] || [],
    debugState[KNOWN_MODELS_KEY] || [], debugState.until || null,
    typeof modelRevision === 'undefined' ? null : modelRevision]);
}

// refreshDebugMenuDisplay updates the elapsed time, capture counts and
// session liveness that change on every poll without touching the checklist
// drafts.
function refreshDebugMenuDisplay() {
  const stop = $('btn-debug-stop');
  if (stop) stop.disabled = operatorState.debug.busy || !debugActive();
  const hd = document.querySelector('#debug-menu .clear-menu-hd');
  if (hd) hd.textContent = debugEditID ? 'Edit session…' : (debugActive() ? 'Capturing debug…' : 'Capture debug…');
  const count = $('debug-count');
  if (count && !count.dataset.err) count.textContent = debugActive() ? debugStatusBits().join(' · ') : '';
  renderDebugSessions();
}

function refreshDebugKnownLists(picks) {
  const pickedC = picks && picks.clients instanceof Set
    ? picks.clients : new Set(selectedCheckboxValues('df-clients'));
  const pickedP = picks && picks.providers instanceof Set
    ? picks.providers : new Set(selectedCheckboxValues('df-providers'));
  const pickedM = picks && picks.models instanceof Set
    ? picks.models : new Set(selectedCheckboxValues('df-models'));
  const knownC = knownNames(debugState, KNOWN_CLIENTS_KEY, pickedC);
  const knownP = knownNames(debugState, KNOWN_PROVIDERS_KEY, pickedP);
  const knownM = knownNames(debugState, KNOWN_MODELS_KEY, pickedM);
  fillDebugChecks($('df-clients'), knownC, pickedC, 'client');
  fillDebugChecks($('df-providers'), knownP, pickedP, 'provider');
  fillDebugChecks($('df-models'), knownM, pickedM, 'model');
  updateDebugApplyEnabled();
}

function syncDebugMenuState(picks) {
  if (debugEditID && !liveDebugSessions().some(h => h.id === debugEditID)) {
    debugEditID = '';
    resetDebugMenuValues();
    picks = { clients: new Set(), providers: new Set(), models: new Set() };
  }
  fillSelectPairs($('df-dur'), DEBUG_DURS, debugMenuDuration());
  refreshDebugKnownLists(picks);
  if ($('df-dur')) $('df-dur').disabled = operatorState.debug.busy;
  refreshDebugMenuDisplay();
  updateDebugApplyEnabled();
  _debugMenuSig = debugMenuStructuralSig();
}

// resetDebugMenuValues clears the editable draft without painting: the one
// following sync owns the single list paint.
function resetDebugMenuValues() {
  debugEditID = '';
  if ($('df-dur')) $('df-dur').value = '';
  operatorCountClear('debug');
}

function resetDebugMenuForm() {
  resetDebugMenuValues();
  syncDebugMenuState({ clients: new Set(), providers: new Set(), models: new Set() });
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
  // The record's stamped debug_session_id is the authoritative match: the
  // server captured that request under that session. The raw-scope heuristic
  // (debugMatchesRecord) is only a labeled fallback for a record the stamp
  // does not reach.
  const stamped = r.debug_session_id && holds.find(x => x.id === r.debug_session_id);
  const h = stamped || holds.find(x => debugMatchesRecord(x, r));
  if (!h) return;
  openHdrMenuForEdit('debug-menu');
  editDebugSession(h.id);
}

function editDebugSession(id) {
  if (!id || operatorState.debug.busy) return;
  if (debugEditID === id) {
    resetDebugMenuForm();
    return;
  }
  const h = liveDebugSessions().find(x => x.id === id);
  if (!h) return;
  debugEditID = id;
  fillSelectPairs($('df-dur'), DEBUG_DURS, h.duration || '');
  operatorCountClear('debug');
  // The picked sets ride the single sync: every checklist is painted once
  // with the session's exact raw scope.
  syncDebugMenuState({
    clients: new Set(h.clients || []),
    providers: new Set(h.providers || []),
    models: new Set(h.models || []),
  });
}

function applyDebugMenu() {
  const clients = selectedCheckboxValues('df-clients');
  const providers = selectedCheckboxValues('df-providers');
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
// Spans shared with other surfaces borrow DURATION_LABELS' wording; the
// sub-minute entries are limits-specific.
const LIMIT_WINDOWS = [
  ['1s', '1 second'], ['10s', '10 seconds'], ['30s', '30 seconds'],
  ['1m', '1 minute'], ['5m', '5 minutes'],
].concat(durationPairs('15m', '1h', '6h', '24h'));
// The fallback window when a limit carries none (fillLimitWindows' default
// and the apply body's missing-select fallback): '1m', named once.
const LIMIT_WINDOW_DEFAULT = '1m';
function fillLimitWindows(sel, value) {
  fillSelectPairs(sel, LIMIT_WINDOWS, value || LIMIT_WINDOW_DEFAULT);
}

function syncLimitsMenuState() {
  document.querySelectorAll('#limits-menu input, #limits-menu select').forEach(el => { el.disabled = operatorState.throttle.busy; });
  const known = knownNames(throttleState, KNOWN_PROVIDERS_KEY,
    (throttleState.throttles || []).map(t => t.provider).filter(Boolean));
  const sel = $('lim-provider');
  const providerSig = JSON.stringify(known);
  if (sel && providerSig !== _limitsProviderSig) {
    _limitsProviderSig = providerSig;
    const keep = sel.value;
    const empty = known.length ? '' : `<option value="">${NONE_YET_NOTE}</option>`;
    sel.innerHTML = empty + known.map(p => `<option value="${escapeHtml(p)}">${escapeHtml(p)}</option>`).join('');
    if (keep && known.includes(keep)) sel.value = keep;
  }
  updateLimitCount();
  renderLimitHolds();
  updateLimitsApplyEnabled();
  _limitsMenuSig = limitsMenuStructuralSig();
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
  operatorCountClear('throttle');
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
    request_window: req > 0 ? ($('lim-reqw') ? $('lim-reqw').value : LIMIT_WINDOW_DEFAULT) : '',
    tokens: tok,
    token_window: tok > 0 ? ($('lim-tokw') ? $('lim-tokw').value : LIMIT_WINDOW_DEFAULT) : '',
  };
  return mutateOperator('throttle', body);
}

function clearLimitsMenu() {
  const provider = $('lim-provider')?.value;
  if (!provider) return;
  return mutateOperator('throttle', { provider, clear: true }, onLimitProviderChange);
}

function limitHoldLabel(t) {
  const bits = [];
  if (t.concurrency) bits.push('×' + t.concurrency);
  if (t.requests) bits.push(t.requests + '/' + (t.request_window || ''));
  if (t.tokens) bits.push(fmt(t.tokens) + ' tok/' + (t.token_window || ''));
  return (t.provider || '') + ' · ' + bits.join(' · ');
}

function createLimitHoldRow() {
  const row = document.createElement('div');
  row.className = 'pause-hold';
  const button = document.createElement('button');
  button.className = 'hold-edit';
  button.dataset.operator = 'limit-edit';
  const source = document.createElement('span');
  source.className = 'lim-src';
  row.append(button, source);
  return row;
}

function paintLimitHoldRow(row, t) {
  setNodeData(row, 'rowKey', t.provider || '');
  const button = row.querySelector('[data-operator="limit-edit"]');
  const source = row.querySelector('.lim-src');
  const src = t.source === 'header' ? 'header' : 'ui';
  setNodeData(button, 'value', t.provider || '');
  setNodeText(button, limitHoldLabel(t));
  setNodeDisabled(button, operatorState.throttle.busy);
  setNodeText(source, src);
  setNodeClass(source, 'header', src === 'header');
  setNodeClass(source, 'ui', src === 'ui');
  setNodeClass(row, 'editing', !!( $('lim-provider') && $('lim-provider').value === t.provider));
}

const isLimitHoldRow = el => isHoldRow(el) &&
  !!el.querySelector('[data-operator="limit-edit"]') && !!el.querySelector('.lim-src');

function renderLimitHolds() {
  renderKeyedRows($('lim-holds'), 'Active limits', throttleState.throttles || [], t => t.provider || '',
    createLimitHoldRow, paintLimitHoldRow, isLimitHoldRow);
}

function limitsMenuStructuralSig() {
  const rows = (throttleState.throttles || []).map(t => [
    t.provider || '', Number(t.concurrency) || 0, Number(t.requests) || 0,
    t.request_window || '', Number(t.tokens) || 0, t.token_window || '',
    t.source || '', t.updated_by || '',
  ]);
  return JSON.stringify([rows, throttleState[KNOWN_PROVIDERS_KEY] || []]);
}

// refreshLimitsMenuDisplay owns occupancy-only limits updates: the live
// count line and the hold rows. The provider options and the editable cap
// fields are untouched unless the apply gate saw a structural change.
function refreshLimitsMenuDisplay() {
  updateLimitCount();
  renderLimitHolds();
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
  operatorCountClear('throttle');
  onLimitProviderChange();
  renderLimitHolds();
}

function editLimitForRecord(r) {
  if (!r || !r.provider) return;
  openHdrMenuForEdit('limits-menu');
  syncLimitsMenuState();
  editLimitProvider(r.provider);
}

function pauseUntilLabel(until) {
  if (!until) return '';
  const ms = new Date(until).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return '';
  return hmBreakdown(Math.ceil(ms / 60000)) + ' left';
}

// pauseStatusBits and debugStatusBits own the operator status word lists
// shared by the menus' count lines and the footer state line. The footer's
// hold variant inserts the live-hold scope bits before the queued count;
// only that surface renders scope, so its call site passes them in.
function pauseStatusBits(scopeBits = []) {
  const bits = ['paused', ...scopeBits];
  if (pauseState.queued > 0) bits.push('queued ' + pauseState.queued);
  const left = pauseUntilLabel(pauseState.until);
  if (left) bits.push(left);
  return bits;
}

function debugStatusBits() {
  const bits = ['debug'];
  const live = liveDebugSessions();
  if (live.length === 1) bits.push(debugScopeLabel(live[0]));
  else if (live.length > 1) bits.push(live.length + ' sessions');
  const left = pauseUntilLabel(debugState.until);
  if (left) bits.push(left);
  return bits;
}

// setFooterLive mirrors SSE connectivity. refreshFooterState combines that
// with every other footer input: the operator surfaces (a hold reads as
// "paused", a debug session as "debug", each with scope and remaining
// time), the storage health footnotes (durable drops, a degraded boot
// scan), and "offline" when the live stream drops.
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
      const live = liveHolds();
      const scopeBits = live.length === 1 ? [holdScopeLabel(live[0])] : live.length > 1 ? [live.length + ' holds'] : [];
      state.textContent = pauseStatusBits(scopeBits).join(' · ');
    } else if (debugging) {
      state.textContent = debugStatusBits().join(' · ');
    } else {
      state.textContent = streamLive ? 'live' : 'offline';
    }
    // Storage health footnotes share the warning styling: durable drops,
    // and a degraded boot scan (since-inception totals incomplete).
    const dropped = storageState?.enabled ? storageState.dropped : 0;
    const degraded = storageState?.totals_degraded === true;
    state.classList.toggle('storage-warning', dropped > 0 || degraded);
    state.title = dropped > 0
      ? 'Records not saved durably since this process started (queue overflow, closed store, or write failure). Some may remain in the live ring.'
      : degraded
        ? 'The startup history scan failed, so since-inception totals only count rows committed since this process started.'
        : '';
    if (dropped > 0) state.textContent += ' · ' + fmt(dropped) + ' storage drops';
    if (degraded) state.textContent += ' · totals degraded';
  }
}

function doFilter() {
  filters.status = $('f-status').value;
  storage.set(FILTERS_STORAGE_KEY, JSON.stringify(filters)); // persist across reloads
  renderAll(lastData);
  fetchChart();      // chart is scoped to status + explorer filters
  // The shared request gate cancels an obsolete scan immediately and
  // dedupes repeated selections; no delay or second debounce owner.
  fetchExplorer();
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
  const unavailable = restartServerState.available === false;
  const btn = $('btn-restart-now');
  if (btn) {
    btn.disabled = active || unavailable;
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
  // A binary that cannot restart is not offered at all: the whole control and
  // its menu leave the row (the wrap is the unit - the menu means nothing
  // without its trigger, and an empty wrap would hold the row's gap open). A
  // run in progress keeps it reachable so progress can still be reopened.
  // Hiding the control also hides the eligibility REASON: nothing tells the
  // operator why the row lost its Restart entry, because the one surface that
  // could (the menu's status line, fed by fetchRestartStatus) is itself
  // unreachable once the trigger is gone.
  const wrap = $('restart-wrap');
  if (wrap) wrap.hidden = unavailable && !active;
}

// applyRestartState folds the bootstrap's boot-time eligibility into the
// restart state and re-syncs the controls, so a page that never opens the menu
// still knows whether to OFFER the action. The verdict rides the bootstrap the
// page already loads: no extra round trip, and a payload without the section
// (or without a boolean available) leaves the current behaviour alone. Only the
// verdict is adopted. The accompanying reason is deliberately NOT: the control
// is hidden exactly when the verdict is false, so there is nowhere to render it.
// The server's own refusal (POST /admin/restart) still carries the same string
// for a caller of the API.
function applyRestartState(st) {
  if (!st || typeof st.available !== 'boolean') return;
  restartServerState = { ...restartServerState, available: st.available };
  syncRestartControls();
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

// The poll fallback's budget and cadence. The deadline must cover the whole
// remaining choreography or the fallback gives up while the server is still
// draining: the server bounds the build (restartBuildTimeout, 5 minutes) and
// the child's ready wait (restartReadyTimeout, 5 minutes) on its side, and
// the status document carries the drain bound as drain_timeout_ms (the
// configured restart_drain_timeout; 0 = the server waits indefinitely).
// RESTART_DRAIN_FALLBACK_MS mirrors the server-side default drain timeout
// (internal/config Default: RestartDrainTimeout, 10 minutes) for documents
// that omit the bound or report an unbounded drain.
const RESTART_BUILD_BUDGET_MS = 300000;
const RESTART_READY_BUDGET_MS = 300000;
const RESTART_DRAIN_FALLBACK_MS = 600000;
// Poll cadence while the choreography runs; each poll is raced against the
// hang bound because drain-phase polls sit unanswered in the inherited
// socket's backlog.
const RESTART_POLL_INTERVAL_MS = 750;
const RESTART_POLL_HANG_MS = 5000;
// How long the success notice stays up before the menu closes.
const RESTART_DONE_MS = 2500;

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
  toggleHdrMenu(e, 'restart-menu', null, fetchRestartStatus);
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
    let doc;
    try {
      doc = await operatorJsonBody('/admin/restart', { method: 'POST' }, r => 'restart failed (' + r.status + ')');
    } catch (err) {
      // The server answered with a refusal: show its message as the status
      // line, exactly as every other restart status reads. Transport
      // failures rethrow and take the generic prefix in the outer catch.
      if (!err || err.operatorBody !== true) throw err;
      setRestartStatus(err.message, true);
      return;
    }
    if (restartFailed) return;
    applyRestartEvent(doc);
    // A fast failed build can already be idle in the POST response, before
    // this client has observed its active phase on the watch connection.
    if (doc.phase === 'idle' && doc.error) return;
    // Poll in parallel with the watch: streaming drives the step list live
    // (including drain); polls detect the fresh process via started_at and
    // are the fallback when the client cannot stream. Drain polls hang, so
    // each is raced against the hang bound. The deadline derives from the
    // status document's drain_timeout_ms plus the server's own build and
    // ready budgets, so a long drain never reads as a timeout.
    const drainMs = Number(doc.drain_timeout_ms) > 0 ? Number(doc.drain_timeout_ms) : RESTART_DRAIN_FALLBACK_MS;
    const deadline = Date.now() + RESTART_BUILD_BUDGET_MS + drainMs + RESTART_READY_BUDGET_MS;
    const pollDone = (async () => {
      while (Date.now() < deadline) {
        await new Promise(res => setTimeout(res, RESTART_POLL_INTERVAL_MS));
        if (restartFailed) return null;
        const poll = new AbortController();
        const timeout = setTimeout(() => poll.abort(), RESTART_POLL_HANG_MS);
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
      setTimeout(() => { hideHdrMenu('restart-menu'); setRestartStatus(''); const ol = $('restart-steps'); if (ol) { ol.hidden = true; ol.innerHTML = ''; } }, RESTART_DONE_MS);
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
