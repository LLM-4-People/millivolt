#!/usr/bin/env python3
"""Regression checks for the repository's workflow and automation policy."""

from __future__ import annotations

import copy
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
from fnmatch import fnmatchcase
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github/workflows"
PIN = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$")


def load(path: Path) -> dict:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def trigger(workflow: dict):
    return workflow.get("on", workflow.get(True))


def steps(job: dict) -> list[dict]:
    value = job.get("steps", [])
    return value if isinstance(value, list) else []


def check_actions(workflow: dict, path: Path, errors: list[str]) -> None:
    for job_name, job in (workflow.get("jobs") or {}).items():
        for step in steps(job):
            uses = step.get("uses")
            if not uses:
                continue
            if not PIN.match(uses):
                errors.append(f"{path.name}: {job_name} action is not pinned to a full SHA: {uses}")
            if "@main" in uses or re.search(r"@v\d", uses):
                errors.append(f"{path.name}: {job_name} uses a mutable action reference: {uses}")
            if uses.startswith("actions/checkout@"):
                options = step.get("with") or {}
                if options.get("persist-credentials") is not False:
                    errors.append(f"{path.name}: {job_name} checkout must set persist-credentials: false")


def named_step(job: dict, name: str) -> int:
    for index, step in enumerate(steps(job)):
        if step.get("name") == name:
            return index
    return -1


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


def workflow_paths(root: Path = WORKFLOWS) -> list[Path]:
    """Every workflow file the policy checks, for both accepted suffixes."""
    return sorted(path for suffix in WORKFLOW_SUFFIXES for path in root.glob("*" + suffix))


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


def shell_function(run: str, name: str) -> str:
    """Return the body of one shell function declared at column zero."""
    match = re.search(rf"(?ms)^{re.escape(name)}\(\) \{{\n(.*?)^\}}", run)
    return match.group(1) if match else ""


def shell_declaration(run: str, name: str) -> str:
    """Return one complete shell function declaration, including braces."""
    body = shell_function(run, name)
    return f"{name}() {{\n{body}}}" if body else ""


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


def non_comment_lines(run: str) -> list[str]:
    """Return command lines only, so a commented-out command cannot satisfy a contract."""
    return [line for line in run.splitlines()
            if line.strip() and not line.lstrip().startswith("#")]


def missing_commands(run: str, commands: tuple[str, ...]) -> list[str]:
    """Return commands that are not present as their own non-comment line."""
    lines = non_comment_lines(run)
    return [
        command for command in commands
        if not any(re.fullmatch(r"\s*" + re.escape(command) + r"\s*", line) for line in lines)
    ]


def missing_needles(text: str, needles: tuple[str, ...]) -> list[str]:
    return [needle for needle in needles if needle not in text]


PUBLISH_GUARD = (
    "always() && needs.required.result == 'success' && "
    "github.repository == 'LLM-4-People/millivolt' && "
    "((github.event_name == 'push' && "
    "(github.ref == 'refs/heads/main' || startsWith(github.ref, 'refs/tags/v'))) || "
    "(github.event_name == 'pull_request' && github.event.action == 'closed' && "
    "github.event.pull_request.merged == true && github.base_ref == 'main' && "
    "github.event.pull_request.head.repo.full_name == github.repository && "
    "needs.required.outputs.merged_docs_only != 'true'))"
)
PUBLISH_TESTING_GUARD = (
    "github.event_name == 'push' && github.repository == 'LLM-4-People/millivolt' && "
    "github.ref == 'refs/heads/testing'"
)
PUBLISH_GUARD_NEEDLES = (
    "always()",
    "needs.required.result == 'success'",
    "github.event_name == 'push'",
    "github.repository == 'LLM-4-People/millivolt'",
    "github.ref == 'refs/heads/main'",
    "startsWith(github.ref, 'refs/tags/v')",
    "github.event_name == 'pull_request'",
    "github.event.action == 'closed'",
    "github.event.pull_request.merged == true",
    "github.base_ref == 'main'",
    "github.event.pull_request.head.repo.full_name == github.repository",
    "needs.required.outputs.merged_docs_only != 'true'",
)
PUBLISH_TESTING_GUARD_NEEDLES = (
    "github.event_name == 'push'",
    "github.repository == 'LLM-4-People/millivolt'",
    "github.ref == 'refs/heads/testing'",
)


def publish_guard_errors(publish_if: str, publish_testing_if: str) -> list[str]:
    """Reject any publication condition beyond the exact normalized policy.

    Positive needles alone let an extra `||` ref ride along. The exact string
    makes every event, repository and ref term part of the contract, so a new
    disjunct fails even when all required fragments remain present.
    """
    errors = []
    if publish_if != PUBLISH_GUARD:
        errors.append("check.yml: publish guard must be exactly " + PUBLISH_GUARD)
    if publish_testing_if != PUBLISH_TESTING_GUARD:
        errors.append("check.yml: publish-testing guard must be exactly " + PUBLISH_TESTING_GUARD)
    return errors



GATE_FUNCTIONS = (
    "canonical_runs",
    "canonical_run",
    "suite_outcome",
)
REQUIRED_GATE_IF = (
    "always() && (contains(needs.*.result, 'failure') || "
    "contains(needs.*.result, 'cancelled') || "
    "(contains(needs.*.result, 'skipped') && !(needs.checks.result == 'skipped' && "
    "needs.container.result == 'skipped' && "
    "((github.event_name == 'push' && github.ref == 'refs/heads/main') || "
    "(github.event_name == 'pull_request' && github.base_ref == 'main' && "
    "(github.event.action == 'closed' && github.event.pull_request.merged == true || "
    "github.event.pull_request.user.login != 'dependabot[bot]'))))))"
)
REQUIRED_JOB_IF = (
    "always() && (github.event_name != 'pull_request' || "
    "github.event.action != 'closed' || "
    "(github.event.pull_request.merged == true && github.base_ref == 'main'))"
)
FULL_GATE_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action != 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.user.login != 'dependabot[bot]' && "
    "steps.promotion.outputs.docs_only != 'true'"
)
DOCS_ONLY_VALIDATION_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action != 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.user.login != 'dependabot[bot]' && "
    "steps.promotion.outputs.docs_only == 'true'"
)
MERGED_VALIDATION_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action == 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.merged == true"
)
CONTAINER_PLATFORMS = ("linux/amd64", "linux/arm64")
CONTAINER_RUNNERS = {
    "linux/amd64": "ubuntu-24.04",
    "linux/arm64": "ubuntu-24.04-arm",
}
GATE_CHECK_NAMES = ("checks",) + tuple(
    f"container ({platform})" for platform in CONTAINER_PLATFORMS)
CHECKS_IF = (
    "(github.event_name == 'push' && "
    "(github.ref == 'refs/heads/testing' || github.ref_type == 'tag')) || "
    "(github.event_name == 'pull_request' && github.event.action != 'closed' && "
    "(github.base_ref != 'main' || "
    "github.event.pull_request.user.login == 'dependabot[bot]'))"
)
# Per-job permission allowlists. An unknown job or any write outside the exact
# expected map fails the policy: a new docs-family job may not quietly inherit
# a write scope, and only the Pages deploy job may publish.
JOB_PERMISSIONS = {
    "check.yml": {
        "checks": {},
        "container": {},
        "required": {"contents": "read", "checks": "read", "actions": "read"},
        "publish": {"contents": "write", "packages": "write"},
        "publish-testing": {"contents": "read", "packages": "write"},
    },
    "docs.yml": {"markdown": {}},
    "pages.yml": {
        "build": {"contents": "read", "pages": "read"},
        "deploy": {"pages": "write", "id-token": "write"},
    },
    "security.yml": {"govulncheck": {}},
    "dependency-review.yml": {"dependency-review": {"contents": "read"}},
}


def permission_errors(path: Path, workflow: dict, expected: dict[str, dict]) -> list[str]:
    """Reject unknown jobs and any job-level permission outside the allowlist."""
    errors = []
    jobs = workflow.get("jobs") or {}
    actual = set(jobs)
    wanted = set(expected)
    for name in sorted(actual - wanted):
        errors.append(f"{path.name}: job {name} is not in the permission allowlist")
    for name in sorted(wanted - actual):
        errors.append(f"{path.name}: allowlisted job {name} is missing")
    for name in sorted(actual & wanted):
        granted = (jobs.get(name) or {}).get("permissions") or {}
        if granted != expected[name]:
            errors.append(f"{path.name}: job {name} permissions must be exactly "
                          f"{expected[name]}, got {granted}")
    return errors


def continue_on_error_errors(workflow: dict, path: Path) -> list[str]:
    """A gate job or step may never continue past its own failure."""
    errors = []
    for job_name, job in (workflow.get("jobs") or {}).items():
        if job.get("continue-on-error") not in (None, False):
            errors.append(f"{path.name}: job {job_name} must not continue on error")
        for step in steps(job):
            if step.get("continue-on-error") not in (None, False):
                errors.append(f"{path.name}: job {job_name} step "
                              f"{step.get('name') or step.get('uses')} must not continue on error")
    return errors


MAIN_REF_NAMES = {"main", "refs/heads/main"}
# `git` may carry options before the subcommand, including a value-taking
# `-c key=value` or `-C path`. Stop at a command separator so a later `push`
# in another command cannot make this one look like a push.
GIT_PUSH = re.compile(r"\bgit\b[^;|&]*?\bpush\b")
MAIN_BROAD_PUSH_FLAGS = {"--all", "--branches", "--mirror"}


def logical_command_lines(text: str) -> list[str]:
    """Join shell continuation lines, ignoring full-line comments."""
    lines = []
    buffer = ""
    for line in non_comment_lines(text):
        stripped = line.rstrip()
        if stripped.endswith("\\"):
            buffer += stripped[:-1] + " "
            continue
        lines.append(buffer + stripped)
        buffer = ""
    if buffer:
        lines.append(buffer)
    return lines


def main_push_errors(text: str) -> list[str]:
    """Reject every non-comment git push whose destination is main.

    Covers bare and fully qualified refs, `<src>:main` and `:main` deletion
    refspecs, `main:` with an empty destination, `--delete main`, broad
    `--all`/`--branches`/`--mirror` pushes, `git -c ... push`, and commands
    split over continuation lines.
    """
    errors = []
    for line in logical_command_lines(text):
        # A line can chain commands. Check every segment so a safe push before
        # `;` or `&&` cannot hide a main push after it.
        line_has_error = False
        for segment in re.split(r"\s*(?:;|&&|\|\||\|)\s*", line):
            match = GIT_PUSH.search(segment)
            if not match:
                continue
            try:
                arguments = shlex.split(segment[match.end():])
            except ValueError:
                arguments = segment[match.end():].split()
            if any(argument in MAIN_BROAD_PUSH_FLAGS for argument in arguments):
                errors.append("check.yml: a git push line can target main: " + line.strip())
                line_has_error = True
                break
            for argument in arguments:
                if argument.startswith("-"):
                    continue
                refspec = argument.lstrip("+")
                if ":" in refspec:
                    source, destination = refspec.split(":", 1)
                    # `:main` deletes main; `main:` pushes main to an empty
                    # destination, which is still a main refspec.
                    if (destination in MAIN_REF_NAMES or
                            (not destination and source in MAIN_REF_NAMES)):
                        errors.append("check.yml: a git push line targets main: " + line.strip())
                        line_has_error = True
                        break
                elif refspec in MAIN_REF_NAMES:
                    errors.append("check.yml: a git push line targets main: " + line.strip())
                    line_has_error = True
                    break
            if line_has_error:
                break
    return errors


def container_wiring_errors(job: dict, check_step_name: str = "Check isolated container") -> list[str]:
    """Pin the native build wiring that proves each matrix leg runs natively."""
    errors = []
    if job.get("runs-on") != "${{ matrix.runner }}":
        errors.append("check.yml: container must run on ${{ matrix.runner }}")
    build = next((step for step in steps(job)
                  if step.get("name") == "Build native image without publishing"), {})
    options = build.get("with") or {}
    for key, value in (
        ("platforms", "${{ matrix.platform }}"),
        ("load", True),
        ("push", False),
        ("tags", "millivolt:check"),
        ("cache-from", "type=gha,scope=${{ matrix.cache_scope }}"),
        ("cache-to", "type=gha,mode=max,scope=${{ matrix.cache_scope }},ignore-error=true"),
    ):
        if options.get(key) != value:
            errors.append(f"check.yml: container build {key} must be exactly {value!r}")
    check = next((step for step in steps(job) if step.get("name") == check_step_name), {})
    if (check.get("env") or {}).get("IMAGE_ID") != "${{ steps.build.outputs.imageid }}":
        errors.append("check.yml: container check must consume steps.build.outputs.imageid")
    return errors



def container_matrix_errors(job: dict) -> list[str]:
    """Require one native build leg per supported platform.

    The release image is multi-platform, so the container job must prove both
    architectures natively. The exact job name is asserted separately; this
    helper owns the matrix contents.
    """
    matrix = (job.get("strategy") or {}).get("matrix") or {}
    include = matrix.get("include")
    if not isinstance(include, list):
        return ["check.yml: container matrix must declare an include list"]
    errors = []
    legs = [entry for entry in include if isinstance(entry, dict)]
    if len(legs) != len(include):
        errors.append("check.yml: every container matrix leg must be a mapping")
    platforms = [str(entry.get("platform") or "") for entry in legs]
    if sorted(platforms) != sorted(CONTAINER_PLATFORMS):
        errors.append("check.yml: container matrix must build exactly "
                      + ", ".join(CONTAINER_PLATFORMS))
    if len(set(platforms)) != len(platforms):
        errors.append("check.yml: container matrix platforms must be unique")
    for entry in legs:
        platform = entry.get("platform") or "unknown"
        for field in ("runner", "platform", "cache_scope"):
            if not entry.get(field):
                errors.append(f"check.yml: container matrix leg {platform} is missing {field}")
        expected_runner = CONTAINER_RUNNERS.get(platform)
        if expected_runner and entry.get("runner") != expected_runner:
            errors.append(
                f"check.yml: container matrix leg {platform} must run on {expected_runner}, "
                f"got {entry.get('runner')}")
    scopes = [str(entry.get("cache_scope") or "") for entry in legs]
    if len(set(scopes)) != len(scopes):
        errors.append("check.yml: container matrix cache scopes must be unique")
    return errors


def container_matrix_mutation(job: dict, platform: str, field: str | None = None) -> dict:
    """Copy the container job with one platform leg or field removed."""
    mutated = copy.deepcopy(job)
    matrix = (mutated.get("strategy") or {}).get("matrix") or {}
    include = matrix.get("include")
    if not isinstance(include, list):
        return mutated
    if field is None:
        matrix["include"] = [entry for entry in include
                             if not isinstance(entry, dict) or entry.get("platform") != platform]
    else:
        for entry in include:
            if isinstance(entry, dict) and entry.get("platform") == platform:
                entry.pop(field, None)
    return mutated


def check_run(name: str, conclusion: str | None = "success", status: str = "completed",
              suite: int = 1) -> dict:
    return {"name": name, "status": status, "conclusion": conclusion,
            "check_suite": {"id": suite}}


GATED_RUNS = (
    check_run("checks"),
    check_run("container (linux/amd64)"),
    check_run("container (linux/arm64)"),
)
SKIPPED_GATED_RUNS = (
    check_run("checks", "skipped"),
    check_run("container (linux/amd64)", "skipped"),
    check_run("container (linux/arm64)", "skipped"),
)


def required_gate_policy_errors(gate_if: str, gate_run: str) -> list[str]:
    """Require the exact required-gate predicate and a hard failure command."""
    errors = []
    if gate_if != REQUIRED_GATE_IF:
        errors.append("check.yml: required gate condition must be exactly " + REQUIRED_GATE_IF)
    if gate_run.strip() != "exit 1":
        errors.append("check.yml: required gate must fail with exactly exit 1")
    return errors


def case_branch_exit_errors(run: str, labels: tuple[str, ...]) -> list[str]:
    """Require every matching case branch to end in an explicit exit 1."""
    errors = []
    for label in labels:
        branches = list(re.finditer(rf"(?ms)^\s*{re.escape(label)}\)\n(.*?)^\s*;;", run))
        if not branches:
            errors.append(f"check.yml: gate outcome has no {label}) branch")
            continue
        for branch in branches:
            if not re.search(r"(?m)^\s*exit 1\s*$", branch.group(1)):
                errors.append(f"check.yml: {label}) branch must exit 1")
    return errors


def all_run_text(workflow: dict) -> str:
    """Join every step run value so commented commands cannot satisfy policy."""
    return "\n".join(
        str(step.get("run", ""))
        for job in (workflow.get("jobs") or {}).values()
        for step in steps(job)
        if step.get("run")
    )


def comment_commands(text: str, commands: tuple[str, ...]) -> str:
    """Comment out exact command lines so the non-comment checker must reject."""
    lines = []
    for line in text.splitlines():
        if any(line.strip() == command for command in commands):
            lines.append("# " + line)
        else:
            lines.append(line)
    return "\n".join(lines)


def run_record(sha: str, suite: int = 1, status: str = "completed",
               conclusion: str | None = "success",
               path: str = ".github/workflows/check.yml", event: str = "push",
               branch: str = "testing", created: str = "2026-01-01T00:00:00Z",
               run_id: int = 1) -> dict:
    return {
        "head_sha": sha,
        "path": path,
        "event": event,
        "head_branch": branch,
        "status": status,
        "conclusion": conclusion,
        "check_suite_id": suite,
        "created_at": created,
        "id": run_id,
    }


def workflow_run_page(runs: list[dict]) -> dict:
    return {"total_count": len(runs), "workflow_runs": runs}


def check_run_page(runs: list[dict]) -> dict:
    return {"total_count": len(runs), "check_runs": runs}


def write_fixture(path: Path, pages: list[dict]) -> None:
    path.write_text(json.dumps(pages, separators=(",", ":")), encoding="utf-8")


def suite_outcome_errors(run: str) -> list[str]:
    """Exercise the canonical suite classifier over synthetic check runs.

    The workflow owns the predicate; this test sources the same shell function
    and feeds it payloads instead of imitating its jq logic. A qualifying gate
    is one check suite containing completed, successful `checks` and both
    native container legs. Names from any other suite, pending and failed
    duplicates, and skipped leftovers are all classified explicitly.
    """
    missing = [name for name in GATE_FUNCTIONS if not shell_function(run, name)]
    if missing:
        return ["check.yml: full-gate proof is missing " + ", ".join(missing)]
    if shutil.which("bash") is None or shutil.which("jq") is None:
        return ["workflows self-check: bash and jq are required to exercise the suite classifier"]
    body = "\n".join(shell_declaration(run, name) for name in GATE_FUNCTIONS)
    script = "set -euo pipefail\n" + body + '\nsuite_outcome "$1" "$2"\n'

    def outcome(payload: dict, suite: int = 1) -> str:
        result = subprocess.run(
            ["bash", "-c", script, "bash", json.dumps(payload, separators=(",", ":")), str(suite)],
            capture_output=True, text=True, timeout=10, check=False)
        if result.returncode != 0:
            raise RuntimeError((result.stderr or result.stdout).strip())
        return result.stdout.strip()

    errors = []

    def expect(label: str, payload: dict, expected: str, suite: int = 1) -> None:
        try:
            actual = outcome(payload, suite)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
            errors.append(f"workflows self-check: {label}: {error}")
            return
        if actual != expected:
            errors.append(f"workflows self-check: {label} must classify as {expected}, got {actual}")

    expect("one suite with all three successful runs", {"check_runs": list(GATED_RUNS)}, "success")
    expect("a successful duplicate supersedes cancelled leftovers", {"check_runs": [
        check_run("checks", "cancelled", suite=1),
        check_run("checks", "success", suite=1),
        check_run("container (linux/amd64)", "success", suite=1),
        check_run("container (linux/arm64)", "success", suite=1),
    ]}, "success")
    expect("two amd64 successes plus an arm64 failure", {"check_runs": [
        check_run("checks"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)", "failure"),
    ]}, "failed")
    expect("arm64 missing from the only suite", {"check_runs": [
        check_run("checks"),
        check_run("container (linux/amd64)"),
    ]}, "incomplete")
    expect("arm64 still pending in the only suite", {"check_runs": [
        check_run("checks"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)", status="in_progress", conclusion=None),
    ]}, "pending")
    expect("success in one suite with cancelled and failed duplicates in another", {"check_runs": [
        *GATED_RUNS,
        check_run("checks", "cancelled", suite=2),
        check_run("container (linux/amd64)", "failure", suite=2),
        check_run("container (linux/arm64)", "cancelled", suite=2),
    ]}, "success")
    expect("checks, amd64 and arm64 split across three suites", {"check_runs": [
        check_run("checks", suite=1),
        check_run("container (linux/amd64)", suite=2),
        check_run("container (linux/arm64)", suite=3),
    ]}, "incomplete")
    expect("names from another suite cannot complete the canonical suite", {"check_runs": [
        check_run("checks", suite=1),
        check_run("container (linux/amd64)", suite=1),
        *[check_run(name, suite=77) for name in GATE_CHECK_NAMES],
    ]}, "incomplete")
    expect("a successful container-extra run is ignored", {"check_runs": [
        check_run("checks"),
        check_run("container (linux/amd64)"),
        check_run("container-extra"),
    ]}, "incomplete")
    expect("failure without a successful full gate", {"check_runs": [
        check_run("checks", "failure"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)"),
    ]}, "failed")
    expect("cancelled duplicate runs without a successful full gate", {"check_runs": [
        check_run("checks", "cancelled", suite=2),
        check_run("container (linux/amd64)", "cancelled", suite=2),
        check_run("container (linux/arm64)", "cancelled", suite=2),
    ]}, "failed", suite=2)
    expect("cancelled duplicate beside the current pending run", {"check_runs": [
        check_run("checks", "cancelled", suite=2),
        check_run("container (linux/amd64)", "cancelled", suite=2),
        check_run("checks", status="in_progress", conclusion=None, suite=1),
        check_run("container (linux/amd64)", status="in_progress", conclusion=None, suite=1),
        check_run("container (linux/arm64)", status="in_progress", conclusion=None, suite=1),
    ]}, "pending")
    expect("pending check runs", {"check_runs": [
        check_run("checks", status="in_progress"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)"),
    ]}, "pending")
    expect("skipped-only gate entries stay incomplete", {"check_runs": list(SKIPPED_GATED_RUNS)}, "incomplete")
    expect("non-skipped incomplete gate entries fail", {"check_runs": [
        check_run("checks"),
        check_run("container (linux/amd64)"),
    ]}, "incomplete")
    expect("a run without a check suite cannot qualify", {"check_runs": [
        {"name": name, "status": "completed", "conclusion": "success"}
        for name in GATE_CHECK_NAMES
    ]}, "incomplete")
    return errors


def full_gate_behavior_errors(run: str) -> list[str]:
    """Exercise the real full-gate step with fake git, gh and sleep.

    The step is extracted verbatim. Fake commands select a canonical run and
    suite, a forged workflow, a pull-request run, a wrong branch, an exact
    pending or incomplete run, a failed run, a docs-only gated ancestor, a code
    delta, a non-ancestor gated run, an incomplete fallback suite, no run at
    all, and a run list split past one page. The harness never injects `set -e`
    or the inline classifier: a missing guard must fail the proof.
    """
    required = missing_commands(run, ("set -euo pipefail",))
    if required:
        return ["check.yml: full-gate proof must keep these command lines: " + ", ".join(required)]
    if "source scripts/docs-paths.sh" in non_comment_lines(run):
        return ["check.yml: full-gate proof must not source the candidate classifier"]
    if shutil.which("bash") is None or shutil.which("jq") is None:
        return ["workflows self-check: bash and jq are required to exercise the full-gate proof"]
    fake_commands = r"""git() {
  case "$1" in
    merge-base)
      if [ "$2" = "--is-ancestor" ]; then
        grep -qxF "$3 $4" "$FIXTURES/ancestors"
        return $?
      fi
      echo base
      return 0
      ;;
    rev-list)
      # The proof asks for `--count <candidate>..<head>` only. A missing
      # distance fixture defaults to one so a scenario with one ancestor does
      # not need to restate it.
      range="${3:-${2:-}}"
      candidate="${range%%..*}"
      if [ -f "$FIXTURES/distance-$candidate" ]; then
        cat "$FIXTURES/distance-$candidate"
      else
        echo 1
      fi
      ;;
    diff)
      if [ -f "$FIXTURES/delta-$4" ]; then
        cat "$FIXTURES/delta-$4"
      else
        cat "$FIXTURES/delta"
      fi
      ;;
    *) return 0 ;;
  esac
}
gh() {
  local url="" countfile count
  for arg in "$@"; do url="$arg"; done
  case "$url" in
    */actions/runs\?head_sha=*)
      sha="${url#*head_sha=}"
      sha="${sha%%&*}"
      countfile="$FIXTURES/count-runs-$sha"
      count=0
      [ -f "$countfile" ] && count="$(cat "$countfile")"
      count=$((count + 1))
      printf '%s' "$count" > "$countfile"
      if [ -f "$FIXTURES/runs-$sha-$count.json" ]; then
        cat "$FIXTURES/runs-$sha-$count.json"
      else
        cat "$FIXTURES/runs-$sha.json"
      fi
      ;;
    */actions/runs\?branch=testing*)
      cat "$FIXTURES/runs-branch.json"
      ;;
    */commits/*/check-runs*)
      sha="${url#*/commits/}"
      sha="${sha%%/*}"
      countfile="$FIXTURES/count-checks-$sha"
      count=0
      [ -f "$countfile" ] && count="$(cat "$countfile")"
      count=$((count + 1))
      printf '%s' "$count" > "$countfile"
      if [ -f "$FIXTURES/checks-$sha-$count.json" ]; then
        cat "$FIXTURES/checks-$sha-$count.json"
      else
        cat "$FIXTURES/checks-$sha.json"
      fi
      ;;
    *) return 1 ;;
  esac
}
sleep() { :; }
"""
    script = fake_commands + run
    head = "headsha"
    gate = [check_run(name) for name in GATE_CHECK_NAMES]
    pending = [check_run(name, status="in_progress", conclusion=None)
               for name in GATE_CHECK_NAMES]
    scenarios = (
        {
            "label": "canonical success",
            "runs": [run_record(head)],
            "branch_runs": [],
            "checks": {head: gate},
            "ancestors": [],
            "delta": [],
            "code": 0,
            "needle": "canonical check.yml push run",
        },
        {
            "label": "forged workflow",
            "runs": [run_record(head, suite=21, path=".github/workflows/evil.yml")],
            "branch_runs": [run_record(head, suite=21, path=".github/workflows/evil.yml")],
            "checks": {head: [check_run(name, suite=21) for name in GATE_CHECK_NAMES]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical check.yml push run",
        },
        {
            "label": "pull-request event",
            "runs": [run_record(head, suite=31, event="pull_request")],
            "branch_runs": [run_record(head, suite=31, event="pull_request")],
            "checks": {head: [check_run(name, suite=31) for name in GATE_CHECK_NAMES]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical check.yml push run",
        },
        {
            "label": "wrong branch",
            "runs": [run_record(head, branch="feature")],
            "branch_runs": [run_record(head, branch="feature")],
            "checks": {head: gate},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical check.yml push run",
        },
        {
            "label": "other suite names",
            "runs": [run_record(head)],
            "branch_runs": [],
            "checks": {head: [
                check_run("checks", suite=1),
                check_run("container (linux/amd64)", suite=1),
                *[check_run(name, suite=77) for name in GATE_CHECK_NAMES],
            ]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "succeeded but its check suite is incomplete",
        },
        {
            "label": "exact pending",
            "runs": [run_record(head, status="in_progress", conclusion=None)],
            "branch_runs": [],
            "checks": {head: pending},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "still pending",
        },
        {
            # The first poll sees the run in flight; the refresh after sleep
            # must observe the completed run and its successful suite.
            "label": "pending then success",
            "runs": [run_record(head, status="in_progress", conclusion=None)],
            "runs_pages": [
                [run_record(head, status="in_progress", conclusion=None)],
                [run_record(head)],
            ],
            "branch_runs": [],
            "checks": {head: pending},
            "checks_pages": {head: [pending, gate]},
            "ancestors": [],
            "delta": [],
            "code": 0,
            "needle": "succeeded for",
        },
        {
            "label": "exact incomplete",
            "runs": [run_record(head)],
            "branch_runs": [],
            "checks": {head: [
                check_run("checks"),
                check_run("container (linux/amd64)"),
            ]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "suite is incomplete",
        },
        {
            "label": "failed run",
            "runs": [run_record(head, conclusion="failure")],
            "branch_runs": [],
            "checks": {head: [
                check_run("checks", "failure"),
                check_run("container (linux/amd64)"),
                check_run("container (linux/arm64)"),
            ]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "fallback docs delta",
            "runs": [],
            "branch_runs": [run_record("oldersha")],
            "checks": {"oldersha": gate},
            "ancestors": [("oldersha", head)],
            "delta": ["README.md", "docs/operations.md"],
            "code": 0,
            "needle": "only docs paths changed",
        },
        {
            # The older commit's run was created later (a re-run). Selection
            # must follow commit distance, not run creation time, so the
            # nearer failed commit still gates and the proof fails closed.
            "label": "re-run inversion",
            "runs": [],
            "branch_runs": [
                run_record("oldersha", created="2026-03-01T00:00:00Z", run_id=11),
                run_record("newersha", conclusion="failure",
                           created="2026-02-01T00:00:00Z", run_id=10),
            ],
            "checks": {"oldersha": gate, "newersha": gate},
            "ancestors": [("oldersha", head), ("newersha", head)],
            "distances": {"oldersha": 4, "newersha": 1},
            "delta": ["README.md"],
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "fallback code delta",
            "runs": [],
            "branch_runs": [run_record("oldersha")],
            "checks": {"oldersha": gate},
            "ancestors": [("oldersha", head)],
            "delta": ["internal/web/main.go"],
            "code": 1,
            "needle": "changes non-docs paths",
        },
        {
            "label": "fallback run failed",
            "runs": [],
            "branch_runs": [run_record("oldersha", conclusion="failure")],
            "checks": {"oldersha": gate},
            "ancestors": [("oldersha", head)],
            "delta": ["README.md"],
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "fallback non-ancestor",
            "runs": [],
            "branch_runs": [run_record("othersha")],
            "checks": {"othersha": gate},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical check.yml push run on testing is a gated ancestor",
        },
        {
            "label": "fallback default branch",
            "runs": [],
            "branch_runs": [run_record("oldersha")],
            "checks": {"oldersha": [
                check_run("checks"),
                check_run("container (linux/amd64)"),
            ]},
            "ancestors": [("oldersha", head)],
            "delta": [],
            "code": 1,
            "needle": "did not pass (incomplete)",
        },
        {
            "label": "no run",
            "runs": [],
            "branch_runs": [],
            "checks": {},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical check.yml push run on testing is a gated ancestor",
        },
        {
            "label": "paginated fallback",
            "runs": [],
            "branch_runs": [
                [run_record(f"filler{i:03d}", suite=50 + i,
                            created=f"2025-12-{1 + i % 28:02d}T00:00:00Z", run_id=100 + i)
                 for i in range(100)],
                [run_record("oldersha", created="2026-01-01T00:00:00Z", run_id=2),
                 run_record("newersha", created="2026-02-01T00:00:00Z", run_id=3)],
            ],
            "checks": {"oldersha": gate, "newersha": gate},
            "ancestors": [("oldersha", head), ("newersha", head)],
            "distances": {"oldersha": 4, "newersha": 1},
            "delta": ["internal/web/main.go"],
            "delta_by_gated": {"newersha": ["README.md"]},
            "code": 0,
            "needle": "newersha",
        },
    )
    with tempfile.TemporaryDirectory(prefix="millivolt-full-gate-") as temp:
        root = Path(temp)
        for index, scenario in enumerate(scenarios):
            case = root / str(index)
            fixtures = case / "fixtures"
            fixtures.mkdir(parents=True)
            runs = scenario["runs"]
            shas = {head} | {record["head_sha"] for record in runs}
            shas |= set(scenario["checks"])
            runs_pages = scenario.get("runs_pages") or []
            checks_pages = scenario.get("checks_pages") or {}
            for sha in shas:
                if sha == head and runs_pages:
                    for page_index, page in enumerate(runs_pages, 1):
                        write_fixture(fixtures / f"runs-{sha}-{page_index}.json",
                                      [workflow_run_page(page)])
                pages = [workflow_run_page(runs)] if sha == head and runs else []
                write_fixture(fixtures / f"runs-{sha}.json",
                              pages or [workflow_run_page([])])
                if sha in checks_pages:
                    for page_index, page_checks in enumerate(checks_pages[sha], 1):
                        write_fixture(fixtures / f"checks-{sha}-{page_index}.json",
                                      [check_run_page(page_checks)])
                checks = scenario["checks"].get(sha)
                write_fixture(fixtures / f"checks-{sha}.json",
                              [check_run_page(checks)] if checks is not None
                              else [check_run_page([])])
            for sha, distance in (scenario.get("distances") or {}).items():
                (fixtures / f"distance-{sha}").write_text(str(distance), encoding="utf-8")
            branch_pages = scenario["branch_runs"]
            if not branch_pages:
                branch_pages = [workflow_run_page([])]
            elif branch_pages and isinstance(branch_pages[0], dict):
                branch_pages = [workflow_run_page(branch_pages)]
            else:
                branch_pages = [workflow_run_page(page) for page in branch_pages]
            write_fixture(fixtures / "runs-branch.json", branch_pages)
            (fixtures / "ancestors").write_text(
                "".join(f"{candidate} {descendant}\n"
                        for candidate, descendant in scenario["ancestors"]),
                encoding="utf-8")
            (fixtures / "delta").write_text(
                "".join(f"{path}\n" for path in scenario["delta"]), encoding="utf-8")
            for gated, paths in (scenario.get("delta_by_gated") or {}).items():
                (fixtures / f"delta-{gated}").write_text(
                    "".join(f"{path}\n" for path in paths), encoding="utf-8")
            env = {**os.environ, "FIXTURES": str(fixtures), "REPO": "owner/repo",
                   "HEAD_SHA": head, "DOCS_PATH_PATTERNS": "docs/** *.md"}
            try:
                result = subprocess.run(["bash", "-c", script], env=env, cwd=case,
                                        capture_output=True, text=True, timeout=20,
                                        check=False)
            except (OSError, subprocess.TimeoutExpired) as error:
                return [f"workflows self-check: full-gate {scenario['label']} proof: {error}"]
            if result.returncode != scenario["code"]:
                return [f"workflows self-check: full-gate {scenario['label']} must exit "
                        f"{scenario['code']}, got {result.returncode}: "
                        f"{(result.stderr or result.stdout).strip()}"]
            if scenario["needle"].lower() not in (result.stdout + result.stderr).lower():
                return [f"workflows self-check: full-gate {scenario['label']} must report "
                        f"{scenario['needle']!r}"]
    return []


GATE_STEP_COMMANDS = (
    ("Check source and unit tests", ("bash scripts/check.sh",)),
    ("Check isolated browser fixtures", (
        'bash scripts/check.sh browser --target "http://${DEV_HOST}:${DEV_PORT}"',
        "MILLIVOLT_MCP_INTEGRATION=1 bash scripts/check.sh go test -count=1 ./internal/mcp/...",
    )),
)
CONTAINER_CHECK_COMMAND = 'bash scripts/check.sh container --image "$IMAGE_ID"'
BUILD_ACTION = re.compile(r"^docker/build-push-action@[0-9a-f]{40}$")


def gate_command_errors(workflow: dict) -> list[str]:
    """Pin the exact CI gate commands and reject any masking fallback.

    A renamed step, a dropped MCP command, an `|| true`, or an unpinned or
    differently identified image build would let the workflow run something
    other than the reviewed gate.
    """
    errors = []
    checks = (workflow.get("jobs") or {}).get("checks", {})
    for step_name, commands in GATE_STEP_COMMANDS:
        step = next((entry for entry in steps(checks) if entry.get("name") == step_name), {})
        if not step:
            errors.append(f"check.yml: checks must keep the {step_name} step")
            continue
        run = str(step.get("run", ""))
        missing = missing_commands(run, commands)
        if missing:
            errors.append(f"check.yml: {step_name} is missing command lines: "
                          + ", ".join(missing))
        if step.get("continue-on-error") not in (None, False):
            errors.append(f"check.yml: {step_name} must not continue on error")
        if any("||" in line for line in non_comment_lines(run)):
            errors.append(f"check.yml: {step_name} must not mask a failure with ||")
    container = (workflow.get("jobs") or {}).get("container", {})
    check = next((entry for entry in steps(container)
                  if entry.get("name") == "Check isolated container"), {})
    if missing_commands(str(check.get("run", "")), (CONTAINER_CHECK_COMMAND,)):
        errors.append("check.yml: container check must run exactly " + CONTAINER_CHECK_COMMAND)
    if check.get("continue-on-error") not in (None, False):
        errors.append("check.yml: container check must not continue on error")
    build = next((entry for entry in steps(container)
                  if entry.get("name") == "Build native image without publishing"), {})
    if build.get("id") != "build":
        errors.append("check.yml: native image build step must keep id: build")
    uses = str(build.get("uses", ""))
    if not BUILD_ACTION.fullmatch(uses):
        errors.append("check.yml: native image build must use a full-SHA pinned "
                      "docker/build-push-action, got " + uses)
    return errors


GOVULNCHECK_VERSION = "v1.1.4"
GOVULNCHECK_COMMAND = (
    f"go run golang.org/x/vuln/cmd/govulncheck@{GOVULNCHECK_VERSION} ./...")
DEPENDENCY_REVIEW_SETTINGS = {
    "fail-on-severity": "high",
    "fail-on-scopes": "runtime",
    "license-check": False,
}


def security_scan_errors(workflow: dict) -> list[str]:
    """Pin the vulnerability scanner version and its whole-module target."""
    errors = []
    scan = next((step for step in steps((workflow.get("jobs") or {}).get("govulncheck", {}))
                 if step.get("name") == "Scan the Go module graph"), {})
    command = " ".join(str(scan.get("run", "")).split())
    if command != GOVULNCHECK_COMMAND:
        errors.append("security.yml: govulncheck must run exactly " + GOVULNCHECK_COMMAND)
    return errors


def dependency_review_errors(workflow: dict) -> list[str]:
    """Pin the dependency review thresholds that fail a pull request."""
    errors = []
    review = next((step for step in steps(
        (workflow.get("jobs") or {}).get("dependency-review", {}))
        if step.get("name") == "Review dependency changes"), {})
    settings = review.get("with") or {}
    if settings != DEPENDENCY_REVIEW_SETTINGS:
        errors.append("dependency-review.yml: review settings must be exactly "
                      + str(DEPENDENCY_REVIEW_SETTINGS))
    return errors


def check_workflows(errors: list[str]) -> None:
    required = {
        "check.yml",
        "docs.yml",
        "pages.yml",
        "dependency-review.yml",
        "security.yml",
    }
    paths = workflow_paths()
    present = {path.name for path in paths}
    missing = required - present
    if missing:
        errors.append("missing workflows: " + ", ".join(sorted(missing)))
    # A workflow may use either GitHub suffix. Prove discovery is not limited
    # to *.yml, so a new *.yaml workflow cannot skip pinning or permissions.
    with tempfile.TemporaryDirectory(prefix="millivolt-workflows-") as temp:
        probe = Path(temp)
        for name in ("one.yml", "two.yaml", "ignore.txt"):
            (probe / name).write_text("", encoding="utf-8")
        found = {path.name for path in workflow_paths(probe)}
        if found != {"one.yml", "two.yaml"}:
            errors.append("workflows self-check: .yaml workflow discovery is incomplete")
    yaml_workflow = {"jobs": {"extra": {"steps": [{"uses": "actions/checkout@v4"}]}}}
    yaml_errors: list[str] = []
    check_actions(yaml_workflow, Path("extra.yaml"), yaml_errors)
    if not yaml_errors:
        errors.append("workflows self-check: a mutable action in a .yaml workflow must fail pinning")
    if not permission_errors(Path("extra.yaml"), yaml_workflow, {}):
        errors.append("workflows self-check: an unknown .yaml workflow job must fail permissions")

    for path in paths:
        workflow = load(path)
        if not isinstance(workflow, dict):
            errors.append(f"{path.name}: workflow is not a mapping")
            continue
        if "pull_request_target" in path.read_text(encoding="utf-8"):
            errors.append(f"{path.name}: pull_request_target is forbidden")
        if workflow.get("permissions") != {"contents": "read"}:
            errors.append(f"{path.name}: top-level permissions must be contents: read")
        check_actions(workflow, path, errors)
        expected_permissions = JOB_PERMISSIONS.get(path.name, {})
        errors.extend(permission_errors(path, workflow, expected_permissions))
        errors.extend(continue_on_error_errors(workflow, path))
        if path.name in {"docs.yml", "pages.yml", "security.yml", "dependency-review.yml"}:
            for job_name, granted in expected_permissions.items():
                if path.name == "pages.yml" and job_name == "deploy":
                    continue
                if any(scope != "read" for scope in granted.values()):
                    errors.append(f"{path.name}: docs-family job {job_name} must be read-only")
        for label, mutated in (
            ("adding an unknown job", copy.deepcopy(workflow)),
            ("granting a docs job contents: write", copy.deepcopy(workflow)),
            ("granting the Pages build job contents: write", copy.deepcopy(workflow)),
            ("granting the Pages build job pages: write", copy.deepcopy(workflow)),
            ("setting continue-on-error on a job", copy.deepcopy(workflow)),
        ):
            jobs = mutated.setdefault("jobs", {})
            if label == "adding an unknown job":
                jobs["extra"] = {"runs-on": "ubuntu-24.04", "steps": []}
            elif label == "granting a docs job contents: write" and path.name == "docs.yml":
                jobs["markdown"]["permissions"] = {"contents": "write"}
            elif label == "granting the Pages build job contents: write" and path.name == "pages.yml":
                jobs["build"]["permissions"] = {"contents": "write"}
            elif label == "granting the Pages build job pages: write" and path.name == "pages.yml":
                jobs["build"]["permissions"] = {"contents": "read", "pages": "write"}
            elif label == "setting continue-on-error on a job":
                next(iter(jobs.values()))["continue-on-error"] = True
            else:
                continue
            if not permission_errors(path, mutated, expected_permissions) and \
                    not continue_on_error_errors(mutated, path):
                errors.append(f"workflows self-check: {label} in {path.name} must fail")
        if path.name == "docs.yml":
            mutated = copy.deepcopy(workflow)
            mutated["jobs"]["markdown"]["steps"][0]["continue-on-error"] = True
            if not continue_on_error_errors(mutated, path):
                errors.append("workflows self-check: a continue-on-error docs step must fail")
        if path.name == "pages.yml":
            mutated = copy.deepcopy(workflow)
            mutated["jobs"]["deploy"]["permissions"].pop("pages")
            if not permission_errors(path, mutated, expected_permissions):
                errors.append("workflows self-check: dropping Pages deploy write must fail")
            mutated = copy.deepcopy(workflow)
            mutated["jobs"]["build"]["permissions"].pop("pages")
            if not permission_errors(path, mutated, expected_permissions):
                errors.append("workflows self-check: dropping Pages build read must fail")

    checks = load(WORKFLOWS / "check.yml")
    on = trigger(checks) or {}
    push = on.get("push") or {}
    if set(push.get("branches", [])) != {"main", "testing"}:
        errors.append("check.yml: push must be limited to main and testing")
    if push.get("tags") != ["v*"]:
        errors.append("check.yml: release tags must be limited to v*")
    pull_request_trigger = on.get("pull_request")
    if not isinstance(pull_request_trigger, dict):
        errors.append("check.yml: pull_request must keep the merged-PR release trigger")
    else:
        expected_pr_types = ["opened", "synchronize", "reopened", "closed"]
        if pull_request_trigger.get("types") != expected_pr_types:
            errors.append("check.yml: pull_request types must be exactly " + str(expected_pr_types))
        if "closed" not in pull_request_trigger.get("types", []):
            errors.append("check.yml: pull_request must include the closed release event")
    ignored = set(push.get("paths-ignore", []))
    for path in ("docs/**", "site-src/**", "scripts/build_site.py", "requirements-docs.txt"):
        if path not in ignored:
            errors.append(f"check.yml: docs/site path is missing from paths-ignore: {path}")
    concurrency = checks.get("concurrency") or {}
    cancel = str(concurrency.get("cancel-in-progress", ""))
    if "pull_request" not in cancel or "refs/heads/testing" not in cancel:
        errors.append("check.yml: cancel-in-progress must be limited to PRs and testing")
    if "github.event.action != 'closed'" not in cancel:
        errors.append("check.yml: a merged-PR release run must not be cancelled")
    jobs = checks.get("jobs") or {}
    required_job = jobs.get("required") or {}
    if required_job.get("name") != "required":
        errors.append("check.yml: branch-protection gate must be named required")
    if required_job.get("needs") != ["checks", "container"]:
        errors.append("check.yml: required gate must wait for checks and container")
    required_if = " ".join(str(required_job.get("if", "")).split())
    if required_if != REQUIRED_JOB_IF:
        errors.append("check.yml: required gate condition must be exactly " + REQUIRED_JOB_IF)
    for label, mutated_if in (
        ("dropping the closed-event exception",
         required_if.replace("github.event.action != 'closed'", "false")),
        ("dropping the merged check",
         required_if.replace("github.event.pull_request.merged == true", "true")),
        ("broadening the closed-event exception to testing",
         required_if.replace("github.base_ref == 'main'", "true")),
    ):
        if mutated_if == required_if:
            errors.append(f"workflows self-check: cannot mutate {label}")
        elif mutated_if == REQUIRED_JOB_IF:
            errors.append(f"workflows self-check: {label} must not satisfy the exact condition")
    if required_job.get("timeout-minutes") != 10:
        errors.append("check.yml: required gate must keep its ten-minute budget")
    required_permissions = required_job.get("permissions") or {}
    if required_permissions != {"contents": "read", "checks": "read", "actions": "read"}:
        errors.append("check.yml: required gate permissions must be exactly contents: read, "
                      "checks: read and actions: read")

    check_job = jobs.get("checks") or {}
    check_if = " ".join(str(check_job.get("if", "")).split())
    if check_if != CHECKS_IF:
        errors.append("check.yml: full gate condition must be exactly " + CHECKS_IF)
    for label, mutated_if in (
        ("inverting the Dependabot exception",
         check_if.replace("== 'dependabot[bot]'", "!= 'dependabot[bot]'")),
        ("dropping the tag event", check_if.replace("github.ref_type == 'tag'", "false")),
        ("dropping the testing branch", check_if.replace("github.ref == 'refs/heads/testing'", "false")),
        ("running the full gate on a closed PR",
         check_if.replace("github.event.action != 'closed' && ", "")),
    ):
        if mutated_if == check_if:
            errors.append(f"workflows self-check: cannot mutate {label}")
        elif mutated_if == CHECKS_IF:
            errors.append(f"workflows self-check: {label} must not satisfy the exact condition")
    if "refs/heads/main" in check_if:
        errors.append("check.yml: full gate must not run for main pushes")
    container_job = jobs.get("container") or {}
    if container_job.get("name") != "container (${{ matrix.platform }})":
        errors.append("check.yml: container job name must be exactly container (${{ matrix.platform }})")
    errors.extend(container_matrix_errors(container_job))
    wrong_runner = copy.deepcopy(container_job)
    for entry in (wrong_runner.get("strategy") or {}).get("matrix", {}).get("include", []):
        if entry.get("platform") == "linux/arm64":
            entry["runner"] = "ubuntu-24.04"
    for label, mutated in (
        ("dropping the arm64 container leg",
         container_matrix_mutation(container_job, "linux/arm64")),
        ("dropping the arm64 container runner",
         container_matrix_mutation(container_job, "linux/arm64", "runner")),
        ("swapping the arm64 native runner", wrong_runner),
    ):
        if not container_matrix_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")
    duplicate_scope = copy.deepcopy(container_job)
    scopes = (duplicate_scope.get("strategy") or {}).get("matrix", {}).get("include", [])
    if len(scopes) == 2:
        scopes[1]["cache_scope"] = scopes[0]["cache_scope"]
    if not container_matrix_errors(duplicate_scope):
        errors.append("workflows self-check: duplicating a container cache scope must fail")
    if container_job.get("needs") != "checks":
        errors.append("check.yml: container must depend on checks")
    if "if" in container_job:
        errors.append("check.yml: container must inherit the checks skip")
    errors.extend(container_wiring_errors(container_job))
    errors.extend(gate_command_errors(checks))
    def named_step_in(workflow: dict, job_name: str, step_name: str) -> dict:
        return next(step for step in steps(workflow["jobs"][job_name])
                    if step.get("name") == step_name)

    for label, mutate in (
        ("masking the core gate with || true",
         lambda workflow: named_step_in(workflow, "checks", "Check source and unit tests")
         .update({"run": "bash scripts/check.sh || true"})),
        ("letting the core gate continue on error",
         lambda workflow: named_step_in(workflow, "checks", "Check source and unit tests")
         .update({"continue-on-error": True})),
        ("dropping the live MCP command",
         lambda workflow: named_step_in(workflow, "checks", "Check isolated browser fixtures")
         .update({"run": 'bash scripts/check.sh browser --target "http://${DEV_HOST}:${DEV_PORT}"'})),
        ("renaming the image build id",
         lambda workflow: named_step_in(workflow, "container",
                                        "Build native image without publishing")
         .update({"id": "image"})),
        ("unpinning the image build action",
         lambda workflow: named_step_in(workflow, "container",
                                        "Build native image without publishing")
         .update({"uses": "docker/build-push-action@v7"})),
        ("changing the container image selector",
         lambda workflow: named_step_in(workflow, "container", "Check isolated container")
         .update({"run": 'bash scripts/check.sh container --image "$IMAGE"'})),
    ):
        mutated = copy.deepcopy(checks)
        mutate(mutated)
        if not gate_command_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")
    def build_step(job: dict) -> dict:
        return next(step for step in steps(job)
                    if step.get("name") == "Build native image without publishing")

    def check_step(job: dict) -> dict:
        return next(step for step in steps(job)
                    if step.get("name") == "Check isolated container")

    for label, mutate in (
        ("hard-coding the container platform",
         lambda job: build_step(job)["with"].update({"platforms": "linux/amd64"})),
        ("disabling the local image load",
         lambda job: build_step(job)["with"].update({"load": False})),
        ("pushing the check image",
         lambda job: build_step(job)["with"].update({"push": True})),
        ("reading the image id from the wrong output",
         lambda job: check_step(job)["env"].update(
             {"IMAGE_ID": "${{ steps.build.outputs.digest }}"})),
        ("switching the build cache to mode=min",
         lambda job: build_step(job)["with"].update(
             {"cache-to": "type=gha,mode=min,scope=${{ matrix.cache_scope }},ignore-error=true"})),
    ):
        mutated = copy.deepcopy(container_job)
        mutate(mutated)
        if not container_wiring_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")
    pull_request = on.get("pull_request")
    if isinstance(pull_request, dict) and ("paths" in pull_request or "paths-ignore" in pull_request):
        errors.append("check.yml: pull_request path filters would leave required pending")

    required_gate_index = named_step(required_job, "Require every gate")
    if required_gate_index != 0:
        errors.append("check.yml: required gate must be the first step")
    required_gate_step = steps(required_job)[required_gate_index] if required_gate_index >= 0 else {}
    gate_if = " ".join(str(required_gate_step.get("if", "")).split())
    gate_run = str(required_gate_step.get("run", ""))
    errors.extend(required_gate_policy_errors(gate_if, gate_run))
    if not required_gate_policy_errors(
            gate_if.replace("always() && ", "always() && false && "), gate_run):
        errors.append("workflows self-check: a false required-gate predicate must fail")
    if not required_gate_policy_errors(gate_if, "exit 0"):
        errors.append("workflows self-check: an exit 0 required gate must fail")
    if not required_gate_policy_errors(
            gate_if.replace("!= 'dependabot[bot]'", "== 'dependabot[bot]'"), gate_run):
        errors.append("workflows self-check: inverted Dependabot required-gate exception must fail")
    identity = next((step for step in steps(check_job) if step.get("name") == "Validate clean source identity"), {})
    if "git rev-parse HEAD" not in str(identity.get("run", "")):
        errors.append("check.yml: tag identity must use the checked-out commit")

    required_steps = steps(required_job)
    promotion_checkout = next((step for step in required_steps
                               if str(step.get("uses", "")).startswith("actions/checkout@")), {})
    checkout_options = promotion_checkout.get("with") or {}
    if checkout_options.get("fetch-depth") != 0:
        errors.append("check.yml: promotion checkout must fetch full history (fetch-depth: 0)")
    if checkout_options.get("persist-credentials") is not False:
        errors.append("check.yml: promotion checkout must set persist-credentials: false")

    merged_step = next((step for step in required_steps
                        if step.get("name") == "Validate merged promotion"), {})
    if not merged_step:
        errors.append("check.yml: merged promotion validation step is missing")
    else:
        merged_if = " ".join(str(merged_step.get("if", "")).split())
        if merged_if != MERGED_VALIDATION_IF:
            errors.append("check.yml: merged promotion validation condition must be exactly "
                          + MERGED_VALIDATION_IF)
        merged_run = str(merged_step.get("run", ""))
        for needle in (
            'set -euo pipefail',
            'gh api "repos/$REPO/git/commits/$MERGE_SHA"',
            '.parents[].sha',
            'compare/$MERGE_SHA...main',
            'check-runs?filter=all&per_page=100',
            '.name == "required" and .status == "completed"',
            'latest_required" != "success"',
            'pulls/$PR_NUMBER/files',
            '.previous_filename',
            'echo "docs_only=$docs_only" >> "$GITHUB_OUTPUT"',
        ):
            if needle not in merged_run:
                errors.append(f"check.yml: merged promotion validation is missing {needle}")
        for label, mutated_if in (
            ("dropping the merged flag", merged_if.replace(
                "github.event.pull_request.merged == true", "true")),
            ("broadening to closed testing PRs", merged_if.replace(
                "github.base_ref == 'main'", "true")),
        ):
            if mutated_if == merged_if:
                errors.append(f"workflows self-check: cannot mutate {label}")
            elif mutated_if == MERGED_VALIDATION_IF:
                errors.append(f"workflows self-check: {label} must not satisfy the exact condition")
    expected_outputs = {"merged_docs_only": "${{ steps.merged.outputs.docs_only }}"}
    if required_job.get("outputs") != expected_outputs:
        errors.append("check.yml: required must expose merged_docs_only from the merged validation")
    for step in required_steps:
        name = str(step.get("name", ""))
        step_if = " ".join(str(step.get("if", "")).split())
        if (name not in ("Require every gate", "Validate merged promotion") and
                "github.event_name == 'pull_request'" in step_if and
                "github.event.action != 'closed'" not in step_if):
            errors.append(f"check.yml: promotion step must not run on a merged PR: {name}")

    docs_patterns = " ".join(str((required_job.get("env") or {}).get("DOCS_PATH_PATTERNS", "")).split())
    if not docs_patterns:
        errors.append("check.yml: required must own the docs/site path patterns")
    promotion_step = next((step for step in required_steps
                           if step.get("name") == "Require main promotions to come from testing"), {})
    promotion_run = str(promotion_step.get("run", ""))
    check_text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    docs_script = ROOT / "scripts/docs-paths.sh"
    if docs_script.exists():
        errors.append("scripts/docs-paths.sh: the candidate classifier must not exist; "
                      "both gate steps own an inline copy")
    if "source scripts/docs-paths.sh" in check_text:
        errors.append("check.yml: gate steps must not source the candidate classifier")
    if "docs_only" not in promotion_run or "git diff --name-only --no-renames" not in promotion_run:
        errors.append("check.yml: main promotions must classify docs-only changes with --no-renames")
    capture_diff = 'changed="$(git diff --name-only --no-renames "$base" "$HEAD_SHA")"'
    missing_promotion_lines = missing_commands(
        promotion_run, ("set -euo pipefail",
                        "git fetch --no-tags origin main testing", capture_diff))
    if missing_promotion_lines:
        errors.append("check.yml: docs-only classification must keep these command lines: "
                      + ", ".join(missing_promotion_lines))
    if 'done <<< "$changed"' not in promotion_run:
        errors.append("check.yml: docs-only classification must iterate the captured diff")
    if 'done < <(git diff --name-only --no-renames' in promotion_run:
        errors.append("check.yml: docs-only classification must not swallow git diff failures in a process substitution")
    commented_capture = "\n".join(
        "# " + line if capture_diff in line else line
        for line in promotion_run.splitlines())
    if not missing_commands(commented_capture, (capture_diff,)):
        errors.append("workflows self-check: a commented-out docs-only capture must fail")
    unset_pipefail = "\n".join(
        line for line in promotion_run.splitlines()
        if line.strip() != "set -euo pipefail")
    if not missing_commands(unset_pipefail, ("set -euo pipefail",)):
        errors.append("workflows self-check: a missing set -euo pipefail must fail")
    errors.extend(docs_only_fail_open_errors(promotion_run))
    forced_docs = promotion_run.replace("\ndocs_only=true\n", "\ndocs_only=false\n", 1)
    if forced_docs == promotion_run or not docs_only_fail_open_errors(forced_docs):
        errors.append("workflows self-check: forcing docs_only false must fail the docs-only proof")
    broken_classifier = promotion_run.replace("$pattern) return 0 ;;", "$pattern) return 1 ;;", 1)
    if broken_classifier == promotion_run or not docs_only_fail_open_errors(broken_classifier):
        errors.append("workflows self-check: rejecting every docs path must fail the docs-only proof")
    full_gate_step = next((step for step in required_steps
                           if step.get("name") == "Require the full testing gate for the promoted commit"), {})
    full_gate_run = str(full_gate_step.get("run", ""))
    full_gate_if = " ".join(str(full_gate_step.get("if", "")).split())
    for label, gate_run in (("promotion", promotion_run), ("full gate", full_gate_run)):
        if any(line.lstrip().startswith("source ") for line in non_comment_lines(gate_run)):
            errors.append(f"check.yml: {label} step must not source any candidate file")
    promotion_classifier = shell_declaration(promotion_run, "is_docs_path")
    full_classifier = shell_declaration(full_gate_run, "is_docs_path")
    if not promotion_classifier:
        errors.append("check.yml: promotion classification must define is_docs_path inline")
    if not full_classifier:
        errors.append("check.yml: full-gate proof must define is_docs_path inline")
    if promotion_classifier and full_classifier and promotion_classifier != full_classifier:
        errors.append("check.yml: both inline is_docs_path declarations must be byte-identical")
    if promotion_run.count("is_docs_path() {") != 1 or full_gate_run.count("is_docs_path() {") != 1:
        errors.append("check.yml: each gate step must define exactly one inline is_docs_path")
    errors.extend(docs_classifier_errors(promotion_run, docs_patterns))
    errors.extend(docs_classifier_errors(full_gate_run, docs_patterns))
    if not docs_classifier_errors(promotion_run,
                                  docs_patterns + " .github/workflows/*.yml"):
        errors.append("workflows self-check: widening the classifier to the workflow family "
                      "must classify check.yml as docs")
    if "git merge-base --is-ancestor origin/main" not in promotion_run:
        errors.append("check.yml: main must already be merged back into testing")
    if "git rev-parse origin/testing" not in promotion_run:
        errors.append("check.yml: main promotions must use the current testing tip")
    if "DOCS_PATH_PATTERNS" not in promotion_run:
        errors.append("check.yml: docs-only promotion must use the shared docs/site path patterns")
    docs_pattern_set = set(docs_patterns.split())
    expected_docs_patterns = shell_case_family(push.get("paths-ignore", []))
    errors.extend(docs_pattern_errors(docs_pattern_set, expected_docs_patterns))
    # Regression proof: a workflow-family glob would classify check.yml as
    # docs-only, so the check.yml guard must reject it with its exact error.
    # Asserting the message rather than mere non-emptiness keeps the proof from
    # passing on the exact-family mismatch alone. A silent pass here would let a
    # workflow-only change reuse a stale testing gate.
    workflow_glob_errors = docs_pattern_errors(
        expected_docs_patterns | {WORKFLOW_FAMILY_GLOB}, expected_docs_patterns)
    if WORKFLOW_DOCS_ERROR not in workflow_glob_errors:
        errors.append("workflows self-check: adding .github/workflows/*.yml must fail the check.yml guard")
    yaml_glob_errors = docs_pattern_errors(
        expected_docs_patterns | {WORKFLOW_FAMILY_YAML_GLOB}, expected_docs_patterns)
    if WORKFLOW_DOCS_ERROR not in yaml_glob_errors:
        errors.append("workflows self-check: adding .github/workflows/*.yaml must fail the check.yml guard")
    if not full_gate_step:
        errors.append("check.yml: main promotions must prove the full testing gate")
    else:
        full_commands = missing_commands(full_gate_run, ("set -euo pipefail",))
        if full_commands:
            errors.append("check.yml: full-gate proof must keep these command lines: "
                          + ", ".join(full_commands))
        if full_gate_if != FULL_GATE_IF:
            errors.append("check.yml: full-gate condition must be exactly " + FULL_GATE_IF)
        errors.extend(case_branch_exit_errors(full_gate_run, ("pending", "*")))
        for needle in (
            "--paginate --slurp",
            "actions/runs?$1&per_page=100",
            "commits/$1/check-runs?filter=all&per_page=100",
            "git rev-list --count",
            'select(.path == ".github/workflows/check.yml")',
            'select(.event == "push")',
            'select(.head_branch == "testing")',
            "select(.head_sha == $sha)",
            "select(.check_suite.id == $suite)",
            "sort_by(.created_at, .id)",
            "branch=testing&event=push",
            "git merge-base --is-ancestor",
            "git diff --name-only --no-renames",
            "sleep 10",
            "for attempt in 1 2 3 4 5 6",
            "--argjson suite",
            "is_docs_path",
        ):
            if needle not in full_gate_run:
                errors.append(f"check.yml: full-gate proof is missing {needle}")
        if "max-count" in full_gate_run or re.search(r"\brev-list\s+-\d+", full_gate_run):
            errors.append("check.yml: full-gate fallback must not bound the walk by commit count")
        if "actions/workflows/check.yml/runs" in full_gate_run or "status=success" in full_gate_run:
            errors.append("check.yml: the gate proof must select canonical runs by path, event, branch and sha")
        for name in GATE_CHECK_NAMES:
            if f'"{name}"' not in full_gate_run:
                errors.append(f"check.yml: full-gate proof must use the exact check-run name {name}")
        if 'startswith("container")' in full_gate_run:
            errors.append("check.yml: full-gate proof must match exact check-run names, not a container prefix")
        if full_gate_step.get("env", {}).get("GH_TOKEN") != "${{ github.token }}":
            errors.append("check.yml: full-gate proof needs the checks and actions read token")
        if "steps.promotion.outputs.docs_only != 'true'" not in full_gate_if:
            errors.append("check.yml: docs-only promotions must skip the full-gate proof")
        if "dependabot[bot]" not in full_gate_if:
            errors.append("check.yml: Dependabot must keep its full-gate path")
        for name in GATE_FUNCTIONS:
            if not shell_function(full_gate_run, name):
                errors.append(f"check.yml: full-gate proof is missing {name}")
        for needle in (
            'select(.status == "completed" and .conclusion == "success")',
            "| unique as $success",
            "($success | length) == 3",
            'select(.status != "completed")',
            'select(.conclusion == "failure" or .conclusion == "cancelled"',
        ):
            if needle not in shell_function(full_gate_run, "suite_outcome"):
                errors.append(f"check.yml: suite_outcome is missing {needle}")
        if 'echo "No canonical check.yml push run exists for $HEAD_SHA; checking the closest gated testing ancestor."' not in full_gate_run:
            errors.append("check.yml: a missing exact canonical run must fall through to the closest gated ancestor")
        if 'echo "::error::The canonical check.yml push run for $HEAD_SHA succeeded but its check suite is $outcome."' not in full_gate_run:
            errors.append("check.yml: a successful run with an incomplete suite must fail the proof")
        if 'echo "::error::No canonical check.yml push run on testing is a gated ancestor of $HEAD_SHA."' not in full_gate_run:
            errors.append("check.yml: no gated ancestor must fail with its exact message")
        errors.extend(suite_outcome_errors(full_gate_run))
        errors.extend(full_gate_behavior_errors(full_gate_run))
        # Mutation proofs: every selector, the suite boundary, the fallback
        # docs delta, an early exit 0 and a `*)` branch exiting 0 must be
        # rejected by the behavioral harness.
        for old, new, label in (
            ('select(.path == ".github/workflows/check.yml")', "select(true)",
             "workflow path filter"),
            ('select(.event == "push")', "select(true)", "push event filter"),
            ('select(.head_branch == "testing")', "select(true)", "testing branch filter"),
            ("select(.check_suite.id == $suite)", "select(true)", "check-suite boundary"),
            ('if ! is_docs_path "$path"; then', "if false; then", "fallback docs delta"),
        ):
            if old not in full_gate_run:
                errors.append(f"workflows self-check: cannot mutate the gate proof {label}")
            elif not full_gate_behavior_errors(full_gate_run.replace(old, new)):
                errors.append(f"workflows self-check: weakening the gate proof {label} must fail")
        early_exit = full_gate_run.replace(
            "set -euo pipefail\n", "set -euo pipefail\nexit 0\n", 1)
        if early_exit == full_gate_run:
            errors.append("workflows self-check: cannot add an early exit to the gate proof")
        elif not full_gate_behavior_errors(early_exit):
            errors.append("workflows self-check: an early exit 0 must fail the gate proof")
        # The pending-then-success scenario fails if the loop stops refreshing
        # the run list after sleep and hands the initial pending payload to
        # every later attempt.
        refresh_after_sleep = '    runs="$(fetch_workflow_runs "head_sha=$HEAD_SHA")"\n'
        if refresh_after_sleep not in full_gate_run:
            errors.append("workflows self-check: cannot remove the post-sleep run refresh")
        elif not full_gate_behavior_errors(full_gate_run.replace(refresh_after_sleep, "", 1)):
            errors.append("workflows self-check: dropping the post-sleep run refresh must fail")
        fallback_zero = re.sub(
            r"(?ms)(^\s*\*\)\n(?:.*?)^\s*)exit 1(\s*$)",
            r"\1exit 0\2", full_gate_run, count=1)
        if fallback_zero == full_gate_run:
            errors.append("workflows self-check: cannot mutate the fallback *) branch to exit 0")
        elif not full_gate_behavior_errors(fallback_zero):
            errors.append("workflows self-check: a fallback *) branch exiting 0 must fail")
    docs_validation = next((step for step in required_steps
                            if step.get("name") == "Validate the docs-only promotion"), {})
    docs_validation_if = " ".join(str(docs_validation.get("if", "")).split())
    docs_validation_run = str(docs_validation.get("run", ""))
    if not docs_validation:
        errors.append("check.yml: docs-only promotions must validate docs, workflow policy and the site")
    else:
        if docs_validation_if != DOCS_ONLY_VALIDATION_IF:
            errors.append("check.yml: docs-only validation condition must be exactly "
                          + DOCS_ONLY_VALIDATION_IF)
        if missing_commands(docs_validation_run, ("set -euo pipefail",)):
            errors.append("check.yml: docs-only validation must keep set -euo pipefail as a command line")
        docs_commands = (
            "python3 -m pip install -r requirements-docs.txt",
            "python3 -B -O -m tests.python.site",
            "python3 -B -O -m tests.python.repository",
            "python3 -B -O -m tests.python.workflows",
            "python3 -B -m tests.repository_check",
            "python3 -B -m tests.go test -mod=readonly ./internal/mcp -count=1",
        )
        missing_docs_commands = missing_commands(docs_validation_run, docs_commands)
        if missing_docs_commands:
            errors.append("check.yml: docs-only validation is missing command lines: "
                          + ", ".join(missing_docs_commands))
        commented = "\n".join(
            "# " + line if line.lstrip().startswith("python3") else line
            for line in docs_validation_run.splitlines())
        if not missing_commands(commented, ("python3 -B -O -m tests.python.site",)):
            errors.append("workflows self-check: a commented-out docs command must not satisfy the contract")
        if docs_validation.get("env", {}).get("GOTOOLCHAIN") != "local":
            errors.append("check.yml: docs-only validation must set GOTOOLCHAIN: local")
    setup_python = next((step for step in required_steps
                         if step.get("name") == "Set up Python for docs-only validation"), {})
    if not str(setup_python.get("uses", "")).startswith("actions/setup-python@"):
        errors.append("check.yml: docs-only validation must set up Python")
    elif "steps.promotion.outputs.docs_only == 'true'" not in str(setup_python.get("if", "")):
        errors.append("check.yml: Python setup must run only for docs-only promotions")
    setup_go = next((step for step in required_steps
                     if str(step.get("uses", "")).startswith("actions/setup-go@")), {})
    if not setup_go:
        errors.append("check.yml: required must set up Go for promotion validation")
    elif "docs_only != 'true'" in str(setup_go.get("if", "")):
        errors.append("check.yml: Go setup must also run for docs-only promotions")
    version_step = next((step for step in required_steps
                         if step.get("name") == "Require an unreleased version for main promotions"), {})
    if not version_step:
        errors.append("check.yml: main promotions must require an unreleased version")
    elif "git fetch --tags" not in str(version_step.get("run", "")):
        errors.append("check.yml: version promotion check must fetch release tags")
    elif "-newer-than" not in str(version_step.get("run", "")):
        errors.append("check.yml: version promotion check must compare SemVer precedence")
    elif "steps.promotion.outputs.docs_only != 'true'" not in " ".join(str(version_step.get("if", "")).split()):
        errors.append("check.yml: docs-only promotions must skip the version check")

    publish = jobs.get("publish") or {}
    if publish.get("needs") not in ("required", ["required"]):
        errors.append("check.yml: publish must depend on the required gate")
    publish_if = " ".join(str(publish.get("if", "")).split())
    publish_missing = missing_needles(publish_if, PUBLISH_GUARD_NEEDLES)
    for needle in publish_missing:
        errors.append(f"check.yml: publish guard is missing {needle}")
    publish_testing = jobs.get("publish-testing") or {}
    if publish_testing.get("needs") != ["checks", "container"]:
        errors.append("check.yml: publish-testing must still wait for checks and container")
    publish_testing_if = " ".join(str(publish_testing.get("if", "")).split())
    publish_testing_missing = missing_needles(publish_testing_if, PUBLISH_TESTING_GUARD_NEEDLES)
    for needle in publish_testing_missing:
        errors.append(f"check.yml: publish-testing guard is missing {needle}")
    errors.extend(publish_guard_errors(publish_if, publish_testing_if))
    # Mutation proofs: dropping any one event, repository or ref condition from
    # either publication guard must be caught, not masked by the others, and an
    # extra `||` ref must fail the exact normalized contract.
    for guard, needles, label in (
        (publish_if, PUBLISH_GUARD_NEEDLES, "publish"),
        (publish_testing_if, PUBLISH_TESTING_GUARD_NEEDLES, "publish-testing"),
    ):
        for needle in needles:
            if not missing_needles(guard.replace(needle, ""), needles):
                errors.append(f"workflows self-check: removing the {label} guard {needle} must fail")
    forbidden_publish = publish_if + " || github.ref == 'refs/heads/testing'"
    if not publish_guard_errors(forbidden_publish, publish_testing_if):
        errors.append("workflows self-check: adding a forbidden ref to the publish guard must fail")
    forbidden_testing = publish_testing_if + " || github.ref == 'refs/heads/main'"
    if not publish_guard_errors(publish_if, forbidden_testing):
        errors.append("workflows self-check: adding a forbidden ref to the publish-testing guard must fail")
    # A main push deliberately skips checks and container. Without always(),
    # GitHub's implicit success gate also skips publish even though required
    # passes, so the release never gets tagged.
    without_always = publish_if.replace("always() && ", "", 1)
    if not publish_guard_errors(without_always, publish_testing_if):
        errors.append("workflows self-check: dropping always() from publish must fail")
    without_required = publish_if.replace("needs.required.result == 'success' && ", "", 1)
    if not publish_guard_errors(without_required, publish_testing_if):
        errors.append("workflows self-check: dropping the required success check must fail")

    publish_checkout = next((step for step in steps(publish)
                             if str(step.get("uses", "")).startswith("actions/checkout@")), {})
    publish_ref = " ".join(str((publish_checkout.get("with") or {}).get("ref", "")).split())
    if "github.event.pull_request.merge_commit_sha" not in publish_ref:
        errors.append("check.yml: merged-PR publish checkout must use the event merge commit")
    if "github.ref" not in publish_ref:
        errors.append("check.yml: branch and tag publish checkout must keep the pushed ref")
    verify_target = next((step for step in steps(publish)
                          if step.get("name") == "Verify release target"), {})
    verify_run = str(verify_target.get("run", ""))
    if "git rev-parse HEAD" not in verify_run or '!= "$EXPECTED"' not in verify_run:
        errors.append("check.yml: merged-PR publish must verify the checked-out merge commit")

    image_index = named_step(publish, "Publish verified multi-platform image")
    tag_push_index = named_step(publish, "Push verified tag")
    release_index = named_step(publish, "Create the GitHub release")
    if min(image_index, tag_push_index, release_index) < 0:
        errors.append("check.yml: publish image, tag push and release steps must all exist")
    elif not (image_index < tag_push_index < release_index):
        errors.append("check.yml: image must be published before the tag and release")
    image = steps(publish)[image_index] if image_index >= 0 else {}
    if image.get("with", {}).get("sbom") is not True:
        errors.append("check.yml: release image must include an SBOM")
    if image.get("with", {}).get("provenance") != "mode=max":
        errors.append("check.yml: release image must include max provenance")
    cut = next((step for step in steps(publish) if step.get("name") == "Cut the next version"), {})
    if "git push" in str(cut.get("run", "")):
        errors.append("check.yml: the cut step must not push before the image exists")
    if "git commit" in str(cut.get("run", "")):
        errors.append("check.yml: the release workflow must not create version commits")
    tag_push = steps(publish)[tag_push_index] if tag_push_index >= 0 else {}
    tag_push_run = str(tag_push.get("run", ""))
    if '"v$RELEASE_VERSION"' not in tag_push_run:
        errors.append("check.yml: the release workflow must push the verified tag")
    if "skip != 'true'" not in str(tag_push.get("if", "")):
        errors.append("check.yml: the tag push must honor the no-release guard")
    publish_text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    errors.extend(main_push_errors(publish_text))
    for label, line in (
        ("a bare main refspec", "git push origin main"),
        ("a fully qualified main refspec", "git push origin refs/heads/main"),
        ("a main refspec destination", "git push origin HEAD:refs/heads/main"),
        ("a same-name main destination", "git push origin main:main"),
        ("a short main destination", "git push origin HEAD:main"),
        ("a main deletion refspec", "git push origin :main"),
        ("a qualified main deletion refspec", "git push origin :refs/heads/main"),
        ("an empty main destination", "git push origin main:"),
        ("an empty qualified main destination", "git push origin refs/heads/main:"),
        ("a git -c main push", "git -c user.name=release push origin main"),
        ("a git -c main destination", "git -c advice.detachedHead=false push origin HEAD:main"),
        ("a git option before push", "git --no-pager push origin main"),
        ("a --delete main branch", "git push --delete origin main"),
        ("a broad --all push", "git push --all origin"),
        ("a continued main push", "git push \\\n  origin main"),
        ("a quoted main destination", "git push origin HEAD:'refs/heads/main'"),
        ("a main push chained after a safe push",
         "git push origin HEAD:testing; git push origin main"),
        ("a main push chained with &&",
         "git push origin HEAD:testing && git -c user.name=release push origin main"),
        ("a continued git -c main push",
         "git -c \\\n  user.name=release push \\\n  origin main"),
    ):
        if not main_push_errors(line):
            errors.append(f"workflows self-check: {label} must fail")
    for label, line in (
        ("a testing refspec push", "git push origin HEAD:testing"),
        ("a source-main testing destination", "git push origin main:testing"),
        ("a qualified testing destination", "git push origin HEAD:refs/heads/testing"),
        ("a git -c testing push", "git -c user.name=release push origin HEAD:testing"),
        ("a --delete feature branch", "git push --delete origin feature"),
        ("a tags-only push", "git push --tags origin"),
        ("a continued testing push", "git push \\\n  origin HEAD:testing"),
    ):
        if main_push_errors(line):
            errors.append(f"workflows self-check: {label} must pass")
    if "ghcr.io/${REPO}" in publish_text or "ghcr.io/LLM-4-People" in publish_text:
        errors.append("check.yml: release image references must use lowercase GHCR paths")
    if "value=main,enable=${{ github.ref == 'refs/heads/main' }}" in publish_text:
        errors.append("check.yml: main image must exclude prereleases")

    docs = load(WORKFLOWS / "docs.yml")
    docs_on = trigger(docs) or {}
    docs_push = docs_on.get("push") or {}
    if docs_push.get("tags-ignore") != ["**"]:
        errors.append("docs.yml: tag pushes must not run docs checks")
    if "pull_request" not in docs_on:
        errors.append("docs.yml: docs site changes need a pull request trigger")
    docs_text = all_run_text(docs)
    docs_commands = (
        "python3 -m pip install -r requirements-docs.txt",
        "python3 -B -m tests.repository_check",
        "python3 -B -O -m tests.python.site",
        "python3 -B -O -m tests.python.repository",
        "python3 -B -O -m tests.python.workflows",
        "python3 -B -m tests.go test -mod=readonly ./internal/mcp -count=1",
    )
    missing_docs = missing_commands(docs_text, docs_commands)
    if missing_docs:
        errors.append("docs.yml: docs-only validation is missing command lines: "
                      + ", ".join(missing_docs))
    for command in docs_commands:
        if not missing_commands(comment_commands(docs_text, (command,)), (command,)):
            errors.append(f"workflows self-check: a commented-out docs command must fail: {command}")
    docs_paths = docs_push.get("paths") or []
    docs_pr_paths = (docs_on.get("pull_request") or {}).get("paths") or []
    if docs_paths != docs_pr_paths:
        errors.append("docs.yml: push and pull_request docs paths must match")
    for workflow_glob in (WORKFLOW_FAMILY_GLOB, WORKFLOW_FAMILY_YAML_GLOB):
        if workflow_glob not in docs_paths:
            errors.append(f"docs.yml: workflow changes must run the policy checks: {workflow_glob}")
    # Every push that check.yml ignores must still be covered by docs.yml.
    # Literal ignores are covered by the same docs path; glob ignores are
    # covered when every concrete depth sample matches a docs path. The
    # samples must match the ignored glob itself, so the docs coverage
    # assertion cannot pass through an unrelated pattern. Nested samples keep
    # a `docs/*` or `site-src/*` narrowing from passing on a shallow path.
    errors.extend(github_glob_errors())
    ignored_paths = push.get("paths-ignore", [])
    if not ignored_paths:
        errors.append("check.yml: push paths-ignore must not be empty")
    errors.extend(docs_coverage_errors(ignored_paths, docs_paths))
    # Mutation proofs: each narrowed glob must fail the depth coverage proof.
    for pattern, narrowed in (("docs/**", "docs/*"), ("docs/**", "docs/*/*"),
                              ("site-src/**", "site-src/*"),
                              ("site-src/**", "site-src/*/*"),
                              ("**/*.md", "*.md")):
        mutated = [narrowed if path == pattern else path for path in ignored_paths]
        if not docs_coverage_errors(mutated, docs_paths):
            errors.append(f"workflows self-check: narrowing {pattern} to {narrowed} must fail")

    pages = load(WORKFLOWS / "pages.yml")
    pages_on = trigger(pages) or {}
    pages_push = pages_on.get("push") or {}
    if pages_push.get("branches") != ["main"]:
        errors.append("pages.yml: Pages must deploy from main")
    pages_paths = pages_push.get("paths") or []
    for path in ("docs/**", "site-src/**", "scripts/build_site.py",
                 "requirements-docs.txt", "VERSION", "README.md", "CONTRIBUTING.md",
                 "SECURITY.md", "LICENSE", "THIRD_PARTY_NOTICES.md",
                 ".github/workflows/pages.yml"):
        if path not in pages_paths:
            errors.append(f"pages.yml: existing deployment path is missing: {path}")
    # build_site.py reads these directly; a change outside the paths filter
    # would leave the deployed site stale. Directory inputs use the same glob
    # helper as the docs coverage checks.
    direct_site_inputs = (
        "docs/README.md",
        "site-src/templates/base.html",
        "scripts/build_site.py",
        "requirements-docs.txt",
        "VERSION",
        "README.md",
        "CONTRIBUTING.md",
        "SECURITY.md",
        "LICENSE",
        "THIRD_PARTY_NOTICES.md",
        "internal/web/static/css/dashboard.css",
        "internal/web/static/favicon.svg",
        "internal/web/static/favicon.ico",
        "internal/web/static/apple-touch-icon.png",
        "tests/repository_check.py",
        "tests/support.py",
    )
    for path in direct_site_inputs:
        if not any(path == pattern or github_glob(pattern, path) for pattern in pages_paths):
            errors.append(f"pages.yml: build_site input is not covered by a deployment path: {path}")
    if not any(github_glob(path, "README.md") for path in pages_paths):
        errors.append("pages.yml: README.md must deploy the generated index")
    pages_jobs = pages.get("jobs") or {}
    if "build" not in pages_jobs or "deploy" not in pages_jobs:
        errors.append("pages.yml: build and deploy jobs are required")
    else:
        deploy = pages_jobs["deploy"]
        if deploy.get("environment", {}).get("name") != "github-pages":
            errors.append("pages.yml: deploy must use the github-pages environment")
        permissions = deploy.get("permissions") or {}
        if permissions.get("pages") != "write" or permissions.get("id-token") != "write":
            errors.append("pages.yml: deploy needs pages: write and id-token: write")
    pages_text = all_run_text(pages)
    pages_commands = (
        "python3 -m pip install -r requirements-docs.txt",
        "python3 -B -O -m tests.python.site",
        'python3 scripts/build_site.py --output site --base-url "$SITE_URL"',
    )
    missing_pages = missing_commands(pages_text, pages_commands)
    if missing_pages:
        errors.append("pages.yml: deployment is missing command lines: "
                      + ", ".join(missing_pages))
    for command in pages_commands:
        if not missing_commands(comment_commands(pages_text, (command,)), (command,)):
            errors.append(f"workflows self-check: a commented-out pages command must fail: {command}")

    dependency = load(WORKFLOWS / "dependency-review.yml")
    dependency_on = trigger(dependency) or {}
    if "pull_request" not in dependency_on or len(dependency_on) != 1:
        errors.append("dependency-review.yml: must run only on pull_request")
    errors.extend(dependency_review_errors(dependency))
    def review_with(workflow: dict) -> dict:
        return next(step for step in steps(workflow["jobs"]["dependency-review"])
                    if step.get("name") == "Review dependency changes")["with"]

    for label, mutate in (
        ("lowering the dependency severity floor",
         lambda workflow: review_with(workflow).update({"fail-on-severity": "moderate"})),
        ("widening dependency scopes",
         lambda workflow: review_with(workflow).update({"fail-on-scopes": "all"})),
        ("enabling license checks",
         lambda workflow: review_with(workflow).update({"license-check": True})),
        ("dropping the scope setting",
         lambda workflow: review_with(workflow).pop("fail-on-scopes")),
    ):
        mutated = copy.deepcopy(dependency)
        mutate(mutated)
        if not dependency_review_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")

    security = load(WORKFLOWS / "security.yml")
    if "schedule" not in (trigger(security) or {}):
        errors.append("security.yml: vulnerability scan must run on a schedule")
    errors.extend(security_scan_errors(security))
    for label, command in (
        ("floating the govulncheck version",
         "go run golang.org/x/vuln/cmd/govulncheck@latest ./..."),
        ("narrowing the govulncheck target",
         f"go run golang.org/x/vuln/cmd/govulncheck@{GOVULNCHECK_VERSION} ./internal/..."),
        ("dropping the govulncheck module target",
         f"go run golang.org/x/vuln/cmd/govulncheck@{GOVULNCHECK_VERSION}"),
    ):
        mutated = copy.deepcopy(security)
        scan = next(step for step in steps(mutated["jobs"]["govulncheck"])
                    if step.get("name") == "Scan the Go module graph")
        scan["run"] = command
        if not security_scan_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")

    dependabot = load(ROOT / ".github/dependabot.yml")
    updates = dependabot.get("updates") or []
    if len(updates) != 5:
        errors.append("dependabot.yml: expected five ecosystems")
    for update in updates:
        if update.get("target-branch") != "testing":
            errors.append(f"dependabot.yml: {update.get('package-ecosystem')} must target testing")
        if not update.get("groups"):
            errors.append(f"dependabot.yml: {update.get('package-ecosystem')} needs a group")
        if update.get("package-ecosystem") == "docker":
            ignored = {item.get("dependency-name") for item in update.get("ignore", [])}
            if "golang" not in ignored:
                errors.append("dependabot.yml: Docker Go updates must stay coordinated with go.mod")

    requirements = (ROOT / "requirements-docs.txt").read_text(encoding="utf-8")
    if "Markdown==" not in requirements:
        errors.append("requirements-docs.txt: Markdown must be pinned")


def main() -> int:
    errors: list[str] = []
    try:
        check_workflows(errors)
    except (OSError, yaml.YAMLError) as error:
        errors.append(str(error))
    if errors:
        for error in errors:
            print("workflows: " + error)
        return 1
    print("workflows: clean")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
