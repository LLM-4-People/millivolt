// Settings shell and lazy loader: the header trigger, the one-shot versioned
// SETTINGS_MODULES loader, the sheet open-state probe, the shared status line,
// the close entry point, the artifact download owners and the debug surface.
// The sheet itself lives in the lazy settings modules.

// ---------- settings shell and lazy module ----------
// chrome_settings.js keeps only the Settings shell: the header trigger, the one-shot
// versioned loader, the sheet's open-state probe, the shared status line and
// the single close entry point. The sheet's state, rendering, editors,
// apply/revert and backup/restore/purge controls live in the settings
// modules named by SETTINGS_MODULES and load on the first open. Until they
// arrive, close is a DOM-only no-op and Escape cannot touch a draft that
// does not exist.
//
// SETTINGS_MODULES is the one owner list of the sheet's deferred classic
// scripts, in execution order: the entry (state + shell) first, the editor
// modules next and the collection/apply/delegation wiring last. The loader
// appends them one at a time, so no module runs before the module it was
// split from, and index.html never references one (first paint stays free
// of the whole sheet). A new settings module joins this list only.
const SETTINGS_MODULES = [
  'settings.js',
  'settings_backup.js',
  'settings_providers.js',
  'settings_model_rules.js',
  'settings_overrides.js',
  'settings_subconversations.js',
  'settings_providers_actions.js',
  'settings_state.js',
];
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

function settingsModuleURL(name) {
  const version = document.querySelector('meta[name="dashboard-version"]')?.content || '';
  return '/dash/js/' + name + '?v=' + encodeURIComponent(version);
}

// loadSettingsModule is the single-flight sequential loader: concurrent or
// repeated opens share one promise and one pass over SETTINGS_MODULES for the
// page's lifetime, and the promise resolves only after the last module's
// onload registered the sheet owners. from is the list index to start at: the
// production trigger always starts at 0, while a page that included the entry
// directly (the jsdom fixture) resumes after it instead of injecting it twice.
function loadSettingsModule(from = 0) {
  if (window.settingsModule) return Promise.resolve(window.settingsModule);
  if (settingsLoadPromise) return settingsLoadPromise;
  settingsLoadPromise = new Promise((resolve, reject) => {
    let at = from;
    const append = () => {
      if (at >= SETTINGS_MODULES.length) {
        const module = window.settingsModule;
        if (module && typeof module.open === 'function' && typeof module.close === 'function') resolve(module);
        else reject(new Error(SETTINGS_LOAD_FAIL));
        return;
      }
      const script = document.createElement('script');
      script.src = settingsModuleURL(SETTINGS_MODULES[at++]);
      script.async = true;
      // A page may register the sheet module itself (the loader tests do) or
      // a module may register before the list ends: adopt it and stop, exactly
      // as the one-module loader did. The split's last module owns the real
      // registration, so a normal load walks the whole list.
      script.onload = () => {
        const module = window.settingsModule;
        if (module && typeof module.open === 'function' && typeof module.close === 'function') resolve(module);
        else append();
      };
      script.onerror = () => reject(new Error(SETTINGS_LOAD_FAIL));
      document.head.appendChild(script);
    };
    append();
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

// cancelSettingsOpen consumes an Escape aimed at Settings: it closes an open
// sheet through the real owner, or clears a pending open while the lazy modules
// are still loading. The second case matters because the sheet stays hidden
// until the module resolves; without it the loader's want-open check opened the
// sheet after the keypress.
function cancelSettingsOpen() {
  if (settingsIsOpen()) { closeSettings(); return true; }
  if (!settingsWantOpen) return false;
  settingsWantOpen = false;
  return true;
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
    // A close, Escape or another header menu while the module was loading
    // cancels the pending open; reopening while loading keeps the intent
    // true.
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
