#!/usr/bin/env python3
"""M4.1 synthetic full-group restore. The offline plan is not the apply."""
import json
from pathlib import Path
import unittest

import upgrade_backup_contract as contract
from test_upgrade_backup import BackupTests
import upgrade_restore_verify as restore


class FullGroupRestoreTests(BackupTests):
    def sealed_request(self):
        self.repo()
        body = b'full group restore body' * 80
        self.cas(body)
        sealed = self.take()
        restore.verify_sealed_backup(sealed['backup_dir'], group=self.last_group,
                                     dsn='sqlite:' + str(self.dbpath), schema=self.admission,
                                     trash=lambda path: None)
        return {'kind': 'full-restore', 'backup_dir': sealed['backup_dir'],
                'group': self.last_group, 'dsn': 'sqlite:' + str(self.dbpath),
                'schema_sha256': self.admission, 'snapshot_id': sealed['manifest']['snapshot_id'],
                'approval_id': 'restore-approval-1', 'incident_id': 'incident-1'}

    def test_synthetic_full_group_restore_replaces_every_member(self):
        request = self.sealed_request()
        original = (self.root / 'installation').read_bytes()
        (self.root / 'installation').write_bytes(b'changed-after-tq')
        import routeb_full_restore
        result = routeb_full_restore.apply_restore(request)
        self.assertTrue(result['applied'])
        self.assertEqual((self.root / 'installation').read_bytes(), original)
        self.assertFalse((Path(str(self.dbpath) + '-wal')).exists())
        self.assertEqual({item['id'] for item in result['members']},
                         {item['id'] for item in self.last_group['objects']})
        self.assertIn('tool', {item['id'] for item in result['members'] if item.get('provision_separately')})

    def test_mixed_group_and_wrong_snapshot_refuse_before_switch(self):
        request = self.sealed_request()
        import routeb_full_restore
        before = (self.root / 'installation').read_bytes()
        bad = dict(request)
        bad['snapshot_id'] = 'not-the-sealed-snapshot'
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.apply_restore(bad)
        self.assertEqual((self.root / 'installation').read_bytes(), before)

    def test_missing_target_is_recreated_before_restore_is_observed(self):
        request = self.sealed_request()
        target = self.root / 'installation'
        original = target.read_bytes()
        preserved = target.with_name('installation.moved-before-restore')
        target.rename(preserved)
        journal = []
        import routeb_full_restore

        result = routeb_full_restore.apply_restore(request, journal=journal)

        self.assertTrue(result['applied'])
        self.assertTrue(target.is_file())
        self.assertEqual(target.read_bytes(), original)
        self.assertEqual(preserved.read_bytes(), original)
        self.assertFalse(target.with_name('.installation.routeb-restore').exists())
        self.assertIn('installation', result['switched'])
        self.assertEqual([item['status'] for item in journal
                          if item['member'] == 'installation'], ['intent', 'observed'])

    def test_audit_member_is_not_rolled_back(self):
        request = self.sealed_request()
        audit = self.root / 'journal.json'
        audit.write_text('{"kept":true}\n')
        import routeb_full_restore
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.apply_restore({**request, 'include_audit': True})
        self.assertTrue(audit.exists())


if __name__ == '__main__':
    unittest.main()
