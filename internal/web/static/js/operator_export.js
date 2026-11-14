// Operator export: the download builder, the shared purge predicate preview
// and the receipt. The record ring and the server's count endpoint are the
// only sources; this module never infers a count locally.

// ---------- export ----------
const OC_AGE_CHOICES = [['', 'any time'], ['1h', 'older than 1 hour'], ['24h', 'older than 1 day'], ['168h', 'older than 1 week']];
const OC_AFTER_CHOICES = [['', 'from the beginning'], ['1h', 'last hour'], ['24h', 'last day'], ['168h', 'last week']];

function ocBlankExportDraft() {
  return {
    mode: 'all', provider: '', model: '', client: '', conversation: '',
    status: '', errors: false, debug: false, errorType: '', age: '', after: '',
    count: null, counting: false, error: '', picker: null,
  };
}

function ocExportFilter(draft) {
  const now = Date.now();
  return {
    provider: draft.provider || '',
    model: draft.model || '',
    client: draft.client || '',
    conversation_id: draft.conversation || '',
    error_type: draft.errorType || '',
    status_code: draft.status ? Number(draft.status) : 0,
    has_error: !!draft.errors,
    debug: !!draft.debug,
    before_ms: draft.age ? now - parseGoDuration(draft.age) : 0,
    after_ms: draft.after ? now - parseGoDuration(draft.after) : 0,
  };
}

function ocExportURL(filter) {
  const p = new URLSearchParams();
  for (const [key, value] of Object.entries(filter)) {
    if (value === '' || value === 0 || value === false) continue;
    p.set(key, value === true ? '1' : String(value));
  }
  const qs = p.toString();
  return '/metrics/export' + (qs ? '?' + qs : '');
}

// purgeCount is the shared count owner for export previews and the Settings
// delete block. Both surfaces must preview with the exact predicate they act
// on, so the parse and validation live here once.
async function purgeCount(filter) {
  const doc = await operatorJsonBody('/admin/purge/count', {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(filter),
  }, 'count unavailable');
  if (!Number.isSafeInteger(doc.count) || doc.count < 0) throw new Error(doc.error || 'count unavailable');
  return doc.count;
}

function ocFilterActive(draft) {
  return !!(draft.provider || draft.model || draft.client || draft.conversation ||
    draft.status || draft.errors || draft.debug || draft.errorType || draft.age || draft.after);
}

function ocFilterValues(dim) {
  const recs = (typeof lastData !== 'undefined' && lastData?.records) || [];
  if (dim === 'provider') return [...new Set(recs.map(r => r.provider).filter(Boolean))].sort();
  if (dim === 'model') return [...new Set(recs.map(r => r.model).filter(Boolean))].sort();
  if (dim === 'client') return [...new Set(recs.map(r => r.client).filter(Boolean))].sort();
  if (dim === 'conversation') return [...new Set(recs.map(r => r.conversation_id).filter(Boolean))].sort();
  if (dim === 'status') return [...new Set(recs.map(r => r.status_code).filter(n => n > 0))].sort((a, b) => a - b).map(String);
  return [];
}

function ocFilterLabel(dim, value) {
  if (dim === 'status') return 'status ' + value;
  return String(value);
}

// ocExportFiltersHTML renders every record dimension through the console's one
// multi-dimension strip. Five stacked filter rows cost the export section more
// height than the download receipt itself.
const OC_FILTER_DIMS = ['provider', 'model', 'client', 'conversation', 'status'];

function ocFilterRowHTML(draft, dim) {
  const value = draft[dim] || '';
  const open = !!draft.picker && draft.picker.dim === dim;
  return ocDimChipHTML({
    dim, label: dim, open, set: !!value,
    attr: 'data-oc-filter-pick', key: 'filter-' + dim,
    valuesHTML: value
      ? `<span class="oc-cond-values"><span class="oc-chip">${escapeHtml(ocFilterLabel(dim, value))}` +
        `<button type="button" class="oc-chip-x" data-oc-filter-clear="${dim}" aria-label="clear ${dim} filter">×</button></span></span>`
      : '',
  });
}

function ocExportFiltersHTML(draft) {
  let picker = '';
  const chips = OC_FILTER_DIMS.map(dim => {
    if (draft.picker && draft.picker.dim === dim) picker = ocFilterPickerHTML(draft, dim);
    return ocFilterRowHTML(draft, dim);
  });
  return ocChipStripHTML(chips, picker);
}

function ocFilterPickerHTML(draft, dim) {
  const filter = (draft.picker.filter || '').toLowerCase();
  const values = ocFilterValues(dim).filter(value => !filter || value.toLowerCase().includes(filter));
  const rows = values.map(value => {
    const on = String(draft[dim] || '') === String(value);
    return `<button type="button" class="oc-pick-row" role="menuitemradio" aria-checked="${on}" data-oc-filter-value="${dim}" data-oc-value="${escapeHtml(value)}">` +
      `<span class="oc-pick-check" aria-hidden="true">${on ? '✓' : ''}</span><span class="oc-pick-label">${escapeHtml(ocFilterLabel(dim, value))}</span></button>`;
  }).join('');
  const empty = values.length ? '' : `<div class="oc-pick-empty">none yet - they appear as requests arrive</div>`;
  const custom = dim === 'conversation';
  return `<div class="oc-pick">` +
    `<input type="search" class="oc-pick-filter" data-oc-filter-filter="${dim}" value="${escapeHtml(draft.picker.filter || '')}" placeholder="filter ${escapeHtml(dim)}" aria-label="filter ${escapeHtml(dim)}" data-oc-key="filter-pick-${dim}">` +
    `<div class="oc-pick-list" role="menu" aria-label="${escapeHtml(dim)} values">` +
    `<button type="button" class="oc-pick-row" role="menuitemradio" aria-checked="${!draft[dim]}" data-oc-filter-value="${dim}" data-oc-value=""><span class="oc-pick-check" aria-hidden="true">${!draft[dim] ? '✓' : ''}</span><span class="oc-pick-label">any</span></button>` +
    rows + empty + `</div>` +
    (custom ? `<div class="oc-pick-custom"><input type="text" data-oc-filter-custom="${dim}" placeholder="exact conversation id" aria-label="exact conversation id" data-oc-key="filter-custom-${dim}"><button type="button" class="btn" data-oc-filter-add="${dim}">use</button></div>` : '') +
    `</div>`;
}

function ocRenderExport() {
  if (!ocDrafts.export) ocDrafts.export = ocBlankExportDraft();
  const draft = ocDrafts.export;
  const filtering = draft.mode === 'filter';
  const filter = ocExportFilter(draft);
  const count = draft.count;
  const receipt = draft.counting ? 'counting…'
    : draft.error ? draft.error
      : count == null ? (filtering ? 'choose a filter to preview the export' : 'every stored record')
        : fmt(count) + (count === 1 ? ' record' : ' records') + ' · JSON, gzip';
  const enabled = !filtering || (count != null && count > 0);
  const rows = filtering
    ? `<div class="oc-group"><div class="oc-label">matching</div>` +
      ocExportFiltersHTML(draft) +
      `<div class="oc-filter-pair">` +
      `<div class="oc-filter-row"><span class="oc-filter-name">errors</span>${ocChipRow('export-errors', ['any', 'only'], draft.errors ? 'only' : 'any', v => v)}</div>` +
      `<div class="oc-filter-row"><span class="oc-filter-name">debug</span>${ocChipRow('export-debug', ['any', 'only'], draft.debug ? 'only' : 'any', v => v)}</div>` +
      `</div>` +
      `<div class="oc-filter-pair">` +
      `<div class="oc-filter-row"><span class="oc-filter-name">newer than</span>${ocChipRow('export-after', OC_AFTER_CHOICES.map(([v]) => v), draft.after, v => OC_AFTER_CHOICES.find(([x]) => x === v)?.[1] || v)}</div>` +
      `<div class="oc-filter-row"><span class="oc-filter-name">older than</span>${ocChipRow('export-age', OC_AGE_CHOICES.map(([v]) => v), draft.age, v => OC_AGE_CHOICES.find(([x]) => x === v)?.[1] || v)}</div>` +
      `</div>` +
      `<label class="oc-field wide" data-oc-key="error-type"><span class="oc-field-name">error type</span><input type="text" data-oc-export-field="errorType" value="${escapeHtml(draft.errorType)}" placeholder="exact provider error type"></label>` +
      `</div>`
    : '';
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><div><h2 class="oc-h2">export</h2>` +
    `<p class="oc-sub">Stream matching request records as a gzip JSON artifact. The preview and the download use the same predicate.</p></div></div>` +
    `<div class="oc-group"><div class="oc-label">what to download</div>` +
    ocChipRow('export-mode', ['all', 'filter'], draft.mode, v => v === 'all' ? 'all history' : 'matching records') +
    `</div>` + rows +
    `<div class="oc-receipt"><div class="oc-receipt-main" id="oc-export-receipt" role="status" aria-live="polite">${escapeHtml(receipt)}</div>` +
    `<div class="oc-receipt-sub">Debug records carry their captured bodies. Treat every export as sensitive.</div>` +
    `<div class="oc-compose-ft"><button type="button" class="btn" data-oc-reset-export>reset</button>` +
    `<button type="button" class="btn btn-accent" id="oc-export-download" data-oc-download ${enabled ? '' : 'disabled'}>download</button></div></div>` +
    // Backups are not an export; the cross-link is one row, not a card.
    `<div class="oc-inline oc-export-more"><span class="oc-note">Configuration and database backups live in Settings.</span>` +
    `<button type="button" class="btn" data-oc-settings>open settings</button></div>` +
    `</div>`;
}

let _ocExportTimer = 0;
function ocScheduleExportCount() {
  clearTimeout(_ocExportTimer);
  const draft = ocDrafts.export;
  if (!draft || draft.mode !== 'filter') return;
  if (!ocFilterActive(draft)) {
    draft.count = null;
    draft.counting = false;
    draft.error = '';
    ocUpdateExportReceipt();
    return;
  }
  draft.counting = true;
  ocUpdateExportReceipt();
  _ocExportTimer = setTimeout(ocRunExportCount, 280);
}

async function ocRunExportCount() {
  const draft = ocDrafts.export;
  if (!draft) return;
  const filter = ocExportFilter(draft);
  const token = JSON.stringify(filter);
  draft.counting = true;
  ocUpdateExportReceipt();
  try {
    const count = await purgeCount(filter);
    if (ocDrafts.export !== draft || JSON.stringify(ocExportFilter(draft)) !== token) return;
    draft.count = count;
    draft.error = '';
  } catch (err) {
    if (ocDrafts.export !== draft) return;
    draft.count = null;
    draft.error = String(err.message || 'count unavailable');
  } finally {
    draft.counting = false;
    ocUpdateExportReceipt();
  }
}

function ocUpdateExportReceipt() {
  const draft = ocDrafts.export;
  const el = ocEl('oc-export-receipt');
  const btn = ocEl('oc-export-download');
  if (!draft || !el) return;
  const filtering = draft.mode === 'filter';
  const text = draft.counting ? 'counting…'
    : draft.error ? draft.error
      : draft.count == null ? (filtering ? 'choose a filter to preview the export' : 'every stored record')
        : fmt(draft.count) + (draft.count === 1 ? ' record' : ' records') + ' · JSON, gzip';
  el.textContent = text;
  el.dataset.tone = draft.error ? 'err' : '';
  if (btn) {
    btn.disabled = !!draft.counting || !!draft.error ||
      (filtering && (draft.count == null || draft.count === 0));
  }
}

function ocDownloadExport() {
  const draft = ocDrafts.export;
  if (!draft) return;
  if (draft.mode === 'all') {
    triggerDownload('/metrics/export', '');
    ocRecordChange('downloaded every stored record', null, 'ok');
    toast('downloading every stored record');
    return;
  }
  if (draft.count == null || draft.count <= 0) {
    ocScheduleExportCount();
    return;
  }
  triggerDownload(ocExportURL(ocExportFilter(draft)), '');
  const phrase = 'downloaded ' + fmt(draft.count) + (draft.count === 1 ? ' record' : ' records');
  ocRecordChange(phrase, null, 'ok');
  toast(phrase);
}
