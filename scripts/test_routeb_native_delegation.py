#!/usr/bin/env python3
"""Scope-grant component contracts. Synthetic peers do not prove native entry."""
import copy
from datetime import datetime, timedelta, timezone
from pathlib import Path
import unittest
from unittest.mock import Mock, patch

import routeb_native_delegation as delegation
from upgrade_backup_contract import ContractError, canonical, fingerprint


class DelegationTests(unittest.TestCase):
    def setUp(self):
        self.root = '/home/example/.local/state/ags-routeb-native/fixture-one/ags'
        self.registry = delegation.REGISTRY / 'fixture-one'
        moment = datetime.now(timezone.utc) - timedelta(minutes=5)
        self.recorded = moment.isoformat()
        self.reviewed = (moment + timedelta(minutes=1)).isoformat()
        self.issued = (moment + timedelta(minutes=2)).isoformat()
        self.expires = (moment + timedelta(hours=1)).isoformat()
        self.grant = {
            'schema': delegation.GRANT_SCHEMA, 'grant_id': 'fixture-scope',
            'grantor': 'human:fixture-owner', 'delegate': 'agent:fixture-reviewer',
            'source': {'kind': 'conversation', 'captured_by': 'agent:fixture-reviewer',
                       'messages': [{'text': 'Synthetic scope grant for this test only.', 'sent_at': None}]},
            'scope': 'isolated-native', 'host': 'imile-win', 'host_instance': 'imile-fixture-one',
            'transaction_id': 'fixture-one', 'root': self.root,
            'purposes': ['host-window'], 'recorded_at': self.recorded}
        self.profile = {'scope': 'isolated-native', 'logical_host': 'imile-win',
                        'host_instance': 'imile-fixture-one', 'transaction_id': 'fixture-one', 'root': self.root}
        self.approval = {'schema': delegation.SCHEMAS['host-window'], 'actor': 'agent:fixture-reviewer',
                         'issued_at': self.issued, 'expires_at': self.expires, 'package_sha256': 'a' * 64}
        self.rebind()

    def rebind(self):
        self.raw = {self.registry / 'scope-grant.json': canonical(self.grant),
                    self.registry / 'host-profile.json': canonical(self.profile)}
        grant_sha = fingerprint(self.grant)
        registration = {'scope': 'isolated-native', 'id': 'fixture-one',
                        'binding': {'host_instance': 'imile-fixture-one'},
                        'host_profile': {'sha256': fingerprint(self.profile)},
                        'scope_grant': {'path': str(self.registry / 'scope-grant.json'),
                                        'sha256': grant_sha, 'size': len(canonical(self.grant))}}
        self.raw[self.registry / 'stage0-registration.json'] = canonical(registration)
        self.context = {'root': self.root, 'registration_id': 'fixture-one',
                        'registration_sha256': fingerprint(registration),
                        'scope_grant_sha256': grant_sha, 'operation': 'transaction', 'phase': 'transaction'}
        self.approval['delegation'] = {'scope_grant_sha256': grant_sha,
            'review': {'actor': self.approval['actor'], 'reviewed_at': self.reviewed,
                       'subject_sha256': self.approval['package_sha256'],
                       'evidence_refs': [{'kind': 'independent-review', 'sha256': 'b' * 64}]}}

    def verify(self, **kwargs):
        with patch.object(delegation, '_context', return_value=self.context) as peer, \
                patch.object(delegation, '_protected', side_effect=lambda path: self.raw[path]):
            result = delegation.require_delegation(self.approval, 'host-window', **kwargs)
        peer.assert_called_once_with(kwargs.get('root'), 'host-window')
        return result

    def test_existing_human_scope_and_later_agent_review_remain_distinct(self):
        self.assertEqual(self.verify(root=self.root, host_instance='imile-fixture-one'), self.context)
        self.assertEqual(self.grant['source']['messages'][0]['sent_at'], None)
        self.assertNotEqual(self.grant['grantor'], self.approval['actor'])

    def test_files_and_review_cannot_replace_a_live_native_parent(self):
        with patch.object(delegation, '_context', side_effect=ContractError('stage0_root_peer_required')), \
                patch.object(delegation, '_protected') as read, self.assertRaises(ContractError):
            delegation.require_delegation(self.approval, 'host-window')
        read.assert_not_called()

    def test_production_wrong_host_other_root_and_other_transaction_refuse(self):
        for field, value in [('scope', 'production'), ('host', 'mini'), ('host_instance', 'other'),
                             ('root', '/home/example/.ags'), ('transaction_id', 'other')]:
            original = self.grant[field]
            self.grant[field] = value
            self.rebind()
            with self.subTest(field=field), self.assertRaises(ContractError):
                self.verify()
            self.grant[field] = original
        self.rebind()
        for kwargs in ({'root': '/home/example/.ags'}, {'host_instance': 'other'}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ContractError):
                self.verify(**kwargs)

    def test_unknown_broader_or_duplicate_purposes_refuse(self):
        for purposes in (['*'], ['production-deploy'], ['native-bootstrap'], ['host-window'] * 2):
            self.grant['purposes'] = purposes
            self.rebind()
            with self.subTest(purposes=purposes), self.assertRaises(ContractError):
                self.verify()

    def test_approval_cannot_name_another_host_or_root_inside_a_valid_review(self):
        for key, value in (('host', 'mini'), ('host_instance', 'other'),
                           ('transaction_id', 'other'), ('root', '/home/example/.ags')):
            self.approval[key] = value
            with self.subTest(key=key), self.assertRaisesRegex(ContractError, 'delegation_scope_invalid'):
                self.verify()
            del self.approval[key]

    def test_a_bootstrap_parent_cannot_approve_a_host_window(self):
        self.context['operation'] = 'bootstrap'
        with self.assertRaisesRegex(ContractError, 'delegation_grant_binding'):
            self.verify()

    def test_context_registration_and_grant_digests_are_independent(self):
        for field in ('scope_grant_sha256', 'registration_sha256'):
            original = self.context[field]
            self.context[field] = 'c' * 64
            with self.subTest(field=field), self.assertRaises(ContractError):
                self.verify()
            self.context[field] = original
        self.raw[self.registry / 'scope-grant.json'] += b'\n'
        with self.assertRaisesRegex(ContractError, 'delegation_grant_changed'):
            self.verify()

    def test_agent_must_not_claim_to_be_the_human_approver(self):
        self.approval['actor'] = 'human:fixture-owner'
        with self.assertRaisesRegex(ContractError, 'delegation_actor_invalid'):
            self.verify()

    def test_review_actor_subject_and_original_delegate_are_bound(self):
        for mutation in ({'actor': 'agent:other'}, {'subject_sha256': 'c' * 64}):
            previous = copy.deepcopy(self.approval['delegation']['review'])
            self.approval['delegation']['review'].update(mutation)
            with self.subTest(mutation=mutation), self.assertRaisesRegex(ContractError, 'delegation_review_binding'):
                self.verify()
            self.approval['delegation']['review'] = previous
        self.grant['delegate'] = 'agent:other'
        self.rebind()
        with self.assertRaisesRegex(ContractError, 'delegation_scope_invalid'):
            self.verify()

    def test_review_evidence_must_be_bounded_explicit_and_unique(self):
        for refs in ([], [{'kind': 'review', 'sha256': '0' * 64}],
                     [{'kind': 'review', 'sha256': 'b' * 64}] * 2,
                     [{'kind': 'review', 'sha256': 'b' * 64, 'accepted': True}]):
            self.approval['delegation']['review']['evidence_refs'] = refs
            with self.subTest(refs=refs), self.assertRaises(ContractError):
                self.verify()

    def test_review_does_not_predate_the_grant_or_postdate_issuance(self):
        for reviewed in ((datetime.now(timezone.utc) - timedelta(days=1)).isoformat(), self.expires):
            self.approval['delegation']['review']['reviewed_at'] = reviewed
            with self.subTest(reviewed=reviewed), self.assertRaisesRegex(ContractError, 'delegation_review_time'):
                self.verify()

    def test_original_instruction_time_is_nullable_and_not_invented(self):
        self.grant['source']['messages'][0]['sent_at'] = self.expires
        self.rebind()
        with self.assertRaisesRegex(ContractError, 'delegation_source_invalid'):
            self.verify()

    def test_snapshot_purpose_changes_the_recovery_review_subject(self):
        approval = {'approval_id': 'recovery-one', 'purpose': 'full-restore',
                    'host_instance': 'imile-fixture-one', 'snapshot_id': 'snapshot-one',
                    'manifest_sha256': 'c' * 64, 'data_loss_acknowledged': True,
                    'external_reconciliation_acknowledged': True}
        before = delegation.subject_sha256(approval, 'full-restore')
        approval['purpose'] = 'old-start'
        self.assertNotEqual(before, delegation.subject_sha256(approval, 'old-start'))
        approval['manifest_sha256'] = 'd' * 64
        self.assertNotEqual(before, delegation.subject_sha256(approval, 'full-restore'))


class ExistingApprovalBoundaryTests(unittest.TestCase):
    def test_legacy_host_approval_cannot_smuggle_a_delegation_field(self):
        import test_routeb_host_admission as fixtures
        case = fixtures.AdmissionTests(); case.setUp()
        try:
            case.approval['delegation'] = {}
            with self.assertRaisesRegex(ContractError, 'fields_invalid'):
                case.admission.admit(case.package, case.approval, case.verified, now=datetime.now(timezone.utc))
        finally:
            case.doCleanups()

    def test_delegated_host_approval_requires_exact_registered_transaction(self):
        import routeb_host_admission as admission
        import test_routeb_host_admission as fixtures
        case = fixtures.AdmissionTests(); case.setUp()
        try:
            case.approval.update(schema=delegation.SCHEMAS['host-window'], actor='agent:fixture', delegation={})
            with patch.object(admission, 'require_delegation', return_value={'registration_id': 'other'}), \
                    self.assertRaisesRegex(ContractError, 'transaction_mismatch'):
                admission.admit(case.package, case.approval, case.verified, now=datetime.now(timezone.utc))
        finally:
            case.doCleanups()

    def test_bootstrap_keeps_legacy_human_and_delegated_review_separate(self):
        import routeb_native_bootstrap as bootstrap
        now = datetime.now(timezone.utc)
        value = {'schema': 'ags.routeb-native-bootstrap-approval.v1', 'actor': 'agent:fixture',
                 'purpose': 'native-bootstrap', 'decision': 'approve', 'authorization_id': 'fixture',
                 'issued_at': (now - timedelta(minutes=1)).isoformat(),
                 'expires_at': (now + timedelta(minutes=1)).isoformat()}
        with self.assertRaisesRegex(ContractError, 'bootstrap_approval_invalid'):
            bootstrap.check_human(value, 'native-bootstrap', now)
        value['actor'] = 'human:fixture'
        bootstrap.check_human(value, 'native-bootstrap', now)
        value.update(schema=delegation.SCHEMAS['native-bootstrap'], actor='agent:fixture')
        with patch.object(bootstrap, 'require_delegation', side_effect=ContractError('stage0_root_peer_required')), \
                self.assertRaisesRegex(ContractError, 'stage0_root_peer_required'):
            bootstrap.check_human(value, 'native-bootstrap', now)


if __name__ == '__main__':
    unittest.main()
