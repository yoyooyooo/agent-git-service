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
            self.root, 'install_started', 'restore_verified', 'install_started',
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
            self.root, 'start_intent', 'installed', 'start_attempted',
            effect={'effect_id': 'start-1', 'type': 'spawn',
                    'expected_before': 'stopped', 'desired_after': 'generation-1', 'status': 'intent'})
        result = upgrade_state.commit_phase(
            self.root, 'start_observed', 'start_attempted', 'running',
            effect={'effect_id': 'start-1', 'type': 'spawn',
                    'expected_before': 'stopped', 'desired_after': 'generation-1', 'status': 'observed'})
        self.assertEqual(result['projection']['pending_effects'], [])
        self.assertEqual(upgrade_state.load_verified(self.root)['projection']['generation'], 'generation-1')


if __name__ == '__main__':
    unittest.main()
