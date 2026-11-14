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

    const modeChips = [...d.querySelectorAll('#oc-pane [data-oc-choice="export-mode"]')]
      .map(chip => chip.dataset.ocValue);
    check('the export section offers all history and matching records',
      JSON.stringify(modeChips) === JSON.stringify(['all', 'filter']) &&
      d.getElementById('oc-export-receipt').textContent === 'every stored record');

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
    check('filter mode without an active filter asks for one and disables download',
      d.getElementById('oc-export-receipt').textContent === 'choose a filter to preview the export' &&
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
    check('the console shares the settings sentence-case filter dimensions',
      ['provider', 'model', 'client', 'status'].every(dim =>
        !!d.querySelector(`#oc-pane [data-oc-filter-pick="${dim}"]`)) &&
      !!d.querySelector('#oc-pane [data-oc-export-field="errorType"]') &&
      ['export-errors', 'export-debug', 'export-age'].every(name =>
        !!d.querySelector(`#oc-pane [data-oc-choice="${name}"]`)));

    // FILTER_AGES is the one age vocabulary; the console renders it as chips
    // and ocExportFilter reads the same tokens.
    check('FILTER_AGES offers canonical Go duration spellings',
      JSON.stringify(w.eval('FILTER_AGES')) === JSON.stringify([
        ['', 'any'], ['1h', '1 hour'], ['24h', '1 day'], ['168h', '1 week']]));
    const ageChips = [...d.querySelectorAll('#oc-pane [data-oc-choice="export-age"]')]
      .map(chip => chip.dataset.ocValue);
    check('the console export age choices are the FILTER_AGES tokens',
      JSON.stringify(ageChips) === JSON.stringify(w.eval('FILTER_AGES').map(([value]) => value)));

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
    check('download uses the previewed filter URL',
      downloaded && downloaded.href === '/metrics/export?provider=p' && downloaded.download === '' &&
      !d.querySelector('#oc-pane [data-oc-download]').disabled &&
      d.getElementById('oc-change').textContent === 'downloaded 3 records');

    d.querySelector('#oc-pane [data-oc-reset-export]').click();
    check('reset returns the export section to all history',
      d.getElementById('oc-export-receipt').textContent === 'every stored record' &&
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
