// ---------- charts ----------
// The traffic chart is one multi-series time chart rendered by uPlot (MIT -
// vendored at vendor/uplot.js). The preset dropdown picks a curated view; the
// time window and percentile stay as controls. Buckets are
// server-computed since inception over the same scope as the request log
// (/metrics/agg/chart) with clock-aligned widths - the payload carries the
// exact integer bucket_ms and each bucket's start edge t.
// Rendering follows the aggregated-bucket convention: per-bucket sums
// (requests/errors/tokens/cost) render as bars (discrete sums, not smooth
// data), speed and latency render as lines at the selected percentile,
// and the error rate renders as a % line on
// a pinned 0–100 right axis - never a % series on a count scale.
// CHART_WINDOWS / CHART_PCTS are the single owners of the chart window and
// percentile selects (values are strings matching the <select> values;
// minutes, with all = first in-scope stored request→now). HTML options are filled from
// these lists - never a second copy.
const CHART_WINDOWS = [
  ['all', 'All time'],
  ['15', '15 minutes'],
  ['60', '1 hour'],
  ['360', '6 hours'],
  ['1440', '24 hours'],
  ['10080', '7 days'],
  ['43200', '30 days'],
  ['129600', '90 days'],
  ['525600', '1 year'],
];
const CHART_PCTS = [50, 95, 99];
// Internal chart geometry: minimum readable tick spacing and restrained bars.
const CHART_X_TICK_PX = 72;
const CHART_Y_TICK_PX = 28;
const CHART_BAR_MAX_PX = 18;
const CHART_GROUP_WIDTH = 0.72;
const CHART_GROUP_GAP = 0.08;
const CHART_AXIS_SIZE = 48;
const CHART_POINT_SIZE = 4; // isolated line samples need a visible mark
const CHART_POINT_RING_WIDTH = 1;
// Reuse locale formatters; cursor movement must not create one per readout.
const CHART_DATE_FORMAT = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' });
const CHART_FULL_DATE_FORMAT = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
const CHART_DAY_MS = 86400000;

// hexA blends a #rrggbb CSS var into an rgba() string (bar fills).
function hexA(hex, a) {
  const n = parseInt(hex.slice(1), 16);
  return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
}
// fmtPct keeps % axis ticks and legend values human (≤1 decimal, no float spew).
const fmtPct = v => (Math.round(v * 10) / 10) + '%';
const fmtTPS = v => v == null || !Number.isFinite(v) ? '-' : fmt(v) + ' tok/s';

// hm is the single HH:MM local-time label (x ticks + hover chip).
const hm = t => {
  const d = new Date(t);
  return String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
};
// chartXTick labels a bottom-axis time tick. uPlot's default formatter reads
// timestamps as seconds (ms values print as the year - 58598), so the x axis
// owns its labels: day granularity across multiple days, and full years
// whenever the viewed history crosses a calendar year.
function chartSpansYears() {
  return !!chartAgg && new Date(chartAgg.from_ms).getFullYear() !== new Date(chartAgg.now_ms).getFullYear();
}
function chartXTick(t) {
  const span = chartAgg ? (chartAgg.now_ms - chartAgg.from_ms) : 0;
  // Alignment can extend a 24h range by one partial bucket; that is still
  // a clock-time view. Multi-day sub-day buckets keep both date and time.
  if (span > CHART_DAY_MS + (chartAgg?.bucket_ms || 0)) {
    const d = new Date(t);
    const date = (d.getMonth() + 1) + '/' + d.getDate() + (chartSpansYears() ? '/' + d.getFullYear() : '');
    return date + (chartAgg.bucket_ms < CHART_DAY_MS ? ' ' + hm(t) : '');
  }
  return hm(t);
}

// A multi-day bucket must name both dates; clock-only end labels would make
// a seven-day bucket look like zero elapsed time. Cross-year ranges include
// years on both ends, using the same range formatter for every preset.
function chartBucketRange(t0, t1) {
  const start = new Date(t0), end = new Date(t1);
  const formatter = chartSpansYears() || start.getFullYear() !== end.getFullYear()
    ? CHART_FULL_DATE_FORMAT : CHART_DATE_FORMAT;
  const endDate = start.toDateString() !== end.toDateString() ? formatter.format(end) + ' · ' : '';
  return formatter.format(start) + ' · ' + hm(t0) + '–' + endDate + hm(t1);
}

// asinhTickLadder returns round decade values (1…1e15) for the arcsinh bars
// scale. uPlot treats axis splits as scale (data-space) values - the asinh
// compression happens inside valToPos - so the ticks are plain decades and
// the labels format them directly. The ladder starts at 1 for unit-ful data
// (counts/money ≥ 1 have no sub-1 ticks); for a sub-1 max it starts at
// 10^(floor(log10 tmax) − 3) - three decades below the max's own decade,
// which is up to four decades under the max itself. chartYTicks then keeps
// only the measurements with enough screen space for their labels.
function asinhTickLadder(tmax) {
  const out = [0];
  if (!Number.isFinite(tmax) || tmax <= 0) return out;
  let v = tmax >= 1 ? 1 : Math.pow(10, Math.floor(Math.log10(tmax)) - 3);
  for (; v <= tmax && v <= 1e15; v *= 10) out.push(v);
  return out;
}

// Series registry: the single owner of every chart series' id, label, palette
// color and formatter, plus its default renderer (`bar: true` = per-bucket sum
// drawn as a bar on the shared arcsinh scale; everything else is a line - a
// preset row may override it, see chartRowBar). Percentile entries own their
// wire metric. The dropdown picks the same
// percentile for every metric; names never repeat that selection.
const CHART_SERIES = [
  { id: 'req',     label: 'requests',   color: 'accent',  fmt: fmt,      bar: true },
  { id: 'err',     label: 'errors',     color: 'err',     fmt: fmt,      bar: true },
  { id: 'inTok',   label: 'tokens in',  color: 'accent2', fmt: fmt,      bar: true },
  { id: 'outTok',  label: 'tokens out', color: 'ok',      fmt: fmt,      bar: true },
  { id: 'reason',  label: 'reasoning',  color: 'cyan',    fmt: fmt,      bar: true },
  { id: 'cost',    label: 'cost',       color: 'warn',    fmt: fmtMoney, bar: true },
  { id: 'cache',   label: 'cached',     color: 'muted',   fmt: fmt,      dash: [3, 3] },
  { id: 'errRate', label: 'error rate', color: 'warn',    fmt: fmtPct },
  { id: 'tps',  metric: 'tps',      label: 'speed',   color: 'ok',     fmt: fmtTPS,
    title: 'Output tokens per second over wall time; decode-window speed when overall throughput is unavailable.' },
  { id: 'ttft', metric: 'ttft', label: 'latency', color: 'accent', fmt: fmtDur,
    title: 'Time to first token, including reasoning or answer content.' },
];

// Preset registry: the curated combinations the dropdown offers. `series` is
// the ordered [id, y-scale, renderer?] plot list - every entry renders per
// its own registry flag by default (bar sums share the grouped arcsinh scale;
// lines carry their per-unit scales), so a preset may mix renderers; a
// third element ('bar' | 'line') overrides the registry renderer for that row
// alone (Cost plots requests as a line on its right axis - a count
// accompanying money, never a second sum stacking the bars scale).
const CHART_PRESETS = [
  {
    id: 'traffic', label: 'Traffic',
    left: { scale: 'y', fmt: fmt, label: 'Requests' }, right: null,
    series: [['req', 'y'], ['err', 'y']],
  },
  {
    id: 'tokens', label: 'Tokens',
    left: { scale: 'y', fmt: fmt, label: 'Tokens' }, right: null,
    series: [['inTok', 'y'], ['outTok', 'y'], ['reason', 'y'], ['cache', 'y']],
  },
  {
    id: 'latency', label: 'Speed + latency',
    left: { scale: 'ytps', fmt: fmt, label: 'Speed (tok/s)' }, right: { scale: 'yttft', fmt: fmtDur, label: 'Latency (TTFT)' },
    series: [['tps', 'ytps'], ['ttft', 'yttft']],
  },
  {
    id: 'errors', label: 'Errors',
    left: { scale: 'y', fmt: fmt, label: 'Errors' }, right: { scale: 'pct', fmt: fmtPct, label: 'Error rate' },
    series: [['err', 'y'], ['errRate', 'pct']],
  },
  {
    id: 'cost', label: 'Cost',
    left: { scale: 'y', fmt: fmtMoney, label: 'Spend (USD)' }, right: { scale: 'yr', fmt: fmt, label: 'Requests' },
    series: [['cost', 'y'], ['req', 'yr', 'line']],
  },
];
const activePreset = () => CHART_PRESETS.find(pr => pr.id === chartView.preset) || CHART_PRESETS[0];
function chartSpec(id) {
  return CHART_SERIES.find(s => s.id === id);
}
// chartRowBar resolves a preset row's renderer: an optional third tuple
// element ('bar' | 'line') overrides the registry flag for that row alone -
// CHART_SERIES stays the default owner and the override narrows per preset.
const chartRowBar = (id, rend) => (rend ? rend === 'bar' : !!chartSpec(id).bar);

// Hidden series are per-preset: { presetId: [series ids] }.
// An old flat array (previous shape) is discarded - fresh default.
let chartView = { window: CHART_WINDOWS[0][0], pct: 95, preset: 'traffic', hidden: {} };
// loadChartView applies the persisted dash.chart payload onto chartView.
// Deny by default: each field lands only when its saved shape validates
// (window/pct/preset against their owner lists, hidden per-preset), so a
// legacy or garbage payload can never smuggle in an unknown value.
function loadChartView() {
  try {
    const saved = JSON.parse(storage.get('dash.chart') || '{}');
    if (CHART_WINDOWS.some(([v]) => v === String(saved.window))) chartView.window = String(saved.window);
    const p = Number(saved.pct);
    if (CHART_PCTS.includes(p)) chartView.pct = p;
    if (CHART_PRESETS.some(pr => pr.id === saved.preset)) chartView.preset = saved.preset;
    if (saved.hidden && !Array.isArray(saved.hidden)) {
      for (const pr of CHART_PRESETS) {
        const ids = Array.isArray(saved.hidden[pr.id])
          ? saved.hidden[pr.id].filter(id => pr.series.some(([seriesId]) => seriesId === id))
          : [];
        if (ids.length) chartView.hidden[pr.id] = ids;
      }
    }
  } catch (e) {}
}
loadChartView();

// Only the server folds chart metrics. SSE rows and durable snapshots have
// independent delivery boundaries, so adding them would double-count or lose
// completions. The configured chart refresh cadence owns freshness.
let _up = null, _upKey = '';

// pctIdx maps the percentile select onto the server's [p50, p95, p99] triple.
const pctIdx = () => CHART_PCTS.indexOf(chartView.pct);

// One owner of the server bucket's units and selected percentile.
function chartBucketVal(s, b) {
  switch (s.id) {
    case 'req': return b.req;
    case 'err': return b.err;
    case 'inTok': return b.in;
    case 'outTok': return b.out;
    case 'cache': return b.cache;
    case 'reason': return b.reason;
    case 'cost': return b.cost;
    case 'errRate': return b.req ? (b.err / b.req) * 100 : null;
    default: return s.metric ? (b[s.metric]?.[pctIdx()] ?? null) : null;
  }
}

// chartPlan computes the active preset's full render plan: uPlot series
// metadata in plot order, the legend rows, and the hidden flags. Every
// preset row carries its resolved renderer (registry flag or per-row
// override → m.bar); grouped-bar sizing runs over the visible bar series
// only, so hiding one re-centers the rest. Hidden series keep their column
// (all-null) - a toggle must be a pure setData with a stable column count
// or the plot keeps its stale frame.
let _plan = null;
function chartPlan() {
  const preset = activePreset();
  const hiddenId = id => (chartView.hidden[preset.id] || []).includes(id);
  const rows = preset.series;
  const nBars = rows.filter(([id, , rend]) => chartRowBar(id, rend) && !hiddenId(id)).length;
  let barK = 0;
  const meta = rows.map(([id, scale, rend]) => {
    const s = chartSpec(id);
    const m = { spec: s, scale, bar: chartRowBar(id, rend), hidden: hiddenId(id) };
    // barK counts visible bar rows only - a hidden bar draws nothing and
    // must not consume a slot, or the surviving bars drift off-center.
    if (m.bar && !m.hidden) {
      m.align = 1;
      m.size = (CHART_GROUP_WIDTH - CHART_GROUP_GAP * (nBars - 1)) / nBars;
      m.offset = -CHART_GROUP_WIDTH / 2 + barK * (m.size + CHART_GROUP_GAP);
      barK++;
    }
    return m;
  });
  return { preset, meta };
}

// chartData builds uPlot's [x, series…] column arrays for the active preset.
// Hidden series pass all-null columns (stable column count - a toggle is a
// plain setData; the plot re-creates only on preset changes).
// x values are bucket-slot centers. On the bars scale the empty buckets are
// dropped and x compacts to slot centers in index space (an empty slot is an
// invisible hole that only wastes width - each surviving bar keeps its true
// time for ticks/hover via _vis); line presets keep every bucket, where a
// gap is honest data.
let _vis = null, _compacted = false;
function chartData() {
  _plan = chartPlan();
  if (!chartAgg || !chartAgg.buckets || !chartAgg.buckets.length) return null;
  const bm = chartAgg.bucket_ms || 0;
  let idxs = null;
  if (_plan.preset.left.scale === 'y' && chartAgg.buckets.some(b => b.req === 0)) {
    idxs = [];
    chartAgg.buckets.forEach((b, i) => { if (b.req > 0) idxs.push(i); });
    if (!idxs.length) return null; // everything empty → blank state
  }
  _vis = idxs;
  _compacted = !!idxs;
  const src = idxs || chartAgg.buckets.map((_, i) => i);
  const data = [src.map((pi, k) => (idxs ? k + 0.5 : chartAgg.buckets[pi].t + bm / 2))];
  for (const m of _plan.meta) {
    data.push(src.map(pi => (m.hidden ? null : chartBucketVal(m.spec, chartAgg.buckets[pi]))));
  }
  return data;
}

// chartIsEmpty reports whether the server chart carries zero traffic - an empty scope shows the blank message
// instead of a misleading all-zero plot.
function chartIsEmpty() {
  if (!chartAgg || !chartAgg.buckets || !chartAgg.buckets.length) return true;
  let req = 0;
  for (const b of chartAgg.buckets) req += b.req;
  return req === 0;
}

// Pick bucket centers using the actual plot width. Both dense timelines and
// compacted intervals share these callbacks, including after a window change.
function compactXTicks(n, width) {
  const out = [];
  const count = Math.max(1, Math.floor(width / CHART_X_TICK_PX));
  const step = Math.max(1, Math.ceil(n / count));
  for (let k = 0; k < n; k += step) out.push(k + 0.5);
  return out;
}

function chartXSplits(u) {
  if (!chartAgg || !u.data[0].length) return [];
  const width = u.bbox.width / (window.devicePixelRatio || 1);
  return compactXTicks(u.data[0].length, width).map(v => u.data[0][Math.floor(v)]);
}

function chartXValues(u, splits) {
  if (!chartAgg) return splits.map(() => '');
  return splits.map(v => {
    const i = _compacted ? _vis?.[Math.round(v - 0.5)] : null;
    return chartXTick(i == null ? v : chartAgg.buckets[i].t);
  });
}

function chartXRange(u, min, max) {
  const xs = u.data[0];
  if (!chartAgg || !xs.length) return [0, 1];
  const half = (_compacted ? 1 : chartAgg.bucket_ms) / 2;
  // uPlot pre-expands equal min/max before calling range. For a single
  // timestamp that expansion spans years; use the actual bucket edges.
  return [xs[0] - half, xs[xs.length - 1] + half];
}

function chartBarRange(u, min, max) {
  if (!(max > 0)) return [0, 1];
  const step = Math.pow(10, Math.floor(Math.log10(max)));
  return [0, Math.ceil(max / step) * step];
}

// Thin in screen space, where asinh decades can pile up near zero. Keep a
// useful upper measurement for sub-dollar ranges, even when the last decade
// occupies only a few pixels. The same filter removes duplicate money labels.
function chartYTicks(u, ai, min, max) {
  const axis = _plan.preset.left;
  const ticks = asinhTickLadder(max);
  if (max > 0 && ticks[ticks.length - 1] < max / 2) ticks.push(max);
  const kept = [];
  let lastPos = null, lastLabel = null;
  for (const v of ticks) {
    const pos = u.valToPos(v, axis.scale);
    const label = axis.fmt(v);
    if (lastPos != null && (Math.abs(pos - lastPos) < CHART_Y_TICK_PX || label === lastLabel)) continue;
    kept.push(v);
    lastPos = pos;
    lastLabel = label;
  }
  return kept;
}

// Resolve geometry on every draw, so setData after a legend toggle moves the
// remaining bars. Cap the whole group proportionally, keeping sparse groups
// centered. disp.x0 is the left edge; align alone cannot separate three bars.
function chartBarGeometry(u, si) {
  const m = _plan.meta[si - 1];
  if (m.hidden) return { offset: 0, size: 0 };
  const step = _compacted ? 1 : chartAgg.bucket_ms;
  const slotPx = Math.abs(u.valToPos(step, 'x') - u.valToPos(0, 'x'));
  const factor = Math.min(1, CHART_BAR_MAX_PX / (slotPx * m.size));
  return { offset: m.offset * step * factor, size: m.size * step * factor };
}

function chartBarPaths() {
  return uPlot.paths.bars({
    size: [1, Infinity, 0],
    radius: 0.12,
    disp: {
      x0: { unit: 1, values: (u, si) => {
        const g = chartBarGeometry(u, si);
        return u.data[0].map(x => x + g.offset);
      } },
      size: { unit: 1, values: (u, si) => [chartBarGeometry(u, si).size] },
    },
  });
}

// A connected segment already draws a line. Mark only isolated samples,
// including zeros and the ends of sparse series, without bridging gaps.
// uPlot calls this against current setData columns; hidden series are null.
function chartIsolatedPoints(u, si) {
  const values = u.data[si];
  const indices = [];
  for (let i = 0; i < values.length; i++) {
    if (Number.isFinite(values[i]) && !Number.isFinite(values[i - 1]) && !Number.isFinite(values[i + 1])) indices.push(i);
  }
  return indices.length ? indices : null;
}
// Both numeric axes share formatting and uniqueness. Compact unit formatters
// may round adjacent splits alike; suppress duplicates, never relabel a tick.
function chartAxisValues(axis, splits) {
  const seen = new Set();
  return splits.map(value => {
    if (!Number.isFinite(value) || value < 0) return null;
    const label = axis.fmt(value);
    if (seen.has(label)) return null;
    seen.add(label);
    return label;
  });
}

function upOpts(w, h) {
  const grid = { stroke: COLORS.grid, width: 1 };
  // uPaint paints axis tick labels from axis.stroke - that must be a readable
  // text color (--muted, ≥5.2:1 on the case background), never --border2
  // (a ~1.5:1 dark gray that renders the measurements invisible).
  const axBase = {
    stroke: COLORS.muted,
    grid,
    ticks: { show: false },
    font: '11px ui-monospace',
    labelFont: '11px ui-monospace',
    size: CHART_AXIS_SIZE,
    space: CHART_Y_TICK_PX,
    gap: 8,
  };
  const preset = _plan.preset;
  // The DOM legend uPlot can render is disabled: its absolute-position table
  // spills outside compact cards and duplicates our toggle legend. Live
  // values ride the setCursor hook - every series' value at the hovered
  // bucket, stamped in the series color, prefixed by the bucket's time range.
  const hoverEl = () => $('chart-hover');
  // Stable line order (including hidden series) gives coincident isolated
  // samples distinct marks: a filled inner point, then transparent rings.
  // Independent axes may place different values at exactly the same pixel.
  let lineIndex = 0;
  return {
    width: w,
    height: h,
    padding: [12, preset.right ? 0 : 12, 0, 0],
    legend: { show: false },
    select: { show: false },
    cursor: { y: false, drag: { x: false, y: false }, points: { show: false } },
    hooks: {
      setCursor: [u => {
        const el = hoverEl();
        if (!el) return;
        const i = u.cursor.idx;
        if (i == null || !chartAgg) { el.hidden = true; el.textContent = ''; return; }
        const bm = chartAgg.bucket_ms || 0;
        // compacted bars index buckets through _vis (x is slot positions);
        // line presets index the payload buckets directly
        const pi = _vis ? _vis[Math.max(0, Math.min(_vis.length - 1, i))] : i;
        const bucket = chartAgg.buckets[pi];
        if (!bucket) { el.hidden = true; return; }
        const t0 = bucket.t, t1 = t0 + bm;
        const parts = ['<div class="chart-hover-time">' + chartBucketRange(t0, t1) + '</div>'];
        for (let si = 0; si < _plan.meta.length; si++) {
          const m = _plan.meta[si];
          if (m.hidden) continue;
          const v = u.data[si + 1][i];
          if (v == null) continue;
          parts.push(`<div class="chart-hover-row"><span style="color:${COLORS[m.spec.color]}">${m.spec.label}</span><span>${m.spec.fmt(v)}</span></div>`);
        }
        el.innerHTML = parts.join('  ');
        el.style.left = u.cursor.left < u.bbox.width / (window.devicePixelRatio || 1) / 2 ? 'auto' : '8px';
        el.style.right = u.cursor.left < u.bbox.width / (window.devicePixelRatio || 1) / 2 ? '8px' : 'auto';
        el.hidden = false;
      }],
      leave: [() => {
        const el = hoverEl();
        if (el) { el.hidden = true; el.textContent = ''; }
      }],
    },
    scales: {
      // Numeric milliseconds / slot indices, always app-labeled. Callbacks
      // consult current data so compaction transitions never stale the axes.
      x: { time: false, range: chartXRange },
      // bar presets share one zero-safe arcsinh scale (distr 4): linear near
      // 0, log-like in the tail - a series or bucket 100× smaller than its
      // neighbor stays visible instead of flattening into the baseline, and
      // true zeros plot at 0 (impossible on a log scale).
      y: { distr: 4, asinh: 1, range: chartBarRange },
      yttft: { auto: true },
      ytps: { auto: true },
      yr: { auto: true },
      pct: { auto: false, range: [0, 100] }, // error-rate axis pinned to 0–100
    },
    axes: [
      // uPlot's axes[0] is always the bottom (x/time) axis - the left y axis
      // must be the second entry or it collapses to an auto-created default
      // (black labels). values() receives the splits array and must map it -
      // feeding the array straight into a number formatter yields one "-".
      // The x labels are app-owned: chartXTick on real timestamps, or (on the
      // compacted bars x, where x is slot positions) the true bucket times.
      { ...axBase, size: 32, grid: { show: false }, splits: chartXSplits, values: chartXValues },
      {
        ...axBase,
        scale: preset.left.scale,
        // the shared bars scale is arcsinh (distr 4, compression inside
        // valToPos): uPlot's default integer splits would bunch at the
        // baseline and its log filter would thin the decades away, so the
        // ticks come from our screen-spaced decade ladder instead
        ...(preset.left.scale === 'y'
          ? { splits: chartYTicks, filter: (u, splits) => splits }
          : {}),
        values: (u, splits) => chartAxisValues(preset.left, splits),
      },
      ...(preset.right
        ? [{ ...axBase, scale: preset.right.scale, side: 1, values: (u, splits) => chartAxisValues(preset.right, splits), grid: { show: false } }]
        : []),
    ],
    series: [
      {},
      ..._plan.meta.map(m => {
        const s = m.spec;
        if (m.bar) {
          // per-bucket sum → bar on the shared arcsinh scale; width 0 keeps
          // pure fills (no stroke). bars-preset series carry their grouped
          // slot offsets from the current plan; a lone cost bar centers.
          return {
            label: s.label,
            fill: hexA(COLORS[s.color], 0.8),
            width: 0,
            paths: chartBarPaths(),
            scale: m.scale || preset.left.scale,
            points: { show: false },
            value: (u, v) => (v == null ? '-' : s.fmt(v)),
          };
        }
        const pointIndex = lineIndex++;
        return {
          label: s.label,
          stroke: COLORS[s.color],
          width: 1.6,
          dash: s.dash,
          scale: m.scale,
          points: {
            show: false, filter: chartIsolatedPoints,
            size: CHART_POINT_SIZE * (pointIndex + 1),
            width: pointIndex ? CHART_POINT_RING_WIDTH : 0,
            fill: pointIndex ? 'transparent' : COLORS[s.color],
          },
          value: (u, v) => (v == null ? '-' : s.fmt(v)),
        };
      }),
    ],
  };
}

// chartTotals renders the viewed period's totals (server buckets,
// always all series - hiding is a visual declutter, the totals stay
// honest) as the strip at the top of the graph card. Speed/latency totals are
// period-wide percentiles the server computes (tps_p / ttft_p), never an
// average of bucket percentiles.
function chartTotals() {
  if (!chartAgg || !chartAgg.buckets || !chartAgg.buckets.length) return '';
  let req = 0, err = 0, tin = 0, tout = 0, tcache = 0, treason = 0, cost = 0;
  chartAgg.buckets.forEach((b, i) => {
    req += b.req;
    err += b.err;
    tin += b.in;
    tout += b.out;
    tcache += (b.cache || 0);
    treason += b.reason;
    cost += b.cost;
  });
  const rate = req ? (err / req) * 100 : null;
  const span = (label, val) => `<span class="chart-total"><span class="tl">${label}</span> ${val}</span>`;
  const seriesSpan = (s, val) => `<span class="chart-total" style="color:${COLORS[s.color]}"${s.title ? ` title="${escapeHtml(s.title)}"` : ''}><span class="tl">${s.label}</span> ${s.fmt(val)}</span>`;
  const p = activePreset();
  const parts = [];
  switch (p.id) {
    case 'traffic':
      parts.push(seriesSpan(chartSpec('req'), req));
      parts.push(seriesSpan(chartSpec('err'), err));
      if (rate != null) parts.push(span('rate', fmtPct(rate)));
      break;
    case 'tokens':
      parts.push(seriesSpan(chartSpec('inTok'), tin));
      parts.push(seriesSpan(chartSpec('outTok'), tout));
      parts.push(seriesSpan(chartSpec('reason'), treason));
      parts.push(seriesSpan(chartSpec('cache'), tcache));
      break;
    case 'errors':
      parts.push(seriesSpan(chartSpec('err'), err));
      if (rate != null) parts.push(span('rate', fmtPct(rate)));
      break;
    case 'cost':
      parts.push(seriesSpan(chartSpec('cost'), cost));
      parts.push(span('requests', fmt(req)));
      break;
    case 'latency': {
      for (const [id] of p.series) {
        const spec = chartSpec(id);
        parts.push(seriesSpan(spec, chartAgg[spec.metric + '_p']?.[pctIdx()] ?? null));
      }
      break;
    }
  }
  return parts.join('  ');
}

function chartChromeSync() {
  const preset = activePreset();
  $('chart-pct').hidden = !preset.series.some(([id]) => chartSpec(id).metric);
  const compressed = preset.left.scale === 'y'
    ? ' <span class="chart-scale" title="The scale compresses large values so smaller values remain visible. Compare the labeled values, not bar-height ratios.">· compressed scale</span>' : '';
  updateSection('chart-axes', `<span>${preset.left.label}${compressed}</span><span>${preset.right?.label || ''}</span>`);
  const context = chartAgg ? fmtDur(chartAgg.bucket_ms) + ' buckets' + (_compacted ? ' · empty intervals omitted' : '') : 'Waiting for traffic';
  updateSection('chart-context', context);
}

function renderChart() {
  const box = $('chart-traffic'); // the .chart-wrap div - uPlot mounts INTO it
  if (!box) return;
  const data = chartData(); // also refreshes _plan (legend + series read it)
  chartChromeSync();
  chartLegendSync();
  const strip = $('chart-totals');
  if (strip) updateSection('chart-totals', chartTotals());
  // Boot has no chart payload yet. Sync its controls without forcing layout
  // of the just-written KPI/log DOM for a canvas that cannot draw anything.
  // A real zero-traffic payload still measures and paints the blank state.
  const canMeasure = typeof uPlot !== 'undefined' && chartAgg?.buckets?.length;
  const w = canMeasure ? Math.round(box.clientWidth) : 0;
  const h = canMeasure ? Math.round(box.clientHeight) : 0;
  const canRender = canMeasure && data && !chartIsEmpty() && w >= 80 && h >= 40;
  if (!canRender) {
    if (_up) { _up.destroy(); _up = null; _upKey = ''; }
    if (canMeasure && w >= 80 && h >= 40) {
      const { ctx } = setupCanvas(box);
      ctx.clearRect(0, 0, w, h);
      drawBlank(ctx, 'no traffic yet');
    }
    return;
  }
  // Only the preset changes series/axes. Percentile, window, and hidden
  // choices only replace data, preserving the canvas and cursor bindings.
  const key = chartView.preset;
  if (_up && key !== _upKey) { _up.destroy(); _up = null; }
  for (const c of box.querySelectorAll('canvas.chart-blank')) c.remove();
  if (!_up) {
    _up = new uPlot(upOpts(w, h), data, box); // canvas created by uPlot
    _upKey = key;
  } else {
    if (_up.width !== w || _up.height !== h) _up.setSize({ width: w, height: h });
    _up.setData(data);
  }
}

// chartLegendSync renders the active preset's toggleable series legend through
// updateSection (content-hash dirty check), so an unchanged legend costs zero
// DOM churn. Bar presets list their series in slot order (the same order they
// plot in - grouped bars need no reversal).
function chartLegendSync() {
  const l = $('traffic-legend');
  if (!l || !_plan) return;
  const html = _plan.meta.map(({ spec, bar, hidden }) => {
    const mark = bar ? 'bar' : spec.dash ? 'dashed' : 'line';
    const title = (hidden ? 'Show' : 'Hide') + ' ' + spec.label + (spec.title ? '. ' + spec.title : '');
    return `<button type="button" class="leg-item${hidden ? ' off' : ''}" data-series="${spec.id}" aria-pressed="${!hidden}" title="${escapeHtml(title)}" style="--sw:${COLORS[spec.color]}"><span class="swatch ${mark}" aria-hidden="true"></span>${spec.label}</button>`;
  }).join('');
  updateSection('traffic-legend', html);
}

function toggleChartSeries(id) {
  const preset = activePreset();
  if (!preset.series.some(([seriesId]) => seriesId === id)) return;
  const cur = chartView.hidden[preset.id] || [];
  chartView.hidden[preset.id] = cur.includes(id) ? cur.filter(x => x !== id) : cur.concat(id);
  storage.set('dash.chart', JSON.stringify(chartView)); // persist across reloads
  renderChart();
}

function setChartPreset(v) {
  if (!CHART_PRESETS.some(pr => pr.id === v)) return;
  chartView.preset = v;
  storage.set('dash.chart', JSON.stringify(chartView));
  renderChart();
}

function setChartWindow(v) {
  if (!CHART_WINDOWS.some(([val]) => val === v)) return;
  chartView.window = v;
  storage.set('dash.chart', JSON.stringify(chartView));
  fetchChart(); // buckets are server-computed per window
}

function setChartPct(v) {
  const n = Number(v);
  if (!CHART_PCTS.includes(n)) return;
  chartView.pct = n;
  storage.set('dash.chart', JSON.stringify(chartView));
  renderChart();
}

function fillChartControls() {
  fillSelectPairs($('chart-preset'), CHART_PRESETS.map(pr => [pr.id, pr.label]), chartView.preset);
  fillSelectPairs($('chart-window'), CHART_WINDOWS, chartView.window);
  fillSelectPairs($('chart-pct'), CHART_PCTS.map(p => [String(p), 'p'+p]), String(chartView.pct));
}

function drawBlank(ctx, msg) {
  ctx.fillStyle = COLORS.muted; ctx.font = '12px ui-monospace'; ctx.textAlign = 'center';
  const dpr = window.devicePixelRatio || 1;
  ctx.fillText(msg, (ctx.canvas.width / dpr) / 2, (ctx.canvas.height / dpr) / 2);
}
