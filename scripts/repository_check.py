#!/usr/bin/env python3
"""Small offline publication checks; no Git checkout or third-party packages needed."""

import os
from pathlib import Path
import re
import subprocess
import tempfile
from urllib.parse import unquote, urlsplit

IGNORED_PATHS = (
    "proxy.yaml", "proxy", "stress", "proxy.db", "proxy.db-wal", "proxy.db-shm",
    "proxy.log", "node_modules/fixture/index.js", ".venv/bin/python",
    "scripts/__pycache__/fixture.cpython-313.pyc", ".env", ".env.local",
    ".proxy.yaml-incomplete", "operator.local.yaml", "other.db-journal",
    "other.sqlite-wal", "other.sqlite3-shm", "credential.pem", "credential.key",
    "credential.p12", "credential.pfx", "backups/history.json", "captures/request.json",
    "exports/history.jsonl", "profile.pprof", "proxy.test", "bin/proxy",
    "dist/proxy", "test-results/result.json", "playwright-report/index.html",
    "docs_datadoghq_com_.txt", "grafana_com_.txt",
)
PUBLIC_PATHS = (
    "proxy.example.yaml", "cmd/proxy/main.go", "cmd/stress/main.go",
    "internal/config/config.go", "go.mod", "go.sum", "package.json",
    "package-lock.json", ".node-version", ".python-version", "requirements-dev.txt",
    "README.md", "AGENTS.md", "CONTRIBUTING.md", "SECURITY.md", "LICENSE",
    "THIRD_PARTY_NOTICES.md", "internal/web/static/vendor/uplot.LICENSE",
    "docs/architecture.md", "scripts/check.sh", "scripts/repository_check.py",
    "Dockerfile", ".dockerignore", "compose.yaml", "compose.dev.yaml", "VERSION",
    "version.go", "cmd/release/main.go", "scripts/containercheck/main.go",
    "scripts/licenses/main.go", "deploy/nginx.conf", "docs/reverse-proxy.md",
    "docs/images/dashboard.png", "docs/images/explorer.png",
    ".github/workflows/check.yml",
    ".github/dependabot.yml", ".github/pull_request_template.md", ".env.example",
)
# Deliberately limited to the inline/reference-link syntax used by these docs.
LINK = re.compile(r'!?\[[^\]\n]*\]\(\s*(<[^>\n]+>|[^\s)]+)')
REFERENCE = re.compile(r'^\s{0,3}\[[^\]\n]+\]:\s*(<[^>\n]+>|[^\s]+)')
FENCE = re.compile(r'^ {0,3}(' + chr(96) + r'{3,}|~{3,})')


def markdown_errors(root):
    root = Path(root).resolve()
    files = sorted([*root.glob("*.md"), *(root / "docs").rglob("*.md")])
    errors = []
    for file in files:
        fence = None
        for number, line in enumerate(file.read_text(encoding="utf-8").splitlines(), 1):
            marker = FENCE.match(line)
            if marker:
                value = marker.group(1)
                if fence is None:
                    fence = value
                elif value[0] == fence[0] and len(value) >= len(fence):
                    fence = None
                continue
            if fence is not None:
                continue
            targets = [match.group(1) for match in LINK.finditer(line)]
            reference = REFERENCE.match(line)
            if reference:
                targets.append(reference.group(1))
            for raw in targets:
                target = raw[1:-1] if raw.startswith("<") else raw
                parsed = urlsplit(target)
                if parsed.scheme or parsed.netloc or not parsed.path:
                    continue
                relative = unquote(parsed.path)
                candidate = (root / relative.lstrip("/") if relative.startswith("/")
                             else file.parent / relative).resolve()
                if not candidate.is_relative_to(root) or not candidate.exists():
                    errors.append(f"{file.relative_to(root)}:{number}: missing local link {target}")
    return errors


def git_environment():
    # Inherited GIT_DIR/WORK_TREE/CONFIG_* can redirect even a -C invocation.
    env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
    env["GIT_CONFIG_NOSYSTEM"] = "1"
    env["GIT_CONFIG_GLOBAL"] = os.devnull
    return env


def typography_errors(root, names):
    errors = []
    root = Path(root)
    for name in names:
        file = root / name
        # A symlink publishes its path, not the potentially private target.
        if file.is_symlink() or not file.is_file():
            continue
        try:
            content = file.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue  # Binary assets are not prose or source text.
        if "\0" in content:
            continue
        for number, line in enumerate(content.splitlines(), 1):
            if chr(0x2014) in line:
                errors.append(f"{name}:{number}: use sentence punctuation or '-' instead of an em dash")
    return errors


def publication_errors(root):
    root = Path(root).resolve()
    env = git_environment()
    with tempfile.TemporaryDirectory(prefix="millivolt-repository-check-") as temp:
        directory = Path(temp)
        (directory / ".gitignore").write_bytes((root / ".gitignore").read_bytes())
        initialized = subprocess.run(
            ["git", "-c", "init.defaultBranch=check", "init", "--quiet", temp],
            env=env, capture_output=True, text=True,
        )
        if initialized.returncode:
            raise RuntimeError("temporary git init failed: " + initialized.stderr.strip())
        paths = (*IGNORED_PATHS, *PUBLIC_PATHS)
        result = subprocess.run(
            ["git", "-c", "core.excludesFile=" + os.devnull, "-C", temp,
             "check-ignore", "--no-index", "--stdin", "-z"],
            input="\0".join(paths) + "\0", env=env, capture_output=True, text=True,
        )
        if result.returncode not in (0, 1):
            raise RuntimeError("git check-ignore failed: " + result.stderr.strip())
        ignored = set(result.stdout.split("\0"))
        # The same isolated Git owner enumerates the publication candidate tree.
        # This works before the first commit and in archives without a .git;
        # ignored runtime state, dependencies and private config stay unread.
        files = subprocess.run(
            ["git", "-c", "core.excludesFile=" + os.devnull, "-C", temp,
             "--work-tree=" + str(root), "ls-files", "--others", "--exclude-standard", "-z"],
            env=env, capture_output=True, text=True,
        )
        if files.returncode:
            raise RuntimeError("git publication listing failed: " + files.stderr.strip())
        typography = typography_errors(root, sorted(filter(None, files.stdout.split("\0"))))
    return ([f"sensitive/runtime path is not ignored: {name}" for name in IGNORED_PATHS if name not in ignored]
            + [f"public source is ignored: {name}" for name in PUBLIC_PATHS if name in ignored]
            + typography)


def check(root):
    return markdown_errors(root) + publication_errors(root)


def main():
    root = Path(__file__).resolve().parent.parent
    try:
        errors = check(root)
    except (OSError, RuntimeError, ValueError) as error:
        raise SystemExit("repository check: " + str(error)) from error
    if errors:
        raise SystemExit("\n".join(errors))
    print("repository checks passed: local Markdown targets, publication ignore rules and typography")


if __name__ == "__main__":
    main()
