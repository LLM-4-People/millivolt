#!/usr/bin/env python3
"""Behavioural regressions for the promotion ancestry guard.

The workflow rejects a testing tip whose history does not contain main. The
local guard must catch that before a full push gate and offer a merge that
actually fixes it. These tests drive the shipped scripts/promote.sh inside a
temporary repository with its own bare origin; no network and no real remote.
"""

from __future__ import annotations

import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from ..support import ROOT

PROMOTE = ROOT / "scripts" / "promote.sh"


def git(repo: Path, *args: str, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(
        ["git", *args], cwd=repo, capture_output=True, text=True, timeout=30, check=False)
    if check and result.returncode != 0:
        raise AssertionError(f"git {' '.join(args)} failed: {result.stderr or result.stdout}")
    return result


@unittest.skipUnless(shutil.which("git") and shutil.which("bash"),
                     "git and bash are required to drive the promotion owner")
class PromotionGuardTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="millivolt-promote-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.origin = self.root / "origin.git"
        self.repo = self.root / "work"
        git(self.root, "init", "--bare", "-q", str(self.origin))
        self.repo.mkdir()
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.name", "Promotion Fixture")
        git(self.repo, "config", "user.email", "promotion@example.invalid")
        (self.repo / "README.md").write_text("fixture\n", encoding="utf-8")
        scripts = self.repo / "scripts"
        scripts.mkdir()
        shutil.copy2(PROMOTE, scripts / "promote.sh")
        git(self.repo, "add", ".")
        git(self.repo, "commit", "-q", "-m", "fixture")
        git(self.repo, "branch", "-M", "main")
        git(self.repo, "remote", "add", "origin", str(self.origin))
        git(self.repo, "push", "-q", "-u", "origin", "main")
        git(self.repo, "switch", "-q", "-c", "testing")
        git(self.repo, "push", "-q", "-u", "origin", "testing")
        # main moves ahead of testing.
        git(self.repo, "switch", "-q", "main")
        (self.repo / "README.md").write_text("main moved\n", encoding="utf-8")
        git(self.repo, "commit", "-q", "-am", "main moves")
        git(self.repo, "push", "-q", "origin", "main")
        git(self.repo, "switch", "-q", "testing")

    def promote(self, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            ["bash", str(self.repo / "scripts" / "promote.sh"), *args],
            cwd=self.repo, capture_output=True, text=True, timeout=60, check=False)

    def test_check_fails_when_main_is_not_an_ancestor(self):
        result = self.promote("check")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("scripts/promote.sh sync", result.stderr)

    def test_sync_makes_main_an_ancestor_without_touching_main(self):
        before = git(self.repo, "rev-parse", "origin/main").stdout.strip()
        result = self.promote("sync")
        self.assertEqual(0, result.returncode, result.stderr)
        git(self.repo, "merge-base", "--is-ancestor", "origin/main", "HEAD")
        self.assertEqual(before, git(self.repo, "rev-parse", "origin/main").stdout.strip())
        self.assertEqual(0, self.promote("check").returncode)

    def test_sync_refuses_a_dirty_tree(self):
        (self.repo / "README.md").write_text("uncommitted\n", encoding="utf-8")
        result = self.promote("sync")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("commit or stash", result.stderr)
        # The refusal must not leave a merge in progress.
        self.assertFalse((self.repo / ".git" / "MERGE_HEAD").exists())

    def test_check_enforces_ancestry_on_a_work_branch(self):
        # A feature branch is pushed with HEAD:testing, so the branch name is
        # not the promotion target and must carry current main too.
        git(self.repo, "switch", "-q", "-c", "feature")
        result = self.promote("check")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("scripts/promote.sh sync", result.stderr)
        self.assertEqual(0, self.promote("sync").returncode)
        self.assertEqual(0, self.promote("check").returncode)

    def test_check_refuses_main(self):
        git(self.repo, "switch", "-q", "main")
        result = self.promote("check")
        self.assertNotEqual(0, result.returncode)
        self.assertIn("promote through a pull request", result.stderr)

    def test_push_never_targets_main(self):
        text = PROMOTE.read_text(encoding="utf-8")
        self.assertIn("git push origin HEAD:testing", text)
        self.assertNotIn("HEAD:main", text)


if __name__ == "__main__":
    unittest.main()
