#!/usr/bin/env python3
"""Single-root AGS process owner: fixed paths, startup checks and bounded logs.

This supervisor never installs software, migrates data, rotates trust, or restarts
on a health failure. The service manager owns restart policy. No deployment slots.
"""
from __future__ import annotations
import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import selectors
import shlex
import signal
import subprocess
import sys
import time


def environment(path: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        if line.startswith('export '):
            line = line[7:]
        key, separator, raw = line.partition('=')
        if not separator or not key or not key.isascii() or not key.replace('_', 'a').isalnum() or key[0].isdigit():
            raise ValueError('invalid runtime environment entry')
        words = shlex.split(raw, comments=True, posix=True)
        if len(words) > 1 or key in result:
            raise ValueError('ambiguous runtime environment entry')
        result[key] = words[0] if words else ''
    return result


def atomic_json(path: Path, value: dict) -> None:
    temporary = path.with_name(path.name + '.new')
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(value, output, indent=2)
        output.write('\n')
        output.flush()
        os.fsync(output.fileno())
    os.replace(temporary, path)


def private_file(path: Path) -> bool:
    return (not path.is_symlink() and path.is_file()
            and path.stat().st_uid == os.getuid() and path.stat().st_mode & 0o077 == 0)


def durable_path(root: Path, path: Path) -> bool:
    try:
        relative = path.resolve().relative_to(root)
        return (not path.is_symlink() and path.absolute() == path.resolve()
                and relative.parts[0] in ('bin', 'config', 'data', 'state', 'logs', 'cache'))
    except (ValueError, IndexError):
        return False


def retention_limits(root: Path) -> dict:
    path = root / 'config/retention.json'
    if not path.exists() and not path.is_symlink():
        return {'log_segment_bytes': 10 * 1024 * 1024, 'log_backups': 4}
    if not private_file(path) or path.stat().st_size > 65536:
        raise ValueError('unsafe retention configuration')
    value = json.loads(path.read_text())
    if not isinstance(value, dict):
        raise ValueError('invalid retention configuration')
    maximum = value.get('log_segment_bytes', 10 * 1024 * 1024)
    backups = value.get('log_backups', 4)
    if (type(maximum) is not int or not 65536 <= maximum <= 64 * 1024 * 1024
            or type(backups) is not int or not 0 <= backups <= 8):
        raise ValueError('invalid retention limits')
    return {'log_segment_bytes': maximum, 'log_backups': backups}


def inspect(root: Path) -> dict:
    checks: dict[str, str] = {}
    root_ok = root.is_dir() and not root.is_symlink() and root.resolve() == root.absolute()
    checks['root'] = 'ok' if root_ok and root.stat().st_uid == os.getuid() and root.stat().st_mode & 0o077 == 0 else 'unsafe'
    marker = root / 'state/installing.json'
    checks['installation'] = 'interrupted' if marker.exists() or marker.is_symlink() else 'ok'
    for name in ('bin', 'config', 'state', 'logs'):
        directory = root / name
        checks[name + '_directory'] = 'ok' if directory.is_dir() and not directory.is_symlink() and directory.resolve() == directory.absolute() else 'missing_or_unsafe'
    try:
        retention_limits(root)
        checks['retention_config'] = 'ok'
    except (OSError, ValueError, TypeError):
        checks['retention_config'] = 'invalid_or_unsafe'
    env_file = root / 'config/runtime.env'
    checks['runtime_environment'] = 'ok' if private_file(env_file) else 'missing_or_unsafe'
    env: dict[str, str] = {}
    if checks['runtime_environment'] == 'ok':
        try:
            env = environment(env_file)
        except (ValueError, OSError):
            checks['runtime_environment'] = 'invalid'
    binary = root / 'bin/gh-server'
    checks['binary'] = 'ok' if binary.is_file() and not binary.is_symlink() and os.access(binary, os.X_OK) else 'missing_or_unsafe'
    try:
        receipt_path = root / 'state/installation.json'
        receipt = json.loads(receipt_path.read_text()) if private_file(receipt_path) else {}
        expected = receipt['binaries']['gh-server']['sha256']
        if receipt.get('schema') != 'ags.installation.v2' or hashlib.sha256(binary.read_bytes()).hexdigest() != expected:
            checks['binary'] = 'identity_drift'
    except (OSError, ValueError, KeyError, TypeError):
        checks['binary'] = 'identity_unverified'
    for key in ('AGS_INTEGRATIONS_CONFIG', 'GIT_REPO_DIR'):
        value = env.get(key)
        if value:
            path = Path(value)
            checks[key.lower()] = 'ok' if durable_path(root, path) and path.exists() else 'outside_root_or_missing'
    dsn = env.get('DB_DSN', '')
    if dsn.startswith('sqlite:'):
        # Both forms are supported by the native server. sqlite:/absolute/path
        # is used by existing Linux deployments; do not silently omit its check.
        prefix = 'sqlite://' if dsn.startswith('sqlite://') else 'sqlite:'
        path = Path(dsn[len(prefix):].split('?', 1)[0])
        if not path.is_absolute():
            path = root / path
        checks['database_path'] = 'ok' if durable_path(root, path) and path.is_file() else 'outside_root_or_missing'
    value = env.get('AGS_REPLICATION_CONFIG_FILE')
    if value:
        config_path = Path(value)
        checks['replication_config'] = 'ok' if durable_path(root, config_path) and private_file(config_path) else 'missing_or_unsafe'
        if checks['replication_config'] == 'ok':
            try:
                config = json.loads(config_path.read_text())
                for key in ('ca_file', 'certificate_file', 'private_key_file', 'snapshot_root'):
                    path = Path(config[key])
                    if not path.is_absolute():
                        path = config_path.parent / path
                    valid = durable_path(root, path) and path.exists()
                    if key == 'private_key_file':
                        valid = valid and private_file(path)
                    checks['replication_' + key] = 'ok' if valid else 'missing_or_unsafe'
                cert = Path(config['certificate_file'])
                if not cert.is_absolute():
                    cert = config_path.parent / cert
                result = subprocess.run(['openssl', 'x509', '-in', str(cert), '-noout', '-checkend', '0'], capture_output=True, timeout=5)
                checks['replication_certificate_valid'] = 'ok' if result.returncode == 0 else 'expired_or_invalid'
            except (OSError, ValueError, KeyError, TypeError, subprocess.TimeoutExpired):
                checks['replication_config'] = 'invalid'
    report = {'schema': 'ags.startup-health.v1', 'checked_at': dt.datetime.now(dt.timezone.utc).isoformat(),
              'ready_to_start': all(value == 'ok' for value in checks.values()), 'checks': checks}
    return report


class BoundedLog:
    """One current file plus a fixed number of segments; byte-bounded writes."""
    def __init__(self, directory: Path, maximum: int, backups: int):
        if (type(maximum) is not int or not 65536 <= maximum <= 64 * 1024 * 1024
                or type(backups) is not int or not 0 <= backups <= 8):
            raise ValueError('log limits out of range')
        self.path = directory / 'runtime.log'
        self.maximum, self.backups = maximum, backups
        self.fd: int | None = None
        self._open()

    def _open(self) -> None:
        self.fd = os.open(self.path, os.O_WRONLY | os.O_CREAT | os.O_APPEND | os.O_NOFOLLOW, 0o600)
        self.size = os.fstat(self.fd).st_size

    def _rotate(self) -> None:
        os.close(self.fd)
        self.fd = None
        for index in range(self.backups, 0, -1):
            src = self.path if index == 1 else self.path.with_name(f'runtime.log.{index - 1}')
            dst = self.path.with_name(f'runtime.log.{index}')
            if src.exists() or src.is_symlink():
                if src.is_symlink() or dst.is_symlink():
                    raise ValueError('unsafe log segment')
                os.replace(src, dst)
        if self.backups == 0:
            fd = os.open(self.path, os.O_WRONLY | os.O_TRUNC | os.O_NOFOLLOW)
            os.close(fd)
        self._open()

    def write(self, data: bytes) -> None:
        while data:
            if self.size >= self.maximum:
                self._rotate()
            chunk = data[:self.maximum - self.size]
            count = os.write(self.fd, chunk)
            self.size += count
            data = data[count:]

    def close(self) -> None:
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None


def supervise(root: Path, env: dict, build: dict, log: BoundedLog, install_lock) -> int:
    child = subprocess.Popen([str(root / 'bin/gh-server')], cwd=root, env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
    selector = None
    previous = {}
    stop_deadline = None

    def forward(signum, frame=None):
        nonlocal stop_deadline
        if signum in (signal.SIGTERM, signal.SIGINT) and stop_deadline is None:
            stop_deadline = time.monotonic() + 80
        try:
            os.killpg(child.pid, signum)
        except ProcessLookupError:
            pass

    # Every operation after spawn is inside this scope, including receipt writes.
    # A bad state directory must not leave an unowned primary process behind.
    try:
        for sig in (signal.SIGTERM, signal.SIGINT):
            previous[sig] = signal.signal(sig, forward)
        current = {'schema': 'ags.runtime.v1', 'supervisor_pid': os.getpid(), 'pid': child.pid,
                   'started_at': dt.datetime.now(dt.timezone.utc).isoformat(), 'root': str(root), 'build': build}
        atomic_json(root / 'state/runtime.json', current)
        install_lock.close()
        selector = selectors.DefaultSelector()
        selector.register(child.stdout, selectors.EVENT_READ)
        next_check = time.monotonic() + 60
        last_checks = None
        while selector.get_map():
            if stop_deadline is not None and time.monotonic() >= stop_deadline:
                forward(signal.SIGKILL)
            for key, _ in selector.select(1):
                data = os.read(key.fileobj.fileno(), 65536)
                if data:
                    log.write(data)
                else:
                    selector.unregister(key.fileobj)
            if time.monotonic() >= next_check:
                report = inspect(root)
                atomic_json(root / 'state/startup-health.json', report)
                if not report['ready_to_start'] and report['checks'] != last_checks:
                    log.write((json.dumps(report) + '\n').encode())
                elif last_checks is not None and report['ready_to_start']:
                    log.write((json.dumps(report) + '\n').encode())
                last_checks = report['checks'] if not report['ready_to_start'] else None
                next_check = time.monotonic() + 60
        code = child.wait()
        current.update(exit_code=code, stopped_at=dt.datetime.now(dt.timezone.utc).isoformat())
        atomic_json(root / 'state/runtime.json', current)
        return code
    finally:
        if selector is not None:
            selector.close()
        if child.poll() is None:
            forward(signal.SIGTERM)
            try:
                child.wait(timeout=80)
            except subprocess.TimeoutExpired:
                forward(signal.SIGKILL)
                child.wait()
        child.stdout.close()
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def serve(root: Path) -> int:
    os.umask(0o077)
    if (not root.is_dir() or root.is_symlink() or root.resolve() != root.absolute()
            or root.stat().st_uid != os.getuid() or root.stat().st_mode & 0o077):
        raise ValueError('runtime root must be an existing private canonical directory')
    for name in ('state', 'logs'):
        path = root / name
        if path.is_symlink():
            raise ValueError('unsafe managed directory')
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    lockfd = os.open(root / 'state/runtime.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
    with os.fdopen(lockfd, 'w') as lock, contextlib.ExitStack() as resources:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        try:
            limits = retention_limits(root)
        except (OSError, ValueError, TypeError):
            # Invalid limits fail startup; defaults are used only to record that failure.
            limits = {'log_segment_bytes': 10 * 1024 * 1024, 'log_backups': 4}
        log = BoundedLog(root / 'logs', limits['log_segment_bytes'], limits['log_backups'])
        resources.callback(log.close)
        stage = 'startup_dependencies'
        try:
            install_lockfd = os.open(root / 'state/install.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW, 0o600)
            install_lock = resources.enter_context(os.fdopen(install_lockfd, 'w'))
            fcntl.flock(install_lock, fcntl.LOCK_SH | fcntl.LOCK_NB)
            report = inspect(root)
            atomic_json(root / 'state/startup-health.json', report)
            if not report['ready_to_start']:
                log.write((json.dumps(report) + '\n').encode())
                return 78
            env = os.environ.copy()
            env.update(environment(root / 'config/runtime.env'))
            env['AGS_HOME'] = str(root)
            env['PATH'] = str(root / 'bin') + ':' + env.get('PATH', '')
            stage = 'program_identity'
            result = subprocess.run([str(root / 'bin/gh-server'), '--version'], env=env,
                                    capture_output=True, check=True, timeout=10)
            build = json.loads(result.stdout)
            receipt = json.loads((root / 'state/installation.json').read_text())
            if build != receipt['binaries']['gh-server']['identity']:
                raise ValueError('program identity mismatch')
            stage = 'process_lifecycle'
            return supervise(root, env, build, log, install_lock)
        except Exception as error:
            # Do not serialize exception text: subprocess output/config can be sensitive.
            failure = {'schema': 'ags.runtime-failure.v1', 'stage': stage,
                       'error_type': type(error).__name__, 'at': dt.datetime.now(dt.timezone.utc).isoformat()}
            try:
                atomic_json(root / 'state/runtime-failure.json', failure)
            except OSError:
                pass
            log.write((json.dumps(failure) + '\n').encode())
            return 78


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, default=Path(os.environ.get('AGS_HOME', str(Path.home() / '.ags'))))
    parser.add_argument('command', choices=('doctor', 'serve'))
    args = parser.parse_args()
    root = args.root.expanduser().absolute()
    if args.command == 'doctor':
        report = inspect(root)
        print(json.dumps(report, indent=2))
        return 0 if report['ready_to_start'] else 1
    return serve(root)


if __name__ == '__main__':
    raise SystemExit(main())
