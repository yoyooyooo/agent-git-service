#!/usr/bin/env python3
import hashlib
import json
import os
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

    def _approve(self, old, new, object_id):
        body = {
            'schema': 'ags.restore-approval.v1', 'decision': 'approve',
            'transaction_id': object_id, 'object_id': object_id,
            'host_instance': 'fixture-host', 'identity_sha256': 'a' * 64,
            'binding_sha256': 'b' * 64, 'target_group': [str(old)],
            'object_digest': hashlib.sha256(new.read_bytes()).hexdigest(),
            'before_digest': hashlib.sha256(old.read_bytes()).hexdigest(),
        }
        raw = json.dumps(body, sort_keys=True, separators=(',', ':')).encode()
        approvals = self.root / 'state/ags-autodeploy/local-v2/control/approvals'
        approvals.mkdir(parents=True, mode=0o700)
        for path in (self.root / 'state', self.root / 'state/ags-autodeploy',
                     self.root / 'state/ags-autodeploy/local-v2', approvals.parent, approvals):
            os.chmod(path, 0o700)
        digest = hashlib.sha256(raw).hexdigest()
        path = approvals / (digest + '.json')
        path.write_bytes(raw)
        os.chmod(path, 0o600)
        host = approvals.parent / 'host.json'
        host.write_bytes(json.dumps({'schema': 'ags.host-record.v1', 'verified': True,
                                     'host_instance': 'fixture-host', 'identity_sha256': 'a' * 64},
                                    sort_keys=True, separators=(',', ':')).encode())
        os.chmod(host, 0o600)
        return digest

    def _restore(self, old, new, object_id, **extra):
        return upgrade_full_restore.restore_object_durable(
            self.root, old, new, object_id, approval_ref=self._approve(old, new, object_id), **extra)

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
