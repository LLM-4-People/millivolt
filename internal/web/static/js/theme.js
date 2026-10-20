// Theme picker: the palette list, its persistence, and the one place that
// applies a palette to the document.
//
// The stored choice is applied before first paint by the inline guard in
// index.html (key below); this file owns everything after that: rendering the
// header menu, keeping aria-checked honest, and recoloring the canvas charts
// when a palette changes. Palettes themselves live in css/themes.css.
const THEME_STORAGE_KEY = 'millivolt.theme';

const THEMES = [
  { id: 'instrument', label: 'Instrument', hint: 'Warm near-black, blue and violet' },
  { id: 'slate', label: 'Slate', hint: 'Cool graphite, blue and violet' },
  { id: 'midnight', label: 'Midnight', hint: 'Deep navy, sky and periwinkle' },
  { id: 'nord', label: 'Nord', hint: 'Blue-grey, frost and heather' },
  { id: 'forest', label: 'Forest', hint: 'Green-black, emerald and lime' },
  { id: 'rose', label: 'Rose', hint: 'Plum black, pink and lilac' },
  { id: 'ember', label: 'Ember', hint: 'Warm near-black, amber and coral' },
  { id: 'paper', label: 'Paper', hint: 'Warm light, indigo and plum' },
  { id: 'mist', label: 'Mist', hint: 'Cool light, blue and teal' },
  { id: 'contrast', label: 'Contrast', hint: 'Black, white and signal yellow' },
];

function currentTheme() {
  const id = document.documentElement.dataset.theme;
  return THEMES.some(t => t.id === id) ? id : 'instrument';
}

// applyTheme normalizes the id, writes the document attribute, persists the
// choice, and repaints what canvas color cannot inherit: the chart palette is
// read once at boot in core.js, so the charts need an explicit refresh.
function applyTheme(id, persist) {
  const theme = THEMES.some(t => t.id === id) ? id : 'instrument';
  const root = document.documentElement;
  if (theme === 'instrument') delete root.dataset.theme;
  else root.dataset.theme = theme;
  if (persist) storage.set(THEME_STORAGE_KEY, theme);
  syncThemeColor();
  if (typeof refreshChartColors === 'function') refreshChartColors();
  // The plot captures series colors when it is constructed, so drop it first;
  // renderChart then rebuilds the canvas against the new palette.
  if (typeof resetChart === 'function') resetChart();
  if (typeof renderChart === 'function') renderChart();
  syncThemeMenu();
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
  const active = currentTheme();
  for (const option of menu.querySelectorAll('.theme-opt')) {
    option.setAttribute('aria-checked', String(option.dataset.themeId === active));
  }
  const button = document.getElementById('btn-theme');
  if (button) button.setAttribute('aria-label', `Theme: ${THEMES.find(t => t.id === active).label}`);
}

// renderThemeMenu builds the option rows once. Each row previews the palette
// it selects: the swatch colors come from themes.css, keyed by data-theme-id,
// so no hex is repeated here.
function renderThemeMenu() {
  const list = document.getElementById('theme-list');
  if (!list) return;
  list.textContent = '';
  for (const theme of THEMES) {
    const option = document.createElement('button');
    option.type = 'button';
    option.className = 'theme-opt';
    option.dataset.themeId = theme.id;
    option.setAttribute('role', 'menuitemradio');
    option.setAttribute('aria-checked', 'false');
    option.onclick = () => chooseTheme(theme.id);

    const swatch = document.createElement('span');
    swatch.className = 'theme-swatch';
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
  applyTheme(id, true);
  hideHdrMenu('theme-menu');
  document.getElementById('btn-theme')?.focus();
}

function toggleThemeMenu(e) {
  toggleHdrMenu(e, 'theme-menu', syncThemeMenu);
}

function initTheme() {
  renderThemeMenu();
  syncThemeColor();
  syncThemeMenu();
}

if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', initTheme);
} else {
  initTheme();
}
