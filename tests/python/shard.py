"""Unit contracts for the parallel Go test sharder."""

import unittest

from tests import shard


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


if __name__ == "__main__":
    unittest.main()
