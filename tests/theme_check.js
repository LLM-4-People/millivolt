// Theme picker checks: surfaces and palettes resolve from the served
// stylesheets, the pair persists as one value, a switch recolors the canvas,
// and every theme/mode combination clears the contrast floor.
//
// The dashboard shell is assembled the way the browser sees it: stylesheets
// are inlined because jsdom does not fetch them from disk, while scripts keep
// their <script src="/dash/..."> tags and are loaded through a local resource
// loader. Building script elements as strings would make the fixture's own
// test data an HTML-injection sink, which CodeQL rightly rejects.
const fs = require('fs');
const path = require('path');
const { JSDOM, requestInterceptor } = require('jsdom');

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

function assembleHTML() {
  let html = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
  html = html.replaceAll('__DASHBOARD_VERSION__', TEST_DASHBOARD_VERSION);
  html = html.replace('__DASHBOARD_BOOTSTRAP__', '{"pending_revision":0}');
  html = html.replace(/<link\s+rel="stylesheet"\s+href="\/dash\/([^"]+)"\s*>/g, (_, ref) =>
    `<style>\n${fs.readFileSync(path.join(STATIC, ref.split('?')[0]), 'utf8')}\n</style>`);
  return html;
}

function boot(stored) {
  const dom = new JSDOM(assembleHTML(), {
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

function rgb(value) {
  const match = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(value.trim());
  if (!match) return null;
  return match.slice(1).map(part => parseInt(part, 16) / 255);
}

function luminance(value) {
  const channels = rgb(value);
  if (!channels) return null;
  const linear = channels.map(v => v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4);
  return .2126 * linear[0] + .7152 * linear[1] + .0722 * linear[2];
}

function contrast(a, b) {
  const la = luminance(a);
  const lb = luminance(b);
  if (la == null || lb == null) return 0;
  const [hi, lo] = la > lb ? [la, lb] : [lb, la];
  return (hi + .05) / (lo + .05);
}

function tint(color, surface, alpha) {
  const fg = rgb(color);
  const bg = rgb(surface);
  if (!fg || !bg) return surface;
  return '#' + fg.map((v, i) => Math.round((v * alpha + bg[i] * (1 - alpha)) * 255)
    .toString(16).padStart(2, '0')).join('');
}

function lab(value) {
  const channels = rgb(value);
  if (!channels) return null;
  const linear = channels.map(v => v <= .04045 ? v / 12.92 : ((v + .055) / 1.055) ** 2.4);
  const x = (linear[0] * .4124 + linear[1] * .3576 + linear[2] * .1805) / .95047;
  const y = linear[0] * .2126 + linear[1] * .7152 + linear[2] * .0722;
  const z = (linear[0] * .0193 + linear[1] * .1192 + linear[2] * .9505) / 1.08883;
  const f = v => v > .008856 ? Math.cbrt(v) : 7.787 * v + 16 / 116;
  return [116 * f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))];
}

function deltaE(a, b) {
  const left = lab(a);
  const right = lab(b);
  if (!left || !right) return 0;
  return Math.hypot(left[0] - right[0], left[1] - right[1], left[2] - right[2]);
}

let failures = 0;
const check = (label, cond) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + label);
  if (!cond) failures += 1;
};
const token = (win, name) =>
  win.getComputedStyle(win.document.documentElement).getPropertyValue(name).trim();

(async () => {
  const dom = boot();
  await loaded(dom);
  const win = dom.window;
  const d = win.document;
  const root = d.documentElement;
  const themes = [...d.querySelectorAll('.theme-opt')].map(option => option.dataset.themeId);
  const modes = [...d.querySelectorAll('.theme-mode-opt')].map(option => option.dataset.modeId);

  check('the picker offers every palette', themes.length === 8);
  check('the picker offers both surfaces', modes.length === 2);
  check('palette ids are unique', new Set(themes).size === themes.length);
  check('surface ids are unique', new Set(modes).size === modes.length);
  check('the storage key has one owner', d.querySelector('meta[name="theme-storage-key"]')?.content === STORAGE_KEY);

  check('the default palette is instrument', root.dataset.theme === 'instrument');
  check('the default surface is black', root.dataset.mode === 'black');
  check('the default resolves the canonical surface', token(win, '--bg') === '#141210');
  check('the default resolves the canonical accent', token(win, '--accent') === '#7aa0ff');
  const defaultSwatch = d.querySelector('[data-theme-id="instrument"] .theme-swatch');
  const swatchMirror = ['--accent', '--accent-hover', '--accent2', '--cyan']
    .filter(name => win.getComputedStyle(defaultSwatch).getPropertyValue(name).trim() !== token(win, name));
  check('the default swatch mirrors every canonical accent role', swatchMirror.length === 0);
  if (swatchMirror.length) console.log(swatchMirror.join('\n'));

  win.toggleThemeMenu({ stopPropagation() {} });
  check('the theme menu opens', !d.getElementById('theme-menu').hidden);
  check('the current palette is marked',
    d.querySelector('[data-theme-id="instrument"]').getAttribute('aria-pressed') === 'true');
  check('the current surface is marked',
    d.querySelector('[data-mode-id="black"]').getAttribute('aria-pressed') === 'true');
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
  check('switching recolors the canvas palette', win.eval('COLORS.accent') === '#1e40af');
  check('every swatch previews the chosen surface',
    [...d.querySelectorAll('.theme-swatch')].every(swatch => swatch.dataset.mode === 'clear'));
  check('the instrument swatch resolves the clear accent',
    win.getComputedStyle(d.querySelector('[data-theme-id="instrument"] .theme-swatch'))
      .getPropertyValue('--accent').trim() === '#1e40af');
  check('a non-active swatch resolves its own accent',
    win.getComputedStyle(d.querySelector('[data-theme-id="forest"] .theme-swatch'))
      .getPropertyValue('--accent').trim() === '#065f46');

  d.querySelector('[data-theme-id="forest"]').click();
  check('forest applies to the document', root.dataset.theme === 'forest');
  check('a palette keeps the chosen surface', root.dataset.mode === 'clear');
  check('forest persists with the surface',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'forest', mode: 'clear' }));
  check('forest swaps the accent tokens', token(win, '--accent') === '#065f46');
  check('a palette switch keeps the surface tokens', token(win, '--bg') === '#f7f2e8');
  check('the trigger names both choices',
    d.getElementById('btn-theme').getAttribute('aria-label') === 'Theme: Forest, Clear');
  check('switching a palette closes the menu', d.getElementById('theme-menu').hidden);

  win.toggleThemeMenu({ stopPropagation() {} });
  d.querySelector('[data-mode-id="black"]').click();
  check('black applies without changing the palette',
    root.dataset.theme === 'forest' && root.dataset.mode === 'black');
  check('black keeps the menu open', !d.getElementById('theme-menu').hidden);
  check('black restores the dark surface', token(win, '--bg') === '#141210');
  check('black resolves the forest accent', token(win, '--accent') === '#34d399');
  check('black persists with the palette',
    win.localStorage.getItem(STORAGE_KEY) === JSON.stringify({ theme: 'forest', mode: 'black' }));
  check('every swatch previews the black surface',
    [...d.querySelectorAll('.theme-swatch')].every(swatch => swatch.dataset.mode === 'black'));

  const reloaded = boot({ theme: 'midnight', mode: 'clear' });
  await loaded(reloaded);
  const rw = reloaded.window;
  const rd = rw.document;
  check('a stored pair applies before first paint',
    rd.documentElement.dataset.theme === 'midnight' && rd.documentElement.dataset.mode === 'clear');
  check('the stored pair resolves its surface', token(rw, '--bg') === '#f7f2e8');
  check('the stored pair resolves its accent', token(rw, '--accent') === '#075985');
  check('the stored pair marks the palette row',
    rd.querySelector('[data-theme-id="midnight"]').getAttribute('aria-pressed') === 'true');
  check('the stored pair marks the surface row',
    rd.querySelector('[data-mode-id="clear"]').getAttribute('aria-pressed') === 'true');

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

  // Contract: every theme has a block for both surfaces in the stylesheet.
  const themeCSS = fs.readFileSync(path.join(STATIC, 'css/themes.css'), 'utf8');
  const missing = [];
  for (const theme of themes) {
    for (const mode of modes) {
      const selector = `[data-theme="${theme}"][data-mode="${mode}"]`;
      if (!themeCSS.includes(selector)) missing.push(selector);
    }
  }
  check('every theme defines both surfaces', missing.length === 0);
  if (missing.length) console.log(missing.join('\n'));

  // A palette must be identifiable in each surface. Exact equality is not
  // enough: two accents can differ in hex yet read as the same color. Primary
  // accents must clear a CIE76 distance floor; secondary accents must at least
  // not duplicate one another. No accent role may reuse a semantic status hex.
  const paletteFailures = [];
  const statusRoles = ['ok', 'warn', 'err', 'rl', 'cyan'];
  for (const mode of modes) {
    const primary = new Map();
    const secondary = new Map();
    for (const theme of themes) {
      root.dataset.theme = theme;
      root.dataset.mode = mode;
      const accent = token(win, '--accent');
      const accent2 = token(win, '--accent2');
      for (const role of statusRoles) {
        const value = token(win, '--' + role);
        if (accent === value) paletteFailures.push(`${mode} ${theme} --accent equals --${role}`);
        if (accent2 === value) paletteFailures.push(`${mode} ${theme} --accent2 equals --${role}`);
      }
      for (const [other, value] of primary) {
        const distance = deltaE(accent, value);
        if (distance < 20) paletteFailures.push(`${mode} ${theme} and ${other} are ${distance.toFixed(1)} ΔE apart`);
      }
      if (secondary.has(accent2)) {
        paletteFailures.push(`${mode} ${theme} --accent2 duplicates ${secondary.get(accent2)}`);
      }
      for (const role of ['rl', 'err']) {
        const distance = deltaE(accent, token(win, '--' + role));
        if (distance < 20) paletteFailures.push(`${mode} ${theme} --accent is ${distance.toFixed(1)} ΔE from --${role}`);
      }
      primary.set(theme, accent);
      secondary.set(accent2, theme);
    }
  }
  check('each surface keeps every palette distinct from its peers and status', paletteFailures.length === 0);
  if (paletteFailures.length) console.log(paletteFailures.join('\n'));

  // Every theme/mode pair is exercised for the same text and status roles.
  const contrastFailures = [];
  const words = {
    bg: '--bg', panel: '--panel', panel2: '--panel2', rail: '--rail',
    text: '--text', muted: '--muted', accent: '--accent', accentHover: '--accent-hover',
    accent2: '--accent2', cyan: '--cyan', ok: '--ok', warn: '--warn', err: '--err',
    rl: '--rl', errText: '--err-text', warnText: '--warn-text', dangerHover: '--danger-hover',
  };
  const pair = (fg, bg, minimum = 4.5) => ({ fg, bg, minimum });
  const pairs = [
    pair('text', 'bg'), pair('text', 'panel'), pair('text', 'panel2'),
    pair('muted', 'bg'), pair('muted', 'panel'), pair('muted', 'panel2'),
    pair('accent', 'panel'), pair('accent', 'panel2'), pair('accent', 'bg'),
    pair('accentHover', 'bg'), pair('accent2', 'panel'), pair('accent2', 'panel2'),
    pair('cyan', 'panel'), pair('cyan', 'panel2'),
    pair('ok', 'panel'), pair('ok', 'panel2'), pair('ok', 'rail'),
    pair('warn', 'panel'), pair('warn', 'panel2'),
    pair('err', 'panel'), pair('err', 'panel2'), pair('err', 'bg'),
    pair('rl', 'panel'), pair('rl', 'panel2'),
    pair('errText', 'panel2'), pair('warnText', 'panel2'),
    pair('bg', 'dangerHover'),
  ];
  const tintPairs = [
    ['accent', 15], ['accent2', 16], ['cyan', 16],
    ['ok', 16], ['warn', 16], ['err', 16], ['rl', 16],
  ];
  for (const theme of themes) {
    for (const mode of modes) {
      root.dataset.theme = theme;
      root.dataset.mode = mode;
      const values = {};
      for (const [name, css] of Object.entries(words)) values[name] = token(win, css);
      for (const { fg, bg, minimum } of pairs) {
        const ratio = contrast(values[fg], values[bg]);
        if (ratio < minimum) {
          contrastFailures.push(`${theme}/${mode} ${fg} on ${bg}: ${ratio.toFixed(2)} < ${minimum}`);
        }
      }
      for (const [fg, alpha] of tintPairs) {
        const ratio = contrast(values[fg], tint(values[fg], values.panel2, alpha / 100));
        if (ratio < 4.5) {
          contrastFailures.push(`${theme}/${mode} ${fg} on ${alpha}% tint: ${ratio.toFixed(2)} < 4.5`);
        }
      }
    }
  }
  check('every theme and surface clears the contrast floor', contrastFailures.length === 0);
  if (contrastFailures.length) console.log(contrastFailures.join('\n'));

  win.close();
  reloaded.window.close();
  legacy.window.close();
  invalid.window.close();
  console.log(failures === 0 ? 'ALL THEME CHECKS PASSED' : `${failures} THEME CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch(error => {
  console.error('theme check crashed:', error);
  process.exit(1);
});
