// jsdom functional test for the dashboard's incremental live-update pipeline.
// Most checks drive the page through its external interfaces: the stubbed
// fetch() and the EventSource listeners the page registers itself. Focused
// helper sections call production render/merge helpers directly to pin
// optimizer contracts that the external paths cannot isolate; they supplement,
// not replace, the end-to-end coverage.
//
// The shared page, fixtures and stub state live in tests/ui_harness/; the
// ordered section modules under tests/ui_harness/sections/ run in registry
// order. This file stays the single entry point (`npm test`).
const harness = require('./ui_harness/harness');
const sections = require('./ui_harness/sections');
const {
  dom, w, d, html, check, reportFailures, rows, fire, sleep, settleUntil,
  installDiagnostics, checkEntityIdentity, assembleHTML, dashboardDOM, loaded, JSDOM, fs,
  path, STATIC, mkRec, LIST, BIG, CHART_FROM, BUCKET_MS, mkChart, modelFixture,
  observerFixture, DEF_RULES, cfgDoc, TEST_DASHBOARD_VERSION, pageOptions, state
} = harness;

async function main() {
  // Deferred asset scripts load through the interceptor after parsing. Wait
  // for the load event before inspecting styles or driving the dashboard.
  await loaded(dom);
  installDiagnostics();
  checkEntityIdentity();
  // Shared CSS must preserve sentence case, including protocol acronyms and
  // user-authored names. Inspect every formerly forced-uppercase owner.
  {
    // jsdom does not apply viewport media queries; inspect those declarations
    // as well so the shared mobile picker cannot force uppercase unnoticed.
    const declarations = [];
    const visitRules = rules => {
      for (const rule of rules) {
        if (rule.style) declarations.push(rule.style.getPropertyValue('text-transform'));
        if (rule.cssRules) visitRules(rule.cssRules);
      }
    };
    for (const sheet of d.styleSheets) visitRules(sheet.cssRules);
    check('dashboard labels never force uppercase, including mobile media rules',
      !declarations.some(value => value === 'uppercase' || value === 'capitalize'));
    const host = d.createElement('section');
    host.innerHTML = '<div class="pause-known-hd"></div><div class="lim-src"></div>' +
      '<div class="clear-opts"><label></label></div><div class="kpi"><h2></h2></div>' +
      '<span class="xp-dim-by"></span><div class="prov-lb"></div><div class="mr-what"></div>' +
      '<div class="mr-ex-hd"></div><div class="card"><h2></h2></div><div class="ct-h"></div>' +
      '<table><thead><tr><th></th></tr></thead></table><span class="day-chip"></span>' +
      '<div class="detail-section"><h4></h4></div>';
    d.body.appendChild(host);
    try {
      for (const selector of ['.pause-known-hd', '.lim-src', '.clear-opts label', '.kpi h2',
        '.xp-dim-by', '.prov-lb', '.mr-what', '.mr-ex-hd', '.card h2', '.ct-h', 'th',
        '.day-chip', '.detail-section h4']) {
        const label = host.querySelector(selector);
        label.textContent = 'Latency TTFT for Model.MixedCase';
        check(`${selector} preserves sentence case and exact labels`,
          w.getComputedStyle(label).textTransform === 'none' &&
          label.textContent === 'Latency TTFT for Model.MixedCase');
      }
    } finally { host.remove(); }
  }
  await sleep(200); // initial fetch + renderAll

  for (const run of sections) await run(harness);

  // ---- NONE_YET_NOTE: the operator menus' one empty-state sentence ----
  // Both directions are pinned: empty known lists render the note (the
  // pause/debug checklists paint it as .pause-none; the limits provider
  // select carries it as its empty option), and populated lists render
  // their rows without it anywhere - an empty surface silently painting
  // nothing, or a populated one leaking the note, are both drift. This
  // also repairs the W14 record: that commit claimed this pin existed.
  {
    const noteText = 'none yet - they appear as requests arrive';
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: [], known_providers: []});
    w.resetPauseMenuForm();
    w.eval("debugState = {...debugState, sessions: [], known_clients: [], known_providers: [], known_models: []}");
    w.resetDebugMenuForm();
    w.applyThrottleState({ok: true, active: false, throttles: [], known_providers: []});
    w.syncLimitsMenuState();
    check('empty known lists render the none-yet note in the pause and debug checklists',
      ['#pf-clients', '#pf-providers', '#df-clients', '#df-providers', '#df-models']
        .every(id => d.querySelector(id + ' .pause-none')?.textContent === noteText));
    check('the empty limits provider select carries the none-yet note as its only option',
      d.getElementById('lim-provider').options.length === 1 &&
      d.getElementById('lim-provider').options[0].value === '' &&
      d.getElementById('lim-provider').options[0].textContent === noteText);
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: ['client-a'], known_providers: ['prov-a']});
    w.resetPauseMenuForm();
    w.eval("debugState = {...debugState, known_clients: ['client-a'], known_providers: ['prov-a'], known_models: ['glm-5.3']}");
    w.resetDebugMenuForm();
    w.applyThrottleState({ok: true, active: false, throttles: [], known_providers: ['prov-a']});
    w.syncLimitsMenuState();
    check('populated known lists render their rows, never the none-yet note',
      !d.querySelector('#pause-menu .pause-none') && !d.querySelector('#debug-menu .pause-none') &&
      !!d.querySelector('#pf-clients input[value="client-a"]') &&
      !!d.querySelector('#pf-providers input[value="prov-a"]') &&
      !!d.querySelector('#df-models input[value="glm-5.3"]') &&
      [...d.getElementById('lim-provider').options].every(o => o.value === 'prov-a'));
    // Leave the surfaces on the empty baseline the rest of the suite expects.
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: [], known_providers: []});
    w.resetPauseMenuForm();
    w.eval("debugState = {...debugState, known_clients: [], known_providers: [], known_models: []}");
    w.resetDebugMenuForm();
    w.applyThrottleState({ok: true, active: false, throttles: [], known_providers: []});
    w.syncLimitsMenuState();
  }

  // ---- GAP-6 pin: the pause/debug status-bits wording on menu AND footer.
  // jsdom only ever asserted menu counts for error/busy strings; the
  // composition itself ('queued N', the until countdown) was unpinned on
  // both surfaces. One fixture drives each builder through the real apply
  // path: the menu count line and the footer state line must compose the
  // identical bits string, with the footer adding only its documented
  // scope insertion (holdScopeLabel after 'paused · ').
  {
    const until = new Date(Date.now() + 90 * 60000).toISOString();
    d.getElementById('pause-menu').hidden = false;
    w.applyPauseState({ok: true, paused: true, queued: 3, until,
      holds: [{id: 'bits-hold', all: true, until}],
      known_clients: [], known_providers: []});
    const pauseMenuBits = d.getElementById('pause-count').textContent;
    const pauseFooterBits = d.getElementById('f-state').textContent;
    check('the pause menu count composes the paused/queued/until wording',
      pauseMenuBits === 'paused · queued 3 · 1h 30m left');
    check('the pause footer carries the identical bits around its scope insertion',
      pauseFooterBits === 'paused · all requests · queued 3 · 1h 30m left' &&
      pauseFooterBits === pauseMenuBits.replace('paused · ', 'paused · all requests · '));
    d.getElementById('pause-menu').hidden = true;
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: [], known_providers: []});
    w.resetPauseMenuForm();

    d.getElementById('debug-menu').hidden = false;
    w.applyDebugState(observerFixture({ok: true, enabled: true, until,
      sessions: [{id: 'bits-session', clients: ['client-a'], duration: '15m'}],
      known_clients: [], known_providers: [], known_models: [],
      ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture()}));
    const debugBits = 'debug · client-a · 1h 30m left';
    check('menu and footer compose the identical debug bits string',
      d.getElementById('debug-count').textContent === debugBits &&
      d.getElementById('f-state').textContent === debugBits);
    w.applyDebugState(observerFixture({ok: true, enabled: true, until,
      sessions: [{id: 'bits-session', clients: ['client-a'], duration: '15m'},
        {id: 'bits-session-2', providers: ['prov-a'], duration: '1h'}],
      known_clients: [], known_providers: [], known_models: [],
      ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture()}));
    const debugMultiBits = 'debug · 2 sessions · 1h 30m left';
    check('the multi-session debug count stays one wording on both surfaces',
      d.getElementById('debug-count').textContent === debugMultiBits &&
      d.getElementById('f-state').textContent === debugMultiBits);
    d.getElementById('debug-menu').hidden = true;
    w.applyDebugState(observerFixture({ok: true, enabled: false, sessions: [],
      known_clients: [], known_providers: [], known_models: [],
      ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture()}));
    w.resetDebugMenuForm();
  }

  // ---- operator poll stability: pause/debug/limits lists update in place.
  // Bootstrap polls include live occupancy (pause queued, debug ran_ms and
  // captures, limits in-flight/queued/remaining). Those are display-only
  // changes, so the renderers must keep the existing nodes, focus, checked
  // selections and scroll position instead of rebuilding each list. One
  // structural test below pins the exact replacement path too.
  {
    const debugFixture = (sessions, known = {}) => observerFixture({
      ok: true, enabled: sessions.length > 0, sessions,
      known_clients: known.clients || [], known_providers: known.providers || [],
      known_models: known.models || [], until: null, ttl: '24h', max_bytes: '1MiB',
      model_canon: modelFixture(),
    });
    const pauseFixture = (holds, queued, known = {}) => ({
      ok: true, paused: holds.length > 0, holds, queued,
      clients: [], providers: [],
      known_clients: known.clients || [], known_providers: known.providers || [],
      until: null, default_max_queued: 0,
    });
    const limitFixture = (counts, providers = ['provider-a']) => ({
      ok: true, active: true, known_providers: providers,
      throttles: [{
        provider: 'provider-a', source: 'ui', concurrency: 2,
        requests: 10, request_window: '1m', tokens: 0, token_window: '',
        in_flight: counts[0], queued: counts[1], requests_remaining: counts[2],
        requests_capacity: 20, tokens_remaining: 0, tokens_capacity: 0,
      }],
    });

    // Debug: an elapsed/capture-only poll must not replace the focused model
    // checkbox or the active-session row; the row text still advances.
    d.getElementById('debug-menu').hidden = false;
    w.applyDebugState(debugFixture(
      [{ id: 'flow-dbg', clients: ['client-a'], duration: '15m', ran_ms: 60000, captures: 2 }],
      { clients: ['client-a', 'client-b'], models: ['model-a', 'model-b'] }));
    const debugModel = d.querySelector('#df-models input[value="model-a"]');
    debugModel.focus();
    debugModel.checked = true;
    const debugHold = d.querySelector('#df-holds .pause-hold');
    const debugDur = d.getElementById('df-dur');
    const debugDurOption = debugDur.options[0];
    w.applyDebugState(debugFixture(
      [{ id: 'flow-dbg', clients: ['client-a'], duration: '15m', ran_ms: 180000, captures: 9 }],
      { clients: ['client-a', 'client-b'], models: ['model-a', 'model-b'] }));
    check('debug elapsed/capture polling keeps the focused model checkbox and session row in place',
      d.activeElement === debugModel && debugModel.isConnected && debugModel.checked &&
      d.querySelector('#df-models input[value="model-a"]') === debugModel &&
      d.querySelector('#df-holds .pause-hold') === debugHold &&
      debugHold.textContent.includes('ran 3m') && debugHold.textContent.includes('9 req'));
    check('debug duration options are not rebuilt by an elapsed-only poll',
      d.getElementById('df-dur') === debugDur && debugDur.options[0] === debugDurOption);

    // Expiry: a timed session expiring between polls changes the taken-name
    // vocabulary without changing any raw session field. Liveness therefore
    // belongs in the structural gate, not only in the row renderer.
    {
      const OriginalDate = w.Date;
      let fakeNow = OriginalDate.now();
      class FakeDate extends OriginalDate {
        constructor(...args) { super(...(args.length ? args : [fakeNow])); }
        static now() { return fakeNow; }
      }
      const until = new OriginalDate(fakeNow + 60000).toISOString();
      try {
        w.Date = FakeDate;
        w.applyDebugState(debugFixture(
          [{ id: 'expiry-dbg', models: ['model-a'], until, ran_ms: 1000, captures: 0 }],
          { models: ['model-a', 'model-b'] }));
        w.resetDebugMenuForm();
        const takenModel = d.querySelector('#df-models input[value="model-a"]');
        const keptModel = d.querySelector('#df-models input[value="model-b"]');
        check('a live exclusive debug model scope disables its model row',
          !!takenModel && takenModel.disabled && takenModel.closest('label').classList.contains('taken'));
        keptModel.focus();
        keptModel.checked = true;
        fakeNow += 120000;
        w.applyDebugState(debugFixture(
          [{ id: 'expiry-dbg', models: ['model-a'], until, ran_ms: 2000, captures: 3 }],
          { models: ['model-a', 'model-b'] }));
        check('an expired debug session runs the full sync and re-enables its exclusive model scope',
          d.querySelector('#df-models input[value="model-a"]') === takenModel &&
          !takenModel.disabled && !takenModel.closest('label').classList.contains('taken') &&
          d.querySelector('#df-models input[value="model-b"]') === keptModel &&
          d.activeElement === keptModel && keptModel.checked &&
          d.getElementById('df-holds').textContent === '');
      } finally {
        w.Date = OriginalDate;
        w.applyDebugState(debugFixture([], { models: ['model-a', 'model-b'] }));
      }
    }

    // Pause: queued occupancy must repaint the count and hold text without
    // replacing the focused client checkbox or the hold row.
    d.getElementById('pause-menu').hidden = false;
    w.applyPauseState(pauseFixture(
      [{ id: 'flow-hold', all: true, duration: '15m', queued: 1 }], 1,
      { clients: ['client-a', 'client-b'] }));
    const pauseClient = d.querySelector('#pf-clients input[value="client-a"]');
    pauseClient.focus();
    pauseClient.checked = true;
    const pauseHold = d.querySelector('#pf-holds .pause-hold');
    const pauseDur = d.getElementById('pf-dur');
    const pauseDurOption = pauseDur.options[0];
    w.applyPauseState(pauseFixture(
      [{ id: 'flow-hold', all: true, duration: '15m', queued: 6 }], 7,
      { clients: ['client-a', 'client-b'] }));
    check('pause occupancy polling keeps the focused checklist selection and hold row in place',
      d.activeElement === pauseClient && pauseClient.isConnected && pauseClient.checked &&
      d.querySelector('#pf-clients input[value="client-a"]') === pauseClient &&
      d.querySelector('#pf-holds .pause-hold') === pauseHold &&
      pauseHold.textContent.includes('q 6') &&
      d.getElementById('pause-count').textContent === 'paused · queued 7');
    check('pause duration options are not rebuilt by an occupancy-only poll',
      d.getElementById('pf-dur') === pauseDur && pauseDur.options[0] === pauseDurOption);

    // Limits: in-flight/queued/remaining are display-only. The provider
    // select and the focused hold row must survive the poll.
    d.getElementById('limits-menu').hidden = false;
    w.applyThrottleState(limitFixture([1, 0, 19]));
    const limitProvider = d.getElementById('lim-provider');
    const limitProviderOption = limitProvider.options[0];
    const limitHold = d.querySelector('#lim-holds .pause-hold');
    const limitButton = limitHold.querySelector('[data-operator="limit-edit"]');
    limitButton.focus();
    w.applyThrottleState(limitFixture([4, 3, 15]));
    check('limits occupancy polling keeps the provider select, focused hold row and live count in place',
      d.activeElement === limitButton && limitButton.isConnected &&
      d.getElementById('lim-provider') === limitProvider &&
      limitProvider.options[0] === limitProviderOption &&
      d.querySelector('#lim-holds .pause-hold') === limitHold &&
      d.getElementById('lim-count').textContent === 'set in UI · in-flight 4 · queued 3 · 15/20 req');

    // Opening each menu paints every checklist once: the reset owns the
    // values, the sync owns the single list paint.
    {
      w.hideHdrMenu('debug-menu');
      const paints = {};
      const originalFill = w.fillDebugChecks;
      w.fillDebugChecks = function (box, names, picked, kind) {
        paints[kind] = (paints[kind] || 0) + 1;
        return originalFill.apply(this, arguments);
      };
      try { w.toggleDebugMenu({ stopPropagation() {} }); }
      finally { w.fillDebugChecks = originalFill; }
      check('opening Debug paints each checklist once',
        paints.client === 1 && paints.provider === 1 && paints.model === 1);
      w.hideHdrMenu('debug-menu');

      const pausePaints = {};
      const originalRows = w.fillCheckRows;
      w.fillCheckRows = function (box) {
        if (box) pausePaints[box.id] = (pausePaints[box.id] || 0) + 1;
        return originalRows.apply(this, arguments);
      };
      try { w.togglePauseMenu({ stopPropagation() {} }); }
      finally { w.fillCheckRows = originalRows; }
      check('opening Pause paints each checklist once',
        pausePaints['pf-clients'] === 1 && pausePaints['pf-providers'] === 1);
      w.hideHdrMenu('pause-menu');
    }

    // Structural changes still render exact content: session/hold rows add
    // and remove, and vocabulary names enter and leave the checklist.
    {
      d.getElementById('debug-menu').hidden = false;
      w.applyDebugState(debugFixture(
        [{ id: 'structure-1', clients: ['client-a'], duration: '15m', ran_ms: 0, captures: 0 }],
        { clients: ['client-a'], models: ['model-a'] }));
      w.resetDebugMenuForm();
      const debugRow = () => [...d.querySelectorAll('#df-holds .pause-hold')]
        .map(row => row.querySelector('.hold-edit').textContent);
      check('a debug session renders its exact scope, duration and capture count',
        JSON.stringify(debugRow()) === JSON.stringify(['client-a · 15m · 0 req']) &&
        d.querySelector('#df-holds [data-operator="debug-stop"]').textContent === 'Stop');
      const oldClient = d.querySelector('#df-clients input[value="client-a"]');
      w.applyDebugState(debugFixture(
        [{ id: 'structure-2', providers: ['provider-a'], duration: '1h', ran_ms: 0, captures: 0 }],
        { clients: ['client-b'], models: ['model-b'] }));
      check('a replaced debug session and vocabulary renders the new exact rows and removes the old name',
        JSON.stringify(debugRow()) === JSON.stringify(['provider-a · 1h · 0 req']) &&
        !oldClient.isConnected && !d.querySelector('#df-clients input[value="client-a"]') &&
        d.querySelector('#df-clients label').textContent.trim() === 'client-b' &&
        d.querySelector('#df-models input[value="model-b"]').type === 'checkbox');
      w.applyDebugState(debugFixture([], { clients: ['client-b'], models: ['model-b'] }));
      check('stopping every debug session removes the row and its header',
        d.getElementById('df-holds').textContent === '' && !d.querySelector('#df-holds .pause-hold'));

      d.getElementById('pause-menu').hidden = false;
      w.applyPauseState(pauseFixture([], 0, { clients: ['client-a'] }));
      w.resetPauseMenuForm();
      check('no pause holds renders an empty hold container', d.getElementById('pf-holds').textContent === '');
      w.applyPauseState(pauseFixture(
        [{ id: 'structure-hold', all: true, duration: '15m', queued: 0 }], 0,
        { clients: ['client-a'] }));
      const pauseRow = () => [...d.querySelectorAll('#pf-holds .pause-hold')]
        .map(row => row.querySelector('.hold-edit').textContent);
      check('a pause hold renders its exact scope and duration with its Resume action',
        JSON.stringify(pauseRow()) === JSON.stringify(['all requests · 15m']) &&
        d.querySelector('#pf-holds [data-operator="pause-resume"]').textContent === 'Resume');
      w.applyPauseState(pauseFixture(
        [{ id: 'structure-hold', all: true, duration: '15m', queued: 0 },
          { id: 'structure-hold-2', clients: ['client-b'], duration: '1h', queued: 0 }], 0,
        { clients: ['client-a', 'client-b'] }));
      check('a second pause hold appends its exact row and vocabulary name',
        JSON.stringify(pauseRow()) === JSON.stringify(['all requests · 15m', 'client-b · 1h']) &&
        !!d.querySelector('#pf-clients input[value="client-b"]'));
      w.applyPauseState(pauseFixture([], 0, { clients: [] }));
      check('removing every pause hold clears the rows and known clients',
        d.getElementById('pf-holds').textContent === '' &&
        !d.querySelector('#pf-clients input[value="client-b"]'));

      d.getElementById('limits-menu').hidden = false;
      w.applyThrottleState({ ok: true, active: true, known_providers: ['provider-a'],
        throttles: [{ provider: 'provider-a', source: 'ui', concurrency: 2 }] });
      const limitRows = () => [...d.querySelectorAll('#lim-holds .pause-hold')]
        .map(row => row.querySelector('[data-operator="limit-edit"]').textContent);
      check('a limits row renders its exact cap and source',
        JSON.stringify(limitRows()) === JSON.stringify(['provider-a · ×2']) &&
        d.querySelector('#lim-holds .lim-src').textContent === 'ui');
      w.applyThrottleState({ ok: true, active: true, known_providers: ['provider-a', 'provider-b'],
        throttles: [{ provider: 'provider-a', source: 'ui', concurrency: 2 },
          { provider: 'provider-b', source: 'header', updated_by: 'edge' }] });
      check('a second limits row appends with its source badge and provider option',
        JSON.stringify(limitRows()) === JSON.stringify(['provider-a · ×2', 'provider-b · ']) &&
        JSON.stringify([...d.getElementById('lim-provider').options].map(o => o.value)) ===
          JSON.stringify(['provider-a', 'provider-b']) &&
        d.querySelectorAll('#lim-holds .lim-src')[1].textContent === 'header');
      w.applyThrottleState({ ok: true, active: false, known_providers: ['provider-a'], throttles: [] });
      check('removing every limits row clears the container', d.getElementById('lim-holds').textContent === '');

      w.applyPauseState({ ok: true, paused: false, holds: [], known_clients: [], known_providers: [] });
      w.resetPauseMenuForm();
      w.hideHdrMenu('pause-menu');
      w.applyDebugState(observerFixture({ ok: true, enabled: false, sessions: [],
        known_clients: [], known_providers: [], known_models: [],
        ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture() }));
      w.resetDebugMenuForm();
      w.hideHdrMenu('debug-menu');
      w.applyThrottleState({ ok: true, active: false, throttles: [], known_providers: [] });
      w.syncLimitsMenuState();
      w.hideHdrMenu('limits-menu');
    }
  }

  // ---- GAP-5 pin: the settings search filter (rowMatches/showSettingsCat).
  // No jsdom check ever set settingsQ or typed into settings-q, so the row
  // filter, the "N matches" header and the rail badge counts were unpinned.
  // A crafted two-category doc drives the real oninput entry
  // (filterSettings): a help-text word, a multi-hit help word, and a key
  // word; the rail badges must equal the visible rows per category.
  {
    w.__filterDoc = {
      revision: 'fixture-revision',
      fields: [
        { key: 'conc_cap', category: 'rate', label: 'Concurrency cap', help: 'upstream in-flight request ceiling', kind: 'int', hot_reload: true },
        { key: 'queue_cap', category: 'rate', label: 'Queue cap', help: 'requests held while paused', kind: 'int', hot_reload: true },
        { key: 'rpm_limit', category: 'window', label: 'Request window', help: 'per-minute request budget', kind: 'int', hot_reload: true },
        { key: 'tpm_limit', category: 'window', label: 'Token window', help: 'per-minute token budget', kind: 'int', hot_reload: true },
      ],
      categories: [
        { id: 'rate', label: 'Rate', help: 'rate controls' },
        { id: 'window', label: 'Windows', help: 'window controls' },
      ],
      values: { conc_cap: 4, queue_cap: 0, rpm_limit: 60, tpm_limit: 1000 },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.eval('settingsDoc = window.__filterDoc; fillSettingsForm(settingsDoc)');
    const visibleKeys = () => [...d.querySelectorAll('#settings-fields .st-row')]
      .filter(r => !r.hidden).map(r => r.dataset.key);
    const badges = () => Object.fromEntries([...d.querySelectorAll('#settings-rail .st-rail-item')]
      .map(b => [b.dataset.stCat, Number(b.querySelector('.rail-n').textContent)]));
    const q = d.getElementById('settings-q');
    q.value = 'ceiling';
    w.filterSettings();
    check('a help-text word narrows the settings rows to its haystack hit',
      JSON.stringify(visibleKeys()) === JSON.stringify(['conc_cap']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match');
    check('the rail badges count the visible rows per category',
      JSON.stringify(badges()) === JSON.stringify({rate: 1, window: 0}) &&
      d.querySelector('[data-st-cat="rate"]').classList.contains('has-hit') &&
      d.querySelector('[data-st-cat="window"]').classList.contains('is-miss'));
    q.value = 'per-minute';
    w.filterSettings();
    check('a multi-hit help word shows every matching row in its category',
      JSON.stringify(visibleKeys()) === JSON.stringify(['rpm_limit', 'tpm_limit']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '2 matches');
    check('the multi-hit badge counts stay per-category',
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 2}));
    q.value = 'rpm_limit';
    w.filterSettings();
    check('a key word matches by the row key alone',
      JSON.stringify(visibleKeys()) === JSON.stringify(['rpm_limit']) &&
      d.querySelector('#settings-pane-hd p').textContent === 'rpm_limit');
    check('the key-word hit re-badges both categories',
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 1}) &&
      d.querySelector('[data-st-cat="window"]').classList.contains('has-hit'));
    q.value = '';
    w.filterSettings();
    check('clearing the query restores the category view',
      JSON.stringify(badges()) === JSON.stringify({rate: 2, window: 2}) &&
      ![...d.querySelectorAll('#settings-rail .st-rail-item')].some(b => b.classList.contains('has-hit')));
    w.__filterDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__filterDoc; fillSettingsForm(settingsDoc)');
    delete w.__filterDoc;
  }

  // ---- settings search haystack cache: fillSettingsForm stamps each rendered
  // row once with its lowercase label + key + help join, and showSettingsCat
  // then serves both row visibility and every rail badge from one row query
  // and one pass. A three-category fixture pins the cross-category badges,
  // the active/aria state, the singular/plural copy, the empty result, the
  // refill replacement and the back-to-category restore.
  {
    const doc = {
      revision: 'search-cache-r1',
      fields: [
        { key: 'alpha_rate', category: 'rate', label: 'Legacy alpha rate', help: 'old wording ceiling', kind: 'int', hot_reload: true },
        { key: 'beta_rate', category: 'rate', label: 'Beta rate', help: 'shared limit', kind: 'int', hot_reload: false },
        { key: 'gamma_window', category: 'window', label: 'Gamma window', help: 'ledger size', kind: 'int', hot_reload: true },
        { key: 'delta_window', category: 'window', label: 'Delta window', help: 'ledger cadence', kind: 'int', hot_reload: false },
        { key: 'epsilon_client', category: 'client', label: 'Epsilon client', help: 'client ledger label', kind: 'string', hot_reload: true },
        { key: 'zeta_client', category: 'client', label: 'Zeta client', help: 'unrelated note', kind: 'string', hot_reload: true },
      ],
      categories: [
        { id: 'rate', label: 'Rate', help: 'rate controls' },
        { id: 'window', label: 'Windows', help: 'window controls' },
        { id: 'client', label: 'Clients', help: 'client controls' },
      ],
      values: { alpha_rate: 1, beta_rate: 2, gamma_window: 3, delta_window: 4, epsilon_client: 'one', zeta_client: 'two' },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.__searchCacheDoc = doc;
    w.eval("settingsCat = 'rate'; settingsDoc = window.__searchCacheDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const rail = d.getElementById('settings-rail');
    const q = d.getElementById('settings-q');
    const visibleKeys = () => [...box.querySelectorAll('.st-row')]
      .filter(r => !r.hidden).map(r => r.dataset.key);
    const badges = () => Object.fromEntries([...rail.querySelectorAll('.st-rail-item')]
      .map(b => [b.dataset.stCat, Number(b.querySelector('.rail-n').textContent)]));
    const railState = id => {
      const b = rail.querySelector(`[data-st-cat="${id}"]`);
      return { hit: b.classList.contains('has-hit'), miss: b.classList.contains('is-miss'),
        active: b.classList.contains('active'), current: b.getAttribute('aria-current') };
    };
    const cachedRows = [...box.querySelectorAll('.st-row')];
    check('every rendered settings row caches its exact lowercase haystack',
      cachedRows.length === 8 && cachedRows.every(row => row.dataset.search ===
        ((row.dataset.label || '') + ' ' + (row.dataset.key || '') + ' ' + (row.dataset.help || '')).toLowerCase()) &&
      box.querySelector('.st-row[data-key="gamma_window"]').dataset.search === 'gamma window gamma_window ledger size');
    // One hook around the fields container proves the keystroke path reads
    // the rendered rows once, not once per rail item.
    const nativeQSA = box.querySelectorAll.bind(box);
    let rowQueries = 0;
    box.querySelectorAll = sel => { if (sel === '.st-row') rowQueries++; return nativeQSA(sel); };
    q.value = '  LEDGER  ';
    w.filterSettings();
    delete box.querySelectorAll;
    check('one settings row query serves a keystroke regardless of rail length',
      rail.querySelectorAll('.st-rail-item').length === 3 && rowQueries === 1);
    check('a trimmed lowercase query shows every cross-category hit with exact copy',
      JSON.stringify(visibleKeys()) === JSON.stringify(['gamma_window', 'delta_window', 'epsilon_client']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '3 matches' &&
      d.querySelector('#settings-pane-hd p').textContent === 'ledger');
    check('one query pass sets every rail badge and its hit/miss class',
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 2, client: 1}) &&
      !railState('rate').hit && railState('rate').miss &&
      railState('window').hit && !railState('window').miss &&
      railState('client').hit && !railState('client').miss);
    check('a query clears the active rail item and aria-current',
      ['rate', 'window', 'client'].every(id => !railState(id).active && railState(id).current === 'false'));
    w.eval("settingsCat = 'window'; showSettingsCat()");
    check('switching category under a query keeps every hit and badge unchanged',
      JSON.stringify(visibleKeys()) === JSON.stringify(['gamma_window', 'delta_window', 'epsilon_client']) &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 2, client: 1}) &&
      d.querySelector('#settings-pane-hd h4').textContent === '3 matches' &&
      ['rate', 'window', 'client'].every(id => !railState(id).active && railState(id).current === 'false'));
    q.value = 'zeta';
    w.filterSettings();
    check('a one-row query uses the singular copy and one badge hit',
      JSON.stringify(visibleKeys()) === JSON.stringify(['zeta_client']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match' &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 0, client: 1}) &&
      railState('client').hit && railState('rate').miss && railState('window').miss);
    q.value = 'delta_window';
    w.filterSettings();
    check('a row key alone is part of the cached search haystack',
      JSON.stringify(visibleKeys()) === JSON.stringify(['delta_window']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match' &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 1, client: 0}));
    q.value = 'no-such-setting';
    w.filterSettings();
    check('an empty result hides every row and marks every rail item a miss',
      visibleKeys().length === 0 &&
      d.querySelector('#settings-pane-hd h4').textContent === '0 matches' &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 0, client: 0}) &&
      ['rate', 'window', 'client'].every(id => railState(id).miss));
    w.eval("settingsCat = 'rate'");
    q.value = '';
    w.filterSettings();
    check('clearing the query restores the active category rows and full badges',
      JSON.stringify(visibleKeys()) === JSON.stringify(['alpha_rate', 'beta_rate']) &&
      JSON.stringify(badges()) === JSON.stringify({rate: 2, window: 2, client: 2}) &&
      d.querySelector('#settings-pane-hd h4').textContent === 'Rate' &&
      d.querySelector('#settings-pane-hd p').textContent === 'rate controls' &&
      railState('rate').active && railState('rate').current === 'page' &&
      ['rate', 'window', 'client'].every(id => !railState(id).hit && !railState(id).miss));
    q.value = 'legacy';
    w.filterSettings();
    check('the pre-refill label term is searchable', JSON.stringify(visibleKeys()) === JSON.stringify(['alpha_rate']));
    const next = JSON.parse(JSON.stringify(doc));
    next.revision = 'search-cache-r2';
    next.fields.find(f => f.key === 'alpha_rate').label = 'Current alpha rate';
    next.fields.find(f => f.key === 'alpha_rate').help = 'new wording ceiling';
    w.__searchCacheNext = next;
    w.eval('settingsDoc = window.__searchCacheNext; fillSettingsForm(settingsDoc)');
    check('a refill replaces the cached haystack instead of keeping stale text',
      box.querySelector('.st-row[data-key="alpha_rate"]').dataset.search === 'current alpha rate alpha_rate new wording ceiling');
    q.value = 'legacy';
    w.filterSettings();
    check('the old label stops matching after the refill', visibleKeys().length === 0);
    q.value = 'current';
    w.filterSettings();
    check('the refill label is the row text the search now sees',
      JSON.stringify(visibleKeys()) === JSON.stringify(['alpha_rate']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match');
    q.value = '';
    w.filterSettings();
    w.eval("settingsCat = 'providers'");
    w.__searchCacheRestore = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__searchCacheRestore; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__searchCacheDoc;
    delete w.__searchCacheNext;
    delete w.__searchCacheRestore;
  }

  // ---- settings open no-op: a clean, unchanged reopen still fetches for
  // freshness, but the accepted document must leave the retained form in
  // place - same nodes, same values, same baseline, no full fingerprint -
  // while the category/search presentation is re-applied. Revision or
  // writable changes, dirty drafts and Revert still refill.
  {
    const originalFetch = w.fetch;
    const pending = [];
    w.fetch = (url, init) => {
      if (!String(url).includes('/admin/config')) return originalFetch(url, init);
      return new Promise(resolve => pending.push(resolve));
    };
    const reply = doc => ({ ok: true, statusText: 'OK', json: async () => JSON.parse(JSON.stringify(doc)) });
    const doc = {
      revision: 'noop-r1',
      fields: [
        { key: 'noop_count', category: 'alpha', label: 'Noop count', help: 'alpha row', kind: 'int', min: 0, max: 10, hot_reload: true },
        { key: 'noop_flag', category: 'beta', label: 'Noop flag', help: 'beta row', kind: 'bool', hot_reload: true },
      ],
      categories: [
        { id: 'alpha', label: 'Alpha', help: 'alpha controls' },
        { id: 'beta', label: 'Beta', help: 'beta controls' },
      ],
      values: { noop_count: 2, noop_flag: true },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    const box = d.getElementById('settings-fields');
    const q = d.getElementById('settings-q');
    const apply = d.getElementById('btn-settings-apply');
    const revert = d.getElementById('btn-settings-revert');
    const status = () => d.getElementById('settings-count').textContent;
    let fillCalls = 0, fingerprintCalls = 0;
    const realFill = w.fillSettingsForm;
    const realFingerprint = w.settingsFingerprint;
    w.fillSettingsForm = function () { fillCalls++; return realFill.apply(this, arguments); };
    w.settingsFingerprint = function () { fingerprintCalls++; return realFingerprint.apply(this, arguments); };
    const valueInput = () => box.querySelector('input[data-key="noop_count"]');
    const openWith = async next => {
      w.toggleSettings({ stopPropagation() {} });
      await settleUntil(() => pending.length === 1);
      pending.shift()(reply(next));
      await sleep(20);
    };
    // A first open has no baseline and no rendered rows: it must fill.
    w.closeSettings(true);
    w.eval('settingsDoc = null; settingsSnap = ""; settingsSnapByKey = null; settingsChangedKeys = new Set(); settingsKeyEls = new Map(); settingsCat = ""; settingsQ = "";');
    box.innerHTML = '';
    d.getElementById('settings-rail').innerHTML = '';
    q.value = '';
    await openWith(doc);
    const firstInput = valueInput();
    check('settings first open still fills the accepted document',
      fillCalls === 1 && !!firstInput && firstInput.value === '2' &&
      w.eval('settingsDoc.revision') === 'noop-r1');
    // Leave the next open a search and a non-default active category to
    // restore. Closing keeps the populated DOM.
    w.eval("settingsCat = 'beta'");
    q.value = 'noop';
    w.filterSettings();
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(doc);
    check('a clean, unchanged reopen performs no fill and no full fingerprint collection',
      fillCalls === 0 && fingerprintCalls === 0 &&
      valueInput() === firstInput && firstInput.value === '2');
    check('the skipped reopen resets search and re-applies the active category',
      q.value === '' && w.eval('settingsQ') === '' &&
      !box.querySelector('.st-row[data-key="noop_flag"]').hidden &&
      box.querySelector('.st-row[data-key="noop_count"]').hidden &&
      d.querySelector('#settings-pane-hd h4').textContent === 'Beta' &&
      d.querySelector('[data-st-cat="beta"]').classList.contains('active'));
    check('the skipped reopen keeps a clean draft with both actions disabled',
      !w.settingsIsDirty() && apply.disabled && revert.disabled && status() === '');
    fingerprintCalls = 0;
    const cleanDirty = w.settingsIsDirty();
    w.updateSettingsActions();
    check('the clean dirty predicate and action refresh stay on the incremental baseline',
      !cleanDirty && fingerprintCalls === 0 && apply.disabled && revert.disabled);
    // An out-of-band revision change is fresh state and must refill.
    const revisionDoc = JSON.parse(JSON.stringify(doc));
    revisionDoc.revision = 'noop-r2';
    revisionDoc.values.noop_count = 7;
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(revisionDoc);
    check('an out-of-band revision change refills and reflects the new document',
      fillCalls === 1 && valueInput() !== firstInput && valueInput().value === '7' &&
      w.eval('settingsDoc.revision') === 'noop-r2');
    // A writable flip on the same revision is a different document too.
    const readOnlyDoc = JSON.parse(JSON.stringify(revisionDoc));
    readOnlyDoc.writable = false;
    readOnlyDoc.values.noop_count = 8;
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(readOnlyDoc);
    check('a writable flip refills and reflects the read-only document',
      fillCalls === 1 && valueInput().value === '8' &&
      status() === 'in-memory only - start with -config to persist' &&
      apply.disabled && revert.disabled);
    // A dirty draft always wins over a clean server refresh.
    const cleanDoc = JSON.parse(JSON.stringify(doc));
    cleanDoc.revision = 'noop-r3';
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(cleanDoc);
    const dirtyInput = valueInput();
    dirtyInput.value = '5';
    dirtyInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the reopen fixture drafts an edit with both actions armed',
      dirtyInput.value === '5' && w.settingsIsDirty() && !apply.disabled && !revert.disabled);
    fingerprintCalls = 0;
    const draftDirty = w.settingsIsDirty();
    w.updateSettingsActions();
    check('the dirty predicate and action refresh stay on the incremental baseline',
      draftDirty && fingerprintCalls === 0 && !apply.disabled && !revert.disabled);
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(cleanDoc);
    check('a clean refresh never replaces a dirty draft',
      fillCalls === 0 && fingerprintCalls === 0 && valueInput() === dirtyInput &&
      dirtyInput.value === '5' && w.settingsIsDirty() && !apply.disabled && !revert.disabled);
    // Revert discards only the draft it started with: typing while its GET
    // is in flight wins, a later quiet Revert refills from the server.
    fillCalls = 0;
    const reverting = w.revertSettings();
    await settleUntil(() => pending.length === 1);
    dirtyInput.value = '6';
    dirtyInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    pending.shift()(reply(cleanDoc));
    await reverting;
    check('typing after Revert starts survives its response',
      valueInput() === dirtyInput && dirtyInput.value === '6' && w.settingsIsDirty() &&
      !apply.disabled && !revert.disabled);
    fillCalls = 0;
    const revertingClean = w.revertSettings();
    await settleUntil(() => pending.length === 1);
    check('Revert keeps both actions disabled while its request is in flight',
      apply.disabled && revert.disabled && revert.dataset.busy === '1');
    pending.shift()(reply(cleanDoc));
    await revertingClean;
    check('Revert discards its own draft and refills from the accepted document',
      fillCalls === 1 && valueInput() !== dirtyInput && valueInput().value === '2' &&
      !w.settingsIsDirty() && apply.disabled && revert.disabled);
    w.fillSettingsForm = realFill;
    w.settingsFingerprint = realFingerprint;
    w.fetch = originalFetch;
    w.__settingsNoopRestore = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsNoopRestore; fillSettingsForm(settingsDoc)');
    w.eval("settingsCat = 'providers'");
    w.closeSettings(true);
    delete w.__settingsNoopRestore;
  }

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
      await sleep(30);
      check('the queued hashchange still routes the new scope',
        w.eval('_navScopeKey') !== scopeBefore &&
        w.eval('_routedHash') === w.eval('location.hash'));
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
    await sleep(20);
    check('unchanged frontend updates data without navigation or extra version requests',
      state.jsdomReloads === 0 && calls === 1 && w.eval('kpiAgg.requests') === 37);

    // An unexpected full response to a resume request is re-captured behind
    // the full/SSE barrier. The accepted full carries its own state/version.
    setResponse({ ...payload(TEST_DASHBOARD_VERSION), feed_id: 'same-assets-new-process', incremental: false });
    calls = 0;
    w.fetchBootstrap('resume');
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
    await sleep(20);
    check('unchanged frontend runs the deferred restart sweep once', sweeps === 2);

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
    await sleep(10);
    pending[0]({ok: true, json: async () => payload(newVersion, 999)});
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
      await sleep(100);
      const sw = seeded.window;
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
        await sleep(20);
        check('completion between HTML and SSE replay finalizes once without duplicating a row',
          sw.eval('lastData.records.filter(r => r.id === "seed-pending").length') === 1 &&
          !sw.eval('lastData.records.find(r => r.id === "seed-pending")?.live'));
        sw.fetchBootstrap('full');
        await sleep(20);
        check('later full resync fetches fresh state rather than reusing HTML seed', bootstrapFetches === 1);
      }
      sw.close();
    }
  }

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
      await sleep(40);
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
      await sleep(5);
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
      await sleep(5);
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

  // Delegated operator actions repaint in place, so the clicked node stays
  // connected through the action itself. Outside-click dismissal still uses
  // the original event path rather than live target ancestry, for both edits
  // and pending/confirmed mutations.
  for (const [kind, action] of [['pause','pause-edit'], ['pause','pause-resume'], ['debug','debug-edit'], ['debug','debug-stop'], ['limits','limit-edit']]) {
    const isolated = dashboardDOM(assembleHTML(), pageOptions);
    await loaded(isolated);
    const sw = isolated.window, sd = sw.document;
    try {
      await sleep(60);
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
        await sleep(5);
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
    let adminReply, adminCalls = 0, chartReply;
    const isolated = dashboardDOM(assembleHTML(state), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      const fallback = win.fetch;
      win.fetch = (url, options) => {
        if (String(url).includes('/metrics/bootstrap')) return new Promise(resolve => requests.push(resolve));
        if (String(url).includes('/metrics/agg/chart') && chartReply) return chartReply;
        if (String(url).startsWith('/admin/') && options?.method === 'POST' && adminReply) {adminCalls++; return adminReply;}
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
      await sleep(40);
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
      await sleep(20);
      const incidentDialog = sd.getElementById('storm-dialog');
      const detail = label => [...incidentDialog.querySelectorAll('dt')].find(node => node.textContent === label)?.nextElementSibling.textContent;
      const closeIncident = incidentDialog.querySelector('[data-operator="storm-close"]');
      check('incident click opens shared modal with distinct requests, attempt rates and provider evidence',
        !incidentDialog.hidden && incidentDialog.getAttribute('role') === 'dialog' &&
        sd.activeElement === closeIncident && sd.querySelector('main').hasAttribute('inert') &&
        detail('Requests affected') === '9' && detail('Failed upstream attempts') === '15' &&
        detail('Sampled upstream attempts') === '20' && detail('Upstream attempt failure rate') === '75%' &&
        detail('Queued requests') === '4' && detail('Detection window') === '1m' && detail('Error') === 'http_503' &&
        detail('Next retry') === new sw.Date(stormFixture.storms[0].retry_at).toLocaleString() &&
        detail('Affected active models') === '2 / 3 (66.7%)');
      const updatedStorm = {...stormFixture,storms:[{...stormFixture.storms[0],queued:7,error_requests:10,
        state:'half_open',recovery_successes:1,retry_at:'2026-09-05T12:00:30Z'}]};
      sw.applyBootstrapState({...state,storm:updatedStorm});
      check('accepted bootstrap updates the open incident while keeping close control and trigger focus identities',
        sd.activeElement === closeIncident && incidentDialog.querySelector('[data-operator="storm-close"]') === closeIncident &&
        banner.querySelector('button') === incidentButton && detail('Queued requests') === '7' &&
        detail('Requests affected') === '10' && detail('Successful recovery probes') === '1 / 2' &&
        detail('Next retry') === new sw.Date(updatedStorm.storms[0].retry_at).toLocaleString());
      closeIncident.dispatchEvent(new sw.KeyboardEvent('keydown', {key:'Escape',bubbles:true,cancelable:true}));
      check('incident Escape closes shared modal and restores the current incident trigger',
        incidentDialog.hidden && sd.activeElement === incidentButton && !sd.querySelector('main').hasAttribute('inert'));
      incidentButton.click();
      await sleep(20);
      sw.applyBootstrapState({...state,storm:{...stormFixture,storms:[]}});
      check('resolved incident stays reviewable without showing stale counts or stealing focus',
        !incidentDialog.hidden && incidentDialog.textContent.includes('no longer active') && !incidentDialog.querySelector('dl') &&
        sd.activeElement === closeIncident);
      closeIncident.click();
      check('resolved incident close restores a visible dashboard control',
        incidentDialog.hidden && (sd.activeElement === sd.getElementById('btn-settings') ||
          sd.activeElement === sd.getElementById('btn-nav')));

      const unspecifiedStorm = {...stormFixture,storms:[stormFixture.storms[0], {...stormFixture.storms[0],scope:'model',model:''}]};
      sw.applyBootstrapState({...state,storm:unspecifiedStorm});
      check('unspecified model incident remains distinct from the provider and does not hide valid incidents',
        banner.querySelectorAll('button').length === 2 && banner.textContent.includes('(unspecified model)'));
      banner.querySelectorAll('button')[1].click();
      await sleep(20);
      check('unspecified model details preserve exact empty-model scope', detail('Model scope') === '(unspecified model)');
      closeIncident.click();

      const modelStorm = {...stormFixture,storms:[{...stormFixture.storms[0],provider:'<img src=x onerror=alert(1)>',
        model:'raw/model<script>fixture</script>',scope:'model',state:'half_open',reason:'transport',recovery_successes:1}]};
      sw.applyBootstrapState({...state,storm:modelStorm});
      check('model storm labels stay exact and inert in compact recovery summaries',
        banner.textContent.includes('raw/model<script>fixture</script>') && !banner.querySelector('img, script') &&
        banner.textContent.includes('Checking recovery') &&
        !banner.textContent.includes('active models affected'));
      banner.querySelector('button').click();
      await sleep(20);
      check('incident details render hostile provider/model/error values as inert text',
        detail('Provider') === modelStorm.storms[0].provider && detail('Model scope') === modelStorm.storms[0].model &&
        detail('Successful recovery probes') === '1 / 2' && !incidentDialog.querySelector('img, script') && !detail('Active models'));
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
        await sleep(20);
        check('quota incident dialog shows the pause title, probe facts and a resume action, without storm noise',
          !incidentDialog.hidden && sd.getElementById('storm-dialog-title').textContent === 'Provider quota pause' &&
          detail('Error') === 'insufficient_quota' && detail('Queued requests') === '5' &&
          detail('Successful recovery probes') === '0 / 1' && !detail('Failed upstream attempts') &&
          incidentDialog.querySelector('[data-operator="quota-resume"]')?.dataset.value === 'neutral.example');
        const releaseQuota = Promise.resolve({ok:true,json:async()=>({enabled:true,banner_enabled:true,storms:[]})});
        adminReply = releaseQuota;
        const callsBeforeResume = adminCalls;
        incidentDialog.querySelector('[data-operator="quota-resume"]').click();
        await sleep(20);
        check('operator resume posts to the quota endpoint and repaints from the confirmed storm snapshot',
          adminCalls === callsBeforeResume + 1 && banner.hidden &&
          incidentDialog.textContent.includes('no longer active'));
        adminReply = Promise.resolve({ok:false,json:async()=>({error:'quota endpoint unavailable'})});
        sw.applyBootstrapState({...state,storm:quotaStorm});
        banner.querySelector('button').click();
        await sleep(20);
        incidentDialog.querySelector('[data-operator="quota-resume"]').click();
        await sleep(20);
        check('a failed resume keeps the dialog open with the failure beside the action',
          !incidentDialog.hidden && incidentDialog.querySelector('[data-operator="quota-resume"]') &&
          incidentDialog.textContent.includes('quota endpoint unavailable'));
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
      await sleep(20);
      check('foreign observer metadata re-captures through the existing full barrier', requests.length === 0 && streams.at(-1).url.includes('since=2'));

      // A reload between bootstrap and Debug serialization cannot install
      // mixed dictionary revisions or even expose that response's storage state.
      const mixed = sw.fetchBootstrap('full');
      reply({...state,storage:{enabled:true,dropped:99},debug:{ok:true,enabled:false,sessions:[],known_models:[],model_canon:modelFixture()}});
      await sleep(10);
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

      const hostile = "label');window.__operatorXSS=1;//";
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],throttles:[{provider:hostile,requests:2,request_window:'2m'}]});
      sw.toggleLimitsMenu({stopPropagation(){}});
      sd.querySelector('#lim-holds [data-operator]').click();
      check('provider actions are inert delegated data, including quotes and script text',
        !sw.__operatorXSS && sd.getElementById('lim-provider').value === hostile && !sd.querySelector('#lim-holds [onclick]'));
      check('Limits custom duration survives edit and refresh', sd.getElementById('lim-reqw').value === '2m');
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
      await sleep(20);
      const drawer=sd.getElementById('drawer');
      check('keyboard-operable log button opens an accessible modal and makes background inert',
        !drawer.hidden && !drawer.hasAttribute('inert') && drawer.contains(sd.activeElement) && sd.querySelector('main').hasAttribute('inert'));
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
      await sleep(30);
      const sheet=sd.getElementById('settings-sheet');
      check('Settings shares modal focus containment and background inertness',
        !sheet.hidden && sheet.contains(sd.activeElement) && sd.querySelector('main').hasAttribute('inert'));
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
      await sleep(10);
      check('a rejected restart cancels its already-open watcher and releases the reader',
        watchedSignal?.aborted && readerCanceled===1 && sd.getElementById('restart-count').textContent.includes('restart rejected'));
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

  // ---- W16 mirror pins: the Go<->JS pairs the mutation rounds proved unguarded ----
  // One isolated page built directly (not through dashboardDOM's fetch
  // wrapper) so the debug-capture fixture reaches the drawer byte-exact.
  // Every pin names the Go owner it mirrors; the Go suites pin their side of
  // each pair.
  {
    const captureDoc = {
      schema: 'millivolt.debug/v1',
      id: 'dbg-drawer',
      captured_at: '2026-09-14T00:00:00Z',
      expires_at: '2026-09-15T00:00:00Z',
      debug_session_id: 'first',
      match: { client: 'c', provider: 'neutral.example', model: 'm' },
      identity: {
        conversation_id: 'cv', client: 'c', provider: 'neutral.example', model: 'm',
        key_hash: 'kh', stream: true, path: '/v1/chat/completions', method: 'POST', format: 'openai',
      },
      timing: { start: '2026-09-14T00:00:00Z', end: '2026-09-14T00:00:01Z', duration_ms: 1000, ttft_ms: 250, queue_wait_ms: 10, first_answer_ms: 300 },
      outcome: { status_code: 200, finish_reason: 'stop', error_type: '', error_code: '', error_msg: '', client_disconnected: false },
      usage: { input_tokens: 10, output_tokens: 5, total_tokens: 15 },
      cost: 0.01,
      request: {
        headers: [{name: 'Authorization', value: '[REDACTED]'}, {name: 'X-Empty-Value', value: ''}],
        body: { raw: '{"model":"m"}', truncated: true },
        url: 'https://up.example/v1/chat/completions',
      },
      response: {
        headers: [{name: 'Content-Type', value: 'application/json'}],
        body: { raw: '{"id":"1"}', truncated: false },
      },
      attempts: [],
    };
    const seed = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'w16-mirrors', seq: 1,
      pending_revision: 0, incremental: false,
      records: [{...mkRec('dbg-drawer', 200), debug: true}],
      in_flight_records: [],
      counters: { in_flight: 0, total_requests: 1, total_errors: 0 },
      kpi: { requests: 1, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null },
      dash: {},
      storage: { enabled: true, dropped: 0, totals_degraded: false },
    });
    const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      const fallback = win.fetch;
      const dlFetches = [];
      win.__dlFetches = dlFetches;
      win.__dlResponse = { ok: true, blob: async () => new win.Blob(['gz-bytes']),
        headers: { get: k => k === 'Content-Disposition' ? 'attachment; filename="millivolt-debug-20260914-000000.json.gz"' : null } };
      win.fetch = (url, opts) => {
        const u = String(url);
        if (u.includes('/admin/debug/capture')) {
          if (u.includes('download=1')) {
            dlFetches.push({ u, auth: (opts && opts.headers && opts.headers.Authorization) || '' });
            return Promise.resolve(win.__dlResponse);
          }
          return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(captureDoc)) });
        }
        return fallback(url, opts);
      };
    }});
    const sw = isolated.window, sd = sw.document;
    try {
      await loaded(isolated);
      await sleep(60);

      // recordIsError mirrors metrics.Record.IsError. The 24 rows mirror the
      // semantics of tests/_go/internal/metrics/iserror_corpus.go (the Go
      // suite owns the corpus); the JS predicate must agree on every row.
      const corpus = [
        ['final 200, no attempts', {status_code: 200}, false],
        ['final 429 rate limit', {status_code: 429, error_type: 'rate_limit_error'}, false],
        ['final 499 client closed', {status_code: 499}, false],
        ['final 499 after an absorbed 5xx', {status_code: 499, attempts: [{status_code: 503}]}, true],
        ['final 500', {status_code: 500}, true],
        ['final 502', {status_code: 502}, true],
        ['final 400 (4xx other than 429)', {status_code: 400}, true],
        ['structured error, status 0', {status_code: 0, error_type: 'upstream_unreachable'}, true],
        ['recovered after absorbed 5xx', {status_code: 200, attempts: [{status_code: 502}]}, true],
        ['recovered after absorbed 500 (the >= 500 boundary)', {status_code: 200, attempts: [{status_code: 500}]}, true],
        ['recovered after absorbed 429 only', {status_code: 200, attempts: [{status_code: 429, error_type: 'rate_limit'}]}, false],
        ['final 429 after an absorbed 5xx', {status_code: 429, attempts: [{status_code: 503}]}, true],
        ['429 after an absorbed 500 still counts (flow-arm boundary)', {status_code: 429, rate_limited: true, attempts: [{status_code: 500}]}, true],
        ['queue wait is not429', {status_code: 200}, false],
        ['typed final429 without broad flag', {status_code: 429, error_type: 'insufficient_quota'}, false],
        ['recovered429 twice', {status_code: 200, attempts: [{status_code: 429}, {status_code: 429}]}, false],
        ['final429 plus retries', {status_code: 429, attempts: [{status_code: 429}, {status_code: 429}]}, false],
        ['recovered503', {status_code: 200, attempts: [{status_code: 503}]}, true],
        ['final429 after503', {status_code: 429, attempts: [{status_code: 503}]}, true],
        ['final500 after429', {status_code: 500, attempts: [{status_code: 429}]}, true],
        ['pending after429', {attempts: [{status_code: 429}]}, false],
        ['cancel after429', {status_code: 499, attempts: [{status_code: 429}]}, false],
        ['final 499 with in-band error', {status_code: 499, error_type: 'api_error', error_code: 'internal_error'}, true],
        ['final 499 with in-band rate limit error', {status_code: 499, error_type: 'rate_limit_error'}, true],
        ['committed 200 with an in-band 429', {status_code: 200, error_type: 'rate_limit_error', attempts: [{status_code: 429, in_band: true}]}, true],
        ['synthetic in-band 500 counts as an absorbed failure', {status_code: 200, attempts: [{status_code: 500, in_band: true}]}, true],
      ];
      const errorMisses = corpus.filter(([, rec, want]) => sw.recordIsError(rec) !== want).map(([name]) => name);
      check('recordIsError agrees with the 26-row Go IsError corpus' + (errorMisses.length ? ' (missed: ' + errorMisses.join(', ') + ')' : ''),
        errorMisses.length === 0);

      // statusClass mirrors contribStatusClass/statusClass in aggregate.go on
      // the boundary rows: 429 stays a 4xx class (never an error), 499 is its
      // own cancel class, live rows use the pill vocabulary with the
      // paused > throttled > streaming precedence, and a stored stream row
      // with status 0 is a finalized error, never "currently streaming".
      const classRows = [
        [{status_code: 200}, '2xx'], [{status_code: 299}, '2xx'],
        [{status_code: 429}, '4xx'], [{status_code: 404}, '4xx'],
        [{status_code: 499}, 'cancel'],
        [{status_code: 500}, '5xx'], [{status_code: 503}, '5xx'],
        [{}, 'err'], [{status_code: 302}, 'err'],
        [{live: true, paused: true, throttled: true, stream: true}, 'paused'],
        [{live: true, throttled: true, stream: true}, 'throttled'],
        [{live: true, stream: true}, 'streaming'],
        [{live: true}, 'pending'],
        [{stream: true, status_code: 0}, 'err'],
      ];
      const classMisses = classRows.filter(([rec, want]) => sw.statusClass(rec) !== want)
        .map(([rec, want]) => JSON.stringify(rec) + ' -> ' + sw.statusClass(rec) + ' (want ' + want + ')');
      check('statusClass agrees with the Go owner on the boundary rows' + (classMisses.length ? ' (missed: ' + classMisses.join('; ') + ')' : ''),
        classMisses.length === 0);

      // The live pill vocabulary mirrors liveStatusFilter (aggregate.go): the
      // exact set the server accepts as s=, routed through statusClass,
      // while numeric codes match the exact HTTP status.
      check('the live pill vocabulary is exactly the server-accepted s= set',
        JSON.stringify(sw.eval('Object.keys(LIVE_STATUS_FILTERS).sort()')) ===
        JSON.stringify(['paused', 'pending', 'streaming', 'throttled']));
      check('named pills route through statusClass; numeric codes match exactly',
        sw.recordMatchesStatusFilter({live: true, stream: true}, 'streaming') === true &&
        sw.recordMatchesStatusFilter({status_code: 200}, 'streaming') === false &&
        sw.recordMatchesStatusFilter({status_code: 429}, '429') === true &&
        sw.recordMatchesStatusFilter({status_code: 429}, '200') === false &&
        sw.recordMatchesStatusFilter({status_code: 200}, '') === true);

      // recordErrorEntries mirrors errorEntries (aggregate.go): 429 and a
      // plain 499 final produce no entry, a 499 carrying the provider's
      // in-band error does (the client aborted around a real failure), an
      // in-band provider error on a 200 does, and
      // absorbed attempts keep genuine failures (5xx, typed, 4xx) while
      // skipping flow control and bare non-failures. In-band attempt entries
      // are health evidence, never a second disclosure for the final failure,
      // including the synthetic in-band 5xx shape. errorKey mirrors the Go
      // type|code|msg identity the explorer error dimension filters by.
      const entsRec = {
        status_code: 200, error_code: '', error_msg: '',
        attempts: [
          {status_code: 429},
          {status_code: 502, error_type: 'upstream'},
          {status_code: 0, error_type: 'dial_failed'},
          {status_code: 401},
          {status_code: 302},
          {status_code: 429, in_band: true},
          {status_code: 500, in_band: true},
        ],
        start: '2026-09-14T00:00:00Z',
      };
      const ents = sw.recordErrorEntries(entsRec);
      check('recordErrorEntries enumerates the absorbed-attempt boundary rows like the Go owner',
        JSON.stringify(ents.map(e => [e.type, e.code, e.absorbed, e.at])) === JSON.stringify([
          ['upstream', '502', true, '2026-09-14T00:00:00Z'],
          ['dial_failed', '', true, '2026-09-14T00:00:00Z'],
          ['http_401', '401', true, '2026-09-14T00:00:00Z'],
        ]));
      check('final 429 and a plain 499 produce no error entry (flow events)',
        sw.recordErrorEntries({status_code: 429, error_type: 'rate_limit_error'}).length === 0 &&
        sw.recordErrorEntries({status_code: 499}).length === 0);
      check('a 499 carrying the provider in-band error produces an entry like its 200 twin',
        JSON.stringify(sw.recordErrorEntries({status_code: 499, error_type: 'api_error', error_code: 'internal_error', error_msg: 'temporarily unavailable'}).map(e => [e.type, e.code, e.msg])) ===
        JSON.stringify([['api_error', 'internal_error', 'temporarily unavailable']]));
      check('an in-band provider error on a 200 is still a final error entry',
        JSON.stringify(sw.recordErrorEntries({status_code: 200, error_type: 'provider_overloaded', error_msg: 'x'}).map(e => [e.type, e.code, e.msg])) ===
        JSON.stringify([['provider_overloaded', '200', 'x']]));
      check('a final 5xx carries its synthetic http_N entry and code',
        JSON.stringify(sw.recordErrorEntries({status_code: 500}).map(e => [e.type, e.code, e.absorbed])) ===
        JSON.stringify([['http_500', '500', false]]));
      check('errorKey identity is type|code|msg with empty parts kept',
        sw.errorKey('http_500', '500', '') === 'http_500|500|' &&
        sw.errorKey('', '', '') === '||' &&
        ents.every(e => sw.errorKey(e.type, e.code, e.msg) === [e.type, e.code, e.msg].join('|')));

      // The duration vocabularies mirror Go acceptance: PAUSE_DURS and
      // DEBUG_DURS tokens are the pauseDurations allowlist (pinned through
      // the real pause/debug handlers Go-side), LIMIT_WINDOWS tokens pass
      // parseLimitWindow, and FILTER_AGES tokens are canonical
      // config.FormatDuration spellings. Labels ride the same pin so the
      // '15 min' vs '15 minutes' drift class cannot return.
      check('PAUSE_DURS offers exactly the server-accepted hold durations',
        JSON.stringify(sw.eval('PAUSE_DURS')) === JSON.stringify([
          ['', 'I resume'], ['15m', '15 minutes'], ['1h', '1 hour'],
          ['6h', '6 hours'], ['12h', '12 hours'], ['24h', '24 hours']]));
      check('DEBUG_DURS shares the pause vocabulary with its own empty label',
        JSON.stringify(sw.eval('DEBUG_DURS')) === JSON.stringify([
          ['', 'I stop'], ['15m', '15 minutes'], ['1h', '1 hour'],
          ['6h', '6 hours'], ['12h', '12 hours'], ['24h', '24 hours']]));
      check('LIMIT_WINDOWS offers exactly the parseLimitWindow-accepted tokens',
        JSON.stringify(sw.eval('LIMIT_WINDOWS')) === JSON.stringify([
          ['1s', '1 second'], ['10s', '10 seconds'], ['30s', '30 seconds'],
          ['1m', '1 minute'], ['5m', '5 minutes'], ['15m', '15 minutes'],
          ['1h', '1 hour'], ['6h', '6 hours'], ['24h', '24 hours']]));
      check('FILTER_AGES offers canonical Go duration spellings',
        JSON.stringify(sw.eval('FILTER_AGES')) === JSON.stringify([
          ['', '-'], ['1h', '1 hour'], ['24h', '1 day'], ['168h', '1 week']]));
      sw.togglePauseMenu({stopPropagation() {}});
      check('the pause menu renders the PAUSE_DURS token set',
        JSON.stringify([...sd.getElementById('pf-dur').options].map(o => o.value)) ===
        JSON.stringify(['', '15m', '1h', '6h', '12h', '24h']));
      sw.togglePauseMenu({stopPropagation() {}});

      // holdMatchesRecord mirrors scheduler holdSnap.matches on the scope
      // half (scheduler/hold.go); chrome.js holdLive owns the expiry half
      // the Go matcher folds in. The matrix mirrors the Go suite's
      // TestHoldMatchesMatrix row for row (that suite owns the table, the
      // hand-mirror documented here): the wire keys are all/new/clients/
      // providers/known_at_new, and every verdict must agree.
      const holdRows = [
        ['all matches any request', {all: true}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['all matches an empty identity', {all: true}, {client: '', provider: ''}, true],
        ['new skips a known client', {new: true, known_at_new: ['known']}, {client: 'known', provider: 'alpha.example'}, false],
        ['new matches an unseen client', {new: true, known_at_new: ['known']}, {client: 'fresh', provider: 'alpha.example'}, true],
        ['named client matches itself', {clients: ['client-a']}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['named client matches with any provider', {clients: ['client-a']}, {client: 'client-a', provider: ''}, true],
        ['named client misses another client', {clients: ['client-a']}, {client: 'client-b', provider: 'alpha.example'}, false],
        ['named hit wins over new', {clients: ['client-a'], new: true, known_at_new: ['client-a', 'seen']}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['named plus new skips a known non-member', {clients: ['client-a'], new: true, known_at_new: ['client-a', 'seen']}, {client: 'seen', provider: 'alpha.example'}, false],
        ['named plus new matches an unseen client', {clients: ['client-a'], new: true, known_at_new: ['client-a', 'seen']}, {client: 'brand-new', provider: 'alpha.example'}, true],
        ['provider-only matches its provider with any client', {providers: ['alpha.example']}, {client: 'whoever', provider: 'alpha.example'}, true],
        ['provider-only misses another provider', {providers: ['alpha.example']}, {client: 'whoever', provider: 'beta.example'}, false],
        ['provider-only never matches an empty provider argument', {providers: ['alpha.example']}, {client: 'whoever', provider: ''}, false],
        ['client and provider AND-match', {clients: ['client-a'], providers: ['alpha.example']}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['pair hold misses on provider', {clients: ['client-a'], providers: ['alpha.example']}, {client: 'client-a', provider: 'beta.example'}, false],
        ['pair hold misses on client', {clients: ['client-a'], providers: ['alpha.example']}, {client: 'client-b', provider: 'alpha.example'}, false],
        ['dimensionless hold is inactive', {all: false, clients: [], providers: [], new: false}, {client: 'client-a', provider: 'alpha.example'}, false],
      ];
      const holdMisses = holdRows
        .filter(([name, h, r, want]) => sw.eval(`holdMatchesRecord(${JSON.stringify(h)}, ${JSON.stringify(r)})`) !== want)
        .map(([name]) => name);
      check('holdMatchesRecord agrees with the Go matcher matrix' +
        (holdMisses.length ? ' (missed: ' + holdMisses.join(', ') + ')' : ''),
        holdMisses.length === 0);
      check('holdLive owns the expiry half the Go matcher folds in',
        sw.eval(`holdLive({all: true, until: new Date(Date.now() - 60000).toISOString()})`) === false &&
        sw.eval(`holdLive({all: true, until: new Date(Date.now() + 60000).toISOString()})`) === true);

      // parseGoDuration mirrors Go's duration grammar for every spelling
      // config.FormatDuration emits (pinned Go-side by
      // TestFormatDurationSpellingsParseBack, whose d.String() fallback row
      // spells microseconds with the micro sign, and whose parser accepts
      // the Greek mu too). Rows mirror that Go table; values are
      // milliseconds.
      const durRows = [
        ['0s', 0], ['1ns', 0.000001], ['1.5µs', 0.0015], ['1500us', 1.5],
        ['1500μs', 1.5], ['5ms', 5], ['500ms', 500], ['2s', 2000],
        ['90m', 5400000], ['3h', 10800000], ['24h', 86400000], ['-5s', -5000],
        ['1h30m', 5400000], ['0.5s', 500],
      ];
      const durMisses = durRows
        .filter(([spelling, want]) => sw.eval(`parseGoDuration(${JSON.stringify(spelling)})`) !== want)
        .map(([spelling]) => spelling);
      check('parseGoDuration parses every FormatDuration spelling the surfaces use' +
        (durMisses.length ? ' (missed: ' + durMisses.join(', ') + ')' : ''),
        durMisses.length === 0);
      check('parseGoDuration fails closed to 0 on garbage and empty input',
        sw.eval('parseGoDuration(\'45 min\')') === 0 &&
        sw.eval('parseGoDuration(\'\')') === 0 &&
        sw.eval('parseGoDuration(null)') === 0);

      // MODEL_RULES_MAX mirrors config.ModelRulesMax (pinned Go-side): the
      // count line and the add gate at the cap.
      {
        const wrap = sd.createElement('div');
        wrap.innerHTML = sw.modelRulesEditorHTML(Array.from({length: 64}, () => ({mode: 'lower'})));
        sd.getElementById('settings-fields').appendChild(wrap);
        sw.mrPreview(wrap);
        check('a full 64-rule draft shows the capped count line',
          wrap.querySelector('.mr-count').textContent === '64 / 64 rules');
        check('the add gate refuses a 65th rule at MODEL_RULES_MAX',
          wrap.querySelector('[data-mr-add]').disabled && wrap.querySelector('.mr-tpl').disabled);
        wrap.querySelector('[data-mr-add]').click();
        check('a disabled add control adds no row',
          wrap.querySelectorAll('.mr-row').length === 64);
        check('the mode select renders its options in the vocabulary\'s order with labels as values',
          JSON.stringify([...wrap.querySelector('.mr-row .mr-mode').options].map(o => o.value)) ===
          JSON.stringify(['exact', 'pattern', 'lower']));
        wrap.remove();
      }

      // The shipped-template derivation against a CRAFTED defaults doc:
      // payloads come from settingsDoc.defaults.model_rules positionally
      // (never a client copy), labels pair by position, and beyond the
      // authored label table the position fallback still offers every rule.
      {
        sw.__craftedDefaults = [
          {mode: 'lower'}, {mode: 'exact', from: 'a', to: 'b'},
          {mode: 'pattern', from: 'x', to: 'y'}, {mode: 'lower'},
          {mode: 'lower'}, {mode: 'exact', from: 'c', to: 'd'},
        ];
        sw.eval('settingsDoc = {defaults: {model_rules: window.__craftedDefaults}}');
        const wrap = sd.createElement('div');
        wrap.innerHTML = sw.modelRulesEditorHTML([{mode: 'lower'}]);
        sd.getElementById('settings-fields').appendChild(wrap);
        const tpl = wrap.querySelector('.mr-tpl');
        check('template labels pair by position with a longer defaults doc',
          [...tpl.options].map(o => o.textContent).join(',') ===
          'add from template…,lowercase fold,strip vendor/ prefix,strip :tag suffix,strip architecture/quant suffix (fp4, nvfp4, int8, q4_k_m…),unify . and - between digits,shipped rule 6,exact merge…,custom pattern…');
        tpl.value = '5';
        tpl.dispatchEvent(new sw.Event('change', {bubbles: true}));
        const rows = [...wrap.querySelectorAll('.mr-row')];
        check('the positional-fallback template carries the crafted doc payload',
          rows.length === 2 &&
          JSON.stringify(sw.collectModelRules(wrap)) === JSON.stringify([{mode: 'lower'}, {mode: 'exact', from: 'c', to: 'd'}]));
        wrap.remove();
        sw.eval('settingsDoc = null');
      }

      // The restart poll budgets mirror the Go choreography bounds
      // (cmd/proxy restartBuildTimeout/restartReadyTimeout and the config
      // default RestartDrainTimeout - each pinned Go-side) and RESTART_STEPS
      // follows the server's phaseRank order.
      check('the restart poll budgets mirror the Go choreography bounds',
        sw.eval('[RESTART_BUILD_BUDGET_MS, RESTART_READY_BUDGET_MS, RESTART_DRAIN_FALLBACK_MS].join()') === '300000,300000,600000');
      check('RESTART_STEPS ranks follow the server phaseRank order',
        sw.eval('RESTART_STEPS.map(s => s.rank).join()') === '0,1,2,3');

      // The two identity-width gates mirror the Go producers: the stamped
      // asset version is 16 lowercase hex (web.go etagHex), the model-canon
      // content revision is 64 lowercase hex (aggregate_observer.go sha256).
      check('the dashboard version gate accepts exactly 16 lowercase hex',
        sw.eval('DASHBOARD_VERSION_RE.test("0000000000000000")') === true &&
        sw.eval('DASHBOARD_VERSION_RE.test("000000000000000")') === false &&
        sw.eval('DASHBOARD_VERSION_RE.test("00000000000000000")') === false &&
        sw.eval('DASHBOARD_VERSION_RE.test("000000000000000A")') === false &&
        sw.eval(`DASHBOARD_VERSION_RE.test('${TEST_DASHBOARD_VERSION}')`) === true);
      check('the model-canon revision gate accepts exactly 64 lowercase hex',
        sw.eval('MODEL_REVISION_RE.test("a".repeat(64))') === true &&
        sw.eval('MODEL_REVISION_RE.test("a".repeat(63))') === false &&
        sw.eval('MODEL_REVISION_RE.test("a".repeat(65))') === false &&
        sw.eval('MODEL_REVISION_RE.test("A".repeat(64))') === false);

      // The entity palette: the registry is the single owner (favicon and
      // PWA-asset hexes are detector-covered against the :root palette
      // Go-side of the CSS; the .prov-ic emitters are pinned in the settings
      // editor tests). This pins what nothing else did: every dimension's
      // badge keys its color through the registry, colors stay pairwise
      // distinct (colorblind-safe classes), and the log's turn-role colors
      // reference the registry instead of a second palette.
      {
        const palette = sw.eval('ENTITY_TYPES');
        const want = {
          client: ['#56B4E9', '◈', 'Client'], provider: ['#0072B2', '☁', 'Provider'],
          model: ['#CC79A7', '◆', 'Model'], conversation: ['#009E73', '❝', 'Conversation'],
          key: ['#F0E442', '⚷', 'Key'], status: ['#8A8F98', '●', 'Status'],
          time: ['#5AC8FA', '◷', 'Time'], tool: ['#1ABC9C', '⚒', 'Tool'],
          request: ['#E69F00', '⇄', 'Request'], error: ['#D55E00', '⚠', 'Error'],
        };
        const got = Object.fromEntries(Object.entries(palette).map(([k, v]) => [k, [v.color, v.icon, v.label]]));
        check('ENTITY_TYPES carries the colorblind-safe palette triple for every dimension',
          JSON.stringify(got) === JSON.stringify(want));
        const colors = Object.values(palette).map(v => v.color.toLowerCase());
        check('entity palette colors stay pairwise distinct', new Set(colors).size === colors.length);
        check('every rendered entity badge keys its color through the registry',
          Object.entries(palette).every(([dim, v]) =>
            sw.entityBadge(dim, 'fixture-id').includes(`style="--ent:${v.color}"`)));
        check('turn-role colors reference the registry, not a second palette',
          JSON.stringify(sw.eval('TURN_ROLE_COLOR')) === JSON.stringify({
            user: '#56B4E9', assistant: '#CC79A7', tool: '#1ABC9C',
            function: '#1ABC9C', system: '#8A8F98', developer: '#8A8F98',
          }));
      }

      // The debug-capture drawer body: key-value rows from the capture doc,
      // the capture-header tint on the key span, empty values denied, and
      // truncated bodies labeled.
      sw.openDrawer('dbg-drawer');
      await sleep(30);
      const dbg = sd.getElementById('drawer-debug');
      const dbgRow = k => [...dbg.querySelectorAll('.detail-kv')].find(r => r.querySelector('.k') && r.querySelector('.k').textContent === k);
      check('the drawer renders the capture identity and timing rows',
        !!dbgRow('session') && dbgRow('session').querySelector('.v').textContent === 'first' &&
        !!dbgRow('client') && dbgRow('client').querySelector('.v').textContent === 'c' &&
        !!dbgRow('stream') && dbgRow('stream').querySelector('.v').textContent === 'yes' &&
        !!dbgRow('duration') && dbgRow('duration').querySelector('.v').textContent === '1s' &&
        !!dbgRow('queue wait') && dbgRow('queue wait').querySelector('.v').textContent === '10ms');
      const headerSection = [...dbg.querySelectorAll('.detail-section')]
        .find(s => s.querySelector('h4') && s.querySelector('h4').textContent === 'Request headers');
      const authRow = headerSection && [...headerSection.querySelectorAll('.detail-kv')]
        .find(r => r.querySelector('.k').textContent === 'Authorization');
      check('capture headers render with the accent tint on the key span',
        !!authRow && authRow.querySelector('.k').getAttribute('style') === 'color:var(--accent2)' &&
        authRow.querySelector('.v').textContent === '[REDACTED]');
      check('an empty capture-header value renders no row (deny by default)',
        !dbg.textContent.includes('X-Empty-Value'));
      check('truncated bodies are labeled and previewed',
        dbg.textContent.includes('Request body (truncated)') &&
        dbg.textContent.includes('Response body') &&
        !!dbg.querySelector('.preview-box'));

      // The download affordance: the section header button fetches the
      // same capture with download=1 through the operator gate and hands
      // the server-named artifact to triggerDownload (the backup flow's
      // blob-fetch pattern), with the fallback and failure contracts.
      const dbgBtn = dbg.querySelector('#drawer-debug-download');
      check('the debug capture section renders the download affordance with its accessible name',
        !!dbgBtn && dbgBtn.getAttribute('aria-label') === 'download debug capture' &&
        dbgBtn.dataset.id === 'dbg-drawer');
      if (dbgBtn) {
        sw.eval("operatorCredential = 'op-token'");
        const dlTape = [];
        sd.addEventListener('click', e => dlTape.push(e.target));
        sw.URL.createObjectURL = () => 'blob:w16-dl';
        sw.URL.revokeObjectURL = href => dlTape.push('revoke:' + href);
        dbgBtn.click();
        await sleep(30);
        check('clicking download fetches the capture artifact through the operator gate',
          sw.__dlFetches.length === 1 &&
          sw.__dlFetches[0].u === '/admin/debug/capture?id=dbg-drawer&download=1' &&
          sw.__dlFetches[0].auth === 'Bearer op-token');
        check('the server-named artifact reaches triggerDownload and the blob URL is revoked',
          dlTape.some(el => el && el.tagName === 'A' &&
            el.getAttribute('download') === 'millivolt-debug-20260914-000000.json.gz') &&
          dlTape.includes('revoke:blob:w16-dl'));
        sw.__dlResponse = { ok: true, blob: async () => new sw.Blob(['gz']), headers: { get: () => null } };
        dbgBtn.click();
        await sleep(30);
        check('a missing Content-Disposition falls back to the designed artifact name',
          dlTape.filter(el => el && el.tagName === 'A')
            .some(a => a.getAttribute('download') === 'millivolt-debug.json.gz'));
        sw.__dlResponse = { ok: false, status: 502, headers: { get: () => null } };
        dbgBtn.click();
        await sleep(30);
        check('a failed capture download surfaces the designed failure message in the section',
          sd.getElementById('drawer-debug-status').textContent === 'debug capture download failed');
      }
      sw.closeDrawer();

      // triggerDownload's anchor contract: href/download set, appended,
      // clicked, removed - and a blob URL is revoked after the click.
      {
        const tape = [];
        sd.addEventListener('click', e => tape.push(e.target));
        sw.URL.createObjectURL = () => 'blob:w16-fixture';
        sw.URL.revokeObjectURL = href => tape.push('revoke:' + href);
        sw.triggerDownload('/metrics/export?x=1', '');
        check('triggerDownload builds the anchor with href/download, clicks and removes it',
          tape.length === 1 && tape[0].tagName === 'A' &&
          tape[0].getAttribute('href') === '/metrics/export?x=1' &&
          tape[0].getAttribute('download') === '' && !tape[0].isConnected);
        sw.triggerDownload('blob:w16-fixture', 'millivolt-backup.mvb', true);
        check('a blob download names the file and revokes the URL after the click',
          tape.length === 3 && tape[1].tagName === 'A' &&
          tape[1].getAttribute('href') === 'blob:w16-fixture' &&
          tape[1].getAttribute('download') === 'millivolt-backup.mvb' &&
          tape[2] === 'revoke:blob:w16-fixture');
      }
    } finally { sw.close(); }
  }

  // ---- lazy Settings module ----
  // Production keeps settings.js out of the first-paint shell; the trigger
  // injects exactly one versioned request and the sheet opens only after that
  // module has initialized. These checks assemble without settings.js so the
  // real chrome.js trigger/loader path runs, then simulate the script element's
  // load/error events (jsdom does not fetch external scripts).
  {
    const shell = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
    check('production index.html has no settings.js script reference',
      !/<script\b[^>]*\bsrc=["'][^"']*\/dash\/js\/settings\.js/.test(shell));

    const lazyShell = async () => {
      const appended = [];
      const errors = [];
      const dom = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), {
        ...pageOptions,
        beforeParse(win) {
          pageOptions.beforeParse(win);
          win.console.error = (...args) => errors.push(args.join(' '));
          const append = win.HTMLHeadElement.prototype.appendChild;
          win.HTMLHeadElement.prototype.appendChild = function (node) {
            if (node && node.tagName === 'SCRIPT' &&
                String(node.getAttribute('src') || '').includes('/dash/js/settings.js')) {
              appended.push(node);
              return node;
            }
            return append.call(this, node);
          };
        },
      });
      await loaded(dom);
      return { dom, sw: dom.window, sd: dom.window.document, appended, errors };
    };

    const ok = await lazyShell();
    try {
      check('first paint issues no settings.js request', ok.appended.length === 0);
      check('the settings sheet starts closed before the module loads',
        ok.sd.getElementById('settings-sheet').hidden);
      // Escape and the header close path must be safe before the module exists.
      ok.sd.dispatchEvent(new ok.sw.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      ok.sw.closeHeaderMenus(undefined, true);
      ok.sw.closeSettings(true);
      check('close and Escape are safe no-ops before settings.js loads',
        ok.sd.getElementById('settings-sheet').hidden);

      ok.sw.toggleSettings({ stopPropagation() {} });
      ok.sw.toggleSettings({ stopPropagation() {} });
      ok.sw.toggleSettings({ stopPropagation() {} });
      const script = ok.appended[0];
      check('repeated and concurrent settings opens issue exactly one versioned request',
        ok.appended.length === 1 && script &&
        script.getAttribute('src') === '/dash/js/settings.js?v=' + TEST_DASHBOARD_VERSION);

      let opened = 0;
      ok.sw.settingsModule = { open() { opened++; }, close() {} };
      check('the sheet stays closed while the settings module is loading',
        ok.sd.getElementById('settings-sheet').hidden);
      script.onload();
      await sleep(0);
      check('the loaded settings module opens the sheet exactly once', opened === 1);
    } finally { ok.dom.window.close(); }

    // A close while the module is still downloading must cancel the pending
    // open; the loader resolving later must not pop the sheet back up.
    const cancelled = await lazyShell();
    try {
      let opened = 0;
      cancelled.sw.toggleSettings({ stopPropagation() {} });
      cancelled.sw.closeSettings(true);
      cancelled.sw.settingsModule = { open() { opened++; }, close() {} };
      cancelled.appended[0].onload();
      await sleep(0);
      check('closing during the settings load cancels the pending open', opened === 0);
      cancelled.sw.toggleSettings({ stopPropagation() {} });
      check('a later settings open uses the loaded module exactly once', opened === 1);
    } finally { cancelled.dom.window.close(); }

    // The chart metrics picker renders at first paint and shares its chevron
    // with the settings provider cards. It must not depend on settings.js.
    {
      const noSettings = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), pageOptions);
      try {
        await loaded(noSettings);
        const sd = noSettings.window.document;
        noSettings.window.eval("chartView.preset = 'overview'");
        noSettings.window.summaryPickerSync();
        const btn = sd.getElementById('chart-metrics-btn');
        check('the chart metrics picker renders before settings.js loads',
          !!btn && btn.innerHTML.includes('<svg') &&
          sd.getElementById('chart-metrics-count').textContent === '5/5');
      } finally { noSettings.window.close(); }
    }

    const failed = await lazyShell();
    try {
      failed.sw.toggleSettings({ stopPropagation() {} });
      check('a settings load attempt starts with the sheet closed',
        failed.appended.length === 1 && failed.sd.getElementById('settings-sheet').hidden);
      failed.appended[0].onerror();
      await sleep(0);
      check('a settings load failure stays closed and surfaces the shared status',
        failed.sd.getElementById('settings-sheet').hidden &&
        failed.sd.getElementById('settings-count').textContent === 'could not load config');
      check('a settings load failure reports a console error',
        failed.errors.some(line => line.includes('settings module failed to load')));
      failed.sw.toggleSettings({ stopPropagation() {} });
      await sleep(0);
      check('a failed settings load is not retried on the next open',
        failed.appended.length === 1);
    } finally { failed.dom.window.close(); }
  }

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
    await sleep(50);
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
        await sleep(120);
        check('the archive cap is terminal: one older-page request, no retained rows',
          urls.length === 1 && w.eval('logArchive.length') === 20000 &&
          !w.eval('logArchive.some(r => r.id === "cap-old")') && w.eval('logExhausted') === true);
        check('the terminal cap does not grow logLimit for discarded rows', w.eval('logLimit') === 20001);
        w.fetchOlderLog();
        await sleep(80);
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
        await sleep(10);
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
        await sleep(50);
      }
    }

    // F4: an open drawer follows the live finalization of its record without
    // rewriting on unrelated traffic, preserving scroll for the same identity.
    {
      const base = 1700003000000;
      fire('begin', { record: { ...mkRec('audit-drawer', 0, base) }, in_flight: 1 });
      await settleUntil(() => !!d.querySelector('#tbl-requests tr.exp-row[data-id="audit-drawer"]'));
      w.openDrawer('audit-drawer');
      const body = d.getElementById('drawer-body');
      body.scrollTop = 120;
      check('the drawer opens on the live request', /streaming/.test(body.textContent));
      fire('end', { record: { ...mkRec('audit-drawer', 200, base), duration_ms: 900, ttft_ms: 250 }, in_flight: 0 });
      await settleUntil(() => /900ms/.test(d.getElementById('drawer-body').textContent));
      check('an open drawer follows the live finalization',
        !/streaming/.test(d.getElementById('drawer-body').textContent) &&
        /900ms/.test(d.getElementById('drawer-body').textContent));
      check('the live drawer repaint preserves its scroll position', body.scrollTop === 120);
      const html = body.innerHTML;
      fire('record', { ...mkRec('audit-unrelated', 200, base + 2000) }, '900');
      await sleep(40);
      check('an unrelated live event does not rewrite the open drawer',
        body.innerHTML === html && body.scrollTop === 120);
      body.scrollTop = 120;
      w.openDrawer('audit-unrelated');
      check('a drawer identity change resets its scroll position', body.scrollTop === 0);
      w.closeDrawer();
    }

    // F6: row replacement, gallery repaint and legend activation restore
    // keyboard focus to the equivalent control through one shared helper.
    {
      const base = 1700004000000;
      const rowBtn = () => d.querySelector('#tbl-requests tr.exp-row[data-id="audit-focus"] button[data-open-req]');
      fire('begin', { record: { ...mkRec('audit-focus', 0, base) }, in_flight: 1 });
      await settleUntil(() => !!rowBtn());
      rowBtn().focus();
      check('the row control owns focus before the live replacement', d.activeElement === rowBtn());
      fire('end', { record: { ...mkRec('audit-focus', 200, base) }, in_flight: 0 });
      await settleUntil(() => {
        const next = rowBtn();
        return !!next && !next.closest('tr').classList.contains('live-row') && d.activeElement === next;
      });
      check('a live row replacement restores focus to the equivalent open control',
        d.activeElement === rowBtn() && !rowBtn().closest('tr').classList.contains('live-row'));
    }
    {
      const savedFetch = w.fetch;
      let version = 1;
      const ent = n => ({ name: 'focus-card.example', n, cost: 0, in: 10, out: 5, cache: 2, tools: 0,
        err_final: 0, rate_limit_requests: 0, cost_per_mtok: null,
        ttft_p50: null, ttft_p95: null, tps_p50: null, tps_p95: null });
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/explorer')) return savedFetch(url, opts);
        return Promise.resolve({ ok: true, json: async () => ({
          dim: 'provider', total: 3, error_total: 0, rail: { provider: 1 },
          groups: [ent(version)], scope: { matches: version, errors: 0 },
        }) });
      };
      const card = () => d.querySelector('#xp-gallery [data-xp-key="focus-card.example"]');
      try {
        w.navigateTo([{ dim: 'provider', id: 'focus-gallery.example' }]);
        await settleUntil(() => !!card() && /1req/.test(card().textContent));
        card().focus();
        version = 2;
        w.fetchExplorer('refresh');
        await settleUntil(() => !!card() && /2req/.test(card().textContent) && d.activeElement === card());
        check('a gallery repaint restores focus to the equivalent card', d.activeElement === card());
      } finally {
        w.fetch = savedFetch;
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('explorerAgg'), 3000);
      }
    }
    // F6 navigation: activating a gallery card changes scope, and the loading
    // repaint destroys that card before the matching payload exists. The focus
    // identity must survive both repaints; a card that vanishes from the
    // filtered payload must not hand focus to some other card.
    {
      const savedFetch = w.fetch;
      const ent = (name, n) => ({ name, n, cost: 0, in: 10, out: 5, cache: 2, tools: 0,
        err_final: 0, rate_limit_requests: 0, cost_per_mtok: null,
        ttft_p50: null, ttft_p95: null, tps_p50: null, tps_p95: null });
      const payload = groups => ({ dim: 'provider', total: 2, error_total: 0,
        rail: { provider: groups.length }, groups, scope: { matches: 2, errors: 0 } });
      let hold = false;
      const held = [];
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/explorer')) return savedFetch(url, opts);
        if (!hold) return Promise.resolve({ ok: true, json: async () => payload([ent('focus-nav.example', 2)]) });
        return new Promise(resolve => held.push({ resolve }));
      };
      const card = key => d.querySelector(`#xp-gallery [data-xp-key="${key}"]`);
      const prime = async () => {
        hold = false;
        w.navigateTo([{ dim: 'client', id: 'focus-nav-client' }]);
        // Wait for the hashchange's loading repaint and payload, not the old
        // scenario's card: the unfiltered card is the one without `selected`.
        await settleUntil(() => !!card('focus-nav.example') &&
          !card('focus-nav.example').classList.contains('selected'));
      };
      // A browser maps Enter on the focused button to the same click event the
      // delegated explorer handler consumes; jsdom has no implicit activation,
      // so the production path is invoked through click() from keyboard focus.
      const activate = () => {
        card('focus-nav.example').focus();
        hold = true;
        card('focus-nav.example').click();
        return settleUntil(() => held.length === 1 &&
          /loading breakdown/.test(d.getElementById('xp-gallery').textContent) &&
          !card('focus-nav.example'));
      };
      try {
        await prime();
        check('a gallery card activation paints the loading state and drops the old card',
          await activate() && d.activeElement === d.body);
        held.shift().resolve({ ok: true, json: async () => payload([ent('focus-nav.example', 3)]) });
        await settleUntil(() => d.activeElement === card('focus-nav.example'));
        check('the filtered payload restores focus to the equivalent gallery card',
          d.activeElement === card('focus-nav.example') && d.activeElement !== d.body);

        await prime();
        check('the second card activation paints the loading state again',
          await activate() && d.activeElement === d.body);
        held.shift().resolve({ ok: true, json: async () => payload([ent('focus-nav-other.example', 2)]) });
        await settleUntil(() => !!card('focus-nav-other.example') && !card('focus-nav.example'));
        check('a vanished gallery card does not move focus to a different card',
          !!card('focus-nav-other.example') && d.activeElement === d.body);
      } finally {
        hold = false;
        w.fetch = savedFetch;
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('explorerAgg'), 3000);
      }
    }
    {
      w.eval("chartView.preset = 'traffic'; chartView.hidden = {}; chartView.pct = 95; chartView.window = 'all'; fillChartControls(); renderChart();");
      const leg = () => d.querySelector('#traffic-legend .leg-item[data-series="err"]');
      const before = leg();
      before.focus();
      before.click();
      const after = leg();
      check('a legend activation restores focus to the equivalent toggle',
        before !== after && after.classList.contains('off') && d.activeElement === after);
      after.click();
      await sleep(0);
    }
    {
      const legend = d.getElementById('traffic-legend');
      const item = legend.querySelector('.leg-item');
      item.focus();
      const token = typeof w.captureFocus === 'function' ? w.captureFocus(legend, el => {
        const series = el.closest('.leg-item[data-series]')?.dataset.series;
        return series == null ? null : `[data-series="${w.CSS.escape(series)}"]`;
      }) : null;
      const other = d.getElementById('btn-pause');
      other.focus();
      const restored = typeof w.restoreFocus === 'function' ? w.restoreFocus(token) : false;
      check('the shared focus restore never steals focus from a moved operator',
        !!token && restored === false && d.activeElement === other);
    }

    // F13: owner cleanup and stable focus identities. A competing prompt must
    // not leave the Settings veil/pressed state behind; removing a scope chip
    // must land on its surviving neighbor/root; a drawer whose initiating row
    // was replaced must return focus to the equivalent control.
    {
      const sheet = d.getElementById('settings-sheet');
      const veil = d.getElementById('settings-veil');
      const settingsBtn = d.getElementById('btn-settings');
      if (w.settingsIsOpen()) w.closeSettings(true);
      w.openSettings();
      await settleUntil(() => w.settingsIsOpen() && sheet.classList.contains('open'));
      await sleep(20);
      const prompt = w.askOperatorToken();
      await sleep(0);
      check('an operator prompt preempts open Settings without a stale veil or pressed trigger',
        !w.settingsIsOpen() && !sheet.classList.contains('open') && !veil.classList.contains('open') &&
        settingsBtn.getAttribute('aria-pressed') === 'false' &&
        !d.getElementById('operator-dialog').hidden);
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      await prompt;
      check('the Settings takeover cleanup survives prompt cancel',
        !sheet.classList.contains('open') && !veil.classList.contains('open') &&
        settingsBtn.getAttribute('aria-pressed') === 'false' &&
        d.getElementById('operator-dialog').hidden && !d.querySelector('main').hasAttribute('inert'));
    }
    {
      w.navigateTo([{dim: 'provider', id: 'chip-focus.example'}, {dim: 'client', id: 'c'}]);
      const chips = () => [...d.querySelectorAll('#xp-scope .chip-x')];
      await settleUntil(() => chips().length === 2);
      const first = chips()[0];
      first.focus();
      first.click();
      const survivor = await settleUntil(() => chips().length === 1 && d.activeElement === chips()[0]);
      check('removing a scope chip focuses the surviving neighbor',
        survivor && chips()[0].dataset.chipDim === 'client');
      const last = chips()[0];
      last.focus();
      last.click();
      const rootFocused = await settleUntil(() => d.activeElement === d.querySelector('#xp-scope .bc-root'));
      check('removing the last scope chip falls back to the all-traffic root',
        rootFocused && chips().length === 0);
      w.navigateTo([]);
      await settleUntil(() => w.eval('explorerFilters().length') === 0);
    }
    {
      const base = 1700007000000;
      const rowBtn = () => d.querySelector('#tbl-requests tr.exp-row[data-id="focus-drawer"] button[data-open-req]');
      fire('begin', { record: { ...mkRec('focus-drawer', 0, base) }, in_flight: 1 });
      await settleUntil(() => !!rowBtn());
      rowBtn().focus();
      rowBtn().click();
      await settleUntil(() => !d.getElementById('drawer').hidden);
      const oldBtn = rowBtn();
      fire('end', { record: { ...mkRec('focus-drawer', 200, base), duration_ms: 100 }, in_flight: 0 });
      await settleUntil(() => {
        const next = rowBtn();
        return !!next && next !== oldBtn && !next.closest('tr').classList.contains('live-row');
      });
      check('the drawer initiating control was replaced while the drawer stayed open',
        !oldBtn.isConnected && !!rowBtn());
      const newBtn = rowBtn();
      w.closeDrawer();
      check('closing the drawer after row replacement restores the equivalent control',
        d.activeElement === newBtn);
    }

    // F9: a page opened in a hidden tab must not arm the clock or poll before
    // its first visible transition; that transition runs the resume path.
    {
      let hidden = true;
      let hiddenBoot = 0, hiddenChart = 0, hiddenExplorer = 0;
      const hiddenDom = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), {
        url: 'http://127.0.0.1:8081/',
        runScripts: 'dangerously',
        pretendToBeVisual: true,
        beforeParse(window) {
          Object.defineProperty(window.document, 'hidden', { configurable: true, get: () => hidden });
          window.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
          window.matchMedia = () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} });
          window.CSS = window.CSS || {};
          if (!window.CSS.escape) window.CSS.escape = s => String(s).replace(/[\\"]/g, '\\$&');
          Object.defineProperty(window.HTMLElement.prototype, 'offsetHeight', { get() { return 20; }, configurable: true });
          window.Path2D = class { moveTo() {} lineTo() {} arcTo() {} arc() {} rect() {} closePath() {} bezierCurveTo() {} };
          window.HTMLCanvasElement.prototype.getContext = () => new Proxy({}, {
            get: (t, k) => (k === 'measureText' ? () => ({ width: 1 }) :
              k === 'canvas' ? { width: 0, height: 0 } :
              (typeof k === 'string' ? () => {} : undefined)),
            set: () => true,
          });
          window.EventSource = function (url) {
            return { url, readyState: 1, addEventListener() {}, close() { this.readyState = 2; } };
          };
          window.fetch = url => {
            const u = String(url);
            if (u.includes('/metrics/bootstrap')) {
              hiddenBoot++;
              return Promise.resolve({ ok: true, json: async () => ({
                records: [], in_flight_records: [], counters: { in_flight: 0 },
                pending_revision: 0, seq: 0, feed_id: 'hidden-feed', incremental: false,
                kpi: { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null,
                  input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0,
                  avg_ttft_ms: null, avg_tps: null },
                dashboard_version: TEST_DASHBOARD_VERSION,
                model_canon: { rules: [], names: {} },
                dash: {}, storage: { enabled: true, dropped: 0, totals_degraded: false },
                pause: { ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: [], known_providers: [], until: null, queued: 0, default_max_queued: 0 },
                throttle: { ok: true, throttles: [], known_providers: [], active: false },
                debug: { ok: true, enabled: false, sessions: [], known_clients: [], known_providers: [], known_models: [], until: null, ttl: '24h', max_bytes: '1MiB' },
              }) });
            }
            if (u.includes('/metrics/agg/chart')) {
              hiddenChart++;
              return Promise.resolve({ ok: true, json: async () => ({
                now_ms: Date.now(), from_ms: Date.now() - 3600000, bucket_ms: 60000,
                ttft_p: [null, null, null], tps_p: [null, null, null], dec_p: [null, null, null],
                buckets: [],
              }) });
            }
            if (u.includes('/metrics/agg/explorer')) {
              hiddenExplorer++;
              return Promise.resolve({ ok: true, json: async () => ({
                dim: 'provider', total: 0, error_total: 0, rail: { provider: 0 }, groups: [],
                scope: { matches: 0, errors: 0 },
              }) });
            }
            if (u.includes('/metrics/agg/log')) return Promise.resolve({ ok: true, json: async () => ({ records: [], more: false, cursor_ms: 0, cursor_id: '' }) });
            return Promise.resolve({ ok: true, json: async () => ({}) });
          };
        },
      });
      await loaded(hiddenDom);
      const hw = hiddenDom.window;
      const hd = hw.document;
      await sleep(300);
      check('a page loaded hidden keeps its clock and poll timers unarmed',
        hw.eval('_wasHidden === true && _clockTimer === null && _dashTickTimer === null'));
      check('a hidden-at-load page completes only its initial paint fetches',
        hiddenBoot === 1 && hiddenChart === 1 && hiddenExplorer === 1);
      const bootBefore = hiddenBoot;
      hidden = false;
      hd.dispatchEvent(new hw.Event('visibilitychange'));
      await sleep(80);
      check('the visible transition arms both timers and refreshes the page',
        hw.eval('_wasHidden === false && _clockTimer !== null && _dashTickTimer !== null') &&
        hiddenBoot > bootBefore);
      hiddenDom.window.close();
    }
  }


  reportFailures(false);
  process.exit(state.failures.length ? 1 : 0);
}
// A crashed run must not swallow what it already knows: without this
// handler the process dies on the raw exception and the accumulated
// failures, the diagnostic dump and the check history never print.
main().catch(err => {
  console.error(err);
  if (state.diagDump) console.log(state.diagDump('crash: ' + (err && err.message)));
  reportFailures(true);
  process.exit(1);
});
