"""Side-effect-free archive and ABI contracts shared by release tools.

The historical standalone installer retains its frozen v1 copy until its v2
bootstrap/dependency distribution is reviewed separately.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import hashlib
import json
from pathlib import Path
import re
import tarfile

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

