#!/usr/bin/env python3
"""Consistent SQLite snapshot, including committed WAL pages, to a NEW file.

The source is opened read-only. Existing destinations are never overwritten.
Used by dev.sh; also useful for explicit operator backups.
"""
from contextlib import closing
from pathlib import Path
import argparse
import os
import sqlite3

# Internal backup page chunk; allows the source writer to run between steps.
BACKUP_PAGES = 256


def backup(source: Path, destination: Path) -> None:
    source = source.resolve(strict=True)
    with closing(sqlite3.connect(source.as_uri() + "?mode=ro", uri=True)) as src:
        fd = os.open(destination, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        os.close(fd)
        try:
            with closing(sqlite3.connect(destination)) as dst:
                src.backup(dst, pages=BACKUP_PAGES)
        except BaseException:
            # Only this newly created, incomplete backup belongs to us.
            destination.unlink(missing_ok=True)
            raise


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("source", type=Path)
    parser.add_argument("destination", type=Path)
    args = parser.parse_args()
    backup(args.source, args.destination)


if __name__ == "__main__":
    main()
