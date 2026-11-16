#!/usr/bin/env python3
"""Regressions for the code-scanning release gate.

The gate is the only thing standing between an open finding and a published
release, so its failure modes matter as much as its happy path: an unreadable
API or an empty token must stop the release, not pass it.
"""

from __future__ import annotations

import io
import json
import os
import unittest
from contextlib import redirect_stderr, redirect_stdout
from unittest.mock import patch

import yaml

from scripts import security_gate
from tests.support import ROOT


def alert(number: int, rule: str = "go/example", path: str = "main.go", line: int = 1) -> dict:
    return {
        "number": number,
        "state": "open",
        "rule": {"id": rule, "severity": "warning", "security_severity_level": "medium"},
        "most_recent_instance": {"location": {"path": path, "start_line": line}},
    }


class PaginationTests(unittest.TestCase):
    def test_every_page_is_read(self):
        first = [alert(1)]
        second = [alert(2)]
        calls = []

        def fetch(url, token):
            calls.append(url)
            if len(calls) == 1:
                return first, '<https://api.test/page2>; rel="next"'
            return second, ""

        alerts = security_gate.open_alerts("o/r", "token", "https://api.test", fetch)
        self.assertEqual([1, 2], [entry["number"] for entry in alerts])
        self.assertEqual(["https://api.test/repos/o/r/code-scanning/alerts?state=open&per_page=100",
                          "https://api.test/page2"], calls)

    def test_next_link_ignores_other_relations(self):
        header = '<https://api.test/page2>; rel="next", <https://api.test/page1>; rel="prev"'
        self.assertEqual("https://api.test/page2", security_gate.next_link(header))
        self.assertEqual("", security_gate.next_link(""))

    def test_a_pagination_loop_is_an_error(self):
        def fetch(url, token):
            return [alert(1)], f'<{url}>; rel="next"'

        with self.assertRaises(security_gate.GateError):
            security_gate.open_alerts("o/r", "token", "https://api.test", fetch)

    def test_a_non_list_body_is_an_error(self):
        def fetch(url, token):
            return {"message": "forbidden"}, ""

        with self.assertRaises(security_gate.GateError):
            security_gate.open_alerts("o/r", "token", "https://api.test", fetch)


class GateTests(unittest.TestCase):
    def run_gate(self, alerts=None, error=None):
        def fake(repo, token, base):
            if error:
                raise error
            return alerts or []

        out, err = io.StringIO(), io.StringIO()
        with patch.dict(os.environ, {"GH_TOKEN": "token"}, clear=False), \
                patch.object(security_gate, "open_alerts", fake), \
                patch.object(security_gate, "api_base", lambda: "https://api.test"), \
                redirect_stdout(out), redirect_stderr(err):
            code = security_gate.main([])
        return code, out.getvalue() + err.getvalue()

    def test_open_alerts_fail_the_run_and_name_every_finding(self):
        code, output = self.run_gate([alert(21, "go/cookie", "cmd/proxy/operator.go", 258)])
        self.assertEqual(1, code)
        self.assertIn("#21", output)
        self.assertIn("go/cookie", output)
        self.assertIn("cmd/proxy/operator.go:258", output)

    def test_a_clean_repository_passes(self):
        code, output = self.run_gate([])
        self.assertEqual(0, code)
        self.assertIn("no open code-scanning alerts", output)

    def test_an_unreadable_api_fails_closed(self):
        code, output = self.run_gate(error=security_gate.GateError("HTTP 403"))
        self.assertEqual(1, code)
        self.assertIn("fails closed", output)

    def test_a_missing_token_is_not_clean(self):
        out, err = io.StringIO(), io.StringIO()
        with patch.dict(os.environ, {"GH_TOKEN": "", "GITHUB_TOKEN": ""}, clear=False), \
                redirect_stdout(out), redirect_stderr(err):
            code = security_gate.main([])
        self.assertEqual(2, code)
        self.assertIn("no GH_TOKEN", err.getvalue())

    def test_describe_survives_a_sparse_alert(self):
        text = security_gate.describe({"number": 3})
        self.assertIn("#3", text)
        self.assertIn("unknown rule", text)


class WorkflowWiringTests(unittest.TestCase):
    """Removing the gate from a workflow must fail here, not at release time."""

    def setUp(self):
        self.workflow = yaml.safe_load(
            (ROOT / ".github" / "workflows" / "check.yml").read_text(encoding="utf-8"))

    def job_commands(self, job: str) -> str:
        return "\n".join(step.get("run", "") for step in self.workflow["jobs"][job]["steps"])

    def test_the_gate_runs_where_a_release_can_start(self):
        self.assertIn("scripts/security_gate.py", self.job_commands("unit"))
        self.assertIn("scripts/security_gate.py", self.job_commands("publish"))

    def test_the_gate_only_reads_the_security_state(self):
        for job in ("unit", "publish"):
            self.assertEqual("read", self.workflow["jobs"][job]["permissions"]["security-events"])

    def test_the_publish_gate_is_skipped_only_when_no_release_is_due(self):
        gate = [step for step in self.workflow["jobs"]["publish"]["steps"]
                if "security_gate.py" in step.get("run", "")]
        self.assertEqual(1, len(gate))
        self.assertIn("steps.cut.outputs.skip != 'true'", gate[0].get("if", ""))


if __name__ == "__main__":
    unittest.main()
