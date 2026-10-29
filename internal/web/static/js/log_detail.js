// Detail-view primitives shared by the request drawer and the debug capture:
// the one key/value row builder, section shell, header rows, formatted
// timestamps and the long-text code block with copy, wrap and expand controls.
// Record and attempt composition stays in log_drawer.js; capture composition
// stays in log_debug.js.

// detailSlug is the stable id/focus fragment for a section or field.
const detailSlug = s => String(s || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'section';

// detailFocusKey maps a focused control inside a repainted detail surface to
// its stable selector. Ids win; generated controls carry data-focus-key.
function detailFocusKey(el) {
  if (!el || !el.getAttribute) return null;
  if (el.id) return '#' + el.id;
  const key = el.getAttribute('data-focus-key');
  return key && /^[A-Za-z0-9:_-]+$/.test(key) ? `[data-focus-key="${key}"]` : null;
}

// fmtStamp renders a captured timestamp in local time with the full date and
// milliseconds. A missing, invalid or pre-1970 value renders nothing: it was
// never a real observation.
function fmtStamp(t) {
  if (!t) return '';
  const d = new Date(t);
  if (isNaN(d) || d.getTime() < 86400000) return '';
  const p = n => String(n).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ` +
    `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.` +
    String(d.getMilliseconds()).padStart(3, '0');
}

// detailTextRow is the one scalar key/value row descriptor. Values render
// escaped; an explicit copy:true (or a plain payload) adds the copy control,
// and text always feeds the section-level copy.
function detailTextRow(key, value, opts = {}) {
  if (value === null || value === undefined || value === '') return null;
  const text = String(value);
  return {k: key, v: escapeHtml(text), text, ...opts};
}

// detailHtmlRow is the row descriptor for a value already built from shared
// HTML helpers (status pills, entity badges, linked titles). Callers must pass
// the plain display text for section copy.
function detailHtmlRow(key, html, text, opts = {}) {
  if (html === null || html === undefined || html === '') return null;
  return {k: key, v: String(html), text: text == null ? '' : String(text), ...opts};
}

// detailBoolRow keeps the explicit false contract: a stored false is a real
// observation and renders as "false", never an absent row.
function detailBoolRow(key, value, opts = {}) {
  if (value === null || value === undefined) return null;
  return detailTextRow(key, value ? 'true' : 'false', opts);
}

// detailBytesRow formats a wire byte count and keeps the exact integer as the
// copy payload.
const detailBytesRow = (key, value) => {
  if (value === null || value === undefined) return null;
  return detailHtmlRow(key, vt(fmtBytes(value), fmtFull(value) + ' bytes'), fmtBytes(value),
    {copy: String(value), copyLabel: key + ' (exact bytes)'});
};

// detailNum renders an exact captured count with grouping. The compact hero is
// the only place a captured count is abbreviated; a section card keeps the full
// value and its exact copy payload.
const detailNum = (key, value, opts = {}) => (value === null || value === undefined) ? null
  : detailTextRow(key, fmtFull(value), {
    copy: String(value), copyLabel: key + ' (exact)', ...opts,
  });

// detailDurRow displays a compact duration while the hover title and copy
// payload keep the exact millisecond count.
function detailDurRow(key, value, opts = {}) {
  if (!value || !isFinite(value)) return null;
  return detailHtmlRow(key, vt(fmtDur(value), fmtFull(value) + ' ms'), String(value) + ' ms', {
    copyLabel: key + ' (exact ms)',
    ...opts,
    copy: opts.copy === false ? false : String(value) + ' ms',
  });
}

// detailMsRow renders an exact integer millisecond observation.
const detailMsRow = (key, value) => value
  ? detailTextRow(key, value + ' ms', {copy: String(value) + ' ms', copyLabel: key + ' (exact ms)'})
  : null;

// detailStampRow renders local time with milliseconds; the copy payload keeps
// the captured ISO instant so a correlation never depends on the display zone.
function detailStampRow(key, value, opts = {}) {
  const display = fmtStamp(value);
  if (!display) return null;
  return detailHtmlRow(key, display, display, {
    copyLabel: key + ' (ISO 8601)',
    ...opts,
    copy: opts.copy === false ? false : String(value),
  });
}

function detailRowHTML(row) {
  if (!row || row.v === '') return '';
  const kStyle = row.keyTone ? ` style="color:${row.keyTone}"` : '';
  const vStyle = row.tone ? ` style="color:${row.tone}"` : '';
  const payload = row.copy === true ? row.text : row.copy;
  const copy = payload ? copyButton(payload, row.copyLabel || row.k, row.focus) : '';
  return `<div class="detail-kv"><span class="k"${kStyle}>${escapeHtml(row.k)}</span>` +
    `<span class="v"${vStyle}>${row.v}</span>${copy}</div>`;
}

// detailCollapsed is the shared collapsed-section state for the open drawer.
// It is cleared when the drawer closes; live repaints re-apply it by key.
const detailCollapsed = new Set();

const SECTION_CHEV = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M4 6.5 8 10.5 12 6.5"/></svg>';

// detailSection wraps rows (or a prebuilt body) in one titled card. It drops
// an empty section, carries a section copy action, and keeps the collapse
// toggle outside the h4 so the heading text stays the section's own name.
function detailSection(title, rows, opts = {}) {
  const visible = (rows || []).filter(row => row && row.v !== '');
  const body = opts.body != null ? String(opts.body) : visible.map(detailRowHTML).join('');
  if (!body) return '';
  const key = opts.key || detailSlug(title);
  const id = opts.id || ('d-sec-' + key);
  const collapsed = detailCollapsed.has(key);
  // A string copy payload is the exact fact behind a formatted display (bytes,
  // milliseconds, ISO instants); copy:true rows already carry exact text.
  const payload = opts.copyText != null ? String(opts.copyText)
    : visible.map(row => `${row.k}: ${typeof row.copy === 'string' ? row.copy : row.text}`).join('\n');
  const copy = opts.copy === false || !payload ? '' : copyButton(payload, title, 'copy-sec:' + key);
  const toggle = `<button type="button" class="sec-toggle" data-section-toggle data-section-key="${escapeHtml(key)}" aria-expanded="${collapsed ? 'false' : 'true'}" aria-label="${collapsed ? 'expand' : 'collapse'} ${escapeHtml(title)}" title="${collapsed ? 'expand' : 'collapse'} ${escapeHtml(title)}" data-focus-key="sec:${escapeHtml(key)}">${SECTION_CHEV}</button>`;
  return `<section class="detail-section${collapsed ? ' collapsed' : ''}" id="${escapeHtml(id)}" data-title="${escapeHtml(title)}"${opts.navLabel ? ` data-nav="${escapeHtml(opts.navLabel)}"` : ''}>` +
    `<div class="detail-sec-hd" title="${escapeHtml(title)}"><h4>${escapeHtml(title)}</h4><div class="detail-sec-actions">` +
    `${opts.headExtra || ''}${copy}${toggle}</div></div>` +
    `<div class="detail-sec-body">${body}</div></section>`;
}

// detailHeaderRows turns a captured header map (one value per key in the wire
// model) into sorted copies of the shared row descriptor. Multi-value headers
// get one row per value and a unique focus key per row.
function detailHeaderRows(headers) {
  if (!headers || typeof headers !== 'object') return [];
  const rows = [];
  Object.keys(headers).sort().forEach(name => {
    const values = Array.isArray(headers[name]) ? headers[name] : [headers[name]];
    values.forEach((value, i) => rows.push(detailTextRow(name, value, {
      keyTone: 'var(--accent2)',
      copy: true,
      copyLabel: name,
      focus: 'hdr:' + detailSlug(name) + ':' + i,
    })));
  });
  return rows.filter(Boolean);
}

// detailCodeState keeps each block's wrap and expand choice across a live
// repaint. It is keyed by the stable block focus (prompt, response, debug
// request/response) and cleared with the drawer.
const detailCodeState = new Map();

// detailCodeBlock renders bounded long text as a scrollable code surface with
// a size/line readout, a wrap toggle, a copy action and an expand/collapse
// control for long bodies. It keeps the .preview-box class every existing
// capture reader and test already targets.
function detailCodeBlock(text, opts = {}) {
  const raw = String(text ?? '');
  if (!raw) return '';
  const lines = raw.split('\n').length;
  const long = raw.length > 1400 || lines > 16;
  const focus = 'code:' + (opts.focus || detailSlug(opts.label || 'body'));
  const saved = detailCodeState.get(focus);
  const collapsed = long && (saved ? saved.collapsed : opts.collapsed !== false);
  const nowrap = saved ? saved.nowrap : false;
  const label = opts.label || 'text';
  const meta = `${lines} line${lines === 1 ? '' : 's'}${opts.truncated ? ' · truncated' : ''}`;
  const bodyId = 'd-code-' + detailSlug(focus);
  const wrap = `<button type="button" class="mini-btn" data-code-wrap aria-pressed="${nowrap ? 'false' : 'true'}" aria-label="wrap lines" title="toggle line wrap" data-focus-key="${escapeHtml(focus)}:wrap">wrap</button>`;
  const copy = copyButton(raw, label, focus + ':copy');
  const toggle = long ? `<button type="button" class="mini-btn" data-code-toggle aria-expanded="${collapsed ? 'false' : 'true'}" aria-controls="${bodyId}" aria-label="${collapsed ? 'expand' : 'collapse'} ${escapeHtml(label)}" title="${collapsed ? 'expand' : 'collapse'} ${escapeHtml(label)}" data-focus-key="${escapeHtml(focus)}:toggle">${collapsed ? 'expand' : 'collapse'}</button>` : '';
  return `<div class="detail-code${collapsed ? ' collapsed' : ''}${nowrap ? ' nowrap' : ''}" data-code data-code-label="${escapeHtml(label)}" data-code-focus="${escapeHtml(focus)}">` +
    `<div class="detail-code-bar"><span class="detail-code-meta">${escapeHtml(meta)}</span>` +
    `<span class="detail-code-actions">${wrap}${copy}${toggle}</span></div>` +
    `<pre class="preview-box" id="${bodyId}" tabindex="0" data-focus-key="${escapeHtml(focus)}:body">${escapeHtml(raw)}</pre></div>`;
}

// detailCodeSection is the body section wrapper for a captured preview. The
// code block owns copy/wrap; the section owns collapse.
function detailCodeSection(title, text, opts = {}) {
  const raw = String(text ?? '');
  if (!raw) return '';
  return detailSection(title, [], {
    key: opts.key || detailSlug(title),
    copy: false,
    body: detailCodeBlock(raw, {label: title, truncated: opts.truncated, focus: opts.focus || detailSlug(title), collapsed: opts.collapsed}),
  });
}

// detailWireNav builds the sticky section jump strip from the sections that
// actually rendered. It runs after the drawer body commits, so conditional
// sections (Prompt, retries, Debug) never leave a dead chip behind.
function detailWireNav() {
  const body = $('drawer-body');
  if (!body) return;
  const old = body.querySelector('.d-jump');
  if (old) old.remove();
  const sections = [...body.querySelectorAll('.detail-section[data-title]')];
  if (sections.length < 3) return;
  const nav = document.createElement('nav');
  nav.className = 'd-jump';
  nav.setAttribute('aria-label', 'detail sections');
  nav.innerHTML = sections.map(sec =>
    `<button type="button" class="d-jump-btn" data-jump="${escapeHtml(sec.id)}" data-focus-key="jump:${escapeHtml(sec.id)}">${escapeHtml(sec.dataset.nav || sec.dataset.title || '')}</button>`
  ).join('');
  const first = sections[0];
  body.insertBefore(nav, first);
}

function detailToggleSection(btn) {
  const sec = btn && btn.closest('.detail-section');
  if (!sec) return;
  const collapsed = sec.classList.toggle('collapsed');
  const key = btn.dataset.sectionKey || detailSlug(sec.dataset.title);
  const title = sec.dataset.title || 'section';
  btn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
  btn.setAttribute('aria-label', (collapsed ? 'expand ' : 'collapse ') + title);
  btn.title = (collapsed ? 'expand ' : 'collapse ') + title;
  if (collapsed) detailCollapsed.add(key); else detailCollapsed.delete(key);
}

function detailJump(id) {
  const body = $('drawer-body');
  const sec = id ? document.getElementById(id) : null;
  if (!body || !sec || !body.contains(sec)) return;
  if (sec.classList.contains('collapsed')) {
    const toggle = sec.querySelector('[data-section-toggle]');
    if (toggle) detailToggleSection(toggle);
  }
  if (typeof sec.scrollIntoView === 'function') sec.scrollIntoView({block: 'start'});
  else if (body) body.scrollTop = sec.offsetTop - 8;
}

document.addEventListener('click', e => {
  const toggle = e.target.closest('[data-section-toggle]');
  if (toggle) { detailToggleSection(toggle); return; }
  const codeToggle = e.target.closest('[data-code-toggle]');
  if (codeToggle) {
    const block = codeToggle.closest('.detail-code');
    if (!block) return;
    const collapsed = block.classList.toggle('collapsed');
    codeToggle.textContent = collapsed ? 'expand' : 'collapse';
    codeToggle.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    const label = block.dataset.codeLabel || 'text';
    const verb = collapsed ? 'expand ' : 'collapse ';
    codeToggle.setAttribute('aria-label', verb + label);
    codeToggle.title = verb + label;
    const key = block.dataset.codeFocus;
    detailCodeState.set(key, {...detailCodeState.get(key), collapsed});
    return;
  }
  const wrap = e.target.closest('[data-code-wrap]');
  if (wrap) {
    const block = wrap.closest('.detail-code');
    if (!block) return;
    const wrapped = !block.classList.toggle('nowrap');
    wrap.setAttribute('aria-pressed', wrapped ? 'true' : 'false');
    const key = block.dataset.codeFocus;
    detailCodeState.set(key, {...detailCodeState.get(key), nowrap: !wrapped});
  }
});
