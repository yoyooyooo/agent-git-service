#!/usr/bin/env python3
import unittest
import upgrade_adapter_systemd as adapter

class SystemdNativeContractTests(unittest.TestCase):
    def test_scope_is_rendered_as_systemctl_option(self):
        unit={'scope':'system','name':'fixture.service','expected_uid':0,'peer_uid':0,
              'dropin_sha256':'a'*64,'exec_start':'/bin/true'}
        self.assertEqual(adapter.command(unit,'show'), ['systemctl','--system','show','fixture.service'])
        self.assertEqual(adapter.command({**unit,'scope':'user'},'stop'), ['systemctl','--user','stop','fixture.service'])
    def test_factory_injected_runner_never_marks_native_verified(self):
        unit={'scope':'system','name':'fixture.service','expected_uid':0,'peer_uid':0,
              'dropin_sha256':'a'*64,'exec_start':'/bin/true'}
        cap=adapter.create('/tmp', {'native_profile':unit,'native_runner':lambda argv: {}})
        self.assertFalse(cap['native_verified'])

if __name__=='__main__': unittest.main()
