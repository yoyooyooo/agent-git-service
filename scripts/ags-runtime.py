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
import re
import selectors
import shlex
import signal
import subprocess
import sys
import time
import uuid


def cold_pending(root):
    return any(p.exists() or p.is_symlink() for p in
               (root / 'state/upgrade-cold.json', root / 'state/ags-autodeploy/local-v2/pending'))


def startup_permitted(root, *, native_context=None):
    if native_context is not None:
        try:
            native = _native_context(root, native_context)
            if native['phase'] == 'serve-old':
                from routeb_native_recovery import old_start_binding
                old_start_binding(root, native)
                return True
        except Exception:
            return False
    control = root / 'state/ags-autodeploy/local-v2/control'
    events = control / 'events'
    published = events.exists() and not events.is_symlink() and any(
        path for path in events.iterdir() if not path.name.startswith('.'))
    # Only the durable control journal fences startup. A cold-window claim is
    # handled below and is not itself an authority projection.
    journal_present = ((control / 'format.json').is_file() and not (control / 'format.json').is_symlink()) or published
    if journal_present:
        format_path = control / 'format.json'
        if not format_path.is_file() or format_path.is_symlink():
            return False
        try:
            import upgrade_state
            loaded = upgrade_state.load_verified(root)
            projection = loaded.get('projection') or {}
            authority = projection.get('authority')
            # A journal with no authority projection is fenced. Absence is not permission.
            if authority is None or authority.get('state') != 'active':
                return False
            if projection.get('pending_effects'):
                return False
        except Exception:
            return False
    if not cold_pending(root):
        return True
    try:
        from upgrade_identity import runtime_identity
        runtime_identity(root)
        from upgrade_recovery import startup_permitted as permitted
        return permitted(root)
    except (ImportError, OSError, ValueError):
        return False


def consume_start(root, *, native_context=None):
    if native_context is not None:
        native = _native_context(root, native_context)
        if native['phase'] == 'serve-old':
            from routeb_native_recovery import consume_old_generation
            return consume_old_generation(root, native)
    if cold_pending(root):
        from upgrade_identity import runtime_identity
        runtime_identity(root)
    if not startup_permitted(root):
        raise CheckFailure('start_refused')
    if not cold_pending(root):
        return None
    from upgrade_recovery import consume_start as consume
    return consume(root)


def running_permitted(root, generation, child_pid, *, native_context=None):
    """A consumed handoff permits this living generation, never another spawn."""
    if native_context is not None:
        try:
            native = _native_context(root, native_context)
            if native['phase'] == 'serve-old':
                from routeb_native_recovery import old_generation_running
                return old_generation_running(root, generation, child_pid, native)
        except Exception:
            return False
    if not cold_pending(root):
        return startup_permitted(root)
    try:
        from upgrade_identity import runtime_identity
        from upgrade import private_json
        from upgrade_inventory import file_digest
        from upgrade_recovery import read_window
        import upgrade_state
        identity = runtime_identity(root)
        loaded = upgrade_state.load_verified(root)
        projection = loaded.get('projection') or {}
        authority = projection.get('authority') or {}
        marker, journal, _ = read_window(root)
        handoff = journal.get('handoff') or {}
        runtime, _ = private_json(root / 'state/runtime.json')
        if not (isinstance(generation, str) and generation
                and type(child_pid) is int and child_pid > 0
                and loaded.get('format', {}).get('host_instance') == identity.host_instance
                and authority.get('state') == 'active' and not projection.get('pending_effects')
                and authority.get('epoch') == projection.get('epoch') == 1
                and projection.get('generation') == generation
                and marker['generation'] == journal['generation'] == generation
                and journal['backup_binding']['host_instance'] == identity.host_instance
                and journal['phase'] == 'start_attempted' and journal['start_attempted'] is True
                and handoff.get('generation') == generation and handoff.get('consumed') is True
                and handoff.get('manager_requested') is True
                and type(handoff.get('token')) is str
                and re.fullmatch(r'[0-9a-f]{32}', handoff['token']) is not None
                and handoff.get('binary_sha256') == journal['backup_binding']['new_binary_sha256']
                and handoff.get('receipt_sha256') == journal.get('installed_receipt_sha256')
                and handoff['binary_sha256'] == file_digest(root / 'bin/gh-server')
                and handoff['receipt_sha256'] == file_digest(root / 'state/installation.json')
                and runtime.get('schema') == 'ags.runtime.v1' and runtime.get('root') == str(root)
                and runtime.get('generation') == generation and 'stopped_at' not in runtime
                and runtime.get('supervisor_pid') == os.getpid() and runtime.get('pid') == child_pid):
            return False
        parent = subprocess.run(['/bin/ps', '-p', str(child_pid), '-o', 'ppid='],
                                capture_output=True, text=True, timeout=5,
                                env={'PATH': '/usr/bin:/bin', 'LC_ALL': 'C'})
        return parent.returncode == 0 and not parent.stderr and parent.stdout.strip() == str(os.getpid())
    except Exception:
        # Unknown ownership or lifecycle facts remain unhealthy. No token is
        # consumed, recreated or refunded by a periodic inspection.
        return False


class CheckFailure(ValueError):
    """Allowlisted diagnostics only; never serialize config values or exception text."""
    def __init__(self, reason: str, **details):
        super().__init__(reason)
        self.details = {'reason': reason, **details}


def environment(path: Path) -> dict[str, str]:
    result: dict[str, str] = {}
    for number, line in enumerate(path.read_text().splitlines(), 1):
        line = line.strip()
        if not line or line.startswith('#'):
            continue
        if line.startswith('export '):
            line = line[7:]
        key, separator, raw = line.partition('=')
        if not separator or not key or not key.isascii() or not key.replace('_', 'a').isalnum() or key[0].isdigit():
            raise CheckFailure('environment_entry_invalid', line=number)
        try:
            words = shlex.split(raw, comments=True, posix=True)
        except ValueError:
            raise CheckFailure('environment_entry_invalid', line=number) from None
        if len(words) > 1 or key in result:
            raise CheckFailure('environment_entry_ambiguous', line=number)
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
    fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


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
        raise CheckFailure('retention_file_unsafe_or_too_large')
    try:
        value = json.loads(path.read_text())
    except ValueError:
        raise CheckFailure('retention_json_invalid') from None
    if not isinstance(value, dict):
        raise CheckFailure('retention_not_object')
    maximum = value.get('log_segment_bytes', 10 * 1024 * 1024)
    backups = value.get('log_backups', 4)
    if type(maximum) is not int or not 65536 <= maximum <= 64 * 1024 * 1024:
        raise CheckFailure('log_segment_bytes_out_of_range', minimum_bytes=65536, maximum_bytes=64 * 1024 * 1024)
    if type(backups) is not int or not 0 <= backups <= 8:
        raise CheckFailure('log_backups_out_of_range', minimum=0, maximum=8)
    return {'log_segment_bytes': maximum, 'log_backups': backups}


def inspect(root: Path, *, running_generation=None, running_pid=None, native_context=None) -> dict:
    checks: dict[str, str] = {}
    details: dict[str, dict] = {}
    root_ok = root.is_dir() and not root.is_symlink() and root.resolve() == root.absolute()
    checks['root'] = 'ok' if root_ok and root.stat().st_uid == os.getuid() and root.stat().st_mode & 0o077 == 0 else 'unsafe'
    marker = root / 'state/installing.json'
    checks['installation'] = 'interrupted' if marker.exists() or marker.is_symlink() else 'ok'
    native_options = {'native_context': native_context} if native_context is not None else {}
    permitted = (startup_permitted(root, **native_options) if running_generation is None and running_pid is None else
                 running_permitted(root, running_generation, running_pid, **native_options))
    checks['upgrade_cold_window'] = 'ok' if permitted else 'blocked'
    for name in ('bin', 'config', 'state', 'logs'):
        directory = root / name
        checks[name + '_directory'] = 'ok' if directory.is_dir() and not directory.is_symlink() and directory.resolve() == directory.absolute() else 'missing_or_unsafe'
    try:
        retention_limits(root)
        checks['retention_config'] = 'ok'
    except (OSError, ValueError, TypeError) as error:
        checks['retention_config'] = 'invalid_or_unsafe'
        details['retention_config'] = (error.details if isinstance(error, CheckFailure)
                                       else {'reason': 'retention_file_unreadable', 'error_type': type(error).__name__})
    env_file = root / 'config/runtime.env'
    checks['runtime_environment'] = 'ok' if private_file(env_file) else 'missing_or_unsafe'
    env: dict[str, str] = {}
    if checks['runtime_environment'] == 'ok':
        try:
            env = environment(env_file)
        except (ValueError, OSError) as error:
            checks['runtime_environment'] = 'invalid'
            details['runtime_environment'] = (error.details if isinstance(error, CheckFailure)
                                              else {'reason': 'environment_file_unreadable', 'error_type': type(error).__name__})
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
            stage = 'config_read_failed'
            try:
                content = config_path.read_text()
                stage = 'config_json_invalid'
                config = json.loads(content)
                if not isinstance(config, dict):
                    raise CheckFailure('config_not_object')
                paths = {}
                for key in ('ca_file', 'certificate_file', 'private_key_file', 'snapshot_root'):
                    if key not in config:
                        raise CheckFailure('config_field_missing', field=key)
                    if not isinstance(config[key], str) or not config[key] or '\x00' in config[key]:
                        raise CheckFailure('config_field_invalid', field=key)
                    path = Path(config[key])
                    if not path.is_absolute():
                        path = config_path.parent / path
                    paths[key] = path
                    stage = 'replication_dependency_inspection_failed'
                    valid = durable_path(root, path) and path.exists()
                    if key == 'private_key_file':
                        valid = valid and private_file(path)
                    checks['replication_' + key] = 'ok' if valid else 'missing_or_unsafe'
                minimum = config.get('min_free_bytes')
                if type(minimum) is not int or not 0 < minimum <= 1 << 60:
                    raise CheckFailure('config_field_invalid', field='min_free_bytes')
                if checks['replication_snapshot_root'] == 'ok':
                    try:
                        space = os.statvfs(paths['snapshot_root'])
                        available = space.f_bavail * space.f_frsize
                        checks['replication_disk_free'] = 'ok' if available >= minimum else 'invalid'
                        if available < minimum:
                            details['replication_disk_free'] = {'reason': 'disk_free_below_threshold',
                                                               'available_bytes': available, 'minimum_bytes': minimum}
                    except OSError as error:
                        checks['replication_disk_free'] = 'invalid'
                        details['replication_disk_free'] = {'reason': 'disk_free_probe_failed',
                                                           'error_type': type(error).__name__}
                if checks['replication_certificate_file'] == 'ok':
                    try:
                        result = subprocess.run(['openssl', 'x509', '-in', str(paths['certificate_file']),
                                                 '-noout', '-checkend', '0'], capture_output=True, timeout=5)
                        checks['replication_certificate_valid'] = 'ok' if result.returncode == 0 else 'expired_or_invalid'
                        if result.returncode != 0:
                            details['replication_certificate_valid'] = {'reason': 'certificate_expired_or_invalid',
                                                                       'exit_code': result.returncode}
                    except (OSError, subprocess.TimeoutExpired) as error:
                        checks['replication_certificate_valid'] = 'invalid'
                        details['replication_certificate_valid'] = {
                            'reason': 'certificate_probe_timeout' if isinstance(error, subprocess.TimeoutExpired)
                                      else 'certificate_probe_failed', 'error_type': type(error).__name__}
            except (OSError, ValueError, KeyError, TypeError) as error:
                checks['replication_config'] = 'invalid'
                details['replication_config'] = (error.details if isinstance(error, CheckFailure)
                                                 else {'reason': stage, 'error_type': type(error).__name__})
    # Keep v1 string statuses compatible; add credential-free details for every failure.
    for name, status in checks.items():
        if status != 'ok' and name not in details:
            details[name] = {'reason': status}
    report = {'schema': 'ags.startup-health.v1', 'checked_at': dt.datetime.now(dt.timezone.utc).isoformat(),
              'ready_to_start': all(value == 'ok' for value in checks.values()), 'checks': checks, 'check_details': details}
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


def _journal_effect(root, operation, before, after, kind, effect):
    control = root / 'state/ags-autodeploy/local-v2/control/format.json'
    if not control.is_file() or control.is_symlink():
        return
    import upgrade_state
    loaded = upgrade_state.load_verified(root)
    authority = (loaded.get('projection') or {}).get('authority')
    # Before the first activation there is no authority to CAS against.
    # Keep the historical epoch=1 write. After activation, the verified epoch
    # is both the written epoch and the expected epoch.
    if not isinstance(authority, dict) or authority.get('state') != 'active':
        epoch, expected = 1, None
    else:
        epoch = authority.get('epoch')
        if type(epoch) is not int or epoch < 1:
            raise upgrade_state.StateError('state_epoch_conflict')
        expected = epoch
    upgrade_state.commit_phase(root, operation, before, after, kind=kind, effect=effect,
                               epoch=epoch, expected_epoch=expected)


def _native_context(root, callback, previous=None):
    """Read the launcher's live peer binding, never a serialized permission flag."""
    if callback is None:
        return None
    value = callback()
    if (not isinstance(value, dict) or value.get('schema') != 'ags.routeb-stage0-context.v1'
            or value.get('root') != str(root) or value.get('operation') != 'serve'
            or not isinstance(value.get('supervisor'), dict) or not isinstance(value.get('child'), dict)
            or value['supervisor'].get('pid') != os.getppid()
            or value['supervisor'].get('uid') != 0 or value['supervisor'].get('gid') != 0
            or value['child'].get('pid') != os.getpid()
            or value['child'].get('parent_pid') != os.getppid()
            or value['child'].get('uid') != os.getuid() or value['child'].get('gid') != os.getgid()
            or (previous is not None and value != previous)):
        raise ValueError('native runtime context changed')
    # Freeze the snapshot: a callback retaining a mutable dictionary cannot
    # change the saved generation identity in place between later checks.
    return json.loads(json.dumps(value))


class NativeSupervisionFailure(RuntimeError):
    """A failed native stop remains failed even if the child later exits zero."""
    def __init__(self, reason):
        self.reason = reason
        super().__init__(reason)


def _native_failure(root, log, reason, error_type):
    failure = {'schema': 'ags.runtime-failure.v1', 'stage': 'process_lifecycle',
               'reason': reason, 'error_type': error_type,
               'at': dt.datetime.now(dt.timezone.utc).isoformat()}
    raw = (json.dumps(failure) + '\n').encode()
    # Neither a broken state directory nor a failed log may release the pipe
    # or runtime lock while the owned process is still alive.
    try:
        atomic_json(root / 'state/runtime-failure.json', failure)
    except BaseException:
        pass
    try:
        log.write(raw)
    except BaseException:
        pass
    try:
        os.write(2, raw)
    except BaseException:
        pass


def _process_group_empty(pid):
    try:
        os.killpg(pid, 0)
    except ProcessLookupError:
        return True
    except PermissionError:
        return False
    return False


def _retain_native_child(child, log):
    """Keep ownership and the read end until natural exit and observed EOF.

    A failed log becomes a discard sink. An unreadable pipe is unknown, never
    permission to close it or to force a child that might be blocked writing.
    Each iteration performs at most one bounded read and an observation.
    """
    pipe_eof, reader = False, None
    try:
        reader = child.stdout.fileno()
        os.set_blocking(reader, False)
    except (OSError, ValueError):
        reader = None
    while True:
        data = None
        if reader is not None and not pipe_eof:
            try:
                data = os.read(reader, 65536)
            except BlockingIOError:
                pass
            except OSError:
                reader = None
            else:
                if data:
                    try:
                        log.write(data)
                    except Exception:
                        pass
                else:
                    pipe_eof = True
        if child.poll() is not None and pipe_eof and _process_group_empty(child.pid):
            return child.wait()
        if not data:
            time.sleep(.05)


def supervise(root: Path, env: dict, build: dict, log: BoundedLog, install_lock, generation=None,
              *, native_context=None) -> int:
    native = _native_context(root, native_context)
    if cold_pending(root):
        from upgrade_identity import runtime_identity
        runtime_identity(root)
    child = subprocess.Popen([str(root / 'bin/gh-server')], cwd=root, env=env,
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True)
    selector = None
    previous = {}
    stop_deadline = None
    forced_kill = False
    current = None
    native_reaped = False
    native_signal_failed = False

    def forward(signum, frame=None):
        nonlocal stop_deadline, forced_kill, native_signal_failed
        if native is not None and signum == signal.SIGKILL:
            raise NativeSupervisionFailure('native_forced_signal_refused')
        if signum == signal.SIGKILL:
            forced_kill = True
        if signum in (signal.SIGTERM, signal.SIGINT) and stop_deadline is None:
            stop_deadline = time.monotonic() + 80
        if native is not None and child.poll() is not None:
            return  # Never signal a potentially reused PGID after its leader exited.
        try:
            os.killpg(child.pid, signum)
        except ProcessLookupError:
            pass
        except OSError:
            if native is None:
                raise
            native_signal_failed = True

    # Every operation after spawn is inside this scope, including receipt writes.
    # A bad state directory must not leave an unowned primary process behind.
    try:
        for sig in (signal.SIGTERM, signal.SIGINT):
            previous[sig] = signal.signal(sig, forward)
        current = {'schema': 'ags.runtime.v1', 'supervisor_pid': os.getpid(), 'pid': child.pid,
                   'generation': generation or uuid.uuid4().hex,
                   'started_at': dt.datetime.now(dt.timezone.utc).isoformat(), 'root': str(root), 'build': build}
        if native is not None:
            _native_context(root, native_context, native)
            current['stage0'] = native
        atomic_json(root / 'state/runtime.json', current)
        _journal_effect(root, 'supervise-spawn-' + current['generation'], 'active', 'running',
                        'generation_spawned',
                        {'effect_id': current['generation'], 'type': 'spawn', 'expected_before': 'stopped',
                         'desired_after': current['generation'], 'status': 'observed'})
        install_lock.close()
        selector = selectors.DefaultSelector()
        selector.register(child.stdout, selectors.EVENT_READ)
        next_check = time.monotonic() + 60
        last_checks = None
        while selector.get_map() or (native is not None and child.poll() is None):
            if native_signal_failed:
                raise NativeSupervisionFailure('native_signal_failed')
            if stop_deadline is not None and time.monotonic() >= stop_deadline:
                if native is not None:
                    raise NativeSupervisionFailure('native_stop_timeout')
                forward(signal.SIGKILL)
            for key, _ in selector.select(1):
                data = os.read(key.fileobj.fileno(), 65536)
                if data:
                    log.write(data)
                else:
                    selector.unregister(key.fileobj)
            if time.monotonic() >= next_check:
                _native_context(root, native_context, native)
                options = {'native_context': native_context} if native_context is not None else {}
                report = inspect(root, running_generation=current['generation'], running_pid=child.pid, **options)
                atomic_json(root / 'state/startup-health.json', report)
                if not report['ready_to_start'] and (report['checks'], report['check_details']) != last_checks:
                    log.write((json.dumps(report) + '\n').encode())
                elif last_checks is not None and report['ready_to_start']:
                    log.write((json.dumps(report) + '\n').encode())
                last_checks = (report['checks'], report['check_details']) if not report['ready_to_start'] else None
                next_check = time.monotonic() + 60
        code = child.wait()
        try:
            os.killpg(child.pid, 0)
            group_empty = False
        except ProcessLookupError:
            group_empty = True
        if native is not None and not group_empty:
            raise NativeSupervisionFailure('native_process_group_not_empty')
        _native_context(root, native_context, native)
        current.update(process_group_empty=group_empty, exit_code=code, forced_kill=forced_kill,
                       stop_requested=stop_deadline is not None,
                       stopped_at=dt.datetime.now(dt.timezone.utc).isoformat())
        atomic_json(root / 'state/runtime.json', current)
        _journal_effect(root, 'supervise-exit-' + current['generation'], 'running', 'exited',
                        'generation_exited',
                        {'effect_id': current['generation'], 'type': 'exit', 'expected_before': 'running',
                         'desired_after': 'exited', 'status': 'observed'})
        native_reaped = native is not None
        return code
    except BaseException as error:
        if native is None:
            raise
        reason = error.reason if isinstance(error, NativeSupervisionFailure) else 'native_supervision_failed'
        _native_failure(root, log, reason, type(error).__name__)
        forward(signal.SIGTERM)
        code = _retain_native_child(child, log)
        native_reaped = True
        if current is not None:
            current.update(exit_code=code, forced_kill=False, supervision_failed=True,
                           stop_timed_out=reason == 'native_stop_timeout', supervisor_exit_code=78,
                           stop_requested=stop_deadline is not None, process_group_empty=True,
                           stopped_at=dt.datetime.now(dt.timezone.utc).isoformat())
            try:
                atomic_json(root / 'state/runtime.json', current)
            except Exception:
                pass
        raise NativeSupervisionFailure(reason) from error
    finally:
        if native is not None and not native_reaped:
            _native_failure(root, log, 'native_supervision_failed', 'NativeSupervisionFailure')
            forward(signal.SIGTERM)
            _retain_native_child(child, log)
        if selector is not None:
            selector.close()
        if native is None and child.poll() is None:
            forward(signal.SIGTERM)
            try:
                child.wait(timeout=80)
            except subprocess.TimeoutExpired:
                forward(signal.SIGKILL)
                child.wait()
        child.stdout.close()
        for sig, handler in previous.items():
            signal.signal(sig, handler)


def serve(root: Path, *, native_context=None) -> int:
    native = _native_context(root, native_context)
    native_options = {'native_context': native_context} if native_context is not None else {}
    if cold_pending(root) and not startup_permitted(root, **native_options):
        return 78
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
            report = inspect(root, **native_options)
            atomic_json(root / 'state/startup-health.json', report)
            if not report['ready_to_start']:
                log.write((json.dumps(report) + '\n').encode())
                return 78
            env = os.environ.copy()
            env.update(environment(root / 'config/runtime.env'))
            env['AGS_HOME'] = str(root)
            env['PATH'] = str(root / 'bin') + ':' + env.get('PATH', '')
            stage = 'program_identity'
            _native_context(root, native_context, native)
            result = subprocess.run([str(root / 'bin/gh-server'), '--version'], env=env,
                                    capture_output=True, check=True, timeout=10)
            build = json.loads(result.stdout)
            receipt = json.loads((root / 'state/installation.json').read_text())
            if build != receipt['binaries']['gh-server']['identity']:
                raise ValueError('program identity mismatch')
            stage = 'upgrade_handoff'
            generation = consume_start(root, **native_options)
            stage = 'process_lifecycle'
            return supervise(root, env, build, log, install_lock, generation, native_context=native_context)
        except Exception as error:
            # Do not serialize exception text: subprocess output/config can be sensitive.
            failure = {'schema': 'ags.runtime-failure.v1', 'stage': stage,
                       'error_type': type(error).__name__, 'at': dt.datetime.now(dt.timezone.utc).isoformat()}
            if isinstance(error, NativeSupervisionFailure):
                failure['reason'] = error.reason
            try:
                atomic_json(root / 'state/runtime-failure.json', failure)
            except OSError:
                pass
            if isinstance(error, NativeSupervisionFailure):
                # The native path already reported to stderr and retained its
                # child until natural completion. Log failure cannot turn 78
                # into an unreported exception or a successful exit.
                try:
                    log.write((json.dumps(failure) + '\n').encode())
                except Exception:
                    pass
            else:
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
