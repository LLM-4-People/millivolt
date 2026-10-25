// Settings provider-editor actions: the delegated half of the provider cards,
// aliases and mapping rows that jsdom drives through #settings-fields clicks,
// keys and pickers. The renderers live in settings_providers.js; these
// handlers mutate their DOM and rebuild only the affected section. The click
// owner also carries the sub-conversations remove branches (the same delegated
// sheet listener). settings.js owns the accepted document and the shell;
// chrome_settings.js's SETTINGS_MODULES list owns the load order.

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
