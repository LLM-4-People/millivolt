#!/usr/bin/env python3
"""Regression checks for the repository's workflow and automation policy."""

from __future__ import annotations

import yaml

from .workflows_escalation import check_escalation_suite
from .workflows_gates import (
    check_escalation_proof_job,
    check_gate_proof,
    check_required_gate,
)
from .workflows_jobs import check_job_graph, check_promotion
from .workflows_policy import (
    check_automation,
    check_docs_site,
    check_security_review,
    check_triggers,
    check_workflow_files,
)
from .workflows_release import check_release
from .workflows_support import WORKFLOWS, load


def check_workflows(errors: list[str]) -> None:
    check_workflow_files(errors)
    checks = load(WORKFLOWS / "check.yml")
    check_triggers(checks, errors)
    check_job_graph(checks, errors)
    check_required_gate(checks, errors)
    check_escalation_proof_job(checks, errors)
    check_promotion(checks, errors)
    check_gate_proof(checks, errors)
    check_release(checks, errors)
    check_docs_site(checks, errors)
    check_escalation_suite(errors)
    check_security_review(errors)
    check_automation(errors)


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
