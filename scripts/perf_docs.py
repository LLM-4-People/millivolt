#!/usr/bin/env python3
"""Render docs/performance.md from the measured result bundle.

The bundle (perf/results.json) is the only source of published performance and
resource numbers. scripts/perf.sh docs writes it from a fresh measurement run,
and this module turns it into the page. Nothing here invents a measurement: a
missing row, a failed stage or a malformed bundle is an error, never a blank
cell or a fallback value.

The page is checked byte for byte by tests/python/site.py, so a hand edit or a
stale bundle fails the unit job. Regenerate with:

    scripts/perf.sh docs
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BUNDLE_PATH = ROOT / "perf" / "results.json"
PAGE_PATH = ROOT / "docs" / "performance.md"
GENERATED_BY = "scripts/perf.sh docs"


class DataError(Exception):
    """The bundle cannot produce the page without inventing a value."""


def load_bundle(root: Path = ROOT) -> dict:
    path = root / "perf" / "results.json"
    try:
        bundle = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise DataError(f"{path.relative_to(root)} is missing; run {GENERATED_BY}") from exc
    except ValueError as exc:
        raise DataError(f"{path.relative_to(root)} is not valid JSON: {exc}") from exc
    if bundle.get("generated_by") != GENERATED_BY:
        raise DataError(f"{path.relative_to(root)} was not written by {GENERATED_BY}")
    for key in ("method", "artifacts", "fast", "sustained", "streaming_concurrency",
                "streaming_throughput", "benchmarks"):
        if key not in bundle:
            raise DataError(f"{path.relative_to(root)} has no {key} section")
    if not isinstance(bundle["method"], dict):
        raise DataError(f"{path.relative_to(root)} method is not an object")
    return bundle


def _number(value, what: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise DataError(f"{what} is not a number: {value!r}")
    return float(value)


def _num(value: float) -> str:
    return f"{value:,.0f}"


def _one(value: float) -> str:
    return f"{value:,.1f}"


def _two(value: float) -> str:
    return f"{value:,.2f}"


def _mib(byte_count: float) -> str:
    return f"{byte_count / (1024 * 1024):,.1f} MiB"


def _kib_mib(kib: float) -> str:
    return f"{kib / 1024:,.1f} MiB"


def _mb(byte_count: float) -> str:
    """Decimal megabytes, the unit the query table uses for allocation bytes."""
    if byte_count >= 1_000_000:
        return f"{byte_count / 1_000_000:,.2f} MB"
    return f"{byte_count / 1_000:,.1f} KB"


def _ms_from_ns(ns: float) -> str:
    return f"{ns / 1_000_000:,.2f} ms"


def _per_second(value: float) -> str:
    return f"{value:,.1f}"


def _flag(method: dict, key: str) -> bool:
    """A harness declaration the page publishes; missing is an error, not false."""
    value = method.get(key)
    if not isinstance(value, bool):
        raise DataError(f"method has no boolean {key}; rerun {GENERATED_BY}")
    return value


def _text(method: dict, key: str) -> str:
    value = method.get(key)
    if not isinstance(value, str) or not value:
        raise DataError(f"method has no text {key}; rerun {GENERATED_BY}")
    return value


def _fixture_shape(stages: list[dict], what: str) -> dict:
    """The one response shape every stage in a group drove.

    A table that describes "the synthetic response" is only honest when the
    stages agree; a mixed group would make the sentence describe some rows and
    not others.
    """
    if not stages:
        raise DataError(f"no {what} stage to describe")
    keys = ("chunks", "hold_s", "input_tokens", "cost_usd")
    shape = {key: _number(stages[0][key], f"{what} {key}") for key in keys}
    for stage in stages[1:]:
        for key, value in shape.items():
            if _number(stage[key], f"{what} {key}") != value:
                raise DataError(f"{what} stages disagree about fixture {key}")
    return shape


def _bytes_label(n: float) -> str:
    """A fixture body size taken from the benchmark's own sub-name."""
    for unit, size in (("MiB", 1 << 20), ("KiB", 1 << 10)):
        if n >= size and n % size == 0:
            return f"{int(n // size)} {unit}"
    return f"{_num(n)} bytes"


def _records_in(name: str) -> float:
    """The record count a benchmark sub-name declares (`records=100000`)."""
    match = re.search(r"records=(\d+)", name)
    if not match:
        raise DataError(f"benchmark {name!r} does not declare its record count")
    return float(match.group(1))


def _body_in(name: str) -> float:
    """The body size a benchmark sub-name declares (`bytes102400`)."""
    match = re.search(r"bytes(\d+)", name)
    if not match:
        raise DataError(f"benchmark {name!r} does not declare its body size")
    return float(match.group(1))


def _table(headers: list[str], rows: list[list[str]], numeric: list[int] = ()) -> str:
    if not rows:
        raise DataError("a generated table has no rows")
    numeric = set(numeric)
    head = "| " + " | ".join(headers) + " |"
    rule = "| " + " | ".join("---:" if i in numeric else "---" for i in range(len(headers))) + " |"
    body = ["| " + " | ".join(row) + " |" for row in rows]
    return "\n".join([head, rule] + body)


def _section(bundle: dict) -> dict:
    if not isinstance(bundle.get("method"), dict):
        raise DataError("bundle has no method section")
    return bundle


def _bench(bundle: dict, name: str) -> dict:
    for entry in bundle.get("benchmarks", []):
        if entry.get("name") == name:
            return entry
    raise DataError(f"bundle is missing benchmark {name!r}")


def _metric(bench: dict, unit: str) -> float:
    metrics = bench.get("metrics") or {}
    if unit not in metrics:
        raise DataError(f"benchmark {bench.get('name')!r} has no {unit!r} metric")
    return _number(metrics[unit], f"{bench.get('name')} {unit}")


def _method_table(method: dict) -> str:
    revision = method.get("revision") or ""
    link = f"[`{revision[:12]}`](https://github.com/LLM-4-People/millivolt/commit/{revision})"
    if method.get("dirty"):
        link += " (working tree had uncommitted changes)"
    rows = [
        ["Date", method["date_utc"]],
        ["Source revision", link],
        ["Go toolchain", f"{method['go']}, `CGO_ENABLED=0`"],
        ["Platform", method["platform"]],
        ["CPU", f"{method['cpu']}, {_num(_number(method['cpus'], 'cpus'))} visible logical CPUs"],
        ["Visible RAM", f"{_num(_number(method['ram_gib'], 'ram'))} GiB"],
        ["Configuration", method["config"]],
        ["Debug captures", "Off" if not _flag(method, "debug_captures") else "On"],
        ["Body previews", "Off" if not _flag(method, "body_previews") else "On"],
        ["Browser and live observer",
         "None during load stages" if not _flag(method, "live_observer") else "Attached during load stages"],
        ["Upstream", _text(method, "upstream")],
        ["Load shape", "Closed-loop HTTP stages, then drain"],
        ["RSS sampling", f"{_num(_number(method['rss_sampling_ms'], 'rss sampling'))} ms"],
        ["CPU unit", "100% equals one logical CPU"],
        ["Accounting check",
         "Exact stages reconcile rows, tokens and cost; a dropping stage reconciles durable plus dropped"],
        ["Host load (1-minute)", f"{method['load_1m_start']:g} at start, {method['load_1m_end']:g} at end"],
    ]
    return _table(["Item", "Setting"], rows)


def _artifact_table(artifacts: dict) -> str:
    binary = _number(artifacts["binary_bytes"], "binary bytes")
    image = _number(artifacts["image_bytes"], "image bytes")
    archive = _number(artifacts["archive_bytes"], "archive bytes")
    idle_kib = _number(artifacts["idle_rss_kib"], "idle rss")
    ticks = _number(artifacts["idle_cpu_ticks"], "idle cpu ticks")
    seconds = _number(artifacts["idle_seconds"], "idle seconds")
    percent = _number(artifacts["idle_cpu_percent"], "idle cpu percent")
    rows = [
        ["Static amd64 binary", _num(binary), _mib(binary), "Stripped, no CGO"],
        ["amd64 runtime image", _num(image), _mib(image), "Docker-reported content size"],
        ["Compressed image archive", _num(archive), _mib(archive), "`docker save` piped through `gzip -9`"],
        ["Idle process, empty SQLite", _num(idle_kib) + " KiB", _kib_mib(idle_kib),
         f"After a {_num(_number(artifacts.get('idle_settle_seconds', 15), 'settle'))}-second quiet interval"],
        ["Idle process CPU", f"{_num(ticks)} scheduler ticks", f"{percent:,.3f}%",
         f"{_num(ticks)} ticks over {_num(seconds)} seconds"],
    ]
    return _table(["Artifact or state", "Bytes", "Display", "Notes"], rows, [1, 2])


def _fast_table(stages: list[dict]) -> str:
    rows = []
    for stage in stages:
        workers = _num(_number(stage["workers"], "workers"))
        if _number(stage["seconds"], "seconds") >= 9:
            workers += ", " + _num(_number(stage["seconds"], "seconds")) + " s"
        rows.append([
            workers,
            _num(_number(stage["requests"], "requests")),
            _one(_number(stage["rps"], "rps")),
            _two(_number(stage["p50_ms"], "p50")) + " ms",
            _two(_number(stage["p95_ms"], "p95")) + " ms",
            _two(_number(stage["p99_ms"], "p99")) + " ms",
            f"{_one(_number(stage['cpu_percent'], 'cpu'))}%",
            f"{_one(_number(stage['peak_rss_mib'], 'rss'))} MiB",
            "Exact, " + _num(_number(stage["durable"], "durable")),
        ])
    return _table(["Client workers", "HTTP requests", "Requests/s", "p50", "p95", "p99",
                   "Proxy CPU", "Peak proxy RSS", "Durable rows"], rows, range(1, 9))


def _sustained_table(stages: list[dict]) -> str:
    rows = []
    for stage in stages:
        drops = _number(stage.get("drops", 0), "drops")
        durable = "Exact, " + _num(_number(stage["durable"], "durable"))
        if drops:
            durable = _num(_number(stage["durable"], "durable")) + " kept"
        rows.append([
            _num(_number(stage["workers"], "workers")),
            f"{_two(_number(stage['seconds'], 'seconds'))} s",
            _num(_number(stage["requests"], "requests")),
            _one(_number(stage["rps"], "rps")),
            _two(_number(stage["p50_ms"], "p50")) + " ms",
            _two(_number(stage["p95_ms"], "p95")) + " ms",
            _two(_number(stage["p99_ms"], "p99")) + " ms",
            f"{_one(_number(stage['cpu_percent'], 'cpu'))}%",
            f"{_one(_number(stage['peak_rss_mib'], 'rss'))} MiB",
            durable,
            _num(drops),
        ])
    return _table(["Workers", "Duration", "HTTP requests", "Requests/s", "p50", "p95", "p99",
                   "Proxy CPU", "Peak proxy RSS", "Durable rows", "Drops"], rows, range(2, 11))


def _streaming_rate_table(stages: list[dict]) -> str:
    rows = []
    for stage in stages:
        rows.append([
            _num(_number(stage["workers"], "workers")),
            _num(_number(stage["requests"], "requests")),
            _one(_number(stage["rps"], "rps")),
            _num(_number(stage["sse_per_s"], "sse")),
            _num(_number(stage["tokens_per_s"], "tokens")),
            _one(_number(stage["stream_kib_per_s"], "kib")),
            _two(_number(stage["p50_ms"], "p50")) + " ms",
            _two(_number(stage["p95_ms"], "p95")) + " ms",
            _two(_number(stage["p99_ms"], "p99")) + " ms",
            _two(_number(stage["first_byte_p95_ms"], "first byte")) + " ms",
        ])
    return _table(["Workers", "HTTP requests", "Requests/s", "SSE events/s", "Output tokens/s",
                   "Response KiB/s", "p50", "p95", "p99", "First-byte p95"], rows, range(1, 10))


def _streaming_resource_table(stages: list[dict]) -> str:
    rows = []
    for stage in stages:
        drops = _number(stage.get("drops", 0), "drops")
        durable = "Exact, " + _num(_number(stage["durable"], "durable"))
        if drops:
            durable = _num(drops) + " dropped"
        rows.append([
            _num(_number(stage["workers"], "workers")),
            f"{_one(_number(stage['cpu_percent'], 'cpu'))}%",
            f"{_one(_number(stage['peak_rss_mib'], 'rss'))} MiB",
            _num(_number(stage["upstream_peak"], "upstream peak")),
            durable,
            _num(drops),
        ])
    return _table(["Workers", "Proxy CPU", "Peak proxy RSS", "Upstream peak", "Durable rows", "Drops"],
                  rows, range(1, 6))


def _implementation_table(bundle: dict) -> str:
    rows = []
    for bench in bundle.get("benchmarks", []):
        if bench.get("suite") != "architecture":
            continue
        ns = _number(bench.get("ns_per_op"), bench["name"] + " ns/op")
        allocs = bench.get("metrics", {}).get("allocs/op")
        bop = bench.get("metrics", {}).get("B/op")
        rows.append([
            "`" + bench["name"] + "`",
            _ms_from_ns(ns) if ns >= 1_000_000 else f"{ns:,.0f} ns",
            _num(_number(bop, "B/op")) + " B" if bop is not None else "-",
            _num(_number(allocs, "allocs")) if allocs is not None else "-",
        ])
    if not rows:
        raise DataError("bundle has no architecture benchmarks; add the suite to scripts/perf.sh")
    return _table(["Benchmark", "Time", "Bytes per op", "Allocs per op"], rows)


def render(bundle: dict) -> str:
    _section(bundle)
    method = bundle["method"]
    artifacts = bundle["artifacts"]
    fast = bundle.get("fast") or []
    sustained = bundle.get("sustained") or []
    concurrency = bundle.get("streaming_concurrency") or []
    throughput = bundle.get("streaming_throughput") or []

    for key, stages in (("fast", fast), ("sustained", sustained),
                        ("streaming_concurrency", concurrency),
                        ("streaming_throughput", throughput)):
        if not stages:
            raise DataError(f"bundle has no {key} stages")

    fast_exact = [s for s in fast if not s.get("drops")]
    if not fast_exact:
        raise DataError("no fast-response stage completed with exact accounting")
    fast_best = max(fast_exact, key=lambda s: _number(s["rps"], "rps"))
    fast_long = max(fast, key=lambda s: _number(s["seconds"], "seconds"))
    sustained_exact = [s for s in sustained if not s.get("drops")]
    sustained_over = [s for s in sustained if s.get("drops")]
    if not sustained_exact:
        raise DataError("no sustained stage completed without durable drops")
    sustained_best = max(sustained_exact, key=lambda s: _number(s["rps"], "rps"))
    throughput_best = max(throughput, key=lambda s: _number(s["rps"], "rps"))
    plateau = next((s for s in throughput if _number(s["upstream_peak"], "peak") < _number(s["workers"], "workers")), None)
    plateau_index = throughput.index(plateau) if plateau else -1
    plateau_before = throughput[plateau_index - 1] if plateau_index > 0 else None
    loaded = min((s for s in fast if s.get("workers") == 128), key=lambda s: s["seconds"])
    max_conns = _num(_number(method["max_conns_per_host"], "max_conns_per_host"))

    binary = _number(artifacts["binary_bytes"], "binary")
    image = _number(artifacts["image_bytes"], "image")
    archive = _number(artifacts["archive_bytes"], "archive")
    idle_kib = _number(artifacts["idle_rss_kib"], "idle rss")
    writer_batch = _bench(bundle, "BenchmarkStorageWriter/batch")
    writer_queued = _bench(bundle, "BenchmarkStorageWriter/queued")
    drop32 = _bench(bundle, "BenchmarkProxyDurableContention/workers32")
    drop128 = _bench(bundle, "BenchmarkProxyDurableContention/workers128")
    disk10 = _bench(bundle, "BenchmarkStoreDiskBytes/records=10000")
    disk100 = _bench(bundle, "BenchmarkStoreDiskBytes/records=100000")
    foot10 = _bench(bundle, "BenchmarkProjectionFootprint/records=10000")
    foot100 = _bench(bundle, "BenchmarkProjectionFootprint/records=100000")
    rss10 = _bench(bundle, "BenchmarkProjectionProcessRSS/records=10000")
    rss100 = _bench(bundle, "BenchmarkProjectionProcessRSS/records=100000")
    meta1 = _bench(bundle, "BenchmarkRequestMetadata/bytes1024")
    meta100 = _bench(bundle, "BenchmarkRequestMetadata/bytes102400")
    meta1m = _bench(bundle, "BenchmarkRequestMetadata/bytes1048576")
    query_fixture = _bench(bundle, "BenchmarkQueryBeforeEquality")

    fast_shape = _fixture_shape(fast, "fast")
    sustained_shape = _fixture_shape(sustained, "sustained")
    if sustained_shape != fast_shape:
        raise DataError("sustained stages do not use the fast-response fixture")
    for shape, what in ((fast_shape, "fast"), (sustained_shape, "sustained")):
        if _number(shape["hold_s"], "stream hold") != 0:
            raise DataError(f"{what} stages used a stream hold; the page describes a buffered response")
    streaming_shape = _fixture_shape(concurrency, "concurrency")
    throughput_shape = _fixture_shape(throughput, "throughput")
    fixture_text = (
        f"{_num(_number(fast_shape['chunks'], 'chunks'))} content frames, usage "
        f"`{_num(_number(fast_shape['input_tokens'], 'input tokens'))}/"
        f"{_num(_number(fast_shape['chunks'], 'chunks'))}`, and fixture cost "
        f"`{_number(fast_shape['cost_usd'], 'fixture cost'):g}`"
    )
    chunk_label = f"{_num(_number(streaming_shape['chunks'], 'chunks'))}-frame"
    hold_label = f"{_num(_number(streaming_shape['hold_s'], 'hold'))}-second"
    throughput_chunks = f"{_num(_number(throughput_shape['chunks'], 'chunks'))}"
    throughput_hold = _num(_number(throughput_shape['hold_s'], 'hold'))
    fast_duration = _num(_number(method["fast_duration_s"], "fast duration"))
    sustained_duration = _num(_number(method["sustained_duration_s"], "sustained duration"))

    query_rows = []
    for field, label in (("provider", "Provider equality"), ("model", "Model equality"),
                         ("client", "Client equality"), ("key_hash", "Key-hash equality"),
                         ("conversation", "Conversation equality"), ("status", "Status equality")):
        before = _bench(bundle, f"BenchmarkQueryBeforeEquality/{field}")
        cursor = _bench(bundle, f"BenchmarkQueryBeforeEquality/{field}_cursor")
        query_rows.append([
            label,
            _ms_from_ns(_number(before["ns_per_op"], "ns")),
            _ms_from_ns(_number(cursor["ns_per_op"], "ns")),
            _mb(_metric(before, "B/op")) + ", " + _num(_metric(before, "allocs/op")) + " allocs",
        ])

    dirty = ""
    if method.get("dirty"):
        dirty = ("\n\n> **Measurement note:** the working tree had uncommitted changes during this "
                 "run. Rerun `scripts/perf.sh docs` on a clean commit before publishing.\n")

    fast_note = (
        f"The exact fast-response point is {_one(_number(fast_best['rps'], 'rps'))} requests/s "
        f"at {_num(_number(fast_best['workers'], 'workers'))} workers, with p95 "
        f"{_two(_number(fast_best['p95_ms'], 'p95'))} ms and p99 "
        f"{_two(_number(fast_best['p99_ms'], 'p99'))} ms. The "
        f"{_num(_number(fast_long['workers'], 'workers'))}-worker "
        f"{_num(_number(fast_long['seconds'], 'seconds'))}-second stage used "
        f"{_one(_number(fast_long['generator_cpu_percent'], 'generator cpu'))}% CPU and "
        f"{_one(_number(fast_long['generator_peak_rss_mib'], 'generator rss'))} MiB peak RSS in the "
        f"separate load generator and synthetic upstream process."
    )

    sustained_note = (
        f"The exact {sustained_duration}-second point is {_one(_number(sustained_best['rps'], 'rps'))} requests/s "
        f"at {_num(_number(sustained_best['workers'], 'workers'))} workers, with p95 "
        f"{_two(_number(sustained_best['p95_ms'], 'p95'))} ms and p99 "
        f"{_two(_number(sustained_best['p99_ms'], 'p99'))} ms."
    )
    if sustained_over:
        first_over = min(sustained_over, key=lambda s: _number(s["workers"], "workers"))
        drops = _number(first_over["drops"], "drops")
        pct = drops / _number(first_over["requests"], "requests") * 100
        sustained_note += (
            f" The {_num(_number(first_over['workers'], 'workers'))}-worker point dropped "
            f"{_num(drops)} rows ({pct:,.1f}% of its requests). A level that drops is an overload "
            f"observation, not a capacity result; the bounded durable queue is not lossless at "
            f"every load."
        )

    if plateau_before is not None:
        before_rps = _one(_number(plateau_before["rps"], "rps"))
        at_rps = _one(_number(plateau["rps"], "rps"))
        before_p50 = _number(plateau_before["p50_ms"], "p50")
        at_p50 = _number(plateau["p50_ms"], "p50")
        throughput_note = (
            f"Throughput peaked at {_one(_number(throughput_best['rps'], 'rps'))} requests/s at "
            f"{_num(_number(throughput_best['workers'], 'workers'))} workers. At "
            f"{_num(_number(plateau['workers'], 'workers'))} workers the upstream peak stayed at "
            f"{_num(_number(plateau['upstream_peak'], 'peak'))}, throughput was {at_rps} requests/s "
            f"against {before_rps} at {_num(_number(plateau_before['workers'], 'workers'))} workers, "
            f"and p50 latency rose from {before_p50 / 1000:,.2f} s to {at_p50 / 1000:,.2f} s. The "
            f"extra clients queued behind the configured {max_conns} upstream connections."
        )
    else:
        throughput_note = (
            f"Throughput peaked at {_one(_number(throughput_best['rps'], 'rps'))} requests/s at "
            f"{_num(_number(throughput_best['workers'], 'workers'))} workers. No stage exceeded the "
            f"configured upstream connection limit, so this ramp did not reach the queueing plateau."
        )

    return f"""# Performance and resource usage

This page is the single source for the published performance and resource
numbers. It records what was measured, how it was measured, and what the result
does not prove. The [performance and footprint](operations.md#performance-and-footprint)
section in operations owns the implementation cost model and `go_memory_limit`
behavior.

These are scoped local measurements of the native proxy process, not deployment
guarantees. The page was regenerated from the result bundle on
{method['date_utc']} UTC.{dirty}

## Artifact and idle footprint

{_artifact_table(artifacts)}

The image size includes the distroless base, dependency notices, and the
embedded UI. It is not a compressed download size, a registry digest, or a
persistent-volume estimate. Idle RSS and CPU describe one process on this host.
A short sample cannot prove that a longer idle period uses exactly the same
resources.

## Method

{_method_table(method)}

The public example configuration was used, with private listen and database
paths supplied by `scripts/dev.sh`. The method table records the harness
declarations for captures, previews, the observer and the upstream. CPU and RSS
describe the native Go process, not Docker's container-wide accounting.
Percentiles are nearest-rank.

## Fast-response throughput

Each stage used a fresh disposable database and the same synthetic response:
{fixture_text}. The upstream added no intentional delay. Each stage started
requests for {fast_duration} seconds, then let active requests drain. The final
row is a separate
{_num(_number(method['fast_long_duration_s'], 'long duration'))}-second run used
to reach about {_num(_number(fast_long['durable'], 'durable rows'))} durable rows
without a storage drop.

{_fast_table(fast)}

{fast_note}

All HTTP responses succeeded in these stages, including the
{_num(_number(fast_best['workers'], 'workers'))}-worker stage. That is not a
promise that every configuration and history size stays exact under overload.
The durable queue is bounded and best-effort; the deliberate overload fixture
below shows what a drop result looks like.

## Sustained capacity

The sustained capacity point is the highest tested closed-loop level that
completed a {sustained_duration}-second run with zero durable drops and exact
accounting. Each level started from a fresh disposable database. The upstream
response and client shape were the same as the fast-response table:
{fixture_text}.

{_sustained_table(sustained)}

{sustained_note}

## Streaming concurrency

The synthetic upstream emitted {chunk_label} response across approximately
{hold_label} stream, followed by usage and a terminator. Each stage started its
fixed number of requests and then drained. Latency includes the intentional
upstream delay, so these numbers are not proxy-only overhead.

{_streaming_rate_table(concurrency)}

{_streaming_resource_table(concurrency)}

These stages measure concurrency and end-to-end latency for that fixed fixture.
They do not measure SSE event rate, bytes per second, token throughput, TTFT,
chunk-size or hold-duration effects, sustained streaming, or the point where
streaming saturates. First-byte p95 is the first response byte, not the first
generated token.

The {_num(_number(concurrency[-1]['workers'], 'workers'))}-worker stage reached
{_num(_number(concurrency[-1]['upstream_peak'], 'peak'))} upstream connections
and remained exact. This is a configured concurrency result on one host, not a
universal request-capacity ceiling. The largest level also matches the example
configuration's `max_conns_per_host` of {max_conns}. No higher level, sustained
streaming stage, or automatic saturation sweep was run; see
[Limitations](#limitations).

## Streaming throughput

`scripts/perf.sh streaming` runs the longer end-to-end throughput ramp. Each
level starts a fresh private `scripts/dev.sh` instance and sends requests
through the real proxy to a local synthetic SSE upstream. Each level ran for
{_num(_number(method['streaming_duration_s'], 'streaming duration'))} seconds
with {throughput_chunks} content frames over a roughly {throughput_hold}-second
stream.

{_streaming_rate_table(throughput)}

{_streaming_resource_table(throughput)}

{throughput_note}

Every level completed with exact durable accounting unless its row reports
drops. This run started under the host load the harness recorded, so absolute
rates are comparable only within this page and this host.

## Durable writer and overload

These are isolated Go benchmarks from the test overlay, not HTTP runs. Every
row and drop count is verified against successful requests. The overload
benchmark deliberately fills the bounded queue, so its drop count is a fixture
result, not a recommended operating point.

{_table(["Path", "Durable rows/s", "Allocations"], [
    ["Direct batch insert at the configured batch capacity",
     _num(_metric(writer_batch, "records/s")),
     f"{_num(_metric(writer_batch, 'B/op'))} B/op, {_num(_metric(writer_batch, 'allocs/op'))} allocs/op"],
    ["Queued `Record` plus `Flush` at the queue capacity",
     _num(_metric(writer_queued, "records/s")),
     f"{_num(_metric(writer_queued, 'B/op'))} B/op, {_num(_metric(writer_queued, 'allocs/op'))} allocs/op"],
], [1, 2])}

{_table(["Overload fixture", "Wire requests/s", "Durable rows/s", "Dropped rows"], [
    ["32 workers", _num(_metric(drop32, "wire/s")), _num(_metric(drop32, "durable/s")),
     _num(_metric(drop32, "dropped"))],
    ["128 workers", _num(_metric(drop128, "wire/s")), _num(_metric(drop128, "durable/s")),
     _num(_metric(drop128, "dropped"))],
], [1, 2, 3])}

The overload fixture confirms the accounting invariant `durable + dropped =
successful requests`. Increasing queue capacity alone cannot make sustained
overload lossless. A higher HTTP success rate is not a complete-accounting
result unless the durable row count and drop delta agree.

## Storage and retained projection

The storage fixture stores complete records, indexes, projection triggers, and
marshaled response headers in SQLite. Disk bytes include the database, WAL, and
shared-memory sidecar after a final flush.

{_table(["Measure", f"{_num(_records_in(disk10['name']))} records", f"{_num(_records_in(disk100['name']))} records"], [
    ["SQLite bytes total", f"{_metric(disk10, 'MiB-total'):,.2f} MiB",
     f"{_metric(disk100, 'MiB-total'):,.2f} MiB"],
    ["SQLite bytes per record", _one(_metric(disk10, "B/record")), _one(_metric(disk100, "B/record"))],
    ["Projection retained, built", _one(_metric(foot10, "built-B/record")) + " B/record",
     _one(_metric(foot100, "built-B/record")) + " B/record"],
    ["Projection interned dimensions", _one(_metric(foot10, "interned-B/record")) + " B/record",
     _one(_metric(foot100, "interned-B/record")) + " B/record"],
    ["Projection scanned decode", _one(_metric(foot10, "scanned-B/record")) + " B/record",
     _one(_metric(foot100, "scanned-B/record")) + " B/record"],
    ["Projection transient allocation", _num(_metric(foot10, "transient-B/record")) + " B/record",
     _num(_metric(foot100, "transient-B/record")) + " B/record"],
], [1, 2])}

The projection process benchmark is a relative measure inside the test binary,
not the deployed proxy's RSS. At {_num(_records_in(rss100['name']))} records it
measured {_one(_metric(rss100, "MiB"))} MiB process RSS and a
{_one(_metric(rss100, "projection-MiB"))} MiB projection delta; at
{_num(_records_in(rss10['name']))} records it measured
{_one(_metric(rss10, "MiB"))} MiB process RSS and a
{_one(_metric(rss10, "projection-MiB"))} MiB delta. The test binary and its
linked packages are part of the absolute figure.

A fast-response run reached a {_one(_number(loaded['peak_rss_mib'], 'rss'))} MiB
peak with {_num(_number(loaded['durable'], 'durable'))} durable rows while no
dashboard projection was preloaded. Use the projection delta above for the
retained analytical cost, not the loaded-process peak as a substitute.

## Query and metadata paths

The query benchmarks seed a {_num(_metric(query_fixture, 'fixture-rows'))}-row
scratch database and decode a {_num(_metric(query_fixture, 'fixture-page'))}-row
page. These are single-iteration measurements on a shared host, so small
differences are not meaningful.

{_table(["Query", "Before cursor", "With cursor", "Allocations before cursor"], query_rows, [1, 2])}

The request metadata path performs one bounded metadata decode; the allocation
column is measured per call.

{_table(["Metadata body", "Time per request", "Throughput", "Allocations"], [
    [_bytes_label(_body_in(meta1['name'])), f"{_num(_number(meta1['ns_per_op'], 'ns'))} ns",
     f"{_one(_metric(meta1, 'MB/s'))} MB/s",
     f"{_num(_metric(meta1, 'B/op'))} B, {_num(_metric(meta1, 'allocs/op'))} alloc"],
    [_bytes_label(_body_in(meta100['name'])), f"{_num(_number(meta100['ns_per_op'], 'ns'))} ns",
     f"{_one(_metric(meta100, 'MB/s'))} MB/s",
     f"{_num(_metric(meta100, 'B/op'))} B, {_num(_metric(meta100, 'allocs/op'))} alloc"],
    [_bytes_label(_body_in(meta1m['name'])), f"{_num(_number(meta1m['ns_per_op'], 'ns'))} ns",
     f"{_one(_metric(meta1m, 'MB/s'))} MB/s",
     f"{_num(_metric(meta1m, 'B/op'))} B, {_num(_metric(meta1m, 'allocs/op'))} alloc"],
], [1, 2, 3])}

## Implementation benchmarks

These are the isolated benchmark results the design notes cite. They are current
measurements from the same run as the tables above, not a historical before and
after across revisions.

{_implementation_table(bundle)}

## Reproduce

`scripts/perf.sh` owns the repeatable local run: `bench`, `stress`, `streaming`,
`sustained`, `image`, or `all`. `scripts/perf.sh docs` runs the full set,
writes `perf/results.json`, and regenerates this page. It refuses a dirty tree
unless `PERF_ALLOW_DIRTY=1` is set, because published numbers must name a
commit.

The full run uses the documented levels and durations:
{_num(_number(method['fast_duration_s'], 'fast duration'))} seconds for each
fast stage, {_num(_number(method['sustained_duration_s'], 'sustained duration'))}
seconds for each sustained level, and
{_num(_number(method['streaming_duration_s'], 'streaming duration'))} seconds for
each streaming throughput level. Override the level lists and durations with
`PERF_FAST_LEVELS`, `PERF_SUSTAINED_LEVELS`, `PERF_STREAMING_LEVELS`,
`PERF_FAST_DURATION`, `PERF_SUSTAINED_DURATION`, `PERF_STREAMING_DURATION`,
`PERF_STREAMING_HOLD` and `PERF_STREAMING_CHUNKS`. The bundle records what ran;
a changed run changes the page.

The individual commands below are the same evidence path. Use a fresh
`DEV_COPY_DB=0` instance for each workload. Never target the main instance on
port 8080. The stress harness verifies that the target is the private
`scripts/dev.sh` instance before sending traffic.

```sh
(
  set -e
  trap 'scripts/dev.sh stop' EXIT
  export MILLIVOLT_OPERATOR_TOKEN='dev-instance-credential'
  DEV_COPY_DB=0 scripts/dev.sh
  go run ./cmd/stress -target http://127.0.0.1:8081 \\
    -concurrency 1,8,32,128 -duration 5s -stream-duration 0
)
```

The isolated benchmarks use the test overlay:

```sh
scripts/check.sh go test -run='^$' -bench='BenchmarkProjectionFootprint|BenchmarkStoreDiskBytes' \\
  -benchtime=1x -count=1 ./internal/web
scripts/check.sh go test -run='^$' -bench='BenchmarkProjectionProcessRSS' \\
  -benchtime=1x -count=1 ./internal/web
scripts/check.sh go test -run='^$' -bench='BenchmarkStorageWriter' \\
  -benchtime=2s -benchmem -count=1 ./internal/storage
scripts/check.sh go test -run='^$' -bench='BenchmarkQueryBeforeEquality' \\
  -benchtime=1x -benchmem -count=1 ./internal/storage
scripts/check.sh go test -run='^$' -bench='Benchmark(RequestMetadata|ProxyDurableContention)' \\
  -benchtime=2s -benchmem -count=1 ./internal/proxy
```

The image measurements use a disposable local image:

```sh
docker build --platform linux/amd64 -t millivolt-perf .
docker image inspect millivolt-perf --format '{{{{.Size}}}}'
docker save millivolt-perf | gzip -9 | wc -c
```

## Limitations

- The host was shared. Other workloads can change absolute rates and latency.
- The upstream was a local synthetic server. Provider latency, TLS, retries,
  payload shape, and real identity cardinality are not represented.
- These are short closed-loop bursts, not sustained production capacity. No
  result here is a capacity ceiling or a guarantee.
- The streaming concurrency table is a short ramp on a {chunk_label}, roughly
  {hold_label} fixture. The streaming throughput table is a
  {_num(_number(method['streaming_duration_s'], 'streaming duration'))}-second
  per-level ramp on a
  {throughput_chunks}-frame, roughly {throughput_hold}-second
  fixture. Both are synthetic-fixture measurements, not universal maxima. The
  throughput table's
  {_num(_number(throughput[-1]['workers'], 'workers'))}-client level is above the
  example `max_conns_per_host` of {max_conns} and measures queueing; it is a
  fixed level list, not an automatic saturation search. Its token rate is the
  fixture's usage count, not provider-token throughput.
- The {sustained_duration}-second sustained sweep uses the fast-response fixture with no
  intentional stream hold (`-stream-duration 0`). A level with durable drops is
  an overload observation, not a maximum streaming rate. The streaming
  throughput ramp is the only sustained held-stream run; it stops at its fixed
  levels and does not search beyond the largest listed level.
- Performance checks are manual (`scripts/perf.sh docs`), not CI. Each level is
  one run per invocation on one host; there is no repetition or variance study.
- Durable accounting is best-effort. The fast stages happened to remain exact;
  the overload benchmark intentionally does not.
- Container-wide CPU/RSS, cold-start time, browser rendering, arm64 image size,
  disk throughput, and a full dashboard preload over a large authorized history
  were not measured for this page.
- The image archive is a local artifact, not a published registry digest.
- Do not compare these numbers directly across differing hosts, workloads, or
  revisions.
"""


def write(root: Path = ROOT) -> Path:
    bundle = load_bundle(root)
    page = root / "docs" / "performance.md"
    page.write_text(render(bundle), encoding="utf-8")
    return page


def check(root: Path = ROOT) -> list[str]:
    try:
        bundle = load_bundle(root)
        want = render(bundle)
    except DataError as exc:
        return [f"performance page: {exc}"]
    page = root / "docs" / "performance.md"
    if not page.is_file():
        return ["performance page: docs/performance.md is missing; run scripts/perf.sh docs"]
    got = page.read_text(encoding="utf-8")
    if got != want:
        return ["performance page: docs/performance.md is stale; run scripts/perf.sh docs"]
    return []


def landing_metrics(root: Path = ROOT) -> dict[str, str]:
    """The four headline values the landing page carries, from the same bundle."""
    bundle = load_bundle(root)
    artifacts = bundle["artifacts"]
    fast = bundle.get("fast") or []
    sustained = bundle.get("sustained") or []
    loaded = next((s for s in fast if s.get("workers") == 128), None)
    if loaded is None:
        raise DataError("bundle has no fast stage with 128 workers")
    exact = [s for s in sustained if not s.get("drops")]
    if not exact:
        raise DataError("bundle has no exact sustained stage")
    point = next((s for s in exact if s.get("workers") == 48), max(exact, key=lambda s: s["rps"]))
    method = bundle["method"]
    # The headline describes the configured stage, not its measured elapsed
    # time: a 300-second run finishes at 300.0x seconds, and rounding that into
    # a "minutes" label would either lie or lose the intended shape.
    seconds = _number(method["sustained_duration_s"], "sustained duration")
    workers = _num(_number(point["workers"], "workers"))
    if seconds >= 120 and seconds % 60 == 0:
        sustained = f"{int(seconds // 60)} minutes at {workers} workers"
    else:
        sustained = f"{_num(seconds)} s at {workers} workers"
    revision = str(method.get("revision") or "")
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise DataError("method revision is not a full commit hash")
    date = _text(method, "date_utc")
    if not re.fullmatch(r"\d{4}-\d{2}-\d{2}", date):
        raise DataError("method date is not an ISO date")
    return {
        "perf_image": _mib(_number(artifacts["image_bytes"], "image bytes")),
        "perf_idle": _kib_mib(_number(artifacts["idle_rss_kib"], "idle rss")),
        "perf_loaded": f"{_one(_number(loaded['peak_rss_mib'], 'rss'))} MiB",
        "perf_rps": _one(_number(point["rps"], "rps")),
        "perf_sustained": sustained,
        "perf_date": date,
        "perf_revision": revision[:7],
        "perf_revision_full": revision,
        "perf_platform": _text(method, "platform"),
    }


def main(argv: list[str]) -> int:
    if len(argv) != 2 or argv[1] not in ("--write", "--check"):
        print("usage: scripts/perf_docs.py --write|--check", file=sys.stderr)
        return 2
    if argv[1] == "--write":
        try:
            print(f"wrote {write().relative_to(ROOT)}")
        except DataError as exc:
            print(f"perf_docs: {exc}", file=sys.stderr)
            return 1
        return 0
    errors = check()
    for error in errors:
        print(f"perf_docs: {error}", file=sys.stderr)
    if not errors:
        print("performance page: clean")
    return 1 if errors else 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
