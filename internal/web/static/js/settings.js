// Settings sheet module. chrome.js owns only the shell: the header trigger,
// the one-shot lazy loader, the sheet's open-state probe, the shared status
// line and the close entry point. Everything below - state, rendering,
// collection, validation, search, apply/revert, backup/restore and purge
// controls, and their editors - loads with this deferred classic script on
// the first Settings open. Keep the moved globals global: the dashboard is
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
let backupMode = 'replace';
let backupIncludeConfig = true;
let backupIncludeDatabase = true;
let backupInspect = null; // {file, data} after a validated inspect, before apply
let backupBusy = false;
let backupReq = 0; // newest backup fetch owns state; cancel invalidates older ones
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

function backupCheckOn(id, enabled) {
  if (!enabled) return false;
  if (id === 'backup-include-config') return backupIncludeConfig;
  if (id === 'backup-include-database') return backupIncludeDatabase;
  const el = $(id);
  return el ? el.checked : true;
}

function settingsBackupHTML(doc) {
  const b = doc.backup || {};
  const restoring = !!(backupInspect && backupInspect.data);
  let html = '';
  if (!restoring) {
    html += backupDownloadRowHTML(b);
    html += backupRestoreRowHTML(b);
    return html;
  }
  html += backupArchiveRowHTML();
  html += backupDiffRowsHTML(doc);
  html += backupRestoreRowHTML(b);
  return html;
}

// The storage category's scoped delete, moved out of the header Clear
// menu (the header no longer carries any deletion control). It renders
// through the fillSettingsForm pipeline like the backup action rows,
// joins the category filter, the search haystack and the rail badge count
// through its data-cat/label/help attributes, and - like the backup rows -
// carries no data-key, so it never dirties the form or joins value
// collection. Clicks arrive through the settings sheet's delegated
// handler. The selects start EMPTY on every rebuild on purpose: the
// refill resets any previous selection - a stale invisible filter must
// never arm a destructive default - and fillSettingsForm's
// populateFilterMenu('del') restores the current option values.
function settingsDeleteRowHTML() {
  return `<div class="st-row st-block" data-cat="storage" data-purge="matching" data-label="delete matching purge filtered records storage history" data-help="delete only the records matching the chosen provider, model, client, status, errors, debug and age filters; a live count previews the deletion; cannot be undone">
    <div class="st-name">Delete matching</div>
    <div class="st-ctl">
      <div class="st-del-opts">
        <label>provider <select id="del-provider"></select></label>
        <label>model <select id="del-model"></select></label>
        <label>client <select id="del-client"></select></label>
        <label>status <select id="del-status"></select></label>
        <label>errors <select id="del-errors"></select></label>
        <label>debug <select id="del-debug"></select></label>
        <label>older than <select id="del-age"></select></label>
      </div>
      <span class="st-del-count" id="del-count"></span>
      <div class="st-backup-actions"><button type="button" class="btn btn-danger" id="btn-del-matching" disabled>Delete matching</button></div>
      <span class="st-hint">delete only the records matching the chosen provider, model, client, status, errors, debug and age filters; a live count previews the deletion; cannot be undone</span>
    </div>
  </div>`;
}

// The storage category's whole-database delete, the scoped block's
// companion action. It renders like the backup action rows, joins the
// category filter, the search haystack and the rail badge count through
// its data-cat/label/help attributes, and - like the backup rows -
// carries no data-key, so it never dirties the form or joins value
// collection. Clicks arrive through the settings sheet's delegated
// handler.
function settingsPurgeRowHTML() {
  return `<div class="st-row st-block" data-cat="storage" data-purge="all" data-label="delete everything purge database storage history" data-help="permanently delete all metrics history: the live ring and the entire durable database; cannot be undone">
    <div class="st-name">Delete everything</div>
    <div class="st-ctl">
      <div class="st-backup-actions"><button type="button" class="btn btn-danger-solid" id="btn-purge-all">Delete everything</button></div>
      <span class="st-hint">permanently delete all metrics history: the live ring and the entire durable database; cannot be undone</span>
    </div>
  </div>`;
}

function backupMemberCheck(id, label, enabled) {
  const on = backupCheckOn(id, enabled);
  return `<label class="st-check"><input type="checkbox" id="${id}"${enabled ? '' : ' disabled'}${on ? ' checked' : ''}> ${label}</label>`;
}

function backupMemberChecks(cfgId, dbId, canCfg, canDb) {
  return backupMemberCheck(cfgId, 'config', canCfg) + backupMemberCheck(dbId, 'database', canDb);
}

function backupDownloadRowHTML(b) {
  const canCfg = !!b.config;
  const canDb = !!b.database;
  const modified = Array.isArray(b.modified) ? b.modified : [];
  let hints = '';
  if (canCfg) {
    hints += `<span class="st-hint">${modified.length ? modified.length + (modified.length === 1 ? ' setting differs from default' : ' settings differ from default') : 'all settings are the built-in default'}</span>`;
  }
  if (canDb) {
    const n = Number(b.requests) || 0;
    hints += `<span class="st-hint">${n} ${n === 1 ? 'request' : 'requests'}</span>`;
  }
  return `<div class="st-row st-block st-backup" data-cat="backup" data-backup="download" data-label="download backup config database archive" data-help="download a self-checked archive of the saved config and sqlite history">
    <div class="st-name">Download</div>
    <div class="st-ctl">
      ${backupMemberChecks('backup-dl-config', 'backup-dl-database', canCfg, canDb)}
      ${hints}
      <div class="st-backup-actions"><button type="button" class="btn" id="btn-backup-download">Download</button></div>
    </div>
  </div>`;
}

function backupRestoreRowHTML(b) {
  const restoring = !!(backupInspect && backupInspect.data);
  const pending = b.pending_database
    ? '<span class="st-hint">database restore is staged; restart to apply</span>'
    : '';
  const file = `<input type="file" id="backup-file" accept=".mvb,application/octet-stream" hidden>`;
  let ctl;
  if (restoring) {
    ctl = pending + backupRestoreOptionsHTML(b) +
      `<div class="st-backup-actions">` +
      `<button type="button" class="btn" id="btn-backup-cancel">Cancel</button>` +
      `<button type="button" class="btn btn-accent" id="btn-backup-apply">Apply</button></div>` + file;
  } else {
    ctl = pending +
      `<div class="st-backup-actions"><button type="button" class="btn" id="btn-backup-restore">Restore</button></div>` +
      file +
      `<span class="st-hint">validates the archive before it is applied.</span>`;
  }
  return `<div class="st-row st-block st-backup" data-cat="backup" data-backup="restore" data-label="restore backup config database archive merge replace" data-help="restore a self-checked archive of the saved config and sqlite history">
    <div class="st-name">Restore</div>
    <div class="st-ctl">${ctl}</div>
  </div>`;
}

function backupBytes(n) {
  n = Number(n);
  if (!Number.isFinite(n) || n < 0) return '';
  if (n < 1024) return Math.round(n) + ' B';
  if (n < 1024 * 1024) return Math.round(n / 1024) + ' KiB';
  const mib = n / (1024 * 1024);
  return (mib >= 10 ? Math.round(mib) : Math.round(mib * 10) / 10) + ' MiB';
}

function backupWhen(v) {
  if (v == null || v === '' || v === 0) return '';
  const d = new Date(typeof v === 'number' ? v : String(v));
  if (Number.isNaN(d.getTime())) return '';
  const p = n => String(n).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
}

function backupSpan(lo, hi) {
  const a = backupWhen(lo), b = backupWhen(hi);
  if (!a) return b;
  if (!b || a === b) return a;
  return a + ' - ' + b;
}

function backupArchiveRowHTML() {
  const j = backupInspect && backupInspect.data || {};
  const file = backupInspect && backupInspect.file || {};
  const cfg = j.config || {};
  const db = j.database || {};
  const ident = [];
  if (file.name) ident.push(String(file.name));
  const sz = backupBytes(file.size);
  if (sz) ident.push(sz);
  const created = backupWhen(j.created);
  if (created) ident.push(created);
  let hints = ident.length ? `<span class="st-hint">${escapeHtml(ident.join(' · '))}</span>` : '';
  if (cfg.present) {
    const bits = ['config'];
    const cfgSize = backupBytes(cfg.bytes);
    if (cfgSize) bits.push(cfgSize);
    hints += `<span class="st-hint">${escapeHtml(bits.join(' · '))}</span>`;
  }
  if (db.present) {
    const bits = ['database'];
    const dbSize = backupBytes(db.bytes);
    if (dbSize) bits.push(dbSize);
    const reqs = Number(db.requests) || 0;
    bits.push(reqs + (reqs === 1 ? ' request' : ' requests'));
    if (db.debug) bits.push(db.debug + ' debug');
    const span = backupSpan(db.oldest_ms, db.newest_ms);
    if (span) bits.push(span);
    hints += `<span class="st-hint">${escapeHtml(bits.join(' · '))}</span>`;
    const overlap = db.overlap == null ? null : Number(db.overlap);
    if (overlap != null) {
      hints += `<span class="st-hint">${overlap} ${overlap === 1 ? 'request' : 'requests'} already on this store</span>`;
    }
  }
  const label = ['archive'].concat(ident, cfg.present ? ['config'] : [], db.present ? ['database', 'requests'] : []).join(' ');
  return `<div class="st-row st-block st-backup" data-cat="backup" data-backup="archive" data-label="${escapeHtml(label)}" data-help="contents of the selected archive">
    <div class="st-name">Archive</div>
    <div class="st-ctl">${hints}</div>
  </div>`;
}

function backupRestoreOptionsHTML(b) {
  const j = backupInspect && backupInspect.data || {};
  const cfg = j.config || {};
  const db = j.database || {};
  const canCfg = !!b.config && !!cfg.present;
  const canDb = !!b.database && !!db.present;
  const wantCfg = backupCheckOn('backup-include-config', canCfg);
  const wantDb = backupCheckOn('backup-include-database', canDb);
  const merge = backupMode === 'merge';
  let html = '';
  if (cfg.present) {
    html += backupMemberCheck('backup-include-config', 'config', canCfg);
    if (!canCfg) html += '<span class="st-hint">start with -config to restore settings</span>';
  }
  if (db.present) {
    html += backupMemberCheck('backup-include-database', 'database', canDb);
    if (!canDb) html += '<span class="st-hint">durable storage is disabled</span>';
  }
  if (wantCfg || wantDb) {
    html += `<label class="st-check"><input type="radio" name="backup-mode" value="replace"${merge ? '' : ' checked'}> replace</label>` +
      `<label class="st-check"><input type="radio" name="backup-mode" value="merge"${merge ? ' checked' : ''}> merge</label>`;
    const hints = [];
    if (wantCfg) hints.push(merge ? 'overlays keys that differ from default' : 'writes every key from the backup');
    if (wantDb) hints.push(merge ? 'inserts request ids that are not already stored' : 'replaces the live database on restart');
    html += `<span class="st-hint">${escapeHtml(hints.join('. '))}.</span>`;
  }
  return html;
}

function backupWantConfig() {
  const b = (settingsDoc && settingsDoc.backup) || {};
  const cfg = (backupInspect && backupInspect.data && backupInspect.data.config) || {};
  return backupCheckOn('backup-include-config', !!(b.config && cfg.present));
}

function backupWantDatabase() {
  const b = (settingsDoc && settingsDoc.backup) || {};
  const db = (backupInspect && backupInspect.data && backupInspect.data.database) || {};
  return backupCheckOn('backup-include-database', !!(b.database && db.present));
}

function backupQuery(forRestore = false) {
  const q = new URLSearchParams();
  if (forRestore) {
    if (backupWantConfig()) q.set('config', '1');
    if (backupWantDatabase()) q.set('database', '1');
    const mode = backupMode === 'merge' ? 'merge' : 'replace';
    if (backupWantConfig()) q.set('config_mode', mode);
    if (backupWantDatabase()) q.set('database_mode', mode);
    return q;
  }
  if (backupCheckOn('backup-dl-config', true)) q.set('config', '1');
  if (backupCheckOn('backup-dl-database', true)) q.set('database', '1');
  return q;
}

function backupScalar(v) {
  if (v == null) return '-';
  if (typeof v === 'boolean' || typeof v === 'number') return String(v);
  if (typeof v === 'string') return v;
  if (Array.isArray(v)) return v.length ? v.length + ' items' : '[]';
  if (typeof v === 'object') {
    const n = Object.keys(v).length;
    return n ? n + ' entries' : '{}';
  }
  return String(v);
}

function backupDiffRowsHTML(doc) {
  if (!backupInspect || !backupInspect.data) return '';
  const cfg = backupInspect.data.config || {};
  if (!cfg.present) return '';
  const modified = Array.isArray(cfg.modified) ? cfg.modified : [];
  if (!modified.length) {
    return `<div class="st-row st-backup" data-cat="backup" data-backup="diff" data-label="restore config default" id="backup-preview"><div class="st-name">Config</div><div class="st-ctl"><span class="st-hint">every key in this backup is still the built-in default</span></div></div>`;
  }
  const fields = (doc && doc.fields) || (settingsDoc && settingsDoc.fields) || [];
  const byKey = {};
  for (const f of fields) byKey[f.key] = f;
  const defaults = (doc && doc.defaults) || (settingsDoc && settingsDoc.defaults) || {};
  const live = (doc && doc.values) || (settingsDoc && settingsDoc.values) || {};
  const values = cfg.values || {};
  const vsLive = new Set(Array.isArray(cfg.vs_live) ? cfg.vs_live : []);
  return modified.map((key, i) => {
    const label = (byKey[key] && byKey[key].label) || key;
    const val = backupScalar(values[key]);
    let hint = 'default ' + backupScalar(defaults[key]);
    hint += vsLive.has(key) ? ' · live ' + backupScalar(live[key]) : ' · same as live';
    const id = i === 0 ? ' id="backup-preview"' : '';
    return `<div class="st-row" data-cat="backup" data-backup="diff" data-key="${escapeHtml(key)}" data-label="${escapeHtml(label)} restore"${id}>
      <div class="st-name">${escapeHtml(label)}</div>
      <div class="st-ctl"><div class="st-ctl-line">${escapeHtml(val)}</div><span class="st-hint">${escapeHtml(hint)}</span></div>
    </div>`;
  }).join('');
}

function backupSetBusy(on) {
  backupBusy = !!on;
  for (const id of ['btn-backup-download', 'btn-backup-restore', 'btn-backup-apply', 'btn-backup-cancel']) {
    const b = $(id);
    if (!b) continue;
    if (backupBusy) b.dataset.busy = '1'; else delete b.dataset.busy;
    if (id === 'btn-backup-apply' && !backupBusy) b.disabled = !backupWantConfig() && !backupWantDatabase();
    else b.disabled = backupBusy;
  }
  if (backupBusy) {
    document.querySelectorAll('#backup-include-config, #backup-include-database, input[name="backup-mode"]').forEach(el => {
      el.disabled = true;
    });
  }
}

// backupPaneReset is the one restore-baseline epilogue, shared by the
// validated-inspect path and cancel: both include flags back on, end the
// busy state, and repaint the pane.
function backupPaneReset() {
  backupIncludeConfig = true;
  backupIncludeDatabase = true;
  backupSetBusy(false);
  paintBackupPane();
}

function wireBackupPane() {
  const backupFile = $('backup-file');
  if (backupFile) backupFile.addEventListener('change', runBackupRestore);
  backupSetBusy(backupBusy);
}

function runBackupDownload() {
  const q = backupQuery();
  if (![...q.keys()].length) { settingsStatus('select config, database, or both'); return; }
  if (backupBusy) return;
  backupSetBusy(true);
  settingsStatus('building backup');
  downloadArtifact('/admin/backup?' + q, 'millivolt-backup.mvb', 'backup failed')
    .then(blob => {
      const n = backupBytes(blob.size);
      settingsStatus(n ? 'downloaded ' + n : 'backup downloaded', 'ok');
    })
    .catch(err => settingsStatus(String(err.message || err)))
    .finally(() => backupSetBusy(false));
}

function runBackupRestore(ev) {
  const input = ev && ev.target && ev.target.files ? ev.target : $('backup-file');
  const file = input && input.files && input.files[0];
  if (!file) return;
  if (backupBusy) return;
  const req = ++backupReq;
  backupSetBusy(true);
  settingsStatus('checking backup');
  operatorJsonBody('/admin/restore?inspect=1', {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: file,
  }, 'restore failed').then(j => {
    if (req !== backupReq) return;
    backupInspect = { file, data: j };
    backupPaneReset();
    settingsStatus('review then apply', 'ok');
  }).catch(err => {
    if (req !== backupReq) return;
    settingsStatus(String(err.message || err));
  }).finally(() => {
    if (input) input.value = '';
    if (req === backupReq && !backupInspect) backupSetBusy(false);
  });
}

function paintBackupPane() {
  const box = $('settings-fields');
  if (!box || !settingsDoc) return;
  box.querySelectorAll('[data-backup]').forEach(el => el.remove());
  box.insertAdjacentHTML('beforeend', settingsBackupHTML(settingsDoc));
  wireBackupPane();
  // The repaint replaces the backup rows; re-cache before the filter reads
  // them.
  settingsCacheHaystacks(box);
  showSettingsCat();
}

function runBackupApply() {
  if (!backupInspect || !backupInspect.file) return;
  const q = backupQuery(true);
  if (!q.get('config') && !q.get('database')) { settingsStatus('select config, database, or both'); return; }
  if (backupBusy) return;
  const req = ++backupReq;
  backupSetBusy(true);
  settingsStatus('restoring');
  operatorJsonBody('/admin/restore?' + q, {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: backupInspect.file,
  }, 'restore failed').then(j => {
    if (req !== backupReq) return;
    backupInspect = null;
    backupSetBusy(false);
    const restart = settingsRestartNotice(j);
    settingsStatus(restart ? 'restored · ' + restart : 'restored', restart ? '' : 'ok');
    return fetchSettings(true);
  }).catch(err => {
    if (req !== backupReq) return;
    backupSetBusy(false);
    paintBackupPane();
    settingsStatus(String(err.message || err));
  });
}

function runBackupCancel() {
  if (backupBusy && backupInspect) return;
  backupReq++;
  backupInspect = null;
  backupPaneReset();
  settingsStatus('');
}

// settingsReloadDetail renders the server's last_reload report as one
// compact line: when, whether the reload failed, how many keys were
// dropped, and the restart-affected keys (the shared settingsRestartNotice
// vocabulary).
function settingsReloadDetail(lr) {
  const bits = [new Date(lr.at).toLocaleString()];
  if (lr.ok === false) bits.push('failed');
  if (Array.isArray(lr.dropped_keys) && lr.dropped_keys.length) bits.push('dropped ' + lr.dropped_keys.length);
  const restart = settingsRestartNotice(lr);
  if (restart) bits.push(restart);
  return bits.join(' · ');
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
  // last_reload reports the latest Settings apply the server completed;
  // the server omits it before the first one. Deny by default: only a
  // safe-integer arrival time renders. The title carries the server's
  // failure text when the reload failed.
  const lr = doc.last_reload;
  const lastApply = lr && Number.isSafeInteger(lr.at)
    ? `<div class="st-live-row"><span class="k">last apply</span><span class="v" title="${escapeHtml(String(lr.error || settingsReloadDetail(lr)))}">${escapeHtml(settingsReloadDetail(lr))}</span></div>`
    : '';
  el.innerHTML =
    `<div class="st-live-row"><span class="k">listen</span><span class="v" title="${escapeHtml(String(listen))}">${escapeHtml(String(listen))}</span></div>` +
    `<div class="st-live-row"><span class="k">store</span><span class="v" title="${escapeHtml(String(db))}">${escapeHtml(String(db || 'ring'))}</span></div>` +
    `<div class="st-live-row"><span class="k">write</span><span class="v">${escapeHtml(persist)}</span></div>` +
    lastApply;
}

function settingsFieldID(key, part) {
  return `settings-${part}-${encodeURIComponent(String(key))}`;
}

function settingsScalarErrorMessage(input) {
  const validity = input.validity;
  if (validity.valueMissing) return 'enter a value';
  if (validity.rangeUnderflow) return 'enter a value of at least ' + input.min;
  if (validity.rangeOverflow) return 'enter a value no greater than ' + input.max;
  if (validity.stepMismatch) return 'enter a value in steps of ' + (input.step || '1');
  return input.validationMessage || 'enter a valid value';
}

function validateSettingsScalar(input) {
  if (!input || !input.dataset || input.dataset.stScalar !== '1') return true;
  const row = input.closest('.st-row');
  const error = row && row.querySelector('.st-error');
  const valid = !input.willValidate || input.checkValidity();
  input.classList.toggle('prov-bad', !valid);
  if (valid) input.removeAttribute('aria-invalid');
  else input.setAttribute('aria-invalid', 'true');
  if (error) error.textContent = valid ? '' : settingsScalarErrorMessage(input);
  const helpId = input.dataset.stHelpId || '';
  const errorId = input.dataset.stErrorId || '';
  const described = [helpId, valid ? '' : errorId].filter(Boolean).join(' ');
  if (described) input.setAttribute('aria-describedby', described);
  else input.removeAttribute('aria-describedby');
  return valid;
}

function settingsFieldHTML(f, val, def, override) {
  const locked = override != null && String(override) !== '';
  const structured = f.kind === 'providers' || f.kind === 'aliases' || f.kind === 'model_rules' || f.kind === 'request_overrides' || f.kind === 'sub_conversations';
  const fieldId = settingsFieldID(f.key, 'field');
  const helpId = settingsFieldID(f.key, 'help');
  const errorId = settingsFieldID(f.key, 'error');
  const dis = (locked ? ' disabled' : '') + ` aria-label="${escapeHtml(f.label)}"`;
  const scalarAttrs = ` data-st-scalar="1" data-st-error-id="${escapeHtml(errorId)}"${f.help ? ` data-st-help-id="${escapeHtml(helpId)}" aria-describedby="${escapeHtml(helpId)}"` : ''}`;
  const pills = locked ? '<span class="st-pill lock">flag</span>' : '';
  let hint = '';
  if (f.zero_means && (val === 0 || val === '0' || (f.zero_token && val === f.zero_token))) hint = `<span class="st-hint">${escapeHtml(f.zero_means)}</span>`;
  else if (def != null && def !== '' && f.kind !== 'providers' && f.kind !== 'aliases' && f.kind !== 'strings' && f.kind !== 'request_overrides' && f.kind !== 'sub_conversations' && String(val) === String(def)) {
    hint = `<span class="st-hint">default</span>`;
  } else if (def != null && def !== '' && f.kind !== 'providers' && f.kind !== 'aliases' && f.kind !== 'strings' && f.kind !== 'request_overrides' && f.kind !== 'sub_conversations') {
    hint = `<span class="st-hint">default ${escapeHtml(String(def))}</span>`;
  }
  if (locked) hint += `<span class="st-hint">via flag · ${escapeHtml(String(override))}</span>`;
  let control = '';
  const unit = f.unit ? `<span class="st-unit">${escapeHtml(f.unit)}</span>` : '';
  const nameExtra = f.kind === 'providers' ? providersAddHTML() : '';
  if (f.kind === 'bool') {
    const on = val === true || val === 'true';
    control = `<input type="checkbox" class="st-switch" id="${escapeHtml(fieldId)}" data-key="${escapeHtml(f.key)}"${on ? ' checked' : ''}${scalarAttrs}${dis}>`;
  } else if (f.kind === 'strings') {
    const text = Array.isArray(val) ? val.join('\n') : (val || '');
    control = `<textarea id="${escapeHtml(fieldId)}" data-key="${escapeHtml(f.key)}" rows="3" placeholder="one per line"${scalarAttrs}${dis}>${escapeHtml(text)}</textarea>`;
  } else if (f.kind === 'providers') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="providers">${providersEditorHTML(val)}</div>`;
  } else if (f.kind === 'aliases') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="aliases">${aliasesEditorHTML(val)}</div>`;
  } else if (f.kind === 'model_rules') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="model_rules">${modelRulesEditorHTML(val)}</div>`;
  } else if (f.kind === 'request_overrides') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="request_overrides">${requestOverridesEditorHTML(val)}</div>`;
  } else if (f.kind === 'sub_conversations') {
    control = `<div data-key="${escapeHtml(f.key)}" data-kind="sub_conversations">${subConversationsEditorHTML(val)}</div>`;
  } else if (f.kind === 'int') {
    const min = f.min != null ? ` min="${escapeHtml(String(f.min))}"` : '';
    const max = f.max != null ? ` max="${escapeHtml(String(f.max))}"` : '';
    control = `<div class="st-ctl-line"><input type="number" id="${escapeHtml(fieldId)}" data-key="${escapeHtml(f.key)}" value="${escapeHtml(String(val ?? ''))}" step="1" required${min}${max}${scalarAttrs}${dis}>${unit}</div>`;
  } else {
    control = `<div class="st-ctl-line"><input type="text" id="${escapeHtml(fieldId)}" data-key="${escapeHtml(f.key)}" value="${escapeHtml(val == null ? '' : String(val))}"${scalarAttrs}${dis}>${unit}</div>`;
  }
  const block = (structured || f.kind === 'strings') ? ' st-block' : '';
  const name = structured
    ? `<div class="st-name">${escapeHtml(f.label)}${pills}${nameExtra}</div>`
    : `<label class="st-name" for="${escapeHtml(fieldId)}">${escapeHtml(f.label)}${pills}</label>`;
  const help = f.help ? `<details class="st-help"><summary>Help</summary><div class="st-help-text" id="${escapeHtml(helpId)}">${escapeHtml(f.help)}</div></details>` : '';
  const error = structured ? '' : `<div class="st-error" id="${escapeHtml(errorId)}" aria-live="polite"></div>`;
  const ttl = escapeHtml((f.help || '') + (f.help && f.key ? ' · ' : '') + (f.key || ''));
  const hot = f.hot_reload ? '1' : '0';
  return `<div class="st-row${block}" data-cat="${escapeHtml(f.category)}" data-key="${escapeHtml(f.key)}" data-label="${escapeHtml(f.label)}" data-help="${escapeHtml(f.help || '')}" data-hot="${hot}" title="${ttl}">${name}<div class="st-ctl">${control}${error}${hint}${help}</div></div>`;
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

// The provider card's remove icon is one 16x16 glyph authored as a markup
// constant (the trash geometry lost its header Clear button when deletion
// moved into the settings sheet). The constant carries no class; .prov-x svg
// owns this surface's stroke and size. The collapse chevron lives in
// core.js because the chart metrics picker also uses it before settings.js
// is loaded.
const PROV_TRASH_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.2 5.4h9.6"/><path d="M6.1 5.4V4.3A1.2 1.2 0 0 1 7.3 3.1h1.4A1.2 1.2 0 0 1 9.9 4.3v1.1"/><path d="M4.2 5.4l.7 7.4a1 1 0 0 0 1 .8h5.2a1 1 0 0 0 1-.8l.7-7.4"/></svg>';

// mappingRowHTML renders one canonical-field → custom-path row for both
// structured maps. The two kinds differ only in wire keys: the select and
// input classes the delegated handlers and the value collector match on,
// the remove attribute, the aria-labels and the placeholders.
function mappingRowHTML(kind, field, path, fields) {
  const usage = kind === 'usage';
  const opts = settingsFieldOptions(fields, field);
  return `<div class="prov-urow"><select class="sp-${usage ? 'ufield' : 'mfield'}" aria-label="canonical ${usage ? 'token' : 'model'} field">${opts}</select><span class="prov-arrow" aria-hidden="true">→</span><input class="sp-${usage ? 'upath' : 'mkey'}" value="${escapeHtml(path == null ? '' : String(path))}" placeholder="${usage ? 'usage-relative path' : 'path inside the enrichment entry'}" aria-label="${usage ? 'usage key path' : 'model metadata path'}"><button type="button" class="prov-x" data-prov-${usage ? 'u' : 'm'}row-rm aria-label="remove mapping">✕</button></div>`;
}

function usageRowHTML(field, path, fields) { return mappingRowHTML('usage', field, path, fields); }
function modelRowHTML(field, path, fields) { return mappingRowHTML('model', field, path, fields); }

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
  const ensureTools = Array.isArray(p.ensure_tools) ? p.ensure_tools.map(n => String(n)) : [];
  const headers = p.headers && typeof p.headers === 'object' && !Array.isArray(p.headers) ? p.headers : {};
  const fields = canonicalUsageFields();
  const used = new Set(Object.keys(usage));
  const free = fields.filter(f => !used.has(f));
  const mfields = canonicalModelFields();
  const mused = new Set(Object.keys(modelsKeys));
  const mfree = mfields.filter(f => !mused.has(f));
  // Section order is fixed: cost keys, then usage keys, then models
  // enrichment, then ensured tools, then upstream headers last. Every
  // section opens and closes inside the card - one missing close here
  // cascades the following sections into each other (the models section
  // once rendered nested inside usage keys).
  return `<div class="st-prov">` +
    `<div class="prov-hd"><span class="prov-ic" style="--ent:${ENTITY_TYPES.provider.color}" aria-hidden="true">☁</span><input class="sp-label" value="${escapeHtml(label)}" placeholder="provider label - registrable domain of the base URL, e.g. nano-gpt.com" aria-label="provider label"><button type="button" class="prov-chev" data-prov-collapse aria-expanded="true" aria-label="collapse or expand ${escapeHtml(label)}" title="collapse / expand">${PROV_CHEV_SVG}</button><button type="button" class="prov-x" data-prov-rm aria-label="remove provider" title="remove provider">${PROV_TRASH_SVG}</button></div>` +
    `<div class="prov-body">` +
    `<div class="prov-sec"><div class="prov-lb"><span>cost keys</span><span class="prov-sub">dotted JSON path from the response root - e.g. usage.cost</span></div>` +
    `<div class="prov-chips">${costs.map(costChipHTML).join('')}</div>` +
    `<div class="prov-add"><input class="sp-cost-in" placeholder="add cost key path… ↵" aria-label="add cost key"><button type="button" class="btn prov-addbtn" data-prov-add-cost aria-label="add cost key">+</button></div></div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>usage keys</span><span class="prov-sub">canonical token field → custom path</span></div>` +
    `<div class="prov-umap">${Object.entries(usage).map(([f, path]) => usageRowHTML(f, path, fields)).join('')}</div>` +
    (free.length ? `<div class="prov-add prov-add-u"><select class="sp-ufield-new" aria-label="canonical token field">${free.map(f => `<option value="${escapeHtml(f)}">${escapeHtml(f)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-usage aria-label="add usage mapping">+ map</button></div>` : '') +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>models enrichment</span><span class="prov-sub">metadata endpoint merged into this provider's /v1/models list</span></div>` +
    `<div class="prov-add"><input class="sp-mpath" value="${escapeHtml(modelsPath)}" placeholder="metadata path - e.g. /language-models" aria-label="models metadata path"></div>` +
    `<div class="prov-mmap">${Object.entries(modelsKeys).map(([f, path]) => modelRowHTML(f, path, mfields)).join('')}</div>` +
    (mfree.length ? `<div class="prov-add prov-add-m"><select class="sp-mfield-new" aria-label="canonical model field">${mfree.map(f => `<option value="${escapeHtml(f)}">${escapeHtml(f)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-model aria-label="add model mapping">+ map</button></div>` : '') +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>ensured tools</span><span class="prov-sub">tool names every chat request's tools array must carry - missing names are injected as inert stubs upstream</span></div>` +
    `<div class="prov-add"><input class="sp-etools" value="${escapeHtml(ensureTools.join(', '))}" placeholder="tool names, comma-separated - e.g. bash, read" aria-label="ensured tool names"></div>` +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>upstream headers</span><span class="prov-sub">extra headers sent upstream - {{uuid4}} / {{platform}} / {{opencode-msg-id}} / {{opencode-ses-id}} expand per request</span></div>` +
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
// The template menu's labels for the shipped pipeline rules: presentation
// text, paired to settingsDoc.defaults.model_rules BY POSITION - the menu
// offers "the shipped pipeline" in its authored order, and the rule payloads
// themselves come from the server doc (mrTemplates below), never a copy
// here. A defaults list longer than the label table still offers every
// rule, labeled by position.
const MR_SHIPPED_LABELS = [
  'lowercase fold',
  'strip vendor/ prefix',
  'strip :tag suffix',
  'strip architecture/quant suffix (fp4, nvfp4, int8, q4_k_m…)',
  'unify . and - between digits',
];
// The custom starters: manual rows a shipped rule cannot express.
const MR_CUSTOM_TEMPLATES = [
  ['exact merge…', { mode: 'exact' }],
  ['custom pattern…', { mode: 'pattern' }],
];
// mrTemplates derives the editor's template menu at render time: the
// shipped entries ride in from the settings doc (defaults.model_rules - the
// same source the restore-defaults control reads), so their payloads cannot
// drift from the server. Deny by default: without a server doc only the
// custom starters are offered.
function mrTemplates() {
  const shipped = Array.isArray(settingsDoc?.defaults?.model_rules) ? settingsDoc.defaults.model_rules : [];
  return shipped.map((r, i) => [MR_SHIPPED_LABELS[i] || 'shipped rule ' + (i + 1), r])
    .concat(MR_CUSTOM_TEMPLATES);
}

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

// The park/visibility button's title pair, shared by the row template and
// the toggle handler so the two spellings of the same states cannot drift.
const MR_VIS_TITLE_PARKED = 'un-park this rule';
const MR_VIS_TITLE_ACTIVE = 'park this rule - keep it without applying it';

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
    `<span class="mr-move"><button type="button" class="mr-vis" data-mr-vis aria-pressed="${off}" title="${off ? MR_VIS_TITLE_PARKED : MR_VIS_TITLE_ACTIVE}">${off ? '◌' : '◉'}</button><button type="button" data-mr-up aria-label="move rule up" title="move up">▲</button><button type="button" data-mr-dn aria-label="move rule down" title="move down">▼</button></span>` +
    `<button type="button" class="prov-x" data-mr-rm aria-label="remove rule" title="remove">✕</button>` +
    `<div class="mr-err" aria-live="polite"></div>` +
    `</div>`;
}

function modelRulesEditorHTML(val) {
  const rules = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap">` +
    `<div class="mr-hint">rules apply top→bottom to each stored model name · exact merges a whole spelling · pattern is a regex replace-all (RE2, $1 refs) · lower folds case · ◉ parks a rule</div>` +
    `<div class="mr-tools">` +
    `<select class="mr-tpl" aria-label="add a rule from a template"><option value="">add from template…</option>${mrTemplates().map((t, i) => `<option value="${i}">${escapeHtml(t[0])}</option>`).join('')}</select>` +
    `<button type="button" class="mr-restore" data-mr-restore title="replace the draft with the shipped five-rule pipeline">↺ default pipeline</button>` +
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
// Keep one last result keyed by the live data object, its revision and the
// records array identity: preview keystrokes must not rescan the ring, while
// a live revision bump or a manual fixture replacement (new array without a
// bump) still invalidates it. The cache is bounded to that single result.
let _mrTrafficCache = null;
function mrTrafficExamples() {
  if (typeof lastData !== 'object' || !lastData || !Array.isArray(lastData.records)) return [];
  const rev = lastData._rev || 0;
  const cached = _mrTrafficCache;
  if (cached && cached.data === lastData && cached.rev === rev && cached.records === lastData.records) {
    return cached.list;
  }
  const counts = new Map();
  for (const r of lastData.records) {
    const m = String((r && r.model) || '').trim();
    if (m) counts.set(m, (counts.get(m) || 0) + 1);
  }
  const list = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
  _mrTrafficCache = { data: lastData, rev, records: lastData.records, list };
  return list;
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

// mrDraftChanged is the one draft-changed epilogue: every edit that
// mutates the rules draft (add, remove, reorder, park/unpark, restore)
// marks the sheet dirty, revalidates the whole draft and refreshes the
// preview bench. Per-keystroke typing and the settings open/refetch paint
// call the validate+preview pair directly - no draft mutation happens
// there, so they must not mark the sheet.
function mrDraftChanged(wrap) {
  markSettingsDirty(settingsKeyFor(wrap));
  validateModelRulesDraft(wrap);
  mrPreview(wrap);
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
  mrDraftChanged(wrap);
}

// mrApplyTemplate appends one pre-filled rule from the templates select -
// the common cases are one click (Cloudflare-style rule templates). The
// payloads come from mrTemplates (the server doc for shipped rules).
function mrApplyTemplate(sel) {
  const idx = parseInt(sel.value, 10);
  sel.value = '';
  const templates = mrTemplates();
  if (!Number.isInteger(idx) || !templates[idx]) return;
  const wrap = sel.closest('.mr-wrap');
  if (wrap.querySelectorAll('.mr-row').length >= MODEL_RULES_MAX) return;
  const t = templates[idx][1];
  wrap.querySelector('.mr-rows').insertAdjacentHTML('beforeend', modelRuleRowHTML(t));
  const rows = wrap.querySelectorAll('.mr-row');
  const row = rows[rows.length - 1];
  validateMrRow(row, null);
  markSettingsDirty(settingsKeyFor(sel));
  mrPreview(wrap);
  const from = row.querySelector('.mr-from');
  if (from && !from.value) from.focus();
}

// mrRestoreDefaults replaces the draft with the shipped pipeline exactly
// as the server reports it (the config defaults doc - no client-side
// mirror to drift). Nothing is committed until Apply; with no server
// defaults in reach it refuses instead of emptying the draft.
function mrRestoreDefaults(wrap) {
  const rules = settingsDoc?.defaults?.model_rules;
  if (!Array.isArray(rules)) return;
  wrap.querySelector('.mr-rows').innerHTML = rules.map(modelRuleRowHTML).join('');
  mrDraftChanged(wrap);
}

// ---- request overrides editor ----
// request_overrides: ordered rules rewriting the upstream request before
// relay (headers set/replace/remove plus OpenAI-wire token ceilings),
// scoped per client, provider and/or model. One visual grammar with the
// other structured editors: each rule is a .prov-sec card inside an
// .mr-wrap shell (tools row, count, muted tokens), the headers mapping
// reuses the providers editor's headerRowHTML family verbatim (its add
// row, Enter key, remove buttons and change-time token lint all ride the
// existing delegated wiring), and remove_headers reuses the chip grammar.
// Live per-row validation mirrors config.validateRequestOverrides at every
// keystroke (the validateMrRow discipline): scope required, action
// required, RFC 7230 names, the credential/protocol/x-proxy- ownership
// set, one canonical name never both set and removed, the body band
// (1..REQUEST_OVERRIDE_BODY_MAX) and the duplicate-scope rejection. Apply
// walks the draft and blocks the POST on the first invalid card, focusing
// the offending input.
// Scope inputs get datalist autocomplete from the dashboard's known sets
// (the wire-key owners) plus the settings doc's provider labels; free text
// stays allowed because rules may target values not yet observed.
// Mirrors config.RequestOverridesMax (64) - the count line and template
// add-gate use it.
const REQUEST_OVERRIDES_MAX = 64;
// REQUEST_OVERRIDE_BODY_MAX mirrors config's body-ceiling band (the
// validateRequestOverrides 1..1_000_000 range): the body inputs' max
// attributes and the live band check share this one number. The
// cross-language pin in js_mirrors binds it to the config owner.
const REQUEST_OVERRIDE_BODY_MAX = 1000000;
// RO_FORBIDDEN_HEADERS mirrors config.forbiddenOverrideHeaders: the
// credential and protocol names the proxy or the transport owns, keyed
// lowercase because HTTP header names are case-insensitive. The x-proxy-
// control prefix is denied as a whole by the prefix check in
// roForbiddenReason. Deleting one of these is as breaking as rewriting it,
// so both of a rule's header lists share the grammar.
const RO_FORBIDDEN_HEADERS = {
  authorization: 'credential',
  cookie: 'credential',
  'proxy-authorization': 'credential',
  host: 'protocol',
  'content-type': 'protocol',
  'content-length': 'protocol',
  'transfer-encoding': 'protocol',
  te: 'protocol',
  connection: 'protocol',
  'keep-alive': 'protocol',
  'proxy-authenticate': 'protocol',
  'proxy-connection': 'protocol',
  trailer: 'protocol',
  upgrade: 'protocol',
};
// RO_TEMPLATES are the one-click starter rules (the mrTemplates precedent):
// payloads stay minimal so the live validation guides the missing scope. The
// budget template sets both token spellings to the same value: the cap pair
// is precedence-merged (max_completion_tokens wins over max_tokens), so a
// client request carrying either spelling would shrug off a rule that sets
// only the other one.
const RO_TEMPLATES = {
  blank: { label: 'empty rule', starter: {} },
  budget: { label: 'raise the output budget for a provider', starter: { body: { max_tokens: 32768, max_completion_tokens: 32768 } } },
  header: { label: 'set an upstream header for a client', starter: { headers: { 'X-Title': 'my app' } } },
};

// roForbiddenReason mirrors config.ForbiddenOverrideHeader: which owner
// denies a header name, or '' when the name is allowed.
function roForbiddenReason(name) {
  const reason = RO_FORBIDDEN_HEADERS[name.toLowerCase()];
  if (reason) return reason;
  if (name.toLowerCase().startsWith('x-proxy-')) return 'control';
  return '';
}

// roForbiddenMsg is the operator wording for a denied header name (the
// config.forbiddenOverrideHeaderError mirror): each reason explains who
// owns the header, so the fix is obvious from the message alone.
function roForbiddenMsg(name, field) {
  const reason = roForbiddenReason(name);
  if (reason === 'credential')
    return `${field}: '${name}' is credential-owned and cannot be overridden; the proxy injects the provider key itself`;
  if (reason === 'protocol')
    return `${field}: '${name}' is protocol-owned and cannot be overridden; the proxy and the HTTP transport manage it`;
  return `${field}: '${name}' carries the x-proxy- control prefix and cannot be overridden; x-proxy- headers are the proxy's own request channel`;
}

const RO_SERVER_SPACE = /[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028-\u2029\u202f\u205f\u3000]/;
function trimServerSpace(value) {
  const text = String(value == null ? '' : value);
  let start = 0;
  let end = text.length;
  while (start < end && RO_SERVER_SPACE.test(text[start])) start++;
  while (end > start && RO_SERVER_SPACE.test(text[end - 1])) end--;
  return text.slice(start, end);
}

// roHeaderValueOK mirrors config.ValidHeaderValue plus the overrides'
// non-empty rule: no NUL/CR/LF or other control bytes, and not blank after
// trim. Tab is allowed like the server allows it.
function roHeaderValueOK(v) {
  const value = trimServerSpace(v);
  if (!value) return false;
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c === 9 || (c >= 32 && c !== 127)) continue;
    return false;
  }
  return true;
}

// roKnownOptions derives one kind's autocomplete options: provider from
// the settings doc's provider labels plus the dashboard's known-providers
// sets, client and model from the dashboard's known sets (the wire-key
// owners - knownNames' union discipline across the operator states).
function roKnownOptions(kind) {
  const out = new Set();
  if (kind === 'provider') {
    const m = settingsDoc && settingsDoc.values && settingsDoc.values.providers;
    if (m && typeof m === 'object') for (const k of Object.keys(m)) if (k) out.add(String(k));
  }
  const key = kind === 'client' ? KNOWN_CLIENTS_KEY : kind === 'provider' ? KNOWN_PROVIDERS_KEY : KNOWN_MODELS_KEY;
  for (const st of [pauseState, debugState, throttleState]) {
    const list = st && st[key];
    if (Array.isArray(list)) for (const n of list) if (n) out.add(String(n));
  }
  return [...out].sort();
}

// roDlId owns the scope datalist id grammar shared by the generator, the
// rule card's three list attributes and the focus-time refresher, so the
// association cannot drift between render and refresh.
function roDlId(kind) {
  return 'ro-dl-' + kind;
}

// roDlOptionsHTML owns the datalist options expression shared by the
// initial render and the focus-time refresher: both spellings must derive
// the same option list from the same known sets.
function roDlOptionsHTML(kind) {
  return roKnownOptions(kind).map(o => `<option value="${escapeHtml(o)}"></option>`).join('');
}

function roDatalistHTML(kind) {
  return `<datalist id="${roDlId(kind)}">${roDlOptionsHTML(kind)}</datalist>`;
}

// roSyncDatalists refreshes the autocomplete options at focus time: the
// known sets arrive on their own refresh cadence, so options picked at
// render time can be stale while the sheet is open.
function roSyncDatalists(wrap) {
  if (!wrap) return;
  ['client', 'provider', 'model'].forEach(kind => {
    const dl = wrap.querySelector('#' + roDlId(kind));
    if (dl) dl.innerHTML = roDlOptionsHTML(kind);
  });
}

function roRemoveChipHTML(name) {
  return `<span class="prov-chip" data-rh="${escapeHtml(name)}"><span class="prov-chip-p">${escapeHtml(name)}</span><button type="button" class="prov-x" data-prov-chip-rm aria-label="remove ${escapeHtml(name)}">✕</button></span>`;
}

// roRuleLabel owns the rule-number label ("rule N", the 1-based index the
// server cites in request_overrides[i] errors) shared by the card render
// and roRenumber, so the initial label and the rewritten one cannot drift.
function roRuleLabel(i) {
  return 'rule ' + (i + 1);
}

// roRuleFieldLabel composes an ordinal-carrying input's accessible name
// from the same roRuleLabel ordinal the visible rule number uses.
function roRuleFieldLabel(i, suffix) {
  return roRuleLabel(i) + ' ' + suffix;
}

// roRuleFieldAttrs renders an ordinal-carrying input's accessible-name
// attributes: the initial aria-label composes through roRuleLabel, and
// data-ro-lbl carries the suffix roRenumber restamps the label from, so
// the rendered names and the rewritten ones share one composer and
// cannot drift after an add or remove.
function roRuleFieldAttrs(i, suffix) {
  return `aria-label="${escapeHtml(roRuleFieldLabel(i, suffix))}" data-ro-lbl="${escapeHtml(suffix)}"`;
}

let roRuleDomSeq = 0;

function roRuleDomToken() {
  return ++roRuleDomSeq;
}

function roRuleActionLabel(i, suffix) {
  return roRuleFieldLabel(i, suffix);
}

function roRuleActionAttrs(i, suffix) {
  const label = roRuleActionLabel(i, suffix);
  return `aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}" data-ro-action="${escapeHtml(suffix)}"`;
}

function roSummaryValue(value) {
  const text = trimServerSpace(value);
  if (!text) return 'any';
  if (text === 'any') return '"any" (exact)';
  return text === '*' ? '* (exact)' : text;
}

function roCountLabel(n, singular, plural) {
  return `${n} ${n === 1 ? singular : plural || singular + 's'}`;
}

function roScopeSummary(r) {
  r = r || {};
  return `client ${roSummaryValue(r.client)} · provider ${roSummaryValue(r.provider)} · model ${roSummaryValue(r.model)}`;
}

function roFormatActionSummary(headers, removes, body) {
  return `${roCountLabel(headers, 'header')} · ${roCountLabel(removes, 'remove header')} · ${roCountLabel(body, 'body ceiling')}`;
}

function roActionSummary(r) {
  r = r || {};
  const headers = r.headers && typeof r.headers === 'object' && !Array.isArray(r.headers) ? Object.keys(r.headers).length : 0;
  const removes = Array.isArray(r.remove_headers) ? r.remove_headers.length : 0;
  const body = r.body && typeof r.body === 'object' ? r.body : {};
  const bodyCount = ['max_tokens', 'max_completion_tokens'].filter(k => body[k] != null && trimServerSpace(body[k]) !== '').length;
  return roFormatActionSummary(headers, removes, bodyCount);
}

function roSyncSummary(card) {
  if (!card) return;
  const scope = roScopeSummary({
    client: (card.querySelector('.ro-client') || {}).value,
    provider: (card.querySelector('.ro-provider') || {}).value,
    model: (card.querySelector('.ro-model') || {}).value,
  });
  const headers = card.querySelectorAll('.prov-hmap .prov-hrow').length;
  const removes = card.querySelectorAll('.prov-chips .prov-chip').length;
  const body = [...card.querySelectorAll('.ro-max-tokens, .ro-max-mct')].filter(input => trimServerSpace(input.value)).length;
  const actions = roFormatActionSummary(headers, removes, body);
  const scopeEl = card.querySelector('.ro-summary-scope');
  const actionEl = card.querySelector('.ro-summary-actions');
  if (scopeEl) { scopeEl.textContent = scope; scopeEl.title = scope; }
  if (actionEl) { actionEl.textContent = actions; actionEl.title = actions; }
  if (card.hasAttribute('tabindex')) {
    const i = Number(card.dataset.roIndex || 0);
    card.setAttribute('aria-label', `${roRuleLabel(i)}: ${scope}; ${actions}`);
  }
}

function roSetCardExpanded(card, expanded) {
  if (!card) return;
  const open = !!expanded;
  card.dataset.roExpanded = open ? '1' : '0';
  const body = card.querySelector('.ro-rule-body');
  const toggle = card.querySelector('[data-ro-toggle]');
  if (body) body.hidden = !open;
  if (!toggle) return;
  toggle.setAttribute('aria-expanded', String(open));
  if (body) toggle.setAttribute('aria-controls', body.id);
  const action = open ? 'collapse rule details' : 'expand rule details';
  const label = roRuleActionLabel(Number(card.dataset.roIndex || 0), action);
  toggle.setAttribute('aria-label', label);
  toggle.title = label;
  const caret = toggle.querySelector('.ro-disclosure-caret');
  if (caret) caret.textContent = open ? '▾' : '▸';
}

function roSetActionLabel(el, i, suffix) {
  if (!el) return;
  const label = roRuleActionLabel(i, suffix);
  el.setAttribute('aria-label', label);
  el.title = label;
}

function roSyncActionLabels(card, i) {
  if (!card) return;
  card.querySelectorAll('[data-ro-action]').forEach(el => {
    if (!el.matches('[data-ro-toggle]')) roSetActionLabel(el, i, el.dataset.roAction);
  });
  roSetActionLabel(card.querySelector('[data-prov-add-header]'), i, 'add header');
  roSetActionLabel(card.querySelector('[data-ro-rh-add]'), i, 'add remove header');
  roSetActionLabel(card.querySelector('.ro-rh-in'), i, 'add remove header');
  card.querySelectorAll('.prov-add-h .sp-hname').forEach(el => roSetActionLabel(el, i, 'add header name'));
  card.querySelectorAll('.prov-add-h .sp-hval').forEach(el => roSetActionLabel(el, i, 'add header value'));
  card.querySelectorAll('.prov-hrow .sp-hname').forEach(el => roSetActionLabel(el, i, 'upstream header name'));
  card.querySelectorAll('.prov-hrow .sp-hval').forEach(el => roSetActionLabel(el, i, 'upstream header value'));
  card.querySelectorAll('.prov-hrow [data-prov-hrow-rm]').forEach(el => {
    const name = trimServerSpace((el.closest('.prov-hrow').querySelector('.sp-hname') || {}).value) || 'header';
    roSetActionLabel(el, i, `remove ${name}`);
  });
  card.querySelectorAll('.prov-chip [data-prov-chip-rm]').forEach(el => {
    const name = trimServerSpace((el.closest('.prov-chip') || {}).dataset?.rh) || 'header';
    roSetActionLabel(el, i, `remove ${name}`);
  });
  roSetCardExpanded(card, card.dataset.roExpanded === '1');
}

function roRuleCardHTML(r, i, expanded = false) {
  r = r || {};
  const headers = r.headers && typeof r.headers === 'object' && !Array.isArray(r.headers) ? r.headers : {};
  const removes = Array.isArray(r.remove_headers) ? r.remove_headers : [];
  const body = r.body && typeof r.body === 'object' ? r.body : {};
  const open = !!expanded;
  const domToken = roRuleDomToken();
  const bodyId = `ro-rule-body-${domToken}`;
  const errorId = `ro-rule-error-${domToken}`;
  const scope = roScopeSummary(r);
  const actions = roActionSummary(r);
  return `<div class="prov-sec ro-rule" role="group" tabindex="-1" data-ro-index="${i}" data-ro-expanded="${open ? '1' : '0'}" aria-label="${escapeHtml(roRuleLabel(i) + ': ' + scope + '; ' + actions)}">` +
    `<div class="ro-rule-head">` +
    `<button type="button" class="ro-disclosure" data-ro-toggle ${roRuleActionAttrs(i, 'toggle')} aria-expanded="${open}" aria-controls="${bodyId}"><span class="ro-disclosure-caret" aria-hidden="true">${open ? '▾' : '▸'}</span></button>` +
    `<span class="ro-num">${escapeHtml(roRuleLabel(i))}</span>` +
    `<div class="ro-summary"><span class="ro-summary-scope" title="${escapeHtml(scope)}">${escapeHtml(scope)}</span><span class="ro-summary-actions" title="${escapeHtml(actions)}">${escapeHtml(actions)}</span></div>` +
    `<span class="ro-head-actions"><button type="button" class="ro-action" data-ro-up ${roRuleActionAttrs(i, 'move up')}>Move up</button><button type="button" class="ro-action" data-ro-dn ${roRuleActionAttrs(i, 'move down')}>Move down</button><button type="button" class="prov-x ro-action" data-ro-rm ${roRuleActionAttrs(i, 'remove rule')}>Remove</button></span>` +
    `</div>` +
    `<div class="ro-rule-body" id="${bodyId}" role="region" aria-label="${escapeHtml(roRuleLabel(i) + ' details')}"${open ? '' : ' hidden'}>` +
    `<div class="prov-lb"><span>scope</span><span class="prov-sub">an empty field matches any request</span></div>` +
    `<div class="st-ctl-line ro-scope">` +
    `<label class="ro-field"><span>client</span><input class="ro-client" list="${roDlId('client')}" value="${escapeHtml(r.client || '')}" placeholder="any client - e.g. claude-code" ${roRuleFieldAttrs(i, 'client scope')}></label>` +
    `<label class="ro-field"><span>provider</span><input class="ro-provider" list="${roDlId('provider')}" value="${escapeHtml(r.provider || '')}" placeholder="any provider - e.g. nano-gpt.com" ${roRuleFieldAttrs(i, 'provider scope')}></label>` +
    `<label class="ro-field"><span>model</span><input class="ro-model" list="${roDlId('model')}" value="${escapeHtml(r.model || '')}" placeholder="any model - e.g. glm-5.3" ${roRuleFieldAttrs(i, 'model scope')}></label>` +
    `</div>` +
    `<div class="prov-lb"><span>headers</span><span class="prov-sub">set or replace upstream headers - e.g. X-Title, my app</span></div>` +
    `<div class="prov-hmap">${Object.entries(headers).map(([n, v]) => headerRowHTML(n, v)).join('')}</div>` +
    `<div class="prov-add prov-add-h"><input class="sp-hname" placeholder="header name… ↵" aria-label="add header name"><input class="sp-hval" placeholder="value - e.g. my-app/1.0 ↵" aria-label="add header value"><button type="button" class="btn prov-addbtn" data-prov-add-header aria-label="add header">+</button></div>` +
    `<div class="prov-lb"><span>remove headers</span><span class="prov-sub">delete upstream headers by name - e.g. User-Agent</span></div>` +
    `<div class="prov-chips">${removes.map(n => roRemoveChipHTML(String(n == null ? '' : n))).join('')}</div>` +
    `<div class="prov-add"><input class="ro-rh-in" placeholder="header name to remove… ↵" aria-label="add header to remove"><button type="button" class="btn prov-addbtn" data-ro-rh-add aria-label="add header to remove">+</button></div>` +
    `<div class="prov-lb"><span>body</span><span class="prov-sub">output-token ceilings on the OpenAI wire - empty leaves the request's own value</span></div>` +
    `<div class="st-ctl-line ro-body">` +
    `<label class="ro-field"><span>max_tokens</span><input class="ro-max-tokens" type="number" min="1" max="${REQUEST_OVERRIDE_BODY_MAX}" step="1" value="${escapeHtml(body.max_tokens == null ? '' : String(body.max_tokens))}" placeholder="max_tokens - e.g. 32768" ${roRuleFieldAttrs(i, 'max_tokens ceiling')}></label>` +
    `<label class="ro-field"><span>max_completion_tokens</span><input class="ro-max-mct" type="number" min="1" max="${REQUEST_OVERRIDE_BODY_MAX}" step="1" value="${escapeHtml(body.max_completion_tokens == null ? '' : String(body.max_completion_tokens))}" placeholder="max_completion_tokens - e.g. 32768" ${roRuleFieldAttrs(i, 'max_completion_tokens ceiling')}></label>` +
    `</div>` +
    `<div class="mr-err ro-err" id="${errorId}" role="status" aria-live="polite" aria-atomic="true"></div>` +
    `</div></div>`;
}

function requestOverridesEditorHTML(val) {
  const rules = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap ro-wrap">` +
    `<div class="mr-hint">each rule rewrites the upstream request before relay · a rule matches when every non-empty scope field equals the request's value exactly · all matching rules apply in list order, the later rule wins · headers set or replace, remove_headers deletes, body stamps output-token ceilings</div>` +
    `<div class="mr-tools"><div class="ro-add"><button type="button" class="btn ro-add-btn" data-ro-add aria-label="add rule" aria-expanded="false" aria-controls="ro-template-menu" title="add a request-override rule from a template">+ add rule</button><div class="ro-tpl-menu" id="ro-template-menu" data-ro-tpl-menu hidden>${Object.entries(RO_TEMPLATES).map(([kind, template]) => `<button type="button" class="ro-tpl-item" data-ro-tpl="${kind}" title="${escapeHtml(template.label)}">${escapeHtml(template.label)}</button>`).join('')}</div></div><span class="mr-count muted"></span></div>` +
    `<div class="ro-rows">${rules.map((r, i) => roRuleCardHTML(r, i)).join('')}</div>` +
    roDatalistHTML('client') + roDatalistHTML('provider') + roDatalistHTML('model') +
    `</div>`;
}

function roSetTemplateMenuOpen(menu, open) {
  if (!menu) return;
  if (menu._roCloseTimer) { clearTimeout(menu._roCloseTimer); menu._roCloseTimer = null; }
  menu.hidden = !open;
  const btn = menu.parentElement && menu.parentElement.querySelector('[data-ro-add]');
  if (btn) btn.setAttribute('aria-expanded', String(!!open));
}

function closeRoTemplateMenus(scope) {
  (scope || document).querySelectorAll('.ro-tpl-menu:not([hidden])').forEach(menu => roSetTemplateMenuOpen(menu, false));
}

function roApplyTemplate(source) {
  const kind = String((source && source.dataset && source.dataset.roTpl) || '');
  const wrap = source && source.closest ? source.closest('.ro-wrap') : null;
  if (!wrap) return;
  closeRoTemplateMenus(wrap);
  const template = RO_TEMPLATES[kind];
  if (!template || wrap.querySelectorAll('.ro-rule').length >= REQUEST_OVERRIDES_MAX) return;
  const rows = wrap.querySelector('.ro-rows');
  rows.insertAdjacentHTML('beforeend', roRuleCardHTML(template.starter, rows.children.length, true));
  roDraftChanged(wrap);
  const card = [...wrap.querySelectorAll('.ro-rule')].pop();
  const focus = card && card.querySelector(kind === 'budget' ? '.ro-provider' : '.ro-client');
  if (focus) focus.focus();
}

// addRoRemoveHeader appends one remove_headers chip from the add input.
// Deny by default on the token grammar (flash instead of adding a chip the
// server would refuse); ownership and cross-list conflicts surface through
// the live validation with their full messages.
function addRoRemoveHeader(card) {
  const input = card && card.querySelector('.ro-rh-in');
  const chips = card && card.querySelector('.prov-chips');
  if (!input || !chips) return;
  const name = trimServerSpace(input.value);
  if (!name || !PROV_HEADER_RE.test(name)) { flashBadInput(input); return; }
  if ([...chips.querySelectorAll('.prov-chip')].some(c => trimServerSpace(c.dataset.rh || '').toLowerCase() === name.toLowerCase())) { flashBadInput(input); return; }
  chips.insertAdjacentHTML('beforeend', roRemoveChipHTML(name));
  input.value = '';
  roDraftChanged(card.closest('.ro-wrap'));
}

function validateRoRow(card, seen, index) {
  const err = card.querySelector('.ro-err');
  if (err) err.replaceChildren();
  else card.querySelectorAll('.ro-error-detail').forEach(detail => detail.remove());
  card.querySelectorAll('.prov-bad').forEach(el => el.classList.remove('prov-bad'));
  card.querySelectorAll('input, button').forEach(el => {
    el.removeAttribute('aria-invalid');
    el.removeAttribute('aria-describedby');
  });
  const detailByControl = new Map();
  let detailIndex = 0;
  const scopeVal = sel => trimServerSpace((card.querySelector(sel) || {}).value);
  const client = scopeVal('.ro-client');
  const provider = scopeVal('.ro-provider');
  const model = scopeVal('.ro-model');
  const headerRows = [...card.querySelectorAll('.prov-hmap .prov-hrow')];
  const removeChips = [...card.querySelectorAll('.prov-chips .prov-chip')].map(chip => ({
    name: trimServerSpace(chip.dataset.rh || ''),
    button: chip.querySelector('[data-prov-chip-rm]'),
  }));
  const bodyFields = [
    ['max_tokens', card.querySelector('.ro-max-tokens')],
    ['max_completion_tokens', card.querySelector('.ro-max-mct')],
  ];
  let level = 'ok';
  const fail = (input, text) => {
    if (input) {
      input.classList.add('prov-bad');
      input.setAttribute('aria-invalid', 'true');
      if (err && err.id && !detailByControl.has(input)) {
        const detail = document.createElement('div');
        detail.className = 'ro-error-detail';
        detail.id = `${err.id}-detail-${detailIndex++}`;
        detail.textContent = text;
        (err || card).appendChild(detail);
        detailByControl.set(input, detail.id);
        input.setAttribute('aria-describedby', detail.id);
      }
    }
    if (level !== 'error') level = 'error';
  };
  if (!client && !provider && !model) {
    ['.ro-client', '.ro-provider', '.ro-model'].forEach(sel => fail(card.querySelector(sel),
      'no scope set - give the rule a client, provider or model, or remove the rule'));
  } else if (!headerRows.length && !removeChips.length && !bodyFields.some(([, input]) => input && trimServerSpace(input.value))) {
    fail(card.querySelector('.prov-add-h .sp-hname'),
      'no action set - give the rule a headers entry, a remove_headers entry or a body value, or remove the rule');
  } else {
    const seenHeaders = new Set();
    const headerKeys = new Set();
    for (const row of headerRows) {
      const nameIn = row.querySelector('.sp-hname');
      const valIn = row.querySelector('.sp-hval');
      const name = trimServerSpace((nameIn || {}).value);
      const value = trimServerSpace((valIn || {}).value);
      if (!name) { fail(nameIn, 'headers: a header name is empty or only whitespace - fill it in or remove the row'); continue; }
      if (!PROV_HEADER_RE.test(name)) { fail(nameIn, `headers: '${name}' is not a valid header name (RFC 7230 token)`); continue; }
      if (roForbiddenReason(name)) { fail(nameIn, roForbiddenMsg(name, 'headers')); continue; }
      const key = name.toLowerCase();
      if (seenHeaders.has(key)) { fail(nameIn, `headers: duplicate HTTP header name '${name}'`); continue; }
      seenHeaders.add(key);
      headerKeys.add(key);
      if (!roHeaderValueOK(value)) fail(valIn, `headers: ${name}: value must be a non-empty single-line header value`);
    }
    const seenRemoves = new Set();
    for (const {name, button} of removeChips) {
      if (!name || !PROV_HEADER_RE.test(name)) { fail(button, `remove_headers: '${name}' is not a valid header name (RFC 7230 token)`); continue; }
      if (roForbiddenReason(name)) { fail(button, roForbiddenMsg(name, 'remove_headers')); continue; }
      const key = name.toLowerCase();
      if (seenRemoves.has(key)) { fail(button, `remove_headers: duplicate HTTP header name '${name}'`); continue; }
      seenRemoves.add(key);
      if (headerKeys.has(key)) fail(button,
        `'${name}' is both set in headers and removed in remove_headers; keep exactly one action per header`);
    }
    for (const [name, input] of bodyFields) {
      const text = input && trimServerSpace(input.value);
      if (!text) continue;
      const n = Number(text);
      if (!Number.isSafeInteger(n) || n < 1 || n > REQUEST_OVERRIDE_BODY_MAX) fail(input, `body.${name}: '${text}' must be a whole number between 1 and ${REQUEST_OVERRIDE_BODY_MAX}`);
    }
  }
  const triple = client + '\u0000' + provider + '\u0000' + model;
  if (level === 'ok' && seen && seen.has(triple)) {
    const focusedScope = [...card.querySelectorAll('.ro-client, .ro-provider, .ro-model')].find(input => input === document.activeElement);
    fail(focusedScope || card.querySelector('.ro-client'), `duplicate scope with rule ${seen.get(triple)} - the same client, provider and model; merge the rules or change one scope`);
  } else if (seen && (client || provider || model) && !seen.has(triple)) {
    seen.set(triple, index + 1);
  }
  card.dataset.roState = level;
  if (err) err.dataset.level = level === 'error' ? 'error' : '';
  roSyncSummary(card);
  roSyncActionLabels(card, index);
  if (level === 'error') roSetCardExpanded(card, true);
  return level;
}

// validateRequestOverridesDraft walks every rule card in order and returns
// the first invalid one (Apply blocks on it; the button flow focuses and
// flashes it).
function validateRequestOverridesDraft(wrap) {
  const seen = new Map();
  let firstBad = null;
  wrap.querySelectorAll('.ro-rule').forEach((card, i) => {
    if (validateRoRow(card, seen, i) === 'error' && !firstBad) firstBad = card;
  });
  return { ok: !firstBad, firstBad };
}

// roRenumber rewrites the cards' rule numbers after any add or remove so
// they keep matching the request_overrides[i] indexes the server cites in
// its validation errors. The ordinal-carrying accessible names restamp
// through the same composer (roRuleFieldLabel reads data-ro-lbl), so a
// non-tail removal cannot leave a scope or body input announcing a stale
// rule number.
function roRenumber(wrap) {
  wrap.querySelectorAll('.ro-rule').forEach((card, i) => {
    card.dataset.roIndex = String(i);
    const el = card.querySelector('.ro-num');
    if (el) el.textContent = roRuleLabel(i);
    card.querySelectorAll('[data-ro-lbl]').forEach(input => {
      input.setAttribute('aria-label', roRuleFieldLabel(i, input.dataset.roLbl));
    });
    const body = card.querySelector('.ro-rule-body');
    if (body) body.setAttribute('aria-label', `${roRuleLabel(i)} details`);
    roSyncSummary(card);
    roSyncActionLabels(card, i);
  });
}

// roSyncCount keeps the "N / 64 rules" line honest and gates the template
// add-control at the cap (mirrors config.RequestOverridesMax).
function roSyncCount(wrap) {
  const n = wrap.querySelectorAll('.ro-rule').length;
  const count = wrap.querySelector('.mr-count');
  if (count) count.textContent = n + ' / ' + REQUEST_OVERRIDES_MAX + ' rules';
  const cap = n >= REQUEST_OVERRIDES_MAX;
  const add = wrap.querySelector('[data-ro-add]');
  if (add) add.disabled = cap;
  wrap.querySelectorAll('[data-ro-tpl]').forEach(item => { item.disabled = cap; });
  if (cap) closeRoTemplateMenus(wrap);
}

// roDraftChanged is the request-overrides draft-changed epilogue (the
// mrDraftChanged discipline): mark the sheet dirty, revalidate the whole
// draft, renumber the cards and refresh the count.
function roDraftChanged(wrap) {
  markSettingsDirty(settingsKeyFor(wrap));
  validateRequestOverridesDraft(wrap);
  roRenumber(wrap);
  roSyncCount(wrap);
}

// collectRequestOverrides reads the editor into the strict POST shape:
// scope fields always ride along (empty string = wildcard), header names
// are trimmed like the server trims them, and empty collections stay
// absent (headers, remove_headers, body and its fields) so a rule carries
// only what it sets. The shared duplicate-key put discipline throws on
// Apply for an exact duplicate or empty header name; canonical-case
// conflicts and the rest of the grammar are the live gate's to block and
// the server's to validate.
function collectRequestOverrides(wrap, put) {
  const out = [];
  wrap.querySelectorAll('.ro-rule').forEach(card => {
    const r = Object.create(null);
    r.client = trimServerSpace((card.querySelector('.ro-client') || {}).value);
    r.provider = trimServerSpace((card.querySelector('.ro-provider') || {}).value);
    r.model = trimServerSpace((card.querySelector('.ro-model') || {}).value);
    const headers = Object.create(null);
    card.querySelectorAll('.prov-hmap .prov-hrow').forEach(row => {
      const n = trimServerSpace((row.querySelector('.sp-hname') || {}).value);
      const v = trimServerSpace((row.querySelector('.sp-hval') || {}).value);
      put(headers, n, v, row.querySelector('.sp-hname'));
    });
    if (Object.keys(headers).length) r.headers = headers;
    const removes = [...card.querySelectorAll('.prov-chip')].map(c => trimServerSpace(c.dataset.rh || '')).filter(Boolean);
    if (removes.length) r.remove_headers = removes;
    const body = Object.create(null);
    for (const [key, sel] of [['max_tokens', '.ro-max-tokens'], ['max_completion_tokens', '.ro-max-mct']]) {
      const input = card.querySelector(sel);
      const text = input && trimServerSpace(input.value);
      if (!text) continue;
      const n = Number(text);
      body[key] = Number.isSafeInteger(n) ? n : text;
    }
    if (Object.keys(body).length) r.body = body;
    out.push(r);
  });
  return out;
}

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
    `<label class="st-check"><input type="checkbox" class="st-switch sc-strip"${e.strip ? ' checked' : ''}><span>strip every configured field from the top level of the relayed upstream body - unchecked forwards it unchanged</span></label>` +
    `<div class="mr-err sc-err" aria-live="polite"></div>` +
    `</div>`;
}

function subConversationsEditorHTML(val) {
  const entries = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap sc-wrap">` +
    `<div class="mr-hint">each entry tracks one classified client's sub-conversation identity from top-level request body fields · the first present field decides: a present value that is not a JSON string counts as absent and the next field is checked, while a present string that cannot be decoded or fails the identity rules drops the identity for the request - no later field is consulted, never a client error · the value groups requests under a k: identity, after X-Proxy-Session and before client+key grouping · strip removes every configured field present at the top level of the relayed body</div>` +
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
    refreshAggregates();
    fetchBootstrap('full');
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
