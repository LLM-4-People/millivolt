# Operations

Read the [security boundary](../SECURITY.md) before exposing the listener.
The whole embedded dashboard is protected: every dashboard, `/metrics/*`,
`/admin/*` and `/mcp` request requires the `MILLIVOLT_OPERATOR_TOKEN` credential
and is denied while it is not configured. At the application gate, only the
unauthenticated `/healthz` liveness probe, the `POST /session` login handshake,
origin-root brand/PWA files and transparent inference stay open; see the
canonical [operator access](#operator-access) list.

## Configuration and CLI

[proxy.example.yaml](../proxy.example.yaml) is generated from the canonical
`config.Example()` and schema documentation. `Example()` starts with neutral
`config.Default()` values and adds the enabled
[Grok, OpenCode Zen and Cursor compatibility profiles](adapters.md#bundled-compatibility-profiles)
and the opencode sub-conversation tracking exemplar (the `promptCacheKey`
tracked param, strip on). It is not mutable repository state. Source
deployments copy it to ignored local `proxy.yaml`; images bundle their own copy
for fresh config volumes. Do not start Settings against the committed example.

| Flag | Behavior |
| --- | --- |
| `-config PATH` | Config path; defaults to local `proxy.yaml`. Empty selects built-in defaults and disables Settings persistence. |
| `-listen ADDRESS` | Override the configured listen address. |
| `-db-path PATH` | Override durable storage; `none` disables it. |
| `-pid-file PATH` | Write/refresh the process PID after boot, including handoff children. |
| `-healthcheck` | Probe `GET /healthz` on the configured listen address and exit nonzero on failure. Loads config read-only; used by the image's Docker HEALTHCHECK. |
| `-print-config` | Print neutral built-in default YAML. |
| `-print-example-config` | Print the deployment example, including its enabled compatibility profiles and the opencode sub-conversation tracking exemplar. |
| `-version` | Print JSON build identity. |

The three output-only flags are mutually exclusive and exit without loading local
config, opening a database or starting runtime services.

Precedence is built-in defaults, then the YAML file, then CLI overrides.
A missing file loads defaults; verify the path rather than assuming a typo
created a new empty overlay. On startup, unknown keys, unknown nested fields and
extra YAML documents are dropped and the file is rewritten without them. An
invalid recognized value fails startup without rewriting the file, and an
unparseable file is moved to `PATH.invalid` and replaced with the documented
defaults, except an ambiguous duplicate top-level key, which fails closed and
stays in place. Reload rejects an invalid document without rewriting it. Settings,
backup capture, restore merge and healthcheck read the file fail-closed and
report an error instead of substituting a default. Settings POST still rejects
those inputs. CLI-overridden fields cannot be saved from Settings. Healthcheck
loads the file read-only and never rewrites it.

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
CLI overrides, the process-effective snapshot and the current revision. Its
`last_reload` section reports the most recent configuration application:
`ok`, `error` (empty when ok), `dropped_keys`, `restart_required` and the
unix-millisecond `at` timestamp.
Settings searches labels, keys and help across categories.
Durations and byte sizes use a magnitude plus unit (`2s`, `32MiB`) in YAML and
Settings; do not write a bare number and assume seconds. A bare integer is still
accepted as a count of bytes. Integer counts (retries, percents, tokens) stay
unitless numbers with the unit in the schema label when it is not obvious.

| Settings category | Configuration area |
| --- | --- |
| Server | Listen/database paths, ring depth, shutdown/restart drain and HTTP server timeouts. |
| Request | Upload bound, allowed upstream prefixes, content preview, debug capture retention/size cap and token refresh. |
| Upstream | Response-header deadline, connection pools, model-discovery budgets and SSE keepalives. |
| Queue & retry | Per-key admission, queue capacity/wait, retry counts/hints, backoff, quality retries and thinking rescues. |
| Error storm protection | Enable provider/model detection, failure selectors and window/sample thresholds, recovery, queue/scope bounds, additional retries and banner visibility. |
| Quota pause | Reaction to durable quota/billing 429s: surface, provider-wide recovery gate or indefinite operator-held pause, and the recovery-probe success count. |
| Conversations | Automatic grouping idle gap, open-conversation cap and per-client tracked body params. |
| Format translation | Native-adapter defaults, the cross-protocol [translation loss policy](adapters.md#cross-protocol-translation), Cursor parked-run lifetime and heartbeat. |
| Storage | Writer queue/batches/flush cadence, restricted-query time/output limits and the irreversible database deletes (filtered and whole-database). |
| Backup | Size cap for one Settings backup download or restore upload. |
| MCP | The operator-gated `/mcp` LLM tool endpoint: the enable switch, the result-size limits its tools clamp with, and the call timeouts. Restart to apply. |
| Dashboard | Request-log page size, KPI/chart/explorer refresh cadence and background refresh while its tab is hidden. |
| Models | Ordered model-name grouping rules and their preview. |
| Providers | Usage/cost field paths, discovery enrichment, upstream headers and provider aliases. |
| Request overrides | Scoped upstream rewrites: set, replace or remove headers and set output-token ceilings per client, provider and/or model. |

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

The response separates saved values from the effective snapshot, reports
restart-required keys and carries the same `last_reload` section as GET; the
Settings sheet renders it as its last-apply row with apply time, failure,
dropped-key count and restart notice. A successful file write can still be
followed by a reload failure; `saved:true` means the file changed even when
the response reports an error. Do not assume rollback. The `writable`
indicator means a config path is configured, not that filesystem permissions
or every future write are guaranteed.
See [persistence](#persistent-state-and-configuration) and
[reload behavior](#reload-restart-and-shutdown) before changing startup-bound fields.

### Provider aliases and model grouping

`provider_aliases` merges provider labels in stored history at boot/reload and
rekeys new requests. All aliases in one map commit as a single transaction, so
a trigger or constraint failure on a later alias leaves every earlier label
unchanged. It rejects self-maps and alias chains. This is a durable rename, not
a temporary view: removing the alias does not reconstruct original labels.
Existing in-memory ring records can retain their previous label until they age
out or the process restarts. Back up before an intentional history rename.

`model_rules` instead controls display/grouping. The list is capped at 64 rules.
Its ordered exact, pattern and lowercase steps keep stored model spellings
unchanged; an empty rule list groups by those raw spellings. The Settings editor supports reordering, parking and
previewing rules. Raw export/delete predicates do not silently expand to a
display group. Debug's native-model normalization is separate from these rules.

### Request overrides

`request_overrides` is an ordered rule list that rewrites the upstream request
before relay; the default empty list leaves every request untouched. Each rule
has a scope and at least one action: `headers` sets or replaces upstream
headers, `remove_headers` deletes them, and `body` sets the output-token
ceilings `max_tokens` / `max_completion_tokens` (1..1000000) on a Chat
Completions or Anthropic client body before translation. A Responses client
body is not rewritten by the body action because its cap is
`max_output_tokens`; header and removal rules still apply. A Chat client whose
target is the Responses wire has the rewritten cap translated to
`max_output_tokens`. Both override spellings address an Anthropic body's single
`max_tokens` field, with `max_completion_tokens` winning when both are set. The
list is capped at 64 rules and validated like every
other setting: a rule needs at least one scope field and one action, duplicate
scope triples are rejected, and one header name cannot be both set and removed
in the same rule.

Scope matching is exact leaf. A rule applies when every non-empty scope field
equals the request's value exactly: `client` against the classified client
label (`X-Proxy-Client` when sent, except a label that only repeats the user
agent's product name, which records the fuller user-agent identity; otherwise
inferred from SDK/user-agent metadata), `provider` against the canonical
label after `provider_aliases`,
and `model` against the recorded model id (for Cursor requests the base id the
dashboard records, not the fused spelling). An empty scope field matches
anything. There is no model-rule canonicalization, regex or wildcard broadening.
All matching rules apply in list order, and for the same header or body field
the later rule wins.

Header rules are the operator's last word on the upstream wire: the resolved
merge applies after client-forwarded values, configured provider headers and
the client's `X-Proxy-Headers` map, so a matching override wins over all of
them. The proxy-owned transport-mode names `accept-encoding` and `range` are
the exception: the client header maps (`X-Proxy-Headers` and an
`X-Proxy-Auth-Header` naming one) reject them with a 400 before any send, and
configured provider headers or request-override values are stripped from the
final upstream request, so the transport keeps its own gzip negotiation and
never sends a partial range. Credential and protocol names (`authorization`, `cookie`,
`proxy-authorization`, `host`, `content-type`, `content-length`,
`transfer-encoding`, `te`, `connection`, `keep-alive`, `proxy-authenticate`,
`proxy-connection`, `trailer`, `upgrade`) and the whole `x-proxy-` control
prefix are rejected at load in both header lists, and the client header maps
reject `content-length` before any send so the transport keeps ownership of
message framing; at runtime the target's configured auth header is skipped
even if one slips through, so credentials stay at their owner. The fixed list cannot know a provider's configured
auth header, so a rule naming the target's (for example `x-api-key`) passes
config validation and is denied at send time by the runtime belt. Header
names are trimmed and canonicalized
case-insensitively, and values must be non-empty single-line header values.

The body rewrite is fail closed. It applies to Chat Completions or Anthropic
client bodies before translation, whatever the target (a Responses client body
is not rewritten); when the body is not a JSON object, or the rewritten
result would exceed `max_request_bytes`, nothing is written: the original
bytes are used unchanged by the rewrite stage and are still translated when
translation is active, with one process log line recording the reason. Only a
passthrough relays them verbatim. A successful rewrite replaces the body for
the request's whole life, so every relay attempt, absorbed retry and quality
re-send reuses it, and the recorded request cap is re-stamped from the
effective body. A rule sets only the body
fields it names, and the re-read pair keeps the wire's documented precedence -
`max_completion_tokens` wins over `max_tokens` - so a rule setting only
`max_tokens` cannot raise a request whose client already sent
`max_completion_tokens`: set both fields to the same value to override either
spelling. Cursor targets carry no
token ceilings on their wire, so the body section is skipped for them while
header rules still apply at the native send.

Overrides apply to inference sends only, never model discovery or token
refresh. The list hot-reloads: changed rules apply to new requests. The
Settings editor mirrors the server's checks and rejections live for each
rule, marks empty scope fields as matching any request, and blocks Apply
on the first invalid rule.

To raise the output budget for one provider, either write:

```yaml
request_overrides:
  - provider: nano-gpt.com
    body:
      max_completion_tokens: 65536
```

or open Settings, select **Request overrides**, pick **raise the output budget
for a provider** from the add-a-rule menu, and fill the provider scope; the
other starter templates seed an empty rule and an upstream header for a
client.

### Sub-conversation tracking

`sub_conversations` gives requests from one client a conversation identity of
their own, taken from a value the client already sends in its request body
(exemplar: opencode's `promptCacheKey`). The default empty list leaves tracking
fully off, and the feature is deny-by-default per client: an entry applies only
when its `client` equals the request's classified client label exactly (the same
leaf the request-overrides `client` scope matches; no wildcard, no case
folding), and a request from any other client is never inspected, leaving its
bytes and grouping untouched.

Each entry lists 1..4 exact JSON field names, matched only at the request
body's top level and checked in order; the first present field decides. A
present value that is not a JSON string counts as absent and the next field is
checked, while a present string that cannot be decoded or fails the identity
rules drops the identity for the request - no later field is consulted. A
value is kept only when it is non-empty after trimming, valid UTF-8, free of
control bytes and at most 512 bytes, the same bound explicit session
declarations satisfy. The UTF-8 judgment runs on the value's raw bytes
before JSON decoding: the standard decoder coerces invalid UTF-8 inside
strings to U+FFFD, so a raw invalid-UTF-8 sequence drops the identity while
an escaped lone surrogate, whose bytes are ASCII, is kept and decodes to
U+FFFD. The drop is conservative: the value's raw spelling is
scanned inside a bounded window, so a value whose raw span exceeds the window
(escape sequences spend more raw bytes than they decode to, and trimmable
edges count before trimming) is dropped even when its trimmed form would
pass. An absent or dropped identity is never a client error: on the
passthrough wire the body is payload, and the request keeps automatic
grouping. A translated target still requires the whole request body to be
valid UTF-8, as [protocol](protocol.md#relay-retries-and-model-discovery)
describes.

The kept value groups the request under a `k:` conversation identity.
Precedence is the explicit `X-Proxy-Session` header first, then the tracked
value, then automatic client+key grouping; a request carrying both header and
tracked value keeps the header identity. `X-Proxy-Parent-Session` still owns
parent linkage, and declaring a parent still requires the explicit session
pair, so a tracked identity never carries a parent. In the dashboard a `k:` id
is an ordinary conversation id: lineage, filters, saved views and the request
drawer read it like any other.

`strip: true` removes every configured field of the entry that is present at
the body's top level from the client body before relay or translation,
including a Responses client body; the default `false` forwards them
unchanged. Unlike the [request overrides](#request-overrides) body fields,
strip is not limited to Chat Completions or Anthropic inbound. The removal
rides the same body-rewrite engine: one document decode when both features
match one request, the original bytes used unchanged by the rewrite stage when
the body is not a JSON object or the rewrite would exceed
`max_request_bytes`, and cursor targets skipping the body section. Without
`strip`, the client's spelling reaches the upstream wire on a same-protocol
passthrough; a translated body carries the value in `prompt_cache_key` or drops
it.

When a tracked value was extracted and the request translates to the Anthropic
wire, the translated body gains a top-level `prompt_cache_key` carrying the
value. This is a proxy tracking extension, not an Anthropic cache field:
Anthropic caching uses `cache_control`, and an upstream that rejects unknown
top-level fields will reject the injected request. Injection follows extraction
alone: `strip` governs the tracked client-body fields, never the injected
field.

Validation runs at the load boundary for YAML and Settings saves alike: at most
16 entries, at most 4 params per entry, param names one JSON key segment each
(trimmed, non-empty, at most 64 bytes, printable ASCII, no quote, backslash or
control bytes), and duplicate clients or duplicate params within one entry are
rejected. The list hot-reloads: changed entries apply to new requests.

The Settings editor lives in the Conversations category and mirrors every check
live per entry card: classified-client autocomplete from observed clients,
ordered param rows with add/remove gated at the caps, the strip toggle, a count
line against the entry cap, the server's exact rejection wording, and an Apply
blocked on the first invalid card. To track opencode's prompt cache key and
strip it from the upstream body, write:

```yaml
sub_conversations:
  - client: "opencode"
    params:
      - "promptCacheKey"
    strip: true
```

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
[development Compose](../CONTRIBUTING.md#container-development).
A registry tag is usable only after its publishing workflow succeeds.

### Compose operation

Keep the same Compose project name to reuse its named volumes. The normal file
names the project `millivolt`; an explicit `-p` overrides it. The deployment
inputs are `MILLIVOLT_IMAGE` (image reference),
`MILLIVOLT_PORT` (loopback host port), `MILLIVOLT_OPERATOR_TOKEN` (the
operator credential, forwarded into the container environment only), and
`MILLIVOLT_STOP_GRACE_PERIOD` (container
stop grace). Their defaults live in [compose.yaml](../compose.yaml), not server
configuration. Supply them consistently through the environment or an ignored
local `.env` file. `docker compose config` shows the resolved deployment.
The image carries a Dockerfile `HEALTHCHECK` that probes the unauthenticated
`/healthz` through the binary's `-healthcheck` mode, so `docker compose ps`
reports health without any operator credential.

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
[development Compose](../CONTRIBUTING.md#container-development) owns
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

A first launch cannot prepare its own bind-mount host directories. When a
bind-mount target does not exist, Docker creates it as `root:root` mode `0755`,
which the runtime UID cannot write, and startup fails with SQLite's
`unable to open database file` error. The log names the database path, the
directory state and the runtime UID. Pre-create the host `data/` and `config/`
directories owned by `65532:65532` before the first start, or
`chown -R 65532:65532` them after a failed start, then start the container
again. Named volumes avoid this entirely: Docker seeds them from the image
with correct ownership.

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
upgrades. If only a saved config file changed, ordinary `up -d` need not replace
the container. Apply startup-bound changes explicitly:

```sh
docker compose up -d --force-recreate millivolt
```

Container stop sends SIGTERM, which cancels unfinished requests before
draining storage. Allow enough stop grace time for the configured shutdown and
queued writes; forced termination can prevent that drain. No stop/flush can
recover records already dropped under storage overload.

### Image-only backup and restore

The supplied deployment can back up both named volumes without a source checkout,
Python, a shell inside the image or a helper image. This is an **offline** backup:
stop callers first, allow active work to finish, and keep the service stopped
while copying. Run from the deployment directory with the same Compose project
and environment used to start it. If you use `-p`, repeat that selection on every
Compose command. Do not concurrently replace or restart the source container.

The following saves the original Compose file, the pulled image's registry digest,
and both volume archives in a new private directory. It expects the supplied
single-service layout, `/data` and `/config`, with a saved `proxy.yaml` present.
Privately retain any local `.env` or other deployment inputs as well.

```sh
(
  set -eu
  umask 077
  backup_dir="$(mktemp -d ./millivolt-backup.XXXXXXXX)"
  printf 'Backup directory: %s\n' "$backup_dir"
  container_id="$(docker compose ps -q millivolt)"
  test -n "$container_id"
  image_id="$(docker inspect --format '{{.Image}}' "$container_id")"
  docker image inspect --format '{{index .RepoDigests 0}}' "$image_id" > "$backup_dir/image.txt"
  cp compose.yaml "$backup_dir/compose.yaml"
  docker compose stop millivolt
  test "$(docker inspect --format '{{.State.Status}} {{.State.ExitCode}}' "$container_id")" = 'exited 0'
  docker compose cp millivolt:/data/. - > "$backup_dir/data.tar"
  docker compose cp millivolt:/config/. - > "$backup_dir/config.tar"
  docker compose start millivolt
)
```

A failed stop or copy exits without automatically restarting the service; inspect
the failure before resuming. Copy the completed backup to protected independent
storage. The data archive includes any WAL/SHM files, not just `proxy.db`. Never
use this file-copy procedure while SQLite is open. An archive cannot recover
records already dropped or writes lost through forced termination.

Restore into a **new project and fresh volumes**, leaving the original deployment
and backups untouched. Enter the completed backup directory and an unused
loopback host port when prompted. The original supplied Compose file is required:
do not substitute resolved `docker compose config` output, custom external-volume
names or bind mounts, which can point the new project at existing data.

```sh
(
  set -eu
  umask 077
  printf 'Backup directory to restore: '
  IFS= read -r backup_dir
  test -s "$backup_dir/data.tar"
  test -s "$backup_dir/config.tar"
  MILLIVOLT_IMAGE="$(head -n 1 "$backup_dir/image.txt")"
  case "$MILLIVOLT_IMAGE" in *@sha256:*) ;; *) exit 1 ;; esac
  printf 'Unused loopback host port: '
  IFS= read -r MILLIVOLT_PORT
  case "$MILLIVOLT_PORT" in ''|*[!0-9]*) exit 1 ;; esac
  test "$MILLIVOLT_PORT" -ge 1
  test "$MILLIVOLT_PORT" -le 65535
  export MILLIVOLT_IMAGE MILLIVOLT_PORT
  restore_dir="$(mktemp -d ./millivolt-restore-XXXXXXXX)"
  restore_project="$(basename "$restore_dir" | tr '[:upper:]' '[:lower:]')"
  cp "$backup_dir/compose.yaml" "$restore_dir/compose.yaml"
  restore_compose() {
    docker compose --env-file /dev/null -p "$restore_project" -f "$restore_dir/compose.yaml" "$@"
  }
  existing_containers="$(docker ps -aq --filter "label=com.docker.compose.project=$restore_project")"
  test -z "$existing_containers"
  existing_volumes="$(docker volume ls -q --filter "name=^${restore_project}_")"
  test -z "$existing_volumes"
  restore_compose create millivolt
  restore_compose cp --archive - millivolt:/data < "$backup_dir/data.tar"
  restore_compose cp --archive - millivolt:/config < "$backup_dir/config.tar"
  restore_compose up -d millivolt
  restore_compose logs --tail 100 millivolt
  printf 'Restored project: %s\nCompose directory: %s\nHost port: %s\n' "$restore_project" "$restore_dir" "$MILLIVOLT_PORT"
)
```

Keep the tar streams intact and retain `--archive` on restore: the private config
must remain readable/writable by runtime UID/GID `65532:65532`, with mode `0600`.
Check Settings and retained history on the new private port before redirecting
clients. Save the printed project name and deployment inputs for later commands;
the example bypasses automatic `.env` loading during recovery.

Docker documents [tar-stream copying](https://docs.docker.com/reference/cli/docker/container/cp/)
and [Compose archive mode](https://docs.docker.com/reference/cli/docker/compose/cp/).
The repository's isolated container check exercises stopped-volume backup and
restore into fresh volumes, including settings, history and file permissions.
Source deployments can instead use the live SQLite
[online-backup helper](#storage-and-accounting) or the Settings archive
described next.

### Settings backup and restore

The dashboard Settings Backup category downloads and restores a self-checked
`.mvb` archive. Choose config, database, or both. The archive is a packed
SQLite snapshot (`VACUUM INTO`, so free pages are not copied) and/or the
schema YAML, then zstd-compressed with a content checksum and an outer SHA-256
of the uncompressed payload. Encoding streams each member into the compressor
while hashing and counting it, writes the archive once into a staging file,
and self-checks that file before returning it; an archive that fails decode
or semantic checks is never emitted, so a produced backup is never a silent
corrupt file. The database member is read from the packed snapshot file in
bounded chunks instead of being copied whole into memory, so the download cost
does not grow with a heap copy of the database. A source file that changes
size or cannot be read fails the encode before an archive is emitted.

Restore streams the upload through the `backup_max_bytes` ceiling into one
private staging file and decodes it with the same header, zstd, SHA-256 and
member-framing checks. The database member is written straight into a private
file on the live database's volume instead of being materialized on the heap;
config bytes stay buffered within the same 16 MiB config-document bound
`Config.Validate` enforces on the canonical rendered YAML, so a config that
validates can never exceed what an archive may retain. A config member that
declares a larger size is rejected before its bytes are copied, and encoding
refuses to emit one, so an older archive whose config member exceeds that bound
fails restore. The bound covers only the config member; the database member
stays streamed under the payload ceiling. It validates that hash, the
zstd frame, config `Validate()`, and `PRAGMA integrity_check` before applying
anything. Decode refuses an uncompressed payload larger than the Schema maximum
for `backup_max_bytes`, so a small compressed file cannot expand without bound.
Download lists how many live settings differ from default and how many stored
requests would be packed. Picking a file first inspects the archive: created
time, size, request span, and each config key as default vs modified vs the
live file. Inspect loads the live file through the same strict, bounded reader
used by config reload and restore, not the boot repair path: an unknown nested
field, an extra YAML document, a non-regular file, or a file above the 16 MiB
document bound fails the inspect instead of comparing against a repaired or
partial view. Inspect validates and
counts the decoded database file in place and computes its overlap with the
live store without writing another copy.
Config **replace** loads and validates the backup, writes canonical YAML, and
preserves CLI-overridden `listen` and `db_path`. Config **merge** overlays only
keys that differ from `Default()` onto the live file, so live custom values
stay unless the backup also customized them. An omitted `config_mode` or
`database_mode` defaults to replace; an explicitly empty or whitespace value is
rejected as malformed rather than treated as absent. Either way the restored config
hot-applies through the same owner as a Settings save, is reflected in
`last_reload`, and reports restart-required keys. Database
**replace** streams the decoded file into a pending snapshot next to `db_path`,
syncs it, and atomically renames it into place; Open admits the pending file
only after it checks again, and the restore waits for process restart.
Database **merge** attaches the decoded file read-only and inserts request ids
that are not already in the live store immediately (live rows win on conflict);
the attached snapshot rejects writes, while the live database stays writable
for the merge, and no restart is required. Its request rows, debug sidecars and
since-inception totals commit as one transaction, so a failed totals write
leaves no partial merge behind. Truncated or bit-flipped files are rejected.
The `backup_max_bytes` setting is the archive ceiling; encoding enforces it
while the stream is written, so an over-cap archive is removed instead of
partially served, and an over-cap upload answers 413 before decoding.

Restore phases are ordered, not atomic. The database member runs first: a
replace stages the decoded file and preflights it with the same schema,
writer-INSERT and request-scan value checks boot runs before touching the live
database, and a merge commits rows into the running store. Only then is the
config written and hot-applied. A failed stage, preflight, or merge leaves the
config file and live snapshot unchanged, and a failed config write or reload
rolls the config back and restores a replace's previous pending slot. A
successful live merge has no rollback: if the later config phase fails, the
merged rows stay applied. A crash between the database and config phases is
likewise not atomic; the pending snapshot or completed merge can be durable
while the config file still has its previous bytes. Preserved `.incoming`,
`.previous`, and `.snapshot-state` files next to `db_path` are boot-recovery
evidence and may need operator inspection after such a crash; do not remove them
while a restore may have been in flight. A combined config and database merge
answers 400 when the restored config's effective `db_path` differs from the
running store's file, because the rows would be written to a database the next
boot does not open; merge the database separately or use database replace.

At boot, a pending snapshot is admitted only after the same schema, trigger,
writer-insert and request-scan value preflight `Open` runs. Recovery consults
the durable `.snapshot-state` marker rather than inferring a transition from the
files present. A markerless rollback set from a pre-marker release cannot be
classified: when a live main exists, recovery keeps it and removes the
`.previous` debris. Discarding a non-empty `.previous-wal` in that markerless
state logs a startup warning, because its committed frames cannot be
distinguished from debris and may be lost. Marked pending and committed recovery
behavior is unchanged.

Both directions stage database-sized transient files in the live database's own
directory, so capacity follows the data volume rather than a hardened
deployment's small `/tmp` tmpfs. Budget roughly one compressed upload plus one
packed database copy during a restore, and for a replace also the pending copy
at `db_path`. The download's `VACUUM INTO` snapshot directory is removed after
the archive is encoded, including on failure or a client disconnect. A restore
removes its private upload file and decoded database directory after inspect or
apply on every path: a normal response, an invalid or over-cap archive, a
validation or apply failure, or a client disconnect. A config-only upload may
use the process temp directory because it carries no database-sized member. A
packed file already on the staging volume (VACUUM dest, decoded member, pending
`.incoming`) is integrity-checked in place, and inspect, overlap, replace and
merge consume it by path, so none writes a second database-sized copy. A
container's `/tmp` is a small tmpfs by design and never caps backup size.

## Versions and images

[VERSION](../VERSION) is the application release source. The dashboard asset
fingerprint is separate: it detects changed embedded frontend bytes, not a
semantic release or a complete backend change.

`main` is promotion-only. Full source, browser and live MCP integration, and
native container checks run on all `testing` pull requests, code-bearing
`testing` pushes, manually dispatched `testing` gates, and manually pushed `v*`
tags; docs and site-only pushes use `docs.yml`. They also run on Dependabot's `main` security PRs. An ordinary
promotion pull request from `testing` requires its head to be the current
`testing` tip, `main` to be already merged back into `testing`, and a
successful full-gate record for that exact commit: the canonical `check.yml`
push run's own check suite, with `checks` and both native container legs. When
a final docs-only commit follows the closest gated `testing` commit, it reuses
that gate if every path since it is docs/site-only; the fallback selects the
closest gated testing commit from canonical `testing` push runs, with no
commit-count bound. The proof polls
pending exact check runs a bounded number of times and fails closed if they are
still pending. For a code-bearing head, let the `testing` `check.yml` push gate
finish before opening or re-running a promotion PR. A docs/site-only head has no
canonical `check.yml` push run of its own, so wait for `docs.yml` or only the
`required` docs validation. It does not rerun the suite. A code-bearing promotion must also have a `VERSION`
newer than every existing release tag. Docs/site-only promotions skip the
version and full-gate proofs, validate docs, workflow policy and the site in
`required` within its ten-minute budget, and do not rebuild images or cut a
release. Merge with a merge
commit. The merged-PR release workflow tags that reviewed merge commit and
publishes the image; a delivered code-bearing main push is an idempotent
fallback. It never pushes `main`. Dependabot security pull requests are the
only other exception and normally do not cut a release by themselves; an
untagged pending `VERSION` can publish. A manually pushed `v*` tag ships exactly
as tagged. The full maintainer checklist lives in
[Contributing](../CONTRIBUTING.md#branch-workflow).

`go run ./cmd/proxy -version` (or `/millivolt -version` in the image) prints
JSON with `version`, `revision`, `modified`, and `go_version`. Unavailable VCS
metadata is reported honestly as revision `unknown` or `modified:null`.

The registry target is `ghcr.io/llm-4-people/millivolt`. Check the
[container package](https://github.com/LLM-4-People/millivolt/pkgs/container/millivolt)
and [image workflow](https://github.com/LLM-4-People/millivolt/actions/workflows/check.yml) for what has
actually been published; a version in a checkout does not prove an image exists.
Use a published digest to pin exact bytes. Version tags identify releases but
are not intrinsically immutable; moving branch and stable aliases can change.

The [workflow](../.github/workflows/check.yml) runs source, race and browser
checks, then native container smoke checks on Linux `amd64` and `arm64` for all
`testing` pull requests, code-bearing `testing` pushes, manually dispatched
`testing` gates, and manually pushed `v*` tags, and for Dependabot's `main`
security PRs. Docs and site-only pushes use `docs.yml` instead. A normal
promotion PR proves the current testing tip, merged-back main and the full-gate
record for that commit, or for the closest gated testing commit with only docs
changes after it; docs-only promotions validate the docs, workflow policy and
site directly. The merged promotion PR carries the reviewed merge commit and
releases it; a delivered code-bearing `main` push only reuses the same reviewed
merge and is idempotent.
Publication is limited to this repository's merged promotion pull requests,
delivered pushes to `main`, matching release tags, and `testing`-branch pushes
(which publish only the moving `testing` tag). The release `publish` job waits
for `required`. The `publish-testing` job waits for `checks` and `container`
instead, because a testing push has no promotion proof to wait for. A release
run whose `VERSION` is already tagged, such as a Dependabot security merge,
normally does not publish; if main carries an untagged pending `VERSION`, that
run can publish it. Open or unmerged pull requests do not publish images; a
merged promotion PR does.

| Image tag | Meaning |
| --- | --- |
| `main` | Moving production image for the released current `main` revision; updated by non-prerelease versioned releases from a merged promotion PR, a delivered main push or a release tag. A prerelease does not move it. |
| `testing` | Moving development image after a successful testing-branch push; never a release. Its OCI version label reports the embedded `VERSION`, which can lag the latest release. |
| `sha-<full commit>` | Release tag for the exact cut revision built and published; the version bump is part of the reviewed promotion merge, so this is a main commit. Testing pushes do not publish it. |
| Version from `VERSION` | Created by the matching `v`-prefixed release tag on the promotion merge commit, or by a manually pushed tag. |
| `latest` | Moving stable alias updated by every non-prerelease release, including a release cut by a merged promotion PR, a main push or a tag. Prereleases and testing pushes do not update it. |

Release builds request an SBOM and max-mode provenance. The image and its
attestations are published before the tag and release are updated; the workflow
never pushes `main`. A failed image build does not leave a public release
pointing at a missing image. Verify
the published manifest and attestations with `docker buildx imagetools inspect`
and pin the digest for deployment. A tag name alone is not an integrity check.

Set `MILLIVOLT_IMAGE` to the desired full image reference or published digest
before the Compose pull/up commands. The `0.x` version series is initial
development, not a promise of stable APIs across releases.

GitHub initially publishes GHCR packages privately, even when their repository
is public. A package administrator must make the package public separately and
verify an anonymous pull. Repository linkage grants workflow access, not public
visibility; see GitHub's
[package access documentation](https://docs.github.com/en/packages/learn-github-packages/configuring-a-packages-access-control-and-visibility).

## Reload, restart and shutdown

`SIGHUP`, `POST /admin/reload`, a successful Settings save, and a backup
config restore use the same hot-reload owner. Invalid reloads preserve the
running config. Reloadable transport/scheduler settings affect new work
without closing active transports.

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

The routes below belong to millivolt. They are operator-gated unless the row
is marked open; the canonical open-path list is in
[operator access](#operator-access). Read handlers enforce their supported
methods; wrong-method requests must not fall through to inference. The
namespace is standardized: `/metrics/*` and dashboard routes are the protected
read plane, and `/admin/*` is the operator action plane; the credential gates
both.

| Route | Action / important contract |
| --- | --- |
| `GET /healthz` | Unauthenticated liveness probe for Docker HEALTHCHECK and load balancers. Reports process/HTTP liveness only, no storage depth. |
| `GET /favicon.ico` | Unauthenticated ICO brand mark. Browsers fetch this without Authorization; it is registered on the mux so it never reaches the inference catch-all. |
| `GET /favicon.svg`, `/apple-touch-icon.png`, `/icon-*.png` | Ungated SVG/PNG icons for tabs, home screens and the web app manifest. |
| `GET /manifest.webmanifest` | Ungated web app manifest (name, standalone display, 192/512 icons). |
| `GET /sw.js` | Ungated service worker. Precaches brand icons/manifest; cache-first only for a `/dash/` URL carrying exactly the stamped version, network-first with cache fallback for unversioned or stale `/dash/` URLs and brand paths. Navigations to `/` are fetched live (never cached: login vs bootstrap) with a static offline fallback. Never intercepts `/metrics/*`, `/admin/*`, `/session/*` or `/v1`. |
| `POST /session` | The one open operator route: exchanges the credential for the session cookie the dashboard's live feed needs. Throttled like every gated route. Lives outside `/admin` so an external authentication layer can front the admin plane without gating millivolt's own login. |
| `GET/POST /admin/config` | Schema/file/effective state; save `{revision,values}`. Stale revision returns 409. Saved-but-reload-failed is explicitly reported. GET and save responses carry a `last_reload` section: `ok`, `error`, `dropped_keys`, `restart_required`, `at`. |
| `POST /admin/reload` | Re-read config and report restart-required keys. |
| `GET/POST /admin/restart` | Status / rebuild. `GET ?watch=1` streams progress; concurrent starts are rejected. |
| `GET/POST /admin/pause` | Inspect/add/edit/resume holds. POST requires `paused`; optional ID targets one hold. |
| `GET/POST /admin/quota` | Inspect storm state (shared with the banner) / `POST {"provider","resume":true}` closes a retry-mode quota gate; parked sends resume immediately. |
| `GET/POST /admin/debug` | Inspect/add/edit/stop capture sessions. POST requires `enabled`; starting a session requires durable storage (503 `durable storage is disabled` otherwise); optional ID targets one session. |
| `GET/POST /admin/throttle` | Inspect/provider-limit updates; POST requires provider. Supplied limits merge; `clear:true` removes policy. |
| `GET /admin/debug/capture?id=` | Load an unexpired durable debug sidecar; absent/no-store returns 404. `&download=1` serves the same document as a gzip artifact named `millivolt-debug-<captured_at>.json.gz`, falling back to the record id when `captured_at` does not decode. |
| `POST /admin/purge/count` | Preview the same deletion/export predicate; traffic can change the count afterward. Filters are body-only: any query key is rejected with 400 before the body is read. |
| `POST /admin/purge` | Delete matching finalized records; genuinely no body means all. An empty/invalid supplied object is rejected. Filters are body-only: any query key is rejected with 400 before the body is read, so `?provider=...` can never broaden the no-body full delete. |
| `GET /admin/backup` | Download the self-checked `.mvb` Settings backup archive of config and/or database members. `?config=1` and `?database=1` select members, defaulting to both when no key is given; an explicit empty selection is an error. The query grammar is strict: parse errors, repeated or unknown keys, and malformed values all return 400. |
| `POST /admin/restore` | Restore an uploaded `.mvb` Settings backup archive, validated before anything is applied. `?inspect=1` previews the archive (created time, member sizes, config keys vs default and live, database request span) without applying anything. `?config=1` and `?database=1` select members as on backup; `?config_mode=` and `?database_mode=` take merge or replace and default to replace when omitted; an explicit empty or whitespace value is rejected as malformed. Success reports the `restart_required` keys. The query grammar is strict: parse errors, repeated or unknown keys, and malformed values all return 400. |
| `GET /metrics/export` | Download all or exactly filtered finalized records as a gzip artifact named `millivolt-logs-<timestamp>.json.gz`; gunzip it for the JSON array. Debug-only export can contain sensitive sidecars. |
| `GET /metrics/query?q=` | Restricted SELECT with timeout/output limits; 503 when durable storage is disabled. Not a hostile-query sandbox. |
| `GET /metrics/bootstrap` | Dashboard state and full/incremental recent-record snapshot. |
| `GET /metrics/live/stream` | Replayable finalized SSE feed plus ephemeral pending lifecycle/reset events. |
| `GET /metrics/agg/chart`, `/explorer`, `/log` | Scoped history chart, faceted explorer, and durable log paging. |
| `GET /metrics/prometheus` | Prometheus exposition over the in-memory ring, not durable since-inception dashboard totals. |
| `GET /metrics/pprof/` | Standard Go profiling handlers behind the same credential as every `/metrics` route: the index listing plus the `profile` (CPU), `heap`, `allocs`, `goroutine`, `block`, `mutex` and `threadcreate` profiles at `/metrics/pprof/<name>`. Unregistered subpaths answer the reserved namespace 404, never inference. See the capture workflow below the route table. |
| `POST /mcp` | Streamable HTTP MCP endpoint, operator-gated like `/metrics` and enabled by default: `mcp_enabled: false` removes it from the process (the route stays reserved, answering 404 and never inference; restart to apply). Serves the 22 MCP tools. Bearer-only: the gate admits the session cookie on gated paths, but this endpoint refuses a cookie-only request (`403`) and forwards the caller's own Bearer on its in-process calls to the routes above, so no session cookie or per-request state is retained between requests; the admitted credential keys an in-memory cache of its SDK server object (never logged or persisted) so repeated calls do not rebuild it. Stateless: `GET`/`DELETE` answer `405`. `/mcp/...` look-alikes are reserved and answer `404`, never inference, and a reserved-namespace spelling whose separator is written single-encoded (`%2f` or `%5c`, any hex case) is refused the same way. A double-encoded separator (`%252f`) is not a reserved-namespace spelling: it decodes to a literal `%2f` inside one segment, so it can reach neither the MCP handler nor a reserved namespace and is ordinary inference traffic. |

Pause/Debug/Limits successes may include a persistence warning: runtime state
was applied, but saving it failed. Do not retry as though the mutation rolled
back. With storage disabled, Pause and Limits keep their memory-only state
intentionally. Debug is the exception: starting a session answers 503 because
with no store every matched request would retain bodies for a sidecar that
could never be fetched.

The profiling subtree is operator-gated like every `/metrics` route: an
unarmed plane answers 403, a wrong credential 401, and a reserved-namespace
spelling with an encoded separator answers the reserved 404. `go tool pprof`
cannot send an `Authorization` header, so fetch the profile with curl and
analyze the file against a locally built binary. Against the dev instance
([`scripts/dev.sh`](../scripts/dev.sh), loopback `127.0.0.1:8081`):

```sh
go build -o /tmp/millivolt-profiling ./cmd/proxy
curl -fsS -H "Authorization: Bearer $MILLIVOLT_OPERATOR_TOKEN" \
  -o /tmp/cpu.pprof 'http://127.0.0.1:8081/metrics/pprof/profile?seconds=30'
go tool pprof /tmp/millivolt-profiling /tmp/cpu.pprof
```

The `seconds=N` parameter on `/metrics/pprof/profile` is the capture window:
that one request stays open for the whole window while samples accumulate. A
process runs one CPU profile at a time, so a second concurrent `profile`
request fails with 500 until the first finishes. On `/metrics/pprof/heap`,
`gc=1` forces a garbage collection cycle before the sample.

The proxy's own `/mcp` endpoint serves this whole surface to an LLM, using the
same credential and the same routes. It adds no route of its own and dispatches
each tool call in process through the same gate, so its forwarded Bearer is
re-validated exactly like an external request. For its irreversible call, the
operator credential and the tool's own guards are the entire gate: an empty
purge filter, a purge without the exact confirmation phrase, and a purge
without the `purge_preview` token issued for that same filter are each refused.
It also refuses a capture session on a proxy without durable storage, and an
unknown name in a capture scope or a throttle provider is offered the known
values instead. The throttle provider check fails open when the proxy's
vocabulary is empty or cannot be read; the capture scope check accepts any name
when the vocabulary is empty but refuses when the vocabulary cannot be read.
It never follows a redirect, and it presents the credential only in the
`Authorization` header. See the [MCP server guide](mcp.md).

### Operator access

`MILLIVOLT_OPERATOR_TOKEN` is the single operator credential for the whole
dashboard. It is read once at boot from the process environment, never from
proxy.yaml, so it cannot leak through Settings, `-print-config`,
`/admin/config` output, snapshots or logs.

The gate is deny by default and lives in one chokepoint in front of every
route. This is the canonical open-path list; [Security](../SECURITY.md) and the
[reverse-proxy guide](reverse-proxy.md) summarize it instead of maintaining a
second list.

- Open: `GET /healthz`, the `POST /session` login handshake (the one open
  operator route), origin-root brand/PWA files (`/favicon.ico`, icons,
  `/manifest.webmanifest`, `/sw.js`) and transparent inference. Provider
  credentials ride the same header name and are never inspected by the gate.
- Gated: the dashboard HTML and `/dash/*` assets, every `/metrics/*` surface
  and every `/admin/*` route, including unclassified future admin paths and
  unsupported methods, plus the `/mcp` endpoint and its reserved `/mcp/*`
  look-alikes. A gated MCP request also needs its credential as a Bearer
  header: the session cookie alone is refused there (`403`), because the
  caller's credential is forwarded on the endpoint's internal calls.

Present the credential as `Authorization: Bearer <value>`. A valid Bearer
request also mints a session cookie, because the dashboard's live feed uses
EventSource, which cannot send Authorization headers. The cookie is HttpOnly
(script never reads it), SameSite Strict, scoped to 12 hours, and signed with
a key derived from the operator token, so a container restart with the same
credential keeps the session. Rotating the token invalidates outstanding
cookies. The cookie deliberately carries no `Secure` flag:
millivolt's listener is plain HTTP, and the documented TLS deployments
terminate at the ingress, where the remaining loopback hop is trusted-local.
Do not expose the listener over plaintext networks anyway. The login page
served for unauthenticated dashboard visits exchanges the entered value for
that cookie through `POST /session` and works without JavaScript; API
clients can call the same endpoint or simply send the Bearer header on every
request. An expired cookie re-prompts in the dashboard or reappears as the
login page on navigation.
Unregistered `/admin/*`, `/session/*`, `/metrics/*` and `/mcp/*` paths are reserved: they
answer 404 and are never forwarded upstream. The gate also refuses 404 when an
owned-namespace path is written with a percent-encoded separator (`%2f` or
`%5c`, any hex case): the mux cannot route it to the reserved subtree, so it
would otherwise fall through to inference. A double-encoded separator
(`%252f`) is not a reserved-namespace spelling: it decodes to a literal `%2f`
inside one segment, matches no reserved route, and is ordinary inference
traffic.

Failed authentications are throttled per source IP (RemoteAddr only;
forwarded-header chains are never trusted, since no trusted-proxy model
exists): after five rejected credentials the IP is locked out with a window
that doubles from 10 seconds to a 15 minute ceiling. The throttle counts only
presented-but-wrong credentials, so an expired-cookie dashboard polling in a
background tab cannot lock its own operator out, and a verified credential is
always admission: presenting the correct Bearer during a lockout clears the
budget, because the lockout exists to rate-limit guessing, not to lock the
operator out of their own dashboard. The map is hard-capped at 4096 source
IPs with idle expiry, so spoofed-address floods cannot grow it. A lockout
response is `429` with a coarse rounded `Retry-After` and a generic body.

With the variable unset or empty the deny-all state holds: the whole dashboard
returns 403, the open `POST /session` handshake cannot mint a cookie, and only
`/healthz`, brand/PWA files and inference respond. A nonempty value shorter than 16
or longer than 512 characters fails the boot instead of silently weakening
the gate. The credential is process bound: config reload does not re-read it,
so rotating the value requires a real process restart (a supervisor restart
or a fresh deployment; the dashboard's restart handoff inherits the parent
environment, so it keeps the old credential by design). The dashboard asks
for the value once per browser tab, keeps it in sessionStorage, attaches it
to gated calls only and drops it as soon as the server rejects it.
Non-browser callers send the header directly, for example
`curl -H "Authorization: Bearer $MILLIVOLT_OPERATOR_TOKEN" -XPOST .../admin/purge`.
Environment variables are readable by same-user processes through `/proc`,
so run the proxy under a dedicated account.

### Pause and Limits

Pause creates holds; it does not disconnect already-running sends. Matching new
sends and retries wait until the hold expires or is resumed. Choose all traffic,
named clients/providers, or clients that were unseen when a New-clients hold was
created. "New" does not mean new requests or new conversations. Multiple names
within a dimension are alternatives; provider and client constraints intersect.
When named clients and New are combined, either client condition may match.

A hold has its own duration and queue cap; the cap defaults to `max_queue_size`
when the hold does not set one. Excess waiting work can be refused;
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

Operator limit edits persist synchronously and report a warning when the store
rejects the write. Header-driven limit changes apply in memory immediately and
are persisted by one coalescing background writer: a burst of changes becomes
the latest whole-policy document, with bounded retry and backoff after a store
failure. A crash can therefore lose the most recent header-driven changes, and
a failed save never rejects inference. Graceful shutdown and restart flush the
latest policy before storage closes or the restarted process opens the
database. Provider labels are capped at 255 bytes and the persisted throttle
document at 8 MiB; normal provider labels derived from base URLs remain below
both bounds.

Admission is bounded end to end. `max_inflight_requests` is the process-wide
inference ceiling (default 10000), `max_inflight_body_bytes` its buffered
body budget (default 256 MiB), and `max_inflight_response_bytes` its budget
for buffered upstream response bodies (default 256 MiB): whole-document
translation of non-streaming responses, the non-streaming 200 quality spool
(at most 1 MiB plus one 32 KiB chunk), and model discovery. Requests above
any bound are refused with `503` and `Retry-After` before the body or upstream
response is read. Request-body reads must make progress within
`request_body_idle_timeout` and finish within the absolute
`request_body_read_timeout`, so a stalled peer releases its reservation
without cutting off a slow but progressing upload. Buffered whole-document
response reads use `whole_document_read_timeout` when the send has no deadline
and `whole_document_read_max` as the ceiling even when `X-Proxy-Timeout-Ms` is
longer, so a slow or stalled body releases its response-buffer reservation; a
shorter caller deadline wins and configuration validation preserves the
`models_discovery_timeout` budget below the ceiling. The three admission
budgets accept `0` = unlimited and hot-reload.
`max_queue_size` (default 1024
per group) and `max_queue_wait` (default 10m) bound a held or saturated
provider's parked work; both accept explicit `0` = unlimited. Independent
per-key/header concurrency remains unlimited by default. The scheduler retains
at most 100000 distinct provider+key groups, evicting the least-recently-idle
group (no in-flight or queued work) to admit a new key at the bound; a new key
is refused only when every retained group has active work, so a table full of
active groups still fails closed. Recent pacing/backoff state for evicted
groups is kept in a separate 4096-entry bounded LRU. The scheduler also
retains at most 4096 header-created provider limits, refusing new ones past
that bound rather than growing process memory without limit. A new request
body has the `request_body_read_timeout` budget, SSE writes have the
`sse_write_progress_timeout` per-event progress bound, and an upstream body
that stops sending for `upstream_stream_idle_timeout` is canceled.

Request and token budgets start full when enabled and refill continuously at
the configured count/window rate. Request budget is charged once per proxy
admission, not again for each absorbed upstream retry. Token admission reserves
an estimate from captured prompt characters divided by four plus a positive
captured output limit, then settles against final reported usage. A reservation
larger than the bucket's entire capacity can proceed while credit is positive,
putting the bucket into debt that refill or usage settlement can repay. These
controls permit bursts and oversized requests; they are not strict fixed-window
provider quota enforcement.

### Error storm protection

Open Settings, select **Error storm protection**, and turn on **Enable
protection**. It is opt-in in both neutral defaults and the deployment example.
Choose the rolling window, minimum samples and failure threshold, then select
HTTP failure statuses, transient transport failures and response/stream failures.
Provider protection requires all models receiving requests in that same window
to meet the thresholds individually. A single affected model is held on its own
while other active models remain healthy. If only one model is active, that
model can establish a provider-wide incident. Provider and model protection can
also be enabled independently.

Configure initial/maximum recovery delays, multiplier, jitter, recovery successes,
additional retries, queue capacity, per-wait timeout and retained scope capacity
in that category. The generated [example](../proxy.example.yaml) supplies every
type, range and default. Existing Queue & retry limits and provider Limits still
apply; neither `storm_max_wait` nor a per-send deadline is a total request timeout.
Use a client timeout appropriate to the desired end-to-end wait.

Each clickable banner identifies its current scope and safe failure
category (HTTP status, transport error or response error), percentage/count of
failed upstream attempts, detection window, queued callers and next probe or
recovery progress. Open an incident to inspect the distinct requests that
encountered a selected failure, failed attempts, total sampled attempts, error
category, queue count and next retry time. Provider entries also show affected
versus active models. Distinct request counts follow each request's latest selected failure in the
rolling window, so repeated retries do not inflate that count. The percentage
still describes upstream attempts. These are process-local observations, refreshed
through the normal dashboard bootstrap/poll cadence; they are not chart history or the
Errors chart's distinct-request percentage. Scope counts can overlap where a
caller waits on both provider and model protection. Hide the banner independently
without disabling protection.

Changing a storm policy resets its samples/incidents and wakes waiters to
re-evaluate the new policy. Banner visibility alone preserves detector state.
A reset does not cancel work already sent or erase request history. Restart
starts detection afresh, and queued requests do not persist. History deletion
does not reset live outage protection.

Storage stays outside the detector: fixed 64 time buckets retain counters per
scope, with oldest-edge precision within approximately `storm_window / 64`.
`storm_max_scopes` includes provider aggregates and exact provider/model scopes;
stale unreferenced healthy scopes can be evicted, while active or referenced
scopes remain protected. Scope identifiers over the internal 512-byte retention
bound and exhausted scope capacity reject admission rather than silently bypass
protection. These bounds are not a total heap ceiling: waiting callers retain
bounded request bodies, and existing ordinary queues remain separately sized.
The [protocol guide](protocol.md#error-storm-protection) defines retry safety,
quota exclusions, native handshakes and client-facing failures.

### Quota pause

Open Settings, select **Quota pause**, and choose the reaction to a 429 whose
error envelope is a durable account condition - `insufficient_quota`,
`insufficient_credits`, spend/hard limits, the recognized numeric billing codes
(the classification is fixed; only the reaction is configurable):

- **off** (default): the 429 surfaces to the client immediately. Nothing is
  parked or retried.
- **retry**: a provider-wide recovery gate opens on the first such 429 from an
  OpenAI-wire provider. The triggering request absorbs the 429 and re-sends
  per the error storm recovery delays; every new request to the provider
  parks on the gate (bounded by `storm_max_queue` and `storm_max_wait`); the
  gate closes after `quota_pause_recovery_successes` consecutive successful
  probes (the default 1 closes on the first 2xx) and the queue drains. A provider
  `Retry-After` on the quota 429 paces the next probe like any other 429.
- **manual**: an indefinite provider-scoped pause (an operator hold labeled
  with the quota class) is created automatically; every request, triggering
  one included, queues until an operator resumes it on the pause surface.
  When a pre-existing hold already covers the provider, no duplicate is
  created: the triggering request queues under that hold only if it parks
  the triggering client too, and a hold that parks other clients only lets
  the triggering request surface the 429 immediately.

The retry-mode gate shares the storm scheduler's parking, probes and pacing,
so storm recovery settings configure it without storm protection being
enabled. It surfaces in the storm banner as a quota incident with a
**Resume now** action (`POST /admin/quota`); manual-mode holds appear on the
pause surface with their reason and resume like any hold. A quota 429 is
never marked as a rate limit, and quota re-sends never burn the transient
`max_retries` budget. The retry gate is in-memory - a restart drops it and
the provider's next durable quota 429 re-arms it - while manual-mode holds
persist like every operator pause. In-band occurrences (the same envelope
inside a 200 stream) arm the pause the same way; that request already
answered, so it is never parked. Translated Anthropic-format and Cursor
upstreams keep the off behavior in every mode.

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
bytes. Oversized captures are marked truncated. `debug_store_max_bytes`
(default 512 MiB) caps the whole capture sidecar; once the next write would
exceed it, new captures are skipped and TTL expiry frees capacity again.
Capture writes are counted in bytes, not characters. The request drawer loads an
available sidecar only for a captured request; expired/absent captures are not
recreated from ordinary metrics. Completion is published before the sidecar is
encoded and saved, so a slow sidecar write delays neither the live end nor
durable accounting. A drawer fetch that follows the live end waits for that
request's in-flight save instead of answering a transient 404; a disconnected
client stops waiting.

Starting a capture requires durable storage. With `db_path` empty,
`enabled:true` without an ID answers 503 `durable storage is disabled` and
creates no session. A store-less process
also has no persisted session document to restore, so it never starts with an
enabled session; if one were present in memory, the request path still refuses
to build a capture tap, so no headers or bodies are retained for an unfetchable
sidecar. Editing or stopping a named existing session remains available.

Retained request headers use the same policy as recorded response headers:
names are visited in sorted order for the budget, each value is capped at 4 KiB
and marked with `…` when cut, at most 16 values are kept per name, ordinary
headers share a 16 KiB total budget per header map, a header name is capped at
512 bytes, and at most 256 names are retained per map. A longer name or an extra
name is omitted without a marker, exactly like a value beyond the budget.
Credential headers (the
configured auth header, `Authorization`, cookies, and token-, API-key- or
secret-bearing names) become `[REDACTED]` before any truncation, so a cut
value is never a credential prefix. Headers or values beyond the budget are
omitted without a marker; the capture schema has no header-truncation field.
The capture JSON is encoded without HTML escaping, so `<`, `>` and `&` appear
literally while JSON control escapes remain.

The drawer's Debug section also carries a Download button. It fetches the same
capture with `download=1` and saves a gzip artifact named
`millivolt-debug-<captured_at>.json.gz`, using the record id in the name when
the capture does not carry a readable `captured_at`.

`capture_body_preview` is a different opt-in feature: it retains short prompt/
response previews in the normal record without starting a Debug session. The
two features also retain different request bytes: on the passthrough wire
the record's prompt preview is the client's original body, captured at the
metadata decode before `request_overrides` or the sub-conversations strip
rewrites it, while a debug capture's request body is the rewritten upstream
body sent after any rewrite. A translated Chat Completions or Anthropic target
re-decodes the translated body for its prompt preview; a translated Responses
target keeps the client-body prompt preview, because the generated item list is
not re-decoded for preview. The response preview follows the client-facing
response content on every target. Both features can retain sensitive content
even when credential headers are redacted.
Exports/backups can outlive the configured retention period. See
[Security](../SECURITY.md#upstream-destinations-and-credentials).

### Logs and deletion

Logs downloads finalized request records as JSON, not the process's stderr log
and not a complete administrative audit trail. With durable storage it reads the
database; otherwise it uses available ring records. Open the menu, choose a
filter and review its matching count before Download matching, or deliberately
choose Download all. A debug-only export can include retained capture sidecars.

The download is always a gzip artifact named `millivolt-logs-<timestamp>.json.gz`;
gunzip it to read the JSON array of records. Compression is applied at the
export writer itself, so every export shape (filtered, all or debug-only,
from the ring or durable storage) downloads compressed the same way. This is a
breaking change for scripts that consumed the plain JSON download: there is
no uncompressed fallback, so scripted consumers must decompress the artifact
first.

A durable export holds one read snapshot for the whole stream, so the server
bounds it with an internal total budget. If the budget expires after bytes were
written, the download is aborted mid-artifact instead of completing as a
successful gzip: the saved file is partial and must be discarded. A caller
cancellation or shorter caller deadline is reported separately from this
server budget.

In the dashboard, both database deletions live in Settings, in the storage
category, and nowhere else. Delete matching uses the same filter/count owner
as the export, and its scope is the chosen filter; Delete everything wipes
the whole database. Deletion requires confirmation in the dashboard; it does
not happen when the sheet opens. A selected filter makes a read-only
`POST /admin/purge/count` preview. Current traffic can change the count
before the action, but the UI retains the previewed age cutoff rather than
silently moving it. The filter selections reset every time the sheet
refills, so a stale invisible filter never arms a destructive default.
Both actions are irreversible without a backup.

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
Tools and error signatures can each have multiple memberships per request.
Tool cards count affected requests for N and share, with request-level card
metrics counted once per affected request; their calls KPI separately sums
request ToolCalls, so repeated same-name calls do not inflate the request count.
A request using several tools contributes to each tool card. Error/429 health
badges and every gallery share use distinct affected requests.

The [visual guide](dashboard.md) covers every timeline and its controls. Overview
is the summary: it carries no plot, and its tiles take the whole card (requests
over the blended token volume, tokens in/out/cached with their shares, spend
with the blended per-Mtok price, the errors / 429 pair, and average latency /
speed over every measured request with each metric's low-high sample range);
Traffic compares requests/errors; Tokens separates available input/output/reasoning/cache
usage and tracks the cache-hit share of input on a percentage axis; Speed + latency
plots throughput on its own axis with TTFT and the decode window (first token to
last, the span decode speed measures) sharing a duration axis; Errors compares
error count/rate; Cost places reported USD spend beside request volume. One
percentile selection controls the timing series (the Overview keeps only the
time-range control). Period readouts are computed from period samples, not
averages of bucket percentiles. Insufficient samples
remain unavailable. All time means the available retained history, not deleted,
dropped or never-observed requests.

### Request rows and details

The log starts with recent rows and pages older durable records as you scroll.
Live rows show in-flight/queue/retry progress; final rows retain their outcomes.
Retry disclosure opens absorbed attempts under the original request, not another
inference record. Click the detail arrow or a non-link row cell to open its
drawer; previous/next controls navigate requests without changing the scope.

The drawer presents available facts in sections: request identity/status/times;
client/runtime and provider metadata; captured model and parameters; conversation
turns and content-size counts; input/output/cache/reasoning tokens; performance,
cost and finish state; tool names/calls; queue/rate-limit information; retries,
errors and retained headers. Recognized parameters include sampling and output
limits, stop/logprob controls, reasoning/verbosity, response format, tool choice
and streaming flags. Explicit zero/false parameters remain distinct from absent
ones. Ordinary HTTP relay metadata does not infer missing parameter defaults.
Adapters differ: Anthropic metadata comes from the translated body, including
its configured output-limit default, and Cursor stores the base model ID rather
than its fused spelling. See [adapters](adapters.md) before treating these fields
as an exact representation of the caller's original request.

Opening ordinary details does not fetch private request content. Prompt/response
previews appear only if captured, and Debug sidecars use their separate opt-in
and retention contract. Even ordinary labels, error text and headers can be
sensitive; do not publish request drawers from private history without review.

### Reading timing, cost and health

Request duration starts after upload and includes metadata work, queue/hold/retry
waits and response handling. TTFT is measured from the upstream attempt's own
send start to the first token (reasoning or answer), so every retry has its own
TTFT and the value excludes queueing, request parsing and retry backoff. A
request that produced no token, and any non-streamed response, has no TTFT: the
log shows a placeholder, not 0 ms, and aggregates skip it. TPS is the single
generation-speed measure: (generation tokens - 1) over the first-token to
last-token window. Average TTFT and average TPS are the arithmetic means of
those per-request samples. TPS is absent for fewer than two tokens, a
non-positive window, or a response that did not stream (no token timestamps).
The compact surfaces (the KPI band and the Overview timing tile) abbreviate
this unit as `t/s`; roomier readouts spell it `tok/s`.
The numerator assumes the first observed content chunk carries one token; a provider that bundles several tokens into that chunk can
still overstate the rate. These are provider-attempt measurements, not claims
about total user wait or system-wide throughput.

Money stays USD in storage and APIs. The shared UI formatter uses cents below
$1, including fractional cents, and dollars for larger magnitudes. Usage/cost
comes from recognized upstream fields or configured field mappings; unavailable
reporting and a displayed zero do not establish a zero bill. Reasoning/cache
splits depend on what the upstream actually reports. When a stream carries no
usage object, or carries one whose output count is zero, the recorded output
token count is restored from the streamed chunk count, which includes
tool-argument fragments; the count is an estimate, not a provider report. Chat
tool calls are deduplicated by delta index while the retained index set holds
up to 1,024 distinct indexes. Once the set is full a further index cannot be
retained, so each of its fragments counts again; a hostile stream of repeated
indexes can overstate the tool-call count without growing memory past the
bound.

Errors count affected requests with a genuine failure, including recovered
upstream failures. HTTP 429 counts requests that encountered a final or retried
429; the two signals can overlap, but 429 alone is not an error. Client-local
cancellation is likewise distinct from an upstream failure - except a 499 whose
record carries a structured error type: that observed the provider's in-band
stream failure before the client left (typically an error event the client
received and aborted around), and counts as the provider error it is. Zero
explorer health badges are hidden independently. Error-dimension card
headlines count failure events (final plus absorbed), while card N counts
distinct affected requests. The gallery share denominator `error_total` is the
union of affected requests represented by the error cards, so a card's share
is N / `error_total` and can never exceed 100%. This denominator includes
recovered quality failures whose final status is not an error (for example an
absorbed empty-completion attempt), counting each affected request once even
when no request ends in an error status. For every other dimension
`error_total` remains the canonical affected-request error count. Do not treat
every displayed error number as the same metric.

The explorer gallery renders at most 24 cards. Its fold tracks exact
per-value accumulators for at most 8192 distinct values in the active
dimension; later values are combined into a synthetic `other` card with
aggregate count, cost, token, tool and health fields, competing for the 24
cards by that count. The synthetic card carries `synthetic: true` on the wire,
renders as `other (grouped)`, and is not filterable. A real value named `other`
keeps its own exact card and filter; `other` is a display bucket only when the
card is synthetic. Rail distinct-value counts, footer scope totals and
conversation summaries still cover the whole filtered history, so card counts
need not add up to them.

### Snapshots, live feeds and monitoring

The HTML includes the canonical bootstrap state so the first paint need not wait
for a second state request. Live SSE carries pending progress and finalization;
KPI/operator polling and configured aggregate refresh keep derived surfaces current.
Reconnects use sequence/feed identity and fall back to full snapshots when replay
cannot be trusted. The footer distinguishes live/offline, active holds/Debug,
process-local storage drops and totals degraded by a failed startup history
scan. An offline indicator alone does not prove inference
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
An unchanged finalized ring revision serves the exact last exposition without
another ring copy, aggregate or TTFT sort; any ring content transition rebuilds.

### Operator recipes

The gated query route (`GET /metrics/query?q=`, above) and the [MCP](mcp.md)
`query` tool both take exactly one `SELECT` with no statement separator, and
results are bounded by `storage_query_max_rows` and
`storage_query_max_bytes`; an over-budget result is an error on the route, so
keep wide scans behind a `started_at` window or a `LIMIT`. Column meanings
come from the MCP `describe` tool. `started_at` is Unix milliseconds: a
relative window is `started_at >= strftime('%s','now')*1000 - 3600000`, and
calendar math divides by 1000 and applies SQLite's `unixepoch` modifier,
with `localtime` for the host's zone. The recipes below earn their keep in
the derived shapes no dashboard surface computes: the outlier ratio, the
write/read ratio, the latency decompositions. Their ingredient aggregates
are conceded up front, the way the decode window already is: the explorer
sums per-group counts and cache reads (its `N` and `Cache` fields) and a
per-group blended $/Mtok (its `cost_per_mtok` KPI, the same cost-reporting
in+out rule), and the chart's dec series samples the shared decode window
(`last_token_at - first_token_at`) that recipe 1 decomposes latency
around. Several of the columns a recipe reads do overlap the aggregate
scans: `ttft_ms`, `reasoning_tokens`, `status_code`, `attempts`,
`output_tokens` and `cache_read_tokens` all appear in the dashboard's scan
lists. The recipes' private reading set, which no aggregate scan reads, is
`queue_wait_ms`, `first_answer_at`, `retry_after_ms`,
`cache_write_tokens`, the `chars_*` columns, `total_tokens`,
`req_max_tokens`, `duration_ms` and `retries`. Each recipe is a
question, the SQL that answers it, and how to read the result.

**Where did the time go on a slow request?**

```sql
SELECT started_at, duration_ms, queue_wait_ms, retries,
       first_token_at - started_at AS pre_token_ms,
       ttft_ms,
       last_token_at - first_token_at AS decode_ms,
       started_at + duration_ms - last_token_at AS tail_ms
FROM requests
WHERE started_at >= strftime('%s','now')*1000 - 3600000
  AND first_token_at > 0
ORDER BY duration_ms DESC
LIMIT 20
```

`pre_token_ms` is the wait the caller felt, including queue and retry waits;
`ttft_ms` is measured from the adopted upstream attempt's send start, so a
retried request shows a small ttft beside a large `pre_token_ms`. `decode_ms` is
the streaming window and `tail_ms` what remains after the last token, where a
millisecond of rounding noise is normal. A request that produced no token, or
was not streamed, has nothing to decompose, hence the `first_token_at > 0` gate.

**How long did a model think before answering?**

```sql
SELECT started_at, model, reasoning_tokens,
       first_answer_at - first_token_at AS thinking_ms
FROM requests
WHERE started_at >= strftime('%s','now')*1000 - 3600000
  AND first_answer_at > 0
ORDER BY thinking_ms DESC
LIMIT 20
```

`thinking_ms` spans the first token of any kind, reasoning included, to the
first answer token; 0 means the first token already was the answer.
`first_answer_at` is streaming-only on the generic relay; cursor-format
non-streaming requests set it (the bridge synthesizes each delta and tracks
it via cursorTrackDelta), and such rows legitimately enter the gate because
`first_token_at` is stamped by the same helper. Tool-call-only streams keep
0 and stay outside the gate.

**How long does an agent idle between conversation turns?**

```sql
SELECT conversation_id, started_at,
       started_at - LAG(started_at + duration_ms)
         OVER (PARTITION BY conversation_id ORDER BY started_at) AS idle_ms
FROM requests
WHERE started_at >= strftime('%s','now')*1000 - 86400000
  AND conversation_id != ''
ORDER BY conversation_id, started_at
```

`idle_ms` is the gap between one request's end and the next request's start
inside the same conversation; the first turn has no predecessor and stays
null. Order by `idle_ms DESC` instead to surface the biggest stalls first.

**What retried before the client saw a response?**

```sql
SELECT r.started_at, r.id, r.retries, r.status_code AS final_status,
       j.value->>'status_code' AS attempt_status,
       j.value->>'at' AS attempt_at,
       j.value->>'retry_after_ms' AS retry_after_ms
FROM requests r, json_each(r.attempts) j
WHERE r.started_at >= strftime('%s','now')*1000 - 86400000
  AND json_array_length(r.attempts) > 0
ORDER BY r.started_at, j.key
```

One row per absorbed attempt, oldest first. The `json_array_length` gate is
required: `attempts` defaults to the JSON literal `null`, and `json_each`
over that literal still yields a row. `retry_after_ms` appears (nonzero)
when the provider sent a `Retry-After` header or a ratelimit-reset header
(`x-ratelimit-reset-requests`/`-tokens`, `anthropic-ratelimit-requests-reset`,
`anthropic-ratelimit-tokens-reset`), bounded by the proxy's hint ceiling.

**Did a conversation's cache writes pay for themselves?**

```sql
SELECT conversation_id, COUNT(*) AS requests,
       SUM(cache_write_tokens) AS cache_written,
       SUM(cache_read_tokens) AS cache_read,
       CASE WHEN SUM(cache_read_tokens) > 0
            THEN ROUND(1.0 * SUM(cache_write_tokens) / SUM(cache_read_tokens), 2)
       END AS write_per_read
FROM requests
WHERE conversation_id != ''
GROUP BY conversation_id
ORDER BY cache_written DESC, cache_read DESC
LIMIT 20
```

`write_per_read` at or below 1 means the reads repaid the writes; reads with
no writes ride a cache somebody else warmed. A conversation with writes and
no reads paid for a prompt that never came back.

**When does traffic arrive?**

```sql
SELECT strftime('%w', started_at/1000, 'unixepoch', 'localtime') AS weekday,
       strftime('%H', started_at/1000, 'unixepoch', 'localtime') AS hour,
       COUNT(*) AS requests, SUM(cost) AS cost,
       ROUND(AVG(duration_ms), 1) AS avg_ms
FROM requests
GROUP BY weekday, hour
ORDER BY weekday, hour
```

`weekday` is `0` (Sunday) through `6` and `hour` is two digits in the host's
local zone; drop `localtime` for UTC. Cost is USD, summed as stored.

**How far does the token estimate drift?**

```sql
SELECT model, COUNT(*) AS requests,
       ROUND(AVG((chars_system + chars_user + chars_assistant + chars_tool) / 4), 1) AS avg_chars_est,
       ROUND(AVG(input_tokens), 1) AS avg_input,
       ROUND(AVG(1.0 * input_tokens /
            ((chars_system + chars_user + chars_assistant + chars_tool) / 4)), 3) AS input_vs_chars4,
       ROUND(AVG(1.0 * total_tokens /
            ((chars_system + chars_user + chars_assistant + chars_tool) / 4 + req_max_tokens)), 3) AS total_vs_est
FROM requests
WHERE input_tokens > 0
  AND chars_system + chars_user + chars_assistant + chars_tool > 0
GROUP BY model
ORDER BY input_vs_chars4 DESC
```

The scheduler reserves prompt chars/4 plus `req_max_tokens` when the client
sent one. `input_vs_chars4` compares real input tokens with the chars/4
share, and `total_vs_est` compares settled total tokens with the full
reservation. A ratio far from 1 on a well-represented model means the
reservation systematically over- or under-reserves for it.

**Which requests paid far above their model's blended rate?**

```sql
SELECT o.model, o.id, o.started_at,
       ROUND(1e6 * o.cost / (o.input_tokens + o.output_tokens), 2) AS per_mtok,
       ROUND(1e6 * a.cost_sum / a.tok_sum, 2) AS model_avg_per_mtok,
       ROUND((1e6 * o.cost / (o.input_tokens + o.output_tokens)) /
             (1e6 * a.cost_sum / a.tok_sum), 2) AS ratio
FROM requests o
JOIN (SELECT model, SUM(cost) AS cost_sum,
             SUM(input_tokens + output_tokens) AS tok_sum
      FROM requests WHERE cost > 0 GROUP BY model) a ON a.model = o.model
WHERE o.cost > 0 AND o.input_tokens + o.output_tokens > 0
ORDER BY ratio DESC
LIMIT 10
```

`per_mtok` is the request's own rate over its in+out tokens, and `ratio`
scales it against the model's blended average, computed over cost-reporting
requests only so unpriced traffic cannot dilute it. A ratio far above 1
marks expensive outliers such as cache misses or long-tail pricing; flip the
sort to find the bargains. SQLite has no `median()`, which is why the
blended average plus a sorted ratio stands in for it.

## Storage and accounting

SQLite retains finalized records; the ring and pending registry support live
observation. Full bootstrap windows are capped in durable mode and older log
rows page from storage. Without storage, the ring is the available history.

The durable log order owns one secondary index, `idx_requests_log(started_at,
id)`. The log page pushes exact provider, client, key-hash, conversation and
canonical numeric status predicates into SQL and rechecks them on that scan;
model, status-class, tool, error and time-bucket filters stay Go-owned because
they match canonical/derived values, not raw stored text. The export and purge
filter surface is different: it accepts exact provider, model, client,
conversation, error type, status code, `has_error`, `debug`, and time bounds,
with no key-hash selector, and its equality-only preview count or purge scans
the table instead of maintaining per-dimension composite indexes.
`BenchmarkStoreDiskBytes` measures
this on its deterministic synthetic fixture (10,000 and 100,000 seeded
records); on that fixture the 100,000-row database fell from 118.2 MiB to
45.4 MiB. It is a reproducible fixture scope, not a prediction of a
deployment's disk use or a capacity ceiling.

Dropping a superseded request-log index only moves its pages to SQLite's
freelist, so the first startup that finds and removes one also runs a one-time
`VACUUM` after the migration. Fresh and already-migrated databases skip it. A
`VACUUM` needs free temporary space up to roughly twice the database size; if
it fails for space, locking or interruption, startup logs a warning and
continues with every row intact, but the freed pages stay on the freelist.
Reclaim them manually during a maintenance window: stop the proxy, run
`sqlite3 /data/proxy.db 'VACUUM;'` (or any SQLite client) against `db_path`,
and confirm `PRAGMA freelist_count` returns 0. The automatic pass is never
retried on a later boot, so a failed attempt needs this manual step.

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
log/bootstrap, capture reads) and the ad-hoc query route. Startup projection
preload, the boot totals rebuild, an administrative purge, and a backup merge's
totals scan run under a separate internal ten-minute maintenance budget: they
legitimately read or rewrite full history and must not inherit the interactive
bound. The
maintenance budget still honors an earlier caller cancellation or deadline, and
its expiry fails the operation closed (a purge transaction rolls back). It is a
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
[Settings backup and restore](#settings-backup-and-restore). For image-only
deployments and both persistent volumes, use
[container backup and restore](#image-only-backup-and-restore).

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
startup burst. The projection's per-record retained footprint is measured
reproducibly by `BenchmarkProjectionFootprint` (retained and transient bytes per
record over a seeded scratch database) and `BenchmarkProjectionProcessRSS`
(process RSS after readiness), without a live instance; the
[performance workflow](../CONTRIBUTING.md#performance-evidence) owns the commands.

`go_memory_limit` is the operator's steady-state RSS lever: a soft cap on Go
runtime managed memory (heap, stacks, runtime structures) applied once at
startup. With the setting at 0, an operator `GOMEMLIMIT` wins and otherwise a
detected finite cgroup memory ceiling of at least 80MiB (v2 `memory.max` or v1
`memory.limit_in_bytes`) becomes a soft limit at 80% of that ceiling so a
container's own bound actually reaches the Go runtime; smaller ceilings are
ignored and, with no `GOMEMLIMIT`, the runtime stays unbounded. A set value
overrides `GOMEMLIMIT`, not `GOGC`, for the process lifetime. `GOGC` remains
independent. The limit is soft - exceeding it makes the collector
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
  the translated document under a fixed 32 MiB cap each. Exceeding either
  fails closed with 502 `response_too_large` before any client bytes; the cap
  is an internal safety constant, not a setting, and discovery budgets do not
  bound the path.
- General multi-line SSE accounting/native event assembly remains incomplete;
  some split terminal/content shapes can affect classification, not just metrics.
- SQL result limits do not bound every intermediate SQLite allocation.
- Automatically grouped sessions are not proof of agent ancestry. Explicit parent
  declarations and exact-leaf filtering are described in [protocol](protocol.md).
- Existing values lost by older storage formats cannot be reconstructed.
- Extreme numeric data can produce explicit accounting/encoding errors rather
  than invented saturated prices or token counts.
