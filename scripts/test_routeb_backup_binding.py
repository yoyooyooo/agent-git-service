#!/usr/bin/env python3
"""Exact-source backup binding dispatch. v1 Release bindings stay closed."""
import copy
import unittest

import upgrade_backup_contract as contract


def release_binding():
    value = {key: 'e' * 64 if key.endswith('_sha256') else 'fixture' for key in contract.BINDING_FIELDS}
    value.update({key: 'f' * 40 for key in ('old', 'new', 'runtime_tree', 'control_revision', 'control_tree')})
    value.update(release_id=1, release_tag='v1', target='darwin_arm64')
    return value


def exact_binding():
    return {
        'artifact_kind': 'exact-source',
        'host_instance': 'mini-1', 'deployment_id': 'dep-1', 'transaction_id': 'txn-1',
        'attempt_id': 'attempt-1', 'authorization_id': 'auth-1',
        'old': '0' * 39 + '1', 'new': '5' * 40, 'runtime_tree': 'f' * 40,
        'manifest_sha256': 'a' * 64, 'build_review_sha256': 'b' * 64,
        'ci_evidence_sha256': 'c' * 64, 'tool_sha256': 'd' * 64, 'policy_sha256': 'e' * 64,
        'target': 'darwin_arm64',
    }


class BindingDispatchTests(unittest.TestCase):
    def test_executable_exact_binding_requires_hashes_and_explicit_schema(self):
        value = {**exact_binding(), 'schema': contract.EXACT_COLD_SCHEMA,
                 'old_binary_sha256': '1' * 64, 'new_binary_sha256': '2' * 64}
        self.assertEqual(contract.validate_cold_binding(value), value)
        self.assertEqual(contract.validate_binding_version(value), value)
        for change in ({'schema': 'unknown'}, {'new_binary_sha256': '0' * 64},
                       {'release_id': 1}):
            with self.subTest(change=change), self.assertRaises(contract.ContractError):
                contract.validate_cold_binding({**value, **change})
        with self.assertRaises(contract.ContractError):
            contract.validate_cold_binding(exact_binding())

    def test_v1_release_binding_still_requires_release_fields(self):
        contract.validate_binding(release_binding())
        forged = release_binding()
        del forged['release_id']
        forged['artifact_kind'] = 'exact-source'
        with self.assertRaises(contract.ContractError):
            contract.validate_binding(forged)

    def test_exact_source_binding_is_a_separate_kind(self):
        accepted = contract.validate_binding_v2(exact_binding())
        self.assertEqual(accepted['artifact_kind'], 'exact-source')
        self.assertNotIn('release_id', accepted)

    def test_exact_source_missing_review_evidence_is_refused(self):
        missing = exact_binding()
        del missing['build_review_sha256']
        with self.assertRaises(contract.ContractError) as caught:
            contract.validate_binding_v2(missing)
        self.assertIn('fields', str(caught.exception))

    def test_v1_consumer_does_not_accept_exact_source_identity(self):
        with self.assertRaises(contract.ContractError):
            contract.validate_binding(exact_binding())

    def test_kind_dispatch_refuses_a_mixed_binding(self):
        mixed = exact_binding()
        mixed['release_id'] = 1
        mixed['release_tag'] = 'v1'
        with self.assertRaises(contract.ContractError) as caught:
            contract.validate_binding_version(mixed)
        self.assertIn(str(caught.exception), ('fields_invalid', 'kind_invalid'))


if __name__ == '__main__':
    unittest.main()
