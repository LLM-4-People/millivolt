// test 12d: a stale tick's pending snapshot must never regress an
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function liveInvariants(H) {
  const {check, d, fire, mkRec, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 12d: a stale tick's pending snapshot must never regress an
  // in-flight row's retry progress (attempts are append-only - an older
  // snapshot carries fewer absorbed attempts than a just-arrived SSE update).
  {
    fire('begin', { record: { ...mkRec('pendX', 0, 1700000300000), attempts: [] }, in_flight: 1 });
    fire('update', { record: { ...mkRec('pendX', 0, 1700000300000), attempts: [{ status: 503 }, { status: 502 }] }, in_flight: 1 });
    await sleep(20);
    const attN = () => w.eval("(lastData.records.find(r => r.id === 'pendX')?.attempts || []).length");
    w.eval("upsertPendingRec({ id: 'pendX', status_code: 0, live: true, attempts: [{ status: 503 }] }, _pendingRevision - 1)"); // stale tick snapshot
    check('stale pending snapshot never regresses retry progress', attN() === 2);
    w.eval("upsertPendingRec({ id: 'pendX', status_code: 0, live: true, attempts: [{ status: 503 }, { status: 502 }, { status: 500 }] }, _pendingRevision + 1)"); // newer
    check('newer pending snapshot refreshes retry progress', attN() === 3);
    fire('end', { record: { ...mkRec('pendX', 200, 1700000300000), attempts: [{ status: 503 }, { status: 502 }, { status: 500 }] }, in_flight: 0 });
    await sleep(20);
    check('pending row finalizes after the badge checks',
      !!rows().find(tr => tr.dataset.id === 'pendX') && w.eval("lastData.records.find(r => r.id === 'pendX').live") === false);
  }

  // ---- test 12e: O(1) upsert - _recIdx map is maintained on insert/update ----
  {
    const origU = w.fetch;
    state.fullPayload = { feed_id: 'feedB', seq: 5,
      records: [{ ...mkRec('idx1', 200, 1700000400000), __seq: 1 },
                { ...mkRec('idx2', 200, 1700000401000), __seq: 2 }],
      counters: { in_flight: 0, total_requests: 2, total_errors: 0 } };
    w.eval("fetchBootstrap('full')");
    await sleep(20);
    const has = id => w.eval("_recIdx.has('" + id + "')");
    const get = id => w.eval("_recIdx.get('" + id + "')");
    check('_recIdx is populated after full snapshot', has('idx1') && has('idx2'));
    check('_recIdx indexes are correct', get('idx1') === 0 && get('idx2') === 1);
    // Insert a new record via SSE
    fire('record', { ...mkRec('idx3', 200, 1700000402000) });
    await sleep(20);
    check('_recIdx grows on new record insert', has('idx3'));
    // Replay an existing record - should NOT bump _rev (skip-bump-on-replay)
    const revBefore = w.eval('lastData._rev');
    fire('record', { ...mkRec('idx1', 200, 1700000400000) }); // replay
    await sleep(20);
    check('pure replay does not bump _rev', w.eval('lastData._rev') === revBefore);
    w.fetch = origU;
  }

  // ---- test 12f: row-identity cache - reqRow is skipped for unchanged records ----
  {
    // Insert two records, then replay one - the replayed row must not rebuild
    // HTML. The cache stores record identity, not markup, so count reqRow calls.
    w.eval("window.__reqRowOrig = window.reqRow; window.__reqRowCalls = 0; window.reqRow = (...a) => { window.__reqRowCalls++; return window.__reqRowOrig(...a); };");
    fire('record', { ...mkRec('idx1', 200, 1700000400000) }); // replay
    await sleep(20);
    const replayCalls = w.eval('window.__reqRowCalls');
    check('row-identity cache skips reqRow for an unchanged record', replayCalls === 0);
    // A real update (pending → finalized) MUST rebuild
    w.eval('window.__reqRowCalls = 0');
    fire('begin', { record: { ...mkRec('idx4', 0, 1700000403000), attempts: [] }, in_flight: 1 });
    await sleep(20);
    const liveBefore = w.eval("logHTML.get('idx4')?.rec?.live");
    fire('end', { record: { ...mkRec('idx4', 200, 1700000403000), attempts: [] }, in_flight: 0 });
    await sleep(20);
    const liveAfter = w.eval("logHTML.get('idx4')?.rec?.live");
    check('row-identity cache rebuilds on pending→finalized transition',
      liveBefore === true && liveAfter === false && w.eval('window.__reqRowCalls') > 0);
    w.eval('window.reqRow = window.__reqRowOrig; delete window.__reqRowOrig; delete window.__reqRowCalls;');
  }

  // ---- test 12g: requestGate helper - issue-order supersede + dedupe ----
  {
    // Direct test of the requestGate logic
    w.eval(`(() => {
      const gate = requestGate(true);
      let applied = [];
      gate('key1', (accept) => { applied.push('a1'); accept('p1', true); }, (p) => { applied.push('apply1'); });
      gate('key1', (accept) => { applied.push('a2'); accept('p2', true); }, (p) => { applied.push('apply2'); });
      window.__gateTest = applied;
    })()`);
    check('requestGate dedupes same-key calls', w.eval('window.__gateTest')?.join(',') === 'a1,apply1');
    // Supersede: different key wins
    w.eval(`(() => {
      const gate = requestGate(false);
      let result = [];
      // gate IS the run function directly
      let accept1;
      gate('k1', (accept) => { result.push('f1'); accept1 = accept; }, (p) => { result.push('a1'); });
      gate('k2', (accept) => { result.push('f2'); accept('p2', true); }, (p) => { result.push('a2'); });
      accept1('stale', true); // try to apply after superseded
      window.__gateSuper = result;
    })()`);
    check('requestGate supersedes older response', w.eval('window.__gateSuper')?.join(',') === 'f1,f2,a2');
    w.eval(`(() => {
      const gate = requestGate(true);
      let firstSignal, stale, result = [];
      gate('same', (accept, signal) => { stale = accept; firstSignal = signal; }, p => result.push(p));
      gate('same', () => result.push('duplicate'), p => result.push(p), 'refresh');
      result.push(firstSignal.aborted ? 'starved' : 'not-starved');
      gate('same', accept => accept('fresh', true), p => result.push(p), 'invalidate');
      stale('stale', true);
      gate('failure', accept => accept(null, false), p => result.push(p));
      gate('failure', accept => accept('retried', true), p => result.push(p));
      window.__gateCancel = {aborted: firstSignal.aborted, result};
    })()`);
    check('requestGate cancels superseded work, forces same-key refresh and retries failure',
      w.eval('window.__gateCancel.aborted') && w.eval('window.__gateCancel.result.join(",")') === 'not-starved,fresh,retried');
  }

  // ---- test 12h: visibilitychange pauses timers ----
  {
    const hasClock = w.eval('typeof _clockTimer === "number" && _clockTimer !== null');
    check('clock timer is armed', hasClock);
    // Simulate hidden
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    // Override document.hidden for the handler
    Object.defineProperty(w.document, 'hidden', { value: true, configurable: true });
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    const clockStopped = w.eval('_clockTimer === null');
    check('clock timer stops when hidden', clockStopped);
    const tickStopped = w.eval('_dashTickTimer === null');
    check('tick timer stops when hidden', tickStopped);
    // Simulate visible
    Object.defineProperty(w.document, 'hidden', { value: false, configurable: true });
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    await sleep(20);
    const clockRestarted = w.eval('typeof _clockTimer === "number" && _clockTimer !== null');
    check('clock timer restarts on visible', clockRestarted);
    const tickRestarted = w.eval('typeof _dashTickTimer === "number" && _dashTickTimer !== null');
    check('tick timer restarts on visible', tickRestarted);

    // A live-status explorer view must not schedule refetches while hidden.
    w.eval(`window.__origFetchExplorer = fetchExplorer;
      window.__origWantsLive = explorerWantsLiveStatus;
      fetchExplorer = () => { window.__xpRefreshCount = (window.__xpRefreshCount || 0) + 1; };
      explorerWantsLiveStatus = () => true;
      window.__xpRefreshCount = 0; _xpLiveTimer = null;`);
    Object.defineProperty(w.document, 'hidden', { value: true, configurable: true });
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    w.eval('scheduleExplorerLiveRefresh()');
    await sleep(1100);
    check('hidden tab does not schedule a live-status explorer refresh',
      w.eval('_xpLiveTimer === null && window.__xpRefreshCount === 0'));
    check('live-status explorer cadence stays at or above one second',
      w.eval('EXPLORER_LIVE_REFRESH_MS >= 1000'));
    Object.defineProperty(w.document, 'hidden', { value: false, configurable: true });
    w.eval('d3.dispatchEvent(new Event("visibilitychange"))'.replace('d3', 'document'));
    await sleep(20);
    check('returning to view refreshes a live-status explorer immediately',
      w.eval('window.__xpRefreshCount === 1'));
    w.eval('scheduleExplorerLiveRefresh()');
    await sleep(1100);
    check('visible tab keeps the throttled live-status refresh',
      w.eval('window.__xpRefreshCount === 2 && _xpLiveTimer === null'));
    w.eval(`fetchExplorer = window.__origFetchExplorer;
      explorerWantsLiveStatus = window.__origWantsLive;
      delete window.__origFetchExplorer; delete window.__origWantsLive; delete window.__xpRefreshCount;`);
  }

  // ---- test 12i: record-then-end gauge correction - the server publishes
  // the ring "record" BEFORE the lifecycle "end", so the end event's record is
  // usually a replay that bumps nothing in upsertRec: the counters write
  // itself must bump the derivation or the In-flight tile keeps the pre-end
  // value until an unrelated bump. An unchanged gauge (an update re-carrying
  // its begin's value) must NOT re-derive.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;'); // no tick may repaint the tile for us
    const flightVal = () => {
      const el = [...d.querySelectorAll('#kpis .kpi')].find(k => (k.querySelector('h2') || {}).textContent === 'In-flight');
      return el ? el.querySelector('.val').textContent.trim() : null;
    };
    fire('begin', { record: { ...mkRec('gauge1', 0, 1700000500000), attempts: [] }, in_flight: 1 });
    await sleep(20);
    check('begin paints the In-flight tile live (new pending row)', flightVal() === '1');
    fire('record', { ...mkRec('gauge1', 200, 1700000500000) }, '9');
    await sleep(20);
    check('ring record finalizes the row; the tile keeps the pre-end gauge', flightVal() === '1');
    const revBeforeEnd = w.eval('lastData._rev');
    fire('end', { record: { ...mkRec('gauge1', 200, 1700000500000) }, in_flight: 0 });
    await sleep(20);
    check('end event corrects the In-flight tile with no further event', flightVal() === '0');
    check('the end-event counters write bumps the derivation', w.eval('lastData._rev') > revBeforeEnd);
    const revStable = w.eval('lastData._rev');
    fire('end', { record: { ...mkRec('gauge1', 200, 1700000500000) }, in_flight: 0 }); // replayed end, unchanged gauge
    await sleep(20);
    check('an unchanged gauge does not re-derive (no redundant pass)', w.eval('lastData._rev') === revStable);
    w.eval('armDashboardTicks()');
  }

  // Chart/explorer share the request gate; a new scope never waits behind
  // old work, and the footer consumes the explorer's FULL-scope totals.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const originalFetch = w.fetch;
    const pending = [];
    let scopeFetches = 0;
    w.fetch = (url, options) => {
      if (String(url).includes('/metrics/agg/scope')) scopeFetches++;
      if (!String(url).includes('/metrics/agg/explorer')) return originalFetch(url, options);
      return new Promise(resolve => pending.push({url, signal: options.signal, resolve}));
    };
    const reply = (request, matches) => request.resolve({ok: true, json: async () => ({
      dim: 'provider', total: 1000, error_total: 0, rail: {}, groups: [], scope: {matches, errors: 0},
    })});
    w.navigateTo([{dim: 'provider', id: 'scope-a.example'}]);
    await sleep(20);
    w.fetchExplorer();
    check('same-scope explorer requests dedupe while the first response is pending', pending.length === 1);
    check('unresolved filtered footer never substitutes ring counts or another scope', d.getElementById('f-scope').textContent.startsWith('… of '));
    w.navigateTo([{dim: 'provider', id: 'scope-b.example'}]);
    await sleep(20);
    check('new scope starts immediately and cancels the old explorer scan', pending.length === 2 && pending[0].signal.aborted);
    reply(pending[1], 7);
    await sleep(20);
    check('footer uses full-scope counts from explorer, not cross-filter gallery totals', d.getElementById('f-scope').textContent.startsWith('7 of '));
    reply(pending[0], 999);
    await sleep(10);
    check('a stale explorer response cannot overwrite current footer counts', d.getElementById('f-scope').textContent.startsWith('7 of '));
    w.fetchExplorer('invalidate');
    w.fetchExplorer('invalidate');
    check('forced same-scope refresh supersedes by issue order', pending.length === 4 && pending[2].signal.aborted);
    reply(pending[3], 8);
    reply(pending[2], 999);
    await sleep(20);
    check('latest forced refresh wins without a separate footer history scan', scopeFetches === 0 && d.getElementById('f-scope').textContent.startsWith('8 of '));
    const previousExplorer = w.eval('explorerAgg');
    w.fetchExplorer('invalidate');
    d.getElementById('f-status').value = 'streaming';
    w.doFilter();
    check('status selections immediately supersede old scans without a debounce delay', pending.length === 6 && pending[4].signal.aborted);
    check('live Requests status filters refresh explorer/footer regardless of breakdown dimension', w.explorerWantsLiveStatus());
    reply(pending[4], 999);
    await sleep(10);
    check('an old status response cannot paint under the new selection',
      w.eval('explorerAgg') === null && w.eval('explorerAgg') !== previousExplorer &&
      d.getElementById('f-scope').textContent.startsWith('… of '));
    reply(pending[5], 0);
    await sleep(10);
    check('zero live matches is a real full-scope count', d.getElementById('f-scope').textContent.startsWith('0 of '));
    w.fetch = originalFetch;
    d.getElementById('f-status').value = '';
    w.doFilter();
    w.navigateTo([]);
    await sleep(20);
    check('unfiltered footer reads global KPI immediately', !d.getElementById('f-scope').textContent.includes('…'));
  }

  // ---- test 12i-bis: a failed explorer scan for a new scope must not leave
  // the old scope's cards visible, and the dashboard tick must retry it.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const originalFetch = w.fetch;
    const gallery = d.getElementById('xp-gallery');
    const calls = [];
    let bFailures = 0;
    let bPending = 0;
    let failB = true;
    const ent = (name, n) => ({name, n, cost:0, in:10, out:5, cache:2, tools:0,
      err_final:0, rate_limit_requests:0, cost_per_mtok:null,
      ttft_p50:null, ttft_p95:null, tps_p50:null, tps_p95:null});
    const payload = (isB) => ({
      dim: 'provider', total: isB ? 20 : 10, error_total: 0,
      rail: {provider: isB ? 20 : 10},
      groups: [ent(isB ? 'b-node.example' : 'a-node.example', isB ? 20 : 10)],
      scope: {matches: isB ? 20 : 10, errors: 0},
    });
    w.fetch = (url, options) => {
      const u = String(url);
      if (!u.includes('/metrics/agg/explorer')) return originalFetch(url, options);
      const f = new URL(u, 'http://x').searchParams.get('f') || '';
      const isB = f === 'provider:scope-b.example';
      calls.push({f, isB});
      if (isB && failB) {
        bFailures++;
        bPending++;
        return Promise.resolve({ok: false, status: 503, json: async () => ({error: 'scan unavailable'})})
          .finally(() => { bPending--; });
      }
      return Promise.resolve({ok: true, json: async () => payload(isB)});
    };
    try {
      w.navigateTo([{dim: 'provider', id: 'scope-a.example'}]);
      await settleUntil(() => !!gallery.querySelector('[data-xp-key="a-node.example"]'));
      check('explorer success paints the requested scope cards',
        calls.some(c => !c.isB) && !!gallery.querySelector('[data-xp-key="a-node.example"]') &&
        !/breakdown unavailable/.test(gallery.textContent));

      w.navigateTo([{dim: 'provider', id: 'scope-b.example'}]);
      await settleUntil(() => bFailures === 1 && /breakdown unavailable/.test(gallery.textContent));
      check('a failed new-scope explorer fetch removes the old scope cards',
        !!gallery.querySelector('[data-xp-key="a-node.example"]') === false &&
        gallery.querySelectorAll('.xp-node').length === 0 &&
        /breakdown unavailable/.test(gallery.textContent));
      check('the failed scope shows an honest loading/failure state instead of old cards',
        gallery.querySelector('.empty') !== null && !/a-node\.example/.test(gallery.textContent));

      w.refreshExplorerIfStale();
      await settleUntil(() => bFailures === 2 && bPending === 0);
      check('the stale-refresh owner retries a failed scope even while live data is unchanged',
        calls.filter(c => c.isB).length >= 2 && calls[calls.length - 1].f === 'provider:scope-b.example');

      failB = false;
      w.refreshExplorerIfStale();
      await settleUntil(() => !!gallery.querySelector('[data-xp-key="b-node.example"]'));
      check('a restored explorer endpoint repaints scope B and clears the failure state',
        !!gallery.querySelector('[data-xp-key="b-node.example"]') &&
        !gallery.querySelector('[data-xp-key="a-node.example"]') &&
        !/breakdown unavailable/.test(gallery.textContent));
    } finally {
      failB = false;
      w.fetch = originalFetch;
      w.navigateTo([]);
      await sleep(20);
    }
  }

  // ---- test 12i-ter: a live-status explorer count refresh rebuilds the
  // dimension rail, but keyboard focus must stay on the same rail item.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    if (w.eval('_xpLiveTimer')) w.eval('clearTimeout(_xpLiveTimer); _xpLiveTimer = null;');
    const originalFetch = w.fetch;
    const originalTimeout = w.setTimeout;
    let railModel = 3;
    let liveRefresh = null;
    w.fetch = (url, options) => {
      const u = String(url);
      if (!u.includes('/metrics/agg/explorer')) return originalFetch(url, options);
      return Promise.resolve({ok: true, json: async () => ({
        dim: 'status', total: 30, error_total: 0, rail: {provider: 1, model: railModel},
        groups: [], scope: {matches: 30, errors: 0},
      })});
    };
    try {
      w.navigateTo([{dim: 'by', id: 'status'}]);
      await settleUntil(() => d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'status' &&
        w.eval('!!explorerAgg && explorerAgg.dim === "status"'));
      const rail = d.getElementById('xp-rail');
      const focused = rail.querySelector('.xp-rail-item[data-xp-dim="model"]');
      focused.focus();
      check('the explorer rail can take keyboard focus before its count changes',
        d.activeElement === focused && focused.querySelector('.rail-n').textContent === '3');

      railModel = 999;
      w.setTimeout = (fn, ms, ...args) => {
        if (ms === w.eval('EXPLORER_LIVE_REFRESH_MS')) {
          liveRefresh = () => fn(...args);
          return 4242;
        }
        return originalTimeout(fn, ms, ...args);
      };
      fire('begin', {record: {...mkRec('rail-focus1', 200, 1700000600000)}, in_flight: 1});
      w.setTimeout = originalTimeout;
      check('a live-status event schedules the explorer refresh owner', typeof liveRefresh === 'function');
      if (liveRefresh) liveRefresh();
      await settleUntil(() => d.querySelector('#xp-rail [data-xp-dim="model"] .rail-n')?.textContent === '999');
      const next = d.querySelector('#xp-rail [data-xp-dim="model"]');
      check('a rail count repaint keeps focus on the same dimension button',
        next !== focused && d.activeElement === next &&
        next.querySelector('.rail-n').textContent === '999');
    } finally {
      w.setTimeout = originalTimeout;
      w.fetch = originalFetch;
      try { fire('end', {record: mkRec('rail-focus1', 200, 1700000600000), in_flight: 0}); } catch (e) {}
      w.eval('if (_xpLiveTimer) { clearTimeout(_xpLiveTimer); _xpLiveTimer = null; }');
      w.navigateTo([]);
      await sleep(20);
    }
  }

  // ---- test 12i-quater: #/request/<id> is inert. The old request leaf was
  // removed from EXPLORER_DIMS, so the route must not become a bogus filter
  // and must never throw. Normal filter, status and breakdown routes still
  // parse through the same hash owner.
  {
    let probe = null, error = null;
    try {
      probe = w.eval(`(() => {
        const savedStatus = filters.status;
        history.replaceState(null, '', '#/request/req-123');
        const request = explorerState();
        const requestSegments = parseHash();
        const requestScope = scopeQS();
        history.replaceState(null, '', '#/provider/a.example/by/model');
        const normal = explorerState();
        filters.status = '500';
        const scope = scopeQS();
        filters.status = savedStatus;
        history.replaceState(null, '', location.pathname + location.search);
        return {
          requestSegments: requestSegments.length,
          requestFilters: request.filters.length,
          requestDim: request.activeDim,
          requestScope,
          normalFilters: normal.filters,
          normalDim: normal.activeDim,
          scope,
        };
      })()`);
    } catch (e) { error = e; }
    check('#/request/<id> is ignored without a bogus filter or an exception',
      error === null && !!probe && probe.requestSegments === 0 &&
      probe.requestFilters === 0 && probe.requestDim === 'provider' && probe.requestScope === '');
    check('normal explorer filter, status and by routes still parse',
      !!probe && probe.normalFilters.length === 1 &&
      probe.normalFilters[0].dim === 'provider' && probe.normalFilters[0].id === 'a.example' &&
      probe.normalDim === 'model' && probe.scope.includes('s=500') &&
      probe.scope.includes('f=provider%3Aa.example'));
  }

  // ---- test 12j: a rail click changes only the breakdown dimension (view
  // state, by/<dim>), not the log/chart scope. A by-only hash change must
  // repaint the explorer and fetch just the new breakdown; the full render
  // path (which resets the paged log window and rebuilds the chart/drawer)
  // stays on real filter or status changes.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;'); // a tick must not mask the route
    const navBase = 1700001000000;
    state.fullPayload = { feed_id: 'feedB', seq: 200, incremental: false,
      records: [
        { ...mkRec('nav-a1', 200, navBase + 1000), provider: 'nav-a.example', __seq: 1 },
        { ...mkRec('nav-a2', 200, navBase + 2000), provider: 'nav-a.example', __seq: 2 },
        { ...mkRec('nav-b1', 500, navBase + 3000), provider: 'nav-b.example', __seq: 3 },
      ],
      counters: { in_flight: 0, total_requests: 3, total_errors: 1 } };
    w.eval("fetchBootstrap('full')");
    await settleUntil(() => rows().length === 3);

    // Establish a filtered scope with one durable archive page below the ring,
    // then give the log window a non-default limit and scroll position.
    state.logPage = { records: [{ ...mkRec('nav-a-old', 200, navBase - 1000), provider: 'nav-a.example' }],
      more: false, cursor_ms: navBase - 1000, cursor_id: 'nav-a-old' };
    Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 200; } });
    w.navigateTo([{ dim: 'provider', id: 'nav-a.example' }]);
    await settleUntil(() => w.eval('logArchive.length') === 1 && rows().length === 3 &&
      !d.getElementById('f-scope').textContent.startsWith('…'));
    Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 4000; } });
    state.box.scrollTop = 137;
    w.eval('logLimit = 2');
    w.openDrawer('nav-a1');
    await sleep(20);

    const filtersBefore = w.eval('explorerFilters()');
    const derFBefore = w.eval('_derFS');
    const chartBefore = w.eval('chartAgg');
    const archiveBefore = w.eval('logArchive');
    const limitBefore = w.eval('logLimit');
    const htmlBefore = d.getElementById('tbl-requests').innerHTML;
    const drawerBefore = d.getElementById('drawer-body').innerHTML;
    const footerBefore = d.getElementById('f-scope').textContent;
    const railBefore = d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim;
    const priorXp = new URL(state.initialFetches.filter(u => u.includes('/metrics/agg/explorer')).pop() || '',
      'http://x').searchParams;
    check('the canonical filter key is order-independent and value-sensitive',
      typeof w.explorerFilterKey === 'function' &&
      w.explorerFilterKey([{ dim: 'provider', id: 'a' }, { dim: 'model', id: 'm' }]) ===
        w.explorerFilterKey([{ dim: 'model', id: 'm' }, { dim: 'provider', id: 'a' }]) &&
      w.explorerFilterKey([{ dim: 'provider', id: 'a' }]) !==
        w.explorerFilterKey([{ dim: 'provider', id: 'b' }]));

    const orig = {
      renderAll: w.renderAll, fetchChart: w.fetchChart, renderChart: w.renderChart,
      resetLogWindow: w.resetLogWindow, renderDrawer: w.renderDrawer,
      renderExplorer: w.renderExplorer, fetchExplorer: w.fetchExplorer, fetch: w.fetch,
    };
    const calls = { renderAll: 0, fetchChart: 0, renderChart: 0, resetLogWindow: 0,
      renderDrawer: 0, renderExplorer: 0, fetchExplorer: 0 };
    const explorerURLs = [];
    w.renderAll = (...a) => { calls.renderAll++; return orig.renderAll(...a); };
    w.fetchChart = (...a) => { calls.fetchChart++; return orig.fetchChart(...a); };
    w.renderChart = (...a) => { calls.renderChart++; return orig.renderChart(...a); };
    w.resetLogWindow = (...a) => { calls.resetLogWindow++; return orig.resetLogWindow(...a); };
    w.renderDrawer = (...a) => { calls.renderDrawer++; return orig.renderDrawer(...a); };
    w.renderExplorer = (...a) => { calls.renderExplorer++; return orig.renderExplorer(...a); };
    w.fetchExplorer = (...a) => { calls.fetchExplorer++; return orig.fetchExplorer(...a); };
    w.fetch = (url, opts) => {
      if (String(url).includes('/metrics/agg/explorer')) explorerURLs.push(String(url));
      return orig.fetch(url, opts);
    };
    const fstatus = d.getElementById('f-status');
    try {
      // Real navigateTo path: same filter set + status, new breakdown only.
      w.navigateTo([{ dim: 'provider', id: 'nav-a.example' }, { dim: 'by', id: 'model' }]);
      await settleUntil(() => calls.renderExplorer > 0 && calls.fetchExplorer > 0 &&
        explorerURLs.length > 0 && d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'model');

      check('by-only navigation does not run the full render path',
        calls.renderAll === 0 && calls.fetchChart === 0 && calls.renderChart === 0 &&
        calls.resetLogWindow === 0);
      check('by-only navigation leaves the log window, archive, limit and scroll intact',
        d.getElementById('tbl-requests').innerHTML === htmlBefore &&
        w.eval('logArchive') === archiveBefore && w.eval('logLimit') === limitBefore &&
        state.box.scrollTop === 137);
      check('by-only navigation leaves the open drawer untouched',
        calls.renderDrawer === 0 && d.getElementById('drawer-body').innerHTML === drawerBefore);
      check('by-only navigation repaints the rail and fetches only the new breakdown',
        calls.renderExplorer > 0 && calls.fetchExplorer === 1 && railBefore === 'provider' &&
        d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'model' &&
        d.querySelector('#xp-rail .xp-rail-item[data-xp-dim="model"]')?.getAttribute('aria-pressed') === 'true');
      const xpURL = new URL(explorerURLs[explorerURLs.length - 1], 'http://x').searchParams;
      check('the new breakdown request keeps the same filter/status scope',
        xpURL.get('dim') === 'model' && xpURL.get('f') === 'provider:nav-a.example' &&
        (xpURL.get('s') || '') === '' && xpURL.get('f') === priorXp.get('f') &&
        (xpURL.get('s') || '') === (priorXp.get('s') || ''));
      check('by-only navigation keeps the cached filter-set identity (derive stays valid)',
        w.eval('explorerFilters()') === filtersBefore && w.eval('_derFS') === derFBefore &&
        w.eval('chartAgg') === chartBefore && d.getElementById('f-scope').textContent === footerBefore);

      // Filter removal: drop the provider filter, keep the breakdown.
      const beforeRemove = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      w.navigateTo([{ dim: 'by', id: 'model' }]);
      await settleUntil(() => calls.renderAll === beforeRemove.renderAll + 1);
      check('a real filter removal still runs the full path and resets the paged log',
        calls.renderAll === beforeRemove.renderAll + 1 &&
        calls.fetchChart === beforeRemove.fetchChart + 1 &&
        calls.resetLogWindow === beforeRemove.resetLogWindow + 1 &&
        w.eval('logArchive.length') === 0 && w.eval('logLimit') === 0);

      // Filter addition: scope to the other provider, same breakdown.
      const beforeAdd = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      w.navigateTo([{ dim: 'provider', id: 'nav-b.example' }, { dim: 'by', id: 'model' }]);
      await settleUntil(() => calls.renderAll === beforeAdd.renderAll + 1);
      check('a real filter addition still runs the full path and resets the log',
        calls.renderAll === beforeAdd.renderAll + 1 &&
        calls.fetchChart === beforeAdd.fetchChart + 1 &&
        calls.resetLogWindow === beforeAdd.resetLogWindow + 1);

      // Status changes are independent of the hash and still invalidate the
      // memoized filter set and reset the log window through doFilter.
      const statusFiltersBefore = w.eval('explorerFilters()');
      const beforeStatus = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      fstatus.value = '500';
      fstatus.dispatchEvent(new w.Event('change'));
      await sleep(20);
      check('a status change still runs the full path and resets the log',
        calls.renderAll === beforeStatus.renderAll + 1 &&
        calls.fetchChart === beforeStatus.fetchChart + 1 &&
        calls.resetLogWindow === beforeStatus.resetLogWindow + 1 &&
        w.eval('logArchive.length') === 0);
      check('a status change invalidates the memoized filter-set identity',
        w.eval('explorerFilters()') !== statusFiltersBefore);

      // The status filter lives outside the hash but is part of the full
      // scope. After doFilter's full render establishes the new scope, a
      // later by-only change must still stay on the explorer-only path.
      const beforePostStatus = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      w.navigateTo([{ dim: 'provider', id: 'nav-b.example' }, { dim: 'by', id: 'client' }]);
      await settleUntil(() => calls.renderExplorer > 0 &&
        d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'client');
      check('by-only navigation after a status change keeps the explorer-only path',
        calls.renderAll === beforePostStatus.renderAll &&
        calls.fetchChart === beforePostStatus.fetchChart &&
        calls.resetLogWindow === beforePostStatus.resetLogWindow);
    } finally {
      w.renderAll = orig.renderAll;
      w.fetchChart = orig.fetchChart;
      w.renderChart = orig.renderChart;
      w.resetLogWindow = orig.resetLogWindow;
      w.renderDrawer = orig.renderDrawer;
      w.renderExplorer = orig.renderExplorer;
      w.fetchExplorer = orig.fetchExplorer;
      w.fetch = orig.fetch;
      fstatus.value = '';
      fstatus.dispatchEvent(new w.Event('change'));
      w.closeDrawer();
    }
    w.navigateTo([]);
    await sleep(20);
    check('the by-only suite restores the unfiltered log window',
      w.eval('logArchive.length') === 0 && rows().length === 3);
  }
};
