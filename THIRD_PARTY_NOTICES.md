# Third-party notices

millivolt's [MIT license](LICENSE) covers the project's own work, not a
relicensing of dependencies.

## Embedded dashboard library

[uPlot 1.6.32](https://github.com/leeoniya/uPlot/tree/1.6.32) is vendored as
`internal/web/static/vendor/uplot.js` and `uplot.css`. Copyright (c) 2022 Leon
Sorokin. Its complete [MIT notice](internal/web/static/vendor/uplot.LICENSE)
is kept beside the assets and included in the embedded filesystem.
Preserve that notice when redistributing the library or built dashboard.

The notice was checked against the
[upstream versioned license](https://github.com/leeoniya/uPlot/blob/1.6.32/LICENSE).
Update the version, assets and notice together when changing this dependency.

## Module and development dependencies

[go.mod](go.mod) and [go.sum](go.sum) identify Go dependencies; their upstream
module distributions carry their own licenses. [package-lock.json](package-lock.json)
pins development-only jsdom dependencies. [requirements-dev.txt](requirements-dev.txt)
pins Python browser tooling; Playwright selects its corresponding Chromium build.
Node, Python and these test dependencies are not required to run the proxy.

## Container distribution

The [Dockerfile](Dockerfile) runs [the notice collector](scripts/licenses/main.go)
against the exact target binary's Go dependency graph. It preserves module,
package-ancestor and Go toolchain license, copyright, notice and patent files,
plus the embedded uPlot notice, under `/usr/share/licenses/millivolt`.
The deterministic `manifest.json` records versions, relative paths and SHA-256
content hashes. Missing notices and module replacements fail the build for
review; this collector does not classify licenses or grant additional rights.

The pinned distroless runtime retains its own files and dependency notices.
Its [upstream project](https://github.com/GoogleContainerTools/distroless)
documents the base-image contents. Node, Python and development dependencies
are not copied into the runtime image.

When distributing a binary by another route, generate and include the same
bundle with `go run ./scripts/licenses -target linux/amd64 -out NEW_DIRECTORY`
(or `linux/arm64`) and review the actual distribution. This source notice alone
is not a complete binary-release license bundle.
