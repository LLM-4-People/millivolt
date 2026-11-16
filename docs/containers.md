# Containers and data

This page covers the image, Compose operation, persistent volumes, backups,
versions and the restart lifecycle. The settings surface is in
[configuration](configuration.md).

> [!WARNING]
> This is an offline backup: stop callers first, allow active work to finish,
> and keep the service stopped while copying. Never copy an open SQLite database
> as plain files. Restore into a new project and fresh volumes, leaving the
> original deployment and backups untouched.

## Containers

The [Dockerfile](../Dockerfile) builds Linux `amd64` and `arm64` targets and runs
as UID/GID `65532:65532`, with no source tree, Go toolchain or shell in the
runtime image. JavaScript/CSS are embedded in the Go binary; Node and Python
are test tools, not runtime dependencies. The binary is stripped of debug and
symbol tables.

The final stage copies only that binary, dependency notices, the generated
example and prepared state directories onto the minimal distroless base.
The [Compose quickstart](../README.md#docker-compose) downloads the image-only
[compose.yaml](../compose.yaml). Normal deployment never builds from source.

Local builds use the separate
[development Compose](../CONTRIBUTING.md#container-development).
A registry tag is usable only after its publishing workflow succeeds.

### Compose operation

Keep the same Compose project name to reuse its named volumes. The normal file
names the project `millivolt`; an explicit `-p` overrides it.

The deployment inputs are:

- `MILLIVOLT_IMAGE` (image reference)
- `MILLIVOLT_PORT` (loopback host port)
- `MILLIVOLT_OPERATOR_TOKEN` (the operator credential, forwarded into the
  container environment only)
- `MILLIVOLT_STOP_GRACE_PERIOD` (container stop grace)
- `MILLIVOLT_MEM_LIMIT` (container memory ceiling, default `1g`)
- `MILLIVOLT_PIDS_LIMIT` (process/thread ceiling, default `512`)

Their defaults live in [compose.yaml](../compose.yaml), not server
configuration. Supply them consistently through the environment or an ignored
local `.env` file. `docker compose config` shows the resolved deployment.

The memory ceiling is also the source of the runtime's own soft limit: with
`go_memory_limit` at its `0B` default, a detected finite cgroup ceiling at or
above 80 MiB becomes a soft limit at 80% of itself, so the container bound
reaches the Go runtime instead of leaving it unbounded.

Raise the ceiling with `history_size`, request buffers, concurrency or observed
identity cardinality; a ceiling under 80 MiB is ignored by the runtime and
leaves only the hard OOM kill. `pids_limit` bounds process and thread
amplification, not goroutines.

The image carries a Dockerfile `HEALTHCHECK` that probes the unauthenticated
`/healthz` through the binary's `-healthcheck` mode, so `docker compose ps`
reports health without any operator credential. Both Compose files replace that
probe with the [readiness check](operations.md#health-and-readiness), which is
strictly stronger.

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

Existing volumes and host bind mounts must be owned by the runtime UID, or the
database file must already be mode `0600`. Read/write permission alone is not
sufficient: startup narrows a broader database mode to `0600` and fails closed
when it cannot, naming the file path, owner and current mode. A foreign-owned
`0600` database is left unchanged; a broader foreign-owned file must be pre-set
to `0600` or chown'd to `65532:65532`.

Bind mounts hide bundled files: supply a config yourself when using one, or
intentionally accept the application's missing-file defaults. Use private
directory permissions and mode `0600` for a supplied config; with rootless
Docker or user namespaces, account for the host UID mapping. Do not make
sensitive directories world-writable to work around ownership failures.

A first launch cannot prepare its own bind-mount host directories. When a
bind-mount target does not exist, Docker creates it as `root:root` mode `0755`,
which the runtime UID cannot write, and startup fails with SQLite's
`unable to open database file` error. The log names the database path, the
directory state and the runtime UID.

Pre-create the host `data/` and `config/` directories owned by `65532:65532`
before the first start, or `chown -R 65532:65532` them after a failed start,
then start the container again. Named volumes avoid this entirely: Docker seeds
them from the image with correct ownership.

Settings saves by creating a temporary file beside the resolved config and
atomically renaming it; a symlinked config path is followed to its final
target, so the link stays in place and the target receives the document. The
restore rollback publishes through the same writer, so a failed restore of a
symlinked config keeps the link too.

A read-only mount, or a bind mount of only the config file, cannot support that
save contract. Mount the whole writable directory, or deliberately use
read-only configuration and update it outside the dashboard. Do not mount the
committed example as a mutable runtime config.

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
must remain owned by runtime UID/GID `65532:65532` with mode `0600`; the restored
database is subject to the same ownership and mode check at boot.
Check Settings and retained history on the new private port before redirecting
clients. Save the printed project name and deployment inputs for later commands;
the example bypasses automatic `.env` loading during recovery.

Docker documents [tar-stream copying](https://docs.docker.com/reference/cli/docker/container/cp/)
and [Compose archive mode](https://docs.docker.com/reference/cli/docker/compose/cp/).
The repository's isolated container check exercises stopped-volume backup and
restore into fresh volumes, including settings, history and file permissions.
Source deployments can instead use the live SQLite
[online-backup helper](operations.md#storage-and-accounting) or the Settings archive
described next.

### Settings backup and restore

The dashboard Settings Backup category downloads and restores a self-checked
`.mvb` archive. Choose config, database, or both. The archive is a packed
SQLite snapshot (`VACUUM INTO`, so free pages are not copied) and/or the
schema YAML, then zstd-compressed with a content checksum and an outer SHA-256
of the uncompressed payload.

Encoding streams each member into the compressor while hashing and counting it,
writes the archive once into a staging file, and self-checks that file before
returning it; an archive that fails decode or semantic checks is never emitted,
so a produced backup is never a silent corrupt file.

The database member is read from the packed snapshot file in bounded chunks
instead of being copied whole into memory, so the download cost does not grow
with a heap copy of the database. A source file that changes size or cannot be
read fails the encode before an archive is emitted.

Restore streams the upload through the `backup_max_bytes` ceiling into one
private staging file and decodes it with the same header, zstd, SHA-256 and
member-framing checks.

When a live store can stage it, the database member is written straight into a
private file on the live database's volume instead of being materialized on the
heap; a restore that selects the database member without a store fails closed
before staging, and a member the request did not select is stream-discarded
after its framing checks.

Config bytes stay buffered within the same 16 MiB config-document bound
`Config.Validate` enforces on the canonical rendered YAML, so a config that
validates can never exceed what an archive may retain. A config member that
declares a larger size is rejected before its bytes are copied, and encoding
refuses to emit one, so an older archive whose config member exceeds that bound
fails restore. The bound covers only the config member; the database member
stays streamed under the payload ceiling.

Restore validates that hash, the zstd frame, member framing and config
`Validate()` before applying anything. A selected database member is decoded
into its private staging file first, then must pass `PRAGMA integrity_check`;
only that checked file is published as the pending snapshot, which restore then
preflights for admission before the config member is written.

Decode holds the uncompressed payload to the same `backup_max_bytes` ceiling as
the upload (and to the Schema maximum, whichever is lower), so a small
compressed file cannot expand past what the operator configured; an over-cap
payload answers 413 with the upload's wording and stages no pending snapshot.

Download lists how many live settings differ from default and how many stored
requests would be packed. Picking a file first inspects the archive: created
time, size, member sizes, each config key as default vs modified vs the live
file, and the database request span when a live store can stage the member.

Inspect loads the live file through the same strict, bounded reader used by
config reload and restore, not the boot repair path: an unknown nested field,
an extra YAML document, a non-regular file, or a file above the 16 MiB document
bound fails the inspect instead of comparing against a repaired or partial
view.

Inspect decodes and counts the database member in place and computes its
overlap with the live store without writing another copy. Without a live store
it reports `present:true`, the decoded `bytes`, `inspectable:false` and the
reason, and produces no request span.

Config **replace** loads and validates the backup, writes canonical YAML, and
preserves CLI-overridden `listen` and `db_path`. Config **merge** overlays only
keys that differ from `Default()` onto the live file, so live custom values
stay unless the backup also customized them. An omitted `config_mode` or
`database_mode` defaults to replace; an explicitly empty or whitespace value is
rejected as malformed rather than treated as absent.

Either way the restored config hot-applies through the same owner as a Settings
save, is reflected in `last_reload`, and reports restart-required keys.

Database **replace** streams the decoded file into a pending snapshot next to
`db_path`, syncs it, and atomically renames it into place; Open admits the
pending file only after it checks again, and the restore waits for process
restart.

A `db_path` that is a symlink is resolved to its real target first, so staging,
admission, the rollback set and the state marker all belong to the file SQLite
actually opened; the link stays intact and points at the replacement. A
combined merge compares the same resolved identities rather than the two
spellings.

Database **merge** attaches the decoded file read-only and inserts request ids
that are not already in the live store immediately (live rows win on conflict);
the attached snapshot rejects writes, while the live database stays writable
for the merge, and no restart is required. Its request rows, debug sidecars and
since-inception totals commit as one transaction, so a failed totals write
leaves no partial merge behind. Truncated or bit-flipped files are rejected.

The `backup_max_bytes` setting is the encoded-archive and the decoded-payload
ceiling alike; encoding enforces it while the stream is written, so an over-cap
archive is removed instead of partially served, an over-cap upload answers 413
before decoding, and a decoded payload over it answers 413 before staging a
snapshot.

Restore phases are ordered, not atomic. The database member runs first: a
replace stages the decoded file and preflights it with the same schema,
writer-INSERT and request-scan value checks boot runs before touching the live
database, bounded by the same maintenance budget as boot maintenance, and a
merge commits rows into the running store.

One restore owns the whole transaction from the first uploaded byte, so two
restores cannot stage and decode two full members on the database volume at
once; an inspect shares that serialization because it stages the same members.
Only then is the config written and hot-applied.

A failed stage, preflight, or merge leaves the config file and live snapshot
unchanged, and a failed config write or reload rolls the config back and
restores a replace's previous pending slot. A successful live merge has no
rollback: if the later config phase fails, the merged rows stay applied.

A crash between the database and config phases is likewise not atomic; the
pending snapshot or completed merge can be durable while the config file still
has its previous bytes.

Preserved `.incoming`,
`.incoming.invalid-<token>`, `.incoming.invalid-interrupted-<token>`,
`.previous`, `.snapshot-state`, and `.millivolt-restore-pending-*` files next to
`db_path` are boot-recovery evidence and may need operator inspection after such
a crash; do not remove them while a restore may have been in flight. Quarantine
names carry a unique token and keep their sidecars beside them, so repeated
rejections accumulate instead of overwriting the previous one.

The `.millivolt-restore-pending-*` copy is a hard link to the snapshot that was
queued before the restore; it is created before the successor replaces the slot,
so the pending queue is never empty, and a crash on either side leaves a
recoverable snapshot instead of an orphan that the 24 hour staging sweep
eventually deletes.

A volume without hard links (exFAT, vfat, and several FUSE or SMB mounts)
rejects `link(2)`; there the slot is moved aside instead, still without copying
the snapshot bytes, and a crash leaves only the preserved sibling, which boot
re-admits. When the stage fails before the successor replaces the slot the
restore drops the copy again, and when a boot finds a rejected successor or an
empty slot it renames the preserved copy back onto the slot and admits once, so
the queued snapshot is neither hidden nor swept.

A combined config and database merge answers 400 when the restored config's
effective `db_path` differs from the running store's file, because the rows
would be written to a database the next boot does not open; merge the database
separately or use database replace.

At boot, a pending snapshot is admitted only after the same schema, trigger,
writer-insert, request-scan value and capture-id preflight `Open` runs. The
trigger fence rejects every unowned trigger in the snapshot, not only those
attached to `requests`: a trigger body is arbitrary SQL, so one attached to
`meta` or `request_debug` can delete or rewrite request rows.

The capture-id fence rejects a `request_debug` id that is NULL, non-text or
blank: such a row can never be fetched by id or reached by the expiry sweep,
yet still counts against the debug byte budget. Restore merge applies the same
fence before it imports any row.

Recovery consults the durable `.snapshot-state` marker rather than inferring a
transition from the files present. A markerless rollback set from a pre-marker
release cannot be classified: when a live main exists, recovery keeps it and
removes the `.previous` debris.

Discarding a non-empty `.previous-wal` in that markerless state logs a startup
warning, because its committed frames cannot be distinguished from debris and
may be lost. Marked pending and committed recovery behavior is unchanged.

Both directions stage database-sized transient files in the live database's own
directory, so capacity follows the data volume rather than a hardened
deployment's small `/tmp` tmpfs. Budget roughly one compressed upload plus one
packed database copy during a restore, and for a replace also the pending copy
at `db_path`.

The download's `VACUUM INTO` snapshot directory is removed after the archive is
encoded, including on failure or a client disconnect. A restore removes its
private upload file and decoded database directory after inspect or apply on
every path: a normal response, an invalid or over-cap archive, a validation or
apply failure, or a client disconnect. A config-only upload may use the process
temp directory because it carries no database-sized member.

A packed file already on the staging volume (VACUUM dest, decoded member,
pending `.incoming`) is integrity-checked in place, and inspect, overlap,
replace and merge consume it by path, so none writes a second database-sized
copy. A container's `/tmp` is a small tmpfs by design and never caps backup
size.

## Versions and images

[VERSION](../VERSION) is the application release source. The dashboard asset
fingerprint is separate: it detects changed embedded frontend bytes, not a
semantic release or a complete backend change.

`main` is promotion-only. Full source, browser and live MCP integration, and
native container checks run on all `testing` pull requests, code-bearing
`testing` pushes, manually dispatched `testing` gates, and manually pushed `v*`
tags; docs and site-only pushes use `docs.yml`. They also run on Dependabot's
`main` security PRs.

An ordinary promotion pull request from `testing` requires its head to be the
current `testing` tip, `main` to be already merged back into `testing`, and a
successful full-gate record for that exact commit: the canonical `check.yml`
push run's own check suite, with `unit`, `browser` and both native container
legs.

When a final docs-only commit follows the closest gated `testing` commit, it
reuses that gate if every path since it is docs/site-only; the fallback selects
the closest gated testing commit from canonical `testing` push runs, with no
commit-count bound. The proof polls pending exact check runs a bounded number of
times and fails closed if they are still pending.

For a code-bearing head, let the `testing` `check.yml` push gate finish before
opening or re-running a promotion PR. A docs/site-only head has no canonical
`check.yml` push run of its own, so wait for `docs.yml` or only the `required`
docs validation. It does not rerun the suite.

A code-bearing promotion must also have a `VERSION` newer than every existing
release tag. Docs/site-only promotions skip the version and full-gate proofs,
validate docs, workflow policy and the site in `required` within its ten-minute
budget, and do not rebuild images or cut a release.

Merge with a merge commit. The merged-PR release workflow tags that reviewed
merge commit and publishes the image; a delivered code-bearing main push is an
idempotent fallback. It never pushes `main`.

Dependabot security pull requests are the only other exception and normally do
not cut a release by themselves; an untagged pending `VERSION` can publish. A
manually pushed `v*` tag ships exactly as tagged.

If the version tag already points at the released commit but its GitHub release
is missing, the publish job fails closed with a repair instruction instead of
reporting a successful re-run. Repair the existing tag's release after verifying
the image; the job never moves a tag or republishes images. The full maintainer
checklist lives in [Contributing](../CONTRIBUTING.md#branch-workflow).

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
security PRs. The `unit`, `browser` and `container` jobs start together. Docs
and site-only pushes use `docs.yml` instead.

A normal promotion PR proves the current testing tip, merged-back main and the
full-gate record for that commit, or for the closest gated testing commit with
only docs changes after it, and requires a successful
[escalation](../.github/workflows/escalation.yml) run (every Go test under the
race detector plus a five-run clock-sensitive flake sweep) for the reviewed
head; docs-only promotions validate the docs, workflow policy and site
directly.

The merged promotion PR carries the reviewed merge commit and releases it; a
delivered code-bearing `main` push only reuses the same reviewed merge and is
idempotent. Publication is limited to this repository's merged promotion pull
requests, delivered pushes to `main`, matching release tags, and
`testing`-branch pushes (which publish only the moving `testing` tag).

The release `publish` job waits for `required`. The `publish-testing` job waits
for `unit`, `browser` and `container` instead, because a testing push has no
promotion proof to wait for.

A release run whose `VERSION` is already tagged, such as a Dependabot security
merge, normally does not publish; if main carries an untagged pending `VERSION`,
that run can publish it. Open or unmerged pull requests do not publish images; a
merged promotion PR does.

| Image tag | Meaning |
| --- | --- |
| `main` | Moving production image for the released current `main` revision; updated by a non-prerelease versioned release cut from a merged promotion PR or a delivered main push. A prerelease does not move it, and a manually pushed release tag does not either. |
| `testing` | Moving development image after a successful testing-branch push; never a release. Its OCI version label reports the embedded `VERSION`, which can lag the latest release. |
| `sha-<full commit>` | Release tag for the exact cut revision built and published; the version bump is part of the reviewed promotion merge, so this is a main commit. Testing pushes do not publish it, while a manually pushed tag publishes it for the tagged revision. |
| Version from `VERSION` | Created by the matching `v`-prefixed release tag on the promotion merge commit, or by a manually pushed tag. |
| `latest` | Moving stable alias updated by every non-prerelease release, including a release cut by a merged promotion PR, a main push or a manually pushed tag. Prereleases and testing pushes do not update it. A tag push moves the version, `sha-` and `latest` tags only; `main` stays where the last main-branch release put it. |

Release builds request an SBOM and max-mode provenance. The image and its
attestations are published before the tag and release are updated; the workflow
never pushes `main`. A failed image build does not leave a public release
pointing at a missing image. Verify
the published manifest and attestations with `docker buildx imagetools inspect`,
verify the repository-associated artifact attestation with
`gh attestation verify oci://ghcr.io/llm-4-people/millivolt:<version> -R LLM-4-People/millivolt`,
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
the working tree exactly as it is: uncommitted, unstaged and untracked files are
compiled, so a restart is not a review gate and a dirty tree ships as-is.

The rebuilt binary is written into the process temp directory, which must be
writable (`TMPDIR`, or `/tmp` when unset); an unusable directory is refused with
that directory named rather than as a build fault.

The choreography builds first, stops acceptance while retaining the listening
socket, drains active handlers, flushes storage, then hands the socket to a
ready child. A flush failure aborts and resumes the old process. A drain timeout
still flushes and hands off: the in-flight requests are cut, but everything
finalized before them is durable before the child opens the database. The flush
has its own bound rather than sharing the drain window.

The handoff also takes the config transaction lock for its whole flush fence, so
a start while a reload, Settings save or backup restore is running is refused
with `409` naming the operation instead of queueing behind it; the fence itself
is bounded by `restart_drain_timeout` plus the flush bound.

A restore holds that lock across its streaming upload, so `POST /admin/reload`
and a Settings save answer `409` while one is in flight rather than waiting for
a client-paced upload to finish.

Parked native-adapter runs are process-local and do not survive a successful
restart.

Ordinary SIGINT/SIGTERM shutdown is different: it cancels active stream contexts,
then shuts down HTTP and drains/closes storage. Do not describe it as preserving
unfinished streams. Already-dropped records cannot be recovered by any flush.

Starting another matching proxy executable on an occupied port can trigger
same-binary port reclamation (SIGTERM, then escalation). This is not the graceful
rebuild workflow; choose distinct ports and databases for separate instances.

