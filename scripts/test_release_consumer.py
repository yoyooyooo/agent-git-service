#!/usr/bin/env python3
"""Complete offline v2 consumer chain. Synthetic transport is not Sigstore evidence."""
import argparse
import base64
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

import release_install_v2 as v2
import release_verify as verifier

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location('consumer_installer', HERE / 'install-release.py')
installer = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = installer
spec.loader.exec_module(installer)

CONTROL, RUNTIME, TREE = 'c' * 40, 'a' * 40, 'b' * 40
VERSION = 'fork-20260924.9-rc1'


def source_document():
    return {'schema': 'ags.release-source.v1', 'version': VERSION,
            'runtime': {'revision': RUNTIME, 'tree': TREE},
            'acceptance': {'runtime_pull_request': 1, 'disclosure': 'fork/PUBLICATION.md'}}


def archive(directory, target, source):
    runtime = source['runtime']
    binaries = {}
    for command in installer.COMMANDS:
        data = (target + ':' + command).encode()
        identity = {'schema': 'ags.build.v1', 'command': command, 'version': VERSION, **runtime,
                    'goos': target.split('_')[0], 'goarch': target.split('_')[1], 'go_version': 'go1.26.8'}
        binaries[command] = {'sha256': hashlib.sha256(data).hexdigest(), 'size': len(data),
                             'cgo_enabled': command == 'gh-server', 'identity': identity}
    info = {'schema': installer.MANIFEST_SCHEMA, 'repository': installer.REPO,
            'repository_id': installer.REPO_ID, 'version': VERSION, 'target': target,
            'revision': RUNTIME, 'tree': TREE, 'abi': verifier.contract.abi_for(target),
            'toolchain': {'go_version': 'go1.26.8', 'pin_path': '.go-version'},
            'minimum_platform': 'macOS 15 (Apple Silicon)' if target.startswith('darwin') else 'Ubuntu 22.04',
            'binaries': binaries}
    path = directory / ('ags-' + VERSION + '-' + target + '.tar.gz')
    with tarfile.open(path, 'w:gz') as stream:
        for name, raw in {'LICENSE': b'license', 'NOTICE': b'notice', 'build-info.json': (json.dumps(info) + '\n').encode()}.items():
            member = tarfile.TarInfo(name); member.size = len(raw); stream.addfile(member, io.BytesIO(raw))
        for command, binary in binaries.items():
            raw = (target + ':' + command).encode()
            member = tarfile.TarInfo('bin/' + command); member.size = len(raw); member.mode = 0o755
            stream.addfile(member, io.BytesIO(raw))
    return path, info


class ConsumerChainTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix='ags-consumer-chain-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = source_document(); self.raw = (json.dumps(self.source) + '\n').encode()
        self.files = {'release-source.json': self.raw, 'install.sh': b'#!/bin/sh\nexit 64\n'}
        self.infos = {}
        for target in v2.TARGETS:
            path, info = archive(self.root, target, self.source)
            self.files[path.name] = path.read_bytes()
            self.files['build-info-' + target + '.json'] = (json.dumps(info) + '\n').encode()
            self.files['attestation-' + target + '.sigstore.json'] = (target + '-bundle').encode()
            self.infos[target] = info
        names = sorted(set(self.files) - {'SHA256SUMS'})
        self.files['SHA256SUMS'] = ''.join(hashlib.sha256(self.files[name]).hexdigest() + '  ' + name + '\n' for name in names).encode()
        self.assets = [{'id': index, 'name': name, 'size': len(raw),
                        'digest': 'sha256:' + hashlib.sha256(raw).hexdigest()}
                       for index, (name, raw) in enumerate(sorted(self.files.items()), 1)]
        self.release = {'id': 9, 'tag_name': VERSION, 'draft': False, 'immutable': True,
                        'prerelease': True, 'assets': self.assets}
        self.prefix = self.root / 'install-root'
        self.args = argparse.Namespace(version=VERSION, install=False, verify_only=True, prefix=self.prefix)
        self.calls = []

    def api(self, path):
        self.calls.append(path)
        if '/contents/fork/release-source.json?ref=' in path:
            return {'type': 'file', 'path': 'fork/release-source.json', 'encoding': 'base64',
                    'sha': hashlib.sha1(b'blob ' + str(len(self.raw)).encode() + b'\0' + self.raw).hexdigest(),
                    'content': base64.b64encode(self.raw).decode()}
        if '/git/commits/' in path:
            return {'tree': {'sha': TREE}}
        if '/compare/' in path:
            return {'status': 'ahead', 'merge_base_commit': {'sha': RUNTIME}}
        raise AssertionError(path)

    def execute(self, command):
        self.calls.append(command)
        if command[1] == 'release' and command[2] == 'download':
            name = command[command.index('--pattern') + 1]
            path = Path(command[command.index('--dir') + 1]) / name
            path.write_bytes(self.files[name])
            return b''
        if command[1] == 'release' and command[2] == 'verify-asset':
            return b''
        if command[1] == '--version':
            target = command[0].split('/')[-3].split('-')[-2] + '_' + command[0].split('/')[-3].split('-')[-1]
            return json.dumps(self.infos[target]['binaries'][Path(command[0]).name]['identity']).encode()
        raise AssertionError(command)

    def proof(self, archive, pin, target, **kwargs):
        return {'run_id': '42', 'run_attempt': '1', 'control_revision': CONTROL,
                'signer_workflow': verifier.WORKFLOW, 'predicate_type': verifier.PREDICATE_TYPE,
                'bundle_sha256': hashlib.sha256(kwargs['bundle'].read_bytes()).hexdigest(), 'verified': True}

    def consume(self, *, install=False, mutate=None, proof=None):
        self.args.install = install
        self.calls.clear()
        consumer = installer.adjacent_module('release_install_v2')
        verifier_module = consumer.verifier
        original = verifier_module.verify_release
        verifier_module.verify_release = proof or self.proof
        try:
            with patch.object(installer, 'api', side_effect=self.api), patch.object(installer, 'execute', side_effect=self.execute):
                if mutate:
                    mutate()
                return consumer.run(installer, self.args, self.release, CONTROL, 'darwin_arm64')
        finally:
            verifier_module.verify_release = original

    def test_all_nine_assets_proofs_and_both_attestations_before_any_install(self):
        result = self.consume()
        self.assertFalse(self.prefix.exists())
        self.assertEqual(result['verification'], {'metadata': 'verified', 'asset_bytes': 'verified',
                                                   'immutable_release': 'verified', 'attestation': 'verified',
                                                   'archive': 'verified', 'executable_identity': 'unverified'})
        downloaded = [call[call.index('--pattern') + 1] for call in self.calls if isinstance(call, list) and 'download' in call]
        self.assertEqual(len(downloaded), 9)
        self.assertEqual(set(downloaded), {asset['name'] for asset in self.assets})
        self.assertEqual({call[2] for call in self.calls if isinstance(call, list) and call[1] == 'release'}, {'download', 'verify-asset'})
        self.assertNotIn('--version', [call[1] for call in self.calls if isinstance(call, list)])

    def test_any_failed_check_writes_no_prefix_and_makes_no_install_call(self):
        def wrong_sums():
            self.files['SHA256SUMS'] = b'0' * 64 + b'  install.sh\n'
        def wrong_source():
            self.files['release-source.json'] = b'{}'
        def wrong_info():
            name = 'build-info-linux_amd64.json'
            self.files[name] = self.files[name].replace(b'"tree":"' + TREE.encode(), b'"tree":"' + (b'0' * 40))
        def wrong_run(archive, pin, target, **kwargs):
            return {**self.proof(archive, pin, target, **kwargs), 'run_id': target}
        cases = [wrong_sums, wrong_source, wrong_info, lambda: None]
        proofs = [self.proof, self.proof, self.proof, wrong_run]
        for mutate, proof in zip(cases, proofs):
            with self.subTest(mutate=mutate.__name__), self.assertRaises(ValueError):
                self.consume(install=True, mutate=mutate, proof=proof)
            self.assertFalse(self.prefix.exists())
            self.assertFalse(any(isinstance(call, list) and call[1] == '--version' for call in self.calls))

    def test_successful_install_only_after_complete_verification(self):
        with patch.object(installer, 'install_verified', side_effect=AssertionError('installer boundary')) as boundary:
            with self.assertRaisesRegex(AssertionError, 'installer boundary'):
                self.consume(install=True)
            boundary.assert_called_once()
            archive, info, receipt, prefix = boundary.call_args.args
            self.assertEqual((archive.name, info['tree'], receipt['verification']['executable_identity'], prefix),
                             ('ags-' + VERSION + '-darwin_arm64.tar.gz', TREE, 'verified', self.prefix))
        self.assertFalse(self.prefix.exists())


if __name__ == '__main__':
    unittest.main()
