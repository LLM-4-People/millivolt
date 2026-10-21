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

`main` is promotion-only. Do not develop on it, push it directly, or bypass its
rules. The `main-testing-first` ruleset requires a pull request and the
`required` check, allows merge commits only, and has no bypass actors. The
`long-lived-branch-integrity` ruleset also blocks deletion and non-fast-forward
updates. Administrators have no bypass either. When release automation needs new
behavior, change the workflow in a pull request; never add a bypass actor or use
an administrator override. Inspect the active ruleset with:

```sh
ruleset_id="$(gh api repos/LLM-4-People/millivolt/rulesets \
  --jq '.[] | select(.name == "main-testing-first") | .id')"
gh api "repos/LLM-4-People/millivolt/rulesets/$ruleset_id" \
  --jq '{name,enforcement,bypass_actors,current_user_can_bypass}'
```

It must report `enforcement: active`, an empty `bypass_actors`, and
`current_user_can_bypass: never`.

Completed feature pushes to `testing` publish only the moving development image.
All pull requests to `testing`, all code-bearing pushes to `testing`, and
manually pushed `v*` tags run the full gate; docs and site-only pushes to
`testing` run the smaller documentation gate in `docs.yml`. An ordinary promotion
pull request to `main` runs only `check.yml`'s lightweight `required` promotion
gate: it verifies the head is the current `testing` tip, `main` is already
merged back into `testing`, and the full gate already succeeded for that
commit: the canonical `check.yml` push run's own check suite, with `checks` and
both native container legs. A final docs-only commit after the closest gated
`testing` commit reuses that gate when every path since it is docs/site-only.
The fallback selects the closest gated testing commit from canonical `testing`
push runs, with no commit-count bound. For a
code-bearing head, let the `testing` `check.yml` push gate finish before opening
or re-running a promotion PR; pending exact check runs get a short bounded wait
and the proof fails closed while they remain pending. A docs/site-only head has
no canonical `check.yml` push run of its own, so wait for `docs.yml` or only the
`required` docs validation. A Dependabot security pull request targets `main`
directly and therefore still runs the full gate.

### Promote testing to main

1. Fetch and align the branches. `testing` must contain the feature work and
   `main` must be an ancestor:
   ```sh
   git fetch origin
   git switch testing
   git merge --ff-only origin/testing
   ```
   If `main` is not an ancestor of `testing`, merge `origin/main` into
   `testing` first and resolve conflicts before continuing.
2. For a code-bearing promotion, bump `VERSION` on `testing` to a version
   newer than every existing `v*` tag. Normal SemVer precedence applies; a
   prerelease is lower than the same stable version. Commit the bump
   separately:
   ```sh
   printf '<next-version>\n' > VERSION
   git add VERSION
   git commit -m "bump version to <next-version>"
   git push origin testing
   ```
   A docs/site-only promotion may skip this step. `required` detects that every
   changed path is covered by the docs/site filter (for example Markdown,
   `docs/`, `site-src/`, the docs and Pages workflows, and `LICENSE`), and the
   merged main push stays docs-only.
3. Open a pull request from `testing` to `main`. For a code-bearing head, wait
   for the `testing` `check.yml` push gate to finish first; a docs/site-only
   head has no canonical `check.yml` push run of its own, so wait for `docs.yml`
   or only the `required` docs validation. Then wait for `required`. The proof
   polls pending exact runs a bounded number of times and fails closed while
   they remain pending, so opening or re-running the PR early only produces a
   red check. `required` does not rerun the full `testing` gate. For
   every non-Dependabot
   promotion it verifies that the head is the current `testing` tip and that
   `main` is already merged back into `testing`; code-bearing promotions must
   also have a successful full-gate record for that exact commit, or for the
   closest gated `testing` commit when every path since it is docs/site-only,
   and `VERSION` newer than every release tag.
   If it fails with "Promote the current testing tip", push the final commit to
   `testing` and reopen or update the PR. If it fails with "Merge main back
   into testing", merge `origin/main` into `testing` first. If it fails with
   "changes non-docs paths after the gated commit", push the final code commit
   and let its full gate finish. If it fails with "VERSION must be newer", bump
   `VERSION` above every tag and push the commit.
4. Merge with a merge commit:
   ```sh
   gh pr merge <number> --merge
   ```
   Squash and rebase are disabled on `main`; they would replace the reviewed
   testing history instead of preserving it.
5. After the promotion is on `main` and its release or Pages run finishes,
   merge `main` back into `testing` so the next promotion is up to date:
   ```sh
   git fetch origin main
   git switch testing
   git merge --ff-only origin/main
   git push origin testing
   ```
   If `testing` moved after the promotion merge, merge `origin/main` normally
   and resolve conflicts before the next promotion.

For a code-bearing promotion, CI creates the annotated release tag on the merge
and publishes the image. It never pushes `main`. Do not push a `v*` tag as part
of feature work; the manual tag path is only for an explicitly verified release
and must match `VERSION`.

Dependabot version updates target `testing`. Dependabot security updates target
the default branch and are the only exception to the testing-ancestry check;
they also skip the version check. They still run the full gate and the
`required` check. A security-only merge normally publishes no image and creates
no tag because `VERSION` is already tagged; if `VERSION` is still untagged, that
merge can publish the pending version. Merge `main` back into `testing` and
include the fix in the next versioned promotion.

Docs and site-only pushes to `testing` skip image work. A docs/site-only
promotion to `main` skips the `VERSION` and full-gate proofs, validates the
Markdown, generated site, workflow policy and docs contracts in `required`, and
leaves the merged main push docs-only, so a Pages-relevant change deploys the
site without rebuilding images or cutting a release.

`AGENTS.md` is intentionally local and ignored by the publication guard. Keep its
working instructions aligned with this shared guide; do not force-add private
instruction files to publish workflow policy.

## Development dependencies

Linux is supported. Version sources are:

- Go: [go.mod](go.mod).
- Node: [.node-version](.node-version).
- Python: [.python-version](.python-version).
- `jq`: the workflow contract suite executes the promotion gate's shell
  predicates, so the core checks need the `jq` binary on `PATH`.
- Browser tooling: [requirements-dev.txt](requirements-dev.txt).
- Documentation site: [requirements-docs.txt](requirements-docs.txt).

The proxy embeds its vanilla JavaScript/CSS and needs no frontend build or Node
runtime in deployment. Node/jsdom, Python/Playwright, Python-Markdown and PyYAML
are development and documentation tools.

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
documentation-link, publication-ignore and typography regressions (Git is required),
the generated-site contract, and the workflow/automation policy. `npm test` runs
the jsdom suite separately. All tests and disposable harnesses
live in [tests/](tests/README.md), with suffix-free physical filenames. Go's
temporary overlay adds its required virtual `_test.go` names; Python unit
modules live in `tests/python/`. Use `scripts/check.sh go test` for targeted Go
checks and `scripts/check.sh go vet` or `scripts/check.sh go mod tidy` to include
test code and dependencies. Raw `go test ./...` does not discover the relocated
tests and can report success without running them. Re-run checks after your
last edit. Concurrency changes need race coverage. JavaScript unit checks do
not establish real canvas rendering, layout, focus or viewport behavior.

### Documentation site

The public site is generated from the existing Markdown, not maintained as a
second documentation tree. `site-src/` owns the layout, styles, and brand
assets; `scripts/build_site.py` renders the pages, rewrites repository links,
and emits the search index, sitemap, and 404 page. The generated `site/`
directory is ignored and never committed.

```sh
python3 -m pip install -r requirements-docs.txt
python3 -B -O -m tests.python.site
python3 scripts/build_site.py --output site
```

For a local preview, serve `site/` from a separate terminal with
`python3 -m http.server --directory site 8082`; do not point it at the proxy or
use the main `:8080` instance. The `docs.yml` workflow builds the site for
documentation changes, and `pages.yml` deploys from `main` through the official
GitHub Pages Actions flow. Repository-level Pages enablement and the
`github-pages` environment are one-time administrator settings.

### Push gate

Never push `main` directly: open a promotion pull request and merge it with a
merge commit. Do not push `testing` until the exact commit you will push has
passed the same `checks` job CI runs. A request to push is not a waiver.
Package-scoped `go test` and `npm test` during development are not sufficient.
CI's source-and-unit step is the default core suite:

```sh
scripts/check.sh
```

CI sets `GOTOOLCHAIN=local`, so the host Go must match the `go` version in
`go.mod`. On a host with an older base Go, install Go 1.27.2 or select the
matching toolchain before running the gate; `GOTOOLCHAIN=local` alone cannot
upgrade an older installation.

Full-gate CI events also run the isolated browser fixtures (desktop and mobile)
in the `checks` job. Use `scripts/dev.sh` plus `scripts/check.sh browser` as
below, with `MILLIVOLT_OPERATOR_TOKEN` set. Re-run both after the last commit. A
red GitHub check after a push means the gate was skipped, stale, or unmonitored:
fix, re-run, and push only a passing tree, then watch the new run to completion.

That same `checks` job also runs the live MCP integration test, which
`scripts/check.sh browser` alone does not run. It is mandatory on the same
commit:

```sh
MILLIVOLT_MCP_INTEGRATION=1 scripts/check.sh go test -count=1 ./internal/mcp/...
```

The test starts a private dev instance through `scripts/dev.sh` on its own
loopback port and scratch database, drives every registered tool, and stops the
instance again, so it does not touch the instance the browser check uses.

Before every push, check open pull requests and report any that are awaiting
the user's review or approval. Do not approve or merge them without explicit
authorization. Use:

```sh
gh pr list --state open --json number,title,author,isDraft,reviewDecision,mergeable,statusCheckRollup,url
```

After pushing, monitor the GitHub Actions run for the pushed commit until every
required job completes successfully. The local gate cannot see the browser,
MCP integration, container, code-scanning, or dependency-review jobs. Find the run with
`gh run list --branch <branch> --limit 1`, then wait for it with
`gh run watch <run-id> --exit-status`. A queued or in-progress run is not green.
If any job fails, treat the push as incomplete: fix the root cause, rerun this
gate, and push the fix. Do not leave `testing` or `main` red or start unrelated
work on top of a failing run.

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

### Live Chrome DevTools verification

Frontend changes are additionally verified in a visible Chrome window through
Chrome DevTools - headful, never headless. Sign in with the dev operator token,
drive the real fixture traffic and the actual controls, and check console
output, element geometry, the accessibility tree and screen flow in the
rendered page. Narrow fixture data passes the harnesses but can hide
distribution problems (wide values, wrapping, density); the live session is
where those surface. Keep the dev instance and the window open while you look;
stop the instance with `scripts/dev.sh stop` when done.

Fixture traffic for these sessions has its own lifecycle owner, never a paid
provider and never the main instance on `:8080`:

```sh
scripts/dev-traffic.sh         # bursts of neutral loopback traffic at the dev instance
scripts/dev-traffic.sh status
scripts/dev-traffic.sh stop
```

Bursts land about a minute apart so one-minute chart buckets gain a bucket
per burst; each run includes recovered 429s (rate-limited, not errors) and one
final 400 so both health surfaces carry data.

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

The published tables, method and limitations live in
[performance and resource usage](docs/performance.md). That page is the single
source for measured numbers; do not copy them into other documentation.

`scripts/perf.sh` owns a repeatable local measurement run without touching the
main instance. It starts fresh private instances on a disposable port and stops
them through `scripts/dev.sh`:

```sh
scripts/perf.sh bench     # isolated Go benchmarks
scripts/perf.sh stress    # private fast and streaming ramps
scripts/perf.sh streaming # sustained end-to-end SSE throughput ramp
scripts/perf.sh sustained # five-minute fast-response capacity levels
scripts/perf.sh image     # disposable image content and compressed size
scripts/perf.sh all       # bench, stress and image, in that order
```

For request, SSE-event, response-byte and output-token rates, use the
`streaming` mode. It derives those rates from responses the client accepted
byte-for-byte, and it is still a synthetic-fixture measurement, not a universal
capacity claim.

For an individual stress run with the private dev instance already running:

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
For the analytical projection's memory, use the self-seeded
`BenchmarkProjectionFootprint` (retained and transient bytes per stored record)
and `BenchmarkProjectionProcessRSS` (process RSS after readiness) from the test
overlay; they need no running instance:

```sh
scripts/check.sh go test -run='^$' -bench='BenchmarkProjection' -count=1 ./internal/web
```

## Versioning and publication

[VERSION](VERSION) is the only semantic version source; do not duplicate it in
package manifests, build arguments or image metadata. Normal and prerelease
SemVer are supported without build metadata. A release tag must be exactly
`v` followed by that file's version. The release CLI validates the same contract:

```sh
millivolt_version="$(tr -d '\r\n' < VERSION)"
go run ./cmd/release -revision "$(git rev-parse HEAD)" -tag "v$millivolt_version"
```

Publication is automatic. A code-bearing versioned push to `main` cuts a release
after the full gate has passed for the promoted commit, or after the closest
gated `testing` commit plus a docs-only delta. A Dependabot security PR targets
`main` and runs the full gate itself rather than reusing a `testing` result.
The release `publish` job waits for `required`. The `publish-testing` job waits
for `checks` and `container` instead, because a testing push has no promotion
proof to wait for:

1. A code-bearing promotion PR owns the version bump. `required` compares
   VERSION against every existing `v*` tag with SemVer precedence and rejects a
   non-Dependabot main PR unless VERSION is newer. Docs/site-only promotions
   skip that comparison, validate docs, workflow policy and the site in
   `required`, and stay release-free. If VERSION is already tagged, no
   release is cut: this is the Dependabot security-merge case, and the next
   versioned promotion releases the change.
2. The workflow creates the annotated `v<version>` tag on the reviewed
   promotion merge commit and builds and publishes the multi-arch image with
   `main`, `sha-*`, `version` and (for non-prereleases) `latest` tags, including
   an SBOM and max-mode provenance. The `sha-*` tag and OCI revision label name
   the promotion merge commit that was built.
3. Only after the image exists does it push the tag and create the GitHub
   release. It never pushes `main`. Release notes filter merge commits, dedupe
   subjects, use the lowercase image path, include the digest, and link the
   compare range. A failed image build leaves no public release pointing at a
   missing image. `main` and `latest` exclude prereleases; the `testing` alias
   never moves them.

Docs and site-only pushes to `testing` skip image work. They validate Markdown,
the generated site, workflow policy and docs contracts. A docs/site-only main
promotion also skips the VERSION and full-gate proofs but runs the same
Markdown, generated-site, workflow-policy and repository checks in `required`;
a Pages-relevant merged main push stays docs-only, so Pages deploys without
rebuilding images or cutting a release. A code-bearing main promotion
must carry a VERSION newer than every existing tag and cuts a release. A
manually pushed `v*` tag still ships exactly as tagged through the same
pipeline, with the image published before the release is created. A manual tag
must match VERSION. Code-bearing pushes to `testing` publish the moving
`ghcr.io/llm-4-people/millivolt:testing` image after the same checks and
container builds, without cutting a release or any version tag. Its OCI version
label reports the embedded VERSION, which can lag the latest release.
Manual validation of a candidate remains available:

```sh
scripts/check.sh release -revision "$(git rev-parse HEAD)" -tag "v$millivolt_version"
```

Run the shared checks before relying on a release. Confirm the hosted container
build and smoke tests succeed, then check the package visibility and anonymous
pull. Do not report a release/image as available merely because validation or a
local build passed. Image usage and persistence limits live in
[operations](docs/operations.md#versions-and-images). The gate clears inherited
Git routing/configuration variables before checking
source identity, so another worktree or external ignore policy cannot satisfy
the release checks accidentally.

Only push the intended release tag after that check succeeds. Main changes go
through the protected pull request path. The workflow publishes images
automatically and creates the release with notes generated from the commit log.
Native container checks run before the multi-platform publish job. After the
promotion run, verify the published tag and image:
```sh
gh release view "v$millivolt_version"
docker buildx imagetools inspect "ghcr.io/llm-4-people/millivolt:$millivolt_version"
```

### If a release fails

- Before the tag is pushed, no public release exists. Fix the failure and rerun
  the main workflow or promote a new version.
- After the tag is pushed, keep the tag in place. If the GitHub release is
  missing, create it from that tag or publish a new patch version. An automatic
  tag push does not start a second workflow run.
- Do not move or delete a published version tag.

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

Dependabot checks Go, npm, Python, Docker and GitHub Actions dependencies weekly
with grouped minor and patch updates. Review dependency changes and re-run
checks; runtime version files still require deliberate updates and verification.
The Docker `golang` base is deliberately left manual because its tag must move
with go.mod; distroless updates remain automated. Security updates target
`main` as described in the branch workflow. Dependency review runs on pull
requests, and a scheduled `govulncheck` workflow covers the Go module graph.
These scans complement tests; a clean scan is not a security guarantee.
