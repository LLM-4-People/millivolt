# Operations

Read the [security boundary](../SECURITY.md) before exposing the listener.
The proxy does not provide an authenticated operator plane.

## Configuration and CLI

[proxy.example.yaml](../proxy.example.yaml) is generated from the canonical
`config.Default()` and schema documentation. It is an example, not mutable
runtime state. Copy it to an ignored local `proxy.yaml` for a new deployment;
do not start Settings against the committed example.

| Flag | Behavior |
| --- | --- |
| `-config PATH` | Config path; defaults to local `proxy.yaml`. Empty selects built-in defaults and disables Settings persistence. |
| `-listen ADDRESS` | Override the configured listen address. |
| `-db-path PATH` | Override durable storage; `none` disables it. |
| `-pid-file PATH` | Write/refresh the process PID after boot, including handoff children. |
| `-print-config` | Print the generated default YAML and exit without loading local config or starting runtime services. |
| `-version` | Print JSON build identity and exit without loading config or starting runtime services. Mutually exclusive with `-print-config`. |

Precedence is built-in defaults, then the YAML file, then CLI overrides.
A missing file currently loads defaults; verify the path rather than assuming
a typo will stop startup. Existing files are strictly decoded and validated:
unknown keys, multiple documents, invalid types/ranges and conflicting values
fail load. CLI-overridden fields cannot be saved from Settings.

To regenerate the public example after changing its canonical owners:

```sh
go run ./cmd/proxy -print-config > proxy.example.yaml
```

The shared checks verify generated-file consistency. Do not copy local provider
fingerprints or credentials into defaults. Optional mappings are mechanisms,
not required provider registrations, for example:

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

## Containers

The [Dockerfile](../Dockerfile) builds Linux `amd64` and `arm64` targets and runs
as UID/GID `65532:65532`, with no source tree, Go toolchain or shell in the
runtime image. JavaScript/CSS are embedded in the Go binary; Node and Python
are test tools, not runtime dependencies. The binary is stripped of debug and
symbol tables. The final stage copies only that binary, dependency notices and
prepared state directories onto the minimal distroless base.
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
| `/config` | Writable private configuration directory if Settings should save. The default file is `/config/proxy.yaml`; a missing file uses built-in defaults. |

Fresh named volumes use the image's prepared directories. Existing volumes and
host bind mounts must permit the runtime UID to read/write their contents.
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
`-print-config` command can print the public defaults without starting a server.

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
| `GET /metrics/query?q=` | Durable-store-only restricted SELECT with timeout/output limits; not a hostile-query sandbox. |
| `GET /metrics/bootstrap` | Dashboard state and full/incremental recent-record snapshot. |
| `GET /metrics/live/stream` | Replayable finalized SSE feed plus ephemeral pending lifecycle/reset events. |
| `GET /metrics/agg/chart`, `/explorer`, `/log` | Scoped history chart, faceted explorer, and durable log paging. |
| `GET /metrics/prometheus` | Prometheus exposition over the in-memory ring, not durable since-inception dashboard totals. |

Pause holds and Debug sessions support indefinite or preset durations, partial
ID-preserving edits, and conflict rejection for overlapping scopes. Pause lets
already-running sends finish; matching new sends/retries wait. A hold cap can
refuse excess waiters. Limits are provider-wide; group concurrency is separate.

Pause/Debug/Limits successes may include a persistence warning: runtime state
was applied, but saving it failed. Do not retry as though the mutation rolled
back. With storage disabled, their memory-only state is intentional.

Export/delete filters are exact raw record fields, not display-name model
canonicalization. The API also supports conversation/error-type/time filters
beyond the menu's common choices. See the shared
[filter decoder](../cmd/proxy/log.go) and
[PurgeFilter](../internal/storage/store.go). Empty-body full deletion is
deliberate and destructive; never use malformed JSON as an all-records command.
The delete fence preserves newer completions and in-flight work.

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
