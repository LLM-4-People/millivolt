# Contributing

Start with the [architecture map](docs/architecture.md) and
[mandatory working rules](AGENTS.md). Keep changes focused on existing shared
owners, include regressions, and use neutral fixtures rather than real providers.

Use the [issue tracker](https://github.com/LLM-4-People/millivolt/issues) for
non-sensitive bugs and proposals, and
[pull requests](https://github.com/LLM-4-People/millivolt/pulls) for changes.
Security reports belong in the private channel in [SECURITY.md](SECURITY.md).

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
`npm test` runs the jsdom suite separately. Use targeted Go tests while iterating,
then re-run checks
after your last edit. Concurrency changes need race coverage; JavaScript unit
checks do not establish real canvas rendering, layout or browser timing.

For changes to configuration, update the Config/default/validation/schema owners
and regenerate the example as described in [operations](docs/operations.md).
Do not edit `proxy.yaml`: it may be a running operator's configuration.

## Isolated development

Never use an existing main instance on port 8080 for tests. Use the lifecycle
script, with cleanup even if verification fails:

```sh
trap 'scripts/dev.sh stop' EXIT
scripts/dev.sh
scripts/check.sh browser --target http://127.0.0.1:8081
```

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
sensitive. Do not use broad process kills or start an ad-hoc second main instance.

The browser fixtures share private-target/config/database and redirect guards.
They disable SSE and test bootstrap/poll rendering, not uninterrupted live-feed
delivery. They remove only their synthetic client records. Their assertions must
remain enabled under Python `-O`; the guard suite verifies this.

## Performance evidence

With the private dev instance already running:

```sh
go run ./cmd/stress -target http://127.0.0.1:8081 -concurrency 1,8,32,128 -duration 5s
```

The harness does not manage or reconfigure the proxy. It uses a local synthetic
upstream and checks exact bytes, durable rows/tokens/cost and the same-process
storage-drop delta. A drop is a failed stage even if every HTTP request succeeded.
Read `go run ./cmd/stress -h` for workload/safety controls.

Report workload, duration, concurrency, configuration, contention and whether
storage/observers were active. Separate request throughput from durable
throughput. Closed-loop bursts, microbenchmarks and local upstreams do not
establish sustained production capacity; see the [dated reports](docs/README.md#historical-evidence).

## Versioning and publication

[VERSION](VERSION) is the only semantic version source; do not duplicate it in
package manifests, build arguments or image metadata. Normal and prerelease
SemVer are supported without build metadata. A release tag must be exactly
`v` followed by that file's version. The release CLI validates the same contract:

```sh
millivolt_version="$(tr -d '\r\n' < VERSION)"
go run ./cmd/release -revision "$(git rev-parse HEAD)" -tag "v$millivolt_version"
```

Run the shared checks before tagging. Confirm the hosted container build and
smoke tests succeed, then check the package visibility and anonymous pull.
Do not report a release/image as available merely because validation or a local
build passed. Image usage and persistence limits live in
[operations](docs/operations.md#versions-and-images).

After committing a version change and creating its exact tag, validate the
checked-out revision, clean worktree and tag target through the shared gate:

```sh
scripts/check.sh release -revision "$(git rev-parse HEAD)" -tag "v$millivolt_version"
```

The gate clears inherited Git routing/configuration variables before checking
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
It checks startup, HTTP, Settings and durable state across stop/start without
publishing a host port or reusing an operator volume. It complements, rather
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
