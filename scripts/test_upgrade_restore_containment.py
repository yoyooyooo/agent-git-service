#!/usr/bin/env python3
"""B1: restore_object_durable must refuse paths and bindings before any write."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_full_restore
import upgrade_state


class RestoreContainmentTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='s6b-b1-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        (self.root / 'state').mkdir(mode=0o700)
        self.approved = self.root / 'restore'
        self.approved.mkdir(mode=0o700)
        self.target = self.approved / 'object.dat'
        self.source = self.approved / 'object.new'
        self.target.write_bytes(b'old')
        self.source.write_bytes(b'new')
        os.chmod(self.target, 0o600)
        os.chmod(self.source, 0o600)
        self.binding = 'b' * 64
        self.approval = 'c' * 64
        self.transaction = 'obj-1'
        self.ledger = Mock()

    def _call(self, target, source, **extra):
        kwargs = dict(restore_root=self.approved, approval_sha256=self.approval,
                      binding_sha256=self.binding, transaction_id='obj-1',
                      expected_binding_sha256=self.binding,
                      expected_approval_sha256=self.approval,
                      expected_transaction_id='obj-1',
                      effect_ledger=self.ledger)
        kwargs.update(extra)
        return upgrade_full_restore.restore_object_durable(
            self.root, target, source, 'obj-1', **kwargs)


    def test_missing_approval_is_refused_before_any_write(self):
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            upgrade_full_restore.restore_object_durable(self.root, self.target, self.source, 'obj-1')
        self.assertEqual(caught.exception.code, 'restore_approval_required')
        self.assertEqual(self.target.read_bytes(), b'old')
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control/events').exists())

    def test_caller_cannot_move_restore_root_outside_deployment(self):
        outside = Path(tempfile.mkdtemp(dir=self.root.parent, prefix='s6b-b1-sibling-'))
        self.addCleanup(lambda: None)
        sibling_old = outside / 'old'
        sibling_new = outside / 'new'
        sibling_old.write_bytes(b'old')
        sibling_new.write_bytes(b'new')
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            upgrade_full_restore.restore_object_durable(
                self.root, sibling_old, sibling_new, 'obj-1', restore_root=outside,
                approval_sha256=self.approval, binding_sha256=self.binding,
                transaction_id='obj-1', expected_binding_sha256=self.binding,
                expected_approval_sha256=self.approval, expected_transaction_id='obj-1')
        self.assertEqual(caught.exception.code, 'restore_root_outside')
        self.assertEqual(sibling_old.read_bytes(), b'old')
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control/events').exists())

    def test_outside_target_is_refused_before_any_write(self):
        outside = Path(tempfile.mkdtemp(dir=Path.home(), prefix='s6b-b1-tgt-'))
        self.addCleanup(lambda: outside.rmdir() if outside.exists() and not any(outside.iterdir()) else None)
        victim = outside / 'victim.dat'
        victim.write_bytes(b'keep')
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            self._call(victim, self.source)
        self.assertEqual(caught.exception.code, 'restore_target_outside')
        self.assertEqual(victim.read_bytes(), b'keep')
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control/events').exists())
        self.assertEqual(self.ledger.mock_calls, [])

    def test_outside_source_is_refused_before_any_write(self):
        outside = Path(tempfile.mkdtemp(dir=Path.home(), prefix='s6b-b1-src-'))
        self.addCleanup(lambda: outside.rmdir() if outside.exists() and not any(outside.iterdir()) else None)
        foreign = outside / 'foreign.dat'
        foreign.write_bytes(b'secret')
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            self._call(self.target, foreign)
        self.assertEqual(caught.exception.code, 'restore_source_outside')
        self.assertEqual(self.target.read_bytes(), b'old')
        self.assertFalse(list(self.approved.glob('.*.restore-new')))
        self.assertEqual(self.ledger.mock_calls, [])

    def test_dotdot_and_symlink_escape_are_refused(self):
        link = self.approved / 'escape'
        link.symlink_to(self.root.parent)
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            self._call(self.approved / 'escape' / 'victim', self.source)
        self.assertIn(caught.exception.code, ('restore_target_outside', 'restore_target_invalid', 'symlink_forbidden'))
        nested = self.approved / 'down'
        nested.mkdir()
        climbed = nested / '..' / '..' / 'outside-climb.dat'
        with self.assertRaises(upgrade_full_restore.RestoreError):
            self._call(climbed, self.source)
        self.assertFalse((self.root.parent / 'outside-climb.dat').exists())
        self.assertEqual(self.ledger.mock_calls, [])

    def test_hardlink_source_is_refused(self):
        linked = self.approved / 'hard.dat'
        os.link(self.source, linked)
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            self._call(self.target, linked)
        self.assertEqual(caught.exception.code, 'hardlink_forbidden')
        self.assertEqual(self.target.read_bytes(), b'old')
        self.assertEqual(self.ledger.mock_calls, [])

    def test_binding_mismatch_is_refused_before_intent(self):
        with self.assertRaises(upgrade_full_restore.RestoreError) as caught:
            self._call(self.target, self.source, binding_sha256='d' * 64)
        self.assertEqual(caught.exception.code, 'restore_binding_mismatch')
        self.assertEqual(self.target.read_bytes(), b'old')
        self.assertFalse((self.root / 'state/ags-autodeploy/local-v2/control/events').exists())
        self.assertEqual(self.ledger.mock_calls, [])

    def test_approved_switch_fsyncs_and_records_only_inside_root(self):
        import stat
        watched = []
        real_fsync = os.fsync

        def watch(fd):
            info = os.fstat(fd)
            watched.append((info.st_dev, info.st_ino, stat.S_ISDIR(info.st_mode)))
            return real_fsync(fd)

        os.fsync = watch
        self.addCleanup(lambda: setattr(os, 'fsync', real_fsync))
        result = self._call(self.target, self.source)
        self.assertEqual(result['status'], 'observed')
        self.assertEqual(self.target.read_bytes(), b'new')
        target_info = self.target.stat()
        parent_info = self.target.parent.stat()
        self.assertIn((target_info.st_dev, target_info.st_ino, False), watched)
        self.assertIn((parent_info.st_dev, parent_info.st_ino, True), watched)
        rows = upgrade_state.load_verified(self.root)['events']
        self.assertEqual(rows[0]['binding_sha256'], self.binding)
        self.assertEqual(rows[0]['approval_sha256'], self.approval)
        self.assertEqual(rows[0]['transaction_id'], 'obj-1')
        self.assertTrue(self.ledger.mock_calls)
        for call in self.ledger.mock_calls:
            self.assertTrue(all(str(arg).startswith(str(self.approved)) for arg in call.args[1:]))


if __name__ == '__main__':
    unittest.main()
