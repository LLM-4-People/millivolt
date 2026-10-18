# Architecture and ownership

millivolt is one Go HTTP service with an embedded, no-build dashboard.
The client-facing OpenAI-compatible interface and upstream relay/translation
are separate boundaries; the [compatibility matrix](adapters.md#client-and-upstream-compatibility)
defines their supported combinations.
[Contributing](../CONTRIBUTING.md) covers change and verification rules;
[protocol](protocol.md) and [operations](operations.md) describe public behavior.

## Source map

| Owner | Responsibility |
| --- | --- |
| [cmd/proxy](../cmd/proxy/main.go) | Config/CLI, dependency wiring, routes, process lifecycle. Restart, log, backup and operator-gate handlers have separate existing files. |
| [internal/config](../internal/config/config.go) | Config/defaults/validation; schema, YAML generation, revision-checked Settings, model-rule execution. |
| [internal/backup](../internal/backup/archive.go) | Streamed self-checked operator archive format (VACUUM INTO snapshot + zstd + SHA-256), with byte- and file-backed database members. One decoder core serves byte and file callers; restore stages its upload and decoded database as private files instead of heap copies. |
| [internal/proxy](../internal/proxy/proxy.go) | Routing, admission integration, retry/relay, request metadata, scoped request overrides, native-run ownership, operator state/capture; the quota-pause reaction owner [quotapause.go](../internal/proxy/quotapause.go) (settleQuotaFailure/addQuotaHold) and GET/POST /admin/quota. |
| [internal/scheduler](../internal/scheduler/scheduler.go) | Provider/key admission, retry pacing, scoped holds and provider-wide budgets; the provider-wide durable quota/billing recovery gate (quota facet, probe-paced recovery). |
| [internal/metrics](../internal/metrics/metrics.go) | Records, numeric/usage/outcome semantics, pending/ring lifecycle, observers and Prometheus. |
| [internal/sse](../internal/sse/analyzer.go) | Streaming content/usage/TTFT inspection; client-facing OpenAI SSE frames and completion envelopes. |
| [internal/format](../internal/format) | Explicit native wire translation and Connect/protobuf framing. |
| [internal/storage](../internal/storage/store.go) | Asynchronous SQLite writing, schema/read fidelity, totals, purge fence, packed snapshots and bounded queries. |
| [internal/web](../internal/web/aggregate.go) | Embedded shell/assets, bootstrap, canonical history projection and chart/explorer/log aggregates. |
| [internal/mcp](../internal/mcp) | The MCP tool server for the operator plane: one HTTP client owner (credential, request building, error extraction), one scope-filter owner, one tool registry served by the proxy's streamable HTTP `/mcp` endpoint, the destructive-call guards, and the streamable HTTP handler the proxy mounts at `/mcp`. It owns no analytical projection of its own. |

## Request and scheduling path

`readRequest` retains bounded original bytes and calls the single metadata
decoder. Ordinary content is measured without retaining decoded prompt/schema
trees; opt-in previews stay bounded. Exceptional decoder compatibility belongs
at that owner, not a second routing parser. The Start stamp follows body upload
and precedes metadata/translation CPU.

`resolveRequestOverrides` in
[request.go](../internal/proxy/request.go) merges the matching
`request_overrides` rules once per request, after routing metadata is known,
and its merge has exactly two application stages: the final header stage of
`buildUpstreamRequest`, and the cursor identity headers at the native send. Its
body stamp is fail closed (an undecodable or oversize result relays the
original bytes with one log line), and a nil resolution - the feature off or
nothing matched - leaves the request path byte-identical. The sub-conversation
strip is the single body-rewrite engine's second consumer: it fires
independently of override resolution, after the tracked value is extracted
from the buffered body and before translation, with
[conversation.go](../internal/proxy/conversation.go) owning the extraction and
the `k:` identity. A provider's ensured tool names are the same engine's
third consumer: they are restricted to the OpenAI wire, because the injected
stub tools are chat/completions-shaped and must never reach a
translated-anthropic or cursor body, and they fire with any override and
strip actions through one document decode.

Immutable config/client snapshots hot-swap for new work; old transports finish
active requests. Admission is provider+key scoped, with provider-wide token,
request and concurrency limits layered at the same grant owner. Acquisition
release settles exactly once. The provider gate and token-bucket epoch captured
at admission keep clearing/replacing a cap from charging another generation's
reservation. A provider-to-groups index in `Scheduler` records each group on
its first attributed acquire and counts the indexed groups with a live queue.
A provider release reads that counter and the provider's slice header under the
scheduler mutex, releases it, returns without touching any group when the
counter is zero, and otherwise skips groups whose per-group queued witness is
false before draining the rest. Unrelated groups are neither scanned nor
locked.

[tokenrefresh.go](../internal/proxy/tokenrefresh.go) separates provider exchanges
from response policy. Exchange adapters return credential data only; the shared
`resolveKey` ends successful inference refreshes with handback before admission,
live publication or recording. `resolveKeyTransparent` owns the model-discovery
exception. The token-refresh tests cover both mechanisms, adoption/retry, absent
replacement tokens, applicability/failure gates and bounded exchange responses.

Retries, provider hints, hold waits and per-send deadlines have distinct owners.
Capture context error before canceling it for cleanup. Never retry a canceled
caller or replay meaningful emitted stream content. Quality failure handling is
explicitly exceptional; see [adapters](adapters.md) and the relay tests.

[scheduler/storm.go](../internal/scheduler/storm.go) owns bounded rolling
attempt counters, active-model evidence, cross-key provider/model gates and
exclusive recovery permits. Provider activation requires all models with
activity inside the same window to independently qualify. Old permits cannot
recover newer incidents. Initial admission waits for readiness without reserving
a probe; only the actual send boundary claims a probe after key admission.
Storm policy changes reset observations and wake waiters without mutating
retained history. Each
request carries an observation for its provider and model, moving distinct
failed-request membership between fixed buckets across retries without retaining
a request-ID map. No storm observation scans metrics history or waits for SQLite.

[proxy/storm.go](../internal/proxy/storm.go) maps config once into scheduler
options and bridges existing relay/native send outcomes. A 2xx permit settles
after relay or before an existing quality re-ask or thinking rescue re-send,
preventing double samples and stream replay. Safe protocol-level reason labels
carry no provider error bodies.
The banner uses the canonical bootstrap builder and browser apply/render gates,
with no client-side event arithmetic. Storm scheduler/proxy/config regressions
and the shared UI/browser suite cover these boundaries.

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
A debug capture registers its pending sidecar marker in `debugRuntime` before
that publish and encodes/saves after it, so the sidecar write delays neither
the live end nor durable accounting. A drawer fetch for the id waits on the
marker, then reads the authoritative store; a resolved marker without a row
keeps the normal 404.

`Buffer.Record`, `PublishLive` and `Backfill` share the one record-retention
pass ([internal/metrics/retain.go](../internal/metrics/retain.go)) at that
boundary. It bounds client-controlled paths/methods and provider-controlled
model/request/finish/error text, tool names and attempt metadata; normal values
stay byte-identical. The bounds and the UTF-8 cut marker are documented in the
[protocol](protocol.md).

Existing lock order keeps sequence assignment and finalized publication ordered.
A feed epoch changes on restart/deletion; queued fanout retains its mutation
epoch. Subscribers attach before snapshot capture and overflow retires that
subscriber so reconnect can replay, rather than silently dropping permanent rows.
When more than one dashboard subscriber is attached, `Record` and `PublishLive`
attach one lazy observed-JSON cache to that published event
([internal/metrics/event_cache.go](../internal/metrics/event_cache.go)). The
first consuming stream encodes the observer result and every subscriber of that
same event reuses the retained bytes; an encode error is retained too, so all of
them close and resync. Zero or one subscriber keeps the direct `json.Marshal`
path with no cache allocation, and no cache outlives its event or crosses a
feed epoch.

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

Tests live under [tests/](../tests/README.md), with Go package mirrors in
`tests/_go/`. Use the overlay-aware `scripts/check.sh go test` entry point.
See buffer snapshot/stream tests and `tests/ui_check.js` lifecycle/bootstrap races.
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

The durable log order is `(started_at, id)`. Every exact `LogFilter` equality
field - provider, client, key hash, conversation id, model and status code -
has an ordered composite index ending in that key, so a filtered log page,
count, export or purge scans one index range in cursor order instead of sorting
or scanning its match set. SQLite uses one index per query and rechecks any
remaining equality predicates against each row. Canonical model rules and
status classes still match in Go.

`Store.Clear` serializes administrative purges, captures an accepted-record
boundary and publishes one command. The writer flushes earlier records, deletes
matching records/sidecars and rebuilds totals transactionally, then removes only
matching ring rows through that boundary. New completions survive. Failed SQL
does not mutate the ring; postcommit memory effects finish even after caller
cancellation. The atomic command check must not acquire the producer lock for
every ordinary consumed record.

Operator backup and restore stage database-sized work as private files.
`backup.EncodeFile` streams the packed snapshot, encoded archive and self-check
in bounded chunks. On restore, the handler streams the capped upload into one
private file, and `backup.DecodeFile` shares the byte decoder's header, zstd,
SHA-256 and member-framing owner while writing the database member into a
unique staging directory on the live database's volume. Config bytes remain
buffered; the database member does not. Inspect and overlap integrity-check or
attach that file in place, replace streams it into the pending snapshot and
atomically renames it, and merge attaches it to the held writer transaction.
Decode and validation finish before an apply path takes the writer connection.
The upload and decoded database files are removed on every handler path,
including over-cap, invalid archive, apply failure and client disconnect.

Relevant coverage: storage writer/purge/request-parameter tests,
`BenchmarkStorageWriter` and `BenchmarkProxyDurableContention`. The latter
deliberately overloads storage and separates durable throughput from drops.

Non-stream responses decode once into one analysis: quality outcome, usage,
model/id, finish reason, tool names, bounded preview and the top-level error
envelope share that pass, and the relay applies the same value at commit.
Cost-bearing bodies add the existing provider-keyed cost pass; streaming
analyzer semantics are unchanged.

## History projection and aggregate math

One durable analytical projection retains compact contributions, request IDs,
interned dimensions and immutable sorted metric row orders. It is not a second
database or a per-view history cache. Startup preload and incremental catch-up
share its builder. SQLite transactions capture rowid high-water/destructive
epoch/schema authority; destructive changes rebuild. Failed or canceled reads
never publish stale rows or advance authority.

Materialization runs off the projection lock with one single-flight builder;
concurrent readers keep serving the previous complete snapshot and wait for
that builder instead of repeating its scan. A destructive change builds a
complete replacement off-lock and swaps it under a short write lock. The
durable since-inception totals are one meta row written in the same
transaction as every insert, purge and merge on the held writer connection;
its recorded projection epoch is the mutation witness. A merge writes its
request rows, debug sidecars and totals document in one transaction, so a
failed totals write rolls the whole merge back instead of advancing the epoch
over partial rows. An unsupported external writer that advances the epoch
leaves the pair mismatched, so the next boot rebuilds rather than trusting it.
A boot with a changed epoch rebuilds once from the rows
instead of trusting a stale cache, and a full purge has no surviving rows to
scan. Restart readiness therefore no longer scales with the sum of a totals
scan plus a separate preload scan.

Chart and explorer each retain one immutable last-result slot. Keys include
validated view/scope, canonical model rules, finalized/database revisions and,
for explorer, pending revisions. Clock-aligned geometry also gates chart reuse.
One in-flight fold per memo is shared by concurrent same-key callers: a
different key never waits, and a waiter that finds no published entry after the
leader ends recomputes as the next leader instead of inheriting its error.
The bootstrap KPI fold retains one exact totals slot keyed by the ring's
finalized revision and the store's durable accounting revision; live counters
are filled at serve time and the ring snapshot itself is never retained.
SQLite data_version is read on one retained connection; unsupported external
writers are not made safe merely by cache invalidation. Log pages push exact
provider/client/key/conversation/status equalities into SQLite before
decoding, plus raw model equality when no canonical rules rewrite names. The
Go scope matcher remains authoritative for canonical models, status classes,
errors, tools, time buckets and live-only classes. Ad-hoc SELECT results are
encoded once during the bounded scan and the endpoint writes that array
directly; row/byte cap failure still precedes every response byte.

Explorer uses dictionary IDs, scoped predicates and bitsets. Projected metric
samples stay in shared sorted orders: single-valued dimensions retain one group
id per row, while tool/error membership spans preserve multi-valued
occurrences. Selected groups use the same exact rank reader as chart buckets,
and only top cards derive percentile payloads. Pending dedupe retains only
relevant pending IDs, not another history-sized seen set. Chart's rank reader
also produces the occurrence-weighted avg/min/max in the same traversal, so a
metric order is no longer walked separately for stats. The all-history order is
a compacted immutable base plus a bounded value-sorted pending delta (16,384
samples): a catch-up append folds only its new samples, so it stays
O(delta log delta) regardless of history, and reaching the bound folds
base+pending once. Chart and explorer read the exact merged order through one
view owner, which caches the merge per published snapshot and never mutates
either input. A full rebuild replaces the base and clears pending. A retained
last-65,536-row order serves bounded chart windows whose earliest in-window
durable row is inside that tail. It uses the same compacted base plus bounded
pending delta design and its own per-snapshot view holder, so a sub-threshold
catch-up merges only its new batch instead of copying the tail; a rebuild of
the newest 65,536 rows replaces its base, clears the pending delta and releases
samples that fell out of the tail. Older or unscoped windows fall back to the
all-history order, so the fast path never changes rank or stat results.

Both consumers share R7 rank/interpolation and the minimum-sample gate. Period
speed/TTFT percentiles use period samples, never means of bucket percentiles.
Prometheus and the stress harness instead share `metrics.Percentile`'s
nearest-rank contract; do not conflate these two intentionally distinct APIs.
Checked counts/sums/ratios prevent wrapped tokens or non-finite JSON. Cost stays
USD in storage/API; missing/unreportable data must not become fabricated prices.

`Record.IsError` and `HasRateLimit` deliberately differ. Final/retried 429
counts once per affected request, not via the broad pacing flag. A real failure
and 429 may overlap. `metrics.IsNonRetryableQuotaErr` is the fixed durable
quota/billing classification vocabulary: the retry ladder, storm evidence and
in-band precedence all consult it, so the account condition never counts as a
rate limit. A plain 499 (the local client's own abort) is a flow
event; a 499 carrying a structured error observed the upstream's in-band
failure before the client left and counts like its 200 twin. The shared
first-membership gate keeps duplicate tool/error
occurrences from duplicating these health counts without changing event/sample
multiplicity. Tests cover raw/projected/durable/ring/pending parity.

See the files under `tests/_go/internal/web/`: `aggregate_projection.go`,
`aggregate_dimensions.go`, `aggregate_percentiles.go`, `aggregate_explorer_fold.go`,
`aggregate_rate_limit.go` and `conversation_lineage.go`.

## Canonical observer meaning and conversation lineage

Go's compiled model-rule executor owns canonical grouping. Observer payloads
carry exact raw-to-canonical names and a rules-content revision; only full
authoritative snapshots install a new revision. Deltas, archive and Debug names
merge under that revision, with recapture on conflicts. Browser dictionaries
prune to loaded rows/selector needs. RE2 patterns do not execute in JavaScript.
The installed observer provider is an immutable atomic holder, so SSE
record/end serialization never takes the ring lock. Canonical names are
memoized across observer calls under the compiled rules revision, and each
known-name vocabulary caches its sorted view under a set revision. Both caches
are bounded and invalidate on content change, so bootstrap polls do not re-run
the rules or re-sort thousands of names.

Observer records carry server-local `time_bucket`; browser-local dates are
presentation only. Raw names remain raw for exact export/delete semantics.

The explorer's one iterative lineage resolver observes declarations by
client/key/conversation before applying rail/gallery scopes. Parent-only
dictionary names do not become observed memberships. Conflicts, cycles and
mixed namespaces remain unknown; missing parents are not invented roots.
Summary counts follow full-filter rail scope; conversation cards follow the
gallery cross-filter. Payloads retain small resolved fields, not the temporary
graph. Parent pivots remain exact namespace-qualified leaves.

## Operator plane clients

The HTTP client in [internal/mcp](../internal/mcp) owns the operator credential,
request building, the bounded response read and the `.error` extraction that
reads the flat error body regardless of the declared Content-Type. The credential
is redacted from both the transport error and the failure body. Redirects are
refused rather than followed: `net/http`
strips `Authorization` only when the hostname changes, so a same-host
different-port `307` would replay the credential and the request body. Its route
paths are one closed set beside it, so no tool can name a path outside the
operator plane: nothing reaches the transparent inference catch-all, and no
restore-adjacent route is exposed. The explorer's `f=dim:id` / `s=` grammar is
owned once and shared by every scoped tool, and the tools never loop: a
non-advancing cursor is reported as exhaustion because the aggregate scan budget
is bounded per call. An empty page is deliberately not exhaustion, because the
proxy advances its cursor on every row it scanned, including rows the scope
excluded, so an empty page with an advanced cursor is a page that must be
continued.

The same client backs the whole tool registry. The proxy's own `/mcp` endpoint
injects a transport that dispatches each synthesized request straight to the
proxy's gated handler,
with the caller's Bearer attached so the gate re-validates it, and a named
non-routable origin (`http://millivolt.internal`) that is never dialed and
never derived from the request Host. The endpoint is stateless and
Bearer-only: every POST gets a temporary server session bound to that request's
credential, nothing survives it, and the dashboard session cookie is refused
there because a cookie must not create an internal call.

The destructive surface is guarded where the risk lives. A purge needs a filter
that constrains something (the bodyless delete-everything command is never sent),
an exact confirmation phrase, and the opaque `preview_token` that
`purge_preview` issued for that same filter, its count and an expiry. A count
cannot bind the two: it is only a number, so two filters matching the same number
would authorize each other. The token is stateless and HMAC-bound under the
operator credential, so nothing is persisted, no server-side cleanup is needed,
and a rotated credential invalidates every outstanding preview. It carries its
own expiry, enforced when the token is verified, so a preview authorizes a
deletion only shortly after it was taken. A refusal discloses no count, because
a refusal that reports the live count is a count oracle a model can retry its
way through. The residual gap is stated rather than claimed away:
the re-check and the delete are two requests with no shared transaction, so rows
committed between them are removed un-previewed. Capture refuses to start without
durable storage, and checks a scope name against the vocabulary the proxy
reports, because either way the session would report success and store nothing.
Stopping a capture session deletes nothing, and a purge deletes the stored
capture document of every request it matches; the tools say which is which
rather than implying otherwise. When that vocabulary is readable and non-empty,
an unknown capture scope or throttle provider is refused and the known names are
offered; both checks accept any name when it is empty, and the throttle provider
check also accepts one when the debug read fails, while the capture tool refuses
when the same read fails. See the [MCP server guide](mcp.md).

## Dashboard render owners

HTML embeds the canonical bootstrap builder's JSON, safely escaped in an inert
data block. Deferred external assets download alongside HTML; `settings.js` is
the one exception, injected once by the Settings trigger on first open under
the same versioned URL and single-flighted across concurrent opens. Saved
hash/chart choices apply before rendering; invalid/missing seed falls back to
the endpoint.
Dynamic HTML/state is private/no-store. Every local `/dash/` CSS/JS URL in the
shell carries the content version as `?v=`. On a hit, the service worker
answers a `/dash/` request carrying exactly that version from its stamped
CacheStorage before the network; on a miss it fetches once, stores a successful
response, and returns it. This is the one cache-first path. A request with no
`v`, a stale value, or more than one `v` stays network-first with cache
fallback, as does every brand/PWA path, so an old index cannot pin new bytes
under an old URL. The server serves the exact-version response
`private, max-age=31536000, immutable`, while a missing or stale version
revalidates with `no-cache`. A Bearer-authenticated response that also mints a
session cookie keeps the gate's stricter `no-store`. Immutable asset
representations and revalidation identities are precomputed. Content version
changes use the same winning bootstrap gate to reload once. The cache-first
path is asset caching only, never an offline app shell: navigation to `/` stays
live with the static offline fallback, and live/operator routes are never
cached.

Vanilla modules under [static/js](../internal/web/static/js) have distinct owners:
`core` formatters/shared state, `entities` identities/record labels, `explorer`
routing/cards, `chart` uPlot plans, `log` window/drawer, `chrome` operator shell,
`settings` the lazily loaded Settings sheet, and `live` bootstrap/SSE/derived
state. `chrome.js` owns the Settings trigger, loader, open-state probe, shared
status line and the single close entry point; the module owns the sheet's state,
editors and apply/revert/backup/purge behavior. A clean open at the same
document revision and writable state still fetches freshness but keeps the
retained rows and baseline; revision/writable changes, drafts and Revert refill.
The model-rule preview reuses one traffic-example cache keyed by the live
revision, and its test spelling is preview-only: it never revalidates the
draft or marks the sheet dirty.

One memoized derivation serves scoped rows and shared readouts. O(1) ID upserts,
pure-replay suppression and rAF-coalesced live rendering avoid repeated scans.
Incremental log groups preserve scroll/selection; archive paging uses paired
timestamp/ID cursors and stays outside the live arrival-ordered ring. One
single-entry memo reuses the merged ring+archive window within a live revision,
keyed by the live data and its revision plus the archive array identity/length
and the scoped-ring (or prior merged) array identity; archive replacement or
trim, reset, a live revision bump and a data replacement each rebuild it. The
explorer memoizes its filter-set identity on the canonical, order-independent
scope (filter set plus status, excluding the `by/<dim>` breakdown). A by-only
hash change repaints the rail/gallery and fetches that one breakdown while the
scoped log window, chart and drawer keep their state. Unchanged explorer
renders write nothing beyond card string construction: the rail and gallery
HTML is compared before assignment, the shared dimension trigger is rewritten
only when its dimension/count presentation key changes, and gallery layout
sizing runs only after the gallery content actually changes.

Server chart responses alone own buckets/totals at configured cadence. Presets,
series, money/unit formatters and visible dimension order each have one registry.
Derived series (error rate, cache hit, blended totals) recompute from those
server bucket sums; the browser never folds events into chart data. Hidden
lines keep stable columns; percentile/range changes reuse the plot.
Isolated samples use filled/ring points without coordinate jitter. Shared axis
formatting avoids duplicate labels. The percentile dropdown is the only visible
pXX label owner; conditional error/429 badges omit zero independently.

Operators use shared mutation/revision gates, lossless Settings collection,
stable event-path outside-click handling and a focus/inert dialog lifecycle.
CSS owns responsive breakpoints. Preserve accessibility, saved state and cheap
boot; jsdom tests do not replace real desktop/mobile canvas checks.
