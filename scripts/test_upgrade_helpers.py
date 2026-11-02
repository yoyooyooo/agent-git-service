#!/usr/bin/env python3
import tempfile
from pathlib import Path
import unittest

import upgrade_metadata
import upgrade_trash

class HelperContractTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(dir=Path.home(),prefix='s6b09-'); self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve(); self.stage=self.root/'stage'; self.stage.mkdir(mode=0o700)
        self.source=self.stage/'tool'; self.source.write_text('tool'); self.source.chmod(0o600)
        self.dest=self.stage/'dest'; self.dest.write_text('dest'); self.dest.chmod(0o600)
        self.expected={'owner':0,'uid':0,'gid':0}
    def test_metadata_without_provisioned_helper_refuses(self):
        with self.assertRaises(upgrade_metadata.MetadataError) as caught:
            upgrade_metadata.materialize(self.source,self.dest,self.expected,role='tool',transaction='tx',staging=self.stage)
        self.assertEqual(caught.exception.code,'helper_unavailable')
    def test_trash_without_provisioned_mover_refuses(self):
        with self.assertRaises(upgrade_trash.TrashError) as caught:
            upgrade_trash.move(self.source,eligible=True,readback={'source_gone':True,'trash_entry':'x'})
        self.assertEqual(caught.exception.code,'trash_unavailable')

if __name__=='__main__': unittest.main()
