// Delegated operator actions repaint in place, so the clicked node stays
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function delegatedActions(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, loaded, mkRec, modelFixture, observerFixture, pageOptions, settleUntil, sleep} = H;
  const state = H.state;

  // The console is the one action surface. A row action rebuilds the pane, so
  // the clicked node itself is expected to detach: identity is the row's id,
  // not the node. Edit opens that row's composer, remove/resume/stop posts
  // exactly one mutation and leaves the console open, and only a genuine
  // outside press dismisses it. Each case gets its own page so no operator
  // state leaks between them.
  for (const spec of [
    {
      kind: 'limit', section: 'limits', draft: 'limit', endpoint: '/admin/throttle',
      edit: '[data-oc-edit="limit"]', destroy: '[data-oc-remove="limit"]',
      rowEdit: '.oc-rule-main',
      compose: '.oc-compose[data-kind="limit"]',
      post: {id: 'first', clear: true},
      seed(sw) {
        sw.applyThrottleState({ok: true, active: true, known_providers: ['p'], throttles: [{
          id: 'first', scope: {providers: ['p']}, concurrency: 0, requests: 3,
          request_window: '2m', tokens: 0, token_window: '', anti_burst: false,
          ramp_duration: '', source: 'console', updated_by: '', in_flight: 0, queued: 0,
          requests_remaining: 1, requests_capacity: 3, tokens_remaining: 0, tokens_capacity: 0,
        }]});
      },
      confirmed: {ok: true, active: false, known_providers: ['p'], throttles: []},
      gone(sw) { return sw.eval('throttleState.throttles.length') === 0; },
    },
    {
      kind: 'pause', section: 'pause', draft: 'pause', endpoint: '/admin/pause',
      edit: '[data-oc-edit="pause"]', destroy: '[data-oc-resume="first"]',
      rowEdit: '.oc-rule-main',
      compose: '.oc-compose[data-kind="pause"]',
      post: {paused: false, id: 'first'},
      seed(sw) {
        sw.applyPauseState({ok: true, paused: true,
          holds: [{id: 'first', all: true, duration: '15m', max_queued: 7}],
          clients: [], providers: [], known_clients: ['c'], known_providers: ['p'],
          until: null, queued: 0, default_max_queued: 0});
      },
      confirmed: {ok: true, paused: false, holds: [], clients: [], providers: [],
        known_clients: ['c'], known_providers: ['p'], until: null, queued: 0, default_max_queued: 0},
      gone(sw) { return !sw.pauseActive() && sw.eval('pauseState.holds.length') === 0; },
    },
    {
      kind: 'capture', section: 'capture', draft: 'capture', endpoint: '/admin/debug',
      edit: '[data-oc-edit="capture"]', destroy: '[data-oc-stop="first"]',
      rowEdit: '.oc-rule-main',
      compose: '.oc-compose[data-kind="capture"]',
      post: {enabled: false, id: 'first'},
      seed(sw) {
        sw.applyDebugState(observerFixture({ok: true, enabled: true,
          sessions: [{id: 'first', clients: ['c'], providers: [], models: [], duration: '15m'}],
          known_clients: ['c'], known_providers: ['p'], known_models: []}));
      },
      confirmed: observerFixture({ok: true, enabled: false, sessions: [],
        known_clients: ['c'], known_providers: ['p'], known_models: []}),
      gone(sw) { return !sw.debugActive() && sw.eval('debugState.sessions.length') === 0; },
    },
  ]) {
    const isolated = dashboardDOM(assembleHTML(), pageOptions);
    await loaded(isolated);
    const sw = isolated.window, sd = sw.document;
    try {
      await settleUntil(() => sw.eval('!!lastData'));
      spec.seed(sw);
      const shell = sd.getElementById('operator-console');
      sw.openOperatorConsole(spec.section);
      const row = () => sd.querySelector('#oc-pane [data-oc-rule="first"]');
      check(spec.kind + ' rule row renders inside the console', !!row() && !shell.hidden);
      // The row itself is the one edit target: it opens the composer and the
      // clicked node detaches because the pane rebuilds, which is why identity
      // is the row id and never the element. A separate side "edit" button
      // would duplicate this action and take width the scope needs.
      const rowEdit = row().querySelector(spec.rowEdit);
      check(spec.kind + ' row has no duplicate side edit action',
        !row().querySelector('.oc-rule-actions ' + spec.edit));
      rowEdit.click();
      check(spec.kind + ' edit swaps in a composer that keeps the row identity',
        !rowEdit.isConnected && !!sd.querySelector('#oc-pane ' + spec.compose) &&
        sw.eval('ocDrafts.' + spec.draft + '.id') === 'first');
      sd.querySelector('#oc-pane [data-oc-cancel]').click();
      check(spec.kind + ' cancel closes the composer and restores the row',
        !sd.querySelector('#oc-pane ' + spec.compose) && !!row());
      const mainEdit = row().querySelector('.oc-rule-main');
      mainEdit.click();
      check(spec.kind + ' the row body is the same edit action as the side button',
        !mainEdit.isConnected && !!sd.querySelector('#oc-pane ' + spec.compose) &&
        sw.eval('ocDrafts.' + spec.draft + '.id') === 'first');
      sd.querySelector('#oc-pane [data-oc-cancel]').click();
      const posts = [];
      const originalFetch = sw.fetch;
      const originalConfirm = sw.ocConfirm;
      sw.ocConfirm = () => Promise.resolve(true);
      sw.fetch = (url, options) => {
        if (options?.method === 'POST') {
          posts.push({url: String(url), body: JSON.parse(options.body)});
          return Promise.resolve({ok: true, json: async () => spec.confirmed});
        }
        return originalFetch(url, options);
      };
      try {
        row().querySelector(spec.destroy).click();
        check(spec.kind + ' remove/resume/stop posts exactly one mutation',
          await settleUntil(() => posts.length === 1) &&
          posts[0].url === spec.endpoint &&
          JSON.stringify(posts[0].body) === JSON.stringify(spec.post));
        check(spec.kind + ' confirmed mutation clears the rule through the shared state owner',
          await settleUntil(() => spec.gone(sw)));
        await sleep(10); // negative proof: one confirmed mutation must not be followed by a second
        check(spec.kind + ' console stays open after its confirmed mutation',
          posts.length === 1 && !shell.hidden && sw.menuIsOpen(shell) && !row());
      } finally {
        sw.fetch = originalFetch;
        sw.ocConfirm = originalConfirm;
      }
      sd.querySelector('main').dispatchEvent(new sw.Event('pointerdown', {bubbles: true, composed: true}));
      sd.querySelector('main').click();
      check(spec.kind + ' console closes on a genuine outside press',
        shell.hidden && !sw.menuIsOpen(shell));
    } finally { sw.close(); }
  }

  // Observer authority, operator confirmation, and keyboard regressions.
  {
    const baseRules = [{mode: 'pattern', from: '(?P<name>a)', to: '${name}x'}];
    const baseCanon = modelFixture(baseRules, {' a ': 'ax', 'server-model': 'canonical'});
    const stormFixture = {enabled:true,banner_enabled:true,storms:[{
      provider:'neutral.example',model:'',scope:'provider',state:'open',reason:'http_503',quota:false,
      error_percent:75,error_requests:9,failures:15,samples:20,window_ms:60000,queued:4,
      active_models:3,affected_models:2,affected_model_percent:200/3,
      retry_at:'2026-09-05T12:00:00Z',recovery_successes:0,recovery_required:2,
    }]};
    let state = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'frontend-audit', seq: 1,
      pending_revision: 0, incremental: false, records: [{...mkRec('audit-row'), model: ' a ', time_bucket: 'night'}],
      in_flight_records: [], counters: {in_flight: 0, total_requests: 1}, kpi: {requests: 1}, dash: {},
      model_canon: baseCanon, storage: {enabled: true, dropped: 3}, storm:stormFixture,
    });
    const streams = [], requests = [];
    const adminBodies = [];
    let adminReply, adminCalls = 0, chartReply;
    const isolated = dashboardDOM(assembleHTML(state), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      const fallback = win.fetch;
      win.fetch = (url, options) => {
        if (String(url).includes('/metrics/bootstrap')) return new Promise(resolve => requests.push(resolve));
        if (String(url).includes('/metrics/agg/chart') && chartReply) return chartReply;
        if (String(url).startsWith('/admin/') && options?.method === 'POST' && adminReply) {adminCalls++; adminBodies.push(options.body); return adminReply;}
        return fallback(url, options);
      };
      win.EventSource = function(url) {
        const stream = {url, handlers: {}, readyState: 1, addEventListener(type, cb) {this.handlers[type] = cb;}, close() {this.readyState = 2;}};
        streams.push(stream); return stream;
      };
    }});
    await loaded(isolated);
    const sw = isolated.window, sd = sw.document;
    const emit = (type, data, seq) => streams.at(-1).handlers[type]({type, data: JSON.stringify(data), lastEventId: seq});
    const reply = p => requests.shift()({ok: true, json: async () => p});
    try {
      await settleUntil(() => sw.eval('!!lastData'));
      check('observer dictionary uses exact raw keys and Go named-capture output without JS execution',
        sw.canonicalModel(' a ') === 'ax' && sw.canonicalModel('a') === 'a');
      check('time scope uses server classification even when browser time differs',
        sw.recordMatchesDim(state.records[0], 'time', 'night') && !sw.recordMatchesDim({...state.records[0], time_bucket: 'work'}, 'time', 'night'));
      check('durable storage drops are discoverable in the existing footer',
        sd.getElementById('f-state').textContent.includes('3 storage drops') && sd.getElementById('f-state').title.includes('not saved durably'));
      sw.applyBootstrapState({...state, storage: {enabled: true, dropped: 0, totals_degraded: true}});
      check('a degraded boot scan is discoverable in the footer as totals degraded',
        sd.getElementById('f-state').textContent.includes('totals degraded') &&
        sd.getElementById('f-state').title.includes('startup history scan failed'));
      sw.applyBootstrapState({...state, storage: {enabled: true, dropped: 0}});
      check('a clean storage signal renders no degraded footnote',
        !sd.getElementById('f-state').textContent.includes('totals degraded') && !sd.getElementById('f-state').title);
      // A degraded operator-state restore is the twin of a degraded boot scan:
      // a durable hold or cap document existed and could not be read, so rules
      // saved before the restart are not in force. It must read apart from
      // "nothing is held", which is what an empty list would otherwise say.
      {
        const emptyPause = {ok: true, paused: false, holds: [], clients: [], providers: [],
          known_clients: [], known_providers: [], until: null, queued: 0,
          storm_waiters: 0, default_max_queued: 0};
        sw.applyPauseState({...emptyPause, restore_degraded: true});
        const footer = sd.getElementById('f-state');
        check('a degraded hold restore is named in the footer, not read as "nothing is held"',
          footer.textContent.includes('state degraded') &&
          footer.title.includes('could not be read at startup') &&
          !footer.textContent.includes('paused'));
        sw.applyPauseState({...emptyPause, restore_degraded: false});
        check('a healthy restore clears the degraded footnote',
          !sd.getElementById('f-state').textContent.includes('state degraded'));
        // The cap document reports the same fact through the same footnote.
        sw.applyThrottleState({ok: true, active: false, throttles: [], known_providers: [],
          known_clients: [], known_models: [], restore_degraded: true});
        check('a degraded cap restore reaches the same footnote',
          sd.getElementById('f-state').textContent.includes('state degraded'));
        sw.applyThrottleState({ok: true, active: false, throttles: [], known_providers: [],
          known_clients: [], known_models: [], restore_degraded: false});
        check('the footnote clears once both documents restore cleanly',
          !sd.getElementById('f-state').textContent.includes('state degraded'));
      }
      const banner = sd.getElementById('storm-banner');
      check('embedded bootstrap exposes compact actionable provider storm summaries',
        banner && !banner.hidden && banner.getAttribute('role') === 'status' && banner.getAttribute('aria-live') === 'polite' &&
        banner.textContent.includes('neutral.example · all models') && banner.textContent.includes('http_503') &&
        banner.textContent.includes('75% failed attempts') && banner.textContent.includes('9 requests affected') &&
        banner.textContent.includes('4 queued') && banner.querySelector('button[aria-haspopup="dialog"]') &&
        // Both state words come from one table now. Only the half-open branch
        // was ever asserted, so the open branch - the one that says requests
        // are still being held - went untested.
        banner.textContent.includes('Holding requests') &&
        sw.eval('stormStateWord("open")') === 'Holding requests' &&
        sw.eval('stormStateWord("half_open")') === 'Checking recovery' &&
        sw.eval('stormStateWord("unknown_state")') === 'unknown_state');
      const incidentButton = banner.querySelector('button');
      incidentButton.focus();
      incidentButton.click();
      const incidentDialog = sd.getElementById('storm-dialog');
      const detail = label => [...incidentDialog.querySelectorAll('dt')].find(node => node.textContent === label)?.nextElementSibling.textContent;
      const closeIncident = incidentDialog.querySelector('[data-operator="storm-close"]');
      check('incident click opens shared modal with distinct requests, attempt rates and provider evidence',
        await settleUntil(() => !incidentDialog.hidden && incidentDialog.getAttribute('role') === 'dialog' &&
          sd.activeElement === closeIncident && sd.querySelector('main').hasAttribute('inert') &&
          detail('Requests affected') === '9' && detail('Failed upstream attempts') === '15' &&
          detail('Sampled upstream attempts') === '20' && detail('Upstream attempt failure rate') === '75%' &&
          detail('Queued requests') === '4' && detail('Detection window') === '1m' && detail('Error') === 'http_503' &&
          detail('Next retry') === new sw.Date(stormFixture.storms[0].retry_at).toLocaleString() &&
          detail('Affected active models') === '2 / 3 (66.7%)'));
      const updatedStorm = {...stormFixture,storms:[{...stormFixture.storms[0],queued:7,error_requests:10,
        state:'half_open',recovery_successes:1,retry_at:'2026-09-05T12:00:30Z'}]};
      sw.applyBootstrapState({...state,storm:updatedStorm});
      check('accepted bootstrap updates the open incident while keeping close control and trigger focus identities',
        await settleUntil(() => sd.activeElement === closeIncident &&
          incidentDialog.querySelector('[data-operator="storm-close"]') === closeIncident &&
          banner.querySelector('button') === incidentButton && detail('Queued requests') === '7' &&
          detail('Requests affected') === '10' && detail('Successful recovery probes') === '1 / 2' &&
          detail('Next retry') === new sw.Date(updatedStorm.storms[0].retry_at).toLocaleString()));
      closeIncident.dispatchEvent(new sw.KeyboardEvent('keydown', {key:'Escape',bubbles:true,cancelable:true}));
      check('incident Escape closes shared modal and restores the current incident trigger',
        incidentDialog.hidden && sd.activeElement === incidentButton && !sd.querySelector('main').hasAttribute('inert'));
      incidentButton.click();
      sw.applyBootstrapState({...state,storm:{...stormFixture,storms:[]}});
      check('resolved incident stays reviewable without showing stale counts or stealing focus',
        await settleUntil(() => !incidentDialog.hidden &&
          incidentDialog.textContent.includes('no longer active') && !incidentDialog.querySelector('dl') &&
          sd.activeElement === closeIncident));
      closeIncident.click();
      check('resolved incident close restores a visible dashboard control',
        incidentDialog.hidden && (sd.activeElement === sd.getElementById('btn-settings') ||
          sd.activeElement === sd.getElementById('btn-nav')));

      const unspecifiedStorm = {...stormFixture,storms:[stormFixture.storms[0], {...stormFixture.storms[0],scope:'model',model:''}]};
      sw.applyBootstrapState({...state,storm:unspecifiedStorm});
      check('unspecified model incident remains distinct from the provider and does not hide valid incidents',
        banner.querySelectorAll('button').length === 2 && banner.textContent.includes('(unspecified model)'));
      banner.querySelectorAll('button')[1].click();
      check('unspecified model details preserve exact empty-model scope',
        await settleUntil(() => detail('Model scope') === '(unspecified model)'));
      closeIncident.click();

      const modelStorm = {...stormFixture,storms:[{...stormFixture.storms[0],provider:'<img src=x onerror=alert(1)>',
        model:'raw/model<script>fixture</script>',scope:'model',state:'half_open',reason:'transport',recovery_successes:1}]};
      sw.applyBootstrapState({...state,storm:modelStorm});
      check('model storm labels stay exact and inert in compact recovery summaries',
        banner.textContent.includes('raw/model<script>fixture</script>') && !banner.querySelector('img, script') &&
        banner.textContent.includes('Checking recovery') &&
        !banner.textContent.includes('active models affected'));
      banner.querySelector('button').click();
      check('incident details render hostile provider/model/error values as inert text',
        await settleUntil(() => detail('Provider') === modelStorm.storms[0].provider &&
          detail('Model scope') === modelStorm.storms[0].model &&
          detail('Successful recovery probes') === '1 / 2' && !incidentDialog.querySelector('img, script') &&
          !detail('Active models')));
      const stormRow = banner.querySelector('.storm-row');
      sw.applyBootstrapState({...state,storm:modelStorm});
      check('unchanged storm polls retain the rendered nodes', banner.querySelector('.storm-row') === stormRow);
      for (const storm of [{...stormFixture,enabled:false}, {...stormFixture,banner_enabled:false},
        {...stormFixture,storms:[]}, {...stormFixture,storms:[{...stormFixture.storms[0],error_percent:NaN}]},
        ...[{active_models:-1}, {affected_models:4}, {affected_model_percent:Infinity}, {error_requests:NaN},
          {error_requests:16}, {error_requests:-1}, {error_requests:'9'}].map(values =>
          ({...stormFixture,storms:[{...stormFixture.storms[0],...values}]})), undefined]) {
        sw.applyBootstrapState({...state,storm});
        check('disabled, recovered, missing or malformed storm state clears the banner', banner.hidden && !banner.textContent);
      }
      check('malformed incident updates invalidate open details without misreporting recovery',
        !incidentDialog.hidden && incidentDialog.textContent.includes('unavailable') && !incidentDialog.querySelector('dl'));
      closeIncident.click();
      sw.applyBootstrapState(state);
      const staleStormPoll = sw.fetchBootstrap('none'), freshStormPoll = sw.fetchBootstrap('none');
      const staleStormReply = requests.shift(), freshStormReply = requests.shift();
      freshStormReply({ok:true,json:async()=>({...state,storm:{...stormFixture,storms:[]}})});
      await freshStormPoll;
      staleStormReply({ok:true,json:async()=>state});
      await staleStormPoll;
      check('older bootstrap responses cannot restore recovered storm warnings', banner.hidden);
      sw.applyBootstrapState(state);

      // Retry-mode quota pause surface: banner facts, dialog entries, the
      // operator resume action, and the manual-mode hold's reason label.
      {
        const quotaStorm = {enabled:true,banner_enabled:true,storms:[{
          provider:'neutral.example',model:'',scope:'provider',state:'open',reason:'insufficient_quota',quota:true,
          error_percent:0,error_requests:0,failures:0,samples:0,window_ms:60000,queued:5,
          active_models:0,affected_models:0,affected_model_percent:0,
          retry_at:'2026-09-05T12:01:00Z',recovery_successes:0,recovery_required:1,
        }]};
        sw.applyBootstrapState({...state,storm:quotaStorm});
        check('quota gate rows report the pause and its recovery facts, not storm failure rates',
          !banner.hidden && banner.textContent.includes('neutral.example · all models') &&
          banner.textContent.includes('insufficient_quota') && banner.textContent.includes('Quota pause') &&
          banner.textContent.includes('5 queued') && banner.textContent.includes('0 / 1 recovery probes') &&
          !banner.textContent.includes('% failed attempts'));
        const quotaRow = banner.querySelector('button');
        quotaRow.click();
        check('quota incident dialog shows the pause title, probe facts and a resume action, without storm noise',
          await settleUntil(() => !incidentDialog.hidden &&
            sd.getElementById('storm-dialog-title').textContent === 'Provider quota pause' &&
            detail('Error') === 'insufficient_quota' && detail('Queued requests') === '5' &&
            detail('Successful recovery probes') === '0 / 1' && !detail('Failed upstream attempts') &&
            incidentDialog.querySelector('[data-operator="quota-resume"]')?.dataset.value === 'neutral.example'));
        const releaseQuota = Promise.resolve({ok:true,json:async()=>({enabled:true,banner_enabled:true,storms:[]})});
        adminReply = releaseQuota;
        const callsBeforeResume = adminCalls;
        incidentDialog.querySelector('[data-operator="quota-resume"]').click();
        check('operator resume posts to the quota endpoint and repaints from the confirmed storm snapshot',
          await settleUntil(() => adminCalls === callsBeforeResume + 1 && banner.hidden &&
            incidentDialog.textContent.includes('no longer active')));
        adminReply = Promise.resolve({ok:false,json:async()=>({error:'quota endpoint unavailable'})});
        sw.applyBootstrapState({...state,storm:quotaStorm});
        banner.querySelector('button').click();
        await settleUntil(() => !incidentDialog.hidden &&
          incidentDialog.querySelector('[data-operator="quota-resume"]'));
        incidentDialog.querySelector('[data-operator="quota-resume"]').click();
        check('a failed resume keeps the dialog open with the failure beside the action',
          await settleUntil(() => !incidentDialog.hidden &&
            incidentDialog.querySelector('[data-operator="quota-resume"]') &&
            incidentDialog.textContent.includes('quota endpoint unavailable')));
        closeIncident.click();
        adminReply = null;
        sw.applyBootstrapState({...state,
          pause: {ok:true, paused:true, clients: [], providers: ['neutral.example'],
            holds: [{id:'q1', all:false, new:false, clients: [], providers: ['neutral.example'], known_at_new: [],
              duration:'', until:null, max_queued:0, reason:'insufficient_quota', queued:0}],
            known_clients: [], known_providers: ['neutral.example'], until: null, queued: 0, default_max_queued: 0}});
        sw.openOperatorConsole('pause');
        const quotaRule = sd.querySelector('#oc-pane [data-oc-rule="q1"]');
        check('manual-mode quota holds show the system reason beside their scope',
          !!quotaRule && quotaRule.textContent.includes('neutral.example') &&
          quotaRule.textContent.includes('insufficient_quota'));
        sw.closeOperatorConsole();
        sw.applyPauseState({ok:true, paused:false, holds:[], clients:[], providers:[],
          known_clients: [], known_providers: [], until: null, queued: 0, default_max_queued: 0});
        // The quota surface consumed the shared admin POST tally; hand the
        // later busy-gate assertions the same zero baseline they assume.
        adminCalls = 0;
      }

      const event = {...mkRec('observer-new'), model: 'server-model', model_canon: {revision: baseCanon.revision, names: {'server-model': 'canonical'}}};
      emit('record', event, '2');
      check('record envelopes merge names before row adoption without retaining dictionary copies',
        sw.canonicalModel('server-model') === 'canonical' && !sw.eval('lastData.records.at(-1).model_canon'));
      sw.eval("modelNames.set('unretained', 'gone'); logArchive = [{model:'archive-only'}]; modelNames.set('archive-only','archive'); debugState.known_models=['debug-only']; modelNames.set('debug-only','debug'); _rebuildRecIdx();");
      check('model dictionary pruning preserves loaded/archive/debug names but removes abandoned names',
        !sw.eval("modelNames.has('unretained')") && sw.eval("modelNames.has('archive-only') && modelNames.has('debug-only')"));
      sw.resetLogWindow();
      check('archive reset releases dictionary names that no loaded surface needs', !sw.eval("modelNames.has('archive-only')"));

      const beforeRows = sw.eval('lastData.records.length'), beforeSeq = sw.eval('lastSeq');
      emit('record', {...mkRec('foreign-model'), model_canon: modelFixture([], {'m':'m'})}, '3');
      check('foreign observer revision cannot advance the cursor or mix differently grouped rows',
        sw.eval('lastSeq') === beforeSeq && sw.eval('lastData.records.length') === beforeRows && requests.length === 1 && sw.eval('source') === null);
      state = {...state, seq: 2, records: [state.records[0], event]};
      delete state.records[1].model_canon;
      reply(state);
      check('foreign observer metadata re-captures through the existing full barrier',
        await settleUntil(() => requests.length === 0 && streams.at(-1).url.includes('since=2')));

      // A reload between bootstrap and Debug serialization cannot install
      // mixed dictionary revisions or even expose that response's storage state.
      const mixed = sw.fetchBootstrap('full');
      reply({...state,storage:{enabled:true,dropped:99},debug:{ok:true,enabled:false,sessions:[],known_models:[],model_canon:modelFixture()}});
      await sleep(10); // negative proof: the rejected response must not adopt any state
      check('mixed bootstrap observer revisions are rejected before any state adoption',
        requests.length===1 && !sd.getElementById('f-state').textContent.includes('99 storage drops') && sw.eval('source')===null);
      reply(state);
      await mixed;
      check('mixed observer recapture keeps full-barrier reopening with its accepted owner', requests.length===0 && sw.eval('source')!==null);

      const debug = observerFixture({ok:true, enabled:true, sessions:[{id:'dbg-safe',clients:['c'],duration:'15m'}],known_clients:['c'],known_models:[],model_canon:baseCanon});
      sw.applyDebugState(debug);
      sw.openOperatorConsole('capture');
      const captureRow = () => sd.querySelector('#oc-pane [data-oc-rule="dbg-safe"]');
      const captureApply = () => sd.querySelector('#oc-pane [data-oc-apply="capture"]');
      captureRow().querySelector('.oc-rule-main').click();
      check('editing a capture keeps its row id and an allowed preset duration',
        sw.eval('ocDrafts.capture.id') === 'dbg-safe' &&
        sd.querySelector('#oc-pane [data-oc-choice="capture-duration"][data-oc-value="15m"]').getAttribute('aria-pressed') === 'true');
      let release;
      adminReply = new Promise(resolve => {release = resolve;});
      sw.ocStopSession('dbg-safe');
      sw.ocStopSession('dbg-safe');
      sw.applyDebugState({...debug, enabled:false,sessions:[]});
      check('capture busy gate disables the composer action and ignores duplicate submissions/stale polls',
        adminCalls === 1 && captureApply().disabled && sw.eval('debugState.sessions.length') === 1);
      release({ok:false,json:async()=>({error:'disk unavailable'})});
      check('failed capture stop preserves its confirmed session, edit draft, and names the error',
        await settleUntil(() => sw.eval('debugState.sessions.length') === 1 &&
          sw.eval('ocDrafts.capture.id') === 'dbg-safe' &&
          sd.querySelector('#oc-pane .oc-banner[role="alert"]')?.textContent.includes('disk unavailable')));
      const oldRevisions = sw.captureOperatorRevisions();
      adminReply = Promise.resolve({ok:true,json:async()=>({...debug, enabled:false,sessions:[],warning:'not persisted'})});
      sw.ocStopSession('dbg-safe');
      sw.applyDebugState(debug, oldRevisions.debug);
      check('successful capture warnings stay visible and late pre-mutation state cannot restore the session',
        await settleUntil(() => sw.eval('debugState.sessions.length') === 0 &&
          sd.querySelector('#oc-pane .oc-banner')?.textContent.includes('not persisted')) &&
        !sd.getElementById('operator-console').hidden);

      const hostile = 'x'.repeat(227) + "');window.__operatorXSS=1;//";
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],throttles:[{
        id:'hostile-limit',scope:{providers:[hostile]},concurrency:0,requests:2,request_window:'2m',
        tokens:0,token_window:'',anti_burst:true,ramp_duration:'10s',source:'console',updated_by:'',
        in_flight:0,queued:0,requests_remaining:0,requests_capacity:2,tokens_remaining:0,tokens_capacity:0,
      }]});
      sw.openOperatorConsole('limits');
      const hostileRow = sd.querySelector('#oc-pane [data-oc-rule="hostile-limit"]');
      check('a 255-byte provider is inert delegated data in the rule row, quotes and script text included',
        !!hostileRow && hostile.length === 255 &&
        hostileRow.textContent.includes(hostile) &&
        !sd.querySelector('#oc-pane [onclick]') && !sw.__operatorXSS);
      check('the hostile rule row keeps its stored window and budget wording',
        hostileRow.querySelector('.oc-rule-budget').textContent.includes('2 requests / 2m'));
      hostileRow.querySelector('.oc-rule-main').click();
      check('editing the hostile rule keeps its stored budgets, ramp and custom window',
        sd.querySelector('#oc-pane [data-oc-field="requests"]').value === '2' &&
        sw.eval('ocDrafts.limit.requestWindow') === '2m' &&
        // The preview and the row read one budget owner, so the same draft
        // prints the same sentence above and below it.
        sd.getElementById('oc-lim-preview').textContent.includes('2 requests / 2m') &&
        hostileRow.querySelector('.oc-rule-budget').textContent.includes('2 requests / 2m') &&
        sd.querySelector('#oc-pane [data-oc-field="antiBurst"]').checked &&
        sd.querySelector('#oc-pane [data-oc-choice="ramp"]').value === '10s' &&
        !sw.__operatorXSS);
      const reqInput = () => sd.querySelector('#oc-pane [data-oc-field="requests"]');
      for (const [value,want] of [['1e3',1000],['1.5',NaN],['-1',NaN],['',0]]) {
        reqInput().value = value;
        reqInput().dispatchEvent(new sw.Event('input', {bubbles: true}));
        check('limit whole-number parsing: ' + JSON.stringify(value),
          Object.is(sw.eval('ocDrafts.limit.requests'), value) && Object.is(sw.ocNumber(value), want));
      }
      reqInput().value = '-1';
      reqInput().dispatchEvent(new sw.Event('input', {bubbles: true}));
      const beforeInvalid = adminCalls;
      const invalidApply = sd.querySelector('#oc-pane [data-oc-apply="limit"]');
      check('a negative limit count disables Apply before any request',
        invalidApply.disabled && sw.eval('ocDrafts.limit.requests') === '-1');
      sw.eval('ocApplyLimit()');
      check('a negative limit count is rejected before any request',
        adminCalls === beforeInvalid && sd.querySelector('#oc-pane .oc-error').textContent.includes('whole numbers'));
      adminReply = Promise.resolve({ok:false,json:async()=>({error:'limit write failed'})});
      {
        const savedConfirm = sw.ocConfirm;
        sw.ocConfirm = () => Promise.resolve(true);
        try { await sw.ocRemoveLimit('hostile-limit'); } finally { sw.ocConfirm = savedConfirm; }
      }
      check('a failed limit remove keeps the confirmed rule and names the error in the banner',
        await settleUntil(() => !!sd.querySelector('#oc-pane .oc-banner[role="alert"]') &&
          sd.querySelector('#oc-pane .oc-banner').textContent.includes('limit write failed')) &&
        sw.eval('throttleState.throttles.length') === 1 &&
        !!sd.querySelector('#oc-pane [data-oc-rule="hostile-limit"]'));
      // GAP-B pin: the console's banner is the one rejection surface, and a
      // later accepted poll must clear it while the confirmed rule stays.
      check('the failed limit write paints the one console banner as an alert',
        sd.querySelector('#oc-pane .oc-banner').getAttribute('data-tone') === 'err');
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],
        throttles:[{id:'hostile-limit',scope:{providers:[hostile]},source:'console',in_flight:1,queued:2,
          concurrency:0,requests:10,request_window:'1m',tokens:0,token_window:'',anti_burst:false,ramp_duration:'',
          requests_capacity:20,requests_remaining:5,tokens_remaining:0,tokens_capacity:0}]});
      check('an accepted throttle poll repaints the live rule facts',
        await settleUntil(() => sd.querySelector('#oc-pane [data-oc-rule="hostile-limit"]')
          ?.querySelector('.oc-rule-live').textContent.includes('5 of 20 requests left')));
      sw.eval("ocOperatorErrorClear()");
      check('dismissing the rejection banner leaves the confirmed rule in place',
        !sd.querySelector('#oc-pane .oc-banner') &&
        !!sd.querySelector('#oc-pane [data-oc-rule="hostile-limit"]'));

      // The anti-burst pair rides the same composer Apply: a stored ramp
      // survives with its flag off, an active cap sends both fields, and an
      // empty form is refused so no rule is written without a budget.
      {
        const policyState = policy => ({ok:true,active:true,known_providers:['provider-a'],
          throttles:[{id:'burst-limit',scope:{providers:['provider-a']},source:'console',updated_by:'',
            concurrency:2,requests:3,request_window:'1m',tokens:0,token_window:'',
            in_flight:0,queued:0,requests_remaining:0,requests_capacity:3,
            tokens_remaining:0,tokens_capacity:0,...policy}]});
        const field = key => sd.querySelector(`#oc-pane [data-oc-field="${key}"]`);
        const choice = name => sd.querySelector(`#oc-pane [data-oc-choice="${name}"]`);
        const pickChoice = (name, value) => {
          const el = choice(name);
          el.value = value;
          el.dispatchEvent(new sw.Event('change', {bubbles: true}));
        };
        const applyButton = () => sd.querySelector('#oc-pane [data-oc-apply="limit"]');
        sw.applyThrottleState(policyState({anti_burst:false,ramp_duration:'10s'}));
        sw.ocOpenLimitComposer('burst-limit');
        check('editing a rule keeps a stored ramp selected while its flag is off',
          !field('antiBurst').checked && choice('ramp').value === '10s' &&
          field('concurrency').value === '2' && field('requests').value === '3' &&
          choice('request-window').value === '1m');
        field('antiBurst').checked = true;
        field('antiBurst').dispatchEvent(new sw.Event('change', {bubbles: true}));
        pickChoice('ramp', '30s');
        check('the anti-burst ramp picker derives its tokens from the window vocabulary',
          JSON.stringify([...choice('ramp').options].map(option => option.value)) ===
            JSON.stringify([''].concat(sw.eval('LIMIT_WINDOWS').map(([v]) => v))) &&
          choice('ramp').value === '30s');
        let releaseApply;
        adminReply = new Promise(resolve => { releaseApply = resolve; });
        const applyIndex = adminBodies.length;
        applyButton().click();
        check('the limits busy gate disables the composer Apply while the write is pending',
          adminBodies.length === applyIndex + 1 && applyButton().disabled &&
          sw.eval('operatorState.throttle.busy'));
        releaseApply({ok:true,json:async()=>policyState({anti_burst:true,ramp_duration:'30s'})});
        check('the confirmed anti-burst policy lands and leaves the composer',
          await settleUntil(() => !sw.eval('operatorState.throttle.busy') &&
            !sd.querySelector('#oc-pane .oc-compose[data-kind="limit"]') &&
            sd.querySelector('#oc-pane [data-oc-rule="burst-limit"]')));
        check('apply sends the exact active-cap body with its anti-burst policy',
          JSON.stringify(Object.entries(JSON.parse(adminBodies[applyIndex])).sort()) === JSON.stringify(Object.entries({
            id:'burst-limit',providers:['provider-a'],models:[],clients:[],key_hashes:[],conversations:[],
            days:[],start_minute:0,end_minute:0,concurrency:2,requests:3,request_window:'1m',
            tokens:0,token_window:'',anti_burst:true,ramp_duration:'30s',
          }).sort()));
        sw.applyThrottleState(policyState({anti_burst:true,ramp_duration:'30s'}));
        sw.ocOpenLimitComposer('burst-limit');
        check('an applied anti-burst rule reopens with both controls live',
          field('antiBurst').checked && !field('antiBurst').disabled &&
          !sd.querySelector('#oc-pane .oc-advanced .oc-note'));
        sw.eval("ocDrafts.limit.concurrency = ''; ocDrafts.limit.requests = ''; ocDrafts.limit.tokens = ''");
        const emptyIndex = adminBodies.length;
        const emptyAfter = adminCalls;
        sw.eval('ocApplyLimit()');
        check('an empty composer is refused before any request',
          adminBodies.length === emptyIndex && adminCalls === emptyAfter &&
          sd.querySelector('#oc-pane .oc-error').textContent.includes('at least one budget'));
      }

      const draft=sd.createElement('div');
      const rules=[{mode:'pattern',from:' x ',to:' y '},{mode:'exact',from:' padded ',to:' kept ',disabled:true},{mode:'lower',from:' parked ',to:' value ',disabled:true}];
      draft.innerHTML=sw.modelRulesEditorHTML(rules);
      check('model-rule collection preserves exact pattern/replacement and parked bytes',
        JSON.stringify(sw.collectModelRules(draft))===JSON.stringify(rules));
      const previewRules=[{mode:'pattern',from:'(a+)+$',to:'x'},{mode:'lower'}];
      draft.innerHTML=sw.modelRulesEditorHTML(previewRules);
      sw.validateModelRulesDraft(draft);
      check('draft preview never executes backtracking pattern rules', sw.mrDraftSteps(draft).length===1);
      check('basic Settings controls have accessible names', ['int','strings','duration'].every(kind => sw.settingsFieldHTML({kind,key:'sample',label:'Sample setting'},1).includes('aria-label="Sample setting"')));

      sw.closeOperatorConsole();
      for (const button of sd.querySelectorAll('.menu-wrap > button[aria-controls]')) {
        const menu = sd.getElementById(button.getAttribute('aria-controls'));
        sw.closeHeaderMenus();
        menu.hidden = false;
        sw.syncHdrMenuExpanded();
        (menu.querySelector('button:not(:disabled), input, select') || button).focus();
        sd.dispatchEvent(new sw.KeyboardEvent('keydown', {key:'Escape', bubbles:true, cancelable:true}));
        check('Escape closes ' + menu.id + ' and restores its trigger focus',
          menu.hidden && button.getAttribute('aria-expanded') === 'false' && sd.activeElement === button);
      }
      {
        // Theme tray structure: one current/preview comparator, the surface
        // radios in the readable caption bar, and honest Reset/Close actions.
        const menu = sd.getElementById('theme-menu');
        const panes = menu.querySelector('.theme-panes');
        const paneList = [...menu.querySelectorAll('.theme-pane')];
        const captionBar = menu.querySelector('.theme-caption-bar');
        check('theme comparator is one aria-hidden pair of shared panes',
          !!panes && panes.getAttribute('aria-hidden') === 'true' && paneList.length === 2 &&
          paneList[0].id === 'theme-current' && paneList[1].id === 'theme-preview' &&
          paneList.every(pane => pane.classList.contains('theme-pane') &&
            pane.querySelector('.tp-label')));
        check('theme panes carry one neutral series legend, not status pills',
          paneList.every(pane =>
            pane.querySelectorAll('.tp-legend .tp-key').length === 3 &&
            !pane.querySelector('.tp-pill') &&
            pane.querySelector('.tp-legend').textContent === '' &&
            !/200|live|429/.test(pane.textContent)));
        check('theme pane markup is identical apart from id and labels', (() => {
          const shape = pane => {
            const clone = pane.cloneNode(true);
            clone.removeAttribute('id');
            clone.removeAttribute('data-theme');
            clone.removeAttribute('data-mode');
            clone.querySelector('.tp-label').textContent = '';
            return clone.innerHTML;
          };
          return shape(paneList[0]) === shape(paneList[1]);
        })());
        check('theme captions stay outside the decorative comparator',
          !!captionBar && !panes.contains(captionBar) &&
          captionBar.querySelector('#theme-current-label') &&
          captionBar.querySelector('#theme-preview-label') &&
          !captionBar.querySelector('#theme-preview-label').classList.contains('has-preview'));
        check('theme surface radios sit in the caption bar under a visible label',
          captionBar.querySelector('#theme-mode[role="radiogroup"]') &&
          captionBar.querySelectorAll('.theme-mode-opt').length === 2 &&
          captionBar.querySelector('.theme-mode-label')?.textContent === 'Surface' &&
          sd.getElementById('theme-mode').getAttribute('aria-labelledby') === 'theme-mode-label');
        check('theme tray lists all eight palettes and offers Reset and Close',
          menu.querySelectorAll('.theme-opt').length === 8 &&
          sd.getElementById('theme-reset')?.textContent === 'Reset' &&
          sd.getElementById('theme-reset')?.title === 'Restore Instrument Black' &&
          sd.getElementById('theme-close')?.textContent === 'Close' &&
          !sd.getElementById('theme-done'));
        check('theme hint explains preview, pointer activation and Escape',
          menu.querySelector('.theme-hintbar')?.textContent ===
            'Arrows preview. Click or press Enter to apply. Esc closes.');
      }
      const open=sd.querySelector('button[data-open-req]');
      open.focus(); open.click();
      const drawer=sd.getElementById('drawer');
      check('keyboard-operable log button opens an accessible modal and makes background inert',
        await settleUntil(() => !drawer.hidden && !drawer.hasAttribute('inert') &&
          drawer.contains(sd.activeElement) && sd.querySelector('main').hasAttribute('inert')));
      const focus=sw.modalFocusables(drawer);
      focus.at(-1).focus();
      sd.dispatchEvent(new sw.KeyboardEvent('keydown',{key:'Tab',bubbles:true,cancelable:true}));
      check('modal Tab wraps to its first control', sd.activeElement===focus[0]);
      sw.closeDrawer();
      check('closing the drawer hides all controls and restores the initiating focus',
        drawer.hidden && drawer.hasAttribute('inert') && sd.activeElement===open && !sd.querySelector('main').hasAttribute('inert'));

      const gal=sd.getElementById('xp-gallery');
      const xbody=gal.closest('.xp-body');
      sd.documentElement.style.setProperty('--gallery-locked','');
      Object.defineProperty(gal,'scrollHeight',{configurable:true,value:1000});
      const firstCard=gal.firstElementChild;
      Object.defineProperty(firstCard,'offsetTop',{configurable:true,value:7});
      Object.defineProperty(firstCard,'getBoundingClientRect',{configurable:true,value:()=>({height:148})});
      if (gal.children[1]) {
        Object.defineProperty(gal.children[1],'offsetTop',{configurable:true,value:165});
        Object.defineProperty(gal.children[1],'getBoundingClientRect',{configurable:true,value:()=>({height:148})});
      }
      Object.defineProperty(xbody,'getBoundingClientRect',{configurable:true,value:()=>({top:20})});
      Object.defineProperty(gal,'getBoundingClientRect',{configurable:true,value:()=>({top:20})});
      sw.applyGallerySize(gal);
      const capped=xbody.style.height==='148px' && gal.style.height==='';
      Object.defineProperty(firstCard,'getBoundingClientRect',{configurable:true,value:()=>({height:148.25})});
      Object.defineProperty(gal,'getBoundingClientRect',{configurable:true,value:()=>({top:74.25})});
      sw.applyGallerySize(gal);
      const stacked=xbody.style.height==='203px';
      sd.documentElement.style.setProperty('--gallery-locked','1');
      sw.applyGallerySize(gal);
      check('gallery sizing follows the CSS-owned lock state and reserves stacked layout space',
        capped && stacked && xbody.style.height==='' && gal.style.height==='');
      sd.documentElement.style.removeProperty('--gallery-locked');
      Object.defineProperty(firstCard,'getBoundingClientRect',{configurable:true,value:()=>({height:148})});
      Object.defineProperty(gal,'getBoundingClientRect',{configurable:true,value:()=>({top:20})});
      Object.defineProperty(gal,'scrollHeight',{configurable:true,value:148});
      sw.applyGallerySize(gal);
      // The band stays one row even when the content fits: a scrollHeight-
      // conditional "natural" flip was the set-clear-set oscillation.
      const settled=xbody.style.height==='148px';
      check('the band stays one row even when the gallery content fits it', settled);

      sw.applyBootstrapState({...state, storage:{enabled:true,dropped:0}});
      check('zero durable drops clear the footer warning without a second counter',
        !sd.getElementById('f-state').textContent.includes('storage drops') && !sd.getElementById('f-state').title);
      sw.applyBootstrapState({...state, storage:{enabled:false,dropped:3}});
      check('disabled storage never reports stale drops', !sd.getElementById('f-state').textContent.includes('storage drops'));

      const settingsButton=sd.getElementById('btn-settings');
      settingsButton.focus();
      sw.toggleSettings({stopPropagation(){}});
      const sheet=sd.getElementById('settings-sheet');
      check('Settings shares modal focus containment and background inertness',
        await settleUntil(() => !sheet.hidden && sheet.contains(sd.activeElement) &&
          sd.querySelector('main').hasAttribute('inert')));
      sw.closeSettings(true);
      check('Settings closes with focus restored and controls removed from tab order',
        sheet.hidden && sheet.hasAttribute('inert') && sd.activeElement===settingsButton);
      const provider=sd.createElement('div');
      provider.innerHTML=sw.providersEditorHTML({'fixture.example':{usage_keys:{input_tokens:'usage.input'},models_keys:{input_modalities:'input'}}});
      sd.getElementById('settings-fields').appendChild(provider);
      for(const [kind,selector] of [['usage','.sp-ufield'],['model','.sp-mfield']]) {
        const field=provider.querySelector(selector), sec=field.closest('.prov-sec'), freed=field.value;
        field.closest('.prov-urow').remove();
        sw.syncMappingPicker(sec,kind);
        check('removing a '+kind+' mapping refreshes an already-present free-field picker',
          [...sec.querySelectorAll('select option')].some(option=>option.value===freed));
      }
      provider.remove();

      const savedFetch=sw.fetch;
      const savedRestartConfirm=sw.ocConfirm;
      let watchedSignal, readerCanceled=0;
      sw.ocConfirm = () => Promise.resolve(true);
      // The restart status line lives in the console's system section now, so
      // open it before driving the restart to read that surface.
      sw.openOperatorConsole('system');
      sw.fetch=(url, options)=>{
        if(String(url).includes('watch=1')) {
          watchedSignal=options.signal;
          return Promise.resolve({ok:true,body:{getReader(){return {
            read(){return new Promise((resolve,reject)=>watchedSignal.addEventListener('abort',()=>reject(new Error('aborted')),{once:true}));},
            cancel(){readerCanceled++;return Promise.resolve();},
          };}}});
        }
        if(String(url).includes('/admin/restart')) return Promise.resolve({ok:options?.method!=='POST',status:500,
          json:async()=>options?.method==='POST'?{error:'restart rejected'}:{available:true,phase:'idle',started_at:1}});
        return savedFetch(url,options);
      };
      try {
        await sw.restartProxy();
        check('a rejected restart cancels its already-open watcher and releases the reader',
          await settleUntil(() => watchedSignal?.aborted && readerCanceled===1 &&
            sd.getElementById('restart-count').textContent.includes('restart rejected')));
      } finally {
        sw.fetch=savedFetch;
        sw.ocConfirm=savedRestartConfirm;
      }

      const OriginalDate=sw.Date, oldTZ=process.env.TZ;
      process.env.TZ='America/Toronto';
      try {
        for (const [now,yesterday,older] of [
          ['2026-03-09T00:30:00-04:00','2026-03-08T12:00:00-04:00','2026-03-07T12:00:00-05:00'],
          ['2026-11-02T00:30:00-05:00','2026-11-01T12:00:00-05:00','2026-10-31T12:00:00-04:00'],
        ]) {
          sw.Date=class extends OriginalDate {constructor(...args){super(...(args.length?args:[now]));}};
          check('Yesterday uses calendar subtraction across DST '+now,
            sw.dayDividerLabel({start:yesterday})==='yesterday' && sw.dayDividerLabel({start:older})!=='yesterday');
        }
      } finally {sw.Date=OriginalDate;if(oldTZ===undefined)delete process.env.TZ;else process.env.TZ=oldTZ;}
    } finally {sw.close();}
  }
};
