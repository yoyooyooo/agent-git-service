#!/usr/bin/env python3
"""M5 host transaction entry. Synthetic fixtures only; no production root or label.

A plan without a private package must have no effects. The default rehearsal
entry stays isolated and still refuses a real production root.
"""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from test_routeb_full_restore import FullGroupRestoreTests
import routeb_candidate as candidate
import routeb_host_admission as admission

ROOT = Path(__file__).resolve().parent
ENTRY = ROOT / 'ags-maintenance-transaction'


class HostModeTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='routeb-host-txn-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def test_host_plan_without_package_has_no_effects(self):
        completed = subprocess.run(
            [str(ENTRY), '--mode', 'host', 'plan', '--host', 'mini'],
            cwd=self.root, check=False, text=True, capture_output=True,
        )
        self.assertEqual(completed.returncode, 78, completed.stderr)
        self.assertIn('package_missing', completed.stderr)
        self.assertNotIn('effects', completed.stdout)
        self.assertEqual(list(self.root.iterdir()), [])

    def test_default_mode_still_refuses_a_production_shaped_root(self):
        home = self.root / 'home'
        home.mkdir()
        production = home / '.ags'
        production.mkdir()
        env = os.environ.copy()
        env['HOME'] = str(home)
        completed = subprocess.run(
            [str(ENTRY), 'plan', '--host', 'mini', '--root', str(production),
             '--candidate', str(self.root / 'missing'), '--window', str(self.root / 'missing'),
             '--restore', str(self.root / 'restore'), '--manager-dir', str(self.root / 'manager'),
             '--audit', str(self.root / 'audit'), '--external-contract', str(self.root / 'contract')],
            cwd=self.root, check=False, text=True, capture_output=True, env=env,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn('production_root_refused', completed.stderr)
        self.assertFalse((production / 'state').exists())

    def test_host_mode_rejects_manager_dir_and_fault_hook(self):
        completed = subprocess.run(
            [str(ENTRY), '--mode', 'host', 'run', '--execute', '--manager-dir', str(self.root),
             '--fault-after', 'stopped'],
            cwd=self.root, check=False, text=True, capture_output=True,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertTrue('manager_dir_refused' in completed.stderr or 'fault_hook_refused' in completed.stderr)
        self.assertEqual(list(self.root.iterdir()), [])


def digest(body):
    return hashlib.sha256(body).hexdigest()


class HostChainTests(FullGroupRestoreTests):
    """An admitted isolated chain must really seal, verify and start once."""

    def chain(self):
        request = self.sealed_request()
        body = b'candidate-bytes'
        binary = self.root / 'candidate-binary'
        binary.write_bytes(body)
        oid = '5154feaf84e4a3fb910be6c720c10024f35020d0'
        tree = 'f74c12b40249a2a37655b05a02e23ceaa2527c33'
        manifest = {
            'schema': 'ags.routeb-candidate-manifest.v1', 'host': 'mini',
            'platform': 'darwin/arm64', 'binary': 'candidate-binary', 'sha256': digest(body),
            'built_revision': oid, 'built_tree': tree,
            'baseline_revision': 'f63aa88e0c82b1e17174cf07f8033552457185b1',
            'baseline_tree_documented': '8bf2c52ec3f7c7bc04850791b4e507bbc8261ab4',
            'version': {'schema': 'ags.build.v1', 'command': 'gh-server', 'version': 'source-' + oid,
                        'revision': oid, 'tree': tree, 'goos': 'darwin', 'goarch': 'arm64',
                        'go_version': 'go1.26.8'},
            'toolchain_requested': 'go1.26.8', 'host_go_version': 'go1.26.1',
            'cgo_requested': '1', 'go_build_settings': {},
            'not_a_release': True, 'not_installed': True, 'started_against_ags_home': False,
        }
        path = self.root / 'candidate-manifest.json'
        path.write_bytes(json.dumps(manifest, sort_keys=True).encode())
        verified = candidate.load_candidate(path, {'revision': oid, 'tree': tree})
        package = {
            'schema': 'ags.routeb-host-package.v1', 'host': 'mini', 'host_instance': 'mini-1',
            'deployment_id': 'dep-1', 'transaction_id': 'txn-1', 'attempt_id': 'attempt-1',
            'authorization_id': 'auth-1', 'candidate_manifest_sha256': verified.raw_manifest_sha256,
            'candidate_transaction_id': 'txn-1', 'candidate_revision': oid, 'candidate_tree': tree,
            'tool_sha256': 'ab' * 32, 'policy_sha256': 'cd' * 32,
            'recovery_group_sha256': 'ef' * 32, 'native_profile_sha256': '11' * 32,
        }
        raw = json.dumps(package, sort_keys=True).encode()
        grant = {
            'schema': 'ags.routeb-window.v2', 'authorization_id': 'auth-1', 'actor': 'human:fixture',
            'issued_at': '2026-11-03T00:00:00+00:00', 'expires_at': '2026-11-03T02:00:00+00:00',
            'decision': 'approve', 'package_sha256': digest(raw), 'host': 'mini',
            'transaction_id': 'txn-1', 'start_restores_business': True,
            'actions': ['stop', 'install', 'start'],
        }
        admitted = admission.admit(package, grant, verified, now=__import__('datetime').datetime(2026, 11, 3, 1, tzinfo=__import__('datetime').timezone.utc), raw=raw)
        return {'request': request, 'admitted': admitted, 'package': package, 'approval': grant,
                'verified': verified, 'binary': binary}

    def test_admitted_chain_seals_verifies_and_starts_once(self):
        import routeb_host_transaction as host
        built = self.chain()
        calls = []

        def manager(name):
            calls.append(name)
            if name == 'start':
                return {'started': True, 'generation': 'gen-1'}
            return {'stopped': True}

        result = host.execute_chain(built, manager=manager, clock=lambda: 0,
                                     now=__import__('datetime').datetime(2026, 11, 3, 1, tzinfo=__import__('datetime').timezone.utc))
        self.assertEqual(calls.count('start'), 1)
        self.assertIn('sealed', result['phases'])
        self.assertIn('restore_verified', result['phases'])
        self.assertTrue((Path(built['request']['backup_dir']) / 'restore-report.json').is_file())

    def test_unknown_start_is_not_replayed(self):
        import routeb_host_transaction as host
        built = self.chain()
        calls = []

        def manager(name):
            calls.append(name)
            if name == 'start':
                return None
            return {'stopped': True}

        with self.assertRaises(host.HostFailure) as caught:
            host.execute_chain(built, manager=manager, clock=lambda: 0,
                               now=__import__('datetime').datetime(2026, 11, 3, 1, tzinfo=__import__('datetime').timezone.utc))
        self.assertEqual(caught.exception.code, 'start_unknown')
        with self.assertRaises(host.HostFailure) as caught:
            host.execute_chain(built, manager=manager, clock=lambda: 1,
                               now=__import__('datetime').datetime(2026, 11, 3, 1, tzinfo=__import__('datetime').timezone.utc))
        self.assertEqual(caught.exception.code, 'start_unknown')
        self.assertEqual(calls.count('start'), 1)

    def test_expired_approval_stops_before_stop(self):
        import routeb_host_transaction as host
        built = self.chain()
        calls = []
        now = __import__('datetime').datetime(2026, 11, 3, 1, tzinfo=__import__('datetime').timezone.utc)
        later = __import__('datetime').datetime(2026, 11, 4, tzinfo=__import__('datetime').timezone.utc)
        seen = {'n': 0}

        def clock_now():
            seen['n'] += 1
            return now if seen['n'] < 2 else later

        with self.assertRaises(host.HostFailure) as caught:
            host.execute_chain(built, manager=lambda name: calls.append(name) or {'stopped': True},
                               clock=lambda: 0, now_fn=clock_now)
        self.assertEqual(caught.exception.code, 'approval_expired')
        self.assertNotIn('stop', calls)


if __name__ == '__main__':
    unittest.main()
