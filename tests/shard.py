#!/usr/bin/env python3
"""Run the overlay-aware Go suite as parallel package/test shards.

The default gate used to run one race test binary per package. Most packages
are serial internally, so the slowest package set the floor. This runner keeps
the same overlay, race detector, build tags and test names, but gives each
disjoint test group its own process. Packages that own fixed ports or other
cross-process resources stay single-shard.

Coverage is explicit: every name returned by `go test -list` is assigned to
exactly one shard, and a shard that selects no tests is never launched.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import os
import re
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TEST_LIST = re.compile(r"^(?:Test|Fuzz|Example)\w*$")
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

    @property
    def weight(self) -> float:
        return sum(package_weights(self.package).get(name, 1.0) for name in self.names)

    @property
    def label(self) -> str:
        if self.shards == 1:
            return self.package
        return f"{self.package} [{self.shard + 1}/{self.shards}]"

    def command(self, binary: str) -> list[str]:
        command = [binary, "-test.count=1", "-test.benchtime=200ms"]
        if self.shards > 1:
            pattern = "^(?:" + "|".join(re.escape(name) for name in self.names) + ")$"
            command.extend(["-test.run", pattern])
        return command


def go_packages() -> list[str]:
    result = subprocess.run(
        ["go", "list", "./..."],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode:
        raise RuntimeError("go list failed: " + result.stderr.strip())
    return [line for line in result.stdout.splitlines() if line]


def package_dirs() -> dict[str, str]:
    result = subprocess.run(
        ["go", "list", "-f", "{{.ImportPath}}\t{{.Dir}}", "./..."],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode:
        raise RuntimeError("go list failed: " + result.stderr.strip())
    dirs = {}
    for line in result.stdout.splitlines():
        package, _, directory = line.partition("\t")
        if package and directory:
            dirs[package] = directory
    return dirs


def build_binaries(tasks: list[Task], directory: Path, workers: int) -> dict[str, str]:
    packages = sorted({task.package for task in tasks})
    binaries: dict[str, str] = {}

    def build(package: str) -> tuple[str, int, float, str, str]:
        target = directory / (package.replace("/", "_") + ".test")
        started = time.monotonic()
        result = subprocess.run(
            [sys.executable, "-B", "-m", "tests.go", "test", "-c", "-mod=readonly",
             "-race", "-o", str(target), package],
            cwd=ROOT,
            capture_output=True,
            text=True,
        )
        return package, result.returncode, time.monotonic() - started, result.stdout + result.stderr, str(target)

    started = time.monotonic()
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(build, package) for package in packages]
        for future in concurrent.futures.as_completed(futures):
            package, code, elapsed, output, target = future.result()
            if code:
                print(f"build FAIL {package} ({elapsed:.1f}s)\n{output}", file=sys.stderr)
                raise RuntimeError(f"test binary build failed for {package}")
            binaries[package] = target
            print(f"built {package} ({elapsed:.1f}s)")
    print(f"built {len(packages)} test binaries in {time.monotonic() - started:.1f}s")
    return binaries


def list_tests(package: str) -> list[str]:
    result = subprocess.run(
        [sys.executable, "-B", "-m", "tests.go", "test", "-list", "^(Test|Fuzz|Example)", package],
        cwd=ROOT,
        capture_output=True,
        text=True,
    )
    if result.returncode:
        raise RuntimeError(f"go test -list failed for {package}: {result.stderr.strip()}")
    return [line for line in result.stdout.splitlines() if TEST_LIST.match(line)]


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


def tasks_for_package(package: str, names: list[str]) -> list[Task]:
    if package in SERIAL_PACKAGES:
        shards = 1
    else:
        shards = SHARD_COUNTS.get(package, 1)
    if not names:
        return [Task(package, (), 0, 1)]
    shards = min(shards, len(names))
    buckets = shard_names(names, shards, package_weights(package))
    tasks = [
        Task(package, tuple(bucket), index, len(buckets))
        for index, bucket in enumerate(buckets)
        if bucket
    ]
    assigned = [name for task in tasks for name in task.names]
    if sorted(assigned) != sorted(names):
        raise RuntimeError(f"shard assignment lost tests for {package}")
    return tasks


def build_tasks(packages: list[str]) -> list[Task]:
    tasks: list[Task] = []
    for package in packages:
        names = list_tests(package)
        tasks.extend(tasks_for_package(package, names))
    return tasks


def selected(tasks: list[Task], index: int, total: int) -> list[Task]:
    if total <= 1:
        return tasks
    if not 0 <= index < total:
        raise ValueError("shard index must be within 0..total-1")
    return [task for position, task in enumerate(tasks) if position % total == index]


def run_one(task: Task, binary: str, directory: str) -> tuple[Task, int, float, str]:
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
    args = parser.parse_args(argv)
    try:
        index_text, total_text = args.shard.split("/", 1)
        index, total = int(index_text), int(total_text)
        packages = go_packages()
        directories = package_dirs()
        tasks = selected(build_tasks(packages), index, total)
        tasks.sort(key=lambda task: (-task.weight, task.label))
    except (OSError, RuntimeError, ValueError) as exc:
        print(f"shard: {exc}", file=sys.stderr)
        return 1
    if not tasks:
        print(f"shard: no tasks for shard {index}/{total}", file=sys.stderr)
        return 1
    workers = max(1, args.workers)
    failed: list[tuple[Task, str]] = []
    try:
        with tempfile.TemporaryDirectory(prefix="millivolt-test-bin-") as directory:
            binaries = build_binaries(tasks, Path(directory), workers)
            started = time.monotonic()
            with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
                futures = {
                    pool.submit(run_one, task, binaries[task.package], directories[task.package]): task
                    for task in tasks
                }
                for future in concurrent.futures.as_completed(futures):
                    task, code, elapsed, output = future.result()
                    if code:
                        failed.append((task, output))
                        print(f"FAIL {task.label} ({elapsed:.1f}s)", file=sys.stderr)
                    else:
                        print(f"ok   {task.label} ({elapsed:.1f}s)")
            elapsed = time.monotonic() - started
    except (OSError, RuntimeError) as exc:
        print(f"shard: {exc}", file=sys.stderr)
        return 1
    if failed:
        for task, output in failed:
            print(f"\n===== {task.label} =====\n{output}", file=sys.stderr)
        print(f"shard: {len(failed)} of {len(tasks)} tasks failed in {elapsed:.1f}s", file=sys.stderr)
        return 1
    print(f"shard: {len(tasks)} tasks passed in {elapsed:.1f}s with {workers} workers")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
