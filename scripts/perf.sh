#!/usr/bin/env bash
# perf.sh - reproducible local measurement harness.
#
# This script never touches the main instance on :8080. Stress modes start a
# private scripts/dev.sh instance on a disposable loopback port and always stop
# it through the script's own lifecycle owner.
#
# `docs` is the publishing path: it runs the complete documented measurement
# set, writes perf/results.json, and regenerates docs/performance.md. The page
# is never edited by hand; tests/python/site.py fails when it drifts from the
# bundle.
set -euo pipefail
cd "$(dirname "$0")/.."

PERF_PORT="${PERF_PORT:-8091}"
PERF_HOST=127.0.0.1
PERF_DB="/tmp/millivolt/millivolt-dev-${PERF_PORT}.db"
PERF_DIR="$(mktemp -d "${TMPDIR:-/tmp}/millivolt-perf.XXXXXX")"
IMAGE_TAG="${IMAGE_TAG:-millivolt-perf:local}"
RSS_SAMPLE_MS="${PERF_RSS_SAMPLE_MS:-100}"

die() { echo "perf: $*" >&2; exit 1; }
case "$PERF_PORT" in ''|*[!0-9]*) die "PERF_PORT must be numeric" ;; esac
(( PERF_PORT >= 1024 && PERF_PORT <= 65535 && PERF_PORT != 8080 )) \
  || die "PERF_PORT must be 1024..65535 and not production port 8080"
[ -d /tmp/millivolt ] || mkdir -p /tmp/millivolt

usage() {
  cat <<'EOF'
usage: scripts/perf.sh [bench|stress|streaming|sustained|image|docs|all]

  bench      run the isolated Go benchmarks used by docs/performance.md
  stress     start private dev instances and run fast and streaming ramps
  streaming  run the sustained end-to-end SSE throughput ramp
  sustained  run fixed-concurrency five-minute fast-response stages
  image      build a disposable amd64 image and report content/compressed sizes
  docs       run every published measurement, write perf/results.json and
             regenerate docs/performance.md (refuses a dirty tree unless
             PERF_ALLOW_DIRTY=1)
  all        image, bench, then stress

Environment:
  PERF_PORT   private loopback port (default 8091, never 8080)
  IMAGE_TAG   disposable image tag (default millivolt-perf:local)
  MILLIVOLT_OPERATOR_TOKEN  optional; stress uses a private fixture default
  PERF_FAST_LEVELS         comma-separated fast-response worker counts
                           (default 1,8,32,128)
  PERF_FAST_DURATION       time per fast level (default 5s)
  PERF_FAST_LONG_DURATION  final 128-worker row (default 10s)
  PERF_STREAMING_LEVELS    comma-separated worker counts for the throughput ramp
                           (default 1,8,32,64,128,256,512,1024)
  PERF_STREAMING_DURATION  time per streaming level (default 60s)
  PERF_STREAMING_HOLD      stream duration per response (default 1s)
  PERF_STREAMING_CHUNKS    content frames per response (default 100)
  PERF_STREAMING_TIMEOUT   per-request and drain timeout (default 60s)
  PERF_SUSTAINED_LEVELS    comma-separated fixed worker counts
                           (default 32,48,64,128)
  PERF_SUSTAINED_DURATION  time per sustained level (default 300s)
  PERF_IDLE_SETTLE         quiet interval before the idle sample (default 15s)
  PERF_IDLE_SAMPLE         idle CPU sampling window (default 30s)
  PERF_RSS_SAMPLE_MS       stress RSS sampling cadence (default 100)
  PERF_ALLOW_DIRTY         1 allows docs to run on an uncommitted tree
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
    -bench='^BenchmarkStorageWriter' \
    -benchtime=2s -benchmem -count=1 ./internal/storage
  scripts/check.sh go test -run='^$' \
    -bench='Benchmark(QueryBeforeEquality|QueryBeforeCount)$' \
    -benchtime=1x -benchmem -count=1 ./internal/storage
  scripts/check.sh go test -run='^$' \
    -bench='^(BenchmarkRequestMetadata$|BenchmarkProxyDurableContention)' \
    -benchtime=2s -benchmem -count=1 ./internal/proxy
}

run_image() {
  command -v docker >/dev/null || die "docker is required for image measurements"
  docker build --platform linux/amd64 -t "$IMAGE_TAG" . >"$PERF_DIR/image-build.log" 2>&1
  echo "image: $IMAGE_TAG"
  docker image inspect "$IMAGE_TAG" --format 'content-bytes={{.Size}}'
  local image_bytes archive_bytes binary_bytes
  image_bytes="$(docker image inspect "$IMAGE_TAG" --format '{{.Size}}')"
  archive_bytes="$(docker save "$IMAGE_TAG" | gzip -9 | wc -c)"
  printf 'compressed-bytes=%s\n' "$archive_bytes"
  CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags='-s -w' \
    -o "$PERF_DIR/millivolt" ./cmd/proxy
  binary_bytes="$(stat -c '%s' "$PERF_DIR/millivolt")"
  python3 - "$PERF_DIR/image.json" "$binary_bytes" "$image_bytes" "$archive_bytes" <<'PY'
import json
import sys

path, binary, image, archive = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4])
with open(path, "w", encoding="utf-8") as out:
    json.dump({"binary_bytes": binary, "image_bytes": image, "archive_bytes": archive}, out)
    out.write("\n")
PY
  echo "build log: $PERF_DIR/image-build.log"
}

# run_idle samples the native proxy process after a quiet interval: one RSS
# read, then CPU ticks across the sampling window. The process is started and
# stopped only through scripts/dev.sh.
run_idle() {
  local settle="${PERF_IDLE_SETTLE:-15s}" sample="${PERF_IDLE_SAMPLE:-30s}"
  [[ "$settle" =~ ^[1-9][0-9]*s$ ]] || die "PERF_IDLE_SETTLE must be seconds, for example 15s"
  [[ "$sample" =~ ^[1-9][0-9]*s$ ]] || die "PERF_IDLE_SAMPLE must be seconds, for example 30s"
  local settle_s="${settle%s}" sample_s="${sample%s}"
  local pid_file="/tmp/millivolt/millivolt-dev-${PERF_PORT}.pid"
  echo "idle: settle ${settle_s}s, sample ${sample_s}s"
  (
    set -e
    export DEV_PORT="$PERF_PORT" DEV_DB="$PERF_DB" DEV_COPY_DB=0
    export MILLIVOLT_OPERATOR_TOKEN="${MILLIVOLT_OPERATOR_TOKEN:-perf-instance-credential}"
    trap 'scripts/dev.sh stop' EXIT
    scripts/dev.sh >"$PERF_DIR/dev-idle.log" 2>&1
    local pid hz rss before after ticks percent
    pid="$(cat "$pid_file")"
    [[ "$pid" =~ ^[0-9]+$ ]] || die "idle: pid file has no pid"
    sleep "$settle_s"
    rss="$(awk '/^VmRSS:/ {print $2}' "/proc/$pid/status")"
    hz="$(getconf CLK_TCK)"
    before="$(awk '{print $14 + $15}' "/proc/$pid/stat")"
    sleep "$sample_s"
    after="$(awk '{print $14 + $15}' "/proc/$pid/stat")"
    ticks=$(( after - before ))
    percent="$(python3 -c 'print(f"{100.0*int(__import__("sys").argv[1])/(int(__import__("sys").argv[2])*int(__import__("sys").argv[3])):.6f}")' "$ticks" "$hz" "$sample_s")"
    python3 - "$PERF_DIR/idle.json" "$rss" "$ticks" "$sample_s" "$percent" "$settle_s" <<'PY'
import json
import sys

path, rss, ticks, seconds, percent, settle = sys.argv[1], int(sys.argv[2]), int(sys.argv[3]), int(sys.argv[4]), float(sys.argv[5]), int(sys.argv[6])
with open(path, "w", encoding="utf-8") as out:
    json.dump({"idle_rss_kib": rss, "idle_cpu_ticks": ticks, "idle_seconds": seconds,
               "idle_cpu_percent": percent, "idle_settle_seconds": settle}, out)
    out.write("\n")
PY
    echo "idle: ${rss} KiB, ${ticks} ticks over ${sample_s}s"
  ) >"$PERF_DIR/idle.log" 2>&1
}

run_one() {
  local name="$1"
  shift
  local allow_drops=0
  if [ "${1:-}" = "--allow-drops" ]; then
    allow_drops=1
    shift
  fi
  echo "stress: $name (fresh private database)"
  mkdir -p "$PERF_DIR/stress"
  (
    set -e
    export DEV_PORT="$PERF_PORT" DEV_DB="$PERF_DB" DEV_COPY_DB=0
    export MILLIVOLT_OPERATOR_TOKEN="${MILLIVOLT_OPERATOR_TOKEN:-perf-instance-credential}"
    trap 'scripts/dev.sh stop >"$PERF_DIR/dev-${name}.stop.log" 2>&1' EXIT
    scripts/dev.sh >"$PERF_DIR/dev-${name}.log" 2>&1
    if (( allow_drops )); then
      "$PERF_DIR/stress-bin" -target "http://${PERF_HOST}:${PERF_PORT}" -allow-drops "$@"
    else
      "$PERF_DIR/stress-bin" -target "http://${PERF_HOST}:${PERF_PORT}" "$@"
    fi
  ) >"$PERF_DIR/stress/${name}.jsonl"
  cat "$PERF_DIR/stress/${name}.jsonl"
}

run_sustained() {
  go build -trimpath -o "$PERF_DIR/stress-bin" ./cmd/stress
  command -v curl >/dev/null || die "curl is required for dev lifecycle readiness"
  local levels="${PERF_SUSTAINED_LEVELS:-32,48,64,128}" duration="${PERF_SUSTAINED_DURATION:-300s}" level
  local IFS=,
  for level in $levels; do
    [[ "$level" =~ ^[1-9][0-9]*$ ]] || die "PERF_SUSTAINED_LEVELS must be positive integers"
    run_one "sustained-${level}" --allow-drops -concurrency "$level" -duration "$duration" -stream-duration 0 -timeout 60s
  done
  echo "raw output: $PERF_DIR"
}

run_stress() {
  local stress="$PERF_DIR/stress-bin"
  go build -trimpath -o "$stress" ./cmd/stress
  command -v curl >/dev/null || die "curl is required for dev lifecycle readiness"

  run_one fast -concurrency 1,8,32,128 -duration 5s -stream-duration 0
  run_one streaming -concurrency 1,8,32,128,512 -duration 5s -stream-duration 1s
  echo "raw output: $PERF_DIR"
}

# run_streaming is the long end-to-end SSE throughput ramp. Each level gets a
# fresh private instance and a fixed ten-or-more-frame stream; the stress tool
# reports requests/s, SSE events/s, response bytes/s and output tokens/s from
# the exact responses the client accepted. The 1024 default level is above the
# example max_conns_per_host (512) and therefore measures queueing/plateau, not
# 1024 simultaneous upstream connections.
run_streaming() {
  local stress="$PERF_DIR/stress-bin"
  go build -trimpath -o "$stress" ./cmd/stress
  command -v curl >/dev/null || die "curl is required for dev lifecycle readiness"

  local levels="${PERF_STREAMING_LEVELS:-1,8,32,64,128,256,512,1024}"
  local duration="${PERF_STREAMING_DURATION:-60s}"
  local hold="${PERF_STREAMING_HOLD:-1s}"
  local chunks="${PERF_STREAMING_CHUNKS:-100}"
  local timeout="${PERF_STREAMING_TIMEOUT:-60s}"
  local level
  [[ "$chunks" =~ ^[1-9][0-9]*$ ]] || die "PERF_STREAMING_CHUNKS must be a positive integer"
  local IFS=,
  for level in $levels; do
    [[ "$level" =~ ^[1-9][0-9]*$ ]] || die "PERF_STREAMING_LEVELS must be positive integers"
    run_one "streaming-${level}" \
      -concurrency "$level" -duration "$duration" -stream-duration "$hold" \
      -chunks "$chunks" -timeout "$timeout"
  done
  echo "raw output: $PERF_DIR"
}

# run_bench_docs records every benchmark the published page or the design notes
# cite. Each group is a separate log so the collector owns the parse.
run_bench_docs() {
  mkdir -p "$PERF_DIR/bench"
  scripts/check.sh go test -run='^$' \
    -bench='^(BenchmarkProjectionFootprint|BenchmarkStoreDiskBytes|BenchmarkProjectionProcessRSS)' \
    -benchtime=1x -count=1 ./internal/web >"$PERF_DIR/bench/web-storage.log" 2>&1
  scripts/check.sh go test -run='^$' \
    -bench='^(BenchmarkHandleLogPage|BenchmarkAggregateUnchanged|BenchmarkKPI|BenchmarkAggregateFold|BenchmarkProjectionMetricAppend|BenchmarkObserveModelNameEvent|BenchmarkObserveModelNameEventWired|BenchmarkObserveModelNames4096)' \
    -benchtime=1x -count=1 ./internal/web >"$PERF_DIR/bench/web-aggregate.log" 2>&1
  scripts/check.sh go test -run='^$' \
    -bench='^BenchmarkStorageWriter' \
    -benchtime=2s -benchmem -count=1 ./internal/storage >"$PERF_DIR/bench/storage-writer.log" 2>&1
  scripts/check.sh go test -run='^$' \
    -bench='^BenchmarkQueryBeforeEquality' \
    -benchtime=1x -benchmem -count=1 ./internal/storage >"$PERF_DIR/bench/storage-query.log" 2>&1
  scripts/check.sh go test -run='^$' \
    -bench='^(BenchmarkRequestMetadata$|BenchmarkProxyDurableContention)' \
    -benchtime=2s -benchmem -count=1 ./internal/proxy >"$PERF_DIR/bench/proxy.log" 2>&1
  scripts/check.sh go test -run='^$' \
    -bench='^(BenchmarkSkipJSONString|BenchmarkRequestMetadataShapes)' \
    -benchtime=1x -count=1 ./internal/metrics ./internal/proxy >"$PERF_DIR/bench/scanners.log" 2>&1
  scripts/check.sh go test -run='^$' \
    -bench='^(BenchmarkAnalyzerFeed|BenchmarkAnalyzerToolNameDedupe)' \
    -benchtime=1x -count=1 ./internal/sse >"$PERF_DIR/bench/sse.log" 2>&1
}

duration_seconds() {
  local value="$1" name="$2"
  [[ "$value" =~ ^[1-9][0-9]*s$ ]] || die "$name must be seconds, for example 5s"
  printf '%s' "${value%s}"
}

run_docs() {
  if [ "${PERF_ALLOW_DIRTY:-0}" != 1 ] && [ -n "$(git status --porcelain --untracked-files=all)" ]; then
    die "the working tree is not clean; commit the measurement code first or set PERF_ALLOW_DIRTY=1"
  fi
  local fast_levels="${PERF_FAST_LEVELS:-1,8,32,128}"
  local fast_duration="${PERF_FAST_DURATION:-5s}"
  local fast_long="${PERF_FAST_LONG_DURATION:-10s}"
  local sustained_levels="${PERF_SUSTAINED_LEVELS:-32,48,64,128}"
  local sustained_duration="${PERF_SUSTAINED_DURATION:-300s}"
  local streaming_levels="${PERF_STREAMING_LEVELS:-1,8,32,64,128,256,512,1024}"
  local streaming_duration="${PERF_STREAMING_DURATION:-60s}"
  local streaming_hold="${PERF_STREAMING_HOLD:-1s}"
  local streaming_chunks="${PERF_STREAMING_CHUNKS:-100}"
  local streaming_timeout="${PERF_STREAMING_TIMEOUT:-60s}"
  local fast_s sustained_s streaming_s hold_s
  fast_s="$(duration_seconds "$fast_duration" PERF_FAST_DURATION)"
  duration_seconds "$fast_long" PERF_FAST_LONG_DURATION >/dev/null
  sustained_s="$(duration_seconds "$sustained_duration" PERF_SUSTAINED_DURATION)"
  streaming_s="$(duration_seconds "$streaming_duration" PERF_STREAMING_DURATION)"
  hold_s="$(duration_seconds "$streaming_hold" PERF_STREAMING_HOLD)"
  [[ "$streaming_chunks" =~ ^[1-9][0-9]*$ ]] || die "PERF_STREAMING_CHUNKS must be a positive integer"

  command -v curl >/dev/null || die "curl is required for dev lifecycle readiness"
  mkdir -p "$PERF_DIR/stress"
  awk '{print $1}' /proc/loadavg >"$PERF_DIR/load-start"
  run_image
  run_idle
  run_bench_docs

  go build -trimpath -o "$PERF_DIR/stress-bin" ./cmd/stress
  local IFS=,
  local level
  for level in $fast_levels; do
    [[ "$level" =~ ^[1-9][0-9]*$ ]] || die "PERF_FAST_LEVELS must be positive integers"
    run_one "fast-${level}" -concurrency "$level" -duration "$fast_duration" -stream-duration 0
  done
  run_one "fast-128-${fast_long}" -concurrency 128 -duration "$fast_long" -stream-duration 0
  for level in $streaming_levels; do
    [[ "$level" =~ ^[1-9][0-9]*$ ]] || die "PERF_STREAMING_LEVELS must be positive integers"
    run_one "concurrency-${level}" -concurrency "$level" -duration 5s -stream-duration 1s
  done
  for level in $sustained_levels; do
    [[ "$level" =~ ^[1-9][0-9]*$ ]] || die "PERF_SUSTAINED_LEVELS must be positive integers"
    run_one "sustained-${level}" --allow-drops -concurrency "$level" -duration "$sustained_duration" -stream-duration 0 -timeout 60s
  done
  for level in $streaming_levels; do
    run_one "throughput-${level}" -concurrency "$level" -duration "$streaming_duration" \
      -stream-duration "$streaming_hold" -chunks "$streaming_chunks" -timeout "$streaming_timeout"
  done
  awk '{print $1}' /proc/loadavg >"$PERF_DIR/load-end"

  python3 - "$PERF_DIR" "$fast_s" "$fast_long" "$sustained_s" "$streaming_s" "$hold_s" \
    "$streaming_chunks" "$RSS_SAMPLE_MS" "$fast_levels" "$sustained_levels" "$streaming_levels" <<'PY'
import json
import sys

(raw, fast_s, fast_long, sustained_s, streaming_s, hold_s, chunks, rss, fast_levels,
 sustained_levels, streaming_levels) = sys.argv[1:]
raw = raw.rstrip("/")
fast = [f"stress/fast-{level}.jsonl" for level in fast_levels.split(",")]
fast.append(f"stress/fast-128-{fast_long.rstrip('s')}s.jsonl")
manifest = {
    "artifacts": "image.json",
    "idle": "idle.json",
    "load": "load.json",
    "rss_sampling_ms": int(rss),
    "fast_duration_s": int(fast_s),
    "fast_long_duration_s": int(fast_long.rstrip("s")),
    "sustained_duration_s": int(sustained_s),
    "streaming_duration_s": int(streaming_s),
    "streaming_hold_s": int(hold_s),
    "streaming_chunks": int(chunks),
    "fast": fast,
    "streaming_concurrency": [f"stress/concurrency-{level}.jsonl" for level in streaming_levels.split(",")],
    "sustained": [f"stress/sustained-{level}.jsonl" for level in sustained_levels.split(",")],
    "streaming_throughput": [f"stress/throughput-{level}.jsonl" for level in streaming_levels.split(",")],
    "benchmarks": [
        "bench/web-storage.log",
        "bench/web-aggregate.log",
        "bench/storage-writer.log",
        "bench/storage-query.log",
        "bench/proxy.log",
        "bench/scanners.log",
        "bench/sse.log",
    ],
}
with open(raw + "/manifest.json", "w", encoding="utf-8") as out:
    json.dump(manifest, out, indent=2)
    out.write("\n")
with open(raw + "/load.json", "w", encoding="utf-8") as out:
    start = open(raw + "/load-start", encoding="utf-8").read().strip()
    end = open(raw + "/load-end", encoding="utf-8").read().strip()
    json.dump({"start": float(start), "end": float(end)}, out)
    out.write("\n")
PY

  python3 scripts/perf_collect.py --raw "$PERF_DIR" --out perf/results.json
  python3 scripts/perf_docs.py --write
  echo "raw output: $PERF_DIR"
  echo "bundle: perf/results.json"
  echo "page: docs/performance.md"
}

mode="${1:-all}"
case "$mode" in
  bench) run_bench ;;
  stress) run_stress ;;
  streaming) run_streaming ;;
  sustained) run_sustained ;;
  image) run_image ;;
  docs) run_docs ;;
  all) run_image; run_bench; run_stress ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 2 ;;
esac
