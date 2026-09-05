# Explorer and storage follow-up - 2026-09-05

Historical workload-specific evidence. Referenced `/tmp` artifacts are local to
the original review environment, not distributed with this repository. Current
setup/check commands live in [CONTRIBUTING.md](../CONTRIBUTING.md); the measured
storage overload failure below remains a limitation, not a completed fix.

This follows the [codebase audit](audit-2026-09-05.md). Repository writers were
serialized; independent readers and research agents reviewed the changes. Tests
use temporary databases and local synthetic upstreams, never paid providers or
the main instance. Source/docs now use neutral client fixtures and development
paths; existing runtime history and compiled artifacts were not rewritten.

## Explorer and conversation relationships

- `Record.HasRateLimit` detects final or retried HTTP 429 at the shared aggregate
  input. The broader scheduler `RateLimited` flag is not an exact HTTP counter.
  Each tile shows separate affected-request counts for errors and 429. Zero
  badges are hidden; the separator appears only when both counts are positive.
  API counts stay numeric zeros. A request with both a real failure and 429
  belongs to both counts.
- A reproduced duplicate tool/error membership bug counted one failed request
  twice. One first-membership gate now owns both health counts; event, sample,
  token and cost occurrence semantics are otherwise unchanged.
- `XP_RAIL_DIMS` alone owns the requested provider-first display order. The
  encoded server dimensions keep their existing layout.
- Parent relationships use explicit `X-Proxy-Session` plus
  `X-Proxy-Parent-Session`, documented in the README. One shared resolver operates
  on observed history in the existing explorer fold and respects client/key
  namespaces. The UI displays main/sub/unresolved counts, role labels, and exact
  parent links; it does not infer a hierarchy from capped browser data.
- Missing parents, out-of-order completion, purged roots, duplicate observations,
  conflicting declarations, cycles and reused IDs are covered mechanically.
  A 20,000-node chain verifies iterative resolution. An absent declaration is
  not proof of a root, and exact conversation filtering does not include descendants.

The OpenAI Docs skill informed the explicit-only protocol choice. Official
[`conversation` / `previous_response_id` documentation](https://developers.openai.com/api/docs/guides/conversation-state)
describes state continuity, not a universal spawned-agent parent relationship.
No historical ancestry or client-specific heuristic is fabricated. Kagi, Scrapling,
Context7 and DeepWiki were retried successfully for this follow-up; earlier
tool-unavailability notes in the original audit describe the earlier run.

## Storage changes and reproduced cause

The first profile identified wide SQLite binding vectors as avoidable allocation.
The existing insert owner now uses explicit zero/empty literals and parameters
for everything else, promoting columns within each batch. It keeps one prepared
statement at a time and preserves REPLACE, rollback, WAL and accounting semantics.
Column-completeness, mixed-shape parity, injection-looking strings, explicit
zero/false and failed-transaction recovery have regression coverage.

Paired isolated medians improved from 26,587 to 31,452 durable records/sec in the
core writer and 23,902 to 28,607 in the queued writer; allocated bytes fell about
66%. This was insufficient in a real HTTP replay: 128 workers still produced
54,181 successful responses but only 27,652 durable records, with 26,529 confirmed
drops. An isolated insertion benchmark was therefore not accepted as the fix.

The full proxy profile then reproduced writer starvation: every consumed record
took the same producer mutex to check whether a purge existed. Under contention,
the writer waited even with no purge pending. One atomic command pointer now owns
that check; publication still captures its accepted-record boundary under the
producer fence, and administrative purges serialize until completion.
Normal, Flush and Close drains preserve that same fence. A deterministic test
holds the producer lock after two records are accepted: the old implementation
times out, while the new writer persists both without acquiring that lock.
Overlapping purges and exact durable survivor IDs are also checked.

Three paired 500,000-request, 128-worker, in-process full-proxy benchmarks raised
median durable throughput from 5,034 to 12,941 records/sec (about 2.6×). This fixture
intentionally overwhelms storage and still drops records; it separates durable
throughput from much faster response throughput. It is not a real-network capacity
claim or an unconditional lossless guarantee.

The synchronization change follows the official
[Go atomic ordering contract](https://pkg.go.dev/sync/atomic), checked against the
installed toolchain. SQLite's [reset contract](https://www.sqlite.org/c3ref/reset.html)
and [conflict policies](https://www.sqlite.org/lang_conflict.html) were checked
against the installed driver; a conflict-policy experiment was rejected because
it did not materially help and changed semantics.

## Verification and limits

The stress harness now requires a same-process before/after `storage.dropped`
delta as well as exact bytes, durable rows, tokens and cost. Disabled or changed
epochs fail the comparison, and any confirmed drop fails the stage. Queue, batch,
flush and durability settings were not enlarged or weakened to hide overload.
Storage remains bounded, asynchronous and best-effort: no finite nonblocking queue
can guarantee persistence under arbitrary sustained overload or CPU starvation.

Initial follow-up source passed `go test ./... -count=1`, `go test -race ./... -count=1`,
`go vet ./...`, build, all 509 UI checks, shell syntax, and the private-fixture
guard tests under Python `-O`. Both reusable browser fixtures passed under `-O`:

- `scripts/explorer_check.py`: all nine dimensions at 1440px and 390px, one main
  plus two children, missing parents, recovered 500, recovered/final 429, conditional
  health badges, exact namespace-preserving parent pivots, and no overflow/browser errors.
- `scripts/browser_check.py`: all six desktop/mobile × p50/p95/p99 canvas cases
  contained both speed and latency marks, with saved controls and no overflow.

The explorer fixture reuses the existing target/config/database/redirect guards,
has no lifecycle or settings actions, and purges only its unique synthetic client.
Both fixtures disable SSE and test HTTP/bootstrap/poll rendering. Assertions now
share one `require` helper and remain enabled under Python optimization; the
guard test explicitly verifies this. Regenerable stale Python bytecode was removed.

The subsequent zero-badge correction changes only the shared card renderer:
zero errors and 429s are absent, with separators only between positive signals.
API counts are unchanged. New regression cases failed before the change across
all nine dimensions; afterward, all 544 UI checks, `go test ./internal/web -count=1`,
and the real desktop/mobile explorer fixture passed. The browser checks cover
neither badge, error-only, 429-only and both-positive states without new fixtures.
Evidence: `/tmp/millivolt-zero429-after.log` and
`/tmp/millivolt-zero429-browser.jsonl`. The main instance was not restarted.

### Remaining capacity failure

The original 128-client fast-response storage failure is **not fixed**. The
following real HTTP measurements use the retained source, default 8192-record
queue, 256-record transactions, 500ms partial-batch flush, 512-connection upstream
pool, and ten synthetic frames with no artificial response delay. Successful
responses had exact bytes; failing rows below had zero HTTP failures but confirmed
durable loss, and the harness returned failure rather than reporting a clean stage.

| Workers | Duration | HTTP requests/sec | p95 total | Durable / successful requests | Confirmed drops |
| ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 3s | 1,279 | 1.30ms | 3,838 / 3,838 | 0 |
| 32 | 3s | 10,159 | 6.70ms | 30,497 / 30,497 | 0 |
| 128 | 3s | 16,147 | 15.21ms | 26,888 / 48,539 | 21,651 |
| 16 | 30s | 9,541 | 3.73ms | 286,256 / 286,256 | 0 |
| 32 | 30s | 12,624 | 5.49ms | 277,661 / 378,758 | 101,097 |

The 16-worker stage establishes a measured lossless floor for this local workload:
286,256 exact durable records, matching input/output/cost totals, zero HTTP errors
and zero storage drops over 30 seconds. It used 3.84 sampled CPU cores and 78.9MiB
peak sampled RSS. This is not an exact universal maximum or a latency SLA.
After shutdown, an independent read-only SQLite check returned `quick_check: ok`
and the same 286,256 rows, 28,625,600 input tokens, 2,862,560 output tokens and
$286.256 synthetic cost. These are fixture values, not provider charges.
The failed 32-worker 30-second result demonstrates why a short successful burst
is not a sustained storage-capacity claim. The 128-worker process used 5.30 sampled
CPU cores and 120.2MiB peak sampled RSS; the 30-second 32-worker process used 4.61 cores and
90.9MiB. Generator and proxy share the host; workload speed and competing CPU
vary. A separate heavily CPU-contended intermediate run is excluded from any
before/after comparison. No settings or performance measurements were applied
to the main instance.

Original evidence: `/tmp/millivolt-storage-atomic-repeat.jsonl`,
`/tmp/millivolt-storage-sustained-final.jsonl`,
`/tmp/millivolt-storage-floor16-final.jsonl`,
`/tmp/millivolt-explorer-persistent-final.jsonl`, and
`/tmp/millivolt-chart-persistent-final.jsonl`.

### Evaluated but not installed

Actual-HTTP profiling after the atomic fix found negligible writer-lock blocking.
SQLite execution remained dominant: in one 2.97-second run, the writer used
2.40 CPU-seconds, including 1.83 seconds in `sqlite3_step` and 0.66 in statement
subjournal copying. CPU categories overlap and must not be added together.
Further scratch experiments retained exact-byte and durable-plus-drop checks:

| Experiment | Measured evidence | Decision |
| --- | --- | --- |
| Shared named bindings, 16-row statements | Paired median 12,280 durable records/sec versus 8,687 baseline; still roughly 17–18k drops per 50k requests. | Gain did not meet the target or justify a new hidden batching policy. Not installed. |
| Larger 32/64-row groups | Medians 10,836 / 8,748 in the same sweep. | Regressed toward baseline; rejected. |
| Flattened transaction-wide column plan | 7,344 / 11,138 / 7,188 durable records/sec, about 4KiB/request additional allocation. | No convincing benefit; rejected. |
| Public raw-driver interface plus shared bindings | In a later slower paired sequence: baseline 5,060, named bindings 7,531, raw plus named 8,662 durable records/sec. All still dropped. | Additional lifecycle/transaction/vector ownership complexity without the target result; not installed. |
| Projection-trigger write elision | 8,231 versus 8,301 baseline. | No useful gain; existing projection authority remains unchanged. |
| Text JSON-table packing | About 7k direct records/sec versus about 24k baseline. | Slower and additional value-preservation risks; rejected. |

The newer driver was also researched: its
[v1.58 binding implementation](https://gitlab.com/cznic/sqlite/-/raw/v1.58.0/conn.go)
still searches the argument vector per placeholder, while
[prepared statements already reuse their VM](https://gitlab.com/cznic/sqlite/-/raw/v1.58.0/stmt.go).
An upgrade alone is therefore not evidence of a fix. Repeated named parameters
are [supported SQLite behavior](https://www.sqlite.org/c3ref/bind_blob.html), but
did not eliminate overload in these tests. JSONB can avoid text parsing, not all
traversal; plain Go JSON also replaces invalid UTF-8 and rejects non-finite values,
so it cannot silently replace lossless stored-value handling.
See [SQLite JSON behavior](https://sqlite.org/json1.html) and
[Go JSON marshaling](https://pkg.go.dev/encoding/json#Marshal).

The scratch experiment handoff and original profile paths are recorded in
`/tmp/millivolt-storage-experiment-notes.md`. Its benchmark tables explicitly
identify normalized tool-output transcriptions, not original redirected stdout.
No scratch raw-driver, multi-row, JSON, conflict-policy or projection-trigger
prototype was installed. Eliminating the remaining overload needs further storage
execution work or an explicit change to the nonblocking/best-effort contract;
it is not achieved by these retained changes.

A native SQLite driver is a possible future benchmark, not a measured improvement
or an installed dependency. For example, the
[mattn driver build contract](https://github.com/mattn/go-sqlite3#compilation)
requires CGO and a C compiler; cross-compilation needs a target C toolchain.
Static executables remain possible with suitable linking. This changes build/CI
portability and needs an explicit project decision before a driver switch.
No such switch, installation or native-driver benchmark was performed here.
