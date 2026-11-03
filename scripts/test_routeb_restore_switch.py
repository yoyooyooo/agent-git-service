#!/usr/bin/env python3
"""M4.2 journaled switch. External targets and self-overwrite stay refused."""
from pathlib import Path
import unittest

import upgrade_backup_contract as contract
from test_routeb_full_restore import FullGroupRestoreTests


class RestoreSwitchTests(FullGroupRestoreTests):
    def test_external_target_outside_the_group_is_refused(self):
        request = self.sealed_request()
        outside = self.root.parent / ('outside-' + self.root.name)
        outside.write_bytes(b'not a member')
        self.addCleanup(outside.unlink)
        request['group'] = dict(request['group'])
        request['group']['objects'] = list(request['group']['objects'])
        request['group']['objects'].append({
            'id': 'escaped', 'role': 'config', 'kind': 'file',
            'source': str(outside), 'restore_target': str(outside), 'depends_on': []})
        import routeb_full_restore
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.apply_restore(request)
        self.assertEqual(outside.read_bytes(), b'not a member')

    def test_supervisor_does_not_overwrite_itself(self):
        request = self.sealed_request()
        import routeb_full_restore
        supervisor = self.root / 'routeb_full_restore.py'
        supervisor.write_bytes(b'supervisor bytes')
        with self.assertRaises(contract.ContractError):
            routeb_full_restore.apply_restore(request, supervisor_path=self.root / 'installation')

    def test_each_switch_records_an_effect_before_rename(self):
        request = self.sealed_request()
        journal = []
        import routeb_full_restore
        result = routeb_full_restore.apply_restore(request, journal=journal)
        self.assertTrue(result['applied'])
        self.assertTrue(journal)
        self.assertTrue(any(item['status'] == 'intent' for item in journal))
        self.assertTrue(any(item['status'] == 'observed' for item in journal))
        self.assertTrue(all(item['effect_id'] != request.get('transaction_id') for item in journal))



    def test_parent_directory_is_fsynced_after_rename(self):
        request = self.sealed_request()
        import routeb_full_restore
        calls = []
        original = routeb_full_restore._fsync_dir
        def record(path):
            calls.append(Path(path))
            return original(path)
        routeb_full_restore._fsync_dir = record
        try:
            routeb_full_restore.apply_restore(request)
        finally:
            routeb_full_restore._fsync_dir = original
        self.assertTrue(calls)

if __name__ == '__main__':
    unittest.main()
