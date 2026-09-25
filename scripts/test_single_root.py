#!/usr/bin/env python3
"""Real isolated files/processes for the single-root installer and supervisor."""
import fcntl
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent

def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, HERE / filename)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result

installer = module('single_installer', 'install-release.py')
runtime = module('single_runtime', 'ags-runtime.py')

class SingleRootTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix='ags-single-root-')
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name).resolve()
        self.root = self.directory / 'home'

    def bundle(self, suffix):
        info = {'version': 'fork-20260924.' + suffix, 'binaries': {}}
        archive = self.directory / ('bundle-' + suffix + '.tgz')
        with tarfile.open(archive, 'w:gz') as output:
            for command in installer.COMMANDS:
                identity = {'command': command, 'version': info['version']}
                data = ('#!' + sys.executable + '\nimport json,sys,time\n'
                        'if "--version" in sys.argv: print(' + repr(json.dumps(identity)) + '); raise SystemExit(0)\n'
                        'print("started",flush=True)\nwhile True: time.sleep(.05)\n').encode()
                info['binaries'][command] = {'identity': identity, 'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data)}
                entry = tarfile.TarInfo('bin/' + command); entry.size = len(data); entry.mode = 0o755
                output.addfile(entry, io.BytesIO(data))
            for name, data in (('LICENSE', b'fixture license'), ('build-info.json', json.dumps(info).encode())):
                entry = tarfile.TarInfo(name); entry.size = len(data)
                output.addfile(entry, io.BytesIO(data))
        receipt = {'schema': installer.INSTALLATION_SCHEMA, 'version': info['version'], 'source': 'a' * 40,
                   'target': 'fixture', 'binaries': info['binaries']}
        return archive, info, receipt

    def install(self, suffix='1'):
        archive, info, receipt = self.bundle(suffix)
        installer.install_verified(archive, info, receipt, self.root)
        return archive, info, receipt

    def configured(self):
        self.install()
        for directory in ('config', 'data', 'logs', 'cache'):
            (self.root / directory).mkdir(mode=0o700)
        (self.root / 'data/fixture.db').write_bytes(b'fixture')
        runtime.atomic_json(self.root / 'config/retention.json', {'log_segment_bytes': 65536, 'log_backups': 2})
        env = self.root / 'config/runtime.env'
        env.write_text('DB_DSN="sqlite://' + str(self.root / 'data/fixture.db') + '"\nPRIVATE_TEST_TOKEN=never-print-me\n')
        env.chmod(0o600)

    def test_repeated_upgrades_keep_one_installation_and_no_downloads(self):
        for suffix in ('1', '2', '3', '3'):
            self.install(suffix)
            self.assertFalse((self.root / 'current').exists())
            self.assertFalse((self.root / 'releases').exists())
            self.assertEqual(list(self.root.glob('.install-*')), [])
            self.assertEqual(list(self.root.glob('.download-*')), [])
            self.assertFalse((self.root / 'state/installing.json').exists())
            self.assertEqual(len(list((self.root / 'bin').iterdir())), 3)
        receipt = json.loads((self.root / 'state/installation.json').read_text())
        self.assertEqual(receipt['version'], 'fork-20260924.3')

    def test_install_preserves_config_and_data(self):
        self.configured()
        before = (self.root / 'config/runtime.env').read_bytes()
        self.install('2')
        self.assertEqual((self.root / 'config/runtime.env').read_bytes(), before)
        self.assertEqual((self.root / 'data/fixture.db').read_bytes(), b'fixture')

    def test_binary_drift_and_foreign_symlink_rejected(self):
        self.install()
        binary = self.root / 'bin/gh-server'
        binary.write_bytes(b'foreign')
        with self.assertRaises(ValueError): self.install('2')
        binary.unlink(); binary.symlink_to('/bin/true')
        with self.assertRaises(ValueError): self.install('2')
        self.assertEqual(list(self.root.glob('.install-*')), [])

    def test_interrupted_replacement_fences_start_and_exact_retry_finishes(self):
        self.configured()
        archive, info, receipt = self.bundle('2')
        real_replace = os.replace
        def fail_one(src, dst):
            if Path(dst) == self.root / 'bin/ags-edge': raise OSError('injected replacement interruption')
            return real_replace(src, dst)
        with patch.object(installer.os, 'replace', side_effect=fail_one):
            with self.assertRaises(OSError): installer.install_verified(archive, info, receipt, self.root)
        self.assertFalse(runtime.inspect(self.root)['ready_to_start'])
        with self.assertRaises(ValueError): self.install('3')
        installer.install_verified(archive, info, receipt, self.root)
        self.assertTrue(runtime.inspect(self.root)['ready_to_start'])

    def test_failed_candidate_validation_leaves_old_installation(self):
        self.install()
        before = (self.root / 'bin/gh-server').read_bytes()
        archive, info, receipt = self.bundle('2')
        with patch.object(installer, 'execute', return_value=b'{}'):
            with self.assertRaises(ValueError): installer.install_verified(archive, info, receipt, self.root)
        self.assertEqual((self.root / 'bin/gh-server').read_bytes(), before)
        self.assertFalse((self.root / 'state/installing.json').exists())
        self.assertEqual(list(self.root.glob('.install-*')), [])

    def test_single_colon_sqlite_dsn_is_checked_like_native_linux_runtime(self):
        self.configured()
        env = self.root / 'config/runtime.env'
        env.write_text('DB_DSN="sqlite:' + str(self.root / 'data/fixture.db') + '?_pragma=foreign_keys(ON)"\n')
        report = runtime.inspect(self.root)
        self.assertTrue(report['ready_to_start'])
        self.assertEqual(report['checks']['database_path'], 'ok')
        env.write_text('DB_DSN="sqlite:' + str(self.directory / 'outside.db') + '"\n')
        report = runtime.inspect(self.root)
        self.assertFalse(report['ready_to_start'])
        self.assertEqual(report['checks']['database_path'], 'outside_root_or_missing')

    def test_doctor_detects_missing_dependency_and_never_prints_token(self):
        self.configured()
        self.assertTrue(runtime.inspect(self.root)['ready_to_start'])
        env = self.root / 'config/runtime.env'
        with env.open('a') as output:
            output.write('AGS_REPLICATION_CONFIG_FILE="' + str(self.root / 'config/missing.json') + '"\n')
        report = runtime.inspect(self.root)
        self.assertFalse(report['ready_to_start'])
        self.assertEqual(report['checks']['replication_config'], 'missing_or_unsafe')
        self.assertNotIn('never-print-me', json.dumps(report))
        self.assertFalse(runtime.durable_path(self.root, self.root / 'deployments/old/config.json'))

    def test_log_budget_is_enforced_on_large_unbroken_output(self):
        directory = self.directory / 'logs'; directory.mkdir()
        log = runtime.BoundedLog(directory, 65536, 2)
        log.write(b'x' * (65536 * 11 + 123)); log.close()
        files = list(directory.iterdir())
        self.assertEqual(len(files), 3)
        self.assertLessEqual(sum(path.stat().st_size for path in files), 65536 * 3)
        self.assertTrue(all(path.stat().st_mode & 0o077 == 0 for path in files))

    def test_running_owner_blocks_program_replacement(self):
        self.configured()
        before = (self.root / 'bin/gh-server').read_bytes()
        with (self.root / 'state/runtime.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            with self.assertRaisesRegex(ValueError, 'Stop the managed runtime'):
                self.install('2')
        self.assertEqual((self.root / 'bin/gh-server').read_bytes(), before)
        self.assertFalse((self.root / 'state/installing.json').exists())

    def test_symlinked_managed_directory_fails_doctor(self):
        self.configured()
        source = self.root / 'config'
        outside = self.directory / 'outside-config'
        source.rename(outside)
        source.symlink_to(outside, target_is_directory=True)
        report = runtime.inspect(self.root)
        self.assertFalse(report['ready_to_start'])
        self.assertEqual(report['checks']['config_directory'], 'missing_or_unsafe')

    def test_invalid_retention_is_visible_without_starting_primary(self):
        self.configured()
        for value in ([], {'log_segment_bytes': 1.5}, {'log_backups': True}):
            runtime.atomic_json(self.root / 'config/retention.json', value)
            report = runtime.inspect(self.root)
            self.assertFalse(report['ready_to_start'])
            self.assertEqual(report['checks']['retention_config'], 'invalid_or_unsafe')
            with patch.object(runtime, 'supervise') as spawn:
                self.assertEqual(runtime.serve(self.root), 78)
                spawn.assert_not_called()
        log = (self.root / 'logs/runtime.log').read_text()
        self.assertIn('invalid_or_unsafe', log)
        self.assertNotIn('never-print-me', log)

    def test_bad_version_identity_is_recorded_before_primary_spawn(self):
        self.configured()
        with patch.object(runtime.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'{}', b'')):
            with patch.object(runtime, 'supervise') as spawn:
                self.assertEqual(runtime.serve(self.root), 78)
                spawn.assert_not_called()
        failure = json.loads((self.root / 'state/runtime-failure.json').read_text())
        self.assertEqual(failure['stage'], 'program_identity')
        self.assertFalse((self.root / 'state/runtime.json').exists())

    def test_failed_initial_receipt_reaps_real_child_and_redacts_error(self):
        self.configured()
        real_write = runtime.atomic_json
        real_spawn = subprocess.Popen
        children = []
        def fail_receipt(path, value):
            if path.name == 'runtime.json':
                raise OSError('never-print-me')
            return real_write(path, value)
        def track_spawn(args, **kwargs):
            child = real_spawn(args, **kwargs)
            if len(args) == 1 and args[0] == str(self.root / 'bin/gh-server'):
                children.append(child)
            return child
        with patch.object(runtime, 'atomic_json', side_effect=fail_receipt):
            with patch.object(runtime.subprocess, 'Popen', side_effect=track_spawn):
                self.assertEqual(runtime.serve(self.root), 78)
        self.assertEqual(len(children), 1)
        self.assertIsNotNone(children[0].poll())
        with self.assertRaises(ProcessLookupError): os.kill(children[0].pid, 0)
        diagnostic = (self.root / 'state/runtime-failure.json').read_text()
        self.assertEqual(json.loads(diagnostic)['stage'], 'process_lifecycle')
        self.assertNotIn('never-print-me', diagnostic)
        self.assertNotIn('never-print-me', (self.root / 'logs/runtime.log').read_text())

    def test_real_supervisor_forwards_stop_and_reaps_child(self):
        self.configured()
        process = subprocess.Popen([sys.executable, str(HERE / 'ags-runtime.py'), '--root', str(self.root), 'serve'], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            state = self.root / 'state/runtime.json'
            for _ in range(100):
                if state.exists(): break
                if process.poll() is not None: self.fail(process.stderr.read().decode())
                time.sleep(.05)
            pid = json.loads(state.read_text())['pid']
            process.send_signal(signal.SIGTERM)
            process.wait(timeout=10)
            with self.assertRaises(ProcessLookupError): os.kill(pid, 0)
            self.assertIn('stopped_at', json.loads(state.read_text()))
            self.assertNotIn('never-print-me', (self.root / 'logs/runtime.log').read_text())
        finally:
            if process.poll() is None: process.kill(); process.wait()
            process.stdout.close(); process.stderr.close()

if __name__ == '__main__':
    unittest.main()
