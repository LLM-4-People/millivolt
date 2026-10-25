// Shared fixtures and the one mutable stub state for the jsdom dashboard
// suite. The page's fetch and EventSource stubs read this object on every
// call, so a section that rebinds a payload changes what the page is served
// next - the single-page contract the inline suite had, with one owner.
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

const mkRec = (id, status = 200, start = Date.now() - 1000) => ({
  id, provider: 'p', model: 'm', client: 'c', key_hash: 'k' + id,
  conversation_id: 'cv', status_code: status, start: new Date(start).toISOString(),
  end: new Date(start + 500).toISOString(), stream: true, time_bucket: 'work',
  ttft_ms: 100, duration_ms: 500, usage: { input_tokens: 10, output_tokens: 5, total_tokens: 15 },
  retries: 0, attempts: [],
});
const LIST = Array.from({ length: 30 }, (_, i) => mkRec('req' + String(i).padStart(3, '0'), 200, 1700000000000 + i * 1000));


// Test 10's 80-record ring; test 10b restores it after the purge wipe.
const BIG = Array.from({ length: 80 }, (_, i) => mkRec('big' + String(i).padStart(3, '0'), 200, 1800000000000 + i * 1000));

// Bucket-math fixtures shared by the chart sections.
const CHART_FROM = 1800000000000;
const BUCKET_MS = 120000;
const mkChart = () => ({
  from_ms: CHART_FROM, now_ms: CHART_FROM + 30 * BUCKET_MS, bucket_ms: BUCKET_MS,
  ttft_p: [null, null, null], tps_p: [null, null, null], dec_p: [null, null, null],
  buckets: Array.from({ length: 30 }, (_, i) => ({
    t: CHART_FROM + i * BUCKET_MS,
    req: 0, err: 0, rl: 0, in: 0, out: 0, cache: 0, reason: 0, cost: 0,
    ttft: [null, null, null], tps: [null, null, null], dec: [null, null, null],
  })),
});

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


// The traffic chart's stubbed payload: the REAL zero-traffic server shape
// (HandleAggChart with an empty store + empty ring) - a clock-aligned
// ZERO-FILLED buckets array (count >= 1, every req 0, all-null percentile
// triples), never an empty one. Pinned server-side by
// TestChartZeroTrafficPayloadShape; test 11g drives it end-to-end.
const chartNow = Date.now();
const chartStep = 120000; // the 60m window's ladder step (2m)
const chartFrom = chartNow - 3600000 - ((chartNow - 3600000) % chartStep);

// One mutable state object owns every payload the stubs serve and every
// counter the sections read, so rebinding a field is visible immediately.
const state = {
  fullPayload: {
    feed_id: 'feedA', seq: 30, incremental: false,
    records: LIST.map((r, i) => ({ ...r, __seq: i + 1 })),
    in_flight_records: [],
    counters: { in_flight: 0, total_requests: 30, total_errors: 0 },
  },
  logPage: { records: [], more: false, cursor_ms: 1 },
  lastLogURL: '',
  logPageGate: null, // when set, log-page responses wait on this promise
  chartPayload: {
    now_ms: chartNow, from_ms: chartFrom, bucket_ms: chartStep,
    ttft_p: [null, null, null], tps_p: [null, null, null], dec_p: [null, null, null],
    ttft_stat: null, tps_stat: null,
    buckets: Array.from({ length: Math.ceil((chartNow - chartFrom) / chartStep) }, (_, i) => ({
      t: chartFrom + i * chartStep,
      req: 0, err: 0, rl: 0, in: 0, out: 0, cache: 0, reason: 0, cost: 0,
      ttft: [null, null, null], tps: [null, null, null], dec: [null, null, null],
    })),
  },
  restartState: { restarted: false, postCount: 0, statusGate: null, postGate: null, status: {} },
  cfgFetches: 0,
  sseHandlers: {},
  initialFetches: [],
  failures: [],
  diagDump: null,
  jsdomReloads: 0,
  bootURL: '',
  box: null,
  innerPoint: null,
};

const TEST_DASHBOARD_VERSION = '0000000000000001';

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
      state.initialFetches.push(u);
      if (u.includes('/admin/restart')) {
        if (opts && opts.method === 'POST') {
          state.restartState.postCount++;
          // postFail simulates a refusal whose body is not JSON-with-an-
          // error field (the operator plane writes plain-text denials): the
          // fallback contract, not a server error message, must surface.
          if (state.restartState.postFail) {
            const status = state.restartState.postFail.status;
            return Promise.resolve({
              ok: false, status,
              json: async () => { throw new SyntaxError('unexpected token < in HTML'); },
              text: async () => '<html>bad gateway</html>',
            });
          }
          const response = () => { state.restartState.restarted = true; return { ok: true, json: async () => ({ ok: true, phase: 'building', rank: 0, drain_timeout_ms: 600000, error: '' }) }; };
          return state.restartState.postGate ? state.restartState.postGate.then(response) : Promise.resolve(response());
        }
        const status = { ok: true, phase: 'idle', rank: -1, error: '', pid: state.restartState.restarted ? 424243 : 424242, started_at: state.restartState.restarted ? 2000 : 1000, available: true, reason: '', ...state.restartState.status };
        const response = { ok: true, json: async () => status };
        return state.restartState.statusGate ? state.restartState.statusGate.then(() => response) : Promise.resolve(response);
      }
      if (u.includes('/admin/pause')) return Promise.resolve({ json: async () => ({ ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], until: null, queued: 0, default_max_queued: 0 }) });
      if (u.includes('/admin/debug')) return Promise.resolve({ json: async () => ({ ok: true, enabled: false, sessions: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], known_models: ['m'], until: null, ttl: '24h', max_bytes: '1MiB' }) });
      if (u.includes('/admin/throttle')) return Promise.resolve({ json: async () => ({ ok: true, throttles: [], known_providers: ['p'], active: false }) });
      if (u.includes('/admin/config')) { state.cfgFetches++; return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(cfgDoc)) }); }
      if (u.includes('/metrics/agg/log')) {
        state.lastLogURL = u;
        const resp = { ok: true, json: async () => JSON.parse(JSON.stringify(state.logPage)) };
        return state.logPageGate ? state.logPageGate.then(() => resp) : Promise.resolve(resp);
      }
      if (u.includes('/metrics/agg/chart')) {
        return Promise.resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(state.chartPayload)) });
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
        const bp = state.fullPayload;
        const bparams = new URL(url, 'http://x').searchParams;
        const bsince = bparams.get('since');
        // A cursor resume delivers only the requested epoch's rows. Once a
        // test moved the page's feed, fullPayload still holds the previous
        // process's ring; serving it under the live feed's identity (below)
        // would fold records the new process never had. The canned ring
        // answers only its own feed; a foreign feed has nothing to add.
        const bsameFeed = !bparams.get('feed') || bparams.get('feed') === bp.feed_id;
        const brecs = bsince
          ? (bsameFeed ? bp.records.filter(r => r.__seq > Number(bsince)) : [])
          : bp.records;
        return Promise.resolve({ ok: true, json: async () => ({
          records: JSON.parse(JSON.stringify(brecs)),
          in_flight_records: bp.in_flight_records,
          counters: bp.counters,
          pending_revision: bp.pending_revision ?? Math.max(0, window.eval('_pendingRevision')),
          // The canned payload's feed identity tracks the page's live
          // epoch: this stub also answers the page's real periodic tick,
          // and a tick announcing a foreign feed fabricates a proxy
          // restart - refreshAfterRestart re-pulls config and refills an
          // open settings sheet, invalidating every DOM handle the
          // current test holds - whenever a tick fires after some test
          // moved the feed. Tests that exercise a restart supply the
          // foreign feed explicitly through their own snapshot dispatches
          // or bootstrap stubs, so the default keeps the same-feed
          // contract and the cursor rule below applies unconditionally:
          // the sample collection between scenarios never rewinds the
          // live cursor.
          seq: Math.max(bp.seq, window.eval('lastSeq')),
          feed_id: window.eval('feedId') || bp.feed_id, incremental: !!bsince,
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
      const fp = state.fullPayload;
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
        addEventListener(type, cb) { state.sseHandlers[type] = cb; },
        close() { this.readyState = 2; },
      };
    };
  },
};

module.exports = {
  modelFixture, observerFixture, mkRec, LIST, BIG, CHART_FROM, BUCKET_MS, mkChart,
  DEF_RULES, cfgDoc, TEST_DASHBOARD_VERSION, pageOptions, state,
};
