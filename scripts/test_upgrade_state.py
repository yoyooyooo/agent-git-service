#!/usr/bin/env python3
"""S6b.01 boundary reproduction against the current owning APIs.

The red assertions name the current defect: an accepted lifecycle dictionary
does not fence the launcher, and an unstamped build has no attributable
identity. A missing module is not the defect these tests report.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent


def load(name):
    """Load a sibling module by file. The runner cwd is not scripts/."""
    path = HERE / name
    spec = importlib.util.spec_from_file_location(path.stem.replace('-', '_'), path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def completion():
    accepted = {'completion_sha256': '', 's4_approval_id': 's4-approval', 'epoch': 1}
    accepted['completion_sha256'] = hashlib.sha256(json.dumps(
        {key: value for key, value in accepted.items() if key != 'completion_sha256'},
        sort_keys=True).encode()).hexdigest()
    return accepted


class BoundaryReproduction(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b01-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)
        self.ledger = Mock()
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))

    def test_accepted_lifecycle_does_not_fence_the_launcher(self):
        """activate() returns an in-memory v1 dictionary. The launcher ignores it."""
        accepted = completion()
        accepted_dir = self.root / 'state/ags-autodeploy/local-v2/accepted-gen'
        accepted_dir.mkdir(parents=True, mode=0o700)
        (accepted_dir / 'completion.json').write_bytes(b'{"schema":"accepted"}')
        os.chmod(accepted_dir / 'completion.json', 0o600)
        approval = {'purpose': 'lifecycle-activate', 'decision': 'approve',
                    'approval_id': 'life-not-durable',
                    'completion_sha256': accepted['completion_sha256']}
        upgrade_lifecycle = load('upgrade_lifecycle.py')
        authority = upgrade_lifecycle.activate(accepted, approval)
        self.assertEqual(authority['schema'], 'ags.runtime-authority.v1')
        self.assertNotIn('durable', authority)
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control').exists())
        # Current defect: no cold marker means startup is permitted, even after
        # an accepted lifecycle activation that was never persisted.
        ags_runtime = load('ags-runtime.py')
        upgrade_recovery = load('upgrade_recovery.py')
        self.assertTrue(ags_runtime.startup_permitted(self.root))
        self.assertTrue(upgrade_recovery.startup_permitted(self.root))

    def test_second_upgrade_is_blocked_by_an_unarchived_marker(self):
        """A leftover cold marker refuses the next begin and is not archived."""
        state = self.root / 'state'
        marker = state / 'upgrade-cold.json'
        marker.write_bytes(b'{"schema":"ags.cold-window.v1"}')
        os.chmod(marker, 0o600)
        pending = state / 'ags-autodeploy/local-v2/pending'
        pending.mkdir(parents=True, mode=0o700)
        os.chmod(pending.parent, 0o700)
        # begin() refuses on this exact predicate before any manager call.
        # require() raises ContractError, which the cold-window owner treats as refusal.
        upgrade_recovery = load('upgrade_recovery.py')
        window = upgrade_recovery.ColdWindow(
            self.root, {}, {}, {}, 'dsn', 'schema', self.ledger, self.ledger, lambda *_: None)
        window.owner = 1
        marker_path = window.state / upgrade_recovery.MARKER
        refused = marker_path.exists() or marker_path.is_symlink()
        self.assertTrue(refused)
        with self.assertRaises(upgrade_recovery.ContractError) as caught:
            upgrade_recovery.require(not refused, 'cold_claim_held')
        self.assertEqual(caught.exception.args[0], 'cold_claim_held')
        self.assertTrue(marker.is_file())
        self.assertFalse(list((state / 'ags-autodeploy/local-v2').glob('history-*')))
        self.assertEqual(self.ledger.mock_calls, [])

    def test_old_start_reports_started_without_a_process_observation(self):
        """started=true is a dictionary flag. There is no pid, boot id or effect."""
        restored = {'status': 'old_restored', 'started': False, 'recovery_approval_id': 'rec',
                    'data_sha256': 'c' * 64, 'tq_sha256': 'c' * 64}
        approval = {'purpose': 'old-start', 'decision': 'approve', 'approval_id': 'start-1'}
        upgrade_recovery_cli = load('upgrade_recovery_cli.py')
        result = upgrade_recovery_cli.old_start(restored, approval)
        self.assertTrue(result['started'])
        self.assertNotIn('pid', result)
        self.assertNotIn('boot_id', result)
        self.assertEqual(self.ledger.mock_calls, [])

    def test_default_buildinfo_is_not_attributable(self):
        """An unstamped binary prints development/unknown. The builder omits the stamps."""
        source = (ROOT / 'internal/buildinfo/buildinfo.go').read_text()
        self.assertIn('var Version = "development"', source)
        builder = (HERE / 'build-exact-source.sh').read_text()
        self.assertIn('-X github.com/ngaut/agent-git-service/server.gitSHA=', builder)
        # Current defect: the ordinary exact-source builder stamps only server.gitSHA.
        self.assertNotIn('internal/buildinfo.Version', builder)
        self.assertNotIn('internal/buildinfo.Revision', builder)
        self.assertNotIn('internal/buildinfo.Tree', builder)
        dockerfile = (ROOT / 'Dockerfile').read_text()
        self.assertNotIn('internal/buildinfo.Version', dockerfile)
        self.assertNotIn('internal/buildinfo.Tree', dockerfile)


if __name__ == '__main__':
    unittest.main()
