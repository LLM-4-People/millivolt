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
// DURATION_ENDINGS is the one open-ended sentence per surface. The duration
// tokens are shared; only the word for "no window chosen" differs, because a
// hold waits to be resumed while a capture records until it is stopped. It was
// spelled six times across the two sections, and one capture row had already
// drifted into calling the same open-ended session "until stopped" in its live
// column and "manual stop" in its budget line.
const DURATION_ENDINGS = { pause: 'until resumed', capture: 'until stopped' };
const PAUSE_DURS = [['', DURATION_ENDINGS.pause]].concat(durationPairs('15m', '1h', '6h', '12h', '24h'));
// CAPTURE_DURS is the same ladder with the capture surface's ending. It derives
// from PAUSE_DURS rather than re-listing the tokens, so the two pickers cannot
// offer different windows.
const CAPTURE_DURS = [['', DURATION_ENDINGS.capture]].concat(PAUSE_DURS.slice(1));

// ocDurationText reads a duration token as its label, falling back to the
// surface's open-ended sentence when no window was chosen. The kind is
// required: a default would silently give a capture the pause wording, which is
// exactly the drift this replaced.
function ocDurationText(kind, value) {
  return DURATION_LABELS[value] || value || DURATION_ENDINGS[kind];
}

// ocDurationTail is the clause a sentence appends: "for 15 minutes" when a
// window was chosen, or the surface's own open-ended sentence.
function ocDurationTail(kind, value) {
  return value ? ' for ' + ocDurationText(kind, value) : ' ' + DURATION_ENDINGS[kind];
}

// ocCaptureWindowPhrase is the one reading of a capture session's window: the
// time it has left, else the span it was started with, else the open-ended
// sentence. The console's row and the overview's row both print it, and they
// had drifted three ways: the row called an open-ended session "until stopped"
// in its live column and "manual stop" in its budget line, while the overview
// printed the raw token ("15m") instead of its label.
function ocCaptureWindowPhrase(session) {
  if (!session) return '';
  return untilLeft(session.until) || ocDurationText('capture', session.duration);
}
// ---------- operator scope vocabulary ----------
// The selector dimensions and their readings live here, in the module that
// loads before every other operator surface, because three layers read them:
// the footer's hold line, the header readout and the console's rows and
// composers. They used to live in operator_console.js, which loads after the
// footer, so the footer could not call the phrase owner and re-derived a
// poorer copy: a hold scoped to a model, a key or a conversation printed only
// the word "hold", and "all traffic" became "all requests" on one surface.
//
// ocScopePhrase is the one human reading of a selector. Rule rows, previews,
// confirmations and the readout all call it, so an operator never has to
// translate three vocabularies for one policy.
function ocScopePhrase(scope) {
  if (!scope) return 'every request';
  const bits = [];
  for (const dim of OC_SCOPE_DIMS) {
    const values = scope[dim.key] || [];
    if (!values.length) continue;
    if (values.length === 1) bits.push(dim.one + ' ' + ocScopeValueLabel(dim.key, values[0]));
    else bits.push(values.length + ' ' + dim.many);
  }
  return bits.length ? bits.join(' · ') : 'every request';
}

// ocScopeValueLabel is the one shortening of a raw selector value. The
// dimension name always comes from the surrounding sentence or row, so this
// never repeats it: a key reads as its short hash, not "key key abcdef12".
function ocScopeValueLabel(dim, value) {
  if (dim === 'keyHashes') return String(value).slice(0, 8) + '…';
  return String(value);
}

// OC_SCOPE_DIMS is the one table of selector dimensions, in display order.
// A limit, a hold and a capture all match a request by the same identity, so
// every spelling of a dimension lives on its one row:
//
//   key    the client's field name, which the editors and drafts use
//   wire   the server document's name, which the store and the POST body use
//   one    the word a sentence uses for a single value
//   many   the word a sentence uses for several
//   record the request record field that carries the value
//
// The phrase readers, the selector key, the record matchers, the composer
// bodies and the picker labels all read this table. Adding a dimension is one
// row here instead of eight edits that can disagree about the wire name or the
// singular word.
const OC_SCOPE_DIMS = [
  { key: 'providers', wire: 'providers', one: 'provider', many: 'providers', record: 'provider' },
  { key: 'models', wire: 'models', one: 'model', many: 'models', record: 'model' },
  { key: 'clients', wire: 'clients', one: 'client', many: 'clients', record: 'client' },
  { key: 'keyHashes', wire: 'key_hashes', one: 'key', many: 'keys', record: 'key_hash' },
  { key: 'conversations', wire: 'conversations', one: 'conversation', many: 'conversations', record: 'conversation_id' },
];

// OC_SCOPE_KEYS is the table's client-side order, the shape every composer and
// chip strip iterates.
const OC_SCOPE_KEYS = OC_SCOPE_DIMS.map(dim => dim.key);

// OC_CAPTURE_SCOPE_KEYS is the subset a capture session can select on. It is
// narrower on purpose: the server's debug scope carries clients, providers and
// models, while a limit and a hold also match a key hash and a conversation.
// It is derived from the table so a dimension rename reaches it too.
const OC_CAPTURE_SCOPE_KEYS = OC_SCOPE_DIMS
  .filter(dim => ['clients', 'providers', 'models'].includes(dim.key))
  .map(dim => dim.key);

function ocScopeDim(key) {
  return OC_SCOPE_DIMS.find(dim => dim.key === key || dim.wire === key) || null;
}

function ocNewScope() {
  const scope = {};
  for (const dim of OC_SCOPE_DIMS) scope[dim.key] = [];
  return scope;
}

// ocScopeFromPolicy reads a stored document into the draft shape. A limit
// nests its selector under `scope`; a hold and a capture session carry the same
// dimensions flat, so both shapes are accepted and the caller passes the
// document it has.
function ocScopeFromPolicy(policy) {
  const raw = policy?.scope || policy || {};
  const scope = ocNewScope();
  for (const dim of OC_SCOPE_DIMS) scope[dim.key] = [...(raw[dim.wire] || [])];
  return scope;
}

// ocScopeToWire turns a draft's selector into the document's field names. The
// bodies send every dimension explicitly, because the server treats a supplied
// array as a replacement and an omitted one as "keep".
function ocScopeToWire(scope) {
  const wire = {};
  for (const dim of OC_SCOPE_DIMS) wire[dim.wire] = scope[dim.key] || [];
  return wire;
}

// ocPolicyScopePhrase is the phrase for a stored server document, which nests
// its selector. Every surface that shows a rule's blast radius reads this, so
// the row, the removal confirmation and the overview cannot re-derive it
// differently.
function ocPolicyScopePhrase(policy) {
  return ocScopePhrase(ocScopeFromPolicy(policy));
}

function ocScopeHas(scope) {
  return Object.values(scope).some(values => values.length > 0);
}

// ocHoldScopePhrase is the one reading of a hold's blast radius. A hold is one
// of three shapes - everything, clients unseen when it started, or a named
// selector - and the preview, both rule rows, the ledger phrase and the resume
// confirmation each stated that choice themselves. One owner keeps "all
// traffic" from becoming "everything" on one surface and "all" on another.
function ocHoldScopePhrase(hold) {
  if (!hold) return 'every request';
  if (hold.all) return 'all traffic';
  if (hold.new) return 'new clients';
  return ocScopePhrase(ocScopeFromPolicy({ scope: hold }));
}

// ocHoldDraftScopePhrase is the same reading for a composer draft, whose mode
// is a name rather than the two booleans a stored hold carries.
function ocHoldDraftScopePhrase(draft) {
  if (!draft) return 'every request';
  if (draft.mode === 'all') return 'all traffic';
  if (draft.mode === 'new') return 'new clients';
  return ocScopePhrase(draft.scope);
}

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

// ---------- operator clock ----------
// Every hold and capture deadline is absolute server time, and the server is
// the only clock that can decide whether one has passed. Reading a deadline
// with Date.now() let a workstation that ran ahead hide a live hold: the row
// disappeared, the readout said "steady", and the proxy went on parking
// traffic. Each operator document carries server_ms, so one anchor is recorded
// when a document is accepted and every deadline is measured against it.
let _opClockOffsetMs = 0;
let _opClockAnchored = false;

function anchorOperatorClock(serverMs) {
  if (!Number.isFinite(serverMs) || serverMs <= 0) return;
  _opClockOffsetMs = serverMs - Date.now();
  _opClockAnchored = true;
}

// operatorNowMs is the server's current instant as well as this page can know
// it. Before any anchor arrives it is the local clock, which is what every
// surface used to read unconditionally.
function operatorNowMs() {
  return Date.now() + (_opClockAnchored ? _opClockOffsetMs : 0);
}

// untilRemainingMs is the one arithmetic for an absolute operator deadline:
// null when none is set, otherwise milliseconds remaining, negative once past.
// holdLive, the footer's remaining-time bit and the console's rule rows all read
// this, so they cannot disagree about whether a rule is still in force.
function untilRemainingMs(until) {
  if (!until) return null;
  const t = new Date(until).getTime();
  if (!Number.isFinite(t)) return null;
  return t - operatorNowMs();
}

// untilLeft is the one remaining-time sentence. `past` is what an already
// expired deadline reads as: a rule row says "expired", while the footer omits
// the bit rather than asserting a state it does not track.
function untilLeft(until, past = '') {
  const ms = untilRemainingMs(until);
  if (ms === null) return '';
  if (ms <= 0) return past;
  return hmBreakdown(Math.max(1, Math.ceil(ms / 60000))) + ' left';
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
// LIMIT_WINDOW_DEFAULT is the one fallback window: what a new budget starts
// with, and what a stored policy without a window reads as. It is a member of
// LIMIT_WINDOWS, so the fallback can never name a token the picker does not
// offer (and a Go test posts every LIMIT_WINDOWS token through the real
// handler). The composer wrote the literal four times before this owner.
const LIMIT_WINDOW_DEFAULT = '1m';
function pauseUntilLabel(until) {
  return untilLeft(until);
}

// parkedCounts is the one reading of the two parked populations the pause
// document publishes. They are not interchangeable: an operator drain releases
// admission and send-gate waiters, while a request parked in the storm/quota
// gate waits before admission, so only that gate opening or a quota resume
// frees it. Keeping the split here is what stops a surface from offering a
// drain for work the drain cannot touch, and from claiming nothing is waiting
// while the gate holds every new request.
function parkedCounts() {
  return {
    drainable: Math.max(0, Number(pauseState.queued) || 0),
    gated: Math.max(0, Number(pauseState.storm_waiters) || 0),
  };
}

// pauseStatusBits and debugStatusBits own the operator status word lists the
// footer renders. The footer's hold variant inserts the live-hold scope bits
// before the queued count; only that surface renders scope, so its call site
// passes them in.
function pauseStatusBits(scopeBits = []) {
  const bits = ['paused', ...scopeBits];
  const parked = parkedCounts();
  if (parked.drainable > 0) bits.push('queued ' + parked.drainable);
  // A gated request is parked by a gate no hold in this list owns, so the
  // footer names it instead of folding it into the hold's own queued count.
  if (parked.gated > 0) bits.push('gated ' + parked.gated);
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
      // One hold names its blast radius through the shared reader, the same
      // one the console's rows, previews and receipts call. The footer used to
      // assemble its own from all/new/clients/providers, so a hold scoped to a
      // model, a key or a conversation printed only the word "hold", and its
      // "all requests" had already drifted from the console's "all traffic"
      // for the same state.
      const scopeBits = live.length === 1 ? [ocHoldScopePhrase(live[0])]
        : live.length > 1 ? [countPhrase(live.length, 'hold')]
          : [];
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
