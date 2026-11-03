#!/usr/bin/env python3
"""M3.3 storage, trash and retention. Trash is not free space; failures are not kept copies."""
import os
from pathlib import Path
import unittest
from unittest.mock import patch

import upgrade_backup as backup
import upgrade_backup_contract as contract
import upgrade_trash
from test_upgrade_backup import BackupTests


class StorageRetentionTests(BackupTests):
    def test_trash_occupancy_is_not_counted_as_free_space(self):
        self.repo()
        group = self.prepared_group()
        trashed = self.backups / 'already-trashed'
        trashed.mkdir(mode=0o700)
        (trashed / 'payload').write_bytes(b'x' * 4096)
        group['capacity']['available_bytes'] = group['capacity']['backup_bytes'] + group['capacity']['restore_copy_bytes'] + group['capacity']['candidate_scratch_bytes'] + group['capacity']['migration_growth_bytes'] + group['capacity']['runtime_reserve_bytes']
        with patch.object(backup, 'available_bytes', return_value=group['capacity']['available_bytes']):
            with patch.object(upgrade_trash, 'move', return_value={'status': 'trashed', 'entry': 'id'}) as moved:
                listed = backup.list_backups(self.backups, group=group, host_instance='fixture')
                self.assertEqual(listed['verified'], [])
                with self.assertRaises(contract.ContractError):
                    backup.preflight_space(group, self.backups, self.restores)
                self.assertEqual(moved.call_count, 0)

    def test_failed_backup_does_not_count_toward_the_five_verified_copies(self):
        self.repo()
        group = self.prepared_group()
        calls = []

        def fake_trash(path):
            calls.append(Path(path).name)

        for index in range(5):
            self.take(group, snapshot_id='keep' + str(index),
                      now='2026-02-0' + str(index + 1) + 'T00:00:00+00:00')
        failed = self.backups / 'failed-one'
        failed.mkdir(mode=0o700)
        (failed / 'failure.json').write_bytes(b'{"state":"failed"}\n')
        backup.apply_retention(self.backups, group=group, host_instance='fixture', trash=fake_trash)
        self.assertEqual(calls, [])
        self.assertTrue(failed.exists())
        for index in range(5):
            self.assertTrue((self.backups / ('keep' + str(index))).exists())

    def test_native_helper_without_registration_cannot_apply_root_owner(self):
        import upgrade_metadata as metadata
        source = self.root / 'source'
        dest = self.root / 'dest'
        source.write_bytes(b'helper')
        dest.write_bytes(b'helper')
        with self.assertRaises(metadata.MetadataError) as caught:
            metadata.materialize(source, dest, {'uid': 0, 'gid': 0, 'owner': 0},
                                 role='tool', transaction='tx', staging=self.root)
        self.assertEqual(caught.exception.code, 'helper_unavailable')
        self.assertNotEqual(dest.stat().st_uid, 0)


if __name__ == '__main__':
    unittest.main()
