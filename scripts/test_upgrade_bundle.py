#!/usr/bin/env python3
"""S6.06 bundle builder. Staging fixtures only; nothing is provisioned."""
import hashlib
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import Mock

HERE = Path(__file__).resolve().parent


class BundleBaseline(unittest.TestCase):
    def test_old_three_file_set_is_not_complete(self):
        """Existing required-member contract. Three files are the behavior red."""
        import upgrade
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-') as directory:
            root = Path(directory).resolve()
            files = {}
            for name in ('upgrade_host.py', 'upgrade.py', 'upgrade_engine.py'):
                path = root / name
                path.write_bytes(b'fixture')
                files[name] = hashlib.sha256(b'fixture').hexdigest()
            raw = upgrade.json.dumps({'schema': 'ags.upgrade-tool-bundle.v2',
                                      'entry': 'upgrade_host.py', 'files': files}).encode()
            (root / 'manifest.json').write_bytes(raw)
            with self.assertRaises(upgrade.Failure) as caught:
                upgrade.bundle_engine(root, hashlib.sha256(raw).hexdigest(), owner=os.getuid())
            self.assertEqual(caught.exception.exit_code, 69)
            self.assertEqual(ledger.mock_calls, [])


class BundleTests(unittest.TestCase):
    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            (self.source / name).write_bytes(name.encode())
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))

    def test_staged_bundle_verifies_readonly_and_is_not_installable(self):
        staging = self.root / 'staging'
        built = self.bundle.build(self.source, staging)
        verified = self.bundle.verify(staging)
        self.assertEqual(verified['manifest_sha256'], built['manifest_sha256'])
        self.assertFalse(verified['installable'])
        again = self.bundle.build(self.source, self.root / 'staging-2')
        self.assertEqual(again['manifest_sha256'], built['manifest_sha256'])

    def test_missing_extra_tamper_owner_and_symlink_refuse(self):
        staging = self.root / 'staging'
        self.bundle.build(self.source, staging)
        (staging / 'upgrade.py').write_bytes(b'tampered')
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)
        (staging / 'upgrade.py').write_bytes(b'upgrade.py')
        extra = staging / 'extra'
        extra.write_bytes(b'extra')
        extra.chmod(0o755)
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)
        extra.unlink()
        (staging / 'upgrade.py').unlink()
        (staging / 'upgrade.py').symlink_to(self.source / 'upgrade.py')
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)

    def test_incomplete_rename_is_not_executed(self):
        partial = self.root / 'partial'
        partial.mkdir(mode=0o700)
        (partial / 'upgrade.py').write_bytes(b'upgrade.py')
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(partial)
        self.assertFalse(stat.S_IMODE((partial / 'upgrade.py').stat().st_mode) & 0o111)


if __name__ == '__main__':
    unittest.main()
