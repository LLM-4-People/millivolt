// Theme picker: the dashboard tray, its preview states and its keyboard
// lifecycle. THEME_REGISTRY in js/theme_boot.js owns the palette registry;
// js/theme_state.js owns normalization, persistence and the one apply path.
// Palette tokens live in css/themes.css.

// syncThemeMenu restores both panes to the committed pair. It never rebuilds
// the option DOM, so scroll, focus, and a hover preview stay stable. A commit
// or a fresh open also resets the roving tab stop to the committed rows; a
// mere focus leave keeps the keyboard cursor's tab stop so Tab returns to the
// row the user was previewing.
function syncThemeMenu(resetCursor = true) {
  const menu = $(HEADER_MENU_IDS.themeMenu);
  if (!menu) return;
  const theme = currentTheme();
  const mode = currentMode();
  for (const option of menu.querySelectorAll('.theme-opt')) {
    const checked = option.dataset.themeId === theme;
    option.setAttribute('aria-checked', String(checked));
    if (resetCursor) option.tabIndex = checked ? 0 : -1;
    option.classList.remove('is-preview');
  }
  for (const option of menu.querySelectorAll('.theme-mode-opt')) {
    const checked = option.dataset.modeId === mode;
    option.setAttribute('aria-checked', String(checked));
    if (resetCursor) option.tabIndex = checked ? 0 : -1;
    option.classList.remove('is-preview');
  }
  for (const swatch of menu.querySelectorAll('.theme-swatch')) {
    swatch.dataset.mode = mode;
  }
  const currentPane = $('theme-current');
  if (currentPane) {
    currentPane.dataset.theme = theme;
    currentPane.dataset.mode = mode;
  }
  const previewPane = $('theme-preview');
  if (previewPane) {
    previewPane.dataset.theme = theme;
    previewPane.dataset.mode = mode;
  }
  setText($('theme-current-label'), `Current: ${themeLabel(theme)} / ${modeLabel(mode)}`);
  const previewLabel = $('theme-preview-label');
  setText(previewLabel, 'Preview: same');
  if (previewLabel) previewLabel.classList.remove('has-preview');
  const button = $(HEADER_MENU_IDS.themeTrigger);
  if (button) {
    button.setAttribute('aria-label', `Theme: ${themeLabel(theme)}, ${modeLabel(mode)}`);
  }
  // A recovered storage backend must restore the truthful saved label when
  // the stored pair already matches. A failed write leaves the pair in memory
  // only, so storedPairMatches stays false and the Not saved label survives
  // syncThemeMenu until an actual write succeeds.
  if (storedPairMatches(theme, mode)) {
    setText($('theme-saved'), 'Saved in this browser');
  }
  // Every reset of the panes also resets the polite candidate line. The menu
  // can be closed or focus can sit outside both groups by now.
  syncCandidateStatus();
}

// previewTheme shows a candidate in the preview pane and captions. The
// current pane, the root, storage, the chart and the browser theme color stay
// on the committed pair; only a commit moves them.
function previewTheme(theme, mode) {
  const menu = $(HEADER_MENU_IDS.themeMenu);
  if (!menu || menu.hidden) return;
  const candidateTheme = normalizeTheme(theme);
  const candidateMode = normalizeMode(mode);
  const previewPane = $('theme-preview');
  if (previewPane) {
    previewPane.dataset.theme = candidateTheme;
    previewPane.dataset.mode = candidateMode;
  }
  for (const option of menu.querySelectorAll('.theme-opt')) {
    option.classList.toggle('is-preview',
      option.dataset.themeId === candidateTheme && candidateTheme !== currentTheme());
  }
  for (const option of menu.querySelectorAll('.theme-mode-opt')) {
    option.classList.toggle('is-preview',
      option.dataset.modeId === candidateMode && candidateMode !== currentMode());
  }
  const changed = candidateTheme !== currentTheme() || candidateMode !== currentMode();
  const previewLabel = $('theme-preview-label');
  setText(previewLabel, changed
    ? `Preview: ${themeLabel(candidateTheme)} / ${modeLabel(candidateMode)}`
    : 'Preview: same');
  // The muted default is the hierarchy: a real candidate lifts the line to
  // primary ink, "same" does not compete with the committed caption.
  if (previewLabel) previewLabel.classList.toggle('has-preview', changed);
}

// announceCandidate writes the focused option's candidate to the polite live
// region. Only keyboard focus reaches this writer: hover and pointer-induced
// focus preview silently, so the live region never narrates a mouse action.
// setText keeps an unchanged status from being re-announced.
function announceCandidate(theme, mode) {
  const candidateTheme = normalizeTheme(theme);
  const candidateMode = normalizeMode(mode);
  const changed = candidateTheme !== currentTheme() || candidateMode !== currentMode();
  setText($('theme-status'), changed
    ? `Preview: ${themeLabel(candidateTheme)} / ${modeLabel(candidateMode)}`
    : `Current: ${themeLabel(candidateTheme)} / ${modeLabel(candidateMode)}`);
}

// syncCandidateStatus keeps the live region aligned with the committed pair.
// After a commit, reset or programmatic activation the panes show the
// committed pair again, so only a focused option whose candidate is that pair
// may announce; a focused row showing some other candidate would contradict
// the pane, and focus outside both groups clears the line.
function syncCandidateStatus() {
  const focused = focusedPreviewOption();
  const status = $('theme-status');
  if (!focused) {
    setText(status, '');
    return;
  }
  const theme = focused.dataset.modeId ? currentTheme() : focused.dataset.themeId;
  const mode = focused.dataset.modeId || currentMode();
  if (theme === currentTheme() && mode === currentMode()) announceCandidate(theme, mode);
  else setText(status, '');
}

// previewOption restates an option's candidate through the one preview path,
// so a focused option can reclaim the preview pane after a hover elsewhere
// ends.
function previewOption(option) {
  if (!option) return;
  if (option.dataset.modeId) previewTheme(currentTheme(), option.dataset.modeId);
  else if (option.dataset.themeId) previewTheme(option.dataset.themeId, currentMode());
}

function focusedPreviewOption() {
  const menu = $(HEADER_MENU_IDS.themeMenu);
  const active = document.activeElement;
  if (!menu || menu.hidden || !active || !menu.contains(active)) return null;
  return active.closest?.('.theme-opt, .theme-mode-opt') || null;
}

// clearThemePreview restores the committed pair only when no preview control
// still owns keyboard focus. Mouse leave over another option must not discard
// the focused candidate. Focus moving between tray options keeps that option's
// preview; focus leaving the tray or landing on a non-preview control clears it.
function clearThemePreview(event) {
  const menu = $(HEADER_MENU_IDS.themeMenu);
  if (!menu) return;
  if (event && event.type === 'blur') {
    const next = event.relatedTarget;
    if (next && next.closest && menu.contains(next) &&
        next.closest('.theme-opt, .theme-mode-opt')) return;
  }
  const focused = focusedPreviewOption();
  if (focused) {
    previewOption(focused);
    return;
  }
  // Keep the keyboard cursor's tab stop: Tab back into the tray should return
  // to the preview row the user chose, even though the panes reset.
  syncThemeMenu(false);
}

// A pointer press focuses the option before the click commits it. That focus
// must not write the keyboard candidate live region: only a keyboard focus
// announces a preview. The flag is consumed by the focus it caused.
let pointerOption = null;

// wirePreviewOption is the one preview/commit wiring both radio families
// share: hover previews, focus previews and announces, pointer focus previews
// silently, and leave/blur restore the committed pair.
function wirePreviewOption(option, preview) {
  option.onpointerdown = () => { pointerOption = option; };
  option.onpointerup = () => { pointerOption = null; };
  option.onpointercancel = () => { pointerOption = null; };
  option.onfocus = () => {
    const fromPointer = pointerOption === option;
    pointerOption = null;
    const candidate = preview();
    if (!fromPointer) announceCandidate(candidate.theme, candidate.mode);
  };
  option.onmouseenter = () => { preview(); };
  option.onmouseleave = clearThemePreview;
  option.onblur = clearThemePreview;
}

// renderModeMenu and renderThemeMenu build their controls once. Hover and
// keyboard focus preview; click or native Enter/Space commits and leaves the
// tray open.
function renderModeMenu() {
  const list = $('theme-mode');
  if (!list) return;
  list.textContent = '';
  for (const mode of MODES) {
    const option = document.createElement('button');
    option.type = 'button';
    option.className = 'theme-mode-opt';
    option.dataset.modeId = mode.id;
    option.setAttribute('role', 'radio');
    option.setAttribute('aria-checked', 'false');
    option.tabIndex = -1;
    option.textContent = mode.label;

    const check = document.createElement('span');
    check.className = 'theme-check';
    check.textContent = '✓';
    check.setAttribute('aria-hidden', 'true');
    option.append(check);

    option.onclick = () => chooseMode(mode.id);
    wirePreviewOption(option, () => {
      const candidate = { theme: currentTheme(), mode: mode.id };
      previewTheme(candidate.theme, candidate.mode);
      return candidate;
    });
    list.append(option);
  }
  wireThemeKeys(list);
}

function renderThemeMenu() {
  const list = $('theme-list');
  if (!list) return;
  list.textContent = '';
  for (const theme of THEMES) {
    const option = document.createElement('button');
    option.type = 'button';
    option.className = 'theme-opt';
    option.dataset.themeId = theme.id;
    option.setAttribute('role', 'radio');
    option.setAttribute('aria-checked', 'false');
    option.tabIndex = -1;
    option.onclick = () => chooseTheme(theme.id);
    wirePreviewOption(option, () => {
      const candidate = { theme: theme.id, mode: currentMode() };
      previewTheme(candidate.theme, candidate.mode);
      return candidate;
    });

    const swatch = document.createElement('span');
    swatch.className = 'theme-swatch';
    swatch.dataset.theme = theme.id;
    swatch.dataset.mode = currentMode();
    swatch.setAttribute('aria-hidden', 'true');

    const copy = document.createElement('span');
    copy.className = 'theme-copy';
    const name = document.createElement('span');
    name.className = 'theme-name';
    name.textContent = theme.label;
    const hint = document.createElement('span');
    hint.className = 'theme-hint';
    hint.textContent = theme.hint;
    copy.append(name, hint);

    const check = document.createElement('span');
    check.className = 'theme-check';
    check.textContent = '✓';
    check.setAttribute('aria-hidden', 'true');

    option.append(swatch, copy, check);
    list.append(option);
  }
  wireThemeKeys(list);
}

// moveRovingFocus keeps one tab stop in a radio group: the row the keyboard
// cursor moves to becomes tabbable, so Tab out and back returns to the live
// preview row instead of the committed row.
function moveRovingFocus(options, next) {
  const target = options[next];
  if (!target) return;
  options.forEach((option, index) => { option.tabIndex = index === next ? 0 : -1; });
  target.focus();
}

// One roving-radio key handler per group. The arrow/Home/End keys move the
// keyboard cursor and preview candidate only; Enter or Space on the focused
// button is the deliberate commit, through its normal activation. The guard
// makes a re-render idempotent instead of stacking a second listener.
function wireThemeKeys(list) {
  if (list.dataset.themeKeysWired === '1') return;
  list.dataset.themeKeysWired = '1';
  list.addEventListener('keydown', event => {
    // Alt/Ctrl/Meta + arrow are browser or OS navigation, not radio
    // selection. Leave default behavior and the committed pair alone.
    if (keyModified(event)) return;
    const options = [...list.querySelectorAll('[role="radio"]')];
    const index = options.indexOf(event.target);
    if (index < 0) return;
    let next = index;
    if (event.key === 'ArrowDown' || event.key === 'ArrowRight') next = (index + 1) % options.length;
    else if (event.key === 'ArrowUp' || event.key === 'ArrowLeft') next = (index - 1 + options.length) % options.length;
    else if (event.key === 'Home') next = 0;
    else if (event.key === 'End') next = options.length - 1;
    else return;
    event.preventDefault();
    // Focus owns the preview and its announcement; activation stays on the
    // focused option's native button behavior.
    moveRovingFocus(options, next);
  });
}

function chooseTheme(id) {
  applyState(id, currentMode(), true);
}

function chooseMode(id) {
  applyState(currentTheme(), id, true);
}

function resetTheme() {
  applyState(DEFAULT_THEME, DEFAULT_MODE, true);
  $('theme-reset')?.focus();
}

function closeThemeMenu() {
  // hideHdrMenu runs the registered close cleanup, which restores the panes
  // and clears the candidate line before focus returns to the trigger.
  hideHdrMenu(HEADER_MENU_IDS.themeMenu);
  $(HEADER_MENU_IDS.themeTrigger)?.focus();
}

// themeTriggerKey gives the closed trigger the usual disclosure entry: open
// and focus the saved palette. While the tray is already open, the same keys
// move the keyboard cursor through the palette without committing it.
function themeTriggerKey(e) {
  // Modified arrows belong to the browser or OS; they must not open the tray,
  // move the cursor, or cancel the native action.
  if (keyModified(e)) return;
  if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
  e.preventDefault();
  const menu = $(HEADER_MENU_IDS.themeMenu);
  if (!menu) return;
  if (menu.hidden) {
    toggleThemeMenu({ stopPropagation() {}, detail: 0 });
    return;
  }
  const options = [...menu.querySelectorAll('.theme-opt')];
  if (!options.length) return;
  const checked = Math.max(0, options.findIndex(option => option.getAttribute('aria-checked') === 'true'));
  const direction = e.key === 'ArrowDown' ? 1 : -1;
  moveRovingFocus(options, (checked + direction + options.length) % options.length);
}

function toggleThemeMenu(e) {
  toggleHdrMenu(e, HEADER_MENU_IDS.themeMenu, syncThemeMenu);
  const menu = $(HEADER_MENU_IDS.themeMenu);
  if (!menu || menu.hidden) return;
  // Reopening must not restore an arbitrary previous scroll offset. Bring the
  // saved palette into view instead; on compact viewports the tray or header
  // flyout scrolls to satisfy it. Keyboard opens also focus that row.
  const checked = menu.querySelector('.theme-opt[aria-checked="true"]');
  if (checked) {
    if (e && e.detail === 0) checked.focus({ preventScroll: true });
    if (typeof checked.scrollIntoView === 'function') {
      checked.scrollIntoView({ block: 'nearest' });
    }
  }
}

function initTheme() {
  const root = document.documentElement;
  const theme = normalizeTheme(root.dataset.theme);
  const mode = normalizeMode(root.dataset.mode);
  const changed = root.dataset.theme !== theme || root.dataset.mode !== mode;
  root.dataset.theme = theme;
  root.dataset.mode = mode;
  // A valid stored theme can arrive with an invalid sibling mode: the inline
  // guard keeps the valid half and leaves the default mode. Rewrite the pair
  // whenever storage does not already match the normalized values, not only
  // when the document attributes changed.
  if (changed || !storedPairMatches(theme, mode)) persistTheme();
  // The theme owns transient candidate state, so it registers the cleanup the
  // shared header-menu close path (Escape, outside click, Close) runs. If
  // focus was inside the tray it must not stay on a hidden control.
  if (typeof onHeaderMenuClose === 'function') {
    onHeaderMenuClose(HEADER_MENU_IDS.themeMenu, () => {
      const menu = $(HEADER_MENU_IDS.themeMenu);
      const focusWasInside = !!menu && menu.contains(document.activeElement);
      clearThemePreview();
      if (focusWasInside) $(HEADER_MENU_IDS.themeTrigger)?.focus();
    });
  }
  renderModeMenu();
  renderThemeMenu();
  syncThemeColor();
  syncThemeMenu();
  if (changed) {
    if (typeof refreshChartColors === 'function') refreshChartColors();
    if (typeof resetChart === 'function') resetChart();
    if (typeof renderChart === 'function') renderChart();
  }
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initTheme);
} else {
  initTheme();
}
