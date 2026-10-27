// Pause, debug and limits operator menus: the shared keyed reconcilers, their
// rows, checklists and pickers, the footer state and the post-mutation
// refresh. Loaded before chrome_menus.js because FILTER_AGES reads
// DURATION_LABELS from this module at initialization.

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
// LIMIT_RAMPS is the single owner of the anti-burst ramp dropdown: the off
// pair plus the same curated window vocabulary the capped limits use.
const LIMIT_RAMPS = [['', 'off']].concat(LIMIT_WINDOWS);
function fillLimitWindows(sel, value) {
  fillSelectPairs(sel, LIMIT_WINDOWS, value || LIMIT_WINDOW_DEFAULT);
}
function fillLimitPolicy(force = false) {
  const name = $('lim-provider') ? $('lim-provider').value : '';
  const t = limitByProvider(name);
  const anti = !!(t && t.anti_burst);
  const ramp = (t && t.ramp_duration) || '';
  const sig = JSON.stringify([name, anti, ramp]);
  if (!force && sig === _limitsPolicySig) return;
  _limitsPolicySig = sig;
  const box = $('lim-antiburst');
  if (box) box.checked = anti;
  fillSelectPairs($('lim-ramp'), LIMIT_RAMPS, ramp);
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
  fillLimitPolicy();
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
  fillLimitPolicy(true);
  operatorCountClear('throttle');
  updateLimitCount();
  updateLimitsApplyEnabled();
}

function limitPolicyBits(t) {
  if (!t || !t.anti_burst) return [];
  return ['anti-burst' + (t.ramp_duration ? ' ' + t.ramp_duration : '')];
}

function updateLimitCount() {
  const count = $('lim-count');
  if (!count || count.dataset.err) return;
  const name = $('lim-provider') ? $('lim-provider').value : '';
  const t = limitByProvider(name);
  if (t) {
    const bits = [];
    if (t.source) bits.push(t.source === 'header' ? 'set by header' + (t.updated_by ? ' · ' + t.updated_by : '') : 'set in UI');
    bits.push(...limitPolicyBits(t));
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
  const anti = $('lim-antiburst');
  const ramp = $('lim-ramp');
  // A ramp only shapes an active cap: an explicit on-ramp with every cap at
  // zero is a server-side 400, so the form's empty state clears the policy.
  const live = conc > 0 || req > 0 || tok > 0;
  const body = {
    provider,
    concurrency: conc,
    requests: req,
    request_window: req > 0 ? ($('lim-reqw') ? $('lim-reqw').value : LIMIT_WINDOW_DEFAULT) : '',
    tokens: tok,
    token_window: tok > 0 ? ($('lim-tokw') ? $('lim-tokw').value : LIMIT_WINDOW_DEFAULT) : '',
    anti_burst: live && !!(anti && anti.checked),
    ramp_duration: live && ramp ? ramp.value : '',
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
  bits.push(...limitPolicyBits(t));
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
    !!(t.anti_burst), t.ramp_duration || '',
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
