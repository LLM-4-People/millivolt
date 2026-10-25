"""Escalation run proof, escalation workflow policy and the flake sweep."""

from __future__ import annotations

import copy
import os
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

from .workflows_gate_proof import check_run, run_record
from .workflows_support import (
    ROOT,
    WORKFLOWS,
    check_run_page,
    comment_commands,
    load,
    missing_commands,
    steps,
    trigger,
    workflow_run_page,
    write_fixture,
)

ESCALATION_JOB_IF = (
    "github.event_name != 'pull_request' || "
    "(github.event.pull_request.user.login != 'dependabot[bot]' && "
    "github.event.pull_request.head.repo.full_name == github.repository)"
)


ESCALATION_COMMANDS = (
    "bash scripts/check.sh race",
    "bash scripts/check.sh flake",
)


def escalation_proof_behavior_errors(run: str) -> list[str]:
    """Exercise the real escalation proof with fake gh and sleep.

    The requested exact run must pass. A missing run, a pending run, a failed
    or cancelled run, a skipped or missing race job, a wrong commit, a wrong
    workflow, a wrong event, a wrong branch and a race job recorded in another
    suite must all fail closed. A pending run that completes while the proof
    polls must pass through the post-sleep refresh.
    """
    required = missing_commands(run, ("set -euo pipefail",))
    if required:
        return ["check.yml: escalation proof must keep these command lines: "
                + ", ".join(required)]
    if shutil.which("bash") is None or shutil.which("jq") is None:
        return ["workflows self-check: bash and jq are required to exercise the escalation proof"]
    fake_gh = r"""gh() {
  local url="" countfile count sha
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
    script = fake_gh + run
    head = "headsha"
    good = escalation_run_record(head)
    pending_run = escalation_run_record(head, status="in_progress", conclusion=None)
    race_pending = check_run("race", status="in_progress", conclusion=None)
    scenarios = (
        {
            "label": "successful exact run",
            "runs": [good],
            "checks": {head: [check_run("race")]},
            "code": 0,
            "needle": "escalation run and its race job succeeded",
        },
        {
            "label": "missing run",
            "runs": [],
            "checks": {},
            "code": 1,
            "needle": "No escalation.yml pull_request run on testing exists",
        },
        {
            "label": "pending run",
            "runs": [pending_run],
            "checks": {head: [race_pending]},
            "code": 1,
            "needle": "still pending after the bounded wait",
        },
        {
            "label": "failed run",
            "runs": [escalation_run_record(head, conclusion="failure")],
            "checks": {head: [check_run("race", "failure")]},
            "code": 1,
            "needle": "completed as failure",
        },
        {
            "label": "cancelled run",
            "runs": [escalation_run_record(head, conclusion="cancelled")],
            "checks": {head: [check_run("race", "cancelled")]},
            "code": 1,
            "needle": "completed as cancelled",
        },
        {
            "label": "wrong sha",
            "runs": [escalation_run_record("oldersha")],
            "checks": {head: [check_run("race")]},
            "code": 1,
            "needle": "No escalation.yml pull_request run on testing exists",
        },
        {
            "label": "wrong workflow",
            "runs": [escalation_run_record(head, path=".github/workflows/check.yml")],
            "checks": {head: [check_run("race")]},
            "code": 1,
            "needle": "No escalation.yml pull_request run on testing exists",
        },
        {
            "label": "wrong event",
            "runs": [escalation_run_record(head, event="workflow_dispatch")],
            "checks": {head: [check_run("race")]},
            "code": 1,
            "needle": "No escalation.yml pull_request run on testing exists",
        },
        {
            "label": "wrong branch",
            "runs": [escalation_run_record(head, branch="feature")],
            "checks": {head: [check_run("race")]},
            "code": 1,
            "needle": "No escalation.yml pull_request run on testing exists",
        },
        {
            "label": "skipped race job",
            "runs": [good],
            "checks": {head: [check_run("race", "skipped")]},
            "code": 1,
            "needle": "race job is incomplete",
        },
        {
            "label": "missing race job",
            "runs": [good],
            "checks": {head: []},
            "code": 1,
            "needle": "race job is incomplete",
        },
        {
            "label": "failed race job in a successful run",
            "runs": [good],
            "checks": {head: [check_run("race", "failure")]},
            "code": 1,
            "needle": "race job is failed",
        },
        {
            "label": "race job in another suite",
            "runs": [good],
            "checks": {head: [check_run("race", suite=77)]},
            "code": 1,
            "needle": "race job is incomplete",
        },
        {
            "label": "pending then success",
            "runs": [pending_run],
            "runs_pages": [[pending_run], [good]],
            "checks": {head: [race_pending]},
            "checks_pages": {head: [[race_pending], [check_run("race")]]},
            "code": 0,
            "needle": "escalation run and its race job succeeded",
        },
    )
    with tempfile.TemporaryDirectory(prefix="millivolt-escalation-") as temp:
        root = Path(temp)
        for index, scenario in enumerate(scenarios):
            case = root / str(index)
            fixtures = case / "fixtures"
            fixtures.mkdir(parents=True)
            shas = {head} | {record["head_sha"] for record in scenario["runs"]}
            runs_pages = scenario.get("runs_pages") or []
            checks_pages = scenario.get("checks_pages") or {}
            for sha in shas:
                if sha == head and runs_pages:
                    for page_index, page in enumerate(runs_pages, 1):
                        write_fixture(fixtures / f"runs-{sha}-{page_index}.json",
                                      [workflow_run_page(page)])
                pages = [workflow_run_page(scenario["runs"])] if sha == head else []
                write_fixture(fixtures / f"runs-{sha}.json", pages or [workflow_run_page([])])
                if sha in checks_pages:
                    for page_index, page_checks in enumerate(checks_pages[sha], 1):
                        write_fixture(fixtures / f"checks-{sha}-{page_index}.json",
                                      [check_run_page(page_checks)])
                checks = scenario["checks"].get(sha)
                write_fixture(fixtures / f"checks-{sha}.json",
                              [check_run_page(checks)] if checks is not None
                              else [check_run_page([])])
            env = {**os.environ, "FIXTURES": str(fixtures), "REPO": "owner/repo",
                   "HEAD_SHA": head}
            try:
                result = subprocess.run(["bash", "-c", script], env=env, cwd=case,
                                        capture_output=True, text=True, timeout=30,
                                        check=False)
            except (OSError, subprocess.TimeoutExpired) as error:
                return [f"workflows self-check: escalation {scenario['label']} proof: {error}"]
            if result.returncode != scenario["code"]:
                return [f"workflows self-check: escalation {scenario['label']} must exit "
                        f"{scenario['code']}, got {result.returncode}: "
                        f"{(result.stderr or result.stdout).strip()}"]
            if scenario["needle"].lower() not in (result.stdout + result.stderr).lower():
                return [f"workflows self-check: escalation {scenario['label']} must report "
                        f"{scenario['needle']!r}"]
    return []


def escalation_run_record(sha: str, *, suite: int = 1, status: str = "completed",
                          conclusion: str | None = "success",
                          path: str = ".github/workflows/escalation.yml",
                          event: str = "pull_request", branch: str = "testing",
                          created: str = "2026-01-01T00:00:00Z", run_id: int = 1) -> dict:
    return run_record(sha, suite=suite, status=status, conclusion=conclusion, path=path,
                      event=event, branch=branch, created=created, run_id=run_id)


def escalation_workflow_errors(workflow: dict) -> list[str]:
    """Pin the escalation workflow's triggers, single job and sweep commands."""
    errors = []
    on = trigger(workflow) or {}
    if set(on) != {"schedule", "workflow_dispatch", "pull_request"}:
        errors.append("escalation.yml: triggers must be exactly schedule, "
                      "workflow_dispatch and pull_request to main")
    schedule = on.get("schedule")
    if not isinstance(schedule, list) or len(schedule) != 1:
        errors.append("escalation.yml: nightly schedule must declare exactly one cron")
    elif not isinstance(schedule[0], dict) or not schedule[0].get("cron"):
        errors.append("escalation.yml: nightly schedule must declare a cron expression")
    pull_request = on.get("pull_request")
    if not isinstance(pull_request, dict) or pull_request.get("branches") != ["main"]:
        errors.append("escalation.yml: pull_request must target main only")
    elif pull_request.get("types") != ["opened", "synchronize", "reopened"]:
        errors.append("escalation.yml: pull_request types must be exactly "
                      "opened, synchronize and reopened")
    jobs = workflow.get("jobs") or {}
    if set(jobs) != {"race"}:
        errors.append("escalation.yml: the only job must be race; it must not publish or release")
    race = jobs.get("race") or {}
    if race.get("name") != "race":
        errors.append("escalation.yml: escalation job must be named race")
    race_if = " ".join(str(race.get("if", "")).split())
    if race_if != ESCALATION_JOB_IF:
        errors.append("escalation.yml: race condition must be exactly " + ESCALATION_JOB_IF)
    if race.get("timeout-minutes") != 90:
        errors.append("escalation.yml: race must keep its ninety-minute budget")
    if race.get("runs-on") != "ubuntu-24.04":
        errors.append("escalation.yml: race must run on ubuntu-24.04")
    run_text = "\n".join(str(step.get("run", "")) for step in steps(race))
    missing = missing_commands(run_text, ESCALATION_COMMANDS)
    if missing:
        errors.append("escalation.yml: race is missing command lines: " + ", ".join(missing))
    for command in ESCALATION_COMMANDS:
        commented = comment_commands(run_text, (command,))
        if not missing_commands(commented, (command,)):
            errors.append(f"workflows self-check: a commented-out escalation command "
                          f"must fail: {command}")
    for forbidden in ("docker/build-push-action", "docker/login-action", "gh release",
                      "git push", "npm ", "playwright", "check.sh browser",
                      "gh workflow run"):
        if forbidden in run_text:
            errors.append("escalation.yml: escalation must not run " + forbidden)
    checkout = next((step for step in steps(race)
                     if str(step.get("uses", "")).startswith("actions/checkout@")), {})
    options = checkout.get("with") or {}
    if options.get("ref") != "${{ github.event.pull_request.head.sha || github.sha }}":
        errors.append("escalation.yml: checkout must pin the reviewed pull-request head")
    if options.get("persist-credentials") is not False:
        errors.append("escalation.yml: checkout must set persist-credentials: false")
    verify = next((step for step in steps(race)
                   if step.get("name") == "Verify the checked-out revision"), {})
    verify_run = str(verify.get("run", ""))
    if "git rev-parse HEAD" not in verify_run or '!= "$EXPECTED"' not in verify_run:
        errors.append("escalation.yml: race must verify the checked-out revision")
    if verify.get("env", {}).get("EXPECTED") != (
            "${{ github.event.pull_request.head.sha || github.sha }}"):
        errors.append("escalation.yml: revision check must compare the reviewed head")
    if any(str(step.get("uses", "")).startswith("actions/setup-node@")
           for step in steps(race)):
        errors.append("escalation.yml: race must not set up Node")
    return errors


def flake_sweep_errors(script: str) -> list[str]:
    """Pin the escalation flake entry point and prove every named test exists.

    The sweep list is the single owner of the repeated clock-sensitive set.
    Exact test names (not wildcards) mean a rename cannot silently shrink the
    sweep: every declared name must still be a `func` in its package's test
    sources, and every command line is pinned.
    """
    errors = []
    if not re.search(r"(?m)^  flake\)$", script):
        errors.append("scripts/check.sh: escalation flake entry point is missing")
        return errors
    if not re.search(r"(?m)^usage=.*\bflake\b", script):
        errors.append("scripts/check.sh: usage must document the flake entry point")
    for package, directory in (("proxy", "internal/proxy"),
                               ("scheduler", "internal/scheduler")):
        match = re.search(rf"(?m)^\s*flake_{package}='([^']*)'$", script)
        if not match:
            errors.append(f"scripts/check.sh: flake sweep for {directory} is missing")
            continue
        names = re.findall(r"\bTest[A-Za-z0-9_]+\b", match.group(1))
        if not names:
            errors.append(f"scripts/check.sh: flake sweep for {directory} is empty")
            continue
        if len(set(names)) != len(names):
            errors.append(f"scripts/check.sh: flake sweep for {directory} repeats a test")
        sources = "\n".join(path.read_text(encoding="utf-8")
                            for path in sorted((ROOT / "tests/_go" / directory).glob("*.go")))
        for name in names:
            if not re.search(rf"(?m)^func {re.escape(name)}\(", sources):
                errors.append(f"scripts/check.sh: flake sweep names a missing test: "
                              f"{directory} {name}")
        pattern = match.group(1)
        if not (pattern.startswith("^(") and pattern.endswith(")$")):
            errors.append(f"scripts/check.sh: flake sweep for {directory} must use "
                          "exact anchored test names")
    for command in (
        'python3 -B -m tests.go test -race -count=5 -run "$flake_proxy" ./internal/proxy',
        'python3 -B -m tests.go test -race -count=5 -run "$flake_scheduler" ./internal/scheduler',
    ):
        if missing_commands(script, (command,)):
            errors.append("scripts/check.sh: flake sweep must keep the command line " + command)
    return errors


def check_escalation_suite(errors: list[str]) -> None:
    escalation = load(WORKFLOWS / "escalation.yml")
    errors.extend(escalation_workflow_errors(escalation))
    for label, mutate in (
        ("dropping the nightly schedule",
         lambda workflow: trigger(workflow).pop("schedule")),
        ("targeting testing instead of main",
         lambda workflow: trigger(workflow)["pull_request"].update({"branches": ["testing"]})),
        ("adding a push trigger",
         lambda workflow: trigger(workflow).update({"push": {"branches": ["testing"]}})),
        ("adding a publish job",
         lambda workflow: workflow["jobs"].update(
             {"publish": {"runs-on": "ubuntu-24.04", "steps": []}})),
        ("renaming the race job",
         lambda workflow: workflow["jobs"]["race"].update({"name": "escalate"})),
        ("dropping the pull-request skip",
         lambda workflow: workflow["jobs"]["race"].pop("if")),
        ("dropping the full-race command",
         lambda workflow: next(step for step in steps(workflow["jobs"]["race"])
                               if step.get("name") == "Run the forced full-race sweep")
         .update({"run": "true"})),
        ("dropping the flake command",
         lambda workflow: next(step for step in steps(workflow["jobs"]["race"])
                               if step.get("name") == "Run the clock-sensitive flake sweep")
         .update({"run": "true"})),
        ("dispatching itself",
         lambda workflow: next(step for step in steps(workflow["jobs"]["race"])
                               if step.get("name") == "Run the clock-sensitive flake sweep")
         .update({"run": "gh workflow run escalation.yml\nbash scripts/check.sh flake"})),
    ):
        mutated = copy.deepcopy(escalation)
        mutate(mutated)
        if not escalation_workflow_errors(mutated):
            errors.append(f"workflows self-check: {label} in escalation.yml must fail")
    errors.extend(flake_sweep_errors((ROOT / "scripts/check.sh").read_text(encoding="utf-8")))
