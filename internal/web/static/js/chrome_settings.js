// Settings shell and lazy loader: the header trigger, the one-shot versioned
// SETTINGS_MODULES loader, the sheet open-state probe, the shared status line,
// the close entry point, the artifact download owners and the debug surface.
// The sheet itself lives in the lazy settings modules.

// ---------- settings shell and lazy module ----------
// chrome_settings.js keeps only the Settings shell: the header trigger, the one-shot
// versioned loader, the sheet's open-state probe, the shared status line and
// the single close entry point. The sheet's state, rendering, editors,
// apply/revert and backup/restore/purge controls live in the settings
// modules named by SETTINGS_MODULES and load on the first open. Until they
// arrive, close is a DOM-only no-op and Escape cannot touch a draft that
// does not exist.
//
// SETTINGS_MODULES is the one owner list of the sheet's deferred classic
// scripts, in execution order: the entry (state + shell) first, the editor
// modules next and the collection/apply/delegation wiring last. The loader
// appends them one at a time, so no module runs before the module it was
// split from, and index.html never references one (first paint stays free
// of the whole sheet). A new settings module joins this list only.
const SETTINGS_MODULES = [
  'settings.js',
  'settings_backup.js',
  'settings_providers.js',
  'settings_model_rules.js',
  'settings_overrides.js',
  'settings_subconversations.js',
  'settings_providers_actions.js',
  'settings_state.js',
];
let settingsLoadPromise = null;
let settingsLoading = false;
let settingsWantOpen = false;

// settingsExpanded is the one writer of the opener's open state. It says
// aria-expanded, matching the operator console trigger beside it: this button
// has aria-haspopup="dialog" and controls one sheet, so it is an expander, not
// a toggle. The attribute was aria-pressed, which ARIA treats as contradictory
// with a popup role, and three call sites spelled the write by hand.
function settingsExpanded(open) {
  const b = $('btn-settings');
  if (b) b.setAttribute('aria-expanded', open ? 'true' : 'false');
}

// settingsFields is the one reader of the settings fields container. The id was
// spelled at fifteen sites across the modules loaded below, so a rename had to
// reach every one of them or a module would quietly operate on nothing. It lives
// in this shell because the shell loads eagerly and every deferred module can
// call it; the last-loaded module cannot own a name the earlier ones use.
const settingsFields = () => $('settings-fields');

function settingsIsOpen() {
  const s = $('settings-sheet');
  return !!(s && !s.hidden);
}

function settingsStatus(text, kind) {
  const el = $('settings-count');
  if (!el) return;
  // setText, not an assignment: this region is role=status, and marking a key
  // dirty calls this on every keystroke with the same "N unsaved" sentence.
  setText(el, text || '');
  el.classList.toggle('ok', kind === 'ok');
  if (kind === 'error') el.dataset.tone = 'error';
  else delete el.dataset.tone;
}

// The settings-load failure copy, shared by the lazy loader and the fetched
// form builder (both render the same unavailable-config state).
const SETTINGS_LOAD_FAIL = 'could not load config';

function settingsModuleURL(name) {
  const version = document.querySelector('meta[name="dashboard-version"]')?.content || '';
  return '/dash/js/' + name + '?v=' + encodeURIComponent(version);
}

// loadSettingsModule is the single-flight sequential loader: concurrent or
// repeated opens share one promise and one pass over SETTINGS_MODULES for the
// page's lifetime, and the promise resolves only after the last module's
// onload registered the sheet owners. from is the list index to start at: the
// production trigger always starts at 0, while a page that included the entry
// directly (the jsdom fixture) resumes after it instead of injecting it twice.
function loadSettingsModule(from = 0) {
  if (window.settingsModule) return Promise.resolve(window.settingsModule);
  if (settingsLoadPromise) return settingsLoadPromise;
  settingsLoadPromise = new Promise((resolve, reject) => {
    let at = from;
    const append = () => {
      if (at >= SETTINGS_MODULES.length) {
        const module = window.settingsModule;
        if (module && typeof module.open === 'function' && typeof module.close === 'function') resolve(module);
        else reject(new Error(SETTINGS_LOAD_FAIL));
        return;
      }
      const script = document.createElement('script');
      script.src = settingsModuleURL(SETTINGS_MODULES[at++]);
      script.async = true;
      // A page may register the sheet module itself (the loader tests do) or
      // a module may register before the list ends: adopt it and stop, exactly
      // as the one-module loader did. The split's last module owns the real
      // registration, so a normal load walks the whole list.
      script.onload = () => {
        const module = window.settingsModule;
        if (module && typeof module.open === 'function' && typeof module.close === 'function') resolve(module);
        else append();
      };
      script.onerror = () => reject(new Error(SETTINGS_LOAD_FAIL));
      document.head.appendChild(script);
    };
    append();
  }).catch(err => {
    settingsStatus(SETTINGS_LOAD_FAIL, 'error');
    toast(SETTINGS_LOAD_FAIL, 'err');
    reportProblem('settings module failed to load', err);
    throw err;
  });
  return settingsLoadPromise;
}

// closeSettings is the one close entry point every surface calls. Before the
// module loads there is no draft state to guard, so the shell hides the
// sheet/veil directly; after load it delegates to the real owner.
function closeSettings(force) {
  settingsWantOpen = false;
  const module = window.settingsModule;
  if (module && typeof module.close === 'function') { module.close(force); return; }
  const s = $('settings-sheet');
  const v = $('settings-veil');
  if (s) { s.classList.remove('open'); closeModal(s); }
  if (v) v.classList.remove('open');
  settingsExpanded(false);
}

// cancelSettingsOpen consumes an Escape aimed at Settings: it closes an open
// sheet through the real owner, or clears a pending open while the lazy modules
// are still loading. The second case matters because the sheet stays hidden
// until the module resolves; without it the loader's want-open check opened the
// sheet after the keypress.
function cancelSettingsOpen() {
  if (settingsIsOpen()) { closeSettings(); return true; }
  if (!settingsWantOpen) return false;
  settingsWantOpen = false;
  return true;
}

// settingsCatRequest is the one-shot category the next Settings open lands
// on. A console warning names one setting; without this the operator reaches
// the sheet's first pane and has to find the field again.
let settingsCatRequest = '';

function settingsApplyCatRequest() {
  const cat = settingsCatRequest;
  if (!cat) return false;
  const rail = $('settings-rail');
  const btn = rail && [...rail.querySelectorAll('.st-rail-item[data-st-cat]')]
    .find(b => b.dataset.stCat === cat);
  if (!btn) return false;
  settingsCatRequest = '';
  btn.click();
  return true;
}

// openSettingsAt is the one entry point for "open Settings showing this
// category". It sets the request, then opens through the real owner; the
// fill path applies the request once the rail exists.
function openSettingsAt(category) {
  settingsCatRequest = category || '';
  if (settingsIsOpen()) { settingsApplyCatRequest(); return; }
  toggleSettings();
}

// toggleSettings loads the module once, then opens the sheet through its
// owner. A load failure leaves the sheet closed and surfaces the shared
// status copy plus a console error.
function toggleSettings(e) {
  if (e) e.stopPropagation();
  if (settingsIsOpen()) { closeSettings(); return; }
  settingsWantOpen = true;
  const module = window.settingsModule;
  if (module) { module.open(); return; }
  if (settingsLoading) return;
  settingsLoading = true;
  loadSettingsModule().then(mod => {
    settingsLoading = false;
    // A close, Escape or another header menu while the module was loading
    // cancels the pending open; reopening while loading keeps the intent
    // true.
    if (settingsWantOpen && !settingsIsOpen()) mod.open();
  }, () => {
    settingsLoading = false;
    settingsWantOpen = false;
    // A failed load must be retryable: the rejected promise is not a
    // permanent state.
    settingsLoadPromise = null;
    settingsStatus(SETTINGS_LOAD_FAIL, 'error');
  });
}

// triggerDownload is the one anchor-click download path (log exports,
// backups and debug captures): one owner for the download attribute
// contract - an empty string defers the filename to the server's
// Content-Disposition - and for revoking a passed object URL once the click
// landed.
function triggerDownload(href, download, revoke) {
  const a = document.createElement('a');
  a.href = href;
  a.download = download;
  document.body.appendChild(a);
  a.click();
  a.remove();
  if (revoke) URL.revokeObjectURL(href);
}

// downloadArtifact is the one fetch-blob-filename-click owner for operator
// artifact downloads (the settings backup and the debug capture): through
// the operator gate, blobbed, named from the server's Content-Disposition
// with the caller's fallback, then handed to triggerDownload. It resolves
// the blob (callers report sizes) and rejects through operatorErrorBody
// with the caller's designed failure message.
async function downloadArtifact(url, fallback, failed) {
  const r = await operatorFetch(url);
  if (!r.ok) return operatorErrorBody(r, failed);
  const blob = await r.blob();
  const dispo = r.headers.get('Content-Disposition') || '';
  const m = /filename="([^"]+)"/.exec(dispo);
  triggerDownload(URL.createObjectURL(blob), (m && m[1]) || fallback, true);
  return blob;
}


function applyDebugState(st, revision = operatorState.debug.revision) {
  if (!acceptOperator('debug', st, revision) || !applyModelCanon(st.model_canon)) return;
  const sig = JSON.stringify(st);
  if (sig === _debugStateSig) return;
  _debugStateSig = sig;
  debugState = {
    enabled: !!st.enabled,
    sessions: Array.isArray(st.sessions) ? st.sessions : [],
    [KNOWN_CLIENTS_KEY]: Array.isArray(st[KNOWN_CLIENTS_KEY]) ? st[KNOWN_CLIENTS_KEY] : [],
    [KNOWN_PROVIDERS_KEY]: Array.isArray(st[KNOWN_PROVIDERS_KEY]) ? st[KNOWN_PROVIDERS_KEY] : [],
    [KNOWN_MODELS_KEY]: Array.isArray(st[KNOWN_MODELS_KEY]) ? st[KNOWN_MODELS_KEY] : [],
    until: st.until || null,
    ttl: st.ttl || '',
    max_bytes: st.max_bytes == null || st.max_bytes === '' ? '' : String(st.max_bytes),
  };
  refreshFooterState();
  resyncOperatorSurfaces('debug');
}

// toggleHdrMenu is the one header-menu toggle branch: stop the event,
// close every other header menu, run the caller's pre-open and post-open
// syncs around the hidden flip, and publish the shared expanded state.
// The named wrappers stay as the onclick surface every button names.
function toggleHdrMenu(e, menuId, prepare, after) {
  e.stopPropagation();
  const m = $(menuId);
  if (m.hidden) {
    closeHeaderMenus(menuId);
    if (prepare) prepare();
    m.hidden = false;
    if (after) after();
  } else {
    hideHdrMenu(menuId);
  }
  syncHdrMenuExpanded();
}

function holdLive(h) {
  if (!h) return false;
  const ms = untilRemainingMs(h.until);
  return ms === null || ms > 0;
}

function liveHolds() {
  return (pauseState.holds || []).filter(holdLive);
}

function pauseActive() {
  return liveHolds().length > 0;
}

// NONE_YET_NOTE is the one empty-state sentence for a known-name picker: the
// console's scope pickers render it when nothing is known yet. JS owns the
// text; the stylesheet only paints its muted look (.oc-pick-empty).
const NONE_YET_NOTE = 'none yet - they appear as requests arrive';

function recordById(id) {
  if (!id) return undefined;
  const recs = lastData?.records || [];
  const i = _recIdx.get(id);
  if (i !== undefined && recs[i]?.id === id) return recs[i];
  // A full replacement renders the drawer before _rebuildRecIdx runs; fall
  // back to a scan rather than drop the open drawer for one frame.
  return recs.find(r => r && r.id === id) || logArchive.find(r => r && r.id === id);
}
