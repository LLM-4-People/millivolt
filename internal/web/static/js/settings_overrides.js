// Settings request-overrides editor: ordered rules that rewrite the upstream
// request before relay, scoped by client, provider and model. Header set/
// replace/remove rides the providers editor's header-row grammar; the body
// ceilings apply to Chat Completions or Anthropic client bodies before
// translation. The rule cap, body band and forbidden-header vocabulary are
// mirrored by config's owners. settings.js owns the accepted document and the
// shell; chrome_settings.js's SETTINGS_MODULES list owns the load order.

// ---- request overrides editor ----
// request_overrides: ordered rules rewriting the upstream request before
// relay (headers set/replace/remove plus Chat or Anthropic client-body token
// ceilings), scoped per client, provider and/or model. One visual grammar with
// the other structured editors: each rule is a .prov-sec card inside an
// .mr-wrap shell (tools row, count, muted tokens), the headers mapping
// reuses the providers editor's headerRowHTML family verbatim (its add
// row, Enter key, remove buttons and change-time token lint all ride the
// existing delegated wiring), and remove_headers reuses the chip grammar.
// Live per-row validation mirrors config.validateRequestOverrides at every
// keystroke (the validateMrRow discipline): scope required, action
// required, RFC 7230 names, the credential/protocol/x-proxy- ownership
// set, one canonical name never both set and removed, the body band
// (1..REQUEST_OVERRIDE_BODY_MAX) and the duplicate-scope rejection. Apply
// walks the draft and blocks the POST on the first invalid card, focusing
// the offending input.
// Scope inputs get datalist autocomplete from the dashboard's known sets
// (the wire-key owners) plus the settings doc's provider labels; free text
// stays allowed because rules may target values not yet observed.
// Mirrors config.RequestOverridesMax (64) - the count line and template
// add-gate use it.
const REQUEST_OVERRIDES_MAX = 64;
// REQUEST_OVERRIDE_BODY_MAX mirrors config's body-ceiling band (the
// validateRequestOverrides 1..1_000_000 range): the body inputs' max
// attributes and the live band check share this one number. The
// cross-language pin in js_mirrors binds it to the config owner.
const REQUEST_OVERRIDE_BODY_MAX = 1000000;
// RO_FORBIDDEN_HEADERS mirrors config.forbiddenOverrideHeaders: the
// credential and protocol names the proxy or the transport owns, keyed
// lowercase because HTTP header names are case-insensitive. The x-proxy-
// control prefix is denied as a whole by the prefix check in
// roForbiddenReason. Deleting one of these is as breaking as rewriting it,
// so both of a rule's header lists share the grammar.
const RO_FORBIDDEN_HEADERS = {
  authorization: 'credential',
  cookie: 'credential',
  'proxy-authorization': 'credential',
  host: 'protocol',
  'content-type': 'protocol',
  'content-length': 'protocol',
  'transfer-encoding': 'protocol',
  te: 'protocol',
  connection: 'protocol',
  'keep-alive': 'protocol',
  'proxy-authenticate': 'protocol',
  'proxy-connection': 'protocol',
  trailer: 'protocol',
  upgrade: 'protocol',
};
// RO_TEMPLATES are the one-click starter rules (the mrTemplates precedent):
// payloads stay minimal so the live validation guides the missing scope. The
// budget template sets both token spellings to the same value: the cap pair
// is precedence-merged (max_completion_tokens wins over max_tokens), so a
// client request carrying either spelling would shrug off a rule that sets
// only the other one.
const RO_TEMPLATES = {
  blank: { label: 'empty rule', starter: {} },
  budget: { label: 'raise the output budget for a provider', starter: { body: { max_tokens: 32768, max_completion_tokens: 32768 } } },
  header: { label: 'set an upstream header for a client', starter: { headers: { 'X-Title': 'my app' } } },
};

// roForbiddenReason mirrors config.ForbiddenOverrideHeader: which owner
// denies a header name, or '' when the name is allowed.
function roForbiddenReason(name) {
  const reason = RO_FORBIDDEN_HEADERS[name.toLowerCase()];
  if (reason) return reason;
  if (name.toLowerCase().startsWith('x-proxy-')) return 'control';
  return '';
}

// roForbiddenMsg is the operator wording for a denied header name (the
// config.forbiddenOverrideHeaderError mirror): each reason explains who
// owns the header, so the fix is obvious from the message alone.
function roForbiddenMsg(name, field) {
  const reason = roForbiddenReason(name);
  if (reason === 'credential')
    return `${field}: '${name}' is credential-owned and cannot be overridden; the proxy injects the provider key itself`;
  if (reason === 'protocol')
    return `${field}: '${name}' is protocol-owned and cannot be overridden; the proxy and the HTTP transport manage it`;
  return `${field}: '${name}' carries the x-proxy- control prefix and cannot be overridden; x-proxy- headers are the proxy's own request channel`;
}

const RO_SERVER_SPACE = /[\u0009-\u000d\u0020\u0085\u00a0\u1680\u2000-\u200a\u2028-\u2029\u202f\u205f\u3000]/;
function trimServerSpace(value) {
  const text = String(value == null ? '' : value);
  let start = 0;
  let end = text.length;
  while (start < end && RO_SERVER_SPACE.test(text[start])) start++;
  while (end > start && RO_SERVER_SPACE.test(text[end - 1])) end--;
  return text.slice(start, end);
}

// roHeaderValueOK mirrors config.ValidHeaderValue plus the overrides'
// non-empty rule: no NUL/CR/LF or other control bytes, and not blank after
// trim. Tab is allowed like the server allows it.
function roHeaderValueOK(v) {
  const value = trimServerSpace(v);
  if (!value) return false;
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    if (c === 9 || (c >= 32 && c !== 127)) continue;
    return false;
  }
  return true;
}

// roKnownOptions derives one kind's autocomplete options: provider from
// the settings doc's provider labels plus the dashboard's known-providers
// sets, client and model from the dashboard's known sets (the wire-key
// owners - knownNames' union discipline across the operator states).
function roKnownOptions(kind) {
  const out = new Set();
  if (kind === 'provider') {
    const m = settingsDoc && settingsDoc.values && settingsDoc.values.providers;
    if (m && typeof m === 'object') for (const k of Object.keys(m)) if (k) out.add(String(k));
  }
  const key = kind === 'client' ? KNOWN_CLIENTS_KEY : kind === 'provider' ? KNOWN_PROVIDERS_KEY : KNOWN_MODELS_KEY;
  for (const st of [pauseState, debugState, throttleState]) {
    const list = st && st[key];
    if (Array.isArray(list)) for (const n of list) if (n) out.add(String(n));
  }
  return [...out].sort();
}

// roDlId owns the scope datalist id grammar shared by the generator, the
// rule card's three list attributes and the focus-time refresher, so the
// association cannot drift between render and refresh.
function roDlId(kind) {
  return 'ro-dl-' + kind;
}

// roDlOptionsHTML owns the datalist options expression shared by the
// initial render and the focus-time refresher: both spellings must derive
// the same option list from the same known sets.
function roDlOptionsHTML(kind) {
  return roKnownOptions(kind).map(o => `<option value="${escapeHtml(o)}"></option>`).join('');
}

function roDatalistHTML(kind) {
  return `<datalist id="${roDlId(kind)}">${roDlOptionsHTML(kind)}</datalist>`;
}

// roSyncDatalists refreshes the autocomplete options at focus time: the
// known sets arrive on their own refresh cadence, so options picked at
// render time can be stale while the sheet is open.
function roSyncDatalists(wrap) {
  if (!wrap) return;
  ['client', 'provider', 'model'].forEach(kind => {
    const dl = wrap.querySelector('#' + roDlId(kind));
    if (dl) dl.innerHTML = roDlOptionsHTML(kind);
  });
}

function roRemoveChipHTML(name) {
  return `<span class="prov-chip" data-rh="${escapeHtml(name)}"><span class="prov-chip-p">${escapeHtml(name)}</span><button type="button" class="prov-x" data-prov-chip-rm aria-label="remove ${escapeHtml(name)}">✕</button></span>`;
}

// roRuleLabel owns the rule-number label ("rule N", the 1-based index the
// server cites in request_overrides[i] errors) shared by the card render
// and roRenumber, so the initial label and the rewritten one cannot drift.
function roRuleLabel(i) {
  return 'rule ' + (i + 1);
}

// roRuleFieldLabel composes an ordinal-carrying input's accessible name
// from the same roRuleLabel ordinal the visible rule number uses.
function roRuleFieldLabel(i, suffix) {
  return roRuleLabel(i) + ' ' + suffix;
}

// roRuleFieldAttrs renders an ordinal-carrying input's accessible-name
// attributes: the initial aria-label composes through roRuleLabel, and
// data-ro-lbl carries the suffix roRenumber restamps the label from, so
// the rendered names and the rewritten ones share one composer and
// cannot drift after an add or remove.
function roRuleFieldAttrs(i, suffix) {
  return `aria-label="${escapeHtml(roRuleFieldLabel(i, suffix))}" data-ro-lbl="${escapeHtml(suffix)}"`;
}

let roRuleDomSeq = 0;

function roRuleDomToken() {
  return ++roRuleDomSeq;
}

function roRuleActionLabel(i, suffix) {
  return roRuleFieldLabel(i, suffix);
}

function roRuleActionAttrs(i, suffix) {
  const label = roRuleActionLabel(i, suffix);
  return `aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}" data-ro-action="${escapeHtml(suffix)}"`;
}

function roSummaryValue(value) {
  const text = trimServerSpace(value);
  if (!text) return 'any';
  if (text === 'any') return '"any" (exact)';
  return text === '*' ? '* (exact)' : text;
}

function roCountLabel(n, singular, plural) {
  return `${n} ${n === 1 ? singular : plural || singular + 's'}`;
}

function roScopeSummary(r) {
  r = r || {};
  return `client ${roSummaryValue(r.client)} · provider ${roSummaryValue(r.provider)} · model ${roSummaryValue(r.model)}`;
}

function roFormatActionSummary(headers, removes, body) {
  return `${roCountLabel(headers, 'header')} · ${roCountLabel(removes, 'remove header')} · ${roCountLabel(body, 'body ceiling')}`;
}

function roActionSummary(r) {
  r = r || {};
  const headers = r.headers && typeof r.headers === 'object' && !Array.isArray(r.headers) ? Object.keys(r.headers).length : 0;
  const removes = Array.isArray(r.remove_headers) ? r.remove_headers.length : 0;
  const body = r.body && typeof r.body === 'object' ? r.body : {};
  const bodyCount = ['max_tokens', 'max_completion_tokens'].filter(k => body[k] != null && trimServerSpace(body[k]) !== '').length;
  return roFormatActionSummary(headers, removes, bodyCount);
}

function roSyncSummary(card) {
  if (!card) return;
  const scope = roScopeSummary({
    client: (card.querySelector('.ro-client') || {}).value,
    provider: (card.querySelector('.ro-provider') || {}).value,
    model: (card.querySelector('.ro-model') || {}).value,
  });
  const headers = card.querySelectorAll('.prov-hmap .prov-hrow').length;
  const removes = card.querySelectorAll('.prov-chips .prov-chip').length;
  const body = [...card.querySelectorAll('.ro-max-tokens, .ro-max-mct')].filter(input => trimServerSpace(input.value)).length;
  const actions = roFormatActionSummary(headers, removes, body);
  const scopeEl = card.querySelector('.ro-summary-scope');
  const actionEl = card.querySelector('.ro-summary-actions');
  if (scopeEl) { scopeEl.textContent = scope; scopeEl.title = scope; }
  if (actionEl) { actionEl.textContent = actions; actionEl.title = actions; }
  if (card.hasAttribute('tabindex')) {
    const i = Number(card.dataset.roIndex || 0);
    card.setAttribute('aria-label', `${roRuleLabel(i)}: ${scope}; ${actions}`);
  }
}

function roSetCardExpanded(card, expanded) {
  if (!card) return;
  const open = !!expanded;
  card.dataset.roExpanded = open ? '1' : '0';
  const body = card.querySelector('.ro-rule-body');
  const toggle = card.querySelector('[data-ro-toggle]');
  if (body) body.hidden = !open;
  if (!toggle) return;
  toggle.setAttribute('aria-expanded', String(open));
  if (body) toggle.setAttribute('aria-controls', body.id);
  const action = open ? 'collapse rule details' : 'expand rule details';
  const label = roRuleActionLabel(Number(card.dataset.roIndex || 0), action);
  toggle.setAttribute('aria-label', label);
  toggle.title = label;
  const caret = toggle.querySelector('.ro-disclosure-caret');
  if (caret) caret.textContent = open ? '▾' : '▸';
}

function roSetActionLabel(el, i, suffix) {
  if (!el) return;
  const label = roRuleActionLabel(i, suffix);
  el.setAttribute('aria-label', label);
  el.title = label;
}

function roSyncActionLabels(card, i) {
  if (!card) return;
  card.querySelectorAll('[data-ro-action]').forEach(el => {
    if (!el.matches('[data-ro-toggle]')) roSetActionLabel(el, i, el.dataset.roAction);
  });
  roSetActionLabel(card.querySelector('[data-prov-add-header]'), i, 'add header');
  roSetActionLabel(card.querySelector('[data-ro-rh-add]'), i, 'add remove header');
  roSetActionLabel(card.querySelector('.ro-rh-in'), i, 'add remove header');
  card.querySelectorAll('.prov-add-h .sp-hname').forEach(el => roSetActionLabel(el, i, 'add header name'));
  card.querySelectorAll('.prov-add-h .sp-hval').forEach(el => roSetActionLabel(el, i, 'add header value'));
  card.querySelectorAll('.prov-hrow .sp-hname').forEach(el => roSetActionLabel(el, i, 'upstream header name'));
  card.querySelectorAll('.prov-hrow .sp-hval').forEach(el => roSetActionLabel(el, i, 'upstream header value'));
  card.querySelectorAll('.prov-hrow [data-prov-hrow-rm]').forEach(el => {
    const name = trimServerSpace((el.closest('.prov-hrow').querySelector('.sp-hname') || {}).value) || 'header';
    roSetActionLabel(el, i, `remove ${name}`);
  });
  card.querySelectorAll('.prov-chip [data-prov-chip-rm]').forEach(el => {
    const name = trimServerSpace((el.closest('.prov-chip') || {}).dataset?.rh) || 'header';
    roSetActionLabel(el, i, `remove ${name}`);
  });
  roSetCardExpanded(card, card.dataset.roExpanded === '1');
}

function roRuleCardHTML(r, i, expanded = false) {
  r = r || {};
  const headers = r.headers && typeof r.headers === 'object' && !Array.isArray(r.headers) ? r.headers : {};
  const removes = Array.isArray(r.remove_headers) ? r.remove_headers : [];
  const body = r.body && typeof r.body === 'object' ? r.body : {};
  const open = !!expanded;
  const domToken = roRuleDomToken();
  const bodyId = `ro-rule-body-${domToken}`;
  const errorId = `ro-rule-error-${domToken}`;
  const scope = roScopeSummary(r);
  const actions = roActionSummary(r);
  return `<div class="prov-sec ro-rule" role="group" tabindex="-1" data-ro-index="${i}" data-ro-expanded="${open ? '1' : '0'}" aria-label="${escapeHtml(roRuleLabel(i) + ': ' + scope + '; ' + actions)}">` +
    `<div class="ro-rule-head">` +
    `<button type="button" class="ro-disclosure" data-ro-toggle ${roRuleActionAttrs(i, 'toggle')} aria-expanded="${open}" aria-controls="${bodyId}"><span class="ro-disclosure-caret" aria-hidden="true">${open ? '▾' : '▸'}</span></button>` +
    `<span class="ro-num">${escapeHtml(roRuleLabel(i))}</span>` +
    `<div class="ro-summary"><span class="ro-summary-scope" title="${escapeHtml(scope)}">${escapeHtml(scope)}</span><span class="ro-summary-actions" title="${escapeHtml(actions)}">${escapeHtml(actions)}</span></div>` +
    `<span class="ro-head-actions"><button type="button" class="ro-action" data-ro-up ${roRuleActionAttrs(i, 'move up')}>Move up</button><button type="button" class="ro-action" data-ro-dn ${roRuleActionAttrs(i, 'move down')}>Move down</button><button type="button" class="prov-x ro-action" data-ro-rm ${roRuleActionAttrs(i, 'remove rule')}>Remove</button></span>` +
    `</div>` +
    `<div class="ro-rule-body" id="${bodyId}" role="region" aria-label="${escapeHtml(roRuleLabel(i) + ' details')}"${open ? '' : ' hidden'}>` +
    `<div class="prov-lb"><span>scope</span><span class="prov-sub">an empty field matches any request</span></div>` +
    `<div class="st-ctl-line ro-scope">` +
    `<label class="ro-field"><span>client</span><input class="ro-client" list="${roDlId('client')}" value="${escapeHtml(r.client || '')}" placeholder="any client - e.g. claude-code" ${roRuleFieldAttrs(i, 'client scope')}></label>` +
    `<label class="ro-field"><span>provider</span><input class="ro-provider" list="${roDlId('provider')}" value="${escapeHtml(r.provider || '')}" placeholder="any provider - e.g. nano-gpt.com" ${roRuleFieldAttrs(i, 'provider scope')}></label>` +
    `<label class="ro-field"><span>model</span><input class="ro-model" list="${roDlId('model')}" value="${escapeHtml(r.model || '')}" placeholder="any model - e.g. glm-5.3" ${roRuleFieldAttrs(i, 'model scope')}></label>` +
    `</div>` +
    `<div class="prov-lb"><span>headers</span><span class="prov-sub">set or replace upstream headers - e.g. X-Title, my app</span></div>` +
    `<div class="prov-hmap">${Object.entries(headers).map(([n, v]) => headerRowHTML(n, v)).join('')}</div>` +
    `<div class="prov-add prov-add-h"><input class="sp-hname" placeholder="header name… ↵" aria-label="add header name"><input class="sp-hval" placeholder="value - e.g. my-app/1.0 ↵" aria-label="add header value"><button type="button" class="btn prov-addbtn" data-prov-add-header aria-label="add header">+</button></div>` +
    `<div class="prov-lb"><span>remove headers</span><span class="prov-sub">delete upstream headers by name - e.g. User-Agent</span></div>` +
    `<div class="prov-chips">${removes.map(n => roRemoveChipHTML(String(n == null ? '' : n))).join('')}</div>` +
    `<div class="prov-add"><input class="ro-rh-in" placeholder="header name to remove… ↵" aria-label="add header to remove"><button type="button" class="btn prov-addbtn" data-ro-rh-add aria-label="add header to remove">+</button></div>` +
    `<div class="prov-lb"><span>body</span><span class="prov-sub">output-token ceilings on Chat Completions or Anthropic client bodies, before translation - empty leaves the request's own value</span></div>` +
    `<div class="st-ctl-line ro-body">` +
    `<label class="ro-field"><span>max_tokens</span><input class="ro-max-tokens" type="number" min="1" max="${REQUEST_OVERRIDE_BODY_MAX}" step="1" value="${escapeHtml(body.max_tokens == null ? '' : String(body.max_tokens))}" placeholder="max_tokens - e.g. 32768" ${roRuleFieldAttrs(i, 'max_tokens ceiling')}></label>` +
    `<label class="ro-field"><span>max_completion_tokens</span><input class="ro-max-mct" type="number" min="1" max="${REQUEST_OVERRIDE_BODY_MAX}" step="1" value="${escapeHtml(body.max_completion_tokens == null ? '' : String(body.max_completion_tokens))}" placeholder="max_completion_tokens - e.g. 32768" ${roRuleFieldAttrs(i, 'max_completion_tokens ceiling')}></label>` +
    `</div>` +
    `<div class="mr-err ro-err" id="${errorId}" role="status" aria-live="polite" aria-atomic="true"></div>` +
    `</div></div>`;
}

function requestOverridesEditorHTML(val) {
  const rules = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap ro-wrap">` +
    `<div class="mr-hint">each rule rewrites the upstream request before relay · a rule matches when every non-empty scope field equals the request's value exactly · all matching rules apply in list order, the later rule wins · headers set or replace, remove_headers deletes, body stamps output-token ceilings</div>` +
    `<div class="mr-tools"><div class="ro-add"><button type="button" class="btn ro-add-btn" data-ro-add aria-label="add rule" aria-expanded="false" aria-controls="ro-template-menu" title="add a request-override rule from a template">+ add rule</button><div class="ro-tpl-menu" id="ro-template-menu" data-ro-tpl-menu hidden>${Object.entries(RO_TEMPLATES).map(([kind, template]) => `<button type="button" class="ro-tpl-item" data-ro-tpl="${kind}" title="${escapeHtml(template.label)}">${escapeHtml(template.label)}</button>`).join('')}</div></div><span class="mr-count muted"></span></div>` +
    `<div class="ro-rows">${rules.map((r, i) => roRuleCardHTML(r, i)).join('')}</div>` +
    roDatalistHTML('client') + roDatalistHTML('provider') + roDatalistHTML('model') +
    `</div>`;
}

function roSetTemplateMenuOpen(menu, open) {
  if (!menu) return;
  if (menu._roCloseTimer) { clearTimeout(menu._roCloseTimer); menu._roCloseTimer = null; }
  menu.hidden = !open;
  const btn = menu.parentElement && menu.parentElement.querySelector('[data-ro-add]');
  if (btn) btn.setAttribute('aria-expanded', String(!!open));
}

function closeRoTemplateMenus(scope) {
  (scope || document).querySelectorAll('.ro-tpl-menu:not([hidden])').forEach(menu => roSetTemplateMenuOpen(menu, false));
}

function roApplyTemplate(source) {
  const kind = String((source && source.dataset && source.dataset.roTpl) || '');
  const wrap = source && source.closest ? source.closest('.ro-wrap') : null;
  if (!wrap) return;
  closeRoTemplateMenus(wrap);
  const template = RO_TEMPLATES[kind];
  if (!template || wrap.querySelectorAll('.ro-rule').length >= REQUEST_OVERRIDES_MAX) return;
  const rows = wrap.querySelector('.ro-rows');
  rows.insertAdjacentHTML('beforeend', roRuleCardHTML(template.starter, rows.children.length, true));
  roDraftChanged(wrap);
  const card = [...wrap.querySelectorAll('.ro-rule')].pop();
  const focus = card && card.querySelector(kind === 'budget' ? '.ro-provider' : '.ro-client');
  if (focus) focus.focus();
}

// addRoRemoveHeader appends one remove_headers chip from the add input.
// Deny by default on the token grammar (flash instead of adding a chip the
// server would refuse); ownership and cross-list conflicts surface through
// the live validation with their full messages.
function addRoRemoveHeader(card) {
  const input = card && card.querySelector('.ro-rh-in');
  const chips = card && card.querySelector('.prov-chips');
  if (!input || !chips) return;
  const name = trimServerSpace(input.value);
  if (!name || !PROV_HEADER_RE.test(name)) { flashBadInput(input); return; }
  if ([...chips.querySelectorAll('.prov-chip')].some(c => trimServerSpace(c.dataset.rh || '').toLowerCase() === name.toLowerCase())) { flashBadInput(input); return; }
  chips.insertAdjacentHTML('beforeend', roRemoveChipHTML(name));
  input.value = '';
  roDraftChanged(card.closest('.ro-wrap'));
}

function validateRoRow(card, seen, index) {
  const err = card.querySelector('.ro-err');
  if (err) err.replaceChildren();
  else card.querySelectorAll('.ro-error-detail').forEach(detail => detail.remove());
  card.querySelectorAll('.prov-bad').forEach(el => el.classList.remove('prov-bad'));
  card.querySelectorAll('input, button').forEach(el => {
    el.removeAttribute('aria-invalid');
    el.removeAttribute('aria-describedby');
  });
  const detailByControl = new Map();
  let detailIndex = 0;
  const scopeVal = sel => trimServerSpace((card.querySelector(sel) || {}).value);
  const client = scopeVal('.ro-client');
  const provider = scopeVal('.ro-provider');
  const model = scopeVal('.ro-model');
  const headerRows = [...card.querySelectorAll('.prov-hmap .prov-hrow')];
  const removeChips = [...card.querySelectorAll('.prov-chips .prov-chip')].map(chip => ({
    name: trimServerSpace(chip.dataset.rh || ''),
    button: chip.querySelector('[data-prov-chip-rm]'),
  }));
  const bodyFields = [
    ['max_tokens', card.querySelector('.ro-max-tokens')],
    ['max_completion_tokens', card.querySelector('.ro-max-mct')],
  ];
  let level = 'ok';
  const fail = (input, text) => {
    if (input) {
      input.classList.add('prov-bad');
      input.setAttribute('aria-invalid', 'true');
      if (err && err.id && !detailByControl.has(input)) {
        const detail = document.createElement('div');
        detail.className = 'ro-error-detail';
        detail.id = `${err.id}-detail-${detailIndex++}`;
        detail.textContent = text;
        (err || card).appendChild(detail);
        detailByControl.set(input, detail.id);
        input.setAttribute('aria-describedby', detail.id);
      }
    }
    if (level !== 'error') level = 'error';
  };
  if (!client && !provider && !model) {
    ['.ro-client', '.ro-provider', '.ro-model'].forEach(sel => fail(card.querySelector(sel),
      'no scope set - give the rule a client, provider or model, or remove the rule'));
  } else if (!headerRows.length && !removeChips.length && !bodyFields.some(([, input]) => input && trimServerSpace(input.value))) {
    fail(card.querySelector('.prov-add-h .sp-hname'),
      'no action set - give the rule a headers entry, a remove_headers entry or a body value, or remove the rule');
  } else {
    const seenHeaders = new Set();
    const headerKeys = new Set();
    for (const row of headerRows) {
      const nameIn = row.querySelector('.sp-hname');
      const valIn = row.querySelector('.sp-hval');
      const name = trimServerSpace((nameIn || {}).value);
      const value = trimServerSpace((valIn || {}).value);
      if (!name) { fail(nameIn, 'headers: a header name is empty or only whitespace - fill it in or remove the row'); continue; }
      if (!PROV_HEADER_RE.test(name)) { fail(nameIn, `headers: '${name}' is not a valid header name (RFC 7230 token)`); continue; }
      if (roForbiddenReason(name)) { fail(nameIn, roForbiddenMsg(name, 'headers')); continue; }
      const key = name.toLowerCase();
      if (seenHeaders.has(key)) { fail(nameIn, `headers: duplicate HTTP header name '${name}'`); continue; }
      seenHeaders.add(key);
      headerKeys.add(key);
      if (!roHeaderValueOK(value)) fail(valIn, `headers: ${name}: value must be a non-empty single-line header value`);
    }
    const seenRemoves = new Set();
    for (const {name, button} of removeChips) {
      if (!name || !PROV_HEADER_RE.test(name)) { fail(button, `remove_headers: '${name}' is not a valid header name (RFC 7230 token)`); continue; }
      if (roForbiddenReason(name)) { fail(button, roForbiddenMsg(name, 'remove_headers')); continue; }
      const key = name.toLowerCase();
      if (seenRemoves.has(key)) { fail(button, `remove_headers: duplicate HTTP header name '${name}'`); continue; }
      seenRemoves.add(key);
      if (headerKeys.has(key)) fail(button,
        `'${name}' is both set in headers and removed in remove_headers; keep exactly one action per header`);
    }
    for (const [name, input] of bodyFields) {
      const text = input && trimServerSpace(input.value);
      if (!text) continue;
      const n = Number(text);
      if (!Number.isSafeInteger(n) || n < 1 || n > REQUEST_OVERRIDE_BODY_MAX) fail(input, `body.${name}: '${text}' must be a whole number between 1 and ${REQUEST_OVERRIDE_BODY_MAX}`);
    }
  }
  const triple = client + '\u0000' + provider + '\u0000' + model;
  if (level === 'ok' && seen && seen.has(triple)) {
    const focusedScope = [...card.querySelectorAll('.ro-client, .ro-provider, .ro-model')].find(input => input === document.activeElement);
    fail(focusedScope || card.querySelector('.ro-client'), `duplicate scope with rule ${seen.get(triple)} - the same client, provider and model; merge the rules or change one scope`);
  } else if (seen && (client || provider || model) && !seen.has(triple)) {
    seen.set(triple, index + 1);
  }
  card.dataset.roState = level;
  if (err) err.dataset.level = level === 'error' ? 'error' : '';
  roSyncSummary(card);
  roSyncActionLabels(card, index);
  if (level === 'error') roSetCardExpanded(card, true);
  return level;
}

// validateRequestOverridesDraft walks every rule card in order and returns
// the first invalid one (Apply blocks on it; the button flow focuses and
// flashes it).
function validateRequestOverridesDraft(wrap) {
  const seen = new Map();
  let firstBad = null;
  wrap.querySelectorAll('.ro-rule').forEach((card, i) => {
    if (validateRoRow(card, seen, i) === 'error' && !firstBad) firstBad = card;
  });
  return { ok: !firstBad, firstBad };
}

// roRenumber rewrites the cards' rule numbers after any add or remove so
// they keep matching the request_overrides[i] indexes the server cites in
// its validation errors. The ordinal-carrying accessible names restamp
// through the same composer (roRuleFieldLabel reads data-ro-lbl), so a
// non-tail removal cannot leave a scope or body input announcing a stale
// rule number.
function roRenumber(wrap) {
  wrap.querySelectorAll('.ro-rule').forEach((card, i) => {
    card.dataset.roIndex = String(i);
    const el = card.querySelector('.ro-num');
    if (el) el.textContent = roRuleLabel(i);
    card.querySelectorAll('[data-ro-lbl]').forEach(input => {
      input.setAttribute('aria-label', roRuleFieldLabel(i, input.dataset.roLbl));
    });
    const body = card.querySelector('.ro-rule-body');
    if (body) body.setAttribute('aria-label', `${roRuleLabel(i)} details`);
    roSyncSummary(card);
    roSyncActionLabels(card, i);
  });
}

// roSyncCount keeps the "N / 64 rules" line honest and gates the template
// add-control at the cap (mirrors config.RequestOverridesMax).
function roSyncCount(wrap) {
  const n = wrap.querySelectorAll('.ro-rule').length;
  const count = wrap.querySelector('.mr-count');
  if (count) count.textContent = n + ' / ' + REQUEST_OVERRIDES_MAX + ' rules';
  const cap = n >= REQUEST_OVERRIDES_MAX;
  const add = wrap.querySelector('[data-ro-add]');
  if (add) add.disabled = cap;
  wrap.querySelectorAll('[data-ro-tpl]').forEach(item => { item.disabled = cap; });
  if (cap) closeRoTemplateMenus(wrap);
}

// roDraftChanged is the request-overrides draft-changed epilogue (the
// mrDraftChanged discipline): mark the sheet dirty, revalidate the whole
// draft, renumber the cards and refresh the count.
function roDraftChanged(wrap) {
  markSettingsDirty(settingsKeyFor(wrap));
  validateRequestOverridesDraft(wrap);
  roRenumber(wrap);
  roSyncCount(wrap);
}

// collectRequestOverrides reads the editor into the strict POST shape:
// scope fields always ride along (empty string = wildcard), header names
// are trimmed like the server trims them, and empty collections stay
// absent (headers, remove_headers, body and its fields) so a rule carries
// only what it sets. The shared duplicate-key put discipline throws on
// Apply for an exact duplicate or empty header name; canonical-case
// conflicts and the rest of the grammar are the live gate's to block and
// the server's to validate.
function collectRequestOverrides(wrap, put) {
  const out = [];
  wrap.querySelectorAll('.ro-rule').forEach(card => {
    const r = Object.create(null);
    r.client = trimServerSpace((card.querySelector('.ro-client') || {}).value);
    r.provider = trimServerSpace((card.querySelector('.ro-provider') || {}).value);
    r.model = trimServerSpace((card.querySelector('.ro-model') || {}).value);
    const headers = Object.create(null);
    card.querySelectorAll('.prov-hmap .prov-hrow').forEach(row => {
      const n = trimServerSpace((row.querySelector('.sp-hname') || {}).value);
      const v = trimServerSpace((row.querySelector('.sp-hval') || {}).value);
      put(headers, n, v, row.querySelector('.sp-hname'));
    });
    if (Object.keys(headers).length) r.headers = headers;
    const removes = [...card.querySelectorAll('.prov-chip')].map(c => trimServerSpace(c.dataset.rh || '')).filter(Boolean);
    if (removes.length) r.remove_headers = removes;
    const body = Object.create(null);
    for (const [key, sel] of [['max_tokens', '.ro-max-tokens'], ['max_completion_tokens', '.ro-max-mct']]) {
      const input = card.querySelector(sel);
      const text = input && trimServerSpace(input.value);
      if (!text) continue;
      const n = Number(text);
      body[key] = Number.isSafeInteger(n) ? n : text;
    }
    if (Object.keys(body).length) r.body = body;
    out.push(r);
  });
  return out;
}
