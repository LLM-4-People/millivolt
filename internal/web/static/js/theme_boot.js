// Pre-paint theme guard and registry: a classic, non-deferred script so it runs
// before the body paints and after the stylesheets (the computed palette tokens
// must already resolve). A separate file rather than an inline block is what
// lets the document policy drop 'unsafe-inline' from script-src.
//
// This file is the one registry for palette ids, labels, order and defaults.
// js/theme_state.js owns normalization and persistence; js/theme.js owns the
// dashboard tray. The static site publishes these same bytes, so its menu and
// pre-paint guard cannot drift from the dashboard.
const THEME_REGISTRY = {
  themes: [
    { id: 'instrument', label: 'Instrument', hint: 'Warm graphite, blue and violet' },
    { id: 'slate', label: 'Slate', hint: 'Cool stone, lilac and sand' },
    { id: 'midnight', label: 'Midnight', hint: 'Deep navy, sky and periwinkle' },
    { id: 'nord', label: 'Nord', hint: 'Arctic slate, frost and heather' },
    { id: 'forest', label: 'Forest', hint: 'Moss, emerald and lime' },
    { id: 'rose', label: 'Rose', hint: 'Plum, pink and lilac' },
    { id: 'ember', label: 'Ember', hint: 'Charcoal, orange and orchid' },
    { id: 'contrast', label: 'Contrast', hint: 'Maximum separation' },
  ],
  modes: [
    { id: 'clear', label: 'Clear' },
    { id: 'black', label: 'Black' },
  ],
  defaults: { theme: 'instrument', mode: 'black' },
};
try {
  var key = document.querySelector('meta[name="theme-storage-key"]');
  var raw = key ? localStorage.getItem(key.content) : '';
  if (raw) {
    var saved = raw;
    try { saved = JSON.parse(raw); } catch (e) {}
    if (typeof saved === 'string') saved = { theme: saved };
    if (saved && typeof saved === 'object') {
      if (typeof saved.theme === 'string' && THEME_REGISTRY.themes.some(t => t.id === saved.theme)) {
        document.documentElement.dataset.theme = saved.theme;
      }
      if (THEME_REGISTRY.modes.some(m => m.id === saved.mode)) {
        document.documentElement.dataset.mode = saved.mode;
      }
    }
  }
  var themeMeta = document.querySelector('meta[name="theme-color"]');
  var rail = getComputedStyle(document.documentElement).getPropertyValue('--rail').trim();
  if (themeMeta && rail) themeMeta.setAttribute('content', rail);
} catch (e) {}
