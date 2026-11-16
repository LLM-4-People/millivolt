// Site theme host: renders the shared palette registry into the header tray and
// binds preview, commit, reset and dismissal. Palette ids, labels and defaults
// come from THEME_REGISTRY (theme_boot.js); normalization and persistence come
// from theme_state.js. This file owns only the site tray's DOM and anchoring.
(function () {
  const menu = document.getElementById('theme-menu');
  const trigger = document.getElementById('btn-theme');
  if (!menu || !trigger || typeof THEME_REGISTRY === 'undefined') return;

  const modeGroup = document.getElementById('theme-mode');
  const themeGroup = document.getElementById('theme-list');
  const currentPane = document.getElementById('theme-current');
  const previewPane = document.getElementById('theme-preview');
  const currentLabel = document.getElementById('theme-current-label');
  const previewLabel = document.getElementById('theme-preview-label');
  const hint = document.getElementById('theme-hint');

  function option(kind, item) {
    const button = document.createElement('button');
    button.type = 'button';
    button.className = kind === 'mode' ? 'theme-mode-opt' : 'theme-opt';
    button.dataset[kind === 'mode' ? 'modeId' : 'themeId'] = item.id;
    button.setAttribute('role', 'radio');
    button.setAttribute('aria-checked', 'false');
    button.tabIndex = -1;

    if (kind === 'mode') {
      button.textContent = item.label;
      const check = document.createElement('span');
      check.className = 'theme-check';
      check.textContent = '✓';
      check.setAttribute('aria-hidden', 'true');
      button.append(check);
      button.addEventListener('click', () => commit(currentTheme(), item.id));
      button.addEventListener('mouseenter', () => preview(currentTheme(), item.id));
      button.addEventListener('focus', () => preview(currentTheme(), item.id));
    } else {
      const swatch = document.createElement('span');
      swatch.className = 'theme-swatch';
      swatch.dataset.theme = item.id;
      swatch.dataset.mode = currentMode();
      swatch.setAttribute('aria-hidden', 'true');

      const copy = document.createElement('span');
      copy.className = 'theme-copy';
      const name = document.createElement('span');
      name.className = 'theme-name';
      name.textContent = item.label;
      const note = document.createElement('span');
      note.className = 'theme-note';
      note.textContent = item.hint;
      copy.append(name, note);

      const check = document.createElement('span');
      check.className = 'theme-check';
      check.textContent = '✓';
      check.setAttribute('aria-hidden', 'true');

      button.append(swatch, copy, check);
      button.addEventListener('click', () => commit(item.id, currentMode()));
      button.addEventListener('mouseenter', () => preview(item.id, currentMode()));
      button.addEventListener('focus', () => preview(item.id, currentMode()));
    }
    button.addEventListener('mouseleave', clearPreview);
    button.addEventListener('blur', clearPreview);
    return button;
  }

  function render() {
    modeGroup.textContent = '';
    for (const mode of THEME_REGISTRY.modes) modeGroup.append(option('mode', mode));
    themeGroup.textContent = '';
    for (const theme of THEME_REGISTRY.themes) themeGroup.append(option('theme', theme));
    wireKeys(modeGroup);
    wireKeys(themeGroup);
  }

  function preview(theme, mode) {
    const nextTheme = normalizeTheme(theme);
    const nextMode = normalizeMode(mode);
    previewPane.dataset.theme = nextTheme;
    previewPane.dataset.mode = nextMode;
    const changed = nextTheme !== currentTheme() || nextMode !== currentMode();
    previewLabel.textContent = changed
      ? `Preview: ${themeLabel(nextTheme)} / ${modeLabel(nextMode)}`
      : 'Preview: same';
    previewLabel.classList.toggle('has-preview', changed);
    const item = THEME_REGISTRY.themes.find(t => t.id === nextTheme);
    if (hint) hint.textContent = item ? item.hint : '';
    for (const row of themeGroup.querySelectorAll('.theme-opt')) {
      row.classList.toggle('is-preview', changed && row.dataset.themeId === nextTheme);
    }
    for (const row of modeGroup.querySelectorAll('.theme-mode-opt')) {
      row.classList.toggle('is-preview', changed && row.dataset.modeId === nextMode);
    }
  }

  function clearPreview() {
    preview(currentTheme(), currentMode());
  }

  // The shared state module calls this after any commit.
  window.syncThemeMenu = function () {
    if (!menu) return;
    const theme = currentTheme();
    const mode = currentMode();
    for (const row of themeGroup.querySelectorAll('.theme-opt')) {
      const checked = row.dataset.themeId === theme;
      row.setAttribute('aria-checked', String(checked));
      row.tabIndex = checked ? 0 : -1;
      row.classList.remove('is-preview');
    }
    for (const row of modeGroup.querySelectorAll('.theme-mode-opt')) {
      const checked = row.dataset.modeId === mode;
      row.setAttribute('aria-checked', String(checked));
      row.tabIndex = checked ? 0 : -1;
      row.classList.remove('is-preview');
    }
    for (const swatch of themeGroup.querySelectorAll('.theme-swatch')) swatch.dataset.mode = mode;
    currentPane.dataset.theme = theme;
    currentPane.dataset.mode = mode;
    currentLabel.textContent = `Current: ${themeLabel(theme)} / ${modeLabel(mode)}`;
    previewLabel.textContent = 'Preview: same';
    previewLabel.classList.remove('has-preview');
    const current = THEME_REGISTRY.themes.find(t => t.id === theme);
    if (hint) hint.textContent = current ? current.hint : '';
    trigger.setAttribute('aria-label', `Theme: ${themeLabel(theme)}, ${modeLabel(mode)}`);
    if (storedPairMatches(theme, mode)) {
      document.getElementById('theme-saved').textContent = 'Saved in this browser';
    }
  };

  function commit(theme, mode) {
    applyState(theme, mode, true);
    clearPreview();
  }

  function open() {
    menu.hidden = false;
    trigger.setAttribute('aria-expanded', 'true');
    const checked = themeGroup.querySelector('.theme-opt[aria-checked="true"]');
    if (checked) checked.focus({ preventScroll: true });
  }

  function close(returnFocus) {
    if (menu.hidden) return;
    menu.hidden = true;
    trigger.setAttribute('aria-expanded', 'false');
    clearPreview();
    if (returnFocus) trigger.focus();
  }

  function wireKeys(group) {
    if (group.dataset.keysWired === '1') return;
    group.dataset.keysWired = '1';
    group.addEventListener('keydown', event => {
      if (event.altKey || event.ctrlKey || event.metaKey) return;
      const options = [...group.querySelectorAll('[role="radio"]')];
      const index = options.indexOf(event.target);
      if (index < 0) return;
      let next = index;
      if (event.key === 'ArrowDown' || event.key === 'ArrowRight') next = (index + 1) % options.length;
      else if (event.key === 'ArrowUp' || event.key === 'ArrowLeft') next = (index - 1 + options.length) % options.length;
      else if (event.key === 'Home') next = 0;
      else if (event.key === 'End') next = options.length - 1;
      else return;
      event.preventDefault();
      options.forEach((option, i) => { option.tabIndex = i === next ? 0 : -1; });
      options[next].focus();
    });
  }

  trigger.addEventListener('click', () => {
    if (menu.hidden) open();
    else close(true);
  });
  trigger.addEventListener('keydown', event => {
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      open();
    }
  });
  document.getElementById('theme-close').addEventListener('click', () => close(true));
  document.getElementById('theme-reset').addEventListener('click', () => {
    commit(DEFAULT_THEME, DEFAULT_MODE);
    document.getElementById('theme-reset').focus();
  });
  document.addEventListener('click', event => {
    if (!menu.hidden && !menu.contains(event.target) && !trigger.contains(event.target)) close(false);
  });
  document.addEventListener('keydown', event => {
    if (event.key === 'Escape' && !menu.hidden) {
      event.preventDefault();
      close(true);
    }
  });

  render();
  // A legacy or invalid stored pair is repaired once, through the same path a
  // click uses, so the pre-paint guard and the stored JSON cannot disagree.
  if (!storedPairMatches(currentTheme(), currentMode())) persistTheme();
  syncThemeMenu();
})();
