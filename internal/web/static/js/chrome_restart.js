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
    btn.textContent = active ? 'restarting…' : 'restart now';
    btn.setAttribute('aria-busy', String(active));
  }
  // The console's system section owns the action, so a build that cannot
  // restart says why in place instead of hiding a header control.
  if (typeof refreshOperatorReadout === 'function') refreshOperatorReadout();
}

// applyRestartState folds the bootstrap's boot-time eligibility into the
// restart state, so a page that never opens the system section still knows
// whether to offer the action. The verdict rides the bootstrap the page
// already loads: no extra round trip, and a payload without the section (or
// without a boolean available) leaves the current behaviour alone. The reason
// is adopted with the verdict because the system section names it in place.
function applyRestartState(st) {
  if (!st || typeof st.available !== 'boolean') return;
  restartServerState = {
    ...restartServerState,
    available: st.available,
    reason: typeof st.reason === 'string' ? st.reason : '',
  };
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

// The poll fallback's cadence. There is deliberately no budget constant here:
// the status document publishes budget_ms, the whole choreography bound, from
// the binary that enforces its parts (restartBuildTimeout, restartFlushTimeout,
// restartReadyTimeout and the configured restart_drain_timeout). Mirroring
// those as literals put the same durations in two languages, where the
// copy could silently disagree with the value the server actually applied.
//
// budget_ms 0 means the operator configured restart_drain_timeout: 0, which
// the server documents as "wait indefinitely". An unbounded drain has no
// deadline: the watch stream, a reported failure and the fresh process's own
// started_at are the completion evidence. Substituting a fallback for that
// case made this client announce a timeout during a healthy handoff.
//
// Poll cadence while the choreography runs; each poll is raced against the
// hang bound because drain-phase polls sit unanswered in the inherited
// socket's backlog.
const RESTART_POLL_INTERVAL_MS = 750;
const RESTART_POLL_HANG_MS = 5000;
// renderRestartSteps paints the step list from a status document
// {rank, phase, drain_elapsed_ms, drain_timeout_ms, error}. Steps below rank
// are done, the ranked one is active (with the live drain timer), the rest
// pending; an error marks the active step failed.
// RESTART_STEP_WORDS is the state word each step carries in text. The dot is
// colour alone, so without this a screen reader heard four identical labels and
// a colour-blind operator saw four identical rows: a failed build read exactly
// like a finished one. The word is visually hidden because the dot already
// carries it for sighted users, but it is part of the step's accessible text.
const RESTART_STEP_WORDS = {done: 'done', active: 'now', failed: 'failed', pending: 'pending'};

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
      sub = `<span class="rs-sub">${fmtDur(Math.round((st.drain_elapsed_ms || 0) / TIME_MS.second) * TIME_MS.second)} / ${fmtDur(st.drain_timeout_ms)}</span>`;
    }
    // aria-current marks the running step for assistive technology; the word
    // states it for everyone else. Both come from the same state so they
    // cannot disagree.
    const current = state === 'active' ? ' aria-current="step"' : '';
    return `<li class="rs-step ${state}"${current}><span class="rs-dot" aria-hidden="true"></span>` +
      `<span class="rs-label">${s.label}</span>` +
      `<span class="visually-hidden"> - ${RESTART_STEP_WORDS[state]}</span>${sub}</li>`;
  }).join('');
  // One announcement per transition, not one per repaint: the poll fires every
  // 750ms with the same rank, and setText suppresses the repeats.
  const ranked = RESTART_STEPS[Math.min(st.rank, RESTART_STEPS.length - 1)];
  setText($('restart-announce'),
    fail ? 'restart failed at ' + ranked.label
      : st.rank >= RESTART_STEPS.length ? 'restart complete'
        : 'restart step: ' + ranked.label);
}

function setRestartStatus(text, err) {
  const el = $('restart-count');
  if (el) {
    // setText: this is a status region and the poll repaints it with the same
    // sentence on every tick until the phase changes.
    setText(el, text || '');
    if (err) el.dataset.err = '1'; else delete el.dataset.err;
    return;
  }
  // The system section is closed. A failure still has to reach the operator,
  // so it takes the one toast path; routine progress stays silent.
  if (err && text) toast(text, 'err');
}

function fetchRestartStatus() {
  return operatorFetch('/admin/restart')
    .then(r => r.json())
    .then(st => {
      applyRestartEvent(st);
      if (st && st.available === false) {
        setRestartStatus(st.reason || 'restart unavailable', true);
      }
      // The count line and the action repaint in place, but the refusal
      // sentence belongs to the system section's markup. Patch just that
      // paragraph: a full section repaint would schedule another status fetch
      // from the render path and chase its own tail.
      if (typeof ocSyncRestartVerdict === 'function') ocSyncRestartVerdict();
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
  const ok = await ocConfirm({
    title: 'Rebuild and restart?',
    body: 'Build the latest source, finish in-flight streams, and save metrics before the new process takes over. New requests queue during the handoff.',
    confirm: 'restart now',
  });
  if (!ok) return;
  const beforeFeed = feedId;
  const watcher = new AbortController();
  // Lock before the first await: rapid clicks must not start two preflights.
  restartBusy = true;
  syncRestartControls();
  try {
    const before = await fetchRestartStatus();
    if (before.available === false || before.phase !== 'idle') {
      if (before.available === false) setRestartStatus(before.reason || 'restart unavailable', true);
      // A restart already running is a refusal too. Returning silently after
      // the operator confirmed made a real no-op look like a missed click, and
      // two tabs restarting at once is the ordinary way to reach this branch.
      else setRestartStatus('a restart is already running', true);
      return;
    }
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
    const budgetMs = Number(doc.budget_ms);
    // Infinity for a server-declared unbounded drain: the loop below then
    // exits only on real evidence (a new started_at, a failure, or an abort
    // through restartFailed), which is exactly what the server promised.
    const deadline = Number.isFinite(budgetMs) && budgetMs > 0
      ? Date.now() + budgetMs
      : Infinity;
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
      // The ledger is the one announcement owner; it toasts only when the
      // console is closed and the ledger cannot be heard.
      if (typeof ocRecordChange === 'function') ocRecordChange('restarted the proxy', null, 'ok');
      // SSE normally detects the new feed first. If it is disconnected,
      // check fresh assets/state immediately through the same bootstrap gate.
      if (feedId === beforeFeed) fetchBootstrap('resume');
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
