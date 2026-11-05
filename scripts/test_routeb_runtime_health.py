#!/usr/bin/env python3
"""Living-generation checks with real child processes; component evidence only."""
import copy
import hashlib
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

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


if __name__ == '__main__':
    unittest.main()
