# Performance and resource usage

This page is the single source for the published performance and resource
numbers. It records what was measured, how it was measured, and what the result
does not prove. The [performance and footprint](operations.md#performance-and-footprint)
section in operations owns the implementation cost model and `go_memory_limit`
behavior.

These are scoped local measurements of the native proxy process, not deployment
guarantees. They were taken on 2026-10-20 UTC. The footprint, fast-response,
streaming, writer, storage and query tables used source revision
[`facb699`](https://github.com/LLM-4-People/millivolt/commit/facb69900ac64c19ec3262fcb88b6428d33a68d2);
the sustained-capacity table used revision `54d1276`. Measurements used Go 1.27.2 on Linux/amd64 under WSL2. The host was a shared Intel Core Ultra
9 285H with 12 visible logical CPUs and 62 GiB of visible RAM. Builds and tests
finished before measurement. The public example configuration was used, with
private listen and database paths supplied by `scripts/dev.sh`. Debug captures
and body previews were off. No browser or live observer was attached during the
load stages. CPU and RSS describe the native Go process, not Docker's
container-wide accounting. Percentiles are nearest-rank; RSS was sampled every
100 ms unless stated otherwise.

## Method

| Item | Setting |
| --- | --- |
| Date | 2026-10-20 UTC |
| Source revision, footprint and ramps | `facb69900ac64c19ec3262fcb88b6428d33a68d2` |
| Source revision, sustained capacity | `54d1276` |
| Go toolchain | 1.27.2, `CGO_ENABLED=0` |
| Platform | Linux/amd64 under WSL2 |
| CPU | Intel Core Ultra 9 285H, 12 visible logical CPUs |
| Visible RAM | 62 GiB |
| Configuration | Public example with private listen and database paths |
| Debug and body previews | Off |
| Browser and live observer | None during load stages |
| Upstream | Local synthetic server on loopback |
| Load shape | Closed-loop HTTP stages, then drain |
| RSS sampling | 100 ms unless the idle sample states otherwise |
| CPU unit | 100% equals one logical CPU |
| Accounting check | Durable rows, tokens, cost and drop delta |

## Artifact and idle footprint

The image was built from the current Dockerfile. The binary was built with
`CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false`, and `-ldflags="-s -w"`.

| Artifact or state | Bytes | Display | Notes |
| --- | ---: | ---: | --- |
| Static amd64 binary | 19,611,808 | 18.7 MiB | Stripped, no CGO |
| amd64 runtime image | 35,109,551 | 33.5 MiB | Docker-reported content size |
| Compressed image archive | 9,047,520 | 8.63 MiB | `docker save` piped through `gzip -9` |
| Idle process, empty SQLite | 27,004 KiB | 26.4 MiB | After a 15-second quiet interval |
| Idle process CPU | 2 scheduler ticks | 0.067% | Two ticks over 30 seconds at 100 Hz |

The image size includes the distroless base, dependency notices, and the
embedded UI. It is not a compressed download size, a registry digest, or a
persistent-volume estimate. Idle RSS and CPU describe one process on this host.
A short sample cannot prove that a longer idle period uses exactly the same
resources.

## Fast-response throughput

Each stage used a fresh disposable database and the same synthetic response:
ten content frames, usage `100/10`, and fixture cost `0.001`. The upstream added
no intentional delay. Each stage started requests for five seconds, then let
active requests drain. The final row is a separate ten-second run used to reach
about 90,000 durable rows without a storage drop.

| Client workers | HTTP requests | Requests/s | p50 | p95 | p99 | Proxy CPU | Peak proxy RSS | Durable rows |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 4,259 | 851.5 | 0.99 ms | 2.24 ms | 3.41 ms | 42.8% | 47.8 MiB | Exact, 4,259 |
| 8 | 14,997 | 2,998.0 | 1.62 ms | 8.32 ms | 15.03 ms | 150.1% | 85.6 MiB | Exact, 14,997 |
| 32 | 35,746 | 7,147.0 | 3.66 ms | 9.61 ms | 13.66 ms | 322.9% | 98.0 MiB | Exact, 35,746 |
| 128 | 55,713 | 11,114.5 | 10.37 ms | 23.04 ms | 32.88 ms | 492.8% | 114.3 MiB | Exact, 55,713 |
| 128, 10 s | 90,620 | 9,055.1 | 12.42 ms | 29.92 ms | 42.77 ms | 404.4% | 115.0 MiB | Exact, 90,620 |

All HTTP responses succeeded in these stages, including the 128-worker stage.
That is not a promise that every configuration and history size stays exact
under overload. The durable queue is bounded and best-effort; the deliberate
overload fixture below shows what a drop result looks like.

At the ten-second 128-worker stage, the separate load generator and synthetic
upstream process used 218.8% CPU and 31.8 MiB peak RSS. The proxy figure
excludes that process.

## Sustained capacity

The sustained capacity point is the highest tested closed-loop level that
completed five minutes with zero durable drops and exact accounting. Each level
started from a fresh disposable database. The upstream response and client
shape were the same as the fast-response table: ten content frames, usage
`100/10`, and fixture cost `0.001`. A level that dropped rows is an overload
observation, not a capacity result.

| Workers | Duration | HTTP requests | Requests/s | p50 | p95 | p99 | Proxy CPU | Peak proxy RSS | Durable rows | Drops |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- | --- |
| 32 | 300.0 s | 3,745,048 | 12,483.4 | 2.12 ms | 5.13 ms | 11.49 ms | 409.9% | 99.4 MiB | Exact, 3,745,048 | 0 |
| 48 | 300.0 s | 4,249,881 | 14,166.2 | 2.84 ms | 6.96 ms | 13.14 ms | 475.8% | 113.7 MiB | Exact, 4,249,881 | 0 |
| 64 | 300.0 s | 4,138,385 | 13,794.5 | 3.89 ms | 9.66 ms | 16.85 ms | 456.9% | 117.4 MiB | 4,088,177 | 50,208 |
| 128 | 300.0 s | 3,789,537 | 12,631.5 | 8.28 ms | 23.15 ms | 40.91 ms | 415.5% | 134.7 MiB | 3,355,678 | 433,859 |

The separate load generator and synthetic upstream used 249.0% CPU and 230.9
MiB peak RSS at the 48-worker exact point, and 206.5% CPU and 186.1 MiB at the
128-worker overload point. The proxy figures exclude that process.

The exact five-minute result is 14,166.2 requests/s at 48 workers, with p95
6.96 ms and p99 13.14 ms. The 64-worker point produced 50,208 drops (1.2% of
successful requests) and the 128-worker point produced 433,859 drops (11.4%).
Those rows show that the bounded durable queue is not a promise of lossless
storage at every load. They are not evidence of a universal capacity ceiling;
host, configuration, response size, history growth, and attached observers all
change the result.

## Streaming concurrency

The synthetic upstream emitted ten content frames across approximately one
second, followed by usage and a terminator. Each stage started requests for five
seconds and then drained. Latency includes the intentional upstream delay, so
these numbers are not proxy-only overhead.

| Client workers | HTTP requests | Requests/s | p50 | p95 | p99 | First-byte p95 | Proxy CPU | Peak proxy RSS | Durable rows |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 5 | 1.0 | 1,009 ms | 1,010 ms | 1,010 ms | 2.86 ms | 0.6% | 27.9 MiB | Exact, 5 |
| 8 | 40 | 7.9 | 1,013 ms | 1,023 ms | 1,024 ms | 10.31 ms | 2.0% | 30.3 MiB | Exact, 40 |
| 32 | 160 | 31.5 | 1,011 ms | 1,017 ms | 1,028 ms | 10.62 ms | 6.5% | 35.6 MiB | Exact, 160 |
| 128 | 640 | 124.7 | 1,010 ms | 1,051 ms | 1,090 ms | 41.30 ms | 21.6% | 45.7 MiB | Exact, 640 |
| 512 | 2,560 | 490.5 | 1,014 ms | 1,114 ms | 1,137 ms | 101.64 ms | 74.1% | 99.1 MiB | Exact, 2,560 |

The 512-worker stage reached 512 upstream connections and remained exact. This
is a configured concurrency result on one host, not a universal request-capacity
ceiling. The generator and upstream used 69.9% CPU and 65.8 MiB peak RSS at that
stage.

## Durable writer and overload

These are isolated Go benchmarks from the test overlay, not HTTP runs. Every
row and drop count is verified against successful requests. The overload
benchmark deliberately fills the bounded queue, so its drop count is a fixture
result, not a recommended operating point.

| Path | Durable rows/s | Allocations |
| --- | ---: | ---: |
| Direct batch insert, 256 records per batch | 15,397 | 436,164 B/op, 7,281 allocs/op |
| Queued `Record` plus `Flush`, 8,192 records per batch | 31,608 | 13,649,079 B/op, 232,863 allocs/op |

| Overload fixture | Wire requests/s | Durable rows/s | Dropped rows |
| --- | ---: | ---: | ---: |
| 32 workers | 74,840 | 15,760 | 136,067 |
| 128 workers | 70,620 | 13,157 | 101,213 |

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
| SQLite bytes total | 6.66 MiB | 45.35 MiB |
| SQLite bytes per record | 698.4 | 475.5 |
| Projection retained, built | 449.6 B/record | 470.8 B/record |
| Projection interned dimensions | 441.9 B/record | 429.0 B/record |
| Projection scanned decode | 571.1 B/record | 566.6 B/record |
| Projection transient allocation | 2,815 B/record | 3,045 B/record |

The projection process benchmark is a relative measure inside the test binary,
not the deployed proxy's RSS. At 100,000 records it measured 93.84 MiB process
RSS and a 76.50 MiB projection delta; at 10,000 records it measured 37.61 MiB
process RSS and a 20.27 MiB delta. The test binary and its linked packages are
part of the absolute figure.

A loaded proxy run reached a 115.0 MiB peak with 90,620 durable rows while no
dashboard projection was preloaded. A full dashboard preload over an authorized
100,000-row history was not remeasured for this page. Use the projection delta
above for the retained analytical cost, not the loaded-process peak as a
substitute.

## Query and metadata paths

The query benchmarks seed a 100,000-row scratch database and decode a 200-row
page. These are single-iteration measurements on a shared host, so small
differences are not meaningful.

| Query | Before cursor | With cursor | Allocations before cursor |
| --- | ---: | ---: | ---: |
| Provider equality | 4.76 ms | 4.48 ms | 1.39 MB, 5,858 allocs |
| Model equality | 4.25 ms | 4.55 ms | 1.38 MB, 5,848 allocs |
| Client equality | 57.06 ms | 32.16 ms | 1.38 MB, 5,846 allocs |
| Key-hash equality | 47.20 ms | 26.54 ms | 23.7 KB, 269 allocs |
| Conversation equality | 60.58 ms | 36.11 ms | 23.7 KB, 268 allocs |
| Status equality | 56.06 ms | 33.68 ms | 700 KB, 3,145 allocs |

The request metadata path was measured separately. It performs one bounded
metadata decode and allocates 16 bytes per call.

| Metadata body | Time per request | Throughput | Allocations |
| ---: | ---: | ---: | ---: |
| 1 KiB | 431.9 ns | 2,558.6 MB/s | 16 B, 1 alloc |
| 100 KiB | 7,504 ns | 13,657.6 MB/s | 16 B, 1 alloc |
| 1 MiB | 108,203 ns | 9,691.5 MB/s | 16 B, 1 alloc |

The 1 MiB case is about 108 microseconds on this host. It is a metadata decode
measurement, not a promise that every 1 MiB request body is fully processed in
that time.

## Reproduce

`scripts/perf.sh` owns the repeatable local run: `bench`, `stress`, `image`, or
`all`. It uses a fresh private `scripts/dev.sh` instance for each stress mode and
stops it through the script's own lifecycle owner.

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

Use a separate fresh instance for the streaming ramp:

```sh
(
  set -e
  trap 'scripts/dev.sh stop' EXIT
  export MILLIVOLT_OPERATOR_TOKEN='dev-instance-credential'
  DEV_COPY_DB=0 scripts/dev.sh
  go run ./cmd/stress -target http://127.0.0.1:8081 \
    -concurrency 1,8,32,128,512 -duration 5s -stream-duration 1s
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
scripts/check.sh go test -run='^$' -bench='Benchmark(QueryBeforeEquality|QueryBeforeCount)' \
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

Idle RSS and CPU were sampled from `/proc/<pid>/status` and
`/proc/<pid>/stat` after a 15-second settle. The process was started and stopped
only through `scripts/dev.sh`.

## Limitations

- The host was shared. Other workloads can change absolute rates and latency.
- The upstream was a local synthetic server. Provider latency, TLS, retries,
  payload shape, and real identity cardinality are not represented.
- These are short closed-loop bursts, not sustained production capacity. No
  result here is a capacity ceiling or a guarantee.
- Durable accounting is best-effort. The fast stages happened to remain exact;
  the overload benchmark intentionally does not.
- Container-wide CPU/RSS, cold-start time, browser rendering, arm64 image size,
  disk throughput, and a full dashboard preload over a large authorized history
  were not measured for this page.
- The image archive is a local artifact, not a published registry digest.
- The source revision is the runtime tree before the later documentation, site,
  test, and login-page color commits in the same change set. Those commits do
  not change the request accounting or storage paths measured here.
- The current numbers supersede the older v0.1.0 example. Do not compare them
  directly across differing hosts, workloads, or revisions.
