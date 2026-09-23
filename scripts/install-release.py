#!/usr/bin/env python3
"""Verify and stage an immutable AGS release; never migrate data or restart a service.
Default is a read-only plan. --install downloads and verifies release and build
attestations. --activate separately selects prefix/current after verification.
"""
from __future__ import annotations
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import stat
import subprocess
import tarfile
import tempfile

REPO='yoyooyooo/agent-git-service'
REPO_ID=1383799420
COMMANDS=('gh-server','ags-edge','ags-replication')
VERSION=re.compile(r'fork-[0-9]{8}\.[1-9][0-9]*(?:-rc[1-9][0-9]*)?')
SHA=re.compile(r'[0-9a-f]{40}')
MAX_ARCHIVE=500*1024*1024
MAX_UNPACKED=1024*1024*1024

def execute(args):
    result=subprocess.run(args,capture_output=True,timeout=180)
    if result.returncode: raise RuntimeError('Verification/download command failed: '+args[0]+' '+args[1]+' (no fallback)')
    return result.stdout

def api(path): return json.loads(execute(['gh','api',path]))
def digest(path): return hashlib.sha256(path.read_bytes()).hexdigest()

def target():
    system={'Darwin':'darwin','Linux':'linux'}.get(platform.system())
    arch={'arm64':'arm64','aarch64':'arm64','x86_64':'amd64','AMD64':'amd64'}.get(platform.machine())
    value=str(system)+'_'+str(arch)
    if value not in ('darwin_arm64','linux_amd64'): raise ValueError('No supported release target for this platform')
    return value

def inspect_bundle(archive, version, source, platform_target):
    if archive.stat().st_size>MAX_ARCHIVE: raise ValueError('Oversized archive')
    with tarfile.open(archive) as tf:
        entries=tf.getmembers(); names=set(); total=0
        allow={'LICENSE','NOTICE','build-info.json',*('bin/'+c for c in COMMANDS)}
        for entry in entries:
            if entry.name in names or entry.name not in allow or not entry.isfile() or entry.mode & 0o7000:
                raise ValueError('Unsafe, duplicate or unexpected archive entry')
            total+=entry.size
            if total>MAX_UNPACKED: raise ValueError('Archive expands beyond size limit')
            names.add(entry.name)
        if not {'LICENSE','build-info.json',*('bin/'+c for c in COMMANDS)}<=names: raise ValueError('Incomplete release bundle')
        if tf.getmember('build-info.json').size>1024*1024: raise ValueError('Oversized build manifest')
        info=json.load(tf.extractfile('build-info.json'))
        if info.get('schema')!='ags.release.v1' or info.get('repository')!=REPO or info.get('repository_id')!=REPO_ID or info.get('revision')!=source or not SHA.fullmatch(info.get('tree','')) or info.get('version')!=version or info.get('target')!=platform_target:
            raise ValueError('Release/source/platform identity mismatch')
        if set(info.get('binaries',{}))!=set(COMMANDS): raise ValueError('Unexpected binary inventory')
        for command in COMMANDS:
            data=tf.extractfile('bin/'+command).read(); binary=info['binaries'][command]; identity=binary['identity']
            if hashlib.sha256(data).hexdigest()!=binary['sha256'] or len(data)!=binary['size']:
                raise ValueError('Binary checksum mismatch')
            if identity['command']!=command or identity['revision']!=source or identity['tree']!=info['tree'] or identity['version']!=version or identity['goos']+'_'+identity['goarch']!=platform_target:
                raise ValueError('Embedded binary identity mismatch')
        return info

def extract_bundle(archive,destination):
    # Call only after inspect_bundle and external attestation verification.
    with tarfile.open(archive) as tf:
        for member in tf.getmembers():
            path=destination/member.name
            path.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
            with path.open('xb') as output: output.write(tf.extractfile(member).read())
            path.chmod(0o755 if member.name.startswith('bin/') else 0o644)

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--version',required=True);p.add_argument('--prefix',type=Path,default=Path.home()/'.local/lib/agent-git-service')
    p.add_argument('--install',action='store_true');p.add_argument('--activate',action='store_true');p.add_argument('--allow-prerelease',action='store_true')
    a=p.parse_args()
    if not VERSION.fullmatch(a.version): raise ValueError('Invalid explicit release version')
    if a.activate and not a.install: raise ValueError('Activation requires explicit --install')
    if os.environ.get('MULTICA_TOKEN'): raise ValueError('Workload context cannot install operator software')
    meta=api('repos/'+REPO)
    if meta['id']!=REPO_ID or meta['private']: raise ValueError('Unexpected release repository')
    release=api('repos/'+REPO+'/releases/tags/'+a.version)
    if release['draft'] or not release.get('immutable') or (release['prerelease'] and not a.allow_prerelease): raise ValueError('Release is draft, mutable or an unapproved prerelease')
    ref=api('repos/'+REPO+'/git/ref/tags/'+a.version)['object']
    if ref['type']=='tag': ref=api('repos/'+REPO+'/git/tags/'+ref['sha'])['object']
    if ref['type']!='commit' or not SHA.fullmatch(ref['sha']): raise ValueError('Release tag is not an exact commit')
    source=ref['sha']; platform_target=target(); name='ags-'+a.version+'-'+platform_target+'.tar.gz'
    matching=[asset for asset in release['assets'] if asset['name']==name]
    if len(matching)!=1 or not matching[0].get('digest','').startswith('sha256:'): raise ValueError('Exact asset or digest is missing')
    if matching[0]['size']>MAX_ARCHIVE: raise ValueError('Oversized release asset')
    prefix=a.prefix.expanduser().absolute()
    if prefix.is_symlink() or prefix.resolve()!=prefix: raise ValueError('Installation prefix must be canonical and not a symlink')
    previous=None; current=prefix/'current'
    if current.is_symlink():
        previous=os.readlink(current)
        if not re.fullmatch(r'releases/fork-[0-9]{8}\.[1-9][0-9]*(?:-rc[1-9][0-9]*)?',previous): raise ValueError('Existing selector is not owned by this installer')
    elif current.exists(): raise ValueError('Refusing to replace a non-symlink current selector')
    plan={'repository':REPO,'version':a.version,'source':source,'target':platform_target,'prefix':str(prefix),'previous':previous,'install':a.install,'activate':a.activate,'service_or_data_changed':False}
    if not a.install: print(json.dumps(plan,indent=2));return
    os.umask(0o077);prefix.mkdir(mode=0o700,parents=True,exist_ok=True)
    st=prefix.stat()
    if st.st_uid!=os.getuid() or st.st_mode & 0o022: raise ValueError('Installation directory must be owner-controlled')
    cache=Path(tempfile.mkdtemp(prefix='.download-',dir=prefix))
    execute(['gh','release','download',a.version,'--repo',REPO,'--pattern',name,'--dir',str(cache)])
    archive=cache/name
    if 'sha256:'+digest(archive)!=matching[0]['digest'] or archive.stat().st_size!=matching[0]['size']: raise ValueError('Downloaded asset differs from GitHub digest/size')
    execute(['gh','release','verify-asset',a.version,str(archive),'--repo',REPO])
    execute(['gh','attestation','verify',str(archive),'--repo',REPO,'--source-digest',source,'--signer-workflow',REPO+'/.github/workflows/release.yml','--deny-self-hosted-runners'])
    info=inspect_bundle(archive,a.version,source,platform_target)
    releases=prefix/'releases'
    if releases.is_symlink(): raise ValueError('Release directory may not be a symlink')
    releases.mkdir(mode=0o700,exist_ok=True);destination=releases/a.version
    if destination.exists() or destination.is_symlink():
        if destination.is_symlink() or json.loads((destination/'build-info.json').read_text())!=info: raise ValueError('Existing version differs; never overwrite')
        for command in COMMANDS:
            binary=destination/'bin'/command
            if binary.is_symlink() or not binary.is_file() or digest(binary)!=info['binaries'][command]['sha256']: raise ValueError('Installed binary has drifted; never overwrite')
    else:
        staged=Path(tempfile.mkdtemp(prefix='.stage-',dir=releases));extract_bundle(archive,staged)
        for command in COMMANDS:
            identity=json.loads(execute([str(staged/'bin'/command),'--version']))
            if identity!=info['binaries'][command]['identity']: raise ValueError('Actual installed binary identity mismatch')
        os.rename(staged,destination)
    receipt={**plan,'asset_sha256':digest(archive),'release_id':release['id'],'immutable_release_verified':True,'build_provenance_verified':True,'directory':str(destination)}
    receipt_file=cache/'installation.json';receipt_file.write_text(json.dumps(receipt,indent=2)+'\n')
    if a.activate:
        if (os.readlink(current) if current.is_symlink() else None)!=previous: raise ValueError('Selector changed during installation; not activating')
        temporary=cache/'current';os.symlink('releases/'+a.version,temporary);os.replace(temporary,current)
    print(json.dumps(receipt,indent=2))
if __name__=='__main__': main()
