// The five header operator menus were replaced by one operator console. This
// section drives the console's contract end to end: the always-visible header
// readout, the one panel lifecycle, the section rail, the three policy
// composers, export, overview, system, the shared error surface and the
// record entry points. It runs early, so it leaves every operator surface on
// the empty baseline the later sections expect.
module.exports = async function operatorMenus(H) {
  const {check, d, mkRec, modelFixture, observerFixture, settleUntil, w} = H;
  const state = H.state;
  const STATIC = H.STATIC;
  const fs = require('fs');
  const path = require('path');

  // ---------- local helpers ----------
  const q = sel => d.querySelector(sel);
  const qa = sel => [...d.querySelectorAll(sel)];
  const click = sel => {
    const el = typeof sel === 'string' ? q(sel) : sel;
    if (!el) throw new Error('missing ' + sel);
    el.click();
    return el;
  };
  const text = sel => {
    const el = typeof sel === 'string' ? q(sel) : sel;
    return el ? el.textContent : null;
  };
  const input = (sel, value) => {
    const el = typeof sel === 'string' ? q(sel) : sel;
    if (!el) throw new Error('missing ' + sel);
    el.value = value;
    el.dispatchEvent(new w.Event('input', {bubbles: true}));
    return el;
  };
  const change = (sel, value) => {
    const el = typeof sel === 'string' ? q(sel) : sel;
    if (!el) throw new Error('missing ' + sel);
    el.value = value;
    el.dispatchEvent(new w.Event('change', {bubbles: true}));
    return el;
  };
  // A closed vocabulary is either a chip row or, when the list is long, a
  // native select. One helper drives both so a test states the choice, not the
  // control shape.
  const choose = (name, value) => {
    const el = q(`#oc-pane [data-oc-choice="${name}"]`);
    if (!el) throw new Error('missing choice ' + name);
    if (el.tagName === 'SELECT') {
      el.value = value;
      el.dispatchEvent(new w.Event('change', {bubbles: true}));
      return el;
    }
    return click(`#oc-pane [data-oc-choice="${name}"][data-oc-value="${value}"]`);
  };
  const choiceValue = name => {
    const el = q(`#oc-pane [data-oc-choice="${name}"]`);
    if (!el) return null;
    return el.tagName === 'SELECT' ? el.value : el.getAttribute('aria-pressed') === 'true' ? el.dataset.ocValue : null;
  };
  // startNew is the composer-entry path the UI actually offers: the head
  // action only appears when no draft is open (or when editing a stored rule),
  // because the composer footer's cancel is the one way to abandon a draft.
  const startNew = kind => {
    const cancel = q('#oc-pane [data-oc-cancel]');
    if (cancel) cancel.click();
    click(`#oc-pane [data-oc-new="${kind}"]`);
  };
  const openCond = dim => {
    const btn = q(`#oc-pane [data-oc-pick="${dim}"]`);
    if (!btn) throw new Error('missing picker ' + dim);
    if (btn.getAttribute('aria-expanded') !== 'true') btn.click();
  };
  const pickKnown = (dim, value) => {
    openCond(dim);
    click(`#oc-pane [data-oc-pick-value="${dim}"][data-oc-value="${value}"]`);
  };
  const pickCustom = (dim, value) => {
    openCond(dim);
    const field = q(`#oc-pane [data-oc-pick-custom="${dim}"]`);
    if (!field) throw new Error('missing free-value field ' + dim);
    field.value = value;
    click(`#oc-pane [data-oc-pick-add="${dim}"]`);
  };
  const setTime = (which, value) => change(`#oc-pane [data-oc-time="${which}"]`, value);
  // Field-order-independent document comparison: the POST contract names
  // fields, never a serialization order.
  const stable = value => JSON.stringify(value, (_key, v) =>
    v && typeof v === 'object' && !Array.isArray(v)
      ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => a.localeCompare(b)))
      : v);
  const evalJSON = expr => JSON.parse(w.eval('JSON.stringify(' + expr + ')'));
  const EMPTY_PAUSE = {ok: true, paused: false, holds: [], queued: 0,
    known_clients: [], known_providers: [], until: null, default_max_queued: 0};
  const EMPTY_THROTTLE = {ok: true, active: false, throttles: [], known_providers: []};

  // The throttle document carries the client and model vocabularies too, so a
  // limit selector reads the same names the pause and capture surfaces offer.
  const throttleDoc = (doc, vocab = {}) => {
    w.applyThrottleState({...doc,
      known_clients: vocab.clients || [],
      known_models: vocab.models || []});
  };

  // applyDebugState validates the canonical model metadata against the live
  // revision; reuse the page's revision so a fixture never triggers a full
  // bootstrap as a side effect.
  const canon = () => ({rules: [], revision: w.eval('modelRevision') || modelFixture().revision, names: {}});
  const debugDoc = p => observerFixture({...p, model_canon: canon()});
  const emptyDebug = () => debugDoc({ok: true, enabled: false, sessions: [],
    known_clients: [], known_providers: [], known_models: [],
    ttl: '24h', max_bytes: '1MiB'});

  const shell = d.getElementById('operator-console');
  const paneSection = () => d.getElementById('oc-pane').dataset.section;
  const selectedTab = () => q('.oc-rail-item[aria-selected="true"]');

  w.closeOperatorConsole();
  w.applyPauseState(EMPTY_PAUSE);
  w.applyDebugState(emptyDebug());
  w.applyThrottleState(EMPTY_THROTTLE);
  w.refreshOperatorReadout();
  const savedRestartState = w.eval('JSON.stringify(restartServerState)');
  const savedRecords = w.eval('lastData && lastData.records');
  const savedStorage = w.eval('JSON.stringify(storageState)');

  try {
    // ---- 1. the header readout is the one always-visible state line ----
    {
      const out = () => d.getElementById('op-readout-state').textContent;
      const trigger = d.getElementById('btn-controls');
      const dot = d.getElementById('op-readout-dot');
      check('the operator readout starts steady with no rules in force',
        out() === 'steady' && trigger.dataset.state === 'steady' &&
        d.getElementById('op-readout').dataset.state === 'steady' &&
        dot.title === 'steady');

      w.applyPauseState({ok: true, paused: true, holds: [{id: 'readout-hold', all: true}],
        queued: 0, known_clients: [], known_providers: [], until: null, default_max_queued: 0});
      w.applyDebugState(debugDoc({ok: true, enabled: true,
        sessions: [{id: 'readout-session', clients: ['client-a']}],
        known_clients: [], known_providers: [], known_models: [],
        ttl: '24h', max_bytes: '1MiB'}));
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'],
        throttles: [{id: 'readout-limit', scope: {providers: ['prov-a']}, concurrency: 2}]});
      // A page that never connected is still loading; only a dropped stream
      // makes the line stale, so the connect is what precedes this check.
      w.eval('streamSeen = false; streamLive = false');
      w.refreshOperatorReadout();
      check('a page still connecting does not call the readout stale',
        !out().includes('offline') && trigger.dataset.state !== 'stale');
      w.eval('streamSeen = true; streamLive = false');
      w.refreshOperatorReadout();
      check('a dropped stream names the last known state as offline',
        out() === 'paused 1 · captures 1 · limits 1 · offline' &&
        trigger.dataset.state === 'stale');
      // The connectivity owner repaints both surfaces, so a reconnect clears
      // the stale words from the header without waiting for a policy poll.
      w.eval('setFooterLive(true)');
      w.refreshOperatorReadout();
      check('a reconnect clears the offline words', out() === 'paused 1 · captures 1 · limits 1');
      const activeText = 'paused 1 · captures 1 · limits 1';
      // A hold outranks a routine cap in the readout's tone: it is the one
      // state that stops live traffic.
      check('one hold, one capture session and one limit read as three segments',
        out() === activeText && trigger.dataset.state === 'paused' &&
        d.getElementById('op-readout').dataset.state === 'paused');
      check('the readout trigger names the same words to assistive tech',
        trigger.getAttribute('aria-label') ===
          'operator state: ' + activeText + '. Open the operator console.');
      check('the readout tooltip mirrors the same words instead of the control name',
        trigger.title === trigger.getAttribute('aria-label') && !/^Operator console$/.test(trigger.title));
      check('the readout dot title carries the same words', dot.title === activeText);

      // A restart outranks every policy because it explains why traffic is
      // moving. With no policy in force the step phrase is the whole line.
      w.applyPauseState(EMPTY_PAUSE);
      w.applyDebugState(emptyDebug());
      w.applyThrottleState(EMPTY_THROTTLE);
      w.eval("restartServerState = {phase: 'building', rank: 1}");
      w.refreshOperatorReadout();
      check('a running restart reads as its step and marks the readout busy',
        out() === 'restarting · step 2' &&
        trigger.dataset.state === 'busy' && d.getElementById('op-readout').dataset.state === 'busy');
      check('the restart words reach the trigger name and the dot title',
        trigger.getAttribute('aria-label') ===
          'operator state: restarting · step 2. Open the operator console.' &&
        dot.title === 'restarting · step 2');
      // rank -1 is the server's idle marker, not a step: it must not read as
      // "step 0", which looks like a bug exactly when the line must be trusted.
      w.eval("restartServerState = {phase: 'building', rank: -1}");
      w.refreshOperatorReadout();
      check('an idle restart rank names no step', out() === 'restarting');
      // The bootstrap restart section carries only the eligibility verdict, so
      // rank is absent until a status fetch lands. `null >= 0` is true in
      // JavaScript, so a plain >= test claimed "step 1" before the server had
      // reported any step.
      w.eval("restartServerState = {phase: 'building'}");
      w.refreshOperatorReadout();
      check('a restart with no reported rank yet names no step',
        out() === 'restarting' && !out().includes('step'));
      w.eval('restartServerState = {}');
      w.refreshOperatorReadout();

      // The storm/quota gate parks before admission, so its waiters are in
      // neither the admission queue nor the send gate. A proxy refusing every
      // new request for an affected model read `steady` and offered a drain
      // that could not release them.
      w.applyPauseState({...EMPTY_PAUSE, queued: 2, storm_waiters: 3});
      w.refreshOperatorReadout();
      check('requests parked by the storm gate are named apart from the drainable queue',
        out() === 'gated 3' &&
        d.getElementById('op-readout').dataset.state === 'paused' &&
        evalJSON('parkedCounts().drainable') === 2 &&
        evalJSON('parkedCounts().gated') === 3);
      w.openOperatorConsole('overview');
      const gateNote = text('#oc-pane .oc-group[data-oc-gate] .oc-note');
      check('the overview names a gate the drain cannot release',
        gateNote.includes('3 waiting on error storm or quota protection') &&
        gateNote.includes('a drain cannot release them') &&
        qa('#oc-pane [data-oc-drain]').length === 1);
      // A gate-only park offers no drain at all: the button would answer
      // "nothing released" for work it cannot reach.
      w.applyPauseState({...EMPTY_PAUSE, queued: 0, storm_waiters: 4});
      w.refreshOperatorReadout();
      check('a gate-only park offers no drain control',
        out() === 'gated 4' && !q('#oc-pane [data-oc-drain]') &&
        text('#oc-pane .oc-group[data-oc-gate] .oc-note').includes('4 waiting'));
      // The incident banner honours storm_banner_enabled, so it cannot be the
      // only place a quota pause can be resumed: with the banner off, a
      // provider parked by a durable quota gate had no reachable resume
      // anywhere in the dashboard. The snapshot keeps every incident
      // regardless of that display switch, so the console offers the action.
      {
        const quotaStorm = {enabled: true, banner_enabled: false, storms: [{
          provider: 'prov-a', model: '', scope: 'provider', state: 'open',
          reason: 'insufficient_quota', quota: true, error_percent: 0,
          error_requests: 0, failures: 0, samples: 0, window_ms: 60000,
          queued: 3, active_models: 0, affected_models: 0,
          affected_model_percent: 0, retry_at: '2026-11-14T12:00:00Z',
          recovery_successes: 0, recovery_required: 1,
        }]};
        w.applyStormState(quotaStorm);
        check('a quota pause stays reachable when the incident banner is off',
          !d.getElementById('storm-banner').textContent.includes('prov-a') &&
          evalJSON('quotaPausedProviders().join()') === 'prov-a');
        w.operatorConsoleSync('pause');
        const resume = q('#oc-pane [data-oc-quota-resume="prov-a"]');
        check('the overview offers the quota resume the banner cannot',
          !!resume &&
          text('#oc-pane .oc-group[data-oc-gate] .oc-note').includes('resumed now'));
        const savedFetch = w.fetch;
        let resumed = null;
        w.fetch = (url, options) => {
          if (String(url).includes('/admin/quota') && options && options.method === 'POST') {
            resumed = JSON.parse(options.body);
            return Promise.resolve({ok: true, json: async () => ({...quotaStorm, storms: []})});
          }
          return savedFetch(url, options);
        };
        try {
          click('#oc-pane [data-oc-quota-resume="prov-a"]');
          check('the console resume posts the same quota document the banner does',
            await settleUntil(() => resumed !== null) &&
            stable(resumed) === stable({provider: 'prov-a', resume: true}));
        } finally {
          w.fetch = savedFetch;
          w.applyStormState({enabled: false, banner_enabled: false, storms: []});
        }
      }
      w.applyPauseState(EMPTY_PAUSE);
      w.refreshOperatorReadout();

      // A stored policy outside its schedule is installed but enforces
      // nothing. Counting it would let the readout claim a cap that is not
      // applied, so the server's own verdict decides which word is used.
      w.applyThrottleState({ok: true, active: true,
        known_providers: ['prov-a'],
        throttles: [
          {id: 'live-limit', scope: {providers: ['prov-a']}, concurrency: 2, scheduled: true},
          {id: 'parked-limit', scope: {providers: ['prov-b']}, concurrency: 2, scheduled: false},
        ]});
      // The server's `scheduled` field is true only while the window is open,
      // so a policy it reports as false is off its schedule: naming that
      // segment "scheduled" told the operator the cap was working.
      check('a limit outside its schedule is named as off schedule, not counted as a cap',
        out() === 'limits 1 · off schedule 1' &&
        evalJSON('ocRuleCounts().limits') === 1 &&
        evalJSON('ocRuleCounts().parked') === 1);
      // The row must say the same thing. Its live column used to read "idle"
      // for a policy the server reports as enforcing nothing, which an
      // operator reads as "no traffic right now", not "this cap is off".
      {
        w.openOperatorConsole('limits');
        const row = q('#oc-pane .oc-rule[data-oc-rule="parked-limit"]');
        const onRow = q('#oc-pane .oc-rule[data-oc-rule="live-limit"]');
        check('the row of an off-schedule limit says so instead of reading idle',
          row?.getAttribute('data-oc-off-schedule') === '1' &&
          text(row.querySelector('.oc-rule-live')) === 'off schedule' &&
          !onRow?.hasAttribute('data-oc-off-schedule') &&
          text(onRow.querySelector('.oc-rule-live')) === 'idle');
        w.closeOperatorConsole();
      }
      w.applyThrottleState(EMPTY_THROTTLE);
      w.refreshOperatorReadout();
    }

    // ---- a failed poll must not present stale policy as current ----
    {
      // The bootstrap poll is the only source for operator policy state, and
      // its failure branch used to swallow everything. Holds, limits and
      // captures kept their last values with no marker, so the always-visible
      // readout presented minutes-old policy as a live reading. The stream
      // can be healthy while the poll is down, so this is not `offline`.
      w.applyThrottleState({ok: true, active: true, known_providers: [],
        throttles: [{id: 'stale-limit', scope: {providers: ['prov-a']},
          concurrency: 2, scheduled: true}]});
      w.refreshOperatorReadout();
      const fresh = {
        text: d.getElementById('op-readout-state').textContent,
        state: d.getElementById('op-readout').dataset.state,
        stale: w.eval('operatorPollStale()'),
      };
      check('a fresh poll reads as current, not stale',
        fresh.text === 'limits 1' && fresh.state === 'active' && fresh.stale === false);

      // A poll that has not succeeded inside its window is stale. The clock
      // is moved, not the test's patience.
      w.eval('_bootLastOk = Date.now() - 60000');
      w.refreshOperatorReadout();
      const stale = {
        text: d.getElementById('op-readout-state').textContent,
        state: d.getElementById('op-readout').dataset.state,
        stale: w.eval('operatorPollStale()'),
      };
      check('a poll that stopped landing says the counts are stale',
        stale.stale === true && stale.state === 'stale' &&
        stale.text === 'limits 1 · stale' &&
        d.getElementById('btn-controls').dataset.state === 'stale');
      // The words stay the last known truth; only the currency disclaimer is
      // added, so the operator keeps the counts and learns not to trust them.
      check('the stale line keeps the last known counts',
        d.getElementById('op-readout-state').textContent.includes('limits 1'));

      w.eval('_bootLastOk = Date.now()');
      w.applyThrottleState(EMPTY_THROTTLE);
      w.refreshOperatorReadout();
      check('a recovered poll clears the stale word',
        d.getElementById('op-readout-state').textContent === 'steady' &&
        d.getElementById('op-readout').dataset.state === 'steady');
    }

    // ---- empty states state only what the server can prove ----
    {
      // Each empty state used to promise admission - "every request is
      // admitted", "requests flow unrestricted", "as fast as it allows" -
      // while the group queue (ErrQueueFull) and the error-storm gate
      // (ErrStormQueueFull) still refuse, and capture_body_preview stores
      // prompt previews with no session running.
      const emptySays = (section, needle) => {
        w.openOperatorConsole(section);
        return text('#oc-pane .oc-empty') || '';
      };
      w.applyThrottleState({ok: true, active: false, known_providers: [], throttles: []});
      w.applyPauseState(EMPTY_PAUSE);
      w.applyDebugState({ok: true, enabled: false, sessions: [], model_canon: {}});
      w.refreshOperatorReadout();

      check('the limits empty state does not promise that every request is admitted',
        emptySays('limits').includes('No limits.') &&
        emptySays('limits').includes('still apply') &&
        !emptySays('limits').includes('Every request is admitted'));
      check("the pause empty state does not promise a provider's own rate",
        emptySays('pause').includes('No holds.') &&
        emptySays('pause').includes('can still delay') &&
        !emptySays('pause').includes('as fast as it allows'));
      check('the capture empty state does not claim records hold only headers',
        emptySays('capture').includes('No captures running.') &&
        emptySays('capture').includes('Earlier captures stay readable') &&
        !emptySays('capture').includes('only headers and metrics'));
      check('the overview empty state does not promise unrestricted flow',
        emptySays('overview').includes('still apply') &&
        !emptySays('overview').includes('flow unrestricted'));
      // The overview opens with the same four facts the page band carries, in
      // the same readout recipe, so an operator reading the policy register
      // first sees the traffic it acts on. Only a real failure flags the tick.
      w.eval('kpiAgg = { requests: 2000, errors: 0, in_flight: 0, cost: 1.25, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null }');
      w.openOperatorConsole('overview');
      const heads = qa('#oc-pane .oc-kpis .kpi h2').map(h => h.textContent);
      check('the overview opens with the four traffic facts in the band recipe (' + heads.join('/') + ' ' + (d.querySelector('#oc-pane .oc-kpis .kpi .val')||{textContent:''}).textContent + ')',
        JSON.stringify(heads) === JSON.stringify(['Requests', 'In-flight', 'Error rate', 'Total cost']) &&
        qa('#oc-pane .oc-kpis .kpi .val')[0].textContent === '2K' &&
        !d.querySelector('#oc-pane .oc-kpis .kpi.k-err'));
      w.eval('kpiAgg = { requests: 2000, errors: 4, in_flight: 0, cost: 1.25, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null }');
      w.openOperatorConsole('overview');
      check('the overview error tile flags only a real failure (' + (qa('#oc-pane .oc-kpis .kpi.k-err .val')[0]||{textContent:'none'}).textContent + ')',
        !!d.querySelector('#oc-pane .oc-kpis .kpi.k-err') &&
        qa('#oc-pane .oc-kpis .kpi.k-err .val')[0].textContent === '0.2%');
      w.eval('kpiAgg = null');
      w.closeOperatorConsole();
    }

    // ---- 2. one panel lifecycle, one focus owner ----
    {
      w.openOperatorConsole('overview');
      check('opening the console shows the shared menu panel and its trigger state',
        !shell.hidden && shell.classList.contains('mn') && shell.classList.contains('mn-on') &&
        d.getElementById('btn-controls').getAttribute('aria-expanded') === 'true' &&
        w.eval('ocMounted') === true);
      check('the open console moves focus into the panel', shell.contains(d.activeElement));
      check('the rail names the six sections in order',
        JSON.stringify(qa('.oc-rail-item').map(tab => tab.dataset.ocSection)) ===
          JSON.stringify(['overview', 'limits', 'pause', 'capture', 'export', 'system']) &&
        JSON.stringify(qa('.oc-rail-label').map(label => label.textContent)) ===
          JSON.stringify(['overview', 'limits', 'pause', 'capture', 'export', 'system']));
      check('the overview is the selected tab and the pane names it',
        selectedTab().dataset.ocSection === 'overview' && paneSection() === 'overview' &&
        d.getElementById('oc-pane').getAttribute('aria-labelledby') === 'oc-tab-overview');

      w.toggleOperatorConsole();
      check('toggling the console closes it and clears the expanded state',
        shell.hidden && !shell.classList.contains('mn-on') &&
        d.getElementById('btn-controls').getAttribute('aria-expanded') === 'false' &&
        w.eval('ocMounted') === false);

      w.openOperatorConsole('overview');
      d.activeElement.dispatchEvent(new w.KeyboardEvent('keydown',
        {key: 'Escape', bubbles: true, cancelable: true}));
      check('Escape closes the console and returns focus to its trigger',
        shell.hidden && w.eval('ocMounted') === false &&
        d.activeElement === d.getElementById('btn-controls') &&
        d.getElementById('btn-controls').getAttribute('aria-expanded') === 'false');

      // The session remembers the last section. Reopening with no argument
      // must land where the operator left, not reset to the overview.
      w.openOperatorConsole('capture');
      w.closeOperatorConsole();
      w.toggleOperatorConsole();
      check('the console reopens on the section the session last used',
        paneSection() === 'capture' && !shell.hidden &&
        d.getElementById('btn-controls').getAttribute('aria-expanded') === 'true');
      w.closeOperatorConsole();

      // Settings is a modal sheet, so the console closes it before opening:
      // a surface left under an inert modal would be visible but unusable.
      w.toggleSettings();
      await settleUntil(() => !d.getElementById('settings-sheet').hidden, 4000);
      // A console warning that names one setting needs a route to the pane
      // that holds it. The fixture's config document carries one category, so
      // this drives the contract on a two-category document: the request lands
      // on the named pane instead of the sheet's first one.
      {
        const savedDoc = w.eval('JSON.stringify(settingsDoc)');
        const savedCat = w.eval('settingsCat');
        w.eval(`settingsDoc = {
          revision: 'cat-jump', writable: true,
          fields: [
            {key: 'cat_a_one', category: 'server', label: 'A', help: '', kind: 'string'},
            {key: 'cat_a_two', category: 'server', label: 'B', help: '', kind: 'string'},
            {key: 'cat_b_one', category: 'storage', label: 'C', help: '', kind: 'string'},
            {key: 'cat_b_two', category: 'storage', label: 'D', help: '', kind: 'string'}
          ],
          categories: [{id: 'server', label: 'Server', help: 's'},
            {id: 'storage', label: 'Storage', help: 't'}],
          values: {}, defaults: {}, overrides: {}, effective: {}, usage_fields: []
        };
        settingsCat = 'server';
        fillSettingsForm(settingsDoc);`);
        const storageTab = q('#settings-rail [data-st-cat="storage"]');
        const serverTab = q('#settings-rail [data-st-cat="server"]');
        check('a two-category sheet starts on its first category',
          !!storageTab && !!serverTab &&
          serverTab.classList.contains('active') && !storageTab.classList.contains('active'));
        w.eval("openSettingsAt('storage')");
        check('a category request lands Settings on that category',
          storageTab.classList.contains('active') &&
          storageTab.getAttribute('aria-current') === 'page' &&
          w.eval('settingsCat') === 'storage' &&
          !serverTab.classList.contains('active'));
        w.eval('settingsDoc = ' + savedDoc + '; settingsCat = ' + JSON.stringify(savedCat)
          + '; fillSettingsForm(settingsDoc)');
      }
      w.openOperatorConsole('overview');
      check('opening the console closes an open settings sheet first',
        d.getElementById('settings-sheet').hidden && !shell.hidden &&
        shell.contains(d.activeElement));
      w.closeOperatorConsole();
      // A sheet opened FROM the console goes back to that console section when
      // it closes, instead of dropping the operator into the header.
      w.openOperatorConsole('system');
      const consoleSettings = d.querySelector('#oc-pane [data-oc-settings]');
      consoleSettings.click();
      check('the console Settings button opens the sheet and closes the console',
        !d.getElementById('settings-sheet').hidden && shell.hidden);
      w.closeSettings();
      check('closing that sheet reopens the console on the section it left',
        d.getElementById('settings-sheet').hidden && !shell.hidden &&
        w.eval('ocSection') === 'system');
      w.closeOperatorConsole();
    }

    // ---- 3. the rail is a real tablist ----
    {
      w.openOperatorConsole('overview');
      const press = key => {
        const tab = q('.oc-rail-item[aria-selected="true"]');
        tab.focus();
        tab.dispatchEvent(new w.KeyboardEvent('keydown', {key, bubbles: true, cancelable: true}));
      };
      const stateNow = () => ({
        section: paneSection(), focus: d.activeElement && d.activeElement.dataset.ocSection,
        labelledBy: d.getElementById('oc-pane').getAttribute('aria-labelledby'),
      });
      press('ArrowDown');
      let now = stateNow();
      check('ArrowDown selects the next section and moves focus to its tab',
        now.section === 'limits' && now.focus === 'limits' &&
        now.labelledBy === 'oc-tab-limits');
      press('ArrowUp');
      now = stateNow();
      check('ArrowUp selects the previous section', now.section === 'overview' && now.focus === 'overview');
      press('ArrowUp');
      now = stateNow();
      check('ArrowUp on the first tab wraps to the last section', now.section === 'system' && now.focus === 'system');
      press('Home');
      now = stateNow();
      check('Home jumps to the first section and labels the pane with its tab',
        now.section === 'overview' && now.focus === 'overview' &&
        now.labelledBy === 'oc-tab-overview');
      press('End');
      now = stateNow();
      check('End jumps to the last section and labels the pane with its tab',
        now.section === 'system' && now.focus === 'system' &&
        now.labelledBy === 'oc-tab-system');
      press('ArrowDown');
      now = stateNow();
      check('ArrowDown on the last tab wraps to the first section',
        now.section === 'overview' && now.focus === 'overview');
      // At 560px and below the same tablist paints as a three-column grid
      // (operator_console.js OC_RAIL_COLUMNS via CSS). The arrows then follow
      // the rows and columns the operator can see instead of the flat order:
      // no vertical key could reach the second row before.
      {
        const savedCols = w.ocRailColumns;
        w.ocRailColumns = () => 3;
        try {
          press('Home');
          press('ArrowDown');
          now = stateNow();
          check('in the grid, ArrowDown moves one row down the same column',
            now.section === 'capture' && now.focus === 'capture');
          press('ArrowDown');
          now = stateNow();
          check('in the grid, ArrowDown past the last row stays put',
            now.section === 'capture' && now.focus === 'capture');
          press('ArrowUp');
          press('ArrowRight');
          now = stateNow();
          check('in the grid, ArrowRight moves along the row', now.section === 'limits');
          press('End');
          press('ArrowLeft');
          now = stateNow();
          check('in the grid, ArrowLeft stops at the row start', now.section === 'export');
          // Leave the section where the flat flow left it, and drop the export
          // draft so a count fetched on real handlers here cannot pre-empt the
          // export suite's own stubbed count.
          press('Home');
          w.eval('ocDrafts.export = null');
        } finally {
          w.ocRailColumns = savedCols;
        }
      }
      // A roving handler that claims arrows must leave the browser's own
      // bindings alone. Alt+Left/Right are Back and Forward, and the rail
      // silently used them to switch section because it preventDefaulted a key
      // it did not own.
      {
        const pressMod = (key, mod) => {
          const tab = q('.oc-rail-item[aria-selected="true"]');
          tab.focus();
          const evt = new w.KeyboardEvent('keydown', {key, bubbles: true, cancelable: true, ...mod});
          tab.dispatchEvent(evt);
          return evt.defaultPrevented;
        };
        const beforeMod = paneSection();
        const altPrevented = pressMod('ArrowRight', {altKey: true});
        check('a modified arrow stays with the browser instead of switching section',
          altPrevented === false && paneSection() === beforeMod);
        check('the browser-navigation guard is one predicate',
          w.eval('keyModified({})') === false && w.eval('keyModified({altKey: true})') === true &&
          w.eval('keyModified({ctrlKey: true})') === true && w.eval('keyModified({metaKey: true})') === true &&
          w.eval('keyModified({shiftKey: true})') === false);
        // The unmodified key still works, so the guard cannot be satisfied by
        // disabling the handler.
        check('an unmodified arrow still selects the next section',
          pressMod('ArrowRight', {}) === true && paneSection() !== beforeMod);
        // A control's accessible name is the words on screen, never the wire
        // key the dispatcher compares against. The name and the label are
        // separate fields because they are separate jobs, and a missing or
        // duplicated label is now visible here rather than audible only to a
        // speech-input user.
        check('every choice announces a human label, never its wire key',
          evalJSON('Object.values(OC_CHOICES).map(c => c.label)').every(l => typeof l === 'string' && l.length > 0) &&
          evalJSON('Object.values(OC_CHOICES).every(c => c.label !== c.name)') === true &&
          // The labels are the visible headings: a dash would mean a key
          // leaked back in, and a duplicate would make two groups in one pane
          // indistinguishable when announced.
          evalJSON('Object.values(OC_CHOICES).every(c => !/-/.test(c.label))') === true &&
          new Set(evalJSON('Object.values(OC_CHOICES).map(c => c.label + c.kind)')).size ===
            evalJSON('Object.values(OC_CHOICES).length'));
        // One predicate, not one per handler: the rule is easy to state and
        // easy to forget, and five handlers had already written it out by hand
        // while two others omitted it entirely. Penalizing only a re-spelling
        // is what let the drawer's tab strip keep consuming Alt+Left for months:
        // it never wrote the modifier chain, it just never asked. So the rule is
        // asserted from both directions - one owner, and every module that
        // claims a navigation key asks it before calling preventDefault.
        {
          const {readFileSync, readdirSync} = require('fs');
          const {join: joinPath} = require('path');
          const dir = joinPath(__dirname, '..', '..', '..', 'internal', 'web', 'static', 'js');
          const raw = [/altKey\s*\|\|\s*\w+\.ctrlKey/, /ctrlKey\s*\|\|\s*\w+\.metaKey/];
          const sources = readdirSync(dir).filter(n => /\.js$/.test(n)).map(n =>
            [n, readFileSync(joinPath(dir, n), 'utf8')]);
          const respelled = sources.filter(([n, text]) => n !== 'core.js' && raw.some(rx =>
            rx.test(text.replace(/\/\/.*$/gm, '')))).map(([n]) => n);
          check('the browser-navigation rule is not re-spelled outside its owner',
            respelled.length === 0);
          // A module that claims a navigation key and cancels it owns the
          // browser's binding until it asks whether the key was modified.
          const claimants = sources.filter(([n, text]) => {
            const code = text.replace(/\/\/.*$/gm, '');
            return /'(ArrowDown|ArrowUp|ArrowLeft|ArrowRight|Home|End)'/.test(code) &&
              /preventDefault\(/.test(code);
          }).map(([n]) => n);
          const unguarded = claimants.filter(n => !sources.find(([x]) => x === n)[1].includes('keyModified('));
          check('every handler that claims a navigation key asks the one predicate first',
            claimants.length >= 4 && unguarded.length === 0);
        }
      }
      // The console covers the page it sits over, so Tab is contained: a
      // roving Tab that walked into covered content would move focus somewhere
      // the operator cannot see.
      {
        const items = w.modalFocusables(shell);
        const pressTab = (target, shiftKey) => target.dispatchEvent(
          new w.KeyboardEvent('keydown', {key: 'Tab', shiftKey, bubbles: true, cancelable: true}));
        items.at(-1).focus();
        pressTab(d.activeElement, false);
        check('Tab from the last console control wraps to its first', d.activeElement === items[0]);
        pressTab(d.activeElement, true);
        check('shift+Tab from the first console control wraps to its last',
          d.activeElement === items.at(-1));
        // A press inside a confirmation dialog above the console stays with
        // that dialog's own focus order.
        const confirm = d.createElement('section');
        confirm.setAttribute('role', 'dialog');
        confirm.setAttribute('aria-modal', 'true');
        const inside = d.createElement('button');
        confirm.appendChild(inside);
        d.body.appendChild(confirm);
        inside.focus();
        pressTab(inside, false);
        check('a Tab inside a dialog above the console is not pulled back',
          d.activeElement === inside);
        confirm.remove();
      }
      w.closeOperatorConsole();
    }

    // ---- 4. a full limits selector compiles to the server document ----
    {
      // A menu needs more than one row to show a cursor moving, so the
      // vocabulary carries neighbours the block never picks.
      throttleDoc({ok: true, active: false, throttles: [],
        known_providers: ['prov-a', 'prov-b', 'prov-c']},
        {clients: ['client-a'], models: ['model-a']});
      w.__ocRecordFixture = [{
        ...mkRec('oc-limit-key'),
        provider: 'prov-a', model: 'model-a', client: 'client-a',
        key_hash: 'abcdef123456', conversation_id: 'conv-1',
      }];
      w.eval('lastData.records = window.__ocRecordFixture');
      w.openOperatorConsole('limits');
      startNew('limit');
      pickKnown('providers', 'prov-a');
      pickKnown('models', 'model-a');
      pickKnown('clients', 'client-a');
      // The requested "free-value field for a key hash" cannot be expressed:
      // the console deliberately offers no free-value field for keys
      // (operator_console.js ocScopePickerHTML sets canAdd = dim !== 'keyHashes'),
      // because a key scope lists only hashes already present in the live
      // record ring. The fixture record seeds that ring, so the key is picked
      // from its row instead. Conversations do accept a typed value.
      pickCustom('conversations', 'conv-1');
      pickKnown('keyHashes', 'abcdef123456');
      // One action row while a draft is open. The head used to grow a second
      // "start over" button beside the composer's own cancel: a duplicate
      // discard of the same draft, with no dialog, that also took the accent
      // the commit button needs.
      check('a new draft leaves the composer footer as its only action row',
        !q('#oc-pane .oc-sec-head [data-oc-new]') &&
        !!q('#oc-pane .oc-compose [data-oc-cancel]') &&
        !!q('#oc-pane [data-oc-apply="limit"]'));
      check('the five scope conditions carry the picked values',
        JSON.stringify(evalJSON('ocDrafts.limit.scope')) === JSON.stringify({
          providers: ['prov-a'], models: ['model-a'], clients: ['client-a'],
          keyHashes: ['abcdef123456'], conversations: ['conv-1'],
        }));
      // A value chip holds a label and a remove button. The label has to be
      // its own shrinking box: as a bare nowrap text node it could not give
      // way, so a value of nineteen characters or more pushed the remove
      // button outside the chip, where it had no clickable area and painted
      // over the next dimension chip.
      {
        const chips = qa('#oc-pane .oc-cond-values .oc-chip');
        check('a value chip keeps its remove button inside the chip',
          chips.length > 0 &&
          chips.every(chip => !!chip.querySelector('.oc-chip-text') &&
            !!chip.querySelector('.oc-chip-x') &&
            chip.getAttribute('title') === chip.querySelector('.oc-chip-text').textContent));
      }
      for (const key of ['concurrency', 'requests', 'tokens']) {
        click(`#oc-pane [data-oc-budget="${key}"]`);
      }
      input('#oc-pane [data-oc-field="concurrency"]', '3');
      input('#oc-pane [data-oc-field="requests"]', '10');
      input('#oc-pane [data-oc-field="tokens"]', '1000');
      click('#oc-pane [data-oc-toggle="schedule"]');
      click('#oc-pane [data-oc-day="0"]');
      click('#oc-pane [data-oc-day="1"]');
      setTime('start', '22:00');
      setTime('end', '06:00');
      check('the schedule chips and times become start and end minutes',
        JSON.stringify(evalJSON('ocDrafts.limit.schedule')) ===
          JSON.stringify({days: [0, 1], start: 1320, end: 360}));

      const expected = {
        providers: ['prov-a'], models: ['model-a'], clients: ['client-a'],
        key_hashes: ['abcdef123456'], conversations: ['conv-1'],
        days: [0, 1], start_minute: 1320, end_minute: 360,
        concurrency: 3, requests: 10, request_window: '1m',
        tokens: 1000, token_window: '1m',
        anti_burst: false, ramp_duration: '',
      };
      const savedFetch = w.fetch;
      let captured = null;
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/throttle') && options && options.method === 'POST') {
          captured = {url: String(url), body: JSON.parse(options.body)};
          const body = captured.body;
          return {ok: true, json: async () => ({ok: true, active: true,
            known_providers: ['prov-a'], known_clients: ['client-a'], known_models: ['model-a'],
            throttles: [{
              id: 'limit-created',
              scope: {providers: body.providers, models: body.models, clients: body.clients,
                key_hashes: body.key_hashes, conversations: body.conversations},
              schedule: {days: body.days, start_minute: body.start_minute, end_minute: body.end_minute},
              concurrency: body.concurrency, requests: body.requests,
              request_window: body.request_window, tokens: body.tokens,
              token_window: body.token_window, anti_burst: body.anti_burst,
              ramp_duration: body.ramp_duration, source: 'ui', in_flight: 0, queued: 0,
            }]})};
        }
        return savedFetch(url, options);
      };
      try {
        click('#oc-lim-apply');
        check('the limit composer posts to the throttle endpoint',
          await settleUntil(() => captured !== null) && captured.url === '/admin/throttle');
        check('a full limit selector posts the exact server document',
          captured && stable(captured.body) === stable(expected) && !('id' in captured.body));
        await settleUntil(() => text('#oc-change').startsWith('added limit for '));
        const scopePhrase = w.ocScopePhrase({
          providers: ['prov-a'], models: ['model-a'], clients: ['client-a'],
          keyHashes: ['abcdef123456'], conversations: ['conv-1'],
        });
        // The row and the ledger must render the SAME shared phrase, and a key
        // is named once: ocScopePhrase prefixes the dimension, so the value
        // label must not prefix it again.
        check('the applied limit row reads the shared scope phrase',
          text('#oc-pane .oc-rule[data-kind="limit"] .oc-rule-scope') === scopePhrase &&
          ['provider prov-a', 'model model-a', 'client client-a', 'conversation conv-1']
            .every(part => scopePhrase.includes(part)));
        check('a key is named once, by its hash prefix',
          scopePhrase.includes('key abcdef12…') && !scopePhrase.includes('key key') &&
          text('#oc-pane .oc-rule[data-kind="limit"] .oc-rule-scope').includes('key abcdef12…'));
        check('the applied limit row names its budgets and schedule',
          text('#oc-pane .oc-rule[data-kind="limit"] .oc-rule-budget') ===
            '3 in flight · 10 requests / 1m · 1K tokens / 1m · SM · 22:00–06:00');
        check('the change ledger records the add with the same scope wording',
          text('#oc-change').startsWith('added limit for ' + scopePhrase) &&
          text('#oc-change').includes('3 in flight') && text('#oc-change').includes('1K tokens / 1m'));
      } finally {
        w.fetch = savedFetch;
      }
      w.closeOperatorConsole();
    }

    // ---- 5. limits validation and edit mode ----
    {
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a']});
      w.openOperatorConsole('limits');
      startNew('limit');
      check('a limit with no budget cannot be applied and says so',
        d.getElementById('oc-lim-apply').disabled &&
        text('#oc-lim-preview') === 'set at least one budget');
      click('#oc-pane [data-oc-budget="concurrency"]');
      // Arming a budget opens its field; it must not install a number the
      // operator never chose. One stray click on "concurrency" followed by
      // Apply would otherwise put a 4-in-flight cap on whatever is in scope.
      check('arming a budget opens an empty field and still refuses to apply',
        d.querySelector('#oc-pane [data-oc-field="concurrency"]').value === '' &&
        !d.querySelector('#oc-pane [data-oc-field="concurrency"]').disabled &&
        d.getElementById('oc-lim-apply').disabled &&
        text('#oc-lim-preview') === 'set at least one budget');
      input('#oc-pane [data-oc-field="concurrency"]', '-1');
      check('a negative count is refused as not a whole number',
        d.getElementById('oc-lim-apply').disabled &&
        text('#oc-lim-preview').includes('whole numbers'));
      input('#oc-pane [data-oc-field="concurrency"]', '1.5');
      check('a fractional count is refused by the same sentence',
        d.getElementById('oc-lim-apply').disabled &&
        text('#oc-lim-preview').includes('whole numbers') &&
        d.getElementById('oc-lim-preview').dataset.tone === 'err');
      input('#oc-pane [data-oc-field="concurrency"]', '2');

      const policy = {
        id: 'pol-edit', scope: {providers: ['prov-a'], models: ['model-a']},
        schedule: {days: [2], start_minute: 600, end_minute: 660},
        concurrency: 4, requests: 0, request_window: '', tokens: 5, token_window: '1h',
        anti_burst: false, ramp_duration: '', source: 'ui', in_flight: 0, queued: 0,
      };
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'], throttles: [policy]});
      click('#oc-pane [data-oc-edit="limit"][data-oc-id="pol-edit"]');
      const draft = evalJSON('({id: ocDrafts.limit.id, scope: ocDrafts.limit.scope, ' +
        'concurrency: ocDrafts.limit.concurrency, requests: ocDrafts.limit.requests, ' +
        'tokens: ocDrafts.limit.tokens, tokenWindow: ocDrafts.limit.tokenWindow, ' +
        'schedule: ocDrafts.limit.schedule})');
      check('editing a policy prefills its id, scope, budgets and schedule',
        draft.id === 'pol-edit' &&
        JSON.stringify(draft.scope.providers) === JSON.stringify(['prov-a']) &&
        JSON.stringify(draft.scope.models) === JSON.stringify(['model-a']) &&
        draft.concurrency === '4' && draft.requests === '' && draft.tokens === '5' &&
        draft.tokenWindow === '1h' &&
        JSON.stringify(draft.schedule) === JSON.stringify({days: [2], start: 600, end: 660}));
      // The edited rule stays in the list, and nothing used to say the form
      // and that row were the same rule: the console showed the rule twice
      // with no relationship between them.
      {
        const row = q('#oc-pane .oc-rule[data-oc-rule="pol-edit"]');
        const others = qa('#oc-pane .oc-rule').filter(el => el.dataset.ocRule !== 'pol-edit');
        check('the rule open in the composer is marked in the list',
          row?.getAttribute('data-oc-editing') === '1' &&
          row.getAttribute('aria-current') === 'true' &&
          others.every(el => !el.hasAttribute('data-oc-editing')));
      }
      check('an edit composer offers the save action', text('#oc-lim-apply') === 'save limit' &&
        !d.getElementById('oc-lim-apply').disabled);

      // Copy is the follow-up path: the same scope and schedule, no budgets,
      // and no id, so the write creates a second rule instead of editing the
      // first. Retyping four dimensions was the most expensive task here.
      click('#oc-pane [data-oc-copy="limit"][data-oc-id="pol-edit"]');
      const copied = evalJSON('({id: ocDrafts.limit.id, scope: ocDrafts.limit.scope,'
        + 'schedule: ocDrafts.limit.schedule, concurrency: ocDrafts.limit.concurrency,'
        + 'requests: ocDrafts.limit.requests, tokens: ocDrafts.limit.tokens,'
        + 'window: ocDrafts.limit.tokenWindow})');
      check('copy reuses the scope and schedule but clears the budgets and id',
        copied.id === '' &&
        JSON.stringify(copied.scope.providers) === JSON.stringify(['prov-a']) &&
        JSON.stringify(copied.scope.models) === JSON.stringify(['model-a']) &&
        JSON.stringify(copied.schedule) === JSON.stringify({days: [2], start: 600, end: 660}) &&
        copied.concurrency === '' && copied.requests === '' && copied.tokens === '' &&
        copied.window === '1h' &&
        text('#oc-lim-apply') === 'apply limit' &&
        d.getElementById('oc-lim-apply').disabled);
      // A copy keeps the source selector, and the server identifies a limit
      // by that selector, so a copy that only changed the budgets would
      // silently replace the rule it was copied from while the console
      // reported "added". The panel now refuses the write and says why.
      check('a copied selector that still matches its source refuses to write',
        (() => {
          const key = w.ocLimitSelectorKey;
          return typeof key === 'function' &&
            key(evalJSON('ocScopeFromPolicy(throttleState.throttles[0])'),
              evalJSON('ocLimitScheduleKey(throttleState.throttles[0])')) ===
            key(evalJSON('ocDrafts.limit.scope'), evalJSON('ocDrafts.limit.schedule'));
        })());
      // The collision is knowable the moment the composer opens: a copy reuses
      // the source selector and clears the budgets. Gating the refusal on a
      // typed budget told the operator "set at least one budget" first and only
      // refused afterwards, so the one thing they could do could not help.
      check('a copied selector names the collision before any budget is typed',
        !w.eval('ocDrafts.limit.concurrency') && !w.eval('ocDrafts.limit.requests') &&
        !w.eval('ocDrafts.limit.tokens') &&
        d.getElementById('oc-lim-apply').disabled &&
        text('#oc-lim-preview').includes('already has a limit') &&
        q('#oc-lim-preview').dataset.tone === 'err');
      {
        for (const key of ['concurrency', 'requests', 'tokens']) {
          const box = q(`#oc-pane [data-oc-budget="${key}"]`);
          if (box && !box.checked) box.click();
        }
        input('#oc-pane [data-oc-field="concurrency"]', '2');
        input('#oc-pane [data-oc-field="requests"]', '5');
        input('#oc-pane [data-oc-field="tokens"]', '500');
        w.eval('ocLimitPreview()');
        check('the gate names the existing selector instead of applying a replacement',
          d.getElementById('oc-lim-apply').disabled &&
          text('#oc-lim-preview').includes('already has a limit') &&
          q('#oc-lim-preview').dataset.tone === 'err');
        // Changing one dimension makes it a genuinely new rule again. The
        // fixture's model vocabulary holds only model-a, so the extra value is
        // added the way the free-value field would add it.
        w.eval("ocScopeToggle(ocDrafts.limit.scope, 'models', 'model-b'); ocRender()");
        w.eval('ocLimitPreview()');
        check('changing a dimension clears the collision and re-enables apply',
          !d.getElementById('oc-lim-apply').disabled &&
          !text('#oc-lim-preview').includes('already has a limit'));
        w.eval("ocDrafts.limit = null; ocView = {kind: '', id: ''}; ocRender()");
      }
      click('#oc-pane [data-oc-edit="limit"][data-oc-id="pol-edit"]');

      const savedFetch = w.fetch;
      let editBody = null;
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/throttle') && options && options.method === 'POST') {
          editBody = JSON.parse(options.body);
          return {ok: true, json: async () => ({ok: true, active: true,
            known_providers: ['prov-a'], throttles: [policy]})};
        }
        return savedFetch(url, options);
      };
      try {
        click('#oc-lim-apply');
        check('an edit POST carries the policy id and its fields',
          await settleUntil(() => editBody !== null) && editBody.id === 'pol-edit' &&
          JSON.stringify(editBody.providers) === JSON.stringify(['prov-a']) &&
          editBody.concurrency === 4 && editBody.tokens === 5 &&
          editBody.token_window === '1h');
      } finally {
        w.fetch = savedFetch;
      }

      click('#oc-pane [data-oc-edit="limit"][data-oc-id="pol-edit"]');
      w.eval('operatorState.throttle.busy = true');
      w.operatorConsoleSync('throttle');
      check('the apply action stays disabled while the throttle gate is busy',
        d.getElementById('oc-lim-apply').disabled);
      w.eval('operatorState.throttle.busy = false');
      w.operatorConsoleSync('throttle');
      check('clearing the gate re-enables the apply action',
        !d.getElementById('oc-lim-apply').disabled);
      w.closeOperatorConsole();
    }

    // ---- 5b. a stored duration outside the preset list still shows itself ----
    {
      // The server accepts any window from 1s to 24h, so a policy created
      // through the API, MCP or a header can carry 90s while the presets only
      // offer 1s..24h. An exact-match-only select marked no option current and
      // the browser fell back to the FIRST option: the composer read "1
      // second" for a rule whose own row printed 90s, and a save would have
      // rewritten the window the operator never meant to touch.
      const odd = {
        id: 'pol-odd', scope: {providers: ['prov-a']}, schedule: null,
        concurrency: 0, requests: 10, request_window: '90s',
        tokens: 2000, token_window: '45s', anti_burst: false, ramp_duration: '',
        source: 'api', in_flight: 0, queued: 0,
      };
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'], throttles: [odd]});
      w.openOperatorConsole('limits');
      check('an off-list window still reads as itself on the rule row',
        text('#oc-pane .oc-rule[data-oc-rule="pol-odd"] .oc-rule-budget')
          .includes('10 requests / 90s'));
      click('#oc-pane [data-oc-edit="limit"][data-oc-id="pol-odd"]');
      const reqSel = q('#oc-pane [data-oc-choice="request-window"]');
      const tokSel = q('#oc-pane [data-oc-choice="token-window"]');
      check('the composer shows an off-list request window instead of the first preset',
        reqSel && reqSel.value === '90s' &&
        reqSel.selectedOptions[0]?.textContent === '90s' &&
        ![...reqSel.options].some(o => o.selected && o.value === '1s'));
      check('the composer shows an off-list token window instead of the first preset',
        tokSel && tokSel.value === '45s' &&
        tokSel.selectedOptions[0]?.textContent === '45s');
      // Selecting a preset must still replace it, and the off-list option has
      // to leave the list rather than linger as a stale extra choice.
      choose('request-window', '5m');
      const afterPick = q('#oc-pane [data-oc-choice="request-window"]');
      check('choosing a preset replaces the off-list window',
        afterPick.value === '5m' &&
        ![...afterPick.options].some(o => o.value === '90s') &&
        w.eval('ocDrafts.limit.requestWindow') === '5m');
      w.closeOperatorConsole();
    }

    // ---- 6. anti-burst is gated to one provider without a schedule ----
    {
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a', 'prov-b']},
        {clients: ['client-a']});
      w.openOperatorConsole('limits');
      startNew('limit');
      pickKnown('providers', 'prov-a');
      click('#oc-pane [data-oc-toggle="advanced"]');
      const burstBox = () => q('#oc-pane [data-oc-field="antiBurst"]');
      const burstNote = () => q('#oc-pane .oc-advanced .oc-note');
      check('a single-provider limit without a schedule enables burst control',
        !!burstBox() && !burstBox().disabled && !burstNote());
      click('#oc-pane [data-oc-budget="concurrency"]');
      input('#oc-pane [data-oc-field="concurrency"]', '2');
      burstBox().click();
      check('arming burst control writes the draft flag',
        w.eval('ocDrafts.limit.antiBurst') === true);
      choose('ramp', '10s');
      const eligible = evalJSON('ocLimitBody(ocDrafts.limit)');
      check('an armed eligible limit compiles anti_burst with its ramp',
        eligible.anti_burst === true && eligible.ramp_duration === '10s' &&
        eligible.providers.length === 1);

      // A draft armed before the scope widens must stop compiling anti_burst:
      // ocLimitBody asks the same eligibility owner the control does, so the
      // body can never carry a burst the server rejects.
      {
        const armed = evalJSON('ocLimitBody(ocDrafts.limit)');
        evalJSON('ocDrafts.limit.scope.clients = ["client-a"]');
        const widened = evalJSON('ocLimitBody(ocDrafts.limit)');
        evalJSON('ocDrafts.limit.scope.clients = []');
        check('a widened scope drops the armed burst from the compiled body',
          armed.anti_burst === true && widened.anti_burst === false &&
          widened.ramp_duration === '');
      }
      // A picker promises a menu, so its rows honour the menu keys and leave
      // the browser's own bindings alone. Ctrl+End is the document bottom and
      // Alt+Arrow is history navigation; the row handler used to consume both.
      {
        openCond('providers');
        const rows = [...d.querySelectorAll('#oc-pane .oc-pick-row')];
        rows[0].focus();
        const modified = new w.KeyboardEvent('keydown',
          {key: 'End', ctrlKey: true, bubbles: true, cancelable: true});
        rows[0].dispatchEvent(modified);
        check('a modified key in a picker stays with the browser',
          rows.length > 1 && modified.defaultPrevented === false);
        const plain = new w.KeyboardEvent('keydown',
          {key: 'End', bubbles: true, cancelable: true});
        d.activeElement.dispatchEvent(plain);
        check('an unmodified End still moves the picker cursor to the last row',
          plain.defaultPrevented === true &&
          d.activeElement.getAttribute('aria-checked') !== null &&
          rows[rows.length - 1] === d.activeElement);
        // Close the picker again so the draft flow below starts where it did.
        openCond('providers');
      }
      // One fresh draft per disqualifier: the control is offered only for a
      // single-provider rule without a schedule.
      startNew('limit');
      pickKnown('providers', 'prov-a');
      pickKnown('providers', 'prov-b');
      click('#oc-pane [data-oc-budget="concurrency"]');
      input('#oc-pane [data-oc-field="concurrency"]', '2');
      click('#oc-pane [data-oc-toggle="advanced"]');
      check('a second provider disables burst control and explains why',
        burstBox().disabled &&
        text(burstNote()) === 'Burst control needs a single-provider limit without a schedule.' &&
        evalJSON('ocLimitBody(ocDrafts.limit)').anti_burst === false);

      startNew('limit');
      pickKnown('providers', 'prov-a');
      pickKnown('clients', 'client-a');
      click('#oc-pane [data-oc-budget="concurrency"]');
      input('#oc-pane [data-oc-field="concurrency"]', '2');
      click('#oc-pane [data-oc-toggle="advanced"]');
      check('a client dimension disables burst control and compiles no burst',
        burstBox().disabled && !!burstNote() &&
        evalJSON('ocLimitBody(ocDrafts.limit)').anti_burst === false);

      startNew('limit');
      pickKnown('providers', 'prov-a');
      click('#oc-pane [data-oc-budget="concurrency"]');
      input('#oc-pane [data-oc-field="concurrency"]', '2');
      click('#oc-pane [data-oc-toggle="schedule"]');
      click('#oc-pane [data-oc-toggle="advanced"]');
      check('a schedule disables burst control and compiles no burst',
        burstBox().disabled && !!burstNote() &&
        evalJSON('ocLimitBody(ocDrafts.limit)').anti_burst === false);

      click('#oc-pane [data-oc-toggle="schedule"]');
      check('clearing the schedule restores burst control', !burstBox().disabled);
      w.closeOperatorConsole();
    }

    // ---- 7. remove, confirmation, undo, single flight ----
    {
      const policy = {
        id: 'pol-rm', scope: {providers: ['prov-a'], models: ['model-a']},
        schedule: null, concurrency: 4, requests: 10, request_window: '1m',
        tokens: 0, token_window: '', anti_burst: false, ramp_duration: '',
        source: 'ui', in_flight: 2, queued: 0,
      };
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'], throttles: [policy]});
      throttleDoc({ok: true, active: true, known_providers: ['prov-a'], throttles: [policy]});
      w.openOperatorConsole('limits');
      const posts = [];
      const savedFetch = w.fetch;
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/throttle') && options && options.method === 'POST') {
          posts.push(JSON.parse(options.body));
          return {ok: true, json: async () => ({ok: true, active: false,
            known_providers: ['prov-a'], throttles: []})};
        }
        if (String(url).includes('/admin/throttle')) {
          return {ok: true, json: async () => ({ok: true, active: true,
            known_providers: ['prov-a'], throttles: [policy]})};
        }
        return savedFetch(url, options);
      };
      try {
        click('#oc-pane [data-oc-remove="limit"][data-oc-id="pol-rm"]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        const dialog = d.getElementById('oc-confirm-dialog');
        check('removing a limit opens the shared confirmation and names the scope',
          !!dialog && text('#oc-confirm-title') === 'Remove this limit?' &&
          dialog.querySelector('.oc-confirm-body').textContent.includes(
            'provider prov-a · model model-a') &&
          dialog.querySelector('.oc-confirm-body').textContent.includes('2 requests are in flight'));
        check('the confirmation focuses Cancel before anything is sent',
          d.activeElement === dialog.querySelector('[data-oc-confirm="cancel"]') && posts.length === 0);
        click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
        check('cancelling the removal leaves the policy and sends nothing',
          !d.getElementById('oc-confirm-dialog') &&
          !!q('#oc-pane .oc-rule[data-kind="limit"][data-oc-rule="pol-rm"]') &&
          posts.length === 0);

        click('#oc-pane [data-oc-remove="limit"][data-oc-id="pol-rm"]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        click('#oc-confirm-dialog [data-oc-confirm="ok"]');
        check('confirming the removal posts the clear document',
          await settleUntil(() => posts.length === 1) &&
          stable(posts[0]) === stable({id: 'pol-rm', clear: true}));
        await settleUntil(() => text('#oc-change').startsWith('removed limit for '));
        check('the removal is recorded and offers undo',
          text('#oc-change') === 'removed limit for provider prov-a · model model-a' &&
          !d.getElementById('oc-undo').hidden);

        click('#oc-undo');
        check('undo restores the original selector and budgets without an id',
          await settleUntil(() => posts.length === 2) &&
          !('id' in posts[1]) &&
          JSON.stringify(posts[1].providers) === JSON.stringify(['prov-a']) &&
          JSON.stringify(posts[1].models) === JSON.stringify(['model-a']) &&
          posts[1].concurrency === 4 && posts[1].requests === 10 &&
          posts[1].request_window === '1m');
        check('the undo receipt names what it reversed',
          await settleUntil(() => text('#oc-change').startsWith('undid: removed limit for ')) &&
          text('#oc-change') === 'undid: removed limit for provider prov-a · model model-a' &&
          d.getElementById('oc-undo').hidden);
      } finally {
        w.fetch = savedFetch;
      }

      // Undo must name the rule this write created, not the first rule in the
      // list. With older holds already present, an identity test that compares
      // against a null previous id resumes the oldest hold instead of the new
      // one, which is the worst failure the ledger can have.
      {
        const savedHoldFetch = w.fetch;
        const holdPosts = [];
        w.fetch = async (url, options) => {
          if (String(url).includes('/admin/pause')) {
            const body = JSON.parse(options.body);
            holdPosts.push(body);
            // Confirm exactly what was asked for, so the console sees the
            // rule this write created rather than a canned document.
            const base = {ok: true, paused: true, clients: [], providers: [],
              known_clients: ['client-a'], known_providers: [], until: null,
              queued: 0, default_max_queued: 0};
            if (body.paused === false) {
              return {ok: true, json: async () => ({...base, paused: true,
                holds: [{id: 'hold-old-1', all: true}, {id: 'hold-old-2', all: true}]})};
            }
            return {ok: true, json: async () => ({...base,
              holds: [{id: 'hold-old-1', all: true}, {id: 'hold-old-2', all: true},
                {id: 'hold-new', clients: body.clients, providers: body.providers}]})};
          }
          return savedHoldFetch(url, options);
        };
        w.applyPauseState({ok: true, paused: true, clients: [], providers: [],
          holds: [{id: 'hold-old-1', all: true}, {id: 'hold-old-2', all: true}],
          known_clients: ['client-a'], known_providers: [], until: null,
          queued: 0, default_max_queued: 0});
        w.openOperatorConsole('pause');
        w.ocOpenPauseComposer('');
        w.eval("ocDrafts.pause.scope.clients = ['client-a']; ocRender()");
        click('#oc-pane [data-oc-apply="pause"]');
        await settleUntil(() => holdPosts.length === 1);
        await settleUntil(() => !d.getElementById('oc-undo').hidden);
        click('#oc-undo');
        check('undo after creating a hold resumes that hold, not an older one',
          await settleUntil(() => holdPosts.length === 2) &&
          JSON.stringify(holdPosts[1]) === JSON.stringify({paused: false, id: 'hold-new'}));
        w.fetch = savedHoldFetch;
      }

      // One confirmation at a time: a second call settles the open one as a
      // cancel before replacing it, so neither promise is left pending.
      const first = w.ocConfirm({title: 'First question', body: 'one', confirm: 'ok'});
      const second = w.ocConfirm({title: 'Second question', body: 'two', confirm: 'ok'});
      check('a second confirmation settles the open one as a cancel',
        await first === false &&
        text('#oc-confirm-title') === 'Second question' &&
        !!d.getElementById('oc-confirm-dialog'));
      click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
      check('the replacement confirmation still resolves on its own action',
        await second === false && !d.getElementById('oc-confirm-dialog'));
      w.closeOperatorConsole();
    }

    // ---- 8. pause holds: modes, bodies, queue cap, resume ----
    {
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a']},
        {clients: ['client-a']});
      w.applyPauseState({ok: true, paused: false, holds: [], queued: 0,
        known_clients: ['client-a'], known_providers: ['prov-a'],
        until: null, default_max_queued: 16});
      w.openOperatorConsole('pause');
      startNew('pause');
      // A quick hold is the incident shortcut, but a hold's blast radius makes
      // an unbounded one-click action the worst control this console could
      // offer. It is time-bounded by construction and writes the same document
      // the composer writes, so ledger, undo and gating are identical.
      {
        const savedFetch = w.fetch;
        const posts = [];
        w.fetch = async (url, options) => {
          if (String(url).includes('/admin/pause') && options && options.method === 'POST') {
            const body = JSON.parse(options.body);
            posts.push(body);
            // Echo the scope vocabulary the server keeps, so the confirmed
            // document does not blank the pickers the next block reads.
            return {ok: true, json: async () => ({ok: true, paused: true,
              holds: body.paused === false ? [] : [{id: 'hold-quick', all: true,
                duration: body.duration || '', clients: [], providers: []}],
              clients: [], providers: [],
              known_clients: ['client-a'], known_providers: ['prov-a'],
              until: null, queued: 0, default_max_queued: 16})};
          }
          return savedFetch(url, options);
        };
        try {
          w.applyPauseState({ok: true, paused: false, holds: [], clients: [], providers: [],
            known_clients: ['client-a'], known_providers: ['prov-a'], until: null,
            queued: 0, default_max_queued: 0});
          w.eval("ocDrafts.pause = null; ocView = {kind: '', id: ''}");
          w.openOperatorConsole('pause');
          // The label names the scope as well as the bound: "hold 15m" read as
          // a duration preset for the composer beside it, and the fact that it
          // parks everything lived only in a hover title.
          check('the pause head offers a one-click hold naming its scope and bound',
            text('#oc-pane [data-oc-quick-hold]') === 'hold all 15m');
          // The incident shortcut is the section's one filled commit; the
          // sibling "new hold" is a plain control, so two accents never sit
          // side by side.
          check('the incident shortcut alone carries the accent',
            d.querySelector('#oc-pane [data-oc-quick-hold]').classList.contains('btn-accent') &&
            !d.querySelector('#oc-pane [data-oc-new="pause"]').classList.contains('btn-accent'));
          click('#oc-pane [data-oc-quick-hold]');
          check('a one-click hold posts a bounded all-traffic hold',
            await settleUntil(() => posts.length === 1) &&
            stable(posts[0]) === stable({paused: true, all: true, duration: '15m'}) &&
            !posts[0].max_queued);
          check('the quick hold is recorded so it can be undone',
            await settleUntil(() => text('#oc-change').startsWith('held ')) &&
            text('#oc-change').includes('all traffic'));
        } finally {
          w.fetch = savedFetch;
        }
        // A committed hold closes its composer and writes the ledger, so the
        // next block starts from a cleared draft and an empty receipt rather
        // than inheriting this block's.
        w.eval("ocDrafts.pause = null; ocView = {kind: '', id: ''};"
          + "ocChanges.length = 0; ocRenderFoot();");
        w.openOperatorConsole('pause');
        startNew('pause');
      }

      // The scope vocabulary for clients lives on the throttle document, so
      // this block seeds it alongside the pause document it picks from.
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a']},
        {clients: ['client-a']});
      w.applyPauseState({ok: true, paused: false, holds: [], clients: [], providers: [],
        known_clients: ['client-a'], known_providers: ['prov-a'], until: null,
        queued: 0, default_max_queued: 0});
      w.openOperatorConsole('pause');
      startNew('pause');
      check('the pause composer offers named, new and all modes',
        JSON.stringify(qa('#oc-pane [data-oc-choice="pause-mode"]')
          .map(chip => [chip.dataset.ocValue, chip.getAttribute('aria-pressed')])) ===
          JSON.stringify([['named', 'true'], ['new', 'false'], ['all', 'false']]));
      check('named scope with no pick cannot be applied',
        d.getElementById('oc-pause-apply').disabled &&
        text('#oc-pause-preview').includes('Pick at least one condition'));

      const savedFetch = w.fetch;
      let posts = [];
      const pauseFixture = holds => ({ok: true, paused: holds.length > 0, holds, queued: 0,
        clients: [], providers: [], known_clients: ['client-a'], known_providers: ['prov-a'],
        until: null, default_max_queued: 16});
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/pause') && options && options.method === 'POST') {
          const body = JSON.parse(options.body);
          posts.push(body);
          const holds = body.paused === false ? [] : [{
            id: body.all ? 'hold-all' : 'hold-named',
            all: !!body.all, clients: body.clients || [], providers: body.providers || [],
            duration: body.duration || '', max_queued: body.max_queued,
          }];
          return {ok: true, json: async () => pauseFixture(holds)};
        }
        return savedFetch(url, options);
      };
      try {
        pickKnown('clients', 'client-a');
        pickKnown('providers', 'prov-a');
        check('a named pause previews both dimensions',
          text('#oc-pause-preview') === 'Hold provider prov-a · client client-a until resumed.' &&
          !d.getElementById('oc-pause-apply').disabled);
        click('#oc-pause-apply');
        // Every scope dimension is restated, so an edit narrows rather than
        // silently inheriting a dimension the previous hold had set.
        check('a named hold posts every explicit scope dimension',
          await settleUntil(() => posts.length === 1) &&
          stable(posts[0]) === stable({paused: true, all: false, new: false,
            clients: ['client-a'], providers: ['prov-a'], duration: '',
            models: [], key_hashes: [], conversations: []}));
        check('the named hold is recorded as held',
          await settleUntil(() => text('#oc-change').startsWith('held ')) &&
          text('#oc-change') === 'held provider prov-a · client client-a');

        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        startNew('pause');
        choose('pause-mode', 'all');
        check('all traffic previews the global hold',
          text('#oc-pause-preview') === 'Hold all traffic until resumed.');
        click('#oc-pause-apply');
        check('an all-traffic hold omits the narrower scope fields',
          await settleUntil(() => posts.length === 2) &&
          stable(posts[1]) === stable({paused: true, all: true, duration: ''}) &&
          !('new' in posts[1]) && !('clients' in posts[1]) && !('providers' in posts[1]));

        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: false, holds: [], queued: 0,
          known_clients: ['client-a'], known_providers: ['prov-a'],
          until: null, default_max_queued: 16});
        w.eval("ocDrafts.pause = null; ocView = {kind: '', id: ''}; ocRender()");
        startNew('pause');
        choose('pause-mode', 'all');
        const capToggle = q('#oc-pane [data-oc-field="pauseCapOn"]');
        capToggle.checked = true;
        capToggle.dispatchEvent(new w.Event('change', {bubbles: true}));
        check('the queue cap field starts at the server default',
          q('#oc-pane [data-oc-field="pauseCap"]').value === '16');
        // The number is config.MaxQueueSize, published by the pause snapshot
        // as default_max_queued. A client-side fallback would be a second copy
        // of a server default, and it would apply exactly when the document
        // has not arrived.
        {
          const src = fs.readFileSync(path.join(STATIC, 'js', 'operator_console.js'), 'utf8')
            + fs.readFileSync(path.join(STATIC, 'js', 'operator_holds.js'), 'utf8');
          check('no console file carries its own copy of the default queue cap',
            !/default_max_queued\s*\|\|\s*\d/.test(src) &&
            !/\b1024\b/.test(src) &&
            typeof w.ocPauseDefaultCap === 'function');
          const saved = w.eval('pauseState.default_max_queued');
          w.eval('pauseState.default_max_queued = 64');
          check('the default queue cap is read from the server document',
            w.ocPauseDefaultCap() === 64);
          w.eval('pauseState.default_max_queued = ' + JSON.stringify(saved));
        }
        input('#oc-pane [data-oc-field="pauseCap"]', '-1');
        click('#oc-pause-apply');
        check('a negative queue cap is refused with no POST',
          await settleUntil(() => !!q('#oc-pane .oc-error')) &&
          text('#oc-pane .oc-error').includes('queue cap') && posts.length === 2);
        input('#oc-pane [data-oc-field="pauseCap"]', '8');
        click('#oc-pause-apply');
        check('a valid queue cap travels as max_queued',
          await settleUntil(() => posts.length === 3) && posts[2].max_queued === 8);

        // Unchecking the box on a stored hold must remove the cap. The body
        // used to omit max_queued, and the server keeps the previous value on
        // an edit, so the control silently did nothing while the row went on
        // printing a cap the operator had just cleared.
        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'hold-cap', all: true, max_queued: 8}], queued: 0,
          known_clients: [], known_providers: [], until: null, default_max_queued: 16});
        click('#oc-pane [data-oc-edit="pause"][data-oc-id="hold-cap"]');
        check('editing a capped hold prefills its queue cap',
          q('#oc-pane [data-oc-field="pauseCap"]')?.value === '8');
        const capOff = q('#oc-pane [data-oc-field="pauseCapOn"]');
        capOff.checked = false;
        capOff.dispatchEvent(new w.Event('change', {bubbles: true}));
        const beforeUncap = posts.length;
        click('#oc-pause-apply');
        check('unchecking a stored hold\'s cap sends max_queued 0',
          await settleUntil(() => posts.length === beforeUncap + 1) &&
          posts[beforeUncap].max_queued === 0 &&
          posts[beforeUncap].id === 'hold-cap');

        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'hold-r1', all: true}], queued: 0,
          known_clients: [], known_providers: [], until: null, default_max_queued: 0});
        const beforeResume = posts.length;
        click('#oc-pane [data-oc-resume="hold-r1"]');
        check('resuming one hold posts only its id',
          await settleUntil(() => posts.length === beforeResume + 1) &&
          stable(posts[beforeResume]) === stable({paused: false, id: 'hold-r1'}));
        check('the resume is recorded as resumed',
          await settleUntil(() => text('#oc-change').startsWith('resumed ')) &&
          text('#oc-change') === 'resumed all traffic');

        // A hold with parked work releases that backlog on resume, which is
        // the same consequence `resume all` confirms. Asking only in the bulk
        // case taught the operator that the single resume was safer.
        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'hold-q', all: true, queued: 5}], queued: 5,
          known_clients: [], known_providers: [], until: null, default_max_queued: 0});
        click('#oc-pane [data-oc-resume="hold-q"]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        check('resuming a hold that has parked work asks first',
          text('#oc-confirm-title') === 'Resume this hold?' &&
          d.querySelector('.oc-confirm-body').textContent.includes('5 parked requests') &&
          d.querySelector('.oc-confirm-body').textContent.includes('releases them'));
        const beforeQueued = posts.length;
        click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
        check('cancelling a queued resume leaves the hold in force',
          posts.length === beforeQueued && w.eval('pauseState.holds.length') === 1);
        click('#oc-pane [data-oc-resume="hold-q"]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        click('#oc-confirm-dialog [data-oc-confirm="ok"]');
        check('confirming a queued resume posts the single id',
          await settleUntil(() => posts.length === beforeQueued + 1) &&
          stable(posts[posts.length - 1]) === stable({paused: false, id: 'hold-q'}));

        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'hold-a', all: true}, {id: 'hold-b', clients: ['client-a']}],
          queued: 2, known_clients: ['client-a'], known_providers: [],
          until: null, default_max_queued: 0});
        const beforeAll = posts.length;
        click('#oc-pane [data-oc-resume-all]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        check('resuming every hold asks for confirmation first',
          text('#oc-confirm-title') === 'Resume every hold?' &&
          d.querySelector('.oc-confirm-body').textContent.includes('2 holds are') &&
          posts.length === beforeAll);
        click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
        check('cancelling resume-all keeps every hold', w.eval('pauseState.holds.length') === 2);
        click('#oc-pane [data-oc-resume-all]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        click('#oc-confirm-dialog [data-oc-confirm="ok"]');
        check('confirming resume-all posts the global resume',
          await settleUntil(() => posts.length === beforeAll + 1) &&
          stable(posts[beforeAll]) === stable({paused: false}) &&
          await settleUntil(() => text('#oc-change') === 'resumed every hold'));
      } finally {
        w.fetch = savedFetch;
      }
      w.closeOperatorConsole();
    }

    // ---- 9. capture sessions ----
    {
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a']},
        {clients: ['client-a'], models: ['model-a']});
      w.applyDebugState(debugDoc({ok: true, enabled: false, sessions: [],
        known_clients: ['client-a'], known_providers: ['prov-a'], known_models: ['model-a'],
        ttl: '24h', max_bytes: '1MiB'}));
      w.openOperatorConsole('capture');
      startNew('capture');
      check('a capture with no scope cannot start',
        d.getElementById('oc-capture-apply').disabled &&
        text('#oc-capture-preview') === 'Pick at least one condition.');
      check('the capture composer states its retention and byte bounds',
        text('#oc-pane .oc-facts').includes('24h') &&
        text('#oc-pane .oc-facts').includes('1MiB'));

      w.eval('storageState = {...storageState, enabled: false}');
      w.operatorConsoleSync('debug');
      pickKnown('clients', 'client-a');
      check('disabled durable storage warns and blocks the start action',
        text('#oc-pane .oc-warning').startsWith('Durable storage is off') &&
        d.getElementById('oc-capture-apply').disabled);
      // The warning names db_path, and db_path lives in one Settings pane. A
      // sentence that names a field with no control to reach it leaves the
      // operator to hunt the sheet for it. The link carries the category; the
      // settings section pins that the request lands there.
      check('the storage warning carries a control that opens the pane holding db_path',
        q('#oc-pane .oc-warning [data-oc-settings]')?.dataset.ocSettings === 'server' &&
        text('#oc-pane .oc-warning [data-oc-settings]') === 'open server settings');
      w.eval('storageState = {...storageState, enabled: true}');
      w.operatorConsoleSync('debug');
      check('re-enabling storage clears the warning and allows the start action',
        !q('#oc-pane .oc-warning') && !d.getElementById('oc-capture-apply').disabled);

      const savedFetch = w.fetch;
      const posts = [];
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/debug') && options && options.method === 'POST') {
          const body = JSON.parse(options.body);
          posts.push(body);
          const sessions = body.enabled === false ? [] : [{
            id: 'sess-created', clients: body.clients, providers: body.providers,
            models: body.models, duration: body.duration,
          }];
          const doc = debugDoc({ok: true,
            enabled: sessions.length > 0, sessions,
            known_clients: ['client-a'], known_providers: ['prov-a'], known_models: ['model-a'],
            ttl: '24h', max_bytes: '1MiB'});
          // The debug route returns created_id on a create, exactly so the
          // client never has to infer which session its write installed.
          if (body.enabled !== false && !body.id) doc.created_id = 'sess-created';
          return {ok: true, json: async () => doc};
        }
        return savedFetch(url, options);
      };
      try {
        pickKnown('models', 'model-a');
        click('#oc-capture-apply');
        check('creating a capture posts enabled, scope and duration',
          await settleUntil(() => posts.length === 1) &&
          stable(posts[0]) === stable({enabled: true, clients: ['client-a'],
            providers: [], models: ['model-a'], duration: ''}));
        check('the capture start is recorded',
          await settleUntil(() => text('#oc-change').startsWith('capturing ')) &&
          text('#oc-change') === 'capturing model model-a · client client-a');
        // The undo closes the session this write installed. It used to infer
        // that session from a before/after set difference, which picks the
        // wrong one when another operator starts a capture at the same time.
        // The route returns created_id for exactly this, so the undo must
        // follow it rather than the list order.
        {
          const undoWorks = await (async () => {
            const before = posts.length;
            click('#oc-undo');
            const settled = await settleUntil(() => posts.length > before, 2000);
            const last = posts[posts.length - 1];
            return settled && last.enabled === false && last.id === 'sess-created';
          })();
          check('the capture undo closes the session the reply named', undoWorks);
        }

        // The undo above closed the session, so restore the fixture before the
        // stop path is exercised. The undo closure has already been consumed,
        // which is the behaviour under test.
        await settleUntil(() => w.eval('operatorState.debug.busy') === false);
        w.applyDebugState(debugDoc({ok: true, enabled: true,
          sessions: [{id: 'sess-created', clients: ['client-a'], models: ['model-a']}],
          known_clients: ['client-a'], known_providers: ['prov-a'], known_models: ['model-a'],
          ttl: '24h', max_bytes: '1MiB'}));
        const beforeStop = posts.length;
        click('#oc-pane [data-oc-stop="sess-created"]');
        check('stopping one capture posts only its id',
          await settleUntil(() => posts.length === beforeStop + 1) &&
          stable(posts[beforeStop]) === stable({enabled: false, id: 'sess-created'}));
        check('the capture stop is recorded',
          await settleUntil(() => text('#oc-change').startsWith('stopped capture on ')) &&
          text('#oc-change') === 'stopped capture on model model-a · client client-a');

        await settleUntil(() => w.eval('operatorState.debug.busy') === false);
        w.applyDebugState(debugDoc({ok: true, enabled: true,
          sessions: [{id: 'sess-a', clients: ['client-a']}, {id: 'sess-b', models: ['model-a']}],
          known_clients: [], known_providers: [], known_models: [],
          ttl: '24h', max_bytes: '1MiB'}));
        click('#oc-pane [data-oc-stop-all]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        check('stopping every capture asks for confirmation first',
          text('#oc-confirm-title') === 'Stop every capture?' &&
          d.querySelector('.oc-confirm-body').textContent.includes('2 sessions are') &&
          posts.length === beforeStop + 1);
        click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
        check('cancelling stop-all leaves both sessions', w.eval('debugState.sessions.length') === 2);
      } finally {
        w.fetch = savedFetch;
      }
      // The cancelled stop-all left its fixture sessions in force; the
      // overview below counts every rule, so clear them here.
      w.applyDebugState(emptyDebug());
      w.closeOperatorConsole();
    }

    // ---- long rule lists stay navigable ----
    {
      // A console with two rules has no use for a search box, and one with
      // fourteen cannot be used without it: the pane overflows and finding a
      // rule became a scroll. The filter appears at the threshold, narrows on
      // the text each row already renders, and states that it hid rows rather
      // than letting an empty filter read as an empty policy.
      const many = [];
      for (let i = 0; i < 8; i++) {
        many.push({id: 'flt-' + i, scope: {models: ['m-' + i]}, concurrency: 2,
          requests: 0, request_window: '', tokens: 0, token_window: '',
          anti_burst: false, ramp_duration: '', source: 'ui', in_flight: 0, queued: 0});
      }
      throttleDoc({ok: true, active: true, known_providers: [], throttles: many.slice(0, 2)});
      w.openOperatorConsole('limits');
      check('a short rule list does not grow a filter row',
        !q('#oc-pane [data-oc-list-filter]') && qa('#oc-pane .oc-rule').length === 2);

      throttleDoc({ok: true, active: true, known_providers: [], throttles: many});
      w.operatorConsoleSync('throttle');
      check('a long rule list offers a filter naming how many rules it holds',
        !!q('#oc-pane [data-oc-list-filter="limits"]') &&
        qa('#oc-pane .oc-rule').length === 8 &&
        q('#oc-pane [data-oc-list-filter="limits"]').placeholder === 'filter 8 rules');

      input('#oc-pane [data-oc-list-filter="limits"]', 'm-3');
      check('the filter narrows the list to the rule whose reading matches',
        qa('#oc-pane .oc-rule').length === 1 &&
        text('#oc-pane .oc-rule .oc-rule-scope') === 'model m-3');
      // A partial result is as misleading as an empty one: one row on screen
      // beside a badge of eight reads as an eight-rule policy narrowed to one,
      // or worse, as a one-rule policy. The note names the fraction.
      check('a filter that hides some rules names how many of the total it shows',
        text('#oc-pane .oc-list-note') === '1 of 8 rules shown. clear the filter' &&
        qa('#oc-pane [data-oc-list-clear="limits"]').length === 1);

      input('#oc-pane [data-oc-list-filter="limits"]', 'nothing matches this');
      check('a filter that hides every rule says so instead of reading as empty',
        qa('#oc-pane .oc-rule').length === 0 &&
        text('#oc-pane .oc-empty').includes('No rule matches that filter') &&
        text('#oc-pane .oc-empty').includes('8 rules are in force'));
      click('#oc-pane [data-oc-list-clear="limits"]');
      check('clearing the filter restores every rule and the filter row',
        qa('#oc-pane .oc-rule').length === 8 &&
        q('#oc-pane [data-oc-list-filter="limits"]').value === '' &&
        !q('#oc-pane .oc-list-note'));
      // Dropping below the filter threshold must keep the control: a stored
      // filter used to outlive its input, leaving the list narrowed with no
      // visible cause and no way back except leaving the section.
      input('#oc-pane [data-oc-list-filter="limits"]', 'm-3');
      // Keep the matching rule plus one other, so the list drops to two rules
      // (below the threshold) while the stored filter still selects one.
      throttleDoc({ok: true, active: true, known_providers: [],
        throttles: [many[0], many[3]]});
      w.operatorConsoleSync('throttle');
      check('a stored filter keeps its control when the list shrinks below the threshold',
        !!q('#oc-pane [data-oc-list-filter="limits"]') &&
        q('#oc-pane [data-oc-list-filter="limits"]').value === 'm-3' &&
        qa('#oc-pane .oc-rule').length === 1);
      click('#oc-pane [data-oc-list-clear="limits"]');
      // Two rules is below the threshold, so clearing the filter also retires
      // its input: the control exists only while a filter or a long list
      // needs one.
      check('the retained filter still clears back to every rule',
        qa('#oc-pane .oc-rule').length === 2 &&
        !q('#oc-pane [data-oc-list-filter="limits"]') &&
        w.eval('ocListFilter.limits') === '');
      w.closeOperatorConsole();
    }

    // ---- the phone sheet keeps enough pane to work in ----
    {
      // jsdom does not apply media queries, so this is a source contract. At
      // 70dvh a 320x568 phone put the pane at 253px against a 771px composer:
      // opening one left zero rule rows visible and the list three pane
      // heights away. The sheet may take most of a phone, but it must not
      // lose the dismissal strip: 100dvh minus its own cap stays positive.
      const css = fs.readFileSync(path.join(STATIC, 'css', 'console.css'), 'utf8');
      const sheet = /@media \(max-width: 800px\)[\s\S]*?\.mn\.oc\[data-placement="sheet"\]\s*\{([^}]*)\}/.exec(css);
      check('the phone sheet takes most of the viewport but not all of it',
        !!sheet && /88dvh/.test(sheet[1]) &&
        sheet[1].includes('calc(100dvh - 16px)'));
      // The pinned commit row must park on the pane edge, and the pane must
      // reserve room for it so a focused field cannot land underneath.
      const block = css.slice(css.indexOf('@media (max-width: 640px)'));
      check('the pinned commit row parks on the pane edge',
        /\.mn\.oc \.oc-compose-ft\s*\{[^}]*position:\s*sticky;\s*bottom:\s*0/.test(block));
      check('the pane reserves room for the pinned commit row',
        /\.mn\.oc \.oc-pane\s*\{[^}]*scroll-padding-bottom:\s*76px/.test(block));
    }

    // ---- the phone row keeps the fact the row exists to carry ----
    {
      // jsdom does not apply media queries, so this is a source contract: the
      // phone block used to set .oc-rule-budget to display:none, which left a
      // 390px row reading "model glm-5.3 · idle" with no statement of what the
      // limit caps. The rule may be restyled there, never removed.
      const css = fs.readFileSync(path.join(STATIC, 'css', 'console.css'), 'utf8');
      const phone = css.slice(css.indexOf('@media (max-width: 640px)'));
      const block = phone.slice(0, phone.indexOf('\n@media '));
      const budgetRules = [...block.matchAll(/\.oc-rule-budget[^{]*\{([^}]*)\}/g)].map(m => m[1]);
      check('the phone rule row never hides the budget the rule enforces',
        budgetRules.length > 0 &&
        budgetRules.every(rule => !/display:\s*none/.test(rule)));
    }

    // ---- the rule row keeps its scope and budget when the live figure grows ----
    {
      // jsdom computes no grid tracks, so this is a source contract. The live
      // column was an `auto` track, which is sized to its max-content: measured
      // in Chrome at 1280, a busy policy (11 in flight, 27 queued, both request
      // and token budgets remaining) made that track 457px of a 498px row and
      // squeezed the edit target to 16px. The scope and the budget rendered at
      // zero width, so the row hid the cap it exists to state.
      const css = fs.readFileSync(path.join(STATIC, 'css', 'console.css'), 'utf8');
      const row = /^\.oc-rule\s*\{([^}]*)\}/m.exec(css);
      check('the rule row caps its live column instead of sizing it to content',
        !!row && /grid-template-columns:\s*40px\s+minmax\(0,\s*1fr\)\s+fit-content\(var\(--oc-rule-side-max\)\)/.test(row[1]));
      // One owner for the cap: the token is declared on the console shell, so
      // the row and any later rule that needs the same share read it once.
      const shell = /^\.mn\.oc\s*\{([^}]*)\}/m.exec(css);
      check('the live column cap is declared once on the console shell',
        !!shell && /--oc-rule-side-max:\s*\d+%/.test(shell[1]) &&
        (css.match(/--oc-rule-side-max:/g) || []).length === 1);
    }

    // ---- 10. export builder ----
    {
      w.__ocExportRecords = [{
        ...mkRec('oc-export'),
        provider: 'prov-a', model: 'model-a', client: 'client-a', conversation_id: 'conv-1',
      }];
      w.eval('lastData.records = window.__ocExportRecords');
      const savedDownload = w.triggerDownload;
      const downloads = [];
      w.triggerDownload = (href, download) => { downloads.push([href, download]); };
      // Both modes preview through the same server count, so the fixture
      // answers with one throughout instead of only for the filtered mode.
      const savedFetch = w.fetch;
      let countBody = null;
      let countCalls = 0;
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/purge/count')) {
          countBody = JSON.parse(options.body);
          countCalls += 1;
          return {ok: true, json: async () => ({count: 7})};
        }
        return savedFetch(url, options);
      };
      w.openOperatorConsole('export');
      try {
        check('all history previews the same record count as a filter (' + text('#oc-export-receipt') + '|' + countCalls + ')',
          await settleUntil(() => text('#oc-export-receipt') === '7 records · JSON, gzip', 3000) &&
          countBody && countBody.filter.provider === '' && !d.getElementById('oc-export-download').disabled &&
          countCalls >= 1);
        click('#oc-pane [data-oc-download]');
        // The artifact streams, so the client cannot observe the transfer's
        // outcome. The ledger and toast report the request it made.
        check('all history downloads the bare export URL and names the count',
          JSON.stringify(downloads[0]) === JSON.stringify(['/metrics/export', '']) &&
          text('#oc-change') === 'downloading 7 records');

        choose('export-mode', 'filter');
        check('filter mode with no filter asks for one and disables download',
          text('#oc-export-receipt') === 'choose a filter to preview the export' &&
          d.getElementById('oc-export-download').disabled);
        {
          click('#oc-pane [data-oc-filter-pick="provider"]');
          click('#oc-pane [data-oc-filter-value="provider"][data-oc-value="prov-a"]');
          check('a chosen filter previews its record count',
            await settleUntil(() => text('#oc-export-receipt') === '7 records · JSON, gzip', 3000) &&
            !d.getElementById('oc-export-download').disabled);
          check('the count preview uses the same provider predicate as the download',
            countBody && countBody.filter.provider === 'prov-a' && countBody.filter.model === '' &&
            countBody.filter.client === '' && countBody.filter.conversation_id === '' &&
            countBody.filter.status_code === 0 && countBody.filter.has_error === false &&
            countBody.filter.debug === false);
          click('#oc-pane [data-oc-download]');
          check('the filtered download uses the exact previewed URL',
            JSON.stringify(downloads[1]) ===
              JSON.stringify(['/metrics/export?provider=prov-a', '']) &&
            text('#oc-change') === 'downloading 7 records');
        }
        {
          // Switching back to all history must drop the predicate, not just
          // the label. The filter strip is hidden in that mode, so the
          // provider chosen above would download 5 records under a chip that
          // says all history, with no visible sign of the narrowing.
          choose('export-mode', 'all');
          check('all history downloads the whole history even after a filter',
            await settleUntil(() => text('#oc-export-receipt') === '7 records · JSON, gzip', 3000));
          click('#oc-pane [data-oc-download]');
          check('the mode, the receipt and the URL agree after a mode switch',
            JSON.stringify(downloads[downloads.length - 1]) ===
              JSON.stringify(['/metrics/export', '']) &&
            countBody && countBody.filter.provider === '');
          choose('export-mode', 'filter');
          check('returning to filter mode restores the filter work',
            await settleUntil(() => text('#oc-export-receipt') === '7 records · JSON, gzip', 3000) &&
            q('#oc-pane [data-oc-filter-pick="provider"]').closest('.oc-cond')
              .getAttribute('data-set') === '1');
        }
        click('#oc-pane [data-oc-reset-export]');
        check('reset restores the blank all-history draft and recounts it',
          await settleUntil(() => text('#oc-export-receipt') === '7 records · JSON, gzip', 3000) &&
          q('#oc-pane [data-oc-choice="export-mode"][data-oc-value="all"]')
            .getAttribute('aria-pressed') === 'true' &&
          !d.getElementById('oc-export-download').disabled);
      } finally {
        w.triggerDownload = savedDownload;
        w.fetch = savedFetch;
      }
      w.closeOperatorConsole();
    }

    // ---- a numeric filter dimension must narrow, not throw ----
    {
      // Seeded exactly the way the export block above seeds the ring, so the
      // status vocabulary is the real numeric one from records.
      w.__ocStatusRecords = [
        {...mkRec('exp-200'), status_code: 200, provider: 'prov-a'},
        {...mkRec('exp-499'), status_code: 499, provider: 'prov-b'},
      ];
      w.eval('lastData.records = window.__ocStatusRecords');
      w.openOperatorConsole('export');
      choose('export-mode', 'filter');
      click('#oc-pane [data-oc-filter-pick="status"]');
      // Status values come from the record as numbers. A filter box that
      // called toLowerCase on them threw a TypeError and left the list
      // unnarrowed, so the one filter the section offers there was unusable.
      const typed = q('#oc-pane [data-oc-filter-filter="status"]');
      typed.value = '49';
      typed.dispatchEvent(new w.Event('input', {bubbles: true}));
      // An age filter's absolute bounds move every millisecond. The count's
      // staleness guard used to compare the whole predicate, so every reply
      // looked stale, the count never landed, and the receipt stayed on
      // "choose a filter to preview the export" while download stayed off.
      {
        const savedFetch = w.fetch;
        let seen = null;
        w.fetch = async (url, options) => {
          if (String(url).includes('/admin/purge/count')) {
            seen = JSON.parse(options.body);
            // A realistic round trip: the reply arrives after the clock moved.
            await new Promise(r => setTimeout(r, 5));
            return {ok: true, json: async () => ({ok: true, count: 12})};
          }
          return savedFetch(url, options);
        };
        try {
          w.eval("ocDrafts.export = ocBlankExportDraft(); ocDrafts.export.mode = 'filter';"
            + " ocDrafts.export.age = '1h'; ocDrafts.export.provider = 'prov-a';"
            + " ocScheduleExportCount();");
          check('an age filter still lands its count after a round trip',
            await settleUntil(() => text('#oc-export-receipt') === '12 records · JSON, gzip', 3000) &&
            !d.getElementById('oc-export-download').disabled &&
            !!seen && seen.filter.before_ms > 0 && seen.filter.after_ms === 0);
          // The download must reuse the epoch the count measured, not the
          // current clock: otherwise the file describes a slightly later
          // window than the receipt the operator just read.
          const epoch = w.eval('ocDrafts.export.epoch');
          const urls = [];
          const savedDownload = w.triggerDownload;
          w.triggerDownload = href => { urls.push(href); };
          try {
            w.eval('ocDownloadExport()');
          } finally {
            w.triggerDownload = savedDownload;
          }
          const last = urls[urls.length - 1] || '';
          check('the download reuses the count\'s epoch instead of the current clock',
            epoch > 0 && last.includes('before_ms=' + (epoch - 3600000)) &&
            !last.includes('after_ms='));
        } finally {
          w.fetch = savedFetch;
        }
      }

      check('a typed status filter narrows the numeric vocabulary',
        JSON.stringify(qa('#oc-pane [data-oc-filter-value="status"]')
          .map(row => row.dataset.ocValue)) === JSON.stringify(['', '499']));
      // The export filter renders through the same row markup as the scope
      // picker, so it owes the same one-tab-stop contract.
      {
        const chip = q('#oc-pane [data-oc-filter-pick="status"]');
        const list = q('#oc-pane [role="menu"]');
        const rows = qa('#oc-pane [data-oc-filter-value="status"]');
        check('the export filter chip announces its menu and keeps one tab stop',
          chip.getAttribute('aria-haspopup') === 'menu' &&
          chip.getAttribute('aria-controls') === list.id &&
          rows[0].tabIndex === 0 && rows.slice(1).every(row => row.tabIndex === -1));
      }
      w.closeOperatorConsole();
    }

    // ---- 11. overview KPIs, in-force rows, drain and history ----
    {
      w.applyPauseState({ok: true, paused: true,
        holds: [{id: 'ov-hold', all: true}], queued: 3,
        known_clients: [], known_providers: [], until: null, default_max_queued: 0});
      // The throttle document is where the server merges every seen name, so
      // it carries the client vocabulary the drain suggestions read.
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'],
        known_clients: ['client-a'],
        throttles: [{id: 'ov-limit', scope: {providers: ['prov-a']}, concurrency: 2,
          requests: 10, request_window: '1m', source: 'ui'}]});
      w.openOperatorConsole('overview');
      // The counts are stated once, in the console head and on the section
      // chips; the overview body is the rules themselves.
      check('the overview opens on the rules with no repeated count band',
        !q('#oc-pane .oc-kpi') &&
        text('#oc-live').includes('paused 1') && text('#oc-live').includes('limits 1') &&
        q('#oc-tab-pause .oc-rail-n').textContent === '1' &&
        q('#oc-tab-limits .oc-rail-n').textContent === '1');
      const rows = qa('.oc-ov-row');
      check('the in-force list carries one row per rule',
        rows.length === 2 &&
        JSON.stringify(rows.map(row => row.dataset.ocSection)) ===
          JSON.stringify(['pause', 'limits']) &&
        JSON.stringify(rows.map(row => row.querySelector('.oc-ov-kind').textContent)) ===
          JSON.stringify(['pause', 'limit']) &&
        rows[0].querySelector('.oc-ov-scope').textContent === 'all traffic' &&
        rows[1].querySelector('.oc-ov-scope').textContent === 'provider prov-a');
      // An in-force row is one reading, not a stack of lines: the kind, the
      // scope and the detail share a line, and each keeps its own box so the
      // kind tag cannot run into the scope. The row rule once vanished with a
      // neighbouring block and the overview silently became one run of text,
      // so its own computed layout is pinned here. The row is a wrapping flex
      // line rather than a grid: a grid split gave the budget half the row and
      // cut the scope to "client...", and the row must instead let the budget
      // drop to its own line so neither fact is truncated.
      const ovStyle = w.getComputedStyle(rows[1]);
      check('an in-force row lays out kind, scope and detail as one wrapping line',
        ovStyle.display === 'flex' &&
        ovStyle.flexWrap === 'wrap' &&
        rows[1].querySelectorAll('.oc-ov-kind, .oc-ov-scope, .oc-ov-detail').length === 3 &&
        // The three boxes sit on one line until the budget must wrap.
        w.getComputedStyle(rows[0].querySelector('.oc-ov-kind')).flex === '0 0 auto');
      // A row carries two facts: the scope that identifies the rule and the
      // budget it enforces. Neither may be clipped. A grid split gave the
      // budget half the row and cut the scope to "client...", and clipping
      // both in step was no better: the row wraps the budget to a second
      // line instead, so it grows only for the rules that need it.
      check('an overview row never clips the scope or the budget it carries',
        qa('#oc-pane .oc-ov-scope').every(el => el.scrollWidth <= el.clientWidth + 1) &&
        qa('#oc-pane .oc-ov-detail').every(el => el.scrollWidth <= el.clientWidth + 1));
      // An overview row names one rule, so it opens that rule in its editor
      // instead of dropping the operator at the top of a list to search.
      check('an in-force row carries the rule it names',
        rows[1].dataset.ocTarget === 'ov-limit' && rows[0].dataset.ocTarget === 'ov-hold');
      // A policy outside its window enforces nothing. The readout says
      // `off schedule`, and the section row says it too; the overview used to
      // list it as an ordinary in-force row, so one rule read two ways.
      w.applyThrottleState({ok: true, active: true, known_providers: [],
        throttles: [
          {id: 'ov-live', scope: {providers: ['prov-a']}, concurrency: 2, scheduled: true},
          {id: 'ov-parked', scope: {providers: ['prov-b']}, concurrency: 2, scheduled: false},
        ]});
      w.operatorConsoleSync('throttle');
      {
        const parked = q('#oc-pane .oc-ov-row[data-oc-target="ov-parked"]');
        const live = q('#oc-pane .oc-ov-row[data-oc-target="ov-live"]');
        check('the overview marks an off-schedule limit instead of listing it as in force',
          !!parked && text(parked.querySelector('.oc-ov-detail')).includes('off schedule') &&
          parked.querySelector('.oc-ov-kind').getAttribute('data-oc-off-schedule') === '1' &&
          !!live && !text(live.querySelector('.oc-ov-detail')).includes('off schedule') &&
          !live.querySelector('.oc-ov-kind').hasAttribute('data-oc-off-schedule'));
      }
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'],
        throttles: [{id: 'ov-limit', scope: {providers: ['prov-a']}, concurrency: 2, scheduled: true}]});
      w.operatorConsoleSync('throttle');
      w.openOperatorConsole('overview');
      click('#oc-pane .oc-ov-row[data-oc-section="limits"]');
      check('an in-force row opens the rule it names',
        paneSection() === 'limits' &&
        selectedTab().dataset.ocSection === 'limits' &&
        w.eval('ocView.id') === 'ov-limit' &&
        !!q('#oc-pane .oc-compose[data-kind="limit"]'));

      w.openOperatorConsole('overview');
      const savedFetch = w.fetch;
      const drains = [];
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/queue/drain')) {
          drains.push(JSON.parse(options.body));
          return {ok: true, json: async () => ({ok: true, drained_through: drains.length})};
        }
        return savedFetch(url, options);
      };
      try {
        click('#oc-pane [data-oc-drain]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        check('a drain asks for confirmation before releasing the queue',
          text('#oc-confirm-title') === 'Drain the queue?' &&
          d.querySelector('.oc-confirm-body').textContent.includes('Every parked request') &&
          drains.length === 0);
        click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
        check('cancelling a drain sends nothing', drains.length === 0);
        click('#oc-pane [data-oc-drain]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        click('#oc-confirm-dialog [data-oc-confirm="ok"]');
        check('draining everything posts the all scope and records the receipt',
          await settleUntil(() => drains.length === 1) &&
          stable(drains[0]) === stable({all: true}) &&
          await settleUntil(() => text('#oc-change') === 'drained the queue'));

        // The drain's bootstrap refresh re-reads the stubbed pause document
        // (queued 0), which disables the action. Re-seed the queue so the
        // scoped case exercises the real control rather than a stale draft.
        // The drain's bootstrap refresh replaces both operator documents, so
        // both vocabularies are re-seeded: the server always reports its seen
        // names on the throttle document.
        throttleDoc({ok: true, active: true, known_providers: ['prov-a'],
          throttles: [{id: 'ov-limit', scope: {providers: ['prov-a']}, concurrency: 2,
            requests: 10, request_window: '1m', source: 'ui'}]}, {clients: ['client-a']});
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'ov-hold', all: true}], queued: 3,
          known_clients: ['client-a'], known_providers: [], until: null, default_max_queued: 0});
        w.openOperatorConsole('overview');
        await settleUntil(() => w.eval('pauseState.queued') === 3);
        choose('drain-scope', 'client');
        // The drain value stays free text (a client may not have sent traffic
        // since a restart) but it now offers the names the console knows, so a
        // drain does not require retyping a value a picker would have offered.
        check('a scoped drain offers the known names as suggestions',
          [...(q('#oc-pane datalist#oc-drain-values')?.options || [])]
            .some(option => option.value === 'client-a'));
        input('#oc-pane [data-oc-drain-value]', 'client-a');
        await settleUntil(() => q('#oc-pane [data-oc-drain]') && !q('#oc-pane [data-oc-drain]').disabled);
        click('#oc-pane [data-oc-drain]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        click('#oc-confirm-dialog [data-oc-confirm="ok"]');
        check('a client-scoped drain posts only that client',
          await settleUntil(() => drains.length === 2) &&
          stable(drains[1]) === stable({client: 'client-a'}) &&
          await settleUntil(() => text('#oc-change') === 'drained client client-a'));
      } finally {
        w.fetch = savedFetch;
      }

      // The ledger has one owner: the console footer, newest first. The
      // overview does not repeat it, so the panel keeps one scroll region.
      w.openOperatorConsole('overview');
      const phrases = evalJSON('ocChanges.map(change => change.phrase)');
      check('the change ledger lives once, in the console footer',
        !q('#oc-pane .oc-history') &&
        text('#oc-change') === phrases[phrases.length - 1] &&
        !!q('#oc-undo'));
      w.closeOperatorConsole();
    }

    // ---- 12. system: restart wording, eligibility and confirmation ----
    {
      // The system section queues a status poll on every render; pin the
      // ineligible document so the queued poll cannot re-enable the action.
      const savedFetch = w.fetch;
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/restart') && (!options || options.method !== 'POST')) {
          return {ok: true, json: async () => ({ok: true, phase: 'idle', rank: -1,
            available: false, reason: 'source unavailable', pid: 1, started_at: 2})};
        }
        return savedFetch(url, options);
      };
      try {
        w.openOperatorConsole('system');
        check('the system section owns restart and explains the handoff',
          !!d.getElementById('btn-restart-now') &&
          d.getElementById('btn-restart-now').hasAttribute('data-oc-restart') &&
          /in-flight streams/.test(text('#restart-description')) &&
          /queue/.test(text('#restart-description')));
        // The section's own status poll disables the action and names the
        // server's reason in the row's count line and its warning paragraph.
        // The warning is painted by the ordinary render path, so the shared
        // sync hook (the same one every other operator change calls) is what
        // repaints an already-open pane.
        check('an ineligible status poll disables restart and names the reason',
          await settleUntil(() => d.getElementById('btn-restart-now').disabled &&
            text('#restart-count') === 'source unavailable'));
        w.operatorConsoleSync('system');
        check('a system repaint shows the in-place warning for an ineligible build',
          text('#oc-pane .oc-warning') === 'source unavailable' &&
          d.getElementById('btn-restart-now').disabled);
        w.applyRestartState({available: false, reason: ''});
        w.operatorConsoleSync('system');
        check('an ineligible build with no reason falls back to the generic warning',
          text('#oc-pane .oc-warning') ===
            'This build cannot restart in place. Update the container or service externally.' &&
          d.getElementById('btn-restart-now').disabled);
        w.applyRestartState({available: true, reason: ''});
        w.operatorConsoleSync('system');
        check('an eligible build offers the action again with no warning',
          !q('#oc-pane .oc-warning') && !d.getElementById('btn-restart-now').disabled);
      } finally {
        w.fetch = savedFetch;
        // Restoring the whole document (not just `available`) keeps the
        // started_at identity the restart poll compares against intact for
        // the sections that follow.
        w.eval('restartServerState = JSON.parse(' + JSON.stringify(savedRestartState) + '); syncRestartControls()');
      }

      // A stubbed confirmation proves the action asks before any POST.
      const savedConfirm = w.ocConfirm;
      let asked = null;
      w.ocConfirm = opts => { asked = opts; return Promise.resolve(false); };
      const before = state.restartState.postCount;
      try {
        w.openOperatorConsole('system');
        click('#btn-restart-now');
        check('restart asks for confirmation and sends nothing on cancel',
          asked && asked.title === 'Rebuild and restart?' &&
          state.restartState.postCount === before);
        // A staged database restore is applied by the restart that follows: the
        // dialog that commits the restart must say so.
        w.eval('window.__savedDoc = settingsDoc; settingsDoc = Object.assign({}, settingsDoc, {backup: {pending_database: true}})');
        click('#btn-restart-now');
        check('the restart dialog names a staged database restore',
          !!asked && /restored database is staged/.test(asked.body || ''));
        w.eval('settingsDoc = window.__savedDoc; delete window.__savedDoc');
      } finally {
        w.ocConfirm = savedConfirm;
        w.closeOperatorConsole();
      }

      // The real dialog is the shared confirmation owner, not a bespoke one.
      w.openOperatorConsole('system');
      click('#btn-restart-now');
      await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
      check('the real restart confirmation names the action',
        text('#oc-confirm-title') === 'Rebuild and restart?' &&
        text('#oc-confirm-dialog [data-oc-confirm="ok"]') === 'restart now');
      click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
      check('cancelling the real dialog sends no restart',
        !d.getElementById('oc-confirm-dialog') &&
        state.restartState.postCount === before);
      w.closeOperatorConsole();
    }

    // ---- 13. one error surface for rejected writes ----
    {
      const policy = {
        id: 'pol-err', scope: {providers: ['prov-a']}, schedule: null,
        concurrency: 4, requests: 0, request_window: '', tokens: 0, token_window: '',
        anti_burst: false, ramp_duration: '', source: 'ui', in_flight: 0, queued: 0,
      };
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'], throttles: [policy]});
      w.openOperatorConsole('limits');
      click('#oc-pane [data-oc-edit="limit"][data-oc-id="pol-err"]');
      const savedFetch = w.fetch;
      w.fetch = async (url, options) => {
        if (String(url).includes('/admin/throttle') && options && options.method === 'POST') {
          return {ok: false, json: async () => ({error: 'rule rejected'})};
        }
        return savedFetch(url, options);
      };
      try {
        click('#oc-lim-apply');
        await settleUntil(() => !!q('#oc-pane .oc-banner[role="alert"]'));
        // One surface, not two. The banner is the visible rejection while the
        // console is open; also firing a toast spoke the same sentence twice
        // to a screen reader.
        // The toast element persists across the suite and may still be
        // showing an unrelated earlier confirmation, so the assertion is that
        // it never carries this rejection, not that it is hidden.
        check('a rejected write is named once, in the open pane',
          text('#oc-pane .oc-banner[role="alert"] span') === 'rule rejected' &&
          !text('#toast').includes('rule rejected'));
        {
          // Every unrelated repaint used to re-insert a fresh role="alert"
          // carrying the same sentence, so assistive tech re-announced the
          // rejection on each poll. The node is now stable.
          const before = q('#oc-pane .oc-banner[role="alert"]');
          w.operatorConsoleSync('throttle');
          w.eval('ocRender()');
          const after = q('#oc-pane .oc-banner[role="alert"]');
          check('a repaint keeps the same rejection node instead of re-announcing it',
            !!after && after === before &&
            text('#oc-pane .oc-banner[role="alert"] span') === 'rule rejected');
        }
        click('#oc-pane [data-oc-error-dismiss]');
        check('dismissing the banner clears the stored error',
          !q('#oc-pane .oc-banner') && w.eval('ocError') === '');
      } finally {
        w.fetch = savedFetch;
      }

      // An undo that the server refuses must not report success. The shared
      // write owner shows the rejection in the banner and never throws, so the
      // ledger used to pop the entry and print "undid: ..." over the top of the
      // refusal: the operator lost both the change and the retry.
      {
        const savedUndoFetch = w.fetch;
        w.fetch = async (url, options) => {
          if (String(url).includes('/admin/throttle') && options && options.method === 'POST') {
            return {ok: false, json: async () => ({error: 'restore refused'})};
          }
          return savedUndoFetch(url, options);
        };
        try {
          w.ocRecordChange('removed limit for provider prov-a', async () => {
            const ok = await w.mutateOperator('throttle', {id: 'pol-err', clear: true});
            return ok;
          }, 'ok');
          await settleUntil(() => !d.getElementById('oc-undo').hidden);
          click('#oc-undo');
          check('a refused undo keeps the entry and says it could not undo',
            await settleUntil(() => text('#oc-change').startsWith('could not undo: ')) &&
            text('#oc-change').includes('could not undo: ') &&
            !text('#oc-change').startsWith('undid: ') &&
            !d.getElementById('oc-undo').hidden);
          // The entry stays retryable: a second attempt still runs the closure
          // instead of finding an empty ledger.
          check('the refused undo leaves a retryable entry in the ledger',
            w.eval('ocChanges.length') > 0 &&
            typeof w.eval('ocChanges[ocChanges.length - 1].undo') === 'function');
        } finally {
          w.fetch = savedUndoFetch;
          w.eval('ocChanges.length = 0; ocOperatorErrorClear();');
        }
      }
      w.closeOperatorConsole();
    }

    // ---- a control whose rule vanished must answer the click ----
    {
      // A poll can drop a rule while its row is still painted. The lookup then
      // returned early, so the click gave no banner, no toast and no repaint:
      // it read as a control that does not work.
      const policy = {
        id: 'pol-gone', scope: {providers: ['prov-a']}, schedule: null,
        concurrency: 2, requests: 0, request_window: '', tokens: 0, token_window: '',
        anti_burst: false, ramp_duration: '', source: 'ui', in_flight: 0, queued: 0,
      };
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'], throttles: [policy]});
      w.openOperatorConsole('limits');
      w.eval('ocOperatorErrorClear()');
      const stale = q('#oc-pane [data-oc-remove="limit"][data-oc-id="pol-gone"]');
      // Drop the rule without a render, exactly as a poll landing between the
      // paint and the click would.
      w.eval('throttleState.throttles = [];');
      stale.click();
      check('a click on a rule that is gone says so instead of doing nothing',
        await settleUntil(() => !!q('#oc-pane .oc-banner[role="alert"]')) &&
        text('#oc-pane .oc-banner[role="alert"] span') === 'that limit is no longer in force');
      w.eval('ocOperatorErrorClear()');
      w.closeOperatorConsole();
    }

    // ---- 14. live repaints keep the operator's place ----
    {
      const policy = {
        id: 'pol-focus', scope: {providers: ['prov-a']}, schedule: null,
        concurrency: 2, requests: 10, request_window: '1m', tokens: 0, token_window: '',
        anti_burst: false, ramp_duration: '', source: 'ui', in_flight: 0, queued: 0,
      };
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'], throttles: [policy]});
      w.openOperatorConsole('limits');
      click('#oc-pane [data-oc-edit="limit"][data-oc-id="pol-focus"]');
      const count = q('#oc-pane [data-oc-field="requests"]');
      count.focus();
      count.value = '37';
      count.dispatchEvent(new w.Event('input', {bubbles: true}));
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'],
        throttles: [{...policy, in_flight: 3, queued: 2}]});
      await settleUntil(() => q('#oc-pane [data-oc-field="requests"]')?.value === '37');
      // ocRender rebuilds the whole pane with innerHTML, so focus is restored,
      // not retained: the keyed slot resolves its own control, and a number
      // input has no caret to restore.
      check('a live occupancy sync keeps the typed request count in the draft and the field',
        w.eval('ocDrafts.limit.requests') === '37' &&
        q('#oc-pane [data-oc-field="requests"]').value === '37');
      {
        const field = q('#oc-pane [data-oc-field="requests"]');
        field.focus();
        field.value = '41';
        field.dispatchEvent(new w.Event('input', {bubbles: true}));
        w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a'],
          throttles: [{...policy, in_flight: 5, queued: 0}]});
        check('a live sync returns focus to the rebuilt budget field',
          await settleUntil(() => d.activeElement === q('#oc-pane [data-oc-field="requests"]')) &&
          q('#oc-pane [data-oc-field="requests"]').value === '41');
      }

      // The picker declares role="menu" with checkbox rows, so it must honour
      // the contract that role promises: one tab stop in the list, arrows and
      // Home/End between rows, and Escape that closes the picker rather than
      // the whole panel (which would lose the operator's place in the form).
      // A menu needs more than one row to show a cursor move, so the
      // vocabulary carries neighbours this block never picks.
      w.applyThrottleState({ok: true, active: true,
        known_providers: ['prov-a', 'prov-b', 'prov-c'], throttles: [policy]});
      w.operatorConsoleSync('throttle');
      {
        openCond('providers');
        const rows = qa('#oc-pane [data-oc-pick-value="providers"]');
        // The chip must announce the menu it opens. Without the pair a
        // screen reader said only "collapsed", so the arrow keys the picker
        // implements were undiscoverable from the control that opens it.
        const chip = q('#oc-pane [data-oc-pick="providers"]');
        const list = q('#oc-pane [role="menu"]');
        check('the dimension chip announces the menu it opens by name',
          chip.getAttribute('aria-haspopup') === 'menu' &&
          !!list && chip.getAttribute('aria-controls') === list.id &&
          !!list.id);
        check('a menu picker keeps one tab stop and removes the rest',
          rows.length > 1 && rows[0].tabIndex === 0 &&
          rows.slice(1).every(row => row.tabIndex === -1));
        // Opening the menu lands on its first row. A menu that leaves focus on
        // its trigger implements arrow keys nobody can reach without a
        // preliminary Tab.
        check('opening a picker moves focus into its first row',
          d.activeElement === rows[0]);
        rows[0].focus();
        // A real keypress targets the focused row, not the first one: after
        // ArrowDown the cursor has moved, so a helper pinned to rows[0] would
        // test a key nobody could press.
        const press = key => d.activeElement.dispatchEvent(new w.KeyboardEvent('keydown',
          {key, bubbles: true, cancelable: true}));
        press('ArrowDown');
        check('ArrowDown moves the picker cursor to the next row',
          d.activeElement === rows[1]);
        press('ArrowUp');
        check('ArrowUp moves the picker cursor back', d.activeElement === rows[0]);
        press('End');
        check('End moves the picker cursor to the last row',
          d.activeElement === rows[rows.length - 1]);
        press('Home');
        check('Home moves the picker cursor to the first row',
          d.activeElement === rows[0]);
        const before = w.eval('ocDrafts.limit.picker && ocDrafts.limit.picker.dim');
        press('Escape');
        check('Escape closes the open picker and leaves the console open',
          before === 'providers' && w.eval('ocDrafts.limit.picker') === null &&
          !shell.hidden &&
          d.activeElement === q('#oc-pane [data-oc-pick="providers"]'));
      }

      // A picker row is a transient choice inside a slot. Selecting one
      // rebuilds the pane, so focus must return to the picker that owns the
      // row instead of falling to <body> and losing the keyboard operator's
      // place. Scroll preservation is verified in the browser, because jsdom
      // keeps scrollTop across an innerHTML replacement on its own.
      {
        openCond('providers');
        const row = q('#oc-pane [data-oc-pick-value="providers"]');
        row.focus();
        row.click();
        check('a pick returns focus to the picker that owns the row',
          d.activeElement?.closest('[data-oc-key="pick-providers"]') !== null);
      }

      // A keyed TEXT control carries the caret contract that the rebuilt
      // pane can preserve: the picker's data-oc-key is on the input itself, so
      // ocRestoreFocus can refocus it and restore the selection. A long known
      // list is what shows the filter, so seed one.
      w.applyThrottleState({ok: true, active: true,
        known_providers: ['prov-a', 'prov-b', 'prov-c', 'prov-d', 'prov-e', 'prov-f', 'prov-g'],
        throttles: [policy]});
      openCond('providers');
      const filter = q('#oc-pane [data-oc-key="pick-filter-providers"]');
      filter.focus();
      filter.value = 'pro';
      filter.dispatchEvent(new w.Event('input', {bubbles: true}));
      const settledFilter = q('#oc-pane [data-oc-key="pick-filter-providers"]');
      settledFilter.focus();
      settledFilter.setSelectionRange(2, 2);
      // The list stays long: a short known list hides the filter by design.
      w.applyThrottleState({ok: true, active: true,
        known_providers: ['prov-a', 'prov-b', 'prov-c', 'prov-d', 'prov-e', 'prov-f', 'prov-g'],
        throttles: [{...policy, in_flight: 9, queued: 4}]});
      await settleUntil(() => d.activeElement?.dataset?.ocKey === 'pick-filter-providers');
      const restored = q('#oc-pane [data-oc-key="pick-filter-providers"]');
      check('a live sync returns focus to the keyed filter with its value and caret',
        d.activeElement === restored && restored.value === 'pro' &&
        restored.selectionStart === 2 && restored.selectionEnd === 2 &&
        w.eval('ocDrafts.limit.picker.filter') === 'pro');
      w.closeOperatorConsole();
      w.eval("ocDrafts.limit = null; ocView = {kind: '', id: ''}");
    }

    // ---- 15. record pills open the owning rule or a matching draft ----
    {
      const record = {id: 'rec-1', provider: 'prov-a', model: 'model-a',
        client: 'client-a', key_hash: 'abcdef123456', conversation_id: 'conv-1'};
      w.applyThrottleState({ok: true, active: true, known_providers: ['prov-a', 'prov-b'],
        throttles: [
          {id: 'pol-provider', scope: {providers: ['prov-a']}, concurrency: 2, source: 'ui'},
          {id: 'pol-specific', scope: {providers: ['prov-a'], models: ['model-a'], clients: ['client-a']},
            concurrency: 4, source: 'ui'},
        ]});
      w.ocOpenOperatorForRecord('limit', record);
      check('a matching limit record opens the most specific stored policy',
        paneSection() === 'limits' && w.eval('ocDrafts.limit.id') === 'pol-specific' &&
        q('#oc-pane .oc-compose[data-kind="limit"]').getAttribute('aria-label') === 'edit limit');
      w.closeOperatorConsole();
      w.applyThrottleState({ok: true, active: false, known_providers: ['prov-a'], throttles: []});
      w.ocOpenOperatorForRecord('limit', {provider: 'prov-z', model: 'model-z'});
      check('a limit record with no stored policy opens a draft scoped to it',
        paneSection() === 'limits' && w.eval('ocDrafts.limit.id') === '' &&
        JSON.stringify(evalJSON('ocDrafts.limit.scope.providers')) === JSON.stringify(['prov-z']) &&
        JSON.stringify(evalJSON('ocDrafts.limit.scope.models')) === JSON.stringify(['model-z']));

      w.applyPauseState({ok: true, paused: false, holds: [], queued: 0,
        known_clients: ['client-a'], known_providers: ['prov-a'],
        until: null, default_max_queued: 0});
      w.ocOpenOperatorForRecord('pause', record);
      check('a pause record with no hold opens a named draft scoped to it',
        paneSection() === 'pause' && w.eval('ocDrafts.pause.id') === '' &&
        JSON.stringify(evalJSON('ocDrafts.pause.scope.clients')) === JSON.stringify(['client-a']) &&
        JSON.stringify(evalJSON('ocDrafts.pause.scope.providers')) === JSON.stringify(['prov-a']) &&
        w.eval('ocDrafts.pause.mode') === 'named');
      w.closeOperatorConsole();
      w.applyPauseState({ok: true, paused: true,
        holds: [{id: 'hold-record', clients: ['client-a']}], queued: 0,
        known_clients: ['client-a'], known_providers: ['prov-a'],
        until: null, default_max_queued: 0});
      w.ocOpenOperatorForRecord('pause', record);
      check('a pause record under a matching hold opens that hold for editing',
        paneSection() === 'pause' && w.eval('ocDrafts.pause.id') === 'hold-record');
      w.closeOperatorConsole();

      w.applyDebugState(debugDoc({ok: true, enabled: true,
        sessions: [{id: 'sess-match', clients: ['client-a']},
          {id: 'sess-stamped', providers: ['prov-a']}],
        known_clients: [], known_providers: [], known_models: [],
        ttl: '24h', max_bytes: '1MiB'}));
      w.ocOpenOperatorForRecord('capture', {...record, debug_session_id: 'sess-stamped'});
      check('a capture record prefers the session stamped on the record',
        paneSection() === 'capture' && w.eval('ocDrafts.capture.id') === 'sess-stamped');
      w.closeOperatorConsole();
      w.applyDebugState(emptyDebug());
      w.ocOpenOperatorForRecord('capture', record);
      check('a capture record with no session opens a draft scoped to it',
        paneSection() === 'capture' && w.eval('ocDrafts.capture.id') === '' &&
        JSON.stringify(evalJSON('ocDrafts.capture.scope.clients')) === JSON.stringify(['client-a']) &&
        JSON.stringify(evalJSON('ocDrafts.capture.scope.models')) === JSON.stringify(['model-a']));

      check('a limit rule matches only records inside its client and model scope',
        w.ocPolicyMatchesRecord({scope: {clients: ['client-a'], models: ['model-a']}},
          {client: 'client-a', model: 'model-a'}) === true &&
        w.ocPolicyMatchesRecord({scope: {clients: ['client-a'], models: ['model-a']}},
          {client: 'client-b', model: 'model-a'}) === false);
      w.closeOperatorConsole();
    }

    // ---- 16. the one empty-vocabulary sentence ----
    {
      const note = w.eval('NONE_YET_NOTE');
      throttleDoc({ok: true, active: false, throttles: [], known_providers: []});
      w.openOperatorConsole('limits');
      startNew('limit');
      openCond('providers');
      check('an empty provider vocabulary renders the none-yet sentence',
        text('#oc-pane .oc-pick-empty') === note &&
        note === 'none yet - they appear as requests arrive' &&
        qa('#oc-pane [data-oc-pick-value="providers"]').length === 0);
      // The rule that an empty dimension matches everything was stated only
      // in the pause composer's prose, which is invisible while the picker is
      // open. A close-with-nothing-checked must not read as a narrowing.
      check('an open picker says that an empty dimension narrows nothing',
        q('#oc-pane .oc-pick-note')?.textContent.includes('does not narrow this rule'));
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a']});
      w.operatorConsoleSync('throttle');
      if (q('#oc-pane [data-oc-pick="providers"]').getAttribute('aria-expanded') !== 'true') {
        click('#oc-pane [data-oc-pick="providers"]');
      }
      check('a populated provider vocabulary renders rows instead of the note',
        !q('#oc-pane .oc-pick-empty') &&
        JSON.stringify(qa('#oc-pane [data-oc-pick-value="providers"]')
          .map(row => row.dataset.ocValue)) === JSON.stringify(['prov-a']));
      w.closeOperatorConsole();
    }

    // ---- 17. the shared console owners are one owner each ----
    {
      // ocControlSignature names an unkeyed control by what it is, not where
      // it sits. If two distinct controls could share a signature, focus
      // restoration would land on the wrong one; the index disambiguates only
      // genuinely identical controls (seven day chips, two rules' copy).
      // w.eval takes one expression string, so the element is resolved inside
      // the page rather than passed across.
      const sigAt = (selector, index = 0) => w.eval(
        'ocControlSignature(document.querySelectorAll(' + JSON.stringify(selector) + ')[' + index + '])');
      throttleDoc({ok: true, active: false, throttles: [], known_providers: ['prov-a']});
      w.openOperatorConsole('limits');
      startNew('limit');
      check('two budget checkboxes of one composer do not share a signature',
        qa('#oc-pane [data-oc-budget]').length >= 2 &&
        sigAt('#oc-pane [data-oc-budget]', 0) !== sigAt('#oc-pane [data-oc-budget]', 1));
      check('two disclosures of one composer do not share a signature',
        sigAt('#oc-pane [data-oc-toggle="advanced"]') !==
        sigAt('#oc-pane [data-oc-toggle="schedule"]'));
      // The rebuild path must resolve the same signature after the pane is
      // replaced, which is what lets an unkeyed control keep focus.
      const before = sigAt('#oc-pane [data-oc-budget="requests"]');
      w.operatorConsoleSync('throttle');
      check('a control signature is stable across a pane rebuild',
        before !== '' && before === sigAt('#oc-pane [data-oc-budget="requests"]'));
      check('a control signature omits the row key it is stamped beside',
        sigAt('#oc-pane [data-oc-apply="limit"]') !== '' &&
        !sigAt('#oc-pane [data-oc-apply="limit"]').includes('data-oc-key'));
      // ocRailBadgeCount is the one reading of a destination's count, so the
      // badge and the section cannot disagree about which kinds carry one.
      check('only the three rule sections carry a rail count',
        evalJSON('["limits","pause","capture"].map(s => ocRailBadgeCount(s, {limits: 2, holds: 3, captures: 4}))')
          .join() === '2,3,4' &&
        evalJSON('["overview","export","system"].map(s => ocRailBadgeCount(s, {limits: 2, holds: 3, captures: 4}))')
          .join() === '0,0,0');
      // ocComposeControls reads the same table the markup is emitted from: an
      // id that only one of the two knew about would detach the preview from
      // its commit button.
      check('the composer table names three kinds and both nodes resolve',
        evalJSON('Object.keys(OC_COMPOSERS).join()') === 'limit,pause,capture' &&
        w.eval('Object.values(OC_COMPOSERS).every(c => c.preview && c.apply && c.state && c.key)') &&
        evalJSON('!!ocComposeControls("limit")') === true);
      // OC_CHOICES is the one table of closed-vocabulary controls: every row
      // carries the name the control renders and the write it performs, so the
      // markup and the dispatcher cannot disagree about either. The twelve
      // names used to be spelled twice, and a rename in one half left a chip
      // that toggled visually and changed no state.
      check('one table owns every choice name and its write',
        evalJSON('Object.values(OC_CHOICES).length') === 12 &&
        w.eval('Object.values(OC_CHOICES).every(c => c.name && c.kind && (c.kind === "overview" || typeof c.set === "function"))') &&
        evalJSON('Object.keys(OC_CHOICE_BY_NAME).length') === 12 &&
        w.eval('Object.values(OC_CHOICES).every(c => OC_CHOICE_BY_NAME[c.name] === c)') &&
        // A name the table does not own is a no-op, never a write into
        // whichever draft happens to be open.
        (() => {
          const before = JSON.stringify(w.eval('ocDrafts.limit'));
          w.eval('ocHandleChoice("not-a-choice", "x")');
          return JSON.stringify(w.eval('ocDrafts.limit')) === before;
        })());
      // Every rendered choice control is one the table names, in every state
      // that reveals one: the limit and pause windows only exist with a draft
      // open, four export filters only in filter mode, and the drain scope only
      // while something is parked. Both directions are asserted, so a control
      // that renders without a row and a row nothing ever renders are both
      // failures.
      {
        const registered = new Set(w.eval('Object.values(OC_CHOICES).map(c => c.name)'));
        const rendered = new Set();
        const collect = () => {
          for (const el of d.querySelectorAll('#oc-pane [data-oc-choice]')) rendered.add(el.dataset.ocChoice);
        };
        w.openOperatorConsole('limits');
        startNew('limit');
        // The ramp window lives inside the burst-control disclosure, so the
        // draft alone does not reveal it.
        w.eval("ocDrafts.limit.advanced = true; ocRender()");
        collect();
        w.openOperatorConsole('pause');
        startNew('pause');
        collect();
        w.openOperatorConsole('capture');
        startNew('capture');
        collect();
        w.openOperatorConsole('export');
        w.eval("ocDrafts.export.mode = 'filter'; ocDrafts.export.provider = 'p'; ocRender()");
        collect();
        w.applyPauseState({ok: true, paused: false, holds: [], clients: [], providers: [],
          known_clients: [], known_providers: [], until: null, queued: 3,
          storm_waiters: 0, default_max_queued: 0});
        w.openOperatorConsole('overview');
        collect();
        w.applyPauseState({ok: true, paused: false, holds: [], clients: [], providers: [],
          known_clients: [], known_providers: [], until: null, queued: 0,
          storm_waiters: 0, default_max_queued: 0});
        w.eval("ocDrafts.export = null; ocDrafts.limit = null; ocDrafts.pause = null; ocDrafts.capture = null; ocRender()");
        check('every rendered choice control is registered and every registered name renders',
          [...rendered].every(name => registered.has(name)) &&
          [...registered].every(name => rendered.has(name)));
        w.openOperatorConsole('limits');
      }
      // OC_ACTION_HANDLERS is the one registry of delegated click actions: the
      // listener's selector is built from it, so an action cannot be
      // dispatched without being selected, or the reverse. The console carried
      // those names in two lists and adding one meant editing both.
      check('one registry owns both the action selector and the dispatch order',
        w.eval('OC_ACTION_SELECTOR') === w.eval('OC_ACTION_HANDLERS.map(([a]) => "[" + a + "]").join(",")') &&
        evalJSON('OC_ACTION_HANDLERS.length') >= 33 &&
        w.eval('OC_ACTION_HANDLERS.every(([a, fn]) => a.startsWith("data-oc-") && typeof fn === "function")') &&
        evalJSON('new Set(OC_ACTION_HANDLERS.map(([a]) => a)).size') ===
          evalJSON('OC_ACTION_HANDLERS.length'));
      // Every delegated control carries exactly one action attribute, which is
      // what lets the dispatcher take the first match instead of improvising
      // precedence between two actions on one element.
      w.openOperatorConsole('limits');
      startNew('limit');
      {
        const actionAttrs = w.eval('OC_ACTION_HANDLERS.map(([a]) => a)');
        const offenders = [];
        for (const el of d.querySelectorAll('#oc-pane button, #oc-pane a')) {
          const hits = actionAttrs.filter(attr => el.hasAttribute(attr));
          if (hits.length > 1) offenders.push(el.textContent.trim().slice(0, 20) + ': ' + hits.join('+'));
        }
        check('no delegated control carries two action attributes',
          offenders.length === 0);
      }
      w.closeOperatorConsole();
      // ocParking is the one answer to "is traffic being held": a hold parks,
      // and so does the gate, which is why the readout tone cannot be derived
      // from the hold count alone.
      const parking = doc => {
        w.applyPauseState({...EMPTY_PAUSE, ...doc});
        return evalJSON('ocParking()');
      };
      check('parking is true for a hold, for a gate waiter, and false for neither',
        parking({paused: true, holds: [{id: 'p1', all: true}]}) === true &&
        parking({storm_waiters: 2}) === true &&
        parking({}) === false);
      // OC_SCOPE_DIMS is the one table of selector dimensions. Every derived
      // list and every spelling of a dimension must come from it, or a rename
      // reaches one reader and silently misses the wire name or the record
      // field.
      check('the scope table owns every spelling of every dimension',
        JSON.stringify(w.eval('OC_SCOPE_DIMS.map(d => d.key)')) === JSON.stringify(w.eval('OC_SCOPE_KEYS')) &&
        // The capture subset is derived in table order, not re-listed in the
        // order the old composer happened to write it.
        JSON.stringify(w.eval('OC_CAPTURE_SCOPE_KEYS')) === JSON.stringify(['providers', 'models', 'clients']) &&
        w.eval('OC_SCOPE_DIMS.every(d => d.key && d.wire && d.one && d.many && d.record)') &&
        w.eval('OC_SCOPE_DIMS.every(d => ocDimLabel(d.key) === d.one)') &&
        // The wire name is what the server document calls the dimension, and
        // the composer bodies must send exactly that.
        w.eval('OC_SCOPE_DIMS.every(d => Object.prototype.hasOwnProperty.call(ocScopeToWire(ocNewScope()), d.wire))'));
      // The phrase reads the table: a single value takes the singular word, a
      // key is shortened, and several values take the count and the plural.
      check('the scope phrase reads its words and shortening from the table',
        w.eval('ocScopePhrase({providers: ["p"], models: ["a", "b"], keyHashes: ["abcdef1234567"]})') ===
          'provider p · 2 models · key abcdef12…' &&
        w.eval('ocScopePhrase(ocNewScope())') === 'every request' &&
        w.eval('ocScopePhrase(null)') === 'every request');
      // A stored policy nests its selector while a hold carries the dimensions
      // flat; the reader accepts both so no caller wraps a hold by hand.
      check('the scope reader accepts a nested selector and a flat document',
        JSON.stringify(evalJSON('ocScopeFromPolicy({scope: {providers: ["n"], key_hashes: ["k"]}})')) ===
          JSON.stringify({providers: ['n'], models: [], clients: [], keyHashes: ['k'], conversations: []}) &&
        JSON.stringify(evalJSON('ocScopeFromPolicy({providers: ["f"]})')) ===
          JSON.stringify({providers: ['f'], models: [], clients: [], keyHashes: [], conversations: []}));
      // A schedule is one phrase with three shapes; the day letters come from
      // the same weekday table the chips use, so two chosen days join into one
      // run of letters rather than a spaced list.
      check('the schedule phrase names days, hours and the all-day case',
        w.eval('ocSchedulePhrase({days: [1], start_minute: 0, end_minute: 0})') === 'M · all day' &&
        w.eval('ocSchedulePhrase({days: [], start_minute: 540, end_minute: 1020})') === 'every day · 09:00–17:00' &&
        w.eval('ocSchedulePhrase({days: [0, 6], start: 60, end: 120})') === 'SS · 01:00–02:00' &&
        // A stored document names its minutes and a draft names its hours; the
        // reader accepts both spellings of the same window.
        w.eval('ocSchedulePhrase({days: [1], start: 540, end: 1020})') ===
          w.eval('ocSchedulePhrase({days: [1], start_minute: 540, end_minute: 1020})') &&
        // The weekday table is shared with the log dividers, so a rename
        // reaches the schedule chips and the day stamps together.
        evalJSON('CAL_DAYS.map(d => d.name).join()') ===
          'Sunday,Monday,Tuesday,Wednesday,Thursday,Friday,Saturday' &&
        evalJSON('CAL_DAYS.map(d => d.short).join()') ===
          'SUN,MON,TUE,WED,THU,FRI,SAT' &&
        evalJSON('CAL_DAYS.map(d => d.letter).join()') === 'S,M,T,W,T,F,S');
      // The empty frame has one shape, whatever the surface's words are. Its
      // two parts are trusted differently: the lead is escaped here, while the
      // body is caller markup that may embed a real control (the filter's
      // no-match note carries its clear action), so a body is asserted as
      // written rather than escaped.
      check('the empty frame wraps an escaped lead and a caller-built body',
        w.eval('ocEmptyHTML("No holds.", "Nothing is parked.")') ===
          '<div class="oc-empty"><strong>No holds.</strong> Nothing is parked.</div>' &&
        w.eval('ocEmptyHTML("<b>x</b>", "a &amp; b")') ===
          '<div class="oc-empty"><strong>&lt;b&gt;x&lt;/b&gt;</strong> a &amp; b' + '</div>');
      // Every selector dimension the export filter offers is one the scope
      // table owns, so its known-values list and its record field come from the
      // row rather than from a per-dimension branch. Status is the one derived
      // dimension and stays local.
      check('the export filter reads each selector dimension through its scope row',
        evalJSON('OC_FILTER_DIMS.filter(d => d !== "status").every(d => !!ocScopeDim(d))') === true &&
        // Each filter name resolves to the scope row that matches it, so the
        // record field it reads is that row's, never a parallel literal.
        JSON.stringify(evalJSON('OC_FILTER_DIMS.filter(d => d !== "status").map(d => ocScopeDim(d).record)')) ===
          JSON.stringify(['provider', 'model', 'client', 'conversation_id']) &&
        w.eval('ocScopeDim("status")') === null &&
        evalJSON('ocScopeDim("provider") === ocScopeDim("providers")') === true &&
        evalJSON('ocScopeDim("conversation") === ocScopeDim("conversation_id")') === true);
      // Count agreement is one rule for every surface that states a count.
      check('count agreement is one rule, not one ternary per phrase',
        w.eval('agree(1, "rule is", "rules are")') === 'rule is' &&
        w.eval('agree(2, "rule is", "rules are")') === 'rules are' &&
        w.eval('agree(0, "rule is", "rules are")') === 'rules are' &&
        w.eval('countPhrase(1, "record")') === '1 record' &&
        w.eval('countPhrase(7, "record")') === '7 records');
      // The export's window choices are the delete filter's tokens with their
      // own wording: an exclusion reads "older than", an inclusion "last".
      check('the export window choices derive from the history token list',
        JSON.stringify(w.eval('OC_AGE_CHOICES.map(c => c[0])')) ===
          JSON.stringify(w.eval('FILTER_AGE_TOKENS')) &&
        JSON.stringify(w.eval('OC_AFTER_CHOICES.map(c => c[0])')) ===
          JSON.stringify(w.eval('FILTER_AGE_TOKENS')) &&
        w.eval('OC_AGE_CHOICES[3][1]') === 'older than 1 week' &&
        w.eval('OC_AFTER_CHOICES[3][1]') === 'last week');
      // ocRecordChange is also the one announcement owner. The footer ledger
      // and the toast are both polite live regions, so a caller that recorded
      // a change and then toasted the same phrase spoke it twice. The ledger
      // is audible while the panel is open; the toast covers the closed panel.
      w.closeOperatorConsole();
      const phrase = 'announcement probe';
      w.eval('ocRecordChange(' + JSON.stringify(phrase) + ', null, "ok")');
      check('a change recorded with the console closed reaches the toast',
        text('#toast') === phrase);
      w.openOperatorConsole('overview');
      const openPhrase = 'announcement probe open';
      const toastBefore = text('#toast');
      w.eval('ocRecordChange(' + JSON.stringify(openPhrase) + ', null, "ok")');
      check('a change recorded with the console open is not spoken twice',
        text('#oc-change') === openPhrase && text('#toast') === toastBefore);
      // The rule is not just "the owner toasts": a caller must never toast its
      // own phrase either, or the two channels speak together again the moment
      // the owner's guard decides the ledger is audible. A source scan over the
      // console modules is the only assertion that covers callers the tests do
      // not drive, and it names every one this suite does.
      {
        const {readFileSync, readdirSync} = require('fs');
        const {join: joinPath} = require('path');
        const dir = joinPath(__dirname, '..', '..', '..', 'internal', 'web', 'static', 'js');
        const sources = readdirSync(dir).filter(n => /^operator_.*\.js$/.test(n))
          .map(n => [n, readFileSync(joinPath(dir, n), 'utf8')]);
        const offenders = [];
        for (const [name, src] of sources) {
          const code = src.replace(/\/\/.*$/gm, '');
          // Every ocRecordChange( call: walk its argument list, then look at the
          // statement that follows for a toast of the same phrase.
          let at = code.indexOf('ocRecordChange(');
          while (at >= 0) {
            let depth = 0, i = at + 'ocRecordChange'.length;
            for (; i < code.length; i++) {
              if (code[i] === '(') depth++;
              else if (code[i] === ')') { depth--; if (depth === 0) break; }
            }
            const after = code.slice(i + 1, i + 220);
            if (/^[\s;]*toast\(/.test(after)) offenders.push(name);
            at = code.indexOf('ocRecordChange(', at + 1);
          }
        }
        check('no console caller toasts the phrase its change record already announces',
          sources.length >= 4 && offenders.length === 0);
      }
      // The ledger is a polite live region that repaints on every accepted
      // operator poll and every console render. An unconditional assignment
      // replaces the text node, so a console left open would re-announce the
      // last change continuously. Node identity proves the guard: the same
      // words keep the node, different words replace it.
      {
        const foot = d.getElementById('oc-change');
        const node = foot.firstChild;
        w.eval('ocRender()');
        w.eval('ocRender()');
        const kept = foot.firstChild === node;
        w.eval('ocRecordChange("a later change", null, "ok")');
        w.eval('ocRender()');
        check('the console ledger speaks only when its words change',
          kept && foot.textContent === 'a later change' && foot.firstChild !== node);
      }
      w.closeOperatorConsole();
    }
  } finally {
    // Leave the shared page on the empty baseline every later section
    // expects: no holds, sessions or limits, the console closed, blank
    // drafts and the production globals restored. A confirmation left open
    // by a thrown assertion would keep the modal layer (and its focus trap)
    // in force for the next section, so it is settled as a cancel first.
    const leftover = d.getElementById('oc-confirm-dialog');
    if (leftover) {
      leftover.querySelector('[data-oc-confirm="cancel"]')?.click();
      if (leftover.isConnected) leftover.remove();
    }
    w.closeOperatorConsole();
    w.applyPauseState(EMPTY_PAUSE);
    w.applyDebugState(emptyDebug());
    w.applyThrottleState(EMPTY_THROTTLE);
    w.eval("ocDrafts.limit = null; ocDrafts.pause = null; ocDrafts.capture = null; " +
      "ocDrafts.export = null; ocError = ''; ocView = {kind: '', id: ''}; " +
      "ocDrain = {scope: 'all', value: ''}; ocChanges.length = 0; ocRenderFoot();");
    w.eval("ocSection = 'overview'; try { sessionStorage.setItem('millivolt.operatorSection', 'overview'); } catch (e) {}");
    w.eval('restartServerState = JSON.parse(' + JSON.stringify(savedRestartState) + '); syncRestartControls()');
    w.eval('storageState = JSON.parse(' + JSON.stringify(savedStorage) + ')');
    if (savedRecords) {
      w.__ocSavedRecords = savedRecords;
      w.eval('lastData.records = window.__ocSavedRecords');
      delete w.__ocSavedRecords;
    }
    delete w.__ocRecordFixture;
    delete w.__ocExportRecords;
    w.refreshOperatorReadout();
  }
};
