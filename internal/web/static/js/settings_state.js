// Settings sheet state and actions: the delegated sheet wiring, the per-key
// collectors and dirty baseline, the search/category view, apply/revert, the
// storage purge controls and the module registration the chrome shell opens.
// The renderers and editors live in the earlier SETTINGS_MODULES entries;
// this module is last so every function it names at wiring time exists.

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
