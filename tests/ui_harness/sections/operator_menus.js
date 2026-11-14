// The five header operator menus were replaced by one operator console. This
// section drives the console's contract end to end: the always-visible header
// readout, the one panel lifecycle, the section rail, the three policy
// composers, export, overview, system, the shared error surface and the
// record entry points. It runs early, so it leaves every operator surface on
// the empty baseline the later sections expect.
module.exports = async function operatorMenus(H) {
  const {check, d, mkRec, modelFixture, observerFixture, settleUntil, w} = H;
  const state = H.state;

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
        out() === 'paused 1 · capture 1 · limits 1 · offline' &&
        trigger.dataset.state === 'stale');
      // The connectivity owner repaints both surfaces, so a reconnect clears
      // the stale words from the header without waiting for a policy poll.
      w.eval('setFooterLive(true)');
      w.refreshOperatorReadout();
      check('a reconnect clears the offline words', out() === 'paused 1 · capture 1 · limits 1');
      const activeText = 'paused 1 · capture 1 · limits 1';
      // A hold outranks a routine cap in the readout's tone: it is the one
      // state that stops live traffic.
      check('one hold, one capture session and one limit read as three segments',
        out() === activeText && trigger.dataset.state === 'paused' &&
        d.getElementById('op-readout').dataset.state === 'paused');
      check('the readout trigger names the same words to assistive tech',
        trigger.getAttribute('aria-label') ===
          'operator state: ' + activeText + '. Open the operator console.');
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
      w.eval('restartServerState = {}');
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
      check('a limit outside its schedule is named as scheduled, not counted as a cap',
        out() === 'limits 1 · scheduled 1' &&
        evalJSON('ocRuleCounts().limits') === 1 &&
        evalJSON('ocRuleCounts().parked') === 1);
      w.applyThrottleState(EMPTY_THROTTLE);
      w.refreshOperatorReadout();
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
            '3 in flight · 10 requests / 1m · 1K tokens / 1m · SM · 22:00-06:00');
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
        w.eval("ocDrafts.pause.clients = ['client-a']; ocRender()");
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
          check('the pause head offers a one-click hold with its bound in the label',
            text('#oc-pane [data-oc-quick-hold]') === 'hold 15m');
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
        text('#oc-pause-preview').includes('Pick at least one client or provider'));

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
        check('a named hold posts both explicit dimensions',
          await settleUntil(() => posts.length === 1) &&
          stable(posts[0]) === stable({paused: true, all: false, new: false,
            clients: ['client-a'], providers: ['prov-a'], duration: ''}));
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
        input('#oc-pane [data-oc-field="pauseCap"]', '-1');
        click('#oc-pause-apply');
        check('a negative queue cap is refused with no POST',
          await settleUntil(() => !!q('#oc-pane .oc-error')) &&
          text('#oc-pane .oc-error').includes('queue cap') && posts.length === 2);
        input('#oc-pane [data-oc-field="pauseCap"]', '8');
        click('#oc-pause-apply');
        check('a valid queue cap travels as max_queued',
          await settleUntil(() => posts.length === 3) && posts[2].max_queued === 8);

        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'hold-r1', all: true}], queued: 0,
          known_clients: [], known_providers: [], until: null, default_max_queued: 0});
        click('#oc-pane [data-oc-resume="hold-r1"]');
        check('resuming one hold posts only its id',
          await settleUntil(() => posts.length === 4) &&
          stable(posts[3]) === stable({paused: false, id: 'hold-r1'}));
        check('the resume is recorded as resumed',
          await settleUntil(() => text('#oc-change').startsWith('resumed ')) &&
          text('#oc-change') === 'resumed all traffic');

        await settleUntil(() => w.eval('operatorState.pause.busy') === false);
        w.applyPauseState({ok: true, paused: true,
          holds: [{id: 'hold-a', all: true}, {id: 'hold-b', clients: ['client-a']}],
          queued: 2, known_clients: ['client-a'], known_providers: [],
          until: null, default_max_queued: 0});
        click('#oc-pane [data-oc-resume-all]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        check('resuming every hold asks for confirmation first',
          text('#oc-confirm-title') === 'Resume every hold?' &&
          d.querySelector('.oc-confirm-body').textContent.includes('2 holds are') &&
          posts.length === 4);
        click('#oc-confirm-dialog [data-oc-confirm="cancel"]');
        check('cancelling resume-all keeps every hold', w.eval('pauseState.holds.length') === 2);
        click('#oc-pane [data-oc-resume-all]');
        await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
        click('#oc-confirm-dialog [data-oc-confirm="ok"]');
        check('confirming resume-all posts the global resume',
          await settleUntil(() => posts.length === 5) &&
          stable(posts[4]) === stable({paused: false}) &&
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
        text('#oc-capture-preview') === 'Pick at least one client, provider or model.');
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
          return {ok: true, json: async () => debugDoc({ok: true,
            enabled: sessions.length > 0, sessions,
            known_clients: ['client-a'], known_providers: ['prov-a'], known_models: ['model-a'],
            ttl: '24h', max_bytes: '1MiB'})};
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

        await settleUntil(() => w.eval('operatorState.debug.busy') === false);
        click('#oc-pane [data-oc-stop="sess-created"]');
        check('stopping one capture posts only its id',
          await settleUntil(() => posts.length === 2) &&
          stable(posts[1]) === stable({enabled: false, id: 'sess-created'}));
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
          posts.length === 2);
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

      input('#oc-pane [data-oc-list-filter="limits"]', 'nothing matches this');
      check('a filter that hides every rule says so instead of reading as empty',
        qa('#oc-pane .oc-rule').length === 0 &&
        text('#oc-pane .oc-empty').includes('No rule matches that filter') &&
        text('#oc-pane .oc-empty').includes('8 rules are in force'));
      click('#oc-pane [data-oc-list-clear="limits"]');
      check('clearing the filter restores every rule and the filter row',
        qa('#oc-pane .oc-rule').length === 8 &&
        q('#oc-pane [data-oc-list-filter="limits"]').value === '');
      w.closeOperatorConsole();
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
        check('all history previews the same record count as a filter',
          await settleUntil(() => text('#oc-export-receipt') === '7 records · JSON, gzip', 3000) &&
          countBody && countBody.provider === '' && !d.getElementById('oc-export-download').disabled &&
          countCalls >= 1);
        click('#oc-pane [data-oc-download]');
        check('all history downloads the bare export URL and names the count',
          JSON.stringify(downloads[0]) === JSON.stringify(['/metrics/export', '']) &&
          text('#oc-change') === 'downloaded 7 records');

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
            countBody && countBody.provider === 'prov-a' && countBody.model === '' &&
            countBody.client === '' && countBody.conversation_id === '' &&
            countBody.status_code === 0 && countBody.has_error === false &&
            countBody.debug === false);
          click('#oc-pane [data-oc-download]');
          check('the filtered download uses the exact previewed URL',
            JSON.stringify(downloads[1]) ===
              JSON.stringify(['/metrics/export?provider=prov-a', '']) &&
            text('#oc-change') === 'downloaded 7 records');
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
            countBody && countBody.provider === '');
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
        JSON.stringify(evalJSON('ocDrafts.pause.clients')) === JSON.stringify(['client-a']) &&
        JSON.stringify(evalJSON('ocDrafts.pause.providers')) === JSON.stringify(['prov-a']) &&
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
        JSON.stringify(evalJSON('ocDrafts.capture.clients')) === JSON.stringify(['client-a']) &&
        JSON.stringify(evalJSON('ocDrafts.capture.models')) === JSON.stringify(['model-a']));

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
