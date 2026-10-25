// test 11g: REAL zero-traffic chart payload end-to-end (live-verify
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function chartE2e(H) {
  const {check, d, mkChart, sleep, w} = H;
  const state = H.state;

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
    // window is too little to read a trend, so the mount gate holds until the
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
    // The axes section renders ahead of the mount gate (chartChromeSync),
    // so the latency preset names its dual axes even on the blank: speed on
    // the left, decode latency on the right.
    check('the latency preset names its axes: speed left, decode latency right',
      d.getElementById('chart-axes').textContent.includes('Speed (tok/s)') &&
      d.getElementById('chart-axes').textContent.includes('Latency / decode'));
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
    totalsOv.includes('$12.5/Mtok') &&
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
      byColor[w.eval('COLORS.accent2')] === 'M0.0 32.0L72.0 0.0' &&
      byColor[w.eval('COLORS.ok')] === 'M0.0 0.0L72.0 32.0' &&
      byColor[w.eval('COLORS.muted')] === 'M0.0 32.0L72.0 32.0';
  })());
  check('every summary tile is a plain readout carrying one multi-line spark',
    (totalsOv.match(/class="chart-total"/g) || []).length === 5 &&
      totalsOv.split('<svg class="spark"').length === 6 &&
      (totalsOv.match(/<svg class="spark"[^>]*width="72" height="32"/g) || []).length === 5 &&
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
  // Cascade owner for pins whose selector can appear again later (media
  // overrides or a duplicate declaration): the last matching rule in sheet
  // order wins for the same specificity, so a first-rule read can guard a
  // declaration the browser never uses.
  const lastCSSRule = sel => {
    let found = null;
    const visit = list => {
      for (const r of list) {
        if (r.selectorText === sel) found = r;
        if (r.cssRules && r.cssRules.length) visit(r.cssRules);
      }
    };
    for (const sheet of d.styleSheets) visit(sheet.cssRules);
    return found;
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
  // The 118px floor fits the tallest structure (label + value + two share
  // rows + spark = 113, line-height-1 rows) and the spark refuses to flex-shrink,
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
      tile.style.getPropertyValue('height') === '118px' &&
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
  check('the day divider parks exactly on the measured header edge',
    lastCSSRule('tr.day-sep td')?.style.getPropertyValue('top') === '29.5px');
  check('the request log declares a visible focus ring',
    lastCSSRule('#tbl-requests:focus-visible')?.style.getPropertyValue('outline')?.includes('var(--accent)') === true);
  // Duplicate-rule mutation: a later rule with the same selector owns the
  // browser's declaration. The pins above must read the last match, not a
  // stale first occurrence.
  check('a later duplicate day/focus rule owns the cascade pins', (() => {
    const style = d.createElement('style');
    style.textContent = '#tbl-requests:focus-visible { outline: none; } ' +
      'tr.day-sep td { top: 0px; }';
    d.head.appendChild(style);
    try {
      return firstCSSRule('#tbl-requests:focus-visible')
          ?.style.getPropertyValue('outline')?.includes('var(--accent)') === true &&
        lastCSSRule('#tbl-requests:focus-visible')
          ?.style.getPropertyValue('outline')?.includes('var(--accent)') === false &&
        lastCSSRule('tr.day-sep td')?.style.getPropertyValue('top') === '0px';
    } finally {
      style.remove();
    }
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
  const tps202 = w.eval('fmtTPSCompact(202.75)');
  check('the merged timing tile reads avg latency / speed with sample ranges',
    totalsOv.includes('avg latency / speed</span> <span class="val-pair"><span class="v-ttft">220ms</span><span class="pair-sep">/</span><span class="v-tps">' + tps202 + '</span>') &&
      totalsOv.includes('<span class="chart-sub">10-330ms · 50-300t/s</span>') &&
      !totalsOv.includes('101.25') && !totalsOv.includes('110ms') &&
      !/p(?:50|95|99)/.test(totalsOv) && !totalsOv.includes('tok/s'));
  check('mixed-scale timing bounds take a spaced dash and both units', (() => {
    const savedT = w.eval('JSON.stringify(chartAgg.ttft_stat)');
    const savedP = w.eval('JSON.stringify(chartAgg.tps_stat)');
    const tps = w.eval('fmtTPSCompact(148.239)');
    w.eval('chartAgg.ttft_stat = [1620, 390, 6160]; chartAgg.tps_stat = [148.239, 24, 480]');
    const t = w.eval('chartTotals()');
    w.eval('chartAgg.ttft_stat = JSON.parse(' + JSON.stringify(savedT) +
      '); chartAgg.tps_stat = JSON.parse(' + JSON.stringify(savedP) + ')');
    return t.includes('1.62s</span><span class="pair-sep">/</span><span class="v-tps">' + tps + '</span>') &&
      t.includes('<span class="chart-sub">390ms - 6.16s · 24-480t/s</span>');
  })());
  check('compact range bounds keep their magnitude suffix apart from the unit', (() => {
    const savedP = w.eval('JSON.stringify(chartAgg.tps_stat)');
    w.eval('chartAgg.tps_stat = [2300, 1200, 2400]');
    const t = w.eval('chartTotals()');
    w.eval('chartAgg.tps_stat = JSON.parse(' + JSON.stringify(savedP) + ')');
    return t.includes('· 1.2K-2.4K t/s');
  })());
  check('the percentile dropdown never re-gates the summary timing tile', (() => {
    w.setChartPct('50');
    const t50 = w.eval('chartTotals()');
    w.setChartPct('99');
    w.eval('chartTotals()');
    w.setChartPct('95');
    return t50.includes('220ms') && t50.includes(w.eval('fmt(202.75)')) &&
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
      paths.every(([, dattr]) => dattr === 'M0.0 0.0L72.0 32.0') &&
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
};
