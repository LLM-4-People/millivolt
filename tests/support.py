"""Shared repository location and private-state-safe source discovery."""

import contextlib
import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def viewport(name: str) -> dict:
    """One measured viewport from the capture registry, for a harness."""
    from scripts.gallery.model import VIEWPORTS

    size = VIEWPORTS[name]
    return {'width': size.width, 'height': size.height}


def require(value, message):
    """Test safety gates must remain active when Python runs with -O."""
    if not value:
        raise RuntimeError(message)


# The layout probes ask one question: does an element clip content a reader is
# meant to see? A scroll box reports scrollWidth > clientWidth for the text it
# clips, which is the defect the probes exist to catch.
#
# An element that paints nothing by design reports the same numbers while
# showing nothing, so it is not a clip. `.visually-hidden` is exactly that shape
# - 1px, overflow hidden, clip-path inset(50%), which is what makes
# screen-reader-only text work - and the three chart triggers gained such spans
# when their accessible names moved to aria-labelledby. The probes then failed a
# correct page. The same latent trap sits under the hidden live region: it is
# empty today, and any status text would trip a probe the moment it is not.
#
# So the exclusion is a property of the reading, not of those spans: an element
# whose own content is invisible (display none, visibility hidden, zero opacity,
# a clip) or that sits inside a clipped or opacity-0 ancestor paints nothing.
# Three probes and any future one share this one definition.
CLIP_PROBE_JS = r"""
const paintsNothing = e => {
  const own = getComputedStyle(e);
  // visibility inherits, so the element's own computed value already answers
  // for an ancestor rule; a child that overrides it back to visible still
  // paints and must not be excluded.
  if (own.display === 'none' || own.visibility === 'hidden') return true;
  for (let n = e; n && n.nodeType === 1; n = n.parentElement) {
    const cs = getComputedStyle(n);
    if (parseFloat(cs.opacity) === 0) return true;
    if (cs.contentVisibility === 'hidden') return true;
    // Both spellings: the modern clip-path and the clip-rect fallback the
    // dashboard's visually-hidden rule still carries for older engines.
    if (cs.clipPath === 'inset(50%)') return true;
    const clip = (cs.clip || '').replace(/\s+/g, '');
    if (clip === 'rect(0px,0px,0px,0px)' || clip === 'rect(0,0,0,0)') return true;
  }
  return false;
};
const clipsPaintedContent = (e, slop = 2) =>
  e.clientWidth > 0 && e.scrollWidth > e.clientWidth + slop && !paintsNothing(e);
const clipped = (root, slop = 2) =>
  [...root.querySelectorAll('*')].filter(e => clipsPaintedContent(e, slop));
"""


def clip_js(script):
    """Give one page probe the shared clip predicate.

    Playwright calls the string form `async () => { ... }` as a function, so a
    declaration prepended to the script would replace it with a statement whose
    completion value is never invoked. The declaration goes inside the body,
    where it is still a statement, and every probe then reads the same
    definition instead of restating the comparison.
    """
    head = 'async () => {'
    if not script.startswith(head):
        raise RuntimeError('clip_js expects an async arrow function body: ' + script[:40])
    return head + CLIP_PROBE_JS + script[len(head):]


# A probe that installs its own rows is a fixture, and the page does not know
# it exists: the bootstrap poll repaints the table from the server's rows, and
# the footer clock repaints its own line. Whatever a probe measured can be
# replaced between its write and its read, which is a race the probe loses at
# random under load. browser_drawer.py cleared the dashboard tick for this
# reason; the request-table width probe then failed the same way on the insert
# path, where the learned floor survived but the injected row did not.
#
# These are the timers the page arms for itself. The scope is one probe: pause
# on entry, and on exit re-arm through the page's own functions, so the cadence
# stays owned by the page and every later check sees the normal timer set.
PAUSE_AMBIENT_JS = """() => {
  // These are top-level `let` bindings in a classic script: they live in the
  // global lexical environment, not on `window`, so they must be named
  // directly. Indexing window by name silently finds nothing.
  window.__ambientWas = {dash: !!_dashTickTimer, clock: !!_clockTimer};
  if (_dashTickTimer) { clearInterval(_dashTickTimer); _dashTickTimer = null; }
  if (_clockTimer) { clearInterval(_clockTimer); _clockTimer = null; }
  if (_xpLiveTimer) { clearTimeout(_xpLiveTimer); _xpLiveTimer = null; }
  if (logRetryTimer) { clearTimeout(logRetryTimer); logRetryTimer = null; }
}"""

RESUME_AMBIENT_JS = """() => {
  const was = window.__ambientWas || {};
  delete window.__ambientWas;
  // Only re-arm what the page had armed. A hidden page arms neither, and a
  // probe must not force a cadence the page deliberately withheld.
  if (was.dash && !_wasHidden) armDashboardTicks();
  if (was.clock && !_wasHidden) _armClock();
}"""


@contextlib.asynccontextmanager
async def paused_ambient(page):
    """Stop the timers the page arms for itself while one fixture is measured."""
    await page.evaluate(PAUSE_AMBIENT_JS)
    try:
        yield
    finally:
        await page.evaluate(RESUME_AMBIENT_JS)


async def pick_chart_control(page, trigger, value):
    """Pick one value from a chart-head control.

    The preset, percentile and timeframe controls are buttons that open a
    shared menu-foundation panel appended to the body: choice rows carry
    data-mn-id and timeframe chips carry data-tf. One owner of that
    interaction, so every browser suite drives the real control the same way.
    """
    await page.click(trigger)
    row = (f'.mn-on [data-tf="{value}"]' if trigger == '#chart-window'
           else f'.mn-on [data-mn-id="{value}"]')
    await page.click(row)


def operator_token():
    """The gated dashboard needs the same MILLIVOLT_OPERATOR_TOKEN the target
    instance was started with."""
    token = os.environ.get('MILLIVOLT_OPERATOR_TOKEN', '')
    require(token, 'MILLIVOLT_OPERATOR_TOKEN is required for the gated dashboard')
    return token


def operator_headers():
    """Fixture cleanup purges through the gated operator plane, so these
    harnesses need the operator credential as a Bearer header."""
    return {'Authorization': 'Bearer ' + operator_token()}


def test_layout_errors(names):
    """Keep test sources centralized and physical filenames suffix-free."""
    suffixes = ("_test.go", "_test.py", "_check.py", "_check.js", ".test.js", ".spec.js")
    errors = []
    for name in names:
        path = Path(name)
        if path.is_relative_to("tests"):
            if path.stem.endswith("_test"):
                errors.append("physical test filename must omit _test suffix: " + name)
        elif name.endswith(suffixes):
            errors.append("test source must live under tests/: " + name)
    return errors


def git_environment():
    # Inherited routing/configuration must not redirect the isolated Git owner.
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env["GIT_CONFIG_NOSYSTEM"] = "1"
    env["GIT_CONFIG_GLOBAL"] = os.devnull
    return env


def source_inventory(root, probes=()):
    """Return ignored probes and public source paths, including untracked files.

    A disposable Git index applies the repository's ignore policy equally in
    checkouts and source archives. Private state and installed dependencies are
    never opened. Tests and publication checks share this one discovery owner.
    """
    root = Path(root).resolve(strict=True)
    env = git_environment()
    with tempfile.TemporaryDirectory(prefix="millivolt-source-check-") as temp:
        (Path(temp) / ".gitignore").write_bytes((root / ".gitignore").read_bytes())
        initialized = subprocess.run(
            ["git", "-c", "init.defaultBranch=check", "init", "--quiet", temp],
            env=env, capture_output=True, text=True,
        )
        if initialized.returncode:
            raise RuntimeError("temporary git init failed: " + initialized.stderr.strip())
        command = ["git", "-c", "core.excludesFile=" + os.devnull, "-C", temp]
        ignored = set()
        if probes:
            result = subprocess.run(
                [*command, "check-ignore", "--no-index", "--stdin", "-z"],
                input="\0".join(probes) + "\0", env=env, capture_output=True, text=True,
            )
            if result.returncode not in (0, 1):
                raise RuntimeError("git check-ignore failed: " + result.stderr.strip())
            ignored = set(filter(None, result.stdout.split("\0")))
        files = subprocess.run(
            [*command, "--work-tree=" + str(root), "ls-files", "--others", "--exclude-standard", "-z"],
            env=env, capture_output=True, text=True,
        )
        if files.returncode:
            raise RuntimeError("git source listing failed: " + files.stderr.strip())
        return ignored, sorted(filter(None, files.stdout.split("\0")))


async def operator_signin(context, base):
    """Mint the operator session cookie inside a Playwright browser context.

    The dashboard page and its EventSource authenticate with the HttpOnly
    cookie; harness requests may also send the Bearer header directly.
    """
    response = await context.request.post(base + '/session',
                                          form={'token': operator_token()})
    require(response.ok, 'operator sign-in failed: ' + str(response.status))


# ---- mechanical source hygiene detectors -------------------------------
# Each detector consumes (relative name, text) pairs so the repository check
# and the unit tests share one implementation over the real corpus or a
# seeded fixture. They exist to catch mechanically detectable regressions
# (dead selectors, duplicated helpers, retired vocabulary, misplaced
# comments) that review agents found by hand - the classes below must never
# come back undetected.

STATIC_CSS = ('internal/web/static/css/',)
STATIC_EMITTERS = ('internal/web/static/js/', 'internal/web/static/index.html',
                   'internal/web/static/sw.js')
SITE_STYLESHEET = 'site-src/assets/site.css'
STATIC_SCAN = STATIC_CSS + STATIC_EMITTERS + ('internal/web/static/favicon.svg',
                                              'internal/web/static/manifest.webmanifest',
                                              SITE_STYLESHEET, 'cmd/proxy/operator.go')


def static_pairs(names, prefixes=STATIC_SCAN, skip_vendor=True):
    """Filter the inventory to the dashboard source corpus as (name, text)."""
    pairs = []
    for name in names:
        if not name.startswith(prefixes):
            continue
        if skip_vendor and '/vendor/' in name:
            continue
        try:
            text = (ROOT / name).read_text(encoding='utf-8')
        except (OSError, UnicodeDecodeError):
            continue
        pairs.append((name, text))
    return pairs


def dead_css_class_errors(pairs):
    """Every stylesheet class must have an emitter: a literal occurrence in
    the JS templates, index.html, sw.js or the manifest. A class no markup
    ever carries is dead styling; the browser audit found exactly such a
    rule surviving after its feature was removed."""
    css = [pair for pair in pairs if pair[0].startswith(STATIC_CSS)]
    emitters = [pair for pair in pairs if not pair[0].startswith(STATIC_CSS)]
    emitted = '\n'.join(text for _, text in emitters)
    # uPlot creates these structural and cursor classes on its own DOM; the
    # dashboard override keeps the remaining vertical cursor on the active
    # surface tokens. The disabled cursor-y and selection rules are gone.
    vendor_classes = {'uplot', 'u-over', 'u-cursor-x'}
    class_re = re.compile(r'\.([A-Za-z_][A-Za-z0-9_-]*)')
    errors = []
    for name, text in css:
        stripped = re.sub(r'/\*.*?\*/', '', text, flags=re.S)
        for selector_list in re.findall(r'([^{}]+)\{', stripped):
            # The vendor exemption belongs to the uPlot-mounted selector tree.
            # Split the list: `.u-over, .uplot` would otherwise exempt a bare
            # dashboard `.u-over` through an unrelated sibling selector in the
            # same rule. `.uplot` must appear in the same individual selector.
            for selector in selector_list.split(','):
                vendor_selector = re.search(r'\.uplot\b', selector) is not None
                for token in class_re.findall(selector):
                    if token in emitted or (vendor_selector and token in vendor_classes):
                        continue
                    errors.append(f"{name}: stylesheet class .{token} has no emitter")
    return sorted(set(errors))


def _js_functions(text):
    """Yield (name, start_line, normalized_body) for every function or
    arrow-const with a brace body. Normalization strips whitespace only:
    byte-identical twins stay identical, anything reworded is left for
    human review."""
    for match in re.finditer(r'(?:function\s+([A-Za-z_$][\w$]*)\s*\([^)]*\)|const\s+([A-Za-z_$][\w$]*)\s*=\s*(?:\([^)]*\)|[A-Za-z_$][\w$]*)\s*=>)\s*\{', text):
        name = match.group(1) or match.group(2)
        depth = 1
        index = match.end()
        while depth and index < len(text):
            char = text[index]
            if char == '{':
                depth += 1
            elif char == '}':
                depth -= 1
            index += 1
        body = text[match.end():index - 1] if depth == 0 else ''
        if len(re.sub(r'\s+', '', body)) >= 30:
            yield name, text.count('\n', 0, match.start()) + 1, re.sub(r'\s+', '', body)


def duplicate_js_function_errors(pairs):
    """Two functions with whitespace-identical bodies are one helper written
    twice: a change applied to one silently misses the other. The frontend
    audit found a byte-identical pair exactly this way; this detector keeps
    the class from returning. _js_functions owns the minimum body size."""
    seen = {}
    errors = []
    for file, text in pairs:
        if not file.startswith('internal/web/static/js/'):
            continue
        for name, line, body in _js_functions(text):
            key = body
            twin = seen.get(key)
            if twin is not None and twin[1] != name:
                errors.append(f"{file}:{line}: function {name} duplicates {twin[1]} "
                              f"({twin[0]}:{twin[2]}) - extract the shared helper")
            else:
                seen[key] = (file, name, line)
    return errors


RETIRED_TOKENS = (
    # Vocabulary of removed features: a reference in dashboard source is
    # stale by definition. Extend this list whenever a feature is deleted.
    ('spark-row', 'the per-half spark rows became one multi-line sparkline per tile'),
    ('tileFmt', 'the tile-specific formatter was removed with the merged tiles'),
    ('inOutRatio', 'the ratio became the bounded in/out share'),
    ('chartStackPaths', 'stacked-bar machinery was deleted'),
    ('ten tiles', 'the summary never had ten tiles again'),
    ('ten-tile', 'the summary never had ten tiles again'),
)


def _retired_token_pattern(token):
    """Compile one retired token into its matcher: case folded, word-boundary
    anchored at both ends. Every word boundary - an explicit separator (- or
    space) in the token or a camelCase hump - accepts any mix of space, -,
    _ and / separators or none at all ([\\s_/-]*), so spark-row, sparkRow,
    sparkrow, inOutRatio, IN_OUT_RATIO, in out ratio and in/out ratio all
    match while ordinary prose does not. Squished forms (sparkrow,
    tentiles) matching is deliberate: retired vocabulary is stale in
    any near spelling a dashboard source could carry. The final word
    tolerates a plural s."""
    parts = []
    for i, word in enumerate(re.split(r'[-\s]+', token)):
        humps = re.findall(r'[A-Z]?[a-z]+|[A-Z]+', word)
        if ''.join(humps).lower() != word.lower():
            raise ValueError("retired token has unsupported characters: " + token)
        if i:
            parts.append(r'[\s_/-]*')
        for j, hump in enumerate(humps):
            if j:
                parts.append(r'[\s_/-]*')
            parts.append(hump)
    last = parts.pop()
    if last.endswith('s'):
        last = last[:-1]
    parts.append(last + 's?')
    return re.compile(r'(?i)\b' + ''.join(parts) + r'\b')


RETIRED_PATTERNS = tuple((token, _retired_token_pattern(token), reason)
                         for token, reason in RETIRED_TOKENS)


def removed_vocabulary_errors(pairs):
    """Retired feature vocabulary must not reappear in dashboard source:
    comments referencing it misdescribe current behavior. Matching is case
    folded and separator tolerant, so every spelling of a removed name
    flags."""
    errors = []
    for name, text in pairs:
        if not name.startswith('internal/web/static/'):
            continue
        for token, pattern, reason in RETIRED_PATTERNS:
            if pattern.search(text):
                errors.append(f"{name}: retired token '{token}' ({reason})")
    return sorted(set(errors))


# A '/' starts a regex literal when the previous significant token is one
# of these punctuation characters, one of the keywords below, or nothing
# yet (start of file). After an identifier, a number, ')', ']' or a string,
# regex or template close it is division instead.
_REGEX_PREV_TOKENS = frozenset('(,=:[!&|?{;}+-*%<>^~')
_REGEX_KEYWORDS = frozenset((
    'await', 'case', 'delete', 'do', 'else', 'in', 'instanceof', 'new',
    'of', 'return', 'throw', 'typeof', 'void', 'yield',
))


def _template_comment_spans(text):
    """Return (start, end) spans of template-literal BODIES (excluding
    ${...} expressions) using a minimal JS lexer: comments, strings, regex
    literals and nested templates are tracked so quotes, backticks and
    braces inside them never desynchronize the walk.

    Two misread classes are accepted as theoretical residuals. A
    statement-position regex after a closing ')' or ']' (e.g.
    "if (x) /re/.test(s)") is classified as division, so a quote inside the
    regex opens a phantom string; the string skipper bails at the first
    unescaped newline (a real string literal never spans lines), so that
    desync is bounded to the regex's own source line. A backtick inside
    such a regex opens a phantom template instead, and template bodies
    legitimately span lines, so that desync extends to the next backtick in
    the file. Both require regex-in-statement-position-after-a-closing-
    token, a shape no real dashboard code uses."""
    spans = []
    length = len(text)
    # Previous significant token as [kind, value]; kind is 'punct',
    # 'keyword' or 'id'. It decides regex literal vs division for '/'.
    prev = ['none', '']

    def skip_string(position, quote):
        position += 1
        while position < length:
            char = text[position]
            if char == '\\':
                position += 2
                continue
            if char == quote:
                return position + 1
            if char == '\n':
                return position  # a real string literal never spans lines
            position += 1
        return position

    def skip_regex(position):
        """Consume a regex literal from its opening '/': to the first
        unescaped '/' outside a [...] character class, then the flags."""
        position += 1
        in_class = False
        while position < length:
            char = text[position]
            if char == '\\':
                position += 2
                continue
            if char == '\n':
                return position  # a real regex literal never spans lines
            if char == '[':
                in_class = True
            elif char == ']':
                in_class = False
            elif char == '/' and not in_class:
                position += 1
                while position < length and text[position].isalpha():
                    position += 1
                return position
            position += 1
        return position

    def slash_starts_regex():
        return (prev[0] == 'none'
                or prev[0] == 'keyword'
                or (prev[0] == 'punct' and prev[1] in _REGEX_PREV_TOKENS))

    def template_body(position):
        """Walk a template body after its opening backtick; returns the
        index just past the closing backtick and records body spans."""
        start = position
        while position < length:
            char = text[position]
            if char == '\\':
                position += 2
                continue
            if char == '`':
                spans.append((start, position))
                return position + 1
            if char == '$' and position + 1 < length and text[position + 1] == '{':
                spans.append((start, position))
                prev[0], prev[1] = 'punct', '{'
                position = walk_code(position + 2, True)
                start = position
                continue
            position += 1
        spans.append((start, position))
        return position

    def walk_code(position, expression):
        """Lex code from position, skipping comments, strings, regex
        literals and template literals while tracking the previous
        significant token. In expression context (just past '${') the walk
        ends just past the unmatched closing '}'."""
        depth = 1 if expression else 0
        while position < length:
            char = text[position]
            if char == '/' and text.startswith('//', position):
                end = text.find('\n', position)
                position = length if end == -1 else end
                continue
            if char == '/' and text.startswith('/*', position):
                end = text.find('*/', position + 2)
                position = length if end == -1 else end + 2
                continue
            if char in '\'"':
                position = skip_string(position, char)
                prev[0], prev[1] = 'punct', 'str'
                continue
            if char == '`':
                position = template_body(position + 1)
                prev[0], prev[1] = 'punct', '`'
                continue
            if char == '/' and slash_starts_regex():
                position = skip_regex(position)
                prev[0], prev[1] = 'punct', 'regex'
                continue
            if char == '/':
                prev[0], prev[1] = 'punct', '/'
                position += 1
                continue
            if char in '+-' and text.startswith(char * 2, position):
                # '++' / '--': postfix when the previous significant token
                # ends an expression (identifier, number, ')', ']', or a
                # string/regex/template close), so the following '/' is
                # division. Any other previous token (start of statement,
                # operator, keyword, ';', braces, '(', ',', '=') means
                # prefix, where the regex rules stand.
                if prev[0] == 'id' or (prev[0] == 'punct'
                                       and prev[1] in (')', ']', 'str', 'regex', '`')):
                    prev[0], prev[1] = 'id', ''
                else:
                    prev[0], prev[1] = 'punct', char
                position += 2
                continue
            if expression and char == '}':
                depth -= 1
                prev[0], prev[1] = 'punct', '}'
                if not depth:
                    return position + 1
            elif char == '{':
                if expression:
                    depth += 1
                prev[0], prev[1] = 'punct', '{'
            elif char.isalnum() or char in '_$':
                end = position + 1
                while end < length and (text[end].isalnum() or text[end] in '_$'):
                    end += 1
                prev[0] = 'keyword' if text[position:end] in _REGEX_KEYWORDS else 'id'
                prev[1] = text[position:end]
                position = end
                continue
            elif not char.isspace():
                prev[0], prev[1] = 'punct', char
            position += 1
        return position

    walk_code(0, False)
    return spans


def js_template_comment_errors(pairs):
    """JS comments never belong inside an HTML template-literal body: they
    render as visible text. This tokenizes templates (including nested
    ${...} expressions) and flags a line comment or a block-comment opener
    at a body line start. A rendered URL like //cdn.example/x still flags:
    suspicious either way. So does a body fragment that starts with //
    right after a ${...} interpolation on the same source line (rendered
    markup following the expression): the same accepted class, suspicious
    either way."""
    errors = []
    for name, text in pairs:
        if not name.startswith('internal/web/static/js/'):
            continue
        for start, end in _template_comment_spans(text):
            body = text[start:end]
            offset = 0
            for line in body.split('\n'):
                if re.match(r'\s*(?://|/\*)', line):
                    number = text.count('\n', 0, start + offset) + 1
                    errors.append(f"{name}:{number}: JS comment inside a template-literal body")
                offset += len(line) + 1
    return errors


SPARK_HEIGHT_JS = 'internal/web/static/js/chart.js'
SPARK_HEIGHT_CSS = 'internal/web/static/css/charts.css'


def spark_height_mirror_errors(pairs):
    """chart.js plots each tile's sparkline into an SVG sized by
    CHART_SPARK_H while charts.css boxes .chart-total .spark with a px
    height. The two sites must stay equal or the plotted extent no longer
    matches the visible strip."""
    errors = []
    js = [text for name, text in pairs if name == SPARK_HEIGHT_JS]
    css = [text for name, text in pairs if name == SPARK_HEIGHT_CSS]
    height = None
    if js:
        match = re.search(r'\bCHART_SPARK_H\s*=\s*(\d+)', js[0])
        if match:
            height = int(match.group(1))
        else:
            errors.append(SPARK_HEIGHT_JS + ': CHART_SPARK_H declaration not found')
    else:
        errors.append(SPARK_HEIGHT_JS + ': not in the static corpus')
    rule = None
    if css:
        match = re.search(r'\.chart-total\s+\.spark\s*\{[^}]*\bheight:\s*(\d+)px', css[0])
        if match:
            rule = int(match.group(1))
        else:
            errors.append(SPARK_HEIGHT_CSS + ': .chart-total .spark height not found')
    else:
        errors.append(SPARK_HEIGHT_CSS + ': not in the static corpus')
    if height is not None and rule is not None and height != rule:
        errors.append(SPARK_HEIGHT_JS + ': CHART_SPARK_H=' + str(height)
                      + ' no longer mirrors the .chart-total .spark height=' + str(rule)
                      + 'px in ' + SPARK_HEIGHT_CSS)
    return errors


VENDOR_BANNER = 'internal/web/static/vendor/uplot.js'
VENDOR_NOTICE = 'THIRD_PARTY_NOTICES.md'


def vendor_notice_errors(pairs):
    """The vendored library's version is one fact in three places: the banner
    the upstream build writes, the notice's version link, and the notice's
    license link. Only the first is generated, so a bump that replaces the file
    silently leaves a stale version in a legally meaningful notice."""
    errors = []
    # The vendored bundle is outside the JS scan corpus on purpose (third-party
    # code is never linted as ours), so its banner is read directly.
    try:
        banner = (ROOT / VENDOR_BANNER).read_text(encoding='utf-8', errors='replace')
    except OSError:
        return [VENDOR_BANNER + ': unreadable']
    try:
        text = (ROOT / VENDOR_NOTICE).read_text(encoding='utf-8')
    except OSError:
        return [VENDOR_NOTICE + ': unreadable']
    match = re.search(r'uPlot \(v(\d+\.\d+\.\d+)\)', banner)
    if not match:
        return [VENDOR_BANNER + ': version banner not found']
    version = match.group(1)
    if version not in text:
        errors.append(VENDOR_NOTICE + ': notice does not name the vendored uPlot '
                      + version + ' the banner declares')
    for label, pattern in (
        ('tree link', r'\(https://github\.com/leeoniya/uPlot/tree/(\d+\.\d+\.\d+)\)'),
        ('license link', r'\(https://github\.com/leeoniya/uPlot/blob/(\d+\.\d+\.\d+)/LICENSE\)'),
    ):
        found = re.search(pattern, text)
        if not found:
            errors.append(VENDOR_NOTICE + ': ' + label + ' does not name a version')
        elif found.group(1) != version:
            errors.append(VENDOR_NOTICE + ': ' + label + ' names ' + found.group(1)
                          + ', the vendored banner declares ' + version)
    return errors


# A coarse pointer cannot hover, aim finely or recover a clipped value, so every
# control a dense layout sizes below the shared target must be raised by the
# coarse block that owns its surface. --tap is that target.
TAP_TOKEN_PX = 44
# One reading of "this selector names a control". A class-based control is any
# element whose final class name ends in a word from the control vocabulary this
# dashboard uses: a button, tab, row, item, chip, disclosure, close, undo, and
# so on. The suffix list is intentionally broad - a false positive costs one
# coarse rule, while a false negative is a 20px target nobody sees.
CONTROL_SUFFIXES = (
    'btn', 'button', 'tab', 'row', 'item', 'chip', 'toggle', 'move', 'vis',
    'restore', 'select', 'close', 'undo', 'disclosure', 'opt', 'action',
    'link', 'trigger', 'x', 'switch',
)


def css_names_control(selector):
    """True when the selector's last compound names something a finger taps."""
    last = selector.split()[-1] if selector.split() else selector
    for part in re.split(r'[>:+~]', last):
        part = part.strip()
        if not part:
            continue
        if part in ('button', 'select', 'input', 'summary', 'textarea'):
            return True
        if part.startswith('#') and 'legend' not in part:
            # An id selector on an interactive surface (a specific control).
            return True
        for cls in re.findall(r'\.([A-Za-z][\w-]*)', part):
            if cls in ('btn', 'prov-x', 'mini-btn', 'copy-btn'):
                return True
            if any(cls == suffix or cls.endswith('-' + suffix) for suffix in CONTROL_SUFFIXES):
                return True
    return False


def _css_blocks(text):
    """(media context, selector, declarations) for every CSS block.

    Comments are removed first: they contain braces and quoted selectors, and a
    naive scan would attribute a declaration to whichever comment preceded it.
    """
    text = re.sub(r'/\*.*?\*/', '', text, flags=re.S)
    out, stack, token = [], [], ''
    for char in text:
        if char == '{':
            stack.append(token.strip())
            token = ''
        elif char == '}':
            if stack:
                out.append((stack[-2] if len(stack) > 1 else '', stack[-1], token))
            token = ''
            if stack:
                stack.pop()
        else:
            token += char
    return out


def tap_target_errors(pairs):
    """Every interactive control a fine-pointer rule sizes below --tap must be
    named by the coarse block for its surface.

    A dense dashboard legitimately renders 20-28px rows: they are read at a desk
    with a mouse. The same rules also reach a phone, where a mis-tap on the
    remove, park or move control beside a rule costs real operator state. This
    check asks the two questions a reviewer cannot: which controls are declared
    short, and which of them some coarse block actually raises.
    """
    errors = []
    for name, text in pairs:
        if not name.startswith('internal/web/static/css/') or not name.endswith('.css'):
            continue
        blocks = _css_blocks(text)
        if not blocks:
            continue
        short = {}
        for media, selector, body in blocks:
            if media:
                continue
            match = re.search(r'min-height:\s*(\d+)px', body)
            if not match or int(match.group(1)) >= TAP_TOKEN_PX:
                continue
            for part in selector.split(','):
                part = part.strip()
                if part and css_names_control(part):
                    short.setdefault(part, int(match.group(1)))
        if not short:
            continue
        covered = set()
        for media, selector, body in blocks:
            if 'pointer: coarse' not in media and 'hover: none' not in media:
                continue
            for part in selector.split(','):
                part = part.strip()
                if not part:
                    continue
                # A coarse rule may name the control itself or qualify it with
                # its surface prefix; either reaches the same element.
                covered.add(part)
                covered.add(part.split()[-1])
        for selector, px in sorted(short.items()):
            if selector in covered or selector.split()[-1] in covered:
                continue
            errors.append(name + ': ' + selector + ' is ' + str(px)
                          + 'px at a fine pointer and no coarse block raises it to the '
                          + str(TAP_TOKEN_PX) + 'px target')
    return errors


PALETTE_OWNER = 'internal/web/static/css/dashboard.css'
PALETTE_MIRRORS = ('internal/web/static/favicon.svg', 'internal/web/static/index.html',
                   'internal/web/static/manifest.webmanifest', 'internal/web/static/sw.js',
                   'cmd/proxy/operator.go')
# The static site only owns values with no dashboard equivalent: a stronger
# control edge, a second-level text tone, elevation, panel radius, content width
# and the editorial display face used by marketing and documentation titles.
# Everything else, including --sans and --mono, comes from tokens.css.
SITE_TOKENS = frozenset({
    '--border-control', '--muted-strong', '--shadow', '--radius', '--max', '--docs-max',
    '--docs-width', '--display',
})
_HEX_LITERAL = re.compile(r'#[0-9a-fA-F]{3,8}(?![0-9a-fA-F])')


def root_custom_properties(css_text):
    """Return normalized custom-property declarations from every :root block.

    Comments are stripped first, later duplicate declarations win, and
    whitespace is collapsed so a generated copy can compare byte-for-byte
    with the owner without inheriting source indentation.
    """
    stripped = re.sub(r'/\*.*?\*/', '', css_text, flags=re.S)
    properties = {}
    for match in re.finditer(r':root\s*\{', stripped):
        end = match.end()
        depth = 1
        while end < len(stripped) and depth:
            if stripped[end] == '{':
                depth += 1
            elif stripped[end] == '}':
                depth -= 1
            end += 1
        block = stripped[match.end():end - 1]
        for name, value in re.findall(r'(--[A-Za-z0-9-]+)\s*:\s*([^;]+);', block):
            properties[name] = re.sub(r'\s+', ' ', value).strip()
    return properties


def _root_palette_hexes(css_text):
    """Hex literals from :root custom-property values - the palette owner.
    The stylesheets legitimately carry other hexes outside :root, so only
    this block is authoritative."""
    hexes = set()
    for value in root_custom_properties(css_text).values():
        hexes.update(literal.lower() for literal in _HEX_LITERAL.findall(value))
    return hexes


def site_palette_errors(pairs):
    """The static site consumes tokens.css from the dashboard owner. Its own
    :root block may only carry the site-only tokens: redeclaring a canonical
    token creates a second value table, and an unlisted token would silently
    grow one."""
    canonical = None
    site = None
    for name, text in pairs:
        if name == PALETTE_OWNER:
            canonical = root_custom_properties(text)
        elif name == SITE_STYLESHEET:
            site = root_custom_properties(text)
    errors = []
    if canonical is None:
        errors.append(PALETTE_OWNER + ': the :root custom-property palette is missing')
    if site is None:
        errors.append(SITE_STYLESHEET + ': not in the static corpus')
    if canonical is None or site is None:
        return errors
    for token in sorted(set(site) & set(canonical)):
        errors.append(f"{SITE_STYLESHEET}: redeclares canonical token {token} "
                      f"from {PALETTE_OWNER}")
    for token in sorted(set(site) - SITE_TOKENS - set(canonical)):
        errors.append(f"{SITE_STYLESHEET}: site-only token {token} is not in the "
                      "site token allowlist")
    return errors


def token_drift_errors(canonical_text, generated_text, generated_name):
    """Generated tokens.css must be the canonical :root block, no additions,
    omissions or value changes. The comparison reuses the palette owner's
    parser, so generated markup formatting cannot hide drift."""
    canonical = root_custom_properties(canonical_text)
    generated = root_custom_properties(generated_text)
    errors = []
    for token in sorted(set(canonical) - set(generated)):
        errors.append(f"{generated_name}: missing canonical token {token}")
    for token in sorted(set(generated) - set(canonical)):
        errors.append(f"{generated_name}: token {token} is not declared in {PALETTE_OWNER}")
    for token in sorted(set(canonical) & set(generated)):
        if canonical[token] != generated[token]:
            errors.append(f"{generated_name}: {token} = {generated[token]} does not match "
                          f"{PALETTE_OWNER} {canonical[token]}")
    return errors


def palette_mirror_errors(pairs):
    """Standalone static files cannot resolve var(), so their hex colors
    mirror the :root custom properties in dashboard.css. A mirror hex that
    is not a palette value is drift by definition."""
    palette = None
    for name, text in pairs:
        if name != PALETTE_OWNER:
            continue
        palette = _root_palette_hexes(text)
        break
    if palette is None:
        return [PALETTE_OWNER + ': the :root custom-property palette is missing']
    errors = []
    for name, text in pairs:
        if name not in PALETTE_MIRRORS:
            continue
        for literal in sorted({value.lower() for value in _HEX_LITERAL.findall(text)}):
            if literal not in palette:
                errors.append(f"{name}: hex {literal} is not a :root custom-property "
                              f"value in {PALETTE_OWNER}")
    return errors


ARTIFACT_GZIP_OWNER = 'cmd/proxy/log.go'
ARTIFACT_GZIP_CAPTURE_OWNER = 'internal/proxy/debug.go'
ARTIFACT_GZIP_CODEC_OWNERS = ('internal/proxy/gzip.go', 'internal/web/gzip.go')
_GZIP_TEST_ROOT = 'tests/'
ARTIFACT_GZIP_CALL = 'NewArtifactGzipWriter('
_GZIP_WRITER_CONSTRUCTION = re.compile(r'\bgzip\.NewWriter(?:Level)?\s*\(')


def artifact_gzip_choke_point_errors(pairs):
    """cmd/proxy/log.go and internal/proxy/debug.go are the two compression
    gates every saved gzip artifact crosses: the logs export route and the
    debug capture download each write through exactly one
    NewArtifactGzipWriter wrap, so matching, all and debug-only exports and
    every capture download inherit the same artifact codec. A second wrap is
    a per-branch compression gate a new surface could silently skip; a
    missing wrap means the artifact contract itself was dropped. Neither
    gate may construct a compress/gzip writer directly: NewArtifactGzipWriter
    (internal/proxy/gzip.go) is the only construction site, so the artifact
    codec cannot fork per surface.
    The whole-tree rule: no .go file outside tests in the checked corpus
    may construct a compress/gzip writer directly - the two codec owners
    (internal/proxy/gzip.go for saved artifacts, internal/web/gzip.go for
    the dashboard's transit and precomputed representations) are the only
    construction sites, so a future download surface in any package (the
    repository root, deploy/, or a new cmd) cannot hand-roll gzip silently.
    A violation reports file:line. The two artifact gate files above carry
    their own stricter per-file construction ban and are not rescanned here
    (one defect, one report). Test sources stay outside the ban by design;
    ignored paths (agents/ and build artifacts) never enter the corpus the
    repository check feeds this detector."""
    errors = []
    for owner in (ARTIFACT_GZIP_OWNER, ARTIFACT_GZIP_CAPTURE_OWNER):
        text = None
        for name, body in pairs:
            if name == owner:
                text = body
                break
        if text is None:
            errors.append(owner + ': artifact source is not in the checked corpus')
            continue
        wraps = text.count(ARTIFACT_GZIP_CALL)
        if wraps == 0:
            errors.append(owner + ': the NewArtifactGzipWriter artifact choke point is '
                            'gone - every artifact download must cross the one shared gzip artifact writer')
        elif wraps > 1:
            errors.append(owner + ': ' + str(wraps) + ' NewArtifactGzipWriter wraps - '
                          'the artifact compression gate is one choke point, never a per-branch writer')
        if _GZIP_WRITER_CONSTRUCTION.search(text):
            errors.append(owner + ': direct compress/gzip writer construction - '
                          'NewArtifactGzipWriter is the only saved-artifact construction site')
    for name, text in pairs:
        if name in ARTIFACT_GZIP_CODEC_OWNERS or name in (ARTIFACT_GZIP_OWNER, ARTIFACT_GZIP_CAPTURE_OWNER):
            continue
        if name.startswith(_GZIP_TEST_ROOT):
            continue
        for match in _GZIP_WRITER_CONSTRUCTION.finditer(text):
            line = text.count('\n', 0, match.start()) + 1
            errors.append(f"{name}:{line}: direct compress/gzip writer construction - "
                          "the only codec owners are internal/proxy/gzip.go "
                          "(saved artifacts) and internal/web/gzip.go "
                          "(the dashboard's transit and precomputed representations)")
    return errors
