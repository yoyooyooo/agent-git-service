#!/usr/bin/env python3
"""M3.2 independent restore must reject a self-consistent but damaged payload."""
import sqlite3
import unittest
from pathlib import Path

import upgrade_backup_contract as contract
import upgrade_restore_verify as restore
from test_upgrade_backup import BackupTests
from test_upgrade_inventory import fixture_db


class RestoreVerifyExtensions(BackupTests):
    def test_exact_source_seal_is_accepted_by_independent_restore(self):
        self.repo()
        group = self.prepared_group()
        binding = {
            'artifact_kind': 'exact-source',
            'host_instance': 'mini-1', 'deployment_id': 'dep-1', 'transaction_id': 'txn-1',
            'attempt_id': 'attempt-1', 'authorization_id': 'auth-1',
            'old': '0' * 39 + '1', 'new': '5' * 40, 'runtime_tree': 'f' * 40,
            'manifest_sha256': 'a' * 64, 'build_review_sha256': 'b' * 64,
            'ci_evidence_sha256': 'c' * 64, 'tool_sha256': 'd' * 64, 'policy_sha256': 'e' * 64,
            'target': 'darwin_arm64',
        }
        import upgrade_backup as backup
        sealed = backup.create_sealed_backup(
            group, 'sqlite:' + str(self.dbpath), self.admission, binding,
            {'nonce': 'nonce1', 'stop_evidence_sha256': 'a' * 64,
             'writers_evidence_sha256': 'b' * 64}, 'exact-restore')
        calls = []
        report = restore.verify_sealed_backup(sealed['backup_dir'], group=group,
                                              dsn='sqlite:' + str(self.dbpath), schema=self.admission,
                                              trash=calls.append)
        self.assertTrue(all(report['checks'].values()))
        self.assertEqual(set(report['checks']), set(restore.CHECKS))
        self.assertEqual(len(calls), 1)
        self.assertEqual(report['snapshot_id'], 'exact-restore')

    def test_metadata_sidecar_digest_must_match_the_restored_copy(self):
        self.repo()
        group = self.prepared_group()
        import subprocess
        import sys
        path = self.root / 'old_binary'
        if sys.platform != 'darwin':
            self.skipTest('Darwin metadata sidecar')
        subprocess.run(['/usr/bin/xattr', '-w', 'com.example.ags-review', 'synthetic', str(path)], check=True)
        subprocess.run(['/bin/chmod', '+a', 'group:staff allow read', str(path)], check=True)
        sealed = self.take(group)
        calls = []
        report = restore.verify_sealed_backup(sealed['backup_dir'], group=group,
                                              dsn='sqlite:' + str(self.dbpath), schema=self.admission,
                                              trash=calls.append)
        restored = calls[0] / 'old_binary'
        listed = subprocess.run(['/usr/bin/xattr', '-p', 'com.example.ags-review', str(restored)],
                                check=True, capture_output=True, text=True)
        self.assertEqual(listed.stdout.strip(), 'synthetic')
        self.assertTrue(report['checks']['manifest'])


if __name__ == '__main__':
    unittest.main()
