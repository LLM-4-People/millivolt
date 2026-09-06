# Operations

Read the [security boundary](../SECURITY.md) before exposing the listener.
The proxy does not provide an authenticated operator plane.

## Configuration and CLI

[proxy.example.yaml](../proxy.example.yaml) is generated from the canonical
`config.Example()` and schema documentation. `Example()` starts with neutral
`config.Default()` values and adds the enabled
[Grok/Cursor compatibility profiles](adapters.md#bundled-compatibility-profiles).
It is not mutable repository state. Source deployments copy it to ignored local
`proxy.yaml`; images bundle their own copy for fresh config volumes. Do not start
Settings against the committed example.

| Flag | Behavior |
| --- | --- |
| `-config PATH` | Config path; defaults to local `proxy.yaml`. Empty selects built-in defaults and disables Settings persistence. |
| `-listen ADDRESS` | Override the configured listen address. |
| `-db-path PATH` | Override durable storage; `none` disables it. |
| `-pid-file PATH` | Write/refresh the process PID after boot, including handoff children. |
| `-print-config` | Print neutral built-in default YAML. |
| `-print-example-config` | Print the deployment example, including its enabled compatibility profiles. |
| `-version` | Print JSON build identity. |

The three output-only flags are mutually exclusive and exit without loading local
config, opening a database or starting runtime services.

Precedence is built-in defaults, then the YAML file, then CLI overrides.
A missing file currently loads defaults; verify the path rather than assuming
a typo will stop startup. Existing files are strictly decoded and validated:
unknown keys, multiple documents, invalid types/ranges and conflicting values
fail load. CLI-overridden fields cannot be saved from Settings.

To regenerate the public example after changing its canonical owners:

```sh
go run ./cmd/proxy -print-example-config > proxy.example.yaml
```

The shared checks verify generated-file consistency. Built-in defaults remain
neutral; compatibility-profile values belong only in `config.Example()`, never
consumer fallbacks or private credentials. Optional mappings are mechanisms,
not provider registrations, for example:

```yaml
providers:
  neutral.example:
    cost_keys: [billing.charge_usd]
    usage_keys:
      input_tokens: prompt_toks
```

Use the derived provider label as the map key. The example file and Settings
schema document other maps, model rules and header templates; do not maintain
a second complete list of defaults here.

### Finding a setting

The generated example is the complete field reference: it includes defaults,
types, ranges, units and hot-reload/restart behavior. `GET /admin/config` exposes
the same field/category metadata alongside saved values, neutral defaults,
CLI overrides, the process-effective snapshot and the current revision.
Settings searches labels, keys and help across categories.

| Settings category | Configuration area |
| --- | --- |
| Server | Listen/database paths, ring depth, shutdown/restart drain and HTTP server timeouts. |
| Request | Upload bound, allowed upstream prefixes, content preview, debug retention and token refresh. |
| Upstream | Response-header deadline, connection pools, model-discovery budgets and SSE keepalives. |
| Queue & retry | Per-key admission, queue capacity/wait, retry hints, backoff and quality retries. |
| Conversations | Automatic grouping idle gap and open-conversation cap. |
| Format translation | Native-adapter defaults, Cursor parked-run lifetime and heartbeat. |
| Storage | Writer queue/batches/flush cadence and restricted-query time/output limits. |
| Dashboard | Request-log page size and KPI/chart/explorer refresh cadence. |
| Models | Ordered model-name grouping rules and their preview. |
| Providers | Usage/cost field paths, discovery enrichment, upstream headers and provider aliases. |

The table is a navigation aid, not another source of default values.
[Schema](../internal/config/schema.go), configuration validation and generated
YAML remain authoritative when a setting is added or changed.

### Saving settings safely

Settings collects a draft until Apply. Revert reloads the saved document, and
closing a dirty sheet keeps it open until Apply or Revert. The server validates the whole
result and compares its revision before writing. A stale tab receives 409;
reload/review its values instead of resubmitting an old full form.

API saves overlay supplied top-level keys on the saved config. Omitted keys
remain unchanged, but a supplied map/list replaces that entire field. For
example, `providers`, `provider_aliases` and `model_rules` are not recursively
patched. Read the current document and preserve wanted entries when editing a
structured field. Explicitly empty collections clear those fields.

The response separates saved values from the effective snapshot and reports
restart-required keys. A successful file write can still be followed by a reload
failure; `saved:true` means the file changed even when the response reports an
error. Do not assume rollback. The `writable` indicator means a config path is
configured, not that filesystem permissions or every future write are guaranteed.
See [persistence](#persistent-state-and-configuration) and
[reload behavior](#reload-restart-and-shutdown) before changing startup-bound fields.

### Provider aliases and model grouping

`provider_aliases` merges provider labels in stored history at boot/reload and
rekeys new requests. It rejects self-maps and alias chains. This is a durable
rename, not a temporary view: removing the alias does not reconstruct original
labels. Existing in-memory ring records can retain their previous label until
they age out or the process restarts. Back up before an intentional history rename.

`model_rules` instead controls display/grouping. Its ordered exact, pattern and
lowercase steps keep stored model spellings unchanged; an empty rule list groups
by those raw spellings. The Settings editor supports reordering, parking and
previewing rules. Raw export/delete predicates do not silently expand to a
display group. Debug's native-model normalization is separate from these rules.

## Containers

The [Dockerfile](../Dockerfile) builds Linux `amd64` and `arm64` targets and runs
as UID/GID `65532:65532`, with no source tree, Go toolchain or shell in the
runtime image. JavaScript/CSS are embedded in the Go binary; Node and Python
are test tools, not runtime dependencies. The binary is stripped of debug and
symbol tables. The final stage copies only that binary, dependency notices,
the generated example and prepared state directories onto the minimal distroless base.
The [Compose quickstart](../README.md#docker-compose) downloads the image-only
[compose.yaml](../compose.yaml). Normal deployment never builds from source.
Local builds use the separate
[development overlay](../CONTRIBUTING.md#container-development).
A registry tag is usable only after its publishing workflow succeeds.

### Compose operation

Keep the same Compose project name to reuse its named volumes. The normal file
names the project `millivolt`; an explicit `-p` overrides it. The deployment
inputs are `MILLIVOLT_IMAGE` (image reference),
`MILLIVOLT_PORT` (loopback host port), and `MILLIVOLT_STOP_GRACE_PERIOD` (container
stop grace). Their defaults live in [compose.yaml](../compose.yaml), not server
configuration. Supply them consistently through the environment or an ignored
local `.env` file. `docker compose config` shows the resolved deployment.

```sh
# Start or adopt a newly pulled image while preserving volumes.
docker compose pull
docker compose up -d
docker compose ps
docker compose logs --tail 100

# Stop without removing containers or data.
docker compose stop

# Remove this project's containers and network, retaining named volumes.
docker compose down
```

Do not add `--volumes` or `-v` to `down` unless deliberately deleting its
configuration and history. Changing the Compose project name selects different
volumes and can look like missing data. Back up before an upgrade; the existing
image can be replaced, but a database migration is not necessarily reversible.
See Docker's [Compose lifecycle](https://docs.docker.com/reference/cli/docker/compose/up/)
and [volume removal](https://docs.docker.com/reference/cli/docker/compose/down/)
documentation for the underlying behavior.

Do not add a build section to the deployment file. The
[development Compose overlay](../CONTRIBUTING.md#container-development) owns
source builds, its private port and its separate project identity.

The Compose service drops Linux capabilities, disallows privilege escalation,
uses a read-only root filesystem and a bounded temporary filesystem. These
controls do not add application authentication or a tenant boundary. Access to
a rootful Docker daemon through the `docker` group is effectively root access;
see Docker's [Linux post-installation guidance](https://docs.docker.com/engine/install/linux-postinstall/).

### Persistent state and configuration

| Container path | Persistence and permissions |
| --- | --- |
| `/data` | Writable database directory, including SQLite WAL/SHM sidecars. Mount the directory, not just `proxy.db`. |
| `/config` | Writable private configuration directory. The image bundles `/config/proxy.yaml` from the generated example; if deliberately absent, the application uses neutral built-in defaults. |

Fresh named config volumes receive the image's bundled example, owned by
UID/GID `65532:65532` with mode `0600`. No separate configuration download is
required. Replacing an image does not overwrite an existing saved configuration;
review example-profile changes deliberately when upgrading.

These bundle semantics apply to images built from this revision. Existing release
images are not changed by a main-branch update; consult the documentation at the
selected release tag for older images.

Existing volumes and host bind mounts must permit the runtime UID to read/write
their contents. Bind mounts hide bundled files: supply a config yourself when
using one, or intentionally accept the application's missing-file defaults.
Use private directory permissions and mode `0600` for a supplied config; with
rootless Docker or user namespaces, account for the host UID mapping. Do not
make sensitive directories world-writable to work around ownership failures.

Settings saves by creating a temporary file beside the config and atomically
renaming it. A read-only mount, or a bind mount of only the config file, cannot
support that save contract. Mount the whole writable directory, or deliberately
use read-only configuration and update it outside the dashboard. Do not mount
the committed example as a mutable runtime config.

The container listens on `:8080`; restrict publication on the host with
`-p 127.0.0.1:8080:8080`. Binding to loopback *inside* the container prevents
ordinary published-port access. Docker port publication does not add operator
authentication. Do not share one database among independent proxy containers.

Upstream URLs are reached from the proxy container, so `localhost` in
`X-Proxy-Base-URL` means that container, not the Docker host. Use an upstream
hostname reachable on its network, such as another Compose service name.
Host-service access needs an explicitly configured host-gateway mapping or
another reachable host address; it is not included in the supplied deployment.

Arguments after the image replace its complete default command. If overriding
a server flag, also keep `-config /config/proxy.yaml -db-path /data/proxy.db`
unless intentionally selecting other paths or disabling storage. The
`-print-config` and `-print-example-config` commands print their respective
documents without starting a server.

Replace the container to upgrade or adopt startup-bound settings; the dashboard
cannot rebuild an immutable image. Keep the same volumes and back them up before
upgrades. Container stop sends SIGTERM, which cancels unfinished requests before
draining storage. Allow enough stop grace time for the configured shutdown and
queued writes; forced termination can prevent that drain. No stop/flush can
recover records already dropped under storage overload.

## Versions and images

[VERSION](../VERSION) is the application release source. The dashboard asset
fingerprint is separate: it detects changed embedded frontend bytes, not a
semantic release or a complete backend change.

`go run ./cmd/proxy -version` (or `/millivolt -version` in the image) prints
JSON with `version`, `revision`, `modified`, and `go_version`. Unavailable VCS
metadata is reported honestly as revision `unknown` or `modified:null`.

The registry target is `ghcr.io/llm-4-people/millivolt`. Check the
[container package](https://github.com/LLM-4-People/millivolt/pkgs/container/millivolt)
and [image workflow](https://github.com/LLM-4-People/millivolt/actions/workflows/check.yml) for what has
actually been published; a version in a checkout does not prove an image exists.
Use a published digest to pin exact bytes. Version tags identify releases but
are not intrinsically immutable; moving development/stable aliases can change.

The [workflow](../.github/workflows/check.yml) runs source, race and browser
checks, then native container smoke checks on Linux `amd64` and `arm64`.
Publication is limited to this repository's pushes to `main` and matching
release tags, and depends on those checks. Pull requests do not publish images.

| Image tag | Meaning |
| --- | --- |
| `main` | Moving development image after a successful main-branch push. |
| `sha-<full commit>` | Source-commit tag for a published build. |
| Version from `VERSION` | Created by the matching `v`-prefixed release tag. |
| `latest` | Updated only by a non-prerelease version tag, not main-branch pushes. |

Set `MILLIVOLT_IMAGE` to the desired full image reference or published digest
before the Compose pull/up commands. The `0.x` version series is initial
development, not a promise of stable APIs across releases.

GitHub initially publishes GHCR packages privately, even when their repository
is public. A package administrator must make the package public separately and
verify an anonymous pull. Repository linkage grants workflow access, not public
visibility; see GitHub's
[package access documentation](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).

## Reload, restart and shutdown

`SIGHUP`, `POST /admin/reload`, and a successful Settings save use the same
hot-reload owner. Invalid reloads preserve the running config. Reloadable
transport/scheduler settings affect new work without closing active transports.

Startup-bound consumers keep their boot settings until restart. Settings reports
`restart_required`; its effective configuration snapshot can contain new values
that those consumers have not adopted yet. The schema/example identify which
fields require restart.

Rebuild & restart requires Go on PATH and the original source tree available at
its compiled-in path. Moved/trimpath builds may report it unavailable. It builds
first, stops acceptance while retaining the listening socket, drains active
handlers, flushes storage, then hands the socket to a ready child. Drain timeout
or flush failure aborts and resumes the old process. Parked native-adapter runs
are process-local and do not survive a successful restart.

Ordinary SIGINT/SIGTERM shutdown is different: it cancels active stream contexts,
then shuts down HTTP and drains/closes storage. Do not describe it as preserving
unfinished streams. Already-dropped records cannot be recovered by any flush.

Starting another matching proxy executable on an occupied port can trigger
same-binary port reclamation (SIGTERM, then escalation). This is not the graceful
rebuild workflow; choose distinct ports and databases for separate instances.

## Operator and data routes

All these endpoints share the same trust boundary. Read handlers enforce their
supported methods; wrong-method requests must not fall through to inference.

| Route | Action / important contract |
| --- | --- |
| `GET/POST /admin/config` | Schema/file/effective state; save `{revision,values}`. Stale revision returns 409. Saved-but-reload-failed is explicitly reported. |
| `POST /admin/reload` | Re-read config and report restart-required keys. |
| `GET/POST /admin/restart` | Status / rebuild. `GET ?watch=1` streams progress; concurrent starts are rejected. |
| `GET/POST /admin/pause` | Inspect/add/edit/resume holds. POST requires `paused`; optional ID targets one hold. |
| `GET/POST /admin/debug` | Inspect/add/edit/stop capture sessions. POST requires `enabled`; optional ID targets one session. |
| `GET/POST /admin/throttle` | Inspect/provider-limit updates; POST requires provider. Supplied limits merge; `clear:true` removes policy. |
| `GET /metrics/export` | Download all or exactly filtered finalized records. Debug-only export can contain sensitive sidecars. |
| `POST /metrics/purge/count` | Preview the same deletion/export predicate; traffic can change the count afterward. |
| `POST /metrics/purge` | Delete matching finalized records; genuinely no body means all. An empty/invalid supplied object is rejected. |
| `GET /metrics/debug?id=` | Load an unexpired durable debug sidecar; absent/no-store returns 404. |
| `GET /metrics/query?q=` | Restricted SELECT with timeout/output limits; 503 when durable storage is disabled. Not a hostile-query sandbox. |
| `GET /metrics/bootstrap` | Dashboard state and full/incremental recent-record snapshot. |
| `GET /metrics/live/stream` | Replayable finalized SSE feed plus ephemeral pending lifecycle/reset events. |
| `GET /metrics/agg/chart`, `/explorer`, `/log` | Scoped history chart, faceted explorer, and durable log paging. |
| `GET /metrics/prometheus` | Prometheus exposition over the in-memory ring, not durable since-inception dashboard totals. |

Pause/Debug/Limits successes may include a persistence warning: runtime state
was applied, but saving it failed. Do not retry as though the mutation rolled
back. With storage disabled, their memory-only state is intentional.

### Pause and Limits

Pause creates holds; it does not disconnect already-running sends. Matching new
sends and retries wait until the hold expires or is resumed. Choose all traffic,
named clients/providers, or clients that were unseen when a New-clients hold was
created. "New" does not mean new requests or new conversations. Multiple names
within a dimension are alternatives; provider and client constraints intersect.
When named clients and New are combined, either client condition may match.

A hold has its own duration and queue cap. Excess waiting work can be refused;
an indefinite hold needs an explicit resume. Existing holds can be edited in
place or resumed individually, while Resume all clears all holds. Overlapping
scopes are rejected instead of silently replacing a different hold.

Limits applies to a provider across all clients and keys, independently of the
per-key concurrency/queue settings. Choose concurrency, requests per window and
tokens per window, then Apply; Clear removes that provider policy. The menu
shows whether a policy came from the UI or a request header, together with
available budget and in-flight/queued state. Header semantics are in
[protocol](protocol.md#routing-headers). Persisted policy does not mean the live
scheduler's counters and in-flight requests survive a restart.

### Debug and preview capture

Debug selects clients, providers and/or models and starts a capture session for
a chosen duration or until stopped. At least one dimension is required; selected
dimensions intersect, with alternatives inside each dimension. It does not have
an unrestricted "all" switch. Sessions can be edited or stopped individually;
Stop all ends every active session. Conflicting scopes are rejected.

Debug model matching uses the native-model normalization in
[debug.go](../internal/proxy/debug.go), separate from display-only model rules.
Do not assume that every model spelling is an independent exact scope.

The session timer selects future captures. `debug_capture_ttl` separately
controls retained capture lifetime after completion, and
`debug_capture_max_bytes` bounds captured request plus client-facing response
bytes. Oversized captures are marked truncated. The request drawer loads an
available sidecar only for a captured request; expired/absent captures are not
recreated from ordinary metrics.

`capture_body_preview` is a different opt-in feature: it retains short prompt/
response previews in the normal record without starting a Debug session. Both
features can retain sensitive content even when credential headers are redacted.
Exports/backups can outlive the configured retention period. See
[Security](../SECURITY.md#upstream-destinations-and-credentials).

### Logs and Clear

Logs downloads finalized request records as JSON, not the process's stderr log
and not a complete administrative audit trail. With durable storage it reads the
database; otherwise it uses available ring records. Open the menu, choose a
filter and review its matching count before Download matching, or deliberately
choose Download all. A debug-only export can include retained capture sidecars.

Clear uses the same filter/count owner. Deletion requires confirmation in the
dashboard; it does not happen when the menu opens. A selected filter makes a
read-only `POST /metrics/purge/count` preview. Current traffic can change the
count before the action, but the UI retains the previewed age cutoff rather
than silently moving it. Delete everything is irreversible without a backup.

Export/delete filters are exact raw record fields, not display-name model
canonicalization. The API also supports conversation/error-type/time filters
beyond the menu's common choices. See the shared
[filter decoder](../cmd/proxy/log.go) and
[PurgeFilter](../internal/storage/store.go). Empty-body full deletion is
deliberate and destructive; never use malformed JSON as an all-records command.
The delete fence preserves newer completions and in-flight work.

### Settings and Restart

Settings only reads configuration when opened. Its search, category rail and
structured editors work on a local draft; Apply uses the
[revision-checked save contract](#saving-settings-safely).

Opening Restart reads availability and status. Restart now is disabled while a
restart is in progress or rebuilding is unavailable. Progress reports actual
build/drain/flush/handoff phases; it is not a simulated timer. After a successful
handoff, reconnect/bootstrap refreshes history, aggregates, operator state and
config. Changed embedded frontend assets trigger a page reload through that same
refresh owner. The [lifecycle section](#reload-restart-and-shutdown) distinguishes
source rebuilds from container replacement and ordinary shutdown.

## Dashboard data and request inspection

The KPI band is global. Explorer selections and the request status filter scope
the timeline and request log; selecting a card adds a filter and entity links
pivot within that shared route. Browser back/forward restores hash-based scopes.
Chart preset, range, percentile and series visibility persist in browser storage.
These saved views are local browser state, not shared server settings.

Explorer dimensions are Providers, Models, Client, Conversations, Tools, Time,
Status, Errors and Keys. Their meanings are server-owned: model grouping and
time-bucket membership are not browser guesses. Conversations can show declared
parents and main/sub/unresolved counts; read the
[lineage contract](protocol.md#main-and-sub-conversations) before interpreting them.
Tools and error occurrences can have multiple memberships per request, while
error/429 health badges count distinct affected requests.

The [visual guide](dashboard.md) covers every timeline and its controls. Traffic
compares requests/errors; Tokens separates available input/output/reasoning/cache
usage; Speed + latency uses independent throughput/TTFT axes; Errors compares
error count/rate; Cost places reported USD spend beside request volume. One
percentile selection controls the timing series. Period percentiles are computed
from period samples, not averages of bucket percentiles. Insufficient samples
remain unavailable. All time means the available retained history, not deleted,
dropped or never-observed requests.

### Request rows and details

The log starts with recent rows and pages older durable records as you scroll.
Live rows show in-flight/queue/retry progress; final rows retain their outcomes.
Retry disclosure opens absorbed attempts under the original request, not another
inference record. Click the detail arrow or a non-link row cell to open its
drawer; previous/next controls navigate requests without changing the scope.

The drawer presents available facts in sections: request identity/status/times;
client/runtime and provider metadata; requested model and parameters; conversation
turns and content-size counts; input/output/cache/reasoning tokens; performance,
cost and finish state; tool names/calls; queue/rate-limit information; retries,
errors and retained headers. Recognized parameters include sampling and output
limits, stop/logprob controls, reasoning/verbosity, response format, tool choice
and streaming flags. Missing fields are not inferred from defaults, and explicit
zero/false parameters remain distinct from absent ones.

Opening ordinary details does not fetch private request content. Prompt/response
previews appear only if captured, and Debug sidecars use their separate opt-in
and retention contract. Even ordinary labels, error text and headers can be
sensitive; do not publish request drawers from private history without review.

### Reading timing, cost and health

Request duration starts after upload and includes metadata work, queue/hold/retry
waits and response handling. First-token latency uses the final upstream attempt's
start when recorded, not accumulated retry delay. Overall throughput divides
output tokens by the full request duration; chart speed can use the measured
generation window when overall throughput is unavailable. These are different
measurements, not interchangeable claims about provider speed.

Money stays USD in storage and APIs. The shared UI formatter uses cents below
$1, including fractional cents, and dollars for larger magnitudes. Usage/cost
comes from recognized upstream fields or configured field mappings; unavailable
reporting and a displayed zero do not establish a zero bill. Reasoning/cache
splits depend on what the upstream actually reports.

Errors count affected requests with a genuine failure, including recovered
upstream failures. HTTP 429 counts requests that encountered a final or retried
429; the two signals can overlap, but 429 alone is not an error. Client-local
cancellation is likewise distinct from an upstream failure. Zero explorer health
badges are hidden independently. Error-dimension headline counts can count error
occurrences, so do not treat every displayed error number as the same metric.

### Snapshots, live feeds and monitoring

The HTML includes the canonical bootstrap state so the first paint need not wait
for a second state request. Live SSE carries pending progress and finalization;
KPI/operator polling and configured aggregate refresh keep derived surfaces current.
Reconnects use sequence/feed identity and fall back to full snapshots when replay
cannot be trusted. The footer distinguishes live/offline, active holds/Debug and
process-local storage drops. An offline indicator alone does not prove inference
is down. None of these mechanisms makes initial history preload instantaneous.

For integrations, use the documented read routes rather than scraping the DOM.
Bootstrap/live data is a recent observation window; scoped aggregate routes query
available history; JSON export provides finalized records; restricted SQL queries
durable data within configured time/output limits. The API is in initial
development, so pin and verify the selected revision for integrations.

Prometheus is a bounded-ring view. Its request/token/error totals are gauges that
can shrink after eviction, purge or restart, not monotonic lifetime counters;
do not apply counter-only `rate()`/`increase()` assumptions to them. Its TTFT
quantiles use the ring's sample contract, which differs from dashboard R7 history
percentiles. Metric names and units are exposed with HELP/TYPE metadata at
`/metrics/prometheus`; [prometheus.go](../internal/metrics/prometheus.go) owns them.

## Storage and accounting

SQLite retains finalized records; the ring and pending registry support live
observation. Full bootstrap windows are capped in durable mode and older log
rows page from storage. Without storage, the ring is the available history.

Enqueue is bounded and nonblocking. Overflow or write failures can lose durable
records even when HTTP succeeds. Bootstrap's `storage:{enabled,dropped}` reports
cumulative process-local loss; a nonzero value appears in the status footer.
Increasing queue size absorbs bursts, not arbitrary sustained overload.
Storage overload remains an unresolved capacity boundary, not a lossless mode.
Measure it with the [isolated stress workflow](../CONTRIBUTING.md#performance-evidence):
successful responses, exact durable accounting and process-local drops are
separate acceptance criteria. A short successful burst does not establish
sustainable writer throughput or an absolute request-capacity ceiling.

One shared analytical projection retains compact data and sorted metric orders
proportional to stored history. Initial preload costs startup CPU/RAM; fast
dashboard reuse is not free startup or bounded total-history memory.
Concurrent independent proxy writers sharing a database are not a supported
coherent-KPI deployment.

Scheduler groups and some observed-identity sets also retain historical labels.
Bounded request queues do not bound all process memory; long-running workloads
with many unique identities need their own memory/cardinality measurements.

Use the existing online backup into a new destination:

```sh
python3 scripts/backup_db.py proxy.db /path/to/new-backup.db
```

The helper opens the source read-only and includes committed WAL state. Do not
plain-copy an open database or omit its WAL. Protect backups, debug captures and
exports as sensitive data.

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

Use the [isolated performance workflow](../CONTRIBUTING.md#performance-evidence)
and report the exact source/image revision, platform, CPU resources, configuration,
history size and workload. Distinguish idle from loaded CPU/RSS, cold from warm
reads, proxy overhead from upstream time, and HTTP success from durable records,
tokens/cost reconciliation and storage-drop deltas. Include observer load and
whether Debug/body previews were enabled. Short local-upstream bursts are useful
regressions, not sustained production capacity or provider-latency guarantees.

### Measured example

Measured on 2026-09-06 UTC at source revision
[`495464c`](https://github.com/LLM-4-People/millivolt/commit/495464c5b485a4b8d021e5b36dcecc9f291f5a2c):
Go 1.26.6, Linux/amd64 under WSL2, Intel Core Ultra 9 285H, 12 visible logical
CPUs and 70.7 GiB of visible RAM. This was a shared workstation, not dedicated
benchmark hardware. Builds/tests were finished before measurement. CPU/RSS
figures describe the native Go process, not Docker's container-wide accounting.

The public example configuration was used, with only private listen/database
paths supplied by `scripts/dev.sh`. Debug and body previews were off. No browser
or live observer was attached during the load runs; the harness read control,
bootstrap and accounting endpoints outside the measured request stages.

| Artifact or idle state | Measured size |
| --- | --- |
| Local amd64 runtime image | 26,503,314 bytes, about 26.5 MB or 25.3 MiB |
| Fresh process, empty SQLite history | 20.5 MiB RSS |
| Process with 99,304 redacted history records, after readiness | 217.0 MiB RSS |
| Same history after All time chart and model-explorer reads | 217.1 MiB RSS |

The local image ID was
`sha256:bd9f40a0ac119b87d47297130fd63787ed793c6d71b8f56578ce6d3459b247dc`.
It passed the isolated container smoke test. Image size is Docker's reported
artifact size, not compressed download size. Idle RSS was sampled after a
15-second quiet interval. No additional process CPU tick was observed in those
intervals at 10 ms tick resolution; this does not mean the service uses literally
zero CPU. These samples do not measure cold-start time or browser rendering.

#### Streaming concurrency

The local upstream emitted ten content frames across approximately one second,
followed by usage and a terminator. Six closed-loop stages used 1, 8, 32, 128,
512 and 1,024 workers, each starting work for five seconds and then draining
active requests. Selected stages:

| Client workers | Peak upstream concurrency | HTTP requests/s | End-to-end p95 | Proxy CPU | Peak proxy RSS |
| --- | --- | --- | --- | --- | --- |
| 32 | 32 | 31.6 | 1,011 ms | 3.9% | 25.9 MiB |
| 512 | 512 | 495.4 | 1,076 ms | 41.6% | 94.7 MiB |
| 1,024 | 512 | 504.4 | 2,034 ms | 40.6% | 141.0 MiB |

Every stage had unchanged response bytes, zero failed responses, exact stored
row/token/cost reconciliation and zero storage-drop delta. At 1,024 workers,
the example's 512-connection per-host limit held upstream concurrency at 512;
the additional clients waited. That is a configured bottleneck, not a universal
1,024-request capacity ceiling. The roughly one-second upstream workload is
included in these latency figures, not proxy overhead.

#### Fast-response throughput and storage overload

A separate fresh process used the same ten-frame response without intentional
upstream delay. Each stage ran for five seconds. The 512-worker stage was not
attempted because the harness stopped on storage loss at 128 workers.

| Client workers | HTTP requests/s | End-to-end p50 / p95 | Proxy CPU | Peak proxy RSS | Stored accounting |
| --- | --- | --- | --- | --- | --- |
| 1 | 1,453 | 0.61 / 1.14 ms | 47.6% | 52.3 MiB | Exact |
| 8 | 7,092 | 0.95 / 2.00 ms | 259.7% | 80.6 MiB | Exact |
| 32 | 11,073 | 2.35 / 6.42 ms | 386.8% | 89.5 MiB | Exact |
| 128 | 16,626 | 7.02 / 15.00 ms | 503.1% | 130.5 MiB | Failed: records dropped |

At 32 workers, all 55,388 responses reconciled with stored rows, tokens and
fixture cost. At 128 workers, all 83,241 HTTP responses succeeded, but only
42,045 records persisted and 41,196 were reported dropped. The higher HTTP
throughput is therefore **not** a complete-accounting result. Increasing queue
capacity alone cannot make sustained overload lossless.

CPU uses 100% for one logical CPU and excludes the load generator/local upstream.
For context, that separate process used 229.3% CPU and 19.6 MiB peak RSS at the
32-worker fast stage, and 39.1% CPU and 63.1 MiB at the 512-worker streaming stage.
RSS was sampled every 100 ms, so shorter peaks may be missed. Latencies cover
the complete local HTTP exchange, including the synthetic upstream, and are
not isolated proxy-only overhead. Percentiles use the harness's nearest-rank
sample contract.

Each ramp retained its cumulative history between stages and cleaned up only
its unique client records afterward. HTTP rates and process CPU cover the
request stage; durable reconciliation can wait up to the harness's separate
accounting timeout afterward. Consequently, these are short HTTP-load results
with checked eventual accounting, not sustained durable-writer rates. Real
payload sizes, identity cardinality, observers, upstream delays, disk speed and
configuration can materially change the outcome.

Reproduce through the [private development workflow](../CONTRIBUTING.md#performance-evidence),
using a fresh `DEV_COPY_DB=0` instance for each command:

```sh
go run ./cmd/stress -target http://127.0.0.1:8081 -concurrency 1,8,32,128,512,1024 -duration 5s -stream-duration 1s
go run ./cmd/stress -target http://127.0.0.1:8081 -concurrency 1,8,32,128,512 -duration 5s -stream-duration 0
```

## Known limits

- Recognized provider fields drive usage/cost; this is not independent billing.
- Non-streaming usage/cost inspection keeps a bounded prefix, so larger valid
  responses can forward completely without complete accounting. Optional
  non-streaming translation buffers the entire body; discovery budgets do not
  bound that path.
- General multi-line SSE accounting/native event assembly remains incomplete;
  some split terminal/content shapes can affect classification, not just metrics.
- SQL result limits do not bound every intermediate SQLite allocation.
- Automatically grouped sessions are not proof of agent ancestry. Explicit parent
  declarations and exact-leaf filtering are described in [protocol](protocol.md).
- Existing values lost by older storage formats cannot be reconstructed.
- Extreme numeric data can produce explicit accounting/encoding errors rather
  than invented saturated prices or token counts.
