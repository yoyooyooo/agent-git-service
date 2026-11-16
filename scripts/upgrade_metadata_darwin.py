#!/usr/bin/env python3
"""Darwin ACL and xattr capture. Values stay in a private sidecar, not in logs.

Linux and other platforms are not this backend. A missing or unreadable
attribute is a refusal, not an empty digest.
"""
from __future__ import annotations

import ctypes
import hashlib
import subprocess
import sys

from upgrade_backup_contract import ContractError, canonical, fingerprint, require


ACL_TYPE_EXTENDED = 0x00000100
XATTR_NOFOLLOW = 0x0001
NONE_ACL = hashlib.sha256(b'acl:none').hexdigest()


def _libc():
    if sys.platform != 'darwin':
        raise ContractError('metadata_platform_unsupported')
    library = ctypes.CDLL('/usr/lib/libSystem.B.dylib', use_errno=True)
    library.acl_get_link_np.argtypes = [ctypes.c_char_p, ctypes.c_int]
    library.acl_get_link_np.restype = ctypes.c_void_p
    library.acl_set_link_np.argtypes = [ctypes.c_char_p, ctypes.c_int, ctypes.c_void_p]
    library.acl_set_link_np.restype = ctypes.c_int
    library.acl_to_text.argtypes = [ctypes.c_void_p, ctypes.POINTER(ctypes.c_ssize_t)]
    library.acl_to_text.restype = ctypes.c_void_p
    library.acl_from_text.argtypes = [ctypes.c_char_p]
    library.acl_from_text.restype = ctypes.c_void_p
    library.acl_free.argtypes = [ctypes.c_void_p]
    library.acl_free.restype = ctypes.c_int
    library.listxattr.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_size_t, ctypes.c_int]
    library.listxattr.restype = ctypes.c_ssize_t
    library.getxattr.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_void_p,
                                 ctypes.c_size_t, ctypes.c_uint32, ctypes.c_int]
    library.getxattr.restype = ctypes.c_ssize_t
    library.setxattr.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_void_p,
                                 ctypes.c_size_t, ctypes.c_uint32, ctypes.c_int]
    library.setxattr.restype = ctypes.c_int
    library.removexattr.argtypes = [ctypes.c_char_p, ctypes.c_char_p, ctypes.c_int]
    library.removexattr.restype = ctypes.c_int
    return library


def _acl_text(path):
    library = _libc()
    pointer = library.acl_get_link_np(str(path).encode(), ACL_TYPE_EXTENDED)
    if not pointer:
        return b''
    try:
        length = ctypes.c_ssize_t()
        text = library.acl_to_text(pointer, ctypes.byref(length))
        require(text and length.value > 0, 'metadata_unavailable')
        try:
            return ctypes.string_at(text, length.value)
        finally:
            library.acl_free(text)
    finally:
        library.acl_free(pointer)


def _set_acl(path, text):
    library = _libc()
    if not text:
        result = subprocess.run(['/bin/chmod', '-N', str(path)], capture_output=True, timeout=30)
        require(result.returncode == 0, 'metadata_unavailable')
        return
    parsed = library.acl_from_text(text)
    require(parsed, 'metadata_unavailable')
    try:
        require(library.acl_set_link_np(str(path).encode(), ACL_TYPE_EXTENDED, parsed) == 0,
                'metadata_unavailable')
    finally:
        library.acl_free(parsed)


def xattr_map(path):
    library = _libc()
    encoded = str(path).encode()
    size = library.listxattr(encoded, None, 0, XATTR_NOFOLLOW)
    require(size >= 0, 'metadata_unavailable')
    if size == 0:
        return {}
    buffer = ctypes.create_string_buffer(size)
    require(library.listxattr(encoded, buffer, size, XATTR_NOFOLLOW) == size, 'metadata_unavailable')
    names = [item for item in buffer.raw.split(b'\0') if item]
    values = {}
    for name in names:
        length = library.getxattr(encoded, name, None, 0, 0, XATTR_NOFOLLOW)
        require(length >= 0, 'metadata_unavailable')
        data = ctypes.create_string_buffer(length)
        require(library.getxattr(encoded, name, data, length, 0, XATTR_NOFOLLOW) == length,
                'metadata_unavailable')
        values[name.decode('utf-8')] = bytes(data.raw[:length]).hex()
    return values


def apply_xattrs(path, attributes):
    library = _libc()
    encoded = str(path).encode()
    current = xattr_map(path)
    for name in current.keys() - attributes.keys():
        require(library.removexattr(encoded, name.encode(), XATTR_NOFOLLOW) == 0, 'metadata_unavailable')
    for name, value in attributes.items():
        raw = bytes.fromhex(value)
        if current.get(name) == value:
            continue
        require(library.setxattr(encoded, name.encode(), raw, len(raw), 0, XATTR_NOFOLLOW) == 0,
                'metadata_unavailable')


def capture(path):
    """Return digests plus the private payload needed to materialize the same ACL/xattrs."""
    attributes = xattr_map(path)
    acl = _acl_text(path)
    return {
        'acl_sha256': hashlib.sha256(acl or b'acl:none').hexdigest(),
        'xattrs_sha256': fingerprint(attributes) if attributes else hashlib.sha256(b'xattr:none').hexdigest(),
        'sidecar': {'schema': 'ags.metadata-sidecar.v1', 'acl': acl.hex(),
                    'xattrs': attributes},
    }


def materialize(path, captured):
    sidecar = captured['sidecar']
    require(sidecar.get('schema') == 'ags.metadata-sidecar.v1', 'metadata_mismatch')
    _set_acl(path, bytes.fromhex(sidecar['acl']) if sidecar['acl'] else b'')
    apply_xattrs(path, sidecar['xattrs'])


def compare(path, captured):
    current = capture(path)
    require(current['acl_sha256'] == captured['acl_sha256']
            and current['xattrs_sha256'] == captured['xattrs_sha256']
            and current['sidecar'] == captured['sidecar'], 'metadata_mismatch')
    return True
