// Delegated operator actions repaint in place, so the clicked node stays
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function delegatedActions(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, loaded, mkRec, modelFixture, observerFixture, pageOptions, settleUntil, sleep} = H;
  const state = H.state;

  // Delegated operator actions repaint in place, so the clicked node stays
  // connected through the action itself. Outside-click dismissal still uses
  // the original event path rather than live target ancestry, for both edits
  // and pending/confirmed mutations.
  for (const [kind, action] of [['pause','pause-edit'], ['pause','pause-resume'], ['debug','debug-edit'], ['debug','debug-stop'], ['limits','limit-edit']]) {
    const isolated = dashboardDOM(assembleHTML(), pageOptions);
    await loaded(isolated);
    const sw = isolated.window, sd = sw.document;
    try {
      await settleUntil(() => sw.eval('!!lastData'));
      let resolveMutation;
      const originalFetch = sw.fetch;
      sw.fetch = (url, options) => options?.method === 'POST'
        ? new Promise(resolve => { resolveMutation = resolve; }) : originalFetch(url, options);
      const pause = {ok:true,paused:true,holds:[{id:'first',all:true,duration:'15m',max_queued:7}],known_clients:['c'],known_providers:['p']};
      const debug = observerFixture({ok:true,enabled:true,sessions:[{id:'first',clients:['c'],duration:'15m'},{id:'second',clients:['second'],duration:'1h'}],known_clients:['c','second'],known_models:[]});
      sw.applyPauseState(pause);
      sw.applyDebugState(debug);
      sw.applyThrottleState({ok:true,active:true,known_providers:['p'],throttles:[{provider:'p',requests:3,request_window:'2m'}]});
      const toggle = kind === 'pause' ? sw.togglePauseMenu : kind === 'debug' ? sw.toggleDebugMenu : sw.toggleLimitsMenu;
      toggle({stopPropagation(){}});
      const menu = sd.getElementById(kind + '-menu');
      const button = menu.querySelector('[data-operator="' + action + '"]');
      let originalPathInside = false;
      sd.addEventListener('click', e => { if (e.target === button) originalPathInside = e.composedPath().includes(menu); });
      button.click();
      check(action + ' keeps its clicked row in place and stays open through its own delegated click',
        button.isConnected && originalPathInside && !menu.hidden);
      if (resolveMutation) {
        const confirmed = kind === 'pause' ? {...pause,paused:false,holds:[]} : {...debug,sessions:[debug.sessions[1]]};
        resolveMutation({ok:true,json:async()=>confirmed});
        await sleep(5); // negative proof: a confirmed scoped mutation must not close the menu
        check(action + ' remains open after confirmed scoped mutation', !menu.hidden);
      }
      sd.querySelector('main').click();
      check(action + ' still closes on a genuine outside click', menu.hidden);
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
      const banner = sd.getElementById('storm-banner');
      check('embedded bootstrap exposes compact actionable provider storm summaries',
        banner && !banner.hidden && banner.getAttribute('role') === 'status' && banner.getAttribute('aria-live') === 'polite' &&
        banner.textContent.includes('neutral.example · all models') && banner.textContent.includes('http_503') &&
        banner.textContent.includes('75% failed attempts') && banner.textContent.includes('9 requests affected') &&
        banner.textContent.includes('4 queued') && banner.querySelector('button[aria-haspopup="dialog"]'));
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
        sw.togglePauseMenu({stopPropagation(){}});
        sw.applyBootstrapState({...state,
          pause: {ok:true, paused:true, clients: [], providers: ['neutral.example'],
            holds: [{id:'q1', all:false, new:false, clients: [], providers: ['neutral.example'], known_at_new: [],
              duration:'', until:null, max_queued:0, reason:'insufficient_quota', queued:0}],
            known_clients: [], known_providers: ['neutral.example'], until: null, queued: 0, default_max_queued: 0}});
        check('manual-mode quota holds show the system reason beside their scope',
          sd.getElementById('pf-holds').textContent.includes('neutral.example · insufficient_quota'));
        sw.togglePauseMenu({stopPropagation(){}});
        sw.applyBootstrapState(state);
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
      sw.toggleDebugMenu({stopPropagation(){}});
      sw.editDebugSession('dbg-safe');
      check('editing Debug retains an allowed preset duration', sd.getElementById('df-dur').value === '15m');
      let release;
      adminReply = new Promise(resolve => {release = resolve;});
      const mutation = sw.stopDebugSession('dbg-safe');
      sw.stopDebugSession('dbg-safe');
      sw.applyDebugState({...debug, enabled:false,sessions:[]});
      check('Debug busy gate disables related actions and ignores duplicate submissions/stale polls',
        adminCalls === 1 && sd.getElementById('btn-debug-stop').disabled && sw.eval('debugState.sessions.length') === 1);
      release({ok:false,json:async()=>({error:'disk unavailable'})});
      await mutation;
      check('failed Debug stop preserves its confirmed session, draft, and visible error',
        sw.eval('debugState.sessions.length') === 1 && sw.eval('debugEditID') === 'dbg-safe' && sd.getElementById('debug-count').textContent.includes('disk unavailable'));
      const oldRevisions = sw.captureOperatorRevisions();
      adminReply = Promise.resolve({ok:true,json:async()=>({...debug, enabled:false,sessions:[],warning:'not persisted'})});
      await sw.stopDebugMenu();
      sw.applyDebugState(debug, oldRevisions.debug);
      check('successful Debug warnings remain visible and late pre-mutation state cannot restore the session',
        sw.eval('debugState.sessions.length') === 0 && !sd.getElementById('debug-menu').hidden && sd.getElementById('debug-count').textContent.includes('not persisted'));

      const hostile = 'x'.repeat(227) + "');window.__operatorXSS=1;//";
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],throttles:[{provider:hostile,requests:2,request_window:'2m',anti_burst:true,ramp_duration:'10s'}]});
      sw.toggleLimitsMenu({stopPropagation(){}});
      sd.querySelector('#lim-holds [data-operator]').click();
      check('provider actions are inert delegated data, including quotes and script text',
        !sw.__operatorXSS && sd.getElementById('lim-provider').value === hostile && !sd.querySelector('#lim-holds [onclick]'));
      check('Limits custom duration survives edit and refresh', sd.getElementById('lim-reqw').value === '2m');
      check('a 255-byte provider keeps its stored anti-burst controls exact and inert',
        hostile.length === 255 && sd.getElementById('lim-antiburst').checked &&
        sd.getElementById('lim-ramp').value === '10s' &&
        sd.getElementById('lim-count').textContent === 'anti-burst 10s' &&
        !sw.__operatorXSS);
      for (const [value,want] of [['1e3',1000],['1.5',NaN],['-1',NaN],['',0]]) {
        sd.getElementById('lim-conc').value=value;
        check('Limits whole-number parsing: '+JSON.stringify(value), Object.is(sw.limitNum('lim-conc'),want));
      }
      sd.getElementById('lim-conc').value='-1';
      const beforeInvalid=adminCalls;
      sw.applyLimitsMenu();
      check('invalid Limits input is rejected before any request', adminCalls===beforeInvalid && sd.getElementById('lim-count').textContent.includes('whole numbers'));
      adminReply=Promise.resolve({ok:false,json:async()=>({error:'limit write failed'})});
      await sw.clearLimitsMenu();
      check('failed Limits clear preserves the confirmed limit and shows an error', sw.eval('throttleState.throttles.length')===1 && sd.getElementById('lim-count').textContent.includes('limit write failed'));
      // GAP-B pin: operatorCountClear's limits repaint is load-bearing, not
      // behavior-neutral: updateLimitCount early-returns while the count line
      // carries the error flag, so the err clear in the shared reset is what
      // lets a later provider edit repaint the live facts. Pin the whole
      // sequence: failed write (err flagged) -> provider edit -> repaint.
      check('the failed Limits write flags the count line as an error', sd.getElementById('lim-count').dataset.err === '1');
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],
        throttles:[{provider:hostile,source:'ui',in_flight:1,queued:2,requests:10,requests_capacity:20,requests_remaining:5}]});
      const errLine = sd.getElementById('lim-count').textContent;
      sw.onLimitProviderChange();
      check('a provider edit repaints the limits count line after a failed write',
        !sd.getElementById('lim-count').dataset.err &&
          sd.getElementById('lim-count').textContent === 'set in UI · in-flight 1 · queued 2 · 5/20 req' &&
          errLine.includes('limit write failed'));

      // The anti-burst pair rides the same full-form Apply: a stored ramp
      // survives with its flag off, an active cap sends both fields, and an
      // empty cap form clears them so no on-ramp follows a zero limit.
      {
        const policyState = policy => ({ok:true,active:true,known_providers:['provider-a'],
          throttles:[{provider:'provider-a',source:'ui',concurrency:2,requests:3,request_window:'2m',
            tokens:0,token_window:'',...policy}]});
        const anti = sd.getElementById('lim-antiburst');
        const ramp = sd.getElementById('lim-ramp');
        sw.applyThrottleState(policyState({anti_burst:false,ramp_duration:'10s'}));
        sw.syncLimitsMenuState();
        sw.onLimitProviderChange();
        check('editing a provider keeps a stored ramp selected while its flag is off',
          !anti.checked && ramp.value === '10s' &&
          sd.getElementById('lim-conc').value === '2' &&
          sd.getElementById('lim-req').value === '3' &&
          sd.getElementById('lim-reqw').value === '2m');
        anti.checked = true;
        ramp.value = '30s';
        let releaseApply;
        adminReply = new Promise(resolve => { releaseApply = resolve; });
        const applyIndex = adminBodies.length;
        const applying = sw.applyLimitsMenu();
        check('the Limits busy gate disables both anti-burst controls and Apply',
          adminBodies.length === applyIndex + 1 && anti.disabled && ramp.disabled &&
          sd.getElementById('btn-limits-apply').disabled);
        releaseApply({ok:true,json:async()=>policyState({anti_burst:true,ramp_duration:'30s'})});
        await applying;
        check('Limits Apply sends the exact active-cap body with its anti-burst policy',
          JSON.stringify(JSON.parse(adminBodies[applyIndex])) === JSON.stringify({
            provider:'provider-a', concurrency:2, requests:3, request_window:'2m',
            tokens:0, token_window:'', anti_burst:true, ramp_duration:'30s',
          }));
        check('the confirmed anti-burst policy repaints both controls and the count line',
          anti.checked && ramp.value === '30s' &&
          sd.getElementById('lim-count').textContent === 'set in UI · anti-burst 30s');
        sw.applyThrottleState(policyState({anti_burst:true,ramp_duration:'30s'}));
        sw.onLimitProviderChange();
        for (const id of ['lim-conc','lim-req','lim-tok']) sd.getElementById(id).value = '0';
        anti.checked = true;
        ramp.value = '30s';
        adminReply = Promise.resolve({ok:true,json:async()=>({
          ok:true,active:false,known_providers:['provider-a'],throttles:[]})});
        const clearIndex = adminBodies.length;
        await sw.applyLimitsMenu();
        check('an empty cap form clears anti-burst instead of sending an on-ramp',
          JSON.stringify(JSON.parse(adminBodies[clearIndex])) === JSON.stringify({
            provider:'provider-a', concurrency:0, requests:0, request_window:'',
            tokens:0, token_window:'', anti_burst:false, ramp_duration:'',
          }));
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

      sw.hideHdrMenu('limits-menu');
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
      let watchedSignal, readerCanceled=0;
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
      await sw.restartProxy();
      check('a rejected restart cancels its already-open watcher and releases the reader',
        await settleUntil(() => watchedSignal?.aborted && readerCanceled===1 &&
          sd.getElementById('restart-count').textContent.includes('restart rejected')));
      sw.fetch=savedFetch;

      const OriginalDate=sw.Date, oldTZ=process.env.TZ;
      process.env.TZ='America/Toronto';
      try {
        for (const [now,yesterday,older] of [
          ['2026-03-09T00:30:00-04:00','2026-03-08T12:00:00-04:00','2026-03-07T12:00:00-05:00'],
          ['2026-11-02T00:30:00-05:00','2026-11-01T12:00:00-05:00','2026-10-31T12:00:00-04:00'],
        ]) {
          sw.Date=class extends OriginalDate {constructor(...args){super(...(args.length?args:[now]));}};
          check('Yesterday uses calendar subtraction across DST '+now,
            sw.dayDividerLabel({start:yesterday})==='YESTERDAY' && sw.dayDividerLabel({start:older})!=='YESTERDAY');
        }
      } finally {sw.Date=OriginalDate;if(oldTZ===undefined)delete process.env.TZ;else process.env.TZ=oldTZ;}
    } finally {sw.close();}
  }
};
