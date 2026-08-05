#!/usr/bin/env python3
"""Safe SQLite backup and restore verification for the Pong PVC.

The backup operation uses SQLite's online backup API, so it does not copy a
live database by byte-for-byte filesystem race. The verify operation restores
only into a new temporary database; it never writes to a live PVC.
"""
from __future__ import annotations

import argparse
import os
import sqlite3
import sys
import tempfile
from pathlib import Path


def integrity_check(connection: sqlite3.Connection, label: str) -> None:
    result = connection.execute("PRAGMA integrity_check").fetchone()
    if not result or result[0] != "ok":
        raise RuntimeError(f"{label} integrity_check failed: {result!r}")


def read_only_connection(path: Path) -> sqlite3.Connection:
    return sqlite3.connect(f"file:{path.resolve()}?mode=ro", uri=True)


def backup(source: Path, destination: Path, dry_run: bool) -> None:
    if source.resolve() == destination.resolve():
        raise ValueError("source and destination must be different")
    if dry_run:
        print(f"would online-backup {source} -> {destination}")
        if not source.exists():
            print("source does not exist locally; this is a command preview only")
        print("would run PRAGMA integrity_check on source and backup")
        return
    if not source.exists():
        raise FileNotFoundError(f"SQLite source does not exist: {source}")

    destination.parent.mkdir(parents=True, exist_ok=True)
    partial = destination.with_name(destination.name + ".partial")
    if partial.exists():
        partial.unlink()
    try:
        with read_only_connection(source) as source_db, sqlite3.connect(partial) as backup_db:
            integrity_check(source_db, "source")
            source_db.backup(backup_db)
            integrity_check(backup_db, "backup")
            backup_db.execute("PRAGMA journal_mode=DELETE")
            backup_db.commit()
        os.replace(partial, destination)
        print(f"backup created and verified: {destination}")
    finally:
        partial.unlink(missing_ok=True)


def verify(backup_path: Path, dry_run: bool) -> None:
    if dry_run:
        print(f"would restore {backup_path} into a new temporary SQLite database")
        if not backup_path.exists():
            print("backup does not exist locally; this is a command preview only")
        print("would run PRAGMA integrity_check and enumerate application tables")
        return
    if not backup_path.exists():
        raise FileNotFoundError(f"SQLite backup does not exist: {backup_path}")

    with tempfile.TemporaryDirectory(prefix="pong-restore-") as directory:
        restored = Path(directory) / "pong.db"
        with read_only_connection(backup_path) as backup_db, sqlite3.connect(restored) as restored_db:
            backup_db.backup(restored_db)
            integrity_check(restored_db, "restored database")
            tables = [row[0] for row in restored_db.execute(
                "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"
            )]
        print(f"restore verification passed: {backup_path}")
        print(f"restored tables: {', '.join(tables) if tables else '(none)'}")


def self_test(dry_run: bool) -> None:
    if dry_run:
        print("would create a temporary SQLite database, online-backup it, and verify the restore")
        return
    with tempfile.TemporaryDirectory(prefix="pong-backup-test-") as directory:
        root = Path(directory)
        source = root / "source.db"
        backup_path = root / "backup.db"
        with sqlite3.connect(source) as connection:
            connection.execute("CREATE TABLE rooms (id TEXT PRIMARY KEY, status TEXT NOT NULL)")
            connection.execute("INSERT INTO rooms VALUES ('dry-run-room', 'waiting')")
            connection.commit()
        backup(source, backup_path, False)
        verify(backup_path, False)
        with sqlite3.connect(backup_path) as connection:
            row = connection.execute("SELECT id, status FROM rooms").fetchone()
        if row != ("dry-run-room", "waiting"):
            raise RuntimeError(f"backup round-trip data mismatch: {row!r}")
        print("self-test passed: row data survived backup and restore verification")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    backup_parser = subparsers.add_parser("backup")
    backup_parser.add_argument("source", type=Path)
    backup_parser.add_argument("destination", type=Path)
    backup_parser.add_argument("--dry-run", action="store_true")

    verify_parser = subparsers.add_parser("verify")
    verify_parser.add_argument("backup", type=Path)
    verify_parser.add_argument("--dry-run", action="store_true")

    test_parser = subparsers.add_parser("self-test")
    test_parser.add_argument("--dry-run", action="store_true")

    args = parser.parse_args()
    if args.command == "backup":
        backup(args.source, args.destination, args.dry_run)
    elif args.command == "verify":
        verify(args.backup, args.dry_run)
    else:
        self_test(args.dry_run)
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, ValueError) as error:
        print(f"backup/restore error: {error}", file=sys.stderr)
        raise SystemExit(1)
