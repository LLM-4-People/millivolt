// Theme picker checks: palettes resolve from the served stylesheets, the
// choice persists before first paint, and a switch recolors the canvas.
//
// The dashboard shell is assembled the way ui_check.js assembles it: jsdom
// does not fetch external assets, so every stylesheet and script referenced by
// index.html is inlined in document order.
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const STATIC = path.join(__dirname, '..', 'internal/web/static');
const TEST_DASHBOARD_VERSION = '0000000000000001';

function assembleHTML() {
  let html = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
  html = html.replaceAll('__DASHBOARD_VERSION__', TEST_DASHBOARD_VERSION);
  html = html.replace('__DASHBOARD_BOOTSTRAP__', '{"pending_revision":0}');
  html = html.replace(/<link\s+rel="stylesheet"\s+href="\/dash\/([^"]+)"\s*>/g, (_, ref) =>
    `<style>\n${fs.readFileSync(path.join(STATIC, ref.split('?')[0]), 'utf8')}\n</style>`);
  const scripts = [];
  html = html.replace(/<script\b[^>]*\bsrc="\/dash\/([^"]+)"[^>]*><\/script>/g, (_, ref) => {
    scripts.push(`<script>\n${fs.readFileSync(path.join(STATIC, ref.split('?')[0]), 'utf8')}\n</script>`);
    return '';
  });
  return html.replace('</body>', () => scripts.join('\n') + '\n</body>');
}

function boot(storedTheme) {
  const dom = new JSDOM(assembleHTML(), {
    url: 'http://127.0.0.1:8471/',
    runScripts: 'dangerously',
    pretendToBeVisual: true,
    beforeParse(win) {
      if (storedTheme) win.localStorage.setItem('millivolt.theme', storedTheme);
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

  check('the picker offers every palette', d.querySelectorAll('.theme-opt').length === 10);
  check('the default palette is instrument', !d.documentElement.dataset.theme);
  check('the default resolves the canonical surface', token(win, '--bg') === '#141210');

  win.toggleThemeMenu({ stopPropagation() {} });
  check('the theme menu opens', !d.getElementById('theme-menu').hidden);
  check('the current palette is marked',
    d.querySelector('[data-theme-id="instrument"]').getAttribute('aria-checked') === 'true');
  check('opening the theme menu sets aria-expanded',
    d.getElementById('btn-theme').getAttribute('aria-expanded') === 'true');

  d.querySelector('[data-theme-id="paper"]').click();
  check('paper applies to the document', d.documentElement.dataset.theme === 'paper');
  check('paper persists the choice', win.localStorage.getItem('millivolt.theme') === 'paper');
  check('paper swaps the surface tokens', token(win, '--bg') === '#f7f2e8');
  check('paper flips the native color scheme', token(win, 'color-scheme') === 'light');
  check('the browser theme color follows the palette',
    d.querySelector('meta[name="theme-color"]').getAttribute('content') === '#efe7d6');
  check('switching recolors the canvas palette', win.eval('COLORS.accent') === '#3056d3');
  check('switching closes the menu', d.getElementById('theme-menu').hidden);

  const reloaded = boot('midnight');
  await loaded(reloaded);
  const rw = reloaded.window;
  check('a stored palette applies before first paint',
    rw.document.documentElement.dataset.theme === 'midnight');
  check('the stored palette resolves its tokens', token(rw, '--bg') === '#070b14');
  check('the stored palette marks its picker row',
    rw.document.querySelector('[data-theme-id="midnight"]').getAttribute('aria-checked') === 'true');

  win.close();
  rw.close();
  console.log(failures === 0 ? 'ALL THEME CHECKS PASSED' : `${failures} THEME CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch(error => {
  console.error('theme check crashed:', error);
  process.exit(1);
});
