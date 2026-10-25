// Palette token ownership and the parsed contrast contract.

const {
  cssBlock,
  cssInk,
  cssProperties,
  cssRule,
  cssSource,
  cssTintPercentage,
  resolvedColorMix,
} = require('./theme_check_css');

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

module.exports = {
  ERR_INK,
  MATERIAL_TOKENS,
  MODE_TOKENS,
  MUTED_INK,
  PALETTE_TOKENS,
  SELECTED_ENTITY_INK_SELECTORS,
  SURFACE_ACCENT_TOKENS,
  TEXT_INK,
  contrastContract,
  inkContractErrors,
  paletteBlockErrors,
  tintContractErrors,
};
