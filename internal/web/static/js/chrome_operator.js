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
//
// The rejection is a real Error carrying the status, not a bare
// Promise.reject(). A reasonless rejection reached the ledger as
// "could not undo: undefined", which tells the operator nothing and hides
// the one fact that would help - whether the proxy refused or was
// unreachable.
function operatorJson(url, options = {}) {
  return operatorFetch(url, options).then(r => {
    if (r.ok) return r.json();
    const err = new Error('request failed (' + r.status + ')');
    err.status = r.status;
    return Promise.reject(err);
  });
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
  pause: { revision: 0, busy: false, valid: st => st?.ok === true && Array.isArray(st.holds), apply: applyPauseState },
  debug: { revision: 0, busy: false, valid: st => st?.ok === true && Array.isArray(st.sessions), apply: applyDebugState },
  throttle: { revision: 0, busy: false, valid: st => st?.ok === true && Array.isArray(st.throttles), apply: applyThrottleState },
};
document.addEventListener('click', e => {
  const button = e.target.closest('[data-operator]');
  if (!button || button.disabled) return;
  const actions = { 'storm-open': openStormDetails, 'storm-close': closeStormDetails,
    'quota-resume': resumeQuotaProvider };
  const action = actions[button.dataset.operator];
  if (action) { e.stopPropagation(); action(button.dataset.value); }
});
function captureOperatorRevisions() {
  return Object.fromEntries(Object.entries(operatorState).map(([key, st]) => [key, st.revision]));
}
function acceptOperator(kind, st, revision) {
  const gate = operatorState[kind];
  const accepted = !gate.busy && revision === gate.revision && gate.valid(st);
  // Every operator document carries the server instant its deadlines are
  // measured against, and this is the one gate the bootstrap and every poll
  // pass through. Anchoring here means no surface can read a server-owned
  // deadline with the workstation clock.
  if (accepted) anchorOperatorClock(st?.server_ms);
  return accepted;
}
// operatorError is the one rejected-write surface. The console names the
// rejection in the pane the operator is working in; when it is not loaded (a
// partial harness), the toast is the fallback.
function operatorError(kind, message) {
  if (typeof ocOperatorError === 'function') { ocOperatorError(message); return; }
  toast(String(message || 'the change was rejected'), 'err');
}
// operatorClearError drops the previous rejection before a new write starts,
// so a stale banner cannot outlive the condition that produced it.
function operatorClearError() {
  if (typeof ocOperatorErrorClear === 'function') ocOperatorErrorClear();
}
// resyncOperatorSurfaces is the shared apply-state epilogue: repaint the
// console when it is mounted, then schedule a live repaint when a snapshot
// exists. The console owns focus-preserving rerenders, so a display-only
// poll needs no separate path.
function resyncOperatorSurfaces(kind) {
  if (typeof operatorConsoleSync === 'function') operatorConsoleSync(kind);
  if (lastData) scheduleRenderLive();
}
// operatorNoun is the console's own word for a kind. The wire route is
// throttle/pause/debug; the operator reads limit/hold/capture and never sees
// the route name anywhere else, so a failure must not answer in it.
const OPERATOR_NOUNS = { throttle: 'limit', pause: 'hold', debug: 'capture' };
function operatorNoun(kind) { return OPERATOR_NOUNS[kind] || kind; }

async function mutateOperator(kind, body, after) {
  const gate = operatorState[kind];
  const noun = operatorNoun(kind);
  // A second action while the first is in flight is a real double click, not
  // a command. Answering it with silence made the click look ignored.
  if (gate.busy) {
    operatorError(kind, 'the ' + noun + ' change is still in flight');
    return false;
  }
  gate.busy = true;
  ++gate.revision;
  operatorClearError();
  if (typeof operatorConsoleSync === 'function') operatorConsoleSync(kind);
  let confirmed;
  const fail = 'could not change the ' + noun;
  try {
    const st = await operatorJsonBody('/admin/' + kind, {
      method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(body),
    }, fail);
    if (!gate.valid(st)) throw new Error(st?.error || fail);
    confirmed = st;
  } catch (err) {
    // A transport failure surfaces the browser's own "Failed to fetch"; the
    // server's answer carries the operatorBody marker. Only the transport case
    // needs the console's context prepended, so the operator learns what was
    // not changed as well as why.
    const message = String(err.message || fail);
    operatorError(kind, err.operatorBody ? message : fail + ' - ' + message);
  } finally {
    gate.busy = false;
    ++gate.revision;
    if (confirmed) {
      // The gate's revision was advanced above and again before this call, so
      // the defaulted revision is the one acceptOperator expects.
      gate.apply(confirmed);
      // The confirmed document is the server's own answer for this write. A
      // callback that needs the identity the write created reads it from
      // here (the capture route returns created_id) instead of inferring it
      // from a before/after list difference, which picks the wrong object
      // when someone else creates one at the same time.
      if (after) after(confirmed);
      if (confirmed.warning) {
        // Persistence warnings are non-fatal: the runtime state is confirmed,
        // so the console names the warning in place and stays exactly where
        // the operator left it.
        operatorError(kind, confirmed.warning);
      }
    }
    if (typeof operatorConsoleSync === 'function') operatorConsoleSync(kind);
    refreshFooterState();
  }
  // Confirmed means the server's own snapshot arrived and passed the gate.
  // Callers that must know (the undo path above all) read this instead of
  // assuming success: the rejection above is shown in the banner and never
  // thrown, so a caller that ignored the result reported a change that was
  // refused. Returning a value is additive - every direct caller still just
  // fires and lets the banner speak.
  return !!confirmed;
}

let _pauseStateSig = '';
let _throttleStateSig = '';
let _debugStateSig = '';
// Each accepted snapshot is deduped by its full document signature, so an
// unchanged poll does no paint work. The console repaints the open pane and
// preserves focus itself when the signature does change.
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
    storm_waiters: typeof st.storm_waiters === 'number' ? st.storm_waiters : 0,
    default_max_queued: typeof st.default_max_queued === 'number' ? st.default_max_queued : 0,
  };
  refreshFooterState();
  resyncOperatorSurfaces('pause');
}

function applyThrottleState(st, revision = operatorState.throttle.revision) {
  if (!acceptOperator('throttle', st, revision)) return;
  const sig = JSON.stringify(st);
  if (sig === _throttleStateSig) return;
  _throttleStateSig = sig;
  throttleState = {
    throttles: Array.isArray(st.throttles) ? st.throttles : [],
    [KNOWN_PROVIDERS_KEY]: Array.isArray(st[KNOWN_PROVIDERS_KEY]) ? st[KNOWN_PROVIDERS_KEY] : [],
    [KNOWN_CLIENTS_KEY]: Array.isArray(st[KNOWN_CLIENTS_KEY]) ? st[KNOWN_CLIENTS_KEY] : [],
    [KNOWN_MODELS_KEY]: Array.isArray(st[KNOWN_MODELS_KEY]) ? st[KNOWN_MODELS_KEY] : [],
    active: !!st.active,
  };
  resyncOperatorSurfaces('throttle');
}
