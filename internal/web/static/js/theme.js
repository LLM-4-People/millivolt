// Theme picker: the mode and palette registry, its persistence, and the one
// place that applies either choice to the document.
//
// The stored pair is applied before first paint by the inline guard in
// index.html (the meta tag owns the key); this file owns everything after
// that: rendering the header menu, keeping aria-pressed honest, and
// recoloring the canvas charts when a surface or palette changes. Palette
// tokens live in css/themes.css.
const THEME_STORAGE_KEY = document.querySelector('meta[name="theme-storage-key"]')?.content || '';

const THEMES = [
  { id: 'instrument', label: 'Instrument', hint: 'Blue, violet and mint' },
  { id: 'slate', label: 'Slate', hint: 'Steel blue and ice' },
  { id: 'midnight', label: 'Midnight', hint: 'Sky blue and periwinkle' },
  { id: 'nord', label: 'Nord', hint: 'Frost blue and heather' },
  { id: 'forest', label: 'Forest', hint: 'Emerald and lime' },
  { id: 'rose', label: 'Rose', hint: 'Pink and lilac' },
  { id: 'ember', label: 'Ember', hint: 'Amber and coral' },
  { id: 'contrast', label: 'Contrast', hint: 'Maximum separation' },
];

const MODES = [
  { id: 'clear', label: 'Clear' },
  { id: 'black', label: 'Black' },
];

function normalizeTheme(id) {
  return THEMES.some(t => t.id === id) ? id : 'instrument';
}

function normalizeMode(id) {
  return MODES.some(m => m.id === id) ? id : 'black';
}

function currentTheme() {
  return normalizeTheme(document.documentElement.dataset.theme);
}

function currentMode() {
  return normalizeMode(document.documentElement.dataset.mode);
}

function modeLabel(id) {
  return MODES.find(m => m.id === id).label;
}

// applyState is the one mutation path for a theme, a surface, or both. It
// normalizes first, then persists, recolors the canvas, and resyncs the menu.
function applyState(theme, mode, persist) {
  const root = document.documentElement;
  root.dataset.theme = normalizeTheme(theme);
  root.dataset.mode = normalizeMode(mode);
  if (persist) persistTheme();
  syncThemeColor();
  if (typeof refreshChartColors === 'function') refreshChartColors();
  // The plot captures series colors when it is constructed, so drop it first;
  // renderChart then rebuilds the canvas against the new tokens.
  if (typeof resetChart === 'function') resetChart();
  if (typeof renderChart === 'function') renderChart();
  syncThemeMenu();
}

function persistTheme() {
  if (!THEME_STORAGE_KEY) return;
  storage.set(THEME_STORAGE_KEY, JSON.stringify({ theme: currentTheme(), mode: currentMode() }));
}

// The browser paints its own chrome (address bar, scrollbars) from this meta
// tag. Resolving --rail keeps the literal in index.html the only default and
// avoids a second hex table in JS.
function syncThemeColor() {
  const meta = document.querySelector('meta[name="theme-color"]');
  const rail = getComputedStyle(document.documentElement).getPropertyValue('--rail').trim();
  if (meta && rail) meta.setAttribute('content', rail);
}

function syncThemeMenu() {
  const menu = document.getElementById('theme-menu');
  if (!menu) return;
  const theme = currentTheme();
  const mode = currentMode();
  for (const option of menu.querySelectorAll('.theme-opt')) {
    option.setAttribute('aria-pressed', String(option.dataset.themeId === theme));
  }
  for (const option of menu.querySelectorAll('.theme-mode-opt')) {
    option.setAttribute('aria-pressed', String(option.dataset.modeId === mode));
  }
  for (const swatch of menu.querySelectorAll('.theme-swatch')) {
    swatch.dataset.mode = mode;
  }
  const button = document.getElementById('btn-theme');
  if (button) {
    button.setAttribute('aria-label',
      `Theme: ${THEMES.find(t => t.id === theme).label}, ${modeLabel(mode)}`);
  }
}

// renderModeMenu and renderThemeMenu build their controls once. The mode
// control changes the surface; the palette rows keep the current surface and
// preview their accent through the same data attributes the page uses.
function renderModeMenu() {
  const list = document.getElementById('theme-mode');
  if (!list) return;
  list.textContent = '';
  for (const mode of MODES) {
    const option = document.createElement('button');
    option.type = 'button';
    option.className = 'theme-mode-opt';
    option.dataset.modeId = mode.id;
    option.setAttribute('aria-pressed', 'false');
    option.textContent = mode.label;
    option.onclick = () => chooseMode(mode.id);
    list.append(option);
  }
}

function renderThemeMenu() {
  const list = document.getElementById('theme-list');
  if (!list) return;
  list.textContent = '';
  for (const theme of THEMES) {
    const option = document.createElement('button');
    option.type = 'button';
    option.className = 'theme-opt';
    option.dataset.themeId = theme.id;
    option.setAttribute('aria-pressed', 'false');
    option.onclick = () => chooseTheme(theme.id);

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

    option.append(swatch, copy);
    list.append(option);
  }
}

function chooseTheme(id) {
  applyState(id, currentMode(), true);
  hideHdrMenu('theme-menu');
  document.getElementById('btn-theme')?.focus();
}

function chooseMode(id) {
  applyState(currentTheme(), id, true);
}

function toggleThemeMenu(e) {
  toggleHdrMenu(e, 'theme-menu', syncThemeMenu);
}

function initTheme() {
  const root = document.documentElement;
  const theme = normalizeTheme(root.dataset.theme);
  const mode = normalizeMode(root.dataset.mode);
  const changed = root.dataset.theme !== theme || root.dataset.mode !== mode;
  root.dataset.theme = theme;
  root.dataset.mode = mode;
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
