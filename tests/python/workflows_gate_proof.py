"""Check-run fixtures and the canonical testing gate shell proofs."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

from .workflows_jobs import GATE_CHECK_NAMES
from .workflows_support import (
    check_run_page,
    missing_commands,
    non_comment_lines,
    shell_declaration,
    shell_function,
    workflow_run_page,
    write_fixture,
)

GATE_FUNCTIONS = (
    "canonical_runs",
    "canonical_run",
    "suite_outcome",
)


def check_run(name: str, conclusion: str | None = "success", status: str = "completed",
              suite: int = 1) -> dict:
    return {"name": name, "status": status, "conclusion": conclusion,
            "check_suite": {"id": suite}}


GATED_RUNS = (
    check_run("unit"),
    check_run("browser"),
    check_run("container (linux/amd64)"),
    check_run("container (linux/arm64)"),
)
SKIPPED_GATED_RUNS = (
    check_run("unit", "skipped"),
    check_run("browser", "skipped"),
    check_run("container (linux/amd64)", "skipped"),
    check_run("container (linux/arm64)", "skipped"),
)


def run_record(sha: str, suite: int = 1, status: str = "completed",
               conclusion: str | None = "success",
               path: str = ".github/workflows/check.yml", event: str = "push",
               branch: str = "testing", created: str = "2026-01-01T00:00:00Z",
               run_id: int = 1) -> dict:
    return {
        "head_sha": sha,
        "path": path,
        "event": event,
        "head_branch": branch,
        "status": status,
        "conclusion": conclusion,
        "check_suite_id": suite,
        "created_at": created,
        "id": run_id,
    }


def suite_outcome_errors(run: str) -> list[str]:
    """Exercise the canonical suite classifier over synthetic check runs.

    The workflow owns the predicate; this test sources the same shell function
    and feeds it payloads instead of imitating its jq logic. A qualifying gate
    is one check suite containing completed, successful `unit`, `browser` and
    both native container legs. Names from any other suite, pending and failed
    duplicates, and skipped leftovers are all classified explicitly.
    """
    missing = [name for name in GATE_FUNCTIONS if not shell_function(run, name)]
    if missing:
        return ["check.yml: full-gate proof is missing " + ", ".join(missing)]
    if shutil.which("bash") is None or shutil.which("jq") is None:
        return ["workflows self-check: bash and jq are required to exercise the suite classifier"]
    body = "\n".join(shell_declaration(run, name) for name in GATE_FUNCTIONS)
    script = "set -euo pipefail\n" + body + '\nsuite_outcome "$1" "$2"\n'

    def outcome(payload: dict, suite: int = 1) -> str:
        result = subprocess.run(
            ["bash", "-c", script, "bash", json.dumps(payload, separators=(",", ":")), str(suite)],
            capture_output=True, text=True, timeout=10, check=False)
        if result.returncode != 0:
            raise RuntimeError((result.stderr or result.stdout).strip())
        return result.stdout.strip()

    errors = []

    def expect(label: str, payload: dict, expected: str, suite: int = 1) -> None:
        try:
            actual = outcome(payload, suite)
        except (OSError, RuntimeError, subprocess.TimeoutExpired) as error:
            errors.append(f"workflows self-check: {label}: {error}")
            return
        if actual != expected:
            errors.append(f"workflows self-check: {label} must classify as {expected}, got {actual}")

    expect("one suite with all four successful runs", {"check_runs": list(GATED_RUNS)}, "success")
    expect("a successful duplicate supersedes cancelled leftovers", {"check_runs": [
        check_run("unit", "cancelled", suite=1),
        check_run("unit", "success", suite=1),
        check_run("browser", "success", suite=1),
        check_run("container (linux/amd64)", "success", suite=1),
        check_run("container (linux/arm64)", "success", suite=1),
    ]}, "success")
    expect("browser missing from a suite with the other three successes", {"check_runs": [
        check_run("unit"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)"),
    ]}, "incomplete")
    expect("two amd64 successes plus an arm64 failure", {"check_runs": [
        check_run("unit"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)", "failure"),
    ]}, "failed")
    expect("arm64 missing from the only suite", {"check_runs": [
        check_run("unit"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
    ]}, "incomplete")
    expect("arm64 still pending in the only suite", {"check_runs": [
        check_run("unit"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)", status="in_progress", conclusion=None),
    ]}, "pending")
    expect("success in one suite with cancelled and failed duplicates in another", {"check_runs": [
        *GATED_RUNS,
        check_run("unit", "cancelled", suite=2),
        check_run("browser", "cancelled", suite=2),
        check_run("container (linux/amd64)", "failure", suite=2),
        check_run("container (linux/arm64)", "cancelled", suite=2),
    ]}, "success")
    expect("the four gate jobs split across four suites", {"check_runs": [
        check_run("unit", suite=1),
        check_run("browser", suite=2),
        check_run("container (linux/amd64)", suite=3),
        check_run("container (linux/arm64)", suite=4),
    ]}, "incomplete")
    expect("names from another suite cannot complete the canonical suite", {"check_runs": [
        check_run("unit", suite=1),
        check_run("browser", suite=1),
        check_run("container (linux/amd64)", suite=1),
        *[check_run(name, suite=77) for name in GATE_CHECK_NAMES],
    ]}, "incomplete")
    expect("a successful container-extra run cannot replace a missing leg", {"check_runs": [
        check_run("unit"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
        check_run("container-extra"),
    ]}, "incomplete")
    expect("failure without a successful full gate", {"check_runs": [
        check_run("unit", "failure"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)"),
    ]}, "failed")
    expect("cancelled duplicate runs without a successful full gate", {"check_runs": [
        check_run("unit", "cancelled", suite=2),
        check_run("browser", "cancelled", suite=2),
        check_run("container (linux/amd64)", "cancelled", suite=2),
        check_run("container (linux/arm64)", "cancelled", suite=2),
    ]}, "failed", suite=2)
    expect("cancelled duplicate beside the current pending run", {"check_runs": [
        check_run("unit", "cancelled", suite=2),
        check_run("browser", "cancelled", suite=2),
        check_run("unit", status="in_progress", conclusion=None, suite=1),
        check_run("browser", status="in_progress", conclusion=None, suite=1),
        check_run("container (linux/amd64)", status="in_progress", conclusion=None, suite=1),
        check_run("container (linux/arm64)", status="in_progress", conclusion=None, suite=1),
    ]}, "pending")
    expect("pending check runs", {"check_runs": [
        check_run("unit", status="in_progress"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
        check_run("container (linux/arm64)"),
    ]}, "pending")
    expect("skipped-only gate entries stay incomplete", {"check_runs": list(SKIPPED_GATED_RUNS)}, "incomplete")
    expect("non-skipped incomplete gate entries fail", {"check_runs": [
        check_run("unit"),
        check_run("browser"),
        check_run("container (linux/amd64)"),
    ]}, "incomplete")
    expect("a run without a check suite cannot qualify", {"check_runs": [
        {"name": name, "status": "completed", "conclusion": "success"}
        for name in GATE_CHECK_NAMES
    ]}, "incomplete")
    return errors


def full_gate_behavior_errors(run: str) -> list[str]:
    """Exercise the real full-gate step with fake git, gh and sleep.

    The step is extracted verbatim. Fake commands select a canonical run and
    suite, a forged workflow, a pull-request run, a wrong branch, an exact
    pending or incomplete run, a failed run, a docs-only gated ancestor, a code
    delta, a non-ancestor gated run, an incomplete fallback suite, no run at
    all, and a run list split past one page. The harness never injects `set -e`
    or the inline classifier: a missing guard must fail the proof.
    """
    required = missing_commands(run, ("set -euo pipefail",))
    if required:
        return ["check.yml: full-gate proof must keep these command lines: " + ", ".join(required)]
    if "source scripts/docs-paths.sh" in non_comment_lines(run):
        return ["check.yml: full-gate proof must not source the candidate classifier"]
    if shutil.which("bash") is None or shutil.which("jq") is None:
        return ["workflows self-check: bash and jq are required to exercise the full-gate proof"]
    fake_commands = r"""git() {
  case "$1" in
    merge-base)
      if [ "$2" = "--is-ancestor" ]; then
        grep -qxF "$3 $4" "$FIXTURES/ancestors"
        return $?
      fi
      echo base
      return 0
      ;;
    rev-list)
      # The proof asks for `--count <candidate>..<head>` only. A missing
      # distance fixture defaults to one so a scenario with one ancestor does
      # not need to restate it.
      range="${3:-${2:-}}"
      candidate="${range%%..*}"
      if [ -f "$FIXTURES/distance-$candidate" ]; then
        cat "$FIXTURES/distance-$candidate"
      else
        echo 1
      fi
      ;;
    diff)
      if [ -f "$FIXTURES/delta-$4" ]; then
        cat "$FIXTURES/delta-$4"
      else
        cat "$FIXTURES/delta"
      fi
      ;;
    *) return 0 ;;
  esac
}
gh() {
  local url="" countfile count
  for arg in "$@"; do url="$arg"; done
  case "$url" in
    */actions/runs\?head_sha=*)
      sha="${url#*head_sha=}"
      sha="${sha%%&*}"
      countfile="$FIXTURES/count-runs-$sha"
      count=0
      [ -f "$countfile" ] && count="$(cat "$countfile")"
      count=$((count + 1))
      printf '%s' "$count" > "$countfile"
      if [ -f "$FIXTURES/runs-$sha-$count.json" ]; then
        cat "$FIXTURES/runs-$sha-$count.json"
      else
        cat "$FIXTURES/runs-$sha.json"
      fi
      ;;
    */actions/runs\?branch=testing*)
      cat "$FIXTURES/runs-branch.json"
      ;;
    */commits/*/check-runs*)
      sha="${url#*/commits/}"
      sha="${sha%%/*}"
      countfile="$FIXTURES/count-checks-$sha"
      count=0
      [ -f "$countfile" ] && count="$(cat "$countfile")"
      count=$((count + 1))
      printf '%s' "$count" > "$countfile"
      if [ -f "$FIXTURES/checks-$sha-$count.json" ]; then
        cat "$FIXTURES/checks-$sha-$count.json"
      else
        cat "$FIXTURES/checks-$sha.json"
      fi
      ;;
    *) return 1 ;;
  esac
}
sleep() { :; }
"""
    script = fake_commands + run
    head = "headsha"
    gate = [check_run(name) for name in GATE_CHECK_NAMES]
    pending = [check_run(name, status="in_progress", conclusion=None)
               for name in GATE_CHECK_NAMES]
    scenarios = (
        {
            "label": "canonical success",
            "runs": [run_record(head)],
            "branch_runs": [],
            "checks": {head: gate},
            "ancestors": [],
            "delta": [],
            "code": 0,
            "needle": "canonical testing gate run",
        },
        {
            "label": "dispatched testing gate",
            "runs": [run_record(head, event="workflow_dispatch")],
            "branch_runs": [],
            "checks": {head: gate},
            "ancestors": [],
            "delta": [],
            "code": 0,
            "needle": "canonical testing gate run",
        },
        {
            "label": "forged workflow",
            "runs": [run_record(head, suite=21, path=".github/workflows/evil.yml")],
            "branch_runs": [run_record(head, suite=21, path=".github/workflows/evil.yml")],
            "checks": {head: [check_run(name, suite=21) for name in GATE_CHECK_NAMES]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical testing gate run",
        },
        {
            "label": "pull-request event",
            "runs": [run_record(head, suite=31, event="pull_request")],
            "branch_runs": [run_record(head, suite=31, event="pull_request")],
            "checks": {head: [check_run(name, suite=31) for name in GATE_CHECK_NAMES]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical testing gate run",
        },
        {
            "label": "wrong branch",
            "runs": [run_record(head, branch="feature")],
            "branch_runs": [run_record(head, branch="feature")],
            "checks": {head: gate},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical testing gate run",
        },
        {
            "label": "other suite names",
            "runs": [run_record(head)],
            "branch_runs": [],
            "checks": {head: [
                check_run("unit", suite=1),
                check_run("browser", suite=1),
                *[check_run(name, suite=77) for name in GATE_CHECK_NAMES],
            ]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "succeeded but its check suite is incomplete",
        },
        {
            "label": "exact pending",
            "runs": [run_record(head, status="in_progress", conclusion=None)],
            "branch_runs": [],
            "checks": {head: pending},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "still pending",
        },
        {
            # The first poll sees the run in flight; the refresh after sleep
            # must observe the completed run and its successful suite.
            "label": "pending then success",
            "runs": [run_record(head, status="in_progress", conclusion=None)],
            "runs_pages": [
                [run_record(head, status="in_progress", conclusion=None)],
                [run_record(head)],
            ],
            "branch_runs": [],
            "checks": {head: pending},
            "checks_pages": {head: [pending, gate]},
            "ancestors": [],
            "delta": [],
            "code": 0,
            "needle": "succeeded for",
        },
        {
            "label": "exact incomplete",
            "runs": [run_record(head)],
            "branch_runs": [],
            "checks": {head: [
                check_run("unit"),
                check_run("container (linux/amd64)"),
            ]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "suite is incomplete",
        },
        {
            "label": "failed run",
            "runs": [run_record(head, conclusion="failure")],
            "branch_runs": [],
            "checks": {head: [
                check_run("unit", "failure"),
                check_run("browser"),
                check_run("container (linux/amd64)"),
                check_run("container (linux/arm64)"),
            ]},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "fallback docs delta",
            "runs": [],
            "branch_runs": [run_record("oldersha")],
            "checks": {"oldersha": gate},
            "ancestors": [("oldersha", head)],
            "delta": ["README.md", "docs/operations.md"],
            "code": 0,
            "needle": "only docs paths changed",
        },
        {
            # The older commit's run was created later (a re-run). Selection
            # must follow commit distance, not run creation time, so the
            # nearer failed commit still gates and the proof fails closed.
            "label": "re-run inversion",
            "runs": [],
            "branch_runs": [
                run_record("oldersha", created="2026-03-01T00:00:00Z", run_id=11),
                run_record("newersha", conclusion="failure",
                           created="2026-02-01T00:00:00Z", run_id=10),
            ],
            "checks": {"oldersha": gate, "newersha": gate},
            "ancestors": [("oldersha", head), ("newersha", head)],
            "distances": {"oldersha": 4, "newersha": 1},
            "delta": ["README.md"],
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "fallback code delta",
            "runs": [],
            "branch_runs": [run_record("oldersha")],
            "checks": {"oldersha": gate},
            "ancestors": [("oldersha", head)],
            "delta": ["internal/web/main.go"],
            "code": 1,
            "needle": "changes non-docs paths",
        },
        {
            "label": "fallback run failed",
            "runs": [],
            "branch_runs": [run_record("oldersha", conclusion="failure")],
            "checks": {"oldersha": gate},
            "ancestors": [("oldersha", head)],
            "delta": ["README.md"],
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "fallback non-ancestor",
            "runs": [],
            "branch_runs": [run_record("othersha")],
            "checks": {"othersha": gate},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical testing gate run on testing is a gated ancestor",
        },
        {
            "label": "fallback default branch",
            "runs": [],
            "branch_runs": [run_record("oldersha")],
            "checks": {"oldersha": [
                check_run("unit"),
                check_run("container (linux/amd64)"),
            ]},
            "ancestors": [("oldersha", head)],
            "delta": [],
            "code": 1,
            "needle": "did not pass (incomplete)",
        },
        {
            "label": "no run",
            "runs": [],
            "branch_runs": [],
            "checks": {},
            "ancestors": [],
            "delta": [],
            "code": 1,
            "needle": "No canonical testing gate run on testing is a gated ancestor",
        },
        {
            "label": "paginated fallback",
            "runs": [],
            "branch_runs": [
                [run_record(f"filler{i:03d}", suite=50 + i,
                            created=f"2025-12-{1 + i % 28:02d}T00:00:00Z", run_id=100 + i)
                 for i in range(100)],
                [run_record("oldersha", created="2026-01-01T00:00:00Z", run_id=2),
                 run_record("newersha", created="2026-02-01T00:00:00Z", run_id=3)],
            ],
            "checks": {"oldersha": gate, "newersha": gate},
            "ancestors": [("oldersha", head), ("newersha", head)],
            "distances": {"oldersha": 4, "newersha": 1},
            "delta": ["internal/web/main.go"],
            "delta_by_gated": {"newersha": ["README.md"]},
            "code": 0,
            "needle": "newersha",
        },
    )
    with tempfile.TemporaryDirectory(prefix="millivolt-full-gate-") as temp:
        root = Path(temp)
        for index, scenario in enumerate(scenarios):
            case = root / str(index)
            fixtures = case / "fixtures"
            fixtures.mkdir(parents=True)
            runs = scenario["runs"]
            shas = {head} | {record["head_sha"] for record in runs}
            shas |= set(scenario["checks"])
            runs_pages = scenario.get("runs_pages") or []
            checks_pages = scenario.get("checks_pages") or {}
            for sha in shas:
                if sha == head and runs_pages:
                    for page_index, page in enumerate(runs_pages, 1):
                        write_fixture(fixtures / f"runs-{sha}-{page_index}.json",
                                      [workflow_run_page(page)])
                pages = [workflow_run_page(runs)] if sha == head and runs else []
                write_fixture(fixtures / f"runs-{sha}.json",
                              pages or [workflow_run_page([])])
                if sha in checks_pages:
                    for page_index, page_checks in enumerate(checks_pages[sha], 1):
                        write_fixture(fixtures / f"checks-{sha}-{page_index}.json",
                                      [check_run_page(page_checks)])
                checks = scenario["checks"].get(sha)
                write_fixture(fixtures / f"checks-{sha}.json",
                              [check_run_page(checks)] if checks is not None
                              else [check_run_page([])])
            for sha, distance in (scenario.get("distances") or {}).items():
                (fixtures / f"distance-{sha}").write_text(str(distance), encoding="utf-8")
            branch_pages = scenario["branch_runs"]
            if not branch_pages:
                branch_pages = [workflow_run_page([])]
            elif branch_pages and isinstance(branch_pages[0], dict):
                branch_pages = [workflow_run_page(branch_pages)]
            else:
                branch_pages = [workflow_run_page(page) for page in branch_pages]
            write_fixture(fixtures / "runs-branch.json", branch_pages)
            (fixtures / "ancestors").write_text(
                "".join(f"{candidate} {descendant}\n"
                        for candidate, descendant in scenario["ancestors"]),
                encoding="utf-8")
            (fixtures / "delta").write_text(
                "".join(f"{path}\n" for path in scenario["delta"]), encoding="utf-8")
            for gated, paths in (scenario.get("delta_by_gated") or {}).items():
                (fixtures / f"delta-{gated}").write_text(
                    "".join(f"{path}\n" for path in paths), encoding="utf-8")
            env = {**os.environ, "FIXTURES": str(fixtures), "REPO": "owner/repo",
                   "HEAD_SHA": head, "DOCS_PATH_PATTERNS": "docs/** *.md"}
            try:
                result = subprocess.run(["bash", "-c", script], env=env, cwd=case,
                                        capture_output=True, text=True, timeout=20,
                                        check=False)
            except (OSError, subprocess.TimeoutExpired) as error:
                return [f"workflows self-check: full-gate {scenario['label']} proof: {error}"]
            if result.returncode != scenario["code"]:
                return [f"workflows self-check: full-gate {scenario['label']} must exit "
                        f"{scenario['code']}, got {result.returncode}: "
                        f"{(result.stderr or result.stdout).strip()}"]
            if scenario["needle"].lower() not in (result.stdout + result.stderr).lower():
                return [f"workflows self-check: full-gate {scenario['label']} must report "
                        f"{scenario['needle']!r}"]
    return []
