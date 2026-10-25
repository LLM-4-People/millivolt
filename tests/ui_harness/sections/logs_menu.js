// test 8: Logs menu (filter menu + export URL)
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function logsMenu(H) {
  const {JSDOM, STATIC, check, d, fire, fs, mkRec, path, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 8: Logs menu (filter menu + export URL) ----
  const logsMenu = d.getElementById('logs-menu');
  check('logs menu starts hidden', logsMenu && logsMenu.hidden);
  w.toggleLogsMenu({ stopPropagation() {} });
  check('logs menu opens', !logsMenu.hidden);
  check('logs open sets aria-expanded', d.getElementById('btn-logs').getAttribute('aria-expanded') === 'true');
  // The whole Clear control moved into the settings sheet's storage
  // category: the header must not keep the button, the menu, or a
  // HEADER_MENUS registration for either.
  check('the header carries no clear control',
    !d.getElementById('btn-clear') && !d.getElementById('clear-menu'));
  check('the header menu registry carries no clear entry',
    !w.eval('Object.entries(HEADER_MENUS).some(([b, m]) => b === "btn-clear" || m === "clear-menu")'));
  check('logs menu populated from records', d.querySelector('#lf-provider option[value="p"]') !== null);
  // ---- R25 L4-1 freeze row: the export has been a gzip artifact with no
  // plain-JSON fallback since wave 17, so the menu header must say so.
  // Exact-text pin (like the debug menu header's) so the pre-compression
  // "(JSON)" label cannot return.
  check('logs menu header states the gzip export format',
    d.querySelector('#logs-menu .clear-menu-hd').textContent === 'Download logs (JSON, gzip)…');
  // ---- L4-C8 pin: the Logs menu (lf-) and the settings sheet's
  // delete-matching block (del-) are one row set modulo the id prefix.
  // The row lists must stay identical (same label texts, same per-row
  // select ids after the prefix), and the runtime fill
  // (populateFilterMenu, already prefix-parameterized) must write the
  // same option values into both surfaces from one record set.
  {
    const skelDoc = new JSDOM(fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8')).window.document;
    const filterRows = root => [...root.querySelectorAll('label')].map(l => [
      l.textContent.replace(/\s+/g, ' ').trim(),
      l.querySelector('select').id.replace(/^(lf|del)-/, ''),
    ]);
    const lfRows = filterRows(skelDoc.getElementById('logs-menu'));
    const delHost = d.createElement('div');
    delHost.innerHTML = w.eval('settingsDeleteRowHTML()');
    d.body.appendChild(delHost);
    try {
      const delRows = filterRows(delHost);
      check('the logs menu and the settings delete block render one prefix-identical filter row set',
        lfRows.length === 7 && JSON.stringify(lfRows) === JSON.stringify(delRows));
      w.populateFilterMenu('del');
      const optValues = pre => ['provider', 'model', 'client', 'status', 'errors', 'debug', 'age']
        .map(name => [...d.getElementById(pre + '-' + name).options].map(o => o.value));
      check('both filter surfaces fill the same option values per row',
        JSON.stringify(optValues('lf')) === JSON.stringify(optValues('del')));
    } finally { delHost.remove(); }
  }
  check('export URL encodes the filter', w.logsExportURL({ provider: 'openai', status_code: 429, has_error: false, debug: false, before_ms: 0, model: '', client: '' }) === '/metrics/export?provider=openai&status_code=429');
  check('empty filter exports everything', w.logsExportURL({ provider: '', model: '', client: '', status_code: 0, has_error: false, debug: false, before_ms: 0 }) === '/metrics/export');
  check('debug-only export encodes the flag', w.logsExportURL({ provider: '', model: '', client: '', status_code: 0, has_error: false, debug: true, before_ms: 0 }) === '/metrics/export?debug=1');
  d.getElementById('lf-provider').value = 'p';
  w.updateLogsCount();
  await sleep(30);
  check('logs count preview reads the backend count', d.getElementById('logs-count').textContent.includes('download 3 records'));
  w.toggleLogsMenu({ stopPropagation() {} });
  check('logs menu closes on second toggle', logsMenu.hidden);
  check('logs close clears aria-expanded', d.getElementById('btn-logs').getAttribute('aria-expanded') === 'false');
  // equal-size icon buttons: all six header actions share one min-width
  const hdrIds = ['btn-pause', 'btn-debug', 'btn-limits', 'btn-logs', 'btn-restart', 'btn-settings'];
  const bts = hdrIds.map(id => d.getElementById(id));
  const styles = bts.map(b => w.getComputedStyle(b).minWidth);
  check('header action buttons share one min-width', styles.every(s => s === styles[0] && s) && styles[0] !== 'auto');
  check('header actions are icon-only', bts.every(b => b && b.querySelector('svg.hdr-ico') && b.getAttribute('aria-label')));
  check('header hamburger exists', !!d.getElementById('btn-nav') && d.getElementById('btn-nav').getAttribute('aria-controls') === 'hdr-actions');
  check('header hamburger is hidden on desktop', w.getComputedStyle(d.getElementById('btn-nav')).display === 'none');
  check('header action labels are present for the mobile panel', bts.every(b => b.querySelector('.hdr-btn-label')));
  w.refreshFooterState();
  check('pause icon survives pressed-state refresh', !!d.getElementById('btn-pause').querySelector('svg.hdr-ico'));
  check('debug icon survives pressed-state refresh', !!d.getElementById('btn-debug').querySelector('svg.hdr-ico'));
  const debugMenu = d.getElementById('debug-menu');
  check('debug menu starts hidden', debugMenu && debugMenu.hidden);
  w.toggleDebugMenu({ stopPropagation() {} });
  check('debug menu opens', !debugMenu.hidden);
  check('debug open sets aria-expanded', d.getElementById('btn-debug').getAttribute('aria-expanded') === 'true');
  check('opening debug closes logs', logsMenu.hidden);
  w.toggleDebugMenu({ stopPropagation() {} });
  check('debug menu closes on second toggle', debugMenu.hidden);
  // Stamped-first debug priority: a request row's dbg pill must edit the
  // session the server STAMPED on the record (debug_session_id), even when
  // another live session's scope would also match the record. The stamp is
  // the authoritative match; the raw-scope heuristic only serves records
  // the stamp does not reach.
  fire('record', { ...mkRec('dbg-stamped-rec'), debug: true, debug_session_id: 'dbg-stamped' }, '5');
  // The record upsert schedules the coalesced rAF render; poll for the
  // painted end state (the row's dbg pill) instead of a fixed sleep.
  await settleUntil(() => !!d.querySelector('#tbl-requests tr.exp-row[data-id="dbg-stamped-rec"] .pill.debug[data-edit-debug]'));
  check('a stamped debug record renders the dbg pill on its row',
    !!d.querySelector('#tbl-requests tr.exp-row[data-id="dbg-stamped-rec"] .pill.debug[data-edit-debug]'));
  w.eval("debugState = {...debugState, enabled: true, sessions: [" +
    "{id: 'dbg-stamped', clients: ['other-client'], duration: '15m'}," +
    "{id: 'dbg-scope', clients: ['c'], duration: '1h'}]}");
  d.querySelector('#tbl-requests tr.exp-row[data-id="dbg-stamped-rec"] .pill.debug[data-edit-debug]').click();
  {
    const holds = [...d.querySelectorAll('#debug-menu .pause-hold')];
    const editing = holds.filter(h => h.classList.contains('editing'));
    check('the dbg pill edits the stamped session, not a scope-matching one',
      !debugMenu.hidden && holds.length === 2 && editing.length === 1 &&
        editing[0].querySelector('[data-operator="debug-edit"]').dataset.value === 'dbg-stamped' &&
        holds.some(h => !h.classList.contains('editing') &&
          h.querySelector('[data-operator="debug-edit"]').dataset.value === 'dbg-scope') &&
        d.querySelector('#debug-menu .clear-menu-hd').textContent === 'Edit session…');
  }
  w.resetDebugMenuForm();
  w.toggleDebugMenu({ stopPropagation() {} });
  w.eval("debugState = {...debugState, enabled: false, sessions: []}");
  check('debug menu closes after the stamped edit', debugMenu.hidden);
  // GAP-A pin: the pill-edit table's paused and throttled arms. The debug
  // arm is pinned above; these two had zero coverage (mutation-proven).
  // Same contract per arm: a live record carrying the server's state flag
  // renders its state pill, and the pill opens the matching operator editor
  // - the pause menu editing the hold matched via holdMatchesRecord (not
  // the all-requests fallback, not a non-matching named hold), the limits
  // menu editing the record's own provider among several.
  {
    const pausedRec = { ...mkRec('paused-live-rec', 0, 1700000105000), paused: true };
    fire('begin', { record: pausedRec, in_flight: 1 });
    await sleep(20);
    const pausedPill = () => d.querySelector('#tbl-requests tr.exp-row[data-id="paused-live-rec"] .pill.paused[data-edit-pause]');
    check('a paused live record renders the paused pill on its row', !!pausedPill());
    w.eval("pauseState = {...pauseState, paused: true, holds: [" +
      "{id: 'hold-other', clients: ['other-client'], duration: '1h'}," +
      "{id: 'hold-named', clients: ['c'], duration: '15m'}," +
      "{id: 'hold-all', all: true, duration: '6h'}]}");
    pausedPill().click();
    {
      const pauseMenu = d.getElementById('pause-menu');
      const holds = [...pauseMenu.querySelectorAll('.pause-hold')];
      const editing = holds.filter(h => h.classList.contains('editing'));
      check('the paused pill edits the hold matched via holdMatchesRecord',
        !pauseMenu.hidden && holds.length === 3 && editing.length === 1 &&
          editing[0].querySelector('[data-operator="pause-edit"]').dataset.value === 'hold-named');
    }
    w.resetPauseMenuForm();
    w.hideHdrMenu('pause-menu');
    w.eval("pauseState = {...pauseState, paused: false, holds: []}");

    const throttledRec = { ...mkRec('throttled-live-rec', 0, 1700000106000), throttled: true };
    fire('begin', { record: throttledRec, in_flight: 2 });
    await sleep(20);
    const throttledPill = () => d.querySelector('#tbl-requests tr.exp-row[data-id="throttled-live-rec"] .pill.throttled[data-edit-limit]');
    check('a throttled live record renders the throttled pill on its row', !!throttledPill());
    w.eval("throttleState = {...throttleState, active: true, throttles: [" +
      "{provider: 'other.example', requests: 1, request_window: '1m'}," +
      "{provider: 'p', requests: 2, request_window: '2m'}]," +
      "known_providers: ['p', 'other.example']}");
    throttledPill().click();
    {
      const limitsMenu = d.getElementById('limits-menu');
      const holds = [...limitsMenu.querySelectorAll('#lim-holds .pause-hold')];
      const editing = holds.filter(h => h.classList.contains('editing'));
      check('the throttled pill opens the limits editor for the record provider',
        !limitsMenu.hidden && d.getElementById('lim-provider').value === 'p' &&
          holds.length === 2 && editing.length === 1 &&
          editing[0].querySelector('[data-operator="limit-edit"]').dataset.value === 'p');
    }
    w.hideHdrMenu('limits-menu');
    w.eval("throttleState = {...throttleState, active: false, throttles: []}");
  }
  // GAP-C pin: resyncOperatorSurfaces' live-repaint tail. The menu-sync
  // half is pinned (the count-bits checks); `if (lastData)
  // scheduleRenderLive()` is not. The row renderer detects change by record
  // identity, so the tail's contract: after a wholesale record replacement
  // in lastData (what upsert does) with the derivation invalidated but NO
  // render scheduled and no feed event, applying operator state repaints
  // the stale row from the fresh record.
  {
    const liveRec = { ...mkRec('gapc-live-rec', 0, 1700000107000) };
    fire('begin', { record: liveRec, in_flight: 1 });
    await sleep(20);
    const gapcPill = () => d.querySelector('#tbl-requests tr.exp-row[data-id="gapc-live-rec"] .pill');
    check('the in-flight row paints the live pill first', !!gapcPill() && gapcPill().classList.contains('live'));
    w.eval("lastData.records[lastData.records.findIndex(r => r.id === 'gapc-live-rec')]" +
      " = {...lastData.records.find(r => r.id === 'gapc-live-rec'), paused: true}; bumpData();");
    check('no feed event leaves the painted row stale', gapcPill().classList.contains('live'));
    let livePaints = 0;
    const originalLive = w.renderLive;
    w.renderLive = () => { livePaints++; originalLive(); };
    w.applyPauseState({ ok: true, paused: true, holds: [{ id: 'gapc-hold', all: true, duration: '15m' }],
      known_clients: ['c'], known_providers: ['p'], until: null, queued: 0, default_max_queued: 0 });
    await sleep(20);
    w.renderLive = originalLive;
    check('applying a pause state repaints the held row without a feed event',
      livePaints === 1 &&
        !!d.querySelector('#tbl-requests tr.exp-row[data-id="gapc-live-rec"] .pill.paused[data-edit-pause]'));
    w.eval("pauseState = {...pauseState, paused: false, holds: []}");
  }
};
