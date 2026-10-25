// Settings backup and restore: the archive download, the validated restore
// inspect and diff, the apply/cancel lifecycle, and the storage category's two
// deletions. The shared live snapshot and scalar field renderers live here too
// because the backup pane and every category field row are painted by the same
// fill pass. settings.js owns the accepted document and the shell; this module
// only renders and runs these surfaces, and chrome.js's SETTINGS_MODULES list
// owns the load order.

let backupMode = 'replace';
let backupIncludeConfig = true;
let backupIncludeDatabase = true;
let backupInspect = null; // {file, data} after a validated inspect, before apply
let backupBusy = false;
let backupReq = 0; // newest backup fetch owns state; cancel invalidates older ones

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
  const canDb = !!b.database && !!db.present && db.inspectable !== false;
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
    if (!canDb) html += `<span class="st-hint">${escapeHtml(db.inspectable === false && db.reason ? db.reason : 'durable storage is disabled')}</span>`;
  }
  if (wantCfg || wantDb) {
    html += `<label class="st-check"><input type="radio" name="backup-mode" value="replace"${merge ? '' : ' checked'}> replace</label>` +
      `<label class="st-check"><input type="radio" name="backup-mode" value="merge"${merge ? ' checked' : ''}> merge</label>`;
    const hints = [];
    const cliKeys = cliOverrideKeys();
    if (wantCfg) {
      if (merge) hints.push('overlays keys that differ from default');
      else if (cliKeys.length) hints.push('writes every key from the backup except CLI-overridden ' + cliKeys.join(' and '));
      else hints.push('writes every key from the backup');
    }
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
  return backupCheckOn('backup-include-database', !!(b.database && db.present && db.inspectable !== false));
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

// cliOverrideKeys names the settings the process-start CLI flags keep pinned
// through a restore. prepareRestoreConfig applies the live listen/db_path after
// either mode, so the preview must not claim a replace resets them.
function cliOverrideKeys() {
  // The settings payload already enumerates the process-start CLI overrides.
  // Sort for a stable preview; never keep a second hardcoded key list here.
  const overrides = (settingsDoc && settingsDoc.overrides) || {};
  return Object.keys(overrides).sort();
}

function backupDiffRowsHTML(doc) {
  if (!backupInspect || !backupInspect.data) return '';
  const cfg = backupInspect.data.config || {};
  if (!cfg.present) return '';
  const modified = Array.isArray(cfg.modified) ? cfg.modified : [];
  const modifiedSet = new Set(modified);
  const vsLive = new Set(Array.isArray(cfg.vs_live) ? cfg.vs_live : []);
  // A replace restore writes every key, including a key that is default in
  // the backup but customized live. Render the union so that reset is visible;
  // merge only overlays non-default backup keys, so a live-only key is
  // explicitly reported as unchanged instead.
  const keys = modified.slice();
  for (const key of vsLive) if (!modifiedSet.has(key)) keys.push(key);
  if (!keys.length) {
    return `<div class="st-row st-backup" data-cat="backup" data-backup="diff" data-label="restore config default" id="backup-preview"><div class="st-name">Config</div><div class="st-ctl"><span class="st-hint">every key in this backup is still the built-in default and matches the live config</span></div></div>`;
  }
  const fields = (doc && doc.fields) || (settingsDoc && settingsDoc.fields) || [];
  const byKey = {};
  for (const f of fields) byKey[f.key] = f;
  const defaults = (doc && doc.defaults) || (settingsDoc && settingsDoc.defaults) || {};
  const live = (doc && doc.values) || (settingsDoc && settingsDoc.values) || {};
  const values = cfg.values || {};
  const merge = backupMode === 'merge';
  const cliKeys = cliOverrideKeys();
  return keys.map((key, i) => {
    const label = (byKey[key] && byKey[key].label) || key;
    const val = backupScalar(values[key]);
    const inBackup = modifiedSet.has(key);
    const differsLive = vsLive.has(key);
    let hint = inBackup ? 'default ' + backupScalar(defaults[key]) : 'backup default ' + backupScalar(defaults[key]);
    if (differsLive) hint += ' · live ' + backupScalar(live[key]);
    else if (inBackup) hint += ' · same as live';
    if (cliKeys.includes(key)) hint += ' · preserved by the CLI override';
    else if (!inBackup) hint += merge ? ' · merge leaves it unchanged' : ' · replace resets it';
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
  else if (def != null && def !== '' && f.kind === 'model_rules' && JSON.stringify(val) === JSON.stringify(def)) hint = `<span class="st-hint">default</span>`;
  else if (def != null && def !== '' && f.kind !== 'providers' && f.kind !== 'aliases' && f.kind !== 'model_rules' && f.kind !== 'strings' && f.kind !== 'request_overrides' && f.kind !== 'sub_conversations' && String(val) === String(def)) {
    hint = `<span class="st-hint">default</span>`;
  } else if (def != null && def !== '' && f.kind !== 'providers' && f.kind !== 'aliases' && f.kind !== 'model_rules' && f.kind !== 'strings' && f.kind !== 'request_overrides' && f.kind !== 'sub_conversations') {
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
