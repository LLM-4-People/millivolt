"""Workflow discovery, triggers and repository automation policy."""

from __future__ import annotations

import copy
import tempfile
from pathlib import Path

from .workflows_docs import (
    WORKFLOW_FAMILY_GLOB,
    WORKFLOW_FAMILY_YAML_GLOB,
    WORKFLOW_SUFFIXES,
    docs_coverage_errors,
    github_glob,
    github_glob_errors,
)
from .workflows_pins import (
    JOB_PERMISSIONS,
    check_actions,
    continue_on_error_errors,
    permission_errors,
)
from .workflows_support import (
    ROOT,
    WORKFLOWS,
    all_run_text,
    comment_commands,
    load,
    missing_commands,
    steps,
    trigger,
)


def workflow_paths(root: Path = WORKFLOWS) -> list[Path]:
    """Every workflow file the policy checks, for both accepted suffixes."""
    return sorted(path for suffix in WORKFLOW_SUFFIXES for path in root.glob("*" + suffix))


def check_workflow_files(errors: list[str]) -> None:
    required = {
        "check.yml",
        "docs.yml",
        "escalation.yml",
        "pages.yml",
        "dependency-review.yml",
        "security.yml",
    }
    paths = workflow_paths()
    present = {path.name for path in paths}
    missing = required - present
    if missing:
        errors.append("missing workflows: " + ", ".join(sorted(missing)))
    # A workflow may use either GitHub suffix. Prove discovery is not limited
    # to *.yml, so a new *.yaml workflow cannot skip pinning or permissions.
    with tempfile.TemporaryDirectory(prefix="millivolt-workflows-") as temp:
        probe = Path(temp)
        for name in ("one.yml", "two.yaml", "ignore.txt"):
            (probe / name).write_text("", encoding="utf-8")
        found = {path.name for path in workflow_paths(probe)}
        if found != {"one.yml", "two.yaml"}:
            errors.append("workflows self-check: .yaml workflow discovery is incomplete")
    yaml_workflow = {"jobs": {"extra": {"steps": [{"uses": "actions/checkout@v4"}]}}}
    yaml_errors: list[str] = []
    check_actions(yaml_workflow, Path("extra.yaml"), yaml_errors)
    if not yaml_errors:
        errors.append("workflows self-check: a mutable action in a .yaml workflow must fail pinning")
    if not permission_errors(Path("extra.yaml"), yaml_workflow, {}):
        errors.append("workflows self-check: an unknown .yaml workflow job must fail permissions")

    for path in paths:
        workflow = load(path)
        if not isinstance(workflow, dict):
            errors.append(f"{path.name}: workflow is not a mapping")
            continue
        if "pull_request_target" in path.read_text(encoding="utf-8"):
            errors.append(f"{path.name}: pull_request_target is forbidden")
        if workflow.get("permissions") != {"contents": "read"}:
            errors.append(f"{path.name}: top-level permissions must be contents: read")
        check_actions(workflow, path, errors)
        expected_permissions = JOB_PERMISSIONS.get(path.name, {})
        errors.extend(permission_errors(path, workflow, expected_permissions))
        errors.extend(continue_on_error_errors(workflow, path))
        if path.name in {"docs.yml", "pages.yml", "security.yml", "dependency-review.yml"}:
            for job_name, granted in expected_permissions.items():
                if path.name == "pages.yml" and job_name == "deploy":
                    continue
                if any(scope != "read" for scope in granted.values()):
                    errors.append(f"{path.name}: docs-family job {job_name} must be read-only")
        for label, mutated in (
            ("adding an unknown job", copy.deepcopy(workflow)),
            ("granting a docs job contents: write", copy.deepcopy(workflow)),
            ("granting the Pages build job contents: write", copy.deepcopy(workflow)),
            ("granting the Pages build job pages: write", copy.deepcopy(workflow)),
            ("setting continue-on-error on a job", copy.deepcopy(workflow)),
        ):
            jobs = mutated.setdefault("jobs", {})
            if label == "adding an unknown job":
                jobs["extra"] = {"runs-on": "ubuntu-24.04", "steps": []}
            elif label == "granting a docs job contents: write" and path.name == "docs.yml":
                jobs["markdown"]["permissions"] = {"contents": "write"}
            elif label == "granting the Pages build job contents: write" and path.name == "pages.yml":
                jobs["build"]["permissions"] = {"contents": "write"}
            elif label == "granting the Pages build job pages: write" and path.name == "pages.yml":
                jobs["build"]["permissions"] = {"contents": "read", "pages": "write"}
            elif label == "setting continue-on-error on a job":
                next(iter(jobs.values()))["continue-on-error"] = True
            else:
                continue
            if not permission_errors(path, mutated, expected_permissions) and \
                    not continue_on_error_errors(mutated, path):
                errors.append(f"workflows self-check: {label} in {path.name} must fail")
        if path.name == "docs.yml":
            mutated = copy.deepcopy(workflow)
            mutated["jobs"]["markdown"]["steps"][0]["continue-on-error"] = True
            if not continue_on_error_errors(mutated, path):
                errors.append("workflows self-check: a continue-on-error docs step must fail")
        if path.name == "pages.yml":
            mutated = copy.deepcopy(workflow)
            mutated["jobs"]["deploy"]["permissions"].pop("pages")
            if not permission_errors(path, mutated, expected_permissions):
                errors.append("workflows self-check: dropping Pages deploy write must fail")
            mutated = copy.deepcopy(workflow)
            mutated["jobs"]["build"]["permissions"].pop("pages")
            if not permission_errors(path, mutated, expected_permissions):
                errors.append("workflows self-check: dropping Pages build read must fail")


def check_triggers(checks: dict, errors: list[str]) -> None:
    on = trigger(checks) or {}
    push = on.get("push") or {}
    if set(push.get("branches", [])) != {"main", "testing"}:
        errors.append("check.yml: push must be limited to main and testing")
    if push.get("tags") != ["v*"]:
        errors.append("check.yml: release tags must be limited to v*")
    pull_request_trigger = on.get("pull_request")
    if not isinstance(pull_request_trigger, dict):
        errors.append("check.yml: pull_request must keep the merged-PR release trigger")
    else:
        expected_pr_types = ["opened", "synchronize", "reopened", "closed"]
        if pull_request_trigger.get("types") != expected_pr_types:
            errors.append("check.yml: pull_request types must be exactly " + str(expected_pr_types))
        if "closed" not in pull_request_trigger.get("types", []):
            errors.append("check.yml: pull_request must include the closed release event")
    if "workflow_dispatch" not in on:
        errors.append("check.yml: testing gate must have a workflow_dispatch recovery path")
    ignored = set(push.get("paths-ignore", []))
    for path in ("docs/**", "site-src/**", "scripts/build_site.py", "requirements-docs.txt"):
        if path not in ignored:
            errors.append(f"check.yml: docs/site path is missing from paths-ignore: {path}")
    concurrency = checks.get("concurrency") or {}
    cancel = str(concurrency.get("cancel-in-progress", ""))
    if "pull_request" not in cancel or "refs/heads/testing" not in cancel:
        errors.append("check.yml: cancel-in-progress must be limited to PRs and testing")
    if "github.event.action != 'closed'" not in cancel:
        errors.append("check.yml: a merged-PR release run must not be cancelled")


def check_docs_site(checks: dict, errors: list[str]) -> None:
    push = (trigger(checks) or {}).get("push") or {}
    docs = load(WORKFLOWS / "docs.yml")
    docs_on = trigger(docs) or {}
    docs_push = docs_on.get("push") or {}
    if docs_push.get("tags-ignore") != ["**"]:
        errors.append("docs.yml: tag pushes must not run docs checks")
    if "pull_request" not in docs_on:
        errors.append("docs.yml: docs site changes need a pull request trigger")
    docs_text = all_run_text(docs)
    docs_commands = (
        "python3 -m pip install -r requirements-docs.txt",
        "python3 -B -m tests.repository_check",
        "python3 -B -O -m tests.python.site",
        "python3 -B -O -m tests.python.repository",
        "python3 -B -O -m tests.python.workflows",
        "python3 -B -m tests.go test -mod=readonly ./internal/mcp -count=1",
    )
    missing_docs = missing_commands(docs_text, docs_commands)
    if missing_docs:
        errors.append("docs.yml: docs-only validation is missing command lines: "
                      + ", ".join(missing_docs))
    for command in docs_commands:
        if not missing_commands(comment_commands(docs_text, (command,)), (command,)):
            errors.append(f"workflows self-check: a commented-out docs command must fail: {command}")
    docs_paths = docs_push.get("paths") or []
    docs_pr_paths = (docs_on.get("pull_request") or {}).get("paths") or []
    if docs_paths != docs_pr_paths:
        errors.append("docs.yml: push and pull_request docs paths must match")
    for workflow_glob in (WORKFLOW_FAMILY_GLOB, WORKFLOW_FAMILY_YAML_GLOB):
        if workflow_glob not in docs_paths:
            errors.append(f"docs.yml: workflow changes must run the policy checks: {workflow_glob}")
    # Every push that check.yml ignores must still be covered by docs.yml.
    # Literal ignores are covered by the same docs path; glob ignores are
    # covered when every concrete depth sample matches a docs path. The
    # samples must match the ignored glob itself, so the docs coverage
    # assertion cannot pass through an unrelated pattern. Nested samples keep
    # a `docs/*` or `site-src/*` narrowing from passing on a shallow path.
    errors.extend(github_glob_errors())
    ignored_paths = push.get("paths-ignore", [])
    if not ignored_paths:
        errors.append("check.yml: push paths-ignore must not be empty")
    errors.extend(docs_coverage_errors(ignored_paths, docs_paths))
    # Mutation proofs: each narrowed glob must fail the depth coverage proof.
    for pattern, narrowed in (("docs/**", "docs/*"), ("docs/**", "docs/*/*"),
                              ("site-src/**", "site-src/*"),
                              ("site-src/**", "site-src/*/*"),
                              ("**/*.md", "*.md")):
        mutated = [narrowed if path == pattern else path for path in ignored_paths]
        if not docs_coverage_errors(mutated, docs_paths):
            errors.append(f"workflows self-check: narrowing {pattern} to {narrowed} must fail")

    pages = load(WORKFLOWS / "pages.yml")
    pages_on = trigger(pages) or {}
    pages_push = pages_on.get("push") or {}
    if pages_push.get("branches") != ["main"]:
        errors.append("pages.yml: Pages must deploy from main")
    pages_paths = pages_push.get("paths") or []
    for path in ("docs/**", "site-src/**", "scripts/build_site.py",
                 "requirements-docs.txt", "VERSION", "README.md", "CONTRIBUTING.md",
                 "SECURITY.md", "LICENSE", "THIRD_PARTY_NOTICES.md",
                 ".github/workflows/pages.yml"):
        if path not in pages_paths:
            errors.append(f"pages.yml: existing deployment path is missing: {path}")
    # build_site.py reads these directly; a change outside the paths filter
    # would leave the deployed site stale. Directory inputs use the same glob
    # helper as the docs coverage checks.
    direct_site_inputs = (
        "docs/README.md",
        "site-src/templates/base.html",
        "scripts/build_site.py",
        "requirements-docs.txt",
        "VERSION",
        "README.md",
        "CONTRIBUTING.md",
        "SECURITY.md",
        "LICENSE",
        "THIRD_PARTY_NOTICES.md",
        "internal/web/static/css/dashboard.css",
        "internal/web/static/favicon.svg",
        "internal/web/static/favicon.ico",
        "internal/web/static/apple-touch-icon.png",
        "tests/repository_check.py",
        "tests/support.py",
    )
    for path in direct_site_inputs:
        if not any(path == pattern or github_glob(pattern, path) for pattern in pages_paths):
            errors.append(f"pages.yml: build_site input is not covered by a deployment path: {path}")
    if not any(github_glob(path, "README.md") for path in pages_paths):
        errors.append("pages.yml: README.md must deploy the generated index")
    pages_jobs = pages.get("jobs") or {}
    if "build" not in pages_jobs or "deploy" not in pages_jobs:
        errors.append("pages.yml: build and deploy jobs are required")
    else:
        deploy = pages_jobs["deploy"]
        if deploy.get("environment", {}).get("name") != "github-pages":
            errors.append("pages.yml: deploy must use the github-pages environment")
        permissions = deploy.get("permissions") or {}
        if permissions.get("pages") != "write" or permissions.get("id-token") != "write":
            errors.append("pages.yml: deploy needs pages: write and id-token: write")
    pages_text = all_run_text(pages)
    pages_commands = (
        "python3 -m pip install -r requirements-docs.txt",
        "python3 -B -O -m tests.python.site",
        'python3 scripts/build_site.py --output site --base-url "$SITE_URL"',
    )
    missing_pages = missing_commands(pages_text, pages_commands)
    if missing_pages:
        errors.append("pages.yml: deployment is missing command lines: "
                      + ", ".join(missing_pages))
    for command in pages_commands:
        if not missing_commands(comment_commands(pages_text, (command,)), (command,)):
            errors.append(f"workflows self-check: a commented-out pages command must fail: {command}")


GOVULNCHECK_VERSION = "v1.1.4"
GOVULNCHECK_COMMAND = (
    f"go run golang.org/x/vuln/cmd/govulncheck@{GOVULNCHECK_VERSION} ./...")
DEPENDENCY_REVIEW_SETTINGS = {
    "fail-on-severity": "high",
    "fail-on-scopes": "runtime",
    "license-check": False,
}


def security_scan_errors(workflow: dict) -> list[str]:
    """Pin the vulnerability scanner version and its whole-module target."""
    errors = []
    scan = next((step for step in steps((workflow.get("jobs") or {}).get("govulncheck", {}))
                 if step.get("name") == "Scan the Go module graph"), {})
    command = " ".join(str(scan.get("run", "")).split())
    if command != GOVULNCHECK_COMMAND:
        errors.append("security.yml: govulncheck must run exactly " + GOVULNCHECK_COMMAND)
    return errors


def dependency_review_errors(workflow: dict) -> list[str]:
    """Pin the dependency review thresholds that fail a pull request."""
    errors = []
    review = next((step for step in steps(
        (workflow.get("jobs") or {}).get("dependency-review", {}))
        if step.get("name") == "Review dependency changes"), {})
    settings = review.get("with") or {}
    if settings != DEPENDENCY_REVIEW_SETTINGS:
        errors.append("dependency-review.yml: review settings must be exactly "
                      + str(DEPENDENCY_REVIEW_SETTINGS))
    return errors


def check_security_review(errors: list[str]) -> None:
    dependency = load(WORKFLOWS / "dependency-review.yml")
    dependency_on = trigger(dependency) or {}
    if "pull_request" not in dependency_on or len(dependency_on) != 1:
        errors.append("dependency-review.yml: must run only on pull_request")
    errors.extend(dependency_review_errors(dependency))
    def review_with(workflow: dict) -> dict:
        return next(step for step in steps(workflow["jobs"]["dependency-review"])
                    if step.get("name") == "Review dependency changes")["with"]

    for label, mutate in (
        ("lowering the dependency severity floor",
         lambda workflow: review_with(workflow).update({"fail-on-severity": "moderate"})),
        ("widening dependency scopes",
         lambda workflow: review_with(workflow).update({"fail-on-scopes": "all"})),
        ("enabling license checks",
         lambda workflow: review_with(workflow).update({"license-check": True})),
        ("dropping the scope setting",
         lambda workflow: review_with(workflow).pop("fail-on-scopes")),
    ):
        mutated = copy.deepcopy(dependency)
        mutate(mutated)
        if not dependency_review_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")

    security = load(WORKFLOWS / "security.yml")
    if "schedule" not in (trigger(security) or {}):
        errors.append("security.yml: vulnerability scan must run on a schedule")
    errors.extend(security_scan_errors(security))
    for label, command in (
        ("floating the govulncheck version",
         "go run golang.org/x/vuln/cmd/govulncheck@latest ./..."),
        ("narrowing the govulncheck target",
         f"go run golang.org/x/vuln/cmd/govulncheck@{GOVULNCHECK_VERSION} ./internal/..."),
        ("dropping the govulncheck module target",
         f"go run golang.org/x/vuln/cmd/govulncheck@{GOVULNCHECK_VERSION}"),
    ):
        mutated = copy.deepcopy(security)
        scan = next(step for step in steps(mutated["jobs"]["govulncheck"])
                    if step.get("name") == "Scan the Go module graph")
        scan["run"] = command
        if not security_scan_errors(mutated):
            errors.append(f"workflows self-check: {label} must fail")


def check_automation(errors: list[str]) -> None:
    dependabot = load(ROOT / ".github/dependabot.yml")
    updates = dependabot.get("updates") or []
    if len(updates) != 5:
        errors.append("dependabot.yml: expected five ecosystems")
    for update in updates:
        if update.get("target-branch") != "testing":
            errors.append(f"dependabot.yml: {update.get('package-ecosystem')} must target testing")
        if not update.get("groups"):
            errors.append(f"dependabot.yml: {update.get('package-ecosystem')} needs a group")
        if update.get("package-ecosystem") == "docker":
            ignored = {item.get("dependency-name") for item in update.get("ignore", [])}
            if "golang" not in ignored:
                errors.append("dependabot.yml: Docker Go updates must stay coordinated with go.mod")

    requirements = (ROOT / "requirements-docs.txt").read_text(encoding="utf-8")
    if "Markdown==" not in requirements:
        errors.append("requirements-docs.txt: Markdown must be pinned")
