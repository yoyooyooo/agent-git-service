#!/usr/bin/env python3
"""Route B host admission. Synthetic package bytes; no production root or manager."""
import hashlib
import json
from datetime import datetime, timedelta, timezone
from pathlib import Path
import tempfile
import unittest

import routeb_candidate as candidate


OID = '5154feaf84e4a3fb910be6c720c10024f35020d0'
TREE = 'f74c12b40249a2a37655b05a02e23ceaa2527c33'
OTHER = '0013aa744c8c90637ec54562ffc248a1bf7c9a52'


def digest(body):
    return hashlib.sha256(body).hexdigest()


def package(candidate_digest, **overrides):
    value = {
        'schema': 'ags.routeb-host-package.v1',
        'host': 'mini',
        'host_instance': 'mini-1',
        'deployment_id': 'dep-1',
        'transaction_id': 'txn-1',
        'attempt_id': 'attempt-1',
        'authorization_id': 'auth-1',
        'candidate_manifest_sha256': candidate_digest,
        'candidate_transaction_id': 'txn-1',
        'candidate_revision': OID,
        'candidate_tree': TREE,
        'tool_sha256': 'ab' * 32,
        'policy_sha256': 'cd' * 32,
        'recovery_group_sha256': 'ef' * 32,
        'native_profile_sha256': '11' * 32,
    }
    value.update(overrides)
    return value


def approval(package_digest, **overrides):
    now = datetime.now(timezone.utc)
    value = {
        'schema': 'ags.routeb-window.v2',
        'authorization_id': 'auth-1',
        'actor': 'human:fixture',
        'issued_at': (now - timedelta(minutes=5)).isoformat(),
        'expires_at': (now + timedelta(hours=1)).isoformat(),
        'decision': 'approve',
        'package_sha256': package_digest,
        'host': 'mini',
        'transaction_id': 'txn-1',
        'start_restores_business': True,
        'actions': ['stop', 'install', 'start'],
    }
    value.update(overrides)
    return value


class AdmissionTests(unittest.TestCase):
    def setUp(self):
        import routeb_host_admission as admission
        self.admission = admission
        self.tmp = tempfile.TemporaryDirectory(prefix='routeb-admit-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        body = b'candidate-bytes'
        (self.root / 'gh-server').write_bytes(body)
        manifest = {
            'schema': 'ags.routeb-candidate-manifest.v1', 'host': 'mini',
            'platform': 'darwin/arm64', 'binary': 'gh-server', 'sha256': digest(body),
            'built_revision': OID, 'built_tree': TREE,
            'baseline_revision': 'f63aa88e0c82b1e17174cf07f8033552457185b1',
            'baseline_tree_documented': '8bf2c52ec3f7c7bc04850791b4e507bbc8261ab4',
            'version': {'schema': 'ags.build.v1', 'command': 'gh-server',
                        'version': 'source-' + OID, 'revision': OID, 'tree': TREE,
                        'goos': 'darwin', 'goarch': 'arm64', 'go_version': 'go1.26.8'},
            'toolchain_requested': 'go1.26.8', 'host_go_version': 'go1.26.1',
            'cgo_requested': '1', 'go_build_settings': {},
            'not_a_release': True, 'not_installed': True, 'started_against_ags_home': False,
        }
        path = self.root / 'manifest.json'
        path.write_bytes(json.dumps(manifest, sort_keys=True).encode())
        self.verified = candidate.load_candidate(path, {'revision': OID, 'tree': TREE})
        self.package = package(self.verified.raw_manifest_sha256)
        self.package_digest = digest(json.dumps(self.package, sort_keys=True).encode())
        self.approval = approval(self.package_digest)

    def test_exact_source_without_release_fields_is_admitted(self):
        admitted = self.admission.admit(self.package, self.approval, self.verified, now=datetime.now(timezone.utc))
        self.assertEqual(admitted.kind, 'exact-source')
        self.assertNotIn('release_id', self.package)
        self.assertEqual(admitted.package_sha256, self.package_digest)

    def test_same_build_other_transaction_or_host_is_refused(self):
        for changed in ({'transaction_id': 'txn-2'}, {'host': 'other'}):
            with self.subTest(changed=changed):
                other = dict(self.package, **changed)
                other_digest = digest(json.dumps(other, sort_keys=True).encode())
                grant = dict(self.approval, package_sha256=other_digest, **changed)
                with self.assertRaises(self.admission.AdmissionError) as caught:
                    self.admission.admit(other, grant, self.verified, now=datetime.now(timezone.utc))
                self.assertIn(caught.exception.code, ('transaction_mismatch', 'host_mismatch', 'package_binding_mismatch'))

    def test_expired_or_replaced_package_is_refused(self):
        expired = dict(self.approval, expires_at=(datetime.now(timezone.utc) - timedelta(minutes=1)).isoformat())
        with self.assertRaises(self.admission.AdmissionError) as caught:
            self.admission.admit(self.package, expired, self.verified, now=datetime.now(timezone.utc))
        self.assertEqual(caught.exception.code, 'approval_expired')
        replaced = dict(self.approval, package_sha256='ff' * 32)
        with self.assertRaises(self.admission.AdmissionError) as caught:
            self.admission.admit(self.package, replaced, self.verified, now=datetime.now(timezone.utc))
        self.assertEqual(caught.exception.code, 'package_replaced')

    def test_release_shaped_package_is_not_an_exact_source_admission(self):
        forged = dict(self.package, release_id=1, release_tag='v1')
        forged_digest = digest(json.dumps(forged, sort_keys=True).encode())
        grant = dict(self.approval, package_sha256=forged_digest)
        with self.assertRaises(self.admission.AdmissionError) as caught:
            self.admission.admit(forged, grant, self.verified, now=datetime.now(timezone.utc))
        self.assertEqual(caught.exception.code, 'fields_invalid')

    def test_duplicate_package_key_is_refused(self):
        raw = json.dumps(self.package, sort_keys=True).encode()
        raw = raw.replace(b'"host": "mini"', b'"host": "mini", "host": "other"', 1)
        with self.assertRaises(self.admission.AdmissionError) as caught:
            self.admission.admit_raw(raw, self.approval, self.verified, now=datetime.now(timezone.utc))
        self.assertEqual(caught.exception.code, 'duplicate_key')


if __name__ == '__main__':
    unittest.main()
