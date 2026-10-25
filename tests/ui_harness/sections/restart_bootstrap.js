// // The Restart control's eligibility rides the payload the page ALREADY
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function restartBootstrap(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, loaded, mkRec, observerFixture, pageOptions, settleUntil, sleep} = H;
  const state = H.state;

  // The Restart control's eligibility rides the payload the page ALREADY
  // loads: a fresh page must decide before the menu is ever opened, without a
  // second round trip. A -trimpath image (no source tree, no go toolchain)
  // therefore never paints the control at all, while a build that reports
  // nothing keeps today's behaviour.
  {
    const base = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'restart-seed', seq: 1, pending_revision: 0,
      incremental: false, records: [mkRec('restart-seed')], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 1, total_errors: 0},
      kpi: {requests: 1, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    const trimpath = 'built with -trimpath, so the binary cannot locate its own source; ' +
      'self-restart is unavailable (restart via your process manager)';
    for (const [label, restart] of [
      ['ineligible', {available: false, reason: trimpath}],
      ['eligible', {available: true, reason: ''}],
      ['silent', undefined],
    ]) {
      let bootstrapFetches = 0;
      const seed = restart ? {...base, restart} : base;
      const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions,
        beforeParse(win) {
          pageOptions.beforeParse(win);
          const fallback = win.fetch;
          win.fetch = (url, options) => {
            if (String(url).includes('/metrics/bootstrap')) bootstrapFetches++;
            return fallback(url, options);
          };
          win.EventSource = function() {
            return {readyState: 1, addEventListener() {}, close() {}};
          };
        },
      });
      await loaded(isolated);
      await sleep(100);
      const wrap = isolated.window.document.getElementById('restart-wrap');
      check(`${label} seed decides the Restart control from the payload it already loads`,
        bootstrapFetches === 0 && !!wrap && wrap.hidden === !!(restart && restart.available === false));
      if (label === 'ineligible') {
        check('the ineligible verdict also keeps the action itself disabled',
          isolated.window.document.getElementById('btn-restart-now').disabled);
      }
      isolated.window.close();
    }
  }

  // State-only bootstrap changes must refresh the log even when the optional
  // operator snapshots are absent. Their render side effects cannot own dash
  // config or model-rule application, and an empty SSE replay remains a no-op.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'state-only-feed', seq: 17,
      incremental: false,
      records: Array.from({length: 17}, (_, i) => ({
        ...mkRec('state' + i, 200, 1700000000000 + i * 1000),
        model: i === 0 ? 'canonical-model' : 'raw-model',
      })),
      in_flight_records: [], counters: {in_flight: 0, total_requests: 17, total_errors: 0},
      kpi: {requests: 17, errors: 0}, dash: {dash_log_rows: 20}, model_canon: {rules: []},
    };
    let payload = {...seed, incremental: true, records: [], dash: {dash_log_rows: 10}};
    const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions,
      beforeParse(win) {
        pageOptions.beforeParse(win);
        const fallback = win.fetch;
        win.fetch = (url, options) => String(url).includes('/metrics/bootstrap')
          ? Promise.resolve({ok: true, json: async () => JSON.parse(JSON.stringify(String(url).includes('since=') ? payload : {...payload, incremental: false, records: seed.records}))})
          : fallback(url, options);
        win.EventSource = function() { return {readyState: 1, addEventListener() {}, close() {}}; };
      },
    });
    await loaded(isolated);
    const sw = isolated.window;
    const stateRows = () => [...sw.document.querySelectorAll('#tbl-requests tr.exp-row[data-id]')];
    try {
      await settleUntil(() => stateRows().length === 17);
      check('operator-free HTML seed applies dashboard config on its first paint', stateRows().length === 17);
      sw.fetchBootstrap('resume');
      await settleUntil(() => sw.eval('dashCfg.log_rows') === 10 && stateRows().length === 10);
      check('dash-only incremental bootstrap refreshes the log without operator snapshots',
        sw.eval('dashCfg.log_rows') === 10 && stateRows().length === 10);
      sw.navigateTo([{dim: 'model', id: 'canonical-model'}]);
      await settleUntil(() => stateRows().length === 1);
      check('model-only bootstrap fixture starts with one in-scope row', stateRows().length === 1);
      payload = {...payload, model_canon: {rules: [{mode: 'exact', from: 'raw-model', to: 'canonical-model'}]}};
      sw.fetchBootstrap('resume');
      await settleUntil(() => stateRows().length === 10 && stateRows().some(row => row.dataset.id !== 'state0'));
      check('model-only incremental bootstrap refreshes scoped rows without operator snapshots',
        stateRows().length === 10 && stateRows().some(row => row.dataset.id !== 'state0'));
      payload = {...payload, dash: {dash_log_rows: 20}};
      sw.fetchBootstrap('none');
      await settleUntil(() => stateRows().length === 17);
      check('state-only restart bootstrap also refreshes the log without replacing records', stateRows().length === 17);
      let liveRenders = 0;
      const originalLive = sw.renderLive;
      sw.renderLive = () => { liveRenders++; originalLive(); };
      payload = {...payload, incremental: false, records: seed.records};
      sw.fetchBootstrap('full');
      await sleep(30);
      check('a full bootstrap replacement does not queue a second live render', stateRows().length === 17 && liveRenders === 0);
    } finally {
      sw.close();
    }
  }

  // Purge is a feed-epoch transition, including in another tab. Each source
  // retains its own queued handlers so these tests can deliver stale callbacks
  // after close(), as well as an older bootstrap that completes after SSE.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'purge-epoch-1', seq: 2, pending_revision: 0,
      incremental: false, records: [mkRec('delete-me'), mkRec('survivor')],
      in_flight_records: [], counters: {in_flight: 0, total_requests: 2, total_errors: 0},
      kpi: {requests: 2, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    let payload = seed, delayNext = false, resolveOld;
    const streams = [], requests = [];
    const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions,
      beforeParse(win) {
        pageOptions.beforeParse(win);
        const fallback = win.fetch;
        win.fetch = (url, options) => {
          if (!String(url).includes('/metrics/bootstrap')) return fallback(url, options);
          requests.push(String(url));
          const captured = JSON.parse(JSON.stringify(payload));
          const reply = {ok: true, json: async () => captured};
          if (delayNext) {
            delayNext = false;
            return new Promise(resolve => { resolveOld = () => resolve(reply); });
          }
          return Promise.resolve(reply);
        };
        win.EventSource = function(url) {
          const stream = {url, readyState: 1, handlers: {},
            addEventListener(type, callback) {this.handlers[type] = callback;},
            close() {this.readyState = 2;}};
          streams.push(stream);
          return stream;
        };
      },
    });
    await loaded(isolated);
    const sw = isolated.window;
    const emit = (stream, type, data, seq) => stream.handlers[type]({type, data: JSON.stringify(observerFixture(data)), lastEventId: seq});
    const IDs = () => sw.eval('lastData.records.map(r => r.id).join(",")');
    try {
      await sleep(50);
      const original = streams.at(-1);
      delayNext = true;
      sw.fetchBootstrap('resume');
      payload = {...seed, feed_id: 'purge-epoch-2', records: [mkRec('survivor')]};
      emit(original, 'snapshot', payload);
      await sleep(20);
      const replacement = streams.at(-1);
      check('a changed SSE epoch closes and repins the stream URL exactly once',
        streams.length === 2 && original.readyState === 2 && replacement.url.includes('feed=purge-epoch-2') && replacement.url.includes('since=2'));
      for (const type of ['begin', 'update', 'end']) {
        emit(original, type, {record: mkRec('deleted-' + type), in_flight: 99});
      }
      emit(original, 'record', mkRec('deleted-record'), '99');
      emit(original, 'snapshot', seed);
      original.onerror();
      resolveOld();
      await sleep(20);
      check('old stream lifecycle/record/snapshot and older bootstrap cannot resurrect purged rows',
        IDs() === 'survivor' && sw.eval('feedId') === 'purge-epoch-2' && sw.eval('lastSeq') === 2 && sw.eval('lastData.counters.in_flight') === 0);

      // A server reset starts one immediate full refresh; no native retry delay
      // or second polling timer is needed for another tab's clear operation.
      payload = {...payload, feed_id: 'purge-epoch-3', seq: 3, records: [mkRec('during-purge')]};
      const before = requests.length;
      emit(replacement, 'reset', {feed_id: payload.feed_id});
      emit(replacement, 'record', mkRec('deleted-during-reset'), '100');
      await sleep(30);
      const resetStream = streams.at(-1);
      check('cross-tab reset immediately fetches one full snapshot and resumes the new epoch',
        requests.length === before + 1 && !requests.at(-1).includes('since=') &&
        IDs() === 'during-purge' && resetStream !== replacement && resetStream.url.includes('feed=purge-epoch-3'));
      emit(resetStream, 'snapshot', {...payload, incremental: true, seq: 4, records: [mkRec('after-purge')]});
      check('new arrivals between purge snapshot and SSE subscribe survive delta replay', IDs() === 'during-purge,after-purge');

      // The existing bootstrap poll is also sufficient if the stream is down.
      payload = {...payload, feed_id: 'purge-epoch-4', records: [mkRec('poll-survivor')]};
      sw.fetchBootstrap('resume');
      await sleep(20);
      emit(resetStream, 'end', {record: mkRec('deleted-poll-end'), in_flight: 7});
      check('poll-detected purge replaces the view and rejects the previous stream',
        IDs() === 'poll-survivor' && streams.at(-1).url.includes('feed=purge-epoch-4'));
    } finally {
      sw.close();
    }
  }
};
