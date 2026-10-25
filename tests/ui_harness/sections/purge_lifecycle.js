// Lifecycle and mandatory full refresh ordering share the real application
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function purgeLifecycle(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, loaded, mkRec, observerFixture, pageOptions, settleUntil} = H;
  const state = H.state;

  // Lifecycle and mandatory full refresh ordering share the real application
  // gates. Keep closed-source callbacks available to reproduce browser queues.
  {
    let state = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'ordered-feed', seq: 1,
      pending_revision: 0, incremental: false, records: [mkRec('ordered-base')], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 1, total_errors: 0},
      kpi: {requests: 1, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    const streams = [], pending = [];
    const isolated = dashboardDOM(assembleHTML(state), {...pageOptions,
      beforeParse(win) {
        pageOptions.beforeParse(win);
        const fallback = win.fetch;
        win.fetch = (url, options) => {
          if (!String(url).includes('/metrics/bootstrap')) return fallback(url, options);
          return new Promise((resolve, reject) => pending.push({url: String(url), resolve, reject}));
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
    const ids = () => sw.eval('lastData.records.map(r => r.id).join(",")');
    const reply = (request, payload) => request.resolve({ok: true, json: async () => JSON.parse(JSON.stringify(payload))});
    try {
      await settleUntil(() => streams.length > 0 && sw.eval('_dashTickTimer !== null'));
      sw.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
      const chartBefore = sw.eval('JSON.stringify(chartAgg)');
      const first = streams.at(-1);
      const completed = mkRec('ordered-final');
      emit(first, 'record', completed, '2');
      emit(first, 'begin', {record: mkRec('ordered-final', 0), in_flight: 1, pending_revision: 1});
      emit(first, 'update', {record: {...mkRec('ordered-final', 0), attempts: [{}]}, in_flight: 1, pending_revision: 2});
      emit(first, 'end', {record: completed, in_flight: 0, pending_revision: 3});
      check('record before buffered begin/update/end never regresses final state or double-folds the chart',
        chartBefore === sw.eval('JSON.stringify(chartAgg)') && sw.eval('lastData.records.find(r => r.id === "ordered-final").live') === false);

      emit(first, 'update', {record: {...mkRec('ordered-pending', 0), attempts: [{}, {}]}, in_flight: 2, pending_revision: 6});
      emit(first, 'begin', {record: mkRec('ordered-other', 0), in_flight: 1, pending_revision: 5});
      emit(first, 'begin', {record: mkRec('ordered-pending', 0), in_flight: 1, pending_revision: 4});
      check('delayed lifecycle rows still merge while retry progress and the global gauge stay monotonic',
        sw.eval('lastData.records.find(r => r.id === "ordered-pending").attempts.length') === 2 &&
        ids().includes('ordered-other') && sw.eval('lastData.counters.in_flight') === 2);
      sw.applySnapshotPayload({...state, incremental: true, records: [], pending_revision: 3,
        counters: {in_flight: 0, total_requests: 99, total_errors: 4}}, true);
      check('an older incremental snapshot cannot regress the gauge but retains unrelated counter updates',
        sw.eval('lastData.counters.in_flight') === 2 && sw.eval('lastData.counters.total_requests') === 99 &&
        sw.eval('lastData.counters.total_errors') === 4);
      sw.applySnapshotPayload({...state, seq: 2, records: [completed], pending_revision: 6,
        counters: {in_flight: 2, total_requests: 101, total_errors: 5}}, true);
      check('full replacements normalize through the same revision-gated counter owner',
        sw.eval('lastData.counters.in_flight') === 2 && sw.eval('lastData.counters.total_requests') === 101);

      state = {...state, feed_id: 'ordered-restarted'};
      sw.applySnapshotPayload(JSON.parse(JSON.stringify(state)), true);
      check('a new feed adopts its lower lifecycle revision before normalizing its gauge',
        sw.eval('_pendingRevision') === 0 && sw.eval('lastData.counters.in_flight') === 0);

      const beforeFull = streams.at(-1);
      const full = sw.fetchBootstrap('full');
      const fullRequest = pending.shift();
      let joined = 0;
      const resume = sw.fetchBootstrap('resume', () => joined++);
      const none = sw.fetchBootstrap('none', () => joined++);
      emit(beforeFull, 'record', mkRec('during-full'), '2');
      check('mandatory full refresh closes SSE at issue and ordinary polls join without superseding it',
        beforeFull.readyState === 2 && sw.eval('source') === null && pending.length === 0 &&
        ids() === 'ordered-base' && sw.eval('lastSeq') === 1);
      reply(fullRequest, state);
      await Promise.all([full, resume, none]);
      const resumed = streams.at(-1);
      emit(resumed, 'snapshot', {...state, incremental: true, seq: 2, records: [mkRec('during-full')]});
      check('full refresh reopens from its accepted cursor and replays intervening arrivals exactly once',
        joined === 2 && resumed !== beforeFull && resumed.url.includes('since=1') &&
        ids() === 'ordered-base,during-full' && sw.eval('lastSeq') === 2);

      const older = sw.fetchBootstrap('full'), olderRequest = pending.shift();
      const newer = sw.fetchBootstrap('full'), newerRequest = pending.shift();
      const streamCount = streams.length;
      reply(olderRequest, state);
      await older;
      check('an older full completion cannot reopen SSE or replace state during a newer full refresh',
        streams.length === streamCount && sw.eval('source') === null && ids().includes('during-full'));
      state = {...state, seq: 3, records: [mkRec('ordered-base'), mkRec('during-full'), mkRec('newest-full')]};
      reply(newerRequest, state);
      await newer;
      check('only the newest full completion owns stream reopening',
        streams.length === streamCount + 1 && streams.at(-1).url.includes('since=3') && ids().includes('newest-full'));

      const unexpected = sw.fetchBootstrap('resume');
      reply(pending.shift(), {...state, seq: 1, records: [mkRec('ordered-base')]});
      await settleUntil(() => pending.length > 0);
      const replacement = pending.shift();
      check('an unexpected full resume response uses the barrier without erasing newer live state',
        replacement && !replacement.url.includes('since=') && sw.eval('source') === null &&
        ids().includes('newest-full') && sw.eval('lastSeq') === 3);
      reply(replacement, state);
      await unexpected;

      const failed = sw.fetchBootstrap('full');
      pending.shift().resolve({ok: false, json: async () => ({error: 'unavailable'})});
      await failed;
      check('failed full refresh preserves rows and reopens SSE with a full-snapshot cursor',
        ids().includes('newest-full') && streams.at(-1).readyState === 1 && !streams.at(-1).url.includes('since='));

      // Native SSE reconnects can deliver a full snapshot after an HTTP poll
      // already advanced rows/cursor. Reject it before any state mutation.
      const oldReconnect = streams.at(-1), captured = JSON.parse(JSON.stringify(state));
      const poll = sw.fetchBootstrap('resume');
      const arrival = mkRec('http-arrival');
      state = {...state, seq: 4, records: [...state.records, arrival]};
      reply(pending.shift(), {...state, incremental: true, records: [arrival]});
      await poll;
      emit(oldReconnect, 'snapshot', captured);
      const recapture = sw.eval('_bootFull'), recaptureRequest = pending.shift();
      check('a delayed SSE full snapshot cannot erase a newer HTTP arrival or its advanced cursor',
        oldReconnect.readyState === 2 && sw.eval('source') === null && ids().includes('http-arrival') &&
        sw.eval('lastSeq') === 4 && sw.eval('lastData.counters.in_flight') === 0 && recaptureRequest);
      reply(recaptureRequest, state);
      await recapture;
      check('stale SSE full recapture reopens exactly from the fresh accepted snapshot',
        ids().includes('http-arrival') && streams.at(-1).url.includes('since=4') && pending.length === 0);

      const pendingSource = streams.at(-1), beforePending = JSON.parse(JSON.stringify(state));
      const held = {...mkRec('revision-pending', 0), paused: true, throttled: true, attempts: []};
      emit(pendingSource, 'begin', {record: held, in_flight: 1, pending_revision: 1});
      state = {...state, pending_revision: 1, in_flight_records: [held], counters: {...state.counters, in_flight: 1}};
      emit(pendingSource, 'snapshot', beforePending);
      const pendingRecapture = sw.eval('_bootFull'), pendingRequest = pending.shift();
      check('an equal-cursor SSE full snapshot cannot erase newer pending rows or their gauge',
        pendingSource.readyState === 2 && sw.eval('source') === null && ids().includes('revision-pending') &&
        sw.eval('lastSeq') === 4 && sw.eval('lastData.counters.in_flight') === 1 && pendingRequest);
      reply(pendingRequest, state);
      await pendingRecapture;
      check('a fresh full snapshot seeds each pending row with its authoritative lifecycle watermark',
        sw.eval('lastData.records.find(r => r.id === "revision-pending")._pendingRevision') === 1 && pending.length === 0);

      const updates = streams.at(-1);
      const released = {...held, paused: false, throttled: false};
      emit(updates, 'update', {record: released, in_flight: 1, pending_revision: 3});
      emit(updates, 'update', {record: held, in_flight: 1, pending_revision: 2});
      check('older equal-attempt updates cannot restore cleared pause or throttle state',
        sw.eval('lastData.records.find(r => r.id === "revision-pending").paused') === false &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending").throttled') === false);
      const snapshotPoll = sw.fetchBootstrap('resume');
      reply(pending.shift(), {...state, incremental: true, records: [], pending_revision: 2, in_flight_records: [held]});
      await snapshotPoll;
      check('an older HTTP pending snapshot cannot revert same-attempt pause/throttle progress',
        sw.eval('lastData.records.find(r => r.id === "revision-pending").paused') === false &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending")._pendingRevision') === 3);
      emit(updates, 'update', {record: {...held, attempts: [{}]}, in_flight: 1, pending_revision: 4});
      check('a newer row revision still advances pause, throttle and retry fields together',
        sw.eval('lastData.records.find(r => r.id === "revision-pending").paused') === true &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending").throttled') === true &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending").attempts.length') === 1);

      // The shared gate also protects an unexpectedly stale full HTTP result.
      // Its outer completion must not reopen while its replacement is pending.
      const staleHTTP = sw.fetchBootstrap('full');
      reply(pending.shift(), state); // revision1, older than accepted update4
      await settleUntil(() => pending.length > 0);
      const finalReplacement = pending.shift();
      const countBeforeReplacement = streams.length;
      check('a rejected full HTTP snapshot cannot reopen over its newer recapture owner',
        finalReplacement && sw.eval('source') === null &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending")._pendingRevision') === 4);
      state = {...state, pending_revision: 4, in_flight_records: [{...held, attempts: [{}]}]};
      reply(finalReplacement, state);
      await staleHTTP;
      check('only the accepted recapture reopens after a stale full HTTP result',
        streams.length === countBeforeReplacement + 1 && pending.length === 0 && sw.eval('_bootFull') === null);
    } finally {
      sw.close();
    }
  }
};
