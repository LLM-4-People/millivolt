# Architecture and ownership

millivolt is one Go HTTP service with an embedded, no-build dashboard.
[AGENTS.md](../AGENTS.md) contains mandatory change rules;
[protocol](protocol.md) and [operations](operations.md) describe public behavior.

## Source map

| Owner | Responsibility |
| --- | --- |
| [cmd/proxy](../cmd/proxy/main.go) | Config/CLI, dependency wiring, routes, process lifecycle. Restart and log handlers have separate existing files. |
| [internal/config](../internal/config/config.go) | Config/defaults/validation; schema, YAML generation, revision-checked Settings, model-rule execution. |
| [internal/proxy](../internal/proxy/proxy.go) | Routing, admission integration, retry/relay, request metadata, native-run ownership, operator state/capture. |
| [internal/scheduler](../internal/scheduler/scheduler.go) | Provider/key admission, retry pacing, scoped holds and provider-wide budgets. |
| [internal/metrics](../internal/metrics/metrics.go) | Records, numeric/usage/outcome semantics, pending/ring lifecycle, observers and Prometheus. |
| [internal/sse](../internal/sse/analyzer.go) | Streaming content/usage/TTFT inspection. |
| [internal/format](../internal/format) | Explicit native wire translation and Connect/protobuf framing. |
| [internal/storage](../internal/storage/store.go) | Asynchronous SQLite writing, schema/read fidelity, totals, purge fence and bounded queries. |
| [internal/web](../internal/web/aggregate.go) | Embedded shell/assets, bootstrap, canonical history projection and chart/explorer/log aggregates. |

## Request and scheduling path

`readRequest` retains bounded original bytes and calls the single metadata
decoder. Ordinary content is measured without retaining decoded prompt/schema
trees; opt-in previews stay bounded. Exceptional decoder compatibility belongs
at that owner, not a second routing parser. The Start stamp follows body upload
and precedes metadata/translation CPU.

Immutable config/client snapshots hot-swap for new work; old transports finish
active requests. Admission is provider+key scoped, with provider-wide token,
request and concurrency limits layered at the same grant owner. Acquisition
release settles exactly once. Policy generations prevent clearing/replacing a
cap from charging another generation's reservation.

Retries, provider hints, hold waits and per-send deadlines have distinct owners.
Capture context error before canceling it for cleanup. Never retry a canceled
caller or replay meaningful emitted stream content. Quality failure handling is
explicitly exceptional; see [adapters](adapters.md) and the relay tests.

Important coverage includes request metadata/parser fuzz tests,
`BenchmarkRequestMetadata`, `BenchmarkProxyRequest`,
`TestConcurrentStreamingAccounting`, transport lifecycle tests and scheduler
efficiency/race tests. Proxy requests must not perform history scans or wait for
SQLite accounting.

## Finalized records, pending lifecycle and replay

`PublishLive(begin/update)` snapshots pending state without adding durable rows.
`Buffer.Record` atomically finalizes the row, retires pending state, updates its
gauge/revision and emits end. Exactly one finalized record per request is the
accounting unit; proxy-owned IDs cannot collide with reused client request IDs.

Existing lock order keeps sequence assignment and finalized publication ordered.
A feed epoch changes on restart/deletion; queued fanout retains its mutation
epoch. Subscribers attach before snapshot capture and overflow retires that
subscriber so reconnect can replay, rather than silently dropping permanent rows.

`SnapshotRequest` is the shared atomic cursor/feed gate for bootstrap and SSE.
A foreign/stale cursor requires a full snapshot; deltas are never truncated.
Snapshots include current pending rows and their revision. Full durable-mode
snapshots cap the recent window, with archives fetched separately.

The browser's one upsert owner deduplicates finalized replays. Pending upserts
cannot overwrite finalized rows or a newer pending revision of the same ID.
The authoritative gauge revision is independent: an older event may still add
a missing different row without rolling back the gauge. Full bootstrap closes
the stream before capture; only the winning refresh reopens it. Stale same-feed
full SSE snapshots recapture through that barrier by sequence/pending revision.

See buffer snapshot/stream tests and `ui_check.js` lifecycle/bootstrap races.
Do not collapse these row, gauge, epoch and request-ownership gates into a
whole-event drop rule.

## Durable writes and deletion

`Store.Recorder(buffer)` publishes the live final record and nonblocking enqueue
under one short producer fence. SQLite is the durable authority; enqueue overflow
and failed batches remain visible best-effort losses.

The one writer owns transactions and incremental totals. Its insertion plan
binds arbitrary values, using exact zero/empty literals only for absent scalar
work, and preserves existing conflict/rollback semantics. Record columns,
migration, scanner and request-parameter presence bits must evolve together;
explicit zero/false is not absence.

`Store.Clear` serializes administrative purges, captures an accepted-record
boundary and publishes one command. The writer flushes earlier records, deletes
matching records/sidecars and rebuilds totals transactionally, then removes only
matching ring rows through that boundary. New completions survive. Failed SQL
does not mutate the ring; postcommit memory effects finish even after caller
cancellation. The atomic command check must not acquire the producer lock for
every ordinary consumed record.

Relevant coverage: storage writer/purge/request-parameter tests,
`BenchmarkStorageWriter` and `BenchmarkProxyDurableContention`. The latter
deliberately overloads storage and separates durable throughput from drops.

## History projection and aggregate math

One durable analytical projection retains compact contributions, request IDs,
interned dimensions and immutable sorted metric row orders. It is not a second
database or a per-view history cache. Startup preload and incremental catch-up
share its builder. SQLite transactions capture rowid high-water/destructive
epoch/schema authority; destructive changes rebuild. Failed or canceled reads
never publish stale rows or advance authority.

Chart and explorer each retain one immutable last-result slot. Keys include
validated view/scope, canonical model rules, finalized/database revisions and,
for explorer, pending revisions. Clock-aligned geometry also gates chart reuse.
SQLite data_version is read on one retained connection; unsupported external
writers are not made safe merely by cache invalidation.

Explorer uses dictionary IDs, scoped predicates and bitsets. Projected metric
samples stay in shared sorted orders: per-row membership spans preserve
multi-valued occurrences, selected groups use the same exact rank reader as
chart buckets, and only top cards derive percentile/spark payloads. Pending
dedupe retains only relevant pending IDs, not another history-sized seen set.

Both consumers share R7 rank/interpolation and the minimum-sample gate. Period
speed/TTFT percentiles use period samples, never means of bucket percentiles.
Prometheus and the stress harness instead share `metrics.Percentile`'s
nearest-rank contract; do not conflate these two intentionally distinct APIs.
Checked counts/sums/ratios prevent wrapped tokens or non-finite JSON. Cost stays
USD in storage/API; missing/unreportable data must not become fabricated prices.

`Record.IsError` and `HasRateLimit` deliberately differ. Final/retried 429
counts once per affected request, not via the broad pacing flag. A real failure
and 429 may overlap. The shared first-membership gate keeps duplicate tool/error
occurrences from duplicating these health counts without changing event/sample
multiplicity. Tests cover raw/projected/durable/ring/pending parity.

See `aggregate_projection*_test.go`, `aggregate_dimensions_test.go`,
`aggregate_percentile*_test.go`, `aggregate_explorer_fold_test.go`,
`aggregate_rate_limit_test.go` and `conversation_lineage_test.go`.

## Canonical observer meaning and conversation lineage

Go's compiled model-rule executor owns canonical grouping. Observer payloads
carry exact raw-to-canonical names and a rules-content revision; only full
authoritative snapshots install a new revision. Deltas, archive and Debug names
merge under that revision, with recapture on conflicts. Browser dictionaries
prune to loaded rows/selector needs. RE2 patterns do not execute in JavaScript.

Observer records carry server-local `time_bucket`; browser-local dates are
presentation only. Raw names remain raw for exact export/delete semantics.

The explorer's one iterative lineage resolver observes declarations by
client/key/conversation before applying rail/gallery scopes. Parent-only
dictionary names do not become observed memberships. Conflicts, cycles and
mixed namespaces remain unknown; missing parents are not invented roots.
Summary counts follow full-filter rail scope; conversation cards follow the
gallery cross-filter. Payloads retain small resolved fields, not the temporary
graph. Parent pivots remain exact namespace-qualified leaves.

## Dashboard render owners

HTML embeds the canonical bootstrap builder's JSON, safely escaped in an inert
data block. Deferred external assets download alongside HTML. Saved hash/chart
choices apply before rendering; invalid/missing seed falls back to the endpoint.
Dynamic HTML/state is private/no-store; immutable asset representations and
revalidation identities are precomputed. Content version changes use the same
winning bootstrap gate to reload once.

Vanilla modules under [static/js](../internal/web/static/js) have distinct owners:
`core` formatters/shared state, `entities` identities/record labels, `explorer`
routing/cards, `chart` uPlot plans, `log` window/drawer, `chrome` operator UI,
and `live` bootstrap/SSE/derived state.

One memoized derivation serves scoped rows and shared readouts. O(1) ID upserts,
pure-replay suppression and rAF-coalesced live rendering avoid repeated scans.
Incremental log groups preserve scroll/selection; archive paging uses paired
timestamp/ID cursors and stays outside the live arrival-ordered ring.

Server chart responses alone own buckets/totals at configured cadence. Presets,
series, money/unit formatters and visible dimension order each have one registry.
Hidden lines keep stable columns; percentile/range changes reuse the plot.
Isolated samples use filled/ring points without coordinate jitter. Shared axis
formatting avoids duplicate labels. The percentile dropdown is the only visible
pXX label owner; conditional error/429 badges omit zero independently.

Operators use shared mutation/revision gates, lossless Settings collection,
stable event-path outside-click handling and a focus/inert dialog lifecycle.
CSS owns responsive breakpoints. Preserve accessibility, saved state and cheap
boot; jsdom tests do not replace real desktop/mobile canvas checks.
