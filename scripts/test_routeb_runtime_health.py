#!/usr/bin/env python3
"""Living-generation checks with real child processes; component evidence only."""
import copy
import contextlib
import fcntl
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from upgrade_backup_contract import EXACT_COLD_SCHEMA, canonical, fingerprint
from upgrade_recovery import MARKER
import upgrade_state
from test_upgrade_backup import binding

spec = importlib.util.spec_from_file_location('routeb_health_runtime', Path(__file__).with_name('ags-runtime.py'))
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class RunningHealthTests(unittest.TestCase):
    def setUp(self):
        base = Path.home() / '.local/state/ags-routeb-component-tests'
        base.mkdir(parents=True, exist_ok=True, mode=0o700)
        self.root = Path(tempfile.mkdtemp(prefix='running-health-', dir=base))
        for name in ('state', 'bin', 'config', 'logs'):
            (self.root / name).mkdir(mode=0o700)
        self.pending = self.root / 'state/ags-autodeploy/local-v2/pending'
        self.pending.mkdir(parents=True, mode=0o700)
        self.pending.parent.chmod(0o700)
        self.pending.parent.parent.chmod(0o700)
        self.binary = self.root / 'bin/gh-server'
        self.binary.write_bytes(b'inert fixture binary')
        self.binary.chmod(0o700)
        self.binary_sha = hashlib.sha256(self.binary.read_bytes()).hexdigest()
        self.generation = 'fixture-living-generation'
        self.host = 'fixture-native-host'
        self.b = {key: value for key, value in binding().items() if key in
                  ('host_instance', 'deployment_id', 'transaction_id', 'attempt_id', 'authorization_id',
                   'old', 'new', 'runtime_tree', 'tool_sha256', 'policy_sha256', 'target')}
        self.b.update(schema=EXACT_COLD_SCHEMA, artifact_kind='exact-source', host_instance=self.host,
                      old='a' * 40, new='b' * 40, old_binary_sha256=self.binary_sha,
                      new_binary_sha256=self.binary_sha, manifest_sha256='1' * 64,
                      build_review_sha256='2' * 64, ci_evidence_sha256='3' * 64)
        self.installation = {'schema': 'ags.installation.v2', 'source': self.b['new'],
                             'binaries': {'gh-server': {'sha256': self.binary_sha,
                                                       'identity': {'revision': self.b['new']}}}}
        self.write(self.root / 'state/installation.json', self.installation)
        self.receipt_sha = hashlib.sha256((self.root / 'state/installation.json').read_bytes()).hexdigest()
        self.write(self.pending / 'claim.json', {'schema': 'ags.local-claim.v2', 'binding': self.b})
        self.write(self.root / 'state' / MARKER,
                   {'schema': 'ags.cold-window.v1', 'generation': self.generation,
                    'binding_sha256': fingerprint(self.b)})
        self.journal = {'schema': 'ags.cold-journal.v1', 'generation': self.generation,
                        'binding_sha256': fingerprint(self.b), 'backup_binding': self.b,
                        'phase': 'start_attempted', 'start_attempted': True,
                        'installed_receipt_sha256': self.receipt_sha,
                        'handoff': {'token': 'a' * 32, 'generation': self.generation, 'consumed': True,
                                    'manager_requested': True, 'binary_sha256': self.binary_sha,
                                    'receipt_sha256': self.receipt_sha}}
        self.write(self.pending / 'cold-journal.json', self.journal)
        upgrade_state.commit_phase(self.root, 'fixture-activate', 'provisioned', 'active',
                                   kind='authority_activated', epoch=1, host_instance=self.host,
                                   identity_sha256=self.binary_sha, binding_sha256=fingerprint(self.b),
                                   approval_sha256='4' * 64)
        self.child = subprocess.Popen([sys.executable, '-B', '-c', 'import time; time.sleep(120)'])
        self.addCleanup(self.stop_child)
        self.record = {'schema': 'ags.runtime.v1', 'root': str(self.root), 'generation': self.generation,
                       'supervisor_pid': os.getpid(), 'pid': self.child.pid, 'build': {'revision': self.b['new']}}
        self.write(self.root / 'state/runtime.json', self.record)
        upgrade_state.commit_phase(self.root, 'fixture-spawn', 'active', 'running', kind='generation_spawned',
                                   epoch=1, effect={'effect_id': self.generation, 'type': 'spawn',
                                                  'expected_before': 'stopped', 'desired_after': self.generation,
                                                  'status': 'observed'})
        patched = patch('upgrade_identity.runtime_identity', return_value=SimpleNamespace(host_instance=self.host))
        patched.start()
        self.addCleanup(patched.stop)

    def write(self, path, value):
        path.write_bytes(canonical(value))
        path.chmod(0o600)

    def stop_child(self):
        if self.child.poll() is None:
            self.child.terminate()
        self.child.wait(timeout=10)

    def permitted(self):
        return runtime.running_permitted(self.root, self.generation, self.child.pid)

    def test_consumed_token_keeps_own_generation_healthy_and_second_spawn_blocked(self):
        before = (self.pending / 'cold-journal.json').read_bytes()
        self.assertTrue(self.permitted())
        self.assertFalse(runtime.startup_permitted(self.root))
        with self.assertRaises(runtime.CheckFailure):
            runtime.consume_start(self.root)
        self.assertEqual((self.pending / 'cold-journal.json').read_bytes(), before)

    def test_periodic_inspection_uses_the_running_context_only_when_supplied(self):
        cold = runtime.inspect(self.root)
        living = runtime.inspect(self.root, running_generation=self.generation, running_pid=self.child.pid)
        self.assertEqual(cold['checks']['upgrade_cold_window'], 'blocked')
        self.assertEqual(living['checks']['upgrade_cold_window'], 'ok')

    def test_changed_generation_child_or_supervisor_is_unhealthy(self):
        for change in ({'generation': 'another-generation'}, {'pid': os.getpid()},
                       {'supervisor_pid': os.getpid() + 100000}, {'stopped_at': '2026-01-01T00:00:00Z'}):
            with self.subTest(change=change):
                self.write(self.root / 'state/runtime.json', {**self.record, **change})
                self.assertFalse(self.permitted())

    def test_unconsumed_or_changed_handoff_is_unhealthy(self):
        for change in ({'consumed': False}, {'manager_requested': False},
                       {'generation': 'another-generation'}, {'binary_sha256': '9' * 64},
                       {'token': 'invalid-token'}):
            with self.subTest(change=change):
                journal = copy.deepcopy(self.journal)
                journal['handoff'].update(change)
                self.write(self.pending / 'cold-journal.json', journal)
                self.assertFalse(self.permitted())

    def test_binary_and_receipt_drift_are_unhealthy(self):
        self.binary.write_bytes(b'changed bytes')
        self.assertFalse(self.permitted())
        self.binary.write_bytes(b'inert fixture binary')
        self.write(self.root / 'state/installation.json', {**self.installation, 'changed': True})
        self.assertFalse(self.permitted())

    def test_suspended_authority_and_unknown_effects_remain_unhealthy(self):
        upgrade_state.commit_phase(self.root, 'fixture-pending', 'running', 'unknown', epoch=1,
                                   kind='fixture_pending', effect={'effect_id': 'other-effect', 'type': 'start',
                                   'expected_before': 'stopped', 'desired_after': 'running', 'status': 'intent'})
        self.assertFalse(self.permitted())
        upgrade_state.commit_phase(self.root, 'fixture-suspend', 'active', 'suspended',
                                   kind='authority_suspended', epoch=1)
        self.assertFalse(self.permitted())

    def test_a_dead_child_does_not_reuse_the_consumed_permission(self):
        self.stop_child()
        self.assertFalse(self.permitted())


class NativeNoForceTests(unittest.TestCase):
    """Real pipe/child ownership; injected elapsed deadline is component evidence."""
    def exercise(self, *, native, fault, broken_log=False, broken_state=False, via_serve=False):
        base = Path.home() / '.local/state/ags-routeb-component-tests'
        base.mkdir(parents=True, exist_ok=True, mode=0o700)
        root = Path(tempfile.mkdtemp(prefix='native-no-force-', dir=base))
        (root / 'state').mkdir(mode=0o700)
        if via_serve:
            (root / 'config').mkdir(mode=0o700)
            (root / 'config/runtime.env').write_bytes(b'')
            (root / 'config/runtime.env').chmod(0o600)
            runtime.atomic_json(root / 'state/installation.json', {'binaries': {'gh-server': {'identity': {}}}})
        control_read, control_write = os.pipe()
        ack_read, ack_write = os.pipe()
        program = ('import os,signal,sys\n'
                   'signal.signal(signal.SIGTERM,lambda *_:None)\n'
                   'os.write(1,b"READY\\n")\n'
                   'command=os.read(int(sys.argv[1]),1)\n'
                   'if command==b"a": raise SystemExit(0)\n'
                   'assert command==b"w"\n'
                   'for _ in range(8):\n'
                   ' data=b"x"*32768\n'
                   ' while data:\n'
                   '  n=os.write(1,data);data=data[n:]\n'
                   'os.write(int(sys.argv[2]),b"DRAINED\\n")\n'
                   'assert os.read(int(sys.argv[1]),1)==b"q"\n')
        child = subprocess.Popen([sys.executable, '-B', '-c', program, str(control_read), str(ack_write)],
                                 stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 pass_fds=(control_read, ack_write), start_new_session=True)
        os.close(control_read); os.close(ack_write)
        self.assertTrue(select.select([child.stdout], [], [], 5)[0], 'fixture child not ready')
        self.assertEqual(child.stdout.readline(), b'READY\n')
        original_wait = child.wait
        def bounded_wait(timeout=None):
            if timeout == 80:
                raise subprocess.TimeoutExpired('owned fixture child', timeout)
            return original_wait(timeout=timeout)
        child.wait = bounded_wait
        signal_calls, handlers, failures, observed = [], {}, [], {}
        claimed, writing = [False], [False]
        def claim_child(*_args, **_kwargs):
            claimed[0] = True
            return child
        real_killpg, real_clock = os.killpg, time.monotonic
        real_write, failure_seen, stderr = os.write, threading.Event(), bytearray()
        def write(fd, data):
            if fd == 2:
                stderr.extend(data)
                failure_seen.set()
            return real_write(fd, data)
        offset = [0]
        def install_handler(signum, handler):
            handlers[signum] = handler
            if signum == signal.SIGINT and fault == 'deadline' and callable(handler):
                handlers[signal.SIGTERM](signal.SIGTERM)
                offset[0] = 81
            return signal.SIG_DFL
        def signal_group(pid, signum):
            signal_calls.append((pid, signum))
            if signum != signal.SIGKILL:
                return real_killpg(pid, signum)
            # Never force the real fixture: observing the attempted escalation
            # is enough for the old source to fail this assertion.
        context = {'schema': 'ags.routeb-stage0-context.v1', 'root': str(root), 'operation': 'serve', 'phase': 'serve-initial',
                   'supervisor': {'pid': os.getppid(), 'uid': 0, 'gid': 0},
                   'child': {'pid': os.getpid(), 'parent_pid': os.getppid(), 'uid': os.getuid(), 'gid': os.getgid()}}
        context_calls = [0]
        def live_context():
            context_calls[0] += 1
            if fault == 'exception' and context_calls[0] == 2:
                raise ValueError('fixture peer became unavailable')
            return copy.deepcopy(context)
        class Log:
            count = 0
            def close(self):
                pass
            def write(self, data):
                if broken_log:
                    raise OSError('fixture log unavailable')
                self.count += len(data)
        log = Log()
        def release_after_failure():
            try:
                deadline = real_clock() + 10
                while real_clock() < deadline:
                    if failure_seen.is_set() or (root / 'state/runtime-failure.json').exists() or any(s == signal.SIGKILL for _, s in signal_calls):
                        break
                    time.sleep(.01)
                else:
                    raise AssertionError('no failure fact or attempted escalation observed')
                self.assertTrue(claimed[0], 'fixture failed before primary supervision')
                observed['alive_before_release'] = child.poll() is None
                observed['pipe_open_before_release'] = not child.stdout.closed
                if via_serve:
                    fd = os.open(root / 'state/runtime.lock', os.O_RDWR)
                    try:
                        try:
                            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
                        except BlockingIOError:
                            observed['runtime_lock_held'] = True
                        else:
                            observed['runtime_lock_held'] = False
                            fcntl.flock(fd, fcntl.LOCK_UN)
                    finally:
                        os.close(fd)
                os.write(control_write, b'w')
                writing[0] = True
                if fault == 'exception' and any(s == signal.SIGKILL for _, s in signal_calls):
                    # The old finally branch waits without draining. Keep this
                    # intentionally failing reproduction from stranding its
                    # cooperative child; this is never used by a passing run.
                    end = real_clock() + 5
                    while real_clock() < end:
                        readable = select.select([child.stdout, ack_read], [], [], .1)[0]
                        if ack_read in readable:
                            break
                        if child.stdout in readable:
                            os.read(child.stdout.fileno(), 65536)
                ready = select.select([ack_read], [], [], 5)[0]
                observed['drained_after_failure'] = bool(ready) and os.read(ack_read, 64) == b'DRAINED\n'
                observed['alive_after_drain'] = child.poll() is None
            except BaseException as error:
                failures.append(error)
            finally:
                # A cooperative fixture exit is independent of the stop signal.
                os.write(control_write, b'q' if writing[0] else b'a')
        release = threading.Thread(target=release_after_failure, daemon=True)
        release.start()
        result, error = None, None
        try:
            with patch.object(runtime.subprocess, 'Popen', side_effect=claim_child), \
                    patch.object(runtime.signal, 'signal', side_effect=install_handler), \
                    patch.object(runtime.os, 'killpg', side_effect=signal_group), \
                    patch.object(runtime.os, 'write', side_effect=write), \
                    (patch.object(runtime, 'inspect', return_value={'ready_to_start': True})
                     if via_serve else contextlib.nullcontext()), \
                    (patch.object(runtime, 'BoundedLog', return_value=log)
                     if via_serve else contextlib.nullcontext()), \
                    (patch.object(runtime.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'{}', b''))
                     if via_serve else contextlib.nullcontext()), \
                    (patch.object(runtime, 'atomic_json', side_effect=OSError('fixture state unavailable'))
                     if broken_state else contextlib.nullcontext()), \
                    patch.object(runtime.time, 'monotonic', side_effect=lambda: real_clock() + offset[0]):
                try:
                    if via_serve:
                        result = runtime.serve(root, native_context=live_context if native else None)
                    else:
                        result = runtime.supervise(root, {}, {}, log, Mock(),
                                                   native_context=live_context if native else None)
                except BaseException as failure:
                    error = failure
        finally:
            release.join(timeout=10)
            child.wait = original_wait
            if child.poll() is None:
                # A failed harness must also preserve the read end. Abort before
                # output starts, or drain while its own protocol requests exit.
                try:
                    os.write(control_write, b'q' if writing[0] else b'a')
                except OSError:
                    pass
                os.set_blocking(child.stdout.fileno(), False)
                deadline = real_clock() + 10
                while child.poll() is None and real_clock() < deadline:
                    try:
                        os.read(child.stdout.fileno(), 65536)
                    except BlockingIOError:
                        pass
                    time.sleep(.01)
            original_wait(timeout=10)
            for fd in (control_write, ack_read):
                os.close(fd)
            child.stdout.close()
        self.assertFalse(release.is_alive())
        if failures:
            raise failures[0]
        self.assertEqual(child.returncode, 0)
        if broken_state:
            self.assertIn(b'"reason": "native_supervision_failed"', stderr)
            self.assertNotIn(b'fixture state unavailable', stderr)
        return root, signal_calls, observed, result, error

    def test_native_deadline_retains_real_pipe_and_late_child_zero_is_failure(self):
        root, signals, observed, result, error = self.exercise(native=True, fault='deadline', via_serve=True)
        self.assertNotIn(signal.SIGKILL, [number for _, number in signals])
        self.assertTrue(all(observed.values()), observed)
        self.assertEqual(result, 78)
        self.assertIsNone(error)
        self.assertEqual(json.loads((root / 'state/runtime-failure.json').read_bytes())['reason'], 'native_stop_timeout')
        stopped = json.loads((root / 'state/runtime.json').read_bytes())
        self.assertEqual(stopped['exit_code'], 0)
        self.assertTrue(stopped['supervision_failed'])
        self.assertTrue(stopped['stop_timed_out'])
        self.assertEqual(stopped['supervisor_exit_code'], 78)

    def test_native_exception_retains_child_and_drains_even_when_log_fails(self):
        root, signals, observed, result, error = self.exercise(native=True, fault='exception', broken_log=True)
        self.assertNotIn(signal.SIGKILL, [number for _, number in signals])
        self.assertTrue(all(observed.values()), observed)
        self.assertIsNone(result)
        self.assertIsInstance(error, runtime.NativeSupervisionFailure)
        self.assertEqual(error.reason, 'native_supervision_failed')
        failure = json.loads((root / 'state/runtime-failure.json').read_bytes())
        self.assertEqual(failure['reason'], 'native_supervision_failed')

    def test_ordinary_runtime_keeps_existing_escalation_contract(self):
        _, signals, _, result, error = self.exercise(native=False, fault='deadline')
        self.assertIn(signal.SIGKILL, [number for _, number in signals])
        self.assertEqual(result, 0)
        self.assertIsNone(error)

    def test_failed_state_and_log_keep_stderr_failure_and_pipe_ownership(self):
        root, signals, observed, result, error = self.exercise(
            native=True, fault='exception', broken_log=True, broken_state=True)
        self.assertNotIn(signal.SIGKILL, [number for _, number in signals])
        self.assertTrue(all(observed.values()), observed)
        self.assertIsNone(result)
        self.assertIsInstance(error, runtime.NativeSupervisionFailure)
        self.assertFalse((root / 'state/runtime-failure.json').exists())


if __name__ == '__main__':
    unittest.main()
