#!/usr/bin/env python3
"""Synthetic policy tests ONLY. No Sigstore bundles or real signatures are made."""
import copy
import hashlib
import importlib.util
import json
import os
import re
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]

def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

verify = load('binding', ROOT / 'scripts/release_verify.py')
release = load('producer', ROOT / 'fork/scripts/release.py')
CONTROL = 'c' * 40


def synthetic_policy():
    # This is only the ABI/binding fragment used by unit tests, NOT an install pin.
    return {'schema': verify.contract.PIN_SCHEMA, 'repository': verify.REPOSITORY,
            'repository_id': verify.REPOSITORY_ID,
            'runtime': {'revision': verify.RUNTIME_REVISION, 'tree': verify.RUNTIME_TREE},
            'targets': {name: {'manifest_schema': verify.contract.MANIFEST_SCHEMA,
                               'abi': verify.contract.abi_for(name),
                               'provenance': {'control_revision': CONTROL, 'signer_digest': CONTROL,
                                              'signer_workflow': verify.WORKFLOW}}
                        for name in ('linux_amd64', 'darwin_arm64')}}


def synthetic_result(digest):
    return [{'verificationResult': {'statement': {
        'predicateType': verify.PREDICATE_TYPE,
        'predicate': {'schema': 'ags.runtime-binding.v1',
                      'runtime_revision': verify.RUNTIME_REVISION, 'runtime_tree': verify.RUNTIME_TREE},
        'subject': [{'name': 'synthetic.txt', 'digest': {'sha256': digest}}]}}}]


class BindingTests(unittest.TestCase):
    def test_dispatch_workflow_has_fixed_native_builders_and_no_release_writes(self):
        text = (ROOT / '.github/workflows/release-control.yml').read_text()
        self.assertIn('workflow_dispatch:', text)
        build=text.split('  build:',1)[1].split('  publish:',1)[0]
        for forbidden in ('pull_request:', 'workflow_call:', 'self-hosted', 'contents: write', 'release.py publish'):
            self.assertNotIn(forbidden, build)
        for required in ('runner: ubuntu-22.04', 'runner: macos-15', 'GOTOOLCHAIN: local',
                         'go-version-file: .go-version', "'1.26.8'", 'ref: ${{ github.sha }}',
                         'predicate-path:', verify.PREDICATE_TYPE, 'build-runtime',
                         'bundle-path', 'persist-credentials: false'):
            self.assertIn(required, text)

    def test_tag_jobs_do_not_grant_candidate_or_gate_release_writes(self):
        text=(ROOT/'.github/workflows/release-control.yml').read_text()
        gate=text.split('  gate:',1)[1].split('  build:',1)[0]
        build=text.split('  build:',1)[1].split('  publish:',1)[0]
        publish=text.split('  publish:',1)[1]
        self.assertNotIn('contents: write',gate)
        self.assertNotIn('id-token: write',gate)
        self.assertNotIn('attestations: write',gate)
        self.assertNotIn('contents: write',build)
        self.assertIn("github.event_name == 'push'",publish)
        self.assertIn("needs.gate.result == 'success'",publish)
        self.assertNotIn('id-token: write',publish)
        self.assertIn('merge-multiple: false',publish)
        self.assertIn('cancel-in-progress: false',text)
        self.assertIn('AGS_RELEASE_PUBLICATION_ENABLED',text)

    def test_synthetic_verification_command_has_all_hard_policy_flags(self):
        args = verify.verification_command('synthetic.txt', synthetic_policy(), 'linux_amd64')
        self.assertEqual(args[args.index('--source-digest') + 1], CONTROL)
        self.assertEqual(args[args.index('--signer-workflow') + 1], verify.WORKFLOW)
        self.assertEqual(args[args.index('--signer-digest') + 1], CONTROL)
        self.assertIn('--deny-self-hosted-runners', args)
        self.assertIn('--predicate-type', args)

    def test_synthetic_revision_tree_and_subject_changes_fail(self):
        good = synthetic_result('d' * 64)
        verify.check_verified_statement(good, 'd' * 64)
        for field, value in (('runtime_revision', 'a' * 40), ('runtime_tree', verify.RUNTIME_TREE[:-1] + '0')):
            bad = copy.deepcopy(good)
            bad[0]['verificationResult']['statement']['predicate'][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(verify.Rejected, 'binding_unverified'):
                verify.check_verified_statement(bad, 'd' * 64)
        with self.assertRaisesRegex(verify.Rejected, 'digest_mismatch'):
            verify.check_verified_statement(good, 'e' * 64)

    def test_synthetic_old_or_omitted_source_digest_and_legacy_signer_fail(self):
        for value in (verify.RUNTIME_REVISION, verify.RUNTIME_REVISION[:8], None):
            pin = synthetic_policy()
            pin['targets']['linux_amd64']['provenance']['control_revision'] = value
            with self.subTest(value=value), self.assertRaises(verify.Rejected):
                verify.verification_command('synthetic.txt', pin, 'linux_amd64')
        pin = synthetic_policy()
        pin['targets']['linux_amd64']['provenance']['signer_workflow'] = verify.REPOSITORY + '/.github/workflows/release.yml'
        with self.assertRaisesRegex(verify.Rejected, 'attestation_failed'):
            verify.verification_command('synthetic.txt', pin, 'linux_amd64')

    def test_synthetic_gh_failure_including_self_hosted_denial_is_not_ignored(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / 'synthetic.txt'
            archive.write_bytes(b'not a signed bundle')
            with mock.patch.object(verify.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1, b'', b'self-hosted denied')):
                with self.assertRaisesRegex(verify.Rejected, 'attestation_failed'):
                    verify.verify_evidence(archive, synthetic_policy(), 'linux_amd64')

    def test_synthetic_material_exercises_post_gh_path_but_never_enables_production(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / 'synthetic.txt'
            archive.write_bytes(b'synthetic evidence, NOT a Sigstore bundle')
            payload = json.dumps(synthetic_result(hashlib.sha256(archive.read_bytes()).hexdigest())).encode()
            with mock.patch.object(verify.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, payload, b'')):
                verify.verify_evidence(archive, synthetic_policy(), 'linux_amd64')
            with mock.patch.object(verify.subprocess, 'run', side_effect=AssertionError('disabled before network')):
                with self.assertRaisesRegex(verify.Rejected, 'binding_unverified') as error:
                    verify.verify_production(archive, synthetic_policy(), 'linux_amd64')
                self.assertEqual(error.exception.exit_code, 65)
            pin_path = Path(directory) / 'synthetic-policy-fragment.json'
            pin_path.write_text(json.dumps(synthetic_policy()))
            result = subprocess.run([str(ROOT / 'scripts/ags-python.sh'), str(ROOT / 'scripts/release_verify.py'), str(archive),
                                     '--pin', str(pin_path), '--target', 'linux_amd64'], capture_output=True)
            self.assertEqual(result.returncode, 65)
            self.assertEqual(result.stderr.strip(), b'release_source_binding_unverified')

    def test_pin_v1_and_retired_linux_abi_are_not_aliases(self):
        for field, value in (('schema', 'ags.abi.v1'), ('build_distribution_version', '24.04'),
                             ('minimum_libc', '2.39'), ('arch', 'arm64')):
            pin = synthetic_policy()
            pin['targets']['linux_amd64']['abi'][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(verify.Rejected, 'abi_incompatible'):
                verify.binding_policy(pin, 'linux_amd64')
        pin = synthetic_policy()
        pin['schema'] = 'ags.deploy-release-pin.v1'
        with self.assertRaisesRegex(verify.Rejected, 'pin_invalid'):
            verify.binding_policy(pin, 'linux_amd64')

    def test_native_distribution_check_does_not_substitute_symbol_versions(self):
        release.linux_builder('ID=ubuntu\nVERSION_ID="22.04"\n')
        for text in ('ID=ubuntu\nVERSION_ID="24.04"\n', 'ID=debian\nVERSION_ID="22.04"\n'):
            with self.assertRaisesRegex(ValueError, '22.04'):
                release.linux_builder(text)
        headers = '[Requesting program interpreter: /lib64/ld-linux-x86-64.so.2]'
        with mock.patch.object(release.Path, 'is_file', return_value=True):
            release.linux_dependencies('Name: GLIBC_2.34', headers, 'libc.so.6 => /lib/libc.so.6')
            for versions, libraries in (('GLIBC_2.39', 'libc.so.6 => /lib/libc.so.6'),
                                        ('GLIBC_2.35', 'libc.so.6 => not found'), ('', '')):
                with self.assertRaises(ValueError):
                    release.linux_dependencies(versions, headers, libraries)

    def test_workflow_blocks_have_only_dispatch_and_exact_native_matrix(self):
        # Match the repository's dependency-free source-contract checks. Hosted
        # CI pins Bun 1.2.17, which has no Bun.YAML.parse API.
        text=(ROOT/'.github/workflows/release-control.yml').read_text()
        def block(name):
            match=re.search(r'^'+name+r':[^\n]*\n(?:(?:[ \t]+[^\n]*|)\n)*',text,re.M)
            self.assertIsNotNone(match)
            return match.group(0).strip()
        self.assertEqual(block('permissions'),'permissions:\n  contents: read')
        trigger=block('on')
        self.assertRegex(trigger,r"^on:\n  push:\n    tags:\n      - 'fork-\*'\n  workflow_dispatch:\n    inputs:")
        self.assertEqual(re.findall(r'^  ([a-z_]+):',trigger,re.M),['push','workflow_dispatch'])
        self.assertEqual(re.findall(r'^      ([a-z_]+):',trigger,re.M),['runtime_revision','runtime_tree'])
        matrix=re.search(r'^      matrix:\n        include:\n((?:          [^\n]*\n|            [^\n]*\n)+)',text,re.M)
        self.assertIsNotNone(matrix)
        self.assertEqual(matrix.group(1),
                         '          - runner: ubuntu-22.04\n            target: linux_amd64\n'
                         '          - runner: macos-15\n            target: darwin_arm64\n')
        actions=re.findall(r'^\s+(?:- )?uses: (\S+)',text,re.M)
        self.assertGreaterEqual(len(actions),4)
        for action in actions: self.assertRegex(action,r'^[^ ]+@[0-9a-f]{40}$')
        self.assertIn('predicate-type: '+verify.PREDICATE_TYPE,text)
        old=(ROOT/'.github/workflows/release.yml').read_text()
        self.assertEqual(re.findall(r'^  ([a-z_]+):',old.split('jobs:',1)[1],re.M),['reject'])
        self.assertIn('exit 65',old)

    def test_synthetic_build_uses_control_tools_and_writes_matching_predicate(self):
        import io
        import sqlite3
        import tarfile
        payload=io.BytesIO()
        with tarfile.open(fileobj=payload,mode='w') as tf:
            for name in ('LICENSE','NOTICE'):
                raw=b'synthetic build fixture'
                member=tarfile.TarInfo(name);member.size=len(raw)
                tf.addfile(member,io.BytesIO(raw))
        calls=[]
        version='source-'+verify.RUNTIME_REVISION
        def fake_git(*args):
            if args[0]=='show': return '1.26.8'
            if args[0]=='status': return ''
            if args==('rev-parse','HEAD'): return CONTROL
            return verify.RUNTIME_TREE if args[-1].endswith('^{tree}') else verify.RUNTIME_REVISION
        def fake_run(args,**kwargs):
            calls.append((args,kwargs))
            if args[:3]==['go','env','GOVERSION']: return b'go1.26.8'
            if 'archive' in args: return payload.getvalue()
            if args[:2]==['go','build']:
                Path(args[args.index('-o')+1]).write_bytes(b'synthetic executable, not runnable')
                self.assertEqual(kwargs['env']['CGO_ENABLED'],'1' if args[-1]=='./cmd/gh-server' else '0')
                for key,value in (('GOTOOLCHAIN','local'),('GOENV','off'),('GOWORK','off')):
                    self.assertEqual(kwargs['env'][key],value)
                return b''
            if args[-1:] == ['--version']:
                command=Path(args[0]).name
                return json.dumps({'schema':'ags.build.v1','command':command,'version':version,
                                   'revision':verify.RUNTIME_REVISION,'tree':verify.RUNTIME_TREE,
                                   'goos':'linux','goarch':'amd64','go_version':'go1.26.8'}).encode()
            if args[:2]==['readelf','--version-info']: return b'Name: GLIBC_2.34'
            if args[:2]==['readelf','-l']: return b'[Requesting program interpreter: /lib64/ld-linux-x86-64.so.2]'
            if args[0]=='ldd': return b'libc.so.6 => /lib/libc.so.6'
            if args[:2]==['bash','scripts/build-exact-source.sh']:
                self.assertEqual(kwargs['env']['GIT_SHA'],CONTROL)
                self.assertNotIn('cwd',kwargs)
                return b''
            if 'fsck' in args: return b''
            raise AssertionError(args)
        def fake_smoke(binary,env,cwd,*args,**kwargs):
            if binary.name=='gh-server':
                db=sqlite3.connect(cwd/'smoke.sqlite')
                for name in ('users','repositories','access_grants'):
                    db.execute('CREATE TABLE '+name+' (id INTEGER)')
                db.commit();db.close()
        previous_umask=os.umask(0o077)
        try:
            with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=fake_git),mock.patch.object(release,'run',side_effect=fake_run),mock.patch.object(release,'smoke',side_effect=fake_smoke),mock.patch.object(release.platform,'system',return_value='Linux'),mock.patch.object(release.platform,'machine',return_value='x86_64'),mock.patch.object(release,'linux_builder') as distro,mock.patch.object(release,'linux_dependencies'):
                # Only os-release is read on the host; the Linux baseline decision
                # itself is covered by the non-mocked distribution tests.
                output=Path(directory)/'out'
                with mock.patch.object(release.Path,'read_text',return_value='ID=ubuntu\nVERSION_ID=22.04'):
                    result=release.build(version,output,runtime_revision=verify.RUNTIME_REVISION,runtime_tree=verify.RUNTIME_TREE)
                predicate=json.loads((output/'runtime-predicate.json').read_text())
                self.assertEqual(predicate,synthetic_result('d'*64)[0]['verificationResult']['statement']['predicate'])
                self.assertEqual(result['source'],verify.RUNTIME_REVISION)
                self.assertEqual(len([args for args,_ in calls if args[:2]==['go','build']]),3)
                distro.assert_called_once()
        finally:
            os.umask(previous_umask)

    def test_macho_minimum_is_checked_not_just_declared(self):
        release.darwin_minimum('cmd LC_BUILD_VERSION\n minos 15.0\n sdk 15.5\n')
        for text in ('minos 14.0', 'minos 16.0', 'sdk 15.0', ''):
            with self.subTest(text=text),self.assertRaises(ValueError):
                release.darwin_minimum(text)

    def test_runtime_build_rejects_wrong_revision_tree_before_tools_or_staging(self):
        for revision, tree in ((verify.RUNTIME_REVISION[:8], verify.RUNTIME_TREE),
                               (verify.RUNTIME_REVISION, 'f'*40),
                               (CONTROL, verify.RUNTIME_TREE)):
            def fake_git(*args):
                if args==('rev-parse','HEAD'): return CONTROL
                if args[-1].endswith('^{commit}'): return verify.RUNTIME_REVISION
                return verify.RUNTIME_TREE
            with mock.patch.object(release,'git',side_effect=fake_git),mock.patch.object(release,'run',side_effect=AssertionError('must reject before tools')):
                with self.subTest(revision=revision,tree=tree),self.assertRaises(ValueError):
                    release.build('source-'+revision,'/unused',runtime_revision=revision,runtime_tree=tree)

    def test_pre_enablement_pin_must_not_claim_separated_control_mode(self):
        pin=synthetic_policy()
        pin['targets']['linux_amd64']['provenance']['mode']=verify.MODE
        with self.assertRaisesRegex(verify.Rejected,'binding_unverified'):
            verify.binding_policy(pin,'linux_amd64')

    def test_no_runtime_build_executes_historical_builder_scripts(self):
        text = (ROOT / 'fork/scripts/release.py').read_text()
        self.assertIn('GIT_SHA=control', text)
        self.assertIn("unpack_source(run(['git','--no-replace-objects','archive','--format=tar',sha]),source)", text)
        self.assertNotIn("run(['bash','scripts/build-exact-source.sh','verify'],cwd=source", text)


class InvocationBindingTests(unittest.TestCase):
    def test_signed_certificate_must_bind_current_tag_push_run_attempt(self):
        uri='https://github.com/'+verify.REPOSITORY+'/actions/runs/123/attempts/1'
        ref='refs/tags/fork-20260924.5-rc5'
        with tempfile.TemporaryDirectory() as directory:
            archive=Path(directory)/'synthetic.txt';archive.write_bytes(b'fixture')
            result=synthetic_result(hashlib.sha256(archive.read_bytes()).hexdigest())
            certificate={'runInvocationURI':uri,'sourceRepositoryRef':ref,'buildTrigger':'push'}
            result[0]['verificationResult']['signature']={'certificate':certificate}
            for field,value in ((None,None),('runInvocationURI',uri+'0'),('sourceRepositoryRef','refs/heads/main'),('buildTrigger','workflow_dispatch')):
                rows=copy.deepcopy(result)
                if field: rows[0]['verificationResult']['signature']['certificate'][field]=value
                with mock.patch.object(verify.subprocess,'run',return_value=subprocess.CompletedProcess([],0,json.dumps(rows).encode(),b'')):
                    if field:
                        with self.assertRaisesRegex(verify.Rejected,'attestation_failed'):
                            verify.verify_evidence(archive,synthetic_policy(),'linux_amd64',invocation_uri=uri,source_ref=ref)
                    else:
                        verify.verify_evidence(archive,synthetic_policy(),'linux_amd64',invocation_uri=uri,source_ref=ref)

class ReviewedSourceTests(unittest.TestCase):
    def test_binding_uses_reviewed_source_not_pin_chosen_runtime(self):
        source=json.loads((ROOT/'fork/release-source.json').read_text())
        pin=synthetic_policy()
        pin['runtime']=source['runtime']
        verify.binding_policy(pin,'linux_amd64',source=source)
        pin['runtime']={'revision':'e'*40,'tree':'f'*40}
        with self.assertRaisesRegex(verify.Rejected,'identity_mismatch'):
            verify.binding_policy(pin,'linux_amd64',source=source)

    def test_shared_contract_does_not_import_installer(self):
        text=(ROOT/'scripts/release_contract.py').read_text()
        self.assertNotIn('install-release.py',text)
        self.assertNotIn('install-release.py',(ROOT/'scripts/release_verify.py').read_text())
        self.assertFalse(verify.SEPARATED_CONTROL_ENABLED)

if __name__ == '__main__':
    unittest.main()
