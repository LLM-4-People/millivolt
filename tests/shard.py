#!/usr/bin/env python3
"""Run the overlay-aware Go suite as parallel package/test shards.

Each package is split into two passes. Tests declared by `tests/race_scope.py`
run under the race detector; the rest run plain. Both passes use the same
overlay, build tags and test names, and each disjoint group gets its own test
binary and process. Packages that own fixed ports or other cross-process
resources stay single-shard and are additionally locked so their race and
plain passes never overlap.

Coverage is explicit and default-deny: every name returned by `go test -list`
is assigned to exactly one pass (race or plain), the union of both passes must
equal the listing, and a task that selects no tests is never launched.
`--all-race` escalates every test to the race pass for the scheduled job.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import contextlib
import os
import re
import subprocess
import sys
import tempfile
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Iterable, Mapping, Sequence

from . import race_scope
from .go import PARITY_HANDSHAKE, parity_digest

ROOT = Path(__file__).resolve().parent.parent
TEST_LIST = re.compile(r"^(?:Test|Fuzz|Example)\w*$")
TEST_PATTERN = "^(Test|Fuzz|Example)"
# One `go test -list ./...` block ends with a per-package result line; these
# are the only statuses it can report for a package that built.
TEST_RESULT = re.compile(r"^(ok|FAIL|\?)\s+(\S+)")
# Packages with expensive, temp-isolated tests get several shards. Fixed-port
# and child-process packages remain one process; sharding them would collide.
SHARD_COUNTS = {
    "github.com/LLM-4-People/millivolt/internal/backup": 3,
    "github.com/LLM-4-People/millivolt/internal/config": 2,
    "github.com/LLM-4-People/millivolt/internal/format": 2,
    "github.com/LLM-4-People/millivolt/internal/mcp": 3,
    "github.com/LLM-4-People/millivolt/internal/proxy": 6,
    "github.com/LLM-4-People/millivolt/internal/storage": 8,
    "github.com/LLM-4-People/millivolt/internal/web": 6,
}
SERIAL_PACKAGES = {
    "github.com/LLM-4-People/millivolt/cmd/proxy",
    "github.com/LLM-4-People/millivolt/cmd/stress",
}
# Relative no-race durations from the default-gate profile. They only break
# ties between shards; stale weights can unbalance a run but never skip a test.
WEIGHTS = {
    "internal/storage": {
        "TestRestoreRejectsUnrepresentableTimestamps": 5.5,
        "TestPreflightRejectsNullInEveryRecordScanColumn": 5.2,
        "TestRestoreTimestampBoundariesAreUTC": 3.1,
        "TestRestoreRejectsAggregateUnsafeValues": 3.1,
        "TestRestoreRejectsNonTextExactKeys": 2.3,
        "TestMigrateReclaimsFreedIndexPagesOnce": 1000,
        "TestOpenQuarantinesUnscannableScanColumnSnapshot": 2.2,
        "TestRestoreRejectsUnsafeDurations": 2.1,
        "TestMergeRejectsUnscannableScanColumnValues": 1.9,
        "TestRestoreAttemptsJSONInvariant": 1.6,
        "TestMigrateRepairProbeIsOneShot": 1.6,
        "TestOpenQuarantinesIncompatiblePendingSnapshot": 1.5,
        "TestRestoreRejectsIntegerTotalOverflowAndNegative": 1.4,
    },
    "internal/web": {
        "TestLogPageDecodeBudget": 6.0,
        "TestLogPageNarrowScanMatchesFullDecode": 10,
        "TestLineageDirtyExplorerAllocationIsPooled": 1000,
        "TestProjectionRecentOrderMatchesFullRebuild": 1.5,
        "TestChartUnchangedViewAllocationBounded": 1.3,
        "TestProjectionRecentAppendAllocationBounded": 1.3,
        "TestExplorerDistinctGroupsStayBounded": 1.0,
        "TestProjectionMetricOrderMatchesFullRebuild": 0.9,
    },
    "internal/proxy": {
        "TestExhaustedRetriesPaceNextFirstSend": 5.9,
        "TestTarpitErrorBodyReleasesChunkedInflightBudget": 5.1,
        "TestRequestBodyAllocationBounds": 4.5,
        "TestRequestToolChoiceDecodeAllocationBounded": 3.7,
        "TestTarpitBodylessErrorReleasesRequestSlot": 2.6,
        "TestCursorParkedRunOutlivesGenericIdleGuard": 2.3,
        "TestRecoveredRequestResetsBackoff": 2.0,
        "TestConsecutiveExhausted429sDoubleBackoff": 2.0,
        "TestConsecutiveFailuresDoubleBackoff": 1.6,
        "TestHookPublishSerializesSeededHoldAndThrottleDispatch": 1.5,
        "TestRequestStopArrayAllocationBound": 1.4,
        "TestRetryTTFTIsFinalAttempt": 1.4,
        "TestRequestTopLevelMemberAllocationBound": 1.3,
        "TestSubConversationsStripAcrossRetriesAndResends": 1.3,
        "TestRequestAggregateContentPartsAllocationBound": 1.3,
    },
    "internal/backup": {
        "TestDatabaseMemberAboveConfigCapDecodes": 1000,
        "TestEncodeFileEnforcesCapBeforeArtifact": 0.5,
        "TestEncodeConfigMemberCap": 0.5,
    },
    "internal/mcp": {
        "TestTokenInsideTheScanCapIsCaughtAtTheBoundaryOffsets": 1000,
    },
}


@dataclass(frozen=True)
class Task:
    package: str
    names: tuple[str, ...]
    shard: int
    shards: int
    race: bool = True

    @property
    def mode(self) -> str:
        return "race" if self.race else "plain"

    @property
    def weight(self) -> float:
        return sum(package_weights(self.package).get(name, 1.0) for name in self.names)

    @property
    def label(self) -> str:
        if self.shards == 1:
            return f"{self.package} ({self.mode})"
        return f"{self.package} [{self.shard + 1}/{self.shards}] ({self.mode})"

    def command(self, binary: str) -> list[str]:
        command = [binary, "-test.count=1", "-test.benchtime=200ms"]
        if self.shards > 1:
            pattern = "^(?:" + "|".join(re.escape(name) for name in self.names) + ")$"
            command.extend(["-test.run", pattern])
        return command


def packages() -> dict[str, str]:
    """Return Go's package order and directories from one discovery call."""
    result = subprocess.run(
        ["go", "list", "-f", "{{.ImportPath}}\t{{.Dir}}", "./..."],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode:
        raise RuntimeError("go list failed: " + result.stderr.strip())
    found = {}
    for line in result.stdout.splitlines():
        package, _, directory = line.partition("\t")
        if package and directory:
            found[package] = directory
    return found


def build_binaries(tasks: list[Task], directory: Path, workers: int,
                   env: dict[str, str] | None = None) -> dict[tuple[str, bool], str]:
    targets = sorted({(task.package, task.race) for task in tasks})
    binaries: dict[tuple[str, bool], str] = {}

    def build(package: str, race: bool) -> tuple[tuple[str, bool], int, float, str, str]:
        suffix = ".race.test" if race else ".plain.test"
        target = directory / (package.replace("/", "_") + suffix)
        command = [sys.executable, "-B", "-m", "tests.go", "test", "-c", "-mod=readonly"]
        if race:
            command.append("-race")
        command.extend(["-o", str(target), package])
        started = time.monotonic()
        result = subprocess.run(
            command,
            cwd=ROOT,
            capture_output=True,
            text=True,
            env=env,
        )
        output = result.stdout + result.stderr
        return (package, race), result.returncode, time.monotonic() - started, output, str(target)

    started = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(build, package, race) for package, race in targets]
        for future in concurrent.futures.as_completed(futures):
            key, code, elapsed, output, target = future.result()
            package, race = key
            if code:
                print(f"build FAIL {package} ({'race' if race else 'plain'}, {elapsed:.1f}s)\n{output}",
                      file=sys.stderr)
                raise RuntimeError(f"test binary build failed for {package}")
            binaries[key] = target
            print(f"built {package} ({'race' if race else 'plain'}, {elapsed:.1f}s)")
    print(f"built {len(targets)} test binaries in {time.monotonic() - started:.1f}s")
    return binaries


def parse_test_listing(output: str) -> dict[str, list[str]]:
    """Map each package's `go test -list ./...` name block to that package.

    The go command emits one contiguous block per package: the matching test
    names, then a single result line naming the package. Names are never
    attributed across a boundary, so output order cannot mix two packages.
    """
    listing: dict[str, list[str]] = {}
    pending: list[str] = []
    for line in output.splitlines():
        result = TEST_RESULT.match(line)
        if result:
            status, package = result.groups()
            if status == "FAIL":
                raise RuntimeError(f"go test -list failed for {package}")
            if package in listing:
                raise RuntimeError(f"go test -list repeated package {package}")
            listing[package] = pending
            pending = []
        elif TEST_LIST.match(line):
            pending.append(line)
        elif line.strip():
            raise RuntimeError("unexpected go test -list output: " + line)
    if pending:
        raise RuntimeError("go test -list output ended without a package result")
    return listing


def discover_tests(packages: list[str], *, all_race: bool = False
                   ) -> tuple[list[Task], dict[str, list[str]], dict[str, str]]:
    """List every test once, then return the proven build environment.

    The listing call performs the run's only whole-module package-parity
    verification. An inherited handshake is dropped here so this run always
    proves parity itself; builds then receive the fingerprint of that verified
    overlay instead of re-listing the module graph.
    """
    environment = {name: value for name, value in os.environ.items()
                   if name != PARITY_HANDSHAKE}
    result = subprocess.run(
        [sys.executable, "-B", "-m", "tests.go", "test", "-list", TEST_PATTERN, "./..."],
        cwd=ROOT,
        capture_output=True,
        text=True,
        env=environment,
    )
    if result.returncode:
        raise RuntimeError("go test -list failed: "
                           + (result.stderr.strip() or result.stdout.strip()))
    listing = parse_test_listing(result.stdout)
    if set(listing) != set(packages):
        raise RuntimeError("go test -list package mismatch: "
                           + repr(sorted(set(listing) ^ set(packages))))
    tasks: list[Task] = []
    for package in packages:
        tasks.extend(tasks_for_package(package, listing[package], all_race=all_race))
    return tasks, listing, {**environment, PARITY_HANDSHAKE: parity_digest(environment=environment)}


def package_weights(package: str) -> dict[str, float]:
    for suffix, weights in WEIGHTS.items():
        if package.endswith("/" + suffix):
            return weights
    return {}


def shard_names(names: list[str], shards: int, weights: dict[str, float] | None = None) -> list[list[str]]:
    """Greedy longest-processing-time distribution, stable for equal weights."""
    weights = weights or {}
    buckets: list[list[str]] = [[] for _ in range(max(1, shards))]
    loads = [0.0] * len(buckets)
    for name in sorted(names, key=lambda item: (-weights.get(item, 1.0), item)):
        index = min(range(len(buckets)), key=lambda item: (loads[item], item))
        buckets[index].append(name)
        loads[index] += weights.get(name, 1.0)
    return buckets


def tasks_for_package(package: str, names: list[str], *, all_race: bool = False) -> list[Task]:
    if not names:
        # go test -c emits no binary for a package with no test files. Keep
        # the package out of the build entirely: launching a task with an
        # absent binary is not a test result.
        return []
    race_names, plain_names = race_scope.select(package, names, all_race=all_race)
    limit = 1 if package in SERIAL_PACKAGES else SHARD_COUNTS.get(package, 1)
    tasks: list[Task] = []
    for race, group in ((True, race_names), (False, plain_names)):
        if not group:
            continue
        shards = min(limit, len(group))
        for index, bucket in enumerate(shard_names(group, shards, package_weights(package))):
            tasks.append(Task(package, tuple(bucket), index, shards, race=race))
    verify_assignments({package: names}, tasks)
    return tasks


def verify_assignments(listing: Mapping[str, Sequence[str]], tasks: Iterable[Task]) -> None:
    """Fail unless both passes cover the listing exactly once.

    This is the mechanical default-deny guard: a name assigned to neither pass,
    to both passes, or to two tasks fails the run before any test starts.
    """
    assigned: dict[str, list[str]] = {}
    for task in tasks:
        assigned.setdefault(task.package, []).extend(task.names)
    for package in sorted(set(listing) | set(assigned)):
        expected = list(listing.get(package, ()))
        got = assigned.get(package, [])
        if len(got) != len(set(got)):
            duplicates = sorted(name for name in set(got) if got.count(name) > 1)
            raise RuntimeError(f"tests assigned to more than one pass for {package}: {duplicates}")
        if set(got) != set(expected) or len(got) != len(expected):
            missing = sorted(set(expected) - set(got))
            extra = sorted(set(got) - set(expected))
            raise RuntimeError(f"test assignment differs from go test -list for {package}: "
                               f"missing {missing}, unexpected {extra}")


def selected(tasks: list[Task], index: int, total: int) -> list[Task]:
    if total <= 1:
        return tasks
    if not 0 <= index < total:
        raise ValueError("shard index must be within 0..total-1")
    return [task for position, task in enumerate(tasks) if position % total == index]


class PackageLocks:
    """Per-package exclusive locks for packages that own fixed ports.

    The race and plain passes of a serial package are separate processes that
    bind the same port and start the same child instance, so they must never
    overlap. Other packages stay fully parallel.
    """

    def __init__(self, exclusive: Iterable[str]):
        self._exclusive = frozenset(exclusive)
        self._guard = threading.Lock()
        self._locks: dict[str, threading.Lock] = {}

    def hold(self, package: str):
        if package not in self._exclusive:
            return contextlib.nullcontext()
        with self._guard:
            lock = self._locks.setdefault(package, threading.Lock())
        return lock


def run_one(task: Task, binary: str, directory: str,
            locks: PackageLocks) -> tuple[Task, int, float, str]:
    # The lock wraps the whole process: fixed-port packages bind their port for
    # the lifetime of the test binary, not only while a test function runs.
    with locks.hold(task.package):
        started = time.monotonic()
        result = subprocess.run(
            task.command(binary),
            cwd=directory,
            capture_output=True,
            text=True,
        )
        return task, result.returncode, time.monotonic() - started, result.stdout + result.stderr


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    default_workers = os.environ.get("MILLIVOLT_TEST_WORKERS")
    try:
        workers = int(default_workers) if default_workers is not None else min(12, os.cpu_count() or 1)
    except ValueError:
        workers = 0
    parser.add_argument("--workers", type=int, default=workers)
    parser.add_argument("--shard", default="0/1", help="select one CI shard as INDEX/TOTAL")
    parser.add_argument("--all-race", action="store_true",
                        default=os.environ.get("MILLIVOLT_TEST_ALL_RACE", "") in {"1", "true"},
                        help="escalate every test to the race pass (scheduled full-race job)")
    args = parser.parse_args(argv)
    try:
        index_text, total_text = args.shard.split("/", 1)
        index, total = int(index_text), int(total_text)
        discovered = packages()
        tasks, listing, go_env = discover_tests(list(discovered), all_race=args.all_race)
        race_scope_failures = race_scope.audit(discovered, listing)
        if race_scope_failures:
            raise RuntimeError("race scope audit failed: " + "; ".join(race_scope_failures))
        verify_assignments(listing, tasks)
        tasks = selected(tasks, index, total)
        tasks.sort(key=lambda task: (-task.weight, task.label))
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"shard: {exc}", file=sys.stderr)
        return 1
    if not tasks:
        print(f"shard: no tasks for shard {index}/{total}", file=sys.stderr)
        return 1
    race_tasks = sum(1 for task in tasks if task.race)
    plain_tasks = len(tasks) - race_tasks
    workers = max(1, args.workers)
    failed: list[tuple[Task, str]] = []
    timings: list[tuple[Task, float]] = []
    try:
        with tempfile.TemporaryDirectory(prefix="millivolt-test-bin-") as directory:
            binaries = build_binaries(tasks, Path(directory), workers, go_env)
            locks = PackageLocks(SERIAL_PACKAGES)
            started = time.monotonic()
            with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
                futures = {
                    pool.submit(run_one, task, binaries[(task.package, task.race)],
                                discovered[task.package], locks): task
                    for task in tasks
                }
                for future in concurrent.futures.as_completed(futures):
                    task, code, elapsed, output = future.result()
                    timings.append((task, elapsed))
                    if code:
                        failed.append((task, output))
                        print(f"FAIL {task.label} ({elapsed:.1f}s)", file=sys.stderr)
                    else:
                        print(f"ok   {task.label} ({elapsed:.1f}s)")
            elapsed = time.monotonic() - started
    except (OSError, RuntimeError) as exc:
        print(f"shard: {exc}", file=sys.stderr)
        return 1
    task_seconds = sum(seconds for _, seconds in timings)
    longest_task, longest_seconds = max(timings, key=lambda item: item[1])
    summary = (f"{len(tasks)} tasks ({race_tasks} race, {plain_tasks} plain), "
               f"task sum {task_seconds:.1f}s, longest {longest_task.label} {longest_seconds:.1f}s")
    mode = "full-race" if args.all_race else "race/plain"
    if failed:
        for task, output in failed:
            print(f"\n===== {task.label} =====\n{output}", file=sys.stderr)
        print(f"shard: {len(failed)} of {len(tasks)} tasks failed in {elapsed:.1f}s "
              f"({mode}; {summary})", file=sys.stderr)
        return 1
    print(f"shard: {summary} passed in {elapsed:.1f}s with {workers} workers ({mode})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
