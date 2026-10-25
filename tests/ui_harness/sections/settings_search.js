// GAP-5 pin: the settings search filter (rowMatches/showSettingsCat).
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function settingsSearch(H) {
  const {cfgDoc, check, d, settleUntil, sleep, w} = H;
  const state = H.state;

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
};
