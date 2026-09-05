#!/usr/bin/env python3
"""Exercise the shared release gate with isolated tool fixtures, never a remote."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
REVISION = "a" * 40


class ReleaseGateTest(unittest.TestCase):
    def run_gate(self, extra=(), **changes):
        with tempfile.TemporaryDirectory(prefix="millivolt-release-check-") as tmp:
            root = Path(tmp)
            (root / "scripts").mkdir()
            (root / "tools").mkdir()
            script = root / "scripts/check.sh"
            script.write_bytes((ROOT / "scripts/check.sh").read_bytes())
            go = root / "tools/go"
            go.write_text("#!/bin/sh\n[ \"$FIXTURE_GO_FAIL\" != 1 ] || exit 1\n"
                          "[ -z \"$GIT_DIR$GIT_WORK_TREE$GIT_CONFIG_COUNT$GIT_CONFIG_KEY_0$GIT_CONFIG_VALUE_0\" ] || exit 12\n"
                          "printf 'version=0.1.0\\nprerelease=false\\nrevision=%s\\n' \"$FIXTURE_HEAD\"\n")
            git = root / "tools/git"
            git.write_text("""#!/bin/sh
[ -z "$GIT_DIR$GIT_WORK_TREE$GIT_CONFIG_COUNT$GIT_CONFIG_KEY_0$GIT_CONFIG_VALUE_0" ] || exit 12
case "$*" in
  'rev-parse --verify HEAD') printf '%s\\n' "$FIXTURE_HEAD" ;;
  'status --porcelain --untracked-files=all') printf '%s' "$FIXTURE_STATUS" ;;
  'rev-parse --verify --end-of-options v0.1.0^{commit}') printf '%s\\n' "$FIXTURE_TAG_HEAD" ;;
  *) exit 2 ;;
esac
""")
            go.chmod(0o700)
            git.chmod(0o700)
            env = dict(os.environ, PATH=str(root / "tools") + os.pathsep + os.environ["PATH"],
                       FIXTURE_HEAD=REVISION, FIXTURE_TAG_HEAD=REVISION,
                       FIXTURE_STATUS="", FIXTURE_GO_FAIL="")
            env.update(changes)
            return subprocess.run(["bash", str(script), "release", "-revision", REVISION, *extra],
                                  env=env, text=True, capture_output=True, check=False)

    def test_clean_main_and_tag(self):
        for extra in [(), ("-tag", "v0.1.0")]:
            with self.subTest(extra=extra):
                result = self.run_gate(extra)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("version=0.1.0\n", result.stdout)

    def test_wrong_head(self):
        self.assertNotEqual(self.run_gate(FIXTURE_HEAD="b" * 40).returncode, 0)

    def test_untracked_and_tracked_changes(self):
        for status in ["?? extra.txt\n", " M version.go\n"]:
            self.assertNotEqual(self.run_gate(FIXTURE_STATUS=status).returncode, 0)

    def test_wrong_tag_target(self):
        self.assertNotEqual(self.run_gate(("-tag", "v0.1.0"),
                                         FIXTURE_TAG_HEAD="b" * 40).returncode, 0)

    def test_canonical_validator_failure(self):
        self.assertNotEqual(self.run_gate(FIXTURE_GO_FAIL="1").returncode, 0)

    def test_inherited_git_routing_is_removed_before_every_tool(self):
        result = self.run_gate(GIT_DIR="/unrelated/repo/.git", GIT_WORK_TREE="/unrelated/repo",
                               GIT_CONFIG_COUNT="1", GIT_CONFIG_KEY_0="core.excludesFile",
                               GIT_CONFIG_VALUE_0="/unrelated/ignore")
        self.assertEqual(result.returncode, 0, result.stderr)


if __name__ == "__main__":
    unittest.main()
