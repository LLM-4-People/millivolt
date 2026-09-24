// jsdom functional test for the dashboard's incremental live-update pipeline.
// Drives the page ONLY through its own external interfaces: the stubbed
// fetch() and the EventSource event listeners the page registers itself.
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');
const {createHash} = require('crypto');
function modelFixture(rules = [], names = {}) {
  return {rules, revision: createHash('sha256').update(JSON.stringify({rules})).digest('hex'), names};
}
function observerFixture(p) {
  if (!p || typeof p !== 'object') return p;
  const records = [...(Array.isArray(p.records) ? p.records : []), ...(Array.isArray(p.in_flight_records) ? p.in_flight_records : []), ...(p.record ? [p.record] : []), ...(p.id ? [p] : [])];
  const rawNames = [...records.map(r => r.model || ''), ...(p.known_models || []), ...(p.sessions || []).flatMap(s => s.models || [])];
  const rules = p.model_canon?.rules || [];
  const names = Object.fromEntries(rawNames.map(raw => {
    let name = raw.trim();
    for (const rule of rules) {
      if (rule.disabled) continue;
      if (rule.mode === 'lower') name = name.toLowerCase();
      if (rule.mode === 'exact' && name === rule.from) name = rule.to;
    }
    return [raw, name || raw.trim()];
  }));
  const out = {...p, model_canon: {...modelFixture(rules, names), ...p.model_canon, names: {...names, ...p.model_canon?.names}}};
  if (p.debug) out.debug = observerFixture({...p.debug, model_canon: {...modelFixture(rules), ...p.debug.model_canon}});
  return out;
}
// Synthetic observers emit the same metadata contract as the server tests.
// Tests of pattern semantics provide explicit Go-authored dictionary values.
function dashboardDOM(html, options) {
  return new JSDOM(html, {...options, beforeParse(win) {
    options.beforeParse(win);
    const original = win.fetch;
    let wrapped;
    Object.defineProperty(win, 'fetch', {configurable: true, get: () => wrapped, set(fetch) {
      wrapped = (url, opts) => Promise.resolve(fetch(url, opts)).then(response => {
        if (!String(url).includes('/metrics/bootstrap') && !String(url).includes('/metrics/agg/log') && !String(url).includes('/admin/debug')) return response;
        return {...response, json: async () => observerFixture(await response.json())};
      });
    }});
    win.fetch = original;
  }});
}

// jsdom does not fetch <script src> / <link href> from disk. Assemble the
// dashboard the same way the browser will see it after /dash/* loads: inline
// every allowlisted asset referenced from index.html.
const STATIC = path.join(__dirname, '..', 'internal/web/static');
const TEST_DASHBOARD_VERSION = '0000000000000001';
function assembleHTML(bootstrap) {
  let html = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
  html = html.replace('__DASHBOARD_VERSION__', TEST_DASHBOARD_VERSION);
  if (bootstrap !== undefined) {
    const data = typeof bootstrap === 'string' ? bootstrap : JSON.stringify(observerFixture({pending_revision: 0, ...bootstrap})).replace(/</g, '\\u003c');
    html = html.replace('__DASHBOARD_BOOTSTRAP__', () => data);
  }
  html = html.replace(/<link\s+rel="stylesheet"\s+href="\/dash\/([^"]+)"\s*>/g, (_, rel) => {
    return `<style>\n${fs.readFileSync(path.join(STATIC, rel), 'utf8')}\n</style>`;
  });
  const scripts = [];
  html = html.replace(/<script\b[^>]*\bsrc="\/dash\/([^"]+)"[^>]*><\/script>/g, (_, rel) => {
    scripts.push(`<script>\n${fs.readFileSync(path.join(STATIC, rel), 'utf8')}\n</script>`);
    return '';
  });
  // Inline scripts cannot defer. Put the same ordered scripts after the DOM
  // and data block to mirror the external defer scripts' browser execution.
  return html.replace('</body>', () => scripts.join('\n') + '\n</body>');
}
const html = assembleHTML();

let failures = [];
// diagDump is installed once the dashboard DOM exists; until then a failed
// check carries only its label. On every red check it prints the recorded
// activity that explains the failure, so a diagnosis needs no second run
// and no ad-hoc instrumentation, locally or in CI output.
let diagDump = null;
const check = (label, cond) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + label);
  if (!cond) {
    failures.push(label);
    if (diagDump) console.log(diagDump(label));
  }
};
// The shared summary owner: the green path, a red run and a crash all
// report the accumulated failures through this one printer.
const reportFailures = crashed => {
  if (failures.length) console.log('\nFAILURES: ' + failures.join(' | '));
  else console.log(crashed ? '\nCRASHED BEFORE ANY CHECK FAILED' : '\nALL UI CHECKS PASSED');
};

const mkRec = (id, status = 200, start = Date.now() - 1000) => ({
  id, provider: 'p', model: 'm', client: 'c', key_hash: 'k' + id,
  conversation_id: 'cv', status_code: status, start: new Date(start).toISOString(),
  end: new Date(start + 500).toISOString(), stream: true, time_bucket: 'work',
  ttft_ms: 100, duration_ms: 500, usage: { input_tokens: 10, output_tokens: 5, total_tokens: 15 },
  retries: 0, attempts: [],
});
const LIST = Array.from({ length: 30 }, (_, i) => mkRec('req' + String(i).padStart(3, '0'), 200, 1700000000000 + i * 1000));

let fullPayload = {
  feed_id: 'feedA', seq: 30, incremental: false,
  records: LIST.map((r, i) => ({ ...r, __seq: i + 1 })),
  in_flight_records: [],
  counters: { in_flight: 0, total_requests: 30, total_errors: 0 },
};
let logPage = { records: [], more: false, cursor_ms: 1 };
let lastLogURL = '';
let logPageGate = null; // when set, log-page responses wait on this promise
// The traffic chart's stubbed payload: the REAL zero-traffic server shape
// (HandleAggChart with an empty store + empty ring) - a clock-aligned
// ZERO-FILLED buckets array (count >= 1, every req 0, all-null percentile
// triples), never an empty one. Pinned server-side by
// TestChartZeroTrafficPayloadShape; test 11g drives it end-to-end.
const chartNow = Date.now();
const chartStep = 120000; // the 60m window's ladder step (2m)
const chartFrom = chartNow - 3600000 - ((chartNow - 3600000) % chartStep);
let chartPayload = {
  now_ms: chartNow, from_ms: chartFrom, bucket_ms: chartStep,
  ttft_p: [null, null, null], tps_p: [null, null, null],
  ttft_stat: null, tps_stat: null,
  buckets: Array.from({ length: Math.ceil((chartNow - chartFrom) / chartStep) }, (_, i) => ({
    t: chartFrom + i * chartStep,
    req: 0, err: 0, rl: 0, in: 0, out: 0, cache: 0, reason: 0, cost: 0,
    ttft: [null, null, null], tps: [null, null, null],
  })),
};
let restartState = { restarted: false, postCount: 0, statusGate: null, postGate: null, status: {} };
let cfgFetches = 0;
// Default model canonicalization pipeline - the ui_check mirror of
// config.DefaultModelRules, kept in lockstep with the Go table. It seeds
// the harness like a real boot (the bootstrap payload's model_canon.rules,
// so every grouped consumer below - debug checklist, explorer scope,
// Clear/Logs optgroups - sees variants merged) and rides cfgDoc.defaults
// .model_rules, the server-provided shape the restore-defaults control
// reads in Settings.
const DEF_RULES = [
  { mode: 'lower' },
  { mode: 'pattern', from: '^[a-z0-9][a-z0-9._-]*/', to: '' },
  { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' },
  { mode: 'pattern', from: '-(?:[a-z]{0,2}fp\\d+|bf\\d+|int\\d+|nf\\d+|[a-z]?q\\d+(?:_[0-9a-z]+)*)$', to: '' },
  { mode: 'pattern', from: '(\\d)\\.(\\d)', to: '$1-$2' },
];
// Providers editor fixture: the server-owned canonical usage fields plus one
// mapped provider (neutral fixture label).
const cfgDoc = {
  revision: 'fixture-revision',
  fields: [
    { key: 'providers', category: 'providers', label: 'Provider field maps', help: 'Per-provider JSON key-path overrides.', kind: 'providers', hot_reload: true },
    { key: 'provider_aliases', category: 'providers', label: 'Provider aliases', help: 'Merge an old provider label into its canonical one (old → canonical).', kind: 'aliases', hot_reload: true },
  ],
  categories: [{ id: 'providers', label: 'Providers', help: 'Per-provider usage/cost JSON field-name maps.' }],
  values: { providers: { 'epsilon.example': { cost_keys: ['x_billing_pricing.cost'], usage_keys: { input_tokens: 'x_billing_pricing.inputTokens' }, models_path: '/model-meta', models_keys: { input_modalities: 'input_modalities' }, ensure_tools: ['bash', 'read'], headers: { 'User-Agent': 'my-shell/1.0 ({{platform}})' } } }, provider_aliases: { 'old.example': 'new.example' } },
  defaults: { providers: {}, model_rules: DEF_RULES },
  effective: {},
  overrides: {},
  writable: true,
  usage_fields: ['input_tokens', 'output_tokens', 'total_tokens', 'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens'],
};

const sseHandlers = {};
const initialFetches = [];
const pageOptions = {
  url: 'http://127.0.0.1:8081/',
  runScripts: 'dangerously',
  pretendToBeVisual: true,
  beforeParse(window) {
    window.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
    window.confirm = () => true;
    window.matchMedia = () => ({ matches: false, addListener() {}, removeListener() {}, addEventListener() {}, removeEventListener() {} });
    window.CSS = window.CSS || {};
    if (!window.CSS.escape) window.CSS.escape = s => String(s).replace(/[\\"]/g, '\\$&');
    Object.defineProperty(window.HTMLElement.prototype, 'offsetHeight', { get() { return 20; }, configurable: true });
    // jsdom has no canvas path implementation. The mounted-chart smoke check
    // exercises uPlot's lifecycle; raster drawing is verified in a browser.
    window.Path2D = class {
      moveTo() {} lineTo() {} arcTo() {} arc() {} rect() {} closePath() {} bezierCurveTo() {}
    };
    window.HTMLCanvasElement.prototype.getContext = () => new Proxy({}, {
      get: (t, k) => (k === 'measureText' ? () => ({ width: 1 }) :
        k === 'canvas' ? { width: 0, height: 0 } :
        (typeof k === 'string' ? () => {} : undefined)),
      set: () => true,
    });
    window.fetch = (url, opts) => {
      const u = typeof url === 'string' ? url : '';
      initialFetches.push(u);
      if (u.includes('/admin/restart')) {
        if (opts && opts.method === 'POST') {
          restartState.postCount++;
          // postFail simulates a refusal whose body is not JSON-with-an-
          // error field (the operator plane writes plain-text denials): the
          // fallback contract, not a server error message, must surface.
          if (restartState.postFail) {
            const status = restartState.postFail.status;
            return Promise.resolve({
              ok: false, status,
              json: async () => { throw new SyntaxError('unexpected token < in HTML'); },
              text: async () => '<html>bad gateway</html>',
            });
          }
          const response = () => { restartState.restarted = true; return { ok: true, json: async () => ({ ok: true, phase: 'building', rank: 0, drain_timeout_ms: 600000, error: '' }) }; };
          return restartState.postGate ? restartState.postGate.then(response) : Promise.resolve(response());
        }
        const status = { ok: true, phase: 'idle', rank: -1, error: '', pid: restartState.restarted ? 424243 : 424242, started_at: restartState.restarted ? 2000 : 1000, available: true, reason: '', ...restartState.status };
        const response = { ok: true, json: async () => status };
        return restartState.statusGate ? restartState.statusGate.then(() => response) : Promise.resolve(response);
      }
      if (u.includes('/admin/pause')) return Promise.resolve({ json: async () => ({ ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], until: null, queued: 0, default_max_queued: 0 }) });
      if (u.includes('/admin/debug')) return Promise.resolve({ json: async () => ({ ok: true, enabled: false, sessions: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], known_models: ['m'], until: null, ttl: '24h', max_bytes: '1MiB' }) });
      if (u.includes('/admin/throttle')) return Promise.resolve({ json: async () => ({ ok: true, throttles: [], known_providers: ['p'], active: false }) });
      if (u.includes('/admin/config')) { cfgFetches++; return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(cfgDoc)) }); }
      if (u.includes('/metrics/agg/log')) {
        lastLogURL = u;
        const resp = { ok: true, json: async () => JSON.parse(JSON.stringify(logPage)) };
        return logPageGate ? logPageGate.then(() => resp) : Promise.resolve(resp);
      }
      if (u.includes('/metrics/agg/chart')) {
        return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(chartPayload)) });
      }
      if (u.includes('/metrics/agg/explorer')) {
        return Promise.resolve({ok: true, json: async () => ({
          dim: 'provider', total: 30, error_total: 0, rail: {}, groups: [], scope: {matches: 30, errors: 0},
        })});
      }
      if (url.includes('/admin/purge/count')) return Promise.resolve({ ok: true, json: async () => ({ count: 3 }) });
      if (url.includes('/admin/purge')) return Promise.resolve({ ok: true, json: async () => ({ok: true}) });
      // /metrics/bootstrap: the real payload carries the snapshot PLUS the
      // state surfaces (kpi, dash, pause, throttle) - mirror the shapes the
      // dedicated /admin/ endpoints serve so state-driven UI stays exercised.
      if (u.includes('/metrics/bootstrap')) {
        const bp = fullPayload;
        const bsince = new URL(url, 'http://x').searchParams.get('since');
        const brecs = bsince ? bp.records.filter(r => r.__seq > Number(bsince)) : bp.records;
        return Promise.resolve({ ok: true, json: async () => ({
          records: JSON.parse(JSON.stringify(brecs)),
          in_flight_records: bp.in_flight_records,
          counters: bp.counters,
          pending_revision: bp.pending_revision ?? Math.max(0, window.eval('_pendingRevision')),
          // These ordinary fixtures replace the sample collection between
          // scenarios; a live server's same-feed cursor never rewinds with it.
          seq: bp.feed_id === window.eval('feedId') ? Math.max(bp.seq, window.eval('lastSeq')) : bp.seq,
          feed_id: bp.feed_id, incremental: !!bsince,
          kpi: { requests: 30, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null },
          dashboard_version: TEST_DASHBOARD_VERSION,
          model_canon: {rules: []},
          dash: {},
          storage: { enabled: true, dropped: 0, totals_degraded: false },
          pause: { ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], until: null, queued: 0, default_max_queued: 0 },
          throttle: { ok: true, throttles: [], known_providers: ['p'], active: false },
          debug: { ok: true, enabled: false, sessions: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], known_models: ['m'], until: null, ttl: '24h', max_bytes: '1MiB' },
        }) });
      }
      const fp = fullPayload;
      const since = new URL(url, 'http://x').searchParams.get('since');
      const recs = since ? fp.records.filter(r => r.__seq > Number(since)) : fp.records;
      const payload = {
        records: JSON.parse(JSON.stringify(recs)),
        in_flight_records: [],
        counters: fp.counters,
        pending_revision: fp.pending_revision ?? 0,
        seq: fp.seq,
        feed_id: fp.feed_id, incremental: !!since,
      };
      return Promise.resolve({ json: async () => payload });
    };
    window.EventSource = function (url) {
      return {
        url, readyState: 1,
        addEventListener(type, cb) { sseHandlers[type] = cb; },
        close() { this.readyState = 2; },
      };
    };
  },
};
const dom = dashboardDOM(html, pageOptions);

const w = dom.window;
const d = w.document;
// jsdom reports uncaught page errors and its own not-implemented navigation
// through virtualConsole jsdomError events; the default console forwarding
// only prints them to stderr, where a green run can hide an uncaught
// thrower. Install the counting listener at harness start, before any test
// runs: a not-implemented navigation counts as the reload signal the
// reload-boundary test reads (jsdomReloads); every other error fails the
// run while still printing. Installed previously at the reload test, too
// late to count throwers from earlier scenarios.
let jsdomReloads = 0;
dom.virtualConsole.removeAllListeners('jsdomError');
dom.virtualConsole.on('jsdomError', error => {
  if (error.type === 'not-implemented' && /navigation/.test(error.message)) jsdomReloads++;
  else { failures.push(error.message); console.error(error); }
});
// ---------- failure diagnostics ----------
// One bounded activity recorder feeds one dump owner, so any red check or
// a crashed run reports the same decisive context in its own output. The
// recorded owners are the page's lifecycle functions because the settings
// sheet rebuilding under a cached DOM handle is the failure class that
// hides from a bare label: a refill turns every element reference the
// tests hold stale while focus and flags stay plausible. Recording is
// passive - names and one key argument per call, the log capped - so a
// green run prints nothing and pays almost nothing.
const diagT0 = Date.now();
const diagEvents = [];
const diagNote = entry => {
  diagEvents.push(((Date.now() - diagT0) / 1000).toFixed(1) + 's ' + entry);
  if (diagEvents.length > 100) diagEvents.shift();
};
for (const [name, describe] of [
  ['fillSettingsForm', doc => 'settings refill, revision ' + (doc && doc.revision)],
  ['fetchSettings', () => 'settings fetch'],
  ['refreshAfterRestart', () => 'reconnect refresh of every server-fed surface'],
  ['fetchBootstrap', mode => 'bootstrap fetch, mode ' + mode],
  ['applySnapshotPayload', payload => 'snapshot, feed ' + (payload && payload.feed_id)],
]) {
  if (typeof w[name] !== 'function') continue;
  const orig = w[name];
  w[name] = function () {
    diagNote(name + ' (' + describe(...arguments) + ')');
    return orig.apply(this, arguments);
  };
}
// The DOM truth supplements the owner names: any rebuild of the settings
// fields (including a load-failure render) and every sheet open or close
// are recorded whatever called them, since event listeners capture their
// handler references before this wrapper layer existed.
const diagFields = d.getElementById('settings-fields');
if (diagFields && typeof w.MutationObserver === 'function') {
  new w.MutationObserver(() => diagNote('settings fields rebuilt')).observe(diagFields, { childList: true });
  const diagSheet = d.getElementById('settings-sheet');
  if (diagSheet) new w.MutationObserver(() => diagNote('settings sheet ' + (diagSheet.hidden ? 'closed' : 'opened'))).observe(diagSheet, { attributes: true, attributeFilter: ['hidden'] });
}
const diagActive = () => {
  const el = d.activeElement;
  if (!el) return null;
  return el.tagName + (el.id ? '#' + el.id : '') + (el.className ? '.' + String(el.className).trim().split(/\s+/).join('.') : '');
};
diagDump = why => 'DIAG ' + JSON.stringify({
  why,
  activity: diagEvents,
  activeElement: diagActive(),
  settingsSheetOpen: (() => { const s = d.getElementById('settings-sheet'); return s ? !s.hidden : null; })(),
  jsdomReloads,
  feedId: w.eval('feedId'),
  lastSeq: w.eval('lastSeq'),
  cannedFeed: typeof fullPayload !== 'undefined' && fullPayload ? { feed_id: fullPayload.feed_id, seq: fullPayload.seq } : null,
  settingsDoc: w.eval('typeof settingsDoc !== "undefined" && settingsDoc ? settingsDoc.revision : null'),
  settingsCat: w.eval('typeof settingsCat !== "undefined" ? settingsCat : null'),
});
for (const name of ['', 'provider 5', 'private-label', '127.0.0.1', '[::1]', 'localhost',
  'LOCALHOST', 'gateway.LOCALHOST', 'gateway.internal', 'gateway.local', 'host:443', 'https://host.example',
  'bad_label.example', '-host.example', 'host-.example', 'host..example']) {
  check('provider favicon rejects non-public host label ' + JSON.stringify(name),
    w.providerOrigin(name) === '' && !w.entityBadge('provider', name).includes('<img'));
}
check('qualified provider favicon keeps the canonical host',
  w.providerOrigin('API.vendor.example') === 'api.vendor.example' &&
  w.entityBadge('provider', 'api.vendor.example').includes('domain=api.vendor.example'));
const sleep = ms => new Promise(r => setTimeout(r, ms));

const rows = () => [...d.querySelectorAll('#tbl-requests tr.exp-row[data-id]')].filter(tr => !tr.classList.contains('retry-sub'));
const fire = (type, data, lastEventId) => {
  const cb = sseHandlers[type];
  if (!cb) throw new Error('no handler for ' + type);
  if (typeof data === 'object' && type !== 'record' && data.pending_revision === undefined) {
    // Ordinary fixtures model fresh lifecycle/snapshot state. Ordering tests
    // below supply explicit server revisions to exercise delayed deliveries.
    data = {...data, pending_revision: Math.max(0, w.eval('_pendingRevision')) + (type === 'snapshot' ? 0 : 1)};
    if (type === 'snapshot' && !data.incremental && data.feed_id === w.eval('feedId')) {
      data.seq = Math.max(data.seq, w.eval('lastSeq'));
    }
  }
  cb({ type, data: typeof data === 'string' ? data : JSON.stringify(observerFixture(data)), lastEventId });
};

async function main() {
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

  // ---- test 1: full render painted 30 rows ----
  check('full render shows 30 rows', rows().length === 30);
  check('dimension picker trigger exists', !!d.getElementById('xp-dim-trigger'));
  check('dimension rail still has nine dims', d.querySelectorAll('#xp-rail .xp-rail-item').length === 9);
  check('first paint fetches chart and explorer exactly once when model rules arrive',
    initialFetches.filter(u => u.includes('/metrics/agg/chart')).length === 1 &&
    initialFetches.filter(u => u.includes('/metrics/agg/explorer')).length === 1);
  check('first chart fetch starts its refresh cadence instead of refetching on the first tick', w.eval('_lastChartFetch > 0'));

  // Shared explorer health signals/order and server-authored conversations.
  {
    const order = ['provider','model','client','conversation','tool','time','status','error','key'];
    check('explorer dimension order is the single desktop/mobile order',
      [...d.querySelectorAll('#xp-rail [data-xp-dim]')].map(n=>n.dataset.xpDim).join()===order.join());
    const entity = {name:'neutral.example',n:3,cost:0,in:0,out:0,cache:0,err_final:1,
      rate_limit_requests:2,tools:0,cost_per_mtok:null,ttft_p50:null,ttft_p95:null,tps_p50:null,tps_p95:null,
      err_events:4,code:'500',last_ms:0};
    const renderCard = (dim, overrides={}) => {
      const host=d.createElement('div');
      host.innerHTML=w.xpNodeCard({activeDim:dim,filters:[]},{...entity,...overrides},{total:3,error_total:4});
      return host;
    };
    for (const dim of order) {
      const card=renderCard(dim);
      check(`${dim} has one separate error/429 request tally`,
        card.querySelectorAll('.xp-node-err').length===1 && card.querySelector('.xp-node-err').textContent==='1 err' &&
        card.querySelectorAll('.xp-node-rate').length===1 && card.querySelector('.xp-node-rate').textContent==='2 ×429' &&
        card.querySelector('.xp-node-rate').title==='Requests with final or retried HTTP 429; each request counted once' &&
        card.querySelector('.xp-node-signals').textContent==='1 err · 2 ×429');
      for (const [name, counts, expected] of [
        ['zero', {err_final:0,rate_limit_requests:0}, ''],
        ['error only', {err_final:1,rate_limit_requests:0}, '1 err'],
        ['429 only', {err_final:0,rate_limit_requests:2}, '2 ×429'],
        ['missing', {err_final:undefined,rate_limit_requests:undefined}, ''],
      ]) {
        const only=renderCard(dim,counts);
        check(`${dim} ${name} health signals omit zero badges and stray separators`,
          (only.querySelector('.xp-node-signals')?.textContent || '')===expected &&
          !!only.querySelector('.xp-node-err')===(counts.err_final>0) &&
          !!only.querySelector('.xp-node-rate')===(counts.rate_limit_requests>0) &&
          !only.querySelector('.xp-node-signals [aria-hidden]'));
      }
      if (['status','key','time'].includes(dim)) check(`${dim} grid does not duplicate the error count`,
        !card.querySelector('.xp-node-kpis').textContent.includes('errors'));
    }
    for(const dim of ['status','key']) check(`${dim} retains its error-rate KPI`,renderCard(dim).querySelector('.xp-node-kpis').textContent.includes('err rate'));
    // shareBar: the % flows through the pct/pctCap owner, so a
    // strictly-sub-1 fraction can never round up to a false '100%' and a
    // full fraction is a true 100. An unmeasured share (zero denominator)
    // renders no share row at all, never a fabricated 0%.
    check('shareBar never rounds a sub-100 share to a false 100%',
      w.shareBar(0.9998, '#56B4E9').includes('99.9% of total') &&
        !w.shareBar(0.9998, '#56B4E9').includes('100% of total') &&
        w.shareBar(1, '#56B4E9').includes('100.0% of total'));
    check('an unmeasured share renders no fabricated 0% row', (() => {
      const measured = renderCard('provider');
      const zero = d.createElement('div');
      zero.innerHTML = w.xpNodeCard({activeDim:'provider',filters:[]}, {...entity, n: 3}, {total: 0, error_total: 0});
      const zeroErr = d.createElement('div');
      zeroErr.innerHTML = w.xpNodeCard({activeDim:'error',filters:[]}, {...entity, err_events: 2}, {total: 0, error_total: 0});
      return !!measured.querySelector('.xp-node-share') &&
        measured.querySelector('.xp-node-share').title === 'of requests' &&
        !zero.querySelector('.xp-node-share') && !zero.textContent.includes('0%') &&
        !zeroErr.querySelector('.xp-node-share') && !zeroErr.textContent.includes('0%');
    })());
    const savedAgg=w.eval('explorerAgg');
    const trigger=d.getElementById('xp-dim-trigger'), originalStyle=w.getComputedStyle;
    try {
      w.eval('explorerAgg={rail:{conversation:4},conversation_summary:{main:1,sub:2,unresolved:1}}');
      w.renderDimRail({activeDim:'conversation',filters:[]});
      const summary='1 main + 2 sub + 1 unresolved';
      check('conversation rail headline uses server main/sub/unresolved counts',d.querySelector('[data-xp-dim="conversation"] .rail-n').textContent===summary);
      check('mobile dimension trigger reuses the same conversation summary',trigger.querySelector('.rail-n').textContent===summary);
      w.getComputedStyle=el=>el===trigger?{display:'flex'}:originalStyle(el);
      w.toggleDimMenu();
      check('mobile picker opens the same ordered dimension nodes',trigger.getAttribute('aria-expanded')==='true' &&
        [...d.querySelectorAll('#xp-rail [data-xp-dim]')].map(n=>n.dataset.xpDim).join()===order.join());
      w.closeDimMenu();
      check('mobile picker closes without losing summary',trigger.getAttribute('aria-expanded')==='false' && trigger.querySelector('.rail-n').textContent===summary);
    } finally {w.getComputedStyle=originalStyle;w.__restoreExplorer=savedAgg;w.eval('explorerAgg=window.__restoreExplorer;delete window.__restoreExplorer');w.renderDimRail(w.explorerState());}
    const parent='s:parent"<neutral>',scope=[{dim:'client',id:'client'},{dim:'key',id:'hashed-key'},{dim:'conversation',id:parent}];
    const child=renderCard('conversation',{name:'s:child',conversation:{role:'sub',parent_id:parent,parent_in_scope:false,parent_scope:scope}});
    const link=child.querySelector('a.xp-conversation-parent');
    check('subconversation role and external parent link are visible',child.querySelector('.xp-conversation-role').textContent==='sub' &&
      !!link && !child.querySelector('button a') && link.textContent.includes('outside view'));
    check('parent pivot retains exact namespace and leaf filter',link?.getAttribute('href')===w.buildHash([...scope,{dim:'by',id:'conversation'}]) && link.title.includes('descendants are not included'));
    check('parent identifiers are escaped as text and a hash URL',!child.querySelector('neutral') && !child.querySelector('[onerror]') && link.textContent.includes(parent.slice(2)));
    check('unobserved parents are distinct from filtered-out parents',
      renderCard('conversation',{conversation:{role:'sub',parent_id:parent,parent_observed:false,parent_in_scope:false}}).querySelector('.xp-conversation-parent').textContent.includes('not observed'));
    for(const badScope of [undefined,[{dim:'client',id:'client'},{dim:'key',id:''},{dim:'conversation',id:parent}]]) {
      const unresolved=renderCard('conversation',{conversation:{role:'sub',parent_id:parent,parent_scope:badScope}});
      check('unfilterable parent remains visible without a broadened link',!unresolved.querySelector('a.xp-conversation-parent') && unresolved.querySelector('.xp-conversation-parent').textContent.includes('parent'));
    }
    check('unknown relationships stay explicitly unresolved',renderCard('conversation',{conversation:{role:'unknown',issue:'multiple_scopes'}}).querySelector('.xp-conversation-role').textContent==='unresolved');
    check('main conversation role is explicit',renderCard('conversation',{conversation:{role:'main'}}).querySelector('.xp-conversation-role').textContent==='main');
    check('main and sub tooltips state only observed facts',
      renderCard('conversation',{conversation:{role:'main'}}).querySelector('.xp-conversation-role').title==='No parent declared in observed history' &&
      child.querySelector('.xp-conversation-role').title==='Client-declared parent conversation');
  }

  // ---- test 2: live 'record' event → incremental insertion, stable nodes ----
  const oldFifth = rows()[5];
  fire('record', { ...mkRec('reqnew0', 200, 1700000031000) }, '31');
  await sleep(20);
  check('record event inserts new row at top', rows().length === 31 && rows()[0].dataset.id === 'reqnew0');
  check('existing rows keep the SAME nodes (no table rebuild)', rows().includes(oldFifth));

  // ---- test 3: in-flight begin → final end replaces ONLY that row ----
  const inRec = { ...mkRec('reqnew1', 200, 1700000032000) };
  delete inRec.status_code;
  fire('begin', { record: inRec, in_flight: 1 });
  await sleep(20);
  const inRow = d.querySelector('#tbl-requests tr.live-row[data-id="reqnew1"]');
  check('begin event renders the in-flight live-row', !!inRow);
  const stable = rows().find(tr => tr === oldFifth);
  fire('end', { record: mkRec('reqnew1', 200, 1700000032000), in_flight: 0 });
  await sleep(20);
  const finRow = d.querySelector('#tbl-requests tr.exp-row[data-id="reqnew1"]');
  check('end event finalizes only that row', finRow && !finRow.classList.contains('live-row') && finRow.textContent.includes('200'));
  check('unrelated rows untouched across lifecycle events', rows().some(tr => tr === stable));
  check('two rows for same id never coexist', d.querySelectorAll('#tbl-requests tr.exp-row[data-id="reqnew1"]').length === 1);

  // ---- test 4: scroll anchoring (user scrolled into the list) ----
  const cont = d.getElementById('tbl-requests');
  cont.scrollTop = 200;
  fire('record', { ...mkRec('reqnew2', 200, 1700000033000) }, '32');
  await sleep(20);
  check('scrollTop compensated for the inserted row height', cont.scrollTop === 220);

  // ---- test 5: incremental snapshot merge (SSE reconnect with cursor) ----
  fire('snapshot', { feed_id: 'feedA', seq: 34, incremental: true,
    records: [{ ...mkRec('reqnew3', 200, 1700000034000) }],
    counters: { in_flight: 0, total_requests: 34, total_errors: 0 } });
  await sleep(20);
  check('incremental snapshot merges without full replace', rows().includes(oldFifth) && rows().length === 34);

  // ---- test 6: feed change (proxy restart) → full replace ----
  fire('snapshot', { feed_id: 'feedB', seq: 2, incremental: false,
    records: [{ ...mkRec('fresh1', 200, 1700000100000) }, { ...mkRec('fresh2', 200, 1700000101000) }],
    counters: { in_flight: 0, total_requests: 2, total_errors: 0 } });
  await sleep(20);
  const ids = rows().map(tr => tr.dataset.id);
  check('feed change resets view to the new snapshot', rows().length === 2 && ids.includes('fresh1') && ids.includes('fresh2'));

  // ---- test 7: stale-cursor poll (since > latest) → server full replaces ----
  // (Feed B has seq 2; a poll with the old feedA cursor would ask since=34 -
  // the page's own cursor was invalidated to 2, so the page asks since=2 and
  // the stub returns an in-range delta. Verify the page cursor followed.)
  fullPayload = {
    feed_id: 'feedB', seq: 3,
    records: [{ ...mkRec('fresh1', 200, 1700000100000), __seq: 1 },
              { ...mkRec('fresh2', 200, 1700000101000), __seq: 2 },
              { ...mkRec('fresh3', 200, 1700000102000), __seq: 3 }],
    counters: { in_flight: 0, total_requests: 3, total_errors: 0 },
  };
  // Trigger the page's own 5s poll path by simulating a disconnected SSE-less
  // state is awkward; instead assert the fetch the page would make is formed
  // correctly by spying on window.fetch calls once (invoked manually).
  let fetchedSince = null;
  const origFetch = w.fetch;
  // Forward BOTH args - later tests POST via this wrapper, and dropping the
  // options object silently turns their POSTs into GET-shaped calls.
  w.fetch = (url, opts) => { fetchedSince = new URL(url, 'http://x').searchParams.get('since'); return origFetch(url, opts); };
  // run the page's poll tick handler? The interval skips when SSE 'connected'.
  // The page's sse readyState is 1, so polling is dormant - verify cursor by
  // calling the page's own snapshot application with an in-range delta.
  fire('snapshot', { feed_id: 'feedB', seq: 3, incremental: true,
    records: [{ ...mkRec('fresh3', 200, 1700000102000) }],
    counters: { in_flight: 0, total_requests: 3, total_errors: 0 } });
  await sleep(20);
  const ok = rows().length === 3 && rows().includes(rows().find(tr => tr.dataset.id === 'fresh3'));
  check('post-restart incremental delta applies on the new feed', ok);

  // ---- test 7b: a kpiAgg swap with an EMPTY delta must still repaint the
  // KPI band (the derivation is memoized on the record revision -
  // applyBootstrapState bumps _rev when the aggregate changes).
  const origBootstrap = w.fetch;
  let bootstrapStateApplied = false;
  w.fetch = (url) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) {
      bootstrapStateApplied = true;
      return Promise.resolve({ ok: true, json: async () => ({
        records: [], counters: { in_flight: 0, total_requests: 3, total_errors: 0 },
        seq: 3, feed_id: 'feedB', incremental: true,
        kpi: { requests: 31, errors: 1, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0 },
        dash: {}, pause: {}, throttle: {},
      }) });
    }
    return origBootstrap(url);
  };
  w.eval("fetchBootstrap('none')");
  await sleep(20);
  w.fetch = origBootstrap;
  check('bootstrap state-only pull repaints the band from fresh kpiAgg',
    bootstrapStateApplied && d.getElementById('kpis').textContent.includes('31'));

  // ---- test 7b-2: a never-measured aggregate renders derived rate tiles as
  // '-' through the shared pct owner, never a fabricated 0%, while counts
  // stay honest zeros (0 requests, 0 of 0, 0 / 0 in).
  w.fetch = (url) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) {
      return Promise.resolve({ ok: true, json: async () => ({
        records: [], counters: { in_flight: 0, total_requests: 0, total_errors: 0 },
        seq: 4, feed_id: 'feedB', incremental: true,
        kpi: { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0 },
        dash: {}, pause: {}, throttle: {},
      }) });
    }
    return origBootstrap(url);
  };
  w.eval("fetchBootstrap('none')");
  await sleep(20);
  w.fetch = origBootstrap;
  const neverMeasured = d.getElementById('kpis').textContent;
  check('a never-measured band shows - rates and honest zero counts',
    neverMeasured.includes('0 of 0') && neverMeasured.includes('0 / 0 in') &&
    !neverMeasured.includes('%'));

  // ---- test 7b-3: the static KPI skeleton in index.html and the live
  // renderer's first kpiHtml must agree on tile count, headings and class
  // structure, so the first payload only swaps text and nothing below the
  // band moves (the chart-tile skeleton pin is the same idea for totals).
  {
    const skelDoc = new JSDOM(fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8')).window.document;
    const shape = root => [...root.querySelectorAll('.kpi')].map(k => {
      const val = k.querySelector('.val');
      return {
        cls: [...k.classList].sort().join(' '),
        h2: (k.querySelector('h2') || {textContent: ''}).textContent,
        val: val ? [...val.classList].sort().join(' ') : '',
      };
    });
    const skel = shape(skelDoc.getElementById('kpis'));
    const firstRender = w.eval(`(() => {
      const saved = kpiAgg;
      kpiAgg = { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null,
        input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, avg_ttft_ms: null, avg_tps: null };
      const html = derive({ records: [], counters: { in_flight: 0 }, _rev: 1 }).kpiHtml;
      kpiAgg = saved;
      return html;
    })()`);
    const host = d.createElement('div');
    host.innerHTML = firstRender;
    const live = shape(host);
    check('KPI band skeleton matches the first-rendered tile structure',
      skel.length === live.length && JSON.stringify(skel) === JSON.stringify(live) &&
      JSON.stringify(live));
  }

  // ---- test 7c: the 5s tick IS the SSE fallback - a cursor-resumed bootstrap
  // folds missed records exactly like the stream does, and replay-tolerant
  // delivery (the same record twice) must not duplicate the row.
  let bootURL = '';
  const origTick = w.fetch;
  w.fetch = (url) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) {
      bootURL = u;
      const fp = fullPayload;
      const since = new URL(url, 'http://x').searchParams.get('since');
      const recs = since ? fp.records.filter(r => r.__seq > Number(since)) : fp.records;
      return Promise.resolve({ ok: true, json: async () => ({
        records: JSON.parse(JSON.stringify(recs)),
        counters: fp.counters, seq: fp.seq,
        feed_id: fp.feed_id, incremental: !!since,
      }) });
    }
    return origTick(url);
  };
  fullPayload = { feed_id: 'feedB', seq: 4,
    records: [{ ...mkRec('fresh4', 200, 1700000103000), __seq: 4 }],
    counters: { in_flight: 0, total_requests: 4, total_errors: 0 } };
  w.eval("fetchBootstrap('resume')");
  await sleep(20);
  check('tick resume URL carries since + feed', bootURL.includes('since=3') && bootURL.includes('feed=feedB'));
  check('tick resume folds the missed record once', rows().length === 4 && rows()[0].dataset.id === 'fresh4');
  w.eval("fetchBootstrap('resume')"); // same cursor → the same record replays
  await sleep(20);
  check('tick replay of the same record never duplicates the row', rows().length === 4);
  w.fetch = origTick;

  // ---- test 7d: selection identity survives re-render ticks ----
  // The drawer's selection (drawerId) owns both the drawer's content and
  // the row highlight. Two re-render paths must keep the selection on the
  // same request: the lifecycle finalization that replaces the selected
  // row's own DOM node in place, and the full resync (renderAll re-renders
  // drawer AND log together).
  {
    const expandingRows = () => [...d.querySelectorAll('#tbl-requests tr.exp-row')]
      .filter(tr => tr.classList.contains('expanding'));
    const selRec = { ...mkRec('sel-live', 200, 1700000104000) };
    delete selRec.status_code;
    fire('begin', { record: selRec, in_flight: 1 });
    await sleep(20); // the live row paints on the coalesced render
    w.openDrawer('sel-live');
    check('opening a request row selects it in the drawer and highlights exactly its row',
      d.getElementById('drawer').classList.contains('open') &&
        d.getElementById('drawer-body').textContent.includes('sel-live') &&
        expandingRows().length === 1 && expandingRows()[0].dataset.id === 'sel-live');
    const nodeBefore = d.querySelector('#tbl-requests tr.exp-row[data-id="sel-live"]');
    fire('end', { record: { ...mkRec('sel-live', 200, 1700000104000), duration_ms: 999 }, in_flight: 0 });
    await sleep(20);
    const nodeAfter = d.querySelector('#tbl-requests tr.exp-row[data-id="sel-live"]');
    check('the selected row keeps its highlight when its own node is re-rendered',
      nodeBefore && nodeAfter && nodeBefore !== nodeAfter &&
        nodeAfter.classList.contains('expanding') &&
        expandingRows().length === 1 && expandingRows()[0].dataset.id === 'sel-live' &&
        d.getElementById('drawer').classList.contains('open'));
    fire('snapshot', { feed_id: 'feedB', seq: 4, incremental: false,
      records: [mkRec('fresh1', 200, 1700000100000), mkRec('fresh2', 200, 1700000101000),
        mkRec('fresh3', 200, 1700000102000), mkRec('fresh4', 200, 1700000103000),
        { ...mkRec('sel-live', 200, 1700000104000), duration_ms: 999 }],
      counters: { in_flight: 0, total_requests: 5, total_errors: 0 } });
    await sleep(20);
    check('a full resync re-renders drawer and log from the same selection identity',
      d.getElementById('drawer').classList.contains('open') &&
        d.getElementById('drawer-body').textContent.includes('sel-live') &&
        d.getElementById('drawer-body').textContent.includes('999ms') &&
        expandingRows().length === 1 && expandingRows()[0].dataset.id === 'sel-live');
    w.closeDrawer();
  }

  // ---- test 7e: attempt metadata parity in the drawer ----
  // Absorbed attempts store the same upstream information as the final
  // response; the attempt log shows each attempt's own provider request id,
  // the matching key between a provider-side failure report and the exact
  // absorbed attempt. Attempts without one (transport failures) render no
  // empty req span.
  {
    const atts = [
      { status_code: 502, error_type: 'bad_gateway', error_msg: 'upstream overloaded',
        provider_request_id: 'req_meta_1', at: new Date(1700000104500).toISOString() },
      { status_code: 0, error_type: 'transport', error_msg: 'connection reset',
        at: new Date(1700000104800).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-meta', 200, 1700000104000), retries: 2, attempts: atts }, in_flight: 0 });
    await sleep(20);
    w.openDrawer('att-meta');
    const attRows = [...d.querySelectorAll('.attempt-link')];
    check('the drawer attempt log renders the absorbed attempts with their own provider request ids',
      d.getElementById('drawer').classList.contains('open') &&
        attRows.length === 2 &&
        attRows[0].textContent.includes('502') &&
        attRows[0].textContent.includes('req req_meta_1') &&
        attRows[1].textContent.includes('transport'));
    check('an attempt without a provider request id renders no empty req span',
      !attRows[1].textContent.includes('· req'));
    w.closeDrawer();
  }

  // ---- test 7f: the attempt-detail drawer mode ----
  // Clicking an absorbed attempt (a drawer attempt link or a log retry-sub
  // row) must open THAT attempt in the drawer, not the final record: the
  // attempt view carries only the retained per-attempt truth (Go's
  // RetryAttempt parity) plus parent context - never the record-level
  // usage/cost/TTFT/preview sections that have no attempt twin. drawerId
  // stays the parent record id throughout, so the row highlight and the
  // debug download-status guard keep their record identity.
  {
    const atts = [
      { status_code: 502, error_type: 'bad_gateway', error_code: 'overloaded',
        error_msg: 'upstream overloaded', retry_after_ms: 1260,
        at: new Date(1700000104500).toISOString(),
        provider_request_id: 'req_meta_1', provider_server: 'srv-a.example',
        provider_model: 'm-upstream', processing_ms: 45,
        rate_limit_remaining: 12, rate_limit_limit: 100,
        response_headers: {'x-request-id': ['req_meta_1'], 'retry-after': ['2']} },
      { status_code: 429, error_type: 'rate_limit', error_msg: 'slow down',
        at: new Date(1700000104800).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-open', 200, 1700000104000), retries: 2, attempts: atts,
      prompt_preview: 'PROMPT-PREVIEW-7F', response_preview: 'RESPONSE-PREVIEW-7F' }, in_flight: 0 });
    await sleep(20);
    w.openDrawer('att-open');
    const title = () => d.getElementById('drawer-title').textContent;
    const bodyText = () => d.getElementById('drawer-body').textContent;
    // typeof guard: pre-fix trees have no drawerAttempt at all, and every row
    // must stay a clean red, never an uncaught ReferenceError.
    const attemptSel = () => w.eval('typeof drawerAttempt === "undefined" ? "absent" : drawerAttempt');
    const links = () => [...d.querySelectorAll('.attempt-link[data-open-req]')];
    const back = () => d.getElementById('drawer-back');
    check('the drawer attempt links carry their attempt index',
      links().length === 2 && links()[0].dataset.attempt === '0' && links()[1].dataset.attempt === '1');
    links()[0].click();
    check('clicking an attempt link opens THAT attempt in the drawer',
      d.getElementById('drawer').classList.contains('open') &&
        title().includes('attempt 1 of 2') && title().includes('502') &&
        title().includes('req_meta_1') && !!back());
    check('the attempt view seats focus on its back affordance', d.activeElement === back());
    const attRow = k => [...d.querySelectorAll('#drawer-body .detail-kv .k')]
      .find(el => el.textContent === k)?.closest('.detail-kv');
    check('the attempt view renders the retained attempt truth',
      attRow('request')?.querySelector('.v')?.textContent === 'att-open' &&
        attRow('attempt')?.querySelector('.v')?.textContent === '1 of 2' &&
        attRow('status')?.querySelector('.v')?.textContent === '502' &&
        !!attRow('when') &&
        attRow('type')?.querySelector('.v')?.textContent === 'bad_gateway' &&
        attRow('code')?.querySelector('.v')?.textContent === 'overloaded' &&
        attRow('message')?.querySelector('.v')?.textContent === 'upstream overloaded' &&
        attRow('retry-after')?.querySelector('.v')?.textContent === '1.26s' &&
        attRow('request id')?.querySelector('.v')?.textContent === 'req_meta_1' &&
        attRow('server')?.querySelector('.v')?.textContent === 'srv-a.example' &&
        attRow('model (upstream)')?.querySelector('.v')?.textContent === 'm-upstream' &&
        attRow('processing')?.querySelector('.v')?.textContent === '45 ms' &&
        attRow('remaining')?.querySelector('.v')?.textContent === '12' &&
        attRow('limit')?.querySelector('.v')?.textContent === '100');
    check('the attempt view renders exactly its own sections',
      JSON.stringify([...d.querySelectorAll('#drawer-body h4')].map(h => h.textContent)) ===
        JSON.stringify(['Attempt', 'Provider', 'Rate limit', 'Response headers']));
    check('the attempt response headers render sorted with the shared row pattern', (() => {
      const sec = [...d.querySelectorAll('#drawer-body .detail-section')]
        .find(s => s.querySelector('h4')?.textContent === 'Response headers');
      return !!sec && [...sec.querySelectorAll('.detail-kv .k')].map(k => k.textContent).join() ===
        'retry-after,x-request-id';
    })());
    check('the record-level sections and previews have no attempt twin and never render',
      !bodyText().includes('PROMPT-PREVIEW-7F') && !bodyText().includes('RESPONSE-PREVIEW-7F') &&
        !bodyText().includes('ttft') && !bodyText().includes('cost') &&
        !bodyText().includes('Tokens'));
    check('the attempt view carries no debug status slot for a stale download to paint',
      !d.getElementById('drawer-debug-status'));
    w.eval("debugDownloadStatus('att-open', 'stale download message')");
    check('a download result cannot paint a stale status into the attempt view',
      !bodyText().includes('stale download message') && attemptSel() === 0);
    // Drivers are guarded so a pre-fix tree stays red row by row, never an
    // uncaught TypeError; each row keeps its own red conjunct.
    const backBtn = back();
    if (backBtn) backBtn.click();
    check('the back affordance returns to the request view with focus on the drawer\'s first control',
      !!backBtn && !back() && !title().includes('attempt') && bodyText().includes('PROMPT-PREVIEW-7F') &&
        d.activeElement === d.querySelector('#drawer .nav-btns button'));
    // The log retry-sub rows open the same attempt mode (their index rides
    // data-attempt).
    d.querySelector('.retry-toggle[data-retry-for="att-open"]').click();
    const sub = () => d.querySelector('tr.retry-sub[data-retry-of="att-open"][data-attempt="1"]');
    check('the retry-sub rows carry their attempt index', !!sub() && !sub().hidden);
    const subRow = sub();
    if (subRow) subRow.click();
    check('clicking a retry-sub row opens that attempt in the drawer',
      title().includes('attempt 2 of 2') && title().includes('429') && attemptSel() === 1);
    if (subRow) { subRow.setAttribute('data-attempt', 'not-a-number'); subRow.click(); }
    check('a malformed attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    // The empty-string spelling: Number('') coerces to 0, which opened
    // attempt mode instead of failing closed - the index must pass the
    // digit-spelling gate first.
    if (subRow) { subRow.setAttribute('data-attempt', ''); subRow.click(); }
    check('an empty-string attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    // The remaining spellings the drawer's digit gate names: Number coerces
    // '0x1' and ' 1 ' to 1, so each must fail closed through the same gate.
    if (subRow) { subRow.setAttribute('data-attempt', '0x1'); subRow.click(); }
    check('a 0x1 attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    if (subRow) { subRow.setAttribute('data-attempt', ' 1 '); subRow.click(); }
    check('a space-padded attempt index fails closed to the request view',
      attemptSel() === null && !title().includes('attempt') &&
        bodyText().includes('PROMPT-PREVIEW-7F'));
    d.querySelector('#tbl-requests tr.exp-row[data-id="att-open"]:not(.retry-sub)').click();
    check('clicking the parent row still opens the parent detail',
      !back() && !title().includes('attempt') && bodyText().includes('PROMPT-PREVIEW-7F'));
    // The selection must survive a wholesale record replacement through the
    // full-resync render: the (id, index) key re-resolves from the fresh
    // record - attempts are append-only, so the grown array must show in the
    // title (a captured attempt object would still say 2).
    links()[0].click();
    fire('snapshot', { feed_id: 'feedB', seq: 6, incremental: false,
      records: [mkRec('fresh1', 200, 1700000100000), mkRec('fresh2', 200, 1700000101000),
        mkRec('fresh3', 200, 1700000102000), mkRec('fresh4', 200, 1700000103000),
        { ...mkRec('sel-live', 200, 1700000104000), duration_ms: 999 },
        { ...mkRec('att-open', 200, 1700000104000), retries: 3,
          attempts: [...atts.map(a => ({ ...a })),
            { status_code: 500, error_type: 'server_error', at: new Date(1700000105100).toISOString() }],
          prompt_preview: 'PROMPT-PREVIEW-7F', response_preview: 'RESPONSE-PREVIEW-7F' }],
      counters: { in_flight: 0, total_requests: 6, total_errors: 0 } });
    await sleep(20);
    check('the attempt selection re-resolves through a wholesale record replacement',
      title().includes('attempt 1 of 3') && title().includes('502') &&
        bodyText().includes('req_meta_1') && attemptSel() === 0 &&
        d.getElementById('drawer').classList.contains('open'));
    w.eval('drawerAttempt = 99');
    w.renderDrawer();
    check('a dangling attempt key fails closed to the parent view',
      !back() && !title().includes('attempt') && bodyText().includes('PROMPT-PREVIEW-7F') &&
        attemptSel() === null);
    links()[0].click();
    check('back in attempt mode for the lifecycle rows', title().includes('attempt 1 of 3'));
    d.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Escape' }));
    check('Escape closes the drawer and clears the attempt mode',
      d.getElementById('drawer').hidden && w.eval('drawerId') === null && attemptSel() === null);
    d.querySelector('.retry-toggle[data-retry-for="att-open"]').click();
    const sub0 = d.querySelector('tr.retry-sub[data-retry-of="att-open"][data-attempt="0"]');
    if (sub0) sub0.click();
    check('a fresh open from a sub-row click lands in attempt mode',
      d.getElementById('drawer').classList.contains('open') && title().includes('attempt 1 of 3'));
    d.querySelector('#drawer .nav-btns button[onclick="drawerNav(1)"]').click();
    check('drawerNav clears the attempt mode',
      !title().includes('attempt') && attemptSel() === null &&
        bodyText().includes('sel-live') && bodyText().includes('999ms'));
    w.closeDrawer();
    check('closing the drawer clears the attempt mode',
      d.getElementById('drawer').hidden && w.eval('drawerId') === null && attemptSel() === null);
  }

  // ---- test 7g: the attempt pill class unification ----
  // A degenerate absorbed attempt - an HTTP 200 that still carried an
  // error payload and was retried - must wear the same pill class on
  // every surface: the class computation is one owner (attemptPillClass),
  // so the drawer's retry list, the attempt view and the log sub-row can
  // never disagree about what a 1xx-3xx failure looks like.
  {
    const atts = [
      { status_code: 200, error_type: 'upstream_error', error_msg: 'degenerate 200 with an error payload',
        at: new Date(1700000104500).toISOString() },
    ];
    fire('end', { record: { ...mkRec('att-200', 200, 1700000104000), retries: 1, attempts: atts }, in_flight: 0 });
    await sleep(20);
    w.openDrawer('att-200');
    const listPill = d.querySelector('.attempt-link .pill');
    check('the drawer retry list classes the degenerate 200 attempt warn',
      listPill.classList.contains('warn') && !listPill.classList.contains('err'));
    d.querySelector('.attempt-link').click();
    const viewPill = [...d.querySelectorAll('#drawer-body .detail-kv')]
      .find(el => el.querySelector('.k')?.textContent === 'status')?.querySelector('.v .pill');
    check('the attempt view status pill wears the same class',
      viewPill && viewPill.classList.contains('warn') && !viewPill.classList.contains('err'));
    w.closeDrawer();
    d.querySelector('.retry-toggle[data-retry-for="att-200"]').click();
    const subPill = d.querySelector('tr.retry-sub[data-retry-of="att-200"] .pill');
    check('the log sub-row pill wears the same shared class',
      subPill && subPill.classList.contains('warn') && !subPill.classList.contains('err'));
  }


  // ---- test 8: Logs menu (filter menu + export URL) ----
  const logsMenu = d.getElementById('logs-menu');
  const clearMenu = d.getElementById('clear-menu');
  check('logs menu starts hidden', logsMenu && logsMenu.hidden);
  w.toggleLogsMenu({ stopPropagation() {} });
  check('logs menu opens', !logsMenu.hidden);
  check('logs open sets aria-expanded', d.getElementById('btn-logs').getAttribute('aria-expanded') === 'true');
  check('opening logs closes clear', clearMenu.hidden);
  check('logs menu populated from records', d.querySelector('#lf-provider option[value="p"]') !== null);
  // ---- R25 L4-1 freeze row: the export has been a gzip artifact with no
  // plain-JSON fallback since wave 17, so the menu header must say so.
  // Exact-text pin (like the debug menu header's) so the pre-compression
  // "(JSON)" label cannot return.
  check('logs menu header states the gzip export format',
    d.querySelector('#logs-menu .clear-menu-hd').textContent === 'Download logs (JSON, gzip)…');
  // ---- L4-C8 pin: the Logs (lf-) and Clear (cf-) filter menus are one row
  // set modulo the id prefix. The static HTML row lists must stay identical
  // (same label texts, same per-row select ids after the prefix), and the
  // runtime fill (populateFilterMenu, already prefix-parameterized) must
  // write the same option values into both menus from one record set.
  {
    const skelDoc = new JSDOM(fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8')).window.document;
    const filterRows = menu => [...menu.querySelectorAll('.clear-opts label')].map(l => [
      l.textContent.replace(/\s+/g, ' ').trim(),
      l.querySelector('select').id.replace(/^[cl]f-/, ''),
    ]);
    const lfRows = filterRows(skelDoc.getElementById('logs-menu'));
    const cfRows = filterRows(skelDoc.getElementById('clear-menu'));
    check('the logs and clear menus render one prefix-identical filter row set',
      lfRows.length === 7 && JSON.stringify(lfRows) === JSON.stringify(cfRows));
    w.populateFilterMenu('cf');
    const optValues = pre => ['provider', 'model', 'client', 'status', 'errors', 'debug', 'age']
      .map(name => [...d.getElementById(pre + '-' + name).options].map(o => o.value));
    check('both filter menus fill the same option values per row',
      JSON.stringify(optValues('lf')) === JSON.stringify(optValues('cf')));
  }
  check('export URL encodes the filter', w.logsExportURL({ provider: 'openai', status_code: 429, has_error: false, debug: false, before_ms: 0, model: '', client: '' }) === '/metrics/export?provider=openai&status_code=429');
  check('empty filter exports everything', w.logsExportURL({ provider: '', model: '', client: '', status_code: 0, has_error: false, debug: false, before_ms: 0 }) === '/metrics/export');
  check('debug-only export encodes the flag', w.logsExportURL({ provider: '', model: '', client: '', status_code: 0, has_error: false, debug: true, before_ms: 0 }) === '/metrics/export?debug=1');
  d.getElementById('lf-provider').value = 'p';
  w.updateLogsCount();
  await sleep(30);
  check('logs count preview reads the backend count', d.getElementById('logs-count').textContent.includes('download 3 records'));
  w.toggleLogsMenu({ stopPropagation() {} });
  check('logs menu closes on second toggle', logsMenu.hidden);
  check('logs close clears aria-expanded', d.getElementById('btn-logs').getAttribute('aria-expanded') === 'false');
  // equal-size icon buttons: all five header actions share one min-width
  const hdrIds = ['btn-pause', 'btn-debug', 'btn-limits', 'btn-logs', 'btn-clear', 'btn-settings'];
  const bts = hdrIds.map(id => d.getElementById(id));
  const styles = bts.map(b => w.getComputedStyle(b).minWidth);
  check('header action buttons share one min-width', styles.every(s => s === styles[0] && s) && styles[0] !== 'auto');
  check('header actions are icon-only', bts.every(b => b && b.querySelector('svg.hdr-ico') && b.getAttribute('aria-label')));
  check('header hamburger exists', !!d.getElementById('btn-nav') && d.getElementById('btn-nav').getAttribute('aria-controls') === 'hdr-actions');
  check('header hamburger is hidden on desktop', w.getComputedStyle(d.getElementById('btn-nav')).display === 'none');
  check('header action labels are present for the mobile panel', bts.every(b => b.querySelector('.hdr-btn-label')));
  w.refreshFooterState();
  check('pause icon survives pressed-state refresh', !!d.getElementById('btn-pause').querySelector('svg.hdr-ico'));
  check('debug icon survives pressed-state refresh', !!d.getElementById('btn-debug').querySelector('svg.hdr-ico'));
  const debugMenu = d.getElementById('debug-menu');
  check('debug menu starts hidden', debugMenu && debugMenu.hidden);
  w.toggleDebugMenu({ stopPropagation() {} });
  check('debug menu opens', !debugMenu.hidden);
  check('debug open sets aria-expanded', d.getElementById('btn-debug').getAttribute('aria-expanded') === 'true');
  check('opening debug closes logs', logsMenu.hidden);
  w.toggleDebugMenu({ stopPropagation() {} });
  check('debug menu closes on second toggle', debugMenu.hidden);
  // Stamped-first debug priority: a request row's dbg pill must edit the
  // session the server STAMPED on the record (debug_session_id), even when
  // another live session's scope would also match the record. The stamp is
  // the authoritative match; the raw-scope heuristic only serves records
  // the stamp does not reach.
  fire('record', { ...mkRec('dbg-stamped-rec'), debug: true, debug_session_id: 'dbg-stamped' }, '5');
  await sleep(20); // the record row paints on the coalesced live render
  check('a stamped debug record renders the dbg pill on its row',
    !!d.querySelector('#tbl-requests tr.exp-row[data-id="dbg-stamped-rec"] .pill.debug[data-edit-debug]'));
  w.eval("debugState = {...debugState, enabled: true, sessions: [" +
    "{id: 'dbg-stamped', clients: ['other-client'], duration: '15m'}," +
    "{id: 'dbg-scope', clients: ['c'], duration: '1h'}]}");
  d.querySelector('#tbl-requests tr.exp-row[data-id="dbg-stamped-rec"] .pill.debug[data-edit-debug]').click();
  {
    const holds = [...d.querySelectorAll('#debug-menu .pause-hold')];
    const editing = holds.filter(h => h.classList.contains('editing'));
    check('the dbg pill edits the stamped session, not a scope-matching one',
      !debugMenu.hidden && holds.length === 2 && editing.length === 1 &&
        editing[0].querySelector('[data-operator="debug-edit"]').dataset.value === 'dbg-stamped' &&
        holds.some(h => !h.classList.contains('editing') &&
          h.querySelector('[data-operator="debug-edit"]').dataset.value === 'dbg-scope') &&
        d.querySelector('#debug-menu .clear-menu-hd').textContent === 'Edit session…');
  }
  w.resetDebugMenuForm();
  w.toggleDebugMenu({ stopPropagation() {} });
  w.eval("debugState = {...debugState, enabled: false, sessions: []}");
  check('debug menu closes after the stamped edit', debugMenu.hidden);
  // GAP-A pin: the pill-edit table's paused and throttled arms. The debug
  // arm is pinned above; these two had zero coverage (mutation-proven).
  // Same contract per arm: a live record carrying the server's state flag
  // renders its state pill, and the pill opens the matching operator editor
  // - the pause menu editing the hold matched via holdMatchesRecord (not
  // the all-requests fallback, not a non-matching named hold), the limits
  // menu editing the record's own provider among several.
  {
    const pausedRec = { ...mkRec('paused-live-rec', 0, 1700000105000), paused: true };
    fire('begin', { record: pausedRec, in_flight: 1 });
    await sleep(20);
    const pausedPill = () => d.querySelector('#tbl-requests tr.exp-row[data-id="paused-live-rec"] .pill.paused[data-edit-pause]');
    check('a paused live record renders the paused pill on its row', !!pausedPill());
    w.eval("pauseState = {...pauseState, paused: true, holds: [" +
      "{id: 'hold-other', clients: ['other-client'], duration: '1h'}," +
      "{id: 'hold-named', clients: ['c'], duration: '15m'}," +
      "{id: 'hold-all', all: true, duration: '6h'}]}");
    pausedPill().click();
    {
      const pauseMenu = d.getElementById('pause-menu');
      const holds = [...pauseMenu.querySelectorAll('.pause-hold')];
      const editing = holds.filter(h => h.classList.contains('editing'));
      check('the paused pill edits the hold matched via holdMatchesRecord',
        !pauseMenu.hidden && holds.length === 3 && editing.length === 1 &&
          editing[0].querySelector('[data-operator="pause-edit"]').dataset.value === 'hold-named');
    }
    w.resetPauseMenuForm();
    w.hideHdrMenu('pause-menu');
    w.eval("pauseState = {...pauseState, paused: false, holds: []}");

    const throttledRec = { ...mkRec('throttled-live-rec', 0, 1700000106000), throttled: true };
    fire('begin', { record: throttledRec, in_flight: 2 });
    await sleep(20);
    const throttledPill = () => d.querySelector('#tbl-requests tr.exp-row[data-id="throttled-live-rec"] .pill.throttled[data-edit-limit]');
    check('a throttled live record renders the throttled pill on its row', !!throttledPill());
    w.eval("throttleState = {...throttleState, active: true, throttles: [" +
      "{provider: 'other.example', requests: 1, request_window: '1m'}," +
      "{provider: 'p', requests: 2, request_window: '2m'}]," +
      "known_providers: ['p', 'other.example']}");
    throttledPill().click();
    {
      const limitsMenu = d.getElementById('limits-menu');
      const holds = [...limitsMenu.querySelectorAll('#lim-holds .pause-hold')];
      const editing = holds.filter(h => h.classList.contains('editing'));
      check('the throttled pill opens the limits editor for the record provider',
        !limitsMenu.hidden && d.getElementById('lim-provider').value === 'p' &&
          holds.length === 2 && editing.length === 1 &&
          editing[0].querySelector('[data-operator="limit-edit"]').dataset.value === 'p');
    }
    w.hideHdrMenu('limits-menu');
    w.eval("throttleState = {...throttleState, active: false, throttles: []}");
  }
  // GAP-C pin: resyncOperatorSurfaces' live-repaint tail. The menu-sync
  // half is pinned (the count-bits checks); `if (lastData)
  // scheduleRenderLive()` is not. The row renderer detects change by record
  // identity, so the tail's contract: after a wholesale record replacement
  // in lastData (what upsert does) with the derivation invalidated but NO
  // render scheduled and no feed event, applying operator state repaints
  // the stale row from the fresh record.
  {
    const liveRec = { ...mkRec('gapc-live-rec', 0, 1700000107000) };
    fire('begin', { record: liveRec, in_flight: 1 });
    await sleep(20);
    const gapcPill = () => d.querySelector('#tbl-requests tr.exp-row[data-id="gapc-live-rec"] .pill');
    check('the in-flight row paints the live pill first', !!gapcPill() && gapcPill().classList.contains('live'));
    w.eval("lastData.records[lastData.records.findIndex(r => r.id === 'gapc-live-rec')]" +
      " = {...lastData.records.find(r => r.id === 'gapc-live-rec'), paused: true}; bumpData();");
    check('no feed event leaves the painted row stale', gapcPill().classList.contains('live'));
    let livePaints = 0;
    const originalLive = w.renderLive;
    w.renderLive = () => { livePaints++; originalLive(); };
    w.applyPauseState({ ok: true, paused: true, holds: [{ id: 'gapc-hold', all: true, duration: '15m' }],
      known_clients: ['c'], known_providers: ['p'], until: null, queued: 0, default_max_queued: 0 });
    await sleep(20);
    w.renderLive = originalLive;
    check('applying a pause state repaints the held row without a feed event',
      livePaints === 1 &&
        !!d.querySelector('#tbl-requests tr.exp-row[data-id="gapc-live-rec"] .pill.paused[data-edit-pause]'));
    w.eval("pauseState = {...pauseState, paused: false, holds: []}");
  }
  const canonCases = [
    ['glm-5.3', 'glm-5-3'], ['glm-5-3', 'glm-5-3'], ['GLM-5.3', 'glm-5-3'],
    ['moonshotai/kimi-k3', 'kimi-k3'], ['moonshotai/kimi-k3:nube', 'kimi-k3'],
    ['z-ai/glm-5.3-flash:crofai', 'glm-5-3-flash'],
    ['deepseek-v4-pro-0813', 'deepseek-v4-pro-0813'],
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
  // Debug model checklist: spelling variants of the same model must render as
  // ONE checkbox (grouped display; a session stores the raw spellings and the
  // server matches them canonically through the cursor base name), and the
  // models box must share the clients/providers structure - nesting
  // it under .clear-opts used to stack the checkbox ABOVE the label (the
  // .clear-opts label rule out-specifies .pause-check).
  const dfm = d.getElementById('df-models');
  check('models checklist sits in the pause-known structure', !!dfm.closest('.pause-known') && !dfm.closest('.clear-opts'));
  w.eval("debugState.known_models = ['glm-5-3','glm-5.3','glm-5-3-flash','glm-5.3-flash','kimi-k3','moonshotai/kimi-k3','moonshotai/kimi-k3:nube','deepseek-v4-pro','deepseek-v4-pro-0813','grok-4.6']");
  w.fillDebugChecks(dfm, w.eval('debugState.known_models').slice().sort(), new Set(), 'model');
  const mboxes = [...dfm.querySelectorAll('input[type=checkbox]')];
  check('spelling variants merge into one checkbox each',
    mboxes.length === 6 && mboxes.every(b => ['deepseek-v4-pro','deepseek-v4-pro-0813','glm-5-3','glm-5-3-flash','grok-4.6','kimi-k3'].includes(b.value)));
  check('a multi-variant group shows the bare spelling', !!dfm.querySelector('input[value="kimi-k3"]'));
  dfm.querySelector('input[value="kimi-k3"]').checked = true;
  const expanded = w.selectedDebugModels();
  check('checking a group expands to every raw spelling',
    expanded.length === 3 && expanded.includes('kimi-k3') && expanded.includes('moonshotai/kimi-k3') && expanded.includes('moonshotai/kimi-k3:nube'));
  // The picked Set must come from the WINDOW realm (fillDebugChecks checks
  // `picked instanceof Set` - a Node-realm Set is cross-realm and fails it).
  w.fillDebugChecks(dfm, w.eval('debugState.known_models').slice().sort(), w.eval("new Set(['glm-5.3'])"), 'model');
  check('editing a session with a raw spelling re-checks its group', dfm.querySelector('input[value="glm-5-3"]').checked);
  w.eval("debugState.known_models = ['m']");

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
  w.applyModelCanon(modelFixture([{mode: 'exact', from: 'deepseek-v4-pro-0813', to: 'deepseek-v4-pro'}].concat(DEF_RULES), {...canonNames, 'deepseek-v4-pro-0813': 'deepseek-v4-pro'}), true);
  check('an exact rule merges what the generic pipeline cannot',
    w.canonicalModel('deepseek-v4-pro-0813') === 'deepseek-v4-pro' &&
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
  // The debug checklist re-groups through the SAME rules: with an exact
  // rule live, the two deepseek spellings collapse into one checkbox.
  w.applyModelCanon(modelFixture([{mode: 'exact', from: 'deepseek-v4-pro-0813', to: 'deepseek-v4-pro'}].concat(DEF_RULES), {...canonNames, 'deepseek-v4-pro-0813': 'deepseek-v4-pro'}), true);
  w.eval("debugState.known_models = ['deepseek-v4-pro', 'deepseek-v4-pro-0813', 'grok-4.6']");
  w.fillDebugChecks(dfm, w.eval('debugState.known_models').slice().sort(), new Set(), 'model');
  check('a config rule re-groups the debug checklist',
    dfm.querySelectorAll('input[type=checkbox]').length === 2);
  w.applyModelCanon(modelFixture(DEF_RULES, canonNames), true);
  w.eval("debugState.known_models = ['m']");
  // Clear/Logs model select: spelling variants cluster under a canonical
  // <optgroup>, but every <option> keeps its RAW value (exact delete).
  const modelSelHTML = w.eval(`(() => {
    const saved = lastData;
    lastData = { records: [{ model: 'glm-5.3' }, { model: 'glm-5-3' }, { model: 'grok-4.6' }] };
    populateFilterMenu('cf');
    const html = document.getElementById('cf-model').innerHTML;
    lastData = saved;
    return html;
  })()`);
  check('clear-menu model options cluster variants under canonical optgroups',
    modelSelHTML.includes('<optgroup label="glm-5-3 (2)"') &&
    modelSelHTML.includes('value="glm-5.3"') && modelSelHTML.includes('value="glm-5-3"') &&
    modelSelHTML.includes('value="grok-4.6"') && !modelSelHTML.includes('value="glm-5-3 (2)"'));
  // applyModelCanon: a rule change invalidates the scope derivation and
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
      tpl.options.length === 8 && tplLabels === 'add from template…,lowercase fold,strip vendor/ prefix,strip :tag suffix,strip architecture/quant suffix (fp4, nvfp4, int8, q4_k_m…),unify . and - between digits,exact merge…,custom pattern…');
    tpl.value = '1';
    tpl.dispatchEvent(new w.Event('change', { bubbles: true }));
    const rows = [...wrap.querySelectorAll('.mr-row')];
    check('a template appends its pre-filled rule',
      rows.length === 2 && rows[1].querySelector('.mr-from').value === '^[a-z0-9][a-z0-9._-]*/');
    check('a shipped template carries the server doc payload, not a client copy',
      rows[1].querySelector('.mr-mode').value === 'pattern' && rows[1].querySelector('.mr-to').value === '' &&
      JSON.stringify(w.eval('settingsDoc.defaults.model_rules[1]')) === JSON.stringify({ mode: 'pattern', from: '^[a-z0-9][a-z0-9._-]*/', to: '' }));
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
  check('settings close matches drawer', (d.getElementById('btn-settings-close') || {}).textContent.includes('esc'));
  {
    const doc = {
      writable: true,
      fields: [{ key: 'backup_max_bytes', category: 'backup', label: 'Backup size cap', kind: 'bytes', help: 'cap', hot_reload: true }],
      categories: [{ id: 'backup', label: 'Backup', help: 'archives' }],
      values: { backup_max_bytes: '1GiB' },
      defaults: { backup_max_bytes: '1GiB' },
      overrides: {},
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
            config: { present: true, bytes: 2048, values: { backup_max_bytes: '2GiB' }, modified: ['backup_max_bytes'], vs_live: ['backup_max_bytes'] },
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
    await sleep(20);
    check('backup download uses the operator fetch gate',
      calls.some(c => c.u.includes('/admin/backup') && c.auth === 'Bearer op-token'));
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
    await sleep(20);
    check('an empty-body 502 backup download surfaces the designed fallback message',
      d.getElementById('settings-count').textContent === 'backup failed');
    calls.length = 0;
    const inspectPayload = {
      ok: true, created: '2026-09-07T12:00:00Z',
      config: { present: true, bytes: 2048, values: { backup_max_bytes: '2GiB' }, modified: ['backup_max_bytes'], vs_live: ['backup_max_bytes'] },
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
    await sleep(0);
    check('inspect holds the busy gate until the archive is reviewed',
      d.getElementById('btn-backup-download')?.dataset.busy === '1' &&
      d.getElementById('btn-backup-restore')?.dataset.busy === '1' &&
      !d.getElementById('btn-backup-apply'));
    w.fillSettingsForm(doc);
    check('settings refresh during inspect keeps the busy gate',
      d.getElementById('btn-backup-download')?.dataset.busy === '1' &&
      d.getElementById('btn-backup-restore')?.dataset.busy === '1' &&
      !d.getElementById('btn-backup-apply'));
    releaseInspect();
    await sleep(20);
    check('backup inspect uses the operator fetch gate',
      calls.some(c => c.u.includes('/admin/restore') && c.u.includes('inspect=1') && c.auth === 'Bearer op-token'));
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
    await sleep(20);
    check('backup apply uses the operator fetch gate',
      calls.some(c => c.u.includes('/admin/restore') && !c.u.includes('inspect=1') && c.auth === 'Bearer op-token'));
    check('apply omits an unchecked config member',
      calls.some(c => c.u.includes('/admin/restore') && !c.u.includes('inspect=1') && c.u.includes('database=1') && !c.u.includes('config=1')));
    check('restore refreshes settings even when a database restart is pending',
      calls.some(c => c.u.includes('/admin/config') && c.method === 'GET'));
    w.fetch = originalFetch;
    w.HTMLAnchorElement.prototype.click = origClick;
    w.eval("operatorCredential = ''; backupInspect = null; backupIncludeConfig = true; backupIncludeDatabase = true; backupMode = 'replace'; backupBusy = false; backupReq = 0;");
  }


  w.applyModelCanon(modelFixture(), true);
  // ---- test 9: menu action rows (split ends alignment + red all-actions) ----
  w.toggleLogsMenu({ stopPropagation() {} });
  const js = w.getComputedStyle(d.querySelector('#logs-menu .clear-actions'));
  check('menu action row splits ends-wise', js.justifyContent === 'space-between');
  const dlAll = [...d.querySelectorAll('#logs-menu .clear-actions button')].find(b => b.textContent.includes('Download all'));
  const delAll = [...d.querySelectorAll('#clear-menu .clear-actions button')].find(b => b.textContent.includes('Delete everything'));
  check('download all is the red-outline variant', dlAll && dlAll.classList.contains('btn-danger-outline'));
  check('delete everything is the solid-red variant', delAll && delAll.classList.contains('btn-danger-solid'));
  w.toggleLogsMenu({ stopPropagation() {} });

  // ---- test 10: infinite-scroll grows the log window, then pages the store ----
  const BIG = Array.from({ length: 80 }, (_, i) => mkRec('big' + String(i).padStart(3, '0'), 200, 1800000000000 + i * 1000));
  fire('snapshot', { feed_id: 'feedB', seq: 80, incremental: false,
    records: BIG, counters: { in_flight: 0, total_requests: 80, total_errors: 0 } });
  await sleep(30);
  check('first paint caps at dash_log_rows (60 of 80)', rows().length === 60);
  check('newest row is on top', rows()[0].dataset.id === 'big079');
  check('f-stats shows window of loaded', d.getElementById('f-stats').textContent === '60 of 80 records');
  {
    const logBox = d.getElementById('tbl-requests');
    let inserted = false, lateScrollReset = false, top = 200;
    const originalUpdate = w.updateSection;
    Object.defineProperty(logBox, 'scrollTop', { configurable: true,
      get: () => top,
      set(value) { if (inserted) lateScrollReset = true; top = value; },
    });
    const rowCache = w.eval('logHTML');
    const cachedRows = [...rowCache];
    rowCache.clear();
    w.resetLogWindow();
    check('first boot does not force layout by resetting an untouched log', top === 200);
    for (const [id, value] of cachedRows) rowCache.set(id, value);
    w.updateSection = (id, html) => {
      if (id === 'tbl-requests') inserted = true;
      return originalUpdate(id, html);
    };
    w.renderAll(w.eval('lastData'));
    check('full render resets scroll before inserting the table, never after it',
      inserted && top === 0 && !lateScrollReset);
    w.updateSection = originalUpdate;
    delete logBox.scrollTop;
    // Drain the real queued fill before checking the shared coalescing gate.
    await sleep(30);
    const originalRAF = w.requestAnimationFrame;
    const originalFill = w.maybeLoadMoreLog;
    let nextFrame, scheduled = 0, probes = 0;
    w.requestAnimationFrame = callback => { scheduled++; nextFrame = callback; return 1; };
    w.maybeLoadMoreLog = () => { probes++; };
    w.scheduleLogFill();
    w.scheduleLogFill();
    logBox.dispatchEvent(new w.Event('scroll'));
    check('boot, append, and scroll share one deferred log fill probe', scheduled === 1 && probes === 0);
    nextFrame();
    check('the coalesced fill probe runs once on the frame', probes === 1);
    w.requestAnimationFrame = originalRAF;
    w.maybeLoadMoreLog = originalFill;
  }
  const pinned = rows()[0];
  logPage = {
    records: [
      mkRec('old004', 200, 1700000004000),
      mkRec('old003', 200, 1700000003000),
      mkRec('old002', 200, 1700000002000),
      mkRec('old001', 200, 1700000001000),
      mkRec('old000', 200, 1700000000000),
    ],
    more: false, cursor_ms: 1700000000000,
  };
  const box = d.getElementById('tbl-requests');
  Object.defineProperty(box, 'clientHeight', { configurable: true, get() { return 200; } });
  Object.defineProperty(box, 'scrollHeight', { configurable: true, get() { return 4000; } });
  box.scrollTop = 4000 - 200 - 10;
  lastLogURL = '';
  w.maybeLoadMoreLog();
  await sleep(40);
  const idsAfter = rows().map(tr => tr.dataset.id);
  check('near-bottom grows through the ring then appends store pages', rows().length === 85);
  check('scroll-load keeps existing row nodes', rows().includes(pinned));
  check('older store rows land at the bottom', idsAfter[idsAfter.length - 1] === 'old000');
  check('first log archive page has no cutoff derived from ring arrivals', !lastLogURL.includes('before_ms='));
  check('f-stats after fill lists loaded count', d.getElementById('f-stats').textContent === '85 records');
  w.openDrawer('old000');
  check('drawer opens a store-paged row', d.getElementById('drawer').classList.contains('open'));
  fire('record', { ...mkRec('bignew', 200, 1800000000000 + 80 * 1000) }, '81');
  await sleep(20);
  check('live insert stays at top after paging', rows()[0].dataset.id === 'bignew');
  check('paged oldest row is not evicted by a live insert', rows().some(tr => tr.dataset.id === 'old000'));
  box.scrollTop = 0; // full rebuild resets scroll (jsdom keeps the stubbed top)
  fire('snapshot', { feed_id: 'feedB', seq: 81, incremental: false,
    records: BIG, counters: { in_flight: 0, total_requests: 80, total_errors: 0 } });
  await sleep(30);
  check('full resync after paging resets to page size', rows().length === 60);
  check('resync drops store-paged rows', !rows().some(tr => tr.dataset.id === 'old000'));

  // Clear/Logs preview ownership and failed mutations: exercise each actual
  // handler, with controlled out-of-order requests and a fixed preview cutoff.
  {
    const savedFetch = w.fetch;
    w.populateFilterMenu('cf');
    w.populateFilterMenu('lf');
    d.getElementById('cf-provider').value = 'p';
    let replyCount;
    w.fetch = () => new Promise(resolve => { replyCount = resolve; });
    const oldCount = w.updateClearCount();
    check('delete matching remains disabled while count is pending', d.getElementById('btn-clear-filtered').disabled);
    d.getElementById('cf-provider').value = '';
    await w.updateClearCount();
    replyCount({ok: true, json: async () => ({count: 99})});
    await oldCount;
    check('an old count cannot overwrite an empty selection or re-enable delete',
      d.getElementById('btn-clear-filtered').disabled && d.getElementById('clear-count').textContent === '');

    check('Logs/Clear age options use duration tokens',
      ['cf', 'lf'].every(prefix => {
        const el = d.getElementById(prefix + '-age');
        return !!el.querySelector('option[value="1h"]') &&
          !!el.querySelector('option[value="24h"]') &&
          !!el.querySelector('option[value="168h"]') &&
          !el.querySelector('option[value="3600000"]') &&
          !el.querySelector('option[value="86400000"]') &&
          !el.querySelector('option[value="604800000"]') &&
          !el.querySelector('option[value="7d"]');
      }));
    d.getElementById('cf-age').value = '1h';
    d.getElementById('lf-age').value = '1h';
    const pendingCounts = [];
    w.fetch = () => new Promise(resolve => pendingCounts.push(resolve));
    const clearCount = w.updateClearCount(), logsCount = w.updateLogsCount();
    pendingCounts[1]({ok: true, json: async () => ({count: 4})});
    pendingCounts[0]({ok: true, json: async () => ({count: 3})});
    await Promise.all([clearCount, logsCount]);
    check('Logs and Clear counts do not supersede each other',
      d.getElementById('clear-count').textContent.includes('3 records') && d.getElementById('logs-count').textContent.includes('4 records'));
    const preview = w.previewedFilter('cf');
    const selectedRows = rows().slice();
    d.getElementById('clear-menu').hidden = false;
    let submitted, resyncs = 0;
    w.fetch = async (url, options) => {
      if (String(url).includes('/admin/purge')) {
        submitted = JSON.parse(options.body);
        return {ok: false, json: async () => ({error: 'delete rejected'})};
      }
      resyncs++;
      return savedFetch(url, options);
    };
    await sleep(5);
    await w.clearFiltered();
    check('filtered deletion uses the previewed cutoff, not a later time', submitted.before_ms === preview.filter.before_ms);
    check('failed deletion reports its error without wiping or resyncing the log', resyncs === 0 &&
      !d.getElementById('clear-menu').hidden && d.getElementById('clear-count').textContent === 'delete rejected' &&
      rows().every((row, i) => row === selectedRows[i]));
    let replyPurge, posts = 0;
    w.fetch = () => { posts++; return new Promise(resolve => { replyPurge = resolve; }); };
    const first = w.clearAll();
    w.clearAll();
    w.clearFiltered();
    check('all destructive actions share one in-flight gate', posts === 1 &&
      d.getElementById('btn-clear-all').disabled && d.getElementById('btn-clear-filtered').disabled);
    replyPurge({ok: false, json: async () => ({error: 'still rejected'})});
    await first;
    d.getElementById('cf-age').value = '';
    d.getElementById('lf-age').value = '';
    d.getElementById('lf-provider').value = '';
    w.fetch = savedFetch;
    await w.updateClearCount();
    await w.updateLogsCount();
    d.getElementById('clear-menu').hidden = true;
  }

  // Durable pagination is independent of the ring, including timestamp ties,
  // empty-ring starts and duplicate-only pages. No page may stall the cursor.
  {
    const savedFetch = w.fetch, savedFill = w.scheduleLogFill;
    w.scheduleLogFill = () => {};
    w.resetLogWindow();
    w.eval('logLimit = lastData.records.length'); // real paging starts after the ring window is exhausted
    let pageRequest = 0;
    const urls = [];
    w.fetch = async url => {
      urls.push(String(url));
      const payload = pageRequest++ === 0
        ? {records: [BIG[79]], more: true, cursor_ms: 0, cursor_id: 'tie-z'}
        : {records: [mkRec('tie-b', 200, 0), mkRec('tie-a', 200, 0)], more: false, cursor_ms: 0, cursor_id: 'tie-a'};
      return {ok: true, json: async () => payload};
    };
    w.fetchOlderLog();
    await sleep(10);
    check('a duplicate-only durable page advances without ending pagination', !w.eval('logExhausted') && w.eval('logCursor.id') === 'tie-z');
    w.fetchOlderLog();
    await sleep(10);
    check('log keyset sends both timestamp zero and its tie-break ID', urls[1].includes('before_ms=0') && urls[1].includes('before_id=tie-z'));
    check('same-timestamp archive records retain the server tie order',
      w.logVisible().filter(r => r.id.startsWith('tie-')).map(r => r.id).join(',') === 'tie-b,tie-a');
    const snapshot = w.eval('lastData');
    w.eval('lastData = {records: [], counters: {}}');
    w.resetLogWindow();
    pageRequest = 1;
    w.fetchOlderLog();
    await sleep(10);
    check('an entirely empty ring still loads durable request history', w.eval('logArchive.length') === 2 && !urls[2].includes('before_ms='));
    w.eval('lastData = null');
    w.renderAll(snapshot);
    w.fetch = savedFetch;
    w.scheduleLogFill = savedFill;
  }

  // ---- test 10b: the purge re-sync - no removal event exists, so clearAll
  // must fetch a CURSOR-LESS bootstrap (mode 'full': a resume could never see
  // the deletions) and the empty post-purge snapshot IS the wipe render.
  fullPayload = { feed_id: 'feedB', seq: 0, records: [],
    counters: { in_flight: 0, total_requests: 0, total_errors: 0 } };
  let purgeURL = '';
  const origPurge = w.fetch;
  w.fetch = (url, opts) => {
    const u = String(url);
    if (u.includes('/metrics/bootstrap')) { bootURL = u; return origPurge(url, opts); }
    if (u.includes('/admin/purge') && !u.includes('/count')) { purgePOST = true; }
    return origPurge(url, opts);
  };
  let purgePOST = false;
  await w.clearAll();
  await sleep(30);
  w.fetch = origPurge;
  check('clearAll POSTs the purge endpoint', purgePOST === true);
  check('purge resync bootstrap is cursor-less (no since/feed)',
    bootURL.includes('/metrics/bootstrap') && !bootURL.includes('since=') && !bootURL.includes('feed='));
  check('the empty post-purge snapshot IS the wipe render', rows().length === 0);
  check('purge resets the log window state', w.eval('logArchive.length') === 0 && w.eval('logLimit') === 0);
  // Restore a live view so the later suites run against real state.
  fullPayload = { feed_id: 'feedB', seq: 80,
    records: BIG.map((r, i) => ({ ...r, __seq: i + 1 })),
    counters: { in_flight: 0, total_requests: 80, total_errors: 0 } };
  w.eval("fetchBootstrap('full')");
  await sleep(30);
  check('post-purge resync restores the fresh snapshot', rows().length === 60);

  // ---- test 10c: a scope whose records predate the ring (a provider the
  // user hasn't touched in a while) must page the store from its NEWEST rows -
  // the empty scoped window probes the durable newest edge without a ring
  // timestamp cutoff instead of dead-ending on "no matching requests".
  logPage = {
    records: [
      { ...mkRec('stale1', 200, 1600000000000), provider: 'stale.example' },
      { ...mkRec('stale0', 200, 1599999999000), provider: 'stale.example' },
    ],
    more: false, cursor_ms: 1599999999000,
  };
  // empty box: content does not fill it → "near bottom" is true, like a real
  // browser with a zero-row table
  Object.defineProperty(box, 'clientHeight', { configurable: true, get() { return 200; } });
  Object.defineProperty(box, 'scrollHeight', { configurable: true, get() { return 200; } });
  w.navigateTo([{ dim: 'provider', id: 'stale.example' }]);
  await sleep(50);
  check('stale-provider scope probes the store at the newest edge',
    !lastLogURL.includes('before_ms=') && lastLogURL.includes('f=provider%3Astale.example'));
  check('stale-provider rows paint instead of the empty state',
    rows().length === 2 && rows()[0].dataset.id === 'stale1' && rows()[1].dataset.id === 'stale0');
  check('stale-provider fill updates the log stats', d.getElementById('f-stats').textContent === '2 records');
  check('stale-provider scope line comes from the server count',
    d.getElementById('f-scope').textContent.includes('of'));

  // A page that resolves after a window reset belongs to the OLD scope and
  // must be dropped - never merged into the new one.
  let gateResolve = null;
  logPageGate = new Promise(res => { gateResolve = res; });
  w.navigateTo([{ dim: 'provider', id: 'other.example' }]); // resetLogWindow → fetch fires, held by the gate
  await sleep(50);
  logPage = { records: [{ ...mkRec('other1', 200, 1599999980000), provider: 'other.example' }], more: false, cursor_ms: 1599999980000 };
  gateResolve(); // both held pages resolve - only the current scope's may land
  await sleep(50);
  check('stale in-flight page is dropped after a scope change',
    rows().length === 1 && rows()[0].dataset.id === 'other1' && w.eval('logArchive.length') === 1);
  logPageGate = null;
  // Restore the tall-box stubs (test 10's geometry) before leaving the suite.
  Object.defineProperty(box, 'scrollHeight', { configurable: true, get() { return 4000; } });
  w.navigateTo([]);
  await sleep(50);
  check('clearing the scope restores the unfiltered window', rows().length === 60);

  // ---- test 10c-bis: per-view history rows ----
  // A selected view owns its durable history rows: the scoped window must
  // page the store under its own scope and render exactly its own rows (its
  // scoped ring rows plus its archive page, never the default view's rows),
  // and a view's history must not leak back into the default window.
  {
    const mixed = [
      mkRec('p-row0', 200, 1700000000000),
      { ...mkRec('sa-ring0', 200, 1700000100000), provider: 'scope-a.example' },
      mkRec('p-row1', 200, 1700000200000),
      { ...mkRec('sa-ring1', 200, 1700000300000), provider: 'scope-a.example' },
      mkRec('p-row2', 200, 1700000400000),
      { ...mkRec('sa-ring2', 200, 1700000500000), provider: 'scope-a.example' },
      mkRec('p-row3', 200, 1700000600000),
    ];
    fire('snapshot', { feed_id: 'feedB', seq: 90, incremental: false,
      records: mixed, counters: { in_flight: 0, total_requests: 7, total_errors: 0 } });
    await sleep(30);
    check('the mixed ring paints the default window', rows().length === 7);
    logPage = {
      records: [
        { ...mkRec('sa-old1', 200, 1699999900000), provider: 'scope-a.example' },
        { ...mkRec('sa-old0', 200, 1699999800000), provider: 'scope-a.example' },
      ],
      more: false, cursor_ms: 1699999800000,
    };
    Object.defineProperty(box, 'scrollHeight', { configurable: true, get() { return 200; } });
    w.navigateTo([{ dim: 'provider', id: 'scope-a.example' }]);
    await sleep(50);
    const scopedIds = rows().map(tr => tr.dataset.id);
    check('the scoped view renders its own ring rows plus its own history rows',
      scopedIds.join() === 'sa-ring2,sa-ring1,sa-ring0,sa-old1,sa-old0' &&
        w.eval('logArchive.length') === 2);
    Object.defineProperty(box, 'scrollHeight', { configurable: true, get() { return 4000; } });
    logPage = { records: [], more: false, cursor_ms: 0 };
    w.navigateTo([]);
    await sleep(50);
    check('the default view renders its own ring rows without the scope\'s history',
      w.eval('logArchive.length') === 0 && rows().length === 7 &&
        rows().some(tr => tr.dataset.id === 'p-row3') &&
        rows().some(tr => tr.dataset.id === 'sa-ring2') &&
        !rows().some(tr => tr.dataset.id.startsWith('sa-old')));
  }

  // ---- test 10d: the request log nests by local calendar day - a divider
  // rides above each day's first row, moves with live inserts, and never
  // duplicates or orphans across full resyncs. Fixtures anchor to local
  // midnights so the days are distinct regardless of the run time.
  const DAY_MS = 86400000;
  const midnight = new Date(); midnight.setHours(0, 0, 0, 0);
  const at = daysAgo => midnight.getTime() - daysAgo * DAY_MS + 3600000; // 01:00 local, N days ago
  const MIXED = [
    mkRec('day2', 200, at(2)),
    mkRec('day1', 200, at(1)),
    mkRec('day0', 200, at(0)),
  ];
  fire('snapshot', { feed_id: 'feedB', seq: 3, incremental: false,
    records: MIXED, counters: { in_flight: 0, total_requests: 3, total_errors: 0 } });
  await sleep(30);
  const seps = () => [...d.querySelectorAll('#tbl-requests tr.day-sep')];
  check('day dividers: exactly one per local day', seps().length === 3);
  check('newest day divider is stamped TODAY', seps()[0].textContent.includes('TODAY'));
  check('each divider sits directly above its day opener',
    seps()[0].nextElementSibling.dataset.id === 'day0' &&
    seps()[1].nextElementSibling.dataset.id === 'day1' &&
    seps()[2].nextElementSibling.dataset.id === 'day2');
  fire('record', { ...mkRec('day0b', 200, at(0) + 3600000) }, '4');
  await sleep(20);
  check('same-day live insert moves the divider, never duplicates it',
    seps().length === 3 && seps()[0].nextElementSibling.dataset.id === 'day0b' &&
    seps()[0].nextElementSibling.nextElementSibling.dataset.id === 'day0');
  fire('record', { ...mkRec('day1b', 200, at(1) + 7200000) }, '5');
  await sleep(20);
  // An out-of-order arrival (a late-finalized older record) opens its own day
  // boundary at its arrival position - the display is arrival-ordered, so the
  // divider tracks the displayed sequence, never a re-sorted one.
  check('late-arriving previous-day record opens its own divider at its arrival position',
    seps().length === 4 && seps()[0].nextElementSibling.dataset.id === 'day1b');
  fire('snapshot', { feed_id: 'feedB', seq: 5, incremental: false,
    records: [ mkRec('day2', 200, at(2)), mkRec('day1', 200, at(1)), mkRec('day0', 200, at(0)),
      mkRec('day0b', 200, at(0) + 3600000), mkRec('day1b', 200, at(1) + 7200000) ],
    counters: { in_flight: 0, total_requests: 5, total_errors: 0 } });
  await sleep(30);
  check('full resync rebuilds the dividers exactly (no orphans)', seps().length === 4);
  check('dividers are not request rows (log stats unchanged)', rows().length === 5);

  // ---- test 11: chart bucket math (pure pipeline; renderChart stays a jsdom
  // no-op behind the canvas size guard, so the data layer is driven directly) ----
  const CHART_FROM = 1800000000000;
  // Money is displayed in cents below $1 on every surface. Unit selection
  // uses the unrounded magnitude; stored values and aggregate math stay USD.
  const moneyCases = [
    [0, '0¢'], [0.5, '50¢'], [0.01, '1¢'], [0.0012, '0.12¢'],
    [0.00001, '0.001¢'], [0.99999, '99.999¢'], [1, '$1'], [3.5, '$3.5'],
    [1234, '$1.23K'], [2500000, '$2.5M'], [-0.25, '-25¢'], [-2, '-$2'],
  ];
  for (const [value, want] of moneyCases) {
    check(`money ${value} displays as ${want}`, w.eval(`fmtMoney(${value})`) === want);
  }
  check('unknown money stays unknown', w.eval('fmtMoney(null) === "-" && fmtMoney(NaN) === "-" && fmtMoney(Infinity) === "-"'));
  check('request log uses cents', w.reqRow({ ...mkRec('cents'), cost: 0.25 }).includes('25¢'));
  check('absent ttft is a placeholder, not 0ms', (() => {
    const row = w.reqRow({ ...mkRec('no-ttft'), ttft_ms: 0 });
    const detail = w.formatDetail({ ...mkRec('no-ttft'), ttft_ms: 0 });
    return w.eval('fmtTTFT(0) === "-" && fmtTTFT(null) === "-" && fmtTTFT(100) === "100ms"') &&
      /<td>100ms<\/td>/.test(w.reqRow(mkRec('has-ttft'))) &&
      /<td>-<\/td>/.test(row) &&
      !/<td>0ms<\/td>/.test(row) &&
      detail.includes('<span class="k">ttft</span><span class="v">-</span>') &&
      !detail.includes('>0ms<');
  })());
  check('explorer cost uses cents with a unit-neutral per-token label',
    w.kpiBlend({cost_per_mtok: 0.025}).includes('2.5¢') && !w.kpiBlend({cost_per_mtok: 0.025}).includes('$/Mtok'));
  // shareText owns every summary/plotted share row: a zero denominator means
  // the ratio was never measured, so nothing may render - never a fabricated
  // share (the underlying pctCap on a zero b would floor an infinite ratio to
  // a false "99.9%").
  check('shareText renders a measured share and guards the zero denominator',
    w.eval('shareText(60, 100, "in")') === '60.0% in' &&
      w.eval('shareText(50, 0, "in")') === null &&
      w.eval('shareText(0, 0, "in")') === null);
  // hexA blends only exact 6-digit hex colors; anything else (a CSS var
  // reference, a named color, a 3-digit hex, an empty string from a failed
  // palette read) must pass through unchanged so a malformed value can never
  // become rgba(NaN,NaN,NaN,...).
  check('hexA blends real hex and passes non-hex colors through unchanged',
    w.hexA('#5b8cff', 0.5) === 'rgba(91,140,255,0.5)' &&
      w.hexA('var(--accent)', 0.5) === 'var(--accent)' &&
      w.hexA('red', 0.8) === 'red' &&
      w.hexA('#fff', 0.8) === '#fff' &&
      w.hexA('', 0.8) === '');
  const BUCKET_MS = 120000;
  const mkChart = () => ({
    from_ms: CHART_FROM, now_ms: CHART_FROM + 30 * BUCKET_MS, bucket_ms: BUCKET_MS,
    ttft_p: [null, null, null], tps_p: [null, null, null],
    buckets: Array.from({ length: 30 }, (_, i) => ({
      t: CHART_FROM + i * BUCKET_MS,
      req: 0, err: 0, rl: 0, in: 0, out: 0, cache: 0, reason: 0, cost: 0,
      ttft: [null, null, null], tps: [null, null, null],
    })),
  });
  // Explicit renders own dirty consumption, including no-data boot renders.
  // Geometry reads on a missing payload force layout without drawing; a real
  // empty payload still passes through the existing jsdom/canvas size guard.
  const chartBootRender = w.eval(`(() => {
    const box = $('chart-traffic');
    const previous = {agg: chartAgg, up: _up, key: _upKey};
    const width = Object.getOwnPropertyDescriptor(box, 'clientWidth');
    const height = Object.getOwnPropertyDescriptor(box, 'clientHeight');
    let reads = 0, destroyed = 0, measuredWidth = 0, measuredHeight = 0;
    Object.defineProperties(box, {
      clientWidth: {configurable: true, get() { reads++; return measuredWidth; }},
      clientHeight: {configurable: true, get() { reads++; return measuredHeight; }},
    });
    try {
      chartAgg = null;
      _up = {destroy() { destroyed++; }}; _upKey = 'traffic';
      renderChart();
      const missing = reads === 0 && _up === null && destroyed === 1;
      chartAgg = {buckets: []};
      renderChart();
      const noBuckets = reads === 0;
      chartAgg = ${JSON.stringify(mkChart())};
      renderChart();
      const empty = reads === 2 && _up === null;
      chartAgg.buckets[0].req = 1;
      renderChart();
      const data = reads === 4 && _up === null;
      chartAgg.buckets[1].req = 1; chartAgg.buckets[2].req = 1; chartAgg.buckets[3].req = 1;
      let draws = 0;
      measuredWidth = 640; measuredHeight = 210;
      _up = {width: 640, height: 210, setData() { draws++; }};
      _upKey = chartView.preset;
      renderChart();
      const explicit = draws === 1;
      return {missing, noBuckets, empty, data, explicit};
    } finally {
      if (width) Object.defineProperty(box, 'clientWidth', width); else delete box.clientWidth;
      if (height) Object.defineProperty(box, 'clientHeight', height); else delete box.clientHeight;
      chartAgg = previous.agg;
      _up = previous.up; _upKey = previous.key;
    }
  })()`);
  check('missing chart data consumes dirty state and removes the old plot without layout reads', chartBootRender.missing && chartBootRender.noBuckets);
  check('real empty and populated chart payloads retain the jsdom dimension guard and consume dirty state', chartBootRender.empty && chartBootRender.data);
  check('an explicit populated chart draw consumes dirty state at the render owner', chartBootRender.explicit);
  w.eval('window.__cp = ' + JSON.stringify(mkChart()) + ';');
  // traffic preset: grouped req/err bars over the authoritative server pair;
  // EMPTY buckets are dropped and x compacts to slot positions (_vis keeps
  // the true bucket per plotted slot)
  w.eval(`
    chartAgg = window.__cp;
    chartAgg.buckets[3].req = 10; chartAgg.buckets[3].err = 2;
    chartAgg.buckets[7].req = 4; chartAgg.buckets[7].err = 1;

    chartAgg.buckets[3].req = 15; chartAgg.buckets[3].err = 3;
    chartView.preset = 'traffic'; chartView.hidden = {};
  `);
  const dataTraffic = w.eval('chartData()');
  check('empty buckets are dropped from bar plots', w.eval('_vis.join(",")') === '3,7' && dataTraffic[0].length === 2);
  check('compacted x values are slot positions', dataTraffic[0][0] === 0.5 && dataTraffic[0][1] === 1.5);
  check('traffic bars carry merged requests', dataTraffic[1][0] === 15 && dataTraffic[1][1] === 4);
  check('traffic bars carry merged errors', dataTraffic[2][0] === 3 && dataTraffic[2][1] === 1);

  // totals strip: the viewed period's server sums:
  // req = 15+4, err = 3+1 → rate 4/19 = 21.1%
  const totalsHtml = w.eval('chartTotals()');
  check('totals strip shows period requests + errors + rate', totalsHtml.includes('requests</span> 19') && totalsHtml.includes('errors</span> 4') && totalsHtml.includes('21.1%'));

  // No-data placeholder strip: the preset's tile skeleton at full height, so
  // the first payload swaps text without moving anything below. Tile counts
  // must equal the data-filled strip (what wraps is rows) and the merged
  // overview labels must not drift from the data case.
  {
    const savedAgg = w.eval('JSON.stringify(chartAgg)');
    const counts = {};
    for (const preset of ['traffic', 'tokens', 'errors', 'cost', 'latency', 'overview']) {
      w.eval(`chartView.preset = '${preset}'`);
      const filled = (w.eval('chartTotals()').match(/class="chart-total/g) || []).length;
      w.eval('chartAgg = null');
      const skel = w.eval('chartTotals()');
      const skelCount = (skel.match(/class="chart-total/g) || []).length;
      w.eval('chartAgg = ' + savedAgg + ';');
      counts[preset] = { filled, skel: skelCount };
    }
    check('every preset skeleton matches its data-filled tile count',
      Object.values(counts).every(c => c.filled === c.skel) && JSON.stringify(counts));
    check('overview skeleton lists the merged tile labels without drift',
      w.eval(`(() => { chartView.preset = 'overview'; chartAgg = null;
        const s = chartTotals(); chartAgg = ${savedAgg};
        return ['requests / tokens','tokens in/out/cached','cost','errors / 429','avg latency / speed']
          .every(l => s.includes('tl">' + l + '</span> -'));
      })()`));
    check('percentile ordinals keep the teens and centuries right (111th, not 111st)',
      w.eval('[0,1,2,3,11,12,13,21,95,111,112,113,121].map(p => pctOrdinal(p)).join()') ===
      '0th,1st,2nd,3rd,11th,12th,13th,21st,95th,111th,112th,113th,121st');
    w.eval("chartView.preset = 'traffic'");
  }

  // Every selected tile always renders: summaryVisibleTiles applies only
  // the picker's hidden set - the band's auto-fit grid wraps the tiles into
  // as many whole rows as the card needs, so no viewport can drop a metric
  // or disagree with the picker's count.
  {
    w.eval("chartView.preset = 'overview'; chartView.hidden.overview = [];");
    check('every selected summary tile renders with no viewport cap',
      w.eval('summaryVisibleTiles(activePreset()).join()') === 'req,tokens,cost,health,timing');
    check('the picker hidden set is the only filter',
      w.eval(`(() => { chartView.hidden.overview = ['req'];
        const vis = summaryVisibleTiles(activePreset());
        chartView.hidden.overview = []; return vis.join(); })()`) === 'tokens,cost,health,timing');
    w.eval("chartView.preset = 'traffic'");
  }

  // errors preset: err count + rate recomputed from the merged pair
  w.eval("chartView.preset = 'errors'");
  const dataErr = w.eval('chartData()');
  check('err_rate recomputed from the merged pair', Math.abs(dataErr[2][0] - (3 / 15) * 100) < 1e-9 && Math.abs(dataErr[2][1] - 25) < 1e-9);
  check('err count bars use the authoritative server pair', dataErr[1][0] === 3 && dataErr[1][1] === 1);

  // hidden series keep their column as all-nulls (a toggle is a pure setData
  // with a STABLE column count - shrinking the array leaves a stale frame)
  w.eval("chartView.hidden = { traffic: ['err'] }; chartView.preset = 'traffic'");
  const dataHidden = w.eval('chartData()');
  check('hiding a bar series passes all-null columns', dataHidden.length === 3 && dataHidden[2].every(v => v === null) && dataHidden[1][0] === 15);
  w.eval("chartView.preset = 'errors'");
  const dataErr2 = w.eval('chartData()');
  check('hidden set does not leak across presets', dataErr2[1][0] === 3 && dataErr2[2][0] !== null);
  w.eval("chartView.hidden = { errors: ['errRate'] }; chartView.preset = 'errors'");
  const dataErr3 = w.eval('chartData()');
  check('hiding a line series passes all-null columns', dataErr3[2].every(v => v === null) && dataErr3[1][0] === 3);
  w.eval('chartView.hidden = {}');

  // tokens preset: 3 bars + the cache line + the cache-hit % line on the
  // pinned pct right axis, in declaration order (buckets 3 and 7 still
  // carry traffic from the traffic sub-test → compacted slots)
  w.eval(`
    chartView.preset = 'tokens'; chartView.hidden = {};
    chartAgg.buckets[4].req = 1;
    chartAgg.buckets[4].in = 100; chartAgg.buckets[4].out = 40; chartAgg.buckets[4].reason = 10; chartAgg.buckets[4].cache = 60;
  `);
  const dataTok = w.eval('chartData()');
  const tokSlot = w.eval('_vis.indexOf(4)');
  check('token slots plot in/out/reason/cache', dataTok.length === 6 && tokSlot >= 0 && dataTok[1][tokSlot] === 100 && dataTok[2][tokSlot] === 40 && dataTok[3][tokSlot] === 10 && dataTok[4][tokSlot] === 60);
  check('cache hit derives pct-of-input per bucket and stays unmeasured at zero input',
    dataTok[5][tokSlot] === 60 && dataTok[5][0] === null);
  const tokTotals = w.eval('chartTotals()');
  check('tokens totals carry shares naming their denominators, cache pct of input and the pair with its share',
    tokTotals.includes('71.4%') && tokTotals.includes('28.6%') &&
    tokTotals.includes('60.0% of input') &&
    tokTotals.includes('in:out</span> <span class="val-pair"><span class="v-in">100</span><span class="pair-sep">/</span><span class="v-out">40</span>') &&
    tokTotals.includes('<span class="chart-sub">71.4% of tokens</span>'));
  check('tokens preset pins the cache-hit line to a 0-100 right axis', w.eval(`(() => {
    const opts = upOpts(600, 180);
    return opts.axes.length === 3 && opts.axes[2].scale === 'pct' && opts.axes[2].side === 1 &&
      opts.series[5].scale === 'pct' && opts.series[5].label === 'cache hit';
  })()`));
  w.eval("chartView.hidden = { tokens: ['outTok'] }");
  const dataTok2 = w.eval('chartData()');
  check('hiding a token series nulls its column, count stable', dataTok2.length === 6 && dataTok2[2].every(v => v === null) && dataTok2[1][tokSlot] === 100 && dataTok2[3][tokSlot] === 10 && dataTok2[4][tokSlot] === 60);
  w.eval('chartView.hidden = {}');

  // Speed + latency: one selected percentile per metric. A bucket plots only
  // when BOTH measurements exist at that percentile (a null in either line is
  // a gap point, not a zero); unmeasured buckets compact away like empty bar
  // slots instead of stranding gap points in dead space.
  w.eval(`
    chartView.preset = 'latency'; chartView.pct = 95;
    chartAgg.buckets[2].req = 1;
    chartAgg.buckets[2].ttft = [10, 20, 30]; chartAgg.buckets[2].tps = [100.25, 200.75, 300.5];
    chartAgg.buckets[7].req = 1;
    chartAgg.buckets[7].ttft = [1, 2, 3];
    chartAgg.buckets[9].req = 1;
    chartAgg.buckets[9].ttft = [null, 5, null]; chartAgg.buckets[9].tps = [null, 50, null];
  `);
  const dataLat = w.eval('chartData()');
  check('speed + latency keeps only buckets with both measurements',
    dataLat[0].length === 2 && dataLat[0][0] === 0.5 && dataLat[0][1] === 1.5);
  check('compacted points keep their true bucket times for ticks and hover', w.eval(`(() => {
    const ticks = chartXValues({data: [chartData()[0]]}, [0.5, 1.5]);
    return (_vis && _vis.join() === '2,9') &&
      ticks.join() === [chartXTick(chartAgg.buckets[2].t), chartXTick(chartAgg.buckets[9].t)].join();
  })()`));
  check('speed + latency reads only the selected percentile, preserving fractional speed',
    dataLat.length === 3 && dataLat[1][0] === 200.75 && dataLat[2][0] === 20 && dataLat[1][1] === 50 && dataLat[2][1] === 5);
  check('speed + latency uses independent unit-honest axes without bands', w.eval(`(() => {
    const opts = upOpts(640, 210);
    return opts.axes[1].scale === 'ytps' && opts.axes[2].scale === 'yttft' &&
      opts.series.length === 3 && opts.series[1].scale === 'ytps' && opts.series[2].scale === 'yttft' &&
      opts.series[1].label === 'speed' && opts.series[2].label === 'latency' && !opts.bands;
  })()`));
  check('a bucket missing one measurement never plots, even with that line hidden', w.eval(`(() => {
    chartView.hidden = {latency:['tps']};
    const d = chartData();
    chartView.hidden = {};
    return d[0].length === 2 && d[1].every(v => v === null) && d[2][0] === 20 && d[2][1] === 5;
  })()`));
  const isolatedOpts = w.eval('upOpts(370, 210)');
  const [innerPoint, outerPoint] = isolatedOpts.series.slice(1).map(s => s.points);
  check('coincident isolated lines use an inner filled point and transparent outer ring',
    innerPoint.width === 0 && innerPoint.fill === isolatedOpts.series[1].stroke &&
    outerPoint.fill === 'transparent' && outerPoint.width > 0 &&
    outerPoint.size / 2 - outerPoint.width > innerPoint.size / 2);
  check('isolated marker identity is stable when an earlier line is hidden', w.eval(`(() => {
    const hidden = chartView.hidden;
    const before = upOpts(370, 210).series.slice(1).map(s => s.points);
    chartView.hidden = {latency:['tps']};
    const after = upOpts(370, 210).series.slice(1).map(s => s.points);
    chartView.hidden = hidden;
    return before.every((p, i) => p.size === after[i].size && p.width === after[i].width && p.fill === after[i].fill);
  })()`));
  for (const si of [1, 2]) {
    const marks = isolatedOpts.series[si].points.filter?.({ data: [[0], [9.928], [1]] }, si);
    check(`a single ${isolatedOpts.series[si].label} sample has a visible point`, marks?.join() === '0');
  }
  for (const [values, expected] of [
    [[1, null, 2, null, 3], [0, 2, 4]],
    [[1, 2, null, 3, 4], null],
    [[null, 0, null], [1]],
    [[null, null, null], null],
    [[1, null, 2, 3, null, 4], [0, 5]],
  ]) {
    const marks = isolatedOpts.series[1].points.filter?.({ data: [[], values] }, 1) ?? null;
    check(`line points preserve sparse gaps: ${JSON.stringify(values)}`, JSON.stringify(marks) === JSON.stringify(expected));
  }
  check('fractional millisecond ticks retain precise, unique units',
    isolatedOpts.axes[2].values({}, [0, 0.5, 1, 1.5, 2]).join() === '0ms,0.5ms,1ms,1.5ms,2ms');
  check('both axes suppress duplicate compact labels without relabeling ticks', w.eval(`(() => {
    const splits = [1000, 1001, 1010, 1011];
    return JSON.stringify(chartAxisValues({fmt: fmtDur}, splits)) === JSON.stringify(['1s', null, '1.01s', null]);
  })()`));
  check('fractional duration formatter preserves zero, signs, and scale boundaries', w.eval(`
    fmtDur(0) === '0ms' && fmtDur(-0.5) === '-0.5ms' && fmtDur(0.00001) !== '0ms' && fmtDur(999.9999) === '1s'
  `));
  check('ttft formatter treats 0 as absent', w.eval('fmtTTFT(0) === "-" && fmtTTFT(undefined) === "-" && fmtTTFT(1) === "1ms"'));

  // Both period totals follow the one percentile control, not bucket means
  // and not a second always-p50 baseline. Names never repeat the dropdown.
  w.eval(`
    chartView.preset = 'latency';
    chartAgg.ttft_p = [110, 220, 330]; chartAgg.tps_p = [101.25, 202.75, 303.5];
  `);
  const latTotals = w.eval('chartTotals()');
  check('speed + latency totals show only selected period values', latTotals.includes('202.75 tok/s') && latTotals.includes('220ms') && !latTotals.includes('101.25') && !latTotals.includes('110ms') && !/p(?:50|95|99)/.test(latTotals));
  for (const [pct, speed, ttft, totalSpeed, totalTTFT, kept] of [[50, 100.25, 10, 101.25, 110, 1], [99, 300.5, 30, 303.5, 330, 1]]) {
    w.setChartPct(String(pct));
    const data = w.eval('chartData()'), totals = w.eval('chartTotals()');
    check(`percentile dropdown chooses both series and period totals at ${pct}`,
      data.length === 3 && data[0].length === kept && data[1][0] === speed && data[2][0] === ttft &&
      totals.includes(totalSpeed + ' tok/s') && totals.includes(totalTTFT + 'ms'));
  }
  w.setChartPct('95');
  // blank state: zero merged traffic reads as empty
  w.eval(`
    chartAgg = window.__cp = ${JSON.stringify(mkChart())};

    chartView.preset = 'traffic';
  `);
  check('all-zero traffic reads as empty', w.eval('chartIsEmpty()') === true);
  w.eval('chartAgg.buckets[7].req = 1');
  check('any merged traffic un-empties the chart', w.eval('chartIsEmpty()') === false);

  // Independent end/record delivery never mutates authoritative chart buckets.
  w.eval(`
    chartAgg = window.__cp = ${JSON.stringify(mkChart())};

  `);
  const raceRec = { ...mkRec('chartend', 200, CHART_FROM + 4 * BUCKET_MS) };
  fire('end', { record: raceRec, in_flight: 0 }, '91');
  fire('record', raceRec, '92');
  await sleep(20);
  check('live end/record delivery never adds to canonical chart buckets', w.eval('chartAgg.buckets[4].req') === 0);
  w.eval('chartAgg = null; chartView.preset = "traffic"; chartView.hidden = {}');

  // ---- test 11b: Cost preset renderer - cost is the lone bar on the arcsinh
  // left scale while requests renders as a LINE on its right axis (the preset
  // row overrides the registry's bar flag; CHART_SERIES stays the default
  // owner). Traffic keeps requests as a bar. Pinned at the plan/upOpts seam:
  // chartPlan resolves each row's renderer onto the plan meta, upOpts
  // dispatches on the resolved flag.
  w.eval(`
    chartAgg = window.__cp = ${JSON.stringify(mkChart())};

    chartView.preset = 'cost'; chartView.hidden = {};
    chartAgg.buckets[5].req = 3; chartAgg.buckets[5].cost = 1.25;
  `);
  check('cost plan resolves cost=bar on y, requests=line on yr',
    w.eval('chartPlan().meta.map(m => m.spec.id + "=" + (m.bar ? "bar" : "line") + "@" + m.scale).join("|")') === 'cost=bar@y|req=line@yr');
  check('cost legend still lists every preset series',
    w.eval('chartPlan().meta.map(l => l.spec.id).join()') === 'cost,req');
  const dataCost = w.eval('chartData()');
  check('cost keeps bar-preset compaction with merged cost + requests values',
    w.eval('_vis.length') === 1 && dataCost[0][0] === 0.5 && dataCost[1][0] === 1.25 && dataCost[2][0] === 3);
  const optsCost = w.eval('upOpts(600, 150)');
  check('upOpts dispatches cost as a bar path, requests as a line',
    typeof optsCost.series[1].paths === 'function' && optsCost.series[2].width === 1.6 &&
    optsCost.series[2].scale === 'yr' && !optsCost.series[2].paths);
  w.eval("chartView.hidden = { cost: ['req'] }");
  const dataCostH = w.eval('chartData()');
  check('cost hidden toggle keeps a stable all-null line column',
    dataCostH.length === 3 && dataCostH[2].every(v => v === null) && dataCostH[1][0] === 1.25);
  check('hidden lines never leave an isolated point behind', optsCost.series[2].points.filter?.({ data: dataCostH }, 2) === null);
  check('mixed presets reuse isolated line points and keep bars point-free',
    optsCost.series[2].points.filter === w.chartIsolatedPoints &&
    optsCost.series[1].points.show === false && !optsCost.series[1].points.filter);
  check('bar series do not consume the inner isolated-line marker',
    optsCost.series[2].points.width === 0 && optsCost.series[2].points.size === innerPoint.size &&
    optsCost.series[2].points.fill === optsCost.series[2].stroke);
  w.eval("chartView.preset = 'traffic'; chartView.hidden = {}");
  check('traffic plan keeps requests + errors as bars',
    w.eval('chartPlan().meta.map(m => m.spec.id + "=" + (m.bar ? "bar" : "line") + "@" + m.scale).join("|")') === 'req=bar@y|err=bar@y');
  w.eval('chartAgg = null; chartView.preset = "traffic"; chartView.hidden = {}');

  // ---- test 11c: percentile selection never changes series structure ----
  // The former band-collapse predicate could blank unrelated presets when
  // p50 persisted. Every percentile now keeps the same two metric lines,
  // and sum/rate presets are independent of the percentile choice.
  w.eval(`
    chartAgg = window.__cp = ${JSON.stringify(mkChart())};

    chartAgg.buckets[5].req = 3; chartAgg.buckets[5].err = 1;
    chartView.hidden = {};
  `);
  const pct50Want = {
    overview: [],
    traffic: ['req', 'err'],
    tokens: ['inTok', 'outTok', 'reason', 'cache', 'cachePct'],
    errors: ['err', 'errRate'],
    cost: ['cost', 'req'],
    latency: ['tps', 'ttft'],
  };
  for (const [p, want] of Object.entries(pct50Want)) {
    w.eval(`chartView.preset = '${p}'; chartView.pct = 50;`);
    check(`pct=50 keeps every ${p} series row`,
      w.eval('chartPlan().meta.map(m => m.spec.id).join()') === want.join());
  }
  for (const pct of [95, 99]) {
    w.eval(`chartView.preset = 'latency'; chartView.pct = ${pct};`);
    check(`speed + latency keeps two lines at pct=${pct}`,
      w.eval('chartPlan().meta.map(m => m.spec.id).join()') === 'tps,ttft');
  }

  // ---- test 11d: grouped bars occupy separate, centered intervals ----
  // align -1/0/+1 alone made three equal-width bars overlap: the middle
  // interval straddled both neighbors. Check the occupied intervals, not
  // merely the alignment flags that previously let this regression pass.
  const visBars = () => JSON.parse(w.eval(
    'JSON.stringify(chartPlan().meta.filter(m => m.bar && !m.hidden).map(m => ({id:m.spec.id, align:m.align, size:m.size, offset:m.offset})))'));
  const centeredBars = (bars, ids) => {
    const extents = bars.map(m => [m.offset, m.offset + m.size]);
    return bars.map(m => m.id).join() === ids.join() && extents.length > 0 &&
      bars.every(m => m.align === 1) &&
      extents.every(([a, b], i) => Number.isFinite(a) && b > a && a >= -0.5 && b <= 0.5 &&
        (i === 0 || extents[i - 1][1] <= a + 1e-9)) &&
      Math.abs(extents[0][0] + extents.at(-1)[1]) < 1e-9;
  };
  w.eval("chartView.preset = 'tokens'; chartView.pct = 95; chartView.hidden = {}");
  check('three token bars have disjoint intervals centered within one bucket',
    centeredBars(visBars(), ['inTok', 'outTok', 'reason']));
  w.eval("chartView.hidden = { tokens: ['outTok'] }");
  check('hiding the middle bar re-centers disjoint surviving intervals',
    centeredBars(visBars(), ['inTok', 'reason']));
  w.eval("chartView.hidden = { tokens: ['inTok'] }");
  check('hiding the first bar re-centers disjoint surviving intervals',
    centeredBars(visBars(), ['outTok', 'reason']));
  w.eval("chartView.hidden = { tokens: ['inTok', 'outTok'] }");
  check('one remaining bar is centered without occupying hidden slots',
    centeredBars(visBars(), ['reason']));
  w.eval("chartView.hidden = {}");

  // ---- test 11e: legend toggle DOM (chartLegendSync's rendered output) ----
  // The delegated #traffic-legend click hides a series: the button gains
  // the off class and the plot column goes all-null with a STABLE column
  // count (a toggle is a pure setData; the plot is never re-created). jsdom
  // blocks the canvas behind the size guard, but the legend syncs before it.
  w.eval("chartView.preset = 'traffic'; chartView.hidden = {}");
  w.renderChart();
  const legErr = () => d.querySelector('#traffic-legend .leg-item[data-series="err"]');
  check('legend renders every preset series as a toggle button',
    [...d.querySelectorAll('#traffic-legend .leg-item')].map(b => b.dataset.series).join() === 'req,err' &&
    !!legErr() && !legErr().classList.contains('off'));
  legErr().click();
  check('delegated legend click marks the item off', legErr().classList.contains('off'));
  const dataLeg = w.eval('chartData()');
  check('legend hide nulls the column, column count stable',
    dataLeg.length === 3 && dataLeg[2].every(v => v === null) && dataLeg[1][0] === 3);

  w.eval("chartView.preset = 'latency'; chartView.pct = 95; chartView.hidden = {}; chartAgg.buckets[5].ttft = [10, 20, 30]; chartAgg.buckets[5].tps = [100, 200, 300]");
  w.renderChart();
  const legTTFT = () => d.querySelector('#traffic-legend .leg-item[data-series="ttft"]');
  check('speed + latency legend has two pressed toggles with no repeated percentile labels',
    [...d.querySelectorAll('#traffic-legend .leg-item')].map(b => b.dataset.series).join() === 'tps,ttft' &&
    [...d.querySelectorAll('#traffic-legend .leg-item')].map(b => b.textContent).join() === 'speed,latency' &&
    [...d.querySelectorAll('#traffic-legend .leg-item')].every(b => b.getAttribute('aria-pressed') === 'true'));
  const dataLegLat = w.eval('chartData()');
  check('speed + latency legend controls the selected value for each metric',
    dataLegLat.length === 3 && dataLegLat[0].length === 1 && dataLegLat[1][0] === 200 && dataLegLat[2][0] === 20);
  legTTFT().click();
  const dataLegLatHidden = w.eval('chartData()');
  check('latency toggle hides only its line and exposes its unpressed state',
    legTTFT().getAttribute('aria-pressed') === 'false' && legTTFT().classList.contains('off') &&
    dataLegLatHidden.length === 3 && dataLegLatHidden[0].length === 1 && dataLegLatHidden[1][0] === 200 && dataLegLatHidden[2].every(v => v === null));
  legTTFT().click();
  check('latency metric toggle restores only its own line',
    legTTFT().getAttribute('aria-pressed') === 'true' && w.eval('chartData()[1][0] === 200 && chartData()[2][0] === 20'));
  const pctControl = d.getElementById('chart-pct');
  check('the percentile control names both plotted metrics without a second owner', pctControl.getAttribute('aria-label') === 'Percentile' && pctControl.title.includes('speed and latency'));
  pctControl.value = '99';
  pctControl.dispatchEvent(new w.Event('change', { bubbles: true }));
  check('the percentile dropdown updates both plotted lines together', w.eval('chartView.pct === 99 && chartData()[1][0] === 300 && chartData()[2][0] === 30'));
  w.eval(`upOpts(640, 210).hooks.setCursor[0]({data: chartData(), cursor: {idx: 0, left: 100}, bbox: {width: 640}})`);
  check('hover shows one speed and one latency value with no percentile duplication',
    [...d.querySelectorAll('#chart-hover .chart-hover-row')].map(el => el.textContent).join() === 'speed300 tok/s,latency30ms' &&
    !/p(?:50|95|99)/.test(d.getElementById('chart-hover').textContent));
  w.eval(`chartView.hidden = {}; storage.set('dash.chart', JSON.stringify({preset: 'latency', pct: 99, window: '10080', hidden: {latency: ['ttft', 'dur', 'ttftP50', 'req']}})); loadChartView();`);
  check('saved latency selection survives while obsolete band/duration and foreign series ids are discarded',
    w.eval('chartView.preset === "latency" && chartView.pct === 99 && chartView.window === "10080" && chartView.hidden.latency.join() === "ttft"'));
  w.toggleChartSeries('dur');
  check('unknown or obsolete series cannot enter hidden state', w.eval('chartView.hidden.latency.join() === "ttft"'));
  // Both toggle paths share one flip contract, each writing its own
  // hidden-map key: legend series under the active preset id, summary
  // tiles under 'overview' - never cross-contaminated - with persistence
  // through dash.chart on every flip.
  w.toggleChartSeries('ttft');
  check('a legend toggle flips its series under the preset key and persists',
    w.eval('chartView.hidden.latency.join() === ""') &&
    (JSON.parse(w.eval('storage.get("dash.chart")') || '{}').hidden || {}).latency !== undefined);
  w.toggleChartSeries('ttft');
  check('toggling again restores the series and re-persists',
    w.eval('chartView.hidden.latency.join() === "ttft"') &&
    JSON.parse(w.eval('storage.get("dash.chart")') || '{}').hidden.latency.join() === 'ttft');
  w.eval("chartView.preset = 'overview'; chartView.hidden = {};");
  w.toggleSummaryMetric('cost');
  check('a picker flip hides its metric under the overview key, not the preset id',
    w.eval('chartView.hidden.overview.join() === "cost"') &&
    (JSON.parse(w.eval('storage.get("dash.chart")') || '{}').hidden || {}).overview.join() === 'cost');
  w.toggleSummaryMetric('cost');
  w.eval("chartView.hidden = {}; chartView.preset = 'traffic';");
  w.eval("chartView.hidden = {}; chartView.window = 'all'; chartView.pct = 95; fillChartControls()");
  for (const preset of ['traffic', 'tokens', 'errors', 'cost', 'latency', 'overview']) {
    w.eval(`chartView.preset = '${preset}'`);
    w.renderChart();
    check(`percentile control is ${preset === 'latency' ? 'visible' : 'hidden'} for ${preset}`,
      d.getElementById('chart-pct').hidden === (preset !== 'latency'));
  }

  // GAP-D pin: commitChartView's preset/pct callers. setChartPreset had no
  // test caller at all (mutation-proven - every other test assigned
  // chartView.preset directly), so the dropdown's real handler could be
  // dropped or broken without reddening anything. Both setters must
  // validate, persist the merged view through dash.chart, and re-render:
  // the percentile control's visibility flip proves the render, and the
  // stored payload proves the save kept the rest of the view.
  {
    const savedView = () => JSON.parse(w.eval('storage.get("dash.chart")') || 'null');
    w.eval("chartView.hidden = {}; chartView.window = 'all'; chartView.pct = 95;");
    w.setChartPreset('latency');
    check("setChartPreset persists the merged view and re-renders",
      w.eval('chartView.preset') === 'latency' &&
        savedView() && savedView().preset === 'latency' && savedView().pct === 95 &&
        savedView().window === 'all' && d.getElementById('chart-pct').hidden === false);
    w.setChartPreset('bogus');
    check('setChartPreset rejects unknown preset ids without touching the view',
      w.eval('chartView.preset') === 'latency' && savedView().preset === 'latency');
    w.setChartPct('99');
    check("setChartPct persists the merged view and re-renders",
      w.eval('chartView.pct') === 99 && savedView().pct === 99 && savedView().preset === 'latency' &&
        w.eval('chartData()[1][0]') === 300 && w.eval('chartData()[2][0]') === 30);
    w.setChartPct('42');
    check('setChartPct rejects unlisted percentiles without touching the view',
      w.eval('chartView.pct') === 99 && savedView().pct === 99);
    // GAP-C pin: the window select's setChartWindow caller. No jsdom row
    // drove #chart-window (mutation-proven: swapping the body to
    // commitChartView or dropping its validation left npm green), so the
    // window dropdown's real handler could be dropped or broken without
    // reddening anything. setChartWindow deliberately diverges from
    // commitChartView (a window change needs new server-computed buckets):
    // a valid switch must persist the merged view, issue a window-keyed
    // chart fetch, and apply the fetched payload; an unlisted window is
    // denied by default - no persist, no fetch.
    const chartFetchURLs = () => initialFetches.filter(u => u.includes('/metrics/agg/chart'));
    const fetchesBeforeWindow = chartFetchURLs().length;
    w.setChartWindow('15');
    check('setChartWindow persists the merged view and issues the window-keyed fetch',
      w.eval('chartView.window') === '15' &&
        savedView() && savedView().window === '15' && savedView().pct === 99 &&
        savedView().preset === 'latency' &&
        chartFetchURLs().length === fetchesBeforeWindow + 1 &&
        chartFetchURLs().at(-1).includes('window=15'));
    await sleep(30);
    check('setChartWindow applies the fetched window payload',
      w.eval('chartAgg && chartAgg.now_ms') === chartPayload.now_ms);
    const fetchesBeforeReject = chartFetchURLs().length;
    w.setChartWindow('bogus');
    check('setChartWindow rejects unlisted windows without persisting or fetching',
      w.eval('chartView.window') === '15' && savedView().window === '15' &&
        chartFetchURLs().length === fetchesBeforeReject);
  }

  // W43 pin: the real chart select wirings in live.js. The rows above prove
  // setChartWindow and setChartPreset behave as functions, but no row drove
  // the dropdown elements themselves (mutation-proven: dropping either
  // select's addEventListener line left every suite green), so a dead select
  // could ship without reddening anything. Drive the real selects through
  // change events. The window path must persist, fetch and apply; a preset
  // change deliberately re-renders from the live payload with no refetch
  // (commitChartView's contract - only a window switch needs new buckets).
  {
    const savedView = () => JSON.parse(w.eval('storage.get("dash.chart")') || 'null');
    const chartFetchURLs = () => initialFetches.filter(u => u.includes('/metrics/agg/chart'));
    // '60', not another '15': the chart request gate dedupes a re-issued
    // 'reuse' key (requestGate in core.js), so repeating the window the
    // previous block just fetched would issue no fetch at all.
    const windowSelect = d.getElementById('chart-window');
    const fetchesBeforeWindowWiring = chartFetchURLs().length;
    // The W41 rows above already fetched and applied this same chartPayload,
    // so chartAgg starts equal to it: without a delta the apply check below
    // passes against the stale W41 state even if the wiring never fetches
    // or never applies anything (mutation-proven: a dropped window listener
    // left it green). Bump now_ms with a unique delta so only applying the
    // window drive's own fetched payload can satisfy it.
    chartPayload.now_ms += 4000;
    windowSelect.value = '60';
    windowSelect.dispatchEvent(new w.Event('change', { bubbles: true }));
    check('the window dropdown wiring persists the merged view and issues the window-keyed fetch',
      w.eval('chartView.window') === '60' &&
        savedView() && savedView().window === '60' && savedView().pct === 99 &&
        savedView().preset === 'latency' &&
        chartFetchURLs().length === fetchesBeforeWindowWiring + 1 &&
        chartFetchURLs().at(-1).includes('window=60'));
    await sleep(30);
    check('the window dropdown wiring applies the fetched window payload',
      w.eval('chartAgg && chartAgg.now_ms') === chartPayload.now_ms);
    const presetSelect = d.getElementById('chart-preset');
    const fetchesBeforePresetWiring = chartFetchURLs().length;
    // The no-refetch conjunct below is otherwise masked by the chart request
    // gate's dedupe: a regressed setChartPreset that called fetchChart()
    // would re-issue the pinned window=60 key and issue no fetch at all, so
    // the count could never redden (mutation-proven). Shift the in-memory
    // window without fetching or saving first, so the pinned key differs and
    // a regressed refetch becomes a real fetch. The preset drive's save then
    // persists the shifted window, keeping the saved-view conjunct comparing
    // like with like; no later row reads chartView.window before test 11f
    // re-seeds the view wholesale.
    w.eval("chartView.window = '30'");
    presetSelect.value = 'traffic';
    presetSelect.dispatchEvent(new w.Event('change', { bubbles: true }));
    check('the preset dropdown wiring persists the merged view and re-renders without a refetch',
      w.eval('chartView.preset') === 'traffic' &&
        savedView() && savedView().preset === 'traffic' &&
        savedView().window === w.eval('chartView.window') &&
        savedView().pct === 99 && d.getElementById('chart-pct').hidden === true &&
        chartFetchURLs().length === fetchesBeforePresetWiring);
  }

  // Axis callbacks survive setData changing between timestamp and compacted
  // slots. Reusing the original options is deliberate: legend/window changes
  // update data without constructing a new plot.
  w.eval(`
    chartAgg = window.__cp = ${JSON.stringify(mkChart())};
    chartAgg.buckets.forEach(b => { b.req = 1; });

    chartView.preset = 'traffic'; chartView.hidden = {};
    chartData();
  `);
  const fullOpts = w.eval('upOpts(600, 180)');
  const axisSnapshot = opts => {
    try {
      const xs = w.eval('chartData()[0]');
      const axis = opts.axes[0];
      const u = { bbox: { width: 500, height: 140 }, data: [xs], scales: { x: { min: xs[0], max: xs.at(-1) } } };
      const range = opts.scales.x.range(u, xs[0], xs.at(-1));
      u.scales.x.min = range[0]; u.scales.x.max = range[1];
      const splits = axis.splits(u, 0, ...range, 100, 1);
      return { range, splits, labels: axis.values(u, splits) };
    } catch {
      return null;
    }
  };
  const fullAxisOK = got => got && got.range[0] <= CHART_FROM && got.range[1] >= CHART_FROM + 30 * BUCKET_MS &&
    got.splits.length >= 2 && got.splits.every(v => v >= CHART_FROM && v <= CHART_FROM + 30 * BUCKET_MS) &&
    got.labels.every((label, i) => label === w.chartXTick(got.splits[i]));
  const compactAxisOK = got => got && got.range[0] <= 0 && got.range[1] >= 2 &&
    JSON.stringify(got.splits) === '[0.5,1.5]' &&
    JSON.stringify(got.labels) === JSON.stringify([w.chartXTick(CHART_FROM + 3 * BUCKET_MS), w.chartXTick(CHART_FROM + 7 * BUCKET_MS)]);
  check('full chart x uses numeric milliseconds and includes complete edge buckets',
    fullOpts.scales.x.time === false && fullAxisOK(axisSnapshot(fullOpts)));
  w.eval('chartAgg.buckets.forEach((b, i) => { b.req = i === 3 || i === 7 ? 1 : 0; }); chartData()');
  check('existing full-chart axis callbacks follow compacted slots after setData',
    compactAxisOK(axisSnapshot(fullOpts)));
  const compactOpts = w.eval('upOpts(600, 180)');
  check('compacted x includes the full first and last slots', compactAxisOK(axisSnapshot(compactOpts)));
  w.eval('chartAgg.buckets.forEach(b => { b.req = 1; }); chartData()');
  check('existing compact-chart axis callbacks return to timestamps after setData',
    fullAxisOK(axisSnapshot(compactOpts)));
  const singleX = CHART_FROM + BUCKET_MS / 2;
  const singleRange = fullOpts.scales.x.range({ data: [[singleX]] }, singleX * 0.9, singleX * 1.1);
  check('single-bucket range ignores uPlot timestamp expansion and keeps the actual bucket edges',
    singleRange[0] === CHART_FROM && singleRange[1] === CHART_FROM + BUCKET_MS);

  // A decade ladder alone puts several gridlines within a few pixels near
  // zero. Validate the actual rendered label positions under asinh, including
  // sub-dollar values that can round to identical formatted labels.
  for (const [preset, max] of [['traffic', 100000], ['cost', 0.03]]) {
    w.eval(`chartView.preset = '${preset}'; chartData()`);
    const opts = w.eval('upOpts(600, 180)');
    const axis = opts.axes[1];
    const u = {
      bbox: { width: 500, height: 140 }, axes: opts.axes,
      scales: { y: { min: 0, max } },
      valToPos: value => 140 * (1 - Math.asinh(value) / Math.asinh(max)),
    };
    const splits = axis.splits(u, 1, 0, max, 28, 1);
    const filtered = axis.filter ? axis.filter(u, splits, 1, 28, 1) : splits;
    const labels = axis.values(u, filtered, 1, 28, 1);
    const rendered = filtered.map((v, i) => ({ value: v, label: labels[i] })).filter(t => t.value !== null && t.label != null);
    check(`${preset} y-axis labels stay at least 28px apart without duplicate values`,
      rendered.length >= 2 && new Set(rendered.map(t => t.label)).size === rendered.length &&
      rendered.every((t, i) => i === 0 || Math.abs(u.valToPos(t.value) - u.valToPos(rendered[i - 1].value)) >= 28 - 1e-9));
  }
  // Hand-cleared payload state, defense-in-depth: chartAgg is never nulled
  // after boot, this row clears it by hand, and the plan no longer
  // carries a plotted left axis (uPlot queues redraws as microtasks and
  // destroy() does not cancel them, so a queued draw can fire after the
  // plot is gone - the TypeError observed twice in fresh npm runs). The
  // x values callback guards !chartAgg; the y splits callback (chartYTicks,
  // uPlot's splits(self, axisIdx, scaleMin, scaleMax) signature) must
  // tolerate the same cleared state and return the same ladder a guarded
  // draw computes.
  w.eval('window.__aggKeep = chartAgg; chartAgg = null; chartView.preset = "overview"; chartData()');
  let resetAxisSafe = true;
  try { compactOpts.axes[0].values({}, [0.5]); } catch { resetAxisSafe = false; }
  const yStub = { valToPos: v => -v * 1000 }; // decades land >= CHART_Y_TICK_PX apart, so a guarded draw keeps the whole ladder
  let clearedY;
  try { clearedY = compactOpts.axes[1].splits(yStub, 1, 0, 100, 28, 1); } catch { resetAxisSafe = false; }
  // uPlot draws series after axes in one queued pass, so the same cleared
  // window also reaches a bar series' disp callbacks: disp.x0.values and
  // disp.size.values both route through chartBarGeometry, which reads the
  // live module state at call time. Drive the builder through a minimal
  // plot stub (the builder needs mode/series/_data/scales/bbox plus the two
  // valToPos callbacks; all-null series values skip the raster loop after
  // the disp calls), then pin the geometry the cleared state must answer
  // with: the hidden-series contract, same as a toggled-off bar.
  const barU = {
    mode: 1,
    data: [[0.5, 1.5], [null, null]],
    _data: [[0.5, 1.5], [null, null]],
    bbox: { left: 0, top: 0, width: 500, height: 140 },
    scales: { x: { ori: 0, dir: 1, distr: 1 }, y: { ori: 0, dir: 1, distr: 1 } },
    series: [{ scale: 'x' }, { scale: 'y', width: 0, min: 0, max: 100, fillTo: () => 0, pxRound: Math.round }],
    valToPosH: (v, k, dim, off) => off + dim * v / 100,
    valToPosV: (v, k, dim, off) => off + dim * (1 - v / 100),
  };
  let resetBarSafe = true, clearedBarGeom = '';
  try {
    compactOpts.series[1].paths(barU, 1, 0, 1);
    clearedBarGeom = w.eval('JSON.stringify(chartBarGeometry(1))');
  } catch { resetBarSafe = false; }
  w.eval('chartAgg = window.__aggKeep; chartView.preset = "traffic"; chartData()');
  const guardedY = compactOpts.axes[1].splits(yStub, 1, 0, 100, 28, 1);
  // The live plan through the same builder proves the drive is real: a
  // visible grouped bar must answer with its slot geometry, never the
  // degenerate contract (the stub cannot vacuously "pass" the cleared
  // drives below).
  let guardedBarSafe = true, guardedBarGeom = '';
  try {
    compactOpts.series[1].paths(barU, 1, 0, 1);
    guardedBarGeom = w.eval('JSON.stringify(chartBarGeometry(1))');
  } catch { guardedBarSafe = false; }
  w.eval('chartAgg = null');
  // The hand-cleared payload state with the plan still live: chartAgg is
  // never nulled after boot (a missing payload is real only before the
  // first payload arrives), so this row is defense-in-depth, pinning the
  // deny-by-default guard's tolerance of the deref shape the overview
  // state never reaches (chartAgg.bucket_ms read on a null payload, with
  // the traffic plan's meta row present and visible).
  let clearedPayloadBarSafe = true, clearedPayloadBarGeom = '';
  try {
    compactOpts.series[1].paths(barU, 1, 0, 1);
    clearedPayloadBarGeom = w.eval('JSON.stringify(chartBarGeometry(1))');
  } catch { clearedPayloadBarSafe = false; }
  check('a queued axis callback tolerates a hand-cleared chart payload',
    resetAxisSafe && JSON.stringify(clearedY) === JSON.stringify(guardedY));
  check('a queued bar disp callback tolerates a hand-cleared chart payload',
    resetBarSafe && clearedPayloadBarSafe && guardedBarSafe &&
    clearedBarGeom === '{"offset":0,"size":0}' && clearedPayloadBarGeom === '{"offset":0,"size":0}' &&
    guardedBarGeom !== '{"offset":0,"size":0}');
  // The sticky-_compacted window: a queued draw can also land after a
  // hole-bucket compaction armed _vis/_compacted and the payload was
  // hand-cleared - defense-in-depth, since chartAgg is never nulled after
  // boot (the production stale-_vis window is the blank-state early
  // return pinned below). The guard's (!_compacted && !chartAgg) arm must
  // NOT fire: the compacted step is 1 with no payload read, so the bar
  // answers with its live compacted geometry, never the hidden contract.
  w.eval('chartAgg = window.__aggKeep; chartAgg.buckets.forEach((b, i) => { b.req = i === 3 || i === 7 ? 1 : 0; }); chartData()');
  let liveCompactedSafe = true, liveCompactedGeom = '';
  try {
    compactOpts.series[1].paths(barU, 1, 0, 1);
    liveCompactedGeom = w.eval('JSON.stringify(chartBarGeometry(1))');
  } catch { liveCompactedSafe = false; }
  w.eval('chartAgg = null');
  let stickyCompactedSafe = true, stickyCompactedGeom = '';
  try {
    compactOpts.series[1].paths(barU, 1, 0, 1);
    stickyCompactedGeom = w.eval('JSON.stringify(chartBarGeometry(1))');
  } catch { stickyCompactedSafe = false; }
  check('a queued bar disp callback keeps live geometry in the sticky-compacted cleared state',
    liveCompactedSafe && stickyCompactedSafe && stickyCompactedGeom === liveCompactedGeom &&
    stickyCompactedGeom !== '{"offset":0,"size":0}');

  // The stale-_vis window: the rebuild takes its blank-state early return
  // BEFORE the _vis/_compacted update, so a shorter non-null payload
  // swapped in through that window leaves _vis indexing the previous,
  // longer payload (chartAgg itself is never nulled after boot). A queued
  // x-values draw then resolves a stale out-of-range bucket index - the
  // same queued-draw family as the chartYTicks and chartBarGeometry rows
  // above.
  w.eval(`
    chartAgg = window.__aggKeep;
    chartAgg.buckets.forEach((b, i) => { b.req = i === 3 || i === 7 ? 1 : 0; });
    chartView.preset = 'traffic'; chartData();
  `);
  const shortAgg = mkChart();
  shortAgg.now_ms = CHART_FROM + 2 * BUCKET_MS;
  shortAgg.buckets = shortAgg.buckets.slice(0, 2); // shorter, non-null
  // Every bucket fails the traffic keep filter, so this rebuild takes the
  // blank-state early return and leaves _vis/_compacted untouched.
  const staleEarlyReturn = w.eval(`chartAgg = ${JSON.stringify(shortAgg)}; chartData() === null`);
  const staleVisState = w.eval('JSON.stringify([_vis, _compacted])');
  let staleXSafe = true, staleXLabels = '';
  try { staleXLabels = JSON.stringify(compactOpts.axes[0].values({}, [0.5, 1.5])); }
  catch { staleXSafe = false; }
  // The same drive as an uncaught page error: the throw a real queued draw
  // surfaces must reach the harness-start jsdomError listener (failures
  // plus stderr), not only this row's caught flag.
  w.eval(`
    const w39drive = () => { upOpts(600, 180).axes[0].values({}, [0.5, 1.5]); };
    window.addEventListener('w39-stale-x', w39drive);
    window.dispatchEvent(new Event('w39-stale-x'));
    window.removeEventListener('w39-stale-x', w39drive);
  `);
  w.eval(`
    chartAgg = window.__aggKeep;
    chartAgg.buckets.forEach((b, i) => { b.req = i === 3 || i === 7 ? 1 : 0; });
    chartData();
  `);
  let liveXLabels = null;
  try { liveXLabels = compactOpts.axes[0].values({}, [0.5]); } catch { liveXLabels = null; }
  check('a queued x-values callback blanks stale ticks instead of dereferencing the swapped payload',
    staleEarlyReturn === true && staleVisState === '[[3,7],true]' &&
    staleXSafe && staleXLabels === '["",""]' &&
    Array.isArray(liveXLabels) && liveXLabels[0] === w.chartXTick(CHART_FROM + 3 * BUCKET_MS));

  // ---- test 11f: dash.chart restore (deny by default) ----
  // A legacy FLAT hidden array is discarded wholesale (the old shape carried
  // no per-preset mapping), garbage JSON keeps the defaults, and every field
  // validates independently against its owner list.
  const CHART_DEFAULTS = { window: 'all', pct: 95, preset: 'traffic', hidden: {} };
  check('chart range control comes from the one curated range registry',
    [...d.querySelectorAll('#chart-window option')].map(o => o.value).join(',') === w.eval('CHART_WINDOWS.map(([v]) => v).join(",")'));
  check('chart offers all history plus short, daily, weekly, monthly and yearly ranges',
    ['all', '15', '60', '360', '1440', '10080', '43200', '129600', '525600'].every(v =>
      [...d.querySelectorAll('#chart-window option')].some(o => o.value === v)));
  check('multi-day bucket durations use the shared duration formatter', w.eval('fmtDur(1209600000)') === '14d');
  w.eval(`
    storage.set('dash.chart', JSON.stringify({ window: '60', pct: 99, preset: 'cost', hidden: ['req'] }));
    chartView = ${JSON.stringify(CHART_DEFAULTS)};
    loadChartView();
  `);
  check('legacy flat-array hidden is discarded, valid fields still apply',
    w.eval('JSON.stringify(chartView)') === JSON.stringify({ window: '60', pct: 99, preset: 'cost', hidden: {} }));
  w.eval(`
    storage.set('dash.chart', '{not json');
    chartView = ${JSON.stringify(CHART_DEFAULTS)};
    loadChartView();
  `);
  check('garbage JSON keeps the defaults',
    w.eval('JSON.stringify(chartView)') === JSON.stringify(CHART_DEFAULTS));
  w.eval(`
    storage.set('dash.chart', JSON.stringify({ window: '9999', pct: 42, preset: 'nope', hidden: { tokens: ['cache'], traffic: ['bogus'] } }));
    chartView = ${JSON.stringify(CHART_DEFAULTS)};
    loadChartView();
  `);
  check('unknown values drop per-field; unknown hidden ids are filtered',
    w.eval('JSON.stringify(chartView)') === JSON.stringify({ window: 'all', pct: 95, preset: 'traffic', hidden: { tokens: ['cache'] } }));
  for (const window of ['all', '10080', '43200', '129600', '525600']) {
    w.eval(`storage.set('dash.chart', JSON.stringify({window: '${window}'})); loadChartView()`);
    check(`chart range ${window} restores from saved preferences`, w.eval('chartView.window') === window);
  }
  w.eval("chartView.hidden = {}; storage.set('dash.chart', JSON.stringify(chartView))");
  w.eval('chartAgg = null');

  // ---- test 11g: REAL zero-traffic chart payload end-to-end (live-verify
  // follow-up). A fresh store + empty ring serves a CLOCK-ALIGNED
  // ZERO-FILLED buckets array (the fetch stub's chartPayload mirrors
  // HandleAggChart's exact empty-store shape - count >= 1, never []). On
  // that shape the merged chart must show the BLANK, never mount an
  // all-zero plot, for every preset. The legend and totals strip still
  // render (they precede the mount gate), and 'no traffic yet' is
  // CANVAS-PAINTED (drawBlank fillText), never DOM text. The jsdom size
  // guard is neutralized on the chart box for this block only, so the
  // empty gate is the only thing that can block a mount - without this,
  // a jsdom renderChart check passes vacuously behind clientWidth 0.
  {
    const box = d.getElementById('chart-traffic');
    Object.defineProperty(box, 'clientWidth', { value: 600, configurable: true });
    Object.defineProperty(box, 'clientHeight', { value: 150, configurable: true });
    w.eval("window.__blankMsgs = []; window.__drawBlankOrig = drawBlank; drawBlank = (ctx, msg) => { window.__blankMsgs.push(msg); window.__drawBlankOrig(ctx, msg); };");
    w.eval("chartView = { window: '60', pct: 50, preset: 'cost', hidden: {} }");
    w.eval("fetchChart('invalidate')");
    await sleep(30);
    check('zero-traffic server payload is zero-FILLED buckets, never an empty array',
      w.eval('chartAgg && chartAgg.buckets.length >= 1 && chartAgg.buckets.every(b => b.req === 0)') === true);
    check('zero-traffic merged chart reads as empty (chartIsEmpty)',
      w.eval('chartIsEmpty()') === true);
    check('bar preset yields no data on all-zero buckets (no compaction slots)',
      w.eval('chartData()') === null);
    w.renderChart();
    check('no uPlot mounts on the zero-traffic payload (blank, not an all-zero plot)',
      !box.querySelector('div.uplot') && w.eval('_up') === null);
    const blankMsgs = w.eval('window.__blankMsgs');
    check("the blank is painted: 'no traffic yet' (canvas drawBlank, not DOM text)",
      Array.isArray(blankMsgs) && blankMsgs.length >= 1 && blankMsgs.every(m => m === 'no traffic yet') &&
      !box.textContent.includes('no traffic yet'));
    check('legend + totals still render ahead of the gate (the card stays alive)',
      [...d.querySelectorAll('#traffic-legend .leg-item')].map(b => b.dataset.series).join() === 'cost,req' &&
      d.getElementById('chart-totals').textContent.includes('cost'));
    // latency preset: zero traffic fails the requireAll keep too - compaction
    // empties the window, so the blank arrives through both gates
    w.eval("chartView.preset = 'latency'");
    w.renderChart();
    check('latency preset stays blank on zero traffic (compaction and empty gate agree)',
      w.eval('chartData()') === null && !box.querySelector('div.uplot') && w.eval('_up') === null);
    // honest-data rule intact: SOME traffic must still plot - but a sparse
    // window cannot fill the plot width, so the mount gate holds until the
    // fourth populated bucket. The blank names the sparseness, not the
    // traffic.
    w.eval("chartView.preset = 'cost'; chartAgg.buckets[7].req = 1");
    w.renderChart();
    check('a single populated bucket paints the sparse blank, not a stranded bar',
      w.eval('chartData() !== null && chartData()[0].length === 1') &&
      !box.querySelector('div.uplot') && w.eval('_up') === null &&
      w.eval('window.__blankMsgs').at(-1) === 'not enough data points yet');
    w.eval("chartAgg.buckets[5].req = 1; chartAgg.buckets[6].req = 1");
    w.renderChart();
    check('three populated buckets stay behind the sparse gate',
      w.eval('chartData()[0].length === 3') &&
      !box.querySelector('div.uplot') && w.eval('window.__blankMsgs').at(-1) === 'not enough data points yet');
    w.eval("chartAgg.buckets[8].req = 1");
    w.renderChart();
    check('the fourth populated bucket un-blanks and mounts the plot',
      w.eval('chartData()[0].length === 4') &&
      !!box.querySelector('div.uplot') && w.eval('_up') !== null);
    // traffic without timing samples: compaction empties the latency window
    // while traffic exists, so the blank names the preset, not the traffic
    w.eval("chartView.preset = 'latency'");
    w.renderChart();
    check("traffic without timing samples paints its own blank, not 'no traffic yet'",
      w.eval('chartData()') === null && w.eval('chartIsEmpty()') === false &&
      !box.querySelector('div.uplot') && w.eval('window.__blankMsgs').at(-1) === 'no speed + latency samples yet');
    // restore: tear the mounted plot down, unspy, clear overrides + state
    w.eval("if (_up) { _up.destroy(); _up = null; } drawBlank = window.__drawBlankOrig; chartAgg = null; chartView = { window: 'all', pct: 95, preset: 'traffic', hidden: {} }");
    for (const c of box.querySelectorAll('canvas.chart-blank')) c.remove();
    delete box.clientWidth;
    delete box.clientHeight;
  }

  // ---- test 11h: Overview preset - the summary, metrics-only ----
  // No plot: the tiles ARE the surface and take the card. Every tile reads
  // value first, then its measured companion fact, then its per-bucket
  // evolution sparkline; the merged timing tile carries the server's period
  // averages with each metric's low-high sample range - no percentile
  // selector or visible pXX label. The tiles are plain readouts; the
  // metrics picker at the card head owns what shows through the legend's
  // hidden-set contract: flips persist in dash.chart, hidden metrics leave
  // the band, unknown ids drop on load.
  w.eval(`
    chartAgg = window.__cp = ${JSON.stringify(mkChart())};
    chartView.preset = 'overview'; chartView.hidden = {}; chartView.pct = 95;
    chartAgg.ttft_p = [110, 220, 330]; chartAgg.tps_p = [101.25, 202.75, 303.5];
    chartAgg.ttft_stat = [220, 10, 330]; chartAgg.tps_stat = [202.75, 50, 300];
    chartAgg.cost_per_mtok = 12.5;
    chartAgg.buckets[3].req = 10; chartAgg.buckets[3].err = 2; chartAgg.buckets[3].rl = 1; chartAgg.buckets[3].cost = 1.25;
    chartAgg.buckets[3].in = 100; chartAgg.buckets[3].out = 40; chartAgg.buckets[3].cache = 60;
    chartAgg.buckets[5].req = 4; chartAgg.buckets[5].err = 1; chartAgg.buckets[5].cost = 0.5;
    chartAgg.buckets[5].in = 50; chartAgg.buckets[5].out = 10; chartAgg.buckets[5].cache = 0;
    chartAgg.buckets[2].ttft = [10, 20, 30]; chartAgg.buckets[2].tps = [100, 200, 300];
    chartAgg.buckets[3].ttft = [5, 10, 15]; chartAgg.buckets[3].tps = [50, 60, 70];
  `);
  check('the summary has no plotted rows and renders no data columns',
    w.eval('chartPlan().meta.length') === 0 && w.eval('chartData()') === null &&
    w.eval('_vis') === null && w.eval('_compacted') === false);
  w.renderChart();
  check('the summary card carries tiles-only and never mounts a plot or blank canvas',
    d.querySelector('.traffic-card').classList.contains('tiles-only') &&
    w.eval('_up') === null && !d.querySelector('#chart-traffic canvas.chart-blank') &&
    d.getElementById('chart-pct').hidden &&
    d.getElementById('chart-axes').textContent === '');
  const totalsOv = w.eval('chartTotals()');
  check('overview totals carry every headline metric for the period',
    totalsOv.includes('requests / tokens</span> <span class="val-pair"><span class="v-req">14</span><span class="pair-sep">/</span><span class="v-tok">200</span>') &&
      !totalsOv.includes('200 tokens') &&
      totalsOv.includes('cost</span> $1.75') &&
      totalsOv.includes('<span class="chart-sub">of 14 requests</span>') &&
      !totalsOv.includes('blended</span>'));
  check('the health pair reads errors/429 in the chart line colors over the denominator',
    totalsOv.includes('errors / 429</span> <span class="val-pair"><span class="v-err">3</span><span class="pair-sep">/</span><span class="v-rl">1</span>') &&
    !totalsOv.includes('rate limited</span>'));
  check('the tokens triple reads in/out/cached in the plot colors with both named shares',
    totalsOv.includes('tokens in/out/cached</span> <span class="val-pair"><span class="v-in">150</span><span class="pair-sep">/</span><span class="v-out">50</span><span class="pair-sep">/</span><span class="v-cache">60</span>') &&
      totalsOv.includes('<span class="chart-sub">input 75.0% of tokens<br>cache hit 40.0% of input</span>') &&
      !totalsOv.includes('in:out ') && !totalsOv.includes('data-tile'));
  check('overview totals annotate the server blended price',
    totalsOv.includes('$12.5 /Mtok') &&
    !totalsOv.includes('/ req'));
  check('the pair stays tile-bounded and its share never fakes 100%', (() => {
    const saved = w.eval('JSON.stringify(chartAgg.buckets[3])');
    w.eval('chartView.preset = "tokens"; chartAgg.buckets[3].in = 1000000; chartAgg.buckets[3].out = 3;');
    const wide = w.eval('chartTotals()');
    w.eval('chartAgg.buckets[3] = JSON.parse(' + JSON.stringify(saved) + '); chartView.preset = "overview";');
    // period tin = 1000050, tout = 13: the pair compact-bounds each half
    // ('1M/13') and the share caps below 100% - pctCap never rounds a
    // strictly-sub-100 ratio up to a false '100.0%'.
    return wide.includes('<span class="v-in">1M</span><span class="pair-sep">/</span><span class="v-out">13</span>') &&
      !wide.includes('76926') && wide.includes('<span class="chart-sub">99.9% of tokens</span>');
  })());
  // The tokens tile's mini-chart: one self-scaled spark line per metric half
  // (in accent2, out ok, cache muted) over the kept buckets. Crafted series
  // with distinct shapes pin that each rendered line traces its OWN input
  // series, in its half's palette color.
  check('the tokens tile spark renders the crafted in/out/cache series', (() => {
    const saved = w.eval('JSON.stringify([chartAgg.buckets[3], chartAgg.buckets[5]])');
    w.eval('chartAgg.buckets[3].in = 10; chartAgg.buckets[3].out = 60; chartAgg.buckets[3].cache = 30;' +
      'chartAgg.buckets[5].in = 60; chartAgg.buckets[5].out = 10; chartAgg.buckets[5].cache = 30;');
    const t = w.eval('chartTotals()');
    w.eval('const b = JSON.parse(' + JSON.stringify(saved) + '); chartAgg.buckets[3] = b[0]; chartAgg.buckets[5] = b[1];');
    const seg = t.slice(t.indexOf('title="tokens in/out/cached'), t.indexOf('title="cost'));
    const byColor = {};
    for (const [, dattr, stroke] of seg.matchAll(/<path d="([^"]+)" fill="none" stroke="([^"]+)"/g)) byColor[stroke] = dattr;
    // kept buckets 3 and 5: in [10, 60] rises, out [60, 10] falls, cache
    // [30, 30] is flat - each line can only come from its own series.
    return Object.keys(byColor).length === 3 &&
      byColor[w.eval('COLORS.accent2')] === 'M0.0 14.0L72.0 0.0' &&
      byColor[w.eval('COLORS.ok')] === 'M0.0 0.0L72.0 14.0' &&
      byColor[w.eval('COLORS.muted')] === 'M0.0 14.0L72.0 14.0';
  })());
  check('every summary tile is a plain readout carrying one multi-line spark',
    (totalsOv.match(/class="chart-total"/g) || []).length === 5 &&
      totalsOv.split('<svg class="spark"').length === 6 &&
      (totalsOv.match(/<svg class="spark"[^>]*width="72" height="14"/g) || []).length === 5 &&
      !totalsOv.includes('spark-row') &&
      !totalsOv.includes('role="button"') &&
      !totalsOv.includes('data-tile') &&
      !totalsOv.includes('NaN'));
  check('tile sparks span kept buckets only, one self-scaled line per half', (() => {
    // The fixture carries traffic in exactly two buckets (3 and 5), so each
    // spark line over the kept buckets has one M + one L command; the raw
    // ladder would draw ~30. The requests tile draws two lines (requests +
    // token volume) in the pair halves' colors.
    const seg = totalsOv.slice(totalsOv.indexOf('title="requests / tokens'), totalsOv.indexOf('title="tokens in/out/cached'));
    const kept = w.eval('chartAgg.buckets.filter(b => b.req > 0).length');
    const paths = (seg.match(/<path /g) || []).length;
    const cmds = (seg.match(/[ML](?=[0-9])/g) || []).length;
    return kept === 2 && paths === 2 && cmds === kept * paths;
  })());
  // Contract: TILE_HALVES is the one owner of the half-class -> spark-color
  // pairing. Every registry entry must actually wire its class (tilePair)
  // to its palette key (tileSpark), and the rendered summary must never
  // emit a half class the registry does not know - the class and its spark
  // color cannot drift apart silently.
  check('every tile half class pairs with its spark color through the one registry', (() => {
    const halves = w.eval('TILE_HALVES');
    const wired = Object.entries(halves).every(([cls, color]) => {
      const pair = w.eval(`tilePair([['${cls}', 7]])`);
      const sparkHtml = w.eval(`tileSpark([1, 2], [['${cls}', v => v]])`);
      return pair.includes(`<span class="${cls}">7</span>`) &&
        sparkHtml.includes(`stroke="${w.eval(`COLORS[${JSON.stringify(color)}]`)}"`);
    });
    const emitted = [...totalsOv.matchAll(/class="(v-[a-z]+)"/g)].map(m => m[1]);
    return Object.keys(halves).length === 9 && wired &&
      emitted.length === 9 && emitted.every(cls => halves[cls]);
  })());
  // Real palette pin: the registry test above proves the half class pairs
  // with its palette KEY; this proves the VALUES. jsdom's CSSOM resolves the
  // inlined dashboard.css :root custom properties, so the palette registry
  // the tiles and sparks consume (COLORS, read at boot from the computed
  // style) must hold the real declared values - not the empty strings a
  // failed resolution or a renamed property would leave behind - and every
  // rendered spark must stroke one of those declared colors. Only a run
  // against the live style sheet can see this; the repository_check mirror
  // detector only compares static file text.
  check('the tile palette registry holds the real declared :root palette values', (() => {
    const cs = w.getComputedStyle(d.documentElement);
    const keys = w.eval('Object.keys(COLORS).filter(k => k !== "grid")');
    const declared = Object.fromEntries(keys.map(k => [k, cs.getPropertyValue('--' + k).trim()]));
    const hex = v => /^#[0-9a-f]{6}$/i.test(v);
    const halves = w.eval('TILE_HALVES');
    const consumed = Object.entries(halves).every(([, key]) =>
      hex(declared[key]) && w.eval(`COLORS[${JSON.stringify(key)}]`) === declared[key]);
    const strokes = [...totalsOv.matchAll(/ stroke="([^"]+)"/g)].map(m => m[1]);
    return keys.length === 9 && Object.keys(halves).length === 9 && consumed &&
      strokes.length > 0 && strokes.every(s => Object.values(declared).includes(s));
  })());
  // Shared CSSOM reader for the declared-value pins below: the FIRST rule
  // matching a selector (media overrides later in the sheet - e.g. the
  // reduced-motion .pill.live animation reset - must not shadow the
  // top-level declaration the pin reads).
  const firstCSSRule = sel => {
    const visit = list => { for (const r of list) { if (r.selectorText === sel) return r; if (r.cssRules && r.cssRules.length) { const got = visit(r.cssRules); if (got) return got; } } };
    for (const sheet of d.styleSheets) { const got = visit(sheet.cssRules); if (got) return got; }
    return null;
  };
  // Summary-blend pin: the tiles-only preset replaces the boxed tile chrome
  // with the KPI band's cell language (the tiles are plain readouts now -
  // the metrics picker owns what shows - so the old hover lift and toggle
  // affordance rules are retired with it; the explorer category tile keeps
  // its own card language). The override must (a) keep the per-tile box
  // gone (background none, hairline left border, first cell open) so cells
  // read as blended readouts, not nested cards, (b) close the band through
  // the shared tick strip like the KPI band instead of a hard rule, and
  // (c) leave no interactive tile rule behind - a reintroduced
  // role="button" affordance redds here without a browser.
  check('the summary tiles blend into the card like the KPI band cells', (() => {
    const flat = firstCSSRule('.traffic-card.tiles-only .chart-total');
    const first = firstCSSRule('.traffic-card.tiles-only .chart-total:first-child');
    const strip = firstCSSRule('.traffic-card.tiles-only .chart-totals::after');
    return !!flat && !!first && !!strip &&
      !firstCSSRule('.chart-total[role="button"]') &&
      !firstCSSRule('.chart-total.off') &&
      flat.style.getPropertyValue('background') === 'none' &&
      flat.style.getPropertyValue('border-radius') === '0px' &&
      flat.style.getPropertyValue('border-left') !== '' &&
      first.style.getPropertyValue('border-left') === '0px' &&
      strip.style.getPropertyValue('background') === 'var(--tick-strip)';
  })());
  // Uniform-tile pin: every summary tile is ONE fixed size whatever it
  // carries (spark or not, one share line or the tokens tile's two -
  // skeleton or data - the no-jump skeleton swap is exact by construction).
  // The 100px floor fits the tallest structure (label + value + two share
  // rows + spark = 95, line-height-1 rows) and the spark refuses to flex-shrink,
  // so an oversized tile clips visibly for the browser gate instead of
  // silently crushing the spark - the failure mode the 72px attempt
  // exposed. The band sits at its natural rows height (the old
  // flex-basis-0 stretch both bloated tiles on the locked desktop pair and
  // collapsed the strip to a sliver on
  // content-height cards), and the card opts out of the pair's stretch so
  // the summary never paints a hollow tall card.
  check('summary tiles keep one fixed size on a content-height band', (() => {
    const tile = firstCSSRule('.traffic-card.tiles-only .chart-total');
    const strip = firstCSSRule('.traffic-card.tiles-only .chart-totals');
    const card = firstCSSRule('.traffic-card.tiles-only');
    const spark = firstCSSRule('.traffic-card.tiles-only .chart-total .spark');
    return !!tile && !!strip && !!card && !!spark &&
      tile.style.getPropertyValue('height') === '100px' &&
      spark.style.getPropertyValue('flex-shrink') === '0' &&
      strip.style.getPropertyValue('flex') === '' &&
      strip.style.getPropertyValue('min-height') === '' &&
      strip.style.getPropertyValue('grid-auto-rows') === '' &&
      card.style.getPropertyValue('align-self') === 'start';
  })());
  // Pill alpha pin: the status pills' tinted backgrounds share one ladder -
  // --pill-tint at rest, --pill-tint-strong once a pill flags an
  // operator-visible state (paused, throttled, debug) - and the live pill's
  // 15% is its documented one-off mid tint (dashboard.css's token block).
  // Assert the DECLARED values: a pill re-stating its strength as a literal
  // (the .pill.cancel rgba regression) or drifting to the other tier redds
  // here without a browser.
  check('every status pill derives its background alpha from the shared tint ladder', (() => {
    const bg = sel => firstCSSRule(sel)?.style.getPropertyValue('background') || '';
    return [
      ['.pill.ok', 'var(--pill-tint)'],
      ['.pill.err', 'var(--pill-tint)'],
      ['.pill.warn', 'var(--pill-tint)'],
      ['.pill.cancel', 'var(--pill-tint)'],
      ['.pill.paused', 'var(--pill-tint-strong)'],
      ['.pill.throttled', 'var(--pill-tint-strong)'],
      ['.pill.debug', 'var(--pill-tint-strong)'],
      ['.pill.live', '15%, transparent'],
    ].every(([sel, want]) => bg(sel).includes(want));
  })());
  // Tick-strip pin: the seven graduated-scale edges (header and footer
  // rails, KPI band, summary tiles band, section head and foot, day rule)
  // all paint the signature motif through --tick-strip - the one authored
  // gradient pair. A site reverting to a hand-written gradient (or a plain
  // rule) once went green; the declared-value pin makes each site exact.
  check('the seven graduated-scale edges all paint through the --tick-strip token', (() => {
    const sites = ['header::after', '.kpis::after', '.traffic-card.tiles-only .chart-totals::after', '.st-hd::after', '.st-ft::before', '.day-rule', 'footer::before'];
    return sites.length === 7 && sites.every(sel => {
      const r = firstCSSRule(sel);
      return !!r && r.style.getPropertyValue('background') === 'var(--tick-strip)';
    });
  })());
  // Focus-contract pin: the focusable editor surfaces - the operator
  // dialog's input, the settings search, the settings rows, the provider
  // card header, the provider section inputs and the picker's custom-label
  // input - declare the SAME accessible focus trio (accent border-color,
  // outline: none, the 3px focus ring). The groups are deliberately NOT
  // merged into one selector (each section stays decoupled, the W32
  // hover-contract precedent), so a one-sided edit could drift silently;
  // the declared-value pin makes every group exact and non-vacuous.
  check('every focusable editor surface declares one identical focus contract', (() => {
    const sels = [
      '.operator-dialog input:focus',
      '.st-hd input[type="search"]:focus',
      '.st-ctl input:focus, .st-ctl textarea:focus',
      '.prov-hd .sp-label:focus',
      '.prov-sec input:focus, .prov-sec select:focus',
      '.prov-menu-custom input:focus',
    ];
    const rules = sels.map(sel => firstCSSRule(sel));
    if (rules.length !== 6 || rules.some(r => !r)) return false;
    const props = ['border-color', 'outline', 'box-shadow'];
    return props.every(p => {
      const v = rules[0].style.getPropertyValue(p);
      return v !== '' && rules.every(r => r.style.getPropertyValue(p) === v);
    });
  })());
  check('the summary hides the bucket-cadence context row',
    d.getElementById('chart-context').hidden &&
      w.eval('chartView.preset') === 'overview');
  check('the merged timing tile reads avg latency / speed with sample ranges',
    totalsOv.includes('avg latency / speed</span> <span class="val-pair"><span class="v-ttft">220ms</span><span class="pair-sep">/</span><span class="v-tps">202.75</span>') &&
      totalsOv.includes('<span class="chart-sub">10-330ms · 50-300/s</span>') &&
      !totalsOv.includes('101.25') && !totalsOv.includes('110ms') &&
      !/p(?:50|95|99)/.test(totalsOv) && !totalsOv.includes('tok/s'));
  check('the percentile dropdown never re-gates the summary timing tile', (() => {
    w.setChartPct('50');
    const t50 = w.eval('chartTotals()');
    w.setChartPct('99');
    w.eval('chartTotals()');
    w.setChartPct('95');
    return t50.includes('220ms') && t50.includes('202.75') &&
      w.eval('chartPlan().meta.length') === 0;
  })());
  // The timing tile's spark lines draw the per-bucket 95TH-percentile series
  // (CHART_TILE_PCT), independent of the percentile dropdown. Crafted
  // triples where p50, p95 and p99 have different shapes pin the selection:
  // only the p95 series produces the drawn paths.
  check('the timing tile spark selects the per-bucket p95 series', (() => {
    const saved = w.eval('JSON.stringify([chartAgg.buckets[3], chartAgg.buckets[5]])');
    w.eval('chartAgg.buckets[3].ttft = [5, 30, 5]; chartAgg.buckets[3].tps = [10, 60, 10];' +
      'chartAgg.buckets[5].ttft = [30, 5, 5]; chartAgg.buckets[5].tps = [60, 10, 10];');
    const t = w.eval('chartTotals()');
    w.eval('const b = JSON.parse(' + JSON.stringify(saved) + '); chartAgg.buckets[3] = b[0]; chartAgg.buckets[5] = b[1];');
    const seg = t.slice(t.indexOf('title="avg latency / speed'));
    const paths = [...seg.matchAll(/<path d="([^"]+)" fill="none" stroke="([^"]+)"/g)];
    // kept buckets 3 and 5: the p95 triples ([30, 5] ttft, [60, 10] tps) are
    // falling lines, while p50 ([5, 30] / [10, 60]) rises and p99 ([5, 5] /
    // [10, 10]) is flat - the drawn shapes can only be the 95th percentile.
    return paths.length === 2 &&
      paths.every(([, dattr]) => dattr === 'M0.0 0.0L72.0 14.0') &&
      paths.some(([, , stroke]) => stroke === w.eval('COLORS.accent')) &&
      paths.some(([, , stroke]) => stroke === w.eval('COLORS.ok'));
  })());
  // The metrics picker: the card-head trigger opens the menu (the prov-menu
  // pattern), rows flip the same dash.chart hidden set, hidden metrics leave
  // the band (no stub - the grid reflows), and the trigger's count and
  // accessible name always tell the truth. The menu stays open across row
  // flips - it is multi-select - and a flip must not rebuild the rows, so
  // an open menu never loses focus.
  const mBtn = d.getElementById('chart-metrics-btn');
  const mMenu = d.getElementById('chart-metrics-menu');
  check('the picker control shows for the summary with all rows checked and a truthful count',
    !d.getElementById('chart-metrics').hidden &&
      mBtn.getAttribute('aria-haspopup') === 'menu' &&
      [...mMenu.querySelectorAll('.metrics-row')].map(r => r.dataset.metric).join() === 'req,tokens,cost,health,timing' &&
      [...mMenu.querySelectorAll('.metrics-row')].every(r => r.getAttribute('aria-checked') === 'true' && r.querySelector('.mp-check').textContent === '✓') &&
      d.getElementById('chart-metrics-count').textContent === '5/5' &&
      mBtn.getAttribute('aria-label') === 'Summary metrics: 5 of 5 shown');
  mBtn.click();
  check('the picker trigger opens the menu and focuses its first row',
    !mMenu.hidden && mBtn.getAttribute('aria-expanded') === 'true' &&
      d.activeElement === mMenu.querySelector('.metrics-row'));
  w.eval('window.__mrow0 = document.getElementById("chart-metrics-menu").querySelector(".metrics-row")');
  const tokRow = [...mMenu.querySelectorAll('.metrics-row')].find(r => r.dataset.metric === 'tokens');
  tokRow.click();
  const totalsHidden = w.eval('chartTotals()');
  check('a picker flip drops the tile from the band and updates row, count and name',
    (totalsHidden.match(/class="chart-total"/g) || []).length === 4 &&
      !totalsHidden.includes('title="tokens in/out/cached') && !totalsHidden.includes('75.0% of tokens') &&
      tokRow.getAttribute('aria-checked') === 'false' && tokRow.querySelector('.mp-check').textContent === '' &&
      d.getElementById('chart-metrics-count').textContent === '4/5' &&
      mBtn.getAttribute('aria-label') === 'Summary metrics: 4 of 5 shown' &&
      !mMenu.hidden && w.eval('chartView.hidden.overview.join()') === 'tokens' &&
      w.eval('window.__mrow0 === document.getElementById("chart-metrics-menu").querySelector(".metrics-row")'));
  d.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Escape' }));
  check('Escape closes the menu and returns focus to the trigger',
    mMenu.hidden && mBtn.getAttribute('aria-expanded') === 'false' && d.activeElement === mBtn);
  mBtn.click();
  check('reopening the picker carries the stored selection',
    !mMenu.hidden && tokRow.getAttribute('aria-checked') === 'false' &&
      d.getElementById('chart-metrics-count').textContent === '4/5');
  d.body.click();
  check('a click outside the picker closes it',
    mMenu.hidden && mBtn.getAttribute('aria-expanded') === 'false');
  mBtn.click();
  tokRow.click();
  check('a second flip restores the tile and re-persists',
    w.eval('chartTotals()').includes('input 75.0% of tokens') &&
      w.eval('(chartView.hidden.overview || []).length') === 0 &&
      d.getElementById('chart-metrics-count').textContent === '5/5');
  d.dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Escape' }));
  check('unknown tile ids never enter hidden state',
    (() => { w.eval('toggleSummaryMetric("bogus")'); return w.eval('(chartView.hidden.overview || []).length') === 0; })());
  w.eval(`storage.set('dash.chart', JSON.stringify({preset: 'overview', hidden: {overview: ['cost', 'bogus', 'inTok']}})); loadChartView();`);
  check('saved summary selection validates hidden ids against the tile list',
    w.eval('chartView.preset === "overview" && chartView.hidden.overview.join() === "cost"'));
  w.eval("chartView.hidden = {}; chartAgg = null; chartView = { window: 'all', pct: 95, preset: 'traffic', hidden: {} }");

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
  restartState.statusGate = new Promise(resolve => { releaseRestartStatus = resolve; });
  restartState.postGate = new Promise(resolve => { releaseRestartPost = resolve; });
  const restartPending = w.restartProxy();
  check('restart action disables immediately while its status check is pending', restartAction.disabled);
  await w.restartProxy(); // a rapid second call must not issue another POST
  releaseRestartStatus();
  restartState.statusGate = null;
  await sleep(20);
  check('only one restart POST is issued and the pending request stays disabled',
    restartState.postCount === 1 && restartAction.disabled && restartAction.textContent.includes('Restarting'));
  await w.fetchRestartStatus();
  check('an idle status refresh cannot re-enable a locally pending restart', restartAction.disabled);
  check('the header indicates busy while keeping progress accessible',
    d.getElementById('btn-restart').hasAttribute('data-busy') && !d.getElementById('btn-restart').disabled);
  releaseRestartPost();
  restartState.postGate = null;
  await restartPending;
  check('restart POST hit the endpoint', restartState.restarted);
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
    restartState.status = { phase, rank };
    await w.fetchRestartStatus();
    check(`server-reported ${phase} disables the restart action`, restartAction.disabled);
  }
  restartState.status = { phase: 'idle', error: 'build failed', rank: 0 };
  await w.fetchRestartStatus();
  check('failed restart restores the action and reports the error',
    !restartAction.disabled && d.getElementById('restart-count').textContent.includes('build failed'));
  restartState.status = { available: false, reason: 'source unavailable' };
  await w.fetchRestartStatus();
  check('unavailable restart stays disabled', restartAction.disabled);
  restartState.status = {};
  await w.fetchRestartStatus();
  // The refusal path through operatorErrorBody: a non-2xx whose body is not
  // JSON-with-an-error (a plain-text operator denial, an HTML 502) must
  // reject with the restart site's designed fallback text, marked as a
  // server answer so the generic transport prefix never wraps it. Nothing
  // reddened before this pin (the W20 mutation round proved the fallback
  // could be dropped silently).
  restartState.postFail = { status: 502 };
  const refusalPostCount = restartState.postCount;
  await w.restartProxy();
  const restartStatusLine = d.getElementById('restart-count');
  check('a non-JSON 502 restart refusal shows the designed fallback, never an empty error',
    restartState.postCount === refusalPostCount + 1 &&
    restartStatusLine.textContent === 'restart failed (502)' &&
    restartStatusLine.dataset.err === '1');
  restartState.postFail = null;
  w.toggleRestartMenu({ stopPropagation() {} });
  check('restart menu closes on second toggle', rmenu.hidden);

  // ---- test 13: restart sweep + weekday clock ----
  // A feed_id change is the page's "new process" signal: the snapshot resets
  // the log AND refreshAfterRestart re-pulls operator state (pause, throttle,
  // effective config) + aggregates in place when frontend assets match.
  const cfgBefore = cfgFetches;
  fire('snapshot', { feed_id: 'feedC', seq: 1, incremental: false,
    records: [{ ...mkRec('restarted1', 200, 1700000200000) }],
    counters: { in_flight: 0, total_requests: 1, total_errors: 0 } });
  await sleep(30);
  check('feed change resets the log to the fresh process snapshot', rows().length === 1 && rows()[0].dataset.id === 'restarted1');
  check('feed change sweeps operator state (config re-fetched in place)', cfgFetches > cfgBefore);
  check('footer clock carries the weekday', /(mon|tues|wednes|thurs|fri|satur|sun)day/i.test(d.getElementById('f-clock').textContent));

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
  // trashIconSVG clones the header Clear button's glyph at render time: the
  // geometry (every path d) must stay authored once, in index.html, and the
  // header-only .hdr-ico class must stay stripped (the .prov-x svg owns this
  // surface's stroke and size).
  {
    const hdrPaths = [...d.getElementById('btn-clear').querySelectorAll('svg path')].map(p => p.getAttribute('d'));
    const clone = card.querySelector('[data-prov-rm] svg');
    check('the card trash icon is the header glyph, minus the header class',
      JSON.stringify([...clone.querySelectorAll('path')].map(p => p.getAttribute('d'))) === JSON.stringify(hdrPaths) &&
      clone.getAttribute('class') === null);
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
    const failuresBefore = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid model-rules draft blocks the save without an uncaught TypeError',
      failures.length === failuresBefore &&
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
    const mrHiddenFailures = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden model-rules offender selects its category and focuses it',
      failures.length === mrHiddenFailures &&
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
    const intFailuresBefore = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden scalar-int offender selects its category and focuses it',
      failures.length === intFailuresBefore &&
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
    await sleep(0);
    check('focus moving between request-override template items keeps the menu open',
      !roTemplateMenu.hidden && roAddButton.getAttribute('aria-expanded') === 'true' && d.activeElement === roSecondItem);
    const roEscape = new w.KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true });
    roSecondItem.dispatchEvent(roEscape);
    check('Escape from the request-override template menu keeps Settings open and restores Add rule focus',
      roEscape.defaultPrevented && !d.getElementById('settings-sheet').hidden && roTemplateMenu.hidden &&
      roAddButton.getAttribute('aria-expanded') === 'false' && d.activeElement === roAddButton);
    roAddButton.click();
    roTemplateMenu.querySelector('[data-ro-tpl="blank"]').dispatchEvent(new w.KeyboardEvent('keydown', { key: '/', bubbles: true, cancelable: true }));
    await sleep(0);
    check('the Settings search shortcut dismisses the request-override template menu',
      !d.getElementById('settings-sheet').hidden && roTemplateMenu.hidden &&
      roAddButton.getAttribute('aria-expanded') === 'false' && d.activeElement === d.getElementById('settings-q'));
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
      const removeFailuresBefore = failures.length;
      captureInputFlash(() => d.getElementById('btn-settings-apply').click());
      check(`${removeCase.label} targets its remove button`,
        failures.length === removeFailuresBefore && d.activeElement === button && button.classList.contains('prov-bad') &&
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
    const collectFailuresBefore = failures.length;
    w.applySettings();
    check('a collect-time duplicate header opens its collapsed card, focuses the offender and sends no POST',
      failures.length === collectFailuresBefore && collectPosts === 0 &&
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
    const earlierCollectFailuresBefore = failures.length;
    w.applySettings();
    check('collect-time duplicate input yields to the first invalid request-override card',
      failures.length === earlierCollectFailuresBefore && earlierCollectPosts === 0 &&
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
    const failuresBefore = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid request-overrides draft blocks the save and focuses the offending row',
      failures.length === failuresBefore &&
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
    const ctlFailuresBefore = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a control-byte header value blocks the save and focuses the value input',
      failures.length === ctlFailuresBefore &&
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
    const conflictFailuresBefore = failures.length;
    captureInputFlash(() => d.getElementById('btn-settings-apply').click());
    check('Apply focuses the conflicting remove button',
      failures.length === conflictFailuresBefore && d.activeElement === conflictButton && conflictButton.classList.contains('prov-bad'));
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

  // ---- sub-conversations editor: the settings dashboard surface ----
  // The W52 spec's settings-editor section: the editor renders the
  // server doc's seeded entries under the conversation category the
  // schema owns, edits collect round-trip to the strict POST shape
  // (client, params, strip), add/remove rows work through the shared
  // delegated wiring, live per-card validation mirrors
  // config.validateSubConversations with the server's message wording,
  // and Apply blocks on the first invalid card before the POST. The
  // client datalist seeds from the same known-set union the ro scope
  // datalists read, and the help-text search surfaces the row like its
  // siblings with the rail badge counting the hit.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields.push(
      { key: 'conversation_idle_gap', category: 'conversation', label: 'Idle gap', help: 'A gap longer than this starts a new conversation for the same client+key.', kind: 'duration', hot_reload: true },
      { key: 'sub_conversations', category: 'conversation', label: 'Sub-conversation tracking', help: 'Per-client tracking of a request body field that carries a sub-conversation identity, exemplified by opencode promptCacheKey.', kind: 'sub_conversations', hot_reload: true },
    );
    doc.categories.push({ id: 'conversation', label: 'Conversations', help: 'How the request log groups turns into conversations.' });
    doc.values.conversation_idle_gap = '15m';
    doc.values.sub_conversations = [
      { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
      { client: 'zed-cli', params: ['threadId'], strip: false },
    ];
    doc.revision = 'sc-r1';
    // The known sets are unioned across the operator states at datalist
    // render time; pin them to the stubbed wire values before the render
    // (the ro section's discipline).
    w.eval(`(() => {
      pauseState = { ...pauseState, [KNOWN_CLIENTS_KEY]: ['c'] };
      debugState = { ...debugState, [KNOWN_CLIENTS_KEY]: ['c'] };
      throttleState = { ...throttleState, [KNOWN_CLIENTS_KEY]: ['c'] };
    })()`);
    w.__scDoc = doc;
    w.eval("settingsCat = 'conversation'; settingsDoc = window.__scDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const scWrap = d.querySelector('#settings-fields [data-kind="sub_conversations"]');
    check('the settings sheet renders the sub-conversations editor under the conversation category',
      !!scWrap && scWrap.closest('.st-row').dataset.cat === 'conversation' &&
      Number(d.querySelector('[data-st-cat="conversation"] .rail-n').textContent) === 2);
    const cards = () => [...scWrap.querySelectorAll('.sc-card')];
    check('seeded entries render as cards with client, ordered params and the strip toggle',
      cards().length === 2 &&
      cards()[0].querySelector('.sc-client').value === 'opencode' &&
      [...cards()[0].querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'promptCacheKey,sessionKey' &&
      cards()[0].querySelector('.sc-strip').checked === true &&
      cards()[1].querySelector('.sc-client').value === 'zed-cli' &&
      !cards()[1].querySelector('.sc-strip').checked);
    check('watermark examples ride the empty fields',
      cards()[0].querySelector('.sc-client').placeholder.includes('e.g. opencode') &&
      cards()[0].querySelector('.prov-add .sc-param-in').placeholder.includes('e.g. promptCacheKey') &&
      [...cards()[0].querySelectorAll('.sc-param')].every(p => p.placeholder.includes('e.g. promptCacheKey')));
    check('the entry count line renders against the server cap',
      scWrap.querySelector('.mr-count').textContent === '2 / 16 entries');
    check('the client datalist seeds from the same known sets the ro scope datalists read',
      cards()[0].querySelector('.sc-client').getAttribute('list') === 'sc-dl-client' &&
      [...scWrap.querySelectorAll('#sc-dl-client option')].map(o => o.value).join(',') === 'c');
    check('the strip label states the forward and strip semantics',
      cards()[0].querySelector('.st-check').textContent.includes('strip') &&
      cards()[0].querySelector('.st-check').textContent.includes('forwards it unchanged'));
    check('collect round-trips the entries losslessly in the strict wire shape',
      JSON.stringify(w.collectSettingsValues().sub_conversations) === JSON.stringify([
        { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
        { client: 'zed-cli', params: ['threadId'], strip: false },
      ]));
    check('the clean sub-conversations draft reports no unsaved changes',
      d.getElementById('settings-count').textContent === '' && !w.settingsIsDirty());
    // The help-text search: the same label + key + help haystack the
    // siblings ride surfaces the row, and the category rail badge counts
    // the hit (the GAP-5 filter pattern).
    {
      const q = d.getElementById('settings-q');
      q.value = 'promptCacheKey';
      w.filterSettings();
      const visible = [...d.querySelectorAll('#settings-fields .st-row')].filter(r => !r.hidden).map(r => r.dataset.key);
      check('the help-text search surfaces the sub-conversations row',
        JSON.stringify(visible) === JSON.stringify(['sub_conversations']) &&
        Number(d.querySelector('[data-st-cat="conversation"] .rail-n').textContent) === 1 &&
        d.querySelector('[data-st-cat="conversation"]').classList.contains('has-hit'));
      q.value = '';
      w.filterSettings();
    }
    // add/remove param rows through the real controls; the add row trims
    // and enforces the grammar and the cap (deny by default: a refusal
    // flashes the add input and adds nothing).
    const zed = () => cards()[1];
    const zedAddIn = () => zed().querySelector('.prov-add .sc-param-in');
    zedAddIn().value = 'cacheKey';
    zed().querySelector('[data-sc-param-add]').click();
    check('the param add button appends a row, clears the add input and marks the sheet dirty',
      [...zed().querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'threadId,cacheKey' &&
      zedAddIn().value === '' && w.settingsIsDirty() &&
      d.getElementById('settings-count').textContent === '1 unsaved');
    zedAddIn().value = 'fallbackKey';
    zedAddIn().dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    check('Enter in the param add input appends through the same wiring',
      [...zed().querySelectorAll('.sc-param')].length === 3);
    zedAddIn().value = 'threadId';
    zed().querySelector('[data-sc-param-add]').click();
    check('a duplicate param name flashes the add input instead of adding a row',
      [...zed().querySelectorAll('.sc-param')].length === 3 &&
      zedAddIn().classList.contains('prov-bad'));
    zedAddIn().value = 'fourthKey';
    zedAddIn().dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    check('the fourth param row still fits the per-entry cap',
      [...zed().querySelectorAll('.sc-param')].length === 4);
    zedAddIn().value = 'fifthKey';
    zed().querySelector('[data-sc-param-add]').click();
    check('the param add gate refuses beyond the server cap of 4',
      [...zed().querySelectorAll('.sc-param')].length === 4 &&
      [...zed().querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'threadId,cacheKey,fallbackKey,fourthKey' &&
      zedAddIn().classList.contains('prov-bad'));
    // the live grammar mirror of config.checkSubConversationParam,
    // judged per keystroke on the row itself with the server's message
    // vocabulary. The pinned control byte is \x01: a single-line input's
    // spec value-sanitization strips \r\n at the DOM value layer before
    // validation could see it, but other control bytes survive a paste.
    const gin = () => [...zed().querySelectorAll('.sc-param')].pop();
    const type = v => { gin().value = v; gin().dispatchEvent(new w.Event('input', { bubbles: true })); };
    type('bad\x01byte');
    check('a control byte in a param name reddens the row with the JSON key segment message',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === `params: 'bad\x01byte' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)` &&
      gin().classList.contains('prov-bad'));
    type('ba"d');
    check('a quote in a param name reddens with the same server vocabulary',
      zed().querySelector('.sc-err').textContent === `params: 'ba"d' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`);
    type('ba\\ck');
    check('a backslash in a param name reddens with the same server vocabulary',
      zed().querySelector('.sc-err').textContent === `params: 'ba\\ck' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`);
    type('a'.repeat(65));
    check('a 65-byte param name reddens with the byte-bound message',
      zed().querySelector('.sc-err').textContent === 'params: must be 1..64 bytes, got 65');
    type('');
    check('an emptied param row names the empty rule with the server wording',
      zed().querySelector('.sc-err').textContent === 'params: must not be empty or only whitespace');
    type('fourthKey');
    check('repairing the row clears the entry error',
      zed().dataset.scState !== 'error' && zed().querySelector('.sc-err').textContent === '');
    // the duplicate-param live check: the same name twice in one entry.
    const dupIn = [...zed().querySelectorAll('.sc-param')][1];
    dupIn.value = 'threadId';
    dupIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a duplicate param within the entry names the duplicate with the server wording',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === "params: duplicate 'threadId'" &&
      dupIn.classList.contains('prov-bad'));
    dupIn.value = 'cacheKey';
    dupIn.dispatchEvent(new w.Event('input', { bubbles: true }));
    // removing every param row surfaces the no-param rule.
    [...zed().querySelectorAll('[data-sc-p-rm]')].forEach(btn => btn.click());
    check('removing every param row names the no-param rule and reddens the add row',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === 'no param set - give the entry a request body field to track, or remove the entry' &&
      zedAddIn().classList.contains('prov-bad'));
    zedAddIn().value = 'threadId';
    zedAddIn().dispatchEvent(new w.KeyboardEvent('keydown', { key: 'Enter', bubbles: true }));
    check('re-adding the param clears the no-param error',
      zed().dataset.scState !== 'error' &&
      [...zed().querySelectorAll('.sc-param')].map(p => p.value).join(',') === 'threadId');
    // the client rules: required, exact, one entry per client.
    const zedClient = () => zed().querySelector('.sc-client');
    zedClient().value = 'opencode';
    zedClient().dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a duplicate client across cards names the earlier entry with the server wording',
      zed().dataset.scState === 'error' &&
      zed().querySelector('.sc-err').textContent === "duplicate client 'opencode' with entry 1; merge the entries or change one client");
    // The middle-duplicate ordinal (the first-occurrence registration): a
    // duplicate client whose card ALSO carries another error must not
    // re-register the client, or a later duplicate cites the wrong entry.
    gin().value = 'ba"d';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    scWrap.querySelector('[data-sc-add]').click();
    const scDupMid = cards()[2];
    scDupMid.querySelector('.sc-client').value = 'opencode';
    scDupMid.querySelector('.sc-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    scDupMid.querySelector('.prov-add .sc-param-in').value = 'k';
    scDupMid.querySelector('[data-sc-param-add]').click();
    check('a duplicate client carrying another error does not shift the cited ordinal',
      cards().length === 3 &&
      scDupMid.querySelector('.sc-err').textContent === "duplicate client 'opencode' with entry 1; merge the entries or change one client");
    scDupMid.querySelector('[data-sc-rm]').click();
    gin().value = 'threadId';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    zedClient().value = '';
    zedClient().dispatchEvent(new w.Event('input', { bubbles: true }));
    check('an emptied client names the required-client rule with the server wording',
      zed().querySelector('.sc-err').textContent === 'client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry');
    zedClient().value = 'zed-cli';
    zedClient().dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the fully repaired draft is clean again',
      scWrap.querySelectorAll('.sc-card[data-sc-state="error"]').length === 0 && !w.settingsIsDirty());
    // the blocked-Apply drive: an invalid draft must stop the POST, name
    // the fix on the status line and focus the first offending row (the
    // ro save-gate precedent).
    gin().value = 'ba"d';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    const failuresBefore = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid sub-conversations draft blocks the save and focuses the offending row',
      failures.length === failuresBefore &&
      d.getElementById('settings-count').textContent === 'sub-conversations - fix the highlighted entry first' &&
      d.activeElement === gin() && gin().classList.contains('prov-bad'));
    gin().value = 'threadId';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    // The hidden-offender drive (the W53 bug, the live-verified sc case):
    // with the sheet showing the providers category the sub-conversations
    // block is hidden, so the gate's focus() and scrollIntoView() were
    // silent no-ops on it - only the status line moved. The shared reveal
    // must select the offender's category through the rail machinery before
    // focusing the offending param.
    gin().value = 'ba"d';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    w.eval("settingsCat = 'providers'; showSettingsCat()");
    const scBlockRow = scWrap.closest('.st-row');
    check('switching the sheet to the providers category hides the sub-conversations block',
      scBlockRow.hidden && !d.querySelector('[data-st-cat="conversation"]').classList.contains('active'));
    const scHiddenFailures = failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden sub-conversations offender selects its category and focuses it',
      failures.length === scHiddenFailures &&
        d.getElementById('settings-count').textContent === 'sub-conversations - fix the highlighted entry first' &&
        w.eval('settingsCat') === 'conversation' &&
        d.querySelector('[data-st-cat="conversation"]').classList.contains('active') &&
        d.querySelector('[data-st-cat="conversation"]').getAttribute('aria-current') === 'page' &&
        !scBlockRow.hidden &&
        d.activeElement === gin() && gin().classList.contains('prov-bad'));
    gin().value = 'threadId';
    gin().dispatchEvent(new w.Event('input', { bubbles: true }));
    // the entry add/remove lifecycle: a fresh card asks for its client,
    // then its param; a middle removal renumbers the survivors' visible
    // ordinals and their aria-labels (the one ordinal owner's restamp).
    scWrap.querySelector('[data-sc-add]').click();
    const fresh = cards()[2];
    check('a fresh entry card asks for its client and counts against the cap line',
      cards().length === 3 && fresh.dataset.scState === 'error' &&
      fresh.querySelector('.sc-err').textContent === 'client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry' &&
      scWrap.querySelector('.mr-count').textContent === '3 / 16 entries' &&
      d.activeElement === fresh.querySelector('.sc-client') &&
      fresh.querySelector('.sc-client').getAttribute('aria-label') === 'entry 3 client');
    fresh.querySelector('.sc-client').value = 'new-cli';
    fresh.querySelector('.sc-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    check('a named client with no param names the missing param',
      fresh.dataset.scState === 'error' &&
      fresh.querySelector('.sc-err').textContent === 'no param set - give the entry a request body field to track, or remove the entry');
    fresh.querySelector('.prov-add .sc-param-in').value = 'convKey';
    fresh.querySelector('[data-sc-param-add]').click();
    check('the completed entry joins the draft valid',
      fresh.dataset.scState !== 'error' && cards().length === 3);
    zed().querySelector('[data-sc-rm]').click();
    const midSurvivors = cards();
    check('a middle-card removal renumbers the survivors\' visible ordinals',
      midSurvivors.length === 2 &&
      [...scWrap.querySelectorAll('.sc-num')].map(n => n.textContent).join(',') === 'entry 1,entry 2');
    check('the renumbered survivors\' client labels carry the same ordinals',
      midSurvivors[0].querySelector('.sc-client').getAttribute('aria-label') === 'entry 1 client' &&
      midSurvivors[1].querySelector('.sc-client').getAttribute('aria-label') === 'entry 2 client');
    midSurvivors[1].querySelector('[data-sc-rm]').click();
    check('the tail removal leaves the seeded first entry alone',
      cards().length === 1 && cards()[0].querySelector('.sc-client').value === 'opencode');
    // rebuild the removed entry behind the survivor through the real
    // controls; the strip toggle rides the rebuild and must collect.
    scWrap.querySelector('[data-sc-add]').click();
    const rebuilt = cards()[1];
    rebuilt.querySelector('.sc-client').value = 'zed-cli';
    rebuilt.querySelector('.sc-client').dispatchEvent(new w.Event('input', { bubbles: true }));
    rebuilt.querySelector('.prov-add .sc-param-in').value = 'threadId';
    rebuilt.querySelector('[data-sc-param-add]').click();
    rebuilt.querySelector('.sc-strip').click();
    check('the strip toggle collects into the entry and the rebuilt draft is the real edit',
      JSON.stringify(w.collectSettingsValues().sub_conversations) === JSON.stringify([
        { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
        { client: 'zed-cli', params: ['threadId'], strip: true },
      ]) && w.settingsIsDirty());
    // the save path: the POST carries the strict shape with the loaded
    // revision (the settings save-flow stub discipline).
    const originalScFetch = w.fetch;
    const scPosts = [];
    w.fetch = (url, init) => {
      if (!String(url).includes('/admin/config')) return originalScFetch(url, init);
      return new Promise(resolve => {
        if (init?.method === 'POST') scPosts.push({ resolve, body: JSON.parse(init.body) });
        else resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
      });
    };
    const scSave = w.applySettings();
    check('the sub-conversations save posts the strict lossless shape with the loaded revision',
      scPosts.length === 1 && scPosts[0].body.revision === 'sc-r1' &&
      JSON.stringify(scPosts[0].body.values.sub_conversations) === JSON.stringify([
        { client: 'opencode', params: ['promptCacheKey', 'sessionKey'], strip: true },
        { client: 'zed-cli', params: ['threadId'], strip: true },
      ]));
    const savedSc = JSON.parse(JSON.stringify(scPosts[0].body.values.sub_conversations));
    scPosts.shift().resolve({ ok: true, statusText: 'OK', json: async () => ({ saved: true, revision: 'sc-r2', values: { ...doc.values, sub_conversations: savedSc }, effective: {}, restart_required: [] }) });
    await scSave;
    check('the accepted save adopts the new revision and a clean draft',
      w.eval('settingsDoc.revision') === 'sc-r2' && !w.settingsIsDirty() &&
      /saved/.test(d.getElementById('settings-count').textContent));
    w.fetch = originalScFetch;
    // the entry cap: 16 seeded entries saturate the count line and the
    // add gate refuses a 17th (mirrors config.SubConversationsMax).
    {
      const capDoc = JSON.parse(JSON.stringify(cfgDoc));
      capDoc.fields.push({ key: 'sub_conversations', category: 'conversation', label: 'Sub-conversation tracking', help: 'Per-client tracking of a request body field that carries a sub-conversation identity.', kind: 'sub_conversations', hot_reload: true });
      capDoc.categories.push({ id: 'conversation', label: 'Conversations', help: 'How the request log groups turns into conversations.' });
      capDoc.values.sub_conversations = Array.from({ length: 16 }, (_, i) => ({ client: 'cap-' + i, params: ['k'], strip: false }));
      w.__scCapDoc = capDoc;
      w.eval("settingsCat = 'conversation'; settingsDoc = window.__scCapDoc; fillSettingsForm(settingsDoc)");
      const capWrap = d.querySelector('#settings-fields [data-kind="sub_conversations"]');
      const capAdd = capWrap.querySelector('[data-sc-add]');
      check('sixteen seeded entries saturate the count line and disable the add button',
        capWrap.querySelectorAll('.sc-card').length === 16 &&
        capWrap.querySelector('.mr-count').textContent === '16 / 16 entries' && capAdd.disabled);
      capAdd.click();
      w.addScCard(capWrap);
      check('the entry add gate refuses beyond the server cap of 16',
        capWrap.querySelectorAll('.sc-card').length === 16);
      delete w.__scCapDoc;
    }
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__scDoc;
  }

  // ---- test 12: chart preset registry - ids unique, dropdown follows ----
  const presetIds = w.eval('CHART_PRESETS.map(p => p.id)');
  check('CHART_PRESETS ids are unique', new Set(presetIds).size === presetIds.length);
  const presetOpts = [...d.querySelectorAll('#chart-preset option')].map(o => o.value);
  check('#chart-preset has no duplicate options', new Set(presetOpts).size === presetOpts.length && presetOpts.length === presetIds.length);

  // ---- test 12b: the derive cache invalidates on a filter change - the
  // filter-set identity rides the memo key, so narrowing the status dropdown
  // must re-derive the scoped log even with zero record mutations.
  {
    const origF = w.fetch;
    fullPayload = { feed_id: 'feedB', seq: 3,
      records: [{ ...mkRec('mix1', 200, 1700000200000), __seq: 1 },
                { ...mkRec('mix2', 500, 1700000201000), __seq: 2 },
                { ...mkRec('mix3', 200, 1700000202000), __seq: 3 }],
      counters: { in_flight: 0, total_requests: 3, total_errors: 1 } };
    w.eval("fetchBootstrap('full')");
    await sleep(20);
    w.eval("filters.status = ''");
    const allN = w.eval('derive(lastData).scopedRecs.length');
    const fstatus = d.getElementById('f-status');
    fstatus.value = '500';
    fstatus.dispatchEvent(new w.Event('change'));
    await sleep(20);
    const errN = w.eval('derive(lastData).scopedRecs.length');
    check('derive cache invalidates on a filter change', allN === 3 && errN === 1 &&
      rows().every(tr => tr.dataset.id === 'mix2'));
    fstatus.value = '';
    fstatus.dispatchEvent(new w.Event('change'));
    await sleep(20);
    w.fetch = origF;
  }

  // An idle SSE connection sends cursor/counter metadata after the HTML seed.
  // It must not invalidate the shared derivation or schedule another render.
  {
    await sleep(30);
    const originalSchedule = w.scheduleRenderLive;
    let scheduled = 0;
    w.scheduleRenderLive = () => { scheduled++; };
    const rev = w.eval('lastData._rev || 0');
    const counters = w.eval('lastData.counters');
    const derived = w.eval('derive(lastData)');
    const cursor = w.eval('lastSeq') + 1;
    const payload = { feed_id: w.eval('feedId'), seq: cursor,
      pending_revision: w.eval('_pendingRevision'), incremental: true, records: [], in_flight_records: [], counters: { ...counters } };
    w.applySnapshotPayload(observerFixture(payload));
    check('empty SSE replay advances metadata without deriving or rendering',
      scheduled === 0 && w.eval('lastData._rev || 0') === rev &&
      w.eval('lastSeq') === cursor && w.eval('lastData.seq') === cursor &&
      w.eval('derive(lastData)') === derived);
    w.applySnapshotPayload(observerFixture({ ...payload, pending_revision: w.eval('_pendingRevision') + 1,
      counters: { ...counters, in_flight: (counters.in_flight || 0) + 1 } }));
    check('a counter-only snapshot still invalidates and schedules the live band',
      scheduled === 1 && w.eval('lastData._rev || 0') === rev + 1 &&
      counters.in_flight !== w.eval('lastData.counters.in_flight'));
    w.applySnapshotPayload(observerFixture(payload));
    scheduled = 0;
    w.applySnapshotPayload(observerFixture({ ...payload, in_flight_records: [{ ...mkRec('seed-pending', 0), live: true }] }));
    check('a pending-only snapshot still paints the in-flight request', scheduled === 1 &&
      w.eval('lastData.records.some(r => r.id === "seed-pending" && r.live)'));
    w.scheduleRenderLive = originalSchedule;
    fire('end', { record: mkRec('seed-pending', 200), in_flight: counters.in_flight || 0 });
    await sleep(20);
  }

  // ---- test 12c: overlapping bootstraps resolve by ISSUE order - a stale
  // in-flight delta that lands after a newer full resync must never re-apply
  // its (pre-resync) state over the fresh one.
  {
    const origSup = w.fetch;
    const bootResp = reqs => ({ ok: true, json: async () => ({
      records: [], counters: { in_flight: 0, total_requests: reqs, total_errors: 0 },
      seq: 5, feed_id: 'feedB', incremental: false,
      kpi: { requests: reqs, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0 },
      dash: {}, pause: {}, throttle: {},
    }) });
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;'); // no tick may race the two calls
    let resA = null, resB = null;
    w.fetch = () => {
      if (!resA) return new Promise(res => { resA = () => res(bootResp(111)); });
      if (!resB) return new Promise(res => { resB = () => res(bootResp(222)); });
      return Promise.resolve(bootResp(222)); // any other caller gets fresh state
    };
    w.eval("fetchBootstrap('resume')"); // issued first, resolves LAST
    w.eval("fetchBootstrap('resume')"); // issued second, resolves first
    resB();
    await sleep(10);
    check('fresh bootstrap applies', w.eval('kpiAgg && kpiAgg.requests') === 222);
    resA();
    await sleep(10);
    check('stale bootstrap response is superseded', w.eval('kpiAgg && kpiAgg.requests') === 222);
    w.fetch = origSup;
    w.eval('armDashboardTicks()');
  }

  // Pause mutations must not apply error documents, overlap each other, or
  // lose to an older bootstrap response. These drive the actual menu handlers.
  {
    const originalFetch = w.fetch;
    const hold = {id: 'pause-fixture', all: true, max_queued: 0};
    const paused = {ok: true, paused: true, holds: [hold], queued: 0};
    const resumed = {ok: true, paused: false, holds: [], queued: 0};
    const settle = () => new Promise(resolve => setTimeout(resolve, 20));
    d.getElementById('pause-menu').hidden = false;
    w.applyPauseState(paused);
    w.fetch = async () => ({ok: false, json: async () => ({error: 'resume rejected'})});
    w.resumePauseHold(hold.id);
    await settle();
    check('failed Resume keeps the confirmed hold and displays its error',
      w.pauseActive() && d.getElementById('pause-count').textContent.includes('resume rejected'));

    let replyMutation, posts = 0;
    w.fetch = () => { posts++; return new Promise(resolve => { replyMutation = resolve; }); };
    w.resumePauseMenu();
    w.resumePauseMenu();
    w.resumePauseHold(hold.id);
    check('pause mutations share one busy gate and disable every resume action', posts === 1 &&
      d.getElementById('btn-pause-resume').disabled &&
      [...d.querySelectorAll('#pf-holds button')].every(b => b.disabled));
    // State refreshes cannot enable controls while a mutation is outstanding.
    w.refreshFooterState();
    w.syncPauseMenuState();
    check('pause polling and footer refresh preserve the busy state', d.getElementById('btn-pause-resume').disabled);
    replyMutation({ok: true, json: async () => resumed});
    await settle();
    check('successful Resume uses the confirmed server state', !w.pauseActive());

    let replyBootstrap;
    w.applyPauseState(paused);
    w.fetch = url => String(url).includes('/metrics/bootstrap')
      ? new Promise(resolve => { replyBootstrap = resolve; })
      : Promise.resolve({ok: true, json: async () => resumed});
    w.fetchBootstrap('none');
    w.resumePauseMenu();
    await settle();
    replyBootstrap({ok: true, json: async () => ({pause: paused, dash: {}})});
    await settle();
    check('a pre-Resume bootstrap cannot resurrect the resumed hold', !w.pauseActive());

    w.applyPauseState(resumed);
    w.resetPauseMenuForm();
    d.getElementById('pf-scope').value = 'all';
    w.onPauseScopeChange();
    d.getElementById('pf-cap').value = '-1';
    posts = 0;
    w.fetch = async () => { posts++; return {ok: true, json: async () => paused}; };
    w.applyPauseMenu();
    await settle();
    check('an invalid queue cap is rejected instead of silently becoming unlimited', posts === 0 &&
      d.getElementById('pause-count').textContent.includes('queue cap'));
    w.applyPauseState({...paused, holds: [{...hold, until: new Date(Date.now() - 1000).toISOString()}]});
    check('expired holds stop showing paused even before the next server poll', !w.pauseActive());
    const named = {id: 'named-hold', clients: ['client-a'], providers: ['old.example']};
    w.applyPauseState({...paused, holds: [named], known_clients: ['client-a'], known_providers: ['old.example', 'new.example']});
    w.resetPauseMenuForm();
    const clientChoice = d.querySelector('#pf-clients input[value="client-a"]');
    check('pause permits selecting a reused client for a disjoint provider scope', clientChoice && !clientChoice.disabled);
    clientChoice.checked = true;
    d.querySelector('#pf-providers input[value="new.example"]').checked = true;
    w.syncPauseMenuState();
    check('pause state refresh preserves the selected cross-scope draft',
      w.selectedCheckboxValues('pf-clients').includes('client-a') && w.selectedCheckboxValues('pf-providers').includes('new.example'));
    w.applyPauseState({...paused, holds: [{...named, new: true, known_at_new: ['client-a']}]});
    w.editPauseHold(named.id);
    let scopedPauseBody;
    w.fetch = async (url, options) => {
      scopedPauseBody = JSON.parse(options.body);
      return {ok: true, json: async () => resumed};
    };
    await w.applyPauseMenu();
    check('editing a scoped new-client hold preserves both explicit dimensions', scopedPauseBody.new &&
      scopedPauseBody.clients.includes('client-a') && scopedPauseBody.providers.includes('old.example'));
    w.applyPauseState(paused);
    w.fetch = async () => ({ok: true, json: async () => ({...resumed, warning: 'applied in memory but not saved'})});
    await w.resumePauseMenu();
    check('pause persistence warnings retain confirmed runtime state and remain visible', !w.pauseActive() &&
      !d.getElementById('pause-menu').hidden && d.getElementById('pause-count').textContent.includes('not saved'));
    w.applyPauseState(resumed);
    w.resetPauseMenuForm();
    d.getElementById('pause-menu').hidden = true;
    w.fetch = originalFetch;
  }

  // ---- NONE_YET_NOTE: the operator menus' one empty-state sentence ----
  // Both directions are pinned: empty known lists render the note (the
  // pause/debug checklists paint it as .pause-none; the limits provider
  // select carries it as its empty option), and populated lists render
  // their rows without it anywhere - an empty surface silently painting
  // nothing, or a populated one leaking the note, are both drift. This
  // also repairs the W14 record: that commit claimed this pin existed.
  {
    const noteText = 'none yet - they appear as requests arrive';
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: [], known_providers: []});
    w.resetPauseMenuForm();
    w.eval("debugState = {...debugState, sessions: [], known_clients: [], known_providers: [], known_models: []}");
    w.resetDebugMenuForm();
    w.applyThrottleState({ok: true, active: false, throttles: [], known_providers: []});
    w.syncLimitsMenuState();
    check('empty known lists render the none-yet note in the pause and debug checklists',
      ['#pf-clients', '#pf-providers', '#df-clients', '#df-providers', '#df-models']
        .every(id => d.querySelector(id + ' .pause-none')?.textContent === noteText));
    check('the empty limits provider select carries the none-yet note as its only option',
      d.getElementById('lim-provider').options.length === 1 &&
      d.getElementById('lim-provider').options[0].value === '' &&
      d.getElementById('lim-provider').options[0].textContent === noteText);
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: ['client-a'], known_providers: ['prov-a']});
    w.resetPauseMenuForm();
    w.eval("debugState = {...debugState, known_clients: ['client-a'], known_providers: ['prov-a'], known_models: ['glm-5.3']}");
    w.resetDebugMenuForm();
    w.applyThrottleState({ok: true, active: false, throttles: [], known_providers: ['prov-a']});
    w.syncLimitsMenuState();
    check('populated known lists render their rows, never the none-yet note',
      !d.querySelector('#pause-menu .pause-none') && !d.querySelector('#debug-menu .pause-none') &&
      !!d.querySelector('#pf-clients input[value="client-a"]') &&
      !!d.querySelector('#pf-providers input[value="prov-a"]') &&
      !!d.querySelector('#df-models input[value="glm-5.3"]') &&
      [...d.getElementById('lim-provider').options].every(o => o.value === 'prov-a'));
    // Leave the surfaces on the empty baseline the rest of the suite expects.
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: [], known_providers: []});
    w.resetPauseMenuForm();
    w.eval("debugState = {...debugState, known_clients: [], known_providers: [], known_models: []}");
    w.resetDebugMenuForm();
    w.applyThrottleState({ok: true, active: false, throttles: [], known_providers: []});
    w.syncLimitsMenuState();
  }

  // ---- GAP-6 pin: the pause/debug status-bits wording on menu AND footer.
  // jsdom only ever asserted menu counts for error/busy strings; the
  // composition itself ('queued N', the until countdown) was unpinned on
  // both surfaces. One fixture drives each builder through the real apply
  // path: the menu count line and the footer state line must compose the
  // identical bits string, with the footer adding only its documented
  // scope insertion (holdScopeLabel after 'paused · ').
  {
    const until = new Date(Date.now() + 90 * 60000).toISOString();
    d.getElementById('pause-menu').hidden = false;
    w.applyPauseState({ok: true, paused: true, queued: 3, until,
      holds: [{id: 'bits-hold', all: true, until}],
      known_clients: [], known_providers: []});
    const pauseMenuBits = d.getElementById('pause-count').textContent;
    const pauseFooterBits = d.getElementById('f-state').textContent;
    check('the pause menu count composes the paused/queued/until wording',
      pauseMenuBits === 'paused · queued 3 · 1h 30m left');
    check('the pause footer carries the identical bits around its scope insertion',
      pauseFooterBits === 'paused · all requests · queued 3 · 1h 30m left' &&
      pauseFooterBits === pauseMenuBits.replace('paused · ', 'paused · all requests · '));
    d.getElementById('pause-menu').hidden = true;
    w.applyPauseState({ok: true, paused: false, holds: [], known_clients: [], known_providers: []});
    w.resetPauseMenuForm();

    d.getElementById('debug-menu').hidden = false;
    w.applyDebugState(observerFixture({ok: true, enabled: true, until,
      sessions: [{id: 'bits-session', clients: ['client-a'], duration: '15m'}],
      known_clients: [], known_providers: [], known_models: [],
      ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture()}));
    const debugBits = 'debug · client-a · 1h 30m left';
    check('menu and footer compose the identical debug bits string',
      d.getElementById('debug-count').textContent === debugBits &&
      d.getElementById('f-state').textContent === debugBits);
    w.applyDebugState(observerFixture({ok: true, enabled: true, until,
      sessions: [{id: 'bits-session', clients: ['client-a'], duration: '15m'},
        {id: 'bits-session-2', providers: ['prov-a'], duration: '1h'}],
      known_clients: [], known_providers: [], known_models: [],
      ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture()}));
    const debugMultiBits = 'debug · 2 sessions · 1h 30m left';
    check('the multi-session debug count stays one wording on both surfaces',
      d.getElementById('debug-count').textContent === debugMultiBits &&
      d.getElementById('f-state').textContent === debugMultiBits);
    d.getElementById('debug-menu').hidden = true;
    w.applyDebugState(observerFixture({ok: true, enabled: false, sessions: [],
      known_clients: [], known_providers: [], known_models: [],
      ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture()}));
    w.resetDebugMenuForm();
  }

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

  // ---- test 12d: a stale tick's pending snapshot must never regress an
  // in-flight row's retry progress (attempts are append-only - an older
  // snapshot carries fewer absorbed attempts than a just-arrived SSE update).
  {
    fire('begin', { record: { ...mkRec('pendX', 0, 1700000300000), attempts: [] }, in_flight: 1 });
    fire('update', { record: { ...mkRec('pendX', 0, 1700000300000), attempts: [{ status: 503 }, { status: 502 }] }, in_flight: 1 });
    await sleep(20);
    const attN = () => w.eval("(lastData.records.find(r => r.id === 'pendX')?.attempts || []).length");
    w.eval("upsertPendingRec({ id: 'pendX', status_code: 0, live: true, attempts: [{ status: 503 }] }, _pendingRevision - 1)"); // stale tick snapshot
    check('stale pending snapshot never regresses retry progress', attN() === 2);
    w.eval("upsertPendingRec({ id: 'pendX', status_code: 0, live: true, attempts: [{ status: 503 }, { status: 502 }, { status: 500 }] }, _pendingRevision + 1)"); // newer
    check('newer pending snapshot refreshes retry progress', attN() === 3);
    fire('end', { record: { ...mkRec('pendX', 200, 1700000300000), attempts: [{ status: 503 }, { status: 502 }, { status: 500 }] }, in_flight: 0 });
    await sleep(20);
    check('pending row finalizes after the badge checks',
      !!rows().find(tr => tr.dataset.id === 'pendX') && w.eval("lastData.records.find(r => r.id === 'pendX').live") === false);
  }

  // ---- test 12e: O(1) upsert - _recIdx map is maintained on insert/update ----
  {
    const origU = w.fetch;
    fullPayload = { feed_id: 'feedB', seq: 5,
      records: [{ ...mkRec('idx1', 200, 1700000400000), __seq: 1 },
                { ...mkRec('idx2', 200, 1700000401000), __seq: 2 }],
      counters: { in_flight: 0, total_requests: 2, total_errors: 0 } };
    w.eval("fetchBootstrap('full')");
    await sleep(20);
    const has = id => w.eval("_recIdx.has('" + id + "')");
    const get = id => w.eval("_recIdx.get('" + id + "')");
    check('_recIdx is populated after full snapshot', has('idx1') && has('idx2'));
    check('_recIdx indexes are correct', get('idx1') === 0 && get('idx2') === 1);
    // Insert a new record via SSE
    fire('record', { ...mkRec('idx3', 200, 1700000402000) });
    await sleep(20);
    check('_recIdx grows on new record insert', has('idx3'));
    // Replay an existing record - should NOT bump _rev (skip-bump-on-replay)
    const revBefore = w.eval('lastData._rev');
    fire('record', { ...mkRec('idx1', 200, 1700000400000) }); // replay
    await sleep(20);
    check('pure replay does not bump _rev', w.eval('lastData._rev') === revBefore);
    w.fetch = origU;
  }

  // ---- test 12f: row-identity cache - reqRow is skipped for unchanged records ----
  {
    // Insert two records, then replay one - the replayed row should NOT rebuild HTML
    const origHTML = w.eval("logHTML.get('idx1')?.html?.length || 0");
    fire('record', { ...mkRec('idx1', 200, 1700000400000) }); // replay
    await sleep(20);
    const newHTML = w.eval("logHTML.get('idx1')?.html?.length || 0");
    check('row-identity cache preserves HTML for unchanged record', origHTML === newHTML && origHTML > 0);
    // A real update (pending → finalized) MUST rebuild
    fire('begin', { record: { ...mkRec('idx4', 0, 1700000403000), attempts: [] }, in_flight: 1 });
    await sleep(20);
    const liveBefore = w.eval("logHTML.get('idx4')?.rec?.live");
    fire('end', { record: { ...mkRec('idx4', 200, 1700000403000), attempts: [] }, in_flight: 0 });
    await sleep(20);
    const liveAfter = w.eval("logHTML.get('idx4')?.rec?.live");
    check('row-identity cache rebuilds on pending→finalized transition', liveBefore === true && liveAfter === false);
  }

  // ---- test 12g: requestGate helper - issue-order supersede + dedupe ----
  {
    // Direct test of the requestGate logic
    w.eval(`(() => {
      const gate = requestGate(true);
      let applied = [];
      gate('key1', (accept) => { applied.push('a1'); accept('p1', true); }, (p) => { applied.push('apply1'); });
      gate('key1', (accept) => { applied.push('a2'); accept('p2', true); }, (p) => { applied.push('apply2'); });
      window.__gateTest = applied;
    })()`);
    check('requestGate dedupes same-key calls', w.eval('window.__gateTest')?.join(',') === 'a1,apply1');
    // Supersede: different key wins
    w.eval(`(() => {
      const gate = requestGate(false);
      let result = [];
      // gate IS the run function directly
      let accept1;
      gate('k1', (accept) => { result.push('f1'); accept1 = accept; }, (p) => { result.push('a1'); });
      gate('k2', (accept) => { result.push('f2'); accept('p2', true); }, (p) => { result.push('a2'); });
      accept1('stale', true); // try to apply after superseded
      window.__gateSuper = result;
    })()`);
    check('requestGate supersedes older response', w.eval('window.__gateSuper')?.join(',') === 'f1,f2,a2');
    w.eval(`(() => {
      const gate = requestGate(true);
      let firstSignal, stale, result = [];
      gate('same', (accept, signal) => { stale = accept; firstSignal = signal; }, p => result.push(p));
      gate('same', () => result.push('duplicate'), p => result.push(p), 'refresh');
      result.push(firstSignal.aborted ? 'starved' : 'not-starved');
      gate('same', accept => accept('fresh', true), p => result.push(p), 'invalidate');
      stale('stale', true);
      gate('failure', accept => accept(null, false), p => result.push(p));
      gate('failure', accept => accept('retried', true), p => result.push(p));
      window.__gateCancel = {aborted: firstSignal.aborted, result};
    })()`);
    check('requestGate cancels superseded work, forces same-key refresh and retries failure',
      w.eval('window.__gateCancel.aborted') && w.eval('window.__gateCancel.result.join(",")') === 'not-starved,fresh,retried');
  }

  // ---- test 12h: visibilitychange pauses timers ----
  {
    const hasClock = w.eval('typeof _clockTimer === "number" && _clockTimer !== null');
    check('clock timer is armed', hasClock);
    // Simulate hidden
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    // Override document.hidden for the handler
    Object.defineProperty(w.document, 'hidden', { value: true, configurable: true });
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    const clockStopped = w.eval('_clockTimer === null');
    check('clock timer stops when hidden', clockStopped);
    const tickStopped = w.eval('_dashTickTimer === null');
    check('tick timer stops when hidden', tickStopped);
    // Simulate visible
    Object.defineProperty(w.document, 'hidden', { value: false, configurable: true });
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    await sleep(20);
    const clockRestarted = w.eval('typeof _clockTimer === "number" && _clockTimer !== null');
    check('clock timer restarts on visible', clockRestarted);
    const tickRestarted = w.eval('typeof _dashTickTimer === "number" && _dashTickTimer !== null');
    check('tick timer restarts on visible', tickRestarted);
  }

  // ---- test 12i: record-then-end gauge correction - the server publishes
  // the ring "record" BEFORE the lifecycle "end", so the end event's record is
  // usually a replay that bumps nothing in upsertRec: the counters write
  // itself must bump the derivation or the In-flight tile keeps the pre-end
  // value until an unrelated bump. An unchanged gauge (an update re-carrying
  // its begin's value) must NOT re-derive.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;'); // no tick may repaint the tile for us
    const flightVal = () => {
      const el = [...d.querySelectorAll('#kpis .kpi')].find(k => (k.querySelector('h2') || {}).textContent === 'In-flight');
      return el ? el.querySelector('.val').textContent.trim() : null;
    };
    fire('begin', { record: { ...mkRec('gauge1', 0, 1700000500000), attempts: [] }, in_flight: 1 });
    await sleep(20);
    check('begin paints the In-flight tile live (new pending row)', flightVal() === '1');
    fire('record', { ...mkRec('gauge1', 200, 1700000500000) }, '9');
    await sleep(20);
    check('ring record finalizes the row; the tile keeps the pre-end gauge', flightVal() === '1');
    const revBeforeEnd = w.eval('lastData._rev');
    fire('end', { record: { ...mkRec('gauge1', 200, 1700000500000) }, in_flight: 0 });
    await sleep(20);
    check('end event corrects the In-flight tile with no further event', flightVal() === '0');
    check('the end-event counters write bumps the derivation', w.eval('lastData._rev') > revBeforeEnd);
    const revStable = w.eval('lastData._rev');
    fire('end', { record: { ...mkRec('gauge1', 200, 1700000500000) }, in_flight: 0 }); // replayed end, unchanged gauge
    await sleep(20);
    check('an unchanged gauge does not re-derive (no redundant pass)', w.eval('lastData._rev') === revStable);
    w.eval('armDashboardTicks()');
  }

  // Chart/explorer share the request gate; a new scope never waits behind
  // old work, and the footer consumes the explorer's FULL-scope totals.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const originalFetch = w.fetch;
    const pending = [];
    let scopeFetches = 0;
    w.fetch = (url, options) => {
      if (String(url).includes('/metrics/agg/scope')) scopeFetches++;
      if (!String(url).includes('/metrics/agg/explorer')) return originalFetch(url, options);
      return new Promise(resolve => pending.push({url, signal: options.signal, resolve}));
    };
    const reply = (request, matches) => request.resolve({ok: true, json: async () => ({
      dim: 'provider', total: 1000, error_total: 0, rail: {}, groups: [], scope: {matches, errors: 0},
    })});
    w.navigateTo([{dim: 'provider', id: 'scope-a.example'}]);
    await sleep(20);
    w.fetchExplorer();
    check('same-scope explorer requests dedupe while the first response is pending', pending.length === 1);
    check('unresolved filtered footer never substitutes ring counts or another scope', d.getElementById('f-scope').textContent.startsWith('… of '));
    w.navigateTo([{dim: 'provider', id: 'scope-b.example'}]);
    await sleep(20);
    check('new scope starts immediately and cancels the old explorer scan', pending.length === 2 && pending[0].signal.aborted);
    reply(pending[1], 7);
    await sleep(20);
    check('footer uses full-scope counts from explorer, not cross-filter gallery totals', d.getElementById('f-scope').textContent.startsWith('7 of '));
    reply(pending[0], 999);
    await sleep(10);
    check('a stale explorer response cannot overwrite current footer counts', d.getElementById('f-scope').textContent.startsWith('7 of '));
    w.fetchExplorer('invalidate');
    w.fetchExplorer('invalidate');
    check('forced same-scope refresh supersedes by issue order', pending.length === 4 && pending[2].signal.aborted);
    reply(pending[3], 8);
    reply(pending[2], 999);
    await sleep(20);
    check('latest forced refresh wins without a separate footer history scan', scopeFetches === 0 && d.getElementById('f-scope').textContent.startsWith('8 of '));
    w.fetchExplorer('invalidate');
    const previousExplorer = w.eval('explorerAgg');
    d.getElementById('f-status').value = 'streaming';
    w.doFilter();
    check('status selections immediately supersede old scans without a debounce delay', pending.length === 6 && pending[4].signal.aborted);
    check('live Requests status filters refresh explorer/footer regardless of breakdown dimension', w.explorerWantsLiveStatus());
    reply(pending[4], 999);
    await sleep(10);
    check('an old status response cannot paint under the new selection', w.eval('explorerAgg') === previousExplorer);
    reply(pending[5], 0);
    await sleep(10);
    check('zero live matches is a real full-scope count', d.getElementById('f-scope').textContent.startsWith('0 of '));
    w.fetch = originalFetch;
    d.getElementById('f-status').value = '';
    w.doFilter();
    w.navigateTo([]);
    await sleep(20);
    check('unfiltered footer reads global KPI immediately', !d.getElementById('f-scope').textContent.includes('…'));
  }

  // Frontend changes reload through the existing bootstrap boundary only.
  // jsdom reports navigation as not implemented; the harness-start
  // jsdomError listener counts that actual reload signal (jsdomReloads)
  // while every other error fails the run. Chromium verifies the
  // resulting new document separately.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const originalFetch = w.fetch;
    const newVersion = '0000000000000002';
    const payload = (version, requests = 37) => ({
      dashboard_version: version, feed_id: w.eval('feedId'), seq: w.eval('lastSeq'), incremental: true,
      records: [], counters: { in_flight: 0 }, kpi: { requests, errors: 0 }, dash: {},
    });
    let calls = 0;
    const setResponse = response => {
      w.fetch = (url, opts) => {
        if (!String(url).includes('/metrics/bootstrap')) return originalFetch(url, opts);
        calls++;
        return Promise.resolve({ ok: true, json: async () => response });
      };
    };
    setResponse(payload(TEST_DASHBOARD_VERSION));
    w.fetchBootstrap('resume');
    await sleep(20);
    check('unchanged frontend updates data without navigation or extra version requests',
      jsdomReloads === 0 && calls === 1 && w.eval('kpiAgg.requests') === 37);

    // An unexpected full response to a resume request is re-captured behind
    // the full/SSE barrier. The accepted full carries its own state/version.
    setResponse({ ...payload(TEST_DASHBOARD_VERSION), feed_id: 'same-assets-new-process', incremental: false });
    calls = 0;
    w.fetchBootstrap('resume');
    await sleep(20);
    check('same-assets restart re-captures an unexpected full response without a page reload', jsdomReloads === 0 && calls === 2);

    // Do not start expensive scans that a changed-frontend reload would
    // immediately throw away. An SSE restart first checks the bootstrap.
    const originalAggregates = w.refreshAggregates;
    const originalSettings = w.fetchSettings;
    let sweeps = 0;
    w.refreshAggregates = () => { sweeps++; };
    w.fetchSettings = () => { sweeps++; };
    let finishRestartBootstrap;
    w.fetch = () => new Promise(resolve => { finishRestartBootstrap = resolve; });
    w.refreshAfterRestart();
    check('SSE restart waits for the asset version before starting aggregate scans', sweeps === 0);
    finishRestartBootstrap({ok: true, json: async () => payload(TEST_DASHBOARD_VERSION)});
    await sleep(20);
    check('unchanged frontend runs the deferred restart sweep once', sweeps === 2);

    for (const version of [undefined, '', 'not-a-version']) {
      setResponse(payload(version, 38));
      w.fetchBootstrap('resume');
      await sleep(10);
    }
    check('missing or malformed asset versions never create a reload loop', jsdomReloads === 0);

    const pending = [];
    w.fetch = () => new Promise(resolve => pending.push(resolve));
    w.fetchBootstrap('resume');
    w.fetchBootstrap('resume');
    pending[1]({ok: true, json: async () => payload(TEST_DASHBOARD_VERSION, 39)});
    await sleep(10);
    pending[0]({ok: true, json: async () => payload(newVersion, 999)});
    await sleep(10);
    check('a superseded bootstrap cannot reload the page using an outdated version',
      jsdomReloads === 0 && w.eval('kpiAgg.requests') === 39);

    let afterRan = false;
    setResponse(payload(newVersion, 999));
    w.fetchBootstrap('resume', () => { afterRan = true; });
    await sleep(20);
    check('changed frontend requests one reload before applying state or boot callbacks',
      jsdomReloads === 1 && !afterRan && w.eval('kpiAgg.requests') === 39);
    sweeps = 0;
    const callsBeforeReload = calls;
    w.refreshAfterRestart();
    w.fetchBootstrap('resume');
    w.fetchBootstrap('full');
    await sleep(10);
    check('reload in progress suppresses duplicate navigation and bootstrap work',
      jsdomReloads === 1 && calls === callsBeforeReload && sweeps === 0);
    w.fetch = originalFetch;
    w.refreshAggregates = originalAggregates;
    w.fetchSettings = originalSettings;
  }

  // The HTML seed follows the exact same bootstrap application gate as HTTP.
  // Exercise full page boot with saved views, then replay a completion that
  // happened between HTML generation and opening the live feed.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'seed-feed', seq: 12, pending_revision: 1,
      incremental: false, records: [mkRec('seed-final'), {...mkRec('excluded'), provider: 'other.example'}],
      in_flight_records: [mkRec('seed-pending', 0)], counters: {in_flight: 1},
      kpi: {requests: 100, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    for (const [label, embedded, expectedFetches] of [
      ['valid', seed, 0], ['malformed JSON', '{broken', 1],
      ['invalid lifecycle revision', {...seed, pending_revision: -1}, 1],
      ['invalid shape', {...seed, records: {}}, 1], ['missing', undefined, 1],
    ]) {
      let bootstrapFetches = 0, streamURL = '';
      const handlers = {};
      const seeded = dashboardDOM(assembleHTML(embedded), {...pageOptions,
        url: 'http://127.0.0.1:8081/#/provider/p',
        beforeParse(win) {
          pageOptions.beforeParse(win);
          const fallback = win.fetch;
          win.fetch = (url, options) => {
            if (String(url).includes('/metrics/bootstrap')) bootstrapFetches++;
            return fallback(url, options);
          };
          win.EventSource = function(url) {
            streamURL = url;
            return {readyState: 1, addEventListener(type, callback) {handlers[type] = callback;}, close() {}};
          };
          win.localStorage.setItem('dash.chart', JSON.stringify({window: '10080', preset: 'cost', pct: 99}));
        },
      });
      await sleep(100);
      const sw = seeded.window;
      check(`${label} HTML seed uses ${expectedFetches} bootstrap fetches`, bootstrapFetches === expectedFetches);
      check(`${label} HTML seed is discarded after consumption`, !sw.document.getElementById('dashboard-bootstrap'));
      if (label === 'valid') {
        check('server-seeded boot restores saved chart and URL scope before first render',
          sw.eval('chartView.window') === '10080' && sw.eval('chartView.preset') === 'cost' &&
          !sw.document.querySelector('tr.exp-row[data-id="excluded"]') && sw.document.querySelector('tr.exp-row[data-id="seed-final"]'));
        check('server-seeded boot paints all-history KPI and pending rows with the right SSE cursor',
          sw.eval('kpiAgg.requests') === 100 && sw.eval('lastData.records.find(r => r.id === "seed-pending")?.live') &&
          streamURL.includes('since=12') && streamURL.includes('feed=seed-feed'));
        const finalized = mkRec('seed-pending');
        handlers.snapshot({data: JSON.stringify(observerFixture({feed_id:'seed-feed',seq:13,incremental:true,pending_revision:2,
          records:[finalized],in_flight_records:[],counters:{in_flight:0}}))});
        handlers.record({data:JSON.stringify(observerFixture(finalized)),lastEventId:'13'});
        await sleep(20);
        check('completion between HTML and SSE replay finalizes once without duplicating a row',
          sw.eval('lastData.records.filter(r => r.id === "seed-pending").length') === 1 &&
          !sw.eval('lastData.records.find(r => r.id === "seed-pending")?.live'));
        sw.fetchBootstrap('full');
        await sleep(20);
        check('later full resync fetches fresh state rather than reusing HTML seed', bootstrapFetches === 1);
      }
      sw.close();
    }
  }

  // State-only bootstrap changes must refresh the log even when the optional
  // operator snapshots are absent. Their render side effects cannot own dash
  // config or model-rule application, and an empty SSE replay remains a no-op.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'state-only-feed', seq: 17,
      incremental: false,
      records: Array.from({length: 17}, (_, i) => ({
        ...mkRec('state' + i, 200, 1700000000000 + i * 1000),
        model: i === 0 ? 'canonical-model' : 'raw-model',
      })),
      in_flight_records: [], counters: {in_flight: 0, total_requests: 17, total_errors: 0},
      kpi: {requests: 17, errors: 0}, dash: {dash_log_rows: 20}, model_canon: {rules: []},
    };
    let payload = {...seed, incremental: true, records: [], dash: {dash_log_rows: 10}};
    const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions,
      beforeParse(win) {
        pageOptions.beforeParse(win);
        const fallback = win.fetch;
        win.fetch = (url, options) => String(url).includes('/metrics/bootstrap')
          ? Promise.resolve({ok: true, json: async () => JSON.parse(JSON.stringify(String(url).includes('since=') ? payload : {...payload, incremental: false, records: seed.records}))})
          : fallback(url, options);
        win.EventSource = function() { return {readyState: 1, addEventListener() {}, close() {}}; };
      },
    });
    const sw = isolated.window;
    const stateRows = () => [...sw.document.querySelectorAll('#tbl-requests tr.exp-row[data-id]')];
    try {
      await sleep(100);
      check('operator-free HTML seed applies dashboard config on its first paint', stateRows().length === 17);
      sw.fetchBootstrap('resume');
      await sleep(30);
      check('dash-only incremental bootstrap refreshes the log without operator snapshots',
        sw.eval('dashCfg.log_rows') === 10 && stateRows().length === 10);
      sw.navigateTo([{dim: 'model', id: 'canonical-model'}]);
      await sleep(30);
      check('model-only bootstrap fixture starts with one in-scope row', stateRows().length === 1);
      payload = {...payload, model_canon: {rules: [{mode: 'exact', from: 'raw-model', to: 'canonical-model'}]}};
      sw.fetchBootstrap('resume');
      await sleep(30);
      check('model-only incremental bootstrap refreshes scoped rows without operator snapshots',
        stateRows().length === 10 && stateRows().some(row => row.dataset.id !== 'state0'));
      payload = {...payload, dash: {dash_log_rows: 20}};
      sw.fetchBootstrap('none');
      await sleep(30);
      check('state-only restart bootstrap also refreshes the log without replacing records', stateRows().length === 17);
      let liveRenders = 0;
      const originalLive = sw.renderLive;
      sw.renderLive = () => { liveRenders++; originalLive(); };
      payload = {...payload, incremental: false, records: seed.records};
      sw.fetchBootstrap('full');
      await sleep(30);
      check('a full bootstrap replacement does not queue a second live render', stateRows().length === 17 && liveRenders === 0);
    } finally {
      sw.close();
    }
  }

  // Purge is a feed-epoch transition, including in another tab. Each source
  // retains its own queued handlers so these tests can deliver stale callbacks
  // after close(), as well as an older bootstrap that completes after SSE.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'purge-epoch-1', seq: 2, pending_revision: 0,
      incremental: false, records: [mkRec('delete-me'), mkRec('survivor')],
      in_flight_records: [], counters: {in_flight: 0, total_requests: 2, total_errors: 0},
      kpi: {requests: 2, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    let payload = seed, delayNext = false, resolveOld;
    const streams = [], requests = [];
    const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions,
      beforeParse(win) {
        pageOptions.beforeParse(win);
        const fallback = win.fetch;
        win.fetch = (url, options) => {
          if (!String(url).includes('/metrics/bootstrap')) return fallback(url, options);
          requests.push(String(url));
          const captured = JSON.parse(JSON.stringify(payload));
          const reply = {ok: true, json: async () => captured};
          if (delayNext) {
            delayNext = false;
            return new Promise(resolve => { resolveOld = () => resolve(reply); });
          }
          return Promise.resolve(reply);
        };
        win.EventSource = function(url) {
          const stream = {url, readyState: 1, handlers: {},
            addEventListener(type, callback) {this.handlers[type] = callback;},
            close() {this.readyState = 2;}};
          streams.push(stream);
          return stream;
        };
      },
    });
    const sw = isolated.window;
    const emit = (stream, type, data, seq) => stream.handlers[type]({type, data: JSON.stringify(observerFixture(data)), lastEventId: seq});
    const IDs = () => sw.eval('lastData.records.map(r => r.id).join(",")');
    try {
      await sleep(50);
      const original = streams.at(-1);
      delayNext = true;
      sw.fetchBootstrap('resume');
      payload = {...seed, feed_id: 'purge-epoch-2', records: [mkRec('survivor')]};
      emit(original, 'snapshot', payload);
      await sleep(20);
      const replacement = streams.at(-1);
      check('a changed SSE epoch closes and repins the stream URL exactly once',
        streams.length === 2 && original.readyState === 2 && replacement.url.includes('feed=purge-epoch-2') && replacement.url.includes('since=2'));
      for (const type of ['begin', 'update', 'end']) {
        emit(original, type, {record: mkRec('deleted-' + type), in_flight: 99});
      }
      emit(original, 'record', mkRec('deleted-record'), '99');
      emit(original, 'snapshot', seed);
      original.onerror();
      resolveOld();
      await sleep(20);
      check('old stream lifecycle/record/snapshot and older bootstrap cannot resurrect purged rows',
        IDs() === 'survivor' && sw.eval('feedId') === 'purge-epoch-2' && sw.eval('lastSeq') === 2 && sw.eval('lastData.counters.in_flight') === 0);

      // A server reset starts one immediate full refresh; no native retry delay
      // or second polling timer is needed for another tab's clear operation.
      payload = {...payload, feed_id: 'purge-epoch-3', seq: 3, records: [mkRec('during-purge')]};
      const before = requests.length;
      emit(replacement, 'reset', {feed_id: payload.feed_id});
      emit(replacement, 'record', mkRec('deleted-during-reset'), '100');
      await sleep(30);
      const resetStream = streams.at(-1);
      check('cross-tab reset immediately fetches one full snapshot and resumes the new epoch',
        requests.length === before + 1 && !requests.at(-1).includes('since=') &&
        IDs() === 'during-purge' && resetStream !== replacement && resetStream.url.includes('feed=purge-epoch-3'));
      emit(resetStream, 'snapshot', {...payload, incremental: true, seq: 4, records: [mkRec('after-purge')]});
      check('new arrivals between purge snapshot and SSE subscribe survive delta replay', IDs() === 'during-purge,after-purge');

      // The existing bootstrap poll is also sufficient if the stream is down.
      payload = {...payload, feed_id: 'purge-epoch-4', records: [mkRec('poll-survivor')]};
      sw.fetchBootstrap('resume');
      await sleep(20);
      emit(resetStream, 'end', {record: mkRec('deleted-poll-end'), in_flight: 7});
      check('poll-detected purge replaces the view and rejects the previous stream',
        IDs() === 'poll-survivor' && streams.at(-1).url.includes('feed=purge-epoch-4'));
    } finally {
      sw.close();
    }
  }

  // Lifecycle and mandatory full refresh ordering share the real application
  // gates. Keep closed-source callbacks available to reproduce browser queues.
  {
    let state = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'ordered-feed', seq: 1,
      pending_revision: 0, incremental: false, records: [mkRec('ordered-base')], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 1, total_errors: 0},
      kpi: {requests: 1, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    const streams = [], pending = [];
    const isolated = dashboardDOM(assembleHTML(state), {...pageOptions,
      beforeParse(win) {
        pageOptions.beforeParse(win);
        const fallback = win.fetch;
        win.fetch = (url, options) => {
          if (!String(url).includes('/metrics/bootstrap')) return fallback(url, options);
          return new Promise((resolve, reject) => pending.push({url: String(url), resolve, reject}));
        };
        win.EventSource = function(url) {
          const stream = {url, readyState: 1, handlers: {},
            addEventListener(type, callback) {this.handlers[type] = callback;},
            close() {this.readyState = 2;}};
          streams.push(stream);
          return stream;
        };
      },
    });
    const sw = isolated.window;
    const emit = (stream, type, data, seq) => stream.handlers[type]({type, data: JSON.stringify(observerFixture(data)), lastEventId: seq});
    const ids = () => sw.eval('lastData.records.map(r => r.id).join(",")');
    const reply = (request, payload) => request.resolve({ok: true, json: async () => JSON.parse(JSON.stringify(payload))});
    try {
      await sleep(40);
      sw.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
      const chartBefore = sw.eval('JSON.stringify(chartAgg)');
      const first = streams.at(-1);
      const completed = mkRec('ordered-final');
      emit(first, 'record', completed, '2');
      emit(first, 'begin', {record: mkRec('ordered-final', 0), in_flight: 1, pending_revision: 1});
      emit(first, 'update', {record: {...mkRec('ordered-final', 0), attempts: [{}]}, in_flight: 1, pending_revision: 2});
      emit(first, 'end', {record: completed, in_flight: 0, pending_revision: 3});
      check('record before buffered begin/update/end never regresses final state or double-folds the chart',
        chartBefore === sw.eval('JSON.stringify(chartAgg)') && sw.eval('lastData.records.find(r => r.id === "ordered-final").live') === false);

      emit(first, 'update', {record: {...mkRec('ordered-pending', 0), attempts: [{}, {}]}, in_flight: 2, pending_revision: 6});
      emit(first, 'begin', {record: mkRec('ordered-other', 0), in_flight: 1, pending_revision: 5});
      emit(first, 'begin', {record: mkRec('ordered-pending', 0), in_flight: 1, pending_revision: 4});
      check('delayed lifecycle rows still merge while retry progress and the global gauge stay monotonic',
        sw.eval('lastData.records.find(r => r.id === "ordered-pending").attempts.length') === 2 &&
        ids().includes('ordered-other') && sw.eval('lastData.counters.in_flight') === 2);
      sw.applySnapshotPayload({...state, incremental: true, records: [], pending_revision: 3,
        counters: {in_flight: 0, total_requests: 99, total_errors: 4}}, true);
      check('an older incremental snapshot cannot regress the gauge but retains unrelated counter updates',
        sw.eval('lastData.counters.in_flight') === 2 && sw.eval('lastData.counters.total_requests') === 99 &&
        sw.eval('lastData.counters.total_errors') === 4);
      sw.applySnapshotPayload({...state, seq: 2, records: [completed], pending_revision: 6,
        counters: {in_flight: 2, total_requests: 101, total_errors: 5}}, true);
      check('full replacements normalize through the same revision-gated counter owner',
        sw.eval('lastData.counters.in_flight') === 2 && sw.eval('lastData.counters.total_requests') === 101);

      state = {...state, feed_id: 'ordered-restarted'};
      sw.applySnapshotPayload(JSON.parse(JSON.stringify(state)), true);
      check('a new feed adopts its lower lifecycle revision before normalizing its gauge',
        sw.eval('_pendingRevision') === 0 && sw.eval('lastData.counters.in_flight') === 0);

      const beforeFull = streams.at(-1);
      const full = sw.fetchBootstrap('full');
      const fullRequest = pending.shift();
      let joined = 0;
      const resume = sw.fetchBootstrap('resume', () => joined++);
      const none = sw.fetchBootstrap('none', () => joined++);
      emit(beforeFull, 'record', mkRec('during-full'), '2');
      check('mandatory full refresh closes SSE at issue and ordinary polls join without superseding it',
        beforeFull.readyState === 2 && sw.eval('source') === null && pending.length === 0 &&
        ids() === 'ordered-base' && sw.eval('lastSeq') === 1);
      reply(fullRequest, state);
      await Promise.all([full, resume, none]);
      const resumed = streams.at(-1);
      emit(resumed, 'snapshot', {...state, incremental: true, seq: 2, records: [mkRec('during-full')]});
      check('full refresh reopens from its accepted cursor and replays intervening arrivals exactly once',
        joined === 2 && resumed !== beforeFull && resumed.url.includes('since=1') &&
        ids() === 'ordered-base,during-full' && sw.eval('lastSeq') === 2);

      const older = sw.fetchBootstrap('full'), olderRequest = pending.shift();
      const newer = sw.fetchBootstrap('full'), newerRequest = pending.shift();
      const streamCount = streams.length;
      reply(olderRequest, state);
      await older;
      check('an older full completion cannot reopen SSE or replace state during a newer full refresh',
        streams.length === streamCount && sw.eval('source') === null && ids().includes('during-full'));
      state = {...state, seq: 3, records: [mkRec('ordered-base'), mkRec('during-full'), mkRec('newest-full')]};
      reply(newerRequest, state);
      await newer;
      check('only the newest full completion owns stream reopening',
        streams.length === streamCount + 1 && streams.at(-1).url.includes('since=3') && ids().includes('newest-full'));

      const unexpected = sw.fetchBootstrap('resume');
      reply(pending.shift(), {...state, seq: 1, records: [mkRec('ordered-base')]});
      await sleep(5);
      const replacement = pending.shift();
      check('an unexpected full resume response uses the barrier without erasing newer live state',
        replacement && !replacement.url.includes('since=') && sw.eval('source') === null &&
        ids().includes('newest-full') && sw.eval('lastSeq') === 3);
      reply(replacement, state);
      await unexpected;

      const failed = sw.fetchBootstrap('full');
      pending.shift().resolve({ok: false, json: async () => ({error: 'unavailable'})});
      await failed;
      check('failed full refresh preserves rows and reopens SSE with a full-snapshot cursor',
        ids().includes('newest-full') && streams.at(-1).readyState === 1 && !streams.at(-1).url.includes('since='));

      // Native SSE reconnects can deliver a full snapshot after an HTTP poll
      // already advanced rows/cursor. Reject it before any state mutation.
      const oldReconnect = streams.at(-1), captured = JSON.parse(JSON.stringify(state));
      const poll = sw.fetchBootstrap('resume');
      const arrival = mkRec('http-arrival');
      state = {...state, seq: 4, records: [...state.records, arrival]};
      reply(pending.shift(), {...state, incremental: true, records: [arrival]});
      await poll;
      emit(oldReconnect, 'snapshot', captured);
      const recapture = sw.eval('_bootFull'), recaptureRequest = pending.shift();
      check('a delayed SSE full snapshot cannot erase a newer HTTP arrival or its advanced cursor',
        oldReconnect.readyState === 2 && sw.eval('source') === null && ids().includes('http-arrival') &&
        sw.eval('lastSeq') === 4 && sw.eval('lastData.counters.in_flight') === 0 && recaptureRequest);
      reply(recaptureRequest, state);
      await recapture;
      check('stale SSE full recapture reopens exactly from the fresh accepted snapshot',
        ids().includes('http-arrival') && streams.at(-1).url.includes('since=4') && pending.length === 0);

      const pendingSource = streams.at(-1), beforePending = JSON.parse(JSON.stringify(state));
      const held = {...mkRec('revision-pending', 0), paused: true, throttled: true, attempts: []};
      emit(pendingSource, 'begin', {record: held, in_flight: 1, pending_revision: 1});
      state = {...state, pending_revision: 1, in_flight_records: [held], counters: {...state.counters, in_flight: 1}};
      emit(pendingSource, 'snapshot', beforePending);
      const pendingRecapture = sw.eval('_bootFull'), pendingRequest = pending.shift();
      check('an equal-cursor SSE full snapshot cannot erase newer pending rows or their gauge',
        pendingSource.readyState === 2 && sw.eval('source') === null && ids().includes('revision-pending') &&
        sw.eval('lastSeq') === 4 && sw.eval('lastData.counters.in_flight') === 1 && pendingRequest);
      reply(pendingRequest, state);
      await pendingRecapture;
      check('a fresh full snapshot seeds each pending row with its authoritative lifecycle watermark',
        sw.eval('lastData.records.find(r => r.id === "revision-pending")._pendingRevision') === 1 && pending.length === 0);

      const updates = streams.at(-1);
      const released = {...held, paused: false, throttled: false};
      emit(updates, 'update', {record: released, in_flight: 1, pending_revision: 3});
      emit(updates, 'update', {record: held, in_flight: 1, pending_revision: 2});
      check('older equal-attempt updates cannot restore cleared pause or throttle state',
        sw.eval('lastData.records.find(r => r.id === "revision-pending").paused') === false &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending").throttled') === false);
      const snapshotPoll = sw.fetchBootstrap('resume');
      reply(pending.shift(), {...state, incremental: true, records: [], pending_revision: 2, in_flight_records: [held]});
      await snapshotPoll;
      check('an older HTTP pending snapshot cannot revert same-attempt pause/throttle progress',
        sw.eval('lastData.records.find(r => r.id === "revision-pending").paused') === false &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending")._pendingRevision') === 3);
      emit(updates, 'update', {record: {...held, attempts: [{}]}, in_flight: 1, pending_revision: 4});
      check('a newer row revision still advances pause, throttle and retry fields together',
        sw.eval('lastData.records.find(r => r.id === "revision-pending").paused') === true &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending").throttled') === true &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending").attempts.length') === 1);

      // The shared gate also protects an unexpectedly stale full HTTP result.
      // Its outer completion must not reopen while its replacement is pending.
      const staleHTTP = sw.fetchBootstrap('full');
      reply(pending.shift(), state); // revision1, older than accepted update4
      await sleep(5);
      const finalReplacement = pending.shift();
      const countBeforeReplacement = streams.length;
      check('a rejected full HTTP snapshot cannot reopen over its newer recapture owner',
        finalReplacement && sw.eval('source') === null &&
        sw.eval('lastData.records.find(r => r.id === "revision-pending")._pendingRevision') === 4);
      state = {...state, pending_revision: 4, in_flight_records: [{...held, attempts: [{}]}]};
      reply(finalReplacement, state);
      await staleHTTP;
      check('only the accepted recapture reopens after a stale full HTTP result',
        streams.length === countBeforeReplacement + 1 && pending.length === 0 && sw.eval('_bootFull') === null);
    } finally {
      sw.close();
    }
  }

  // Delegated operator actions replace their own clicked node synchronously.
  // Outside-click dismissal must use the original event path, not detached
  // target ancestry, for both edits and pending/confirmed mutations.
  for (const [kind, action] of [['pause','pause-edit'], ['pause','pause-resume'], ['debug','debug-edit'], ['debug','debug-stop'], ['limits','limit-edit']]) {
    const isolated = dashboardDOM(assembleHTML(), pageOptions);
    const sw = isolated.window, sd = sw.document;
    try {
      await sleep(60);
      let resolveMutation;
      const originalFetch = sw.fetch;
      sw.fetch = (url, options) => options?.method === 'POST'
        ? new Promise(resolve => { resolveMutation = resolve; }) : originalFetch(url, options);
      const pause = {ok:true,paused:true,holds:[{id:'first',all:true,duration:'15m',max_queued:7}],known_clients:['c'],known_providers:['p']};
      const debug = observerFixture({ok:true,enabled:true,sessions:[{id:'first',clients:['c'],duration:'15m'},{id:'second',clients:['second'],duration:'1h'}],known_clients:['c','second'],known_models:[]});
      sw.applyPauseState(pause);
      sw.applyDebugState(debug);
      sw.applyThrottleState({ok:true,active:true,known_providers:['p'],throttles:[{provider:'p',requests:3,request_window:'2m'}]});
      const toggle = kind === 'pause' ? sw.togglePauseMenu : kind === 'debug' ? sw.toggleDebugMenu : sw.toggleLimitsMenu;
      toggle({stopPropagation(){}});
      const menu = sd.getElementById(kind + '-menu');
      const button = menu.querySelector('[data-operator="' + action + '"]');
      let originalPathInside = false;
      sd.addEventListener('click', e => { if (e.target === button) originalPathInside = e.composedPath().includes(menu); });
      button.click();
      check(action + ' stays open when its own delegated click detaches the target',
        !button.isConnected && originalPathInside && !menu.hidden);
      if (resolveMutation) {
        const confirmed = kind === 'pause' ? {...pause,paused:false,holds:[]} : {...debug,sessions:[debug.sessions[1]]};
        resolveMutation({ok:true,json:async()=>confirmed});
        await sleep(5);
        check(action + ' remains open after confirmed scoped mutation', !menu.hidden);
      }
      sd.querySelector('main').click();
      check(action + ' still closes on a genuine outside click', menu.hidden);
    } finally { sw.close(); }
  }

  // Observer authority, operator confirmation, and keyboard regressions.
  {
    const baseRules = [{mode: 'pattern', from: '(?P<name>a)', to: '${name}x'}];
    const baseCanon = modelFixture(baseRules, {' a ': 'ax', 'server-model': 'canonical'});
    const stormFixture = {enabled:true,banner_enabled:true,storms:[{
      provider:'neutral.example',model:'',scope:'provider',state:'open',reason:'http_503',quota:false,
      error_percent:75,error_requests:9,failures:15,samples:20,window_ms:60000,queued:4,
      active_models:3,affected_models:2,affected_model_percent:200/3,
      retry_at:'2026-09-05T12:00:00Z',recovery_successes:0,recovery_required:2,
    }]};
    let state = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'frontend-audit', seq: 1,
      pending_revision: 0, incremental: false, records: [{...mkRec('audit-row'), model: ' a ', time_bucket: 'night'}],
      in_flight_records: [], counters: {in_flight: 0, total_requests: 1}, kpi: {requests: 1}, dash: {},
      model_canon: baseCanon, storage: {enabled: true, dropped: 3}, storm:stormFixture,
    });
    const streams = [], requests = [];
    let adminReply, adminCalls = 0, chartReply;
    const isolated = dashboardDOM(assembleHTML(state), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      const fallback = win.fetch;
      win.fetch = (url, options) => {
        if (String(url).includes('/metrics/bootstrap')) return new Promise(resolve => requests.push(resolve));
        if (String(url).includes('/metrics/agg/chart') && chartReply) return chartReply;
        if (String(url).startsWith('/admin/') && options?.method === 'POST' && adminReply) {adminCalls++; return adminReply;}
        return fallback(url, options);
      };
      win.EventSource = function(url) {
        const stream = {url, handlers: {}, readyState: 1, addEventListener(type, cb) {this.handlers[type] = cb;}, close() {this.readyState = 2;}};
        streams.push(stream); return stream;
      };
    }});
    const sw = isolated.window, sd = sw.document;
    const emit = (type, data, seq) => streams.at(-1).handlers[type]({type, data: JSON.stringify(data), lastEventId: seq});
    const reply = p => requests.shift()({ok: true, json: async () => p});
    try {
      await sleep(40);
      check('observer dictionary uses exact raw keys and Go named-capture output without JS execution',
        sw.canonicalModel(' a ') === 'ax' && sw.canonicalModel('a') === 'a');
      check('time scope uses server classification even when browser time differs',
        sw.recordMatchesDim(state.records[0], 'time', 'night') && !sw.recordMatchesDim({...state.records[0], time_bucket: 'work'}, 'time', 'night'));
      check('durable storage drops are discoverable in the existing footer',
        sd.getElementById('f-state').textContent.includes('3 storage drops') && sd.getElementById('f-state').title.includes('not saved durably'));
      sw.applyBootstrapState({...state, storage: {enabled: true, dropped: 0, totals_degraded: true}});
      check('a degraded boot scan is discoverable in the footer as totals degraded',
        sd.getElementById('f-state').textContent.includes('totals degraded') &&
        sd.getElementById('f-state').title.includes('startup history scan failed'));
      sw.applyBootstrapState({...state, storage: {enabled: true, dropped: 0}});
      check('a clean storage signal renders no degraded footnote',
        !sd.getElementById('f-state').textContent.includes('totals degraded') && !sd.getElementById('f-state').title);
      const banner = sd.getElementById('storm-banner');
      check('embedded bootstrap exposes compact actionable provider storm summaries',
        banner && !banner.hidden && banner.getAttribute('role') === 'status' && banner.getAttribute('aria-live') === 'polite' &&
        banner.textContent.includes('neutral.example · all models') && banner.textContent.includes('http_503') &&
        banner.textContent.includes('75% failed attempts') && banner.textContent.includes('9 requests affected') &&
        banner.textContent.includes('4 queued') && banner.querySelector('button[aria-haspopup="dialog"]'));
      const incidentButton = banner.querySelector('button');
      incidentButton.focus();
      incidentButton.click();
      await sleep(20);
      const incidentDialog = sd.getElementById('storm-dialog');
      const detail = label => [...incidentDialog.querySelectorAll('dt')].find(node => node.textContent === label)?.nextElementSibling.textContent;
      const closeIncident = incidentDialog.querySelector('[data-operator="storm-close"]');
      check('incident click opens shared modal with distinct requests, attempt rates and provider evidence',
        !incidentDialog.hidden && incidentDialog.getAttribute('role') === 'dialog' &&
        sd.activeElement === closeIncident && sd.querySelector('main').hasAttribute('inert') &&
        detail('Requests affected') === '9' && detail('Failed upstream attempts') === '15' &&
        detail('Sampled upstream attempts') === '20' && detail('Upstream attempt failure rate') === '75%' &&
        detail('Queued requests') === '4' && detail('Detection window') === '1m' && detail('Error') === 'http_503' &&
        detail('Next retry') === new sw.Date(stormFixture.storms[0].retry_at).toLocaleString() &&
        detail('Affected active models') === '2 / 3 (66.7%)');
      const updatedStorm = {...stormFixture,storms:[{...stormFixture.storms[0],queued:7,error_requests:10,
        state:'half_open',recovery_successes:1,retry_at:'2026-09-05T12:00:30Z'}]};
      sw.applyBootstrapState({...state,storm:updatedStorm});
      check('accepted bootstrap updates the open incident while keeping close control and trigger focus identities',
        sd.activeElement === closeIncident && incidentDialog.querySelector('[data-operator="storm-close"]') === closeIncident &&
        banner.querySelector('button') === incidentButton && detail('Queued requests') === '7' &&
        detail('Requests affected') === '10' && detail('Successful recovery probes') === '1 / 2' &&
        detail('Next retry') === new sw.Date(updatedStorm.storms[0].retry_at).toLocaleString());
      closeIncident.dispatchEvent(new sw.KeyboardEvent('keydown', {key:'Escape',bubbles:true,cancelable:true}));
      check('incident Escape closes shared modal and restores the current incident trigger',
        incidentDialog.hidden && sd.activeElement === incidentButton && !sd.querySelector('main').hasAttribute('inert'));
      incidentButton.click();
      await sleep(20);
      sw.applyBootstrapState({...state,storm:{...stormFixture,storms:[]}});
      check('resolved incident stays reviewable without showing stale counts or stealing focus',
        !incidentDialog.hidden && incidentDialog.textContent.includes('no longer active') && !incidentDialog.querySelector('dl') &&
        sd.activeElement === closeIncident);
      closeIncident.click();
      check('resolved incident close restores a visible dashboard control',
        incidentDialog.hidden && (sd.activeElement === sd.getElementById('btn-settings') ||
          sd.activeElement === sd.getElementById('btn-nav')));

      const unspecifiedStorm = {...stormFixture,storms:[stormFixture.storms[0], {...stormFixture.storms[0],scope:'model',model:''}]};
      sw.applyBootstrapState({...state,storm:unspecifiedStorm});
      check('unspecified model incident remains distinct from the provider and does not hide valid incidents',
        banner.querySelectorAll('button').length === 2 && banner.textContent.includes('(unspecified model)'));
      banner.querySelectorAll('button')[1].click();
      await sleep(20);
      check('unspecified model details preserve exact empty-model scope', detail('Model scope') === '(unspecified model)');
      closeIncident.click();

      const modelStorm = {...stormFixture,storms:[{...stormFixture.storms[0],provider:'<img src=x onerror=alert(1)>',
        model:'raw/model<script>fixture</script>',scope:'model',state:'half_open',reason:'transport',recovery_successes:1}]};
      sw.applyBootstrapState({...state,storm:modelStorm});
      check('model storm labels stay exact and inert in compact recovery summaries',
        banner.textContent.includes('raw/model<script>fixture</script>') && !banner.querySelector('img, script') &&
        banner.textContent.includes('Checking recovery') &&
        !banner.textContent.includes('active models affected'));
      banner.querySelector('button').click();
      await sleep(20);
      check('incident details render hostile provider/model/error values as inert text',
        detail('Provider') === modelStorm.storms[0].provider && detail('Model scope') === modelStorm.storms[0].model &&
        detail('Successful recovery probes') === '1 / 2' && !incidentDialog.querySelector('img, script') && !detail('Active models'));
      const stormRow = banner.querySelector('.storm-row');
      sw.applyBootstrapState({...state,storm:modelStorm});
      check('unchanged storm polls retain the rendered nodes', banner.querySelector('.storm-row') === stormRow);
      for (const storm of [{...stormFixture,enabled:false}, {...stormFixture,banner_enabled:false},
        {...stormFixture,storms:[]}, {...stormFixture,storms:[{...stormFixture.storms[0],error_percent:NaN}]},
        ...[{active_models:-1}, {affected_models:4}, {affected_model_percent:Infinity}, {error_requests:NaN},
          {error_requests:16}, {error_requests:-1}, {error_requests:'9'}].map(values =>
          ({...stormFixture,storms:[{...stormFixture.storms[0],...values}]})), undefined]) {
        sw.applyBootstrapState({...state,storm});
        check('disabled, recovered, missing or malformed storm state clears the banner', banner.hidden && !banner.textContent);
      }
      check('malformed incident updates invalidate open details without misreporting recovery',
        !incidentDialog.hidden && incidentDialog.textContent.includes('unavailable') && !incidentDialog.querySelector('dl'));
      closeIncident.click();
      sw.applyBootstrapState(state);
      const staleStormPoll = sw.fetchBootstrap('none'), freshStormPoll = sw.fetchBootstrap('none');
      const staleStormReply = requests.shift(), freshStormReply = requests.shift();
      freshStormReply({ok:true,json:async()=>({...state,storm:{...stormFixture,storms:[]}})});
      await freshStormPoll;
      staleStormReply({ok:true,json:async()=>state});
      await staleStormPoll;
      check('older bootstrap responses cannot restore recovered storm warnings', banner.hidden);
      sw.applyBootstrapState(state);

      // Retry-mode quota pause surface: banner facts, dialog entries, the
      // operator resume action, and the manual-mode hold's reason label.
      {
        const quotaStorm = {enabled:true,banner_enabled:true,storms:[{
          provider:'neutral.example',model:'',scope:'provider',state:'open',reason:'insufficient_quota',quota:true,
          error_percent:0,error_requests:0,failures:0,samples:0,window_ms:60000,queued:5,
          active_models:0,affected_models:0,affected_model_percent:0,
          retry_at:'2026-09-05T12:01:00Z',recovery_successes:0,recovery_required:1,
        }]};
        sw.applyBootstrapState({...state,storm:quotaStorm});
        check('quota gate rows report the pause and its recovery facts, not storm failure rates',
          !banner.hidden && banner.textContent.includes('neutral.example · all models') &&
          banner.textContent.includes('insufficient_quota') && banner.textContent.includes('Quota pause') &&
          banner.textContent.includes('5 queued') && banner.textContent.includes('0 / 1 recovery probes') &&
          !banner.textContent.includes('% failed attempts'));
        const quotaRow = banner.querySelector('button');
        quotaRow.click();
        await sleep(20);
        check('quota incident dialog shows the pause title, probe facts and a resume action, without storm noise',
          !incidentDialog.hidden && sd.getElementById('storm-dialog-title').textContent === 'Provider quota pause' &&
          detail('Error') === 'insufficient_quota' && detail('Queued requests') === '5' &&
          detail('Successful recovery probes') === '0 / 1' && !detail('Failed upstream attempts') &&
          incidentDialog.querySelector('[data-operator="quota-resume"]')?.dataset.value === 'neutral.example');
        const releaseQuota = Promise.resolve({ok:true,json:async()=>({enabled:true,banner_enabled:true,storms:[]})});
        adminReply = releaseQuota;
        const callsBeforeResume = adminCalls;
        incidentDialog.querySelector('[data-operator="quota-resume"]').click();
        await sleep(20);
        check('operator resume posts to the quota endpoint and repaints from the confirmed storm snapshot',
          adminCalls === callsBeforeResume + 1 && banner.hidden &&
          incidentDialog.textContent.includes('no longer active'));
        adminReply = Promise.resolve({ok:false,json:async()=>({error:'quota endpoint unavailable'})});
        sw.applyBootstrapState({...state,storm:quotaStorm});
        banner.querySelector('button').click();
        await sleep(20);
        incidentDialog.querySelector('[data-operator="quota-resume"]').click();
        await sleep(20);
        check('a failed resume keeps the dialog open with the failure beside the action',
          !incidentDialog.hidden && incidentDialog.querySelector('[data-operator="quota-resume"]') &&
          incidentDialog.textContent.includes('quota endpoint unavailable'));
        closeIncident.click();
        adminReply = null;
        sw.togglePauseMenu({stopPropagation(){}});
        sw.applyBootstrapState({...state,
          pause: {ok:true, paused:true, clients: [], providers: ['neutral.example'],
            holds: [{id:'q1', all:false, new:false, clients: [], providers: ['neutral.example'], known_at_new: [],
              duration:'', until:null, max_queued:0, reason:'insufficient_quota', queued:0}],
            known_clients: [], known_providers: ['neutral.example'], until: null, queued: 0, default_max_queued: 0}});
        check('manual-mode quota holds show the system reason beside their scope',
          sd.getElementById('pf-holds').textContent.includes('neutral.example · insufficient_quota'));
        sw.togglePauseMenu({stopPropagation(){}});
        sw.applyBootstrapState(state);
        // The quota surface consumed the shared admin POST tally; hand the
        // later busy-gate assertions the same zero baseline they assume.
        adminCalls = 0;
      }

      const event = {...mkRec('observer-new'), model: 'server-model', model_canon: {revision: baseCanon.revision, names: {'server-model': 'canonical'}}};
      emit('record', event, '2');
      check('record envelopes merge names before row adoption without retaining dictionary copies',
        sw.canonicalModel('server-model') === 'canonical' && !sw.eval('lastData.records.at(-1).model_canon'));
      sw.eval("modelNames.set('unretained', 'gone'); logArchive = [{model:'archive-only'}]; modelNames.set('archive-only','archive'); debugState.known_models=['debug-only']; modelNames.set('debug-only','debug'); _rebuildRecIdx();");
      check('model dictionary pruning preserves loaded/archive/debug names but removes abandoned names',
        !sw.eval("modelNames.has('unretained')") && sw.eval("modelNames.has('archive-only') && modelNames.has('debug-only')"));
      sw.resetLogWindow();
      check('archive reset releases dictionary names that no loaded surface needs', !sw.eval("modelNames.has('archive-only')"));

      const beforeRows = sw.eval('lastData.records.length'), beforeSeq = sw.eval('lastSeq');
      emit('record', {...mkRec('foreign-model'), model_canon: modelFixture([], {'m':'m'})}, '3');
      check('foreign observer revision cannot advance the cursor or mix differently grouped rows',
        sw.eval('lastSeq') === beforeSeq && sw.eval('lastData.records.length') === beforeRows && requests.length === 1 && sw.eval('source') === null);
      state = {...state, seq: 2, records: [state.records[0], event]};
      delete state.records[1].model_canon;
      reply(state);
      await sleep(20);
      check('foreign observer metadata re-captures through the existing full barrier', requests.length === 0 && streams.at(-1).url.includes('since=2'));

      // A reload between bootstrap and Debug serialization cannot install
      // mixed dictionary revisions or even expose that response's storage state.
      const mixed = sw.fetchBootstrap('full');
      reply({...state,storage:{enabled:true,dropped:99},debug:{ok:true,enabled:false,sessions:[],known_models:[],model_canon:modelFixture()}});
      await sleep(10);
      check('mixed bootstrap observer revisions are rejected before any state adoption',
        requests.length===1 && !sd.getElementById('f-state').textContent.includes('99 storage drops') && sw.eval('source')===null);
      reply(state);
      await mixed;
      check('mixed observer recapture keeps full-barrier reopening with its accepted owner', requests.length===0 && sw.eval('source')!==null);

      const debug = observerFixture({ok:true, enabled:true, sessions:[{id:'dbg-safe',clients:['c'],duration:'15m'}],known_clients:['c'],known_models:[],model_canon:baseCanon});
      sw.applyDebugState(debug);
      sw.toggleDebugMenu({stopPropagation(){}});
      sw.editDebugSession('dbg-safe');
      check('editing Debug retains an allowed preset duration', sd.getElementById('df-dur').value === '15m');
      let release;
      adminReply = new Promise(resolve => {release = resolve;});
      const mutation = sw.stopDebugSession('dbg-safe');
      sw.stopDebugSession('dbg-safe');
      sw.applyDebugState({...debug, enabled:false,sessions:[]});
      check('Debug busy gate disables related actions and ignores duplicate submissions/stale polls',
        adminCalls === 1 && sd.getElementById('btn-debug-stop').disabled && sw.eval('debugState.sessions.length') === 1);
      release({ok:false,json:async()=>({error:'disk unavailable'})});
      await mutation;
      check('failed Debug stop preserves its confirmed session, draft, and visible error',
        sw.eval('debugState.sessions.length') === 1 && sw.eval('debugEditID') === 'dbg-safe' && sd.getElementById('debug-count').textContent.includes('disk unavailable'));
      const oldRevisions = sw.captureOperatorRevisions();
      adminReply = Promise.resolve({ok:true,json:async()=>({...debug, enabled:false,sessions:[],warning:'not persisted'})});
      await sw.stopDebugMenu();
      sw.applyDebugState(debug, oldRevisions.debug);
      check('successful Debug warnings remain visible and late pre-mutation state cannot restore the session',
        sw.eval('debugState.sessions.length') === 0 && !sd.getElementById('debug-menu').hidden && sd.getElementById('debug-count').textContent.includes('not persisted'));

      const hostile = "label');window.__operatorXSS=1;//";
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],throttles:[{provider:hostile,requests:2,request_window:'2m'}]});
      sw.toggleLimitsMenu({stopPropagation(){}});
      sd.querySelector('#lim-holds [data-operator]').click();
      check('provider actions are inert delegated data, including quotes and script text',
        !sw.__operatorXSS && sd.getElementById('lim-provider').value === hostile && !sd.querySelector('#lim-holds [onclick]'));
      check('Limits custom duration survives edit and refresh', sd.getElementById('lim-reqw').value === '2m');
      for (const [value,want] of [['1e3',1000],['1.5',NaN],['-1',NaN],['',0]]) {
        sd.getElementById('lim-conc').value=value;
        check('Limits whole-number parsing: '+JSON.stringify(value), Object.is(sw.limitNum('lim-conc'),want));
      }
      sd.getElementById('lim-conc').value='-1';
      const beforeInvalid=adminCalls;
      sw.applyLimitsMenu();
      check('invalid Limits input is rejected before any request', adminCalls===beforeInvalid && sd.getElementById('lim-count').textContent.includes('whole numbers'));
      adminReply=Promise.resolve({ok:false,json:async()=>({error:'limit write failed'})});
      await sw.clearLimitsMenu();
      check('failed Limits clear preserves the confirmed limit and shows an error', sw.eval('throttleState.throttles.length')===1 && sd.getElementById('lim-count').textContent.includes('limit write failed'));
      // GAP-B pin: operatorCountClear's limits repaint is load-bearing, not
      // behavior-neutral: updateLimitCount early-returns while the count line
      // carries the error flag, so the err clear in the shared reset is what
      // lets a later provider edit repaint the live facts. Pin the whole
      // sequence: failed write (err flagged) -> provider edit -> repaint.
      check('the failed Limits write flags the count line as an error', sd.getElementById('lim-count').dataset.err === '1');
      sw.applyThrottleState({ok:true,active:true,known_providers:[hostile],
        throttles:[{provider:hostile,source:'ui',in_flight:1,queued:2,requests:10,requests_capacity:20,requests_remaining:5}]});
      const errLine = sd.getElementById('lim-count').textContent;
      sw.onLimitProviderChange();
      check('a provider edit repaints the limits count line after a failed write',
        !sd.getElementById('lim-count').dataset.err &&
          sd.getElementById('lim-count').textContent === 'set in UI · in-flight 1 · queued 2 · 5/20 req' &&
          errLine.includes('limit write failed'));

      const draft=sd.createElement('div');
      const rules=[{mode:'pattern',from:' x ',to:' y '},{mode:'exact',from:' padded ',to:' kept ',disabled:true},{mode:'lower',from:' parked ',to:' value ',disabled:true}];
      draft.innerHTML=sw.modelRulesEditorHTML(rules);
      check('model-rule collection preserves exact pattern/replacement and parked bytes',
        JSON.stringify(sw.collectModelRules(draft))===JSON.stringify(rules));
      const previewRules=[{mode:'pattern',from:'(a+)+$',to:'x'},{mode:'lower'}];
      draft.innerHTML=sw.modelRulesEditorHTML(previewRules);
      sw.validateModelRulesDraft(draft);
      check('draft preview never executes backtracking pattern rules', sw.mrDraftSteps(draft).length===1);
      check('basic Settings controls have accessible names', ['int','strings','duration'].every(kind => sw.settingsFieldHTML({kind,key:'sample',label:'Sample setting'},1).includes('aria-label="Sample setting"')));

      sw.hideHdrMenu('limits-menu');
      for (const button of sd.querySelectorAll('.menu-wrap > button[aria-controls]')) {
        const menu = sd.getElementById(button.getAttribute('aria-controls'));
        sw.closeHeaderMenus();
        menu.hidden = false;
        sw.syncHdrMenuExpanded();
        (menu.querySelector('button:not(:disabled), input, select') || button).focus();
        sd.dispatchEvent(new sw.KeyboardEvent('keydown', {key:'Escape', bubbles:true, cancelable:true}));
        check('Escape closes ' + menu.id + ' and restores its trigger focus',
          menu.hidden && button.getAttribute('aria-expanded') === 'false' && sd.activeElement === button);
      }
      const open=sd.querySelector('button[data-open-req]');
      open.focus(); open.click();
      await sleep(20);
      const drawer=sd.getElementById('drawer');
      check('keyboard-operable log button opens an accessible modal and makes background inert',
        !drawer.hidden && !drawer.hasAttribute('inert') && drawer.contains(sd.activeElement) && sd.querySelector('main').hasAttribute('inert'));
      const focus=sw.modalFocusables(drawer);
      focus.at(-1).focus();
      sd.dispatchEvent(new sw.KeyboardEvent('keydown',{key:'Tab',bubbles:true,cancelable:true}));
      check('modal Tab wraps to its first control', sd.activeElement===focus[0]);
      sw.closeDrawer();
      check('closing the drawer hides all controls and restores the initiating focus',
        drawer.hidden && drawer.hasAttribute('inert') && sd.activeElement===open && !sd.querySelector('main').hasAttribute('inert'));

      const gal=sd.getElementById('xp-gallery');
      const xbody=gal.closest('.xp-body');
      sd.documentElement.style.setProperty('--gallery-locked','');
      Object.defineProperty(gal,'scrollHeight',{configurable:true,value:1000});
      const firstCard=gal.firstElementChild;
      Object.defineProperty(firstCard,'offsetTop',{configurable:true,value:7});
      Object.defineProperty(firstCard,'getBoundingClientRect',{configurable:true,value:()=>({height:148})});
      if (gal.children[1]) {
        Object.defineProperty(gal.children[1],'offsetTop',{configurable:true,value:165});
        Object.defineProperty(gal.children[1],'getBoundingClientRect',{configurable:true,value:()=>({height:148})});
      }
      Object.defineProperty(xbody,'getBoundingClientRect',{configurable:true,value:()=>({top:20})});
      Object.defineProperty(gal,'getBoundingClientRect',{configurable:true,value:()=>({top:20})});
      sw.applyGallerySize(gal);
      const capped=xbody.style.height==='148px' && gal.style.height==='';
      Object.defineProperty(firstCard,'getBoundingClientRect',{configurable:true,value:()=>({height:148.25})});
      Object.defineProperty(gal,'getBoundingClientRect',{configurable:true,value:()=>({top:74.25})});
      sw.applyGallerySize(gal);
      const stacked=xbody.style.height==='203px';
      sd.documentElement.style.setProperty('--gallery-locked','1');
      sw.applyGallerySize(gal);
      check('gallery sizing follows the CSS-owned lock state and reserves stacked layout space',
        capped && stacked && xbody.style.height==='' && gal.style.height==='');
      sd.documentElement.style.removeProperty('--gallery-locked');
      Object.defineProperty(firstCard,'getBoundingClientRect',{configurable:true,value:()=>({height:148})});
      Object.defineProperty(gal,'getBoundingClientRect',{configurable:true,value:()=>({top:20})});
      Object.defineProperty(gal,'scrollHeight',{configurable:true,value:148});
      sw.applyGallerySize(gal);
      // The band stays one row even when the content fits: a scrollHeight-
      // conditional "natural" flip was the set-clear-set oscillation.
      const settled=xbody.style.height==='148px';
      check('the band stays one row even when the gallery content fits it', settled);

      sw.applyBootstrapState({...state, storage:{enabled:true,dropped:0}});
      check('zero durable drops clear the footer warning without a second counter',
        !sd.getElementById('f-state').textContent.includes('storage drops') && !sd.getElementById('f-state').title);
      sw.applyBootstrapState({...state, storage:{enabled:false,dropped:3}});
      check('disabled storage never reports stale drops', !sd.getElementById('f-state').textContent.includes('storage drops'));

      const settingsButton=sd.getElementById('btn-settings');
      settingsButton.focus();
      sw.toggleSettings({stopPropagation(){}});
      await sleep(30);
      const sheet=sd.getElementById('settings-sheet');
      check('Settings shares modal focus containment and background inertness',
        !sheet.hidden && sheet.contains(sd.activeElement) && sd.querySelector('main').hasAttribute('inert'));
      sw.closeSettings(true);
      check('Settings closes with focus restored and controls removed from tab order',
        sheet.hidden && sheet.hasAttribute('inert') && sd.activeElement===settingsButton);
      const provider=sd.createElement('div');
      provider.innerHTML=sw.providersEditorHTML({'fixture.example':{usage_keys:{input_tokens:'usage.input'},models_keys:{input_modalities:'input'}}});
      sd.getElementById('settings-fields').appendChild(provider);
      for(const [kind,selector] of [['usage','.sp-ufield'],['model','.sp-mfield']]) {
        const field=provider.querySelector(selector), sec=field.closest('.prov-sec'), freed=field.value;
        field.closest('.prov-urow').remove();
        sw.syncMappingPicker(sec,kind);
        check('removing a '+kind+' mapping refreshes an already-present free-field picker',
          [...sec.querySelectorAll('select option')].some(option=>option.value===freed));
      }
      provider.remove();

      const savedFetch=sw.fetch;
      let watchedSignal, readerCanceled=0;
      sw.fetch=(url, options)=>{
        if(String(url).includes('watch=1')) {
          watchedSignal=options.signal;
          return Promise.resolve({ok:true,body:{getReader(){return {
            read(){return new Promise((resolve,reject)=>watchedSignal.addEventListener('abort',()=>reject(new Error('aborted')),{once:true}));},
            cancel(){readerCanceled++;return Promise.resolve();},
          };}}});
        }
        if(String(url).includes('/admin/restart')) return Promise.resolve({ok:options?.method!=='POST',status:500,
          json:async()=>options?.method==='POST'?{error:'restart rejected'}:{available:true,phase:'idle',started_at:1}});
        return savedFetch(url,options);
      };
      await sw.restartProxy();
      await sleep(10);
      check('a rejected restart cancels its already-open watcher and releases the reader',
        watchedSignal?.aborted && readerCanceled===1 && sd.getElementById('restart-count').textContent.includes('restart rejected'));
      sw.fetch=savedFetch;

      const OriginalDate=sw.Date, oldTZ=process.env.TZ;
      process.env.TZ='America/Toronto';
      try {
        for (const [now,yesterday,older] of [
          ['2026-03-09T00:30:00-04:00','2026-03-08T12:00:00-04:00','2026-03-07T12:00:00-05:00'],
          ['2026-11-02T00:30:00-05:00','2026-11-01T12:00:00-05:00','2026-10-31T12:00:00-04:00'],
        ]) {
          sw.Date=class extends OriginalDate {constructor(...args){super(...(args.length?args:[now]));}};
          check('Yesterday uses calendar subtraction across DST '+now,
            sw.dayDividerLabel({start:yesterday})==='YESTERDAY' && sw.dayDividerLabel({start:older})!=='YESTERDAY');
        }
      } finally {sw.Date=OriginalDate;if(oldTZ===undefined)delete process.env.TZ;else process.env.TZ=oldTZ;}
    } finally {sw.close();}
  }

  // ---- operator unlock: one answered dialog covers every in-flight 401
  // (a slow scan whose 401 lands after the unlock must not ask again), and a
  // rejected token says so instead of silently asking the same question ----
  {
    const authState = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'feed-auth', seq: 1,
      pending_revision: 0, incremental: false, records: [], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 0}, kpi: {requests: 0}, dash: {},
      model_canon: modelFixture(), storm: {enabled: false, banner_enabled: false, storms: []},
      storage: {enabled: true, dropped: 0},
    });
    const isolated = dashboardDOM(assembleHTML(authState), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      win.EventSource = function(url) {
        return {url, handlers: {}, readyState: 1, addEventListener() {}, close() {this.readyState = 2;}};
      };
    }});
    const w = isolated.window, d = w.document;
    const submitDialog = value => {
      d.getElementById('operator-dialog-input').value = value;
      d.getElementById('operator-dialog-form').dispatchEvent(new w.Event('submit', {cancelable: true}));
    };
    try {
      await sleep(30);
      // Freeze the page's own timers so only test-driven calls hit fetch.
      w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null; clearInterval(_clockTimer); _clockTimer = null;');
      w.eval('storeOperatorCredential("")');
      const calls = [];
      let chartGate = null, chartPending = null; // gate exactly one chart request (the slow scan)
      let bootGate = null, bootPending = null;   // gate exactly one bootstrap request (in-flight credential race)
      w.fetch = (url, options) => {
        const auth = options && options.headers ? (options.headers.Authorization || '') : '';
        calls.push({url: String(url), auth});
        if (String(url).includes('/metrics/agg/chart') && chartGate) {
          chartPending = [];
          chartGate = null;
          const gate = chartPending;
          return new Promise(resolve => gate.push(resolve));
        }
        if (String(url).includes('/metrics/bootstrap') && bootGate) {
          bootPending = [];
          bootGate = null;
          const gate = bootPending;
          return new Promise(resolve => gate.push(resolve));
        }
        if (auth === 'Bearer op-token-correct') return Promise.resolve({ok: true, status: 200, json: async () => ({})});
        return Promise.resolve({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      };
      let modalOpens = 0;
      const realOpenModal = w.openModal;
      w.openModal = el => { if (el.id === 'operator-dialog') modalOpens++; return realOpenModal(el); };

      // The unlock: bootstrap's 401 opens the dialog; the chart scan stays in
      // flight and its 401 lands only after the credential was stored.
      const boot = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      chartGate = [];
      const chart = w.operatorFetch('/metrics/agg/chart?window=60m');
      await sleep(5);
      check('a 401 opens the operator dialog once',
        !d.getElementById('operator-dialog').hidden && modalOpens === 1 && w.eval('operatorPrompt !== null'));
      submitDialog('op-token-correct');
      const bootResult = await boot;
      check('unlock retries the original request with the stored credential',
        bootResult.status === 200 && calls.some(c => c.url.includes('/metrics/bootstrap') && c.auth === 'Bearer op-token-correct'));
      check('the dialog closed after unlock',
        d.getElementById('operator-dialog').hidden && w.eval('operatorPrompt === null'));
      chartPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      const chartResult = await chart;
      const chartCalls = calls.filter(c => c.url.includes('/metrics/agg/chart'));
      check('a late 401 after unlock retries silently without a second dialog',
        chartResult.status === 200 && chartCalls.length === 2 && chartCalls[1].auth === 'Bearer op-token-correct' &&
        d.getElementById('operator-dialog').hidden && w.eval('operatorPrompt === null'));

      // A stored credential rejected while a request is in flight: the silent
      // retry must drop it (other callers must not keep burning the server's
      // wrong-credential throttle with it) and the next prompt says so. The
      // request itself still presents the phase-1 credential; the wrong one is
      // stored while it pends, so it is the silent retry that presents (and
      // gets rejected for) the wrong value.
      bootGate = [];
      const stale = w.operatorFetch('/metrics/bootstrap'); // in flight presenting the phase-1 credential
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-wrong")'); // stored while pending
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      check('a rejected silent retry drops the stored credential and shows the notice',
        w.eval('operatorCredential') === '' && !d.getElementById('operator-dialog').hidden &&
        d.querySelector('.operator-dialog-note').textContent.includes('token was rejected'));
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      check('cancel after a rejected silent retry returns the 401 with nothing stored',
        (await stale).status === 401 && w.eval('operatorCredential') === '' && w.eval('operatorRejected === false'));

      // A silent retry whose 401 lands after a NEWER credential was stored
      // (another caller's successful unlock) must wipe only the token it
      // presented - a 401 indicts the presented value, never the current one.
      bootGate = [];
      const raced = w.operatorFetch('/metrics/bootstrap'); // in flight with no credential
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-wrong")'); // the silent retry will present this
      bootGate = [];                                       // gate the silent retry itself
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-correct")'); // a newer unlock lands while it flies
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      check('a late rejection does not wipe a newer stored credential',
        w.eval('operatorCredential') === 'op-token-correct' && !d.getElementById('operator-dialog').hidden);
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      check('cancel keeps the newer credential',
        (await raced).status === 401 && w.eval('operatorCredential') === 'op-token-correct');

      // The post-prompt wipe is value-guarded too: a retry presenting an
      // older token must not wipe a newer one stored while it flew.
      w.eval('storeOperatorCredential("")');
      const gen = w.operatorFetch('/metrics/bootstrap'); // 401 -> dialog
      await sleep(5);
      bootGate = [];                                     // gate the upcoming post-prompt retry
      submitDialog('op-token-wrong');                    // stores W, dispatches the retry
      await sleep(5);
      w.eval('storeOperatorCredential("op-token-correct")'); // a newer generation's unlock
      bootPending.shift()({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      await sleep(5);
      check('a rejected post-prompt retry does not wipe a newer stored credential',
        w.eval('operatorCredential') === 'op-token-correct' && w.eval('operatorRejected === true'));
      check('the raced post-prompt caller returns its 401', (await gen).status === 401);

      // The dialog presents the entered value VERBATIM (the server never
      // trims credentials): edge whitespace must reach the wire untouched.
      w.eval('storeOperatorCredential("")');
      const padded = ' padded-token ';
      const paddedCall = w.operatorFetch('/metrics/bootstrap'); // 401 (no credential) -> dialog
      await sleep(5);
      const savedPadFetch = w.fetch;
      w.fetch = (url, options) => {
        const auth = options && options.headers ? (options.headers.Authorization || '') : '';
        calls.push({url: String(url), auth});
        if (auth === 'Bearer ' + padded) return Promise.resolve({ok: true, status: 200, json: async () => ({})});
        return Promise.resolve({ok: false, status: 401, json: async () => ({error: 'operator token required'})});
      };
      submitDialog(padded);
      check('the dialog presents the token verbatim without trimming',
        (await paddedCall).status === 200 && w.eval('operatorCredential') === padded &&
        calls.some(c => c.auth === 'Bearer ' + padded));
      w.fetch = savedPadFetch;

      // A rejected entry is visible on the next prompt instead of a silent
      // re-ask that reads as "nothing happened".
      w.eval('storeOperatorCredential("")');
      const wrong = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      submitDialog('op-token-wrong');
      const wrongResult = await wrong;
      check('a wrong entry clears the credential and returns the 401',
        wrongResult.status === 401 && w.eval('operatorCredential') === '');
      const retry = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      const note = d.querySelector('.operator-dialog-note');
      check('the re-prompt after a rejection shows the failure notice',
        !d.getElementById('operator-dialog').hidden && note.hasAttribute('data-err') &&
        note.textContent.includes('token was rejected'));
      submitDialog('op-token-correct');
      check('the corrected entry unlocks', (await retry).status === 200);
      w.eval('storeOperatorCredential("")');
      const fresh = w.operatorFetch('/metrics/bootstrap');
      await sleep(5);
      check('a fresh prompt drops the rejection notice',
        !note.hasAttribute('data-err') && note.textContent.includes('This dashboard is protected'));
      d.querySelector('#operator-dialog [data-operator-auth="cancel"]').click();
      check('cancel returns the original 401 without storing anything',
        (await fresh).status === 401 && w.eval('operatorCredential') === '');

      // Concurrent gated calls share one dialog and one entry.
      const opensBefore = modalOpens;
      const a = w.operatorFetch('/metrics/bootstrap');
      const b = w.operatorFetch('/admin/pause');
      await sleep(5);
      check('concurrent 401s share one dialog and one entry',
        !d.getElementById('operator-dialog').hidden && modalOpens === opensBefore + 1);
      submitDialog('op-token-correct');
      check('one entry resolves every joined caller with the credential',
        (await a).status === 200 && (await b).status === 200 && modalOpens === opensBefore + 1);
    } finally {w.close();}
  }

  // ---- dash_background_refresh: the default pauses the tick while hidden;
  // the opt-in keeps it running at the browser's throttled cadence and holds
  // the documented Chromium freeze-exemption Web Lock until visible again ----
  {
    const bgState = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'feed-bg', seq: 1,
      pending_revision: 0, incremental: false, records: [], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 0}, kpi: {requests: 0}, dash: {},
      model_canon: modelFixture(), storm: {enabled: false, banner_enabled: false, storms: []},
      storage: {enabled: true, dropped: 0},
    });
    const isolated = dashboardDOM(assembleHTML(bgState), {...pageOptions, beforeParse(win) {
      pageOptions.beforeParse(win);
      win.EventSource = function(url) {
        return {url, handlers: {}, readyState: 1, addEventListener() {}, close() {this.readyState = 2;}};
      };
      win.__lockRequests = [];
      win.Object.defineProperty(win.navigator, 'locks', {configurable: true, value: {
        request(name, cb) {
          win.__lockRequests.push({name, released: false});
          const entry = win.__lockRequests[win.__lockRequests.length - 1];
          // The real lock releases when the callback's promise settles.
          return Promise.resolve().then(() => cb({release() { entry.released = true; }})).then(() => { entry.released = true; });
        },
      }});
    }});
    const w = isolated.window, d = w.document;
    const setHidden = hidden => {
      w.Object.defineProperty(d, 'hidden', {configurable: true, get: () => hidden});
      d.dispatchEvent(new w.Event('visibilitychange'));
    };
    try {
      await sleep(30);
      w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null; clearInterval(_clockTimer); _clockTimer = null;');
      w.fetch = () => new Promise(() => {}); // ticks stay pending; only state is asserted
      w.eval('armDashboardTicks()');
      // Default off: hidden tears the tick down and never locks.
      setHidden(true);
      check('default hidden pauses the tick without any lock',
        w.eval('_dashTickTimer === null') && w.__lockRequests.length === 0);
      setHidden(false);
      w.eval('armDashboardTicks()');
      // Opt-in: hidden keeps the tick and holds the freeze-exemption lock.
      w.applyDashValues({dash_background_refresh: true});
      setHidden(true);
      await sleep(5); // the lock stub grants in a microtask
      check('background refresh keeps the tick running while hidden and holds the lock',
        w.eval('_dashTickTimer !== null') && w.__lockRequests.length === 1 && w.eval('_bgLockRelease !== null'));
      setHidden(false);
      await sleep(5);
      check('returning to view releases the lock and re-arms the tick',
        w.eval('_bgLockRelease === null') && w.eval('_dashTickTimer !== null') &&
        w.__lockRequests[0].released === true);
      // Hot-reloaded toggle-off releases the hold and stops the tick even
      // while still hidden.
      setHidden(true);
      await sleep(5);
      w.applyDashValues({dash_background_refresh: false});
      await sleep(5); // the stub marks released on promise settlement
      check('a hot-reloaded toggle-off releases the lock and the tick while hidden',
        w.__lockRequests.length === 2 && w.__lockRequests[1].released === true &&
        w.eval('_bgLockRelease === null') && w.eval('_dashTickTimer === null'));

      // A grant landing for a superseded hold (hide/show cycled while the
      // grant was pending) must self-release, not steal the live claim.
      setHidden(false);
      await sleep(5);
      w.applyDashValues({dash_background_refresh: true}); // visible: no auto-activation
      setHidden(true);   // request #3, grant pending
      setHidden(false);  // release: no live claim yet, no-op
      setHidden(true);   // request #4
      await sleep(5);    // both grants land
      check('a grant for a superseded hold self-releases without stealing the live claim',
        w.__lockRequests.length === 4 && w.__lockRequests[2].released === true &&
        w.__lockRequests[3].released === false && w.eval('_bgLockRelease !== null'));
      setHidden(false);
      await sleep(5);
      check('the live claim releases on return to view',
        w.eval('_bgLockRelease === null') && w.__lockRequests[3].released === true &&
        w.eval('_dashTickTimer !== null'));

      // A grant landing after its hold was released (no newer hold taken)
      // must self-release too: it must never hold the lock while visible.
      setHidden(true);   // request #5, grant pending
      setHidden(false);  // release with no live claim: still invalidates the pending grant
      await sleep(5);
      check('a grant landing after release self-releases instead of holding while visible',
        w.__lockRequests.length === 5 && w.__lockRequests[4].released === true &&
        w.eval('_bgLockRelease === null'));

      // A cadence change in a payload that lands while hidden with
      // background refresh off must not re-arm the tick: payload paths stay
      // live while hidden (an in-flight resume fetch, the SSE reset path),
      // and the visibilitychange handler only runs on transitions.
      w.applyDashValues({dash_background_refresh: false});
      setHidden(true);
      w.applyDashValues({dash_poll_interval: '7s'});
      check('a cadence change while hidden with background refresh off does not re-arm the tick',
        w.eval('_dashTickTimer === null') && w.__lockRequests.length === 5);
      // Symmetric activation: the flag flipped on while already hidden (an
      // out-of-band config change) arms the tick and takes the hold with no
      // visibilitychange transition.
      w.applyDashValues({dash_background_refresh: true});
      await sleep(5); // the lock stub grants in a microtask
      check('a hot-reloaded toggle-on while hidden takes the lock and arms the tick',
        w.eval('_dashTickTimer !== null') && w.__lockRequests.length === 6 &&
        w.eval('_bgLockRelease !== null'));
      // With the opt-in on, the same cadence change re-arms the tick. Stop
      // it first so a non-null timer proves the flag-on arm ran: phase 2's
      // activation arm would otherwise satisfy this vacuously.
      w.eval('dashStopDashboardTick()');
      w.applyDashValues({dash_poll_interval: '11s'});
      check('a cadence change while hidden with background refresh on re-arms the tick',
        w.eval('_dashTickTimer !== null'));
    } finally {w.close();}
  }

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
    const isolated = new JSDOM(assembleHTML(seed), {...pageOptions, beforeParse(win) {
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
      ];
      const errorMisses = corpus.filter(([, rec, want]) => sw.recordIsError(rec) !== want).map(([name]) => name);
      check('recordIsError agrees with the 24-row Go IsError corpus' + (errorMisses.length ? ' (missed: ' + errorMisses.join(', ') + ')' : ''),
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
      // skipping flow control and bare non-failures. errorKey mirrors the Go
      // type|code|msg identity the explorer error dimension filters by.
      const entsRec = {
        status_code: 200, error_code: '', error_msg: '',
        attempts: [
          {status_code: 429},
          {status_code: 502, error_type: 'upstream'},
          {status_code: 0, error_type: 'dial_failed'},
          {status_code: 401},
          {status_code: 302},
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
        sw.eval('[RESTART_BUILD_BUDGET_MS, RESTART_READY_BUDGET_MS, RESTART_DRAIN_FALLBACK_MS].join()') === '300000,30000,600000');
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

  reportFailures(false);
  process.exit(failures.length ? 1 : 0);
}
// A crashed run must not swallow what it already knows: without this
// handler the process dies on the raw exception and the accumulated
// failures, the diagnostic dump and the check history never print.
main().catch(err => {
  console.error(err);
  if (diagDump) console.log(diagDump('crash: ' + (err && err.message)));
  reportFailures(true);
  process.exit(1);
});
