// jsdom functional test for the dashboard's incremental live-update pipeline.
// Most checks drive the page through its external interfaces: the stubbed
// fetch() and the EventSource listeners the page registers itself. Focused
// helper sections call production render/merge helpers directly to pin
// optimizer contracts that the external paths cannot isolate; they supplement,
// not replace, the end-to-end coverage.
const fs = require('fs');
const path = require('path');
const { JSDOM, requestInterceptor } = require('jsdom');
const fixtures = require('./fixtures');
const { modelFixture, observerFixture, mkRec, LIST, BIG, CHART_FROM, BUCKET_MS, mkChart,
  DEF_RULES, cfgDoc, TEST_DASHBOARD_VERSION, pageOptions, state } = fixtures;

function dashboardDOM(html, options) {
  const resources = options.resources || {};
  return new JSDOM(html, {...options, resources: {
    ...resources,
    interceptors: [...(resources.interceptors || []), dashAssets],
  }, beforeParse(win) {
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

// jsdom does not fetch <script src> / <link href> from disk. Stylesheets stay
// inline for deterministic computed styles; scripts keep their real deferred
// <script src> tags and load through the same requestInterceptor asset loader
// theme_check uses. Building script elements as strings would make the fixture
// an HTML-injection sink, which CodeQL rightly rejects.
const STATIC = path.join(__dirname, '..', '..', 'internal/web/static');
const SETTINGS_SCRIPT = path.join(STATIC, 'js/settings.js');
const dashAssets = requestInterceptor(request => {
  const url = new URL(request.url);
  if (!url.pathname.startsWith('/dash/')) return undefined;
  const rel = decodeURIComponent(url.pathname.slice('/dash/'.length));
  const file = path.resolve(STATIC, rel);
  if (!file.startsWith(STATIC + path.sep)) return undefined;
  // The lazy loader appends the unmarked production URL; leave that request
  // pending so the loader tests can drive its onload/onerror contract. The
  // fixture's includeSettings tag carries the marker and loads the real file.
  if (file === SETTINGS_SCRIPT && url.searchParams.get('fixture') !== '1') {
    return new Promise(() => {});
  }
  return new Response(fs.readFileSync(file), {
    headers: { 'Content-Type': 'application/javascript' },
  });
});
function assembleHTML(bootstrap, options = {}) {
  let html = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
  html = html.replaceAll('__DASHBOARD_VERSION__', TEST_DASHBOARD_VERSION);
  if (bootstrap !== undefined) {
    const data = typeof bootstrap === 'string' ? bootstrap : JSON.stringify(observerFixture({pending_revision: 0, ...bootstrap})).replace(/</g, '\\u003c');
    html = html.replace('__DASHBOARD_BOOTSTRAP__', () => data);
  }
  // Local asset URLs carry the ?v= version query; jsdom reads the file after
  // stripping it, exactly as the server's lookup path does.
  html = html.replace(/<link\s+rel="stylesheet"\s+href="\/dash\/([^"]+)"\s*>/g, (_, ref) => {
    return `<style>\n${fs.readFileSync(path.join(STATIC, ref.split('?')[0]), 'utf8')}\n</style>`;
  });
  // settings.js is lazy in production (the trigger injects it with the shell's
  // version), so the shell has no script tag. Add one only for the functional
  // suite so every moved settings surface is exercised; the lazy-loader tests
  // assemble without it to drive the real trigger path.
  if (options.includeSettings !== false) {
    html = html.replace('</body>', () =>
      `<script defer src="/dash/js/settings.js?v=${TEST_DASHBOARD_VERSION}&amp;fixture=1"></script>\n</body>`);
  }
  return html;
}
function loaded(dom) {
  return new Promise(resolve => {
    if (dom.window.document.readyState === 'complete') resolve();
    else dom.window.addEventListener('load', resolve, { once: true });
  });
}
const html = assembleHTML();

// diagDump is installed once the dashboard DOM exists; until then a failed
// check carries only its label. On every red check it prints the recorded
// activity that explains the failure, so a diagnosis needs no second run
// and no ad-hoc instrumentation, locally or in CI output.
const check = (label, cond) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + label);
  if (!cond) {
    state.failures.push(label);
    if (state.diagDump) console.log(state.diagDump(label));
  }
};
// The shared summary owner: the green path, a red run and a crash all
// report the accumulated failures through this one printer.
const reportFailures = crashed => {
  if (state.failures.length) console.log('\nFAILURES: ' + state.failures.join(' | '));
  else console.log(crashed ? '\nCRASHED BEFORE ANY CHECK FAILED' : '\nALL UI CHECKS PASSED');
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
dom.virtualConsole.removeAllListeners('jsdomError');
dom.virtualConsole.on('jsdomError', error => {
  if (error.type === 'not-implemented' && /navigation/.test(error.message)) state.jsdomReloads++;
  else { state.failures.push(error.message); console.error(error); }
});
// ---------- failure diagnostics ----------
// One bounded activity recorder feeds one dump owner, so any red check or
// a crashed run reports the same decisive context in its own output. The
// recorded owners are the page's lifecycle functions because the settings
// sheet rebuilding under a cached DOM handle is the failure class that
// hides from a bare label: a refill turns every element reference the
// tests hold stale while focus and flags stay plausible. Recording is
// passive - names and one key argument per call, the log capped - so a
// green run prints nothing and pays almost nothing. Installed after the
// deferred scripts load so every owner exists.
function installDiagnostics() {
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
  state.diagDump = why => 'DIAG ' + JSON.stringify({
    why,
    activity: diagEvents,
    activeElement: diagActive(),
    settingsSheetOpen: (() => { const s = d.getElementById('settings-sheet'); return s ? !s.hidden : null; })(),
    jsdomReloads: state.jsdomReloads,
    feedId: w.eval('feedId'),
    lastSeq: w.eval('lastSeq'),
    cannedFeed: typeof state.fullPayload !== 'undefined' && state.fullPayload ? { feed_id: state.fullPayload.feed_id, seq: state.fullPayload.seq } : null,
    settingsDoc: w.eval('typeof settingsDoc !== "undefined" && settingsDoc ? settingsDoc.revision : null'),
    settingsCat: w.eval('typeof settingsCat !== "undefined" ? settingsCat : null'),
  });
}

function checkEntityIdentity() {
  for (const name of ['', 'provider 5', 'private-label', '127.0.0.1', '[::1]', 'localhost',
    'LOCALHOST', 'gateway.LOCALHOST', 'gateway.internal', 'gateway.local', 'host:443', 'https://host.example',
    'bad_label.example', '-host.example', 'host-.example', 'host..example']) {
    check('provider favicon rejects non-public host label ' + JSON.stringify(name),
      w.providerOrigin(name) === '' && !w.entityBadge('provider', name).includes('<img'));
  }
  check('qualified provider favicon keeps the canonical host',
    w.providerOrigin('API.vendor.example') === 'api.vendor.example' &&
    w.entityBadge('provider', 'api.vendor.example').includes('domain=api.vendor.example'));
  check('client labels show the recorded identity verbatim',
    w.clientLabel('opencode/1.18.32') === 'opencode/1.18.32' &&
    w.clientLabel('opencode/latest/2.0.26/cli') === 'opencode/latest/2.0.26/cli' &&
    w.clientLabel('python 1.55') === 'python 1.55' &&
    w.clientLabel('team/2fa') === 'team/2fa' &&
    w.dimLabel('client', 'opencode/1.18.32') === 'opencode/1.18.32' &&
    w.entityBadge('client', 'opencode/1.18.32').includes('>opencode/1.18.32</span>') &&
    w.clientTipHTML('opencode/1.18.32').includes('>opencode/1.18.32</span>'));
}
// Fixed waits are the exception, not a scheduling shortcut. A wait may stay a
// literal duration only when the duration itself is the asserted behavior or
// when the assertion is a negative proof with no observable state to poll:
//   - protocol pacing and poll cadence (SSE debounce windows, explorer
//     live-status refresh above one second),
//   - negative proofs ("nothing was added / changed / reloaded / retried"):
//     poll instead when the finished state exists, and keep the fixed bound
//     only for the window that proves absence,
//   - draining a queued frame/timer callback before a case swaps the scheduler
//     hook that would otherwise own it, or before leaving a suite,
//   - hidden-at-load timer proofs, where the fixed window is the assertion.
// Everything else waits on the page's observable state via settleUntil so a
// loaded runner cannot turn a slow fetch or render into a false failure.
const sleep = ms => new Promise(r => setTimeout(r, ms));
// Poll the page's own observable state instead of sleeping a fixed time: a
// loaded runner can stretch a fetch/render round trip past any hard-coded
// sleep. Returning false on timeout lets the caller's own check()s fail with
// their labels and the diagnostic dump, so a genuine product failure is not
// turned into a harness crash.
async function settleUntil(cond, timeoutMs = 2000) {
  const deadline = Date.now() + timeoutMs;
  while (!cond()) {
    if (Date.now() >= deadline) return false;
    await sleep(10);
  }
  return true;
}


const rows = () => [...d.querySelectorAll('#tbl-requests tr.exp-row[data-id]')].filter(tr => !tr.classList.contains('retry-sub'));
const fire = (type, data, lastEventId) => {
  const cb = state.sseHandlers[type];
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

module.exports = {
  dom, w, d, html, check, reportFailures, rows, fire, sleep, settleUntil,
  installDiagnostics, checkEntityIdentity, assembleHTML, dashboardDOM, loaded,
  JSDOM, fs, path, STATIC,
  mkRec, LIST, BIG, CHART_FROM, BUCKET_MS, mkChart, modelFixture, observerFixture,
  DEF_RULES, cfgDoc, TEST_DASHBOARD_VERSION, pageOptions, state,
};
