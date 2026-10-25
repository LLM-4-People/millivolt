// Theme picker interaction: menu structure, previews, commits and keys.

const { contrast } = require('./theme_check_color');
const {
  cssBlock,
  cssProperties,
  cssSource,
  cssTokenReference,
} = require('./theme_check_css');

async function pickerChecks(context) {
  const {
    check, win, d, root, themes, modes, token, elementToken, selectionIsSingle,
    dashboardCSS, STORAGE_KEY,
  } = context;
  win.toggleThemeMenu({ stopPropagation() {} });
  check('the theme menu opens', !d.getElementById('theme-menu').hidden);
  const panes = d.querySelector('.theme-panes');
  const currentPane = d.getElementById('theme-current');
  const stage = d.getElementById('theme-preview');
  check('the comparator holds current and preview panes',
    !!panes && panes.getAttribute('aria-hidden') === 'true' &&
    !!currentPane && !!stage && panes.contains(currentPane) && panes.contains(stage));
  check('both panes use the shared pane component and visual labels',
    [currentPane, stage].every(pane => pane.classList.contains('theme-pane')) &&
    currentPane?.querySelector('.tp-label')?.textContent === 'Current' &&
    stage?.querySelector('.tp-label')?.textContent === 'Preview');
  check('both panes hold the same series legend instead of status pills',
    [currentPane, stage].every(pane =>
      pane.querySelectorAll('.tp-legend .tp-key').length === 3 &&
      !pane.querySelector('.tp-pill') &&
      pane.querySelector('.tp-legend').textContent === '' &&
      !/200|live|429/.test(pane.textContent)));
  check('the pane legend keys resolve the three plot roles', (() => {
    const roles = ['.tp-key', '.tp-key:nth-child(2)', '.tp-key:nth-child(3)']
      .map(selector => cssTokenReference(
        cssProperties(cssBlock(dashboardCSS, selector)).get('background')));
    return roles.join(',') === 'accent,accent2,cyan' &&
      !/status|200|live|429/i.test(cssBlock(dashboardCSS, '.tp-legend'));
  })());
  check('the palette list never becomes a nested scroller', (() => {
    const list = cssProperties(cssBlock(dashboardCSS, '.theme-list'));
    return !list.has('max-height') && !list.has('overflow-y') &&
      !/theme-list\s*\{[^}]*overflow/.test(cssSource(dashboardCSS));
  })());
  check('the panes differ only in id, data pair and label text', (() => {
    const shape = pane => {
      const clone = pane.cloneNode(true);
      clone.removeAttribute('id');
      clone.removeAttribute('data-theme');
      clone.removeAttribute('data-mode');
      clone.querySelector('.tp-label').textContent = '';
      return clone.innerHTML;
    };
    return shape(currentPane) === shape(stage);
  })());
  check('the current pane resolves the committed pair',
    currentPane?.dataset.theme === 'instrument' && currentPane?.dataset.mode === 'black');
  check('the preview pane starts on the committed pair',
    stage?.dataset.theme === 'instrument' && stage?.dataset.mode === 'black');
  check('the caption bar stays outside the decorative comparator',
    !!d.querySelector('.theme-caption-bar') && !panes.contains(d.querySelector('.theme-caption-bar')) &&
    d.getElementById('theme-current-label')?.textContent === 'Current: Instrument / Black' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same');
  check('the preview caption starts muted without a candidate',
    d.getElementById('theme-preview-label')?.classList.contains('has-preview') === false);
  check('the Ember hint names its orchid accent rather than coral',
    d.querySelector('[data-theme-id="ember"] .theme-hint')?.textContent ===
      'Charcoal, orange and orchid');
  check('pane labels use the compact 10px step',
    /\b10px\b/.test(cssProperties(cssBlock(dashboardCSS, '.tp-label')).get('font') || ''));
  check('the surface control lives in the caption bar with a visible label',
    d.getElementById('theme-mode').closest('.theme-caption-bar') === d.querySelector('.theme-caption-bar') &&
    d.querySelector('.theme-caption-bar .theme-mode-label')?.textContent === 'Surface' &&
    d.getElementById('theme-mode').getAttribute('aria-labelledby') === 'theme-mode-label' &&
    !d.getElementById('theme-surface-hd'));
  check('the surface control is a radio group', d.getElementById('theme-mode').getAttribute('role') === 'radiogroup');
  check('the palette control is a radio group', d.getElementById('theme-list').getAttribute('role') === 'radiogroup');
  check('the palette radio group is vertical',
    d.getElementById('theme-list').getAttribute('aria-orientation') === 'vertical');
  check('each surface is a radio',
    d.querySelectorAll('[data-mode-id]').length === modes.length &&
    [...d.querySelectorAll('[data-mode-id]')].every(o => o.getAttribute('role') === 'radio'));
  check('each palette is a radio',
    d.querySelectorAll('[data-theme-id]').length === themes.length &&
    [...d.querySelectorAll('[data-theme-id]')].every(o => o.getAttribute('role') === 'radio'));
  check('the current palette is marked',
    d.querySelector('[data-theme-id="instrument"]').getAttribute('aria-checked') === 'true');
  check('the current surface is marked',
    d.querySelector('[data-mode-id="black"]').getAttribute('aria-checked') === 'true');
  check('each surface carries a visible selection marker',
    [...d.querySelectorAll('[data-mode-id]')].every(option => !!option.querySelector('.theme-check')));
  check('exactly one surface marker is visible', (() => {
    const marks = [...d.querySelectorAll('[data-mode-id] .theme-check')]
      .filter(mark => parseFloat(win.getComputedStyle(mark).opacity) > 0);
    return marks.length === 1 &&
      marks[0].closest('[data-mode-id]').getAttribute('aria-checked') === 'true';
  })());
  check('each palette carries a visible selection marker',
    [...d.querySelectorAll('[data-theme-id]')].every(option => !!option.querySelector('.theme-check')));
  check('exactly one palette marker is visible', (() => {
    const marks = [...d.querySelectorAll('[data-theme-id] .theme-check')]
      .filter(mark => parseFloat(win.getComputedStyle(mark).opacity) > 0);
    return marks.length === 1 &&
      marks[0].closest('[data-theme-id]').getAttribute('aria-checked') === 'true';
  })());
  check('the comparator is decorative for assistive technology',
    panes?.getAttribute('aria-hidden') === 'true');
  check('the picker has one polite candidate status',
    d.getElementById('theme-status')?.getAttribute('role') === 'status' &&
    d.getElementById('theme-status')?.getAttribute('aria-live') === 'polite');
  check('the hint describes preview, pointer and commit keys',
    d.querySelector('.theme-hintbar')?.textContent ===
      'Arrows preview. Click or press Enter to apply. Esc closes.');
  check('the footer offers Reset and a neutral Close, not an apply step',
    d.getElementById('theme-reset')?.textContent === 'Reset' &&
    d.getElementById('theme-reset')?.title === 'Restore Instrument Black' &&
    d.getElementById('theme-close')?.textContent === 'Close' &&
    !d.getElementById('theme-close')?.classList.contains('btn-accent') &&
    !d.getElementById('theme-done'));
  check('the surface radio group has one checked row and one tab stop',
    selectionIsSingle(d.getElementById('theme-mode'), modes.length));
  check('the palette radio group has one checked row and one tab stop',
    selectionIsSingle(d.getElementById('theme-list'), themes.length));
  check('opening the theme menu sets aria-expanded',
    d.getElementById('btn-theme').getAttribute('aria-expanded') === 'true');

  d.querySelector('[data-mode-id="clear"]').click();
  check('clear applies to the document', root.dataset.mode === 'clear');
  check('choosing a surface leaves the menu open', !d.getElementById('theme-menu').hidden);
  check('clear swaps the surface tokens', token(win, '--bg') === '#f7f2e8');
  check('clear flips the native color scheme', token(win, 'color-scheme') === 'light');
  check('the browser theme color follows the surface',
    d.querySelector('meta[name="theme-color"]').getAttribute('content') === '#efe7d6');
  check('the pair persists as one value',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'instrument', mode: 'clear' }));
  check('a successful write keeps the truthful saved label',
    d.getElementById('theme-saved')?.textContent === 'Saved in this browser');
  check('the saved status is a polite live region',
    d.getElementById('theme-saved')?.getAttribute('role') === 'status' &&
    d.getElementById('theme-saved')?.getAttribute('aria-live') === 'polite');
  check('switching recolors the canvas palette', win.eval('COLORS.accent') === '#1e40af');
  check('every swatch previews the chosen surface',
    d.querySelectorAll('.theme-swatch').length === themes.length &&
    [...d.querySelectorAll('.theme-swatch')].every(swatch => swatch.dataset.mode === 'clear'));
  check('choosing a surface leaves one checked row and one tab stop per group',
    selectionIsSingle(d.getElementById('theme-mode'), modes.length) &&
    selectionIsSingle(d.getElementById('theme-list'), themes.length));
  check('the instrument swatch resolves the clear accent',
    win.getComputedStyle(d.querySelector('[data-theme-id="instrument"] .theme-swatch'))
      .getPropertyValue('--accent').trim() === '#1e40af');
  check('a non-active swatch resolves its own accent',
    win.getComputedStyle(d.querySelector('[data-theme-id="forest"] .theme-swatch'))
      .getPropertyValue('--accent').trim() === '#065f46');

  // A Black preview inside the committed Clear page must resolve Black ink
  // and materials, not inherit Clear text and status colors.
  const stageToken = name => stage ? elementToken(win, stage, name) : '';
  const currentPaneToken = name => currentPane ? elementToken(win, currentPane, name) : '';
  const blackMode = d.querySelector('[data-mode-id="black"]');
  blackMode.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  check('a Black preview keeps the committed Clear root',
    root.dataset.theme === 'instrument' && root.dataset.mode === 'clear');
  check('a Black preview resolves Black ink in the stage',
    !!stage && stageToken('--text') === '#e8e4de' &&
    stageToken('--muted') === '#97907e');
  check('a Black preview forces the dark native color scheme',
    !!stage && stage.getAttribute('data-mode') === 'black' &&
    elementToken(win, stage, 'color-scheme') === 'dark');
  check('the preview caption stays readable against its stage',
    !!stage && contrast(stageToken('--muted'), stageToken('--bg')) >= 4.5);
  check('a preview leaves the current pane on the committed pair',
    currentPane?.dataset.mode === 'clear' && currentPaneToken('--bg') === '#f7f2e8' &&
    d.getElementById('theme-current-label')?.textContent === 'Current: Instrument / Clear');
  check('a preview caption names the candidate surface',
    d.getElementById('theme-preview-label')?.textContent === 'Preview: Instrument / Black' &&
    d.getElementById('theme-preview-label')?.classList.contains('has-preview'));
  blackMode.dispatchEvent(new win.MouseEvent('mouseleave'));
  check('a Clear preview forces the light native color scheme',
    !!stage && stage.getAttribute('data-mode') === 'clear' &&
    elementToken(win, stage, 'color-scheme') === 'light');
  check('leaving a candidate restores the muted preview caption',
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same' &&
    !d.getElementById('theme-preview-label')?.classList.contains('has-preview'));

  const forestOpt = d.querySelector('[data-theme-id="forest"]');
  forestOpt.focus();
  check('focusing a palette previews it in the stage',
    d.getElementById('theme-preview')?.dataset.theme === 'forest');
  check('focusing a palette does not commit the root', root.dataset.theme === 'instrument');
  check('keyboard focus announces the candidate politely',
    d.getElementById('theme-status')?.textContent === 'Preview: Forest / Clear');
  forestOpt.blur();
  check('leaving a palette with focus restores the committed preview',
    d.getElementById('theme-preview')?.dataset.theme === 'instrument');
  check('leaving the palette clears the candidate status',
    d.getElementById('theme-status')?.textContent === '');
  // A pointer press focuses the row before its click commits. That focus is
  // silent in the polite live region; only a keyboard focus announces.
  d.getElementById('btn-theme').focus();
  check('focus on the trigger clears the candidate status',
    d.getElementById('theme-status')?.textContent === '');
  forestOpt.dispatchEvent(new win.MouseEvent('pointerdown', { bubbles: true }));
  forestOpt.focus();
  check('pointer-induced focus previews without announcing',
    d.getElementById('theme-preview')?.dataset.theme === 'forest' &&
    d.getElementById('theme-status')?.textContent === '');
  forestOpt.dispatchEvent(new win.MouseEvent('pointerup', { bubbles: true }));
  d.getElementById('btn-theme').focus();
  forestOpt.focus();
  check('keyboard focus after a pointer press still announces',
    d.getElementById('theme-status')?.textContent === 'Preview: Forest / Clear');
  forestOpt.blur();
  forestOpt.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  check('hovering a palette previews it in the stage',
    d.getElementById('theme-preview')?.dataset.theme === 'forest');
  check('hovering a palette does not commit the root',
    root.dataset.theme === 'instrument');
  check('hovering a palette does not persist it',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'instrument', mode: 'clear' }));
  check('hovering a palette does not touch the browser theme color',
    d.querySelector('meta[name="theme-color"]').getAttribute('content') === '#efe7d6');
  check('hovering a palette does not announce a candidate',
    d.getElementById('theme-status')?.textContent === '');
  check('the preview caption names the candidate',
    d.getElementById('theme-preview-label')?.textContent === 'Preview: Forest / Clear');
  forestOpt.dispatchEvent(new win.MouseEvent('mouseleave'));
  check('leaving a palette restores the committed preview',
    d.getElementById('theme-preview')?.dataset.theme === 'instrument');

  // Keyboard focus and hover can name different candidates. Leaving the
  // hovered row must restore the focused row, not the committed pair.
  forestOpt.focus();
  const roseOpt = d.querySelector('[data-theme-id="rose"]');
  roseOpt.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  check('hovering a second palette previews it over the focused one',
    d.getElementById('theme-preview')?.dataset.theme === 'rose');
  roseOpt.dispatchEvent(new win.MouseEvent('mouseleave'));
  check('mouse leave restores the focused palette preview',
    d.getElementById('theme-preview')?.dataset.theme === 'forest');
  check('mouse leave keeps the committed root',
    root.dataset.theme === 'instrument');
  forestOpt.blur();
  check('focus leaving the tray restores the committed preview',
    d.getElementById('theme-preview')?.dataset.theme === 'instrument');

  // Surface previews follow the same contract: focus and hover show the
  // candidate, leaving restores the committed pair, and neither touches the
  // chart. A preview is presentation inside the tray only.
  blackMode.focus();
  check('focusing a surface previews it in the stage',
    d.getElementById('theme-preview')?.dataset.mode === 'black');
  blackMode.blur();
  check('leaving a surface with focus restores the committed preview',
    d.getElementById('theme-preview')?.dataset.mode === 'clear');
  blackMode.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  check('hovering a surface previews it in the stage',
    d.getElementById('theme-preview')?.dataset.mode === 'black');
  blackMode.dispatchEvent(new win.MouseEvent('mouseleave'));
  check('leaving a surface restores the committed preview',
    d.getElementById('theme-preview')?.dataset.mode === 'clear');
  {
    const saved = {
      refresh: win.refreshChartColors,
      reset: win.resetChart,
      render: win.renderChart,
    };
    let chartEffects = 0;
    win.refreshChartColors = () => { chartEffects += 1; };
    win.resetChart = () => { chartEffects += 1; };
    win.renderChart = () => { chartEffects += 1; };
    try {
      forestOpt.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
      forestOpt.focus();
      forestOpt.blur();
      forestOpt.dispatchEvent(new win.MouseEvent('mouseleave'));
      blackMode.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
      blackMode.focus();
      blackMode.blur();
      blackMode.dispatchEvent(new win.MouseEvent('mouseleave'));
    } finally {
      win.refreshChartColors = saved.refresh;
      win.resetChart = saved.reset;
      win.renderChart = saved.render;
    }
    check('palette and surface previews never touch the chart', chartEffects === 0);
  }

  forestOpt.focus();
  forestOpt.click();
  check('forest applies to the document', root.dataset.theme === 'forest');
  check('a palette keeps the chosen surface', root.dataset.mode === 'clear');
  check('forest persists with the surface',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'forest', mode: 'clear' }));
  check('committing keeps the focused candidate status truthful',
    d.getElementById('theme-status')?.textContent === 'Current: Forest / Clear');
  check('forest swaps the accent tokens', token(win, '--accent') === '#065f46');
  check('committing updates both captions and both panes',
    d.getElementById('theme-current-label')?.textContent === 'Current: Forest / Clear' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same' &&
    currentPane?.dataset.theme === 'forest' && currentPane?.dataset.mode === 'clear' &&
    stage?.dataset.theme === 'forest' && stage?.dataset.mode === 'clear');
  check('a palette switch swaps the full surface family', token(win, '--bg') === '#eef6ef');
  check('the trigger names both choices',
    d.getElementById('btn-theme').getAttribute('aria-label') === 'Theme: Forest, Clear');
  forestOpt.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  check('previewing the committed pair keeps the current caption and says same',
    d.getElementById('theme-current-label')?.textContent === 'Current: Forest / Clear' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same');
  forestOpt.dispatchEvent(new win.MouseEvent('mouseleave'));
  check('switching a palette leaves the tray open', !d.getElementById('theme-menu').hidden);
  check('switching a palette keeps focus on that row', d.activeElement?.dataset.themeId === 'forest');
  check('switching a palette keeps one checked row and one tab stop per group',
    selectionIsSingle(d.getElementById('theme-mode'), modes.length) &&
    selectionIsSingle(d.getElementById('theme-list'), themes.length));

  // Selecting the pair that is already committed must not persist or touch
  // any chart owner. A corrected or legacy stored value still deserves exactly
  // one write. Spy on the shared Storage prototype for both proofs instead of
  // reading the final value, which cannot distinguish a no-op from a rewrite.
  const committed = win.localStorage.getItem(STORAGE_KEY);
  const chartOwners = {
    refreshChartColors: win.refreshChartColors,
    resetChart: win.resetChart,
    renderChart: win.renderChart,
  };
  const chartCalls = { refresh: 0, reset: 0, render: 0 };
  win.refreshChartColors = (...args) => { chartCalls.refresh += 1; return chartOwners.refreshChartColors(...args); };
  win.resetChart = (...args) => { chartCalls.reset += 1; return chartOwners.resetChart(...args); };
  win.renderChart = (...args) => { chartCalls.render += 1; return chartOwners.renderChart(...args); };
  const realSetItem = win.Storage.prototype.setItem;
  let writes = 0;
  win.Storage.prototype.setItem = function (...args) {
    writes += 1;
    return realSetItem.apply(this, args);
  };
  try {
    forestOpt.click();
  } finally {
    win.Storage.prototype.setItem = realSetItem;
    win.refreshChartColors = chartOwners.refreshChartColors;
    win.resetChart = chartOwners.resetChart;
    win.renderChart = chartOwners.renderChart;
  }
  check('selecting the current pair is a no-op for every chart owner and storage',
    chartCalls.refresh === 0 && chartCalls.reset === 0 && chartCalls.render === 0 &&
    writes === 0 && win.localStorage.getItem(STORAGE_KEY) === committed);
  win.localStorage.setItem(STORAGE_KEY, 'legacy-forest');
  let repairs = 0;
  win.Storage.prototype.setItem = function (...args) {
    repairs += 1;
    return realSetItem.apply(this, args);
  };
  try {
    forestOpt.click();
  } finally {
    win.Storage.prototype.setItem = realSetItem;
  }
  check('a same-pair click rewrites an invalid stored value exactly once',
    repairs === 1 &&
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'forest', mode: 'clear' }));

  d.querySelector('[data-mode-id="black"]').click();
  check('black applies without changing the palette',
    root.dataset.theme === 'forest' && root.dataset.mode === 'black');
  check('black keeps the menu open', !d.getElementById('theme-menu').hidden);
  check('black restores the forest dark surface', token(win, '--bg') === '#0f1713');
  check('black resolves the forest accent', token(win, '--accent') === '#34d399');
  check('black persists with the palette',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'forest', mode: 'black' }));
  check('every swatch previews the black surface',
    d.querySelectorAll('.theme-swatch').length === themes.length &&
    [...d.querySelectorAll('.theme-swatch')].every(swatch => swatch.dataset.mode === 'black'));
  check('black keeps one checked row and one tab stop per group',
    selectionIsSingle(d.getElementById('theme-mode'), modes.length) &&
    selectionIsSingle(d.getElementById('theme-list'), themes.length));

  d.getElementById('theme-reset').click();
  check('reset commits the default pair and leaves the tray open',
    root.dataset.theme === 'instrument' && root.dataset.mode === 'black' &&
    !d.getElementById('theme-menu').hidden);
  check('reset persists the default pair',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'instrument', mode: 'black' }));
  check('reset restores one checked row and one tab stop per group',
    selectionIsSingle(d.getElementById('theme-mode'), modes.length) &&
    selectionIsSingle(d.getElementById('theme-list'), themes.length));

  // The saved label is a live region. Reassigning identical text replaces its
  // text node and can announce the same status again, so the guarded writer
  // must leave it untouched through persist, a same-pair click, and a
  // close/open cycle.
  {
    const saved = d.getElementById('theme-saved');
    check('the saved label starts truthful', saved.textContent === 'Saved in this browser');
    const mutations = [];
    const observer = new win.MutationObserver(records => mutations.push(...records));
    observer.observe(saved, {childList: true, characterData: true, subtree: true});
    win.eval('persistTheme()');
    d.querySelector('.theme-opt[aria-checked="true"]').click();
    d.getElementById('theme-close').click();
    d.getElementById('btn-theme').click();
    await new Promise(resolve => setTimeout(resolve, 0));
    observer.disconnect();
    check('same-pair, close and open never rewrite identical saved text',
      mutations.length === 0 && saved.textContent === 'Saved in this browser');
  }

  // Roving-radio keys are a preview cursor: arrows, Home and End move focus
  // and the preview candidate without committing. Enter or Space stays a
  // normal button activation on the focused option. Alt/Ctrl/Meta + arrow
  // belong to the browser or OS and change nothing.
  const currentPalette = () => d.querySelector('.theme-opt[aria-checked="true"]');
  currentPalette().focus();
  {
    const before = {
      theme: root.dataset.theme,
      mode: root.dataset.mode,
      focus: d.activeElement?.dataset.themeId,
    };
    for (const modifier of ['altKey', 'ctrlKey', 'metaKey']) {
      const event = new win.KeyboardEvent('keydown', {
        key: 'ArrowDown', bubbles: true, cancelable: true, [modifier]: true,
      });
      currentPalette().dispatchEvent(event);
      check(`${modifier}+ArrowDown leaves the palette, focus and default alone`,
        root.dataset.theme === before.theme && root.dataset.mode === before.mode &&
        d.activeElement?.dataset.themeId === before.focus && !event.defaultPrevented);
    }
  }
  currentPalette().focus();
  let arrow = new win.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true });
  currentPalette().dispatchEvent(arrow);
  check('ArrowDown previews the next palette without committing',
    root.dataset.theme === 'instrument' && d.activeElement?.dataset.themeId === 'slate' &&
    stage?.dataset.theme === 'slate' && currentPalette().dataset.themeId === 'instrument' &&
    selectionIsSingle(d.getElementById('theme-list'), themes.length) &&
    arrow.defaultPrevented);
  check('ArrowDown announces the preview candidate',
    d.getElementById('theme-status')?.textContent === 'Preview: Slate / Black' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: Slate / Black');
  check('the keyboard cursor owns the palette group tab stop',
    d.querySelector('[data-theme-id="slate"]').tabIndex === 0 &&
    [...d.querySelectorAll('[data-theme-id]')].filter(option => option.tabIndex === 0).length === 1 &&
    d.querySelector('[data-theme-id="instrument"]').tabIndex === -1);
  d.getElementById('btn-theme').focus();
  d.querySelector('[data-theme-id="slate"]').focus();
  check('Tab out and back returns to the focused preview row',
    d.activeElement?.dataset.themeId === 'slate' &&
    d.querySelector('[data-theme-id="slate"]').tabIndex === 0 &&
    stage?.dataset.theme === 'slate');
  arrow = new win.KeyboardEvent('keydown', { key: 'End', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(arrow);
  check('End previews the last palette without committing',
    root.dataset.theme === 'instrument' && d.activeElement?.dataset.themeId === 'contrast' &&
    stage?.dataset.theme === 'contrast');
  arrow = new win.KeyboardEvent('keydown', { key: 'Home', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(arrow);
  check('Home previews the first palette without committing',
    root.dataset.theme === 'instrument' && d.activeElement?.dataset.themeId === 'instrument' &&
    stage?.dataset.theme === 'instrument' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same');
  arrow = new win.KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(arrow);
  check('ArrowUp wraps the preview to the last palette without committing',
    root.dataset.theme === 'instrument' && d.activeElement?.dataset.themeId === 'contrast' &&
    stage?.dataset.theme === 'contrast');
  const enter = new win.KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(enter);
  check('Enter is left to the focused radio option\'s native activation',
    !enter.defaultPrevented && root.dataset.theme === 'instrument');
  d.activeElement.click();
  check('activating the previewed palette commits it',
    root.dataset.theme === 'contrast' &&
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'contrast', mode: 'black' }));
  d.getElementById('theme-reset').click();

  // The surface group is a preview cursor too: keys move focus and preview
  // the other surface; activation commits it.
  const currentSurface = () => d.querySelector('.theme-mode-opt[aria-checked="true"]');
  currentSurface().focus();
  arrow = new win.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true });
  currentSurface().dispatchEvent(arrow);
  check('ArrowDown previews the next surface without committing',
    root.dataset.mode === 'black' && d.activeElement?.dataset.modeId === 'clear' &&
    stage?.dataset.mode === 'clear' && currentSurface().dataset.modeId === 'black' &&
    selectionIsSingle(d.getElementById('theme-mode'), modes.length) && arrow.defaultPrevented);
  check('the surface preview announces its candidate',
    d.getElementById('theme-status')?.textContent === 'Preview: Instrument / Clear' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: Instrument / Clear');
  check('the surface keyboard cursor owns that group tab stop',
    d.querySelector('[data-mode-id="clear"]').tabIndex === 0 &&
    [...d.querySelectorAll('[data-mode-id]')].filter(option => option.tabIndex === 0).length === 1 &&
    d.querySelector('[data-mode-id="black"]').tabIndex === -1);
  arrow = new win.KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(arrow);
  check('ArrowUp previews the previous surface without committing',
    root.dataset.mode === 'black' && d.activeElement?.dataset.modeId === 'black' &&
    stage?.dataset.mode === 'black');
  arrow = new win.KeyboardEvent('keydown', { key: 'End', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(arrow);
  check('End previews the last surface without committing', root.dataset.mode === 'black');
  arrow = new win.KeyboardEvent('keydown', { key: 'Home', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(arrow);
  check('Home previews the first surface without committing',
    root.dataset.mode === 'black' && d.activeElement?.dataset.modeId === 'clear' &&
    stage?.dataset.mode === 'clear');
  const space = new win.KeyboardEvent('keydown', { key: ' ', bubbles: true, cancelable: true });
  d.activeElement.dispatchEvent(space);
  check('Space is left to the focused surface option\'s native activation',
    !space.defaultPrevented && root.dataset.mode === 'black');
  d.activeElement.click();
  check('activating the previewed surface commits it', root.dataset.mode === 'clear');
  d.getElementById('theme-reset').click();

  // Programmatic or assistive-technology activation can commit a row without
  // moving DOM focus. The panes reset to the committed pair, so a stale
  // candidate line from another focused or hovered row must not survive.
  {
    const reset = d.getElementById('theme-reset');
    reset.focus();
    const forest = d.querySelector('[data-theme-id="forest"]');
    forest.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
    check('a hovered candidate does not write the keyboard live region',
      stage?.dataset.theme === 'forest' &&
      d.getElementById('theme-status')?.textContent === '');
    forest.click();
    check('programmatic activation commits and clears the stale candidate',
      root.dataset.theme === 'forest' &&
      d.activeElement === reset &&
      d.getElementById('theme-status')?.textContent === '' &&
      stage?.dataset.theme === 'forest' &&
      d.getElementById('theme-preview-label')?.textContent === 'Preview: same' &&
      !d.getElementById('theme-preview-label')?.classList.contains('has-preview'));
    d.getElementById('theme-reset').click();
  }

  // Close with a live candidate: the committed root, storage and both panes
  // stay put, the candidate line clears and focus returns to the trigger.
  {
    const rose = d.querySelector('[data-theme-id="rose"]');
    rose.dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
    check('a live candidate is visible before Close',
      stage?.dataset.theme === 'rose' &&
      d.getElementById('theme-preview-label')?.textContent === 'Preview: Rose / Black');
    d.getElementById('theme-close').click();
    check('Close with a live candidate keeps the committed pair and clears the candidate',
      d.getElementById('theme-menu').hidden &&
      root.dataset.theme === 'instrument' && root.dataset.mode === 'black' &&
      stage?.dataset.theme === 'instrument' && stage?.dataset.mode === 'black' &&
      d.getElementById('theme-status')?.textContent === '' &&
      d.getElementById('theme-preview-label')?.textContent === 'Preview: same' &&
      !d.getElementById('theme-preview-label')?.classList.contains('has-preview') &&
      win.localStorage.getItem(STORAGE_KEY) ===
        JSON.stringify({ theme: 'instrument', mode: 'black' }) &&
      d.activeElement?.id === 'btn-theme');
  }

  // Escape shares the one close cleanup: the candidate is discarded even
  // though the close comes from the document key handler.
  d.getElementById('btn-theme').click();
  d.querySelector('[data-theme-id="nord"]')
    .dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  d.dispatchEvent(new win.KeyboardEvent('keydown', {
    key: 'Escape', bubbles: true, cancelable: true,
  }));
  check('Escape with a live candidate keeps the committed pair and clears the candidate',
    d.getElementById('theme-menu').hidden &&
    root.dataset.theme === 'instrument' && root.dataset.mode === 'black' &&
    stage?.dataset.theme === 'instrument' && stage?.dataset.mode === 'black' &&
    d.getElementById('theme-status')?.textContent === '' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same' &&
    !d.getElementById('theme-preview-label')?.classList.contains('has-preview') &&
    win.localStorage.getItem(STORAGE_KEY) ===
      JSON.stringify({ theme: 'instrument', mode: 'black' }) &&
    d.activeElement?.id === 'btn-theme');

  // An outside click closes through the same lifecycle and never leaves focus
  // on a hidden row.
  d.getElementById('btn-theme').click();
  d.querySelector('[data-theme-id="ember"]')
    .dispatchEvent(new win.MouseEvent('mouseenter', { bubbles: true }));
  d.body.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('an outside click with a live candidate keeps the committed pair and clears the candidate',
    d.getElementById('theme-menu').hidden &&
    root.dataset.theme === 'instrument' && root.dataset.mode === 'black' &&
    stage?.dataset.theme === 'instrument' && stage?.dataset.mode === 'black' &&
    d.getElementById('theme-status')?.textContent === '' &&
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same' &&
    !d.getElementById('theme-preview-label')?.classList.contains('has-preview') &&
    win.localStorage.getItem(STORAGE_KEY) ===
      JSON.stringify({ theme: 'instrument', mode: 'black' }) &&
    !d.getElementById('theme-menu').contains(d.activeElement));
  d.getElementById('btn-theme').click();

  d.getElementById('theme-close').click();
  check('Close dismisses the tray without changing the pair',
    d.getElementById('theme-menu').hidden &&
    root.dataset.theme === 'instrument' && root.dataset.mode === 'black' &&
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'instrument', mode: 'black' }));
  check('Close clears aria-expanded', d.getElementById('btn-theme').getAttribute('aria-expanded') === 'false');
  check('Close restores focus to the trigger', d.activeElement?.id === 'btn-theme');

  // A modified arrow on the closed trigger must not open the tray or cancel
  // the browser's native action.
  {
    const trigger = d.getElementById('btn-theme');
    trigger.focus();
    for (const modifier of ['altKey', 'ctrlKey', 'metaKey']) {
      const event = new win.KeyboardEvent('keydown', {
        key: 'ArrowDown', bubbles: true, cancelable: true, [modifier]: true,
      });
      trigger.dispatchEvent(event);
      check(`${modifier}+ArrowDown on the closed trigger does not open the tray`,
        d.getElementById('theme-menu').hidden && !event.defaultPrevented &&
        root.dataset.theme === 'instrument');
    }
  }

  d.getElementById('btn-theme').focus();
  d.getElementById('btn-theme').dispatchEvent(new win.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
  check('ArrowDown on the trigger opens the tray on the checked palette',
    !d.getElementById('theme-menu').hidden &&
    d.activeElement?.dataset.themeId === 'instrument' &&
    d.getElementById('btn-theme').getAttribute('aria-expanded') === 'true');
  d.getElementById('btn-theme').focus();
  for (const modifier of ['altKey', 'ctrlKey', 'metaKey']) {
    const event = new win.KeyboardEvent('keydown', {
      key: 'ArrowDown', bubbles: true, cancelable: true, [modifier]: true,
    });
    d.getElementById('btn-theme').dispatchEvent(event);
    check(`${modifier}+ArrowDown on the open trigger does not commit`,
      root.dataset.theme === 'instrument' && !d.getElementById('theme-menu').hidden &&
      d.activeElement?.id === 'btn-theme' && !event.defaultPrevented);
  }
  d.getElementById('btn-theme').focus();
  d.getElementById('btn-theme').dispatchEvent(new win.KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true }));
  check('ArrowDown on an open trigger previews the next palette and keeps the tray open',
    root.dataset.theme === 'instrument' && d.activeElement?.dataset.themeId === 'slate' &&
    stage?.dataset.theme === 'slate' && !d.getElementById('theme-menu').hidden &&
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'instrument', mode: 'black' }));
  d.getElementById('btn-theme').focus();
  d.getElementById('btn-theme').dispatchEvent(new win.KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }));
  check('ArrowUp on an open trigger previews the previous palette and keeps the tray open',
    root.dataset.theme === 'instrument' && d.activeElement?.dataset.themeId === 'contrast' &&
    stage?.dataset.theme === 'contrast' && !d.getElementById('theme-menu').hidden);
  d.body.dispatchEvent(new win.MouseEvent('click', { bubbles: true }));
  check('an outside click closes the tray and clears aria-expanded',
    d.getElementById('theme-menu').hidden &&
    d.getElementById('btn-theme').getAttribute('aria-expanded') === 'false');
}

module.exports = { pickerChecks };
