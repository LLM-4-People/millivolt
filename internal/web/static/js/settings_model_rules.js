// Settings model-rules editor: the ordered merge rules that canonicalize a
// reported model spelling before it reaches a dimension, their RE2 lint, the
// traffic preview bench and the draft validators. The three mode literals are
// mirrored by config's rule-mode vocabulary. settings.js owns the accepted
// document and the shell; chrome.js's SETTINGS_MODULES list owns the load
// order.

// ---- model rules editor ----
// model_rules: an ordered rewrite pipeline (exact | pattern | lower steps)
// merging spelling variants of the same model in every grouped surface.
// One visual grammar with the other structured editors (.prov-urow rows,
// dashed add-row). Affordances, each researched from how mature tools do
// rule builders (regex101 / Cloudflare / Grafana / Stripe):
//   - live per-keystroke validation with consequence-phrased inline errors
//     (validateMrRow below runs on every input; Apply is blocked while any
//     row is red - nothing invalid can reach the POST).
//   - an RE2-compatibility lint (re2Lint): the server compiles patterns
//     with Go's RE2, whose syntax differs from JavaScript RegExp -
//     lookarounds/backreferences/\u pass `new RegExp` but fail the server.
//     The lint flags those before the save, and marks server-only-but-
//     Go-valid syntax as info rows the browser preview skips honestly.
//   - a preview bench: the typed spelling's final result, a per-rule step
//     trace, and a before→after table over the real model spellings from
//     the request log (the backtest pattern - synthetic examples lie).
//   - templates + restore-defaults (the shipped pipeline as one click).
//   - ◉/◌ per-rule parking: keep a rule without applying it (A/B the
//     pipeline; parked rows ride the wire as {disabled: true}, skipped by
//     ValidateModelRules' body checks and CompileModelRules).
const MODEL_RULE_MODES = [['exact', 'exact'], ['pattern', 'pattern'], ['lower', 'lower']];
const MR_MODE_TITLES = { exact: 'exact - merge one whole spelling', pattern: 'pattern - regex replace-all (RE2 syntax, $1 refs)', lower: 'lower - lowercase fold' };
// Mirrors config.ModelRulesMax (64) - the count line and add-gate use it.
const MODEL_RULES_MAX = 64;
// The template menu's labels for the shipped pipeline rules: presentation
// text, paired to settingsDoc.defaults.model_rules BY POSITION - the menu
// offers "the shipped pipeline" in its authored order, and the rule payloads
// themselves come from the server doc (mrTemplates below), never a copy
// here. A defaults list longer than the label table still offers every
// rule, labeled by position.
const MR_SHIPPED_LABELS = [
  'lowercase fold',
  'strip vendor/ prefix',
  'strip :tag suffix',
  'strip architecture/quant suffix (fp4, nvfp4, int8, q4_k_m…)',
  'unify . and - between digits',
];
// The custom starters: manual rows a shipped rule cannot express.
const MR_CUSTOM_TEMPLATES = [
  ['exact merge…', { mode: 'exact' }],
  ['custom pattern…', { mode: 'pattern' }],
];
// mrTemplates derives the editor's template menu at render time: the
// shipped entries ride in from the settings doc (defaults.model_rules - the
// same source the restore-defaults control reads), so their payloads cannot
// drift from the server. Deny by default: without a server doc only the
// custom starters are offered.
function mrTemplates() {
  const shipped = Array.isArray(settingsDoc?.defaults?.model_rules) ? settingsDoc.defaults.model_rules : [];
  return shipped.map((r, i) => [MR_SHIPPED_LABELS[i] || 'shipped rule ' + (i + 1), r])
    .concat(MR_CUSTOM_TEMPLATES);
}

function mrModeSelectHTML(sel) {
  return `<select class="mr-mode" title="${MR_MODE_TITLES[sel] || MR_MODE_TITLES.exact}" aria-label="rule mode">` +
    MODEL_RULE_MODES.map(([v, l]) => `<option value="${l}"${v === sel ? ' selected' : ''}>${l}</option>`).join('') +
    `</select>`;
}

// re2Lint classifies one pattern against the server's regex engine (Go
// regexp - RE2 syntax, which is not JavaScript RegExp): 'error' means the
// server would reject the pattern (blocks Apply), 'info' means it is valid
// for the server but the browser cannot run it faithfully (the preview
// skips the row and says so), 'warn' means valid everywhere but with
// different semantics (drift the preview would otherwise hide). Token
// tables verified against Go's regexp/syntax parser and the RE2 syntax
// reference; only constructs that are certain are flagged - anything
// uncertain is left to the server's own Validate (its error text is
// surfaced on the POST).
function re2Lint(p) {
  // RE2 caps counted repeats at 1000; JS allows astronomically more.
  const rep = /\{(\d+)(?:,(\d*))?\}/.exec(p);
  if (rep && (+rep[1] > 1000 || (rep[2] && +rep[2] > 1000)))
    return { level: 'error', msg: 'RE2 caps repeats at 1000 - "' + rep[0] + '" would fail to load on the server' };
  const SRV_ONLY = 'server-only syntax - the browser preview skips this rule; it still applies once saved';
  let cls = false;
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === '\\') {
      const e = p[i + 1] || '';
      if (!e) break;
      // JS RegExp backreference - RE2 has none. Multi-digit octal forms
      // (\17) are escapes in both, so only flag when the next char cannot
      // continue an octal escape.
      if ('123456789'.includes(e) && !(p[i + 2] && '01234567'.includes(p[i + 2])))
        return { level: 'error', msg: 'backreferences (\\' + e + ') are not supported by the server\'s regex engine (RE2) - rewrite the rule without them' };
      // \h \k \R \X \K \G \e \N \Z: identity escapes in JS, hard errors in RE2.
      if ('hkRXKGeNZ'.includes(e))
        return { level: 'error', msg: '\\' + e + ' is not supported by the server\'s regex engine (RE2)' };
      if (e === 'c' && p[i + 2] && /[A-Za-z]/.test(p[i + 2]))
        return { level: 'error', msg: '\\c control escapes are not supported by RE2 - use \\xNN' };
      if (e === 'b' && cls)
        return { level: 'error', msg: 'RE2 has no \\b inside a character class - use \\x08 for a backspace byte' };
      if (e === 'u')
        return { level: 'error', msg: 'RE2 does not support \\u escapes - write the character itself or use \\xNN' };
      if (e === 'p' || e === 'P')
        return { level: 'info', msg: 'unicode classes are ' + SRV_ONLY };
      if (e === 'A' || e === 'z')
        return { level: 'info', msg: 'text anchors \\A/\\z are ' + SRV_ONLY + ' (the browser would read them as literal letters)' };
      if (e === 'Q')
        return { level: 'info', msg: '\\Q…\\E literal quoting is ' + SRV_ONLY };
      if (e === 'x' && p[i + 2] === '{')
        return { level: 'info', msg: '\\x{…} hex escapes are ' + SRV_ONLY };
      i++; // consume the escape pair atomically
      continue;
    }
    if (c === '[') {
      if (p.startsWith('[[:', i))
        return { level: 'info', msg: 'POSIX classes like [[:alpha:]] are ' + SRV_ONLY };
      cls = true;
      continue;
    }
    if (c === ']') { cls = false; continue; }
    if (c === '(' && p[i + 1] === '?') {
      if (p.startsWith('(?=', i) || p.startsWith('(?!', i))
        return { level: 'error', msg: 'lookahead is not supported by the server\'s regex engine (RE2) - rewrite with character classes and anchors' };
      if (p.startsWith('(?<=', i) || p.startsWith('(?<!', i))
        return { level: 'error', msg: 'lookbehind is not supported by RE2 - rewrite with character classes and anchors' };
      if (p.startsWith('(?P<', i))
        return { level: 'info', msg: 'named groups (?P<…>) are ' + SRV_ONLY };
      const fl = /^\(\?([imsU-]+)\)/.exec(p.slice(i));
      if (fl)
        return { level: 'info', msg: 'inline flags like ' + fl[0] + ' are ' + SRV_ONLY };
    }
  }
  // Valid in both engines, but with different character sets: the browser
  // preview would show matches the server never makes.
  if (/\\s/.test(p))
    return { level: 'warn', msg: '\\s matches more characters in the browser than on the server (RE2: ASCII [ \\t\\n\\f\\r ] only) - prefer an explicit class' };
  return null;
}

// validateMrRow is the live per-row gate: renders the inline state (error
// text under the row, red border on the offending input) and returns the
// level. `seen` (a Set of earlier enabled rows' mode+from keys) powers
// the never-runs duplicate warning; pass null to skip it.
function validateMrRow(row, seen) {
  const mode = row.querySelector('.mr-mode').value;
  const fromIn = row.querySelector('.mr-from');
  const toIn = row.querySelector('.mr-to');
  const err = row.querySelector('.mr-err');
  const from = String((fromIn || {}).value || '');
  let level = 'ok', msg = '';
  if (row.dataset.mrOff === '1') {
    level = 'off';
  } else if (mode === 'lower') {
    level = 'ok';
  } else if (!from.trim()) {
    level = 'error';
    msg = (mode === 'exact' ? 'needs a from spelling' : 'needs a from pattern') + ' - fill it in, or remove the row';
  } else if (mode === 'exact') {
    if (from !== from.trim()) { level = 'error'; msg = 'the from spelling has leading/trailing spaces - the rule compares the whole string and would never fire'; }
  } else {
    const lint = re2Lint(from);
    let jsErr = null;
    try { new RegExp(from); } catch (e) { jsErr = e; }
    if (lint && lint.level === 'error') { level = 'error'; msg = lint.msg; }
    else if (jsErr && lint && lint.level === 'info') { level = 'info'; msg = lint.msg; }
    else if (jsErr) { level = 'error'; msg = String(jsErr.message || jsErr) + ' - the server would reject this pattern too'; }
    else if (lint) { level = lint.level; msg = lint.msg; }
  }
  if (level === 'ok' && seen) {
    const key = mode === 'lower' ? '\u0000lower' : mode + '\u0000' + from;
    if (seen.has(key)) { level = 'warn'; msg = 'an earlier rule already does this - this one never runs'; }
    else seen.add(key);
  }
  row.dataset.mrState = level;
  if (err) { err.textContent = msg; err.dataset.level = (level === 'error' || level === 'warn') ? level : ''; }
  if (fromIn) fromIn.classList.toggle('prov-bad', level === 'error');
  return level;
}

// validateModelRulesDraft walks every row in order and returns the first
// hard error (Apply blocks on it; the button flow focuses and flashes it).
function validateModelRulesDraft(wrap) {
  const seen = new Set();
  let firstBad = null;
  wrap.querySelectorAll('.mr-row').forEach(row => {
    if (validateMrRow(row, seen) === 'error' && !firstBad) firstBad = row;
  });
  return { ok: !firstBad, firstBad };
}

// The park/visibility button's title pair, shared by the row template and
// the toggle handler so the two spellings of the same states cannot drift.
const MR_VIS_TITLE_PARKED = 'un-park this rule';
const MR_VIS_TITLE_ACTIVE = 'park this rule - keep it without applying it';

function modelRuleRowHTML(r) {
  r = r || {};
  const mode = MODEL_RULE_MODES.some(([v]) => v === r.mode) ? r.mode : 'exact';
  const lower = mode === 'lower';
  const off = !!r.disabled;
  const dead = lower || off ? ' disabled' : '';
  return `<div class="prov-urow mr-row"${off ? ' data-mr-off="1"' : ''}>` +
    mrModeSelectHTML(mode).replace('<select ', `<select${off ? ' disabled' : ''} `) +
    `<input class="mr-from" value="${escapeHtml(r.from || '')}" placeholder="from - spelling (exact) or regex (pattern)"${dead} aria-label="from">` +
    `<span class="prov-arrow" aria-hidden="true">→</span>` +
    `<input class="mr-to" value="${escapeHtml(r.to || '')}" placeholder="to - empty removes the match"${dead} aria-label="to">` +
    `<span class="mr-move"><button type="button" class="mr-vis" data-mr-vis aria-pressed="${off}" title="${off ? MR_VIS_TITLE_PARKED : MR_VIS_TITLE_ACTIVE}">${off ? '◌' : '◉'}</button><button type="button" data-mr-up aria-label="move rule up" title="move up">▲</button><button type="button" data-mr-dn aria-label="move rule down" title="move down">▼</button></span>` +
    `<button type="button" class="prov-x" data-mr-rm aria-label="remove rule" title="remove">✕</button>` +
    `<div class="mr-err" aria-live="polite"></div>` +
    `</div>`;
}

function modelRulesEditorHTML(val) {
  const rules = Array.isArray(val) ? val : [];
  return `<div class="prov-sec mr-wrap">` +
    `<div class="mr-hint">rules apply top→bottom to each stored model name · exact merges a whole spelling · pattern is a regex replace-all (RE2, $1 refs) · lower folds case · ◉ parks a rule</div>` +
    `<div class="mr-tools">` +
    `<select class="mr-tpl" aria-label="add a rule from a template"><option value="">add from template…</option>${mrTemplates().map((t, i) => `<option value="${i}">${escapeHtml(t[0])}</option>`).join('')}</select>` +
    `<button type="button" class="mr-restore" data-mr-restore title="replace the draft with the shipped five-rule pipeline">↺ default pipeline</button>` +
    `<span class="mr-count muted"></span>` +
    `</div>` +
    `<div class="prov-urow mr-preview">` +
    `<span class="mr-what" title="Approximate exact/lower preview only; patterns execute on the server after saving.">preview*</span>` +
    `<input class="mr-test" placeholder="type any spelling - e.g. moonshotai/kimi-k3:nube" aria-label="test spelling">` +
    `<span class="prov-arrow" aria-hidden="true">→</span>` +
    `<span class="mr-out muted">…</span>` +
    `<span></span><span></span>` +
    `</div>` +
    `<div class="mr-trace" hidden></div>` +
    `<div class="mr-examples"></div>` +
    `<div class="mr-rows">${rules.map(modelRuleRowHTML).join('')}</div>` +
    `<div class="prov-add mr-add">` +
    mrModeSelectHTML('exact') +
    `<input class="mr-new-from" placeholder="from… (↵ to add)" aria-label="new rule from">` +
    `<span class="prov-arrow" aria-hidden="true">→</span>` +
    `<input class="mr-new-to" placeholder="to…" aria-label="new rule to">` +
    `<button type="button" class="btn prov-addbtn" data-mr-add aria-label="add rule">+</button>` +
    `</div>` +
    `</div>`;
}

// collectModelRules reads the editor's rows into the wire shape. Parked
// rows ride along as {disabled: true} (with their from/to kept, so
// un-parking restores them); incomplete enabled rows are dropped here for
// preview robustness - but the save gate (validateModelRulesDraft) blocks
// Apply on them first, so a dropped row can never silently vanish from a
// saved config. The server re-validates everything (mode vocabulary,
// pattern compilability, cap) - deny by default on both sides.
function collectModelRules(wrap) {
  const out = [];
  wrap.querySelectorAll('.mr-row').forEach(row => {
    const mode = row.querySelector('.mr-mode').value;
    const off = row.dataset.mrOff === '1';
    const from = String((row.querySelector('.mr-from') || {}).value || '');
    const to = String((row.querySelector('.mr-to') || {}).value || '');
    if (!from.trim() && !off && mode !== 'lower') return;
    const r = { mode };
    if (from || mode !== 'lower') r.from = from;
    if (to || mode !== 'lower') r.to = to;
    if (off) r.disabled = true;
    out.push(r);
  });
  return out;
}

// mrTrafficExamples gathers the top raw model spellings from the request
// log (live ring data - the real backtest the preview demonstrates on).
// Keep one last result keyed by the live data object, its revision and the
// records array identity: preview keystrokes must not rescan the ring, while
// a live revision bump or a manual fixture replacement (new array without a
// bump) still invalidates it. The cache is bounded to that single result.
let _mrTrafficCache = null;
function mrTrafficExamples() {
  if (typeof lastData !== 'object' || !lastData || !Array.isArray(lastData.records)) return [];
  const rev = lastData._rev || 0;
  const cached = _mrTrafficCache;
  if (cached && cached.data === lastData && cached.rev === rev && cached.records === lastData.records) {
    return cached.list;
  }
  const counts = new Map();
  for (const r of lastData.records) {
    const m = String((r && r.model) || '').trim();
    if (m) counts.set(m, (counts.get(m) || 0) + 1);
  }
  const list = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
  _mrTrafficCache = { data: lastData, rev, records: lastData.records, list };
  return list;
}

// mrDraftSteps builds the preview's compiled pipeline from the current
// editor rows: parked rows, rows the browser cannot run (info state), and
// rows that could never be saved (error state - the preview must not
// demonstrate behavior no valid config can have) are skipped - the preview
// is honestly partial when they exist.
function mrDraftSteps(wrap) {
  const steps = [];
  wrap.querySelectorAll('.mr-row').forEach(row => {
    const st = row.dataset.mrState;
    if (row.dataset.mrOff === '1' || st === 'info' || st === 'error') return;
    const mode = row.querySelector('.mr-mode').value;
    if (mode === 'lower') { steps.push({ label: 'lower', run: s => s.toLowerCase() }); return; }
    const from = String((row.querySelector('.mr-from') || {}).value || '');
    const to = String((row.querySelector('.mr-to') || {}).value || '');
    if (!from.trim()) return;
    if (mode === 'exact') { const f = from, t = to; steps.push({ label: f, run: s => s === f ? t : s }); return; }
    // Never execute RE2 patterns in a backtracking JavaScript engine.
    // Draft pattern outcomes are server-only; saved names use observer metadata.
  });
  return steps;
}

// mrRunSteps folds one spelling through the draft steps with the same
// empty-result fallback for the explicitly partial exact/lower preview.
function mrRunSteps(steps, s) {
  let cur = s;
  for (const st of steps) {
    const nxt = String(st.run(cur));
    if (!nxt) return { out: s, stripped: true };
    cur = nxt;
  }
  return { out: cur, stripped: false };
}

// mrSyncCount keeps the "N / 64 rules" line honest and gates the add
// controls at the cap (mirrors config.ModelRulesMax on the server).
function mrSyncCount(wrap) {
  const n = wrap.querySelectorAll('.mr-row').length;
  const count = wrap.querySelector('.mr-count');
  if (count) count.textContent = n + ' / ' + MODEL_RULES_MAX + ' rules';
  const cap = n >= MODEL_RULES_MAX;
  const add = wrap.querySelector('[data-mr-add]');
  if (add) add.disabled = cap;
  const tpl = wrap.querySelector('.mr-tpl');
  if (tpl) tpl.disabled = cap;
}

// mrPreview is the live bench: the typed spelling's final result, a
// per-rule step trace showing what each rule did, and the before→after
// table over the request log's real spellings.
function mrPreview(wrap) {
  const test = wrap.querySelector('.mr-test');
  const out = wrap.querySelector('.mr-out');
  if (!test || !out) return;
  const steps = mrDraftSteps(wrap);
  const s = String(test.value || '').trim();
  out.classList.toggle('muted', !s);
  if (!s) {
    out.textContent = '…';
    const trace = wrap.querySelector('.mr-trace');
    if (trace) trace.hidden = true;
  } else {
    const r = mrRunSteps(steps, s);
    out.textContent = r.out;
    const trace = wrap.querySelector('.mr-trace');
    if (trace) {
      let cur = s;
      const parts = [`<span class="mr-step-in">${escapeHtml(s)}</span>`];
      for (const st of steps) {
        const nxt = String(st.run(cur));
        if (!nxt) { parts.push(`<em class="mr-step-rule">stripped everything - original kept</em>`); cur = s; break; }
        if (nxt !== cur) {
          const lbl = st.label.length > 20 ? st.label.slice(0, 19) + '…' : st.label;
          parts.push(`<em class="mr-step-rule">${escapeHtml(lbl)}</em><span class="mr-step">${escapeHtml(nxt)}</span>`);
        }
        cur = nxt;
      }
      trace.innerHTML = parts.join('');
      trace.hidden = false;
    }
  }
  // real-traffic examples
  const exBox = wrap.querySelector('.mr-examples');
  if (exBox) {
    const exs = mrTrafficExamples();
    if (!exs.length) {
      exBox.innerHTML = `<div class="mr-ex-hd muted">no model names in the request log yet - type a test spelling above</div>`;
    } else {
      let changed = 0;
      const rows = exs.map(([m, n]) => {
        const r = mrRunSteps(steps, m);
        if (r.out !== m) changed++;
        return `<div class="mr-ex${r.out !== m ? ' ch' : ''}"><span class="mr-ex-in">${escapeHtml(m)}</span><span class="prov-arrow" aria-hidden="true">→</span><span class="mr-ex-out">${escapeHtml(r.out)}</span><span class="mr-ex-n muted">×${n}</span></div>`;
      }).join('');
      const partial = wrap.querySelector('.mr-mode option[value="pattern"]:checked') ? ' · partial - pattern rules execute on the server after saving' : '';
      exBox.innerHTML = `<div class="mr-ex-hd muted">your traffic - top spellings · ${changed}/${exs.length} change${partial}</div>` + rows;
    }
  }
  mrSyncCount(wrap);
}

// mrDraftChanged is the one draft-changed epilogue: every edit that
// mutates the rules draft (add, remove, reorder, park/unpark, restore)
// marks the sheet dirty, revalidates the whole draft and refreshes the
// preview bench. Per-keystroke typing and the settings open/refetch paint
// call the validate+preview pair directly - no draft mutation happens
// there, so they must not mark the sheet.
function mrDraftChanged(wrap) {
  markSettingsDirty(settingsKeyFor(wrap));
  validateModelRulesDraft(wrap);
  mrPreview(wrap);
}

// mrValidateInputs pre-checks the add-row inputs (deny by default: an
// invalid pattern or empty from flashes instead of adding a row that could
// never load). Server-only syntax (info level) is accepted - it saves and
// applies fine; the browser preview just skips it.
function mrValidateInputs(mode, fromIn, toIn) {
  const from = String((fromIn || {}).value || '');
  const to = String((toIn || {}).value || '');
  if (mode === 'lower') return { mode: 'lower' };
  if (!from.trim()) { flashBadInput(fromIn); return null; }
  if (mode === 'exact') {
    if (from !== from.trim()) { flashBadInput(fromIn); return null; }
    return { mode, from, to };
  }
  // Same gates the server applies at load - pre-check here so the row
  // never enters a save the POST would reject.
  const lint = re2Lint(from);
  if (lint && lint.level === 'error') { flashBadInput(fromIn); return null; }
  if (lint && lint.level === 'info') return { mode, from, to };
  try { new RegExp(from); } catch { flashBadInput(fromIn); return null; }
  return { mode, from, to };
}

// addModelRuleRow appends one rule from the add-row inputs.
function addModelRuleRow(addRow) {
  const wrap = addRow.closest('.mr-wrap');
  const mode = addRow.querySelector('.mr-mode').value;
  const rule = mrValidateInputs(mode, addRow.querySelector('.mr-new-from'), addRow.querySelector('.mr-new-to'));
  if (!rule) return;
  wrap.querySelector('.mr-rows').insertAdjacentHTML('beforeend', modelRuleRowHTML(rule));
  addRow.querySelector('.mr-new-from').value = '';
  addRow.querySelector('.mr-new-to').value = '';
  mrDraftChanged(wrap);
}

// mrApplyTemplate appends one pre-filled rule from the templates select -
// the common cases are one click (Cloudflare-style rule templates). The
// payloads come from mrTemplates (the server doc for shipped rules).
function mrApplyTemplate(sel) {
  const idx = parseInt(sel.value, 10);
  sel.value = '';
  const templates = mrTemplates();
  if (!Number.isInteger(idx) || !templates[idx]) return;
  const wrap = sel.closest('.mr-wrap');
  if (wrap.querySelectorAll('.mr-row').length >= MODEL_RULES_MAX) return;
  const t = templates[idx][1];
  wrap.querySelector('.mr-rows').insertAdjacentHTML('beforeend', modelRuleRowHTML(t));
  const rows = wrap.querySelectorAll('.mr-row');
  const row = rows[rows.length - 1];
  validateMrRow(row, null);
  markSettingsDirty(settingsKeyFor(sel));
  mrPreview(wrap);
  const from = row.querySelector('.mr-from');
  if (from && !from.value) from.focus();
}

// mrRestoreDefaults replaces the draft with the shipped pipeline exactly
// as the server reports it (the config defaults doc - no client-side
// mirror to drift). Nothing is committed until Apply; with no server
// defaults in reach it refuses instead of emptying the draft.
function mrRestoreDefaults(wrap) {
  const rules = settingsDoc?.defaults?.model_rules;
  if (!Array.isArray(rules)) return;
  wrap.querySelector('.mr-rows').innerHTML = rules.map(modelRuleRowHTML).join('');
  mrDraftChanged(wrap);
}
