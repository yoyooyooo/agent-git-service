"""Small, closed-file primitives for the disposable Linux fixture only."""
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess


REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
INTERPRETERS = Path('/usr/local/libexec/ags-routeb-interpreters')
ID = re.compile(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,47}')
SHA = re.compile(r'[0-9a-f]{64}')
OID = re.compile(r'[0-9a-f]{40}')


class Refused(RuntimeError):
    pass


# Closed vocabulary shared with native-tools' diagnostic boundary. Unknown
# error text, including text that resembles a code, is never exported.
NATIVE_FAILURE_REASONS = frozenset("""
already_exists authority_absence_changed authority_acl_unsupported authority_ancestor_not_directory
authority_ancestor_unavailable authority_asset_changed authority_asset_not_independent
authority_asset_unreadable authority_binding_invalid authority_binding_mismatch
authority_directory_invalid authority_document_changed authority_document_size
authority_duplicate_path authority_elf_dynamic authority_elf_dynamic_disagreement
authority_elf_dynamic_duplicate authority_elf_dynamic_unmapped authority_elf_dynamic_unterminated
authority_elf_flags_unsupported authority_elf_interpreter authority_elf_invalid
authority_elf_loader_extension_unsupported authority_elf_mapping_ambiguous_or_gap
authority_elf_mapping_discontinuous authority_elf_mapping_invalid authority_elf_mapping_unmapped
authority_elf_mapping_unreadable authority_elf_string_invalid authority_elf_string_offset
authority_elf_string_table_missing authority_elf_string_table_size authority_elf_string_termination
authority_entry_invalid authority_environment_invalid authority_executable_invalid
authority_executable_not_native authority_expanded_size authority_fat_image_unsupported
authority_file_invalid authority_file_unavailable authority_file_unsafe authority_invalid
authority_inventory_unavailable authority_json_depth authority_json_duplicate authority_json_invalid
authority_json_missing authority_json_null authority_json_shape authority_json_trailing
authority_json_unknown authority_kind_invalid authority_link_changed authority_link_cycle
authority_link_dangling authority_link_depth authority_link_escape authority_link_invalid
authority_linux_entry_invalid authority_linux_entry_required authority_linux_external_rpath
authority_linux_library_member authority_linux_loader_missing authority_linux_needed_soname
authority_linux_needed_unbound authority_linux_needed_unsupported authority_linux_search_unbound
authority_linux_search_unsupported authority_macho_invalid authority_macho_loader_unsupported
authority_member_changed authority_member_count authority_member_kind_changed
authority_member_unavailable authority_metadata_changed authority_metadata_unavailable
authority_metadata_unsupported authority_native_architecture authority_native_dependency_missing
authority_native_image_changed authority_native_image_invalid authority_native_image_set
authority_native_image_unavailable authority_native_loader_configuration_missing
authority_native_loader_missing authority_native_needed_path authority_native_platform
authority_native_resolution_incomplete authority_native_search_unbound
authority_native_search_unsupported authority_native_soname_mismatch authority_parent_missing
authority_path_outside_boundary authority_path_untrusted authority_prefix_member_set
authority_prefix_metadata_unsupported authority_prefix_unavailable authority_publisher_invalid
authority_read_limit authority_recovery_asset_invalid authority_recovery_asset_mismatch
authority_recovery_asset_missing authority_reference_invalid authority_search_members_changed
authority_search_path_invalid authority_search_unavailable authority_xattrs_changed catalog_error
database_error elf_architecture elf_interpreter end_of_file filesystem_error fixture_command
fixture_database_path invalid_elf invalid_json json_type_mismatch not_found operation_error
output_encoding permission_denied publisher_archive_changed publisher_archive_invalid
publisher_archive_stream_limit publisher_archive_trailing publisher_checksum_mismatch
publisher_checksum_missing_or_duplicate publisher_checksums_unreadable publisher_expanded_size
publisher_link_invalid publisher_member_collision publisher_member_count publisher_member_path
publisher_member_size publisher_member_unsupported publisher_parent_invalid
publisher_prefix_content_mismatch publisher_prefix_link_mismatch publisher_prefix_member_mismatch
publisher_prefix_member_set unexpected_eof
""".split())
NATIVE_COMMAND_STAGES = {
    'inspect_elf': 'inspect-elf', 'verify_catalog': 'verify-catalog', 'seed_database': 'seed-db',
}
COMMAND_OPERATIONS = frozenset({
    'source_metadata', 'source_head', 'source_tree', 'source_status', 'candidate_version',
    'inspect_elf', 'verify_catalog', 'seed_database', 'manager_properties', 'manager_reload', 'initial_start',
})


class CommandFailed(Refused):
    def __init__(self, operation, result):
        super().__init__('command_failed')
        self.diagnostic = {
            'operation': operation, 'returncode': result.returncode,
            'stdout': {'bytes': len(result.stdout), 'sha256': sha(result.stdout)},
            'stderr': {'bytes': len(result.stderr), 'sha256': sha(result.stderr)},
        }
        # Only the fixed native tool may supply a closed, stage-matched reason.
        # Raw argv, environment, stdout and stderr never enter the result.
        if operation in NATIVE_COMMAND_STAGES:
            try:
                value = object_json(result.stderr, 4096)
                if (set(value) == {'schema', 'stage', 'reason'}
                        and value['schema'] == 'ags.routeb-fixture-command-error.v1'
                        and value['stage'] == NATIVE_COMMAND_STAGES[operation]
                        and type(value['reason']) is str and value['reason'] in NATIVE_FAILURE_REASONS):
                    self.diagnostic['native_tools'] = value
            except Refused:
                pass


def require(condition, code):
    if not condition:
        raise Refused(code)


def utc():
    return datetime.now(timezone.utc).isoformat()


def canonical(value):
    return (json.dumps(value, sort_keys=True, separators=(',', ':'),
                       ensure_ascii=True, allow_nan=False) + '\n').encode('ascii')


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def fingerprint(value):
    return sha(canonical(value))


def object_json(raw, maximum=1 << 20):
    require(0 < len(raw) <= maximum, 'json_size')

    def pairs(items):
        result = {}
        for key, value in items:
            require(key not in result, 'duplicate_json_key')
            result[key] = value
        return result

    def constant(_):
        raise Refused('nonfinite_json')

    try:
        result = json.loads(raw, object_pairs_hook=pairs, parse_constant=constant)
    except (ValueError, UnicodeError, RecursionError):
        raise Refused('invalid_json') from None
    require(type(result) is dict, 'json_object_required')
    return result


def absolute(path):
    path = Path(path)
    require(path.is_absolute() and str(path) == os.path.normpath(str(path))
            and path.resolve() == path, 'noncanonical_path')
    return path


def read_bytes(path, maximum=1 << 20, *, owner=None, mode=None):
    path = absolute(path)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC)
    with os.fdopen(fd, 'rb') as stream:
        before = os.fstat(stream.fileno())
        require(stat.S_ISREG(before.st_mode) and before.st_nlink == 1
                and 0 <= before.st_size <= maximum, 'file_shape')
        require(owner is None or before.st_uid == owner, 'file_owner')
        require(mode is None or stat.S_IMODE(before.st_mode) == mode, 'file_mode')
        raw = stream.read(maximum + 1)
        after = os.fstat(stream.fileno())
        signature = lambda s: (s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns)
        require(len(raw) == before.st_size and signature(before) == signature(after), 'file_changed')
        return raw


def read_json(path, maximum=1 << 20, **kwargs):
    raw = read_bytes(path, maximum, **kwargs)
    return object_json(raw, maximum), raw


def fsync_dir(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def exclusive(path, raw, *, mode=0o600, uid=None, gid=None):
    path = absolute(path)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, mode)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        os.fchmod(stream.fileno(), mode)
        if uid is not None:
            os.fchown(stream.fileno(), uid, gid)
        stream.flush()
        os.fsync(stream.fileno())
    fsync_dir(path.parent)
    return {'path': str(path), 'sha256': sha(raw), 'size': len(raw)}


def exclusive_json(path, value, **kwargs):
    return exclusive(path, canonical(value), **kwargs)


def reference(path, maximum=1 << 20):
    raw = read_bytes(path, maximum)
    return {'path': str(path), 'sha256': sha(raw), 'size': len(raw)}


def absent(path):
    try:
        Path(path).lstat()
    except FileNotFoundError:
        return True
    return False


def protected_directory(path):
    path = absolute(path)
    for current in (path, *path.parents):
        info = current.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0
                and not stat.S_IMODE(info.st_mode) & 0o022, 'unprotected_directory')
    return path


def file_reference(value, *, owner=0, maximum=1 << 20):
    require(type(value) is dict and set(value) == {'path', 'sha256', 'size'}
            and type(value['sha256']) is str and SHA.fullmatch(value['sha256'])
            and type(value['size']) is int and 0 < value['size'] <= maximum, 'file_reference')
    raw = read_bytes(value['path'], maximum, owner=owner)
    require(len(raw) == value['size'] and sha(raw) == value['sha256'], 'file_reference_changed')
    return raw


def process_identity(pid):
    require(type(pid) is int and pid > 1, 'pid_invalid')
    proc = Path('/proc') / str(pid)
    raw = (proc / 'stat').read_text()
    close = raw.rfind(')')
    require(close > 0 and raw[:raw.find(' ')] == str(pid), 'process_stat')
    fields = raw[close + 2:].split()
    status = (proc / 'status').read_text().splitlines()
    identities = {}
    for line in status:
        key, separator, value = line.partition(':')
        if separator and key in ('Uid', 'Gid'):
            values = value.split()
            require(len(values) == 4 and len(set(values)) == 1, 'process_credentials')
            identities[key] = int(values[0])
    require(set(identities) == {'Uid', 'Gid'}, 'process_credentials')
    return {'pid': pid, 'parent_pid': int(fields[1]), 'start_ticks': int(fields[19]),
            'uid': identities['Uid'], 'gid': identities['Gid']}


def command(argv, *, operation, timeout=60, cwd=None, env=None):
    require(operation in COMMAND_OPERATIONS, 'command_operation_invalid')
    result = subprocess.run(argv, cwd=cwd, env=env, capture_output=True, timeout=timeout)
    if result.returncode != 0:
        raise CommandFailed(operation, result)
    return result.stdout
