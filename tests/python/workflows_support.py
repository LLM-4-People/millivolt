"""Shared filesystem, YAML and shell-text helpers for the workflow policy suite."""

from __future__ import annotations

import json
import re
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[2]
WORKFLOWS = ROOT / ".github/workflows"


def load(path: Path) -> dict:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def trigger(workflow: dict):
    return workflow.get("on", workflow.get(True))


def steps(job: dict) -> list[dict]:
    value = job.get("steps", [])
    return value if isinstance(value, list) else []


def named_step(job: dict, name: str) -> int:
    for index, step in enumerate(steps(job)):
        if step.get("name") == name:
            return index
    return -1


def shell_function(run: str, name: str) -> str:
    """Return the body of one shell function declared at column zero."""
    match = re.search(rf"(?ms)^{re.escape(name)}\(\) \{{\n(.*?)^\}}", run)
    return match.group(1) if match else ""


def shell_declaration(run: str, name: str) -> str:
    """Return one complete shell function declaration, including braces."""
    body = shell_function(run, name)
    return f"{name}() {{\n{body}}}" if body else ""


def non_comment_lines(run: str) -> list[str]:
    """Return command lines only, so a commented-out command cannot satisfy a contract."""
    return [line for line in run.splitlines()
            if line.strip() and not line.lstrip().startswith("#")]


def missing_commands(run: str, commands: tuple[str, ...]) -> list[str]:
    """Return commands that are not present as their own non-comment line."""
    lines = non_comment_lines(run)
    return [
        command for command in commands
        if not any(re.fullmatch(r"\s*" + re.escape(command) + r"\s*", line) for line in lines)
    ]


def missing_needles(text: str, needles: tuple[str, ...]) -> list[str]:
    return [needle for needle in needles if needle not in text]


def logical_command_lines(text: str) -> list[str]:
    """Join shell continuation lines, ignoring full-line comments."""
    lines = []
    buffer = ""
    for line in non_comment_lines(text):
        stripped = line.rstrip()
        if stripped.endswith("\\"):
            buffer += stripped[:-1] + " "
            continue
        lines.append(buffer + stripped)
        buffer = ""
    if buffer:
        lines.append(buffer)
    return lines


def all_run_text(workflow: dict) -> str:
    """Join every step run value so commented commands cannot satisfy policy."""
    return "\n".join(
        str(step.get("run", ""))
        for job in (workflow.get("jobs") or {}).values()
        for step in steps(job)
        if step.get("run")
    )


def comment_commands(text: str, commands: tuple[str, ...]) -> str:
    """Comment out exact command lines so the non-comment checker must reject."""
    lines = []
    for line in text.splitlines():
        if any(line.strip() == command for command in commands):
            lines.append("# " + line)
        else:
            lines.append(line)
    return "\n".join(lines)


def workflow_run_page(runs: list[dict]) -> dict:
    return {"total_count": len(runs), "workflow_runs": runs}


def check_run_page(runs: list[dict]) -> dict:
    return {"total_count": len(runs), "check_runs": runs}


def write_fixture(path: Path, pages: list[dict]) -> None:
    path.write_text(json.dumps(pages, separators=(",", ":")), encoding="utf-8")
