// Operator export: the download builder, the shared purge predicate preview
// and the receipt. The record ring and the server's count endpoint are the
// only sources; this module never infers a count locally.

// ---------- export ----------
// The export offers the same history windows the delete filter does, so the
// tokens come from FILTER_AGES (chrome_menus.js loads first). Only the words
// are the export's own: "older than 1 week" names an exclusion and "last
// week" names an inclusion, and the same token means one of those depending
// on which side of the window the operator is choosing.
//
// A window added to FILTER_AGES without wording here keeps that list's own
// label rather than rendering `undefined`, so the two lists cannot disagree
// about which windows exist.
const OC_AGE_LABELS = { '': 'any time', '1h': 'older than 1 hour', '24h': 'older than 1 day', '168h': 'older than 1 week' };
const OC_AFTER_LABELS = { '': 'from the beginning', '1h': 'last hour', '24h': 'last day', '168h': 'last week' };
const ocHistoryChoices = labels =>
  FILTER_AGES.map(([token, fallback]) => [token, labels[token] || fallback]);
const OC_AGE_CHOICES = ocHistoryChoices(OC_AGE_LABELS);
const OC_AFTER_CHOICES = ocHistoryChoices(OC_AFTER_LABELS);

function ocBlankExportDraft() {
  return {
    mode: 'all', provider: '', model: '', client: '', conversation: '',
    status: '', errors: false, debug: false, errorType: '', age: '', after: '',
    count: null, counting: false, error: '', picker: null,
    // The instant the current count measured its relative ages from. The
    // download reuses it so the file and the receipt describe one window.
    epoch: 0,
  };
}

// ocExportFields is the predicate without its clock: the dimensions the
// operator chose. It is what changes when the filter changes, so it is also
// the right thing to compare when deciding whether a reply is still current.
// An age filter's absolute bounds move every millisecond, which made the
// staleness check below reject every reply and left the receipt reading
// "choose a filter to preview the export" forever.
function ocExportFields(draft) {
  if (draft.mode === 'all') {
    return {
      provider: '', model: '', client: '', conversation_id: '', error_type: '',
      status_code: 0, has_error: false, debug: false,
    };
  }
  return {
    provider: draft.provider || '',
    model: draft.model || '',
    client: draft.client || '',
    conversation_id: draft.conversation || '',
    error_type: draft.errorType || '',
    status_code: draft.status ? Number(draft.status) : 0,
    has_error: !!draft.errors,
    debug: !!draft.debug,
  };
}

// ocExportFilter is the one predicate owner for the preview and the download.
// All history means no predicate at all: the filter fields persist across a
// mode switch so returning to matching records restores the work, but they
// must not reach the predicate while the mode says all history. A mode chip
// that says one thing while the URL carries another is the export lying about
// what it will download.
// `epoch` is the instant the relative ages are measured from. The count names
// its epoch and the download reuses it, so the receipt and the file describe
// the same window instead of two instants a few milliseconds apart.
function ocExportFilter(draft, epoch) {
  const at = Number.isFinite(epoch) ? epoch : Date.now();
  const fields = ocExportFields(draft);
  if (draft.mode === 'all') return { ...fields, before_ms: 0, after_ms: 0 };
  return {
    ...fields,
    before_ms: draft.age ? at - parseGoDuration(draft.age) : 0,
    after_ms: draft.after ? at - parseGoDuration(draft.after) : 0,
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

// ocFilterActive asks whether the operator chose any predicate at all. It reads
// the predicate owner rather than listing the draft's fields a second time: a
// new filter field is then live in the "choose a filter" gate the moment it
// reaches ocExportFields, instead of leaving a mode that looks armed while the
// count refuses to run. The age fields carry a window, not a selector, so they
// are the two this reads from the draft directly.
function ocFilterActive(draft) {
  const fields = ocExportFields(draft);
  const chosen = Object.values(fields).some(value => value !== '' && value !== 0 && value !== false);
  return chosen || !!(draft.age || draft.after);
}

// ocFilterValues is one dimension's export vocabulary. The live ring alone is
// not enough: exporting history is the section's purpose, and after a restart
// the ring is empty while the server still knows every provider, model and
// client it has seen. Those come from the same server-owned lists the scope
// picker uses, so a quiet proxy can still filter by what actually ran.
//
// The dimension's own row supplies both names, so this is one shape for every
// selector instead of four branches that restated the record field and the
// known-values name per dimension. Status stays local: it is derived from a
// record's status code rather than matched by the server, so it has no row.
function ocFilterValues(dim) {
  const recs = (typeof lastData !== 'undefined' && lastData?.records) || [];
  const fromRing = pick => [...new Set(recs.map(pick).filter(Boolean))].sort();
  if (dim === 'status') {
    return fromRing(r => r.status_code > 0 ? r.status_code : '').sort((a, b) => a - b);
  }
  const row = ocScopeDim(dim);
  if (!row) return [];
  const merged = [...new Set([
    ...(typeof ocKnownValues === 'function' ? ocKnownValues(row.key) : []),
    ...fromRing(r => r[row.record]),
  ])];
  return merged.sort();
}

function ocFilterLabel(dim, value) {
  // A status code alone is ambiguous in a menu of numbers, so it carries the
  // word that says what it is. Every other value is a name the operator chose.
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
      // Same shape as a scope chip: one shrinking label box and a remove
      // button that keeps its own width, so a long value cannot push the
      // control out of the chip.
      ? `<span class="oc-cond-values"><span class="oc-chip" title="${escapeHtml(ocFilterLabel(dim, value))}">` +
        `<span class="oc-chip-text">${escapeHtml(ocFilterLabel(dim, value))}</span>` +
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
  // stop, so every value row is reachable by arrow only. That is why tabStop is
  // false here and true for the scope picker, which has no "any" row.
  const rows = ocPickRowsHTML(values, {
    role: 'menuitemradio', dim, attrs: 'data-oc-filter-value', tabStop: false,
    isOn: value => String(draft[dim] || '') === String(value),
    label: value => ocFilterLabel(dim, value),
  });
  const empty = values.length ? '' : `<div class="oc-pick-empty">none yet - they appear as requests arrive</div>`;
  // A selector dimension can be typed as well as picked, because the value may
  // predate this page's ring. A status code cannot: its vocabulary is the set of
  // codes actually seen.
  const custom = !!ocScopeDim(dim);
  // A typed value is matched exactly, which the conversation hint says because
  // an id is not a name the operator recognizes; every other dimension's hint is
  // its own word.
  const customHint = dim === 'conversation' ? 'exact conversation id' : dim + ' name';
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
      `<div class="oc-filter-row"><span class="oc-filter-name">errors</span>${ocChipRow(OC_CHOICES.exportErrors, ['any', 'only'], draft.errors ? 'only' : 'any', v => v)}</div>` +
      `<div class="oc-filter-row"><span class="oc-filter-name">debug</span>${ocChipRow(OC_CHOICES.exportDebug, ['any', 'only'], draft.debug ? 'only' : 'any', v => v)}</div>` +
      `</div>` +
      // Each age list is one of four labelled spans, so a select states the
      // choice in one line instead of wrapping four long chips.
      `<div class="oc-filter-pair">` +
      `<div class="oc-filter-row"><span class="oc-filter-name">newer than</span>${ocChoiceSelect(OC_CHOICES.exportAfter, OC_AFTER_CHOICES.map(([v]) => v), draft.after, v => OC_AFTER_CHOICES.find(([x]) => x === v)?.[1] || v)}</div>` +
      `<div class="oc-filter-row"><span class="oc-filter-name">older than</span>${ocChoiceSelect(OC_CHOICES.exportAge, OC_AGE_CHOICES.map(([v]) => v), draft.age, v => OC_AGE_CHOICES.find(([x]) => x === v)?.[1] || v)}</div>` +
      `</div>` +
      `<label class="oc-field wide" data-oc-key="error-type"><span class="oc-field-name">error type</span><input type="text" data-oc-export-field="errorType" value="${escapeHtml(draft.errorType)}" placeholder="exact provider error type"></label>` +
      `</div>`
    : '';
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><p class="oc-sub">Stream matching request records as a gzip JSON artifact. The preview and the download use the same predicate.</p></div>` +
    `<div class="oc-group"><div class="oc-label">what to download</div>` +
    ocChipRow(OC_CHOICES.exportMode, ['all', 'filter'], draft.mode, v => v === 'all' ? 'all history' : 'matching records') +
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
  // The epoch is frozen for the whole count. Comparing the full predicate
  // would compare two moving clocks: an age filter's absolute bounds advance
  // every millisecond, so every reply looked stale and the receipt never left
  // "choose a filter to preview the export". The dimensions the operator
  // chose are what decide whether a reply is still current.
  const epoch = Date.now();
  const token = JSON.stringify(ocExportFields(draft));
  draft.counting = true;
  ocUpdateExportReceipt();
  try {
    const count = await purgeCount(ocExportFilter(draft, epoch));
    if (ocDrafts.export !== draft || JSON.stringify(ocExportFields(draft)) !== token) return;
    draft.count = count;
    draft.epoch = epoch;
    draft.error = '';
  } catch (err) {
    if (ocDrafts.export !== draft) return;
    draft.count = null;
    draft.epoch = 0;
    draft.error = String(err.message || PURGE_COUNT_UNAVAILABLE);
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
  return countPhrase(draft.count, 'record') + ' · JSON, gzip';
}

// ocExportEnabled refuses a download of nothing, and refuses it while a count
// it depends on is missing or pending.
function ocExportEnabled(draft) {
  return !draft.counting && !draft.error && draft.count != null && draft.count > 0;
}

function ocUpdateExportReceipt() {
  const draft = ocDrafts.export;
  const el = $('oc-export-receipt');
  const btn = $('oc-export-download');
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
  // The receipt counted a window that starts at draft.epoch. Recomputing the
  // age bounds here would move both edges forward and download a slightly
  // different set than the one the operator was shown.
  triggerDownload(ocExportURL(ocExportFilter(draft, draft.epoch || Date.now())), '');
  // The artifact streams by design (no Content-Length, chunked framing), so the
  // browser owns the request and this code cannot see its status: a 500
  // retraction or a dead connection produces no signal here. Reporting
  // "downloaded N records" therefore asserted an outcome nobody observed, and
  // the ledger kept that claim after a failed transfer. The phrase states the
  // action that was taken; the browser's own download UI reports how it went.
  const phrase = 'downloading ' + countPhrase(draft.count, 'record');
  ocRecordChange(phrase, null, 'ok');
}
