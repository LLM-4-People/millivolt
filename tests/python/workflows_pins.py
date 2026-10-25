"""Action pinning and per-job permission invariants."""

from __future__ import annotations

import re
from pathlib import Path

from .workflows_support import steps

PIN = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+@[0-9a-f]{40}$")


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
            if uses.startswith("actions/checkout@"):
                options = step.get("with") or {}
                if options.get("persist-credentials") is not False:
                    errors.append(f"{path.name}: {job_name} checkout must set persist-credentials: false")


# Per-job permission allowlists. An unknown job or any write outside the exact
# expected map fails the policy: a new docs-family job may not quietly inherit
# a write scope, and only the Pages deploy job may publish.
JOB_PERMISSIONS = {
    "check.yml": {
        "unit": {},
        "browser": {},
        "container": {},
        "escalation-proof": {"actions": "read", "checks": "read"},
        "required": {"contents": "read", "checks": "read", "actions": "read"},
        "publish": {"contents": "write", "packages": "write"},
        "publish-testing": {"contents": "read", "packages": "write"},
    },
    "docs.yml": {"markdown": {}},
    "escalation.yml": {"race": {}},
    "pages.yml": {
        "build": {"contents": "read", "pages": "read"},
        "deploy": {"pages": "write", "id-token": "write"},
    },
    "security.yml": {"govulncheck": {}},
    "dependency-review.yml": {"dependency-review": {"contents": "read"}},
}


def permission_errors(path: Path, workflow: dict, expected: dict[str, dict]) -> list[str]:
    """Reject unknown jobs and any job-level permission outside the allowlist."""
    errors = []
    jobs = workflow.get("jobs") or {}
    actual = set(jobs)
    wanted = set(expected)
    for name in sorted(actual - wanted):
        errors.append(f"{path.name}: job {name} is not in the permission allowlist")
    for name in sorted(wanted - actual):
        errors.append(f"{path.name}: allowlisted job {name} is missing")
    for name in sorted(actual & wanted):
        granted = (jobs.get(name) or {}).get("permissions") or {}
        if granted != expected[name]:
            errors.append(f"{path.name}: job {name} permissions must be exactly "
                          f"{expected[name]}, got {granted}")
    return errors


def continue_on_error_errors(workflow: dict, path: Path) -> list[str]:
    """A gate job or step may never continue past its own failure."""
    errors = []
    for job_name, job in (workflow.get("jobs") or {}).items():
        if job.get("continue-on-error") not in (None, False):
            errors.append(f"{path.name}: job {job_name} must not continue on error")
        for step in steps(job):
            if step.get("continue-on-error") not in (None, False):
                errors.append(f"{path.name}: job {job_name} step "
                              f"{step.get('name') or step.get('uses')} must not continue on error")
    return errors
