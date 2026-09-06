"""Deterministic ownership and native-tool regressions for the Go overlay."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

from .. import go as runner
from ..support import ROOT


@unittest.skipUnless(shutil.which("git"), "Git is required for repository source discovery")
class OverlayTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="millivolt-overlay-fixture-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        (self.root / ".gitignore").write_text("private/\n", encoding="utf-8")
        directive = next(line for line in (ROOT / "go.mod").read_text().splitlines() if line.startswith("go "))
        (self.root / "go.mod").write_text("module example.invalid/fixture\n\n" + directive + "\n", encoding="utf-8")
        (self.root / "value.go").write_text("package fixture\nfunc privateValue() int { return 42 }\n", encoding="utf-8")
        self.tests = self.root / "tests/_go"
        self.tests.mkdir(parents=True)
        self.backing = self.tests / "value.go"
        self.backing.write_text('''package fixture
import ("testing"; "os"; "runtime"; "path/filepath")
func TestPrivate(t *testing.T) {
    if privateValue() != 42 { t.Fatal("private access failed") }
    if _, err := os.ReadFile("go.mod"); err != nil { t.Fatal("source cwd changed", err) }
    cwd, _ := os.Getwd()
    _, file, _, _ := runtime.Caller(0)
    if file != filepath.Join(cwd, "value_test.go") { t.Fatal("source identity changed", file) }
}
''', encoding="utf-8")
        self.mapping = {str(self.root / "value_test.go"): str(self.backing)}

    def metadata(self):
        return json.dumps({"Dir": str(self.root), "TestGoFiles": ["value_test.go"]})

    def test_discovery_and_temp_cleanup(self):
        self.assertEqual(runner.discover(self.root), self.mapping)
        second = self.tests / "another.go"
        second.write_text("package fixture\n", encoding="utf-8")
        self.mapping[str(self.root / "another_test.go")] = str(second)
        private = self.root / "private"
        private.mkdir()
        (private / "private_test.go").write_text("not public source\n", encoding="utf-8")
        with runner.overlay(self.root) as (path, mapping):
            self.assertEqual(mapping, self.mapping)
            self.assertEqual(list(mapping), sorted(mapping))
            self.assertEqual(json.loads(path.read_text()), {"Replace": self.mapping})
            self.assertEqual(path.parent.stat().st_mode & 0o777, 0o700)
            self.assertFalse((self.root / "value_test.go").exists())
        self.assertFalse(path.parent.exists())

    def test_empty_misplaced_duplicate_hidden_and_orphan_tests_fail(self):
        with self.subTest("empty"):
            self.backing.unlink()
            with self.assertRaisesRegex(RuntimeError, "no Go tests"):
                runner.discover(self.root)
            self.backing.write_text("package fixture\n", encoding="utf-8")
        with self.subTest("source neighbor"):
            duplicate = self.root / "value_test.go"
            duplicate.write_text("package fixture\n", encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "under tests/"):
                runner.discover(self.root)
            duplicate.unlink()
        with self.subTest("hidden"):
            (self.root / ".gitignore").write_text("tests/_go/value.go\n", encoding="utf-8")
            with self.assertRaises(RuntimeError):
                runner.discover(self.root)
            (self.root / ".gitignore").write_text("", encoding="utf-8")
        with self.subTest("orphan"):
            directory = self.tests / "orphan"
            directory.mkdir()
            (directory / "orphan.go").write_text("package orphan\n", encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "source package"):
                runner.discover(self.root)

    def test_redundant_test_suffix_and_symlinks_fail(self):
        source = self.tests / "helper_test.go"
        source.write_text("package fixture\n", encoding="utf-8")
        with self.assertRaisesRegex(RuntimeError, "omit _test suffix"):
            runner.discover(self.root)
        source.unlink()
        source = self.tests / "helper.go"
        source.symlink_to(self.backing)
        with self.assertRaisesRegex(RuntimeError, "symlink"):
            runner.discover(self.root)

    def test_package_parity_includes_platform_exclusions(self):
        runner.package_parity(self.mapping, self.metadata())
        runner.package_parity(self.mapping, self.metadata().replace("TestGoFiles", "IgnoredGoFiles"))
        for invalid in ("", "null", "[]", "{}", self.metadata() + self.metadata(),
                        self.metadata().replace("value_test.go", "other_test.go")):
            with self.subTest(document=invalid), self.assertRaises((RuntimeError, ValueError)):
                runner.package_parity(self.mapping, invalid)

    def test_command_ownership_exit_status_and_cleanup(self):
        paths = []

        def command(arguments, root, env, capture=False):
            flags = [argument for argument in arguments if argument.startswith("-overlay=")]
            self.assertEqual(len(flags), 1)
            path = Path(flags[0].split("=", 1)[1])
            paths.append(path)
            self.assertEqual(json.loads(path.read_text()), {"Replace": self.mapping})
            self.assertEqual(root, self.root)
            self.assertEqual(env["GOWORK"], "off")
            return subprocess.CompletedProcess(arguments, 0 if capture else 17,
                                               self.metadata() if capture else "", "")

        with patch.object(runner, "go_command", side_effect=command):
            self.assertEqual(runner.run(["test", "./..."], self.root), 17)
        self.assertEqual(len(paths), 2)
        self.assertTrue(all(not path.parent.exists() for path in paths))

    def test_conflicting_flags_rejected_before_commands(self):
        for flag in ("-overlay=elsewhere.json", "--overlay=elsewhere.json", "-C", "--C", "-modfile=elsewhere.mod", "--modfile=elsewhere.mod"):
            with self.subTest(flag=flag), patch.object(runner, "go_command") as command:
                with self.assertRaisesRegex(RuntimeError, "owned by"):
                    runner.run(["test", flag], self.root)
                with patch.dict(os.environ, {"GOFLAGS": flag}), self.assertRaisesRegex(RuntimeError, "owned by"):
                    runner.run(["test"], self.root)
                command.assert_not_called()

    def test_command_exception_cleans_overlay(self):
        paths = []

        def fail(arguments, *args, **kwargs):
            paths.append(Path(next(a.split("=", 1)[1] for a in arguments if a.startswith("-overlay="))))
            raise RuntimeError("fixture failure")

        with patch.object(runner, "go_command", side_effect=fail), self.assertRaisesRegex(RuntimeError, "fixture failure"):
            runner.run(["test"], self.root)
        self.assertTrue(paths)
        self.assertTrue(all(not path.parent.exists() for path in paths))

    @unittest.skipUnless(shutil.which("go"), "Go is required for native overlay integration")
    def test_native_private_access_fixture_paths_and_test_registration(self):
        # Helpers, external-package tests and platform suffixes all retain Go's
        # native behavior after the required virtual suffix is appended.
        (self.tests / "helper.go").write_text("package fixture\nfunc helperValue() int { return privateValue() }\n", encoding="utf-8")
        (self.tests / "helper_check.go").write_text('''package fixture
import "testing"
func TestHelper(t *testing.T) { if helperValue() != 42 { t.Fatal("helper missing") } }
''', encoding="utf-8")
        (self.tests / "external.go").write_text('''package fixture_test
import "testing"
func TestExternal(t *testing.T) {}
''', encoding="utf-8")
        (self.tests / "platform_windows.go").write_text("//go:build never && !never\n\npackage fixture\n", encoding="utf-8")
        with runner.overlay(self.root) as (path, mapping):
            self.assertIn(str(self.root / "platform_windows_test.go"), mapping)
            listing = runner.go_command(["test", "-overlay=" + str(path), "-list", ".", "./..."],
                                        self.root, dict(os.environ, GOFLAGS="", GOWORK="off"), capture=True)
            self.assertEqual(listing.returncode, 0, listing.stderr)
            self.assertEqual([line for line in listing.stdout.splitlines() if line.startswith("Test")],
                             ["TestHelper", "TestPrivate", "TestExternal"])
        with patch.dict(os.environ, {"GOFLAGS": "", "GOPROXY": "off", "GOSUMDB": "off"}):
            self.assertEqual(runner.run(["test", "-count=1", "./..."], self.root), 0)
            self.assertEqual(runner.run(["vet", "./..."], self.root), 0)
            self.assertEqual(runner.run(["mod", "tidy", "-diff"], self.root), 0)


if __name__ == "__main__":
    unittest.main()
