#!/usr/bin/env python3
"""Read package-manager lock ownership without acquiring or changing any lock."""

import os
import stat
import sys


PACKAGE_LOCKS = (
    "/var/lib/apt/lists/lock",
    "/var/cache/apt/archives/lock",
    "/var/lib/dpkg/lock",
    "/var/lib/dpkg/lock-frontend",
)


def held_package_locks(paths=PACKAGE_LOCKS, locks_file="/proc/locks"):
    identities = {}
    for path in paths:
        try:
            metadata = os.stat(path, follow_symlinks=False)
        except FileNotFoundError:
            continue
        if not stat.S_ISREG(metadata.st_mode):
            raise ValueError("package lock path is not a regular file: " + path)
        identities[(os.major(metadata.st_dev), os.minor(metadata.st_dev), metadata.st_ino)] = path

    held = []
    with open(locks_file, encoding="ascii") as locks:
        for line in locks:
            fields = line.split()
            if not fields:
                continue
            # A blocked waiter is preceded by '->'; the owner has its own row.
            if len(fields) > 1 and fields[1] == "->":
                fields.pop(1)
            if len(fields) != 8:
                raise ValueError("unrecognized kernel lock record")
            major, minor, inode = fields[5].split(":")
            identity = (int(major, 16), int(minor, 16), int(inode))
            pid = int(fields[4])
            if identity in identities:
                held.append((identities[identity], pid))
    return held


def main():
    try:
        held = held_package_locks()
    except (OSError, ValueError) as error:
        print("EPAR cannot verify package-manager locks: " + str(error), file=sys.stderr)
        return 2
    for path, pid in held:
        print("{} pid={}".format(path, pid))
    return 1 if held else 0


if __name__ == "__main__":
    sys.exit(main())
