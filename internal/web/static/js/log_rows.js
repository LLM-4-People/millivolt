// Request-log rows: the day dividers, the row-group DOM identity, the cell
// renderers, the column registry with its incremental width floors, and the
// one HTML escaper the dashboard modules share. log.js owns when a row is
// built and replaced; this module owns what a row is.

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
// only the debug pill above. The drawer's status row uses this helper directly.
function liveStatusCell(r) {
  const sc = statusClass(r);
  if (sc === 'paused') {
    return `<span class="pill paused" data-edit-pause title="held by operator pause - click to edit the hold">paused</span>` + debugBadge(r);
  }
  if (sc === 'throttled') {
    return `<span class="pill throttled" data-edit-limit title="queued by the proxy - not yet sent upstream; click to edit provider limits">throttled</span>` + debugBadge(r);
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

function escapeHtml(s) {
  if (!s) return '';
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}
