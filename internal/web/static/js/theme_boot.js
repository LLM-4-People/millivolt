// Pre-paint theme guard: a classic, non-deferred script so it runs before the
// body paints and after the stylesheets (the computed palette tokens must
// already resolve). A separate file rather than an inline block is what lets
// the document policy drop 'unsafe-inline' from script-src.
//
// The meta tag in index.html owns the storage key; js/theme.js owns the
// registry, the writes and the validation. This guard only removes the flash
// of the default pair and publishes the resolved rail color to the browser
// chrome. The eight ids mirror THEMES in js/theme.js and theme_check pins both
// sets.
try {
  var key = document.querySelector('meta[name="theme-storage-key"]');
  var raw = key ? localStorage.getItem(key.content) : '';
  if (raw) {
    var saved = raw;
    try { saved = JSON.parse(raw); } catch (e) {}
    if (typeof saved === 'string') saved = { theme: saved };
    if (saved && typeof saved === 'object') {
      if (typeof saved.theme === 'string' && /^(instrument|slate|midnight|nord|forest|rose|ember|contrast)$/.test(saved.theme)) {
        document.documentElement.dataset.theme = saved.theme;
      }
      if (saved.mode === 'clear' || saved.mode === 'black') {
        document.documentElement.dataset.mode = saved.mode;
      }
    }
  }
  var themeMeta = document.querySelector('meta[name="theme-color"]');
  var rail = getComputedStyle(document.documentElement).getPropertyValue('--rail').trim();
  if (themeMeta && rail) themeMeta.setAttribute('content', rail);
} catch (e) {}
