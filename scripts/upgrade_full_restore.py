#!/usr/bin/env python3
"""Read-only full-group Tq recovery plan. No copy, rename, install or start API.

S2's sealed payload and complete restore report remain mandatory. Audit state
is never snapshot data. Root tools require a separately approved provisioner.
"""
from pathlib import Path

from upgrade_backup_contract import require, validate_group, validate_restore_report, strict_json
from upgrade_backup import verify_payload
from upgrade_restore_verify import load_sealed, load_inventory, verify_dependencies
from upgrade_inventory import safe_path


def require_snapshot_target(target):
    path = Path(target)
    # libexec/ags-autodeploy supplies tools, unlike state/ags-autodeploy.
    # Only exempt that component, not audit paths nested below the bundle.
    audit_component = any(part in ('accepted', 'pending') or
                          (part == 'ags-autodeploy' and
                           (index == 0 or path.parts[index - 1] != 'libexec'))
                          for index, part in enumerate(path.parts))
    require(not audit_component
            and path.name not in ('upgrade-cold.json', 'runtime-authority.json',
                'runtime.json', 'runtime.lock', 'install.lock', 'hostops.lock',
                'claim.json', 'journal.json', 'cold-journal.json', 'completion.json',
                'authority.json', 'recovery-journal.json')
            and not path.name.startswith(('upgrade-authorization-', 'recovery-approval-')),
            'audit_restore_forbidden')


def plan(request):
    group = request['group']
    objects = validate_group(group)
    for obj in objects.values():
        require_snapshot_target(obj['restore_target'])
    sealed, manifest, receipt, _ = load_sealed(request['backup_dir'], group)
    # S1 authenticates each target and its mapping within a directory member.
    # Reject the whole group, never filter entries out of a complete snapshot.
    for entry in manifest['entries']:
        require_snapshot_target(entry['restore_target'])
    load_inventory(sealed, manifest, group, request['schema_sha256'], request['dsn'])
    verify_payload(sealed, manifest['entries'], auxiliary={'manifest.json', 'receipt.json',
        'inventory.json', 'recovery-group.json', 'restore-report.json', 'publication.json',
        'restore-publication.json'})
    report_path = sealed / 'restore-report.json'
    require(report_path.is_file(), 'restore_report_missing')
    report = validate_restore_report(safe_path(report_path, 'file').read_bytes(), receipt)
    publication = strict_json(safe_path(sealed / 'restore-publication.json', 'file').read_bytes())
    require(publication == {'state': 'published', 'manifest_sha256': receipt['manifest_sha256']},
            'restore_outcome_unknown')
    verify_dependencies(sealed, group, request['dsn'])
    return {'schema': 'ags.full-restore-plan.v1', 'kind': 'full-restore',
            'snapshot_id': manifest['snapshot_id'], 'manifest_sha256': receipt['manifest_sha256'],
            'report_sha256': report['report_sha256'],
            'members': [{'id': key, 'role': obj['role'], 'target': obj['restore_target'],
                         'provision_separately': obj['role'] in ('tool', 'launcher', 'manager', 'policy')}
                        for key, obj in sorted(objects.items())],
            'effects': [], 'started': False, 'live_switch_supported': False,
            'required_approval': 'data-loss-and-external-reconciliation',
            'requirements': ['preserve-failed-group-including-wal-shm', 'close-all-writers',
                'independent-staging-and-s2-revalidation', 'journaled-whole-group-switch',
                'exclude-audit-state', 'independent-old-start', 'incident-acceptance']}
