"""Gate identity, job graph, conditions, timeouts and gate command wiring."""

from __future__ import annotations

import copy
import re

from .workflows_support import missing_commands, non_comment_lines, steps, trigger

# Every full-gate job, in required's `needs` order. `unit` and `browser` run
# the two halves of the old single `checks` job; both native container legs
# stay separate named checks.
GATE_JOB_NAMES = ("unit", "browser", "container")
CONTAINER_PLATFORMS = ("linux/amd64", "linux/arm64")
GATE_CHECK_NAMES = ("unit", "browser") + tuple(
    f"container ({platform})" for platform in CONTAINER_PLATFORMS)


REQUIRED_JOB_IF = (
    "always() && (github.event_name != 'workflow_dispatch' || "
    "github.ref == 'refs/heads/testing') && "
    "(github.event_name != 'pull_request' || github.event.action != 'closed' || "
    "(github.event.pull_request.merged == true && github.base_ref == 'main'))"
)


MERGED_VALIDATION_IF = (
    "success() && github.event_name == 'pull_request' && "
    "github.event.action == 'closed' && github.base_ref == 'main' && "
    "github.event.pull_request.merged == true"
)


CONTAINER_RUNNERS = {
    "linux/amd64": "ubuntu-24.04",
    "linux/arm64": "ubuntu-24.04-arm",
}


GATE_JOB_IF = (
    "(github.event_name == 'push' && "
    "(github.ref == 'refs/heads/testing' || github.ref_type == 'tag')) || "
    "(github.event_name == 'workflow_dispatch' && github.ref == 'refs/heads/testing') || "
    "(github.event_name == 'pull_request' && github.event.action != 'closed' && "
    "(github.base_ref != 'main' || "
    "github.event.pull_request.user.login == 'dependabot[bot]'))"
)


def container_wiring_errors(job: dict, check_step_name: str = "Check isolated container") -> list[str]:
    """Pin the native build wiring that proves each matrix leg runs natively."""
    errors = []
    if job.get("runs-on") != "${{ matrix.runner }}":
        errors.append("check.yml: container must run on ${{ matrix.runner }}")
    build = next((step for step in steps(job)
                  if step.get("name") == "Build native image without publishing"), {})
    options = build.get("with") or {}
    for key, value in (
        ("platforms", "${{ matrix.platform }}"),
        ("load", True),
        ("push", False),
        ("tags", "millivolt:check"),
        ("cache-from", "type=gha,scope=${{ matrix.cache_scope }}"),
        ("cache-to", "type=gha,mode=max,scope=${{ matrix.cache_scope }},ignore-error=true"),
    ):
        if options.get(key) != value:
            errors.append(f"check.yml: container build {key} must be exactly {value!r}")
    check = next((step for step in steps(job) if step.get("name") == check_step_name), {})
    if (check.get("env") or {}).get("IMAGE_ID") != "${{ steps.build.outputs.imageid }}":
        errors.append("check.yml: container check must consume steps.build.outputs.imageid")
    return errors


def container_matrix_errors(job: dict) -> list[str]:
    """Require one native build leg per supported platform.

    The release image is multi-platform, so the container job must prove both
    architectures natively. The exact job name is asserted separately; this
    helper owns the matrix contents.
    """
    matrix = (job.get("strategy") or {}).get("matrix") or {}
    include = matrix.get("include")
    if not isinstance(include, list):
        return ["check.yml: container matrix must declare an include list"]
    errors = []
    legs = [entry for entry in include if isinstance(entry, dict)]
    if len(legs) != len(include):
        errors.append("check.yml: every container matrix leg must be a mapping")
    platforms = [str(entry.get("platform") or "") for entry in legs]
    if sorted(platforms) != sorted(CONTAINER_PLATFORMS):
        errors.append("check.yml: container matrix must build exactly "
                      + ", ".join(CONTAINER_PLATFORMS))
    if len(set(platforms)) != len(platforms):
        errors.append("check.yml: container matrix platforms must be unique")
    for entry in legs:
        platform = entry.get("platform") or "unknown"
        for field in ("runner", "platform", "cache_scope"):
            if not entry.get(field):
                errors.append(f"check.yml: container matrix leg {platform} is missing {field}")
        expected_runner = CONTAINER_RUNNERS.get(platform)
        if expected_runner and entry.get("runner") != expected_runner:
            errors.append(
                f"check.yml: container matrix leg {platform} must run on {expected_runner}, "
                f"got {entry.get('runner')}")
    scopes = [str(entry.get("cache_scope") or "") for entry in legs]
    if len(set(scopes)) != len(scopes):
        errors.append("check.yml: container matrix cache scopes must be unique")
    return errors


def container_matrix_mutation(job: dict, platform: str, field: str | None = None) -> dict:
    """Copy the container job with one platform leg or field removed."""
    mutated = copy.deepcopy(job)
    matrix = (mutated.get("strategy") or {}).get("matrix") or {}
    include = matrix.get("include")
    if not isinstance(include, list):
        return mutated
    if field is None:
        matrix["include"] = [entry for entry in include
                             if not isinstance(entry, dict) or entry.get("platform") != platform]
    else:
        for entry in include:
            if isinstance(entry, dict) and entry.get("platform") == platform:
                entry.pop(field, None)
    return mutated


GATE_STEP_COMMANDS = (
    ("unit", "Check source and unit tests", ("bash scripts/check.sh",)),
    ("browser", "Check isolated browser fixtures", (
        'bash scripts/check.sh browser --target "http://${DEV_HOST}:${DEV_PORT}"',
        "MILLIVOLT_MCP_INTEGRATION=1 bash scripts/check.sh go test -count=1 ./internal/mcp/...",
    )),
)
CONTAINER_CHECK_COMMAND = 'bash scripts/check.sh container --image "$IMAGE_ID"'
BUILD_ACTION = re.compile(r"^docker/build-push-action@[0-9a-f]{40}$")


def gate_command_errors(workflow: dict) -> list[str]:
    """Pin the exact CI gate commands and reject any masking fallback.

    A renamed step, a dropped MCP command, an `|| true`, or an unpinned or
    differently identified image build would let the workflow run something
    other than the reviewed gate. The core suite and the browser fixtures are
    pinned in their own jobs, so a command moved between them fails the
    contract instead of silently changing what each job proves.
    """
    errors = []
    jobs = workflow.get("jobs") or {}
    for job_name, step_name, commands in GATE_STEP_COMMANDS:
        job = jobs.get(job_name, {})
        step = next((entry for entry in steps(job) if entry.get("name") == step_name), {})
        if not step:
            errors.append(f"check.yml: {job_name} must keep the {step_name} step")
            continue
        run = str(step.get("run", ""))
        missing = missing_commands(run, commands)
        if missing:
            errors.append(f"check.yml: {job_name} {step_name} is missing command lines: "
                          + ", ".join(missing))
        if step.get("continue-on-error") not in (None, False):
            errors.append(f"check.yml: {job_name} {step_name} must not continue on error")
        if any("||" in line for line in non_comment_lines(run)):
            errors.append(f"check.yml: {job_name} {step_name} must not mask a failure with ||")
    container = jobs.get("container", {})
    check = next((entry for entry in steps(container)
                  if entry.get("name") == "Check isolated container"), {})
    if missing_commands(str(check.get("run", "")), (CONTAINER_CHECK_COMMAND,)):
        errors.append("check.yml: container check must run exactly " + CONTAINER_CHECK_COMMAND)
    if check.get("continue-on-error") not in (None, False):
        errors.append("check.yml: container check must not continue on error")
    build = next((entry for entry in steps(container)
                  if entry.get("name") == "Build native image without publishing"), {})
    if build.get("id") != "build":
        errors.append("check.yml: native image build step must keep id: build")
    uses = str(build.get("uses", ""))
    if not BUILD_ACTION.fullmatch(uses):
        errors.append("check.yml: native image build must use a full-SHA pinned "
                      "docker/build-push-action, got " + uses)
    return errors


def check_job_graph(checks: dict, errors: list[str]) -> None:
    on = trigger(checks) or {}
    jobs = checks.get("jobs") or {}
    if set(jobs) != {
            "unit", "browser", "container", "escalation-proof",
            "required", "publish", "publish-testing"}:
        errors.append("check.yml: job graph must be exactly unit, browser, container, "
                      "escalation-proof, required, publish and publish-testing")
    required_job = jobs.get("required") or {}
    if required_job.get("name") != "required":
        errors.append("check.yml: branch-protection gate must be named required")
    if required_job.get("needs") != ["unit", "browser", "container", "escalation-proof"]:
        errors.append("check.yml: required gate must wait for unit, browser, container "
                      "and escalation-proof")
    required_if = " ".join(str(required_job.get("if", "")).split())
    if required_if != REQUIRED_JOB_IF:
        errors.append("check.yml: required gate condition must be exactly " + REQUIRED_JOB_IF)
    for label, mutated_if in (
        ("dropping the closed-event exception",
         required_if.replace("github.event.action != 'closed'", "false")),
        ("dropping the merged check",
         required_if.replace("github.event.pull_request.merged == true", "true")),
        ("broadening the closed-event exception to testing",
         required_if.replace("github.base_ref == 'main'", "true")),
        ("allowing a main dispatch",
         required_if.replace("github.ref == 'refs/heads/testing'", "true")),
    ):
        if mutated_if == required_if:
            errors.append(f"workflows self-check: cannot mutate {label}")
        elif mutated_if == REQUIRED_JOB_IF:
            errors.append(f"workflows self-check: {label} must not satisfy the exact condition")
    if required_job.get("timeout-minutes") != 10:
        errors.append("check.yml: required gate must keep its ten-minute budget")
    required_permissions = required_job.get("permissions") or {}
    if required_permissions != {"contents": "read", "checks": "read", "actions": "read"}:
        errors.append("check.yml: required gate permissions must be exactly contents: read, "
                      "checks: read and actions: read")

    # The three full-gate jobs run the same event set and must never inherit a
    # skip from another job. Only the deliberate main-promotion reuse path may
    # skip them, and required's predicate proves they are skipped together.
    for job_name in GATE_JOB_NAMES:
        job = jobs.get(job_name) or {}
        job_if = " ".join(str(job.get("if", "")).split())
        if job_if != GATE_JOB_IF:
            errors.append(f"check.yml: {job_name} condition must be exactly " + GATE_JOB_IF)
        for label, mutated_if in (
            ("inverting the Dependabot exception",
             job_if.replace("== 'dependabot[bot]'", "!= 'dependabot[bot]'")),
            ("dropping the tag event", job_if.replace("github.ref_type == 'tag'", "false")),
            ("dropping the testing branch",
             job_if.replace("github.ref == 'refs/heads/testing'", "false")),
            ("running the full gate on a closed PR",
             job_if.replace("github.event.action != 'closed' && ", "")),
            ("dropping the dispatched testing gate",
             job_if.replace("(github.event_name == 'workflow_dispatch' && "
                            "github.ref == 'refs/heads/testing') || ", "")),
        ):
            if mutated_if == job_if:
                errors.append(f"workflows self-check: cannot mutate {job_name} {label}")
            elif mutated_if == GATE_JOB_IF:
                errors.append(f"workflows self-check: {job_name} {label} "
                              "must not satisfy the exact condition")
        if "refs/heads/main" in job_if:
            errors.append(f"check.yml: {job_name} must not run for main pushes")
        if "needs" in job:
            errors.append(f"check.yml: {job_name} must start independently")
    if (jobs.get("container") or {}).get("name") != "container (${{ matrix.platform }})":
        errors.append("check.yml: container job name must be exactly container (${{ matrix.platform }})")

    # The browser job owns the Playwright download and starts no Node tooling;
    # the unit job owns Node and never downloads a browser. A swap would either
    # waste a download or hide a missing dependency behind a shared install.
    unit_steps = steps(jobs.get("unit") or {})
    browser_steps = steps(jobs.get("browser") or {})
    unit_text = "\n".join(str(step.get("run", "")) for step in unit_steps)
    browser_text = "\n".join(str(step.get("run", "")) for step in browser_steps)
    if "npm ci" not in unit_text:
        errors.append("check.yml: unit must install the Node development dependencies")
    if "playwright install" in unit_text:
        errors.append("check.yml: unit must not download a Playwright browser")
    if "playwright install --with-deps chromium" not in browser_text:
        errors.append("check.yml: browser must install Playwright Chromium")
    if "npm" in browser_text:
        errors.append("check.yml: browser must not run npm; its harnesses are Python and Go")
    if any(str(step.get("uses", "")).startswith("actions/setup-node@")
           for step in browser_steps):
        errors.append("check.yml: browser must not set up Node")
    if not any(str(step.get("uses", "")).startswith("actions/setup-node@")
               for step in unit_steps):
        errors.append("check.yml: unit must set up Node for npm test")
    if not any(str(step.get("uses", "")).startswith("actions/setup-python@")
               for step in unit_steps):
        errors.append("check.yml: unit must set up Python for the Python suites")
    if not any(str(step.get("uses", "")).startswith("actions/setup-python@")
               for step in browser_steps):
        errors.append("check.yml: browser must set up Python for Playwright")

    container_job = jobs.get("container") or {}
    errors.extend(container_matrix_errors(container_job))
    wrong_runner = copy.deepcopy(container_job)
    for entry in (wrong_runner.get("strategy") or {}).get("matrix", {}).get("include", []):
        if entry.get("platform") == "linux/arm64":
            entry["runner"] = "ubuntu-24.04"
    for label, mutated in (
        ("dropping the arm64 container leg",
         container_matrix_mutation(container_job, "linux/arm64")),
        ("dropping the arm64 container runner",
         container_matrix_mutation(container_job, "linux/arm64", "runner")),
        ("swapping the arm64 native runner", wrong_runner),
    ):
        if not container_matrix_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")
    duplicate_scope = copy.deepcopy(container_job)
    scopes = (duplicate_scope.get("strategy") or {}).get("matrix", {}).get("include", [])
    if len(scopes) == 2:
        scopes[1]["cache_scope"] = scopes[0]["cache_scope"]
    if not container_matrix_errors(duplicate_scope):
        errors.append("workflows self-check: duplicating a container cache scope must fail")
    if "needs" in container_job:
        errors.append("check.yml: container must start independently of the other gate jobs")
    errors.extend(container_wiring_errors(container_job))
    errors.extend(gate_command_errors(checks))
    def named_step_in(workflow: dict, job_name: str, step_name: str) -> dict:
        return next(step for step in steps(workflow["jobs"][job_name])
                    if step.get("name") == step_name)

    for label, mutate in (
        ("moving the core suite into the browser job",
         lambda workflow: named_step_in(workflow, "browser", "Check isolated browser fixtures")
         .update({"run": "bash scripts/check.sh"})),
        ("masking the core gate with || true",
         lambda workflow: named_step_in(workflow, "unit", "Check source and unit tests")
         .update({"run": "bash scripts/check.sh || true"})),
        ("letting the core gate continue on error",
         lambda workflow: named_step_in(workflow, "unit", "Check source and unit tests")
         .update({"continue-on-error": True})),
        ("dropping the live MCP command",
         lambda workflow: named_step_in(workflow, "browser", "Check isolated browser fixtures")
         .update({"run": 'bash scripts/check.sh browser --target "http://${DEV_HOST}:${DEV_PORT}"'})),
        ("renaming the image build id",
         lambda workflow: named_step_in(workflow, "container",
                                        "Build native image without publishing")
         .update({"id": "image"})),
        ("unpinning the image build action",
         lambda workflow: named_step_in(workflow, "container",
                                        "Build native image without publishing")
         .update({"uses": "docker/build-push-action@v7"})),
        ("changing the container image selector",
         lambda workflow: named_step_in(workflow, "container", "Check isolated container")
         .update({"run": 'bash scripts/check.sh container --image "$IMAGE"'})),
    ):
        mutated = copy.deepcopy(checks)
        mutate(mutated)
        if not gate_command_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")
    def build_step(job: dict) -> dict:
        return next(step for step in steps(job)
                    if step.get("name") == "Build native image without publishing")

    def check_step(job: dict) -> dict:
        return next(step for step in steps(job)
                    if step.get("name") == "Check isolated container")

    for label, mutate in (
        ("hard-coding the container platform",
         lambda job: build_step(job)["with"].update({"platforms": "linux/amd64"})),
        ("disabling the local image load",
         lambda job: build_step(job)["with"].update({"load": False})),
        ("pushing the check image",
         lambda job: build_step(job)["with"].update({"push": True})),
        ("reading the image id from the wrong output",
         lambda job: check_step(job)["env"].update(
             {"IMAGE_ID": "${{ steps.build.outputs.digest }}"})),
        ("switching the build cache to mode=min",
         lambda job: build_step(job)["with"].update(
             {"cache-to": "type=gha,mode=min,scope=${{ matrix.cache_scope }},ignore-error=true"})),
    ):
        mutated = copy.deepcopy(container_job)
        mutate(mutated)
        if not container_wiring_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")
    pull_request = on.get("pull_request")
    if isinstance(pull_request, dict) and ("paths" in pull_request or "paths-ignore" in pull_request):
        errors.append("check.yml: pull_request path filters would leave required pending")


def check_promotion(checks: dict, errors: list[str]) -> None:
    jobs = checks.get("jobs") or {}
    required_job = jobs.get("required") or {}
    identity = next((step for step in steps(jobs.get("unit") or {})
                     if step.get("name") == "Validate clean source identity"), {})
    if "git rev-parse HEAD" not in str(identity.get("run", "")):
        errors.append("check.yml: tag identity must use the checked-out commit")

    required_steps = steps(required_job)
    promotion_checkout = next((step for step in required_steps
                               if str(step.get("uses", "")).startswith("actions/checkout@")), {})
    checkout_options = promotion_checkout.get("with") or {}
    if checkout_options.get("fetch-depth") != 0:
        errors.append("check.yml: promotion checkout must fetch full history (fetch-depth: 0)")
    if checkout_options.get("persist-credentials") is not False:
        errors.append("check.yml: promotion checkout must set persist-credentials: false")

    merged_step = next((step for step in required_steps
                        if step.get("name") == "Validate merged promotion"), {})
    if not merged_step:
        errors.append("check.yml: merged promotion validation step is missing")
    else:
        merged_if = " ".join(str(merged_step.get("if", "")).split())
        if merged_if != MERGED_VALIDATION_IF:
            errors.append("check.yml: merged promotion validation condition must be exactly "
                          + MERGED_VALIDATION_IF)
        merged_run = str(merged_step.get("run", ""))
        for needle in (
            'set -euo pipefail',
            'gh api "repos/$REPO/git/commits/$MERGE_SHA"',
            '.parents[].sha',
            'compare/$MERGE_SHA...main',
            'check-runs?filter=all&per_page=100',
            '.name == "required" and .status == "completed"',
            'latest_required" != "success"',
            'pulls/$PR_NUMBER/files',
            '.previous_filename',
            'echo "docs_only=$docs_only" >> "$GITHUB_OUTPUT"',
        ):
            if needle not in merged_run:
                errors.append(f"check.yml: merged promotion validation is missing {needle}")
        for label, mutated_if in (
            ("dropping the merged flag", merged_if.replace(
                "github.event.pull_request.merged == true", "true")),
            ("broadening to closed testing PRs", merged_if.replace(
                "github.base_ref == 'main'", "true")),
        ):
            if mutated_if == merged_if:
                errors.append(f"workflows self-check: cannot mutate {label}")
            elif mutated_if == MERGED_VALIDATION_IF:
                errors.append(f"workflows self-check: {label} must not satisfy the exact condition")
    expected_outputs = {"merged_docs_only": "${{ steps.merged.outputs.docs_only }}"}
    if required_job.get("outputs") != expected_outputs:
        errors.append("check.yml: required must expose merged_docs_only from the merged validation")
    for step in required_steps:
        name = str(step.get("name", ""))
        step_if = " ".join(str(step.get("if", "")).split())
        if (name not in ("Require every gate", "Validate merged promotion") and
                "github.event_name == 'pull_request'" in step_if and
                "github.event.action != 'closed'" not in step_if):
            errors.append(f"check.yml: promotion step must not run on a merged PR: {name}")
