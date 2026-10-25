"""GitHub path filters and the inline docs-only classifier proof."""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import tempfile
from fnmatch import fnmatchcase
from pathlib import Path

from .workflows_support import non_comment_lines, shell_declaration


def github_glob(pattern: str, path: str) -> bool:
    """Match one repository path against GitHub's paths-filter glob syntax.

    GitHub treats `*` as any run without `/`, `**` as any run including `/`,
    and a leading `**/` as zero or more directories. A plain `fnmatch` would
    require a directory, so `**/*.md` would not cover root `README.md`.
    """
    # `?` is not modeled: no policy pattern uses it, and GitHub's filter
    # syntax for it is not plain single-character matching. Fail closed so a
    # future `?` pattern must extend the helper instead of passing vacuously.
    if "?" in pattern:
        raise ValueError(
            "github_glob does not model GitHub's `?` filter syntax; "
            "no policy pattern uses it, so extend this helper before adding one")
    translated = []
    index = 0
    while index < len(pattern):
        character = pattern[index]
        if character == "*" and pattern.startswith("**", index):
            index += 2
            if index < len(pattern) and pattern[index] == "/":
                translated.append("(?:.*/)?")
                index += 1
            else:
                translated.append(".*")
            continue
        if character == "*":
            translated.append("[^/]*")
        else:
            translated.append(re.escape(character))
        index += 1
    return re.fullmatch("".join(translated), path) is not None


def ignored_samples(pattern: str) -> tuple[str, ...]:
    """Return concrete paths that represent one paths-ignore pattern.

    Nested samples are deliberate: a shallow `docs/sample` or `site-src/sample`
    would keep passing when the pattern silently narrows to a single level. The
    root and nested Markdown samples keep a docs/** path from masking a missing
    `**/*.md` match, and prove that `*.md` cannot cover a nested file.
    """
    fixed = {
        "docs/**": ("docs/nested/sample", "docs/a/b/sample"),
        "site-src/**": ("site-src/nested/sample", "site-src/a/b/sample"),
        "**/*.md": ("README.md", "docs/nested/README.md"),
        # Narrowed mutation forms keep the same depth sample, so the coverage
        # proof fails when the glob stops covering nested paths.
        "docs/*": ("docs/nested/sample", "docs/a/b/sample"),
        "docs/*/*": ("docs/nested/sample", "docs/a/b/sample"),
        "site-src/*": ("site-src/nested/sample", "site-src/a/b/sample"),
        "site-src/*/*": ("site-src/nested/sample", "site-src/a/b/sample"),
        "*.md": ("README.md", "docs/nested/README.md"),
    }
    if pattern in fixed:
        return fixed[pattern]
    if "*" not in pattern:
        return (pattern,)
    return ((pattern.replace("**/", "")
             .replace("**", "nested/sample")
             .replace("*", "sample")),)


def docs_coverage_errors(ignored_paths: list[str], docs_paths: list[str]) -> list[str]:
    """Require every ignored pattern sample to match both sides of the filter."""
    errors = []
    for ignored_path in ignored_paths:
        for sample in ignored_samples(ignored_path):
            if not github_glob(ignored_path, sample):
                errors.append(f"check.yml: sample {sample} does not match paths-ignore {ignored_path}")
            covered = ignored_path in docs_paths or any(
                github_glob(pattern, sample) for pattern in docs_paths)
            if not covered:
                errors.append(f"docs.yml: no docs path covers check.yml paths-ignore {ignored_path} ({sample})")
    return errors


DOCS_CASE_OVERRIDES = {"**/*.md": "*.md"}
CHECK_WORKFLOW = ".github/workflows/check.yml"
WORKFLOW_SUFFIXES = (".yml", ".yaml")
WORKFLOW_FAMILY_GLOB = ".github/workflows/*.yml"
WORKFLOW_FAMILY_YAML_GLOB = ".github/workflows/*.yaml"
WORKFLOW_SAMPLE_PATHS = (CHECK_WORKFLOW, ".github/workflows/example.yaml")
WORKFLOW_DOCS_ERROR = "check.yml: DOCS_PATH_PATTERNS must not classify a workflow file as docs-only"


def shell_case_family(patterns: list[str]) -> set[str]:
    """Map GitHub paths-ignore globs to DOCS_PATH_PATTERNS shell case globs.

    A shell case `*` also matches `/`, so `**/*.md` becomes `*.md`. Everything
    else is passed through unchanged, including the explicit docs/site workflow
    files, so code-bearing check.yml is never classified as docs-only.
    """
    return {DOCS_CASE_OVERRIDES.get(pattern, pattern) for pattern in patterns}


def docs_pattern_errors(patterns: set[str], expected: set[str]) -> list[str]:
    """Validate DOCS_PATH_PATTERNS against the paths-ignore family."""
    errors = []
    if patterns != expected:
        missing = sorted(expected - patterns)
        unexpected = sorted(patterns - expected)
        errors.append(
            "check.yml: DOCS_PATH_PATTERNS must equal the paths-ignore family mapped "
            f"to shell case globs (missing: {missing}; unexpected: {unexpected})")
    if any(fnmatchcase(path, pattern)
           for path in WORKFLOW_SAMPLE_PATHS for pattern in patterns):
        errors.append(WORKFLOW_DOCS_ERROR)
    return errors


def github_glob_errors() -> list[str]:
    """Pin GitHub glob translation with positive and negative samples."""
    cases = (
        ("docs/**", "README.md", False),
        ("**/*.md", CHECK_WORKFLOW, False),
        ("**/*.md", "README.md", True),
        ("a/**/b/**/c.md", "a/b/c.md", True),
        ("a/**/b/**/c.md", "a/x/b/y/c.md", True),
        ("a/**/b/**/c.md", "a/x/c.md", False),
    )
    return [
        f"workflows self-check: github_glob({pattern!r}, {path!r}) must be {expected}"
        for pattern, path, expected in cases
        if github_glob(pattern, path) is not expected
    ]


DOCS_PATH_SAMPLES = (
    ("docs/operations.md", True),
    ("docs/a/b/sample", True),
    ("site-src/templates/base.html", True),
    ("site-src/a/b/sample", True),
    ("README.md", True),
    ("docs/nested/README.md", True),
    ("LICENSE", True),
    ("scripts/build_site.py", True),
    ("requirements-docs.txt", True),
    (".github/workflows/docs.yml", True),
    (".github/workflows/pages.yml", True),
    (".github/workflows/dependency-review.yml", True),
    (".github/workflows/security.yml", True),
    (".github/workflows/check.yml", False),
    ("scripts/check.sh", False),
    ("scripts/dev.sh", False),
    ("internal/web/main.go", False),
    ("cmd/proxy/main.go", False),
    ("tests/support.py", False),
)


def docs_classifier_errors(run: str, patterns: str) -> list[str]:
    """Exercise the inline classifier over tracked docs and non-docs prefixes."""
    body = shell_declaration(run, "is_docs_path")
    if not body:
        return ["check.yml: inline docs classifier is missing"]
    if shutil.which("bash") is None:
        return ["workflows self-check: bash is required to exercise the docs classifier"]
    script = "set -euo pipefail\n" + body + "\nis_docs_path \"$1\"\n"
    errors = []
    for path, expected in DOCS_PATH_SAMPLES:
        result = subprocess.run(
            ["bash", "-c", script, "bash", path],
            env={**os.environ, "DOCS_PATH_PATTERNS": patterns},
            capture_output=True, text=True, timeout=10, check=False)
        actual = result.returncode == 0
        if actual is not expected:
            errors.append(f"check.yml: inline docs classifier must classify {path} as "
                          f"{'docs' if expected else 'code'}")
    return errors


def docs_only_fail_open_errors(run: str) -> list[str]:
    """Prove docs-only classification follows the diff and fails closed.

    The promotion step is executed with a fake git. A code path must classify as
    a full gate, a docs-only path as docs-only, and a failing diff must abort
    before writing a classification. The step's own `set -euo pipefail` is
    required as a command line and is never injected here: a missing `set -e`
    must fail the static contract instead of being masked by the harness. The
    classifier is inline, so a malicious `scripts/docs-paths.sh` in the
    candidate checkout must have no effect. A non-ancestor main or a head that
    is not the current testing tip must fail the step.
    """
    if shutil.which("bash") is None:
        return ["workflows self-check: bash is required to exercise docs-only classification"]
    if "set -euo pipefail" not in non_comment_lines(run):
        return ["workflows self-check: docs-only classification must keep set -euo pipefail as a command line"]
    if "source scripts/docs-paths.sh" in non_comment_lines(run):
        return ["workflows self-check: docs-only classification must not source the candidate classifier"]
    fake_git = """git() {
  case "$1" in
    fetch) return 0 ;;
    merge-base)
      if [ "$2" = "--is-ancestor" ]; then return "${FAKE_MAIN_ANCESTOR:-0}"; fi
      echo base
      return 0
      ;;
    rev-parse) echo "${FAKE_TIP:-testing}"; return 0 ;;
    diff)
      case "${FAKE_DIFF_MODE:-fail}" in
        code) printf '%s\\n' "internal/web/main.go"; return 0 ;;
        docs) printf '%s\\n' "README.md" "docs/operations.md"; return 0 ;;
        *) return 7 ;;
      esac
      ;;
    *) return 0 ;;
  esac
}
"""
    script = fake_git + run + '\necho "reached-after-diff"\n'
    scenarios = (
        ("code", {}, 0, "", "docs_only=false"),
        ("docs", {}, 0, "reached-after-diff", "docs_only=true"),
        ("fail", {}, None, "", ""),
        ("docs", {"FAKE_TIP": "elsewhere"}, 1, "current testing tip", ""),
        ("docs", {"FAKE_MAIN_ANCESTOR": "1"}, 1, "Merge main back into testing", ""),
    )
    with tempfile.TemporaryDirectory(prefix="millivolt-docs-only-") as temp:
        scripts = Path(temp) / "scripts"
        scripts.mkdir()
        # A candidate checkout can carry any script it wants. The inline
        # classifier must ignore it, so a code diff still classifies as code.
        (scripts / "docs-paths.sh").write_text(
            "#!/usr/bin/env bash\nis_docs_path() { return 0; }\n", encoding="utf-8")
        for index, (mode, overrides, expected_code, needle, expected_output) in enumerate(scenarios):
            output = Path(temp) / f"github-output-{index}"
            env = {**os.environ, "GITHUB_OUTPUT": str(output),
                   "HEAD_SHA": "testing", "DOCS_PATH_PATTERNS": "docs/** *.md",
                   "FAKE_DIFF_MODE": mode, **overrides}
            try:
                result = subprocess.run(
                    ["bash", "-c", script], env=env, cwd=temp, capture_output=True,
                    text=True, timeout=10, check=False)
            except (OSError, subprocess.TimeoutExpired) as error:
                return [f"workflows self-check: docs-only {mode} proof: {error}"]
            if expected_code is None:
                if result.returncode == 0:
                    return [f"workflows self-check: a {mode} diff must abort classification"]
            elif result.returncode != expected_code:
                return [f"workflows self-check: a {mode} diff must exit {expected_code}, "
                        f"got {result.returncode}: {(result.stderr or result.stdout).strip()}"]
            if needle and needle.lower() not in (result.stdout + result.stderr).lower():
                return [f"workflows self-check: a {mode} diff must report {needle!r}"]
            actual = output.read_text(encoding="utf-8").strip() if output.exists() else ""
            if actual != expected_output:
                return [f"workflows self-check: a {mode} diff must write {expected_output!r}, got {actual!r}"]
            if expected_code != 0 and "reached-after-diff" in result.stdout:
                return [f"workflows self-check: a rejected {mode} classification must not continue"]
            if expected_code == 0 and "reached-after-diff" not in result.stdout:
                return [f"workflows self-check: a {mode} classification must continue"]
    return []
