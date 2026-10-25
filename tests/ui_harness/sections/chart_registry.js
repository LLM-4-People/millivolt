// test 12: chart preset registry - ids unique, dropdown follows
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function chartRegistry(H) {
  const {check, d, fire, mkRec, observerFixture, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 12: chart preset registry - ids unique, dropdown follows ----
  const presetIds = w.eval('CHART_PRESETS.map(p => p.id)');
  check('CHART_PRESETS ids are unique', new Set(presetIds).size === presetIds.length);
  const presetOpts = [...d.querySelectorAll('#chart-preset option')].map(o => o.value);
  check('#chart-preset has no duplicate options', new Set(presetOpts).size === presetOpts.length && presetOpts.length === presetIds.length);

  // ---- test 12b: the derive cache invalidates on a filter change - the
  // filter-set identity rides the memo key, so narrowing the status dropdown
  // must re-derive the scoped log even with zero record mutations.
  {
    const origF = w.fetch;
    state.fullPayload = { feed_id: 'feedB', seq: 3,
      records: [{ ...mkRec('mix1', 200, 1700000200000), __seq: 1 },
                { ...mkRec('mix2', 500, 1700000201000), __seq: 2 },
                { ...mkRec('mix3', 200, 1700000202000), __seq: 3 }],
      counters: { in_flight: 0, total_requests: 3, total_errors: 1 } };
    w.eval("fetchBootstrap('full')");
    await settleUntil(() => w.eval('derive(lastData).scopedRecs.length') === 3);
    w.eval("filters.status = ''");
    const allN = w.eval('derive(lastData).scopedRecs.length');
    const fstatus = d.getElementById('f-status');
    fstatus.value = '500';
    fstatus.dispatchEvent(new w.Event('change'));
    check('derive cache invalidates on a filter change', await settleUntil(() => allN === 3 &&
      w.eval('derive(lastData).scopedRecs.length') === 1 &&
      rows().every(tr => tr.dataset.id === 'mix2')));
    fstatus.value = '';
    fstatus.dispatchEvent(new w.Event('change'));
    await settleUntil(() => w.eval('derive(lastData).scopedRecs.length') === 3);
    w.fetch = origF;
  }

  // ---- test 12b-bis: the unfiltered derivation aliases the ring instead
  // of copying it on every live revision, while filtered scopes still build
  // their own array and an abnormal nil-bearing ring still yields nil-free
  // ordered rows. The 10k case proves the common path makes zero copies.
  {
    check('unfiltered derive aliases the live ring without a copy',
      w.eval('derive(lastData).scopedRecs === lastData.records'));

    const fstatus = d.getElementById('f-status');
    fstatus.value = '500';
    fstatus.dispatchEvent(new w.Event('change'));
    check('a filtered derive returns its own array with exactly the matching records',
      await settleUntil(() => w.eval('derive(lastData).scopedRecs !== lastData.records') &&
        w.eval('derive(lastData).scopedRecs.length') === 1 &&
        w.eval('derive(lastData).scopedRecs[0].id') === 'mix2' &&
        w.eval('derive(lastData).scopedRecs.every(r => r.status_code === 500)')));
    fstatus.value = '';
    fstatus.dispatchEvent(new w.Event('change'));
    await settleUntil(() => w.eval('derive(lastData).scopedRecs.length') === 3);

    // The incremental painter consumes the alias directly (it slices before
    // reversing); the unfiltered window keeps the ring's arrival order.
    w.eval('renderRequestsIncremental(derive(lastData).scopedRecs)');
    check('incremental rendering accepts the aliased ring and keeps row order',
      rows().map(tr => tr.dataset.id).join() === 'mix3,mix2,mix1');

    // A nil entry is abnormal but must never leak into the scoped rows; the
    // fallback copy preserves order. A local fixture keeps lastData intact.
    const nilRecs = [mkRec('nil0'), null, mkRec('nil1'), undefined, mkRec('nil2')];
    w.__aliasNilFixture = { records: nilRecs, counters: {}, _rev: 1 };
    try {
      const nilScoped = w.eval('derive(window.__aliasNilFixture).scopedRecs');
      check('a nil-bearing ring falls back to a nil-free ordered copy',
        nilScoped !== nilRecs && nilScoped.length === 3 &&
        nilScoped.map(r => r.id).join() === 'nil0,nil1,nil2');
    } finally { delete w.__aliasNilFixture; }

    // Identity at the 10k ring cap: no N-record copy and no push loop.
    const bigRecs = Array.from({ length: 10000 }, (_, i) => mkRec('big' + i));
    w.__aliasBigFixture = { records: bigRecs, counters: {}, _rev: 1 };
    try {
      check('the 10k unfiltered derivation is the ring array itself',
        w.eval('derive(window.__aliasBigFixture).scopedRecs === window.__aliasBigFixture.records') &&
        w.eval('derive(window.__aliasBigFixture).scopedRecs.length') === 10000 &&
        w.eval('derive(window.__aliasBigFixture).scopedRecs[9999] === window.__aliasBigFixture.records[9999]'));
    } finally { delete w.__aliasBigFixture; }
  }

  // An idle SSE connection sends cursor/counter metadata after the HTML seed.
  // It must not invalidate the shared derivation or schedule another render.
  {
    await settleUntil(() => w.eval('!!lastData'));
    const originalSchedule = w.scheduleRenderLive;
    let scheduled = 0;
    w.scheduleRenderLive = () => { scheduled++; };
    const rev = w.eval('lastData._rev || 0');
    const counters = w.eval('lastData.counters');
    const derived = w.eval('derive(lastData)');
    const cursor = w.eval('lastSeq') + 1;
    const payload = { feed_id: w.eval('feedId'), seq: cursor,
      pending_revision: w.eval('_pendingRevision'), incremental: true, records: [], in_flight_records: [], counters: { ...counters } };
    w.applySnapshotPayload(observerFixture(payload));
    check('empty SSE replay advances metadata without deriving or rendering',
      scheduled === 0 && w.eval('lastData._rev || 0') === rev &&
      w.eval('lastSeq') === cursor && w.eval('lastData.seq') === cursor &&
      w.eval('derive(lastData)') === derived);
    w.applySnapshotPayload(observerFixture({ ...payload, pending_revision: w.eval('_pendingRevision') + 1,
      counters: { ...counters, in_flight: (counters.in_flight || 0) + 1 } }));
    check('a counter-only snapshot still invalidates and schedules the live band',
      scheduled === 1 && w.eval('lastData._rev || 0') === rev + 1 &&
      counters.in_flight !== w.eval('lastData.counters.in_flight'));
    w.applySnapshotPayload(observerFixture(payload));
    scheduled = 0;
    w.applySnapshotPayload(observerFixture({ ...payload, in_flight_records: [{ ...mkRec('seed-pending', 0), live: true }] }));
    check('a pending-only snapshot still paints the in-flight request', scheduled === 1 &&
      w.eval('lastData.records.some(r => r.id === "seed-pending" && r.live)'));
    w.scheduleRenderLive = originalSchedule;
    fire('end', { record: mkRec('seed-pending', 200), in_flight: counters.in_flight || 0 });
    await settleUntil(() => !w.eval('lastData.records.some(r => r.id === "seed-pending" && r.live)'));
  }

  // ---- test 12c: overlapping bootstraps resolve by ISSUE order - a stale
  // in-flight delta that lands after a newer full resync must never re-apply
  // its (pre-resync) state over the fresh one.
  {
    const origSup = w.fetch;
    const bootResp = reqs => ({ ok: true, json: async () => ({
      records: [], counters: { in_flight: 0, total_requests: reqs, total_errors: 0 },
      seq: 5, feed_id: 'feedB', incremental: false,
      kpi: { requests: reqs, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0 },
      dash: {}, pause: {}, throttle: {},
    }) });
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;'); // no tick may race the two calls
    let resA = null, resB = null;
    w.fetch = () => {
      if (!resA) return new Promise(res => { resA = () => res(bootResp(111)); });
      if (!resB) return new Promise(res => { resB = () => res(bootResp(222)); });
      return Promise.resolve(bootResp(222)); // any other caller gets fresh state
    };
    w.eval("fetchBootstrap('resume')"); // issued first, resolves LAST
    w.eval("fetchBootstrap('resume')"); // issued second, resolves first
    resB();
    check('fresh bootstrap applies',
      await settleUntil(() => w.eval('kpiAgg && kpiAgg.requests') === 222));
    resA();
    // Negative proof: the late stale response must not overwrite 222.
    await sleep(10);
    check('stale bootstrap response is superseded', w.eval('kpiAgg && kpiAgg.requests') === 222);
    w.fetch = origSup;
    w.eval('armDashboardTicks()');
  }

  // Pause mutations must not apply error documents, overlap each other, or
  // lose to an older bootstrap response. These drive the actual menu handlers.
  {
    const originalFetch = w.fetch;
    const hold = {id: 'pause-fixture', all: true, max_queued: 0};
    const paused = {ok: true, paused: true, holds: [hold], queued: 0};
    const resumed = {ok: true, paused: false, holds: [], queued: 0};
    const settle = () => new Promise(resolve => setTimeout(resolve, 20));
    d.getElementById('pause-menu').hidden = false;
    w.applyPauseState(paused);
    w.fetch = async () => ({ok: false, json: async () => ({error: 'resume rejected'})});
    w.resumePauseHold(hold.id);
    await settle();
    check('failed Resume keeps the confirmed hold and displays its error',
      w.pauseActive() && d.getElementById('pause-count').textContent.includes('resume rejected'));

    let replyMutation, posts = 0;
    w.fetch = () => { posts++; return new Promise(resolve => { replyMutation = resolve; }); };
    w.resumePauseMenu();
    w.resumePauseMenu();
    w.resumePauseHold(hold.id);
    check('pause mutations share one busy gate and disable every resume action', posts === 1 &&
      d.getElementById('btn-pause-resume').disabled &&
      [...d.querySelectorAll('#pf-holds button')].every(b => b.disabled));
    // State refreshes cannot enable controls while a mutation is outstanding.
    w.refreshFooterState();
    w.syncPauseMenuState();
    check('pause polling and footer refresh preserve the busy state', d.getElementById('btn-pause-resume').disabled);
    replyMutation({ok: true, json: async () => resumed});
    await settle();
    check('successful Resume uses the confirmed server state', !w.pauseActive());

    let replyBootstrap;
    w.applyPauseState(paused);
    w.fetch = url => String(url).includes('/metrics/bootstrap')
      ? new Promise(resolve => { replyBootstrap = resolve; })
      : Promise.resolve({ok: true, json: async () => resumed});
    w.fetchBootstrap('none');
    w.resumePauseMenu();
    await settle();
    replyBootstrap({ok: true, json: async () => ({pause: paused, dash: {}})});
    await settle();
    check('a pre-Resume bootstrap cannot resurrect the resumed hold', !w.pauseActive());

    w.applyPauseState(resumed);
    w.resetPauseMenuForm();
    d.getElementById('pf-scope').value = 'all';
    w.onPauseScopeChange();
    d.getElementById('pf-cap').value = '-1';
    posts = 0;
    w.fetch = async () => { posts++; return {ok: true, json: async () => paused}; };
    w.applyPauseMenu();
    await settle();
    check('an invalid queue cap is rejected instead of silently becoming unlimited', posts === 0 &&
      d.getElementById('pause-count').textContent.includes('queue cap'));
    w.applyPauseState({...paused, holds: [{...hold, until: new Date(Date.now() - 1000).toISOString()}]});
    check('expired holds stop showing paused even before the next server poll', !w.pauseActive());
    const named = {id: 'named-hold', clients: ['client-a'], providers: ['old.example']};
    w.applyPauseState({...paused, holds: [named], known_clients: ['client-a'], known_providers: ['old.example', 'new.example']});
    w.resetPauseMenuForm();
    const clientChoice = d.querySelector('#pf-clients input[value="client-a"]');
    check('pause permits selecting a reused client for a disjoint provider scope', clientChoice && !clientChoice.disabled);
    clientChoice.checked = true;
    d.querySelector('#pf-providers input[value="new.example"]').checked = true;
    w.syncPauseMenuState();
    check('pause state refresh preserves the selected cross-scope draft',
      w.selectedCheckboxValues('pf-clients').includes('client-a') && w.selectedCheckboxValues('pf-providers').includes('new.example'));
    w.applyPauseState({...paused, holds: [{...named, new: true, known_at_new: ['client-a']}]});
    w.editPauseHold(named.id);
    let scopedPauseBody;
    w.fetch = async (url, options) => {
      scopedPauseBody = JSON.parse(options.body);
      return {ok: true, json: async () => resumed};
    };
    await w.applyPauseMenu();
    check('editing a scoped new-client hold preserves both explicit dimensions', scopedPauseBody.new &&
      scopedPauseBody.clients.includes('client-a') && scopedPauseBody.providers.includes('old.example'));
    w.applyPauseState(paused);
    w.fetch = async () => ({ok: true, json: async () => ({...resumed, warning: 'applied in memory but not saved'})});
    await w.resumePauseMenu();
    check('pause persistence warnings retain confirmed runtime state and remain visible', !w.pauseActive() &&
      !d.getElementById('pause-menu').hidden && d.getElementById('pause-count').textContent.includes('not saved'));
    w.applyPauseState(resumed);
    w.resetPauseMenuForm();
    d.getElementById('pause-menu').hidden = true;
    w.fetch = originalFetch;
  }
};
