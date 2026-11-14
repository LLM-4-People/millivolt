// Operator limits: the policy list, the one composer, its live preview, the
// direct restore path used by undo, and the removal confirmation. The shell
// (operator_console.js) owns the readout, rail, section dispatch, ledger and
// event delegation; this module owns every limit rule and the POST shape.
//
// Ownership: the server owns policy semantics (scope intersection, schedule,
// identity, anti-burst eligibility). This module never re-implements them; it
// builds the one strict document and reads the one snapshot document.

// ---------- limits ----------
function ocBlankLimitDraft() {
  return {
    id: '',
    scope: ocNewScope(),
    schedule: null,
    concurrency: '',
    requests: '',
    requestWindow: '1m',
    tokens: '',
    tokenWindow: '1m',
    antiBurst: false,
    ramp: '',
    advanced: false,
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
    requestWindow: policy.request_window || '1m',
    tokens: policy.tokens ? String(policy.tokens) : '',
    tokenWindow: policy.token_window || '1m',
    antiBurst: !!policy.anti_burst,
    ramp: policy.ramp_duration || '',
    advanced: !!(policy.anti_burst || policy.ramp_duration),
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

function ocOpenLimitComposer(id, copy) {
  if (copy) {
    const source = ocLimitById(id);
    if (!source) return;
    ocDrafts.limit = ocLimitDraftCopy(source);
    ocView = { kind: 'limit', id: '' };
    ocSection = 'limits';
    ocRender();
    return;
  }
  if (id) {
    const policy = ocLimitById(id);
    if (!policy) return;
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

function ocLimitComposerHTML(draft) {
  const editing = !!draft.id;
  const conds = ocScopeConditionsHTML(draft, ['providers', 'models', 'clients', 'keyHashes', 'conversations']);
  const global = !ocScopeHas(draft.scope);
  const burstAllowed = ocBurstAllowed(draft);
  const scheduleOn = !!draft.schedule;
  const schedule = draft.schedule || { days: [], start: 540, end: 1020 };
  return `<section class="oc-compose" data-kind="limit" aria-label="${editing ? 'edit limit' : 'new limit'}">` +
    `<div class="oc-compose-hd"><span>${editing ? 'editing limit' : 'new limit'}</span></div>` +
    `<div class="oc-group"><div class="oc-label">applies to</div>${conds}` +
    (global ? `<p class="oc-note">No condition selected, so this limit applies to every request that crosses the proxy.</p>` : '') +
    `</div>` +
    `<div class="oc-group"><div class="oc-label">budgets</div>` +
    `<div class="oc-budget"><label class="oc-check"><input type="checkbox" data-oc-budget="concurrency" ${draft.concurrency !== '' ? 'checked' : ''}><span>concurrency</span></label>` +
    ocField('concurrency', '', draft.concurrency, { bare: true, aria: 'in flight', disabled: draft.concurrency === '', placeholder: '4' }) + `</div>` +
    `<div class="oc-budget"><label class="oc-check"><input type="checkbox" data-oc-budget="requests" ${draft.requests !== '' ? 'checked' : ''}><span>requests</span></label>` +
    ocField('requests', '', draft.requests, { bare: true, aria: 'requests per window', disabled: draft.requests === '', placeholder: '60' }) +
    `<div class="oc-window">${ocWindowSelect('request-window', draft.requestWindow, 'request window')}</div></div>` +
    `<div class="oc-budget"><label class="oc-check"><input type="checkbox" data-oc-budget="tokens" ${draft.tokens !== '' ? 'checked' : ''}><span>tokens</span></label>` +
    ocField('tokens', '', draft.tokens, { bare: true, aria: 'tokens per window', disabled: draft.tokens === '', placeholder: '100000' }) +
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
      `<div class="oc-days" role="group" aria-label="days">` + OC_DAY_LABELS.map((label, day) =>
        `<button type="button" class="oc-chip oc-chip-day" aria-pressed="${schedule.days.includes(day) ? 'true' : 'false'}" data-oc-day="${day}">${label}</button>`).join('') + `</div>` +
      `<div class="oc-times"><label class="oc-field"><span class="oc-field-name">from</span><input type="time" data-oc-time="start" value="${ocTimeValue(schedule.start)}"></label>` +
      `<label class="oc-field"><span class="oc-field-name">to</span><input type="time" data-oc-time="end" value="${ocTimeValue(schedule.end)}"></label>` +
      `<span class="oc-field-hint">server local time</span></div>` +
      `</div>` : '') +
    `</div>` +
    `<p class="oc-preview" id="oc-lim-preview" data-oc-key="lim-preview"></p>` +
    (draft.error ? `<p class="oc-error" role="alert">${escapeHtml(draft.error)}</p>` : '') +
    `<div class="oc-compose-ft">` +
    `<button type="button" class="btn" data-oc-cancel>cancel</button>` +
    `<button type="button" class="btn btn-accent" id="oc-lim-apply" data-oc-apply="limit">${editing ? 'save limit' : 'apply limit'}</button>` +
    `</div></section>`;
}

// ocLimitPreview is the live sentence above the commit button. It is the
// operator's proof of what will be enforced, written from the same scope and
// budget owners the rule rows use.
function ocLimitPreview() {
  const draft = ocDrafts.limit;
  if (!draft) return;
  const el = ocEl('oc-lim-preview');
  const apply = ocEl('oc-lim-apply');
  if (!el || !apply) return;
  const conc = ocNumber(draft.concurrency);
  const req = ocNumber(draft.requests);
  const tok = ocNumber(draft.tokens);
  const bad = [conc, req, tok].some(Number.isNaN);
  const any = conc > 0 || req > 0 || tok > 0;
  let text;
  if (bad) {
    text = 'counts must be whole numbers of 0 or more';
  } else if (!any) {
    text = 'set at least one budget';
  } else {
    const bits = [];
    if (conc > 0) bits.push(conc + ' requests in flight');
    if (req > 0) bits.push(fmt(req) + ' requests per ' + ocWindowLabel(draft.requestWindow));
    if (tok > 0) bits.push(fmt(tok) + ' tokens per ' + ocWindowLabel(draft.tokenWindow));
    text = 'At most ' + bits.join(', and ') + ', for ' + ocScopePhrase({
      providers: draft.scope.providers,
      models: draft.scope.models,
      clients: draft.scope.clients,
      keyHashes: draft.scope.keyHashes,
      conversations: draft.scope.conversations,
    }) + '.';
    if (draft.schedule) text += ' Active ' + ocSchedulePhrase(draft.schedule) + '.';
    if (draft.antiBurst && draft.ramp) text += ' Each new key ramps over ' + ocWindowLabel(draft.ramp) + '.';
  }
  el.textContent = text;
  el.dataset.tone = bad ? 'err' : '';
  apply.disabled = !!operatorState.throttle.busy || bad || !any;
}

function ocRenderLimits() {
  const policies = ((typeof throttleState !== 'undefined' && throttleState.throttles) || []);
  if (!ocDrafts.limit) ocDrafts.limit = ocBlankLimitDraft();
  const rows = policies.map(policy => ocLimitRuleHTML(policy)).join('');
  const empty = policies.length ? '' :
    `<div class="oc-empty"><strong>No limits in force.</strong> Every request is admitted, at whatever rate its provider accepts.</div>`;
  const composing = ocDrafts.limit && (ocDrafts.limit.id || ocView.kind === 'limit');
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><div><h2 class="oc-h2">limits</h2>` +
    `<p class="oc-sub">Cap concurrency, requests or tokens for any provider, model, client, key, conversation, or the whole proxy.</p></div>` +
    (composing
      ? `<button type="button" class="btn" data-oc-new="limit">start over</button>`
      : `<button type="button" class="btn btn-accent" data-oc-new="limit">new limit</button>`) + `</div>` +
    (composing ? ocLimitComposerHTML(ocDrafts.limit) : '') +
    (rows || empty) +
    `</div>`;
}

function ocLimitRuleHTML(policy) {
  const scope = ocScopePhrase({
    providers: policy.scope?.providers,
    models: policy.scope?.models,
    clients: policy.scope?.clients,
    keyHashes: policy.scope?.key_hashes,
    conversations: policy.scope?.conversations,
  });
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
  return `<div class="oc-rule" data-kind="limit" data-oc-rule="${escapeHtml(policy.id)}">` +
    `<button type="button" class="oc-rule-main" data-oc-edit="limit" data-oc-id="${escapeHtml(policy.id)}" data-oc-key="rule-${escapeHtml(policy.id)}" title="${escapeHtml(scope + ' · ' + source)}">` +
    `<span class="oc-rule-scope">${escapeHtml(scope)}</span>` +
    `<span class="oc-rule-budget">${escapeHtml(ocBudgetPhrase(policy))}${escapeHtml(schedule)}</span></button>` +
    `<div class="oc-rule-side"><span class="oc-rule-live">${escapeHtml(live.join(' · ') || 'idle')}</span>` +
    `<div class="oc-rule-actions"><button type="button" class="oc-icon-btn" data-oc-copy="limit" data-oc-id="${escapeHtml(policy.id)}" aria-label="copy this limit's scope into a new limit" title="copy this scope into a new limit">copy</button>` +
    `<button type="button" class="oc-icon-btn" data-oc-edit="limit" data-oc-id="${escapeHtml(policy.id)}" aria-label="edit limit">edit</button>` +
    `<button type="button" class="oc-icon-btn danger" data-oc-remove="limit" data-oc-id="${escapeHtml(policy.id)}" aria-label="remove limit">remove</button></div></div>` +
    `</div>`;
}

// ocLimitBody turns a draft into the strict POST document. Empty arrays are
// meaningful: they clear a dimension, which is how the UI expresses a scope
// shrink without deleting and recreating the rule.
function ocLimitBody(draft) {
  const burst = !!(draft.antiBurst && draft.concurrency && ocBurstAllowed(draft));
  const body = {
    providers: draft.scope.providers,
    models: draft.scope.models,
    clients: draft.scope.clients,
    key_hashes: draft.scope.keyHashes,
    conversations: draft.scope.conversations,
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
    draft.error = 'budgets must be whole numbers of 0 or more';
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
    toast(phrase);
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
    providers: policy.scope?.providers || [],
    models: policy.scope?.models || [],
    clients: policy.scope?.clients || [],
    key_hashes: policy.scope?.key_hashes || [],
    conversations: policy.scope?.conversations || [],
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
  if (!policy) return;
  const scope = ocScopePhrase({
    providers: policy.scope?.providers, models: policy.scope?.models,
    clients: policy.scope?.clients, keyHashes: policy.scope?.key_hashes,
    conversations: policy.scope?.conversations,
  });
  const live = policy.in_flight ? ' ' + policy.in_flight + ' request' +
    (policy.in_flight === 1 ? ' is' : 's are') + ' in flight and will finish.' : '';
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

