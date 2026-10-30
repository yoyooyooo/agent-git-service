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
MAX_ARCHIVE=128*1024*1024
MAX_UNPACKED=256*1024*1024
MAX_MEMBER=128*1024*1024
MANIFEST_SCHEMA='ags.release.v2'
LEGACY_MANIFEST_SCHEMA='ags.release.v1'
ABI_SCHEMA='ags.abi.v2'
PIN_SCHEMA='ags.deploy-release-pin.v2'

def abi_for(platform_target):
    if platform_target=='linux_amd64':
        return {'schema':ABI_SCHEMA,'os':'linux','arch':'amd64','build_distribution':'ubuntu',
                'build_distribution_version':'22.04','libc':'glibc','minimum_libc':'2.35'}
    if platform_target=='darwin_arm64':
        return {'schema':ABI_SCHEMA,'os':'darwin','arch':'arm64','minimum_os':'15.0','system_dynamic_libraries_only':True}
    raise ValueError('release_abi_incompatible')

def validate_abi(abi, platform_target):
    if json.dumps(abi,sort_keys=True)!=json.dumps(abi_for(platform_target),sort_keys=True): raise ValueError('release_abi_incompatible')

def validate_manifest_contract(info, platform_target, *, allow_legacy=False):
    if info.get('schema')==LEGACY_MANIFEST_SCHEMA and allow_legacy:
        # Historical v1 is not an alias for the structured v2 ABI.
        if 'abi' in info or '22.04' in info.get('minimum_platform',''):
            raise ValueError('release_manifest_invalid')
    elif info.get('schema')==MANIFEST_SCHEMA:
        validate_abi(info.get('abi'),platform_target)
        if any(v in info.get('minimum_platform','') for v in ('24.04','2.39')):
            raise ValueError('release_abi_incompatible')
        if info.get('toolchain')!={'go_version':'go1.26.8','pin_path':'.go-version'}:
            raise ValueError('release_manifest_invalid')
        for command in COMMANDS:
            binary=info.get('binaries',{}).get(command,{})
            if binary.get('cgo_enabled') is not (command=='gh-server') or binary.get('identity',{}).get('go_version')!='go1.26.8':
                raise ValueError('release_abi_incompatible')
    else:
        raise ValueError('release_manifest_invalid')

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
            if entry.size>MAX_MEMBER or total>MAX_UNPACKED: raise ValueError('Archive expands beyond size limit')
            names.add(entry.name)
        if not {'LICENSE','build-info.json',*('bin/'+c for c in COMMANDS)}<=names: raise ValueError('Incomplete release bundle')
        if tf.getmember('build-info.json').size>1024*1024: raise ValueError('Oversized build manifest')
        info=json.load(tf.extractfile('build-info.json'))
        validate_manifest_contract(info,platform_target,allow_legacy=True)
        if info.get('schema')==MANIFEST_SCHEMA and names!=allow: raise ValueError('Incomplete v2 release bundle')
        if info.get('repository')!=REPO or info.get('repository_id')!=REPO_ID or info.get('revision')!=source or not SHA.fullmatch(info.get('tree','')) or info.get('version')!=version or info.get('target')!=platform_target:
            raise ValueError('Release/source/platform identity mismatch')
        if set(info.get('binaries',{}))!=set(COMMANDS): raise ValueError('Unexpected binary inventory')
        for command in COMMANDS:
            data=tf.extractfile('bin/'+command).read(); binary=info['binaries'][command]; identity=binary['identity']
            if hashlib.sha256(data).hexdigest()!=binary['sha256'] or len(data)!=binary['size']:
                raise ValueError('Binary checksum mismatch')
            if identity.get('schema')!='ags.build.v1' or identity['command']!=command or identity['revision']!=source or identity['tree']!=info['tree'] or identity['version']!=version or identity['goos']+'_'+identity['goarch']!=platform_target:
                raise ValueError('Embedded binary identity mismatch')
        return info

class SourceBindingUnverified(ValueError):
    pass

def require_installable_manifest(info):
    if info.get('schema')==MANIFEST_SCHEMA:
        # Tag destination and separated-control proof are not approved. Do not
        # route v2 through the historical release.yml/runtime-digest policy.
        raise SourceBindingUnverified('release_source_binding_unverified')

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

def adjacent_module(name):
    import importlib.util
    path=Path(__file__).with_name(name+'.py')
    if not path.is_file() or path.is_symlink():
        raise SourceBindingUnverified('dependency_unavailable: use a complete reviewed checkout for v2')
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module

def main():
    p=argparse.ArgumentParser(description=__doc__)
    p.add_argument('--version',required=True);p.add_argument('--prefix',type=Path,default=Path.home()/'.ags')
    mode=p.add_mutually_exclusive_group()
    mode.add_argument('--install',action='store_true');mode.add_argument('--verify-only',action='store_true')
    p.add_argument('--allow-prerelease',action='store_true')
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
    # An exact nine-asset inventory selects v2. A damaged v2 cannot fall back to v1.
    v2_names={'release-source.json','attestation-darwin_arm64.sigstore.json','attestation-linux_amd64.sigstore.json'}
    is_v2=any(asset['name'] in v2_names for asset in release['assets'])
    if is_v2:
        helper=adjacent_module('release_install_v2')
        import sys
        print(json.dumps(helper.run(sys.modules[__name__],a,release,source,platform_target),indent=2))
        return
    matching=[asset for asset in release['assets'] if asset['name']==name]
    if len(matching)!=1 or not matching[0].get('digest','').startswith('sha256:'): raise ValueError('Exact asset or digest is missing')
    if matching[0]['size']>MAX_ARCHIVE: raise ValueError('Oversized release asset')
    prefix=a.prefix.expanduser().absolute()
    if prefix.is_symlink() or prefix.resolve()!=prefix: raise ValueError('Installation prefix must be canonical and not a symlink')
    plan={'schema':INSTALLATION_SCHEMA,'repository':REPO,'version':a.version,'source':source,'target':platform_target,'prefix':str(prefix),'install':a.install,'service_or_data_changed':False,'verification':{'metadata':'verified','asset_bytes':'unverified','immutable_release':'unverified','attestation':'unverified','archive':'unverified','executable_identity':'unverified'}}
    if not a.install and not a.verify_only: print(json.dumps(plan,indent=2));return
    os.umask(0o077)
    if a.install: owned_directory(prefix)
    # Downloads are transaction-local on both success and verification failure.
    with tempfile.TemporaryDirectory(prefix='.download-',dir=prefix if a.install else None) as directory:
        cache=Path(directory)
        execute(['gh','release','download',a.version,'--repo',REPO,'--pattern',name,'--dir',str(cache)])
        archive=cache/name
        if 'sha256:'+digest(archive)!=matching[0]['digest'] or archive.stat().st_size!=matching[0]['size']: raise ValueError('Downloaded asset differs from GitHub digest/size')
        execute(['gh','release','verify-asset',a.version,str(archive),'--repo',REPO])
        info=inspect_bundle(archive,a.version,source,platform_target)
        require_installable_manifest(info)
        execute(['gh','attestation','verify',str(archive),'--repo',REPO,'--source-digest',source,'--signer-workflow',REPO+'/.github/workflows/release.yml','--deny-self-hosted-runners'])
        receipt={**plan,'asset_sha256':digest(archive),'release_id':release['id'],'immutable_release_verified':True,'build_provenance_verified':True,'binaries':info['binaries']}
        receipt['verification']={'metadata':'verified','asset_bytes':'verified','immutable_release':'verified','attestation':'verified','archive':'verified','executable_identity':'unverified'}
        if a.install:
            install_verified(archive,info,receipt,prefix)
            receipt['verification']['executable_identity']='verified'
    print(json.dumps(receipt,indent=2))
if __name__=='__main__':
    try: main()
    except SourceBindingUnverified as error:
        import sys
        print(str(error),file=sys.stderr);sys.exit(65)
