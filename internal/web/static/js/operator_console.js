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
// OC_DAYS is the one weekday table: the letter the chip paints and the name
// it announces. Index matches the server's day numbering (0 = Sunday). The
// letter alone is not a name - "S" is Sunday and Saturday, "T" is Tuesday and
// Thursday - so the chip's accessible name comes from `name` while the visible
// label stays one character wide.
const OC_DAYS = [
  { letter: 'S', name: 'Sunday' },
  { letter: 'M', name: 'Monday' },
  { letter: 'T', name: 'Tuesday' },
  { letter: 'W', name: 'Wednesday' },
  { letter: 'T', name: 'Thursday' },
  { letter: 'F', name: 'Friday' },
  { letter: 'S', name: 'Saturday' },
];
const OC_DAY_LABELS = OC_DAYS.map(day => day.letter);

// ocNewSchedule is the one schedule a limit starts with when the operator
// opens the schedule disclosure: no days chosen yet, 09:00 to 17:00 in the
// server's local clock. The composer's preview fallback and the disclosure
// handler both read it, so the form cannot show one window and open another.
function ocNewSchedule() {
  return { days: [], start: 540, end: 1020 };
}

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

// ocPauseDefaultCap is the one client reader of the server's default hold
// queue cap. The number belongs to config.MaxQueueSize and the pause snapshot
// publishes it as default_max_queued, so the panel must never carry a second
// copy: a hardcoded fallback would silently disagree with a configured
// max_queue_size, and it would apply exactly when the pause document has not
// arrived yet. Zero means the server reports no default.
function ocPauseDefaultCap() {
  const n = typeof pauseState === 'object' && pauseState ? Number(pauseState.default_max_queued) : NaN;
  return isCount(n) ? n : 0;
}

// ---------- long rule lists ----------
// A rule list that outgrows the 560px panel is only usable if it can be
// narrowed: fourteen limits fill the pane and finding one becomes a scroll
// with no way to search. The filter appears only once a list is long enough to
// need it, so a console with two rules never spends a row on an empty box.
const OC_LIST_FILTER_MIN = 6;

// A picker list is a 190px scroller: about seven rows are ever visible. The
// row list was unbounded, so an export conversation picker over a record ring
// at its configured ceiling emitted 10,001 rows - 30,083 nodes and 2.6MB of
// HTML rebuilt on every keystroke, measured at 248-295ms - and the hidden
// panel retained them after close. One cap bounds both pickers, and the
// remainder is named so a truncated list never reads as a complete one.
const OC_PICK_ROWS_MAX = 100;

// ocPickRows renders a picker's value rows with the shared cap. `role` is
// menuitemcheckbox for a multi-select dimension and menuitemradio for the
// export filter's single choice; `tabStop` says whether row 0 owns the menu's
// one tab stop, which the export picker gives to its "any" row instead.
// Both pickers had their own copy of this row markup and had already drifted
// on the empty note, so the shape lives here once.
function ocPickRowsHTML(values, opts) {
  const role = opts.role;
  const shown = values.slice(0, OC_PICK_ROWS_MAX);
  const rows = shown.map((value, index) => {
    const on = !!opts.isOn(value);
    const tab = opts.tabStop === false ? '-1' : (index === 0 ? '0' : '-1');
    return `<button type="button" class="oc-pick-row" role="${role}" aria-checked="${on}" tabindex="${tab}" ${opts.attrs}="${escapeHtml(opts.dim)}" data-oc-value="${escapeHtml(value)}">` +
      `<span class="oc-pick-check" aria-hidden="true">${on ? '✓' : ''}</span>` +
      `<span class="oc-pick-label">${escapeHtml(opts.label(value))}</span></button>`;
  }).join('');
  // The filter input is the way to reach the rest, so the note names it
  // only while the list is actually truncated by the cap.
  const more = values.length > shown.length
    ? `<p class="oc-pick-note">${fmt(values.length - shown.length)} more match. Type to narrow the list.</p>`
    : '';
  return rows + more;
}
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

// ocRuleRowHTML assembles one stored-rule row. Limits, holds and captures each
// wrote this eight-tag skeleton out by hand, so the row key, the edit target
// and the side column had to be changed in three files to stay the same row.
// What differs between the kinds is content: the scope reading, the budget
// line, the live column and the action verbs. Those stay with the section that
// owns their words; the structure lives here once.
//
// The words are escaped here because every part is a server or operator
// string. `actions` is trusted markup: each section builds its own verbs and
// escapes their own values, exactly as before.
//
// It returns the row with the text it renders. The list filter searches that
// text, so deriving it from the same strings the row paints is what makes
// "anything on screen can be found" true by construction. Each section used to
// re-assemble what it believed the row said, and the copies had already
// drifted: a limit's schedule was on screen but unsearchable, and a hold's
// "queue cap" wording was replaced by the bare number. `title` is included
// because it carries the source ("set by header"), which is operator-visible
// on hover and nowhere else.
function ocRuleRowHTML(rule) {
  const id = escapeHtml(rule.id || '');
  const html = `<div class="oc-rule" data-kind="${escapeHtml(rule.kind)}" data-oc-rule="${id}"${ocRuleEditing(rule.kind, rule.id || '')}${rule.attrs || ''}>` +
    `<button type="button" class="oc-rule-main" data-oc-edit="${escapeHtml(rule.kind)}" data-oc-id="${id}" data-oc-key="rule-${id}" title="${escapeHtml(rule.title)}">` +
    `<span class="oc-rule-scope">${escapeHtml(rule.scope)}</span>` +
    `<span class="oc-rule-budget">${escapeHtml(rule.budget)}</span></button>` +
    `<div class="oc-rule-side"><span class="oc-rule-live">${escapeHtml(rule.live)}</span>` +
    `<div class="oc-rule-actions">${rule.actions}</div></div>` +
    `</div>`;
  const text = [rule.kind, rule.scope, rule.budget, rule.live, rule.title].join(' ');
  return { html, text };
}

// OC_COMPOSERS names each composer's preview and commit control once. The
// markup emitted below and the live preview that writes into it both read this
// table, so an id cannot be spelled for one and forgotten for the other - the
// previous code named each id twice and only a rename in both places kept the
// proof of what will be enforced attached to its button.
const OC_COMPOSERS = {
  limit: { preview: 'oc-lim-preview', key: 'lim-preview', apply: 'oc-lim-apply', state: 'throttle' },
  pause: { preview: 'oc-pause-preview', key: 'pause-preview', apply: 'oc-pause-apply', state: 'pause' },
  capture: { preview: 'oc-capture-preview', key: 'capture-preview', apply: 'oc-capture-apply', state: 'debug' },
};

// OC_CHOICES is the one table of every closed-vocabulary control in the
// console: the name it renders and the field it writes. The markup emits the
// name from here and ocHandleChoice resolves it here, so a control cannot be
// renamed on one side and silently stop writing. Before this, the same twelve
// names were spelled once where each control rendered and again in a chain of
// `if (name === ...)` comparisons, so a rename in one half left a control that
// toggled visually and changed nothing, with no error anywhere.
//
// `kind` is the section that owns the control, so a choice arriving from a
// stale pane cannot write into another section's draft. `recount` marks the
// controls whose write invalidates a derived read - every export filter
// changes the predicate its receipt quotes - so the count is refreshed before
// the repaint rather than after it.
const OC_CHOICES = {
  // The drain scope lives in the overview, which is not a composer kind.
  drainScope: { name: 'drain-scope', kind: 'overview' },
  requestWindow: { name: 'request-window', kind: 'limit', set: (d, v) => { d.requestWindow = v; } },
  tokenWindow: { name: 'token-window', kind: 'limit', set: (d, v) => { d.tokenWindow = v; } },
  ramp: { name: 'ramp', kind: 'limit', set: (d, v) => { d.ramp = v; } },
  pauseMode: {
    name: 'pause-mode', kind: 'pause',
    // Switching mode discards the open picker: the dimensions it offered
    // belong to the previous mode.
    set: (d, v) => { d.mode = v; d.picker = null; },
  },
  pauseDuration: { name: 'pause-duration', kind: 'pause', set: (d, v) => { d.duration = v; } },
  captureDuration: { name: 'capture-duration', kind: 'capture', set: (d, v) => { d.duration = v; } },
  exportMode: {
    name: 'export-mode', kind: 'export', recount: true,
    // A mode switch also invalidates the count outright: the predicate the
    // receipt quoted belonged to the previous mode, so keeping it would state
    // a total beside a download that acts on a different one.
    set: (d, v) => { d.mode = v; d.count = null; d.counting = false; d.error = ''; },
  },
  exportErrors: { name: 'export-errors', kind: 'export', recount: true, set: (d, v) => { d.errors = v === 'only'; } },
  exportDebug: { name: 'export-debug', kind: 'export', recount: true, set: (d, v) => { d.debug = v === 'only'; } },
  exportAfter: { name: 'export-after', kind: 'export', recount: true, set: (d, v) => { d.after = v; } },
  exportAge: { name: 'export-age', kind: 'export', recount: true, set: (d, v) => { d.age = v; } },
};

// OC_CHOICE_BY_NAME resolves the rendered attribute back to its row, so the
// dispatcher never compares against a name it spells itself.
const OC_CHOICE_BY_NAME = Object.fromEntries(
  Object.values(OC_CHOICES).map(choice => [choice.name, choice]));

// ocComposeControls resolves the two nodes a composer's preview owns, or null
// when the composer is not mounted.
function ocComposeControls(kind) {
  const ids = OC_COMPOSERS[kind];
  if (!ids) return null;
  const preview = ocEl(ids.preview);
  const apply = ocEl(ids.apply);
  return preview && apply ? { preview, apply } : null;
}

// ocComposeTailHTML is the shared tail of every composer: the preview the
// commit button points at, the draft's rejection, and the commit row. The
// preview is referenced by aria-describedby because a disabled button leaves
// the tab order, and without that reference a screen-reader operator can never
// reach the reason the action is unavailable.
function ocComposeTailHTML(kind, opts) {
  const ids = OC_COMPOSERS[kind];
  const busy = !!operatorState[ids.state].busy;
  return `<p class="oc-preview" id="${ids.preview}" data-oc-key="${ids.key}" role="status"></p>` +
    (opts.error ? `<p class="oc-error" role="alert">${escapeHtml(opts.error)}</p>` : '') +
    `<div class="oc-compose-ft"><button type="button" class="btn" data-oc-cancel>cancel</button>` +
    `<button type="button" class="btn btn-accent" id="${ids.apply}" data-oc-apply="${kind}" aria-describedby="${ids.preview}" aria-busy="${busy ? 'true' : 'false'}">${opts.label}</button></div>`;
}

// ocComposing reports whether a section is showing its editor. The three
// sections spelled the same expression out separately (a draft exists AND is
// either an edit of a stored rule or a fresh one for this section); one owner
// keeps "is a form open here" from drifting between them.
function ocComposing(kind) {
  const draft = ocDrafts[kind];
  return !!draft && (!!draft.id || ocView.kind === kind);
}

// ocEmptyHTML is the one empty-state frame for the console: a bold lead that
// names what is absent, then the sentence that explains the consequence. Five
// surfaces wrote the wrapper by hand - three sections, the filter's no-match
// note and the overview - so the frame could drift between them. The words
// stay with the surface that owns them.
//
// The two arguments are trusted differently, on purpose. `title` is a plain
// phrase and is escaped here. `body` is markup the caller composes and may
// carry a real control - the filter's no-match note embeds its clear action -
// so it is inserted as written and every caller must escape the values it
// interpolates. A body passed as plain text needs no escaping because it has
// no markup to break.
function ocEmptyHTML(title, body) {
  return `<div class="oc-empty"><strong>${escapeHtml(title)}</strong> ${body}</div>`;
}

// ocSectionEmpty is the one rule for a section's empty state: it describes a
// policy with no rules, and an open draft is already the answer to "what does
// this section do now". Only the limits section honoured that, so pause and
// capture printed "No holds." / "No captures running." underneath a live form,
// where the pane's pinned action row cut across the sentence. Each section
// still owns its own words; the frame comes from ocEmptyHTML.
function ocSectionEmpty(kind, total, title, body) {
  if (total || ocComposing(kind)) return '';
  return ocEmptyHTML(title, body);
}

// OC_SECTION_OF_KIND maps a rule kind to the destination that owns it. The
// draft and document vocabulary is the singular kind; the rail, the filter
// store and the empty-state clear action speak the destination's name. Only
// limits differ, and deriving the name from the kind with a suffix would spell
// "limits" correctly but "captures" for a section actually called capture.
const OC_SECTION_OF_KIND = { limit: 'limits', pause: 'pause', capture: 'capture' };

// ocSectionHTML assembles the list shell every policy section shares: the
// explainer with its actions, the composer when one is open, the list filter,
// the visible rows (or the section's own empty state), and the note that names
// a partial list. Limits, pause and capture each wrote this sequence by hand,
// and limits placed its action outside the head-actions wrapper the other two
// used, so on a phone its button stretched while theirs stayed compact.
//
// The rows arrive already built and carrying the text they render, because the
// filter searches what the operator can read rather than a re-derived
// predicate. The words stay with the section that owns them.
function ocSectionHTML(sec) {
  const section = OC_SECTION_OF_KIND[sec.kind] || sec.kind;
  const shown = ocFilterRows(section, sec.entries);
  const rows = shown.map(entry => entry.html).join('');
  const actions = sec.actions ? `<div class="oc-head-actions">${sec.actions}</div>` : '';
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><p class="oc-sub">${escapeHtml(sec.sub)}</p>${actions}</div>` +
    (sec.composer || '') +
    ocListFilterHTML(section, sec.total) +
    (rows || ocSectionEmpty(sec.kind, sec.total, sec.empty)) +
    ocListEmptyNote(section, shown.length, sec.total) +
    `</div>`;
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
  const value = ocListFilter[section] || '';
  // A stored filter must keep its control even when the list has shrunk below
  // the threshold. Otherwise removing rules leaves the list silently narrowed
  // with no visible cause and no clear action: the field unmounted while its
  // value went on filtering.
  if (total < OC_LIST_FILTER_MIN && !value) return '';
  return `<div class="oc-list-filter">` +
    `<input type="search" data-oc-list-filter="${section}" value="${escapeHtml(value)}"` +
    ` placeholder="filter ${total} rules" aria-label="filter the ${section} rules"` +
    ` data-oc-key="list-filter-${section}">` +
    `</div>`;
}

// ocListEmptyNote states that a filter hid the section's rules, so an empty
// list never reads as an empty policy.
function ocListEmptyNote(section, shown, total) {
  // A partial result is as misleading as an empty one: five rows on screen
  // beside a badge of fifty reads as a five-rule policy. Any active filter
  // states what it hid and offers the same one-click clear.
  const filtering = !!(ocListFilter[section] || '');
  if (!total || (shown === total && !filtering)) return '';
  if (!shown) {
    return ocEmptyHTML('No rule matches that filter.',
      `${fmt(total)} ${agree(total, 'rule is', 'rules are')} in force; ` +
      `<button type="button" class="oc-link" data-oc-list-clear="${section}">clear the filter</button> to see them.`);
  }
  return `<div class="oc-list-note">` +
    `${fmt(shown)} of ${fmt(total)} ${agree(total, 'rule', 'rules')} shown. ` +
    `<button type="button" class="oc-link" data-oc-list-clear="${section}">clear the filter</button></div>`;
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

// ocLimitOffSchedule is the same fact read the other way. Both rows need the
// negative, and writing `policy.scheduled === false` at each row was a second
// reading of one server field: a server that started answering null would
// flip one surface and not the other. The positive test stays the primitive
// and this is its complement, so they can never disagree.
function ocLimitOffSchedule(policy) {
  return !ocLimitEnforcing(policy);
}

// The one word both the readout and every row use for a policy outside its
// window. It is not "scheduled": that asserts the opposite of the truth.
const OC_OFF_SCHEDULE = 'off schedule';

function ocRuleCounts() {
  const holds = typeof liveHolds === 'function' ? liveHolds().length : 0;
  const captures = typeof liveDebugSessions === 'function' ? liveDebugSessions().length : 0;
  const policies = (typeof throttleState !== 'undefined' && throttleState.throttles) || [];
  const limits = policies.filter(ocLimitEnforcing).length;
  // `parked` is the complement of `limits`, so every count here answers a
  // question a surface asks. A total and an installed count were computed
  // alongside them for a badge design that never shipped: a second way to say
  // policies.length, which the limits section already reads directly.
  return { holds, captures, limits, parked: policies.length - limits };
}

function ocRestartActive() {
  return typeof restartInProgress === 'function' && restartInProgress();
}

// ocGatedCount is the readout's reading of requests parked by the storm/quota
// gate. It is separate from an operator drain because the gate runs before
// admission: draining the queue cannot release these waiters, only the gate
// opening or a quota resume can.
function ocGatedCount() {
  return typeof parkedCounts === 'function' ? parkedCounts().gated : 0;
}

// ocReadoutSegments returns the readout's words in priority order: a restart
// outranks a policy because it explains why traffic is moving.
function ocReadoutSegments() {
  const segs = [];
  if (ocRestartActive()) {
    const rank = typeof restartServerState === 'object' && restartServerState
      ? restartServerState.rank : null;
    // rank must be a real integer. The bootstrap restart section carries only
    // the eligibility verdict, so rank is absent until someone fetches the
    // status document; `null >= 0` is true in JavaScript, so a plain >= test
    // printed "step 1" before the server had reported any step at all. rank -1
    // is the server's idle marker, not a step either.
    segs.push('restarting' + (Number.isInteger(rank) && rank >= 0 ? ' · step ' + (rank + 1) : ''));
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
  if (counts.parked > 0) segs.push(OC_OFF_SCHEDULE + ' ' + counts.parked);
  // Requests the storm/quota gate is parking. They are in neither the
  // admission queue nor the send gate, so without this segment a proxy
  // refusing every new request still read `steady`.
  const gated = ocGatedCount();
  if (gated > 0) segs.push(ocPopulationLead(gated, OC_POPULATIONS.gated));
  return segs;
}

// ocParking reports whether live traffic is being held rather than merely
// capped. A hold parks matching requests, and so does the storm/quota gate;
// both are the warn tone in the readout because "steady" or a routine accent
// would say traffic is moving when it is not. One predicate, so the readout's
// words and its tone cannot disagree about whether something is parked.
function ocParking() {
  return ocRuleCounts().holds > 0 || ocGatedCount() > 0;
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
      : ocParking() ? 'paused'
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

// ocControls lists the pane's focusable controls in document order. A rebuild
// must resolve the same list before and after, so the enumeration is the one
// definition of "reachable by keyboard inside the pane".
function ocControls(root) {
  return [...root.querySelectorAll('button, input, select, textarea, [tabindex]')]
    .filter(el => !el.disabled && el.tabIndex >= 0);
}

// ocControlSignature names a control by what it is rather than where it sits:
// its tag, its input type and the data-oc-* attributes the console already
// stamps on every action. Anything that distinguishes two controls is part of
// the name, so a rule's copy button is told apart from its remove button and
// from the copy button of another rule.
function ocControlSignature(el) {
  const attrs = [];
  for (const attr of el.attributes) {
    if (attr.name.startsWith('data-oc-') && attr.name !== 'data-oc-key') {
      attrs.push(attr.name + '=' + attr.value);
    }
  }
  attrs.sort();
  return [el.tagName.toLowerCase(), el.type || '', attrs.join(',')].join('|');
}

// ocFocusKey names the focused control across a full pane rebuild. Draft
// values live in ocDrafts, so only the caret and the element identity need to
// survive.
function ocFocusKey() {
  const active = document.activeElement;
  if (!active || !ocShell()?.contains(active)) return null;
  let start = null, end = null;
  try { start = active.selectionStart; end = active.selectionEnd; } catch (e) { /* not a text input */ }
  // A picker row sits inside the picker's own keyed slot, so the generic
  // keyed lookup below already restores focus into the picker that owns it.
  // Choosing a value therefore rebuilds the pane without dropping the keyboard
  // operator back to <body>.
  const keyed = active.closest('[data-oc-key]');
  if (keyed) return { key: keyed.dataset.ocKey, start, end };
  const pane = ocEl('oc-pane');
  if (!pane || !pane.contains(active)) {
    // The undo button lives in the ledger, which is not rebuilt, but it hides
    // itself after use. Any other surface inside the panel still needs a
    // landing place, so the pane is the fallback.
    return { key: '', pane: true };
  }
  // Unkeyed controls are named structurally. Without that, arming a budget
  // checkbox, opening a disclosure, picking a day or using a rule action threw
  // the operator to the top of the form on every rebuild, because an unkeyed
  // control had no identity to return to.
  const sig = ocControlSignature(active);
  const same = ocControls(pane).filter(el => ocControlSignature(el) === sig);
  return { key: '', pane: true, sig, index: same.indexOf(active), start, end };
}

function ocRestoreFocus(saved) {
  if (!saved) return;
  const shell = ocShell();
  if (!shell) return;
  let next = null;
  if (saved.key) {
    const slot = [...shell.querySelectorAll('[data-oc-key]')].find(el => el.dataset.ocKey === saved.key);
    next = ocFocusTarget(slot);
  } else if (saved.sig) {
    const pane = ocEl('oc-pane');
    const same = pane ? ocControls(pane).filter(el => ocControlSignature(el) === saved.sig) : [];
    next = same[saved.index] || null;
  }
  // Never leave focus on <body>: that is the page behind the open panel, and
  // the operator's next Tab would walk the dashboard underneath.
  if (!next) next = saved.pane ? ocEl('oc-pane') : null;
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
  // The composer is inserted ABOVE the rule list, so restoring the old
  // scrollTop is not enough: with 20+ rules it leaves the whole form off the
  // top of the pane and the clicked row 350px down the screen, which reads as
  // "clicking the row did nothing". The picker case keeps its own position
  // (the composer is already in view and the picker is what just opened).
  if (!ocPickerFocus && ocView.kind) ocRevealComposer(pane);
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

// ocRailBadgeCount is the one reading of "how many rules does this
// destination hold". Only three sections count rules; the rest return 0 and
// never paint a badge.
function ocRailBadgeCount(section, counts) {
  if (section === 'limits') return counts.limits;
  if (section === 'pause') return counts.holds;
  if (section === 'capture') return counts.captures;
  return 0;
}

// ocRailTab creates one destination. It runs only when the rail is first
// painted: afterwards the tab is patched in place.
function ocRailTab(section) {
  const tab = document.createElement('button');
  tab.type = 'button';
  tab.className = 'oc-rail-item';
  tab.setAttribute('role', 'tab');
  tab.id = 'oc-tab-' + section.id;
  tab.setAttribute('aria-controls', 'oc-pane');
  tab.dataset.ocSection = section.id;
  tab.dataset.ocKey = 'rail-' + section.id;
  const label = document.createElement('span');
  label.className = 'oc-rail-label';
  label.textContent = section.label;
  tab.append(label, document.createTextNode(' '));
  return tab;
}

// ocRenderRail paints the six destinations by patching them. It replaced the
// whole tablist with innerHTML on every render, so a poll for a section the
// operator was not looking at rebuilt the strip and dropped the focused tab to
// <body> - a keyboard operator lost the console entirely while reading it.
// Patching also keeps the accessible name stable: the badge is separated by a
// space and hidden from assistive technology, so the tab reads "limits" and
// its count is announced by the selected pane, not glued on as "limits8".
function ocRenderRail() {
  const rail = ocEl('oc-rail');
  if (!rail) return;
  ocSyncRailOrientation();
  const counts = ocRuleCounts();
  OC_SECTIONS.forEach((section, index) => {
    let tab = rail.children[index];
    if (!tab || tab.dataset.ocSection !== section.id) {
      tab = ocRailTab(section);
      rail.insertBefore(tab, rail.children[index] || null);
    }
    const active = section.id === ocSection;
    tab.setAttribute('aria-selected', active ? 'true' : 'false');
    tab.tabIndex = active ? 0 : -1;
    const count = ocRailBadgeCount(section.id, counts);
    let badge = tab.querySelector('.oc-rail-n');
    if (!count) {
      if (badge) badge.remove();
      return;
    }
    if (!badge) {
      badge = document.createElement('span');
      badge.className = 'oc-rail-n';
      badge.setAttribute('aria-hidden', 'true');
      tab.appendChild(badge);
    }
    const text = fmt(count);
    if (badge.textContent !== text) badge.textContent = text;
  });
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
// ocBudgetPhrase is the one human reading of a limit's caps. It is what the
// row, the overview row and the ledger print, and the composer preview reads
// the same clause list rather than spelling the numbers a second way: the
// preview used to say "1 requests per 1 minute" for a row that said
// "1 request / 1m", and it never went singular. A draft carries camelCase
// windows and a stored policy carries snake_case ones, so the reader
// normalizes both instead of each caller choosing a shape.
function ocBudgetPhrase(policy) {
  const num = value => {
    const n = Number(value);
    return Number.isFinite(n) ? n : 0;
  };
  const reqWindow = policy.request_window || policy.requestWindow || LIMIT_WINDOW_DEFAULT;
  const tokWindow = policy.token_window || policy.tokenWindow || LIMIT_WINDOW_DEFAULT;
  const conc = num(policy.concurrency);
  const req = num(policy.requests);
  const tok = num(policy.tokens);
  const bits = [];
  if (conc > 0) bits.push(ocPopulation(conc, OC_POPULATIONS.inFlight));
  if (req > 0) bits.push(fmt(req) + (req === 1 ? ' request / ' : ' requests / ') + reqWindow);
  if (tok > 0) bits.push(fmt(tok) + (tok === 1 ? ' token / ' : ' tokens / ') + tokWindow);
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
  return days + ' · ' + fmtClockOfDay(start) + '-' + fmtClockOfDay(end);
}

// ocUntilPhrase is the console's wording for a server deadline. The
// arithmetic belongs to untilLeft, which the footer and the hold matcher also
// read; only the expired word is the console's own, because a rule row has to
// say something where a footer can simply omit the bit.
function ocUntilPhrase(until) {
  return untilLeft(until, 'expired');
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
  for (const key of OC_SCOPE_KEYS) {
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
  return ocScopeDim(dim)?.one || dim;
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
  // A menu is one tab stop: arrows move the cursor, Tab leaves the list.
  const rows = ocPickRowsHTML(known, {
    role: 'menuitemcheckbox', dim, attrs: 'data-oc-pick-value',
    isOn: value => selected.has(value),
    label: value => ocScopeValueLabel(dim, value),
  });
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

// ocRevealComposer brings a freshly opened composer into view. It scrolls only
// when the composer's top is outside the pane, so a rebuild while the operator
// is editing never moves a form they are already looking at, and it keeps the
// row they clicked on screen by scrolling to the composer rather than to the
// row (which the composer would immediately cover).
function ocRevealComposer(pane) {
  if (!pane) return;
  const compose = pane.querySelector('.oc-compose');
  if (!compose) return;
  const paneTop = pane.getBoundingClientRect().top;
  const composeTop = compose.getBoundingClientRect().top;
  if (composeTop >= paneTop - 1) return;
  pane.scrollTop += composeTop - paneTop - 2;
}

// ---------- change ledger ----------
// ocRecordChange is the one change receipt. It writes a human phrase, keeps a
// bounded session history and stores the undo closure that restores the exact
// prior policy. A change the operator cannot see or reverse was the old
// menus' core failure.
//
// It is also the one place a change is announced. The footer ledger and the
// toast are both polite live regions, so recording a change and then raising
// the same phrase as a toast spoke it twice. The ledger lives inside the
// panel, which means it is the announcement channel while the console is
// open, and the toast carries the phrase when the panel is closed and the
// ledger cannot be heard. Callers pass the phrase once and do not toast it
// themselves.
function ocRecordChange(phrase, undo, tone) {
  ocError = '';
  // No timestamp: the ledger is a bounded LIFO of the newest changes, and
  // nothing renders or serialises when each one happened.
  ocChanges.push({ phrase, undo: typeof undo === 'function' ? undo : null, tone: tone || '' });
  if (ocChanges.length > OC_CHANGE_LIMIT) ocChanges.shift();
  ocRenderFoot();
  refreshOperatorReadout();
  const shell = ocShell();
  const audible = !!shell && ocMounted && menuIsOpen(shell);
  if (!audible && typeof toast === 'function') toast(phrase, tone);
}

async function ocUndoLast() {
  const last = ocChanges[ocChanges.length - 1];
  if (!last || typeof last.undo !== 'function' || ocUndoBusy) return;
  ocUndoBusy = true;
  ocRenderFoot();
  try {
    // The shared write owner answers false when the server refused the
    // restore. It never throws, because the banner already carries the
    // reason; treating that as success popped the entry and printed
    // "undid: ..." over the top of the refusal, so the operator lost both
    // the change and the retry.
    if (await last.undo() === false) {
      throw new Error('the server refused that change');
    }
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
function ocChipRow(choice, values, current, labelOf) {
  const name = choice.name;
  return `<div class="oc-chips" role="group" aria-label="${escapeHtml(name)}">` +
    values.map(value => {
      const pressed = String(current) === String(value) ? 'true' : 'false';
      return `<button type="button" class="oc-chip" aria-pressed="${pressed}" data-oc-choice="${escapeHtml(name)}" data-oc-value="${escapeHtml(String(value))}">${escapeHtml(labelOf(value))}</button>`;
    }).join('') + `</div>`;
}

// ocChoiceSelect is the same closed vocabulary for a long list in a narrow
// track. A ten-chip ladder cannot show ten labels in the width of one number
// field, so it either wraps into three rows or clips mid-word; a native select
// states the current window in the same 24px and hands long lists to the
// platform's own keyboard and touch pickers.
// ocChoiceSelect renders the one native select every choice control uses.
//
// A stored value that is not in the preset list must still be shown. The
// server accepts any window from 1s to 24h, so a policy created through the
// API, MCP or a header can carry 90s while the presets only offer 1s..24h. With
// an exact-match-only test no option is selected and the browser displays the
// FIRST option, so the composer said "1 second" for a rule the row above
// correctly described as 90s, and a save would have rewritten the window the
// operator never meant to change. An off-list value therefore gets its own
// selected option, labelled by the same label function.
function ocChoiceSelect(choice, values, current, labelOf, aria) {
  const name = choice.name;
  const list = values.map(String);
  const missing = current != null && current !== '' && !list.includes(String(current));
  const all = missing ? list.concat(String(current)) : list;
  return `<select class="oc-select" data-oc-choice="${escapeHtml(name)}" aria-label="${escapeHtml(aria || name)}">` +
    all.map(value => {
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
  return isCount(n) ? n : NaN;
}

// ocField is the one labelled input. A bare field carries its name as an
// aria-label instead of a visible line: the surrounding row already names the
// dimension, and a second caption above every number is pure noise.
// ocField is the one numeric field. Every caller is a whole-number budget, so
// the input type, the numeric inputmode and the arming placeholder are fixed
// here rather than offered as options no call site set. Wide and hint spans
// exist in the stylesheet for the markup that writes them directly.
function ocField(key, label, value, opts = {}) {
  const name = opts.aria || label;
  return `<label class="oc-field${opts.bare ? ' bare' : ''}" data-oc-key="${escapeHtml(key)}">` +
    (opts.bare ? '' : `<span class="oc-field-name">${escapeHtml(label)}</span>`) +
    `<input type="number" min="0" step="1" inputmode="numeric" data-oc-field="${escapeHtml(key)}" data-oc-key="field-${escapeHtml(key)}"` +
    (name ? ` aria-label="${escapeHtml(name)}"` : '') +
    ` value="${escapeHtml(String(value == null ? '' : value))}" placeholder="${escapeHtml(opts.placeholder || '')}" ${opts.disabled ? 'disabled' : ''}>` +
    `</label>`;
}

function ocTimeValue(minutes) {
  if (minutes == null) return '';
  return fmtClockOfDay(Math.max(0, Math.min(1439, Number(minutes) || 0)));
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

// ---------- delegated actions ----------
// OC_ACTION_HANDLERS is the one registry of console click actions: the
// data-oc-* attribute that names each action, in dispatch order, paired with
// the handler that runs it. The listener's selector is built from these keys,
// so an action cannot be added to the dispatch chain and left out of the
// selector, or the reverse. The console carried those same 33 names in two
// lists, and adding one meant editing both.
//
// Every delegated control carries exactly one action attribute, which the
// harness pins, so the first match is the action and no precedence is
// improvised. Handlers receive the clicked element and read the values they
// need from it, exactly as the branches they replace did.
const OC_ACTION_HANDLERS = [
  ['data-oc-section', target => {
    // An overview row names one rule, so it opens that rule rather than
    // dropping the operator at the top of the section's list to search for it.
    const section = target.dataset.ocSection;
    const rule = target.dataset.ocTarget || '';
    ocSelectSection(section);
    if (rule) {
      if (section === 'limits') ocOpenLimitComposer(rule);
      if (section === 'pause') ocOpenPauseComposer(rule);
      if (section === 'capture') ocOpenCaptureComposer(rule);
    }
  }],
  ['data-oc-list-clear', target => {
    ocListFilter[target.dataset.ocListClear] = '';
    ocRender();
  }],
  ['data-oc-error-dismiss', () => ocOperatorErrorClear()],
  ['data-oc-close', () => closeOperatorConsole(true)],
  ['data-oc-cancel', () => { ocClearComposer(); ocRender(); }],
  ['data-oc-new', target => {
    const kind = target.dataset.ocNew;
    if (kind === 'limit') ocOpenLimitComposer('');
    if (kind === 'pause') ocOpenPauseComposer('');
    if (kind === 'capture') ocOpenCaptureComposer('');
  }],
  ['data-oc-quick-hold', () => ocQuickHold()],
  // A copy reuses the source rule's scope and schedule with no budgets, so a
  // follow-up rule starts from what already worked instead of four pickers.
  ['data-oc-copy', target => {
    const kind = target.dataset.ocCopy;
    const id = target.dataset.ocId || '';
    if (kind === 'limit') ocOpenLimitComposer(id, true);
    if (kind === 'pause') ocOpenPauseComposer(id, true);
    if (kind === 'capture') ocOpenCaptureComposer(id, true);
  }],
  ['data-oc-edit', target => {
    const kind = target.dataset.ocEdit;
    const id = target.dataset.ocId || '';
    if (kind === 'limit') ocOpenLimitComposer(id);
    if (kind === 'pause') ocOpenPauseComposer(id);
    if (kind === 'capture') ocOpenCaptureComposer(id);
  }],
  ['data-oc-remove', target => {
    if (target.dataset.ocRemove === 'limit') ocRemoveLimit(target.dataset.ocId || '');
  }],
  ['data-oc-resume', target => {
    const id = target.getAttribute('data-oc-resume');
    if (id) ocResumeHold(id);
  }],
  ['data-oc-resume-all', () => ocResumeAllHolds()],
  ['data-oc-stop', target => {
    const id = target.getAttribute('data-oc-stop');
    if (id) ocStopSession(id);
  }],
  ['data-oc-stop-all', () => ocStopAllSessions()],
  ['data-oc-apply', target => {
    const kind = target.dataset.ocApply;
    if (kind === 'limit') ocApplyLimit();
    if (kind === 'pause') ocApplyPause();
    if (kind === 'capture') ocApplyCapture();
  }],
  ['data-oc-pick', target => {
    const draft = ocActiveDraft();
    if (!draft) return;
    ocTogglePicker(draft, target.dataset.ocPick);
  }],
  ['data-oc-scope-remove', target => {
    const draft = ocActiveDraft();
    if (!draft) return;
    ocScopeToggle(ocDraftScope(draft), target.dataset.ocScopeRemove, target.dataset.ocValue || '');
    draft.picker = null;
    ocRender();
  }],
  ['data-oc-pick-value', target => {
    const draft = ocActiveDraft();
    if (!draft) return;
    ocScopeToggle(ocDraftScope(draft), target.dataset.ocPickValue, target.dataset.ocValue || '');
    ocRender();
  }],
  ['data-oc-pick-add', target => {
    const draft = ocActiveDraft();
    const dim = target.dataset.ocPickAdd;
    const input = ocShell()?.querySelector(`[data-oc-pick-custom="${dim}"]`);
    const value = (input?.value || '').trim();
    if (draft && value) { ocScopeToggle(ocDraftScope(draft), dim, value); ocRender(); }
  }],
  ['data-oc-toggle', target => {
    const toggle = target.dataset.ocToggle;
    if (toggle === 'advanced') {
      if (ocDrafts.limit) { ocDrafts.limit.advanced = !ocDrafts.limit.advanced; ocRender(); }
      return;
    }
    if (toggle === 'schedule') {
      if (ocDrafts.limit) {
        ocDrafts.limit.schedule = ocDrafts.limit.schedule ? null : ocNewSchedule();
        if (!ocDrafts.limit.schedule && ocDrafts.limit.antiBurst) ocDrafts.limit.antiBurst = false;
        ocRender();
      }
    }
  }],
  ['data-oc-day', target => {
    const day = target.getAttribute('data-oc-day');
    if (day == null || !ocDrafts.limit?.schedule) return;
    const days = ocDrafts.limit.schedule.days;
    const n = Number(day);
    const at = days.indexOf(n);
    if (at >= 0) days.splice(at, 1); else days.push(n);
    ocRender();
  }],
  ['data-oc-choice', target => ocHandleChoice(target.dataset.ocChoice, target.dataset.ocValue || '')],
  ['data-oc-filter-pick', target => ocToggleFilterPick(target.dataset.ocFilterPick)],
  ['data-oc-filter-value', target => {
    const draft = ocDrafts.export;
    if (!draft) return;
    draft[target.dataset.ocFilterValue] = target.dataset.ocValue || '';
    draft.picker = null;
    ocScheduleExportCount();
    ocRender();
  }],
  ['data-oc-filter-clear', target => {
    const draft = ocDrafts.export;
    if (!draft) return;
    draft[target.dataset.ocFilterClear] = '';
    draft.picker = null;
    ocScheduleExportCount();
    ocRender();
  }],
  ['data-oc-filter-add', target => {
    const dim = target.dataset.ocFilterAdd;
    const input = ocShell()?.querySelector(`[data-oc-filter-custom="${dim}"]`);
    const value = (input?.value || '').trim();
    if (!value || !ocDrafts.export) return;
    ocDrafts.export[dim] = value;
    ocDrafts.export.picker = null;
    ocScheduleExportCount();
    ocRender();
  }],
  ['data-oc-download', () => ocDownloadExport()],
  ['data-oc-reset-export', () => { ocDrafts.export = ocBlankExportDraft(); ocRender(); }],
  ['data-oc-settings', target => {
    // A warning that names one setting carries the category, so the sheet
    // opens on the pane that holds it instead of its first pane.
    const category = target.dataset.ocSettings || '';
    closeOperatorConsole();
    if (category && typeof openSettingsAt === 'function') openSettingsAt(category);
    else toggleSettings();
  }],
  ['data-oc-drain', () => ocDrainQueue()],
  ['data-oc-quota-resume', target => {
    // The resume owner answers whether the gate was released, so a refusal is
    // named in the pane the operator is looking at rather than in the storm
    // dialog, which may not be open at all.
    const provider = target.dataset.ocQuotaResume;
    target.disabled = true;
    return Promise.resolve(resumeQuotaProvider(provider)).then(ok => {
      if (!ok) ocOperatorError(quotaResumeError || 'could not resume ' + provider);
      ocRender();
    });
  }],
  ['data-oc-restart', () => { if (typeof restartProxy === 'function') restartProxy(); }],
  ['data-oc-undo', () => ocUndoLast()],
];

// The listener selector and the dispatch table are the same list.
const OC_ACTION_SELECTOR = OC_ACTION_HANDLERS.map(([attr]) => '[' + attr + ']').join(',');

async function ocHandleAction(target) {
  const entry = OC_ACTION_HANDLERS.find(([attr]) => target.hasAttribute(attr));
  if (!entry) return;
  await entry[1](target);
}

// ocHandleChoice writes a closed-vocabulary pick into the section that owns
// the control. The section and the write both come from OC_CHOICES, so a
// choice the table does not name is a no-op rather than a silent write into
// whatever draft happens to be open.
function ocHandleChoice(name, value) {
  const choice = OC_CHOICE_BY_NAME[name];
  if (!choice) return;
  if (choice.kind === 'overview') {
    ocDrain.scope = value;
    ocDrain.value = '';
    ocRender();
    return;
  }
  if (ocCurrentKind() !== choice.kind) return;
  const draft = ocDrafts[choice.kind];
  if (!draft) return;
  choice.set(draft, value);
  // A control whose write invalidates a derived read refreshes it before the
  // repaint, so the receipt never quotes a count for the previous predicate.
  if (choice.recount) ocScheduleExportCount();
  ocRender();
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
    if (ocDrafts.pause) { ocDrafts.pause.cap = target.checked ? String(ocPauseDefaultCap()) : ''; ocRender(); }
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
  const target = event.target.closest?.(OC_ACTION_SELECTOR);
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
  return OC_SCOPE_DIMS.every(dim => {
    const values = scope[dim.wire] || [];
    return !values.length || values.includes(record[dim.record] || '');
  });
}

// ocScopeWeight ranks matches so a model rule outranks the global rule when
// both parked the same request. Summing non-empty dimensions is enough to
// make the choice stable without inventing a precedence the server has not
// promised.
function ocScopeWeight(scope) {
  return OC_SCOPE_DIMS.reduce((n, dim) => n + ((scope?.[dim.wire] || []).length ? 1 : 0), 0);
}

// ocHoldMatchesRecord mirrors scheduler.holdSnap.matches: AND across the
// dimensions the hold sets, with the client dimension carrying the New
// modifier. It reads the same five dimensions a limit can select on, so
// "which hold covers this request" answers with the rule the server applied
// rather than a narrower guess.
function ocHoldMatchesRecord(hold, record) {
  if (!hold || !record) return false;
  if (hold.all) return true;
  const scope = ocScopeFromPolicy({ scope: hold });
  const client = record.client || '';
  const known = hold.known_at_new || [];
  const named = scope.clients.length > 0;
  if (named) {
    if (!scope.clients.includes(client)) {
      // A named client wins over New; otherwise New may still admit a client
      // the server had not seen. A client it already knew never matches.
      if (!hold.new || known.includes(client)) return false;
    }
  } else if (hold.new && known.includes(client)) {
    return false;
  }
  for (const dim of OC_SCOPE_DIMS) {
    // The client dimension was decided above, where New participates.
    if (dim.key === 'clients') continue;
    const set = scope[dim.key];
    if (set.length && !set.includes(record[dim.record] || '')) return false;
  }
  // A hold with no named dimension at all is inactive; the server never
  // installs one, and a row for it would claim a park that does not happen.
  return named || hold.new || OC_SCOPE_DIMS.some(dim =>
    dim.key !== 'clients' && (scope[dim.key] || []).length > 0);
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
  // The same five dimensions a limit seeds from. A record knows its model,
  // key and conversation, so a hold started from the row can name them;
  // seeding only client and provider silently narrowed what the operator saw.
  draft.scope = ocScopeFromPolicy({ scope: ocRecordScope(record) });
  if (!ocScopeHas(draft.scope)) draft.mode = 'all';
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
  draft.scope = ocScopeFromPolicy({ scope: ocRecordScope(record) });
  ocDrafts.capture = draft;
  ocView = { kind: 'capture', id: '' };
  ocRender();
}

// ocRecordScope is the one projection from a request record to a scope
// document, using the names the console drafts and the server documents
// already share. Record-match seeds read it so a new dimension is offered by
// every prefill at once instead of being added at each call site.
function ocRecordScope(record) {
  const scope = {};
  for (const dim of OC_SCOPE_DIMS) {
    const value = record?.[dim.record];
    scope[dim.wire] = value ? [value] : [];
  }
  return scope;
}

// ocOpenOperatorForRecord is the one dispatch for the three record pills.
function ocOpenOperatorForRecord(kind, record) {
  if (!record) return;
  if (kind === 'limit') ocLimitForRecord(record);
  else if (kind === 'pause') ocPauseForRecord(record);
  else if (kind === 'capture') ocCaptureForRecord(record);
}
