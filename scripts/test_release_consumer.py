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
import platform
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
        self.proof_targets = []

    def rebuild_assets(self):
        names = sorted(set(self.files) - {'SHA256SUMS'})
        self.files['SHA256SUMS'] = ''.join(
            hashlib.sha256(self.files[name]).hexdigest() + '  ' + name + '\n' for name in names).encode()
        self.assets = [{'id': index, 'name': name, 'size': len(raw),
                        'digest': 'sha256:' + hashlib.sha256(raw).hexdigest()}
                       for index, (name, raw) in enumerate(sorted(self.files.items()), 1)]
        self.release = {**self.release, 'assets': self.assets}

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
            raise AssertionError('candidate executed')
        raise AssertionError(command)

    def proof(self, archive, pin, target, **kwargs):
        self.proof_targets.append(target)
        return {'run_id': '42', 'run_attempt': '1', 'control_revision': CONTROL,
                'signer_workflow': verifier.WORKFLOW, 'predicate_type': verifier.PREDICATE_TYPE,
                'bundle_sha256': hashlib.sha256(kwargs['bundle'].read_bytes()).hexdigest(), 'verified': True}

    def simulated_host(self, consumer, target, *, compatible):
        # Patch the probes production host_abi reads. Do not replace host_abi itself.
        if target == 'darwin_arm64':
            version = '15.5' if compatible else '14.0'
            return patch.object(consumer.platform, 'mac_ver', return_value=(version, ('', '', ''), 'arm64'))
        if target == 'linux_amd64':
            version = '2.35' if compatible else '2.31'
            return patch.object(consumer.platform, 'libc_ver', return_value=('glibc', version))
        raise AssertionError(target)

    def consume(self, *, install=False, verify_only=None, mutate=None, proof=None, target='darwin_arm64', host=None):
        self.args.install = install
        if verify_only is None:
            verify_only = not install
        self.args.verify_only = verify_only
        self.calls.clear()
        self.proof_targets = []
        consumer = installer.adjacent_module('release_install_v2')
        verifier_module = consumer.verifier
        original = verifier_module.verify_release
        selected = proof or (lambda *args, **kwargs: self.proof(*args, **kwargs))
        verifier_module.verify_release = selected
        contexts = [patch.object(installer, 'api', side_effect=self.api),
                    patch.object(installer, 'execute', side_effect=self.execute)]
        # Verify-only proves both platforms. A non-native target is not this
        # machine, so an unspecified host uses a compatible probe. Explicit
        # host=False still refuses that platform.
        native = {'Darwin': 'darwin', 'Linux': 'linux'}.get(platform.system())
        arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
        native_target = native + '_' + arch if native and arch else None
        if host is None and target != native_target:
            host = True
        if host is not None:
            contexts.append(self.simulated_host(consumer, target, compatible=host))
        try:
            with contextlib.ExitStack() as stack:
                for context in contexts:
                    stack.enter_context(context)
                if mutate:
                    mutate()
                return consumer.run(installer, self.args, self.release, CONTROL, target)
        finally:
            verifier_module.verify_release = original

    def fresh(self):
        case = ConsumerChainTests()
        case.setUp()
        self.addCleanup(case.doCleanups)
        return case

    def sentinel(self, case):
        case.prefix.mkdir()
        marker = case.prefix / 'sentinel'
        marker.write_bytes(b'original')
        return marker.read_bytes()

    def assert_untouched(self, case, before, install):
        self.assertEqual(install.call_count, 0)
        self.assertEqual(list(case.prefix.iterdir()), [case.prefix / 'sentinel'])
        self.assertEqual((case.prefix / 'sentinel').read_bytes(), before)
        self.assertFalse(any(isinstance(call, list) and '--version' in call for call in case.calls))

    def rejected(self, case, error, *, proof=None, install=True):
        before = self.sentinel(case)
        with patch.object(installer, 'install_verified') as called:
            with self.assertRaisesRegex(ValueError, error):
                case.consume(install=install, verify_only=not install, proof=proof)
        self.assert_untouched(case, before, called)
        return case

    def test_all_nine_assets_proofs_and_both_attestations_before_any_install(self):
        with patch.object(installer, 'install_verified') as called:
            result = self.consume()
        called.assert_not_called()
        self.assertEqual(self.proof_targets, list(v2.TARGETS))
        self.assertFalse(self.prefix.exists())
        self.assertEqual(result['verification'], {'metadata': 'verified', 'asset_bytes': 'verified',
                                                   'immutable_release': 'verified', 'attestation': 'verified',
                                                   'archive': 'verified', 'executable_identity': 'unverified'})
        downloaded = [call[call.index('--pattern') + 1] for call in self.calls if isinstance(call, list) and 'download' in call]
        self.assertEqual(len(downloaded), 9)
        self.assertEqual(set(downloaded), {asset['name'] for asset in self.assets})
        self.assertEqual({call[2] for call in self.calls if isinstance(call, list) and call[1] == 'release'}, {'download', 'verify-asset'})
        self.assertNotIn('--version', [call[1] for call in self.calls if isinstance(call, list)])

    def test_verify_only_rejects_each_failed_check_without_install(self):
        def sums(case):
            original = case.files['SHA256SUMS']
            case.files['SHA256SUMS'] = b'0' * 64 + b'  install.sh\n'
            self.assertNotEqual(case.files['SHA256SUMS'], original)

        def source(case):
            original = case.files['release-source.json']
            case.files['release-source.json'] = b'{}'
            self.assertNotEqual(case.files['release-source.json'], original)
            case.rebuild_assets()

        def info(case):
            name = 'build-info-linux_amd64.json'
            original = case.files[name]
            document = json.loads(original)
            document['tree'] = '0' * 40
            case.files[name] = (json.dumps(document) + '\n').encode()
            self.assertNotEqual(case.files[name], original)
            case.rebuild_assets()

        def split(case):
            def attest(archive, pin, target, **kwargs):
                return {**case.proof(archive, pin, target, **kwargs), 'run_id': target}
            return attest

        cases = [('sums', sums, 'release_digest_mismatch', None),
                 ('source', source, 'release_identity_mismatch', None),
                 ('info', info, 'release_manifest_invalid', None),
                 ('split-run', None, 'release_attestation_failed', split)]
        for name, mutate, error, proof in cases:
            with self.subTest(name=name):
                case = self.fresh()
                if mutate:
                    mutate(case)
                seen = self.rejected(case, error, proof=proof(case) if proof else None, install=False)
                if name == 'split-run':
                    self.assertEqual(seen.proof_targets, list(v2.TARGETS))

    def test_each_immutable_proof_failure_is_isolated(self):
        names = [asset['name'] for asset in self.assets]
        self.assertEqual(len(names), 9)
        for index, name in enumerate(names):
            with self.subTest(asset=name):
                case = self.fresh()
                execute = case.execute
                seen = []

                def run(command, execute=execute, seen=seen, failing=index):
                    if command[1:3] == ['release', 'verify-asset']:
                        seen.append(Path(command[4]).name)
                        if len(seen) - 1 == failing:
                            raise ValueError('immutable proof failure')
                    return execute(command)

                case.execute = run
                self.rejected(case, 'immutable proof failure', install=False)
                self.assertEqual(seen, names[:index + 1])

    def test_each_platform_attestation_failure_is_isolated(self):
        for failing in v2.TARGETS:
            with self.subTest(target=failing):
                case = self.fresh()

                def attest(archive, pin, target, failing=failing, **kwargs):
                    proof = case.proof(archive, pin, target, **kwargs)
                    if target == failing:
                        raise ValueError('attestation failure')
                    return proof

                self.rejected(case, 'attestation failure', proof=attest, install=False)
                self.assertIn(failing, case.proof_targets)

    def test_install_refuses_missing_contracts_before_root_or_candidate(self):
        for target in v2.TARGETS:
            with self.subTest(target=target):
                case = self.fresh()
                before = self.sentinel(case)
                called = unittest.mock.Mock(wraps=installer.install_verified)
                with patch.object(installer, 'install_verified', called):
                    with self.assertRaises(ValueError) as caught:
                        case.consume(install=True, target=target, host=True)
                self.assertEqual(type(caught.exception).__name__, 'InstallClosed')
                self.assertEqual(caught.exception.exit_code, 83)
                self.assertEqual(caught.exception.missing_contracts, [
                    'backup_git_config_coverage_and_git_db_consistency',
                    'independent_observation_approval_and_finalize',
                    'trusted_mini_handoff_bound_to_pin_release_deployment'])
                self.assertEqual(case.proof_targets, [])
                self.assertFalse(any(isinstance(call, list) and call[1:3] == ['release', 'download'] for call in case.calls))
                self.assert_untouched(case, before, called)

    def test_install_refusal_ignores_host_config_and_zero_exit_commands(self):
        case = self.fresh()
        before = self.sentinel(case)
        called = unittest.mock.Mock(wraps=installer.install_verified)
        with patch.object(installer, 'install_verified', called), \
                patch.dict(os.environ, {'AGS_BACKUP': '/usr/bin/true', 'AGS_HANDOFF': '/usr/bin/true'}):
            with self.assertRaisesRegex(ValueError, 'install_contracts_unimplemented') as caught:
                case.consume(install=True, host=True)
        self.assertEqual(type(caught.exception).__name__, 'InstallClosed')
        self.assert_untouched(case, before, called)

    def test_cli_install_refuses_before_consumer_effects(self):
        case = self.fresh()
        before = self.sentinel(case)

        def api(path):
            if path == 'repos/' + installer.REPO:
                return {'id': installer.REPO_ID, 'private': False}
            if '/releases/tags/' in path:
                return case.release
            if '/git/ref/' in path:
                return {'object': {'type': 'commit', 'sha': CONTROL}}
            return case.api(path)

        argv = ['install-release.py', '--version', VERSION, '--allow-prerelease', '--install',
                '--prefix', str(case.prefix)]
        with patch.object(sys, 'argv', argv), patch.object(installer, 'api', side_effect=api), \
                patch.object(installer, 'target', return_value='darwin_arm64'), \
                patch.object(installer, 'execute', side_effect=AssertionError('download')), \
                patch.object(installer, 'install_verified', side_effect=AssertionError('replace')) as called, \
                patch.object(sys, 'stderr', io.StringIO()):
            with self.assertRaises(SystemExit) as caught:
                installer.entry()
        self.assertEqual(caught.exception.code, 83)
        called.assert_not_called()
        self.assertEqual((case.prefix / 'sentinel').read_bytes(), before)
        self.assertFalse(any(isinstance(call, list) for call in case.calls))

    def test_install_gate_does_not_hide_verify_only_failures(self):
        case = self.fresh()
        original = case.files['SHA256SUMS']
        case.files['SHA256SUMS'] = b'not-a-sum\n'
        self.assertNotEqual(case.files['SHA256SUMS'], original)
        self.rejected(case, 'release_digest_mismatch', install=False)
        self.assertFalse(case.args.install)
        self.assertTrue(case.args.verify_only)

    def v2_assets_without(self, case, omitted):
        return [asset for asset in case.assets if asset['name'] not in omitted]

    def assert_cli_refuses_before_root(self, case, assets, *,
                                       allow_identify_download=False, error='release_artifact_unverified', existing=False):
        case.release = {**case.release, 'assets': assets}
        if existing:
            (case.prefix / 'bin').mkdir(parents=True)
            (case.prefix / 'bin/gh-server').write_bytes(b'original binary')
            (case.prefix / 'state').mkdir()
            (case.prefix / 'state/installation.json').write_bytes(b'original receipt')
        else:
            self.assertFalse(case.prefix.exists())

        def snapshot():
            if not case.prefix.exists():
                return None
            paths = [case.prefix, *sorted(case.prefix.rglob('*'))]
            return [(str(path.relative_to(case.prefix)), path.stat().st_mode,
                     path.stat().st_mtime_ns, None if path.is_dir() else path.read_bytes()) for path in paths]

        before = snapshot()
        events = []

        def api(path):
            events.append(('api', path))
            if path == 'repos/' + installer.REPO:
                return {'id': installer.REPO_ID, 'private': False}
            if '/releases/tags/' in path:
                return case.release
            if '/git/ref/' in path:
                return {'object': {'type': 'commit', 'sha': CONTROL}}
            return case.api(path)

        def command(argv):
            events.append(('command', tuple(argv)))
            if allow_identify_download or (len(argv) > 2 and argv[1:3] == ['release', 'download']):
                return case.execute(argv)
            if len(argv) > 2 and argv[1:3] == ['release', 'verify-asset']:
                return b''
            raise AssertionError('command')

        def owned(path):
            events.append(('owned', path))
            raise AssertionError('root')

        mode = '--install' if case.args.install else '--verify-only'
        argv = ['install-release.py', '--version', VERSION, '--allow-prerelease', mode,
                '--prefix', str(case.prefix)]
        stderr = io.StringIO()
        with patch.object(sys, 'argv', argv), patch.object(installer, 'api', side_effect=api), \
                patch.object(installer, 'target', return_value='darwin_arm64'), \
                patch.object(installer, 'execute', side_effect=command), \
                patch.object(installer, 'owned_directory', side_effect=owned), \
                patch.object(installer, 'install_verified', side_effect=AssertionError('replace')) as called, \
                patch.object(sys, 'stderr', stderr):
            with self.assertRaises(SystemExit) as caught:
                installer.entry()
        self.assertNotEqual(caught.exception.code, 0)
        self.assertIn(error, stderr.getvalue())
        called.assert_not_called()
        self.assertEqual(snapshot(), before)
        if not existing:
            self.assertFalse(case.prefix.exists())
        self.assertNotIn('owned', [kind for kind, _ in events])
        self.assertEqual(caught.exception.code, 83 if error == 'install_contracts_unimplemented' else 1)
        downloads = [argv for kind, argv in events if kind == 'command']
        self.assertFalse(any('--version' in argv for argv in downloads))
        for argv in downloads:
            self.assertNotIn(str(case.prefix), ' '.join(argv))
            self.assertIn(argv[1:3], (('release', 'download'), ('release', 'verify-asset')))
            if '--dir' in argv:
                self.assertFalse(Path(argv[argv.index('--dir') + 1]).exists())
        if case.prefix.exists():
            self.assertFalse(any(path.name.endswith(('.lock', '.claim')) or path.name.startswith('.install-')
                                 for path in case.prefix.rglob('*')))
        if not allow_identify_download:
            # A named v2 selection stops at the closed install gate. It may
            # download nothing, and it never downloads into the prefix.
            self.assertFalse(any(argv[1:3] == ('release', 'download') and str(case.prefix) in argv for argv in downloads))
            if error == 'install_contracts_unimplemented':
                self.assertEqual(downloads, [])
        return events

    def test_damaged_v2_inventory_is_refused_before_any_install_root(self):
        markers = {'release-source.json', 'attestation-darwin_arm64.sigstore.json',
                   'attestation-linux_amd64.sigstore.json'}
        # Any surviving v2-only name selects v2 before a download or a root.
        partial = [{'release-source.json'}, {'attestation-darwin_arm64.sigstore.json'},
                   {'attestation-linux_amd64.sigstore.json'},
                   {'attestation-darwin_arm64.sigstore.json', 'attestation-linux_amd64.sigstore.json'}]
        for omitted in partial:
            with self.subTest(omitted=sorted(omitted)):
                case = self.fresh()
                case.args.install = True
                case.args.verify_only = False
                self.assert_cli_refuses_before_root(case, self.v2_assets_without(case, omitted),
                                                    error='install_contracts_unimplemented')
        # None of the three names. External build-info assets remain, and the
        # v2 schema inside the archive is refused before the install root.
        case = self.fresh()
        case.args.install = True
        case.args.verify_only = False
        unnamed = self.v2_assets_without(case, markers)
        self.assertTrue(any(asset['name'].startswith('build-info-') for asset in unnamed))
        self.assertIn('ags-' + VERSION + '-darwin_arm64.tar.gz', {asset['name'] for asset in unnamed})
        events = self.assert_cli_refuses_before_root(case, unnamed, allow_identify_download=True,
                                                    error='install_contracts_unimplemented')
        commands = [argv for kind, argv in events if kind == 'command']
        self.assertEqual([argv[1:3] for argv in commands], [('release', 'download'), ('release', 'verify-asset')])
        self.assertTrue(all('--dir' in argv and str(case.prefix) not in argv for argv in commands if argv[2] == 'download'))
        # Archive only must exercise --install as well as --verify-only.
        case = self.fresh()
        case.args.install = True
        case.args.verify_only = False
        archive_only = [asset for asset in unnamed if not asset['name'].startswith('build-info-')]
        self.assertIn('ags-' + VERSION + '-darwin_arm64.tar.gz', {asset['name'] for asset in archive_only})
        self.assertFalse(any(asset['name'].startswith('build-info-') for asset in archive_only))
        events = self.assert_cli_refuses_before_root(case, archive_only, allow_identify_download=True,
                                                    error='install_contracts_unimplemented')
        commands = [argv for kind, argv in events if kind == 'command']
        self.assertEqual([argv[1:3] for argv in commands], [('release', 'download'), ('release', 'verify-asset')])
        verify = self.fresh()
        verify.args.install = False
        verify.args.verify_only = True
        self.assert_cli_refuses_before_root(verify, archive_only, allow_identify_download=True)
        # Schema removed. v2 ABI inside the manifest, or NOTICE in the archive, still refuses.
        for label, mutate in (
                ('abi', lambda info: info.__setitem__('schema', 'damaged')),
                ('notice', lambda info: info.__setitem__('schema', 'ags.release.v1') or info.pop('abi'))):
            with self.subTest(shape=label):
                shaped = self.fresh()
                shaped.args.install = True
                shaped.args.verify_only = False
                info = json.loads(shaped.files['build-info-darwin_arm64.json'])
                mutate(info)
                raw = (json.dumps(info) + '\n').encode()
                path = shaped.root / ('ags-' + VERSION + '-darwin_arm64.tar.gz')
                with tarfile.open(path, 'w:gz') as stream:
                    for name in ('LICENSE', 'NOTICE', 'build-info.json'):
                        payload = raw if name == 'build-info.json' else name.encode()
                        member = tarfile.TarInfo(name); member.size = len(payload)
                        stream.addfile(member, io.BytesIO(payload))
                shaped.files[path.name] = path.read_bytes()
                rows = []
                for asset in archive_only:
                    if asset['name'] == path.name:
                        raw = shaped.files[path.name]
                        rows.append({**asset, 'size': len(raw),
                                     'digest': 'sha256:' + hashlib.sha256(raw).hexdigest()})
                    elif not asset['name'].endswith('.tar.gz'):
                        rows.append(asset)
                self.assert_cli_refuses_before_root(shaped, rows, allow_identify_download=True,
                                                    error='install_contracts_unimplemented')

    def reshape_archive(self, case, shape, target='darwin_arm64'):
        name = 'ags-' + VERSION + '-' + target + '.tar.gz'
        output = io.BytesIO()
        with tarfile.open(fileobj=io.BytesIO(case.files[name])) as original, \
                tarfile.open(fileobj=output, mode='w:gz') as stream:
            for member in original.getmembers():
                if (member.name == 'NOTICE' and shape != 'v1-notice') or (member.name == 'build-info.json' and shape == 'missing-manifest'):
                    continue
                payload = original.extractfile(member).read()
                if member.name == 'build-info.json':
                    if shape == 'invalid-json':
                        payload = b'{"schema":"ags.release.v2",'
                    elif shape == 'oversized-manifest':
                        payload += b' ' * 1048576
                    elif shape in ('v1', 'v1-notice'):
                        info = json.loads(payload)
                        info.update(schema=installer.LEGACY_MANIFEST_SCHEMA, revision=CONTROL,
                                    minimum_platform='macOS 15 (Apple Silicon)' if target.startswith('darwin')
                                    else 'Ubuntu 24.04 / glibc 2.39 for gh-server')
                        info.pop('abi'); info.pop('toolchain')
                        for binary in info['binaries'].values():
                            binary['identity']['revision'] = CONTROL
                        payload = json.dumps(info).encode()
                    elif shape == 'unknown-schema':
                        info = json.loads(payload)
                        info['schema'] = 'unknown'; info.pop('abi')
                        payload = json.dumps(info).encode()
                    elif shape == 'invalid-v1':
                        payload = b'{"schema":"ags.release.v1","binaries":{}}'
                member.size = len(payload)
                stream.addfile(member, io.BytesIO(payload))
        case.files = {name: b'not a tar archive' if shape == 'non-tar' else output.getvalue()}
        case.rebuild_assets()
        return case.assets

    def test_unknown_archives_are_refused_before_install_and_verify_effects(self):
        for shape in ('missing-manifest', 'invalid-json', 'oversized-manifest', 'non-tar',
                      'unknown-schema', 'invalid-v1'):
            for install in (True, False):
                for existing in (False, True):
                    with self.subTest(shape=shape, install=install, existing=existing):
                        case = self.fresh()
                        case.args.install = install
                        case.args.verify_only = not install
                        assets = self.reshape_archive(case, shape)
                        self.assert_cli_refuses_before_root(case, assets, allow_identify_download=True,
                                                            existing=existing)

    def test_identification_scratch_stays_outside_an_unsafe_tmpdir(self):
        archive_name = 'ags-' + VERSION + '-darwin_arm64.tar.gz'
        for location in ('root', 'state', 'root-link'):
            for damaged in (False, True):
                with self.subTest(location=location, damaged=damaged):
                    case = self.fresh()
                    case.args.install = True
                    case.args.verify_only = False
                    (case.prefix / 'state').mkdir(parents=True)
                    (case.prefix / 'state/installation.json').write_bytes(b'original receipt')
                    (case.prefix / 'bin').mkdir()
                    (case.prefix / 'bin/gh-server').write_bytes(b'original binary')
                    if damaged:
                        assets = self.reshape_archive(case, 'non-tar')
                    else:
                        assets = [asset for asset in case.assets if asset['name'] == archive_name]
                    case.release = {**case.release, 'assets': assets}
                    if location == 'root':
                        configured = case.prefix
                    elif location == 'state':
                        configured = case.prefix / 'state'
                    else:
                        configured = case.root / 'root-link'
                        configured.symlink_to(case.prefix, target_is_directory=True)
                    self.assertTrue(configured.exists())

                    def tree():
                        paths = [case.prefix, *sorted(case.prefix.rglob('*'))]
                        return [(str(path.relative_to(case.prefix)), path.lstat().st_mode,
                                 path.lstat().st_mtime_ns,
                                 None if path.is_dir() and not path.is_symlink() else path.read_bytes())
                                for path in paths]

                    before = tree()
                    seen = []
                    audited = {'open', 'os.mkdir', 'os.makedirs', 'os.rename', 'os.replace',
                               'os.remove', 'os.unlink', 'os.rmdir', 'os.symlink', 'os.link',
                               'os.chmod', 'shutil.rmtree'}

                    def audit(event, args):
                        if event == 'open':
                            args = args[:1]
                        elif event not in audited:
                            return
                        for arg in args:
                            if not isinstance(arg, (str, bytes, os.PathLike)):
                                continue
                            try:
                                path = Path(os.fsdecode(arg))
                                if not path.is_absolute():
                                    path = Path.cwd() / path
                                path.relative_to(case.prefix)
                            except (OSError, ValueError):
                                continue
                            seen.append((event, str(path)))
                            return

                    def api(path):
                        if path == 'repos/' + installer.REPO:
                            return {'id': installer.REPO_ID, 'private': False}
                        if '/releases/tags/' in path:
                            return case.release
                        if '/git/ref/' in path:
                            return {'object': {'type': 'commit', 'sha': CONTROL}}
                        raise AssertionError(path)

                    def command(argv):
                        if '--dir' in argv:
                            scratch = Path(argv[argv.index('--dir') + 1])
                            self.assertFalse(scratch.is_relative_to(case.prefix))
                            self.assertEqual(scratch.stat().st_mode & 0o777, 0o700)
                        self.assertNotIn(str(case.prefix), ' '.join(argv))
                        return case.execute(argv)

                    argv = ['install-release.py', '--version', VERSION, '--allow-prerelease',
                            '--install', '--prefix', str(case.prefix)]
                    stderr = io.StringIO()
                    sys.addaudithook(audit)
                    # setUp creates its fixture through tempfile, which caches the
                    # safe process default. Clear only that cache so this call
                    # observes the unsafe TMPDIR exactly as a fresh process does.
                    tempfile.tempdir = None
                    previous_umask = os.umask(0o022)
                    os.umask(previous_umask)
                    self.addCleanup(os.umask, previous_umask)
                    with patch.dict(os.environ, {'TMPDIR': str(configured)}, clear=False), \
                            patch.object(sys, 'argv', argv), \
                            patch.object(installer, 'api', side_effect=api), \
                            patch.object(installer, 'target', return_value='darwin_arm64'), \
                            patch.object(installer, 'execute', side_effect=command), \
                            patch.object(installer, 'install_verified', side_effect=AssertionError('replace')) as called, \
                            patch.object(sys, 'stderr', stderr):
                        with self.assertRaises(SystemExit) as caught:
                            installer.entry()
                    self.assertEqual(caught.exception.code, 1 if damaged else 83)
                    self.assertIn('release_artifact_unverified' if damaged else 'install_contracts_unimplemented',
                                  stderr.getvalue())
                    called.assert_not_called()
                    self.assertEqual(seen, [])
                    self.assertEqual(tree(), before)
                    commands = [call for call in case.calls if isinstance(call, list)]
                    self.assertEqual([call[1:3] for call in commands],
                                     [['release', 'download'], ['release', 'verify-asset']])
                    self.assertFalse(any('--version' in call for call in commands))
                    self.assertFalse(any(path.name.endswith(('.lock', '.claim', '.new'))
                                         or path.name.startswith(('.install-', 'ags-release-identify-'))
                                         for path in case.prefix.rglob('*')))

    def test_complete_historical_v1_keeps_install_and_verify_routes(self):
        for target, shape in ((target, shape) for target in v2.TARGETS for shape in ('v1', 'v1-notice')):
            for install in (True, False):
                with self.subTest(target=target, shape=shape, install=install):
                    case = self.fresh()
                    self.reshape_archive(case, shape, target)

                    def api(path):
                        if path == 'repos/' + installer.REPO:
                            return {'id': installer.REPO_ID, 'private': False}
                        if '/releases/tags/' in path:
                            return case.release
                        if '/git/ref/' in path:
                            return {'object': {'type': 'commit', 'sha': CONTROL}}
                        raise AssertionError(path)

                    def command(argv):
                        if argv[1:3] == ['attestation', 'verify']:
                            case.calls.append(argv)
                            return b''
                        return case.execute(argv)

                    mode = '--install' if install else '--verify-only'
                    argv = ['install-release.py', '--version', VERSION, '--allow-prerelease', mode,
                            '--prefix', str(case.prefix)]
                    with patch.object(sys, 'argv', argv), patch.object(installer, 'api', side_effect=api), \
                            patch.object(installer, 'target', return_value=target), \
                            patch.object(installer, 'execute', side_effect=command), \
                            patch.object(installer, 'install_verified') as called, \
                            contextlib.redirect_stdout(io.StringIO()) as out:
                        installer.entry()
                    self.assertEqual(called.call_count, int(install))
                    result = json.loads(out.getvalue())
                    self.assertEqual(result['verification']['archive'], 'verified')
                    self.assertTrue(result['build_provenance_verified'])
                    self.assertEqual(case.prefix.exists(), install)
                    if install:
                        self.assertEqual(list(case.prefix.iterdir()), [])
                        self.assertEqual(called.call_args.args[1]['schema'], installer.LEGACY_MANIFEST_SCHEMA)
                    self.assertTrue(any(argv[1:3] == ['attestation', 'verify'] for argv in case.calls))
                    self.assertFalse(any('--version' in argv for argv in case.calls))

    def test_host_abi_rejects_incompatible_platforms_and_reads_this_machine(self):
        consumer = installer.adjacent_module('release_install_v2')
        for target, compatible in ((name, flag) for name in v2.TARGETS for flag in (True, False)):
            with self.subTest(target=target, compatible=compatible):
                with self.simulated_host(consumer, target, compatible=compatible):
                    self.assertEqual(consumer.host_abi(target), compatible)
                    if compatible:
                        consumer.require_host_abi(target)
                    else:
                        with self.assertRaisesRegex(ValueError, 'release_abi_incompatible'):
                            consumer.require_host_abi(target)
        native = {'Darwin': 'darwin', 'Linux': 'linux'}.get(platform.system())
        arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
        native_target = native + '_' + arch if native and arch else None
        for target in v2.TARGETS:
            with self.subTest(native=target):
                if target == native_target and consumer.host_abi(target):
                    consumer.require_host_abi(target)
                else:
                    with self.assertRaisesRegex(ValueError, 'release_abi_incompatible'):
                        consumer.require_host_abi(target)

    def test_verified_consumer_refuses_incompatible_host_before_install(self):
        for target in v2.TARGETS:
            with self.subTest(target=target):
                case = self.fresh()
                before = self.sentinel(case)
                called = unittest.mock.Mock(wraps=installer.install_verified)
                with patch.object(installer, 'install_verified', called):
                    with self.assertRaisesRegex(ValueError, 'release_abi_incompatible'):
                        case.consume(install=False, verify_only=True, target=target, host=False)
                self.assertEqual(case.proof_targets, list(v2.TARGETS))
                self.assert_untouched(case, before, called)


if __name__ == '__main__':
    unittest.main()
