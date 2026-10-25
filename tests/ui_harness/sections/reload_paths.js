// test 12k: a render between a hash assignment and its queued
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function reloadPaths(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, loaded, mkRec, observerFixture, pageOptions, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 12k: a render between a hash assignment and its queued
  // hashchange must not claim the pending scope. Otherwise onNavigate sees
  // the new scope as already rendered, skips the full path, and never fetches
  // the chart/explorer for the new filter set.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const scopeBefore = w.eval('_navScopeKey');
    try {
      w.eval("location.hash = '#/provider/pending.example'");
      w.eval('renderAll(lastData)');
      check('a render before the queued hashchange leaves the pending scope unclaimed',
        w.eval('_navScopeKey') === scopeBefore);
      check('the queued hashchange still routes the new scope',
        await settleUntil(() => w.eval('_navScopeKey') !== scopeBefore &&
          w.eval('_routedHash') === w.eval('location.hash')));
    } finally {
      w.navigateTo([]);
      await sleep(20);
    }
  }

  // Frontend changes reload through the existing bootstrap boundary only.
  // jsdom reports navigation as not implemented; the harness-start
  // jsdomError listener counts that actual reload signal (jsdomReloads)
  // while every other error fails the run. Chromium verifies the
  // resulting new document separately.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const originalFetch = w.fetch;
    const newVersion = '0000000000000002';
    const payload = (version, requests = 37) => ({
      dashboard_version: version, feed_id: w.eval('feedId'), seq: w.eval('lastSeq'), incremental: true,
      records: [], counters: { in_flight: 0 }, kpi: { requests, errors: 0 }, dash: {},
    });
    let calls = 0;
    const setResponse = response => {
      w.fetch = (url, opts) => {
        if (!String(url).includes('/metrics/bootstrap')) return originalFetch(url, opts);
        calls++;
        return Promise.resolve({ ok: true, json: async () => response });
      };
    };
    setResponse(payload(TEST_DASHBOARD_VERSION));
    w.fetchBootstrap('resume');
    // Negative proof: an unchanged frontend must not reload. The fixed bound
    // is the proof window; there is no observable "no reload" state.
    await sleep(20);
    check('unchanged frontend updates data without navigation or extra version requests',
      state.jsdomReloads === 0 && calls === 1 && w.eval('kpiAgg.requests') === 37);

    // An unexpected full response to a resume request is re-captured behind
    // the full/SSE barrier. The accepted full carries its own state/version.
    setResponse({ ...payload(TEST_DASHBOARD_VERSION), feed_id: 'same-assets-new-process', incremental: false });
    calls = 0;
    w.fetchBootstrap('resume');
    // Negative proof: re-capture must not reload the page.
    await sleep(20);
    check('same-assets restart re-captures an unexpected full response without a page reload', state.jsdomReloads === 0 && calls === 2);

    // Do not start expensive scans that a changed-frontend reload would
    // immediately throw away. An SSE restart first checks the bootstrap.
    const originalAggregates = w.refreshAggregates;
    const originalSettings = w.fetchSettings;
    let sweeps = 0;
    w.refreshAggregates = () => { sweeps++; };
    w.fetchSettings = () => { sweeps++; };
    let finishRestartBootstrap;
    w.fetch = () => new Promise(resolve => { finishRestartBootstrap = resolve; });
    w.refreshAfterRestart();
    check('SSE restart waits for the asset version before starting aggregate scans', sweeps === 0);
    finishRestartBootstrap({ok: true, json: async () => payload(TEST_DASHBOARD_VERSION)});
    check('unchanged frontend runs the deferred restart sweep once',
      await settleUntil(() => sweeps === 2));

    for (const version of [undefined, '', 'not-a-version']) {
      setResponse(payload(version, 38));
      w.fetchBootstrap('resume');
      await sleep(10);
    }
    check('missing or malformed asset versions never create a reload loop', state.jsdomReloads === 0);

    const pending = [];
    w.fetch = () => new Promise(resolve => pending.push(resolve));
    w.fetchBootstrap('resume');
    w.fetchBootstrap('resume');
    pending[1]({ok: true, json: async () => payload(TEST_DASHBOARD_VERSION, 39)});
    await settleUntil(() => w.eval('kpiAgg.requests') === 39);
    pending[0]({ok: true, json: async () => payload(newVersion, 999)});
    // Negative proof: the superseded response must not reload or overwrite 39.
    await sleep(10);
    check('a superseded bootstrap cannot reload the page using an outdated version',
      state.jsdomReloads === 0 && w.eval('kpiAgg.requests') === 39);

    let afterRan = false;
    setResponse(payload(newVersion, 999));
    w.fetchBootstrap('resume', () => { afterRan = true; });
    await sleep(20);
    check('changed frontend requests one reload before applying state or boot callbacks',
      state.jsdomReloads === 1 && !afterRan && w.eval('kpiAgg.requests') === 39);
    sweeps = 0;
    const callsBeforeReload = calls;
    w.refreshAfterRestart();
    w.fetchBootstrap('resume');
    w.fetchBootstrap('full');
    await sleep(10);
    check('reload in progress suppresses duplicate navigation and bootstrap work',
      state.jsdomReloads === 1 && calls === callsBeforeReload && sweeps === 0);
    w.fetch = originalFetch;
    w.refreshAggregates = originalAggregates;
    w.fetchSettings = originalSettings;
  }

  // The HTML seed follows the exact same bootstrap application gate as HTTP.
  // Exercise full page boot with saved views, then replay a completion that
  // happened between HTML generation and opening the live feed.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'seed-feed', seq: 12, pending_revision: 1,
      incremental: false, records: [mkRec('seed-final'), {...mkRec('excluded'), provider: 'other.example'}],
      in_flight_records: [mkRec('seed-pending', 0)], counters: {in_flight: 1},
      kpi: {requests: 100, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    for (const [label, embedded, expectedFetches] of [
      ['valid', seed, 0], ['malformed JSON', '{broken', 1],
      ['invalid lifecycle revision', {...seed, pending_revision: -1}, 1],
      ['invalid shape', {...seed, records: {}}, 1], ['missing', undefined, 1],
    ]) {
      let bootstrapFetches = 0, streamURL = '';
      const handlers = {};
      const seeded = dashboardDOM(assembleHTML(embedded), {...pageOptions,
        url: 'http://127.0.0.1:8081/#/provider/p',
        beforeParse(win) {
          pageOptions.beforeParse(win);
          const fallback = win.fetch;
          win.fetch = (url, options) => {
            if (String(url).includes('/metrics/bootstrap')) bootstrapFetches++;
            return fallback(url, options);
          };
          win.EventSource = function(url) {
            streamURL = url;
            return {readyState: 1, addEventListener(type, callback) {handlers[type] = callback;}, close() {}};
          };
          win.localStorage.setItem('dash.chart', JSON.stringify({window: '10080', preset: 'cost', pct: 99}));
        },
      });
      await loaded(seeded);
      const sw = seeded.window;
      await settleUntil(() => bootstrapFetches === expectedFetches &&
        !sw.document.getElementById('dashboard-bootstrap'));
      check(`${label} HTML seed uses ${expectedFetches} bootstrap fetches`, bootstrapFetches === expectedFetches);
      check(`${label} HTML seed is discarded after consumption`, !sw.document.getElementById('dashboard-bootstrap'));
      if (label === 'valid') {
        check('server-seeded boot restores saved chart and URL scope before first render',
          sw.eval('chartView.window') === '10080' && sw.eval('chartView.preset') === 'cost' &&
          !sw.document.querySelector('tr.exp-row[data-id="excluded"]') && sw.document.querySelector('tr.exp-row[data-id="seed-final"]'));
        check('server-seeded boot paints all-history KPI and pending rows with the right SSE cursor',
          sw.eval('kpiAgg.requests') === 100 && sw.eval('lastData.records.find(r => r.id === "seed-pending")?.live') &&
          streamURL.includes('since=12') && streamURL.includes('feed=seed-feed'));
        const finalized = mkRec('seed-pending');
        handlers.snapshot({data: JSON.stringify(observerFixture({feed_id:'seed-feed',seq:13,incremental:true,pending_revision:2,
          records:[finalized],in_flight_records:[],counters:{in_flight:0}}))});
        handlers.record({data:JSON.stringify(observerFixture(finalized)),lastEventId:'13'});
        check('completion between HTML and SSE replay finalizes once without duplicating a row',
          await settleUntil(() => sw.eval('lastData.records.filter(r => r.id === "seed-pending").length') === 1 &&
            !sw.eval('lastData.records.find(r => r.id === "seed-pending")?.live')));
        sw.fetchBootstrap('full');
        await settleUntil(() => bootstrapFetches === 1);
        check('later full resync fetches fresh state rather than reusing HTML seed', bootstrapFetches === 1);
      }
      sw.close();
    }
  }
};
