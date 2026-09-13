"""Shared repository location and private-state-safe source discovery."""

import os
from pathlib import Path
import re
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def require(value, message):
    """Test safety gates must remain active when Python runs with -O."""
    if not value:
        raise RuntimeError(message)


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
    response = await context.request.post(base + '/admin/session',
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
STATIC_SCAN = STATIC_CSS + STATIC_EMITTERS + ('internal/web/static/favicon.svg',
                                              'internal/web/static/manifest.webmanifest',)


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
    class_re = re.compile(r'\.([A-Za-z_][A-Za-z0-9_-]*)')
    errors = []
    for name, text in css:
        stripped = re.sub(r'/\*.*?\*/', '', text, flags=re.S)
        for selector in re.findall(r'([^{}]+)\{', stripped):
            for token in class_re.findall(selector):
                if token in emitted:
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
    anchored at both ends. The token's own explicit separators (- and space)
    require one [\s_-]+ character; camelCase hump boundaries allow those
    separators or none ([\s_-]*), so inOutRatio, IN_OUT_RATIO and in out
    ratio all match while ordinary prose does not. The final word tolerates
    a plural s."""
    parts = []
    for i, word in enumerate(re.split(r'[-\s]+', token)):
        humps = re.findall(r'[A-Z]?[a-z]+|[A-Z]+', word)
        if ''.join(humps).lower() != word.lower():
            raise ValueError("retired token has unsupported characters: " + token)
        if i:
            parts.append(r'[\s_-]+')
        for j, hump in enumerate(humps):
            if j:
                parts.append(r'[\s_-]*')
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
    braces inside them never desynchronize the walk."""
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
    suspicious either way."""
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


def spark_height_mirror_errors(pairs):
    """chart.js plots each tile's sparkline into an SVG sized by
    CHART_SPARK_H while dashboard.css boxes .chart-total .spark with a px
    height. The two sites must stay equal or the plotted extent no longer
    matches the visible strip."""
    errors = []
    js = [text for name, text in pairs if name == 'internal/web/static/js/chart.js']
    css = [text for name, text in pairs if name == 'internal/web/static/css/dashboard.css']
    height = None
    if js:
        match = re.search(r'\bCHART_SPARK_H\s*=\s*(\d+)', js[0])
        if match:
            height = int(match.group(1))
        else:
            errors.append('internal/web/static/js/chart.js: CHART_SPARK_H declaration not found')
    else:
        errors.append('internal/web/static/js/chart.js: not in the static corpus')
    rule = None
    if css:
        match = re.search(r'\.chart-total\s+\.spark\s*\{[^}]*\bheight:\s*(\d+)px', css[0])
        if match:
            rule = int(match.group(1))
        else:
            errors.append('internal/web/static/css/dashboard.css: .chart-total .spark height not found')
    else:
        errors.append('internal/web/static/css/dashboard.css: not in the static corpus')
    if height is not None and rule is not None and height != rule:
        errors.append('internal/web/static/js/chart.js: CHART_SPARK_H=' + str(height)
                      + ' no longer mirrors the .chart-total .spark height=' + str(rule)
                      + 'px in internal/web/static/css/dashboard.css')
    return errors


PALETTE_OWNER = 'internal/web/static/css/dashboard.css'
PALETTE_MIRRORS = ('internal/web/static/favicon.svg', 'internal/web/static/index.html',
                   'internal/web/static/manifest.webmanifest', 'internal/web/static/sw.js')
_HEX_LITERAL = re.compile(r'#[0-9a-fA-F]{3,8}(?![0-9a-fA-F])')


def _root_palette_hexes(css_text):
    """Hex literals from :root custom-property values - the palette owner.
    The stylesheets legitimately carry other hexes outside :root, so only
    this block is authoritative."""
    stripped = re.sub(r'/\*.*?\*/', '', css_text, flags=re.S)
    hexes = set()
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
        for _, value in re.findall(r'(--[A-Za-z0-9-]+)\s*:\s*([^;]+);', block):
            hexes.update(literal.lower() for literal in _HEX_LITERAL.findall(value))
    return hexes


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
