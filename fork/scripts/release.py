#!/usr/bin/env python3
"""Build and publish exact-source release bundles. No deployment or live data access.
All construction uses a verified archive outside the checkout. Publication is a
separate, explicitly invoked operation after exact-source CI and artifact checks.
"""
from __future__ import annotations
import argparse
import hashlib
import io
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
REPOSITORY_ID = 1383799420
VERSION = re.compile(r'fork-[0-9]{8}\.[1-9][0-9]*(?:-rc[1-9][0-9]*)?')
SHA = re.compile(r'[0-9a-f]{40}')
COMMANDS = ('gh-server', 'ags-edge', 'ags-replication')
TARGETS = ('darwin_arm64', 'linux_amd64')

def run(args, *, cwd=ROOT, env=None, timeout=600):
    result = subprocess.run(args, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    if result.returncode:
        raise RuntimeError('command failed: ' + str(args[0]) + ': ' + result.stderr.decode(errors='replace')[-2500:])
    return result.stdout

def git(*args): return run(['git','--no-replace-objects',*args]).decode().strip()
def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()
def validated_version(version):
    if not VERSION.fullmatch(version): raise ValueError('expected fork-YYYYMMDD.N or fork-YYYYMMDD.N-rcN')
    return version

def api(path, *, method='GET', fields=(), missing=False):
    args=['gh','api','--method',method,path]
    for key,value in fields: args += ['-f',key+'='+value]
    result=subprocess.run(args,cwd=ROOT,capture_output=True,timeout=90)
    if result.returncode:
        if missing and b'(HTTP 404)' in result.stderr: return None
        raise RuntimeError('GitHub operation failed: '+method+' '+path)
    return json.loads(result.stdout) if result.stdout else None

def gate(version, sha):
    validated_version(version)
    if not SHA.fullmatch(sha) or git('rev-parse','HEAD')!=sha: raise ValueError('source mismatch')
    meta=api('repos/'+REPOSITORY)
    if meta['id']!=REPOSITORY_ID or meta['private']: raise ValueError('wrong or nonpublic repository')
    branch=meta['default_branch']
    if os.environ.get('GITHUB_ACTIONS')=='true' and os.environ.get('GITHUB_REF')!='refs/heads/'+branch:
        raise ValueError('release workflow must run on the default generation')
    runs=api('repos/'+REPOSITORY+'/actions/runs?head_sha='+sha+'&per_page=100')['workflow_runs']
    receipts={}
    for path in ('.github/workflows/ci.yml','.github/workflows/secret-scan.yml'):
        eligible=[r for r in runs if r['head_sha']==sha and r['path']==path and r['status']=='completed' and r['conclusion']=='success' and r['repository']['id']==REPOSITORY_ID]
        if not eligible: raise ValueError('exact source lacks successful '+path)
        chosen=max(eligible,key=lambda r:r['id'])
        jobs=api('repos/'+REPOSITORY+'/actions/runs/'+str(chosen['id'])+'/jobs?per_page=100')
        expected_min=10 if path.endswith('/ci.yml') else 1
        if jobs['total_count']>100 or len(jobs['jobs'])!=jobs['total_count'] or len(jobs['jobs'])<expected_min or any(j['conclusion']!='success' for j in jobs['jobs']):
            raise ValueError('CI jobs are incomplete, skipped or failed')
        receipts[path]=chosen['id']
    if api('repos/'+REPOSITORY+'/releases/tags/'+version,missing=True) is not None: raise ValueError('release exists; never overwrite or silently resume it')
    if api('repos/'+REPOSITORY+'/git/ref/tags/'+version,missing=True) is not None: raise ValueError('tag already exists')
    # Repository immutability is configured by an administrator, never by a
    # build token. The publisher requires the resulting Release to be locked.
    return {'source':sha,'tree':git('rev-parse',sha+'^{tree}'),'version':version,'ci':receipts,'repository_id':REPOSITORY_ID}

def environment(home):
    # Preserve only build tools/cache settings, not operator credentials/config.
    allowed=('PATH','TMPDIR','TEMP','TMP','GOROOT','GOPATH','GOCACHE','GOMODCACHE','GOPROXY','GOSUMDB','SSL_CERT_FILE','SYSTEMROOT')
    result={k:os.environ[k] for k in allowed if k in os.environ}
    result.update(HOME=str(home),GIT_CONFIG_NOSYSTEM='1',GIT_CONFIG_GLOBAL=os.devnull,GIT_TERMINAL_PROMPT='0',GOWORK='off',GOTOOLCHAIN='local')
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

def smoke(binary, env, cwd, endpoint, expected_revision=None):
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
        finally:
            if proc.poll() is None: proc.send_signal(signal.SIGTERM)
            try: result=proc.wait(timeout=30)
            except subprocess.TimeoutExpired:
                proc.kill(); proc.wait(); raise RuntimeError(binary.name+' did not drain during smoke')
        if result!=0: raise RuntimeError(binary.name+' did not exit cleanly')

def build(version, output):
    validated_version(version)
    sha=git('rev-parse','HEAD'); tree=git('rev-parse','HEAD^{tree}')
    native_os={'Darwin':'darwin','Linux':'linux'}.get(platform.system())
    arch={'arm64':'arm64','aarch64':'arm64','x86_64':'amd64','AMD64':'amd64'}.get(platform.machine())
    target=str(native_os)+'_'+str(arch)
    if target not in TARGETS: raise ValueError('unsupported native build platform')
    output=Path(output).absolute()
    if output.resolve().is_relative_to(ROOT) or output.exists(): raise ValueError('output must be a new external directory')
    verify_env=dict(os.environ,GIT_SHA=sha)
    run(['bash','scripts/build-exact-source.sh','verify'],env=verify_env,timeout=180)
    os.umask(0o077); output.mkdir(mode=0o700,parents=False)
    stage=Path(tempfile.mkdtemp(prefix='ags-release-build-',dir=str(output.parent)))
    source=stage/'source'; unpack_source(run(['git','archive','--format=tar',sha]),source)
    home=stage/'home'; home.mkdir(); env=environment(home)
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
        if native_os=='darwin':
            deps=run(['otool','-L',str(binary)],env=env).decode().splitlines()[1:]
            if any(not line.strip().startswith(('/usr/lib/','/System/Library/')) for line in deps): raise ValueError('non-system dynamic dependency')
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
    edge_env=dict(env,AGS_EDGE_ID='release-smoke',AGS_EDGE_LISTEN_ADDR='127.0.0.1:'+str(port),AGS_EDGE_PRIMARY_URL='http://127.0.0.1:9',AGS_EDGE_CANONICAL_URL='http://primary.example.test')
    smoke(package/'bin/ags-edge',edge_env,edge,'http://127.0.0.1:'+str(port)+'/livez')
    for name in ('LICENSE','NOTICE'):
        if (source/name).is_file(): (package/name).write_bytes((source/name).read_bytes())
    info={'schema':'ags.release.v1','version':version,'repository':REPOSITORY,'repository_id':REPOSITORY_ID,'revision':sha,'tree':tree,'target':target,
          'minimum_platform':'macOS 15 (Apple Silicon)' if native_os=='darwin' else 'Ubuntu 24.04 / glibc 2.39 for gh-server; Edge and replication client are static',
          'binaries':infos,'smoke':{'primary_readiness':True,'sqlite':True,'edge_liveness':True,'clean_shutdown':True},'apple_notarized':False}
    (package/'build-info.json').write_text(json.dumps(info,indent=2)+'\n')
    archive=output/('ags-'+version+'-'+target+'.tar.gz')
    with tarfile.open(archive,'w:gz') as tf:
        for path in sorted(package.rglob('*')):
            if path.is_file():
                entry=tf.gettarinfo(str(path),arcname=str(path.relative_to(package)));entry.uid=entry.gid=0;entry.uname=entry.gname='';entry.mode=0o755 if path.parent.name=='bin' else 0o644
                with path.open('rb') as content: tf.addfile(entry,content)
    (output/(archive.name+'.sha256')).write_text(digest(archive)+'  '+archive.name+'\n')
    (output/('build-info-'+target+'.json')).write_text(json.dumps(info,indent=2)+'\n')
    if git('rev-parse','HEAD')!=sha or git('status','--porcelain=v1','--untracked-files=all'): raise ValueError('source changed during build')
    return {'archive':archive.name,'sha256':digest(archive),'source':sha,'target':target,'smoke':info['smoke']}

def check_assets(directory,version,sha):
    validated_version(version)
    if not SHA.fullmatch(sha): raise ValueError('invalid source identity')
    directory=Path(directory); assets=[]; lines=[]
    for target in TARGETS:
        name='ags-'+version+'-'+target+'.tar.gz'; bundle=directory/name; manifest=directory/('build-info-'+target+'.json')
        info=json.loads(manifest.read_text())
        if info['repository_id']!=REPOSITORY_ID or info['revision']!=sha or info['version']!=version or info['target']!=target or info['tree']!=git('rev-parse',sha+'^{tree}') or set(info['binaries'])!=set(COMMANDS): raise ValueError('asset source/platform mismatch')
        if info.get('smoke')!={'primary_readiness':True,'sqlite':True,'edge_liveness':True,'clean_shutdown':True}: raise ValueError('asset smoke verification is incomplete')
        expected=digest(bundle)
        if (directory/(name+'.sha256')).read_text()!=expected+'  '+name+'\n': raise ValueError('asset checksum mismatch')
        with tarfile.open(bundle) as tf:
            members=tf.getmembers(); seen=set()
            for member in members:
                if member.name in seen or not member.isfile() or member.name not in {'build-info.json','LICENSE','NOTICE',*('bin/'+c for c in COMMANDS)}: raise ValueError('unsafe/unexpected archive member')
                seen.add(member.name)
            for cmd in COMMANDS:
                raw=tf.extractfile('bin/'+cmd).read()
                if hashlib.sha256(raw).hexdigest()!=info['binaries'][cmd]['sha256']: raise ValueError('binary checksum mismatch')
            if json.load(tf.extractfile('build-info.json'))!=info: raise ValueError('embedded manifest mismatch')
        assets.extend([bundle,manifest]);lines.append(expected+'  '+name)
    return assets,lines

def publish(version,sha,directory):
    gate(version,sha)
    assets,lines=check_assets(directory,version,sha)
    for bundle in (p for p in assets if p.name.endswith('.tar.gz')):
        run(['gh','attestation','verify',str(bundle),'--repo',REPOSITORY,'--source-digest',sha,
             '--signer-workflow',REPOSITORY+'/.github/workflows/release.yml','--deny-self-hosted-runners'],timeout=120)
    checks=Path(directory)/'SHA256SUMS'
    if checks.exists(): raise ValueError('refusing to replace checksum manifest')
    checks.write_text('\n'.join(lines)+'\n'); assets.append(checks)
    # Draft is intentionally retained if upload/verification fails. Never
    # republish a partial upload or reuse an already-published version.
    notes='Exact-source pre-release for controlled installation.\n\nSource: `'+sha+'`\n\nNative macOS ARM64 and Linux amd64 bundles contain gh-server, ags-edge, ags-replication, build metadata and license. Verify release assets and build attestations before execution. No service configuration or data is included. See docs/operations/releases.md for platform limits, staging and rollback.\n'
    args=['gh','release','create',version,'--repo',REPOSITORY,'--target',sha,'--draft','--title',version,'--notes',notes]
    if '-rc' in version: args.append('--prerelease')
    args.extend(str(p) for p in assets);run(args,timeout=180)
    release=api('repos/'+REPOSITORY+'/releases/tags/'+version)
    if not release['draft'] or {a['name'] for a in release['assets']}!={p.name for p in assets}: raise ValueError('draft upload incomplete')
    ref=api('repos/'+REPOSITORY+'/git/ref/tags/'+version)
    if ref['object']['type']!='commit' or ref['object']['sha']!=sha: raise ValueError('release tag differs from accepted source')
    for asset in release['assets']:
        p=Path(directory)/asset['name']
        if asset.get('digest')!='sha256:'+digest(p) or asset['size']!=p.stat().st_size: raise ValueError('GitHub upload digest/size mismatch')
    run(['gh','release','edit',version,'--repo',REPOSITORY,'--draft=false','--latest=false'],timeout=90)
    release=api('repos/'+REPOSITORY+'/releases/tags/'+version)
    if release['draft'] or not release.get('immutable'): raise ValueError('release was not locked after publication')
    return {'tag':version,'source':sha,'immutable':True,'assets':len(assets),'release_url':release['html_url']}

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('mode',choices=['gate','build','publish']);p.add_argument('--version',required=True);p.add_argument('--sha');p.add_argument('--output');a=p.parse_args()
    if a.mode=='build': result=build(a.version,a.output)
    elif a.mode=='gate': result=gate(a.version,a.sha)
    else: result=publish(a.version,a.sha,a.output)
    print(json.dumps(result,indent=2))
if __name__=='__main__': main()
