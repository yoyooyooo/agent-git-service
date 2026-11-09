#!/usr/bin/env python3
"""Native identity validation only; no registry or manager is provisioned."""
from pathlib import Path
import unittest

import routeb_host_identity as native
import upgrade_identity as identity
from upgrade_backup_contract import ContractError, canonical


class NativeIdentityTests(unittest.TestCase):
    def fixture(self):
        home = '/fixture/home'
        root = Path(home) / '.local/state/ags-routeb-native/native-case/ags'
        facts = identity.MachineFacts('mini', 501, 501, 20, 'yoyo', home,
                                      'Darwin', 'arm64', 'ioplatformuuid', 'a' * 64)
        value = {'schema': native.SCHEMA, 'logical_host': 'mini', 'host_instance': 'native-mini',
                 'uid': 501, 'gid': 20, 'user': 'yoyo', 'home': home, 'root': str(root),
                 'platform': 'Darwin', 'arch': 'arm64', 'machine_id_kind': 'ioplatformuuid',
                 'machine_id_sha256': 'a' * 64, 'aliases': ['mini'],
                 'scope': 'isolated-native', 'transaction_id': 'native-case',
                 'tool_bundle': str(native.REGISTRY / 'native-case/bundle')}
        return facts, root, value

    def test_registered_native_identity_keeps_machine_facts_and_separate_root(self):
        facts, root, value = self.fixture()
        result = native.parse_native_profile(canonical(value), facts, root)
        self.assertEqual(result.scope, 'isolated-native')
        self.assertEqual(result.identity.root, str(root))
        self.assertEqual(result.identity.machine_id_sha256, facts.machine_id_sha256)
        self.assertEqual(result.profile, native.REGISTRY / 'native-case/host-profile.json')
        self.assertEqual(result.bundle, native.REGISTRY / 'native-case/bundle')

    def test_production_root_other_transaction_and_unregistered_bundle_refuse(self):
        facts, root, value = self.fixture()
        for changes in ({'root': str(Path(facts.home) / '.ags')}, {'transaction_id': 'other'},
                        {'scope': 'production'}, {'tool_bundle': str(Path(facts.home) / 'untrusted')},
                        {'machine_id_sha256': 'b' * 64}, {'uid': 0}):
            with self.subTest(changes=changes), self.assertRaises((ContractError, identity.IdentityError)):
                native.parse_native_profile(canonical({**value, **changes}), facts, root)
        for path in (Path(facts.home) / '.ags', root / 'nested',
                     Path('/tmp/ags-routeb-native/native-case/ags')):
            with self.subTest(root=path), self.assertRaises(ContractError):
                native.native_locations(path, facts.home)


if __name__ == '__main__':
    unittest.main()
