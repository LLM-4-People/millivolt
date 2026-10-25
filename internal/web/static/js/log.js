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
// Durable pages loaded below the ring are a convenience window, not a second
// unlimited history. Twenty thousand records bounds this page's retained JSON
// at a few megabytes while still covering deep operator scroll sessions; the
// server keyset cursor remains valid, so fetching continues below the trim.
// logArchive is ascending oldest-to-newest, so the cap drops from the oldest
// end: the newest archive rows stay adjacent to the live ring and the retained
// window is the newest bounded archive window.
const LOG_ARCHIVE_MAX = 20000;
function logPageSize() { return dashCfg.log_rows; }
function logCap() { return logLimit > 0 ? logLimit : logPageSize(); }
function resetLogWindow() {
  logLimit = 0;
  const hadArchive = logArchive.length > 0;
  logArchiveClear();
  if (hadArchive) pruneModelNames();
  logFetching = false;
  logExhausted = false;
  logCursor = null;
  logFailures = 0;
  logFetchFailed = false;
  clearLogRetry();
  logFetchGen++;
  const box = $('tbl-requests');
  // An untouched/empty log is already at zero. Even assigning zero forces
  // layout, so first paint must not measure the empty shell just to reset it.
  if (box && logHTML.size) box.scrollTop = 0; // newest-first on navigation/full resync
}
// ---- archive identity and the merged log window ----
// The durable archive is a keyset-ordered page cache below the live ring, not
// a second ring: the merge prepends its rows oldest-first and lets the live
// ring win duplicate ids. Rebuilding that merge from the full 20k archive on
// every live frame was the dominant log cost, so the archive owns one
// persistent identity set with a monotonic revision that advances only when
// the array actually changes. Live frames reconcile ring membership
// incrementally and materialize at most the visible cap.
//
// Archive mutation owners: resetLogWindow clears through logArchiveClear and
// fetchOlderLog prepends through logArchivePrepend. Every write must go through
// one of those owners, or replace the array / change its length (tests, future
// callers); either is observed by the identity check and rebuilt once with the
// same keyset and duplicate-suppression semantics. Replacing an element in
// place at an unchanged length is unsupported: the identity cache keys on the
// array object and its length, so that edit alone is not observed.
let _logArchiveIndex = null;
let _logArchiveSeq = 0;
// Archive traversals are the G6 regression guard: the identity rebuild below
// and the full materialized merge are the only owners allowed to walk the
// archive, and steady-state live frames use neither.
let _logArchiveScans = 0;
function logArchiveInvalidate() {
  _logArchiveIndex = null;
  _logLiveIndex = null;
  _logMergeMemo = null;
}
function logArchiveIndex() {
  const archive = logArchive;
  const current = _logArchiveIndex;
  if (current && current.archive === archive && current.length === archive.length) return current;
  _logArchiveScans++;
  const ids = new Set();
  let unique = 0;
  let duplicates = false;
  for (const r of archive) {
    if (!r || !r.id) continue;
    if (ids.has(r.id)) { duplicates = true; continue; }
    ids.add(r.id);
    unique++;
  }
  _logArchiveIndex = {archive, length: archive.length, ids, unique, duplicates, rev: ++_logArchiveSeq};
  _logLiveIndex = null;
  _logMergeMemo = null;
  return _logArchiveIndex;
}
function logArchiveClear() {
  if (logArchive.length) logArchive = [];
  logArchiveInvalidate();
}
function logArchivePrepend(added) {
  logArchive = added.reverse().concat(logArchive);
  // Ascending oldest-to-newest: trim the oldest rows so the newest archive
  // window stays contiguous with the live ring at the top of the merge.
  const excess = logArchive.length - LOG_ARCHIVE_MAX;
  if (excess > 0) logArchive.splice(0, excess);
  logArchiveInvalidate();
}

// Live membership is the only ring fact the archive merge depends on: a live
// id shadows its archived copy, while a content-only row replacement changes
// no id. recordsRevision (live.js) advances on every record mutation; the
// array identity, length and endpoint ids distinguish a content replacement
// from an append/evict. The persistent id set updates only the appended tail
// on growth and rebuilds on eviction or a new scope array.
let _logLiveIndex = null;
function buildLogLive(ring, archive, rev) {
  const ids = new Set();
  for (const r of ring) { if (r && r.id) ids.add(r.id); }
  let shadow = 0;
  if (archive.ids.size) {
    for (const id of ids) { if (archive.ids.has(id)) shadow++; }
  }
  return {ring, archive, rev, ids, shadow, ringLength: ring.length,
    length: ring.length + archive.unique - shadow,
    first: ring.length ? ring[0]?.id : undefined,
    last: ring.length ? ring[ring.length - 1]?.id : undefined};
}
function logLiveIndex(ring) {
  const archive = logArchiveIndex();
  const prev = _logLiveIndex;
  const rev = recordsRevision;
  if (prev && prev.archive === archive && prev.ring === ring && prev.rev === rev) return prev;
  const first = ring.length ? ring[0]?.id : undefined;
  const last = ring.length ? ring[ring.length - 1]?.id : undefined;
  let next;
  if (prev && prev.archive === archive && prev.ring === ring && prev.ringLength > 0 &&
      ring.length > prev.ringLength && first === prev.first) {
    // A grown ring only appends: eviction always brings the length back down.
    // Reconcile exactly the appended ids and their archive shadowing.
    const ids = prev.ids;
    let shadow = prev.shadow;
    for (let i = prev.ringLength; i < ring.length; i++) {
      const r = ring[i];
      if (!r || !r.id || ids.has(r.id)) continue;
      ids.add(r.id);
      if (archive.ids.has(r.id)) shadow++;
    }
    next = {ring, archive, rev, ids, shadow, ringLength: ring.length,
      length: ring.length + archive.unique - shadow, first, last};
  } else if (prev && prev.archive === archive && ring.length === prev.ringLength &&
      first === prev.first && last === prev.last &&
      (prev.ring === ring || prev.rev === rev)) {
    // A replaced record keeps the same array identity and membership. A new
    // scoped array with an unchanged record revision is a counter-only
    // re-derivation of the same scope, so its membership is unchanged too.
    next = {...prev, ring, rev, first, last};
  } else {
    next = buildLogLive(ring, archive, rev);
  }
  _logLiveIndex = next;
  return next;
}
// One-entry memo for the full materialized merge (full rebuild, page landing,
// drawer navigation, fill probing). Counter-only frames never reach it, and a
// record revision alone no longer invalidates it on the live render path.
let _logMergeMemo = null;
function mergeLogArchive(scopedRing) {
  if (!logArchive.length) { _logMergeMemo = null; return scopedRing; }
  const archive = logArchiveIndex();
  const memo = _logMergeMemo;
  if (memo && memo.archive === archive && memo.rev === recordsRevision &&
      (memo.ring === scopedRing || memo.merged === scopedRing)) {
    return memo.merged;
  }
  const live = logLiveIndex(scopedRing);
  const extra = [];
  _logArchiveScans++; // one full traversal for this materialized merge
  if (live.archive.duplicates) {
    const seen = new Set();
    for (const r of logArchive) {
      if (!r || !r.id || live.ids.has(r.id) || seen.has(r.id)) continue;
      seen.add(r.id);
      extra.push(r);
    }
  } else {
    for (const r of logArchive) {
      if (r && r.id && !live.ids.has(r.id)) extra.push(r);
    }
  }
  // Durable pages retain their server keyset order and append below the live
  // arrival-ordered ring. Do not sort live rows or derive an archive cutoff
  // from them: a late completion can have a much older start than its peers.
  const merged = extra.concat(scopedRing);
  _logMergeMemo = {archive, rev: recordsRevision, ring: scopedRing, merged};
  return merged;
}
function visibleLog(recs) {
  const n = Math.min(recs.length, logCap());
  return recs.slice(-n).reverse();
}
// logWindowLength and logWindowRows are the live-frame owner: the count is
// O(1) after membership reconciliation, the rows materialize only the visible
// cap (usually the live-ring tail, otherwise min(cap, ring) plus the newest
// unshadowed archive rows).
function logWindowLength(ring) {
  if (!logArchive.length) return ring.length;
  return logLiveIndex(ring).length;
}
function logWindowRows(ring) {
  const cap = logCap();
  if (!logArchive.length) {
    const n = Math.min(ring.length, cap);
    const rows = new Array(n);
    for (let i = 0; i < n; i++) rows[i] = ring[ring.length - 1 - i];
    return rows;
  }
  const live = logLiveIndex(ring);
  if (live.archive.duplicates) return visibleLog(mergeLogArchive(ring));
  const rows = [];
  for (let i = ring.length - 1; i >= 0 && rows.length < cap; i--) {
    if (ring[i]) rows.push(ring[i]);
  }
  for (let i = logArchive.length - 1; i >= 0 && rows.length < cap; i--) {
    const r = logArchive[i];
    if (r && r.id && !live.ids.has(r.id)) rows.push(r);
  }
  return rows;
}
function logVisible() {
  if (!lastData) return [];
  return logWindowRows(derive(lastData).scopedRecs);
}
function recStartMs(r) {
  if (!r || !r.start) return 0;
  const t = new Date(r.start).getTime();
  return Number.isFinite(t) ? t : 0;
}

// ---- day dividers ----
// The request log nests by local calendar day: a full-width divider rides
// above the first row of each day, styled as a major graduation on the
// instrument scale (the same feathered tick strip as the header/footer rails)
// and pins below the sticky header while that day is in view. Ownership lives
// with the row that opens the day (data-sep-of), so the per-id diff machinery
// inserts, moves, and removes dividers exactly like any other row group -
// nothing ever re-renders wholesale for a boundary.

// dayKey buckets a record's start into its local calendar day. One owner for
// both the boundary flags and (indirectly) the label. The key is cached per
// record object: a live paint re-walks the whole visible window, and rebuilding
// a Date plus a key string per row was the incremental pass's dominant cost
// after the user scrolled deep. Records are replaced wholesale on change, so
// the cache never goes stale.
const dayKeyCache = new WeakMap();
function dayKey(r) {
  let k = dayKeyCache.get(r);
  if (k !== undefined) return k;
  const t = recStartMs(r);
  if (!t) { dayKeyCache.set(r, ''); return ''; }
  const d = new Date(t);
  k = d.getFullYear() + '-' + d.getMonth() + '-' + d.getDate();
  dayKeyCache.set(r, k);
  return k;
}

// dayDividerLabel renders the day's stamp: today / yesterday for the fresh
// groups, else weekday · month day, with the year only when it differs from
// the current one.
const DAY_NAMES = ['SUN', 'MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT'];
const MONTH_NAMES = ['JAN', 'FEB', 'MAR', 'APR', 'MAY', 'JUN', 'JUL', 'AUG', 'SEP', 'OCT', 'NOV', 'DEC'];
function dayDividerLabel(r) {
  const t = recStartMs(r);
  if (!t) return '';
  const d = new Date(t);
  const now = new Date();
  const sameDay = (a, b) => a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
  if (sameDay(d, now)) return 'TODAY';
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);
  if (sameDay(d, yesterday)) return 'YESTERDAY';
  let label = DAY_NAMES[d.getDay()] + ' · ' + MONTH_NAMES[d.getMonth()] + ' ' + d.getDate();
  if (d.getFullYear() !== now.getFullYear()) label += ' ' + d.getFullYear();
  return label;
}

// dayStartLabels(recent) - one divider label per visible row (newest-first):
// the first visible row always opens its day (it is the top of the log and
// carries the sticky current-day context), every later row opens a day only
// when its neighbor above is a different one.
function dayStartLabels(recent) {
  const out = new Array(recent.length);
  let prevKey = null;
  for (let i = 0; i < recent.length; i++) {
    const k = dayKey(recent[i]);
    out[i] = (i === 0 || k !== prevKey) ? dayDividerLabel(recent[i]) : '';
    prevKey = k;
  }
  return out;
}

function daySepHTML(id, label) {
  if (!label) return '';
  return `<tr class="day-sep" data-sep-of="${escapeHtml(id)}"><td colspan="${REQ_COLS}"><div class="day-line"><span class="day-chip">${escapeHtml(label)}</span><span class="day-rule" aria-hidden="true"></span></div></td></tr>`;
}

// replaceRowGroupDOM swaps one row's group in place (divider + main + attempt
// sub-rows). Old nodes are captured before the insert and removed after: the
// replacement divider carries the same data-sep-of, so a post-insert removal
// by selector would delete the new one too.
function replaceRowGroupDOM(container, id, html) {
  const main = container.querySelector(`tr.exp-row[data-id="${CSS.escape(id)}"]`);
  if (!main) return false;
  const oldSubs = container.querySelectorAll(`tr.retry-sub[data-retry-of="${CSS.escape(id)}"]`);
  const oldSeps = container.querySelectorAll(`tr[data-sep-of="${CSS.escape(id)}"]`);
  const frag = trGroupNodesHTML(html);
  main.parentNode.insertBefore(frag, main);
  main.remove();
  oldSubs.forEach(s => s.remove());
  oldSeps.forEach(s => s.remove());
  return true;
}

// rowFocusKey maps a focused control inside one request row to its stable
// selector: the record id plus the control's own identity. Used by the
// render paths to keep keyboard focus on the equivalent control after a row
// group is replaced.
function rowFocusKey(el) {
  const tr = el.closest('tr.exp-row[data-id]');
  const id = tr && tr.dataset.id;
  if (!id) return null;
  const row = `tr.exp-row[data-id="${CSS.escape(id)}"] `;
  if (el.hasAttribute('data-open-req')) return row + 'button[data-open-req]';
  if (el.hasAttribute('data-retry-for')) return row + 'button[data-retry-for]';
  if (el.matches('a.ent-link[href]')) return row + `a.ent-link[href="${CSS.escape(el.getAttribute('href'))}"]`;
  return null;
}

function trGroupNodesHTML(html) {
  const t = document.createElement('template');
  t.innerHTML = html;
  return t.content;
}
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

function logNearBottom() {
  const c = $('tbl-requests');
  if (!c) return false;
  // Not laid out yet (jsdom, hidden): never auto-fetch - a 0×0 box would
  // look like "doesn't fill the viewport" and dump the whole history.
  if (c.clientHeight <= 0) return false;
  const room = c.scrollHeight - c.scrollTop - c.clientHeight;
  if (c.scrollHeight <= c.clientHeight + logNearSlackPx) return true;
  return room <= logNearPx;
}

function maybeLoadMoreLog() {
  if (!lastData || logFetching) return;
  const c = $('tbl-requests');
  if (!c) return;
  if (!logNearBottom()) return;
  const d = derive(lastData);
  const recs = mergeLogArchive(d.scopedRecs);
  const page = logPageSize();
  const cap = logCap();
  if (cap < recs.length) {
    logLimit = Math.min(recs.length, cap + page);
    appendOlderLogRows(recs);
    updateLogStats(d);
    scheduleLogFill();
    return;
  }
  if (logExhausted) return;
  fetchOlderLog();
}

// scheduleLogRetry is the one delayed retry owner for a failed older-page
// fetch. The generation check makes a scope/window reset unable to receive a
// stale retry; the consecutive-failure budget stops the automatic path after
// LOG_RETRY_LIMIT attempts, where a user scroll may try again.
function scheduleLogRetry(gen) {
  if (logRetryTimer != null) return;
  logRetryTimer = setTimeout(() => {
    logRetryTimer = null;
    if (gen !== logFetchGen || logFetching || logExhausted || !lastData) return;
    fetchOlderLog();
  }, LOG_RETRY_MS);
}

function fetchOlderLog() {
  if (logFetching || logExhausted) return;
  clearLogRetry(); // a manual fill owns the next attempt, not a stale timer
  logFetching = true;
  const gen = logFetchGen;
  const before = logCursor;
  const cursorQS = before ? '&before_ms=' + encodeURIComponent(String(before.ms)) + '&before_id=' + encodeURIComponent(before.id) : '';
  const q = 'limit=' + encodeURIComponent(String(logPageSize())) + cursorQS + (scopeQS() ? '&' + scopeQS() : '');
  operatorFetch('/metrics/agg/log?' + q).then(r => {
    if (!r.ok) throw new Error('log page');
    return r.json();
  }).then(p => {
    if (gen !== logFetchGen) return; // scope/window reset mid-flight: drop the stale page
    logFetching = false;
    if (!applyModelCanon(p?.model_canon)) return;
    logFailures = 0;
    logFetchFailed = false;
    // Presence is explicit: cursor_id may be the empty string, which is a real
    // boundary key. `next` must exist for that row even though its own row is
    // not rendered, or pagination stops and every older page is dropped.
    const next = p && Number.isSafeInteger(p.cursor_ms) && typeof p.cursor_id === 'string'
      ? {ms: p.cursor_ms, id: p.cursor_id} : null;
    const advanced = next && (!before || next.ms < before.ms || next.ms === before.ms && next.id !== before.id);
    if (next) logCursor = next;
    // An invalid/repeated cursor must not tight-loop on an identical page.
    logExhausted = !p || p.more !== true || !advanced;
    const incoming = Array.isArray(p && p.records) ? p.records : [];
    const ringScoped = derive(lastData).scopedRecs;
    const recs = mergeLogArchive(ringScoped);
    const seen = new Set();
    for (const r of recs) { if (r && r.id) seen.add(r.id); }
    const added = [];
    for (const r of incoming) {
      if (!r || !r.id || seen.has(r.id)) continue;
      seen.add(r.id);
      added.push(r);
    }
    // At the cap the retained window is the newest LOG_ARCHIVE_MAX rows. A
    // page fetched below that window cannot be retained: prepending then
    // trimming it would only grow logLimit for rows that never stay and
    // re-arm the fill owner forever. Treat the cap as terminal here, at the
    // one cursor/fill owner, so scroll and delayed fills stop too.
    const oldestRetained = logArchive[0];
    const outsideRetained = r => {
      if (!oldestRetained) return false;
      const rm = recStartMs(r), om = recStartMs(oldestRetained);
      return rm < om || (rm === om && String(r.id) < String(oldestRetained.id));
    };
    const capTerminal = logArchive.length >= LOG_ARCHIVE_MAX &&
      (added.length === 0 || added.every(outsideRetained));
    if (capTerminal) {
      logExhausted = true;
    } else if (added.length) {
      logArchivePrepend(added);
      const merged = mergeLogArchive(ringScoped);
      // The fill owner is the only expansion trigger, and it grows in page
      // units. Reserve that page as capacity: a live arrival then lands inside
      // the bounded budget instead of both growing the window per record and
      // evicting the history just loaded.
      logLimit = Math.max(logCap() + logPageSize(), logPageSize());
      appendOlderLogRows(merged);
    }
    // A successful retry clears "history unavailable" even when the page was a
    // duplicate-only reply with nothing new to append.
    updateLogStats();
    if (!logExhausted) scheduleLogFill();
  }).catch(() => {
    if (gen !== logFetchGen) return; // reset already cleared the retry state
    logFetching = false;
    logFailures++;
    logFetchFailed = true;
    updateLogStats();
    if (logFailures <= LOG_RETRY_LIMIT) scheduleLogRetry(gen);
  });
}

// One fill probe per frame, whether triggered by first paint, scrolling, or
// an archive append. Never measure a new table from a boot microtask or grow
// several pages in a synchronous write/read loop. The probe reads current
// scope/window state, so a queued old-scope probe cannot fetch stale rows.
let _logFillFrame = 0;
function scheduleLogFill() {
  if (_logFillFrame) return;
  _logFillFrame = requestAnimationFrame(() => {
    _logFillFrame = 0;
    maybeLoadMoreLog();
  });
}
$('tbl-requests')?.addEventListener('scroll', scheduleLogFill, {passive: true});

// turnsCell renders the turns column: the user→assistant[→tool] count triple,
// then the direction the model is responding (the last turn's role, colored by
// entity type) and a media badge when the request carries images/attachments.
// So the log row alone tells you the request's shape and sense at a glance.
// Turn-role colors reference the ENTITY_TYPES registry (single palette owner) so
// a palette edit can't desync the log's turn arrows from the entity colors.
const TURN_ROLE_COLOR = {
  user: ENTITY_TYPES.client.color, assistant: ENTITY_TYPES.model.color,
  tool: ENTITY_TYPES.tool.color, function: ENTITY_TYPES.tool.color,
  system: ENTITY_TYPES.status.color, developer: ENTITY_TYPES.status.color,
};
function turnsCell(r) {
  const counts = (r.turns_user||0) + '→' + (r.turns_assistant||0) + (r.turns_tool > 0 ? '→'+(r.turns_tool) : '');
  const role = r.last_turn_role || '';
  const rc = TURN_ROLE_COLOR[role] || 'var(--muted)';
  const dir = role ? ` <span style="color:${rc}" title="responding to a ${escapeHtml(role)} turn">⇢${escapeHtml(role.slice(0,4))}</span>` : '';
  const media = (r.images || r.attachments)
    ? ` <span style="color:var(--cyan)" title="${r.images||0} image${(r.images||0)!==1?'s':''}, ${r.attachments||0} attachment${(r.attachments||0)!==1?'s':''}">${[r.images?r.images+'▣':'', r.attachments?r.attachments+'⎘':''].filter(Boolean).join(' ')}</span>` : '';
  return counts + dir + media;
}

// turnsTitle is the full-value tooltip for the turns cell. The compact cell
// can clip a long triple or its media badge, so the counts and direction stay
// recoverable on hover and for assistive technology.
function turnsTitle(r) {
  const tool = r.turns_tool > 0 ? `, ${r.turns_tool} tool` : '';
  const role = r.last_turn_role ? `; responding to the ${r.last_turn_role} turn` : '';
  const media = (r.images || r.attachments)
    ? `; ${r.images||0} image${(r.images||0)!==1?'s':''}, ${r.attachments||0} attachment${(r.attachments||0)!==1?'s':''}`
    : '';
  return `${r.turns_user||0} user, ${r.turns_assistant||0} assistant${tool}${role}${media}`;
}

// debugBadge renders the dbg pill marking a request captured by a debug
// session; liveStatusCell and finalizedStatusPill both concatenate it, so it
// appears in the log rows and the drawer's status row alike. In the log row
// the pill opens the capture's session for editing.
function debugBadge(r) {
  if (!r || !r.debug) return '';
  return ` <span class="pill debug" data-edit-debug title="full debug capture - click to edit the session">dbg</span>`;
}

// liveStatusCell is the one owner of the in-flight status pill (log row +
// drawer). Classification is statusClass(); the retry badge stays outside so
// reqRow can concatenate it - the drawer's status row carries no retry badge,
// only the debug pill above. Drawer kv uses this helper directly.
function liveStatusCell(r) {
  const sc = statusClass(r);
  if (sc === 'paused') {
    return `<span class="pill paused" data-edit-pause title="held by operator pause - click to edit the hold">paused</span>` + debugBadge(r);
  }
  if (sc === 'throttled') {
    return `<span class="pill throttled" data-edit-limit title="queued on a provider limit - click to edit">throttled</span>` + debugBadge(r);
  }
  const title = sc === 'streaming' ? 'request in flight - streaming' : 'request in flight - pending';
  return `<span class="pill live" title="${title}">${sc}</span>` + debugBadge(r);
}

// finalizedStatusPill is the one owner of the finalized status pill (log row
// + drawer). A record that saw an upstream failure is flagged even when its
// final outcome is a 200 - an absorbed 5xx or an in-band error payload inside
// a 200. The pill still shows the final status; red + the error type reflect
// the failure (hover carries the provider's message). A client-disconnected
// record with no error is its own gray 'cancel' pill; a 499 carrying the
// provider's in-band error renders red like its 200 twin (recordIsError),
// keeping the ✕ marker for the plain cancellation only. Retry badges stay
// on the log row only.
function finalizedStatusPill(r) {
  const isErr = recordIsError(r);
  const isCancel = !isErr && !!r.client_disconnected;
  const sc = isErr ? 'err' : (isCancel ? 'cancel' : (r.status_code >= 400 ? 'warn' : 'ok'));
  const errTitle = r.error_type ? ` title="${escapeHtml(r.error_type)}${r.error_msg ? ': ' + escapeHtml(r.error_msg) : ''}"` : '';
  const cancelTitle = isCancel ? ' title="client disconnected - cancelled"' : '';
  return `<span class="pill ${sc}"${errTitle || cancelTitle}>${isCancel ? '✕' : ''}${r.status_code}</span>` + debugBadge(r);
}

// attemptPillClass is the one class computation for an absorbed
// attempt's status pill, shared by the drawer's retry list, the attempt
// view and the log sub-row (each site keeps its own text spelling): a
// transport failure and a 5xx are err, the statuses between are warn -
// a degenerate 2xx/3xx failure wears the same class on every surface.
function attemptPillClass(a) {
  return !a.status_code || a.status_code >= 500 ? 'err' : 'warn';
}

// attemptPill is the one status pill for an absorbed attempt, shared by the
// drawer's retry list and the attempt view. Transport failures have no HTTP
// status (0) - a meaningless "0" becomes the transport pill; HTTP failures
// show their real status.
function attemptPill(a) {
  if (!a.status_code) return `<span class="pill ${attemptPillClass(a)}">transport</span>`;
  return `<span class="pill ${attemptPillClass(a)}">${a.status_code}</span>`;
}

// reqRow renders one request row for the flat live request log, plus the
// day-divider row when this row opens a new local calendar day within the
// visible window (sepLabel carries that day's stamp).
// modelCell renders the log row's model entity under its canonical name -
// the leaf reads grouped like every other surface (clicking it pivots the
// explorer to the canonical group, which the scope mirror matches against
// raw spellings). The original spelling stays one hover away (title) and
// in full view in the request detail (Parameters ▸ model).
function modelCell(r) {
  const raw = String((r && r.model) || '').trim();
  const canon = canonicalModel(raw);
  const piv = entityPivot('model', canon, null, null,
    raw && canon !== raw ? `original: ${raw}` : '');
  return `<span class="req-model">${piv}</span>`;
}

// reqIdentityText is the one full-value identity string for a request, in the
// request table's column order. The identity cell carries it as its hover
// title (the content-sized column shows the whole value; the title keeps it
// recoverable when the card is horizontally scrolled), and the drawer title
// reads the same string, so the two surfaces cannot disagree.
function reqIdentityText(r) {
  return `${clientLabel(r && r.client) || '?'} · ${(r && r.provider) || '?'} · ${canonicalModel(r && r.model) || '-'}`;
}

// retryAttempts returns the absorbed retry attempts paired with their index in
// the record's attempts array. An in-band entry records the upstream status a
// committed keepalive 200 could not relay; it is health evidence, not a retry,
// so every retry disclosure excludes it while data-attempt keeps the original
// index for the attempt drawer.
function retryAttempts(r) {
  const out = [];
  (r && r.attempts ? r.attempts : []).forEach((a, index) => {
    if (!a.in_band) out.push({ a, index });
  });
  return out;
}

// retryOrdinal maps an attempts-array index to its 1-based position among the
// retry attempts, so a hidden in-band entry never shifts "attempt N of M".
function retryOrdinal(r, index) {
  const pos = retryAttempts(r).findIndex(entry => entry.index === index);
  return pos < 0 ? index + 1 : pos + 1;
}

function reqRow(r, sepLabel) {
  const sep = daySepHTML(r.id, sepLabel);
  // In-flight (a "begin" lifecycle record): not yet finalized - no status_code
  // or end. Render a live streaming row; it updates in place until the "end"
  // event lands (same id) with the finalized values.
  if (r.live) {
    const inTok = r.usage?.input_tokens || 0;
    // Live retry visibility: absorbed attempts arrive via "update" events while
    // the request is still in flight, so the row shows a retry badge as they
    // happen (before the final outcome). Hover lists the attempt errors so far.
    const liveAttempts = retryAttempts(r);
    const liveRetry = liveAttempts.length
      ? ` <span class="pill warn" title="${liveAttempts.length} retr${liveAttempts.length>1?'ies':'y'} so far (in flight): ${escapeHtml(liveAttempts.map(({ a }) => a.error_type || (a.status_code ? 'http '+a.status_code : 'transport')).join(', '))}">↻${liveAttempts.length}</span>`
      : '';
    // Input tokens may already carry a real estimate while a request streams
    // (cursor's parked turn reports an estimated prompt), but a plain 0 is
    // "not counted yet", never a real number - show a bare … like every
    // other not-yet-known cell.
    return `<tr class="exp-row live-row" data-id="${escapeHtml(r.id)}">
      <td style="color:var(--muted)" title="${escapeHtml(new Date(r.start).toLocaleString())}">${new Date(r.start).toLocaleTimeString()}</td>
      <td title="${escapeHtml(reqIdentityText(r))}"><span class="req-ident">${entityPivot('client', r.client, null, r)} ${entityPivot('provider', r.provider)} ${modelCell(r)}</span></td>
      <td>${liveStatusCell(r)}${liveRetry}</td>
      <td title="${r.ttft_ms ? escapeHtml(fmtDur(r.ttft_ms)) : ''}">${r.ttft_ms ? fmtDur(r.ttft_ms) : '…'}</td>
      <td>-</td>
      <td>${inTok ? fmt(inTok) + '/…' : '…'}</td>
      <td>-</td>
      <td style="color:var(--cyan)" title="${r.tool_calls ? escapeHtml(String(r.tool_calls)) + ' tool calls' : ''}">${r.tool_calls||'-'}</td>
      <td>-</td>
      <td style="color:var(--muted)">…</td>
      <td style="color:var(--muted)" title="${escapeHtml(turnsTitle(r))}">${turnsCell(r)}</td>
      <td>-</td>
      <td>-</td>
      <td><button type="button" class="hold-edit" data-open-req="${escapeHtml(r.id)}" aria-label="Open request details">›</button></td>
    </tr>`;
  }
  // Retry badge: show the actual status that was retried (not a hardcoded
  // 429) - rate_limited means 429/503, otherwise it was a transient 5xx. When
  // the request has absorbed attempts, the badge is a toggle that expands one
  // sub-row per attempt (collapsed by default).
  const attempts = retryAttempts(r);
  const retryBadge = attempts.length
    ? ` <button type="button" class="pill warn retry-toggle" data-retry-for="${escapeHtml(r.id)}" aria-expanded="false" title="${attempts.length} transparent retr${attempts.length>1?'ies':'y'} absorbed before this response - click to expand">↻${attempts.length} <span class="retry-caret">▸</span></button>`
    : (r.retries ? ` <span class="pill warn" title="${r.retries} retries">↻${r.retries}</span>` : '');
  // Cache as a percentage of input tokens (raw count stays in the drawer).
  const inTok = r.usage?.input_tokens || 0, cacheTok = r.usage?.cache_read_tokens || 0;
  const cachePct = inTok > 0 ? pct2(cacheTok, inTok) : null;
  // One owner for each displayed cell and its recoverable tooltip. The fixed
  // column widths can ellipsize a long value, so the cell title carries the
  // same text (or the exact counts behind a compact one) for hover and
  // assistive technology.
  const start = new Date(r.start);
  const timeText = start.toLocaleTimeString();
  const tpsText = r.gen_tps ? r.gen_tps.toFixed(1) : '-';
  const inOutText = `${fmt(r.usage?.input_tokens)}/${fmt(r.usage?.output_tokens)}`;
  const inOutTitle = `${fmtFull(r.usage?.input_tokens)} input / ` +
    `${fmtFull(r.usage?.output_tokens)} output tokens`;
  const cacheTitle = `${cachePct != null ? cachePct + ' cache hit; ' : ''}` +
    `${cacheTok ? fmt(cacheTok) + ' cached tokens' : 'no cached tokens'}`;
  const costText = fmtMoney(r.cost);
  const main = `<tr class="exp-row ${drawerId===r.id ? 'expanding' : ''}" data-id="${escapeHtml(r.id)}">
    <td style="color:var(--muted)" title="${escapeHtml(start.toLocaleString())}">${timeText}</td>
    <td title="${escapeHtml(reqIdentityText(r))}"><span class="req-ident">${entityPivot('client', r.client, null, r)} ${entityPivot('provider', r.provider)} ${modelCell(r)}</span></td>
    <td>${finalizedStatusPill(r)}${retryBadge}</td>
    <td title="${escapeHtml(fmtTTFT(r.ttft_ms))}">${fmtTTFT(r.ttft_ms)}</td>
    <td title="${escapeHtml(r.gen_tps ? tpsText + ' tok/s' : '')}">${tpsText}</td>
    <td title="${escapeHtml(inOutTitle)}">${inOutText}</td>
    <td style="color:${cacheTok ? 'var(--accent2)' : 'var(--muted)'}" title="${escapeHtml(cacheTitle)}">${cachePct != null ? cachePct : '-'}</td>
    <td style="color:var(--cyan)" title="${r.tool_calls ? escapeHtml(String(r.tool_calls)) + ' tool calls' : ''}">${r.tool_calls||'-'}</td>
    <td title="${escapeHtml(r.finish_reason||'')}">${escapeHtml(r.finish_reason||'-')}</td>
    <td style="color:var(--muted)" title="${escapeHtml(fmtDur(r.duration_ms))}">${fmtDur(r.duration_ms)}</td>
    <td style="color:var(--muted)" title="${escapeHtml(turnsTitle(r))}">${turnsCell(r)}</td>
    <td style="color:${r.queue_wait_ms ? 'var(--warn)' : 'var(--muted)'}" title="${r.queue_wait_ms ? escapeHtml(fmtDur(r.queue_wait_ms)) : ''}">${r.queue_wait_ms ? fmtDur(r.queue_wait_ms) : '-'}</td>
    <td title="estimated cost ${escapeHtml(costText)}">${costText}</td>
    <td><button type="button" class="hold-edit" data-open-req="${escapeHtml(r.id)}" aria-label="Open request details">›</button></td>
  </tr>`;
  // One collapsed sub-row per absorbed attempt (hidden until the badge toggle).
  // Sub-rows are clickable: data-attempt carries the attempt index, so a
  // click opens that attempt inside the request's drawer; the request's own
  // row (no data-attempt) still opens the request detail. The pill class
  // rides the shared attemptPillClass while the text stays this surface's
  // own ('-' for a status-less transport failure).
  if (!attempts.length) return sep + main;
  const subs = attempts.map(({ a, index: i }, pos) => {
    const asc = attemptPillClass(a);
    const when = a.at ? new Date(a.at).toLocaleTimeString() : '-';
    const label = a.error_type || (a.status_code ? 'http ' + a.status_code : 'transport');
    return `<tr class="retry-sub exp-row" data-retry-of="${escapeHtml(r.id)}" data-id="${escapeHtml(r.id)}" data-attempt="${i}" title="open request ${escapeHtml(r.id)} (attempt ${pos + 1})" hidden>
      <td>${when}</td>
      <td colspan="${RETRY_LABEL_COLS}" class="retry-sub-label"><span class="retry-tick">↳</span> attempt ${pos + 1} <span class="pill ${asc}">${a.status_code || '-'}</span> <span>${escapeHtml(label)}</span></td>
      <td colspan="${REQ_COLS - 1 - RETRY_LABEL_COLS}" class="retry-sub-msg" title="${escapeHtml(a.error_msg || '')}"><span class="retry-sub-text">${escapeHtml(a.error_msg || '-')}</span></td>
    </tr>`;
  }).join('');
  return sep + main + subs;
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

// REQ_COLUMNS is the single source of truth for request-log column identity.
// Columns are content-sized, not assigned pixel widths: REQ_FLOORS records the
// widest value each column has needed this session, so REQ_COLS (the day
// divider and retry sub-row span) can never drift from the header.
const REQ_COLUMNS = [
  ['time'],
  ['client · provider · model'],
  ['status'],
  ['ttft'],
  ['tps'],
  ['in/out'],
  ['cache'],
  ['tools'],
  ['finish'],
  ['dur'],
  ['turns'],
  ['queued'],
  ['cost'],
  ['', 'Request actions'],
];
const REQ_COLS = REQ_COLUMNS.length;
// Retry sub-rows keep the time cell, one label cell and one message cell. The
// message span is derived from REQ_COLS so a new log column cannot silently
// leave the sub-row short.
const RETRY_LABEL_COLS = 2;

// Request-log columns fit their content and keep a floor. Automatic table
// layout sizes each column to its widest cell; REQ_FLOORS remembers the widest
// box a column has needed in this session and reapplies it as the header
// cell's min-width, which auto layout treats as a hard column floor. A live row
// finalizing, a shorter neighbour arriving or a new page of history therefore
// never shrinks a column below the widest measured value; a new wider value can
// still grow it once and shift the cells to its right.
//
// The floors are measured offscreen at natural (max-content) width, so the
// extra space the card distributes across columns is never mistaken for
// content. Growth itself needs no measurement: auto layout grows the column in
// the same paint as the wider value, and the probe only records the new floor
// for later paints.
const REQ_FLOORS = new Array(REQ_COLS).fill(0);

// reqHeadCells is the one header-cell builder for the mounted table and the
// offscreen measurement probe. The floor rides the header cell because
// automatic table layout makes a cell's min-width a column floor while a wider
// body cell can still grow past it - exactly the monotonic fit the log needs.
// The probe measures natural widths, so it builds the same cells without the
// learned floors.
function reqHeadCells(withFloors) {
  return REQ_COLUMNS.map(([label, name], i) => {
    const floor = withFloors && REQ_FLOORS[i] > 0
      ? ` style="min-width:${REQ_FLOORS[i]}px"` : '';
    return `<th${name ? ` aria-label="${name}"` : ''}${floor}>${label}</th>`;
  }).join('');
}

// reqThead wraps the mounted table's header cells.
function reqThead() {
  return `<thead><tr>${reqHeadCells(true)}</tr></thead>`;
}

// reqRatchetWidths merges one measurement pass into the session floors. A
// non-finite or non-positive reading (an unlayouted document, a hidden probe)
// is ignored rather than resetting a column, and a narrower reading never
// lowers an existing floor.
function reqRatchetWidths(measured) {
  let grew = false;
  for (let i = 0; i < REQ_COLS; i++) {
    const w = measured[i];
    if (!Number.isFinite(w) || w <= 0) continue;
    // Keep sub-pixel precision: ceilings on fourteen columns add up to a
    // phantom horizontal scrollbar even when the content itself fits the card.
    const next = Math.round(w * 100) / 100;
    if (next > REQ_FLOORS[i]) { REQ_FLOORS[i] = next; grew = true; }
  }
  return grew;
}

// reqMeasureRows measures every column's natural width for the given row HTML
// and ratchets the floors. The probe lives offscreen at max-content, outside
// #tbl-requests, so the card's distributed allocation never leaks into a floor.
// Day dividers and retry sub-rows are removed first: their colspan cells would
// report one column's width across a span.
function reqMeasureRows(rows) {
  if (!rows.length || typeof document === 'undefined' || !document.body) return false;
  const host = document.createElement('div');
  host.id = 'req-measure';
  host.setAttribute('aria-hidden', 'true');
  host.innerHTML = '<table><thead><tr>' + reqHeadCells(false) +
    `</tr></thead><tbody>${rows.join('')}</tbody></table>`;
  document.body.appendChild(host);
  try {
    const table = host.querySelector('table');
    table.tBodies[0].querySelectorAll('tr.day-sep, tr.retry-sub').forEach(tr => tr.remove());
    const measured = [...table.tHead.rows[0].cells].map(th => th.getBoundingClientRect().width);
    return reqRatchetWidths(measured);
  } finally {
    host.remove();
  }
}

// applyReqFloors pushes the current floors onto an already-mounted table's
// header. The incremental paths measure before mutating, so the new floors are
// in place before the browser lays the inserted row out.
function applyReqFloors(container) {
  const ths = container.querySelectorAll('thead th');
  for (let i = 0; i < ths.length && i < REQ_COLS; i++) {
    ths[i].style.minWidth = REQ_FLOORS[i] > 0 ? REQ_FLOORS[i] + 'px' : '';
  }
}

// reqWidthRelevant reports whether a record change can alter a column's
// natural width. Streaming updates replace a row many times for token counts;
// those cells carry into the finalized row, which is measured before its final
// paint, so re-measuring every token would cost a layout for no lasting width
// change. Identity, status (including every live-pill transition and the debug
// badge) and attempts are the fields whose text can widen a column and then
// disappear before finalization.
function reqWidthRelevant(prev, rec) {
  if (!prev || !rec) return true;
  return prev.client !== rec.client || prev.provider !== rec.provider ||
    prev.model !== rec.model || prev.status_code !== rec.status_code ||
    (prev.attempts?.length || 0) !== (rec.attempts?.length || 0) ||
    !!prev.live !== !!rec.live || !!prev.paused !== !!rec.paused ||
    !!prev.throttled !== !!rec.throttled || !!prev.stream !== !!rec.stream ||
    !!prev.debug !== !!rec.debug;
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
// Move the .expanding highlight in place (the class itself is owned by
// renderRequests; this avoids a full table re-render on drawer nav).
function highlightRow() {
  document.querySelectorAll('.exp-row[data-id]').forEach(tr => {
    tr.classList.toggle('expanding', tr.dataset.id === drawerId);
  });
}

// kv builds one key/value row; renders falsy-but-meaningful values (0, false)
// while skipping null/undefined/empty-string so the drawer shows every captured
// field without noise.
function kv(k, v, color) {
  if (v === null || v === undefined || v === '') return '';
  const c = color ? ` style="color:${color}"` : '';
  return `<div class="detail-kv"><span class="k">${k}</span><span class="v"${c}>${v}</span></div>`;
}
const kvBool = (k, v) => (v === null || v === undefined) ? '' : kv(k, v ? 'true' : 'false');
const fmtT = t => {
  if (!t) return '';
  const d = new Date(t);
  return (isNaN(d) || d.getTime() < 86400000) ? '' : d.toLocaleTimeString() + '.' + String(d.getMilliseconds()).padStart(3, '0');
};
// sec wraps a set of kv rows into a titled detail-drawer section, dropping the
// section entirely when every row is empty.
const sec = (title, rows) => (rows && rows.filter(Boolean).length) ? `<div class="detail-section"><h4>${title}</h4>${rows.join('')}</div>` : '';

// detailHeaderRows renders a captured response-header map as sorted key/value
// rows - the record's own section and the attempt view share the one pattern
// (multi-value headers get one row per value).
function detailHeaderRows(headers) {
  if (!headers || !Object.keys(headers).length) return '';
  const hrows = [];
  Object.entries(headers).sort().forEach(([k, vs]) => {
    vs.forEach(v => hrows.push(`<div class="detail-kv"><span class="k" style="color:var(--accent2)">${escapeHtml(k)}</span><span class="v">${escapeHtml(v)}</span></div>`));
  });
  return hrows.join('');
}

// formatDetail renders every captured field for a record, grouped into
// sections. The only thing intentionally absent is message content (which the
// proxy never stores unless capture_body_preview is on, and then only as a
// bounded preview).
function formatDetail(r) {
  const S = [];

  S.push(sec('Request', [
    kv('id', `<span style="color:var(--muted)">${escapeHtml(r.id)}</span>`),
    kv('status', r.live ? liveStatusCell(r) : finalizedStatusPill(r)),
    kv('started', fmtT(r.start)),
    kv('ended', fmtT(r.end)),
    kv('duration', r.duration_ms != null ? fmtDur(r.duration_ms) : ''),
    kv('request bytes', r.request_bytes),
    kv('response bytes', r.response_bytes),
  ]));

  S.push(sec('Client', [
    kv('client', entityBadge('client', r.client, null, r)),
    kv('ip', escapeHtml(r.client_ip)),
    kv('lang / runtime', escapeHtml(r.client_lang)),
    kv('sdk', escapeHtml(r.client_meta && r.client_meta.pkg_ver)),
    kv('runtime', escapeHtml([r.client_meta && r.client_meta.runtime, r.client_meta && r.client_meta.runtime_ver].filter(Boolean).join(' '))),
    kv('os', escapeHtml([r.client_meta && r.client_meta.os, r.client_meta && r.client_meta.arch].filter(Boolean).join(' '))),
    kv('user-agent', escapeHtml(r.user_agent)),
    kv('format', escapeHtml(r.client_meta && r.client_meta.format)),
    kv('timeout', r.client_meta && r.client_meta.timeout_ms ? fmtDur(r.client_meta.timeout_ms) : ''),
    kv('max concurrency', r.client_meta && r.client_meta.max_concurrency),
    kv('method', r.method),
    kv('path', escapeHtml(r.path)),
    kv('key hash', r.key_hash ? `<span style="color:var(--muted)">${escapeHtml(shortId('key', r.key_hash))}</span>` : ''),
    kvBool('client disconnected', r.client_disconnected),
  ]));

  S.push(sec('Provider', [
    kv('provider', entityBadge('provider', r.provider) + ` <span style="color:var(--muted)">${escapeHtml(r.provider)}</span>`),
    kv('model (upstream)', escapeHtml(r.provider_model)),
    kv('request id', escapeHtml(r.provider_request_id)),
    kv('server', escapeHtml(r.provider_server)),
    kv('processing', r.processing_ms ? r.processing_ms + ' ms' : ''),
  ]));

  // The full non-content request shape.
  S.push(sec('Parameters', [
    kv('model', escapeHtml(String(r.model || '')) +
      (String(r.model || '').trim() && canonicalModel(r.model) !== String(r.model).trim()
        ? ` <span style="color:var(--muted)">→ groups as ${escapeHtml(canonicalModel(r.model))}</span>`
        : '')),
    kvBool('stream', r.stream),
    kv('max_tokens', r.req_max_tokens),
    kv('temperature', r.req_temperature),
    kv('top_p', r.req_top_p),
    kv('n', r.req_n),
    kv('presence_penalty', r.req_presence_pen),
    kv('frequency_penalty', r.req_frequency_pen),
    kv('seed', r.req_seed),
    kv('stop sequences', r.req_stop),
    kvBool('logprobs', r.req_logprobs),
    kv('top_logprobs', r.req_top_logprobs),
    kv('logit_bias', r.req_logit_bias ? r.req_logit_bias + ' ids' : ''),
    kv('response_format', escapeHtml(r.req_response_format)),
    kv('service_tier', escapeHtml(r.req_service_tier)),
    kv('reasoning_effort', escapeHtml(r.req_reasoning_effort)),
    kv('verbosity', escapeHtml(r.req_verbosity)),
    kvBool('parallel_tool_calls', r.req_parallel_tools),
    // "thinking" reflects reality: the request flag, or (if unset) whether the
    // response actually produced reasoning tokens - some models reason without
    // an explicit thinking parameter (always-on, or reasoning_effort).
    r.req_thinking != null ? kv('thinking', r.req_thinking ? 'true' : 'false')
      : (r.usage?.reasoning_tokens ? kv('thinking', 'yes (not requested)', 'var(--warn)') : ''),
    kv('tools defined', r.req_tools_count),
    kv('tool_choice', escapeHtml(r.req_tool_choice)),
    kvBool('stream_options', r.req_stream_opts),
    kv('metadata', r.req_metadata_keys ? r.req_metadata_keys + ' keys' : ''),
  ]));

  S.push(sec('Conversation', [
    kv('user turns', r.turns_user),
    kv('assistant turns', r.turns_assistant),
    kv('tool turns', r.turns_tool),
    kv('responding to', escapeHtml(r.last_turn_role)),
    (r.chars_system || r.chars_user || r.chars_assistant || r.chars_tool)
      ? kv('prompt size', 'sys ' + fmt(r.chars_system || 0) + ' · user ' + fmt(r.chars_user || 0) + ' · asst ' + fmt(r.chars_assistant || 0) + ' · tool ' + fmt(r.chars_tool || 0)) : '',
    (r.images || r.attachments)
      ? kv('content parts', (r.images ? r.images + ' image' + (r.images > 1 ? 's' : '') : '') + (r.images && r.attachments ? ', ' : '') + (r.attachments ? r.attachments + ' attachment' + (r.attachments > 1 ? 's' : '') : '')) : '',
  ]));

  if (r.prompt_preview) S.push(`<div class="detail-section"><h4>Prompt</h4><div class="preview-box">${escapeHtml(r.prompt_preview)}</div></div>`);
  if (r.response_preview) S.push(`<div class="detail-section"><h4>Response</h4><div class="preview-box">${escapeHtml(r.response_preview)}</div></div>`);

  S.push(sec('Tokens', [
    kv('input', fmt(r.usage?.input_tokens)),
    kv('output', fmt(r.usage?.output_tokens)),
    kv('total', fmt(r.usage?.total_tokens)),
    kv('reasoning', r.usage?.reasoning_tokens ? fmt(r.usage.reasoning_tokens) : '', 'var(--warn)'),
    kv('answer', r.answer_tokens ? fmt(r.answer_tokens) : ''),
    kv('cache read', r.usage?.cache_read_tokens ? fmt(r.usage.cache_read_tokens) : '', 'var(--accent2)'),
    kv('cache write', r.usage?.cache_write_tokens ? fmt(r.usage.cache_write_tokens) : '', 'var(--accent2)'),
  ]));

  S.push(sec('Performance', [
    kv('ttft', fmtTTFT(r.ttft_ms)),
    kv('tps', r.gen_tps ? r.gen_tps.toFixed(1) + ' tok/s' : ''),
    kv('first token', fmtT(r.first_token_at)),
    kv('last token', fmtT(r.last_token_at)),
    kv('first thinking', fmtT(r.first_reasoning_at)),
    kv('first answer', fmtT(r.first_answer_at)),
    kv('chunks', r.chunks),
    kv('finish', escapeHtml(r.finish_reason)),
    kv('cost', r.cost ? fmtMoney(r.cost) : '', 'var(--warn)'),
    // The adopted attempt's transport decomposition (httptrace): plain
    // integer milliseconds where 0/absent means the hook never fired (pooled
    // connection, plaintext upstream) so the row hides; the pool-hit fact
    // renders through kvBool, so a stored false still shows like every
    // other record flag.
    kv('upstream ttfb', r.upstream_ttfb_ms ? r.upstream_ttfb_ms + ' ms' : ''),
    kv('upstream connect', r.upstream_connect_ms ? r.upstream_connect_ms + ' ms' : ''),
    kv('upstream tls', r.upstream_tls_ms ? r.upstream_tls_ms + ' ms' : ''),
    kvBool('reused connection', r.upstream_conn_reused),
  ]));

  if (r.tool_calls || (r.tool_names && r.tool_names.length)) {
    S.push(sec('Tools', [
      kv('calls', r.tool_calls, 'var(--cyan)'),
      kv('names', (r.tool_names||[]).map(escapeHtml).join(', '), 'var(--cyan)'),
    ]));
  }

  S.push(sec('Rate limit / queue', [
    kv('remaining', r.rate_limit_remaining),
    kv('limit', r.rate_limit_limit),
    kv('retry-after', r.retry_after_ms ? fmtDur(r.retry_after_ms) : '', 'var(--warn)'),
    kv('queue wait', r.queue_wait_ms ? fmtDur(r.queue_wait_ms) : '', 'var(--warn)'),
    kv('retries', r.retries, r.retries ? 'var(--warn)' : ''),
    kvBool('rate limited', r.rate_limited),
  ]));

  // Absorbed retry attempts: the upstream errors the client never saw. Show
  // each transparently-retried response (status + provider error detail) in
  // order, before the final outcome.
  const retryEntries = retryAttempts(r);
  if (retryEntries.length) {
    const items = retryEntries.map(({ a, index: i }, pos) => {
      // Transport failures have no HTTP status (0); the shared attemptPill
      // shows the transport pill instead of a meaningless "0".
      const isTransport = !a.status_code;
      const pill = attemptPill(a);
      const when = fmtT(a.at);
      // Type, code and message are distinct fields - show each on its own so a
      // code is never conflated with a message (a 502 is not always the same).
      // Skip the type text when it merely repeats the transport pill (a transport
      // failure's error_type is "transport" - showing it again reads as
      // "transport · transport").
      const parts = [];
      if (a.error_type && !(isTransport && a.error_type === 'transport')) parts.push(`<span style="color:var(--warn)">${escapeHtml(a.error_type)}</span>`);
      if (a.error_code) parts.push(`<span style="color:var(--muted)">code ${escapeHtml(a.error_code)}</span>`);
      if (a.error_msg) parts.push(escapeHtml(a.error_msg));
      const detail = parts.length ? parts.join(' · ') : '<span style="color:var(--muted)">no error body</span>';
      const ra = a.retry_after_ms ? ` <span style="color:var(--muted)">· retry-after ${fmtDur(a.retry_after_ms)}</span>` : '';
      // Each attempt stores the same upstream metadata as the final
      // response; the provider request id is the matching key between a
      // provider-side failure report and the exact absorbed attempt.
      const rid = a.provider_request_id ? ` <span style="color:var(--muted)">· req ${escapeHtml(a.provider_request_id)}</span>` : '';
      // Each attempt belongs to this same request id; data-attempt names its
      // index, so clicking opens THAT attempt in the drawer (the parent
      // request stays selected and highlighted).
      return `<div class="detail-kv attempt-link" data-open-req="${escapeHtml(r.id)}" data-attempt="${i}" role="button" tabindex="0" title="open this request"><span class="k">#${pos+1} <span style="color:var(--muted)">${when}</span></span><span class="v">${pill} ${detail}${ra}${rid}</span></div>`;
    }).join('');
    S.push(`<div class="detail-section"><h4>Retry attempts (absorbed, transparent to client)</h4>${items}</div>`);
  }

  if (r.error_type || r.error_msg || r.error_code) {
    S.push(`<div class="detail-section"><h4>Error</h4>` +
      kv('type', escapeHtml(r.error_type) || '-', 'var(--err)') +
      // code: provider's structured error.code, else the HTTP status.
      kv('code', escapeHtml(r.error_code || (r.status_code ? String(r.status_code) : '')) || '-', 'var(--err)') +
      kv('message', escapeHtml(r.error_msg) || '-') + `</div>`);
  }

  const hrows = detailHeaderRows(r.response_headers);
  // upstream gzip renders with the wire facts: the transport strips
  // Content-Encoding/Content-Length when it auto-decompresses, so the header
  // map alone cannot show the response was compressed on the wire.
  if (hrows || r.upstream_gzip) {
    S.push(`<div class="detail-section"><h4>Response headers</h4>` +
      kvBool('upstream gzip', r.upstream_gzip) + hrows + `</div>`);
  }

  if (r.debug) {
    S.push(`<div class="detail-section" id="drawer-debug">` + debugSectionHead(r.id) + `<div class="preview-box" id="drawer-debug-body">loading capture…</div></div>`);
  }

  return S.join('');
}

// formatAttemptDetail renders ONE absorbed attempt in the drawer: the
// per-attempt truth Go retains (RetryAttempt parity - status, timing, error
// detail, retry-after, upstream response metadata, rate-limit pair, its own
// response headers) plus the parent identity that keeps the view oriented.
// Record-level outcomes (usage, cost, TTFT, previews, the debug capture) have
// no attempt twin and must never appear here; the parent request owns them.
function formatAttemptDetail(r, i) {
  const a = r.attempts[i];
  const isTransport = !a.status_code;
  const S = [];
  S.push(sec('Attempt', [
    kv('request', `<span style="color:var(--muted)">${escapeHtml(r.id)}</span>`),
    kv('attempt', retryOrdinal(r, i) + ' of ' + retryAttempts(r).length),
    kv('status', attemptPill(a)),
    kv('when', fmtT(a.at)),
    // Type, code and message are distinct fields - one row each, like the
    // record's Error section. Skip the type text when it merely repeats the
    // transport pill (the retry-list convention).
    a.error_type && !(isTransport && a.error_type === 'transport')
      ? kv('type', escapeHtml(a.error_type), 'var(--err)') : '',
    a.error_code ? kv('code', escapeHtml(a.error_code), 'var(--err)') : '',
    a.error_msg ? kv('message', escapeHtml(a.error_msg), 'var(--err)') : '',
    kv('retry-after', a.retry_after_ms ? fmtDur(a.retry_after_ms) : '', 'var(--warn)'),
  ]));
  S.push(sec('Provider', [
    kv('request id', escapeHtml(a.provider_request_id)),
    kv('server', escapeHtml(a.provider_server)),
    kv('model (upstream)', escapeHtml(a.provider_model)),
    kv('processing', a.processing_ms ? a.processing_ms + ' ms' : ''),
  ]));
  S.push(sec('Rate limit', [
    kv('remaining', a.rate_limit_remaining),
    kv('limit', a.rate_limit_limit),
  ]));
  const hrows = detailHeaderRows(a.response_headers);
  if (hrows) S.push(`<div class="detail-section"><h4>Response headers</h4>${hrows}</div>`);
  return S.join('');
}

function escapeHtml(s) {
  if (!s) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

// ---------- detail drawer ----------
// drawerShow owns the drawer's open tail: selection state, render, modal
// lifecycle and the parent row highlight. openDrawer shows the request's own
// detail; openDrawerAttempt shows one absorbed attempt (drawerId stays the
// parent record id, so the parent row keeps its highlight and the debug
// download-status guard stays keyed to the same record).
function drawerShow(id, attempt) {
  if (settingsIsOpen()) { closeSettings(); if (settingsIsOpen()) return; }
  // The initiating row control can be replaced by a live update while the
  // drawer is open; keep its stable selector so close can find the equivalent
  // control rather than falling back to BODY.
  const returnSelector = rowFocusKey(document.activeElement);
  drawerId = id;
  drawerAttempt = attempt;
  renderDrawer();
  if (!drawerId) return;
  openModal($('drawer'), {returnSelector, onPreempt: closeDrawer});
  $('drawer').classList.add('open');
  $('drawer-veil').classList.add('open');
  highlightRow();
}
function openDrawer(id) { drawerShow(id, null); }

// openDrawerAttempt opens one absorbed attempt of request id. The index
// arrives from a DOM dataset, so it is validated here: only the
// canonical digit spelling passes, then the integer and bound checks -
// bare Number coercion alone would let '' (0), '0x1' (1) and ' 1 ' (1)
// open an attempt. A malformed index fails closed to the request view.
// The re-render replaces the clicked control, so the attempt view's first
// control - the back affordance - takes focus, the same seat openModal
// gives a fresh open.
function openDrawerAttempt(id, index) {
  if (!/^\d+$/.test(String(index))) { openDrawer(id); return; }
  const i = Number(index);
  if (!Number.isInteger(i) || i < 0) { openDrawer(id); return; }
  drawerShow(id, i);
  const back = $('drawer-back');
  if (back) back.focus();
}

// drawerBackToRequest is the attempt view's title affordance: back to the
// request's own detail. The re-render removes the button, so the drawer's
// first control takes focus, the same seat a fresh open lands on.
function drawerBackToRequest() {
  if (drawerId == null) return;
  openDrawer(drawerId);
  const first = modalFocusables($('drawer'))[0];
  if (first) first.focus();
}
// _drawerRecord is the record object the drawer was last painted from;
// renderLive compares it to repaint only on a real live change. _drawerRenderKey
// is the (id, attempt) identity: scroll is preserved across a record change
// with the same identity and reset when the drawer navigates elsewhere.
let _drawerRecord = null;
let _drawerRenderKey = null;

function closeDrawer() {
  drawerId = null;
  drawerAttempt = null;
  _drawerRecord = null;
  _drawerRenderKey = null;
  if ($('drawer')) { $('drawer').classList.remove('open'); closeModal($('drawer')); }
  $('drawer-veil')?.classList.remove('open');
  // Release the previous request's rendered detail and debug capture: the
  // hidden drawer otherwise retained request/response bodies for the life of
  // the tab. The next open re-renders from its record.
  if ($('drawer-body')) $('drawer-body').innerHTML = '';
  if ($('drawer-title')) $('drawer-title').textContent = '';
  if ($('drawer-debug')) $('drawer-debug').innerHTML = '';
  highlightRow();
}
function drawerNav(dir) {
  // Same set + order as the request log: explorer scope + the grown window.
  // prev/next navigates requests; the attempt mode does not travel with it.
  const recs = logVisible();
  const i = recs.findIndex(r => r.id === drawerId);
  if (i < 0) return;
  const j = i + dir;
  if (j < 0 || j >= recs.length) return;
  drawerId = recs[j].id;
  drawerAttempt = null;
  renderDrawer();
  highlightRow();
}
function renderDrawer() {
  const r = recordById(drawerId);
  if (!r) { closeDrawer(); return; }
  // The attempt key re-resolves on every render from the live record: upserts
  // replace record objects wholesale and attempts are append-only, so a
  // captured attempt object would go stale as the array grows. A dangling key
  // (record left the window, or the index outran the array) fails closed to
  // the request view, never a throw.
  const a = drawerAttempt != null && Array.isArray(r.attempts) ? r.attempts[drawerAttempt] : null;
  if (drawerAttempt != null && !a) drawerAttempt = null;
  const body = $('drawer-body');
  const renderKey = String(drawerId) + '\u0000' + String(drawerAttempt);
  const scroll = body && _drawerRenderKey === renderKey ? body.scrollTop : 0;
  const base = reqIdentityText(r);
  if (a) {
    // The title carries the attempt identity: index, real status (transport
    // for status-less failures) and the provider request id that matches a
    // provider-side failure report to this exact attempt.
    const status = a.status_code ? String(a.status_code) : 'transport';
    const ident = ` · attempt ${retryOrdinal(r, drawerAttempt)} of ${retryAttempts(r).length} · ${status}`
      + (a.provider_request_id ? ` · ${a.provider_request_id}` : '');
    $('drawer-title').innerHTML = `<button type="button" class="btn" id="drawer-back" onclick="drawerBackToRequest()" title="back to request">‹ request</button> ${escapeHtml(base + ident)}`;
    body.innerHTML = formatAttemptDetail(r, drawerAttempt);
  } else {
    $('drawer-title').innerHTML = escapeHtml(base);
    body.innerHTML = formatDetail(r);
    if (r.debug) fillDrawerDebug(r.id);
  }
  body.scrollTop = scroll;
  _drawerRecord = r;
  _drawerRenderKey = renderKey;
}

// debugKV is the one debug key-value row builder. An empty value renders
// no row (deny by default); callers pass v pre-escaped, k is escaped here.
// color optionally tints the key span (capture headers use --accent2).
function debugKV(k, v, color) {
  if (v == null || v === '') return '';
  return `<div class="detail-kv"><span class="k"${color ? ` style="color:${color}"` : ''}>${escapeHtml(k)}</span><span class="v">${v}</span></div>`;
}

function debugHeaderRows(list) {
  if (!Array.isArray(list) || !list.length) return '';
  return list.map(h => debugKV(h.name || '', escapeHtml(h.value || ''), 'var(--accent2)')).join('');
}

// debugSectionHead renders the debug section header: the title, the
// download affordance and the message line. formatDetail's first paint and
// fillDrawerDebug's rewrite both render through it, so the button survives
// the capture render.
function debugSectionHead(id) {
  return `<h4>Debug <button type="button" class="btn" id="drawer-debug-download" data-id="${escapeHtml(id)}" aria-label="download debug capture" title="download debug capture">⬇</button> <span id="drawer-debug-status" style="color:var(--err)"></span></h4>`;
}

// runDebugCaptureDownload serves the drawer's Download button: the capture
// artifact through the shared operator download owner, with the designed
// failure message surfaced in the section header (the backup pane's
// empty-body 502 convention).
function runDebugCaptureDownload(id) {
  downloadArtifact('/admin/debug/capture?id=' + encodeURIComponent(id) + '&download=1', 'millivolt-debug.json.gz', 'debug capture download failed')
    .then(() => debugDownloadStatus(id, ''))
    .catch(err => debugDownloadStatus(id, String(err.message || err)));
}

// debugDownloadStatus writes the section's message line only while the
// drawer still shows the record that started the download, so a stale
// result never lands on the next request's section.
function debugDownloadStatus(id, msg) {
  if (drawerId !== id) return;
  const el = $('drawer-debug-status');
  if (el) el.textContent = msg;
}

function fillDrawerDebug(id) {
  const box = $('drawer-debug-body');
  if (!box) return;
  operatorJson('/admin/debug/capture?id=' + encodeURIComponent(id))
    .then(d => {
      if (drawerId !== id) return;
      const ident = d.identity || {};
      const timing = d.timing || {};
      const outcome = d.outcome || {};
      const req = d.request || {};
      const resp = d.response || {};
      const bits = [];
      bits.push(sec('Identity', [
        debugKV('session', escapeHtml(d.debug_session_id)),
        debugKV('client', escapeHtml(ident.client)),
        debugKV('provider', escapeHtml(ident.provider)),
        debugKV('model', escapeHtml(ident.model)),
        debugKV('path', escapeHtml(ident.path)),
        debugKV('stream', ident.stream ? 'yes' : 'no'),
        debugKV('expires', escapeHtml(d.expires_at)),
      ]));
      bits.push(sec('Timing', [
        debugKV('start', escapeHtml(timing.start)),
        debugKV('duration', timing.duration_ms != null ? fmtDur(timing.duration_ms) : ''),
        debugKV('ttft', fmtTTFT(timing.ttft_ms)),
        debugKV('queue wait', timing.queue_wait_ms ? fmtDur(timing.queue_wait_ms) : ''),
      ]));
      bits.push(sec('Request', [
        debugKV('url', escapeHtml(req.url)),
        debugKV('status', outcome.status_code != null ? String(outcome.status_code) : ''),
        debugKV('finish', escapeHtml(outcome.finish_reason)),
      ]));
      if (req.body && req.body.raw) {
        bits.push(`<div class="detail-section"><h4>Request body${req.body.truncated ? ' (truncated)' : ''}</h4><div class="preview-box">${escapeHtml(req.body.raw)}</div></div>`);
      }
      if (resp.body && resp.body.raw) {
        bits.push(`<div class="detail-section"><h4>Response body${resp.body.truncated ? ' (truncated)' : ''}</h4><div class="preview-box">${escapeHtml(resp.body.raw)}</div></div>`);
      }
      const rh = debugHeaderRows(req.headers);
      if (rh) bits.push(`<div class="detail-section"><h4>Request headers</h4>${rh}</div>`);
      const sh = debugHeaderRows(resp.headers);
      if (sh) bits.push(`<div class="detail-section"><h4>Response headers</h4>${sh}</div>`);
      const host = $('drawer-debug');
      if (host) host.innerHTML = debugSectionHead(id) + bits.filter(Boolean).join('');
    })
    .catch(() => { box.textContent = 'capture unavailable'; });
}
// Absorbed retry attempts in the drawer are clickable: a link carrying an
// attempt index opens that attempt in the request's drawer; one without an
// index (deny by default) opens the request itself. Delegated on the drawer
// body so re-renders never drop the handler; keyboard-activatable too.
function openAttemptLink(el) {
  if (el.dataset.attempt !== undefined) openDrawerAttempt(el.dataset.openReq, el.dataset.attempt);
  else openDrawer(el.dataset.openReq);
}
$('drawer-body').addEventListener('click', e => {
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) openAttemptLink(el);
});
$('drawer-body').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) { e.preventDefault(); openAttemptLink(el); }
});
// The debug section's Download button: delegated like the attempt links so
// re-renders never drop it. A native button carries its own keyboard
// activation, so no keydown arm is needed.
$('drawer-body').addEventListener('click', e => {
  const btn = e.target.closest('#drawer-debug-download');
  if (btn) runDebugCaptureDownload(btn.dataset.id);
});
