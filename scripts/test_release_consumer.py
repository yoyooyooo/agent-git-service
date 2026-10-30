#!/usr/bin/env python3
"""Complete offline v2 consumer chain. Synthetic transport is not Sigstore evidence."""
import argparse
import ast
import base64
import contextlib
import hashlib
import importlib.util
import io
import json
import os
import platform
import re
from pathlib import Path
import shutil
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


    def test_shell_scratch_is_outside_installation_before_v2_refusal(self):
        for action in ('plan', 'install', 'upgrade'):
            for location in ('root', 'state', 'root-link', 'state-link', 'outside',
                             'relative-inside', 'relative-outside', 'no-safe-parent'):
                with self.subTest(action=action, location=location):
                    f = self.fresh()
                    prefix = f.root / 'installation'
                    (prefix / 'state').mkdir(parents=True)
                    (prefix / 'state/installation.json').write_bytes(b'existing receipt')
                    (prefix / 'binary').write_bytes(b'existing binary')
                    outside = f.root / 'outside'; outside.mkdir()
                    alias = f.root / 'alias'
                    alias.symlink_to(prefix / 'state' if location == 'state-link' else prefix)
                    configured = {'root': str(prefix), 'state': str(prefix / 'state'),
                                  'root-link': str(alias), 'state-link': str(alias),
                                  'outside': str(outside), 'relative-inside': 'installation/state',
                                  'relative-outside': 'outside', 'no-safe-parent': str(prefix)}[location]
                    selected_prefix = Path('/') if location == 'no-safe-parent' else prefix
                    transport = f.root / 'transport'; transport.mkdir()
                    release = {**f.release, 'target_commitish': CONTROL}
                    (f.root / 'release.json').write_text(json.dumps(release))
                    (f.root / 'source.json').write_text(json.dumps({
                        'type': 'file', 'path': 'fork/release-source.json', 'encoding': 'base64',
                        'sha': hashlib.sha1(b'blob ' + str(len(f.raw)).encode() + b'\0' + f.raw).hexdigest(),
                        'content': base64.b64encode(f.raw).decode()}))
                    gh = transport / 'gh'
                    gh.write_text('#!' + sys.executable + '\n' + r"""import json,os,sys
from pathlib import Path
root=Path(os.environ['SHELL_FIXTURE'])
a=sys.argv[1:]
with (root/'calls.jsonl').open('a') as stream: stream.write(json.dumps(a)+'\n')
if a[0]!='api': raise SystemExit('download, installer or candidate execution forbidden')
p=a[1]
if p=='repos/yoyooyooo/agent-git-service':
 value={'id':1384519373,'private':False}
 (root/'scratch-modes.json').write_text(json.dumps([
  {'path':str(path),'mode':path.stat().st_mode&0o777}
  for path in (root/'outside').glob('ags-installer.*')]))
elif '/releases/tags/' in p: value=json.loads((root/'release.json').read_text())
elif '/git/ref/' in p: value={'object':{'type':'commit','sha':'c'*40}}
elif '/contents/' in p: value=json.loads((root/'source.json').read_text())
elif '/git/commits/' in p: value={'tree':{'sha':'b'*40}}
elif '/compare/' in p: value={'status':'ahead','merge_base_commit':{'sha':'a'*40}}
else: raise SystemExit('unexpected transport: '+p)
print(json.dumps(value))
""")
                    gh.chmod(0o755)
                    env = {k: v for k, v in os.environ.items()
                           if k not in ('BASH_ENV', 'ENV', 'PYTHONPATH', 'MULTICA_TOKEN',
                                        'GH_TOKEN', 'GITHUB_TOKEN')}
                    env.update(TMPDIR=configured, TEMP=str(outside), TMP=str(alias),
                               SHELL_FIXTURE=str(f.root), PYTHONDONTWRITEBYTECODE='1',
                               PATH=os.pathsep.join((str(transport), str(Path(sys.executable).parent),
                                                     '/usr/bin', '/bin', '/usr/sbin', '/sbin')))
                    def snapshot():
                        return [(str(path.relative_to(prefix)), path.lstat().st_mode,
                                 path.lstat().st_mtime_ns, None if path.is_dir() else path.read_bytes())
                                for path in [prefix, *sorted(prefix.rglob('*'))]]
                    before = snapshot()
                    result = subprocess.run(['/bin/bash', '-x', str(HERE / 'install.sh'), action,
                                             '--version', VERSION, '--allow-prerelease', '--prefix',
                                             str(selected_prefix), '--bin-dir', str(f.root / 'commands')],
                                            cwd=f.root, env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(snapshot(), before, result.stderr)
                    self.assertFalse((f.root / 'commands').exists())
                    calls = [json.loads(line) for line in (f.root / 'calls.jsonl').read_text().splitlines()]
                    self.assertTrue(all(call[0] == 'api' for call in calls), calls)
                    self.assertNotIn('install_verified', result.stderr)
                    if location == 'no-safe-parent':
                        self.assertEqual(result.returncode, 1, result.stderr)
                        self.assertIn('release_artifact_unverified', result.stderr)
                        self.assertNotIn('mktemp ', result.stderr)
                        self.assertNotIn('python3 ' + str(HERE / 'install-release.py'), result.stderr)
                    else:
                        self.assertEqual(result.returncode, 0 if action == 'plan' else 83, result.stderr)
                        created = re.findall(r'^\+ tmp=(.+)$', result.stderr, re.MULTILINE)
                        self.assertEqual(len(created), 1, result.stderr)
                        scratch = Path(created[0]).resolve()
                        self.assertFalse(scratch.is_relative_to(prefix))
                        self.assertEqual(scratch.parent, outside)
                        observed = json.loads((f.root / 'scratch-modes.json').read_text())
                        self.assertEqual(observed, [{'path': str(scratch), 'mode': 0o700}])
                        self.assertIn('+ chmod 700 ' + created[0], result.stderr)
                        self.assertFalse(scratch.exists())
                        self.assertIn('+ cleanup', result.stderr)
                        if action != 'plan':
                            self.assertIn('install_contracts_unimplemented', result.stderr)

    def selector_code(self):
        script = (HERE / 'install.sh').read_text()
        return script.split("<<'PY_SCRATCH'\n", 1)[1].split('\nPY_SCRATCH', 1)[0]

    def test_shell_standalone_scratch_selector_matches_python(self):
        code = self.selector_code()
        names = ('same_directory', 'contains_directory', 'outside_installation',
                 'installation_scratch_parent')
        embedded = {node.name: ast.dump(node, include_attributes=False)
                    for node in ast.parse(code).body if isinstance(node, ast.FunctionDef)}
        original = {node.name: ast.dump(node, include_attributes=False)
                    for node in ast.parse((HERE / 'install-release.py').read_text()).body
                    if isinstance(node, ast.FunctionDef)}
        self.assertEqual(set(embedded), set(names))
        for name in names:
            self.assertEqual(embedded[name], original[name], name)
        self.assertNotIn('ast.parse', code)
        self.assertNotIn('exec(', code)
        prefix = self.root / 'Installation'; (prefix / 'state').mkdir(parents=True)
        outside = self.root / 'outside'; outside.mkdir()
        link = self.root / 'link'; link.symlink_to(prefix)
        state_link = self.root / 'state-link'; state_link.symlink_to(prefix / 'state')
        file = self.root / 'file'; file.write_bytes(b'file')
        cases = [str(prefix), str(prefix / 'state'), str(link), str(state_link), str(outside),
                 'Installation/state', 'outside', '', str(file), str(self.root / 'missing'),
                 str(self.root / 'future/state/scratch')]
        oldcwd = Path.cwd()
        try:
            os.chdir(self.root)
            for key in ('TMPDIR', 'TEMP', 'TMP'):
                for configured in cases:
                    for protected in (prefix, self.root / 'future', Path('/')):
                        with self.subTest(key=key, configured=configured, prefix=protected):
                            env = {**os.environ, 'TMPDIR': '', 'TEMP': '', 'TMP': '', key: configured,
                                   'PYTHONDONTWRITEBYTECODE': '1'}
                            with patch.dict(os.environ, env):
                                expected = installer.installation_scratch_parent(protected)
                            result = subprocess.run([sys.executable, '-B', '-c', code, str(protected)],
                                                    env=env, text=True, capture_output=True, timeout=30)
                            self.assertEqual(result.returncode, 1 if expected is None else 0, result.stderr)
                            if expected is None:
                                self.assertIn('release_artifact_unverified', result.stderr)
                            else:
                                self.assertEqual(Path(result.stdout.strip()), expected)
        finally:
            os.chdir(oldcwd)

    def copy_source(self, source, destination):
        shutil.copytree(source, destination, ignore=shutil.ignore_patterns('__pycache__', '*.pyc'))

    def shell_fixture(self, root):
        prefix = root / 'installation'
        (prefix / 'state').mkdir(parents=True)
        (prefix / 'state/installation.json').write_bytes(b'existing receipt')
        (prefix / 'binary').write_bytes(b'existing binary')
        outside = root / 'outside'
        outside.mkdir()
        release = {**self.release, 'target_commitish': CONTROL}
        (root / 'release.json').write_text(json.dumps(release))
        (root / 'source.json').write_text(json.dumps({
            'type': 'file', 'path': 'fork/release-source.json', 'encoding': 'base64',
            'sha': hashlib.sha1(b'blob ' + str(len(self.raw)).encode() + b'\0' + self.raw).hexdigest(),
            'content': base64.b64encode(self.raw).decode()}))
        transport = root / 'transport'
        transport.mkdir()
        gh = transport / 'gh'
        gh.write_text('#!' + sys.executable + '\n' + r"""import json,os,sys
from pathlib import Path
root=Path(os.environ['SHELL_FIXTURE'])
a=sys.argv[1:]
with (root/'calls.jsonl').open('a') as stream: stream.write(json.dumps(a)+'\n')
if a[0]!='api': raise SystemExit('download, installer or candidate execution forbidden')
p=a[1]
if p=='repos/yoyooyooo/agent-git-service': value={'id':1384519373,'private':False}
elif '/releases/tags/' in p: value=json.loads((root/'release.json').read_text())
elif '/git/ref/' in p: value={'object':{'type':'commit','sha':'c'*40}}
elif '/contents/' in p: value=json.loads((root/'source.json').read_text())
elif '/git/commits/' in p: value={'tree':{'sha':'b'*40}}
elif '/compare/' in p: value={'status':'ahead','merge_base_commit':{'sha':'a'*40}}
else: raise SystemExit('unexpected transport: '+p)
print(json.dumps(value))
""")
        gh.chmod(0o755)
        return prefix, outside, transport

    def shell_snapshot(self, prefix):
        return [(str(path.relative_to(prefix)), path.lstat().st_mode, path.lstat().st_mtime_ns,
                 None if path.is_dir() else path.read_bytes())
                for path in [prefix, *sorted(prefix.rglob('*'))]]

    def shell_environment(self, root, transport, configured):
        env = {key: value for key, value in os.environ.items()
               if key not in ('BASH_ENV', 'ENV', 'PYTHONPATH', 'PYTHONPYCACHEPREFIX',
                              'PYTHONDONTWRITEBYTECODE', 'MULTICA_TOKEN', 'GH_TOKEN', 'GITHUB_TOKEN')}
        env.update(HOME=str(root / 'home'), TMPDIR=configured, TEMP=configured, TMP=configured, SHELL_FIXTURE=str(root),
                   PATH=os.pathsep.join((str(transport), str(Path(sys.executable).parent),
                                         '/usr/bin', '/bin', '/usr/sbin', '/sbin')))
        return env

    def test_embedded_selector_ignores_malicious_adjacent_definitions(self):
        attacks = {
            'duplicate': "\n\ndef installation_scratch_parent(prefix):\n"
                         "    Path(os.environ['PROBE_ROOT'], 'selector-executed').write_text('duplicate')\n"
                         "    return prefix\n",
            'replaced': None,
            'decorator': "\n\ndef selector_attack(function):\n"
                          "    Path(os.environ['PROBE_ROOT'], 'selector-executed').write_text('decorator')\n"
                          "    return function\n"
                          "@selector_attack\n"
                          "def installation_scratch_parent(prefix):\n    return prefix\n",
            'default': "\n\ndef installation_scratch_parent(prefix, attack=Path(os.environ['PROBE_ROOT'],"
                       " 'selector-executed').write_text('default')):\n    return prefix\n",
            'annotation': "\n\ndef installation_scratch_parent(prefix) -> Path(os.environ['PROBE_ROOT'],"
                          " 'selector-executed').write_text('annotation'):\n    return prefix\n",
        }
        source = (HERE / 'install-release.py').read_text()
        replaced = source.replace(
            'def installation_scratch_parent(prefix):',
            "def installation_scratch_parent(prefix):\n"
            "    Path(os.environ['PROBE_ROOT'], 'selector-executed').write_text('replaced')\n"
            "    return prefix\n"
            "def discarded_original_installation_scratch_parent(prefix):",
            1)
        attacks['replaced'] = replaced
        for name, mutation in attacks.items():
            for action in ('plan', 'install', 'upgrade'):
                with self.subTest(attack=name, action=action):
                    fixture = tempfile.TemporaryDirectory(prefix='ags-selector-attack-')
                    self.addCleanup(fixture.cleanup)
                    root = Path(fixture.name).resolve()
                    checkout = root / 'checkout'
                    checkout.mkdir()
                    for sibling in ('release_install_v2.py', 'release_verify.py', 'release_contract.py'):
                        (checkout / sibling).write_bytes((HERE / sibling).read_bytes())
                    (checkout / 'install.sh').write_bytes((HERE / 'install.sh').read_bytes())
                    (checkout / 'install.sh').chmod(0o755)
                    text = mutation if name == 'replaced' else source + mutation
                    (checkout / 'install-release.py').write_text(text)
                    (root / 'fork').mkdir()
                    (root / 'fork/release-source.json').write_bytes((HERE.parent / 'fork/release-source.json').read_bytes())
                    prefix, outside, transport = self.shell_fixture(root)
                    before = self.shell_snapshot(prefix)
                    env = self.shell_environment(root, transport, str(outside))
                    env['PROBE_ROOT'] = str(prefix)
                    result = subprocess.run(
                        ['/bin/bash', '-x', str(checkout / 'install.sh'), action, '--version', VERSION,
                         '--allow-prerelease', '--prefix', str(prefix), '--bin-dir', str(root / 'commands')],
                        cwd=root, env=env, capture_output=True, text=True, timeout=30)
                    created = re.findall(r'^\+ tmp=(.+)$', result.stderr, re.MULTILINE)
                    self.assertEqual(len(created), 1, result.stderr)
                    scratch = Path(created[0]).resolve()
                    self.assertEqual(scratch.parent, outside)
                    self.assertFalse(scratch.is_relative_to(prefix))
                    selection = result.stderr.split('+ tmp=', 1)[0]
                    self.assertNotIn('selector-executed', selection)
                    self.assertFalse(scratch.exists())
                    calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
                    self.assertTrue(all(call[0] == 'api' for call in calls), calls)
                    if action == 'plan':
                        self.assertEqual(result.returncode, 0, result.stderr)
                        self.assertIn('python3 -B ' + str(checkout / 'install-release.py'), result.stderr)
                    else:
                        self.assertEqual(result.returncode, 83, result.stderr)
                        self.assertEqual(self.shell_snapshot(prefix), before, result.stderr)
                        self.assertFalse((prefix / 'selector-executed').exists())

    def assert_no_bytecode(self, prefix, before, result):
        after = self.shell_snapshot(prefix)
        if after != before:
            changed = [row[0] for row in after if row not in before]
            self.fail('root changed: ' + ','.join(changed[:30]))
        self.assertEqual(list(prefix.rglob('*.pyc')), [])
        self.assertEqual(list(prefix.rglob('__pycache__')), [])

    def test_product_entries_write_no_bytecode_without_caller_protection(self):
        shapes = ('cache-prefix', 'checkout-in-root')
        for shape in shapes:
            for action in ('plan', 'install', 'upgrade'):
                with self.subTest(entry='shell', shape=shape, action=action):
                    fixture = tempfile.TemporaryDirectory(prefix='ags-bytecode-')
                    self.addCleanup(fixture.cleanup)
                    root = Path(fixture.name).resolve()
                    prefix, outside, transport = self.shell_fixture(root)
                    script = HERE / 'install.sh'
                    if shape == 'checkout-in-root':
                        for name in ('scripts', 'fork'):
                            self.copy_source(HERE.parent / name, prefix / name)
                        script = prefix / 'scripts/install.sh'
                    before = self.shell_snapshot(prefix)
                    env = self.shell_environment(root, transport, str(outside))
                    env['PYTHONPYCACHEPREFIX'] = str(prefix / 'cache')
                    result = subprocess.run(
                        ['/bin/bash', str(script), action, '--version', VERSION, '--allow-prerelease',
                         '--prefix', str(prefix), '--bin-dir', str(root / 'commands')],
                        cwd=root, env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0 if action == 'plan' else 83, result.stderr)
                    self.assert_no_bytecode(prefix, before, result)
                    calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
                    self.assertTrue(all(call[0] == 'api' for call in calls), calls)
        modes = {'plan': [], 'install': ['--install'], 'upgrade': ['--install']}
        for mode, extra in modes.items():
            with self.subTest(entry='python', mode=mode):
                fixture = tempfile.TemporaryDirectory(prefix='ags-python-bytecode-')
                self.addCleanup(fixture.cleanup)
                root = Path(fixture.name).resolve()
                prefix, outside, transport = self.shell_fixture(root)
                scripts = prefix / 'scripts'
                scripts.mkdir()
                for name in ('install-release.py', 'release_install_v2.py', 'release_verify.py', 'release_contract.py'):
                    (scripts / name).write_bytes((HERE / name).read_bytes())
                self.copy_source(HERE.parent / 'fork', prefix / 'fork')
                before = self.shell_snapshot(prefix)
                env = self.shell_environment(root, transport, str(outside))
                env['PYTHONPYCACHEPREFIX'] = str(prefix / 'cache')
                env['PYTHONPATH'] = str(prefix / 'scripts')
                result = subprocess.run(
                    [str(HERE / 'ags-python.sh'), str(prefix / 'scripts/install-release.py'), '--version', VERSION,
                     '--allow-prerelease', '--prefix', str(prefix), *extra],
                    cwd=prefix, env=env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 0 if mode == 'plan' else 83, result.stderr)
                self.assert_no_bytecode(prefix, before, result)
                calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
                self.assertTrue(all(call[0] == 'api' for call in calls), calls)

    def test_controlled_launchers_prevent_startup_cache(self):
        # No caller -B or PYTHONDONTWRITEBYTECODE: exercise the actual shell entries.
        commands = [('install-release', [], 0), ('install-release', ['plan'], 0),
                    ('install-release', ['--install'], 83),
                    ('install-release', ['--verify-only'], 1),
                    ('install-release', ['install'], 83),
                    ('install-release', ['verify-only'], 1)]
        commands += [('upgrade', [command], 64) for command in ('plan', 'apply', 'status')]
        for location in ('cache', 'state/cache'):
            for entry, arguments, expected in commands:
                with self.subTest(entry=entry, arguments=arguments, location=location):
                    with tempfile.TemporaryDirectory(prefix='ags-launcher-') as directory:
                        root = Path(directory).resolve()
                        prefix, outside, transport = self.shell_fixture(root)
                        for name in ('scripts', 'fork'):
                            self.copy_source(HERE.parent / name, prefix / name)
                        command = [str(prefix / 'scripts' / entry), *arguments]
                        if entry == 'install-release':
                            command += ['--version', VERSION, '--allow-prerelease', '--prefix', str(prefix)]
                        else:
                            # Invalid request fails before any real host identity/config lookup.
                            command += ['--deployment-id', 'fixture', '--pin-sha256', 'invalid',
                                        '--ci-run-id', '1', '--ci-run-attempt', '1',
                                        '--authorization-id', 'fixture']
                        env = self.shell_environment(root, transport, str(outside))
                        env['PYTHONPYCACHEPREFIX'] = str(prefix / location)
                        self.assertNotIn('PYTHONDONTWRITEBYTECODE', env)
                        self.assertFalse((prefix / location).exists())
                        before = self.shell_snapshot(prefix)
                        result = subprocess.run(command, cwd=prefix, env=env,
                                                capture_output=True, text=True, timeout=30)
                        self.assertEqual(result.returncode, expected, result.stderr)
                        self.assert_no_bytecode(prefix, before, result)
                        if entry == 'upgrade':
                            self.assertEqual(json.loads(result.stdout)['code'], 'invalid_request')
                        else:
                            calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
                            if expected == 1:
                                self.assertIn('Verification/download command failed: gh release (no fallback)', result.stderr)
                                self.assertEqual(calls[-1][:2], ['release', 'download'])
                            else:
                                self.assertTrue(all(call[0] == 'api' for call in calls), calls)

    def test_bare_python_startup_is_outside_readonly_contract(self):
        # Document the excluded interface, rather than pretending in-script settings
        # can undo CPython startup writes. Each interpreter gets a fresh cache path.
        for options in ([], ['-X']):
            with self.subTest(options=options):
                with tempfile.TemporaryDirectory(prefix='ags-bare-startup-') as directory:
                    root = Path(directory).resolve()
                    prefix, outside, transport = self.shell_fixture(root)
                    cache = prefix / 'state/cache'
                    env = self.shell_environment(root, transport, str(outside))
                    env['PYTHONPYCACHEPREFIX'] = str(cache)
                    command = [sys.executable]
                    if options:
                        command += ['-X', 'pycache_prefix=' + str(cache)]
                    command += [str(HERE / 'install-release.py'), '--help']
                    before = self.shell_snapshot(prefix)
                    result = subprocess.run(command, env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertGreater(len(list(cache.rglob('*.pyc'))), 0)
                    self.assertNotEqual(self.shell_snapshot(prefix), before)

    def test_controlled_python_helper_overrides_explicit_cache_prefix(self):
        with tempfile.TemporaryDirectory(prefix='ags-helper-startup-') as directory:
            root = Path(directory).resolve()
            prefix, outside, transport = self.shell_fixture(root)
            env = self.shell_environment(root, transport, str(outside))
            env['PYTHONPYCACHEPREFIX'] = str(prefix / 'cache')
            before = self.shell_snapshot(prefix)
            result = subprocess.run(
                [str(HERE / 'ags-python.sh'), '-X', 'pycache_prefix=' + str(prefix / 'state/cache'),
                 str(HERE / 'install-release.py'), '--help'], env=env,
                capture_output=True, text=True, timeout=30)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assert_no_bytecode(prefix, before, result)

    def test_pre_refusal_implicit_caches_do_not_write_installation_root(self):
        variables = ('HOME', 'XDG_CACHE_HOME', 'XDG_CONFIG_HOME', 'PIP_CACHE_DIR',
                     'npm_config_cache', 'NODE_COMPILE_CACHE')
        for variable in variables:
            for action in ('plan', 'install', 'upgrade'):
                with self.subTest(variable=variable, action=action):
                    fixture = tempfile.TemporaryDirectory(prefix='ags-implicit-root-')
                    self.addCleanup(fixture.cleanup)
                    root = Path(fixture.name).resolve()
                    prefix, outside, transport = self.shell_fixture(root)
                    target = prefix / 'implicit' / variable
                    before = self.shell_snapshot(prefix)
                    env = self.shell_environment(root, transport, str(outside))
                    env[variable] = str(target)
                    result = subprocess.run(
                        ['/bin/bash', '-x', str(HERE / 'install.sh'), action, '--version', VERSION,
                         '--allow-prerelease', '--prefix', str(prefix), '--bin-dir', str(root / 'commands')],
                        cwd=root, env=env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0 if action == 'plan' else 83, result.stderr)
                    self.assertEqual(self.shell_snapshot(prefix), before, result.stderr)
                    self.assertFalse(target.exists())
                    calls = [json.loads(line) for line in (root / 'calls.jsonl').read_text().splitlines()]
                    self.assertTrue(all(call[0] == 'api' for call in calls), calls)

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
        verify_assets = [asset for asset in self.v2_assets_without(verify, markers)
                         if not asset['name'].startswith('build-info-')]
        self.assert_cli_refuses_before_root(verify, verify_assets, allow_identify_download=True)
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

    def test_named_v2_verify_only_scratch_stays_outside_an_unsafe_tmpdir(self):
        """Real --verify-only of the complete nine-asset v2 release.

        TMPDIR is the installation root, its state directory, or a symlink to
        the root. Rejection and successful verification both keep every download
        outside that root and leave its bytes, mode and mtime unchanged.
        """
        for location in ('root', 'state', 'root-link'):
            for outcome in ('reject', 'success'):
                with self.subTest(location=location, outcome=outcome):
                    case = self.fresh()
                    case.args.install = False
                    case.args.verify_only = True
                    (case.prefix / 'state').mkdir(parents=True)
                    (case.prefix / 'state/installation.json').write_bytes(b'original receipt')
                    (case.prefix / 'bin').mkdir()
                    (case.prefix / 'bin/gh-server').write_bytes(b'original binary')
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
                        return case.api(path)

                    downloads = []

                    def command(argv):
                        if '--dir' in argv:
                            scratch = Path(argv[argv.index('--dir') + 1])
                            resolved = scratch.resolve()
                            self.assertFalse(resolved.is_relative_to(case.prefix.resolve()))
                            self.assertEqual(scratch.stat().st_mode & 0o777, 0o700)
                            downloads.append(resolved)
                        self.assertNotIn(str(case.prefix), ' '.join(argv))
                        return case.execute(argv)

                    def proof(archive, pin, target, **kwargs):
                        if outcome == 'reject':
                            raise ValueError('release_artifact_unverified')
                        return case.proof(archive, pin, target, **kwargs)

                    argv = ['install-release.py', '--version', VERSION, '--allow-prerelease',
                            '--verify-only', '--prefix', str(case.prefix)]
                    stderr = io.StringIO()
                    stdout = io.StringIO()
                    sys.addaudithook(audit)
                    tempfile.tempdir = None
                    previous_umask = os.umask(0o022)
                    os.umask(previous_umask)
                    self.addCleanup(os.umask, previous_umask)
                    consumer = installer.adjacent_module('release_install_v2')
                    # The CLI selects this machine's archive. Verification still
                    # covers both platforms, so a non-native host needs the same
                    # compatible ABI probe the other verify-only cases use.
                    native = {'Darwin': 'darwin', 'Linux': 'linux'}.get(platform.system())
                    arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}.get(platform.machine())
                    selected = native + '_' + arch if native and arch else 'darwin_arm64'
                    called = unittest.mock.Mock(side_effect=AssertionError('replace'))
                    contexts = [patch.dict(os.environ, {'TMPDIR': str(configured)}, clear=False),
                                patch.object(sys, 'argv', argv),
                                patch.object(installer, 'api', side_effect=api),
                                patch.object(installer, 'target', return_value=selected),
                                patch.object(installer, 'execute', side_effect=command),
                                patch.object(installer, 'install_verified', called),
                                patch.object(consumer.verifier, 'verify_release', side_effect=proof),
                                patch.object(sys, 'stderr', stderr),
                                patch.object(sys, 'stdout', stdout)]
                    if selected != native + '_' + arch:
                        contexts.append(case.simulated_host(consumer, selected, compatible=True))
                    with contextlib.ExitStack() as stack:
                        for context in contexts:
                            stack.enter_context(context)
                        if outcome == 'reject':
                            with self.assertRaises(SystemExit) as caught:
                                installer.entry()
                            self.assertEqual(caught.exception.code, 1)
                            self.assertIn('release_artifact_unverified', stderr.getvalue())
                        else:
                            installer.entry()
                            result = json.loads(stdout.getvalue())
                            self.assertEqual(result['verification']['asset_bytes'], 'verified')
                            self.assertEqual(result['verification']['immutable_release'], 'verified')
                    called.assert_not_called()
                    self.assertEqual(seen, [])
                    self.assertEqual(tree(), before)
                    commands = [call for call in case.calls if isinstance(call, list)]
                    self.assertEqual(len([call for call in commands if call[1:3] == ['release', 'download']]), 9)
                    self.assertEqual(len(downloads), 9)
                    self.assertTrue(all(not path.exists() for path in downloads))
                    self.assertFalse(any('--version' in call for call in commands))
                    self.assertFalse(any(path.name.endswith(('.lock', '.claim', '.new'))
                                         or path.name.startswith(('.install-', 'ags-release-identify-',
                                                                  'ags-release-verify-'))
                                         or 'receipt' in path.name and path.name != 'installation.json'
                                         for path in case.prefix.rglob('*')))
                    self.assertEqual((case.prefix / 'state/installation.json').read_bytes(), b'original receipt')

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
