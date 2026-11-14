// Operator holds and captures: the pause and capture sections, their shared
// scope and duration controls, the one POST document each, and the resume and
// stop paths. The shell owns the readout, rail, ledger and event delegation;
// this module owns every hold and capture rule.

// ---------- pause ----------
function ocBlankPauseDraft() {
  return { id: '', mode: 'named', clients: [], providers: [], duration: '', cap: '', picker: null, error: '' };
}

function ocPauseDraftFromHold(hold) {
  return {
    id: hold.id || '',
    mode: hold.all ? 'all' : hold.new ? 'new' : 'named',
    clients: [...(hold.clients || [])],
    providers: [...(hold.providers || [])],
    duration: hold.duration || '',
    cap: typeof hold.max_queued === 'number' ? String(hold.max_queued) : '',
    picker: null,
    error: '',
  };
}

// A copy reuses the source hold's scope with no queue cap and no new id, so a
// second hold on a neighbouring client starts from the same shape.
function ocPauseDraftCopy(hold) {
  const draft = ocPauseDraftFromHold(hold);
  draft.id = '';
  draft.cap = '';
  return draft;
}

// A copy reuses the source capture's scope with its duration left at the
// default; the operator is choosing new evidence, not a new window.
function ocCaptureDraftCopy(session) {
  const draft = ocCaptureDraftFromSession(session);
  draft.id = '';
  draft.duration = '';
  return draft;
}

function ocOpenPauseComposer(id, copy) {
  if (copy) {
    const source = liveHolds().find(h => h.id === id);
    if (!source) return;
    ocDrafts.pause = ocPauseDraftCopy(source);
    ocView = { kind: 'pause', id: '' };
    ocSection = 'pause';
    ocRender();
    return;
  }
  if (id) {
    const hold = liveHolds().find(h => h.id === id);
    if (!hold) return;
    ocDrafts.pause = ocPauseDraftFromHold(hold);
    ocView = { kind: 'pause', id };
  } else {
    ocDrafts.pause = ocBlankPauseDraft();
    ocView = { kind: 'pause', id: '' };
  }
  ocSection = 'pause';
  ocRender();
}

function ocPauseComposerHTML(draft) {
  const editing = !!draft.id;
  const named = draft.mode === 'named';
  const scope = { providers: draft.providers, models: [], clients: draft.clients, keyHashes: [], conversations: [] };
  return `<section class="oc-compose" data-kind="pause">` +
    `<div class="oc-compose-hd"><span>${editing ? 'editing hold' : 'new hold'}</span></div>` +
    `<div class="oc-group"><div class="oc-label">scope</div>` +
    ocChipRow('pause-mode', ['named', 'new', 'all'], draft.mode, v =>
      v === 'all' ? 'everything' : v === 'new' ? 'new clients' : 'named clients and providers') +
    (named ? ocScopeConditionsHTML({ scope: { providers: draft.providers, models: [], clients: draft.clients, keyHashes: [], conversations: [] }, picker: draft.picker }, ['clients', 'providers']) : '') +
    `<p class="oc-note">${draft.mode === 'all'
      ? 'Every new request parks. In-flight work always finishes.'
      : draft.mode === 'new'
        ? 'Clients unseen when this hold starts park. Requests already running finish.'
        : 'Pick at least one client or provider. With one dimension left empty, that side matches any value.'}</p></div>` +
    `<div class="oc-group"><div class="oc-label">until</div>` +
    ocChipRow('pause-duration', [''].concat(PAUSE_DURS.slice(1).map(([v]) => v)), draft.duration, v => v ? ocDurationText(v) : PAUSE_DURS[0][1]) +
    `</div>` +
    `<div class="oc-group"><label class="oc-check"><input type="checkbox" data-oc-field="pauseCapOn" ${draft.cap !== '' ? 'checked' : ''}><span>limit the hold queue</span></label>` +
    (draft.cap !== '' ? ocField('pauseCap', 'queued requests', draft.cap, { placeholder: String(pauseState.default_max_queued || 1024) }) : '') +
    `</div>` +
    `<p class="oc-preview" id="oc-pause-preview" data-oc-key="pause-preview"></p>` +
    (draft.error ? `<p class="oc-error" role="alert">${escapeHtml(draft.error)}</p>` : '') +
    `<div class="oc-compose-ft"><button type="button" class="btn" data-oc-cancel>cancel</button>` +
    `<button type="button" class="btn btn-accent" id="oc-pause-apply" data-oc-apply="pause">${editing ? 'save hold' : 'hold traffic'}</button></div>` +
    `</section>`;
}

function ocPausePreview() {
  const draft = ocDrafts.pause;
  const el = ocEl('oc-pause-preview');
  const apply = ocEl('oc-pause-apply');
  if (!draft || !el || !apply) return;
  const named = draft.mode === 'named';
  const empty = named && !draft.clients.length && !draft.providers.length;
  let text;
  if (empty) text = 'Pick at least one client or provider, or hold everything.';
  else {
    const scope = draft.mode === 'all' ? 'all traffic'
      : draft.mode === 'new' ? 'new clients'
        : ocScopePhrase({ providers: draft.providers, clients: draft.clients, models: [], keyHashes: [], conversations: [] });
    text = 'Hold ' + scope + (draft.duration ? ' for ' + ocDurationText(draft.duration) : ' until resumed') + '.';
  }
  el.textContent = text;
  apply.disabled = !!operatorState.pause.busy || empty;
}

function ocRenderPause() {
  const holds = liveHolds();
  if (!ocDrafts.pause) ocDrafts.pause = ocBlankPauseDraft();
  const composing = ocDrafts.pause && (ocDrafts.pause.id || ocView.kind === 'pause');
  const rows = holds.map(hold => {
    const scope = hold.all ? 'all traffic'
      : hold.new ? 'new clients'
        : ocScopePhrase({ providers: hold.providers, clients: hold.clients, models: [], keyHashes: [], conversations: [] });
    // The countdown is the side column's fact; the meta line carries only the
    // context around it, so one row never states the same number twice.
    const meta = [hold.queued ? hold.queued + ' queued' : '', hold.reason || ''].filter(Boolean);
    return `<div class="oc-rule" data-kind="pause" data-oc-rule="${escapeHtml(hold.id || '')}">` +
      `<button type="button" class="oc-rule-main" data-oc-edit="pause" data-oc-id="${escapeHtml(hold.id || '')}" data-oc-key="rule-${escapeHtml(hold.id || '')}" title="${escapeHtml(scope)}">` +
      `<span class="oc-rule-scope">${escapeHtml(scope)}</span>` +
      `<span class="oc-rule-budget">${escapeHtml([hold.max_queued ? 'queue cap ' + fmt(hold.max_queued) : 'default queue', ...meta].join(' · '))}</span></button>` +
      `<div class="oc-rule-side"><span class="oc-rule-live">${escapeHtml(ocUntilPhrase(hold.until) || 'open')}</span>` +
      `<div class="oc-rule-actions"><button type="button" class="oc-icon-btn" data-oc-copy="pause" data-oc-id="${escapeHtml(hold.id || '')}" aria-label="copy this hold's scope into a new hold" title="copy this scope into a new hold">copy</button>` +
      `<button type="button" class="oc-icon-btn" data-oc-resume="${escapeHtml(hold.id || '')}">resume</button></div></div></div>`;
  }).join('');
  const empty = holds.length ? '' :
    `<div class="oc-empty"><strong>Traffic is flowing.</strong> Nothing is held, so requests reach their provider as fast as it allows.</div>`;
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><p class="oc-sub">Park new matching requests without cancelling work that is already running.</p>` +
    `<div class="oc-head-actions"><button type="button" class="btn" data-oc-resume-all ${holds.length ? '' : 'disabled'}>resume all</button>` +
    (composing
      ? `<button type="button" class="btn" data-oc-new="pause">start over</button>`
      : `<button type="button" class="btn" data-oc-quick-hold title="hold all traffic for ${ocQuickHoldMinutes} minutes">hold ${ocQuickHoldMinutes}m</button>` +
        `<button type="button" class="btn btn-accent" data-oc-new="pause">new hold</button>`) + `</div></div>` +
    (composing ? ocPauseComposerHTML(ocDrafts.pause) : '') + (rows || empty) + `</div>`;
}

// ocQuickHoldMinutes is the bounded duration the one-click hold uses. A quick
// action must not be able to leave traffic parked indefinitely: the indefinite
// hold exists, but it goes through the composer where the operator chooses it.
const ocQuickHoldMinutes = 15;

// ocQuickHold parks all traffic for a fixed window in one click. It writes the
// same document the composer writes, through the same gate and ledger, so undo
// and the confirmation rules are identical.
function ocQuickHold() {
  const draft = ocBlankPauseDraft();
  draft.mode = 'all';
  draft.duration = String(ocQuickHoldMinutes) + 'm';
  ocDrafts.pause = draft;
  ocApplyPause();
}

function ocPauseBody(draft) {
  const all = draft.mode === 'all';
  const body = { paused: true, all, duration: draft.duration || '' };
  if (!all) {
    body.new = draft.mode === 'new';
    body.clients = draft.clients;
    body.providers = draft.providers;
  }
  if (draft.cap !== '') body.max_queued = ocNumber(draft.cap);
  if (draft.id) body.id = draft.id;
  return body;
}

function ocApplyPause() {
  const draft = ocDrafts.pause;
  if (!draft) return;
  if (draft.mode === 'named' && !draft.clients.length && !draft.providers.length) {
    draft.error = 'pick at least one client or provider, or hold everything';
    ocRender();
    return;
  }
  const cap = draft.cap === '' ? null : ocNumber(draft.cap);
  if (cap !== null && Number.isNaN(cap)) {
    draft.error = 'queue cap must be a whole number of 0 or more';
    ocRender();
    return;
  }
  draft.error = '';
  const body = ocPauseBody(draft);
  const previous = draft.id ? liveHolds().find(h => h.id === draft.id) || null : null;
  const phrase = (draft.id ? 'updated hold on ' : 'held ') + ocScopePhrase({
    providers: body.providers || [], clients: body.clients || [], models: [], keyHashes: [], conversations: [],
  }).replace('every request', body.all ? 'all traffic' : body.new ? 'new clients' : 'traffic');
  // Undo must name the rule this write created. `find(h => h.id !== previous.id)`
  // looks right but is wrong on a create, where previous is null: it matches
  // whatever is first in the list, which is the oldest hold, not the new one.
  // Identity is the set difference against the ids that existed before.
  const beforeIds = new Set(liveHolds().map(h => h.id));
  mutateOperator('pause', body, () => {
    const created = liveHolds().find(h => !beforeIds.has(h.id)) || null;
    const undo = created
      ? () => mutateOperator('pause', { paused: false, id: created.id })
      : previous ? () => mutateOperator('pause', ocPauseBody(ocPauseDraftFromHold(previous))) : null;
    ocDrafts.pause = ocBlankPauseDraft();
    ocView = { kind: '', id: '' };
    ocRecordChange(phrase, undo, 'ok');
    toast(phrase);
    ocRender();
  });
}

async function ocResumeHold(id) {
  const hold = liveHolds().find(h => h.id === id);
  if (!hold) return;
  const scope = hold.all ? 'all traffic'
    : hold.new ? 'new clients' : ocScopePhrase({ providers: hold.providers, clients: hold.clients });
  const phrase = 'resumed ' + scope;
  mutateOperator('pause', { paused: false, id }, () => {
    ocRecordChange(phrase, () => { const draft = ocPauseDraftFromHold(hold); draft.id = ''; return mutateOperator('pause', ocPauseBody(draft)); }, 'ok');
    toast(phrase);
    ocRender();
  });
}

async function ocResumeAllHolds() {
  const holds = liveHolds();
  if (!holds.length) return;
  const ok = await ocConfirm({
    title: 'Resume every hold?',
    body: holds.length + (holds.length === 1 ? ' hold is' : ' holds are') + ' parking new matching requests. Queued work is released to the provider. In-flight streams finish normally.',
    confirm: 'resume all',
    danger: true,
  });
  if (!ok) return;
  mutateOperator('pause', { paused: false }, () => {
    const restore = holds.slice();
    ocRecordChange('resumed every hold', async () => {
      for (const hold of restore) { const draft = ocPauseDraftFromHold(hold); draft.id = ''; await mutateOperator('pause', ocPauseBody(draft)); }
    }, 'ok');
    toast('resumed every hold');
    ocRender();
  });
}

// ---------- capture ----------
function ocBlankCaptureDraft() {
  return { id: '', clients: [], providers: [], models: [], duration: '', picker: null, error: '' };
}

function ocCaptureDraftFromSession(session) {
  return {
    id: session.id || '',
    clients: [...(session.clients || [])],
    providers: [...(session.providers || [])],
    models: [...(session.models || [])],
    duration: session.duration || '',
    picker: null,
    error: '',
  };
}

function ocOpenCaptureComposer(id, copy) {
  if (copy) {
    const source = liveDebugSessions().find(s => s.id === id);
    if (!source) return;
    ocDrafts.capture = ocCaptureDraftCopy(source);
    ocView = { kind: 'capture', id: '' };
    ocSection = 'capture';
    ocRender();
    return;
  }
  if (id) {
    const session = liveDebugSessions().find(s => s.id === id);
    if (!session) return;
    ocDrafts.capture = ocCaptureDraftFromSession(session);
    ocView = { kind: 'capture', id };
  } else {
    ocDrafts.capture = ocBlankCaptureDraft();
    ocView = { kind: 'capture', id: '' };
  }
  ocSection = 'capture';
  ocRender();
}

function ocCaptureComposerHTML(draft) {
  const editing = !!draft.id;
  const blocked = debugStorageDisabled();
  return `<section class="oc-compose" data-kind="capture">` +
    `<div class="oc-compose-hd"><span>${editing ? 'editing capture' : 'new capture'}</span></div>` +
    `<div class="oc-group"><div class="oc-label">capture</div>` +
    ocScopeConditionsHTML({ scope: { providers: draft.providers, models: draft.models, clients: draft.clients, keyHashes: [], conversations: [] }, picker: draft.picker }, ['clients', 'providers', 'models']) +
    `<p class="oc-note">Pick at least one client, provider or model. Captures can contain credentials and private content; keep the window short and download only what you need.</p></div>` +
    `<div class="oc-group"><div class="oc-label">until</div>` +
    ocChipRow('capture-duration', [''].concat(PAUSE_DURS.slice(1).map(([v]) => v)), draft.duration, v => v ? ocDurationText(v) : 'until stopped') +
    `</div>` +
    `<p class="oc-facts">${escapeHtml(ocCaptureFacts())}</p>` +
    (blocked ? `<p class="oc-warning" role="alert">Durable storage is off, so captures cannot start. Set db_path in Settings, then restart to apply it.` +
      `<button type="button" class="oc-link" data-oc-settings="server">open server settings</button></p>` : '') +
    `<p class="oc-preview" id="oc-capture-preview" data-oc-key="capture-preview"></p>` +
    (draft.error ? `<p class="oc-error" role="alert">${escapeHtml(draft.error)}</p>` : '') +
    `<div class="oc-compose-ft"><button type="button" class="btn" data-oc-cancel>cancel</button>` +
    `<button type="button" class="btn btn-accent" id="oc-capture-apply" data-oc-apply="capture">${editing ? 'save capture' : 'start capture'}</button></div>` +
    `</section>`;
}

function ocCaptureFacts() {
  const ttl = debugState.ttl || '';
  const max = debugState.max_bytes || '';
  const bits = [];
  if (ttl) bits.push('retained ' + ttl);
  if (max) bits.push(max + ' per capture');
  bits.push('headers redacted, bodies retained');
  return bits.join(' · ');
}

function ocCapturePreview() {
  const draft = ocDrafts.capture;
  const el = ocEl('oc-capture-preview');
  const apply = ocEl('oc-capture-apply');
  if (!draft || !el || !apply) return;
  const empty = !draft.clients.length && !draft.providers.length && !draft.models.length;
  el.textContent = empty ? 'Pick at least one client, provider or model.'
    : 'Capture ' + ocScopePhrase({
      providers: draft.providers, models: draft.models, clients: draft.clients,
      keyHashes: [], conversations: [],
    }) + (draft.duration ? ' for ' + ocDurationText(draft.duration) : ' until stopped') + '.';
  apply.disabled = !!operatorState.debug.busy || empty || debugStorageDisabled();
}

function ocRenderCapture() {
  const sessions = liveDebugSessions();
  if (!ocDrafts.capture) ocDrafts.capture = ocBlankCaptureDraft();
  const composing = ocDrafts.capture && (ocDrafts.capture.id || ocView.kind === 'capture');
  const rows = sessions.map(session => {
    const scope = ocScopePhrase({
      providers: session.providers, models: session.models, clients: session.clients,
      keyHashes: [], conversations: [],
    });
    const left = ocUntilPhrase(session.until) || 'until stopped';
    const ran = typeof session.ran_ms === 'number' && session.ran_ms > 0
      ? 'ran ' + hmBreakdown(Math.max(1, Math.round(session.ran_ms / OC_MINUTE))) : '';
    const n = typeof session.captures === 'number' ? session.captures : 0;
    // Capture count and elapsed time own the budget line; the side column owns
    // the remaining window, which is the same fact the row's action changes.
    return `<div class="oc-rule" data-kind="capture" data-oc-rule="${escapeHtml(session.id || '')}">` +
      `<button type="button" class="oc-rule-main" data-oc-edit="capture" data-oc-id="${escapeHtml(session.id || '')}" data-oc-key="rule-${escapeHtml(session.id || '')}" title="${escapeHtml(scope)}">` +
      `<span class="oc-rule-scope">${escapeHtml(scope)}</span>` +
      `<span class="oc-rule-budget">${escapeHtml([n + (n === 1 ? ' capture' : ' captures'), ran,
        session.duration ? ocDurationText(session.duration) : 'manual stop'].filter(Boolean).join(' · '))}</span></button>` +
      `<div class="oc-rule-side"><span class="oc-rule-live">${escapeHtml(left)}</span>` +
      `<div class="oc-rule-actions"><button type="button" class="oc-icon-btn" data-oc-copy="capture" data-oc-id="${escapeHtml(session.id || '')}" aria-label="copy this capture's scope into a new capture" title="copy this scope into a new capture">copy</button>` +
      `<button type="button" class="oc-icon-btn danger" data-oc-stop="${escapeHtml(session.id || '')}">stop</button></div></div></div>`;
  }).join('');
  const empty = sessions.length ? '' :
    `<div class="oc-empty"><strong>No capture sessions.</strong> Nothing is being recorded, so stored records hold only headers and metrics.</div>`;
  return `<div class="oc-sec">` +
    `<div class="oc-sec-head"><p class="oc-sub">Store request and response bodies for a scoped, time-bounded debug session.</p>` +
    `<div class="oc-head-actions"><button type="button" class="btn" data-oc-stop-all ${sessions.length ? '' : 'disabled'}>stop all</button>` +
    (composing
      ? `<button type="button" class="btn" data-oc-new="capture">start over</button>`
      : `<button type="button" class="btn btn-accent" data-oc-new="capture">new capture</button>`) + `</div></div>` +
    (composing ? ocCaptureComposerHTML(ocDrafts.capture) : '') + (rows || empty) + `</div>`;
}

function ocCaptureBody(draft) {
  const body = {
    enabled: true,
    clients: draft.clients,
    providers: draft.providers,
    models: draft.models,
    duration: draft.duration || '',
  };
  if (draft.id) body.id = draft.id;
  return body;
}

function ocApplyCapture() {
  const draft = ocDrafts.capture;
  if (!draft) return;
  if (!draft.clients.length && !draft.providers.length && !draft.models.length) {
    draft.error = 'pick at least one client, provider or model';
    ocRender();
    return;
  }
  draft.error = '';
  const body = ocCaptureBody(draft);
  const previous = draft.id ? liveDebugSessions().find(s => s.id === draft.id) || null : null;
  const scope = ocScopePhrase({
    providers: draft.providers, models: draft.models, clients: draft.clients,
    keyHashes: [], conversations: [],
  });
  const phrase = (draft.id ? 'updated capture on ' : 'capturing ') + scope;
  // Same set-difference identity as the hold path: an edit keeps its id, a
  // create is whatever id did not exist before this write.
  const beforeIds = new Set(liveDebugSessions().map(s => s.id));
  mutateOperator('debug', body, () => {
    const created = liveDebugSessions().find(s => !beforeIds.has(s.id)) || null;
    const undo = created
      ? () => mutateOperator('debug', { enabled: false, id: created.id })
      : previous ? () => mutateOperator('debug', ocCaptureBody(ocCaptureDraftFromSession(previous))) : null;
    ocDrafts.capture = ocBlankCaptureDraft();
    ocView = { kind: '', id: '' };
    ocRecordChange(phrase, undo, 'ok');
    toast(phrase);
    ocRender();
  });
}

async function ocStopSession(id) {
  const session = liveDebugSessions().find(s => s.id === id);
  if (!session) return;
  const scope = ocScopePhrase({ providers: session.providers, models: session.models, clients: session.clients });
  const phrase = 'stopped capture on ' + scope;
  mutateOperator('debug', { enabled: false, id }, () => {
    ocRecordChange(phrase, () => { const draft = ocCaptureDraftFromSession(session); draft.id = ''; return mutateOperator('debug', ocCaptureBody(draft)); }, 'ok');
    toast(phrase);
    ocRender();
  });
}

async function ocStopAllSessions() {
  const sessions = liveDebugSessions();
  if (!sessions.length) return;
  const ok = await ocConfirm({
    title: 'Stop every capture?',
    body: sessions.length + (sessions.length === 1 ? ' session is' : ' sessions are') + ' storing request and response bodies. Retained captures stay readable until their retention window ends.',
    confirm: 'stop all',
    danger: true,
  });
  if (!ok) return;
  mutateOperator('debug', { enabled: false }, () => {
    const restore = sessions.slice();
    ocRecordChange('stopped every capture', async () => {
      for (const session of restore) { const draft = ocCaptureDraftFromSession(session); draft.id = ''; await mutateOperator('debug', ocCaptureBody(draft)); }
    }, 'ok');
    toast('stopped every capture');
    ocRender();
  });
}

