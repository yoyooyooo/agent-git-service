#!/usr/bin/env python3
"""S3 pre-start byte/receipt recovery, fresh approval and interrupted installs."""
from datetime import datetime, timedelta, timezone
import copy
import hashlib
import json
import os
from pathlib import Path
import unittest
from unittest.mock import patch

import upgrade_engine as engine
import upgrade_host as host
import upgrade_recovery as cold
import upgrade_backup_contract as c
import test_upgrade_quiescence as fixtures


class RecoveryTests(fixtures.ColdFixture):
    def approval(self, **changes):
        now = datetime.now(timezone.utc)
        value = {'schema': 'ags.prestart-recovery-approval.v1', 'approval_id': 'fresh-recovery',
                 'purpose': 'prestart-recover', 'decision': 'approve', 'binding': self.request,
                 'generation': self.window.journal['generation'],
                 'journal_sha256': cold.file_digest(self.pending / 'cold-journal.json'),
                 'issued_at': (now - timedelta(seconds=1)).isoformat(),
                 'expires_at': (now + timedelta(minutes=5)).isoformat(),
                 'actor': {'type': 'human', 'id': 'fixture-operator', 'source': 'fixture-approval'}}
        value.update(changes)
        path = self.root / 'approval.json'
        engine.save(path, value)
        return path

    def test_fresh_approval_restores_exact_old_bytes_and_raw_receipt_only(self):
        self.prepare(); self.install()
        before = cold.stable_data(self.group, self.window.dsn, self.window.schema)
        result = cold.recover_prestart(self.window, self.approval())
        self.assertEqual(result['status'], 'old_restored')
        self.assertEqual(self.binary.read_bytes(), self.old)
        self.assertEqual((self.root / 'state/installation.json').read_bytes(), self.raw_old_receipt)
        self.assertEqual(cold.stable_data(self.group, self.window.dsn, self.window.schema), before)
        self.assertEqual(self.manager.starts, 0)
        self.assertFalse(cold.startup_permitted(self.root))
        self.assertEqual(cold.read_original_binding(self.pending), self.request)
        with self.assertRaises(c.ContractError):
            cold.recover_prestart(self.window, self.approval())

    def test_before_install_unchanged_old_program_can_be_accepted_only_with_fresh_approval(self):
        for phase in ('quiescent', 'backup', 'restore_verified'):
            with self.subTest(phase=phase):
                self.prepare(until=phase)
                cold.recover_prestart(self.window, self.approval())
                self.assertEqual(self.binary.read_bytes(), self.old)
                self.assertEqual((self.root / 'state/installation.json').read_bytes(), self.raw_old_receipt)
                self.assertFalse(cold.startup_permitted(self.root))
                if phase != 'restore_verified':
                    self.doCleanups(); self.setUp()

    def test_recovery_uses_new_owner_after_original_process_lost_its_locks(self):
        self.prepare(); self.install()
        approval = self.approval()
        self.window.release(); self.window.release_owner()
        recovered = cold.ColdWindow(self.root, self.request, self.b, self.group,
                                    self.window.dsn, self.window.schema, self.manager,
                                    self.writers, self.trash)
        recovered.acquire_owner()
        self.addCleanup(recovered.release_owner)
        recovered.acquire()
        self.addCleanup(recovered.release)
        cold.recover_prestart(recovered, approval)
        self.assertEqual(self.binary.read_bytes(), self.old)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_data_config_git_or_cas_changed_refuses_without_old_replacement(self):
        self.prepare(); self.install()
        path = self.approval()
        for name in ('runtime_env', 'manager'):
            with self.subTest(name=name):
                source = self.root / name
                raw = source.read_bytes()
                source.write_bytes(raw + b'changed')
                with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, path)
                self.assertEqual(self.binary.read_bytes(), self.new)
                source.write_bytes(raw)
        self.storage.insert('labels', id=123)
        with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, path)
        self.assertEqual(self.binary.read_bytes(), self.new)

    def test_git_and_cas_changes_refuse(self):
        self.prepare(); self.install()
        path = self.approval()
        config = self.storage.store / 'sample/project.git/config'
        config.write_bytes(config.read_bytes() + b'\n# fixture change\n')
        with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, path)
        self.assertEqual(self.binary.read_bytes(), self.new)
        self.storage.cas(b'new fixture CAS')
        with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, path)
        self.assertEqual(self.manager.starts, 0)

    def test_no_approval_old_approval_stale_expired_or_wrong_binding_refuses(self):
        self.prepare(); self.install()
        with self.assertRaises((c.ContractError, OSError, engine.Failure)):
            cold.recover_prestart(self.window, self.root / 'missing')
        for change in ({'approval_id': 'window-auth'}, {'decision': 'pending'},
                       {'journal_sha256': '0' * 64}, {'generation': 'wrong-generation'},
                       {'binding': {**self.request, 'new': 'c' * 40}},
                       {'expires_at': '2000-01-01T00:00:00Z'},
                       {'actor': {'type': 'agent', 'id': 'fixture', 'source': 'fixture'}}):
            with self.subTest(change=change), self.assertRaises(c.ContractError):
                cold.recover_prestart(self.window, self.approval(**change))
        self.assertEqual(self.binary.read_bytes(), self.new)
        self.assertEqual(self.manager.starts, 0)

    def test_start_attempted_refuses_even_before_any_manager_or_spawn(self):
        self.prepare(); self.install()
        self.window.start_intent()
        with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, self.approval())
        self.assertEqual(self.manager.starts, 0)
        self.assertEqual(self.binary.read_bytes(), self.new)

    def test_unknown_phase_generation_or_missing_journal_refuses(self):
        self.prepare(); self.install()
        original = copy.deepcopy(self.window.journal)
        for change in ({'phase': 'future'}, {'generation': 'wrong'}, {'start_attempted': None}):
            with self.subTest(change=change):
                engine.save(self.pending / 'cold-journal.json', {**original, **change})
                with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, self.approval())
                self.assertEqual(self.binary.read_bytes(), self.new)
        (self.pending / 'cold-journal.json').rename(self.pending / 'retained-journal')
        with self.assertRaises((c.ContractError, OSError, engine.Failure)):
            cold.recover_prestart(self.window, self.root / 'missing')

    def test_install_byte_and_receipt_breakpoints_keep_marker_and_allow_approved_recovery(self):
        steps = ('candidate-file-sync', 'installing.json:file-sync', 'installing.json:rename',
                 'installing.json:directory-sync', 'installation.json:file-sync',
                 'installation.json:rename', 'installation.json:directory-sync',
                 'installing-durable', 'binary-renamed',
                 'binary-directory-sync', 'receipt-durable', 'install-marker-cleared')
        for step in steps:
            with self.subTest(step=step):
                self.prepare()
                adapter = object.__new__(host.Effects)
                adapter.root, adapter.binding, adapter._cold = self.root, self.request, self.window
                adapter.candidate = self.root / 'candidate'
                adapter.candidate.write_bytes(self.new)
                adapter.info = {'source': self.b['new'], 'runtime_tree': self.b['runtime_tree'],
                                'control_revision': self.b['control_revision'], 'version': 'fixture',
                                'release_id': 1, 'attestation': {}, 'binaries': {
                                    'gh-server': {'sha256': self.b['new_binary_sha256'], 'identity': self.identity}}}
                pid = os.fork()
                if pid == 0:
                    try:
                        def fail(name):
                            if name == step: os._exit(91)
                        with patch.object(host, 'cold_checkpoint', side_effect=fail):
                            adapter.install()
                        os._exit(92)
                    except BaseException:
                        os._exit(93)
                _, status = os.waitpid(pid, 0)
                self.assertEqual(os.waitstatus_to_exitcode(status), 91)
                self.assertFalse(cold.startup_permitted(self.root))
                cold.recover_prestart(self.window, self.approval())
                self.assertEqual(self.binary.read_bytes(), self.old)
                self.assertEqual((self.root / 'state/installation.json').read_bytes(), self.raw_old_receipt)
                self.assertEqual(self.manager.starts, 0)
                if step != steps[-1]:
                    self.doCleanups(); self.setUp()

    def test_recovery_itself_crashes_retains_consumed_approval_and_refuses_replay(self):
        self.prepare(); self.install()
        approval = self.approval()
        def fail(name):
            if name == 'recovery-binary': raise RuntimeError('fixture crash')
        with patch.object(cold, 'checkpoint', side_effect=fail), self.assertRaises(RuntimeError):
            cold.recover_prestart(self.window, approval)
        self.assertTrue((self.pending / 'recovery-fresh-recovery.json').exists())
        with self.assertRaises(c.ContractError): cold.recover_prestart(self.window, approval)
        self.assertFalse(cold.startup_permitted(self.root))

    def test_restore_trash_none_is_rejected_before_copy(self):
        self.prepare(until='backup')
        import upgrade_restore_verify as restore
        with self.assertRaisesRegex(c.ContractError, 'trash_unavailable'):
            restore.verify_sealed_backup(self.window.journal['backup_dir'], trash=None,
                                         group=self.group, dsn=self.window.dsn, schema=self.window.schema)
        self.assertEqual(list(self.storage.restores.iterdir()), [])


if __name__ == '__main__':
    unittest.main()
