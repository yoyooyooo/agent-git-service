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

    def test_release_gate_requires_preexisting_exact_tag_complete_ci_and_unused_release(self):
        def fake_git(*args): return TREE if '^{tree}' in args[-1] else SOURCE
        with mock.patch.object(release,'git',side_effect=fake_git),mock.patch.dict(os.environ,{},clear=True):
            with mock.patch.object(release,'api',side_effect=self.gate_api()): self.assertEqual(release.gate(VER,SOURCE)['source'],SOURCE)
            for options in ({'bad_sha':True},{'skipped':True},{'incomplete':True},{'existing_release':True},{'tag_missing':True},{'tag_wrong':True}):
                with mock.patch.object(release,'api',side_effect=self.gate_api(**options)),self.assertRaises(ValueError): release.gate(VER,SOURCE)

    def make_bundle(self,root,target='darwin_arm64',extra=None,go_version=PINNED_GO,diagnostics_verified=True):
        binaries={};contents={}
        os_name,arch=target.split('_')
        for command in release.COMMANDS:
            raw=('synthetic-'+command).encode();contents['bin/'+command]=raw
            identity={'schema':'ags.build.v1','command':command,'version':VER,'revision':SOURCE,'tree':TREE,'goos':os_name,'goarch':arch,'go_version':go_version}
            binaries[command]={'sha256':hashlib.sha256(raw).hexdigest(),'size':len(raw),'identity':identity,'cgo_enabled':command=='gh-server'}
        info={'schema':'ags.release.v1','repository':release.REPOSITORY,'repository_id':release.REPOSITORY_ID,'revision':SOURCE,'tree':TREE,'version':VER,'target':target,'binaries':binaries,'smoke':{'primary_readiness':True,'sqlite':True,'edge_liveness':True,'edge_build_identity':True,'clean_shutdown':True}}
        if not diagnostics_verified: info['smoke'].pop('edge_build_identity')
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
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=asset_git):
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

    def test_verified_assets_include_exact_source_installer_bootstrap(self):
        bootstrap=b'#!/usr/bin/env bash\necho synthetic-bootstrap\n'
        def fake_git(*args):
            if args[0]=='show' and args[-1].endswith(':.go-version'): return PINNED_GO.removeprefix('go')
            if args[0]=='ls-tree': return '100755 blob deadbeef\tscripts/install.sh'
            return TREE
        def fake_run(args,**kwargs):
            if args[:3]==['git','--no-replace-objects','show'] and args[-1]==SOURCE+':scripts/install.sh':
                return bootstrap
            if args[:3]==['gh','attestation','verify']:
                return b''
            raise AssertionError(args)
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=fake_git),mock.patch.object(release,'run',side_effect=fake_run):
            root=Path(directory)
            self.make_bundle(root); self.make_bundle(root,'linux_amd64')
            assets=release.verified_assets(root,VER,SOURCE)
            self.assertIn('install.sh',{p.name for p in assets})
            self.assertEqual((root/'install.sh').read_bytes(),bootstrap)
            self.assertEqual((root/'install.sh').stat().st_mode & 0o777,0o755)
            sums=(root/'SHA256SUMS').read_text().splitlines()
            self.assertIn(hashlib.sha256(bootstrap).hexdigest()+'  install.sh',sums)

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

    def test_numeric_draft_finish_requires_preexisting_exact_tag_and_never_reuploads(self):
        with tempfile.TemporaryDirectory() as directory:
            asset=Path(directory)/'fixture';asset.write_bytes(b'release-fixture')
            row={'name':asset.name,'size':asset.stat().st_size,'digest':'sha256:'+release.digest(asset),'state':'uploaded'}
            for case in ('missing-tag','existing-exact-tag','wrong-tag','asset-drift'):
                state={'published':False,'writes':[]}
                def api(path,method='GET',fields=(),missing=False,**kwargs):
                    if '/git/ref/tags/' in path:
                        if case=='missing-tag': return None
                        return {'object':{'type':'commit','sha':'c'*40 if case=='wrong-tag' else SOURCE}}
                    if path.endswith('/releases/123'):
                        if method=='PATCH':
                            self.assertEqual(dict(fields),{'draft':False,'make_latest':'false'})
                            state['published']=True;state['writes'].append('publish')
                        changed={**row,'digest':'sha256:'+'0'*64} if case=='asset-drift' else row
                        return {'id':123,'tag_name':VER,'target_commitish':SOURCE,'draft':not state['published'],'immutable':state['published'],'assets':[changed],'html_url':'https://example.test/release'}
                    raise AssertionError(path)
                with mock.patch.object(release,'api',side_effect=api),mock.patch.object(release,'run',side_effect=AssertionError('no upload or command replay during finish')):
                    if case in ('missing-tag','wrong-tag','asset-drift'):
                        with self.assertRaises(ValueError):release.finish_draft(VER,SOURCE,123,[asset])
                        self.assertNotIn('publish',state['writes'])
                    else:
                        self.assertTrue(release.finish_draft(VER,SOURCE,123,[asset])['immutable'])
                        self.assertEqual(state['writes'],['publish'])

    def test_stable_publication_marks_release_latest(self):
        stable='fork-20260924.1'
        with tempfile.TemporaryDirectory() as directory:
            asset=Path(directory)/'fixture';asset.write_bytes(b'release-fixture')
            row={'name':asset.name,'size':asset.stat().st_size,'digest':'sha256:'+release.digest(asset),'state':'uploaded'}
            state={'published':False,'make_latest':None}
            def api(path,method='GET',fields=(),**kwargs):
                if '/git/ref/tags/' in path:return {'object':{'type':'commit','sha':SOURCE}}
                if path.endswith('/releases/123'):
                    if method=='PATCH':
                        state['make_latest']=dict(fields)['make_latest'];state['published']=True
                    return {'id':123,'tag_name':stable,'target_commitish':SOURCE,'draft':not state['published'],'immutable':state['published'],'assets':[row],'html_url':'https://example.test/stable'}
                raise AssertionError(path)
            with mock.patch.object(release,'api',side_effect=api),mock.patch.object(release,'run',side_effect=AssertionError('no command replay during finish')):
                release.finish_draft(stable,SOURCE,123,[asset])
            self.assertEqual(state['make_latest'],'true')

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
            return TREE if args[-1].endswith('^{tree}') else SOURCE
        with tempfile.TemporaryDirectory() as directory,mock.patch.object(release,'git',side_effect=source_git),mock.patch.object(release.platform,'system',return_value='Darwin'),mock.patch.object(release.platform,'machine',return_value='arm64'),mock.patch.object(release,'run',return_value=b'go1.25.0\n') as runner:
            output=Path(directory)/'output'
            with self.assertRaisesRegex(ValueError,'toolchain'):
                release.build(VER,output)
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
            with self.assertRaisesRegex(ValueError,'toolchain'):
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

    def test_release_is_tag_driven_and_uses_attested_native_targets(self):
        text=(ROOT/'.github/workflows/release.yml').read_text()
        self.assertIn("tags:\n      - 'fork-*'",text)
        self.assertNotIn('workflow_dispatch:',text)
        for trigger in ('pull_request:', 'pull_request_target:', 'workflow_run:'):
            self.assertNotIn(trigger,text)
        self.assertIn('RELEASE_VERSION: ${{ github.ref_name }}',text)
        self.assertIn("github.ref_type == 'tag'",text)
        self.assertIn('macos-15',text);self.assertIn('ubuntu-24.04',text)
        self.assertIn('persist-credentials: false',text);self.assertIn('attestations: write',text)
        self.assertIn('cancel-in-progress: false',text)

if __name__=='__main__':unittest.main()
