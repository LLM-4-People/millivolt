// ---------- chart timeframes ----------
// The chart time vocabulary and the timeline picker, on the shared menu
// foundation. TIMEFRAMES is the ONE owner of every selectable timeframe: the
// persisted token, the chip and long labels, and the kind that decides how a
// selection resolves to a chart request. Duration tokens are minutes-as-
// strings (the exact spellings dash.chart has always saved: '15', '60', ...),
// so a persisted view keeps working; calendar tokens resolve in LOCAL time.
//
// Durations resolve to window=<minutes>, all to window=all, and calendar
// tokens to a clamped from_ms/to_ms pair. Availability never offers a window
// wider than the stored history reported by the server's data_from_ms; all is
// always offered because it means exactly the stored history.
const TIMEFRAMES = [
  { token: '1', kind: 'duration', minutes: 1, short: '1m', long: '1 minute' },
  { token: '5', kind: 'duration', minutes: 5, short: '5m', long: '5 minutes' },
  { token: '15', kind: 'duration', minutes: 15, short: '15m', long: '15 minutes' },
  { token: '30', kind: 'duration', minutes: 30, short: '30m', long: '30 minutes' },
  { token: '60', kind: 'duration', minutes: 60, short: '1h', long: '1 hour' },
  { token: '180', kind: 'duration', minutes: 180, short: '3h', long: '3 hours' },
  { token: '360', kind: 'duration', minutes: 360, short: '6h', long: '6 hours' },
  { token: '720', kind: 'duration', minutes: 720, short: '12h', long: '12 hours' },
  { token: '1440', kind: 'duration', minutes: 1440, short: '24h', long: '24 hours' },
  { token: '10080', kind: 'duration', minutes: 10080, short: '7d', long: '7 days' },
  { token: '43200', kind: 'duration', minutes: 43200, short: '30d', long: '30 days' },
  { token: '129600', kind: 'duration', minutes: 129600, short: '90d', long: '90 days' },
  { token: '525600', kind: 'duration', minutes: 525600, short: '1y', long: '1 year' },
  { token: 'all', kind: 'all', short: 'all', long: 'all time' },
  { token: 'today', kind: 'range', short: 'today', long: 'today' },
  { token: 'yesterday', kind: 'range', short: 'yesterday', long: 'yesterday' },
  { token: 'week', kind: 'range', short: 'this week', long: 'this week' },
  { token: 'month', kind: 'range', short: 'this month', long: 'this month' },
];
const TF_MINUTE_MS = 60000;

// The stored-history extent: from is the earliest stored request (unix ms,
// 0 = nothing stored) and known says whether the server has reported it yet.
// A not-yet-reported extent must not gate a restored selection: the page can
// carry preferences before its first payload lands.
let tfExtent = { from: 0, known: false };

// tfEntry is the one token lookup. Unknown tokens deny by default everywhere.
function tfEntry(token) {
  const t = String(token);
  return TIMEFRAMES.find(frame => frame.token === t) || null;
}

// tfSetExtent stores the server's stored-history floor (data_from_ms). A
// missing or malformed value keeps the last known extent; 0 is the real
// "nothing stored" state and disables every token except all.
function tfSetExtent(fromMs) {
  const n = Number(fromMs);
  if (!Number.isFinite(n) || n < 0) return;
  tfExtent = { from: n > 0 ? Math.floor(n) : 0, known: true };
}

// tfExtentFrom exposes the coverage floor to the surface that describes it
// (the chart context row's all-time coverage append).
function tfExtentFrom() {
  return tfExtent.from;
}

// tfRangeSpan resolves one calendar token to its local-clock [start, end)
// bounds, with the start clamped to the stored extent when the extent is
// later. Unknown tokens return null; the caller decides availability from
// the clamped pair (start < end).
function tfRangeSpan(token, nowMs) {
  const now = Number.isFinite(nowMs) ? nowMs : Date.now();
  const d = new Date(now);
  let from;
  let to = now;
  switch (token) {
    case 'today':
      from = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
      break;
    case 'yesterday':
      from = new Date(d.getFullYear(), d.getMonth(), d.getDate() - 1).getTime();
      to = new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
      break;
    case 'week':
      // Monday is the first day of the week: the days since Monday are
      // weekday index 1..6 shifted onto 0..6 (Sunday is 0).
      from = new Date(d.getFullYear(), d.getMonth(), d.getDate() - ((d.getDay() + 6) % 7)).getTime();
      break;
    case 'month':
      from = new Date(d.getFullYear(), d.getMonth(), 1).getTime();
      break;
    default:
      return null;
  }
  if (tfExtent.from > from) from = tfExtent.from;
  return { fromMs: from, toMs: to };
}

// tfAvailable is the one availability gate. Durations never offer a window
// wider than the stored history (equality allowed); calendar tokens need a
// positive extent whose clamped span is non-empty; all is always available.
// Before the server has reported an extent nothing is gated (the page has no
// fact to judge with), and an extent of 0 disables everything but all.
function tfAvailable(token, nowMs) {
  const entry = tfEntry(token);
  if (!entry) return false;
  if (entry.kind === 'all') return true;
  if (!tfExtent.known) return true;
  if (tfExtent.from <= 0) return false;
  const now = Number.isFinite(nowMs) ? nowMs : Date.now();
  if (entry.kind === 'duration') return entry.minutes * TF_MINUTE_MS <= now - tfExtent.from;
  const span = tfRangeSpan(entry.token, now);
  return !!span && span.fromMs < span.toMs;
}

// tfResolve resolves one token against the current extent and now: durations
// carry their minutes, all carries its kind, calendar tokens carry the
// clamped pair. label is the trigger's short resolved form ('last 1h',
// 'all time', 'today'); long is the context row and coverage note form.
// Resolving does not check availability: a range token beyond the stored
// history still reports its clamped pair, and tfAvailable tells the caller
// whether that pair is usable.
function tfResolve(token, nowMs) {
  const entry = tfEntry(token);
  if (!entry) return null;
  if (entry.kind === 'all') return { token: entry.token, kind: 'all', label: entry.long, long: entry.long };
  if (entry.kind === 'duration') {
    return { token: entry.token, kind: 'duration', minutes: entry.minutes, label: 'last ' + entry.short, long: entry.long };
  }
  const span = tfRangeSpan(entry.token, nowMs);
  return { token: entry.token, kind: 'range', fromMs: span.fromMs, toMs: span.toMs, label: entry.short, long: entry.long };
}

// tfClock and tfSince are the control's whole formatting surface: fmtDur
// (core.js) owns every duration, tfClock is the note's HH:MM stamp and
// tfSince picks the clock form for today, a short date otherwise.
const tfClock = ms => String(new Date(ms).getHours()).padStart(2, '0') + ':' + String(new Date(ms).getMinutes()).padStart(2, '0');
function tfSince(ms) {
  const d = new Date(ms);
  const now = new Date();
  if (d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate()) {
    return tfClock(ms);
  }
  return d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

// tfCoverage computes the rail fill and the note's two ends for the shown
// token: percentages across [extent.from, now]. null means there is no
// stored history to draw (the caller shows the no-history note).
function tfCoverage(shown, now) {
  const from = tfExtent.from;
  if (!tfExtent.known || from <= 0 || now <= from) return null;
  const entry = tfEntry(shown);
  const total = now - from;
  if (entry.kind === 'all') {
    return { left: 0, width: 100, leftText: fmtDur(total) + ' of history', rightText: 'since ' + tfSince(from) };
  }
  if (entry.kind === 'duration') {
    const width = Math.min(100, (entry.minutes * TF_MINUTE_MS / total) * 100);
    return { left: 100 - width, width, leftText: entry.long, rightText: fmtDur(total) + ' of history' };
  }
  const span = tfRangeSpan(entry.token, now);
  const left = Math.max(0, ((span.fromMs - from) / total) * 100);
  const right = Math.min(100, ((span.toMs - from) / total) * 100);
  return { left, width: Math.max(0, right - left), leftText: entry.short, rightText: 'since ' + tfSince(span.fromMs) };
}

// A trigger without an id still gets a unique panel id from this sequence;
// an id-bearing trigger names its panel after itself (the chart-window
// trigger's fixed id matches the aria-controls in index.html).
let tfPanelSeq = 0;

// buildTimeframeControl builds and owns the timeline panel: the title, the
// coverage block (rail + note), the zoom chip group and the calendar chip
// group. onPick receives the token of a chip click (the caller persists and
// fetches); the control never fetches. sync(token) re-reads availability
// against the current extent and now and repaints the trigger label, the
// pressed/enabled chips, the rail fill and the note; the optional token
// overrides the control's stored selection so chart.js stays the one
// selection owner. The panel opens through menu.js (align 'end', panel
// focus) and is appended to document.body.
function buildTimeframeControl(trigger, labelEl, onPick) {
  const panel = document.createElement('div');
  panel.className = 'mn';
  panel.setAttribute('role', 'group');
  panel.setAttribute('aria-label', 'Timeframe');
  panel.hidden = true;
  panel.id = (trigger && trigger.id ? trigger.id : 'timeframe-' + (++tfPanelSeq)) + '-panel';
  panel.style.setProperty('--mn-w', '320px');
  document.body.appendChild(panel);
  if (trigger) trigger.setAttribute('aria-controls', panel.id);

  const head = document.createElement('div');
  head.className = 'mn-hd';
  const title = document.createElement('div');
  title.className = 'mn-title';
  title.textContent = 'Timeframe';
  head.appendChild(title);
  panel.appendChild(head);

  const body = document.createElement('div');
  body.className = 'mn-body';
  panel.appendChild(body);

  const coverage = document.createElement('div');
  coverage.className = 'mn-sec';
  const rail = document.createElement('div');
  rail.className = 'mn-rail';
  const fill = document.createElement('span');
  fill.className = 'mn-rail-fill';
  fill.hidden = true;
  rail.appendChild(fill);
  const note = document.createElement('div');
  note.className = 'mn-note';
  // The ceiling line is the visible explanation a phone needs: a disabled
  // chip's title is hover-only and a disabled button cannot take focus, so
  // the panel states the rule once, in text, whenever any window is gated.
  const ceiling = document.createElement('div');
  ceiling.className = 'mn-ceiling';
  ceiling.id = panel.id + '-ceiling';
  ceiling.textContent = 'disabled windows need more stored history';
  ceiling.hidden = true;
  coverage.append(rail, note, ceiling);
  body.appendChild(coverage);

  const chips = new Map();
  const addGroup = (heading, frames) => {
    const sec = document.createElement('div');
    sec.className = 'mn-sec';
    const h = document.createElement('div');
    h.className = 'mn-sec-h';
    h.textContent = heading;
    const row = document.createElement('div');
    row.className = 'mn-chips';
    sec.append(h, row);
    for (const frame of frames) {
      const chip = document.createElement('button');
      chip.type = 'button';
      chip.className = 'mn-chip';
      chip.dataset.tf = frame.token;
      chip.textContent = frame.short;
      chip.setAttribute('aria-pressed', 'false');
      chip.addEventListener('click', () => pick(frame.token));
      chips.set(frame.token, chip);
      row.appendChild(chip);
    }
    body.appendChild(sec);
    return row;
  };
  const zoomRow = addGroup('zoom', TIMEFRAMES.filter(frame => frame.kind !== 'range'));
  zoomRow.setAttribute('aria-describedby', ceiling.id);
  addGroup('calendar', TIMEFRAMES.filter(frame => frame.kind === 'range'));

  let selected = 'all';

  // chipTitle is the one chip-state explanation: an available chip names what
  // it does, an unavailable one names why.
  function chipTitle(frame, available, now) {
    if (frame.kind === 'all') return 'all stored history';
    if (available) return frame.kind === 'duration' ? 'last ' + frame.long : 'show ' + frame.long;
    if (frame.kind === 'duration' && tfExtent.from > 0) return 'only ' + fmtDur(now - tfExtent.from) + ' of history is stored';
    return tfExtent.from > 0 ? 'no stored history in ' + frame.short : 'no stored history yet';
  }

  // paint is the one repaint of the control's visible state: the shown token
  // falls back to all time when the stored selection is unavailable (a
  // restored window wider than the stored history stays stored, but cannot
  // be shown as active).
  function paint() {
    const now = Date.now();
    const shown = tfAvailable(selected, now) ? selected : 'all';
    const resolved = tfResolve(shown, now);
    let capped = false;
    for (const [token, chip] of chips) {
      const available = tfAvailable(token, now);
      if (!available) capped = true;
      chip.disabled = !available;
      chip.setAttribute('aria-pressed', token === shown ? 'true' : 'false');
      chip.title = chipTitle(tfEntry(token), available, now);
    }
    ceiling.hidden = !capped;
    if (labelEl) labelEl.textContent = resolved.label;
    const cov = tfCoverage(shown, now);
    if (!cov) {
      fill.hidden = true;
      note.textContent = tfExtent.known ? 'no stored history yet' : 'waiting for history';
      return;
    }
    fill.hidden = false;
    fill.style.left = cov.left + '%';
    fill.style.width = cov.width + '%';
    note.textContent = '';
    const start = document.createElement('span');
    start.textContent = cov.leftText;
    const end = document.createElement('span');
    end.textContent = cov.rightText;
    note.append(start, ' · ', end);
  }

  function pick(token) {
    // A stale extent can leave a chip looking enabled after availability
    // moved; the pick re-checks before it reports.
    if (!tfEntry(token) || !tfAvailable(token, Date.now())) return;
    selected = token;
    paint();
    if (typeof onPick === 'function') onPick(token);
    // One pick, one close: the chart the pick just fetched must not stay
    // covered by the panel. menuClose hands focus back to the trigger.
    close();
  }

  function sync(token) {
    if (token != null && tfEntry(token)) selected = String(token);
    paint();
  }

  function open() {
    paint();
    if (trigger) menuOpen(trigger, panel, { align: 'end', initialFocus: 'panel' });
  }

  function close() {
    menuClose(panel);
  }

  if (trigger) {
    if (!trigger.hasAttribute('aria-expanded')) trigger.setAttribute('aria-expanded', 'false');
    trigger.addEventListener('click', () => { if (menuIsOpen(panel)) close(); else open(); });
  }
  paint();
  return { panel, open, close, sync };
}
