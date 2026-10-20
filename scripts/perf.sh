#!/usr/bin/env bash
# perf.sh - reproducible local measurement harness.
#
# This script never touches the main instance on :8080. Stress modes start a
# private scripts/dev.sh instance on a disposable loopback port and always stop
# it through the script's own lifecycle owner. Reports are raw output, not a
# committed benchmark result; docs/performance.md remains the published source.
set -euo pipefail
cd "$(dirname "$0")/.."

PERF_PORT="${PERF_PORT:-8091}"
PERF_HOST=127.0.0.1
PERF_DB="/tmp/millivolt/millivolt-dev-${PERF_PORT}.db"
PERF_DIR="$(mktemp -d "${TMPDIR:-/tmp}/millivolt-perf.XXXXXX")"
IMAGE_TAG="${IMAGE_TAG:-millivolt-perf:local}"

die() { echo "perf: $*" >&2; exit 1; }
case "$PERF_PORT" in ''|*[!0-9]*) die "PERF_PORT must be numeric" ;; esac
(( PERF_PORT >= 1024 && PERF_PORT <= 65535 && PERF_PORT != 8080 )) \
  || die "PERF_PORT must be 1024..65535 and not production port 8080"
[ -d /tmp/millivolt ] || mkdir -p /tmp/millivolt

usage() {
  cat <<'EOF'
usage: scripts/perf.sh [bench|stress|sustained|image|all]

  bench   run the isolated Go benchmarks used by docs/performance.md
  stress  start private dev instances and run fast and streaming ramps
  sustained  run fixed-concurrency five-minute fast-response stages
  image   build a disposable amd64 image and report content/compressed sizes
  all     image, bench, then stress

Environment:
  PERF_PORT   private loopback port (default 8091, never 8080)
  IMAGE_TAG   disposable image tag (default millivolt-perf:local)
  MILLIVOLT_OPERATOR_TOKEN  optional; stress uses a private fixture default
  PERF_SUSTAINED_LEVELS    comma-separated fixed worker counts (default 128)
  PERF_SUSTAINED_DURATION  time per sustained level (default 300s)
EOF
}

run_bench() {
  echo "bench: output below; compare revisions, not absolute numbers"
  scripts/check.sh go test -run='^$' \
    -bench='Benchmark(ProjectionFootprint|StoreDiskBytes)$' \
    -benchtime=1x -count=1 ./internal/web
  scripts/check.sh go test -run='^$' \
    -bench='BenchmarkProjectionProcessRSS$' \
    -benchtime=1x -count=1 ./internal/web
  scripts/check.sh go test -run='^$' \
    -bench='BenchmarkStorageWriter$' \
    -benchtime=2s -benchmem -count=1 ./internal/storage
  scripts/check.sh go test -run='^$' \
    -bench='Benchmark(QueryBeforeEquality|QueryBeforeCount)$' \
    -benchtime=1x -benchmem -count=1 ./internal/storage
  scripts/check.sh go test -run='^$' \
    -bench='Benchmark(RequestMetadata|ProxyDurableContention)$' \
    -benchtime=2s -benchmem -count=1 ./internal/proxy
}

run_image() {
  command -v docker >/dev/null || die "docker is required for image measurements"
  docker build --platform linux/amd64 -t "$IMAGE_TAG" . >"$PERF_DIR/image-build.log" 2>&1
  echo "image: $IMAGE_TAG"
  docker image inspect "$IMAGE_TAG" --format 'content-bytes={{.Size}}'
  printf 'compressed-bytes='
  docker save "$IMAGE_TAG" | gzip -9 | wc -c
  echo "build log: $PERF_DIR/image-build.log"
}

run_one() {
  local name="$1"
  shift
  echo "stress: $name (fresh private database)"
  (
    set -e
    export DEV_PORT="$PERF_PORT" DEV_DB="$PERF_DB" DEV_COPY_DB=0
    export MILLIVOLT_OPERATOR_TOKEN="${MILLIVOLT_OPERATOR_TOKEN:-perf-instance-credential}"
    trap 'scripts/dev.sh stop' EXIT
    scripts/dev.sh >"$PERF_DIR/dev-${name}.log" 2>&1
    "$PERF_DIR/stress" -target "http://${PERF_HOST}:${PERF_PORT}" "$@"
  ) >"$PERF_DIR/stress-${name}.jsonl"
  cat "$PERF_DIR/stress-${name}.jsonl"
}

run_sustained() {
  go build -trimpath -o "$PERF_DIR/stress" ./cmd/stress
  command -v curl >/dev/null || die "curl is required for dev lifecycle readiness"
  local levels="${PERF_SUSTAINED_LEVELS:-128}" duration="${PERF_SUSTAINED_DURATION:-300s}" level
  local IFS=,
  for level in $levels; do
    [[ "$level" =~ ^[1-9][0-9]*$ ]] || die "PERF_SUSTAINED_LEVELS must be positive integers"
    run_one "sustained-${level}" -concurrency "$level" -duration "$duration" -stream-duration 0 -timeout 60s
  done
  echo "raw output: $PERF_DIR"
}

run_stress() {
  local stress="$PERF_DIR/stress"
  go build -trimpath -o "$stress" ./cmd/stress
  command -v curl >/dev/null || die "curl is required for dev lifecycle readiness"

  run_one fast -concurrency 1,8,32,128 -duration 5s -stream-duration 0
  run_one streaming -concurrency 1,8,32,128,512 -duration 5s -stream-duration 1s
  echo "raw output: $PERF_DIR"
}

mode="${1:-all}"
case "$mode" in
  bench) run_bench ;;
  stress) run_stress ;;
  sustained) run_sustained ;;
  image) run_image ;;
  all) run_image; run_bench; run_stress ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 2 ;;
esac
