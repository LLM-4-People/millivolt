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

    def test_package_parity_accepts_only_platform_exclusions(self):
        runner.package_parity(self.mapping, self.metadata())
        platform_sources = {
            "value_windows.go": "package fixture\n",
            "value_linux_amd64.go": "package fixture\n",
        }
        platform = {}
        for name, content in platform_sources.items():
            path = self.tests / name
            path.write_text(content, encoding="utf-8")
            platform[str(self.root / (name[:-len(".go")] + "_test.go"))] = str(path)
        runner.package_parity(platform, json.dumps({
            "Dir": str(self.root),
            "IgnoredGoFiles": ["value_windows_test.go", "value_linux_amd64_test.go"],
        }))
        # A platform suffix alone is not enough: the file's build constraints
        # must also select a matching platform under default tags. Otherwise a
        # platform-suffixed integration test silently never runs.
        constraints = (
            ("value_linux.go", "//go:build linux\n\npackage fixture\n", True),
            ("value_linux.go", "//go:build linux && amd64\n\npackage fixture\n", True),
            ("value_linux.go", "//go:build !windows\n\npackage fixture\n", True),
            ("value_linux.go", "//go:build integration\n\npackage fixture\n", False),
            ("value_linux.go", "//go:build linux && integration\n\npackage fixture\n", False),
            ("value_linux.go", "// +build integration\n\npackage fixture\n", False),
            ("value_linux.go", "// +build linux\n\npackage fixture\n", True),
            ("value_linux.go", "//go:build windows\n\npackage fixture\n", False),
            ("value_linux_amd64.go", "//go:build arm64\n\npackage fixture\n", False),
            ("value_windows.go", "//go:build unix\n\npackage fixture\n", False),
        )
        for name, content, accepted in constraints:
            with self.subTest(name=name, content=content):
                path = self.tests / name
                path.write_text(content, encoding="utf-8")
                virtual = str(self.root / (name[:-len(".go")] + "_test.go"))
                mapping = {virtual: str(path)}
                document = json.dumps({"Dir": str(self.root),
                                       "IgnoredGoFiles": [Path(virtual).name]})
                if accepted:
                    runner.package_parity(mapping, document)
                else:
                    with self.assertRaisesRegex(RuntimeError, "never run"):
                        runner.package_parity(mapping, document)
        with self.assertRaisesRegex(RuntimeError, "never run"):
            runner.package_parity(self.mapping, self.metadata().replace("TestGoFiles", "IgnoredGoFiles"))
        with self.assertRaisesRegex(RuntimeError, "never run"):
            runner.package_parity(self.mapping, json.dumps(
                {"Dir": str(self.root), "IgnoredGoFiles": ["value_integration_test.go"]}))
        for invalid in ("", "null", "[]", "{}", self.metadata() + self.metadata(),
                        self.metadata().replace("value_test.go", "other_test.go")):
            with self.subTest(document=invalid), self.assertRaises((RuntimeError, ValueError)):
                runner.package_parity(self.mapping, invalid)

    def test_package_parity_accepts_race_files_only_with_race_listing(self):
        race = self.tests / "race_on.go"
        race.write_text("//go:build race\n\npackage fixture\n", encoding="utf-8")
        mapping = dict(self.mapping)
        mapping[str(self.root / "race_on_test.go")] = str(race)
        plain = json.dumps({
            "Dir": str(self.root),
            "TestGoFiles": ["value_test.go"],
            "IgnoredGoFiles": ["race_on_test.go"],
        })
        raced = json.dumps({
            "Dir": str(self.root),
            "TestGoFiles": ["value_test.go", "race_on_test.go"],
        })
        # The default listing alone cannot distinguish a deliberate race gate
        # from any other tag that silently excludes the test.
        with self.assertRaisesRegex(RuntimeError, "never run"):
            runner.package_parity(mapping, plain)
        runner.package_parity(mapping, plain, raced)
        with self.assertRaisesRegex(RuntimeError, "never run"):
            runner.package_parity(mapping, plain, json.dumps({
                "Dir": str(self.root), "TestGoFiles": ["value_test.go"],
            }))
        # A race listing does not launder a different non-platform tag.
        integration = self.tests / "integration.go"
        integration.write_text("//go:build integration\n\npackage fixture\n", encoding="utf-8")
        mapping[str(self.root / "integration_test.go")] = str(integration)
        ignored = json.dumps({
            "Dir": str(self.root),
            "TestGoFiles": ["value_test.go"],
            "IgnoredGoFiles": ["race_on_test.go", "integration_test.go"],
        })
        with self.assertRaisesRegex(RuntimeError, "never run"):
            runner.package_parity(mapping, ignored, raced)

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
        for flag in ("-overlay=elsewhere.json", "--overlay=elsewhere.json", "-C", "--C",
                     "-modfile=elsewhere.mod", "--modfile=elsewhere.mod",
                     "-trimpath", "--trimpath", "-trimpath=true", "--trimpath=true"):
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
        (self.tests / "platform_windows.go").write_text("//go:build windows\n\npackage fixture\n", encoding="utf-8")
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

    @unittest.skipUnless(shutil.which("go"), "Go is required for native overlay integration")
    def test_build_tag_ignored_test_fails_instead_of_passing_parity(self):
        # A build tag that excludes the file on every platform would otherwise
        # satisfy physical/native package parity while never running a test.
        integration = self.tests / "integration.go"
        integration.write_text(
            '//go:build integration\n\npackage fixture\n\nimport "testing"\n\nfunc TestIntegration(t *testing.T) {}\n',
            encoding="utf-8")
        with patch.dict(os.environ, {"GOFLAGS": "", "GOPROXY": "off", "GOSUMDB": "off"}):
            with self.assertRaisesRegex(RuntimeError, "never run"):
                runner.run(["test", "-count=1", "./..."], self.root)
            # A recognized platform suffix must not launder the same silently
            # skipped integration test: Go ignores it for the build tag, not
            # the platform, so parity must stay a hard error.
            integration.unlink()
            (self.tests / "integration_linux.go").write_text(
                '//go:build integration\n\npackage fixture\n\nimport "testing"\n\nfunc TestIntegrationLinux(t *testing.T) {}\n',
                encoding="utf-8")
            with self.assertRaisesRegex(RuntimeError, "never run"):
                runner.run(["test", "-count=1", "./..."], self.root)


if __name__ == "__main__":
    unittest.main()
