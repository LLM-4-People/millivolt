// ---- live request log: incremental DOM updates ----
// The request log must never shift under the user: new rows are appended as
// DOM nodes only (nothing existing is rebuilt), a row's DOM is replaced only
// when that one record actually changed (in-flight → finalized), and when the
// user has scrolled into the list, scrollTop is compensated for rows inserted
// above the viewport so the content under the cursor stays put.
// logHTML maps id → {rec, html, sep}: the record object reference enables O(1)
// change detection (replaced objects are always new from JSON.parse) so
// reqRow() is skipped entirely for rows whose record hasn't changed; sep is
// the row's day-divider label (its movement is part of the diff key).
const logHTML = new Map();
function removeRowGroup(id) {
  const c = $('tbl-requests');
  if (!c) return;
  c.querySelectorAll(`tr[data-id="${CSS.escape(id)}"], tr[data-sep-of="${CSS.escape(id)}"]`).forEach(tr => tr.remove());
  logHTML.delete(id);
}
function logPageSize() { return dashCfg.log_rows; }
function logCap() { return logLimit > 0 ? logLimit : logPageSize(); }
function resetLogWindow() {
  logLimit = 0;
  const hadArchive = logArchive.length > 0;
  logArchive = [];
  if (hadArchive) pruneModelNames();
  logFetching = false;
  logExhausted = false;
  logCursor = null;
  logFetchGen++;
  const box = $('tbl-requests');
  // An untouched/empty log is already at zero. Even assigning zero forces
  // layout, so first paint must not measure the empty shell just to reset it.
  if (box && logHTML.size) box.scrollTop = 0; // newest-first on navigation/full resync
}
function mergeLogArchive(scopedRing) {
  if (!logArchive.length) return scopedRing;
  const seen = new Set();
  for (const r of scopedRing) { if (r && r.id) seen.add(r.id); }
  const extra = [];
  for (const r of logArchive) {
    if (r && r.id && !seen.has(r.id)) { extra.push(r); seen.add(r.id); }
  }
  // Durable pages retain their server keyset order and append below the live
  // arrival-ordered ring. Do not sort live rows or derive an archive cutoff
  // from them: a late completion can have a much older start than its peers.
  return extra.concat(scopedRing);
}
function visibleLog(recs) {
  const n = Math.min(recs.length, logCap());
  return recs.slice(-n).reverse();
}
function logVisible() {
  if (!lastData) return [];
  return visibleLog(mergeLogArchive(derive(lastData).scopedRecs));
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
// both the boundary flags and (indirectly) the label.
function dayKey(r) {
  const t = recStartMs(r);
  if (!t) return '';
  const d = new Date(t);
  return d.getFullYear() + '-' + d.getMonth() + '-' + d.getDate();
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

function trGroupNodesHTML(html) {
  const t = document.createElement('template');
  t.innerHTML = html;
  return t.content;
}
function updateLogStats(d) {
  const el = $('f-stats');
  if (!el || !lastData) return;
  if (!d) d = derive(lastData);
  const loaded = mergeLogArchive(d.scopedRecs).length;
  const shown = logHTML.size;
  if (shown && shown < loaded) el.textContent = fmt(shown) + ' of ' + fmt(loaded) + ' records';
  else el.textContent = fmt(loaded) + ' records';
}

function renderRequestsIncremental(recs) {
  recs = mergeLogArchive(recs);
  const container = $('tbl-requests');
  if (!container) return;
  const tbody = container.querySelector('tbody');
  const page = logPageSize();
  const cap = logCap();
  if (cap > page) {
    const preview = recs.slice(-cap).reverse();
    let added = 0;
    for (const r of preview) { if (r && r.id && !logHTML.has(r.id)) added++; }
    if (added) logLimit = Math.min(recs.length, cap + added);
  }
  const recent = visibleLog(recs);
  if (!recent.length) { renderRequests(recs); return; }
  if (!tbody) { renderRequests(recs); return; } // empty-state placeholder in DOM: rebuild properly

  // Drop rows that left the window (scope change or dashCfg.log_rows cap overflow).
  const want = new Set(recent.map(r => r.id));
  container.querySelectorAll('tr.exp-row[data-id]').forEach(tr => {
    if (!want.has(tr.dataset.id)) removeRowGroup(tr.dataset.id);
  });

  // Process oldest→newest, inserting each new group at the top so the final
  // order is newest-first (later inserts land above earlier ones). Day-divider
  // ownership is part of the diff key: a boundary that appears or moves
  // between passes rebuilds exactly the two rows it touches.
  const seps = dayStartLabels(recent);
  // Scroll anchor: the content under the cursor must not move. Every DOM
  // change above the first pre-existing row contributes to its displacement -
  // inserts add their height, in-place group swaps add their net height (a
  // divider moving off a row cancels against the new divider above it). The
  // offset is applied once after the pass, from the scrollTop captured before
  // any mutation. Evictions only remove below the anchor, so they never count.
  const scrollAtTop = container.scrollTop;
  let anchorDelta = 0;
  for (let i = recent.length - 1; i >= 0; i--) {
    const r = recent[i];
    const sep = seps[i];
    const prev = logHTML.get(r.id);
    // O(1) change detection: record objects are replaced wholesale by upsert,
    // so identity compare catches every real change. A new JSON.parse always
    // creates a fresh object, so a replay that discards its duplicate never
    // reaches here (upsertRec returns false, the row is unchanged).
    if (!prev) {
      const html = reqRow(r, sep);
      tbody.insertBefore(trGroupNodesHTML(html), tbody.firstChild);
      if (scrollAtTop > 0) anchorDelta += rowGroupHeight(container, r.id);
      logHTML.set(r.id, { rec: r, html, sep });
    } else if (prev.rec !== r || prev.sep !== sep) {
      // The record itself changed (in-flight row finalized, retry badge
      // grew), or its day-divider position moved: replace only this row's
      // group, in place - everything else (including any text the user is
      // selecting) is untouched. Old nodes are captured before the insert and
      // removed by reference (replaceRowGroupDOM).
      const html = reqRow(r, sep);
      const before = rowGroupHeight(container, r.id);
      replaceRowGroupDOM(container, r.id, html);
      if (scrollAtTop > 0) anchorDelta += rowGroupHeight(container, r.id) - before;
      logHTML.set(r.id, { rec: r, html, sep });
    }
    // else: prev.rec === r && prev.sep === sep → unchanged, skip entirely.
  }
  if (scrollAtTop > 0 && anchorDelta !== 0) container.scrollTop = scrollAtTop + anchorDelta;
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
  const seps = dayStartLabels(recent);
  for (let i = 0; i < recent.length; i++) {
    const r = recent[i];
    if (!r) continue;
    const sep = seps[i];
    const prev = logHTML.get(r.id);
    if (!prev) {
      const html = reqRow(r, sep);
      logHTML.set(r.id, { rec: r, html, sep });
      tbody.appendChild(trGroupNodesHTML(html));
    } else if (prev.sep !== sep) {
      // An older page can re-shape the day boundary of the previously-last
      // row; rebuild just that group in place.
      const html = reqRow(r, sep);
      replaceRowGroupDOM(container, r.id, html);
      logHTML.set(r.id, { rec: r, html, sep });
    }
  }
}

function logNearBottom() {
  const c = $('tbl-requests');
  if (!c) return false;
  // Not laid out yet (jsdom, hidden): never auto-fetch - a 0×0 box would
  // look like "doesn't fill the viewport" and dump the whole history.
  if (c.clientHeight <= 0) return false;
  const room = c.scrollHeight - c.scrollTop - c.clientHeight;
  if (c.scrollHeight <= c.clientHeight + 8) return true;
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

function fetchOlderLog() {
  if (logFetching || logExhausted) return;
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
    const next = p && Number.isSafeInteger(p.cursor_ms) && typeof p.cursor_id === 'string' && p.cursor_id
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
    if (added.length) {
      logArchive = added.reverse().concat(logArchive);
      const merged = mergeLogArchive(ringScoped);
      logLimit = Math.min(merged.length, Math.max(logCap() + added.length, logPageSize()));
      appendOlderLogRows(merged);
      updateLogStats();
    }
    if (!logExhausted) scheduleLogFill();
  }).catch(() => {
    if (gen === logFetchGen) logFetching = false;
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

// liveStatusCell is the one owner of the in-flight status pill (log row +
// drawer). Classification is statusClass(); retry badges stay outside so
// reqRow can concatenate them. Drawer kv uses this helper with no badge.
function debugBadge(r) {
  if (!r || !r.debug) return '';
  return ` <span class="pill debug" data-edit-debug title="full debug capture - click to edit the session">dbg</span>`;
}

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
// record is its own gray 'cancel' pill - not an error. Retry badges stay on
// the log row only.
function finalizedStatusPill(r) {
  const isErr = recordIsError(r);
  const isCancel = !isErr && !!r.client_disconnected;
  const sc = isErr ? 'err' : (isCancel ? 'cancel' : (r.status_code >= 400 ? 'warn' : 'ok'));
  const errTitle = r.error_type ? ` title="${escapeHtml(r.error_type)}${r.error_msg ? ': ' + escapeHtml(r.error_msg) : ''}"` : '';
  const cancelTitle = isCancel ? ' title="client disconnected - cancelled"' : '';
  return `<span class="pill ${sc}"${errTitle || cancelTitle}>${isCancel ? '✕' : ''}${r.status_code}</span>` + debugBadge(r);
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
  const piv = entityPivot('model', canon);
  return raw && canon !== raw ? `<span title="original: ${escapeHtml(raw)}">${piv}</span>` : piv;
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
    const liveAttempts = r.attempts || [];
    const liveRetry = liveAttempts.length
      ? ` <span class="pill warn" title="${liveAttempts.length} retr${liveAttempts.length>1?'ies':'y'} so far (in flight): ${escapeHtml(liveAttempts.map(a => a.error_type || (a.status_code ? 'http '+a.status_code : 'transport')).join(', '))}">↻${liveAttempts.length}</span>`
      : '';
    // Input tokens may already carry a real estimate while a request streams
    // (cursor's parked turn reports an estimated prompt), but a plain 0 is
    // "not counted yet", never a real number - show a bare … like every
    // other not-yet-known cell.
    return `<tr class="exp-row live-row" data-id="${escapeHtml(r.id)}">
      <td style="color:var(--muted)">${new Date(r.start).toLocaleTimeString()}</td>
      <td>${entityPivot('client', r.client, null, r)} ${entityPivot('provider', r.provider)} ${modelCell(r)}</td>
      <td>${liveStatusCell(r)}${liveRetry}</td>
      <td>${r.ttft_ms ? fmtDur(r.ttft_ms) : '…'}</td>
      <td>-</td>
      <td>${inTok ? fmt(inTok) + '/…' : '…'}</td>
      <td>-</td>
      <td style="color:var(--cyan)">${r.tool_calls||'-'}</td>
      <td>-</td>
      <td style="color:var(--muted)">…</td>
      <td style="color:var(--muted)">${turnsCell(r)}</td>
      <td>-</td>
      <td>-</td>
      <td><button type="button" class="hold-edit" data-open-req="${escapeHtml(r.id)}" aria-label="Open request details">›</button></td>
    </tr>`;
  }
  // Retry badge: show the actual status that was retried (not a hardcoded
  // 429) - rate_limited means 429/503, otherwise it was a transient 5xx. When
  // the request has absorbed attempts, the badge is a toggle that expands one
  // sub-row per attempt (collapsed by default).
  const attempts = r.attempts || [];
  const retryBadge = attempts.length
    ? ` <button type="button" class="pill warn retry-toggle" data-retry-for="${escapeHtml(r.id)}" aria-expanded="false" title="${attempts.length} transparent retr${attempts.length>1?'ies':'y'} absorbed before this response - click to expand">↻${attempts.length} <span class="retry-caret">▸</span></button>`
    : (r.retries ? ` <span class="pill warn" title="${r.retries} retries">↻${r.retries}</span>` : '');
  // Cache as a percentage of input tokens (raw count stays in the drawer).
  const inTok = r.usage?.input_tokens || 0, cacheTok = r.usage?.cache_read_tokens || 0;
  const cachePct = inTok > 0 ? pct2(cacheTok, inTok) : null;
  const main = `<tr class="exp-row ${drawerId===r.id ? 'expanding' : ''}" data-id="${escapeHtml(r.id)}">
    <td style="color:var(--muted)">${new Date(r.start).toLocaleTimeString()}</td>
    <td>${entityPivot('client', r.client, null, r)} ${entityPivot('provider', r.provider)} ${modelCell(r)}</td>
    <td>${finalizedStatusPill(r)}${retryBadge}</td>
    <td>${fmtTTFT(r.ttft_ms)}</td>
    <td>${r.overall_tps ? r.overall_tps.toFixed(1) : '-'}</td>
    <td>${fmt(r.usage?.input_tokens)}/${fmt(r.usage?.output_tokens)}</td>
    <td style="color:${cacheTok ? 'var(--accent2)' : 'var(--muted)'}" title="${cacheTok ? fmt(cacheTok) + ' cached tokens' : ''}">${cachePct != null ? cachePct : '-'}</td>
    <td style="color:var(--cyan)">${r.tool_calls||'-'}</td>
    <td>${escapeHtml(r.finish_reason||'-')}</td>
    <td style="color:var(--muted)">${fmtDur(r.duration_ms)}</td>
    <td style="color:var(--muted)">${turnsCell(r)}</td>
    <td style="color:${r.queue_wait_ms ? 'var(--warn)' : 'var(--muted)'}">${r.queue_wait_ms ? fmtDur(r.queue_wait_ms) : '-'}</td>
    <td>${fmtMoney(r.cost)}</td>
    <td><button type="button" class="hold-edit" data-open-req="${escapeHtml(r.id)}" aria-label="Open request details">›</button></td>
  </tr>`;
  // One collapsed sub-row per absorbed attempt (hidden until the badge toggle).
  // Sub-rows are clickable like normal request rows - they open the same
  // request's drawer (the attempts live on the parent record, shown in the
  // drawer's retry section).
  if (!attempts.length) return sep + main;
  const subs = attempts.map((a, i) => {
    const asc = a.status_code >= 500 ? 'err' : (a.status_code >= 400 ? 'warn' : 'err');
    const when = a.at ? new Date(a.at).toLocaleTimeString() : '-';
    const label = a.error_type || (a.status_code ? 'http ' + a.status_code : 'transport');
    return `<tr class="retry-sub exp-row" data-retry-of="${escapeHtml(r.id)}" data-id="${escapeHtml(r.id)}" title="open request ${escapeHtml(r.id)} (attempt ${i + 1})" hidden>
      <td style="color:var(--muted)">${when}</td>
      <td colspan="2" class="retry-sub-label"><span class="retry-tick">↳</span> attempt ${i + 1} <span class="pill ${asc}">${a.status_code || '-'}</span> <span style="color:var(--muted)">${escapeHtml(label)}</span></td>
      <td colspan="11" class="retry-sub-msg" title="${escapeHtml(a.error_msg || '')}">${escapeHtml(a.error_msg || '-')}</td>
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
  logHTML.clear();
  // Incremental/append mutate the tbody without going through updateSection,
  // so lastRender['tbl-requests'] is stale. A full rebuild must always paint
  // (nav / filter / resync), even when the window HTML matches first paint.
  delete lastRender['tbl-requests'];
  if (!recent.length) {
    updateSection('tbl-requests', '<div class="empty">no matching requests</div>');
    // An empty scope (its records predate the ring - a provider not used in a
    // while) must still probe the store: nothing will ever scroll here, so the
    // fill probe below is the only path that can pull the scope's durable rows.
    scheduleLogFill();
    return;
  }
  const seps = dayStartLabels(recent);
  for (let i = 0; i < recent.length; i++) {
    const r = recent[i];
    logHTML.set(r.id, { rec: r, html: reqRow(r, seps[i]), sep: seps[i] });
  }
  updateSection('tbl-requests', `<table>${REQ_THEAD}<tbody>${Array.from(logHTML.values()).map(e => e.html).join('')}</tbody></table>`);
  scheduleLogFill();
}

// REQ_THEAD is the single source of truth for the request log's columns
// (header row; rows are rendered by reqRow). REQ_COLS is derived from it -
// the day-divider rows span the full table (one owner, never a second count).
const REQ_THEAD = `<thead><tr><th>time</th><th>client · provider · model</th><th>status</th><th>ttft</th><th>tps</th><th>in/out</th><th>cache</th><th>tools</th><th>finish</th><th>dur</th><th>turns</th><th>queued</th><th>cost</th><th></th></tr></thead>`;
const REQ_COLS = (REQ_THEAD.match(/<th>/g) || []).length;

// Delegated row clicks - no inline onclick interpolating the record id.
$('tbl-requests').addEventListener('click', e => {
  // A pivot/entity link navigates via the hash router - never open the drawer.
  if (e.target.closest('a.ent-link')) return;
  // The paused pill edits the matching hold (same form as clicking the hold row).
  if (e.target.closest('.pill.paused[data-edit-pause]')) {
    e.stopPropagation();
    const tr = e.target.closest('.exp-row[data-id]');
    if (tr) editHoldForRecord(recordById(tr.dataset.id));
    return;
  }
  if (e.target.closest('.pill.debug[data-edit-debug]')) {
    e.stopPropagation();
    const tr = e.target.closest('.exp-row[data-id]');
    if (tr) editDebugForRecord(recordById(tr.dataset.id));
    return;
  }
  if (e.target.closest('.pill.throttled[data-edit-limit]')) {
    e.stopPropagation();
    const tr = e.target.closest('.exp-row[data-id]');
    if (tr) editLimitForRecord(recordById(tr.dataset.id));
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
  // data-id too, so clicking one opens the same request (its attempts are in
  // the drawer's retry section).
  const tr = e.target.closest('.exp-row[data-id]');
  if (tr) openDrawer(tr.dataset.id);
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
    kv('tps', r.overall_tps ? r.overall_tps.toFixed(1) + ' tok/s' : ''),
    kv('first token', fmtT(r.first_token_at)),
    kv('last token', fmtT(r.last_token_at)),
    kv('first answer', fmtT(r.first_answer_at)),
    kv('finish', escapeHtml(r.finish_reason)),
    kv('cost', r.cost ? fmtMoney(r.cost) : '', 'var(--warn)'),
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
  if (r.attempts && r.attempts.length) {
    const items = r.attempts.map((a, i) => {
      // Transport failures have no HTTP status (0); show a transport pill
      // instead of a meaningless "0". HTTP failures show their real status.
      const isTransport = !a.status_code;
      const pill = isTransport
        ? `<span class="pill err">transport</span>`
        : `<span class="pill ${a.status_code >= 500 ? 'err' : 'warn'}">${a.status_code}</span>`;
      const when = a.at ? new Date(a.at).toLocaleTimeString() + '.' + String(new Date(a.at).getMilliseconds()).padStart(3,'0') : '';
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
      // Each attempt belongs to this same request id; clicking jumps the drawer
      // to that request (and highlights its log row).
      return `<div class="detail-kv attempt-link" data-open-req="${escapeHtml(r.id)}" role="button" tabindex="0" title="open this request"><span class="k">#${i+1} <span style="color:var(--muted)">${when}</span></span><span class="v">${pill} ${detail}${ra}</span></div>`;
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

  if (r.response_headers && Object.keys(r.response_headers).length) {
    const hrows = [];
    Object.entries(r.response_headers).sort().forEach(([k, vs]) => {
      vs.forEach(v => hrows.push(`<div class="detail-kv"><span class="k" style="color:var(--accent2)">${escapeHtml(k)}</span><span class="v">${escapeHtml(v)}</span></div>`));
    });
    S.push(`<div class="detail-section"><h4>Response headers</h4>${hrows.join('')}</div>`);
  }

  if (r.debug) {
    S.push(`<div class="detail-section" id="drawer-debug"><h4>Debug</h4><div class="preview-box" id="drawer-debug-body">loading capture…</div></div>`);
  }

  return S.join('');
}

function escapeHtml(s) {
  if (!s) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

// ---------- detail drawer ----------
function openDrawer(id) {
  if (settingsIsOpen()) { closeSettings(); if (settingsIsOpen()) return; }
  drawerId = id;
  renderDrawer();
  if (!drawerId) return;
  openModal($('drawer'));
  $('drawer').classList.add('open');
  $('drawer-veil').classList.add('open');
  highlightRow();
}
function closeDrawer() {
  drawerId = null;
  if ($('drawer')) { $('drawer').classList.remove('open'); closeModal($('drawer')); }
  $('drawer-veil')?.classList.remove('open');
  highlightRow();
}
function drawerNav(dir) {
  // Same set + order as the request log: explorer scope + the grown window.
  const recs = logVisible();
  const i = recs.findIndex(r => r.id === drawerId);
  if (i < 0) return;
  const j = i + dir;
  if (j < 0 || j >= recs.length) return;
  drawerId = recs[j].id;
  renderDrawer();
  highlightRow();
}
function renderDrawer() {
  const r = recordById(drawerId);
  if (!r) { closeDrawer(); return; }
  $('drawer-title').innerHTML = escapeHtml(`${r.client || '?'} · ${r.provider || '?'} · ${canonicalModel(r.model) || '-'}`);
  $('drawer-body').innerHTML = formatDetail(r);
  $('drawer-body').scrollTop = 0;
  if (r.debug) fillDrawerDebug(r.id);
}

function debugKV(k, v) {
  if (v == null || v === '') return '';
  return `<div class="detail-kv"><span class="k">${escapeHtml(k)}</span><span class="v">${v}</span></div>`;
}

function debugHeaderRows(list) {
  if (!Array.isArray(list) || !list.length) return '';
  return list.map(h => `<div class="detail-kv"><span class="k" style="color:var(--accent2)">${escapeHtml(h.name || '')}</span><span class="v">${escapeHtml(h.value || '')}</span></div>`).join('');
}

function fillDrawerDebug(id) {
  const box = $('drawer-debug-body');
  if (!box) return;
  operatorFetch('/admin/debug/capture?id=' + encodeURIComponent(id))
    .then(r => r.ok ? r.json() : Promise.reject())
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
      if (host) host.innerHTML = '<h4>Debug</h4>' + bits.filter(Boolean).join('');
    })
    .catch(() => { box.textContent = 'capture unavailable'; });
}
// Absorbed retry attempts in the drawer are clickable: they open the drawer on
// the request they belong to (and highlight its log row). Delegated on the
// drawer body so re-renders never drop the handler; keyboard-activatable too.
$('drawer-body').addEventListener('click', e => {
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) openDrawer(el.dataset.openReq);
});
$('drawer-body').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) { e.preventDefault(); openDrawer(el.dataset.openReq); }
});
