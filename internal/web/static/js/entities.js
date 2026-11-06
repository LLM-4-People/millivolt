// ---------- record classification (client-side log / pills) ----------
// recordIsError and recordHasRateLimit mirror the backend
// metrics.Record.IsError and metrics.Record.HasRateLimit exactly (single
// canonical definition, both sides).
//
// The two answer different questions: IsError asks "did this request fail"
// (a final 429 is flow control and a plain 499 is the client's own abort, so
// neither is an error; a recorded structured error on any other status is),
// and HasRateLimit asks "did it hit a rate limit", which is the CLASS, not
// the status: a provider may announce one with a 400
// (`tier_limit_exceeded`, `resource_exhausted`), and those count in the
// rate-limit series whether or not the request also failed.
//
// The vocabulary below is owned by Go (metrics.rateLimitClasses) and published
// to this file's mirror; TestRateLimitClassVocabularyMatchesJS in
// tests/_go/internal/web/js_mirrors.go reddens on any drift. When the operator
// installs rate_limit_error_classes extensions, the server publishes the
// EFFECTIVE vocabulary in the bootstrap payload and
// setRateLimitVocabulary replaces this list at runtime, so the browser cannot
// disagree with the server about a row's class.
const RATE_LIMIT_CLASSES = [
  'membership_limit_exceeded',
  'quota_exceeded',
  'rate_limit',
  'rate_limit_error',
  'rate_limit_exceeded',
  'ratelimiterror',
  'requests_per_minute_exceeded',
  'resource_exhausted',
  'rpm_limit_exceeded',
  'slow_down',
  'tier_limit_exceeded',
  'tokens_per_minute_exceeded',
  'too_many_requests',
  'too_many_requests_error',
  'tpm_limit_exceeded',
];

// EFFECTIVE_RATE_LIMIT_CLASSES is the vocabulary classification actually uses:
// the built-in mirror until the server publishes an operator-extended list.
let EFFECTIVE_RATE_LIMIT_CLASSES = RATE_LIMIT_CLASSES;

// setRateLimitVocabulary installs the server-published effective vocabulary
// (bootstrap payload rate_limit_classes) or restores the built-in mirror when
// the payload carries none (a reload that removed the extensions).
function setRateLimitVocabulary(list) {
  EFFECTIVE_RATE_LIMIT_CLASSES = (Array.isArray(list) && list.length) ? list : RATE_LIMIT_CLASSES;
}

// rateLimitClass reports whether an error type or code names a rate limit,
// case-insensitively, matching metrics.IsRateLimitClass (built-ins plus any
// operator extensions).
function rateLimitClass(typ, code) {
  const t = String(typ || '').trim().toLowerCase();
  const c = String(code || '').trim().toLowerCase();
  return EFFECTIVE_RATE_LIMIT_CLASSES.includes(t) || EFFECTIVE_RATE_LIMIT_CLASSES.includes(c);
}

// recordHasRateLimit mirrors metrics.Record.HasRateLimit: the final response,
// the class, or any attempt.
function recordHasRateLimit(r) {
  if ((r.status_code || 0) === 429) return true;
  if (rateLimitClass(r.error_type, r.error_code)) return true;
  return (r.attempts || []).some(a =>
    (a.status_code || 0) === 429 || rateLimitClass(a.error_type, a.error_code));
}

// recordOutcomeIsError mirrors metrics.OutcomeIsError: whether ONE observed
// outcome (a final response, or a single absorbed retry attempt) is a genuine
// failure on the health surface. The Go owner is the single source of truth;
// its SQL twin (metrics.IsErrorSQL) and this mirror are pinned to it by the
// health-predicate truth table and the corpus lockstep.
//
// The health surface asks "did this request fail", not "what kind of thing was
// this": a final 429 is the flow-control handshake the client can retry and is
// not an error, while a recorded structured error on any other status is a
// failure even when its own type or code names a rate limit (recordHasRateLimit
// counts those in the rate-limit series).
function recordOutcomeIsError(status, typ, code) {
  const s = status || 0;
  const t = String(typ || '');
  if (s === 429) return false;
  if (s === 499) return t !== '';
  return t !== '' || s >= 400;
}

// recordIsError mirrors metrics.Record.IsError: the final outcome through
// recordOutcomeIsError, plus an absorbed 5xx from any attempt (the upstream
// genuinely failed once, even if a later retry recovered).
// Used for log scope and status pills;
// every numeric surface renders the server-side aggregate (same definition in Go).
function recordIsError(r) {
  const absorbed5xx = (r.attempts || []).some(a => (a.status_code || 0) >= 500);
  return recordOutcomeIsError(r.status_code, r.error_type, r.error_code) || absorbed5xx;
}

// ---------- explorer: entity-type visual registry ----------
// Single source of truth for "what is what": every entity type has an icon, a
// color, and a label, and is always rendered with all three (WCAG 1.4.1 - never
// color alone). Colorblind-safe qualitative palette (adapted from Okabe-Ito for
// a dark theme); parent/child pairs share a hue family (provider→model).
// Error reserves vermillion and never shares it. Every surface
// (rail, gallery, scope chips, inspector) renders through entityBadge().
const ENTITY_TYPES = {
  client:       { label: 'Client',       color: '#56B4E9', icon: '◈' }, // app-window (sky blue)
  provider:     { label: 'Provider',     color: '#0072B2', icon: '☁' }, // cloud (blue)
  model:        { label: 'Model',        color: '#CC79A7', icon: '◆' }, // bot (reddish purple)
  conversation: { label: 'Conversation', color: '#009E73', icon: '❝' }, // messages (bluish green)
  key:          { label: 'Key',          color: '#F0E442', icon: '⚷' }, // key (yellow)
  status:       { label: 'Status',       color: '#8A8F98', icon: '●' }, // circle (neutral grey)
  time:         { label: 'Time',         color: '#5AC8FA', icon: '◷' }, // clock (light cyan)
  tool:         { label: 'Tool',         color: '#1ABC9C', icon: '⚒' }, // hammer-pick (teal)
  request:      { label: 'Request',      color: '#E69F00', icon: '⇄' }, // arrow-left-right (orange)
  error:        { label: 'Error',        color: '#D55E00', icon: '⚠' }, // triangle-alert (vermillion)
};
// entityType returns the registry entry for a dimension (defaults to request).
function entityType(dim) { return ENTITY_TYPES[dim] || ENTITY_TYPES.request; }
// entityBadge(dim, id, label?) renders the icon+color+label triple. id is the
// raw entity id (used for the provider favicon origin + tooltip); label is the
// display text (defaults to dimLabel(dim, id)). Always pass the raw id as the
// 2nd arg so providers resolve their favicon from the full host, not the short
// label. Always shows the type icon (color-tinted) so the type is recognizable
// even when color is missed.
function entityBadge(dim, id, label, rec, labelTitle) {
  const t = entityType(dim);
  const disp = label != null ? label : dimLabel(dim, id);
  // The tooltip shows the obfuscated display form (shortId), never the raw id -
  // for a key that is the full sha256, which must only ever appear as …<8-hex>.
  // Client badges use the custom #client-tip card instead of a native title
  // (native title would double-fire on hover). The empty title also suppresses
  // an ancestor request cell's full-identity title over the badge.
  let ttl = id ? ` title="${t.label}: ${escapeHtml(shortId(dim, id))}"` : ` title="${t.label}"`;
  let extra = '';
  if (dim === 'client' && id) {
    ttl = ` title="" aria-label="${t.label}: ${escapeHtml(disp)}"`;
    extra = ` data-client="${escapeHtml(id)}"${rec && rec.id ? ` data-rec="${escapeHtml(rec.id)}"` : ''}`;
  }
  // Providers keep their favicon as the icon (falls back to the type icon for
  // hosts without a public favicon or when the icon fails to load). Every other
  // dimension uses its ENTITY_TYPES icon. Icon + color + label always together.
  let icon = `<span class="ent-ic" aria-hidden="true">${t.icon}</span>`;
  if (dim === 'provider') {
    const origin = providerOrigin(id);
    if (origin && !_faviconBroken.has(origin)) {
      // favicon + a hidden type-icon fallback shown by the delegated favicon
      // error handler (faviconErr reveals the next sibling on load failure).
      icon = `<img class="favicon ent-fav" data-origin="${escapeHtml(origin)}" src="https://www.google.com/s2/favicons?domain=${encodeURIComponent(origin)}&sz=32" alt="" loading="lazy" referrerpolicy="no-referrer"><span class="ent-ic" aria-hidden="true" style="display:none">${t.icon}</span>`;
    }
  }
  // A caller-supplied label title belongs on the innermost visible label, the
  // element a pointer actually hovers, not on a wrapper that never receives
  // the event.
  const labelTitleAttr = labelTitle ? ` title="${escapeHtml(labelTitle)}"` : '';
  return `<span class="ent" style="--ent:${t.color}"${ttl}${extra}>${icon}<span class="ent-lb"${labelTitleAttr}>${escapeHtml(disp)}</span></span>`;
}

// entityPivot wraps entityBadge in a pivot link so an entity reference is both
// type-recognizable (icon+color+label) and navigable (adds that dim=id filter
// in the faceted explorer). Canonical render for a client/provider/model.
function entityPivot(dim, id, label, rec, labelTitle) {
  if (!id) return entityBadge(dim, id, label, rec, labelTitle);
  const extra = dim === 'client' ? ` data-client="${escapeHtml(id)}"${rec && rec.id ? ` data-rec="${escapeHtml(rec.id)}"` : ''}` : '';
  const noTitle = dim === 'client' ? ' title=""' : '';
  return `<a class="ent ent-link" href="${hashFor(dim, id)}"${extra}${noTitle}>${entityBadge(dim, id, label, rec, labelTitle)}</a>`;
}

function latestRecForClient(id) {
  const recs = lastData && lastData.records || [];
  let best = null;
  for (const r of recs) {
    if (!r || r.client !== id) continue;
    if (!best || (r.start && (!best.start || r.start > best.start))) best = r;
  }
  return best;
}

function clientTipHTML(id, rec) {
  const rows = [];
  const sec = (h) => { rows.push(`<div class="ct-h">${h}</div>`); };
  const row = (k, v) => {
    if (v == null || v === '' || v === false) return;
    rows.push(`<div class="ct-row"><span class="ct-k">${escapeHtml(k)}</span><span class="ct-v">${escapeHtml(String(v))}</span></div>`);
  };
  const join = (xs) => xs.filter(Boolean).join(' ');
  sec('Client');
  row('name', clientLabel(id));
  if (!rec) return rows.join('');
  const m = rec.client_meta || {};
  row('lang', rec.client_lang);
  row('sdk', m.pkg_ver);
  row('runtime', join([m.runtime, m.runtime_ver]));
  row('os', join([m.os, m.arch]));
  row('ua', rec.user_agent && rec.user_agent.length > 56 ? rec.user_agent.slice(0, 56) + '…' : rec.user_agent);
  row('ip', rec.client_ip);
  sec('Config');
  row('format', m.format);
  if (rec.stream) row('stream', 'yes');
  row('max_tokens', rec.req_max_tokens);
  row('temp', rec.req_temperature);
  row('top_p', rec.req_top_p);
  row('effort', rec.req_reasoning_effort);
  row('verbosity', rec.req_verbosity);
  if (rec.req_thinking) row('thinking', 'yes');
  if (rec.req_tools_count) row('tools', rec.req_tools_count + (rec.req_tool_choice ? ' · ' + rec.req_tool_choice : ''));
  row('tier', rec.req_service_tier);
  if (m.timeout_ms) row('timeout', fmtDur(m.timeout_ms));
  if (m.max_concurrency) row('max conc', m.max_concurrency);
  const turns = [rec.turns_user, rec.turns_assistant, rec.turns_tool].some(n => n);
  if (turns) row('turns', (rec.turns_user || 0) + '→' + (rec.turns_assistant || 0) + (rec.turns_tool ? '→' + rec.turns_tool : '') + (rec.last_turn_role ? ' ⇢' + rec.last_turn_role : ''));
  return rows.join('');
}

function showClientTip(el) {
  const tip = $('client-tip');
  if (!tip || !el) return;
  const id = el.dataset.client;
  const rec = (el.dataset.rec && recordById(el.dataset.rec)) || latestRecForClient(id);
  const html = clientTipHTML(id, rec);
  if (!html) { hideClientTip(); return; }
  tip.innerHTML = html;
  tip.hidden = false;
  const box = el.getBoundingClientRect();
  const tw = tip.offsetWidth, th = tip.offsetHeight;
  let x = box.left, y = box.bottom + 6;
  if (y + th > window.innerHeight - 8) y = Math.max(8, box.top - th - 6);
  if (x + tw > window.innerWidth - 8) x = Math.max(8, window.innerWidth - tw - 8);
  if (x < 8) x = 8;
  tip.style.left = x + 'px';
  tip.style.top = y + 'px';
}

function hideClientTip() {
  const tip = $('client-tip');
  if (tip) tip.hidden = true;
}

{
  let cur = null;
  document.addEventListener('pointerover', e => {
    const el = e.target.closest && e.target.closest('[data-client]');
    if (!el) return;
    if (cur === el) return;
    cur = el;
    showClientTip(el);
  });
  document.addEventListener('pointerout', e => {
    const el = e.target.closest && e.target.closest('[data-client]');
    if (!el || cur !== el) return;
    const next = e.relatedTarget && e.relatedTarget.closest && e.relatedTarget.closest('[data-client]');
    if (next === el) return;
    cur = null;
    hideClientTip();
  });
  window.addEventListener('scroll', hideClientTip, {passive: true, capture: true});
}

// ---------- summary tile sparks (hand-rolled SVG, no dependency) ----------
// sparkPath builds one sparse-tolerant, self-normalized line path over the
// series' index space: nulls leave real gaps (x still advances per data
// slot), values scale to their own finite min/max, and fewer than two
// finite points render no line at all.
function sparkPath(data, w, h) {
  const pts = [];
  const n = data ? data.length : 0;
  for (let i = 0; i < n; i++) if (Number.isFinite(data[i])) pts.push([i, data[i]]);
  if (pts.length < 2) return '';
  let min = Infinity, max = -Infinity;
  for (const [, v] of pts) { if (v < min) min = v; if (v > max) max = v; }
  if (min === max) max = min + 1; // guard divide-by-zero on a flat series
  const dx = w / (n - 1);
  return pts.map(([i, v], k) => `${k ? 'L' : 'M'}${(i * dx).toFixed(1)} ${(h - ((v - min) / (max - min)) * h).toFixed(1)}`).join('');
}

// sparkFrame is the single owner of the decorative spark <svg> frame: the
// fixed attribute set (size, viewBox, non-uniform stretch, a11y) around the
// paths. One builder keeps every emission site byte-identical.
const sparkFrame = (w, h, body) =>
  `<svg class="spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" preserveAspectRatio="none" aria-hidden="true" focusable="false">${body}</svg>`;
// sparkLine is the one line-path builder: every spark line shares the same
// stroke vocabulary (1.4px, rounded joins and caps, no fill).
const sparkLine = (d, color) =>
  `<path d="${d}" fill="none" stroke="${color}" stroke-width="1.4" stroke-linejoin="round" stroke-linecap="round"/>`;

// sparklineMulti draws several independently-scaled colored lines in ONE
// spark frame: the summary tiles' mini charts, one line per metric half in
// that half's color. Per-line normalization keeps halves with different
// units or magnitudes (ms vs tok/s, requests vs tokens) all readable - a
// shared scale would flatten every smaller series into the baseline. Lines
// only, no area washes: two or three overlapping fills read as mud.
function sparklineMulti(specs, w, h) {
  const lines = specs.map(s => {
    const d = sparkPath(s.vals, w, h);
    return d ? sparkLine(d, s.color) : '';
  }).filter(Boolean);
  return sparkFrame(w, h, lines.join(''));
}

// shareBar renders a part-to-whole proportion bar + % for a value against a
// total - the meaningful secondary encoding for a dimension (its slice of the
// whole), where a sparkline would be meaningless (every value shares the same
// scope's shape). Bar carries the shape, the % carries the value (WCAG: never
// color/length alone). fraction is 0..1. The % flows through pct (the pctCap
// owner): a strictly-sub-1 fraction can never round up to a false "100%",
// and a full fraction is a true 100.
function shareBar(fraction, color) {
  const p = Math.max(0, Math.min(1, fraction || 0));
  const pctTxt = pct(p, 1);
  return `<span class="share" title="${pctTxt} of total"><span class="share-bar"><span class="share-fill" style="width:${(p*100).toFixed(1)}%;background:${color}"></span></span><span class="share-pct">${pctTxt}</span></span>`;
}

// ---------- explorer: dimension value helpers (status / time / tool) ----------
// The derived (non-stored) dimensions each have one canonical value
// derivation shared by filter matching (and mirrored in Go on the server for
// the aggregates - keep both in lockstep). The derived value is the filter id.

// escapeErrorKeyPart is the mirror of Go escapeErrorKeyPart: a backslash
// escapes a backslash or the '|' separator, and ordinary text is unchanged.
// Error messages are arbitrary text, so without this {type:'a', code:'b|c'}
// and {type:'a|b', code:'c'} would share one identity.
function escapeErrorKeyPart(s) {
  s = s || '';
  return s.indexOf('|') < 0 && s.indexOf('\\') < 0 ? s : s.replace(/[\\|]/g, '\\$&');
}

// errorKey is the stable identity of one distinct error (final or absorbed
// attempt): escaped type|code|msg - same grouping as the explorer error
// dimension. The escape keeps saved ids for ordinary errors unchanged.
function errorKey(type, code, msg) {
  return escapeErrorKeyPart(type) + '|' + escapeErrorKeyPart(code) + '|' + escapeErrorKeyPart(msg);
}

// errorKeyMatches compares the joined error identity. Building the key is
// bounded by the few error entries a row carries; the previous delimiter
// walk could not distinguish a separator inside a message from the field
// boundary.
function errorKeyMatches(key, type, code, msg) {
  return key === errorKey(type, code, msg);
}

// errorKeyParts decodes an escaped error key back into [type, code, message].
// A backslash escapes the next character; the encoder only emits \\ and \|,
// so this is the exact inverse of escapeErrorKeyPart.
function errorKeyParts(key) {
  const parts = [];
  let cur = '';
  for (let i = 0; i < key.length; i++) {
    const ch = key[i];
    if (ch === '\\' && i + 1 < key.length) { cur += key[++i]; continue; }
    if (ch === '|') { parts.push(cur); cur = ''; continue; }
    cur += ch;
  }
  parts.push(cur);
  return parts;
}

function errorKeyType(key) { return errorKeyParts(key)[0] || 'error'; }

// errorKeyDisplay renders the decoded parts for a card. When a part contains
// the separator or a backslash, the escaped key itself is the only injective
// human spelling (any invented quoting can collide with ordinary text), so it
// is shown unchanged. Ordinary errors keep the familiar type|code|msg form.
function errorKeyDisplay(key) {
  const parts = errorKeyParts(key);
  return parts.some(p => p.includes('|') || p.includes('\\')) ? key : parts.join('|');
}

// statusClass buckets a record's HTTP status into a coarse class. Keep in
// lockstep with contribStatusClass in aggregate_status.go. Live in-flight rows
// (the live flag - status_code may already be set after upstream headers)
// are 'paused', 'throttled', 'streaming', or 'pending'; a stored stream:true
// row with status 0 is a finalized transport error, never "currently
// streaming". Errors with no status fall to 'err'. 499 - the proxy's
// marker for "local client closed the connection before a final outcome"
// - is its own 'cancel' class.
function statusClass(r) {
  // live is stamped by begin/update/in_flight_records - never infer it
  // from a missing status_code (a finalized stream+status 0 is err).
  if (r && r.live) {
    if (r.paused) return 'paused';
    if (r.throttled) return 'throttled';
    if (r.stream) return 'streaming';
    return 'pending';
  }
  const s = r.status_code || 0;
  if (s >= 200 && s < 300) return '2xx';
  if (s === 499) return 'cancel';
  if (s >= 400 && s < 500) return '4xx';
  if (s >= 500) return '5xx';
  return 'err';
}

// Classification is authored by metrics.TimeBucket in the server's timezone;
// browser-local dates remain presentation only, never a different scope rule.
function timeBucket(r) { return r.time_bucket || null; }

// recordTools returns the distinct tool names a record called (multi-valued).
// The server payload never carries empty names, so the common case returns the
// existing array instead of allocating a filtered copy for every derivation.
function recordTools(r) {
  if (!Array.isArray(r.tool_names)) return [];
  for (const name of r.tool_names) if (!name) return r.tool_names.filter(Boolean);
  return r.tool_names;
}

// finalErrorEntry returns the record's client-visible failure, or null for a
// clean completion, a bare 429 or a plain client close. Genuine failures are
// exactly the outcomes metrics.OutcomeIsError accepts (recordOutcomeIsError
// mirrors it): a structured error on any status other than a bare 429/499
// (including a 499, which observed the upstream's own failure before the
// client aborted around it) or any status >= 400. A non-429 status carrying a
// rate-limit class is therefore a failure here even though the rate-limit
// series also counts it. The status is only the code fallback once a failure
// is established. The drawer banner and the error-group enumerator share this
// one predicate.
function finalErrorEntry(r) {
  if (!recordOutcomeIsError(r.status_code, r.error_type, r.error_code)) return null;
  const finStatus = r.status_code || 0;
  return {
    type: r.error_type || 'http_' + finStatus,
    code: r.error_code || (finStatus ? String(finStatus) : ''),
    msg: r.error_msg,
    at: r.start,
  };
}

// recordErrorEntries is the single canonical enumerator of a record's error
// entries: the final (client-visible) failure plus each absorbed retry attempt.
// forEachErrorEntry is its visitor form so a match can walk the same entries
// without allocating one object per entry; both share the final-failure
// predicate above, and the absorbed arm reads the same health predicate
// (recordOutcomeIsError) the Go owner (web/aggregate.go errorEntries) reads, so
// the explorer groups the same failures the server does. A transport retry is
// retry mechanics, not an upstream error event, and a bare 429 never lists.
function forEachErrorEntry(r, visit) {
  const final = finalErrorEntry(r);
  if (final) visit(final.type, final.code, final.msg, false, final.at);
  // Absorbed retry attempts: only genuine upstream failures the client never
  // saw. An in-band entry is the hidden status of the final failure, already
  // visited above, so it never adds a second entry.
  for (const a of (r.attempts || [])) {
    if (a.in_band) continue;
    if (a.error_type === 'transport') continue;
    if (!recordOutcomeIsError(a.status_code, a.error_type, a.error_code)) continue;
    const as = a.status_code || 0;
    const t = a.error_type || (as ? 'http_' + as : 'transport');
    const code = a.error_code || (as ? String(as) : '');
    visit(t, code, a.error_msg, true, a.at || r.start);
  }
}

function recordErrorEntries(r) {
  const out = [];
  forEachErrorEntry(r, (type, code, msg, absorbed, at) => {
    out.push({ type, code, msg, provider: r.provider, absorbed, at });
  });
  return out;
}

// ---------- targeted render (only re-render what changed) ----------
// updateSection returns true only when it actually wrote: an existing node with
// changed content. Identical content and a missing node write nothing.
function updateSection(id, content) {
  const el = $(id);
  if (!el) return false;
  const h = content;
  if (lastRender[id] === h) return false;
  lastRender[id] = h;
  el.innerHTML = content;
  return true;
}

// LIVE_STATUS_FILTERS are the in-flight pills the request log actually draws.
// The Requests-card dropdown always offers streaming + paused; throttled /
// pending appear when present. Distinct from explorer status classes (2xx/…).
const LIVE_STATUS_FILTERS = { streaming: 1, paused: 1, throttled: 1, pending: 1 };

// recordMatchesStatusFilter is the single choke point for the Requests-card
// status dropdown: named live pills match statusClass(), numeric codes match
// the exact HTTP status. Used by the log filter.
function recordMatchesStatusFilter(r, status) {
  if (!status) return true;
  if (LIVE_STATUS_FILTERS[status]) return statusClass(r) === status;
  return String(r.status_code) === String(status);
}
