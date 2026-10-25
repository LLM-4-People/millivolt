// Request-log archive window: the durable keyset pages below the live ring, the
// archive identity set and the merged-window memo, and the scroll/fill/retry
// owners that page them in. log.js owns the live frame; this module owns the
// bounded window those frames materialize from. It loads first of the log
// modules so every later module reads the window through these owners.

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
