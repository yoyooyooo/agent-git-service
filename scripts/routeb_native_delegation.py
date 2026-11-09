#!/usr/bin/env python3
"""Narrow an existing human scope grant to a reviewed isolated native action.

The grant records original instructions; the agent is the actual reviewer of
the later bytes. Neither document is a signature. Admission depends on the
live registered root parent and its separately provisioned immutable grant.
Production and direct Python entry do not accept this contract.
"""
from datetime import datetime, timezone
import hashlib
import os
from pathlib import Path

from upgrade_backup_contract import (ContractError, digest, fields, fingerprint,
                                    identifier, require, strict_json, text, timestamp)
from upgrade_identity import read_trusted


REGISTRY = Path('/usr/local/libexec/ags-routeb-native')
GRANT_SCHEMA = 'ags.routeb-native-scope-grant.v1'
SCHEMAS = {
    'native-provision': 'ags.routeb-native-provision-approval.v2',
    'immutable-provision': 'ags.immutable-authority-approval.v2',
    'native-bootstrap': 'ags.routeb-native-bootstrap-approval.v2',
    'host-window': 'ags.routeb-window.v3',
    'full-restore': 'ags.routeb-recovery-approval.v2',
    'old-start': 'ags.routeb-recovery-approval.v2',
}
OPERATIONS = {
    'native-provision': ('bootstrap-plan', 'bootstrap'),
    'immutable-provision': ('probe', 'bootstrap-plan', 'bootstrap', 'plan', 'transaction',
                            'serve', 'doctor', 'restore', 'old-start'),
    'native-bootstrap': ('bootstrap-plan', 'bootstrap'),
    'host-window': ('plan', 'transaction', 'restore', 'old-start'),
    'full-restore': ('restore',),
    'old-start': ('old-start', 'serve'),
}
SUBJECT_FIELDS = {
    'native-bootstrap': ('root', 'host_instance', 'transaction_id', 'provision_sha256',
                         'identity_sha256', 'epoch'),
    'immutable-provision': ('host_instance', 'tool_manifest_sha256', 'installer_sha256', 'objects'),
    'full-restore': ('approval_id', 'purpose', 'host_instance', 'snapshot_id', 'manifest_sha256',
                     'data_loss_acknowledged', 'external_reconciliation_acknowledged'),
    'old-start': ('approval_id', 'purpose', 'host_instance', 'snapshot_id', 'manifest_sha256',
                  'data_loss_acknowledged', 'external_reconciliation_acknowledged'),
}


def is_delegated(approval, purpose):
    return type(approval) is dict and purpose in SCHEMAS and approval.get('schema') == SCHEMAS[purpose]


def subject_sha256(approval, purpose):
    """The exact subject excludes its own review to avoid a hash cycle."""
    require(purpose in SCHEMAS and type(approval) is dict, 'delegation_purpose_invalid')
    try:
        if purpose == 'host-window':
            result = approval['package_sha256']
        elif purpose == 'native-provision':
            result = approval['binding_sha256']
        else:
            result = fingerprint({key: approval[key] for key in SUBJECT_FIELDS[purpose]})
        digest(result)
        require(result != '0' * 64, 'delegation_subject_invalid')
        return result
    except KeyError:
        raise ContractError('delegation_subject_invalid') from None


def _context(root, purpose):
    from routeb_stage0_client import context
    return context(root, operations=OPERATIONS[purpose])


def _protected(path):
    raw = read_trusted(path, maximum=256 * 1024)
    require(os.getuid() != 0 and all(not os.access(part, os.W_OK) for part in (path, *path.parents)),
            'delegation_grant_runtime_writable')
    return raw


def _sha(raw):
    return hashlib.sha256(raw).hexdigest()


def require_delegation(approval, purpose, *, root=None, host_instance=None):
    require(is_delegated(approval, purpose), 'delegation_schema_invalid')
    actor = approval.get('actor')
    require(type(actor) is str and actor.startswith('agent:') and len(actor) > 6,
            'delegation_actor_invalid')
    value = approval.get('delegation')
    fields(value, ('scope_grant_sha256', 'review'))
    digest(value['scope_grant_sha256'])
    require(value['scope_grant_sha256'] != '0' * 64, 'delegation_grant_invalid')
    review = value['review']
    fields(review, ('actor', 'subject_sha256', 'reviewed_at', 'evidence_refs'))
    require(review['actor'] == actor and review['subject_sha256'] == subject_sha256(approval, purpose),
            'delegation_review_binding')
    reviewed = timestamp(review['reviewed_at'])
    require(reviewed <= timestamp(approval['issued_at']) < timestamp(approval['expires_at']),
            'delegation_review_time')
    refs = review['evidence_refs']
    require(type(refs) is list and 1 <= len(refs) <= 32, 'delegation_evidence_invalid')
    seen = set()
    for ref in refs:
        fields(ref, ('kind', 'sha256')); identifier(ref['kind']); digest(ref['sha256'])
        key = (ref['kind'], ref['sha256'])
        require(ref['sha256'] != '0' * 64 and key not in seen, 'delegation_evidence_invalid')
        seen.add(key)
    context = _context(root, purpose)
    require(context['scope_grant_sha256'] == value['scope_grant_sha256']
            and context['operation'] in OPERATIONS[purpose], 'delegation_grant_binding')
    if purpose == 'old-start' and context['operation'] == 'serve':
        require(context['phase'] == 'serve-old', 'delegation_purpose_invalid')
    transaction = context['registration_id']; identifier(transaction)
    registry = REGISTRY / transaction
    registration_raw = _protected(registry / 'stage0-registration.json')
    require(_sha(registration_raw) == context['registration_sha256'], 'delegation_registration_changed')
    registration = strict_json(registration_raw)
    require(registration.get('scope') == 'isolated-native' and registration.get('id') == transaction,
            'delegation_scope_invalid')
    reference = registration.get('scope_grant')
    fields(reference, ('path', 'sha256', 'size'))
    grant_path = registry / 'scope-grant.json'
    require(reference['path'] == str(grant_path) and reference['sha256'] == value['scope_grant_sha256'],
            'delegation_grant_binding')
    raw = _protected(grant_path)
    require(_sha(raw) == reference['sha256'] and type(reference['size']) is int
            and len(raw) == reference['size'], 'delegation_grant_changed')
    grant = strict_json(raw)
    fields(grant, ('schema', 'grant_id', 'grantor', 'delegate', 'source', 'scope', 'host',
                   'host_instance', 'transaction_id', 'root', 'purposes', 'recorded_at'))
    identifier(grant['grant_id'])
    require(grant['schema'] == GRANT_SCHEMA and grant['scope'] == 'isolated-native'
            and type(grant['grantor']) is str and grant['grantor'].startswith('human:')
            and len(grant['grantor']) > 6 and grant['delegate'] == actor
            and grant['transaction_id'] == transaction and grant['root'] == context['root']
            and (root is None or str(root) == grant['root'])
            and grant['host_instance'] == registration['binding']['host_instance']
            and (host_instance is None or grant['host_instance'] == host_instance), 'delegation_scope_invalid')
    profile_raw = _protected(registry / 'host-profile.json')
    require(_sha(profile_raw) == registration['host_profile']['sha256'], 'delegation_host_changed')
    profile = strict_json(profile_raw)
    require(profile['scope'] == 'isolated-native' and profile['transaction_id'] == transaction
            and profile['root'] == grant['root'] and profile['logical_host'] == grant['host']
            and profile['host_instance'] == grant['host_instance']
            and grant['host'] in ('mini', 'imile-win'), 'delegation_scope_invalid')
    require(all(key not in approval or approval[key] == grant[key]
                for key in ('host', 'host_instance', 'transaction_id', 'root')),
            'delegation_scope_invalid')
    purposes = grant['purposes']
    require(type(purposes) is list and 0 < len(purposes) <= len(SCHEMAS)
            and all(type(item) is str and item in SCHEMAS for item in purposes)
            and len(set(purposes)) == len(purposes) and purpose in purposes, 'delegation_purpose_invalid')
    recorded = timestamp(grant['recorded_at'])
    require(recorded <= reviewed <= datetime.now(timezone.utc), 'delegation_review_time')
    source = grant['source']
    fields(source, ('kind', 'captured_by', 'messages'))
    require(source['kind'] == 'conversation' and source['captured_by'] == actor
            and type(source['messages']) is list and 1 <= len(source['messages']) <= 8,
            'delegation_source_invalid')
    for message in source['messages']:
        fields(message, ('text', 'sent_at')); text(message['text'])
        require(message['sent_at'] is None or timestamp(message['sent_at']) <= recorded,
                'delegation_source_invalid')
    return context
