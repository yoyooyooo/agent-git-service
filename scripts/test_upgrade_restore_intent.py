#!/usr/bin/env python3
import tempfile
from pathlib import Path
import unittest

import upgrade_full_restore
import upgrade_state


class RestoreIntentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b06-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve(); (self.root / 'state').mkdir(mode=0o700)

    def _restore(self, old, new, object_id, **extra):
        digest = 'b' * 64
        return upgrade_full_restore.restore_object_durable(
            self.root, old, new, object_id,
            approval_sha256=digest, binding_sha256=digest, transaction_id=object_id,
            expected_approval_sha256=digest, expected_binding_sha256=digest,
            expected_transaction_id=object_id, **extra)

    def test_each_object_switch_is_intent_then_observed(self):
        old = self.root / 'old'; new = self.root / 'new'
        old.write_text('old'); new.write_text('new')
        result = self._restore(old, new, 'obj-1')
        self.assertEqual(result['status'], 'observed')
        self.assertEqual(old.read_text(), 'new')
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertEqual([row['kind'] for row in rows], ['restore_intent', 'restore_observed'])

    def test_unknown_target_is_blocked_without_selecting_old_or_new(self):
        old = self.root / 'old'; new = self.root / 'new'
        old.write_text('old'); new.write_text('new')
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            self._restore(old, new, 'obj-2', fail_after='rename')
        self.assertEqual(caught.exception.code, 'restore_observation_unknown')
        self.assertIn(old.read_text(), ('old', 'new'))
        self.assertEqual(upgrade_state.load_verified(self.root)['projection']['phase'], 'blocked')


if __name__ == '__main__':
    unittest.main()
