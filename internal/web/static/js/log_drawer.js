// Request-log detail drawer: the record and absorbed-attempt composition, the
// hero summary, the error banner, the drawer lifecycle and the retry-attempt
// navigation. log_detail.js owns the row/section/code primitives; log_debug.js
// owns the capture section; log.js owns the log rows and state pills that open
// the drawer.

// Move the .expanding highlight in place (the class itself is owned by
// renderRequests; this avoids a full table re-render on drawer nav).
function highlightRow() {
  document.querySelectorAll('.exp-row[data-id]').forEach(tr => {
    tr.classList.toggle('expanding', tr.dataset.id === drawerId);
  });
}

const ALERT_ICON = '<svg class="d-alert-ico" viewBox="0 0 16 16" aria-hidden="true"><path d="M8 1.8 15 14H1L8 1.8Z"/><path d="M8 6v3.6M8 11.7v.1"/></svg>';

function detailStat(label, value, tone) {
  return `<span class="d-stat"><span class="d-stat-k">${escapeHtml(label)}</span>` +
    `<span class="d-stat-v"${tone ? ` style="color:${tone}"` : ''}>${escapeHtml(value)}</span></span>`;
}

// detailHero is the drawer's summary readout: what ran, where it ran and the
// few numbers an operator scans first. The full field inventory follows in
// sections, so the hero never hides a captured value.
function detailHero(r) {
  const status = r.live ? liveStatusCell(r)
    : (r.status_code !== null && r.status_code !== undefined ? finalizedStatusPill(r) : '');
  const method = r.method ? `<span class="d-method">${escapeHtml(r.method)}</span>` : '';
  const path = String(r.path || '');
  const pathHTML = path ? `<span class="d-path" title="${escapeHtml(path)}">${escapeHtml(path)}</span>` : '';
  const pathCopy = path ? copyButton(path, 'path', 'hero-path') : '';
  const ident = [];
  if (r.client) ident.push(entityBadge('client', r.client, null, r));
  if (r.provider) ident.push(entityBadge('provider', r.provider));
  const rawModel = String(r.model || '').trim();
  if (rawModel) {
    const canon = canonicalModel(rawModel);
    ident.push(`<span class="d-model">${escapeHtml(rawModel)}</span>` +
      (canon && canon !== rawModel ? ` <span class="d-canon">→ ${escapeHtml(canon)}</span>` : '') +
      copyButton(rawModel, 'model', 'hero-model'));
  }
  const stats = [];
  if (r.duration_ms !== null && r.duration_ms !== undefined) stats.push(detailStat('duration', fmtDur(r.duration_ms)));
  if (r.ttft_ms) stats.push(detailStat('ttft', fmtTTFT(r.ttft_ms)));
  if (r.usage && r.usage.total_tokens !== null && r.usage.total_tokens !== undefined) {
    stats.push(detailStat('tokens', fmt(r.usage.total_tokens)));
  }
  if (r.gen_tps) stats.push(detailStat('speed', r.gen_tps.toFixed(1) + ' tok/s'));
  if (r.cost) stats.push(detailStat('cost', fmtMoney(r.cost), 'var(--warn)'));
  if (r.retries) stats.push(detailStat('retries', String(r.retries), 'var(--warn)'));
  if (!status && !method && !pathHTML && !ident.length && !stats.length) return '';
  const pathRow = pathHTML ? `<span class="d-path-row">${pathHTML}${pathCopy}</span>` : '';
  return `<div class="d-hero">` +
    `<div class="d-hero-top">${status}${method}${pathRow}</div>` +
    (ident.length ? `<div class="d-hero-ident">${ident.join('<span class="d-sep" aria-hidden="true">·</span>')}</div>` : '') +
    (stats.length ? `<div class="d-stats">${stats.join('')}</div>` : '') +
    `</div>`;
}

// detailErrorAlert surfaces the final failure above the field inventory: type,
// code and message are distinct facts, and the copy action carries the whole
// block for a provider-side report. It is a plain group, not a live region, so
// a live repaint never re-announces the same failure.
function detailErrorAlert(r) {
  const err = finalErrorEntry(r);
  if (!err) return '';
  const type = String(err.type || '');
  const code = String(err.code || '');
  const message = String(err.msg || '');
  const status = r.status_code ? String(r.status_code) : '';
  const text = [
    type ? 'type: ' + type : '',
    code ? 'code: ' + code : '',
    message ? 'message: ' + message : '',
  ].filter(Boolean).join('\n');
  const title = (!r.status_code || r.status_code >= 400) ? 'request failed' : 'upstream error';
  return `<div class="d-alert" role="group" aria-label="error">` +
    `<div class="d-alert-hd">${ALERT_ICON}<span class="d-alert-title">${title}</span>` +
    (status && r.status_code >= 400 ? `<span class="pill err">${escapeHtml(status)}</span>` : '') +
    (type ? `<span class="d-alert-type">${escapeHtml(type)}</span>` : '') +
    (code && code !== status ? `<span class="d-alert-code">code ${escapeHtml(code)}</span>` : '') +
    copyButton(text, 'error detail', 'alert-copy') + `</div>` +
    (message ? `<div class="d-alert-msg">${escapeHtml(message)}</div>` : '') + `</div>`;
}

// formatDetail renders every captured field for a record, grouped into
// sections. The only thing intentionally absent is message content (which the
// proxy never stores unless capture_body_preview is on, and then only as a
// bounded preview).
function formatDetail(r) {
  const S = [];
  S.push(detailHero(r));
  S.push(detailErrorAlert(r));

  S.push(detailSection('Request', [
    detailTextRow('id', r.id, {tone: 'var(--muted)', copy: true, copyLabel: 'request id'}),
    detailStampRow('started', r.start),
    detailStampRow('ended', r.end),
    detailBytesRow('request bytes', r.request_bytes),
    detailBytesRow('response bytes', r.response_bytes),
  ], {key: 'request'}));

  const runtime = [r.client_meta && r.client_meta.runtime, r.client_meta && r.client_meta.runtime_ver].filter(Boolean).join(' ');
  const os = [r.client_meta && r.client_meta.os, r.client_meta && r.client_meta.arch].filter(Boolean).join(' ');
  const shortKey = r.key_hash ? shortId('key', r.key_hash) : '';
  S.push(detailSection('Client', [
    r.client ? detailHtmlRow('client', entityBadge('client', r.client, null, r), clientLabel(r.client), {copy: true}) : null,
    detailTextRow('ip', r.client_ip, {copy: true}),
    detailTextRow('lang / runtime', r.client_lang),
    detailTextRow('sdk', r.client_meta && r.client_meta.pkg_ver),
    detailTextRow('runtime', runtime),
    detailTextRow('os', os),
    detailTextRow('user-agent', r.user_agent, {copy: true}),
    detailTextRow('format', r.client_meta && r.client_meta.format),
    detailTextRow('timeout', r.client_meta && r.client_meta.timeout_ms ? fmtDur(r.client_meta.timeout_ms) : ''),
    detailTextRow('max concurrency', r.client_meta && r.client_meta.max_concurrency),
    detailTextRow('key hash', shortKey, {tone: 'var(--muted)', copy: shortKey || null}),
    detailBoolRow('client disconnected', r.client_disconnected),
  ], {key: 'client'}));

  S.push(detailSection('Provider', [
    r.provider ? detailHtmlRow('provider',
      entityBadge('provider', r.provider) + ` <span style="color:var(--muted)">${escapeHtml(r.provider)}</span>`,
      r.provider, {copy: true}) : null,
    detailTextRow('model (upstream)', r.provider_model, {copy: true}),
    detailTextRow('request id', r.provider_request_id, {copy: true}),
    detailTextRow('server', r.provider_server, {copy: true}),
    detailMsRow('processing', r.processing_ms),
  ], {key: 'provider'}));

  // The full non-content request shape.
  const modelRaw = String(r.model || '').trim();
  const modelCanon = modelRaw ? canonicalModel(modelRaw) : '';
  S.push(detailSection('Parameters', [
    detailHtmlRow('model', escapeHtml(modelRaw) +
      (modelCanon && modelCanon !== modelRaw ? ` <span style="color:var(--muted)">→ groups as ${escapeHtml(modelCanon)}</span>` : ''),
      modelRaw, {copy: modelRaw || null}),
    detailBoolRow('stream', r.stream),
    detailNum('max_tokens', r.req_max_tokens),
    detailTextRow('temperature', r.req_temperature),
    detailTextRow('top_p', r.req_top_p),
    detailNum('n', r.req_n),
    detailTextRow('presence_penalty', r.req_presence_pen),
    detailTextRow('frequency_penalty', r.req_frequency_pen),
    detailNum('seed', r.req_seed),
    detailTextRow('stop sequences', r.req_stop),
    detailBoolRow('logprobs', r.req_logprobs),
    detailNum('top_logprobs', r.req_top_logprobs),
    detailTextRow('logit_bias', r.req_logit_bias ? r.req_logit_bias + ' ids' : ''),
    detailTextRow('response_format', r.req_response_format, {copy: r.req_response_format || null}),
    detailTextRow('service_tier', r.req_service_tier),
    detailTextRow('reasoning_effort', r.req_reasoning_effort),
    detailTextRow('verbosity', r.req_verbosity),
    detailBoolRow('parallel_tool_calls', r.req_parallel_tools),
    // "thinking" reflects reality: the request flag, or (if unset) whether the
    // response actually produced reasoning tokens - some models reason without
    // an explicit thinking parameter (always-on, or reasoning_effort).
    r.req_thinking != null ? detailTextRow('thinking', r.req_thinking ? 'true' : 'false')
      : (r.usage && r.usage.reasoning_tokens ? detailTextRow('thinking', 'yes (not requested)', {tone: 'var(--warn)'}) : null),
    detailNum('tools defined', r.req_tools_count),
    detailTextRow('tool_choice', r.req_tool_choice, {copy: r.req_tool_choice || null}),
    detailBoolRow('stream_options', r.req_stream_opts),
    detailTextRow('metadata', r.req_metadata_keys ? r.req_metadata_keys + ' keys' : ''),
  ], {key: 'parameters'}));

  const promptSize = (r.chars_system || r.chars_user || r.chars_assistant || r.chars_tool)
    ? 'sys ' + fmt(r.chars_system || 0) + ' · user ' + fmt(r.chars_user || 0) +
      ' · asst ' + fmt(r.chars_assistant || 0) + ' · tool ' + fmt(r.chars_tool || 0)
    : '';
  const contentParts = (r.images || r.attachments)
    ? (r.images ? r.images + ' image' + (r.images > 1 ? 's' : '') : '') +
      (r.images && r.attachments ? ', ' : '') +
      (r.attachments ? r.attachments + ' attachment' + (r.attachments > 1 ? 's' : '') : '')
    : '';
  S.push(detailSection('Conversation', [
    detailNum('user turns', r.turns_user),
    detailNum('assistant turns', r.turns_assistant),
    detailNum('tool turns', r.turns_tool),
    detailTextRow('responding to', r.last_turn_role),
    detailTextRow('prompt size', promptSize),
    detailTextRow('content parts', contentParts),
  ], {key: 'conversation'}));

  S.push(detailCodeSection('Prompt', r.prompt_preview, {focus: 'prompt'}));
  S.push(detailCodeSection('Response', r.response_preview, {focus: 'response'}));

  const cacheHit = (r.usage && r.usage.input_tokens > 0 && r.usage.cache_read_tokens)
    ? pct2(r.usage.cache_read_tokens, r.usage.input_tokens) : '';
  S.push(detailSection('Tokens', [
    detailNum('input', r.usage && r.usage.input_tokens),
    detailNum('output', r.usage && r.usage.output_tokens),
    detailNum('total', r.usage && r.usage.total_tokens),
    r.usage && r.usage.reasoning_tokens ? detailTextRow('reasoning', fmt(r.usage.reasoning_tokens), {tone: 'var(--warn)'}) : null,
    r.answer_tokens ? detailTextRow('answer', fmt(r.answer_tokens)) : null,
    r.usage && r.usage.cache_read_tokens ? detailTextRow('cache read', fmt(r.usage.cache_read_tokens), {tone: 'var(--accent2)'}) : null,
    r.usage && r.usage.cache_write_tokens ? detailTextRow('cache write', fmt(r.usage.cache_write_tokens), {tone: 'var(--accent2)'}) : null,
    detailTextRow('cache hit', cacheHit, {tone: 'var(--accent2)'}),
  ], {key: 'tokens'}));

  S.push(detailSection('Performance', [
    detailTextRow('ttft', fmtTTFT(r.ttft_ms)),
    detailTextRow('tps', r.gen_tps ? r.gen_tps.toFixed(1) + ' tok/s' : ''),
    detailStampRow('first token', r.first_token_at),
    detailStampRow('last token', r.last_token_at),
    detailStampRow('first thinking', r.first_reasoning_at),
    detailStampRow('first answer', r.first_answer_at),
    detailNum('chunks', r.chunks),
    detailTextRow('finish', r.finish_reason),
    detailTextRow('cost', r.cost ? fmtMoney(r.cost) : '', {tone: 'var(--warn)'}),
    // The adopted attempt's transport decomposition (httptrace): plain
    // integer milliseconds where 0/absent means the hook never fired (pooled
    // connection, plaintext upstream) so the row hides; the pool-hit fact
    // renders through kvBool, so a stored false still shows like every
    // other record flag.
    detailMsRow('upstream ttfb', r.upstream_ttfb_ms),
    detailMsRow('upstream connect', r.upstream_connect_ms),
    detailMsRow('upstream tls', r.upstream_tls_ms),
    detailBoolRow('reused connection', r.upstream_conn_reused),
  ], {key: 'performance'}));

  if (r.tool_calls || (r.tool_names && r.tool_names.length)) {
    S.push(detailSection('Tools', [
      detailNum('calls', r.tool_calls, {tone: 'var(--cyan)'}),
      detailTextRow('names', (r.tool_names || []).join(', '), {tone: 'var(--cyan)'}),
    ], {key: 'tools'}));
  }

  S.push(detailSection('Rate limit / queue', [
    detailNum('remaining', r.rate_limit_remaining),
    detailNum('limit', r.rate_limit_limit),
    detailDurRow('retry-after', r.retry_after_ms, {tone: 'var(--warn)'}),
    detailDurRow('queue wait', r.queue_wait_ms, {tone: 'var(--warn)'}),
    detailNum('retries', r.retries, {tone: r.retries ? 'var(--warn)' : ''}),
    detailBoolRow('rate limited', r.rate_limited),
  ], {key: 'rate-limit'}));

  // Absorbed retry attempts: the upstream errors the client never saw. Show
  // each transparently-retried response (status + provider error detail) in
  // order, before the final outcome.
  const retryEntries = retryAttempts(r);
  if (retryEntries.length) {
    const copyLines = [];
    const items = retryEntries.map(({a, index: i}, pos) => {
      // Transport failures have no HTTP status (0); the shared attemptPill
      // shows the transport pill instead of a meaningless "0".
      const isTransport = !a.status_code;
      const pill = attemptPill(a);
      const when = fmtStamp(a.at);
      // Type, code and message are distinct fields - show each on its own so a
      // code is never conflated with a message (a 502 is not always the same).
      // Skip the type text when it merely repeats the transport pill (a transport
      // failure's error_type is "transport" - showing it again reads as
      // "transport · transport").
      const parts = [];
      if (a.error_type && !(isTransport && a.error_type === 'transport')) {
        parts.push(`<span style="color:var(--warn)">${escapeHtml(a.error_type)}</span>`);
      }
      if (a.error_code) parts.push(`<span style="color:var(--muted)">code ${escapeHtml(a.error_code)}</span>`);
      if (a.error_msg) parts.push(escapeHtml(a.error_msg));
      const detail = parts.length ? parts.join(' · ') : '<span class="d-muted">no error body</span>';
      const plainParts = [];
      if (a.error_type && !(isTransport && a.error_type === 'transport')) plainParts.push(a.error_type);
      if (a.error_code) plainParts.push('code ' + a.error_code);
      if (a.error_msg) plainParts.push(a.error_msg);
      const extras = [];
      if (a.retry_after_ms) extras.push(`retry-after ${fmtDur(a.retry_after_ms)}`);
      // Each attempt stores the same upstream metadata as the final
      // response; the provider request id is the matching key between a
      // provider-side failure report and the exact absorbed attempt.
      if (a.provider_request_id) extras.push(`req ${escapeHtml(a.provider_request_id)}`);
      copyLines.push(`#${pos + 1} ${when} ${a.status_code || 'transport'} ` +
        `${plainParts.length ? plainParts.join(' · ') : 'no error body'}` +
        `${a.retry_after_ms ? ' · retry-after ' + fmtDur(a.retry_after_ms) : ''}` +
        `${a.provider_request_id ? ' · req ' + a.provider_request_id : ''}`);
      // Each attempt belongs to this same request id; data-attempt names its
      // index, so clicking opens THAT attempt in the drawer (the parent
      // request stays selected and highlighted).
      return `<div class="attempt-link" data-open-req="${escapeHtml(r.id)}" data-attempt="${i}" role="button" tabindex="0" data-focus-key="attempt:${i}" title="open attempt ${pos + 1}">` +
        `<span class="attempt-ord">#${pos + 1}</span>` +
        `<span class="attempt-main">` +
        `<span class="attempt-head">${pill}${when ? `<span class="attempt-when">${escapeHtml(when)}</span>` : ''}</span>` +
        `<span class="attempt-detail">${detail}</span>` +
        (extras.length ? `<span class="attempt-extra">${extras.join(' · ')}</span>` : '') +
        `</span>` +
        `<span class="attempt-open" aria-hidden="true">›</span></div>`;
    }).join('');
    S.push(detailSection('Retry attempts (absorbed, transparent to client)', [], {
      key: 'retries', body: items, copyText: copyLines.join('\n'), navLabel: 'Retries',
    }));
  }

  const headerRows = detailHeaderRows(r.response_headers);
  // upstream gzip renders with the wire facts: the transport strips
  // Content-Encoding/Content-Length when it auto-decompresses, so the header
  // map alone cannot show the response was compressed on the wire.
  const gzipRow = detailBoolRow('upstream gzip', r.upstream_gzip);
  if (headerRows.length || gzipRow) {
    S.push(detailSection('Response headers', [gzipRow, ...headerRows], {key: 'headers'}));
  }

  if (r.debug) {
    S.push(`<section class="detail-section" id="drawer-debug" data-title="Debug">` +
      debugSectionHead(r.id) +
      `<div class="detail-sec-body"><div class="preview-box" id="drawer-debug-body">loading capture…</div></div>` +
      `</section>`);
  }

  return S.filter(Boolean).join('');
}

// formatAttemptDetail renders ONE absorbed attempt in the drawer: the
// per-attempt truth Go retains (RetryAttempt parity - status, timing, error
// detail, retry-after, upstream response metadata, rate-limit pair, its own
// response headers) plus the parent identity that keeps the view oriented.
// Record-level outcomes (usage, cost, TTFT, previews, the debug capture) have
// no attempt twin and must never appear here; the parent request owns them.
function formatAttemptDetail(r, i) {
  const a = r.attempts[i];
  const isTransport = !a.status_code;
  const S = [];
  S.push(detailSection('Attempt', [
    detailTextRow('request', r.id, {tone: 'var(--muted)', copy: true, copyLabel: 'request id'}),
    detailTextRow('attempt', retryOrdinal(r, i) + ' of ' + retryAttempts(r).length),
    detailHtmlRow('status', attemptPill(a), a.status_code ? String(a.status_code) : 'transport'),
    detailStampRow('when', a.at),
    // Type, code and message are distinct fields - one row each, like the
    // record's Error section. Skip the type text when it merely repeats the
    // transport pill (the retry-list convention).
    a.error_type && !(isTransport && a.error_type === 'transport')
      ? detailTextRow('type', a.error_type, {tone: 'var(--err)'}) : null,
    a.error_code ? detailTextRow('code', a.error_code, {tone: 'var(--err)'}) : null,
    a.error_msg ? detailTextRow('message', a.error_msg, {tone: 'var(--err)'}) : null,
    detailDurRow('retry-after', a.retry_after_ms, {tone: 'var(--warn)'}),
  ], {key: 'attempt'}));
  S.push(detailSection('Provider', [
    detailTextRow('request id', a.provider_request_id, {copy: true}),
    detailTextRow('server', a.provider_server, {copy: true}),
    detailTextRow('model (upstream)', a.provider_model, {copy: true}),
    detailMsRow('processing', a.processing_ms),
  ], {key: 'attempt-provider'}));
  S.push(detailSection('Rate limit', [
    detailNum('remaining', a.rate_limit_remaining),
    detailNum('limit', a.rate_limit_limit),
  ], {key: 'attempt-rate-limit'}));
  const headerRows = detailHeaderRows(a.response_headers);
  if (headerRows.length) S.push(detailSection('Response headers', headerRows, {key: 'attempt-headers'}));
  return S.filter(Boolean).join('');
}

// ---------- detail drawer ----------
// drawerShow owns the drawer's open tail: selection state, render, modal
// lifecycle and the parent row highlight. openDrawer shows the request's own
// detail; openDrawerAttempt shows one absorbed attempt (drawerId stays the
// parent record id, so the parent row keeps its highlight and the debug
// download-status guard stays keyed to the same record).
function drawerShow(id, attempt) {
  if (settingsIsOpen()) { closeSettings(); if (settingsIsOpen()) return; }
  // The initiating row control can be replaced by a live update while the
  // drawer is open; keep its stable selector so close can find the equivalent
  // control rather than falling back to BODY.
  const returnSelector = rowFocusKey(document.activeElement);
  drawerId = id;
  drawerAttempt = attempt;
  renderDrawer();
  if (!drawerId) return;
  openModal($('drawer'), {returnSelector, onPreempt: closeDrawer});
  $('drawer').classList.add('open');
  $('drawer-veil').classList.add('open');
  document.documentElement.classList.add('drawer-open');
  highlightRow();
}
function openDrawer(id) { drawerShow(id, null); }

// openDrawerAttempt opens one absorbed attempt of request id. The index
// arrives from a DOM dataset, so it is validated here: only the
// canonical digit spelling passes, then the integer and bound checks -
// bare Number coercion alone would let '' (0), '0x1' (1) and ' 1 ' (1)
// open an attempt. A malformed index fails closed to the request view.
// The re-render replaces the clicked control, so the attempt view's first
// control - the back affordance - takes focus, the same seat openModal
// gives a fresh open.
function openDrawerAttempt(id, index) {
  if (!/^\d+$/.test(String(index))) { openDrawer(id); return; }
  const i = Number(index);
  if (!Number.isInteger(i) || i < 0) { openDrawer(id); return; }
  drawerShow(id, i);
  const back = $('drawer-back');
  if (back) back.focus();
}

// drawerBackToRequest is the attempt view's title affordance: back to the
// request's own detail. The re-render removes the button, so the drawer's
// first control takes focus, the same seat a fresh open lands on.
function drawerBackToRequest() {
  if (drawerId == null) return;
  openDrawer(drawerId);
  const first = modalFocusables($('drawer'))[0];
  if (first) first.focus();
}
// _drawerRecord is the record object the drawer was last painted from;
// renderLive compares it to repaint only on a real live change. _drawerRenderKey
// is the (id, attempt) identity: scroll is preserved across a record change
// with the same identity and reset when the drawer navigates elsewhere.
let _drawerRecord = null;
let _drawerRenderKey = null;

function closeDrawer() {
  drawerId = null;
  drawerAttempt = null;
  _drawerRecord = null;
  _drawerRenderKey = null;
  if ($('drawer')) { $('drawer').classList.remove('open'); closeModal($('drawer')); }
  $('drawer-veil')?.classList.remove('open');
  document.documentElement.classList.remove('drawer-open');
  detailCollapsed.clear();
  detailCodeState.clear();
  // Release the previous request's rendered detail and debug capture: the
  // hidden drawer otherwise retained request/response bodies for the life of
  // the tab. The next open re-renders from its record.
  if ($('drawer-body')) $('drawer-body').innerHTML = '';
  if ($('drawer-title')) $('drawer-title').textContent = '';
  if ($('drawer-debug')) $('drawer-debug').innerHTML = '';
  highlightRow();
}
function drawerNav(dir) {
  // Same set + order as the request log: explorer scope + the grown window.
  // prev/next navigates requests; the attempt mode does not travel with it.
  const recs = logVisible();
  const i = recs.findIndex(r => r.id === drawerId);
  if (i < 0) return;
  const j = i + dir;
  if (j < 0 || j >= recs.length) return;
  drawerId = recs[j].id;
  drawerAttempt = null;
  renderDrawer();
  highlightRow();
}
function renderDrawer() {
  const r = recordById(drawerId);
  if (!r) { closeDrawer(); return; }
  // The attempt key re-resolves on every render from the live record: upserts
  // replace record objects wholesale and attempts are append-only, so a
  // captured attempt object would go stale as the array grows. A dangling key
  // (record left the window, or the index outran the array) fails closed to
  // the request view, never a throw.
  const a = drawerAttempt != null && Array.isArray(r.attempts) ? r.attempts[drawerAttempt] : null;
  if (drawerAttempt != null && !a) drawerAttempt = null;
  const body = $('drawer-body');
  const root = $('drawer');
  const renderKey = String(drawerId) + '\u0000' + String(drawerAttempt);
  const scroll = body && _drawerRenderKey === renderKey ? body.scrollTop : 0;
  // A live repaint replaces every row, so capture focus inside the drawer
  // before the rewrite and put it back on the equivalent control after.
  const focusToken = root ? captureFocus(root, detailFocusKey) : null;
  const base = reqIdentityText(r);
  if (a) {
    // The title carries the attempt identity: index, real status (transport
    // for status-less failures) and the provider request id that matches a
    // provider-side failure report to this exact attempt.
    const status = a.status_code ? String(a.status_code) : 'transport';
    const ident = ` · attempt ${retryOrdinal(r, drawerAttempt)} of ${retryAttempts(r).length} · ${status}`
      + (a.provider_request_id ? ` · ${a.provider_request_id}` : '');
    $('drawer-title').innerHTML = `<button type="button" class="btn drawer-back" id="drawer-back" onclick="drawerBackToRequest()" title="back to request" data-focus-key="drawer-back">‹ request</button>` +
      `<span class="d-title-text" title="${escapeHtml(base + ident)}">${escapeHtml(base + ident)}</span>`;
    body.innerHTML = formatAttemptDetail(r, drawerAttempt);
  } else {
    $('drawer-title').innerHTML = `<span class="d-title-text" title="${escapeHtml(base)}">${escapeHtml(base)}</span>`;
    body.innerHTML = formatDetail(r);
  }
  detailWireNav();
  body.scrollTop = scroll;
  if (focusToken) restoreFocus(focusToken);
  _drawerRecord = r;
  _drawerRenderKey = renderKey;
  if (!a && r.debug) fillDrawerDebug(r.id);
}

// drawerCopyRecord copies the selected record (or the selected absorbed
// attempt) as indented JSON. It reads the live record, never a captured one.
function drawerCopyRecord() {
  const r = recordById(drawerId);
  if (!r) return;
  const a = drawerAttempt != null && Array.isArray(r.attempts) ? r.attempts[drawerAttempt] : null;
  const payload = a ? {request_id: r.id, attempt_index: drawerAttempt, attempt: a} : r;
  let text = '';
  try { text = JSON.stringify(payload, null, 2); } catch (e) { text = ''; }
  if (!text) { toast('copy failed', 'err'); return; }
  copyText(text).then(ok => toast(ok ? 'copied request JSON' : 'copy failed', ok ? '' : 'err'));
}

// Absorbed retry attempts in the drawer are clickable: a link carrying an
// attempt index opens that attempt in the request's drawer; one without an
// index (deny by default) opens the request itself. Delegated on the drawer
// body so re-renders never drop the handler; keyboard-activatable too.
function openAttemptLink(el) {
  if (el.dataset.attempt !== undefined) openDrawerAttempt(el.dataset.openReq, el.dataset.attempt);
  else openDrawer(el.dataset.openReq);
}
$('drawer-body').addEventListener('click', e => {
  const jump = e.target.closest('[data-jump]');
  if (jump) { e.preventDefault(); detailJump(jump.dataset.jump); return; }
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) openAttemptLink(el);
});
$('drawer-body').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) { e.preventDefault(); openAttemptLink(el); }
});
