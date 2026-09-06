"""Run native Go tooling with physically centralized, package-private tests.

Physical Go files have no redundant test suffix. The temporary overlay adds
Go's required *_test.go names to their original packages.
Production sources, go.mod and go.sum are never replaced or copied.
"""

from contextlib import contextmanager
import json
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile

from .support import ROOT, git_environment, require, source_inventory, test_layout_errors


def discover(root):
    root = Path(root).resolve(strict=True)
    location = root / "tests" / "_go"
    require(location.is_dir() and location.resolve() == location,
            "missing or symlinked tests/_go directory")
    _, public = source_inventory(root)
    misplaced = test_layout_errors(public)
    require(not misplaced, "; ".join(misplaced))
    candidates = {name for name in public
                  if name.endswith(".go") and Path(name).is_relative_to("tests/_go")}
    require(candidates, "no Go tests discovered")
    for path in location.rglob("*"):
        require(not path.is_symlink(), "symlink in Go test tree: " + str(path.relative_to(root)))
        if path.suffix == ".go":
            require(path.relative_to(root).as_posix() in candidates,
                    "ignored Go source in tests/_go: " + str(path.relative_to(root)))
    replacements = {}
    for name in sorted(candidates):
        backing = root / name
        relative = backing.relative_to(location)
        require(not any(part.startswith((".", "_")) or part == "testdata" for part in relative.parts),
                "Go ignores this test path: " + name)
        target = root / relative.with_name(relative.stem + "_test.go")
        require(backing.is_file() and backing.resolve() == backing, "invalid backing test: " + name)
        require(target.parent.is_dir() and target.parent.resolve() == target.parent,
                "missing or symlinked source package for " + name)
        require(not target.exists() and not target.is_symlink(), "duplicate source-neighbor test: " + str(relative))
        require(str(target) not in replacements, "duplicate Go overlay target: " + str(relative))
        replacements[str(target)] = str(backing)
    return replacements


@contextmanager
def overlay(root):
    replacements = discover(root)
    with tempfile.TemporaryDirectory(prefix="millivolt-go-tests-") as directory:
        path = Path(directory) / "overlay.json"
        path.write_text(json.dumps({"Replace": replacements}, sort_keys=True) + "\n", encoding="utf-8")
        yield path, replacements


def package_parity(replacements, document):
    """Every physical test must appear in Go's native package discovery.

    IgnoredGoFiles accounts for legitimate platform/build-tag exclusions. No
    second package registry or fixed test count can silently drift from Go.
    """
    decoder = json.JSONDecoder()
    found = set()
    document = document.lstrip()
    while document:
        package, end = decoder.raw_decode(document)
        require(isinstance(package, dict) and isinstance(package.get("Dir"), str),
                "invalid Go package metadata")
        for field in ("TestGoFiles", "XTestGoFiles", "IgnoredGoFiles"):
            names = package.get(field, [])
            require(isinstance(names, list), "invalid Go test file metadata")
            for name in names:
                require(isinstance(name, str), "invalid Go test filename")
                if name.endswith("_test.go"):
                    path = str(Path(package["Dir"]) / name)
                    require(path not in found, "Go discovered a duplicate test: " + path)
                    found.add(path)
        document = document[end:].lstrip()
    require(found == set(replacements),
            "Go test discovery differs from physical tests: " + repr(sorted(found ^ set(replacements))))


def go_command(arguments, root, env, capture=False):
    return subprocess.run(["go", *arguments], cwd=root, env=env,
                          capture_output=capture, text=True)


def run(arguments, root=ROOT):
    arguments = list(arguments)
    require(arguments, "usage: scripts/check.sh go test|vet|list|build|mod tidy [arguments]")
    offset = 2 if arguments[:2] == ["mod", "tidy"] else 1
    require(offset == 2 or arguments[0] in ("test", "vet", "list", "build"),
            "supported Go commands: test, vet, list, build, mod tidy")
    env = git_environment()
    # Test execution owns its source view and module. Do not allow caller flags
    # to replace the overlay or redirect package/dependency discovery elsewhere.
    for argument in [*arguments[offset:], *shlex.split(env.get("GOFLAGS", ""))]:
        require(argument.split("=", 1)[0].lstrip("-") not in ("overlay", "C", "modfile"),
                "overlay, working directory and module file are owned by the test runner")
    env["GOWORK"] = "off"
    root = Path(root).resolve(strict=True)
    with overlay(root) as (path, replacements):
        flag = "-overlay=" + str(path)

        def verify():
            result = go_command(["list", flag, "-mod=readonly", "-json", "./..."], root, env, capture=True)
            require(result.returncode == 0, "Go package discovery failed: " + result.stderr.strip())
            package_parity(replacements, result.stdout)

        # Tidy can introduce a new test-only dependency; validate its package
        # graph after a successful update, not before the dependency exists.
        if offset != 2:
            verify()
        result = go_command([*arguments[:offset], flag, *arguments[offset:]], root, env)
        if offset == 2 and result.returncode == 0:
            verify()
        return result.returncode


def main():
    try:
        result = run(sys.argv[1:])
    except (OSError, RuntimeError, ValueError) as error:
        raise SystemExit("Go checks: " + str(error)) from error
    raise SystemExit(result)


if __name__ == "__main__":
    main()
