#!/usr/bin/env bash
# One local/CI check list. dev.sh owns host proxy lifecycle; container mode owns
# only its disposable, network-isolated Docker fixture.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "check: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "required tool not found: $1"; }
usage="usage: scripts/check.sh [core | go COMMAND [ARGS...] | browser --target URL | container --image SHA256 | release -revision SHA [-tag vVERSION]]"
mode="${1:-core}"
case "$mode" in
  core)
    (( $# <= 1 )) || die "$usage"
    for tool in go gofmt node npm python3 git jq; do need "$tool"; done
    go version
    node --version
    npm --version
    python3 --version
    check_dir="$(mktemp -d /tmp/millivolt-check.XXXXXX)"
    trap 'rm -rf -- "$check_dir"' EXIT
    background_pids=()
    background_names=()
    run_background() {
      local name=$1
      shift
      "$@" >"$check_dir/$name.log" 2>&1 &
      background_pids+=("$!")
      background_names+=("$name")
    }
    python_suites() {
      python3 -B -O -m tests.python.repository
      python3 -B -m tests.repository_check
      python3 -B -O -m tests.python.site
      python3 -B -O -m tests.python.workflows
      python3 -B -O -m tests.python.release
      python3 -B -O -m tests.python.backup_db
      python3 -B -O -m tests.python.go
      python3 -B -O -m tests.python.shard
    }
    wait_background() {
      local failed=0 index
      for index in "${!background_pids[@]}"; do
        if ! wait "${background_pids[$index]}"; then
          echo "check: ${background_names[$index]} failed" >&2
          cat "$check_dir/${background_names[$index]}.log" >&2
          failed=1
        fi
      done
      (( failed == 0 )) || exit 1
    }
    unformatted="$(gofmt -l version.go cmd internal scripts/licenses tests)"
    [ -z "$unformatted" ] || die "gofmt required: $unformatted"
    for script in scripts/*.sh; do bash -n "$script"; done
    go mod verify
    # Explicit command paths make missing command source fail on a fresh checkout.
    go list ./cmd/proxy ./cmd/stress ./cmd/release
    go build -mod=readonly -o "$check_dir/" ./...
    python3 -B -m tests.go vet -mod=readonly ./...
    # Independent suites do not share source state. Start them while the race
    # shards use the cores; collect failures after the gate.
    run_background python python_suites
    run_background npm npm test
    # The shard runs two passes over the same overlay: tests declared by
    # tests/race_scope.py under the race detector, the rest plain. Every test
    # runs exactly once, and fixed-port packages stay serial and locked.
    python3 -B -O -m tests.shard
    wait_background
    ;;
  go)
    (( $# >= 2 )) || die "$usage"
    need python3
    need go
    need git
    python3 -B -m tests.go "${@:2}"
    ;;
  browser)
    (( $# == 3 )) && [ "$2" = --target ] || die "$usage"
    need python3
    # -B leaves no bytecode in the source tree; -O proves guards stay enabled.
    python3 -B -O -m tests.python.browser
    python3 -B -O -m tests.browser_check --target "$3"
    python3 -B -O -m tests.explorer_check --target "$3"
    ;;
  container)
    (( $# == 3 )) && [ "$2" = --image ] || die "$usage"
    need go
    need docker
    check_dir="$(mktemp -d /tmp/millivolt-container.XXXXXX)"
    trap 'rm -rf -- "$check_dir"' EXIT
    CGO_ENABLED=0 go build -mod=readonly -trimpath -o "$check_dir/containercheck" ./tests/containercheck
    "$check_dir/containercheck" -image "$3"
    ;;
  release)
    (( $# == 3 || $# == 5 )) && [ "$2" = -revision ] || die "$usage"
    if (( $# == 5 )); then [ "$4" = -tag ] || die "$usage"; fi
    need go
    need git
    # Inherited Git routing/configuration must not validate a different checkout
    # or alter the Go tool's source identity. The repository owns ignore rules.
    while IFS= read -r variable; do
      if [[ "$variable" == GIT_* ]]; then unset "$variable"; fi
    done < <(compgen -e)
    export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
    # cmd/release alone owns SemVer, tag spelling and revision syntax.
    metadata="$(go run -mod=readonly ./cmd/release "${@:2}")"
    [ "$(git rev-parse --verify HEAD)" = "$3" ] || die "revision does not match checked-out HEAD"
    [ -z "$(git status --porcelain --untracked-files=all)" ] || die "release checkout contains nonignored changes"
    if (( $# == 5 )); then
      [ "$(git rev-parse --verify --end-of-options "$5^{commit}")" = "$3" ] || die "release tag does not identify HEAD"
    fi
    printf '%s\n' "$metadata"
    ;;
  *) die "$usage" ;;
esac
