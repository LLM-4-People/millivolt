#!/usr/bin/env bash
# One local/CI check list. dev.sh owns host proxy lifecycle; container mode owns
# only its disposable, network-isolated Docker fixture.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "check: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "required tool not found: $1"; }
usage="usage: scripts/check.sh [core | browser --target URL | container --image SHA256 | release -revision SHA [-tag vVERSION]]"
mode="${1:-core}"
case "$mode" in
  core)
    (( $# <= 1 )) || die "$usage"
    for tool in go gofmt node npm python3 git; do need "$tool"; done
    go version
    node --version
    npm --version
    python3 --version
    python3 -B -O scripts/repository_check_test.py
    python3 -B scripts/repository_check.py
    python3 -B -O scripts/check_release_test.py
    unformatted="$(gofmt -l version.go version_test.go cmd internal scripts/containercheck scripts/licenses)"
    [ -z "$unformatted" ] || die "gofmt required: $unformatted"
    for script in scripts/*.sh; do bash -n "$script"; done
    go mod verify
    # Explicit command paths make missing command source fail on a fresh checkout.
    go list ./cmd/proxy ./cmd/stress ./cmd/release
    check_dir="$(mktemp -d /tmp/millivolt-check.XXXXXX)"
    trap 'rm -rf -- "$check_dir"' EXIT
    go build -mod=readonly -o "$check_dir/" ./...
    go vet -mod=readonly ./...
    go test -mod=readonly ./... -count=1
    go test -mod=readonly -race ./... -count=1
    npm test
    ;;
  browser)
    (( $# == 3 )) && [ "$2" = --target ] || die "$usage"
    need python3
    # -B leaves no bytecode in the source tree; -O proves guards stay enabled.
    python3 -B -O scripts/browser_check_test.py
    python3 -B -O scripts/browser_check.py --target "$3"
    python3 -B -O scripts/explorer_check.py --target "$3"
    ;;
  container)
    (( $# == 3 )) && [ "$2" = --image ] || die "$usage"
    need go
    need docker
    check_dir="$(mktemp -d /tmp/millivolt-container.XXXXXX)"
    trap 'rm -rf -- "$check_dir"' EXIT
    CGO_ENABLED=0 go build -mod=readonly -trimpath -o "$check_dir/containercheck" ./scripts/containercheck
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
