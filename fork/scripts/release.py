#!/usr/bin/env python3
"""Build runtime candidates and gated v2 tag releases.
Legacy publication and recovery entries remain closed.
"""
from __future__ import annotations
import argparse
import hashlib
import io
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import signal
import socket
import sqlite3
import subprocess
import tarfile
import tempfile
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[2]
REPOSITORY = 'yoyooyooo/agent-git-service'
REPOSITORY_ID = 1384519373
VERSION = re.compile(r'fork-[0-9]{8}\.[1-9][0-9]*(?:-rc[1-9][0-9]*)?')
SHA = re.compile(r'[0-9a-f]{40}')
COMMANDS = ('gh-server', 'ags-edge', 'ags-replication')
TARGETS = ('darwin_arm64', 'linux_amd64')
_spec=importlib.util.spec_from_file_location('ags_bundle_contract',ROOT/'scripts/release_contract.py')
bundle_contract=importlib.util.module_from_spec(_spec);_spec.loader.exec_module(bundle_contract)


def refuse_tag_publication():
    # Tag destination and real separated-control evidence are not approved.
    raise ValueError('release_source_binding_unverified: tag publication disabled; 24.04 contract retired')


def linux_builder(os_release):
    values=dict(line.split('=',1) for line in os_release.splitlines() if '=' in line)
    if values.get('ID','').strip('"')!='ubuntu' or values.get('VERSION_ID','').strip('"')!='22.04':
        raise ValueError('release_abi_incompatible: native Ubuntu 22.04 required')


def linux_dependencies(versions, headers, libraries):
    needs=re.findall(r'GLIBC_([0-9]+(?:\.[0-9]+)+)',versions)
    if not needs or any(tuple(map(int,v.split('.')))>(2,35) for v in needs):
        raise ValueError('release_abi_incompatible: GLIBC version needs exceed 2.35 or missing')
    loaders=re.findall(r'Requesting program interpreter: ([^\]]+)',headers)
    if loaders!=['/lib64/ld-linux-x86-64.so.2'] or not Path(loaders[0]).is_file():
        raise ValueError('release_abi_incompatible: unresolved or unexpected loader')
    if 'not found' in libraries or 'libc.so.6 =>' not in libraries:
        raise ValueError('release_abi_incompatible: unresolved NEEDED libraries')


def darwin_minimum(load_commands):
    versions=re.findall(r'^\s*minos\s+(\S+)',load_commands,re.M)
    if versions!=['15.0']:
        raise ValueError('release_abi_incompatible: Mach-O minimum must be 15.0')


def run(args, *, cwd=ROOT, env=None, timeout=600):
    try:
        result = subprocess.run(args, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    except (subprocess.TimeoutExpired, OSError):
        raise RuntimeError('command outcome unknown: ' + str(args[0])) from None
    if result.returncode:
        raise RuntimeError('command failed: ' + str(args[0]) + ': ' + result.stderr.decode(errors='replace')[-2500:])
    return result.stdout

def git(*args): return run(['git','--no-replace-objects',*args]).decode().strip()
def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()
def validated_version(version):
    if not VERSION.fullmatch(version): raise ValueError('expected fork-YYYYMMDD.N or fork-YYYYMMDD.N-rcN')
    return version

def pinned_toolchain(sha):
    if not isinstance(sha,str) or not SHA.fullmatch(sha): raise ValueError('invalid toolchain source')
    version=git('show',sha+':.go-version')
    if not re.fullmatch(r'1\.[1-9][0-9]*\.[0-9]+',version):
        raise ValueError('release toolchain requires an exact .go-version patch pin')
    return 'go'+version

def tag_source(version):
    ref=api('repos/'+REPOSITORY+'/git/ref/tags/'+version,missing=True)
    if ref is None: return None
    obj=ref.get('object',{})
    if obj.get('type')=='tag':
        obj=api('repos/'+REPOSITORY+'/git/tags/'+str(obj.get('sha'))).get('object',{})
    if obj.get('type')!='commit' or not SHA.fullmatch(str(obj.get('sha',''))):
        raise ValueError('release tag is not an exact commit')
    return obj['sha']

def verify_toolchain(sha):
    expected=pinned_toolchain(sha)
    actual=run(['go','env','GOVERSION'],env=environment(Path.home()),timeout=30).decode().strip()
    if actual!=expected: raise ValueError('build toolchain mismatch: expected '+expected+', found '+actual)
    return {'source':sha,'go_version':actual,'pin':'.go-version'}

def api(path, *, method='GET', fields=(), missing=False):
    args=['gh','api','--method',method,path]
    for key,value in fields:
        if isinstance(value,(bool,int)):
            args += ['-F',key+'='+json.dumps(value)]
        else:
            args += ['-f',key+'='+value]
    try:
        result=subprocess.run(args,cwd=ROOT,capture_output=True,timeout=90)
    except (subprocess.TimeoutExpired, OSError):
        raise RuntimeError('GitHub operation outcome unknown: '+method+' '+path) from None
    if result.returncode:
        if missing and b'(HTTP 404)' in result.stderr: return None
        raise RuntimeError('GitHub operation failed: '+method+' '+path)
    return json.loads(result.stdout) if result.stdout else None

def gate(version, sha, *, recovery_id=None):
    refuse_tag_publication()

def lookup_release(version):
    # Published lookup deliberately does not cover a draft with no Git tag.
    published=api('repos/'+REPOSITORY+'/releases/tags/'+version,missing=True)
    if published is not None: return published
    matches=[]
    for page in range(1,11):
        rows=api('repos/'+REPOSITORY+'/releases?per_page=100&page='+str(page))
        matches.extend(r for r in rows if r.get('tag_name')==version)
        if len(matches)>1: raise ValueError('ambiguous release version')
        if len(rows)<100: return matches[0] if matches else None
    raise ValueError('release inventory exceeds lookup bound; no absence claim')

def require_draft(release,version,sha,release_id=None):
    if not release or not release.get('draft') or release.get('immutable') or release.get('tag_name')!=version or release.get('target_commitish')!=sha or type(release.get('id')) is not int or (release_id is not None and release['id']!=release_id):
        raise ValueError('draft identity, source or publication state changed')
    return release

def environment(home):
    # Preserve only build tools/cache settings, not operator credentials/config.
    allowed=('PATH','TMPDIR','TEMP','TMP','GOROOT','GOPATH','GOCACHE','GOMODCACHE','GOPROXY','GOSUMDB','SSL_CERT_FILE','SYSTEMROOT')
    result={k:os.environ[k] for k in allowed if k in os.environ}
    result.update(HOME=str(home),GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull,GIT_TERMINAL_PROMPT='0',GOWORK='off',GOTOOLCHAIN='local',GOENV='off')
    return result

def unpack_source(payload, destination):
    destination.mkdir(mode=0o700)
    with tarfile.open(fileobj=io.BytesIO(payload)) as source:
        members=source.getmembers()
        for member in members:
            p=Path(member.name)
            if p.is_absolute() or '..' in p.parts or not (member.isdir() or member.isfile()): raise ValueError('unsafe source archive')
        source.extractall(destination,members=members)

def free_port():
    with socket.socket() as sock:
        sock.bind(('127.0.0.1',0)); return sock.getsockname()[1]

def smoke(binary, env, cwd, endpoint, expected_revision=None, *, diagnostics_endpoint=None, expected_build=None):
    log=cwd/(binary.name+'.smoke.log')
    with log.open('wb') as output:
        proc=subprocess.Popen([str(binary)],cwd=cwd,env=env,stdout=output,stderr=subprocess.STDOUT)
        try:
            opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
            deadline=time.monotonic()+60; ok=False
            while time.monotonic()<deadline:
                if proc.poll() is not None: raise RuntimeError(binary.name+' exited during smoke; inspect '+str(log))
                try:
                    with opener.open(endpoint,timeout=3) as response:
                        body=json.load(response)
                        if expected_revision and (body.get('status')!='ready' or body.get('version')!=expected_revision): raise ValueError('wrong readiness/source')
                        ok=True; break
                except (OSError,ValueError): time.sleep(.2)
            if not ok: raise RuntimeError(binary.name+' did not become ready')
            if diagnostics_endpoint is not None:
                with opener.open(diagnostics_endpoint,timeout=5) as response:
                    diagnostics=json.load(response)
                if not expected_build or diagnostics.get('source_revision')!=expected_build['revision'] or diagnostics.get('build')!=expected_build:
                    raise ValueError('actual Edge diagnostics lost or changed the release build identity')
        finally:
            if proc.poll() is None: proc.send_signal(signal.SIGTERM)
            try: result=proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                proc.kill(); proc.wait(); raise RuntimeError(binary.name+' did not drain during smoke')
        if result!=0: raise RuntimeError(binary.name+' did not exit cleanly')

def build(version, output, *, runtime_revision=None, runtime_tree=None, tag_binding=None):
    if runtime_revision is None:
        refuse_tag_publication()
    control=git('rev-parse','HEAD')
    sha=runtime_revision;tree=runtime_tree
    if not isinstance(sha,str) or not SHA.fullmatch(sha) or not isinstance(tree,str) or not SHA.fullmatch(tree):
        raise ValueError('invalid full runtime revision/tree')
    if sha==control or git('rev-parse',sha+'^{commit}')!=sha or git('rev-parse',sha+'^{tree}')!=tree:
        raise ValueError('runtime/control identity mismatch')
    if tag_binding is None:
        if version!='source-'+sha: raise ValueError('candidate version must not allocate a release tag')
    elif (tag_binding['version'],tag_binding['control'],tag_binding['source']['runtime']) != (version,control,{'revision':sha,'tree':tree}):
        raise ValueError('tag build identity mismatch')
    native_os={'Darwin':'darwin','Linux':'linux'}.get(platform.system())
    arch={'arm64':'arm64','aarch64':'arm64','x86_64':'amd64','AMD64':'amd64'}.get(platform.machine())
    target=str(native_os)+'_'+str(arch)
    if target not in TARGETS: raise ValueError('unsupported native build platform')
    if native_os=='linux': linux_builder(Path('/etc/os-release').read_text())
    if native_os=='darwin' and platform.mac_ver()[0].split('.')[0]!='15': raise ValueError('native macOS 15 required')
    # Check both pins and actual compiler before staging.
    if pinned_toolchain(control)!='go1.26.8' or pinned_toolchain(sha)!='go1.26.8': raise ValueError('runtime/control toolchain mismatch')
    toolchain=verify_toolchain(sha)['go_version']
    output=Path(output).absolute()
    if output.resolve().is_relative_to(ROOT) or output.exists(): raise ValueError('output must be a new external directory')
    verify_env=dict(os.environ,GIT_SHA=control)
    run(['bash','scripts/build-exact-source.sh','verify'],env=verify_env,timeout=180)
    run(['git','--no-replace-objects','fsck','--strict','--no-dangling',sha],timeout=180)
    os.umask(0o077); output.mkdir(mode=0o700,parents=False)
    stage=Path(tempfile.mkdtemp(prefix='ags-release-build-',dir=str(output.parent)))
    source=stage/'source'; unpack_source(run(['git','--no-replace-objects','archive','--format=tar',sha]),source)
    home=stage/'home'; home.mkdir(); env=environment(home)
    scratch=stage/'tmp';scratch.mkdir();env['TMPDIR']=str(scratch)
    if native_os=='darwin': env['MACOSX_DEPLOYMENT_TARGET']='15.0'
    package=stage/'package'; (package/'bin').mkdir(parents=True)
    flags=' '.join('-X github.com/ngaut/agent-git-service/'+key+'='+value for key,value in (
        ('internal/buildinfo.Version',version),('internal/buildinfo.Revision',sha),('internal/buildinfo.Tree',tree),('server.gitSHA',sha)))
    infos={}
    for command in COMMANDS:
        binary=package/'bin'/command
        build_env=dict(env,CGO_ENABLED='1' if command=='gh-server' else '0')
        run(['go','build','-mod=readonly','-buildvcs=false','-trimpath','-ldflags='+flags,'-o',str(binary),'./cmd/'+command],cwd=source,env=build_env,timeout=900)
        binary.chmod(0o755)
        reported=json.loads(run([str(binary),'--version'],cwd=stage,env=dict(env,DB_DSN='invalid-must-not-be-opened',AGS_EDGE_READ_CONFIG_FILE='/missing/version-must-not-open'),timeout=10))
        if reported['revision']!=sha or reported['tree']!=tree or reported['version']!=version or reported['command']!=command or reported['goos']!=native_os or reported['goarch']!=arch:
            raise ValueError('binary identity mismatch')
        if reported.get('go_version')!=toolchain: raise ValueError('binary toolchain differs from pinned build compiler')
        if native_os=='darwin':
            if command=='gh-server': darwin_minimum(run(['otool','-l',str(binary)],env=env).decode())
            deps=run(['otool','-L',str(binary)],env=env).decode().splitlines()[1:]
            if any(not line.strip().startswith(('/usr/lib/','/System/Library/')) for line in deps): raise ValueError('non-system dynamic dependency')
        if native_os=='linux' and command=='gh-server':
            linux_dependencies(run(['readelf','--version-info',str(binary)],env=env).decode(),
                               run(['readelf','-l',str(binary)],env=env).decode(),
                               run(['ldd',str(binary)],env=env).decode())
        infos[command]={'sha256':digest(binary),'size':binary.stat().st_size,'identity':reported,'cgo_enabled':command=='gh-server'}
    primary=stage/'primary-smoke'; primary.mkdir(); port=free_port()
    smoke_env=dict(env,DB_DSN='file:'+str(primary/'smoke.sqlite'),GIT_REPO_DIR=str(primary/'repos'),PORT=str(port),BASE_URL='http://127.0.0.1:'+str(port),LISTEN_MODE='production',ENVIRONMENT='production',ENABLE_WORKFLOW_EXEC='0')
    smoke(package/'bin/gh-server',smoke_env,primary,smoke_env['BASE_URL']+'/readyz',sha)
    db=sqlite3.connect(str(primary/'smoke.sqlite'))
    try:
        if db.execute('PRAGMA integrity_check').fetchone()[0]!='ok': raise ValueError('SQLite smoke integrity failure')
        for table in ('users','repositories','access_grants'):
            if not db.execute('SELECT 1 FROM sqlite_master WHERE type=? AND name=?',('table',table)).fetchone(): raise ValueError('SQLite smoke missing table')
    finally: db.close()
    edge=stage/'edge-smoke'; edge.mkdir(); port=free_port()
    diagnostics_port=free_port()
    while diagnostics_port==port: diagnostics_port=free_port()
    edge_env=dict(env,AGS_EDGE_ID='release-smoke',AGS_EDGE_LISTEN_ADDR='127.0.0.1:'+str(port),AGS_EDGE_PRIMARY_URL='http://127.0.0.1:9',AGS_EDGE_CANONICAL_URL='http://primary.example.test',AGS_EDGE_DIAGNOSTICS_ADDR='127.0.0.1:'+str(diagnostics_port))
    smoke(package/'bin/ags-edge',edge_env,edge,'http://127.0.0.1:'+str(port)+'/livez',diagnostics_endpoint='http://127.0.0.1:'+str(diagnostics_port)+'/status',expected_build=infos['ags-edge']['identity'])
    for name in ('LICENSE','NOTICE'):
        if (source/name).is_file(): (package/name).write_bytes((source/name).read_bytes())
    info={'schema':bundle_contract.MANIFEST_SCHEMA,'abi':bundle_contract.abi_for(target),'toolchain':{'go_version':toolchain,'pin_path':'.go-version'},'version':version,'repository':REPOSITORY,'repository_id':REPOSITORY_ID,'revision':sha,'tree':tree,'target':target,
          'minimum_platform':'macOS 15 (Apple Silicon)' if native_os=='darwin' else 'Ubuntu 22.04 / glibc 2.35 for gh-server; Edge and replication client are static',
          'binaries':infos,'smoke':{'primary_readiness':True,'sqlite':True,'edge_liveness':True,'edge_build_identity':True,'clean_shutdown':True},'apple_notarized':False}
    (package/'build-info.json').write_text(json.dumps(info,indent=2)+'\n')
    archive=output/('ags-'+version+'-'+target+'.tar.gz')
    with tarfile.open(archive,'w:gz') as tf:
        for path in sorted(package.rglob('*')):
            if path.is_file():
                entry=tf.gettarinfo(str(path),arcname=str(path.relative_to(package)));entry.uid=entry.gid=0;entry.uname=entry.gname='';entry.mode=0o755 if path.parent.name=='bin' else 0o644
                with path.open('rb') as content: tf.addfile(entry,content)
    if bundle_contract.inspect_bundle(archive,version,sha,target)!=info: raise ValueError('candidate archive mismatch')
    (output/(archive.name+'.sha256')).write_text(digest(archive)+'  '+archive.name+'\n')
    (output/('build-info-'+target+'.json')).write_text(json.dumps(info,indent=2)+'\n')
    if git('rev-parse','HEAD')!=control or git('status','--porcelain=v1','--untracked-files=all'): raise ValueError('source changed during build')
    (output/'runtime-predicate.json').write_text(json.dumps({'schema':'ags.runtime-binding.v1','runtime_revision':sha,'runtime_tree':tree})+'\n')
    return {'archive':archive.name,'sha256':digest(archive),'source':sha,'target':target,'smoke':info['smoke']}

def check_assets(directory,version,sha):
    validated_version(version)
    if not SHA.fullmatch(sha): raise ValueError('invalid source identity')
    directory=Path(directory); assets=[]; lines=[]
    toolchain=pinned_toolchain(sha)
    for target in TARGETS:
        name='ags-'+version+'-'+target+'.tar.gz'; bundle=directory/name; manifest=directory/('build-info-'+target+'.json')
        info=json.loads(manifest.read_text())
        bundle_contract.validate_manifest_contract(info,target)
        if info['repository_id']!=REPOSITORY_ID or info['revision']!=sha or info['version']!=version or info['target']!=target or info['tree']!=git('rev-parse',sha+'^{tree}') or set(info['binaries'])!=set(COMMANDS): raise ValueError('asset source/platform mismatch')
        if any(info['binaries'][command].get('identity',{}).get('go_version')!=toolchain for command in COMMANDS):
            raise ValueError('asset toolchain differs from the exact source pin')
        if info.get('smoke')!={'primary_readiness':True,'sqlite':True,'edge_liveness':True,'edge_build_identity':True,'clean_shutdown':True}: raise ValueError('asset smoke verification is incomplete')
        expected=digest(bundle)
        if (directory/(name+'.sha256')).read_text()!=expected+'  '+name+'\n': raise ValueError('asset checksum mismatch')
        if bundle_contract.inspect_bundle(bundle,version,sha,target)!=info: raise ValueError('embedded manifest mismatch')
        assets.extend([bundle,manifest]);lines.append(expected+'  '+name)
    return assets,lines

def verified_assets(directory,version,sha):
    refuse_tag_publication()

def finish_draft(version,sha,release_id,assets):
    refuse_tag_publication()

def publish(version,sha,directory):
    refuse_tag_publication()

def recover_draft(version,sha,directory,release_id):
    refuse_tag_publication()


# These are complete job names, including every matrix expansion in ci.yml.
# A workflow change must update this reviewed inventory and its contract test.
DEFAULT_BRANCH = 'fork/main.20260924'
CI_JOBS = {
    '.github/workflows/ci.yml': {
        'Source and governance', 'Root tests (service)', 'Root tests (rest)',
        'Root tests (server-graphql)', 'Root tests (db-router-git)',
        'Root tests (edge)', 'Root tests (fork-integrations)', 'Root tests (misc)',
        'Edge and authority race checks', 'Vendored client compatibility',
        'Official gh and CI backend acceptance', 'Real Forgejo protected-branch projection',
    },
    '.github/workflows/secret-scan.yml': {'secrets'},
}


def pages(path, key=None):
    rows = []
    for page in range(1, 11):
        response = api(path + ('&' if '?' in path else '?') + 'per_page=100&page=' + str(page))
        batch = response[key] if key else response
        if not isinstance(batch, list):
            raise ValueError('incomplete API inventory')
        rows.extend(batch)
        if len(batch) < 100:
            if key and response.get('total_count') != len(rows):
                raise ValueError('incomplete API inventory')
            return rows
    raise ValueError('API inventory exceeds bound')


def tag_event(context):
    if (context.get('event_name') != 'push' or context.get('repository_id') != REPOSITORY_ID
            or context.get('deleted') is not False or context.get('forced') is not False
            or context.get('created') is not True or context.get('before') != '0' * 40):
        raise ValueError('tag_creation_required')
    ref = context.get('ref', '')
    if not isinstance(ref, str) or not ref.startswith('refs/tags/'):
        raise ValueError('tag_ref_required')
    version = validated_version(ref.removeprefix('refs/tags/'))
    control = context.get('sha', '')
    if (not isinstance(control, str) or not SHA.fullmatch(control)
            or context.get('workflow_sha') != control):
        raise ValueError('control_identity_mismatch')
    # Annotated tag pushes can report the tag object as `after`; tag_gate peels it.
    if not isinstance(context.get('after'), str) or not SHA.fullmatch(context['after']):
        raise ValueError('tag_event_object_invalid')
    return version


def invocation():
    event = json.loads(Path(os.environ['GITHUB_EVENT_PATH']).read_text())
    return {key: event.get(key) for key in ('created', 'deleted', 'forced', 'before', 'after')} | {
        'event_name': os.environ.get('GITHUB_EVENT_NAME'), 'ref': os.environ.get('GITHUB_REF'),
        'sha': os.environ.get('GITHUB_SHA'), 'workflow_sha': os.environ.get('GITHUB_WORKFLOW_SHA'),
        'repository_id': event.get('repository', {}).get('id'),
        'run_id': os.environ.get('GITHUB_RUN_ID'), 'run_attempt': os.environ.get('GITHUB_RUN_ATTEMPT'),
    }


def source_contract(source, version, control):
    validated_version(version)
    runtime = source.get('runtime', {})
    acceptance = source.get('acceptance', {})
    if (set(source) != {'schema', 'version', 'runtime', 'acceptance'}
            or source.get('schema') != 'ags.release-source.v1' or source.get('version') != version
            or set(runtime) != {'revision', 'tree'}
            or any(not isinstance(v, str) or not SHA.fullmatch(v) for v in runtime.values())
            or runtime['revision'] == control
            or set(acceptance) != {'runtime_pull_request', 'disclosure'}
            or type(acceptance.get('runtime_pull_request')) is not int
            or acceptance['runtime_pull_request'] <= 0
            or acceptance.get('disclosure') != 'fork/PUBLICATION.md'):
        raise ValueError('release_source_invalid')
    return source


def reviewed_commit(sha, number=None):
    prs = pages('repos/' + REPOSITORY + '/commits/' + sha + '/pulls')
    accepted = []
    for row in prs:
        if number is not None and row.get('number') != number:
            continue
        pr = api('repos/' + REPOSITORY + '/pulls/' + str(row['number']))
        if (not pr.get('merged') or pr.get('base', {}).get('ref') != DEFAULT_BRANCH
                or pr.get('base', {}).get('repo', {}).get('id') != REPOSITORY_ID
                or pr.get('head', {}).get('repo', {}).get('id') != REPOSITORY_ID
                or pr.get('merge_commit_sha') != sha):
            continue
        # A rebase/merge may change commit IDs. The PR head's final tree
        # must still equal the accepted commit, rather than trusting `merged`.
        head = pr['head']['sha']
        if not SHA.fullmatch(str(head)):
            continue
        if api('repos/' + REPOSITORY + '/git/commits/' + head)['tree']['sha'] != git('rev-parse', sha + '^{tree}'):
            continue
        accepted.append({'pull_request': pr['number'], 'head': head})
    if len(accepted) != 1:
        raise ValueError('reviewed_commit_required')
    return accepted[0]


def validate_jobs(rows, expected, attempt):
    if (len(rows) != len(expected) or {row.get('name') for row in rows} != expected
            or any(row.get('status') != 'completed' or row.get('conclusion') != 'success'
                   or row.get('run_attempt') != attempt for row in rows)):
        raise ValueError('exact_ci_jobs_required')


def exact_ci(sha):
    rows = pages('repos/' + REPOSITORY + '/actions/runs?head_sha=' + sha, 'workflow_runs')
    evidence = []
    for path, expected in CI_JOBS.items():
        eligible = [row for row in rows if row.get('path') == path and row.get('head_sha') == sha
                    and row.get('repository', {}).get('id') == REPOSITORY_ID
                    and row.get('head_repository', {}).get('id') == REPOSITORY_ID
                    and row.get('event') in ('push', 'pull_request', 'workflow_dispatch')]
        if not eligible:
            raise ValueError('exact_ci_missing')
        # Choose the newest applicable run before looking at its conclusion.
        chosen = max(eligible, key=lambda row: row['id'])
        current = api('repos/' + REPOSITORY + '/actions/runs/' + str(chosen['id']))
        if (current.get('head_sha') != sha or current.get('path') != path
                or current.get('repository', {}).get('id') != REPOSITORY_ID
                or current.get('head_repository', {}).get('id') != REPOSITORY_ID
                or current.get('event') != chosen['event']
                or current.get('status') != 'completed' or current.get('conclusion') != 'success'
                or type(current.get('run_attempt')) is not int):
            raise ValueError('exact_ci_incomplete')
        attempt = current['run_attempt']
        jobs = pages('repos/' + REPOSITORY + '/actions/runs/' + str(chosen['id'])
                     + '/attempts/' + str(attempt) + '/jobs', 'jobs')
        validate_jobs(jobs, expected, attempt)
        evidence.append({'path': path, 'run_id': chosen['id'], 'attempt': attempt})
    return evidence


def tag_gate(context, *, prior=None, release_id=None):
    # Missing variables deny even candidate artifact allocation in tag mode.
    if os.environ.get('AGS_RELEASE_PUBLICATION_ENABLED') != 'true':
        raise ValueError('publication_disabled')
    version = tag_event(context)
    control = context['sha']
    repo = api('repos/' + REPOSITORY)
    if (repo.get('id') != REPOSITORY_ID or repo.get('full_name') != REPOSITORY
            or repo.get('private') is not False or repo.get('fork') is not True
            or repo.get('parent', {}).get('full_name') != 'ngaut/agent-git-service'
            or repo.get('default_branch') != DEFAULT_BRANCH):
        raise ValueError('repository_identity_mismatch')
    enabled = api('repos/' + REPOSITORY + '/actions/variables/AGS_RELEASE_PUBLICATION_ENABLED', missing=True)
    if not enabled or enabled.get('value') != 'true':
        raise ValueError('publication_disabled')
    ref = api('repos/' + REPOSITORY + '/git/ref/tags/' + version)['object']
    if context['after'] != ref.get('sha') or tag_source(version) != control:
        raise ValueError('tag_control_mismatch')
    if git('rev-parse', 'HEAD') != control:
        raise ValueError('checkout_control_mismatch')
    tip = api('repos/' + REPOSITORY + '/commits/' + DEFAULT_BRANCH)['sha']
    if prior is None:
        if tip != control:
            raise ValueError('default_tip_mismatch')
    else:
        if prior.get('control') != control or prior.get('version') != version:
            raise ValueError('frozen_binding_mismatch')
        comparison = api('repos/' + REPOSITORY + '/compare/' + control + '...' + tip)
        if comparison.get('status') not in ('identical', 'ahead') or comparison.get('merge_base_commit', {}).get('sha') != control:
            raise ValueError('default_branch_rewritten')
    # Freeze and compare the exact reviewed workflow and source bytes at C.
    for path in ('.github/workflows/release-control.yml', '.github/workflows/release.yml',
                 'fork/release-source.json'):
        if run(['git', '--no-replace-objects', 'show', control + ':' + path]) != (ROOT / path).read_bytes():
            raise ValueError('control_contract_mismatch')
    control_review = reviewed_commit(control)
    source = source_contract(json.loads(git('show', control + ':fork/release-source.json')), version, control)
    runtime = source['runtime']
    revision = runtime['revision']
    if git('rev-parse', revision + '^{tree}') != runtime['tree']:
        raise ValueError('runtime_tree_mismatch')
    try:
        git('merge-base', '--is-ancestor', revision, control)
    except RuntimeError:
        raise ValueError('runtime_not_ancestor') from None
    runtime_review = reviewed_commit(revision, source['acceptance']['runtime_pull_request'])
    ci = {'control': exact_ci(control), 'runtime': exact_ci(revision)}
    # Public source/disclosure checks only. Never load an operator's private policy.
    run(['python3', 'fork/scripts/audit.py', 'verify'])
    run(['python3', 'fork/scripts/audit.py', 'publication'])
    found = lookup_release(version)
    if found is not None:
        if release_id is None:
            raise ValueError('release_name_occupied')
        require_draft(found, version, control, release_id)
    elif release_id is not None:
        raise ValueError('draft_missing')
    if not re.fullmatch(r'[1-9][0-9]*', str(context.get('run_id', ''))) or not re.fullmatch(r'[1-9][0-9]*', str(context.get('run_attempt', ''))):
        raise ValueError('build_invocation_invalid')
    binding = {'schema': 'ags.release-gate.v2', 'version': version, 'control': control,
               'source': source, 'tag_object': ref['sha'], 'run_id': str(context['run_id']), 'run_attempt': str(context['run_attempt']),
               'control_review': control_review, 'runtime_review': runtime_review, 'ci': ci}
    if prior is not None and any(binding[key] != prior.get(key) for key in
                                 ('source', 'tag_object', 'run_id', 'run_attempt', 'control_review', 'runtime_review')):
        raise ValueError('frozen_binding_mismatch')
    return binding


def load_binding(path):
    binding = json.loads(Path(path).read_text())
    if binding.get('schema') != 'ags.release-gate.v2':
        raise ValueError('invalid gate receipt')
    return binding


def tag_build(binding, output):
    fresh = tag_gate(invocation(), prior=binding)
    runtime = fresh['source']['runtime']
    result = build(fresh['version'], output, runtime_revision=runtime['revision'],
                   runtime_tree=runtime['tree'], tag_binding=fresh)
    (Path(output) / 'build-binding.json').write_text(json.dumps({
        key: fresh[key] for key in ('version', 'control', 'source', 'run_id', 'run_attempt')}, sort_keys=True) + '\n')
    return result


def binding_verifier():
    spec = importlib.util.spec_from_file_location('ags_release_binding', ROOT / 'scripts/release_verify.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def tag_assets(directory, binding):
    """Assemble exactly nine assets from disjoint current-run target directories."""
    directory = Path(directory)
    version, control, source = binding['version'], binding['control'], binding['source']
    revision = source['runtime']['revision']
    verifier = binding_verifier()
    pin = {'schema': bundle_contract.PIN_SCHEMA, 'repository': REPOSITORY,
           'repository_id': REPOSITORY_ID, 'runtime': source['runtime'],
           'targets': {target: {'manifest_schema': bundle_contract.MANIFEST_SCHEMA,
                       'abi': bundle_contract.abi_for(target), 'provenance': {
                           'control_revision': control, 'signer_digest': control,
                           'signer_workflow': verifier.WORKFLOW}} for target in TARGETS}}
    assembled = directory / 'publication'
    assembled.mkdir(mode=0o700)
    for target in TARGETS:
        part = directory / ('release-control-' + target)
        archive_name = 'ags-' + version + '-' + target + '.tar.gz'
        allowed = {archive_name, archive_name + '.sha256', 'build-info-' + target + '.json',
                   'attestation.sigstore.json', 'runtime-predicate.json', 'build-binding.json'}
        if {p.name for p in part.iterdir()} != allowed or any(p.is_symlink() or not p.is_file() for p in part.iterdir()):
            raise ValueError('target_asset_inventory_mismatch')
        expected = {key: binding[key] for key in ('version', 'control', 'source', 'run_id', 'run_attempt')}
        if json.loads((part / 'build-binding.json').read_text()) != expected:
            raise ValueError('target_binding_mismatch')
        for name in (archive_name, archive_name + '.sha256', 'build-info-' + target + '.json'):
            (assembled / name).write_bytes((part / name).read_bytes())
        bundle = assembled / ('attestation-' + target + '.sigstore.json')
        bundle.write_bytes((part / 'attestation.sigstore.json').read_bytes())
        statement = verifier.verify_evidence(assembled / archive_name, pin, target, bundle=bundle, source=source,
            invocation_uri='https://github.com/' + REPOSITORY + '/actions/runs/' + binding['run_id'] + '/attempts/' + binding['run_attempt'],
            source_ref='refs/tags/' + version)
        verifier.check_verified_statement([{'verificationResult': {'statement': statement}}],
                                          digest(assembled / archive_name), source=source, subject_name=archive_name)
    assets, _ = check_assets(assembled, version, revision)
    # check_assets validates the exact R tree; also compare the gate's pinned T.
    for target in TARGETS:
        info = json.loads((assembled / ('build-info-' + target + '.json')).read_text())
        if info['tree'] != source['runtime']['tree']:
            raise ValueError('target_tree_mismatch')
    for target in TARGETS:
        assets.append(assembled / ('attestation-' + target + '.sigstore.json'))
    for name, content in (
            ('release-source.json', run(['git', '--no-replace-objects', 'show', control + ':fork/release-source.json'])),
            ('install.sh', run(['git', '--no-replace-objects', 'show', control + ':scripts/install.sh']))):
        (assembled / name).write_bytes(content)
        assets.append(assembled / name)
    sums = assembled / 'SHA256SUMS'
    sums.write_text(''.join(digest(path) + '  ' + path.name + '\n' for path in sorted(assets, key=lambda p: p.name)))
    assets.append(sums)
    if len(assets) != 9 or len({p.name for p in assets}) != 9:
        raise ValueError('nine_release_assets_required')
    return sorted(assets, key=lambda p: p.name)


def release_body(binding):
    runtime = binding['source']['runtime']
    return ('Release ' + binding['version'] + '\n\nControl: ' + binding['control']
            + '\nRuntime: ' + runtime['revision'] + '\nTree: ' + runtime['tree']
            + '\nBuild run/attempt: ' + binding['run_id'] + '/' + binding['run_attempt']
            + '\n\nNative Ubuntu 22.04 amd64 (glibc <= 2.35) and macOS 15 arm64; Go 1.26.8.'
            + '\nVerify both archives with gh attestation verify using --source-digest and'
            + ' --signer-digest set to Control, --signer-workflow ' + REPOSITORY
            + '/.github/workflows/release-control.yml --deny-self-hosted-runners'
            + ' --predicate-type https://github.com/' + REPOSITORY
            + '/attestations/runtime-binding/v1 --bundle attestation-<target>.sigstore.json.'
            + '\nSee docs/operations/releases.md at Control for the full verification policy.'
            + '\n\nPublication does not deploy. Operators upgrade manually.'
            + '\nV2 installation and production verification remain disabled in slice 1.'
            + '\nStable Latest promotion requires separate approval.\n')


def create_draft(version, control, body):
    if lookup_release(version) is not None:
        raise ValueError('release_name_occupied')
    with tempfile.TemporaryDirectory(prefix='ags-release-notes-') as directory:
        notes = Path(directory) / 'notes.md'
        notes.write_text(body)
        args = ['gh', 'release', 'create', version, '--repo', REPOSITORY,
                '--verify-tag', '--target', control, '--title', version,
                '--notes-file', str(notes), '--draft', '--latest=false']
        if '-rc' in version:
            args.append('--prerelease')
        try:
            run(args, timeout=180)
        except RuntimeError:
            pass
    # Read by exact tag after both successful and uncertain writes. Never POST twice.
    created = lookup_release(version)
    if created is None:
        raise ValueError('create_outcome_unknown; retain run evidence, do not retry')
    require_draft(created, version, control)
    if created.get('body') != body:
        raise ValueError('draft_body_mismatch')
    return created


def asset_matches(row, asset):
    return (row.get('name') == asset.name and row.get('size') == asset.stat().st_size
            and row.get('digest') == 'sha256:' + digest(asset))


def upload_asset(release_id, version, asset):
    # gh release upload never uses --clobber. Each POST has one readback even on timeout.
    try:
        run(['gh', 'release', 'upload', version, str(asset), '--repo', REPOSITORY])
    except RuntimeError:
        pass
    remote = api('repos/' + REPOSITORY + '/releases/' + str(release_id))
    if remote.get('id') != release_id or remote.get('draft') is not True:
        raise ValueError('upload_draft_changed; retained without further writes')
    matches = [row for row in remote.get('assets', []) if row.get('name') == asset.name]
    if len(matches) != 1 or not asset_matches(matches[0], asset):
        raise ValueError('upload_outcome_unknown; failed draft retained')


def validate_remote_assets(remote, assets):
    rows = remote.get('assets', [])
    if len(rows) != 9 or len({row.get('name') for row in rows}) != 9:
        raise ValueError('nine_remote_assets_required; failed draft retained')
    for asset in assets:
        matches = [row for row in rows if row.get('name') == asset.name]
        if len(matches) != 1 or not asset_matches(matches[0], asset):
            raise ValueError('remote_asset_mismatch; failed draft retained')


def tag_publish(binding, directory):
    context = invocation()
    tag_gate(context, prior=binding)
    assets = tag_assets(directory, binding)
    tag_gate(context, prior=binding)
    version, control = binding['version'], binding['control']
    body = release_body(binding)
    # The CLI refuses to allocate a missing tag via --verify-tag.
    if tag_source(version) != control:
        raise ValueError('tag_control_mismatch')
    draft = create_draft(version, control, body)
    release_id = draft['id']
    for asset in assets:
        upload_asset(release_id, version, asset)
    fresh = api('repos/' + REPOSITORY + '/releases/' + str(release_id))
    require_draft(fresh, version, control, release_id)
    if fresh.get('body') != body:
        raise ValueError('draft_body_mismatch')
    validate_remote_assets(fresh, assets)
    tag_gate(context, prior=binding, release_id=release_id)
    try:
        api('repos/' + REPOSITORY + '/releases/' + str(release_id), method='PATCH',
            fields=(('draft', False), ('prerelease', '-rc' in version), ('make_latest', 'false')))
    except RuntimeError:
        pass
    published = api('repos/' + REPOSITORY + '/releases/' + str(release_id))
    if (published.get('id') != release_id or published.get('draft') is not False
            or published.get('immutable') is not True or published.get('tag_name') != version
            or published.get('target_commitish') != control or published.get('body') != body
            or published.get('prerelease') is not ('-rc' in version)):
        raise ValueError('publication_outcome_unknown; retain draft/release and read back, do not retry')
    validate_remote_assets(published, assets)
    for asset in assets:
        run(['gh', 'release', 'verify-asset', version, str(asset), '--repo', REPOSITORY], timeout=180)
    return {'release_id': release_id, 'control': control, 'version': version, 'immutable': True}


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('mode',choices=['toolchain','gate','build','build-runtime','publish','recover-draft','tag-gate','tag-build','tag-publish']);p.add_argument('--version');p.add_argument('--sha');p.add_argument('--output');p.add_argument('--draft-id',type=int);p.add_argument('--runtime-revision');p.add_argument('--runtime-tree');p.add_argument('--binding');a=p.parse_args()
    if a.mode=='tag-gate':
        result=tag_gate(invocation())
        Path(a.output).write_text(json.dumps(result,sort_keys=True)+'\n')
        with open(os.environ['GITHUB_OUTPUT'],'a') as out:
            out.write('binding='+json.dumps(result,separators=(',',':'))+'\n')
        print(json.dumps(result,indent=2));return
    if a.mode in ('tag-build','tag-publish'):
        if not a.binding or not a.output: p.error('tag operation requires --binding and --output')
        binding=load_binding(a.binding)
        result=tag_build(binding,a.output) if a.mode=='tag-build' else tag_publish(binding,a.output)
        print(json.dumps(result,indent=2));return
    if a.mode=='toolchain':
        print(json.dumps(verify_toolchain(a.sha or git('rev-parse','HEAD')),indent=2)); return
    if a.mode=='build-runtime':
        if os.environ.get('GITHUB_ACTIONS')=='true' and os.environ.get('GITHUB_EVENT_NAME')!='workflow_dispatch': p.error('runtime builds require workflow_dispatch')
        result=build('source-'+str(a.runtime_revision),a.output,runtime_revision=a.runtime_revision,runtime_tree=a.runtime_tree)
        print(json.dumps(result,indent=2));return
    if a.version is None: p.error('this operation requires --version')
    if a.mode=='build': result=build(a.version,a.output)
    elif a.mode=='gate': result=gate(a.version,a.sha)
    elif a.mode=='recover-draft':
        if a.draft_id is None: p.error('recovery requires --draft-id')
        result=recover_draft(a.version,a.sha,a.output,a.draft_id)
    else: result=publish(a.version,a.sha,a.output)
    print(json.dumps(result,indent=2))
if __name__=='__main__': main()
