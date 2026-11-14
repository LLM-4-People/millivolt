// The operator console's export section is the one download surface now that
// the five header menus are gone. The settings sheet's delete-matching block
// stays the destructive twin, so both must read the same record dimensions
// from the same server predicate.
module.exports = async function logsMenu(H) {
  const {check, d, mkRec, settleUntil, w} = H;

  const savedRecords = w.eval('lastData && lastData.records');
  const savedFetch = w.fetch;
  const savedDownload = w.triggerDownload;
  let host = null;
  try {
    const shell = d.getElementById('operator-console');
    check('the operator console starts hidden', !!shell && shell.hidden);
    w.openOperatorConsole('export');
    check('the console opens on its export section',
      !shell.hidden && shell.classList.contains('mn') && shell.classList.contains('oc') &&
      d.getElementById('oc-pane').dataset.section === 'export');
    // The rebuild's focus preservation reads one definition of "reachable by
    // keyboard inside the pane". Three literals lived on four adjacent lines,
    // so a control type added to one would have been focusable for the control
    // list but invisible to the slot lookup that restores the caret.
    {
      const host = d.createElement('div');
      host.innerHTML = '<button id="f-b">b</button><label id="f-l"><input id="f-i"></label>' +
        '<span id="f-s" tabindex="0">s</span><button id="f-d" disabled>d</button>' +
        '<a id="f-a" href="#x">a</a>';
      const count = w.ocControls(host).map(el => el.id).join(',');
      check('one selector defines the pane focus order and the slot lookup',
        w.eval('typeof OC_FOCUSABLE') === 'string' &&
        // The pane's contract is the console's own controls. A link is not one:
        // the console navigates by button, and the modal frame keeps the wider
        // selector that also reaches links.
        count === 'f-b,f-i,f-s' &&
        w.eval('OC_FOCUSABLE').includes('[tabindex]') &&
        !w.eval('OC_FOCUSABLE').includes('a[href]') &&
        // A slot that is itself focusable wins over a control inside it; a
        // wrapper label resolves to its field.
        w.ocFocusTarget(host.querySelector('#f-b')).id === 'f-b' &&
        w.ocFocusTarget(host.querySelector('#f-l')).id === 'f-i' &&
        w.ocFocusTarget(host.querySelector('#f-s')).id === 'f-s' &&
        w.ocFocusTarget(null) === null);
    }
    // Every delegated listener scopes its work through one guard, which owns
    // the "closed console owns nothing" rule: a null node, a node outside the
    // shell, and an absent shell are all false rather than a throw.
    check('one guard scopes every delegated listener to the console',
      w.eval('ocOwns(null)') === false &&
      w.eval('ocOwns(document.body)') === false &&
      w.eval('ocOwns(document.getElementById("oc-pane"))') === true &&
      w.eval('ocOwns(document.getElementById("btn-controls"))') === false);

    const modeChips = [...d.querySelectorAll('#oc-pane [data-oc-choice="export-mode"]')]
      .map(chip => chip.dataset.ocValue);
    // All history is not a claim of "everything": it previews through the same
    // server count the filtered mode uses, so its receipt is a number too.
    check('the export section offers all history and matching records',
      JSON.stringify(modeChips) === JSON.stringify(['all', 'filter']) &&
      await settleUntil(() =>
        /^\d+ records? · JSON, gzip$/.test(d.getElementById('oc-export-receipt').textContent), 3000));

    // Controlled record set: both surfaces derive their options from lastData
    // through their own owners, so one fixture proves the vocabularies agree.
    w.__logsParityRecords = [
      { ...mkRec('parity-a', 200), provider: 'p', model: 'glm-5.3', client: 'c', conversation_id: 'cv' },
      { ...mkRec('parity-b', 429), provider: 'other', model: 'grok-4.6', client: 'c2', conversation_id: 'cv2' },
    ];
    w.eval('lastData.records = window.__logsParityRecords');

    host = d.createElement('div');
    host.innerHTML = w.eval('settingsDeleteRowHTML()');
    d.body.appendChild(host);
    w.populateFilterMenu('del');

    d.querySelector('#oc-pane [data-oc-choice="export-mode"][data-oc-value="filter"]').click();
    // A mode switch invalidates the count, so the receipt asks for a filter
    // rather than showing the unfiltered total beside a download that would
    // act on a different predicate.
    check('filter mode without an active filter asks for one and disables download',
      await settleUntil(() =>
        d.getElementById('oc-export-receipt').textContent === 'choose a filter to preview the export', 3000) &&
      d.querySelector('#oc-pane [data-oc-download]').disabled);
    const exportValues = dim => {
      d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`).click();
      const values = [...d.querySelectorAll(`#oc-pane [data-oc-filter-value="${dim}"]`)]
        .map(row => row.dataset.ocValue).filter(value => value !== '');
      d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`).click();
      return values;
    };
    const delValues = dim => [...d.getElementById('del-' + dim).options]
      .map(option => option.value).filter(value => value !== '');
    check('settings delete and console export derive the same provider values',
      JSON.stringify(exportValues('provider')) === JSON.stringify(delValues('provider')));
    check('settings delete and console export derive the same model values',
      JSON.stringify(exportValues('model')) === JSON.stringify(delValues('model')));
    check('settings delete and console export derive the same client values',
      JSON.stringify(exportValues('client')) === JSON.stringify(delValues('client')));
    check('settings delete and console export derive the same status values',
      JSON.stringify(exportValues('status')) === JSON.stringify(delValues('status')));
    // Exporting history is the section's purpose, and after a restart the
    // live ring is empty while the server still knows what ran. A dimension
    // fed only from the ring would have nothing to offer and no way to type.
    {
      const savedRecords = w.eval('lastData.records');
      w.applyThrottleState({ok: true, active: true,
        known_providers: ['p'], known_clients: ['c'], known_models: ['glm-5.3'],
        throttles: []});
      w.eval('lastData.records = []');
      w.openOperatorConsole('export');
      d.querySelector('#oc-pane [data-oc-choice="export-mode"][data-oc-value="filter"]').click();
      const offered = dim => {
        d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`).click();
        const values = [...d.querySelectorAll(`#oc-pane [data-oc-filter-value="${dim}"]`)]
          .map(row => row.dataset.ocValue).filter(value => value !== '');
        d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`).click();
        return values;
      };
      check('an empty record ring still offers the server provider and client vocabularies',
        offered('provider').includes('p') && offered('client').includes('c'));
      // offered() repaints through the section, which closes the picker, so
      // open one directly and read the fallback field it renders.
      const typedField = dim => {
        d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`).click();
        const field = d.querySelector(`#oc-pane [data-oc-filter-custom="${dim}"]`);
        return !!field;
      };
      check('a quiet proxy can still filter an export by a typed model or provider',
        typedField('model') && typedField('provider') && typedField('client'));
      w.eval('lastData.records = ' + JSON.stringify(savedRecords || null));
    }

    check('the console shares the settings sentence-case filter dimensions',
      ['provider', 'model', 'client', 'status'].every(dim =>
        !!d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`)) &&
      !!d.querySelector('#oc-pane [data-oc-export-field="errorType"]') &&
      ['export-errors', 'export-debug', 'export-age'].every(name =>
        !!d.querySelector(`#oc-pane [data-oc-choice="${name}"]`)));

    // A picker list is a 190px scroller, so the rows beyond the visible band
    // are pure layout work. The export conversation picker over a record ring
    // at its configured ceiling emitted 10,001 rows: 30,083 nodes and 2.6MB of
    // HTML rebuilt on every keystroke, measured at 248-295ms, and the hidden
    // panel retained them after close.
    {
      const ring = [];
      for (let i = 0; i < 400; i++) {
        ring.push({id: 'r' + i, started_at: Date.now() - i, client: 'c',
          provider: 'p', model: 'm', conversation_id: 'conv-' + String(i).padStart(4, '0'),
          status: 200});
      }
      const savedData = w.eval('lastData');
      w.eval('lastData = ' + JSON.stringify({records: ring}));
      try {
        w.eval("ocDrafts.export = ocBlankExportDraft(); ocDrafts.export.mode = 'filter';"
          + " ocDrafts.export.picker = {dim: 'conversation', filter: ''}; ocRender();");
        const list = d.querySelector('#oc-pane .oc-pick-list');
        const cap = w.eval('OC_PICK_ROWS_MAX');
        const valueRows = list
          ? [...list.querySelectorAll('.oc-pick-row')].filter(row => row.dataset.ocValue !== '').length
          : 0;
        check('a picker caps its value rows at the shared ceiling',
          cap > 0 && valueRows > 0 && valueRows <= cap &&
          w.eval("ocFilterValues('conversation').length") > cap);
        const note = d.querySelector('#oc-pane .oc-pick-note');
        check('a truncated picker names the remainder instead of reading complete',
          (note?.textContent || '').includes('more match') &&
          (note?.textContent || '').includes('300 more'));
        // Narrowing to one value must show it, not the cap's first page.
        w.eval("ocDrafts.export.picker.filter = 'conv-0399'; ocRender();");
        const narrowed = [...d.querySelectorAll('#oc-pane .oc-pick-row[data-oc-filter-value="conversation"]')]
          .map(row => row.dataset.ocValue).filter(Boolean);
        check('a narrowed picker still reaches a value past the cap',
          narrowed.includes('conv-0399') && narrowed.length === 1);
      } finally {
        w.eval('lastData = ' + JSON.stringify(savedData));
      }
    }

    // FILTER_AGES is the one age vocabulary; the console renders its tokens
    // through the shared choice control and ocExportFilter reads them.
    check('FILTER_AGES offers canonical Go duration spellings',
      JSON.stringify(w.eval('FILTER_AGES')) === JSON.stringify([
        ['', 'any'], ['1h', '1 hour'], ['24h', '1 day'], ['168h', '1 week']]));
    // The age list is four labelled spans, so it renders as a select; its
    // options are still exactly the FILTER_AGES tokens.
    const ageChoice = d.querySelector('#oc-pane [data-oc-choice="export-age"]');
    check('the console export age choices are the FILTER_AGES tokens',
      ageChoice.tagName === 'SELECT' &&
      JSON.stringify([...ageChoice.options].map(option => option.value)) ===
        JSON.stringify(w.eval('FILTER_AGES').map(([value]) => value)));

    const now = Date.now();
    const filter = w.ocExportFilter({
      provider: 'p', model: 'm', client: 'c', conversation: 'cv', errorType: 'rate_limit',
      status: '429', errors: true, debug: true, age: '1h', after: '24h',
    });
    check('ocExportFilter emits exactly the server purge filter keys',
      JSON.stringify(Object.keys(filter).sort()) === JSON.stringify([
        'after_ms', 'before_ms', 'client', 'conversation_id', 'debug', 'error_type',
        'has_error', 'model', 'provider', 'status_code']));
    check('ocExportFilter maps each export dimension to its server key',
      filter.provider === 'p' && filter.model === 'm' && filter.client === 'c' &&
      filter.conversation_id === 'cv' && filter.error_type === 'rate_limit' &&
      filter.status_code === 429 && filter.has_error === true && filter.debug === true &&
      filter.before_ms > now - 3700000 && filter.before_ms < now - 3500000 &&
      filter.after_ms > now - 24 * 3600000 - 10000 && filter.after_ms < now - 24 * 3600000 + 10000);
    check('an empty export filter is a fully empty server predicate',
      JSON.stringify(w.ocExportFilter({})) === JSON.stringify({
        provider: '', model: '', client: '', conversation_id: '', error_type: '',
        status_code: 0, has_error: false, debug: false, before_ms: 0, after_ms: 0,
      }));
    check('ocExportURL encodes the filter with the server keys',
      w.ocExportURL({ provider: 'openai', status_code: 429, has_error: false, debug: false,
        before_ms: 0, model: '', client: '', conversation_id: '', error_type: '', after_ms: 0 }) ===
        '/metrics/export?provider=openai&status_code=429');
    check('empty filter exports everything', w.ocExportURL({}) === '/metrics/export');
    // One predicate decides what counts as a chosen field, and the URL builder
    // and the count gate both read it: a field the URL omits must not arm the
    // gate, and a field the gate counts must reach the URL.
    check('one predicate decides whether a filter field carries a choice',
      w.eval('ocPredicateSet("")') === false && w.eval('ocPredicateSet(0)') === false &&
      w.eval('ocPredicateSet(false)') === false && w.eval('ocPredicateSet("x")') === true &&
      w.eval('ocPredicateSet(429)') === true && w.eval('ocPredicateSet(true)') === true &&
      w.ocExportURL({ provider: '', status_code: 0, has_error: false, model: 'm' }) ===
        '/metrics/export?model=m');
    // The "choose a filter" gate reads the predicate owner, so a field that
    // reaches ocExportFields is armed the moment it exists instead of the mode
    // looking ready while the count refuses to run.
    {
      // The section is left exactly as this check found it: the lines below
      // continue driving the live picker, so a stray mode reset here would
      // remove the control they click.
      const savedDraft = w.eval('JSON.stringify(ocDrafts.export)');
      const armed = {};
      const draftOf = patch => JSON.stringify(Object.assign(
        JSON.parse(w.eval('JSON.stringify(ocBlankExportDraft())')), {mode: 'filter'}, patch));
      w.eval('ocDrafts.export = ' + draftOf({}));
      armed.blank = w.eval('ocFilterActive(ocDrafts.export)');
      for (const field of ['provider', 'model', 'client', 'conversation', 'errorType', 'status']) {
        w.eval('ocDrafts.export = ' + draftOf({[field]: field === 'status' ? '500' : 'x'}));
        armed[field] = w.eval('ocFilterActive(ocDrafts.export)');
      }
      for (const field of ['errors', 'debug']) {
        w.eval('ocDrafts.export = ' + draftOf({[field]: true}));
        armed[field] = w.eval('ocFilterActive(ocDrafts.export)');
      }
      for (const field of ['age', 'after']) {
        w.eval('ocDrafts.export = ' + draftOf({[field]: '1h'}));
        armed[field] = w.eval('ocFilterActive(ocDrafts.export)');
      }
      w.eval("ocDrafts.export = Object.assign(ocBlankExportDraft(), {mode: 'all', provider: 'x'})");
      armed.allMode = w.eval('ocFilterActive(ocDrafts.export)');
      check('the export filter gate arms on every predicate field and stays off when empty',
        armed.blank === false && armed.allMode === false &&
        ['provider', 'model', 'client', 'conversation', 'errorType', 'status', 'errors', 'debug', 'age', 'after']
          .every(field => armed[field] === true));
      w.eval('ocDrafts.export = ' + savedDraft + '; ocRender()');
    }
    check('debug-only export encodes the shared flag',
      w.ocExportURL({ debug: true }) === '/metrics/export?debug=1');
    check('conversation and error type ride their server keys',
      w.ocExportURL({ conversation_id: 'cv', error_type: 'rate_limit' }) ===
        '/metrics/export?conversation_id=cv&error_type=rate_limit');

    // Count preview: purgeCount reads /admin/purge/count with the same
    // predicate, debounced 280ms behind the last filter mutation.
    let countCalls = 0;
    let countBody = null;
    w.fetch = (url, options) => {
      if (String(url).includes('/admin/purge/count')) {
        countCalls++;
        countBody = JSON.parse(options.body);
        return Promise.resolve({ ok: true, json: async () => ({ count: 3 }) });
      }
      return savedFetch(url, options);
    };
    d.querySelector('#oc-pane [data-oc-filter-pick="provider"]').click();
    d.querySelector('#oc-pane [data-oc-filter-value="provider"][data-oc-value="p"]').click();
    check('the export count waits behind the debounce', countCalls === 0);
    check('the debounced export count lands and paints its receipt',
      await settleUntil(() => countCalls === 1 &&
        d.getElementById('oc-export-receipt').textContent === '3 records · JSON, gzip'));
    check('the count preview posts the same predicate the download uses',
      countBody && countBody.provider === 'p' &&
      JSON.stringify(Object.keys(countBody).sort()) === JSON.stringify([
        'after_ms', 'before_ms', 'client', 'conversation_id', 'debug', 'error_type',
        'has_error', 'model', 'provider', 'status_code']));

    let downloaded = null;
    w.triggerDownload = (href, download) => { downloaded = { href, download }; };
    d.querySelector('#oc-pane [data-oc-download]').click();
    // The artifact streams, so this code cannot observe its status. The phrase
    // reports the action taken, not an outcome nobody watched.
    check('download uses the previewed filter URL and reports the request it made',
      downloaded && downloaded.href === '/metrics/export?provider=p' && downloaded.download === '' &&
      !d.querySelector('#oc-pane [data-oc-download]').disabled &&
      d.getElementById('oc-change').textContent === 'downloading 3 records');

    d.querySelector('#oc-pane [data-oc-reset-export]').click();
    check('reset returns the export section to all history and recounts it',
      await settleUntil(() =>
        /^\d+ records? · JSON, gzip$/.test(d.getElementById('oc-export-receipt').textContent), 3000) &&
      d.querySelector('#oc-pane [data-oc-choice="export-mode"][data-oc-value="all"]')
        .getAttribute('aria-pressed') === 'true');
  } finally {
    w.fetch = savedFetch;
    w.triggerDownload = savedDownload;
    if (host) host.remove();
    if (savedRecords) {
      w.__logsParitySaved = savedRecords;
      w.eval('lastData.records = window.__logsParitySaved');
      delete w.__logsParitySaved;
    }
    delete w.__logsParityRecords;
    w.closeOperatorConsole();
    w.eval("ocDrafts.export = null; ocError = ''");
  }
};
