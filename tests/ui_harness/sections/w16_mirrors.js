// W16 mirror pins: the Go<->JS pairs the mutation rounds proved unguarded
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function w16Mirrors(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, dashboardDOM, loaded, mkRec, modelFixture, observerFixture, pageOptions, path, rows, settleUntil} = H;
  const {fs, STATIC} = H;
  const state = H.state;

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
      await settleUntil(() => sw.eval('!!lastData'));

      // recordIsError mirrors metrics.Record.IsError. The rows mirror the
      // semantics of tests/_go/internal/metrics/iserror_corpus.go (the Go
      // suite owns the corpus, and the lockstep pin in
      // tests/_go/internal/metrics/iserror_lockstep.go checks names, order and
      // count); the JS predicate must agree on every row.
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
        ['final 499 with in-band error after an absorbed 429', {status_code: 499, error_type: 'api_error', error_code: 'internal_error', attempts: [{status_code: 429}]}, true],
        ['committed 200 with an in-band 429', {status_code: 200, error_type: 'rate_limit_error', attempts: [{status_code: 429, in_band: true}]}, true],
        ['400 carrying a rate-limit code', {status_code: 400, error_type: 'tier_limit_exceeded', error_code: 'tier_limit_exceeded'}, true],
        ['400 carrying a durable quota code', {status_code: 400, error_type: 'insufficient_quota'}, true],
        ['400 client-fault type with a transient code', {status_code: 400, error_type: 'invalid_request_error', error_code: 'internal_error'}, true],
        ['synthetic in-band 500 counts as an absorbed failure', {status_code: 200, attempts: [{status_code: 500, in_band: true}]}, true],
      ];
      const errorMisses = corpus.filter(([, rec, want]) => sw.recordIsError(rec) !== want).map(([name]) => name);
      check('recordIsError agrees with the 30-row Go IsError corpus' + (errorMisses.length ? ' (missed: ' + errorMisses.join(', ') + ')' : ''),
        errorMisses.length === 0);

      // The built-in vocabulary mirror can be replaced at runtime by the
      // effective vocabulary the server publishes with the bootstrap payload
      // (rate_limit_classes, installed by setRateLimitVocabulary), so an
      // operator's rate_limit_error_classes extension classifies identically
      // in the browser and on the server. Passing no list restores the
      // built-ins.
      check('a published rate-limit vocabulary replaces the built-in mirror, and is restorable',
        (() => {
          const before = sw.recordHasRateLimit({status_code: 400, error_type: 'gateway_throttle'});
          sw.setRateLimitVocabulary(['gateway_throttle']);
          const extended = sw.recordHasRateLimit({status_code: 400, error_type: 'gateway_throttle'});
          const unrelated = sw.recordHasRateLimit({status_code: 200, error_type: 'ordinary_error'});
          sw.setRateLimitVocabulary(null);
          const after = sw.recordHasRateLimit({status_code: 400, error_type: 'gateway_throttle'});
          return before === false && extended === true && unrelated === false && after === false;
        })());

      // statusClass mirrors contribStatusClass/statusClass in aggregate_status.go on
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

      // The live pill vocabulary mirrors liveStatusFilter (aggregate_scope.go): the
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
      // plain 499 final produce no entry, a 499 carrying a structured error
      // does (the client aborted around a real upstream failure), an
      // in-band provider error on a 200 does, and
      // absorbed attempts keep genuine failures (5xx, typed, 4xx, and a
      // rate-limit code, which is a failure with a rate-limit cause) while
      // skipping a bare 429, bare non-failures, and a transport retry
      // (retry mechanics, not an upstream error event - the Go enumerator
      // skips error_type 'transport' the same way). In-band attempt entries
      // are health evidence, never a second disclosure for the final failure,
      // including the synthetic in-band 5xx shape. errorKey mirrors the Go
      // type|code|msg identity the explorer error dimension filters by.
      const entsRec = {
        status_code: 200, error_code: '', error_msg: '',
        attempts: [
          {status_code: 429},
          {status_code: 502, error_type: 'upstream'},
          {status_code: 0, error_type: 'dial_failed'},
          {status_code: 0, error_type: 'transport'},
          {status_code: 401},
          {status_code: 302},
          {status_code: 400, error_type: 'tier_limit_exceeded', error_code: 'tier_limit_exceeded'},
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
          ['tier_limit_exceeded', 'tier_limit_exceeded', true, '2026-09-14T00:00:00Z'],
        ]));
      check('final 429 and a plain 499 produce no error entry (flow events)',
        sw.recordErrorEntries({status_code: 429, error_type: 'rate_limit_error'}).length === 0 &&
        sw.recordErrorEntries({status_code: 499}).length === 0);
      check('a rate limit announced by a code on a 400 is a failure entry and a rate-limit event',
        sw.recordErrorEntries({status_code: 400, error_type: 'tier_limit_exceeded', error_code: 'tier_limit_exceeded'}).length === 1 &&
        sw.recordIsError({status_code: 400, error_type: 'tier_limit_exceeded', error_code: 'tier_limit_exceeded'}) === true &&
        sw.recordHasRateLimit({status_code: 400, error_type: 'tier_limit_exceeded', error_code: 'tier_limit_exceeded'}) === true);
      check('a 499 carrying the provider in-band error produces an entry like its 200 twin',
        JSON.stringify(sw.recordErrorEntries({status_code: 499, error_type: 'api_error', error_code: 'internal_error', error_msg: 'temporarily unavailable'}).map(e => [e.type, e.code, e.msg])) ===
        JSON.stringify([['api_error', 'internal_error', 'temporarily unavailable']]));
      check('the 499 final predicate reads its own type, not the attempt list',
        JSON.stringify(sw.recordErrorEntries({status_code: 499, error_type: 'api_error', error_code: 'internal_error', attempts: [{status_code: 429}]}).map(e => [e.type, e.code, e.absorbed])) ===
        JSON.stringify([['api_error', 'internal_error', false]]));
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
      check('errorKey escapes the separator so distinct errors never collide',
        sw.errorKey('a', 'b|c', 'd') === 'a|b\\|c|d' &&
        sw.errorKey('a|b', 'c', 'd') === 'a\\|b|c|d' &&
        sw.errorKey('a', 'b|c', 'd') !== sw.errorKey('a|b', 'c', 'd') &&
        sw.errorKey('a', 'b\\c', 'd') === 'a|b\\\\c|d' &&
        sw.errorKeyMatches('a|b\\|c|d', 'a', 'b|c', 'd') &&
        !sw.errorKeyMatches('a|b\\|c|d', 'a|b', 'c', 'd'));
      check('escaped error keys decode for display and chips',
        sw.errorKeyType(sw.errorKey('a|b', 'c', 'd')) === 'a|b' &&
        sw.errorKeyDisplay(sw.errorKey('a', 'b', 'd')) === 'a|b|d' &&
        sw.errorKeyDisplay(sw.errorKey('a', 'b|c', 'd')) === 'a|b\\|c|d' &&
        sw.errorKeyDisplay(sw.errorKey('a', 'b|c', 'd')) !==
          sw.errorKeyDisplay(sw.errorKey('a|b', 'c', 'd')) &&
        // Any invented quoting can collide with ordinary text; the escaped
        // key is the only injective display for a special part.
        sw.errorKeyDisplay(sw.errorKey('a', 'b|c', 'd')) !==
          sw.errorKeyDisplay(sw.errorKey('"a" · "b', 'c" · "d', 'e"')) &&
        sw.shortId('error', sw.errorKey('a|b', 'c', 'd')) === 'a|b');

      // The duration vocabularies mirror Go acceptance: PAUSE_DURS tokens are
      // the pauseDurations allowlist (pinned through the real pause/debug
      // handlers Go-side), LIMIT_WINDOWS tokens pass parseLimitWindow, and
      // FILTER_AGES tokens are canonical config.FormatDuration spellings.
      // Labels ride the same pin so the '15 min' vs '15 minutes' drift class
      // cannot return. There is one operator vocabulary now: the console's
      // capture composer reuses PAUSE_DURS and its ramp picker derives from
      // LIMIT_WINDOWS, so DEBUG_DURS and LIMIT_RAMPS must not come back.
      check('PAUSE_DURS offers exactly the server-accepted hold durations',
        JSON.stringify(sw.eval('PAUSE_DURS')) === JSON.stringify([
          ['', 'until resumed'], ['15m', '15 minutes'], ['1h', '1 hour'],
          ['6h', '6 hours'], ['12h', '12 hours'], ['24h', '24 hours']]));
      check('DEBUG_DURS is gone: the capture composer reuses PAUSE_DURS',
        sw.eval('typeof DEBUG_DURS') === 'undefined');
      check('LIMIT_WINDOWS offers exactly the parseLimitWindow-accepted tokens',
        JSON.stringify(sw.eval('LIMIT_WINDOWS')) === JSON.stringify([
          ['1s', '1 second'], ['10s', '10 seconds'], ['30s', '30 seconds'],
          ['1m', '1 minute'], ['5m', '5 minutes'], ['15m', '15 minutes'],
          ['1h', '1 hour'], ['6h', '6 hours'], ['24h', '24 hours']]));
      check('LIMIT_RAMPS is gone: the ramp picker derives from LIMIT_WINDOWS',
        sw.eval('typeof LIMIT_RAMPS') === 'undefined');
      {
        // The ramp picker lives with the limit composer; read the console's
        // modules as one source set so a further split cannot silently drop
        // the derivation assertion.
        const dir = path.join(STATIC, 'js');
        const consoleSources = fs.readdirSync(dir)
          .filter(name => /^operator_.*\.js$/.test(name))
          .map(name => fs.readFileSync(path.join(dir, name), 'utf8'));
        // The window pickers are one owner: ocWindowSelect derives both the
        // request/token windows and the ramp from LIMIT_WINDOWS, so a caller
        // cannot re-derive a divergent token list.
        {
        // One reading of ThrottleInfo.scheduled. It was tested twice with
        // opposite polarity - `scheduled !== false` in the readout and
        // `scheduled === false` in both row builders - so a server answering
        // null would have flipped one surface and not the other.
        const dir = path.join(STATIC, 'js');
        const consoleSources = fs.readdirSync(dir)
          .filter(name => /^operator_.*\\.js$/.test(name))
          .map(name => fs.readFileSync(path.join(dir, name), 'utf8'));
        check('the off-schedule test has one owner instead of two polarities',
          !consoleSources.some(src => src.includes('policy.scheduled === false') ||
            src.includes('policy.scheduled !== false') ||
            src.includes("'off schedule'")));
        check('the enforcing test and its complement can never disagree',
          sw.ocLimitEnforcing({scheduled: true}) === true &&
          sw.ocLimitOffSchedule({scheduled: true}) === false &&
          sw.ocLimitEnforcing({scheduled: false}) === false &&
          sw.ocLimitOffSchedule({scheduled: false}) === true &&
          sw.ocLimitEnforcing(null) === false && sw.ocLimitOffSchedule(null) === true);
      }
      {
        // One budget sentence. The row, the overview and the ledger print
        // ocBudgetPhrase, and the composer preview now reads the same clause
        // list: it used to build a second sentence with its own plural rule
        // and its own window wording, so one draft printed "1 requests per
        // 1 minute" above a row that said "1 request / 1m".
        const dir = path.join(STATIC, 'js');
        const consoleSources = fs.readdirSync(dir)
          .filter(name => /^operator_.*\\.js$/.test(name))
          .map(name => fs.readFileSync(path.join(dir, name), 'utf8'));
        check('the composer preview does not spell budgets a second way',
          !consoleSources.some(src => src.includes("' requests per '") ||
            src.includes("' requests in flight'") ||
            src.includes("' tokens per '")));
        const phrase = sw.ocBudgetPhrase;
        check('one budget sentence names a single request in the singular',
          phrase({concurrency: 1, requests: 1, requestWindow: '1m', tokens: 1, tokenWindow: '1h'}) ===
            '1 in flight · 1 request / 1m · 1 token / 1h');
        check('the same budget sentence serves a stored policy and a draft',
          phrase({concurrency: 2, requests: 10, request_window: '1m', tokens: 1000, token_window: '1h'}) ===
            phrase({concurrency: 2, requests: 10, requestWindow: '1m', tokens: 1000, tokenWindow: '1h'}) &&
          phrase({concurrency: 2, requests: 10, request_window: '1m', tokens: 1000, token_window: '1h'}) ===
            '2 in flight · 10 requests / 1m · 1K tokens / 1h');
        check('a limit with no caps is named once, not an empty string',
          phrase({concurrency: 0, requests: 0, tokens: 0}) === 'no caps');
      }
      check('the window pickers derive their tokens from LIMIT_WINDOWS once',
          consoleSources.some(src => src.includes('const values = (withOff ? [\'\'] : []).concat(LIMIT_WINDOWS.map(([v]) => v))')) &&
          !consoleSources.some(src => src.includes("ocChipRow('ramp'")));
        check('both console duration pickers read the one PAUSE_DURS vocabulary',
          consoleSources.reduce((n, src) =>
            n + (src.match(/\[''\]\.concat\(PAUSE_DURS\.slice\(1\)\.map\(\(\[v\]\) => v\)\)/g) || []).length, 0) === 2);
      }
      check('FILTER_AGES offers canonical Go duration spellings',
        JSON.stringify(sw.eval('FILTER_AGES')) === JSON.stringify([
          ['', 'any'], ['1h', '1 hour'], ['24h', '1 day'], ['168h', '1 week']]));
      {
        // The composers are the only pickers now. Each renders the shared
        // closed vocabulary as pressed chips; the pause and capture sets are
        // PAUSE_DURS minus its empty "resume" pair behind their own empty
        // token, and the limit request window is LIMIT_WINDOWS exactly.
        const chipValues = name => [...sd.querySelectorAll('#oc-pane [data-oc-choice]')]
          .filter(chip => chip.dataset.ocChoice === name)
          .map(chip => chip.dataset.ocValue);
        // A long single-choice list is a native select; its options are the
        // same vocabulary, so read either shape through one accessor.
        const choiceValues = name => {
          const el = sd.querySelector(`#oc-pane [data-oc-choice="${name}"]`);
          if (!el) return [];
          return el.tagName === 'SELECT'
            ? [...el.options].map(option => option.value) : chipValues(name);
        };
        sw.applyPauseState({ok:true,paused:false,holds:[],clients:[],providers:[],
          known_clients:[],known_providers:[],until:null,queued:0,default_max_queued:0});
        sw.openOperatorConsole('pause');
        sw.ocOpenPauseComposer('');
        check('the pause composer renders exactly the PAUSE_DURS durations behind its empty token',
          JSON.stringify(chipValues('pause-duration')) ===
          JSON.stringify([''].concat(sw.eval('PAUSE_DURS').slice(1).map(([v]) => v))));
        sw.applyDebugState(observerFixture({ok:true,enabled:false,sessions:[],
          known_clients:[],known_providers:[],known_models:[],model_canon:modelFixture()}));
        sw.openOperatorConsole('capture');
        sw.ocOpenCaptureComposer('');
        check('the capture composer reuses the pause duration set',
          JSON.stringify(chipValues('capture-duration')) ===
          JSON.stringify([''].concat(sw.eval('PAUSE_DURS').slice(1).map(([v]) => v))));
        sw.openOperatorConsole('limits');
        sw.ocOpenLimitComposer('');
        check('the limit composer renders exactly the LIMIT_WINDOWS request windows',
          JSON.stringify(choiceValues('request-window')) ===
          JSON.stringify(sw.eval('LIMIT_WINDOWS').map(([v]) => v)) &&
          JSON.stringify(choiceValues('token-window')) ===
          JSON.stringify(sw.eval('LIMIT_WINDOWS').map(([v]) => v)));
        sd.querySelector('#oc-pane [data-oc-toggle="advanced"]').click();
        check('the ramp picker is the LIMIT_WINDOWS windows behind the empty off token',
          JSON.stringify(choiceValues('ramp')) ===
          JSON.stringify([''].concat(sw.eval('LIMIT_WINDOWS').map(([v]) => v))) &&
          sd.querySelector('#oc-pane [data-oc-choice="ramp"] option[value=""]').textContent === 'off');
        sw.closeOperatorConsole();
        sw.eval("ocDrafts.limit = null; ocDrafts.pause = null; ocDrafts.capture = null; ocView = {kind: '', id: ''}; ocError = ''");
        sw.applyPauseState({ok:true,paused:false,holds:[],clients:[],providers:[],
          known_clients:['c'],known_providers:['p'],until:null,queued:0,default_max_queued:0});
        sw.applyDebugState(observerFixture({ok:true,enabled:false,sessions:[],
          known_clients:['c'],known_providers:['p'],known_models:['m'],model_canon:modelFixture()}));
        sw.applyThrottleState({ok:true,active:false,throttles:[],known_providers:['p']});
      }

      // ocHoldMatchesRecord mirrors scheduler holdSnap.matches on the scope
      // half (scheduler/hold.go); chrome_settings.js holdLive owns the expiry
      // half the Go matcher folds in. The matrix mirrors the Go suite's
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
        .filter(([name, h, r, want]) => sw.ocHoldMatchesRecord(h, r) !== want)
        .map(([name]) => name);
      check('the console hold matcher agrees with the Go matcher matrix' +
        (holdMisses.length ? ' (missed: ' + holdMisses.join(', ') + ')' : ''),
        holdMisses.length === 0);
      check('a missing record or hold never matches',
        sw.ocHoldMatchesRecord(null, {client: 'c', provider: 'p'}) === false &&
        sw.ocHoldMatchesRecord({all: true}, null) === false);
      check('holdLive owns the expiry half the Go matcher folds in',
        sw.eval(`holdLive({all: true, until: new Date(Date.now() - 60000).toISOString()})`) === false &&
        sw.eval(`holdLive({all: true, until: new Date(Date.now() + 60000).toISOString()})`) === true);
      check('liveHolds and pauseActive read the same expiry rule through applyPauseState',
        (() => {
          const expired = new Date(Date.now() - 60000).toISOString();
          const future = new Date(Date.now() + 60000).toISOString();
          sw.applyPauseState({ok: true, paused: true, clients: [], providers: [],
            holds: [{id: 'live', all: true, until: future}, {id: 'gone', all: true, until: expired}],
            known_clients: [], known_providers: [], until: null, queued: 0, default_max_queued: 0});
          const live = sw.liveHolds().map(h => h.id);
          const active = sw.pauseActive();
          const footer = sd.getElementById('f-state').textContent;
          // The footer names a hold's blast radius through the shared reader,
          // so the expectation is the owner's own sentence rather than a
          // literal. A model-scoped hold used to print only the word "hold"
          // here because the footer assembled the scope itself.
          const ownerPhrase = sw.ocHoldScopePhrase({all: true});
          sw.applyPauseState({ok: true, paused: true, clients: [], providers: [],
            holds: [{id: 'model', models: ['glm-5.3'], until: future}],
            known_clients: [], known_providers: [], until: null, queued: 0, default_max_queued: 0});
          const modelFooter = sd.getElementById('f-state').textContent;
          sw.applyPauseState({ok: true, paused: false, clients: [], providers: [], holds: [],
            known_clients: ['c'], known_providers: ['p'], until: null, queued: 0, default_max_queued: 0});
          return JSON.stringify(live) === JSON.stringify(['live']) && active === true &&
            footer.includes('paused · ' + ownerPhrase) &&
            modelFooter.includes('paused · model glm-5.3');
        })());

      // ocCaptureMatchesRecord is the capture session's scope half: an empty
      // dimension matches anything, and a session with no scope at all can
      // never claim a request.
      const captureRows = [
        ['client match', {clients: ['client-a']}, {client: 'client-a', provider: 'p', model: 'm'}, true],
        ['client miss', {clients: ['client-a']}, {client: 'client-b', provider: 'p', model: 'm'}, false],
        ['provider match', {providers: ['alpha.example']}, {client: 'c', provider: 'alpha.example', model: 'm'}, true],
        ['provider miss', {providers: ['alpha.example']}, {client: 'c', provider: 'beta.example', model: 'm'}, false],
        ['model match', {models: ['glm-5.3']}, {client: 'c', provider: 'p', model: 'glm-5.3'}, true],
        ['model miss', {models: ['glm-5.3']}, {client: 'c', provider: 'p', model: 'grok-4.6'}, false],
        ['and-matched pair', {clients: ['client-a'], models: ['glm-5.3']}, {client: 'client-a', provider: 'p', model: 'glm-5.3'}, true],
        ['and-matched pair misses on model', {clients: ['client-a'], models: ['glm-5.3']}, {client: 'client-a', provider: 'p', model: 'grok-4.6'}, false],
        ['empty client matches anything', {providers: ['alpha.example'], clients: []}, {client: 'whoever', provider: 'alpha.example', model: 'm'}, true],
      ];
      const captureMisses = captureRows
        .filter(([name, session, record, want]) => sw.ocCaptureMatchesRecord(session, record) !== want)
        .map(([name]) => name);
      check('the capture matcher agrees with its scope matrix' +
        (captureMisses.length ? ' (missed: ' + captureMisses.join(', ') + ')' : ''),
        captureMisses.length === 0);
      check('an empty capture scope claims no request (deny by default)',
        sw.ocCaptureMatchesRecord({clients: [], providers: [], models: []}, {client: 'c', provider: 'p', model: 'm'}) === false &&
        sw.ocCaptureMatchesRecord({}, {client: 'c', provider: 'p', model: 'm'}) === false &&
        sw.ocCaptureMatchesRecord({clients: ['c']}, null) === false);

      // ocPolicyMatchesRecord is the limit policy's scope half: every stored
      // dimension AND-matches, and an empty policy matches every request.
      const policyRows = [
        ['provider match', {scope: {providers: ['p']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
        ['provider miss', {scope: {providers: ['p']}}, {provider: 'q', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, false],
        ['model match', {scope: {models: ['m']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
        ['model miss', {scope: {models: ['m']}}, {provider: 'p', model: 'other', client: 'c', key_hash: 'k', conversation_id: 'cv'}, false],
        ['client match', {scope: {clients: ['c']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
        ['client miss', {scope: {clients: ['c']}}, {provider: 'p', model: 'm', client: 'other', key_hash: 'k', conversation_id: 'cv'}, false],
        ['key hash match', {scope: {key_hashes: ['k']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
        ['key hash miss', {scope: {key_hashes: ['k']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'other', conversation_id: 'cv'}, false],
        ['conversation match', {scope: {conversations: ['cv']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
        ['conversation miss', {scope: {conversations: ['cv']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'other'}, false],
        ['pair misses on the second dimension', {scope: {providers: ['p'], models: ['m']}}, {provider: 'p', model: 'other', client: 'c', key_hash: 'k', conversation_id: 'cv'}, false],
        ['pair matches both dimensions', {scope: {providers: ['p'], models: ['m']}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
        ['empty stored dimension matches any value', {scope: {providers: [], models: ['m']}}, {provider: 'anything', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}, true],
      ];
      const policyMisses = policyRows
        .filter(([name, policy, record, want]) => sw.ocPolicyMatchesRecord(policy, record) !== want)
        .map(([name]) => name);
      check('the policy matcher agrees with its scope matrix' +
        (policyMisses.length ? ' (missed: ' + policyMisses.join(', ') + ')' : ''),
        policyMisses.length === 0);
      check('an unscoped policy matches every request',
        sw.ocPolicyMatchesRecord({scope: {}}, {provider: 'p', model: 'm', client: 'c', key_hash: 'k', conversation_id: 'cv'}) === true &&
        sw.ocPolicyMatchesRecord({}, {provider: 'p'}) === true);

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
      // The Apply gate trusts the strict parser, so it must accept exactly the
      // grammar time.ParseDuration accepts (including the Settings-only typed
      // `0`) and reject trailing or mid-string garbage.
      const strictDurRows = [
        ['0', 0], ['0s', 0], ['+1h', 3600000], ['.5h', 1800000],
        ['1.h', 3600000], ['1h.5m', 3630000], ['-1.5h', -5400000],
      ];
      const strictMisses = strictDurRows
        .filter(([spelling, want]) => sw.eval(`parseGoDurationValue(${JSON.stringify(spelling)})`) !== want)
        .map(([spelling]) => spelling);
      check('parseGoDurationValue accepts the Go duration grammar' +
        (strictMisses.length ? ' (missed: ' + strictMisses.join(', ') + ')' : ''),
        strictMisses.length === 0);
      const badDurRows = ['1h garbage', '1h-30m', '.s', '1', '1H', '1e3s', '--1h', '3000000h'];
      const acceptedBad = badDurRows
        .filter(spelling => sw.eval(`parseGoDurationValue(${JSON.stringify(spelling)})`) !== null);
      check('parseGoDurationValue rejects malformed durations' +
        (acceptedBad.length ? ' (accepted: ' + acceptedBad.join(', ') + ')' : ''),
        acceptedBad.length === 0);

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

      // The restart deadline comes from the server's budget_ms, published by
      // the binary that enforces its three parts. The client keeps no mirrored
      // copy of those durations, and RESTART_STEPS follows the server's
      // phaseRank order.
      check('the restart client keeps no mirrored choreography budget',
        sw.eval('typeof RESTART_BUILD_BUDGET_MS') === 'undefined' &&
        sw.eval('typeof RESTART_READY_BUDGET_MS') === 'undefined' &&
        sw.eval('typeof RESTART_DRAIN_FALLBACK_MS') === 'undefined');
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
      const dbg = sd.getElementById('drawer-debug');
      const dbgRow = k => [...dbg.querySelectorAll('.detail-kv')].find(r => r.querySelector('.k') && r.querySelector('.k').textContent === k);
      check('the drawer renders the capture identity and timing rows',
        await settleUntil(() => !!dbgRow('session') && dbgRow('session').querySelector('.v').textContent === 'first' &&
          !!dbgRow('client') && dbgRow('client').querySelector('.v').textContent === 'c' &&
          !!dbgRow('conversation') && dbgRow('conversation').querySelector('.v').textContent === 'cv' &&
          !!dbgRow('captured') && dbgRow('captured').querySelector('.v').textContent.includes('2026-09-14') &&
          !!dbgRow('stream') && dbgRow('stream').querySelector('.v').textContent === 'yes' &&
          !!dbgRow('duration') && dbgRow('duration').querySelector('.v').textContent === '1s' &&
          !!dbgRow('queue wait') && dbgRow('queue wait').querySelector('.v').textContent === '10ms' &&
          !!dbgRow('first answer') && dbgRow('first answer').querySelector('.v').textContent === '300ms' &&
          !!dbgRow('input') && dbgRow('input').querySelector('.v').textContent === '10'));
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
      const dbgIds = [...dbg.querySelectorAll('[id]')].map(el => el.id);
      check('the debug capture card has unique element ids',
        new Set(dbgIds).size === dbgIds.length);
      const dbgToggle = dbg.querySelector('[data-section-toggle]');
      check('the debug capture card collapses through the shared section owner',
        !!dbgToggle && dbgToggle.getAttribute('aria-expanded') === 'true');
      if (dbgToggle) {
        dbgToggle.click();
        check('the debug capture card keeps its body while collapsed',
          dbg.classList.contains('collapsed') && dbgToggle.getAttribute('aria-expanded') === 'false' &&
          dbg.textContent.includes('Request body (truncated)'));
        dbgToggle.click();
        check('the debug capture card expands again',
          !dbg.classList.contains('collapsed') && dbgToggle.getAttribute('aria-expanded') === 'true');
      }

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
        await settleUntil(() => sw.__dlFetches.length === 1 &&
          dlTape.some(el => el && el.tagName === 'A' &&
            el.getAttribute('download') === 'millivolt-debug-20260914-000000.json.gz') &&
          dlTape.includes('revoke:blob:w16-dl'));
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
        await settleUntil(() => dlTape.some(el => el && el.tagName === 'A' &&
          el.getAttribute('download') === 'millivolt-debug.json.gz'));
        check('a missing Content-Disposition falls back to the designed artifact name',
          dlTape.filter(el => el && el.tagName === 'A')
            .some(a => a.getAttribute('download') === 'millivolt-debug.json.gz'));
        sw.__dlResponse = { ok: false, status: 502, headers: { get: () => null } };
        dbgBtn.click();
        await settleUntil(() => {
          const status = sd.getElementById('drawer-debug-status');
          return !!status && status.textContent === 'debug capture download failed';
        });
        check('a failed capture download surfaces the designed failure message in the section',
          sd.getElementById('drawer-debug-status').textContent === 'debug capture download failed');
      }
      sd.querySelector('[data-group-tab="diagnostics"]')?.click();
      const liveDbg = sd.getElementById('drawer-debug');
      const sessionRow = [...liveDbg.querySelectorAll('.detail-kv')]
        .find(r => r.querySelector('.k')?.textContent === 'session');
      const sessionCopy = sessionRow.querySelector('.copy-btn');
      sessionCopy.focus();
      sw.renderDrawer();
      await settleUntil(() => {
        const sec = sd.getElementById('drawer-debug');
        const row = sec && [...sec.querySelectorAll('.detail-kv')]
          .find(r => r.querySelector('.k')?.textContent === 'session');
        return !!row && sd.activeElement === row.querySelector('.copy-btn');
      });
      check('focus inside the debug capture survives a live repaint',
        !!sd.activeElement &&
        sd.activeElement.dataset.focusKey === 'row:dbg-identity:session');

      const realOperatorJson = sw.operatorJson;
      let staleReject = null;
      let captureCalls = 0;
      sw.operatorJson = () => {
        captureCalls++;
        if (captureCalls === 1) {
          return new Promise((_, reject) => { staleReject = reject; });
        }
        return Promise.resolve(captureDoc);
      };
      sw.renderDrawer();
      sw.renderDrawer();
      await settleUntil(() => captureCalls === 2 &&
        !!sd.getElementById('drawer-debug')?.querySelector('.detail-kv'));
      staleReject(new Error('stale capture'));
      await new Promise(r => setTimeout(r, 0));
      check('a stale debug rejection cannot overwrite the newer capture',
        captureCalls === 2 &&
        !!sd.getElementById('drawer-debug')?.querySelector('.detail-kv') &&
        !sd.getElementById('drawer-debug').textContent.includes('capture unavailable'));
      sw.operatorJson = realOperatorJson;
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
};
