"""Produce a catalog from pinned publisher bytes and actual Linux files.

Only the production Go verifier decides whether this inventory closes the
interpreter. This producer never writes catalog_verified/native_pass claims.
"""
import os
import posixpath
import re
from pathlib import Path, PurePosixPath
import stat
import tarfile
from urllib.parse import quote
from urllib.request import urlopen

from common import (INTERPRETERS, command, exclusive, exclusive_json, file_reference,
                    object_json, protected_directory, read_bytes, read_json,
                    reference, require, sha)


def metadata(path):
    info = path.lstat()
    require(info.st_uid == 0 and not stat.S_IMODE(info.st_mode) & 0o7000,
            'authority_owner_or_special_mode')
    attributes = {}
    for key in os.listxattr(path, follow_symlinks=False):
        require('posix_acl' not in key and key != 'security.capability', 'authority_extended_permissions')
        attributes[key] = sha(os.getxattr(path, key, follow_symlinks=False))
    return {'mode': stat.S_IMODE(info.st_mode), 'gid': info.st_gid,
            'device': info.st_dev, 'inode': info.st_ino, 'mtime_ns': info.st_mtime_ns,
            'xattrs': attributes}


def entry(path, name):
    value = {'path': name, 'metadata': metadata(path)}
    info = path.lstat()
    if stat.S_ISLNK(info.st_mode):
        value.update(kind='symlink', link=os.readlink(path))
    elif stat.S_ISDIR(info.st_mode):
        value.update(kind='directory')
    else:
        require(stat.S_ISREG(info.st_mode) and info.st_nlink == 1, 'authority_member_type')
        raw = read_bytes(path, 128 << 20, owner=0)
        value.update(kind='file', file={'sha256': sha(raw), 'size': len(raw)})
    return value


def catalog_file_conditions(value, catalog_digest):
    """Project input facts after authority_file_invalid; never validate or authorize.

    The two File/Entry predicates are in interpreterauthority/catalog.go. Keep
    paths and invalid strings opaque, and bound output independently of inventory.
    """
    rows, count = [], 0

    def add(scope, index, field, path, **numbers):
        nonlocal count
        count += 1
        if len(rows) < 8:
            rows.append({'scope': scope, 'index': index, 'field': field,
                         'path_sha256': sha(path.encode('utf-8')) if type(path) is str else None,
                         **numbers})

    def content(item, scope, index, path, maximum):
        digest, size = item.get('sha256'), item.get('size')
        if type(digest) is not str or not re.fullmatch('[0-9a-f]{64}', digest) or digest == '0' * 64:
            add(scope, index, 'sha256', path)
        if type(size) is not int or not 0 <= size <= maximum:
            add(scope, index, 'size', path, size=size if type(size) is int else None, maximum=maximum)

    def reference(item, scope, index, maximum):
        path = item.get('path')
        if (type(path) is not str or not path or any(c in path for c in '\\\x00\r\n\t')
                or not posixpath.isabs(path) or path.startswith('//') or posixpath.normpath(path) != path):
            add(scope, index, 'path', path)
        content(item, scope, index, path, maximum)

    def entry_facts(item, scope, index):
        if item['kind'] != 'file':
            return
        path, file = item['path'], item.get('file')
        if file is None:
            add(scope, index, 'file', path)
        else:
            content(file, scope, index, path, 128 << 20)
        if item.get('link') is not None:
            add(scope, index, 'link', path)
        mode = item['metadata']['mode']
        if type(mode) is int and mode & 0o022:
            add(scope, index, 'mode', path, mode=mode, non_owner_write_bits=mode & 0o022)

    reference(value['publisher']['archive'], 'publisher_archive', None, 256 << 20)
    reference(value['publisher']['checksums'], 'publisher_checksums', None, 16 << 20)
    for index, item in enumerate(value['prefix']['entries']):
        entry_facts(item, 'prefix_entry', index)
    for index, item in enumerate(value['external']):
        entry_facts(item['entry'], 'external_entry', index)
        if item['entry']['kind'] == 'file' and item.get('asset') is not None:
            reference(item['asset'], 'external_asset', index, 128 << 20)
    return {'authority_sha256': catalog_digest, 'matching_conditions': count,
            'conditions': rows, 'truncated': count > len(rows)}


def publisher_download(pin, directory):
    require(pin['repository'] == 'astral-sh/python-build-standalone'
            and pin['release'].isdigit() and len(pin['release']) == 8, 'publisher_pin')
    result = {}
    for key, name in (('archive', pin['asset_name']), ('checksums', 'SHA256SUMS')):
        path = directory / name
        maximum = (256 if key == 'archive' else 16) << 20
        require(0 < pin[key + '_size'] <= maximum, 'publisher_pin_size')
        url = ('https://github.com/astral-sh/python-build-standalone/releases/download/'
               + pin['release'] + '/' + quote(name, safe=''))
        with urlopen(url, timeout=60) as response:
            raw = response.read(pin[key + '_size'] + 1)
        require(len(raw) == pin[key + '_size'] and sha(raw) == pin[key + '_sha256'],
                'publisher_download_mismatch')
        result[key] = exclusive(path, raw, mode=0o644)
    checksums = read_bytes(result['checksums']['path'], 16 << 20).decode('ascii').splitlines()
    matches = [line.split()[0] for line in checksums if len(line.split()) == 2
               and line.split()[1].lstrip('*') == pin['asset_name']]
    require(matches == [pin['archive_sha256']], 'publisher_checksum_mismatch')
    return {'repository': pin['repository'], 'release': pin['release'],
            'asset_name': pin['asset_name'], **result}


def member_name(name):
    clean = name.rstrip('/')
    path = PurePosixPath(clean)
    require(not path.is_absolute() and path.parts and path.parts[0] == 'python'
            and str(path) == clean
            and '..' not in path.parts and '\\' not in name and '\x00' not in name,
            'publisher_member_path')
    return Path(*path.parts[1:])


def extract_publisher(archive, prefix):
    """No tar.extract: inspect the closed tree before creating any member."""
    with tarfile.open(archive, 'r:gz') as source:
        members, names, total = [], {}, 0
        for member in source:
            require(len(members) < 16384 and member.uid == member.gid == 0
                    and 0 <= member.size <= 128 << 20 and not member.mode & ~0o777
                    and set(member.pax_headers) <= {'path', 'linkpath', 'mtime', 'size'},
                    'publisher_member_unsupported')
            name = member_name(member.name)
            require(str(name) not in names and (member.isfile() or member.isdir() or member.issym()),
                    'publisher_member_unsupported')
            names[str(name)] = member
            members.append((name, member))
            if member.isfile():
                total += member.size
                require(total <= 512 << 20, 'publisher_expanded_budget')
            else:
                require(member.size == 0, 'publisher_member_size')
            if member.issym():
                require(member.linkname and not Path(member.linkname).is_absolute()
                        and '\x00' not in member.linkname and '\\' not in member.linkname,
                        'publisher_link')
                target = os.path.normpath(str(name.parent / member.linkname))
                require(target != '..' and not target.startswith('../'), 'publisher_link_escape')
        for name, member in members:
            for parent in name.parents:
                require(str(parent) not in names or names[str(parent)].isdir(), 'publisher_parent')
        prefix.mkdir(mode=0o755)
        directories = {Path('.')}
        for name, member in members:
            directories.update(name.parents)
            if member.isdir():
                directories.add(name)
        for name in sorted(directories, key=lambda p: (len(p.parts), str(p))):
            if name != Path('.'):
                (prefix / name).mkdir(mode=0o755)
        for name, member in members:
            if member.isfile():
                with source.extractfile(member) as stream:
                    raw = stream.read(member.size + 1)
                require(len(raw) == member.size, 'publisher_member_size')
                exclusive(prefix / name, raw, mode=0o755 if member.mode & 0o111 else 0o644)
        for name, member in members:
            if member.issym():
                os.symlink(member.linkname, prefix / name)
        for name, member in members:
            if member.issym():
                require((prefix / name).resolve().is_relative_to(prefix)
                        and (prefix / name).resolve().exists(), 'publisher_link_escape')


def produce(pin_path, authority_id, binding, inspector):
    require(os.getuid() == 0 and binding['platform'] == 'linux' and binding['architecture'] == 'amd64',
            'root_linux_authority_required')
    directory = INTERPRETERS / authority_id
    protected_directory(directory.parent)
    directory.mkdir(mode=0o755)
    for name in ('publisher', 'runtime-libraries', 'recovery-assets', 'timezone'):
        (directory / name).mkdir(mode=0o755)
    pin, _ = read_json(pin_path)
    publisher = publisher_download(pin, directory / 'publisher')
    prefix = directory / 'python'
    extract_publisher(Path(publisher['archive']['path']), prefix)
    executable = prefix / pin['executable']
    require(executable.is_file() and not executable.is_symlink(), 'publisher_executable')
    external_paths, images, sealed = {}, {}, {}
    libraries = directory / 'runtime-libraries'
    timezone = directory / 'timezone'
    os.chmod(timezone, 0o755)  # Newly created, root-owned input; never host tzdata.

    def inspect(path):
        value = object_json(command([str(inspector), 'inspect-elf', str(path)], operation='inspect_elf', timeout=10))
        if value['native']:
            images[str(path)] = value['image']
            return value['image']
        return None

    def add_external(path):
        path = Path(path)
        if str(path) in external_paths or path.is_relative_to(prefix):
            return
        require(len(external_paths) < 1024, 'external_inventory_budget')
        external_paths[str(path)] = path

    def resolve(path):
        """Keep every real root-owned alias traversed by an external input."""
        current = Path(path)
        for _ in range(64):
            changed = False
            for ancestor in reversed((current, *current.parents)):
                if ancestor.is_symlink():
                    add_external(ancestor)
                    link = Path(os.readlink(ancestor))
                    target = link if link.is_absolute() else ancestor.parent / link
                    suffix = current.relative_to(ancestor)
                    current = Path(os.path.normpath(str(target / suffix)))
                    changed = True
                    break
            if not changed:
                require(current.is_file(), 'elf_dependency_missing')
                return current
        raise ValueError('elf_alias_depth')

    for path in sorted(prefix.rglob('*')):
        if path.is_file() and not path.is_symlink():
            inspect(path)
    pending = list(images)
    seen = set()
    while pending:
        name = pending.pop()
        if name in seen:
            continue
        seen.add(name)
        require(len(images) <= 256, 'native_image_budget')
        image = images[name]
        if image['interpreter']:
            loader = resolve(image['interpreter'])
            add_external(loader)
            if str(loader) not in images:
                require(inspect(loader) is not None, 'elf_loader_not_native')
                pending.append(str(loader))
        resolutions = {}
        for needed in image['needed']:
            if '/' in needed or '$' in needed:
                require(needed.startswith(('$ORIGIN/', '${ORIGIN}/')), 'elf_needed_unsupported')
                expanded = needed.replace('${ORIGIN}', str(Path(name).parent)).replace('$ORIGIN', str(Path(name).parent))
                target = Path(os.path.normpath(expanded)).resolve()
                require(target.is_relative_to(prefix) and target.is_file(), 'elf_needed_escape')
            else:
                require(needed not in ('.', '..') and '\\' not in needed, 'elf_needed_unsupported')
                candidates = [prefix / 'lib' / needed,
                              Path('/usr/lib/x86_64-linux-gnu') / needed,
                              Path('/lib/x86_64-linux-gnu') / needed]
                candidates = [path for path in candidates if path.is_file()]
                require(candidates, 'elf_dependency_missing')
                target = resolve(candidates[0])
                if needed not in sealed:
                    raw = read_bytes(target, 128 << 20, owner=0)
                    sealed[needed] = libraries / needed
                    exclusive(sealed[needed], raw, mode=0o644)
                    add_external(sealed[needed])
                    require(inspect(sealed[needed]) is not None, 'elf_dependency_not_native')
                    pending.append(str(sealed[needed]))
            resolutions[needed] = str(target)
            if not target.is_relative_to(prefix):
                add_external(target)
            if str(target) not in images:
                require(inspect(target) is not None, 'elf_dependency_not_native')
                pending.append(str(target))
        image['resolution'] = resolutions

    loader = resolve(images[str(executable)]['interpreter'])
    add_external(libraries)
    add_external(timezone)
    for path in (Path('/etc/passwd'), Path('/etc/group'), Path('/etc/nsswitch.conf')):
        add_external(resolve(path))
    # The supported Linux/amd64 glibc startup also maps this non-ELF data.
    # Resolve and bind it like every other external, including its recovery asset.
    add_external(resolve(Path('/usr/lib/x86_64-linux-gnu/gconv/gconv-modules.cache')))
    # Default glibc locale lookup tries the archive before split locale files.
    # Keep the existing locale environment and seal the archive when present.
    locale_archive = Path('/usr/lib/locale/locale-archive')
    if locale_archive.exists() or locale_archive.is_symlink():
        add_external(resolve(locale_archive))
    locale = Path('/usr/lib/locale/C.utf8')
    require(locale.is_dir(), 'utf8_locale_unavailable')
    add_external(locale)
    for path in sorted(locale.rglob('*')):
        require(not path.is_symlink(), 'locale_alias_unsupported')
        add_external(path)
    require(not Path('/etc/ld.so.preload').exists() and not Path('/etc/ld.so.preload').is_symlink(),
            'loader_preload_present')
    searches = [{'path': '/etc/ld.so.preload', 'state': 'absent', 'members': []},
                {'path': str(libraries), 'state': 'closed-directory', 'members': sorted(sealed)},
                {'path': str(timezone), 'state': 'closed-directory', 'members': []}]
    for image in images.values():
        for group in image['rpath'] + image['runpath']:
            for part in group.split(':'):
                expanded = part.replace('${ORIGIN}', str(Path(image['path']).parent)).replace('$ORIGIN', str(Path(image['path']).parent))
                require(expanded and '$' not in expanded and Path(expanded).is_absolute(), 'elf_search_unsupported')
                path = Path(os.path.normpath(expanded))
                if not path.is_relative_to(prefix) and path != libraries:
                    require(not path.exists() and not path.is_symlink(), 'external_rpath_present')
                    if not any(row['path'] == str(path) for row in searches):
                        searches.append({'path': str(path), 'state': 'absent', 'members': []})

    externals = []
    for name, path in sorted(external_paths.items()):
        row = {'entry': entry(path, name)}
        if row['entry']['kind'] == 'file':
            asset = directory / 'recovery-assets' / (sha(name.encode()) + '.asset')
            row['asset'] = exclusive(asset, read_bytes(path, 128 << 20, owner=0), mode=0o644)
        externals.append(row)
    prefix_entries = [entry(prefix, '.')]
    prefix_entries += [entry(path, str(path.relative_to(prefix))) for path in sorted(prefix.rglob('*'))]
    value = {'schema': 'ags.interpreter-authority.v1', 'id': authority_id, 'binding': binding,
             'publisher': publisher, 'prefix': {'root': str(prefix), 'entries': prefix_entries},
             'executable': pin['executable'], 'external': externals,
             'search_paths': searches, 'native_images': [images[key] for key in sorted(images)],
             'environment': {'LANG': 'C.UTF-8', 'LC_ALL': 'C.UTF-8',
                             'TZ': 'UTC0', 'TZDIR': str(timezone)},
             'linux': {'loader': str(loader), 'library_directory': str(libraries)}}
    return exclusive_json(directory / 'authority.json', value, mode=0o644)
