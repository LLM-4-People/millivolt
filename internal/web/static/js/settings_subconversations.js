// Settings sub-conversations editor: one tracked classified client per entry,
// its ordered top-level body fields, and the strip switch that removes those
// fields from the client request body before relay or translation. The entry
// cap, param cap and byte band are mirrored by config's owners. settings.js
// owns the accepted document and the shell; chrome_settings.js's SETTINGS_MODULES list
// owns the load order.

// ---- sub-conversations editor ----
// sub_conversations: per-client tracking of request body fields whose
// value identifies a sub-conversation (exemplar: opencode's
// promptCacheKey). One visual grammar with the request-overrides editor
// (the ro family): each entry is a .prov-sec card inside an .mr-wrap
// shell (hint, tools row, count, muted tokens), the client input gets
// datalist autocomplete from the same known-set union the ro scope
// datalists read, and the ordered param rows are inputs, not chips,
// because order is meaning (the first present field decides: a non-string
// value counts as absent and the next field is checked, while a present
// string that cannot be decoded or fails the identity rules drops the
// identity - no later field is consulted). Live per-card
// validation mirrors config.validateSubConversations at every keystroke
// (the validateRoRow discipline): the client required, 1..4 params, the
// one JSON-key-segment grammar with the server's exact message wording,
// duplicate params within the entry, duplicate clients across entries.
// Apply walks the draft and blocks the POST on the first invalid card,
// focusing the offending input. The count line and add gates mirror
// config.SubConversationsMax (16) and SubConversationParamsMax (4); the
// live grammar check mirrors the 64-byte bound.
const SUB_CONVERSATIONS_MAX = 16;
const SUB_CONVERSATION_PARAMS_MAX = 4;
const SUB_CONVERSATION_PARAM_MAX_BYTES = 64;

// scParamOK mirrors config.checkSubConversationParam's per-byte grammar:
// one JSON object key segment - printable ASCII only, no quote, no
// backslash. The empty and 64-byte rules are judged by their own
// server-worded checks in validateScCard; this predicate owns the byte
// grammar half (the roHeaderValueOK class).
function scParamOK(name) {
  for (let i = 0; i < name.length; i++) {
    const c = name.charCodeAt(i);
    if (c < 0x20 || c > 0x7e || c === 0x22 || c === 0x5c) return false;
  }
  return true;
}

// scEntryLabel owns the entry-number label ("entry N", the 1-based index
// beside the sub_conversations[i] indexes the server cites in its
// validation errors) shared by the card render and scRenumber, so the
// initial label and the rewritten one cannot drift.
function scEntryLabel(i) {
  return 'entry ' + (i + 1);
}

// scEntryFieldLabel composes an ordinal-carrying input's accessible name
// from the same scEntryLabel ordinal the visible entry number uses.
function scEntryFieldLabel(i, suffix) {
  return scEntryLabel(i) + ' ' + suffix;
}

// scEntryFieldAttrs renders an ordinal-carrying input's accessible-name
// attributes: the initial aria-label composes through scEntryLabel, and
// data-sc-lbl carries the suffix scRenumber restamps the label from.
function scEntryFieldAttrs(i, suffix) {
  return `aria-label="${escapeHtml(scEntryFieldLabel(i, suffix))}" data-sc-lbl="${escapeHtml(suffix)}"`;
}

// scDlId owns the client datalist id grammar shared by the generator, the
// entry card's list attribute and the focus-time refresher (the roDlId
// discipline), so the association cannot drift between render and
// refresh.
function scDlId() {
  return 'sc-dl-client';
}

// scDlOptionsHTML owns the datalist options expression shared by the
// initial render and the focus-time refresher: both derive the same
// option list from the same known-set union the ro scope datalists read
// (the wire-key owners), so a classified client observed anywhere on the
// dashboard is offered here too. Free text stays allowed because an
// entry may target a client not yet observed.
function scDlOptionsHTML() {
  return roKnownOptions('client').map(o => `<option value="${escapeHtml(o)}"></option>`).join('');
}

function scDatalistHTML() {
  return `<datalist id="${scDlId()}">${scDlOptionsHTML()}</datalist>`;
}

// scSyncDatalists refreshes the autocomplete options at focus time: the
// known sets arrive on their own refresh cadence, so options picked at
// render time can be stale while the sheet is open.
function scSyncDatalists(wrap) {
  if (!wrap) return;
  const dl = wrap.querySelector('#' + scDlId());
  if (dl) dl.innerHTML = scDlOptionsHTML();
}

// scParamRowHTML renders one ordered tracked-field row inside an input
// line that stretches like the ro scope rows.
function scParamRowHTML(name) {
  return `<div class="st-ctl-line ro-scope sc-prow"><input class="sc-param" value="${escapeHtml(name)}" placeholder="body field - e.g. promptCacheKey" aria-label="tracked body field"><button type="button" class="prov-x" data-sc-p-rm aria-label="remove tracked body field" title="remove">✕</button></div>`;
}

// scCardHTML renders one entry card: the classified client (datalist
// autocomplete), the ordered param rows with their add row, and the
// strip toggle whose label states the forward/strip semantics. The
// client input carries the entry ordinal through the one composer.
function scCardHTML(e, i) {
  e = e || {};
  const params = Array.isArray(e.params) ? e.params : [];
  return `<div class="prov-sec sc-card">` +
    `<div class="prov-lb"><span class="sc-num">${escapeHtml(scEntryLabel(i))}</span><span class="prov-sub">one classified client, tracked exactly</span><button type="button" class="prov-x" data-sc-rm aria-label="remove entry" title="remove entry">✕</button></div>` +
    `<div class="st-ctl-line ro-scope">` +
    `<input class="sc-client" list="${scDlId()}" value="${escapeHtml(e.client || '')}" placeholder="classified client - e.g. opencode" ${scEntryFieldAttrs(i, 'client')}>` +
    `</div>` +
    `<div class="prov-lb"><span>params</span><span class="prov-sub">exact top-level request body fields, checked in order - the first present field decides</span></div>` +
    `<div class="sc-params">${params.map(p => scParamRowHTML(String(p == null ? '' : p))).join('')}</div>` +
    `<div class="prov-add"><input class="sc-param-in" placeholder="body field - e.g. promptCacheKey ↵" aria-label="add tracked body field"><button type="button" class="btn prov-addbtn" data-sc-param-add aria-label="add tracked body field">+</button></div>` +
    `<label class="st-check"><input type="checkbox" class="st-switch sc-strip"${e.strip ? ' checked' : ''}><span>strip every configured field from the top level of the client request body before relay or translation - unchecked forwards it unchanged</span></label>` +
    `<div class="mr-err sc-err" aria-live="polite"></div>` +
    `</div>`;
}

function subConversationsEditorHTML(val) {
  const entries = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap sc-wrap">` +
    `<div class="mr-hint">each entry tracks one classified client's sub-conversation identity from top-level request body fields · the first present field decides: a present value that is not a JSON string counts as absent and the next field is checked, while a present string that cannot be decoded or fails the identity rules drops the identity for the request - no later field is consulted, never a client error · the value groups requests under a k: identity, after X-Proxy-Session and before client+key grouping · strip removes every configured field present at the top level of the client request body before relay or translation</div>` +
    `<div class="mr-tools"><button type="button" class="btn" data-sc-add aria-label="add entry">+ add entry</button><span class="mr-count muted"></span></div>` +
    `<div class="sc-rows">${entries.map((e, i) => scCardHTML(e, i)).join('')}</div>` +
    scDatalistHTML() +
    `</div>`;
}

// addScCard appends one blank entry card and moves focus to its client
// input (the guided-input discipline: the live validation immediately
// asks for the client, then one param). The add gate refuses beyond
// config.SubConversationsMax.
function addScCard(wrap) {
  if (!wrap || wrap.querySelectorAll('.sc-card').length >= SUB_CONVERSATIONS_MAX) return;
  const rows = wrap.querySelector('.sc-rows');
  rows.insertAdjacentHTML('beforeend', scCardHTML({}, rows.children.length));
  scDraftChanged(wrap);
  const card = [...wrap.querySelectorAll('.sc-card')].pop();
  const focus = card && card.querySelector('.sc-client');
  if (focus) focus.focus();
}

// addScParamRow appends one tracked-field row from the add inputs. Deny
// by default on the server's own grammar (the addRoRemoveHeader
// discipline): the per-entry cap, an empty name, a grammar violation,
// the 64-byte bound, or a duplicate within the entry flashes the add
// input instead of adding a row the server would refuse.
function addScParamRow(card) {
  const input = card && card.querySelector('.sc-param-in');
  const box = card && card.querySelector('.sc-params');
  if (!input || !box) return;
  if (box.querySelectorAll('.sc-prow').length >= SUB_CONVERSATION_PARAMS_MAX) { flashBadInput(input); return; }
  const name = String(input.value || '').trim();
  if (!name || !scParamOK(name) || name.length > SUB_CONVERSATION_PARAM_MAX_BYTES) { flashBadInput(input); return; }
  if ([...box.querySelectorAll('.sc-param')].some(p => String(p.value || '').trim() === name)) { flashBadInput(input); return; }
  box.insertAdjacentHTML('beforeend', scParamRowHTML(name));
  input.value = '';
  scDraftChanged(card.closest('.sc-wrap'));
}

// validateScCard is the live per-card gate (the validateRoRow
// discipline): every check mirrors config.validateSubConversations in
// the server's own order and message wording (the
// sub_conversations[i].key prefix becomes the card's inline context; %q
// becomes the quoted name). Checks that own an input redden it; the
// duplicate-client cross-card check marks no input (it cites an earlier
// card, not this one); the first failure owns the message text. seen
// maps trimmed clients to their 1-based entry number for the duplicate
// wording, first occurrence only - a duplicate carrying another error
// must not shift the ordinal later duplicates cite; index is this
// card's 0-based position.
function validateScCard(card, seen, index) {
  const err = card.querySelector('.sc-err');
  card.querySelectorAll('.prov-bad').forEach(el => el.classList.remove('prov-bad'));
  const clientIn = card.querySelector('.sc-client');
  const client = String((clientIn || {}).value || '').trim();
  const paramRows = [...card.querySelectorAll('.sc-prow')];
  let level = 'ok', msg = '';
  const fail = (input, text) => {
    if (input) input.classList.add('prov-bad');
    if (level !== 'error') { level = 'error'; msg = text; }
  };
  if (!client) {
    fail(clientIn, 'client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry');
  }
  if (!paramRows.length) {
    fail(card.querySelector('.sc-param-in'),
      'no param set - give the entry a request body field to track, or remove the entry');
  }
  const seenParams = new Set();
  paramRows.forEach(row => {
    const input = row.querySelector('.sc-param');
    const name = String((input || {}).value || '').trim();
    if (!name) { fail(input, 'params: must not be empty or only whitespace'); return; }
    if (!scParamOK(name)) { fail(input, `params: '${name}' is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`); return; }
    if (name.length > SUB_CONVERSATION_PARAM_MAX_BYTES) { fail(input, `params: must be 1..${SUB_CONVERSATION_PARAM_MAX_BYTES} bytes, got ${name.length}`); return; }
    if (seenParams.has(name)) { fail(input, `params: duplicate '${name}'`); return; }
    seenParams.add(name);
  });
  if (level === 'ok' && seen && seen.has(client)) {
    level = 'error';
    msg = `duplicate client '${client}' with entry ${seen.get(client)}; merge the entries or change one client`;
  } else if (seen && client && !seen.has(client)) {
    seen.set(client, index + 1);
  }
  card.dataset.scState = level;
  if (err) { err.textContent = msg; err.dataset.level = level === 'error' ? 'error' : ''; }
  return level;
}

// validateSubConversationsDraft walks every entry card in order and
// returns the first invalid one (Apply blocks on it; the button flow
// focuses and flashes it).
function validateSubConversationsDraft(wrap) {
  const seen = new Map();
  let firstBad = null;
  wrap.querySelectorAll('.sc-card').forEach((card, i) => {
    if (validateScCard(card, seen, i) === 'error' && !firstBad) firstBad = card;
  });
  return { ok: !firstBad, firstBad };
}

// scRenumber rewrites the cards' entry numbers after any add or remove so
// they keep matching the sub_conversations[i] indexes the server cites in
// its validation errors. The ordinal-carrying accessible names restamp
// through the same composer (scEntryFieldLabel reads data-sc-lbl), so a
// non-tail removal cannot leave a client input announcing a stale entry
// number.
function scRenumber(wrap) {
  wrap.querySelectorAll('.sc-card').forEach((card, i) => {
    const el = card.querySelector('.sc-num');
    if (el) el.textContent = scEntryLabel(i);
    card.querySelectorAll('[data-sc-lbl]').forEach(input => {
      input.setAttribute('aria-label', scEntryFieldLabel(i, input.dataset.scLbl));
    });
  });
}

// scSyncCount keeps the "N / 16 entries" line honest and gates the add
// button at the cap (mirrors config.SubConversationsMax).
function scSyncCount(wrap) {
  const n = wrap.querySelectorAll('.sc-card').length;
  const count = wrap.querySelector('.mr-count');
  if (count) count.textContent = n + ' / ' + SUB_CONVERSATIONS_MAX + ' entries';
  const add = wrap.querySelector('[data-sc-add]');
  if (add) add.disabled = n >= SUB_CONVERSATIONS_MAX;
}

// scDraftChanged is the sub-conversations draft-changed epilogue (the
// roDraftChanged discipline): mark the sheet dirty, revalidate the whole
// draft, renumber the cards and refresh the count.
function scDraftChanged(wrap) {
  markSettingsDirty(settingsKeyFor(wrap));
  validateSubConversationsDraft(wrap);
  scRenumber(wrap);
  scSyncCount(wrap);
}

// collectSubConversations reads the editor into the strict POST shape:
// the client and strip always ride along, param names are trimmed like
// the server trims them, and an empty params list stays absent so an
// entry carries only what it sets. The live gate blocks an empty-params
// card before the POST; the server re-validates everything.
function collectSubConversations(wrap) {
  const out = [];
  wrap.querySelectorAll('.sc-card').forEach(card => {
    const e = Object.create(null);
    e.client = String((card.querySelector('.sc-client') || {}).value || '').trim();
    const params = [...card.querySelectorAll('.sc-param')].map(p => String(p.value || '').trim());
    if (params.length) e.params = params;
    const strip = card.querySelector('.sc-strip');
    e.strip = !!(strip && strip.checked);
    out.push(e);
  });
  return out;
}

// addAliasRow appends one mapping row from the add inputs. Deny by default:
// either side empty, a self-mapping, or a duplicate old label flashes the
// offending input instead of adding a row that could never apply.
