// Request-log debug capture: the drawer's debug section, its header and
// download affordance, the capture fetch that fills it, and the delegated
// download listener. log_drawer.js owns the drawer lifecycle that shows it.

// debugKV is the one debug key-value row builder. An empty value renders
// no row (deny by default); callers pass v pre-escaped, k is escaped here.
// color optionally tints the key span (capture headers use --accent2).
function debugKV(k, v, color) {
  if (v == null || v === '') return '';
  return `<div class="detail-kv"><span class="k"${color ? ` style="color:${color}"` : ''}>${escapeHtml(k)}</span><span class="v">${v}</span></div>`;
}

function debugHeaderRows(list) {
  if (!Array.isArray(list) || !list.length) return '';
  return list.map(h => debugKV(h.name || '', escapeHtml(h.value || ''), 'var(--accent2)')).join('');
}

// debugSectionHead renders the debug section header: the title, the
// download affordance and the message line. formatDetail's first paint and
// fillDrawerDebug's rewrite both render through it, so the button survives
// the capture render.
function debugSectionHead(id) {
  return `<h4>Debug <button type="button" class="btn" id="drawer-debug-download" data-id="${escapeHtml(id)}" aria-label="download debug capture" title="download debug capture">⬇</button> <span id="drawer-debug-status" style="color:var(--err)"></span></h4>`;
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
      bits.push(sec('Identity', [
        debugKV('session', escapeHtml(d.debug_session_id)),
        debugKV('client', escapeHtml(ident.client)),
        debugKV('provider', escapeHtml(ident.provider)),
        debugKV('model', escapeHtml(ident.model)),
        debugKV('path', escapeHtml(ident.path)),
        debugKV('stream', ident.stream ? 'yes' : 'no'),
        debugKV('expires', escapeHtml(d.expires_at)),
      ]));
      bits.push(sec('Timing', [
        debugKV('start', escapeHtml(timing.start)),
        debugKV('duration', timing.duration_ms != null ? fmtDur(timing.duration_ms) : ''),
        debugKV('ttft', fmtTTFT(timing.ttft_ms)),
        debugKV('queue wait', timing.queue_wait_ms ? fmtDur(timing.queue_wait_ms) : ''),
      ]));
      bits.push(sec('Request', [
        debugKV('url', escapeHtml(req.url)),
        debugKV('status', outcome.status_code != null ? String(outcome.status_code) : ''),
        debugKV('finish', escapeHtml(outcome.finish_reason)),
      ]));
      if (req.body && req.body.raw) {
        bits.push(`<div class="detail-section"><h4>Request body${req.body.truncated ? ' (truncated)' : ''}</h4><div class="preview-box">${escapeHtml(req.body.raw)}</div></div>`);
      }
      if (resp.body && resp.body.raw) {
        bits.push(`<div class="detail-section"><h4>Response body${resp.body.truncated ? ' (truncated)' : ''}</h4><div class="preview-box">${escapeHtml(resp.body.raw)}</div></div>`);
      }
      const rh = debugHeaderRows(req.headers);
      if (rh) bits.push(`<div class="detail-section"><h4>Request headers</h4>${rh}</div>`);
      const sh = debugHeaderRows(resp.headers);
      if (sh) bits.push(`<div class="detail-section"><h4>Response headers</h4>${sh}</div>`);
      const host = $('drawer-debug');
      if (host) host.innerHTML = debugSectionHead(id) + bits.filter(Boolean).join('');
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
