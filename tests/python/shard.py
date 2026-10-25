"""Unit contracts for the parallel Go test sharder and its race scope."""

import tempfile
import threading
import unittest
from pathlib import Path

from tests import go, race_scope, shard

MODULE = "github.com/LLM-4-People/millivolt"
PROXY = MODULE + "/internal/proxy"
STORAGE = MODULE + "/internal/storage"
ROOT_PACKAGE = MODULE


class ShardTests(unittest.TestCase):
    def test_shard_assignment_is_disjoint_and_complete(self):
        names = [f"Test{i:03d}" for i in range(101)]
        buckets = shard.shard_names(names, 8)
        self.assertEqual(sorted(name for bucket in buckets for name in bucket), names)
        self.assertEqual(len({name for bucket in buckets for name in bucket}), len(names))

    def test_package_shards_cover_every_name(self):
        names = [f"Test{i:03d}" for i in range(41)]
        tasks = shard.tasks_for_package(STORAGE, names)
        self.assertGreater(len(tasks), 1)
        self.assertEqual(sorted(name for task in tasks for name in task.names), names)
        self.assertEqual(len({name for task in tasks for name in task.names}), len(names))

    def test_package_without_tests_produces_no_task(self):
        tasks = shard.tasks_for_package(
            "github.com/LLM-4-People/millivolt/internal/format/ir", [])
        self.assertEqual(tasks, [])

    def test_fixed_port_packages_stay_serial(self):
        for package in shard.SERIAL_PACKAGES:
            tasks = shard.tasks_for_package(package, ["TestA", "TestB", "TestC"])
            self.assertEqual(len(tasks), 1)
            self.assertEqual(tasks[0].names, ("TestA", "TestB", "TestC"))
            self.assertIn("^(?:TestA|TestB|TestC)$", tasks[0].command("/tmp/serial.test"))

    def test_fixed_port_package_keeps_one_task_per_pass(self):
        names = ["TestReloadPublishesNewConfig", "TestPlainHandler"]
        tasks = shard.tasks_for_package(MODULE + "/cmd/proxy", names)
        self.assertEqual(len(tasks), 2)
        self.assertEqual({task.shards for task in tasks}, {1})
        self.assertEqual({task.race for task in tasks}, {True, False})
        for task in tasks:
            command = task.command("/tmp/serial.test")
            self.assertIn("^(?:" + "|".join(task.names) + ")$", command)

    def test_small_packages_do_not_create_empty_shards(self):
        tasks = shard.tasks_for_package(
            "github.com/LLM-4-People/millivolt/internal/web", ["TestOnly"])
        self.assertEqual(len(tasks), 1)
        self.assertEqual(tasks[0].shards, 1)

    def test_ci_shard_selection_is_disjoint(self):
        tasks = [shard.Task(f"pkg{i}", ("TestA",), 0, 1) for i in range(11)]
        selected = [shard.selected(tasks, index, 3) for index in range(3)]
        flat = [task.package for group in selected for task in group]
        self.assertEqual(sorted(flat), sorted(task.package for task in tasks))
        self.assertEqual(len(flat), len(set(flat)))

    def test_command_keeps_overlay_and_race_contract(self):
        task = shard.Task("example.invalid/pkg", ("TestA", "TestB"), 0, 2)
        command = task.command("/tmp/example.test")
        self.assertEqual(command[0], "/tmp/example.test")
        self.assertIn("-test.count=1", command)
        self.assertIn("-test.benchtime=200ms", command)
        self.assertIn("^(?:TestA|TestB)$", command)

    def test_single_shard_task_runs_only_its_assigned_names(self):
        task = shard.Task("example.invalid/pkg", ("TestAlpha", "TestBeta"), 0, 1)
        command = task.command("/tmp/example.test")
        pattern = command[command.index("-test.run") + 1]
        self.assertEqual(pattern, "^(?:TestAlpha|TestBeta)$")
        self.assertIsNotNone(shard.re.fullmatch(pattern, "TestAlpha"))
        self.assertIsNone(shard.re.fullmatch(pattern, "TestAlphaSibling"))

    def test_run_pattern_escapes_regex_metacharacters(self):
        task = shard.Task("example.invalid/pkg", ("TestA.1", "TestB[2]"), 0, 1)
        pattern = task.command("/tmp/example.test")[4]
        self.assertEqual(pattern, r"^(?:TestA\.1|TestB\[2\])$")


class RaceScopeTests(unittest.TestCase):
    def test_passes_partition_the_listing_exactly_once(self):
        names = ["TestStreamPump", "TestRequestMetadata", "TestRetryPacing"]
        race, plain = race_scope.select(PROXY, names)
        self.assertEqual(race, ["TestStreamPump", "TestRetryPacing"])
        self.assertEqual(plain, ["TestRequestMetadata"])
        tasks = shard.tasks_for_package(PROXY, names)
        assigned = [name for task in tasks for name in task.names]
        self.assertEqual(sorted(assigned), sorted(names))
        self.assertEqual(len(assigned), len(set(assigned)))
        self.assertTrue(any(task.race for task in tasks))
        self.assertTrue(any(not task.race for task in tasks))

    def test_unlisted_package_is_default_deny(self):
        names = ["TestAlpha", "TestBeta"]
        race, plain = race_scope.select(ROOT_PACKAGE, names)
        self.assertEqual(race, [])
        self.assertEqual(plain, names)
        tasks = shard.tasks_for_package(ROOT_PACKAGE, names)
        self.assertEqual(len(tasks), 1)
        self.assertFalse(tasks[0].race)

    def test_cmd_proxy_transport_and_watcher_tests_are_race_selected(self):
        names = [
            "TestInProcessTransportEnforcesTheRequestDeadline",
            "TestInProcessTransportEnforcesTheConfiguredBound",
            "TestInProcessTransportNormalCallReturnsPromptly",
            "TestInProcessTransportOversizedBodyKeepsTheClientError",
            "TestRestartProgressStopsAfterDependencyPanic",
            "TestMCPHTTPEndpointAuthAndInProcessDispatch",
            "TestOperatorNamespaceLookAlikesNeverReachInference",
            "TestMetricsPprofRoutesServeOnlyThroughTheOperatorGate",
            "TestDeadlineHandlerSlowProgressingBodyCompletes",
        ]
        race, plain = race_scope.select(MODULE + "/cmd/proxy", names)
        self.assertEqual(race, names)
        self.assertEqual(plain, [])

    def test_all_race_escalates_every_test(self):
        names = ["TestRequestMetadata", "TestStreamPump"]
        race, plain = race_scope.select(PROXY, names, all_race=True)
        self.assertEqual(race, names)
        self.assertEqual(plain, [])
        tasks = shard.tasks_for_package(PROXY, names, all_race=True)
        self.assertTrue(tasks)
        self.assertTrue(all(task.race for task in tasks))
        self.assertEqual(sorted(name for task in tasks for name in task.names), sorted(names))

    def test_assignment_rejects_missing_and_duplicate_names(self):
        names = ["TestAlpha", "TestBeta"]
        listing = {ROOT_PACKAGE: names}
        complete = shard.tasks_for_package(ROOT_PACKAGE, names)
        shard.verify_assignments(listing, complete)
        with self.assertRaises(RuntimeError):
            shard.verify_assignments(
                listing, [shard.Task(ROOT_PACKAGE, ("TestAlpha",), 0, 1, race=False)])
        with self.assertRaises(RuntimeError):
            shard.verify_assignments(listing, [
                shard.Task(ROOT_PACKAGE, ("TestAlpha",), 0, 1, race=True),
                shard.Task(ROOT_PACKAGE, ("TestAlpha", "TestBeta"), 0, 1, race=False),
            ])

    def test_primitive_package_without_selector_fails_audit(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "pkg.go").write_text(
                "package fixture\n\nimport \"sync\"\n\nvar mu sync.Mutex\n", encoding="utf-8")
            failures = race_scope.audit({ROOT_PACKAGE: directory})
        self.assertEqual(len(failures), 1)
        self.assertIn("declares no race selector", failures[0])

    def test_primitive_package_with_selector_passes_audit(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "pkg.go").write_text(
                "package fixture\n\nfunc f() { go func() {}() }\n", encoding="utf-8")
            failures = race_scope.audit(
                {PROXY: directory}, {PROXY: ["TestStreamPump"]},
                selectors={PROXY: ("*Stream*",)})
        self.assertEqual(failures, [])

    def test_comment_and_literal_tokens_are_not_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "pkg.go").write_text(
                "package fixture\n\n"
                "// A future sync.Mutex, sync.WaitGroup and chan live here.\n"
                "/* sync.Once in a block comment. */\n"
                'var label = "sync.Pool and sync.Map"\n'
                "var raw = `go work() and atomic.Int64`\n"
                "var symbol = 'g'\n",
                encoding="utf-8")
            self.assertEqual(race_scope.primitive_evidence(Path(directory)), {})

    def test_modern_sync_channel_and_go_statements_are_evidence(self):
        cases = {
            "waitgroup": "var wg sync.WaitGroup",
            "once": "var once sync.Once",
            "cond": "var cond *sync.Cond",
            "map": "var table sync.Map",
            "pool": "var pool sync.Pool",
            "channel": "var events chan int",
            "go statement": "func f() { go work() }",
        }
        for label, declaration in cases.items():
            with self.subTest(label=label), tempfile.TemporaryDirectory() as directory:
                Path(directory, "pkg.go").write_text(
                    'package fixture\n\nimport "sync"\n\n' + declaration + "\n",
                    encoding="utf-8")
                self.assertTrue(race_scope.primitive_evidence(Path(directory)))

    def test_selector_must_match_a_listed_test(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "pkg.go").write_text(
                "package fixture\n\nimport \"sync/atomic\"\n\nvar n atomic.Int64\n",
                encoding="utf-8")
            failures = race_scope.audit(
                {PROXY: directory}, {PROXY: ["TestPlainRequest"]},
                selectors={PROXY: ("*Stream*",)})
        self.assertEqual(len(failures), 1)
        self.assertIn("match no listed test", failures[0])

    def test_exemption_requires_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "pkg.go").write_text(
                "package fixture\n\nimport \"sync\"\n\nvar mu sync.Mutex\n", encoding="utf-8")
            documented = race_scope.audit(
                {ROOT_PACKAGE: directory}, exemptions={ROOT_PACKAGE: "guarded by the caller"})
            undocumented = race_scope.audit(
                {ROOT_PACKAGE: directory}, exemptions={ROOT_PACKAGE: " "})
        self.assertEqual(documented, [])
        self.assertEqual(len(undocumented), 1)
        self.assertIn("no evidence", undocumented[0])

    def test_real_tree_primitive_packages_declare_race_selectors(self):
        failures = race_scope.audit(shard.packages())
        self.assertEqual(failures, [])


class PackageLockTests(unittest.TestCase):
    def test_fixed_port_package_passes_never_overlap(self):
        package = sorted(shard.SERIAL_PACKAGES)[0]
        locks = shard.PackageLocks(shard.SERIAL_PACKAGES)
        first_inside = threading.Event()
        second_attempted = threading.Event()
        second_inside = threading.Event()
        release = threading.Event()

        def first():
            with locks.hold(package):
                first_inside.set()
                # Hold the package until the parent has observed the second
                # pass blocked outside it.
                release.wait(timeout=5)

        def second():
            first_inside.wait(timeout=5)
            second_attempted.set()
            with locks.hold(package):
                second_inside.set()

        threads = [threading.Thread(target=first), threading.Thread(target=second)]
        for thread in threads:
            thread.start()
        self.assertTrue(first_inside.wait(timeout=5))
        self.assertTrue(second_attempted.wait(timeout=5))
        self.assertFalse(second_inside.wait(timeout=0.3))
        release.set()
        for thread in threads:
            thread.join()
        self.assertTrue(second_inside.is_set())

    def test_distinct_fixed_port_packages_do_not_serialize_each_other(self):
        packages = sorted(shard.SERIAL_PACKAGES)
        locks = shard.PackageLocks(shard.SERIAL_PACKAGES)
        guard = threading.Lock()
        barrier = threading.Barrier(len(packages))
        active = 0
        peak = 0
        errors = []

        def hold(package):
            nonlocal active, peak
            try:
                with locks.hold(package):
                    with guard:
                        active += 1
                        peak = max(peak, active)
                    barrier.wait(timeout=5)
                    with guard:
                        active -= 1
            except Exception as error:  # surfaced through the parent thread
                errors.append(error)

        threads = [threading.Thread(target=hold, args=(package,)) for package in packages]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual(errors, [])
        self.assertEqual(peak, len(packages))


class DiscoveryTests(unittest.TestCase):
    def test_listing_blocks_are_attributed_to_their_package(self):
        output = "\n".join([
            "TestAlpha",
            "TestBeta",
            "ok  \texample.invalid/a\t0.002s",
            "?   \texample.invalid/empty\t[no test files]",
            "FuzzGamma",
            "ExampleDelta",
            "ok  \texample.invalid/b\t0.003s",
        ])
        self.assertEqual(shard.parse_test_listing(output), {
            "example.invalid/a": ["TestAlpha", "TestBeta"],
            "example.invalid/empty": [],
            "example.invalid/b": ["FuzzGamma", "ExampleDelta"],
        })

    def test_listing_rejects_ambiguous_records(self):
        cases = {
            "failed package": "FAIL\texample.invalid/a\t[build failed]",
            "duplicate package": "ok  \texample.invalid/a\t0.001s\nok  \texample.invalid/a\t0.001s",
            "unattributed name": "TestAlpha",
            "stray output": "TestAlpha\nhas no package result\nok  \texample.invalid/a\t0.001s",
        }
        for label, output in cases.items():
            with self.subTest(label=label), self.assertRaises(RuntimeError):
                shard.parse_test_listing(output)


class ParityHandshakeTests(unittest.TestCase):
    """The build handshake must describe exactly the overlay parity proved."""

    mapping = {"/src/pkg/value_test.go": "/src/tests/_go/pkg/value.go"}

    def test_digest_is_stable_for_the_same_proof(self):
        self.assertEqual(go.overlay_digest(self.mapping, {}),
                         go.overlay_digest(dict(self.mapping), {}))

    def test_digest_changes_with_the_overlay(self):
        extra = {**self.mapping, "/src/pkg/extra_test.go": "/src/tests/_go/pkg/extra.go"}
        self.assertNotEqual(go.overlay_digest(self.mapping, {}),
                            go.overlay_digest(extra, {}))

    def test_digest_changes_with_source_selection_environment(self):
        for name in go.PARITY_ENVIRONMENT:
            with self.subTest(name=name):
                self.assertNotEqual(go.overlay_digest(self.mapping, {}),
                                    go.overlay_digest(self.mapping, {name: "fixture"}))


if __name__ == "__main__":
    unittest.main()
