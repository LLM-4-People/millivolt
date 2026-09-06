#!/usr/bin/env python3
"""Deterministic offline regressions for the small publication checker."""

import os
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

from .. import repository_check as check


class RepositoryChecks(unittest.TestCase):
    def test_test_source_layout(self):
        for name in ("internal/unit_test.go", "scripts/browser_check.py", "ui_check.js", "other.spec.js"):
            with self.subTest(name=name):
                self.assertEqual(check.test_layout_errors([name]),
                                 ["test source must live under tests/: " + name])
        self.assertEqual(check.test_layout_errors([
            "tests/_go/internal/unit.go", "tests/python/go.py", "tests/browser_check.py", "tests/ui_check.js",
            "scripts/check.sh", "scripts/backup_db.py", "scripts/licenses/main.go", "cmd/stress/main.go",
        ]), [])
        for name in ("tests/_go/internal/unit_test.go", "tests/python/go_test.py", "tests/unit_test.js"):
            with self.subTest(name=name):
                self.assertEqual(check.test_layout_errors([name]),
                                 ["physical test filename must omit _test suffix: " + name])

    def test_markdown_targets_and_scope(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".gitignore").write_text("node_modules/\n", encoding="utf-8")
            (root / "docs").mkdir()
            (root / "node_modules").mkdir()
            (root / "docs" / "existing file.md").write_text("# Existing\n", encoding="utf-8")
            (root / "node_modules" / "README.md").write_text("[ignored](absent)", encoding="utf-8")
            fence = chr(96) * 3
            (root / "README.md").write_text(
                "[good](docs/existing%20file.md#heading)\n"
                "[external](https://example.invalid/no-network)\n"
                "[mail](mailto:operator@example.invalid)\n"
                "[local anchor](#heading)\n"
                + fence + "\n[example](absent-example)\n" + fence + "\n"
                "[reference]: <docs/existing file.md>\n", encoding="utf-8",
            )
            self.assertEqual(check.markdown_errors(root), [])
            (root / "docs" / "bad.md").write_text("[broken](missing.md)\n", encoding="utf-8")
            self.assertEqual(check.markdown_errors(root),
                             ["docs/bad.md:1: missing local link missing.md"])

    def test_markdown_escape_is_not_portable(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp) / "repo"
            root.mkdir()
            (root / ".gitignore").write_text("", encoding="utf-8")
            (Path(temp) / "outside.md").write_text("outside", encoding="utf-8")
            (root / "README.md").write_text("[outside](../outside.md)", encoding="utf-8")
            self.assertEqual(len(check.markdown_errors(root)), 1)

    def test_private_markdown_is_skipped_and_cannot_satisfy_public_links(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            (root / ".gitignore").write_text("private.md\n", encoding="utf-8")
            (root / "private.md").write_text("[not public](missing.md)\n" + chr(0x2014), encoding="utf-8")
            (root / "README.md").write_text("[public link](private.md)\n", encoding="utf-8")
            self.assertEqual(check.markdown_errors(root),
                             ["README.md:1: missing local link private.md"])
            (root / "README.md").write_text("Public text\n", encoding="utf-8")
            self.assertEqual(check.markdown_errors(root), [])
            _, public = check.source_inventory(root)
            self.assertNotIn("private.md", public)
            self.assertEqual(check.typography_errors(root, public), [])

    def test_git_environment_drops_routing_and_config(self):
        with patch.dict(os.environ, {"GIT_DIR": "/never-use", "GIT_WORK_TREE": "/never-use",
                                     "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "core.excludesFile",
                                     "GIT_CONFIG_VALUE_0": "/never-use", "GIT_TEMPLATE_DIR": "/never-use"}):
            env = check.git_environment()
        self.assertEqual({key for key in env if key.startswith("GIT_")},
                         {"GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL"})
        self.assertEqual(env["GIT_CONFIG_GLOBAL"], os.devnull)

    @unittest.skipUnless(shutil.which("git"), "git is required for publication ignore checks")
    def test_real_git_detects_missing_ignore_and_hidden_source(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            patterns = "".join("/" + name + "\n" for name in check.IGNORED_PATHS)
            (root / ".gitignore").write_text(patterns, encoding="utf-8")
            with patch.dict(os.environ, {"GIT_DIR": str(root / "must-not-exist"),
                                         "GIT_WORK_TREE": str(root / "must-not-exist")}):
                self.assertEqual(check.publication_errors(root), [])
            self.assertFalse((root / "must-not-exist").exists())
            self.assertFalse((root / ".git").exists())
            # An unanchored binary name must not hide cmd/proxy source.
            (root / ".gitignore").write_text(patterns.replace("/proxy.yaml\n", "") + "proxy\n",
                                            encoding="utf-8")
            errors = check.publication_errors(root)
            self.assertIn("sensitive/runtime path is not ignored: proxy.yaml", errors)
            self.assertIn("public source is ignored: cmd/proxy/main.go", errors)

    def test_typography_checks_text_without_following_symlinks(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            dash = chr(0x2014)
            (root / "source.js").write_text("// fine\n// wrong " + dash + " punctuation\n", encoding="utf-8")
            (root / "binary").write_bytes(b"\0" + dash.encode("utf-8"))
            (root / "invalid-utf8").write_bytes(b"\xff" + dash.encode("utf-8"))
            (root / "link").symlink_to(root / "source.js")
            self.assertEqual(check.typography_errors(root, ["source.js", "binary", "invalid-utf8", "link"]),
                             ["source.js:2: use sentence punctuation or '-' instead of an em dash"])

    @unittest.skipUnless(shutil.which("git"), "git is required for publication ignore checks")
    def test_publication_typography_excludes_private_and_dependency_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            patterns = "".join("/" + name + "\n" for name in check.IGNORED_PATHS)
            (root / ".gitignore").write_text(patterns + "node_modules/\n", encoding="utf-8")
            (root / "node_modules").mkdir()
            for name in ["proxy.yaml", "node_modules/private.js", "public.js"]:
                (root / name).write_text(chr(0x2014), encoding="utf-8")
            with patch.dict(os.environ, {"GIT_DIR": str(root / "must-not-exist"),
                                         "GIT_WORK_TREE": str(root / "must-not-exist")}):
                self.assertEqual(check.publication_errors(root),
                                 ["public.js:1: use sentence punctuation or '-' instead of an em dash"])
            self.assertFalse((root / "must-not-exist").exists())


if __name__ == "__main__":
    unittest.main()
