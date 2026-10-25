// MCP settings category pin: the schema-driven mcp category must render
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function mcpCategory(H) {
  const {cfgDoc, check, d, settleUntil, w} = H;
  const state = H.state;

  // ---- MCP settings category pin: the schema-driven mcp category must render
  // the enable switch and the six limit fields, every row restart-required, the
  // rail badge resolving through the SETTINGS_CAT_REF mcp entry (the tool
  // entity icon, not the server fallback), and the category banner naming the
  // restart.
  {
    const doc = {
      revision: 'mcp-r1',
      fields: [
        { key: 'mcp_enabled', category: 'mcp', label: 'Enable MCP endpoint', help: 'Register the streamable HTTP MCP endpoint at /mcp.', kind: 'bool', hot_reload: false },
        { key: 'mcp_page_size', category: 'mcp', label: 'Page size', help: 'Default page for the records and capture listings.', kind: 'int', min: 1, hot_reload: false },
        { key: 'mcp_query_max_rows', category: 'mcp', label: 'Query result rows', help: 'Row cap on one query tool result.', kind: 'int', min: 1, hot_reload: false },
        { key: 'mcp_query_max_bytes', category: 'mcp', label: 'Query result bytes', help: 'Encoded-size cap on one query tool result.', kind: 'bytes', min: 1, hot_reload: false },
        { key: 'mcp_capture_max_bytes', category: 'mcp', label: 'Capture max bytes', help: 'Capture document size above which the document is withheld whole.', kind: 'bytes', min: 1, hot_reload: false },
        { key: 'mcp_query_timeout', category: 'mcp', label: 'Query timeout', help: 'Bounds one proxy read that is not the bounded SQL reader.', kind: 'duration', hot_reload: false },
        { key: 'mcp_timeout', category: 'mcp', label: 'Request timeout', help: 'Bounds every in-process call to the proxy.', kind: 'duration', hot_reload: false },
      ],
      categories: [{ id: 'mcp', label: 'MCP', help: 'The operator-gated /mcp LLM tool endpoint.' }],
      values: { mcp_enabled: true, mcp_page_size: 50, mcp_query_max_rows: 200, mcp_query_max_bytes: '128KiB', mcp_capture_max_bytes: '256KiB', mcp_query_timeout: '2m', mcp_timeout: '30s' },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.__mcpDoc = doc;
    w.eval("settingsDoc = window.__mcpDoc; settingsCat = 'mcp'; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const switchEl = box.querySelector('input.st-switch[data-key="mcp_enabled"]');
    const mcpRows = [...box.querySelectorAll('.st-row[data-cat="mcp"]')];
    const limitRows = ['mcp_page_size', 'mcp_query_max_rows', 'mcp_query_max_bytes', 'mcp_capture_max_bytes', 'mcp_query_timeout', 'mcp_timeout']
      .map(k => box.querySelector(`.st-row[data-key="${k}"]`));
    check('the mcp category renders the enable switch checked and the six limit fields',
      !!switchEl && switchEl.checked && limitRows.every(r => !!r && !!r.querySelector('[data-st-scalar]')) &&
      mcpRows.length === 7);
    check('every mcp row is restart-required and the category banner says so',
      mcpRows.every(r => r.dataset.hot === '0') &&
      d.querySelector('#settings-pane-hd .st-banner').textContent === 'takes a process restart');
    check('the mcp rail badge resolves through SETTINGS_CAT_REF to the tool entity',
      d.querySelector('[data-st-cat="mcp"] .ent-ic').textContent === w.eval('ENTITY_TYPES.tool.icon') &&
      d.querySelector('[data-st-cat="mcp"] .rail-n').textContent === '7');
    w.__mcpDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__mcpDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__mcpDoc;
  }

  // Settings transactions: preserve drafts across late loads/saves and reject
  // incomplete numeric/map edits rather than silently turning them into zeros
  // or deleting entries. HTTP is stubbed; no running YAML is modified.
  {
    const originalFetch = w.fetch;
    const pendingGets = [], pendingPosts = [];
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.unshift(
      { key: 'quality_retries', category: 'providers', label: 'Quality retries', kind: 'int', min: 0, max: 10, hot_reload: true },
      { key: 'db_path', category: 'providers', label: 'Database path', kind: 'string', hot_reload: false });
    doc.values.quality_retries = 1;
    doc.values.db_path = 'private.db';
    doc.revision = 'r1';
    const install = () => {
      w.__settingsTestDoc = JSON.parse(JSON.stringify(doc));
      w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
      d.getElementById('settings-sheet').hidden = false;
    };
    const number = () => d.querySelector('input[data-key="quality_retries"]');
    const edit = value => { number().value = value; number().dispatchEvent(new w.Event('input', { bubbles: true })); };
    const response = (data, ok = true) => ({ ok, statusText: ok ? 'OK' : 'failure', json: async () => data });
    w.fetch = (url, init) => {
      if (!String(url).includes('/admin/config')) return originalFetch(url, init);
      return new Promise(resolve => {
        if (init?.method === 'POST') pendingPosts.push({ resolve, body: JSON.parse(init.body) });
        else pendingGets.push(resolve);
      });
    };
    install();
    edit('');
    await w.applySettings();
    check('blank numeric setting stays empty and cannot silently become zero',
      w.collectSettingsValues().quality_retries === '' && pendingPosts.length === 0 && /integer/.test(d.getElementById('settings-count').textContent));
    install();
    edit('3');
    const save = w.applySettings();
    check('settings save sends its loaded revision and disables Revert until completion',
      pendingPosts.length === 1 && pendingPosts[0].body.revision === 'r1' && d.getElementById('btn-settings-revert').disabled);
    edit('4');
    pendingPosts.shift().resolve(response({ saved: true, revision: 'r2', values: { ...doc.values, quality_retries: 3 }, effective: {}, restart_required: [] }));
    await save;
    check('typing during a settings save remains dirty with the new saved revision',
      number().value === '4' && w.settingsIsDirty() && w.eval('settingsDoc.revision') === 'r2' && !d.getElementById('btn-settings-apply').disabled);
    const refresh = w.fetchSettings();
    pendingGets.shift()(response({ ...doc, revision: 'external' }));
    await refresh;
    check('background settings refresh never erases a dirty draft or its conflict token',
      number().value === '4' && w.eval('settingsDoc.revision') === 'r2');
    const revert = w.revertSettings();
    pendingGets.shift()(response({ ...doc, revision: 'r3' }));
    await revert;
    check('explicit Revert adopts current server values and revision',
      number().value === '1' && !w.settingsIsDirty() && w.eval('settingsDoc.revision') === 'r3');
    const restartWarning = w.fetchSettings();
    pendingGets.shift()(response({ ...doc, revision: 'r3', restart_required: ['db_path'] }));
    await restartWarning;
    edit('2');
    edit('1');
    check('reopening or restoring a clean settings draft retains the server restart warning',
      /restart needed for: db_path/.test(d.getElementById('settings-count').textContent));
    const older = w.fetchSettings(), newer = w.fetchSettings();
    const finishOlder = pendingGets.shift(), finishNewer = pendingGets.shift();
    finishNewer(response({ ...doc, revision: 'newest' }));
    await newer;
    finishOlder(response({ ...doc, revision: 'stale' }));
    await older;
    check('out-of-order settings GETs cannot regress the accepted document', w.eval('settingsDoc.revision') === 'newest');
    const whileTyping = w.fetchSettings();
    edit('5');
    pendingGets.shift()(response({ ...doc, revision: 'late' }));
    await whileTyping;
    check('typing after a settings GET starts also survives its response', number().value === '5' && w.settingsIsDirty());
    const conflict = w.applySettings();
    pendingPosts.shift().resolve(response({ error: 'settings changed; revert first' }, false));
    await conflict;
    check('a settings conflict preserves the draft and exposes the server error',
      number().value === '5' && w.settingsIsDirty() && /settings changed/.test(d.getElementById('settings-count').textContent));
    const failedReload = w.applySettings();
    pendingPosts.shift().resolve(response({ saved: true, revision: 'saved-not-live', values: { ...doc.values, quality_retries: 5 }, effective: {}, error: 'settings saved, but reload failed' }, false));
    await failedReload;
    check('saved-but-reload-failed response retains the usable revision and honest error',
      w.eval('settingsDoc.revision') === 'saved-not-live' && /reload failed/.test(d.getElementById('settings-count').textContent));
    install();
    edit('2');
    const cleanSave = w.applySettings();
    const duringSave = w.fetchSettings();
    pendingPosts.shift().resolve(response({ saved: true, revision: 'r2', values: { ...doc.values, quality_retries: 2 }, effective: {}, restart_required: [] }));
    await cleanSave;
    pendingGets.shift()(response(doc));
    await duringSave;
    check('a GET issued during a save cannot regress its clean accepted state afterward',
      number().value === '2' && w.eval('settingsDoc.revision') === 'r2' && !w.settingsIsDirty());
    install();
    const providers = d.querySelector('[data-kind="providers"]');
    providers.insertAdjacentHTML('beforeend', w.providerCardHTML('custom.example', { models_keys: { custom_limit: 'limits.extra' } }));
    check('configured custom model output keys round-trip without becoming a canonical dropdown default',
      w.collectSettingsValues().providers['custom.example'].models_keys.custom_limit === 'limits.extra');
    providers.insertAdjacentHTML('beforeend', w.providerCardHTML('__proto__', {}));
    check('settings map labels cannot disappear through Object.prototype setters',
      Object.hasOwn(w.collectSettingsValues().providers, '__proto__'));
    providers.insertAdjacentHTML('beforeend', w.providerCardHTML('epsilon.example', {}));
    w.markSettingsDirty();
    await w.applySettings();
    check('duplicate edited provider labels are rejected before POST instead of overwriting a card',
      pendingPosts.length === 0 && /duplicate mapping/.test(d.getElementById('settings-count').textContent));
    install();
    const usagePath = d.querySelector('.prov-umap .sp-upath');
    usagePath.value = '';
    const aliasTarget = d.querySelector('.al-row .al-to');
    aliasTarget.value = '';
    check('incomplete existing mapping rows remain in the draft for server validation',
      Object.hasOwn(w.collectSettingsValues().providers['epsilon.example'].usage_keys, 'input_tokens') &&
      w.collectSettingsValues().provider_aliases['old.example'] === '');
    w.fetch = originalFetch;
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
  }

  // A clean Settings reopen skips the whole form rebuild, but the live
  // snapshot (last apply and effective runtime data) can change without a
  // revision change. Reopening must refresh that panel alone: form nodes,
  // scroll and focus stay exactly where the operator left them.
  {
    const originalFetch = w.fetch;
    const base = JSON.parse(JSON.stringify(cfgDoc));
    base.revision = 'live-panel-revision';
    base.fields.unshift({ key: 'db_path', category: 'providers', label: 'Database path', kind: 'string', hot_reload: false });
    base.values.db_path = 'private.db';
    const docAt = (listen, at) => ({
      ...JSON.parse(JSON.stringify(base)),
      effective: {...base.values, listen},
      last_reload: {ok: true, error: '', dropped_keys: [], restart_required: [], at},
    });
    let configDoc = docAt('127.0.0.1:9001', 1757777000000);
    w.fetch = (url, opts) => {
      if (!String(url).includes('/admin/config')) return originalFetch(url, opts);
      return Promise.resolve({ok: true, json: async () => JSON.parse(JSON.stringify(configDoc))});
    };
    try {
      w.openSettings();
      await settleUntil(() => !d.getElementById('settings-sheet').hidden &&
        /9001/.test(d.getElementById('settings-live').textContent) &&
        !!d.querySelector('#settings-fields .st-row[data-key="db_path"]'));
      const liveBefore = d.getElementById('settings-live').textContent;
      const box = d.getElementById('settings-fields');
      const row = box.querySelector('.st-row[data-key="db_path"]');
      const input = row.querySelector('input[data-key="db_path"]');
      input.focus();
      box.scrollTop = 73;
      w.closeSettings(true);
      await settleUntil(() => d.getElementById('settings-sheet').hidden);

      configDoc = docAt('127.0.0.1:9002', 1757779999000);
      w.openSettings();
      await settleUntil(() => !d.getElementById('settings-sheet').hidden &&
        /9002/.test(d.getElementById('settings-live').textContent));
      const liveAfter = d.getElementById('settings-live').textContent;
      check('a clean Settings reopen refreshes the independent live panel',
        liveAfter !== liveBefore && /9002/.test(liveAfter) &&
        liveAfter.includes(new Date(1757779999000).toLocaleString()));
      check('a clean Settings reopen preserves the form nodes and scroll position',
        box.querySelector('.st-row[data-key="db_path"]') === row &&
        row.querySelector('input[data-key="db_path"]') === input && box.scrollTop === 73);

      const nextDoc = docAt('127.0.0.1:9003', 1757781111000);
      configDoc = nextDoc;
      input.focus();
      await w.fetchSettings();
      check('the skip refill keeps focus in the operator\'s form control',
        d.activeElement === input && /9003/.test(d.getElementById('settings-live').textContent) &&
        box.querySelector('.st-row[data-key="db_path"]') === row);
    } finally {
      w.fetch = originalFetch;
      w.closeSettings(true);
    }
  }

  // mrDraftChanged's dirty leg (the R9 lost finding): markSettingsDirty was
  // pinned only through non-mr paths, so dropping it from mrDraftChanged
  // reddened nothing. Every rules-draft mutation runs through that epilogue;
  // pin it through the real settings surface - a rule added via the editor's
  // add-row must mark the sheet dirty and count itself on the unsaved line.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.push({ key: 'model_rules', category: 'providers', label: 'Model rules', kind: 'model_rules', hot_reload: true });
    doc.values.model_rules = [{ mode: 'lower' }];
    w.__mrDirtyDoc = doc;
    w.eval('settingsDoc = window.__mrDirtyDoc; fillSettingsForm(settingsDoc)');
    d.getElementById('settings-sheet').hidden = false;
    const mrSettingsWrap = d.querySelector('#settings-fields [data-kind="model_rules"]');
    check('the settings sheet renders the rules editor for the model_rules field', !!mrSettingsWrap);
    check('the clean settings draft reports no unsaved changes',
      d.getElementById('settings-count').textContent === '' && !w.settingsIsDirty());
    mrSettingsWrap.querySelector('.mr-add .mr-mode').value = 'pattern';
    mrSettingsWrap.querySelector('.mr-add .mr-new-from').value = 'glm-5\\.3';
    mrSettingsWrap.querySelector('.mr-add .mr-new-to').value = 'glm-5-3';
    w.addModelRuleRow(mrSettingsWrap.querySelector('.mr-add'));
    check('adding a model rule marks the settings sheet dirty through the mr path',
      w.settingsIsDirty() && d.getElementById('settings-count').textContent === '1 unsaved' &&
        !d.getElementById('btn-settings-apply').disabled &&
        w.collectSettingsValues().model_rules.length === 2);
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__mrDirtyDoc;
  }

  // The blocked-Apply drive: an invalid model-rules draft must stop the
  // POST, name the fix on the status line and focus the first offender
  // without a scrollIntoView crash on DOMs whose Element omits it (jsdom
  // 30 does; the provider add-row guards the same call). The
  // harness-start jsdomError listener fails the whole run on an uncaught
  // page throw; the row also snapshots failures so the pin is self-contained.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.push(
      { key: 'model_rules', category: 'providers', label: 'Model rules', kind: 'model_rules', hot_reload: true },
      { key: 'conc_cap', category: 'rate', label: 'Concurrency cap', kind: 'int', hot_reload: true });
    doc.categories.push({ id: 'rate', label: 'Rate', help: 'rate limits' });
    doc.values.model_rules = [{ mode: 'pattern', from: 'a-b', to: 'a.b' }];
    doc.values.conc_cap = 4;
    w.__mrGateDoc = doc;
    w.eval('settingsDoc = window.__mrGateDoc; fillSettingsForm(settingsDoc)');
    d.getElementById('settings-sheet').hidden = false;
    const mrSettingsWrap = d.querySelector('#settings-fields [data-kind="model_rules"]');
    // install the bad row the way a real typo does: type an RE2-invalid
    // pattern into a live row (input-path validation reddens it).
    const fromIn = mrSettingsWrap.querySelector('.mr-row .mr-from');
    fromIn.value = 'x(?=y)';
    fromIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('typing an invalid pattern reddens the row and arms Apply',
      mrSettingsWrap.querySelector('.mr-row').dataset.mrState === 'error' &&
        !d.getElementById('btn-settings-apply').disabled);
    const failuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid model-rules draft blocks the save without an uncaught TypeError',
      state.failures.length === failuresBefore &&
        d.getElementById('settings-count').textContent === 'model rules - fix the highlighted rule first');
    // The hidden-offender drive (the W53 live-verified bug, the mr leg): the
    // sheet showing another category hides the rules block's .st-row, so
    // the gate's focus() and scrollIntoView() were silent no-ops on it -
    // only the status line moved. The shared reveal must select the
    // offender's category through the rail machinery before focusing the
    // bad rule.
    w.eval("settingsCat = 'rate'; showSettingsCat()");
    const mrBlockRow = mrSettingsWrap.closest('.st-row');
    check('switching the sheet to the rate category hides the model-rules block',
      mrBlockRow.hidden && !d.querySelector('[data-st-cat="providers"]').classList.contains('active'));
    const mrHiddenFailures = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden model-rules offender selects its category and focuses it',
      state.failures.length === mrHiddenFailures &&
        d.getElementById('settings-count').textContent === 'model rules - fix the highlighted rule first' &&
        w.eval('settingsCat') === 'providers' &&
        d.querySelector('[data-st-cat="providers"]').classList.contains('active') &&
        d.querySelector('[data-st-cat="providers"]').getAttribute('aria-current') === 'page' &&
        !mrBlockRow.hidden &&
        d.activeElement === fromIn && fromIn.classList.contains('prov-bad'));
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__mrGateDoc;
  }

  // The collect-catch hidden-offender drive (the W53 reveal class, the
  // scalar-int leg): a numeric field with garbage in a category the sheet
  // is not showing throws from collectSettingsValues(true) before any
  // editor gate runs, and the catch used to focus + flash the offender
  // with no reveal - silent no-ops inside a hidden .st-row, so the
  // operator got only the status line. The catch must route the offender
  // through the same reveal the save gates share.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.push({ key: 'conc_cap', category: 'rate', label: 'Concurrency cap', kind: 'int', hot_reload: true });
    doc.categories.push({ id: 'rate', label: 'Rate', help: 'rate limits' });
    doc.values.conc_cap = 4;
    w.__intRevealDoc = doc;
    w.eval('settingsDoc = window.__intRevealDoc; fillSettingsForm(settingsDoc)');
    d.getElementById('settings-sheet').hidden = false;
    w.eval("settingsCat = 'providers'; showSettingsCat()");
    const intRow = d.querySelector('.st-row[data-key="conc_cap"]');
    const intIn = intRow.querySelector('input[data-key="conc_cap"]');
    intIn.value = 'garbage';
    intIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('switching the sheet to the providers category hides the scalar-int row',
      intRow.hidden && w.settingsIsDirty() && !d.getElementById('btn-settings-apply').disabled);
    const intFailuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden scalar-int offender selects its category and focuses it',
      state.failures.length === intFailuresBefore &&
        /conc_cap: enter a valid integer/.test(d.getElementById('settings-count').textContent) &&
        w.eval('settingsCat') === 'rate' &&
        d.querySelector('[data-st-cat="rate"]').classList.contains('active') &&
        d.querySelector('[data-st-cat="rate"]').getAttribute('aria-current') === 'page' &&
        !intRow.hidden &&
        d.activeElement === intIn && intIn.classList.contains('prov-bad'));
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__intRevealDoc;
  }
};
