#!/usr/bin/env python3
"""Regression checks for the repository's workflow and automation policy."""

from __future__ import annotations

import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github/workflows"
PIN = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$")


def load(path: Path) -> dict:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def trigger(workflow: dict):
    return workflow.get("on", workflow.get(True))


def steps(job: dict) -> list[dict]:
    value = job.get("steps", [])
    return value if isinstance(value, list) else []


def check_actions(workflow: dict, path: Path, errors: list[str]) -> None:
    for job_name, job in (workflow.get("jobs") or {}).items():
        for step in steps(job):
            uses = step.get("uses")
            if not uses:
                continue
            if not PIN.match(uses):
                errors.append(f"{path.name}: {job_name} action is not pinned to a full SHA: {uses}")
            if "@main" in uses or re.search(r"@v\d", uses):
                errors.append(f"{path.name}: {job_name} uses a mutable action reference: {uses}")


def named_step(job: dict, name: str) -> int:
    for index, step in enumerate(steps(job)):
        if step.get("name") == name:
            return index
    return -1


def check_workflows(errors: list[str]) -> None:
    required = {
        "check.yml",
        "docs.yml",
        "pages.yml",
        "dependency-review.yml",
        "security.yml",
    }
    present = {path.name for path in WORKFLOWS.glob("*.yml")}
    missing = required - present
    if missing:
        errors.append("missing workflows: " + ", ".join(sorted(missing)))

    for path in sorted(WORKFLOWS.glob("*.yml")):
        workflow = load(path)
        if not isinstance(workflow, dict):
            errors.append(f"{path.name}: workflow is not a mapping")
            continue
        if "pull_request_target" in path.read_text(encoding="utf-8"):
            errors.append(f"{path.name}: pull_request_target is forbidden")
        if workflow.get("permissions") != {"contents": "read"}:
            errors.append(f"{path.name}: top-level permissions must be contents: read")
        check_actions(workflow, path, errors)

    checks = load(WORKFLOWS / "check.yml")
    on = trigger(checks) or {}
    push = on.get("push") or {}
    if set(push.get("branches", [])) != {"main", "testing"}:
        errors.append("check.yml: push must be limited to main and testing")
    if push.get("tags") != ["v*"]:
        errors.append("check.yml: release tags must be limited to v*")
    ignored = set(push.get("paths-ignore", []))
    for path in ("docs/**", "site-src/**", "scripts/build_site.py", "requirements-docs.txt"):
        if path not in ignored:
            errors.append(f"check.yml: docs/site path is missing from paths-ignore: {path}")
    concurrency = checks.get("concurrency") or {}
    cancel = str(concurrency.get("cancel-in-progress", ""))
    if "pull_request" not in cancel or "refs/heads/testing" not in cancel:
        errors.append("check.yml: cancel-in-progress must be limited to PRs and testing")
    jobs = checks.get("jobs") or {}
    required_job = jobs.get("required") or {}
    if required_job.get("needs") != ["checks", "container"]:
        errors.append("check.yml: required gate must wait for checks and container")
    if required_job.get("if") != "always()":
        errors.append("check.yml: required gate must run with always()")

    check_job = jobs.get("checks") or {}
    identity = next((step for step in steps(check_job) if step.get("name") == "Validate clean source identity"), {})
    if "git rev-parse HEAD" not in str(identity.get("run", "")):
        errors.append("check.yml: tag identity must use the checked-out commit")

    required_steps = steps(required_job)
    version_step = next((step for step in required_steps
                         if step.get("name") == "Require an unreleased version for main promotions"), {})
    if not version_step:
        errors.append("check.yml: main promotions must require an unreleased version")
    elif "git fetch --tags" not in str(version_step.get("run", "")):
        errors.append("check.yml: version promotion check must fetch release tags")
    elif "-newer-than" not in str(version_step.get("run", "")):
        errors.append("check.yml: version promotion check must compare SemVer precedence")

    publish = jobs.get("publish") or {}
    image_index = named_step(publish, "Publish verified multi-platform image")
    push_index = named_step(publish, "Push verified tag")
    release_index = named_step(publish, "Create the GitHub release")
    if min(image_index, push_index, release_index) < 0:
        errors.append("check.yml: publish image, tag push and release steps must all exist")
    elif not (image_index < push_index < release_index):
        errors.append("check.yml: image must be published before the tag and release")
    image = steps(publish)[image_index] if image_index >= 0 else {}
    if image.get("with", {}).get("sbom") is not True:
        errors.append("check.yml: release image must include an SBOM")
    if image.get("with", {}).get("provenance") != "mode=max":
        errors.append("check.yml: release image must include max provenance")
    cut = next((step for step in steps(publish) if step.get("name") == "Cut the next version"), {})
    if "git push" in str(cut.get("run", "")):
        errors.append("check.yml: the cut step must not push before the image exists")
    if "git commit" in str(cut.get("run", "")):
        errors.append("check.yml: the release workflow must not create version commits")
    push = steps(publish)[push_index] if push_index >= 0 else {}
    push_run = str(push.get("run", ""))
    if "HEAD:main" in push_run:
        errors.append("check.yml: the release workflow must never push main")
    if '"v$RELEASE_VERSION"' not in push_run:
        errors.append("check.yml: the release workflow must push the verified tag")
    if "skip != 'true'" not in str(push.get("if", "")):
        errors.append("check.yml: the tag push must honor the no-release guard")
    publish_text = (WORKFLOWS / "check.yml").read_text(encoding="utf-8")
    if "HEAD:main" in publish_text:
        errors.append("check.yml: the release workflow must never push main")
    if "ghcr.io/${REPO}" in publish_text or "ghcr.io/LLM-4-People" in publish_text:
        errors.append("check.yml: release image references must use lowercase GHCR paths")
    if "value=main,enable=${{ github.ref == 'refs/heads/main' }}" in publish_text:
        errors.append("check.yml: main image must exclude prereleases")

    docs = load(WORKFLOWS / "docs.yml")
    docs_on = trigger(docs) or {}
    docs_push = docs_on.get("push") or {}
    if docs_push.get("tags-ignore") != ["**"]:
        errors.append("docs.yml: tag pushes must not run docs checks")
    if "pull_request" not in docs_on:
        errors.append("docs.yml: docs site changes need a pull request trigger")
    docs_text = (WORKFLOWS / "docs.yml").read_text(encoding="utf-8")
    if "tests.python.site" not in docs_text:
        errors.append("docs.yml: the generated site must be validated")

    pages = load(WORKFLOWS / "pages.yml")
    if (trigger(pages) or {}).get("push", {}).get("branches") != ["main"]:
        errors.append("pages.yml: Pages must deploy from main")
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
    if "tests.python.site" not in (WORKFLOWS / "pages.yml").read_text(encoding="utf-8"):
        errors.append("pages.yml: deployment must validate the site")

    dependency = load(WORKFLOWS / "dependency-review.yml")
    dependency_on = trigger(dependency) or {}
    if "pull_request" not in dependency_on or len(dependency_on) != 1:
        errors.append("dependency-review.yml: must run only on pull_request")

    security = load(WORKFLOWS / "security.yml")
    if "schedule" not in (trigger(security) or {}):
        errors.append("security.yml: vulnerability scan must run on a schedule")

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


def main() -> int:
    errors: list[str] = []
    try:
        check_workflows(errors)
    except (OSError, yaml.YAMLError) as error:
        errors.append(str(error))
    if errors:
        for error in errors:
            print("workflows: " + error)
        return 1
    print("workflows: clean")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
