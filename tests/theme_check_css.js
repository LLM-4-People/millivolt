// Stylesheet text helpers shared by the theme contract checks.

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

module.exports = {
  cssBlock,
  cssColorMix,
  cssDeclarations,
  cssInk,
  cssPercentToken,
  cssProperties,
  cssRule,
  cssSource,
  cssTintPercentage,
  cssTokenReference,
  mutateCSSRule,
  resolvedColorMix,
};
