// F13: owner cleanup and stable focus identities. A competing prompt must
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function regressionsC(H) {
  const {TEST_DASHBOARD_VERSION, assembleHTML, check, d, dashboardDOM, fire, loaded, mkRec, settleUntil, sleep, w} = H;
  const state = H.state;

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
};
