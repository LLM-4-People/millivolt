// Composite contrast contracts and their declaration mutation proofs.

const fs = require('fs');
const path = require('path');

const { contrast, tint } = require('./theme_check_color');
const {
  cssSource,
  cssTokenReference,
  mutateCSSRule,
} = require('./theme_check_css');
const {
  SELECTED_ENTITY_INK_SELECTORS,
  TEXT_INK,
  contrastContract,
  inkContractErrors,
} = require('./theme_check_palettes');

function contrastChecks(context, declaredContrast) {
  const { check, win, root, themes, modes, token, dashboardCSS, STATIC } = context;
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
}

module.exports = { contrastChecks };
