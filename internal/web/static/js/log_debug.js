// Request-log debug capture: the drawer's Debug section, its download
// affordance, the capture fetch that fills it, and the delegated download
// listener. log_detail.js owns the rows, sections and long-text block;
// log_drawer.js owns the drawer lifecycle that shows it.

// debugSectionHead renders the Debug heading row: the title, the download
// action and the message line. The first paint and fillDrawerDebug's rewrite
// both use it, so the button survives the capture render.
function debugSectionHead(id) {
  return `<div class="detail-sec-hd"><h4>Debug</h4><div class="detail-sec-actions">` +
    `<button type="button" class="copy-btn" id="drawer-debug-download" data-id="${escapeHtml(id)}" aria-label="download debug capture" title="download debug capture">⬇</button>` +
    `<span id="drawer-debug-status"></span></div></div>`;
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
  const box = $('drawer-debug-body');
  if (!box) return;
  operatorJson('/admin/debug/capture?id=' + encodeURIComponent(id))
    .then(d => {
      if (drawerId !== id) return;
      const ident = d.identity || {};
      const timing = d.timing || {};
      const outcome = d.outcome || {};
      const req = d.request || {};
      const resp = d.response || {};
      const bits = [];
      bits.push(detailSection('Identity', [
        detailTextRow('session', d.debug_session_id, {copy: true}),
        detailTextRow('client', ident.client),
        detailTextRow('provider', ident.provider),
        detailTextRow('model', ident.model, {copy: true}),
        detailTextRow('path', ident.path, {copy: true}),
        detailTextRow('stream', ident.stream ? 'yes' : 'no'),
        detailTextRow('expires', d.expires_at),
      ], {key: 'dbg-identity'}));
      bits.push(detailSection('Timing', [
        detailTextRow('start', timing.start),
        detailTextRow('duration', timing.duration_ms != null ? fmtDur(timing.duration_ms) : ''),
        detailTextRow('ttft', fmtTTFT(timing.ttft_ms)),
        detailTextRow('queue wait', timing.queue_wait_ms ? fmtDur(timing.queue_wait_ms) : ''),
      ], {key: 'dbg-timing'}));
      bits.push(detailSection('Request', [
        detailTextRow('url', req.url, {copy: true}),
        detailTextRow('status', outcome.status_code != null ? String(outcome.status_code) : ''),
        detailTextRow('finish', outcome.finish_reason),
      ], {key: 'dbg-request'}));
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
      const host = $('drawer-debug');
      if (host) {
        const focusToken = captureFocus(host, detailFocusKey);
        host.innerHTML = debugSectionHead(id) + `<div class="detail-sec-body">${bits.filter(Boolean).join('')}</div>`;
        if (focusToken) restoreFocus(focusToken);
      }
    })
    .catch(() => { box.textContent = 'capture unavailable'; });
}

// The debug section's Download button: delegated like the attempt links so
// re-renders never drop it. A native button carries its own keyboard
// activation, so no keydown arm is needed.
$('drawer-body').addEventListener('click', e => {
  const btn = e.target.closest('#drawer-debug-download');
  if (btn) runDebugCaptureDownload(btn.dataset.id);
});
