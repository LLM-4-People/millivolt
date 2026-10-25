// jsdom functional test for the dashboard's incremental live-update pipeline.
// Most checks drive the page through its external interfaces: the stubbed
// fetch() and the EventSource listeners the page registers itself. Focused
// helper sections call production render/merge helpers directly to pin
// optimizer contracts that the external paths cannot isolate; they supplement,
// not replace, the end-to-end coverage.
//
// The shared page, fixtures and stub state live in tests/ui_harness/; the
// ordered section modules under tests/ui_harness/sections/ run in registry
// order. This file stays the single entry point (`npm test`).
const harness = require('./ui_harness/harness');
const sections = require('./ui_harness/sections');
const {
  dom, w, d, html, check, reportFailures, rows, fire, sleep, settleUntil,
  installDiagnostics, checkEntityIdentity, assembleHTML, dashboardDOM, loaded, JSDOM, fs,
  path, STATIC, mkRec, LIST, BIG, CHART_FROM, BUCKET_MS, mkChart, modelFixture,
  observerFixture, DEF_RULES, cfgDoc, TEST_DASHBOARD_VERSION, pageOptions, state
} = harness;

async function main() {
  // Deferred asset scripts load through the interceptor after parsing. Wait
  // for the load event before inspecting styles or driving the dashboard.
  await loaded(dom);
  installDiagnostics();
  checkEntityIdentity();
  // Shared CSS must preserve sentence case, including protocol acronyms and
  // user-authored names. Inspect every formerly forced-uppercase owner.
  {
    // jsdom does not apply viewport media queries; inspect those declarations
    // as well so the shared mobile picker cannot force uppercase unnoticed.
    const declarations = [];
    const visitRules = rules => {
      for (const rule of rules) {
        if (rule.style) declarations.push(rule.style.getPropertyValue('text-transform'));
        if (rule.cssRules) visitRules(rule.cssRules);
      }
    };
    for (const sheet of d.styleSheets) visitRules(sheet.cssRules);
    check('dashboard labels never force uppercase, including mobile media rules',
      !declarations.some(value => value === 'uppercase' || value === 'capitalize'));
    const host = d.createElement('section');
    host.innerHTML = '<div class="pause-known-hd"></div><div class="lim-src"></div>' +
      '<div class="clear-opts"><label></label></div><div class="kpi"><h2></h2></div>' +
      '<span class="xp-dim-by"></span><div class="prov-lb"></div><div class="mr-what"></div>' +
      '<div class="mr-ex-hd"></div><div class="card"><h2></h2></div><div class="ct-h"></div>' +
      '<table><thead><tr><th></th></tr></thead></table><span class="day-chip"></span>' +
      '<div class="detail-section"><h4></h4></div>';
    d.body.appendChild(host);
    try {
      for (const selector of ['.pause-known-hd', '.lim-src', '.clear-opts label', '.kpi h2',
        '.xp-dim-by', '.prov-lb', '.mr-what', '.mr-ex-hd', '.card h2', '.ct-h', 'th',
        '.day-chip', '.detail-section h4']) {
        const label = host.querySelector(selector);
        label.textContent = 'Latency TTFT for Model.MixedCase';
        check(`${selector} preserves sentence case and exact labels`,
          w.getComputedStyle(label).textTransform === 'none' &&
          label.textContent === 'Latency TTFT for Model.MixedCase');
      }
    } finally { host.remove(); }
  }
  await sleep(200); // initial fetch + renderAll

  for (const run of sections) await run(harness);

  // ---- W16 mirror pins: the Go<->JS pairs the mutation rounds proved unguarded ----
  // One isolated page built directly (not through dashboardDOM's fetch
  // wrapper) so the debug-capture fixture reaches the drawer byte-exact.
  // Every pin names the Go owner it mirrors; the Go suites pin their side of
  // each pair.
  {
    const captureDoc = {
      schema: 'millivolt.debug/v1',
      id: 'dbg-drawer',
      captured_at: '2026-09-14T00:00:00Z',
      expires_at: '2026-09-15T00:00:00Z',
      debug_session_id: 'first',
      match: { client: 'c', provider: 'neutral.example', model: 'm' },
      identity: {
        conversation_id: 'cv', client: 'c', provider: 'neutral.example', model: 'm',
        key_hash: 'kh', stream: true, path: '/v1/chat/completions', method: 'POST', format: 'openai',
      },
      timing: { start: '2026-09-14T00:00:00Z', end: '2026-09-14T00:00:01Z', duration_ms: 1000, ttft_ms: 250, queue_wait_ms: 10, first_answer_ms: 300 },
      outcome: { status_code: 200, finish_reason: 'stop', error_type: '', error_code: '', error_msg: '', client_disconnected: false },
      usage: { input_tokens: 10, output_tokens: 5, total_tokens: 15 },
      cost: 0.01,
      request: {
        headers: [{name: 'Authorization', value: '[REDACTED]'}, {name: 'X-Empty-Value', value: ''}],
        body: { raw: '{"model":"m"}', truncated: true },
        url: 'https://up.example/v1/chat/completions',
      },
      response: {
        headers: [{name: 'Content-Type', value: 'application/json'}],
        body: { raw: '{"id":"1"}', truncated: false },
      },
      attempts: [],
    };
    const seed = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'w16-mirrors', seq: 1,
      pending_revision: 0, incremental: false,
      records: [{...mkRec('dbg-drawer', 200), debug: true}],
      in_flight_records: [],
      counters: { in_flight: 0, total_requests: 1, total_errors: 0 },
      kpi: { requests: 1, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null },
      dash: {},
      storage: { enabled: true, dropped: 0, totals_degraded: false },
    });
    const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      const fallback = win.fetch;
      const dlFetches = [];
      win.__dlFetches = dlFetches;
      win.__dlResponse = { ok: true, blob: async () => new win.Blob(['gz-bytes']),
        headers: { get: k => k === 'Content-Disposition' ? 'attachment; filename="millivolt-debug-20260914-000000.json.gz"' : null } };
      win.fetch = (url, opts) => {
        const u = String(url);
        if (u.includes('/admin/debug/capture')) {
          if (u.includes('download=1')) {
            dlFetches.push({ u, auth: (opts && opts.headers && opts.headers.Authorization) || '' });
            return Promise.resolve(win.__dlResponse);
          }
          return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(captureDoc)) });
        }
        return fallback(url, opts);
      };
    }});
    const sw = isolated.window, sd = sw.document;
    try {
      await loaded(isolated);
      await sleep(60);

      // recordIsError mirrors metrics.Record.IsError. The 24 rows mirror the
      // semantics of tests/_go/internal/metrics/iserror_corpus.go (the Go
      // suite owns the corpus); the JS predicate must agree on every row.
      const corpus = [
        ['final 200, no attempts', {status_code: 200}, false],
        ['final 429 rate limit', {status_code: 429, error_type: 'rate_limit_error'}, false],
        ['final 499 client closed', {status_code: 499}, false],
        ['final 499 after an absorbed 5xx', {status_code: 499, attempts: [{status_code: 503}]}, true],
        ['final 500', {status_code: 500}, true],
        ['final 502', {status_code: 502}, true],
        ['final 400 (4xx other than 429)', {status_code: 400}, true],
        ['structured error, status 0', {status_code: 0, error_type: 'upstream_unreachable'}, true],
        ['recovered after absorbed 5xx', {status_code: 200, attempts: [{status_code: 502}]}, true],
        ['recovered after absorbed 500 (the >= 500 boundary)', {status_code: 200, attempts: [{status_code: 500}]}, true],
        ['recovered after absorbed 429 only', {status_code: 200, attempts: [{status_code: 429, error_type: 'rate_limit'}]}, false],
        ['final 429 after an absorbed 5xx', {status_code: 429, attempts: [{status_code: 503}]}, true],
        ['429 after an absorbed 500 still counts (flow-arm boundary)', {status_code: 429, rate_limited: true, attempts: [{status_code: 500}]}, true],
        ['queue wait is not429', {status_code: 200}, false],
        ['typed final429 without broad flag', {status_code: 429, error_type: 'insufficient_quota'}, false],
        ['recovered429 twice', {status_code: 200, attempts: [{status_code: 429}, {status_code: 429}]}, false],
        ['final429 plus retries', {status_code: 429, attempts: [{status_code: 429}, {status_code: 429}]}, false],
        ['recovered503', {status_code: 200, attempts: [{status_code: 503}]}, true],
        ['final429 after503', {status_code: 429, attempts: [{status_code: 503}]}, true],
        ['final500 after429', {status_code: 500, attempts: [{status_code: 429}]}, true],
        ['pending after429', {attempts: [{status_code: 429}]}, false],
        ['cancel after429', {status_code: 499, attempts: [{status_code: 429}]}, false],
        ['final 499 with in-band error', {status_code: 499, error_type: 'api_error', error_code: 'internal_error'}, true],
        ['final 499 with in-band rate limit error', {status_code: 499, error_type: 'rate_limit_error'}, true],
        ['committed 200 with an in-band 429', {status_code: 200, error_type: 'rate_limit_error', attempts: [{status_code: 429, in_band: true}]}, true],
        ['synthetic in-band 500 counts as an absorbed failure', {status_code: 200, attempts: [{status_code: 500, in_band: true}]}, true],
      ];
      const errorMisses = corpus.filter(([, rec, want]) => sw.recordIsError(rec) !== want).map(([name]) => name);
      check('recordIsError agrees with the 26-row Go IsError corpus' + (errorMisses.length ? ' (missed: ' + errorMisses.join(', ') + ')' : ''),
        errorMisses.length === 0);

      // statusClass mirrors contribStatusClass/statusClass in aggregate.go on
      // the boundary rows: 429 stays a 4xx class (never an error), 499 is its
      // own cancel class, live rows use the pill vocabulary with the
      // paused > throttled > streaming precedence, and a stored stream row
      // with status 0 is a finalized error, never "currently streaming".
      const classRows = [
        [{status_code: 200}, '2xx'], [{status_code: 299}, '2xx'],
        [{status_code: 429}, '4xx'], [{status_code: 404}, '4xx'],
        [{status_code: 499}, 'cancel'],
        [{status_code: 500}, '5xx'], [{status_code: 503}, '5xx'],
        [{}, 'err'], [{status_code: 302}, 'err'],
        [{live: true, paused: true, throttled: true, stream: true}, 'paused'],
        [{live: true, throttled: true, stream: true}, 'throttled'],
        [{live: true, stream: true}, 'streaming'],
        [{live: true}, 'pending'],
        [{stream: true, status_code: 0}, 'err'],
      ];
      const classMisses = classRows.filter(([rec, want]) => sw.statusClass(rec) !== want)
        .map(([rec, want]) => JSON.stringify(rec) + ' -> ' + sw.statusClass(rec) + ' (want ' + want + ')');
      check('statusClass agrees with the Go owner on the boundary rows' + (classMisses.length ? ' (missed: ' + classMisses.join('; ') + ')' : ''),
        classMisses.length === 0);

      // The live pill vocabulary mirrors liveStatusFilter (aggregate.go): the
      // exact set the server accepts as s=, routed through statusClass,
      // while numeric codes match the exact HTTP status.
      check('the live pill vocabulary is exactly the server-accepted s= set',
        JSON.stringify(sw.eval('Object.keys(LIVE_STATUS_FILTERS).sort()')) ===
        JSON.stringify(['paused', 'pending', 'streaming', 'throttled']));
      check('named pills route through statusClass; numeric codes match exactly',
        sw.recordMatchesStatusFilter({live: true, stream: true}, 'streaming') === true &&
        sw.recordMatchesStatusFilter({status_code: 200}, 'streaming') === false &&
        sw.recordMatchesStatusFilter({status_code: 429}, '429') === true &&
        sw.recordMatchesStatusFilter({status_code: 429}, '200') === false &&
        sw.recordMatchesStatusFilter({status_code: 200}, '') === true);

      // recordErrorEntries mirrors errorEntries (aggregate.go): 429 and a
      // plain 499 final produce no entry, a 499 carrying the provider's
      // in-band error does (the client aborted around a real failure), an
      // in-band provider error on a 200 does, and
      // absorbed attempts keep genuine failures (5xx, typed, 4xx) while
      // skipping flow control and bare non-failures. In-band attempt entries
      // are health evidence, never a second disclosure for the final failure,
      // including the synthetic in-band 5xx shape. errorKey mirrors the Go
      // type|code|msg identity the explorer error dimension filters by.
      const entsRec = {
        status_code: 200, error_code: '', error_msg: '',
        attempts: [
          {status_code: 429},
          {status_code: 502, error_type: 'upstream'},
          {status_code: 0, error_type: 'dial_failed'},
          {status_code: 401},
          {status_code: 302},
          {status_code: 429, in_band: true},
          {status_code: 500, in_band: true},
        ],
        start: '2026-09-14T00:00:00Z',
      };
      const ents = sw.recordErrorEntries(entsRec);
      check('recordErrorEntries enumerates the absorbed-attempt boundary rows like the Go owner',
        JSON.stringify(ents.map(e => [e.type, e.code, e.absorbed, e.at])) === JSON.stringify([
          ['upstream', '502', true, '2026-09-14T00:00:00Z'],
          ['dial_failed', '', true, '2026-09-14T00:00:00Z'],
          ['http_401', '401', true, '2026-09-14T00:00:00Z'],
        ]));
      check('final 429 and a plain 499 produce no error entry (flow events)',
        sw.recordErrorEntries({status_code: 429, error_type: 'rate_limit_error'}).length === 0 &&
        sw.recordErrorEntries({status_code: 499}).length === 0);
      check('a 499 carrying the provider in-band error produces an entry like its 200 twin',
        JSON.stringify(sw.recordErrorEntries({status_code: 499, error_type: 'api_error', error_code: 'internal_error', error_msg: 'temporarily unavailable'}).map(e => [e.type, e.code, e.msg])) ===
        JSON.stringify([['api_error', 'internal_error', 'temporarily unavailable']]));
      check('an in-band provider error on a 200 is still a final error entry',
        JSON.stringify(sw.recordErrorEntries({status_code: 200, error_type: 'provider_overloaded', error_msg: 'x'}).map(e => [e.type, e.code, e.msg])) ===
        JSON.stringify([['provider_overloaded', '200', 'x']]));
      check('a final 5xx carries its synthetic http_N entry and code',
        JSON.stringify(sw.recordErrorEntries({status_code: 500}).map(e => [e.type, e.code, e.absorbed])) ===
        JSON.stringify([['http_500', '500', false]]));
      check('errorKey identity is type|code|msg with empty parts kept',
        sw.errorKey('http_500', '500', '') === 'http_500|500|' &&
        sw.errorKey('', '', '') === '||' &&
        ents.every(e => sw.errorKey(e.type, e.code, e.msg) === [e.type, e.code, e.msg].join('|')));

      // The duration vocabularies mirror Go acceptance: PAUSE_DURS and
      // DEBUG_DURS tokens are the pauseDurations allowlist (pinned through
      // the real pause/debug handlers Go-side), LIMIT_WINDOWS tokens pass
      // parseLimitWindow, and FILTER_AGES tokens are canonical
      // config.FormatDuration spellings. Labels ride the same pin so the
      // '15 min' vs '15 minutes' drift class cannot return.
      check('PAUSE_DURS offers exactly the server-accepted hold durations',
        JSON.stringify(sw.eval('PAUSE_DURS')) === JSON.stringify([
          ['', 'I resume'], ['15m', '15 minutes'], ['1h', '1 hour'],
          ['6h', '6 hours'], ['12h', '12 hours'], ['24h', '24 hours']]));
      check('DEBUG_DURS shares the pause vocabulary with its own empty label',
        JSON.stringify(sw.eval('DEBUG_DURS')) === JSON.stringify([
          ['', 'I stop'], ['15m', '15 minutes'], ['1h', '1 hour'],
          ['6h', '6 hours'], ['12h', '12 hours'], ['24h', '24 hours']]));
      check('LIMIT_WINDOWS offers exactly the parseLimitWindow-accepted tokens',
        JSON.stringify(sw.eval('LIMIT_WINDOWS')) === JSON.stringify([
          ['1s', '1 second'], ['10s', '10 seconds'], ['30s', '30 seconds'],
          ['1m', '1 minute'], ['5m', '5 minutes'], ['15m', '15 minutes'],
          ['1h', '1 hour'], ['6h', '6 hours'], ['24h', '24 hours']]));
      check('FILTER_AGES offers canonical Go duration spellings',
        JSON.stringify(sw.eval('FILTER_AGES')) === JSON.stringify([
          ['', '-'], ['1h', '1 hour'], ['24h', '1 day'], ['168h', '1 week']]));
      sw.togglePauseMenu({stopPropagation() {}});
      check('the pause menu renders the PAUSE_DURS token set',
        JSON.stringify([...sd.getElementById('pf-dur').options].map(o => o.value)) ===
        JSON.stringify(['', '15m', '1h', '6h', '12h', '24h']));
      sw.togglePauseMenu({stopPropagation() {}});

      // holdMatchesRecord mirrors scheduler holdSnap.matches on the scope
      // half (scheduler/hold.go); chrome.js holdLive owns the expiry half
      // the Go matcher folds in. The matrix mirrors the Go suite's
      // TestHoldMatchesMatrix row for row (that suite owns the table, the
      // hand-mirror documented here): the wire keys are all/new/clients/
      // providers/known_at_new, and every verdict must agree.
      const holdRows = [
        ['all matches any request', {all: true}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['all matches an empty identity', {all: true}, {client: '', provider: ''}, true],
        ['new skips a known client', {new: true, known_at_new: ['known']}, {client: 'known', provider: 'alpha.example'}, false],
        ['new matches an unseen client', {new: true, known_at_new: ['known']}, {client: 'fresh', provider: 'alpha.example'}, true],
        ['named client matches itself', {clients: ['client-a']}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['named client matches with any provider', {clients: ['client-a']}, {client: 'client-a', provider: ''}, true],
        ['named client misses another client', {clients: ['client-a']}, {client: 'client-b', provider: 'alpha.example'}, false],
        ['named hit wins over new', {clients: ['client-a'], new: true, known_at_new: ['client-a', 'seen']}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['named plus new skips a known non-member', {clients: ['client-a'], new: true, known_at_new: ['client-a', 'seen']}, {client: 'seen', provider: 'alpha.example'}, false],
        ['named plus new matches an unseen client', {clients: ['client-a'], new: true, known_at_new: ['client-a', 'seen']}, {client: 'brand-new', provider: 'alpha.example'}, true],
        ['provider-only matches its provider with any client', {providers: ['alpha.example']}, {client: 'whoever', provider: 'alpha.example'}, true],
        ['provider-only misses another provider', {providers: ['alpha.example']}, {client: 'whoever', provider: 'beta.example'}, false],
        ['provider-only never matches an empty provider argument', {providers: ['alpha.example']}, {client: 'whoever', provider: ''}, false],
        ['client and provider AND-match', {clients: ['client-a'], providers: ['alpha.example']}, {client: 'client-a', provider: 'alpha.example'}, true],
        ['pair hold misses on provider', {clients: ['client-a'], providers: ['alpha.example']}, {client: 'client-a', provider: 'beta.example'}, false],
        ['pair hold misses on client', {clients: ['client-a'], providers: ['alpha.example']}, {client: 'client-b', provider: 'alpha.example'}, false],
        ['dimensionless hold is inactive', {all: false, clients: [], providers: [], new: false}, {client: 'client-a', provider: 'alpha.example'}, false],
      ];
      const holdMisses = holdRows
        .filter(([name, h, r, want]) => sw.eval(`holdMatchesRecord(${JSON.stringify(h)}, ${JSON.stringify(r)})`) !== want)
        .map(([name]) => name);
      check('holdMatchesRecord agrees with the Go matcher matrix' +
        (holdMisses.length ? ' (missed: ' + holdMisses.join(', ') + ')' : ''),
        holdMisses.length === 0);
      check('holdLive owns the expiry half the Go matcher folds in',
        sw.eval(`holdLive({all: true, until: new Date(Date.now() - 60000).toISOString()})`) === false &&
        sw.eval(`holdLive({all: true, until: new Date(Date.now() + 60000).toISOString()})`) === true);

      // parseGoDuration mirrors Go's duration grammar for every spelling
      // config.FormatDuration emits (pinned Go-side by
      // TestFormatDurationSpellingsParseBack, whose d.String() fallback row
      // spells microseconds with the micro sign, and whose parser accepts
      // the Greek mu too). Rows mirror that Go table; values are
      // milliseconds.
      const durRows = [
        ['0s', 0], ['1ns', 0.000001], ['1.5µs', 0.0015], ['1500us', 1.5],
        ['1500μs', 1.5], ['5ms', 5], ['500ms', 500], ['2s', 2000],
        ['90m', 5400000], ['3h', 10800000], ['24h', 86400000], ['-5s', -5000],
        ['1h30m', 5400000], ['0.5s', 500],
      ];
      const durMisses = durRows
        .filter(([spelling, want]) => sw.eval(`parseGoDuration(${JSON.stringify(spelling)})`) !== want)
        .map(([spelling]) => spelling);
      check('parseGoDuration parses every FormatDuration spelling the surfaces use' +
        (durMisses.length ? ' (missed: ' + durMisses.join(', ') + ')' : ''),
        durMisses.length === 0);
      check('parseGoDuration fails closed to 0 on garbage and empty input',
        sw.eval('parseGoDuration(\'45 min\')') === 0 &&
        sw.eval('parseGoDuration(\'\')') === 0 &&
        sw.eval('parseGoDuration(null)') === 0);

      // MODEL_RULES_MAX mirrors config.ModelRulesMax (pinned Go-side): the
      // count line and the add gate at the cap.
      {
        const wrap = sd.createElement('div');
        wrap.innerHTML = sw.modelRulesEditorHTML(Array.from({length: 64}, () => ({mode: 'lower'})));
        sd.getElementById('settings-fields').appendChild(wrap);
        sw.mrPreview(wrap);
        check('a full 64-rule draft shows the capped count line',
          wrap.querySelector('.mr-count').textContent === '64 / 64 rules');
        check('the add gate refuses a 65th rule at MODEL_RULES_MAX',
          wrap.querySelector('[data-mr-add]').disabled && wrap.querySelector('.mr-tpl').disabled);
        wrap.querySelector('[data-mr-add]').click();
        check('a disabled add control adds no row',
          wrap.querySelectorAll('.mr-row').length === 64);
        check('the mode select renders its options in the vocabulary\'s order with labels as values',
          JSON.stringify([...wrap.querySelector('.mr-row .mr-mode').options].map(o => o.value)) ===
          JSON.stringify(['exact', 'pattern', 'lower']));
        wrap.remove();
      }

      // The shipped-template derivation against a CRAFTED defaults doc:
      // payloads come from settingsDoc.defaults.model_rules positionally
      // (never a client copy), labels pair by position, and beyond the
      // authored label table the position fallback still offers every rule.
      {
        sw.__craftedDefaults = [
          {mode: 'lower'}, {mode: 'exact', from: 'a', to: 'b'},
          {mode: 'pattern', from: 'x', to: 'y'}, {mode: 'lower'},
          {mode: 'lower'}, {mode: 'exact', from: 'c', to: 'd'},
        ];
        sw.eval('settingsDoc = {defaults: {model_rules: window.__craftedDefaults}}');
        const wrap = sd.createElement('div');
        wrap.innerHTML = sw.modelRulesEditorHTML([{mode: 'lower'}]);
        sd.getElementById('settings-fields').appendChild(wrap);
        const tpl = wrap.querySelector('.mr-tpl');
        check('template labels pair by position with a longer defaults doc',
          [...tpl.options].map(o => o.textContent).join(',') ===
          'add from template…,lowercase fold,strip vendor/ prefix,strip :tag suffix,strip architecture/quant suffix (fp4, nvfp4, int8, q4_k_m…),unify . and - between digits,shipped rule 6,exact merge…,custom pattern…');
        tpl.value = '5';
        tpl.dispatchEvent(new sw.Event('change', {bubbles: true}));
        const rows = [...wrap.querySelectorAll('.mr-row')];
        check('the positional-fallback template carries the crafted doc payload',
          rows.length === 2 &&
          JSON.stringify(sw.collectModelRules(wrap)) === JSON.stringify([{mode: 'lower'}, {mode: 'exact', from: 'c', to: 'd'}]));
        wrap.remove();
        sw.eval('settingsDoc = null');
      }

      // The restart poll budgets mirror the Go choreography bounds
      // (cmd/proxy restartBuildTimeout/restartReadyTimeout and the config
      // default RestartDrainTimeout - each pinned Go-side) and RESTART_STEPS
      // follows the server's phaseRank order.
      check('the restart poll budgets mirror the Go choreography bounds',
        sw.eval('[RESTART_BUILD_BUDGET_MS, RESTART_READY_BUDGET_MS, RESTART_DRAIN_FALLBACK_MS].join()') === '300000,300000,600000');
      check('RESTART_STEPS ranks follow the server phaseRank order',
        sw.eval('RESTART_STEPS.map(s => s.rank).join()') === '0,1,2,3');

      // The two identity-width gates mirror the Go producers: the stamped
      // asset version is 16 lowercase hex (web.go etagHex), the model-canon
      // content revision is 64 lowercase hex (aggregate_observer.go sha256).
      check('the dashboard version gate accepts exactly 16 lowercase hex',
        sw.eval('DASHBOARD_VERSION_RE.test("0000000000000000")') === true &&
        sw.eval('DASHBOARD_VERSION_RE.test("000000000000000")') === false &&
        sw.eval('DASHBOARD_VERSION_RE.test("00000000000000000")') === false &&
        sw.eval('DASHBOARD_VERSION_RE.test("000000000000000A")') === false &&
        sw.eval(`DASHBOARD_VERSION_RE.test('${TEST_DASHBOARD_VERSION}')`) === true);
      check('the model-canon revision gate accepts exactly 64 lowercase hex',
        sw.eval('MODEL_REVISION_RE.test("a".repeat(64))') === true &&
        sw.eval('MODEL_REVISION_RE.test("a".repeat(63))') === false &&
        sw.eval('MODEL_REVISION_RE.test("a".repeat(65))') === false &&
        sw.eval('MODEL_REVISION_RE.test("A".repeat(64))') === false);

      // The entity palette: the registry is the single owner (favicon and
      // PWA-asset hexes are detector-covered against the :root palette
      // Go-side of the CSS; the .prov-ic emitters are pinned in the settings
      // editor tests). This pins what nothing else did: every dimension's
      // badge keys its color through the registry, colors stay pairwise
      // distinct (colorblind-safe classes), and the log's turn-role colors
      // reference the registry instead of a second palette.
      {
        const palette = sw.eval('ENTITY_TYPES');
        const want = {
          client: ['#56B4E9', '◈', 'Client'], provider: ['#0072B2', '☁', 'Provider'],
          model: ['#CC79A7', '◆', 'Model'], conversation: ['#009E73', '❝', 'Conversation'],
          key: ['#F0E442', '⚷', 'Key'], status: ['#8A8F98', '●', 'Status'],
          time: ['#5AC8FA', '◷', 'Time'], tool: ['#1ABC9C', '⚒', 'Tool'],
          request: ['#E69F00', '⇄', 'Request'], error: ['#D55E00', '⚠', 'Error'],
        };
        const got = Object.fromEntries(Object.entries(palette).map(([k, v]) => [k, [v.color, v.icon, v.label]]));
        check('ENTITY_TYPES carries the colorblind-safe palette triple for every dimension',
          JSON.stringify(got) === JSON.stringify(want));
        const colors = Object.values(palette).map(v => v.color.toLowerCase());
        check('entity palette colors stay pairwise distinct', new Set(colors).size === colors.length);
        check('every rendered entity badge keys its color through the registry',
          Object.entries(palette).every(([dim, v]) =>
            sw.entityBadge(dim, 'fixture-id').includes(`style="--ent:${v.color}"`)));
        check('turn-role colors reference the registry, not a second palette',
          JSON.stringify(sw.eval('TURN_ROLE_COLOR')) === JSON.stringify({
            user: '#56B4E9', assistant: '#CC79A7', tool: '#1ABC9C',
            function: '#1ABC9C', system: '#8A8F98', developer: '#8A8F98',
          }));
      }

      // The debug-capture drawer body: key-value rows from the capture doc,
      // the capture-header tint on the key span, empty values denied, and
      // truncated bodies labeled.
      sw.openDrawer('dbg-drawer');
      await sleep(30);
      const dbg = sd.getElementById('drawer-debug');
      const dbgRow = k => [...dbg.querySelectorAll('.detail-kv')].find(r => r.querySelector('.k') && r.querySelector('.k').textContent === k);
      check('the drawer renders the capture identity and timing rows',
        !!dbgRow('session') && dbgRow('session').querySelector('.v').textContent === 'first' &&
        !!dbgRow('client') && dbgRow('client').querySelector('.v').textContent === 'c' &&
        !!dbgRow('stream') && dbgRow('stream').querySelector('.v').textContent === 'yes' &&
        !!dbgRow('duration') && dbgRow('duration').querySelector('.v').textContent === '1s' &&
        !!dbgRow('queue wait') && dbgRow('queue wait').querySelector('.v').textContent === '10ms');
      const headerSection = [...dbg.querySelectorAll('.detail-section')]
        .find(s => s.querySelector('h4') && s.querySelector('h4').textContent === 'Request headers');
      const authRow = headerSection && [...headerSection.querySelectorAll('.detail-kv')]
        .find(r => r.querySelector('.k').textContent === 'Authorization');
      check('capture headers render with the accent tint on the key span',
        !!authRow && authRow.querySelector('.k').getAttribute('style') === 'color:var(--accent2)' &&
        authRow.querySelector('.v').textContent === '[REDACTED]');
      check('an empty capture-header value renders no row (deny by default)',
        !dbg.textContent.includes('X-Empty-Value'));
      check('truncated bodies are labeled and previewed',
        dbg.textContent.includes('Request body (truncated)') &&
        dbg.textContent.includes('Response body') &&
        !!dbg.querySelector('.preview-box'));

      // The download affordance: the section header button fetches the
      // same capture with download=1 through the operator gate and hands
      // the server-named artifact to triggerDownload (the backup flow's
      // blob-fetch pattern), with the fallback and failure contracts.
      const dbgBtn = dbg.querySelector('#drawer-debug-download');
      check('the debug capture section renders the download affordance with its accessible name',
        !!dbgBtn && dbgBtn.getAttribute('aria-label') === 'download debug capture' &&
        dbgBtn.dataset.id === 'dbg-drawer');
      if (dbgBtn) {
        sw.eval("operatorCredential = 'op-token'");
        const dlTape = [];
        sd.addEventListener('click', e => dlTape.push(e.target));
        sw.URL.createObjectURL = () => 'blob:w16-dl';
        sw.URL.revokeObjectURL = href => dlTape.push('revoke:' + href);
        dbgBtn.click();
        await sleep(30);
        check('clicking download fetches the capture artifact through the operator gate',
          sw.__dlFetches.length === 1 &&
          sw.__dlFetches[0].u === '/admin/debug/capture?id=dbg-drawer&download=1' &&
          sw.__dlFetches[0].auth === 'Bearer op-token');
        check('the server-named artifact reaches triggerDownload and the blob URL is revoked',
          dlTape.some(el => el && el.tagName === 'A' &&
            el.getAttribute('download') === 'millivolt-debug-20260914-000000.json.gz') &&
          dlTape.includes('revoke:blob:w16-dl'));
        sw.__dlResponse = { ok: true, blob: async () => new sw.Blob(['gz']), headers: { get: () => null } };
        dbgBtn.click();
        await sleep(30);
        check('a missing Content-Disposition falls back to the designed artifact name',
          dlTape.filter(el => el && el.tagName === 'A')
            .some(a => a.getAttribute('download') === 'millivolt-debug.json.gz'));
        sw.__dlResponse = { ok: false, status: 502, headers: { get: () => null } };
        dbgBtn.click();
        await sleep(30);
        check('a failed capture download surfaces the designed failure message in the section',
          sd.getElementById('drawer-debug-status').textContent === 'debug capture download failed');
      }
      sw.closeDrawer();

      // triggerDownload's anchor contract: href/download set, appended,
      // clicked, removed - and a blob URL is revoked after the click.
      {
        const tape = [];
        sd.addEventListener('click', e => tape.push(e.target));
        sw.URL.createObjectURL = () => 'blob:w16-fixture';
        sw.URL.revokeObjectURL = href => tape.push('revoke:' + href);
        sw.triggerDownload('/metrics/export?x=1', '');
        check('triggerDownload builds the anchor with href/download, clicks and removes it',
          tape.length === 1 && tape[0].tagName === 'A' &&
          tape[0].getAttribute('href') === '/metrics/export?x=1' &&
          tape[0].getAttribute('download') === '' && !tape[0].isConnected);
        sw.triggerDownload('blob:w16-fixture', 'millivolt-backup.mvb', true);
        check('a blob download names the file and revokes the URL after the click',
          tape.length === 3 && tape[1].tagName === 'A' &&
          tape[1].getAttribute('href') === 'blob:w16-fixture' &&
          tape[1].getAttribute('download') === 'millivolt-backup.mvb' &&
          tape[2] === 'revoke:blob:w16-fixture');
      }
    } finally { sw.close(); }
  }

  // ---- lazy Settings module ----
  // Production keeps settings.js out of the first-paint shell; the trigger
  // injects exactly one versioned request and the sheet opens only after that
  // module has initialized. These checks assemble without settings.js so the
  // real chrome.js trigger/loader path runs, then simulate the script element's
  // load/error events (jsdom does not fetch external scripts).
  {
    const shell = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
    check('production index.html has no settings.js script reference',
      !/<script\b[^>]*\bsrc=["'][^"']*\/dash\/js\/settings\.js/.test(shell));

    const lazyShell = async () => {
      const appended = [];
      const errors = [];
      const dom = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), {
        ...pageOptions,
        beforeParse(win) {
          pageOptions.beforeParse(win);
          win.console.error = (...args) => errors.push(args.join(' '));
          const append = win.HTMLHeadElement.prototype.appendChild;
          win.HTMLHeadElement.prototype.appendChild = function (node) {
            if (node && node.tagName === 'SCRIPT' &&
                String(node.getAttribute('src') || '').includes('/dash/js/settings.js')) {
              appended.push(node);
              return node;
            }
            return append.call(this, node);
          };
        },
      });
      await loaded(dom);
      return { dom, sw: dom.window, sd: dom.window.document, appended, errors };
    };

    const ok = await lazyShell();
    try {
      check('first paint issues no settings.js request', ok.appended.length === 0);
      check('the settings sheet starts closed before the module loads',
        ok.sd.getElementById('settings-sheet').hidden);
      // Escape and the header close path must be safe before the module exists.
      ok.sd.dispatchEvent(new ok.sw.KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
      ok.sw.closeHeaderMenus(undefined, true);
      ok.sw.closeSettings(true);
      check('close and Escape are safe no-ops before settings.js loads',
        ok.sd.getElementById('settings-sheet').hidden);

      ok.sw.toggleSettings({ stopPropagation() {} });
      ok.sw.toggleSettings({ stopPropagation() {} });
      ok.sw.toggleSettings({ stopPropagation() {} });
      const script = ok.appended[0];
      check('repeated and concurrent settings opens issue exactly one versioned request',
        ok.appended.length === 1 && script &&
        script.getAttribute('src') === '/dash/js/settings.js?v=' + TEST_DASHBOARD_VERSION);

      let opened = 0;
      ok.sw.settingsModule = { open() { opened++; }, close() {} };
      check('the sheet stays closed while the settings module is loading',
        ok.sd.getElementById('settings-sheet').hidden);
      script.onload();
      await sleep(0);
      check('the loaded settings module opens the sheet exactly once', opened === 1);
    } finally { ok.dom.window.close(); }

    // A close while the module is still downloading must cancel the pending
    // open; the loader resolving later must not pop the sheet back up.
    const cancelled = await lazyShell();
    try {
      let opened = 0;
      cancelled.sw.toggleSettings({ stopPropagation() {} });
      cancelled.sw.closeSettings(true);
      cancelled.sw.settingsModule = { open() { opened++; }, close() {} };
      cancelled.appended[0].onload();
      await sleep(0);
      check('closing during the settings load cancels the pending open', opened === 0);
      cancelled.sw.toggleSettings({ stopPropagation() {} });
      check('a later settings open uses the loaded module exactly once', opened === 1);
    } finally { cancelled.dom.window.close(); }

    // The chart metrics picker renders at first paint and shares its chevron
    // with the settings provider cards. It must not depend on settings.js.
    {
      const noSettings = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), pageOptions);
      try {
        await loaded(noSettings);
        const sd = noSettings.window.document;
        noSettings.window.eval("chartView.preset = 'overview'");
        noSettings.window.summaryPickerSync();
        const btn = sd.getElementById('chart-metrics-btn');
        check('the chart metrics picker renders before settings.js loads',
          !!btn && btn.innerHTML.includes('<svg') &&
          sd.getElementById('chart-metrics-count').textContent === '5/5');
      } finally { noSettings.window.close(); }
    }

    const failed = await lazyShell();
    try {
      failed.sw.toggleSettings({ stopPropagation() {} });
      check('a settings load attempt starts with the sheet closed',
        failed.appended.length === 1 && failed.sd.getElementById('settings-sheet').hidden);
      failed.appended[0].onerror();
      await sleep(0);
      check('a settings load failure stays closed and surfaces the shared status',
        failed.sd.getElementById('settings-sheet').hidden &&
        failed.sd.getElementById('settings-count').textContent === 'could not load config');
      check('a settings load failure reports a console error',
        failed.errors.some(line => line.includes('settings module failed to load')));
      failed.sw.toggleSettings({ stopPropagation() {} });
      await sleep(0);
      check('a failed settings load is not retried on the next open',
        failed.appended.length === 1);
    } finally { failed.dom.window.close(); }
  }

  // ---- round-2 frontend regressions ----
  // These drive the production render/fetch owners and shipped events; no
  // helper copies. Each block restores the shared page state it changes.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null; clearTimeout(_xpLiveTimer); _xpLiveTimer = null; clearTimeout(logRetryTimer); logRetryTimer = null;');
    // An earlier reload-boundary check intentionally set dashboardReloading
    // and jsdom cannot navigate; the production gate is reset so these checks
    // can drive the real bootstrap path.
    w.eval('dashboardReloading = false');
    w.closeDrawer();
    if (w.settingsIsOpen()) w.closeSettings();

    const auditFeed = w.eval('feedId') || 'feedB';
    const auditBase = 1700002000000;
    state.fullPayload = { feed_id: auditFeed, seq: Math.max(500, w.eval('lastSeq') + 10), incremental: false,
      records: Array.from({ length: 6 }, (_, i) => ({ ...mkRec('audit' + i, 200, auditBase + i * 1000), __seq: i + 1 })),
      counters: { in_flight: 0, total_requests: 6, total_errors: 0 } };
    w.eval("fetchBootstrap('full')");
    await settleUntil(() => rows().length === 6);
    w.navigateTo([]);
    await sleep(50);
    const chartWith = n => {
      const p = JSON.parse(JSON.stringify(state.chartPayload));
      p.now_ms = Date.now();
      p.from_ms = p.now_ms - 3600000;
      p.buckets.forEach(b => { b.req = 0; b.err = 0; });
      if (p.buckets.length) p.buckets[p.buckets.length - 1].req = n;
      return p;
    };

    // F7: one percentage-value formatter applies the pctCap floor to the
    // chart and storm surfaces; only a real >= 100 prints 100.
    const pctEval = expr => {
      try { return w.eval(expr); } catch (e) { return undefined; }
    };
    check('the shared percentage formatter applies pctCap at the boundary',
      pctEval('pctValue(99.949, 1)') === '99.9%' &&
      pctEval('pctValue(99.95, 1)') === '99.9%' &&
      pctEval('pctValue(99.999, 1)') === '99.9%' &&
      pctEval('pctValue(100, 1)') === '100.0%' &&
      pctEval('pctCap(1999, 2000, 1)') === '99.9%' &&
      pctEval('fmtPct(99.95)') === '99.9%' &&
      pctEval('fmtPct(100)') === '100%' &&
      pctEval('typeof stormPercent') === 'undefined');
    w.applyStormState({ enabled: true, banner_enabled: true, storms: [{
      provider: 'neutral.example', model: '', scope: 'provider', state: 'open',
      reason: 'http_503', quota: false, error_percent: 99.95, error_requests: 9,
      failures: 15, samples: 20, window_ms: 60000, queued: 4, active_models: 3,
      affected_models: 2, affected_model_percent: 99.95,
      retry_at: '2026-09-05T12:00:00Z', recovery_successes: 0, recovery_required: 2,
    }] });
    check('storm surfaces use the shared sub-100 percentage floor',
      d.getElementById('storm-banner').textContent.includes('99.9% failed attempts') &&
      !d.getElementById('storm-banner').textContent.includes('100% failed attempts'));
    w.applyStormState({ enabled: false, banner_enabled: false, storms: [] });

    // F8: a rounded cents form that reaches 100 promotes to the dollar form.
    check('a rounded sub-dollar value promotes to the dollar form, never 100 cents',
      w.eval('fmtMoney(0.999999)') === '$1' &&
      w.eval('fmtMoney(0.9999999)') === '$1' &&
      w.eval('fmtMoney(1)') === '$1' &&
      w.eval('fmtMoney(0.99999)') === '99.999¢' &&
      w.eval('fmtMoney(999.9999)') === '$1000');

    // F2: once the archive holds the cap, a fetched older page cannot be
    // retained. The cursor/fill owner must end pagination after that one
    // request instead of growing logLimit and fetching forever.
    {
      const saved = { fetch: w.fetch, fill: w.scheduleLogFill, page: state.logPage, data: w.eval('lastData') };
      w.scheduleLogFill = () => {};
      const capBase = 1600000000000;
      w.__auditCapArchive = Array.from({ length: 20000 }, (_, i) =>
        mkRec('cap' + String(i).padStart(5, '0'), 200, capBase + i * 1000));
      w.eval('logArchive = window.__auditCapArchive; logLimit = 20001; ' +
        'logCursor = {ms: ' + capBase + ', id: "cap00000"}; ' +
        'logExhausted = false; logFetching = false; logFailures = 0; logFetchFailed = false; logFetchGen++; ' +
        'logHTML.clear(); for (const r of logArchive) logHTML.set(r.id, {rec: r, sep: ""}); ' +
        'for (const r of (lastData && lastData.records || [])) logHTML.set(r.id, {rec: r, sep: ""});');
      const urls = [];
      const canon = { rules: [], revision: w.eval('modelRevision'), names: {} };
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/log')) return saved.fetch(url, opts);
        urls.push(u);
        return Promise.resolve({ ok: true, json: async () => ({
          records: [mkRec('cap-old', 200, capBase - 1000)], more: true,
          cursor_ms: capBase - 1000, cursor_id: 'cap-old', model_canon: canon,
        }) });
      };
      try {
        w.fetchOlderLog();
        await settleUntil(() => urls.length === 1 && !w.eval('logFetching'));
        await sleep(120);
        check('the archive cap is terminal: one older-page request, no retained rows',
          urls.length === 1 && w.eval('logArchive.length') === 20000 &&
          !w.eval('logArchive.some(r => r.id === "cap-old")') && w.eval('logExhausted') === true);
        check('the terminal cap does not grow logLimit for discarded rows', w.eval('logLimit') === 20001);
        w.fetchOlderLog();
        await sleep(80);
        check('scroll and delayed fills stop at the terminal archive cap', urls.length === 1);
      } finally {
        w.fetch = saved.fetch;
        w.scheduleLogFill = saved.fill;
        state.logPage = saved.page;
        w.resetLogWindow();
        w.renderAll(saved.data);
        delete w.__auditCapArchive;
      }
    }

    // F10: a grown visible window keeps server/day order. Older page rows
    // append below the loaded history; a live row with no newer neighbor still
    // paints at the top. Day dividers move with their opener, not the insert.
    {
      const saved = {data: w.eval('lastData'), page: w.eval('dashCfg.log_rows'), fill: w.scheduleLogFill};
      w.scheduleLogFill = () => {};
      const growMid = new Date(); growMid.setHours(0, 0, 0, 0);
      const growAt = day => growMid.getTime() - day * 86400000 + 3600000;
      const grow = [
        mkRec('grow-old', 200, growAt(2)),
        mkRec('grow-yday', 200, growAt(1)),
        mkRec('grow-today1', 200, growAt(0)),
        mkRec('grow-today2', 200, growAt(0) + 3600000),
        mkRec('grow-today3', 200, growAt(0) + 7200000),
      ];
      w.__growData = {records: grow, counters: {}, _rev: 0};
      w.eval('logArchive = []; logLimit = 0; dashCfg.log_rows = 2; lastData = window.__growData; ' +
        'renderRequests(derive(lastData).scopedRecs); updateLogStats();');
      check('window growth: first paint honors the page cap',
        rows().map(r => r.dataset.id).join() === 'grow-today3,grow-today2');
      w.eval('dashCfg.log_rows = 5; renderRequestsIncremental(derive(lastData).scopedRecs);');
      check('window growth: older rows append in server order',
        rows().map(r => r.dataset.id).join() === 'grow-today3,grow-today2,grow-today1,grow-yday,grow-old');
      const grownSeps = [...d.querySelectorAll('#tbl-requests tr.day-sep')];
      check('window growth: day dividers stay attached to their openers',
        grownSeps.length === 3 &&
        grownSeps[0]?.nextElementSibling?.dataset.id === 'grow-today3' &&
        grownSeps[1]?.nextElementSibling?.dataset.id === 'grow-yday' &&
        grownSeps[2]?.nextElementSibling?.dataset.id === 'grow-old');
      w.resetLogWindow();
      w.eval('dashCfg.log_rows = ' + saved.page + ';');
      w.renderAll(saved.data);
      w.scheduleLogFill = saved.fill;
      delete w.__growData;
    }

    // F11: a live record that arrives while an older page is in flight is not
    // history. It must paint at the top when the page lands, not be appended
    // under the loaded rows.
    {
      const saved = {data: w.eval('lastData'), page: state.logPage, fill: w.scheduleLogFill};
      w.scheduleLogFill = () => {};
      const base = 1700005000000;
      const ring = [
        mkRec('race-r1', 200, base + 1000),
        mkRec('race-r2', 200, base + 2000),
        mkRec('race-r3', 200, base + 3000),
      ];
      w.__raceData = {records: ring, counters: {}, _rev: 0};
      w.eval('logArchive = []; logLimit = 0; dashCfg.log_rows = 60; lastData = window.__raceData; ' +
        'logFetching = false; logExhausted = false; logCursor = null; ' +
        'renderRequests(derive(lastData).scopedRecs); updateLogStats();');
      let release = null;
      state.logPageGate = new Promise(resolve => { release = resolve; });
      state.logPage = {records: [mkRec('race-old', 200, base)], more: false, cursor_ms: base, cursor_id: 'race-old'};
      try {
        w.fetchOlderLog();
        await sleep(10);
        // The live event lands in the ring while the page is still held; its
        // scheduled paint has not run yet.
        ring.push(mkRec('race-live', 200, base + 4000));
        w.eval('bumpRecords(); bumpData();');
        check('the racing live row is not painted while the page is held',
          !rows().some(tr => tr.dataset.id === 'race-live') && w.eval('logFetching'));
        release();
        await settleUntil(() => rows().some(tr => tr.dataset.id === 'race-old'));
        check('a live row racing an older page paints at the top',
          rows().map(tr => tr.dataset.id).join() === 'race-live,race-r3,race-r2,race-r1,race-old');
        check('the racing live row is never duplicated under loaded history', rows().length === 5);
      } finally {
        state.logPageGate = null;
        state.logPage = saved.page;
        w.resetLogWindow();
        w.renderAll(saved.data);
        w.scheduleLogFill = saved.fill;
        delete w.__raceData;
      }
    }

    // F12: repeated live arrivals never expand an already-expanded window.
    // Expansion belongs to the scroll/fill owner; a live paint only fills or
    // ages out rows inside the fixed budget.
    {
      const saved = {data: w.eval('lastData'), page: w.eval('dashCfg.log_rows'), fill: w.scheduleLogFill};
      w.scheduleLogFill = () => {};
      const base = 1700006000000;
      const ring = Array.from({length: 120}, (_, i) =>
        mkRec('bound-base' + String(i).padStart(3, '0'), 200, base + i * 1000));
      w.__boundData = {records: ring, counters: {}, _rev: 0};
      w.eval('logArchive = []; logLimit = 120; dashCfg.log_rows = 60; lastData = window.__boundData; ' +
        'renderRequests(derive(lastData).scopedRecs); updateLogStats();');
      let maxRows = 0;
      for (let i = 0; i < 50; i++) {
        ring.push(mkRec('bound-live' + String(i).padStart(2, '0'), 200, base + 120000 + i));
        w.eval('bumpRecords()');
        w.renderRequestsIncremental(ring);
        maxRows = Math.max(maxRows, rows().length);
      }
      check('repeated live arrivals keep the expanded window bounded',
        maxRows === 120 && rows().length === 120 && w.eval('logLimit') === 120 &&
        w.eval('logHTML.size') === 120);
      check('the bounded window keeps the newest live rows at the top',
        rows()[0].dataset.id === 'bound-live49' &&
        rows()[rows().length - 1].dataset.id === 'bound-base050');
      w.resetLogWindow();
      w.eval('dashCfg.log_rows = ' + saved.page + ';');
      w.renderAll(saved.data);
      w.scheduleLogFill = saved.fill;
      delete w.__boundData;
    }

    // F3: the chart payload carries its window/scope identity. A changed key
    // clears the old payload and paints the skeleton before the response; a
    // failed new key shows an honest failure with no old totals.
    {
      const savedFetch = w.fetch;
      let mode = 'ok', value = 300;
      const held = [];
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/chart')) return savedFetch(url, opts);
        if (mode === 'hold') return new Promise((resolve, reject) => held.push({ resolve, reject }));
        if (mode === 'fail') return Promise.resolve({ ok: false, status: 503, json: async () => ({ error: 'chart down' }) });
        return Promise.resolve({ ok: true, json: async () => chartWith(value) });
      };
      try {
        w.eval("chartView.preset = 'traffic'; chartView.hidden = {}; chartView.window = 'all'; chartView.pct = 95; fillChartControls();");
        mode = 'ok'; value = 300;
        w.fetchChart('invalidate');
        await settleUntil(() => w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 300);
        mode = 'hold';
        const win = d.getElementById('chart-window');
        win.value = '1440';
        win.dispatchEvent(new w.Event('change', { bubbles: true }));
        await settleUntil(() => held.length === 1);
        check('a chart window change clears the old payload and paints the skeleton before the response',
          w.eval('chartAgg === null') &&
          !d.getElementById('chart-totals').textContent.includes('300') &&
          d.getElementById('chart-context').textContent === 'Waiting for traffic');
        held.shift().resolve({ ok: true, json: async () => chartWith(1) });
        await settleUntil(() => w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 1);
        check('only the matching window payload paints the new totals',
          w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 1 &&
          d.getElementById('chart-totals').textContent.includes('requests'));
        mode = 'fail';
        await w.fetchChart('refresh');
        check('a same-key chart refresh failure retains the last good payload',
          w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 1 &&
          d.getElementById('chart-totals').textContent.includes('requests 1'));
        w.navigateTo([{ dim: 'provider', id: 'chart-fail.example' }]);
        await settleUntil(() => d.getElementById('chart-context').textContent.includes('chart unavailable'));
        const failTotals = d.getElementById('chart-totals').textContent;
        check('a failed new-scope chart request shows no old-scope totals and a visible failure',
          w.eval('chartAgg === null') && failTotals.includes('chart unavailable') &&
          !failTotals.includes('300') && !/requests\s*1\b/.test(failTotals) &&
          d.getElementById('chart-context').textContent.includes('chart unavailable'));
      } finally {
        mode = 'ok'; value = 0;
        w.eval("chartView.window = 'all'; fillChartControls();");
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('chartAgg'), 3000);
        w.eval("chartView.preset = 'traffic'; chartView.hidden = {}; renderChart();");
        w.fetch = savedFetch;
      }
    }

    // F5: absent explorer counts are unknown, not measured zeros.
    {
      const savedFetch = w.fetch;
      let mode = 'hold';
      const held = [];
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/explorer')) return savedFetch(url, opts);
        if (mode === 'hold') return new Promise((resolve, reject) => held.push({ resolve, reject }));
        if (mode === 'fail') return Promise.resolve({ ok: false, status: 503, json: async () => ({ error: 'down' }) });
        return Promise.resolve({ ok: true, json: async () => ({
          dim: 'provider', total: 0, error_total: 0, rail: { provider: 0 }, groups: [],
          scope: { matches: 0, errors: 0 },
        }) });
      };
      const railValues = () => [...d.querySelectorAll('#xp-rail .xp-rail-item')]
        .map(b => b.querySelector('.rail-n').textContent);
      const triggerValue = () => d.getElementById('xp-dim-trigger').querySelector('.rail-n')?.textContent;
      try {
        w.navigateTo([{ dim: 'provider', id: 'rail-unknown.example' }]);
        await settleUntil(() => held.length > 0);
        check('an unloaded explorer payload renders unknown rail and trigger counts, not fabricated zeros',
          railValues().every(v => v === '-') && triggerValue() === '-' &&
          /loading breakdown/.test(d.getElementById('xp-gallery').textContent));
        mode = 'fail';
        held.shift().resolve({ ok: false, status: 503, json: async () => ({ error: 'down' }) });
        await settleUntil(() => /breakdown unavailable/.test(d.getElementById('xp-gallery').textContent));
        check('a failed explorer payload keeps rail and trigger counts unknown',
          railValues().every(v => v === '-') && triggerValue() === '-');
        mode = 'ok';
        w.fetchExplorer('invalidate');
        await settleUntil(() => w.eval('explorerAgg && explorerAgg.scopeKey === scopeQS()'));
        check('a measured explorer zero renders as a real zero',
          d.querySelector('#xp-rail [data-xp-dim="provider"] .rail-n').textContent === '0' &&
          triggerValue() === '0');
      } finally {
        w.fetch = savedFetch;
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('explorerAgg'), 3000);
      }
    }

    // F1: a purge invalidates same-scope aggregates at once, paints loading,
    // and reports completion only after the forced bootstrap + aggregate
    // resync settle. A failed chart resync keeps the deleted totals gone.
    {
      const savedFetch = w.fetch;
      const savedFull = state.fullPayload;
      const savedConfirm = w.confirm;
      const hold = { chart: [], explorer: [] };
      let phase = 'seed';
      const ghost = { dim: 'provider', total: 40, error_total: 0, rail: { provider: 40 },
        groups: [{ name: 'ghost.example', n: 40, cost: 0, in: 10, out: 5, cache: 2, tools: 0,
          err_final: 0, rate_limit_requests: 0, cost_per_mtok: null,
          ttft_p50: null, ttft_p95: null, tps_p50: null, tps_p95: null }],
        scope: { matches: 40, errors: 0 } };
      const empty = { dim: 'provider', total: 0, error_total: 0, rail: { provider: 0 },
        groups: [], scope: { matches: 0, errors: 0 } };
      w.fetch = (url, opts) => {
        const u = String(url);
        if (u.includes('/admin/purge') && !u.includes('/count')) {
          return Promise.resolve({ ok: true, json: async () => ({ ok: true }) });
        }
        if (u.includes('/metrics/agg/chart')) {
          if (phase === 'hold') return new Promise((resolve, reject) => hold.chart.push({ resolve, reject }));
          return Promise.resolve({ ok: true, json: async () => chartWith(phase === 'seed' ? 40 : 0) });
        }
        if (u.includes('/metrics/agg/explorer')) {
          if (phase === 'hold') return new Promise((resolve, reject) => hold.explorer.push({ resolve, reject }));
          return Promise.resolve({ ok: true, json: async () =>
            JSON.parse(JSON.stringify(phase === 'seed' ? ghost : empty)) });
        }
        return savedFetch(url, opts);
      };
      try {
        w.navigateTo([{ dim: 'provider', id: 'purge-scope.example' }]);
        await settleUntil(() => !!d.querySelector('#xp-gallery [data-xp-key="ghost.example"]') &&
          w.eval('chartAgg && chartAgg.buckets.reduce((a,b) => a + b.req, 0)') === 40);
        const feed = w.eval('feedId');
        state.fullPayload = { feed_id: feed, seq: Math.max(900, w.eval('lastSeq') + 10), incremental: false,
          records: [], counters: { in_flight: 0, total_requests: 0, total_errors: 0 } };
        phase = 'hold';
        w.confirm = () => true;
        let purgeSettled = false;
        const purge = w.purgeAll().then(v => { purgeSettled = true; return v; });
        await settleUntil(() => hold.chart.length === 1 && hold.explorer.length === 1 && rows().length === 0);
        check('purge completion waits for the forced record and aggregate resync',
          purgeSettled === false);
        check('a purge drops same-scope aggregates and paints loading before the resync lands',
          w.eval('explorerAgg === null') && w.eval('chartAgg === null') &&
          /loading breakdown/.test(d.getElementById('xp-gallery').textContent) &&
          [...d.querySelectorAll('#xp-rail .xp-rail-item')].every(b => b.querySelector('.rail-n').textContent === '-') &&
          d.getElementById('xp-dim-trigger').querySelector('.rail-n').textContent === '-' &&
          d.getElementById('f-scope').textContent.startsWith('… of') &&
          !d.getElementById('f-scope').textContent.includes('40 of') &&
          !d.getElementById('chart-totals').textContent.includes('40'));
        hold.chart.shift().reject(new Error('chart down'));
        hold.explorer.shift().resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(empty)) });
        await Promise.race([purge, sleep(2000)]);
        await settleUntil(() => w.eval('explorerAgg !== null') && !w.eval('chartAgg'));
        check('a failed post-purge chart cannot retain deleted totals; the failure is visible',
          w.eval('chartAgg === null') &&
          d.getElementById('chart-totals').textContent.includes('chart unavailable') &&
          d.getElementById('chart-context').textContent.includes('chart unavailable') &&
          !d.getElementById('chart-totals').textContent.includes('40'));
        check('the released post-purge explorer paints the new empty scope',
          w.eval('explorerAgg && explorerAgg.scope.matches === 0') &&
          /no provider values yet/.test(d.getElementById('xp-gallery').textContent) &&
          !d.getElementById('xp-gallery').textContent.includes('ghost') &&
          d.getElementById('f-scope').textContent.startsWith('0 of '));
        check('purge completion is reported only after the forced resync settles',
          d.getElementById('settings-count').textContent === 'all metrics history deleted' &&
          d.getElementById('settings-count').classList.contains('ok'));
        phase = 'ok';
        w.fetchChart('invalidate');
        await settleUntil(() => w.eval('chartAgg && chartAgg.buckets.length > 0'));
        check('the chart recovers after the failed resync with fresh aggregates',
          d.getElementById('chart-totals').textContent.includes('requests 0'));
      } finally {
        phase = 'seed';
        w.confirm = savedConfirm;
        w.fetch = savedFetch;
        savedFull.seq = Math.max(savedFull.seq || 0, w.eval('lastSeq') + 10);
        state.fullPayload = savedFull;
        w.navigateTo([]);
        await w.fetchBootstrap('full');
        await settleUntil(() => rows().length === 6, 3000);
        await sleep(50);
      }
    }

    // F4: an open drawer follows the live finalization of its record without
    // rewriting on unrelated traffic, preserving scroll for the same identity.
    {
      const base = 1700003000000;
      fire('begin', { record: { ...mkRec('audit-drawer', 0, base) }, in_flight: 1 });
      await settleUntil(() => !!d.querySelector('#tbl-requests tr.exp-row[data-id="audit-drawer"]'));
      w.openDrawer('audit-drawer');
      const body = d.getElementById('drawer-body');
      body.scrollTop = 120;
      check('the drawer opens on the live request', /streaming/.test(body.textContent));
      fire('end', { record: { ...mkRec('audit-drawer', 200, base), duration_ms: 900, ttft_ms: 250 }, in_flight: 0 });
      await settleUntil(() => /900ms/.test(d.getElementById('drawer-body').textContent));
      check('an open drawer follows the live finalization',
        !/streaming/.test(d.getElementById('drawer-body').textContent) &&
        /900ms/.test(d.getElementById('drawer-body').textContent));
      check('the live drawer repaint preserves its scroll position', body.scrollTop === 120);
      const html = body.innerHTML;
      fire('record', { ...mkRec('audit-unrelated', 200, base + 2000) }, '900');
      await sleep(40);
      check('an unrelated live event does not rewrite the open drawer',
        body.innerHTML === html && body.scrollTop === 120);
      body.scrollTop = 120;
      w.openDrawer('audit-unrelated');
      check('a drawer identity change resets its scroll position', body.scrollTop === 0);
      w.closeDrawer();
    }

    // F6: row replacement, gallery repaint and legend activation restore
    // keyboard focus to the equivalent control through one shared helper.
    {
      const base = 1700004000000;
      const rowBtn = () => d.querySelector('#tbl-requests tr.exp-row[data-id="audit-focus"] button[data-open-req]');
      fire('begin', { record: { ...mkRec('audit-focus', 0, base) }, in_flight: 1 });
      await settleUntil(() => !!rowBtn());
      rowBtn().focus();
      check('the row control owns focus before the live replacement', d.activeElement === rowBtn());
      fire('end', { record: { ...mkRec('audit-focus', 200, base) }, in_flight: 0 });
      await settleUntil(() => {
        const next = rowBtn();
        return !!next && !next.closest('tr').classList.contains('live-row') && d.activeElement === next;
      });
      check('a live row replacement restores focus to the equivalent open control',
        d.activeElement === rowBtn() && !rowBtn().closest('tr').classList.contains('live-row'));
    }
    {
      const savedFetch = w.fetch;
      let version = 1;
      const ent = n => ({ name: 'focus-card.example', n, cost: 0, in: 10, out: 5, cache: 2, tools: 0,
        err_final: 0, rate_limit_requests: 0, cost_per_mtok: null,
        ttft_p50: null, ttft_p95: null, tps_p50: null, tps_p95: null });
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/explorer')) return savedFetch(url, opts);
        return Promise.resolve({ ok: true, json: async () => ({
          dim: 'provider', total: 3, error_total: 0, rail: { provider: 1 },
          groups: [ent(version)], scope: { matches: version, errors: 0 },
        }) });
      };
      const card = () => d.querySelector('#xp-gallery [data-xp-key="focus-card.example"]');
      try {
        w.navigateTo([{ dim: 'provider', id: 'focus-gallery.example' }]);
        await settleUntil(() => !!card() && /1req/.test(card().textContent));
        card().focus();
        version = 2;
        w.fetchExplorer('refresh');
        await settleUntil(() => !!card() && /2req/.test(card().textContent) && d.activeElement === card());
        check('a gallery repaint restores focus to the equivalent card', d.activeElement === card());
      } finally {
        w.fetch = savedFetch;
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('explorerAgg'), 3000);
      }
    }
    // F6 navigation: activating a gallery card changes scope, and the loading
    // repaint destroys that card before the matching payload exists. The focus
    // identity must survive both repaints; a card that vanishes from the
    // filtered payload must not hand focus to some other card.
    {
      const savedFetch = w.fetch;
      const ent = (name, n) => ({ name, n, cost: 0, in: 10, out: 5, cache: 2, tools: 0,
        err_final: 0, rate_limit_requests: 0, cost_per_mtok: null,
        ttft_p50: null, ttft_p95: null, tps_p50: null, tps_p95: null });
      const payload = groups => ({ dim: 'provider', total: 2, error_total: 0,
        rail: { provider: groups.length }, groups, scope: { matches: 2, errors: 0 } });
      let hold = false;
      const held = [];
      w.fetch = (url, opts) => {
        const u = String(url);
        if (!u.includes('/metrics/agg/explorer')) return savedFetch(url, opts);
        if (!hold) return Promise.resolve({ ok: true, json: async () => payload([ent('focus-nav.example', 2)]) });
        return new Promise(resolve => held.push({ resolve }));
      };
      const card = key => d.querySelector(`#xp-gallery [data-xp-key="${key}"]`);
      const prime = async () => {
        hold = false;
        w.navigateTo([{ dim: 'client', id: 'focus-nav-client' }]);
        // Wait for the hashchange's loading repaint and payload, not the old
        // scenario's card: the unfiltered card is the one without `selected`.
        await settleUntil(() => !!card('focus-nav.example') &&
          !card('focus-nav.example').classList.contains('selected'));
      };
      // A browser maps Enter on the focused button to the same click event the
      // delegated explorer handler consumes; jsdom has no implicit activation,
      // so the production path is invoked through click() from keyboard focus.
      const activate = () => {
        card('focus-nav.example').focus();
        hold = true;
        card('focus-nav.example').click();
        return settleUntil(() => held.length === 1 &&
          /loading breakdown/.test(d.getElementById('xp-gallery').textContent) &&
          !card('focus-nav.example'));
      };
      try {
        await prime();
        check('a gallery card activation paints the loading state and drops the old card',
          await activate() && d.activeElement === d.body);
        held.shift().resolve({ ok: true, json: async () => payload([ent('focus-nav.example', 3)]) });
        await settleUntil(() => d.activeElement === card('focus-nav.example'));
        check('the filtered payload restores focus to the equivalent gallery card',
          d.activeElement === card('focus-nav.example') && d.activeElement !== d.body);

        await prime();
        check('the second card activation paints the loading state again',
          await activate() && d.activeElement === d.body);
        held.shift().resolve({ ok: true, json: async () => payload([ent('focus-nav-other.example', 2)]) });
        await settleUntil(() => !!card('focus-nav-other.example') && !card('focus-nav.example'));
        check('a vanished gallery card does not move focus to a different card',
          !!card('focus-nav-other.example') && d.activeElement === d.body);
      } finally {
        hold = false;
        w.fetch = savedFetch;
        w.navigateTo([]);
        await settleUntil(() => !!w.eval('explorerAgg'), 3000);
      }
    }
    {
      w.eval("chartView.preset = 'traffic'; chartView.hidden = {}; chartView.pct = 95; chartView.window = 'all'; fillChartControls(); renderChart();");
      const leg = () => d.querySelector('#traffic-legend .leg-item[data-series="err"]');
      const before = leg();
      before.focus();
      before.click();
      const after = leg();
      check('a legend activation restores focus to the equivalent toggle',
        before !== after && after.classList.contains('off') && d.activeElement === after);
      after.click();
      await sleep(0);
    }
    {
      const legend = d.getElementById('traffic-legend');
      const item = legend.querySelector('.leg-item');
      item.focus();
      const token = typeof w.captureFocus === 'function' ? w.captureFocus(legend, el => {
        const series = el.closest('.leg-item[data-series]')?.dataset.series;
        return series == null ? null : `[data-series="${w.CSS.escape(series)}"]`;
      }) : null;
      const other = d.getElementById('btn-pause');
      other.focus();
      const restored = typeof w.restoreFocus === 'function' ? w.restoreFocus(token) : false;
      check('the shared focus restore never steals focus from a moved operator',
        !!token && restored === false && d.activeElement === other);
    }

    // F13: owner cleanup and stable focus identities. A competing prompt must
    // not leave the Settings veil/pressed state behind; removing a scope chip
    // must land on its surviving neighbor/root; a drawer whose initiating row
    // was replaced must return focus to the equivalent control.
    {
      const sheet = d.getElementById('settings-sheet');
      const veil = d.getElementById('settings-veil');
      const settingsBtn = d.getElementById('btn-settings');
      if (w.settingsIsOpen()) w.closeSettings(true);
      w.openSettings();
      await settleUntil(() => w.settingsIsOpen() && sheet.classList.contains('open'));
      await sleep(20);
      const prompt = w.askOperatorToken();
      await sleep(0);
      check('an operator prompt preempts open Settings without a stale veil or pressed trigger',
        !w.settingsIsOpen() && !sheet.classList.contains('open') && !veil.classList.contains('open') &&
        settingsBtn.getAttribute('aria-pressed') === 'false' &&
        !d.getElementById('operator-dialog').hidden);
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      await prompt;
      check('the Settings takeover cleanup survives prompt cancel',
        !sheet.classList.contains('open') && !veil.classList.contains('open') &&
        settingsBtn.getAttribute('aria-pressed') === 'false' &&
        d.getElementById('operator-dialog').hidden && !d.querySelector('main').hasAttribute('inert'));
    }
    {
      w.navigateTo([{dim: 'provider', id: 'chip-focus.example'}, {dim: 'client', id: 'c'}]);
      const chips = () => [...d.querySelectorAll('#xp-scope .chip-x')];
      await settleUntil(() => chips().length === 2);
      const first = chips()[0];
      first.focus();
      first.click();
      const survivor = await settleUntil(() => chips().length === 1 && d.activeElement === chips()[0]);
      check('removing a scope chip focuses the surviving neighbor',
        survivor && chips()[0].dataset.chipDim === 'client');
      const last = chips()[0];
      last.focus();
      last.click();
      const rootFocused = await settleUntil(() => d.activeElement === d.querySelector('#xp-scope .bc-root'));
      check('removing the last scope chip falls back to the all-traffic root',
        rootFocused && chips().length === 0);
      w.navigateTo([]);
      await settleUntil(() => w.eval('explorerFilters().length') === 0);
    }
    {
      const base = 1700007000000;
      const rowBtn = () => d.querySelector('#tbl-requests tr.exp-row[data-id="focus-drawer"] button[data-open-req]');
      fire('begin', { record: { ...mkRec('focus-drawer', 0, base) }, in_flight: 1 });
      await settleUntil(() => !!rowBtn());
      rowBtn().focus();
      rowBtn().click();
      await settleUntil(() => !d.getElementById('drawer').hidden);
      const oldBtn = rowBtn();
      fire('end', { record: { ...mkRec('focus-drawer', 200, base), duration_ms: 100 }, in_flight: 0 });
      await settleUntil(() => {
        const next = rowBtn();
        return !!next && next !== oldBtn && !next.closest('tr').classList.contains('live-row');
      });
      check('the drawer initiating control was replaced while the drawer stayed open',
        !oldBtn.isConnected && !!rowBtn());
      const newBtn = rowBtn();
      w.closeDrawer();
      check('closing the drawer after row replacement restores the equivalent control',
        d.activeElement === newBtn);
    }

    // F9: a page opened in a hidden tab must not arm the clock or poll before
    // its first visible transition; that transition runs the resume path.
    {
      let hidden = true;
      let hiddenBoot = 0, hiddenChart = 0, hiddenExplorer = 0;
      const hiddenDom = dashboardDOM(assembleHTML(undefined, { includeSettings: false }), {
        url: 'http://127.0.0.1:8081/',
        runScripts: 'dangerously',
        pretendToBeVisual: true,
        beforeParse(window) {
          Object.defineProperty(window.document, 'hidden', { configurable: true, get: () => hidden });
          window.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
          window.matchMedia = () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} });
          window.CSS = window.CSS || {};
          if (!window.CSS.escape) window.CSS.escape = s => String(s).replace(/[\\"]/g, '\\$&');
          Object.defineProperty(window.HTMLElement.prototype, 'offsetHeight', { get() { return 20; }, configurable: true });
          window.Path2D = class { moveTo() {} lineTo() {} arcTo() {} arc() {} rect() {} closePath() {} bezierCurveTo() {} };
          window.HTMLCanvasElement.prototype.getContext = () => new Proxy({}, {
            get: (t, k) => (k === 'measureText' ? () => ({ width: 1 }) :
              k === 'canvas' ? { width: 0, height: 0 } :
              (typeof k === 'string' ? () => {} : undefined)),
            set: () => true,
          });
          window.EventSource = function (url) {
            return { url, readyState: 1, addEventListener() {}, close() { this.readyState = 2; } };
          };
          window.fetch = url => {
            const u = String(url);
            if (u.includes('/metrics/bootstrap')) {
              hiddenBoot++;
              return Promise.resolve({ ok: true, json: async () => ({
                records: [], in_flight_records: [], counters: { in_flight: 0 },
                pending_revision: 0, seq: 0, feed_id: 'hidden-feed', incremental: false,
                kpi: { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null,
                  input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0,
                  avg_ttft_ms: null, avg_tps: null },
                dashboard_version: TEST_DASHBOARD_VERSION,
                model_canon: { rules: [], names: {} },
                dash: {}, storage: { enabled: true, dropped: 0, totals_degraded: false },
                pause: { ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: [], known_providers: [], until: null, queued: 0, default_max_queued: 0 },
                throttle: { ok: true, throttles: [], known_providers: [], active: false },
                debug: { ok: true, enabled: false, sessions: [], known_clients: [], known_providers: [], known_models: [], until: null, ttl: '24h', max_bytes: '1MiB' },
              }) });
            }
            if (u.includes('/metrics/agg/chart')) {
              hiddenChart++;
              return Promise.resolve({ ok: true, json: async () => ({
                now_ms: Date.now(), from_ms: Date.now() - 3600000, bucket_ms: 60000,
                ttft_p: [null, null, null], tps_p: [null, null, null], dec_p: [null, null, null],
                buckets: [],
              }) });
            }
            if (u.includes('/metrics/agg/explorer')) {
              hiddenExplorer++;
              return Promise.resolve({ ok: true, json: async () => ({
                dim: 'provider', total: 0, error_total: 0, rail: { provider: 0 }, groups: [],
                scope: { matches: 0, errors: 0 },
              }) });
            }
            if (u.includes('/metrics/agg/log')) return Promise.resolve({ ok: true, json: async () => ({ records: [], more: false, cursor_ms: 0, cursor_id: '' }) });
            return Promise.resolve({ ok: true, json: async () => ({}) });
          };
        },
      });
      await loaded(hiddenDom);
      const hw = hiddenDom.window;
      const hd = hw.document;
      await sleep(300);
      check('a page loaded hidden keeps its clock and poll timers unarmed',
        hw.eval('_wasHidden === true && _clockTimer === null && _dashTickTimer === null'));
      check('a hidden-at-load page completes only its initial paint fetches',
        hiddenBoot === 1 && hiddenChart === 1 && hiddenExplorer === 1);
      const bootBefore = hiddenBoot;
      hidden = false;
      hd.dispatchEvent(new hw.Event('visibilitychange'));
      await sleep(80);
      check('the visible transition arms both timers and refreshes the page',
        hw.eval('_wasHidden === false && _clockTimer !== null && _dashTickTimer !== null') &&
        hiddenBoot > bootBefore);
      hiddenDom.window.close();
    }
  }


  reportFailures(false);
  process.exit(state.failures.length ? 1 : 0);
}
// A crashed run must not swallow what it already knows: without this
// handler the process dies on the raw exception and the accumulated
// failures, the diagnostic dump and the check history never print.
main().catch(err => {
  console.error(err);
  if (state.diagDump) console.log(state.diagDump('crash: ' + (err && err.message)));
  reportFailures(true);
  process.exit(1);
});
