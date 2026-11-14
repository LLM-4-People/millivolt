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
      scope: hold.all ? 'all traffic' : hold.new ? 'new clients'
        : ocScopePhrase({ providers: hold.providers, clients: hold.clients }),
      detail: [ocUntilPhrase(hold.until), hold.queued ? hold.queued + ' queued' : '', hold.reason || ''].filter(Boolean).join(' · '),
    });
  }
  for (const session of liveDebugSessions()) {
    const n = typeof session.captures === 'number' ? session.captures : 0;
    rows.push({
      kind: 'capture',
      section: 'capture',
      scope: ocScopePhrase({ providers: session.providers, models: session.models, clients: session.clients }),
      detail: [n + (n === 1 ? ' capture' : ' captures'), ocUntilPhrase(session.until) || session.duration || 'until stopped'].filter(Boolean).join(' · '),
    });
  }
  for (const policy of ((typeof throttleState !== 'undefined' && throttleState.throttles) || [])) {
    rows.push({
      kind: 'limit',
      section: 'limits',
      scope: ocScopePhrase({
        providers: policy.scope?.providers, models: policy.scope?.models,
        clients: policy.scope?.clients, keyHashes: policy.scope?.key_hashes,
        conversations: policy.scope?.conversations,
      }),
      detail: ocBudgetPhrase(policy) + (policy.schedule ? ' · ' + ocSchedulePhrase(policy.schedule) : ''),
    });
  }
  return rows;
}

function ocRenderOverview() {
  const rows = ocAllRuleRows();
  const queued = Number(pauseState.queued) || 0;
  const list = rows.length ? rows.map(row =>
    `<button type="button" class="oc-ov-row" data-oc-section="${row.section}" data-oc-key="ov-${row.kind}-${escapeHtml(row.scope)}">` +
    `<span class="oc-ov-kind" data-kind="${row.kind}">${row.kind === 'capture' ? 'capture' : row.kind}</span>` +
    `<span class="oc-ov-scope">${escapeHtml(row.scope)}</span>` +
    `<span class="oc-ov-detail">${escapeHtml(row.detail)}</span></button>`).join('')
    : `<div class="oc-empty"><strong>Nothing is in force.</strong> Requests follow provider behavior, queue settings and the absence of a cap. Add a limit, hold or capture when you need one.</div>`;
  const recent = ocChanges.length
    ? ocChanges.slice().reverse().map(change =>
      `<li><span class="oc-ov-scope">${escapeHtml(change.phrase)}</span><span class="oc-ov-detail">${escapeHtml(new Date(change.at).toLocaleTimeString())}</span></li>`).join('')
    : `<li class="oc-empty-line">changes this session appear here</li>`;
  const restart = restartInProgress() ? `<div class="oc-banner">Restart in progress. <button type="button" class="oc-link" data-oc-section="system">view steps</button></div>` : '';
  return `<div class="oc-sec">` +
    restart +
    `<div class="oc-kpis">` +
    `<div class="oc-kpi"><span class="oc-kpi-label">rules</span><span class="oc-kpi-value">${fmt(rows.length)}</span></div>` +
    `<div class="oc-kpi"><span class="oc-kpi-label">holds</span><span class="oc-kpi-value">${fmt(ocRuleCounts().holds)}</span></div>` +
    `<div class="oc-kpi"><span class="oc-kpi-label">limits</span><span class="oc-kpi-value">${fmt(ocRuleCounts().limits)}</span></div>` +
    `<div class="oc-kpi"><span class="oc-kpi-label">queued</span><span class="oc-kpi-value">${fmt(queued)}</span></div>` +
    `</div>` +
    `<div class="oc-group"><div class="oc-label">in force</div><div class="oc-ov-list">${list}</div></div>` +
    `<div class="oc-group"><div class="oc-label">queue</div>` +
    `<p class="oc-note">${queued ? fmt(queued) + ' requests are parked. Draining releases them with a retryable response; running streams finish.' : 'The queue is empty.'}</p>` +
    `<div class="oc-inline">${ocChipRow('drain-scope', ['all', 'provider', 'model', 'client'], ocDrain.scope, v => v === 'all' ? 'everything' : 'one ' + v)}` +
    (ocDrain.scope !== 'all' ? `<input type="text" data-oc-drain-value value="${escapeHtml(ocDrain.value)}" placeholder="${escapeHtml(ocDrain.scope)}" aria-label="drain ${escapeHtml(ocDrain.scope)}" data-oc-key="drain-value">` : '') +
    `<button type="button" class="btn" data-oc-drain ${queued > 0 ? '' : 'disabled'}>drain</button></div></div>` +
    `<div class="oc-group"><div class="oc-label">this session</div><ol class="oc-history">${recent}</ol></div>` +
    `</div>`;
}

async function ocDrainQueue() {
  const queued = Number(pauseState.queued) || 0;
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
    toast(phrase);
    fetchBootstrap('none');
    ocRender();
  } catch (err) {
    toast(String(err.message || 'could not drain the queue'), 'err');
  }
}

