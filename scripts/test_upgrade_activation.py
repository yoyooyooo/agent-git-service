#!/usr/bin/env python3
import hashlib
import json
from pathlib import Path
import tempfile
import unittest

import upgrade_lifecycle
import upgrade_state


class ActivationTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b04-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)
        self.accepted = {'completion_sha256': 'a' * 64, 's4_approval_id': 's4', 'epoch': 1}
        self.approval = {'purpose': 'lifecycle-activate', 'decision': 'approve',
                         'approval_id': 'life-1', 'completion_sha256': 'a' * 64}

    def test_activation_is_durable_and_retry_is_idempotent(self):
        first = upgrade_lifecycle.activate_durable(self.root, self.accepted, self.approval)
        again = upgrade_lifecycle.activate_durable(self.root, self.accepted, self.approval)
        self.assertEqual(first, again)
        projection = upgrade_state.load_verified(self.root)['projection']
        self.assertEqual(projection['authority']['state'], 'active')
        self.assertEqual(projection['authority']['epoch'], 1)

    def test_s4_approval_and_completion_drift_are_rejected_without_effect(self):
        with self.assertRaises(upgrade_lifecycle.LifecycleError) as caught:
            upgrade_lifecycle.activate_durable(self.root, self.accepted,
                                               {**self.approval, 'approval_id': 's4'})
        self.assertEqual(caught.exception.exit_code, 77)
        with self.assertRaises(upgrade_lifecycle.LifecycleError):
            upgrade_lifecycle.activate_durable(self.root, self.accepted,
                                               {**self.approval, 'completion_sha256': 'b' * 64})
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control').exists())


if __name__ == '__main__':
    unittest.main()
