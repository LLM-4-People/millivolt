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
const check = (label, cond) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + label);
  if (!cond) failures.push(label);
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
  feed_id: 'feedA', seq: 30, oldest_seq: 1, incremental: false,
  records: LIST.map((r, i) => ({ ...r, __seq: i + 1 })),
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
  buckets: Array.from({ length: Math.ceil((chartNow - chartFrom) / chartStep) }, (_, i) => ({
    t: chartFrom + i * chartStep,
    req: 0, err: 0, in: 0, out: 0, cache: 0, reason: 0, cost: 0,
    ttft: [null, null, null], tps: [null, null, null],
  })),
};
let restartState = { restarted: false, postCount: 0, statusGate: null, postGate: null, status: {} };
let cfgFetches = 0;
// Providers editor fixture: the server-owned canonical usage fields plus one
// mapped provider (neutral fixture label).
const cfgDoc = {
  revision: 'fixture-revision',
  fields: [
    { key: 'providers', category: 'providers', label: 'Provider field maps', help: 'Per-provider JSON key-path overrides.', kind: 'providers', hot_reload: true },
    { key: 'provider_aliases', category: 'providers', label: 'Provider aliases', help: 'Merge an old provider label into its canonical one (old → canonical).', kind: 'aliases', hot_reload: true },
  ],
  categories: [{ id: 'providers', label: 'Providers', help: 'Per-provider usage/cost JSON field-name maps.' }],
  values: { providers: { 'epsilon.example': { cost_keys: ['x_billing_pricing.cost'], usage_keys: { input_tokens: 'x_billing_pricing.inputTokens' }, models_path: '/model-meta', models_keys: { input_modalities: 'input_modalities' }, headers: { 'User-Agent': 'my-shell/1.0 ({{platform}})' } } }, provider_aliases: { 'old.example': 'new.example' } },
  defaults: { providers: {} },
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
          const response = () => { restartState.restarted = true; return { ok: true, json: async () => ({ ok: true, phase: 'building', rank: 0, phase_ms: Date.now(), drain_timeout_ms: 600000, error: '' }) }; };
          return restartState.postGate ? restartState.postGate.then(response) : Promise.resolve(response());
        }
        const status = { ok: true, phase: 'idle', rank: -1, error: '', pid: restartState.restarted ? 424243 : 424242, started_at: restartState.restarted ? 2000 : 1000, available: true, reason: '', ...restartState.status };
        const response = { ok: true, json: async () => status };
        return restartState.statusGate ? restartState.statusGate.then(() => response) : Promise.resolve(response);
      }
      if (u.includes('/admin/pause')) return Promise.resolve({ json: async () => ({ ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], until: null, queued: 0, default_max_queued: 0 }) });
      if (u.includes('/admin/debug')) return Promise.resolve({ json: async () => ({ ok: true, enabled: false, sessions: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], known_models: ['m'], until: null, ttl: '24h', max_bytes: 1048576 }) });
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
          buffer_size: brecs.length,
          counters: bp.counters,
          pending_revision: bp.pending_revision ?? Math.max(0, window.eval('_pendingRevision')),
          // These ordinary fixtures replace the sample collection between
          // scenarios; a live server's same-feed cursor never rewinds with it.
          seq: bp.feed_id === window.eval('feedId') ? Math.max(bp.seq, window.eval('lastSeq')) : bp.seq,
          oldest_seq: bp.oldest_seq,
          feed_id: bp.feed_id, incremental: !!bsince,
          kpi: { requests: 30, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, answer_tokens: 0 },
          dashboard_version: TEST_DASHBOARD_VERSION,
          model_canon: {rules: []},
          dash: {},
          pause: { ok: true, paused: false, clients: [], providers: [], holds: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], until: null, queued: 0, default_max_queued: 0 },
          throttle: { ok: true, throttles: [], known_providers: ['p'], active: false },
          debug: { ok: true, enabled: false, sessions: [], known_clients: ['c'], known_providers: ['epsilon.example', 'p'], known_models: ['m'], until: null, ttl: '24h', max_bytes: 1048576 },
        }) });
      }
      const fp = fullPayload;
      const since = new URL(url, 'http://x').searchParams.get('since');
      const recs = since ? fp.records.filter(r => r.__seq > Number(since)) : fp.records;
      const payload = {
        records: JSON.parse(JSON.stringify(recs)),
        buffer_size: recs.length,
        counters: fp.counters,
        pending_revision: fp.pending_revision ?? 0,
        seq: fp.seq, oldest_seq: fp.oldest_seq,
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
    const entity = {name:'neutral.example',n:3,cost:0,in:0,out:0,cache:0,reasoning:0,err_final:1,
      rate_limit_requests:2,tools:0,cost_per_mtok:null,ttft_p50:null,ttft_p95:null,tps_p50:null,tps_p95:null,
      err_events:4,code:'500',last_ms:0,spark:[1,2],spark_err:[0,1]};
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
  fire('begin', { phase: 'begin', record: inRec, in_flight: 1 });
  await sleep(20);
  const inRow = d.querySelector('#tbl-requests tr.live-row[data-id="reqnew1"]');
  check('begin event renders the in-flight live-row', !!inRow);
  const stable = rows().find(tr => tr === oldFifth);
  fire('end', { phase: 'end', record: mkRec('reqnew1', 200, 1700000032000), in_flight: 0 });
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
  fire('snapshot', { feed_id: 'feedA', seq: 34, oldest_seq: 1, incremental: true,
    records: [{ ...mkRec('reqnew3', 200, 1700000034000) }],
    counters: { in_flight: 0, total_requests: 34, total_errors: 0 } });
  await sleep(20);
  check('incremental snapshot merges without full replace', rows().includes(oldFifth) && rows().length === 34);

  // ---- test 6: feed change (proxy restart) → full replace ----
  fire('snapshot', { feed_id: 'feedB', seq: 2, oldest_seq: 1, incremental: false,
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
    feed_id: 'feedB', seq: 3, oldest_seq: 1,
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
  fire('snapshot', { feed_id: 'feedB', seq: 3, oldest_seq: 1, incremental: true,
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
        records: [], buffer_size: 3, counters: { in_flight: 0, total_requests: 3, total_errors: 0 },
        seq: 3, oldest_seq: 1, feed_id: 'feedB', incremental: true,
        kpi: { requests: 31, errors: 1, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, answer_tokens: 0 },
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
        records: [], buffer_size: 3, counters: { in_flight: 0, total_requests: 0, total_errors: 0 },
        seq: 4, oldest_seq: 1, feed_id: 'feedB', incremental: true,
        kpi: { requests: 0, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, answer_tokens: 0 },
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
        records: JSON.parse(JSON.stringify(recs)), buffer_size: recs.length,
        counters: fp.counters, seq: fp.seq, oldest_seq: fp.oldest_seq,
        feed_id: fp.feed_id, incremental: !!since,
      }) });
    }
    return origTick(url);
  };
  fullPayload = { feed_id: 'feedB', seq: 4, oldest_seq: 1,
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


  // ---- test 8: Logs menu (filter menu + export URL) ----
  const logsMenu = d.getElementById('logs-menu');
  const clearMenu = d.getElementById('clear-menu');
  check('logs menu starts hidden', logsMenu && logsMenu.hidden);
  w.toggleLogsMenu({ stopPropagation() {} });
  check('logs menu opens', !logsMenu.hidden);
  check('logs open sets aria-expanded', d.getElementById('btn-logs').getAttribute('aria-expanded') === 'true');
  check('opening logs closes clear', clearMenu.hidden);
  check('logs menu populated from records', d.querySelector('#lf-provider option[value="p"]') !== null);
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
  // Default model canonicalization pipeline - the ui_check mirror of
  // config.DefaultModelRules, kept in lockstep with the Go table. The
  // harness seeds it like a real boot (the bootstrap payload's
  // model_canon.rules) so every grouped consumer below (debug checklist,
  // explorer scope, Clear/Logs optgroups) sees variants merged.
  const DEF_RULES = [
    { mode: 'lower' },
    { mode: 'pattern', from: '^[a-z0-9][a-z0-9._-]*/', to: '' },
    { mode: 'pattern', from: ':[a-z0-9._-]+$', to: '' },
    { mode: 'pattern', from: '-(?:[a-z]{0,2}fp\\d+|bf\\d+|int\\d+|nf\\d+|[a-z]?q\\d+(?:_[0-9a-z]+)*)$', to: '' },
    { mode: 'pattern', from: '(\\d)\\.(\\d)', to: '$1-$2' },
  ];
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
  // ONE checkbox (grouped display; the session still matches raw spellings),
  // and the models box must share the clients/providers structure - nesting
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
  // Clear/Logs model optgroups. Records, the log leaf, debug session
  // matching, and purge/export keep the exact stored spelling.
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
    // park both broken rows: the gate clears without deleting work
    wrap.querySelectorAll('.mr-row')[0].querySelector('[data-mr-vis]').click();
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
    // Templates + restore-defaults: the shipped pipeline is one click.
    const mrWrap = d.createElement('div');
    mrWrap.innerHTML = w.modelRulesEditorHTML([{ mode: 'lower' }]);
    d.getElementById('settings-fields').appendChild(mrWrap);
    const wrap = mrWrap;
    const tpl = wrap.querySelector('.mr-tpl');
    tpl.value = '1';
    tpl.dispatchEvent(new w.Event('change', { bubbles: true }));
    const rows = [...wrap.querySelectorAll('.mr-row')];
    check('a template appends its pre-filled rule',
      rows.length === 2 && rows[1].querySelector('.mr-from').value === '^[a-z0-9][a-z0-9._-]*/');
    wrap.querySelector('[data-mr-restore]').click();
    const defRows = [...wrap.querySelectorAll('.mr-row')];
    check('restore-defaults replaces the draft with the shipped pipeline',
      defRows.length === 5 && defRows.map(r => r.querySelector('.mr-mode').value).join(',') === 'lower,pattern,pattern,pattern,pattern');
    check('the rule count line tracks the rows',
      wrap.querySelector('.mr-count').textContent === '5 / 64 rules');
    mrWrap.remove();
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
  fire('snapshot', { feed_id: 'feedB', seq: 80, oldest_seq: 1, incremental: false,
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
  fire('snapshot', { feed_id: 'feedB', seq: 81, oldest_seq: 1, incremental: false,
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

    d.getElementById('cf-age').value = '3600000';
    d.getElementById('lf-age').value = '3600000';
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
  fullPayload = { feed_id: 'feedB', seq: 0, oldest_seq: 0, records: [],
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
  fullPayload = { feed_id: 'feedB', seq: 80, oldest_seq: 1,
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
  fire('snapshot', { feed_id: 'feedB', seq: 3, oldest_seq: 1, incremental: false,
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
  fire('snapshot', { feed_id: 'feedB', seq: 5, oldest_seq: 1, incremental: false,
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
  check('explorer cost uses cents with a unit-neutral per-token label',
    w.kpiBlend({cost_per_mtok: 0.025}).includes('2.5¢') && !w.kpiBlend({cost_per_mtok: 0.025}).includes('$/Mtok'));
  const BUCKET_MS = 120000;
  const mkChart = () => ({
    from_ms: CHART_FROM, now_ms: CHART_FROM + 30 * BUCKET_MS, bucket_ms: BUCKET_MS,
    ttft_p: [null, null, null], tps_p: [null, null, null],
    buckets: Array.from({ length: 30 }, (_, i) => ({
      t: CHART_FROM + i * BUCKET_MS,
      req: 0, err: 0, in: 0, out: 0, cache: 0, reason: 0, cost: 0,
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

  // tokens preset: 3 bars + the cache line, in declaration order (buckets 3
  // and 7 still carry traffic from the traffic sub-test → 3 compacted slots)
  w.eval(`
    chartView.preset = 'tokens'; chartView.hidden = {};
    chartAgg.buckets[4].req = 1;
    chartAgg.buckets[4].in = 100; chartAgg.buckets[4].out = 40; chartAgg.buckets[4].reason = 10; chartAgg.buckets[4].cache = 60;
  `);
  const dataTok = w.eval('chartData()');
  const tokSlot = w.eval('_vis.indexOf(4)');
  check('token slots plot in/out/reason/cache', dataTok.length === 5 && tokSlot >= 0 && dataTok[1][tokSlot] === 100 && dataTok[2][tokSlot] === 40 && dataTok[3][tokSlot] === 10 && dataTok[4][tokSlot] === 60);
  w.eval("chartView.hidden = { tokens: ['outTok'] }");
  const dataTok2 = w.eval('chartData()');
  check('hiding a token series nulls its column, count stable', dataTok2.length === 5 && dataTok2[2].every(v => v === null) && dataTok2[1][tokSlot] === 100 && dataTok2[3][tokSlot] === 10 && dataTok2[4][tokSlot] === 60);
  w.eval('chartView.hidden = {}');

  // Speed + latency: one selected percentile per metric + server sparse gate (no
  // compaction - lines keep every bucket, gaps are honest)
  w.eval(`
    chartView.preset = 'latency'; chartView.pct = 95;
    chartAgg.buckets[2].req = 1;
    chartAgg.buckets[2].ttft = [10, 20, 30]; chartAgg.buckets[2].tps = [100.25, 200.75, 300.5];
  `);
  const dataLat = w.eval('chartData()');
  check('speed + latency keeps every bucket (no compaction)', dataLat[0].length === 30 && dataLat[0][2] === CHART_FROM + 2 * BUCKET_MS + BUCKET_MS / 2);
  check('speed + latency reads only the selected percentile, preserving fractional speed', dataLat.length === 3 && dataLat[1][2] === 200.75 && dataLat[2][2] === 20);
  check('speed + latency uses independent unit-honest axes without bands', w.eval(`(() => {
    const opts = upOpts(640, 210);
    return opts.axes[1].scale === 'ytps' && opts.axes[2].scale === 'yttft' &&
      opts.series.length === 3 && opts.series[1].scale === 'ytps' && opts.series[2].scale === 'yttft' &&
      opts.series[1].label === 'speed' && opts.series[2].label === 'latency' && !opts.bands;
  })()`));
  check('server sparse percentiles stay honest gaps', dataLat[1][6] === null && dataLat[2][6] === null);
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

  // Both period totals follow the one percentile control, not bucket means
  // and not a second always-p50 baseline. Names never repeat the dropdown.
  w.eval(`
    chartView.preset = 'latency';
    chartAgg.ttft_p = [110, 220, 330]; chartAgg.tps_p = [101.25, 202.75, 303.5];
  `);
  const latTotals = w.eval('chartTotals()');
  check('speed + latency totals show only selected period values', latTotals.includes('202.75 tok/s') && latTotals.includes('220ms') && !latTotals.includes('101.25') && !latTotals.includes('110ms') && !/p(?:50|95|99)/.test(latTotals));
  for (const [pct, speed, ttft, totalSpeed, totalTTFT] of [[50, 100.25, 10, 101.25, 110], [99, 300.5, 30, 303.5, 330]]) {
    w.setChartPct(String(pct));
    const data = w.eval('chartData()'), totals = w.eval('chartTotals()');
    check(`percentile dropdown chooses both series and period totals at ${pct}`, data.length === 3 && data[1][2] === speed && data[2][2] === ttft && totals.includes(totalSpeed + ' tok/s') && totals.includes(totalTTFT + 'ms'));
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
    traffic: ['req', 'err'],
    tokens: ['inTok', 'outTok', 'reason', 'cache'],
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
    dataLegLat.length === 3 && dataLegLat[1][5] === 200 && dataLegLat[2][5] === 20);
  legTTFT().click();
  const dataLegLatHidden = w.eval('chartData()');
  check('latency toggle hides only its line and exposes its unpressed state',
    legTTFT().getAttribute('aria-pressed') === 'false' && legTTFT().classList.contains('off') &&
    dataLegLatHidden.length === 3 && dataLegLatHidden[1][5] === 200 && dataLegLatHidden[2].every(v => v === null));
  legTTFT().click();
  check('latency metric toggle restores only its own line',
    legTTFT().getAttribute('aria-pressed') === 'true' && w.eval('chartData()[1][5] === 200 && chartData()[2][5] === 20'));
  const pctControl = d.getElementById('chart-pct');
  check('the percentile control names both plotted metrics without a second owner', pctControl.getAttribute('aria-label') === 'Percentile' && pctControl.title.includes('speed and latency'));
  pctControl.value = '99';
  pctControl.dispatchEvent(new w.Event('change', { bubbles: true }));
  check('the percentile dropdown updates both plotted lines together', w.eval('chartView.pct === 99 && chartData()[1][5] === 300 && chartData()[2][5] === 30'));
  w.eval(`upOpts(640, 210).hooks.setCursor[0]({data: chartData(), cursor: {idx: 5, left: 100}, bbox: {width: 640}})`);
  check('hover shows one speed and one latency value with no percentile duplication',
    [...d.querySelectorAll('#chart-hover .chart-hover-row')].map(el => el.textContent).join() === 'speed300 tok/s,latency30ms' &&
    !/p(?:50|95|99)/.test(d.getElementById('chart-hover').textContent));
  w.eval(`chartView.hidden = {}; storage.set('dash.chart', JSON.stringify({preset: 'latency', pct: 99, window: '10080', hidden: {latency: ['ttft', 'dur', 'ttftP50', 'req']}})); loadChartView();`);
  check('saved latency selection survives while obsolete band/duration and foreign series ids are discarded',
    w.eval('chartView.preset === "latency" && chartView.pct === 99 && chartView.window === "10080" && chartView.hidden.latency.join() === "ttft"'));
  w.toggleChartSeries('dur');
  check('unknown or obsolete series cannot enter hidden state', w.eval('chartView.hidden.latency.join() === "ttft"'));
  w.eval("chartView.hidden = {}; chartView.window = 'all'; chartView.pct = 95; fillChartControls()");
  for (const preset of ['traffic', 'tokens', 'errors', 'cost', 'latency']) {
    w.eval(`chartView.preset = '${preset}'`);
    w.renderChart();
    check(`percentile control is ${preset === 'latency' ? 'visible' : 'hidden'} for ${preset}`,
      d.getElementById('chart-pct').hidden === (preset !== 'latency'));
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
  w.eval('chartAgg = null');
  let resetAxisSafe = true;
  try { compactOpts.axes[0].values({}, [0.5]); } catch { resetAxisSafe = false; }
  check('a queued axis callback tolerates chart state being cleared for restart', resetAxisSafe);

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
    // latency preset: no compaction (data non-null) - the chartIsEmpty term
    // of the mount gate is the ONLY blocker left
    w.eval("chartView.preset = 'latency'");
    w.renderChart();
    check('latency preset stays blank on zero traffic (empty gate, data present)',
      w.eval('chartData()') !== null && !box.querySelector('div.uplot') && w.eval('_up') === null);
    // honest-data rule intact: SOME traffic must still plot
    w.eval("chartView.preset = 'cost'; chartAgg.buckets[7].req = 1");
    w.renderChart();
    check('sparse traffic un-blanks and mounts the plot (gaps stay honest)',
      !!box.querySelector('div.uplot') && w.eval('_up') !== null);
    // restore: tear the mounted plot down, unspy, clear overrides + state
    w.eval("if (_up) { _up.destroy(); _up = null; } drawBlank = window.__drawBlankOrig; chartAgg = null; chartView = { window: 'all', pct: 95, preset: 'traffic', hidden: {} }");
    for (const c of box.querySelectorAll('canvas.chart-blank')) c.remove();
    delete box.clientWidth;
    delete box.clientHeight;
  }

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
  w.toggleRestartMenu({ stopPropagation() {} });
  check('restart menu closes on second toggle', rmenu.hidden);

  // ---- test 13: restart sweep + weekday clock ----
  // A feed_id change is the page's "new process" signal: the snapshot resets
  // the log AND refreshAfterRestart re-pulls operator state (pause, throttle,
  // effective config) + aggregates in place when frontend assets match.
  const cfgBefore = cfgFetches;
  fire('snapshot', { feed_id: 'feedC', seq: 1, oldest_seq: 1, incremental: false,
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
  topBtn.click();
  check('add menu opens; already-added providers are not offered', !menu.hidden && !!menu.querySelector('[data-prov-pick="p"]') && !menu.querySelector('[data-prov-pick="epsilon.example"]'));
  menu.querySelector('.prov-new-label').value = 'gamma.example';
  menu.querySelector('[data-prov-add]').click();
  check('new provider card collects with empty maps', JSON.stringify(w.collectSettingsValues().providers['gamma.example']) === JSON.stringify({ cost_keys: [], usage_keys: {}, models_path: '', models_keys: {}, headers: {} }));
  check('add menu closes after adding', menu.hidden);
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
  // models enrichment, upstream headers last.
  check('models enrichment is the last mapping section in every card', [...provRow.querySelectorAll('.st-prov')].every(c2 => {
    const secs = [...c2.querySelectorAll('.prov-body > .prov-sec')];
    return secs.length === 4 &&
      secs[0].querySelector('.prov-lb span').textContent === 'cost keys' &&
      secs[1].querySelector('.prov-lb span').textContent === 'usage keys' &&
      secs[2].querySelector('.prov-lb span').textContent === 'models enrichment' &&
      !!secs[2].querySelector('.sp-mpath') &&
      secs[3].querySelector('.prov-lb span').textContent === 'upstream headers';
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
    fullPayload = { feed_id: 'feedB', seq: 3, oldest_seq: 1,
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
    const payload = { feed_id: w.eval('feedId'), seq: cursor, oldest_seq: 1,
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
      records: [], buffer_size: 0, counters: { in_flight: 0, total_requests: reqs, total_errors: 0 },
      seq: 5, oldest_seq: 1, feed_id: 'feedB', incremental: false,
      kpi: { requests: reqs, errors: 0, in_flight: 0, cost: 0, cost_per_req: null, cost_per_mtok: null, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, reasoning_tokens: 0, answer_tokens: 0 },
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
      w.selectedPauseChecks('pf-clients').includes('client-a') && w.selectedPauseChecks('pf-providers').includes('new.example'));
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
    fullPayload = { feed_id: 'feedB', seq: 5, oldest_seq: 1,
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
  // jsdom reports navigation as not implemented; count that actual reload
  // signal while keeping unexpected errors visible. Chromium verifies the
  // resulting new document separately.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    let reloads = 0;
    dom.virtualConsole.removeAllListeners('jsdomError');
    dom.virtualConsole.on('jsdomError', error => {
      if (error.type === 'not-implemented' && /navigation/.test(error.message)) reloads++;
      else { failures.push(error.message); console.error(error); }
    });
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
      reloads === 0 && calls === 1 && w.eval('kpiAgg.requests') === 37);

    // An unexpected full response to a resume request is re-captured behind
    // the full/SSE barrier. The accepted full carries its own state/version.
    setResponse({ ...payload(TEST_DASHBOARD_VERSION), feed_id: 'same-assets-new-process', incremental: false });
    calls = 0;
    w.fetchBootstrap('resume');
    await sleep(20);
    check('same-assets restart re-captures an unexpected full response without a page reload', reloads === 0 && calls === 2);

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
    check('missing or malformed asset versions never create a reload loop', reloads === 0);

    const pending = [];
    w.fetch = () => new Promise(resolve => pending.push(resolve));
    w.fetchBootstrap('resume');
    w.fetchBootstrap('resume');
    pending[1]({ok: true, json: async () => payload(TEST_DASHBOARD_VERSION, 39)});
    await sleep(10);
    pending[0]({ok: true, json: async () => payload(newVersion, 999)});
    await sleep(10);
    check('a superseded bootstrap cannot reload the page using an outdated version',
      reloads === 0 && w.eval('kpiAgg.requests') === 39);

    let afterRan = false;
    setResponse(payload(newVersion, 999));
    w.fetchBootstrap('resume', () => { afterRan = true; });
    await sleep(20);
    check('changed frontend requests one reload before applying state or boot callbacks',
      reloads === 1 && !afterRan && w.eval('kpiAgg.requests') === 39);
    sweeps = 0;
    const callsBeforeReload = calls;
    w.refreshAfterRestart();
    w.fetchBootstrap('resume');
    w.fetchBootstrap('full');
    await sleep(10);
    check('reload in progress suppresses duplicate navigation and bootstrap work',
      reloads === 1 && calls === callsBeforeReload && sweeps === 0);
    w.fetch = originalFetch;
    w.refreshAggregates = originalAggregates;
    w.fetchSettings = originalSettings;
  }

  // The HTML seed follows the exact same bootstrap application gate as HTTP.
  // Exercise full page boot with saved views, then replay a completion that
  // happened between HTML generation and opening the live feed.
  {
    const seed = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'seed-feed', seq: 12, oldest_seq: 1, pending_revision: 1,
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
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'state-only-feed', seq: 17, oldest_seq: 1,
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
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'purge-epoch-1', seq: 2, oldest_seq: 1, pending_revision: 0,
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
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'ordered-feed', seq: 1, oldest_seq: 1,
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
      provider:'neutral.example',model:'',scope:'provider',state:'open',reason:'http_503',
      error_percent:75,error_requests:9,failures:15,samples:20,window_ms:60000,queued:4,
      active_models:3,affected_models:2,affected_model_percent:200/3,
      retry_at:'2026-09-05T12:00:00Z',recovery_successes:0,recovery_required:2,
    }]};
    let state = observerFixture({
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'frontend-audit', seq: 1, oldest_seq: 1,
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
        incidentDialog.hidden && sd.activeElement === sd.getElementById('btn-settings'));

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
      sd.documentElement.style.setProperty('--gallery-locked','');
      Object.defineProperty(gal,'scrollHeight',{configurable:true,value:1000});
      sw.applyGallerySize(gal);
      const capped=gal.style.height==='340px';
      sd.documentElement.style.setProperty('--gallery-locked','1');
      sw.applyGallerySize(gal);
      check('gallery sizing follows the CSS-owned lock state, without a second height breakpoint',capped && gal.style.height==='');
      sd.documentElement.style.removeProperty('--gallery-locked');

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

  console.log(failures.length ? '\nFAILURES: ' + failures.join(' | ') : '\nALL UI CHECKS PASSED');
  process.exit(failures.length ? 1 : 0);
}
main();
