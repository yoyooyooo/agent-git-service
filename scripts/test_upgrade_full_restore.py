#!/usr/bin/env python3
"""Full-group offline plans; real S1/S2 SQLite/Git, no live switch capability."""
import copy
import json
from pathlib import Path
import unittest
from unittest.mock import Mock

import upgrade_backup_contract as c
import upgrade_recovery_cli as cli
from test_upgrade_backup import BackupTests
import upgrade_restore_verify as restore
from test_upgrade_wireup import snapshot


class FullRestoreTests(BackupTests):
    def prepare(self):
        self.repo()
        body = b'full restore CAS fixture' * 200
        self.cas(body)
        self.sealed = self.take()
        restore.verify_sealed_backup(self.sealed['backup_dir'], group=self.last_group,
            dsn='sqlite:' + str(self.dbpath), schema=self.admission, trash=lambda _: None)
        self.request = {'kind': 'full-restore', 'start_attempted': True,
                        'backup_dir': str(self.sealed['backup_dir']), 'group': self.last_group,
                        'dsn': 'sqlite:' + str(self.dbpath), 'schema_sha256': self.admission}
        self.ledger = Mock()
        self.before = snapshot(self.root)
        self.addCleanup(lambda: self.assertEqual(self.ledger.mock_calls, []))
        self.addCleanup(lambda: self.assertEqual(snapshot(self.root), self.before))

    def test_poststart_plan_covers_entire_sealed_group_without_effect(self):
        self.prepare()
        result = cli.plan(self.request)
        self.assertEqual(result['kind'], 'full-restore')
        self.assertEqual(result['effects'], [])
        self.assertFalse(result['live_switch_supported'])
        self.assertFalse(result['started'])
        self.assertEqual({x['id'] for x in result['members']},
                         {x['id'] for x in self.last_group['objects']})
        self.assertEqual(result['required_approval'], 'data-loss-and-external-reconciliation')

    def test_missing_member_and_audit_member_refuse(self):
        self.prepare()
        group = copy.deepcopy(self.last_group)
        group['objects'] = [x for x in group['objects'] if x['role'] != 'tool']
        with self.assertRaisesRegex(c.ContractError, 'required_object_missing'):
            cli.plan({**self.request, 'group': group})
        group = copy.deepcopy(self.last_group)
        group['objects'].append({'id': 'claim', 'role': 'config', 'kind': 'file',
            'source': str(self.root / 'state/ags-autodeploy/local-v2/pending/journal.json'),
            'restore_target': str(self.root / 'state/ags-autodeploy/local-v2/pending/journal.json'),
            'depends_on': []})
        with self.assertRaisesRegex(c.ContractError, 'audit_restore_forbidden'):
            cli.plan({**self.request, 'group': group})
        for name in ('claim.json', 'journal.json', 'authority.json', 'completion.json'):
            group['objects'][-1].update(source=str(self.root / name), restore_target=str(self.root / name))
            with self.subTest(name=name), self.assertRaisesRegex(c.ContractError, 'audit_restore_forbidden'):
                cli.plan({**self.request, 'group': group})

    def test_libexec_tool_stays_in_the_complete_plan_for_separate_provision(self):
        self.repo()
        group = self.prepared_group()
        bundle = self.root / 'libexec/ags-autodeploy'
        bundle.mkdir(parents=True, mode=0o700)
        tool = bundle / 'upgrade.py'
        tool.write_bytes(b'synthetic tool bytes')
        member = next(obj for obj in group['objects'] if obj['role'] == 'tool')
        member.update(source=str(bundle), restore_target=str(bundle), kind='directory')
        sealed = self.take(group)
        restore.verify_sealed_backup(sealed['backup_dir'], group=group,
            dsn='sqlite:' + str(self.dbpath), schema=self.admission, trash=lambda _: None)
        request = {'kind': 'full-restore', 'start_attempted': True,
            'backup_dir': str(sealed['backup_dir']), 'group': group,
            'dsn': 'sqlite:' + str(self.dbpath), 'schema_sha256': self.admission}
        before = snapshot(self.root)
        try:
            result = cli.plan(request)
            self.assertEqual({item['id'] for item in result['members']},
                             {obj['id'] for obj in group['objects']})
            selected = next(item for item in result['members'] if item['id'] == member['id'])
            self.assertEqual(selected['role'], 'tool')
            self.assertEqual(selected['target'], str(bundle))
            self.assertIs(selected['provision_separately'], True)
            self.assertEqual(result['effects'], [])
        finally:
            self.assertEqual(snapshot(self.root), before)

    def test_nested_audit_members_reject_the_entire_sealed_group(self):
        self.repo()
        for name in ('claim.json', 'journal.json', 'completion.json', 'authority.json'):
            with self.subTest(name=name):
                group = self.prepared_group()
                external = self.root / ('external-' + name)
                audit = external / 'state/ags-autodeploy/local-v2/accepted' / name
                audit.parent.mkdir(parents=True, mode=0o700)
                audit.write_text('{"synthetic_audit":true}')
                group['objects'].append({'id': 'external', 'role': 'external_authority',
                    'kind': 'directory', 'source': str(external),
                    'restore_target': str(external), 'depends_on': []})
                sealed = self.take(group, snapshot_id='nested-' + name)
                restore.verify_sealed_backup(sealed['backup_dir'], group=group,
                    dsn='sqlite:' + str(self.dbpath), schema=self.admission, trash=lambda _: None)
                manifest = json.loads((Path(sealed['backup_dir']) / 'manifest.json').read_bytes())
                self.assertIn(str(audit), [item['restore_target'] for item in manifest['entries']])
                request = {'kind': 'full-restore', 'start_attempted': True,
                    'backup_dir': str(sealed['backup_dir']), 'group': group,
                    'dsn': 'sqlite:' + str(self.dbpath), 'schema_sha256': self.admission}
                before = snapshot(self.root)
                try:
                    with self.assertRaisesRegex(c.ContractError, 'audit_restore_forbidden'):
                        cli.plan(request)
                finally:
                    self.assertEqual(snapshot(self.root), before)

    def test_changed_payload_and_missing_restore_report_refuse(self):
        self.prepare()
        payload = Path(self.sealed['backup_dir']) / 'tool'
        raw = payload.read_bytes()
        metadata = payload.stat()
        payload.write_bytes(b'changed')
        self.before = snapshot(self.root)
        with self.assertRaises(c.ContractError):
            cli.plan(self.request)
        payload.write_bytes(raw)
        import os
        os.utime(payload, ns=(metadata.st_atime_ns, metadata.st_mtime_ns))
        report = Path(self.sealed['backup_dir']) / 'restore-report.json'
        report.rename(self.root / 'retained-report.json')
        self.before = snapshot(self.root)
        with self.assertRaisesRegex(c.ContractError, 'restore_report_missing'):
            cli.plan(self.request)


if __name__ == '__main__':
    unittest.main()
