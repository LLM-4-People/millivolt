// Operator holds and captures: the pause and capture sections, their shared
// scope and duration controls, the one POST document each, and the resume and
// stop paths. The shell owns the readout, rail, ledger and event delegation;
// this module owns every hold and capture rule.

// ---------- pause ----------
// A hold's scope is the same five dimensions a limit has, because the
// scheduler matches both against one request identity. Spelling them out per
// draft kept the hold composer at two dimensions while the limit composer
// offered five, for no reason the operator could see.
function ocBlankPauseDraft() {
  return {
    id: '', mode: 'named', scope: ocNewScope(),
    duration: '', cap: '', picker: null, error: '',
  };
}

function ocPauseDraftFromHold(hold) {
  return {
    id: hold.id || '',
    mode: hold.all ? 'all' : hold.new ? 'new' : 'named',
    scope: ocScopeFromPolicy({ scope: hold }),
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
    if (!source) { ocRuleGone('hold'); return; }
    ocDrafts.pause = ocPauseDraftCopy(source);
    ocView = { kind: 'pause', id: '' };
    ocSection = 'pause';
    ocRender();
    return;
  }
  if (id) {
    const hold = liveHolds().find(h => h.id === id);
    if (!hold) { ocRuleGone('hold'); return; }
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
  return `<section class="oc-compose" data-kind="pause">` +
    `<div class="oc-compose-hd"><span>${editing ? 'editing hold' : 'new hold'}</span></div>` +
    `<div class="oc-group"><div class="oc-label">scope</div>` +
    ocChipRow(OC_CHOICES.pauseMode, ['named', 'new', 'all'], draft.mode, v =>
      v === 'all' ? 'everything' : v === 'new' ? 'new clients' : 'named scope') +
    // The same five dimensions the limit composer offers. A hold that can only
    // name clients and providers cannot ask "park this model" while a limit can
    // cap it, which is a difference the operator cannot account for.
    (named ? ocScopeConditionsHTML({ scope: draft.scope, picker: draft.picker }, OC_SCOPE_KEYS) : '') +
    `<p class="oc-note">${draft.mode === 'all'
      ? 'Every new request parks. In-flight work always finishes.'
      : draft.mode === 'new'
        ? 'Clients unseen when this hold starts park. Requests already running finish.'
        : 'A dimension left empty matches any value; set dimensions combine.'}</p></div>` +
    `<div class="oc-group"><div class="oc-label">until</div>` +
    ocChipRow(OC_CHOICES.pauseDuration, [''].concat(PAUSE_DURS.slice(1).map(([v]) => v)), draft.duration, v => v ? ocDurationText(v) : PAUSE_DURS[0][1]) +
    `</div>` +
    `<div class="oc-group"><label class="oc-check"><input type="checkbox" data-oc-field="pauseCapOn" ${draft.cap !== '' ? 'checked' : ''}><span>limit the hold queue</span></label>` +
    (draft.cap !== '' ? ocField('pauseCap', 'queued requests', draft.cap, { placeholder: String(ocPauseDefaultCap()) }) : '') +
    `</div>` +
    ocComposeTailHTML('pause', { error: draft.error, label: editing ? 'save hold' : 'hold traffic' }) +
    `</section>`;
}

function ocPausePreview() {
  const draft = ocDrafts.pause;
  const nodes = ocComposeControls('pause');
  if (!draft || !nodes) return;
  const { preview: el, apply } = nodes;
  const named = draft.mode === 'named';
  const empty = named && !ocScopeHas(draft.scope);
  let text;
  if (empty) text = 'Pick at least one condition, or hold everything.';
  else {
    const scope = ocHoldDraftScopePhrase(draft);
    text = 'Hold ' + scope + (draft.duration ? ' for ' + ocDurationText(draft.duration) : ' until resumed') + '.';
  }
  el.textContent = text;
  apply.disabled = !!operatorState.pause.busy || empty;
}

function ocRenderPause() {
  const holds = liveHolds();
  if (!ocDrafts.pause) ocDrafts.pause = ocBlankPauseDraft();
  const composing = ocComposing('pause');
  // One action row while a draft is open: the composer footer owns cancel and
  // commit. A head "start over" beside it was a second, silent discard of the
  // same draft. It stays only while editing a stored hold.
  const headAction = ocDrafts.pause?.id
    ? `<button type="button" class="btn" data-oc-new="pause">start over</button>`
    : composing ? ''
      // The label names the scope. "hold 15m" read as a duration preset for
      // the composer beside it, and its blast radius lived only in the title,
      // which a touch operator never sees. It stays one click: this is the
      // incident shortcut, and a dialog would defeat it.
      : `<button type="button" class="btn" data-oc-quick-hold title="hold all traffic for ${escapeHtml(ocDurationText(ocQuickHoldDuration))}">hold all ${escapeHtml(ocQuickHoldDuration)}</button>` +
        `<button type="button" class="btn btn-accent" data-oc-new="pause">new hold</button>`;
  const entries = holds.map(hold => {
    const scope = ocHoldScopePhrase(hold);
    // The countdown is the side column's fact; the meta line carries only the
    // context around it, so one row never states the same number twice.
    const meta = [hold.queued ? hold.queued + ' queued' : '', hold.reason || ''].filter(Boolean);
    return ocRuleRowHTML({
      kind: 'pause', id: hold.id || '', scope, title: scope,
      // A stored 0 is the server's "no cap on this hold", not "use the
      // default": an omitted cap is written as the configured queue size at
      // creation, so 0 only ever reaches here because the operator chose
      // unlimited. Calling that "default queue" named the opposite bound.
      budget: [hold.max_queued ? 'queue cap ' + fmt(hold.max_queued) : 'queue unlimited', ...meta].join(' · '),
      live: ocUntilPhrase(hold.until) || 'open',
      actions: `<button type="button" class="oc-icon-btn" data-oc-copy="pause" data-oc-id="${escapeHtml(hold.id || '')}" aria-label="copy this hold's scope into a new hold" title="copy this scope into a new hold">copy</button>` +
        `<button type="button" class="oc-icon-btn" data-oc-resume="${escapeHtml(hold.id || '')}">resume</button>`,
    });
  });
  // "As fast as it allows" was only true when no limit and no queue bound
  // applied, and this section cannot see either. It states what it knows.
  const empty = ocSectionEmpty('pause', holds.length, 'No holds.',
    'Nothing is parked. New requests go straight to admission, where limits and the queue can still delay them.');
  return ocSectionHTML({
    kind: 'pause',
    sub: 'Park new matching requests without cancelling work that is already running.',
    actions: `<button type="button" class="btn" data-oc-resume-all ${holds.length ? '' : 'disabled'}>resume all</button>` + headAction,
    composer: composing ? ocPauseComposerHTML(ocDrafts.pause) : '',
    entries, total: holds.length, empty,
  });
}

// ocQuickHoldDuration is the bounded duration the one-click hold uses. A quick
// action must not be able to leave traffic parked indefinitely: the indefinite
// hold exists, but it goes through the composer where the operator chooses it.
//
// It is the token itself, not a number that gets a unit appended. PAUSE_DURS is
// the console's copy of the server's allowlist, and a Go test posts every token
// on it through both operator handlers, so naming the token here keeps the
// quick hold inside that contract. Composing "15" + "m" would reintroduce a
// literal that no pin covers, and a token the server rejects would fail at the
// click rather than in the suite.
const ocQuickHoldDuration = '15m';

// ocQuickHold parks all traffic for a fixed window in one click. It writes the
// same document the composer writes, through the same gate and ledger, so undo
// and the confirmation rules are identical.
function ocQuickHold() {
  const draft = ocBlankPauseDraft();
  draft.mode = 'all';
  draft.duration = ocQuickHoldDuration;
  ocDrafts.pause = draft;
  ocApplyPause();
}

function ocPauseBody(draft) {
  const all = draft.mode === 'all';
  const body = { paused: true, all, duration: draft.duration || '' };
  if (!all) {
    body.new = draft.mode === 'new';
    // Every dimension is restated, because the server treats a supplied array
    // as a replacement and an omitted one as "keep". A narrow hold must not
    // leave a previous broader dimension in place. The field names come from
    // the scope table, so a dimension rename reaches the request body too.
    Object.assign(body, ocScopeToWire(draft.scope));
  }
  // An omitted cap means different things on the two paths. On a create the
  // server writes the configured queue size, which is what an unchecked box
  // asks for. On an edit it keeps the previous value, so an omitted cap made
  // "uncheck the box" a no-op: the row went on printing a cap the operator had
  // just removed. Unchecking a stored hold therefore sends the server's own
  // word for no cap, which is zero.
  if (draft.cap !== '') body.max_queued = ocNumber(draft.cap);
  else if (draft.id) body.max_queued = 0;
  if (draft.id) body.id = draft.id;
  return body;
}

function ocApplyPause() {
  const draft = ocDrafts.pause;
  if (!draft) return;
  if (draft.mode === 'named' && !ocScopeHas(draft.scope)) {
    draft.error = 'pick at least one condition, or hold everything';
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
  const phrase = (draft.id ? 'updated hold on ' : 'held ') + ocHoldDraftScopePhrase(draft);
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
    ocRender();
  });
}

async function ocResumeHold(id) {
  const hold = liveHolds().find(h => h.id === id);
  if (!hold) { ocRuleGone('hold'); return; }
  const scope = ocHoldScopePhrase(hold);
  // Resuming a hold with parked work releases that backlog to the provider in
  // one burst, which is the same consequence `resume all` confirms. Asking
  // only in the bulk case taught the operator that the single resume was the
  // safer of the two when it has the identical effect on a smaller scope.
  if (Number(hold.queued) > 0) {
    const ok = await ocConfirm({
      title: 'Resume this hold?',
      body: fmt(Number(hold.queued)) + ' parked ' +
        agree(Number(hold.queued), 'request is', 'requests are') +
        ' waiting behind it. Resuming releases them to the provider. In-flight streams finish normally.',
      confirm: 'resume',
      danger: true,
    });
    if (!ok) return;
  }
  const phrase = 'resumed ' + scope;
  mutateOperator('pause', { paused: false, id }, () => {
    ocRecordChange(phrase, () => { const draft = ocPauseDraftFromHold(hold); draft.id = ''; return mutateOperator('pause', ocPauseBody(draft)); }, 'ok');
    ocRender();
  });
}

async function ocResumeAllHolds() {
  const holds = liveHolds();
  if (!holds.length) return;
  const ok = await ocConfirm({
    title: 'Resume every hold?',
    body: holds.length + ' ' + agree(holds.length, 'hold is', 'holds are') + ' parking new matching requests. Queued work is released to the provider. In-flight streams finish normally.',
    confirm: 'resume all',
    danger: true,
  });
  if (!ok) return;
  mutateOperator('pause', { paused: false }, () => {
    const restore = holds.slice();
    // A partial restore must not read as a finished one: the first refusal
    // leaves the holds it could not recreate off, and the ledger has to keep
    // the entry so the operator can try again.
    ocRecordChange('resumed every hold', async () => {
      let all = true;
      for (const hold of restore) {
        const draft = ocPauseDraftFromHold(hold);
        draft.id = '';
        if (await mutateOperator('pause', ocPauseBody(draft)) === false) all = false;
      }
      return all;
    }, 'ok');
    ocRender();
  });
}

// ---------- capture ----------
function ocBlankCaptureDraft() {
  return { id: '', scope: ocNewScope(), duration: '', picker: null, error: '' };
}

function ocCaptureDraftFromSession(session) {
  return {
    id: session.id || '',
    scope: ocScopeFromPolicy({ scope: session }),
    duration: session.duration || '',
    picker: null,
    error: '',
  };
}

function ocOpenCaptureComposer(id, copy) {
  if (copy) {
    const source = liveDebugSessions().find(s => s.id === id);
    if (!source) { ocRuleGone('capture'); return; }
    ocDrafts.capture = ocCaptureDraftCopy(source);
    ocView = { kind: 'capture', id: '' };
    ocSection = 'capture';
    ocRender();
    return;
  }
  if (id) {
    const session = liveDebugSessions().find(s => s.id === id);
    if (!session) { ocRuleGone('capture'); return; }
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
    ocScopeConditionsHTML({ scope: draft.scope, picker: draft.picker }, OC_CAPTURE_SCOPE_KEYS) +
    `<p class="oc-note">Captures can contain credentials and private content; keep the window short and download only what you need.</p></div>` +
    `<div class="oc-group"><div class="oc-label">until</div>` +
    ocChipRow(OC_CHOICES.captureDuration, [''].concat(PAUSE_DURS.slice(1).map(([v]) => v)), draft.duration, v => v ? ocDurationText(v) : 'until stopped') +
    `</div>` +
    `<p class="oc-facts">${escapeHtml(ocCaptureFacts())}</p>` +
    (blocked ? `<p class="oc-warning" role="alert">Durable storage is off, so captures cannot start. Set db_path in Settings, then restart to apply it.` +
      `<button type="button" class="oc-link" data-oc-settings="server">open server settings</button></p>` : '') +
    ocComposeTailHTML('capture', { error: draft.error, label: editing ? 'save capture' : 'start capture' }) +
    `</section>`;
}

function ocCaptureFacts() {
  const ttl = debugState.ttl || '';
  const max = debugState.max_bytes || '';
  const bits = [];
  if (ttl) bits.push('retained ' + ttl);
  if (max) bits.push(max + ' per capture');
  // Redaction is by header name against the credential vocabulary, not a
  // guarantee about every value: a secret in a header whose name does not look
  // credential-bearing is retained. Saying "headers redacted" here contradicted
  // the sensitivity warning one line above it, and an operator could reasonably
  // read it as "the headers are safe".
  bits.push('credential headers redacted');
  return bits.join(' · ');
}

function ocCapturePreview() {
  const draft = ocDrafts.capture;
  const nodes = ocComposeControls('capture');
  if (!draft || !nodes) return;
  const { preview: el, apply } = nodes;
  const empty = !ocScopeHas(draft.scope);
  el.textContent = empty ? 'Pick at least one condition.'
    : 'Capture ' + ocScopePhrase(draft.scope) +
      (draft.duration ? ' for ' + ocDurationText(draft.duration) : ' until stopped') + '.';
  apply.disabled = !!operatorState.debug.busy || empty || debugStorageDisabled();
}

function ocRenderCapture() {
  const sessions = liveDebugSessions();
  if (!ocDrafts.capture) ocDrafts.capture = ocBlankCaptureDraft();
  const composing = ocComposing('capture');
  // One action row while a draft is open: the composer footer. See the pause
  // section for why the head keeps "start over" only while editing.
  const headAction = ocDrafts.capture?.id
    ? `<button type="button" class="btn" data-oc-new="capture">start over</button>`
    : composing ? ''
      : `<button type="button" class="btn btn-accent" data-oc-new="capture">new capture</button>`;
  const entries = sessions.map(session => {
    const scope = ocScopePhrase(ocScopeFromPolicy({ scope: session }));
    const left = ocUntilPhrase(session.until) || 'until stopped';
    const ran = typeof session.ran_ms === 'number' && session.ran_ms > 0
      ? 'ran ' + hmBreakdown(Math.max(1, Math.round(session.ran_ms / OC_MINUTE))) : '';
    const n = typeof session.captures === 'number' ? session.captures : 0;
    // Capture count and elapsed time own the budget line; the side column owns
    // the remaining window, which is the same fact the row's action changes.
    return ocRuleRowHTML({
      kind: 'capture', id: session.id || '', scope, title: scope,
      budget: [n + ' ' + agree(n, 'capture', 'captures'), ran,
        session.duration ? ocDurationText(session.duration) : 'manual stop'].filter(Boolean).join(' · '),
      live: left,
      actions: `<button type="button" class="oc-icon-btn" data-oc-copy="capture" data-oc-id="${escapeHtml(session.id || '')}" aria-label="copy this capture's scope into a new capture" title="copy this scope into a new capture">copy</button>` +
        `<button type="button" class="oc-icon-btn danger" data-oc-stop="${escapeHtml(session.id || '')}">stop</button>`,
    });
  });
  // Two ways this sentence was wrong: capture_body_preview stores a bounded
  // prompt/response preview even with no session running, and captures from
  // earlier sessions stay readable until their retention window ends - the
  // stop-all dialog says exactly that.
  const empty = ocSectionEmpty('capture', sessions.length, 'No captures running.',
    'Nothing new is being recorded. Earlier captures stay readable until their retention window ends.');
  return ocSectionHTML({
    kind: 'capture',
    sub: 'Store request and response bodies for a scoped, time-bounded debug session.',
    actions: `<button type="button" class="btn" data-oc-stop-all ${sessions.length ? '' : 'disabled'}>stop all</button>` + headAction,
    composer: composing ? ocCaptureComposerHTML(ocDrafts.capture) : '',
    entries, total: sessions.length, empty,
  });
}

function ocCaptureBody(draft) {
  // A capture matches by client, provider and model. It has no key or
  // conversation predicate, so the composer offers only those three rather
  // than showing a control the server would ignore.
  const body = {
    enabled: true,
    clients: draft.scope.clients,
    providers: draft.scope.providers,
    models: draft.scope.models,
    duration: draft.duration || '',
  };
  if (draft.id) body.id = draft.id;
  return body;
}

function ocApplyCapture() {
  const draft = ocDrafts.capture;
  if (!draft) return;
  if (!ocScopeHas(draft.scope)) {
    draft.error = 'pick at least one condition';
    ocRender();
    return;
  }
  draft.error = '';
  const body = ocCaptureBody(draft);
  const previous = draft.id ? liveDebugSessions().find(s => s.id === draft.id) || null : null;
  const phrase = (draft.id ? 'updated capture on ' : 'capturing ') + ocScopePhrase(draft.scope);
  mutateOperator('debug', body, st => {
    // The debug route returns created_id for a create, so the session this
    // write installed is known exactly. Inferring it from a before/after set
    // difference picked whichever session appeared first, which is another
    // operator's session when two starts land together, and then the undo
    // closed the wrong capture.
    const createdId = st && st.created_id ? String(st.created_id) : '';
    const created = createdId ? (liveDebugSessions().find(s => s.id === createdId) || null) : null;
    const undo = created
      ? () => mutateOperator('debug', { enabled: false, id: created.id })
      : previous ? () => mutateOperator('debug', ocCaptureBody(ocCaptureDraftFromSession(previous))) : null;
    ocDrafts.capture = ocBlankCaptureDraft();
    ocView = { kind: '', id: '' };
    ocRecordChange(phrase, undo, 'ok');
    ocRender();
  });
}

async function ocStopSession(id) {
  const session = liveDebugSessions().find(s => s.id === id);
  if (!session) { ocRuleGone('capture'); return; }
  const phrase = 'stopped capture on ' + ocScopePhrase(ocScopeFromPolicy({ scope: session }));
  mutateOperator('debug', { enabled: false, id }, () => {
    ocRecordChange(phrase, () => { const draft = ocCaptureDraftFromSession(session); draft.id = ''; return mutateOperator('debug', ocCaptureBody(draft)); }, 'ok');
    ocRender();
  });
}

async function ocStopAllSessions() {
  const sessions = liveDebugSessions();
  if (!sessions.length) return;
  const ok = await ocConfirm({
    title: 'Stop every capture?',
    body: sessions.length + ' ' + agree(sessions.length, 'session is', 'sessions are') + ' storing request and response bodies. Retained captures stay readable until their retention window ends.',
    confirm: 'stop all',
    danger: true,
  });
  if (!ok) return;
  mutateOperator('debug', { enabled: false }, () => {
    const restore = sessions.slice();
    ocRecordChange('stopped every capture', async () => {
      let all = true;
      for (const session of restore) {
        const draft = ocCaptureDraftFromSession(session);
        draft.id = '';
        if (await mutateOperator('debug', ocCaptureBody(draft)) === false) all = false;
      }
      return all;
    }, 'ok');
    ocRender();
  });
}

