// Settings providers editor: the provider card grid, its usage/model field
// maps, cost-key chips, upstream header rows, and the provider alias editor.
// The two name grammars (PROV_PATH_RE for dotted JSON paths, PROV_HEADER_RE for
// RFC 7230 field names) are mirrored by the Go owners in internal/config.
// settings.js owns the accepted document and the shell; chrome_settings.js's
// SETTINGS_MODULES list owns the load order.

// ---- providers editor ----
// Field-name maps per provider: cost keys are JSON paths from the response
// root (chips), usage keys map a canonical token field to a custom path
// (canonical dropdown - the list is served by /admin/config usage_fields,
// owned by metrics.CanonicalUsageFields). Provider labels are the registrable
// domain of the upstream base URL; known labels from the pause API seed the
// suggestions. All events are delegated on #settings-fields (the sheet's
// innerHTML is rebuilt on every open/refetch, so per-element listeners would
// not survive a re-render).

// Dotted JSON paths only: what digJSON can actually resolve (object segments,
// numeric array segments). Deny anything else at the input.
const PROV_PATH_RE = /^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$/;
// RFC 7230 header token - the same shape the server's config validation
// enforces on providers.<label>.headers names.
const PROV_HEADER_RE = /^[!#$%&'*+\-.^_`|~0-9A-Za-z]+$/;
const USAGE_FIELDS_FALLBACK = ['input_tokens', 'output_tokens', 'total_tokens', 'cache_read_tokens', 'cache_write_tokens', 'reasoning_tokens'];
const MODEL_FIELDS_FALLBACK = ['context_length', 'context_window', 'max_output_tokens', 'input_modalities', 'output_modalities'];

function canonicalUsageFields() {
  const f = settingsDoc && settingsDoc.usage_fields;
  return Array.isArray(f) && f.length ? f : USAGE_FIELDS_FALLBACK;
}

function canonicalModelFields() {
  const f = settingsDoc && settingsDoc.model_fields;
  return Array.isArray(f) && f.length ? f : MODEL_FIELDS_FALLBACK;
}

function costChipHTML(path) {
  return `<span class="prov-chip" data-cost="${escapeHtml(path)}"><span class="prov-chip-p">${escapeHtml(path)}</span><button type="button" class="prov-x" data-prov-chip-rm aria-label="remove ${escapeHtml(path)}">✕</button></span>`;
}

// The provider card's remove icon is one 16x16 glyph authored as a markup
// constant (the trash geometry lost its header Clear button when deletion
// moved into the settings sheet). The constant carries no class; .prov-x svg
// owns this surface's stroke and size. The collapse chevron lives in
// core.js because the chart metrics picker also uses it before settings.js
// is loaded.
const PROV_TRASH_SVG = '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M3.2 5.4h9.6"/><path d="M6.1 5.4V4.3A1.2 1.2 0 0 1 7.3 3.1h1.4A1.2 1.2 0 0 1 9.9 4.3v1.1"/><path d="M4.2 5.4l.7 7.4a1 1 0 0 0 1 .8h5.2a1 1 0 0 0 1-.8l.7-7.4"/></svg>';

// mappingRowHTML renders one canonical-field → custom-path row for both
// structured maps. The two kinds differ only in wire keys: the select and
// input classes the delegated handlers and the value collector match on,
// the remove attribute, the aria-labels and the placeholders.
function mappingRowHTML(kind, field, path, fields) {
  const usage = kind === 'usage';
  const opts = settingsFieldOptions(fields, field);
  return `<div class="prov-urow"><select class="sp-${usage ? 'ufield' : 'mfield'}" aria-label="canonical ${usage ? 'token' : 'model'} field">${opts}</select><span class="prov-arrow" aria-hidden="true">→</span><input class="sp-${usage ? 'upath' : 'mkey'}" value="${escapeHtml(path == null ? '' : String(path))}" placeholder="${usage ? 'usage-relative path' : 'path inside the enrichment entry'}" aria-label="${usage ? 'usage key path' : 'model metadata path'}"><button type="button" class="prov-x" data-prov-${usage ? 'u' : 'm'}row-rm aria-label="remove mapping">✕</button></div>`;
}

function usageRowHTML(field, path, fields) { return mappingRowHTML('usage', field, path, fields); }
function modelRowHTML(field, path, fields) { return mappingRowHTML('model', field, path, fields); }

function settingsFieldOptions(fields, selected) {
  // Models enrichment can carry custom output keys. Existing names must
  // round-trip even when they are not offered as canonical new-row choices.
  const choices = fields.includes(selected) ? fields : fields.concat(selected);
  return choices.map(f => `<option value="${escapeHtml(f)}"${f === selected ? ' selected' : ''}>${escapeHtml(f)}</option>`).join('');
}

function providerCardHTML(label, p) {
  p = p || {};
  const costs = Array.isArray(p.cost_keys) ? p.cost_keys : [];
  const usage = p.usage_keys && typeof p.usage_keys === 'object' && !Array.isArray(p.usage_keys) ? p.usage_keys : {};
  const modelsPath = p.models_path == null ? '' : String(p.models_path);
  const modelsKeys = p.models_keys && typeof p.models_keys === 'object' && !Array.isArray(p.models_keys) ? p.models_keys : {};
  const ensureTools = Array.isArray(p.ensure_tools) ? p.ensure_tools.map(n => String(n)) : [];
  const headers = p.headers && typeof p.headers === 'object' && !Array.isArray(p.headers) ? p.headers : {};
  const fields = canonicalUsageFields();
  const used = new Set(Object.keys(usage));
  const free = fields.filter(f => !used.has(f));
  const mfields = canonicalModelFields();
  const mused = new Set(Object.keys(modelsKeys));
  const mfree = mfields.filter(f => !mused.has(f));
  // Section order is fixed: cost keys, then usage keys, then models
  // enrichment, then ensured tools, then upstream headers last. Every
  // section opens and closes inside the card - one missing close here
  // cascades the following sections into each other (the models section
  // once rendered nested inside usage keys).
  return `<div class="st-prov">` +
    `<div class="prov-hd"><span class="prov-ic" style="--ent:${ENTITY_TYPES.provider.color}" aria-hidden="true">☁</span><input class="sp-label" value="${escapeHtml(label)}" placeholder="provider label - registrable domain of the base URL, e.g. nano-gpt.com" aria-label="provider label"><button type="button" class="prov-chev" data-prov-collapse aria-expanded="true" aria-label="collapse or expand ${escapeHtml(label)}" title="collapse / expand">${PROV_CHEV_SVG}</button><button type="button" class="prov-x" data-prov-rm aria-label="remove provider" title="remove provider">${PROV_TRASH_SVG}</button></div>` +
    `<div class="prov-body">` +
    `<div class="prov-sec"><div class="prov-lb"><span>cost keys</span><span class="prov-sub">dotted JSON path from the response root - e.g. usage.cost</span></div>` +
    `<div class="prov-chips">${costs.map(costChipHTML).join('')}</div>` +
    `<div class="prov-add"><input class="sp-cost-in" placeholder="add cost key path… ↵" aria-label="add cost key"><button type="button" class="btn prov-addbtn" data-prov-add-cost aria-label="add cost key">+</button></div></div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>usage keys</span><span class="prov-sub">canonical token field → custom path</span></div>` +
    `<div class="prov-umap">${Object.entries(usage).map(([f, path]) => usageRowHTML(f, path, fields)).join('')}</div>` +
    (free.length ? `<div class="prov-add prov-add-u"><select class="sp-ufield-new" aria-label="canonical token field">${free.map(f => `<option value="${escapeHtml(f)}">${escapeHtml(f)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-usage aria-label="add usage mapping">+ map</button></div>` : '') +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>models enrichment</span><span class="prov-sub">metadata endpoint merged into this provider's /v1/models list</span></div>` +
    `<div class="prov-add"><input class="sp-mpath" value="${escapeHtml(modelsPath)}" placeholder="metadata path - e.g. /language-models" aria-label="models metadata path"></div>` +
    `<div class="prov-mmap">${Object.entries(modelsKeys).map(([f, path]) => modelRowHTML(f, path, mfields)).join('')}</div>` +
    (mfree.length ? `<div class="prov-add prov-add-m"><select class="sp-mfield-new" aria-label="canonical model field">${mfree.map(f => `<option value="${escapeHtml(f)}">${escapeHtml(f)}</option>`).join('')}</select><button type="button" class="btn prov-addbtn" data-prov-add-model aria-label="add model mapping">+ map</button></div>` : '') +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>ensured tools</span><span class="prov-sub">tool names relayed Chat Completions client bodies on a Chat outbound wire must carry - missing names are injected as inert stubs upstream</span></div>` +
    `<div class="prov-add"><input class="sp-etools" value="${escapeHtml(ensureTools.join(', '))}" placeholder="tool names, comma-separated - e.g. bash, read" aria-label="ensured tool names"></div>` +
    `</div>` +
    `<div class="prov-sec"><div class="prov-lb"><span>upstream headers</span><span class="prov-sub">extra headers sent upstream - {{uuid4}} / {{platform}} / {{opencode-msg-id}} / {{opencode-ses-id}} expand per request</span></div>` +
    `<div class="prov-hmap">${Object.entries(headers).map(([n, v]) => headerRowHTML(n, v)).join('')}</div>` +
    `<div class="prov-add prov-add-h"><input class="sp-hname" placeholder="header name… ↵" aria-label="upstream header name"><input class="sp-hval" placeholder="header value… ↵" aria-label="upstream header value"><button type="button" class="btn prov-addbtn" data-prov-add-header aria-label="add header">+</button></div>` +
    `</div>` +
    `</div>` +
    `</div>`;
}

function headerRowHTML(name, value) {
  return `<div class="prov-urow prov-hrow"><input class="sp-hname" value="${escapeHtml(name == null ? '' : String(name))}" placeholder="Header-Name" aria-label="upstream header name"><input class="sp-hval" value="${escapeHtml(value == null ? '' : String(value))}" placeholder="value - {{uuid4}} / {{platform}} expand" aria-label="upstream header value"><button type="button" class="prov-x" data-prov-hrow-rm aria-label="remove header">✕</button></div>`;
}

function providersEditorHTML(val) {
  const m = val && typeof val === 'object' && !Array.isArray(val) ? val : {};
  return Object.keys(m).map(k => providerCardHTML(k, m[k])).join('');
}

// ---- aliases editor ----
// provider_aliases: rows of [old label] → [canonical label]. Reuses the
// usage-row layout (.prov-urow) so both structured maps share one visual
// grammar. All events ride the same delegated #settings-fields wiring.
function aliasRowHTML(from, to) {
  return `<div class="prov-urow al-row"><input class="al-from" value="${escapeHtml(from)}" placeholder="old label - e.g. host.example.com" aria-label="old provider label"><span class="prov-arrow" aria-hidden="true">→</span><input class="al-to" value="${escapeHtml(to == null ? '' : String(to))}" placeholder="canonical label - e.g. example.com" aria-label="canonical provider label"><button type="button" class="prov-x" data-al-rm aria-label="remove alias">✕</button></div>`;
}

function aliasesEditorHTML(val) {
  const m = val && typeof val === 'object' && !Array.isArray(val) ? val : {};
  const rows = Object.keys(m).sort().map(k => aliasRowHTML(k, m[k])).join('');
  // .prov-sec wrapper imports the provider-editor visual system for the
  // inputs (mono, rail background, 7px radius, accent focus, dashed
  // add-row affordance) - without it the bare inputs fall back to browser
  // defaults and look nothing like the rest of the sheet.
  return `<div class="prov-sec">` +
    `<div class="al-rows">${rows}</div>` +
    `<div class="prov-add"><input class="al-new-from" placeholder="old label… ↵" aria-label="old provider label"><span class="prov-arrow" aria-hidden="true">→</span><input class="al-new-to" placeholder="canonical label… ↵" aria-label="canonical provider label"><button type="button" class="btn prov-addbtn" data-al-add aria-label="add alias">+</button></div>` +
    `</div>`;
}
