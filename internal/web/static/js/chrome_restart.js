// Restart choreography: the rebuild-and-handoff trigger, the status poll, the
// watch stream and the step tracker. Loaded eagerly last among the chrome
// modules.

// ---- Restart (rebuild & graceful handoff) ----
// POST /admin/restart rebuilds from source and hands the listening socket to
// a fresh process. In-flight streams finish on the old process; new requests
// queue in the kernel backlog until the child serves - nothing is refused.
// The client polls GET /admin/restart until `started_at` changes, which can
// only come from the fresh process.

let restartBusy = false;
let restartFailed = false;
let restartServerState = {};

function restartInProgress() {
  return restartBusy || !!(restartServerState.phase && restartServerState.phase !== 'idle');
}

// The local lock covers preflight/POST/polling; server state also covers a
// restart started in another tab. Status refreshes cannot unlock a local run.
function syncRestartControls() {
  const active = restartInProgress();
  const unavailable = restartServerState.available === false;
  const btn = $('btn-restart-now');
  if (btn) {
    btn.disabled = active || unavailable;
    btn.textContent = active ? 'Restarting…' : 'Restart now';
    btn.setAttribute('aria-busy', String(active));
  }
  const trigger = $('btn-restart');
  if (trigger) {
    // Keep the menu accessible so progress can be reopened during a restart.
    if (active) trigger.dataset.busy = '1'; else delete trigger.dataset.busy;
    trigger.title = active ? 'Restarting - view progress' : 'Restart';
    trigger.setAttribute('aria-label', trigger.title);
  }
  // A binary that cannot restart is not offered at all: the whole control and
  // its menu leave the row (the wrap is the unit - the menu means nothing
  // without its trigger, and an empty wrap would hold the row's gap open). A
  // run in progress keeps it reachable so progress can still be reopened.
  // Hiding the control also hides the eligibility REASON: nothing tells the
  // operator why the row lost its Restart entry, because the one surface that
  // could (the menu's status line, fed by fetchRestartStatus) is itself
  // unreachable once the trigger is gone.
  const wrap = $('restart-wrap');
  if (wrap) wrap.hidden = unavailable && !active;
}

// applyRestartState folds the bootstrap's boot-time eligibility into the
// restart state and re-syncs the controls, so a page that never opens the menu
// still knows whether to OFFER the action. The verdict rides the bootstrap the
// page already loads: no extra round trip, and a payload without the section
// (or without a boolean available) leaves the current behaviour alone. Only the
// verdict is adopted. The accompanying reason is deliberately NOT: the control
// is hidden exactly when the verdict is false, so there is nowhere to render it.
// The server's own refusal (POST /admin/restart) still carries the same string
// for a caller of the API.
function applyRestartState(st) {
  if (!st || typeof st.available !== 'boolean') return;
  restartServerState = { ...restartServerState, available: st.available };
  syncRestartControls();
}

// RESTART_STEPS is the visible choreography. `rank` matches the server's
// phaseRank: a reported phase marks every step before it done, itself active
// - the list is rendered the moment each step actually starts (the watch
// stream), never pre-claimed.
const RESTART_STEPS = [
  { rank: 0, label: 'rebuild from source' },
  { rank: 1, label: 'finish in-flight streams' },
  { rank: 2, label: 'flush metrics to disk' },
  { rank: 3, label: 'start the new process' },
];

// The poll fallback's budget and cadence. The deadline must cover the whole
// remaining choreography or the fallback gives up while the server is still
// draining: the server bounds the build (restartBuildTimeout, 5 minutes) and
// the child's ready wait (restartReadyTimeout, 5 minutes) on its side, and
// the status document carries the drain bound as drain_timeout_ms (the
// configured restart_drain_timeout; 0 = the server waits indefinitely).
// RESTART_DRAIN_FALLBACK_MS mirrors the server-side default drain timeout
// (internal/config Default: RestartDrainTimeout, 10 minutes) for documents
// that omit the bound or report an unbounded drain.
const RESTART_BUILD_BUDGET_MS = 300000;
const RESTART_READY_BUDGET_MS = 300000;
const RESTART_DRAIN_FALLBACK_MS = 600000;
// Poll cadence while the choreography runs; each poll is raced against the
// hang bound because drain-phase polls sit unanswered in the inherited
// socket's backlog.
const RESTART_POLL_INTERVAL_MS = 750;
const RESTART_POLL_HANG_MS = 5000;
// How long the success notice stays up before the menu closes.
const RESTART_DONE_MS = 2500;

// renderRestartSteps paints the step list from a status document
// {rank, phase, drain_elapsed_ms, drain_timeout_ms, error}. Steps below rank
// are done, the ranked one is active (with the live drain timer), the rest
// pending; an error marks the active step failed.
function renderRestartSteps(st) {
  const ol = $('restart-steps');
  if (!ol) return;
  if (!st || st.rank == null || st.rank < 0) { ol.hidden = true; ol.innerHTML = ''; return; }
  ol.hidden = false;
  const fail = !!(st.error);
  ol.innerHTML = RESTART_STEPS.map(s => {
    const state = s.rank < st.rank ? 'done' : (s.rank === st.rank ? (fail ? 'failed' : 'active') : 'pending');
    let sub = '';
    if (state === 'active' && s.rank === 1 && st.drain_timeout_ms > 0) {
      sub = `<span class="rs-sub">${fmtDur(Math.round((st.drain_elapsed_ms || 0) / 1000) * 1000)} / ${fmtDur(st.drain_timeout_ms)}</span>`;
    }
    return `<li class="rs-step ${state}"><span class="rs-dot" aria-hidden="true"></span><span class="rs-label">${s.label}</span>${sub}</li>`;
  }).join('');
}

function toggleRestartMenu(e) {
  toggleHdrMenu(e, 'restart-menu', null, fetchRestartStatus);
}

function setRestartStatus(text, err) {
  const el = $('restart-count');
  if (!el) return;
  el.textContent = text || '';
  if (err) el.dataset.err = '1'; else delete el.dataset.err;
}

function fetchRestartStatus() {
  return operatorFetch('/admin/restart')
    .then(r => r.json())
    .then(st => {
      applyRestartEvent(st);
      if (st && st.available === false) {
        setRestartStatus(st.reason || 'restart unavailable', true);
      }
      return st || {};
    })
    .catch(() => { setRestartStatus('could not check restart status', true); return {}; });
}

// watchRestart opens the NDJSON step stream (GET /admin/restart?watch=1) and
// calls onEvent per status document. It resolves true when the stream was
// established (hijacked server-side, so it survives the drain that hangs
// ordinary polls); false when streaming is unsupported - the caller then
// falls back to polling. The stream ends when the old process exits (success)
// or the registry closes it (failure) - the caller detects both by the
// reader finishing.
async function watchRestart(onEvent, signal) {
  let reader;
  try {
    const r = await operatorFetch('/admin/restart?watch=1', {signal});
    if (!r.ok || !r.body || !r.body.getReader) return false;
    const dec = new TextDecoder();
    let buf = '';
    reader = r.body.getReader();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) return true;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line) continue;
        try { onEvent(JSON.parse(line)); } catch (e) { /* malformed line: skip */ }
      }
    }
  } catch (e) {
    return false;
  } finally {
    if (reader) await reader.cancel().catch(() => {});
  }
}

async function restartProxy() {
  if (restartInProgress()) return;
  const beforeFeed = feedId;
  const watcher = new AbortController();
  // Lock before the first await: rapid clicks must not start two preflights.
  restartBusy = true;
  syncRestartControls();
  try {
    const before = await fetchRestartStatus();
    if (before.available === false || before.phase !== 'idle') return;
    if (!confirm('Rebuild from source and restart the proxy?\n\nIn-flight streams finish; new requests queue until the fresh process takes over.')) return;
    restartFailed = false;
    setRestartStatus('');
    renderRestartSteps(null);
    // Open the step stream before the POST: after the commit point the old
    // process stops accepting, so polls hang in the backlog - the hijacked
    // watch connection is already accepted and streams every real transition.
    // Apply each NDJSON line as it arrives (do not wait for the stream to
    // end - that hid draining behind "building" for the whole drain).
    watchRestart(st => { if (!watcher.signal.aborted) applyRestartEvent(st); }, watcher.signal);
    let doc;
    try {
      doc = await operatorJsonBody('/admin/restart', { method: 'POST' }, r => 'restart failed (' + r.status + ')');
    } catch (err) {
      // The server answered with a refusal: show its message as the status
      // line, exactly as every other restart status reads. Transport
      // failures rethrow and take the generic prefix in the outer catch.
      if (!err || err.operatorBody !== true) throw err;
      setRestartStatus(err.message, true);
      return;
    }
    if (restartFailed) return;
    applyRestartEvent(doc);
    // A fast failed build can already be idle in the POST response, before
    // this client has observed its active phase on the watch connection.
    if (doc.phase === 'idle' && doc.error) return;
    // Poll in parallel with the watch: streaming drives the step list live
    // (including drain); polls detect the fresh process via started_at and
    // are the fallback when the client cannot stream. Drain polls hang, so
    // each is raced against the hang bound. The deadline derives from the
    // status document's drain_timeout_ms plus the server's own build and
    // ready budgets, so a long drain never reads as a timeout.
    const drainMs = Number(doc.drain_timeout_ms) > 0 ? Number(doc.drain_timeout_ms) : RESTART_DRAIN_FALLBACK_MS;
    const deadline = Date.now() + RESTART_BUILD_BUDGET_MS + drainMs + RESTART_READY_BUDGET_MS;
    const pollDone = (async () => {
      while (Date.now() < deadline) {
        await new Promise(res => setTimeout(res, RESTART_POLL_INTERVAL_MS));
        if (restartFailed) return null;
        const poll = new AbortController();
        const timeout = setTimeout(() => poll.abort(), RESTART_POLL_HANG_MS);
        const st = await operatorFetch('/admin/restart', {signal: poll.signal}).then(x => x.json()).catch(() => null).finally(() => clearTimeout(timeout));
        if (restartFailed) return null;
        if (st && st.started_at && before.started_at && st.started_at !== before.started_at) {
          return st;
        }
        if (st && st.phase) applyRestartEvent(st);
      }
      return null;
    })();
    const st = await pollDone;
    if (restartFailed) return;
    if (st && st.started_at) {
      applyRestartEvent(st);
      renderRestartSteps({ rank: RESTART_STEPS.length, error: '' });
      setRestartStatus('restarted ✓ (pid ' + st.pid + ')');
      // SSE normally detects the new feed first. If it is disconnected,
      // check fresh assets/state immediately through the same bootstrap gate.
      if (feedId === beforeFeed) fetchBootstrap('resume');
      setTimeout(() => { hideHdrMenu('restart-menu'); setRestartStatus(''); const ol = $('restart-steps'); if (ol) { ol.hidden = true; ol.innerHTML = ''; } }, RESTART_DONE_MS);
      return;
    }
    setRestartStatus('restart did not complete in time', true);
  } catch (err) {
    setRestartStatus('restart request failed - ' + err.message, true);
  } finally {
    watcher.abort();
    restartBusy = false;
    syncRestartControls();
  }
}

// applyRestartEvent folds one status document into the step list. An idle
// phase with an error is a failed run (the watch stream just ended); idle
// without error during the wait is the fresh child answering - completion
// is handled by started_at, never by phase. Phase names live on the
// checklist, never the yellow count line.
function applyRestartEvent(st) {
  if (!st) return;
  const wasActive = restartServerState.phase && restartServerState.phase !== 'idle';
  restartServerState = { ...restartServerState, ...st };
  syncRestartControls();
  if (!st.phase) return;
  if (st.phase === 'idle') {
    if (st.error) {
      // The watch's initial idle snapshot may carry a previous build error.
      // Only an active → idle transition can fail the run now being watched.
      if (wasActive) restartFailed = true;
      renderRestartSteps(st);
      setRestartStatus('restart failed - ' + st.error, true);
    } else if (!restartBusy) {
      renderRestartSteps(null);
      setRestartStatus('');
    }
    return;
  }
  setRestartStatus('');
  renderRestartSteps(st);
}
