#!/usr/bin/env python3
"""Install one verified AGS release in a durable user root (default ~/.ags).
Default is a read-only plan. --install replaces the single installation, never
migrates data or restarts services. Versions identify bytes, not retained slots.
"""
from __future__ import annotations
import argparse
import contextlib
import fcntl
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
REPO_ID=1384519373
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

INSTALLATION_SCHEMA='ags.installation.v2'

def atomic_json(path, value):
    temporary=path.with_name(path.name+'.new')
    if temporary.is_symlink(): raise ValueError('Unsafe state staging path')
    fd=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_TRUNC|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'w') as out:
        json.dump(value,out,indent=2);out.write('\n');out.flush();os.fsync(out.fileno())
    os.replace(temporary,path)

def owned_directory(path):
    if path.is_symlink(): raise ValueError('Managed directory may not be a symlink')
    path.mkdir(mode=0o700,exist_ok=True)
    st=path.stat()
    if not path.is_dir() or st.st_uid!=os.getuid() or st.st_mode & 0o022:
        raise ValueError('Managed directory must be owner-controlled')

def read_receipt(path):
    if not path.exists() and not path.is_symlink(): return None
    if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
        raise ValueError('Installation receipt must be an owner-only regular file')
    receipt=json.loads(path.read_text())
    if receipt.get('schema')!=INSTALLATION_SCHEMA: raise ValueError('Unowned installation receipt')
    return receipt

def install_verified(archive, info, receipt, prefix):
    """Called only after the release/provenance/byte verification in main.

    One lock and one small interruption marker fence partial replacements.
    Retrying the same exact verified release completes an interrupted install;
    startup must reject the marker. No old program or download is retained.
    """
    owned_directory(prefix)
    for name in ('current','releases'):
        if (prefix/name).exists() or (prefix/name).is_symlink():
            raise ValueError('Legacy version slots require an explicit one-time migration')
    state=prefix/'state';owned_directory(state)
    lockfd=os.open(state/'install.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
    with os.fdopen(lockfd,'w') as lock, contextlib.ExitStack() as guards:
        fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        runtimefd=os.open(state/'runtime.lock',os.O_RDWR|os.O_CREAT|os.O_NOFOLLOW,0o600)
        runtime_lock=guards.enter_context(os.fdopen(runtimefd,'w'))
        try:
            fcntl.flock(runtime_lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:
            raise ValueError('Stop the managed runtime before replacing its programs') from None
        pending=read_receipt(state/'installing.json')
        previous=read_receipt(state/'installation.json')
        if pending and any(pending.get(k)!=receipt.get(k) for k in ('version','source','target')):
            raise ValueError('Resume the exact interrupted installation before changing its target')
        bindir=prefix/'bin';owned_directory(bindir)
        for command in COMMANDS:
            binary=bindir/command
            if binary.is_symlink(): raise ValueError('Managed binary may not be a symlink')
            if binary.exists() and not pending:
                expected=(previous or {}).get('binaries',{}).get(command,{}).get('sha256')
                if not expected or not binary.is_file() or digest(binary)!=expected:
                    raise ValueError('Existing binary is unowned or has drifted')
        # This private candidate exists only for the transaction, not as a version slot.
        with tempfile.TemporaryDirectory(prefix='.install-',dir=prefix) as name:
            staged=Path(name);extract_bundle(archive,staged)
            for command in COMMANDS:
                identity=json.loads(execute([str(staged/'bin'/command),'--version']))
                if identity!=info['binaries'][command]['identity']:
                    raise ValueError('Actual executable identity mismatch')
            atomic_json(state/'installing.json',receipt)
            for command in COMMANDS: os.replace(staged/'bin'/command,bindir/command)
            for name in ('LICENSE','NOTICE'):
                destination=state/name
                if destination.is_symlink(): raise ValueError('Unsafe attribution destination')
                if (staged/name).exists(): os.replace(staged/name,destination)
            atomic_json(state/'build-info.json',info)
            atomic_json(state/'installation.json',receipt)
            (state/'installing.json').unlink()

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--version',required=True);p.add_argument('--prefix',type=Path,default=Path.home()/'.ags')
    p.add_argument('--install',action='store_true');p.add_argument('--allow-prerelease',action='store_true')
    a=p.parse_args()
    if not VERSION.fullmatch(a.version): raise ValueError('Invalid explicit release version')
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
    plan={'schema':INSTALLATION_SCHEMA,'repository':REPO,'version':a.version,'source':source,'target':platform_target,'prefix':str(prefix),'install':a.install,'service_or_data_changed':False}
    if not a.install: print(json.dumps(plan,indent=2));return
    os.umask(0o077)
    owned_directory(prefix)
    # Downloads are transaction-local on both success and verification failure.
    with tempfile.TemporaryDirectory(prefix='.download-',dir=prefix) as directory:
        cache=Path(directory)
        execute(['gh','release','download',a.version,'--repo',REPO,'--pattern',name,'--dir',str(cache)])
        archive=cache/name
        if 'sha256:'+digest(archive)!=matching[0]['digest'] or archive.stat().st_size!=matching[0]['size']: raise ValueError('Downloaded asset differs from GitHub digest/size')
        execute(['gh','release','verify-asset',a.version,str(archive),'--repo',REPO])
        execute(['gh','attestation','verify',str(archive),'--repo',REPO,'--source-digest',source,'--signer-workflow',REPO+'/.github/workflows/release.yml','--deny-self-hosted-runners'])
        info=inspect_bundle(archive,a.version,source,platform_target)
        receipt={**plan,'asset_sha256':digest(archive),'release_id':release['id'],'immutable_release_verified':True,'build_provenance_verified':True,'binaries':info['binaries']}
        install_verified(archive,info,receipt,prefix)
    print(json.dumps(receipt,indent=2))
if __name__=='__main__': main()
