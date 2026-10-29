// Shared operator feedback: the clipboard copy path and the one polite toast
// line. Every dashboard surface that offers a copy action routes through
// copyText/copyButton so a secure-context Clipboard API, an insecure-context
// fallback and the visible outcome cannot drift between modules.

// COPY_ICON is the one copy glyph; the button carries its own accessible name.
const COPY_ICON = '<svg viewBox="0 0 16 16" aria-hidden="true"><rect x="5.5" y="5.5" width="8" height="8" rx="1.5"/><path d="M10.5 5.5v-2a1 1 0 0 0-1-1h-6a1 1 0 0 0-1 1v6a1 1 0 0 0 1 1h2"/></svg>';

// copyButton is the one copy affordance. The payload rides data-copy, the
// visible label is icon-only, and focusKey keeps keyboard focus on the same
// control across a live drawer repaint.
function copyButton(text, label, focusKey) {
  if (text === null || text === undefined || text === '') return '';
  const safeLabel = escapeHtml(String(label || 'value'));
  const key = focusKey ? ` data-focus-key="${escapeHtml(String(focusKey))}"` : '';
  return `<button type="button" class="copy-btn" data-copy="${escapeHtml(String(text))}" data-copy-label="${safeLabel}"${key} aria-label="copy ${safeLabel}" title="copy ${safeLabel}">${COPY_ICON}</button>`;
}

let _toastTimer = null;
// toast writes the one polite status line. A repeated message clears first so
// assistive technology announces it again; the timer is shared, so a new
// message can never be hidden by the previous one's timeout.
function toast(message, tone = '') {
  const el = $('toast');
  if (!el) return;
  el.classList.toggle('err', tone === 'err');
  el.textContent = '';
  el.textContent = String(message);
  el.classList.add('show');
  if (_toastTimer != null) clearTimeout(_toastTimer);
  _toastTimer = setTimeout(() => {
    el.classList.remove('show');
    _toastTimer = null;
  }, 1800);
}

// copyText prefers the async Clipboard API and falls back to the legacy
// selection copy for plain-HTTP LAN dashboards, where navigator.clipboard is
// undefined. It resolves false instead of throwing; the caller owns the
// visible failure message.
async function copyText(text) {
  const value = String(text ?? '');
  try {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      await navigator.clipboard.writeText(value);
      return true;
    }
  } catch (e) {
    // Permission denial or an insecure context: try the fallback below.
  }
  let area = null;
  try {
    area = document.createElement('textarea');
    area.value = value;
    area.setAttribute('readonly', '');
    area.style.position = 'fixed';
    area.style.top = '-1000px';
    area.style.opacity = '0';
    document.body.appendChild(area);
    area.select();
    area.setSelectionRange(0, value.length);
    return !!(typeof document.execCommand === 'function' && document.execCommand('copy'));
  } catch (e) {
    return false;
  } finally {
    if (area) area.remove();
  }
}

// One delegated listener owns every copy button. It works for rows rendered
// after load (debug captures) without per-render wiring, and it never lets a
// copy action double as a row-open click.
document.addEventListener('click', e => {
  const btn = e.target.closest('button[data-copy]');
  if (!btn || btn.disabled) return;
  e.preventDefault();
  const label = btn.dataset.copyLabel || 'value';
  copyText(btn.dataset.copy).then(ok => {
    if (!ok) { toast('copy failed', 'err'); return; }
    btn.classList.add('copied');
    setTimeout(() => btn.classList.remove('copied'), 1200);
    toast('copied ' + label);
  });
});
