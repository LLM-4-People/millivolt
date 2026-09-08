# Contributing

Start with the [architecture map](docs/architecture.md). Keep changes focused on
existing shared owners, include regressions, and use neutral fixtures rather
than real providers. Setup, safety and verification rules live in this guide.

Use the [issue tracker](https://github.com/LLM-4-People/millivolt/issues) for
non-sensitive bugs and proposals, and
[pull requests](https://github.com/LLM-4-People/millivolt/pulls) for changes.
Security reports belong in the private channel in [SECURITY.md](SECURITY.md).

## Branch workflow

New features branch from and land on `testing` first, with their documentation
and relevant checks in the same change. Fetch the current remote branch before
starting follow-up work. Use feature branches targeting `testing` for review,
or push a completed, checked feature directly there when that is authorized.

Promotion from `testing` to `main` is a separate deliberate integration after
validation, not part of an ordinary feature push. Do not push release tags as a
side effect of feature work: CI cuts releases itself. CI checks every branch;
production image publishing remains restricted to `main` and version tags.

`AGENTS.md` is intentionally local and ignored by the publication guard. Keep its
working instructions aligned with this shared guide; do not force-add private
instruction files to publish workflow policy.

## Development dependencies

Linux is supported. Version sources are:

- Go: [go.mod](go.mod).
- Node: [.node-version](.node-version).
- Python: [.python-version](.python-version).
- Browser tooling: [requirements-dev.txt](requirements-dev.txt).

The proxy embeds its vanilla JavaScript/CSS and needs no frontend build or Node
runtime in deployment. Node/jsdom and Python/Playwright are development tools.

```sh
npm ci --ignore-scripts
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements-dev.txt
python -m playwright install chromium
```

On fresh Linux installations, Chromium may also need system libraries; use
`python -m playwright install --with-deps chromium` with appropriate installation
permissions. Go race checks require `CGO_ENABLED=1` and a working C compiler;
the regular proxy build does not require a native SQLite driver.

## Checks

```sh
scripts/check.sh
```

The shared script owns the core verification commands, including offline
documentation-link, publication-ignore and typography regressions (Git is required).
`npm test` runs the jsdom suite separately. All tests and disposable harnesses
live in [tests/](tests/README.md), with suffix-free physical filenames. Go's
temporary overlay adds its required virtual `_test.go` names; Python unit
modules live in `tests/python/`. Use `scripts/check.sh go test` for targeted Go
checks and `scripts/check.sh go vet` or `scripts/check.sh go mod tidy` to include
test code and dependencies. Raw `go test ./...` does not discover the relocated
tests and can report success without running them. Re-run checks after your
last edit. Concurrency changes need race coverage. JavaScript unit checks do
not establish real canvas rendering, layout, focus or viewport behavior.

### Push gate

Do not push `testing` or `main` until the exact commit you will push has
passed the same `checks` job CI runs. A request to push is not a waiver.
Package-scoped `go test` and `npm test` during development are not sufficient.
CI's source-and-unit step is the default core suite:

```sh
scripts/check.sh
```

CI always runs isolated browser fixtures as well (desktop and mobile). Use
`scripts/dev.sh` plus `scripts/check.sh browser` as below, with
`MILLIVOLT_OPERATOR_TOKEN` set. Re-run both after the last commit. A red
GitHub check after a push means the gate was skipped or stale: fix, re-run,
and push only a passing tree.

For changes to configuration, update the Config/default/validation/schema owners
and regenerate the example as described in [operations](docs/operations.md).
Do not edit `proxy.yaml`: it may be a running operator's configuration.

## Isolated development

Never use an existing main instance on port 8080 for tests. Use the lifecycle
script, with cleanup even if verification fails:

```sh
(
  set -e
  trap 'scripts/dev.sh stop' EXIT
  export MILLIVOLT_OPERATOR_TOKEN='dev-instance-credential'
  scripts/dev.sh
  scripts/check.sh browser --target http://127.0.0.1:8081
)
```

The dev instance shares your shell environment, so a local
`MILLIVOLT_OPERATOR_TOKEN` arms its operator gate; browser and stress harnesses
need the same value for gated fixture cleanup. An instance started without it
serves reads only and denies every mutation.

`scripts/dev.sh status` reports the private instance. The script builds first,
then replaces only its tracked dev process; a failed build leaves it running.
Starting/restarting development resets its disposable database and private config,
seeded from the public example by default. `DEV_CONFIG=proxy.yaml` explicitly
opts into copying an operator config; it is never selected implicitly.
UI-triggered rebuilds keep that private config and update the tracked PID.

Overrides are deliberately restricted: `DEV_PORT` is a non-production port,
`DEV_HOST` is loopback, and `DEV_DB` is a permitted scratch filename or `none`.
`DEV_COPY_DB=1` seeds a consistent online backup of local history: use it only
when that history is authorized for the task, and treat the scratch copy as
sensitive. `DEV_COPY_DB=redacted` instead derives the separately validated
documentation copy described below; `0` keeps fresh fixture state. Do not use
broad process kills or start an ad-hoc second main instance.

The browser fixtures share private-target/config/database and redirect guards.
They disable SSE and test bootstrap/poll rendering, not uninterrupted live-feed
delivery. They remove only their synthetic client records. Their assertions must
remain enabled under Python `-O`; the guard suite verifies this.

## Container development

Normal [compose.yaml](compose.yaml) only runs a published image. For a local
source build, use [compose.dev.yaml](compose.dev.yaml) from the checkout root.
It is a complete Compose file, not an overlay: service, container and project
are all `millivolt-dev`.

```sh
docker compose -f compose.dev.yaml up -d --build
docker compose -f compose.dev.yaml logs --tail 50
docker compose -f compose.dev.yaml down
```

The development project keeps its containers and named volumes separate from a
normal deployment, including when `COMPOSE_PROJECT_NAME` is set outside the
checkout. Its `MILLIVOLT_DEV_PORT` and `MILLIVOLT_DEV_IMAGE` inputs are defined
only there. Choose an unused development port if `scripts/dev.sh` is already
running.

Rerun the same `up --build` command after source changes. The resulting runtime
image has no compiler or source tree, so dashboard rebuild is unavailable.
`down` preserves its named volumes unless you explicitly request deletion.
This source-image workflow does not replace `scripts/dev.sh`: the host browser
and stress fixtures require that script's private process/database identity.

## Documentation screenshots

The published gallery uses recorded metrics from an explicitly authorized,
redacted local-history snapshot, not fabricated timing or cost values. Never
point capture tools at a main instance or serve a raw history copy for screenshots.
The source is the checkout's local `proxy.db`, opened read-only through the existing
online-backup helper. This workflow needs explicit authorization for that history.

From a checkout with browser dependencies installed and that authorized source:

```sh
(
  set -e
  export DEV_PORT=18081 DEV_HOST=127.0.0.1
  export DEV_DB=/tmp/millivolt/millivolt-dev-docs.db
  export DEV_CONFIG=proxy.example.yaml DEV_COPY_DB=redacted
  trap 'scripts/dev.sh stop' EXIT
  scripts/dev.sh up
  python3 -B -O -m tests.explorer_check \
    --target "http://${DEV_HOST}:${DEV_PORT}" --docs-history docs/images
)
```

Choose an unused permitted dev port. Before startup, `dev.sh` derives a new
metrics-only database: identifiers are replaced, content/headers/debug data are
removed, and recorded timing, token and cost values are retained. The raw online
snapshot is temporary and never served. Treat even the derived database as
sensitive; retained usage and timestamps can reveal activity patterns.

The capture mode revalidates every row and its provenance, plus exact public
configuration bytes, before and after browser capture. It permits only read-only
same-origin requests: no inference, POST, purge or Settings save. Images are
staged before publication, and the exit trap stops the private process. Review
every gallery image before committing. SSE is deliberately disabled, so the images'
offline indicator describes a static capture session, not current service health.
Captures keep the same category folders under `docs/images/`: `overview`,
`charts`, `explorer`, `requests`, `menus` and `settings`. Update the existing
capture owner and Markdown links together when adding a view.

The independent `--docs-images` option remains a strict synthetic regression.
Run it only with `DEV_COPY_DB=0` and direct those two images to a scratch directory,
not `docs/images`. It creates and cleans up its own six-request fixture and
rejects copied history. Neither capture mode is a performance or compatibility
benchmark; do not weaken one mode's admission checks to accommodate the other.

## Performance evidence

With the private dev instance already running:

```sh
go run ./cmd/stress -target http://127.0.0.1:8081 -concurrency 1,8,32,128 -duration 5s
```

The harness does not manage or reconfigure the proxy. It uses a local synthetic
upstream and checks exact bytes, durable rows/tokens/cost and the same-process
storage-drop delta. A drop is a failed stage even if every HTTP request succeeded.
Read `go run ./cmd/stress -h` for workload/safety controls.
The ramp retains its cumulative fixture history across measured stages, then
removes only its unique client records after measurements finish. Cleanup has
its own bounded wait even after cancellation and revalidates private process,
config and database ownership before every deletion. A cleanup failure is
reported, never silently treated as success.

Report workload, duration, concurrency, configuration, contention and whether
storage/observers were active. Separate request throughput from durable
throughput. Closed-loop bursts, microbenchmarks and local upstreams do not
establish sustained production capacity. The current storage contract and
capacity caveats live in [operations](docs/operations.md#storage-and-accounting).
Use the existing `BenchmarkStorageWriter` and `BenchmarkProxyDurableContention`
for isolated writer/contention comparisons; neither replaces a real HTTP run.

## Versioning and publication

[VERSION](VERSION) is the only semantic version source; do not duplicate it in
package manifests, build arguments or image metadata. Normal and prerelease
SemVer are supported without build metadata. A release tag must be exactly
`v` followed by that file's version. The release CLI validates the same contract:

```sh
millivolt_version="$(tr -d '\r\n' < VERSION)"
go run ./cmd/release -revision "$(git rev-parse HEAD)" -tag "v$millivolt_version"
```

Publication is automatic. After the checks and both container architectures
pass, every push to `main` that touches anything besides documentation
(`docs/**`, `**/*.md`) cuts a release:

1. The next version is the higher of VERSION and the newest `v*` tag. If that
   version is already tagged, the patch number increments; an untagged VERSION
   (a manual bump or one carried by a promotion merge) ships as-is.
2. The workflow commits the VERSION change, pushes an annotated `v<version>`
   tag and creates the GitHub release with the commit log since the previous
   tag as notes.
3. It then builds and publishes the multi-arch image with `main`, `sha-*`,
   `version` and (for non-prereleases) `latest` tags on ghcr.io.

Docs-only pushes never cut a release or rebuild images. A manually pushed
`v*` tag still ships exactly as tagged through the same pipeline. VERSION is
recomputed from the higher of the file and the newest tag on every release, so
a promotion merge that resolves a VERSION conflict to either side is safe.
Manual validation of a candidate remains available:

```sh
scripts/check.sh release -revision "$(git rev-parse HEAD)" -tag "v$millivolt_version"
```

Run the shared checks before relying on a release. Confirm the hosted container
build and smoke tests succeed, then check the package visibility and anonymous
pull. Do not report a release/image as available merely because validation or a
local build passed. Image usage and persistence limits live in
[operations](docs/operations.md#versions-and-images).The gate clears inherited Git routing/configuration variables before checking
source identity, so another worktree or external ignore policy cannot satisfy
the release checks accidentally.

Only push the intended branch and release tag after that check succeeds. The
workflow publishes images automatically; it does not create GitHub release
notes. Native container checks run before the multi-platform publish job.

For local container verification, build with Docker, resolve the image's
immutable ID, then use the same fixture as CI:

```sh
docker build -t millivolt:check .
millivolt_image_id="$(docker image inspect --format '{{.Id}}' millivolt:check)"
scripts/check.sh container --image "$millivolt_image_id"
```

The shared container mode validates Compose defaults and owns only its
disposable, network-isolated Docker fixture, selected by immutable image ID.
It checks startup, HTTP, Settings and durable state across stop/start, then backs
up the stopped volumes and restores into another fresh container. Recovery must
retain settings, history and private file ownership/modes. It never publishes
a host port or reuses an operator volume. It complements, rather
than replaces, the host lifecycle in `scripts/dev.sh`.

## Change checklist

Use sentence case or lowercase in prose, headings and labels. Avoid em dashes;
preserve exact protocol names, identifiers, user data and license wording.

- Add a targeted regression for a reproduced defect; preserve existing coverage.
- Keep defaults, units, schemas and state transitions at their canonical owner.
- Measure allocations/latency when changing the hot path or analytical folds.
- Check saved views, mobile layout and accessibility for visible changes.
- Update the relevant existing documentation, not duplicate specifications.
- Exclude local configs, databases, tokens, captures and generated test artifacts.
- Preserve dependency licenses. Report vulnerabilities using [SECURITY.md](SECURITY.md),
  not a public issue containing sensitive reproduction data.

Dependabot checks Go, npm, Python, Docker and GitHub Actions dependencies weekly when
enabled by the hosting repository. Review dependency changes and re-run checks;
the runtime version files still require deliberate updates and verification.
Advisory scans such as `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`
and `npm audit` complement tests; a clean scan is not a security guarantee.
