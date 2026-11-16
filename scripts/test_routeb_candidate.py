#!/usr/bin/env python3
"""Route B candidate identity. Synthetic bytes only; no production root."""
import hashlib
import json
import os
from pathlib import Path
import tempfile
import unittest


OID = '5154feaf84e4a3fb910be6c720c10024f35020d0'
TREE = 'f74c12b40249a2a37655b05a02e23ceaa2527c33'
BASELINE = 'f63aa88e0c82b1e17174cf07f8033552457185b1'
BASELINE_TREE = '8bf2c52ec3f7c7bc04850791b4e507bbc8261ab4'


def manifest(binary='gh-server', **overrides):
    value = {
        'schema': 'ags.routeb-candidate-manifest.v1',
        'host': 'mini',
        'platform': 'darwin/arm64',
        'binary': binary,
        'sha256': 'pending',
        'built_revision': OID,
        'built_tree': TREE,
        'baseline_revision': BASELINE,
        'baseline_tree_documented': BASELINE_TREE,
        'version': {
            'schema': 'ags.build.v1', 'command': 'gh-server',
            'version': 'source-' + OID, 'revision': OID, 'tree': TREE,
            'goos': 'darwin', 'goarch': 'arm64', 'go_version': 'go1.26.8',
        },
        'toolchain_requested': 'go1.26.8',
        'host_go_version': 'go version go1.26.1 darwin/arm64',
        'cgo_requested': '1',
        'go_build_settings': {},
        'not_a_release': True,
        'not_installed': True,
        'started_against_ags_home': False,
    }
    value.update(overrides)
    return value


def write_pair(root, body=b'candidate-bytes', name='gh-server', **overrides):
    binary = root / name
    binary.parent.mkdir(parents=True, exist_ok=True)
    binary.write_bytes(body)
    binary.chmod(0o755)
    raw = manifest(name, **overrides)
    raw['sha256'] = hashlib.sha256(body).hexdigest()
    path = root / 'manifest.json'
    path.write_bytes(json.dumps(raw, sort_keys=True, separators=(',', ':')).encode() + b'\n')
    return path, raw


class CandidateTests(unittest.TestCase):
    def setUp(self):
        import routeb_candidate as candidate
        self.candidate = candidate
        self.tmp = tempfile.TemporaryDirectory(prefix='routeb-candidate-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def test_relative_binary_is_manifest_parent_not_cwd(self):
        path, raw = write_pair(self.root)
        other = self.root / 'elsewhere'
        other.mkdir()
        (other / 'gh-server').write_bytes(b'different-bytes')
        previous = os.getcwd()
        os.chdir(other)
        self.addCleanup(os.chdir, previous)
        loaded = self.candidate.load_candidate(path, {'revision': OID, 'tree': TREE})
        self.assertEqual(loaded.sha256, raw['sha256'])
        self.assertEqual(loaded.binary_path, (self.root / 'gh-server').resolve())
        self.assertEqual(loaded.raw_manifest_sha256, hashlib.sha256(path.read_bytes()).hexdigest())
        self.assertNotEqual(hashlib.sha256((other / 'gh-server').read_bytes()).hexdigest(), loaded.sha256)

    def test_current_schema_string_is_not_accepted_as_rehearsal_schema(self):
        path, _ = write_pair(self.root)
        raw = json.loads(path.read_text())
        raw['schema'] = 'ags.routeb-candidate.v1'
        path.write_bytes(json.dumps(raw).encode())
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(path, {'revision': OID, 'tree': TREE})
        self.assertEqual(caught.exception.code, 'schema_invalid')

    def test_parent_escape_link_and_duplicate_keys_are_refused(self):
        path, raw = write_pair(self.root, name='nested/gh-server')
        escaped = dict(raw, binary='../gh-server')
        (self.root / 'manifest.json').write_bytes(json.dumps(escaped).encode())
        with self.assertRaises(self.candidate.CandidateError):
            self.candidate.load_candidate(self.root / 'manifest.json', {'revision': OID, 'tree': TREE})
        write_pair(self.root)
        linked = self.root / 'linked'
        linked.symlink_to(self.root / 'gh-server')
        linked_raw = json.loads((self.root / 'manifest.json').read_text())
        linked_raw['binary'] = 'linked'
        (self.root / 'manifest.json').write_bytes(json.dumps(linked_raw).encode())
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(self.root / 'manifest.json', {'revision': OID, 'tree': TREE})
        self.assertEqual(caught.exception.code, 'binary_type_rejected')
        duplicated = b'{"schema":"ags.routeb-candidate-manifest.v1","host":"mini","host":"other"}\n'
        (self.root / 'manifest.json').write_bytes(duplicated)
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(self.root / 'manifest.json', {'revision': OID, 'tree': TREE})
        self.assertEqual(caught.exception.code, 'duplicate_key')

    def test_candidate_platform_must_match_embedded_identity(self):
        """manifest.platform must equal version.goos/goarch. Enum membership is not enough."""
        path, _ = write_pair(self.root, platform='linux/amd64')
        raw = json.loads(path.read_text())
        self.assertEqual(raw['version']['goos'], 'darwin')
        self.assertEqual(raw['version']['goarch'], 'arm64')
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(path, {'revision': OID, 'tree': TREE})
        self.assertEqual(caught.exception.code, 'platform_mismatch')

    def test_version_mismatch_and_expected_review_are_refused(self):
        path, _ = write_pair(self.root)
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(path, {'revision': 'a' * 40, 'tree': TREE})
        self.assertEqual(caught.exception.code, 'review_mismatch')
        raw = json.loads(path.read_text())
        raw['version']['revision'] = 'b' * 40
        path.write_bytes(json.dumps(raw).encode())
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(path, {'revision': OID, 'tree': TREE})
        self.assertEqual(caught.exception.code, 'identity_mismatch')

    def test_history_fields_are_not_authority(self):
        path, _ = write_pair(self.root, not_installed=False, not_a_release=False)
        with self.assertRaises(self.candidate.CandidateError) as caught:
            self.candidate.load_candidate(path, {'revision': OID, 'tree': TREE})
        self.assertIn(caught.exception.code, ('not_a_release_required', 'fields_invalid'))


if __name__ == '__main__':
    unittest.main()
