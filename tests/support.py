"""Shared repository location and private-state-safe source discovery."""

import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parent.parent


def require(value, message):
    """Test safety gates must remain active when Python runs with -O."""
    if not value:
        raise RuntimeError(message)


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
