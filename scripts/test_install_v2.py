#!/usr/bin/env python3
"""Consumer policy tests. Synthetic transport never claims cryptographic acceptance."""
import argparse
import base64
import contextlib
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import release_install_v2 as v2
import release_verify as verifier

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('tested_installer', HERE / 'install-release.py')
installer = importlib.util.module_from_spec(spec); sys.modules[spec.name] = installer; spec.loader.exec_module(installer)
CONTROL, RUNTIME, TREE = 'c' * 40, 'a' * 40, 'b' * 40
VERSION = 'fork-20260924.9-rc1'


class ConsumerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='ags-consumer-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = {'schema': 'ags.release-source.v1', 'version': VERSION,
                       'runtime': {'revision': RUNTIME, 'tree': TREE},
                       'acceptance': {'runtime_pull_request': 1, 'disclosure': 'fork/PUBLICATION.md'}}
        self.raw = (json.dumps(self.source) + '\n').encode()
        self.assets = []
        names = {'release-source.json', 'install.sh', 'SHA256SUMS'}
        for target in v2.TARGETS:
            names.update({'ags-' + VERSION + '-' + target + '.tar.gz',
                          'build-info-' + target + '.json', 'attestation-' + target + '.sigstore.json'})
        for i, name in enumerate(sorted(names), 1):
            self.assets.append({'id': i, 'name': name, 'size': 1, 'digest': 'sha256:' + 'd' * 64})
        self.release = {'id': 9, 'tag_name': VERSION, 'draft': False, 'immutable': True,
                        'prerelease': True, 'assets': self.assets}
        self.args = argparse.Namespace(version=VERSION, install=False, verify_only=False,
                                       prefix=self.root / 'never-created')

    def api(self, path):
        if '/contents/fork/release-source.json?ref=' in path:
            return {'type': 'file', 'path': 'fork/release-source.json', 'encoding': 'base64',
                    'sha': hashlib.sha1(b'blob ' + str(len(self.raw)).encode() + b'\0' + self.raw).hexdigest(),
                    'content': base64.b64encode(self.raw).decode()}
        if '/git/commits/' in path:
            return {'tree': {'sha': TREE}}
        if '/compare/' in path:
            return {'status': 'ahead', 'merge_base_commit': {'sha': RUNTIME}}
        raise AssertionError(path)

    def test_v2_plan_has_no_download_or_prefix_write_and_marks_pending_stages(self):
        with patch.object(installer, 'api', side_effect=self.api), patch.object(installer, 'execute', side_effect=AssertionError('no child')):
            result = v2.run(installer, self.args, self.release, CONTROL, 'darwin_arm64')
        self.assertFalse(self.args.prefix.exists())
        self.assertEqual((result['source'], result['control_revision'], result['runtime_tree']), (RUNTIME, CONTROL, TREE))
        self.assertEqual(result['verification']['attestation'], 'unverified')
        self.assertEqual(result['verification']['asset_bytes'], 'unverified')

    def test_wrong_tag_control_runtime_tree_and_missing_asset_refuse(self):
        with patch.object(installer, 'api', side_effect=self.api):
            for field, value in [('tag_name', VERSION + 'x'), ('prerelease', False)]:
                with self.subTest(field=field), self.assertRaises(ValueError):
                    v2.run(installer, self.args, {**self.release, field: value}, CONTROL, 'darwin_arm64')
            with self.assertRaises(ValueError):
                v2.run(installer, self.args, self.release, RUNTIME, 'darwin_arm64')
            self.source['runtime']['tree'] = 'e' * 40
            self.raw = json.dumps(self.source).encode()
            with self.assertRaises(ValueError):
                v2.run(installer, self.args, self.release, CONTROL, 'darwin_arm64')
        with self.assertRaises(ValueError):
            v2.inventory(installer, {**self.release, 'assets': self.assets[:-1]}, VERSION)
        self.assertFalse(self.args.prefix.exists())

    def test_sums_rejects_tamper_duplicate_unlisted_and_wrong_order(self):
        names = {'a', 'b', 'SHA256SUMS'}
        for name in ('a', 'b'):
            (self.root / name).write_bytes(name.encode())
        expected = ''.join(installer.digest(self.root / n) + '  ' + n + '\n' for n in ('a', 'b'))
        sums = self.root / 'SHA256SUMS'; sums.write_text(expected)
        v2.check_sums(self.root, names, installer)
        for wrong in (expected + expected, expected.replace('  a', '  ../a'), '\n'.join(reversed(expected.splitlines())) + '\n'):
            sums.write_text(wrong)
            with self.assertRaises(ValueError): v2.check_sums(self.root, names, installer)
        sums.write_text(expected); (self.root / 'a').write_bytes(b'tamper')
        with self.assertRaises(ValueError): v2.check_sums(self.root, names, installer)

    def test_post_gh_certificate_policy_rejects_dispatch_wrong_ref_and_self_hosted(self):
        # Policy unit only, explicitly not a cryptographic acceptance claim.
        source = {'schema': 'ags.release-source.v1', 'runtime': {'revision': RUNTIME, 'tree': TREE}}
        archive = self.root / 'artifact'; archive.write_bytes(b'synthetic')
        bundle = self.root / 'bundle'; bundle.write_bytes(b'synthetic')
        pin = {'schema': verifier.contract.PIN_SCHEMA, 'repository': installer.REPO,
               'repository_id': installer.REPO_ID, 'runtime': source['runtime'],
               'targets': {name: {'manifest_schema': verifier.contract.MANIFEST_SCHEMA,
                                  'abi': verifier.contract.abi_for(name), 'provenance': {
                                      'control_revision': CONTROL, 'signer_digest': CONTROL,
                                      'signer_workflow': verifier.WORKFLOW}} for name in v2.TARGETS}}
        ref = 'refs/tags/' + VERSION
        cert = {'sourceRepositoryDigest': CONTROL, 'buildSignerDigest': CONTROL,
                'sourceRepositoryIdentifier': str(installer.REPO_ID),
                'sourceRepositoryURI': 'https://github.com/' + installer.REPO,
                'sourceRepositoryRef': ref, 'buildTrigger': 'push', 'runnerEnvironment': 'github-hosted',
                'issuer': 'https://token.actions.githubusercontent.com',
                'buildSignerURI': 'https://github.com/' + verifier.WORKFLOW + '@' + ref,
                'runInvocationURI': 'https://github.com/' + installer.REPO + '/actions/runs/1/attempts/1'}
        statement = {'predicateType': verifier.PREDICATE_TYPE, 'predicate': {
            'schema': 'ags.runtime-binding.v1', 'runtime_revision': RUNTIME, 'runtime_tree': TREE},
            'subject': [{'name': archive.name, 'digest': {'sha256': installer.digest(archive)}}]}
        for field, value in [(None, None), ('buildTrigger', 'workflow_dispatch'),
                              ('sourceRepositoryRef', 'refs/heads/main'), ('runnerEnvironment', 'self-hosted'),
                              ('sourceRepositoryIdentifier', '999'), ('buildSignerDigest', RUNTIME)]:
            changed = dict(cert)
            if field: changed[field] = value
            rows = [{'verificationResult': {'statement': statement, 'signature': {'certificate': changed}}}]
            with patch.object(verifier.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, json.dumps(rows).encode(), b'')):
                if field:
                    with self.assertRaises(verifier.Rejected):
                        verifier.verify_release(archive, pin, 'darwin_arm64', bundle=bundle, source=source, version=VERSION)
                else:
                    self.assertEqual(verifier.verify_release(archive, pin, 'darwin_arm64', bundle=bundle, source=source, version=VERSION)['run_id'], '1')

    def test_v1_default_plan_is_read_only_and_retains_runtime_digest_policy(self):
        release = {**self.release, 'assets': [self.assets[0]]}
        release['assets'] = [{'id': 1, 'name': 'ags-' + VERSION + '-darwin_arm64.tar.gz',
                              'size': 1, 'digest': 'sha256:' + 'd' * 64}]
        def api(path):
            if path == 'repos/' + installer.REPO:
                return {'id': installer.REPO_ID, 'private': False}
            if '/releases/tags/' in path: return release
            if '/git/ref/' in path: return {'object': {'type': 'commit', 'sha': RUNTIME}}
            raise AssertionError(path)
        with patch.object(sys, 'argv', ['install-release.py', '--version', VERSION, '--allow-prerelease', '--prefix', str(self.args.prefix)]), patch.object(installer, 'api', side_effect=api), patch.object(installer, 'target', return_value='darwin_arm64'), contextlib.redirect_stdout(io.StringIO()) as output:
            installer.main()
        self.assertEqual(json.loads(output.getvalue())['source'], RUNTIME)
        self.assertFalse(self.args.prefix.exists())


if __name__ == '__main__':
    unittest.main()
