#!/usr/bin/env python3
"""M6 bundle closure. A host package missing an import or the reviewed interpreter is closed.

Provision stays a read-only plan. A wrong owner or missing native evidence is
not installable and is not a pass.
"""
import os
from pathlib import Path
import tempfile
import unittest

import upgrade_bundle


class BundleClosureTests(unittest.TestCase):
    def test_host_modules_are_required_bundle_members(self):
        for name in ('routeb_host_transaction.py', 'routeb_observation.py'):
            self.assertIn(name, upgrade_bundle.REQUIRED, name)

    def test_missing_import_refuses_the_bundle(self):
        tmp = tempfile.TemporaryDirectory(prefix='routeb-bundle-')
        self.addCleanup(tmp.cleanup)
        source = Path(tmp.name) / 'source'
        source.mkdir()
        for name in upgrade_bundle.REQUIRED:
            if name == 'release-source.json':
                continue
            (source / name).write_text('x\n')
        (source / 'routeb_host_transaction.py').unlink()
        with self.assertRaises(upgrade_bundle.BundleError) as caught:
            upgrade_bundle.build(source, Path(tmp.name) / 'staging')
        self.assertEqual(caught.exception.code, 'member_missing')

    def test_provision_plan_without_native_evidence_is_not_a_pass(self):
        import routeb_provision
        with self.assertRaises(routeb_provision.ProvisionError) as caught:
            routeb_provision.plan({'interpreter': '/usr/bin/python3', 'owner': os.getuid(),
                                   'native_evidence': None, 'exact_head': 'b0b6602f'})
        self.assertEqual(caught.exception.code, 'native_evidence_missing')

    def test_wrong_owner_is_closed(self):
        import routeb_provision
        with self.assertRaises(routeb_provision.ProvisionError) as caught:
            routeb_provision.plan({'interpreter': '/usr/bin/python3', 'owner': 0,
                                   'expected_owner': 501, 'native_evidence': {'passed': True},
                                   'exact_head': 'b0b6602f24d62eaa6597622b87ea06178c4d0607'})
        self.assertEqual(caught.exception.code, 'owner_rejected')


if __name__ == '__main__':
    unittest.main()
