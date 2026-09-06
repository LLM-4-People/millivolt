#!/usr/bin/env bash
# dev.sh - build the latest source and run a SEPARATE dev instance of
# millivolt, so dashboard/proxy changes can be tested live without ever
# restarting the main instance (which keeps serving :8080).
#
# The dev instance runs on its own port (default 127.0.0.1:8081) with its own
# scratch DB and private copy of the public example (or an explicitly selected
# config), so it shares nothing mutable with prod and
# never touches the main instance's port (claimPort kills only the process on
# the SAME port, so the two can never collide).
#
# Usage:
#   scripts/dev.sh          build latest source + (re)start the dev instance
#   scripts/dev.sh stop     stop the dev instance
#   scripts/dev.sh status   print whether it's running and its URL
#
# Env overrides:
#   DEV_PORT   dev listen port          (default 8081)
#   DEV_HOST   dev listen host          (default 127.0.0.1)
#   DEV_DB     dev scratch DB path      (default /tmp/millivolt/millivolt-dev.db;
#                                        "none" = in-memory)
#   DEV_CONFIG source config file (default proxy.example.yaml, relative to repo root)
#   DEV_COPY_DB   0=fresh, 1=private online backup, redacted=docs-safe metrics
set -euo pipefail
cd "$(dirname "$0")/.."   # repo root

DEV_PORT="${DEV_PORT:-8081}"
DEV_HOST="${DEV_HOST:-127.0.0.1}"
DEV_CONFIG="${DEV_CONFIG:-proxy.example.yaml}"
DEV_COPY_DB="${DEV_COPY_DB:-0}"
# Reserved disposable namespace; an internal safety boundary, not a setting.
DEV_DIR=/tmp/millivolt
# Scratch DB lives in /tmp/millivolt (not the repo) so it never dirties the tree.
DEV_DB="${DEV_DB:-${DEV_DIR}/millivolt-dev.db}"
# Safety boundary, including status/stop: overrides can never target production
# or arbitrary files. This namespace belongs exclusively to disposable dev data.
die() { echo "dev: $*" >&2; exit 1; }
case "$DEV_COPY_DB" in 0|1|redacted) ;; *) die "DEV_COPY_DB must be 0, 1, or redacted" ;; esac
[[ "$DEV_PORT" =~ ^[0-9]{1,5}$ ]] || die "DEV_PORT must be a numeric port"
DEV_PORT=$((10#$DEV_PORT))
(( DEV_PORT >= 1024 && DEV_PORT <= 65535 && DEV_PORT != 8080 )) || die "DEV_PORT must be 1024..65535 and not production port 8080"
case "$DEV_HOST" in 127.0.0.1|localhost) ;; *) die "DEV_HOST must be loopback (127.0.0.1 or localhost)" ;; esac
if [ "$DEV_DB" != "none" ]; then
  DEV_DB="$(realpath -m -- "$DEV_DB")"
  case "$DEV_DB" in "$DEV_DIR"/millivolt-dev*.db) ;; *) die "DEV_DB must be a millivolt-dev*.db scratch file inside $DEV_DIR, or none" ;; esac
  [ "$(dirname "$DEV_DB")" = "$DEV_DIR" ] || die "DEV_DB must not contain subdirectories"
fi
LISTEN="${DEV_HOST}:${DEV_PORT}"
DEV_BASE="$DEV_DIR/millivolt-dev-${DEV_PORT}"
PID_FILE="$DEV_BASE.pid"
LOG_FILE="$DEV_BASE.log"
CONFIG_FILE="$DEV_BASE.yaml"
BIN="$DEV_BASE-bin"
mkdir -p "$DEV_DIR"

is_running() {
  [ -f "$PID_FILE" ] || return 1
  local pid args i
  pid="$(cat "$PID_FILE")"
  [[ "$pid" =~ ^[0-9]+$ ]] && (( pid > 1 )) || return 1
  [ -r "/proc/$pid/cmdline" ] || return 1
  mapfile -d '' -t args < "/proc/$pid/cmdline"
  # The rebuild child has a new executable name, but inherits this exact flag.
  for ((i=0; i+1<${#args[@]}; i++)); do
    if [ "${args[i]}" = -pid-file ] && [ "${args[i+1]}" = "$PID_FILE" ]; then
      kill -0 "$pid" 2>/dev/null
      return $?
    fi
  done
  return 1
}

stop() {
  if is_running; then
    local pid; pid="$(cat "$PID_FILE")"
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    if is_running; then kill -9 "$pid" 2>/dev/null || true; fi
    echo "stopped dev instance (pid $pid)"
  else
    echo "dev instance not running"
  fi
  rm -f "$PID_FILE"
}

case "${1:-up}" in
  stop)   stop; exit 0 ;;
  status)
    if is_running; then echo "dev instance running (pid $(cat "$PID_FILE")) at http://${LISTEN}"; else echo "dev instance not running"; fi
    exit 0 ;;
  up|restart|"") ;; # fall through
  *) echo "unknown command: $1 (use: up | stop | status)" >&2; exit 2 ;;
esac

# Reject an unavailable source before rebuilding, stopping, or removing any
# scratch data. Stop/status intentionally do not depend on a source config.
[ -f "$DEV_CONFIG" ] && [ -r "$DEV_CONFIG" ] || die "DEV_CONFIG must name a readable regular file: $DEV_CONFIG"
[ ! "$DEV_CONFIG" -ef "$CONFIG_FILE" ] || die "DEV_CONFIG must not be this instance's private destination"
if [ "$DEV_COPY_DB" != 0 ]; then
  [ "$DEV_DB" != none ] || die "DEV_COPY_DB requires a scratch database file"
  [ -f proxy.db ] && [ -r proxy.db ] || die "DEV_COPY_DB requires a readable proxy.db source"
fi

# 1. Build latest source. Fail fast - never run a stale binary.
echo "building…"
if ! go build -o "$BIN" ./cmd/proxy; then
  echo "build failed; dev instance NOT (re)started" >&2
  exit 1
fi

# 2. Stop any prior dev instance on this port (PID-file scoped; never pkill).
if is_running; then stop; fi

# 3. Scratch DB. Explicit copies use the online-backup owner. Redaction derives
# a new metrics-only file before a server can read it; raw pages are never served.
if [ "$DEV_DB" != "none" ]; then
  rm -f "$DEV_DB" "$DEV_DB-shm" "$DEV_DB-wal"
  if [ "$DEV_COPY_DB" != 0 ]; then
    backup_args=()
    if [ "$DEV_COPY_DB" = redacted ]; then backup_args+=(--docs-safe); fi
    python3 scripts/backup_db.py "${backup_args[@]}" proxy.db "$DEV_DB"
    echo "seeded dev DB from a consistent read-only snapshot (mode: $DEV_COPY_DB)"
  fi
fi

# 4. Settings writes only to this private copy. Local operator configuration
# is never an implicit input; DEV_CONFIG opts in. Headers may contain secrets.
install -m 600 -T -- "$DEV_CONFIG" "$CONFIG_FILE"

# 5. Spawn detached. The proxy's own claimPort frees THIS port if a stale dev
#    binary still holds it; the main instance on :8080 is untouched.
DBFLAG="$DEV_DB"
# -pid-file: a UI-triggered restart (POST /admin/restart) hands off to a NEW
# pid; the child rewrites this file on boot, so stop/status stay authoritative.
nohup "$BIN" -config "$CONFIG_FILE" -listen "$LISTEN" -db-path "$DBFLAG" -pid-file "$PID_FILE" >"$LOG_FILE" 2>&1 &
PID=$!
echo "$PID" > "$PID_FILE"

# 6. Wait for readiness (up to ~5s), then report.
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://${LISTEN}/metrics/bootstrap" 2>/dev/null; then
    echo "dev instance up at http://${LISTEN}  (pid $PID, db=${DBFLAG}, log: $LOG_FILE)"
    exit 0
  fi
  if ! kill -0 "$PID" 2>/dev/null; then
    echo "dev instance died on startup; last log:" >&2
    tail -20 "$LOG_FILE" >&2 || true
    exit 1
  fi
  sleep 0.1
done
echo "dev instance did not become ready in time; see $LOG_FILE" >&2
exit 1
