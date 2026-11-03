#!/usr/bin/env python3
"""Route B host admission for an exact-source package. No install and no manager call.

Release admission stays in upgrade_wireup. This module does not invent a
release_id. A package hash, transaction and host must match the approval and
the already verified candidate.
"""
import sys
sys.dont_write_bytecode = True
sys.pycache_prefix = None
import hashlib
import json
from dataclasses import dataclass
from datetime import datetime, timezone

from upgrade_backup_contract import ContractError, digest, fields, identifier, require, text
from upgrade_wireup_safety import object_with_unique_keys, package_digest, require_unexpired


SCHEMA = 'ags.routeb-host-package.v1'
APPROVAL_SCHEMA = 'ags.routeb-window.v2'
PACKAGE_FIELDS = ('schema', 'host', 'host_instance', 'deployment_id', 'transaction_id',
                  'attempt_id', 'authorization_id', 'candidate_manifest_sha256',
                  'candidate_revision', 'candidate_tree', 'candidate_transaction_id',
                  'tool_sha256', 'policy_sha256', 'recovery_group_sha256', 'native_profile_sha256')
APPROVAL_FIELDS = ('schema', 'authorization_id', 'actor', 'issued_at', 'expires_at',
                   'decision', 'package_sha256', 'host', 'transaction_id',
                   'start_restores_business', 'actions')


class AdmissionError(ContractError):
    def __init__(self, reason):
        super().__init__(reason)
        self.code = reason


def admit_raw(raw, approval, verified, *, now):
    try:
        package = object_with_unique_keys(raw)
    except ContractError as error:
        raise AdmissionError(str(error)) from None
    return admit(package, approval, verified, now=now, raw=raw)


def admit(package, approval, verified, *, now, raw=None):
    try:
        fields(package, PACKAGE_FIELDS)
        require(package['schema'] == SCHEMA, 'schema_invalid')
        identifier(package['host']); identifier(package['host_instance'])
        for key in ('deployment_id', 'transaction_id', 'attempt_id', 'authorization_id'):
            identifier(package[key])
        for key in ('candidate_manifest_sha256', 'tool_sha256', 'policy_sha256',
                    'recovery_group_sha256', 'native_profile_sha256'):
            digest(package[key])
        text(package['candidate_revision']); text(package['candidate_tree'])
        fields(approval, APPROVAL_FIELDS)
        require(approval['schema'] == APPROVAL_SCHEMA and approval['decision'] == 'approve',
                'approval_invalid')
        require(approval['actor'].startswith('human:'), 'actor_not_human')
        require(approval['authorization_id'] == package['authorization_id'], 'approval_mismatch')
        require(approval['host'] == package['host'], 'host_mismatch')
        require(approval['transaction_id'] == package['transaction_id'], 'transaction_mismatch')
        require(tuple(approval['actions']) == ('stop', 'install', 'start'), 'actions_invalid')
        require(approval['start_restores_business'] is True, 'start_not_acknowledged')
        encoded = raw if raw is not None else json.dumps(package, sort_keys=True).encode()
        digest_value = package_digest(encoded)
        require(approval['package_sha256'] == digest_value, 'package_replaced')
        require_unexpired(approval['issued_at'], approval['expires_at'], now)
        require(package['host'] == verified.host, 'host_mismatch')
        require(package['candidate_revision'] == verified.revision
                and package['candidate_tree'] == verified.tree
                and package['candidate_manifest_sha256'] == verified.raw_manifest_sha256,
                'candidate_mismatch')
        require(package['candidate_transaction_id'] == package['transaction_id'] == approval['transaction_id'],
                'transaction_mismatch')
        return HostAdmission(kind='exact-source', package_sha256=digest_value,
                             transaction_id=package['transaction_id'], host=package['host'],
                             authorization_id=package['authorization_id'])
    except AdmissionError:
        raise
    except ContractError as error:
        raise AdmissionError(str(error)) from None


@dataclass(frozen=True)
class HostAdmission:
    kind: str
    package_sha256: str
    transaction_id: str
    host: str
    authorization_id: str
