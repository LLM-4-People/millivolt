# Performance and resource usage

This page is the single source for the published performance and resource
numbers. It records what was measured, how it was measured, and what the result
does not prove. The [performance and footprint](operations.md#performance-and-footprint)
section in operations owns the implementation cost model and `go_memory_limit`
behavior.

These are scoped local measurements of the native proxy process, not deployment
guarantees. The page was regenerated from the result bundle on
2026-11-16 UTC.

## Artifact and idle footprint

| Artifact or state | Bytes | Display | Notes |
| --- | ---: | ---: | --- |
| Static amd64 binary | 21,663,904 | 20.7 MiB | Stripped, no CGO |
| amd64 runtime image | 38,022,232 | 36.3 MiB | Docker-reported content size |
| Compressed image archive | 9,887,262 | 9.4 MiB | `docker save` piped through `gzip -9` |
| Idle process, empty SQLite | 30,204 KiB | 29.5 MiB | After a 15-second quiet interval |
| Idle process CPU | 2 scheduler ticks | 0.067% | 2 ticks over 30 seconds |

The image size includes the distroless base, dependency notices, and the
embedded UI. It is not a compressed download size, a registry digest, or a
persistent-volume estimate. Idle RSS and CPU describe one process on this host.
A short sample cannot prove that a longer idle period uses exactly the same
resources.

## Method

| Item | Setting |
| --- | --- |
| Date | 2026-11-16 |
| Source revision | [`22c0b6b73655`](https://github.com/LLM-4-People/millivolt/commit/22c0b6b73655a349e086c345bc80907efbe16822) |
| Go toolchain | go1.27.4, `CGO_ENABLED=0` |
| Platform | Linux/amd64 under WSL2 |
| CPU | Intel(R) Core(TM) Ultra 9 285H, 12 visible logical CPUs |
| Visible RAM | 62 GiB |
| Configuration | Public example with private listen and database paths |
| Debug captures | Off |
| Body previews | Off |
| Browser and live observer | None during load stages |
| Upstream | Local synthetic server on loopback |
| Load shape | Closed-loop HTTP stages, then drain |
| RSS sampling | 100 ms |
| CPU unit | 100% equals one logical CPU |
| Accounting check | Exact stages reconcile rows, tokens and cost; a dropping stage reconciles durable plus dropped |
| Host load (1-minute) | 23.36 at start, 13.38 at end |

The public example configuration was used, with private listen and database
paths supplied by `scripts/dev.sh`. The method table records the harness
declarations for captures, previews, the observer and the upstream. CPU and RSS
describe the native Go process, not Docker's container-wide accounting.
Percentiles are nearest-rank.

## Fast-response throughput

Each stage used a fresh disposable database and the same synthetic response:
10 content frames, usage `100/10`, and fixture cost `0.001`. The upstream added no intentional delay. Each stage started
requests for 5 seconds, then let active requests drain. The final
row is a separate
10-second run used
to reach about 129,442 durable rows
without a storage drop.

| Client workers | HTTP requests | Requests/s | p50 | p95 | p99 | Proxy CPU | Peak proxy RSS | Durable rows |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 6,879 | 1,375.7 | 0.66 ms | 1.09 ms | 1.66 ms | 52.6% | 61.5 MiB | Exact, 6,879 |
| 8 | 30,264 | 6,050.9 | 1.17 ms | 2.28 ms | 4.06 ms | 239.3% | 88.3 MiB | Exact, 30,264 |
| 32 | 53,081 | 10,567.2 | 2.55 ms | 6.01 ms | 8.63 ms | 374.3% | 93.0 MiB | Exact, 53,081 |
| 128 | 66,813 | 13,346.4 | 8.40 ms | 20.19 ms | 28.29 ms | 465.2% | 119.0 MiB | Exact, 66,813 |
| 128, 10 s | 129,442 | 12,931.5 | 8.50 ms | 21.30 ms | 31.58 ms | 459.7% | 124.5 MiB | Exact, 129,442 |

The exact fast-response point is 13,346.4 requests/s at 128 workers, with p95 20.19 ms and p99 28.29 ms. The 128-worker 10-second stage used 227.3% CPU and 33.7 MiB peak RSS in the separate load generator and synthetic upstream process.

All HTTP responses succeeded in these stages, including the
128-worker stage. That is not a
promise that every configuration and history size stays exact under overload.
The durable queue is bounded and best-effort; the deliberate overload fixture
below shows what a drop result looks like.

## Sustained capacity

The sustained capacity point is the highest tested closed-loop level that
completed a 300-second run with zero durable drops and exact
accounting. Each level started from a fresh disposable database. The upstream
response and client shape were the same as the fast-response table:
10 content frames, usage `100/10`, and fixture cost `0.001`.

| Workers | Duration | HTTP requests | Requests/s | p50 | p95 | p99 | Proxy CPU | Peak proxy RSS | Durable rows | Drops |
| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 32 | 300.00 s | 2,584,965 | 8,616.5 | 2.54 ms | 9.93 ms | 19.71 ms | 289.1% | 107.0 MiB | Exact, 2,584,965 | 0 |
| 48 | 300.01 s | 3,683,159 | 12,276.8 | 3.12 ms | 8.92 ms | 15.68 ms | 421.1% | 121.9 MiB | Exact, 3,683,159 | 0 |
| 64 | 300.00 s | 4,649,811 | 15,499.2 | 3.62 ms | 8.18 ms | 12.03 ms | 522.6% | 125.8 MiB | 4,602,957 kept | 46,854 |
| 128 | 300.03 s | 4,169,782 | 13,897.8 | 7.77 ms | 20.25 ms | 32.11 ms | 451.4% | 141.1 MiB | 3,775,709 kept | 394,073 |

The exact 300-second point is 12,276.8 requests/s at 48 workers, with p95 8.92 ms and p99 15.68 ms. The 64-worker point dropped 46,854 rows (1.0% of its requests). A level that drops is an overload observation, not a capacity result; the bounded durable queue is not lossless at every load.

## Streaming concurrency

The synthetic upstream emitted 10-frame response across approximately
1-second stream, followed by usage and a terminator. Each stage started its
fixed number of requests and then drained. Latency includes the intentional
upstream delay, so these numbers are not proxy-only overhead.

| Workers | HTTP requests | Requests/s | SSE events/s | Output tokens/s | Response KiB/s | p50 | p95 | p99 | First-byte p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 5 | 1.0 | 12 | 10 | 0.6 | 1,009.61 ms | 1,010.49 ms | 1,010.49 ms | 2.22 ms |
| 8 | 40 | 7.9 | 95 | 79 | 4.7 | 1,007.21 ms | 1,009.82 ms | 1,010.22 ms | 3.59 ms |
| 32 | 160 | 31.4 | 377 | 314 | 18.8 | 1,007.57 ms | 1,016.44 ms | 1,059.19 ms | 8.23 ms |
| 64 | 320 | 62.9 | 755 | 629 | 37.6 | 1,008.33 ms | 1,037.34 ms | 1,054.23 ms | 31.17 ms |
| 128 | 640 | 124.5 | 1,494 | 1,245 | 74.4 | 1,009.33 ms | 1,074.57 ms | 1,107.40 ms | 69.27 ms |
| 256 | 1,280 | 248.0 | 2,976 | 2,480 | 148.2 | 1,010.96 ms | 1,093.08 ms | 1,119.54 ms | 89.49 ms |
| 512 | 2,560 | 490.0 | 5,880 | 4,900 | 292.9 | 1,008.64 ms | 1,150.71 ms | 1,153.58 ms | 145.75 ms |
| 1,024 | 3,072 | 494.8 | 5,938 | 4,948 | 295.7 | 2,016.27 ms | 2,144.02 ms | 2,157.64 ms | 1,133.32 ms |

| Workers | Proxy CPU | Peak proxy RSS | Upstream peak | Durable rows | Drops |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 | 0.2% | 31.7 MiB | 1 | Exact, 5 | 0 |
| 8 | 1.0% | 32.7 MiB | 8 | Exact, 40 | 0 |
| 32 | 3.7% | 36.7 MiB | 32 | Exact, 160 | 0 |
| 64 | 6.9% | 40.2 MiB | 64 | Exact, 320 | 0 |
| 128 | 13.2% | 49.8 MiB | 128 | Exact, 640 | 0 |
| 256 | 24.6% | 67.4 MiB | 256 | Exact, 1,280 | 0 |
| 512 | 48.6% | 103.7 MiB | 512 | Exact, 2,560 | 0 |
| 1,024 | 48.6% | 137.9 MiB | 512 | Exact, 3,072 | 0 |

These stages measure concurrency and end-to-end latency for that fixed fixture.
They do not measure SSE event rate, bytes per second, token throughput, TTFT,
chunk-size or hold-duration effects, sustained streaming, or the point where
streaming saturates. First-byte p95 is the first response byte, not the first
generated token.

The 1,024-worker stage reached
512 upstream connections
and remained exact. This is a configured concurrency result on one host, not a
universal request-capacity ceiling. The largest level also matches the example
configuration's `max_conns_per_host` of 512. No higher level, sustained
streaming stage, or automatic saturation sweep was run; see
[Limitations](#limitations).

## Streaming throughput

`scripts/perf.sh streaming` runs the longer end-to-end throughput ramp. Each
level starts a fresh private `scripts/dev.sh` instance and sends requests
through the real proxy to a local synthetic SSE upstream. Each level ran for
60 seconds
with 100 content frames over a roughly 1-second
stream.

| Workers | HTTP requests | Requests/s | SSE events/s | Output tokens/s | Response KiB/s | p50 | p95 | p99 | First-byte p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 57 | 0.9 | 96 | 94 | 4.8 | 1,067.07 ms | 1,079.25 ms | 1,085.80 ms | 3.02 ms |
| 8 | 448 | 7.5 | 760 | 746 | 37.9 | 1,066.92 ms | 1,125.67 ms | 1,146.53 ms | 6.08 ms |
| 32 | 1,824 | 30.3 | 3,086 | 3,026 | 153.7 | 1,055.96 ms | 1,067.91 ms | 1,073.94 ms | 3.17 ms |
| 64 | 3,648 | 60.2 | 6,140 | 6,020 | 305.9 | 1,061.67 ms | 1,072.37 ms | 1,088.48 ms | 3.88 ms |
| 128 | 7,296 | 120.5 | 12,291 | 12,050 | 612.3 | 1,060.13 ms | 1,069.44 ms | 1,113.02 ms | 5.44 ms |
| 256 | 14,592 | 239.2 | 24,395 | 23,916 | 1,215.2 | 1,061.58 ms | 1,138.38 ms | 1,174.13 ms | 11.75 ms |
| 512 | 28,789 | 471.5 | 48,094 | 47,151 | 2,395.8 | 1,063.37 ms | 1,141.20 ms | 1,300.62 ms | 15.32 ms |
| 1,024 | 29,696 | 478.6 | 48,819 | 47,861 | 2,431.9 | 2,128.84 ms | 2,162.96 ms | 2,285.10 ms | 1,091.76 ms |

| Workers | Proxy CPU | Peak proxy RSS | Upstream peak | Durable rows | Drops |
| --- | ---: | ---: | ---: | ---: | ---: |
| 1 | 1.2% | 31.5 MiB | 1 | Exact, 57 | 0 |
| 8 | 5.5% | 37.1 MiB | 8 | Exact, 448 | 0 |
| 32 | 14.9% | 47.8 MiB | 32 | Exact, 1,824 | 0 |
| 64 | 28.2% | 59.0 MiB | 64 | Exact, 3,648 | 0 |
| 128 | 57.9% | 81.3 MiB | 128 | Exact, 7,296 | 0 |
| 256 | 115.1% | 114.9 MiB | 256 | Exact, 14,592 | 0 |
| 512 | 233.1% | 152.2 MiB | 513 | Exact, 28,789 | 0 |
| 1,024 | 239.9% | 189.4 MiB | 512 | Exact, 29,696 | 0 |

Throughput peaked at 478.6 requests/s at 1,024 workers. At 1,024 workers the upstream peak stayed at 512, throughput was 478.6 requests/s against 471.5 at 512 workers, and p50 latency rose from 1.06 s to 2.13 s. The extra clients queued behind the configured 512 upstream connections.

Every level completed with exact durable accounting unless its row reports
drops. This run started under the host load the harness recorded, so absolute
rates are comparable only within this page and this host.

## Durable writer and overload

These are isolated Go benchmarks from the test overlay, not HTTP runs. Every
row and drop count is verified against successful requests. The overload
benchmark deliberately fills the bounded queue, so its drop count is a fixture
result, not a recommended operating point.

| Path | Durable rows/s | Allocations |
| --- | ---: | ---: |
| Direct batch insert at the configured batch capacity | 62,921 | 425,396 B/op, 7,279 allocs/op |
| Queued `Record` plus `Flush` at the queue capacity | 56,055 | 13,594,117 B/op, 232,920 allocs/op |

| Overload fixture | Wire requests/s | Durable rows/s | Dropped rows |
| --- | ---: | ---: | ---: |
| 32 workers | 72,619 | 19,994 | 101,507 |
| 128 workers | 64,161 | 14,605 | 113,242 |

The overload fixture confirms the accounting invariant `durable + dropped =
successful requests`. Increasing queue capacity alone cannot make sustained
overload lossless. A higher HTTP success rate is not a complete-accounting
result unless the durable row count and drop delta agree.

## Storage and retained projection

The storage fixture stores complete records, indexes, projection triggers, and
marshaled response headers in SQLite. Disk bytes include the database, WAL, and
shared-memory sidecar after a final flush.

| Measure | 10,000 records | 100,000 records |
| --- | ---: | ---: |
| SQLite bytes total | 6.84 MiB | 49.20 MiB |
| SQLite bytes per record | 717.3 | 515.9 |
| Projection retained, built | 449.6 B/record | 470.8 B/record |
| Projection interned dimensions | 442.0 B/record | 429.1 B/record |
| Projection scanned decode | 580.3 B/record | 576.0 B/record |
| Projection transient allocation | 3,125 B/record | 3,434 B/record |

The projection process benchmark is a relative measure inside the test binary,
not the deployed proxy's RSS. At 100,000 records it
measured 94.6 MiB process RSS and a
66.0 MiB projection delta; at
10,000 records it measured
40.6 MiB process RSS and a
12.0 MiB delta. The test binary and its
linked packages are part of the absolute figure.

A fast-response run reached a 119.0 MiB
peak with 66,813 durable rows while no
dashboard projection was preloaded. Use the projection delta above for the
retained analytical cost, not the loaded-process peak as a substitute.

## Query and metadata paths

The query benchmarks seed a 100,000-row
scratch database and decode a 200-row
page. These are single-iteration measurements on a shared host, so small
differences are not meaningful.

| Query | Before cursor | With cursor | Allocations before cursor |
| --- | ---: | ---: | --- |
| Provider equality | 4.24 ms | 3.85 ms | 1.38 MB, 5,455 allocs |
| Model equality | 2.81 ms | 2.93 ms | 1.38 MB, 5,445 allocs |
| Client equality | 36.52 ms | 18.73 ms | 1.38 MB, 5,442 allocs |
| Key-hash equality | 33.68 ms | 18.28 ms | 23.5 KB, 265 allocs |
| Conversation equality | 40.16 ms | 21.73 ms | 23.2 KB, 264 allocs |
| Status equality | 38.98 ms | 20.99 ms | 698.1 KB, 2,942 allocs |

The request metadata path performs one bounded metadata decode; the allocation
column is measured per call.

| Metadata body | Time per request | Throughput | Allocations |
| --- | ---: | ---: | ---: |
| 1 KiB | 428 ns | 2,580.8 MB/s | 16 B, 1 alloc |
| 100 KiB | 4,808 ns | 21,314.0 MB/s | 16 B, 1 alloc |
| 1 MiB | 75,397 ns | 13,908.4 MB/s | 16 B, 1 alloc |

## Implementation benchmarks

These are the isolated benchmark results the design notes cite. They are current
measurements from the same run as the tables above, not a historical before and
after across revisions.

| Benchmark | Time | Bytes per op | Allocs per op |
| --- | --- | --- | --- |
| `BenchmarkAggregateFold/chart` | 2.74 ms | 136,736 B | 28 |
| `BenchmarkAggregateFold/explorer` | 4.38 ms | 209,696 B | 37 |
| `BenchmarkAggregateFold/explorer_tool` | 3.73 ms | 807,840 B | 38 |
| `BenchmarkAggregateFold/chart_window` | 413,833 ns | 17,616 B | 28 |
| `BenchmarkHandleLogPage/empty/limit_60` | 1.70 ms | 987,376 B | 3,533 |
| `BenchmarkHandleLogPage/sql_provider/limit_60` | 1.95 ms | 550,080 B | 2,645 |
| `BenchmarkHandleLogPage/go_status_class/limit_60` | 1.89 ms | 903,504 B | 2,686 |
| `BenchmarkHandleLogPage/go_status_class_5xx/limit_60` | 4.88 ms | 1,819,616 B | 21,125 |
| `BenchmarkHandleLogPage/go_tool_missing/limit_60` | 5.00 ms | 1,883,032 B | 29,360 |
| `BenchmarkHandleLogPage/empty/limit_200` | 4.26 ms | 2,889,608 B | 7,999 |
| `BenchmarkHandleLogPage/sql_provider/limit_200` | 4.92 ms | 2,888,720 B | 7,993 |
| `BenchmarkHandleLogPage/go_status_class/limit_200` | 4.14 ms | 2,890,912 B | 8,015 |
| `BenchmarkHandleLogPage/go_status_class_5xx/limit_200` | 3.88 ms | 1,889,744 B | 21,029 |
| `BenchmarkHandleLogPage/go_tool_missing/limit_200` | 4.72 ms | 1,957,512 B | 29,258 |
| `BenchmarkProjectionMetricAppend/history_100000` | 11,393 ns | 960 B | 5 |
| `BenchmarkProjectionMetricAppend/history_1000000` | 11,716 ns | 960 B | 5 |
| `BenchmarkProjectionMetricAppend/append_cross_100000` | 1.58 ms | 3,956,976 B | 14 |
| `BenchmarkProjectionMetricAppend/append_cross_1000000` | 1.54 ms | 3,956,976 B | 14 |
| `BenchmarkAggregateUnchanged/identity_marshal` | 33,009 ns | 6,456 B | 6 |
| `BenchmarkAggregateUnchanged/unchanged` | 1,338 ns | 0 B | 0 |
| `BenchmarkKPI/records_10000/miss` | 403,272 ns | 83,632 B | 6 |
| `BenchmarkKPI/records_10000/hit` | 1,024 ns | 32 B | 3 |
| `BenchmarkKPI/records_100000/miss` | 7.47 ms | 804,528 B | 6 |
| `BenchmarkKPI/records_100000/hit` | 1,986 ns | 32 B | 3 |
| `BenchmarkKPI/records_1000000/miss` | 91.84 ms | 8,005,296 B | 6 |
| `BenchmarkKPI/records_1000000/hit` | 2,153 ns | 32 B | 3 |
| `BenchmarkObserveModelNames4096` | 7.59 ms | 3,613,216 B | 70,975 |
| `BenchmarkObserveModelNameEvent/rules0` | 29,568 ns | 3,136 B | 19 |
| `BenchmarkObserveModelNameEvent/rules64` | 99,814 ns | 26,992 B | 29 |
| `BenchmarkObserveModelNameEventWired/rules0` | 15,688 ns | 3,136 B | 19 |
| `BenchmarkObserveModelNameEventWired/rules64` | 76,644 ns | 31,088 B | 30 |
| `BenchmarkSkipJSONString/plain/102400` | 9,647 ns | 0 B | 0 |
| `BenchmarkSkipJSONString/escaped/102400` | 8,821 ns | 0 B | 0 |
| `BenchmarkSkipJSONString/tail/102400` | 6,882 ns | 0 B | 0 |
| `BenchmarkSkipJSONString/plain/1048576` | 84,535 ns | 0 B | 0 |
| `BenchmarkSkipJSONString/escaped/1048576` | 86,790 ns | 0 B | 0 |
| `BenchmarkSkipJSONString/tail/1048576` | 67,966 ns | 0 B | 0 |
| `BenchmarkRequestMetadataShapes/escaped` | 629,819 ns | 35,552 B | 370 |
| `BenchmarkRequestMetadataShapes/many_messages` | 254,854 ns | 32,280 B | 30 |
| `BenchmarkRequestMetadataShapes/multimodal` | 168,469 ns | 16,240 B | 73 |
| `BenchmarkRequestMetadataShapes/tool_schema` | 85,121 ns | 3,480 B | 22 |
| `BenchmarkRequestMetadataShapes/digit_description` | 73,440 ns | 2,648 B | 6 |
| `BenchmarkAnalyzerFeed/content` | 3,503 ns | 0 B | 0 |
| `BenchmarkAnalyzerFeed/reasoning` | 2,881 ns | 0 B | 0 |
| `BenchmarkAnalyzerFeed/tool_call` | 6,972 ns | 240 B | 1 |
| `BenchmarkAnalyzerFeed/usage` | 3,131 ns | 0 B | 0 |
| `BenchmarkAnalyzerFeed/negative` | 1,798 ns | 0 B | 0 |
| `BenchmarkAnalyzerToolNameDedupe/10000` | 4.88 ms | 192,096 B | 10,121 |
| `BenchmarkAnalyzerToolNameDedupe/100000` | 48.39 ms | 694,264 B | 100,220 |

## Reproduce

`scripts/perf.sh` owns the repeatable local run: `bench`, `stress`, `streaming`,
`sustained`, `image`, or `all`. `scripts/perf.sh docs` runs the full set,
writes `perf/results.json`, and regenerates this page. It refuses a dirty tree
unless `PERF_ALLOW_DIRTY=1` is set, because published numbers must name a
commit.

The full run uses the documented levels and durations:
5 seconds for each
fast stage, 300
seconds for each sustained level, and
60 seconds for
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
  go run ./cmd/stress -target http://127.0.0.1:8081 \
    -concurrency 1,8,32,128 -duration 5s -stream-duration 0
)
```

The isolated benchmarks use the test overlay:

```sh
scripts/check.sh go test -run='^$' -bench='BenchmarkProjectionFootprint|BenchmarkStoreDiskBytes' \
  -benchtime=1x -count=1 ./internal/web
scripts/check.sh go test -run='^$' -bench='BenchmarkProjectionProcessRSS' \
  -benchtime=1x -count=1 ./internal/web
scripts/check.sh go test -run='^$' -bench='BenchmarkStorageWriter' \
  -benchtime=2s -benchmem -count=1 ./internal/storage
scripts/check.sh go test -run='^$' -bench='BenchmarkQueryBeforeEquality' \
  -benchtime=1x -benchmem -count=1 ./internal/storage
scripts/check.sh go test -run='^$' -bench='Benchmark(RequestMetadata|ProxyDurableContention)' \
  -benchtime=2s -benchmem -count=1 ./internal/proxy
```

The image measurements use a disposable local image:

```sh
docker build --platform linux/amd64 -t millivolt-perf .
docker image inspect millivolt-perf --format '{{.Size}}'
docker save millivolt-perf | gzip -9 | wc -c
```

## Limitations

- The host was shared. Other workloads can change absolute rates and latency.
- The upstream was a local synthetic server. Provider latency, TLS, retries,
  payload shape, and real identity cardinality are not represented.
- These are short closed-loop bursts, not sustained production capacity. No
  result here is a capacity ceiling or a guarantee.
- The streaming concurrency table is a short ramp on a 10-frame, roughly
  1-second fixture. The streaming throughput table is a
  60-second
  per-level ramp on a
  100-frame, roughly 1-second
  fixture. Both are synthetic-fixture measurements, not universal maxima. The
  throughput table's
  1,024-client level is above the
  example `max_conns_per_host` of 512 and measures queueing; it is a
  fixed level list, not an automatic saturation search. Its token rate is the
  fixture's usage count, not provider-token throughput.
- The 300-second sustained sweep uses the fast-response fixture with no
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
