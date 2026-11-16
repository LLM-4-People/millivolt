#!/usr/bin/env python3
"""Collect one perf.sh docs run into perf/results.json.

The harness writes raw output (stress JSONL, benchmark logs, image and idle
samples). This module is the only reader that turns them into the bundle
scripts/perf_docs.py renders. It fails on a missing stage, a failed response, a
missing benchmark or a broken accounting result: a partial run never becomes a
published page.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import platform
import re
import subprocess
import sys
from pathlib import Path

GENERATED_BY = "scripts/perf.sh docs"
BENCH_RE = re.compile(r"^Benchmark(\S*?)(?:-\d+)?\s+(\d+)\s+([0-9.]+) ns/op(.*)$")

ARCHITECTURE = (
    "BenchmarkSkipJSONString",
    "BenchmarkRequestMetadataShapes",
    "BenchmarkAnalyzerFeed",
    "BenchmarkAnalyzerToolNameDedupe",
    "BenchmarkHandleLogPage",
    "BenchmarkAggregateUnchanged",
    "BenchmarkKPI",
    "BenchmarkAggregateFold",
    "BenchmarkProjectionMetricAppend",
    "BenchmarkObserveModelNameEvent",
    "BenchmarkObserveModelNameEventWired",
    "BenchmarkObserveModelNames4096",
)


class CollectError(Exception):
    """The raw run cannot become a complete bundle."""


def read_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise CollectError(f"missing raw input: {path}") from exc
    except ValueError as exc:
        raise CollectError(f"{path} is not valid JSON: {exc}") from exc


def command(*args: str) -> str:
    try:
        return subprocess.run(args, check=True, capture_output=True, text=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError) as exc:
        raise CollectError(f"command failed: {' '.join(args)}: {exc}") from exc


def stress_result(path: Path) -> tuple[dict, dict]:
    """Read one stage's workload line and its single result line."""
    workload = None
    result = None
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            entry = json.loads(line)
        except ValueError as exc:
            raise CollectError(f"{path}: malformed JSON line: {exc}") from exc
        if entry.get("kind") == "workload":
            workload = entry
        elif "concurrency" in entry:
            if result is not None:
                raise CollectError(f"{path}: more than one result line")
            result = entry
    if workload is None or result is None:
        raise CollectError(f"{path}: missing workload or result line")
    return workload, result


def number(entry: dict, key: str, what: str) -> float:
    value = entry.get(key)
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise CollectError(f"{what}: {key} is not a number: {value!r}")
    return float(value)


def fixture_shape(workload: dict, path: Path) -> dict:
    """The fixture a stage actually drove, recorded by cmd/stress itself.

    The page describes the response shape beside every table. Reading it from
    the workload line keeps that description tied to the run rather than to
    defaults in this module.
    """
    hold = workload.get("stream_duration")
    if not isinstance(hold, str) or not re.fullmatch(r"[0-9]+s", hold):
        raise CollectError(f"{path}: workload stream_duration is not whole seconds")
    chunks = workload.get("chunks")
    if isinstance(chunks, bool) or not isinstance(chunks, int) or chunks < 1:
        raise CollectError(f"{path}: workload chunks is not a positive integer")
    inputs = workload.get("fixture_input_tokens")
    if isinstance(inputs, bool) or not isinstance(inputs, int) or inputs < 1:
        raise CollectError(f"{path}: workload fixture_input_tokens is not a positive integer")
    cost = number(workload, "fixture_cost", str(path))
    if cost <= 0:
        raise CollectError(f"{path}: workload fixture_cost is not positive")
    return {
        "chunks": chunks,
        "hold_s": int(hold[:-1]),
        "input_tokens": inputs,
        "cost_usd": cost,
    }


def normalize_stage(path: Path) -> tuple[dict, dict]:
    workload, result = stress_result(path)
    if number(result, "Failures", str(path)) != 0:
        raise CollectError(f"{path}: {result.get('Failures')} failed responses")
    if result.get("AccountingOK") is not True:
        raise CollectError(f"{path}: durable accounting did not balance")
    stage = {
        "workers": int(number(result, "concurrency", str(path))),
        "requests": int(number(result, "Requests", str(path))),
        "successes": int(number(result, "Successes", str(path))),
        "failures": int(number(result, "Failures", str(path))),
        "seconds": number(result, "Seconds", str(path)),
        "rps": number(result, "RequestsPerSecond", str(path)),
        "p50_ms": number(result, "P50Ms", str(path)),
        "p95_ms": number(result, "P95Ms", str(path)),
        "p99_ms": number(result, "P99Ms", str(path)),
        "first_byte_p95_ms": number(result, "FirstByteP95Ms", str(path)),
        "cpu_percent": number(result, "CPUPercent", str(path)),
        "peak_rss_mib": number(result, "PeakRSSMiB", str(path)),
        "generator_cpu_percent": number(result, "GeneratorCPUPercent", str(path)),
        "generator_peak_rss_mib": number(result, "GeneratorPeakRSSMiB", str(path)),
        "upstream_peak": int(number(result, "upstream_peak", str(path))),
        "sse_per_s": number(result, "sse_events_per_second", str(path)),
        "tokens_per_s": number(result, "output_tokens_per_second", str(path)),
        "stream_kib_per_s": number(result, "stream_bytes_per_second", str(path)) / 1024,
        "durable": int(number(result, "DurableRequests", str(path))),
        "drops": int(number(result, "storage_dropped", str(path))) if "storage_dropped" in result else 0,
        "accounting_ok": True,
    }
    stage.update(fixture_shape(workload, path))
    if stage["successes"] != stage["requests"]:
        raise CollectError(
            f"{path}: {stage['successes']} successful responses for {stage['requests']} requests")
    if stage["drops"]:
        kept = stage["durable"]
        if kept + stage["drops"] != stage["successes"]:
            raise CollectError(
                f"{path}: durable {kept} + drops {stage['drops']} != successes {stage['successes']}")
    elif stage["durable"] != stage["successes"]:
        raise CollectError(
            f"{path}: durable {stage['durable']} != successes {stage['successes']} without drops")
    return workload, stage


def benchmark_suite(name: str) -> str:
    base = name.split("/", 1)[0]
    if base == "BenchmarkStorageWriter":
        return "writer"
    if base == "BenchmarkProxyDurableContention":
        return "overload"
    if base == "BenchmarkStoreDiskBytes":
        return "store_disk"
    if base == "BenchmarkProjectionFootprint":
        return "projection"
    if base == "BenchmarkProjectionProcessRSS":
        return "process_rss"
    if base == "BenchmarkQueryBeforeEquality":
        return "query"
    if base == "BenchmarkRequestMetadata":
        return "metadata"
    if base in ARCHITECTURE:
        return "architecture"
    return "other"


def parse_benchmarks(paths: list[Path]) -> list[dict]:
    entries = []
    seen = set()
    for path in paths:
        for line in path.read_text(encoding="utf-8").splitlines():
            match = BENCH_RE.match(line.strip())
            if not match:
                continue
            name, _iterations, ns, rest = match.groups()
            full = "Benchmark" + name
            if full in seen:
                raise CollectError(f"benchmark {full!r} appears more than once")
            seen.add(full)
            metrics = {}
            fields = rest.split()
            if len(fields) % 2:
                raise CollectError(f"{full}: unpaired benchmark metric fields: {rest!r}")
            for i in range(0, len(fields), 2):
                unit = fields[i + 1]
                try:
                    value = float(fields[i])
                except ValueError as exc:
                    raise CollectError(f"{full}: {unit} value {fields[i]!r} is not a number") from exc
                if unit != "ns/op":
                    metrics[unit] = value
            entries.append({
                "name": full,
                "suite": benchmark_suite(full),
                "ns_per_op": float(ns),
                "metrics": metrics,
            })
    if not entries:
        raise CollectError("no benchmark results were parsed")
    return entries


def method_section(manifest: dict, workload: dict, load: dict) -> dict:
    revision = command("git", "rev-parse", "HEAD")
    dirty = bool(command("git", "status", "--porcelain"))
    branch = command("git", "rev-parse", "--abbrev-ref", "HEAD")
    go = command("go", "version").split()[2]
    kern = command("uname", "-sr")
    cpu = ""
    for line in command("lscpu").splitlines():
        if line.startswith("Model name:"):
            cpu = line.split(":", 1)[1].strip()
            break
    ram_gib = os.sysconf("SC_PAGE_SIZE") * os.sysconf("SC_PHYS_PAGES") // (1024 ** 3)
    machine = {"x86_64": "amd64", "aarch64": "arm64"}.get(platform.machine(), platform.machine())
    platform_name = f"{platform.system()}/{machine}"
    if "microsoft" in kern.lower():
        platform_name += " under WSL2"
    settings = workload.get("performance_settings") or {}
    now = dt.datetime.now(dt.timezone.utc)
    for key in ("debug_captures", "body_previews", "live_observer", "upstream"):
        if key not in manifest:
            raise CollectError(f"manifest has no {key} declaration")
    for key in ("debug_captures", "body_previews", "live_observer"):
        if not isinstance(manifest[key], bool):
            raise CollectError(f"manifest {key} is not a boolean")
    if not isinstance(manifest["upstream"], str) or not manifest["upstream"]:
        raise CollectError("manifest upstream is not a description")
    return {
        "revision": revision,
        "dirty": dirty,
        "branch": branch,
        "date_utc": now.strftime("%Y-%m-%d"),
        "generated_at": now.isoformat(),
        "go": go,
        "platform": platform_name,
        "kernel": kern,
        "cpu": cpu,
        "cpus": os.cpu_count(),
        "ram_gib": ram_gib,
        "rss_sampling_ms": manifest["rss_sampling_ms"],
        "config": "Public example with private listen and database paths",
        "debug_captures": manifest["debug_captures"],
        "body_previews": manifest["body_previews"],
        "live_observer": manifest["live_observer"],
        "upstream": manifest["upstream"],
        "max_conns_per_host": settings.get("max_conns_per_host"),
        "fast_duration_s": manifest["fast_duration_s"],
        "fast_long_duration_s": manifest["fast_long_duration_s"],
        "sustained_duration_s": manifest["sustained_duration_s"],
        "streaming_duration_s": manifest["streaming_duration_s"],
        "streaming_hold_s": manifest["streaming_hold_s"],
        "streaming_chunks": manifest["streaming_chunks"],
        "load_1m_start": load["start"],
        "load_1m_end": load["end"],
    }


def collect(raw: Path, out: Path) -> dict:
    manifest = read_json(raw / "manifest.json")
    if not isinstance(manifest, dict):
        raise CollectError("manifest is not an object")
    for key in ("artifacts", "idle", "load", "rss_sampling_ms", "fast_duration_s",
                "fast_long_duration_s", "sustained_duration_s", "streaming_duration_s",
                "streaming_hold_s", "streaming_chunks", "fast", "sustained",
                "streaming_concurrency", "streaming_throughput", "benchmarks"):
        if key not in manifest:
            raise CollectError(f"manifest has no {key}")
    artifacts = read_json(raw / manifest["artifacts"])
    idle = read_json(raw / manifest["idle"])
    load = read_json(raw / manifest["load"])
    for key in ("binary_bytes", "image_bytes", "archive_bytes"):
        if not isinstance(artifacts.get(key), int) or artifacts[key] <= 0:
            raise CollectError(f"artifact sample has no positive {key}")
    for key in ("idle_rss_kib", "idle_seconds"):
        if not isinstance(idle.get(key), (int, float)) or idle[key] <= 0:
            raise CollectError(f"idle sample has no positive {key}")
    # A quiet process can consume zero scheduler ticks in a short sampling
    # window; that is a real result, not a missing sample.
    if not isinstance(idle.get("idle_cpu_ticks"), (int, float)) or idle["idle_cpu_ticks"] < 0:
        raise CollectError("idle sample has no non-negative idle_cpu_ticks")
    stages = {}
    workload = None
    for key in ("fast", "sustained", "streaming_concurrency", "streaming_throughput"):
        stages[key] = []
        for rel in manifest[key]:
            stage_workload, stage = normalize_stage(raw / rel)
            if workload is None:
                workload = stage_workload
            elif stage_workload.get("performance_settings") != workload.get("performance_settings"):
                raise CollectError(f"{rel}: performance settings differ from the first stage")
            stages[key].append(stage)
        stages[key].sort(key=lambda stage: (stage["workers"], stage["seconds"]))
    for stage in stages["fast"]:
        if stage["drops"]:
            raise CollectError(f"fast stage at {stage['workers']} workers dropped durable rows")
    benchmarks = parse_benchmarks([raw / rel for rel in manifest["benchmarks"]])
    missing = [name for name in ARCHITECTURE
               if not any(entry["name"].split("/", 1)[0] == name for entry in benchmarks)]
    if missing:
        raise CollectError("architecture benchmarks missing: " + ", ".join(missing))

    if workload is None:
        raise CollectError("no workload line was parsed")
    method = method_section(manifest, workload, load)
    if not method["max_conns_per_host"]:
        raise CollectError("workload line has no max_conns_per_host")
    bundle = {
        "schema": 1,
        "generated_by": GENERATED_BY,
        "generated_at": method["generated_at"],
        "method": method,
        "artifacts": {
            "binary_bytes": artifacts["binary_bytes"],
            "image_bytes": artifacts["image_bytes"],
            "archive_bytes": artifacts["archive_bytes"],
            "idle_rss_kib": idle["idle_rss_kib"],
            "idle_cpu_ticks": idle["idle_cpu_ticks"],
            "idle_seconds": idle["idle_seconds"],
            "idle_cpu_percent": idle["idle_cpu_percent"],
            "idle_settle_seconds": idle["idle_settle_seconds"],
        },
        "fast": stages["fast"],
        "sustained": stages["sustained"],
        "streaming_concurrency": stages["streaming_concurrency"],
        "streaming_throughput": stages["streaming_throughput"],
        "benchmarks": benchmarks,
    }
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(bundle, indent=2, sort_keys=False) + "\n", encoding="utf-8")
    return bundle


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--raw", required=True, type=Path, help="perf.sh raw output directory")
    parser.add_argument("--out", default="perf/results.json", type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    try:
        collect(args.raw, args.out if args.out.is_absolute() else root / args.out)
    except CollectError as exc:
        print(f"perf_collect: {exc}", file=sys.stderr)
        return 1
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
