#!/usr/bin/env python3
"""M4.3 recovery approval and one old start. A second start is refused."""
import unittest
from pathlib import Path
from datetime import datetime, timedelta, timezone

import upgrade_backup_contract as contract
from test_routeb_full_restore import FullGroupRestoreTests


def approval(snapshot, restored=None):
    value = {'schema': 'ags.routeb-recovery-approval.v1', 'decision': 'approve',
            'purpose': 'old-start', 'approval_id': 'old-start-1',
            'incident_id': 'incident-1', 'snapshot_id': snapshot,
            'data_loss_acknowledged': True, 'external_reconciled': False}
    if restored is not None:
        now = datetime.now(timezone.utc)
        value.update(manifest_sha256=restored['recovery_context']['manifest_sha256'],
                     host_instance=restored['recovery_context']['host_instance'], actor='human:fixture',
                     issued_at=(now-timedelta(minutes=1)).isoformat(), expires_at=(now+timedelta(minutes=5)).isoformat())
    return value


class OldStartTests(FullGroupRestoreTests):
    def test_old_start_requires_a_fresh_approval_and_runs_once(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        started = []
        def launcher():
            marker = Path(restored['journal_root']) / '.old-start-old-start-1'
            consumed = contract.strict_json(marker.read_bytes())
            self.assertEqual(consumed['manifest_sha256'], restored['recovery_context']['manifest_sha256'])
            self.assertEqual(consumed['snapshot_id'], restored['snapshot_id'])
            started.append('once')
            return {'started': True, 'pid': 1}
        result = routeb_full_restore.old_start(restored, approval(restored['snapshot_id'], restored),
                                               launcher=launcher)
        self.assertEqual(result['started'], True)
        self.assertEqual(started, ['once'])
        restored['journal_root'] = str(self.root / 'another-private-journal')
        with self.assertRaisesRegex(contract.ContractError, 'recovery_journal_binding_mismatch'):
            routeb_full_restore.old_start(restored, approval(restored['snapshot_id'], restored),
                                          launcher=lambda: started.append('other-directory'))
        self.assertEqual(started, ['once'])
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.old_start(restored, approval(restored['snapshot_id'], restored),
                                          launcher=lambda: started.append('twice'))
        self.assertEqual(started, ['once'])

    def test_unknown_old_start_is_consumed_and_does_not_retry(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        started = []
        approved = approval(restored['snapshot_id'], restored)
        for expected in ('start_observation_unknown', 'old_start_already_consumed'):
            with self.subTest(expected=expected), self.assertRaisesRegex(contract.ContractError, expected):
                routeb_full_restore.old_start(restored, approved,
                                             launcher=lambda: started.append('unknown'))
        self.assertEqual(started, ['unknown'])

    def test_missing_journal_root_cannot_consume_approval_in_the_current_directory(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        restored.pop('journal_root')
        started = []
        with self.assertRaisesRegex(contract.ContractError, 'recovery_journal_required'):
            routeb_full_restore.old_start(restored, approval(restored['snapshot_id'], restored),
                                         launcher=lambda: started.append('must not start'))
        self.assertEqual(started, [])

    def test_restore_approval_cannot_be_reused_as_old_start(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        reused = approval(restored['snapshot_id'])
        reused['approval_id'] = request['approval_id']
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.old_start(restored, reused, launcher=lambda: {'started': True})

    def test_old_start_refuses_changed_separately_provisioned_launcher(self):
        request = self.sealed_request()
        import routeb_full_restore
        restored = routeb_full_restore.apply_restore(request)
        launcher = self.root / 'launcher'
        launcher.write_bytes(launcher.read_bytes() + b'changed after restore')
        started = []
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.old_start(restored, approval(restored['snapshot_id'], restored),
                                          launcher=lambda: started.append('must not start'))
        self.assertEqual(started, [])

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
