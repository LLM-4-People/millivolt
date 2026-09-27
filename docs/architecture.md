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
| [internal/backup](../internal/backup/archive.go) | Self-checked operator archive format (VACUUM INTO snapshot + zstd + SHA-256). |
| [internal/proxy](../internal/proxy/proxy.go) | Routing, admission integration, retry/relay, request metadata, scoped request overrides, native-run ownership, operator state/capture; the quota-pause reaction owner [quotapause.go](../internal/proxy/quotapause.go) (settleQuotaFailure/addQuotaHold) and GET/POST /admin/quota. |
| [internal/scheduler](../internal/scheduler/scheduler.go) | Provider/key admission, retry pacing, scoped holds and provider-wide budgets; the provider-wide durable quota/billing recovery gate (quota facet, probe-paced recovery). |
| [internal/metrics](../internal/metrics/metrics.go) | Records, numeric/usage/outcome semantics, pending/ring lifecycle, observers and Prometheus. |
| [internal/sse](../internal/sse/analyzer.go) | Streaming content/usage/TTFT inspection; client-facing OpenAI SSE frames and completion envelopes. |
| [internal/format](../internal/format) | Explicit native wire translation and Connect/protobuf framing. |
| [internal/storage](../internal/storage/store.go) | Asynchronous SQLite writing, schema/read fidelity, totals, purge fence, packed snapshots and bounded queries. |
| [internal/web](../internal/web/aggregate.go) | Embedded shell/assets, bootstrap, canonical history projection and chart/explorer/log aggregates. |

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
reservation.

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

Opt-in atomic tool calls ([relay.go](../internal/proxy/relay.go) `streamBody`)
narrow that gap rather than widen it: the relay withholds every tool-call-bearing
event until the stream's terminator, so an UNCOMMITTED tool call is never on the
wire. No client generation waits for `finish_reason` to commit a streamed tool
call: every Vercel AI SDK generation checked finalizes its accumulated calls in
an end-of-stream `flush()` - `@ai-sdk/openai-compatible` 2.x, whose `hasFinished`
flag is never assigned, and the `@ai-sdk/provider-utils` 5.x tracker behind
`ai@7` and `@ai-sdk/openai@4.x`, whose `finishToolCall` is called from `flush()`
and nowhere else - so those are the only contract bytes a re-send could
duplicate destructively; with them withheld, every rescue budget keeps its
guarantee. The region opens on whatever byte run revealed the tool call, and a
byte run counts wherever it sits: the newline-free line guardrail splits a data
line longer than 1 MiB and every fragment after the first begins mid-JSON, so
the call is found by scanning the raw run, not a data line, and the same applies
to a trailing partial line. Where those bytes are already going to the wire -
the setting is off, or the line's head went out first - the relay records the
fact instead, and no rescue may follow it. The region is bounded by
`toolCallHoldMax` ([proxy.go](../internal/proxy/proxy.go)), and past that bound
it COMMITS. A commit is a wire fact the relay carries positively: no rescue
fires behind it and nothing is substituted over it. A commit of a region that
holds a tool call suppresses every class but the truncation, since the analyzer
may never have seen a whole call in those bytes; a commit of a region with no
tool call in it does not, because the size guard is not evidence of health - a
65,536-byte usage blob alone used to erase the class of a stream that produced
nothing. Either way the finished stream's class is surfaced in-band after the
committed bytes, so a committed region is never a silent success. A withheld
region is also never released by a transport read error, a client that went
away, or this process shutting the stream down: none of those proves the model
finished the call, so the region is dropped and the failure surfaced in its
place. A close this process initiated carries `503` + `server_shutdown` rather
than the flow-event `499`, which is the client's own abort. The withhold
decision is independent of the chatlike gate the degenerate classifier uses; a
non-chatlike feed's tool-call region is dropped and its truncation surfaced
rather than released.

The count of distinct tool calls has one owner, the index-keyed set on
[analyzer.go](../internal/sse/analyzer.go): a streamed tool call is keyed by its
array index, which is how the official `openai` SDKs (node, python, go) key it,
so two indices are two calls even with one id, an argument-only continuation at a
seen index is the same call, and an entry with an empty or absent id is still a
call. `@ai-sdk/provider-utils` 5.x keys by id first and therefore disagrees: two
entries sharing an id are one call to `ai@7` with concatenated arguments, where
the `openai` SDKs see two - a divergence the protocol documents rather than
reconciles, since the relay reports what the wire carried. It is a fixed-size
bitset plus a bounded id fallback (1024 calls), registration is O(1), and the
reassembled event owns every byte its buffer kept while the raw data line covers
only what that buffer dropped, so no array is decoded twice; a byte run with no
`data:` prefix (a guardrail fragment, a trailing partial) is scanned by the same
tool-call key vocabulary, so a call split by the relay's own line cap is neither
missed nor double-counted. A stream whose tool-call bytes were seen but whose
call is uncountable is never classified as a void. Covered by
`TestAtomicWithholdingIsBoundedByTheTerminator`,
`TestAtomicToolCallNeverReachesClientOnTruncation`,
`TestSplitToolCallIsNotAlsoReportedAsEmptyToolCall`,
`TestToolCallLargerThanTheTerminalCapIsStillWithheld`,
`TestOversizedToolCallFramesAreCountedAndRelayed`,
`TestToolCallFramePastTheRelayGuardrailIsRelayedWholeAndTerminatedOnce`,
`TestCommittedToolCallRegionIsSurfacedAndNeverResent`,
`TestToolCallWithNoUsableIdIsStillACall`,
`TestTwoCallsSharingOneIdAreTwoCalls`,
`TestAtomicWithholdingAppliesToANonChatlikeFeed`,
`TestAtomicFunctionCallIsWithheldAndCounted`
([tests](../tests/_go/internal/proxy/atomic_tool_calls.go),
[edges](../tests/_go/internal/proxy/atomic_tool_calls_edges.go),
[regressions](../tests/_go/internal/proxy/atomic_regressions.go)), plus the
soundness suite this branch's last review added -
`TestGuardrailSplitToolCallIsNeverResent`,
`TestWithheldToolCallSurvivesATransportReadError`,
`TestClientGoneWithAWithheldRegionReleasesNothing`,
`TestCommittedRegionWithoutAToolCallKeepsItsClass`,
`TestDefaultsReleaseATrailingTerminalPartial` and
`TestKeepaliveDoesNotDispatchTheOpenEvent`
([soundness](../tests/_go/internal/proxy/atomic_soundness.go),
[keepalive](../tests/_go/internal/proxy/sse_keepalive.go)), plus
`TestToolCallIdentityIsTheIndex`, `TestTrackedToolCallsAreBounded`,
`TestUncountableToolCallIsNeverAVoid`, `TestOutcomeCodeAssemblesThePendingEvent`
and `BenchmarkAnalyzerToolCallArray`
([sse tests](../tests/_go/internal/sse/toolcalls.go)).

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
chart buckets, and only top cards derive percentile payloads. Pending
dedupe retains only relevant pending IDs, not another history-sized seen set.

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
rate limit. A plain 499 is the LOCAL client's own abort and is a flow event; a
499 carrying a structured error observed the upstream's in-band failure before
the client left and counts like its 200 twin. A close the SERVER initiated (the
graceful-shutdown abort, `proxy.MarkAborting`) is neither: it carries 503 +
`server_shutdown` and counts as the failure it is, because marking it as the
client's own abort reported a request the proxy failed as a clean one. The
shared first-membership gate keeps duplicate tool/error occurrences from
duplicating these health counts without changing event/sample multiplicity.
Tests cover raw/projected/durable/ring/pending parity.

See the files under `tests/_go/internal/web/`: `aggregate_projection.go`,
`aggregate_dimensions.go`, `aggregate_percentiles.go`, `aggregate_explorer_fold.go`,
`aggregate_rate_limit.go` and `conversation_lineage.go`.

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

## Restart eligibility

Whether a graceful self-restart can even be attempted is decided once at boot by
the [restarter](../cmd/proxy/restart.go) (Go on PATH plus the compiled-in source
tree) and is immutable for the process. That boot-time verdict is what the
dashboard renders from, so it travels in the canonical bootstrap builder's
payload as its own `restart` section: a strict SUBSET of `GET /admin/restart`'s
status document, carrying `available` and `reason` and none of that document's
live phase, pid, error, rank or drain state, so it cannot become a second source
of restart truth. A payload without the section (or without a boolean
`available`) leaves the current behavior alone. When the verdict is false the
browser hides the control and its menu together, which also means the
eligibility `reason` has no reachable surface - it is an API-level fact, and the
server's own refusal still carries it. Covered by
`TestEligibilitySnapshotMatchesAvailability`
([tests](../tests/_go/cmd/proxy/restart.go)) and
`TestBootstrapCarriesRestartEligibility`
([tests](../tests/_go/internal/web/aggregate.go)), with the control's
visibility pinned by the shared UI suite.
