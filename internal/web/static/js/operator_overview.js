// Operator overview: the in-force register, the queue drain control and the
// session history. Every row is derived from the same live state the readout
// and the section lists use, so the overview can never disagree with them.

// ---------- overview ----------
function ocAllRuleRows() {
  const rows = [];
  for (const hold of liveHolds()) {
    rows.push({
      kind: 'pause',
      section: 'pause',
      id: hold.id || '',
      scope: ocHoldScopePhrase(hold),
      detail: [ocUntilPhrase(hold.until),
        hold.queued ? ocPopulation(hold.queued, OC_POPULATIONS.queued) : '',
        hold.reason || ''].filter(Boolean).join(' · '),
    });
  }
  for (const session of liveDebugSessions()) {
    const n = typeof session.captures === 'number' ? session.captures : 0;
    rows.push({
      kind: 'capture',
      section: 'capture',
      id: session.id || '',
      scope: ocScopePhrase({ providers: session.providers, models: session.models, clients: session.clients }),
      detail: [n + ' ' + agree(n, 'capture', 'captures'), ocCaptureWindowPhrase(session)].filter(Boolean).join(' · '),
    });
  }
  for (const policy of ((typeof throttleState !== 'undefined' && throttleState.throttles) || [])) {
    // The overview is the in-force register, and an off-schedule policy is
    // installed but enforcing nothing. Listing it as an ordinary row made the
    // same rule read as a live cap here, as "off schedule" in its section and
    // as `off schedule N` in the readout: three surfaces, two answers.
    const offSchedule = ocLimitOffSchedule(policy);
    rows.push({
      kind: 'limit',
      section: 'limits',
      id: policy.id || '',
      offSchedule,
      scope: ocPolicyScopePhrase(policy),
      detail: [ocBudgetPhrase(policy), policy.schedule ? ocSchedulePhrase(policy.schedule) : '',
        offSchedule ? OC_OFF_SCHEDULE : ''].filter(Boolean).join(' · '),
    });
  }
  return rows;
}

// ocDrainValueHTML is the drain's value field. It is one input with the known
// names as suggestions, not a picker plus a field: a drain may name a client
// that has not sent traffic since the restart, so the value must stay free,
// and retyping a name the console already knows was the same friction the
// scope pickers were built to remove.
// OC_DRAIN_SCOPE is the three dimensions scheduler.DrainScope can match, in
// the singular spelling the chips render. The list is the server's contract,
// not a client preference, so it stays one declaration and the plural lookup
// comes from the scope table rather than a second map.
const OC_DRAIN_SCOPE = ['provider', 'model', 'client'];

function ocDrainValueHTML() {
  const dim = ocDrain.scope;
  const listId = 'oc-drain-values';
  const known = typeof ocKnownValues === 'function'
    ? ocKnownValues(ocScopeDim(dim)?.key || dim) : [];
  const options = known.map(value =>
    `<option value="${escapeHtml(value)}"></option>`).join('');
  return `<input type="text" data-oc-drain-value value="${escapeHtml(ocDrain.value)}"` +
    ` placeholder="${escapeHtml(dim)}" aria-label="drain one ${escapeHtml(dim)}"` +
    ` list="${listId}" autocomplete="off" data-oc-key="drain-value">` +
    `<datalist id="${listId}">${options}</datalist>`;
}

function ocRenderOverview() {
  const rows = ocAllRuleRows();
  const parked = parkedCounts();
  const queued = parked.drainable;
  const quotaProviders = typeof quotaPausedProviders === 'function' ? quotaPausedProviders() : [];
  const list = rows.length ? rows.map(row =>
    `<button type="button" class="oc-ov-row" data-oc-section="${row.section}" data-oc-target="${escapeHtml(row.id)}" data-oc-key="ov-${row.kind}-${escapeHtml(row.scope)}">` +
    `<span class="oc-ov-kind" data-kind="${row.kind}"${ocOffScheduleMark(row.offSchedule)}>${row.kind}</span>` +
    `<span class="oc-ov-scope">${escapeHtml(row.scope)}</span>` +
    `<span class="oc-ov-detail">${escapeHtml(row.detail)}</span></button>`).join('')
    : ocEmptyHTML('Nothing is in force.',
      "No limit caps these requests, no hold parks them, no capture records them. The admission queue and each provider's own rate limits still apply.");
  const restart = restartInProgress() ? `<div class="oc-banner">Restart in progress. <button type="button" class="oc-link" data-oc-section="system">view steps</button></div>` : '';
  // The counts live in the head line and on the section chips, so the body
  // opens with the rules themselves. The queue appears only when something is
  // actually parked: an empty queue is not a task.
  //
  // The storm/quota gate parks before admission, so its waiters are not
  // drainable: a drain would answer "nothing released" for work it cannot
  // reach. They are named in their own note, with the actions that do release
  // them, so the operator is not offered a button that cannot help.
  const queue = queued ? `<div class="oc-group"><div class="oc-label">queue</div>` +
    `<p class="oc-note">${fmt(queued)} parked. Draining releases them with a retryable response; running streams finish.</p>` +
    `<div class="oc-inline">${ocChipRow(OC_CHOICES.drainScope, ['all'].concat(OC_DRAIN_SCOPE), ocDrain.scope, v => v === 'all' ? 'everything' : 'one ' + v)}` +
    (ocDrain.scope !== 'all' ? ocDrainValueHTML() : '') +
    `<button type="button" class="btn" data-oc-drain>drain</button></div></div>` : '';
  const gated = parked.gated ? `<div class="oc-group" data-oc-gate><div class="oc-label">gate</div>` +
    `<p class="oc-note">${fmt(parked.gated)} waiting on error storm or quota protection. They are parked before admission, so a drain cannot release them: an error storm opens on its own once the provider recovers.` +
    (quotaProviders.length ? ' A quota pause can also be resumed now.' : '') + ` ` +
    `<button type="button" class="oc-link" data-oc-section="limits">view limits</button></p>` +
    // A quota gate outlives the incident banner's display switch, so the
    // console offers the resume itself instead of pointing at a banner that
    // may be turned off.
    (quotaProviders.length ? `<div class="oc-inline">` + quotaProviders.map(provider =>
      `<button type="button" class="btn" data-oc-quota-resume="${escapeHtml(provider)}">resume ${escapeHtml(provider)}</button>`).join('') +
      `</div>` : '') + `</div>` : '';
  return `<div class="oc-sec">` +
    restart +
    (rows.length ? `<div class="oc-label">in force</div>` : '') +
    `<div class="oc-ov-list">${list}</div>` +
    queue +
    gated +
    `</div>`;
}

async function ocDrainQueue() {
  // Only the drainable population is a precondition: a gate-only park has no
  // drain to perform, and the overview does not offer one.
  const queued = parkedCounts().drainable;
  if (!queued) return;
  let body;
  if (ocDrain.scope === 'all') body = { all: true };
  else {
    const value = (ocDrain.value || '').trim();
    if (!value) { toast('enter a ' + ocDrain.scope + ' to drain', 'err'); return; }
    body = { [ocDrain.scope]: value };
  }
  const ok = await ocConfirm({
    title: 'Drain the queue?',
    body: (body.all ? 'Every parked request' : 'Parked requests matching ' + ocDrain.scope + ' ' + ocDrain.value) +
      ' will be released with a retryable response. Running streams finish normally.',
    confirm: 'drain queue',
    danger: true,
  });
  if (!ok) return;
  try {
    await operatorJsonBody('/admin/queue/drain', {
      method: 'POST', headers: JSON_HEADERS, body: JSON.stringify(body),
    }, 'could not drain the queue');
    const phrase = 'drained ' + (body.all ? 'the queue' : ocDrain.scope + ' ' + ocDrain.value);
    ocRecordChange(phrase, null, 'ok');
    fetchBootstrap('none');
    ocRender();
  } catch (err) {
    toast(String(err.message || 'could not drain the queue'), 'err');
  }
}

