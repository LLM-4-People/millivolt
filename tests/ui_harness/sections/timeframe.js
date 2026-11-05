// test 14: chart time vocabulary, availability and the timeline control
// (js/timeframe.js). The vocabulary is pinned to its literal table, calendar
// resolution to local-midnight boundaries under a stubbed clock, availability
// to the stored extent, and the built control to its groups, chips, rail fill,
// note strings and pick contract. The section builds its own throwaway control
// and removes it afterwards; the shared page's extent is restored from the
// fixture payload at the end.
module.exports = async function timeframe(H) {
  const {check, d, w} = H;
  const state = H.state;

  // ---- vocabulary integrity ----
  const frames = w.eval('TIMEFRAMES');
  check('every timeframe token is unique', new Set(frames.map(f => f.token)).size === frames.length);
  const durations = frames.filter(f => f.kind === 'duration');
  check('duration tokens are minutes-as-strings',
    durations.length === 13 && durations.every(f => String(f.minutes) === f.token && Number.isInteger(f.minutes) && f.minutes > 0));
  check('the duration ladder keeps the persisted spellings',
    durations.map(f => f.token).join() === '1,5,15,30,60,180,360,720,1440,10080,43200,129600,525600');
  check('duration short and long labels stay lowercase with their units',
    durations.every(f => f.short === f.short.toLowerCase() && f.long === f.long.toLowerCase()) &&
    durations.map(f => f.short).join() === '1m,5m,15m,30m,1h,3h,6h,12h,24h,7d,30d,90d,1y' &&
    durations.map(f => f.long).join() === '1 minute,5 minutes,15 minutes,30 minutes,1 hour,3 hours,6 hours,12 hours,24 hours,7 days,30 days,90 days,1 year');
  check('the legacy shared tokens survive for persisted dash.chart payloads',
    ['15', '60', '360', '1440', '10080', '43200', '129600', '525600'].every(token => w.eval(`tfEntry(${JSON.stringify(token)}) !== null`)));
  check('all and the four calendar tokens are the only non-duration frames',
    frames.filter(f => f.kind !== 'duration').map(f => f.token).join() === 'all,today,yesterday,week,month' &&
    w.eval('tfEntry("all").kind') === 'all' &&
    ['today', 'yesterday', 'week', 'month'].every(token => w.eval(`tfEntry(${JSON.stringify(token)}).kind`) === 'range'));
  check('unknown tokens deny by default',
    w.eval('tfEntry("bogus") === null && tfAvailable("bogus", 0) === false && tfResolve("bogus", 0) === null'));

  // ---- resolution under a stubbed local clock (May 13 2026 14:30:15 local) ----
  const NOW = new Date(2026, 4, 13, 14, 30, 15).getTime();
  const midnight = new Date(2026, 4, 13).getTime();
  const yesterdayMidnight = new Date(2026, 4, 12).getTime();
  const monday = new Date(2026, 4, 11).getTime();
  const monthStart = new Date(2026, 4, 1).getTime();
  check('the stubbed clock pins a midweek mid-month local instant',
    new Date(NOW).getDay() === 3 && NOW > midnight);
  w.eval(`tfSetExtent(${NOW - 400 * 86400000})`);
  check('duration tokens resolve to their minute window',
    (() => {
      const r = w.eval(`tfResolve('60', ${NOW})`);
      return r.kind === 'duration' && r.minutes === 60 && r.label === 'last 1h' && r.long === '1 hour';
    })());
  check('all resolves to the all-time window',
    (() => {
      const r = w.eval(`tfResolve('all', ${NOW})`);
      return r.kind === 'all' && r.label === 'all time' && r.long === 'all time';
    })());
  check('today resolves to local midnight through now',
    (() => {
      const r = w.eval(`tfResolve('today', ${NOW})`);
      return r.kind === 'range' && r.fromMs === midnight && r.toMs === NOW && r.label === 'today';
    })());
  check('yesterday resolves to the previous local day',
    (() => {
      const r = w.eval(`tfResolve('yesterday', ${NOW})`);
      return r.fromMs === yesterdayMidnight && r.toMs === midnight;
    })());
  check('this week resolves to Monday local midnight through now',
    (() => {
      const r = w.eval(`tfResolve('week', ${NOW})`);
      return r.fromMs === monday && r.toMs === NOW && r.label === 'this week';
    })());
  check('this month resolves to the first local midnight through now',
    (() => {
      const r = w.eval(`tfResolve('month', ${NOW})`);
      return r.fromMs === monthStart && r.toMs === NOW;
    })());
  w.eval(`tfSetExtent(${new Date(2026, 4, 13, 9, 0).getTime()})`);
  check('a calendar start clamps to a later stored extent',
    (() => {
      const r = w.eval(`tfResolve('today', ${NOW})`);
      return r.fromMs === new Date(2026, 4, 13, 9, 0).getTime() && r.toMs === NOW;
    })());
  w.eval(`tfSetExtent(${midnight})`);
  check('a calendar token whose clamped span is empty is unavailable',
    w.eval(`tfAvailable('yesterday', ${NOW}) === false`) &&
    (() => {
      const r = w.eval(`tfResolve('yesterday', ${NOW})`);
      return r.fromMs >= r.toMs;
    })());
  check('a missing or malformed extent keeps the last known one', (() => {
    w.eval(`tfSetExtent(${NOW - 3600000})`);
    w.eval('tfSetExtent(undefined); tfSetExtent(NaN); tfSetExtent(-5)');
    return w.eval('tfExtentFrom()') === NOW - 3600000;
  })());

  // ---- availability ----
  w.eval(`tfSetExtent(${NOW - 3 * 3600000 - 12 * 60000})`);
  check('a duration is available only while it is no wider than the stored history',
    w.eval(`tfAvailable('180', ${NOW}) === true && tfAvailable('360', ${NOW}) === false`));
  w.eval(`tfSetExtent(${NOW - 3 * 3600000})`);
  check('a duration exactly as wide as the stored history stays available',
    w.eval(`tfAvailable('180', ${NOW}) === true`));
  check('all is always available', w.eval(`tfAvailable('all', ${NOW}) === true`));
  w.eval('tfSetExtent(0)');
  check('a zero extent disables everything but all',
    w.eval(`tfAvailable('1', ${NOW}) === false && tfAvailable('today', ${NOW}) === false && tfAvailable('all', ${NOW}) === true`));
  // Before the first payload the page has no extent fact; a restored
  // selection must not be gated by it.
  w.eval('tfExtent = {from: 0, known: false}');
  check('an unreported extent does not gate a restored selection',
    w.eval(`tfAvailable('60', ${NOW}) === true`));

  // ---- the built control ----
  const trigger = d.createElement('button');
  trigger.type = 'button';
  trigger.id = 'tf-test-trigger';
  trigger.setAttribute('aria-expanded', 'false');
  d.body.appendChild(trigger);
  const label = d.createElement('span');
  trigger.appendChild(label);
  const picks = [];
  const ctrl = w.buildTimeframeControl(trigger, label, token => picks.push(token));
  const panel = ctrl.panel;
  const chip = token => panel.querySelector(`[data-tf="${token}"]`);
  const fill = panel.querySelector('.mn-rail-fill');
  const note = panel.querySelector('.mn-note');
  try {
    check('the control builds a Timeframe group panel on the foundation shell',
      panel.classList.contains('mn') && panel.getAttribute('role') === 'group' &&
      panel.getAttribute('aria-label') === 'Timeframe' && panel.hidden &&
      panel.style.getPropertyValue('--mn-w') === '320px' &&
      trigger.getAttribute('aria-controls') === panel.id &&
      !!panel.querySelector('.mn-body') && !!panel.querySelector('.mn-title'));
    check('the panel carries one chip per token in zoom and calendar groups',
      [...panel.querySelectorAll('.mn-chip')].map(c => c.dataset.tf).join() === w.eval('TIMEFRAMES.map(f => f.token).join(",")') &&
      [...panel.querySelectorAll('.mn-sec-h')].map(h => h.textContent).join(',') === 'zoom,calendar');
    check('every chip carries its short vocabulary label',
      [...panel.querySelectorAll('.mn-chip')].map(c => c.textContent).join() === w.eval('TIMEFRAMES.map(f => f.short).join(",")'));

    // 3h12m of stored history.
    w.eval(`tfSetExtent(${Date.now() - 3 * 3600000 - 12 * 60000})`);
    ctrl.sync('all');
    check('unavailable chips disable with their coverage reason',
      chip('360').disabled && chip('360').title === 'only ' + w.eval('fmtDur(3 * 3600000 + 12 * 60000)') + ' of history is stored' &&
      !chip('180').disabled && !chip('60').disabled && !chip('all').disabled &&
      chip('60').title === 'last 1 hour');
    ctrl.sync('180');
    check('the selected chip is pressed and the trigger label resolves short',
      chip('180').getAttribute('aria-pressed') === 'true' && chip('all').getAttribute('aria-pressed') === 'false' &&
      label.textContent === 'last 3h');
    check('the duration rail fills the last window of the stored coverage',
      !fill.hidden &&
      Math.abs(parseFloat(fill.style.width) - (180 * 60000 / (3 * 3600000 + 12 * 60000)) * 100) < 0.01 &&
      Math.abs(parseFloat(fill.style.left) - (100 - parseFloat(fill.style.width))) < 1e-6);
    check('the duration note names the window and the stored coverage',
      note.textContent === '3 hours · ' + w.eval('fmtDur(3 * 3600000 + 12 * 60000)') + ' of history');
    ctrl.sync('all');
    check('all time presses the all chip, fills the rail and reads the coverage',
      chip('all').getAttribute('aria-pressed') === 'true' &&
      parseFloat(fill.style.width) === 100 && parseFloat(fill.style.left) === 0 &&
      /^3\.2h of history · since (\d{2}:\d{2}|[A-Za-z]+ \d{1,2})$/.test(note.textContent));

    // A ten-day extent gives the calendar spans room inside the rail.
    w.eval(`tfSetExtent(${Date.now() - 10 * 86400000})`);
    ctrl.sync('today');
    check('today fills from local midnight to the rail end and stamps its start',
      label.textContent === 'today' && chip('today').getAttribute('aria-pressed') === 'true' &&
      !fill.hidden && Math.abs(parseFloat(fill.style.left) + parseFloat(fill.style.width) - 100) < 0.01 &&
      note.textContent === 'today · since 00:00');
    ctrl.sync('yesterday');
    check('yesterday fills its fixed local span inside the rail', (() => {
      const ext = w.eval('tfExtentFrom()');
      const nowMs = Date.now();
      const total = nowMs - ext;
      const today = new Date(nowMs);
      const from = new Date(today.getFullYear(), today.getMonth(), today.getDate() - 1).getTime();
      const to = new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime();
      const wantLeft = ((from - ext) / total) * 100;
      const wantWidth = ((to - from) / total) * 100;
      return chip('yesterday').getAttribute('aria-pressed') === 'true' &&
        Math.abs(parseFloat(fill.style.left) - wantLeft) < 0.01 &&
        Math.abs(parseFloat(fill.style.width) - wantWidth) < 0.01 &&
        /^yesterday · since \S+ \d{1,2}$/.test(note.textContent);
    })());

    w.eval('tfSetExtent(0)');
    ctrl.sync('all');
    check('a zero extent hides the fill and reads the no-history note',
      fill.hidden && note.textContent === 'no stored history yet' &&
      chip('60').disabled && !chip('all').disabled && chip('all').getAttribute('aria-pressed') === 'true');
    w.eval('tfExtent = {from: 0, known: false}');
    ctrl.sync('all');
    check('an unreported extent shows the waiting note without gating chips',
      fill.hidden && note.textContent === 'waiting for history' && !chip('60').disabled);

    // A chip click reports its token to the caller and never fetches itself.
    w.eval(`tfSetExtent(${Date.now() - 3 * 3600000})`);
    ctrl.sync('all');
    const chartFetches = () => state.initialFetches.filter(u => u.includes('/metrics/agg/chart')).length;
    const fetchesBefore = chartFetches();
    chip('60').click();
    check('a chip click fires onPick with its token and the control never fetches',
      picks.join() === '60' && chip('60').getAttribute('aria-pressed') === 'true' &&
      label.textContent === 'last 1h' && chartFetches() === fetchesBefore);
    chip('360').click();
    check('a disabled chip cannot be picked',
      picks.join() === '60' && chip('360').getAttribute('aria-pressed') === 'false');

    ctrl.open();
    check('opening the control shows the panel and syncs the expanded state',
      !panel.hidden && panel.classList.contains('mn-on') && trigger.getAttribute('aria-expanded') === 'true');
    ctrl.close();
    check('closing the control hides the panel again',
      panel.hidden && trigger.getAttribute('aria-expanded') === 'false');
    trigger.click();
    check('the trigger click toggles the control', !panel.hidden && trigger.getAttribute('aria-expanded') === 'true');
  } finally {
    w.menuCloseAll();
    panel.remove();
    trigger.remove();
    // Restore the shared page's extent from its own fixture payload.
    w.eval('tfSetExtent(' + state.chartPayload.data_from_ms + ')');
  }
};
