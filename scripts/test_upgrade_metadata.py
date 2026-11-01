#!/usr/bin/env python3
"""S6.05 metadata and Trash. Isolated files only; no production chown or rm."""
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock

import upgrade_backup


class MetadataBaseline(unittest.TestCase):
    def test_unprivileged_copy_of_foreign_owner_is_unsupported(self):
        """Existing apply_metadata. A different uid is the behavior red."""
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='meta-') as directory:
            root = Path(directory).resolve()
            source, dest = root / 'source', root / 'dest'
            source.write_bytes(b'fixture')
            dest.write_bytes(b'fixture')
            metadata = upgrade_backup.supported_metadata(source)
            metadata['uid'] = metadata['uid'] + 1
            with self.assertRaises(upgrade_backup.ContractError) as caught:
                upgrade_backup.apply_metadata(source, dest, metadata)
            self.assertEqual(str(caught.exception), 'ownership_unsupported')
            self.assertEqual(ledger.mock_calls, [])


class MetadataTests(unittest.TestCase):
    def setUp(self):
        import upgrade_metadata as metadata
        import upgrade_trash as trash
        self.metadata, self.trash = metadata, trash
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='meta-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.staging = self.root / 'staging'
        self.staging.mkdir(mode=0o700)
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))

    def test_helper_matches_expected_owner_and_refuses_attacks(self):
        source, dest = self.staging / 'source', self.staging / 'dest'
        source.write_bytes(b'fixture')
        dest.write_bytes(b'fixture')
        expected = {'uid': 0, 'gid': 0, 'owner': 0}
        result = self.metadata.materialize(source, dest, expected, role='tool',
                                           transaction='tx', staging=self.staging)
        self.assertEqual(result['uid'], 0)
        outside = self.root / 'live'
        outside.write_bytes(b'live')
        for kwargs in (
                {'role': 'data'},
                {'role': 'tool', 'source': outside},
                {'role': 'tool', 'destination': outside},
                {'role': 'tool', 'transaction': None}):
            with self.subTest(kwargs=kwargs), self.assertRaises(self.metadata.MetadataError):
                values = {'source': source, 'destination': dest, 'expected': expected,
                          'transaction': 'tx', 'staging': self.staging}
                values.update(kwargs)
                self.metadata.materialize(**values)
        hard = self.staging / 'hard'
        os.link(dest, hard)
        with self.assertRaises(self.metadata.MetadataError):
            self.metadata.materialize(source, dest, expected, role='tool',
                                      transaction='tx', staging=self.staging)

    def test_trash_requires_readback_and_refuses_flags_and_space(self):
        target = self.staging / 'old'
        target.write_bytes(b'old')
        self.assertEqual(self.trash.allow(['/usr/bin/trash', str(target)]),
                         ('/usr/bin/trash', str(target)))
        for argv in (['/usr/bin/trash'], ['/usr/bin/trash', '--', str(target)], ['rm', str(target)]):
            with self.subTest(argv=argv), self.assertRaises(self.trash.TrashError):
                self.trash.allow(argv)
        self.assertEqual(self.trash.move(target, eligible=True,
                                         readback={'source_gone': False})['status'], 'unknown')
        self.assertEqual(self.trash.move(target, eligible=True,
                                         readback={'source_gone': True, 'trash_entry': 'id'})['status'],
                         'trashed')
        with self.assertRaises(self.trash.TrashError):
            self.trash.move(target, eligible=False, readback={'source_gone': True, 'trash_entry': 'id'})
        with self.assertRaises(self.trash.TrashError):
            self.trash.move(target, eligible=True, readback={'source_gone': True, 'trash_entry': 'id'},
                            space_ok=False)


if __name__ == '__main__':
    unittest.main()
