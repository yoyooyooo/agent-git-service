#!/usr/bin/env python3
"""Private S3 component harness: real fixture processes/flock; no host service calls."""
from datetime import datetime, timezone
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import unittest
from unittest.mock import patch

import upgrade
import upgrade_engine as engine
import upgrade_host as host
import upgrade_recovery as cold
import upgrade_backup_contract as c
import test_upgrade_backup as storage_fixtures

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('cold_runtime', HERE / 'ags-runtime.py')
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


class FixtureManager:
    """Fixed local manager stub; only starts/stops this test's subprocesses."""
    def __init__(self, root):
        self.root = root
        self.suppressed = False
        self.starts = 0
        self.proc = None
        self.generation = 'fixture-old-generation'

    def launch(self, mode='normal'):
        code = '''import os,signal,subprocess,sys,time
child=subprocess.Popen([sys.executable,'-B','-c',
 'import signal,time;signal.signal(signal.SIGTERM,lambda *a:exit(0));print("ready",flush=True);time.sleep(120)'],stdout=subprocess.PIPE)
assert child.stdout.readline()==b'ready\\n'
def stop(*a):
 child.terminate();child.wait();exit(0)
signal.signal(signal.SIGTERM, stop if sys.argv[1]=='normal' else signal.SIG_IGN)
print('ready',flush=True)
while True: time.sleep(.02)
'''
        self.proc = subprocess.Popen([sys.executable, '-B', '-c', code, mode],
                                     cwd=self.root, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                     start_new_session=True)
        if self.proc.stdout.readline() != b'ready\n':
            raise AssertionError('fixture failed to initialize')
        self.record = {'root': str(self.root), 'generation': self.generation,
                       'pid': self.proc.pid, 'supervisor_pid': self.proc.pid,
                       'started_at': datetime.now(timezone.utc).isoformat()}
        engine.save(self.root / 'state/runtime.json', self.record)

    def cleanup(self):
        if self.proc is not None:
            if self.proc.poll() is None:
                os.killpg(self.proc.pid, signal.SIGKILL)
            self.proc.wait()
            self.proc.stdout.close(); self.proc.stderr.close()

    def suppress_restart(self):
        self.suppressed = True

    def stop(self):
        self.proc.terminate()
        forced = False
        try:
            self.proc.wait(timeout=.3)
        except subprocess.TimeoutExpired:
            forced = True
            os.killpg(self.proc.pid, signal.SIGKILL)
            self.proc.wait()
        result = {**self.record, 'exit_code': self.proc.returncode, 'forced_kill': forced,
                  'stop_requested': True, 'stopped_at': datetime.now(timezone.utc).isoformat()}
        engine.save(self.root / 'state/runtime.json', result)
        try:
            os.killpg(self.proc.pid, 0)
            empty = False
        except ProcessLookupError:
            empty = True
        return {**result, 'supervisor_exit_code': self.proc.returncode,
                'supervisor_exited': self.proc.poll() is not None, 'process_group_empty': empty}

    def start(self):
        self.starts += 1
        if not self.suppressed:
            raise AssertionError('manager restart must stay suppressed')
        return cold.consume_start(self.root)


class ColdFixture(unittest.TestCase):
    def setUp(self):
        # Reuse S1/S2 synthetic storage/admission. Do not inherit all their tests.
        self.storage = storage_fixtures.BackupTests()
        self.storage.setUp()
        self.addCleanup(self.storage.doCleanups)
        self.root = self.storage.root
        self.storage.repo()
        for directory in ('state', 'bin', 'config', 'logs'):
            (self.root / directory).mkdir(mode=0o700)
        self.pending = self.root / 'state/ags-autodeploy/local-v2/pending'
        self.pending.mkdir(mode=0o700, parents=True)
        for parent in (self.pending.parent, self.pending.parent.parent):
            parent.chmod(0o700)
        self.old = b'inert old binary'
        self.new = b'inert new binary'
        self.binary = self.root / 'bin/gh-server'
        self.binary.write_bytes(self.old); self.binary.chmod(0o700)
        self.b = storage_fixtures.binding()
        self.b.update(old='a' * 40, new='b' * 40, authorization_id='window-auth',
                      old_binary_sha256=hashlib.sha256(self.old).hexdigest(),
                      new_binary_sha256=hashlib.sha256(self.new).hexdigest())
        self.identity = {'revision': self.b['new']}
        self.old_receipt = {'schema': 'ags.installation.v2', 'source': self.b['old'],
                            'binaries': {'gh-server': {'sha256': self.b['old_binary_sha256'],
                                                     'identity': {'revision': self.b['old']}}}}
        engine.save(self.root / 'state/installation.json', self.old_receipt)
        self.raw_old_receipt = (self.root / 'state/installation.json').read_bytes()
        self.request = {'old': self.b['old'], 'new': self.b['new'], 'authorization_id': 'window-auth'}
        engine.save(self.pending / 'claim.json', {'schema': 'ags.local-claim.v2', 'binding': self.request})
        self.group = self.storage.prepared_group()
        for item in self.group['objects']:
            if item['role'] in ('old_binary', 'installation'):
                path = self.binary if item['role'] == 'old_binary' else self.root / 'state/installation.json'
                item.update(source=str(path), restore_target=str(path))
        for item in self.group['objects']:
            path = Path(item['source'])
            if path.is_file():
                path.chmod(0o700 if item['role'] == 'old_binary' else 0o600)
        self.manager = FixtureManager(self.root)
        self.addCleanup(self.manager.cleanup)
        self.manager.launch()
        self.window = cold.ColdWindow(self.root, self.request, self.b, self.group,
                                      'sqlite:' + str(self.storage.dbpath), self.storage.admission,
                                      self.manager, self.writers, self.trash)
        self.window.acquire_owner()
        self.addCleanup(self.window.release_owner)
        self.addCleanup(self.window.release)

    def trash(self, path):
        # Fixture models the callback by moving its own restore copy, never deleting.
        Path(path).rename(self.root / ('fixture-retained-' + Path(path).name))

    def writers(self):
        return {'generation': self.window.journal['generation'], 'closed': True,
                'writers': [], 'unknown': []}

    def prepare(self, until='install_started'):
        self.window.begin(); self.window.stop(); self.window.quiescent()
        if until == 'quiescent': return
        self.window.backup()
        if until == 'backup': return
        self.window.restore()
        if until == 'restore_verified': return
        self.window.install_intent(lambda: None)

    def install(self):
        self.binary.write_bytes(self.new)
        engine.save(self.root / 'state/installation.json', {
            'schema': 'ags.installation.v2', 'source': self.b['new'],
            'runtime_tree': self.b['runtime_tree'], 'control_revision': self.b['control_revision'],
            'release_id': self.b['release_id'], 'version': self.b['release_tag'],
            'binaries': {'gh-server': {'sha256': self.b['new_binary_sha256'], 'identity': self.identity}}})
        self.window.installed(self.identity, lambda _: self.identity)


class QuiescenceTests(ColdFixture):
    def test_normal_exit_dual_locks_and_s2_evidence(self):
        self.prepare()
        self.assertTrue(self.manager.suppressed)
        self.assertEqual(self.manager.proc.returncode, 0)
        self.assertEqual(self.window.journal['phase'], 'install_started')
        self.assertEqual((self.pending / 'old-binary').read_bytes(), self.old)
        self.assertEqual((self.pending / 'old-installation.json').read_bytes(), self.raw_old_receipt)
        for name in ('install.lock', 'runtime.lock'):
            result = subprocess.run([sys.executable, '-B', '-c',
                'import fcntl,sys;f=open(sys.argv[1],"r+");fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)',
                str(self.root / 'state' / name)], capture_output=True, cwd=self.root)
            self.assertNotEqual(result.returncode, 0)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_sigkill_nonzero_unknown_generation_refuse_and_keep_marker(self):
        for kind in ('forced', 'nonzero', 'generation', 'supervisor', 'unknown', 'timeout'):
            with self.subTest(kind=kind):
                # Independent marker/journal generation per negative case.
                if (self.root / 'state' / cold.MARKER).exists():
                    (self.root / 'state' / cold.MARKER).rename(self.root / ('retained-' + kind))
                self.manager.cleanup(); self.manager.launch('hang' if kind == 'forced' else 'normal')
                self.window.begin()
                original = self.manager.stop
                def stop():
                    if kind == 'timeout': raise TimeoutError('fixture timeout')
                    result = original()
                    if kind == 'nonzero': result['exit_code'] = 9
                    if kind == 'generation': result['generation'] = 'unknown'
                    if kind == 'supervisor': result['supervisor_exit_code'] = 9
                    if kind == 'unknown': result['process_group_empty'] = None
                    return result
                with patch.object(self.manager, 'stop', side_effect=stop), self.assertRaises((c.ContractError, TimeoutError)):
                    self.window.stop()
                self.assertFalse(cold.startup_permitted(self.root))
                self.assertEqual(self.manager.starts, 0)

    def test_second_upgrade_process_cannot_take_hostops_ownership(self):
        script = 'import fcntl,sys;f=open(sys.argv[1],"r+");fcntl.flock(f,fcntl.LOCK_EX|fcntl.LOCK_NB)'
        result = subprocess.run([sys.executable, '-B', '-c', script,
                                 str(self.pending.parent / 'hostops.lock')], capture_output=True, cwd=self.root)
        self.assertNotEqual(result.returncode, 0)
        self.window.begin()
        self.window.release_owner()
        self.window.acquire_owner()
        with self.assertRaises(c.ContractError): self.window.begin()
        self.assertFalse(cold.startup_permitted(self.root))

    def test_phase_skips_and_backwards_transition_refuse(self):
        self.window.begin()
        for phase in ('installed', 'start_attempted', 'old_restored', 'unknown'):
            with self.subTest(phase=phase), self.assertRaises(c.ContractError):
                self.window.phase(phase)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_stop_result_loss_is_not_replayed(self):
        self.window.begin()
        with patch.object(self.manager, 'stop', side_effect=TimeoutError('fixture')) as stop:
            with self.assertRaises(TimeoutError): self.window.stop()
            with self.assertRaises(c.ContractError): self.window.stop()
        self.assertEqual(stop.call_count, 1)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_runtime_consumes_token_before_real_fixture_spawn_and_rejects_restart(self):
        # Executable synthetic server, never an AGS binary or production config.
        code = ('#!' + sys.executable + '\nimport json,sys\n'
                'if "--version" in sys.argv: print(' + repr(json.dumps(self.identity)) + ');exit(0)\n'
                'print("fixture started",flush=True)\n').encode()
        self.new = code
        self.b['new_binary_sha256'] = hashlib.sha256(code).hexdigest()
        self.window.backup_binding = copy.deepcopy(self.b)
        self.prepare(); self.install()
        self.binary.chmod(0o700)
        (self.root / 'config/runtime.env').write_text('FIXTURE_ONLY=1\n')
        (self.root / 'config/runtime.env').chmod(0o600)
        self.window.start_intent(); self.window.release()
        observed = []
        real_spawn = runtime.subprocess.Popen
        def spawn(*args, **kwargs):
            journal = cold.read_window(self.root)[1]
            if '--version' not in args[0]:
                observed.append(journal['handoff']['consumed'])
            return real_spawn(*args, **kwargs)
        with patch.object(self.manager, 'start', side_effect=lambda: runtime.serve(self.root)):
            with patch.object(runtime.subprocess, 'Popen', side_effect=spawn):
                self.window.start()
        self.assertTrue(observed)
        self.assertTrue(all(observed))
        self.assertEqual(runtime.serve(self.root), 78)
        record = json.loads((self.root / 'state/runtime.json').read_bytes())
        self.assertEqual(record['generation'], self.window.journal['generation'])

    def test_each_busy_lock_refuses_before_snapshot(self):
        import fcntl
        self.window.begin(); self.window.stop()
        for name in ('install.lock', 'runtime.lock'):
            with (self.root / 'state' / name).open('w') as lock:
                os.chmod(lock.name, 0o600)
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with self.assertRaises(upgrade.Failure) as caught:
                    self.window.quiescent()
                self.assertEqual(caught.exception.exit_code, 73)
                self.assertIsNone(self.window.locks)
        self.assertEqual(list(self.storage.backups.iterdir()), [])

    def test_writer_or_unknown_blocks(self):
        self.window.begin(); self.window.stop()
        for key, value in (('writers', [123]), ('unknown', ['probe']), ('closed', False)):
            with patch.object(self.window, 'writer_probe', return_value={**self.writers(), key: value}):
                with self.assertRaises(c.ContractError): self.window.quiescent()
            self.window.release()
        self.assertFalse(cold.startup_permitted(self.root))

    def test_automatic_manager_restart_cannot_spawn_during_cold_window(self):
        self.window.begin()
        with patch.object(runtime.subprocess, 'Popen', side_effect=AssertionError('no spawn')):
            self.assertEqual(runtime.serve(self.root), 78)
        self.assertEqual(self.manager.starts, 0)

    def test_start_token_once_and_manager_response_loss_never_replays(self):
        self.prepare(); self.install()
        self.window.start_intent()
        self.assertFalse(cold.startup_permitted(self.root))
        self.window.release()
        def lost():
            self.manager.starts += 1
            cold.consume_start(self.root)
            raise TimeoutError('fixture response lost')
        with patch.object(self.manager, 'start', side_effect=lost), self.assertRaises(TimeoutError):
            self.window.start()
        with self.assertRaises(c.ContractError): self.window.start()
        with self.assertRaises(c.ContractError): cold.consume_start(self.root)
        before = {str(p): p.read_bytes() for p in self.pending.rglob('*') if p.is_file()}
        readback = cold.reconcile(self.root)
        self.assertTrue(readback['start_attempted'])
        self.assertTrue(readback['token_consumed'])
        self.assertEqual(readback['action'], 'read_only_no_replay')
        self.assertEqual(before, {str(p): p.read_bytes() for p in self.pending.rglob('*') if p.is_file()})
        self.assertEqual(self.manager.starts, 1)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_process_crash_at_every_cold_phase_keeps_spawn_fenced(self):
        # A fork inherits the real fixture/locks; os._exit simulates owner loss.
        for phase in ('cold-marker', 'stop-intent', 'quiescent', 'backup', 'restore_verified',
                      'install_started', 'installed', 'start_attempted'):
            with self.subTest(phase=phase):
                if phase not in ('cold-marker', 'stop-intent'):
                    self.window.begin(); self.window.stop()
                pid = os.fork()
                if pid == 0:
                    try:
                        def crash(name):
                            if name == phase: os._exit(91)
                        cold.checkpoint = crash
                        if phase in ('cold-marker', 'stop-intent'):
                            self.window.begin()
                        else:
                            self.window.quiescent(); self.window.backup(); self.window.restore()
                            self.window.install_intent(lambda: None)
                            self.install(); self.window.start_intent()
                        os._exit(92)
                    except BaseException:
                        os._exit(93)
                _, status = os.waitpid(pid, 0)
                self.assertEqual(os.waitstatus_to_exitcode(status), 91)
                # No fixed manager request exists at any of these crash points.
                self.assertFalse(cold.startup_permitted(self.root))
                with patch.object(runtime.subprocess, 'Popen', side_effect=AssertionError('no spawn')):
                    self.assertEqual(runtime.serve(self.root), 78)
                # Retain evidence and create another independent fixture next iteration.
                if phase != 'start_attempted':
                    self.doCleanups(); self.setUp()

    def test_missing_journal_unknown_phase_generation_and_missing_marker_fail_closed(self):
        self.window.begin()
        path = self.pending / 'cold-journal.json'
        original = path.read_bytes()
        path.rename(self.pending / 'retained-journal')
        self.assertFalse(cold.startup_permitted(self.root))
        path.write_bytes(original); path.chmod(0o600)
        for key, value in (('phase', 'future'), ('generation', 'other')):
            journal = json.loads(original); journal[key] = value
            engine.save(path, journal)
            self.assertFalse(cold.startup_permitted(self.root))
        (self.root / 'state' / cold.MARKER).rename(self.root / 'retained-marker')
        self.assertFalse(cold.startup_permitted(self.root))

    def test_installed_mismatch_never_creates_start_authority(self):
        self.prepare()
        self.binary.write_bytes(self.new)
        with self.assertRaises(c.ContractError):
            self.window.installed(self.identity, lambda _: self.identity)
        self.install()
        self.binary.write_bytes(b'tampered')
        with self.assertRaises(c.ContractError): self.window.start_intent()
        self.assertFalse(cold.startup_permitted(self.root))
        self.assertFalse(cold.read_window(self.root)[1]['start_attempted'])
        self.assertEqual(self.manager.starts, 0)

    def test_start_intent_crash_never_allows_old_restore_or_second_manager_request(self):
        self.prepare(); self.install(); self.window.start_intent(); self.window.release()
        def crash(name):
            if name == 'manager-start-intent': raise RuntimeError('fixture crash')
        with patch.object(cold, 'checkpoint', side_effect=crash), self.assertRaises(RuntimeError):
            self.window.start()
        with self.assertRaises(c.ContractError): self.window.start()
        self.assertEqual(self.manager.starts, 0)
        # One durable launcher capability remains; consuming before spawn and then
        # crashing must not refund it or permit another launcher.
        def consumed(name):
            if name == 'start-token-consumed': raise RuntimeError('fixture crash')
        with patch.object(cold, 'checkpoint', side_effect=consumed), self.assertRaises(RuntimeError):
            cold.consume_start(self.root)
        with self.assertRaises(c.ContractError): cold.consume_start(self.root)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_s2_inventory_basis_is_revalidated_before_install(self):
        self.prepare(until='restore_verified')
        backup = Path(self.window.journal['backup_dir'])
        engine.save(backup / 'inventory.json', {'inventory_sha256': '0' * 64})
        with self.assertRaises(c.ContractError): self.window.install_intent(lambda: None)
        self.assertEqual(self.binary.read_bytes(), self.old)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_s2_seal_and_restore_report_are_revalidated_not_trusted_booleans(self):
        self.prepare(until='restore_verified')
        backup = Path(self.window.journal['backup_dir'])
        raw = (backup / 'restore-report.json').read_bytes()
        report = json.loads(raw); report['manifest_sha256'] = '0' * 64
        engine.save(backup / 'restore-report.json', report)
        with self.assertRaises(c.ContractError): self.window.install_intent(lambda: None)
        self.assertEqual(self.binary.read_bytes(), self.old)


if __name__ == '__main__':
    unittest.main()
