// Shared theme state, for the dashboard and the static site.
//
// THEME_REGISTRY in js/theme_boot.js is the one source for palette ids, labels,
// order and defaults. This file owns normalization, the stored pair, the one
// mutation path and the browser chrome color. A surface supplies its own tray
// markup and binds the commit call; charts and menu sync are optional callbacks
// so the same module runs where neither exists.
const THEME_STORAGE_KEY = document.querySelector('meta[name="theme-storage-key"]')?.content || '';
const THEMES = THEME_REGISTRY.themes;
const MODES = THEME_REGISTRY.modes;
const DEFAULT_THEME = THEME_REGISTRY.defaults.theme;
const DEFAULT_MODE = THEME_REGISTRY.defaults.mode;

function normalizeTheme(id) {
  return THEMES.some(t => t.id === id) ? id : DEFAULT_THEME;
}

function normalizeMode(id) {
  return MODES.some(m => m.id === id) ? id : DEFAULT_MODE;
}

function currentTheme() {
  return normalizeTheme(document.documentElement.dataset.theme);
}

function currentMode() {
  return normalizeMode(document.documentElement.dataset.mode);
}

function themeLabel(id) {
  return THEMES.find(t => t.id === normalizeTheme(id)).label;
}

function modeLabel(id) {
  return MODES.find(m => m.id === normalizeMode(id)).label;
}

function readStoredPair() {
  if (!THEME_STORAGE_KEY) return null;
  let raw;
  try { raw = localStorage.getItem(THEME_STORAGE_KEY); } catch (e) { return null; }
  if (!raw) return null;
  try {
    const saved = JSON.parse(raw);
    if (!saved || typeof saved !== 'object') return null;
    return { theme: saved.theme, mode: saved.mode };
  } catch (e) {
    return null;
  }
}

// storedPairMatches tells applyState whether the browser already holds the
// requested pair. It lets a same-pair click be a true no-op without skipping
// the one persistence a corrected or legacy value still needs.
function storedPairMatches(theme, mode) {
  if (!THEME_STORAGE_KEY) return true;
  const saved = readStoredPair();
  return !!saved && saved.theme === theme && saved.mode === mode;
}

// applyState is the one mutation path for a palette, a surface, or both. It
// normalizes first, then persists, publishes the chrome color, recolors the
// canvas when the dashboard owns one, and resyncs whichever tray is present.
function applyState(theme, mode, persist) {
  const root = document.documentElement;
  const nextTheme = normalizeTheme(theme);
  const nextMode = normalizeMode(mode);
  if (root.dataset.theme === nextTheme && root.dataset.mode === nextMode) {
    if (persist && !storedPairMatches(nextTheme, nextMode)) persistTheme();
    if (typeof syncThemeMenu === 'function') syncThemeMenu();
    return;
  }
  root.dataset.theme = nextTheme;
  root.dataset.mode = nextMode;
  if (persist) persistTheme();
  syncThemeColor();
  if (typeof refreshChartColors === 'function') refreshChartColors();
  // The plot captures series colors when it is constructed, so drop it first;
  // renderChart then rebuilds the canvas against the new tokens.
  if (typeof resetChart === 'function') resetChart();
  if (typeof renderChart === 'function') renderChart();
  if (typeof syncThemeMenu === 'function') syncThemeMenu();
  if (typeof themeApplied === 'function') themeApplied(nextTheme, nextMode);
}

// persistTheme writes the committed pair and reports the truth. A blocked or
// quota-limited localStorage must not leave a label claiming a write that
// never happened; an unchanged sentence is not rewritten, so its live region
// does not announce the same status again.
function persistTheme() {
  if (!THEME_STORAGE_KEY) return false;
  const value = JSON.stringify({ theme: currentTheme(), mode: currentMode() });
  try { localStorage.setItem(THEME_STORAGE_KEY, value); } catch (e) { /* private mode */ }
  let saved = false;
  try { saved = localStorage.getItem(THEME_STORAGE_KEY) === value; } catch (e) { saved = false; }
  const label = document.getElementById('theme-saved');
  if (label) {
    const text = saved ? 'Saved in this browser' : 'Not saved: browser storage unavailable';
    if (label.textContent !== text) label.textContent = text;
  }
  return saved;
}

// The browser paints its own chrome (address bar, scrollbars) from this meta
// tag. Resolving --rail keeps the literal in the document the only default and
// avoids a second hex table in JS.
function syncThemeColor() {
  const meta = document.querySelector('meta[name="theme-color"]');
  const rail = getComputedStyle(document.documentElement).getPropertyValue('--rail').trim();
  if (meta && rail) meta.setAttribute('content', rail);
}
