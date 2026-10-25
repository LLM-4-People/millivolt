"""Unit contracts for the parallel Go test sharder."""

import unittest

from tests import go, shard


class ShardTests(unittest.TestCase):
    def test_shard_assignment_is_disjoint_and_complete(self):
        names = [f"Test{i:03d}" for i in range(101)]
        buckets = shard.shard_names(names, 8)
        self.assertEqual(sorted(name for bucket in buckets for name in bucket), names)
        self.assertEqual(len({name for bucket in buckets for name in bucket}), len(names))

    def test_package_shards_cover_every_name(self):
        names = [f"Test{i:03d}" for i in range(41)]
        tasks = shard.tasks_for_package(
            "github.com/LLM-4-People/millivolt/internal/storage", names)
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
