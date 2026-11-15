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
#   DEV_DB     dev scratch DB path      (default /tmp/millivolt/millivolt-dev-<port>.db;
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
# _DEV_DB_OVERRIDE keeps an explicit DEV_DB (or "none") until the port is
# normalized below: the default is keyed by that normalized port.
_DEV_DB_OVERRIDE="${DEV_DB:-}"
# Safety boundary, including status/stop: overrides can never target production
# or arbitrary files. This namespace belongs exclusively to disposable dev data.
die() { echo "dev: $*" >&2; exit 1; }
case "$DEV_COPY_DB" in 0|1|redacted) ;; *) die "DEV_COPY_DB must be 0, 1, or redacted" ;; esac
[[ "$DEV_PORT" =~ ^[0-9]{1,5}$ ]] || die "DEV_PORT must be a numeric port"
DEV_PORT=$((10#$DEV_PORT))
(( DEV_PORT >= 1024 && DEV_PORT <= 65535 && DEV_PORT != 8080 )) || die "DEV_PORT must be 1024..65535 and not production port 8080"
case "$DEV_HOST" in 127.0.0.1|localhost) ;; *) die "DEV_HOST must be loopback (127.0.0.1 or localhost)" ;; esac
# Scratch DB lives in /tmp/millivolt (not the repo) so it never dirties the tree.
# It is keyed by port like every other instance artefact: the start sequence
# deletes the file so each run begins fresh, and one shared default meant a
# second instance on another port hard-deleted the database the first instance
# had open, with its -wal and -shm still in use.
# dev_db_path validates an explicit scratch database against the reserved
# namespace and THIS instance's port, then prints the resolved path. It is a
# function so the rule can be driven directly by tests, the way the drain-wait
# owners are.
#
# The start sequence hard-deletes this file, so a name that claims a different
# instance's port is a live database being destroyed:
# DEV_DB=...-8082.db with DEV_PORT=8081 passed the old namespace-only guard and
# then deleted the database the 8082 instance had open. A name that names no
# port is an explicit operator choice (a documentation or CI fixture) and stays
# allowed; a name that names a port must name this one.
#
# Two rules make that hold rather than nearly hold:
#   - the final component must be the literal file, never a symlink. `realpath
#     -m` follows it, so a link named ...-ci.db pointing at another instance's
#     database resolved to that database and the start sequence deleted it;
#   - any dash-delimited all-digit field is a port claim, of any length, and is
#     compared with leading zeros stripped. A four-digit bound skipped
#     `-065535`, which is 65535 and would have been accepted.
dev_db_path() {
  local raw="$1" port="$2" dir="$3" resolved bare field trimmed
  [ "$raw" = none ] && { printf 'none'; return 0; }
  resolved="$(realpath -ms -- "$raw")"
  [ "$(dirname "$resolved")" = "$dir" ] \
    || die "DEV_DB must be a scratch file directly inside $dir, or none"
  [ ! -L "$resolved" ] \
    || die "DEV_DB must be a regular file, not a symlink: ${resolved##*/}"
  bare="${resolved##*/}"
  case "$bare" in millivolt-dev*.db) ;; *) die "DEV_DB must be named millivolt-dev*.db inside $dir, or none: $bare" ;; esac
  local fields=()
  IFS=- read -r -a fields <<<"${bare#millivolt-dev}"
  for field in "${fields[@]}"; do
    field="${field%.db}"
    [[ "$field" =~ ^[0-9]+$ ]] || continue
    # Strip leading zeros rather than evaluating, so an unbounded digit run
    # cannot overflow the shell's arithmetic.
    trimmed="${field#"${field%%[!0]*}"}"
    [ "$trimmed" = "$port" ] \
      || die "DEV_DB names port ${field} but DEV_PORT is $port, so starting would delete another instance's database: $bare"
  done
  printf '%s' "$resolved"
}
DEV_DB="$(dev_db_path "${_DEV_DB_OVERRIDE:-${DEV_DIR}/millivolt-dev-${DEV_PORT}.db}" "$DEV_PORT" "$DEV_DIR")" \
  || exit 1
LISTEN="${DEV_HOST}:${DEV_PORT}"
DEV_BASE="$DEV_DIR/millivolt-dev-${DEV_PORT}"
PID_FILE="$DEV_BASE.pid"
LOG_FILE="$DEV_BASE.log"
CONFIG_FILE="$DEV_BASE.yaml"
BIN="$DEV_BASE-bin"
mkdir -p "$DEV_DIR"

# pid_file_pid prints the pid a pid file names when that pid is alive and
# carries this exact -pid-file flag. It is the primary identity: a rebuild child
# inherits the flag, so a UI-triggered restart stays tracked.
pid_file_pid() {
  local pid_file="$1" pid args i
  [ -f "$pid_file" ] || return 1
  pid="$(cat "$pid_file")"
  [[ "$pid" =~ ^[0-9]+$ ]] && (( pid > 1 )) || return 1
  pid_carries_pid_file "$pid" "$pid_file" || return 1
  printf '%s' "$pid"
}

# pid_carries_pid_file reports whether pid is alive and was started with this
# pid-file flag. An unrelated process that merely reused the pid is refused.
pid_carries_pid_file() {
  local pid="$1" pid_file="$2" args i
  [ -r "/proc/$pid/cmdline" ] || return 1
  mapfile -d '' -t args < "/proc/$pid/cmdline"
  for ((i=0; i+1<${#args[@]}; i++)); do
    if [ "${args[i]}" = -pid-file ] && [ "${args[i+1]}" = "$pid_file" ]; then
      kill -0 "$pid" 2>/dev/null
      return $?
    fi
  done
  return 1
}

# port_owner_pid prints the pid of the process LISTENING on this instance's
# port, or fails. It is the fallback identity: a handoff child rewrites the pid
# file only after it is fully booted, so a child that dies before readiness
# leaves the file naming itself while the parent keeps serving. Requiring the
# socket owner plus the same -pid-file flag keeps the match narrow.
port_owner_pid() {
  local port="$1" out pid
  command -v ss >/dev/null 2>&1 || return 1
  out="$(ss -tlnpH "sport = :$port" 2>/dev/null)" || return 1
  pid="${out#*pid=}"
  [ "$pid" != "$out" ] || return 1
  pid="${pid%%,*}"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  printf '%s' "$pid"
}

# running_pid prints the pid of the instance a pid file names, falling back to
# the listener on that file's port. The port comes from the file name, so the
# same helper serves DEV_PORT and every tracked instance.
running_pid() {
  local pid_file="${1:-$PID_FILE}" port pid
  if pid="$(pid_file_pid "$pid_file")"; then
    printf '%s' "$pid"
    return 0
  fi
  port="${pid_file##*/millivolt-dev-}"
  port="${port%.pid}"
  [[ "$port" =~ ^[0-9]+$ ]] || return 1
  pid="$(port_owner_pid "$port")" || return 1
  pid_carries_pid_file "$pid" "$pid_file" || return 1
  printf '%s' "$pid"
}

is_running() {
  running_pid "${1:-$PID_FILE}" >/dev/null
}

# tracked_ports prints the ports with a live dev instance, comma-separated. It
# is the discovery half of the no-instance report: DEV_PORT selects one
# instance, and naming the others keeps "not running" from reading as "nothing
# is running". It never identifies an instance for termination.
tracked_ports() {
  local pid_file port joined=''
  for pid_file in "$DEV_DIR"/millivolt-dev-*.pid; do
    [ -e "$pid_file" ] || continue
    is_running "$pid_file" || continue
    port="${pid_file##*/millivolt-dev-}"
    joined+="${joined:+, }${port%.pid}"
  done
  printf '%s' "$joined"
}

# not_running is the single owner of the "no instance for the resolved
# DEV_PORT" report; stop and status both use it. It only reports, so stop can
# never terminate an instance the caller did not identify.
not_running() {
  local ports; ports="$(tracked_ports)"
  if [ -n "$ports" ]; then
    echo "dev instance not running on ${LISTEN}; tracked dev instance ports: $ports (DEV_PORT selects the instance)"
  else
    echo "dev instance not running on ${LISTEN} (no running dev instance is tracked; DEV_PORT selects the instance)"
  fi
}

# go_duration_tenths prints the tenths of a second in a Go duration string (the
# grammar time.ParseDuration accepts in proxy.yaml), rounded up to the 100ms
# poll tick stop waits in. It is total: every other value stops the caller with
# the file and the value named, because silently substituting a default is how a
# normal stop came to SIGKILL the proxy mid-drain.
go_duration_tenths() {
  local raw="$1" origin="$2"
  # A drain wait beyond a day is a unit slip, never a stop. Internal safety
  # bound, not an operator setting.
  local bound_hours=24 bound_ns
  local rest="$raw" negative=0 number whole fraction unit unit_ns
  local digits fraction_ns unit_share unit_rest total_ns=0
  bound_ns=$(( bound_hours * 3600 * 1000000000 ))
  [ -n "$raw" ] || die "$origin: shutdown_timeout is empty"
  case "$rest" in
    +*) rest="${rest#+}" ;;
    -*) negative=1; rest="${rest#-}" ;;
  esac
  [ -n "$rest" ] || die "$origin: shutdown_timeout \"$raw\" is not a Go duration (want a number and unit such as 30s, 2m30s or 1h)"
  # time.ParseDuration reads a bare zero without a unit. The config loader then
  # refuses it as non-positive, so name that reason instead of the grammar.
  [ "$rest" != 0 ] || die "$origin: shutdown_timeout \"$raw\" must be positive"
  while [ -n "$rest" ]; do
    # time.ParseDuration reads a number, then every following character up to
    # the next digit or dot as the unit, and repeats. Modelling the same scan
    # keeps a unit the proxy rejects (5x, 5s # drain) from reading as a prefix
    # it accepts.
    if [[ ! "$rest" =~ ^([0-9]+(\.[0-9]*)?|\.[0-9]+)([^0-9.]*)(.*)$ ]]; then
      die "$origin: shutdown_timeout \"$raw\" is not a Go duration (want a number and unit such as 30s, 2m30s or 1h)"
    fi
    number="${BASH_REMATCH[1]}" unit="${BASH_REMATCH[3]}" rest="${BASH_REMATCH[4]}"
    case "$unit" in
      '') die "$origin: shutdown_timeout \"$raw\" is missing a unit (want a number and unit such as 30s, 2m30s or 1h)" ;;
      ns) unit_ns=1 ;;
      us|µs|μs) unit_ns=1000 ;;
      ms) unit_ns=1000000 ;;
      s) unit_ns=1000000000 ;;
      m) unit_ns=60000000000 ;;
      h) unit_ns=3600000000000 ;;
      *) die "$origin: shutdown_timeout \"$raw\" is not a Go duration (\"$unit\" is not a duration unit; want ns, us, ms, s, m or h)" ;;
    esac
    whole="${number%%.*}" fraction=""
    case "$number" in *.*) fraction="${number#*.}" ;; esac
    # Leading zeros cannot change the value, but they would trip the digit
    # bound and the base-10 arithmetic below.
    whole="${whole#"${whole%%[!0]*}"}"
    [ -n "$whole" ] || whole=0
    # The day bound makes every accepted term fit comfortably in 64 bits, and
    # checking the quotient first keeps the multiplication from wrapping.
    (( ${#whole} <= 14 )) || die "$origin: shutdown_timeout \"$raw\" is longer than ${bound_hours}h"
    (( 10#$whole <= bound_ns / unit_ns )) || die "$origin: shutdown_timeout \"$raw\" is longer than ${bound_hours}h"
    total_ns=$(( total_ns + 10#$whole * unit_ns ))
    if [ -n "$fraction" ]; then
      # A nanosecond is the smallest value time.Duration holds, so a fraction
      # finer than 13 digits contributes nothing for any unit up to hours.
      digits="${fraction:0:13}"
      # (digits * unit_ns) / 10^len(digits) without overflowing 64 bits: the
      # unit is stripped to a small factor times a power of ten, so the product
      # stays below the unit itself and the result is exact for every unit.
      unit_share=$unit_ns unit_rest=0
      while (( unit_share % 10 == 0 )); do
        unit_share=$(( unit_share / 10 ))
        unit_rest=$(( unit_rest + 1 ))
      done
      if (( ${#digits} >= unit_rest )); then
        fraction_ns=$(( 10#$digits * unit_share / 10 ** (${#digits} - unit_rest) ))
      else
        fraction_ns=$(( 10#$digits * unit_share * 10 ** (unit_rest - ${#digits}) ))
      fi
      total_ns=$(( total_ns + fraction_ns ))
    fi
    (( total_ns <= bound_ns )) || die "$origin: shutdown_timeout \"$raw\" is longer than ${bound_hours}h"
  done
  (( negative == 0 )) || die "$origin: shutdown_timeout \"$raw\" must be positive"
  (( total_ns > 0 )) || die "$origin: shutdown_timeout \"$raw\" must be positive"
  # Ceil to the 100ms poll tick stop() sleeps in.
  echo $(( (total_ns + 99999999) / 100000000 ))
}

# shutdown_timeout_value prints the top-level shutdown_timeout scalar of the
# instance config with YAML quoting, a trailing comment and the whitespace the
# proxy loader trims removed. The private config copy is rewritten from a file
# a human edits, so every shape the proxy accepts must read the same here. A
# missing, empty, repeated or nested-only key is refused rather than read as
# some other value.
shutdown_timeout_value() {
  local yaml="$1" count line value values
  [ -r "$yaml" ] || die "$yaml: cannot read the instance config to derive the drain wait"
  count="$(grep -c '^shutdown_timeout:' "$yaml" 2>/dev/null || true)"
  case "$count" in
    1) ;;
    0|'') die "$yaml: shutdown_timeout is not set; stop cannot derive the drain wait" ;;
    *)
      # Name every candidate: the operator has to choose which one the proxy
      # is meant to read.
      values="$(sed -n 's/^shutdown_timeout:[[:space:]]*//p' "$yaml" | tr '\n' ',')"
      die "$yaml: shutdown_timeout is set $count times (${values%,}); stop cannot choose the drain wait"
      ;;
  esac
  # A plain scalar may carry a trailing comment after whitespace. A quoted
  # scalar ends at its closing quote, so a # inside it stays part of the value.
  line="$(sed -n '/^shutdown_timeout:/{s/^shutdown_timeout:[[:space:]]*//;p;}' "$yaml")"
  line="${line%"${line##*[![:space:]]}"}"
  case "$line" in
    \"*) line="${line#\"}"; value="${line%%\"*}" ;;
    \'*) line="${line#\'}"; value="${line%%\'*}" ;;
    *) value="${line%%[[:space:]]#*}" ;;
  esac
  # The proxy loader trims the scalar, so an inner space in either form is not
  # part of the duration.
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  printf '%s' "$value"
}

# stop_grace_tenths derives the SIGTERM wait from the instance's own
# shutdown_timeout plus the margin the proxy itself uses before reclaiming a
# stale port. The stop sequence used to wait a flat 3.0s, while the documented
# default drain is 5s and the store flush is uncapped: a normal stop therefore
# SIGKILLed the process mid-drain, discarding the final write-channel drain the
# proxy takes care to finish. The configured value is the one owner of the
# number; a file that does not state a usable one is refused, never replaced by
# a default.
stop_grace_tenths() {
  local yaml="${CONFIG_FILE}"
  # cmd/proxy/portclaim.go sigtermCloseDrainMargin.
  local margin_s=5
  local value tenths
  # A command substitution subshell does not inherit errexit, so each nested
  # derivation's status is checked explicitly. Without this a refused value
  # printed nothing and the arithmetic below read the empty string as 0.
  value="$(shutdown_timeout_value "$yaml")" || exit 1
  tenths="$(go_duration_tenths "$value" "$yaml")" || exit 1
  echo $(( tenths + margin_s * 10 ))
}

stop() {
  local pid ticks
  if pid="$(running_pid)"; then
    # Derive the budget before signalling: a config this cannot read must
    # refuse the stop, never start one it has no wait for.
    ticks="$(stop_grace_tenths)"
    kill "$pid" 2>/dev/null || true
    # Poll at 100ms for the derived budget: the process is exiting cleanly for
    # that long, and only a genuinely wedged one reaches the SIGKILL.
    for _ in $(seq 1 "$ticks"); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    if is_running; then kill -9 "$pid" 2>/dev/null || true; fi
    echo "stopped dev instance (pid $pid)"
  else
    not_running
  fi
  rm -f "$PID_FILE"
}

case "${1:-up}" in
  stop)   stop; exit 0 ;;
  status)
    if pid="$(running_pid)"; then
      echo "dev instance running (pid $pid) at http://${LISTEN}"
    else
      not_running
    fi
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

# 6. Wait for readiness (up to ~5s), then report. /healthz is the
#    unauthenticated liveness probe: the operator gate may deny /metrics
#    when no MILLIVOLT_OPERATOR_TOKEN is exported, which says nothing about
#    process readiness.
for _ in $(seq 1 50); do
  if curl -fsS -o /dev/null "http://${LISTEN}/healthz" 2>/dev/null; then
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
