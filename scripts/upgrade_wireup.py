#!/usr/bin/env python3
"""Private host-package admission; S1-S4 remain the evidence and effect owners.

No CLI, discovery, configurable success command, or enable/skip flag. Adapter
code must be an exact-digest member of the already verified root-owned bundle.
"""
from dataclasses import dataclass
from datetime import datetime, timezone
import importlib.util
from pathlib import Path
import re

from upgrade import Failure, private_json, directory_fd
from upgrade_backup_contract import (ContractError, fields, require, identifier, digest,
                                    absolute, validate_binding, validate_group, strict_json)
from upgrade_backup import (group_object, assert_private_destination, preflight_space)
from upgrade_inventory import inspect_group, file_digest, sqlite_path
from upgrade_engine import authorize


@dataclass(frozen=True)
class Admission:
    root: Path
    request: dict
    pin: dict
    backup_binding: dict
    group: dict
    dsn: str
    schema_sha256: str
    authorization: dict
    manager: object
    writer_probe: object
    trash: object
    handoff: object
    package_path: Path
    package_sha256: str

    def validate(self, host, binding):
        from upgrade_manual_approval import check_pin, validate_request, validate_handoff, release_key
        require(host == self.request['host'] and binding == self.request, 'request_binding_mismatch')
        require(group_object(self.group, 'old_binary')['source'] == str(self.root / 'bin/gh-server')
                and group_object(self.group, 'installation')['source'] == str(self.root / 'state/installation.json'),
                'installation_mapping_mismatch')
        validate_binding(self.backup_binding)
        validate_request(binding, self.backup_binding)
        check_pin(self.pin, binding['pin_sha256'], self.backup_binding)
        require(file_digest(self.package_path) == self.package_sha256, 'private_package_changed')
        authorize(self.authorization, binding)
        require(self.authorization['authorization_id'] == binding['authorization_id']
                and self.authorization['actor']['type'] == 'human', 'window_authorization_invalid')
        if host == 'imile-win':
            validate_handoff(self.handoff, release_key(self.pin), binding['pin_sha256'], datetime.now(timezone.utc))
        else:
            require(self.handoff is None, 'unexpected_handoff')


def validate_policy(policy):
    fields(policy, ('schema', 'deployments', 'migration_admissions'))
    require(policy['schema'] == 'ags.upgrade-policy.v2' and type(policy['deployments']) is dict
            and set(policy['deployments']) <= {'mini', 'imile-win'}
            and type(policy['migration_admissions']) is list, 'policy_invalid')
    for row in policy['migration_admissions']:
        fields(row, ('old', 'new', 'migrations'))
        require(all(type(row[k]) is str and re.fullmatch('[0-9a-f]{40}', row[k])
                    for k in ('old', 'new')) and type(row['migrations']) is list
                and all(type(item) is str for item in row['migrations'])
                and row['migrations'] == sorted(set(row['migrations'])), 'migration_admission_invalid')
    for entry in policy['deployments'].values():
        fields(entry, ('root', 'readyz', 'service_readyz', 'legacy_check', 'host_package'))
        absolute(entry['root']); absolute(entry['host_package'])
        for key in ('readyz', 'service_readyz'):
            require(type(entry[key]) is str and entry[key].startswith(('http://', 'https://')),
                    'health_url_invalid')
        require(type(entry['legacy_check']) is list and len(entry['legacy_check']) > 0
                and all(type(v) is str and v for v in entry['legacy_check'])
                and Path(entry['legacy_check'][0]).is_absolute(), 'legacy_check_invalid')


def load_admission(root, pin, policy, entry, binding, authorization, bundle):
    """All checks/read-only inventory precede claim creation or manager calls."""
    from upgrade_manual_approval import check_pin, validate_request, read_document
    try:
        validate_policy(policy)
        require(policy['deployments'].get(binding['host']) == entry and entry['root'] == str(root),
                'deployment_missing')
        require(any(row['old'] == binding['old'] and row['new'] == binding['new']
                    for row in policy['migration_admissions']), 'migration_registry_empty')
        with directory_fd(root / 'state/ags-autodeploy/local-v2', private=True):
            pass
        # Private package contains no executable argv or credentials. It binds
        # this pin/window, while the root-owned policy selects its exact path.
        package, package_sha = read_document(Path(entry['host_package']))
        fields(package, ('schema', 'host', 'host_instance', 'deployment_id', 'transaction_id',
                         'authorization_id', 'pin_sha256', 'dsn', 'schema_sha256', 'recovery_group',
                         'adapter_module', 'mini_handoff'))
        require(package['schema'] == 'ags.private-host-package.v1', 'package_schema_invalid')
        for key in ('host_instance', 'deployment_id', 'transaction_id', 'authorization_id'):
            identifier(package[key])
        for key in ('host', 'deployment_id', 'authorization_id', 'pin_sha256'):
            require(package[key] == binding[key], 'package_binding_mismatch')
        digest(package['schema_sha256']); digest(package['pin_sha256'])
        group = package['recovery_group']
        validate_group(group)
        require(group_object(group, 'sqlite')['source'] == str(sqlite_path(package['dsn']))
                and group_object(group, 'old_binary')['source'] == str(root / 'bin/gh-server')
                and group_object(group, 'installation')['source'] == str(root / 'state/installation.json'),
                'installation_mapping_mismatch')
        target = 'darwin_arm64' if binding['host'] == 'mini' else 'linux_amd64'
        candidate = pin['targets'][target]
        backup = {key: binding[key] for key in ('deployment_id', 'attempt_id', 'authorization_id',
                    'old', 'new', 'runtime_tree', 'release_id', 'pin_sha256', 'policy_sha256')}
        backup.update(host_instance=package['host_instance'], transaction_id=package['transaction_id'],
                      tool_sha256=binding['engine_sha256'], release_tag=pin['version'], target=target,
                      control_revision=candidate['provenance']['control_revision'],
                      control_tree=candidate['provenance']['control_tree'],
                      asset_sha256=candidate['asset']['digest'][7:],
                      old_binary_sha256=file_digest(root / 'bin/gh-server'),
                      new_binary_sha256=candidate['binaries']['gh-server']['sha256'])
        # A pending attempt retains the original old digest after replacement.
        pending = root / 'state/ags-autodeploy/local-v2/pending'
        if pending.exists():
            if not (pending / 'cold-journal.json').exists():
                raise Failure('transaction_state_unknown', 'resume', 82)
            cold, _ = private_json(pending / 'cold-journal.json')
            original = cold['backup_binding']
            backup['old_binary_sha256'] = original['old_binary_sha256']
            require(backup == original, 'package_binding_mismatch')
        validate_binding(backup); validate_request(binding, backup); check_pin(pin, binding['pin_sha256'], backup)
        # Verify the retained on-disk authorization, not only an in-memory grant.
        stored, _ = read_document(root / 'config' / ('upgrade-authorization-' + binding['authorization_id'] + '.json'))
        require(stored == authorization and stored['evidence_sha256'] == package_sha,
                'window_authorization_changed')
        authorize(stored, binding)
        require(stored['actor']['type'] == 'human' and stored['authorization_id'] == binding['authorization_id'],
                'window_authorization_invalid')
        handoff = package['mini_handoff']
        if binding['host'] == 'imile-win':
            from upgrade_manual_approval import validate_handoff, release_key
            validate_handoff(handoff, release_key(pin), binding['pin_sha256'], datetime.now(timezone.utc))
        else:
            require(handoff is None, 'unexpected_handoff')
        if not pending.exists():
            require(not (root / 'state/upgrade-cold.json').exists()
                    and not (root / 'state/upgrade-cold.json').is_symlink(), 'cold_claim_held')
            inspect_group(group, package['dsn'], package['schema_sha256'])
            with directory_fd(group['storage']['backup_root'], private=True), \
                    directory_fd(group['storage']['restore_root'], private=True):
                preflight_space(group, assert_private_destination(group['storage']['backup_root']),
                                assert_private_destination(group['storage']['restore_root']))
        name = package['adapter_module']
        require(type(name) is str and re.fullmatch(r'upgrade_adapter_[a-z0-9_]+\.py', name),
                'adapter_module_invalid')
        manifest = strict_json((bundle / 'manifest.json').read_bytes())
        path = bundle / name
        require(name in manifest['files'] and file_digest(path) == manifest['files'][name],
                'adapter_not_pinned')
        require(any(obj['role'] == 'tool' and obj['source'] == str(path) for obj in group['objects']),
                'adapter_not_in_recovery_group')
        spec = importlib.util.spec_from_file_location('ags_private_upgrade_adapter', path)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        require(getattr(module, 'SCHEMA', None) == 'ags.upgrade-host-adapter.v1', 'adapter_schema_invalid')
        capabilities = module.create(root, package)
        fields(capabilities, ('manager', 'writer_probe', 'trash'))
        manager = capabilities['manager']
        require(all(callable(getattr(manager, name, None)) for name in ('stop', 'start', 'suppress_restart'))
                and callable(capabilities['writer_probe']) and callable(capabilities['trash']), 'adapter_incomplete')
        result = Admission(root, binding, pin, backup, group, package['dsn'], package['schema_sha256'],
                           stored, manager, capabilities['writer_probe'], capabilities['trash'], handoff,
                           Path(entry['host_package']), package_sha)
        result.validate(binding['host'], binding)
        return result
    except ImportError:
        raise Failure('dependency_unavailable', 'admission', 69) from None
    except (ContractError, OSError, KeyError, TypeError, ValueError, AttributeError) as error:
        # No payload/exception text escapes. Existing explicit identity/approval
        # and bundle failures retain their separate 64/69/77 semantics.
        failure = Failure('minimum_contract_unsatisfied', 'admission', 83)
        failure.reason = str(error) if isinstance(error, ContractError) else 'private_package_invalid'
        raise failure from None
