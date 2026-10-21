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
// Tokens owned by every palette block and by the bare Black mode fallback.
// Keep each list aligned with the stylesheet's documented ownership.
const SURFACE_ACCENT_TOKENS = ['--bg', '--panel', '--panel2', '--rail', '--border',
  '--border2', '--accent', '--accent-hover', '--accent2', '--cyan'];
// Palette-owned materials: each theme block carries its own elevation and
// scrim hue, so a cool Clear palette never inherits the warm Instrument soot.
const MATERIAL_TOKENS = ['--shadow-rgb', '--veil-rgb'];
const PALETTE_TOKENS = [...SURFACE_ACCENT_TOKENS, ...MATERIAL_TOKENS];
const MODE_TOKENS = ['--text', '--muted', '--ok', '--warn', '--err', '--rl',
  '--err-text', '--warn-text', '--danger-hover', '--veil', '--veil-strong',
  '--veil-soft', '--veil-rgb', '--shadow-rgb', '--neutral', '--scale-tick',
  '--scale-base', '--well-hi', '--well-lo', '--chart-grid-a'];

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

// Read one flat CSS rule by its selector list. The dashboard keeps every rule
// this contract reads without nested blocks, so a bounded declaration body is
// enough and avoids pulling in a CSS parser.
const cssSource = css => css.replace(/\/\*[\s\S]*?\*\//g, '');

// Every matching declaration body, in source order. Reading all of them lets
// cssProperties apply the cascade per property: a later `:root` block that
// only sets one token must not hide the earlier block's other tokens.
function cssRule(css, selectors) {
  const source = cssSource(css);
  const wanted = new Set(selectors);
  const bodies = [];
  for (const match of source.matchAll(/(?:^|\n)\s*([^{}]+?)\s*\{([^{}]*)\}/g)) {
    const selectorsHere = match[1].split(',').map(selector => selector.trim()).filter(Boolean);
    if (selectorsHere.length === wanted.size && selectorsHere.every(selector => wanted.has(selector))) {
      bodies.push(match[2]);
    }
  }
  return bodies.join('\n');
}

// Rewrite the last matching rule that actually contains the mutated
// declaration. This keeps a later duplicate's cascade win while still finding
// a token declared in an earlier block with the same selector.
function mutateCSSRule(css, selectors, mutate) {
  const source = cssSource(css);
  const wanted = new Set(selectors);
  const matches = [];
  for (const match of source.matchAll(/(?:^|\n)\s*([^{}]+?)\s*\{([^{}]*)\}/g)) {
    const selectorsHere = match[1].split(',').map(selector => selector.trim()).filter(Boolean);
    if (selectorsHere.length === wanted.size && selectorsHere.every(selector => wanted.has(selector))) {
      const open = match.index + match[0].indexOf('{') + 1;
      matches.push({ body: match[2], open, close: open + match[2].length });
    }
  }
  for (let index = matches.length - 1; index >= 0; index -= 1) {
    const found = matches[index];
    const body = mutate(found.body);
    if (body !== found.body) {
      return source.slice(0, found.open) + body + source.slice(found.close);
    }
  }
  return source;
}

function cssBlock(css, selector) {
  return cssRule(css, [selector]);
}

function cssProperties(body) {
  const declarations = new Map();
  for (const match of cssSource(body).matchAll(/([a-z0-9-]+)\s*:\s*([^;]+);/g)) {
    declarations.set(match[1], match[2].trim());
  }
  return declarations;
}

function cssDeclarations(body) {
  return new Map([...cssProperties(body)].filter(([name]) => name.startsWith('--')));
}

// The guarded rules own the alpha values and ink roles the contrast checks
// use. Parse them from the stylesheet instead of restating the values here, so
// a CSS edit cannot leave the test guarding a stale copy.
function cssTintPercentage(css, selector, role) {
  const background = cssProperties(cssBlock(css, selector)).get('background') || '';
  const match = new RegExp(`var\\(--${role}\\)\\s+([0-9.]+)%`).exec(background);
  return match ? parseFloat(match[1]) : NaN;
}

function cssInk(css, selector) {
  return cssProperties(cssBlock(css, selector)).get('color') || '';
}

// Read one color-mix background and resolve either a literal percentage or a
// percentage token declared in :root. The contract tests mutate the parsed
// declaration or token, never a copied constant.
function cssColorMix(css, selector) {
  const background = cssProperties(cssBlock(css, selector)).get('background') || '';
  const match = /^color-mix\(in srgb,\s*var\(--([a-z0-9-]+)\)\s+(?:([0-9.]+)%|var\(--([a-z0-9-]+)\)),\s*transparent\)$/
    .exec(background.trim());
  return {
    role: match ? match[1] : '',
    alpha: match && match[2] ? parseFloat(match[2]) : NaN,
    alphaToken: match && match[3] ? match[3] : '',
  };
}

function cssPercentToken(css, name) {
  if (!name) return NaN;
  const value = cssDeclarations(cssBlock(css, ':root')).get('--' + name) || '';
  const match = /^([0-9.]+)%$/.exec(value.trim());
  return match ? parseFloat(match[1]) : NaN;
}

function resolvedColorMix(css, selector) {
  const mix = cssColorMix(css, selector);
  return {
    role: mix.role,
    alpha: Number.isFinite(mix.alpha) ? mix.alpha : cssPercentToken(css, mix.alphaToken),
  };
}

function cssTokenReference(value) {
  const match = /^var\(--([a-z0-9-]+)\)$/.exec((value || '').trim());
  return match ? match[1] : '';
}

// Palette completeness reads declarations only. Comments are stripped first,
// so a commented-out token cannot satisfy the contract.
function paletteBlockErrors(css, themes, modes) {
  const missing = [];
  const seen = new Set();
  for (const match of cssSource(css).matchAll(
    /\[data-theme="([^"]+)"\]\[data-mode="([^"]+)"\]\s*\{([^}]*)\}/g)) {
    const [, theme, mode, body] = match;
    seen.add(`${theme}/${mode}`);
    for (const name of PALETTE_TOKENS) {
      if (!new RegExp(`${name}\\s*:`).test(body)) missing.push(`${theme}/${mode} ${name}`);
    }
  }
  for (const theme of themes) {
    for (const mode of modes) {
      if (!seen.has(`${theme}/${mode}`)) missing.push(`${theme}/${mode} block`);
    }
  }
  return missing;
}

const TEXT_INK = 'var(--text)';
const MUTED_INK = 'var(--muted)';
const ERR_INK = 'var(--err)';

const SELECTED_ENTITY_INK_SELECTORS = [
  '.xp-node.selected .xp-node-n small',
  '.xp-node.selected .xp-node-kpis',
  '.xp-node.selected .xp-p95',
  '.xp-node.selected .xp-node-signals',
  '.xp-node.selected .share-pct',
  '.xp-node.selected .xp-conversation-role',
];

function contrastContract(css) {
  const selected = resolvedColorMix(css, '.theme-opt[aria-checked="true"]');
  const cancel = resolvedColorMix(css, '.pill.cancel');
  const retryPill = resolvedColorMix(css, '.pill.err');
  return {
    dangerHoverTint: cssTintPercentage(css, '.btn-danger:hover', 'err'),
    dangerHoverInk: cssInk(css, '.btn-danger:hover'),
    dangerOutlineHoverTint: cssTintPercentage(css, '.btn-danger-outline:hover', 'err'),
    dangerOutlineHoverInk: cssInk(css, '.btn-danger-outline:hover'),
    retryIdleTint: cssTintPercentage(css, 'tr.retry-sub td', 'warn'),
    retryIdleInk: cssInk(css, 'tr.retry-sub td'),
    retryHoverTint: cssTintPercentage(css, 'tr.retry-sub:hover td', 'warn'),
    retryHoverInk: cssInk(css, 'tr.retry-sub:hover td'),
    retryPillRole: retryPill.role,
    retryPillTint: retryPill.alpha,
    // The nested err pill needs its own proven ink over the hovered warn row;
    // the row's text override does not reach a nested pill.
    retryPillHoverInk: cssInk(css, 'tr.retry-sub:hover td .pill.err'),
    cursorInk: cssProperties(cssBlock(css, '.uplot .u-over > .u-cursor-x')).get('border-color') || '',
    entityTint: cssTintPercentage(css, '.xp-node.selected', 'ent'),
    entityInk: cssProperties(cssRule(css, SELECTED_ENTITY_INK_SELECTORS)).get('color') || '',
    selectedRole: selected.role,
    selectedTint: selected.alpha,
    // The checked row owns its ink; reading the base `.theme-opt` rule would
    // miss a later selected-state override and could guard the wrong color.
    selectedInk: cssInk(css, '.theme-opt[aria-checked="true"]'),
    selectedHintInk: cssInk(css, '.theme-opt[aria-checked="true"] .theme-hint'),
    cancelRole: cancel.role,
    cancelTint: cancel.alpha,
    cancelInk: cssInk(css, '.pill.cancel'),
  };
}

function tintContractErrors(contract) {
  const errors = [];
  for (const [label, alpha] of [
    ['danger hover', contract.dangerHoverTint],
    ['danger outline hover', contract.dangerOutlineHoverTint],
    ['retry idle', contract.retryIdleTint],
    ['retry hover', contract.retryHoverTint],
    ['nested retry pill', contract.retryPillTint],
    ['selected entity', contract.entityTint],
    ['selected palette', contract.selectedTint],
    ['cancelled pill', contract.cancelTint],
  ]) {
    if (!Number.isFinite(alpha) || alpha <= 0 || alpha >= 100) {
      errors.push(`${label} tint must be a bounded percentage, got ${alpha}`);
    }
  }
  if (contract.cursorInk !== MUTED_INK) {
    errors.push(`chart cursor ink must be ${MUTED_INK}, got ${contract.cursorInk || 'none'}`);
  }
  if (contract.selectedRole !== 'accent') {
    errors.push(`selected palette tint must derive from --accent, got ${contract.selectedRole || 'none'}`);
  }
  if (contract.cancelRole !== 'neutral') {
    errors.push(`cancelled pill tint must derive from --neutral, got ${contract.cancelRole || 'none'}`);
  }
  if (contract.retryPillRole !== 'err') {
    errors.push(`nested retry pill tint must derive from --err, got --${contract.retryPillRole || 'none'}`);
  }
  return errors;
}

function inkContractErrors(contract) {
  const errors = [];
  for (const [label, ink, expected] of [
    ['selected entity', contract.entityInk, TEXT_INK],
    ['cancelled pill', contract.cancelInk, TEXT_INK],
    ['danger hover', contract.dangerHoverInk, ERR_INK],
    ['danger outline hover', contract.dangerOutlineHoverInk, ERR_INK],
    ['retry idle', contract.retryIdleInk, MUTED_INK],
    ['retry hover', contract.retryHoverInk, TEXT_INK],
    ['nested retry pill hover', contract.retryPillHoverInk, 'var(--err-text)'],
    ['chart cursor', contract.cursorInk, MUTED_INK],
    ['selected palette', contract.selectedInk, TEXT_INK],
    ['selected palette hint', contract.selectedHintInk, TEXT_INK],
  ]) {
    if (ink !== expected) {
      errors.push(`${label} ink must be ${expected}, got ${ink || 'none'}`);
    }
  }
  return errors;
}

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
  const dashboardCSS = fs.readFileSync(path.join(STATIC, 'css/dashboard.css'), 'utf8');
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

  // The live default is Instrument Black: :root in dashboard.css owns the
  // canonical values and themes.css mirrors them for the default swatch and a
  // Black preview. Compare every mirrored declaration, not a hand-picked
  // subset, so a :root edit cannot silently miss the live default.
  const declaredContrast = contrastContract(dashboardCSS);
  const tintErrors = tintContractErrors(declaredContrast);
  check('the guarded tint declarations resolve to bounded percentages',
    tintErrors.length === 0);
  if (tintErrors.length) console.log(tintErrors.join('\n'));
  const inkErrors = inkContractErrors(declaredContrast);
  check('the parsed contrast declarations name their ink roles exactly',
    inkErrors.length === 0);
  if (inkErrors.length) console.log(inkErrors.join('\n'));
  check('the selected entity card overrides its muted rows to the text token',
    declaredContrast.entityInk === TEXT_INK);
  check('the cancelled pill wears the text token over its neutral tint',
    declaredContrast.cancelInk === TEXT_INK);
  const rootTokens = cssDeclarations(cssBlock(dashboardCSS, ':root'));
  const blackModeTokens = cssDeclarations(cssBlock(themeCSS, '[data-mode="black"]'));
  const instrumentBlackTokens = cssDeclarations(cssBlock(themeCSS, '[data-theme="instrument"][data-mode="black"]'));
  const defaultTokenDrift = [];
  for (const [name, value] of [...blackModeTokens, ...instrumentBlackTokens]) {
    if (rootTokens.get(name) !== value) {
      defaultTokenDrift.push(`${name}: ${value} != live ${rootTokens.get(name)}`);
    }
  }
  check('the default Black mirrors match the live :root tokens',
    rootTokens.size > 0 && blackModeTokens.size > 0 && instrumentBlackTokens.size > 0 &&
    defaultTokenDrift.length === 0);
  if (defaultTokenDrift.length) console.log(defaultTokenDrift.join('\n'));

  // Matching values are not enough: the bare Black block is the mode-owned
  // fallback inside a Clear page, so it must own every mode token instead of
  // inheriting Clear ink from dashboard.css. Instrument Black must likewise
  // own the complete surface and accent family that identifies the palette.
  const missingModeTokens = MODE_TOKENS.filter(name => !blackModeTokens.has(name));
  check('the default Black mode block owns every mode token',
    missingModeTokens.length === 0);
  if (missingModeTokens.length) console.log(missingModeTokens.join('\n'));
  const blackSurfaceDrift = SURFACE_ACCENT_TOKENS
    .filter(name => blackModeTokens.get(name) !== instrumentBlackTokens.get(name));
  check('the bare Black fallback matches Instrument Black surfaces and accents',
    SURFACE_ACCENT_TOKENS.every(name => blackModeTokens.has(name)) &&
    blackSurfaceDrift.length === 0);
  if (blackSurfaceDrift.length) console.log(blackSurfaceDrift.join('\n'));
  const missingInstrumentTokens = PALETTE_TOKENS
    .filter(name => !instrumentBlackTokens.has(name));
  check('Instrument Black owns the complete surface, accent and material family',
    missingInstrumentTokens.length === 0);
  if (missingInstrumentTokens.length) console.log(missingInstrumentTokens.join('\n'));

  // Clear is the other mode-owned fallback. Its bare block must own every
  // surface, accent and mode token so an unknown palette or a Clear preview
  // inside a Black page never inherits the committed dark ink.
  const clearModeTokens = cssDeclarations(cssBlock(themeCSS, '[data-mode="clear"]'));
  const missingClear = [...SURFACE_ACCENT_TOKENS, ...MODE_TOKENS]
    .filter(name => !clearModeTokens.has(name));
  check('the Clear mode block owns every surface, accent and mode token',
    clearModeTokens.size > 0 && missingClear.length === 0);
  if (missingClear.length) console.log(missingClear.join('\n'));
  const instrumentClearTokens = cssDeclarations(
    cssBlock(themeCSS, '[data-theme="instrument"][data-mode="clear"]'));
  const clearSurfaceDrift = PALETTE_TOKENS
    .filter(name => clearModeTokens.get(name) !== instrumentClearTokens.get(name));
  check('the bare Clear fallback equals Instrument Clear surfaces, accents and materials',
    PALETTE_TOKENS.every(name =>
      clearModeTokens.has(name) && instrumentClearTokens.has(name)) &&
    clearSurfaceDrift.length === 0);
  if (clearSurfaceDrift.length) console.log(clearSurfaceDrift.join('\n'));

  const fallbackFamily = element => {
    const styles = win.getComputedStyle(element);
    return [...PALETTE_TOKENS, ...MODE_TOKENS]
      .filter(name => !styles.getPropertyValue(name).trim());
  };
  const fallbackContrast = element => {
    const styles = win.getComputedStyle(element);
    const value = name => styles.getPropertyValue(name).trim();
    const failures = [];
    for (const [fg, bg] of [['--text', '--bg'], ['--text', '--panel'],
      ['--muted', '--bg'], ['--muted', '--panel'],
      ['--ok', '--panel'], ['--warn', '--panel'], ['--err', '--panel']]) {
      const ratio = contrast(value(fg), value(bg));
      if (ratio < 4.5) failures.push(`${fg} on ${bg}: ${ratio.toFixed(2)}`);
    }
    return failures;
  };
  const clearFallbacks = [];
  root.removeAttribute('data-theme');
  root.dataset.mode = 'clear';
  clearFallbacks.push(...fallbackFamily(root).map(name => `bare-clear missing ${name}`));
  clearFallbacks.push(...fallbackContrast(root).map(item => `bare-clear ${item}`));
  root.dataset.theme = 'bogus';
  clearFallbacks.push(...fallbackFamily(root).map(name => `unknown-theme missing ${name}`));
  clearFallbacks.push(...fallbackContrast(root).map(item => `unknown-theme ${item}`));
  // A Clear preview stage inside the committed Black page must resolve the
  // same complete fallback family without inheriting dark ink.
  root.dataset.theme = 'instrument';
  root.dataset.mode = 'black';
  stage.dataset.theme = 'bogus';
  stage.dataset.mode = 'clear';
  clearFallbacks.push(...fallbackFamily(stage).map(name => `stage missing ${name}`));
  clearFallbacks.push(...fallbackContrast(stage).map(item => `stage ${item}`));
  stage.dataset.theme = 'instrument';
  stage.dataset.mode = 'black';
  check('bare-clear, unknown-theme and stage fallbacks resolve the full readable family',
    clearFallbacks.length === 0);
  if (clearFallbacks.length) console.log(clearFallbacks.join('\n'));

  // The mirror-image fallback: a bogus-theme Black subtree inside a committed
  // Clear page resolves the bare Black family, which must equal Instrument
  // Black rather than inheriting Clear surfaces.
  const blackFallbacks = [];
  root.dataset.theme = 'instrument';
  root.dataset.mode = 'clear';
  stage.dataset.theme = 'bogus';
  stage.dataset.mode = 'black';
  blackFallbacks.push(...fallbackFamily(stage).map(name => `black-stage missing ${name}`));
  blackFallbacks.push(...fallbackContrast(stage).map(item => `black-stage ${item}`));
  for (const name of PALETTE_TOKENS) {
    if (elementToken(win, stage, name) !== instrumentBlackTokens.get(name)) {
      blackFallbacks.push(`black-stage ${name} != Instrument Black`);
    }
  }
  stage.dataset.theme = 'instrument';
  stage.dataset.mode = 'black';
  root.dataset.theme = 'instrument';
  root.dataset.mode = 'black';
  check('a bogus-theme Black subtree inside Clear resolves Instrument Black',
    blackFallbacks.length === 0);
  if (blackFallbacks.length) console.log(blackFallbacks.join('\n'));

  // Contract: every theme defines both surfaces with the complete surface,
  // accent and material family, not just an accent override. Comments are
  // stripped first.
  const missing = paletteBlockErrors(themeCSS, themes, modes);
  check('every theme defines the complete surface, accent and material family', missing.length === 0);
  if (missing.length) console.log(missing.join('\n'));
  const commentedCyan = themeCSS.replace(
    /(\[data-theme="instrument"\]\[data-mode="black"\]\s*\{[^}]*?)(--cyan:[^;]+;)/,
    (_, head, declaration) => head + '/* ' + declaration + ' */');
  const commentedMissing = paletteBlockErrors(commentedCyan, themes, modes);
  check('a commented-out palette token fails the completeness contract',
    commentedCyan !== themeCSS &&
    commentedMissing.includes('instrument/black --cyan'));
  const commentedMaterial = themeCSS.replace(
    /(\[data-theme="instrument"\]\[data-mode="black"\]\s*\{[^}]*?)(--veil-rgb:[^;]+;)/,
    (_, head, declaration) => head + '/* ' + declaration + ' */');
  const commentedMaterialMissing = paletteBlockErrors(commentedMaterial, themes, modes);
  check('a commented-out material token fails the completeness contract',
    commentedMaterial !== themeCSS &&
    commentedMaterialMissing.includes('instrument/black --veil-rgb'));

  // Every theme's material declaration must be the value the browser resolves
  // for that pair, and the Clear mode's veils must compose that palette hue.
  const materialFailures = [];
  const normalized = value => (value || '').replace(/\s+/g, '');
  for (const mode of modes) {
    for (const theme of themes) {
      const selector = `[data-theme="${theme}"][data-mode="${mode}"]`;
      const block = cssDeclarations(cssBlock(themeCSS, selector));
      root.dataset.theme = theme;
      root.dataset.mode = mode;
      for (const name of MATERIAL_TOKENS) {
        const declared = block.get(name);
        const resolved = token(win, name);
        if (!declared) materialFailures.push(`${theme}/${mode} ${name} undeclared`);
        else if (normalized(declared) !== normalized(resolved)) {
          materialFailures.push(`${theme}/${mode} ${name} ${declared} != resolved ${resolved}`);
        }
      }
    }
  }
  check('every palette resolves its own material tokens', materialFailures.length === 0);
  if (materialFailures.length) console.log(materialFailures.join('\n'));
  const clearModeVeils = ['--veil', '--veil-strong', '--veil-soft']
    .filter(name => !/^rgba\(var\(--veil-rgb\),\s*[0-9.]+\)$/.test(clearModeTokens.get(name) || ''));
  check('the Clear mode veils compose the palette-owned veil-rgb', clearModeVeils.length === 0);
  if (clearModeVeils.length) console.log(clearModeVeils.join('\n'));
  const clearMaterialValues = [];
  for (const theme of themes) {
    root.dataset.theme = theme;
    root.dataset.mode = 'clear';
    clearMaterialValues.push(`${token(win, '--shadow-rgb')}|${token(win, '--veil-rgb')}`);
  }
  check('Clear materials are palette-owned rather than one shared warm pair',
    new Set(clearMaterialValues).size === themes.length);

  // A palette must be identifiable in each surface. Exact equality is not
  // enough: two accents can differ in hex yet read as the same color. Primary
  // accents must clear a CIE76 distance floor; the three accent roles must stay
  // perceptually apart inside one palette and exactly unique across palettes.
  // No accent role may reuse a semantic status hex.
  const paletteFailures = [];
  const statusRoles = ['ok', 'warn', 'err', 'rl', 'cyan'];
  const accentRoles = ['accent', 'accent2', 'cyan'];
  // Cyan is an accent role and a plot series, never a status color. Exact
  // collisions and near-identical hues against ok/warn/err/rl make a series
  // indistinguishable from a semantic state at a glance.
  const CYAN_STATUS_ROLES = ['ok', 'warn', 'err', 'rl'];
  const cyanStatusFailures = (mode, theme, values) => {
    const failures = [];
    for (const role of CYAN_STATUS_ROLES) {
      if (values.cyan === values[role]) {
        failures.push(`${mode} ${theme} --cyan equals --${role}`);
      } else {
        const distance = deltaE(values.cyan, values[role]);
        if (distance < 8) {
          failures.push(`${mode} ${theme} --cyan is ${distance.toFixed(1)} ΔE from --${role}`);
        }
      }
    }
    return failures;
  };
  for (const mode of modes) {
    const primary = new Map();
    const secondary = new Map();
    const roleValues = new Map(accentRoles.map(role => [role, new Map()]));
    for (const theme of themes) {
      root.dataset.theme = theme;
      root.dataset.mode = mode;
      const accent = token(win, '--accent');
      const accent2 = token(win, '--accent2');
      const values = {
        accent,
        accent2,
        cyan: token(win, '--cyan'),
      };
      const statusValues = {};
      for (const role of statusRoles) {
        statusValues[role] = token(win, '--' + role);
        const value = statusValues[role];
        if (role !== 'cyan' && accent === value) paletteFailures.push(`${mode} ${theme} --accent equals --${role}`);
        if (role !== 'cyan' && accent2 === value) paletteFailures.push(`${mode} ${theme} --accent2 equals --${role}`);
      }
      paletteFailures.push(...cyanStatusFailures(mode, theme,
        { cyan: values.cyan, ...statusValues }));
      for (let left = 0; left < accentRoles.length; left += 1) {
        for (let right = left + 1; right < accentRoles.length; right += 1) {
          const first = accentRoles[left];
          const second = accentRoles[right];
          const distance = deltaE(values[first], values[second]);
          if (distance < 8) {
            paletteFailures.push(`${mode} ${theme} --${first} and --${second} are ${distance.toFixed(1)} ΔE apart`);
          }
        }
      }
      for (const role of accentRoles) {
        const seen = roleValues.get(role);
        if (seen.has(values[role])) {
          paletteFailures.push(`${mode} --${role} ${values[role]} is shared by ${theme} and ${seen.get(values[role])}`);
        } else {
          seen.set(values[role], theme);
        }
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
  check('each surface keeps every palette distinct from its peers, roles and status', paletteFailures.length === 0);
  if (paletteFailures.length) console.log(paletteFailures.join('\n'));
  {
    // Mutation proof: the cyan guard must reject a palette that reuses the ok
    // hue. Forest Black is the live fixture; substituting its cyan with its
    // own --ok leaves every other declaration untouched.
    root.dataset.theme = 'forest';
    root.dataset.mode = 'black';
    const live = { cyan: token(win, '--cyan') };
    for (const role of CYAN_STATUS_ROLES) live[role] = token(win, '--' + role);
    check('substituting Forest cyan with --ok fails the cyan status guard',
      cyanStatusFailures('black', 'forest', live).length === 0 &&
      cyanStatusFailures('black', 'forest', { ...live, cyan: live.ok }).length > 0);
  }

  // Surface families must differ too. Every one of the six roles must be
  // unique across palettes in each mode; comparing only bg/panel let a
  // palette ship a duplicate rail, border or secondary panel unnoticed.
  const surfaceFailures = [];
  const surfaceRoles = ['bg', 'panel', 'panel2', 'rail', 'border', 'border2'];
  for (const mode of modes) {
    for (const role of surfaceRoles) {
      const seen = new Map();
      for (const theme of themes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const value = token(win, '--' + role);
        if (seen.has(value)) {
          surfaceFailures.push(`${mode} ${theme} and ${seen.get(value)} share --${role} ${value}`);
        } else {
          seen.set(value, theme);
        }
      }
    }
  }
  check('six surface roles unique across palettes', surfaceFailures.length === 0);
  if (surfaceFailures.length) console.log(surfaceFailures.join('\n'));

  // Within one pair, every surface role must be its own step: a rail equal to
  // panel2 (Ember Clear shipped that) erases the rail/panel hierarchy. The
  // Contrast palette deliberately collapses roles for maximum separation, so
  // it is the documented exception.
  const withinSurfaceFailures = [];
  for (const mode of modes) {
    for (const theme of themes) {
      if (theme === 'contrast') continue;
      root.dataset.theme = theme;
      root.dataset.mode = mode;
      const seen = new Map();
      for (const role of surfaceRoles) {
        const value = token(win, '--' + role);
        if (seen.has(value)) {
          withinSurfaceFailures.push(`${mode} ${theme} --${role} duplicates --${seen.get(value)} ${value}`);
        } else {
          seen.set(value, role);
        }
      }
    }
  }
  check('non-Contrast palettes keep every surface role distinct within the pair',
    withinSurfaceFailures.length === 0);
  if (withinSurfaceFailures.length) console.log(withinSurfaceFailures.join('\n'));

  // Every theme/mode pair is exercised for the same text and status roles.
  const contrastFailures = [];
  const words = {
    bg: '--bg', panel: '--panel', panel2: '--panel2', rail: '--rail',
    text: '--text', muted: '--muted', accent: '--accent', accentHover: '--accent-hover',
    accent2: '--accent2', cyan: '--cyan', ok: '--ok', warn: '--warn', err: '--err',
    rl: '--rl', errText: '--err-text', warnText: '--warn-text', dangerHover: '--danger-hover',
    neutral: '--neutral',
  };
  const pair = (fg, bg, minimum = 4.5) => ({ fg, bg, minimum });
  const pairs = [
    pair('text', 'bg'), pair('text', 'panel'), pair('text', 'panel2'), pair('text', 'rail'),
    pair('muted', 'bg'), pair('muted', 'panel'), pair('muted', 'panel2'), pair('muted', 'rail'),
    pair('accent', 'panel'), pair('accent', 'panel2'), pair('accent', 'bg'), pair('accent', 'rail'),
    pair('accentHover', 'bg'), pair('accent2', 'panel'), pair('accent2', 'panel2'), pair('accent2', 'rail'),
    pair('cyan', 'panel'), pair('cyan', 'panel2'), pair('cyan', 'rail'),
    pair('bg', 'accent'), pair('bg', 'accentHover'),
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
  const entitiesSource = fs.readFileSync(path.join(STATIC, 'js/entities.js'), 'utf8');
  const entityEntries = [...entitiesSource.matchAll(
    /\b([a-z]+):\s*\{ label: '[^']+',\s*color: '(#[0-9A-Fa-f]{6})'/g)];
  const entityNames = entityEntries.map(match => match[1]);
  const entityColors = entityEntries.map(match => match[2]);
  const entityInk = cssTokenReference(declaredContrast.entityInk);
  check('the entity registry declares exactly ten unique colors',
    entityNames.join(',') === 'client,provider,model,conversation,key,status,time,tool,request,error' &&
    entityColors.length === 10 && new Set(entityColors).size === 10);
  const inkValue = (values, ink) => values[cssTokenReference(ink)] || '';
  const dangerTintFailures = contract => {
    const failures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const err = token(win, '--err');
        const panel = token(win, '--panel');
        for (const [label, alpha, ink] of [
          ['danger hover', contract.dangerHoverTint, contract.dangerHoverInk],
          ['danger outline hover', contract.dangerOutlineHoverTint, contract.dangerOutlineHoverInk],
        ]) {
          const fg = inkValue({ err, text: token(win, '--text'), muted: token(win, '--muted') }, ink);
          const ratio = contrast(fg, tint(err, panel, alpha / 100));
          if (!fg || ratio < 4.5) {
            failures.push(`${theme}/${mode} ${ink || 'none'} on ${alpha}% err (${label}): ${ratio.toFixed(2)} < 4.5`);
          }
        }
      }
    }
    return failures;
  };
  const retryTintFailures = contract => {
    const failures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const values = {
          warn: token(win, '--warn'), panel: token(win, '--panel'),
          muted: token(win, '--muted'), text: token(win, '--text'),
        };
        const idle = contrast(inkValue(values, contract.retryIdleInk),
          tint(values.warn, values.panel, contract.retryIdleTint / 100));
        const hover = contrast(inkValue(values, contract.retryHoverInk),
          tint(values.warn, values.panel, contract.retryHoverTint / 100));
        if (!Number.isFinite(idle) || !Number.isFinite(hover) || idle < 4.5 || hover < 4.5) {
          failures.push(`${theme}/${mode} retry sub-row idle ${idle.toFixed(2)}/hover ${hover.toFixed(2)} < 4.5`);
        }
      }
    }
    return failures;
  };
  const cursorTintFailures = contract => {
    const failures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const ink = inkValue({ muted: token(win, '--muted'), bg: token(win, '--bg'),
          panel: token(win, '--panel'), text: token(win, '--text') }, contract.cursorInk);
        const ratio = contrast(ink, token(win, '--panel'));
        if (!Number.isFinite(ratio) || ratio < 3) {
          failures.push(`${theme}/${mode} ${contract.cursorInk || 'none'} on panel: ${ratio.toFixed(2)} < 3`);
        }
      }
    }
    return failures;
  };
  const entityTintFailures = contract => {
    const failures = [];
    const mutedFailures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const values = {
          panel2: token(win, '--panel2'), muted: token(win, '--muted'),
          text: token(win, '--text'),
        };
        for (const color of entityColors) {
          const background = tint(color, values.panel2, contract.entityTint / 100);
          const ratio = contrast(inkValue(values, contract.entityInk), background);
          if (!Number.isFinite(ratio) || ratio < 4.5) {
            failures.push(`${theme}/${mode} ${contract.entityInk || 'none'} on ${color} ${contract.entityTint}% tint: ${ratio.toFixed(2)} < 4.5`);
          }
          const muted = contrast(values.muted, background);
          if (muted < 4.5) mutedFailures.push(`${theme}/${mode} muted on ${color}`);
        }
      }
    }
    return { failures, mutedFailures };
  };
  const selectedCompositeFailures = contract => {
    const failures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const values = {
          accent: token(win, '--accent'), panel: token(win, '--panel'),
          muted: token(win, '--muted'), text: token(win, '--text'),
        };
        const background = tint(values[contract.selectedRole], values.panel,
          contract.selectedTint / 100);
        for (const [label, ink] of [
          ['row', contract.selectedInk],
          ['hint', contract.selectedHintInk],
        ]) {
          const ratio = contrast(inkValue(values, ink), background);
          if (!Number.isFinite(ratio) || ratio < 4.5) {
            failures.push(`${theme}/${mode} selected ${label} ${ink || 'none'}: ${ratio.toFixed(2)} < 4.5`);
          }
        }
      }
    }
    return failures;
  };
  const cancelCompositeFailures = contract => {
    const failures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const values = {
          neutral: token(win, '--neutral'), panel: token(win, '--panel'),
          panel2: token(win, '--panel2'), text: token(win, '--text'),
          muted: token(win, '--muted'),
        };
        for (const surface of ['panel', 'panel2']) {
          const ratio = contrast(inkValue(values, contract.cancelInk),
            tint(values.neutral, values[surface], contract.cancelTint / 100));
          if (!Number.isFinite(ratio) || ratio < 4.5) {
            failures.push(`${theme}/${mode} ${contract.cancelInk || 'none'} on ${contract.cancelTint}% neutral over ${surface}: ${ratio.toFixed(2)} < 4.5`);
          }
        }
      }
    }
    return failures;
  };
  // The nested err pill composites its own 13% err tint over the retry row,
  // which itself sits on the 4%/10% warn tint. The idle ink is --err; the
  // hovered row needs --err-text, proven per palette and surface.
  const nestedRetryPillFailures = contract => {
    const failures = [];
    for (const theme of themes) {
      for (const mode of modes) {
        root.dataset.theme = theme;
        root.dataset.mode = mode;
        const values = {
          err: token(win, '--err'), 'err-text': token(win, '--err-text'),
          warn: token(win, '--warn'), panel: token(win, '--panel'),
        };
        for (const [label, rowTint, ink] of [
          ['idle', contract.retryIdleTint, 'var(--err)'],
          ['hover', contract.retryHoverTint, contract.retryPillHoverInk],
        ]) {
          const row = tint(values.warn, values.panel, rowTint / 100);
          const pill = tint(values.err, row, contract.retryPillTint / 100);
          const ratio = contrast(inkValue(values, ink), pill);
          if (!Number.isFinite(ratio) || ratio < 4.5) {
            failures.push(`${theme}/${mode} ${ink || 'none'} on ${contract.retryPillTint}% err over ${rowTint}% warn (${label}): ${ratio.toFixed(2)} < 4.5`);
          }
        }
      }
    }
    return failures;
  };
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
  const dangerFailures = dangerTintFailures(declaredContrast);
  const retryFailures = retryTintFailures(declaredContrast);
  const cursorFailures = cursorTintFailures(declaredContrast);
  const entityResult = entityTintFailures(declaredContrast);
  const selectedFailures = selectedCompositeFailures(declaredContrast);
  const cancelFailures = cancelCompositeFailures(declaredContrast);
  const nestedRetryFailures = nestedRetryPillFailures(declaredContrast);
  check('every theme and surface clears the contrast floor', contrastFailures.length === 0);
  if (contrastFailures.length) console.log(contrastFailures.join('\n'));
  check('the chart cursor keeps a 3:1 boundary on the panel',
    cursorFailures.length === 0);
  if (cursorFailures.length) console.log(cursorFailures.join('\n'));
  check('danger hover and outline hover keep their parsed error ink readable',
    dangerFailures.length === 0);
  if (dangerFailures.length) console.log(dangerFailures.join('\n'));
  check('retry sub-row idle and hover ink clear the contrast floor',
    retryFailures.length === 0);
  if (retryFailures.length) console.log(retryFailures.join('\n'));
  check('the registry entity tint keeps selected-card ink readable',
    entityColors.length === 10 && entityResult.failures.length === 0);
  if (entityResult.failures.length) console.log(entityResult.failures.join('\n'));
  if (entityResult.mutedFailures.length) {
    console.log('selected-card muted ink (overridden to --text): ' +
      entityResult.mutedFailures.length + ' tint combinations below 4.5');
  }
  check('selected palette and cancelled pill tint composites keep their parsed ink readable',
    selectedFailures.length === 0 && cancelFailures.length === 0);
  if (selectedFailures.length || cancelFailures.length) {
    console.log(selectedFailures.concat(cancelFailures).join('\n'));
  }
  check('the nested retry pill keeps its ink readable over the warn row',
    nestedRetryFailures.length === 0);
  if (nestedRetryFailures.length) console.log(nestedRetryFailures.join('\n'));

  // Mutation proof: the parsed declarations, not copied constants, must feed
  // the checks above. A temporary in-memory edit to each guarded declaration
  // must fail the check that owns it.
  const raisedDangerTint = contrastContract(mutateCSSRule(dashboardCSS,
    ['.btn-danger:hover'],
    body => body.replace(/(var\(--err\)\s+)[0-9.]+%/, (_, prefix) => prefix + '28%')));
  check('raising the danger hover tint to 28% fails the contrast guard',
    raisedDangerTint.dangerHoverTint === 28 && dangerTintFailures(raisedDangerTint).length > 0);
  const duplicateDanger = cssSource(dashboardCSS) +
    '\n.btn-danger:hover { background: color-mix(in srgb, var(--err) 28%, transparent); color: var(--err); }\n';
  const duplicateDangerContract = contrastContract(duplicateDanger);
  check('a later duplicate danger hover rule owns the cascade contract',
    duplicateDangerContract.dangerHoverTint === 28 &&
    dangerTintFailures(duplicateDangerContract).length > 0);
  const duplicateRetryPill = cssSource(dashboardCSS) +
    '\ntr.retry-sub:hover td .pill.err { color: var(--err); }\n';
  const duplicateRetryPillContract = contrastContract(duplicateRetryPill);
  check('a later duplicate nested retry pill rule owns the cascade contract',
    duplicateRetryPillContract.retryPillHoverInk === 'var(--err)' &&
    nestedRetryPillFailures(duplicateRetryPillContract).length > 0);
  const duplicateSelectedInk = cssSource(dashboardCSS) +
    '\n.theme-opt[aria-checked="true"] { color: transparent; }\n';
  const duplicateSelectedInkContract = contrastContract(duplicateSelectedInk);
  check('a later duplicate selected palette ink rule owns the cascade contract',
    duplicateSelectedInkContract.selectedInk === 'transparent' &&
    selectedCompositeFailures(duplicateSelectedInkContract).length > 0);
  const duplicateSelectedHint = cssSource(dashboardCSS) +
    '\n.theme-opt[aria-checked="true"] .theme-hint { color: transparent; }\n';
  const duplicateSelectedHintContract = contrastContract(duplicateSelectedHint);
  check('a later duplicate selected palette hint rule owns the cascade contract',
    duplicateSelectedHintContract.selectedHintInk === 'transparent' &&
    selectedCompositeFailures(duplicateSelectedHintContract).length > 0);
  const raisedRetryTint = contrastContract(mutateCSSRule(dashboardCSS,
    ['tr.retry-sub td'],
    body => body.replace(/(var\(--warn\)\s+)[0-9.]+%/, (_, prefix) => prefix + '100%')));
  check('raising the retry idle tint to 100% fails the contrast guard',
    raisedRetryTint.retryIdleTint === 100 && retryTintFailures(raisedRetryTint).length > 0);
  const mutedCursor = contrastContract(mutateCSSRule(dashboardCSS,
    ['.uplot .u-over > .u-cursor-x'],
    body => body.replace('var(--muted)', 'var(--bg)')));
  check('moving the cursor border to the page background fails the 3:1 guard',
    mutedCursor.cursorInk === 'var(--bg)' && cursorTintFailures(mutedCursor).length > 0);
  const raisedSelectedTint = contrastContract(mutateCSSRule(dashboardCSS,
    [':root'],
    body => body.replace(/--selected-tint:\s*[0-9.]+%/, '--selected-tint: 100%')));
  check('raising the selected palette tint to 100% fails the composite guard',
    raisedSelectedTint.selectedTint === 100 && selectedCompositeFailures(raisedSelectedTint).length > 0);
  const raisedCancelTint = contrastContract(mutateCSSRule(dashboardCSS,
    ['.pill.cancel'],
    body => body.replace('var(--pill-tint)', '80%')));
  check('raising the cancelled pill tint to 80% fails the composite guard',
    raisedCancelTint.cancelTint === 80 && cancelCompositeFailures(raisedCancelTint).length > 0);
  const raisedRetryPillTint = contrastContract(mutateCSSRule(dashboardCSS,
    ['.pill.err'],
    body => body.replace('var(--pill-tint)', '80%')));
  check('raising the nested retry pill tint to 80% fails the composite guard',
    raisedRetryPillTint.retryPillTint === 80 &&
    nestedRetryPillFailures(raisedRetryPillTint).length > 0);
  const mutedRetryPillInk = contrastContract(mutateCSSRule(dashboardCSS,
    ['tr.retry-sub:hover td .pill.err'],
    body => body.replace('var(--err-text)', 'var(--err)')));
  check('muting the nested retry pill hover ink fails the ink contract',
    inkContractErrors(mutedRetryPillInk).length > 0 &&
    nestedRetryPillFailures(mutedRetryPillInk).length > 0);
  root.dataset.theme = 'instrument';
  root.dataset.mode = 'black';

  const mutedEntityInk = contrastContract(mutateCSSRule(dashboardCSS,
    SELECTED_ENTITY_INK_SELECTORS,
    body => body.replace(/color:\s*var\(--text\)/, 'color: var(--muted)')));
  check('muting the selected entity ink fails the ink contract',
    inkContractErrors(mutedEntityInk).length > 0);

  const mutedCancelInk = contrastContract(mutateCSSRule(dashboardCSS,
    ['.pill.cancel'],
    body => body.replace(/color:\s*var\(--text\)/, 'color: var(--muted)')));
  check('muting the cancelled pill ink fails the ink contract',
    inkContractErrors(mutedCancelInk).length > 0);

  win.close();
  reloaded.window.close();
  legacy.window.close();
  invalid.window.close();
  mixed.window.close();
  console.log(failures === 0 ? 'ALL THEME CHECKS PASSED' : `${failures} THEME CHECK(S) FAILED`);
  process.exit(failures === 0 ? 0 : 1);
})().catch(error => {
  console.error('theme check crashed:', error);
  process.exit(1);
});
