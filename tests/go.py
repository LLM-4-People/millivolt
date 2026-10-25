"""Run native Go tooling with physically centralized, package-private tests.

Physical Go files have no redundant test suffix. The temporary overlay adds
Go's required *_test.go names to their original packages.
Production sources, go.mod and go.sum are never replaced or copied.
"""

from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
import tempfile

from .support import ROOT, git_environment, require, source_inventory, test_layout_errors

# The sharder proves whole-module package parity once, then hands this private
# fingerprint to the builds it starts in the same run. It is an internal
# handshake, not a caller flag: the value covers the overlay file mapping and
# the environment that decides which sources Go sees, so a mismatched or stale
# handshake verifies again instead of trusting the caller.
PARITY_HANDSHAKE = "MILLIVOLT_GO_PARITY"
PARITY_ENVIRONMENT = ("CGO_ENABLED", "GOARCH", "GOEXPERIMENT", "GOFLAGS", "GOOS", "GOTOOLCHAIN")

# Go's filename platform suffix vocabulary, from internal/syslist, split by
# suffix position. A test ignored for any other reason is a silent skip: it
# satisfies physical/native parity but never runs, so the overlay rejects it
# instead of counting it as covered.
PLATFORM_OS = frozenset("""
    aix android darwin dragonfly freebsd hurd illumos ios js linux nacl netbsd
    openbsd plan9 solaris wasip1 windows zos
""".split())
PLATFORM_ARCH = frozenset("""
    386 amd64 amd64p32 arm armbe arm64 arm64be loong64 mips mipsle mips64
    mips64le mips64p32 mips64p32le ppc ppc64 ppc64le riscv riscv64 s390 s390x
    sparc sparc64 wasm
""".split())
PLATFORM_TAGS = PLATFORM_OS | PLATFORM_ARCH
# `unix` is the platform-family feature tag Go defines for unix-like GOOS
# values. Every other feature or release tag would make the exclusion depend on
# something other than the platform, so it is not a genuine platform exclusion.
PLATFORM_CONSTRAINT_TAGS = PLATFORM_TAGS | {"unix"}
UNIX_PLATFORMS = frozenset("""
    aix android darwin dragonfly freebsd hurd illumos ios linux netbsd
    openbsd solaris
""".split())


def platform_suffix(name):
    """Return the GOOS/GOARCH pair named by Go's filename suffix, if any."""
    stem = name[:-len("_test.go")] if name.endswith("_test.go") else name[:-len(".go")]
    parts = stem.rsplit("_", 2)
    if len(parts) < 2:
        return None
    last = parts[-1]
    if last in PLATFORM_OS:
        return last, None
    if last in PLATFORM_ARCH:
        if len(parts) == 3 and parts[-2] in PLATFORM_OS:
            return parts[-2], last
        return None, last
    return None


def platform_test_name(name):
    """True when Go's platform suffix explains why it ignored this test file."""
    return platform_suffix(name) is not None


def _constraint_tokens(expression):
    tokens = []
    index = 0
    while index < len(expression):
        if expression[index].isspace():
            index += 1
            continue
        for operator in ("&&", "||"):
            if expression.startswith(operator, index):
                tokens.append(operator)
                index += len(operator)
                break
        else:
            character = expression[index]
            if character in "!()":
                tokens.append(character)
                index += 1
            else:
                match = re.match(r"[A-Za-z0-9_.]+", expression[index:])
                if not match:
                    raise ValueError("invalid Go build constraint: " + expression)
                tokens.append(match.group(0))
                index += len(match.group(0))
    return tokens


def _parse_constraint(expression):
    """Parse a //go:build expression into a boolean tag tree."""
    tokens = _constraint_tokens(expression)

    def parse_or(index):
        node, index = parse_and(index)
        while index < len(tokens) and tokens[index] == "||":
            right, index = parse_and(index + 1)
            node = ("or", node, right)
        return node, index

    def parse_and(index):
        node, index = parse_unary(index)
        while index < len(tokens) and tokens[index] == "&&":
            right, index = parse_unary(index + 1)
            node = ("and", node, right)
        return node, index

    def parse_unary(index):
        if index >= len(tokens):
            raise ValueError("invalid Go build constraint: " + expression)
        token = tokens[index]
        if token == "!":
            node, index = parse_unary(index + 1)
            return ("not", node), index
        if token == "(":
            node, index = parse_or(index + 1)
            if index >= len(tokens) or tokens[index] != ")":
                raise ValueError("invalid Go build constraint: " + expression)
            return node, index + 1
        if token in ("&&", "||", ")"):
            raise ValueError("invalid Go build constraint: " + expression)
        return ("tag", token), index + 1

    node, index = parse_or(0)
    if index != len(tokens):
        raise ValueError("invalid Go build constraint: " + expression)
    return node


def _parse_legacy_constraint(expression):
    """Parse a legacy // +build line: space is OR, comma is AND."""
    groups = []
    for option in expression.split():
        terms = []
        for term in option.split(","):
            negated = term.startswith("!")
            name = term[1:] if negated else term
            if not name:
                raise ValueError("invalid Go build constraint: " + expression)
            atom = ("tag", name)
            terms.append(("not", atom) if negated else atom)
        node = terms[0]
        for term in terms[1:]:
            node = ("and", node, term)
        groups.append(node)
    if not groups:
        raise ValueError("invalid Go build constraint: " + expression)
    node = groups[0]
    for group in groups[1:]:
        node = ("or", node, group)
    return node


def _build_constraints(content):
    """Return the file's build constraints, which must precede the package."""
    constraints = []
    for line in content.splitlines():
        if re.match(r"^\s*package\s", line):
            break
        match = re.match(r"^\s*//go:build\s+(.+?)\s*$", line)
        if match:
            constraints.append(_parse_constraint(match.group(1)))
        match = re.match(r"^\s*//\s*\+build\s+(.+?)\s*$", line)
        if match:
            constraints.append(_parse_legacy_constraint(match.group(1)))
    return constraints


def _constraint_tags(node):
    if node[0] == "tag":
        return {node[1]}
    if node[0] == "not":
        return _constraint_tags(node[1])
    return _constraint_tags(node[1]) | _constraint_tags(node[2])


def _race_constrained(content):
    """True when any build constraint on the file mentions the race tag."""
    try:
        constraints = _build_constraints(content)
    except ValueError:
        return False
    return any("race" in _constraint_tags(constraint) for constraint in constraints)


# Go recognizes a test, benchmark, fuzz target or example by its top-level
# function name. Race-gated files may only carry helpers, so this predicate
# deliberately errs on the side of rejecting a name that begins with one of
# those prefixes.
_TEST_FUNCTION = re.compile(
    r"(?m)^func\s+(?:Test|Benchmark|Fuzz|Example)[A-Za-z0-9_]*\s*\(")


def _constraint_matches(node, tags):
    if node[0] == "tag":
        return node[1] in tags
    if node[0] == "not":
        return not _constraint_matches(node[1], tags)
    if node[0] == "and":
        return _constraint_matches(node[1], tags) and _constraint_matches(node[2], tags)
    return _constraint_matches(node[1], tags) or _constraint_matches(node[2], tags)


def platform_constraint_only(name, content):
    """True when only the filename platform and platform build tags exclude it.

    A platform suffix is necessary but not sufficient: ``value_linux.go`` with
    ``//go:build integration`` is ignored for the integration tag, so it would
    never run under the default suite. The constraints must name only platform
    tags and must select at least one GOOS/GOARCH consistent with the suffix.
    """
    suffix = platform_suffix(name)
    if suffix is None:
        return False
    try:
        constraints = _build_constraints(content)
    except ValueError:
        return False
    tags = set()
    for constraint in constraints:
        tags.update(_constraint_tags(constraint))
    if not tags <= PLATFORM_CONSTRAINT_TAGS:
        return False
    platforms = [suffix[0]] if suffix[0] else sorted(PLATFORM_OS)
    architectures = [suffix[1]] if suffix[1] else sorted(PLATFORM_ARCH)
    for goos in platforms:
        for goarch in architectures:
            environment = {goos, goarch}
            if goos in UNIX_PLATFORMS:
                environment.add("unix")
            if all(_constraint_matches(constraint, environment) for constraint in constraints):
                return True
    return False


def test_source(replacements, package_dir, name):
    """Read a discovered test from its real file, following the overlay map."""
    path = str(Path(package_dir) / name)
    source = replacements.get(path, path)
    try:
        return Path(source).read_text(encoding="utf-8")
    except OSError as error:
        raise RuntimeError("cannot inspect Go test: " + path) from error


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


def _backing_digest(path):
    """Hash one overlay backing file's bytes.

    A content-only edit can change Go's file selection (adding a //go:build
    constraint) while the path map and environment stay identical, so a
    digest over paths alone certifies a selection that no longer exists.
    """
    try:
        return hashlib.sha256(Path(path).read_bytes()).hexdigest()
    except OSError as error:
        raise RuntimeError("cannot hash Go test backing file: " + str(path)) from error


def overlay_digest(replacements, environment):
    """Fingerprint an overlay's paths, backing bytes and selection environment.

    The handshake reuses one parity listing only while every input to that
    listing is unchanged: which virtual path maps to which backing file, what
    those files contain (their build constraints select tests), and the Go
    file-selection environment.
    """
    digest = hashlib.sha256()
    digest.update(json.dumps(replacements, sort_keys=True).encode("utf-8"))
    for virtual in sorted(replacements):
        digest.update(b"\0" + virtual.encode("utf-8") + b"\0"
                      + _backing_digest(replacements[virtual]).encode("ascii"))
    for name in PARITY_ENVIRONMENT:
        digest.update(b"\0" + name.encode("ascii") + b"="
                      + environment.get(name, "").encode("utf-8"))
    return digest.hexdigest()


def parity_digest(root=ROOT, environment=None):
    """Fingerprint the overlay whose package parity a run has already proven."""
    environment = os.environ if environment is None else environment
    return overlay_digest(discover(Path(root)), environment)


def _packages(document):
    """Yield package metadata from a ``go list -json`` stream."""
    decoder = json.JSONDecoder()
    document = document.lstrip()
    while document:
        package, end = decoder.raw_decode(document)
        require(isinstance(package, dict) and isinstance(package.get("Dir"), str),
                "invalid Go package metadata")
        yield package
        document = document[end:].lstrip()


def _listed_tests(document, fields):
    """Return the native test paths the named package fields list."""
    paths = set()
    for package in _packages(document):
        for field in fields:
            names = package.get(field, [])
            require(isinstance(names, list), "invalid Go test file metadata")
            for name in names:
                require(isinstance(name, str), "invalid Go test filename")
                if name.endswith("_test.go"):
                    paths.add(str(Path(package["Dir"]) / name))
    return paths


def package_parity(replacements, document, race_document=None):
    """Every physical test must appear in Go's native package discovery.

    IgnoredGoFiles is accepted when the filename suffix and the file's own
    build constraints together make this a genuine GOOS/GOARCH exclusion. A
    race-gated file is accepted only when a second listing taken with the race
    tag discovers the same path, and only as a helper: a race-tagged file that
    declares a Test, Benchmark, Fuzz or Example function is rejected because
    one pass can list it while the other's binary silently excludes it. A
    platform-suffixed file behind ``//go:build integration`` is still a silent
    skip, and every other non-platform tag stays a hard error. No second
    package registry or fixed test count can silently drift from Go.
    """
    alternate = None if race_document is None else _listed_tests(
        race_document, ("TestGoFiles", "XTestGoFiles"))
    found = set()
    for package in _packages(document):
        for field in ("TestGoFiles", "XTestGoFiles", "IgnoredGoFiles"):
            names = package.get(field, [])
            require(isinstance(names, list), "invalid Go test file metadata")
            for name in names:
                require(isinstance(name, str), "invalid Go test filename")
                if name.endswith("_test.go"):
                    path = str(Path(package["Dir"]) / name)
                    if field == "IgnoredGoFiles":
                        platform = platform_test_name(name)
                        if platform:
                            content = test_source(replacements, package["Dir"], name)
                            platform = platform_constraint_only(name, content)
                        require(platform or (alternate is not None and path in alternate),
                                "Go ignored a test for a non-platform reason, so it never runs: " + path)
                    if path in replacements:
                        content = test_source(replacements, package["Dir"], name)
                        require(not (_race_constrained(content)
                                     and _TEST_FUNCTION.search(content)),
                                "Go test " + path + " is race-gated and defines a test function, "
                                "so one pass can silently skip it; race-tagged files may be helpers only")
                    require(path not in found, "Go discovered a duplicate test: " + path)
                    found.add(path)
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
    # -trimpath strips the source paths the self-locating JS contract tests
    # need, so it is rejected rather than letting those contracts miss.
    for argument in [*arguments[offset:], *shlex.split(env.get("GOFLAGS", ""))]:
        require(argument.split("=", 1)[0].lstrip("-") not in ("overlay", "C", "modfile", "trimpath"),
                "overlay, working directory and module file are owned by the test runner; "
                "trimpath is rejected because source contracts cannot self-locate their source")
    env["GOWORK"] = "off"
    root = Path(root).resolve(strict=True)
    with overlay(root) as (path, replacements):
        flag = "-overlay=" + str(path)
        proven = env.get(PARITY_HANDSHAKE, "")
        reused = offset != 2 and proven == overlay_digest(replacements, env)

        def verify():
            result = go_command(["list", flag, "-mod=readonly", "-json", "./..."], root, env, capture=True)
            require(result.returncode == 0, "Go package discovery failed: " + result.stderr.strip())
            # A race-gated test is ignored by the default build. Prove it is a
            # deliberate selection by discovering it again with the race tag;
            # package_parity still rejects every other non-platform tag.
            race_document = None
            if _listed_tests(result.stdout, ("IgnoredGoFiles",)):
                raced = go_command(["list", flag, "-mod=readonly", "-tags=race", "-json", "./..."],
                                   root, env, capture=True)
                require(raced.returncode == 0,
                        "Go race package discovery failed: " + raced.stderr.strip())
                race_document = raced.stdout
            package_parity(replacements, result.stdout, race_document)

        # Tidy can introduce a new test-only dependency; validate its package
        # graph after a successful update, not before the dependency exists.
        if offset != 2 and not reused:
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
