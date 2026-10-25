// Palette surfaces, materials and the mode-owned fallback families.

const { contrast, deltaE, rgb, tint } = require('./theme_check_color');
const { cssBlock, cssDeclarations } = require('./theme_check_css');
const {
  MATERIAL_TOKENS,
  MODE_TOKENS,
  PALETTE_TOKENS,
  SURFACE_ACCENT_TOKENS,
  TEXT_INK,
  contrastContract,
  inkContractErrors,
  paletteBlockErrors,
  tintContractErrors,
} = require('./theme_check_palettes');

function surfaceChecks(context) {
  const {
    check, win, d, root, themes, modes, token, elementToken, dashboardCSS,
    themeCSS,
  } = context;
  const stage = d.getElementById('theme-preview');
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
  return declaredContrast;
}

module.exports = { surfaceChecks };
