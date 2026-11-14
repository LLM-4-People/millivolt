// Operator limits: the policy list, the one composer, its live preview, the
// direct restore path used by undo, and the removal confirmation. The shell
// (operator_console.js) owns the readout, rail, section dispatch, ledger and
// event delegation; this module owns every limit rule and the POST shape.
//
// Ownership: the server owns policy semantics (scope intersection, schedule,
// identity, anti-burst eligibility). This module never re-implements them; it
// builds the one strict document and reads the one snapshot document.

// ---------- limits ----------
// A budget is armed when its checkbox is on. Armed but empty is a real state:
// checking the box opens the field without writing a number the operator never
// chose, and Apply stays disabled until they type one.
function ocBlankBudgetArmed() {
  return { concurrency: false, requests: false, tokens: false };
}

function ocBudgetArmed(draft, key) {
  if (draft[key] !== '' && draft[key] != null) return true;
  return !!(draft.armed && draft.armed[key]);
}

function ocBlankLimitDraft() {
  return {
    id: '',
    scope: ocNewScope(),
    schedule: null,
    concurrency: '',
    requests: '',
    requestWindow: LIMIT_WINDOW_DEFAULT,
    tokens: '',
    tokenWindow: LIMIT_WINDOW_DEFAULT,
    antiBurst: false,
    ramp: '',
    advanced: false,
    armed: ocBlankBudgetArmed(),
    picker: null,
    error: '',
  };
}

function ocLimitDraftFromPolicy(policy) {
  return {
    id: policy.id,
    scope: ocScopeFromPolicy(policy),
    schedule: policy.schedule ? {
      days: Array.isArray(policy.schedule.days) ? [...policy.schedule.days] : [],
      start: policy.schedule.start_minute || 0,
      end: policy.schedule.end_minute || 0,
    } : null,
    concurrency: policy.concurrency ? String(policy.concurrency) : '',
    requests: policy.requests ? String(policy.requests) : '',
    requestWindow: policy.request_window || LIMIT_WINDOW_DEFAULT,
    tokens: policy.tokens ? String(policy.tokens) : '',
    tokenWindow: policy.token_window || LIMIT_WINDOW_DEFAULT,
    antiBurst: !!policy.anti_burst,
    ramp: policy.ramp_duration || '',
    advanced: !!(policy.anti_burst || policy.ramp_duration),
    armed: ocBlankBudgetArmed(),
    picker: null,
    error: '',
  };
}

// ocLimitDraftCopy opens a new limit that reuses a stored rule's scope and
// schedule with no budgets. Widening a cap to a second provider or model is
// the common follow-up to a rule that worked, and retyping four dimensions to
// do it was the most expensive path in the console.
function ocLimitDraftCopy(policy) {
  const draft = ocLimitDraftFromPolicy(policy);
  draft.id = '';
  draft.concurrency = '';
  draft.requests = '';
  draft.tokens = '';
  draft.antiBurst = false;
  draft.ramp = '';
  draft.advanced = false;
  return draft;
}

function ocLimitById(id) {
  return ((typeof throttleState !== 'undefined' && throttleState.throttles) || [])
    .find(t => t && t.id === id) || null;
}

// ocLimitBySelector finds the stored rule a create would actually edit. The
// server identifies a limit by its selector, so posting a new draft whose
// scope and schedule match an existing rule updates that rule in place. The
// console must know that before it writes: "copy" keeps the source selector,
// so a copy that only changed the budgets silently replaced the original.
function ocLimitBySelector(scope, schedule) {
  const key = ocLimitSelectorKey(scope, schedule);
  return ((typeof throttleState !== 'undefined' && throttleState.throttles) || [])
    .find(t => t && ocLimitSelectorKey(ocScopeFromPolicy(t), ocLimitScheduleKey(t)) === key) || null;
}

function ocOpenLimitComposer(id, copy) {
  if (copy) {
    const source = ocLimitById(id);
    if (!source) { ocRuleGone('limit'); return; }
    ocDrafts.limit = ocLimitDraftCopy(source);
    ocView = { kind: 'limit', id: '' };
    ocSection = 'limits';
    ocRender();
    return;
  }
  if (id) {
    const policy = ocLimitById(id);
    if (!policy) { ocRuleGone('limit'); return; }
    ocDrafts.limit = ocLimitDraftFromPolicy(policy);
    ocView = { kind: 'limit', id };
  } else {
    ocDrafts.limit = ocBlankLimitDraft();
    ocView = { kind: 'limit', id: '' };
  }
  ocSection = 'limits';
  ocRender();
}

// ocBurstAllowed is the one eligibility test for the anti-burst ramp: the
// server accepts it only on a single-provider policy with no schedule, so the
// composer both gates its control on this and refuses to send it. Two copies
// of this rule would let a widened scope post a body the server rejects.
function ocBurstAllowed(draft) {
  return draft.scope.providers.length === 1 &&
    draft.scope.models.length === 0 && draft.scope.clients.length === 0 &&
    draft.scope.keyHashes.length === 0 && draft.scope.conversations.length === 0 &&
    !draft.schedule;
}

// ocWindowSelect is the limit vocabulary in a native select: the list is long,
// the track is one number wide, and every caller used to re-derive the same
// value list. `withOff` prepends the empty ramp choice.
function ocWindowSelect(name, current, aria, withOff) {
  const values = (withOff ? [''] : []).concat(LIMIT_WINDOWS.map(([v]) => v));
  return ocChoiceSelect(name, values, current, v => v ? ocWindowLabel(v) : 'off', aria);
}

// ocBudgetPlaceholder is the hint an unarmed budget field shows. A disabled
// field cannot be clicked, so it must say how to enable itself rather than
// display a value the operator never entered.
function ocBudgetPlaceholder(draft, key, example) {
  return ocBudgetArmed(draft, key) ? example : 'tick to cap';
}

function ocLimitComposerHTML(draft) {
  const editing = !!draft.id;
  const conds = ocScopeConditionsHTML(draft, OC_SCOPE_KEYS);
  const global = !ocScopeHas(draft.scope);
  const burstAllowed = ocBurstAllowed(draft);
  const scheduleOn = !!draft.schedule;
  const schedule = draft.schedule || ocNewSchedule();
  return `<section class="oc-compose" data-kind="limit" aria-label="${editing ? 'edit limit' : 'new limit'}">` +
    `<div class="oc-compose-hd"><span>${editing ? 'editing limit' : 'new limit'}</span></div>` +
    `<div class="oc-group"><div class="oc-label">applies to</div>${conds}` +
    (global ? `<p class="oc-note">No condition selected, so this limit applies to every request that crosses the proxy.</p>` : '') +
    `</div>` +
    `<div class="oc-group"><div class="oc-label">budgets</div>` +
    // An unarmed budget field is disabled, so a click on it does nothing. It
    // used to carry a plausible example value in that state, which read as a
    // pre-filled default and made the dead click look like a broken field.
    // While disabled it now says what to do instead, at no height cost.
    `<div class="oc-budget"><label class="oc-check"><input type="checkbox" data-oc-budget="concurrency" ${ocBudgetArmed(draft, 'concurrency') ? 'checked' : ''}><span>concurrency</span></label>` +
    ocField('concurrency', '', draft.concurrency, { bare: true, aria: 'in flight', disabled: !ocBudgetArmed(draft, 'concurrency'), placeholder: ocBudgetPlaceholder(draft, 'concurrency', '4') }) + `</div>` +
    `<div class="oc-budget"><label class="oc-check"><input type="checkbox" data-oc-budget="requests" ${ocBudgetArmed(draft, 'requests') ? 'checked' : ''}><span>requests</span></label>` +
    ocField('requests', '', draft.requests, { bare: true, aria: 'requests per window', disabled: !ocBudgetArmed(draft, 'requests'), placeholder: ocBudgetPlaceholder(draft, 'requests', '60') }) +
    `<div class="oc-window">${ocWindowSelect('request-window', draft.requestWindow, 'request window')}</div></div>` +
    `<div class="oc-budget"><label class="oc-check"><input type="checkbox" data-oc-budget="tokens" ${ocBudgetArmed(draft, 'tokens') ? 'checked' : ''}><span>tokens</span></label>` +
    ocField('tokens', '', draft.tokens, { bare: true, aria: 'tokens per window', disabled: !ocBudgetArmed(draft, 'tokens'), placeholder: ocBudgetPlaceholder(draft, 'tokens', '100000') }) +
    `<div class="oc-window">${ocWindowSelect('token-window', draft.tokenWindow, 'token window')}</div></div>` +
    `</div>` +
    // Both optional modifiers sit on one row: they are peers, each closed by
    // default, and two stacked full-width bands spent 45px of the panel on two
    // words. The opened body still takes the full width below the pair.
    `<div class="oc-group"><div class="oc-disclosures">` +
    `<button type="button" class="oc-disclosure" data-oc-toggle="advanced" aria-expanded="${draft.advanced ? 'true' : 'false'}">` +
    `<span>burst control</span><span aria-hidden="true">${draft.advanced ? '▴' : '▾'}</span></button>` +
    `<button type="button" class="oc-disclosure" data-oc-toggle="schedule" aria-expanded="${scheduleOn ? 'true' : 'false'}">` +
    `<span>schedule</span><span class="oc-disclosure-state">${scheduleOn ? escapeHtml(ocSchedulePhrase(draft.schedule)) : 'always active'}</span>` +
    `<span aria-hidden="true">${scheduleOn ? '▴' : '▾'}</span></button>` +
    `</div>` +
    (draft.advanced ? `<div class="oc-advanced">` +
      `<label class="oc-check"><input type="checkbox" data-oc-field="antiBurst" ${draft.antiBurst ? 'checked' : ''} ${burstAllowed ? '' : 'disabled'}>` +
      `<span>ramp each new key from one to the concurrency target</span></label>` +
      `<div class="oc-window">${ocWindowSelect('ramp', draft.ramp, 'ramp duration', true)}</div>` +
      (burstAllowed ? '' : `<p class="oc-note">Burst control needs a single-provider limit without a schedule.</p>`) +
      `</div>` : '') +
    (scheduleOn ? `<div class="oc-advanced">` +
      `<div class="oc-days" role="group" aria-label="days">` + OC_DAYS.map((day, index) =>
        // The painted label is one letter to fit seven chips on a row; the
        // accessible name is the day, because "S" and "T" each stand for two
        // different days and a screen reader has no width constraint to honour.
        `<button type="button" class="oc-chip oc-chip-day" aria-label="${escapeHtml(day.name)}" aria-pressed="${schedule.days.includes(index) ? 'true' : 'false'}" data-oc-day="${index}">${day.letter}</button>`).join('') + `</div>` +
      `<div class="oc-times"><label class="oc-field"><span class="oc-field-name">from</span><input type="time" data-oc-time="start" value="${ocTimeValue(schedule.start)}"></label>` +
      `<label class="oc-field"><span class="oc-field-name">to</span><input type="time" data-oc-time="end" value="${ocTimeValue(schedule.end)}"></label>` +
      `<span class="oc-field-hint">server local time</span></div>` +
      `</div>` : '') +
    `</div>` +
    ocComposeTailHTML('limit', { error: draft.error, label: editing ? 'save limit' : 'apply limit' }) +
    `</section>`;
}

// ocLimitPreview is the live sentence above the commit button. It is the
// operator's proof of what will be enforced, written from the same scope and
// budget owners the rule rows use.
// OC_WHOLE_NUMBER_FAULT is the one sentence for a non-whole budget. The
// preview paints it and the apply guard reuses it, so the same condition
// cannot reach the operator in two different wordings.
const OC_WHOLE_NUMBER_FAULT = 'budgets must be whole numbers of 0 or more';

function ocLimitPreview() {
  const draft = ocDrafts.limit;
  if (!draft) return;
  const nodes = ocComposeControls('limit');
  if (!nodes) return;
  const { preview: el, apply } = nodes;
  const conc = ocNumber(draft.concurrency);
  const req = ocNumber(draft.requests);
  const tok = ocNumber(draft.tokens);
  const bad = [conc, req, tok].some(Number.isNaN);
  const any = conc > 0 || req > 0 || tok > 0;
  // A create whose selector already belongs to a stored rule is an edit to
  // that rule, because the server owns identity and identity is the selector.
  // The panel must say so rather than report "added" for a replacement.
  //
  // This is checked before the budgets, not after: a copy reuses the source
  // selector and clears the budgets, so the collision is knowable the moment
  // the composer opens. Gating it on a typed budget told the operator "set at
  // least one budget" first and only refused after they had typed one, which
  // is the one thing they could do that could not help.
  const collision = draft.id ? null : ocLimitBySelector(draft.scope, draft.schedule);
  let text;
  if (bad) {
    text = OC_WHOLE_NUMBER_FAULT;
  } else if (collision) {
    text = 'This selector already has a limit. Change the scope or schedule to add a second rule, or open the existing one to edit it.';
  } else if (!any) {
    text = 'set at least one budget';
  } else {
    // The preview names the same caps the row will print. It used to build a
    // second sentence with its own plural rule and its own window wording, so
    // an operator read "1 requests per 1 minute" while the row underneath said
    // "1 request / 1m" for the same draft.
    text = 'At most ' + ocBudgetPhrase({
      concurrency: conc,
      requests: req,
      requestWindow: draft.requestWindow,
      tokens: tok,
      tokenWindow: draft.tokenWindow,
    }) + ', for ' + ocScopePhrase(draft.scope) + '.';
    if (draft.schedule) text += ' Active ' + ocSchedulePhrase(draft.schedule) + '.';
    if (draft.antiBurst && draft.ramp) text += ' Each new key ramps over ' + ocWindowLabel(draft.ramp) + '.';
  }
  el.textContent = text;
  el.dataset.tone = bad || collision ? 'err' : '';
  apply.disabled = !!operatorState.throttle.busy || bad || !any || !!collision;
}

function ocRenderLimits() {
  const policies = ((typeof throttleState !== 'undefined' && throttleState.throttles) || []);
  if (!ocDrafts.limit) ocDrafts.limit = ocBlankLimitDraft();
  // Each row carries the text it renders, so the shared list filter searches
  // what the operator can actually read instead of re-deriving a predicate.
  const entries = policies.map(policy => {
    const html = ocLimitRuleHTML(policy);
    const scope = ocPolicyScopePhrase(policy);
    return { html, text: scope + ' ' + ocBudgetPhrase(policy) + ' ' + (policy.source || '') };
  });
  const composing = ocComposing('limit');
  const editing = !!ocDrafts.limit?.id;
  // An empty list proves no limit rule exists. It does not prove admission:
  // the group queue still refuses with 429 when full (ErrQueueFull), and the
  // error-storm gate refuses its own backlog (ErrStormQueueFull). Saying
  // "every request is admitted" promised something the scheduler does not.
  const empty = ocSectionEmpty('limit', policies.length, 'No limits.',
    "Nothing here caps a request. The admission queue and each provider's own rate limits still apply.");
  // The composer footer is the one action row while a draft is open. A head
  // "start over" beside it was a second, silent discard of the same draft, and
  // it took the accent the commit needs. It stays only while editing a stored
  // rule, where leaving the edit to begin a fresh one is a real operation the
  // composer cannot express.
  const headAction = editing
    ? `<button type="button" class="btn" data-oc-new="limit">start over</button>`
    : composing ? ''
      : `<button type="button" class="btn btn-accent" data-oc-new="limit">new limit</button>`;
  return ocSectionHTML({
    kind: 'limit',
    sub: 'Cap concurrency, requests or tokens for any provider, model, client, key, conversation, or the whole proxy.',
    actions: headAction,
    composer: composing ? ocLimitComposerHTML(ocDrafts.limit) : '',
    entries, total: policies.length, empty,
  });
}

function ocLimitRuleHTML(policy) {
  const scope = ocPolicyScopePhrase(policy);
  const live = [];
  if (policy.in_flight) live.push(policy.in_flight + ' in flight');
  if (policy.queued) live.push(policy.queued + ' queued');
  if (policy.requests && policy.requests_capacity) {
    live.push(Math.max(0, Math.round(policy.requests_remaining)) + ' of ' + Math.round(policy.requests_capacity) + ' requests left');
  }
  if (policy.tokens && policy.tokens_capacity) {
    live.push(fmt(Math.max(0, Math.round(policy.tokens_remaining))) + ' of ' + fmt(Math.round(policy.tokens_capacity)) + ' tokens left');
  }
  const source = policy.source === 'header'
    ? 'set by header' + (policy.updated_by ? ' · ' + policy.updated_by : '')
    : 'set in the console';
  const schedule = policy.schedule ? ' · ' + ocSchedulePhrase(policy.schedule) : '';
  // The server owns the verdict. A policy outside its window enforces
  // nothing, and the live column used to say "idle" for it, which reads as
  // "no traffic right now" rather than "this cap is not applied at all". The
  // row now says so in the same words the readout uses.
  const offSchedule = ocLimitOffSchedule(policy);
  return ocRuleRowHTML({
    kind: 'limit', id: policy.id, scope, title: scope + ' · ' + source,
    budget: ocBudgetPhrase(policy) + schedule,
    live: offSchedule ? OC_OFF_SCHEDULE : (live.join(' · ') || 'idle'),
    attrs: offSchedule ? ' data-oc-off-schedule="1"' : '',
    // The whole row is the edit target, so a separate edit action would
    // duplicate it and take width the scope needs.
    actions: `<button type="button" class="oc-icon-btn" data-oc-copy="limit" data-oc-id="${escapeHtml(policy.id)}" aria-label="copy this limit's scope into a new limit" title="copy this scope into a new limit">copy</button>` +
      `<button type="button" class="oc-icon-btn danger" data-oc-remove="limit" data-oc-id="${escapeHtml(policy.id)}" aria-label="remove limit">remove</button>`,
  });
}

// ocLimitBody turns a draft into the strict POST document. Empty arrays are
// meaningful: they clear a dimension, which is how the UI expresses a scope
// shrink without deleting and recreating the rule.
function ocLimitBody(draft) {
  const burst = !!(draft.antiBurst && draft.concurrency && ocBurstAllowed(draft));
  const body = {
    ...ocScopeToWire(draft.scope),
    days: draft.schedule ? draft.schedule.days : [],
    start_minute: draft.schedule ? draft.schedule.start : 0,
    end_minute: draft.schedule ? draft.schedule.end : 0,
    concurrency: ocNumber(draft.concurrency),
    requests: ocNumber(draft.requests),
    request_window: draft.requests ? draft.requestWindow : '',
    tokens: ocNumber(draft.tokens),
    token_window: draft.tokens ? draft.tokenWindow : '',
    anti_burst: burst,
    ramp_duration: burst ? draft.ramp : '',
  };
  if (draft.id) body.id = draft.id;
  return body;
}

function ocApplyLimit() {
  const draft = ocDrafts.limit;
  if (!draft) return;
  const conc = ocNumber(draft.concurrency);
  const req = ocNumber(draft.requests);
  const tok = ocNumber(draft.tokens);
  if ([conc, req, tok].some(Number.isNaN)) {
    draft.error = OC_WHOLE_NUMBER_FAULT;
    ocRender();
    return;
  }
  if (conc + req + tok <= 0) {
    draft.error = 'set at least one budget';
    ocRender();
    return;
  }
  draft.error = '';
  const body = ocLimitBody(draft);
  const previous = draft.id ? ocLimitById(draft.id) : null;
  const phrase = (draft.id ? 'updated ' : 'added ') + 'limit for ' + ocScopePhrase(draft.scope) +
    ' · ' + ocBudgetPhrase({
      concurrency: body.concurrency,
      requests: body.requests,
      request_window: body.request_window,
      tokens: body.tokens,
      token_window: body.token_window,
    });
  const beforeIds = new Set((throttleState.throttles || []).map(t => t.id));
  mutateOperator('throttle', body, () => {
    const applied = (throttleState.throttles || []).find(t =>
      draft.id ? t.id === draft.id : !beforeIds.has(t.id));
    const undo = applied
      ? () => ocUndoLimit(applied.id, previous)
      : null;
    ocDrafts.limit = ocBlankLimitDraft();
    ocView = { kind: '', id: '' };
    ocRecordChange(phrase, undo, 'ok');
    ocRender();
  });
}

async function ocUndoLimit(newId, previous) {
  if (previous && newId !== previous.id) {
    await ocOperatorPost({ id: newId, clear: true });
  }
  if (previous) {
    await ocOperatorPost(ocLimitPolicyBody(previous));
  } else {
    await ocOperatorPost({ id: newId, clear: true });
  }
  await fetchBootstrap('none');
  applyThrottleState(await operatorJson('/admin/throttle'));
}

// ocLimitPolicyBody is the one POST document that restores a stored policy.
// It never carries the opaque id: a policy's identity is its normalized scope
// and schedule, so the server re-derives it. Sending an id that was just
// removed would answer 404 instead of recreating the rule.
function ocLimitPolicyBody(policy) {
  return {
    ...ocScopeToWire(ocScopeFromPolicy(policy)),
    days: policy.schedule?.days || [],
    start_minute: policy.schedule?.start_minute || 0,
    end_minute: policy.schedule?.end_minute || 0,
    concurrency: policy.concurrency || 0,
    requests: policy.requests || 0,
    request_window: policy.request_window || '',
    tokens: policy.tokens || 0,
    token_window: policy.token_window || '',
    anti_burst: !!policy.anti_burst,
    ramp_duration: policy.ramp_duration || '',
  };
}

async function ocRemoveLimit(id) {
  const policy = ocLimitById(id);
  if (!policy) { ocRuleGone('limit'); return; }
  const scope = ocPolicyScopePhrase(policy);
  const live = policy.in_flight
    ? ' ' + policy.in_flight + ' ' + agree(policy.in_flight, 'request is', 'requests are') + ' in flight and will finish.'
    : '';
  const ok = await ocConfirm({
    title: 'Remove this limit?',
    body: 'The limit on ' + scope + ' will stop applying.' + live + ' New matching requests run without this cap.',
    confirm: 'remove limit',
    danger: true,
  });
  if (!ok) return;
  const phrase = 'removed limit for ' + scope;
  mutateOperator('throttle', { id, clear: true }, () => {
    ocRecordChange(phrase, async () => {
      await ocOperatorPost(ocLimitPolicyBody(policy));
      await fetchBootstrap('none');
      applyThrottleState(await operatorJson('/admin/throttle'));
    }, 'ok');
    toast(phrase);
    ocRender();
  });
}

// ocOperatorPost is the console's one direct operator JSON write for undo and
// restore paths that are not a first-class mutation gate. It uses the shared
// credential and error owners, never a bare fetch.
async function ocOperatorPost(body) {
  return operatorJsonBody('/admin/throttle', {
    method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(body),
  }, 'could not change the limit');
}

