// NONE_YET_NOTE: the operator menus' one empty-state sentence
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function operatorMenus(H) {
  const {check, d, modelFixture, observerFixture, w} = H;
  const state = H.state;

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
};
