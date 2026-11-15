#!/usr/bin/env python3
"""Deterministic regressions for the dev lifecycle's drain-wait derivation.

`scripts/dev.sh stop` must wait at least the configured drain plus the margin
the proxy itself uses before reclaiming a stale port. The derivation reads the
instance's own config and refuses anything it cannot read: substituting a
default is the defect these rows pin, because the old short default made a
normal stop SIGKILL the proxy mid-drain.

Every row sources the shipped shell functions out of `scripts/dev.sh` instead
of reimplementing them. Bash is only the host for those pure functions: no dev
instance is started, no network is touched, and the one row that exercises
`stop` stubs the process calls so no signal is ever sent.
"""

import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path

from ..support import ROOT
from .workflows_support import shell_declaration

DEV_SH = ROOT / "scripts" / "dev.sh"
PORTCLAIM = ROOT / "cmd" / "proxy" / "portclaim.go"
OWNERS = ("go_duration_tenths", "shutdown_timeout_value", "stop_grace_tenths", "dev_db_path")


def dev_script_text():
    return DEV_SH.read_text(encoding="utf-8")


def die_declaration(text):
    """Return dev.sh's one-line die helper, which declares no body lines."""
    match = re.search(r"(?m)^die\(\) \{.*\}$", text)
    return match.group(0) if match else ""


def shell_harness(extra=""):
    """The shipped owner declarations, ready to drive from a temp config."""
    text = dev_script_text()
    parts = [die_declaration(text)]
    parts.extend(shell_declaration(text, name) for name in OWNERS)
    missing = [part[:18] for part in parts if not part]
    if missing:
        raise AssertionError("scripts/dev.sh no longer declares: " + ", ".join(OWNERS))
    return "set -euo pipefail\n" + "\n".join(parts) + extra


def run_bash(script, *args):
    return subprocess.run(
        ["bash", "-c", script, "bash", *args],
        capture_output=True, text=True, timeout=30, check=False)


@unittest.skipUnless(shutil.which("bash"), "bash is required to drive the dev lifecycle owners")
class DrainWaitTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="millivolt-drain-wait-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)

    def config(self, text):
        path = self.root / "millivolt-dev-18081.yaml"
        path.write_text(text, encoding="utf-8")
        return path

    def derive(self, text):
        """Run the shipped derivation against one config document."""
        path = self.config(text)
        script = shell_harness('\nCONFIG_FILE="$1"\nstop_grace_tenths\n')
        return run_bash(script, str(path))

    def test_duration_grammar_matches_go_time_duration(self):
        """Table against Go's time.ParseDuration grammar: every unit, repeated
        terms, a leading sign and decimal forms. The expected tenths are what
        the real parser produces (no margin here)."""
        rows = (
            # The confirmed defect: the old sed accepted 1-3 digits then s/m.
            ("5s", 50),
            ("30s", 300),
            ("1000s", 10000),
            ("1m", 600),
            ("2m30s", 1500),
            ("1h", 36000),
            ("1h30m", 54000),
            ("1h2m3s", 37230),
            ("500ms", 5),
            ("50ms", 1),
            ("250ns", 1),
            ("1us", 1),
            ("+30s", 300),
            (".5s", 5),
            ("5.s", 50),
            ("1.5s", 15),
            ("0.25s", 3),
            ("1.000000001s", 11),
            ("00005s", 50),
            ("24h", 864000),
            ("23h59m59s", 863990),
        )
        script = shell_harness('\ngo_duration_tenths "$1" "$2"\n')
        for value, expected in rows:
            with self.subTest(value=value):
                result = run_bash(script, value, "fixture.yaml")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), str(expected), value)

    def test_grammar_bounds_the_way_the_proxy_does(self):
        """Every accepted value is truncated to whole nanoseconds exactly as
        time.Duration stores it, then ceiled to the 100ms poll tick. These rows
        are the large-unit fraction shapes a nine-digit cap got wrong."""
        rows = (
            ("1.0000000005s", 10),      # sub-nanosecond tail truncates away
            ("5.0000000005s", 50),
            ("99.0000000005s", 990),
            ("1.000000001s", 11),
            ("0.0000000005h", 1),       # 1800ns survives for an hour unit
            ("0.0000000000005h", 1),    # 1.8ns
            ("1h0m0s", 36000),
            ("0m30s", 300),
            ("0.000001s", 1),           # 1us rounds up to one tick
            ("100ns", 1),
            ("100000000ns", 1),         # exactly one 100ms tick
            ("100000001ns", 2),         # one tick plus a nanosecond
            ("23h59m59s999ms", 864000),
            ("24h", 864000),
        )
        script = shell_harness('\ngo_duration_tenths "$1" "$2"\n')
        for value, expected in rows:
            with self.subTest(value=value):
                result = run_bash(script, value, "fixture.yaml")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), str(expected), value)
        # A value that truncates to zero nanoseconds is what the config loader
        # rejects as non-positive; naming the grammar would be misleading.
        result = run_bash(script, "0.0000000000001h", "fixture.yaml")
        self.assertNotEqual(result.returncode, 0,
                            "a duration that truncates to zero must be refused")
        self.assertIn("must be positive", result.stderr)

    def test_bounds_reject_non_positive_and_absurd_values(self):
        """The proxy refuses a zero drain and a day-long one is a unit slip.
        Both must stop the operator with a clear reason, not a derived wait."""
        script = shell_harness('\ngo_duration_tenths "$1" "$2"\n')
        for value, needle in (
            ("0s", "must be positive"),
            ("0", "must be positive"),
            ("-1ns", "must be positive"),
            ("-1h", "must be positive"),
            ("24h1ns", "is longer than 24h"),
            ("25h", "is longer than 24h"),
            ("1000000h", "is longer than 24h"),
            ("2562047h47m16.854775807s", "is longer than 24h"),
            ("99999999999999999999s", "is longer than 24h"),
        ):
            with self.subTest(value=value):
                result = run_bash(script, value, "fixture.yaml")
                self.assertNotEqual(result.returncode, 0, "accepted: " + value)
                self.assertEqual(result.stdout.strip(), "", "derived a number for " + value)
                self.assertIn(needle, result.stderr)
                self.assertIn('"' + value + '"', result.stderr)

    def test_parser_rejects_values_the_proxy_would_reject(self):
        """A duration string handed in directly is judged by Go's grammar; a
        YAML comment never reaches this function because the config reader
        strips it first."""
        script = shell_harness('\ngo_duration_tenths "$1" "$2"\n')
        for value, needle in (
            ("5x", "is not a Go duration"),
            ("5", "is missing a unit"),
            ("5m30", "is missing a unit"),
            ("5 s", "is not a Go duration"),
            ("5S", "is not a Go duration"),
            ("45s # drain", "is not a Go duration"),
            ("1h-30m", "is not a Go duration"),
            ("0s", "must be positive"),
            ("-5s", "must be positive"),
            ("0", "must be positive"),
            ("100h", "is longer than 24h"),
            ("", "is empty"),
        ):
            with self.subTest(value=value):
                result = run_bash(script, value, "millivolt-dev-18081.yaml")
                self.assertNotEqual(result.returncode, 0, "accepted: " + repr(value))
                self.assertEqual(result.stdout.strip(), "", "derived a number for " + repr(value))
                self.assertIn(needle, result.stderr)
                if value:
                    self.assertIn('"' + value + '"', result.stderr)

    def test_stop_grace_adds_the_proxy_margin_to_every_shape(self):
        rows = (
            ("shutdown_timeout: 5s\n", 100),
            ("shutdown_timeout: 30s\n", 350),
            ("shutdown_timeout: 1000s\n", 10050),
            ("shutdown_timeout: 1m\n", 650),
            ("shutdown_timeout: 2m30s\n", 1550),
            ("shutdown_timeout: 1h\n", 36050),
            ("shutdown_timeout: 1h30m\n", 54050),
            ("shutdown_timeout: 45s # drain\n", 500),
            ("shutdown_timeout: 500ms\n", 55),
            ('shutdown_timeout: "5s"\n', 100),
            ("shutdown_timeout: '5s'\n", 100),
            ('shutdown_timeout: "45s" # drain\n', 500),
            ("shutdown_timeout:    2m30s   \n", 1550),
            ("shutdown_timeout: +30s\n", 350),
            ("server:\n  listen: 127.0.0.1:8081\nshutdown_timeout: 30s\n", 350),
        )
        for text, expected in rows:
            with self.subTest(config=text):
                result = self.derive(text)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), str(expected), text)

    def test_underivable_config_fails_loudly_instead_of_defaulting(self):
        """The old code fell back to the 5s default (100 tenths with margin) for
        every one of these. Each must refuse instead, naming the file and the
        value, and must never print a derived number."""
        rows = (
            ("missing key", "idle_timeout: 30s\n", "shutdown_timeout is not set"),
            ("commented-out key", "# shutdown_timeout: 30s\n", "shutdown_timeout is not set"),
            ("nested-only key", "server:\n  shutdown_timeout: 30s\n", "shutdown_timeout is not set"),
            ("empty value", "shutdown_timeout:\n", "shutdown_timeout is empty"),
            ("malformed value", "shutdown_timeout: 5x\n", "is not a Go duration"),
            ("no unit", "shutdown_timeout: 5\n", "is missing a unit"),
            ("duplicate key", "shutdown_timeout: 5s\nshutdown_timeout: 6s\n",
             "shutdown_timeout is set 2 times"),
            ("zero", "shutdown_timeout: 0s\n", "must be positive"),
            ("negative", "shutdown_timeout: -5s\n", "must be positive"),
            ("absurd", "shutdown_timeout: 100h\n", "is longer than 24h"),
        )
        for label, text, needle in rows:
            with self.subTest(case=label):
                path = self.config(text)
                result = self.derive(text)
                self.assertNotEqual(result.returncode, 0, label + " was accepted")
                self.assertEqual(result.stdout.strip(), "", label + " derived a number")
                self.assertIn(needle, result.stderr)
                self.assertIn(str(path), result.stderr, "the failure must name the config file")
                raw = re.search(r"(?m)^shutdown_timeout:[ \t]*(.*)$", text)
                if raw and raw.group(1).strip():
                    self.assertIn(raw.group(1).strip(), result.stderr,
                                  "the failure must name the value")

    def test_margin_matches_the_proxy_owner(self):
        """The shell margin is one fact with cmd/proxy/portclaim.go. A change to
        either side must redden this row instead of silently shortening stop."""
        source = PORTCLAIM.read_text(encoding="utf-8")
        match = re.search(r"sigtermCloseDrainMargin\s*=\s*(\d+)\s*\*\s*time\.(\w+)", source)
        self.assertIsNotNone(match, "portclaim.go no longer declares sigtermCloseDrainMargin")
        scale = {"Nanosecond": 1e-9, "Microsecond": 1e-6, "Millisecond": 1e-3,
                 "Second": 1, "Minute": 60, "Hour": 3600}[match.group(2)]
        proxy_seconds = int(match.group(1)) * scale
        grace = shell_declaration(dev_script_text(), "stop_grace_tenths")
        margin = re.search(r"(?m)^\s*(?:local\s+)?margin_s=(\d+)\b", grace)
        self.assertIsNotNone(margin, "stop_grace_tenths no longer declares margin_s")
        self.assertEqual(int(margin.group(1)), proxy_seconds,
                         "dev.sh margin_s must equal portclaim.go sigtermCloseDrainMargin")

    def test_stop_derives_the_wait_before_it_signals(self):
        """Ordering inside the shipped stop(): a config it cannot read must
        refuse before the first signal, never start a stop with no wait."""
        text = dev_script_text()
        stop = shell_declaration(text, "stop")
        self.assertTrue(stop, "scripts/dev.sh no longer declares stop")
        stubs = (
            '\nPID_FILE="$1"\nCONFIG_FILE="$2"\nKILL_LOG="$3"\n'
            'running_pid() { cat "$PID_FILE"; }\n'
            'is_running() { return 1; }\n'
            'not_running() { :; }\n'
            'kill() { printf \'%s\\n\' "$*" >>"$KILL_LOG"; return 1; }\n'
            'stop\n')
        script = shell_harness("\n" + stop + stubs)
        pid_file = self.root / "millivolt-dev-18081.pid"
        pid_file.write_text("1234\n", encoding="utf-8")
        log = self.root / "kill.log"

        log.write_text("", encoding="utf-8")
        refused = run_bash(script, str(pid_file), str(self.config("shutdown_timeout: 5x\n")), str(log))
        self.assertNotEqual(refused.returncode, 0, "an unparseable config must refuse the stop")
        self.assertEqual(log.read_text(encoding="utf-8"), "",
                         "stop signalled before deriving its drain wait")
        self.assertTrue(pid_file.exists(), "a refused stop must leave the instance tracked")

        log.write_text("", encoding="utf-8")
        accepted = run_bash(script, str(pid_file), str(self.config("shutdown_timeout: 30s\n")), str(log))
        self.assertEqual(accepted.returncode, 0, accepted.stderr)
        # The configured 30s plus the 5s margin is 350 ticks, but the stubbed
        # liveness probe ends the poll on the first tick. The order is SIGTERM
        # then poll, and the wait was derived before either.
        self.assertEqual(log.read_text(encoding="utf-8").splitlines(), ["1234", "-0 1234"])
        self.assertIn("stopped dev instance (pid 1234)", accepted.stdout)

    def test_scratch_database_can_never_name_another_instance(self):
        """The start sequence hard-deletes DEV_DB, so a name claiming a
        different port destroys a live database. The old guard matched only the
        namespace, so ...-8082.db with DEV_PORT=8081 was accepted and deleted
        the database the 8082 instance had open."""
        script = shell_harness('\ndev_db_path "$1" "$2" "$3"\n')
        cases = (
            # A name that names another port is the destructive case.
            ("/tmp/millivolt/millivolt-dev-8082.db", "8081", False, "another port"),
            ("/tmp/millivolt/millivolt-dev-mcp-18081.db", "8081", False, "another port in a label"),
            ("/tmp/millivolt/millivolt-dev-18099.db", "8081", False, "another port, exact shape"),
            # The namespace and directory are still absolute boundaries.
            ("/tmp/millivolt/prod.db", "8081", False, "outside the namespace"),
            ("/tmp/millivolt/sub/millivolt-dev-8081.db", "8081", False, "a subdirectory"),
            ("/srv/millivolt/proxy.db", "8081", False, "a production path"),
            # Every real fixture name must keep working, including the ones
            # that name no port at all.
            ("/tmp/millivolt/millivolt-dev-8081.db", "8081", True, "the port-keyed default"),
            ("/tmp/millivolt/millivolt-dev-mcp-8081.db", "8081", True, "a labelled own port"),
            ("/tmp/millivolt/millivolt-dev-ci.db", "18081", True, "the CI fixture"),
            ("/tmp/millivolt/millivolt-dev-docs.db", "18081", True, "the docs fixture"),
            ("/tmp/millivolt/millivolt-dev-safe.db", "18081", True, "the browser fixture"),
            ("/tmp/millivolt/millivolt-dev-mode-test.db", "18087", True, "the copy-mode fixture"),
            ("/tmp/millivolt/millivolt-dev-run.db", "18099", True, "the stress fixture"),
            ("/tmp/millivolt/millivolt-dev-08081.db", "8081", True, "a zero-padded own port"),
            ("/tmp/millivolt/millivolt-dev-8081-v2.db", "8081", True, "own port plus a numeric label"),
            ("none", "8081", True, "explicit none"),
        )
        for database, port, accepted, label in cases:
            with self.subTest(case=label):
                result = run_bash(script, database, port, "/tmp/millivolt")
                self.assertEqual(result.returncode == 0, accepted,
                                 label + ": " + database + "\n" + result.stderr)
                if accepted:
                    expected = "none" if database == "none" else database
                    self.assertEqual(result.stdout.strip(), expected, label)
                else:
                    self.assertEqual(result.stdout.strip(), "", label + " resolved a path")
                    self.assertIn("DEV_DB", result.stderr)
        # The function's own port check is what refuses the destructive name;
        # the message must say so, or an operator cannot tell why it stopped.
        wrong = run_bash(script, "/tmp/millivolt/millivolt-dev-8082.db", "8081", "/tmp/millivolt")
        self.assertIn("8082", wrong.stderr)
        self.assertIn("8081", wrong.stderr)

    def test_scratch_database_cannot_be_smuggled_through_a_symlink(self):
        """`realpath -m` resolves the final component, so a link named for a
        harmless fixture resolved to another instance's database and the start
        sequence then deleted that database. `realpath -ms` keeps the link's own
        name, and the guard refuses a symlink outright."""
        script = shell_harness('\ndev_db_path "$1" "$2" "$3"\n')
        live = self.root / "millivolt-dev-run.db"
        live.write_text("another instance's live data", encoding="utf-8")
        (self.root / "millivolt-dev-ci.db").symlink_to(live.name)
        (self.root / "millivolt-dev-safe.db").symlink_to(live)
        for name, label in (("millivolt-dev-ci.db", "relative link"),
                            ("millivolt-dev-safe.db", "absolute link")):
            with self.subTest(link=label):
                result = run_bash(script, str(self.root / name), "8081", str(self.root))
                self.assertNotEqual(result.returncode, 0, label + " was accepted")
                self.assertEqual(result.stdout.strip(), "", label + " resolved a path")
                self.assertIn("symlink", result.stderr)
                # The destructive half: the accepted-and-deleted target must
                # never happen, so the fixture has to survive the refusal.
                self.assertTrue(live.exists(), label + " destroyed the target")
                self.assertEqual(live.read_text(encoding="utf-8"),
                                 "another instance's live data", label + " rewrote the target")

    def test_scratch_database_port_claim_is_any_digit_run(self):
        """A port claim is any dash-delimited all-digit field, of any length,
        compared with leading zeros stripped. A four-digit bound skipped
        `-065535`, which is port 65535: a name `dev.sh` itself would refuse as
        DEV_PORT, accepted as a database name."""
        script = shell_harness('\ndev_db_path "$1" "$2" "$3"\n')
        cases = (
            ("/tmp/millivolt/millivolt-dev-065535.db", "8081", False, "a six-digit foreign port"),
            ("/tmp/millivolt/millivolt-dev-065535.db", "65535", True, "a six-digit own port"),
            ("/tmp/millivolt/millivolt-dev-08081.db", "8081", True, "a five-digit own port"),
            ("/tmp/millivolt/millivolt-dev-0008081.db", "8081", True, "a padded own port"),
            ("/tmp/millivolt/millivolt-dev-0008082.db", "8081", False, "a padded foreign port"),
            ("/tmp/millivolt/millivolt-dev-99999999999999999999.db", "8081", False,
             "a digit run too large for shell arithmetic"),
        )
        for database, port, accepted, label in cases:
            with self.subTest(case=label):
                result = run_bash(script, database, port, "/tmp/millivolt")
                self.assertEqual(result.returncode == 0, accepted,
                                 label + ": " + result.stderr)
                if not accepted:
                    self.assertIn("DEV_DB", result.stderr)


if __name__ == "__main__":
    unittest.main()
