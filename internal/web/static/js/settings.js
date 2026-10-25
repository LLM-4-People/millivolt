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
