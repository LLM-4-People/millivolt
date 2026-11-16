# Operations

> [!WARNING]
> Read the [security boundary](../SECURITY.md) before exposing the listener.

The whole embedded dashboard is protected: every dashboard, `/metrics/*`,
`/admin/*` and `/mcp` request requires the `MILLIVOLT_OPERATOR_TOKEN` credential
and is denied while it is not configured. At the application gate, only the
unauthenticated `/healthz` liveness and `/readyz` readiness probes, the
`POST /session` login handshake, origin-root brand/PWA files and transparent
inference stay open; see the canonical [operator access](routes.md#operator-access)
list.

## Overview

This page is the operating overview. It covers health and readiness, durable
storage and accounting, the runtime cost model, and the known limits. Each
task-shaped page carries the detail.

| If you want to | Start with |
| --- | --- |
| Change a setting, an override or a provider alias | [Configuration](configuration.md) |
| Install, back up, upgrade or restart the deployment | [Containers and data](containers.md) |
| Call an operator route, or debug traffic | [Routes and debugging](routes.md) |
| Protect the listener | [Security](../SECURITY.md), [reverse proxy](reverse-proxy.md) |
| Measure the deployment | [Performance and resource usage](performance.md) |

## Health and readiness

Liveness (`/healthz`) answers "the process is up and serving HTTP". Readiness
(`/readyz`) additionally answers "this instance can persist": it fails with
`503` and a reason when the durable writer is wedged.

Two arms answer that: the
oldest record on the queue has waited, or no flush has succeeded, for longer
than one flush interval plus a progress window. The window is four flush
intervals clamped to at least 30 seconds and at most two minutes; the record's
own interval is allowed on top, so a record inside its own cadence never
convicts and a long configured interval cannot postpone the verdict by more
than that one cadence.

Every clock starts on arrival: the oldest record on the
queue ages the first arm, and while no flush has succeeded since boot the first
accepted record ages the second one, so a writer that never picks records up is
convicted all the same. Readiness also fails when
any durable flush has failed since boot.

Readiness stays red after the first failed flush even once writes recover,
because a lost batch is worth an operator's attention; restart or investigate.
With durable storage disabled both routes answer `200`, since there is no
pipeline to wedge.

The Compose files use `-readycheck` as the service healthcheck, so
`docker compose ps` reports `healthy` only for an instance that can persist.
Point an external load balancer or orchestrator at `/readyz` for the same
signal; both probe routes are unauthenticated and answer the exact body
`{"ok":true}` on success.

Storage state is also scrapeable at `/metrics/prometheus` (behind the operator
credential) as `llm_proxy_storage_enabled`,
`llm_proxy_storage_queue_depth`, `llm_proxy_storage_queue_capacity`,
`llm_proxy_storage_dropped_total{cause}` (`overflow`, `after_close`, `panic`,
`flush_failed`), `llm_proxy_storage_flush_failed_total`,
`llm_proxy_storage_last_flush_timestamp_seconds` and
`llm_proxy_storage_wedged`. The block is rendered on every scrape, outside the
memoized request aggregate, so queue and drop movement shows up immediately.
Alert on `llm_proxy_storage_wedged`, on a rising `flush_failed` counter, and on
`overflow` drops that are not explained by a burst.

## Storage and accounting

SQLite retains finalized records; the ring and pending registry support live
observation. Full bootstrap windows are capped in durable mode and older log
rows page from storage. Without storage, the ring is the available history.

The durable log order owns one secondary index, `idx_requests_log(started_at,
id)`. The log page pushes exact provider, client, key-hash, conversation and
canonical numeric status predicates into SQL and rechecks them on that scan;
model, status-class, tool, error and time-bucket filters stay Go-owned because
they match canonical/derived values, not raw stored text.

The export and purge
filter surface is different: it accepts exact provider, model, client,
conversation, error type, status code, `has_error`, `debug`, and time bounds,
with no key-hash selector, and its equality-only preview count or purge scans
the table instead of maintaining per-dimension composite indexes.

`BenchmarkStoreDiskBytes` measures
this on its deterministic synthetic fixture (10,000 and 100,000 seeded
records). The current per-record and total bytes are published in
[storage and retained projection](performance.md#storage-and-retained-projection);
that is a reproducible fixture scope, not a prediction of a deployment's disk
use or a capacity ceiling.

Dropping a superseded request-log index only moves its pages to SQLite's
freelist, so the first startup that finds and removes one also runs a one-time
`VACUUM` after the migration. Fresh and already-migrated databases skip it.

A
`VACUUM` needs free temporary space up to roughly twice the database size; if
it fails for space, locking or interruption, startup logs a warning and
continues with every row intact, but the freed pages stay on the freelist.

Reclaim them manually during a maintenance window: stop the proxy, run
`sqlite3 /data/proxy.db 'VACUUM;'` (or any SQLite client) against `db_path`,
and confirm `PRAGMA freelist_count` returns 0. The automatic pass is never
retried on a later boot, so a failed attempt needs this manual step.

Boot migrations also own a small set of one-shot data repairs for shapes older
binaries could write but current writers never produce: an empty-string JSON
document in `attempts` or `tool_names` is rewritten to the canonical `null`,
and a positive `ttft_ms` with `final_attempt_at = 0` (the pre-0.3.34
start-anchored TTFT, recorded with upload, queueing and retry backoff
included) is reset to 0 because the original per-attempt anchor was never
stored and cannot be recovered.

Each repair has its own `meta` marker
(`attempts_repaired`, `tool_names_repaired`, `ttft_anchor_repaired`): the
marker is written after a clean probe or after the rewrite, later boots read
one row instead of rescanning the table, and deleting the marker (or restoring
a database that predates it) re-arms the probe.

A repair that cannot run fails
boot closed instead of serving history its own consumers would reject.

The
first boot after an upgrade that finds rows to rewrite may pause while it
scans and rewrites them, and the totals rebuild follows: the rewrite advances
the projection epoch, which invalidates the persisted totals document. The
rewrite can transiently grow the WAL, so keep free space on the database
volume as for any other large write.

Enqueue is bounded and nonblocking. Overflow or write failures can lose durable
records even when HTTP succeeds. Bootstrap's
`storage:{enabled,dropped,totals_degraded}` reports cumulative process-local
loss and a failed startup history scan. Either appears in the status footer;
degraded since-inception totals count only rows committed since process start.

Increasing queue size absorbs bursts, not arbitrary sustained overload.
Storage overload remains an unresolved capacity boundary, not a lossless mode.
Measure it with the [isolated stress workflow](../CONTRIBUTING.md#performance-evidence):
successful responses, exact durable accounting and process-local drops are
separate acceptance criteria. A short successful burst does not establish
sustainable writer throughput or an absolute request-capacity ceiling.

`storage_query_timeout` bounds interactive dashboard reads (chart, explorer,
log/bootstrap, capture reads) and the ad-hoc query route. A capture read waits
first for the in-flight sidecar save of the same request, bounded by the
drawer client's own request context, and only then starts the read budget, so a
slow save cannot spend the budget the read needs.

Startup projection
preload, the boot totals rebuild, an administrative purge, and a backup merge's
totals scan run under a separate internal ten-minute maintenance budget: they
legitimately read or rewrite full history and must not inherit the interactive
bound.

The maintenance budget still honors an earlier caller cancellation or deadline,
and its expiry fails the operation closed (a purge transaction rolls back). It is a
fixed implementation guardrail, not a configuration field.

One shared analytical projection retains compact data and sorted metric orders
proportional to stored history. Initial preload costs startup CPU/RAM; fast
dashboard reuse is not free startup or bounded total-history memory.
Concurrent independent proxy writers sharing a database are not a supported
coherent-KPI deployment.

Expired debug sidecars are hidden from reads immediately by their TTL. Physical
reclamation deletes bounded batches per writer tick and continues on a
near-term tick, so a large expired set drains without holding the sole writer
connection; the sidecar byte cap accounting is updated exactly for the rows
removed.

Scheduler groups and some observed-identity sets also retain historical labels.
Bounded request queues do not bound all process memory; long-running workloads
with many unique identities need their own memory/cardinality measurements.

From a source checkout with Python installed, use the existing online backup
into a new destination:

```sh
python3 scripts/backup_db.py proxy.db /path/to/new-backup.db
```

The helper opens the source read-only and includes committed WAL state. Do not
plain-copy an open database or omit its WAL. Protect backups, debug captures and
exports as sensitive data.
This helper backs up the database, not configuration. Operators can also
download a self-checked config and/or database archive from
[Settings backup and restore](containers.md#settings-backup-and-restore). For image-only
deployments and both persistent volumes, use
[container backup and restore](containers.md#image-only-backup-and-restore).

## Performance and footprint

Separate deployment size, cold startup, idle footprint, request overhead and
loaded throughput. They answer different questions; a small image is not a RAM
limit, and a cached dashboard read is not a cold-start measurement.

The runtime contains a stripped Go binary with embedded UI assets, dependency
notices and the generated configuration, on a minimal non-root base. There is
no frontend build/runtime dependency in deployment. Docker's reported image
size describes an image artifact, not compressed network transfer, persistent
volume growth or the process's resident memory.

On the request path, one bounded body read/metadata decode feeds shared routing
and accounting. Upstream connections are pooled; streamed observation and durable
enqueue avoid a per-request history scan or synchronous SQLite write. On the
dashboard path, the initial HTML embeds bootstrap state and shared analytical
projections/cache slots avoid building a separate history copy for each view.
These implementation choices reduce repeated work, not every cost of a request.

RAM still includes active request/response buffers, connection pools, the live
ring and pending state, and the history/cardinality costs documented under
[storage](#storage-and-accounting). Raising queue/body/pool limits or concurrency
changes that footprint. Retained history and observed identities have no universal
process-memory cap; configuration limits must be evaluated together.

The startup preload decodes every stored row once, so its transient allocations
(SQL strings, attempts and tool-name JSON) exceed the retained projection several
times over. The process returns those burst pages to the OS immediately after the
preload completes; idle RSS after readiness reflects retained state, not the
startup burst.

The projection's per-record retained footprint is measured
reproducibly by `BenchmarkProjectionFootprint` (retained and transient bytes per
record over a seeded scratch database) and `BenchmarkProjectionProcessRSS`
(process RSS after readiness), without a live instance; the
[performance workflow](../CONTRIBUTING.md#performance-evidence) owns the commands.

`go_memory_limit` is the operator's steady-state RSS lever: a soft cap on Go
runtime managed memory (heap, stacks, runtime structures) applied once at
startup.

With the setting at 0, an operator `GOMEMLIMIT` wins and otherwise a
detected finite cgroup memory ceiling of at least 80MiB (v2 `memory.max` or v1
`memory.limit_in_bytes`) becomes a soft limit at 80% of that ceiling so a
container's own bound actually reaches the Go runtime; smaller ceilings are
ignored and, with no `GOMEMLIMIT`, the runtime stays unbounded.

A set value
overrides `GOMEMLIMIT`, not `GOGC`, for the process lifetime. `GOGC` remains
independent.

The limit is soft - exceeding it makes the collector
more aggressive (capped near 50% of CPU time) instead of failing requests - and
bounds Go-managed memory only, never SQLite's page cache or OS overhead.

The [performance and resource usage](performance.md) page is the single source
for the current measured tables, hardware, method, reproduce commands and
limitations. This section owns the implementation cost model above and the
`go_memory_limit` behavior. Use the [isolated performance workflow](../CONTRIBUTING.md#performance-evidence)
when adding evidence. Short local-upstream bursts and microbenchmarks are useful
regressions, not sustained production capacity, provider-latency or lossless
storage guarantees.

## Known limits

- Recognized provider fields drive usage/cost; this is not independent billing.
- Non-streaming usage/cost inspection keeps a bounded prefix, so larger valid
  responses can forward completely without complete accounting.
- Optional non-streaming translation reads the complete upstream 200 body and
  the translated document under the configured `translated_response_max_bytes`
  each (default 32 MiB). Exceeding either fails closed with 502
  `response_too_large` before any client bytes; discovery budgets do not
  bound the path.
- General multi-line SSE accounting/native event assembly remains incomplete;
  some split terminal/content shapes can affect classification, not just metrics.
- SQL result limits do not bound every intermediate SQLite allocation.
- Automatically grouped sessions are not proof of agent ancestry. Explicit parent
  declarations and exact-leaf filtering are described in [protocol](protocol.md).
- Existing values lost by older storage formats cannot be reconstructed.
- Extreme numeric data can produce explicit accounting/encoding errors rather
  than invented saturated prices or token counts.
