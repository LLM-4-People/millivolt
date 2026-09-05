// ---------- record classification (client-side log / pills) ----------
// recordIsError mirrors the backend metrics.Record.IsError exactly (single
// canonical definition, both sides): a request saw an upstream failure if it
// has a structured error_type, or a final error status other than 429/499, or
// an absorbed retry attempt that hit a 5xx. 429 (rate limit) and 499 (local
// client closed the connection - its own cancellation) are flow events, never
// errors. Used for log scope and status pills;
// every numeric surface renders the server-side aggregate (same definition in Go).
function recordIsError(r) {
  const s = r.status_code || 0;
  if (s === 429 || s === 499) {
    // Flow events: only an absorbed 5xx from an earlier attempt makes one a
    // genuine failure.
    return (r.attempts || []).some(a => (a.status_code || 0) >= 500);
  }
  if (r.error_type) return true;
  if (s >= 400) return true;
  return (r.attempts || []).some(a => (a.status_code || 0) >= 500);
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
function entityBadge(dim, id, label, rec) {
  const t = entityType(dim);
  const disp = label != null ? label : dimLabel(dim, id);
  // The tooltip shows the obfuscated display form (shortId), never the raw id -
  // for a key that is the full sha256, which must only ever appear as …<8-hex>.
  // Client badges use the custom #client-tip card instead of native title
  // (native title would double-fire on hover).
  let ttl = id ? ` title="${t.label}: ${escapeHtml(shortId(dim, id))}"` : ` title="${t.label}"`;
  let extra = '';
  if (dim === 'client' && id) {
    ttl = ` aria-label="${t.label}: ${escapeHtml(disp)}"`;
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
      icon = `<img class="favicon ent-fav" data-origin="${escapeHtml(origin)}" src="https://www.google.com/s2/favicons?domain=${encodeURIComponent(origin)}&sz=32" alt="" loading="lazy" referrerpolicy="no-referrer"><span class="ent-ic ent-fav-fb" aria-hidden="true" style="display:none">${t.icon}</span>`;
    }
  }
  return `<span class="ent" style="--ent:${t.color}"${ttl}${extra}>${icon}<span class="ent-lb">${escapeHtml(disp)}</span></span>`;
}

// entityPivot wraps entityBadge in a pivot link so an entity reference is both
// type-recognizable (icon+color+label) and navigable (adds that dim=id filter
// in the faceted explorer). Canonical render for a client/provider/model.
function entityPivot(dim, id, label, rec) {
  if (!id) return entityBadge(dim, id, label, rec);
  const extra = dim === 'client' ? ` data-client="${escapeHtml(id)}"${rec && rec.id ? ` data-rec="${escapeHtml(rec.id)}"` : ''}` : '';
  return `<a class="ent ent-link" href="${hashFor(dim, id)}"${extra}>${entityBadge(dim, id, label, rec)}</a>`;
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
  row('name', id);
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

// ---------- explorer: sparkline (hand-rolled SVG, no dependency) ----------
// sparklineSVG renders a word-sized area+line time-series as a single SVG
// <path> pair (flat cost regardless of point count). Volume is the area+line;
// errors overlay as a thin second line. Independent y-axis per sparkline (nodes
// have wildly different volumes); the numeric total sits beside it. Decorative:
// aria-hidden - the adjacent numbers carry the value (WCAG).
//   series: number[] (volume per bucket), errs: number[] (errors per bucket)
function sparklineSVG(series, errs, w, h, color) {
  if (!color) color = 'var(--accent)';
  if (!series || series.length <= 1) {
    return `<svg class="spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" aria-hidden="true" focusable="false"></svg>`;
  }
  const path = (data) => {
    let min = Math.min(...data), max = Math.max(...data);
    if (min === max) max = min + 1; // guard divide-by-zero on a flat series
    const dx = w / (data.length - 1);
    return data.map((v, i) => `${i ? 'L' : 'M'}${(i * dx).toFixed(1)} ${(h - ((v - min) / (max - min)) * h).toFixed(1)}`).join('');
  };
  const line = path(series);
  // Close the polygon from the first point (line already starts with M at (0, y0)):
  // baseline right edge → bottom edge → implicit close up the left side. Never
  // splice raw numbers after the M command - the old `M0 ${h}${line.slice(1)}`
  // joined "22" and "0.0" into "220.0", emitting invalid path data (Chrome
  // logged "<path> attribute d: Expected number" for every sparkline).
  const area = `${line}L${w} ${h}L0 ${h}Z`;
  const errLine = (errs && errs.some(e => e > 0)) ? `<path d="${path(errs)}" fill="none" stroke="var(--err)" stroke-width="1" opacity="0.9"/>` : '';
  return `<svg class="spark" width="${w}" height="${h}" viewBox="0 0 ${w} ${h}" aria-hidden="true" focusable="false">` +
    `<path d="${area}" fill="${color}" opacity="0.13"/>` +
    `<path d="${line}" fill="none" stroke="${color}" stroke-width="1.4" stroke-linejoin="round" stroke-linecap="round"/>` +
    errLine + `</svg>`;
}

// shareBar renders a part-to-whole proportion bar + % for a value against a
// total - the meaningful secondary encoding for a dimension (its slice of the
// whole), where a sparkline would be meaningless (every value shares the same
// scope's shape). Bar carries the shape, the % carries the value (WCAG: never
// color/length alone). fraction is 0..1.
function shareBar(fraction, color) {
  const p = Math.max(0, Math.min(1, fraction || 0));
  const pctTxt = (p * 100).toFixed(p < 0.1 ? 1 : 0) + '%';
  return `<span class="share" title="${pctTxt} of total"><span class="share-bar"><span class="share-fill" style="width:${(p*100).toFixed(1)}%;background:${color}"></span></span><span class="share-pct">${pctTxt}</span></span>`;
}

// ---------- explorer: dimension value helpers (status / time / tool) ----------
// The derived (non-stored) dimensions each have one canonical value
// derivation shared by filter matching (and mirrored in Go on the server for
// the aggregates - keep both in lockstep). The derived value is the filter id.

// errorKey is the stable identity of one distinct error (final or absorbed
// attempt): type|code|msg - same grouping as the explorer error dimension.
function errorKey(type, code, msg) {
  return [type || '', code || '', msg || ''].join('|');
}

// statusClass buckets a record's HTTP status into a coarse class. Keep in
// lockstep with contribStatusClass in aggregate.go. Live in-flight rows
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
function recordTools(r) {
  return Array.isArray(r.tool_names) ? r.tool_names.filter(Boolean) : [];
}

// recordErrorEntries is the single canonical enumerator of a record's error
// entries: the final (client-visible) failure plus each absorbed retry attempt,
// each as { type, code, msg, provider, absorbed, at }. The log matchers
// consume it so what a card counts and what clicking it filters to cannot drift.
function recordErrorEntries(r) {
  const out = [];
  // Final (client-visible) failure - but 429 (rate limit) and 499 (local
  // client closed the connection) are flow events, not errors, so they
  // produce no error-group entry. Genuine failures: a structured error_type,
  // or an error status other than 429/499.
  const finStatus = r.status_code || 0;
  if (finStatus !== 429 && finStatus !== 499 && (r.error_type || finStatus >= 400)) {
    const code = r.error_code || (finStatus ? String(finStatus) : '');
    out.push({ type: r.error_type || 'http_' + finStatus, code, msg: r.error_msg, provider: r.provider, absorbed: false, at: r.start });
  }
  // Absorbed retry attempts: only genuine failures (a 5xx, or a structured
  // error that isn't a rate limit). A 429 attempt is flow control, excluded.
  for (const a of (r.attempts || [])) {
    const as = a.status_code || 0;
    if (as === 429) continue; // flow control
    const t = a.error_type || (as ? 'http_' + as : 'transport');
    if (as < 500 && (t === 'transport' || t === 'rate_limit')) continue; // not a server failure
    if (as < 400 && !a.error_type) continue; // no failure at all
    const code = a.error_code || (as ? String(as) : '');
    out.push({ type: t, code, msg: a.error_msg, provider: r.provider, absorbed: true, at: a.at || r.start });
  }
  return out;
}

// ---------- targeted render (only re-render what changed) ----------
function updateSection(id, content) {
  const el = $(id);
  if (!el) return;
  const h = content;
  if (lastRender[id] === h) return;
  lastRender[id] = h;
  el.innerHTML = content;
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
