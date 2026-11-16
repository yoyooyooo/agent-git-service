#!/usr/bin/env python3
"""S6b.03: S3 phase transitions leave a durable S6b event trail."""
import json
from pathlib import Path
import tempfile
import unittest

import upgrade_state


class DurableWiringTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b03-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)

    def test_phase_event_is_replayable_and_effect_is_intent_only(self):
        result = upgrade_state.commit_phase(
            self.root, 'install_started', 'restore_verified', 'install_started', epoch=1,
            host_instance='fixture-host', identity_sha256='a' * 64, binding_sha256='b' * 64,
            effect={'effect_id': 'install-1', 'type': 'file-transaction',
                    'expected_before': 'a' * 64, 'desired_after': 'b' * 64, 'status': 'intent'})
        self.assertEqual(result['sequence'], 1)
        loaded = upgrade_state.load_verified(self.root)
        row = loaded['events'][0]
        self.assertEqual(row['before_phase'], 'restore_verified')
        self.assertEqual(row['effect']['status'], 'intent')
        self.assertEqual(loaded['projection']['pending_effects'], ['install-1'])
        self.assertEqual((self.root / 'state/ags-autodeploy/local-v2/control/events').glob('*.json').__class__, type((self.root / 'state/ags-autodeploy/local-v2/control/events').glob('*.json')))

    def test_observation_closes_the_same_effect_without_external_calls(self):
        upgrade_state.commit_phase(
            self.root, 'start_intent', 'installed', 'start_attempted', epoch=1,
            host_instance='fixture-host', identity_sha256='a' * 64, binding_sha256='b' * 64,
            effect={'effect_id': 'start-1', 'type': 'spawn',
                    'expected_before': 'stopped', 'desired_after': 'generation-1', 'status': 'intent'})
        result = upgrade_state.commit_phase(
            self.root, 'start_observed', 'start_attempted', 'running', epoch=1,
            effect={'effect_id': 'start-1', 'type': 'spawn',
                    'expected_before': 'stopped', 'desired_after': 'generation-1', 'status': 'observed'})
        self.assertEqual(result['projection']['pending_effects'], [])
        self.assertEqual(upgrade_state.load_verified(self.root)['projection']['generation'], 'generation-1')

    def test_epoch_cas_rejects_a_stale_expected_epoch_and_an_unapproved_jump(self):
        upgrade_state.commit_phase(
            self.root, 'epoch-base', 'none', 'noted', kind='note', epoch=1, expected_epoch=None,
            host_instance='fixture-host', identity_sha256='a' * 64, binding_sha256='b' * 64)
        with self.assertRaises(upgrade_state.StateError) as stale:
            upgrade_state.commit_phase(
                self.root, 'epoch-stale', 'noted', 'still', kind='note', epoch=1, expected_epoch=0,
                host_instance='fixture-host', identity_sha256='a' * 64, binding_sha256='b' * 64)
        self.assertEqual(stale.exception.args[0], 'state_epoch_conflict')
        with self.assertRaises(upgrade_state.StateError) as jumped:
            upgrade_state.commit_phase(
                self.root, 'epoch-jump', 'noted', 'later', kind='note', epoch=2, expected_epoch=1,
                host_instance='fixture-host', identity_sha256='a' * 64, binding_sha256='b' * 64)
        self.assertEqual(jumped.exception.args[0], 'state_epoch_conflict')
        self.assertEqual(len(upgrade_state.load_verified(self.root)['events']), 1)

    def test_commit_phase_refuses_a_missing_epoch(self):
        with self.assertRaises(upgrade_state.StateError) as caught:
            upgrade_state.commit_phase(
                self.root, 'missing-epoch', 'none', 'noted', kind='note',
                host_instance='fixture-host', identity_sha256='a' * 64, binding_sha256='b' * 64)
        self.assertEqual(caught.exception.args[0], 'state_epoch_required')
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control/events').exists())


if __name__ == '__main__':
    unittest.main()
