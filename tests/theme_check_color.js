// Colour math shared by the theme contrast and palette checks.

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

module.exports = { contrast, deltaE, lab, luminance, rgb, tint };
