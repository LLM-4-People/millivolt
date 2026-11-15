#!/usr/bin/env python3
"""Regressions for the measured-page pipeline.

The published performance page is generated from a result bundle. These tests
drive the collector and renderer with fixture output, so a parser or validation
regression fails in the unit job instead of producing a plausible page from a
partial run. They never start a proxy or run a benchmark.
"""

from __future__ import annotations

import json
import re
import tempfile
import unittest
from pathlib import Path

from scripts import perf_collect, perf_docs


def benchmark_line(name: str, ns: float, **metrics) -> str:
    fields = [f"Benchmark{name}-12", "1", f"{ns:.0f} ns/op"]
    for unit, value in metrics.items():
        fields += [f"{value:.0f}", unit]
    return "\t".join(fields)


def stage(workers: int, seconds: float, rps: float, *, drops: int = 0,
          chunks: int = 10, hold: int = 0) -> dict:
    requests = int(rps * seconds)
    return {
        "workers": workers, "requests": requests, "successes": requests,
        "failures": 0, "seconds": seconds, "rps": rps, "p50_ms": 1.0,
        "p95_ms": 2.0, "p99_ms": 3.0, "first_byte_p95_ms": 0.5,
        "cpu_percent": 10.0, "peak_rss_mib": 20.0,
        "generator_cpu_percent": 5.0, "generator_peak_rss_mib": 10.0,
        "upstream_peak": min(workers, 512), "sse_per_s": rps * 10,
        "tokens_per_s": rps * 9, "stream_kib_per_s": rps, "durable": requests - drops,
        "drops": drops, "accounting_ok": True,
        "chunks": chunks, "hold_s": hold, "input_tokens": 100, "cost_usd": 0.001,
    }


def bench(name: str, suite: str, ns: float = 1000.0, metrics: dict | None = None, **extra) -> dict:
    merged = dict(metrics or {})
    merged.update(extra)
    return {"name": name, "suite": suite, "ns_per_op": ns, "metrics": merged}


def bundle() -> dict:
    query = []
    for field in ("provider", "model", "client", "key_hash", "conversation", "status"):
        query.append(bench(f"BenchmarkQueryBeforeEquality/{field}", "query",
                           metrics={"B/op": 100.0, "allocs/op": 2.0}))
        query.append(bench(f"BenchmarkQueryBeforeEquality/{field}_cursor", "query",
                           metrics={"B/op": 90.0, "allocs/op": 2.0}))
    return {
        "schema": 1,
        "generated_by": perf_docs.GENERATED_BY,
        "method": {
            "revision": "0123456789abcdef0123456789abcdef01234567", "dirty": False,
            "branch": "testing", "date_utc": "2026-11-15",
            "go": "go1.27.4", "platform": "Linux/amd64", "cpu": "Fixture CPU",
            "cpus": 12, "ram_gib": 62, "rss_sampling_ms": 100,
            "config": "Public example with private listen and database paths",
            "debug_captures": False, "body_previews": False,
            "live_observer": False, "upstream": "Local synthetic server on loopback",
            "max_conns_per_host": 512, "fast_duration_s": 5, "fast_long_duration_s": 10,
            "sustained_duration_s": 300, "streaming_duration_s": 60,
            "streaming_hold_s": 1, "streaming_chunks": 100,
            "load_1m_start": 1.0, "load_1m_end": 2.0,
        },
        "artifacts": {
            "binary_bytes": 20_000_000, "image_bytes": 30_000_000,
            "archive_bytes": 9_000_000, "idle_rss_kib": 27_000,
            "idle_cpu_ticks": 2, "idle_seconds": 30, "idle_cpu_percent": 0.067,
            "idle_settle_seconds": 15,
        },
        "fast": [stage(1, 5, 800), stage(128, 5, 11000), stage(128, 10, 9000)],
        "sustained": [stage(48, 300, 14000), stage(64, 300, 13000, drops=10)],
        "streaming_concurrency": [stage(1, 5, 1, hold=1), stage(512, 5, 480, hold=1)],
        "streaming_throughput": [stage(1, 60, 1, chunks=100, hold=1),
                                 stage(512, 60, 470, chunks=100, hold=1),
                                 stage(1024, 60, 460, chunks=100, hold=1)],
        "benchmarks": [
            bench("BenchmarkStorageWriter/batch", "writer",
                 metrics={"records/s": 15000.0, "B/op": 400.0, "allocs/op": 7.0}),
            bench("BenchmarkStorageWriter/queued", "writer",
                 metrics={"records/s": 30000.0, "B/op": 500.0, "allocs/op": 8.0}),
            bench("BenchmarkProxyDurableContention/workers32", "overload",
                 metrics={"wire/s": 70000.0, "durable/s": 15000.0, "dropped": 100.0}),
            bench("BenchmarkProxyDurableContention/workers128", "overload",
                 metrics={"wire/s": 70000.0, "durable/s": 13000.0, "dropped": 200.0}),
            bench("BenchmarkStoreDiskBytes/records=10000", "store_disk",
                 metrics={"B/record": 700.0, "MiB-total": 6.7}),
            bench("BenchmarkStoreDiskBytes/records=100000", "store_disk",
                 metrics={"B/record": 500.0, "MiB-total": 45.0}),
            bench("BenchmarkProjectionFootprint/records=10000", "projection",
                 metrics={"built-B/record": 450.0, "interned-B/record": 440.0,
                          "scanned-B/record": 570.0, "transient-B/record": 2800.0}),
            bench("BenchmarkProjectionFootprint/records=100000", "projection",
                 metrics={"built-B/record": 470.0, "interned-B/record": 430.0,
                          "scanned-B/record": 560.0, "transient-B/record": 3000.0}),
            bench("BenchmarkProjectionProcessRSS/records=10000", "process_rss",
                 metrics={"MiB": 37.0, "projection-MiB": 20.0}),
            bench("BenchmarkProjectionProcessRSS/records=100000", "process_rss",
                 metrics={"MiB": 93.0, "projection-MiB": 76.0}),
            *query,
            bench("BenchmarkQueryBeforeEquality", "query",
                 metrics={"fixture-rows": 100000.0, "fixture-page": 200.0}),
            bench("BenchmarkRequestMetadata/bytes1024", "metadata",
                 metrics={"MB/s": 2500.0, "B/op": 16.0, "allocs/op": 1.0}),
            bench("BenchmarkRequestMetadata/bytes102400", "metadata",
                 metrics={"MB/s": 13000.0, "B/op": 16.0, "allocs/op": 1.0}),
            bench("BenchmarkRequestMetadata/bytes1048576", "metadata",
                 metrics={"MB/s": 9000.0, "B/op": 16.0, "allocs/op": 1.0}),
            bench("BenchmarkHandleLogPage/limit_60", "architecture",
                 metrics={"B/op": 900.0, "allocs/op": 2000.0}),
        ],
    }


class CollectorTests(unittest.TestCase):
    def write(self, directory: Path, name: str, text: str) -> Path:
        path = directory / name
        path.write_text(text, encoding="utf-8")
        return path

    def test_benchmark_metrics_and_suite_are_parsed(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = self.write(Path(tmp), "bench.log", "\n".join([
                benchmark_line("StorageWriter/batch", 1000, **{
                    "records/s": 1234, "B/op": 456, "allocs/op": 7}),
                benchmark_line("HandleLogPage/limit_60", 2000000),
            ]))
            entries = perf_collect.parse_benchmarks([path])
        self.assertEqual(["BenchmarkStorageWriter/batch", "BenchmarkHandleLogPage/limit_60"],
                         [entry["name"] for entry in entries])
        self.assertEqual("writer", entries[0]["suite"])
        self.assertEqual(1234.0, entries[0]["metrics"]["records/s"])
        self.assertEqual("architecture", entries[1]["suite"])

    def test_malformed_or_duplicate_benchmarks_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            bad = self.write(Path(tmp), "bad.log",
                             "BenchmarkX-12\t1\t100 ns/op\t123\n")
            with self.assertRaises(perf_collect.CollectError):
                perf_collect.parse_benchmarks([bad])
            duplicate = self.write(Path(tmp), "dupe.log", "\n".join([
                benchmark_line("X", 100), benchmark_line("X", 200)]))
            with self.assertRaises(perf_collect.CollectError):
                perf_collect.parse_benchmarks([duplicate])

    def stress_line(self, **overrides) -> str:
        entry = {
            "concurrency": 1, "upstream_peak": 1, "Requests": 10, "Successes": 10,
            "Failures": 0, "Seconds": 5, "RequestsPerSecond": 2,
            "P50Ms": 1, "P95Ms": 2, "P99Ms": 3, "FirstByteP95Ms": 0.5,
            "CPUPercent": 10, "PeakRSSMiB": 20, "GeneratorCPUPercent": 5,
            "GeneratorPeakRSSMiB": 10, "sse_events_per_second": 20,
            "output_tokens_per_second": 18, "stream_bytes_per_second": 1024,
            "DurableRequests": 10, "storage_dropped": 0, "AccountingOK": True,
            "chunks": 10, "fixture_input_tokens": 100, "fixture_cost": 0.001,
        }
        entry.update(overrides)
        return json.dumps(entry)

    def test_stage_requires_success_and_balanced_accounting(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "stage.jsonl"
            path.write_text("\n".join([
                json.dumps({"kind": "workload", "chunks": 10, "stream_duration": "0s",
                            "fixture_input_tokens": 100, "fixture_cost": 0.001,
                            "performance_settings": {"max_conns_per_host": 512}}),
                self.stress_line()]), encoding="utf-8")
            _workload, stage = perf_collect.normalize_stage(path)
            self.assertEqual(10, stage["requests"])
            for bad in (
                self.stress_line(Failures=1),
                self.stress_line(AccountingOK=False),
                self.stress_line(DurableRequests=6, storage_dropped=0),
                # A drop must balance: durable + dropped = successes.
                self.stress_line(Successes=10, DurableRequests=6, storage_dropped=3),
            ):
                path.write_text("\n".join([
                    json.dumps({"kind": "workload", "chunks": 10, "stream_duration": "0s",
                            "fixture_input_tokens": 100, "fixture_cost": 0.001,
                            "performance_settings": {"max_conns_per_host": 512}}),
                    bad]), encoding="utf-8")
                with self.assertRaises(perf_collect.CollectError):
                    perf_collect.normalize_stage(path)
            path.write_text("\n".join([
                json.dumps({"kind": "workload", "chunks": 10, "stream_duration": "0s",
                            "fixture_input_tokens": 100, "fixture_cost": 0.001,
                            "performance_settings": {"max_conns_per_host": 512}}),
                self.stress_line(Successes=10, DurableRequests=6, storage_dropped=4)]),
                encoding="utf-8")
            _workload, dropped = perf_collect.normalize_stage(path)
            self.assertEqual(4, dropped["drops"])


class RendererTests(unittest.TestCase):
    def test_page_is_rendered_and_checked_byte_for_byte(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "perf").mkdir()
            (root / "docs").mkdir()
            data = bundle()
            (root / "perf" / "results.json").write_text(json.dumps(data), encoding="utf-8")
            perf_docs.write(root)
            page = (root / "docs" / "performance.md").read_text(encoding="utf-8")
            self.assertIn("## Implementation benchmarks", page)
            self.assertIn("BenchmarkHandleLogPage/limit_60", page)
            self.assertIn("14,000.0 requests/s at 48 workers", page)
            self.assertEqual([], perf_docs.check(root))
            (root / "docs" / "performance.md").write_text(page + "edited\n", encoding="utf-8")
            self.assertTrue(perf_docs.check(root))

    def test_missing_stage_is_not_papered_over(self):
        data = bundle()
        data["sustained"] = []
        with self.assertRaises(perf_docs.DataError):
            perf_docs.render(data)

    def test_mixed_fixture_shape_is_not_described_as_one_response(self):
        data = bundle()
        data["fast"][1]["chunks"] = 12
        with self.assertRaises(perf_docs.DataError):
            perf_docs.render(data)

    def test_malformed_bundles_raise_a_data_error(self):
        for key in ("artifacts", "method", "benchmarks"):
            data = bundle()
            del data[key]
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "perf").mkdir()
                (root / "perf" / "results.json").write_text(json.dumps(data), encoding="utf-8")
                with self.assertRaises(perf_docs.DataError):
                    perf_docs.load_bundle(root)
        data = bundle()
        del data["method"]["debug_captures"]
        with self.assertRaises(perf_docs.DataError):
            perf_docs.render(data)

    def test_landing_metrics_come_from_the_bundle(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "perf").mkdir()
            (root / "perf" / "results.json").write_text(json.dumps(bundle()), encoding="utf-8")
            values = perf_docs.landing_metrics(root)
        self.assertEqual("28.6 MiB", values["perf_image"])
        self.assertEqual("14,000.0", values["perf_rps"])
        self.assertEqual("5 minutes at 48 workers", values["perf_sustained"])
        self.assertEqual("2026-11-15", values["perf_date"])
        self.assertEqual("0123456", values["perf_revision"])
        self.assertEqual("0123456789abcdef0123456789abcdef01234567", values["perf_revision_full"])
        self.assertEqual("Linux/amd64", values["perf_platform"])

    def test_every_published_claim_follows_the_bundle(self):
        """Render two bundles that differ only in their recorded facts.

        A sentence that keeps its old number across both renders is a hand
        copy, not a measurement. This is the guard that would have caught the
        stale landing date, the fixed "five-minute" label and the fixture shape
        restated as prose.
        """
        first = bundle()
        second = bundle()
        method = second["method"]
        method["date_utc"] = "2026-03-02"
        method["revision"] = "f" * 40
        method["platform"] = "Linux/arm64"
        method["fast_duration_s"] = 3
        method["sustained_duration_s"] = 120
        second["artifacts"]["image_bytes"] = 40 * 1024 * 1024
        for entry in second["fast"]:
            entry["seconds"] = 3
        for entry in second["sustained"]:
            entry["seconds"] = 120
        for group in ("fast", "sustained"):
            for entry in second[group]:
                entry["chunks"] = 12
                entry["input_tokens"] = 120
                entry["cost_usd"] = 0.002
        for bench_entry in second["benchmarks"]:
            if bench_entry["name"] == "BenchmarkQueryBeforeEquality":
                bench_entry["metrics"] = {"fixture-rows": 5000.0, "fixture-page": 50.0}
        page = perf_docs.render(first)
        moved = perf_docs.render(second)
        for old, new in (
            ("The page was regenerated from the result bundle on\n2026-11-15", "2026-03-02"),
            ("requests for 5 seconds", "requests for 3 seconds"),
            ("completed a 300-second run", "completed a 120-second run"),
            ("10 content frames, usage `100/10`", "12 content frames, usage `120/12`"),
            ("fixture cost `0.001`", "fixture cost `0.002`"),
            ("seed a 100,000-row", "seed a 5,000-row"),
            ("decode a 200-row", "decode a 50-row"),
        ):
            self.assertIn(old, page)
            self.assertIn(new, moved)
            self.assertNotIn(old, moved)

    def test_provenance_must_look_like_a_real_measurement(self):
        for field, value in (("revision", "not-a-hash"), ("date_utc", "20 March")):
            data = bundle()
            data["method"][field] = value
            with tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "perf").mkdir()
                (root / "perf" / "results.json").write_text(json.dumps(data), encoding="utf-8")
                with self.assertRaises(perf_docs.DataError):
                    perf_docs.landing_metrics(root)

    def test_design_note_promises_have_published_benchmarks(self):
        """A design note that links to the measured page must keep its promise.

        A paragraph may name the benchmark that protects a path. When the same
        paragraph points at the published table, that benchmark has to be in the
        current bundle, or the link leads to a row the run no longer produces.
        """
        bundle = perf_docs.load_bundle()
        available = {entry["name"].split("/", 1)[0] for entry in bundle["benchmarks"]}
        text = (perf_docs.ROOT / "docs" / "architecture.md").read_text(encoding="utf-8")
        missing = []
        for block in re.split(r"\n\s*\n", text):
            if "performance.md" not in block:
                continue
            for name in re.findall(r"`(Benchmark[A-Za-z0-9_]+)", block):
                if name not in available:
                    missing.append(name)
        self.assertEqual([], missing,
                         "a design note promises a benchmark the page does not publish")

    def test_foreign_bundle_is_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "perf").mkdir()
            data = bundle()
            data["generated_by"] = "hand"
            (root / "perf" / "results.json").write_text(json.dumps(data), encoding="utf-8")
            with self.assertRaises(perf_docs.DataError):
                perf_docs.load_bundle(root)


if __name__ == "__main__":
    unittest.main()
