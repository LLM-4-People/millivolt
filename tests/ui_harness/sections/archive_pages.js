// test 10a-cap: the durable archive is ascending oldest-to-newest, so
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function archivePages(H) {
  const {BIG, check, d, fire, mkRec, rows, settleUntil, w} = H;
  const state = H.state;

  // ---- test 10a-cap: the durable archive is ascending oldest-to-newest, so
  // the 20,000-row cap must trim its oldest edge and keep the newest archive
  // rows contiguous with the live ring. Trimming the tail used to drop the
  // newest record, including the boundary row beside the ring.
  {
    const saved = {fetch: w.fetch, fill: w.scheduleLogFill, page: state.logPage,
      data: w.eval('lastData')};
    w.scheduleLogFill = () => {};
    const base = 1600000000000;
    w.__arcArchive = Array.from({length: 20000}, (_, i) =>
      mkRec('arc' + String(i).padStart(5, '0'), 200, base + i * 1000));
    w.__arcRing = mkRec('arc-ring', 200, base + 20000 * 1000);
    w.eval('logArchive = window.__arcArchive; logCursor = {ms: 1600000000000, id: "arc00000"}; ' +
      'logLimit = 0; logExhausted = false; logFetching = false; logFailures = 0; logFetchFailed = false;');
    state.logPage = {records: [mkRec('arc-old', 200, base - 1000)], more: false,
      cursor_ms: base - 1000, cursor_id: 'arc-old'};
    w.fetchOlderLog();
    await settleUntil(() => w.eval('logArchive.length') === 20000 &&
      w.eval('logArchive[19999].id') === 'arc19999' && !w.eval('logFetching'));
    const archived = w.eval('logArchive');
    let contiguous = true;
    for (let i = 1; i < archived.length; i++) {
      if (Number(archived[i].id.slice(3)) !== Number(archived[i - 1].id.slice(3)) + 1) {
        contiguous = false;
        break;
      }
    }
    check('the archive cap keeps the newest 20,000 rows (older page dropped)',
      archived.length === 20000 && archived[0].id === 'arc00000' &&
      archived[19999].id === 'arc19999' && !archived.some(r => r.id === 'arc-old'));
    check('the retained archive remains one contiguous newest window', contiguous);
    const merged = w.mergeLogArchive([w.eval('window.__arcRing')]);
    check('newest archive rows stay adjacent to the live ring with no boundary gap',
      merged.length === 20001 && merged[19998].id === 'arc19998' &&
      merged[19999].id === 'arc19999' && merged[20000].id === 'arc-ring');
    // The archive mutation owner prepends below the retained window and trims
    // its oldest edge when the cap overflows; the identity set rebuilds exactly
    // once for each real mutation.
    w.eval('logArchive.splice(0, 1);'); // 19,999 rows: make room below the window
    w.logWindowLength(w.eval('derive(lastData).scopedRecs'));
    const scansBeforePrepend = w.eval('_logArchiveScans');
    w.logArchivePrepend([mkRec('arc00000', 200, base)]);
    w.logWindowLength(w.eval('derive(lastData).scopedRecs'));
    check('an archive prepend keeps server order below the ring and rescans once',
      w.eval('logArchive.length') === 20000 &&
      w.eval('logArchive[0].id') === 'arc00000' &&
      w.eval('logArchive[19999].id') === 'arc19999' &&
      w.eval('_logArchiveScans') === scansBeforePrepend + 1);
    const scansBeforeTrim = w.eval('_logArchiveScans');
    w.logArchivePrepend([mkRec('arc-older', 200, base - 1000)]);
    w.logWindowLength(w.eval('derive(lastData).scopedRecs'));
    check('an archive prepend at the cap trims the oldest edge and rescans once',
      w.eval('logArchive.length') === 20000 &&
      w.eval('logArchive[0].id') === 'arc00000' &&
      w.eval('logArchive[19999].id') === 'arc19999' &&
      w.eval('_logArchiveScans') === scansBeforeTrim + 1);
    w.fetch = saved.fetch;
    w.scheduleLogFill = saved.fill;
    state.logPage = saved.page;
    w.resetLogWindow();
    w.renderAll(saved.data);
    for (const key of ['__arcArchive', '__arcRing']) delete w[key];
  }

  // ---- test 10a-retry: a failed older-page fetch retries on a delayed,
  // bounded owner and reports "history unavailable"; success clears it.
  {
    const saved = {fetch: w.fetch, fill: w.scheduleLogFill, page: state.logPage,
      data: w.eval('lastData'), status: w.eval('filters.status')};
    w.scheduleLogFill = () => {};
    w.eval("filters.status = '';");
    w.resetLogWindow();
    w.eval('logFetching = false; logExhausted = false; logCursor = null;');
    w.__retryPage = mkRec('retry-ok', 200, 1599990000000);
    state.logPage = {records: [w.eval('window.__retryPage')], more: false,
      cursor_ms: 1599990000000, cursor_id: 'retry-ok'};
    const originalTimeout = w.setTimeout;
    const retryCallbacks = [];
    let calls = 0;
    let failing = true;
    w.fetch = (url, options) => {
      if (!String(url).includes('/metrics/agg/log')) return saved.fetch(url, options);
      calls++;
      if (failing) return Promise.resolve({ok: false, status: 503, json: async () => ({error: 'history down'})});
      return saved.fetch(url, options);
    };
    w.setTimeout = (fn, ms, ...args) => {
      if (ms === w.eval('LOG_RETRY_MS')) {
        retryCallbacks.push(() => fn(...args));
        return 7100 + retryCallbacks.length;
      }
      return originalTimeout(fn, ms, ...args);
    };
    try {
      w.fetchOlderLog();
      await settleUntil(() => calls === 1 && !w.eval('logFetching') && w.eval('logFetchFailed'));
      check('a failed older-page fetch shows history unavailable',
        d.getElementById('f-stats').textContent.includes('history unavailable'));

      let retries = 0;
      while (retryCallbacks.length && retries < 10) {
        const next = retryCallbacks.shift();
        const before = calls;
        retries++;
        next();
        await settleUntil(() => calls > before && !w.eval('logFetching'));
      }
      check('failed history fetches retry automatically but stop at the bounded budget',
        calls === 4 && retries === w.eval('LOG_RETRY_LIMIT') &&
        retryCallbacks.length === 0 && w.eval('logFailures') > w.eval('LOG_RETRY_LIMIT'));
      check('the exhausted retry budget keeps the visible failure state',
        d.getElementById('f-stats').textContent.includes('history unavailable'));

      failing = false;
      w.fetchOlderLog(); // a user scroll after the budget may try again
      await settleUntil(() => calls === 5 && !w.eval('logFetchFailed') &&
        !w.eval('logFetching') && rows().some(tr => tr.dataset.id === 'retry-ok'));
      check('a user-triggered retry after the budget appends rows and clears the failure line',
        !d.getElementById('f-stats').textContent.includes('history unavailable') &&
        !w.eval('logFetchFailed') && w.eval('logFailures') === 0);
    } finally {
      w.setTimeout = originalTimeout;
      w.fetch = saved.fetch;
      w.scheduleLogFill = saved.fill;
      state.logPage = saved.page;
      w.eval('filters.status = ' + JSON.stringify(saved.status) + ';');
      w.resetLogWindow();
      w.renderAll(saved.data);
      delete w.__retryPage;
    }
  }

  // ---- test 10b: the purge re-sync - no removal event exists, so the
  // whole-database delete (now the settings sheet's storage action) must
  // fetch a CURSOR-LESS bootstrap (mode 'full': a resume could never see
  // the deletions) and the empty post-purge snapshot IS the wipe render.
  state.fullPayload = { feed_id: 'feedB', seq: 0, records: [],
    counters: { in_flight: 0, total_requests: 0, total_errors: 0 } };
  const origPurge = w.fetch;
  w.fetch = (url, opts) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) { state.bootURL = u; return origPurge(url, opts); }
    if (u.includes('/admin/purge') && !u.includes('/count')) { purgePOST = true; }
    return origPurge(url, opts);
  };
  let purgePOST = false;
  let confirmText = '';
  const origConfirm = w.confirm;
  w.confirm = m => { confirmText = String(m); return true; };
  await w.purgeAll();
  await settleUntil(() =>
    state.bootURL.includes('/metrics/bootstrap') && rows().length === 0 &&
    d.getElementById('settings-count').textContent === 'all metrics history deleted');
  w.confirm = origConfirm;
  w.fetch = origPurge;
  check('the settings purge POSTs the purge endpoint', purgePOST === true);
  check('the settings purge confirms the whole-database wipe without the log-view count',
    confirmText.includes('The entire database will be wiped.') && !confirmText.includes('records in view'));
  check('purge resync bootstrap is cursor-less (no since/feed)',
    state.bootURL.includes('/metrics/bootstrap') && !state.bootURL.includes('since=') && !state.bootURL.includes('feed='));
  check('the empty post-purge snapshot IS the wipe render', rows().length === 0);
  check('purge resets the log window state', w.eval('logArchive.length') === 0 && w.eval('logLimit') === 0 &&
    w.eval('_logArchiveIndex') === null && w.eval('_logLiveIndex') === null);
  check('the settings purge reports completion through the sheet status',
    d.getElementById('settings-count').textContent === 'all metrics history deleted' &&
    d.getElementById('settings-count').classList.contains('ok'));
  // Restore a live view so the later suites run against real state.
  state.fullPayload = { feed_id: 'feedB', seq: 80,
    records: BIG.map((r, i) => ({ ...r, __seq: i + 1 })),
    counters: { in_flight: 0, total_requests: 80, total_errors: 0 } };
  w.eval("fetchBootstrap('full')");
  await settleUntil(() => rows().length === 60);
  check('post-purge resync restores the fresh snapshot', rows().length === 60);
  // 10b's wrapper records ANY bootstrap URL, so the page's 5s tick stays
  // stopped through the whole purge-resync window: a tick resume landing
  // after the purge's cursor-less resync overwrites bootURL with since/feed
  // and fails that check even though the product satisfied it. Re-armed
  // here, after the restore check, like the 7b/7c windows above.
  w.eval('armDashboardTicks()');

  // ---- test 10c: a scope whose records predate the ring (a provider the
  // user hasn't touched in a while) must page the store from its NEWEST rows -
  // the empty scoped window probes the durable newest edge without a ring
  // timestamp cutoff instead of dead-ending on "no matching requests".
  state.logPage = {
    records: [
      { ...mkRec('stale1', 200, 1600000000000), provider: 'stale.example' },
      { ...mkRec('stale0', 200, 1599999999000), provider: 'stale.example' },
    ],
    more: false, cursor_ms: 1599999999000,
  };
  // empty box: content does not fill it → "near bottom" is true, like a real
  // browser with a zero-row table
  Object.defineProperty(state.box, 'clientHeight', { configurable: true, get() { return 200; } });
  Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 200; } });
  w.navigateTo([{ dim: 'provider', id: 'stale.example' }]);
  // Wait for the page's own end state (fetch dispatched, both rows painted,
  // stats filled) rather than a fixed sleep: under load the scoped fetch had
  // not resolved after 50ms, so a scheduling delay failed three checks that
  // the product actually satisfied.
  await settleUntil(() =>
    state.lastLogURL.includes('f=provider%3Astale.example') && rows().length === 2 &&
    rows()[0].dataset.id === 'stale1' && rows()[1].dataset.id === 'stale0' &&
    d.getElementById('f-stats').textContent === '2 records');
  check('stale-provider scope probes the store at the newest edge',
    !state.lastLogURL.includes('before_ms=') && state.lastLogURL.includes('f=provider%3Astale.example'));
  check('stale-provider rows paint instead of the empty state',
    rows().length === 2 && rows()[0].dataset.id === 'stale1' && rows()[1].dataset.id === 'stale0');
  check('stale-provider fill updates the log stats', d.getElementById('f-stats').textContent === '2 records');
  check('stale-provider scope line comes from the server count',
    d.getElementById('f-scope').textContent.includes('of'));

  // A page that resolves after a window reset belongs to the OLD scope and
  // must be dropped - never merged into the new one.
  let gateResolve = null;
  state.logPageGate = new Promise(res => { gateResolve = res; });
  w.navigateTo([{ dim: 'provider', id: 'other.example' }]); // resetLogWindow → fetch fires, held by the gate
  await settleUntil(() => state.lastLogURL.includes('f=provider%3Aother.example'));
  state.logPage = { records: [{ ...mkRec('other1', 200, 1599999980000), provider: 'other.example' }], more: false, cursor_ms: 1599999980000 };
  gateResolve(); // both held pages resolve - only the current scope's may land
  await settleUntil(() => rows().length === 1 && rows()[0].dataset.id === 'other1' &&
    w.eval('logArchive.length') === 1);
  check('stale in-flight page is dropped after a scope change',
    rows().length === 1 && rows()[0].dataset.id === 'other1' && w.eval('logArchive.length') === 1);
  state.logPageGate = null;
  // Restore the tall-box stubs (test 10's geometry) before leaving the suite.
  Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 4000; } });
  w.navigateTo([]);
  await settleUntil(() => rows().length === 60);
  check('clearing the scope restores the unfiltered window', rows().length === 60);

  // ---- test 10c-bis: per-view history rows ----
  // A selected view owns its durable history rows: the scoped window must
  // page the store under its own scope and render exactly its own rows (its
  // scoped ring rows plus its archive page, never the default view's rows),
  // and a view's history must not leak back into the default window.
  {
    const mixed = [
      mkRec('p-row0', 200, 1700000000000),
      { ...mkRec('sa-ring0', 200, 1700000100000), provider: 'scope-a.example' },
      mkRec('p-row1', 200, 1700000200000),
      { ...mkRec('sa-ring1', 200, 1700000300000), provider: 'scope-a.example' },
      mkRec('p-row2', 200, 1700000400000),
      { ...mkRec('sa-ring2', 200, 1700000500000), provider: 'scope-a.example' },
      mkRec('p-row3', 200, 1700000600000),
    ];
    fire('snapshot', { feed_id: 'feedB', seq: 90, incremental: false,
      records: mixed, counters: { in_flight: 0, total_requests: 7, total_errors: 0 } });
    // Wait for each fixture's own painted state instead of a fixed sleep: a
    // loaded runner can stretch a fetch/render round trip past any hard time.
    await settleUntil(() => rows().length === 7);
    check('the mixed ring paints the default window', rows().length === 7);
    state.logPage = {
      records: [
        { ...mkRec('sa-old1', 200, 1699999900000), provider: 'scope-a.example' },
        { ...mkRec('sa-old0', 200, 1699999800000), provider: 'scope-a.example' },
      ],
      more: false, cursor_ms: 1699999800000,
    };
    Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 200; } });
    w.navigateTo([{ dim: 'provider', id: 'scope-a.example' }]);
    await settleUntil(() =>
      rows().map(tr => tr.dataset.id).join() === 'sa-ring2,sa-ring1,sa-ring0,sa-old1,sa-old0' &&
        w.eval('logArchive.length') === 2);
    const scopedIds = rows().map(tr => tr.dataset.id);
    check('the scoped view renders its own ring rows plus its own history rows',
      scopedIds.join() === 'sa-ring2,sa-ring1,sa-ring0,sa-old1,sa-old0' &&
        w.eval('logArchive.length') === 2);
    Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 4000; } });
    state.logPage = { records: [], more: false, cursor_ms: 0 };
    w.navigateTo([]);
    await settleUntil(() =>
      w.eval('logArchive.length') === 0 && rows().length === 7 &&
        rows().some(tr => tr.dataset.id === 'p-row3') &&
        rows().some(tr => tr.dataset.id === 'sa-ring2') &&
        !rows().some(tr => tr.dataset.id.startsWith('sa-old')));
    check('the default view renders its own ring rows without the scope\'s history',
      w.eval('logArchive.length') === 0 && rows().length === 7 &&
        rows().some(tr => tr.dataset.id === 'p-row3') &&
        rows().some(tr => tr.dataset.id === 'sa-ring2') &&
        !rows().some(tr => tr.dataset.id.startsWith('sa-old')));
  }
};
