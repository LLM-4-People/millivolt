// Dashboard shell boot, stored-pair normalization and storage truthfulness.
//
// The dashboard shell is assembled the way the browser sees it: stylesheets
// are inlined because jsdom does not fetch them from disk, while scripts keep
// their <script src="/dash/..."> tags and are loaded through a local resource
// loader. Building script elements as strings would make the fixture's own
// test data an HTML-injection sink, which CodeQL rightly rejects.

const fs = require('fs');
const path = require('path');
const { JSDOM, requestInterceptor } = require('jsdom');

const { contrast } = require('./theme_check_color');

const STATIC = path.join(__dirname, '..', 'internal/web/static');
const TEST_DASHBOARD_VERSION = '0000000000000001';
const STORAGE_KEY = 'millivolt.theme';

const dashScripts = requestInterceptor(request => {
  const url = new URL(request.url);
  if (!url.pathname.startsWith('/dash/')) return undefined;
  const rel = decodeURIComponent(url.pathname.slice('/dash/'.length));
  const file = path.resolve(STATIC, rel);
  if (!file.startsWith(STATIC + path.sep)) return undefined;
  return new Response(fs.readFileSync(file), {
    headers: { 'Content-Type': 'application/javascript' },
  });
});

function assembleHTML(options = {}) {
  let html = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
  html = html.replaceAll('__DASHBOARD_VERSION__', TEST_DASHBOARD_VERSION);
  html = html.replace('__DASHBOARD_BOOTSTRAP__', '{"pending_revision":0}');
  html = html.replace(/<link\s+rel="stylesheet"\s+href="\/dash\/([^"]+)"\s*>/g, (_, ref) =>
    `<style>\n${fs.readFileSync(path.join(STATIC, ref.split('?')[0]), 'utf8')}\n</style>`);
  if (options.excludeThemeScript) {
    html = html.replace(/\s*<script defer src="\/dash\/js\/theme\.js[^"]*"><\/script>/, '');
  }
  return html;
}

function boot(stored, options = {}) {
  const dom = new JSDOM(assembleHTML(options), {
    url: 'http://127.0.0.1:8471/',
    runScripts: 'dangerously',
    resources: { interceptors: [dashScripts] },
    pretendToBeVisual: true,
    beforeParse(win) {
      if (stored) {
        win.localStorage.setItem(STORAGE_KEY, typeof stored === 'string' ? stored : JSON.stringify(stored));
      }
      // The browser APIs the dashboard touches but jsdom does not implement.
      win.fetch = () => new Promise(() => {});
      win.console.error = () => {};
      win.matchMedia = query => ({
        matches: false, media: query, onchange: null,
        addListener() {}, removeListener() {},
        addEventListener() {}, removeEventListener() {},
        dispatchEvent() { return false; },
      });
      win.ResizeObserver = class { observe() {} unobserve() {} disconnect() {} };
    },
  });
  return dom;
}

function loaded(dom) {
  return new Promise(resolve => dom.window.addEventListener('load', resolve));
}

async function bootChecks(context) {
  const { check, win, d, root, token } = context;
  const reloaded = boot({ theme: 'midnight', mode: 'clear' });
  await loaded(reloaded);
  const rw = reloaded.window;
  const rd = rw.document;
  check('a stored pair applies before first paint',
    rd.documentElement.dataset.theme === 'midnight' && rd.documentElement.dataset.mode === 'clear');
  check('the stored pair resolves its surface', token(rw, '--bg') === '#eaf1fb');
  check('the stored pair resolves its accent', token(rw, '--accent') === '#075985');
  check('the stored pair marks the palette row',
    rd.querySelector('[data-theme-id="midnight"]').getAttribute('aria-checked') === 'true');
  check('the stored pair marks the surface row',
    rd.querySelector('[data-mode-id="clear"]').getAttribute('aria-checked') === 'true');

  // The inline guard alone must publish the stored pair's rail to browser
  // chrome before theme.js runs. Excluding theme.js proves the meta update is
  // owned by the guard, not by the later syncThemeColor pass.
  const guardOnly = boot({ theme: 'forest', mode: 'clear' }, { excludeThemeScript: true });
  await loaded(guardOnly);
  const gd = guardOnly.window.document;
  check('the inline guard alone applies the stored pair',
    gd.documentElement.dataset.theme === 'forest' &&
    gd.documentElement.dataset.mode === 'clear' &&
    gd.querySelectorAll('.theme-opt').length === 0);
  check('the inline guard alone publishes the palette rail as the browser theme color',
    gd.querySelector('meta[name="theme-color"]').getAttribute('content') === '#e6f1e8');
  guardOnly.window.close();

  const legacy = boot('ember');
  await loaded(legacy);
  check('a legacy string normalizes to the default surface',
    legacy.window.document.documentElement.dataset.theme === 'ember' &&
    legacy.window.document.documentElement.dataset.mode === 'black');

  const invalid = boot({ theme: 'bogus', mode: 'sideways' });
  await loaded(invalid);
  check('an unknown pair normalizes to instrument and black',
    invalid.window.document.documentElement.dataset.theme === 'instrument' &&
    invalid.window.document.documentElement.dataset.mode === 'black');
  check('an unknown pair resolves the canonical surface after load',
    token(invalid.window, '--bg') === '#141210' && token(invalid.window, '--text') === '#e8e4de');
  check('an unknown pair repairs the stored value',
    invalid.window.localStorage.getItem(STORAGE_KEY) ===
      JSON.stringify({ theme: 'instrument', mode: 'black' }));

  const mixed = boot({ theme: 'forest', mode: 'sideways' });
  await loaded(mixed);
  check('a valid palette with an invalid surface keeps the palette',
    mixed.window.document.documentElement.dataset.theme === 'forest' &&
    mixed.window.document.documentElement.dataset.mode === 'black');
  check('a valid palette with an invalid surface repairs the stored pair',
    mixed.window.localStorage.getItem(STORAGE_KEY) ===
      JSON.stringify({ theme: 'forest', mode: 'black' }));

  // Defensive CSS contract: the inline guard's id whitelist means an unknown
  // stored palette is not applied, but the stylesheets must still resolve a
  // readable family if a subtree carries an unknown data-theme with a valid
  // mode. The mode block owns the fallback surfaces and accents.
  root.dataset.theme = 'bogus';
  root.dataset.mode = 'clear';
  const fallbackBg = token(win, '--bg');
  const fallbackText = token(win, '--text');
  const fallbackAccents = {
    '--accent': '#1e40af',
    '--accent-hover': '#1e3a8a',
    '--accent2': '#5b21b6',
    '--cyan': '#0b5f66',
  };
  check('an unknown palette falls back to the Clear surface before normalization',
    fallbackBg === '#f7f2e8' && fallbackText === '#2a241a');
  check('an unknown palette falls back to all Clear accent roles before normalization',
    Object.entries(fallbackAccents).every(([name, value]) => token(win, name) === value));
  check('the pre-normalization fallback clears the text contrast floor',
    contrast(fallbackText, fallbackBg) >= 4.5 &&
    contrast(token(win, '--accent'), fallbackBg) >= 4.5);
  root.dataset.theme = 'instrument';
  root.dataset.mode = 'black';

  // A blocked storage write must surface truthfully instead of keeping the
  // static "Saved in this browser" claim.
  {
    const realStorage = win.localStorage;
    Object.defineProperty(win, 'localStorage', {
      configurable: true,
      value: {
        getItem: () => null,
        setItem: () => { throw new Error('storage unavailable'); },
      },
    });
    try {
      d.querySelector('[data-mode-id="clear"]').click();
      check('a blocked storage write reports that the choice was not saved',
        d.getElementById('theme-saved')?.textContent === 'Not saved: browser storage unavailable');
      check('a blocked storage write leaves both panes on the committed pair',
        d.getElementById('theme-current-label')?.textContent === 'Current: Instrument / Clear' &&
        d.getElementById('theme-preview-label')?.textContent === 'Preview: same');
      win.syncThemeMenu();
      check('a blocked write stays Not saved when storage still lacks the pair',
        d.getElementById('theme-saved')?.textContent === 'Not saved: browser storage unavailable');
      Object.defineProperty(win, 'localStorage', {
        configurable: true,
        value: {
          getItem: () => JSON.stringify({ theme: 'instrument', mode: 'clear' }),
          setItem: () => {},
        },
      });
      win.syncThemeMenu();
      check('recovered storage with the matching pair restores the Saved status',
        d.getElementById('theme-saved')?.textContent === 'Saved in this browser');
    } finally {
      Object.defineProperty(win, 'localStorage', { configurable: true, value: realStorage });
    }
    root.dataset.theme = 'instrument';
    root.dataset.mode = 'black';
  }
  return [reloaded, legacy, invalid, mixed];
}

module.exports = { STATIC, STORAGE_KEY, boot, bootChecks, loaded };
