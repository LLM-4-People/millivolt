#!/usr/bin/env bash
# dev-traffic.sh - the sole lifecycle owner of the dev fixture-traffic driver.
# Never kill the driver with pkill/kill/fuser or broad pattern matches: this
# script owns the PID-file identity, and stop is the only supported way down.
#
# Drives neutral loopback fixture traffic at the isolated dev instance
# (scripts/dev.sh), so the dashboard has a realistic multi-bucket timeline:
# varied token/cost usage, recovered 429s (rate-limited, not errors) and one
# final 400 per run. Never targets the main instance on :8080.
#
# Usage:
#   scripts/dev-traffic.sh          start the driver (refuses if already running)
#   scripts/dev-traffic.sh stop     SIGTERM the owned driver, wait, then escalate
#   scripts/dev-traffic.sh status   print whether it runs and its log
#
# Env overrides:
#   DEV_PORT           dev instance port    (default 8081)
#   DEV_TRAFFIC_BURSTS burst count          (default 5)
#   DEV_TRAFFIC_GAP    seconds between bursts (default 62, just over one
#                      chart bucket so the timeline gains a bucket per burst)
set -euo pipefail
cd "$(dirname "$0")/.."   # repo root

DEV_PORT="${DEV_PORT:-8081}"
DEV_DIR=/tmp/millivolt   # the reserved disposable namespace dev.sh owns
STATE="$DEV_DIR/dev-traffic"
PID_FILE="$STATE.pid"
LOG_FILE="$DEV_DIR/dev-traffic.log"
DRIVER="scripts/dev-traffic.py"
mkdir -p "$DEV_DIR"

die() { echo "dev-traffic: $*" >&2; exit 1; }
case "$DEV_PORT" in ''|*[!0-9]*) die "DEV_PORT must be numeric" ;; esac
(( DEV_PORT >= 1024 && DEV_PORT <= 65535 && DEV_PORT != 8080 )) \
  || die "DEV_PORT must be 1024..65535 and not production port 8080"

is_running() {
  [ -f "$PID_FILE" ] || return 1
  local pid
  pid="$(cat "$PID_FILE")"
  [[ "$pid" =~ ^[0-9]+$ ]] && (( pid > 1 )) || return 1
  # PID-file identity: the driver must still be the process behind the pid.
  grep -q "dev-traffic.py" "/proc/$pid/cmdline" 2>/dev/null || return 1
  kill -0 "$pid" 2>/dev/null
}

start() {
  if is_running; then
    die "already running (pid $(cat "$PID_FILE")); stop it first"
  fi
  [ -f "$DRIVER" ] || die "driver missing: $DRIVER"
  # Any HTTP response proves the instance is listening - including the
  # operator gate's 401 - so no -f here; only a refused connection fails.
  curl -s -o /dev/null --max-time 3 "http://127.0.0.1:$DEV_PORT/" 2>/dev/null \
    || die "no dev instance at 127.0.0.1:$DEV_PORT - start scripts/dev.sh first"
  rm -f "$PID_FILE"
  : > "$LOG_FILE"
  DEV_PORT="$DEV_PORT" nohup python3 "$DRIVER" >> "$LOG_FILE" 2>&1 < /dev/null &
  local pid=$!
  echo "$pid" > "$PID_FILE"
  sleep 0.4
  is_running || { cat "$LOG_FILE" >&2; rm -f "$PID_FILE"; die "driver failed to start"; }
  echo "dev traffic running (pid $pid) against 127.0.0.1:$DEV_PORT; log: $LOG_FILE"
}

stop() {
  if ! is_running; then
    rm -f "$PID_FILE"
    echo "dev traffic not running"
    return 0
  fi
  local pid
  pid="$(cat "$PID_FILE")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  if is_running; then kill -9 "$pid" 2>/dev/null || true; fi
  rm -f "$PID_FILE"
  echo "stopped dev traffic (pid $pid)"
}

status() {
  if is_running; then
    echo "dev traffic running (pid $(cat "$PID_FILE")) against 127.0.0.1:$DEV_PORT; log: $LOG_FILE"
  else
    echo "dev traffic not running"
    return 1
  fi
}

case "${1:-}" in
  '') start ;;
  stop) stop ;;
  status) status ;;
  *) die "usage: scripts/dev-traffic.sh [stop|status]" ;;
esac
