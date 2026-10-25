// operator unlock: one answered dialog covers every in-flight 401
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function operatorUnlock(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, d, dashboardDOM, loaded, modelFixture, observerFixture, pageOptions, sleep, w} = H;
  const state = H.state;

  // ---- operator unlock: one answered dialog covers every in-flight 401
  // (a slow scan whose 401 lands after the unlock must not ask again), and a
  // rejected token says so instead of silently asking the same question ----
  {
    const authState = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'feed-auth', seq: 1,
      pending_revision: 0, incremental: false, records: [], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 0}, kpi: {requests: 0}, dash: {},
      model_canon: modelFixture(), storm: {enabled: false, banner_enabled: false, storms: []},
      storage: {enabled: true, dropped: 0},
    });
    const isolated = dashboardDOM(assembleHTML(authState), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      win.EventSource = function(url) {
        return {url, handlers: {}, readyState: 1, addEventListener() {}, close() {this.readyState = 2;}};
      };
    }});
    await loaded(isolated);
    const w = isolated.window, d = w.document;
    const submitDialog = value => {
      d.getElementById('operator-dialog-input').value = value;
      d.getElementById('operator-dialog-form').dispatchEvent(new w.Event('submit', {cancelable: true}));
    };
    try {
      await sleep(30);
      // Freeze the page's own timers so only test-driven calls hit fetch.
      w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null; clearInterval(_clockTimer); _clockTimer = null;');
      w.eval('storeOperatorCredential("")');
      const calls = [];
      let chartGate = null, chartPending = null; // gate exactly one chart request (the slow scan)
      let bootGate = null, bootPending = null;   // gate exactly one bootstrap request (in-flight credential race)
      w.fetch = (url, options) => {
        const auth = options && options.headers ? (options.headers.Authorization || '') : '';
        calls.push({url: String(url), auth});
        if (String(url).includes('/metrics/agg/chart') && chartGate) {
          chartPending = [];
          chartGate = null;
          const gate = chartPending;
          return new Promise(resolve => gate.push(resolve));
        }
        if (String(url).includes('/metrics/bootstrap') && bootGate) {
          bootPending = [];
          bootGate = null;
          const gate = bootPending;
          return new Promise(resolve => gate.push(resolve));
        }
        if (auth === 'Bearer op-token-correct') return Promise.resolve({ok: true, status: 200, json: async () => ({})});
        return Promise.resolve({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      };
      let modalOpens = 0;
      const realOpenModal = w.openModal;
      w.openModal = el => { if (el.id === 'operator-dialog') modalOpens++; return realOpenModal(el); };

      // The unlock: bootstrap's 401 opens the dialog; the chart scan stays in
      // flight and its 401 lands only after the credential was stored.
      const boot = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      chartGate = [];
      const chart = w.operatorFetch('/metrics/agg/chart?window=60m');
      await sleep(5);
      check('a 401 opens the operator dialog once',
        !d.getElementById('operator-dialog').hidden && modalOpens === 1 && w.eval('operatorPrompt !== null'));
      submitDialog('op-token-correct');
      const bootResult = await boot;
      check('unlock retries the original request with the stored credential',
        bootResult.status === 200 && calls.some(c => c.url.includes('/metrics/bootstrap') && c.auth === 'Bearer op-token-correct'));
      check('the dialog closed after unlock',
        d.getElementById('operator-dialog').hidden && w.eval('operatorPrompt === null'));
      chartPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      const chartResult = await chart;
      const chartCalls = calls.filter(c => c.url.includes('/metrics/agg/chart'));
      check('a late 401 after unlock retries silently without a second dialog',
        chartResult.status === 200 && chartCalls.length === 2 && chartCalls[1].auth === 'Bearer op-token-correct' &&
        d.getElementById('operator-dialog').hidden && w.eval('operatorPrompt === null'));

      // A stored credential rejected while a request is in flight: the silent
      // retry must drop it (other callers must not keep burning the server's
      // wrong-credential throttle with it) and the next prompt says so. The
      // request itself still presents the phase-1 credential; the wrong one is
      // stored while it pends, so it is the silent retry that presents (and
      // gets rejected for) the wrong value.
      bootGate = [];
      const stale = w.operatorFetch('/metrics/bootstrap'); // in flight presenting the phase-1 credential
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-wrong")'); // stored while pending
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      check('a rejected silent retry drops the stored credential and shows the notice',
        w.eval('operatorCredential') === '' && !d.getElementById('operator-dialog').hidden &&
        d.querySelector('.operator-dialog-note').textContent.includes('token was rejected'));
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      check('cancel after a rejected silent retry returns the 401 with nothing stored',
        (await stale).status === 401 && w.eval('operatorCredential') === '' && w.eval('operatorRejected === false'));

      // A silent retry whose 401 lands after a NEWER credential was stored
      // (another caller's successful unlock) must wipe only the token it
      // presented - a 401 indicts the presented value, never the current one.
      bootGate = [];
      const raced = w.operatorFetch('/metrics/bootstrap'); // in flight with no credential
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-wrong")'); // the silent retry will present this
      bootGate = [];                                       // gate the silent retry itself
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-correct")'); // a newer unlock lands while it flies
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      check('a late rejection does not wipe a newer stored credential',
        w.eval('operatorCredential') === 'op-token-correct' && !d.getElementById('operator-dialog').hidden);
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      check('cancel keeps the newer credential',
        (await raced).status === 401 && w.eval('operatorCredential') === 'op-token-correct');

      // The post-prompt wipe is value-guarded too: a retry presenting an
      // older token must not wipe a newer one stored while it flew.
      w.eval('storeOperatorCredential("")');
      const gen = w.operatorFetch('/metrics/bootstrap'); // 401 -> dialog
      await sleep(5);
      bootGate = [];                                     // gate the upcoming post-prompt retry
      submitDialog('op-token-wrong');                    // stores W, dispatches the retry
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-correct")'); // a newer generation's unlock
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      check('a rejected post-prompt retry does not wipe a newer stored credential',
        w.eval('operatorCredential') === 'op-token-correct' && w.eval('operatorRejected === true'));
      check('the raced post-prompt caller returns its 401', (await gen).status === 401);

      // The dialog presents the entered value VERBATIM (the server never
      // trims credentials): edge whitespace must reach the wire untouched.
      w.eval('storeOperatorCredential("")');
      const padded = ' padded-token ';
      const paddedCall = w.operatorFetch('/metrics/bootstrap'); // 401 (no credential) -> dialog
      await sleep(5);
      const savedPadFetch = w.fetch;
      w.fetch = (url, options) => {
        const auth = options && options.headers ? (options.headers.Authorization || '') : '';
        calls.push({url: String(url), auth});
        if (auth === 'Bearer ' + padded) return Promise.resolve({ok: true, status: 200, json: async () => ({})});
        return Promise.resolve({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      };
      submitDialog(padded);
      check('the dialog presents the token verbatim without trimming',
        (await paddedCall).status === 200 && w.eval('operatorCredential') === padded &&
        calls.some(c => c.auth === 'Bearer ' + padded));
      w.fetch = savedPadFetch;

      // A rejected entry is visible on the next prompt instead of a silent
      // re-ask that reads as "nothing happened".
      w.eval('storeOperatorCredential("")');
      const wrong = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      submitDialog('op-token-wrong');
      const wrongResult = await wrong;
      check('a wrong entry clears the credential and returns the 401',
        wrongResult.status === 401 && w.eval('operatorCredential') === '');
      const retry = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      const note = d.querySelector('.operator-dialog-note');
      check('the re-prompt after a rejection shows the failure notice',
        !d.getElementById('operator-dialog').hidden && note.hasAttribute('data-err') &&
        note.textContent.includes('token was rejected'));
      submitDialog('op-token-correct');
      check('the corrected entry unlocks', (await retry).status === 200);
      w.eval('storeOperatorCredential("")');
      const fresh = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      check('a fresh prompt drops the rejection notice',
        !note.hasAttribute('data-err') && note.textContent.includes('This dashboard is protected'));
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      check('cancel returns the original 401 without storing anything',
        (await fresh).status === 401 && w.eval('operatorCredential') === '');

      // Concurrent gated calls share one dialog and one entry.
      const opensBefore = modalOpens;
      const a = w.operatorFetch('/metrics/bootstrap');
      const b = w.operatorFetch('/admin/pause');
      await sleep(5);
      check('concurrent 401s share one dialog and one entry',
        !d.getElementById('operator-dialog').hidden && modalOpens === opensBefore + 1);
      submitDialog('op-token-correct');
      check('one entry resolves every joined caller with the credential',
        (await a).status === 200 && (await b).status === 200 && modalOpens === opensBefore + 1);
    } finally {w.close();}
  }

  // ---- dash_background_refresh: the default pauses the tick while hidden;
  // the opt-in keeps it running at the browser's throttled cadence and holds
  // the documented Chromium freeze-exemption Web Lock until visible again ----
  {
    const bgState = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'feed-bg', seq: 1,
      pending_revision: 0, incremental: false, records: [], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 0}, kpi: {requests: 0}, dash: {},
      model_canon: modelFixture(), storm: {enabled: false, banner_enabled: false, storms: []},
      storage: {enabled: true, dropped: 0},
    });
    const isolated = dashboardDOM(assembleHTML(bgState), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      win.EventSource = function(url) {
        return {url, handlers: {}, readyState: 1, addEventListener() {}, close() {this.readyState = 2;}};
      };
      win.__lockRequests = [];
      win.Object.defineProperty(win.navigator, 'locks', {configurable: true, value: {
        request(name, cb) {
          win.__lockRequests.push({name, released: false});
          const entry = win.__lockRequests[win.__lockRequests.length - 1];
          // The real lock releases when the callback's promise settles.
          return Promise.resolve().then(() => cb({release() { entry.released = true; }})).then(() => { entry.released = true; });
        },
      }});
    }});
    await loaded(isolated);
    const w = isolated.window, d = w.document;
    const setHidden = hidden => {
      w.Object.defineProperty(d, 'hidden', {configurable: true, get: () => hidden});
      d.dispatchEvent(new w.Event('visibilitychange'));
    };
    try {
      await sleep(30);
      w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null; clearInterval(_clockTimer); _clockTimer = null;');
      w.fetch = () => new Promise(() => {}); // ticks stay pending; only state is asserted
      w.eval('armDashboardTicks()');
      // Default off: hidden tears the tick down and never locks.
      setHidden(true);
      check('default hidden pauses the tick without any lock',
        w.eval('_dashTickTimer === null') && w.__lockRequests.length === 0);
      setHidden(false);
      w.eval('armDashboardTicks()');
      // Opt-in: hidden keeps the tick and holds the freeze-exemption lock.
      w.applyDashValues({dash_background_refresh: true});
      setHidden(true);
      await sleep(5); // the lock stub grants in a microtask
      check('background refresh keeps the tick running while hidden and holds the lock',
        w.eval('_dashTickTimer !== null') && w.__lockRequests.length === 1 && w.eval('_bgLockRelease !== null'));
      setHidden(false);
      await sleep(5);
      check('returning to view releases the lock and re-arms the tick',
        w.eval('_bgLockRelease === null') && w.eval('_dashTickTimer !== null') &&
        w.__lockRequests[0].released === true);
      // Hot-reloaded toggle-off releases the hold and stops the tick even
      // while still hidden.
      setHidden(true);
      await sleep(5);
      w.applyDashValues({dash_background_refresh: false});
      await sleep(5); // the stub marks released on promise settlement
      check('a hot-reloaded toggle-off releases the lock and the tick while hidden',
        w.__lockRequests.length === 2 && w.__lockRequests[1].released === true &&
        w.eval('_bgLockRelease === null') && w.eval('_dashTickTimer === null'));

      // A grant landing for a superseded hold (hide/show cycled while the
      // grant was pending) must self-release, not steal the live claim.
      setHidden(false);
      await sleep(5);
      w.applyDashValues({dash_background_refresh: true}); // visible: no auto-activation
      setHidden(true);   // request #3, grant pending
      setHidden(false);  // release: no live claim yet, no-op
      setHidden(true);   // request #4
      await sleep(5);    // both grants land
      check('a grant for a superseded hold self-releases without stealing the live claim',
        w.__lockRequests.length === 4 && w.__lockRequests[2].released === true &&
        w.__lockRequests[3].released === false && w.eval('_bgLockRelease !== null'));
      setHidden(false);
      await sleep(5);
      check('the live claim releases on return to view',
        w.eval('_bgLockRelease === null') && w.__lockRequests[3].released === true &&
        w.eval('_dashTickTimer !== null'));

      // A grant landing after its hold was released (no newer hold taken)
      // must self-release too: it must never hold the lock while visible.
      setHidden(true);   // request #5, grant pending
      setHidden(false);  // release with no live claim: still invalidates the pending grant
      await sleep(5);
      check('a grant landing after release self-releases instead of holding while visible',
        w.__lockRequests.length === 5 && w.__lockRequests[4].released === true &&
        w.eval('_bgLockRelease === null'));

      // A cadence change in a payload that lands while hidden with
      // background refresh off must not re-arm the tick: payload paths stay
      // live while hidden (an in-flight resume fetch, the SSE reset path),
      // and the visibilitychange handler only runs on transitions.
      w.applyDashValues({dash_background_refresh: false});
      setHidden(true);
      w.applyDashValues({dash_poll_interval: '7s'});
      check('a cadence change while hidden with background refresh off does not re-arm the tick',
        w.eval('_dashTickTimer === null') && w.__lockRequests.length === 5);
      // Symmetric activation: the flag flipped on while already hidden (an
      // out-of-band config change) arms the tick and takes the hold with no
      // visibilitychange transition.
      w.applyDashValues({dash_background_refresh: true});
      await sleep(5); // the lock stub grants in a microtask
      check('a hot-reloaded toggle-on while hidden takes the lock and arms the tick',
        w.eval('_dashTickTimer !== null') && w.__lockRequests.length === 6 &&
        w.eval('_bgLockRelease !== null'));
      // With the opt-in on, the same cadence change re-arms the tick. Stop
      // it first so a non-null timer proves the flag-on arm ran: phase 2's
      // activation arm would otherwise satisfy this vacuously.
      w.eval('dashStopDashboardTick()');
      w.applyDashValues({dash_poll_interval: '11s'});
      check('a cadence change while hidden with background refresh on re-arms the tick',
        w.eval('_dashTickTimer !== null'));
    } finally {w.close();}
  }
};
