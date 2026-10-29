// Request-log debug capture: the drawer's Debug section, its download
// affordance, the capture fetch that fills it, and the delegated download
// listener. log_detail.js owns the rows, sections and long-text block;
// log_drawer.js owns the drawer lifecycle that shows it.

// _debugPendingFocus carries a focus token across the async capture fill: the
// card is replaced by its placeholder before the body controls exist again.
let _debugPendingFocus = null;
// _debugFillToken invalidates a capture fetch whose drawer was repainted or
// closed before it settled; the id alone cannot distinguish same-record paints.
let _debugFillToken = 0;

// debugSectionActions renders the Debug card's header extras: the download
// action and the message line. detailSection owns the title, collapse toggle
// and card shell, so the header survives fillDrawerDebug's body rewrite.
function debugSectionActions(id) {
  return `<button type="button" class="copy-btn" id="drawer-debug-download" data-id="${escapeHtml(id)}" aria-label="download debug capture" title="download debug capture">⬇</button>` +
    `<span id="drawer-debug-status"></span>`;
}

// debugSection renders the Debug card through the shared section owner. Its
// body starts as the capture placeholder; fillDrawerDebug replaces only that
// region, leaving the header, download action and collapse state intact.
function debugSection(id) {
  return detailSection('Debug', [], {
    key: 'debug',
    id: 'drawer-debug',
    group: 'diagnostics',
    copy: false,
    headExtra: debugSectionActions(id),
    body: `<div class="preview-box">loading capture…</div>`,
  });
}

// debugHeaderRows maps a captured [{name,value}] list onto the shared header
// row owner, keeping the accent key tint and denying empty values.
function debugHeaderRows(list) {
  if (!Array.isArray(list)) return [];
  return list.map((h, i) => detailTextRow(h && h.name, h && h.value, {
    keyTone: 'var(--accent2)',
    copy: true,
    copyLabel: (h && h.name) || 'header',
    focus: 'dbg-hdr:' + detailSlug((h && h.name) || 'header') + ':' + i,
  })).filter(Boolean);
}

// runDebugCaptureDownload serves the drawer's Download button: the capture
// artifact through the shared operator download owner, with the designed
// failure message surfaced in the section header (the backup pane's
// empty-body 502 convention).
function runDebugCaptureDownload(id) {
  downloadArtifact('/admin/debug/capture?id=' + encodeURIComponent(id) + '&download=1', 'millivolt-debug.json.gz', 'debug capture download failed')
    .then(() => debugDownloadStatus(id, ''))
    .catch(err => debugDownloadStatus(id, String(err.message || err)));
}

// debugDownloadStatus writes the section's message line only while the
// drawer still shows the record that started the download, so a stale
// result never lands on the next request's section.
function debugDownloadStatus(id, msg) {
  if (drawerId !== id) return;
  const el = $('drawer-debug-status');
  if (el) el.textContent = msg;
}

function fillDrawerDebug(id) {
  const host = $('drawer-debug');
  if (!host || !host.querySelector('.detail-sec-body')) return;
  const fillToken = ++_debugFillToken;
  operatorJson('/admin/debug/capture?id=' + encodeURIComponent(id))
    .then(d => {
      if (drawerId !== id || fillToken !== _debugFillToken) return;
      const ident = d.identity || {};
      const timing = d.timing || {};
      const outcome = d.outcome || {};
      const req = d.request || {};
      const resp = d.response || {};
      const usage = d.usage || {};
      const bits = [];
      bits.push(detailSection('Identity', [
        detailTextRow('session', d.debug_session_id, {copy: true}),
        detailTextRow('schema', d.schema),
        detailTextRow('captured', d.captured_at),
        detailTextRow('expires', d.expires_at),
        detailTextRow('client', ident.client),
        detailTextRow('provider', ident.provider),
        detailTextRow('model', ident.model, {copy: true}),
        detailTextRow('conversation', ident.conversation_id, {copy: true}),
        detailTextRow('key hash', ident.key_hash, {copy: true}),
        detailTextRow('method', ident.method),
        detailTextRow('path', ident.path, {copy: true}),
        detailTextRow('format', ident.format),
        detailTextRow('stream', ident.stream ? 'yes' : 'no'),
      ], {key: 'dbg-identity'}));
      bits.push(detailSection('Timing', [
        detailStampRow('start', timing.start),
        detailStampRow('end', timing.end),
        detailDurRow('duration', timing.duration_ms),
        detailTextRow('ttft', fmtTTFT(timing.ttft_ms)),
        detailDurRow('queue wait', timing.queue_wait_ms),
        timing.first_answer_ms ? detailDurRow('first answer', timing.first_answer_ms) : null,
      ], {key: 'dbg-timing'}));
      bits.push(detailSection('Request', [
        detailTextRow('url', req.url, {copy: true}),
        detailNum('status', outcome.status_code),
        detailTextRow('finish', outcome.finish_reason),
        outcome.error_type ? detailTextRow('error type', outcome.error_type, {tone: 'var(--warn)'}) : null,
        outcome.error_code ? detailTextRow('error code', outcome.error_code, {tone: 'var(--warn)'}) : null,
        outcome.error_msg ? detailTextRow('error message', outcome.error_msg, {tone: 'var(--warn)'}) : null,
        detailBoolRow('client disconnected', outcome.client_disconnected),
        detailNum('attempts', Array.isArray(d.attempts) ? d.attempts.length : null),
      ], {key: 'dbg-request'}));
      bits.push(detailSection('Usage', [
        detailNum('input', usage.input_tokens),
        detailNum('output', usage.output_tokens),
        detailNum('total', usage.total_tokens),
        detailNum('reasoning', usage.reasoning_tokens),
        detailNum('cache read', usage.cache_read_tokens),
        detailNum('cache write', usage.cache_write_tokens),
        d.cost ? detailTextRow('cost', fmtMoney(d.cost), {tone: 'var(--warn)'}) : null,
      ], {key: 'dbg-usage'}));
      if (req.body && req.body.raw) {
        bits.push(detailCodeSection('Request body' + (req.body.truncated ? ' (truncated)' : ''), req.body.raw,
          {key: 'dbg-req-body', focus: 'dbg-req-body'}));
      }
      if (resp.body && resp.body.raw) {
        bits.push(detailCodeSection('Response body' + (resp.body.truncated ? ' (truncated)' : ''), resp.body.raw,
          {key: 'dbg-resp-body', focus: 'dbg-resp-body'}));
      }
      bits.push(detailSection('Request headers', debugHeaderRows(req.headers), {key: 'dbg-req-headers'}));
      bits.push(detailSection('Response headers', debugHeaderRows(resp.headers), {key: 'dbg-resp-headers'}));
      const live = $('drawer-debug');
      const body = live && live.querySelector('.detail-sec-body');
      if (!body) return;
      const pending = _debugPendingFocus;
      _debugPendingFocus = null;
      const current = document.activeElement;
      // Restore the pre-repaint debug control only if the user has not moved
      // focus since; otherwise keep whatever control they chose.
      const focusToken = pending && (!current || current === document.body)
        ? pending : captureFocus(live, detailFocusKey);
      body.innerHTML = bits.filter(Boolean).join('');
      if (focusToken) restoreFocus(focusToken);
    })
    .catch(() => {
      if (drawerId !== id || fillToken !== _debugFillToken) return;
      const pending = _debugPendingFocus;
      _debugPendingFocus = null;
      const live = $('drawer-debug');
      const body = live && live.querySelector('.detail-sec-body');
      if (body) body.textContent = 'capture unavailable';
      if (pending) restoreFocus(pending);
    });
}

// The debug section's Download button: delegated like the attempt links so
// re-renders never drop it. A native button carries its own keyboard
// activation, so no keydown arm is needed.
$('drawer-body').addEventListener('click', e => {
  const btn = e.target.closest('#drawer-debug-download');
  if (btn) runDebugCaptureDownload(btn.dataset.id);
});
