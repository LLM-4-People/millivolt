// request-overrides editor: the settings dashboard surface
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function requestOverrides(H) {
  const {cfgDoc, check, d, settleUntil, w} = H;
  const state = H.state;

  // ---- request-overrides editor: the settings dashboard surface ----
  // The W47 spec's settings-dashboard section: the editor renders the
  // server doc's seeded rules, edits collect round-trip to the strict POST
  // shape, add/delete rows work, live per-card validation mirrors
  // config.validateRequestOverrides, and Apply blocks on the first invalid
  // card before the POST. Scope datalists seed from the settings doc's
  // provider labels plus the dashboard's known sets (the wire-key owners).
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.push({ key: 'request_overrides', category: 'overrides', label: 'Request override rules', kind: 'request_overrides', hot_reload: true });
    doc.categories.push({ id: 'overrides', label: 'Request overrides', help: 'Scoped rewrites of the upstream request.' });
    doc.values.request_overrides = [
      { client: 'claude-code', provider: '', model: '', headers: { 'X-Title': 'my app' }, remove_headers: null, body: null },
      { client: '', provider: 'epsilon.example', model: '', headers: null, remove_headers: ['User-Agent'], body: { max_tokens: 32768, max_completion_tokens: null } },
    ];
    doc.revision = 'ro-r1';
    // The known sets are unioned across the operator states at datalist
    // render time; pin them to the stubbed wire values before the render.
    w.eval(`(() => {
      pauseState = { ...pauseState, [KNOWN_CLIENTS_KEY]: ['c'], [KNOWN_PROVIDERS_KEY]: ['epsilon.example', 'p'] };
      debugState = { ...debugState, [KNOWN_CLIENTS_KEY]: ['c'], [KNOWN_PROVIDERS_KEY]: ['epsilon.example', 'p'], [KNOWN_MODELS_KEY]: ['m'] };
      throttleState = { ...throttleState, [KNOWN_PROVIDERS_KEY]: ['p'] };
    })()`);
    w.__roDoc = doc;
    w.eval("settingsCat = 'overrides'; settingsDoc = window.__roDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const roWrap = d.querySelector('#settings-fields [data-kind="request_overrides"]');
    check('the settings sheet renders the request-overrides editor for the field', !!roWrap);
    const cards = () => [...roWrap.querySelectorAll('.ro-rule')];
    const roErrorDetail = input => {
      const id = input && input.getAttribute('aria-describedby');
      const detail = id && input.ownerDocument.getElementById(id);
      return detail && detail.classList.contains('ro-error-detail') ? detail : null;
    };
    check('seeded rules render as ordered cards with scope, header rows, chips and body',
      cards().length === 2 &&
      cards()[0].querySelector('.ro-client').value === 'claude-code' &&
      cards()[0].querySelector('.prov-hmap .sp-hname').value === 'X-Title' &&
      cards()[1].querySelector('.ro-provider').value === 'epsilon.example' &&
      cards()[1].querySelector('.prov-chip').dataset.rh === 'User-Agent' &&
      cards()[1].querySelector('.ro-max-tokens').value === '32768');
    const initialRoCards = cards();
    check('valid seeded rules render native collapsed disclosures with distinct wildcard and action summaries',
      initialRoCards.map(card => card.querySelector('.ro-summary-scope').textContent).join('|') === 'client claude-code · provider any · model any|client any · provider epsilon.example · model any' &&
      initialRoCards.every(card => {
        const toggle = card.querySelector('[data-ro-toggle]');
        const body = card.querySelector('.ro-rule-body');
        return toggle && toggle.tagName === 'BUTTON' && toggle.getAttribute('aria-expanded') === 'false' &&
          body && body.hidden && body.id === toggle.getAttribute('aria-controls') &&
          card.querySelector('.ro-summary-actions').textContent.includes('header');
      }));
    check('scope and body controls carry visible field labels',
      initialRoCards.every(card => JSON.stringify([...card.querySelectorAll('.ro-field > span')].map(label => label.textContent)) === JSON.stringify(['client', 'provider', 'model', 'max_tokens', 'max_completion_tokens'])));
    const initialToggle = initialRoCards[0].querySelector('[data-ro-toggle]');
    const initialBody = initialRoCards[0].querySelector('.ro-rule-body');
    initialToggle.click();
    check('the request-override disclosure synchronizes its button and body state',
      initialToggle.getAttribute('aria-expanded') === 'true' && !initialBody.hidden &&
      initialToggle.getAttribute('aria-controls') === initialBody.id);
    initialToggle.click();
    check('collapsing a valid request-override card is view-only',
      initialToggle.getAttribute('aria-expanded') === 'false' && initialBody.hidden && !w.settingsIsDirty());
    const summaryInput = initialRoCards[0].querySelector('.ro-client');
    summaryInput.value = '<b>scope</b>';
    summaryInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the request-override scope summary stays readable and text-only for hostile values',
      initialRoCards[0].querySelector('.ro-summary-scope').textContent.includes('<b>scope</b>') &&
      !initialRoCards[0].querySelector('.ro-summary-scope').querySelector('b'));
    summaryInput.value = 'claude-code';
    summaryInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    const literalAsteriskInput = cards()[0].querySelector('.ro-model');
    literalAsteriskInput.value = '*';
    literalAsteriskInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('literal asterisks remain distinct from empty wildcards in the summary',
      cards()[0].querySelector('.ro-summary-scope').textContent === 'client claude-code · provider any · model * (exact)');
    literalAsteriskInput.value = '';
    literalAsteriskInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    const literalAnyInput = cards()[0].querySelector('.ro-provider');
    literalAnyInput.value = 'any';
    literalAnyInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('literal any remains distinct from the empty wildcard in the summary',
      cards()[0].querySelector('.ro-summary-scope').textContent === 'client claude-code · provider "any" (exact) · model any');
    literalAnyInput.value = '';
    literalAnyInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    const trimClientInput = cards()[0].querySelector('.ro-client');
    const trimHeaderInput = cards()[0].querySelector('.sp-hval');
    const trimClientOriginal = trimClientInput.value;
    const trimHeaderOriginal = trimHeaderInput.value;
    trimClientInput.value = ' \uFEFFclaude-code ';
    trimHeaderInput.value = '  \uFEFFvalue with spaces  ';
    trimClientInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    trimHeaderInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    const trimmedRo = w.collectSettingsValues().request_overrides[0];
    check('request-override collection matches Go whitespace trimming without removing U+FEFF boundaries',
      trimmedRo.client === '\uFEFFclaude-code' && trimmedRo.headers['X-Title'] === '\uFEFFvalue with spaces');
    trimClientInput.value = trimClientOriginal;
    trimHeaderInput.value = trimHeaderOriginal;
    trimClientInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    trimHeaderInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the request-override whitespace fixture restores the clean draft', !w.settingsIsDirty());
    const u85NameInput = cards()[0].querySelector('.prov-add-h .sp-hname');
    const u85ValueInput = cards()[0].querySelector('.prov-add-h .sp-hval');
    u85NameInput.value = '\u0085X-U85\u0085';
    u85ValueInput.value = '\u0085value with spaces\u0085';
    u85ValueInput.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    const u85Row = [...cards()[0].querySelectorAll('.prov-hrow')].pop();
    u85NameInput.value = 'X-FEFF';
    u85ValueInput.value = '\uFEFFvalue with FEFF\uFEFF';
    u85ValueInput.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    const feffRow = [...cards()[0].querySelectorAll('.prov-hrow')].pop();
    const u85Collect = w.collectSettingsValues().request_overrides[0];
    check('request-override header addition trims U+0085 but preserves U+FEFF',
      u85Row.querySelector('.sp-hname').value === 'X-U85' &&
      u85Row.querySelector('.sp-hval').value === 'value with spaces' &&
      feffRow.querySelector('.sp-hname').value === 'X-FEFF' &&
      feffRow.querySelector('.sp-hval').value === '\uFEFFvalue with FEFF\uFEFF' &&
      u85Collect.headers['X-U85'] === 'value with spaces' &&
      u85Collect.headers['X-FEFF'] === '\uFEFFvalue with FEFF\uFEFF');
    [u85Row, feffRow].forEach(row => row.remove());
    w.roDraftChanged(roWrap);
    check('the U+0085 header-add fixture restores the clean draft', !w.settingsIsDirty());
    check('empty scope fields carry the explicit any watermark with one concrete example',
      cards()[1].querySelector('.ro-client').placeholder.includes('any client') &&
      cards()[0].querySelector('.ro-provider').placeholder.includes('any provider') &&
      cards()[0].querySelector('.ro-model').placeholder.includes('any model'));
    check('the rule count line renders after the initial paint',
      roWrap.querySelector('.mr-count').textContent === '2 / 64 rules');
    const capHost = d.createElement('div');
    capHost.innerHTML = w.requestOverridesEditorHTML(Array.from({ length: 64 }, () => ({})));
    const capWrap = capHost.querySelector('.ro-wrap');
    w.roSyncCount(capWrap);
    check('the request-override add control and template menu respect the 64-rule cap',
      capWrap.querySelector('[data-ro-add]').disabled && !capWrap.querySelector('.ro-tpl') &&
      [...capWrap.querySelectorAll('[data-ro-tpl]')].every(item => item.disabled));
    check('scope datalists seed from the settings doc provider labels and the known sets',
      [...roWrap.querySelectorAll('#ro-dl-provider option')].map(o => o.value).join(',') === 'epsilon.example,p' &&
      [...roWrap.querySelectorAll('#ro-dl-client option')].map(o => o.value).join(',') === 'c' &&
      [...roWrap.querySelectorAll('#ro-dl-model option')].map(o => o.value).join(',') === 'm');
    check('collect round-trips the rules losslessly with empty collections omitted',
      JSON.stringify(w.collectSettingsValues().request_overrides) === JSON.stringify([
        { client: 'claude-code', provider: '', model: '', headers: { 'X-Title': 'my app' } },
        { client: '', provider: 'epsilon.example', model: '', remove_headers: ['User-Agent'], body: { max_tokens: 32768 } },
      ]));
    check('the clean request-overrides draft reports no unsaved changes', !w.settingsIsDirty());
    const firstMoveDown = cards()[0].querySelector('[data-ro-dn]');
    firstMoveDown.click();
    check('Move down reorders the actual request-override DOM and collection without sorting a parallel model',
      cards().map(card => card.querySelector('.ro-client').value + '|' + card.querySelector('.ro-provider').value).join(',') === '|epsilon.example,claude-code|' &&
      w.collectSettingsValues().request_overrides.map(rule => rule.client + '|' + rule.provider).join(',') === '|epsilon.example,claude-code|' &&
      d.activeElement === firstMoveDown);
    check('reordered request-override actions and ordinals stay synchronized',
      cards()[0].querySelector('.ro-num').textContent === 'rule 1' &&
      cards()[0].querySelector('[data-ro-up]').getAttribute('aria-label') === 'rule 1 move up' &&
      cards()[0].querySelector('[data-ro-dn]').getAttribute('aria-label') === 'rule 1 move down' &&
      cards()[0].querySelector('[data-ro-rm]').getAttribute('aria-label') === 'rule 1 remove rule' &&
      cards()[0].querySelector('.ro-rule-body').getAttribute('aria-label') === 'rule 1 details');
    const movedUp = cards()[1].querySelector('[data-ro-up]');
    movedUp.click();
    check('Move up restores DOM and collected order while keeping the moved control focused',
      cards().map(card => card.querySelector('.ro-client').value + '|' + card.querySelector('.ro-provider').value).join(',') === 'claude-code|,|epsilon.example' &&
      w.collectSettingsValues().request_overrides.map(rule => rule.client + '|' + rule.provider).join(',') === 'claude-code|,|epsilon.example' &&
      d.activeElement === movedUp);
    const roAddButton = roWrap.querySelector('[data-ro-add]');
    const roTemplateMenu = roWrap.querySelector('.ro-tpl-menu');
    const addRoTemplate = kind => {
      roAddButton.click();
      roTemplateMenu.querySelector(`[data-ro-tpl="${kind}"]`).click();
    };
    roAddButton.click();
    check('the request-override Add rule control opens the small template menu',
      !roTemplateMenu.hidden && roAddButton.getAttribute('aria-expanded') === 'true' &&
      !roAddButton.hasAttribute('aria-haspopup') && !roTemplateMenu.hasAttribute('role') &&
      [...roTemplateMenu.querySelectorAll('[data-ro-tpl]')].every(item => !item.hasAttribute('role')) &&
      [...roTemplateMenu.querySelectorAll('[data-ro-tpl]')].map(item => item.dataset.roTpl).join(',') === 'blank,budget,header' &&
      d.activeElement === roTemplateMenu.querySelector('[data-ro-tpl="blank"]'));
    const roSecondItem = roTemplateMenu.querySelector('[data-ro-tpl="budget"]');
    roSecondItem.focus();
    check('focus moving between request-override template items keeps the menu open',
      await settleUntil(() => !roTemplateMenu.hidden &&
        roAddButton.getAttribute('aria-expanded') === 'true' && d.activeElement === roSecondItem));
    const roEscape = new w.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    roSecondItem.dispatchEvent(roEscape);
    check('Escape from the request-override template menu keeps Settings open and restores Add rule focus',
      roEscape.defaultPrevented && !d.getElementById('settings-sheet').hidden && roTemplateMenu.hidden &&
      roAddButton.getAttribute('aria-expanded') === 'false' && d.activeElement === roAddButton);
    roAddButton.click();
    roTemplateMenu.querySelector('[data-ro-tpl="blank"]').dispatchEvent(new w.KeyboardEvent('keydown', { key: '/', bubbles: true, cancelable: true }));
    check('the Settings search shortcut dismisses the request-override template menu',
      await settleUntil(() => !d.getElementById('settings-sheet').hidden && roTemplateMenu.hidden &&
        roAddButton.getAttribute('aria-expanded') === 'false' &&
        d.activeElement === d.getElementById('settings-q')));
    roAddButton.click();
    roTemplateMenu.querySelector('[data-ro-tpl="header"]').click();
    const menuCard = cards()[2];
    check('the template menu closes after selection and focuses the intended first scope field',
      cards().length === 3 && roTemplateMenu.hidden && roAddButton.getAttribute('aria-expanded') === 'false' &&
      menuCard.querySelector('.ro-rule-body').hidden === false && d.activeElement === menuCard.querySelector('.ro-client'));
    menuCard.querySelector('[data-ro-rm]').click();
    check('removing the tail request-override card focuses the previous surviving card',
      cards().length === 2 && d.activeElement === cards()[1]);
    // template add: the guided-input discipline - a fresh card immediately
    // asks for its scope, and the sheet counts itself dirty.
    addRoTemplate('blank');
    check('the empty-rule template adds a card that asks for its scope and marks the sheet dirty',
      cards().length === 3 && cards()[2].dataset.roState === 'error' &&
      cards()[2].querySelector('.ro-err').textContent.includes('no scope set') &&
      w.settingsIsDirty() && d.getElementById('settings-count').textContent === '1 unsaved');
    const blank = cards()[2];
    const blankError = blank.querySelector('.ro-err');
    const blankScopeInputs = [...blank.querySelectorAll('.ro-client, .ro-provider, .ro-model')];
    const blankScopeDetails = blankScopeInputs.map(roErrorDetail);
    check('a new invalid request-override card opens and wires distinct detail text to every failed scope input',
      blank.querySelector('.ro-rule-body').hidden === false &&
      blankError.id && blankError.getAttribute('role') === 'status' &&
      blankScopeInputs.every(input => input.getAttribute('aria-invalid') === 'true') &&
      blankScopeDetails.every(detail => detail && detail.textContent.includes('no scope set')) &&
      new Set(blankScopeDetails.map(detail => detail && detail.id)).size === 3);
    blank.querySelector('.ro-client').value = 'zed-cli';
    blank.querySelector('.ro-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a scoped card with no action names the missing action',
      blank.dataset.roState === 'error' && blank.querySelector('.ro-err').textContent.includes('no action set'));
    check('the failed action input remains described by its own error detail',
      blank.querySelector('.prov-add-h .sp-hname').getAttribute('aria-invalid') === 'true' &&
      roErrorDetail(blank.querySelector('.prov-add-h .sp-hname'))?.textContent.includes('no action set'));
    blank.querySelector('.ro-rh-in').value = 'X-Old-Thing';
    blank.querySelector('[data-ro-rh-add]').click();
    check('a remove_headers chip completes the action and clears the row error',
      blank.querySelector('.prov-chip').dataset.rh === 'X-Old-Thing' &&
      blank.dataset.roState !== 'error' && blank.querySelector('.ro-err').textContent === '');
    check('repairing a request-override card clears its invalid input wiring and details',
      [...blank.querySelectorAll('input')].every(input => !input.hasAttribute('aria-invalid') && !input.hasAttribute('aria-describedby')) &&
      !blank.querySelector('.ro-error-detail') && blank.querySelector('.ro-err').textContent === '');
    const removeValidationCases = [
      { label: 'invalid remove chip', value: 'Bad Header', detail: 'not a valid header name' },
      { label: 'forbidden remove chip', value: 'Authorization', detail: 'credential-owned' },
      { label: 'duplicate remove chip', value: 'X-Old-Thing', detail: 'duplicate HTTP header name' },
    ];
    const flashMs = w.eval('INPUT_FLASH_MS');
    const captureInputFlash = fn => {
      const originalSetTimeout = w.setTimeout;
      const pending = [];
      w.setTimeout = (callback, delay) => {
        if (delay === flashMs) {
          pending.push(callback);
          return 0;
        }
        return originalSetTimeout(callback, delay);
      };
      try { fn(); } finally { w.setTimeout = originalSetTimeout; }
      pending.forEach(callback => callback());
    };
    for (const removeCase of removeValidationCases) {
      const chips = blank.querySelector('.prov-chips');
      chips.insertAdjacentHTML('beforeend', w.roRemoveChipHTML(removeCase.value));
      const chip = [...chips.querySelectorAll('.prov-chip')].pop();
      const button = chip.querySelector('[data-prov-chip-rm]');
      w.roDraftChanged(roWrap);
      const removeFailuresBefore = state.failures.length;
      captureInputFlash(() => d.getElementById('btn-settings-apply').click());
      check(`${removeCase.label} targets its remove button`,
        state.failures.length === removeFailuresBefore && d.activeElement === button && button.classList.contains('prov-bad') &&
        button.getAttribute('aria-invalid') === 'true' && roErrorDetail(button)?.textContent.includes(removeCase.detail));
      check(`${removeCase.label} keeps its visual and ARIA error state`,
        button.classList.contains('prov-bad') && button.getAttribute('aria-invalid') === 'true' &&
        roErrorDetail(button)?.textContent.includes(removeCase.detail));
      button.click();
      check(`${removeCase.label} removal focuses the previous chip`,
        blank.querySelectorAll('.prov-chip').length === 1 && d.activeElement === blank.querySelector('.prov-chip [data-prov-chip-rm]'));
    }
    blank.querySelector('[data-ro-rm]').click();
    check('the remove button deletes the card and renumbers the survivors',
      cards().length === 2 &&
      [...roWrap.querySelectorAll('.ro-num')].map(n => n.textContent).join(',') === 'rule 1,rule 2');
    // the budget starter seeds BOTH token spellings with the same value: the
    // cap pair is precedence-merged (max_completion_tokens wins), so a
    // single-field template could not raise a request that carries the other
    // spelling.
    addRoTemplate('budget');
    check('the budget template seeds both token ceilings with the same value and still asks for scope',
      cards().length === 3 &&
      cards()[2].querySelector('.ro-max-tokens').value === '32768' &&
      cards()[2].querySelector('.ro-max-mct').value === '32768' &&
      cards()[2].querySelector('.ro-rule-body').hidden === false &&
      cards()[2].dataset.roState === 'error' &&
      cards()[2].querySelector('.ro-err').textContent.includes('no scope set') &&
      d.activeElement === cards()[2].querySelector('.ro-provider'));
    cards()[2].querySelector('[data-ro-rm]').click();
    check('removing the budget card restores the clean two-rule draft',
      cards().length === 2 && !w.settingsIsDirty());
    // a MIDDLE-card removal: both removals above are tail removals, so
    // neither proves the renumber rewrite is load-bearing. Removing a
    // non-last card must renumber both the visible ordinals and the
    // ordinal-carrying aria-labels, which share roRuleLabel with the
    // visible numbers and must not go stale.
    addRoTemplate('blank');
    const midBefore = cards();
    midBefore[1].querySelector('[data-ro-rm]').click();
    const midSurvivors = cards();
    check('a middle-card removal renumbers the survivors\' visible ordinals',
      midBefore.length === 3 && midSurvivors.length === 2 &&
      midSurvivors[0].querySelector('.ro-num').textContent === 'rule 1' &&
      midSurvivors[1].querySelector('.ro-num').textContent === 'rule 2');
    check('a middle-card removal focuses the next surviving request-override card',
      d.activeElement === midSurvivors[1]);
    check('the renumbered survivors\' aria-labels carry the same ordinals and visible token labels',
      midSurvivors[0].querySelector('.ro-client').getAttribute('aria-label') === 'rule 1 client scope' &&
      midSurvivors[1].querySelector('.ro-client').getAttribute('aria-label') === 'rule 2 client scope' &&
      midSurvivors[1].querySelector('.ro-max-tokens').getAttribute('aria-label') === 'rule 2 max_tokens ceiling' &&
      midSurvivors[1].querySelector('.ro-max-mct').getAttribute('aria-label') === 'rule 2 max_completion_tokens ceiling');
    // restore the seeded two-rule draft: drop the template card that made
    // the removal a middle one, then rebuild the removed rule behind the
    // survivor through the real controls so the later rows keep their
    // seeded state.
    midSurvivors[1].querySelector('[data-ro-rm]').click();
    addRoTemplate('blank');
    const rebuilt = cards()[1];
    rebuilt.querySelector('.ro-provider').value = 'epsilon.example';
    rebuilt.querySelector('.ro-rh-in').value = 'User-Agent';
    rebuilt.querySelector('[data-ro-rh-add]').click();
    rebuilt.querySelector('.ro-max-tokens').value = '32768';
    rebuilt.querySelector('.ro-max-tokens').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('rebuilding the removed rule restores the clean two-rule draft',
      cards().length === 2 && !w.settingsIsDirty());
    const focusHeaderCard = cards()[1];
    const focusHeaderMap = focusHeaderCard.querySelector('.prov-hmap');
    ['X-First', 'X-Second', 'X-Third'].forEach(name => focusHeaderMap.insertAdjacentHTML('beforeend', w.headerRowHTML(name, 'value')));
    w.roDraftChanged(roWrap);
    let focusHeaderRows = [...focusHeaderMap.querySelectorAll('.prov-hrow')];
    focusHeaderRows[1].querySelector('[data-prov-hrow-rm]').click();
    check('removing a non-tail request-override header row focuses the next row',
      focusHeaderMap.querySelectorAll('.prov-hrow').length === 2 &&
      d.activeElement === focusHeaderMap.querySelectorAll('.prov-hrow')[1].querySelector('.sp-hname'));
    focusHeaderRows = [...focusHeaderMap.querySelectorAll('.prov-hrow')];
    focusHeaderRows[1].querySelector('[data-prov-hrow-rm]').click();
    check('removing a tail request-override header row focuses the previous row',
      focusHeaderMap.querySelectorAll('.prov-hrow').length === 1 &&
      d.activeElement === focusHeaderMap.querySelector('.prov-hrow .sp-hname'));
    focusHeaderMap.querySelector('[data-prov-hrow-rm]').click();
    check('removing the final request-override header row focuses the add input',
      focusHeaderMap.querySelectorAll('.prov-hrow').length === 0 &&
      d.activeElement === focusHeaderCard.querySelector('.prov-add-h .sp-hname'));
    const collectCard = cards()[0];
    const collectToggle = collectCard.querySelector('[data-ro-toggle]');
    if (collectToggle.getAttribute('aria-expanded') === 'true') collectToggle.click();
    check('the collect-time request-overrides fixture starts with a manually collapsed card',
      collectToggle.getAttribute('aria-expanded') === 'false' && collectCard.querySelector('.ro-rule-body').hidden);
    const collectHeaderMap = collectCard.querySelector('.prov-hmap');
    collectHeaderMap.insertAdjacentHTML('beforeend', w.headerRowHTML('X-Title', 'second'));
    const collectBadInput = collectHeaderMap.lastElementChild.querySelector('.sp-hname');
    w.markSettingsDirty();
    const collectFetch = w.fetch;
    let collectPosts = 0;
    w.fetch = (url, init) => {
      if (init?.method === 'POST') collectPosts++;
      return collectFetch(url, init);
    };
    const collectFailuresBefore = state.failures.length;
    w.applySettings();
    check('a collect-time duplicate header opens its collapsed card, focuses the offender and sends no POST',
      state.failures.length === collectFailuresBefore && collectPosts === 0 &&
      !collectCard.querySelector('.ro-rule-body').hidden && collectToggle.getAttribute('aria-expanded') === 'true' &&
      d.activeElement === collectBadInput && collectBadInput.classList.contains('prov-bad') &&
      collectBadInput.getAttribute('aria-invalid') === 'true' &&
      roErrorDetail(collectBadInput)?.textContent.includes('duplicate HTTP header name'));
    w.fetch = collectFetch;
    collectHeaderMap.lastElementChild.remove();
    w.roDraftChanged(roWrap);
    const earlierCollectClient = collectCard.querySelector('.ro-client');
    const earlierCollectClientOriginal = earlierCollectClient.value;
    const laterCollectCard = cards()[1];
    const laterCollectMap = laterCollectCard.querySelector('.prov-hmap');
    const laterCollectOriginal = laterCollectMap.innerHTML;
    earlierCollectClient.value = '';
    earlierCollectClient.dispatchEvent(new w.Event('input', { bubbles: true }));
    laterCollectMap.insertAdjacentHTML('beforeend', w.headerRowHTML('X-Later', 'one'));
    laterCollectMap.insertAdjacentHTML('beforeend', w.headerRowHTML('X-Later', 'two'));
    const laterCollectBadInput = laterCollectMap.lastElementChild.querySelector('.sp-hname');
    w.markSettingsDirty();
    const earlierCollectFetch = w.fetch;
    let earlierCollectPosts = 0;
    w.fetch = (url, init) => {
      if (init?.method === 'POST') earlierCollectPosts++;
      return earlierCollectFetch(url, init);
    };
    const earlierCollectFailuresBefore = state.failures.length;
    w.applySettings();
    check('collect-time duplicate input yields to the first invalid request-override card',
      state.failures.length === earlierCollectFailuresBefore && earlierCollectPosts === 0 &&
      d.activeElement === earlierCollectClient && d.activeElement !== laterCollectBadInput &&
      earlierCollectClient.getAttribute('aria-invalid') === 'true' &&
      roErrorDetail(earlierCollectClient)?.textContent.includes('no scope set'));
    w.fetch = earlierCollectFetch;
    laterCollectMap.innerHTML = laterCollectOriginal;
    earlierCollectClient.value = earlierCollectClientOriginal;
    earlierCollectClient.dispatchEvent(new w.Event('input', { bubbles: true }));
    w.roDraftChanged(roWrap);
    // the shared header-row grammar: Enter in the add inputs appends a row
    // through the providers editor's own wiring, and the live validation
    // owns the override grammar on top.
    const card0 = cards()[0];
    const multiErrorMap = card0.querySelector('.prov-hmap');
    multiErrorMap.insertAdjacentHTML('beforeend', w.headerRowHTML('Bad Header One', 'one'));
    multiErrorMap.insertAdjacentHTML('beforeend', w.headerRowHTML('Bad Header Two', 'two'));
    const multiErrorInputs = [...multiErrorMap.querySelectorAll('.sp-hname')].slice(-2);
    w.roDraftChanged(roWrap);
    const multiErrorDetails = multiErrorInputs.map(roErrorDetail);
    check('simultaneous header errors receive distinct detail nodes with matching messages',
      card0.querySelector('.ro-err').textContent.includes("'Bad Header One'") &&
      multiErrorDetails.every(detail => detail && detail.classList.contains('ro-error-detail')) &&
      new Set(multiErrorInputs.map(input => input.getAttribute('aria-describedby'))).size === 2 &&
      multiErrorInputs.every((input, index) => input.getAttribute('aria-describedby') === multiErrorDetails[index]?.id) &&
      multiErrorDetails[0]?.textContent.includes("'Bad Header One'") &&
      multiErrorDetails[1]?.textContent.includes("'Bad Header Two'"));
    multiErrorInputs.map(input => input.closest('.prov-hrow')).forEach(row => row.remove());
    w.roDraftChanged(roWrap);
    card0.querySelector('.prov-add-h .sp-hname').value = 'Authorization';
    card0.querySelector('.prov-add-h .sp-hval').value = 'Bearer should-not-fly';
    card0.querySelector('.prov-add-h .sp-hname').dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    const credRow = [...card0.querySelectorAll('.prov-hmap .prov-hrow')].pop();
    check('the shared header-row grammar adds rows inside an override card',
      !!credRow && credRow.querySelector('.sp-hname').value === 'Authorization');
    check('a credential-owned header name reddens the row with the ownership message',
      card0.dataset.roState === 'error' &&
      card0.querySelector('.ro-err').textContent.includes('credential-owned') &&
      credRow.querySelector('.sp-hname').classList.contains('prov-bad'));
    credRow.querySelector('[data-prov-hrow-rm]').click();
    check('removing the offending header row clears the card error',
      card0.dataset.roState !== 'error' && card0.querySelector('.ro-err').textContent === '');
    // canonical-case duplicate: HTTP names are case-insensitive, so
    // x-title collides with the seeded X-Title (a true conflict).
    card0.querySelector('.prov-add-h .sp-hname').value = 'x-title';
    card0.querySelector('.prov-add-h .sp-hval').value = 'second';
    card0.querySelector('.prov-add-h .sp-hval').dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    const dupRow = [...card0.querySelectorAll('.prov-hmap .prov-hrow')].pop();
    check('a canonical-case duplicate header name is rejected live',
      card0.dataset.roState === 'error' &&
      card0.querySelector('.ro-err').textContent.includes('duplicate HTTP header name'));
    const duplicateError = card0.querySelector('.ro-err');
    check('a failed request-override input exposes aria-invalid and its own error detail',
      duplicateError.id && dupRow.querySelector('.sp-hname').getAttribute('aria-invalid') === 'true' &&
      roErrorDetail(dupRow.querySelector('.sp-hname'))?.textContent.includes('duplicate HTTP header name'));
    // the blocked-Apply drive: an invalid request-overrides draft must stop
    // the POST, name the fix on the status line and focus the first
    // offender (the model-rules gate precedent).
    const failuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid request-overrides draft blocks the save and focuses the offending row',
      state.failures.length === failuresBefore &&
      d.getElementById('settings-count').textContent === 'request overrides - fix the highlighted rule first' &&
      d.activeElement === dupRow.querySelector('.sp-hname') &&
      dupRow.querySelector('.sp-hname').classList.contains('prov-bad'));
    dupRow.querySelector('[data-prov-hrow-rm]').click();
    check('repairing a request-override header clears its local accessibility error state',
      card0.dataset.roState !== 'error' && !card0.querySelector('.sp-hname[aria-invalid="true"]') &&
      !card0.querySelector('.sp-hname[aria-describedby]') && card0.querySelector('.ro-err').textContent === '');
    // header-value grammar: the live mirror of config.ValidHeaderValue
    // plus the overrides' non-empty rule. The pinned byte is \x01: a
    // single-line input's spec value-sanitization strips \r\n at the DOM
    // value layer before validation could see it, but other control bytes
    // survive a paste and must redden the row - and the blocked-Apply
    // drive focuses the offending VALUE input, not the name.
    const ctlVal = card0.querySelector('.prov-hmap .prov-hrow .sp-hval');
    ctlVal.value = 'bad\x01value';
    ctlVal.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a control byte in a header value reddens the row with the value-grammar message',
      card0.dataset.roState === 'error' &&
      card0.querySelector('.ro-err').textContent.includes('value must be a non-empty single-line header value') &&
      ctlVal.classList.contains('prov-bad'));
    const ctlFailuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a control-byte header value blocks the save and focuses the value input',
      state.failures.length === ctlFailuresBefore &&
      d.getElementById('settings-count').textContent === 'request overrides - fix the highlighted rule first' &&
      d.activeElement === ctlVal);
    ctlVal.value = 'my app';
    ctlVal.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('repairing the value clears the card error',
      card0.dataset.roState !== 'error' && card0.querySelector('.ro-err').textContent === '');
    // set + remove the same canonical name: one action per header.
    card0.querySelector('.ro-rh-in').value = 'x-title';
    card0.querySelector('[data-ro-rh-add]').click();
    const conflictChip = card0.querySelector('.prov-chip[data-rh="x-title"]');
    const conflictButton = conflictChip.querySelector('[data-prov-chip-rm]');
    const conflictError = card0.querySelector('.ro-err');
    check('a header-set/remove conflict names the conflict and targets the remove button',
      card0.dataset.roState === 'error' && conflictError.textContent.includes('both set in headers and removed') &&
      conflictButton.getAttribute('aria-invalid') === 'true' && roErrorDetail(conflictButton)?.textContent.includes('both set in headers and removed'));
    const conflictFailuresBefore = state.failures.length;
    captureInputFlash(() => d.getElementById('btn-settings-apply').click());
    check('Apply focuses the conflicting remove button',
      state.failures.length === conflictFailuresBefore && d.activeElement === conflictButton && conflictButton.classList.contains('prov-bad'));
    check('the conflicting remove button keeps its visual and ARIA error state',
      conflictButton.classList.contains('prov-bad') && conflictButton.getAttribute('aria-invalid') === 'true' &&
      roErrorDetail(conflictButton)?.textContent.includes('both set in headers and removed'));
    conflictButton.click();
    check('removing the conflicting chip clears the error and focuses the add input',
      card0.dataset.roState !== 'error' && !card0.querySelector('.prov-chip') && d.activeElement === card0.querySelector('.ro-rh-in'));
    // duplicate scope triple across rules: the exact server rejection.
    const card1 = cards()[1];
    card1.querySelector('.ro-client').value = 'claude-code';
    card1.querySelector('.ro-provider').value = '';
    card1.querySelector('.ro-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    const duplicateScopeError = card1.querySelector('.ro-err');
    check('a duplicate scope triple names the earlier rule and wires its first scope input',
      card1.dataset.roState === 'error' &&
      duplicateScopeError.textContent.includes('duplicate scope with rule 1') &&
      card1.querySelector('.ro-client').getAttribute('aria-invalid') === 'true' &&
      roErrorDetail(card1.querySelector('.ro-client'))?.textContent.includes('duplicate scope with rule 1'));
    // The middle-duplicate ordinal (the first-occurrence registration): a
    // duplicate scope whose card ALSO carries another error must not
    // re-register the scope, or a later duplicate cites the wrong rule.
    card1.querySelector('.ro-max-mct').value = '2000000';
    card1.querySelector('.ro-max-mct').dispatchEvent(new w.Event('input', { bubbles: true }));
    addRoTemplate('blank');
    const roDupMid = cards()[2];
    roDupMid.querySelector('.ro-client').value = 'claude-code';
    roDupMid.querySelector('.ro-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    roDupMid.querySelector('.ro-rh-in').value = 'X-Extra';
    roDupMid.querySelector('[data-ro-rh-add]').click();
    check('a duplicate scope carrying another error does not shift the cited ordinal',
      cards().length === 3 &&
      roDupMid.querySelector('.ro-err').textContent === 'duplicate scope with rule 1 - the same client, provider and model; merge the rules or change one scope');
    roDupMid.querySelector('[data-ro-rm]').click();
    card1.querySelector('.ro-max-mct').value = '';
    card1.querySelector('.ro-max-mct').dispatchEvent(new w.Event('input', { bubbles: true }));
    card1.querySelector('.ro-client').value = '';
    card1.querySelector('.ro-provider').value = 'epsilon.example';
    card1.querySelector('.ro-provider').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('repairing a duplicate scope clears its error wiring',
      card1.dataset.roState !== 'error' &&
      !card1.querySelector('.ro-client[aria-invalid="true"]') &&
      !card1.querySelector('.ro-client[aria-describedby]'));
    // body band: outside 1..1000000.
    card1.querySelector('.ro-max-mct').value = '2000000';
    card1.querySelector('.ro-max-mct').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('an out-of-band body ceiling names the 1..1000000 band',
      card1.dataset.roState === 'error' &&
      card1.querySelector('.ro-err').textContent.includes('between 1 and 1000000'));
    card1.querySelector('.ro-max-mct').value = '';
    card1.querySelector('.ro-max-mct').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the repaired draft is valid end to end',
      roWrap.querySelectorAll('.ro-rule[data-ro-state="error"]').length === 0 && !w.settingsIsDirty());
    // the save path: a real edit, then the POST carries the strict shape
    // with the loaded revision (the settings save-flow stub discipline).
    card1.querySelector('.prov-add-h .sp-hname').value = 'X-Workspace';
    card1.querySelector('.prov-add-h .sp-hval').value = 'acme';
    card1.querySelector('.prov-add-h .sp-hval').dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    const originalRoFetch = w.fetch;
    const roPosts = [];
    w.fetch = (url, init) => {
      if (!String(url).includes('/admin/config')) return originalRoFetch(url, init);
      return new Promise(resolve => {
        if (init?.method === 'POST') roPosts.push({ resolve, body: JSON.parse(init.body) });
        else resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
      });
    };
    const roSave = w.applySettings();
    check('the request-overrides save posts the strict lossless shape with the loaded revision',
      roPosts.length === 1 && roPosts[0].body.revision === 'ro-r1' &&
      JSON.stringify(roPosts[0].body.values.request_overrides) === JSON.stringify([
        { client: 'claude-code', provider: '', model: '', headers: { 'X-Title': 'my app' } },
        { client: '', provider: 'epsilon.example', model: '', headers: { 'X-Workspace': 'acme' }, remove_headers: ['User-Agent'], body: { max_tokens: 32768 } },
      ]));
    const savedRo = JSON.parse(JSON.stringify(roPosts[0].body.values.request_overrides));
    roPosts.shift().resolve({ ok: true, statusText: 'OK', json: async () => ({ saved: true, revision: 'ro-r2', values: { ...doc.values, request_overrides: savedRo }, effective: {}, restart_required: [] }) });
    await roSave;
    check('the accepted save adopts the new revision and a clean draft',
      w.eval('settingsDoc.revision') === 'ro-r2' && !w.settingsIsDirty() &&
      /saved/.test(d.getElementById('settings-count').textContent));
    const savedRoWrap = d.querySelector('#settings-fields [data-kind="request_overrides"]');
    const savedRoCards = [...savedRoWrap.querySelectorAll('.ro-rule')];
    const savedRoAdd = savedRoWrap.querySelector('[data-ro-add]');
    savedRoCards[0].querySelector('[data-ro-rm]').click();
    check('removing a request-override card focuses the next surviving card',
      savedRoWrap.querySelectorAll('.ro-rule').length === 1 && d.activeElement === savedRoWrap.querySelector('.ro-rule'));
    savedRoWrap.querySelector('.ro-rule [data-ro-rm]').click();
    check('removing the final request-override card falls back to Add rule focus',
      savedRoWrap.querySelectorAll('.ro-rule').length === 0 && d.activeElement === savedRoAdd);
    const invalidRoDoc = JSON.parse(JSON.stringify(doc));
    invalidRoDoc.values.request_overrides = [...doc.values.request_overrides, {}];
    w.__roInvalidDoc = invalidRoDoc;
    w.eval('settingsDoc = window.__roInvalidDoc; fillSettingsForm(settingsDoc)');
    const invalidRoWrap = d.querySelector('#settings-fields [data-kind="request_overrides"]');
    const invalidLoadedCard = invalidRoWrap.querySelector('.ro-rule:last-child');
    const loadedBodyIds = [...invalidRoWrap.querySelectorAll('.ro-rule-body')].map(body => body.id);
    check('an invalid request-override loaded from the server opens with unique disclosure and error wiring',
      invalidLoadedCard.querySelector('[data-ro-toggle]').getAttribute('aria-expanded') === 'true' &&
      invalidLoadedCard.querySelector('.ro-rule-body').hidden === false &&
      invalidLoadedCard.querySelector('.ro-err').textContent.includes('no scope set') &&
      roErrorDetail(invalidLoadedCard.querySelector('.ro-client'))?.textContent.includes('no scope set') &&
      new Set(loadedBodyIds).size === loadedBodyIds.length);
    w.eval('settingsDoc = window.__roDoc; fillSettingsForm(settingsDoc)');
    delete w.__roInvalidDoc;
    w.fetch = originalRoFetch;
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__roDoc;
  }
};
