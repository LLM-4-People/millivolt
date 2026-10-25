// round-2 frontend regressions
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function regressionsA(H) {
  const {check, d, mkRec, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- round-2 frontend regressions ----
  // These drive the production render/fetch owners and shipped events; no
  // helper copies. Each block restores the shared page state it changes.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null; clearTimeout(_xpLiveTimer); _xpLiveTimer = null; clearTimeout(logRetryTimer); logRetryTimer = null;');
    // An earlier reload-boundary check intentionally set dashboardReloading
    // and jsdom cannot navigate; the production gate is reset so these checks
    // can drive the real bootstrap path.
    w.eval('dashboardReloading = false');
    w.closeDrawer();
    if (w.settingsIsOpen()) w.closeSettings();

    const auditFeed = w.eval('feedId') || 'feedB';
    const auditBase = 1700002000000;
    state.fullPayload = { feed_id: auditFeed, seq: Math.max(500, w.eval('lastSeq') + 10), incremental: false,
      records: Array.from({ length: 6 }, (_, i) => ({ ...mkRec('audit' + i, 200, auditBase + i * 1000), __seq: i + 1 })),
      counters: { in_flight: 0, total_requests: 6, total_errors: 0 } };
    w.eval("fetchBootstrap('full')");
    await settleUntil(() => rows().length === 6);
    w.navigateTo([]);
    await settleUntil(() => rows().length === 6 &&
      !d.getElementById('f-scope').textContent.includes('…'));
    const chartWith = n => {
      const p = JSON.parse(JSON.stringify(state.chartPayload));
      p.now_ms = Date.now();
      p.from_ms = p.now_ms - 3600000;
      p.buckets.forEach(b => { b.req = 0; b.err = 0; });
      if (p.buckets.length) p.buckets[p.buckets.length - 1].req = n;
      return p;
    };

    // F7: one percentage-value formatter applies the pctCap floor to the
    // chart and storm surfaces; only a real >= 100 prints 100.
    const pctEval = expr => {
      try { return w.eval(expr); } catch (e) { return undefined; }
    };
    check('the shared percentage formatter applies pctCap at the boundary',
      pctEval('pctValue(99.949, 1)') === '99.9%' &&
      pctEval('pctValue(99.95, 1)') === '99.9%' &&
      pctEval('pctValue(99.999, 1)') === '99.9%' &&
      pctEval('pctValue(100, 1)') === '100.0%' &&
      pctEval('pctCap(1999, 2000, 1)') === '99.9%' &&
      pctEval('fmtPct(99.95)') === '99.9%' &&
      pctEval('fmtPct(100)') === '100%' &&
      pctEval('typeof stormPercent') === 'undefined');
    w.applyStormState({ enabled: true, banner_enabled: true, storms: [{
      provider: 'neutral.example', model: '', scope: 'provider', state: 'open',
      reason: 'http_503', quota: false, error_percent: 99.95, error_requests: 9,
      failures: 15, samples: 20, window_ms: 60000, queued: 4, active_models: 3,
      affected_models: 2, affected_model_percent: 99.95,
      retry_at: '2026-09-05T12:00:00Z', recovery_successes: 0, recovery_required: 2,
    }] });
    check('storm surfaces use the shared sub-100 percentage floor',
      d.getElementById('storm-banner').textContent.includes('99.9% failed attempts') &&
      !d.getElementById('storm-banner').textContent.includes('100% failed attempts'));
    w.applyStormState({ enabled: false, banner_enabled: false, storms: [] });

    // F8: a rounded cents form that reaches 100 promotes to the dollar form.
    check('a rounded sub-dollar value promotes to the dollar form, never 100 cents',
      w.eval('fmtMoney(0.999999)') === '$1' &&
      w.eval('fmtMoney(0.9999999)') === '$1' &&
      w.eval('fmtMoney(1)') === '$1' &&
      w.eval('fmtMoney(0.99999)') === '99.999¢' &&
      w.eval('fmtMoney(999.9999)') === '$1000');

    // F2: once the archive holds the cap, a fetched older page cannot be
    // retained. The cursor/fill owner must end pagination after that one
    // request instead of growing logLimit and fetching forever.
    {
      const saved = { fetch: w.fetch, fill: w.scheduleLogFill, page: state.logPage, data: w.eval('lastData') };
      w.scheduleLogFill = () => {};
      const capBase = 1600000000000;
      w.__auditCapArchive = Array.from({ length: 20000 }, (_, i) =>
        mkRec('cap' + String(i).padStart(5, '0'), 200, capBase + i * 1000));
      w.eval('logArchive = window.__auditCapArchive; logLimit = 20001; ' +
        'logCursor = {ms: ' + capBase + ', id: "cap00000"}; ' +
        'logExhausted = false; logFetching = false; logFailures = 0; logFetchFailed = false; logFetchGen++; ' +
        'logHTML.clear(); for (const r of logArchive) logHTML.set(r.id, {rec: r, sep: ""}); ' +
        'for (const r of (lastData && lastData.records || [])) logHTML.set(r.id, {rec: r, sep: ""});');
      const urls = [];
      const canon = { rules: [], revision: w.eval('modelRevision'), names: {} };
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/log')) return saved.fetch(url, opts);
        urls.push(u);
        return Promise.resolve({ ok: true, json: async () => ({
          records: [mkRec('cap-old', 200, capBase - 1000)], more: true,
          cursor_ms: capBase - 1000, cursor_id: 'cap-old', model_canon: canon,
        }) });
      };
      try {
        w.fetchOlderLog();
        await settleUntil(() => urls.length === 1 && !w.eval('logFetching'));
        await sleep(120); // negative-proof window: the terminal cap must not page again
        check('the archive cap is terminal: one older-page request, no retained rows',
          urls.length === 1 && w.eval('logArchive.length') === 20000 &&
          !w.eval('logArchive.some(r => r.id === "cap-old")') && w.eval('logExhausted') === true);
        check('the terminal cap does not grow logLimit for discarded rows', w.eval('logLimit') === 20001);
        w.fetchOlderLog();
        await sleep(80); // negative-proof window: scroll and delayed fills stay capped
        check('scroll and delayed fills stop at the terminal archive cap', urls.length === 1);
      } finally {
        w.fetch = saved.fetch;
        w.scheduleLogFill = saved.fill;
        state.logPage = saved.page;
        w.resetLogWindow();
        w.renderAll(saved.data);
        delete w.__auditCapArchive;
      }
    }

    // F10: a grown visible window keeps server/day order. Older page rows
    // append below the loaded history; a live row with no newer neighbor still
    // paints at the top. Day dividers move with their opener, not the insert.
    {
      const saved = {data: w.eval('lastData'), page: w.eval('dashCfg.log_rows'), fill: w.scheduleLogFill};
      w.scheduleLogFill = () => {};
      const growMid = new Date(); growMid.setHours(0, 0, 0, 0);
      const growAt = day => growMid.getTime() - day * 86400000 + 3600000;
      const grow = [
        mkRec('grow-old', 200, growAt(2)),
        mkRec('grow-yday', 200, growAt(1)),
        mkRec('grow-today1', 200, growAt(0)),
        mkRec('grow-today2', 200, growAt(0) + 3600000),
        mkRec('grow-today3', 200, growAt(0) + 7200000),
      ];
      w.__growData = {records: grow, counters: {}, _rev: 0};
      w.eval('logArchive = []; logLimit = 0; dashCfg.log_rows = 2; lastData = window.__growData; ' +
        'renderRequests(derive(lastData).scopedRecs); updateLogStats();');
      check('window growth: first paint honors the page cap',
        rows().map(r => r.dataset.id).join() === 'grow-today3,grow-today2');
      w.eval('dashCfg.log_rows = 5; renderRequestsIncremental(derive(lastData).scopedRecs);');
      check('window growth: older rows append in server order',
        rows().map(r => r.dataset.id).join() === 'grow-today3,grow-today2,grow-today1,grow-yday,grow-old');
      const grownSeps = [...d.querySelectorAll('#tbl-requests tr.day-sep')];
      check('window growth: day dividers stay attached to their openers',
        grownSeps.length === 3 &&
        grownSeps[0]?.nextElementSibling?.dataset.id === 'grow-today3' &&
        grownSeps[1]?.nextElementSibling?.dataset.id === 'grow-yday' &&
        grownSeps[2]?.nextElementSibling?.dataset.id === 'grow-old');
      w.resetLogWindow();
      w.eval('dashCfg.log_rows = ' + saved.page + ';');
      w.renderAll(saved.data);
      w.scheduleLogFill = saved.fill;
      delete w.__growData;
    }

    // F11: a live record that arrives while an older page is in flight is not
    // history. It must paint at the top when the page lands, not be appended
    // under the loaded rows.
    {
      const saved = {data: w.eval('lastData'), page: state.logPage, fill: w.scheduleLogFill};
      w.scheduleLogFill = () => {};
      const base = 1700005000000;
      const ring = [
        mkRec('race-r1', 200, base + 1000),
        mkRec('race-r2', 200, base + 2000),
        mkRec('race-r3', 200, base + 3000),
      ];
      w.__raceData = {records: ring, counters: {}, _rev: 0};
      w.eval('logArchive = []; logLimit = 0; dashCfg.log_rows = 60; lastData = window.__raceData; ' +
        'logFetching = false; logExhausted = false; logCursor = null; ' +
        'renderRequests(derive(lastData).scopedRecs); updateLogStats();');
      let release = null;
      state.logPageGate = new Promise(resolve => { release = resolve; });
      state.logPage = {records: [mkRec('race-old', 200, base)], more: false, cursor_ms: base, cursor_id: 'race-old'};
      try {
        w.fetchOlderLog();
        await settleUntil(() => w.eval('logFetching') === true);
        // The live event lands in the ring while the page is still held; its
        // scheduled paint has not run yet.
        ring.push(mkRec('race-live', 200, base + 4000));
        w.eval('bumpRecords(); bumpData();');
        check('the racing live row is not painted while the page is held',
          !rows().some(tr => tr.dataset.id === 'race-live') && w.eval('logFetching'));
        release();
        await settleUntil(() => rows().some(tr => tr.dataset.id === 'race-old'));
        check('a live row racing an older page paints at the top',
          rows().map(tr => tr.dataset.id).join() === 'race-live,race-r3,race-r2,race-r1,race-old');
        check('the racing live row is never duplicated under loaded history', rows().length === 5);
      } finally {
        state.logPageGate = null;
        state.logPage = saved.page;
        w.resetLogWindow();
        w.renderAll(saved.data);
        w.scheduleLogFill = saved.fill;
        delete w.__raceData;
      }
    }

    // F12: repeated live arrivals never expand an already-expanded window.
    // Expansion belongs to the scroll/fill owner; a live paint only fills or
    // ages out rows inside the fixed budget.
    {
      const saved = {data: w.eval('lastData'), page: w.eval('dashCfg.log_rows'), fill: w.scheduleLogFill};
      w.scheduleLogFill = () => {};
      const base = 1700006000000;
      const ring = Array.from({length: 120}, (_, i) =>
        mkRec('bound-base' + String(i).padStart(3, '0'), 200, base + i * 1000));
      w.__boundData = {records: ring, counters: {}, _rev: 0};
      w.eval('logArchive = []; logLimit = 120; dashCfg.log_rows = 60; lastData = window.__boundData; ' +
        'renderRequests(derive(lastData).scopedRecs); updateLogStats();');
      let maxRows = 0;
      for (let i = 0; i < 50; i++) {
        ring.push(mkRec('bound-live' + String(i).padStart(2, '0'), 200, base + 120000 + i));
        w.eval('bumpRecords()');
        w.renderRequestsIncremental(ring);
        maxRows = Math.max(maxRows, rows().length);
      }
      check('repeated live arrivals keep the expanded window bounded',
        maxRows === 120 && rows().length === 120 && w.eval('logLimit') === 120 &&
        w.eval('logHTML.size') === 120);
      check('the bounded window keeps the newest live rows at the top',
        rows()[0].dataset.id === 'bound-live49' &&
        rows()[rows().length - 1].dataset.id === 'bound-base050');
      w.resetLogWindow();
      w.eval('dashCfg.log_rows = ' + saved.page + ';');
      w.renderAll(saved.data);
      w.scheduleLogFill = saved.fill;
      delete w.__boundData;
    }

    // F3: the chart payload carries its window/scope identity. A changed key
    // clears the old payload and paints the skeleton before the response; a
    // failed new key shows an honest failure with no old totals.
    {
      const savedFetch = w.fetch;
      let mode = 'ok', value = 300;
      const held = [];
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/chart')) return savedFetch(url, opts);
        if (mode === 'hold') return new Promise((resolve, reject) => held.push({ resolve, reject }));
        if (mode === 'fail') return Promise.resolve({ ok: false, status: 503, json: async () => ({ error: 'chart down' }) });
        return Promise.resolve({ ok: true, json: async () => chartWith(value) });
      };
      try {
        w.eval("chartView.preset = 'traffic'; chartView.hidden = {}; chartView.window = 'all'; chartView.pct = 95; fillChartControls();");
        mode = 'ok'; value = 300;
        w.fetchChart('invalidate');
        await settleUntil(() => w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 300);
        mode = 'hold';
        const win = d.getElementById('chart-window');
        win.value = '1440';
        win.dispatchEvent(new w.Event('change', { bubbles: true }));
        await settleUntil(() => held.length === 1);
        check('a chart window change clears the old payload and paints the skeleton before the response',
          w.eval('chartAgg === null') &&
          !d.getElementById('chart-totals').textContent.includes('300') &&
          d.getElementById('chart-context').textContent === 'Waiting for traffic');
        held.shift().resolve({ ok: true, json: async () => chartWith(1) });
        await settleUntil(() => w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 1);
        check('only the matching window payload paints the new totals',
          w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 1 &&
          d.getElementById('chart-totals').textContent.includes('requests'));
        mode = 'fail';
        await w.fetchChart('refresh');
        check('a same-key chart refresh failure retains the last good payload',
          w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 1 &&
          d.getElementById('chart-totals').textContent.includes('requests 1'));
        w.navigateTo([{ dim: 'provider', id: 'chart-fail.example' }]);
        await settleUntil(() => d.getElementById('chart-context').textContent.includes('chart unavailable'));
        const failTotals = d.getElementById('chart-totals').textContent;
        check('a failed new-scope chart request shows no old-scope totals and a visible failure',
          w.eval('chartAgg === null') && failTotals.includes('chart unavailable') &&
          !failTotals.includes('300') && !/requests\s*1\b/.test(failTotals) &&
          d.getElementById('chart-context').textContent.includes('chart unavailable'));
      } finally {
        mode = 'ok'; value = 0;
        w.eval("chartView.window = 'all'; fillChartControls();");
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('chartAgg'), 3000);
        w.eval("chartView.preset = 'traffic'; chartView.hidden = {}; renderChart();");
        w.fetch = savedFetch;
      }
    }

    // F5: absent explorer counts are unknown, not measured zeros.
    {
      const savedFetch = w.fetch;
      let mode = 'hold';
      const held = [];
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/explorer')) return savedFetch(url, opts);
        if (mode === 'hold') return new Promise((resolve, reject) => held.push({ resolve, reject }));
        if (mode === 'fail') return Promise.resolve({ ok: false, status: 503, json: async () => ({ error: 'down' }) });
        return Promise.resolve({ ok: true, json: async () => ({
          dim: 'provider', total: 0, error_total: 0, rail: { provider: 0 }, groups: [],
          scope: { matches: 0, errors: 0 },
        }) });
      };
      const railValues = () => [...d.querySelectorAll('#xp-rail .xp-rail-item')]
        .map(b => b.querySelector('.rail-n').textContent);
      const triggerValue = () => d.getElementById('xp-dim-trigger').querySelector('.rail-n')?.textContent;
      try {
        w.navigateTo([{ dim: 'provider', id: 'rail-unknown.example' }]);
        await settleUntil(() => held.length > 0);
        check('an unloaded explorer payload renders unknown rail and trigger counts, not fabricated zeros',
          railValues().every(v => v === '-') && triggerValue() === '-' &&
          /loading breakdown/.test(d.getElementById('xp-gallery').textContent));
        mode = 'fail';
        held.shift().resolve({ ok: false, status: 503, json: async () => ({ error: 'down' }) });
        await settleUntil(() => /breakdown unavailable/.test(d.getElementById('xp-gallery').textContent));
        check('a failed explorer payload keeps rail and trigger counts unknown',
          railValues().every(v => v === '-') && triggerValue() === '-');
        mode = 'ok';
        w.fetchExplorer('invalidate');
        await settleUntil(() => w.eval('explorerAgg && explorerAgg.scopeKey === scopeQS()'));
        check('a measured explorer zero renders as a real zero',
          d.querySelector('#xp-rail [data-xp-dim="provider"] .rail-n').textContent === '0' &&
          triggerValue() === '0');
      } finally {
        w.fetch = savedFetch;
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('explorerAgg'), 3000);
      }
    }

    // F1: a purge invalidates same-scope aggregates at once, paints loading,
    // and reports completion only after the forced bootstrap + aggregate
    // resync settle. A failed chart resync keeps the deleted totals gone.
    {
      const savedFetch = w.fetch;
      const savedFull = state.fullPayload;
      const savedConfirm = w.confirm;
      const hold = { chart: [], explorer: [] };
      let phase = 'seed';
      const ghost = { dim: 'provider', total: 40, error_total: 0, rail: { provider: 40 },
        groups: [{ name: 'ghost.example', n: 40, cost: 0, in: 10, out: 5, cache: 2, tools: 0,
          err_final: 0, rate_limit_requests: 0, cost_per_mtok: null,
          ttft_p50: null, ttft_p95: null, tps_p50: null, tps_p95: null }],
        scope: { matches: 40, errors: 0 } };
      const empty = { dim: 'provider', total: 0, error_total: 0, rail: { provider: 0 },
        groups: [], scope: { matches: 0, errors: 0 } };
      w.fetch = (url, opts) => {
        const u = String(url);
        if (u.includes('/admin/purge') && !u.includes('/count')) {
          return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
        }
        if (u.includes('/metrics/agg/chart')) {
          if (phase === 'hold') return new Promise((resolve, reject) => hold.chart.push({ resolve, reject }));
          return Promise.resolve({ ok: true, json: async () => chartWith(phase === 'seed' ? 40 : 0) });
        }
        if (u.includes('/metrics/agg/explorer')) {
          if (phase === 'hold') return new Promise((resolve, reject) => hold.explorer.push({ resolve, reject }));
          return Promise.resolve({ ok: true, json: async () =>
            JSON.parse(JSON.stringify(phase === 'seed' ? ghost : empty)) });
        }
        return savedFetch(url, opts);
      };
      try {
        w.navigateTo([{ dim: 'provider', id: 'purge-scope.example' }]);
        await settleUntil(() => !!d.querySelector('#xp-gallery [data-xp-key="ghost.example"]') &&
          w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 40);
        const feed = w.eval('feedId');
        state.fullPayload = { feed_id: feed, seq: Math.max(900, w.eval('lastSeq') + 10), incremental: false,
          records: [], counters: { in_flight: 0, total_requests: 0, total_errors: 0 } };
        phase = 'hold';
        w.confirm = () => true;
        let purgeSettled = false;
        const purge = w.purgeAll().then(v => { purgeSettled = true; return v; });
        await settleUntil(() => hold.chart.length === 1 && hold.explorer.length === 1 && rows().length === 0);
        check('purge completion waits for the forced record and aggregate resync',
          purgeSettled === false);
        check('a purge drops same-scope aggregates and paints loading before the resync lands',
          w.eval('explorerAgg === null') && w.eval('chartAgg === null') &&
          /loading breakdown/.test(d.getElementById('xp-gallery').textContent) &&
          [...d.querySelectorAll('#xp-rail .xp-rail-item')].every(b => b.querySelector('.rail-n').textContent === '-') &&
          d.getElementById('xp-dim-trigger').querySelector('.rail-n').textContent === '-' &&
          d.getElementById('f-scope').textContent.startsWith('… of') &&
          !d.getElementById('f-scope').textContent.includes('40 of') &&
          !d.getElementById('chart-totals').textContent.includes('40'));
        hold.chart.shift().reject(new Error('chart down'));
        hold.explorer.shift().resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(empty)) });
        await Promise.race([purge, sleep(2000)]);
        await settleUntil(() => w.eval('explorerAgg !== null') && !w.eval('chartAgg'));
        check('a failed post-purge chart cannot retain deleted totals; the failure is visible',
          w.eval('chartAgg === null') &&
          d.getElementById('chart-totals').textContent.includes('chart unavailable') &&
          d.getElementById('chart-context').textContent.includes('chart unavailable') &&
          !d.getElementById('chart-totals').textContent.includes('40'));
        check('the released post-purge explorer paints the new empty scope',
          w.eval('explorerAgg && explorerAgg.scope.matches === 0') &&
          /no provider values yet/.test(d.getElementById('xp-gallery').textContent) &&
          !d.getElementById('xp-gallery').textContent.includes('ghost') &&
          d.getElementById('f-scope').textContent.startsWith('0 of '));
        check('purge completion is reported only after the forced resync settles',
          d.getElementById('settings-count').textContent === 'all metrics history deleted' &&
          d.getElementById('settings-count').classList.contains('ok'));
        phase = 'ok';
        w.fetchChart('invalidate');
        await settleUntil(() => w.eval('chartAgg && chartAgg.buckets.length > 0'));
        check('the chart recovers after the failed resync with fresh aggregates',
          d.getElementById('chart-totals').textContent.includes('requests 0'));
      } finally {
        phase = 'seed';
        w.confirm = savedConfirm;
        w.fetch = savedFetch;
        savedFull.seq = Math.max(savedFull.seq || 0, w.eval('lastSeq') + 10);
        state.fullPayload = savedFull;
        w.navigateTo([]);
        await w.fetchBootstrap('full');
        await settleUntil(() => rows().length === 6, 3000);
        await sleep(50); // drain the restored view's queued fill before leaving the case
      }
    }
  }
};
