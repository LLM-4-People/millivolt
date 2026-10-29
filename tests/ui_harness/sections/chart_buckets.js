// test 11: chart bucket math (pure pipeline; renderChart stays a jsdom
// Extracted from the inline suite without changing order, labels or state.
module.exports = async function chartBuckets(H) {
  const {BUCKET_MS, CHART_FROM, check, fire, mkChart, mkRec, sleep, w} = H;
  const state = H.state;

  // ---- test 11: chart bucket math (pure pipeline; renderChart stays a jsdom
  // no-op behind the canvas size guard, so the data layer is driven directly) ----
  
  // Money is displayed in cents below $1 on every surface. Unit selection
  // uses the unrounded magnitude; stored values and aggregate math stay USD.
  const moneyCases = [
    [0, '0¢'], [0.5, '50¢'], [0.01, '1¢'], [0.0012, '0.12¢'],
    [0.00001, '0.001¢'], [0.99999, '99.999¢'], [1, '$1'], [3.5, '$3.5'],
    [999, '$999'], [999.4, '$999.4'], [999499, '$999.5K'], [999500, '$999.5K'],
    [999999, '$1M'], [1000000, '$1M'], [1000500, '$1M'],
    [1234, '$1.23K'], [2500000, '$2.5M'], [-0.25, '-25¢'], [-2, '-$2'], [-999999, '-$1M'],
  ];
  for (const [value, want] of moneyCases) {
    check(`money ${value} displays as ${want}`, w.eval(`fmtMoney(${value})`) === want);
  }
  check('compact counts keep the shared 1000K to 1M rollover',
    w.eval('fmt(999999) === "1M" && fmt(999500) === "999.5K" && fmt(999) === "999"'));
  check('unknown money stays unknown', w.eval('fmtMoney(null) === "-" && fmtMoney(NaN) === "-" && fmtMoney(Infinity) === "-"'));
  // Byte sizes walk one decimal KB/MB/GB ladder; exact counts stay below 1 KB
  // and the exact integer remains the copy payload.
  const byteCases = [
    [0, '0 B'], [999, '999 B'], [1000, '1 KB'], [1234, '1.23 KB'],
    [5678, '5.68 KB'], [999999, '1 MB'], [1500000, '1.5 MB'], [1e12, '1 TB'],
  ];
  check('byte sizes use the shared decimal KB/MB/GB ladder',
    byteCases.every(([value, want]) => w.eval(`fmtBytes(${value})`) === want));
  check('unknown byte sizes stay unknown',
    w.eval('fmtBytes(null) === "-" && fmtBytes(NaN) === "-" && fmtBytes(Infinity) === "-"'));
  check('request log uses cents', w.reqRow({ ...mkRec('cents'), cost: 0.25 }).includes('25¢'));
  check('absent ttft is a placeholder, not 0ms', (() => {
    const row = w.reqRow({ ...mkRec('no-ttft'), ttft_ms: 0 });
    const detail = w.formatDetail({ ...mkRec('no-ttft'), ttft_ms: 0 });
    return w.eval('fmtTTFT(0) === "-" && fmtTTFT(null) === "-" && fmtTTFT(100) === "100ms"') &&
      /<td title="100ms">100ms<\/td>/.test(w.reqRow(mkRec('has-ttft'))) &&
      /<td title="-">-<\/td>/.test(row) &&
      !/<td[^>]*>0ms<\/td>/.test(row) &&
      detail.includes('<span class="k">ttft</span><span class="v">-</span>') &&
      !detail.includes('>0ms<');
  })());
  // The drawer's Performance section renders the analyzer's per-stream
  // telemetry beside the timing rows: chunks as a plain integer, first
  // thinking through the shared timestamp formatter (detailStampRow output is
  // locale-dependent, so only presence and a rendered value are pinned),
  // and neither row renders when the record carries no value.
  check('the drawer renders chunks and first thinking beside the timing rows', (() => {
    const streamed = w.formatDetail({ ...mkRec('perf-stream'), chunks: 42, first_reasoning_at: 1700000005000 });
    const plain = w.formatDetail({ ...mkRec('perf-plain') });
    return streamed.includes('<span class="k">chunks</span><span class="v">42</span>') &&
      /<span class="k">first thinking<\/span><span class="v">[^<]+<\/span>/.test(streamed) &&
      !plain.includes('<span class="k">chunks</span>') &&
      !plain.includes('<span class="k">first thinking</span>');
  })());
  // The drawer's wire-size pair renders through fmtBytes (the human-readable
  // size keeps the exact integer as its hover title and copy payload), and the
  // transport's auto-decompress fact renders beside the response headers - the
  // one place it can show, since the transport strips Content-Encoding and
  // Content-Length when it decodes gzip. None of the rows renders when the
  // record carries no value.
  check('the drawer renders request/response bytes and the upstream gzip fact', (() => {
    const wire = w.formatDetail({ ...mkRec('wire'), request_bytes: 1234, response_bytes: 5678, upstream_gzip: true });
    const plain = w.formatDetail({ ...mkRec('wire-plain') });
    const bytesRow = (key, exact, shown) => new RegExp(
      `<span class="k">${key}</span><span class="v"><span class="vt" title="${exact} bytes">${shown}</span></span>`).test(wire);
    return bytesRow('request bytes', '1,234', '1.23 KB') &&
      bytesRow('response bytes', '5,678', '5.68 KB') &&
      wire.includes('<span class="k">upstream gzip</span><span class="v">true</span>') &&
      wire.includes('<h4>Response headers</h4>') &&
      !plain.includes('<span class="k">request bytes</span>') &&
      !plain.includes('<span class="k">response bytes</span>') &&
      !plain.includes('<span class="k">upstream gzip</span>') &&
      !plain.includes('<h4>Response headers</h4>');
  })());
  // The drawer's Performance section renders the adopted attempt's httptrace
  // decomposition beside the timing rows: plain integer milliseconds (the
  // processing-row style, "N ms"), a 0/absent hook value hides its row (0
  // means "not observed"), and the pool-hit fact renders through detailBoolRow so
  // an explicit false still shows, exactly like client disconnected.
  check('the drawer renders the upstream attempt timing decomposition', (() => {
    const trace = w.formatDetail({ ...mkRec('h2trace'), upstream_ttfb_ms: 12, upstream_connect_ms: 3, upstream_conn_reused: true });
    const tls = w.formatDetail({ ...mkRec('h2tls'), upstream_tls_ms: 7 });
    const plain = w.formatDetail({ ...mkRec('h2plain') });
    return trace.includes('<span class="k">upstream ttfb</span><span class="v">12 ms</span>') &&
      trace.includes('<span class="k">upstream connect</span><span class="v">3 ms</span>') &&
      trace.includes('<span class="k">reused connection</span><span class="v">true</span>') &&
      !trace.includes('<span class="k">upstream tls</span>') &&
      tls.includes('<span class="k">upstream tls</span><span class="v">7 ms</span>') &&
      !plain.includes('<span class="k">upstream ttfb</span>') &&
      !plain.includes('<span class="k">upstream connect</span>') &&
      !plain.includes('<span class="k">upstream tls</span>') &&
      !plain.includes('<span class="k">reused connection</span>');
  })());
  // detailBoolRow's documented contract: a stored false is a rendered "false" row,
  // never an absent one - pinned for both flags that ride it (the transport
  // pool-hit fact, and the wire-compression fact inside the header section,
  // which needs captured headers to render at all).
  check('stored-false transport flags render explicit false rows', (() => {
    const off = w.formatDetail({ ...mkRec('h2false'), upstream_conn_reused: false });
    const gzoff = w.formatDetail({ ...mkRec('h2gzoff'), upstream_gzip: false,
      response_headers: { 'content-type': ['application/json'] } });
    return off.includes('<span class="k">reused connection</span><span class="v">false</span>') &&
      gzoff.includes('<span class="k">upstream gzip</span><span class="v">false</span>');
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

  // Speed + latency: one selected percentile per metric (speed, TTFT and
  // the decode window). A bucket plots only when EVERY measurement exists at
  // that percentile (a null in any line is a gap point, not a zero);
  // unmeasured buckets compact away like empty bar slots instead of
  // stranding gap points in dead space.
  w.eval(`
    chartView.preset = 'latency'; chartView.pct = 95;
    chartAgg.buckets[2].req = 1;
    chartAgg.buckets[2].ttft = [10, 20, 30]; chartAgg.buckets[2].tps = [100.25, 200.75, 300.5]; chartAgg.buckets[2].dec = [40, 80, 120];
    chartAgg.buckets[7].req = 1;
    chartAgg.buckets[7].ttft = [1, 2, 3];
    chartAgg.buckets[9].req = 1;
    chartAgg.buckets[9].ttft = [null, 5, null]; chartAgg.buckets[9].tps = [null, 50, null]; chartAgg.buckets[9].dec = [null, 60, null];
  `);
  const dataLat = w.eval('chartData()');
  check('speed + latency keeps only buckets with every measurement',
    dataLat[0].length === 2 && dataLat[0][0] === 0.5 && dataLat[0][1] === 1.5);
  check('compacted points keep their true bucket times for ticks and hover', w.eval(`(() => {
    const ticks = chartXValues({data: [chartData()[0]]}, [0.5, 1.5]);
    return (_vis && _vis.join() === '2,9') &&
      ticks.join() === [chartXTick(chartAgg.buckets[2].t), chartXTick(chartAgg.buckets[9].t)].join();
  })()`));
  check('speed + latency reads only the selected percentile, preserving fractional speed',
    dataLat.length === 4 && dataLat[1][0] === 200.75 && dataLat[2][0] === 20 && dataLat[3][0] === 80 && dataLat[1][1] === 50 && dataLat[2][1] === 5 && dataLat[3][1] === 60);
  check('speed + latency uses independent unit-honest axes without bands', w.eval(`(() => {
    const opts = upOpts(640, 210);
    return opts.axes[1].scale === 'ytps' && opts.axes[2].scale === 'yttft' &&
      opts.series.length === 4 && opts.series[1].scale === 'ytps' && opts.series[2].scale === 'yttft' && opts.series[3].scale === 'yttft' &&
      opts.series[1].label === 'speed' && opts.series[2].label === 'latency' && opts.series[3].label === 'decode' && !opts.bands;
  })()`));
  check('a bucket missing one measurement never plots, even with that line hidden', w.eval(`(() => {
    chartView.hidden = {latency:['tps']};
    const d = chartData();
    chartView.hidden = {};
    return d[0].length === 2 && d[1].every(v => v === null) && d[2][0] === 20 && d[2][1] === 5 && d[3][0] === 80 && d[3][1] === 60;
  })()`));
  const isolatedOpts = w.eval('upOpts(370, 210)');
  const [innerPoint, outerPoint, outerPoint2] = isolatedOpts.series.slice(1).map(s => s.points); state.innerPoint = innerPoint;
  check('coincident isolated lines use an inner filled point and transparent outer rings',
    innerPoint.width === 0 && innerPoint.fill === isolatedOpts.series[1].stroke &&
    outerPoint.fill === 'transparent' && outerPoint.width > 0 &&
    outerPoint.size / 2 - outerPoint.width > innerPoint.size / 2 &&
    outerPoint2.fill === 'transparent' && outerPoint2.width > 0 && outerPoint2.size > outerPoint.size);
  check('isolated marker identity is stable when an earlier line is hidden', w.eval(`(() => {
    const hidden = chartView.hidden;
    const before = upOpts(370, 210).series.slice(1).map(s => s.points);
    chartView.hidden = {latency:['tps']};
    const after = upOpts(370, 210).series.slice(1).map(s => s.points);
    chartView.hidden = hidden;
    return before.every((p, i) => p.size === after[i].size && p.width === after[i].width && p.fill === after[i].fill);
  })()`));
  for (const si of [1, 2, 3]) {
    const marks = isolatedOpts.series[si].points.filter?.({ data: [[0], [9.928], [1], [3.5]] }, si);
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

  // All three period totals follow the one percentile control, not bucket
  // means and not a second always-p50 baseline. Names never repeat the
  // dropdown.
  w.eval(`
    chartView.preset = 'latency';
    chartAgg.ttft_p = [110, 220, 330]; chartAgg.tps_p = [101.25, 202.75, 303.5]; chartAgg.dec_p = [440, 550, 660];
  `);
  const latTotals = w.eval('chartTotals()');
  const tps202Full = w.eval('fmtTPS(202.75)');
  check('speed + latency totals show only selected period values',
    latTotals.includes(tps202Full) && latTotals.includes('220ms') && latTotals.includes('550ms') &&
    !latTotals.includes('101.25') && !latTotals.includes('110ms') && !latTotals.includes('440ms') && !/p(?:50|95|99)/.test(latTotals));
  for (const [pct, speed, ttft, dec, totalSpeed, totalTTFT, totalDec, kept] of [[50, 100.25, 10, 40, 101.25, 110, 440, 1], [99, 300.5, 30, 120, 303.5, 330, 660, 1]]) {
    w.setChartPct(String(pct));
    const data = w.eval('chartData()'), totals = w.eval('chartTotals()');
    const totalSpeedText = w.eval('fmtTPS(' + totalSpeed + ')');
    check(`percentile dropdown chooses every series and period total at ${pct}`,
      data.length === 4 && data[0].length === kept && data[1][0] === speed && data[2][0] === ttft && data[3][0] === dec &&
      totals.includes(totalSpeedText) && totals.includes(totalTTFT + 'ms') && totals.includes(totalDec + 'ms'));
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
  // Negative proof: the two live deliveries must NOT add a chart bucket. The
  // fixed window is the bound; there is no "nothing happened" state to poll.
  await sleep(20);
  check('live end/record delivery never adds to canonical chart buckets', w.eval('chartAgg.buckets[4].req') === 0);
  w.eval('chartAgg = null; chartView.preset = "traffic"; chartView.hidden = {}');
};
