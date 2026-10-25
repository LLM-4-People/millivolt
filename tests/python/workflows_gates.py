"""Required, escalation and full-gate proof predicates and wiring."""

from __future__ import annotations

import re

from .workflows_docs import (
    WORKFLOW_DOCS_ERROR,
    WORKFLOW_FAMILY_GLOB,
    WORKFLOW_FAMILY_YAML_GLOB,
    docs_classifier_errors,
    docs_only_fail_open_errors,
    docs_pattern_errors,
    shell_case_family,
)
from .workflows_escalation import escalation_proof_behavior_errors
from .workflows_gate_proof import (
    GATE_FUNCTIONS,
    full_gate_behavior_errors,
    suite_outcome_errors,
)
from .workflows_jobs import GATE_CHECK_NAMES
from .workflows_support import (
    ROOT,
    WORKFLOWS,
    missing_commands,
    named_step,
    non_comment_lines,
    shell_declaration,
    shell_function,
    steps,
    trigger,
)

REQUIRED_GATE_IF = (
    "always() && (contains(needs.*.result, 'failure') || "
    "contains(needs.*.result, 'cancelled') || "
    "((needs.unit.result == 'skipped' || needs.browser.result == 'skipped' || "
    "needs.container.result == 'skipped') && "
    "!(needs.unit.result == 'skipped' && needs.browser.result == 'skipped' && "
    "needs.container.result == 'skipped' && "
    "((github.event_name == 'push' && github.ref == 'refs/heads/main') || "
    "(github.event_name == 'pull_request' && github.base_ref == 'main' && "
    "(github.event.action == 'closed' && github.event.pull_request.merged == true || "
    "github.event.pull_request.user.login != 'dependabot[bot]'))))) || "
    "(needs.escalation-proof.result == 'skipped' && "
    "github.event_name == 'pull_request' && github.base_ref == 'main' && "
    "github.event.action != 'closed' && "
    "github.event.pull_request.user.login != 'dependabot[bot]'))"
)


FULL_GATE_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action != 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.user.login != 'dependabot[bot]' && "
    "steps.promotion.outputs.docs_only != 'true'"
)


DOCS_ONLY_VALIDATION_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action != 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.user.login != 'dependabot[bot]' && "
    "steps.promotion.outputs.docs_only == 'true'"
)


ESCALATION_PROOF_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action != 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.user.login != 'dependabot[bot]'"
)


def required_gate_policy_errors(gate_if: str, gate_run: str) -> list[str]:
    """Require the exact required-gate predicate and a hard failure command."""
    errors = []
    if gate_if != REQUIRED_GATE_IF:
        errors.append("check.yml: required gate condition must be exactly " + REQUIRED_GATE_IF)
    if gate_run.strip() != "exit 1":
        errors.append("check.yml: required gate must fail with exactly exit 1")
    return errors


# --- required-gate behavioural model -------------------------------------
#
# Exact-string equality pins the predicate, but it cannot show what the
# predicate *means* for each job result. This small evaluator reads the
# workflow's own expression and answers whether `Require every gate` would run
# `exit 1`. It models only the constructs the gate predicates use; an unknown
# operator, function or identifier raises, so a future edit has to extend the
# model instead of silently escaping the behavioural proof.
GITHUB_EXPRESSION_TOKEN = re.compile(
    r"\s*(?:(?P<string>'(?:[^']|'')*')|(?P<number>\d+)|"
    r"(?P<identifier>[A-Za-z_][A-Za-z0-9_.*-]*)|"
    r"(?P<operator>&&|\|\||==|!=|!|\(|\)|,))")


def github_expression_tokens(expression: str) -> list[tuple[str, str]]:
    tokens: list[tuple[str, str]] = []
    index = 0
    while index < len(expression):
        if expression[index].isspace():
            index += 1
            continue
        match = GITHUB_EXPRESSION_TOKEN.match(expression, index)
        if not match:
            raise ValueError("unsupported GitHub expression syntax: "
                             + repr(expression[index:]))
        kind = match.lastgroup or ""
        tokens.append((kind, match.group(kind)))
        index = match.end()
    return tokens


class GitHubExpression:
    """Evaluate the GitHub expression subset used by the gate predicates."""

    def __init__(self, expression: str, context: dict):
        self.tokens = github_expression_tokens(expression)
        self.position = 0
        self.context = context

    def parse(self):
        value = self.or_expression()
        if self.position != len(self.tokens):
            raise ValueError("unexpected GitHub expression tokens: "
                             + repr(self.tokens[self.position:]))
        return value

    def peek(self) -> tuple[str | None, str | None]:
        return self.tokens[self.position] if self.position < len(self.tokens) else (None, None)

    def accept(self, kind: str, value: str) -> bool:
        if self.peek() == (kind, value):
            self.position += 1
            return True
        return False

    def expect(self, kind: str, value: str) -> None:
        if not self.accept(kind, value):
            raise ValueError(f"expected {value!r}, got {self.peek()!r}")

    def or_expression(self):
        value = self.and_expression()
        while self.accept("operator", "||"):
            right = self.and_expression()
            value = value or right
        return value

    def and_expression(self):
        value = self.not_expression()
        while self.accept("operator", "&&"):
            right = self.not_expression()
            value = value and right
        return value

    def not_expression(self):
        if self.accept("operator", "!"):
            return not self.not_expression()
        return self.comparison()

    def comparison(self):
        left = self.primary()
        for operator in ("==", "!="):
            if self.accept("operator", operator):
                equal = left == self.primary()
                return equal if operator == "==" else not equal
        return left

    def primary(self):
        if self.accept("operator", "("):
            value = self.or_expression()
            self.expect("operator", ")")
            return value
        kind, value = self.peek()
        if kind == "string":
            self.position += 1
            return value[1:-1].replace("''", "'")
        if kind == "number":
            self.position += 1
            return int(value)
        if kind == "identifier":
            self.position += 1
            if self.accept("operator", "("):
                arguments = []
                if not self.accept("operator", ")"):
                    arguments.append(self.or_expression())
                    while self.accept("operator", ","):
                        arguments.append(self.or_expression())
                    self.expect("operator", ")")
                return self.call(value, arguments)
            if value == "true":
                return True
            if value == "false":
                return False
            if value in self.context:
                return self.context[value]
            raise ValueError("unsupported GitHub expression identifier: " + value)
        raise ValueError(f"unsupported GitHub expression token: {(kind, value)!r}")

    @staticmethod
    def call(name: str, arguments: list):
        if name == "always" and not arguments:
            return True
        if name == "contains" and len(arguments) == 2:
            container, needle = arguments
            if isinstance(container, list):
                return needle in container
            raise ValueError("contains() only models array membership")
        raise ValueError("unsupported GitHub expression function: " + name)


def gate_event(event_name: str, *, ref: str | None = None, base_ref: str = "",
               action: str = "", merged: bool = False, user: str = "writer") -> dict:
    default_ref = {
        "push": "refs/heads/testing",
        "workflow_dispatch": "refs/heads/testing",
    }.get(event_name, "refs/pull/7/merge")
    return {
        "event_name": event_name,
        "ref": ref or default_ref,
        "base_ref": base_ref,
        "event.action": action,
        "event.pull_request.merged": merged,
        "event.pull_request.user.login": user,
    }


def required_gate_failure(gate_if: str, needs: dict[str, str], event: dict) -> bool:
    """Return whether `Require every gate` would run its `exit 1`."""
    context = {f"needs.{name}.result": result for name, result in needs.items()}
    context["needs.*.result"] = list(needs.values())
    context.update({f"github.{key}": value for key, value in event.items()})
    return bool(GitHubExpression(gate_if, context).parse())


GATE_SUITE_SUCCESS = {
    "unit": "success",
    "browser": "success",
    "container": "success",
    "escalation-proof": "skipped",
}
ALL_JOBS_SUCCESS = {**GATE_SUITE_SUCCESS, "escalation-proof": "success"}
ALL_GATE_JOBS_SKIPPED = {
    "unit": "skipped",
    "browser": "skipped",
    "container": "skipped",
    "escalation-proof": "skipped",
}
TESTING_PUSH = gate_event("push")
MAIN_PUSH = gate_event("push", ref="refs/heads/main")
TESTING_DISPATCH = gate_event("workflow_dispatch")
TESTING_PR = gate_event("pull_request", base_ref="testing", action="opened", user="writer")
MAIN_PR = gate_event("pull_request", base_ref="main", action="opened", user="writer")
DEPENDABOT_MAIN_PR = gate_event("pull_request", base_ref="main", action="opened",
                                user="dependabot[bot]")
MERGED_MAIN_PR = gate_event("pull_request", base_ref="main", action="closed",
                            merged=True, user="writer")
CLOSED_TESTING_PR = gate_event("pull_request", base_ref="testing", action="closed",
                               merged=True, user="writer")
REQUIRED_GATE_SCENARIOS = (
    ("every gate job succeeds", TESTING_PUSH, GATE_SUITE_SUCCESS, False),
    ("a skipped unit leg fails", TESTING_PUSH, {**GATE_SUITE_SUCCESS, "unit": "skipped"}, True),
    ("a skipped browser leg fails", TESTING_PUSH,
     {**GATE_SUITE_SUCCESS, "browser": "skipped"}, True),
    ("a skipped container leg fails", TESTING_PUSH,
     {**GATE_SUITE_SUCCESS, "container": "skipped"}, True),
    ("all gate jobs skipped outside the reuse path fail", TESTING_PUSH,
     ALL_GATE_JOBS_SKIPPED, True),
    ("a failed leg fails", TESTING_PUSH, {**GATE_SUITE_SUCCESS, "browser": "failure"}, True),
    ("a cancelled leg fails", TESTING_PUSH, {**GATE_SUITE_SUCCESS, "container": "cancelled"}, True),
    ("a skipped escalation leg is allowed where escalation is not required",
     TESTING_PUSH, GATE_SUITE_SUCCESS, False),
    ("main push reuse may skip all gate jobs", MAIN_PUSH, ALL_GATE_JOBS_SKIPPED, False),
    ("main push may skip the escalation job", MAIN_PUSH, GATE_SUITE_SUCCESS, False),
    ("main push with one skipped gate leg fails", MAIN_PUSH,
     {**GATE_SUITE_SUCCESS, "browser": "skipped"}, True),
    ("a promotion reuses the gate and escalates", MAIN_PR,
     {**ALL_GATE_JOBS_SKIPPED, "escalation-proof": "success"}, False),
    ("a promotion with a skipped escalation proof fails", MAIN_PR,
     ALL_GATE_JOBS_SKIPPED, True),
    ("a promotion with a failed escalation proof fails", MAIN_PR,
     {**ALL_GATE_JOBS_SKIPPED, "escalation-proof": "failure"}, True),
    ("a promotion with a cancelled escalation proof fails", MAIN_PR,
     {**ALL_GATE_JOBS_SKIPPED, "escalation-proof": "cancelled"}, True),
    ("a promotion with every job green passes", MAIN_PR, ALL_JOBS_SUCCESS, False),
    ("a Dependabot promotion runs the gate without escalation", DEPENDABOT_MAIN_PR,
     GATE_SUITE_SUCCESS, False),
    ("a Dependabot promotion may not skip the gate", DEPENDABOT_MAIN_PR,
     ALL_GATE_JOBS_SKIPPED, True),
    ("a merged promotion may skip the gate and escalation", MERGED_MAIN_PR,
     ALL_GATE_JOBS_SKIPPED, False),
    ("a merged promotion with a partially skipped gate fails", MERGED_MAIN_PR,
     {**ALL_GATE_JOBS_SKIPPED, "unit": "success"}, True),
    ("a testing dispatch with a skipped leg fails", TESTING_DISPATCH,
     {**GATE_SUITE_SUCCESS, "unit": "skipped"}, True),
    ("a testing pull request with every gate job green passes", TESTING_PR,
     GATE_SUITE_SUCCESS, False),
    ("a closed testing pull request still fails closed", CLOSED_TESTING_PR,
     ALL_GATE_JOBS_SKIPPED, True),
    ("a merged promotion with a green escalation is still allowed", MERGED_MAIN_PR,
     {**ALL_GATE_JOBS_SKIPPED, "escalation-proof": "success"}, False),
)


def required_gate_behavior_errors(gate_if: str) -> list[str]:
    """Run the real required-gate expression over every job-result scenario."""
    errors = []
    for label, event, needs, expected in REQUIRED_GATE_SCENARIOS:
        try:
            actual = required_gate_failure(gate_if, needs, event)
        except ValueError as error:
            errors.append(f"workflows self-check: required gate scenario {label}: {error}")
            continue
        if actual is not expected:
            errors.append(f"workflows self-check: required gate must "
                          f"{'fail' if expected else 'pass'} when {label}, "
                          f"got {'fail' if actual else 'pass'}")
    return errors


def case_branch_exit_errors(run: str, labels: tuple[str, ...]) -> list[str]:
    """Require every matching case branch to end in an explicit exit 1."""
    errors = []
    for label in labels:
        branches = list(re.finditer(rf"(?ms)^\s*{re.escape(label)}\)\n(.*?)^\s*;;", run))
        if not branches:
            errors.append(f"check.yml: gate outcome has no {label}) branch")
            continue
        for branch in branches:
            if not re.search(r"(?m)^\s*exit 1\s*$", branch.group(1)):
                errors.append(f"check.yml: {label}) branch must exit 1")
    return errors


ESCALATION_PROOF_STEP = "Require the full-race escalation for the reviewed commit"


def escalation_proof_errors(job: dict) -> list[str]:
    """Pin the promotion escalation proof's job shape and selector."""
    errors = []
    if job.get("name") != "escalation-proof":
        errors.append("check.yml: escalation proof job must be named escalation-proof")
    job_if = " ".join(str(job.get("if", "")).split())
    if job_if != ESCALATION_PROOF_IF:
        errors.append("check.yml: escalation proof condition must be exactly "
                      + ESCALATION_PROOF_IF)
    if job.get("timeout-minutes") != 60:
        errors.append("check.yml: escalation proof must keep its sixty-minute budget")
    if job.get("runs-on") != "ubuntu-24.04":
        errors.append("check.yml: escalation proof must run on ubuntu-24.04")
    if any(str(step.get("uses", "")).startswith("actions/checkout@")
           for step in steps(job)):
        errors.append("check.yml: escalation proof must not check out candidate code")
    step = next((entry for entry in steps(job) if entry.get("name") == ESCALATION_PROOF_STEP), {})
    run = str(step.get("run", ""))
    if not step:
        errors.append("check.yml: escalation proof step is missing")
        return errors
    missing = missing_commands(run, ("set -euo pipefail",))
    if missing:
        errors.append("check.yml: escalation proof must keep these command lines: "
                      + ", ".join(missing))
    for needle in (
        "--paginate --slurp",
        'select(.path == ".github/workflows/escalation.yml")',
        'select(.event == "pull_request")',
        'select(.head_branch == "testing")',
        'select(.head_sha == $sha)',
        'select(.check_suite.id == $suite)',
        'select(.name == "race")',
        "if $success != 0",
        "for attempt in 1 2 3 4",
        "for attempt in $(seq 1 60)",
        "sleep 30",
        "--argjson suite",
        'echo "::error::No escalation.yml pull_request run on testing exists for $HEAD_SHA."',
        'echo "::error::The escalation run for $HEAD_SHA was still pending after the bounded wait."',
        'echo "::error::The escalation run for $HEAD_SHA completed as ${run_conclusion:-unknown}."',
        'echo "::error::The escalation run for $HEAD_SHA succeeded but its race job is $outcome."',
    ):
        if needle not in run:
            errors.append("check.yml: escalation proof is missing " + needle)
    if "gh workflow run" in run or "actions: write" in run:
        errors.append("check.yml: escalation proof must not dispatch or write workflow runs")
    if step.get("env") != {
        "GH_TOKEN": "${{ github.token }}",
        "REPO": "${{ github.repository }}",
        "HEAD_SHA": "${{ github.event.pull_request.head.sha }}",
    }:
        errors.append("check.yml: escalation proof must read the exact token, repository "
                      "and reviewed head env")
    return errors


def check_required_gate(checks: dict, errors: list[str]) -> None:
    required_job = (checks.get("jobs") or {}).get("required") or {}
    required_gate_index = named_step(required_job, "Require every gate")
    if required_gate_index != 0:
        errors.append("check.yml: required gate must be the first step")
    required_gate_step = steps(required_job)[required_gate_index] if required_gate_index >= 0 else {}
    gate_if = " ".join(str(required_gate_step.get("if", "")).split())
    gate_run = str(required_gate_step.get("run", ""))
    errors.extend(required_gate_policy_errors(gate_if, gate_run))
    if not required_gate_policy_errors(
            gate_if.replace("always() && ", "always() && false && "), gate_run):
        errors.append("workflows self-check: a false required-gate predicate must fail")
    if not required_gate_policy_errors(gate_if, "exit 0"):
        errors.append("workflows self-check: an exit 0 required gate must fail")
    if not required_gate_policy_errors(
            gate_if.replace("!= 'dependabot[bot]'", "== 'dependabot[bot]'"), gate_run):
        errors.append("workflows self-check: inverted Dependabot required-gate exception must fail")
    # Behavioural proof of the real predicate over the whole job-result matrix.
    # The scenarios include every skipped leg, the all-gates-skipped reuse path,
    # and the promotion escalation requirement, so a predicate edit that lets a
    # skipped or failed job through cannot hide behind exact-string equality.
    errors.extend(required_gate_behavior_errors(gate_if))
    for old, new, label in (
        ("(needs.escalation-proof.result == 'skipped' &&", "(false &&",
         "required escalation clause"),
        ("needs.escalation-proof.result == 'skipped'", "needs.escalation-proof.result != 'skipped'",
         "required escalation skip test"),
        ("needs.unit.result == 'skipped' || ", "false || ",
         "required unit skip test"),
        ("!(needs.unit.result == 'skipped' && ", "!(false && ",
         "required all-gates-skipped exception"),
        ("(github.event_name == 'push' && github.ref == 'refs/heads/main')", "false",
         "required main-push reuse"),
        ("contains(needs.*.result, 'cancelled')", "false",
         "required cancelled-result test"),
    ):
        mutated = gate_if.replace(old, new)
        if mutated == gate_if:
            errors.append(f"workflows self-check: cannot mutate the {label}")
        elif not required_gate_behavior_errors(mutated):
            errors.append(f"workflows self-check: weakening the {label} must fail the "
                          "required gate scenarios")


def check_escalation_proof_job(checks: dict, errors: list[str]) -> None:
    jobs = checks.get("jobs") or {}
    escalation_job = jobs.get("escalation-proof") or {}
    errors.extend(escalation_proof_errors(escalation_job))
    escalation_step = next((step for step in steps(escalation_job)
                            if step.get("name") == ESCALATION_PROOF_STEP), {})
    if escalation_step:
        escalation_run = str(escalation_step.get("run", ""))
        errors.extend(escalation_proof_behavior_errors(escalation_run))
        for old, new, label in (
            ('select(.path == ".github/workflows/escalation.yml")', "select(true)",
             "escalation workflow filter"),
            ('select(.event == "pull_request")', "select(true)", "escalation event filter"),
            ('select(.head_branch == "testing")', "select(true)", "escalation branch filter"),
            ('select(.head_sha == $sha)', "select(true)", "escalation commit filter"),
            ('select(.check_suite.id == $suite)', "select(true)", "escalation suite boundary"),
            ('select(.status == "completed" and .conclusion == "success")', "select(false)",
             "escalation race success test"),
            ("if $success != 0", "if true", "escalation race duplicate success test"),
        ):
            if old not in escalation_run:
                errors.append(f"workflows self-check: cannot mutate the {label}")
            elif not escalation_proof_behavior_errors(escalation_run.replace(old, new)):
                errors.append(f"workflows self-check: weakening the {label} must fail the "
                              "escalation proof scenarios")
        refresh = ('  sleep 30\n'
                   '  runs="$(fetch_workflow_runs "head_sha=$HEAD_SHA")"\n'
                   '  run="$(escalation_run "$runs" "$HEAD_SHA")"\n')
        if refresh not in escalation_run:
            errors.append("workflows self-check: cannot remove the escalation post-sleep refresh")
        elif not escalation_proof_behavior_errors(escalation_run.replace(refresh, "", 1)):
            errors.append("workflows self-check: dropping the escalation post-sleep refresh "
                          "must fail")


def check_gate_proof(checks: dict, errors: list[str]) -> None:
    push = (trigger(checks) or {}).get("push") or {}
    required_job = (checks.get("jobs") or {}).get("required") or {}
    required_steps = steps(required_job)
    docs_patterns = " ".join(str((required_job.get("env") or {}).get("DOCS_PATH_PATTERNS", "")).split())
    if not docs_patterns:
        errors.append("check.yml: required must own the docs/site path patterns")
    promotion_step = next((step for step in required_steps
                           if step.get("name") == "Require main promotions to come from testing"), {})
    promotion_run = str(promotion_step.get("run", ""))
    check_text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    docs_script = ROOT / "scripts/docs-paths.sh"
    if docs_script.exists():
        errors.append("scripts/docs-paths.sh: the candidate classifier must not exist; "
                      "both gate steps own an inline copy")
    if "source scripts/docs-paths.sh" in check_text:
        errors.append("check.yml: gate steps must not source the candidate classifier")
    if "docs_only" not in promotion_run or "git diff --name-only --no-renames" not in promotion_run:
        errors.append("check.yml: main promotions must classify docs-only changes with --no-renames")
    capture_diff = 'changed="$(git diff --name-only --no-renames "$base" "$HEAD_SHA")"'
    missing_promotion_lines = missing_commands(
        promotion_run, ("set -euo pipefail",
                        "git fetch --no-tags origin main testing", capture_diff))
    if missing_promotion_lines:
        errors.append("check.yml: docs-only classification must keep these command lines: "
                      + ", ".join(missing_promotion_lines))
    if 'done <<< "$changed"' not in promotion_run:
        errors.append("check.yml: docs-only classification must iterate the captured diff")
    if 'done < <(git diff --name-only --no-renames' in promotion_run:
        errors.append("check.yml: docs-only classification must not swallow git diff failures in a process substitution")
    commented_capture = "\n".join(
        "# " + line if capture_diff in line else line
        for line in promotion_run.splitlines())
    if not missing_commands(commented_capture, (capture_diff,)):
        errors.append("workflows self-check: a commented-out docs-only capture must fail")
    unset_pipefail = "\n".join(
        line for line in promotion_run.splitlines()
        if line.strip() != "set -euo pipefail")
    if not missing_commands(unset_pipefail, ("set -euo pipefail",)):
        errors.append("workflows self-check: a missing set -euo pipefail must fail")
    errors.extend(docs_only_fail_open_errors(promotion_run))
    forced_docs = promotion_run.replace("\ndocs_only=true\n", "\ndocs_only=false\n", 1)
    if forced_docs == promotion_run or not docs_only_fail_open_errors(forced_docs):
        errors.append("workflows self-check: forcing docs_only false must fail the docs-only proof")
    broken_classifier = promotion_run.replace("$pattern) return 0 ;;", "$pattern) return 1 ;;", 1)
    if broken_classifier == promotion_run or not docs_only_fail_open_errors(broken_classifier):
        errors.append("workflows self-check: rejecting every docs path must fail the docs-only proof")

    full_gate_step = next((step for step in required_steps
                           if step.get("name") == "Require the full testing gate for the promoted commit"), {})
    full_gate_run = str(full_gate_step.get("run", ""))
    full_gate_if = " ".join(str(full_gate_step.get("if", "")).split())
    for label, gate_run in (("promotion", promotion_run), ("full gate", full_gate_run)):
        if any(line.lstrip().startswith("source ") for line in non_comment_lines(gate_run)):
            errors.append(f"check.yml: {label} step must not source any candidate file")
    promotion_classifier = shell_declaration(promotion_run, "is_docs_path")
    full_classifier = shell_declaration(full_gate_run, "is_docs_path")
    if not promotion_classifier:
        errors.append("check.yml: promotion classification must define is_docs_path inline")
    if not full_classifier:
        errors.append("check.yml: full-gate proof must define is_docs_path inline")
    if promotion_classifier and full_classifier and promotion_classifier != full_classifier:
        errors.append("check.yml: both inline is_docs_path declarations must be byte-identical")
    if promotion_run.count("is_docs_path() {") != 1 or full_gate_run.count("is_docs_path() {") != 1:
        errors.append("check.yml: each gate step must define exactly one inline is_docs_path")
    errors.extend(docs_classifier_errors(promotion_run, docs_patterns))
    errors.extend(docs_classifier_errors(full_gate_run, docs_patterns))
    if not docs_classifier_errors(promotion_run,
                                  docs_patterns + " .github/workflows/*.yml"):
        errors.append("workflows self-check: widening the classifier to the workflow family "
                      "must classify check.yml as docs")
    if "git merge-base --is-ancestor origin/main" not in promotion_run:
        errors.append("check.yml: main must already be merged back into testing")
    if "git rev-parse origin/testing" not in promotion_run:
        errors.append("check.yml: main promotions must use the current testing tip")
    if "DOCS_PATH_PATTERNS" not in promotion_run:
        errors.append("check.yml: docs-only promotion must use the shared docs/site path patterns")
    docs_pattern_set = set(docs_patterns.split())
    expected_docs_patterns = shell_case_family(push.get("paths-ignore", []))
    errors.extend(docs_pattern_errors(docs_pattern_set, expected_docs_patterns))
    # Regression proof: a workflow-family glob would classify check.yml as
    # docs-only, so the check.yml guard must reject it with its exact error.
    # Asserting the message rather than mere non-emptiness keeps the proof from
    # passing on the exact-family mismatch alone. A silent pass here would let a
    # workflow-only change reuse a stale testing gate.
    workflow_glob_errors = docs_pattern_errors(
        expected_docs_patterns | {WORKFLOW_FAMILY_GLOB}, expected_docs_patterns)
    if WORKFLOW_DOCS_ERROR not in workflow_glob_errors:
        errors.append("workflows self-check: adding .github/workflows/*.yml must fail the check.yml guard")
    yaml_glob_errors = docs_pattern_errors(
        expected_docs_patterns | {WORKFLOW_FAMILY_YAML_GLOB}, expected_docs_patterns)
    if WORKFLOW_DOCS_ERROR not in yaml_glob_errors:
        errors.append("workflows self-check: adding .github/workflows/*.yaml must fail the check.yml guard")
    if not full_gate_step:
        errors.append("check.yml: main promotions must prove the full testing gate")
    else:
        full_commands = missing_commands(full_gate_run, ("set -euo pipefail",))
        if full_commands:
            errors.append("check.yml: full-gate proof must keep these command lines: "
                          + ", ".join(full_commands))
        if full_gate_if != FULL_GATE_IF:
            errors.append("check.yml: full-gate condition must be exactly " + FULL_GATE_IF)
        errors.extend(case_branch_exit_errors(full_gate_run, ("pending", "*")))
        for needle in (
            "--paginate --slurp",
            "actions/runs?$1&per_page=100",
            "commits/$1/check-runs?filter=all&per_page=100",
            "git rev-list --count",
            'select(.path == ".github/workflows/check.yml")',
            'select(.event == "push" or .event == "workflow_dispatch")',
            'select(.head_branch == "testing")',
            "select(.head_sha == $sha)",
            "select(.check_suite.id == $suite)",
            "sort_by(.created_at, .id)",
            "branch=testing",
            "git merge-base --is-ancestor",
            "git diff --name-only --no-renames",
            "sleep 10",
            "for attempt in 1 2 3 4 5 6",
            "--argjson suite",
            "is_docs_path",
        ):
            if needle not in full_gate_run:
                errors.append(f"check.yml: full-gate proof is missing {needle}")
        if "max-count" in full_gate_run or re.search(r"\brev-list\s+-\d+", full_gate_run):
            errors.append("check.yml: full-gate fallback must not bound the walk by commit count")
        if "actions/workflows/check.yml/runs" in full_gate_run or "status=success" in full_gate_run:
            errors.append("check.yml: the gate proof must select canonical runs by path, event, branch and sha")
        for name in GATE_CHECK_NAMES:
            if f'"{name}"' not in full_gate_run:
                errors.append(f"check.yml: full-gate proof must use the exact check-run name {name}")
        if 'startswith("container")' in full_gate_run:
            errors.append("check.yml: full-gate proof must match exact check-run names, not a container prefix")
        if full_gate_step.get("env") != {
            "GH_TOKEN": "${{ github.token }}",
            "REPO": "${{ github.repository }}",
            "HEAD_SHA": "${{ github.event.pull_request.head.sha }}",
        }:
            errors.append("check.yml: full-gate proof must read the exact token, repository "
                          "and reviewed head env")
        if "steps.promotion.outputs.docs_only != 'true'" not in full_gate_if:
            errors.append("check.yml: docs-only promotions must skip the full-gate proof")
        if "dependabot[bot]" not in full_gate_if:
            errors.append("check.yml: Dependabot must keep its full-gate path")
        for name in GATE_FUNCTIONS:
            if not shell_function(full_gate_run, name):
                errors.append(f"check.yml: full-gate proof is missing {name}")
        for needle in (
            'select(.status == "completed" and .conclusion == "success")',
            "| unique as $success",
            "($success | length) == 4",
            'select(.status != "completed")',
            'select(.conclusion == "failure" or .conclusion == "cancelled"',
        ):
            if needle not in shell_function(full_gate_run, "suite_outcome"):
                errors.append(f"check.yml: suite_outcome is missing {needle}")
        if 'echo "No canonical testing gate run exists for $HEAD_SHA; checking the closest gated testing ancestor."' not in full_gate_run:
            errors.append("check.yml: a missing exact canonical run must fall through to the closest gated ancestor")
        if 'echo "::error::The canonical testing gate run for $HEAD_SHA succeeded but its check suite is $outcome."' not in full_gate_run:
            errors.append("check.yml: a successful run with an incomplete suite must fail the proof")
        if 'echo "::error::No canonical testing gate run on testing is a gated ancestor of $HEAD_SHA."' not in full_gate_run:
            errors.append("check.yml: no gated ancestor must fail with its exact message")
        errors.extend(suite_outcome_errors(full_gate_run))
        errors.extend(full_gate_behavior_errors(full_gate_run))
        # Mutation proofs: every selector, the suite boundary, the fallback
        # docs delta, an early exit 0 and a `*)` branch exiting 0 must be
        # rejected by the behavioral harness.
        for old, new, label in (
            ('select(.path == ".github/workflows/check.yml")', "select(true)",
             "workflow path filter"),
            ('select(.event == "push" or .event == "workflow_dispatch")', "select(true)",
             "testing event filter"),
            ('select(.head_branch == "testing")', "select(true)", "testing branch filter"),
            ("select(.check_suite.id == $suite)", "select(true)", "check-suite boundary"),
            ('if ! is_docs_path "$path"; then', "if false; then", "fallback docs delta"),
        ):
            if old not in full_gate_run:
                errors.append(f"workflows self-check: cannot mutate the gate proof {label}")
            elif not full_gate_behavior_errors(full_gate_run.replace(old, new)):
                errors.append(f"workflows self-check: weakening the gate proof {label} must fail")
        early_exit = full_gate_run.replace(
            "set -euo pipefail\n", "set -euo pipefail\nexit 0\n", 1)
        if early_exit == full_gate_run:
            errors.append("workflows self-check: cannot add an early exit to the gate proof")
        elif not full_gate_behavior_errors(early_exit):
            errors.append("workflows self-check: an early exit 0 must fail the gate proof")
        # The pending-then-success scenario fails if the loop stops refreshing
        # the run list after sleep and hands the initial pending payload to
        # every later attempt.
        refresh_after_sleep = '    runs="$(fetch_workflow_runs "head_sha=$HEAD_SHA")"\n'
        if refresh_after_sleep not in full_gate_run:
            errors.append("workflows self-check: cannot remove the post-sleep run refresh")
        elif not full_gate_behavior_errors(full_gate_run.replace(refresh_after_sleep, "", 1)):
            errors.append("workflows self-check: dropping the post-sleep run refresh must fail")
        fallback_zero = re.sub(
            r"(?ms)(^\s*\*\)\n(?:.*?)^\s*)exit 1(\s*$)",
            r"\1exit 0\2", full_gate_run, count=1)
        if fallback_zero == full_gate_run:
            errors.append("workflows self-check: cannot mutate the fallback *) branch to exit 0")
        elif not full_gate_behavior_errors(fallback_zero):
            errors.append("workflows self-check: a fallback *) branch exiting 0 must fail")

    docs_validation = next((step for step in required_steps
                            if step.get("name") == "Validate the docs-only promotion"), {})
    docs_validation_if = " ".join(str(docs_validation.get("if", "")).split())
    docs_validation_run = str(docs_validation.get("run", ""))
    if not docs_validation:
        errors.append("check.yml: docs-only promotions must validate docs, workflow policy and the site")
    else:
        if docs_validation_if != DOCS_ONLY_VALIDATION_IF:
            errors.append("check.yml: docs-only validation condition must be exactly "
                          + DOCS_ONLY_VALIDATION_IF)
        if missing_commands(docs_validation_run, ("set -euo pipefail",)):
            errors.append("check.yml: docs-only validation must keep set -euo pipefail as a command line")
        docs_commands = (
            "python3 -m pip install -r requirements-docs.txt",
            "python3 -B -O -m tests.python.site",
            "python3 -B -O -m tests.python.repository",
            "python3 -B -O -m tests.python.workflows",
            "python3 -B -m tests.repository_check",
            "python3 -B -m tests.go test -mod=readonly ./internal/mcp -count=1",
        )
        missing_docs_commands = missing_commands(docs_validation_run, docs_commands)
        if missing_docs_commands:
            errors.append("check.yml: docs-only validation is missing command lines: "
                          + ", ".join(missing_docs_commands))
        commented = "\n".join(
            "# " + line if line.lstrip().startswith("python3") else line
            for line in docs_validation_run.splitlines())
        if not missing_commands(commented, ("python3 -B -O -m tests.python.site",)):
            errors.append("workflows self-check: a commented-out docs command must not satisfy the contract")
        if docs_validation.get("env", {}).get("GOTOOLCHAIN") != "local":
            errors.append("check.yml: docs-only validation must set GOTOOLCHAIN: local")
    setup_python = next((step for step in required_steps
                         if step.get("name") == "Set up Python for docs-only validation"), {})
    if not str(setup_python.get("uses", "")).startswith("actions/setup-python@"):
        errors.append("check.yml: docs-only validation must set up Python")
    elif "steps.promotion.outputs.docs_only == 'true'" not in str(setup_python.get("if", "")):
        errors.append("check.yml: Python setup must run only for docs-only promotions")
    setup_go = next((step for step in required_steps
                     if str(step.get("uses", "")).startswith("actions/setup-go@")), {})
    if not setup_go:
        errors.append("check.yml: required must set up Go for promotion validation")
    elif "docs_only != 'true'" in str(setup_go.get("if", "")):
        errors.append("check.yml: Go setup must also run for docs-only promotions")
    version_step = next((step for step in required_steps
                         if step.get("name") == "Require an unreleased version for main promotions"), {})
    if not version_step:
        errors.append("check.yml: main promotions must require an unreleased version")
    elif "git fetch --tags" not in str(version_step.get("run", "")):
        errors.append("check.yml: version promotion check must fetch release tags")
    elif "-newer-than" not in str(version_step.get("run", "")):
        errors.append("check.yml: version promotion check must compare SemVer precedence")
    elif "steps.promotion.outputs.docs_only != 'true'" not in " ".join(str(version_step.get("if", "")).split()):
        errors.append("check.yml: docs-only promotions must skip the version check")
