// test 11b: Cost preset renderer - cost is the lone bar on the arcsinh
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function chartRender(H) {
  const {BUCKET_MS, CHART_FROM, check, d, mkChart, settleUntil, w} = H;
  const state = H.state;

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
    check('setChartWindow applies the fetched window payload',
      await settleUntil(() => w.eval('chartAgg && chartAgg.now_ms') === state.chartPayload.now_ms));
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
    check('the window dropdown wiring applies the fetched window payload',
      await settleUntil(() => w.eval('chartAgg && chartAgg.now_ms') === state.chartPayload.now_ms));
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
};
