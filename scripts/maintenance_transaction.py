#!/usr/bin/env python3
"""Isolated maintenance transaction.

The checked-in manager is a directory of JSON controls. This module never
calls launchctl or systemctl. A root that is the real home ~/.ags, its child,
or its ancestor is refused before any write.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import sys

PHASES = (
    "identity",
    "suppressed",
    "stopped",
    "backed_up",
    "restore_verified",
    "installed",
    "start_attempted",
    "healthy",
    "finalized",
)


class Failure(Exception):
    def __init__(self, code: str, exit_code: int) -> None:
        super().__init__(code)
        self.code = code
        self.exit_code = exit_code


def atomic_write(path: Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_name(path.name + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        os.write(fd, text.encode("utf-8"))
        os.fsync(fd)
    finally:
        os.close(fd)
    os.replace(temporary, path)
    directory = os.open(path.parent, os.O_RDONLY)
    try:
        os.fsync(directory)
    finally:
        os.close(directory)


def refuse_root(root: Path) -> None:
    if root.is_symlink():
        raise Failure("symlink_root_refused", 78)
    production = (Path.home() / ".ags").resolve(strict=False)
    candidate = root.resolve(strict=False)
    if candidate == production or production in candidate.parents or candidate in production.parents:
        raise Failure("production_root_refused", 78)


def parse(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(prog="ags-maintenance-transaction")
    sub = parser.add_subparsers(dest="command", required=True)

    def add_common(command: argparse.ArgumentParser) -> None:
        command.add_argument("--host", required=True)
        command.add_argument("--root", required=True)
        command.add_argument("--candidate", required=True)
        command.add_argument("--window", required=True)
        command.add_argument("--restore", required=True)
        command.add_argument("--manager-dir", required=True)
        command.add_argument("--audit", required=True)

    plan = sub.add_parser("plan")
    add_common(plan)
    run = sub.add_parser("run")
    add_common(run)
    run.add_argument("--execute", action="store_true")
    run.add_argument("--fault-after", default="")
    recover = sub.add_parser("recover-prestart")
    add_common(recover)
    recover.add_argument("--execute", action="store_true")
    status = sub.add_parser("status")
    status.add_argument("--root", required=True)
    status.add_argument("--transaction-id", required=True)
    return parser.parse_args(argv)


def read_json(path: Path) -> dict:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise Failure("unreadable_input", 78) from error
    if not isinstance(value, dict):
        raise Failure("unreadable_input", 78)
    return value


def outside(root: Path, path: Path, code: str) -> Path:
    resolved = path.resolve(strict=False)
    if resolved == root or root in resolved.parents:
        raise Failure(code, 78)
    return resolved


def file_digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def copy_file(src: Path, dst: Path) -> None:
    dst.parent.mkdir(parents=True, exist_ok=True)
    data = src.read_bytes()
    fd = os.open(dst, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, src.stat().st_mode & 0o777)
    try:
        os.write(fd, data)
        os.fsync(fd)
    finally:
        os.close(fd)


def copy_tree(src: Path, dst: Path) -> None:
    if dst.exists():
        raise Failure("backup_exists", 78)
    for path in sorted(src.rglob("*")):
        target = dst / path.relative_to(src)
        if path.is_dir():
            target.mkdir(parents=True, exist_ok=True)
            continue
        if path.is_file():
            copy_file(path, target)


def tree_rows(path: Path) -> list[tuple[str, str, int]]:
    rows = []
    for file in sorted(item for item in path.rglob("*") if item.is_file()):
        rows.append((str(file.relative_to(path)), file_digest(file), file.stat().st_mode & 0o777))
    return rows


def manager_call(directory: Path, name: str) -> None:
    control = read_json(directory / "control.json")
    if control.get(name) != "ok":
        raise Failure("manager_failed", 81)
    if name == "stop" and control.get("writers") != 0:
        raise Failure("writers_remain", 81)
    calls_path = directory / "calls.json"
    calls = read_json(calls_path) if calls_path.exists() else {}
    calls[name] = int(calls.get(name, 0)) + 1
    atomic_write(calls_path, json.dumps(calls))


def journal_path(root: Path) -> Path:
    return root / "state" / "journal.json"


def load_journal(root: Path) -> dict | None:
    path = journal_path(root)
    if not path.exists():
        return None
    return read_json(path)


def save_journal(root: Path, journal: dict) -> None:
    atomic_write(journal_path(root), json.dumps(journal))


def write_fence(root: Path) -> None:
    atomic_write(root / "state" / "fence", "stopped\n")


def request_paths(args: argparse.Namespace) -> tuple[Path, Path, dict, dict]:
    root = Path(args.root)
    if not root.is_dir():
        raise Failure("root_missing", 78)
    restore = outside(root.resolve(), Path(args.restore), "restore_inside_root")
    outside(root.resolve(), Path(args.audit), "audit_inside_root")
    manifest = read_json(Path(args.candidate))
    window = read_json(Path(args.window))
    if args.host not in {"mini", "imile-win"} or window.get("host") != args.host:
        raise Failure("host_rejected", 78)
    if window.get("schema") != "ags.routeb-window.v1" or window.get("decision") != "approve":
        raise Failure("window_rejected", 78)
    if manifest.get("schema") != "ags.routeb-candidate.v1":
        raise Failure("candidate_rejected", 78)
    binary = Path(str(manifest.get("binary", "")))
    digest = str(manifest.get("sha256", ""))
    if not binary.is_file() or file_digest(binary) != digest or window.get("candidate_sha256") != digest:
        raise Failure("candidate_rejected", 78)
    transaction = str(window.get("transaction_id", ""))
    if not transaction:
        raise Failure("window_rejected", 78)
    return root, restore, manifest, window


def mark(root: Path, journal: dict, phase: str, fault: str) -> None:
    if phase not in journal["phases"]:
        journal["phases"].append(phase)
        save_journal(root, journal)
    if fault == phase:
        raise Failure("fault_injected", 81)


def run(args: argparse.Namespace) -> int:
    if not args.execute:
        raise Failure("execute_required", 78)
    if args.fault_after and os.environ.get("AGS_TXN_TEST") != "1":
        raise Failure("fault_hook_refused", 78)
    root, restore, manifest, window = request_paths(args)
    root.joinpath("state").mkdir(parents=True, exist_ok=True)
    lock_fd = os.open(root / "state" / "lock", os.O_CREAT | os.O_RDWR, 0o600)
    try:
        import fcntl

        fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as error:
        os.close(lock_fd)
        raise Failure("lock_held", 78) from error
    try:
        return execute(args, root, restore, manifest, window)
    finally:
        os.close(lock_fd)


def execute(args: argparse.Namespace, root: Path, restore: Path, manifest: dict, window: dict) -> int:
    fault = args.fault_after
    digest = str(manifest["sha256"])
    transaction = str(window["transaction_id"])
    journal = load_journal(root)
    if journal and "finalized" in journal.get("phases", []):
        if journal.get("candidate_sha256") != digest or journal.get("transaction_id") != transaction:
            raise Failure("transaction_mismatch", 78)
        return 0
    if journal and (journal.get("transaction_id") != transaction or journal.get("candidate_sha256") != digest):
        raise Failure("transaction_mismatch", 78)
    if (root / "state" / "fence").exists() and journal is None:
        raise Failure("fence_present", 78)
    if journal is None:
        journal = {
            "schema": "ags.routeb-journal.v1",
            "transaction_id": transaction,
            "candidate_sha256": digest,
            "phases": [],
        }
        save_journal(root, journal)
    done = set(journal["phases"])
    if "identity" not in done:
        mark(root, journal, "identity", fault)
    manager = Path(args.manager_dir)
    if "suppressed" not in done:
        manager_call(manager, "suppress")
        mark(root, journal, "suppressed", fault)
    if "stopped" not in done:
        manager_call(manager, "stop")
        mark(root, journal, "stopped", fault)
    backup = root / "state" / "backup" / transaction
    if "backed_up" not in done:
        copy_tree(root / "data", backup / "data")
        copy_file(root / "bin" / "old", backup / "old")
        mark(root, journal, "backed_up", fault)
    if "restore_verified" not in done:
        copy_tree(backup / "data", restore / "data")
        copy_file(backup / "old", restore / "old")
        if fault == "corrupt-restore":
            (restore / "data" / "row.txt").write_text("corrupt\n", encoding="utf-8")
        if tree_rows(backup / "data") != tree_rows(restore / "data") or file_digest(backup / "old") != file_digest(restore / "old"):
            raise Failure("restore_mismatch", 81)
        mark(root, journal, "restore_verified", fault)
    if "installed" not in done:
        copy_file(Path(str(manifest["binary"])), root / "bin" / "current")
        mark(root, journal, "installed", fault)
    if "start_attempted" not in done:
        mark(root, journal, "start_attempted", fault)
        manager_call(manager, "start")
    if "healthy" not in done:
        mark(root, journal, "healthy", fault)
    if "finalized" not in done:
        audit = Path(args.audit)
        audit.mkdir(parents=True, exist_ok=True)
        atomic_write(audit / f"fence-{transaction}", "audited\n")
        fence = root / "state" / "fence"
        if fence.exists():
            fence.unlink()
        mark(root, journal, "finalized", fault)
    return 0


def plan(args: argparse.Namespace) -> int:
    request_paths(args)
    return 0


def recover(args: argparse.Namespace) -> int:
    if not args.execute:
        raise Failure("execute_required", 78)
    root, _restore, _manifest, window = request_paths(args)
    journal = load_journal(root)
    if journal is None or journal.get("transaction_id") != window.get("transaction_id"):
        raise Failure("journal_missing", 78)
    phases = set(journal.get("phases", []))
    if phases.intersection({"start_attempted", "healthy", "finalized"}):
        raise Failure("prestart_restore_refused", 78)
    backup = root / "state" / "backup" / str(window["transaction_id"]) / "old"
    if not backup.is_file():
        raise Failure("backup_missing", 81)
    copy_file(backup, root / "bin" / "current")
    journal["phases"].append("prestart_restored")
    save_journal(root, journal)
    return 0


def status(args: argparse.Namespace) -> int:
    root = Path(args.root)
    journal = load_journal(root)
    if journal is None or journal.get("transaction_id") != args.transaction_id:
        raise Failure("journal_missing", 78)
    sys.stdout.write(json.dumps({"phases": journal["phases"]}) + "\n")
    return 0


def main(argv: list[str]) -> int:
    args = parse(argv)
    try:
        refuse_root(Path(args.root))
        if args.command == "plan":
            return plan(args)
        if args.command == "run":
            return run(args)
        if args.command == "recover-prestart":
            return recover(args)
        if args.command == "status":
            return status(args)
        raise Failure("unknown_command", 78)
    except Failure as error:
        if error.exit_code == 81:
            root = Path(args.root)
            try:
                refuse_root(root)
            except Failure:
                print(f"code={error.code}", file=sys.stderr)
                return error.exit_code
            try:
                write_fence(root)
            except OSError:
                pass
        print(f"code={error.code}", file=sys.stderr)
        return error.exit_code


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
