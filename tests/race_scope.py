"""The single owner of which Go tests run under the race detector.

Race coverage follows evidence of concurrency, not package boundaries. A
package keeps ``-race`` for the tests that exercise its goroutines, channels,
locks or atomic state; every other test in the same package runs plain in the
same gate. The split is default-deny: a test name that no selector below
matches runs in the plain pass, so a new test is never silently dropped and
never gains race coverage by accident.

Every name returned by ``go test -list`` is assigned to exactly one pass by
``tasks_for_package`` in ``tests/shard.py``, which fails the run when the union
of both passes differs from the listing. ``audit`` additionally proves that a
package whose production sources contain concurrency primitives declares race
coverage, and that its selectors match at least one listed test.

Patterns are matched with :func:`fnmatch.fnmatchcase` against the top-level
test name. A package's selectors describe behaviour areas (stream, hold,
retry, projection, ...), so a future test named for the same area inherits
race coverage without maintaining a second list.
"""

from __future__ import annotations

import fnmatch
import re
from pathlib import Path
from typing import Iterable, Mapping, Sequence

MODULE = "github.com/LLM-4-People/millivolt"

# The production constructs that prove a package owns concurrency primitives.
# The audit lexes non-test Go sources in the package directory, blanking
# comments and string/rune literal bodies first, so prose and data can never
# fake evidence. Any package with a hit must declare a race selector, unless
# AUDIT_EXEMPTIONS documents why the primitive cannot be exercised
# concurrently by a test.
PRIMITIVE_PATTERNS: tuple[tuple[str, re.Pattern[str]], ...] = (
    ("go statement", re.compile(r"\bgo\s+\S")),
    ("chan", re.compile(r"\bchan\b")),
    (".Go(func", re.compile(r"\.Go\(func")),
    ("errgroup.", re.compile(r"\berrgroup\.")),
    ("sync.Mutex", re.compile(r"\bsync\.Mutex\b")),
    ("sync.RWMutex", re.compile(r"\bsync\.RWMutex\b")),
    ("sync.WaitGroup", re.compile(r"\bsync\.WaitGroup\b")),
    ("sync.Once", re.compile(r"\bsync\.Once\b")),
    ("sync.Cond", re.compile(r"\bsync\.Cond\b")),
    ("sync.Map", re.compile(r"\bsync\.Map\b")),
    ("sync.Pool", re.compile(r"\bsync\.Pool\b")),
    ("atomic.", re.compile(r"\batomic\.")),
)

# Package import path -> test-name patterns that run under -race. Everything
# else in the package runs plain. Keep each entry tied to the production
# primitive or executor it covers, and keep the comment with the entry.
PACKAGE_SELECTORS: dict[str, tuple[str, ...]] = {
    # Child processes, a real listener and reload/restart/restore fences. Only
    # the lifecycle serialization surface races; argument, handler, admin and
    # doc tests are plain. All tasks in this package are serialized by the
    # fixed-port lock, so the race and plain passes never overlap.
    f"{MODULE}/cmd/proxy": (
        "*Handoff*",
        "*Drain*",
        "*Serializ*",
        "*Concurrent*",
        "*SettingsSave*",
        "*FailAndResume*",
        "*Restore*FailsClosed",
        "*RestartHandoff*",
        "*RestartChild*",
        "*RestartWatch*",
        "*HandleRestart*",
        "*TryReload*",
        "*ReloadPublishes*",
        "*ReloadStatus*",
    ),
    # The stress client owns worker goroutines and atomics; the whole package
    # is small and is serialized by the fixed-port lock.
    f"{MODULE}/cmd/stress": ("*",),
    # One mutex guards settings revision publication. These two tests start
    # goroutines that write and read it concurrently; YAML parsing is plain.
    f"{MODULE}/internal/config": (
        "TestSettingsConcurrentRevisionAndReadIsolation",
        "TestWriteYAMLStringIdentityAndPrivateMode",
    ),
    # Cursor pump, turn publisher and the streaming reader own the package's
    # only goroutines and mutexes. The translation tables are pure functions
    # and run plain.
    f"{MODULE}/internal/format": (
        "TestCursorRun*",
        "TestCursorTurnEndedField14*",
        "TestCursorUsageFromCheckpoint*",
        "TestTurnPublisher*",
        "TestTurnDrains*",
        "TestTurnSettling*",
        "TestTurnSettle*",
        "TestReaderExit*",
        "TestRunTurnEmitFailure*",
        "TestProtoFieldBudget*",
        "TestAnthropicErrorEventTerminates*",
        "TestAnthropicMessageStopTerminates*",
    ),
    # Encoding and decoding are pure I/O in production. The staging tests run
    # concurrent writers, and the capped/streamed readers exercise the shared
    # source guards; the archive codecs run plain.
    f"{MODULE}/internal/backup": (
        "*Concurrent*",
        "*Stream*",
        "TestZstdStream*",
        "TestReadCapped*",
    ),
    # The in-process and live transports dispatch tool calls through the MCP
    # server and its mutex. Schema and documentation tests are plain.
    f"{MODULE}/internal/mcp": (
        "TestInProcess*",
        "TestLive*",
        "TestRegisteredTool*",
        "TestMCPBoundary*",
        "TestConcurrentAudit*",
        "TestErrorPaths*",
        "TestToolErrors*",
        "TestTokenNeverAppears*",
        "TestListingTools*",
        "TestCannedResponses*",
        "TestReadToolsRequestShape",
    ),
    # Ring buffer, live views and the stream handlers are built on mutexes,
    # atomics and per-subscriber goroutines. Small enough to race completely.
    f"{MODULE}/internal/metrics": ("*",),
    # Per-request goroutines, stream pump, SSE fan-out, retry pacing, holds,
    # throttles and storm scheduling. The named areas cover the concurrent
    # executors; parsing and pure translation stay plain where they do not
    # enter those paths.
    f"{MODULE}/internal/proxy": (
        "*Stream*",
        "*Hook*",
        "*Hold*",
        "*Throttle*",
        "*Scheduler*",
        "*Concurrency*",
        "*Concurrent*",
        "*Pause*",
        "*Budget*",
        "*Retry*",
        "*Storm*",
        "*Pacer*",
        "*Quota*",
        "*Keepalive*",
        "*Park*",
        "*Tail*",
        "*Bridge*",
        "*DebugCapture*",
        "*Cancel*",
        "*Disconnect*",
        "*Continuation*",
        "*Live*",
        "*DuplicateClientRequestIDs*",
        "*RequestID*",
    ),
    # Cond-var wait/send, storm backoff, holds and leases. Cheap enough to run
    # the complete package under race.
    f"{MODULE}/internal/scheduler": ("*",),
    # Store.Record/PublishLive/Clear fences, the writer goroutine, and
    # concurrent purge, restore and snapshot work. Schema, query and
    # migration-only tests are plain.
    f"{MODULE}/internal/storage": (
        "*Concurrent*",
        "*Fence*",
        "*Clear*",
        "*Purge*",
        "*BusyTimeout*",
        "*Snapshot*",
        "*Serializ*",
        "*Writer*",
        "*Flush*",
        "*DataVersion*",
    ),
    # Projection publisher, chart aggregation, pooled buffers, stream handlers,
    # single-flight folds, and the lineage payload read lease. Allocation-only
    # lineage measurements stay plain: they pin sync.Pool byte reuse under
    # GC-off sampling and are perturbed by concurrent load, not by a race.
    f"{MODULE}/internal/web": (
        "*Stream*",
        "*Projection*",
        "*Chart*",
        "*Pool*",
        "*Publish*",
        "*Cancel*",
        "*Concurrent*",
        "*Flight*",
        "*Memo*",
        "*Lease*",
        "TestExplorerLineagePayload*",
    ),
}

# Documented exemptions from the primitive audit. A package only belongs here
# when its primitive cannot be reached by a concurrent test, and the reason
# must name the evidence. The gate rejects an empty reason.
AUDIT_EXEMPTIONS: dict[str, str] = {}


def selectors_for(package: str) -> tuple[str, ...]:
    """Return the declared race selectors for a package, empty when unlisted."""
    return PACKAGE_SELECTORS.get(package, ())


def matches(name: str, selectors: Iterable[str]) -> bool:
    return any(fnmatch.fnmatchcase(name, selector) for selector in selectors)


def select(package: str, names: Sequence[str], *, all_race: bool = False
           ) -> tuple[list[str], list[str]]:
    """Partition a package's listed tests into the race and plain passes.

    ``plain`` is the exact complement of ``race`` over ``names``, so every
    name lands in one pass even when the package has no selectors. Callers
    verify the partition against the listing again before running.
    """
    if all_race:
        return list(names), []
    selectors = selectors_for(package)
    race = [name for name in names if matches(name, selectors)]
    raced = set(race)
    plain = [name for name in names if name not in raced]
    return race, plain


def _strip_comments_and_literals(text: str) -> str:
    """Blank comment and string/rune literal bodies, preserving newlines.

    Lexing in one pass means a ``//`` inside a string is data, not a comment,
    and a quote inside a comment cannot start a literal. Newlines survive so
    multi-line comments and raw strings keep their line shape.
    """
    blanked: list[str] = []
    index = 0
    length = len(text)

    def blank(start: int, end: int) -> str:
        return "".join("\n" if char == "\n" else " " for char in text[start:end])

    while index < length:
        char = text[index]
        if text.startswith("//", index):
            end = text.find("\n", index)
            end = length if end < 0 else end
            blanked.append(blank(index, end))
            index = end
        elif text.startswith("/*", index):
            end = text.find("*/", index + 2)
            end = length if end < 0 else end + 2
            blanked.append(blank(index, end))
            index = end
        elif char in {'"', "'"}:
            end = index + 1
            while end < length:
                if text[end] == "\\":
                    end += 2
                    continue
                if text[end] == char:
                    end += 1
                    break
                end += 1
            end = min(end, length)
            blanked.append(blank(index, end))
            index = end
        elif char == "`":
            end = text.find("`", index + 1)
            end = length if end < 0 else end + 1
            blanked.append(blank(index, end))
            index = end
        else:
            blanked.append(char)
            index += 1
    return "".join(blanked)


def primitive_evidence(directory: Path) -> dict[str, int]:
    """Count production concurrency constructs in a package directory."""
    evidence: dict[str, int] = {}
    for source in sorted(directory.glob("*.go")):
        if source.name.endswith("_test.go"):
            continue
        try:
            text = source.read_text(encoding="utf-8")
        except OSError as error:
            raise RuntimeError(f"cannot scan production source {source}") from error
        code = _strip_comments_and_literals(text)
        for label, pattern in PRIMITIVE_PATTERNS:
            count = len(pattern.findall(code))
            if count:
                evidence[label] = evidence.get(label, 0) + count
    return evidence


def audit(packages: Mapping[str, Path | str],
          tests: Mapping[str, Sequence[str]] | None = None,
          *,
          selectors: Mapping[str, Sequence[str]] = PACKAGE_SELECTORS,
          exemptions: Mapping[str, str] = AUDIT_EXEMPTIONS) -> list[str]:
    """Return failures for primitive packages without usable race coverage.

    With ``tests`` omitted the audit checks declarations only, which is the
    cheap guard for the real tree. When a listing is supplied it also proves
    the declared selectors match at least one test that will actually run.
    """
    failures: list[str] = []
    for package, reason in sorted(exemptions.items()):
        if not reason.strip():
            failures.append(f"{package}: race audit exemption has no evidence")
    for package, directory in sorted(packages.items()):
        if package in exemptions:
            continue
        evidence = primitive_evidence(Path(directory))
        if not evidence:
            continue
        declared = selectors.get(package, ())
        summary = ", ".join(f"{token}={count}" for token, count in sorted(evidence.items()))
        if not declared:
            failures.append(f"{package}: ships concurrency primitives ({summary}) "
                            "but declares no race selector")
            continue
        if tests is None:
            continue
        race, _ = select(package, list(tests.get(package, ())))
        if not race:
            failures.append(f"{package}: race selectors match no listed test "
                            f"({summary}); update the selector or document an exemption")
    return failures
