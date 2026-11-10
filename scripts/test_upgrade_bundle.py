#!/usr/bin/env python3
"""S6.06 bundle builder. Staging fixtures only; nothing is provisioned."""
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock

HERE = Path(__file__).resolve().parent


class BundleBaseline(unittest.TestCase):
    def test_old_three_file_set_is_not_complete(self):
        """Existing required-member contract. Three files are the behavior red."""
        import upgrade
        ledger = Mock()
        with tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-') as directory:
            root = Path(directory).resolve()
            files = {}
            for name in ('upgrade_host.py', 'upgrade.py', 'upgrade_engine.py'):
                path = root / name
                path.write_bytes(b'fixture')
                files[name] = hashlib.sha256(b'fixture').hexdigest()
            raw = upgrade.json.dumps({'schema': 'ags.upgrade-tool-bundle.v2',
                                      'entry': 'upgrade_host.py', 'files': files}).encode()
            (root / 'manifest.json').write_bytes(raw)
            with self.assertRaises(upgrade.Failure) as caught:
                upgrade.bundle_engine(root, hashlib.sha256(raw).hexdigest(), owner=os.getuid())
            self.assertEqual(caught.exception.exit_code, 69)
            self.assertEqual(ledger.mock_calls, [])


class BundleTests(unittest.TestCase):
    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            (self.source / name).write_bytes(name.encode())
        self.effects = []
        self.addCleanup(lambda: self.assertEqual(self.effects, []))

    def test_staged_bundle_verifies_readonly_and_is_not_installable(self):
        staging = self.root / 'staging'
        built = self.bundle.build(self.source, staging)
        verified = self.bundle.verify(staging)
        self.assertEqual(verified['manifest_sha256'], built['manifest_sha256'])
        self.assertFalse(verified['installable'])
        again = self.bundle.build(self.source, self.root / 'staging-2')
        self.assertEqual(again['manifest_sha256'], built['manifest_sha256'])

    def test_missing_extra_tamper_owner_and_symlink_refuse(self):
        staging = self.root / 'staging'
        self.bundle.build(self.source, staging)
        (staging / 'upgrade.py').write_bytes(b'tampered')
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)
        (staging / 'upgrade.py').write_bytes(b'upgrade.py')
        extra = staging / 'extra'
        extra.write_bytes(b'extra')
        extra.chmod(0o755)
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)
        extra.unlink()
        (staging / 'upgrade.py').unlink()
        (staging / 'upgrade.py').symlink_to(self.source / 'upgrade.py')
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)

    def test_incomplete_rename_is_not_executed(self):
        partial = self.root / 'partial'
        partial.mkdir(mode=0o700)
        (partial / 'upgrade.py').write_bytes(b'upgrade.py')
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(partial)
        self.assertFalse(stat.S_IMODE((partial / 'upgrade.py').stat().st_mode) & 0o111)


class _Raw:
    def __init__(self, text):
        self.text = text


def _encode(value):
    if isinstance(value, _Raw):
        return value.text
    return json.dumps(value, sort_keys=True, separators=(',', ':'))


def _object(pairs):
    return ('{' + ','.join(json.dumps(key) + ':' + _encode(value) for key, value in pairs) + '}').encode()


class StrictManifestTests(unittest.TestCase):
    """Offline manifest contract. Members are inert bytes and are never executed."""

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-manifest-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            path = self.source / name
            path.write_bytes(b'SYNTHETIC NON-EXECUTABLE ' + name.encode() + b'\n')
            path.chmod(0o600)

    def test_builder_bytes_stay_stable_and_not_installable(self):
        first = self.bundle.build(self.source, self.root / 'stable-a')
        second = self.bundle.build(self.source, self.root / 'stable-b')
        self.assertEqual(second, first)
        manifest = json.loads((self.root / 'stable-a' / 'manifest.json').read_bytes())
        self.assertEqual(set(manifest), {'schema', 'entry', 'files'})
        self.assertNotIn('manifest_sha256', manifest)
        verified = self.bundle.verify(self.root / 'stable-a')
        self.assertIs(verified['verified'], True)
        self.assertIs(verified['installable'], False)
        self.assertEqual(set(verified), {'verified', 'installable', 'manifest_sha256'})

    def test_default_executable_check_is_unchanged(self):
        staging = self._stage('executable-default')
        (staging / self.bundle.REQUIRED[0]).chmod(0o755)
        with self.assertRaises(self.bundle.BundleError) as caught:
            self.bundle.verify(staging)
        self.assertEqual(caught.exception.code, 'unexpected_executable')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertIs(self.bundle.verify(staging, executable=False)['installable'], False)

    def test_manifest_shape_budget_and_member_closure(self):
        """65535/65536/65537 rows are budget pads of JSON whitespace, not new fields."""
        for name, expected, prepare in self._cases():
            with self.subTest(case=name):
                self._assert_case(name, expected, prepare)

    def _stage(self, name):
        staging = self.root / name
        self.bundle.build(self.source, staging)
        return staging

    def _cases(self):
        staging = self._stage('prototype')
        raw = (staging / 'manifest.json').read_bytes()
        parsed = json.loads(raw)
        self.assertLess(len(raw), 65535)
        name = self.bundle.REQUIRED[0]
        digest = parsed['files'][name]
        marker = 'SYNTHETIC-MARKER-DO-NOT-ECHO'
        files = parsed['files']

        def document(pairs):
            return _object(pairs)

        def with_files(value):
            return document((('schema', parsed['schema']), ('entry', parsed['entry']), ('files', value)))

        def padded(size):
            self.assertGreaterEqual(size, len(raw))
            return raw + b' ' * (size - len(raw))

        omitted = dict(files)
        omitted.pop(name)
        traversal = dict(omitted)
        traversal['../outside'] = digest
        extra_key = dict(files)
        extra_key['extra-member'] = digest
        number_digest = dict(files)
        number_digest[name] = 1
        null_digest = dict(files)
        null_digest[name] = None
        object_digest = dict(files)
        object_digest[name] = {'nested': 1}
        array_digest = dict(files)
        array_digest[name] = [digest]
        same_files = _object(((name, digest), *files.items()))
        wrong_then_right = _object(((name, '0' * 64), *files.items()))

        def replace_one_digest(token):
            pairs = [(name, _Raw(token))]
            pairs.extend((key, value) for key, value in files.items() if key != name)
            return _Raw(_object(pairs).decode())

        deep = ('{"a":' * 40 + '1' + '}' * 40).encode()
        recursive = ('{"a":' * 10922 + '1' + '}' * 10922).encode()
        self.assertLessEqual(len(recursive), 65536)
        rows = [
            ('builder_bytes', None, lambda _staging: raw),
            ('field_order', None, lambda _staging: document((
                ('files', files), ('schema', parsed['schema']), ('entry', parsed['entry'])))),
            ('whitespace', None, lambda _staging: json.dumps(parsed, indent=2).encode()),
            ('budget_65535_whitespace', None, lambda _staging: padded(65535)),
            ('budget_65536_whitespace', None, lambda _staging: padded(65536)),
            ('budget_65537_whitespace', 'manifest_budget_exceeded', lambda _staging: padded(65537)),
            ('budget_65537_trailing_junk', 'manifest_budget_exceeded',
             lambda _staging: padded(65536) + b'X'),
            ('empty', 'manifest_invalid', lambda _staging: b''),
            ('truncated', 'manifest_invalid', lambda _staging: b'{'),
            ('invalid_utf8', 'manifest_invalid', lambda _staging: b'\xff'),
            ('utf8_prefix_then_invalid', 'manifest_invalid',
             lambda _staging: marker.encode() + b'\xff'),
            ('wrong_schema', 'manifest_invalid', lambda _staging: document((
                ('schema', marker), ('entry', parsed['entry']), ('files', files)))),
            ('missing_schema', 'manifest_invalid', lambda _staging: document((
                ('entry', parsed['entry']), ('files', files)))),
            ('wrong_entry', 'manifest_invalid', lambda _staging: document((
                ('schema', parsed['schema']), ('entry', marker), ('files', files)))),
            ('missing_entry', 'manifest_invalid', lambda _staging: document((
                ('schema', parsed['schema']), ('files', files)))),
            ('extra_approved_field', 'manifest_invalid', lambda _staging: document((
                ('schema', parsed['schema']), ('entry', parsed['entry']),
                ('files', files), ('approved', True)))),
            ('duplicate_schema_last_legal', 'manifest_invalid', lambda _staging: document((
                ('schema', marker), ('schema', parsed['schema']),
                ('entry', parsed['entry']), ('files', files)))),
            ('duplicate_schema_same_value', 'manifest_invalid', lambda _staging: document((
                ('entry', parsed['entry']), ('files', files),
                ('schema', parsed['schema']), ('schema', parsed['schema'])))),
            ('duplicate_entry_last_legal', 'manifest_invalid', lambda _staging: document((
                ('entry', marker), ('entry', parsed['entry']),
                ('schema', parsed['schema']), ('files', files)))),
            ('duplicate_entry_same_value', 'manifest_invalid', lambda _staging: document((
                ('schema', parsed['schema']), ('entry', parsed['entry']),
                ('files', files), ('entry', parsed['entry'])))),
            ('duplicate_member_last_correct', 'manifest_invalid',
             lambda _staging: with_files(_Raw(wrong_then_right.decode()))),
            ('duplicate_member_same_digest', 'manifest_invalid',
             lambda _staging: with_files(_Raw(same_files.decode()))),
            ('top_array', 'manifest_invalid', lambda _staging: b'[]'),
            ('top_null', 'manifest_invalid', lambda _staging: b'null'),
            ('top_string', 'manifest_invalid', lambda _staging: b'"nope"'),
            ('top_number', 'manifest_invalid', lambda _staging: b'1'),
            ('top_bool', 'manifest_invalid', lambda _staging: b'true'),
            ('files_null', 'manifest_invalid', lambda _staging: with_files(None)),
            ('files_array', 'manifest_invalid', lambda _staging: with_files(list(files))),
            ('files_string', 'manifest_invalid', lambda _staging: with_files(marker)),
            ('digest_number', 'manifest_invalid', lambda _staging: with_files(number_digest)),
            ('digest_null', 'manifest_invalid', lambda _staging: with_files(null_digest)),
            ('digest_object', 'manifest_invalid', lambda _staging: with_files(object_digest)),
            ('digest_array', 'manifest_invalid', lambda _staging: with_files(array_digest)),
            ('nan_constant', 'manifest_invalid', lambda _staging: document((
                ('schema', _Raw('NaN')), ('entry', parsed['entry']), ('files', files)))),
            ('infinity_constant', 'manifest_invalid',
             lambda _staging: with_files(replace_one_digest('Infinity'))),
            ('negative_infinity', 'manifest_invalid',
             lambda _staging: with_files(replace_one_digest('-Infinity'))),
            ('deep_nest', 'manifest_invalid', lambda _staging: deep),
            ('deep_nest_within_budget', 'manifest_invalid', lambda _staging: recursive),
            ('missing_manifest_member', 'member_set_rejected', lambda _staging: document((
                ('schema', parsed['schema']), ('entry', parsed['entry']), ('files', omitted)))),
            ('extra_manifest_member', 'member_set_rejected', lambda _staging: document((
                ('schema', parsed['schema']), ('entry', parsed['entry']), ('files', extra_key)))),
            ('path_traversal_member', 'member_set_rejected', lambda _staging: document((
                ('schema', parsed['schema']), ('entry', parsed['entry']), ('files', traversal)))),
            ('tamper_member', 'digest_rejected', self._tamper),
            ('missing_member_file', 'unexpected_member', self._unlink_member),
            ('extra_directory_entry', 'unexpected_member', self._extra_file),
            ('symlink_member', 'member_type_rejected', self._symlink_member),
            ('directory_member', 'member_type_rejected', self._directory_member),
            ('executable_member', 'unexpected_executable', self._executable_member),
        ]
        return rows

    def _assert_case(self, name, expected, prepare):
        staging = self._stage('case-' + name)
        written = prepare(staging)
        manifest_path = staging / 'manifest.json'
        if written is not None:
            manifest_path.write_bytes(written)
            manifest_path.chmod(0o600)
        raw = manifest_path.read_bytes()
        before = self._snapshot(staging)
        outside = self.root / 'outside'
        try:
            result = self.bundle.verify(staging)
        except self.bundle.BundleError as error:
            self.assertEqual(self._snapshot(staging), before)
            self.assertFalse(outside.exists())
            self.assertEqual(error.code, expected)
            self.assertEqual(error.exit_code, 69)
            self.assertEqual(str(error), expected)
            self.assertNotIn('SYNTHETIC-MARKER-DO-NOT-ECHO', str(error))
            return
        self.assertEqual(self._snapshot(staging), before)
        self.assertFalse(outside.exists())
        self.assertIsNone(expected)
        self.assertEqual(result, {
            'verified': True, 'installable': False,
            'manifest_sha256': hashlib.sha256(raw).hexdigest(),
        })
        forbidden = {'approval.json', 'host-profile.json', 'registration.json', 'pin.json'}
        self.assertTrue(forbidden.isdisjoint(path.name for path in self.root.rglob('*')))

    def _snapshot(self, staging):
        rows = {}
        for path in staging.iterdir():
            info = path.lstat()
            digest = None
            if stat.S_ISREG(info.st_mode):
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
            rows[path.name] = (digest, info.st_mode, info.st_uid, info.st_ino, info.st_size, info.st_mtime_ns)
        return rows

    def _tamper(self, staging):
        (staging / self.bundle.REQUIRED[0]).write_bytes(b'SYNTHETIC TAMPER\n')

    def _unlink_member(self, staging):
        (staging / self.bundle.REQUIRED[0]).unlink()

    def _extra_file(self, staging):
        extra = staging / 'extra-not-approval'
        extra.write_bytes(b'SYNTHETIC NON-EXECUTABLE extra\n')
        extra.chmod(0o644)

    def _symlink_member(self, staging):
        path = staging / self.bundle.REQUIRED[0]
        path.unlink()
        path.symlink_to(staging / self.bundle.REQUIRED[1])

    def _directory_member(self, staging):
        path = staging / self.bundle.REQUIRED[0]
        path.unlink()
        path.mkdir(mode=0o700)

    def _executable_member(self, staging):
        (staging / self.bundle.REQUIRED[0]).chmod(0o755)


def _verify_fifo_manifest(mode, staging):
    """Subprocess entry. A fifo must be rejected without waiting for a writer."""
    import upgrade_bundle as bundle
    staging = Path(staging)
    manifest = staging / 'manifest.json'
    state = {'done': False}
    if mode == 'race':
        real_open = os.open

        def wrapped(path, flags, *args, **kwargs):
            if not state['done'] and os.fspath(path) == os.fspath(manifest):
                state['done'] = True
                os.rename(manifest, staging.parent / 'fifo-held-race.json')
                os.mkfifo(manifest, 0o600)
            return real_open(path, flags, *args, **kwargs)

        os.open = wrapped
    code = None
    try:
        bundle.verify(staging)
    except bundle.BundleError as error:
        code = error.code if error.exit_code == 69 and str(error) == error.code else 'bad-error'
    if mode == 'race' and not state['done']:
        sys.stdout.write('hook-not-used\n')
        return 4
    if code == 'manifest_type_rejected':
        return 0
    sys.stdout.write((code or 'accepted') + '\n')
    return 2


class ManifestFileTests(unittest.TestCase):
    """Manifest path checks. Fixtures are inert and are not executed or imported."""

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-manifest-file-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            path = self.source / name
            path.write_bytes(b'SYNTHETIC NON-EXECUTABLE ' + name.encode() + b'\n')
            path.chmod(0o600)

    def test_manifest_file_contract(self):
        for name, expected, executable, mutate in self._cases():
            with self.subTest(case=name):
                self._assert_case(name, expected, executable, mutate)

    def test_open_is_nofollow_nonblocking_and_bounded(self):
        """The descriptor read stops one byte past the budget and does not follow."""
        staging = self._stage('bounded-read')
        manifest = staging / 'manifest.json'
        raw = manifest.read_bytes()
        padded = raw + b' ' * (200000 - len(raw))
        self.assertGreater(len(padded), 65537)
        manifest.write_bytes(padded)
        before = self._metadata(self.root)
        observed = self._observe(manifest)
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            observed['restore']()
            self.assertEqual(self._metadata(self.root), before)
            self.assertEqual(self._fds(), observed['fds'])
        self.assertTrue(observed['opened'])
        self.assertGreaterEqual(observed['fstats'], 2)
        self.assertTrue(observed['flags'] & os.O_NOFOLLOW)
        self.assertTrue(observed['flags'] & os.O_NONBLOCK)
        self.assertFalse(observed['flags'] & (os.O_WRONLY | os.O_RDWR))
        self.assertEqual(observed['read'], 65537)
        self.assertTrue(observed['requests'])
        self.assertLessEqual(max(observed['requests']), 65537)
        self.assertEqual(caught.exception.code, 'manifest_budget_exceeded')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(str(caught.exception), 'manifest_budget_exceeded')

    def test_checked_file_is_not_replaced_by_a_later_path(self):
        """Hooks call the real open/read. They do not invent the stat or bytes."""
        self._replace_before_open('replace-symlink', 'symlink', 'manifest_type_rejected')
        self._replace_before_open('replace-directory', 'directory', 'manifest_type_rejected')
        self._replace_after_open()
        self._truncate_during_read()

    def test_fifo_manifest_does_not_block(self):
        static = self._stage('fifo-static')
        manifest = static / 'manifest.json'
        held = self.root / 'fifo-static-held.json'
        held.write_bytes(manifest.read_bytes())
        held.chmod(0o600)
        manifest.unlink()
        os.mkfifo(manifest, 0o600)
        before = self._metadata(self.root)
        self._fifo_child('static', static)
        self.assertEqual(self._metadata(self.root), before)

        raced = self._stage('fifo-race')
        digest = hashlib.sha256((raced / 'manifest.json').read_bytes()).hexdigest()
        self._fifo_child('race', raced)
        saved = self.root / 'fifo-held-race.json'
        self.assertEqual(hashlib.sha256(saved.read_bytes()).hexdigest(), digest)
        self.assertTrue(stat.S_ISFIFO((raced / 'manifest.json').lstat().st_mode))

    def test_owner_compares_fstat_uid_with_expected_uid(self):
        """Expected-UID seam only. fstat is real; this is not a foreign-owner proof."""
        for name, executable in (('owner-default', True), ('owner-exec-false', False)):
            with self.subTest(case=name):
                staging = self._stage(name)
                manifest = staging / 'manifest.json'
                manifest.chmod(0o700 if not executable else 0o600)
                real_uid = os.getuid()
                self.assertNotEqual(real_uid, 0)
                self.assertEqual(manifest.lstat().st_uid, real_uid)
                if not hasattr(self.bundle, '_manifest_owner_uid'):
                    self.fail('manifest owner comparison seam is absent')
                original = self.bundle._manifest_owner_uid
                self.bundle._manifest_owner_uid = lambda: real_uid + 1
                try:
                    with self.assertRaises(self.bundle.BundleError) as caught:
                        self.bundle.verify(staging, executable=executable)
                    self.assertEqual(caught.exception.code, 'owner_rejected')
                    self.assertEqual(caught.exception.exit_code, 69)
                    self.assertEqual(str(caught.exception), 'owner_rejected')
                    self.assertNotIn(str(self.root), str(caught.exception))
                finally:
                    self.bundle._manifest_owner_uid = original

    def test_missing_nofollow_or_nonblock_does_not_follow(self):
        cases = (
            ('hide-nofollow-symlink', 'O_NOFOLLOW', True),
            ('hide-nofollow-regular', 'O_NOFOLLOW', False),
            ('hide-nonblock-symlink', 'O_NONBLOCK', True),
            ('hide-nonblock-regular', 'O_NONBLOCK', False),
        )
        for name, flag, symlink in cases:
            with self.subTest(case=name):
                staging = self._stage(name)
                if symlink:
                    self._point_symlink(staging, self.root / (name + '.json'), relative=False)
                saved = getattr(os, flag)
                before = self._metadata(self.root)
                try:
                    delattr(os, flag)
                    with self.assertRaises(self.bundle.BundleError) as caught:
                        self.bundle.verify(staging)
                finally:
                    setattr(os, flag, saved)
                    self.assertEqual(self._metadata(self.root), before)
                self.assertEqual(caught.exception.code, 'manifest_type_rejected')
                self.assertEqual(caught.exception.exit_code, 69)
                self.assertEqual(str(caught.exception), 'manifest_type_rejected')

    def _cases(self):
        def symlink(relative):
            def mutate(staging):
                self._point_symlink(staging, self.root / (staging.name + '.json'), relative=relative)
            return mutate

        def mode(bits):
            def mutate(staging):
                (staging / 'manifest.json').chmod(bits)
            return mutate

        def chain(staging):
            manifest = staging / 'manifest.json'
            inner = self.root / (staging.name + '-inner.json')
            outer = self.root / (staging.name + '-outer.json')
            inner.write_bytes(manifest.read_bytes())
            inner.chmod(0o600)
            outer.symlink_to(inner)
            manifest.unlink()
            manifest.symlink_to(Path('..') / outer.name)

        def internal(staging):
            manifest = staging / 'manifest.json'
            manifest.unlink()
            manifest.symlink_to(self.bundle.REQUIRED[0])

        def dangling(staging):
            manifest = staging / 'manifest.json'
            manifest.unlink()
            manifest.symlink_to('missing-manifest-target.json')

        def directory(staging):
            manifest = staging / 'manifest.json'
            manifest.unlink()
            manifest.mkdir(mode=0o700)

        def linked_directory(staging):
            folder = self.root / (staging.name + '-dir')
            folder.mkdir(mode=0o700)
            manifest = staging / 'manifest.json'
            manifest.unlink()
            manifest.symlink_to(folder)

        def remove(staging):
            (staging / 'manifest.json').unlink()

        def hardlink(staging):
            os.link(staging / 'manifest.json', self.root / (staging.name + '-link.json'))

        def rewrite(suffix):
            def mutate(staging):
                manifest = staging / 'manifest.json'
                raw = manifest.read_bytes()
                if suffix == 'junk':
                    updated = raw + b'x'
                elif suffix == 'second':
                    updated = raw + b'{}'
                elif suffix == 'escape':
                    value = json.loads(raw)
                    updated = (b'{"\\u0073chema":' + json.dumps(value['schema']).encode()
                               + b',' + raw[1:])
                else:
                    updated = raw + b' ' * (int(suffix) - len(raw))
                manifest.write_bytes(updated)
                manifest.chmod(0o644)
            return mutate

        return (
            ('manifest_symlink_absolute', 'manifest_type_rejected', True, symlink(False)),
            ('manifest_symlink_relative', 'manifest_type_rejected', True, symlink(True)),
            ('manifest_symlink_executable_false', 'manifest_type_rejected', False, symlink(False)),
            ('manifest_executable', 'unexpected_executable', True, mode(0o700)),
            ('manifest_unreadable', 'manifest_unreadable', True, mode(0)),
            ('manifest_user_executable', 'unexpected_executable', True, mode(0o601)),
            ('dangling_symlink', 'manifest_type_rejected', True, dangling),
            ('symlink_chain', 'manifest_type_rejected', True, chain),
            ('internal_symlink', 'manifest_type_rejected', True, internal),
            ('symlink_to_directory', 'manifest_type_rejected', True, linked_directory),
            ('directory_manifest', 'manifest_type_rejected', True, directory),
            ('manifest_missing', 'manifest_missing', True, remove),
            ('builder_regular', None, True, lambda staging: None),
            ('manifest_0600', None, True, mode(0o600)),
            ('manifest_0644', None, True, mode(0o644)),
            ('manifest_0400', None, True, mode(0o400)),
            ('manifest_executable_opt_out', None, False, mode(0o700)),
            ('manifest_hardlink', None, True, hardlink),
            ('trailing_junk_under_budget', 'manifest_invalid', True, rewrite('junk')),
            ('second_document_under_budget', 'manifest_invalid', True, rewrite('second')),
            ('escaped_duplicate_schema', 'manifest_invalid', True, rewrite('escape')),
            ('budget_65536', None, True, rewrite('65536')),
            ('budget_65537', 'manifest_budget_exceeded', True, rewrite('65537')),
            ('executable_false_over_budget', 'manifest_budget_exceeded', False, rewrite('65537')),
        )

    def _assert_case(self, name, expected, executable, mutate):
        staging = self._stage(name)
        mutate(staging)
        before = self._metadata(self.root)
        fds = self._fds()
        try:
            try:
                outcome = self.bundle.verify(staging, executable=executable)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            self.assertEqual(self._metadata(self.root), before)
            self.assertEqual(self._fds(), fds)
        self._assert_outcome(staging, expected, outcome)
        forbidden = {'approval.json', 'host-profile.json', 'registration.json', 'pin.json'}
        self.assertTrue(forbidden.isdisjoint(path.name for path in self.root.rglob('*')))

    def _assert_outcome(self, staging, expected, outcome):
        if isinstance(outcome, self.bundle.BundleError):
            self.assertEqual(outcome.code, expected)
            self.assertEqual(outcome.exit_code, 69)
            self.assertEqual(str(outcome), expected)
            self.assertNotIn(str(self.root), str(outcome))
            self.assertNotIn('SYNTHETIC', str(outcome))
            return
        self.assertIsNone(expected)
        manifest = staging / 'manifest.json'
        info = manifest.lstat()
        self.assertTrue(stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode))
        raw = manifest.read_bytes()
        self.assertEqual(outcome, {
            'verified': True, 'installable': False,
            'manifest_sha256': hashlib.sha256(raw).hexdigest(),
        })

    def _replace_before_open(self, name, kind, expected):
        staging = self._stage(name)
        manifest = staging / 'manifest.json'
        held = self.root / (name + '-held.json')
        digest = hashlib.sha256(manifest.read_bytes()).hexdigest()
        state = {'done': False}
        real_open = os.open

        def wrapped(path, flags, *args, **kwargs):
            if not state['done'] and os.fspath(path) == os.fspath(manifest):
                state['done'] = True
                os.rename(manifest, held)
                if kind == 'symlink':
                    os.symlink(held, manifest)
                else:
                    os.mkdir(manifest, 0o700)
            return real_open(path, flags, *args, **kwargs)

        os.open = wrapped
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            os.open = real_open
        self.assertTrue(state['done'])
        self.assertEqual(caught.exception.code, expected)
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(hashlib.sha256(held.read_bytes()).hexdigest(), digest)

    def _replace_after_open(self):
        staging = self._stage('replace-after-open')
        manifest = staging / 'manifest.json'
        held = self.root / 'replace-after-open-held.json'
        digest = hashlib.sha256(manifest.read_bytes()).hexdigest()
        state = {'done': False}
        real_open = os.open

        def wrapped(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if not state['done'] and os.fspath(path) == os.fspath(manifest):
                state['done'] = True
                os.rename(manifest, held)
                replacement = real_open(manifest, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
                os.write(replacement, b'{}\n')
                os.close(replacement)
            return fd

        os.open = wrapped
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            os.open = real_open
        self.assertTrue(state['done'])
        self.assertEqual(caught.exception.code, 'manifest_changed')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(hashlib.sha256(held.read_bytes()).hexdigest(), digest)

    def _truncate_during_read(self):
        staging = self._stage('truncate-during-read')
        manifest = staging / 'manifest.json'
        state = {'done': False, 'fds': set()}
        real_open = os.open
        real_read = os.read

        def wrapped_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if os.fspath(path) == os.fspath(manifest):
                state['fds'].add(fd)
            return fd

        def wrapped_read(fd, size):
            data = real_read(fd, size)
            if fd in state['fds'] and not state['done']:
                state['done'] = True
                writer = real_open(manifest, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    os.ftruncate(writer, 0)
                finally:
                    os.close(writer)
            return data

        os.open = wrapped_open
        os.read = wrapped_read
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            os.open = real_open
            os.read = real_read
        self.assertTrue(state['done'])
        self.assertEqual(caught.exception.code, 'manifest_changed')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(manifest.lstat().st_size, 0)

    def _fifo_child(self, mode, staging):
        try:
            completed = subprocess.run(
                [sys.executable, '-B', '-c',
                 'import sys; sys.path.insert(0, sys.argv[1]); '
                 'import test_upgrade_bundle as tests; '
                 'raise SystemExit(tests._verify_fifo_manifest(sys.argv[2], sys.argv[3]))',
                 str(HERE), mode, str(staging)],
                cwd=self.root, timeout=5, capture_output=True, text=True,
                env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        except subprocess.TimeoutExpired:
            self.fail('fifo manifest verify was still blocked after 5s')
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertNotIn('SYNTHETIC', completed.stdout)
        self.assertNotIn(str(self.root), completed.stdout)

    def _observe(self, manifest):
        observed = {'opened': False, 'flags': 0, 'read': 0, 'requests': [], 'fstats': 0,
                    'fds': self._fds(), 'restore': None}
        real_open, real_read, real_fstat = os.open, os.read, os.fstat
        fds = set()

        def wrapped_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if os.fspath(path) == os.fspath(manifest):
                observed['opened'] = True
                observed['flags'] = flags
                fds.add(fd)
            return fd

        def wrapped_read(fd, size):
            if fd in fds:
                observed['requests'].append(size)
            data = real_read(fd, size)
            if fd in fds:
                observed['read'] += len(data)
            return data

        def wrapped_fstat(fd):
            if fd in fds:
                observed['fstats'] += 1
            return real_fstat(fd)

        os.open, os.read, os.fstat = wrapped_open, wrapped_read, wrapped_fstat

        def restore():
            os.open, os.read, os.fstat = real_open, real_read, real_fstat

        observed['restore'] = restore
        return observed

    def _point_symlink(self, staging, target, *, relative):
        manifest = staging / 'manifest.json'
        target.write_bytes(manifest.read_bytes())
        target.chmod(0o600)
        manifest.unlink()
        manifest.symlink_to(Path('..') / target.name if relative else target)

    def _stage(self, name):
        staging = self.root / name
        self.bundle.build(self.source, staging)
        return staging

    def _metadata(self, root):
        rows = []
        for path in sorted(root.rglob('*'), key=lambda item: item.as_posix()):
            info = path.lstat()
            digest = None
            link = None
            if stat.S_ISLNK(info.st_mode):
                link = os.readlink(path)
            elif stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) & 0o400:
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
            rows.append((str(path.relative_to(root)), stat.S_IFMT(info.st_mode),
                         stat.S_IMODE(info.st_mode), info.st_uid, info.st_dev, info.st_ino,
                         info.st_size, info.st_mtime_ns, info.st_ctime_ns, digest, link))
        return tuple(rows)

    def _fds(self):
        base = '/proc/self/fd' if os.path.isdir('/proc/self/fd') else '/dev/fd'
        return frozenset(os.listdir(base))


if __name__ == '__main__':
    unittest.main()
