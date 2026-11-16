// const canonCases = [
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function modelCanon(H) {
  const {DEF_RULES, cfgDoc, check, d, modelFixture, path, rows, settleUntil, w} = H;
  const state = H.state;

  // Pane controls are repainted by the page's own poll, so a press waits for its
  // node instead of dereferencing whatever the last render left behind.
  const clickWait = async sel => {
    await settleUntil(() => !!d.querySelector(sel));
    d.querySelector(sel).click();
  };

  const canonCases = [
    ['glm-5.3', 'glm-5-3'], ['glm-5-3', 'glm-5-3'], ['GLM-5.3', 'glm-5-3'],
    ['moonshotai/kimi-k3', 'kimi-k3'], ['moonshotai/kimi-k3:nube', 'kimi-k3'],
    ['z-ai/glm-5.3-flash:crofai', 'glm-5-3-flash'],
    ['deploy-ai/deepseek-ai/deepseek-v4.1-flash-1017', 'deepseek-v4-1-flash'],
    ['deepseek-v4-pro-0813', 'deepseek-v4-pro'],
    ['deepseek-v4.1-flash-fast', 'deepseek-v4-1-flash'],
    ['deepseek-v4.1-flash-fast-fp4', 'deepseek-v4-1-flash'],
    ['deepseek-v4.1-flash-value', 'deepseek-v4-1-flash'],
    ['step-5-preview-free', 'step-5'],
    ['glm-5.3-flash', 'glm-5-3-flash'],
    ['deepseek-v4.1-flash-1017-abliterated', 'deepseek-v4-1-flash-1017-abliterated'],
    ['Qwen/Qwen2.5-72B-Instruct-GPTQ-Int4', 'qwen2-5-72b-instruct'],
    ['cursor-grok-4.6-xhigh-fast', 'cursor-grok-4-6'],
    ['grok-4-fast', 'grok-4'],
    ['glm-5.3-fp4', 'glm-5-3'], ['glm-5.3-nvfp4', 'glm-5-3'], ['GLM-5.3-NXFP4', 'glm-5-3'],
    ['llama-3.3-70b-int8', 'llama-3-3-70b'], ['qwen2.5-72b-q4_k_m', 'qwen2-5-72b'],
    ['gemma-3-27b', 'gemma-3-27b'],
    ['gpt-4.1-mini', 'gpt-4-1-mini'],
    ['  glm-5.3 ', 'glm-5-3'],
    ['z-ai/', 'z-ai/'],
  ];

  const canonNames = Object.fromEntries(canonCases.concat([
    ['glm-5.3-flash', 'glm-5-3-flash'], ['glm-5-3-flash', 'glm-5-3-flash'],
    ['kimi-k3', 'kimi-k3'], ['deepseek-v4-pro', 'deepseek-v4-pro'], ['grok-4.6', 'grok-4-6'],
  ]));
  w.applyModelCanon(modelFixture(DEF_RULES, canonNames), true);
  // The console capture composer is the one model picker: known models are
  // folded into canonical groups and every raw spelling is offered, because a
  // capture scopes the EXACT spelling the server stored. Selecting one variant
  // writes that spelling into capture.models, never the canonical group name.
  // ocKnownValues('models') reads the throttle state's known vocabulary; it is
  // seeded directly here because applyThrottleState currently drops the
  // server's known_clients/known_models fields (reported separately).
  {
    const known = ['glm-5-3', 'glm-5.3', 'glm-5-3-flash', 'glm-5.3-flash', 'kimi-k3',
      'moonshotai/kimi-k3', 'moonshotai/kimi-k3:nube', 'deepseek-v4-pro',
      'deepseek-v4-pro-0813', 'grok-4.6'];
    w.__knownModels = known;
    w.eval('throttleState = {...throttleState, known_models: window.__knownModels}');
    w.openOperatorConsole('capture');
    await clickWait('#oc-pane [data-oc-new="capture"]');
    await clickWait('#oc-pane [data-oc-pick="models"]');
    const rows = [...d.querySelectorAll('#oc-pane [data-oc-pick-value="models"]')]
      .map(row => row.dataset.ocValue);
    const grouped = w.eval('groupByCanonical(window.__knownModels).flatMap(g => g.variants).sort()');
    check('the console capture picker lists every canonical-group variant',
      JSON.stringify(rows) === JSON.stringify(grouped) && rows.length === 10 &&
      rows.includes('glm-5.3') && rows.includes('glm-5-3') &&
      rows.includes('moonshotai/kimi-k3:nube'));
    // The picker's declared selection contract: a draft carrying the exact raw
    // spelling marks that variant checked. (The click-to-toggle path for
    // pause/capture drafts is broken in production and reported separately;
    // this pins the value/check contract without hiding it.)
    w.ocOpenOperatorForRecord('capture', {
      id: 'model-pick', provider: 'p', client: 'c', model: 'moonshotai/kimi-k3:nube',
    });
    await clickWait('#oc-pane [data-oc-pick="models"]');
    check('a capture draft keeps the exact model spelling and checks its picker row',
      JSON.stringify(w.eval('ocDrafts.capture.scope.models')) === JSON.stringify(['moonshotai/kimi-k3:nube']) &&
      d.querySelector('#oc-pane [data-oc-pick-value="models"][data-oc-value="moonshotai/kimi-k3:nube"]')
        .getAttribute('aria-checked') === 'true');
    w.closeOperatorConsole();
    w.eval("ocDrafts.capture = null; ocView = {kind: '', id: ''}");
    delete w.__knownModels;
    w.eval('throttleState = {...throttleState, known_models: undefined}');
  }

  // ---- model canonicalization: the observer dictionary contract ----
  // The same case table as Go's TestCanonicalModel pins the lockstep
  // contract. The rules are a fully data-driven ordered pipeline
  // (model_rules - exact | pattern | lower steps, editable in Settings with
  // a live preview) served in every bootstrap's model_canon.rules section
  // and applied everywhere models are GROUPED - the explorer model
  // dimension/scope (recordMatchesDim), the debug checklist, and the
  // Clear/Logs model optgroups. Records, the log leaf, and purge/export keep
  // the exact stored spelling; the server's debug-session matching folds
  // both sides through the cursor base name (debug.go modelIn).
  check('canonicalModel mirrors config.ApplyModelRules (default pipeline)',
    canonCases.every(([raw, want]) => w.canonicalModel(raw) === want));
  w.applyModelCanon(modelFixture([{mode: 'exact', from: 'deepseek-v4-1-flash-1017-abliterated', to: 'deepseek-v4-1-flash'}].concat(DEF_RULES), {...canonNames, 'deepseek-v4-1-flash-1017-abliterated': 'deepseek-v4-1-flash'}), true);
  check('an exact rule merges what the generic pipeline cannot',
    w.canonicalModel('deepseek-v4-1-flash-1017-abliterated') === 'deepseek-v4-1-flash' &&
    w.eval("canonicalModel('GLM-5.3')") === 'glm-5-3');
  w.applyModelCanon(modelFixture(), true);
  check('an empty rule pipeline = identity grouping', w.canonicalModel('GLM-5.3') === 'GLM-5.3' && w.canonicalModel(' glm-5.3 ') === 'glm-5.3');
  w.applyModelCanon(modelFixture(DEF_RULES, canonNames), true);
  // The request-log leaf DISPLAYS the canonical name (grouped like every
  // other surface); the original stays one hover away (title) and in full
  // view in the request detail (Parameters shows the raw spelling).
  {
    const rowHTML = w.reqRow({ id: 'x1', start: Date.now(), client: 'cli', provider: 'prov', model: 'glm-5.3', status_code: 200, live: false });
    check('the log row shows the canonical model with the original on hover',
      rowHTML.includes('#/model/glm-5-3') && rowHTML.includes('title="original: glm-5.3"'));
    const detailHTML = w.formatDetail({ id: 'x1', client: 'cli', provider: 'prov', model: 'glm-5.3', path: '/v1/chat/completions', method: 'POST' });
    check('the request detail keeps the original spelling with a groups-as hint',
      detailHTML.includes('>glm-5.3 <span') && detailHTML.includes('groups as glm-5-3'));
  }
  check('the log-scope mirror matches raw spellings through the canonical group',
    w.recordMatchesDim({ model: 'glm-5.3' }, 'model', 'glm-5-3') === true &&
    w.recordMatchesDim({ model: 'glm-5-3' }, 'model', 'glm-5-3') === true &&
    w.recordMatchesDim({ model: 'grok-4.6' }, 'model', 'glm-5-3') === false);
  // The capture picker re-groups through the SAME rules: with an exact rule
  // live, the two deepseek spellings fold into one canonical group, but each
  // raw variant stays selectable so the wire keeps the exact spelling.
  w.applyModelCanon(modelFixture([{mode: 'exact', from: 'deepseek-v4-1-flash-1017-abliterated', to: 'deepseek-v4-1-flash'}].concat(DEF_RULES), {...canonNames, 'deepseek-v4-1-flash-1017-abliterated': 'deepseek-v4-1-flash'}), true);
  {
    w.__knownModels = ['deepseek-v4-1-flash', 'deepseek-v4-1-flash-1017-abliterated', 'grok-4.6'];
    w.eval('throttleState = {...throttleState, known_models: window.__knownModels}');
    w.openOperatorConsole('capture');
    await clickWait('#oc-pane [data-oc-new="capture"]');
    await clickWait('#oc-pane [data-oc-pick="models"]');
    check('a config rule groups the capture picker variants under one canonical key',
      w.eval("groupByCanonical(window.__knownModels).length") === 2 &&
      d.querySelectorAll('#oc-pane [data-oc-pick-value="models"]').length === 3);
    w.closeOperatorConsole();
    w.eval("ocDrafts.capture = null; ocView = {kind: '', id: ''}");
    delete w.__knownModels;
    w.eval('throttleState = {...throttleState, known_models: undefined}');
  }
  w.applyModelCanon(modelFixture(DEF_RULES, canonNames), true);
  w.eval("debugState.known_models = ['m']");
  // The shared delete/download model select (populateFilterMenu fills the
  // settings sheet's delete block, the surviving filter surface): spelling
  // variants cluster under a canonical <optgroup>, but every <option> keeps
  // its RAW value (exact delete).
  const filterHost = d.createElement('div');
  filterHost.innerHTML = w.eval('settingsDeleteRowHTML()');
  d.body.appendChild(filterHost);
  let modelSelHTML, clientSelHTML;
  try {
    modelSelHTML = w.eval(`(() => {
      const saved = lastData;
      lastData = { records: [{ model: 'glm-5.3' }, { model: 'glm-5-3' }, { model: 'grok-4.6' }] };
      populateFilterMenu('del');
      const html = document.getElementById('del-model').innerHTML;
      lastData = saved;
      return html;
    })()`);
  } finally { filterHost.remove(); }
  check('filter model options cluster variants under canonical optgroups',
    modelSelHTML.includes('<optgroup label="glm-5-3 (2)"') &&
    modelSelHTML.includes('value="glm-5.3"') && modelSelHTML.includes('value="glm-5-3"') &&
    modelSelHTML.includes('value="grok-4.6"') && !modelSelHTML.includes('value="glm-5-3 (2)"'));
  const clientHost = d.createElement('div');
  clientHost.innerHTML = w.eval('settingsDeleteRowHTML()');
  d.body.appendChild(clientHost);
  try {
    clientSelHTML = w.eval(`(() => {
      const saved = lastData;
      lastData = { records: [{ client: 'opencode/1.18.32' }, { client: 'team/2fa' }] };
      populateFilterMenu('del');
      const html = document.getElementById('del-client').innerHTML;
      lastData = saved;
      return html;
    })()`);
  } finally { clientHost.remove(); }
  check('client filter labels show the recorded identity while preserving exact values',
    clientSelHTML.includes('<option value="opencode/1.18.32">opencode/1.18.32</option>') &&
    clientSelHTML.includes('<option value="team/2fa">team/2fa</option>'));
  const clientDrawerTitle = w.eval(`(() => {
    const saved = lastData, savedDrawer = drawerId;
    lastData = { records: [{ id: 'client-label', client: 'opencode/1.18.32', provider: 'provider.example', model: 'space-bunny-free' }] };
    drawerId = 'client-label';
    renderDrawer();
    const title = document.getElementById('drawer-title').textContent;
    lastData = saved;
    drawerId = savedDrawer;
    closeDrawer();
    return title;
  })()`);
  check('drawer title uses the shared client label',
    clientDrawerTitle.startsWith('opencode/1.18.32 · provider.example · space-bunny-free'));
  // refreshes the server aggregates; an unchanged payload does neither.
  {
    const rev0 = w.eval('lastData._rev');
    w.applyModelCanon(modelFixture([{mode: 'exact', from: 'x/y', to: 'y'}].concat(DEF_RULES), {...canonNames, 'x/y': 'y'}), true);
    check('a canonicalization change bumps the derivation', w.eval('lastData._rev') > rev0);
    const rev1 = w.eval('lastData._rev');
    w.applyModelCanon(modelFixture([{mode: 'exact', from: 'x/y', to: 'y'}].concat(DEF_RULES), {...canonNames, 'x/y': 'y'}), true);
    check('an unchanged rule set does not re-derive', w.eval('lastData._rev') === rev1);
    w.applyModelCanon(modelFixture(DEF_RULES, canonNames), true);
  }
  // Settings rules editor: renders the pipeline as ordered rows, collects
  // them back to the wire shape, and previews the draft live.
  {
    const mrWrap = d.createElement('div');
    mrWrap.id = 'mr-test-wrap';
    mrWrap.innerHTML = w.modelRulesEditorHTML([
      { mode: 'lower' },
      { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' },
    ]);
    d.getElementById('settings-fields').appendChild(mrWrap);
    const wrap = mrWrap;
    const rows = [...wrap.querySelectorAll('.mr-row')];
    check('the rules editor renders ordered rule rows',
      rows.length === 2 && rows[0].querySelector('.mr-mode').value === 'lower' &&
      rows[1].querySelector('.mr-from').value === ':[a-z0-9._-]+$');
    check('a lower rule disables its from/to inputs',
      rows[0].querySelector('.mr-from').disabled === true &&
      rows[1].querySelector('.mr-from').disabled === false);
    const collected = w.collectModelRules(wrap);
    check('collectModelRules reads the rows into the wire shape',
      JSON.stringify(collected) === JSON.stringify([{ mode: 'lower' }, { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' }]));
    // live preview: type a spelling, expect the draft pipeline's output
    const testIn = wrap.querySelector('.mr-test');
    testIn.value = 'MoonshotAI/kimi-k3:Nube';
    testIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    w.mrPreview(wrap);
    check('the editor previews the draft pipeline live',
      wrap.querySelector('.mr-out').textContent === 'moonshotai/kimi-k3:nube');
    // add a rule row through the add-row inputs
    wrap.querySelector('.mr-add .mr-mode').value = 'pattern';
    wrap.querySelector('.mr-add .mr-new-from').value = '(\\d)\\.(\\d)';
    wrap.querySelector('.mr-add .mr-new-to').value = '$1-$2';
    w.addModelRuleRow(wrap.querySelector('.mr-add'));
    check('the add-row appends a validated rule',
      wrap.querySelectorAll('.mr-row').length === 3 &&
      wrap.querySelectorAll('.mr-row')[2].querySelector('.mr-from').value === '(\\d)\\.(\\d)');
    // a broken pattern is refused (deny by default - the server would
    // reject it at load)
    wrap.querySelector('.mr-add .mr-new-from').value = 'a(';
    w.addModelRuleRow(wrap.querySelector('.mr-add'));
    check('an invalid pattern is refused at the add-row',
      wrap.querySelectorAll('.mr-row').length === 3);
    // server-only syntax (info) is ACCEPTED - valid to save, preview skips
    wrap.querySelector('.mr-add .mr-new-from').value = '(?i)GLM';
    wrap.querySelector('.mr-add .mr-new-to').value = 'glm';
    w.addModelRuleRow(wrap.querySelector('.mr-add'));
    const infoRows = [...wrap.querySelectorAll('.mr-row')];
    check('server-only syntax is accepted at the add-row and marked info',
      infoRows.length === 4 && infoRows[3].dataset.mrState === 'info');
    infoRows[3].querySelector('[data-mr-rm]').click();
    // reorder: move the last rule up
    wrap.querySelectorAll('.mr-row')[2].querySelector('[data-mr-up]').click();
    check('reordering moves a rule up',
      wrap.querySelectorAll('.mr-row')[1].querySelector('.mr-from').value === '(\\d)\\.(\\d)');
    // remove
    wrap.querySelector('[data-mr-rm]').click();
    check('remove deletes the rule row',
      wrap.querySelectorAll('.mr-row').length === 2);
    mrWrap.remove();
  }
  // RE2-compat lint: JavaScript RegExp and the server's regex engine
  // (Go regexp - RE2) have different feature sets. The lint flags
  // JS-valid-but-RE2-invalid syntax as ERROR (blocks Apply), Go-valid-but-
  // JS-unrunnable syntax as INFO (the browser preview skips it), and
  // valid-everywhere-but-different semantics as WARN.
  {
    const L = q => { const r = w.re2Lint(q); return r ? r.level : null; };
    check('re2Lint flags JS-only regex syntax the server would reject',
      L('x(?=y)') === 'error' && L('(?<=a)b') === 'error' && L('a\\1') === 'error' &&
      L('\\u1234') === 'error' && L('\\cA') === 'error' && L('[\\b]') === 'error' &&
      L('a{2000}') === 'error' && L('(?<=a)b') === 'error');
    check('re2Lint accepts escapes both engines share (no false positives)',
      L('a\\12') === null && L('(\\d)\\.(\\d)') === null && L('^[a-z0-9]+/x$') === null);
    check('re2Lint marks server-only syntax as info for the preview to skip',
      L('(?i)abc') === 'info' && L('(?P<n>x)') === 'info' && L('[[:alpha:]]') === 'info' &&
      L('\\Afoo') === 'info' && L('\\p{L}+') === 'info');
    check('re2Lint warns on the \\s semantic drift', L('\\s+') === 'warn');
  }
  // Live row validation: rows render their own inline state - errors block
  // Apply (validateModelRulesDraft), duplicates warn, parked rows skip.
  {
    const mrWrap = d.createElement('div');
    mrWrap.innerHTML = w.modelRulesEditorHTML([
      { mode: 'pattern', from: 'x(?=y)' },
      { mode: 'exact', from: ' padded ' },
      { mode: 'exact', from: 'glm-5-3', to: 'glm-5.3' },
      { mode: 'exact', from: 'glm-5-3', to: 'glm-5.4' },
      { mode: 'pattern', from: '(?i)GLM' },
    ]);
    d.getElementById('settings-fields').appendChild(mrWrap);
    const wrap = mrWrap;
    const gate = w.validateModelRulesDraft(wrap);
    const states = [...wrap.querySelectorAll('.mr-row')].map(r => r.dataset.mrState);
    check('live validation marks RE2-invalid and padded rows as errors',
      states[0] === 'error' && states[1] === 'error' && !gate.ok && gate.firstBad === wrap.querySelectorAll('.mr-row')[0]);
    check('duplicate exact spellings warn (never runs) without blocking',
      states[2] === 'ok' && states[3] === 'warn');
    check('server-only syntax rows are info, not errors', states[4] === 'info');
    check('error rows show inline consequence text and a red input',
      wrap.querySelector('.mr-row[data-mr-state="error"] .mr-err').textContent.length > 0 &&
      wrap.querySelector('.mr-row[data-mr-state="error"] .mr-from').classList.contains('prov-bad'));
    const steps = w.mrDraftSteps(wrap);
    check('error and info rows are skipped by the browser preview pipeline',
      steps.length === 2 && steps.every(st => st.label === 'glm-5-3'));
    // park both broken rows: the gate clears without deleting work. The
    // park control's title must equal the freshly-rendered template's
    // title for the same state before and after the toggle (the template
    // and the toggle handler share one title pair).
    const visBtn = wrap.querySelectorAll('.mr-row')[0].querySelector('[data-mr-vis]');
    const visTpl = d.createElement('div');
    visTpl.innerHTML = w.modelRuleRowHTML({ mode: 'pattern', from: 'x(?=y)' });
    check('the park control title starts as the template renders it (active rule)',
      visBtn.title === visTpl.querySelector('.mr-vis').title);
    visBtn.click();
    visTpl.innerHTML = w.modelRuleRowHTML({ mode: 'pattern', from: 'x(?=y)', disabled: true });
    check('parking re-renders title, glyph and aria-pressed exactly like the freshly-rendered parked template',
      visBtn.title === visTpl.querySelector('.mr-vis').title &&
      visBtn.textContent === visTpl.querySelector('.mr-vis').textContent &&
      visBtn.getAttribute('aria-pressed') === visTpl.querySelector('.mr-vis').getAttribute('aria-pressed'));
    wrap.querySelectorAll('.mr-row')[1].querySelector('[data-mr-vis]').click();
    const gate2 = w.validateModelRulesDraft(wrap);
    check('parking broken rules clears the save gate',
      gate2.ok === true &&
      wrap.querySelectorAll('.mr-row')[0].dataset.mrOff === '1' &&
      wrap.querySelectorAll('.mr-row')[1].dataset.mrOff === '1');
    check('parked rules ride the wire with disabled:true (from kept)',
      JSON.stringify(w.collectModelRules(wrap).filter(r => r.disabled)) ===
      JSON.stringify([
        { mode: 'pattern', from: 'x(?=y)', to: '', disabled: true },
        { mode: 'exact', from: ' padded ', to: '', disabled: true },
      ]));
    mrWrap.remove();
  }
  check('there is no browser executor for saved pattern rules', typeof w.compileModelRules === 'undefined');
  {
    // Templates + restore-defaults: the shipped pipeline is one click. The
    // menu derives its shipped entries from the server-provided defaults doc
    // at RENDER time (no client-side rule copy), so seed settingsDoc like a
    // real /admin/config GET would before building the editor - and put it
    // back afterwards: later backup tests run with no settings doc.
    const mrWrap = d.createElement('div');
    w.__mrDefaultsDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__mrDefaultsDoc');
    mrWrap.innerHTML = w.modelRulesEditorHTML([{ mode: 'lower' }]);
    d.getElementById('settings-fields').appendChild(mrWrap);
    const wrap = mrWrap;
    const tpl = wrap.querySelector('.mr-tpl');
    const tplLabels = [...tpl.options].map(o => o.textContent).join(',');
    check('the template menu derives the five shipped rules plus the custom starters',
      tpl.options.length === 8 && tplLabels === 'add from template…,lowercase fold,strip vendor/ prefixes,strip :tag suffixes,strip serving decorations (quant, tier, build…),unify . and - between digits,exact merge…,custom pattern…');
    tpl.value = '1';
    tpl.dispatchEvent(new w.Event('change', { bubbles: true }));
    const rows = [...wrap.querySelectorAll('.mr-row')];
    check('a template appends its pre-filled rule',
      rows.length === 2 && rows[1].querySelector('.mr-from').value === '^(?:[a-z0-9][a-z0-9._-]*/)+');
    check('a shipped template carries the server doc payload, not a client copy',
      rows[1].querySelector('.mr-mode').value === 'pattern' && rows[1].querySelector('.mr-to').value === '' &&
      JSON.stringify(w.eval('settingsDoc.defaults.model_rules[1]')) === JSON.stringify({ mode: 'pattern', from: '^(?:[a-z0-9][a-z0-9._-]*/)+', to: '' }));
    wrap.querySelector('[data-mr-restore]').click();
    const defRows = [...wrap.querySelectorAll('.mr-row')];
    check('restore-defaults replaces the draft with the shipped pipeline',
      defRows.length === 5 && defRows.map(r => r.querySelector('.mr-mode').value).join(',') === 'lower,pattern,pattern,pattern,pattern');
    check('the rule count line tracks the rows',
      wrap.querySelector('.mr-count').textContent === '5 / 64 rules');
    mrWrap.remove();
    // Deny by default: without the server doc the menu offers only the
    // custom starters - never a stale client-side copy of the pipeline.
    const bareWrap = d.createElement('div');
    w.eval('settingsDoc = null');
    bareWrap.innerHTML = w.modelRulesEditorHTML([{ mode: 'lower' }]);
    d.getElementById('settings-fields').appendChild(bareWrap);
    check('without the settings doc the template menu offers only the custom starters',
      [...bareWrap.querySelector('.mr-tpl').options].map(o => o.textContent).join(',') === 'add from template…,exact merge…,custom pattern…');
    bareWrap.remove();
  }
  {
    // last_reload settings section: the live snapshot renders the server's
    // last-apply report, deny-by-default when the server has none (the
    // cfgDoc fixture below carries no last_reload, like a fresh process).
    w.__lrOk = JSON.parse(JSON.stringify(cfgDoc));
    w.__lrOk.last_reload = { ok: true, error: '', dropped_keys: [], restart_required: ['db_path'], at: 1757777777000 };
    w.eval('fillSettingsLive(window.__lrOk)');
    let live = d.getElementById('settings-live').textContent;
    check('the live snapshot renders last_reload with the restart vocabulary',
      /last apply/.test(live) && /restart needed for: db_path/.test(live) && !/failed/.test(live));
    w.__lrBad = JSON.parse(JSON.stringify(cfgDoc));
    w.__lrBad.last_reload = { ok: false, error: 'settings saved, but reload failed: bad yaml', dropped_keys: ['limits'], restart_required: [], at: 1757777777000 };
    w.eval('fillSettingsLive(window.__lrBad)');
    live = d.getElementById('settings-live').textContent;
    const badVal = d.querySelector('#settings-live .st-live-row:last-child .v');
    check('a failed reload reads as failed with its dropped keys and error',
      /last apply/.test(live) && /failed/.test(live) && /dropped 1/.test(live) && !/restart needed/.test(live) &&
      /reload failed/.test(badVal.getAttribute('title')));
    w.__lrNone = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('fillSettingsLive(window.__lrNone)');
    live = d.getElementById('settings-live').textContent;
    check('no last_reload row renders before the first server apply',
      !/last apply/.test(live) && /listen/.test(live) && /store/.test(live) && /write/.test(live));
    // A staged database restore is a system fact: it lands on restart and must
    // be visible beside the other runtime facts, not only in the backup pane.
    w.__lrPending = JSON.parse(JSON.stringify(cfgDoc));
    w.__lrPending.backup = { config: true, database: true, requests: 4, pending_database: true };
    w.eval('fillSettingsLive(window.__lrPending)');
    check('a staged database restore is named in the system facts',
      /staged/.test(d.getElementById('settings-live').textContent) &&
      /database restore/.test(d.getElementById('settings-live').textContent));
    w.__lrJunk = JSON.parse(JSON.stringify(cfgDoc));
    w.__lrJunk.last_reload = { ok: true, at: 'not-a-time' };
    w.eval('fillSettingsLive(window.__lrJunk)');
    check('a malformed last_reload arrival time renders nothing',
      !/last apply/.test(d.getElementById('settings-live').textContent));
  }
  {
    // The preview bench: step trace + before→after over the request log's
    // REAL spellings (the backtest - synthetic examples lie).
    const mrWrap = d.createElement('div');
    mrWrap.innerHTML = w.modelRulesEditorHTML([
      { mode: 'lower' },
      { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' },
    ]);
    d.getElementById('settings-fields').appendChild(mrWrap);
    const wrap = mrWrap;
    w.eval(`(() => {
      const saved = lastData;
      lastData = { records: [{ model: 'MoonshotAI/kimi-k3:Nube' }, { model: 'glm-5.3' }, { model: 'glm-5.3' }] };
      window.__mrSaved = saved;
    })()`);
    const testIn = wrap.querySelector('.mr-test');
    testIn.value = 'MoonshotAI/kimi-k3:Nube';
    testIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the step trace shows each rule in sequence',
      !wrap.querySelector('.mr-trace').hidden &&
      wrap.querySelectorAll('.mr-trace .mr-step-rule').length === 1);
    check('the examples table shows real spellings before→after with change counts',
      wrap.querySelector('.mr-examples').textContent.includes('1/2 change') &&
      wrap.querySelector('.mr-examples .mr-ex.ch .mr-ex-out').textContent === 'moonshotai/kimi-k3:nube');
    w.eval('lastData = window.__mrSaved; delete window.__mrSaved;');
    mrWrap.remove();
  }
  check('settings button exists', !!d.getElementById('btn-settings') && !!d.getElementById('settings-sheet'));
  check('settings sheet has category rail and live snapshot', !!d.getElementById('settings-rail') && !!d.getElementById('settings-live'));
  const settingsClose = d.getElementById('btn-settings-close');
  check('settings close names its action and matches the drawer hint',
    !!settingsClose && settingsClose.getAttribute('aria-label') === 'close settings' &&
    !!settingsClose.querySelector('.d-key') && settingsClose.querySelector('.d-key').textContent === 'esc');
  {
    const doc = {
      writable: true,
      fields: [
        { key: 'backup_max_bytes', category: 'backup', label: 'Backup size cap', kind: 'bytes', help: 'cap', hot_reload: true },
        { key: 'whole_document_read_max', category: 'providers', label: 'Whole-document read maximum', kind: 'duration', help: 'cap', hot_reload: true },
        { key: 'db_path', category: 'providers', label: 'Database path', kind: 'string', help: 'path', hot_reload: false },
        { key: 'listen', category: 'providers', label: 'Listen', kind: 'string', help: 'address', hot_reload: false },
      ],
      categories: [{ id: 'backup', label: 'Backup', help: 'archives' }],
      values: { backup_max_bytes: '1GiB', whole_document_read_max: '10m', db_path: 'live.db', listen: '127.0.0.1:8080' },
      defaults: { backup_max_bytes: '1GiB', whole_document_read_max: '10m', db_path: 'default.db', listen: '127.0.0.1:8080' },
      overrides: { listen: '127.0.0.1:9000', db_path: 'cli.db' },
      backup: { config: true, database: true, requests: 12, modified: ['backup_max_bytes'] },
    };
    w.settingsDoc = doc;
    w.fillSettingsForm(doc);
    check('backup archive actions render in settings',
      !!d.getElementById('btn-backup-download') && !!d.getElementById('btn-backup-restore') &&
      !!d.getElementById('backup-dl-config') && !!d.getElementById('backup-dl-database'));
    check('download does not show restore merge options',
      !d.querySelector('input[name="backup-mode"]') &&
      !d.querySelector('input[name="backup-config-mode"]') && !d.querySelector('input[name="backup-database-mode"]') &&
      !d.getElementById('btn-backup-apply') && !d.getElementById('backup-include-config'));
    check('download describes the live archive members',
      d.querySelector('[data-backup="download"]').textContent.includes('1 setting differs from default') &&
      d.querySelector('[data-backup="download"]').textContent.includes('12 requests'));
    const originalFetch = w.fetch;
    const origClick = w.HTMLAnchorElement.prototype.click;
    const calls = [];
    w.eval("operatorCredential = 'op-token'");
    w.URL.createObjectURL = () => 'blob:backup';
    w.URL.revokeObjectURL = () => {};
    w.HTMLAnchorElement.prototype.click = function() {};
    w.fetch = (url, opts) => {
      const u = String(url);
      const headers = (opts && opts.headers) || {};
      calls.push({ u, auth: headers.Authorization || '', method: (opts && opts.method) || 'GET' });
      if (u.includes('/admin/backup')) {
        return Promise.resolve({
          ok: true,
          blob: async () => new w.Blob([new Uint8Array([1])]),
          headers: { get: n => String(n).toLowerCase() === 'content-disposition' ? 'attachment; filename="t.mvb"' : null },
        });
      }
      if (u.includes('/admin/restore')) {
        if (u.includes('inspect=1')) {
          return Promise.resolve({ ok: true, json: async () => ({
            ok: true, created: '2026-09-07T12:00:00Z',
            config: { present: true, bytes: 2048, values: { backup_max_bytes: '2GiB' }, modified: ['backup_max_bytes'], vs_live: ['backup_max_bytes', 'whole_document_read_max', 'db_path', 'listen'] },
            database: { present: true, bytes: 4096, requests: 4, debug: 0, overlap: 1, oldest_ms: Date.parse('2026-08-16T11:00:00Z'), newest_ms: Date.parse('2026-09-07T12:00:00Z') },
          }) });
        }
        return Promise.resolve({ ok: true, json: async () => ({ ok: true, restart_required: ['db_path'] }) });
      }
      if (u.includes('/admin/config')) {
        return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
      }
      return originalFetch(url, opts);
    };
    w.runBackupDownload();
    check('backup download uses the operator fetch gate',
      await settleUntil(() => calls.some(c => c.u.includes('/admin/backup') && c.auth === 'Bearer op-token') &&
        d.getElementById('btn-backup-download')?.dataset.busy !== '1'));
    // The generic operatorErrorBody site: an empty-body 502 (no JSON
    // anywhere, like a bare gateway refusal) must reject with the caller's
    // designed fallback message, never an empty Error. The W20 mutation
    // round proved nothing reddened when the fallback was dropped.
    w.fetch = (url, opts) => {
      const u = String(url);
      if (u.includes('/admin/backup')) return Promise.resolve({ ok: false, status: 502, headers: { get: () => null } });
      return originalFetch(url, opts);
    };
    w.runBackupDownload();
    check('an empty-body 502 backup download surfaces the designed fallback message',
      await settleUntil(() => d.getElementById('settings-count').textContent === 'backup failed'));
    calls.length = 0;
    const inspectPayload = {
      ok: true, created: '2026-09-07T12:00:00Z',
      config: { present: true, bytes: 2048, values: { backup_max_bytes: '2GiB' }, modified: ['backup_max_bytes'], vs_live: ['backup_max_bytes', 'whole_document_read_max', 'db_path', 'listen'] },
      database: { present: true, bytes: 4096, requests: 4, debug: 0, overlap: 1, oldest_ms: Date.parse('2026-08-16T11:00:00Z'), newest_ms: Date.parse('2026-09-07T12:00:00Z') },
    };
    let releaseInspect;
    w.fetch = (url, opts) => {
      const u = String(url);
      const headers = (opts && opts.headers) || {};
      calls.push({ u, auth: headers.Authorization || '', method: (opts && opts.method) || 'GET' });
      if (u.includes('/admin/restore')) {
        if (u.includes('inspect=1')) {
          return new Promise(resolve => {
            releaseInspect = () => resolve({ ok: true, json: async () => inspectPayload });
          });
        }
        return Promise.resolve({ ok: true, json: async () => ({ ok: true, restart_required: ['db_path'] }) });
      }
      if (u.includes('/admin/config')) {
        return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
      }
      return originalFetch(url, opts);
    };
    const input = d.getElementById('backup-file');
    const file = new w.File(['archive'], 't.mvb', { type: 'application/octet-stream' });
    Object.defineProperty(input, 'files', { configurable: true, value: [file] });
    input.dispatchEvent(new w.Event('change', { bubbles: true }));
    check('inspect holds the busy gate until the archive is reviewed',
      await settleUntil(() => d.getElementById('btn-backup-download')?.dataset.busy === '1' &&
      d.getElementById('btn-backup-restore')?.dataset.busy === '1' &&
      !d.getElementById('btn-backup-apply')));
    w.fillSettingsForm(doc);
    check('settings refresh during inspect keeps the busy gate',
      d.getElementById('btn-backup-download')?.dataset.busy === '1' &&
      d.getElementById('btn-backup-restore')?.dataset.busy === '1' &&
      !d.getElementById('btn-backup-apply'));
    releaseInspect();
    check('backup inspect uses the operator fetch gate',
      await settleUntil(() => calls.some(c => c.u.includes('/admin/restore') && c.u.includes('inspect=1') && c.auth === 'Bearer op-token') &&
        !!d.querySelector('input[name="backup-mode"][value="merge"]') &&
        !!d.getElementById('backup-preview')));
    check('restore does not show download actions',
      !d.getElementById('btn-backup-download') && !d.getElementById('btn-backup-restore') &&
      !d.getElementById('backup-dl-config') &&
      !!d.getElementById('btn-backup-apply') && !!d.getElementById('btn-backup-cancel'));
    check('backup restore offers one merge or replace after inspect',
      !!d.querySelector('input[name="backup-mode"][value="merge"]') &&
      !!d.querySelector('input[name="backup-mode"][value="replace"]') &&
      !d.querySelector('input[name="backup-config-mode"]') &&
      !d.querySelector('input[name="backup-database-mode"]'));
    check('backup inspect lists modified settings vs default',
      !!d.getElementById('backup-preview') &&
      d.getElementById('backup-preview').textContent.includes('2GiB') &&
      d.getElementById('backup-preview').textContent.includes('default 1GiB') &&
      d.getElementById('backup-preview').textContent.includes('live 1GiB'));
    const liveOnlyRow = d.querySelector('[data-backup="diff"][data-key="whole_document_read_max"]');
    check('replace preview shows a live-only key as reset',
      !!liveOnlyRow && liveOnlyRow.textContent.includes('replace resets it'));
    const preservedRow = d.querySelector('[data-backup="diff"][data-key="db_path"]');
    check('replace preview keeps a CLI-overridden live key',
      !!preservedRow &&
      preservedRow.textContent.includes('live live.db') &&
      preservedRow.textContent.includes('preserved by the CLI override') &&
      !preservedRow.textContent.includes('replace resets it'));
    check('replace hint excludes CLI-overridden settings',
      d.querySelector('[data-backup="restore"]').textContent.includes('except CLI-overridden db_path and listen'));
    const mergeRadio = d.querySelector('input[name="backup-mode"][value="merge"]');
    mergeRadio.checked = true;
    mergeRadio.dispatchEvent(new w.Event('change', { bubbles: true }));
    const mergeRow = d.querySelector('[data-backup="diff"][data-key="whole_document_read_max"]');
    check('merge preview reports a live-only key as unchanged',
      !!mergeRow && mergeRow.textContent.includes('merge leaves it unchanged'));
    const mergePreserved = d.querySelector('[data-backup="diff"][data-key="listen"]');
    check('merge preview still marks a CLI-overridden key as preserved',
      !!mergePreserved && mergePreserved.textContent.includes('preserved by the CLI override'));
    const replaceRadio = d.querySelector('input[name="backup-mode"][value="replace"]');
    replaceRadio.checked = true;
    replaceRadio.dispatchEvent(new w.Event('change', { bubbles: true }));
    check('archive facts sit apart from restore options',
      d.querySelector('[data-backup="archive"]') &&
      d.querySelector('[data-backup="archive"]').textContent.includes('t.mvb') &&
      d.querySelector('[data-backup="archive"]').textContent.includes('2026-09-07') &&
      d.querySelector('[data-backup="archive"]').textContent.includes('4 requests') &&
      d.querySelector('[data-backup="archive"]').textContent.includes('2026-08-16') &&
      d.querySelector('[data-backup="archive"]').textContent.includes('already on this store') &&
      !d.querySelector('[data-backup="restore"]').textContent.includes('4 requests') &&
      !d.querySelector('[data-backup="restore"]').textContent.includes('already on this store') &&
      !d.querySelector('[data-backup="restore"]').textContent.includes('2GiB'));
    const cfgBox = d.getElementById('backup-include-config');
    cfgBox.checked = false;
    cfgBox.dispatchEvent(new w.Event('change', { bubbles: true }));
    check('unchecking config keeps archive facts and setting rows',
      d.getElementById('backup-include-config') && !d.getElementById('backup-include-config').checked &&
      !!d.querySelector('input[name="backup-mode"]') &&
      d.querySelector('[data-backup="archive"]') &&
      d.getElementById('backup-preview') &&
      d.getElementById('backup-preview').textContent.includes('2GiB'));
    calls.length = 0;
    w.runBackupApply();
    check('backup apply uses the operator fetch gate',
      await settleUntil(() => calls.some(c => c.u.includes('/admin/restore') && !c.u.includes('inspect=1') && c.auth === 'Bearer op-token') &&
        calls.some(c => c.u.includes('/admin/config') && c.method === 'GET')));
    check('apply omits an unchecked config member',
      calls.some(c => c.u.includes('/admin/restore') && !c.u.includes('inspect=1') && c.u.includes('database=1') && !c.u.includes('config=1')));
    check('restore refreshes settings even when a database restart is pending',
      calls.some(c => c.u.includes('/admin/config') && c.method === 'GET'));
    // A restore applies only after one confirmation naming the members, the
    // mode and the missing rollback. Cancel writes nothing and the sheet lives.
    {
      if (w.eval('settingsIsOpen()')) await w.openSettings(); // the ternary closes when open
      await w.openSettings();
      await settleUntil(() => !!w.eval('settingsIsOpen()'));
      calls.length = 0;
      w.eval("backupInspect = { file: new Blob(['x']), data: { config: { present: true }, database: { present: true, inspectable: true, requests: 1 } } }");
      w.runBackupApply();
      await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
      const dlg = d.getElementById('oc-confirm-dialog');
      check('the restore asks first and names the member, mode and the missing rollback (' + (dlg ? dlg.textContent.slice(0, 120) : 'no dialog') + '|' + w.eval('settingsIsOpen()') + '|' + d.getElementById('settings-count').textContent + ')',
        !!dlg && /database/.test(dlg.textContent) && !/config/.test(dlg.textContent) &&
        /cannot be undone/.test(dlg.textContent) &&
        dlg.querySelector('[data-oc-confirm="ok"]').textContent === 'restore');
      dlg.querySelector('[data-oc-confirm="cancel"]').click();
      await settleUntil(() => !!w.eval('settingsIsOpen()'));
      check('a cancelled restore writes nothing and puts the sheet back',
        !calls.some(c => c.u.includes('/admin/restore') && !c.u.includes('inspect=1')) &&
        !!w.eval('settingsIsOpen()'));
      calls.length = 0;
      w.eval("backupInspect = { file: new Blob(['x']), data: { config: { present: true }, database: { present: true, inspectable: true, requests: 1 } } }");
      w.runBackupApply();
      await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
      d.getElementById('oc-confirm-dialog').querySelector('[data-oc-confirm="ok"]').click();
      check('a confirmed restore posts and the sheet comes back',
        await settleUntil(() => calls.some(c => c.u.includes('/admin/restore') && !c.u.includes('inspect=1') && c.u.includes('database=1'))) &&
        await settleUntil(() => !!w.eval('settingsIsOpen()')));
      await settleUntil(() => !d.getElementById('btn-backup-apply')?.dataset.busy);
      // A database-only restore leaves the config alone, so an open dirty
      // config draft is still valid: the refill must not discard it, while a
      // config restore must.
      {
        const savedFetchSettings = w.fetchSettings;
        const discards = [];
        w.fetchSettings = discard => { discards.push(discard); return savedFetchSettings(discard); };
        try {
          w.eval('backupIncludeConfig = false; backupIncludeDatabase = true');
          w.eval("backupInspect = { file: new Blob(['x']), data: { config: { present: true }, database: { present: true, inspectable: true, requests: 1 } } }");
          w.runBackupApply();
          await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
          d.getElementById('oc-confirm-dialog').querySelector('[data-oc-confirm="ok"]').click();
          await settleUntil(() => discards.length > 0);
          check('a database-only restore keeps the config draft', discards[discards.length - 1] === false);
          discards.length = 0;
          w.eval('backupIncludeConfig = true; backupIncludeDatabase = true');
          w.eval("backupInspect = { file: new Blob(['x']), data: { config: { present: true }, database: { present: true, inspectable: true, requests: 1 } } }");
          w.runBackupApply();
          await settleUntil(() => !!d.getElementById('oc-confirm-dialog'));
          d.getElementById('oc-confirm-dialog').querySelector('[data-oc-confirm="ok"]').click();
          await settleUntil(() => discards.length > 0);
          check('a config restore discards the stale draft', discards[discards.length - 1] === true);
        } finally {
          w.fetchSettings = savedFetchSettings;
        }
      }
      w.closeSettings(true);
    }
    // A failed restore must read as an error, not as an informational line:
    // the status tone is the only difference between "restored" and "did not".
    w.fetch = (url, opts) => String(url).includes('/admin/restore')
      ? Promise.resolve({ ok: false, status: 500, json: async () => ({ error: 'restore blew up' }) })
      : originalFetch(url, opts);
    w.eval("backupInspect = { file: new Blob(['x']), data: { config: { present: true }, database: { present: false } } };");
    d.getElementById('backup-include-config').checked = true;
    d.getElementById('backup-include-config').dispatchEvent(new w.Event('change', { bubbles: true }));
    w.runBackupApply();
    await settleUntil(() => /restore blew up/.test(d.getElementById('settings-count').textContent));
    check('a failed restore paints the settings status as an error',
      d.getElementById('settings-count').dataset.tone === 'error');
    w.fetch = originalFetch;
    w.HTMLAnchorElement.prototype.click = origClick;
    // A store-less preview carries the database member but marks it
    // uninspectable. The selection must stay disabled and unchecked so the
    // apply request cannot ask for a database the endpoint cannot stage.
    w.eval("backupInspect = { data: { config: { present: true }, database: { present: true, inspectable: false, reason: 'durable storage is disabled' } } }; backupIncludeConfig = true; backupIncludeDatabase = true;");
    const storeless = d.createElement('div');
    storeless.innerHTML = w.eval("backupRestoreOptionsHTML({ config: true, database: true })");
    d.getElementById('settings-fields').appendChild(storeless);
    const storelessDb = storeless.querySelector('#backup-include-database');
    check('store-less preview disables and unchecks the database member',
      !!storelessDb && storelessDb.disabled && !storelessDb.checked);
    check('store-less preview shows the inspectable reason',
      storeless.textContent.includes('durable storage is disabled'));
    storeless.remove();
    w.eval("operatorCredential = ''; backupInspect = null; backupIncludeConfig = true; backupIncludeDatabase = true; backupMode = 'replace'; backupBusy = false; backupReq = 0;");
  }


  w.applyModelCanon(modelFixture(), true);
};
