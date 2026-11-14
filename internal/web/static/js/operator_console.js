// The operator console: one control plane for every operator policy. It
// replaces the five bespoke header menus with a persistent status readout and
// one panel whose rail names the sections. The panel is a .mn surface, so it
// inherits the one menu lifecycle (placement, sheet, Escape, outside press,
// focus restore) instead of hand-rolling a second one.
//
// Ownership: this module owns the console's DOM, drafts and human wording.
// The server owns policy semantics; chrome_operator.js owns the mutation gate,
// chrome_lists.js owns the live state bits and chrome_settings.js owns the
// shared reconcilers and download paths. Nothing here re-implements those.

const OC_SECTIONS = [
  { id: 'overview', label: 'overview' },
  { id: 'limits', label: 'limits' },
  { id: 'pause', label: 'pause' },
  { id: 'capture', label: 'capture' },
  { id: 'export', label: 'export' },
  { id: 'system', label: 'system' },
];

const OC_STORAGE_KEY = 'millivolt.operatorSection';
const OC_CHANGE_LIMIT = 8;
const OC_MINUTE = 60000;
const OC_DAY_LABELS = ['S', 'M', 'T', 'W', 'T', 'F', 'S'];

let ocSection = 'overview';
try {
  const saved = sessionStorage.getItem(OC_STORAGE_KEY);
  if (saved && OC_SECTIONS.some(s => s.id === saved)) ocSection = saved;
} catch (e) { /* storage denied: overview is the default */ }

let ocMounted = false;
let ocChanges = [];
let ocUndoBusy = false;
let ocError = '';
let ocView = { kind: '', id: '' };

// Drafts survive section switches and live polls. They are the operator's
// in-progress work, never a copy of server state.
const ocDrafts = {
  limit: null,
  pause: null,
  capture: null,
  export: null,
};
let ocDrain = { scope: 'all', value: '' };
// ocPickerFocus holds the dimension whose picker was just opened by a click.
// The picker implements the menu keys, but a menu that leaves focus on its
// trigger makes those keys unreachable until the operator happens to press
// Tab: opening it moves focus to the first row once, and every later repaint
// goes back to the normal keyed-focus rule.
let ocPickerFocus = '';

function ocEl(id) { return document.getElementById(id); }

// ---------- long rule lists ----------
// A rule list that outgrows the 560px panel is only usable if it can be
// narrowed: fourteen limits fill the pane and finding one becomes a scroll
// with no way to search. The filter appears only once a list is long enough to
// need it, so a console with two rules never spends a row on an empty box.
const OC_LIST_FILTER_MIN = 6;
const ocListFilter = { limits: '', pause: '', capture: '' };

// ocRuleEditing reports whether a stored rule is the one open in its
// composer. Without it the edited rule appeared twice on screen - once as the
// composer and again as an identical row in the list - and nothing said which
// row the form belonged to. The row now names that relationship instead of
// reading as a second, separate rule.
function ocRuleEditing(kind, id) {
  if (!id || ocView.kind !== kind || ocView.id !== id) return '';
  return ' data-oc-editing="1" aria-current="true"';
}

// ocFilterRows narrows a section's rows by the text each one renders. Matching
// the rendered reading, rather than re-deriving the scope here, keeps one
// search rule for every row shape and makes a new field searchable for free.
function ocFilterRows(section, rows) {
  const needle = (ocListFilter[section] || '').trim().toLowerCase();
  if (!needle) return rows;
  return rows.filter(row => (row.text || '').toLowerCase().includes(needle));
}

// ocListFilterHTML is one search field, matching the dimension picker's own
// filter: a type="search" input already carries the platform's clear control,
// so a second "clear" button beside it was two controls for one action.
// The hidden-everything case still offers a clear action, because there the
// result set is empty and the input's own glyph is the only way back.
function ocListFilterHTML(section, total) {
  if (total < OC_LIST_FILTER_MIN) return '';
  const value = ocListFilter[section] || '';
  return `<div class="oc-list-filter">` +
    `<input type="search" data-oc-list-filter="${section}" value="${escapeHtml(value)}"` +
    ` placeholder="filter ${total} rules" aria-label="filter the ${section} rules"` +
    ` data-oc-key="list-filter-${section}">` +
    `</div>`;
}

// ocListEmptyNote states that a filter hid the section's rules, so an empty
// list never reads as an empty policy.
function ocListEmptyNote(section, shown, total) {
  if (!total || shown) return '';
  return `<div class="oc-empty"><strong>No rule matches that filter.</strong> ` +
    `${fmt(total)} ${total === 1 ? 'rule is' : 'rules are'} in force; ` +
    `<button type="button" class="oc-link" data-oc-list-clear="${section}">clear the filter</button> to see them.</div>`;
}

// ---------- status readout ----------
// ocRuleCounts is the one summary of everything in force. Restart is included
// because it is the one transient state that must stay visible after the
// console closes.
// ocLimitEnforcing reports whether a stored policy is enforcing right now.
// The server owns the verdict (ThrottleInfo.scheduled) because a policy
// outside its schedule is installed but admits everything; counting it would
// make the readout claim a cap that is not applied.
function ocLimitEnforcing(policy) {
  return !!policy && policy.scheduled !== false;
}

function ocRuleCounts() {
  const holds = typeof liveHolds === 'function' ? liveHolds().length : 0;
  const captures = typeof liveDebugSessions === 'function' ? liveDebugSessions().length : 0;
  const policies = (typeof throttleState !== 'undefined' && throttleState.throttles) || [];
  const limits = policies.filter(ocLimitEnforcing).length;
  return { holds, captures, limits, parked: policies.length - limits,
    installed: policies.length, total: holds + captures + limits };
}

function ocRestartActive() {
  return typeof restartInProgress === 'function' && restartInProgress();
}

// ocReadoutSegments returns the readout's words in priority order: a restart
// outranks a policy because it explains why traffic is moving.
function ocReadoutSegments() {
  const segs = [];
  if (ocRestartActive()) {
    const rank = typeof restartServerState === 'object' && restartServerState &&
      restartServerState.rank != null ? restartServerState.rank : null;
    // rank -1 is the server's idle marker, not a step; naming "step 0" there
    // would read as a bug at the moment the operator needs to trust the line.
    segs.push('restarting' + (rank >= 0 ? ' · step ' + (rank + 1) : ''));
  }
  const counts = ocRuleCounts();
  if (counts.holds > 0) segs.push('paused ' + counts.holds);
  // `limits 1` is a noun and a count; `capture 1` was a verb and a count in
  // the same sentence. The plural noun makes the line read as one list.
  if (counts.captures > 0) segs.push('captures ' + counts.captures);
  if (counts.limits > 0) segs.push('limits ' + counts.limits);
  // A stored policy outside its schedule enforces nothing, so it is named
  // separately rather than counted as a cap. The server's `scheduled` field
  // is true only while the window is open, so this segment counts the closed
  // ones: "off schedule", not "scheduled", which read as the opposite.
  if (counts.parked > 0) segs.push('off schedule ' + counts.parked);
  return segs;
}

// refreshOperatorReadout repaints the header's always-visible state. It is the
// answer to "is anything active right now" and it never depends on the console
// being open.
function refreshOperatorReadout() {
  const box = ocEl('op-readout');
  if (!box) return;
  // ocSyncTriggerName at the end reads the painted words, so the label always
  // matches what is on screen.
  const segs = ocReadoutSegments();
  // A hold is the one state that stops live traffic, so it outranks a routine
  // cap in the readout's own tone. Restart outranks both. An offline stream
  // outranks everything: a stale line must not claim "steady".
  // Only a connection that was open and then dropped makes the line stale;
  // a page that has not connected yet is still loading, not wrong.
  const offline = typeof streamSeen !== 'undefined' && streamSeen && !streamLive;
  // A poll that has been failing means the policy documents are the last ones
  // the server sent, not the current ones. The line must not claim currency
  // for them. The stream can be healthy while the poll is down, so this is
  // separate from `offline`.
  const stale = typeof operatorPollStale === 'function' && operatorPollStale();
  const state = (offline || stale) ? 'stale'
    : ocRestartActive() ? 'busy'
      : ocRuleCounts().holds > 0 ? 'paused'
        : segs.length ? 'active' : 'steady';
  box.dataset.state = state;
  // The words stay the last known truth and say they are not current, so the
  // counts are never silently presented as a live reading.
  const words = segs.length ? segs.join(' · ') : 'steady';
  const text = offline ? words + ' · offline' : stale ? words + ' · stale' : words;
  const label = ocEl('op-readout-state');
  if (label && label.textContent !== text) label.textContent = text;
  const trigger = ocEl('btn-controls');
  if (trigger) trigger.dataset.state = state;
  ocSyncTriggerName();
  box.setAttribute('aria-label', 'operator state: ' + text + '.');
  const dot = ocEl('op-readout-dot');
  if (dot) dot.setAttribute('title', text);
}

// ---------- console lifecycle ----------
function ocShell() {
  return ocEl('operator-console');
}

// ocSyncTriggerName repaints the readout's accessible name after the console
// opens or closes, because the name states the action the trigger performs
// now. menu.js owns aria-expanded; this only follows it.
function ocSyncTriggerName() {
  const trigger = ocEl('btn-controls');
  if (!trigger) return;
  const text = (ocEl('op-readout-state')?.textContent || 'steady').trim();
  const open = trigger.getAttribute('aria-expanded') === 'true';
  trigger.setAttribute('aria-label', 'operator state: ' + text + '. '
    + (open ? 'Close the operator console.' : 'Open the operator console.'));
}

function openOperatorConsole(section, view) {
  const shell = ocShell();
  if (!shell) return;
  // The console is the operator plane's one open surface. Close the theme
  // tray and the Settings sheet first: a modal sheet marks every other body
  // child inert, so opening over it would leave the console visible but
  // unfocusable.
  if (typeof closeHeaderMenus === 'function') closeHeaderMenus('operator-console');
  if (section && OC_SECTIONS.some(s => s.id === section)) ocSelectSection(section, true);
  if (view && view.kind) ocView = { kind: view.kind, id: view.id || '' };
  ocMounted = true;
  ocRender();
  // A record pill may open the console that is already open. Reusing the
  // surface keeps menu.js's measured geometry and avoids a close/reopen that
  // would run onClose and leave the mounted flag false.
  if (!menuIsOpen(shell)) {
    menuOpen(ocEl('btn-controls'), shell, {
      align: 'end',
      initialFocus: 'panel',
      // Escape and an outside press close through menu.js; this keeps the
      // console's mounted state truthful so nothing repaints into a hidden
      // panel after the operator dismisses it.
      onClose: () => { ocMounted = false; ocSyncTriggerName(); },
    });
  }
  // The render above ran while the panel was still hidden, where every box
  // measures zero. The rail's overflow flag was therefore computed against a
  // 0-width strip and stayed "0": on a phone the last sections were cut off
  // with no fade to say more existed. Re-measure now that the panel is laid
  // out.
  ocSyncRailOverflow();
  refreshOperatorReadout();
}

function closeOperatorConsole(restoreFocus = false) {
  const shell = ocShell();
  if (!shell) return;
  const wasOpen = menuIsOpen(shell);
  if (wasOpen) menuClose(shell);
  ocMounted = false;
  ocSyncTriggerName();
  const trigger = ocEl('btn-controls');
  if (restoreFocus && trigger) trigger.focus();
}

function toggleOperatorConsole(e) {
  if (e) e.stopPropagation();
  const shell = ocShell();
  if (shell && menuIsOpen(shell)) { closeOperatorConsole(); return; }
  openOperatorConsole(ocSection);
}

function ocSelectSection(id, skipRender) {
  if (!OC_SECTIONS.some(s => s.id === id)) return;
  // A rejection banner belongs to the write the operator attempted. Moving to
  // another section abandons that attempt, so the banner goes with it instead
  // of following them around.
  if (id !== ocSection) ocError = '';
  ocSection = id;
  try { sessionStorage.setItem(OC_STORAGE_KEY, id); } catch (e) { /* storage denied */ }
  if (!skipRender) ocRender();
}

// ---------- focus preservation ----------
// ocFocusKey names the focused control across a full pane rebuild. Draft
// values live in ocDrafts, so only the caret and the element identity need to
// survive.
// ocFocusTarget resolves the focusable control a keyed slot stands for. A key
// may sit on the button itself or on a field's wrapping label, and a label is
// not focusable, so the slot's own control wins when it has one.
function ocFocusTarget(slot) {
  if (!slot) return null;
  if (slot.matches('button, input, select, textarea, [tabindex]')) return slot;
  return slot.querySelector('button, input, select, textarea, [tabindex]');
}

function ocFocusKey() {
  const active = document.activeElement;
  if (!active || !ocShell()?.contains(active)) return null;
  // A picker row sits inside the picker's own keyed slot, so the generic
  // keyed lookup below already restores focus into the picker that owns it.
  // Choosing a value therefore rebuilds the pane without dropping the keyboard
  // operator back to <body>.
  const keyed = active.closest('[data-oc-key]');
  if (!keyed) {
    // The pane is replaced wholesale on every structural change, so focus
    // inside it must always have a landing place. An unkeyed control resolves
    // to the pane rather than to <body>, which is the page behind the open
    // panel: a keyboard operator who armed a checkbox used to find their next
    // Tab walking the dashboard underneath.
    const pane = ocEl('oc-pane');
    return pane && pane.contains(active) ? { key: '', pane: true } : null;
  }
  let start = null, end = null;
  try { start = active.selectionStart; end = active.selectionEnd; } catch (e) { /* not a text input */ }
  return { key: keyed.dataset.ocKey, start, end };
}

function ocRestoreFocus(saved) {
  if (!saved) return;
  const shell = ocShell();
  if (!shell) return;
  const slot = saved.key
    ? [...shell.querySelectorAll('[data-oc-key]')].find(el => el.dataset.ocKey === saved.key)
    : null;
  const next = ocFocusTarget(slot) || (saved.pane ? ocEl('oc-pane') : null);
  if (!next) return;
  next.focus({ preventScroll: true });
  if (saved.start != null && typeof next.setSelectionRange === 'function') {
    try { next.setSelectionRange(saved.start, saved.end); } catch (e) { /* number inputs reject a caret */ }
  }
}

// ocRender rebuilds the rail and the active pane. It is called on a section
// change, a structural state change, and a draft mutation that reshapes the
// form; live-only updates use the section's own in-place painters.
function ocRender() {
  const shell = ocShell();
  if (!shell || !ocMounted) return;
  const saved = ocFocusKey();
  const paneBefore = ocEl('oc-pane');
  // A pick rebuilds the pane, and the pane is the console's one scroll
  // region. Without carrying scrollTop across, selecting a second value from a
  // picker threw the operator back to the top of a form taller than the panel.
  const savedScroll = paneBefore ? paneBefore.scrollTop : 0;
  ocRenderHead();
  ocRenderRail();
  const pane = ocEl('oc-pane');
  if (pane) {
    const render = {
      overview: ocRenderOverview,
      limits: ocRenderLimits,
      pause: ocRenderPause,
      capture: ocRenderCapture,
      export: ocRenderExport,
      system: ocRenderSystem,
    }[ocSection];
    // The pane keeps two children: a stable rejection banner and a body that
    // every render replaces wholesale. The banner used to ride the rebuilt
    // markup, so each unrelated poll created a fresh role="alert" and a screen
    // reader re-announced the same rejection for as long as it stayed up.
    let body = pane.querySelector(':scope > #oc-pane-body');
    if (!body) {
      body = document.createElement('div');
      body.id = 'oc-pane-body';
      pane.appendChild(body);
    }
    body.innerHTML = render ? render() : '';
    ocSyncErrorBanner(pane);
    pane.dataset.section = ocSection;
    pane.setAttribute('aria-labelledby', 'oc-tab-' + ocSection);
    // The previews are the operator's proof of what will be enforced, so they
    // repaint with the form after any structural change, not only on input.
    if (ocSection === 'limits') ocLimitPreview();
    if (ocSection === 'pause') ocPausePreview();
    if (ocSection === 'capture') ocCapturePreview();
  }
  if (pane) pane.scrollTop = savedScroll;
  ocRenderFoot();
  if (ocPickerFocus) {
    // Opening the menu lands on its first row, so the arrows the menu
    // documents work without a preliminary Tab.
    ocPickerFocus = '';
    pane?.querySelector('.oc-pick-list .oc-pick-row')?.focus({ preventScroll: true });
  } else {
    ocRestoreFocus(saved);
  }
  const tab = shell.querySelector('.oc-rail-item[aria-selected="true"]');
  if (tab && typeof tab.scrollIntoView === 'function') {
    tab.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }
  ocSyncRailOverflow();
}

function ocRenderHead() {
  const live = ocEl('oc-live');
  if (!live) return;
  // The head line repeats the readout words and nothing else. The counts are
  // already in the segments, so a total would only say them twice.
  const segs = ocReadoutSegments();
  const text = segs.length ? segs.join(' · ') : 'steady · no rules in force';
  if (live.textContent !== text) live.textContent = text;
}

// The section strip is one horizontal chip row at every width, so the tablist
// announces a horizontal orientation everywhere and arrows move left and
// right. menu.js owns placement only; it does not change the rail's axis.
const OC_RAIL_ORIENTATION = 'horizontal';

function ocSyncRailOrientation() {
  const rail = ocEl('oc-rail');
  if (rail) rail.setAttribute('aria-orientation', OC_RAIL_ORIENTATION);
}

// ocSyncRailOverflow marks whether the section strip has more to show. A
// narrow panel cannot fit six sections, and a hidden scrollbar with no cue
// left the last one entirely outside the frame. The CSS fades the trailing
// edge only while data-more is set.
function ocSyncRailOverflow() {
  const rail = ocEl('oc-rail');
  if (!rail) return;
  const more = rail.scrollWidth - rail.clientWidth - Math.round(rail.scrollLeft) > 1;
  rail.dataset.more = more ? '1' : '0';
}

function ocRenderRail() {
  const rail = ocEl('oc-rail');
  if (!rail) return;
  ocSyncRailOrientation();
  const counts = { limits: ocRuleCounts().limits, pause: ocRuleCounts().holds, capture: ocRuleCounts().captures };
  rail.innerHTML = OC_SECTIONS.map(section => {
    const count = counts[section.id];
    const badge = count ? `<span class="oc-rail-n">${fmt(count)}</span>` : '';
    const active = section.id === ocSection ? 'true' : 'false';
    return `<button type="button" class="oc-rail-item" role="tab" id="oc-tab-${section.id}" aria-selected="${active}" aria-controls="oc-pane" tabindex="${active === 'true' ? '0' : '-1'}" data-oc-section="${section.id}" data-oc-key="rail-${section.id}">` +
      `<span class="oc-rail-label">${escapeHtml(section.label)}</span>${badge}</button>`;
  }).join('');
}

function ocRenderFoot() {
  const foot = ocEl('oc-change');
  const undo = ocEl('oc-undo');
  if (!foot) return;
  const last = ocChanges[ocChanges.length - 1];
  foot.textContent = last ? last.phrase : 'no changes yet this session';
  foot.dataset.tone = last ? (last.tone || '') : '';
  if (undo) {
    undo.hidden = !last || typeof last.undo !== 'function';
    undo.disabled = ocUndoBusy;
    // The button says what it will restore. The footer line names the newest
    // change, which is not always the reversible one, so the accessible name
    // is the only place the two can be tied together for a screen reader.
    if (!undo.hidden && last) undo.setAttribute('aria-label', 'undo: ' + last.phrase);
    else undo.removeAttribute('aria-label');
  }
}

// ---------- shared wording ----------
// ocScopePhrase is the one human reading of a selector. Rule rows, previews,
// confirmations and the readout all call it, so an operator never has to
// translate three vocabularies for one policy.
function ocScopePhrase(scope) {
  if (!scope) return 'every request';
  const bits = [];
  const dims = [
    ['providers', 'provider', 'providers'],
    ['models', 'model', 'models'],
    ['clients', 'client', 'clients'],
    ['keyHashes', 'key', 'keys'],
    ['conversations', 'conversation', 'conversations'],
  ];
  for (const [key, singular, plural] of dims) {
    const values = scope[key] || [];
    if (!values.length) continue;
    if (values.length === 1) bits.push(singular + ' ' + ocScopeValueLabel(key, values[0]));
    else bits.push(values.length + ' ' + plural);
  }
  return bits.length ? bits.join(' · ') : 'every request';
}

// ocScopeValueLabel is the one shortening of a raw selector value. The
// dimension name always comes from the surrounding sentence or row, so this
// never repeats it: a key reads as its short hash, not "key key abcdef12".
function ocScopeValueLabel(dim, value) {
  if (dim === 'keyHashes') return String(value).slice(0, 8) + '…';
  return String(value);
}

// ocBudgetPhrase is the one human reading of a limit's caps.
function ocBudgetPhrase(policy) {
  const bits = [];
  if (policy.concurrency) bits.push(policy.concurrency + ' in flight');
  if (policy.requests) {
    bits.push(fmt(policy.requests) + ' request' + (policy.requests === 1 ? '' : 's') + ' / ' + (policy.request_window || '1m'));
  }
  if (policy.tokens) {
    bits.push(fmt(policy.tokens) + ' tokens / ' + (policy.token_window || '1m'));
  }
  if (!bits.length) return 'no caps';
  return bits.join(' · ');
}

// ocSchedulePhrase is the one human reading of a schedule. It accepts both
// shapes the console holds: the server document's start_minute/end_minute and
// the composer draft's start/end. One normalizer keeps the two from drifting
// into different sentences for the same window.
function ocSchedulePhrase(schedule) {
  if (!schedule) return '';
  const start = schedule.start_minute != null ? schedule.start_minute : schedule.start;
  const end = schedule.end_minute != null ? schedule.end_minute : schedule.end;
  const days = Array.isArray(schedule.days) && schedule.days.length
    ? schedule.days.map(d => OC_DAY_LABELS[d] || '?').join('') : 'every day';
  if (start === end) return days + ' · all day';
  const clock = m => String(Math.floor(m / 60)).padStart(2, '0') + ':' + String(m % 60).padStart(2, '0');
  return days + ' · ' + clock(start) + '-' + clock(end);
}

function ocUntilPhrase(until) {
  if (!until) return '';
  const ms = new Date(until).getTime() - Date.now();
  if (!Number.isFinite(ms) || ms <= 0) return 'expired';
  const minutes = Math.max(1, Math.ceil(ms / OC_MINUTE));
  return hmBreakdown(minutes) + ' left';
}

function ocDurationText(value) {
  return DURATION_LABELS[value] || value || 'until resumed';
}

// ---------- scope editor ----------
function ocNewScope() {
  return { providers: [], models: [], clients: [], keyHashes: [], conversations: [] };
}

function ocScopeFromPolicy(policy) {
  const scope = ocNewScope();
  for (const key of Object.keys(scope)) {
    const values = policy?.scope?.[key === 'keyHashes' ? 'key_hashes' : key] || [];
    scope[key] = [...values];
  }
  return scope;
}

function ocScopeHas(scope) {
  return Object.values(scope).some(values => values.length > 0);
}

// ocLimitSelectorKey is the client-side reading of the server's policy
// identity. A limit is identified by its normalized scope plus schedule, not
// by an id the console chose: re-applying the same selector is an edit, not a
// second rule with the same meaning. Normalization mirrors
// scheduler.PolicyID - trim, drop blanks, dedupe, sort, collapse a seven-day
// week to "every day" - so the console can tell an edit from a create before
// it writes, instead of discovering it afterwards.
function ocLimitSelectorKey(scope, schedule) {
  const dims = {};
  for (const key of ['providers', 'models', 'clients', 'keyHashes', 'conversations']) {
    const values = [...new Set((scope?.[key] || []).map(v => String(v).trim()).filter(Boolean))].sort();
    dims[key] = values;
  }
  const days = [...new Set((schedule?.days || []).filter(d => Number.isInteger(d) && d >= 0 && d <= 6))]
    .sort((a, b) => a - b);
  const clamp = m => (Number.isInteger(m) && m >= 0 && m < 1440) ? m : 0;
  const start = schedule ? clamp(schedule.start) : 0;
  const end = schedule ? clamp(schedule.end) : 0;
  const week = days.length === 7 ? [] : days;
  dims.schedule = week.length === 0 && start === end ? null : { days: week, start, end };
  return JSON.stringify(dims);
}

// ocLimitScheduleKey reads a stored policy's schedule in the draft's shape, so
// the same key function serves both sides of the comparison.
function ocLimitScheduleKey(policy) {
  const schedule = policy?.schedule;
  if (!schedule) return null;
  return {
    days: Array.isArray(schedule.days) ? schedule.days : [],
    start: schedule.start_minute != null ? schedule.start_minute : 0,
    end: schedule.end_minute != null ? schedule.end_minute : 0,
  };
}

// ocDraftScope is the one selector accessor across the three composer drafts.
// Limit drafts carry a nested scope; pause and capture drafts keep the same
// dimension names at the top level. The picker never needs to know which.
function ocDraftScope(draft) {
  return draft.scope || draft;
}

function ocScopeToggle(scope, dim, value) {
  const values = scope[dim] || [];
  const at = values.indexOf(value);
  if (at >= 0) values.splice(at, 1);
  else values.push(value);
  scope[dim] = values;
}

// ocKnownValues is the one vocabulary for a selector dimension. Provider and
// client names come from the throttle document, which is the server's own
// merge of every seen name; models are folded into canonical groups and
// expanded to every raw spelling the server matches; keys and conversations
// come from the live record ring, because that is where the dashboard already
// has them without a second query.
function ocKnownValues(dim) {
  if (dim === 'providers') {
    const state = typeof throttleState !== 'undefined' ? throttleState : {};
    const list = state.known_providers || [];
    return [...new Set(list)].sort();
  }
  if (dim === 'clients') {
    const list = (typeof throttleState !== 'undefined' && throttleState.known_clients) || [];
    return [...new Set(list)].sort();
  }
  if (dim === 'models') {
    const groups = groupByCanonical((typeof throttleState !== 'undefined' && throttleState.known_models) || []);
    return groups.flatMap(g => g.variants).sort();
  }
  if (dim === 'keyHashes') {
    const recs = (typeof lastData !== 'undefined' && lastData?.records) || [];
    return [...new Set(recs.map(r => r.key_hash).filter(Boolean))].sort();
  }
  if (dim === 'conversations') {
    const recs = (typeof lastData !== 'undefined' && lastData?.records) || [];
    return [...new Set(recs.map(r => r.conversation_id).filter(Boolean))].sort();
  }
  return [];
}

function ocDimLabel(dim) {
  return {
    providers: 'provider', models: 'model', clients: 'client',
    keyHashes: 'key', conversations: 'conversation',
  }[dim] || dim;
}

function ocScopeChipsHTML(scope, dim) {
  const values = scope[dim] || [];
  return values.map(value => {
    const label = ocScopeValueLabel(dim, value);
    // The label is its own box so it can ellipsize. As a bare text node in a
    // nowrap chip it could not shrink, so a long value pushed the remove
    // button past the chip's max-width and out of reach: a provider or model
    // name of nineteen characters left the control with no clickable area at
    // all. The title keeps the whole value readable where hover exists.
    return `<span class="oc-chip" title="${escapeHtml(label)}">` +
      `<span class="oc-chip-text">${escapeHtml(label)}</span>` +
      `<button type="button" class="oc-chip-x" data-oc-scope-remove="${dim}" data-oc-value="${escapeHtml(value)}" aria-label="remove ${escapeHtml(label)}">×</button></span>`;
  }).join('');
}

// ocDimChipHTML is one dimension in a selector strip: a chip that opens its
// picker, carrying the chosen value chips inside it. A dimension with nothing
// chosen stays a dim chip, so the strip never grows an "any" word per row.
function ocDimChipHTML(opt) {
  // A chip that opens a menu says so and names it. Without the pair, a screen
  // reader announced only "collapsed" and never that choosing a value opens a
  // menu at all, so the arrow keys the picker implements were undiscoverable.
  const menu = opt.menuId
    ? ` aria-haspopup="menu" aria-controls="${escapeHtml(opt.menuId)}"`
    : '';
  return `<div class="oc-cond" data-open="${opt.open ? '1' : '0'}" data-set="${opt.set ? '1' : '0'}">` +
    `<button type="button" class="oc-cond-btn" ${opt.attr}="${escapeHtml(opt.dim)}" data-oc-key="${escapeHtml(opt.key)}" aria-expanded="${opt.open ? 'true' : 'false'}"${menu}>` +
    `<span class="oc-cond-name">${escapeHtml(opt.label)}</span>` +
    `<span class="oc-cond-chev" aria-hidden="true">${opt.open ? '▴' : '▾'}</span></button>` +
    (opt.valuesHTML || '') + `</div>`;
}

// ocChipStripHTML is the one multi-dimension selector: one wrapping strip of
// dimension chips, with the single open picker spanning the full width below
// the strip instead of being squeezed into one chip's track. The scope editor
// and the export filter both render through it, so there is one layout rule
// and one open-at-a-time contract for every multi-dimension choice.
function ocChipStripHTML(chips, picker) {
  return `<div class="oc-conds">${chips.join('')}${picker || ''}</div>`;
}

// ocScopeConditionsHTML renders the policy selector through the shared strip.
function ocScopeConditionsHTML(draft, dims) {
  let picker = '';
  const chips = dims.map(dim => {
    const values = draft.scope[dim] || [];
    const open = !!draft.picker && draft.picker.dim === dim;
    if (open) picker = ocScopePickerHTML(draft, dim);
    return ocDimChipHTML({
      dim, label: ocDimLabel(dim), open, set: values.length > 0,
      attr: 'data-oc-pick', key: 'cond-' + dim, menuId: 'oc-pick-menu-' + dim,
      valuesHTML: values.length ? `<span class="oc-cond-values">${ocScopeChipsHTML(draft.scope, dim)}</span>` : '',
    });
  });
  return ocChipStripHTML(chips, picker);
}

// ocScopePickerHTML is the one dimension picker: known values as checkable
// rows plus a free-value field, inline rather than a nested popover because a
// policy editor with four chained popovers is where the old menus lost their
// place. The filter only appears once the list is long enough to need it.
function ocScopePickerHTML(draft, dim) {
  const filter = (draft.picker.filter || '').toLowerCase();
  const all = ocKnownValues(dim);
  const known = all.filter(value =>
    !filter || ocScopeValueLabel(dim, value).toLowerCase().includes(filter));
  const selected = new Set(draft.scope[dim] || []);
  const rows = known.map((value, index) => {
    const on = selected.has(value);
    const label = ocScopeValueLabel(dim, value);
    // A menu is one tab stop: arrows move the cursor, Tab leaves the list.
    return `<button type="button" class="oc-pick-row" role="menuitemcheckbox" aria-checked="${on}" tabindex="${index === 0 ? '0' : '-1'}" data-oc-pick-value="${dim}" data-oc-value="${escapeHtml(value)}">` +
      `<span class="oc-pick-check" aria-hidden="true">${on ? '✓' : ''}</span><span class="oc-pick-label">${escapeHtml(label)}</span></button>`;
  }).join('');
  const empty = known.length ? '' : `<div class="oc-pick-empty">${NONE_YET_NOTE}</div>`;
  const canAdd = dim !== 'keyHashes';
  const showFilter = all.length > 6;
  // The rule that an empty dimension matches everything lived only in the
  // pause composer's prose, where it was invisible at the moment of choice.
  // Saying it inside the picker is what stops a close-with-nothing-checked
  // from reading as a narrowing.
  const wide = selected.size ? '' :
    `<p class="oc-pick-note">Nothing selected, so ${ocDimLabel(dim)} does not narrow this rule.</p>`;
  return `<div class="oc-pick" data-oc-key="pick-${dim}">` +
    (showFilter ? `<input type="search" class="oc-pick-filter" data-oc-pick-filter="${dim}" value="${escapeHtml(draft.picker.filter || '')}" placeholder="filter ${ocDimLabel(dim)}s" aria-label="filter ${ocDimLabel(dim)}s" data-oc-key="pick-filter-${dim}">` : '') +
    `<div class="oc-pick-list" id="oc-pick-menu-${dim}" role="menu" aria-label="${ocDimLabel(dim)} values">${rows}${empty}</div>` +
    wide +
    (canAdd ? `<div class="oc-pick-custom"><input type="text" data-oc-pick-custom="${dim}" placeholder="add a ${ocDimLabel(dim)}" aria-label="add a ${ocDimLabel(dim)}" data-oc-key="pick-custom-${dim}"><button type="button" class="btn" data-oc-pick-add="${dim}">add</button></div>` : '') +
    `</div>`;
}

// ocOperatorError is the console's one server-error surface. chrome_operator's
// mutation gate calls it so a rejected write is named in the pane the operator
// is looking at, not painted into a hidden menu line.
// ocSyncErrorBanner owns the rejection banner as one stable node.
//
// The banner sits directly in the pane and the section's markup goes into
// #oc-pane-body, which every render replaces. That split is what keeps the
// banner's node identity: a banner that rode the rebuilt markup was a fresh
// role="alert" on every unrelated poll, so assistive tech re-announced the
// same rejection for as long as it stayed up. It is created when an error
// appears, patched in place when the text changes, and removed when the error
// clears, so each rejection is announced exactly once.
function ocSyncErrorBanner(pane) {
  if (!pane) return;
  const existing = pane.querySelector(':scope > .oc-banner[data-tone="err"]');
  if (!ocError) {
    if (existing) existing.remove();
    return;
  }
  if (existing) {
    const text = existing.querySelector('span');
    if (text && text.textContent !== ocError) text.textContent = ocError;
    return;
  }
  const banner = document.createElement('div');
  banner.className = 'oc-banner';
  banner.dataset.tone = 'err';
  banner.setAttribute('role', 'alert');
  banner.innerHTML = `<span>${escapeHtml(ocError)}</span>` +
    `<button type="button" class="oc-link" data-oc-error-dismiss>dismiss</button>`;
  pane.insertBefore(banner, pane.firstChild);
}

function ocOperatorError(message) {
  ocError = String(message || 'the change was rejected');
  // The banner is the rejection's visible surface while the console is open,
  // so a toast would announce the same sentence twice. A closed console has no
  // banner to show, and there the toast is the only way the operator learns.
  if (!ocMounted) toast(ocError, 'err');
  if (ocMounted) ocRender();
}

// ocRuleGone is the one answer to a control whose rule disappeared between
// paint and click. A poll can drop a hold, a capture or a limit while its row
// is still on screen, and the lookup then returned early: the click produced
// no banner, no toast and no repaint, so it read as a control that does not
// work.
function ocRuleGone(noun) {
  ocOperatorError('that ' + noun + ' is no longer in force');
}

function ocOperatorErrorClear() {
  if (!ocError) return;
  ocError = '';
  if (ocMounted) ocRender();
}

// ---------- change ledger ----------
// ocRecordChange is the one change receipt. It writes a human phrase, keeps a
// bounded session history and stores the undo closure that restores the exact
// prior policy. A change the operator cannot see or reverse was the old
// menus' core failure.
function ocRecordChange(phrase, undo, tone) {
  ocError = '';
  ocChanges.push({ phrase, undo: typeof undo === 'function' ? undo : null, tone: tone || '', at: Date.now() });
  if (ocChanges.length > OC_CHANGE_LIMIT) ocChanges.shift();
  ocRenderFoot();
  refreshOperatorReadout();
}

async function ocUndoLast() {
  const last = ocChanges[ocChanges.length - 1];
  if (!last || typeof last.undo !== 'function' || ocUndoBusy) return;
  ocUndoBusy = true;
  ocRenderFoot();
  try {
    await last.undo();
    ocChanges.pop();
    ocRecordChange('undid: ' + last.phrase, null, 'ok');
  } catch (err) {
    last.tone = 'err';
    last.phrase = 'could not undo: ' + String(err.message || err);
    ocRenderFoot();
  } finally {
    ocUndoBusy = false;
    ocRender();
  }
}

// ---------- confirmation ----------
// ocConfirm is the one destructive-action dialog. It replaces the old
// two-click button whose armed state was invisible to anyone who looked away.
// It resolves true only for the explicit confirm action.
let ocConfirmSettle = null;
function ocConfirm({ title, body, confirm, danger }) {
  // One confirmation at a time. A second call settles the open dialog as a
  // cancel before replacing it, so neither promise is left pending and the
  // modal layer never points at a detached node.
  if (ocConfirmSettle) {
    const settle = ocConfirmSettle;
    ocConfirmSettle = null;
    settle(false);
  }
  return new Promise(resolve => {
    const id = 'oc-confirm-dialog';
    let dialog = ocEl(id);
    if (dialog) dialog.remove();
    dialog = buildModalDialog(id, 'operator-dialog', 'oc-confirm-title',
      `<div class="operator-dialog-panel oc-confirm-panel">` +
      `<h3 id="oc-confirm-title">${escapeHtml(title)}</h3>` +
      `<p class="oc-confirm-body">${escapeHtml(body)}</p>` +
      `<div class="operator-dialog-actions">` +
      `<button type="button" class="btn" data-oc-confirm="cancel">cancel</button>` +
      `<button type="button" class="btn ${danger ? 'btn-danger-solid' : 'btn-accent'}" data-oc-confirm="ok">${escapeHtml(confirm)}</button>` +
      `</div></div>`);
    let settled = false;
    const finish = value => {
      if (settled) return;
      settled = true;
      if (ocConfirmSettle === finish) ocConfirmSettle = null;
      closeModal(dialog);
      dialog.remove();
      resolve(value);
    };
    ocConfirmSettle = finish;
    wireDialogDismiss(dialog, () => finish(false));
    dialog.querySelector('[data-oc-confirm="cancel"]').onclick = () => finish(false);
    dialog.querySelector('[data-oc-confirm="ok"]').onclick = () => finish(true);
    // The rule that opened the dialog may be replaced by a poll repaint while
    // the dialog is up; the pane is the stable focus home in that case.
    openModal(dialog, { returnSelector: '#oc-pane' });
    dialog.querySelector('[data-oc-confirm="cancel"]').focus();
  });
}

// ---------- shared composer pieces ----------
// ocChipRow is the one closed-vocabulary picker for short lists: a wrapping row
// of pressed chips, where every choice is one click and all of them are
// visible at once.
function ocChipRow(name, values, current, labelOf) {
  return `<div class="oc-chips" role="group" aria-label="${escapeHtml(name)}">` +
    values.map(value => {
      const pressed = String(current) === String(value) ? 'true' : 'false';
      return `<button type="button" class="oc-chip oc-chip-pick" aria-pressed="${pressed}" data-oc-choice="${escapeHtml(name)}" data-oc-value="${escapeHtml(String(value))}">${escapeHtml(labelOf(value))}</button>`;
    }).join('') + `</div>`;
}

// ocChoiceSelect is the same closed vocabulary for a long list in a narrow
// track. A ten-chip ladder cannot show ten labels in the width of one number
// field, so it either wraps into three rows or clips mid-word; a native select
// states the current window in the same 24px and hands long lists to the
// platform's own keyboard and touch pickers.
function ocChoiceSelect(name, values, current, labelOf, aria) {
  return `<select class="oc-select" data-oc-choice="${escapeHtml(name)}" aria-label="${escapeHtml(aria || name)}">` +
    values.map(value => {
      const selected = String(current) === String(value) ? ' selected' : '';
      return `<option value="${escapeHtml(String(value))}"${selected}>${escapeHtml(labelOf(value))}</option>`;
    }).join('') + `</select>`;
}

function ocWindowLabel(value) {
  const hit = LIMIT_WINDOWS.find(([v]) => v === value);
  return hit ? hit[1] : value;
}

function ocNumber(value) {
  if (value === '' || value == null) return 0;
  const n = Number(value);
  return Number.isSafeInteger(n) && n >= 0 ? n : NaN;
}

// ocField is the one labelled input. A bare field carries its name as an
// aria-label instead of a visible line: the surrounding row already names the
// dimension, and a second caption above every number is pure noise.
function ocField(key, label, value, opts = {}) {
  const type = opts.type || 'number';
  const attrs = type === 'number' ? 'min="0" step="1" inputmode="numeric"' : '';
  const name = opts.aria || label;
  return `<label class="oc-field${opts.wide ? ' wide' : ''}${opts.bare ? ' bare' : ''}" data-oc-key="${escapeHtml(key)}">` +
    (opts.bare ? '' : `<span class="oc-field-name">${escapeHtml(label)}</span>`) +
    `<input type="${type}" ${attrs} data-oc-field="${escapeHtml(key)}" data-oc-key="field-${escapeHtml(key)}"` +
    (name ? ` aria-label="${escapeHtml(name)}"` : '') +
    ` value="${escapeHtml(String(value == null ? '' : value))}" placeholder="${escapeHtml(opts.placeholder || '')}" ${opts.disabled ? 'disabled' : ''}>` +
    (opts.hint ? `<span class="oc-field-hint">${escapeHtml(opts.hint)}</span>` : '') +
    `</label>`;
}

function ocTimeValue(minutes) {
  if (minutes == null) return '';
  const m = Math.max(0, Math.min(1439, Number(minutes) || 0));
  return String(Math.floor(m / 60)).padStart(2, '0') + ':' + String(m % 60).padStart(2, '0');
}

function ocParseTime(value) {
  const m = /^(\d{1,2}):(\d{2})$/.exec(String(value || ''));
  if (!m) return 0;
  const hour = Number(m[1]), minute = Number(m[2]);
  if (hour > 23 || minute > 59) return 0;
  return hour * 60 + minute;
}

// ---------- system ----------
function ocRenderSystem() {
  queueMicrotask(() => {
    if (ocSection === 'system' && typeof fetchRestartStatus === 'function') fetchRestartStatus();
  });
  const available = restartServerState.available !== false;
  const reason = restartServerState.reason || 'This build cannot restart in place. Update the container or service externally.';
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><p class="oc-sub">Process lifecycle and the paths that live outside a policy.</p></div>` +
    `<div class="oc-card">` +
    `<div class="oc-label">rebuild and restart</div>` +
    `<p id="restart-description" class="oc-note">Build the latest source, finish in-flight streams, and save metrics before the new process takes over. New requests queue during the handoff.</p>` +
    `<ol class="restart-steps" id="restart-steps" hidden aria-live="polite"></ol>` +
    (available ? '' : `<p class="oc-warning">${escapeHtml(reason)}</p>`) +
    `<div class="oc-compose-ft"><span class="clear-count" id="restart-count" role="status"></span>` +
    `<button type="button" class="btn btn-accent" id="btn-restart-now" data-oc-restart ${available ? '' : 'disabled'}>restart now</button></div>` +
    `</div>` +
    `<div class="oc-card"><div class="oc-label">settings</div>` +
    `<p class="oc-note">Queue bounds, retry budgets, storage, retention and dashboard cadence live in Settings.</p>` +
    `<button type="button" class="btn" data-oc-settings>open settings</button></div>` +
    `</div>`;
}

// ocSyncRestartVerdict patches the system section's eligibility sentence in
// place. A status poll can flip the verdict while the operator is looking at
// the section, and rebuilding the section there would schedule the next poll
// from the render path.
function ocSyncRestartVerdict() {
  if (ocSection !== 'system' || !ocMounted) return;
  const card = ocEl('oc-pane')?.querySelector('.oc-card');
  if (!card) return;
  const available = restartServerState.available !== false;
  const existing = card.querySelector('.oc-warning');
  if (available) {
    if (existing) existing.remove();
    return;
  }
  const reason = restartServerState.reason || 'This build cannot restart in place. Update the container or service externally.';
  const para = existing || document.createElement('p');
  para.className = 'oc-warning';
  if (para.textContent !== reason) para.textContent = reason;
  if (!existing) {
    const steps = card.querySelector('#restart-steps');
    if (steps) steps.after(para); else card.appendChild(para);
  }
}

// A resize can cross the sheet breakpoint without a repaint, so the announced
// orientation is resynced from the same owner the render path uses.
window.addEventListener('resize', () => {
  if (ocMounted) { ocSyncRailOrientation(); ocSyncRailOverflow(); }
});


// The strip's own scroll changes whether more sections remain.
document.addEventListener('scroll', event => {
  if (event.target?.id === 'oc-rail') ocSyncRailOverflow();
}, true);

// ---------- live sync ----------
// operatorConsoleSync is the operator state registry's one repaint hook. It
// keeps the readout truthful even when the console is closed, and refreshes
// only the open console's surface. The pane repaint preserves focus, caret and
// scroll identity itself, so there is no separate display-only path.
function operatorConsoleSync(kind) {
  refreshOperatorReadout();
  const shell = ocShell();
  if (!shell || !ocMounted || !menuIsOpen(shell)) return;
  const section = kind === 'debug' ? 'capture' : kind === 'throttle' ? 'limits' : kind;
  // The overview is the live "in force" summary, so any policy change repaints
  // it. Every other section keeps the operator's place unless its own kind
  // changed: a capture poll must not rebuild an export filter mid-typing.
  if (section !== ocSection && ocSection !== 'overview') {
    ocRenderRail();
    return;
  }
  ocRender();
}

// ---------- event dispatch ----------
// ocActiveDraft is the composer draft of the section on screen. Every action
// that writes into the open form goes through it, so a new section cannot
// forget to teach six separate ternaries about itself.
function ocActiveDraft() {
  if (ocSection === 'limits') return ocDrafts.limit;
  if (ocSection === 'pause') return ocDrafts.pause;
  if (ocSection === 'capture') return ocDrafts.capture;
  return null;
}

function ocCurrentKind() {
  if (ocSection === 'limits') return 'limit';
  if (ocSection === 'pause') return 'pause';
  if (ocSection === 'capture') return 'capture';
  if (ocSection === 'export') return 'export';
  return '';
}

function ocClearComposer() {
  const kind = ocCurrentKind();
  if (kind === 'limit') ocDrafts.limit = ocBlankLimitDraft();
  if (kind === 'pause') ocDrafts.pause = ocBlankPauseDraft();
  if (kind === 'capture') ocDrafts.capture = ocBlankCaptureDraft();
  ocView = { kind: '', id: '' };
}

function ocTogglePicker(draft, dim) {
  if (!draft) return;
  if (draft.picker && draft.picker.dim === dim) {
    draft.picker = null;
    ocPickerFocus = '';
  } else {
    draft.picker = { dim, filter: '' };
    ocPickerFocus = dim;
  }
  ocRender();
}

async function ocHandleAction(target) {
  const section = target.dataset.ocSection;
  if (section) {
    // An overview row names one rule, so it opens that rule rather than
    // dropping the operator at the top of the section's list to search for it.
    const rule = target.dataset.ocTarget || '';
    ocSelectSection(section);
    if (rule) {
      if (section === 'limits') ocOpenLimitComposer(rule);
      if (section === 'pause') ocOpenPauseComposer(rule);
      if (section === 'capture') ocOpenCaptureComposer(rule);
    }
    return;
  }
  if (target.hasAttribute('data-oc-list-clear')) {
    ocListFilter[target.dataset.ocListClear] = '';
    ocRender();
    return;
  }
  if (target.hasAttribute('data-oc-error-dismiss')) { ocOperatorErrorClear(); return; }
  if (target.hasAttribute('data-oc-close')) { closeOperatorConsole(true); return; }
  if (target.hasAttribute('data-oc-cancel')) { ocClearComposer(); ocRender(); return; }
  const newKind = target.dataset.ocNew;
  if (newKind) {
    if (newKind === 'limit') ocOpenLimitComposer('');
    if (newKind === 'pause') ocOpenPauseComposer('');
    if (newKind === 'capture') ocOpenCaptureComposer('');
    return;
  }
  // A copy reuses the source rule's scope and schedule with no budgets, so a
  // follow-up rule starts from what already worked instead of four pickers.
  if (target.hasAttribute('data-oc-quick-hold')) { ocQuickHold(); return; }
  const copyKind = target.dataset.ocCopy;
  if (copyKind) {
    const id = target.dataset.ocId || '';
    if (copyKind === 'limit') ocOpenLimitComposer(id, true);
    if (copyKind === 'pause') ocOpenPauseComposer(id, true);
    if (copyKind === 'capture') ocOpenCaptureComposer(id, true);
    return;
  }
  const editKind = target.dataset.ocEdit;
  if (editKind) {
    const id = target.dataset.ocId || '';
    if (editKind === 'limit') ocOpenLimitComposer(id);
    if (editKind === 'pause') ocOpenPauseComposer(id);
    if (editKind === 'capture') ocOpenCaptureComposer(id);
    return;
  }
  const removeKind = target.dataset.ocRemove;
  if (removeKind) {
    if (removeKind === 'limit') ocRemoveLimit(target.dataset.ocId || '');
    return;
  }
  const resume = target.getAttribute('data-oc-resume');
  if (resume) { ocResumeHold(resume); return; }
  if (target.hasAttribute('data-oc-resume-all')) { ocResumeAllHolds(); return; }
  const stop = target.getAttribute('data-oc-stop');
  if (stop) { ocStopSession(stop); return; }
  if (target.hasAttribute('data-oc-stop-all')) { ocStopAllSessions(); return; }
  const apply = target.dataset.ocApply;
  if (apply === 'limit') { ocApplyLimit(); return; }
  if (apply === 'pause') { ocApplyPause(); return; }
  if (apply === 'capture') { ocApplyCapture(); return; }
  const pick = target.dataset.ocPick;
  if (pick) {
    const draft = ocActiveDraft();
    if (!draft) return;
    ocTogglePicker(draft, pick);
    return;
  }
  if (target.dataset.ocScopeRemove) {
    const draft = ocActiveDraft();
    if (!draft) return;
    ocScopeToggle(ocDraftScope(draft), target.dataset.ocScopeRemove, target.dataset.ocValue || '');
    draft.picker = null;
    ocRender();
    return;
  }
  const pickValue = target.dataset.ocPickValue;
  if (pickValue) {
    const draft = ocActiveDraft();
    if (!draft) return;
    ocScopeToggle(ocDraftScope(draft), pickValue, target.dataset.ocValue || '');
    ocRender();
    return;
  }
  const pickAdd = target.dataset.ocPickAdd;
  if (pickAdd) {
    const draft = ocActiveDraft();
    const input = ocShell()?.querySelector(`[data-oc-pick-custom="${pickAdd}"]`);
    const value = (input?.value || '').trim();
    if (draft && value) { ocScopeToggle(ocDraftScope(draft), pickAdd, value); ocRender(); }
    return;
  }
  const toggle = target.dataset.ocToggle;
  if (toggle === 'advanced') {
    if (ocDrafts.limit) { ocDrafts.limit.advanced = !ocDrafts.limit.advanced; ocRender(); }
    return;
  }
  if (toggle === 'schedule') {
    if (ocDrafts.limit) {
      ocDrafts.limit.schedule = ocDrafts.limit.schedule ? null : { days: [], start: 540, end: 1020 };
      if (!ocDrafts.limit.schedule && ocDrafts.limit.antiBurst) ocDrafts.limit.antiBurst = false;
      ocRender();
    }
    return;
  }
  const day = target.getAttribute('data-oc-day');
  if (day != null && ocDrafts.limit?.schedule) {
    const days = ocDrafts.limit.schedule.days;
    const n = Number(day);
    const at = days.indexOf(n);
    if (at >= 0) days.splice(at, 1); else days.push(n);
    ocRender();
    return;
  }
  const choice = target.dataset.ocChoice;
  if (choice) { ocHandleChoice(choice, target.dataset.ocValue || ''); return; }
  if (target.dataset.ocFilterPick) { ocToggleFilterPick(target.dataset.ocFilterPick); return; }
  if (target.dataset.ocFilterValue != null) {
    const draft = ocDrafts.export;
    if (draft) {
      draft[target.dataset.ocFilterValue] = target.dataset.ocValue || '';
      draft.picker = null;
      ocScheduleExportCount();
      ocRender();
    }
    return;
  }
  if (target.dataset.ocFilterClear) {
    const draft = ocDrafts.export;
    if (draft) {
      draft[target.dataset.ocFilterClear] = '';
      draft.picker = null;
      ocScheduleExportCount();
      ocRender();
    }
    return;
  }
  if (target.dataset.ocFilterAdd) {
    const dim = target.dataset.ocFilterAdd;
    const input = ocShell()?.querySelector(`[data-oc-filter-custom="${dim}"]`);
    const value = (input?.value || '').trim();
    if (value && ocDrafts.export) {
      ocDrafts.export[dim] = value;
      ocDrafts.export.picker = null;
      ocScheduleExportCount();
      ocRender();
    }
    return;
  }
  if (target.hasAttribute('data-oc-download')) { ocDownloadExport(); return; }
  if (target.hasAttribute('data-oc-reset-export')) {
    ocDrafts.export = ocBlankExportDraft();
    ocRender();
    return;
  }
  if (target.hasAttribute('data-oc-settings')) {
    // A warning that names one setting carries the category, so the sheet
    // opens on the pane that holds it instead of its first pane.
    const category = target.dataset.ocSettings || '';
    closeOperatorConsole();
    if (category && typeof openSettingsAt === 'function') openSettingsAt(category);
    else toggleSettings();
    return;
  }
  if (target.hasAttribute('data-oc-drain')) { ocDrainQueue(); return; }
  if (target.hasAttribute('data-oc-restart')) {
    if (typeof restartProxy === 'function') restartProxy();
    return;
  }
  if (target.hasAttribute('data-oc-undo')) { ocUndoLast(); return; }
}

// ocHandleChoice writes a closed-vocabulary chip pick into the active draft.
function ocHandleChoice(name, value) {
  // The drain scope lives in the overview, which is not a composer kind.
  if (name === 'drain-scope') {
    ocDrain.scope = value;
    ocDrain.value = '';
    ocRender();
    return;
  }
  const kind = ocCurrentKind();
  if (kind === 'limit' && ocDrafts.limit) {
    const draft = ocDrafts.limit;
    if (name === 'request-window') draft.requestWindow = value;
    if (name === 'token-window') draft.tokenWindow = value;
    if (name === 'ramp') draft.ramp = value;
    if (name === 'concurrency') draft.concurrency = value;
    ocRender();
    return;
  }
  if (kind === 'pause' && ocDrafts.pause) {
    if (name === 'pause-mode') { ocDrafts.pause.mode = value; ocDrafts.pause.picker = null; }
    if (name === 'pause-duration') ocDrafts.pause.duration = value;
    ocRender();
    return;
  }
  if (kind === 'capture' && ocDrafts.capture) {
    if (name === 'capture-duration') ocDrafts.capture.duration = value;
    ocRender();
    return;
  }
  if (kind === 'export' && ocDrafts.export) {
    const draft = ocDrafts.export;
    // A mode switch invalidates the count: the predicate the receipt quoted
    // belonged to the previous mode, so keeping it would state a total beside
    // a download that acts on a different one.
    if (name === 'export-mode') {
      draft.mode = value;
      draft.count = null;
      draft.counting = false;
      draft.error = '';
    }
    if (name === 'export-errors') draft.errors = value === 'only';
    if (name === 'export-debug') draft.debug = value === 'only';
    if (name === 'export-after') draft.after = value;
    if (name === 'export-age') draft.age = value;
    ocScheduleExportCount();
    ocRender();
  }
}

function ocToggleFilterPick(dim) {
  const draft = ocDrafts.export;
  if (!draft) return;
  if (draft.picker && draft.picker.dim === dim) {
    draft.picker = null;
    ocPickerFocus = '';
  } else {
    draft.picker = { dim, filter: '' };
    ocPickerFocus = dim;
  }
  ocRender();
}

// ocInputField writes a text or number field into the active draft without a
// rebuild, so the caret never moves while typing. The preview and the commit
// gate are the only things that repaint.
function ocInputField(key, value) {
  const kind = ocCurrentKind();
  if (kind === 'limit' && ocDrafts.limit) {
    ocDrafts.limit[key] = value;
    ocLimitPreview();
    return;
  }
  if (kind === 'pause' && ocDrafts.pause) {
    if (key === 'pauseCap') ocDrafts.pause.cap = value;
    ocPausePreview();
    return;
  }
  if (kind === 'export' && ocDrafts.export) {
    if (key === 'errorType') { ocDrafts.export.errorType = value; ocScheduleExportCount(); }
  }
}

document.addEventListener('input', event => {
  const target = event.target;
  if (!target || !ocShell()?.contains(target)) return;
  if (target.dataset.ocField) {
    if (target.type === 'checkbox') return;
    ocInputField(target.dataset.ocField, target.value);
    return;
  }
  if (target.dataset.ocPickFilter) {
    const draft = ocActiveDraft();
    if (draft?.picker) { draft.picker.filter = target.value; ocRender(); }
    return;
  }
  if (target.dataset.ocFilterFilter) {
    if (ocDrafts.export?.picker) { ocDrafts.export.picker.filter = target.value; ocRender(); }
    return;
  }
  if (target.dataset.ocListFilter) {
    ocListFilter[target.dataset.ocListFilter] = target.value;
    ocRender();
    return;
  }
  if (target.dataset.ocExportField) {
    ocInputField(target.dataset.ocExportField, target.value);
    return;
  }
  if (target.hasAttribute('data-oc-drain-value')) {
    ocDrain.value = target.value;
  }
});

document.addEventListener('change', event => {
  const target = event.target;
  if (!target || !ocShell()?.contains(target)) return;
  if (target.tagName === 'SELECT' && target.dataset.ocChoice) {
    ocHandleChoice(target.dataset.ocChoice, target.value);
    return;
  }
  if (target.dataset.ocBudget) {
    const draft = ocDrafts.limit;
    if (!draft) return;
    const key = target.dataset.ocBudget;
    // Arming a budget only opens its field. Writing a plausible default here
    // turned one discovery click into a live cap: a stray click on
    // "concurrency" then Apply would install a 4-in-flight limit the operator
    // never chose. The preview's "set at least one budget" gate is correct.
    draft.armed = draft.armed || ocBlankBudgetArmed();
    draft.armed[key] = !!target.checked;
    if (!target.checked) {
      draft[key] = '';
      if (key === 'concurrency') { draft.antiBurst = false; draft.ramp = ''; }
    }
    ocRender();
    return;
  }
  if (target.dataset.ocField === 'antiBurst') {
    if (ocDrafts.limit) { ocDrafts.limit.antiBurst = target.checked; ocRender(); }
    return;
  }
  if (target.dataset.ocField === 'pauseCapOn') {
    if (ocDrafts.pause) { ocDrafts.pause.cap = target.checked ? String(pauseState.default_max_queued || 1024) : ''; ocRender(); }
    return;
  }
  if (target.dataset.ocTime && ocDrafts.limit?.schedule) {
    const minutes = ocParseTime(target.value);
    if (target.dataset.ocTime === 'start') ocDrafts.limit.schedule.start = minutes;
    else ocDrafts.limit.schedule.end = minutes;
    ocRender();
  }
});

document.addEventListener('keydown', event => {
  if (event.key === 'Enter') {
    const target = event.target;
    if (!target || !ocShell()?.contains(target)) return;
    if (target.dataset.ocPickCustom) {
      const dim = target.dataset.ocPickCustom;
      const draft = ocActiveDraft();
      const value = target.value.trim();
      if (draft && value) {
        event.preventDefault();
        ocScopeToggle(ocDraftScope(draft), dim, value);
        ocRender();
      }
    }
    if (target.dataset.ocFilterCustom) {
      const dim = target.dataset.ocFilterCustom;
      const value = target.value.trim();
      if (value && ocDrafts.export) {
        event.preventDefault();
        ocDrafts.export[dim] = value;
        ocDrafts.export.picker = null;
        ocScheduleExportCount();
        ocRender();
      }
    }
    return;
  }
  // The rail is a real tablist: arrows, Home and End move selection and
  // focus together. The strip is horizontal, so both axes are accepted; Tab
  // still reaches the active tab and the pane in order.
  const step = (event.key === 'ArrowDown' || event.key === 'ArrowRight') ? 1
    : (event.key === 'ArrowUp' || event.key === 'ArrowLeft') ? -1 : 0;
  const jump = event.key === 'Home' || event.key === 'End';
  if (!step && !jump) return;
  const tab = document.activeElement?.closest?.('.oc-rail-item');
  if (!tab || !ocShell()?.contains(tab)) return;
  const tabs = [...ocShell().querySelectorAll('.oc-rail-item')];
  if (!tabs.length) return;
  event.preventDefault();
  const at = tabs.indexOf(tab);
  const next = jump
    ? tabs[event.key === 'Home' ? 0 : tabs.length - 1]
    : tabs[(at + step + tabs.length) % tabs.length];
  ocSelectSection(next.dataset.ocSection);
  ocShell()?.querySelector(`[data-oc-key="rail-${next.dataset.ocSection}"]`)?.focus();
});

// ocClosePicker closes the one open dimension picker and returns focus to the
// chip that opened it. It reports whether anything closed, so Escape can fall
// through to the panel close when no picker is open.
function ocClosePicker() {
  const draft = ocSection === 'export' ? ocDrafts.export : ocActiveDraft();
  if (!draft || !draft.picker) return false;
  const dim = draft.picker.dim;
  draft.picker = null;
  ocRender();
  const key = (ocSection === 'export' ? 'filter-' : 'cond-') + dim;
  ocShell()?.querySelector(`[data-oc-key="${escapeHtml(key)}"]`)?.focus();
  return true;
}

// The picker promises a menu, so it honours the menu keys: arrows and Home/End
// move between rows, and Escape closes the picker instead of the whole panel.
// Capture phase is required because menu.js owns Escape in capture and would
// otherwise dismiss the console from under an open picker, losing the draft
// position the operator was working in.
document.addEventListener('keydown', event => {
  const shell = ocShell();
  if (!ocMounted || !shell || !shell.contains(event.target)) return;
  const row = event.target.closest?.('.oc-pick-row');
  const list = row?.closest('.oc-pick-list');
  if (list) {
    const rows = [...list.querySelectorAll('.oc-pick-row')]
      .filter(r => !r.disabled && !r.hidden);
    const at = rows.indexOf(row);
    let next = -1;
    if (event.key === 'ArrowDown') next = (at + 1) % rows.length;
    else if (event.key === 'ArrowUp') next = (at - 1 + rows.length) % rows.length;
    else if (event.key === 'Home') next = 0;
    else if (event.key === 'End') next = rows.length - 1;
    if (next >= 0 && rows[next]) {
      event.preventDefault();
      event.stopImmediatePropagation();
      menuMoveRoving(rows, next);
      return;
    }
  }
  if (event.key === 'Escape' && ocClosePicker()) {
    event.preventDefault();
    // stopImmediatePropagation, not stopPropagation: menu.js installs its own
    // capture-phase keydown on this same document node when the panel opens,
    // so a plain stopPropagation leaves the panel's close to run anyway and
    // dismisses the console the operator was still working in.
    event.stopImmediatePropagation();
  }
}, true);

document.addEventListener('click', event => {
  // A choice select is a native control: it opens its own list, so the
  // delegated action handler must not preventDefault its click. Its change
  // event carries the selection instead.
  if (event.target?.dataset?.ocChoice && event.target.tagName === 'SELECT') return;
  const target = event.target.closest?.('[data-oc-section],[data-oc-close],[data-oc-error-dismiss],[data-oc-cancel],[data-oc-new],[data-oc-list-clear],[data-oc-edit],[data-oc-copy],[data-oc-quick-hold],[data-oc-remove],[data-oc-resume],[data-oc-resume-all],[data-oc-stop],[data-oc-stop-all],[data-oc-apply],[data-oc-pick],[data-oc-scope-remove],[data-oc-pick-value],[data-oc-pick-add],[data-oc-toggle],[data-oc-day],[data-oc-choice],[data-oc-filter-pick],[data-oc-filter-value],[data-oc-filter-clear],[data-oc-filter-add],[data-oc-download],[data-oc-reset-export],[data-oc-settings],[data-oc-drain],[data-oc-restart],[data-oc-undo]');
  if (!target || !ocShell()?.contains(target)) return;
  event.preventDefault();
  ocHandleAction(target);
});

// ---------- record-driven entry points ----------
// A record's live pill opens the rule that produced it, already in its edit
// transition, or starts the same kind of rule pre-scoped to that request when
// the rule has already ended. The operator never has to translate a request
// into four pickers first.

function ocPolicyMatchesRecord(policy, record) {
  const scope = policy?.scope || {};
  const ok = (values, value) => !(values || []).length || (values || []).includes(value || '');
  return ok(scope.providers, record.provider) && ok(scope.models, record.model) &&
    ok(scope.clients, record.client) && ok(scope.key_hashes, record.key_hash) &&
    ok(scope.conversations, record.conversation_id);
}

// ocScopeWeight ranks matches so a model rule outranks the global rule when
// both parked the same request. Summing non-empty dimensions is enough to
// make the choice stable without inventing a precedence the server has not
// promised.
function ocScopeWeight(scope) {
  return ['providers', 'models', 'clients', 'key_hashes', 'conversations']
    .reduce((n, key) => n + ((scope?.[key] || []).length ? 1 : 0), 0);
}

function ocHoldMatchesRecord(hold, record) {
  if (!hold || !record) return false;
  if (hold.all) return true;
  const clients = hold.clients || [];
  const providers = hold.providers || [];
  const known = hold.known_at_new || [];
  const client = record.client || '';
  let clientOK = false;
  if (clients.length && clients.includes(client)) clientOK = true;
  else if (hold.new) clientOK = !known.includes(client);
  else if (clients.length) clientOK = false;
  else clientOK = providers.length > 0;
  if (!clientOK) return false;
  return !providers.length || providers.includes(record.provider || '');
}

function ocCaptureMatchesRecord(session, record) {
  if (!session || !record) return false;
  if ((session.clients || []).length && !session.clients.includes(record.client || '')) return false;
  if ((session.providers || []).length && !session.providers.includes(record.provider || '')) return false;
  if ((session.models || []).length && !session.models.includes(record.model || '')) return false;
  return !!(session.clients || []).length || !!(session.providers || []).length ||
    !!(session.models || []).length;
}

function ocLimitForRecord(record) {
  const policies = ((typeof throttleState !== 'undefined' && throttleState.throttles) || [])
    .filter(policy => ocPolicyMatchesRecord(policy, record))
    .sort((a, b) => ocScopeWeight(b.scope) - ocScopeWeight(a.scope));
  if (policies.length) {
    openOperatorConsole('limits');
    ocOpenLimitComposer(policies[0].id);
    return;
  }
  openOperatorConsole('limits');
  const draft = ocBlankLimitDraft();
  if (record.provider) draft.scope.providers = [record.provider];
  if (record.model) draft.scope.models = [record.model];
  ocDrafts.limit = draft;
  ocView = { kind: 'limit', id: '' };
  ocRender();
}

function ocPauseForRecord(record) {
  if (!record) return;
  const holds = liveHolds();
  const hold = holds.find(h => !h.all && ocHoldMatchesRecord(h, record)) || holds.find(h => h.all);
  if (hold) {
    openOperatorConsole('pause');
    ocOpenPauseComposer(hold.id);
    return;
  }
  openOperatorConsole('pause');
  const draft = ocBlankPauseDraft();
  if (record.client) draft.clients = [record.client];
  if (record.provider) draft.providers = [record.provider];
  if (!draft.clients.length && !draft.providers.length) draft.mode = 'all';
  ocDrafts.pause = draft;
  ocView = { kind: 'pause', id: '' };
  ocRender();
}

function ocCaptureForRecord(record) {
  if (!record) return;
  const sessions = liveDebugSessions();
  const stamped = record.debug_session_id && sessions.find(s => s.id === record.debug_session_id);
  const session = stamped || sessions.find(s => ocCaptureMatchesRecord(s, record));
  if (session) {
    openOperatorConsole('capture');
    ocOpenCaptureComposer(session.id);
    return;
  }
  openOperatorConsole('capture');
  const draft = ocBlankCaptureDraft();
  if (record.client) draft.clients = [record.client];
  if (record.provider) draft.providers = [record.provider];
  if (record.model) draft.models = [record.model];
  ocDrafts.capture = draft;
  ocView = { kind: 'capture', id: '' };
  ocRender();
}

// ocOpenOperatorForRecord is the one dispatch for the three record pills.
function ocOpenOperatorForRecord(kind, record) {
  if (!record) return;
  if (kind === 'limit') ocLimitForRecord(record);
  else if (kind === 'pause') ocPauseForRecord(record);
  else if (kind === 'capture') ocCaptureForRecord(record);
}
