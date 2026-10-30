// Theme picker checks: surfaces and palettes resolve from the served
// stylesheets, the pair persists as one value, a switch recolors the canvas,
// and every theme/mode combination clears the contrast floor.

const fs = require('fs');
const path = require('path');

const {
  STATIC,
  STORAGE_KEY,
  boot,
  bootChecks,
  loaded,
} = require('./theme_check_boot');
const { contrastChecks } = require('./theme_check_contrast');
const { pickerChecks } = require('./theme_check_picker');
const { surfaceChecks } = require('./theme_check_surfaces');

let failures = 0;
const check = (label, cond) => {
  console.log((cond ? 'PASS ' : 'FAIL ') + label);
  if (!cond) failures += 1;
};
const token = (win, name) =>
  win.getComputedStyle(win.document.documentElement).getPropertyValue(name).trim();
const elementToken = (win, element, name) =>
  win.getComputedStyle(element).getPropertyValue(name).trim();
// Exactly one checked radio and one tab stop per roving group; a group with
// no options is a failure too, never a vacuous pass.
const groupSelection = group => {
  const options = [...group.querySelectorAll('[role="radio"]')];
  return {
    total: options.length,
    checked: options.filter(option => option.getAttribute('aria-checked') === 'true').length,
    tabbable: options.filter(option => option.tabIndex === 0).length,
  };
};
const selectionIsSingle = (group, expected) => {
  const state = groupSelection(group);
  return state.total === expected && state.checked === 1 && state.tabbable === 1;
};

(async () => {
  const dom = boot();
  await loaded(dom);
  const win = dom.window;
  const d = win.document;
  const root = d.documentElement;
  const themes = [...d.querySelectorAll('.theme-opt')].map(option => option.dataset.themeId);
  const modes = [...d.querySelectorAll('.theme-mode-opt')].map(option => option.dataset.modeId);
  // The dashboard serves one stylesheet per surface module. The browser sees
  // their concatenation in link order, so parse the same corpus; themes.css
  // stays separate because it owns the palette layer.
  const indexHTML = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
  const dashboardCSS = [...indexHTML.matchAll(
    /<link\s+rel="stylesheet"\s+href="\/dash\/css\/([^"?]+)[^"]*"/g)]
    .map(match => match[1])
    .filter(name => name !== 'themes.css')
    .map(name => fs.readFileSync(path.join(STATIC, 'css', name), 'utf8'))
    .join('\n');
  const themeCSS = fs.readFileSync(path.join(STATIC, 'css/themes.css'), 'utf8');

  check('the picker offers every palette', themes.length === 8);
  check('the picker offers both surfaces', modes.length === 2);
  check('palette ids are unique', new Set(themes).size === themes.length);
  check('surface ids are unique', new Set(modes).size === modes.length);
  check('the storage key has one owner', d.querySelector('meta[name="theme-storage-key"]')?.content === STORAGE_KEY);

  check('the default palette is instrument', root.dataset.theme === 'instrument');
  check('the default surface is black', root.dataset.mode === 'black');
  check('the committed caption names the current pair',
    d.getElementById('theme-current-label')?.textContent === 'Current: Instrument / Black');
  check('the preview caption starts on the committed pair',
    d.getElementById('theme-preview-label')?.textContent === 'Preview: same');
  check('the inline guard lists exactly the registry ids', (() => {
    const themeSource = fs.readFileSync(path.join(STATIC, 'js/theme.js'), 'utf8');
    const registry = [...themeSource.matchAll(
      /\{ id: '([a-z-]+)', label: '[^']+', hint:/g)].map(match => match[1]);
    const guardSource = fs.readFileSync(path.join(STATIC, 'index.html'), 'utf8');
    const guard = /\/\^\(([^)]+)\)\$\/\.test\(saved\.theme\)/.exec(guardSource);
    const ids = guard ? guard[1].split('|') : [];
    return registry.length === 8 && ids.length === registry.length &&
      registry.every(id => ids.includes(id)) && ids.every(id => registry.includes(id));
  })());
  for (const id of themes) {
    const registryBoot = boot({ theme: id, mode: 'black' });
    await loaded(registryBoot);
    check(`the inline guard keeps the registry palette ${id}`,
      registryBoot.window.document.documentElement.dataset.theme === id);
    registryBoot.window.close();
  }
  const bogusBoot = boot({ theme: 'bogus', mode: 'black' });
  await loaded(bogusBoot);
  check('an unknown stored palette normalizes to instrument after load',
    bogusBoot.window.document.documentElement.dataset.theme === 'instrument' &&
    bogusBoot.window.localStorage.getItem(STORAGE_KEY) ===
      JSON.stringify({ theme: 'instrument', mode: 'black' }));
  bogusBoot.window.close();
  check('the default resolves the canonical surface', token(win, '--bg') === '#141210');
  check('the default resolves the canonical accent', token(win, '--accent') === '#7aa0ff');
  const defaultSwatch = d.querySelector('[data-theme-id="instrument"] .theme-swatch');
  const swatchMirror = ['--accent', '--accent-hover', '--accent2', '--cyan']
    .filter(name => win.getComputedStyle(defaultSwatch).getPropertyValue(name).trim() !== token(win, name));
  check('the default swatch mirrors every canonical accent role', swatchMirror.length === 0);
  if (swatchMirror.length) console.log(swatchMirror.join('\n'));

  const context = {
    check, win, d, root, themes, modes, dashboardCSS, themeCSS,
    token, elementToken, selectionIsSingle, STATIC, STORAGE_KEY,
  };
  await pickerChecks(context);
  const extraWindows = await bootChecks(context);
  const declaredContrast = surfaceChecks(context);
  contrastChecks(context, declaredContrast);

  win.close();
  for (const extra of extraWindows) extra.window.close();
  console.log(failures === 0 ? 'ALL THEME CHECKS PASSED' : `${failures} THEME CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch(error => {
  console.error('theme check crashed:', error);
  process.exit(1);
});
