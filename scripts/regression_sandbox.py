#!/usr/bin/env python3
"""Owned synthetic regression state. Retain uncertainty; never permanently delete."""
from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid

OWNER = '.ags-regression-owner.json'
RECEIPT = 'test-resource-exit.json'


@dataclass(frozen=True)
class Sandbox:
    path: Path
    identity: str
    owner_pid: int


def create(parent: Path | None, owner_pid: int) -> Sandbox:
    path = Path(tempfile.mkdtemp(prefix='ags-upgrade-regression-', dir=parent)).resolve()
    path.chmod(0o700)
    info = path.lstat()
    owner = {'schema': 'ags.regression-owner.v1', 'path': str(path), 'dev': info.st_dev,
             'ino': info.st_ino, 'uid': info.st_uid, 'pid': owner_pid, 'id': uuid.uuid4().hex}
    encoded = json.dumps(owner, sort_keys=True).encode()
    with (path / OWNER).open('xb') as target:
        target.write(encoded)
    (path / OWNER).chmod(0o600)
    return Sandbox(path, hashlib.sha256(encoded).hexdigest(), owner_pid)


def verified(sandbox: Sandbox) -> bool:
    path = sandbox.path
    if not path.is_absolute() or path == Path('/') or path.resolve() != path:
        return False
    info = path.lstat()
    marker = (path / OWNER).lstat()
    if (not stat.S_ISDIR(info.st_mode) or not stat.S_ISREG(marker.st_mode)
            or info.st_uid != os.getuid() or marker.st_uid != info.st_uid
            or marker.st_mode & 0o077 or marker.st_size > 4096):
        return False
    encoded = (path / OWNER).read_bytes()
    owner = json.loads(encoded)
    return (hashlib.sha256(encoded).hexdigest() == sandbox.identity
            and owner.get('schema') == 'ags.regression-owner.v1'
            and owner.get('path') == str(path) and owner.get('pid') == sandbox.owner_pid
            and owner.get('dev') == info.st_dev and owner.get('ino') == info.st_ino
            and owner.get('uid') == info.st_uid)


def inventory(root: Path) -> dict:
    entries, pending = [], [Path('.')]
    truncated = False
    while pending and len(entries) < 128:
        relative = pending.pop(0)
        with os.scandir(root / relative) as children:
            for child in children:
                if len(entries) == 128:
                    truncated = True
                    break
                path = relative / child.name
                info = child.stat(follow_symlinks=False)
                kind = 'symlink' if child.is_symlink() else 'directory' if child.is_dir(follow_symlinks=False) else 'file'
                entries.append({'path': str(path)[:256], 'kind': kind, 'bytes': info.st_size if kind == 'file' else 0})
                if kind == 'directory' and len(path.parts) < 4:
                    pending.append(path)
                elif kind == 'directory':
                    truncated = True
    return {'limit': 128, 'entries': entries, 'truncated': truncated or bool(pending)}


def system_trash(path: Path) -> str:
    if sys.platform != 'darwin':
        return 'retained_trash_unavailable'
    tool = Path('/usr/bin/trash').lstat()
    if not stat.S_ISREG(tool.st_mode) or tool.st_uid != 0 or tool.st_mode & 0o022:
        return 'retained_trash_unavailable'
    result = subprocess.run(['/usr/bin/trash', str(path)], stdout=subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, timeout=5, check=False)
    return 'trashed' if result.returncode == 0 and not os.path.lexists(path) else 'trash_unverified'


def write_evidence(path: Path, value: dict) -> None:
    with path.open('x') as target:
        target.write(json.dumps(value))
    path.chmod(0o600)


def finish(sandbox: Sandbox, code: int, move=system_trash) -> dict:
    result = {'event': 'ags_test_sandbox', 'root': str(sandbox.path), 'identity': sandbox.identity, 'exit_code': code}
    try:
        if not verified(sandbox):
            return {**result, 'cleanup': 'owner_unverified'}
        write_evidence(sandbox.path / RECEIPT,
                       {**result, 'cleanup': 'retained_failed' if code else 'trash_pending',
                        **({'inventory': inventory(sandbox.path)} if code else {})})
    except (OSError, ValueError, TypeError, AttributeError):
        return {**result, 'cleanup': 'evidence_unverified'}
    if code:
        return {**result, 'cleanup': 'retained_failed'}
    try:
        if not verified(sandbox):
            return {**result, 'cleanup': 'owner_unverified'}
        cleanup = move(sandbox.path)
        if cleanup == 'trashed' and os.path.lexists(sandbox.path):
            cleanup = 'trash_unverified'
    except (OSError, ValueError, subprocess.SubprocessError):
        cleanup = 'trash_unverified'
    result['cleanup'] = cleanup
    if cleanup != 'trashed':
        try:
            if verified(sandbox):
                write_evidence(sandbox.path / 'test-resource-cleanup-result.json', result)
        except (OSError, ValueError, TypeError, AttributeError):
            pass  # The pre-effect intent remains; do not overwrite uncertain evidence.
    return result


def group_present(pid: int) -> bool:
    try:
        os.killpg(pid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def settle_group(pid: int) -> bool:
    for selected in (signal.SIGTERM, signal.SIGKILL):
        if not group_present(pid):
            return True
        try:
            os.killpg(pid, selected)
        except ProcessLookupError:
            return True
        deadline = time.monotonic() + 0.5
        while time.monotonic() < deadline:
            if not group_present(pid):
                return True
            time.sleep(0.01)
    return not group_present(pid)


def run_owned(command: list[str]) -> int:
    child = subprocess.Popen(command, start_new_session=True)
    interrupted = []
    previous = {}
    def interrupt(selected, _frame):
        if not interrupted:
            interrupted.append(selected)
        try:
            os.killpg(child.pid, selected)
        except ProcessLookupError:
            pass
    for selected in (signal.SIGINT, signal.SIGTERM):
        previous[selected] = signal.signal(selected, interrupt)
    try:
        while True:
            try:
                code = child.wait(timeout=0.1)
                break
            except subprocess.TimeoutExpired:
                if interrupted:
                    settle_group(child.pid)
                    child.wait(timeout=1)
                    return 128 + interrupted[0]
        lingering = group_present(child.pid)
        settled = settle_group(child.pid) if lingering else True
        if interrupted:
            return 128 + interrupted[0]
        if lingering or not settled:
            print('regression_process_group_unsettled', file=sys.stderr)
            return 125
        return code if code >= 0 else 128 - code
    finally:
        for selected, handler in previous.items():
            signal.signal(selected, handler)


def main() -> int:
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest='command', required=True)
    start = commands.add_parser('create')
    start.add_argument('--owner-pid', type=int, required=True)
    end = commands.add_parser('finish')
    end.add_argument('--root', type=Path, required=True)
    end.add_argument('--identity', required=True)
    end.add_argument('--owner-pid', type=int, required=True)
    end.add_argument('--exit-code', type=int, required=True)
    run = commands.add_parser('run')
    run.add_argument('argv', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    if args.command == 'create':
        sandbox = create(None, args.owner_pid)
        print(f'{sandbox.path}\t{sandbox.identity}')
    elif args.command == 'finish':
        print(json.dumps(finish(Sandbox(args.root, args.identity, args.owner_pid), args.exit_code)))
    else:
        command = args.argv[1:] if args.argv[:1] == ['--'] else args.argv
        if not command:
            parser.error('run requires a command')
        return run_owned(command)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
