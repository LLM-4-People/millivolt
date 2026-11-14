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

// ocExportFilter is the one predicate owner for the preview and the download.
// All history means no predicate at all: the filter fields persist across a
// mode switch so returning to matching records restores the work, but they
// must not reach the predicate while the mode says all history. A mode chip
// that says one thing while the URL carries another is the export lying about
// what it will download.
function ocExportFilter(draft) {
  if (draft.mode === 'all') {
    return {
      provider: '', model: '', client: '', conversation_id: '', error_type: '',
      status_code: 0, has_error: false, debug: false, before_ms: 0, after_ms: 0,
    };
  }
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

// ocFilterValues is one dimension's export vocabulary. The live ring alone is
// not enough: exporting history is the section's purpose, and after a restart
// the ring is empty while the server still knows every provider, model and
// client it has seen. Those three come from the same server-owned lists the
// scope picker uses, so a quiet proxy can still filter by what actually ran.
function ocFilterValues(dim) {
  const recs = (typeof lastData !== 'undefined' && lastData?.records) || [];
  const fromRing = pick => [...new Set(recs.map(pick).filter(Boolean))].sort();
  const merged = (dimension, pick) => [...new Set([
    ...(typeof ocKnownValues === 'function' ? ocKnownValues(dimension) : []),
    ...fromRing(pick),
  ])].sort();
  if (dim === 'provider') return merged('providers', r => r.provider);
  if (dim === 'model') return merged('models', r => r.model);
  if (dim === 'client') return merged('clients', r => r.client);
  if (dim === 'conversation') return fromRing(r => r.conversation_id);
  if (dim === 'status') return fromRing(r => r.status_code > 0 ? r.status_code : '').sort((a, b) => a - b);
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
    attr: 'data-oc-filter-pick', key: 'filter-' + dim, menuId: 'oc-filter-menu-' + dim,
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
  // A dimension's values are not all strings: status is a number read from the
  // record. Comparing through String keeps one filter rule for every
  // dimension, and a typed filter can never throw on a numeric vocabulary.
  const values = ocFilterValues(dim).filter(value =>
    !filter || String(value).toLowerCase().includes(filter));
  // The "any" row below is always first and always owns the menu's one tab
  // stop, so every value row is reachable by arrow only. Numbering these from
  // zero gave the first value the same tab stop as "any".
  const rows = values.map(value => {
    const on = String(draft[dim] || '') === String(value);
    return `<button type="button" class="oc-pick-row" role="menuitemradio" aria-checked="${on}" tabindex="-1" data-oc-filter-value="${dim}" data-oc-value="${escapeHtml(value)}">` +
      `<span class="oc-pick-check" aria-hidden="true">${on ? '✓' : ''}</span><span class="oc-pick-label">${escapeHtml(ocFilterLabel(dim, value))}</span></button>`;
  }).join('');
  const empty = values.length ? '' : `<div class="oc-pick-empty">none yet - they appear as requests arrive</div>`;
  const custom = dim === 'provider' || dim === 'model' || dim === 'client' || dim === 'conversation';
  const customHint = dim === 'conversation' ? 'exact conversation id'
    : dim === 'provider' ? 'provider name'
      : dim === 'model' ? 'model name' : 'client name';
  return `<div class="oc-pick">` +
    `<input type="search" class="oc-pick-filter" data-oc-filter-filter="${dim}" value="${escapeHtml(draft.picker.filter || '')}" placeholder="filter ${escapeHtml(dim)}" aria-label="filter ${escapeHtml(dim)}" data-oc-key="filter-pick-${dim}">` +
    `<div class="oc-pick-list" id="oc-filter-menu-${escapeHtml(dim)}" role="menu" aria-label="${escapeHtml(dim)} values">` +
    `<button type="button" class="oc-pick-row" role="menuitemradio" aria-checked="${!draft[dim]}" tabindex="0" data-oc-filter-value="${dim}" data-oc-value=""><span class="oc-pick-check" aria-hidden="true">${!draft[dim] ? '✓' : ''}</span><span class="oc-pick-label">any</span></button>` +
    rows + empty + `</div>` +
    (custom ? `<div class="oc-pick-custom"><input type="text" data-oc-filter-custom="${dim}" placeholder="${escapeHtml(customHint)}" aria-label="${escapeHtml(customHint)}" data-oc-key="filter-custom-${dim}"><button type="button" class="btn" data-oc-filter-add="${dim}">use</button></div>` : '') +
    `</div>`;
}

function ocRenderExport() {
  if (!ocDrafts.export) ocDrafts.export = ocBlankExportDraft();
  const draft = ocDrafts.export;
  // The receipt is a server count in both modes, so the section asks for one
  // when it opens with no answer yet. The schedule owner debounces and guards
  // against a stale reply.
  if (draft.count == null && !draft.counting && !draft.error) ocScheduleExportCount();
  const filtering = draft.mode === 'filter';
  const receipt = ocExportReceiptText(draft);
  const enabled = ocExportEnabled(draft);
  const rows = filtering
    ? `<div class="oc-group"><div class="oc-label">matching</div>` +
      ocExportFiltersHTML(draft) +
      `<div class="oc-filter-pair">` +
      `<div class="oc-filter-row"><span class="oc-filter-name">errors</span>${ocChipRow('export-errors', ['any', 'only'], draft.errors ? 'only' : 'any', v => v)}</div>` +
      `<div class="oc-filter-row"><span class="oc-filter-name">debug</span>${ocChipRow('export-debug', ['any', 'only'], draft.debug ? 'only' : 'any', v => v)}</div>` +
      `</div>` +
      // Each age list is one of four labelled spans, so a select states the
      // choice in one line instead of wrapping four long chips.
      `<div class="oc-filter-pair">` +
      `<div class="oc-filter-row"><span class="oc-filter-name">newer than</span>${ocChoiceSelect('export-after', OC_AFTER_CHOICES.map(([v]) => v), draft.after, v => OC_AFTER_CHOICES.find(([x]) => x === v)?.[1] || v)}</div>` +
      `<div class="oc-filter-row"><span class="oc-filter-name">older than</span>${ocChoiceSelect('export-age', OC_AGE_CHOICES.map(([v]) => v), draft.age, v => OC_AGE_CHOICES.find(([x]) => x === v)?.[1] || v)}</div>` +
      `</div>` +
      `<label class="oc-field wide" data-oc-key="error-type"><span class="oc-field-name">error type</span><input type="text" data-oc-export-field="errorType" value="${escapeHtml(draft.errorType)}" placeholder="exact provider error type"></label>` +
      `</div>`
    : '';
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><p class="oc-sub">Stream matching request records as a gzip JSON artifact. The preview and the download use the same predicate.</p></div>` +
    `<div class="oc-group"><div class="oc-label">what to download</div>` +
    ocChipRow('export-mode', ['all', 'filter'], draft.mode, v => v === 'all' ? 'all history' : 'matching records') +
    `</div>` + rows +
    `<div class="oc-receipt"><div class="oc-receipt-main" id="oc-export-receipt" role="status" aria-live="polite">${escapeHtml(receipt)}</div>` +
    `<div class="oc-receipt-sub">Debug records carry their captured bodies. Treat every export as sensitive.</div>` +
    `<div class="oc-compose-ft"><button type="button" class="btn" data-oc-reset-export>reset</button>` +
    // Same rule as the composer commits: a disabled button leaves the tab
    // order, so the receipt that says why must be its description.
    `<button type="button" class="btn btn-accent" id="oc-export-download" data-oc-download aria-describedby="oc-export-receipt" ${enabled ? '' : 'disabled'}>download</button></div></div>` +
    // Backups are not an export; the cross-link is one row, not a card.
    `<div class="oc-inline oc-export-more"><span class="oc-note">Configuration and database backups live in Settings.</span>` +
    `<button type="button" class="btn" data-oc-settings>open settings</button></div>` +
    `</div>`;
}

let _ocExportTimer = 0;
// ocScheduleExportCount is the one preview owner for both modes. All history
// is the same predicate with no filter, so it is previewed the same way: the
// console's promise is that the preview and the download use one predicate,
// and a mode that skipped the count could not keep that promise.
function ocScheduleExportCount() {
  clearTimeout(_ocExportTimer);
  const draft = ocDrafts.export;
  if (!draft) return;
  // Filter mode with nothing selected is not an export of everything: it is a
  // mode waiting for a predicate. Counting the unfiltered set there would
  // print a number beside a download that asks for a filter.
  if (draft.mode === 'filter' && !ocFilterActive(draft)) {
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

// ocExportReceiptText is the one receipt sentence. Both the full section
// render and the in-place painter read it, so the two cannot disagree about
// whether a count exists.
function ocExportReceiptText(draft) {
  if (draft.counting) return 'counting…';
  if (draft.error) return draft.error;
  if (draft.count == null) {
    return draft.mode === 'filter'
      ? 'choose a filter to preview the export'
      : 'counting stored records…';
  }
  return fmt(draft.count) + (draft.count === 1 ? ' record' : ' records') + ' · JSON, gzip';
}

// ocExportEnabled refuses a download of nothing, and refuses it while a count
// it depends on is missing or pending.
function ocExportEnabled(draft) {
  return !draft.counting && !draft.error && draft.count != null && draft.count > 0;
}

function ocUpdateExportReceipt() {
  const draft = ocDrafts.export;
  const el = ocEl('oc-export-receipt');
  const btn = ocEl('oc-export-download');
  if (!draft || !el) return;
  el.textContent = ocExportReceiptText(draft);
  el.dataset.tone = draft.error ? 'err' : '';
  if (btn) btn.disabled = !ocExportEnabled(draft);
}

function ocDownloadExport() {
  const draft = ocDrafts.export;
  if (!draft) return;
  // Both modes download through the predicate the receipt just counted, so
  // neither can claim a total the other would not produce.
  if (!ocExportEnabled(draft)) {
    ocScheduleExportCount();
    return;
  }
  triggerDownload(ocExportURL(ocExportFilter(draft)), '');
  const phrase = 'downloaded ' + fmt(draft.count) + (draft.count === 1 ? ' record' : ' records');
  ocRecordChange(phrase, null, 'ok');
  toast(phrase);
}
