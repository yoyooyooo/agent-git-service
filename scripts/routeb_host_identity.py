#!/usr/bin/env python3
"""Separate root-owned identities for disposable native Route B deployments.

Production identity keeps its existing fixed path. A native root has one fixed
registry path derived from its transaction ID, never an environment override or
a caller-selected trust file. Reading or validating this module provisions nothing.
"""
from dataclasses import dataclass, replace
import hashlib
import os
from pathlib import Path
import pwd

import upgrade_identity as identity
from upgrade_backup_contract import identifier, require, strict_json


REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
SCHEMA = 'ags.routeb-native-identity.v1'
FIELDS = set(('schema logical_host host_instance uid gid user home root platform arch '
              'machine_id_kind machine_id_sha256 aliases scope transaction_id tool_bundle').split())


@dataclass(frozen=True)
class VerifiedHost:
    identity: identity.HostIdentity
    scope: str
    profile: Path
    bundle: Path

    def recheck(self):
        require(resolve_for_root(self.identity.root) == self, 'host_identity_changed')
        return self


def native_locations(root, home):
    root = Path(root)
    base = Path(home) / '.local/state/ags-routeb-native'
    require(root.is_absolute() and str(root) == os.path.normpath(str(root)), 'native_root_invalid')
    require(root.parent.parent == base and root.name == 'ags', 'native_root_invalid')
    transaction = root.parent.name
    identifier(transaction)
    registry = REGISTRY / transaction
    return transaction, registry / 'host-profile.json', registry / 'bundle'


def parse_native_profile(raw, facts, root):
    """Pure validation seam. Only resolve_for_root reads the trusted profile."""
    value = strict_json(raw)
    require(type(value) is dict and set(value) == FIELDS
            and value['schema'] == SCHEMA and value['scope'] == 'isolated-native',
            'native_profile_invalid')
    transaction, profile, bundle = native_locations(root, facts.home)
    require(value['transaction_id'] == transaction and value['root'] == str(root)
            and value['tool_bundle'] == str(bundle), 'native_profile_mismatch')
    production_shape = {key: item for key, item in value.items()
                        if key not in ('scope', 'transaction_id', 'tool_bundle')}
    production_shape['schema'] = 'ags.host-identity.v1'
    production_shape['root'] = str(Path(facts.home) / '.ags')
    from upgrade_backup_contract import canonical
    checked = identity.parse_profile(canonical(production_shape), facts)
    checked = replace(checked, root=str(root), profile_sha256=hashlib.sha256(raw).hexdigest())
    return VerifiedHost(checked, 'isolated-native', profile, bundle)


def resolve_for_root(root):
    """Resolve only production ~/.ags or one registered native transaction root."""
    root = Path(root)
    home = pwd.getpwuid(os.getuid()).pw_dir
    require(root.is_absolute() and root.resolve() == root, 'host_root_invalid')
    if root == Path(home) / '.ags':
        current = identity.resolve_identity()
        identity.require_same(current, root=root)
        from upgrade import BUNDLE
        return VerifiedHost(current, 'production', identity.PROFILE, BUNDLE)
    _, profile, _ = native_locations(root, home)
    return parse_native_profile(identity.read_trusted(profile), identity.observed_facts(), root)
