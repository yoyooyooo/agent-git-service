#!/usr/bin/env python3
import tempfile
from pathlib import Path
import unittest
import upgrade_lifecycle
import upgrade_state

class EpochChainTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(dir=Path.home(),prefix='s6b10-'); self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve(); (self.root/'state').mkdir(mode=0o700)
    def test_suspend_then_reactivate_requires_new_approval_and_epoch(self):
        accepted={'completion_sha256':'a'*64,'s4_approval_id':'s4','epoch':1}
        approval={'purpose':'lifecycle-activate','decision':'approve','approval_id':'life-1','completion_sha256':'a'*64}
        upgrade_lifecycle.activate_durable(self.root,accepted,approval)
        suspended=upgrade_lifecycle.suspend_durable(self.root,1)
        self.assertEqual(suspended['state'],'suspended')
        new=upgrade_lifecycle.activate_durable(self.root,{**accepted,'epoch':2},
             {**approval,'approval_id':'life-2'})
        self.assertEqual(new['epoch'],2)
        rows=upgrade_state.load_verified(self.root)['events']
        self.assertEqual([r['kind'] for r in rows],['authority_activated','authority_suspended','authority_activated'])
    def test_old_epoch_cannot_be_reactivated(self):
        accepted={'completion_sha256':'a'*64,'s4_approval_id':'s4','epoch':1}
        approval={'purpose':'lifecycle-activate','decision':'approve','approval_id':'life-1','completion_sha256':'a'*64}
        upgrade_lifecycle.activate_durable(self.root,accepted,approval)
        upgrade_lifecycle.suspend_durable(self.root,1)
        with self.assertRaises(upgrade_lifecycle.LifecycleError):
            upgrade_lifecycle.activate_durable(self.root,accepted,{**approval,'approval_id':'life-2'})
if __name__=='__main__': unittest.main()
