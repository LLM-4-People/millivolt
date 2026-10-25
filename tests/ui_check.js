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
    optsCost.series[2].points.width === 0 && optsCost.series[2].points.size === state.innerPoint.size &&
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
    latency: ['tps', 'ttft', 'dec'],
  };
  for (const [p, want] of Object.entries(pct50Want)) {
    w.eval(`chartView.preset = '${p}'; chartView.pct = 50;`);
    check(`pct=50 keeps every ${p} series row`,
      w.eval('chartPlan().meta.map(m => m.spec.id).join()') === want.join());
  }
  for (const pct of [95, 99]) {
    w.eval(`chartView.preset = 'latency'; chartView.pct = ${pct};`);
    check(`speed + latency keeps all three lines at pct=${pct}`,
      w.eval('chartPlan().meta.map(m => m.spec.id).join()') === 'tps,ttft,dec');
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

  w.eval("chartView.preset = 'latency'; chartView.pct = 95; chartView.hidden = {}; chartAgg.buckets[5].ttft = [10, 20, 30]; chartAgg.buckets[5].tps = [100, 200, 300]; chartAgg.buckets[5].dec = [70, 140, 210]");
  w.renderChart();
  const legTTFT = () => d.querySelector('#traffic-legend .leg-item[data-series="ttft"]');
  const legDec = () => d.querySelector('#traffic-legend .leg-item[data-series="dec"]');
  check('speed + latency legend has three pressed toggles with no repeated percentile labels',
    [...d.querySelectorAll('#traffic-legend .leg-item')].map(b => b.dataset.series).join() === 'tps,ttft,dec' &&
    [...d.querySelectorAll('#traffic-legend .leg-item')].map(b => b.textContent).join() === 'speed,latency,decode' &&
    [...d.querySelectorAll('#traffic-legend .leg-item')].every(b => b.getAttribute('aria-pressed') === 'true'));
  const dataLegLat = w.eval('chartData()');
  check('speed + latency legend controls the selected value for each metric',
    dataLegLat.length === 4 && dataLegLat[0].length === 1 && dataLegLat[1][0] === 200 && dataLegLat[2][0] === 20 && dataLegLat[3][0] === 140);
  legTTFT().click();
  const dataLegLatHidden = w.eval('chartData()');
  check('latency toggle hides only its line and exposes its unpressed state',
    legTTFT().getAttribute('aria-pressed') === 'false' && legTTFT().classList.contains('off') &&
    dataLegLatHidden.length === 4 && dataLegLatHidden[0].length === 1 && dataLegLatHidden[1][0] === 200 && dataLegLatHidden[2].every(v => v === null) && dataLegLatHidden[3][0] === 140);
  legTTFT().click();
  check('latency metric toggle restores only its own line',
    legTTFT().getAttribute('aria-pressed') === 'true' && w.eval('chartData()[1][0] === 200 && chartData()[2][0] === 20 && chartData()[3][0] === 140'));
  legDec().click();
  const dataLegDecHidden = w.eval('chartData()');
  check('decode toggle hides only its own line and keeps the shared duration axis',
    legDec().getAttribute('aria-pressed') === 'false' && legDec().classList.contains('off') &&
    dataLegDecHidden.length === 4 && dataLegDecHidden[0].length === 1 && dataLegDecHidden[3].every(v => v === null) &&
    dataLegDecHidden[1][0] === 200 && dataLegDecHidden[2][0] === 20);
  legDec().click();
  const pctControl = d.getElementById('chart-pct');
  check('the percentile control names the plotted metrics without a second owner', pctControl.getAttribute('aria-label') === 'Percentile' && pctControl.title.includes('speed, latency and decode'));
  pctControl.value = '99';
  pctControl.dispatchEvent(new w.Event('change', { bubbles: true }));
  check('the percentile dropdown updates every plotted line together', w.eval('chartView.pct === 99 && chartData()[1][0] === 300 && chartData()[2][0] === 30 && chartData()[3][0] === 210'));
  w.eval(`upOpts(640, 210).hooks.setCursor[0]({data: chartData(), cursor: {idx: 0, left: 100}, bbox: {width: 640}})`);
  check('hover shows one value per plotted metric with no percentile duplication',
    [...d.querySelectorAll('#chart-hover .chart-hover-row')].map(el => el.textContent).join() === 'speed300 tok/s,latency30ms,decode210ms' &&
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
  w.toggleChartSeries('dec');
  check('the decode series flips under the same preset key and persists',
    w.eval('chartView.hidden.latency.join() === "ttft,dec"') &&
    JSON.parse(w.eval('storage.get("dash.chart")') || '{}').hidden.latency.join() === 'ttft,dec');
  w.toggleChartSeries('dec');
  check('toggling decode back restores it under the same contract',
    w.eval('chartView.hidden.latency.join() === "ttft"'));
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
        w.eval('chartData()[1][0]') === 300 && w.eval('chartData()[2][0]') === 30 && w.eval('chartData()[3][0]') === 210);
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
    const chartFetchURLs = () => state.initialFetches.filter(u => u.includes('/metrics/agg/chart'));
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
      w.eval('chartAgg && chartAgg.now_ms') === state.chartPayload.now_ms);
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
    const chartFetchURLs = () => state.initialFetches.filter(u => u.includes('/metrics/agg/chart'));
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
    state.chartPayload.now_ms += 4000;
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
      w.eval('chartAgg && chartAgg.now_ms') === state.chartPayload.now_ms);
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

  // A line-only preset must fill the plot edge to edge: the domain is the
  // first and last sample, with no half-bucket inset. A four-point
  // speed+latency window used to strand 12.5% of dead space on each side.
  // Bars keep their half slot (pinned above) so edge bars stay full width, and
  // a mixed preset keeps it too because one of its rows still draws bars.
  w.eval(`
    chartAgg.buckets.forEach(b => {
      b.req = 1;
      b.tps = [100, 120, 140]; b.ttft = [20, 30, 40]; b.dec = [60, 80, 100];
    });
    chartView.preset = 'latency'; chartView.hidden = {}; chartData();
  `);
  const lineOpts = w.eval('upOpts(600, 180)');
  const lineXs = w.eval('chartData()[0]');
  const lineRange = lineOpts.scales.x.range({ data: [lineXs] }, lineXs[0], lineXs[lineXs.length - 1]);
  check('line-only x fills the plot from the first sample to the last',
    lineRange[0] === lineXs[0] && lineRange[1] === lineXs[lineXs.length - 1] &&
      lineRange[1] - lineRange[0] === 29 * BUCKET_MS);
  w.eval("chartView.preset = 'cost'; chartData()");
  const costOpts = w.eval('upOpts(600, 180)');
  const costXs = w.eval('chartData()[0]');
  const costRange = costOpts.scales.x.range({ data: [costXs] }, costXs[0], costXs[costXs.length - 1]);
  check('mixed bar/line presets keep the half-slot bar padding',
    costRange[0] === costXs[0] - BUCKET_MS / 2 &&
      costRange[1] === costXs[costXs.length - 1] + BUCKET_MS / 2);
  // Hiding the only bar leaves a line-only plot; the range must follow the
  // visible rows, not the shadowed bar plan.
  w.eval("chartView.hidden = {cost: ['cost']}; chartView.preset = 'cost'; chartData()");
  const hiddenBarOpts = w.eval('upOpts(600, 180)');
  const hiddenBarXs = w.eval('chartData()[0]');
  const hiddenBarRange = hiddenBarOpts.scales.x.range(
    {data: [hiddenBarXs]}, hiddenBarXs[0], hiddenBarXs[hiddenBarXs.length - 1]);
  check('a hidden bar leaves the line-only edge-to-edge range',
    hiddenBarRange[0] === hiddenBarXs[0] &&
      hiddenBarRange[1] === hiddenBarXs[hiddenBarXs.length - 1]);
  w.eval('chartView.hidden = {}');
  // A compacted line window keeps its slot-center domain: the first and last
  // measured samples sit on the plot edges, so adding the half-slot bar
  // padding back would strand dead space.
  w.eval(`
    chartAgg.buckets.forEach(b => {
      b.req = 1;
      b.tps = [100, 120, 140]; b.ttft = [20, 30, 40]; b.dec = [60, 80, 100];
    });
    chartAgg.buckets[3].tps = [null, null, null];
    chartView.preset = 'latency'; chartView.hidden = {}; chartData();
  `);
  const compactLineOpts = w.eval('upOpts(600, 180)');
  const compactLineXs = w.eval('chartData()[0]');
  const compactLineRange = compactLineOpts.scales.x.range(
    {data: [compactLineXs]}, compactLineXs[0], compactLineXs[compactLineXs.length - 1]);
  check('a compacted line window keeps its slot-center edges',
    compactLineXs[0] === 0.5 &&
      compactLineRange[0] === compactLineXs[0] &&
      compactLineRange[1] === compactLineXs[compactLineXs.length - 1]);
  w.eval(`
    chartAgg.buckets[3].tps = [100, 120, 140];
    chartView.preset = 'latency'; chartView.hidden = {}; chartData();
  `);

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
  state.restartState.statusGate = new Promise(resolve => { releaseRestartStatus = resolve; });
  state.restartState.postGate = new Promise(resolve => { releaseRestartPost = resolve; });
  const restartPending = w.restartProxy();
  check('restart action disables immediately while its status check is pending', restartAction.disabled);
  await w.restartProxy(); // a rapid second call must not issue another POST
  releaseRestartStatus();
  state.restartState.statusGate = null;
  await sleep(20);
  check('only one restart POST is issued and the pending request stays disabled',
    state.restartState.postCount === 1 && restartAction.disabled && restartAction.textContent.includes('Restarting'));
  await w.fetchRestartStatus();
  check('an idle status refresh cannot re-enable a locally pending restart', restartAction.disabled);
  check('the header indicates busy while keeping progress accessible',
    d.getElementById('btn-restart').hasAttribute('data-busy') && !d.getElementById('btn-restart').disabled);
  releaseRestartPost();
  state.restartState.postGate = null;
  await restartPending;
  check('restart POST hit the endpoint', state.restartState.restarted);
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
    state.restartState.status = { phase, rank };
    await w.fetchRestartStatus();
    check(`server-reported ${phase} disables the restart action`, restartAction.disabled);
  }
  state.restartState.status = { phase: 'idle', error: 'build failed', rank: 0 };
  await w.fetchRestartStatus();
  check('failed restart restores the action and reports the error',
    !restartAction.disabled && d.getElementById('restart-count').textContent.includes('build failed'));
  state.restartState.status = { available: false, reason: 'source unavailable' };
  await w.fetchRestartStatus();
  check('unavailable restart stays disabled', restartAction.disabled);
  // A binary that can never restart is not OFFERED at all: the control and its
  // menu leave the header row (an empty wrap would hold the row's gap open),
  // and the server's own diagnosis is the status line an attempt would read.
  const restartWrap = d.getElementById('restart-wrap');
  check('an ineligible restart is not offered in the header, and its reason is what the menu would say',
    restartWrap && restartWrap.hidden &&
    d.getElementById('restart-count').textContent === 'source unavailable' &&
    d.getElementById('restart-count').dataset.err === '1');
  // A run in progress keeps the control reachable, so its progress can be
  // reopened; the verdict only withholds the OFFER.
  w.applyRestartEvent({ phase: 'draining', rank: 1, available: false, reason: 'source unavailable' });
  check('a restart in progress keeps the control reachable for its progress',
    !restartWrap.hidden && d.getElementById('btn-restart').hasAttribute('data-busy') && restartAction.disabled);
  w.applyRestartEvent({ phase: 'idle', rank: -1, available: false, reason: 'source unavailable', error: '' });
  check('an idle ineligible restart withholds the control again', restartWrap.hidden);
  // The boot payload is the only pre-menu signal. An absent or unusable
  // section changes nothing: the control stays exactly as the last verdict
  // left it, so a build that never reports eligibility is never worse off
  // than today.
  w.applyRestartState({ available: true, reason: '' });
  w.applyRestartState(null);
  w.applyRestartState({ reason: 'no verdict here' });
  check('a bootstrap without a verdict keeps the control', !restartWrap.hidden);
  w.applyRestartState({ available: false, reason: 'built with -trimpath, so the binary cannot locate its own source; self-restart is unavailable (restart via your process manager)' });
  check('the bootstrap fold withholds the control for an ineligible binary', restartWrap.hidden);
  w.applyRestartState({ available: true, reason: '' });
  check('the bootstrap fold offers the control for an eligible binary', !restartWrap.hidden);
  state.restartState.status = {};
  await w.fetchRestartStatus();
  check('an available status offers the control again', !restartWrap.hidden);
  // The refusal path through operatorErrorBody: a non-2xx whose body is not
  // JSON-with-an-error (a plain-text operator denial, an HTML 502) must
  // reject with the restart site's designed fallback text, marked as a
  // server answer so the generic transport prefix never wraps it. Nothing
  // reddened before this pin (the W20 mutation round proved the fallback
  // could be dropped silently).
  state.restartState.postFail = { status: 502 };
  const refusalPostCount = state.restartState.postCount;
  await w.restartProxy();
  const restartStatusLine = d.getElementById('restart-count');
  check('a non-JSON 502 restart refusal shows the designed fallback, never an empty error',
    state.restartState.postCount === refusalPostCount + 1 &&
    restartStatusLine.textContent === 'restart failed (502)' &&
    restartStatusLine.dataset.err === '1');
  state.restartState.postFail = null;
  w.toggleRestartMenu({ stopPropagation() {} });
  check('restart menu closes on second toggle', rmenu.hidden);

  // ---- test 13: restart sweep + weekday clock ----
  // A feed_id change is the page's "new process" signal: the snapshot resets
  // the log AND refreshAfterRestart re-pulls operator state (pause, throttle,
  // effective config) + aggregates in place when frontend assets match.
  const cfgBefore = state.cfgFetches;
  fire('snapshot', { feed_id: 'feedC', seq: 1, incremental: false,
    records: [{ ...mkRec('restarted1', 200, 1700000200000) }],
    counters: { in_flight: 0, total_requests: 1, total_errors: 0 } });
  // Await the reset's own painted end state and the sweep's config re-pull
  // instead of a fixed sleep: a loaded runner can stretch either past any
  // hard time. Both waits return false on timeout, so the checks below still
  // fail with their labels and the diagnostic dump.
  await settleUntil(() => rows().length === 1 && rows()[0].dataset.id === 'restarted1');
  await settleUntil(() => state.cfgFetches > cfgBefore);
  check('feed change resets the log to the fresh process snapshot', rows().length === 1 && rows()[0].dataset.id === 'restarted1');
  check('feed change sweeps operator state (config re-fetched in place)', state.cfgFetches > cfgBefore);
  check('footer clock carries the weekday', (() => {
    const text = d.getElementById('f-clock').textContent.toLowerCase();
    const weekdays = Array.from({length: 7}, (_, i) =>
      new Date(2024, 0, 7 + i).toLocaleDateString(undefined, {weekday: 'long'}).toLowerCase());
    return weekdays.some(day => text.includes(day));
  })());

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
  // The provider card's trash icon is the PROV_TRASH_SVG markup constant
  // (the header Clear button that used to own this geometry is gone): the
  // rendered card icon must match the constant's paths exactly and carry
  // no class (the .prov-x svg owns this surface's stroke and size).
  {
    const constPaths = w.eval(`(() => {
      const host = document.createElement('div');
      host.innerHTML = PROV_TRASH_SVG;
      return [...host.querySelectorAll('path')].map(p => p.getAttribute('d'));
    })()`);
    const icon = card.querySelector('[data-prov-rm] svg');
    check('the card trash icon renders the constant glyph, class-free',
      JSON.stringify([...icon.querySelectorAll('path')].map(p => p.getAttribute('d'))) === JSON.stringify(constPaths) &&
      icon.getAttribute('class') === null);
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

  // (G3) A model_rules list is an array of objects, so String(val) compares as
  // "[object Object],..." and can fabricate a "default" hint or print that
  // string. The field must not use the scalar default-hint comparison.
  {
    const base = {
      revision: 'mr-hint-r1',
      fields: [{ key: 'model_rules', category: 'ux', label: 'Model rules', help: '', kind: 'model_rules', hot_reload: true }],
      categories: [{ id: 'ux', label: 'UX', help: '' }],
      defaults: { model_rules: DEF_RULES },
      effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    const render = values => {
      const doc = JSON.parse(JSON.stringify(base));
      doc.values = { model_rules: values };
      w.__mrHintDoc = doc;
      w.eval('settingsDoc = window.__mrHintDoc; fillSettingsForm(settingsDoc)');
      return d.querySelector('#settings-fields .st-row[data-key="model_rules"] .st-hint');
    };
    d.getElementById('settings-sheet').hidden = false;
    const untouched = render(JSON.parse(JSON.stringify(DEF_RULES)));
    check('an untouched model rule list renders the default hint', !!untouched && untouched.textContent === 'default');
    const modified = render([
      { mode: 'lower' },
      { mode: 'pattern', from: 'a', to: 'b' },
      { mode: 'pattern', from: 'c', to: 'd' },
      { mode: 'pattern', from: 'e', to: 'f' },
      { mode: 'pattern', from: 'g', to: 'h' },
    ]);
    check('a modified same-length model rule list shows no default hint', !modified);
    const shorter = render([{ mode: 'lower' }]);
    check('a shorter model rule list shows no object string hint', !shorter);
    delete w.__mrHintDoc;
    w.closeSettings(true);
  }

  // ---- MCP settings category pin: the schema-driven mcp category must render
  // the enable switch and the six limit fields, every row restart-required, the
  // rail badge resolving through the SETTINGS_CAT_REF mcp entry (the tool
  // entity icon, not the server fallback), and the category banner naming the
  // restart.
  {
    const doc = {
      revision: 'mcp-r1',
      fields: [
        { key: 'mcp_enabled', category: 'mcp', label: 'Enable MCP endpoint', help: 'Register the streamable HTTP MCP endpoint at /mcp.', kind: 'bool', hot_reload: false },
        { key: 'mcp_page_size', category: 'mcp', label: 'Page size', help: 'Default page for the records and capture listings.', kind: 'int', min: 1, hot_reload: false },
        { key: 'mcp_query_max_rows', category: 'mcp', label: 'Query result rows', help: 'Row cap on one query tool result.', kind: 'int', min: 1, hot_reload: false },
        { key: 'mcp_query_max_bytes', category: 'mcp', label: 'Query result bytes', help: 'Encoded-size cap on one query tool result.', kind: 'bytes', min: 1, hot_reload: false },
        { key: 'mcp_capture_max_bytes', category: 'mcp', label: 'Capture max bytes', help: 'Capture document size above which the document is withheld whole.', kind: 'bytes', min: 1, hot_reload: false },
        { key: 'mcp_query_timeout', category: 'mcp', label: 'Query timeout', help: 'Bounds one proxy read that is not the bounded SQL reader.', kind: 'duration', hot_reload: false },
        { key: 'mcp_timeout', category: 'mcp', label: 'Request timeout', help: 'Bounds every in-process call to the proxy.', kind: 'duration', hot_reload: false },
      ],
      categories: [{ id: 'mcp', label: 'MCP', help: 'The operator-gated /mcp LLM tool endpoint.' }],
      values: { mcp_enabled: true, mcp_page_size: 50, mcp_query_max_rows: 200, mcp_query_max_bytes: '128KiB', mcp_capture_max_bytes: '256KiB', mcp_query_timeout: '2m', mcp_timeout: '30s' },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.__mcpDoc = doc;
    w.eval("settingsDoc = window.__mcpDoc; settingsCat = 'mcp'; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const switchEl = box.querySelector('input.st-switch[data-key="mcp_enabled"]');
    const mcpRows = [...box.querySelectorAll('.st-row[data-cat="mcp"]')];
    const limitRows = ['mcp_page_size', 'mcp_query_max_rows', 'mcp_query_max_bytes', 'mcp_capture_max_bytes', 'mcp_query_timeout', 'mcp_timeout']
      .map(k => box.querySelector(`.st-row[data-key="${k}"]`));
    check('the mcp category renders the enable switch checked and the six limit fields',
      !!switchEl && switchEl.checked && limitRows.every(r => !!r && !!r.querySelector('[data-st-scalar]')) &&
      mcpRows.length === 7);
    check('every mcp row is restart-required and the category banner says so',
      mcpRows.every(r => r.dataset.hot === '0') &&
      d.querySelector('#settings-pane-hd .st-banner').textContent === 'takes a process restart');
    check('the mcp rail badge resolves through SETTINGS_CAT_REF to the tool entity',
      d.querySelector('[data-st-cat="mcp"] .ent-ic').textContent === w.eval('ENTITY_TYPES.tool.icon') &&
      d.querySelector('[data-st-cat="mcp"] .rail-n').textContent === '7');
    w.__mcpDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__mcpDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__mcpDoc;
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

  // A clean Settings reopen skips the whole form rebuild, but the live
  // snapshot (last apply and effective runtime data) can change without a
  // revision change. Reopening must refresh that panel alone: form nodes,
  // scroll and focus stay exactly where the operator left them.
  {
    const originalFetch = w.fetch;
    const base = JSON.parse(JSON.stringify(cfgDoc));
    base.revision = 'live-panel-revision';
    base.fields.unshift({ key: 'db_path', category: 'providers', label: 'Database path', kind: 'string', hot_reload: false });
    base.values.db_path = 'private.db';
    const docAt = (listen, at) => ({
      ...JSON.parse(JSON.stringify(base)),
      effective: {...base.values, listen},
      last_reload: {ok: true, error: '', dropped_keys: [], restart_required: [], at},
    });
    let configDoc = docAt('127.0.0.1:9001', 1757777000000);
    w.fetch = (url, opts) => {
      if (!String(url).includes('/admin/config')) return originalFetch(url, opts);
      return Promise.resolve({ok: true, json: async () => JSON.parse(JSON.stringify(configDoc))});
    };
    try {
      w.openSettings();
      await settleUntil(() => !d.getElementById('settings-sheet').hidden &&
        /9001/.test(d.getElementById('settings-live').textContent) &&
        !!d.querySelector('#settings-fields .st-row[data-key="db_path"]'));
      const liveBefore = d.getElementById('settings-live').textContent;
      const box = d.getElementById('settings-fields');
      const row = box.querySelector('.st-row[data-key="db_path"]');
      const input = row.querySelector('input[data-key="db_path"]');
      input.focus();
      box.scrollTop = 73;
      w.closeSettings(true);
      await settleUntil(() => d.getElementById('settings-sheet').hidden);

      configDoc = docAt('127.0.0.1:9002', 1757779999000);
      w.openSettings();
      await settleUntil(() => !d.getElementById('settings-sheet').hidden &&
        /9002/.test(d.getElementById('settings-live').textContent));
      const liveAfter = d.getElementById('settings-live').textContent;
      check('a clean Settings reopen refreshes the independent live panel',
        liveAfter !== liveBefore && /9002/.test(liveAfter) &&
        liveAfter.includes(new Date(1757779999000).toLocaleString()));
      check('a clean Settings reopen preserves the form nodes and scroll position',
        box.querySelector('.st-row[data-key="db_path"]') === row &&
        row.querySelector('input[data-key="db_path"]') === input && box.scrollTop === 73);

      const nextDoc = docAt('127.0.0.1:9003', 1757781111000);
      configDoc = nextDoc;
      input.focus();
      await w.fetchSettings();
      check('the skip refill keeps focus in the operator\'s form control',
        d.activeElement === input && /9003/.test(d.getElementById('settings-live').textContent) &&
        box.querySelector('.st-row[data-key="db_path"]') === row);
    } finally {
      w.fetch = originalFetch;
      w.closeSettings(true);
    }
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
    const failuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid model-rules draft blocks the save without an uncaught TypeError',
      state.failures.length === failuresBefore &&
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
    const mrHiddenFailures = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden model-rules offender selects its category and focuses it',
      state.failures.length === mrHiddenFailures &&
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
    const intFailuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden scalar-int offender selects its category and focuses it',
      state.failures.length === intFailuresBefore &&
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
      const removeFailuresBefore = state.failures.length;
      captureInputFlash(() => d.getElementById('btn-settings-apply').click());
      check(`${removeCase.label} targets its remove button`,
        state.failures.length === removeFailuresBefore && d.activeElement === button && button.classList.contains('prov-bad') &&
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
    const collectFailuresBefore = state.failures.length;
    w.applySettings();
    check('a collect-time duplicate header opens its collapsed card, focuses the offender and sends no POST',
      state.failures.length === collectFailuresBefore && collectPosts === 0 &&
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
    const earlierCollectFailuresBefore = state.failures.length;
    w.applySettings();
    check('collect-time duplicate input yields to the first invalid request-override card',
      state.failures.length === earlierCollectFailuresBefore && earlierCollectPosts === 0 &&
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
    const failuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid request-overrides draft blocks the save and focuses the offending row',
      state.failures.length === failuresBefore &&
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
    const ctlFailuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a control-byte header value blocks the save and focuses the value input',
      state.failures.length === ctlFailuresBefore &&
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
    const conflictFailuresBefore = state.failures.length;
    captureInputFlash(() => d.getElementById('btn-settings-apply').click());
    check('Apply focuses the conflicting remove button',
      state.failures.length === conflictFailuresBefore && d.activeElement === conflictButton && conflictButton.classList.contains('prov-bad'));
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
    check('the strip hint names the pre-relay client body scope',
      scWrap.querySelector('.mr-hint').textContent.includes('strip removes every configured field present at the top level of the client request body before relay or translation'));
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
    const failuresBefore = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with an invalid sub-conversations draft blocks the save and focuses the offending row',
      state.failures.length === failuresBefore &&
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
    const scHiddenFailures = state.failures.length;
    d.getElementById('btn-settings-apply').click();
    check('Apply with a hidden sub-conversations offender selects its category and focuses it',
      state.failures.length === scHiddenFailures &&
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

  // ---- test 11t: incremental settings dirty tracking ----
  // A keystroke used to collect every category/editor and stringify every
  // key. The dirty path now resolves the edited field's config key,
  // recomputes only that key and maintains a changed-key set; controls with
  // no resolvable key still fall back to the full collector. The fixture is
  // deliberately large so a regression to full-form collection shows up in
  // the collector counters.
  {
    const doc = JSON.parse(JSON.stringify(cfgDoc));
    doc.fields = Array.from({ length: 24 }, (_, i) => ({
      key: 'bulk_' + i,
      category: 'providers',
      label: 'Bulk field ' + i,
      help: 'bulk setting ' + i,
      kind: i % 3 === 0 ? 'int' : 'string',
      hot_reload: true,
    })).concat([
      { key: 'providers', category: 'providers', label: 'Provider field maps', help: 'Per-provider JSON key-path overrides.', kind: 'providers', hot_reload: true },
      { key: 'provider_aliases', category: 'providers', label: 'Provider aliases', help: 'Aliases.', kind: 'aliases', hot_reload: true },
      { key: 'model_rules', category: 'providers', label: 'Model rules', help: 'Rules.', kind: 'model_rules', hot_reload: true },
      { key: 'request_overrides', category: 'providers', label: 'Request override rules', help: 'Rules.', kind: 'request_overrides', hot_reload: true },
      { key: 'sub_conversations', category: 'providers', label: 'Sub-conversation tracking', help: 'Tracking.', kind: 'sub_conversations', hot_reload: true },
    ]);
    doc.categories = [{ id: 'providers', label: 'Providers', help: 'incremental dirty fixture' }];
    doc.values = {
      ...Object.fromEntries(Array.from({ length: 24 }, (_, i) => ['bulk_' + i, i % 3 === 0 ? i : 'value ' + i])),
      providers: { 'epsilon.example': { cost_keys: ['usage.cost'], usage_keys: { input_tokens: 'usage.input' }, models_path: '/model-meta', models_keys: { input_modalities: 'input_modalities' }, ensure_tools: [], headers: { 'X-Test': 'one' } } },
      provider_aliases: { 'old.example': 'new.example' },
      model_rules: [{ mode: 'exact', from: 'a', to: 'b' }],
      request_overrides: [{ client: 'cli', provider: '', model: '', headers: { 'X-Title': 'app' } }],
      sub_conversations: [{ client: 'cli', params: ['convKey'], strip: false }],
    };
    doc.defaults = { ...doc.defaults, providers: {}, model_rules: DEF_RULES };
    doc.revision = 'dirty-r1';
    w.__dirtyDoc = doc;
    w.eval("settingsCat = 'providers'; settingsDoc = window.__dirtyDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const status = () => d.getElementById('settings-count').textContent;
    const apply = d.getElementById('btn-settings-apply');
    const revert = d.getElementById('btn-settings-revert');
    check('the incremental dirty fixture renders every scalar and structured editor',
      box.querySelectorAll('input[data-key^="bulk_"]').length === 24 &&
      !!box.querySelector('[data-kind="providers"]') &&
      !!box.querySelector('[data-kind="aliases"]') &&
      !!box.querySelector('[data-kind="model_rules"]') &&
      !!box.querySelector('[data-kind="request_overrides"]') &&
      !!box.querySelector('[data-kind="sub_conversations"]'));
    {
      const full = w.collectSettingsValues();
      check('the per-key collector matches the full collector for every rendered key',
        typeof w.collectSettingsValue === 'function' &&
        [...box.querySelectorAll('[data-key]')].filter(el => !el.classList.contains('st-row'))
          .every(el => JSON.stringify(w.collectSettingsValue(el.dataset.key)) === JSON.stringify(full[el.dataset.key])));
    }
    const originalKeyCollector = w.collectSettingsValue;
    const originalFullCollector = w.collectSettingsValues;
    let keyCalls = [];
    let fullCalls = 0;
    w.collectSettingsValue = key => { keyCalls.push(key); return originalKeyCollector(key); };
    w.collectSettingsValues = (...args) => { fullCalls++; return originalFullCollector(...args); };
    const resetCounters = () => { keyCalls = []; fullCalls = 0; };
    const checkClean = label => check(label,
      w.settingsDirtyCount() === 0 && !w.settingsIsDirty() && status() === '' &&
      apply.disabled && revert.disabled);
    const checkDirty = (label, n) => check(label,
      w.settingsDirtyCount() === n && w.settingsIsDirty() &&
      status() === (n === 1 ? '1 unsaved' : n + ' unsaved') &&
      !apply.disabled && !revert.disabled);
    const inputEvent = () => new w.Event('input', { bubbles: true });
    const edit = (el, value) => { el.value = value; el.dispatchEvent(inputEvent()); };
    checkClean('the incremental dirty fixture starts clean with both actions disabled');
    // One scalar keystroke: only its own key is collected; the full sheet is
    // never touched.
    resetCounters();
    const bulk1 = box.querySelector('input[data-key="bulk_1"]');
    edit(bulk1, 'edited one');
    check('one scalar input collects only its own config key',
      JSON.stringify(keyCalls) === JSON.stringify(['bulk_1']) && fullCalls === 0);
    checkDirty('the scalar edit reports one unsaved key with Apply armed', 1);
    resetCounters();
    const bulk2 = box.querySelector('input[data-key="bulk_2"]');
    edit(bulk2, 'edited two');
    check('a second scalar input still collects only its own key',
      JSON.stringify(keyCalls) === JSON.stringify(['bulk_2']) && fullCalls === 0);
    checkDirty('a second scalar edit counts two unsaved keys', 2);
    edit(bulk1, 'value 1');
    checkDirty('reverting one scalar keeps the remaining key counted', 1);
    edit(bulk2, 'value 2');
    checkClean('reverting both scalar edits restores the clean draft');
    // Provider mapping and alias changes own the composite editor keys.
    resetCounters();
    const upath = box.querySelector('.prov-umap .sp-upath');
    edit(upath, 'usage.inputTokens');
    check('a provider mapping edit collects only the providers key',
      JSON.stringify(keyCalls) === JSON.stringify(['providers']) && fullCalls === 0);
    checkDirty('a provider mapping edit counts one unsaved key', 1);
    edit(upath, 'usage.input');
    checkClean('reverting the provider mapping restores the clean draft');
    resetCounters();
    const aliasTo = box.querySelector('[data-kind="aliases"] .al-to');
    edit(aliasTo, 'other.example');
    check('an alias edit collects only the provider_aliases key',
      JSON.stringify(keyCalls) === JSON.stringify(['provider_aliases']) && fullCalls === 0);
    checkDirty('an alias edit counts one unsaved key', 1);
    edit(aliasTo, 'new.example');
    checkClean('reverting the alias edit restores the clean draft');
    // Model-rules, request-overrides and sub-conversation row edits.
    resetCounters();
    const mrFrom = box.querySelector('[data-kind="model_rules"] .mr-from');
    edit(mrFrom, 'a2');
    check('a model-rules row edit collects only the model_rules key',
      JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0);
    checkDirty('a model-rules row edit counts one unsaved key', 1);
    edit(mrFrom, 'a');
    checkClean('reverting the model-rules row restores the clean draft');
    resetCounters();
    const roClient = box.querySelector('[data-kind="request_overrides"] .ro-client');
    edit(roClient, 'cli2');
    check('a request-override row edit collects only the request_overrides key',
      JSON.stringify(keyCalls) === JSON.stringify(['request_overrides']) && fullCalls === 0);
    checkDirty('a request-override row edit counts one unsaved key', 1);
    edit(roClient, 'cli');
    checkClean('reverting the request-override row restores the clean draft');
    resetCounters();
    const scClient = box.querySelector('[data-kind="sub_conversations"] .sc-client');
    edit(scClient, 'cli2');
    check('a sub-conversation edit collects only the sub_conversations key',
      JSON.stringify(keyCalls) === JSON.stringify(['sub_conversations']) && fullCalls === 0);
    checkDirty('a sub-conversation edit counts one unsaved key', 1);
    edit(scClient, 'cli');
    checkClean('reverting the sub-conversation edit restores the clean draft');
    // Structural actions each update the cached key directly.
    const provWrap = box.querySelector('[data-kind="providers"]');
    const provCard = provWrap.querySelector('.st-prov');
    resetCounters();
    w.addProviderLabel('delta.example', provCard);
    check('adding a provider card collects only the providers key',
      JSON.stringify(keyCalls) === JSON.stringify(['providers']) && fullCalls === 0);
    checkDirty('adding a provider card counts one unsaved key', 1);
    resetCounters();
    [...provWrap.querySelectorAll('.st-prov')].pop().querySelector('[data-prov-rm]').click();
    check('removing a provider card collects only the providers key',
      JSON.stringify(keyCalls) === JSON.stringify(['providers']) && fullCalls === 0);
    checkClean('removing the added provider card restores the clean draft');
    const mrWrap = box.querySelector('[data-kind="model_rules"]');
    const mrAdd = mrWrap.querySelector('.mr-add');
    mrAdd.querySelector('.mr-mode').value = 'exact';
    mrAdd.querySelector('.mr-new-from').value = 'x';
    mrAdd.querySelector('.mr-new-to').value = 'y';
    resetCounters();
    w.addModelRuleRow(mrAdd);
    check('adding a model-rule row collects only the model_rules key',
      JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0);
    checkDirty('adding a model-rule row counts one unsaved key', 1);
    resetCounters();
    [...mrWrap.querySelectorAll('.mr-row')].pop().querySelector('[data-mr-rm]').click();
    check('removing a model-rule row collects only the model_rules key',
      JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0);
    checkClean('removing the added model-rule row restores the clean draft');
    // The preview-only test spelling drives the bench alone: it must not
    // revalidate the draft, collect the model_rules key or arm Apply/Revert.
    // Rule edits keep the live validation and per-key dirty behavior.
    {
      const mrTest = mrWrap.querySelector('.mr-test');
      const baseline = JSON.stringify(w.collectSettingsValues().model_rules);
      const originalValidate = w.validateModelRulesDraft;
      const originalMark = w.markSettingsDirty;
      let validations = 0;
      let marks = 0;
      w.validateModelRulesDraft = wrap => { validations++; return originalValidate(wrap); };
      w.markSettingsDirty = key => { marks++; return originalMark(key); };
      try {
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrTest, 'a');
        const testKeyCalls = keyCalls.slice();
        const testFullCalls = fullCalls;
        const preview = mrWrap.querySelector('.mr-out').textContent;
        const collected = JSON.stringify(w.collectSettingsValues().model_rules);
        check('the preview-only test spelling refreshes the bench without revalidating or dirtying the draft',
          validations === 0 && marks === 0 && preview === 'b');
        check('the test spelling collects nothing, leaves the model-rules value unchanged and keeps Apply/Revert disabled',
          testKeyCalls.length === 0 && testFullCalls === 0 && collected === baseline &&
          !w.settingsIsDirty() && w.settingsDirtyCount() === 0 &&
          status() === '' && apply.disabled && revert.disabled);
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrFrom, 'a2');
        check('a rule spelling edit still validates once and marks only model_rules dirty',
          validations === 1 && marks === 1 &&
          JSON.stringify(keyCalls) === JSON.stringify(['model_rules']) && fullCalls === 0 &&
          w.settingsDirtyCount() === 1 && status() === '1 unsaved' &&
          !apply.disabled && !revert.disabled);
        edit(mrFrom, '');
        check('an empty rule spelling still renders its error state',
          mrWrap.querySelector('.mr-row').dataset.mrState === 'error' &&
          /needs a from spelling/.test(mrWrap.querySelector('.mr-row .mr-err').textContent));
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrTest, 'a2');
        check('the test spelling stays preview-only beside an invalid rule row',
          validations === 0 && marks === 0 && JSON.stringify(keyCalls) === '[]' && fullCalls === 0 &&
          mrWrap.querySelector('.mr-row').dataset.mrState === 'error' &&
          mrWrap.querySelector('.mr-out').textContent === 'a2');
        edit(mrFrom, 'a');
        checkClean('reverting the rule spelling restores the clean draft after preview-only typing');
        mrWrap.querySelector('.mr-row [data-mr-vis]').click();
        resetCounters();
        validations = 0;
        marks = 0;
        edit(mrTest, 'a');
        check('a parked rule still shows its state and the test spelling neither revalidates nor collects',
          mrWrap.querySelector('.mr-row').dataset.mrState === 'off' &&
          mrWrap.querySelector('.mr-row').dataset.mrOff === '1' &&
          mrWrap.querySelector('.mr-out').textContent === 'a' &&
          validations === 0 && marks === 0 && JSON.stringify(keyCalls) === '[]' && fullCalls === 0 &&
          w.settingsDirtyCount() === 1 && status() === '1 unsaved');
        mrWrap.querySelector('.mr-row [data-mr-vis]').click();
        checkClean('un-parking the rule restores the clean draft');
      } finally {
        w.validateModelRulesDraft = originalValidate;
        w.markSettingsDirty = originalMark;
      }
    }
    const roWrap = box.querySelector('[data-kind="request_overrides"]');
    resetCounters();
    roWrap.querySelector('[data-ro-add]').click();
    roWrap.querySelector('[data-ro-tpl="blank"]').click();
    check('adding a request-override rule collects only the request_overrides key',
      JSON.stringify(keyCalls) === JSON.stringify(['request_overrides']) && fullCalls === 0);
    checkDirty('adding a request-override rule counts one unsaved key', 1);
    resetCounters();
    [...roWrap.querySelectorAll('.ro-rule')].pop().querySelector('[data-ro-rm]').click();
    check('removing a request-override rule collects only the request_overrides key',
      JSON.stringify(keyCalls) === JSON.stringify(['request_overrides']) && fullCalls === 0);
    checkClean('removing the added request-override rule restores the clean draft');
    const scWrap = box.querySelector('[data-kind="sub_conversations"]');
    resetCounters();
    scWrap.querySelector('[data-sc-add]').click();
    check('adding a sub-conversation entry collects only the sub_conversations key',
      JSON.stringify(keyCalls) === JSON.stringify(['sub_conversations']) && fullCalls === 0);
    checkDirty('adding a sub-conversation entry counts one unsaved key', 1);
    resetCounters();
    [...scWrap.querySelectorAll('.sc-card')].pop().querySelector('[data-sc-rm]').click();
    check('removing a sub-conversation entry collects only the sub_conversations key',
      JSON.stringify(keyCalls) === JSON.stringify(['sub_conversations']) && fullCalls === 0);
    checkClean('removing the added sub-conversation entry restores the clean draft');
    // No resolvable key: the full collector still runs and catches the change.
    resetCounters();
    const victim = box.querySelector('input[data-key="bulk_3"]');
    victim.value = 'unnoticed change';
    const fakeRow = d.createElement('div');
    fakeRow.className = 'st-row';
    fakeRow.innerHTML = '<input class="no-key" value="x">';
    box.appendChild(fakeRow);
    fakeRow.querySelector('input').dispatchEvent(inputEvent());
    check('an input without a resolvable key falls back to the full collector',
      fullCalls === 1 && keyCalls.length === 0 &&
      w.settingsDirtyCount() === 1 && status() === '1 unsaved' && !apply.disabled);
    fakeRow.remove();
    edit(victim, '3');
    checkClean('the fallback dirty state clears through the incremental path after the revert');
    // A refill resets the cached per-key snapshots and the changed set.
    edit(bulk1, 'stale draft');
    checkDirty('a draft arms the dirty state before the refill', 1);
    w.eval('settingsDoc = window.__dirtyDoc; fillSettingsForm(settingsDoc)');
    checkClean('a settings refill resets the per-key snapshots and changed set');
    w.collectSettingsValue = originalKeyCollector;
    w.collectSettingsValues = originalFullCollector;
    // A save accepted mid-typing rebases the per-key baseline: only the keys
    // changed after the submission stay unsaved.
    {
      const originalFetch = w.fetch;
      const posts = [];
      w.fetch = (url, init) => {
        if (!String(url).includes('/admin/config')) return originalFetch(url, init);
        return new Promise(resolve => {
          if (init?.method === 'POST') posts.push({ resolve, body: JSON.parse(init.body) });
          else resolve({ ok: true, json: async () => JSON.parse(JSON.stringify(doc)) });
        });
      };
      edit(box.querySelector('input[data-key="bulk_1"]'), 'saved one');
      const save = w.applySettings();
      edit(box.querySelector('input[data-key="bulk_2"]'), 'typed during save');
      const submitted = JSON.parse(JSON.stringify(posts[0].body.values));
      posts.shift().resolve({ ok: true, statusText: 'OK', json: async () => ({ saved: true, revision: 'dirty-r2', values: submitted, effective: {}, restart_required: [] }) });
      await save;
      check('a save accepted mid-typing counts only the keys typed after submission',
        w.settingsDirtyCount() === 1 &&
        d.getElementById('settings-count').textContent === 'saved · 1 unsaved changes' &&
        w.eval('settingsDoc.revision') === 'dirty-r2' && !apply.disabled && !revert.disabled);
      w.fetch = originalFetch;
    }
    w.__settingsTestDoc = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsTestDoc; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__settingsTestDoc;
    delete w.__dirtyDoc;
  }

  // ---- test 11u: the model-rules preview caches its ring-derived traffic
  // examples per live data revision, so a preview pass never rescans the
  // 10k ring or re-sorts the model counts. A revision bump or a records
  // array replacement (a manual fixture swap without a bump) invalidates it.
  {
    const wrap = d.createElement('div');
    wrap.innerHTML = w.modelRulesEditorHTML([{ mode: 'lower' }]);
    w.__mrCacheWrap = wrap;
    let cache;
    try {
      cache = w.eval(`(() => {
        const box = window.__mrCacheWrap;
        const counts = [1500, 1400, 1300, 1200, 1100, 1000, 900, 800, 500, 300];
        const records = [];
        counts.forEach((n, i) => { for (let j = 0; j < n; j++) records.push({ model: 'm' + i }); });
        window.__mrCacheSaved = lastData;
        lastData = { _rev: 1, records };
        mrPreview(box);
        const first = mrTrafficExamples();
        const origSort = Array.prototype.sort;
        const origMapSet = Map.prototype.set;
        let sorts = 0, sets = 0;
        Array.prototype.sort = function(...args) { sorts++; return origSort.apply(this, args); };
        Map.prototype.set = function(...args) { sets++; return origMapSet.apply(this, args); };
        let second;
        try {
          mrPreview(box);
          second = mrTrafficExamples();
        } finally {
          Array.prototype.sort = origSort;
          Map.prototype.set = origMapSet;
        }
        const top = first.map(([name, n]) => name + ':' + n);
        const hit = second === first;
        lastData._rev += 1;
        const afterRevOnly = mrTrafficExamples();
        const revOnlyFresh = afterRevOnly !== first;
        const revOnlySame = JSON.stringify(afterRevOnly) === JSON.stringify(first);
        lastData.records.forEach(r => { if (r.model === 'm0') r.model = 'zeta'; });
        lastData._rev += 1;
        const afterRev = mrTrafficExamples();
        const replacement = [];
        for (let i = 0; i < 4000; i++) replacement.push({ model: 'epsilon' });
        for (let i = 0; i < 2000; i++) replacement.push({ model: 'phi' });
        lastData.records = replacement;
        const afterReplace = mrTrafficExamples();
        return {
          hit, sorts, sets, ring: records.length, top, revOnlyFresh, revOnlySame,
          revFresh: afterRev !== afterRevOnly,
          revTop: afterRev.map(([name, n]) => name + ':' + n),
          replaceFresh: afterReplace !== afterRev,
          replaceTop: afterReplace.map(([name, n]) => name + ':' + n),
        };
      })()`);
    } finally {
      w.eval('lastData = window.__mrCacheSaved; delete window.__mrCacheSaved;');
      delete w.__mrCacheWrap;
    }
    check('the 10k traffic-example scan still returns the top eight spellings in count order',
      cache.ring === 10000 &&
      cache.top.join(',') === 'm0:1500,m1:1400,m2:1300,m3:1200,m4:1100,m5:1000,m6:900,m7:800');
    check('a second preview pass reuses the cached traffic examples without rebuilding the count map or sorting',
      cache.hit && cache.sorts === 0 && cache.sets === 0);
    check('a live-revision bump invalidates the cached traffic examples',
      cache.revOnlyFresh && cache.revOnlySame && cache.revFresh && cache.revTop[0] === 'zeta:1500');
    check('replacing the records array without a revision bump invalidates the cached traffic examples',
      cache.replaceFresh && cache.replaceTop.join(',') === 'epsilon:4000,phi:2000');
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
    state.fullPayload = { feed_id: 'feedB', seq: 3,
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

  // ---- test 12b-bis: the unfiltered derivation aliases the ring instead
  // of copying it on every live revision, while filtered scopes still build
  // their own array and an abnormal nil-bearing ring still yields nil-free
  // ordered rows. The 10k case proves the common path makes zero copies.
  {
    check('unfiltered derive aliases the live ring without a copy',
      w.eval('derive(lastData).scopedRecs === lastData.records'));

    const fstatus = d.getElementById('f-status');
    fstatus.value = '500';
    fstatus.dispatchEvent(new w.Event('change'));
    await sleep(20);
    const filtered = w.eval('derive(lastData).scopedRecs');
    check('a filtered derive returns its own array with exactly the matching records',
      w.eval('derive(lastData).scopedRecs !== lastData.records') &&
      filtered.length === 1 && filtered[0].id === 'mix2' &&
      filtered.every(r => r.status_code === 500));
    fstatus.value = '';
    fstatus.dispatchEvent(new w.Event('change'));
    await sleep(20);

    // The incremental painter consumes the alias directly (it slices before
    // reversing); the unfiltered window keeps the ring's arrival order.
    w.eval('renderRequestsIncremental(derive(lastData).scopedRecs)');
    check('incremental rendering accepts the aliased ring and keeps row order',
      rows().map(tr => tr.dataset.id).join() === 'mix3,mix2,mix1');

    // A nil entry is abnormal but must never leak into the scoped rows; the
    // fallback copy preserves order. A local fixture keeps lastData intact.
    const nilRecs = [mkRec('nil0'), null, mkRec('nil1'), undefined, mkRec('nil2')];
    w.__aliasNilFixture = { records: nilRecs, counters: {}, _rev: 1 };
    try {
      const nilScoped = w.eval('derive(window.__aliasNilFixture).scopedRecs');
      check('a nil-bearing ring falls back to a nil-free ordered copy',
        nilScoped !== nilRecs && nilScoped.length === 3 &&
        nilScoped.map(r => r.id).join() === 'nil0,nil1,nil2');
    } finally { delete w.__aliasNilFixture; }

    // Identity at the 10k ring cap: no N-record copy and no push loop.
    const bigRecs = Array.from({ length: 10000 }, (_, i) => mkRec('big' + i));
    w.__aliasBigFixture = { records: bigRecs, counters: {}, _rev: 1 };
    try {
      check('the 10k unfiltered derivation is the ring array itself',
        w.eval('derive(window.__aliasBigFixture).scopedRecs === window.__aliasBigFixture.records') &&
        w.eval('derive(window.__aliasBigFixture).scopedRecs.length') === 10000 &&
        w.eval('derive(window.__aliasBigFixture).scopedRecs[9999] === window.__aliasBigFixture.records[9999]'));
    } finally { delete w.__aliasBigFixture; }
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

  // ---- operator poll stability: pause/debug/limits lists update in place.
  // Bootstrap polls include live occupancy (pause queued, debug ran_ms and
  // captures, limits in-flight/queued/remaining). Those are display-only
  // changes, so the renderers must keep the existing nodes, focus, checked
  // selections and scroll position instead of rebuilding each list. One
  // structural test below pins the exact replacement path too.
  {
    const debugFixture = (sessions, known = {}) => observerFixture({
      ok: true, enabled: sessions.length > 0, sessions,
      known_clients: known.clients || [], known_providers: known.providers || [],
      known_models: known.models || [], until: null, ttl: '24h', max_bytes: '1MiB',
      model_canon: modelFixture(),
    });
    const pauseFixture = (holds, queued, known = {}) => ({
      ok: true, paused: holds.length > 0, holds, queued,
      clients: [], providers: [],
      known_clients: known.clients || [], known_providers: known.providers || [],
      until: null, default_max_queued: 0,
    });
    const limitFixture = (counts, providers = ['provider-a']) => ({
      ok: true, active: true, known_providers: providers,
      throttles: [{
        provider: 'provider-a', source: 'ui', concurrency: 2,
        requests: 10, request_window: '1m', tokens: 0, token_window: '',
        in_flight: counts[0], queued: counts[1], requests_remaining: counts[2],
        requests_capacity: 20, tokens_remaining: 0, tokens_capacity: 0,
      }],
    });

    // Debug: an elapsed/capture-only poll must not replace the focused model
    // checkbox or the active-session row; the row text still advances.
    d.getElementById('debug-menu').hidden = false;
    w.applyDebugState(debugFixture(
      [{ id: 'flow-dbg', clients: ['client-a'], duration: '15m', ran_ms: 60000, captures: 2 }],
      { clients: ['client-a', 'client-b'], models: ['model-a', 'model-b'] }));
    const debugModel = d.querySelector('#df-models input[value="model-a"]');
    debugModel.focus();
    debugModel.checked = true;
    const debugHold = d.querySelector('#df-holds .pause-hold');
    const debugDur = d.getElementById('df-dur');
    const debugDurOption = debugDur.options[0];
    w.applyDebugState(debugFixture(
      [{ id: 'flow-dbg', clients: ['client-a'], duration: '15m', ran_ms: 180000, captures: 9 }],
      { clients: ['client-a', 'client-b'], models: ['model-a', 'model-b'] }));
    check('debug elapsed/capture polling keeps the focused model checkbox and session row in place',
      d.activeElement === debugModel && debugModel.isConnected && debugModel.checked &&
      d.querySelector('#df-models input[value="model-a"]') === debugModel &&
      d.querySelector('#df-holds .pause-hold') === debugHold &&
      debugHold.textContent.includes('ran 3m') && debugHold.textContent.includes('9 req'));
    check('debug duration options are not rebuilt by an elapsed-only poll',
      d.getElementById('df-dur') === debugDur && debugDur.options[0] === debugDurOption);

    // Expiry: a timed session expiring between polls changes the taken-name
    // vocabulary without changing any raw session field. Liveness therefore
    // belongs in the structural gate, not only in the row renderer.
    {
      const OriginalDate = w.Date;
      let fakeNow = OriginalDate.now();
      class FakeDate extends OriginalDate {
        constructor(...args) { super(...(args.length ? args : [fakeNow])); }
        static now() { return fakeNow; }
      }
      const until = new OriginalDate(fakeNow + 60000).toISOString();
      try {
        w.Date = FakeDate;
        w.applyDebugState(debugFixture(
          [{ id: 'expiry-dbg', models: ['model-a'], until, ran_ms: 1000, captures: 0 }],
          { models: ['model-a', 'model-b'] }));
        w.resetDebugMenuForm();
        const takenModel = d.querySelector('#df-models input[value="model-a"]');
        const keptModel = d.querySelector('#df-models input[value="model-b"]');
        check('a live exclusive debug model scope disables its model row',
          !!takenModel && takenModel.disabled && takenModel.closest('label').classList.contains('taken'));
        keptModel.focus();
        keptModel.checked = true;
        fakeNow += 120000;
        w.applyDebugState(debugFixture(
          [{ id: 'expiry-dbg', models: ['model-a'], until, ran_ms: 2000, captures: 3 }],
          { models: ['model-a', 'model-b'] }));
        check('an expired debug session runs the full sync and re-enables its exclusive model scope',
          d.querySelector('#df-models input[value="model-a"]') === takenModel &&
          !takenModel.disabled && !takenModel.closest('label').classList.contains('taken') &&
          d.querySelector('#df-models input[value="model-b"]') === keptModel &&
          d.activeElement === keptModel && keptModel.checked &&
          d.getElementById('df-holds').textContent === '');
      } finally {
        w.Date = OriginalDate;
        w.applyDebugState(debugFixture([], { models: ['model-a', 'model-b'] }));
      }
    }

    // Pause: queued occupancy must repaint the count and hold text without
    // replacing the focused client checkbox or the hold row.
    d.getElementById('pause-menu').hidden = false;
    w.applyPauseState(pauseFixture(
      [{ id: 'flow-hold', all: true, duration: '15m', queued: 1 }], 1,
      { clients: ['client-a', 'client-b'] }));
    const pauseClient = d.querySelector('#pf-clients input[value="client-a"]');
    pauseClient.focus();
    pauseClient.checked = true;
    const pauseHold = d.querySelector('#pf-holds .pause-hold');
    const pauseDur = d.getElementById('pf-dur');
    const pauseDurOption = pauseDur.options[0];
    w.applyPauseState(pauseFixture(
      [{ id: 'flow-hold', all: true, duration: '15m', queued: 6 }], 7,
      { clients: ['client-a', 'client-b'] }));
    check('pause occupancy polling keeps the focused checklist selection and hold row in place',
      d.activeElement === pauseClient && pauseClient.isConnected && pauseClient.checked &&
      d.querySelector('#pf-clients input[value="client-a"]') === pauseClient &&
      d.querySelector('#pf-holds .pause-hold') === pauseHold &&
      pauseHold.textContent.includes('q 6') &&
      d.getElementById('pause-count').textContent === 'paused · queued 7');
    check('pause duration options are not rebuilt by an occupancy-only poll',
      d.getElementById('pf-dur') === pauseDur && pauseDur.options[0] === pauseDurOption);

    // Limits: in-flight/queued/remaining are display-only. The provider
    // select and the focused hold row must survive the poll.
    d.getElementById('limits-menu').hidden = false;
    w.applyThrottleState(limitFixture([1, 0, 19]));
    const limitProvider = d.getElementById('lim-provider');
    const limitProviderOption = limitProvider.options[0];
    const limitHold = d.querySelector('#lim-holds .pause-hold');
    const limitButton = limitHold.querySelector('[data-operator="limit-edit"]');
    limitButton.focus();
    w.applyThrottleState(limitFixture([4, 3, 15]));
    check('limits occupancy polling keeps the provider select, focused hold row and live count in place',
      d.activeElement === limitButton && limitButton.isConnected &&
      d.getElementById('lim-provider') === limitProvider &&
      limitProvider.options[0] === limitProviderOption &&
      d.querySelector('#lim-holds .pause-hold') === limitHold &&
      d.getElementById('lim-count').textContent === 'set in UI · in-flight 4 · queued 3 · 15/20 req');

    // Opening each menu paints every checklist once: the reset owns the
    // values, the sync owns the single list paint.
    {
      w.hideHdrMenu('debug-menu');
      const paints = {};
      const originalFill = w.fillDebugChecks;
      w.fillDebugChecks = function (box, names, picked, kind) {
        paints[kind] = (paints[kind] || 0) + 1;
        return originalFill.apply(this, arguments);
      };
      try { w.toggleDebugMenu({ stopPropagation() {} }); }
      finally { w.fillDebugChecks = originalFill; }
      check('opening Debug paints each checklist once',
        paints.client === 1 && paints.provider === 1 && paints.model === 1);
      w.hideHdrMenu('debug-menu');

      const pausePaints = {};
      const originalRows = w.fillCheckRows;
      w.fillCheckRows = function (box) {
        if (box) pausePaints[box.id] = (pausePaints[box.id] || 0) + 1;
        return originalRows.apply(this, arguments);
      };
      try { w.togglePauseMenu({ stopPropagation() {} }); }
      finally { w.fillCheckRows = originalRows; }
      check('opening Pause paints each checklist once',
        pausePaints['pf-clients'] === 1 && pausePaints['pf-providers'] === 1);
      w.hideHdrMenu('pause-menu');
    }

    // Structural changes still render exact content: session/hold rows add
    // and remove, and vocabulary names enter and leave the checklist.
    {
      d.getElementById('debug-menu').hidden = false;
      w.applyDebugState(debugFixture(
        [{ id: 'structure-1', clients: ['client-a'], duration: '15m', ran_ms: 0, captures: 0 }],
        { clients: ['client-a'], models: ['model-a'] }));
      w.resetDebugMenuForm();
      const debugRow = () => [...d.querySelectorAll('#df-holds .pause-hold')]
        .map(row => row.querySelector('.hold-edit').textContent);
      check('a debug session renders its exact scope, duration and capture count',
        JSON.stringify(debugRow()) === JSON.stringify(['client-a · 15m · 0 req']) &&
        d.querySelector('#df-holds [data-operator="debug-stop"]').textContent === 'Stop');
      const oldClient = d.querySelector('#df-clients input[value="client-a"]');
      w.applyDebugState(debugFixture(
        [{ id: 'structure-2', providers: ['provider-a'], duration: '1h', ran_ms: 0, captures: 0 }],
        { clients: ['client-b'], models: ['model-b'] }));
      check('a replaced debug session and vocabulary renders the new exact rows and removes the old name',
        JSON.stringify(debugRow()) === JSON.stringify(['provider-a · 1h · 0 req']) &&
        !oldClient.isConnected && !d.querySelector('#df-clients input[value="client-a"]') &&
        d.querySelector('#df-clients label').textContent.trim() === 'client-b' &&
        d.querySelector('#df-models input[value="model-b"]').type === 'checkbox');
      w.applyDebugState(debugFixture([], { clients: ['client-b'], models: ['model-b'] }));
      check('stopping every debug session removes the row and its header',
        d.getElementById('df-holds').textContent === '' && !d.querySelector('#df-holds .pause-hold'));

      d.getElementById('pause-menu').hidden = false;
      w.applyPauseState(pauseFixture([], 0, { clients: ['client-a'] }));
      w.resetPauseMenuForm();
      check('no pause holds renders an empty hold container', d.getElementById('pf-holds').textContent === '');
      w.applyPauseState(pauseFixture(
        [{ id: 'structure-hold', all: true, duration: '15m', queued: 0 }], 0,
        { clients: ['client-a'] }));
      const pauseRow = () => [...d.querySelectorAll('#pf-holds .pause-hold')]
        .map(row => row.querySelector('.hold-edit').textContent);
      check('a pause hold renders its exact scope and duration with its Resume action',
        JSON.stringify(pauseRow()) === JSON.stringify(['all requests · 15m']) &&
        d.querySelector('#pf-holds [data-operator="pause-resume"]').textContent === 'Resume');
      w.applyPauseState(pauseFixture(
        [{ id: 'structure-hold', all: true, duration: '15m', queued: 0 },
          { id: 'structure-hold-2', clients: ['client-b'], duration: '1h', queued: 0 }], 0,
        { clients: ['client-a', 'client-b'] }));
      check('a second pause hold appends its exact row and vocabulary name',
        JSON.stringify(pauseRow()) === JSON.stringify(['all requests · 15m', 'client-b · 1h']) &&
        !!d.querySelector('#pf-clients input[value="client-b"]'));
      w.applyPauseState(pauseFixture([], 0, { clients: [] }));
      check('removing every pause hold clears the rows and known clients',
        d.getElementById('pf-holds').textContent === '' &&
        !d.querySelector('#pf-clients input[value="client-b"]'));

      d.getElementById('limits-menu').hidden = false;
      w.applyThrottleState({ ok: true, active: true, known_providers: ['provider-a'],
        throttles: [{ provider: 'provider-a', source: 'ui', concurrency: 2 }] });
      const limitRows = () => [...d.querySelectorAll('#lim-holds .pause-hold')]
        .map(row => row.querySelector('[data-operator="limit-edit"]').textContent);
      check('a limits row renders its exact cap and source',
        JSON.stringify(limitRows()) === JSON.stringify(['provider-a · ×2']) &&
        d.querySelector('#lim-holds .lim-src').textContent === 'ui');
      w.applyThrottleState({ ok: true, active: true, known_providers: ['provider-a', 'provider-b'],
        throttles: [{ provider: 'provider-a', source: 'ui', concurrency: 2 },
          { provider: 'provider-b', source: 'header', updated_by: 'edge' }] });
      check('a second limits row appends with its source badge and provider option',
        JSON.stringify(limitRows()) === JSON.stringify(['provider-a · ×2', 'provider-b · ']) &&
        JSON.stringify([...d.getElementById('lim-provider').options].map(o => o.value)) ===
          JSON.stringify(['provider-a', 'provider-b']) &&
        d.querySelectorAll('#lim-holds .lim-src')[1].textContent === 'header');
      w.applyThrottleState({ ok: true, active: false, known_providers: ['provider-a'], throttles: [] });
      check('removing every limits row clears the container', d.getElementById('lim-holds').textContent === '');

      w.applyPauseState({ ok: true, paused: false, holds: [], known_clients: [], known_providers: [] });
      w.resetPauseMenuForm();
      w.hideHdrMenu('pause-menu');
      w.applyDebugState(observerFixture({ ok: true, enabled: false, sessions: [],
        known_clients: [], known_providers: [], known_models: [],
        ttl: '24h', max_bytes: '1MiB', model_canon: modelFixture() }));
      w.resetDebugMenuForm();
      w.hideHdrMenu('debug-menu');
      w.applyThrottleState({ ok: true, active: false, throttles: [], known_providers: [] });
      w.syncLimitsMenuState();
      w.hideHdrMenu('limits-menu');
    }
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

  // ---- settings search haystack cache: fillSettingsForm stamps each rendered
  // row once with its lowercase label + key + help join, and showSettingsCat
  // then serves both row visibility and every rail badge from one row query
  // and one pass. A three-category fixture pins the cross-category badges,
  // the active/aria state, the singular/plural copy, the empty result, the
  // refill replacement and the back-to-category restore.
  {
    const doc = {
      revision: 'search-cache-r1',
      fields: [
        { key: 'alpha_rate', category: 'rate', label: 'Legacy alpha rate', help: 'old wording ceiling', kind: 'int', hot_reload: true },
        { key: 'beta_rate', category: 'rate', label: 'Beta rate', help: 'shared limit', kind: 'int', hot_reload: false },
        { key: 'gamma_window', category: 'window', label: 'Gamma window', help: 'ledger size', kind: 'int', hot_reload: true },
        { key: 'delta_window', category: 'window', label: 'Delta window', help: 'ledger cadence', kind: 'int', hot_reload: false },
        { key: 'epsilon_client', category: 'client', label: 'Epsilon client', help: 'client ledger label', kind: 'string', hot_reload: true },
        { key: 'zeta_client', category: 'client', label: 'Zeta client', help: 'unrelated note', kind: 'string', hot_reload: true },
      ],
      categories: [
        { id: 'rate', label: 'Rate', help: 'rate controls' },
        { id: 'window', label: 'Windows', help: 'window controls' },
        { id: 'client', label: 'Clients', help: 'client controls' },
      ],
      values: { alpha_rate: 1, beta_rate: 2, gamma_window: 3, delta_window: 4, epsilon_client: 'one', zeta_client: 'two' },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    w.__searchCacheDoc = doc;
    w.eval("settingsCat = 'rate'; settingsDoc = window.__searchCacheDoc; fillSettingsForm(settingsDoc)");
    d.getElementById('settings-sheet').hidden = false;
    const box = d.getElementById('settings-fields');
    const rail = d.getElementById('settings-rail');
    const q = d.getElementById('settings-q');
    const visibleKeys = () => [...box.querySelectorAll('.st-row')]
      .filter(r => !r.hidden).map(r => r.dataset.key);
    const badges = () => Object.fromEntries([...rail.querySelectorAll('.st-rail-item')]
      .map(b => [b.dataset.stCat, Number(b.querySelector('.rail-n').textContent)]));
    const railState = id => {
      const b = rail.querySelector(`[data-st-cat="${id}"]`);
      return { hit: b.classList.contains('has-hit'), miss: b.classList.contains('is-miss'),
        active: b.classList.contains('active'), current: b.getAttribute('aria-current') };
    };
    const cachedRows = [...box.querySelectorAll('.st-row')];
    check('every rendered settings row caches its exact lowercase haystack',
      cachedRows.length === 8 && cachedRows.every(row => row.dataset.search ===
        ((row.dataset.label || '') + ' ' + (row.dataset.key || '') + ' ' + (row.dataset.help || '')).toLowerCase()) &&
      box.querySelector('.st-row[data-key="gamma_window"]').dataset.search === 'gamma window gamma_window ledger size');
    // One hook around the fields container proves the keystroke path reads
    // the rendered rows once, not once per rail item.
    const nativeQSA = box.querySelectorAll.bind(box);
    let rowQueries = 0;
    box.querySelectorAll = sel => { if (sel === '.st-row') rowQueries++; return nativeQSA(sel); };
    q.value = '  LEDGER  ';
    w.filterSettings();
    delete box.querySelectorAll;
    check('one settings row query serves a keystroke regardless of rail length',
      rail.querySelectorAll('.st-rail-item').length === 3 && rowQueries === 1);
    check('a trimmed lowercase query shows every cross-category hit with exact copy',
      JSON.stringify(visibleKeys()) === JSON.stringify(['gamma_window', 'delta_window', 'epsilon_client']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '3 matches' &&
      d.querySelector('#settings-pane-hd p').textContent === 'ledger');
    check('one query pass sets every rail badge and its hit/miss class',
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 2, client: 1}) &&
      !railState('rate').hit && railState('rate').miss &&
      railState('window').hit && !railState('window').miss &&
      railState('client').hit && !railState('client').miss);
    check('a query clears the active rail item and aria-current',
      ['rate', 'window', 'client'].every(id => !railState(id).active && railState(id).current === 'false'));
    w.eval("settingsCat = 'window'; showSettingsCat()");
    check('switching category under a query keeps every hit and badge unchanged',
      JSON.stringify(visibleKeys()) === JSON.stringify(['gamma_window', 'delta_window', 'epsilon_client']) &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 2, client: 1}) &&
      d.querySelector('#settings-pane-hd h4').textContent === '3 matches' &&
      ['rate', 'window', 'client'].every(id => !railState(id).active && railState(id).current === 'false'));
    q.value = 'zeta';
    w.filterSettings();
    check('a one-row query uses the singular copy and one badge hit',
      JSON.stringify(visibleKeys()) === JSON.stringify(['zeta_client']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match' &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 0, client: 1}) &&
      railState('client').hit && railState('rate').miss && railState('window').miss);
    q.value = 'delta_window';
    w.filterSettings();
    check('a row key alone is part of the cached search haystack',
      JSON.stringify(visibleKeys()) === JSON.stringify(['delta_window']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match' &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 1, client: 0}));
    q.value = 'no-such-setting';
    w.filterSettings();
    check('an empty result hides every row and marks every rail item a miss',
      visibleKeys().length === 0 &&
      d.querySelector('#settings-pane-hd h4').textContent === '0 matches' &&
      JSON.stringify(badges()) === JSON.stringify({rate: 0, window: 0, client: 0}) &&
      ['rate', 'window', 'client'].every(id => railState(id).miss));
    w.eval("settingsCat = 'rate'");
    q.value = '';
    w.filterSettings();
    check('clearing the query restores the active category rows and full badges',
      JSON.stringify(visibleKeys()) === JSON.stringify(['alpha_rate', 'beta_rate']) &&
      JSON.stringify(badges()) === JSON.stringify({rate: 2, window: 2, client: 2}) &&
      d.querySelector('#settings-pane-hd h4').textContent === 'Rate' &&
      d.querySelector('#settings-pane-hd p').textContent === 'rate controls' &&
      railState('rate').active && railState('rate').current === 'page' &&
      ['rate', 'window', 'client'].every(id => !railState(id).hit && !railState(id).miss));
    q.value = 'legacy';
    w.filterSettings();
    check('the pre-refill label term is searchable', JSON.stringify(visibleKeys()) === JSON.stringify(['alpha_rate']));
    const next = JSON.parse(JSON.stringify(doc));
    next.revision = 'search-cache-r2';
    next.fields.find(f => f.key === 'alpha_rate').label = 'Current alpha rate';
    next.fields.find(f => f.key === 'alpha_rate').help = 'new wording ceiling';
    w.__searchCacheNext = next;
    w.eval('settingsDoc = window.__searchCacheNext; fillSettingsForm(settingsDoc)');
    check('a refill replaces the cached haystack instead of keeping stale text',
      box.querySelector('.st-row[data-key="alpha_rate"]').dataset.search === 'current alpha rate alpha_rate new wording ceiling');
    q.value = 'legacy';
    w.filterSettings();
    check('the old label stops matching after the refill', visibleKeys().length === 0);
    q.value = 'current';
    w.filterSettings();
    check('the refill label is the row text the search now sees',
      JSON.stringify(visibleKeys()) === JSON.stringify(['alpha_rate']) &&
      d.querySelector('#settings-pane-hd h4').textContent === '1 match');
    q.value = '';
    w.filterSettings();
    w.eval("settingsCat = 'providers'");
    w.__searchCacheRestore = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__searchCacheRestore; fillSettingsForm(settingsDoc)');
    w.closeSettings(true);
    delete w.__searchCacheDoc;
    delete w.__searchCacheNext;
    delete w.__searchCacheRestore;
  }

  // ---- settings open no-op: a clean, unchanged reopen still fetches for
  // freshness, but the accepted document must leave the retained form in
  // place - same nodes, same values, same baseline, no full fingerprint -
  // while the category/search presentation is re-applied. Revision or
  // writable changes, dirty drafts and Revert still refill.
  {
    const originalFetch = w.fetch;
    const pending = [];
    w.fetch = (url, init) => {
      if (!String(url).includes('/admin/config')) return originalFetch(url, init);
      return new Promise(resolve => pending.push(resolve));
    };
    const reply = doc => ({ ok: true, statusText: 'OK', json: async () => JSON.parse(JSON.stringify(doc)) });
    const doc = {
      revision: 'noop-r1',
      fields: [
        { key: 'noop_count', category: 'alpha', label: 'Noop count', help: 'alpha row', kind: 'int', min: 0, max: 10, hot_reload: true },
        { key: 'noop_flag', category: 'beta', label: 'Noop flag', help: 'beta row', kind: 'bool', hot_reload: true },
      ],
      categories: [
        { id: 'alpha', label: 'Alpha', help: 'alpha controls' },
        { id: 'beta', label: 'Beta', help: 'beta controls' },
      ],
      values: { noop_count: 2, noop_flag: true },
      defaults: {}, effective: {}, overrides: {}, writable: true, usage_fields: [],
    };
    const box = d.getElementById('settings-fields');
    const q = d.getElementById('settings-q');
    const apply = d.getElementById('btn-settings-apply');
    const revert = d.getElementById('btn-settings-revert');
    const status = () => d.getElementById('settings-count').textContent;
    let fillCalls = 0, fingerprintCalls = 0;
    const realFill = w.fillSettingsForm;
    const realFingerprint = w.settingsFingerprint;
    w.fillSettingsForm = function () { fillCalls++; return realFill.apply(this, arguments); };
    w.settingsFingerprint = function () { fingerprintCalls++; return realFingerprint.apply(this, arguments); };
    const valueInput = () => box.querySelector('input[data-key="noop_count"]');
    const openWith = async next => {
      w.toggleSettings({ stopPropagation() {} });
      await settleUntil(() => pending.length === 1);
      pending.shift()(reply(next));
      await sleep(20);
    };
    // A first open has no baseline and no rendered rows: it must fill.
    w.closeSettings(true);
    w.eval('settingsDoc = null; settingsSnap = ""; settingsSnapByKey = null; settingsChangedKeys = new Set(); settingsKeyEls = new Map(); settingsCat = ""; settingsQ = "";');
    box.innerHTML = '';
    d.getElementById('settings-rail').innerHTML = '';
    q.value = '';
    await openWith(doc);
    const firstInput = valueInput();
    check('settings first open still fills the accepted document',
      fillCalls === 1 && !!firstInput && firstInput.value === '2' &&
      w.eval('settingsDoc.revision') === 'noop-r1');
    // Leave the next open a search and a non-default active category to
    // restore. Closing keeps the populated DOM.
    w.eval("settingsCat = 'beta'");
    q.value = 'noop';
    w.filterSettings();
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(doc);
    check('a clean, unchanged reopen performs no fill and no full fingerprint collection',
      fillCalls === 0 && fingerprintCalls === 0 &&
      valueInput() === firstInput && firstInput.value === '2');
    check('the skipped reopen resets search and re-applies the active category',
      q.value === '' && w.eval('settingsQ') === '' &&
      !box.querySelector('.st-row[data-key="noop_flag"]').hidden &&
      box.querySelector('.st-row[data-key="noop_count"]').hidden &&
      d.querySelector('#settings-pane-hd h4').textContent === 'Beta' &&
      d.querySelector('[data-st-cat="beta"]').classList.contains('active'));
    check('the skipped reopen keeps a clean draft with both actions disabled',
      !w.settingsIsDirty() && apply.disabled && revert.disabled && status() === '');
    fingerprintCalls = 0;
    const cleanDirty = w.settingsIsDirty();
    w.updateSettingsActions();
    check('the clean dirty predicate and action refresh stay on the incremental baseline',
      !cleanDirty && fingerprintCalls === 0 && apply.disabled && revert.disabled);
    // An out-of-band revision change is fresh state and must refill.
    const revisionDoc = JSON.parse(JSON.stringify(doc));
    revisionDoc.revision = 'noop-r2';
    revisionDoc.values.noop_count = 7;
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(revisionDoc);
    check('an out-of-band revision change refills and reflects the new document',
      fillCalls === 1 && valueInput() !== firstInput && valueInput().value === '7' &&
      w.eval('settingsDoc.revision') === 'noop-r2');
    // A writable flip on the same revision is a different document too.
    const readOnlyDoc = JSON.parse(JSON.stringify(revisionDoc));
    readOnlyDoc.writable = false;
    readOnlyDoc.values.noop_count = 8;
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(readOnlyDoc);
    check('a writable flip refills and reflects the read-only document',
      fillCalls === 1 && valueInput().value === '8' &&
      status() === 'in-memory only - start with -config to persist' &&
      apply.disabled && revert.disabled);
    // A dirty draft always wins over a clean server refresh.
    const cleanDoc = JSON.parse(JSON.stringify(doc));
    cleanDoc.revision = 'noop-r3';
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(cleanDoc);
    const dirtyInput = valueInput();
    dirtyInput.value = '5';
    dirtyInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    check('the reopen fixture drafts an edit with both actions armed',
      dirtyInput.value === '5' && w.settingsIsDirty() && !apply.disabled && !revert.disabled);
    fingerprintCalls = 0;
    const draftDirty = w.settingsIsDirty();
    w.updateSettingsActions();
    check('the dirty predicate and action refresh stay on the incremental baseline',
      draftDirty && fingerprintCalls === 0 && !apply.disabled && !revert.disabled);
    w.closeSettings(true);
    fillCalls = 0;
    fingerprintCalls = 0;
    await openWith(cleanDoc);
    check('a clean refresh never replaces a dirty draft',
      fillCalls === 0 && fingerprintCalls === 0 && valueInput() === dirtyInput &&
      dirtyInput.value === '5' && w.settingsIsDirty() && !apply.disabled && !revert.disabled);
    // Revert discards only the draft it started with: typing while its GET
    // is in flight wins, a later quiet Revert refills from the server.
    fillCalls = 0;
    const reverting = w.revertSettings();
    await settleUntil(() => pending.length === 1);
    dirtyInput.value = '6';
    dirtyInput.dispatchEvent(new w.Event('input', { bubbles: true }));
    pending.shift()(reply(cleanDoc));
    await reverting;
    check('typing after Revert starts survives its response',
      valueInput() === dirtyInput && dirtyInput.value === '6' && w.settingsIsDirty() &&
      !apply.disabled && !revert.disabled);
    fillCalls = 0;
    const revertingClean = w.revertSettings();
    await settleUntil(() => pending.length === 1);
    check('Revert keeps both actions disabled while its request is in flight',
      apply.disabled && revert.disabled && revert.dataset.busy === '1');
    pending.shift()(reply(cleanDoc));
    await revertingClean;
    check('Revert discards its own draft and refills from the accepted document',
      fillCalls === 1 && valueInput() !== dirtyInput && valueInput().value === '2' &&
      !w.settingsIsDirty() && apply.disabled && revert.disabled);
    w.fillSettingsForm = realFill;
    w.settingsFingerprint = realFingerprint;
    w.fetch = originalFetch;
    w.__settingsNoopRestore = JSON.parse(JSON.stringify(cfgDoc));
    w.eval('settingsDoc = window.__settingsNoopRestore; fillSettingsForm(settingsDoc)');
    w.eval("settingsCat = 'providers'");
    w.closeSettings(true);
    delete w.__settingsNoopRestore;
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
    state.fullPayload = { feed_id: 'feedB', seq: 5,
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
    // Insert two records, then replay one - the replayed row must not rebuild
    // HTML. The cache stores record identity, not markup, so count reqRow calls.
    w.eval("window.__reqRowOrig = window.reqRow; window.__reqRowCalls = 0; window.reqRow = (...a) => { window.__reqRowCalls++; return window.__reqRowOrig(...a); };");
    fire('record', { ...mkRec('idx1', 200, 1700000400000) }); // replay
    await sleep(20);
    const replayCalls = w.eval('window.__reqRowCalls');
    check('row-identity cache skips reqRow for an unchanged record', replayCalls === 0);
    // A real update (pending → finalized) MUST rebuild
    w.eval('window.__reqRowCalls = 0');
    fire('begin', { record: { ...mkRec('idx4', 0, 1700000403000), attempts: [] }, in_flight: 1 });
    await sleep(20);
    const liveBefore = w.eval("logHTML.get('idx4')?.rec?.live");
    fire('end', { record: { ...mkRec('idx4', 200, 1700000403000), attempts: [] }, in_flight: 0 });
    await sleep(20);
    const liveAfter = w.eval("logHTML.get('idx4')?.rec?.live");
    check('row-identity cache rebuilds on pending→finalized transition',
      liveBefore === true && liveAfter === false && w.eval('window.__reqRowCalls') > 0);
    w.eval('window.reqRow = window.__reqRowOrig; delete window.__reqRowOrig; delete window.__reqRowCalls;');
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

    // A live-status explorer view must not schedule refetches while hidden.
    w.eval(`window.__origFetchExplorer = fetchExplorer;
      window.__origWantsLive = explorerWantsLiveStatus;
      fetchExplorer = () => { window.__xpRefreshCount = (window.__xpRefreshCount || 0) + 1; };
      explorerWantsLiveStatus = () => true;
      window.__xpRefreshCount = 0; _xpLiveTimer = null;`);
    Object.defineProperty(w.document, 'hidden', { value: true, configurable: true });
    w.eval('document.dispatchEvent(new Event("visibilitychange"))');
    w.eval('scheduleExplorerLiveRefresh()');
    await sleep(1100);
    check('hidden tab does not schedule a live-status explorer refresh',
      w.eval('_xpLiveTimer === null && window.__xpRefreshCount === 0'));
    check('live-status explorer cadence stays at or above one second',
      w.eval('EXPLORER_LIVE_REFRESH_MS >= 1000'));
    Object.defineProperty(w.document, 'hidden', { value: false, configurable: true });
    w.eval('d3.dispatchEvent(new Event("visibilitychange"))'.replace('d3', 'document'));
    await sleep(20);
    check('returning to view refreshes a live-status explorer immediately',
      w.eval('window.__xpRefreshCount === 1'));
    w.eval('scheduleExplorerLiveRefresh()');
    await sleep(1100);
    check('visible tab keeps the throttled live-status refresh',
      w.eval('window.__xpRefreshCount === 2 && _xpLiveTimer === null'));
    w.eval(`fetchExplorer = window.__origFetchExplorer;
      explorerWantsLiveStatus = window.__origWantsLive;
      delete window.__origFetchExplorer; delete window.__origWantsLive; delete window.__xpRefreshCount;`);
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
    const previousExplorer = w.eval('explorerAgg');
    w.fetchExplorer('invalidate');
    d.getElementById('f-status').value = 'streaming';
    w.doFilter();
    check('status selections immediately supersede old scans without a debounce delay', pending.length === 6 && pending[4].signal.aborted);
    check('live Requests status filters refresh explorer/footer regardless of breakdown dimension', w.explorerWantsLiveStatus());
    reply(pending[4], 999);
    await sleep(10);
    check('an old status response cannot paint under the new selection',
      w.eval('explorerAgg') === null && w.eval('explorerAgg') !== previousExplorer &&
      d.getElementById('f-scope').textContent.startsWith('… of '));
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

  // ---- test 12i-bis: a failed explorer scan for a new scope must not leave
  // the old scope's cards visible, and the dashboard tick must retry it.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const originalFetch = w.fetch;
    const gallery = d.getElementById('xp-gallery');
    const calls = [];
    let bFailures = 0;
    let bPending = 0;
    let failB = true;
    const ent = (name, n) => ({name, n, cost:0, in:10, out:5, cache:2, tools:0,
      err_final:0, rate_limit_requests:0, cost_per_mtok:null,
      ttft_p50:null, ttft_p95:null, tps_p50:null, tps_p95:null});
    const payload = (isB) => ({
      dim: 'provider', total: isB ? 20 : 10, error_total: 0,
      rail: {provider: isB ? 20 : 10},
      groups: [ent(isB ? 'b-node.example' : 'a-node.example', isB ? 20 : 10)],
      scope: {matches: isB ? 20 : 10, errors: 0},
    });
    w.fetch = (url, options) => {
      const u = String(url);
      if (!u.includes('/metrics/agg/explorer')) return originalFetch(url, options);
      const f = new URL(u, 'http://x').searchParams.get('f') || '';
      const isB = f === 'provider:scope-b.example';
      calls.push({f, isB});
      if (isB && failB) {
        bFailures++;
        bPending++;
        return Promise.resolve({ok: false, status: 503, json: async () => ({error: 'scan unavailable'})})
          .finally(() => { bPending--; });
      }
      return Promise.resolve({ok: true, json: async () => payload(isB)});
    };
    try {
      w.navigateTo([{dim: 'provider', id: 'scope-a.example'}]);
      await settleUntil(() => !!gallery.querySelector('[data-xp-key="a-node.example"]'));
      check('explorer success paints the requested scope cards',
        calls.some(c => !c.isB) && !!gallery.querySelector('[data-xp-key="a-node.example"]') &&
        !/breakdown unavailable/.test(gallery.textContent));

      w.navigateTo([{dim: 'provider', id: 'scope-b.example'}]);
      await settleUntil(() => bFailures === 1 && /breakdown unavailable/.test(gallery.textContent));
      check('a failed new-scope explorer fetch removes the old scope cards',
        !!gallery.querySelector('[data-xp-key="a-node.example"]') === false &&
        gallery.querySelectorAll('.xp-node').length === 0 &&
        /breakdown unavailable/.test(gallery.textContent));
      check('the failed scope shows an honest loading/failure state instead of old cards',
        gallery.querySelector('.empty') !== null && !/a-node\.example/.test(gallery.textContent));

      w.refreshExplorerIfStale();
      await settleUntil(() => bFailures === 2 && bPending === 0);
      check('the stale-refresh owner retries a failed scope even while live data is unchanged',
        calls.filter(c => c.isB).length >= 2 && calls[calls.length - 1].f === 'provider:scope-b.example');

      failB = false;
      w.refreshExplorerIfStale();
      await settleUntil(() => !!gallery.querySelector('[data-xp-key="b-node.example"]'));
      check('a restored explorer endpoint repaints scope B and clears the failure state',
        !!gallery.querySelector('[data-xp-key="b-node.example"]') &&
        !gallery.querySelector('[data-xp-key="a-node.example"]') &&
        !/breakdown unavailable/.test(gallery.textContent));
    } finally {
      failB = false;
      w.fetch = originalFetch;
      w.navigateTo([]);
      await sleep(20);
    }
  }

  // ---- test 12i-ter: a live-status explorer count refresh rebuilds the
  // dimension rail, but keyboard focus must stay on the same rail item.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    if (w.eval('_xpLiveTimer')) w.eval('clearTimeout(_xpLiveTimer); _xpLiveTimer = null;');
    const originalFetch = w.fetch;
    const originalTimeout = w.setTimeout;
    let railModel = 3;
    let liveRefresh = null;
    w.fetch = (url, options) => {
      const u = String(url);
      if (!u.includes('/metrics/agg/explorer')) return originalFetch(url, options);
      return Promise.resolve({ok: true, json: async () => ({
        dim: 'status', total: 30, error_total: 0, rail: {provider: 1, model: railModel},
        groups: [], scope: {matches: 30, errors: 0},
      })});
    };
    try {
      w.navigateTo([{dim: 'by', id: 'status'}]);
      await settleUntil(() => d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'status' &&
        w.eval('!!explorerAgg && explorerAgg.dim === "status"'));
      const rail = d.getElementById('xp-rail');
      const focused = rail.querySelector('.xp-rail-item[data-xp-dim="model"]');
      focused.focus();
      check('the explorer rail can take keyboard focus before its count changes',
        d.activeElement === focused && focused.querySelector('.rail-n').textContent === '3');

      railModel = 999;
      w.setTimeout = (fn, ms, ...args) => {
        if (ms === w.eval('EXPLORER_LIVE_REFRESH_MS')) {
          liveRefresh = () => fn(...args);
          return 4242;
        }
        return originalTimeout(fn, ms, ...args);
      };
      fire('begin', {record: {...mkRec('rail-focus1', 200, 1700000600000)}, in_flight: 1});
      w.setTimeout = originalTimeout;
      check('a live-status event schedules the explorer refresh owner', typeof liveRefresh === 'function');
      if (liveRefresh) liveRefresh();
      await settleUntil(() => d.querySelector('#xp-rail [data-xp-dim="model"] .rail-n')?.textContent === '999');
      const next = d.querySelector('#xp-rail [data-xp-dim="model"]');
      check('a rail count repaint keeps focus on the same dimension button',
        next !== focused && d.activeElement === next &&
        next.querySelector('.rail-n').textContent === '999');
    } finally {
      w.setTimeout = originalTimeout;
      w.fetch = originalFetch;
      try { fire('end', {record: mkRec('rail-focus1', 200, 1700000600000), in_flight: 0}); } catch (e) {}
      w.eval('if (_xpLiveTimer) { clearTimeout(_xpLiveTimer); _xpLiveTimer = null; }');
      w.navigateTo([]);
      await sleep(20);
    }
  }

  // ---- test 12i-quater: #/request/<id> is inert. The old request leaf was
  // removed from EXPLORER_DIMS, so the route must not become a bogus filter
  // and must never throw. Normal filter, status and breakdown routes still
  // parse through the same hash owner.
  {
    let probe = null, error = null;
    try {
      probe = w.eval(`(() => {
        const savedStatus = filters.status;
        history.replaceState(null, '', '#/request/req-123');
        const request = explorerState();
        const requestSegments = parseHash();
        const requestScope = scopeQS();
        history.replaceState(null, '', '#/provider/a.example/by/model');
        const normal = explorerState();
        filters.status = '500';
        const scope = scopeQS();
        filters.status = savedStatus;
        history.replaceState(null, '', location.pathname + location.search);
        return {
          requestSegments: requestSegments.length,
          requestFilters: request.filters.length,
          requestDim: request.activeDim,
          requestScope,
          normalFilters: normal.filters,
          normalDim: normal.activeDim,
          scope,
        };
      })()`);
    } catch (e) { error = e; }
    check('#/request/<id> is ignored without a bogus filter or an exception',
      error === null && !!probe && probe.requestSegments === 0 &&
      probe.requestFilters === 0 && probe.requestDim === 'provider' && probe.requestScope === '');
    check('normal explorer filter, status and by routes still parse',
      !!probe && probe.normalFilters.length === 1 &&
      probe.normalFilters[0].dim === 'provider' && probe.normalFilters[0].id === 'a.example' &&
      probe.normalDim === 'model' && probe.scope.includes('s=500') &&
      probe.scope.includes('f=provider%3Aa.example'));
  }

  // ---- test 12j: a rail click changes only the breakdown dimension (view
  // state, by/<dim>), not the log/chart scope. A by-only hash change must
  // repaint the explorer and fetch just the new breakdown; the full render
  // path (which resets the paged log window and rebuilds the chart/drawer)
  // stays on real filter or status changes.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;'); // a tick must not mask the route
    const navBase = 1700001000000;
    state.fullPayload = { feed_id: 'feedB', seq: 200, incremental: false,
      records: [
        { ...mkRec('nav-a1', 200, navBase + 1000), provider: 'nav-a.example', __seq: 1 },
        { ...mkRec('nav-a2', 200, navBase + 2000), provider: 'nav-a.example', __seq: 2 },
        { ...mkRec('nav-b1', 500, navBase + 3000), provider: 'nav-b.example', __seq: 3 },
      ],
      counters: { in_flight: 0, total_requests: 3, total_errors: 1 } };
    w.eval("fetchBootstrap('full')");
    await settleUntil(() => rows().length === 3);

    // Establish a filtered scope with one durable archive page below the ring,
    // then give the log window a non-default limit and scroll position.
    state.logPage = { records: [{ ...mkRec('nav-a-old', 200, navBase - 1000), provider: 'nav-a.example' }],
      more: false, cursor_ms: navBase - 1000, cursor_id: 'nav-a-old' };
    Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 200; } });
    w.navigateTo([{ dim: 'provider', id: 'nav-a.example' }]);
    await settleUntil(() => w.eval('logArchive.length') === 1 && rows().length === 3 &&
      !d.getElementById('f-scope').textContent.startsWith('…'));
    Object.defineProperty(state.box, 'scrollHeight', { configurable: true, get() { return 4000; } });
    state.box.scrollTop = 137;
    w.eval('logLimit = 2');
    w.openDrawer('nav-a1');
    await sleep(20);

    const filtersBefore = w.eval('explorerFilters()');
    const derFBefore = w.eval('_derFS');
    const chartBefore = w.eval('chartAgg');
    const archiveBefore = w.eval('logArchive');
    const limitBefore = w.eval('logLimit');
    const htmlBefore = d.getElementById('tbl-requests').innerHTML;
    const drawerBefore = d.getElementById('drawer-body').innerHTML;
    const footerBefore = d.getElementById('f-scope').textContent;
    const railBefore = d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim;
    const priorXp = new URL(state.initialFetches.filter(u => u.includes('/metrics/agg/explorer')).pop() || '',
      'http://x').searchParams;
    check('the canonical filter key is order-independent and value-sensitive',
      typeof w.explorerFilterKey === 'function' &&
      w.explorerFilterKey([{ dim: 'provider', id: 'a' }, { dim: 'model', id: 'm' }]) ===
        w.explorerFilterKey([{ dim: 'model', id: 'm' }, { dim: 'provider', id: 'a' }]) &&
      w.explorerFilterKey([{ dim: 'provider', id: 'a' }]) !==
        w.explorerFilterKey([{ dim: 'provider', id: 'b' }]));

    const orig = {
      renderAll: w.renderAll, fetchChart: w.fetchChart, renderChart: w.renderChart,
      resetLogWindow: w.resetLogWindow, renderDrawer: w.renderDrawer,
      renderExplorer: w.renderExplorer, fetchExplorer: w.fetchExplorer, fetch: w.fetch,
    };
    const calls = { renderAll: 0, fetchChart: 0, renderChart: 0, resetLogWindow: 0,
      renderDrawer: 0, renderExplorer: 0, fetchExplorer: 0 };
    const explorerURLs = [];
    w.renderAll = (...a) => { calls.renderAll++; return orig.renderAll(...a); };
    w.fetchChart = (...a) => { calls.fetchChart++; return orig.fetchChart(...a); };
    w.renderChart = (...a) => { calls.renderChart++; return orig.renderChart(...a); };
    w.resetLogWindow = (...a) => { calls.resetLogWindow++; return orig.resetLogWindow(...a); };
    w.renderDrawer = (...a) => { calls.renderDrawer++; return orig.renderDrawer(...a); };
    w.renderExplorer = (...a) => { calls.renderExplorer++; return orig.renderExplorer(...a); };
    w.fetchExplorer = (...a) => { calls.fetchExplorer++; return orig.fetchExplorer(...a); };
    w.fetch = (url, opts) => {
      if (String(url).includes('/metrics/agg/explorer')) explorerURLs.push(String(url));
      return orig.fetch(url, opts);
    };
    const fstatus = d.getElementById('f-status');
    try {
      // Real navigateTo path: same filter set + status, new breakdown only.
      w.navigateTo([{ dim: 'provider', id: 'nav-a.example' }, { dim: 'by', id: 'model' }]);
      await settleUntil(() => calls.renderExplorer > 0 && calls.fetchExplorer > 0 &&
        explorerURLs.length > 0 && d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'model');

      check('by-only navigation does not run the full render path',
        calls.renderAll === 0 && calls.fetchChart === 0 && calls.renderChart === 0 &&
        calls.resetLogWindow === 0);
      check('by-only navigation leaves the log window, archive, limit and scroll intact',
        d.getElementById('tbl-requests').innerHTML === htmlBefore &&
        w.eval('logArchive') === archiveBefore && w.eval('logLimit') === limitBefore &&
        state.box.scrollTop === 137);
      check('by-only navigation leaves the open drawer untouched',
        calls.renderDrawer === 0 && d.getElementById('drawer-body').innerHTML === drawerBefore);
      check('by-only navigation repaints the rail and fetches only the new breakdown',
        calls.renderExplorer > 0 && calls.fetchExplorer === 1 && railBefore === 'provider' &&
        d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'model' &&
        d.querySelector('#xp-rail .xp-rail-item[data-xp-dim="model"]')?.getAttribute('aria-pressed') === 'true');
      const xpURL = new URL(explorerURLs[explorerURLs.length - 1], 'http://x').searchParams;
      check('the new breakdown request keeps the same filter/status scope',
        xpURL.get('dim') === 'model' && xpURL.get('f') === 'provider:nav-a.example' &&
        (xpURL.get('s') || '') === '' && xpURL.get('f') === priorXp.get('f') &&
        (xpURL.get('s') || '') === (priorXp.get('s') || ''));
      check('by-only navigation keeps the cached filter-set identity (derive stays valid)',
        w.eval('explorerFilters()') === filtersBefore && w.eval('_derFS') === derFBefore &&
        w.eval('chartAgg') === chartBefore && d.getElementById('f-scope').textContent === footerBefore);

      // Filter removal: drop the provider filter, keep the breakdown.
      const beforeRemove = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      w.navigateTo([{ dim: 'by', id: 'model' }]);
      await settleUntil(() => calls.renderAll === beforeRemove.renderAll + 1);
      check('a real filter removal still runs the full path and resets the paged log',
        calls.renderAll === beforeRemove.renderAll + 1 &&
        calls.fetchChart === beforeRemove.fetchChart + 1 &&
        calls.resetLogWindow === beforeRemove.resetLogWindow + 1 &&
        w.eval('logArchive.length') === 0 && w.eval('logLimit') === 0);

      // Filter addition: scope to the other provider, same breakdown.
      const beforeAdd = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      w.navigateTo([{ dim: 'provider', id: 'nav-b.example' }, { dim: 'by', id: 'model' }]);
      await settleUntil(() => calls.renderAll === beforeAdd.renderAll + 1);
      check('a real filter addition still runs the full path and resets the log',
        calls.renderAll === beforeAdd.renderAll + 1 &&
        calls.fetchChart === beforeAdd.fetchChart + 1 &&
        calls.resetLogWindow === beforeAdd.resetLogWindow + 1);

      // Status changes are independent of the hash and still invalidate the
      // memoized filter set and reset the log window through doFilter.
      const statusFiltersBefore = w.eval('explorerFilters()');
      const beforeStatus = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      fstatus.value = '500';
      fstatus.dispatchEvent(new w.Event('change'));
      await sleep(20);
      check('a status change still runs the full path and resets the log',
        calls.renderAll === beforeStatus.renderAll + 1 &&
        calls.fetchChart === beforeStatus.fetchChart + 1 &&
        calls.resetLogWindow === beforeStatus.resetLogWindow + 1 &&
        w.eval('logArchive.length') === 0);
      check('a status change invalidates the memoized filter-set identity',
        w.eval('explorerFilters()') !== statusFiltersBefore);

      // The status filter lives outside the hash but is part of the full
      // scope. After doFilter's full render establishes the new scope, a
      // later by-only change must still stay on the explorer-only path.
      const beforePostStatus = { renderAll: calls.renderAll, fetchChart: calls.fetchChart,
        resetLogWindow: calls.resetLogWindow };
      w.navigateTo([{ dim: 'provider', id: 'nav-b.example' }, { dim: 'by', id: 'client' }]);
      await settleUntil(() => calls.renderExplorer > 0 &&
        d.querySelector('#xp-rail .xp-rail-item.active')?.dataset.xpDim === 'client');
      check('by-only navigation after a status change keeps the explorer-only path',
        calls.renderAll === beforePostStatus.renderAll &&
        calls.fetchChart === beforePostStatus.fetchChart &&
        calls.resetLogWindow === beforePostStatus.resetLogWindow);
    } finally {
      w.renderAll = orig.renderAll;
      w.fetchChart = orig.fetchChart;
      w.renderChart = orig.renderChart;
      w.resetLogWindow = orig.resetLogWindow;
      w.renderDrawer = orig.renderDrawer;
      w.renderExplorer = orig.renderExplorer;
      w.fetchExplorer = orig.fetchExplorer;
      w.fetch = orig.fetch;
      fstatus.value = '';
      fstatus.dispatchEvent(new w.Event('change'));
      w.closeDrawer();
    }
    w.navigateTo([]);
    await sleep(20);
    check('the by-only suite restores the unfiltered log window',
      w.eval('logArchive.length') === 0 && rows().length === 3);
  }

  // ---- test 12k: a render between a hash assignment and its queued
  // hashchange must not claim the pending scope. Otherwise onNavigate sees
  // the new scope as already rendered, skips the full path, and never fetches
  // the chart/explorer for the new filter set.
  {
    w.eval('clearInterval(_dashTickTimer); _dashTickTimer = null;');
    const scopeBefore = w.eval('_navScopeKey');
    try {
      w.eval("location.hash = '#/provider/pending.example'");
      w.eval('renderAll(lastData)');
      check('a render before the queued hashchange leaves the pending scope unclaimed',
        w.eval('_navScopeKey') === scopeBefore);
      await sleep(30);
      check('the queued hashchange still routes the new scope',
        w.eval('_navScopeKey') !== scopeBefore &&
        w.eval('_routedHash') === w.eval('location.hash'));
    } finally {
      w.navigateTo([]);
      await sleep(20);
    }
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
      state.jsdomReloads === 0 && calls === 1 && w.eval('kpiAgg.requests') === 37);

    // An unexpected full response to a resume request is re-captured behind
    // the full/SSE barrier. The accepted full carries its own state/version.
    setResponse({ ...payload(TEST_DASHBOARD_VERSION), feed_id: 'same-assets-new-process', incremental: false });
    calls = 0;
    w.fetchBootstrap('resume');
    await sleep(20);
    check('same-assets restart re-captures an unexpected full response without a page reload', state.jsdomReloads === 0 && calls === 2);

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
    check('missing or malformed asset versions never create a reload loop', state.jsdomReloads === 0);

    const pending = [];
    w.fetch = () => new Promise(resolve => pending.push(resolve));
    w.fetchBootstrap('resume');
    w.fetchBootstrap('resume');
    pending[1]({ok: true, json: async () => payload(TEST_DASHBOARD_VERSION, 39)});
    await sleep(10);
    pending[0]({ok: true, json: async () => payload(newVersion, 999)});
    await sleep(10);
    check('a superseded bootstrap cannot reload the page using an outdated version',
      state.jsdomReloads === 0 && w.eval('kpiAgg.requests') === 39);

    let afterRan = false;
    setResponse(payload(newVersion, 999));
    w.fetchBootstrap('resume', () => { afterRan = true; });
    await sleep(20);
    check('changed frontend requests one reload before applying state or boot callbacks',
      state.jsdomReloads === 1 && !afterRan && w.eval('kpiAgg.requests') === 39);
    sweeps = 0;
    const callsBeforeReload = calls;
    w.refreshAfterRestart();
    w.fetchBootstrap('resume');
    w.fetchBootstrap('full');
    await sleep(10);
    check('reload in progress suppresses duplicate navigation and bootstrap work',
      state.jsdomReloads === 1 && calls === callsBeforeReload && sweeps === 0);
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
      await loaded(seeded);
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

  // The Restart control's eligibility rides the payload the page ALREADY
  // loads: a fresh page must decide before the menu is ever opened, without a
  // second round trip. A -trimpath image (no source tree, no go toolchain)
  // therefore never paints the control at all, while a build that reports
  // nothing keeps today's behaviour.
  {
    const base = {
      dashboard_version: TEST_DASHBOARD_VERSION, feed_id: 'restart-seed', seq: 1, pending_revision: 0,
      incremental: false, records: [mkRec('restart-seed')], in_flight_records: [],
      counters: {in_flight: 0, total_requests: 1, total_errors: 0},
      kpi: {requests: 1, errors: 0}, dash: {}, model_canon: {rules: []},
    };
    const trimpath = 'built with -trimpath, so the binary cannot locate its own source; ' +
      'self-restart is unavailable (restart via your process manager)';
    for (const [label, restart] of [
      ['ineligible', {available: false, reason: trimpath}],
      ['eligible', {available: true, reason: ''}],
      ['silent', undefined],
    ]) {
      let bootstrapFetches = 0;
      const seed = restart ? {...base, restart} : base;
      const isolated = dashboardDOM(assembleHTML(seed), {...pageOptions,
        beforeParse(win) {
          pageOptions.beforeParse(win);
          const fallback = win.fetch;
          win.fetch = (url, options) => {
            if (String(url).includes('/metrics/bootstrap')) bootstrapFetches++;
            return fallback(url, options);
          };
          win.EventSource = function() {
            return {readyState: 1, addEventListener() {}, close() {}};
          };
        },
      });
      await loaded(isolated);
      await sleep(100);
      const wrap = isolated.window.document.getElementById('restart-wrap');
      check(`${label} seed decides the Restart control from the payload it already loads`,
        bootstrapFetches === 0 && !!wrap && wrap.hidden === !!(restart && restart.available === false));
      if (label === 'ineligible') {
        check('the ineligible verdict also keeps the action itself disabled',
          isolated.window.document.getElementById('btn-restart-now').disabled);
      }
      isolated.window.close();
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
    await loaded(isolated);
    const sw = isolated.window;
    const stateRows = () => [...sw.document.querySelectorAll('#tbl-requests tr.exp-row[data-id]')];
    try {
      await settleUntil(() => stateRows().length === 17);
      check('operator-free HTML seed applies dashboard config on its first paint', stateRows().length === 17);
      sw.fetchBootstrap('resume');
      await settleUntil(() => sw.eval('dashCfg.log_rows') === 10 && stateRows().length === 10);
      check('dash-only incremental bootstrap refreshes the log without operator snapshots',
        sw.eval('dashCfg.log_rows') === 10 && stateRows().length === 10);
      sw.navigateTo([{dim: 'model', id: 'canonical-model'}]);
      await settleUntil(() => stateRows().length === 1);
      check('model-only bootstrap fixture starts with one in-scope row', stateRows().length === 1);
      payload = {...payload, model_canon: {rules: [{mode: 'exact', from: 'raw-model', to: 'canonical-model'}]}};
      sw.fetchBootstrap('resume');
      await settleUntil(() => stateRows().length === 10 && stateRows().some(row => row.dataset.id !== 'state0'));
      check('model-only incremental bootstrap refreshes scoped rows without operator snapshots',
        stateRows().length === 10 && stateRows().some(row => row.dataset.id !== 'state0'));
      payload = {...payload, dash: {dash_log_rows: 20}};
      sw.fetchBootstrap('none');
      await settleUntil(() => stateRows().length === 17);
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
    await loaded(isolated);
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
    await loaded(isolated);
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

  // Delegated operator actions repaint in place, so the clicked node stays
  // connected through the action itself. Outside-click dismissal still uses
  // the original event path rather than live target ancestry, for both edits
  // and pending/confirmed mutations.
  for (const [kind, action] of [['pause','pause-edit'], ['pause','pause-resume'], ['debug','debug-edit'], ['debug','debug-stop'], ['limits','limit-edit']]) {
    const isolated = dashboardDOM(assembleHTML(), pageOptions);
    await loaded(isolated);
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
      check(action + ' keeps its clicked row in place and stays open through its own delegated click',
        button.isConnected && originalPathInside && !menu.hidden);
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
    await loaded(isolated);
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
      {
        // Theme tray structure: one current/preview comparator, the surface
        // radios in the readable caption bar, and honest Reset/Close actions.
        const menu = sd.getElementById('theme-menu');
        const panes = menu.querySelector('.theme-panes');
        const paneList = [...menu.querySelectorAll('.theme-pane')];
        const captionBar = menu.querySelector('.theme-caption-bar');
        check('theme comparator is one aria-hidden pair of shared panes',
          !!panes && panes.getAttribute('aria-hidden') === 'true' && paneList.length === 2 &&
          paneList[0].id === 'theme-current' && paneList[1].id === 'theme-preview' &&
          paneList.every(pane => pane.classList.contains('theme-pane') &&
            pane.querySelector('.tp-label')));
        check('theme panes carry one neutral series legend, not status pills',
          paneList.every(pane =>
            pane.querySelectorAll('.tp-legend .tp-key').length === 3 &&
            !pane.querySelector('.tp-pill') &&
            pane.querySelector('.tp-legend').textContent === '' &&
            !/200|live|429/.test(pane.textContent)));
        check('theme pane markup is identical apart from id and labels', (() => {
          const shape = pane => {
            const clone = pane.cloneNode(true);
            clone.removeAttribute('id');
            clone.removeAttribute('data-theme');
            clone.removeAttribute('data-mode');
            clone.querySelector('.tp-label').textContent = '';
            return clone.innerHTML;
          };
          return shape(paneList[0]) === shape(paneList[1]);
        })());
        check('theme captions stay outside the decorative comparator',
          !!captionBar && !panes.contains(captionBar) &&
          captionBar.querySelector('#theme-current-label') &&
          captionBar.querySelector('#theme-preview-label') &&
          !captionBar.querySelector('#theme-preview-label').classList.contains('has-preview'));
        check('theme surface radios sit in the caption bar under a visible label',
          captionBar.querySelector('#theme-mode[role="radiogroup"]') &&
          captionBar.querySelectorAll('.theme-mode-opt').length === 2 &&
          captionBar.querySelector('.theme-mode-label')?.textContent === 'Surface' &&
          sd.getElementById('theme-mode').getAttribute('aria-labelledby') === 'theme-mode-label');
        check('theme tray lists all eight palettes and offers Reset and Close',
          menu.querySelectorAll('.theme-opt').length === 8 &&
          sd.getElementById('theme-reset')?.textContent === 'Reset' &&
          sd.getElementById('theme-reset')?.title === 'Restore Instrument Black' &&
          sd.getElementById('theme-close')?.textContent === 'Close' &&
          !sd.getElementById('theme-done'));
        check('theme hint explains preview, pointer activation and Escape',
          menu.querySelector('.theme-hintbar')?.textContent ===
            'Arrows preview. Click or press Enter to apply. Esc closes.');
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
    await loaded(isolated);
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
    await loaded(isolated);
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
