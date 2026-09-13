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
STATIC_SCAN = STATIC_CSS + STATIC_EMITTERS + ('internal/web/static/manifest.webmanifest',)


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


def duplicate_js_function_errors(pairs, minimum=30):
    """Two functions with whitespace-identical bodies are one helper written
    twice: a change applied to one silently misses the other. The frontend
    audit found a byte-identical pair exactly this way; this detector keeps
    the class from returning."""
    seen = {}
    errors = []
    for file, text in pairs:
        if not file.startswith('internal/web/static/js/'):
            continue
        for name, line, body in _js_functions(text):
            if len(body) < minimum:
                continue
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


def removed_vocabulary_errors(pairs):
    """Retired feature vocabulary must not reappear in dashboard source:
    comments referencing it misdescribe current behavior."""
    errors = []
    for name, text in pairs:
        if not name.startswith('internal/web/static/'):
            continue
        for token, reason in RETIRED_TOKENS:
            if token in text:
                errors.append(f"{name}: retired token '{token}' ({reason})")
    return sorted(set(errors))


def _template_comment_spans(text):
    """Return (start, end) spans of template-literal BODIES (excluding
    ${...} expressions) using a minimal JS lexer: comments, strings and
    nested templates are tracked so backticks inside them never open a
    template body."""
    spans = []
    index = 0
    length = len(text)

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

    def template_body(position):
        """Walk a template body after its opening backtick; returns the
        index just past the closing backtick and records body spans."""
        start = position
        depth_stack = []
        while position < length:
            char = text[position]
            if char == '\\':
                position += 2
                continue
            if char == '`':
                if depth_stack:
                    spans.append((start, position))  # nested template body
                    return position + 1, depth_stack
                spans.append((start, position))
                return position + 1, []
            if char == '$' and position + 1 < length and text[position + 1] == '{':
                spans.append((start, position))
                position += 2
                position, depth_stack = _template_expression(position, depth_stack)
                start = position
                continue
            position += 1
        spans.append((start, position))
        return position, []

    def _template_expression(position, depth_stack):
        brace_depth = 1
        while position < length and brace_depth:
            char = text[position]
            if char == '\\':
                position += 2
                continue
            if char in '\'"':
                position = skip_string(position, char)
                continue
            if char == '`':
                position, depth_stack = template_body(position + 1)
                continue
            if char == '{':
                brace_depth += 1
            elif char == '}':
                brace_depth -= 1
            position += 1
        return position, depth_stack

    while index < length:
        char = text[index]
        if char == '/' and text.startswith('//', index):
            index = text.find('\n', index)
            index = length if index == -1 else index
            continue
        if char == '/' and text.startswith('/*', index):
            end = text.find('*/', index + 2)
            index = length if end == -1 else end + 2
            continue
        if char in '\'"':
            index = skip_string(index, char)
            continue
        if char == '`':
            index, _ = template_body(index + 1)
            continue
        index += 1
    return spans


def js_template_comment_errors(pairs):
    """JS comments never belong inside an HTML template-literal body: they
    render as visible text. This tokenizes templates (including nested
    ${...} expressions) and flags a line comment inside a body span."""
    errors = []
    for name, text in pairs:
        if not name.startswith('internal/web/static/js/'):
            continue
        for start, end in _template_comment_spans(text):
            body = text[start:end]
            offset = 0
            for line in body.split('\n'):
                if re.match(r'\s*//', line):
                    number = text.count('\n', 0, start + offset) + 1
                    errors.append(f"{name}:{number}: JS comment inside a template-literal body")
                offset += len(line) + 1
    return errors
