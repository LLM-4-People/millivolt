// F4: an open drawer follows the live finalization of its record without
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function regressionsB(H) {
  const {check, d, fire, html, mkRec, settleUntil, sleep, w} = H;
  const state = H.state;

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
};
