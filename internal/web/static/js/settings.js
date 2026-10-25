// Settings sheet entry: the accepted document, the form shell, open/close and
// the fetch/refill pass. chrome.js owns the header trigger, the lazy loader
// (SETTINGS_MODULES) and the open-state probe; the editor, state and backup
// surfaces live in the settings modules that list loads after this one, on
// the first Settings open. Keep every moved global global: the dashboard is
// not an ES module and the existing tests drive these names directly.

let settingsDoc = null;
// The rendered form's document identity. settingsDoc can advance on a
// background refresh while the sheet is closed; the retained rows still
// belong to these values, so a no-op refill must compare against them.
let settingsFormRevision = '';
let settingsFormWritable = null;

let settingsCat = '';
let settingsSnap = '';
// The full snapshot above is the exact saved draft identity. The per-key map
// and changed set make the keystroke path incremental: each entry is the JSON
// of one key's collected value, rebuilt whenever the form is refilled or a
// save establishes a new baseline.
let settingsSnapByKey = null;
let settingsChangedKeys = new Set();
let settingsKeyEls = new Map(); // config key -> its [data-key] control/editor
let settingsQ = '';
let settingsReq = 0; // newest settings request owns state; saves invalidate older GETs
let _purgeBusy = false; // one in-flight gate for both storage deletions

const SETTINGS_CAT_REF = {
  server:       { dim: 'status' },
  request:      { dim: 'request' },
  upstream:     { dim: 'provider' },
  queue:        { dim: null, icon: '↻', color: 'var(--warn)' },
  storm:        { dim: null, icon: '⚠', color: 'var(--warn)' },
  quota:        { dim: null, icon: '⊘', color: 'var(--warn)' },
  conversation: { dim: 'conversation' },
  format:       { dim: 'model' },
  storage:    { dim: 'key' },
  backup:     { dim: null, icon: '⬇', color: 'var(--accent)' },
  mcp:        { dim: 'tool' },
  dashboard:  { dim: 'time' },
  models:     { dim: 'model' },
  providers:  { dim: 'provider' },
  overrides:  { dim: 'request' },
};

function settingsCatVisual(id) {
  const r = SETTINGS_CAT_REF[id] || SETTINGS_CAT_REF.server;
  if (r.dim && ENTITY_TYPES[r.dim]) {
    return { icon: ENTITY_TYPES[r.dim].icon, color: ENTITY_TYPES[r.dim].color };
  }
  return { icon: r.icon, color: r.color || ENTITY_TYPES.status.color };
}

function settingsIsDirty() {
  if (!(settingsDoc && settingsDoc.writable && settingsSnap)) return false;
  // The dirty path maintains the changed-key set beside the per-key
  // baseline, so the hot predicate reads just that set. Only a form without
  // the per-key baseline needs the original full fingerprint comparison.
  if (settingsSnapByKey) return settingsChangedKeys.size > 0;
  return settingsFingerprint() !== settingsSnap;
}

function settingsDirtyCount(values) {
  if (!settingsSnap) return 0;
  // The dirty path maintains the changed-key set directly; callers that pass
  // an explicit collection (or run before a per-key snapshot exists) keep the
  // original full comparison, so the count cannot drift from the saved shape.
  if (!values && settingsSnapByKey) return settingsChangedKeys.size;
  try {
    const a = JSON.parse(settingsSnap);
    const b = values || collectSettingsValues();
    const keys = new Set([...Object.keys(a), ...Object.keys(b)]);
    let n = 0;
    keys.forEach(k => {
      if (JSON.stringify(a[k]) !== JSON.stringify(b[k])) n++;
    });
    return n;
  } catch (e) { return settingsIsDirty() ? 1 : 0; }
}

function settingsRestartNotice(doc) {
  const keys = doc?.restart_required || [];
  return keys.length ? 'restart needed for: ' + keys.join(', ') : '';
}

// settingsBaseStatus is the idle status line shared by a full refill and the
// unchanged-document no-op refill.
function settingsBaseStatus(doc) {
  return doc.writable ? settingsRestartNotice(doc) : 'in-memory only - start with -config to persist';
}

// openSettings is the sheet's open owner, called by the chrome.js trigger
// after this module loads. It preserves the original open sequence.
function openSettings() {
  if (settingsIsOpen()) { settingsClose(); return; }
  closeHeaderMenus('settings');
  closeNavMenu();
  if (typeof closeDrawer === 'function') closeDrawer();
  const s = $('settings-sheet');
  const v = $('settings-veil');
  const b = $('btn-settings');
  if (!s) return;
  // A competing modal (the operator prompt) preempts the sheet through the
  // generic close; run this owner's teardown so the veil and pressed state
  // cannot survive the takeover.
  openModal(s, {onPreempt: () => settingsClose(true)});
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

// settingsClose is the real close owner behind chrome.js's closeSettings
// delegate: the dirty-draft guard, focus restoration and veil/modal teardown
// are unchanged.
function settingsClose(force) {
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
  const wasOpen = settingsIsOpen();
  if (s) { s.classList.remove('open'); closeModal(s); }
  if (v) v.classList.remove('open');
  if (b) b.setAttribute('aria-pressed', 'false');
  if (wasOpen && !force) {
    // The Settings opener lives inside the Actions flyout, which closed
    // before the modal opened; closeModal's stored element is now hidden and
    // cannot receive focus. Return to the visible header control instead.
    visibleHeaderControl()?.focus();
  }
}

// settingsCanSkipFill reports whether an accepted document can leave the
// retained form untouched: the sheet is open over a baseline with rendered
// rows, and the document identity matches the one the form was built from.
function settingsCanSkipFill(doc) {
  return !!(settingsIsOpen() && settingsDoc && settingsSnap && settingsKeyEls.size &&
    doc.revision === settingsFormRevision && doc.writable === settingsFormWritable);
}

function fetchSettings(discard = false) {
  const req = ++settingsReq;
  // Only an explicit Revert needs the draft identity it is allowed to
  // discard; an ordinary refresh checks the incremental dirty predicate at
  // response time instead of collecting the whole form up front.
  const draft = discard ? settingsFingerprint() : '';
  return operatorJsonBody('/admin/config', {}, SETTINGS_LOAD_FAIL)
    .then(doc => {
      if (!Array.isArray(doc.fields) || !doc.values || !doc.revision) throw new Error(doc.error || SETTINGS_LOAD_FAIL);
      return doc;
    })
    .then(doc => {
      if (req !== settingsReq) return null;
      applyDashValues(doc.effective || doc.values);
      const saving = $('btn-settings-apply')?.dataset.busy;
      // Restart/background refreshes never replace a draft. Explicit Revert
      // may discard only the draft it started with, not subsequent typing.
      if (saving) return doc;
      if (settingsIsDirty() && !(discard && draft === settingsFingerprint())) return doc;
      // A clean draft over the exact document identity the form was built
      // from is a no-op refill: adopt the fresh server state and repaint the
      // shared status/actions and search/category view without rebuilding
      // rows, revalidating editors or replacing the accepted baseline.
      const skip = !discard && settingsCanSkipFill(doc);
      settingsDoc = doc;
      if (settingsIsOpen()) {
        if (skip) {
          settingsStatus(settingsBaseStatus(doc));
          // The live snapshot (last apply, effective listen/store) changes
          // independently of the form revision. Refresh only this panel; the
          // rendered rows, editor validation state and accepted baseline stay
          // untouched so focus, scroll and the clean draft survive.
          fillSettingsLive(doc);
          showSettingsCat();
          updateSettingsActions();
        } else {
          fillSettingsForm(doc);
        }
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
    // No accepted document means no draft baseline: an old snapshot must not
    // keep the form dirty (or Apply armed) after a failed reload.
    settingsSnap = '';
    settingsSnapByKey = null;
    settingsChangedKeys = new Set();
    settingsKeyEls = new Map();
    settingsFormRevision = '';
    settingsFormWritable = null;
    settingsStatus(SETTINGS_LOAD_FAIL);
    updateSettingsActions();
    return;
  }
  settingsDoc = doc;
  settingsFormRevision = doc.revision;
  settingsFormWritable = doc.writable;
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
  // The storage pane also owns both database deletions: the scoped
  // delete-matching block and the whole-database action row ride the same
  // innerHTML rebuild as every field and the backup rows, inserted after
  // the storage fields so DOM order follows category order - least
  // destructive first.
  const rows = doc.fields.map(f => settingsFieldHTML(f, values[f.key], defaults[f.key], overrides[f.key]));
  const purgeAt = doc.fields.reduce((at, f, i) => f.category === 'storage' ? i + 1 : at, -1);
  if (purgeAt >= 0) rows.splice(purgeAt, 0, settingsDeleteRowHTML(), settingsPurgeRowHTML());
  box.innerHTML = rows.join('') + settingsBackupHTML(doc);
  box.querySelectorAll('[data-st-scalar]').forEach(validateSettingsScalar);
  wireBackupPane();
  const purgeAllBtn = $('btn-purge-all');
  if (purgeAllBtn) purgeAllBtn.disabled = _purgeBusy;
  if (purgeAt >= 0) {
    // The rebuilt selects start empty (settingsDeleteRowHTML): repopulate
    // their options from the current records, then restart the count
    // preview from the fresh, empty selection.
    populateFilterMenu('del');
    updateDelCount();
    // A refill during a purge must not resurrect enabled controls: the
    // fresh instances inherit the busy state, and the purge's epilogue
    // re-enables whichever instance is current.
    document.querySelectorAll('#settings-fields [data-purge="matching"] select').forEach(el => { el.disabled = _purgeBusy; });
  }
  syncProvMenuList(box.querySelector('.st-row[data-key="providers"]'));
  // Initial paint for every rules editor: validation states, preview bench,
  // rule count - the same pass the delegated events run on every edit.
  box.querySelectorAll('.mr-wrap').forEach(wrap => { validateModelRulesDraft(wrap); mrPreview(wrap); });
  box.querySelectorAll('.ro-wrap').forEach(wrap => { validateRequestOverridesDraft(wrap); roSyncCount(wrap); });
  box.querySelectorAll('.sc-wrap').forEach(wrap => { validateSubConversationsDraft(wrap); scSyncCount(wrap); });
  fillSettingsLive(doc);
  settingsStatus(settingsBaseStatus(doc));
  // Cache each rendered row's search haystack beside the value snapshot: the
  // per-keystroke filter and every rail badge read this copy instead of the
  // row dataset text.
  settingsCacheHaystacks(box);
  // One full collection establishes the accepted baseline for both the full
  // snapshot and the per-key map. Every later keystroke compares just its own
  // key against these entries; a refill starts the draft clean.
  settingsCacheSnapshots(collectSettingsValues());
  settingsKeyEls = settingsKeyElementMap(box);
  showSettingsCat();
  updateSettingsActions();
}

// The entry is SETTINGS_MODULES[0] (chrome.js owns the list). A page that
// included this script directly, without the production loader - the jsdom
// suite does - still gets the rest of the sheet: resume the one sequential
// pass after this module. Under the production loader that promise is
// already in flight, so this call only returns it.
loadSettingsModule(1).catch(() => {});

// ---- sub-conversations editor ----
// sub_conversations: per-client tracking of request body fields whose
// value identifies a sub-conversation (exemplar: opencode's
// promptCacheKey). One visual grammar with the request-overrides editor
// (the ro family): each entry is a .prov-sec card inside an .mr-wrap
// shell (hint, tools row, count, muted tokens), the client input gets
// datalist autocomplete from the same known-set union the ro scope
// datalists read, and the ordered param rows are inputs, not chips,
// because order is meaning (the first present field decides: a non-string
// value counts as absent and the next field is checked, while a present
// string that cannot be decoded or fails the identity rules drops the
// identity - no later field is consulted). Live per-card
// validation mirrors config.validateSubConversations at every keystroke
// (the validateRoRow discipline): the client required, 1..4 params, the
// one JSON-key-segment grammar with the server's exact message wording,
// duplicate params within the entry, duplicate clients across entries.
// Apply walks the draft and blocks the POST on the first invalid card,
// focusing the offending input. The count line and add gates mirror
// config.SubConversationsMax (16) and SubConversationParamsMax (4); the
// live grammar check mirrors the 64-byte bound.
const SUB_CONVERSATIONS_MAX = 16;
const SUB_CONVERSATION_PARAMS_MAX = 4;
const SUB_CONVERSATION_PARAM_MAX_BYTES = 64;

// scParamOK mirrors config.checkSubConversationParam's per-byte grammar:
// one JSON object key segment - printable ASCII only, no quote, no
// backslash. The empty and 64-byte rules are judged by their own
// server-worded checks in validateScCard; this predicate owns the byte
// grammar half (the roHeaderValueOK class).
function scParamOK(name) {
  for (let i = 0; i < name.length; i++) {
    const c = name.charCodeAt(i);
    if (c < 0x20 || c > 0x7e || c === 0x22 || c === 0x5c) return false;
  }
  return true;
}

// scEntryLabel owns the entry-number label ("entry N", the 1-based index
// beside the sub_conversations[i] indexes the server cites in its
// validation errors) shared by the card render and scRenumber, so the
// initial label and the rewritten one cannot drift.
function scEntryLabel(i) {
  return 'entry ' + (i + 1);
}

// scEntryFieldLabel composes an ordinal-carrying input's accessible name
// from the same scEntryLabel ordinal the visible entry number uses.
function scEntryFieldLabel(i, suffix) {
  return scEntryLabel(i) + ' ' + suffix;
}

// scEntryFieldAttrs renders an ordinal-carrying input's accessible-name
// attributes: the initial aria-label composes through scEntryLabel, and
// data-sc-lbl carries the suffix scRenumber restamps the label from.
function scEntryFieldAttrs(i, suffix) {
  return `aria-label="${escapeHtml(scEntryFieldLabel(i, suffix))}" data-sc-lbl="${escapeHtml(suffix)}"`;
}

// scDlId owns the client datalist id grammar shared by the generator, the
// entry card's list attribute and the focus-time refresher (the roDlId
// discipline), so the association cannot drift between render and
// refresh.
function scDlId() {
  return 'sc-dl-client';
}

// scDlOptionsHTML owns the datalist options expression shared by the
// initial render and the focus-time refresher: both derive the same
// option list from the same known-set union the ro scope datalists read
// (the wire-key owners), so a classified client observed anywhere on the
// dashboard is offered here too. Free text stays allowed because an
// entry may target a client not yet observed.
function scDlOptionsHTML() {
  return roKnownOptions('client').map(o => `<option value="${escapeHtml(o)}"></option>`).join('');
}

function scDatalistHTML() {
  return `<datalist id="${scDlId()}">${scDlOptionsHTML()}</datalist>`;
}

// scSyncDatalists refreshes the autocomplete options at focus time: the
// known sets arrive on their own refresh cadence, so options picked at
// render time can be stale while the sheet is open.
function scSyncDatalists(wrap) {
  if (!wrap) return;
  const dl = wrap.querySelector('#' + scDlId());
  if (dl) dl.innerHTML = scDlOptionsHTML();
}

// scParamRowHTML renders one ordered tracked-field row inside an input
// line that stretches like the ro scope rows.
function scParamRowHTML(name) {
  return `<div class="st-ctl-line ro-scope sc-prow"><input class="sc-param" value="${escapeHtml(name)}" placeholder="body field - e.g. promptCacheKey" aria-label="tracked body field"><button type="button" class="prov-x" data-sc-p-rm aria-label="remove tracked body field" title="remove">✕</button></div>`;
}

// scCardHTML renders one entry card: the classified client (datalist
// autocomplete), the ordered param rows with their add row, and the
// strip toggle whose label states the forward/strip semantics. The
// client input carries the entry ordinal through the one composer.
function scCardHTML(e, i) {
  e = e || {};
  const params = Array.isArray(e.params) ? e.params : [];
  return `<div class="prov-sec sc-card">` +
    `<div class="prov-lb"><span class="sc-num">${escapeHtml(scEntryLabel(i))}</span><span class="prov-sub">one classified client, tracked exactly</span><button type="button" class="prov-x" data-sc-rm aria-label="remove entry" title="remove entry">✕</button></div>` +
    `<div class="st-ctl-line ro-scope">` +
    `<input class="sc-client" list="${scDlId()}" value="${escapeHtml(e.client || '')}" placeholder="classified client - e.g. opencode" ${scEntryFieldAttrs(i, 'client')}>` +
    `</div>` +
    `<div class="prov-lb"><span>params</span><span class="prov-sub">exact top-level request body fields, checked in order - the first present field decides</span></div>` +
    `<div class="sc-params">${params.map(p => scParamRowHTML(String(p == null ? '' : p))).join('')}</div>` +
    `<div class="prov-add"><input class="sc-param-in" placeholder="body field - e.g. promptCacheKey ↵" aria-label="add tracked body field"><button type="button" class="btn prov-addbtn" data-sc-param-add aria-label="add tracked body field">+</button></div>` +
    `<label class="st-check"><input type="checkbox" class="st-switch sc-strip"${e.strip ? ' checked' : ''}><span>strip every configured field from the top level of the client request body before relay or translation - unchecked forwards it unchanged</span></label>` +
    `<div class="mr-err sc-err" aria-live="polite"></div>` +
    `</div>`;
}

function subConversationsEditorHTML(val) {
  const entries = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap sc-wrap">` +
    `<div class="mr-hint">each entry tracks one classified client's sub-conversation identity from top-level request body fields · the first present field decides: a present value that is not a JSON string counts as absent and the next field is checked, while a present string that cannot be decoded or fails the identity rules drops the identity for the request - no later field is consulted, never a client error · the value groups requests under a k: identity, after X-Proxy-Session and before client+key grouping · strip removes every configured field present at the top level of the client request body before relay or translation</div>` +
    `<div class="mr-tools"><button type="button" class="btn" data-sc-add aria-label="add entry">+ add entry</button><span class="mr-count muted"></span></div>` +
    `<div class="sc-rows">${entries.map((e, i) => scCardHTML(e, i)).join('')}</div>` +
    scDatalistHTML() +
    `</div>`;
}

// addScCard appends one blank entry card and moves focus to its client
// input (the guided-input discipline: the live validation immediately
// asks for the client, then one param). The add gate refuses beyond
// config.SubConversationsMax.
function addScCard(wrap) {
  if (!wrap || wrap.querySelectorAll('.sc-card').length >= SUB_CONVERSATIONS_MAX) return;
  const rows = wrap.querySelector('.sc-rows');
  rows.insertAdjacentHTML('beforeend', scCardHTML({}, rows.children.length));
  scDraftChanged(wrap);
  const card = [...wrap.querySelectorAll('.sc-card')].pop();
  const focus = card && card.querySelector('.sc-client');
  if (focus) focus.focus();
}

// addScParamRow appends one tracked-field row from the add inputs. Deny
// by default on the server's own grammar (the addRoRemoveHeader
// discipline): the per-entry cap, an empty name, a grammar violation,
// the 64-byte bound, or a duplicate within the entry flashes the add
// input instead of adding a row the server would refuse.
function addScParamRow(card) {
  const input = card && card.querySelector('.sc-param-in');
  const box = card && card.querySelector('.sc-params');
  if (!input || !box) return;
  if (box.querySelectorAll('.sc-prow').length >= SUB_CONVERSATION_PARAMS_MAX) { flashBadInput(input); return; }
  const name = String(input.value || '').trim();
  if (!name || !scParamOK(name) || name.length > SUB_CONVERSATION_PARAM_MAX_BYTES) { flashBadInput(input); return; }
  if ([...box.querySelectorAll('.sc-param')].some(p => String(p.value || '').trim() === name)) { flashBadInput(input); return; }
  box.insertAdjacentHTML('beforeend', scParamRowHTML(name));
  input.value = '';
  scDraftChanged(card.closest('.sc-wrap'));
}

// validateScCard is the live per-card gate (the validateRoRow
// discipline): every check mirrors config.validateSubConversations in
// the server's own order and message wording (the
// sub_conversations[i].key prefix becomes the card's inline context; %q
// becomes the quoted name). Checks that own an input redden it; the
// duplicate-client cross-card check marks no input (it cites an earlier
// card, not this one); the first failure owns the message text. seen
// maps trimmed clients to their 1-based entry number for the duplicate
// wording, first occurrence only - a duplicate carrying another error
// must not shift the ordinal later duplicates cite; index is this
// card's 0-based position.
function validateScCard(card, seen, index) {
  const err = card.querySelector('.sc-err');
  card.querySelectorAll('.prov-bad').forEach(el => el.classList.remove('prov-bad'));
  const clientIn = card.querySelector('.sc-client');
  const client = String((clientIn || {}).value || '').trim();
  const paramRows = [...card.querySelectorAll('.sc-prow')];
  let level = 'ok', msg = '';
  const fail = (input, text) => {
    if (input) input.classList.add('prov-bad');
    if (level !== 'error') { level = 'error'; msg = text; }
  };
  if (!client) {
    fail(clientIn, 'client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry');
  }
  if (!paramRows.length) {
    fail(card.querySelector('.sc-param-in'),
      'no param set - give the entry a request body field to track, or remove the entry');
  }
  const seenParams = new Set();
  paramRows.forEach(row => {
    const input = row.querySelector('.sc-param');
    const name = String((input || {}).value || '').trim();
    if (!name) { fail(input, 'params: must not be empty or only whitespace'); return; }
    if (!scParamOK(name)) { fail(input, `params: '${name}' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`); return; }
    if (name.length > SUB_CONVERSATION_PARAM_MAX_BYTES) { fail(input, `params: must be 1..${SUB_CONVERSATION_PARAM_MAX_BYTES} bytes, got ${name.length}`); return; }
    if (seenParams.has(name)) { fail(input, `params: duplicate '${name}'`); return; }
    seenParams.add(name);
  });
  if (level === 'ok' && seen && seen.has(client)) {
    level = 'error';
    msg = `duplicate client '${client}' with entry ${seen.get(client)}; merge the entries or change one client`;
  } else if (seen && client && !seen.has(client)) {
    seen.set(client, index + 1);
  }
  card.dataset.scState = level;
  if (err) { err.textContent = msg; err.dataset.level = level === 'error' ? 'error' : ''; }
  return level;
}

// validateSubConversationsDraft walks every entry card in order and
// returns the first invalid one (Apply blocks on it; the button flow
// focuses and flashes it).
function validateSubConversationsDraft(wrap) {
  const seen = new Map();
  let firstBad = null;
  wrap.querySelectorAll('.sc-card').forEach((card, i) => {
    if (validateScCard(card, seen, i) === 'error' && !firstBad) firstBad = card;
  });
  return { ok: !firstBad, firstBad };
}

// scRenumber rewrites the cards' entry numbers after any add or remove so
// they keep matching the sub_conversations[i] indexes the server cites in
// its validation errors. The ordinal-carrying accessible names restamp
// through the same composer (scEntryFieldLabel reads data-sc-lbl), so a
// non-tail removal cannot leave a client input announcing a stale entry
// number.
function scRenumber(wrap) {
  wrap.querySelectorAll('.sc-card').forEach((card, i) => {
    const el = card.querySelector('.sc-num');
    if (el) el.textContent = scEntryLabel(i);
    card.querySelectorAll('[data-sc-lbl]').forEach(input => {
      input.setAttribute('aria-label', scEntryFieldLabel(i, input.dataset.scLbl));
    });
  });
}

// scSyncCount keeps the "N / 16 entries" line honest and gates the add
// button at the cap (mirrors config.SubConversationsMax).
function scSyncCount(wrap) {
  const n = wrap.querySelectorAll('.sc-card').length;
  const count = wrap.querySelector('.mr-count');
  if (count) count.textContent = n + ' / ' + SUB_CONVERSATIONS_MAX + ' entries';
  const add = wrap.querySelector('[data-sc-add]');
  if (add) add.disabled = n >= SUB_CONVERSATIONS_MAX;
}

// scDraftChanged is the sub-conversations draft-changed epilogue (the
// roDraftChanged discipline): mark the sheet dirty, revalidate the whole
// draft, renumber the cards and refresh the count.
function scDraftChanged(wrap) {
  markSettingsDirty(settingsKeyFor(wrap));
  validateSubConversationsDraft(wrap);
  scRenumber(wrap);
  scSyncCount(wrap);
}

// collectSubConversations reads the editor into the strict POST shape:
// the client and strip always ride along, param names are trimmed like
// the server trims them, and an empty params list stays absent so an
// entry carries only what it sets. The live gate blocks an empty-params
// card before the POST; the server re-validates everything.
function collectSubConversations(wrap) {
  const out = [];
  wrap.querySelectorAll('.sc-card').forEach(card => {
    const e = Object.create(null);
    e.client = String((card.querySelector('.sc-client') || {}).value || '').trim();
    const params = [...card.querySelectorAll('.sc-param')].map(p => String(p.value || '').trim());
    if (params.length) e.params = params;
    const strip = card.querySelector('.sc-strip');
    e.strip = !!(strip && strip.checked);
    out.push(e);
  });
  return out;
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
  markSettingsDirty(settingsKeyFor(row));
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
  const known = (typeof pauseState === 'object' && pauseState && Array.isArray(pauseState[KNOWN_PROVIDERS_KEY])) ? pauseState[KNOWN_PROVIDERS_KEY] : [];
  list.innerHTML = known.filter(p => !taken.has(p)).map(p => `<button type="button" class="prov-menu-item" data-prov-pick="${escapeHtml(p)}" role="menuitem"><span class="prov-ic" style="--ent:${ENTITY_TYPES.provider.color}" aria-hidden="true">☁</span>${escapeHtml(p)}</button>`).join('');
}

// INPUT_FLASH_MS is the invalid-input red flash: long enough to register,
// short enough that a repeated denial re-flashes.
const INPUT_FLASH_MS = 1200;
function flashBadInput(el) {
  if (!el) return;
  el.classList.add('prov-bad');
  setTimeout(() => {
    if (el.getAttribute && el.getAttribute('aria-invalid') === 'true') return;
    el.classList.remove('prov-bad');
  }, INPUT_FLASH_MS);
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
  markSettingsDirty(settingsKeyFor(sec));
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
  markSettingsDirty(settingsKeyFor(sec));
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
  const trim = sec.closest('.ro-wrap') ? trimServerSpace : value => String(value == null ? '' : value).trim();
  const name = trim(nameIn.value);
  const value = trim(valIn.value);
  if (!name || !PROV_HEADER_RE.test(name)) { flashBadInput(nameIn); return; }
  if (!value) { flashBadInput(valIn); return; }
  if ([...map.querySelectorAll('.prov-hrow .sp-hname')].some(n => trim(n.value) === name)) { flashBadInput(nameIn); return; }
  map.insertAdjacentHTML('beforeend', headerRowHTML(name, value));
  nameIn.value = '';
  valIn.value = '';
  markSettingsDirty(settingsKeyFor(sec));
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
  markSettingsDirty(settingsKeyFor(wrap));
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

// setProvMenuOpen is the one open-state writer for the provider picker
// menus: the menu's visibility and its toggle's aria-expanded always move
// together, on every open and close path.
function setProvMenuOpen(menu, open) {
  menu.hidden = !open;
  const btn = menu.parentElement && menu.parentElement.querySelector('[data-prov-menu-toggle]');
  if (btn) btn.setAttribute('aria-expanded', String(!!open));
}

function closeProvMenus(scope) {
  (scope || document).querySelectorAll('.prov-menu:not([hidden])').forEach(m => setProvMenuOpen(m, false));
}

function roRemovalFocusTarget(container, removed, itemSelector, controlSelector, fallbackSelector) {
  if (!container || !removed) return null;
  const items = [...container.querySelectorAll(itemSelector)];
  const index = items.indexOf(removed);
  const fallbackRoot = removed.closest('.ro-rule') || container;
  const target = items[index + 1] || items[index - 1] || fallbackRoot.querySelector(fallbackSelector);
  if (!target) return null;
  return target.matches(controlSelector) || target.matches(fallbackSelector) ? target : target.querySelector(controlSelector);
}

function providersEditorClick(e) {
  const t = e.target;
  if (t.closest('[data-al-rm]')) { const key = settingsKeyFor(t); t.closest('.al-row').remove(); markSettingsDirty(key); return; }
  if (t.closest('[data-al-add]')) { addAliasRow(t.closest('.prov-add')); return; }
  if (t.closest('[data-mr-rm]')) { const wrap = t.closest('.mr-wrap'); t.closest('.mr-row').remove(); mrDraftChanged(wrap); return; }
  if (t.closest('[data-mr-add]')) { addModelRuleRow(t.closest('.mr-add')); return; }
  if (t.closest('[data-mr-up]')) { const row = t.closest('.mr-row'); const prev = row.previousElementSibling; const wrap = t.closest('.mr-wrap'); if (prev) row.parentNode.insertBefore(row, prev); mrDraftChanged(wrap); return; }
  if (t.closest('[data-mr-dn]')) { const row = t.closest('.mr-row'); const next = row.nextElementSibling; const wrap = t.closest('.mr-wrap'); if (next) row.parentNode.insertBefore(next, row); mrDraftChanged(wrap); return; }
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
    // off is the PRE-toggle state (the row flip above already applied the
    // new one), so the title ternary from the template does not transplant
    // verbatim: the post-toggle title must match what a fresh render of
    // the new state shows (a parked row offers un-parking).
    btn.title = !off ? MR_VIS_TITLE_PARKED : MR_VIS_TITLE_ACTIVE;
    mode.disabled = !off;
    row.querySelector('.mr-from').disabled = !off || lower;
    row.querySelector('.mr-to').disabled = !off || lower;
    mrDraftChanged(wrap);
    return;
  }
  if (t.closest('[data-mr-restore]')) { mrRestoreDefaults(t.closest('.mr-wrap')); return; }
  if (t.closest('[data-ro-add]')) {
    const add = t.closest('[data-ro-add]');
    const menu = add.closest('.ro-add') && add.closest('.ro-add').querySelector('.ro-tpl-menu');
    if (menu) {
      const open = menu.hidden;
      roSetTemplateMenuOpen(menu, open);
      if (open) {
        const first = menu.querySelector('button:not(:disabled)');
        if (first) first.focus();
      }
    }
    return;
  }
  if (t.closest('[data-ro-tpl]')) { roApplyTemplate(t.closest('[data-ro-tpl]')); return; }
  if (t.closest('.ro-tpl-menu')) return;
  if (t.closest('[data-ro-toggle]')) {
    const card = t.closest('.ro-rule');
    roSetCardExpanded(card, card.dataset.roExpanded !== '1');
    return;
  }
  if (t.closest('[data-ro-up], [data-ro-dn]')) {
    const button = t.closest('[data-ro-up], [data-ro-dn]');
    const card = button.closest('.ro-rule');
    const wrap = button.closest('.ro-wrap');
    const sibling = button.matches('[data-ro-up]') ? card.previousElementSibling : card.nextElementSibling;
    if (sibling) {
      if (button.matches('[data-ro-up]')) card.parentNode.insertBefore(card, sibling);
      else card.parentNode.insertBefore(sibling, card);
      roDraftChanged(wrap);
    }
    button.focus();
    return;
  }
  if (t.closest('[data-ro-rm]')) {
    const card = t.closest('.ro-rule');
    const wrap = card.closest('.ro-wrap');
    const before = [...wrap.querySelectorAll('.ro-rule')];
    const index = before.indexOf(card);
    card.remove();
    roDraftChanged(wrap);
    const after = [...wrap.querySelectorAll('.ro-rule')];
    const target = after[index] || after[index - 1] || wrap.querySelector('[data-ro-add]');
    if (target) target.focus();
    return;
  }
  if (t.closest('[data-ro-rh-add]')) { addRoRemoveHeader(t.closest('.ro-rule')); return; }
  if (t.closest('[data-sc-rm]')) { const wrap = t.closest('.sc-wrap'); t.closest('.sc-card').remove(); scDraftChanged(wrap); return; }
  if (t.closest('[data-sc-p-rm]')) { const wrap = t.closest('.sc-wrap'); t.closest('.sc-prow').remove(); scDraftChanged(wrap); return; }
  if (t.closest('[data-sc-param-add]')) { addScParamRow(t.closest('.sc-card')); return; }
  if (t.closest('[data-sc-add]')) { addScCard(t.closest('.sc-wrap')); return; }
  if (t.closest('[data-prov-rm]')) { const key = settingsKeyFor(t); t.closest('.st-prov').remove(); markSettingsDirty(key); return; }
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
      // The pre-flip hidden value is the new open state.
      setProvMenuOpen(menu, menu.hidden);
    }
    return;
  }
  if (t.closest('.prov-menu')) {
    const pick = t.closest('[data-prov-pick]');
    if (pick) { addProviderLabel(pick.dataset.provPick, pick); return; }
    if (t.closest('[data-prov-add]')) { addProviderCard(t.closest('[data-prov-add]')); return; }
    return; // clicks inside the open menu close nothing but themselves
  }
  if (t.closest('[data-prov-chip-rm]')) {
    const chip = t.closest('.prov-chip');
    const roW = t.closest('.ro-wrap');
    const key = settingsKeyFor(chip);
    const focusTarget = roW ? roRemovalFocusTarget(chip.parentElement, chip, '.prov-chip', '[data-prov-chip-rm]', '.ro-rh-in') : null;
    chip.remove();
    if (roW) {
      roDraftChanged(roW);
      if (focusTarget) focusTarget.focus();
    } else markSettingsDirty(key);
    return;
  }
  // Usage and model mapping rows share one remove branch: both remove their
  // .prov-urow and re-sync the kind's picker (the render/add twins already
  // share mappingRowHTML).
  const rowRm = t.closest('[data-prov-urow-rm], [data-prov-mrow-rm]');
  if (rowRm) {
    const sec = t.closest('.prov-sec');
    const key = settingsKeyFor(sec);
    t.closest('.prov-urow').remove();
    syncMappingPicker(sec, rowRm.matches('[data-prov-urow-rm]') ? 'usage' : 'model');
    markSettingsDirty(key);
    return;
  }
  if (t.closest('[data-prov-add-cost]')) { addCostKey(t.closest('.prov-sec')); return; }
  if (t.closest('[data-prov-add-usage]')) { addUsageRow(t.closest('.prov-sec')); return; }
  if (t.closest('[data-prov-add-model]')) { addModelRow(t.closest('.prov-sec')); return; }
  if (t.closest('[data-prov-hrow-rm]')) {
    const row = t.closest('.prov-hrow');
    const roW = t.closest('.ro-wrap');
    const key = settingsKeyFor(row);
    const focusTarget = roW ? roRemovalFocusTarget(row.parentElement, row, '.prov-hrow', '.sp-hname', '.prov-add-h .sp-hname') : null;
    row.remove();
    if (roW) {
      roDraftChanged(roW);
      if (focusTarget) focusTarget.focus();
    } else markSettingsDirty(key);
    return;
  }
  if (t.closest('[data-prov-add-header]')) { addHeaderRow(t.closest('.prov-sec')); const roW = t.closest('.ro-wrap'); if (roW) roDraftChanged(roW); return; }
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
    if (e.target.closest && (e.target.closest('.st-backup') || e.target.closest('[data-backup]') || e.target.closest('[data-purge]'))) return;
    // The model-rules test spelling is preview-only: it cannot change the
    // collected draft, so it must never collect a key or arm Apply/Revert.
    if (e.target.closest && e.target.closest('.mr-test')) return;
    if (e.target.closest && e.target.closest('.st-row')) markSettingsDirty(settingsKeyFor(e.target));
  };
  box.addEventListener('input', e => {
    validateSettingsScalar(e.target);
    if (e.target.closest && e.target.closest('.mr-wrap')) {
      const wrap = e.target.closest('.mr-wrap');
      // Live per-keystroke validation (regex101/NN(g)-style: inline, at
      // the field, the moment it goes wrong) + the preview bench refresh.
      // The test spelling only drives the bench: skip the draft-wide
      // revalidation, its regex compiles, and the dirty collection.
      if (!(e.target.classList && e.target.classList.contains('mr-test'))) {
        validateModelRulesDraft(wrap);
      }
      mrPreview(wrap);
    }
    if (e.target.closest && e.target.closest('.ro-wrap')) {
      // the request-overrides editor rides the same live validation
      // discipline: every keystroke revalidates the rule cards.
      validateRequestOverridesDraft(e.target.closest('.ro-wrap'));
    }
    if (e.target.closest && e.target.closest('.sc-wrap')) {
      // the sub-conversations editor rides the same live validation
      // discipline: every keystroke revalidates the entry cards.
      validateSubConversationsDraft(e.target.closest('.sc-wrap'));
    }
    dirty(e);
  });
  box.addEventListener('change', e => {
    validateSettingsScalar(e.target);
    // The delete-matching block owns its count preview: any select change
    // re-counts through the shared preview owner. The early return also
    // keeps the destructive block out of the per-row dirty marking (the
    // [data-purge] exclusion in dirty() is the second half of that).
    if (e.target.closest && e.target.closest('[data-purge="matching"]')) { updateDelCount(); return; }
    if (e.target.closest && e.target.closest('[data-backup="restore"]')) {
      if (e.target.id === 'backup-file') return;
      if (backupBusy) return;
      if (e.target.id === 'backup-include-config') backupIncludeConfig = e.target.checked;
      if (e.target.id === 'backup-include-database') backupIncludeDatabase = e.target.checked;
      if (e.target.name === 'backup-mode') backupMode = e.target.value;
      paintBackupPane();
      return;
    }
    if (e.target.closest && e.target.closest('[data-backup="download"]')) return;
    if (e.target.matches('.sp-ufield, .sp-mfield')) syncMappingPicker(e.target.closest('.prov-sec'), e.target.matches('.sp-ufield') ? 'usage' : 'model');
    if (e.target.classList && (e.target.classList.contains('sp-upath') || e.target.classList.contains('sp-cost-in') || e.target.classList.contains('sp-label') || e.target.classList.contains('sp-mkey') || e.target.classList.contains('sp-mpath') || e.target.classList.contains('sp-hname') || e.target.classList.contains('sp-hval'))) {
      const trim = e.target.closest('.ro-wrap') ? trimServerSpace : value => String(value == null ? '' : value).trim();
      const v = trim(e.target.value);
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
      mrDraftChanged(e.target.closest('.mr-wrap'));
    }
    if (e.target.classList && e.target.classList.contains('mr-tpl')) mrApplyTemplate(e.target);
    dirty(e);
  });
  box.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
      const menu = e.target.closest && e.target.closest('.ro-tpl-menu');
      if (menu) {
        e.preventDefault();
        e.stopPropagation();
        const trigger = menu.parentElement && menu.parentElement.querySelector('[data-ro-add]');
        closeRoTemplateMenus(menu.closest('.ro-wrap'));
        if (trigger) trigger.focus();
        return;
      }
    }
    if (e.key !== 'Enter') return;
    if (e.target.classList.contains('sp-cost-in')) { e.preventDefault(); addCostKey(e.target.closest('.prov-sec')); }
    else if (e.target.classList.contains('prov-new-label')) { e.preventDefault(); addProviderCard(e.target); }
    else if (e.target.classList.contains('al-new-from') || e.target.classList.contains('al-new-to')) { e.preventDefault(); addAliasRow(e.target.closest('.prov-add')); }
    else if (e.target.classList.contains('mr-new-from') || e.target.classList.contains('mr-new-to')) { e.preventDefault(); addModelRuleRow(e.target.closest('.mr-add')); }
    else if (e.target.classList.contains('sp-hname') || e.target.classList.contains('sp-hval')) {
      e.preventDefault();
      const roW = e.target.closest('.ro-wrap');
      addHeaderRow(e.target.closest('.prov-sec'));
      if (roW) roDraftChanged(roW);
    }
    else if (e.target.classList.contains('ro-rh-in')) { e.preventDefault(); addRoRemoveHeader(e.target.closest('.ro-rule')); }
    else if (e.target.classList.contains('sc-param-in')) { e.preventDefault(); addScParamRow(e.target.closest('.sc-card')); }
  });
  // Scope autocomplete refreshes at focus time: the known sets arrive on
  // their own cadence, so the datalists re-derive when the field is entered.
  box.addEventListener('focusin', e => {
    if (e.target.classList && (e.target.classList.contains('ro-client') || e.target.classList.contains('ro-provider') || e.target.classList.contains('ro-model'))) {
      roSyncDatalists(e.target.closest('.ro-wrap'));
    }
    if (e.target.classList && e.target.classList.contains('sc-client')) {
      scSyncDatalists(e.target.closest('.sc-wrap'));
    }
  });
  box.addEventListener('focusout', e => {
    const menu = e.target.closest && e.target.closest('.ro-tpl-menu');
    if (!menu || (e.relatedTarget && menu.contains(e.relatedTarget))) return;
    // The close is deferred so focus passing through a neighbor (the
    // classic focusout/relatedTarget blur race) cannot close the menu
    // before the focus lands. The pending close is owned BY THE MENU: any
    // later open or close clears it, so a stale timer from an earlier blur
    // can never tear down a freshly reopened menu.
    menu._roCloseTimer = setTimeout(() => {
      menu._roCloseTimer = null;
      if (menu.hidden || (document.activeElement && menu.contains(document.activeElement))) return;
      roSetTemplateMenuOpen(menu, false);
    }, 0);
  });
  box.addEventListener('click', e => {
    if (e.target && e.target.id === 'btn-backup-download') { runBackupDownload(); return; }
    if (e.target && e.target.id === 'btn-backup-restore') { const f = $('backup-file'); if (f) f.click(); return; }
    if (e.target && e.target.id === 'btn-backup-apply') { runBackupApply(); return; }
    if (e.target && e.target.id === 'btn-backup-cancel') { runBackupCancel(); return; }
    if (e.target && e.target.id === 'btn-purge-all') { purgeAll(); return; }
    if (e.target && e.target.id === 'btn-del-matching') { deleteMatching(); return; }
    providersEditorClick(e);
  });
  // Clicks outside the add-menu close it (the menu lives in the sheet, but
  // the veil/other panels are outside #settings-fields).
  document.addEventListener('click', e => {
    if (e.target.closest && (e.target.closest('.prov-menu') || e.target.closest('[data-prov-menu-toggle]') || e.target.closest('.ro-tpl-menu') || e.target.closest('[data-ro-add]'))) return;
    closeProvMenus();
    closeRoTemplateMenus();
  });
}
wireSettingsDelegation();

// collectSettingsFieldValue reads one keyed control or composite editor into
// the Go-facing value for its config key. The full collector and the per-key
// dirty collector share this function, so an incremental read cannot drift
// from the saved shape.
function collectSettingsFieldValue(el, validate = false) {
  const k = el.dataset.key;
  const put = (map, key, value, input) => {
    if (validate && (!key || Object.hasOwn(map, key))) {
      throw Object.assign(new Error(key ? 'duplicate mapping: ' + key : 'mapping name is required'), { input });
    }
    map[key] = value;
  };
  // Usage and model mapping rows collect through one loop: the card's map
  // section selector, the field/path selectors the delegated handlers match,
  // and the target map differ per kind. The headers collector stays separate
  // (its two inputs trim differently and key on the name field).
  const collectMappingRows = (card, mapSel, fieldSel, pathSel, into) => {
    card.querySelectorAll(mapSel).forEach(row => {
      const f = (row.querySelector(fieldSel) || {}).value;
      const p = String((row.querySelector(pathSel) || {}).value || '').trim();
      put(into, f, p, row.querySelector(fieldSel));
    });
  };
  if (el.dataset.kind === 'providers') {
    const m = Object.create(null);
    el.querySelectorAll('.st-prov').forEach(card => {
      const label = String((card.querySelector('.sp-label') || {}).value || '').trim();
      const cost = [...new Set([...card.querySelectorAll('.prov-chip')].map(c => String(c.dataset.cost || '').trim()).filter(Boolean))];
      const usage = Object.create(null);
      collectMappingRows(card, '.prov-umap .prov-urow', '.sp-ufield', '.sp-upath', usage);
      const modelsKeys = Object.create(null);
      collectMappingRows(card, '.prov-mmap .prov-urow', '.sp-mfield', '.sp-mkey', modelsKeys);
      const headers = Object.create(null);
      card.querySelectorAll('.prov-hmap .prov-hrow').forEach(row => {
        const n = String((row.querySelector('.sp-hname') || {}).value || '').trim();
        const v = String((row.querySelector('.sp-hval') || {}).value || '');
        put(headers, n, v, row.querySelector('.sp-hname'));
      });
      // Ensured tools are one comma-separated input; empty segments drop
      // and the rest ride the shared server validator (empty and
      // duplicate names reject at save, like every other editor field).
      const ensureTools = String((card.querySelector('.sp-etools') || {}).value || '')
        .split(',').map(s => s.trim()).filter(Boolean);
      put(m, label, {
        cost_keys: cost,
        usage_keys: usage,
        models_path: String((card.querySelector('.sp-mpath') || {}).value || '').trim(),
        models_keys: modelsKeys,
        ensure_tools: ensureTools,
        headers: headers,
      }, card.querySelector('.sp-label'));
    });
    return m;
  }
  if (el.dataset.kind === 'model_rules') return collectModelRules(el);
  if (el.dataset.kind === 'request_overrides') return collectRequestOverrides(el, put);
  if (el.dataset.kind === 'sub_conversations') return collectSubConversations(el);
  if (el.dataset.kind === 'aliases') {
    const m = Object.create(null);
    el.querySelectorAll('.al-row').forEach(row => {
      const from = String((row.querySelector('.al-from') || {}).value || '').trim();
      const to = String((row.querySelector('.al-to') || {}).value || '').trim();
      // Keep incomplete/self mappings in the draft; the shared server
      // validator rejects them rather than silently deleting a saved row.
      put(m, from, to, row.querySelector('.al-from'));
    });
    return m;
  }
  if (el.type === 'checkbox') return !!el.checked;
  if (el.tagName === 'TEXTAREA') return el.value.split('\n').map(s => s.trim()).filter(Boolean);
  if (el.type === 'number') {
    const n = Number(el.value);
    if (validate && (!el.checkValidity() || !Number.isSafeInteger(n))) {
      throw Object.assign(new Error(k + ': enter a valid integer'), { input: el });
    }
    return el.value.trim() && Number.isFinite(n) ? n : el.value;
  }
  return el.value;
}

function collectSettingsValues(validate = false) {
  const out = Object.create(null);
  const box = $('settings-fields');
  if (!box) return out;
  box.querySelectorAll('[data-key]').forEach(el => {
    if (el.disabled) return;
    if (el.classList.contains('st-row')) return;
    const k = el.dataset.key;
    if (!k) return;
    out[k] = collectSettingsFieldValue(el, validate);
  });
  return out;
}

// settingsKeyElementMap indexes the rendered controls and composite editors
// by config key once per fill, so a keystroke resolves its owner without
// walking every category. Each composite editor (providers, aliases, model
// rules, request overrides, sub-conversations) owns one key on its single
// [data-key] container, so this map loses nothing the full collector reads.
function settingsKeyElementMap(box) {
  const map = new Map();
  if (!box) return map;
  box.querySelectorAll('[data-key]').forEach(el => {
    if (el.classList.contains('st-row')) return;
    const key = el.dataset.key;
    if (!key) return;
    const list = map.get(key);
    if (list) list.push(el);
    else map.set(key, [el]);
  });
  return map;
}

// settingsKeyFor resolves an edited control to its owning config key: the
// control's own data-key or the nearest keyed container. Empty means the
// dirty path must fall back to the full collector.
function settingsKeyFor(node) {
  const keyed = node && node.closest && node.closest('[data-key]');
  return keyed && keyed.dataset && keyed.dataset.key ? keyed.dataset.key : '';
}

// settingsValueSnap serializes one key exactly as the full snapshot would;
// undefined (a key the collector omits) stays distinct from a string value.
function settingsValueSnap(value) {
  const text = JSON.stringify(value);
  return text === undefined ? 'undefined' : text;
}

// settingsCacheSnapshots publishes one baseline for both dirty readers.
function settingsCacheSnapshots(values) {
  settingsSnap = JSON.stringify(values);
  settingsSnapByKey = Object.create(null);
  Object.keys(values).forEach(key => {
    settingsSnapByKey[key] = settingsValueSnap(values[key]);
  });
  settingsChangedKeys = new Set();
}

// collectSettingsValue is the per-key collector: the same value the full
// collector produces for that key, without reading any other field.
function collectSettingsValue(key) {
  const els = settingsKeyEls.get(key);
  let value;
  if (!els) return value;
  els.forEach(el => {
    if (!el.disabled) value = collectSettingsFieldValue(el, false);
  });
  return value;
}

// settingsResyncChangedKeys is the no-resolvable-key fallback: one full
// collection rebuilds the changed set and the hot path resumes per key.
function settingsResyncChangedKeys() {
  const values = collectSettingsValues();
  const baseline = settingsSnapByKey || Object.create(null);
  const changed = new Set();
  const keys = new Set([...Object.keys(baseline), ...Object.keys(values)]);
  keys.forEach(key => {
    const before = Object.hasOwn(baseline, key) ? baseline[key] : 'undefined';
    const now = Object.hasOwn(values, key) ? settingsValueSnap(values[key]) : 'undefined';
    if (before !== now) changed.add(key);
  });
  settingsChangedKeys = changed;
  return changed.size;
}

function settingsFingerprint() {
  return JSON.stringify(collectSettingsValues());
}

function markSettingsDirty(key) {
  if (settingsDoc && settingsDoc.writable) {
    let n = 0;
    if (settingsSnap) {
      if (key && settingsSnapByKey && settingsKeyEls.has(key)) {
        // Hot path: one resolved key, one collection, one snapshot compare.
        // Structural add/remove/renumber actions pass their editor's key
        // explicitly; scalar edits resolve the control's own data-key.
        const before = Object.hasOwn(settingsSnapByKey, key) ? settingsSnapByKey[key] : 'undefined';
        const now = settingsValueSnap(collectSettingsValue(key));
        if (now === before) settingsChangedKeys.delete(key);
        else settingsChangedKeys.add(key);
        n = settingsChangedKeys.size;
      } else {
        // No resolvable key: correctness first, with one full collection.
        n = settingsResyncChangedKeys();
      }
    }
    if (n) settingsStatus(n === 1 ? '1 unsaved' : n + ' unsaved');
    else settingsStatus(settingsRestartNotice(settingsDoc));
    updateSettingsActions(n > 0);
    return;
  }
  updateSettingsActions();
}

function updateSettingsActions(dirtyOverride) {
  const dirty = dirtyOverride !== undefined ? !!dirtyOverride : settingsIsDirty();
  const apply = $('btn-settings-apply');
  const revert = $('btn-settings-revert');
  const busy = !!(apply?.dataset.busy || revert?.dataset.busy);
  if (apply) apply.disabled = !(dirty && settingsDoc && settingsDoc.writable) || busy;
  if (revert) revert.disabled = !dirty || busy;
}

// settingsCacheHaystacks owns the search haystack of every rendered row: one
// lowercase label + key + help copy, stamped after a form rebuild or a
// backup-pane repaint. showSettingsCat's keystroke pass reads only this cached
// text, so neither the row filter nor the rail badge counts re-read or
// re-lowercase the dataset for each rail item.
function settingsCacheHaystacks(box) {
  if (!box) return;
  box.querySelectorAll('.st-row').forEach(row => {
    row.dataset.search = ((row.dataset.label || '') + ' ' + (row.dataset.key || '') + ' ' +
      (row.dataset.help || '')).toLowerCase();
  });
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
  // One row pass serves both visibility and every rail badge count. The
  // cached haystack replaces the per-badge array scans.
  const perCat = new Map();
  box.querySelectorAll('.st-row').forEach(row => {
    const catId = row.dataset.cat;
    const hit = !q ? catId === (cat && cat.id) : (row.dataset.search || '').includes(q);
    row.hidden = !hit;
    if (hit) {
      shown++;
      if (row.dataset.hot === '0') restarts++;
    }
    // Without a query a badge is the category total, visible or not; with a
    // query it counts only that category's matching rows.
    if (!q || hit) perCat.set(catId, (perCat.get(catId) || 0) + 1);
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
      const n = perCat.get(btn.dataset.stCat) || 0;
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

// revealSettingsOffender is the one reveal every blocked save path
// shares: the three editor save gates and the collectSettingsValues
// catch in applySettings all route their offender through it. An
// offender whose category the sheet is not showing sits inside a hidden
// .st-row, where focus() and scrollIntoView() are silent no-ops - the
// operator got only the status line, nothing moved or highlighted. When
// the offender's row is hidden, activate its category by clicking the
// rail item the sheet's own delegated handler selects through (it owns
// settingsCat, the cleared search and showSettingsCat; there is no second
// selection path), then run the caller's focus + flash + scroll sequence
// unchanged. A visible offender changes nothing.
function revealSettingsOffender(firstBad, focusEl) {
  const card = firstBad.closest('.ro-rule');
  if (card && card.dataset.roExpanded !== '1') roSetCardExpanded(card, true);
  const row = firstBad.closest('.st-row');
  if (row && row.hidden && row.dataset.cat) {
    const rail = $('settings-rail');
    const btn = rail && [...rail.querySelectorAll('.st-rail-item[data-st-cat]')].find(b => b.dataset.stCat === row.dataset.cat);
    if (btn) btn.click();
  }
  if (focusEl) { focusEl.focus(); flashBadInput(focusEl); }
  if (firstBad.scrollIntoView) firstBad.scrollIntoView({ block: 'center' });
}

function applySettings() {
  const btn = $('btn-settings-apply');
  if (!btn || btn.disabled || btn.dataset.busy) return;
  let values;
  try { values = collectSettingsValues(true); }
  catch (err) {
    settingsStatus(String(err.message || err));
    if (err.input) {
      const wrap = err.input.closest && err.input.closest('.ro-wrap');
      const gate = wrap ? validateRequestOverridesDraft(wrap) : null;
      const firstBad = gate && gate.firstBad;
      const focusEl = firstBad ? (firstBad.querySelector('.prov-bad') || firstBad.querySelector('.ro-client')) : err.input;
      revealSettingsOffender(firstBad || err.input, focusEl);
    }
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
      revealSettingsOffender(gate.firstBad, gate.firstBad.querySelector('.mr-from'));
      return;
    }
  }
  // Request-overrides save gate: the same discipline - the offending rule
  // card is already marked red inline; focus its first bad input.
  const roW = $('settings-fields') && $('settings-fields').querySelector('[data-kind="request_overrides"]');
  if (roW) {
    const gate = validateRequestOverridesDraft(roW);
    if (!gate.ok) {
      delete btn.dataset.busy;
      updateSettingsActions();
      settingsStatus('request overrides - fix the highlighted rule first');
      revealSettingsOffender(gate.firstBad, gate.firstBad.querySelector('.prov-bad') || gate.firstBad.querySelector('.ro-client'));
      return;
    }
  }
  // Sub-conversations save gate: the same discipline - the offending
  // entry card is already marked red inline; focus its first bad input.
  const scW = $('settings-fields') && $('settings-fields').querySelector('[data-kind="sub_conversations"]');
  if (scW) {
    const gate = validateSubConversationsDraft(scW);
    if (!gate.ok) {
      delete btn.dataset.busy;
      updateSettingsActions();
      settingsStatus('sub-conversations - fix the highlighted entry first');
      revealSettingsOffender(gate.firstBad, gate.firstBad.querySelector('.prov-bad') || gate.firstBad.querySelector('.sc-client'));
      return;
    }
  }
  ++settingsReq;
  return operatorFetch('/admin/config', {
    method: 'POST',
    headers: JSON_HEADERS,
    body: JSON.stringify({ values, revision: settingsDoc.revision }),
  }).then(async r => {
    const j = await r.json();
    ++settingsReq; // GETs issued during this save may still describe pre-save state
    if (r.ok || j.saved) {
      const unchanged = settingsFingerprint() === submitted;
      settingsDoc = { ...settingsDoc, values: j.values, effective: j.effective, revision: j.revision, restart_required: j.restart_required };
      // The POST result carries the fresh last_reload report; adopt it so
      // the live snapshot names the apply the operator just made.
      if (j.last_reload) settingsDoc.last_reload = j.last_reload;
      applyDashValues(j.effective || j.values);
      if (unchanged) fillSettingsForm(settingsDoc);
      else {
        // Typing during the save remains unsaved. Rebase the per-key
        // snapshots to the accepted POST values and resync once, so the
        // unsaved count names exactly the keys that changed after the
        // submission rather than every key changed since the last fill.
        settingsCacheSnapshots(values);
        settingsKeyEls = settingsKeyElementMap($('settings-fields'));
        settingsResyncChangedKeys();
      }
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

// ---------- storage purge controls ----------
// These are the settings sheet's own destructive controls (the shared filter
// preview primitives stay in chrome.js with the Logs menu). They announce
// through the sheet's status line and share one in-flight gate.
function updateDelCount() {
  return updateFilterCount('del', 'del-count', 'btn-del-matching', n => 'will delete ' + recPhrase(n), () => _purgeBusy);
}

async function deleteMatching() {
  if (_purgeBusy) return;
  const preview = previewedFilter('del');
  if (!preview) return updateDelCount();
  if (!confirm(`Delete ${recPhrase(preview.count)} matching the selected filter?\n\nThis cannot be undone.`)) return;
  settingsStatus('deleting');
  if (await purgeMetrics(preview.filter)) settingsStatus(recPhrase(preview.count) + ' deleted', 'ok');
}

async function purgeAll() {
  if (_purgeBusy) return;
  if (!confirm('Permanently delete all metrics history?\n\nThe entire database will be wiped. This cannot be undone.')) return;
  settingsStatus('deleting');
  if (await purgeMetrics(null)) settingsStatus('all metrics history deleted', 'ok');
}

// Confirmed success is the only path that clears UI state. The same gate
// handles full/filtered deletion from either action, disables repeat
// submits, and reports failures to the settings status line - the one
// surface both actions live on.
async function purgeMetrics(filter) {
  const fail = 'could not delete records';
  if (_purgeBusy) return;
  _purgeBusy = true;
  const delBtn = $('btn-del-matching');
  if (delBtn) delBtn.disabled = true;
  const purgeAllBtn = $('btn-purge-all');
  if (purgeAllBtn) purgeAllBtn.disabled = true;
  document.querySelectorAll('#settings-fields [data-purge="matching"] select').forEach(el => { el.disabled = true; });
  try {
    const options = {method: 'POST'};
    if (filter) { options.headers = JSON_HEADERS; options.body = JSON.stringify(filter); }
    const result = await operatorJsonBody('/admin/purge', options, fail);
    if (result.ok !== true) throw new Error(result.error || fail);
    _filterPreviews.clear();
    $('btn-logs-filtered').disabled = true;
    // The scoped block's preview text describes records that just ceased
    // to exist; the next sheet fill rebuilds it empty either way.
    const delPreview = $('del-count');
    if (delPreview) delPreview.textContent = '';
    closeDrawer(); lastRender = {};
    lastSeq = 0;
    // No removal event exists: the forced full bootstrap is the post-purge
    // render, superseding any stale in-flight tick. No optimistic local wipe.
    // Await both the record snapshot and the forced aggregate resync so the
    // reported completion describes the read side too; the invalidation has
    // already painted loading and dropped the old aggregates.
    const aggregates = refreshAggregates();
    const bootstrap = fetchBootstrap('full');
    await Promise.all([bootstrap, aggregates]);
    return true;
  } catch (err) {
    settingsStatus(String(err.message || fail));
    return false;
  } finally {
    _purgeBusy = false;
    // Re-query: fillSettingsForm may have rebuilt the rows mid-flight, and
    // a closed or unfilled sheet has none of them - only the instances
    // currently in the document must end up in the right state.
    const freshPurgeBtn = $('btn-purge-all');
    if (freshPurgeBtn) freshPurgeBtn.disabled = false;
    const freshDelBtn = $('btn-del-matching');
    if (freshDelBtn) freshDelBtn.disabled = !previewedFilter('del');
    document.querySelectorAll('#settings-fields [data-purge="matching"] select').forEach(el => { el.disabled = false; });
  }
}

// The chrome.js shell delegates close/open here after this module loads.
window.settingsModule = { open: openSettings, close: settingsClose };
