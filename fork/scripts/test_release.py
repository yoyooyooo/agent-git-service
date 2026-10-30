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
        self.assertNotIn("  push:",text)
        self.assertIn('workflow_dispatch:',text)
        self.assertIn('exit 65',text)
        for forbidden in ('contents: write','attestations: write','release.py publish','runner: ubuntu-24.04'):
            self.assertNotIn(forbidden,text)

class TagWiringTests(unittest.TestCase):
    def context(self):
        return {'event_name':'push','ref':'refs/tags/'+VER,'deleted':False,'forced':False,
                'created':True,'before':'0'*40,'after':'c'*40,'sha':'c'*40,
                'workflow_sha':'c'*40,'repository_id':release.REPOSITORY_ID,
                'run_id':'123','run_attempt':'1'}

    def test_tag_event_boundary_rejects_each_invalid_fact_before_network(self):
        good=self.context()
        self.assertEqual(release.tag_event(good),VER)
        for key,value in (('event_name','workflow_dispatch'),('ref','refs/heads/main'),
                          ('ref','refs/tags/fork-20260924.0-rc1'),('deleted',True),
                          ('forced',True),('created',False),('before','a'*40),
                          ('repository_id',1),('after',None),('sha','d'*40),('workflow_sha','d'*40)):
            with self.subTest(key=key,value=value),self.assertRaises(ValueError):
                release.tag_event({**good,key:value})

    def test_gate_switch_missing_or_false_rejects_before_network_or_build(self):
        for enabled in ('','false','True'):
            with mock.patch.dict(os.environ,{'AGS_RELEASE_PUBLICATION_ENABLED':enabled}),mock.patch.object(release,'api',side_effect=AssertionError('no network')):
                with self.assertRaisesRegex(ValueError,'publication_disabled'):
                    release.tag_gate(self.context())

    def test_review_requires_independent_human_current_head_approval(self):
        pr={'user':{'id':1},'head':{'sha':'a'*40}}
        approval={'id':10,'user':{'id':2,'type':'User'},'author_association':'COLLABORATOR','state':'APPROVED','commit_id':'a'*40}
        self.assertEqual(release.review_evidence(pr,[approval]),[10])
        for rows in ([],[{**approval,'commit_id':'b'*40}],
                     [{**approval,'user':{'id':1,'type':'User'}}],
                     [{**approval,'user':{'id':2,'type':'Bot'}}],
                     [{**approval,'author_association':'NONE'}],
                     [approval,{**approval,'id':11,'state':'CHANGES_REQUESTED'}]):
            with self.subTest(rows=rows),self.assertRaisesRegex(ValueError,'human_review'):
                release.review_evidence(pr,rows)

    def test_required_job_inventory_is_exact_not_a_minimum_count(self):
        expected=release.CI_JOBS['.github/workflows/ci.yml']
        self.assertEqual(len(expected),12)
        rows=[{'name':name,'status':'completed','conclusion':'success','run_attempt':2} for name in expected]
        release.validate_jobs(rows,expected,2)
        for bad in (rows[:-1],rows+[rows[0]],[{**r,'conclusion':'skipped'} for r in rows],
                    [{**r,'run_attempt':1} for r in rows]):
            with self.assertRaisesRegex(ValueError,'ci_jobs'):
                release.validate_jobs(bad,expected,2)

    def test_source_manifest_is_strict_and_not_its_own_control(self):
        source={'schema':'ags.release-source.v1','version':VER,
                'runtime':{'revision':SOURCE,'tree':TREE},
                'acceptance':{'runtime_pull_request':69,'disclosure':'fork/PUBLICATION.md'}}
        self.assertEqual(release.source_contract(source,VER,'c'*40),source)
        import copy
        for field,value in (('schema','v0'),('version','fork-20260924.2'),('runtime',{'revision':'c'*40,'tree':TREE}),
                            ('runtime',{'revision':SOURCE[:8],'tree':TREE}),('acceptance',{})):
            bad=copy.deepcopy(source);bad[field]=value
            with self.subTest(field=field),self.assertRaises(ValueError):
                release.source_contract(bad,VER,'c'*40)

    def test_publish_unknown_create_reads_back_never_posts_twice(self):
        draft={'id':10,'draft':True,'immutable':False,'tag_name':VER,'target_commitish':'c'*40,'body':'bound body'}
        with mock.patch.object(release,'lookup_release',side_effect=[None,draft]),mock.patch.object(release,'run',side_effect=RuntimeError('timeout')) as writer:
            self.assertEqual(release.create_draft(VER,'c'*40,'bound body')['id'],10)
            self.assertEqual(writer.call_count,1)
            self.assertIn('--verify-tag',writer.call_args.args[0])
            self.assertIn('--draft',writer.call_args.args[0])
        with mock.patch.object(release,'lookup_release',side_effect=[None,None]),mock.patch.object(release,'run',side_effect=RuntimeError('timeout')) as writer:
            with self.assertRaisesRegex(ValueError,'outcome_unknown'):
                release.create_draft(VER,'c'*40,'bound body')
            self.assertEqual(writer.call_count,1)

    def test_upload_unknown_reads_exact_asset_and_preserves_failed_draft(self):
        with tempfile.TemporaryDirectory() as directory:
            asset=Path(directory)/'test.json';asset.write_bytes(b'fixture')
            remote={'id':10,'draft':True,'assets':[{'name':asset.name,'size':asset.stat().st_size,'digest':'sha256:'+release.digest(asset)}]}
            with mock.patch.object(release,'run',side_effect=RuntimeError('timeout')) as writer,mock.patch.object(release,'api',return_value=remote):
                release.upload_asset(10,VER,asset)
                self.assertEqual(writer.call_count,1)
            with mock.patch.object(release,'run',side_effect=RuntimeError('timeout')) as writer,mock.patch.object(release,'api',return_value={'id':10,'draft':True,'assets':[]}):
                with self.assertRaisesRegex(ValueError,'outcome_unknown'):
                    release.upload_asset(10,VER,asset)
                self.assertEqual(writer.call_count,1)

class CompleteGateTests(unittest.TestCase):
    context = TagWiringTests.context
    def fixture(self):
        self.source={'schema':'ags.release-source.v1','version':VER,
                     'runtime':{'revision':SOURCE,'tree':TREE},
                     'acceptance':{'runtime_pull_request':69,'disclosure':'fork/PUBLICATION.md'}}
        self.drift=None
        self.calls=[]
        self.pr={'number':70,'user':{'id':1},'head':{'sha':'d'*40,'repo':{'id':release.REPOSITORY_ID}},
                 'base':{'ref':release.DEFAULT_BRANCH,'repo':{'id':release.REPOSITORY_ID}},
                 'merged':True,'merge_commit_sha':'c'*40}

    def fake_git(self,*args):
        if args==('rev-parse','HEAD'): return 'c'*40
        if args[0]=='show': return json.dumps(self.source)
        if args[0]=='merge-base':
            if self.drift=='ancestor': raise RuntimeError('not ancestor')
            return ''
        return 'e'*40 if self.drift=='tree' and args[-1]==SOURCE+'^{tree}' else TREE

    def fake_run(self,args,**kwargs):
        if 'show' in args:
            return (ROOT/args[-1].split(':',1)[1]).read_bytes()
        self.calls.append(args)
        return b''

    def fake_api(self,path,**kwargs):
        self.assertEqual(kwargs.get('method','GET'),'GET')
        if path=='repos/'+release.REPOSITORY:
            return {'id':release.REPOSITORY_ID,'full_name':release.REPOSITORY,'private':False,
                    'fork':True,'parent':{'full_name':'ngaut/agent-git-service'},'default_branch':release.DEFAULT_BRANCH}
        if '/actions/variables/' in path: return {'value':'true'}
        if '/git/ref/tags/' in path: return {'object':{'type':'commit','sha':'c'*40}}
        if path.endswith('/commits/'+release.DEFAULT_BRANCH): return {'sha':'f'*40 if self.drift=='tip' else 'c'*40}
        if '/compare/' in path:
            return {'status':'ahead','merge_base_commit':{'sha':'f'*40 if self.drift=='rewrite' else 'c'*40}}
        if '/commits/' in path and '/pulls?' in path:
            revision=path.split('/commits/')[1].split('/')[0]
            return [{'number':69 if revision==SOURCE else 70}]
        if '/pulls/' in path and '/reviews?' in path:
            return [] if self.drift=='review' else [{'id':10,'user':{'id':2,'type':'User'},'author_association':'COLLABORATOR','state':'APPROVED','commit_id':'d'*40}]
        if '/pulls/' in path:
            number=int(path.rsplit('/',1)[1]);return {**self.pr,'number':number,'merge_commit_sha':SOURCE if number==69 else 'c'*40}
        if '/git/commits/' in path: return {'tree':{'sha':TREE}}
        if '/actions/runs?' in path:
            revision=path.split('head_sha=')[1].split('&')[0]
            rows=[{'id':n,'head_sha':revision,'path':name,'status':'completed','conclusion':'success',
                   'event':'push','repository':{'id':release.REPOSITORY_ID},'head_repository':{'id':release.REPOSITORY_ID}}
                  for n,name in enumerate(release.CI_JOBS,1)]
            if self.drift=='missing-'+revision: rows=[]
            return {'total_count':len(rows),'workflow_runs':rows}
        if '/attempts/' in path:
            name=list(release.CI_JOBS)[int(path.split('/runs/')[1].split('/')[0])-1]
            rows=[{'name':name,'status':'completed','conclusion':'success','run_attempt':2} for name in release.CI_JOBS[name]]
            if self.drift=='skipped': rows[0]['conclusion']='skipped'
            return {'total_count':len(rows),'jobs':rows}
        if '/actions/runs/' in path:
            n=int(path.rsplit('/',1)[1]);sha=self.ci_sha
            return {'id':n,'head_sha':sha,'path':list(release.CI_JOBS)[n-1],
                    'status':'in_progress' if self.drift=='pending-'+sha else 'completed',
                    'conclusion':'success','event':'push','run_attempt':2,
                    'repository':{'id':release.REPOSITORY_ID},'head_repository':{'id':release.REPOSITORY_ID}}
        if '/releases/tags/' in path: return {'id':10} if self.drift=='occupied' else None
        if '/releases?' in path: return []
        raise AssertionError(path)

    def gate(self,prior=None):
        def api(path,**kwargs):
            if '/actions/runs?' in path: self.ci_sha=path.split('head_sha=')[1].split('&')[0]
            return self.fake_api(path,**kwargs)
        with mock.patch.dict(os.environ,{'AGS_RELEASE_PUBLICATION_ENABLED':'true'}),mock.patch.object(release,'api',side_effect=api),mock.patch.object(release,'git',side_effect=self.fake_git),mock.patch.object(release,'run',side_effect=self.fake_run):
            return release.tag_gate(self.context(),prior=prior)

    def test_complete_gate_and_each_identity_ci_review_rejection(self):
        self.fixture()
        binding=self.gate()
        self.assertEqual(binding['source'],self.source)
        self.assertEqual(len(binding['ci']['control']),2)
        self.assertEqual(len(binding['ci']['runtime']),2)
        self.assertEqual(self.calls,[['python3','fork/scripts/audit.py','verify'],['python3','fork/scripts/audit.py','publication']])
        for drift,error in (('tip','default_tip'),('ancestor','not_ancestor'),('tree','tree_mismatch'),
                            ('missing-'+'c'*40,'ci_missing'),('missing-'+SOURCE,'ci_missing'),
                            ('pending-'+'c'*40,'ci_incomplete'),('pending-'+SOURCE,'ci_incomplete'),
                            ('skipped','ci_jobs'),('review','human_review'),('occupied','name_occupied')):
            self.drift=drift
            with self.subTest(drift=drift),self.assertRaisesRegex(ValueError,error): self.gate()
        self.drift=None
        self.source['version']='fork-20260924.2'
        with self.assertRaisesRegex(ValueError,'source_invalid'): self.gate()
        self.source['version']=VER
        self.drift='rewrite'
        with self.assertRaisesRegex(ValueError,'rewritten'): self.gate(prior=binding)
        self.drift='tip'
        self.assertEqual(self.gate(prior=binding)['control'],'c'*40)

    def test_repo_tag_checkout_source_switch_and_evidence_fail_closed(self):
        import copy
        self.fixture()
        for drift in ('repo_id','private','parent','branch','tag','checkout','workflow','source','switch','permission','runtime_review'):
            self.fixture()
            original_api=self.fake_api
            def api(path,**kwargs):
                if '/actions/runs?' in path:self.ci_sha=path.split('head_sha=')[1].split('&')[0]
                if drift=='permission' and '/reviews?' in path:raise RuntimeError('read denied')
                result=copy.deepcopy(original_api(path,**kwargs))
                if path=='repos/'+release.REPOSITORY:
                    if drift=='repo_id':result['id']=1
                    if drift=='private':result['private']=True
                    if drift=='parent':result['parent']['full_name']='other/repo'
                    if drift=='branch':result['default_branch']='main'
                if drift=='tag' and '/git/ref/tags/' in path:result['object']['type']='tree'
                if drift=='switch' and '/actions/variables/' in path:result['value']='false'
                if drift=='runtime_review' and path.endswith('/pulls/69'):result['merged']=False
                return result
            def git(*args):
                if drift=='checkout' and args==('rev-parse','HEAD'):return 'e'*40
                return self.fake_git(*args)
            def run(args,**kwargs):
                return b'drift' if drift=='workflow' and 'show' in args else self.fake_run(args,**kwargs)
            if drift=='source':self.source['runtime']['revision']='c'*40
            with self.subTest(drift=drift),mock.patch.dict(os.environ,{'AGS_RELEASE_PUBLICATION_ENABLED':'true'}),mock.patch.object(release,'api',side_effect=api),mock.patch.object(release,'git',side_effect=git),mock.patch.object(release,'run',side_effect=run):
                with self.assertRaises((ValueError,RuntimeError)):release.tag_gate(self.context())

    def test_new_failure_or_pending_run_cannot_hide_behind_old_success(self):
        self.fixture()
        calls=[]
        old=self.fake_api('repos/x/actions/runs?head_sha='+SOURCE)
        newer={**old['workflow_runs'][0],'id':100,'status':'in_progress','conclusion':None}
        def api(path,**kwargs):
            calls.append(path)
            if '/actions/runs?' in path:
                return {'total_count':3,'workflow_runs':old['workflow_runs']+[newer]}
            if path.endswith('/100'): return newer
            raise AssertionError('must not choose older success: '+path)
        with mock.patch.object(release,'api',side_effect=api),self.assertRaisesRegex(ValueError,'ci_incomplete'):
            release.exact_ci(SOURCE)
        self.assertTrue(calls[-1].endswith('/100'))


class TransportUncertaintyTests(unittest.TestCase):
    def test_timeout_becomes_unknown_result_for_readback_state_machine(self):
        import subprocess
        with mock.patch.object(release.subprocess,'run',side_effect=subprocess.TimeoutExpired(['gh'],90)):
            with self.assertRaisesRegex(RuntimeError,'outcome unknown'):release.run(['gh','release','create',VER])
            with self.assertRaisesRegex(RuntimeError,'outcome unknown'):release.api('repos/example/releases',method='POST')

class PublicationStateTests(unittest.TestCase):
    def binding(self):
        return {'schema':'ags.release-gate.v2','version':VER,'control':'c'*40,'run_id':'123','run_attempt':'1',
                'source':{'schema':'ags.release-source.v1','version':VER,
                          'runtime':{'revision':SOURCE,'tree':TREE},
                          'acceptance':{'runtime_pull_request':69,'disclosure':'fork/PUBLICATION.md'}}}

    def parts(self,root,binding):
        for target in release.TARGETS:
            part=root/('release-control-'+target);part.mkdir()
            ReleaseTests().make_bundle(part,target)
            (part/'attestation.sigstore.json').write_text('synthetic, NOT Sigstore')
            (part/'runtime-predicate.json').write_text('{}')
            (part/'build-binding.json').write_text(json.dumps({k:binding[k] for k in ('version','control','source','run_id','run_attempt')}))

    def fake_statement(self,archive,pin,target,**kwargs):
        return {'predicateType':release.binding_verifier().PREDICATE_TYPE,
                'predicate':{'schema':'ags.runtime-binding.v1','runtime_revision':SOURCE,'runtime_tree':TREE},
                'subject':[{'name':archive.name,'digest':{'sha256':release.digest(archive)}}]}

    def assemble(self,root,binding):
        verifier=release.binding_verifier()
        def run(args,**kwargs):
            return json.dumps(binding['source']).encode() if args[-1].endswith('release-source.json') else b'fixture installer'
        with mock.patch.object(release,'binding_verifier',return_value=verifier),mock.patch.object(verifier,'verify_evidence',side_effect=self.fake_statement),mock.patch.object(release,'git',side_effect=asset_git),mock.patch.object(release,'run',side_effect=run):
            return release.tag_assets(root,binding)

    def test_nine_assets_checksums_and_binding_all_targets(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);binding=self.binding();self.parts(root,binding)
            assets=self.assemble(root,binding)
            self.assertEqual(len(assets),9)
            sums=(root/'publication/SHA256SUMS').read_text()
            self.assertEqual(len(sums.splitlines()),8)
            self.assertNotIn('  SHA256SUMS',sums)
            for path in assets:
                if path.name!='SHA256SUMS':self.assertIn(release.digest(path)+'  '+path.name+'\n',sums)
            self.assertEqual(json.loads((root/'publication/release-source.json').read_text()),binding['source'])

    def test_missing_platform_or_any_identity_drift_rejects(self):
        for drift in ('missing','control','source','version','run_id','run_attempt','bundle_collision'):
            with self.subTest(drift=drift),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);binding=self.binding();self.parts(root,binding)
                part=root/'release-control-linux_amd64'
                if drift=='missing': part.rename(root/'unexpected')
                elif drift=='bundle_collision': (part/'extra.sigstore.json').write_text('collision')
                else:
                    receipt=json.loads((part/'build-binding.json').read_text());receipt[drift]='drift'
                    (part/'build-binding.json').write_text(json.dumps(receipt))
                with self.assertRaises((ValueError,FileNotFoundError)):self.assemble(root,binding)

    def test_publish_unknown_patch_reads_back_and_failed_draft_is_retained(self):
        for published in (True,False):
            with self.subTest(published=published),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);binding=self.binding()
                assets=[]
                for n in range(9):
                    path=root/(str(n)+'.json');path.write_bytes(str(n).encode());assets.append(path)
                draft={'id':10,'tag_name':VER,'target_commitish':'c'*40,'draft':True,'immutable':False,
                       'body':release.release_body(binding),'prerelease':True,
                       'assets':[{'name':p.name,'size':p.stat().st_size,'digest':'sha256:'+release.digest(p)} for p in assets]}
                final={**draft,'draft':not published,'immutable':published}
                posts=[];reads=[]
                def api(path,**kwargs):
                    if kwargs.get('method')=='PATCH':
                        posts.append(path);raise RuntimeError('timeout')
                    reads.append(path)
                    return draft if len(reads)==1 else final
                with mock.patch.object(release,'invocation',return_value={}),mock.patch.object(release,'tag_gate'),mock.patch.object(release,'tag_assets',return_value=assets),mock.patch.object(release,'tag_source',return_value='c'*40),mock.patch.object(release,'create_draft',return_value=draft),mock.patch.object(release,'upload_asset'),mock.patch.object(release,'api',side_effect=api),mock.patch.object(release,'run') as runner:
                    if published:
                        self.assertTrue(release.tag_publish(binding,root)['immutable'])
                        self.assertEqual(runner.call_count,9)
                    else:
                        with self.assertRaisesRegex(ValueError,'outcome_unknown'):release.tag_publish(binding,root)
                        runner.assert_not_called()
                    self.assertEqual(len(posts),1)
                    self.assertEqual(len(reads),2)

    def test_remote_requires_each_asset_exact_size_and_digest(self):
        with tempfile.TemporaryDirectory() as directory:
            assets=[]
            for n in range(9):
                p=Path(directory)/str(n);p.write_bytes(b'fixture');assets.append(p)
            rows=[{'name':p.name,'size':p.stat().st_size,'digest':'sha256:'+release.digest(p)} for p in assets]
            release.validate_remote_assets({'assets':rows},assets)
            for bad in (rows[:-1],rows+[rows[0]],[{**r,'size':0} for r in rows],[{**r,'digest':'sha256:'+'0'*64} for r in rows]):
                with self.assertRaises(ValueError):release.validate_remote_assets({'assets':bad},assets)

if __name__=='__main__':unittest.main()
