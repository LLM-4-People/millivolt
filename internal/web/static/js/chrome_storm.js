// Storm banner, incident identity and details dialog. The server owns storm
// detection and recovery; this module only renders an accepted status and
// opens the shared modal detail frame. Loaded eagerly before the operator
// modules.

// Storm detection and recovery belong to the server. Accepted bootstrap
// snapshots replace this bounded status surface and its open detail dialog.
// Incident buttons retain their identity across updates for keyboard focus.
let stormSnapshot = {valid: false, enabled: false, storms: []};
let stormDialogKey = '';
let quotaResumeError = '';
const stormIncidentKey = s => JSON.stringify([s.provider, s.scope, s.model]);
function applyStormState(st) {
  const banner = $('storm-banner');
  if (!banner) return;
  const valid = s => s && typeof s.provider === 'string' && s.provider.length > 0 &&
    typeof s.model === 'string' && (s.scope === 'provider' && !s.model || s.scope === 'model') &&
    ['open', 'half_open'].includes(s.state) && typeof s.reason === 'string' &&
    typeof s.quota === 'boolean' && (!s.quota || s.scope === 'provider') &&
    Number.isFinite(s.error_percent) && s.error_percent >= 0 && s.error_percent <= 100 &&
    (s.scope !== 'provider' || Number.isSafeInteger(s.active_models) && s.active_models >= 0 &&
      Number.isSafeInteger(s.affected_models) && s.affected_models >= 0 && s.affected_models <= s.active_models &&
      Number.isFinite(s.affected_model_percent) && s.affected_model_percent >= 0 && s.affected_model_percent <= 100) &&
    ['error_requests', 'failures', 'samples', 'window_ms', 'queued', 'recovery_successes', 'recovery_required'].every(key =>
      Number.isSafeInteger(s[key]) && s[key] >= 0) && s.failures <= s.samples && s.window_ms > 0 &&
    s.error_requests <= s.failures &&
    s.recovery_required > 0 && s.recovery_successes <= s.recovery_required &&
    typeof s.retry_at === 'string' && Number.isFinite(Date.parse(s.retry_at));
  const keys = new Set();
  const accepted = typeof st?.enabled === 'boolean' && typeof st.banner_enabled === 'boolean' &&
    Array.isArray(st.storms) && st.storms.every(s => {
      if (!valid(s)) return false;
      const key = stormIncidentKey(s);
      if (keys.has(key)) return false;
      keys.add(key);
      return true;
    });
  stormSnapshot = {valid: accepted, enabled: accepted && st.enabled, storms: accepted ? st.storms : []};
  const storms = accepted && st.enabled && st.banner_enabled ? st.storms : [];
  const existing = new Map([...banner.children].map(row => [row.dataset.value, row]));
  storms.forEach((s, index) => {
    const key = stormIncidentKey(s);
    const scope = s.provider + ' · ' + (s.scope === 'provider' ? 'all models' : s.model || '(unspecified model)');
    const detail = s.quota
      ? `<span class="storm-reason">${escapeHtml(s.reason)}</span><span>Quota pause</span><span>${fmt(s.queued)} queued</span><span>${fmt(s.recovery_successes)} / ${fmt(s.recovery_required)} recovery probes</span>`
      : `<span class="storm-reason">${escapeHtml(s.reason)}</span><span>${escapeHtml(pctValue(s.error_percent, 1, true))} failed attempts</span><span>${fmt(s.error_requests)} requests affected</span><span>${fmt(s.queued)} queued</span>`;
    const html = `<span class="storm-heading"><strong>${escapeHtml(scope)}</strong><span class="storm-state">${s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'}</span><span class="storm-open-hint">View details ›</span></span><span class="storm-detail">${detail}</span>`;
    let row = existing.get(key);
    existing.delete(key);
    if (!row) {
      row = document.createElement('button');
      row.type = 'button';
      row.className = 'storm-row';
      row.dataset.operator = 'storm-open';
      row.dataset.value = key;
      row.setAttribute('aria-haspopup', 'dialog');
      row.setAttribute('aria-controls', 'storm-dialog');
    }
    if (row.stormHTML !== html) { row.innerHTML = html; row.stormHTML = html; }
    if (banner.children[index] !== row) banner.insertBefore(row, banner.children[index] || null);
  });
  for (const row of existing.values()) row.remove();
  banner.hidden = storms.length === 0;
  renderStormDetails();
}

// buildModalDialog is the one modal-frame constructor: every overlay
// dialog builds the same inert-while-hidden, focusable section and
// appends it to the body exactly once.
function buildModalDialog(id, className, labelledBy, innerHTML) {
  const dialog = document.createElement('section');
  dialog.id = id;
  dialog.className = className;
  dialog.hidden = true;
  dialog.tabIndex = -1;
  dialog.setAttribute('inert', '');
  dialog.setAttribute('role', 'dialog');
  dialog.setAttribute('aria-modal', 'true');
  dialog.setAttribute('aria-labelledby', labelledBy);
  dialog.innerHTML = innerHTML;
  document.body.appendChild(dialog);
  return dialog;
}

// wireDialogDismiss attaches the two dismiss paths every modal frame
// shares: a click on the frame itself and the Escape key. The dismiss
// callback owns what closing means for its dialog.
function wireDialogDismiss(dialog, dismiss) {
  dialog.addEventListener('click', e => { if (e.target === dialog) dismiss(e); });
  dialog.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); dismiss(e); }
  });
}

function openStormDetails(key) {
  if (!stormSnapshot.enabled || !stormSnapshot.storms.some(s => stormIncidentKey(s) === key)) return;
  let dialog = $('storm-dialog');
  if (!dialog) {
    dialog = buildModalDialog('storm-dialog', 'storm-dialog', 'storm-dialog-title', `<div class="storm-dialog-panel"><div class="drawer-hd"><h3 id="storm-dialog-title">Error storm details</h3><button class="btn" type="button" data-operator="storm-close" aria-label="Close error storm details">Close</button></div><div class="storm-dialog-body" id="storm-dialog-body"></div></div>`);
    wireDialogDismiss(dialog, () => closeStormDetails());
  }
  closeHeaderMenus();
  closeNavMenu();
  closeDimMenu();
  stormDialogKey = key;
  quotaResumeError = '';
  renderStormDetails();
  openModal(dialog);
}

function closeStormDetails() {
  const dialog = $('storm-dialog');
  if (!dialog || dialog.hidden) return;
  const returnTo = modalReturnFocus;
  closeModal(dialog);
  stormDialogKey = '';
  if (!returnTo?.isConnected || returnTo.closest('[hidden], [inert]')) {
    const banner = $('storm-banner');
    (banner && !banner.hidden ? banner : visibleHeaderControl())?.focus();
  }
}

function renderStormDetails() {
  if (!stormDialogKey || !$('storm-dialog-body')) return;
  const title = $('storm-dialog-title');
  const incident = stormSnapshot.storms.find(s => stormIncidentKey(s) === stormDialogKey);
  if (!stormSnapshot.valid || !stormSnapshot.enabled || !incident) {
    const message = !stormSnapshot.valid ? 'Current incident details are unavailable.' :
      !stormSnapshot.enabled ? 'Error storm protection is disabled.' : 'This incident is no longer active.';
    if (title) title.textContent = 'Error storm details';
    updateSection('storm-dialog-body', `<p class="storm-resolution" role="status">${message}</p>`);
    return;
  }
  const s = incident;
  if (title) title.textContent = s.quota ? 'Provider quota pause' : 'Error storm details';
  const entries = s.quota ? [
    ['Provider', s.provider], ['Model scope', 'All models'],
    ['State', s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'], ['Error', s.reason],
    ['Queued requests', fmt(s.queued)], ['Next retry', new Date(s.retry_at).toLocaleString()],
    ['Successful recovery probes', s.recovery_successes + ' / ' + s.recovery_required],
  ] : [
    ['Provider', s.provider], ['Model scope', s.scope === 'provider' ? 'All models' : s.model || '(unspecified model)'],
    ['State', s.state === 'half_open' ? 'Checking recovery' : 'Holding requests'], ['Error', s.reason],
    ['Requests affected', fmt(s.error_requests)], ['Failed upstream attempts', fmt(s.failures)],
    ['Sampled upstream attempts', fmt(s.samples)], ['Upstream attempt failure rate', pctValue(s.error_percent, 1, true)],
    ['Detection window', fmtDur(s.window_ms)], ['Queued requests', fmt(s.queued)],
    ['Next retry', new Date(s.retry_at).toLocaleString()],
    ['Successful recovery probes', s.recovery_successes + ' / ' + s.recovery_required],
  ];
  if (!s.quota && s.scope === 'provider') entries.push(['Active models', fmt(s.active_models)],
    ['Affected active models', fmt(s.affected_models) + ' / ' + fmt(s.active_models) + ' (' + pctValue(s.affected_model_percent, 1, true) + ')']);
  const rows = entries.map(([label, value]) => `<div><dt>${escapeHtml(label)}</dt><dd>${escapeHtml(value)}</dd></div>`).join('');
  const explanation = s.quota
    ? 'This provider is paused after a durable quota/billing error (for example insufficient credits). Every new request parks here and one recovery probe is admitted per cooldown - a provider Retry-After on the quota response paces it like any other 429. The first successful probe releases the queue; Resume now closes the gate immediately.'
    : 'Counts cover the rolling detection window. Each request with a selected failure in this window is counted once; retries count as separate upstream attempts. Recovery checks use queued requests, so the next retry may start after the scheduled time.';
  const controls = s.quota
    ? `<div class="storm-quota-actions"><button class="btn" type="button" data-operator="quota-resume" data-value="${escapeHtml(s.provider)}">Resume now</button>${quotaResumeError ? `<span class="storm-resolution" role="alert">${escapeHtml(quotaResumeError)}</span>` : ''}</div>` : '';
  updateSection('storm-dialog-body', `<dl class="storm-facts">${rows}</dl><p class="storm-explanation">${explanation}</p>${controls}`);
}

// resumeQuotaProvider is the retry-mode quota gate's operator action: the
// confirmed response is the shared storm snapshot, so the banner and dialog
// repaint from the same authoritative state; a failure keeps the dialog open
// with the message beside the button.
async function resumeQuotaProvider(provider) {
  if (!provider) return;
  try {
    const st = await operatorJsonBody('/admin/quota', {
      method: 'POST', headers: JSON_HEADERS, body: JSON.stringify({provider, resume: true}),
    }, 'could not resume ' + provider);
    if (!st || !Array.isArray(st.storms)) throw new Error('unexpected resume response');
    quotaResumeError = '';
    applyStormState(st);
  } catch (err) {
    quotaResumeError = String(err.message || 'resume failed');
  }
  renderStormDetails();
}
