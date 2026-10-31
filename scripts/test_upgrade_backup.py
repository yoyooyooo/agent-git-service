#!/usr/bin/env python3
"""Synthetic cold-backup failures. No production data, AGS process or host call."""
from contextlib import contextmanager
import copy
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import tempfile
import unittest

import upgrade_backup as backup
import upgrade_backup_contract as c
import upgrade_inventory as inv
import upgrade
import upgrade_engine as engine
import upgrade_host as host
import release_install_v2 as install_v2
from test_upgrade import authorization
from test_upgrade_inventory import InventoryTests

ROOT = Path(__file__).resolve().parents[1]


def binding():
    value = {key: 'e' * 64 if key.endswith('_sha256') else 'fixture' for key in c.BINDING_FIELDS}
    value.update({key: 'f' * 40 for key in ('old', 'new', 'runtime_tree', 'control_revision', 'control_tree')})
    value.update(release_id=1, target='darwin_arm64')
    return value


def quiescence():
    return {'nonce': 'nonce1', 'stop_evidence_sha256': 'a' * 64, 'writers_evidence_sha256': 'b' * 64}


class BackupTests(InventoryTests):
    def setUp(self):
        super().setUp()
        self.backups = self.root / 'backups'
        self.restores = self.root / 'restores'
        self.backups.mkdir(mode=0o700)
        self.restores.mkdir(mode=0o700)
        os.chmod(self.backups, 0o700)
        os.chmod(self.restores, 0o700)

    def prepared_group(self, *, bytes_available=None):
        group = self.group()
        group['storage'] = {'backup_root': str(self.backups), 'restore_root': str(self.restores),
                            'private_policy_sha256': 'f' * 64, 'copy_method': 'ordinary_copy'}
        if bytes_available is not None:
            group['capacity']['available_bytes'] = bytes_available
        return group

    def take(self, group=None, **kwargs):
        group = group or self.prepared_group()
        return backup.create_sealed_backup(group, 'sqlite:' + str(self.dbpath), self.admission,
                                           binding(), quiescence(), 'snapshot1', **kwargs)

    def test_empty_database_and_optional_wiki_seal_without_hardlinks(self):
        repo = self.repo()
        commit = self.commit(repo)
        wiki = self.store / 'sample/project.wiki.git'
        self.init_repo(wiki)
        (repo / 'hooks').mkdir(exist_ok=True)
        (repo / 'hooks' / 'post-receive').write_bytes(b'#!/bin/sh\nexit 0\n')
        (repo / 'refs' / 'heads').mkdir(parents=True, exist_ok=True)
        (repo / 'refs' / 'heads' / 'main').write_text(commit + '\n')
        body = b'historical ' * 300
        self.cas(body)
        result = self.take()
        self.assertEqual(result['receipt']['schema'], 'ags.local-backup.v2')
        self.assertEqual(result['receipt']['state'], 'sealed')
        payload = Path(result['backup_dir'])
        self.assertTrue(payload.is_dir())
        self.assertEqual(payload.stat().st_mode & 0o777, 0o700)
        copied = payload / 'git_root' / 'sample' / 'project.git' / 'objects'
        self.assertTrue(copied.exists())
        source_object = next((self.store / 'sample/project.git/objects').rglob('*'))
        copied_object = payload / 'git_root' / source_object.relative_to(self.store)
        if source_object.is_file() and copied_object.is_file():
            self.assertNotEqual(source_object.stat().st_ino, copied_object.stat().st_ino)
        self.assertTrue((payload / 'git_root' / '.wikiblobs').exists())
        self.assertFalse((payload / 'manifest.json').stat().st_mode & 0o077)
        listed = backup.list_backups(self.backups)
        self.assertEqual(listed['verified'], [])
        self.assertEqual([item['snapshot_id'] for item in listed['sealed_unverified']], ['snapshot1'])

    def test_source_change_during_copy_invalidates_snapshot(self):
        self.repo()
        original = backup.copy_tree

        def mutate(source, destination, recorder):
            original(source, destination, recorder)
            config = self.store / 'sample' / 'project.git' / 'config'
            config.write_bytes(config.read_bytes() + b'\n# changed during copy\n')

        with unittest.mock.patch.object(backup, 'copy_tree', mutate):
            with self.assertRaisesRegex(c.ContractError, 'source_changed'):
                self.take()
        self.assertEqual(list(self.backups.iterdir()), [])

    def test_hardlink_and_path_escape_refuse(self):
        self.repo()
        outside = self.root / 'outside-target'
        outside.write_bytes(b'not-in-source')
        linked = self.store / 'sample' / 'escaped'
        os.link(outside, linked)
        with self.assertRaisesRegex(c.ContractError, 'hardlink_forbidden'):
            self.take()
        linked.unlink()
        escape = self.store / 'sample' / 'project.git' / 'escape'
        escape.symlink_to(outside)
        with self.assertRaisesRegex(c.ContractError, 'symlink_forbidden'):
            self.take()

    def test_insufficient_space_refuses_without_partial(self):
        self.repo()
        group = self.prepared_group(bytes_available=1)
        with self.assertRaises(c.ContractError):
            self.take(group)
        self.assertEqual(list(self.backups.iterdir()), [])

    def test_interrupted_steps_leave_no_retained_partial(self):
        self.repo()
        steps = ('scan', 'copy', 'sqlite', 'seal')
        for step in steps:
            with self.subTest(step=step):
                def fail(name):
                    if name == step:
                        raise c.ContractError('interrupted')
                with unittest.mock.patch.object(backup, 'checkpoint', fail):
                    with self.assertRaisesRegex(c.ContractError, 'interrupted'):
                        self.take()
                self.assertEqual(list(self.backups.iterdir()), [])

    def test_apply_backup_and_direct_install_stay_83(self):
        for machine in ('mini', 'imile-win'):
            binding_value = {'host': machine}
            effects = unittest.mock.Mock()
            state = self.root / ('absent-' + machine)
            with self.assertRaises(upgrade.Failure) as caught:
                engine.Transaction(state, binding_value, authorization(binding_value), effects).run()
            self.assertEqual(caught.exception.code, 'backup_contract_unimplemented')
            self.assertEqual(caught.exception.exit_code, 83)
            self.assertFalse(state.exists())
            self.assertEqual(effects.mock_calls, [])
            adapter = object.__new__(host.Effects)
            adapter.binding = binding_value
            with self.assertRaises(upgrade.Failure) as refused:
                adapter.backup()
            self.assertEqual(refused.exception.exit_code, 83)
        with self.assertRaises(install_v2.InstallClosed) as direct:
            install_v2.require_install_contracts()
        self.assertEqual(direct.exception.exit_code, 83)
        self.assertFalse(hasattr(host.Effects, 'create_sealed_backup'))

    def test_retention_trashes_only_older_verified_and_keeps_failures(self):
        calls = []

        def fake_trash(path):
            calls.append(Path(path))
            Path(path).rename(self.root / ('trashed-' + Path(path).name))

        verified = []
        for index in range(6):
            directory = self.backups / ('snap' + str(index))
            directory.mkdir(mode=0o700)
            (directory / 'sealed').write_bytes(b'ok')
            verified.append({'snapshot_id': 'snap' + str(index), 'sealed_at': '2026-01-0' + str(index + 1),
                             'path': directory, 'state': 'restore_verified'})
        failed = self.backups / 'failed-partial'
        failed.mkdir(mode=0o700)
        (failed / 'incomplete').write_bytes(b'keep')
        backup.apply_retention(self.backups, verified, trash=fake_trash)
        self.assertEqual([item.name for item in calls], ['snap0'])
        self.assertTrue(failed.exists())
        self.assertFalse((self.backups / 'snap0').exists())
        self.assertTrue((self.backups / 'snap5').exists())


if __name__ == '__main__':
    unittest.main()
