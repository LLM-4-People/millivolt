// test 11t: incremental settings dirty tracking
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function settingsDirty(H) {
  const {DEF_RULES, cfgDoc, check, d, w} = H;
  const state = H.state;

  // ---- test 11t: incremental settings dirty tracking ----
  // A keystroke used to collect every category/editor and stringify every
  // key. The dirty path now resolves the edited field's config key,
  // recomputes only that key and maintains a changed-key set; controls with
  // no resolvable key still fall back to the full collector. The fixture is
  // deliberately large so a regression to full-form collection shows up in
  // the collector counters.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields = Array.from({ length: 24 }, (_, i) => ({
      key: 'bulk_' + i,
      category: 'providers',
      label: 'Bulk field ' + i,
      help: 'bulk setting ' + i,
      kind: i % 3 === 0 ? 'int' : 'string',
      hot_reload: true,
    })).concat([
      { key: 'providers', category: 'providers', label: 'Provider field maps', help: 'Per-provider JSON key-path overrides.', kind: 'providers', hot_reload: true },
      { key: 'provider_aliases', category: 'providers', label: 'Provider aliases', help: 'Aliases.', kind: 'aliases', hot_reload: true },
      { key: 'model_rules', category: 'providers', label: 'Model rules', help: 'Rules.', kind: 'model_rules', hot_reload: true },
      { key: 'request_overrides', category: 'providers', label: 'Request override rules', help: 'Rules.', kind: 'request_overrides', hot_reload: true },
      { key: 'sub_conversations', category: 'providers', label: 'Sub-conversation tracking', help: 'Tracking.', kind: 'sub_conversations', hot_reload: true },
    ]);
    doc.categories = [{ id: 'providers', label: 'Providers', help: 'incremental dirty fixture' }];
    doc.values = {
      ...Object.fromEntries(Array.from({ length: 24 }, (_, i) => ['bulk_' + i, i % 3 === 0 ? i : 'value ' + i])),
      providers: { 'epsilon.example': { cost_keys: ['usage.cost'], usage_keys: { input_tokens: 'usage.input' }, models_path: '/model-meta', models_keys: { input_modalities: 'input_modalities' }, ensure_tools: [], headers: { 'X-Test': 'one' } } },
      provider_aliases: { 'old.example': 'new.example' },
      model_rules: [{ mode: 'exact', from: 'a', to: 'b' }],
      request_overrides: [{ client: 'cli', provider: '', model: '', headers: { 'X-Title': 'app' } }],
      sub_conversations: [{ client: 'cli', params: ['convKey'], strip: false }],
    };
    doc.defaults = { ...doc.defaults, providers: {}, model_rules: DEF_RULES };
    doc.revision = 'dirty-r1';
    w.__dirtyDoc = doc;
    w.eval("settingsCat = 'providers'; settingsDoc = window.__dirtyDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const status = () => d.getElementById('settings-count').textContent;
    const apply = d.getElementById('btn-settings-apply');
    const revert = d.getElementById('btn-settings-revert');
    check('the incremental dirty fixture renders every scalar and structured editor',
      box.querySelectorAll('input[data-key^="bulk_"]').length === 24 &&
      !!box.querySelector('[data-kind="providers"]') &&
      !!box.querySelector('[data-kind="aliases"]') &&
      !!box.querySelector('[data-kind="model_rules"]') &&
      !!box.querySelector('[data-kind="request_overrides"]') &&
      !!box.querySelector('[data-kind="sub_conversations"]'));
    {
      const full = w.collectSettingsValues();
      check('the per-key collector matches the full collector for every rendered key',
        typeof w.collectSettingsValue === 'function' &&
        [...box.querySelectorAll('[data-key]')].filter(el => !el.classList.contains('st-row'))
          .every(el => JSON.stringify(w.collectSettingsValue(el.dataset.key)) === JSON.stringify(full[el.dataset.key])));
    }
    const originalKeyCollector = w.collectSettingsValue;
    const originalFullCollector = w.collectSettingsValues;
    let keyCalls = [];
    let fullCalls = 0;
    w.collectSettingsValue = key => { keyCalls.push(key); return originalKeyCollector(key); };
    w.collectSettingsValues = (...args) => { fullCalls++; return originalFullCollector(...args); };
    const resetCounters = () => { keyCalls = []; fullCalls = 0; };
    const checkClean = label => check(label,
      w.settingsDirtyCount() === 0 && !w.settingsIsDirty() && status() === '' &&
      apply.disabled && revert.disabled);
    const checkDirty = (label, n) => check(label,
      w.settingsDirtyCount() === n && w.settingsIsDirty() &&
      status() === (n === 1 ? '1 unsaved' : n + ' unsaved') &&
      !apply.disabled && !revert.disabled);
    const inputEvent = () => new w.Event('input', { bubbles: true });
    const edit = (el, value) => { el.value = value; el.dispatchEvent(inputEvent()); };
    checkClean('the incremental dirty fixture starts clean with both actions disabled');
    // One scalar keystroke: only its own key is collected; the full sheet is
    // never touched.
    resetCounters();
    const bulk1 = box.querySelector('input[data-key="bulk_1"]');
    edit(bulk1, 'edited one');
    check('one scalar input collects only its own config key',
      JSON.stringify(keyCalls) === JSON.stringify(['bulk_1']) && fullCalls === 0);
    checkDirty('the scalar edit reports one unsaved key with Apply armed', 1);
    resetCounters();
    const bulk2 = box.querySelector('input[data-key="bulk_2"]');
    edit(bulk2, 'edited two');
    check('a second scalar input still collects only its own key',
      JSON.stringify(keyCalls) === JSON.stringify(['bulk_2']) && fullCalls === 0);
    checkDirty('a second scalar edit counts two unsaved keys', 2);
    edit(bulk1, 'value 1');
    checkDirty('reverting one scalar keeps the remaining key counted', 1);
    edit(bulk2, 'value 2');
    checkClean('reverting both scalar edits restores the clean draft');
    // Provider mapping and alias changes own the composite editor keys.
    resetCounters();
    const upath = box.querySelector('.prov-umap .sp-upath');
    edit(upath, 'usage.inputTokens');
    check('a provider mapping edit collects only the providers key',
      JSON.stringify(keyCalls) === JSON.stringify(['providers']) && fullCalls === 0);
    checkDirty('a provider mapping edit counts one unsaved key', 1);
    edit(upath, 'usage.input');
    checkClean('reverting the provider mapping restores the clean draft');
    resetCounters();
    const aliasTo = box.querySelector('[data-kind="aliases"] .al-to');
    edit(aliasTo, 'other.example');
    check('an alias edit collects only the provider_aliases key',
      JSON.stringify(keyCalls) === JSON.stringify(['provider_aliases']) && fullCalls === 0);
    checkDirty('an alias edit counts one unsaved key', 1);
    edit(aliasTo, 'new.example');
    checkClean('reverting the alias edit restores the clean draft');
    // Model-rules, request-overrides and sub-conversation row edits.
    resetCounters();
    const mrFrom = box.querySelector('[data-kind="model_rules"] .mr-from');
    edit(mrFrom, 'a2');
    check('a model-rules row edit collects only the model_rules key',
      JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0);
    checkDirty('a model-rules row edit counts one unsaved key', 1);
    edit(mrFrom, 'a');
    checkClean('reverting the model-rules row restores the clean draft');
    resetCounters();
    const roClient = box.querySelector('[data-kind="request_overrides"] .ro-client');
    edit(roClient, 'cli2');
    check('a request-override row edit collects only the request_overrides key',
      JSON.stringify(keyCalls) === JSON.stringify(['request_overrides']) && fullCalls === 0);
    checkDirty('a request-override row edit counts one unsaved key', 1);
    edit(roClient, 'cli');
    checkClean('reverting the request-override row restores the clean draft');
    resetCounters();
    const scClient = box.querySelector('[data-kind="sub_conversations"] .sc-client');
    edit(scClient, 'cli2');
    check('a sub-conversation edit collects only the sub_conversations key',
      JSON.stringify(keyCalls) === JSON.stringify(['sub_conversations']) && fullCalls === 0);
    checkDirty('a sub-conversation edit counts one unsaved key', 1);
    edit(scClient, 'cli');
    checkClean('reverting the sub-conversation edit restores the clean draft');
    // Structural actions each update the cached key directly.
    const provWrap = box.querySelector('[data-kind="providers"]');
    const provCard = provWrap.querySelector('.st-prov');
    resetCounters();
    w.addProviderLabel('delta.example', provCard);
    check('adding a provider card collects only the providers key',
      JSON.stringify(keyCalls) === JSON.stringify(['providers']) && fullCalls === 0);
    checkDirty('adding a provider card counts one unsaved key', 1);
    resetCounters();
    [...provWrap.querySelectorAll('.st-prov')].pop().querySelector('[data-prov-rm]').click();
    check('removing a provider card collects only the providers key',
      JSON.stringify(keyCalls) === JSON.stringify(['providers']) && fullCalls === 0);
    checkClean('removing the added provider card restores the clean draft');
    const mrWrap = box.querySelector('[data-kind="model_rules"]');
    const mrAdd = mrWrap.querySelector('.mr-add');
    mrAdd.querySelector('.mr-mode').value = 'exact';
    mrAdd.querySelector('.mr-new-from').value = 'x';
    mrAdd.querySelector('.mr-new-to').value = 'y';
    resetCounters();
    w.addModelRuleRow(mrAdd);
    check('adding a model-rule row collects only the model_rules key',
      JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0);
    checkDirty('adding a model-rule row counts one unsaved key', 1);
    resetCounters();
    [...mrWrap.querySelectorAll('.mr-row')].pop().querySelector('[data-mr-rm]').click();
    check('removing a model-rule row collects only the model_rules key',
      JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0);
    checkClean('removing the added model-rule row restores the clean draft');
    // The preview-only test spelling drives the bench alone: it must not
    // revalidate the draft, collect the model_rules key or arm Apply/Revert.
    // Rule edits keep the live validation and per-key dirty behavior.
    {
      const mrTest = mrWrap.querySelector('.mr-test');
      const baseline = JSON.stringify(w.collectSettingsValues().model_rules);
      const originalValidate = w.validateModelRulesDraft;
      const originalMark = w.markSettingsDirty;
      let validations = 0;
      let marks = 0;
      w.validateModelRulesDraft = wrap => { validations++; return originalValidate(wrap); };
      w.markSettingsDirty = key => { marks++; return originalMark(key); };
      try {
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrTest, 'a');
        const testKeyCalls = keyCalls.slice();
        const testFullCalls = fullCalls;
        const preview = mrWrap.querySelector('.mr-out').textContent;
        const collected = JSON.stringify(w.collectSettingsValues().model_rules);
        check('the preview-only test spelling refreshes the bench without revalidating or dirtying the draft',
          validations === 0 && marks === 0 && preview === 'b');
        check('the test spelling collects nothing, leaves the model-rules value unchanged and keeps Apply/Revert disabled',
          testKeyCalls.length === 0 && testFullCalls === 0 && collected === baseline &&
          !w.settingsIsDirty() && w.settingsDirtyCount() === 0 &&
          status() === '' && apply.disabled && revert.disabled);
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrFrom, 'a2');
        check('a rule spelling edit still validates once and marks only model_rules dirty',
          validations === 1 && marks === 1 &&
          JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0 &&
          w.settingsDirtyCount() === 1 && status() === '1 unsaved' &&
          !apply.disabled && !revert.disabled);
        edit(mrFrom, '');
        check('an empty rule spelling still renders its error state',
          mrWrap.querySelector('.mr-row').dataset.mrState === 'error' &&
          /needs a from spelling/.test(mrWrap.querySelector('.mr-row .mr-err').textContent));
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrTest, 'a2');
        check('the test spelling stays preview-only beside an invalid rule row',
          validations === 0 && marks === 0 && JSON.stringify(keyCalls) === '[]' && fullCalls === 0 &&
          mrWrap.querySelector('.mr-row').dataset.mrState === 'error' &&
          mrWrap.querySelector('.mr-out').textContent === 'a2');
        edit(mrFrom, 'a');
        checkClean('reverting the rule spelling restores the clean draft after preview-only typing');
        mrWrap.querySelector('.mr-row [data-mr-vis]').click();
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrTest, 'a');
        check('a parked rule still shows its state and the test spelling neither revalidates nor collects',
          mrWrap.querySelector('.mr-row').dataset.mrState === 'off' &&
          mrWrap.querySelector('.mr-row').dataset.mrOff === '1' &&
          mrWrap.querySelector('.mr-out').textContent === 'a' &&
          validations === 0 && marks === 0 && JSON.stringify(keyCalls) === '[]' && fullCalls === 0 &&
          w.settingsDirtyCount() === 1 && status() === '1 unsaved');
        mrWrap.querySelector('.mr-row [data-mr-vis]').click();
        checkClean('un-parking the rule restores the clean draft');
      } finally {
        w.validateModelRulesDraft = originalValidate;
        w.markSettingsDirty = originalMark;
      }
    }
    const roWrap = box.querySelector('[data-kind="request_overrides"]');
    resetCounters();
    roWrap.querySelector('[data-ro-add]').click();
    roWrap.querySelector('[data-ro-tpl="blank"]').click();
    check('adding a request-override rule collects only the request_overrides key',
      JSON.stringify(keyCalls) === JSON.stringify(['request_overrides']) && fullCalls === 0);
    checkDirty('adding a request-override rule counts one unsaved key', 1);
    resetCounters();
    [...roWrap.querySelectorAll('.ro-rule')].pop().querySelector('[data-ro-rm]').click();
    check('removing a request-override rule collects only the request_overrides key',
      JSON.stringify(keyCalls) === JSON.stringify(['request_overrides']) && fullCalls === 0);
    checkClean('removing the added request-override rule restores the clean draft');
    const scWrap = box.querySelector('[data-kind="sub_conversations"]');
    resetCounters();
    scWrap.querySelector('[data-sc-add]').click();
    check('adding a sub-conversation entry collects only the sub_conversations key',
      JSON.stringify(keyCalls) === JSON.stringify(['sub_conversations']) && fullCalls === 0);
    checkDirty('adding a sub-conversation entry counts one unsaved key', 1);
    resetCounters();
    [...scWrap.querySelectorAll('.sc-card')].pop().querySelector('[data-sc-rm]').click();
    check('removing a sub-conversation entry collects only the sub_conversations key',
      JSON.stringify(keyCalls) === JSON.stringify(['sub_conversations']) && fullCalls === 0);
    checkClean('removing the added sub-conversation entry restores the clean draft');
    // No resolvable key: the full collector still runs and catches the change.
    resetCounters();
    const victim = box.querySelector('input[data-key="bulk_3"]');
    victim.value = 'unnoticed change';
    const fakeRow = d.createElement('div');
    fakeRow.className = 'st-row';
    fakeRow.innerHTML = '<input class="no-key" value="x">';
    box.appendChild(fakeRow);
    fakeRow.querySelector('input').dispatchEvent(inputEvent());
    check('an input without a resolvable key falls back to the full collector',
      fullCalls === 1 && keyCalls.length === 0 &&
      w.settingsDirtyCount() === 1 && status() === '1 unsaved' && !apply.disabled);
    fakeRow.remove();
    edit(victim, '3');
    checkClean('the fallback dirty state clears through the incremental path after the revert');
    // A refill resets the cached per-key snapshots and the changed set.
    edit(bulk1, 'stale draft');
    checkDirty('a draft arms the dirty state before the refill', 1);
    w.eval('settingsDoc = window.__dirtyDoc; fillSettingsForm(settingsDoc)');
    checkClean('a settings refill resets the per-key snapshots and changed set');
    w.collectSettingsValue = originalKeyCollector;
    w.collectSettingsValues = originalFullCollector;
    // A save accepted mid-typing rebases the per-key baseline: only the keys
    // changed after the submission stay unsaved.
    {
      const originalFetch = w.fetch;
      const posts = [];
      w.fetch = (url, init) => {
        if (!String(url).includes('/admin/config')) return originalFetch(url, init);
        return new Promise(resolve => {
          if (init?.method === 'POST') posts.push({ resolve, body: JSON.parse(init.body) });
          else resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
        });
      };
      edit(box.querySelector('input[data-key="bulk_1"]'), 'saved one');
      const save = w.applySettings();
      edit(box.querySelector('input[data-key="bulk_2"]'), 'typed during save');
      const submitted = JSON.parse(JSON.stringify(posts[0].body.values));
      posts.shift().resolve({ ok: true, statusText: 'OK', json: async () => ({ saved: true, revision: 'dirty-r2', values: submitted, effective: {}, restart_required: [] }) });
      await save;
      check('a save accepted mid-typing counts only the keys typed after submission',
        w.settingsDirtyCount() === 1 &&
        d.getElementById('settings-count').textContent === 'saved · 1 unsaved changes' &&
        w.eval('settingsDoc.revision') === 'dirty-r2' && !apply.disabled && !revert.disabled);
      w.fetch = originalFetch;
    }
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__dirtyDoc;
  }

  // ---- test 11u: the model-rules preview caches its ring-derived traffic
  // examples per live data revision, so a preview pass never rescans the
  // 10k ring or re-sorts the model counts. A revision bump or a records
  // array replacement (a manual fixture swap without a bump) invalidates it.
  {
    const wrap = d.createElement('div');
    wrap.innerHTML = w.modelRulesEditorHTML([{ mode: 'lower' }]);
    w.__mrCacheWrap = wrap;
    let cache;
    try {
      cache = w.eval(`(() => {
        const box = window.__mrCacheWrap;
        const counts = [1500, 1400, 1300, 1200, 1100, 1000, 900, 800, 500, 300];
        const records = [];
        counts.forEach((n, i) => { for (let j = 0; j < n; j++) records.push({ model: 'm' + i }); });
        window.__mrCacheSaved = lastData;
        lastData = { _rev: 1, records };
        mrPreview(box);
        const first = mrTrafficExamples();
        const origSort = Array.prototype.sort;
        const origMapSet = Map.prototype.set;
        let sorts = 0, sets = 0;
        Array.prototype.sort = function(...args) { sorts++; return origSort.apply(this, args); };
        Map.prototype.set = function(...args) { sets++; return origMapSet.apply(this, args); };
        let second;
        try {
          mrPreview(box);
          second = mrTrafficExamples();
        } finally {
          Array.prototype.sort = origSort;
          Map.prototype.set = origMapSet;
        }
        const top = first.map(([name, n]) => name + ':' + n);
        const hit = second === first;
        lastData._rev += 1;
        const afterRevOnly = mrTrafficExamples();
        const revOnlyFresh = afterRevOnly !== first;
        const revOnlySame = JSON.stringify(afterRevOnly) === JSON.stringify(first);
        lastData.records.forEach(r => { if (r.model === 'm0') r.model = 'zeta'; });
        lastData._rev += 1;
        const afterRev = mrTrafficExamples();
        const replacement = [];
        for (let i = 0; i < 4000; i++) replacement.push({ model: 'epsilon' });
        for (let i = 0; i < 2000; i++) replacement.push({ model: 'phi' });
        lastData.records = replacement;
        const afterReplace = mrTrafficExamples();
        return {
          hit, sorts, sets, ring: records.length, top, revOnlyFresh, revOnlySame,
          revFresh: afterRev !== afterRevOnly,
          revTop: afterRev.map(([name, n]) => name + ':' + n),
          replaceFresh: afterReplace !== afterRev,
          replaceTop: afterReplace.map(([name, n]) => name + ':' + n),
        };
      })()`);
    } finally {
      w.eval('lastData = window.__mrCacheSaved; delete window.__mrCacheSaved;');
      delete w.__mrCacheWrap;
    }
    check('the 10k traffic-example scan still returns the top eight spellings in count order',
      cache.ring === 10000 &&
      cache.top.join(',') === 'm0:1500,m1:1400,m2:1300,m3:1200,m4:1100,m5:1000,m6:900,m7:800');
    check('a second preview pass reuses the cached traffic examples without rebuilding the count map or sorting',
      cache.hit && cache.sorts === 0 && cache.sets === 0);
    check('a live-revision bump invalidates the cached traffic examples',
      cache.revOnlyFresh && cache.revOnlySame && cache.revFresh && cache.revTop[0] === 'zeta:1500');
    check('replacing the records array without a revision bump invalidates the cached traffic examples',
      cache.replaceFresh && cache.replaceTop.join(',') === 'epsilon:4000,phi:2000');
  }
};
