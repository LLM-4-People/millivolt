#!/usr/bin/env bash
# promote.sh - the one owner for keeping `testing` promotable.
#
# The promotion PR is rejected when `origin/main` is not an ancestor of the
# testing tip. That check belongs here, before a push and before a full gate
# run, so the same merge is not discovered after the PR opens.
#
# It never pushes `main` and never merges a pull request.
set -euo pipefail
cd "$(dirname "$0")/.."

die() { echo "promote: $*" >&2; exit 1; }
usage() {
  cat <<'EOF'
usage: scripts/promote.sh [check|sync|gate|push]

  check  verify that origin/main is an ancestor of the current branch tip
  sync   fetch origin and merge origin/main into the current branch
  gate   check, then run the local push gate (scripts/check.sh)
  push   gate, then push the current testing tip to origin/testing
EOF
}

branch() { git branch --show-current; }

check() {
  # CI owns its own checkout and the promotion workflow repeats this check with
  # the exact PR head. This guard is for the local push gate.
  if [ -n "${GITHUB_ACTIONS:-}" ]; then
    return 0
  fi
  local current
  current="$(branch)"
  if [ "$current" = main ]; then
    die "refusing to run the push gate on main; promote through a pull request"
  fi
  # A feature branch is pushed to testing with HEAD:testing, so the branch name
  # is not the promotion target. Every branch that can reach testing must carry
  # current main, not just a branch literally named testing.
  git rev-parse --verify origin/main >/dev/null 2>&1 \
    || die "origin/main is not available; fetch it before running the push gate"
  git fetch -q origin main || die "cannot fetch origin/main"
  if ! git merge-base --is-ancestor origin/main HEAD; then
    die "origin/main is not an ancestor of ${current:-the detached HEAD}; run scripts/promote.sh sync before pushing"
  fi
}

sync() {
  [ "$(branch)" != main ] || die "sync does not run on main"
  [ -n "$(branch)" ] || die "sync needs a branch, not a detached HEAD"
  [ -z "$(git status --porcelain --untracked-files=all)" ] \
    || die "commit or stash the working tree before syncing main"
  git fetch -q origin main || die "cannot fetch origin/main"
  if git merge-base --is-ancestor origin/main HEAD; then
    echo "promote: $(branch) already contains origin/main"
    return 0
  fi
  git merge --no-edit origin/main
  git merge-base --is-ancestor origin/main HEAD \
    || die "the merge did not make origin/main an ancestor"
  echo "promote: $(branch) now contains origin/main"
}

gate() {
  check
  scripts/check.sh
}

push() {
  gate
  [ "$(branch)" != main ] || die "push never runs on main"
  git push origin HEAD:testing
}

case "${1:-check}" in
  check) check ;;
  sync) sync ;;
  gate) gate ;;
  push) push ;;
  -h|--help|help) usage ;;
  *) usage >&2; exit 2 ;;
esac
