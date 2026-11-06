#!/usr/bin/env python3
"""Online native-parent context. No receipt, argv or environment grants entry.

Linux children cannot normally read a root parent's /proc/PID/exe. The fixed
root registration and privileged service manager are explicit trusted bases;
the kernel authenticates the live root peer. The native parent, not this child,
verifies its executable, namespaces, child image, mappings and source closure.
"""
import hashlib
import os
from pathlib import Path
import socket
import stat
import struct
import sys

from upgrade_backup_contract import ContractError, canonical, identifier, require, strict_json
from upgrade_identity import read_trusted


REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
SOCKETS = Path('/run/ags-routeb-stage0')
PROTOCOL = 'ags.routeb-stage0-peer.v1'
CONTEXT_SCHEMA = 'ags.routeb-stage0-context.v1'
CONTEXT_FIELDS = set(('schema registration_id registration_sha256 root operation phase invocation_id '
                      'attempt_id authority_epoch authorization_id boot_id supervisor child '
                      'interpreter_authority_sha256 tool_revision tool_tree tool_manifest_sha256 '
                      'recovery_registration_sha256 scope_grant_sha256').split())
PROCESS_FIELDS = {'pid', 'start_ticks', 'parent_pid', 'uid', 'gid', 'executable_sha256'}


def _digest(value):
    return isinstance(value, str) and len(value) == 64 and value != '0' * 64 and all(c in '0123456789abcdef' for c in value)


def _process(value):
    require(type(value) is dict and set(value) == PROCESS_FIELDS, 'stage0_process_shape')
    for name in ('pid', 'start_ticks', 'parent_pid'):
        require(type(value[name]) is int and value[name] > 0, 'stage0_process_shape')
    for name in ('uid', 'gid'):
        require(type(value[name]) is int and value[name] >= 0, 'stage0_process_shape')
    require(_digest(value['executable_sha256']), 'stage0_process_shape')


def _registered():
    require(sys.platform.startswith('linux') and os.getuid() != 0 and os.getuid() == os.geteuid()
            and os.getgid() == os.getegid(), 'interpreter_full_closure_required')
    require(sys.flags.isolated == 1 and sys.flags.ignore_environment == 1
            and sys.flags.no_site == 1 and sys.flags.no_user_site == 1
            and sys.dont_write_bytecode, 'stage0_isolated_interpreter_required')
    source = Path(__file__).absolute()
    bundle = source.parent
    require(bundle.name == 'bundle' and bundle.parent.parent == REGISTRY,
            'stage0_fixed_bundle_required')
    transaction = bundle.parent.name
    identifier(transaction)
    # read_trusted checks every root-owned ancestor, no-follow file identity,
    # bounds and metadata stability. This file is not selected by a caller.
    raw = read_trusted(bundle.parent / 'stage0-registration.json', maximum=1024 * 1024)
    registration = strict_json(raw)
    require(type(registration) is dict and registration.get('schema') == 'ags.routeb-stage0.v1'
            and registration.get('id') == transaction and registration.get('scope') == 'isolated-native',
            'stage0_registration_invalid')
    host_ref = registration.get('host_profile') or {}
    require(host_ref.get('path') == str(bundle.parent / 'host-profile.json'), 'stage0_host_binding')
    host_raw = read_trusted(bundle.parent / 'host-profile.json')
    require(hashlib.sha256(host_raw).hexdigest() == host_ref.get('sha256'), 'stage0_host_changed')
    host = strict_json(host_raw)
    require(type(host) is dict and host.get('schema') == 'ags.routeb-native-identity.v1'
            and host.get('scope') == 'isolated-native' and host.get('transaction_id') == transaction
            and host.get('uid') == os.getuid() and host.get('gid') == os.getgid()
            and host.get('tool_bundle') == str(bundle), 'stage0_host_binding')
    # The root native parent binds the full manifest before exec. This read
    # independently ties the child context to this exact registered bundle.
    binding = registration.get('binding') or {}
    manifest_raw = read_trusted(bundle / 'manifest.json', maximum=1024 * 1024)
    require(hashlib.sha256(manifest_raw).hexdigest() == binding.get('tool_manifest_sha256'),
            'stage0_bundle_changed')
    manifest = strict_json(manifest_raw)
    require(hashlib.sha256(read_trusted(source)).hexdigest() == manifest.get('files', {}).get(source.name),
            'stage0_client_changed')
    return registration, host, hashlib.sha256(raw).hexdigest()


def _validate_context(value, registration, host, registration_sha, parent):
    require(type(value) is dict and set(value) == CONTEXT_FIELDS
            and value['schema'] == CONTEXT_SCHEMA, 'stage0_context_shape')
    _process(value['supervisor']); _process(value['child'])
    binding = registration['binding']
    require(value['registration_id'] == registration['id'] and value['registration_sha256'] == registration_sha
            and value['root'] == host['root'] and value['supervisor']['pid'] == parent
            and value['supervisor']['uid'] == value['supervisor']['gid'] == 0
            and value['supervisor']['parent_pid'] == 1
            and value['supervisor']['executable_sha256'] == registration['native_image']['sha256']
            and value['child']['pid'] == os.getpid() and value['child']['parent_pid'] == parent
            and value['child']['uid'] == os.getuid() and value['child']['gid'] == os.getgid()
            and value['interpreter_authority_sha256'] == registration['interpreter']['sha256']
            and value['tool_revision'] == binding['tool_revision'] and value['tool_tree'] == binding['tool_tree']
            and value['tool_manifest_sha256'] == binding['tool_manifest_sha256'], 'stage0_context_binding')
    matches = [item for item in registration['invocations'] if item['id'] == value['invocation_id']]
    require(len(matches) == 1 and all(value[key] == matches[0][key] for key in
            ('operation', 'phase', 'attempt_id', 'authority_epoch', 'authorization_id')),
            'stage0_invocation_binding')
    require(type(value['authority_epoch']) is int and value['authority_epoch'] == 1
            and type(value['boot_id']) is str and len(value['boot_id']) == 36, 'stage0_context_shape')
    recovery = value['operation'] in ('restore', 'old-start') or value['phase'] == 'serve-old'
    require(_digest(value['recovery_registration_sha256']) if recovery else value['recovery_registration_sha256'] == '',
            'stage0_recovery_context_binding')
    grant = registration.get('scope_grant')
    require(value['scope_grant_sha256'] == (grant['sha256'] if grant is not None else ''),
            'stage0_scope_grant_binding')
    return value


def _request(method, *, root=None, invocation_id=None, generation=None, result_sha256=None):
    registration, host, registration_sha = _registered()
    require(root is None or str(Path(root)) == host['root'], 'stage0_root_mismatch')
    parent = os.getppid()
    require(parent > 1, 'interpreter_full_closure_required')
    # Socket lookup is derived solely from the kernel parent, never argv/env.
    for path in (SOCKETS, *SOCKETS.parents):
        info = path.lstat()
        require(stat.S_ISDIR(info.st_mode) and info.st_uid == 0 and not info.st_mode & 0o022,
                'stage0_socket_directory_untrusted')
    path = SOCKETS / (str(parent) + '.sock')
    info = path.lstat()
    require(stat.S_ISSOCK(info.st_mode) and info.st_uid == os.getuid()
            and stat.S_IMODE(info.st_mode) == 0o600, 'stage0_socket_untrusted')
    request = {'schema': PROTOCOL, 'method': method}
    if invocation_id is not None:
        identifier(invocation_id); request['invocation_id'] = invocation_id
    if generation is not None:
        identifier(generation); request['generation'] = generation
    if result_sha256 is not None:
        require(_digest(result_sha256), 'stage0_restore_result_digest')
        request['result_sha256'] = result_sha256
    with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as connection:
        connection.settimeout(155)
        connection.connect(str(path))
        peer_pid, peer_uid, peer_gid = struct.unpack('3i', connection.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12))
        require((peer_pid, peer_uid, peer_gid) == (parent, 0, 0) and os.getppid() == parent,
                'stage0_root_peer_required')
        connection.sendall(canonical(request))
        connection.shutdown(socket.SHUT_WR)
        raw = bytearray()
        while True:
            part = connection.recv(min(65536, 1024 * 1024 + 1 - len(raw)))
            if not part:
                break
            raw.extend(part)
            require(len(raw) <= 1024 * 1024, 'stage0_response_too_large')
        require(os.getppid() == parent, 'stage0_parent_changed')
    response = strict_json(bytes(raw))
    require(type(response) is dict and response.get('schema') == PROTOCOL
            and set(response) <= {'schema', 'ok', 'error', 'context', 'runtime', 'exit', 'action', 'writers', 'restore'},
            'stage0_response_invalid')
    if response.get('ok') is not True:
        reason = response.get('error')
        require(type(reason) is str and reason.isascii() and reason.replace('_', '').isalnum()
                and len(reason) <= 96, 'stage0_response_invalid')
        raise ContractError(reason)
    _validate_context(response.get('context'), registration, host, registration_sha, parent)
    return response


def context(root=None, operations=None):
    """Revalidate through the actual native parent; no cached readiness flag."""
    try:
        value = _request('recheck', root=root)['context']
        require(operations is None or value['operation'] in operations, 'stage0_operation_mismatch')
        return value
    except (OSError, ValueError, KeyError, TypeError) as error:
        if isinstance(error, ContractError):
            raise
        raise ContractError('interpreter_full_closure_required') from None


def manager_action(root, action):
    require(action in ('stop', 'start'), 'stage0_manager_action_invalid')
    return _request('manager_' + action, root=root)


def runtime_identity(root):
    return _request('runtime_identity', root=root)['runtime']


def runtime_exit(root, invocation_id):
    return _request('runtime_exit', root=root, invocation_id=invocation_id)['exit']


def writer_probe(root, generation):
    return _request('writer_probe', root=root, generation=generation)['writers']


def complete_restore(root, result_sha256):
    return _request('restore_complete', root=root, result_sha256=result_sha256)['restore']


def restore_result(root):
    return _request('restore_result', root=root)['restore']
