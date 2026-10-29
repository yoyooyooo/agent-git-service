#!/usr/bin/env python3
"""Release safety regressions; no network, real credentials or deployed state."""
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

ROOT=Path(__file__).resolve().parents[2]
def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
release=load('ags_release',Path(__file__).with_name('release.py'))
installer=load('ags_installer',ROOT/'scripts/install-release.py')
VER='fork-20260924.1-rc1';SOURCE='a'*40;TREE='b'*40
PINNED_GO='go'+(ROOT/'.go-version').read_text().strip()

def asset_git(*args):
    return PINNED_GO.removeprefix('go') if args[0]=='show' and args[-1].endswith(':.go-version') else TREE

class ReleaseTests(unittest.TestCase):
    def test_version_is_not_a_shell_or_path(self):
        self.assertEqual(release.validated_version(VER),VER)
        for value in ('latest','../v1','fork-20260924.1;echo bad','fork-20260924.1\n','fork-20260924.0','--help'):
            with self.assertRaises(ValueError): release.validated_version(value)

    def test_build_environment_does_not_inherit_operator_state(self):
        with mock.patch.dict(os.environ,{'GITHUB_TOKEN':'synthetic','AGS_INTEGRATIONS_CONFIG':'private','GIT_CONFIG_COUNT':'1','GIT_ASKPASS':'private','MULTICA_TOKEN':'synthetic','PATH':'/usr/bin'},clear=True):
            env=release.environment(Path('/example-home'))
            self.assertEqual(env['PATH'],'/usr/bin')
            for key in ('GITHUB_TOKEN','AGS_INTEGRATIONS_CONFIG','GIT_CONFIG_COUNT','GIT_ASKPASS','MULTICA_TOKEN'):
                self.assertNotIn(key,env)

    def gate_api(self, *, bad_sha=False, skipped=False, incomplete=False, existing_release=False, tag_missing=False, tag_wrong=False):
        def api(path,**kwargs):
            if path=='repos/'+release.REPOSITORY:
                return {'id':release.REPOSITORY_ID,'private':False,'fork':True,'parent':{'full_name':'ngaut/agent-git-service'},'default_branch':'fork/main.20260924'}
            if '/actions/runs?' in path:
                return {'workflow_runs':[{'id':n,'head_sha':('c'*40 if bad_sha else SOURCE),'path':p,'status':'completed','conclusion':'success','repository':{'id':release.REPOSITORY_ID}} for n,p in enumerate(('.github/workflows/ci.yml','.github/workflows/secret-scan.yml'),1)]}
            if '/jobs?' in path:
                n=10 if '/1/' in path else 1
                rows=[{'conclusion':'success'} for _ in range(n)]
                if skipped: rows[0]['conclusion']='skipped'
                return {'total_count':n+int(incomplete),'jobs':rows}
            if '/git/ref/tags/' in path:
                if tag_missing: return None
                return {'object':{'type':'commit','sha':'c'*40 if tag_wrong else SOURCE}}
            if '/releases/tags/' in path: return {'id':99} if existing_release else None
            if '/releases?per_page=' in path: return []
            raise AssertionError(path)
        return api

    def test_tag_paths_fail_before_any_network_or_write(self):
        with mock.patch.object(release,'api',side_effect=AssertionError('no network')),mock.patch.object(release,'run',side_effect=AssertionError('no write')):
            for action in (lambda:release.gate(VER,SOURCE),lambda:release.publish(VER,SOURCE,'/unused'),
                           lambda:release.recover_draft(VER,SOURCE,'/unused',123),
                           lambda:release.finish_draft(VER,SOURCE,123,[]),lambda:release.build(VER,'/unused')):
                with self.assertRaisesRegex(ValueError,'release_source_binding_unverified'):
                    action()

    def make_bundle(self,root,target='darwin_arm64',extra=None,go_version=PINNED_GO,diagnostics_verified=True):
        binaries={};contents={}
        os_name,arch=target.split('_')
        for command in release.COMMANDS:
            raw=('synthetic-'+command).encode();contents['bin/'+command]=raw
            identity={'schema':'ags.build.v1','command':command,'version':VER,'revision':SOURCE,'tree':TREE,'goos':os_name,'goarch':arch,'go_version':go_version}
            binaries[command]={'sha256':hashlib.sha256(raw).hexdigest(),'size':len(raw),'identity':identity,'cgo_enabled':command=='gh-server'}
        info={'schema':installer.MANIFEST_SCHEMA,'abi':installer.abi_for(target),'toolchain':{'go_version':'go1.26.8','pin_path':'.go-version'},'repository':release.REPOSITORY,'repository_id':release.REPOSITORY_ID,'revision':SOURCE,'tree':TREE,'version':VER,'target':target,'binaries':binaries,'smoke':{'primary_readiness':True,'sqlite':True,'edge_liveness':True,'edge_build_identity':True,'clean_shutdown':True}}
        if not diagnostics_verified: info['smoke'].pop('edge_build_identity')
        contents['LICENSE']=b'Synthetic fixture license';contents['NOTICE']=b'Synthetic attribution';contents['build-info.json']=json.dumps(info).encode()
        bundle=root/('ags-'+VER+'-'+target+'.tar.gz')
        with tarfile.open(bundle,'w:gz') as tf:
            for name,data in contents.items():
                member=tarfile.TarInfo(name);member.size=len(data);tf.addfile(member,io.BytesIO(data))
            if extra:
                member=tarfile.TarInfo(extra)
                if extra=='symlink':member.name='NOTICE';member.type=tarfile.SYMTYPE;member.linkname='/outside'
                tf.addfile(member,io.BytesIO(b''))
        (root/(bundle.name+'.sha256')).write_text(release.digest(bundle)+'  '+bundle.name+'\n')
        (root/('build-info-'+target+'.json')).write_text(json.dumps(info))
        return bundle

    def test_all_assets_are_exact_and_no_archive_path_escape_or_links(self):
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=asset_git):
            root=Path(directory)/'private';root.mkdir()
            self.make_bundle(root);self.make_bundle(root,'linux_amd64')
            assets,lines=release.check_assets(root,VER,SOURCE)
            self.assertEqual((len(assets),len(lines)),(4,2))
            archive=root/('ags-'+VER+'-darwin_arm64.tar.gz')
            self.assertEqual(installer.inspect_bundle(archive,VER,SOURCE,'darwin_arm64')['revision'],SOURCE)
            for name in ('../outside','bin/gh-server','unexpected.secret','symlink'):
                self.make_bundle(root,extra=name)
                with self.assertRaises(ValueError): release.check_assets(root,VER,SOURCE)
                with self.assertRaises(ValueError): installer.inspect_bundle(archive,VER,SOURCE,'darwin_arm64')
                self.assertFalse((root.parent/'outside').exists())

    def test_legacy_asset_verification_does_not_reuse_v1_signer_for_v2(self):
        with mock.patch.object(release,'run',side_effect=AssertionError('no legacy signer')):
            with self.assertRaisesRegex(ValueError,'binding_unverified'):
                release.verified_assets('/unused',VER,SOURCE)

    def test_download_manifest_source_and_platform_are_not_just_labels(self):
        with tempfile.TemporaryDirectory() as directory:
            archive=self.make_bundle(Path(directory))
            for version,source,target in ((VER,'c'*40,'darwin_arm64'),(VER,SOURCE,'linux_amd64'),('fork-20260924.2',SOURCE,'darwin_arm64')):
                with self.assertRaises(ValueError):installer.inspect_bundle(archive,version,source,target)

    def test_legacy_v1_is_read_only_history_not_a_v2_alias(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            archive=self.make_bundle(root)
            info=json.loads((root/'build-info-darwin_arm64.json').read_text())
            installer.validate_manifest_contract(info,'darwin_arm64')
            with self.assertRaisesRegex(installer.SourceBindingUnverified,'binding_unverified'):
                installer.require_installable_manifest(info)
            old={**info,'schema':installer.LEGACY_MANIFEST_SCHEMA}
            with self.assertRaisesRegex(ValueError,'manifest_invalid'):
                installer.validate_manifest_contract(old,'darwin_arm64',allow_legacy=True)
            old.pop('abi');old.pop('toolchain')
            installer.validate_manifest_contract(old,'darwin_arm64',allow_legacy=True)
            installer.require_installable_manifest(old)
            with self.assertRaisesRegex(ValueError,'manifest_invalid'):
                installer.validate_manifest_contract(old,'darwin_arm64')

    def test_v2_consumer_rejects_incompatible_linux_and_cgo_contract(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);self.make_bundle(root,'linux_amd64')
            info=json.loads((root/'build-info-linux_amd64.json').read_text())
            installer.validate_manifest_contract(info,'linux_amd64')
            import copy
            for change in ('24.04','2.39','cgo','schema'):
                bad=copy.deepcopy(info)
                if change=='24.04': bad['abi']['build_distribution_version']=change
                elif change=='2.39': bad['abi']['minimum_libc']=change
                elif change=='cgo': bad['binaries']['gh-server']['cgo_enabled']=False
                else: bad['abi']['schema']='ags.abi.v1'
                with self.subTest(change=change),self.assertRaises(ValueError):
                    installer.validate_manifest_contract(bad,'linux_amd64')

    def test_draft_lookup_covers_unpublished_version_without_tag(self):
        draft={'id':123,'tag_name':VER,'target_commitish':SOURCE,'draft':True,'immutable':False}
        def api(path,**kwargs):
            if '/releases/tags/' in path:return None
            if '/releases?per_page=' in path:return [draft]
            raise AssertionError(path)
        with mock.patch.object(release,'api',side_effect=api):
            self.assertEqual(release.lookup_release(VER)['id'],123)
        for drift in ({'target_commitish':'main'},{'id':124},{'draft':False},{'tag_name':'other'}):
            with self.assertRaises(ValueError):release.require_draft({**draft,**drift},VER,SOURCE,123)

    def test_toolchain_pin_is_exact_and_does_not_accept_aliases(self):
        for raw in ('1.26.8', '1.27.1'):
            with mock.patch.object(release,'git',return_value=raw):
                self.assertEqual(release.pinned_toolchain(SOURCE),'go'+raw)
        for raw in ('1.26','stable','latest','go1.26.8','1.26.8rc1','1.26.8\nextra','1.26.8+auto'):
            with mock.patch.object(release,'git',return_value=raw),self.assertRaises(ValueError):
                release.pinned_toolchain(SOURCE)

    def test_build_rejects_wrong_toolchain_before_staging_or_building(self):
        def source_git(*args):
            if args[0]=='show': return PINNED_GO.removeprefix('go')
            return TREE if args[-1].endswith('^{tree}') else ('c'*40 if args==('rev-parse','HEAD') else SOURCE)
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=source_git),mock.patch.object(release.platform,'system',return_value='Darwin'),mock.patch.object(release.platform,'machine',return_value='arm64'),mock.patch.object(release.platform,'mac_ver',return_value=('15.0','','')),mock.patch.object(release,'run',return_value=b'go1.25.0\n') as runner:
            output=Path(directory)/'output'
            with self.assertRaisesRegex(ValueError,'toolchain'):
                release.build('source-'+SOURCE,output,runtime_revision=SOURCE,runtime_tree=TREE)
            self.assertFalse(output.exists())
            runner.assert_called_once()
            self.assertEqual(runner.call_args.args[0],['go','env','GOVERSION'])
            self.assertEqual(runner.call_args.kwargs['env']['GOTOOLCHAIN'],'local')
            self.assertEqual(runner.call_args.kwargs['env']['GOENV'],'off')

    def test_publication_rejects_internally_consistent_old_toolchain_bundle(self):
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=asset_git):
            root=Path(directory)
            self.make_bundle(root,go_version='go1.25.0')
            self.make_bundle(root,'linux_amd64')
            with self.assertRaisesRegex(ValueError,'toolchain|abi_incompatible'):
                release.check_assets(root,VER,SOURCE)

    def test_release_requires_actual_edge_diagnostic_identity_smoke(self):
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=asset_git):
            root=Path(directory)
            self.make_bundle(root,diagnostics_verified=False)
            self.make_bundle(root,'linux_amd64')
            with self.assertRaisesRegex(ValueError,'smoke verification'):
                release.check_assets(root,VER,SOURCE)

    def test_all_hosted_go_jobs_use_one_explicit_pin(self):
        import re
        pins=[]
        for workflow in (ROOT/'.github/workflows').glob('*.yml'):
            text=workflow.read_text()
            if 'actions/setup-go@' not in text: continue
            found=re.findall(r'^\s+go-version-file:\s*(\S+)\s*$',text,re.M)
            self.assertEqual(len(found),text.count('actions/setup-go@'),str(workflow))
            self.assertTrue(all(pin=='.go-version' for pin in found),str(workflow))
            self.assertNotRegex(text,r'^\s+go-version:',str(workflow))
            self.assertIn('GOTOOLCHAIN: local',text)
            pins.extend(found)
        self.assertGreaterEqual(len(pins),7)

    def test_old_tag_workflow_cannot_build_sign_or_publish(self):
        text=(ROOT/'.github/workflows/release.yml').read_text()
        self.assertIn("tags:\n      - 'fork-*'",text)
        self.assertNotIn('workflow_dispatch:',text)
        self.assertIn('exit 65',text)
        for forbidden in ('contents: write','attestations: write','release.py publish','runner: ubuntu-24.04'):
            self.assertNotIn(forbidden,text)

if __name__=='__main__':unittest.main()
