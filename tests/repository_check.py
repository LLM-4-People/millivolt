#!/usr/bin/env python3
"""Small offline publication checks; no Git checkout or third-party packages needed."""

from pathlib import Path
import re
from urllib.parse import unquote, urlsplit

from .support import ROOT, git_environment, source_inventory, test_layout_errors

IGNORED_PATHS = (
    "AGENTS.md", "agents.md", "internal/AGENTS.md",
    "proxy.yaml", "proxy", "stress", "proxy.db", "proxy.db-wal", "proxy.db-shm",
    "proxy.log", "node_modules/fixture/index.js", ".venv/bin/python",
    "tests/__pycache__/fixture.cpython-313.pyc", ".env", ".env.local",
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
    "README.md", "CONTRIBUTING.md", "SECURITY.md", "LICENSE",
    "THIRD_PARTY_NOTICES.md", "internal/web/static/vendor/uplot.LICENSE",
    "docs/architecture.md", "scripts/check.sh", "tests/repository_check.py",
    "Dockerfile", ".dockerignore", "compose.yaml", "compose.dev.yaml", "VERSION",
    "version.go", "cmd/release/main.go", "tests/containercheck/main.go",
    "tests/go.py", "tests/python/go.py", "tests/python/repository.py", "tests/python/browser.py",
    "tests/python/backup_db.py", "tests/python/release.py", "tests/support.py", "tests/README.md",
    "tests/_go/version.go", "tests/_go/internal/config/config.go",
    "tests/ui_check.js", "tests/browser_check.py", "tests/explorer_check.py",
    "scripts/licenses/main.go", "deploy/nginx.conf", "docs/reverse-proxy.md",
    "docs/dashboard.md", "docs/images/overview/dashboard.png", "docs/images/explorer/providers.png",
    "docs/images/explorer/models.png", "docs/images/charts/tokens.png", "docs/images/charts/speed-latency.png",
    "docs/images/charts/cost.png", "docs/images/settings/dashboard.png",
    ".github/workflows/check.yml",
    ".github/dependabot.yml", ".github/pull_request_template.md", ".env.example",
)
# Deliberately limited to the inline/reference-link syntax used by these docs.
LINK = re.compile(r'!?\[[^\]\n]*\]\(\s*(<[^>\n]+>|[^\s)]+)')
REFERENCE = re.compile(r'^\s{0,3}\[[^\]\n]+\]:\s*(<[^>\n]+>|[^\s]+)')
FENCE = re.compile(r'^ {0,3}(' + chr(96) + r'{3,}|~{3,})')


def markdown_errors(root, names=None):
    root = Path(root).resolve()
    if names is None:
        _, names = source_inventory(root)
    public = set(names)
    directories = {parent.as_posix() for name in names for parent in Path(name).parents}
    files = sorted(root / name for name in names if name.endswith(".md"))
    errors = []
    for file in files:
        if file.is_symlink():
            continue
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
                relative_target = candidate.relative_to(root).as_posix() if candidate.is_relative_to(root) else None
                if (not candidate.exists() or relative_target is None
                        or relative_target not in public and relative_target not in directories):
                    errors.append(f"{file.relative_to(root)}:{number}: missing local link {target}")
    return errors


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


def publication_errors(root, inventory=None):
    root = Path(root).resolve()
    ignored, files = inventory if inventory is not None else source_inventory(root, (*IGNORED_PATHS, *PUBLIC_PATHS))
    typography = typography_errors(root, files)
    return ([f"sensitive/runtime path is not ignored: {name}" for name in IGNORED_PATHS if name not in ignored]
            + [f"public source is ignored: {name}" for name in PUBLIC_PATHS if name in ignored]
            + typography)


def check(root):
    inventory = source_inventory(root, (*IGNORED_PATHS, *PUBLIC_PATHS))
    return (markdown_errors(root, inventory[1]) + publication_errors(root, inventory)
            + test_layout_errors(inventory[1]))


def main():
    try:
        errors = check(ROOT)
    except (OSError, RuntimeError, ValueError) as error:
        raise SystemExit("repository check: " + str(error)) from error
    if errors:
        raise SystemExit("\n".join(errors))
    print("repository checks passed: local Markdown targets, publication ignore rules and typography")


if __name__ == "__main__":
    main()
