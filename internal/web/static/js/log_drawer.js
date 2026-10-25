// Request-log detail drawer: the key/value detail sections, the record and
// absorbed-attempt views, the drawer lifecycle and the retry-attempt link
// navigation. log.js owns the log rows and state pills that open it.

// Move the .expanding highlight in place (the class itself is owned by
// renderRequests; this avoids a full table re-render on drawer nav).
function highlightRow() {
  document.querySelectorAll('.exp-row[data-id]').forEach(tr => {
    tr.classList.toggle('expanding', tr.dataset.id === drawerId);
  });
}

// kv builds one key/value row; renders falsy-but-meaningful values (0, false)
// while skipping null/undefined/empty-string so the drawer shows every captured
// field without noise.
function kv(k, v, color) {
  if (v === null || v === undefined || v === '') return '';
  const c = color ? ` style="color:${color}"` : '';
  return `<div class="detail-kv"><span class="k">${k}</span><span class="v"${c}>${v}</span></div>`;
}
const kvBool = (k, v) => (v === null || v === undefined) ? '' : kv(k, v ? 'true' : 'false');
const fmtT = t => {
  if (!t) return '';
  const d = new Date(t);
  return (isNaN(d) || d.getTime() < 86400000) ? '' : d.toLocaleTimeString() + '.' + String(d.getMilliseconds()).padStart(3, '0');
};
// sec wraps a set of kv rows into a titled detail-drawer section, dropping the
// section entirely when every row is empty.
const sec = (title, rows) => (rows && rows.filter(Boolean).length) ? `<div class="detail-section"><h4>${title}</h4>${rows.join('')}</div>` : '';

// detailHeaderRows renders a captured response-header map as sorted key/value
// rows - the record's own section and the attempt view share the one pattern
// (multi-value headers get one row per value).
function detailHeaderRows(headers) {
  if (!headers || !Object.keys(headers).length) return '';
  const hrows = [];
  Object.entries(headers).sort().forEach(([k, vs]) => {
    vs.forEach(v => hrows.push(`<div class="detail-kv"><span class="k" style="color:var(--accent2)">${escapeHtml(k)}</span><span class="v">${escapeHtml(v)}</span></div>`));
  });
  return hrows.join('');
}

// formatDetail renders every captured field for a record, grouped into
// sections. The only thing intentionally absent is message content (which the
// proxy never stores unless capture_body_preview is on, and then only as a
// bounded preview).
function formatDetail(r) {
  const S = [];

  S.push(sec('Request', [
    kv('id', `<span style="color:var(--muted)">${escapeHtml(r.id)}</span>`),
    kv('status', r.live ? liveStatusCell(r) : finalizedStatusPill(r)),
    kv('started', fmtT(r.start)),
    kv('ended', fmtT(r.end)),
    kv('duration', r.duration_ms != null ? fmtDur(r.duration_ms) : ''),
    kv('request bytes', r.request_bytes),
    kv('response bytes', r.response_bytes),
  ]));

  S.push(sec('Client', [
    kv('client', entityBadge('client', r.client, null, r)),
    kv('ip', escapeHtml(r.client_ip)),
    kv('lang / runtime', escapeHtml(r.client_lang)),
    kv('sdk', escapeHtml(r.client_meta && r.client_meta.pkg_ver)),
    kv('runtime', escapeHtml([r.client_meta && r.client_meta.runtime, r.client_meta && r.client_meta.runtime_ver].filter(Boolean).join(' '))),
    kv('os', escapeHtml([r.client_meta && r.client_meta.os, r.client_meta && r.client_meta.arch].filter(Boolean).join(' '))),
    kv('user-agent', escapeHtml(r.user_agent)),
    kv('format', escapeHtml(r.client_meta && r.client_meta.format)),
    kv('timeout', r.client_meta && r.client_meta.timeout_ms ? fmtDur(r.client_meta.timeout_ms) : ''),
    kv('max concurrency', r.client_meta && r.client_meta.max_concurrency),
    kv('method', r.method),
    kv('path', escapeHtml(r.path)),
    kv('key hash', r.key_hash ? `<span style="color:var(--muted)">${escapeHtml(shortId('key', r.key_hash))}</span>` : ''),
    kvBool('client disconnected', r.client_disconnected),
  ]));

  S.push(sec('Provider', [
    kv('provider', entityBadge('provider', r.provider) + ` <span style="color:var(--muted)">${escapeHtml(r.provider)}</span>`),
    kv('model (upstream)', escapeHtml(r.provider_model)),
    kv('request id', escapeHtml(r.provider_request_id)),
    kv('server', escapeHtml(r.provider_server)),
    kv('processing', r.processing_ms ? r.processing_ms + ' ms' : ''),
  ]));

  // The full non-content request shape.
  S.push(sec('Parameters', [
    kv('model', escapeHtml(String(r.model || '')) +
      (String(r.model || '').trim() && canonicalModel(r.model) !== String(r.model).trim()
        ? ` <span style="color:var(--muted)">→ groups as ${escapeHtml(canonicalModel(r.model))}</span>`
        : '')),
    kvBool('stream', r.stream),
    kv('max_tokens', r.req_max_tokens),
    kv('temperature', r.req_temperature),
    kv('top_p', r.req_top_p),
    kv('n', r.req_n),
    kv('presence_penalty', r.req_presence_pen),
    kv('frequency_penalty', r.req_frequency_pen),
    kv('seed', r.req_seed),
    kv('stop sequences', r.req_stop),
    kvBool('logprobs', r.req_logprobs),
    kv('top_logprobs', r.req_top_logprobs),
    kv('logit_bias', r.req_logit_bias ? r.req_logit_bias + ' ids' : ''),
    kv('response_format', escapeHtml(r.req_response_format)),
    kv('service_tier', escapeHtml(r.req_service_tier)),
    kv('reasoning_effort', escapeHtml(r.req_reasoning_effort)),
    kv('verbosity', escapeHtml(r.req_verbosity)),
    kvBool('parallel_tool_calls', r.req_parallel_tools),
    // "thinking" reflects reality: the request flag, or (if unset) whether the
    // response actually produced reasoning tokens - some models reason without
    // an explicit thinking parameter (always-on, or reasoning_effort).
    r.req_thinking != null ? kv('thinking', r.req_thinking ? 'true' : 'false')
      : (r.usage?.reasoning_tokens ? kv('thinking', 'yes (not requested)', 'var(--warn)') : ''),
    kv('tools defined', r.req_tools_count),
    kv('tool_choice', escapeHtml(r.req_tool_choice)),
    kvBool('stream_options', r.req_stream_opts),
    kv('metadata', r.req_metadata_keys ? r.req_metadata_keys + ' keys' : ''),
  ]));

  S.push(sec('Conversation', [
    kv('user turns', r.turns_user),
    kv('assistant turns', r.turns_assistant),
    kv('tool turns', r.turns_tool),
    kv('responding to', escapeHtml(r.last_turn_role)),
    (r.chars_system || r.chars_user || r.chars_assistant || r.chars_tool)
      ? kv('prompt size', 'sys ' + fmt(r.chars_system || 0) + ' · user ' + fmt(r.chars_user || 0) + ' · asst ' + fmt(r.chars_assistant || 0) + ' · tool ' + fmt(r.chars_tool || 0)) : '',
    (r.images || r.attachments)
      ? kv('content parts', (r.images ? r.images + ' image' + (r.images > 1 ? 's' : '') : '') + (r.images && r.attachments ? ', ' : '') + (r.attachments ? r.attachments + ' attachment' + (r.attachments > 1 ? 's' : '') : '')) : '',
  ]));

  if (r.prompt_preview) S.push(`<div class="detail-section"><h4>Prompt</h4><div class="preview-box">${escapeHtml(r.prompt_preview)}</div></div>`);
  if (r.response_preview) S.push(`<div class="detail-section"><h4>Response</h4><div class="preview-box">${escapeHtml(r.response_preview)}</div></div>`);

  S.push(sec('Tokens', [
    kv('input', fmt(r.usage?.input_tokens)),
    kv('output', fmt(r.usage?.output_tokens)),
    kv('total', fmt(r.usage?.total_tokens)),
    kv('reasoning', r.usage?.reasoning_tokens ? fmt(r.usage.reasoning_tokens) : '', 'var(--warn)'),
    kv('answer', r.answer_tokens ? fmt(r.answer_tokens) : ''),
    kv('cache read', r.usage?.cache_read_tokens ? fmt(r.usage.cache_read_tokens) : '', 'var(--accent2)'),
    kv('cache write', r.usage?.cache_write_tokens ? fmt(r.usage.cache_write_tokens) : '', 'var(--accent2)'),
  ]));

  S.push(sec('Performance', [
    kv('ttft', fmtTTFT(r.ttft_ms)),
    kv('tps', r.gen_tps ? r.gen_tps.toFixed(1) + ' tok/s' : ''),
    kv('first token', fmtT(r.first_token_at)),
    kv('last token', fmtT(r.last_token_at)),
    kv('first thinking', fmtT(r.first_reasoning_at)),
    kv('first answer', fmtT(r.first_answer_at)),
    kv('chunks', r.chunks),
    kv('finish', escapeHtml(r.finish_reason)),
    kv('cost', r.cost ? fmtMoney(r.cost) : '', 'var(--warn)'),
    // The adopted attempt's transport decomposition (httptrace): plain
    // integer milliseconds where 0/absent means the hook never fired (pooled
    // connection, plaintext upstream) so the row hides; the pool-hit fact
    // renders through kvBool, so a stored false still shows like every
    // other record flag.
    kv('upstream ttfb', r.upstream_ttfb_ms ? r.upstream_ttfb_ms + ' ms' : ''),
    kv('upstream connect', r.upstream_connect_ms ? r.upstream_connect_ms + ' ms' : ''),
    kv('upstream tls', r.upstream_tls_ms ? r.upstream_tls_ms + ' ms' : ''),
    kvBool('reused connection', r.upstream_conn_reused),
  ]));

  if (r.tool_calls || (r.tool_names && r.tool_names.length)) {
    S.push(sec('Tools', [
      kv('calls', r.tool_calls, 'var(--cyan)'),
      kv('names', (r.tool_names||[]).map(escapeHtml).join(', '), 'var(--cyan)'),
    ]));
  }

  S.push(sec('Rate limit / queue', [
    kv('remaining', r.rate_limit_remaining),
    kv('limit', r.rate_limit_limit),
    kv('retry-after', r.retry_after_ms ? fmtDur(r.retry_after_ms) : '', 'var(--warn)'),
    kv('queue wait', r.queue_wait_ms ? fmtDur(r.queue_wait_ms) : '', 'var(--warn)'),
    kv('retries', r.retries, r.retries ? 'var(--warn)' : ''),
    kvBool('rate limited', r.rate_limited),
  ]));

  // Absorbed retry attempts: the upstream errors the client never saw. Show
  // each transparently-retried response (status + provider error detail) in
  // order, before the final outcome.
  const retryEntries = retryAttempts(r);
  if (retryEntries.length) {
    const items = retryEntries.map(({ a, index: i }, pos) => {
      // Transport failures have no HTTP status (0); the shared attemptPill
      // shows the transport pill instead of a meaningless "0".
      const isTransport = !a.status_code;
      const pill = attemptPill(a);
      const when = fmtT(a.at);
      // Type, code and message are distinct fields - show each on its own so a
      // code is never conflated with a message (a 502 is not always the same).
      // Skip the type text when it merely repeats the transport pill (a transport
      // failure's error_type is "transport" - showing it again reads as
      // "transport · transport").
      const parts = [];
      if (a.error_type && !(isTransport && a.error_type === 'transport')) parts.push(`<span style="color:var(--warn)">${escapeHtml(a.error_type)}</span>`);
      if (a.error_code) parts.push(`<span style="color:var(--muted)">code ${escapeHtml(a.error_code)}</span>`);
      if (a.error_msg) parts.push(escapeHtml(a.error_msg));
      const detail = parts.length ? parts.join(' · ') : '<span style="color:var(--muted)">no error body</span>';
      const ra = a.retry_after_ms ? ` <span style="color:var(--muted)">· retry-after ${fmtDur(a.retry_after_ms)}</span>` : '';
      // Each attempt stores the same upstream metadata as the final
      // response; the provider request id is the matching key between a
      // provider-side failure report and the exact absorbed attempt.
      const rid = a.provider_request_id ? ` <span style="color:var(--muted)">· req ${escapeHtml(a.provider_request_id)}</span>` : '';
      // Each attempt belongs to this same request id; data-attempt names its
      // index, so clicking opens THAT attempt in the drawer (the parent
      // request stays selected and highlighted).
      return `<div class="detail-kv attempt-link" data-open-req="${escapeHtml(r.id)}" data-attempt="${i}" role="button" tabindex="0" title="open this request"><span class="k">#${pos+1} <span style="color:var(--muted)">${when}</span></span><span class="v">${pill} ${detail}${ra}${rid}</span></div>`;
    }).join('');
    S.push(`<div class="detail-section"><h4>Retry attempts (absorbed, transparent to client)</h4>${items}</div>`);
  }

  if (r.error_type || r.error_msg || r.error_code) {
    S.push(`<div class="detail-section"><h4>Error</h4>` +
      kv('type', escapeHtml(r.error_type) || '-', 'var(--err)') +
      // code: provider's structured error.code, else the HTTP status.
      kv('code', escapeHtml(r.error_code || (r.status_code ? String(r.status_code) : '')) || '-', 'var(--err)') +
      kv('message', escapeHtml(r.error_msg) || '-') + `</div>`);
  }

  const hrows = detailHeaderRows(r.response_headers);
  // upstream gzip renders with the wire facts: the transport strips
  // Content-Encoding/Content-Length when it auto-decompresses, so the header
  // map alone cannot show the response was compressed on the wire.
  if (hrows || r.upstream_gzip) {
    S.push(`<div class="detail-section"><h4>Response headers</h4>` +
      kvBool('upstream gzip', r.upstream_gzip) + hrows + `</div>`);
  }

  if (r.debug) {
    S.push(`<div class="detail-section" id="drawer-debug">` + debugSectionHead(r.id) + `<div class="preview-box" id="drawer-debug-body">loading capture…</div></div>`);
  }

  return S.join('');
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
  S.push(sec('Attempt', [
    kv('request', `<span style="color:var(--muted)">${escapeHtml(r.id)}</span>`),
    kv('attempt', retryOrdinal(r, i) + ' of ' + retryAttempts(r).length),
    kv('status', attemptPill(a)),
    kv('when', fmtT(a.at)),
    // Type, code and message are distinct fields - one row each, like the
    // record's Error section. Skip the type text when it merely repeats the
    // transport pill (the retry-list convention).
    a.error_type && !(isTransport && a.error_type === 'transport')
      ? kv('type', escapeHtml(a.error_type), 'var(--err)') : '',
    a.error_code ? kv('code', escapeHtml(a.error_code), 'var(--err)') : '',
    a.error_msg ? kv('message', escapeHtml(a.error_msg), 'var(--err)') : '',
    kv('retry-after', a.retry_after_ms ? fmtDur(a.retry_after_ms) : '', 'var(--warn)'),
  ]));
  S.push(sec('Provider', [
    kv('request id', escapeHtml(a.provider_request_id)),
    kv('server', escapeHtml(a.provider_server)),
    kv('model (upstream)', escapeHtml(a.provider_model)),
    kv('processing', a.processing_ms ? a.processing_ms + ' ms' : ''),
  ]));
  S.push(sec('Rate limit', [
    kv('remaining', a.rate_limit_remaining),
    kv('limit', a.rate_limit_limit),
  ]));
  const hrows = detailHeaderRows(a.response_headers);
  if (hrows) S.push(`<div class="detail-section"><h4>Response headers</h4>${hrows}</div>`);
  return S.join('');
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
  const renderKey = String(drawerId) + '\u0000' + String(drawerAttempt);
  const scroll = body && _drawerRenderKey === renderKey ? body.scrollTop : 0;
  const base = reqIdentityText(r);
  if (a) {
    // The title carries the attempt identity: index, real status (transport
    // for status-less failures) and the provider request id that matches a
    // provider-side failure report to this exact attempt.
    const status = a.status_code ? String(a.status_code) : 'transport';
    const ident = ` · attempt ${retryOrdinal(r, drawerAttempt)} of ${retryAttempts(r).length} · ${status}`
      + (a.provider_request_id ? ` · ${a.provider_request_id}` : '');
    $('drawer-title').innerHTML = `<button type="button" class="btn" id="drawer-back" onclick="drawerBackToRequest()" title="back to request">‹ request</button> ${escapeHtml(base + ident)}`;
    body.innerHTML = formatAttemptDetail(r, drawerAttempt);
  } else {
    $('drawer-title').innerHTML = escapeHtml(base);
    body.innerHTML = formatDetail(r);
    if (r.debug) fillDrawerDebug(r.id);
  }
  body.scrollTop = scroll;
  _drawerRecord = r;
  _drawerRenderKey = renderKey;
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
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) openAttemptLink(el);
});
$('drawer-body').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const el = e.target.closest('.attempt-link[data-open-req]');
  if (el) { e.preventDefault(); openAttemptLink(el); }
});
