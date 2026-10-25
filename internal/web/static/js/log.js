// ---- live request log: incremental DOM updates ----
// The request log must never shift under the user: new rows are appended as
// DOM nodes only (nothing existing is rebuilt), a row's DOM is replaced only
// when that one record actually changed (in-flight → finalized), and when the
// user has scrolled into the list, scrollTop is compensated for rows inserted
// above the viewport so the content under the cursor stays put.
// logHTML maps id → {rec, sep}: the record object reference enables O(1)
// change detection (replaced objects are always new from JSON.parse) so
// reqRow() is skipped entirely for rows whose record hasn't changed; sep is
// the row's day-divider label (its movement is part of the diff key). The row
// HTML is deliberately not retained: it is only read by the full rebuild that
// created it, so keeping it duplicated every loaded row's markup in memory.
const logHTML = new Map();

function updateLogStats(d) {
  const el = $('f-stats');
  if (!el || !lastData) return;
  if (!d) d = derive(lastData);
  const loaded = logWindowLength(d.scopedRecs);
  const shown = logHTML.size;
  if (shown && shown < loaded) el.textContent = fmt(shown) + ' of ' + fmt(loaded) + ' records';
  else el.textContent = fmt(loaded) + ' records';
  // The retry owner keeps trying on its own; the status line stays honest
  // until a page lands or the window resets.
  if (logFetchFailed) el.textContent += ' · history unavailable';
}

// logWindowSameSet reports whether logHTML's id set is exactly the visible
// window. The common steady-state frame is O(rows) map probes; only a real
// membership change pays one DOM traversal.
function logWindowSameSet(recent) {
  if (logHTML.size !== recent.length) return false;
  for (const r of recent) { if (!logHTML.has(r.id)) return false; }
  return true;
}
// dropStaleLogRows removes every row group not in the visible window and
// forgets its id. One traversal and one direct removal per stale node: the
// previous per-id rescanned the whole table for every stale row, making a
// scope change quadratic in the loaded window.
function dropStaleLogRows(container, recent) {
  const want = new Set(recent.map(r => r.id));
  const dropped = new Set();
  container.querySelectorAll('tr[data-id], tr[data-sep-of], tr.retry-sub[data-retry-of]').forEach(tr => {
    const id = tr.dataset.id || tr.dataset.sepOf || tr.dataset.retryOf;
    if (want.has(id)) return;
    dropped.add(id);
    tr.remove();
  });
  for (const id of dropped) logHTML.delete(id);
}

function renderRequestsIncremental(recs) {
  const container = $('tbl-requests');
  if (!container) return;
  const tbody = container.querySelector('tbody');
  const recent = logWindowRows(recs);
  if (!recent.length) { renderRequests(recs); return; }
  if (!tbody) { renderRequests(recs); return; } // empty-state placeholder in DOM: rebuild properly
  // A row replacement below removes whichever control owned keyboard focus.
  // Capture its record/control identity before any DOM write.
  const focus = captureFocus(container, rowFocusKey);
  // Drop rows that left the window (scope change or dashCfg.log_rows cap
  // overflow). The steady-state case is just map probes.
  if (!logWindowSameSet(recent)) dropStaleLogRows(container, recent);

  // Process newest→oldest, collecting each new or changed group. A new row is
  // positioned against its newer neighbor (which is already in the DOM), so a
  // grown window appends older rows below existing history while a live row
  // with no newer neighbor still lands at the top. Day-divider ownership is
  // part of the diff key: a boundary that appears or moves between passes
  // rebuilds exactly the two rows it touches.
  const seps = dayStartLabels(recent);
  const scrollAtTop = container.scrollTop;
  const ops = [];
  let leadingNew = true;
  for (let i = 0; i < recent.length; i++) {
    const r = recent[i];
    const sep = seps[i];
    const prev = logHTML.get(r.id);
    // O(1) change detection: record objects are replaced wholesale by upsert,
    // so identity compare catches every real change. A new JSON.parse always
    // creates a fresh object, so a replay that discards its duplicate never
    // reaches here (upsertRec returns false, the row is unchanged).
    if (!prev) {
      ops.push({ kind: 'insert', id: r.id, rec: r, sep, html: reqRow(r, sep), newer: i ? recent[i - 1] : null, above: leadingNew });
    } else {
      leadingNew = false;
      if (prev.rec !== r || prev.sep !== sep) {
        // The record itself changed (in-flight row finalized, retry badge grew),
        // or its day-divider position moved: replace only this row's group, in
        // place. Everything else, including any text the user is selecting, is
        // untouched.
        ops.push({ kind: 'replace', id: r.id, rec: r, sep, html: reqRow(r, sep) });
      }
    }
    // else: prev.rec === r && prev.sep === sep → unchanged, skip entirely.
  }
  if (!ops.length) return;

  // Learn any wider column before the rows land: the new floors then apply to
  // the same layout as the inserted content, and a later shorter value cannot
  // pull the column back. Numeric streaming churn skips the probe: the same
  // TTFT, token and tool values are re-measured when the row finalizes, before
  // its final paint. Identity, a live-pill transition or a new attempt is
  // measured immediately because that text can disappear before finalization.
  const widthRows = ops
    .filter(op => op.kind === 'insert' || reqWidthRelevant(logHTML.get(op.id)?.rec, op.rec))
    .map(op => op.html);
  if (reqMeasureRows(widthRows)) applyReqFloors(container);

  // Scroll anchor: the content under the cursor must not move. Every DOM
  // change above the first pre-existing row contributes to its displacement.
  // A grown window's older rows land below existing history and contribute
  // nothing. Measure all replaced rows in one read pass BEFORE any mutation,
  // apply all writes, then measure the results in one read pass: one or two
  // layouts for the whole frame instead of one forced layout per changed row.
  let before = null;
  if (scrollAtTop > 0) {
    before = new Map();
    for (const op of ops) {
      if (op.kind === 'replace') before.set(op.id, rowGroupHeight(container, op.id));
    }
  }
  for (const op of ops) {
    if (op.kind === 'insert') {
      insertLogGroupHTML(tbody, op.newer, op.html);
    } else {
      replaceRowGroupDOM(container, op.id, op.html);
    }
    logHTML.set(op.id, { rec: op.rec, sep: op.sep });
  }
  if (scrollAtTop > 0) {
    let delta = 0;
    for (const op of ops) {
      if (op.kind === 'replace') delta += rowGroupHeight(container, op.id) - before.get(op.id);
      else if (op.above) delta += rowGroupHeight(container, op.id);
    }
    if (delta !== 0) container.scrollTop = scrollAtTop + delta;
  }
  restoreFocus(focus);
}

// insertLogGroupHTML places a row group in visible (newest-first) order by
// inserting it after its newer neighbor's whole group (day divider, main row,
// attempt sub-rows). With no newer neighbor it is the newest row and goes to
// the top. Never append blindly: a live record that arrived while an older
// page was in flight is not automatically history.
function insertLogGroupHTML(tbody, newer, html) {
  const frag = trGroupNodesHTML(html);
  if (!newer || !newer.id) { tbody.insertBefore(frag, tbody.firstChild); return; }
  const id = CSS.escape(newer.id);
  const subs = tbody.querySelectorAll(`tr.retry-sub[data-retry-of="${id}"]`);
  let anchor = subs.length ? subs[subs.length - 1] : null;
  if (!anchor) anchor = tbody.querySelector(`tr.exp-row[data-id="${id}"]`);
  if (!anchor) anchor = tbody.querySelector(`tr.day-sep[data-sep-of="${id}"]`);
  if (anchor) anchor.after(frag);
  else tbody.insertBefore(frag, tbody.firstChild);
}

// rowGroupHeight sums the rendered heights of one record's whole group
// (divider + main + attempt sub-rows) - the unit of scroll-anchor accounting.
function rowGroupHeight(container, id) {
  let h = 0;
  container.querySelectorAll(`tr[data-id="${CSS.escape(id)}"], tr[data-sep-of="${CSS.escape(id)}"]`).forEach(n => { h += n.offsetHeight; });
  return h;
}

function appendOlderLogRows(recs) {
  const container = $('tbl-requests');
  if (!container) return;
  let tbody = container.querySelector('tbody');
  const recent = visibleLog(recs);
  if (!recent.length) { renderRequests(recs); return; }
  if (!tbody) { renderRequests(recs); return; }
  const focus = captureFocus(container, rowFocusKey);
  // A live row can take a slot while the page was in flight, pushing the
  // oldest loaded row out of the bounded window. Drop it like the live path.
  if (!logWindowSameSet(recent)) dropStaleLogRows(container, recent);
  const seps = dayStartLabels(recent);
  // Build new rows once so the width probe and the insertion share the same
  // HTML. Measuring before the first insert lets the browser lay the new rows
  // out under their final floors instead of widening after the fact.
  const fresh = new Map();
  for (let i = 0; i < recent.length; i++) {
    const r = recent[i];
    if (r && !logHTML.has(r.id)) fresh.set(r.id, reqRow(r, seps[i]));
  }
  if (fresh.size && reqMeasureRows([...fresh.values()])) applyReqFloors(container);
  const scrollAtTop = container.scrollTop;
  let before = null;
  if (scrollAtTop > 0) {
    before = new Map();
    for (let i = 0; i < recent.length; i++) {
      const r = recent[i];
      if (r && logHTML.has(r.id) && logHTML.get(r.id).sep !== seps[i]) {
        before.set(r.id, rowGroupHeight(container, r.id));
      }
    }
  }
  let leadingNew = true;
  const topInserts = [];
  for (let i = 0; i < recent.length; i++) {
    const r = recent[i];
    if (!r) continue;
    const sep = seps[i];
    const prev = logHTML.get(r.id);
    if (!prev) {
      insertLogGroupHTML(tbody, i ? recent[i - 1] : null, fresh.get(r.id));
      if (leadingNew) topInserts.push(r.id);
      logHTML.set(r.id, { rec: r, sep });
    } else {
      leadingNew = false;
      if (prev.sep !== sep) {
        // An older page can re-shape the day boundary of the previously-last
        // row; rebuild just that group in place.
        const html = reqRow(r, sep);
        replaceRowGroupDOM(container, r.id, html);
        logHTML.set(r.id, { rec: r, sep });
      }
    }
  }
  if (scrollAtTop > 0) {
    // A live row that raced the page can land above the viewport; compensate
    // it. Archive rows below existing history contribute nothing.
    let delta = 0;
    for (const id of topInserts) delta += rowGroupHeight(container, id);
    for (const [id, h] of before) delta += rowGroupHeight(container, id) - h;
    if (delta !== 0) container.scrollTop = scrollAtTop + delta;
  }
  restoreFocus(focus);
}

// renderRequests renders the live request log - the explorer's scoped
// drill-through. It receives the already-scoped record set (filter bar +
// explorer filters, applied by renderAll through the cached derive), merges any
// already-fetched archive pages, and paints the current window (newest
// logCap() rows). First paint is dash_log_rows; scrolling grows it.
// This is the full rebuild (navigation/first paint/full resync); live events
// use renderRequestsIncremental instead. It also seeds the logHTML map so the
// incremental path can diff against what is actually in the DOM.
function renderRequests(recs) {
  recs = mergeLogArchive(recs);
  const recent = visibleLog(recs);
  const container = $('tbl-requests');
  const focus = captureFocus(container, rowFocusKey);
  logHTML.clear();
  // Incremental/append mutate the tbody without going through updateSection,
  // so lastRender['tbl-requests'] is stale. A full rebuild must always paint
  // (nav / filter / resync), even when the window HTML matches first paint.
  delete lastRender['tbl-requests'];
  if (!recent.length) {
    updateSection('tbl-requests', '<div class="empty">no matching requests</div>');
    restoreFocus(focus);
    // An empty scope (its records predate the ring - a provider not used in a
    // while) must still probe the store: nothing will ever scroll here, so the
    // fill probe below is the only path that can pull the scope's durable rows.
    scheduleLogFill();
    return;
  }
  const seps = dayStartLabels(recent);
  const rows = new Array(recent.length);
  for (let i = 0; i < recent.length; i++) {
    const r = recent[i];
    rows[i] = reqRow(r, seps[i]);
    logHTML.set(r.id, { rec: r, sep: seps[i] });
  }
  // Learn the window's natural column widths before the table paints, so the
  // first paint already carries every floor this rebuild needs.
  reqMeasureRows(rows);
  updateSection('tbl-requests', `<table>${reqThead()}<tbody>${rows.join('')}</tbody></table>`);
  restoreFocus(focus);
  scheduleLogFill();
}

// Delegated row clicks - no inline onclick interpolating the record id.
$('tbl-requests').addEventListener('click', e => {
  // A pivot/entity link navigates via the hash router - never open the drawer.
  if (e.target.closest('a.ent-link')) return;
  // A state pill edits its matching operator hold/session/limit (the same
  // form as clicking the hold row in the header menu): one table keyed by
  // the pill's data-edit vocabulary, one branch.
  const pillEdits = [
    ['.pill.paused[data-edit-pause]', editHoldForRecord],
    ['.pill.debug[data-edit-debug]', editDebugForRecord],
    ['.pill.throttled[data-edit-limit]', editLimitForRecord],
  ];
  for (const [sel, edit] of pillEdits) {
    if (!e.target.closest(sel)) continue;
    e.stopPropagation();
    const tr = e.target.closest('.exp-row[data-id]');
    if (tr) edit(recordById(tr.dataset.id));
    return;
  }
  // The retry badge toggles the absorbed-attempt sub-rows (collapsed default).
  const tog = e.target.closest('.retry-toggle[data-retry-for]');
  if (tog) {
    const id = tog.dataset.retryFor;
    const open = tog.getAttribute('aria-expanded') === 'true';
    tog.setAttribute('aria-expanded', String(!open));
    tog.querySelector('.retry-caret').textContent = open ? '▸' : '▾';
    document.querySelectorAll(`tr.retry-sub[data-retry-of="${CSS.escape(id)}"]`).forEach(tr => { tr.hidden = open; });
    return;
  }
  // A request row opens its detail drawer. Retry sub-rows carry the parent's
  // data-id plus data-attempt (the attempt index), so clicking one opens
  // that exact attempt inside the request's drawer; the request's own row
  // (no data-attempt) opens the request detail.
  const tr = e.target.closest('.exp-row[data-id]');
  if (tr && tr.dataset.attempt !== undefined) openDrawerAttempt(tr.dataset.id, tr.dataset.attempt);
  else if (tr) openDrawer(tr.dataset.id);
});
