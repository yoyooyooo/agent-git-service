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

    def gate_api(self, *, bad_sha=False, skipped=False, incomplete=False, existing=False):
        def api(path,**kwargs):
            if path=='repos/'+release.REPOSITORY: return {'id':release.REPOSITORY_ID,'private':False,'default_branch':'fork/main.20260924'}
            if '/actions/runs?' in path:
                return {'workflow_runs':[{'id':n,'head_sha':('c'*40 if bad_sha else SOURCE),'path':p,'status':'completed','conclusion':'success','repository':{'id':release.REPOSITORY_ID}} for n,p in enumerate(('.github/workflows/ci.yml','.github/workflows/secret-scan.yml'),1)]}
            if '/jobs?' in path:
                n=10 if '/1/' in path else 1
                rows=[{'conclusion':'success'} for _ in range(n)]
                if skipped: rows[0]['conclusion']='skipped'
                return {'total_count':n+int(incomplete),'jobs':rows}
            if '/releases?per_page=' in path: return []
            if '/releases/tags/' in path or '/git/ref/tags/' in path: return {'exists':True} if existing else None
            raise AssertionError(path)
        return api

    def test_release_gate_requires_exact_complete_ci_and_unused_version(self):
        def fake_git(*args): return TREE if '^{tree}' in args[-1] else SOURCE
        with mock.patch.object(release,'git',side_effect=fake_git),mock.patch.dict(os.environ,{},clear=True):
            with mock.patch.object(release,'api',side_effect=self.gate_api()): self.assertEqual(release.gate(VER,SOURCE)['source'],SOURCE)
            for options in ({'bad_sha':True},{'skipped':True},{'incomplete':True},{'existing':True}):
                with mock.patch.object(release,'api',side_effect=self.gate_api(**options)),self.assertRaises(ValueError): release.gate(VER,SOURCE)

    def make_bundle(self,root,target='darwin_arm64',extra=None):
        binaries={};contents={}
        os_name,arch=target.split('_')
        for command in release.COMMANDS:
            raw=('synthetic-'+command).encode();contents['bin/'+command]=raw
            identity={'schema':'ags.build.v1','command':command,'version':VER,'revision':SOURCE,'tree':TREE,'goos':os_name,'goarch':arch,'go_version':'go-fixture'}
            binaries[command]={'sha256':hashlib.sha256(raw).hexdigest(),'size':len(raw),'identity':identity,'cgo_enabled':command=='gh-server'}
        info={'schema':'ags.release.v1','repository':release.REPOSITORY,'repository_id':release.REPOSITORY_ID,'revision':SOURCE,'tree':TREE,'version':VER,'target':target,'binaries':binaries,'smoke':{'primary_readiness':True,'sqlite':True,'edge_liveness':True,'clean_shutdown':True}}
        contents['LICENSE']=b'Synthetic fixture license';contents['build-info.json']=json.dumps(info).encode()
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
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',return_value=TREE):
            root=Path(directory)
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

    def test_download_manifest_source_and_platform_are_not_just_labels(self):
        with tempfile.TemporaryDirectory() as directory:
            archive=self.make_bundle(Path(directory))
            for version,source,target in ((VER,'c'*40,'darwin_arm64'),(VER,SOURCE,'linux_amd64'),('fork-20260924.2',SOURCE,'darwin_arm64')):
                with self.assertRaises(ValueError):installer.inspect_bundle(archive,version,source,target)

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

    def test_numeric_draft_finish_creates_exact_absent_tag_and_never_reuploads(self):
        with tempfile.TemporaryDirectory() as directory:
            asset=Path(directory)/'fixture';asset.write_bytes(b'release-fixture')
            row={'name':asset.name,'size':asset.stat().st_size,'digest':'sha256:'+release.digest(asset),'state':'uploaded'}
            for case in ('absent-tag','existing-exact-tag','wrong-tag','asset-drift'):
                state={'ref':None if case=='absent-tag' else {'object':{'type':'commit','sha':'c'*40 if case=='wrong-tag' else SOURCE}},'published':False,'writes':[],'reads':0}
                def api(path,method='GET',fields=(),**kwargs):
                    if path.endswith('/git/refs'):
                        self.assertEqual(method,'POST');self.assertEqual(dict(fields),{'ref':'refs/tags/'+VER,'sha':SOURCE})
                        state['ref']={'object':{'type':'commit','sha':SOURCE}};state['writes'].append('create-tag');return state['ref']
                    if '/git/ref/tags/' in path:return state['ref']
                    if path.endswith('/releases/123'):
                        if method=='PATCH':
                            self.assertEqual(dict(fields),{'draft':False,'make_latest':'false'});state['published']=True;state['writes'].append('publish')
                        state['reads']+=1
                        changed={**row,'digest':'sha256:'+'0'*64} if case=='asset-drift' else row
                        return {'id':123,'tag_name':VER,'target_commitish':SOURCE,'draft':not state['published'],'immutable':state['published'],'assets':[changed],'html_url':'https://example.test/release'}
                    raise AssertionError(path)
                with mock.patch.object(release,'api',side_effect=api),mock.patch.object(release,'run',side_effect=AssertionError('no upload or command replay during finish')):
                    if case in ('wrong-tag','asset-drift'):
                        with self.assertRaises(ValueError):release.finish_draft(VER,SOURCE,123,[asset])
                        self.assertNotIn('publish',state['writes'])
                    else:
                        self.assertTrue(release.finish_draft(VER,SOURCE,123,[asset])['immutable'])
                        self.assertEqual(state['writes'],['create-tag','publish'] if case=='absent-tag' else ['publish'])

    def test_release_is_manual_and_uses_attested_native_targets(self):
        text=(ROOT/'.github/workflows/release.yml').read_text()
        self.assertIn('workflow_dispatch:',text)
        for trigger in ('pull_request:', 'pull_request_target:', 'workflow_run:', '\n  push:'):
            self.assertNotIn(trigger,text)
        self.assertIn('macos-15',text);self.assertIn('ubuntu-24.04',text)
        self.assertIn('persist-credentials: false',text);self.assertIn('attestations: write',text)
        self.assertIn('cancel-in-progress: false',text)

if __name__=='__main__':unittest.main()
