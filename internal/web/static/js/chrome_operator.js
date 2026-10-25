// Operator credential and session gate: the tab-scoped token, one 401 ->
// prompt path, the gated fetch/json owners, and the per-surface revision
// resync that follows an accepted unlock. Loaded eagerly after the storm
// module.

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
