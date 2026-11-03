#!/usr/bin/env python3
"""M3.1 Darwin metadata round-trip. Red until a backend captures ACL/xattr values."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import upgrade_backup as backup
import upgrade_backup_contract as contract
from test_upgrade_backup import BackupTests


@unittest.skipUnless(sys.platform == 'darwin', 'Darwin metadata backend')
class DarwinMetadataRoundTrip(BackupTests):
    def test_supported_acl_and_xattr_round_trip_in_a_sealed_backup(self):
        self.repo()
        group = self.prepared_group()
        path = self.root / 'old_binary'
        subprocess.run(['/usr/bin/xattr', '-w', 'com.example.ags-review', 'synthetic', str(path)],
                       check=True)
        subprocess.run(['/bin/chmod', '+a', 'group:staff allow read', str(path)], check=True)
        sealed = self.take(group)
        entry = next(item for item in sealed['manifest']['entries'] if item['path'] == 'old_binary')
        self.assertNotEqual(entry['acl_sha256'], backup.NONE_ACL)
        self.assertNotEqual(entry['xattrs_sha256'], backup.NONE_XATTR)
        copied = Path(sealed['backup_dir']) / 'old_binary'
        self.assertEqual(backup.metadata_fields(backup.supported_metadata(copied)), {
            key: entry[key] for key in
            ('mode', 'uid', 'gid', 'mtime_ns', 'acl_sha256', 'xattrs_sha256')})
        listed = subprocess.run(['/usr/bin/xattr', '-p', 'com.example.ags-review', str(copied)],
                                check=True, capture_output=True, text=True)
        self.assertEqual(listed.stdout.strip(), 'synthetic')

    def test_exact_source_binding_seals_without_inventing_a_release(self):
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
        sealed = backup.create_sealed_backup(
            group, 'sqlite:' + str(self.dbpath), self.admission, binding,
            {'nonce': 'nonce1', 'stop_evidence_sha256': 'a' * 64,
             'writers_evidence_sha256': 'b' * 64}, 'exact-snapshot')
        self.assertEqual(sealed['manifest']['binding']['artifact_kind'], 'exact-source')
        self.assertNotIn('release_id', sealed['manifest']['binding'])
        contract.validate_seal(sealed['manifest_raw'],
                               (Path(sealed['backup_dir']) / 'receipt.json').read_bytes(), group)

    def test_root_owned_member_is_not_silently_copied_by_the_current_user(self):
        import upgrade_metadata as metadata
        self.repo()
        with tempfile.TemporaryDirectory(dir=self.root, prefix='staging-') as directory:
            staging = Path(directory)
            source, dest = staging / 'source', staging / 'dest'
            source.write_bytes(b'root-owned fixture')
            dest.write_bytes(b'root-owned fixture')
            expected = {'uid': 0, 'gid': 0, 'owner': 0, 'mode': 0o755}
            with self.assertRaises(metadata.MetadataError) as caught:
                metadata.materialize(source, dest, expected, role='tool',
                                     transaction='tx-root', staging=staging)
            self.assertEqual(caught.exception.code, 'helper_unavailable')
            self.assertNotEqual(dest.stat().st_uid, 0)


if __name__ == '__main__':
    unittest.main()
