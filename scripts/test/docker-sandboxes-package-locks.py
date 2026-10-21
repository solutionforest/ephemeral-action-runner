#!/usr/bin/env python3
"""Regression checks for the guest's read-only package lock observation."""

import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


source = Path(__file__).resolve().parents[2] / "templates/docker-sandboxes/guest/package-locks.py"
spec = importlib.util.spec_from_file_location("package_locks", source)
helper = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helper)


class PackageLockTests(unittest.TestCase):
    def setUp(self):
        self.scratch = tempfile.TemporaryDirectory()
        self.addCleanup(self.scratch.cleanup)
        self.root = Path(self.scratch.name)
        self.lock = self.root / "lock"
        self.lock.write_text("existing package lock")
        self.inventory = self.root / "proc-locks"
        self.inventory.write_text("")

    def observe(self):
        return helper.held_package_locks([str(self.lock)], str(self.inventory))

    def record(self, kind="POSIX", pid=123, device=None, waiter=False):
        metadata = self.lock.stat()
        major, minor = device or (os.major(metadata.st_dev), os.minor(metadata.st_dev))
        prefix = "1: ->" if waiter else "1:"
        return "{} {} ADVISORY WRITE {} {:02x}:{:02x}:{} 0 EOF\n".format(
            prefix, kind, pid, major, minor, metadata.st_ino
        )

    def test_unheld_and_missing_locks_are_idle_without_changing_files(self):
        before = self.lock.stat()
        self.assertEqual(self.observe(), [])
        self.assertEqual(self.lock.stat().st_ino, before.st_ino)
        self.assertEqual(self.lock.read_text(), "existing package lock")
        self.lock.unlink()
        self.assertEqual(self.observe(), [])
        self.assertFalse(self.lock.exists())

    def test_lock_owner_is_independent_of_process_name_or_cli_command(self):
        for kind, pid, waiter in [("POSIX", 123, False), ("FLOCK", 124, False), ("OFDLCK", -1, False), ("POSIX", 125, True)]:
            with self.subTest(kind=kind, waiter=waiter):
                self.inventory.write_text(self.record(kind, pid, waiter=waiter))
                self.assertEqual(self.observe(), [(str(self.lock), pid)])

    def test_inode_on_another_device_is_not_the_package_lock(self):
        metadata = self.lock.stat()
        self.inventory.write_text(self.record(device=(os.major(metadata.st_dev) + 1, os.minor(metadata.st_dev))))
        self.assertEqual(self.observe(), [])

    def test_unreadable_or_unrecognized_inventory_does_not_prove_idle(self):
        for content in ["unknown schema\n", "1: POSIX ADVISORY WRITE unknown 00:01:2 0 EOF\n", "1: POSIX ADVISORY WRITE 123 invalid 0 EOF\n"]:
            with self.subTest(content=content):
                self.inventory.write_text(content)
                with self.assertRaises(ValueError):
                    self.observe()
        self.inventory.unlink()
        with self.assertRaises(OSError):
            self.observe()

    def test_nonregular_lock_paths_do_not_prove_idle(self):
        self.lock.unlink()
        self.lock.mkdir()
        with self.assertRaises(ValueError):
            self.observe()
        self.lock.rmdir()
        self.lock.symlink_to(self.inventory)
        with self.assertRaises(ValueError):
            self.observe()

    @unittest.skipUnless(sys.platform == "linux", "requires Linux /proc/locks")
    def test_actual_unknown_process_holds_and_releases_posix_lock(self):
        owner = subprocess.Popen(
            [sys.executable, "-c", "import fcntl, sys; lock = open(sys.argv[1], 'r+'); fcntl.lockf(lock, fcntl.LOCK_EX); print('locked', flush=True); sys.stdin.read()", str(self.lock)],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
        )
        try:
            self.assertEqual(owner.stdout.readline().strip(), "locked")
            self.assertIn((str(self.lock), owner.pid), helper.held_package_locks([str(self.lock)]))
        finally:
            owner.communicate(timeout=5)
        self.assertEqual(helper.held_package_locks([str(self.lock)]), [])


if __name__ == "__main__":
    unittest.main()
