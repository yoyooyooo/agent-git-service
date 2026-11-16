#!/usr/bin/env python3
"""B2: two upgrade chains on the real local-v2 layout, including a cross-process phase.

This is an in-repository fixture. It does not start a service, call a manager,
or claim a host restart. The second process is a real Python process.
"""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

import upgrade_engine
import upgrade_lifecycle
import upgrade_state


def _private(path, root):
    path.mkdir(parents=True, mode=0o700, exist_ok=True)
    current = path
    while current != root:
        os.chmod(current, 0o700)
        current = current.parent


class TwoChainTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b-b2-chain-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.state = self.root / 'state/ags-autodeploy/local-v2'
        _private(self.state / 'pending', self.root)
        _private(self.state / 'accepted', self.root)

    def _transaction(self, authorization_id):
        binding = {'host': 'fixture', 'host_instance': 'fixture-host',
                   'identity_sha256': 'c' * 64, 'authorization_id': authorization_id,
                   'old': 'a' * 40, 'new': 'b' * 40}
        journal = {'schema': 'ags.local-upgrade-journal.v2', 'binding': binding, 'phase': 'none'}
        tx = upgrade_engine.Transaction(
            self.state, binding, {'authorization_id': authorization_id}, object())
        tx.journal = journal
        return tx

    def _activate(self, epoch, approval_id, completion):
        upgrade_lifecycle.activate_durable(
            self.root,
            {'completion_sha256': completion, 's4_approval_id': 's4-' + approval_id,
             'epoch': epoch, 'host_instance': 'fixture-host'},
            {'purpose': 'lifecycle-activate', 'decision': 'approve', 'approval_id': approval_id,
             'completion_sha256': completion, 'host_instance': 'fixture-host'})

    def test_two_chains_and_a_second_process_replay_the_same_phase(self):
        chain_a = self._transaction('auth-A')
        chain_a.checkpoint('admission')
        chain_a.checkpoint('awaiting_observation')
        completion_a = 'a' * 64
        (self.state / 'accepted/completion-a.json').write_text(json.dumps({'completion_sha256': completion_a}))
        os.chmod(self.state / 'accepted/completion-a.json', 0o600)
        self._activate(1, 'life-A', completion_a)
        upgrade_state.commit_phase(
            self.root, 'restart-A', 'active', 'restarted', kind='generation_spawned', epoch=1,
            host_instance='fixture-host', identity_sha256='c' * 64, binding_sha256='b' * 64,
            effect={'effect_id': 'gen-A', 'type': 'spawn', 'expected_before': 'stopped',
                    'desired_after': 'gen-A', 'status': 'observed'})
        upgrade_lifecycle.suspend_durable(self.root, 1)
        chain_b = self._transaction('auth-B')
        chain_b.checkpoint('admission')
        script = r'''
import os, sys
sys.path.insert(0, os.environ["SCRIPTS"])
import upgrade_engine
root = os.environ["ROOT"]
state = upgrade_engine.Path(root) / "state/ags-autodeploy/local-v2"
binding = {"host": "fixture", "host_instance": "fixture-host", "identity_sha256": "c" * 64,
           "authorization_id": "auth-B", "old": "a" * 40, "new": "b" * 40}
tx = upgrade_engine.Transaction(state, binding, {"authorization_id": "auth-B"}, object())
tx.journal = {"phase": "none"}
tx.checkpoint("admission")
print("replayed")
'''
        env = os.environ.copy()
        for name in ('PYTHONDONTWRITEBYTECODE', 'PYTHONPYCACHEPREFIX', 'PYTHONPATH', 'BASH_ENV'):
            env.pop(name, None)
        env['ROOT'] = str(self.root)
        env['SCRIPTS'] = str(Path(upgrade_state.__file__).resolve().parent)
        completed = subprocess.run(
            [str(Path(upgrade_state.__file__).resolve().parent / 'ags-python.sh'), '-c', script],
            check=False, capture_output=True, text=True, env=env, cwd=self.tmp.name)
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn('replayed', completed.stdout)
        chain_b.checkpoint('awaiting_observation')
        completion_b = 'd' * 64
        self._activate(2, 'life-B', completion_b)
        loaded = upgrade_state.load_verified(self.root)
        kinds = [row['kind'] for row in loaded['events']]
        self.assertEqual(kinds.count('authority_activated'), 2)
        self.assertEqual(loaded['projection']['authority']['epoch'], 2)
        self.assertEqual(loaded['projection']['authority']['state'], 'active')
        admissions = [row for row in loaded['events'] if row['after_phase'] == 'admission']
        self.assertEqual(sorted(row['transaction_id'] for row in admissions), ['auth-A', 'auth-B'])
        self.assertEqual(len({row['operation_id'] for row in admissions}), 2)
        replay = [row for row in admissions if row['transaction_id'] == 'auth-B']
        self.assertEqual(len(replay), 1)
        self.assertEqual(upgrade_state.project(self.root)['sequence'], loaded['projection']['sequence'])


if __name__ == '__main__':
    unittest.main()
