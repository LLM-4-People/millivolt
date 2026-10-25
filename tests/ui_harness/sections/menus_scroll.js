// test 9: menu action rows (split ends alignment + red all-actions)
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function menusScroll(H) {
  const {BIG, check, d, fire, html, mkRec, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 9: menu action rows (split ends alignment + red all-actions) ----
  w.toggleLogsMenu({ stopPropagation() {} });
  const js = w.getComputedStyle(d.querySelector('#logs-menu .clear-actions'));
  check('menu action row splits ends-wise', js.justifyContent === 'space-between');
  const dlAll = [...d.querySelectorAll('#logs-menu .clear-actions button')].find(b => b.textContent.includes('Download all'));
  check('download all is the red-outline variant', dlAll && dlAll.classList.contains('btn-danger-outline'));
  w.toggleLogsMenu({ stopPropagation() {} });

  // ---- test 9b: every database deletion control lives in the settings
  // sheet's storage category (the header Clear menu is gone): the scoped
  // delete-matching block first, then the whole-database Delete everything
  // row. Both render through the real fillSettingsForm pipeline and
  // behave like every other action row.
  {
    const doc = {
      revision: 'purge-r1',
      fields: [{ key: 'storage_batch_cap', category: 'storage', label: 'Batch cap', help: 'writer batch bound', kind: 'int', hot_reload: true }],
      categories: [{ id: 'storage', label: 'Storage', help: 'durable writes' }],
      values: { storage_batch_cap: 5 },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.__purgeDoc = doc;
    w.eval('settingsDoc = window.__purgeDoc; fillSettingsForm(settingsDoc)');
    const row = d.querySelector('#settings-fields [data-purge="all"]');
    const purgeBtn = d.getElementById('btn-purge-all');
    const delRow = d.querySelector('#settings-fields [data-purge="matching"]');
    const delBtn = d.getElementById('btn-del-matching');
    check('the whole-database delete renders as a storage-category action row',
      !!row && row.dataset.cat === 'storage' && !row.hidden && row.classList.contains('st-block') &&
      !!row.querySelector('.st-backup-actions') &&
      row.querySelector('.st-hint').textContent.includes('permanently delete all metrics history'));
    check('delete everything is the solid-red variant',
      purgeBtn && purgeBtn.classList.contains('btn-danger-solid'));
    check('the scoped delete renders as a storage-category action block before Delete everything',
      !!delRow && delRow.dataset.cat === 'storage' && !delRow.hidden && delRow.classList.contains('st-block') &&
      delRow.querySelectorAll('.st-del-opts select').length === 7 &&
      [...d.querySelectorAll('#settings-fields .st-row[data-cat="storage"]')].indexOf(delRow) <
        [...d.querySelectorAll('#settings-fields .st-row[data-cat="storage"]')].indexOf(row));
    check('delete matching is the light-red variant and starts disabled with an empty preview',
      delBtn && delBtn.classList.contains('btn-danger') && delBtn.disabled &&
      d.getElementById('del-count').textContent === '');
    check('both delete blocks join the rail badge count',
      d.querySelector('#settings-rail [data-st-cat="storage"] .rail-n').textContent === '3');
    const search = d.getElementById('settings-q');
    search.value = 'purge';
    w.filterSettings();
    check('both delete blocks join the search haystack',
      !row.hidden && !delRow.hidden && d.getElementById('settings-pane-hd').textContent.includes('2 matches'));
    search.value = '';
    w.filterSettings();
    d.getElementById('del-provider').value = 'p';
    d.getElementById('del-provider').dispatchEvent(new w.Event('change', { bubbles: true }));
    await settleUntil(() => !delBtn.disabled);
    check('a select change re-counts the live preview through the delegated listener',
      d.getElementById('del-count').textContent === 'will delete 3 records' && !delBtn.disabled);
    purgeBtn.dispatchEvent(new w.Event('input', { bubbles: true }));
    purgeBtn.dispatchEvent(new w.Event('change', { bubbles: true }));
    delBtn.dispatchEvent(new w.Event('input', { bubbles: true }));
    delBtn.dispatchEvent(new w.Event('change', { bubbles: true }));
    const collected = w.collectSettingsValues();
    check('the delete blocks never dirty the form or join value collection',
      w.settingsDirtyCount() === 0 && d.getElementById('btn-settings-revert').disabled &&
      d.getElementById('btn-settings-apply').disabled &&
      Object.keys(collected).length === 1 && collected.storage_batch_cap === 5);
    // Selections reset on every sheet fill: a stale invisible filter must
    // never arm a destructive default.
    w.eval('fillSettingsForm(settingsDoc)');
    check('a sheet refill resets the selection, the preview and the button',
      d.getElementById('del-provider').value === '' &&
      d.getElementById('del-count').textContent === '' &&
      d.getElementById('btn-del-matching').disabled);
  }

  // ---- test 10: infinite-scroll grows the log window, then pages the store ----
  
  fire('snapshot', { feed_id: 'feedB', seq: 80, incremental: false,
    records: BIG, counters: { in_flight: 0, total_requests: 80, total_errors: 0 } });
  check('first paint caps at dash_log_rows (60 of 80)', await settleUntil(() => rows().length === 60));
  check('newest row is on top', rows()[0].dataset.id === 'big079');
  check('f-stats shows window of loaded', d.getElementById('f-stats').textContent === '60 of 80 records');
  {
    const logBox = d.getElementById('tbl-requests');
    let inserted = false, lateScrollReset = false, top = 200;
    const originalUpdate = w.updateSection;
    Object.defineProperty(logBox, 'scrollTop', { configurable: true,
      get: () => top,
      set(value) { if (inserted) lateScrollReset = true; top = value; },
    });
    const rowCache = w.eval('logHTML');
    const cachedRows = [...rowCache];
    rowCache.clear();
    w.resetLogWindow();
    check('first boot does not force layout by resetting an untouched log', top === 200);
    for (const [id, value] of cachedRows) rowCache.set(id, value);
    w.updateSection = (id, html) => {
      if (id === 'tbl-requests') inserted = true;
      return originalUpdate(id, html);
    };
    w.renderAll(w.eval('lastData'));
    check('full render resets scroll before inserting the table, never after it',
      inserted && top === 0 && !lateScrollReset);
    w.updateSection = originalUpdate;
    delete logBox.scrollTop;
    // Drain the real queued fill before checking the shared coalescing gate.
    await sleep(30);
    const originalRAF = w.requestAnimationFrame;
    const originalFill = w.maybeLoadMoreLog;
    let nextFrame, scheduled = 0, probes = 0;
    w.requestAnimationFrame = callback => { scheduled++; nextFrame = callback; return 1; };
    w.maybeLoadMoreLog = () => { probes++; };
    w.scheduleLogFill();
    w.scheduleLogFill();
    logBox.dispatchEvent(new w.Event('scroll'));
    check('boot, append, and scroll share one deferred log fill probe', scheduled === 1 && probes === 0);
    nextFrame();
    check('the coalesced fill probe runs once on the frame', probes === 1);
    w.requestAnimationFrame = originalRAF;
    w.maybeLoadMoreLog = originalFill;
  }
  const pinned = rows()[0];
  state.logPage = {
    records: [
      mkRec('old004', 200, 1700000004000),
      mkRec('old003', 200, 1700000003000),
      mkRec('old002', 200, 1700000002000),
      mkRec('old001', 200, 1700000001000),
      mkRec('old000', 200, 1700000000000),
    ],
    more: false, cursor_ms: 1700000000000,
  };
  state.box = d.getElementById('tbl-requests');
  Object.defineProperty(state.box, 'clientHeight', { configurable: true, get() { return 200; } });
  Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 4000; } });
  state.box.scrollTop = 4000 - 200 - 10;
  state.lastLogURL = '';
  w.maybeLoadMoreLog();
  await settleUntil(() => rows().length === 85);
  const idsAfter = rows().map(tr => tr.dataset.id);
  check('near-bottom grows through the ring then appends store pages', rows().length === 85);
  check('scroll-load keeps existing row nodes', rows().includes(pinned));
  check('older store rows land at the bottom', idsAfter[idsAfter.length - 1] === 'old000');
  check('first log archive page has no cutoff derived from ring arrivals', !state.lastLogURL.includes('before_ms='));
  check('f-stats after fill lists loaded count', d.getElementById('f-stats').textContent === '85 records');
  w.openDrawer('old000');
  check('drawer opens a store-paged row', d.getElementById('drawer').classList.contains('open'));
  fire('record', { ...mkRec('bignew', 200, 1800000000000 + 80 * 1000) }, '81');
  check('live insert stays at top after paging',
    await settleUntil(() => rows()[0].dataset.id === 'bignew'));
  check('paged oldest row is not evicted by a live insert', rows().some(tr => tr.dataset.id === 'old000'));
  // The Clear/Logs preview and durable-pagination suites below install
  // URL-agnostic fetch wrappers and assert exact fetch counts (a failed
  // deletion resyncs nothing; every pagination URL is the test's own). The
  // page's 5s tick must not land inside them: 7b/7c stop the same timer for
  // their hand-driven cursors, and a captured bootstrap is applied as a log
  // page (or counted as a resync or post), failing checks the product
  // satisfies. Re-armed after the purge-resync window.
  w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
  state.box.scrollTop = 0; // full rebuild resets scroll (jsdom keeps the stubbed top)
  fire('snapshot', { feed_id: 'feedB', seq: 81, incremental: false,
    records: BIG, counters: { in_flight: 0, total_requests: 80, total_errors: 0 } });
  check('full resync after paging resets to page size', await settleUntil(() => rows().length === 60));
  check('resync drops store-paged rows', !rows().some(tr => tr.dataset.id === 'old000'));

  // Delete/Logs preview ownership and failed mutations: exercise each actual
  // handler, with controlled out-of-order requests and a fixed preview cutoff.
  {
    const savedFetch = w.fetch;
    w.populateFilterMenu('lf');
    d.getElementById('del-provider').value = 'p';
    let replyCount;
    w.fetch = () => new Promise(resolve => { replyCount = resolve; });
    const oldCount = w.updateDelCount();
    check('delete matching remains disabled while count is pending', d.getElementById('btn-del-matching').disabled);
    d.getElementById('del-provider').value = '';
    await w.updateDelCount();
    replyCount({ok: true, json: async () => ({count: 99})});
    await oldCount;
    check('an old count cannot overwrite an empty selection or re-enable delete',
      d.getElementById('btn-del-matching').disabled && d.getElementById('del-count').textContent === '');

    check('Delete/Logs age options use duration tokens',
      ['del', 'lf'].every(prefix => {
        const el = d.getElementById(prefix + '-age');
        return !!el.querySelector('option[value="1h"]') &&
          !!el.querySelector('option[value="24h"]') &&
          !!el.querySelector('option[value="168h"]') &&
          !el.querySelector('option[value="3600000"]') &&
          !el.querySelector('option[value="86400000"]') &&
          !el.querySelector('option[value="604800000"]') &&
          !el.querySelector('option[value="7d"]');
      }));
    d.getElementById('del-age').value = '1h';
    d.getElementById('lf-age').value = '1h';
    const pendingCounts = [];
    w.fetch = () => new Promise(resolve => pendingCounts.push(resolve));
    const delCount = w.updateDelCount(), logsCount = w.updateLogsCount();
    pendingCounts[1]({ok: true, json: async () => ({count: 4})});
    pendingCounts[0]({ok: true, json: async () => ({count: 3})});
    await Promise.all([delCount, logsCount]);
    check('Logs and Delete counts do not supersede each other',
      d.getElementById('del-count').textContent.includes('3 records') && d.getElementById('logs-count').textContent.includes('4 records'));
    const preview = w.previewedFilter('del');
    const selectedRows = rows().slice();
    let submitted, resyncs = 0;
    w.fetch = async (url, options) => {
      if (String(url).includes('/admin/purge')) {
        submitted = JSON.parse(options.body);
        return {ok: false, json: async () => ({error: 'delete rejected'})};
      }
      resyncs++;
      return savedFetch(url, options);
    };
    await sleep(5); // drain the previous case's queued count render before the fetch swap
    await w.deleteMatching();
    check('filtered deletion uses the previewed cutoff, not a later time', submitted.before_ms === preview.filter.before_ms);
    check('failed deletion reports through the sheet status without wiping or resyncing the log', resyncs === 0 &&
      d.getElementById('settings-count').textContent === 'delete rejected' &&
      d.getElementById('del-count').textContent === 'will delete 3 records' &&
      !d.getElementById('btn-del-matching').disabled &&
      rows().every((row, i) => row === selectedRows[i]));
    let replyPurge, posts = 0;
    w.fetch = () => { posts++; return new Promise(resolve => { replyPurge = resolve; }); };
    const first = w.deleteMatching();
    w.purgeAll();
    w.deleteMatching();
    check('all destructive actions share one in-flight gate', posts === 1 &&
      d.getElementById('btn-purge-all').disabled && d.getElementById('btn-del-matching').disabled);
    check('the seven delete selects disable with the shared gate',
      [...d.querySelectorAll('#settings-fields [data-purge="matching"] select')].every(s => s.disabled));
    // A mid-flight settings refill must not resurrect enabled purge
    // controls: the fresh render inherits the busy state, and the purge's
    // epilogue re-enables whichever instance is current.
    w.eval('fillSettingsForm(settingsDoc)');
    check('a mid-flight settings refill renders both actions and the selects still disabled',
      d.getElementById('btn-purge-all').disabled && d.getElementById('btn-del-matching').disabled &&
      [...d.querySelectorAll('#settings-fields [data-purge="matching"] select')].every(s => s.disabled));
    replyPurge({ok: false, json: async () => ({error: 'still rejected'})});
    await first;
    check('the purge epilogue re-enables the re-rendered button and selects; the unarmed delete button stays disabled',
      !d.getElementById('btn-purge-all').disabled &&
      ![...d.querySelectorAll('#settings-fields [data-purge="matching"] select')].some(s => s.disabled) &&
      d.getElementById('btn-del-matching').disabled);
    d.getElementById('del-age').value = '';
    d.getElementById('lf-age').value = '';
    d.getElementById('lf-provider').value = '';
    w.fetch = savedFetch;
    await w.updateDelCount();
    await w.updateLogsCount();
    // Arm a fresh scoped preview, then prove a settings-origin failure
    // reports through the sheet status and never writes the delete
    // block's count preview.
    d.getElementById('del-age').value = '1h';
    await w.updateDelCount();
    check('the delete preview arms from the live count',
      d.getElementById('del-count').textContent === 'will delete 3 records' &&
      !d.getElementById('btn-del-matching').disabled);
    let replySettingsPurge;
    w.fetch = () => new Promise(resolve => { replySettingsPurge = resolve; });
    const settingsPurge = w.purgeAll();
    check('the settings purge announces its start through the sheet status',
      d.getElementById('settings-count').textContent === 'deleting');
    replySettingsPurge({ok: false, json: async () => ({error: 'nope'})});
    await settingsPurge;
    check('a failed settings purge reports through the sheet status, not the delete preview',
      d.getElementById('settings-count').textContent === 'nope' &&
      d.getElementById('del-count').textContent === 'will delete 3 records' &&
      !d.getElementById('btn-purge-all').disabled && !d.getElementById('btn-del-matching').disabled);
    // Leave the block disarmed for the suites that follow.
    w.fetch = savedFetch;
    d.getElementById('del-age').value = '';
    await w.updateDelCount();
  }

  // Durable pagination is independent of the ring, including timestamp ties,
  // empty-ring starts and duplicate-only pages. No page may stall the cursor.
  {
    const savedFetch = w.fetch, savedFill = w.scheduleLogFill;
    w.scheduleLogFill = () => {};
    w.resetLogWindow();
    w.eval('logLimit = lastData.records.length'); // real paging starts after the ring window is exhausted
    let pageRequest = 0;
    const urls = [];
    w.fetch = async url => {
      urls.push(String(url));
      const payload = pageRequest++ === 0
        ? {records: [BIG[79]], more: true, cursor_ms: 0, cursor_id: 'tie-z'}
        : {records: [mkRec('tie-b', 200, 0), mkRec('tie-a', 200, 0)], more: false, cursor_ms: 0, cursor_id: 'tie-a'};
      return {ok: true, json: async () => payload};
    };
    w.fetchOlderLog();
    check('a duplicate-only durable page advances without ending pagination',
      await settleUntil(() => !w.eval('logExhausted') && w.eval('logCursor && logCursor.id') === 'tie-z'));
    w.fetchOlderLog();
    check('log keyset sends both timestamp zero and its tie-break ID',
      await settleUntil(() => urls[1] && urls[1].includes('before_ms=0') && urls[1].includes('before_id=tie-z')));
    check('same-timestamp archive records retain the server tie order',
      await settleUntil(() => w.logVisible().filter(r => r.id.startsWith('tie-')).map(r => r.id).join(',') === 'tie-b,tie-a'));
    // The fetch above went through the page's real fetchOlderLog path; the
    // visible DOM must carry both the live ring window and the durable page in
    // one newest-first order.
    const archiveWindow = rows().map(tr => tr.dataset.id);
    const liveEdge = archiveWindow.indexOf('big000');
    check('a live ring row and a durable archive row share one visible log order',
      archiveWindow[0] === 'big079' && liveEdge >= 0 &&
      archiveWindow[liveEdge + 1] === 'tie-b' && archiveWindow[liveEdge + 2] === 'tie-a');
    const snapshot = w.eval('lastData');
    w.eval('lastData = {records: [], counters: {}}');
    w.resetLogWindow();
    pageRequest = 1;
    w.fetchOlderLog();
    check('an entirely empty ring still loads durable request history',
      await settleUntil(() => w.eval('logArchive.length') === 2 && urls[2] && !urls[2].includes('before_ms=')));
    w.eval('lastData = null');
    w.renderAll(snapshot);
    w.fetch = savedFetch;
    w.scheduleLogFill = savedFill;
  }

  // A durable boundary row may have an EMPTY id. The server still publishes
  // {ms, id:""} as the next cursor; treating the empty id as absence stopped
  // pagination and dropped every older page. The row itself may stay hidden,
  // but the cursor must advance and the continuation must send before_id=.
  {
    const savedFetch = w.fetch, savedFill = w.scheduleLogFill;
    const snapshot = w.eval('lastData');
    w.scheduleLogFill = () => {};
    w.resetLogWindow();
    w.eval('logLimit = lastData.records.length');
    const boundaryMs = 1800000002000;
    let pageRequest = 0;
    const urls = [];
    w.fetch = async url => {
      urls.push(String(url));
      const payload = pageRequest++ === 0
        ? {records: [mkRec('', 200, boundaryMs)], more: true,
          cursor_ms: boundaryMs, cursor_id: ''}
        : {records: [mkRec('older-after-empty', 200, boundaryMs - 1000)], more: false,
          cursor_ms: boundaryMs - 1000, cursor_id: 'older-after-empty'};
      return {ok: true, json: async () => payload};
    };
    w.fetchOlderLog();
    check('an empty-id boundary advances the durable cursor instead of ending pagination',
      await settleUntil(() => !w.eval('logExhausted') && w.eval('logCursor && logCursor.ms') === boundaryMs &&
        w.eval('logCursor && logCursor.id') === ''));
    w.fetchOlderLog();
    await settleUntil(() => urls[1]);
    const continuationURL = urls[1] || '';
    check('the continuation sends an explicit empty before_id after an empty-id boundary',
      continuationURL.includes('before_ms=' + boundaryMs) && continuationURL.includes('before_id=') &&
      !continuationURL.includes('before_id=undefined'));
    check('older durable pages load past a hidden empty-id boundary row',
      await settleUntil(() => w.eval('logArchive.length') === 1 && w.eval('logArchive[0].id') === 'older-after-empty'));
    w.eval('lastData = null');
    w.resetLogWindow();
    w.renderAll(snapshot);
    w.fetch = savedFetch;
    w.scheduleLogFill = savedFill;
  }
};
