#!/usr/bin/env python3
"""M4.3 recovery approval and one old start. A second start is refused."""
import unittest

import upgrade_backup_contract as contract
from test_routeb_full_restore import FullGroupRestoreTests


def approval(snapshot):
    return {'schema': 'ags.routeb-recovery-approval.v1', 'decision': 'approve',
            'purpose': 'old-start', 'approval_id': 'old-start-1',
            'incident_id': 'incident-1', 'snapshot_id': snapshot,
            'data_loss_acknowledged': True, 'external_reconciled': False}


class OldStartTests(FullGroupRestoreTests):
    def test_old_start_requires_a_fresh_approval_and_runs_once(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        restored['journal_root'] = str(self.root / 'journal')
        started = []
        result = routeb_full_restore.old_start(restored, approval(restored['snapshot_id']),
                                               launcher=lambda: started.append('once') or {'started': True, 'pid': 1})
        self.assertEqual(result['started'], True)
        self.assertEqual(started, ['once'])
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.old_start(restored, approval(restored['snapshot_id']),
                                          launcher=lambda: started.append('twice'))
        self.assertEqual(started, ['once'])

    def test_restore_approval_cannot_be_reused_as_old_start(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        restored['journal_root'] = str(self.root / 'journal')
        reused = approval(restored['snapshot_id'])
        reused['approval_id'] = request['approval_id']
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.old_start(restored, reused, launcher=lambda: {'started': True})

    def test_fault_matrix_refuses_missing_member_and_old_wal_mix(self):
        request = self.sealed_request()
        import routeb_full_restore
        missing = dict(request)
        missing['group'] = dict(request['group'])
        missing['group']['objects'] = [item for item in request['group']['objects'] if item['role'] != 'sqlite']
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.apply_restore(missing)
        wal = self.dbpath.with_name(self.dbpath.name + '-wal')
        wal.write_bytes(b'old wal must not merge')
        restored = routeb_full_restore.apply_restore(request)
        self.assertFalse(wal.exists())
        self.assertTrue(wal.with_name(wal.name + '.incident').exists())


if __name__ == '__main__':
    unittest.main()
