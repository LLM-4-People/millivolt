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
// and percentage rates (error rate, cache hit) render as % lines on
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
const CHART_GROUP_WIDTH = 0.72;
const CHART_GROUP_GAP = 0.08;
const CHART_AXIS_SIZE = 48;
const CHART_POINT_SIZE = 4; // isolated line samples need a visible mark
const CHART_POINT_RING_WIDTH = 1;
// Totals-strip sparklines: the per-bucket evolution carried under EVERY
// tile value (uniform rhythm; decorative like the explorer sparks - the tile
// value carries the number, the spark carries the shape). The emitted width
// only sizes the viewBox: each spark stretches with its tile (CSS
// width:100%), whose tracks floor at 160px in the tiles-only summary grid.
const CHART_SPARK_W = 72, CHART_SPARK_H = 14;
// CHART_TILE_PCT selects the summary tiles' SPARKLINE percentile only: the
// timing tile's value is the server's period average (ttft_stat / tps_stat),
// never this percentile - it reaches the tile solely as the per-bucket spark
// lines. The preset keeps only the time-range control, no percentile
// selector and no visible pXX label anywhere; the tooltip's ordinal text
// derives from this constant, and its index into the server's [p50, p95,
// p99] wire triple derives from the CHART_PCTS owner, so the sparks and the
// tooltip cannot drift apart.
const CHART_TILE_PCT = 95;
const CHART_TILE_PCT_IDX = CHART_PCTS.indexOf(CHART_TILE_PCT);
// pctOrdinal names a percentile ordinally ('50th', '95th', '99th') for the
// timing tile's tooltip, from CHART_TILE_PCT - the text can never claim a
// percentile the spark lines do not draw. The suffix table is literal for
// p % 100 so the teens are visible rather than derived: 111th, 112th,
// 113th read from rows 11-13, not from a last-digit rule (the audited
// "111st" bug class). An out-of-table index (a negative input) falls back
// to 'th' exactly like the previous suffix chain.
const PCT_SUFFIX = [
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'th', 'th', 'th', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
  'th', 'st', 'nd', 'rd', 'th', 'th', 'th', 'th', 'th', 'th',
];
const pctOrdinal = p => p + (PCT_SUFFIX[p % 100] || 'th');
// A sparse window cannot fill the plot width honestly: one to three plotted
// buckets strand a lone bar or a short line in dead space on both sides.
// The big chart waits for a fourth point and paints the blank meanwhile -
// the totals tiles and their sparklines still carry the story.
const CHART_MIN_POINTS = 4;
// The smallest box (CSS pixels) the big chart measures before it paints:
// below this the canvas stays untouched. A sub-minimum dashboard cell must
// show nothing rather than a clipped or misread plot. The blank-state
// branch uses the same two thresholds.
const CHART_MIN_W = 80;
const CHART_MIN_H = 40;
// Reuse locale formatters; cursor movement must not create one per readout.
const CHART_DATE_FORMAT = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric' });
const CHART_FULL_DATE_FORMAT = new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
const CHART_DAY_MS = 86400000;

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
  { id: 'rl',      label: 'rate limited', color: 'rl',    fmt: fmt,
    title: X429_COUNTING_RULE + '. A rate limit is not an error.' },
  { id: 'inTok',   label: 'tokens in',  color: 'accent2', fmt: fmt,      bar: true },
  { id: 'outTok',  label: 'tokens out', color: 'ok',      fmt: fmt,      bar: true },
  { id: 'reason',  label: 'reasoning',  color: 'cyan',    fmt: fmt,      bar: true },
  { id: 'cost',    label: 'cost',       color: 'warn',    fmt: fmtMoney, bar: true },
  { id: 'cache',   label: 'cached',     color: 'muted',   fmt: fmt,      dash: [3, 3] },
  { id: 'errRate', label: 'error rate', color: 'warn',    fmt: fmtPct },
  { id: 'cachePct', label: 'cache hit', color: 'warn',   fmt: fmtPct,
    title: 'Cached prompt tokens as a share of input tokens, per bucket.' },
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
    // Overview is the summary: it carries NO plot. The tiles ARE the
    // surface, and they take the whole card. Tile ids drive selection,
    // persistence and validation exactly like series ids do for plotted
    // presets: clicking a tile hides it (a label-only stub keeps the grid
    // cell, so toggling never rewraps or shifts anything), the choice
    // persists in dash.chart, and unknown ids drop on load.
    id: 'overview', label: 'Overview', tilesOnly: true,
    tiles: ['req', 'tokens', 'cost', 'health', 'timing'],
  },
  {
    id: 'traffic', label: 'Traffic',
    left: { scale: 'y', fmt: fmt, label: 'Requests' }, right: null,
    series: [['req', 'y'], ['err', 'y']],
  },
  {
    id: 'tokens', label: 'Tokens',
    left: { scale: 'y', fmt: fmt, label: 'Tokens' }, right: { scale: 'pct', fmt: fmtPct, label: 'Cache hit' },
    series: [['inTok', 'y'], ['outTok', 'y'], ['reason', 'y'], ['cache', 'y'], ['cachePct', 'pct']],
  },
  {
    // Speed + latency: both lines must read the same requests, so only
    // buckets carrying BOTH measurements at the selected percentile plot -
    // a bucket missing either is dropped (and empty intervals compact
    // away) instead of stranding gap points in dead space.
    id: 'latency', label: 'Speed + latency', requireAll: true,
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
// localStorage key for the persisted chart view. One owner: the load and
// save paths below reference the same constant.
const CHART_VIEW_STORAGE_KEY = 'dash.chart';
// saveChartView is the one persistence path for the chart view (preset,
// window, percentile, hidden sets): every validated change goes through it.
function saveChartView() {
  storage.set(CHART_VIEW_STORAGE_KEY, JSON.stringify(chartView));
}
// loadChartView applies the persisted dash.chart payload onto chartView.
// Deny by default: each field lands only when its saved shape validates
// (window/pct/preset against their owner lists, hidden per-preset), so a
// legacy or garbage payload can never smuggle in an unknown value.
function loadChartView() {
  try {
    const saved = JSON.parse(storage.get(CHART_VIEW_STORAGE_KEY) || '{}');
    if (CHART_WINDOWS.some(([v]) => v === String(saved.window))) chartView.window = String(saved.window);
    const p = Number(saved.pct);
    if (CHART_PCTS.includes(p)) chartView.pct = p;
    if (CHART_PRESETS.some(pr => pr.id === saved.preset)) chartView.preset = saved.preset;
    if (saved.hidden && !Array.isArray(saved.hidden)) {
      for (const pr of CHART_PRESETS) {
        // Hidden ids validate against the preset's selectable surface: tile
        // ids for the summary, series ids for plotted presets. Anything
        // else drops (deny by default).
        const valid = pr.tilesOnly ? pr.tiles : pr.series.map(([seriesId]) => seriesId);
        const ids = Array.isArray(saved.hidden[pr.id])
          ? saved.hidden[pr.id].filter(id => valid.includes(id))
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
    case 'rl': return b.rl;
    case 'inTok': return b.in;
    case 'outTok': return b.out;
    case 'cache': return b.cache;
    case 'reason': return b.reason;
    case 'cost': return b.cost;
    case 'errRate': return b.req ? (b.err / b.req) * 100 : null;
    case 'cachePct': return b.in ? (b.cache / b.in) * 100 : null;
    default: return s.metric ? (b[s.metric]?.[pctIdx()] ?? null) : null;
  }
}

// chartPlan computes the active preset's full render plan: uPlot series
// metadata in plot order, the legend rows, and the hidden flags. Every
// preset row carries its resolved renderer (registry flag or per-row
// override → m.bar); grouped-bar sizing runs over the visible bar
// series only, so hiding one re-centers the rest. Hidden series keep their
// column (all-null) - a toggle must be a pure setData with a stable column
// count or the plot keeps its stale frame.
let _plan = null;
function chartPlan() {
  const preset = activePreset();
  // A tiles-only preset has no plotted rows: the plan is the preset alone
  // (chartData renders nothing, the legend is empty, the summary tiles read
  // chartAgg directly).
  if (preset.tilesOnly) return { preset, meta: [] };
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
// x values are bucket-slot centers. Bars-scale presets drop empty (req 0)
// buckets and a requireAll preset (speed + latency) drops buckets missing
// any row's measurement at the selected percentile: either way x compacts
// to slot centers in index space (a dropped slot is a hole that only
// wastes width - each surviving point keeps its true time for ticks/hover
// via _vis). Hidden rows still count toward requireAll: the bucket contract
// is what the preset measures, not what is momentarily shown.
let _vis = null, _compacted = false;
function chartData() {
  _plan = chartPlan();
  if (_plan.preset.tilesOnly) {
    // No plotted rows: also clear any compaction state a previous preset
    // left behind, so the module never carries stale geometry.
    _vis = null;
    _compacted = false;
    return null; // the summary plots nothing
  }
  if (!chartAgg || !chartAgg.buckets || !chartAgg.buckets.length) return null;
  const bm = chartAgg.bucket_ms || 0;
  let idxs = null;
  const keep = _plan.preset.requireAll
    ? (b => _plan.meta.every(m => Number.isFinite(chartBucketVal(m.spec, b))))
    : (b => b.req > 0);
  const canCompact = _plan.preset.requireAll || _plan.preset.left.scale === 'y';
  if (canCompact && chartAgg.buckets.some(b => !keep(b))) {
    idxs = [];
    chartAgg.buckets.forEach((b, i) => { if (keep(b)) idxs.push(i); });
    if (!idxs.length) return null; // nothing measurable → blank state
  }
  _vis = idxs;
  _compacted = !!idxs;
  const src = idxs || chartAgg.buckets.map((_, i) => i);
  const data = [src.map((pi, k) => (idxs ? k + 0.5 : chartAgg.buckets[pi].t + bm / 2))];
  for (const m of _plan.meta) {
    if (m.hidden) {
      data.push(src.map(() => null));
      continue;
    }
    data.push(src.map(pi => chartBucketVal(m.spec, chartAgg.buckets[pi])));
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
// remaining bars. Bars fill their fraction of the bucket slot at ANY
// density: dense windows stay slim, sparse windows fill the plot instead of
// stranding hairline marks in empty space (a category axis - the compacted
// x already omits empty intervals). disp.x0 is the left edge; align alone
// cannot separate three bars.
function chartBarGeometry(si) {
  const m = _plan.meta[si - 1];
  if (m.hidden) return { offset: 0, size: 0 };
  const step = _compacted ? 1 : chartAgg.bucket_ms;
  return { offset: m.offset * step, size: m.size * step };
}

function chartBarPaths() {
  return uPlot.paths.bars({
    size: [1, Infinity, 0],
    radius: 0.12,
    disp: {
      x0: { unit: 1, values: (u, si) => {
        const g = chartBarGeometry(si);
        return u.data[0].map(x => x + g.offset);
      } },
      size: { unit: 1, values: (u, si) => [chartBarGeometry(si).size] },
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
      pct: { auto: false, range: [0, 100] }, // pinned 0-100: error rate and cache hit
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

// shareText renders a measured part-to-whole share ('60.0% of in') or null
// when the ratio was never measured (zero denominator) - one owner for the
// summary's combined share row and the plotted presets' share sub-rows, so
// every surface renders the same measured-share-or-nothing semantics.
const shareText = (a, b, of) => {
  const p = pct(a, b);
  return p === '-' ? null : p + (of ? ' ' + of : '');
};
// TILE_HALVES pairs every summary-tile metric half's CSS class (dashboard.css
// owns the class styling) with the palette key that colors the half's spark
// line. tilePair emits the half spans and tileSpark emits the spark lines,
// both from this one registry, so a half's class and its spark color cannot
// drift apart; the ui_check contract test pins the pairing.
const TILE_HALVES = {
  'v-req': 'accent',
  'v-tok': 'cyan',
  'v-in': 'accent2',
  'v-out': 'ok',
  'v-cache': 'muted',
  'v-err': 'err',
  'v-rl': 'rl',
  'v-ttft': 'accent',
  'v-tps': 'ok',
};
// tilePair renders the halves as ONE wrapped line over thin separators, no
// spaces - the KPI band's duo format, so the pair fits the narrowest tile.
// It must stay a single wrapped element: bare sibling spans become one flex
// line each and stack the pair vertically.
const tilePair = entries =>
  `<span class="val-pair">${entries.map(([cls, v]) => `<span class="${cls}">${v}</span>`).join('<span class="pair-sep">/</span>')}</span>`;
// sparkLines draws the summary sparks' shared wiring: one self-scaled line
// per [palette key, bucket pick] over the traffic-bearing buckets.
const sparkLines = (kept, specs) =>
  sparklineMulti(specs.map(([color, pick]) => ({ vals: kept.map(pick), color: COLORS[color] })), CHART_SPARK_W, CHART_SPARK_H);
// tileSpark routes each half's spark line through its registered palette key.
const tileSpark = (kept, halves) => sparkLines(kept, halves.map(([cls, pick]) => [TILE_HALVES[cls], pick]));

// TILE_LABELS is the one owner of the overview tiles' labels: the
// skeleton strip maps the preset's tile order through it and the
// data-filled tiles read theirs from it, so the two can never drift
// (ui_check pins them label for label). Ids match the data-tile toggle
// contract.
const TILE_LABELS = {
  req: 'requests / tokens',
  tokens: 'tokens in/out/cached',
  cost: 'cost',
  health: 'errors / 429',
  timing: 'avg latency / speed',
};

// Placeholder strip for the no-data state: the active preset's tile skeleton
// at full tile height, so the first payload swaps text and the card below the
// strip never moves. Tile COUNTS must match the data-filled strip per preset
// (what wraps is rows, not labels) - ui_check pins the equality.
function chartTotalsSkeleton() {
  const p = activePreset();
  const ph = label =>
    `<span class="chart-total"><span class="tl">${label}</span> -<span class="chart-sub">-</span></span>`;
  if (p.id === 'overview') {
    return p.tiles.map(id => ph(TILE_LABELS[id])).join('  ');
  }
  return p.series.map(([id]) => ph(chartSpec(id).label)).join('  ')
    + (p.id === 'traffic' ? '  ' + ph('rate') : '');
}

// chartTotals renders the viewed period's totals (server buckets,
// always all series - hiding is a visual declutter, the totals stay
// honest) as the strip at the top of the graph card. Speed/latency totals
// are period-wide server figures (plotted presets: tps_p / ttft_p
// percentiles; the summary's timing tile: ttft_stat / tps_stat averages
// with sample ranges), never an average of bucket percentiles.
function chartTotals() {
  if (!chartAgg || !chartAgg.buckets || !chartAgg.buckets.length) return chartTotalsSkeleton();
  let req = 0, err = 0, rl = 0, tin = 0, tout = 0, tcache = 0, treason = 0, cost = 0;
  chartAgg.buckets.forEach((b, i) => {
    req += b.req;
    err += b.err;
    rl += (b.rl || 0);
    tin += b.in;
    tout += b.out;
    tcache += (b.cache || 0);
    treason += b.reason;
    cost += b.cost;
  });
  // Tile percentages flow through pct (the pctCap owner): a strictly-sub-100
  // ratio can never round up to a false "100%".
  const span = (label, val, sub = '') => `<span class="chart-total"><span class="tl">${label}</span> ${val}${sub}</span>`;
  const seriesSpan = (s, val, sub = '') => `<span class="chart-total" style="color:${COLORS[s.color]}"${s.title ? ` title="${escapeHtml(s.title)}"` : ''}><span class="tl">${s.label}</span> ${s.fmt(val)}${sub}</span>`;
  // pctSub renders a measured part-to-whole share as a small muted sub-row
  // ('60.0% of in') under the value. An unmeasured ratio (zero denominator)
  // renders no sub-row, never a fabricated 0%.
  const pctSub = (a, b, of) => {
    const t = shareText(a, b, of);
    return t == null ? '' : `<span class="chart-sub">${t}</span>`;
  };
  // inOutShare renders the token balance as the input share of the blended
  // volume. A percentage is bounded and reads identically in every locale;
  // the old ratio formatter emitted comma decimals ('2,664') that read like
  // a count. Zero denominator renders no sub-row, never a fabricated share.
  const inOutShare = (tin, tout) => pctSub(tin, tin + tout, 'in');
  // tokenPair renders the in/out pair in the token bars' colors (input
  // purple, output green) through the shared tilePair owner.
  const tokenPair = (tin, tout) => tilePair([['v-in', fmt(tin)], ['v-out', fmt(tout)]]);
  const p = activePreset();
  const parts = [];
  switch (p.id) {
    case 'traffic':
      parts.push(seriesSpan(chartSpec('req'), req));
      parts.push(seriesSpan(chartSpec('err'), err));
      if (req) parts.push(span('rate', pct(err, req)));
      break;
    case 'tokens': {
      const tot = tin + tout;
      parts.push(seriesSpan(chartSpec('inTok'), tin, pctSub(tin, tot)));
      parts.push(seriesSpan(chartSpec('outTok'), tout, pctSub(tout, tot)));
      parts.push(seriesSpan(chartSpec('reason'), treason));
      parts.push(seriesSpan(chartSpec('cache'), tcache, pctSub(tcache, tin, 'of in')));
      parts.push(span('in:out', tokenPair(tin, tout), inOutShare(tin, tout)));
      break;
    }
    case 'errors':
      parts.push(seriesSpan(chartSpec('err'), err));
      if (req) parts.push(span('rate', pct(err, req)));
      break;
    case 'cost':
      parts.push(seriesSpan(chartSpec('cost'), cost));
      parts.push(span('requests', fmt(req)));
      break;
    case 'overview': {
      // The summary is metrics-only: every tile reads value first, then its
      // measured companion fact, then its per-bucket evolution sparkline.
      // Merged tiles keep the story compact: requests carries the blended
      // token volume as its second half, tokens carries the in/out/cached
      // triple with both shares, the health tile carries both health counts
      // in the chart's health colors, and cost carries the server's blended
      // per-Mtok price - the SAME figure the KPI band shows,
      // cost-reporting requests only.
      // Tiles follow the legend's toggle contract: role=button +
      // aria-pressed + data-tile, the choice persists in dash.chart, and a
      // hidden tile keeps its grid cell as a label-only stub so toggling
      // never rewraps or shifts anything.
      const hid = chartView.hidden.overview || [];
      const tile = (id, label, title, color, body) => {
        const attrs = `role="button" tabindex="0" data-tile="${id}" aria-pressed="${!hid.includes(id)}" title="${escapeHtml((hid.includes(id) ? 'Show ' : 'Hide ') + label + '. ' + title)}"`;
        return hid.includes(id)
          ? `<span class="chart-total off" ${attrs}><span class="tl">${label}</span></span>`
          : `<span class="chart-total" style="color:${COLORS[color]}" ${attrs}><span class="tl">${label}</span> ${body}</span>`;
      };
      const tot = tin + tout;
      // Tile sparks are mini charts: one sparkline per tile, one line per
      // metric half in exactly that half's color (TILE_HALVES pairs each
      // half class with its palette key), each line scaled to its own
      // series (halves measure different units). Only traffic-bearing
      // buckets enter the spark - the same empty-bucket removal the plots
      // apply - so ladder padding never squeezes the data into a corner.
      const kept = chartAgg.buckets.filter(b => b.req > 0);
      parts.push(tile('req', TILE_LABELS.req,
        'Request count over the blended token volume (input plus output), as a pair.',
        'accent',
        tilePair([['v-req', fmt(req)], ['v-tok', fmt(tot)]]) +
        tileSpark(kept, [['v-req', b => b.req], ['v-tok', b => b.in + b.out]])));
      // One share row for the tokens triple: the input share of the blended
      // volume and the cached share of input, both through the shareText
      // owner (pct/pctCap) so a sub-100 ratio can never round to a false 100%.
      const shares = [shareText(tin, tot, 'in'), shareText(tcache, tin, 'of in')].filter(Boolean).join(' · ');
      parts.push(tile('tokens', TILE_LABELS.tokens,
        'Input, output and cached prompt tokens as one in / out / cached triple, with the input share of the blended volume and the cached share of input underneath.',
        'cyan',
        tilePair([['v-in', fmt(tin)], ['v-out', fmt(tout)], ['v-cache', fmt(tcache)]]) +
        (shares ? `<span class="chart-sub">${shares}</span>` : '') +
        tileSpark(kept, [['v-in', b => b.in], ['v-out', b => b.out], ['v-cache', b => b.cache]])));
      const costSpec = chartSpec('cost');
      parts.push(tile('cost', TILE_LABELS.cost, costSpec.title || '', costSpec.color,
        fmtMoney(cost) + (chartAgg.cost_per_mtok != null ? `<span class="chart-sub">${fmtMoney(chartAgg.cost_per_mtok)} /Mtok</span>` : '') + sparkLines(kept, [[costSpec.color, b => b.cost]])));
      parts.push(tile('health', TILE_LABELS.health,
        'Errors and rate-limited requests as a pair (error red / 429 tone), over the request count. A rate limit is not an error: both are distinct affected requests.', 'err',
        tilePair([['v-err', fmt(err)], ['v-rl', fmt(rl)]]) +
        (req ? `<span class="chart-sub">of ${fmt(req)} requests</span>` : '') +
        tileSpark(kept, [['v-err', b => b.err], ['v-rl', b => b.rl || 0]])));
      // The timing tile merges latency and speed into one story with both
      // halves in the latency preset's plot colors. The value is the PERIOD
      // AVERAGE over every captured sample (server-computed, never an
      // average of bucket percentiles), each metric's low-high sample
      // range underneath, and the per-bucket pair at CHART_TILE_PCT as its
      // spark lines.
      const ts = chartAgg.ttft_stat, ps = chartAgg.tps_stat;
      // Range bounds render as integers: a locale comma decimal ('46,853'
      // for 46.85) reads as a count - the same ambiguity that replaced the
      // old in:out ratio with a bounded share. A shared trailing unit is
      // stated once ('31-32ms', never '31ms-32ms'); bounds on different
      // scales keep both units ('900ms-2.50s').
      const unitOf = v => (String(v).match(/[a-zA-Z%]+$/) || [''])[0];
      const range = (m, fmtFn, unit) => {
        if (!m) return '';
        const lo = fmtFn(m[1]), hi = fmtFn(m[2]);
        const shared = unitOf(lo) === unitOf(hi) && unitOf(lo) !== '';
        return `${shared ? String(lo).slice(0, -unitOf(lo).length) : lo}-${hi}${unit}`;
      };
      const ranges = [range(ts, fmtDur, ''), range(ps, v => fmt(Math.round(v)), '/s')].filter(Boolean).join(' · ');
      parts.push(tile('timing', TILE_LABELS.timing,
        'Averages over every measured request in the viewed period, never an average of bucket percentiles, with each metric\'s low-high sample range underneath. Sparks: per-bucket ' + pctOrdinal(CHART_TILE_PCT) + ' percentiles.',
        'accent',
        tilePair([['v-ttft', ts ? fmtDur(ts[0]) : '-'], ['v-tps', ps ? fmt(ps[0]) : '-']]) +
        (ranges ? `<span class="chart-sub">${ranges}</span>` : '') +
        tileSpark(kept, [['v-ttft', b => b.ttft?.[CHART_TILE_PCT_IDX] ?? null], ['v-tps', b => b.tps?.[CHART_TILE_PCT_IDX] ?? null]])));
      break;
    }
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
  if (preset.tilesOnly) {
    // The summary has no axes, no percentile control and no cadence note:
    // the tiles carry the choices themselves and take the freed space.
    $('chart-pct').hidden = true;
    updateSection('chart-axes', '');
    $('chart-context').hidden = true;
    return;
  }
  $('chart-pct').hidden = !preset.series.some(([id]) => chartSpec(id).metric);
  $('chart-context').hidden = false;
  const compressed = preset.left.scale === 'y'
    ? ' <span class="chart-scale" title="The scale compresses large values so smaller values remain visible. Compare the labeled values, not bar-height ratios.">· compressed scale</span>' : '';
  updateSection('chart-axes', `<span>${preset.left.label}${compressed}</span><span>${preset.right?.label || ''}</span>`);
  const omitted = _compacted
    ? (_plan?.preset.requireAll ? ' · unmeasured intervals omitted' : ' · empty intervals omitted')
    : '';
  const context = chartAgg ? fmtDur(chartAgg.bucket_ms) + ' buckets' + omitted : 'Waiting for traffic';
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
  // The summary is metrics-only: the tiles ARE the surface and take the
  // card (the CSS class swaps the layout); no plot ever mounts and no
  // blank canvas paints - the skeleton tiles own the no-data state.
  const preset = activePreset();
  const card = box.closest('.traffic-card');
  if (card) card.classList.toggle('tiles-only', !!preset.tilesOnly);
  if (preset.tilesOnly) {
    // Tear down whatever the previous preset left: a mounted plot would
    // linger hidden, and a stale blank canvas would outlive its state.
    if (_up) { _up.destroy(); _up = null; _upKey = ''; }
    for (const c of box.querySelectorAll('canvas.chart-blank')) c.remove();
    return;
  }
  // Boot has no chart payload yet. Sync its controls without forcing layout
  // of the just-written KPI/log DOM for a canvas that cannot draw anything.
  // A real zero-traffic payload still measures and paints the blank state.
  const canMeasure = typeof uPlot !== 'undefined' && chartAgg?.buckets?.length;
  const w = canMeasure ? Math.round(box.clientWidth) : 0;
  const h = canMeasure ? Math.round(box.clientHeight) : 0;
  const points = data ? data[0].length : 0;
  const canRender = canMeasure && data && !chartIsEmpty() && points >= CHART_MIN_POINTS && w >= CHART_MIN_W && h >= CHART_MIN_H;
  if (!canRender) {
    if (_up) { _up.destroy(); _up = null; _upKey = ''; }
    if (canMeasure && w >= CHART_MIN_W && h >= CHART_MIN_H) {
      const { ctx } = setupCanvas(box);
      ctx.clearRect(0, 0, w, h);
      // Three honest blanks, in priority order: no traffic at all, traffic
      // the preset cannot measure, or a window too sparse to fill the plot
      // width. 'no traffic yet' would be false for the last two.
      let msg;
      if (chartIsEmpty()) msg = 'no traffic yet';
      else if (!data) msg = `no ${activePreset().label.toLowerCase()} samples yet`;
      else msg = 'not enough data points yet';
      drawBlank(ctx, msg);
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

// toggleHiddenMember is the one flip contract for chart visibility: a
// member is validated against the surface's own list, toggled in its
// hidden-map key, persisted in dash.chart, and re-rendered. Legend series
// key by preset id; summary tiles key under 'overview'.
function toggleHiddenMember(key, id, validList) {
  if (!validList.includes(id)) return;
  const cur = chartView.hidden[key] || [];
  chartView.hidden[key] = cur.includes(id) ? cur.filter(x => x !== id) : cur.concat(id);
  saveChartView();
  renderChart();
}

function toggleChartSeries(id) {
  const preset = activePreset();
  if (!preset.series) return;
  toggleHiddenMember(preset.id, id, preset.series.map(([seriesId]) => seriesId));
}

// toggleSummaryTile flips one summary tile - the same contract as a legend
// toggle, over tile ids: validated against the preset's tile list, persisted
// in dash.chart under the overview key, re-rendered in place.
function toggleSummaryTile(id) {
  const preset = activePreset();
  if (!preset.tilesOnly) return;
  toggleHiddenMember('overview', id, preset.tiles);
}

function setChartPreset(v) {
  if (!CHART_PRESETS.some(pr => pr.id === v)) return;
  chartView.preset = v;
  saveChartView();
  renderChart();
}

function setChartWindow(v) {
  if (!CHART_WINDOWS.some(([val]) => val === v)) return;
  chartView.window = v;
  saveChartView();
  fetchChart(); // buckets are server-computed per window
}

function setChartPct(v) {
  const n = Number(v);
  if (!CHART_PCTS.includes(n)) return;
  chartView.pct = n;
  saveChartView();
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
