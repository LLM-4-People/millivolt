# Tests

All test sources and disposable test harnesses live here. Operational commands
remain in `cmd/` and `scripts/`. Run commands below from the repository root.

## Shared entry point

```sh
scripts/check.sh core
scripts/check.sh go test -mod=readonly ./internal/config -count=1
scripts/check.sh go test -mod=readonly -race ./... -count=1
scripts/check.sh go vet ./...
scripts/check.sh go list -json ./internal/config
scripts/check.sh go mod tidy -diff
npm test
```

`scripts/check.sh` owns the local/CI check sequence. `go test ./...` without this
entry point does **not** discover the relocated package tests; it can succeed
with no tests run. Likewise, use the overlay-aware command for vet and module
tidying so test code and test-only dependencies are included.

## Go source layout

`_go/` mirrors each tested package's original path, without a redundant `_test`
suffix on physical files. For example, `tests/_go/internal/config/config.go`
is compiled as the virtual file `internal/config/config_test.go`.
`tests/go.py` discovers every Go file in that tree and appends Go's required
virtual suffix through a temporary native `-overlay` manifest. It changes no production source,
exports no private APIs, and uses the root `go.mod` and `go.sum` only.

The runner checks physical-file/native-package discovery parity, rejects
misplaced, ignored, duplicate and symlinked tests, and removes its private
manifest on success or failure. The `_go` directory is intentionally excluded
from Go's normal recursive package walk. New Go tests and their helpers use
ordinary `.go` filenames inside the mirrored tree. Platform filename suffixes
and build tags retain their native Go meaning; `_test` exists only in the
temporary virtual filenames.

Package-private access, source-package working directories, benchmarks, fuzz
tests and race detection are retained. Runtime file reads do not see virtual
files: read real fixtures, not a relocated test's virtual source filename.
Editors and debuggers do not automatically learn this mapping; test discovery,
navigation and clickable stack paths may need tool-specific configuration.
No global `go env -w` setting or persistent machine-specific manifest is needed.
The runner owns `-overlay`, the working directory and the module file, and
disables an inherited Go workspace. Use `gofmt` on the physical files under
`tests/_go`, not the nonexistent virtual paths.

These behaviors are verified against the Go version in the root module. See
the official [Go command documentation](https://pkg.go.dev/cmd/go) for overlay
and package-pattern limitations.

## Python and browser harnesses

Python unit tests live in `python/`, separate from the harnesses they test.
Run them as modules from the repository root, not as standalone scripts:

```sh
python3 -B -O -m tests.python.go
python3 -B -O -m tests.python.repository
python3 -B -O -m tests.python.backup_db
python3 -B -O -m tests.python.release
python3 -B -O -m tests.python.browser
scripts/check.sh browser --target http://127.0.0.1:8081
python3 -B -O -m tests.explorer_check --target http://127.0.0.1:8081 --docs-history /tmp/millivolt-docs-images
scripts/check.sh container --image sha256:YOUR_LOCAL_IMAGE_ID
```

Browser commands require installed Playwright/Chromium and a verified private
instance owned by `scripts/dev.sh`; they never start or restart it. Port 8080
is rejected. Documentation-history capture additionally requires a validated
sanitized copy and public example configuration before opening dashboard data.
It cannot submit inference or operator mutations. Container checks own only
their disposable local image/container/volume fixtures.
