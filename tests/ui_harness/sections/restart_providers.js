// test 12: restart menu + rebuild-and-restart poll
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function restartProviders(H) {
  const {DEF_RULES, cfgDoc, check, d, fire, mkRec, rows, settleUntil, sleep, w} = H;
  const state = H.state;

  // ---- test 12: restart menu + rebuild-and-restart poll ----
  const rmenu = d.getElementById('restart-menu');
  check('restart menu starts hidden', rmenu && rmenu.hidden);
  w.toggleRestartMenu({ stopPropagation() {} });
  check('restart menu opens', rmenu && !rmenu.hidden);
  check('restart open sets aria-expanded', d.getElementById('btn-restart').getAttribute('aria-expanded') === 'true');
  check('restart action wears the accent primary', d.getElementById('btn-restart-now').classList.contains('btn-accent'));
  const restartAction = d.getElementById('btn-restart-now');
  const restartDescription = d.getElementById('restart-description');
  check('restart description explains streams, saved metrics, and queued requests',
    restartDescription && /in-flight streams/.test(restartDescription.textContent) &&
    /metrics/.test(restartDescription.textContent) && /queue/.test(restartDescription.textContent) &&
    restartAction.getAttribute('aria-describedby') === restartDescription.id);
  await sleep(0); // settle the menu's initial GET before holding the preflight
  let releaseRestartStatus, releaseRestartPost;
  state.restartState.statusGate = new Promise(resolve => { releaseRestartStatus = resolve; });
  state.restartState.postGate = new Promise(resolve => { releaseRestartPost = resolve; });
  const restartPending = w.restartProxy();
  check('restart action disables immediately while its status check is pending', restartAction.disabled);
  await w.restartProxy(); // a rapid second call must not issue another POST
  releaseRestartStatus();
  state.restartState.statusGate = null;
  await sleep(20);
  check('only one restart POST is issued and the pending request stays disabled',
    state.restartState.postCount === 1 && restartAction.disabled && restartAction.textContent.includes('Restarting'));
  await w.fetchRestartStatus();
  check('an idle status refresh cannot re-enable a locally pending restart', restartAction.disabled);
  check('the header indicates busy while keeping progress accessible',
    d.getElementById('btn-restart').hasAttribute('data-busy') && !d.getElementById('btn-restart').disabled);
  releaseRestartPost();
  state.restartState.postGate = null;
  await restartPending;
  check('restart POST hit the endpoint', state.restartState.restarted);
  check('restart poll detects the fresh process via started_at', d.getElementById('restart-count').textContent.includes('restarted'));
  check('completed restart restores the action and clears the busy header',
    !restartAction.disabled && restartAction.textContent === 'Restart now' && !d.getElementById('btn-restart').hasAttribute('data-busy'));
  {
    // The step tracker: the POST's status seeds it; completion marks every
    // step done. Drain renders its live window; a failure turns the step red.
    const steps = [...d.querySelectorAll('#restart-steps .rs-step')];
    check('restart step list rendered the full choreography, all done', steps.length === 4 && steps.every(s => s.classList.contains('done')));
    w.renderRestartSteps({ rank: 1, phase: 'draining', error: '', drain_timeout_ms: 600000, drain_elapsed_ms: 5000 });
    check('drain step shows the live drain window',
      [...d.querySelectorAll('#restart-steps .rs-step')][1].classList.contains('active') &&
      [...d.querySelectorAll('#restart-steps .rs-step')][1].querySelector('.rs-sub').textContent.includes('/'));
    // The deadline text itself derives from the status fields: elapsed
    // seconds over the configured drain_timeout_ms, rounded to whole
    // seconds. A zero timeout means the server waits indefinitely and must
    // render no deadline at all.
    check('the drain deadline renders the exact elapsed/timeout text',
      [...d.querySelectorAll('#restart-steps .rs-step')][1].querySelector('.rs-sub').textContent === '5s / 10m');
    w.renderRestartSteps({ rank: 1, phase: 'draining', error: '', drain_timeout_ms: 600000, drain_elapsed_ms: 30500 });
    check('the drain deadline rounds elapsed milliseconds up to whole seconds',
      [...d.querySelectorAll('#restart-steps .rs-step')][1].querySelector('.rs-sub').textContent === '31s / 10m');
    w.renderRestartSteps({ rank: 1, phase: 'draining', error: '', drain_timeout_ms: 0 });
    check('an unbounded drain renders no deadline text',
      ![...d.querySelectorAll('#restart-steps .rs-step')].some(s => s.querySelector('.rs-sub')));
    w.renderRestartSteps({ rank: 0, phase: 'idle', error: 'build failed: boom' });
    check('a failed run marks its step red', !!d.querySelector('#restart-steps .rs-step.failed'));
    w.renderRestartSteps(null);
    check('the step list hides when idle', d.getElementById('restart-steps').hidden);
  }
  for (const [rank, phase] of ['building', 'draining', 'flushing', 'handoff'].entries()) {
    state.restartState.status = { phase, rank };
    await w.fetchRestartStatus();
    check(`server-reported ${phase} disables the restart action`, restartAction.disabled);
  }
  state.restartState.status = { phase: 'idle', error: 'build failed', rank: 0 };
  await w.fetchRestartStatus();
  check('failed restart restores the action and reports the error',
    !restartAction.disabled && d.getElementById('restart-count').textContent.includes('build failed'));
  state.restartState.status = { available: false, reason: 'source unavailable' };
  await w.fetchRestartStatus();
  check('unavailable restart stays disabled', restartAction.disabled);
  // A binary that can never restart is not OFFERED at all: the control and its
  // menu leave the header row (an empty wrap would hold the row's gap open),
  // and the server's own diagnosis is the status line an attempt would read.
  const restartWrap = d.getElementById('restart-wrap');
  check('an ineligible restart is not offered in the header, and its reason is what the menu would say',
    restartWrap && restartWrap.hidden &&
    d.getElementById('restart-count').textContent === 'source unavailable' &&
    d.getElementById('restart-count').dataset.err === '1');
  // A run in progress keeps the control reachable, so its progress can be
  // reopened; the verdict only withholds the OFFER.
  w.applyRestartEvent({ phase: 'draining', rank: 1, available: false, reason: 'source unavailable' });
  check('a restart in progress keeps the control reachable for its progress',
    !restartWrap.hidden && d.getElementById('btn-restart').hasAttribute('data-busy') && restartAction.disabled);
  w.applyRestartEvent({ phase: 'idle', rank: -1, available: false, reason: 'source unavailable', error: '' });
  check('an idle ineligible restart withholds the control again', restartWrap.hidden);
  // The boot payload is the only pre-menu signal. An absent or unusable
  // section changes nothing: the control stays exactly as the last verdict
  // left it, so a build that never reports eligibility is never worse off
  // than today.
  w.applyRestartState({ available: true, reason: '' });
  w.applyRestartState(null);
  w.applyRestartState({ reason: 'no verdict here' });
  check('a bootstrap without a verdict keeps the control', !restartWrap.hidden);
  w.applyRestartState({ available: false, reason: 'built with -trimpath, so the binary cannot locate its own source; self-restart is unavailable (restart via your process manager)' });
  check('the bootstrap fold withholds the control for an ineligible binary', restartWrap.hidden);
  w.applyRestartState({ available: true, reason: '' });
  check('the bootstrap fold offers the control for an eligible binary', !restartWrap.hidden);
  state.restartState.status = {};
  await w.fetchRestartStatus();
  check('an available status offers the control again', !restartWrap.hidden);
  // The refusal path through operatorErrorBody: a non-2xx whose body is not
  // JSON-with-an-error (a plain-text operator denial, an HTML 502) must
  // reject with the restart site's designed fallback text, marked as a
  // server answer so the generic transport prefix never wraps it. Nothing
  // reddened before this pin (the W20 mutation round proved the fallback
  // could be dropped silently).
  state.restartState.postFail = { status: 502 };
  const refusalPostCount = state.restartState.postCount;
  await w.restartProxy();
  const restartStatusLine = d.getElementById('restart-count');
  check('a non-JSON 502 restart refusal shows the designed fallback, never an empty error',
    state.restartState.postCount === refusalPostCount + 1 &&
    restartStatusLine.textContent === 'restart failed (502)' &&
    restartStatusLine.dataset.err === '1');
  state.restartState.postFail = null;
  w.toggleRestartMenu({ stopPropagation() {} });
  check('restart menu closes on second toggle', rmenu.hidden);

  // ---- test 13: restart sweep + weekday clock ----
  // A feed_id change is the page's "new process" signal: the snapshot resets
  // the log AND refreshAfterRestart re-pulls operator state (pause, throttle,
  // effective config) + aggregates in place when frontend assets match.
  const cfgBefore = state.cfgFetches;
  fire('snapshot', { feed_id: 'feedC', seq: 1, incremental: false,
    records: [{ ...mkRec('restarted1', 200, 1700000200000) }],
    counters: { in_flight: 0, total_requests: 1, total_errors: 0 } });
  // Await the reset's own painted end state and the sweep's config re-pull
  // instead of a fixed sleep: a loaded runner can stretch either past any
  // hard time. Both waits return false on timeout, so the checks below still
  // fail with their labels and the diagnostic dump.
  await settleUntil(() => rows().length === 1 && rows()[0].dataset.id === 'restarted1');
  await settleUntil(() => state.cfgFetches > cfgBefore);
  check('feed change resets the log to the fresh process snapshot', rows().length === 1 && rows()[0].dataset.id === 'restarted1');
  check('feed change sweeps operator state (config re-fetched in place)', state.cfgFetches > cfgBefore);
  check('footer clock carries the weekday', (() => {
    const text = d.getElementById('f-clock').textContent.toLowerCase();
    const weekdays = Array.from({length: 7}, (_, i) =>
      new Date(2024, 0, 7 + i).toLocaleDateString(undefined, {weekday: 'long'}).toLowerCase());
    return weekdays.some(day => text.includes(day));
  })());

  // ---- test 14: providers field-map editor ----
  // Cards with removable cost-key chips and canonical-dropdown usage rows;
  // edits drive the dirty tracking and collect() round-trips the map.
  w.toggleSettings();
  await sleep(20);
  w.eval('settingsCat = "providers"; showSettingsCat()');
  const provRow = d.querySelector('.st-row[data-key="providers"]');
  const card = provRow && provRow.querySelector('.st-prov');
  check('providers field renders the card editor', !!card);
  check('provider card shows label + cost chip + usage row', card.querySelector('.sp-label').value === 'epsilon.example' &&
    card.querySelector('.prov-chip').dataset.cost === 'x_billing_pricing.cost' &&
    card.querySelector('.prov-urow .sp-ufield').value === 'input_tokens');
  check('canonical dropdown is the server-owned field list', card.querySelectorAll('.prov-urow .sp-ufield option').length === 6);
  check('collect round-trips the provider map', JSON.stringify(w.collectSettingsValues().providers) === JSON.stringify(cfgDoc.values.providers));
  card.querySelector('.prov-chip [data-prov-chip-rm]').click();
  await sleep(10);
  check('chip removal reflects in collect + enables apply', w.collectSettingsValues().providers['epsilon.example'].cost_keys.length === 0 && !d.getElementById('btn-settings-apply').disabled);
  const cin = card.querySelector('.sp-cost-in');
  cin.value = 'not a path';
  card.querySelector('[data-prov-add-cost]').click();
  check('invalid cost path is refused', card.querySelectorAll('.prov-chip').length === 0);
  cin.value = 'x_billing_pricing.costUsd';
  card.querySelector('[data-prov-add-cost]').click();
  check('valid cost path adds a chip', [...card.querySelectorAll('.prov-chip')].some(c => c.dataset.cost === 'x_billing_pricing.costUsd'));
  card.querySelector('.sp-ufield-new').value = 'output_tokens';
  card.querySelector('[data-prov-add-usage]').click();
  check('usage mapping added via the canonical dropdown', [...card.querySelectorAll('.prov-urow .sp-ufield')].some(s => s.value === 'output_tokens'));
  // Add provider: a "+ add provider" button in the title row (top right),
  // opening a dropdown that only offers seen providers NOT already mapped.
  const prow = card.closest('.st-row');
  const topBtn = prow.querySelector('.st-name [data-prov-menu-toggle]');
  const menu = prow.querySelector('.prov-menu');
  check('add button sits in the title row, lowercase', !!topBtn && topBtn.textContent === '+ add provider' && topBtn.closest('.st-name') === prow.querySelector('.st-name'));
  check('remove is a recycle-bin icon', !!card.querySelector('[data-prov-rm] svg'));
  // The provider card's trash icon is the PROV_TRASH_SVG markup constant
  // (the header Clear button that used to own this geometry is gone): the
  // rendered card icon must match the constant's paths exactly and carry
  // no class (the .prov-x svg owns this surface's stroke and size).
  {
    const constPaths = w.eval(`(() => {
      const host = document.createElement('div');
      host.innerHTML = PROV_TRASH_SVG;
      return [...host.querySelectorAll('path')].map(p => p.getAttribute('d'));
    })()`);
    const icon = card.querySelector('[data-prov-rm] svg');
    check('the card trash icon renders the constant glyph, class-free',
      JSON.stringify([...icon.querySelectorAll('path')].map(p => p.getAttribute('d'))) === JSON.stringify(constPaths) &&
      icon.getAttribute('class') === null);
  }
  // The 16x16 down-chevron is one glyph authored twice as markup: the
  // explorer dimension picker's caret (index-adjacent, .hdr-ico family) and
  // the provider card's collapse chevron (PROV_CHEV_SVG). The paths were
  // born drifted by 0.3 units and aligned to the header-family variant;
  // byte-equality of the rendered d attributes keeps them from drifting
  // apart again (either side may not re-derive its geometry alone).
  {
    const caret = d.querySelector('#xp-dim-trigger .xp-dim-caret path');
    const chevs = [...card.querySelectorAll('.prov-chev svg path')].map(p => p.getAttribute('d'));
    check('the provider chevron and the explorer caret share one glyph geometry',
      !!caret && chevs.length === 1 &&
        JSON.stringify(chevs) === JSON.stringify([caret.getAttribute('d')]));
  }
  topBtn.click();
  check('add menu opens; already-added providers are not offered', !menu.hidden && !!menu.querySelector('[data-prov-pick="p"]') && !menu.querySelector('[data-prov-pick="epsilon.example"]'));
  // setProvMenuOpen is the one open-state writer: the toggle's aria-expanded
  // moves with the menu on every open and close path.
  check('opening the provider picker couples aria-expanded on its toggle',
    topBtn.getAttribute('aria-expanded') === 'true');
  // Both provider-icon emitters carry the entity color through --ent, so
  // the icon and its glow ride the entity palette instead of a hardcoded
  // hex in the stylesheet.
  check('the card icon keys its color to the provider entity token',
    card.querySelector('.prov-hd .prov-ic').getAttribute('style') === '--ent:#0072B2');
  check('the picker icon keys its color to the provider entity token',
    menu.querySelector('.prov-menu-item .prov-ic').getAttribute('style') === '--ent:#0072B2');
  menu.querySelector('.prov-new-label').value = 'gamma.example';
  menu.querySelector('[data-prov-add]').click();
  check('new provider card collects with empty maps', JSON.stringify(w.collectSettingsValues().providers['gamma.example']) === JSON.stringify({ cost_keys: [], usage_keys: {}, models_path: '', models_keys: {}, ensure_tools: [], headers: {} }));
  // The ensured-tools input round-trips through collect: comma-separated
  // names split and trim, empty segments drop, and the saved provider keeps
  // the section, so editing any other section never drops the signature.
  const gamma = prow.querySelector('.st-prov:last-child');
  check('ensured tools section renders on new cards', !!gamma.querySelector('.sp-etools'));
  gamma.querySelector('.sp-etools').value = ' bash ,, read ';
  check('ensured tools collect splits, trims and drops empty segments',
    JSON.stringify(w.collectSettingsValues().providers['gamma.example'].ensure_tools) === JSON.stringify(['bash', 'read']));
  gamma.querySelector('.sp-etools').value = 'bash, bash';
  check('ensured tools pass duplicates to the shared server validator',
    JSON.stringify(w.collectSettingsValues().providers['gamma.example'].ensure_tools) === JSON.stringify(['bash', 'bash']));
  check('add menu closes after adding', menu.hidden);
  check('closing the provider picker couples aria-expanded on its toggle',
    topBtn.getAttribute('aria-expanded') === 'false');
  // The same writer serves the outside-dismiss path: reopening and closing
  // through closeProvMenus keeps the pair together.
  topBtn.click();
  w.closeProvMenus(prow);
  check('closeProvMenus closes the picker and its aria-expanded together',
    menu.hidden && topBtn.getAttribute('aria-expanded') === 'false');
  const cards = [...prow.querySelectorAll('.st-prov')];
  check('each provider renders as its own section', cards.length === 2 && cards.every(c => c.querySelector('.prov-hd .prov-ic') && c.querySelector('.prov-body .prov-sec')));
  const margins = cards.map(c => w.getComputedStyle(c).marginBottom);
  check('provider cards keep their section separation', margins.every(m => m === margins[0] && m !== '0px'));
  // Regression: an unbalanced provider card template nested every following
  // card (and the aliases row) inside the previous card. Cards - and the
  // aliases row after them - must be SIBLINGS, never descendants.
  check('provider cards are siblings, not nested', !cards[0].contains(cards[1]));
  // Regression: an unbalanced provider-card template let its open divs
  // swallow the row's closing tags, cascading every FOLLOWING settings row
  // (aliases, …) inside the providers row - mis-styled and mis-nested. The
  // invariant is structural: every settings row is a DIRECT child of the
  // fields box, no matter what any card template does.
  {
    const box = d.getElementById('settings-fields');
    const orphaned = [...box.querySelectorAll('.st-row')].filter(r2 => r2.parentElement !== box);
    check('every settings row is a direct child of the fields box', orphaned.length === 0);
  }
  {
    const arow = d.querySelector('.st-row[data-key="provider_aliases"]');
    check('aliases editor is not swallowed by a provider card', !!arow && !cards.some(c => c.contains(arow.querySelector('[data-kind="aliases"]'))) && arow.querySelectorAll('.st-prov').length === 0);
  }

  // Models enrichment section: metadata path + canonical→path rows, same
  // row grammar as the usage map; edits round-trip through collect. Its slot
  // is FIXED - last section in every card, after cost keys and usage keys -
  // so it never wanders between the other sections' rows.
  check('models section renders path input + mapped row', card.querySelector('.sp-mpath').value === '/model-meta' &&
    card.querySelector('.prov-mmap .sp-mfield').value === 'input_modalities' &&
    card.querySelector('.prov-mmap .sp-mkey').value === 'input_modalities');
  // Upstream headers ride the same card grammar: name/value rows, collected
  // like the other maps. Section order is FIXED - cost keys, usage keys,
  // models enrichment, ensured tools, upstream headers last.
  check('upstream headers is the last section in every card', [...provRow.querySelectorAll('.st-prov')].every(c2 => {
    const secs = [...c2.querySelectorAll('.prov-body > .prov-sec')];
    return secs.length === 5 &&
      secs[0].querySelector('.prov-lb span').textContent === 'cost keys' &&
      secs[1].querySelector('.prov-lb span').textContent === 'usage keys' &&
      secs[2].querySelector('.prov-lb span').textContent === 'models enrichment' &&
      !!secs[2].querySelector('.sp-mpath') &&
      secs[3].querySelector('.prov-lb span').textContent === 'ensured tools' &&
      !!secs[3].querySelector('.sp-etools') &&
      secs[4].querySelector('.prov-lb span').textContent === 'upstream headers';
  }));
  check('model field dropdown is the server-owned canonical list', card.querySelectorAll('.prov-mmap .sp-mfield option').length === 5);
  card.querySelector('.sp-mfield-new').value = 'max_output_tokens';
  card.querySelector('[data-prov-add-model]').click();
  const mrow = [...card.querySelectorAll('.prov-mmap .prov-urow')].find(r2 => r2.querySelector('.sp-mfield').value === 'max_output_tokens');
  mrow.querySelector('.sp-mkey').value = 'limits.max_output';
  check('models mapping added and collected',
    w.collectSettingsValues().providers['epsilon.example'].models_keys['max_output_tokens'] === 'limits.max_output');
  mrow.querySelector('[data-prov-mrow-rm]').click();
  check('models mapping row removal reflects in collect',
    !('max_output_tokens' in w.collectSettingsValues().providers['epsilon.example'].models_keys));
  card.querySelector('.sp-mpath').value = 'no-slash';
  card.querySelector('.sp-mpath').dispatchEvent(new w.Event('change', { bubbles: true }));
  check('relative metadata path is flagged on change', card.querySelector('.sp-mpath').classList.contains('prov-bad'));
  card.querySelector('.sp-mpath').value = '/model-meta';
  card.querySelector('.sp-mpath').dispatchEvent(new w.Event('change', { bubbles: true }));
  // Collapse/expand: the header band toggles the body without dirtying the form.
  const first = cards[0];
  const fp1 = w.settingsFingerprint();
  first.querySelector('.prov-hd').click();
  check('header click collapses the provider section', first.classList.contains('collapsed') && w.getComputedStyle(first.querySelector('.prov-body')).display === 'none');
  check('collapsing does not dirty the form', w.settingsFingerprint() === fp1);
  first.querySelector('[data-prov-collapse]').click();
  check('chevron click re-expands', !first.classList.contains('collapsed'));
  w.closeSettings(true);

  // ---- test 14b: provider aliases editor - rows of [old label] → [canonical],
  // with add/remove driving the same dirty tracking and collect() round-trip.
  w.toggleSettings();
  await sleep(20);
  w.eval('settingsCat = "providers"; showSettingsCat()');
  const aliasRow = d.querySelector('.st-row[data-key="provider_aliases"]');
  const aliasWrap = aliasRow && aliasRow.querySelector('[data-kind="aliases"]');
  check('aliases field renders the row editor', !!aliasWrap && aliasWrap.querySelectorAll('.al-row').length === 1);
  // The editor rides .prov-sec so its inputs share the provider-editor
  // visual system; bare inputs would fall back to browser defaults.
  check('aliases editor rides the provider input styling', !!aliasWrap.querySelector('.prov-sec .al-rows') && !!aliasWrap.querySelector('.prov-sec .prov-add .al-new-from'));
  const a0 = aliasWrap.querySelector('.al-row');
  check('alias row carries the configured mapping', a0.querySelector('.al-from').value === 'old.example' && a0.querySelector('.al-to').value === 'new.example');
  check('collect round-trips the alias map', JSON.stringify(w.collectSettingsValues().provider_aliases) === JSON.stringify({ 'old.example': 'new.example' }));
  a0.querySelector('[data-al-rm]').click();
  check('alias row removal clears the map + arms apply',
    Object.keys(w.collectSettingsValues().provider_aliases).length === 0 && !d.getElementById('btn-settings-apply').disabled);
  const af = aliasWrap.querySelector('.al-new-from'), at2 = aliasWrap.querySelector('.al-new-to');
  af.value = 'old.example'; at2.value = 'old.example';
  aliasWrap.querySelector('[data-al-add]').click();
  check('self-mapping is refused', aliasWrap.querySelectorAll('.al-row').length === 0);
  af.value = 'old.example'; at2.value = '';
  aliasWrap.querySelector('[data-al-add]').click();
  check('empty target is refused', aliasWrap.querySelectorAll('.al-row').length === 0);
  af.value = 'old.example'; at2.value = 'new.example';
  aliasWrap.querySelector('[data-al-add]').click();
  af.value = 'old.example'; at2.value = 'other.example';
  aliasWrap.querySelector('[data-al-add]').click();
  check('duplicate old label is refused', aliasWrap.querySelectorAll('.al-row').length === 1);
  check('valid alias row collects', w.collectSettingsValues().provider_aliases['old.example'] === 'new.example');
  w.closeSettings(true);

  {
    const doc = {
      revision: 'ux-r1',
      fields: [
        { key: 'safe_text', category: 'ux', label: 'Plain label', help: 'Search <em>needle</em> & "quoted"', kind: 'string', hot_reload: true },
        { key: 'safe_list', category: 'ux', label: 'List label', help: 'List help', kind: 'strings', hot_reload: true },
        { key: 'safe_toggle', category: 'ux', label: 'Toggle label', help: '', kind: 'bool', hot_reload: true },
        { key: 'safe_number', category: 'ux', label: 'Number label', help: 'Number help', kind: 'int', min: 0, max: 10, unit: 'items', hot_reload: true },
        { key: 'providers', category: 'ux', label: 'Provider maps', help: 'Structured help', kind: 'providers', hot_reload: true },
      ],
      categories: [{ id: 'ux', label: 'UX', help: 'ordinary fields' }],
      values: { safe_text: 'ordinary value', safe_list: ['one'], safe_toggle: true, safe_number: 3, providers: {} },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.__settingsUxDoc = doc;
    w.eval('settingsDoc = window.__settingsUxDoc; fillSettingsForm(settingsDoc)');
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const textRow = box.querySelector('.st-row[data-key="safe_text"]');
    const textInput = textRow.querySelector('[data-st-scalar]');
    const help = textRow.querySelector('details.st-help');
    const helpText = help.querySelector('.st-help-text');
    const scalarRows = [...box.querySelectorAll('.st-row')].filter(row => row.querySelector('[data-st-scalar]'));
    const scalarIds = scalarRows.map(row => row.querySelector('[data-st-scalar]').id);
    const search = d.getElementById('settings-q');
    search.value = 'needle';
    w.filterSettings();
    const visibleKeys = [...box.querySelectorAll('.st-row')].filter(row => !row.hidden).map(row => row.dataset.key);
    check('ordinary settings help is an escaped collapsed disclosure and remains searchable',
      help && !help.open && help.querySelector('summary').textContent === 'Help' &&
      helpText.textContent === 'Search <em>needle</em> & "quoted"' && !helpText.querySelector('em') &&
      textRow.dataset.help === 'Search <em>needle</em> & "quoted"' &&
      textRow.title === 'Search <em>needle</em> & "quoted" · safe_text' &&
      JSON.stringify(visibleKeys) === JSON.stringify(['safe_text']));
    check('ordinary settings controls have unique visible label associations',
      scalarRows.length === 4 && new Set(scalarIds).size === scalarIds.length &&
      scalarRows.every(row => {
        const input = row.querySelector('[data-st-scalar]');
        const label = row.querySelector('label.st-name');
        return label && label.htmlFor === input.id && d.getElementById(input.id) === input;
      }));
    const structuredRow = box.querySelector('.st-row[data-key="providers"]');
    check('specialized settings fields remain non-label containers',
      !!structuredRow.querySelector('[data-kind="providers"]') && !structuredRow.querySelector('label.st-name') &&
      !structuredRow.querySelector('[data-st-scalar]'));
    check('scalar controls associate their visible help text',
      textInput.getAttribute('aria-describedby').split(/\s+/).includes(helpText.id));
    const number = box.querySelector('input[data-key="safe_number"]');
    const numberError = number.closest('.st-row').querySelector('.st-error');
    check('valid ordinary settings start without false local errors',
      scalarRows.every(row => {
        const input = row.querySelector('[data-st-scalar]');
        const error = row.querySelector('.st-error');
        return !input.classList.contains('prov-bad') && !input.hasAttribute('aria-invalid') && error.textContent === '';
      }) && number.required && number.min === '0' && number.max === '10' && number.step === '1');
    number.value = '11';
    number.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('invalid numeric input is visibly marked with adjacent live feedback',
      number.classList.contains('prov-bad') && number.getAttribute('aria-invalid') === 'true' &&
      numberError.textContent.length > 0 && number.getAttribute('aria-describedby').split(/\s+/).includes(numberError.id));
    number.value = '1.5';
    number.dispatchEvent(new w.Event('change', { bubbles: true }));
    check('native step validity is reflected in the same local feedback',
      number.classList.contains('prov-bad') && numberError.textContent.includes('steps'));
    number.value = '3';
    number.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('repairing numeric input clears its local error and description',
      !number.classList.contains('prov-bad') && !number.hasAttribute('aria-invalid') && numberError.textContent === '' &&
      !number.getAttribute('aria-describedby').split(/\s+/).includes(numberError.id));
    search.value = '';
    w.filterSettings();
    const restore = JSON.parse(JSON.stringify(cfgDoc));
    w.__settingsUxRestore = restore;
    w.eval('settingsDoc = window.__settingsUxRestore; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsUxDoc;
    delete w.__settingsUxRestore;
  }

  // (G3) A model_rules list is an array of objects, so String(val) compares as
  // "[object Object],..." and can fabricate a "default" hint or print that
  // string. The field must not use the scalar default-hint comparison.
  {
    const base = {
      revision: 'mr-hint-r1',
      fields: [{ key: 'model_rules', category: 'ux', label: 'Model rules', help: '', kind: 'model_rules', hot_reload: true }],
      categories: [{ id: 'ux', label: 'UX', help: '' }],
      defaults: { model_rules: DEF_RULES },
      effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    const render = values => {
      const doc = JSON.parse(JSON.stringify(base));
      doc.values = { model_rules: values };
      w.__mrHintDoc = doc;
      w.eval('settingsDoc = window.__mrHintDoc; fillSettingsForm(settingsDoc)');
      return d.querySelector('#settings-fields .st-row[data-key="model_rules"] .st-hint');
    };
    d.getElementById('settings-sheet').hidden = false;
    const untouched = render(JSON.parse(JSON.stringify(DEF_RULES)));
    check('an untouched model rule list renders the default hint', !!untouched && untouched.textContent === 'default');
    const modified = render([
      { mode: 'lower' },
      { mode: 'pattern', from: 'a', to: 'b' },
      { mode: 'pattern', from: 'c', to: 'd' },
      { mode: 'pattern', from: 'e', to: 'f' },
      { mode: 'pattern', from: 'g', to: 'h' },
    ]);
    check('a modified same-length model rule list shows no default hint', !modified);
    const shorter = render([{ mode: 'lower' }]);
    check('a shorter model rule list shows no object string hint', !shorter);
    delete w.__mrHintDoc;
    w.closeSettings(true);
  }
};
