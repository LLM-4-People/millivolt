// Operator vocabularies, live state bits and the footer: the shared duration
// and window lists, canonical model grouping, capture storage state and the
// status words the footer renders. Loaded before chrome_menus.js because
// FILTER_AGES reads DURATION_LABELS from this module at initialization.

const selectPairSigs = new WeakMap();
// setSelectValue sets one select's value, keeping exactly one custom option
// for a saved value outside the curated pairs and removing it once the value
// is curated again. Curated choices are suggestions, not permission to erase
// a saved value.
function setSelectValue(sel, value) {
  const v = value == null ? '' : String(value);
  let custom = null;
  for (const option of sel.options) if (option.dataset.pairCustom === '1') { custom = option; break; }
  const curated = [...sel.options].some(o => o.value === v && o.dataset.pairCustom !== '1');
  if (curated) {
    if (custom) custom.remove();
  } else if (custom) {
    if (custom.value !== v) custom.value = v;
    const label = v || 'off';
    if (custom.textContent !== label) custom.textContent = label;
  } else {
    const option = new Option(v || 'off', v);
    option.dataset.pairCustom = '1';
    sel.add(option);
  }
  if (sel.value !== v) sel.value = v;
}

function fillSelectPairs(sel, pairs, value) {
  if (!sel || !pairs) return;
  const sig = JSON.stringify(pairs);
  if (selectPairSigs.get(sel) !== sig) {
    selectPairSigs.set(sel, sig);
    sel.innerHTML = pairs.map(([val, lab]) => `<option value="${escapeHtml(String(val))}">${escapeHtml(lab)}</option>`).join('');
  }
  setSelectValue(sel, value);
}

const DURATION_LABELS = {
  '15m': '15 minutes', '1h': '1 hour', '6h': '6 hours',
  '12h': '12 hours', '24h': '24 hours',
};
// durationPairs builds [value, label] select pairs from DURATION_LABELS.
const durationPairs = (...vals) => vals.map(v => [v, DURATION_LABELS[v]]);
const PAUSE_DURS = [['', 'I resume']].concat(durationPairs('15m', '1h', '6h', '12h', '24h'));
function liveDebugSessions() {
  return (debugState.sessions || []).filter(holdLive);
}

function debugActive() {
  return !!(debugState.enabled || liveDebugSessions().length);
}

function groupByCanonical(names) {
  const byGroup = new Map();
  for (const n of [...new Set(names || [])]) {
    const g = canonicalModel(n);
    if (!byGroup.has(g)) byGroup.set(g, []);
    byGroup.get(g).push(n);
  }
  return [...byGroup.entries()].map(([key, variants]) => ({ key, variants: variants.sort() }));
}
// dbgGroupedModels: raw (deduped, sorted) names → ordered [group] objects
// {key, display, variants}; a group's display name is its shortest variant
// (the bare spelling), ties broken lexicographically.
function debugStorageDisabled() {
  return typeof storageState !== 'undefined' && !!storageState && storageState.enabled === false;
}

function debugScopeLabel(h) {
  if (!h) return '';
  const bits = [];
  if (Array.isArray(h.clients) && h.clients.length) bits.push(h.clients.join(', '));
  if (Array.isArray(h.providers) && h.providers.length) bits.push(h.providers.join(', '));
  if (Array.isArray(h.models) && h.models.length) bits.push(h.models.join(', '));
  return bits.join(' + ') || 'session';
}

// hmBreakdown is the one compact H/M breakdown for remaining-time labels:
// whole minutes below an hour, hours plus remaining minutes above. Callers
// own the rounding of their minute count and any suffix - pause holds say
// "N left" from a ceil, debug sessions report whole minutes from a round.
function hmBreakdown(m) {
  if (m < 60) return m + 'm';
  const hr = Math.floor(m / 60), rem = m % 60;
  return rem ? hr + 'h ' + rem + 'm' : hr + 'h';
}

const LIMIT_WINDOWS = [
  ['1s', '1 second'], ['10s', '10 seconds'], ['30s', '30 seconds'],
  ['1m', '1 minute'], ['5m', '5 minutes'],
].concat(durationPairs('15m', '1h', '6h', '24h'));
// The fallback window when a limit carries none (fillLimitWindows' default
// and the apply body's missing-select fallback): '1m', named once.
function pauseUntilLabel(until) {
  if (!until) return '';
  const ms = new Date(until).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return '';
  return hmBreakdown(Math.ceil(ms / 60000)) + ' left';
}

// pauseStatusBits and debugStatusBits own the operator status word lists the
// footer renders. The footer's hold variant inserts the live-hold scope bits
// before the queued count; only that surface renders scope, so its call site
// passes them in.
function pauseStatusBits(scopeBits = []) {
  const bits = ['paused', ...scopeBits];
  if (pauseState.queued > 0) bits.push('queued ' + pauseState.queued);
  const left = pauseUntilLabel(pauseState.until);
  if (left) bits.push(left);
  return bits;
}

function debugStatusBits() {
  const bits = ['debug'];
  const live = liveDebugSessions();
  if (live.length === 1) bits.push(debugScopeLabel(live[0]));
  else if (live.length > 1) bits.push(live.length + ' sessions');
  const left = pauseUntilLabel(debugState.until);
  if (left) bits.push(left);
  return bits;
}

// setFooterLive mirrors SSE connectivity. refreshFooterState combines that
// with every other footer input: the operator surfaces (a hold reads as
// "paused", a debug session as "debug", each with scope and remaining
// time), the storage health footnotes (durable drops, a degraded boot
// scan), and "offline" when the live stream drops.
function setFooterLive(live) {
  streamLive = !!live;
  if (streamLive) streamSeen = true;
  refreshFooterState();
  // Connectivity is one fact, and the header readout reports it too. Repaint
  // both from this owner so neither can lag the other.
  if (typeof refreshOperatorReadout === 'function') refreshOperatorReadout();
}

function refreshFooterState() {
  const dot = $('f-live'), state = $('f-state');
  const holding = pauseActive();
  const debugging = debugActive();
  if (dot) dot.classList.toggle('paused', holding || debugging || !streamLive);
  if (state) {
    if (holding) {
      const live = liveHolds();
      let scopeBits = [];
      if (live.length === 1) {
        const h = live[0];
        const bits = [];
        if (h.all) bits.push('all requests');
        else {
          if (h.new) bits.push('new clients');
          if (Array.isArray(h.clients) && h.clients.length) bits.push(h.clients.join(', '));
          if (Array.isArray(h.providers) && h.providers.length) bits.push(h.providers.join(', '));
        }
        scopeBits = [bits.join(' + ') || 'hold'];
      } else if (live.length > 1) {
        scopeBits = [live.length + ' holds'];
      }
      state.textContent = pauseStatusBits(scopeBits).join(' · ');
    } else if (debugging) {
      state.textContent = debugStatusBits().join(' · ');
    } else {
      state.textContent = streamLive ? 'live' : 'offline';
    }
    // Storage health footnotes share the warning styling: durable drops,
    // and a degraded boot scan (since-inception totals incomplete).
    const dropped = storageState?.enabled ? storageState.dropped : 0;
    const degraded = storageState?.totals_degraded === true;
    state.classList.toggle('storage-warning', dropped > 0 || degraded);
    state.title = dropped > 0
      ? 'Records not saved durably since this process started (queue overflow, closed store, or write failure). Some may remain in the live ring.'
      : degraded
        ? 'The startup history scan failed, so since-inception totals only count rows committed since this process started.'
        : '';
    if (dropped > 0) state.textContent += ' · ' + fmt(dropped) + ' storage drops';
    if (degraded) state.textContent += ' · totals degraded';
  }
}

function doFilter() {
  filters.status = $('f-status').value;
  storage.set(FILTERS_STORAGE_KEY, JSON.stringify(filters)); // persist across reloads
  renderAll(lastData);
  fetchChart();      // chart is scoped to status + explorer filters
  // The shared request gate cancels an obsolete scan immediately and
  // dedupes repeated selections; no delay or second debounce owner.
  fetchExplorer();
}
