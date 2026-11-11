#!/usr/bin/env python3
"""S6.06 bundle builder. Staging fixtures only; nothing is provisioned."""
import errno
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


def _entry_matches(path, target, kwargs=None):
    """Historical full path, or one basename looked up through dir_fd."""
    if isinstance(path, int):
        return False
    try:
        text = os.fspath(path)
    except TypeError:
        return False
    if os.path.normpath(text) == os.path.normpath(os.fspath(target)):
        return True
    if not kwargs or kwargs.get('dir_fd') is None:
        return False
    if text in ('.', '..') or text != Path(text).name:
        return False
    return text == Path(os.fspath(target)).name


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
            if not state['done'] and _entry_matches(path, manifest, kwargs):
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
                real_open = os.open
                opened = []

                def wrapped(path, flags, *args, **kwargs):
                    opened.append(path if isinstance(path, int) else os.fspath(path))
                    return real_open(path, flags, *args, **kwargs)

                os.open = wrapped
                try:
                    delattr(os, flag)
                    with self.assertRaises(self.bundle.BundleError) as caught:
                        self.bundle.verify(staging)
                finally:
                    os.open = real_open
                    setattr(os, flag, saved)
                    self.assertEqual(self._metadata(self.root), before)
                self.assertEqual(opened, [])
                self.assertEqual(caught.exception.code, 'staging_unreadable')
                self.assertEqual(caught.exception.exit_code, 69)
                self.assertEqual(str(caught.exception), 'staging_unreadable')

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
            if not state['done'] and _entry_matches(path, manifest, kwargs):
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
            if not state['done'] and _entry_matches(path, manifest, kwargs):
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
            if _entry_matches(path, manifest, kwargs):
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
            if _entry_matches(path, manifest, kwargs):
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


def _verify_fifo_member(mode, staging, member):
    """Subprocess entry. A member fifo must be rejected without waiting for a writer."""
    import upgrade_bundle as bundle
    staging = Path(staging)
    target = staging / member
    state = {'done': False}
    if mode == 'race':
        real_open = os.open

        def wrapped(path, flags, *args, **kwargs):
            if not state['done'] and _entry_matches(path, target, kwargs):
                state['done'] = True
                os.rename(target, staging.parent / 'member-fifo-held')
                os.mkfifo(target, 0o600)
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
    if code == 'member_type_rejected':
        return 0
    sys.stdout.write((code or 'accepted') + '\n')
    return 2


class MemberFileTests(unittest.TestCase):
    """Ordinary member descriptor contract. Fixtures are inert and never executed."""

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-member-file-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            path = self.source / name
            path.write_bytes(b'SYNTHETIC INERT MEMBER ' + name.encode() + b'\n')
            path.chmod(0o600)

    def test_member_descriptor_probe_cases(self):
        """The six recorded races plus five controls. Hooks call the real open."""
        cases = (
            ('normal', None, True, 'none', True),
            ('symlink-static', 'member_type_rejected', True, 'static-symlink', False),
            ('exec-static', 'unexpected_executable', True, 'static-exec', True),
            ('exec-optout', None, False, 'static-exec', True),
            ('unreadable', 'member_unreadable', True, 'static-unreadable', True),
            ('symlink-before-open', 'member_type_rejected', True, 'symlink-before', True),
            ('symlink-before-open-optout', 'member_type_rejected', False, 'symlink-before', True),
            ('executable-replacement-before-open', 'member_changed', True, 'replace-exec-before', True),
            ('path-replacement-after-open', 'member_changed', True, 'replace-after', True),
            ('chmod-after-open', 'member_changed', True, 'chmod-after', True),
            ('digest-tamper', 'digest_rejected', True, 'static-tamper', True),
        )
        for name, expected, executable, action, expect_hook in cases:
            with self.subTest(case=name):
                self._assert_probe(name, expected, executable, action, expect_hook)

    def test_static_member_shape_and_listing(self):
        cases = (
            ('symlink-absolute', 'member_type_rejected', True, 'absolute'),
            ('symlink-relative', 'member_type_rejected', True, 'relative'),
            ('symlink-internal', 'member_type_rejected', True, 'internal'),
            ('symlink-dangling', 'member_type_rejected', True, 'dangling'),
            ('symlink-chain', 'member_type_rejected', True, 'chain'),
            ('directory', 'member_type_rejected', True, 'directory'),
            ('mode-0600', None, True, '0600'),
            ('mode-0644', None, True, '0644'),
            ('mode-0700', 'unexpected_executable', True, '0700'),
            ('mode-0700-optout', None, False, '0700'),
            ('tamper-optout', 'digest_rejected', False, 'tamper'),
            ('missing-before-list', 'unexpected_member', True, 'unlink'),
        )
        for name, expected, executable, kind in cases:
            with self.subTest(case=name):
                staging = self._stage(name)
                target = staging / 'upgrade.py'
                self._prepare_static(target, kind)
                if kind in ('0600', '0644', '0700', 'tamper'):
                    self._expect(staging, expected, executable)
                else:
                    self._expect_unopened(staging, target, expected, executable)

    def test_before_open_replacement_rejects_type_and_identity(self):
        """Replacement runs before the real open. executable=False does not bypass it."""
        cases = (
            ('absolute', 'member_type_rejected', True),
            ('relative', 'member_type_rejected', True),
            ('internal', 'member_type_rejected', True),
            ('dangling', 'member_type_rejected', True),
            ('chain', 'member_type_rejected', True),
            ('directory', 'member_type_rejected', True),
            ('absolute-optout', 'member_type_rejected', False),
            ('same-bytes', 'member_changed', True),
            ('same-bytes-optout', 'member_changed', False),
        )
        for kind, expected, executable in cases:
            name = 'before-' + kind
            with self.subTest(case=name):
                self._race(name, expected, executable, lambda target: self._replace_before(target, kind),
                           when='before')
                if kind.startswith('same-bytes'):
                    held = self.root / (name + '-held')
                    current = self.root / name / 'upgrade.py'
                    self.assertEqual(held.read_bytes(), current.read_bytes())
                    self.assertNotEqual(held.lstat().st_ino, current.lstat().st_ino)

    def test_fifo_member_does_not_block(self):
        static = self._stage('fifo-static')
        target = static / 'upgrade.py'
        held = self.root / 'fifo-static-held'
        held.write_bytes(target.read_bytes())
        held.chmod(0o600)
        target.unlink()
        os.mkfifo(target, 0o600)
        before = self._metadata(self.root)
        self._fifo_child('static', static)
        self.assertEqual(self._metadata(self.root), before)
        self.assertTrue(stat.S_ISFIFO(target.lstat().st_mode))

        raced = self._stage('fifo-race')
        digest = hashlib.sha256((raced / 'upgrade.py').read_bytes()).hexdigest()
        self._fifo_child('race', raced)
        saved = self.root / 'member-fifo-held'
        self.assertEqual(hashlib.sha256(saved.read_bytes()).hexdigest(), digest)
        self.assertTrue(stat.S_ISFIFO((raced / 'upgrade.py').lstat().st_mode))

    def test_disappearance_is_missing_then_changed(self):
        self._missing_after_listing()
        staging, target = self._race(
            'unlink-after-open', 'member_changed', True,
            lambda path: path.rename(self.root / 'unlink-after-open-held'), when='after')
        self.assertFalse(target.exists())
        held = self.root / 'unlink-after-open-held'
        self.assertEqual(held.read_bytes(), (self.source / 'upgrade.py').read_bytes())
        self.assertFalse((staging / 'upgrade.py').exists())

    def test_changes_during_read_reject_matching_fd_bytes(self):
        """Mutations run after a real read. They are not verify writes and not hardware faults."""
        for kind in ('rewrite', 'truncate', 'chmod', 'grow'):
            with self.subTest(case=kind):
                self._during_read(kind)

    def test_streamed_digest_is_not_the_manifest_budget(self):
        self.assertEqual(self.bundle._MANIFEST_BUDGET, 65536)
        self.assertEqual(self.bundle._MEMBER_CHUNK, 65536)
        for size in (0, 65535, 65536, 65537, 2 * 1024 * 1024):
            with self.subTest(size=size):
                payload = b'Z' * size
                source_member = self.source / 'upgrade.py'
                source_member.write_bytes(payload)
                source_member.chmod(0o600)
                staging = self._stage('size-' + str(size))
                target = staging / 'upgrade.py'
                self.assertEqual(target.stat().st_size, size)
                if size > self.bundle._MEMBER_CHUNK:
                    outcome = self._observe_member_read(staging, target, size)
                else:
                    fds = self._fds()
                    outcome = self.bundle.verify(staging)
                    self.assertEqual(self._fds(), fds)
                self._assert_member_outcome(staging, None, outcome)

    def test_short_read_and_eintr_retry(self):
        self._short_read()
        self._eintr('open')
        self._eintr('read')

    def test_owner_compares_real_fstat_uid(self):
        """Expected-UID seam only. fstat is real; this is not a foreign-owner proof."""
        real_uid = os.getuid()
        self.assertNotEqual(real_uid, 0)
        for executable in (True, False):
            with self.subTest(executable=executable):
                staging = self._stage('owner-' + str(executable).lower())
                target = staging / 'upgrade.py'
                if not executable:
                    target.chmod(0o700)
                self.assertEqual(target.lstat().st_uid, real_uid)
                seen = []
                real_fstat = os.fstat

                def wrapped(fd):
                    info = real_fstat(fd)
                    seen.append(info.st_uid)
                    return info

                original = self.bundle._member_owner_uid
                os.fstat = wrapped
                self.bundle._member_owner_uid = lambda: real_uid + 1
                fds = self._fds()
                try:
                    with self.assertRaises(self.bundle.BundleError) as caught:
                        self.bundle.verify(staging, executable=executable)
                finally:
                    os.fstat = real_fstat
                    self.bundle._member_owner_uid = original
                    self.assertEqual(self._fds(), fds)
                self.assertEqual(caught.exception.code, 'owner_rejected')
                self.assertEqual(caught.exception.exit_code, 69)
                self.assertEqual(str(caught.exception), 'owner_rejected')
                self.assertNotIn(str(self.root), str(caught.exception))
                self.assertIn(real_uid, seen)

    def test_missing_open_flags_do_not_follow(self):
        cases = (
            ('hide-nofollow-symlink', 'O_NOFOLLOW', True),
            ('hide-nofollow-regular', 'O_NOFOLLOW', False),
            ('hide-nonblock-symlink', 'O_NONBLOCK', True),
            ('hide-nonblock-regular', 'O_NONBLOCK', False),
        )
        for name, flag, symlink in cases:
            with self.subTest(case=name):
                self._hide_flag(name, flag, symlink)

    def test_injected_io_seams_do_not_swallow_programming_errors(self):
        """Injected errno values cover the mapping. They are not hardware failures."""
        self._inject_open(errno.ENXIO, 'member_type_rejected')
        self._inject_open(errno.ENOENT, 'member_missing')
        self._inject_member_io('read', 'member_unreadable')
        self._inject_member_io('fstat', 'member_unreadable')
        self._inject_programming_error()

    def test_external_hardlink_is_not_a_new_rejection(self):
        staging = self._stage('hardlink')
        target = staging / 'upgrade.py'
        link = self.root / 'hardlink-outside'
        os.link(target, link)
        self._expect(staging, None)
        self.assertEqual(target.lstat().st_ino, link.lstat().st_ino)
        self.assertGreaterEqual(target.lstat().st_nlink, 2)

    def test_member_identity_ignores_atime(self):
        staging = self._stage('atime')
        info = (staging / 'upgrade.py').lstat()
        identity = self.bundle._manifest_identity(info)
        self.assertEqual(identity, (
            info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
            info.st_mtime_ns, info.st_ctime_ns))
        self.assertEqual(len(identity), 7)
        # A fresh file can stamp atime equal to mtime or ctime. That shared
        # number is not an atime field in the identity.
        if info.st_atime_ns not in (info.st_mtime_ns, info.st_ctime_ns):
            self.assertNotIn(info.st_atime_ns, identity)
        self._expect(staging, None)

    def _assert_probe(self, name, expected, executable, action, expect_hook):
        staging = self._stage(name)
        target = staging / 'upgrade.py'
        outside = self.root / (name + '-outside')
        outside.write_bytes(target.read_bytes())
        outside.chmod(0o600)
        held = self.root / (name + '-held')
        original = target.read_bytes()
        original_inode = target.lstat().st_ino
        if action == 'static-symlink':
            target.rename(held)
            target.symlink_to(outside)
        elif action == 'static-exec':
            target.chmod(0o700)
        elif action == 'static-unreadable':
            target.chmod(0)
        elif action == 'static-tamper':
            target.write_bytes(b'SYNTHETIC WRONG BYTES')
        state = {'hook': False, 'mutation': None}

        def mutate():
            if action == 'symlink-before':
                target.rename(held)
                target.symlink_to(outside)
            elif action == 'replace-exec-before':
                target.rename(held)
                outside.rename(target)
                target.chmod(0o700)
            elif action == 'replace-after':
                target.rename(held)
                outside.rename(target)
            elif action == 'chmod-after':
                target.chmod(0o700)
            state['mutation'] = self._metadata(self.root)

        before = self._metadata(self.root)
        fds = self._fds()
        outcome = self._verify_hooked(
            staging, target, executable, state, mutate,
            before_open=action in ('symlink-before', 'replace-exec-before'),
            after_open=action in ('replace-after', 'chmod-after'))
        self.assertEqual(self._metadata(self.root), state['mutation'] or before)
        self.assertEqual(self._fds(), fds)
        self.assertEqual(state['hook'], expect_hook)
        self._assert_member_outcome(staging, expected, outcome)
        if action in ('replace-exec-before', 'replace-after'):
            self.assertEqual(held.read_bytes(), original)
            self.assertEqual(target.read_bytes(), original)
            self.assertNotEqual(held.lstat().st_ino, target.lstat().st_ino)
            self.assertNotEqual(target.lstat().st_ino, original_inode)
        if action == 'chmod-after':
            self.assertEqual(target.lstat().st_ino, original_inode)
            self.assertTrue(stat.S_IMODE(target.lstat().st_mode) & 0o111)
        forbidden = {'approval.json', 'host-profile.json', 'registration.json', 'pin.json'}
        self.assertTrue(forbidden.isdisjoint(path.name for path in self.root.rglob('*')))

    def _verify_hooked(self, staging, target, executable, state, mutate, *, before_open, after_open):
        real_path_open, real_os_open = Path.open, os.open

        def intercept(path, real_call, open_kwargs):
            if not _entry_matches(path, target, open_kwargs) or state['hook']:
                return real_call()
            state['hook'] = True
            if before_open:
                mutate()
            opened = real_call()
            if after_open:
                mutate()
            return opened

        def path_open(path, *args, **kwargs):
            return intercept(path, lambda: real_path_open(path, *args, **kwargs), kwargs)

        def os_open(path, *args, **kwargs):
            return intercept(path, lambda: real_os_open(path, *args, **kwargs), kwargs)

        Path.open = path_open
        os.open = os_open
        try:
            try:
                return self.bundle.verify(staging, executable=executable)
            except self.bundle.BundleError as error:
                return error
            except OSError as error:
                self.fail(type(error).__name__ + ' leaked from member access')
        finally:
            Path.open = real_path_open
            os.open = real_os_open

    def _assert_member_outcome(self, staging, expected, outcome):
        if isinstance(outcome, self.bundle.BundleError):
            self.assertEqual(outcome.code, expected)
            self.assertEqual(outcome.exit_code, 69)
            self.assertEqual(str(outcome), expected)
            self.assertNotIn(str(self.root), str(outcome))
            self.assertNotIn('SYNTHETIC', str(outcome))
            return
        self.assertIsNone(expected)
        manifest = staging / 'manifest.json'
        raw = manifest.read_bytes()
        self.assertEqual(outcome, {
            'verified': True, 'installable': False,
            'manifest_sha256': hashlib.sha256(raw).hexdigest(),
        })

    def _stage(self, name):
        staging = self.root / name
        self.bundle.build(self.source, staging)
        return staging

    def _metadata(self, root):
        rows = []
        for path in sorted(root.rglob('*'), key=lambda item: item.as_posix()):
            info = path.lstat()
            link = os.readlink(path) if stat.S_ISLNK(info.st_mode) else None
            rows.append((str(path.relative_to(root)), stat.S_IFMT(info.st_mode),
                         stat.S_IMODE(info.st_mode), info.st_uid, info.st_dev, info.st_ino,
                         info.st_size, info.st_mtime_ns, info.st_ctime_ns, link))
        return tuple(rows)

    def _fds(self):
        base = '/proc/self/fd' if os.path.isdir('/proc/self/fd') else '/dev/fd'
        return frozenset(os.listdir(base))

    def _expect(self, staging, expected, executable=True):
        fds = self._fds()
        before = self._metadata(self.root)
        try:
            try:
                outcome = self.bundle.verify(staging, executable=executable)
            except self.bundle.BundleError as error:
                outcome = error
            except OSError as error:
                self.fail(type(error).__name__ + ' leaked from member access')
        finally:
            self.assertEqual(self._fds(), fds)
            self.assertEqual(self._metadata(self.root), before)
        self._assert_member_outcome(staging, expected, outcome)
        forbidden = {'approval.json', 'host-profile.json', 'registration.json', 'pin.json'}
        self.assertTrue(forbidden.isdisjoint(path.name for path in self.root.rglob('*')))

    def _expect_unopened(self, staging, target, expected, executable=True):
        real_open, real_path_open = os.open, Path.open
        opened = []

        def os_open(path, *args, **kwargs):
            if _entry_matches(path, target, kwargs):
                opened.append('os')
            return real_open(path, *args, **kwargs)

        def path_open(path, *args, **kwargs):
            if _entry_matches(path, target, kwargs):
                opened.append('path')
            return real_path_open(path, *args, **kwargs)

        os.open = os_open
        Path.open = path_open
        try:
            self._expect(staging, expected, executable)
        finally:
            os.open = real_open
            Path.open = real_path_open
        self.assertEqual(opened, [])

    def _prepare_static(self, target, kind):
        raw = target.read_bytes()
        outside = self.root / (target.parent.name + '-outside')
        if kind == 'absolute':
            outside.write_bytes(raw)
            outside.chmod(0o600)
            target.unlink()
            target.symlink_to(outside)
        elif kind == 'relative':
            outside.write_bytes(raw)
            outside.chmod(0o600)
            target.unlink()
            target.symlink_to(Path('..') / outside.name)
        elif kind == 'internal':
            target.unlink()
            target.symlink_to(self.bundle.REQUIRED[0])
        elif kind == 'dangling':
            target.unlink()
            target.symlink_to('missing-member-target')
        elif kind == 'chain':
            inner = self.root / (target.parent.name + '-inner')
            outer = self.root / (target.parent.name + '-outer')
            inner.write_bytes(raw)
            inner.chmod(0o600)
            outer.symlink_to(inner)
            target.unlink()
            target.symlink_to(Path('..') / outer.name)
        elif kind == 'directory':
            target.unlink()
            target.mkdir(mode=0o700)
        elif kind == '0600':
            target.chmod(0o600)
        elif kind == '0644':
            target.chmod(0o644)
        elif kind == '0700':
            target.chmod(0o700)
        elif kind == 'tamper':
            target.write_bytes(b'SYNTHETIC WRONG BYTES')
        elif kind == 'unlink':
            target.unlink()
        else:
            self.fail('unknown static kind ' + kind)

    def _replace_before(self, target, kind):
        label = kind.removesuffix('-optout')
        raw = target.read_bytes()
        root = target.parent.parent
        name = target.parent.name
        outside = root / (name + '-outside')
        held = root / (name + '-held')
        if label in ('absolute', 'relative', 'chain', 'same-bytes'):
            outside.write_bytes(raw)
            outside.chmod(0o644 if label == 'same-bytes' else 0o600)
        target.rename(held)
        if label == 'absolute':
            target.symlink_to(outside)
        elif label == 'relative':
            target.symlink_to(Path('..') / outside.name)
        elif label == 'internal':
            target.symlink_to(self.bundle.REQUIRED[0])
        elif label == 'dangling':
            target.symlink_to('missing-member-target')
        elif label == 'chain':
            inner = root / (name + '-inner')
            outer = root / (name + '-outer')
            inner.write_bytes(raw)
            inner.chmod(0o600)
            outer.symlink_to(inner)
            target.symlink_to(Path('..') / outer.name)
        elif label == 'directory':
            target.mkdir(mode=0o700)
        elif label == 'same-bytes':
            outside.rename(target)
        else:
            self.fail('unknown replacement ' + kind)

    def _race(self, name, expected, executable, mutate, *, when):
        staging = self._stage(name)
        target = staging / 'upgrade.py'
        state = {'hook': False, 'mutation': None}

        def run_mutate():
            mutate(target)
            state['mutation'] = self._metadata(self.root)

        before = self._metadata(self.root)
        fds = self._fds()
        outcome = self._verify_hooked(
            staging, target, executable, state, run_mutate,
            before_open=when == 'before', after_open=when == 'after')
        self.assertEqual(self._metadata(self.root), state['mutation'] or before)
        self.assertEqual(self._fds(), fds)
        self.assertTrue(state['hook'])
        self._assert_member_outcome(staging, expected, outcome)
        return staging, target

    def _missing_after_listing(self):
        staging = self._stage('missing-after-list')
        target = staging / 'upgrade.py'
        real_scandir, real_listdir = os.scandir, os.listdir
        state = {'mutation': None}
        snap = self._metadata
        root = self.root
        staging_path = os.path.normpath(os.fspath(staging))

        def matches(path):
            if isinstance(path, int):
                info = os.fstat(path)
                held = os.lstat(staging)
                return (stat.S_ISDIR(info.st_mode) and not stat.S_ISLNK(info.st_mode)
                        and (info.st_dev, info.st_ino) == (held.st_dev, held.st_ino))
            return os.path.normpath(os.fspath(path)) == staging_path

        def drop():
            if state.get('dropped'):
                return
            state['dropped'] = True
            target.unlink()
            state['mutation'] = snap(root)

        def wrapped_listdir(path, *args, **kwargs):
            names = real_listdir(path, *args, **kwargs)
            if matches(path):
                drop()
            return names

        def wrapped_scandir(path, *args, **kwargs):
            inner = real_scandir(path, *args, **kwargs)
            if not matches(path):
                return inner

            class _Scan:
                def __enter__(self):
                    inner.__enter__()
                    return self

                def __exit__(self, exc_type, exc, tb):
                    try:
                        return inner.__exit__(exc_type, exc, tb)
                    finally:
                        drop()

                def __iter__(self):
                    return iter(inner)

            return _Scan()

        os.scandir = wrapped_scandir
        os.listdir = wrapped_listdir
        fds = self._fds()
        try:
            try:
                outcome = self.bundle.verify(staging)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            os.scandir = real_scandir
            os.listdir = real_listdir
            self.assertEqual(self._fds(), fds)
        self.assertIsNotNone(state['mutation'])
        self.assertEqual(self._metadata(self.root), state['mutation'])
        self.assertIsInstance(outcome, self.bundle.BundleError)
        self.assertEqual(outcome.code, 'member_missing')
        self.assertEqual(outcome.exit_code, 69)
        self.assertEqual(str(outcome), 'member_missing')

    def _during_read(self, kind):
        staging = self._stage('during-' + kind)
        target = staging / 'upgrade.py'
        original = target.read_bytes()
        inode = target.lstat().st_ino
        state = {'done': False, 'read': 0, 'requests': [], 'fds': set(), 'mutation': None, 'flags': 0}
        real_open, real_read, real_close = os.open, os.read, os.close

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs):
                state['fds'].add(fd)
                state['flags'] = flags
            return fd

        def os_close(fd):
            state['fds'].discard(fd)
            return real_close(fd)

        def mutate():
            if kind == 'rewrite':
                writer = real_open(target, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    self._write_all(writer, b'X' * len(original))
                finally:
                    os.close(writer)
            elif kind == 'truncate':
                writer = real_open(target, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    os.ftruncate(writer, 0)
                finally:
                    os.close(writer)
            elif kind == 'chmod':
                os.chmod(target, 0o700)
            else:
                writer = real_open(target, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    os.lseek(writer, 0, os.SEEK_END)
                    self._write_all(writer, b'Y' * (1024 * 1024))
                finally:
                    os.close(writer)
            state['mutation'] = self._metadata(self.root)

        def os_read(fd, size):
            data = real_read(fd, size)
            if fd in state['fds']:
                state['requests'].append(size)
                if not state['done']:
                    state['done'] = True
                    mutate()
                state['read'] += len(data)
            return data

        os.open = os_open
        os.read = os_read
        os.close = os_close
        fds = self._fds()
        try:
            try:
                outcome = self.bundle.verify(staging)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            os.open = real_open
            os.read = real_read
            os.close = real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertIsNotNone(state['mutation'])
        self.assertEqual(self._metadata(self.root), state['mutation'])
        self.assertTrue(state['flags'] & os.O_NOFOLLOW)
        self.assertTrue(state['flags'] & os.O_NONBLOCK)
        self.assertTrue(state['requests'])
        self.assertLessEqual(max(state['requests']), self.bundle._MEMBER_CHUNK)
        self.assertLessEqual(state['read'], len(original) + 1)
        self.assertIsInstance(outcome, self.bundle.BundleError)
        self.assertEqual(outcome.code, 'member_changed')
        self.assertEqual(outcome.exit_code, 69)
        self.assertEqual(str(outcome), 'member_changed')
        self.assertEqual(target.lstat().st_ino, inode)
        if kind == 'grow':
            self.assertEqual(state['read'], len(original) + 1)
            self.assertEqual(target.stat().st_size, len(original) + 1024 * 1024)
        elif kind == 'rewrite':
            self.assertEqual(target.read_bytes(), b'X' * len(original))
        elif kind == 'truncate':
            self.assertEqual(target.stat().st_size, 0)
        else:
            self.assertTrue(stat.S_IMODE(target.lstat().st_mode) & 0o111)

    def _observe_member_read(self, staging, target, size):
        state = {'read': 0, 'requests': [], 'flags': 0, 'path_open': False, 'fds': set()}
        real_open, real_read, real_close, real_path_open = os.open, os.read, os.close, Path.open

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs):
                state['fds'].add(fd)
                state['flags'] = flags
            return fd

        def os_close(fd):
            state['fds'].discard(fd)
            return real_close(fd)

        def os_read(fd, request):
            if fd in state['fds']:
                state['requests'].append(request)
                self.assertLessEqual(request, self.bundle._MEMBER_CHUNK)
            data = real_read(fd, request)
            if fd in state['fds']:
                state['read'] += len(data)
            return data

        def path_open(path, *args, **kwargs):
            if Path(path) == target:
                state['path_open'] = True
            return real_path_open(path, *args, **kwargs)

        os.open, os.read, os.close, Path.open = os_open, os_read, os_close, path_open
        fds = self._fds()
        try:
            try:
                outcome = self.bundle.verify(staging)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            os.open, os.read, os.close, Path.open = real_open, real_read, real_close, real_path_open
            self.assertEqual(self._fds(), fds)
        self.assertFalse(state['path_open'])
        self.assertTrue(state['flags'] & os.O_NOFOLLOW)
        self.assertTrue(state['flags'] & os.O_NONBLOCK)
        self.assertFalse(state['flags'] & (os.O_WRONLY | os.O_RDWR))
        self.assertIn(self.bundle._MEMBER_CHUNK, state['requests'])
        self.assertEqual(state['read'], size)
        self.assertLessEqual(state['read'], size + 1)
        return outcome

    def _short_read(self):
        staging = self._stage('short-read')
        target = staging / 'upgrade.py'
        state = {'fds': set(), 'short': False}
        real_open, real_read = os.open, os.read

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs):
                state['fds'].add(fd)
            return fd

        def os_read(fd, size):
            if fd in state['fds'] and not state['short'] and size > 1:
                data = real_read(fd, size)
                if len(data) > 1:
                    state['short'] = True
                    os.lseek(fd, 1 - len(data), os.SEEK_CUR)
                    return data[:1]
            return real_read(fd, size)

        os.open = os_open
        os.read = os_read
        try:
            self._expect(staging, None)
        finally:
            os.open = real_open
            os.read = real_read
        self.assertTrue(state['short'])

    def _eintr(self, where):
        staging = self._stage('eintr-' + where)
        target = staging / 'upgrade.py'
        state = {'fds': set(), 'once': False}
        real_open, real_read = os.open, os.read

        def os_open(path, flags, *args, **kwargs):
            if where == 'open' and _entry_matches(path, target, kwargs) and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted')
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs):
                state['fds'].add(fd)
            return fd

        def os_read(fd, size):
            if where == 'read' and fd in state['fds'] and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted')
            return real_read(fd, size)

        os.open = os_open
        os.read = os_read
        try:
            self._expect(staging, None)
        finally:
            os.open = real_open
            os.read = real_read
        self.assertTrue(state['once'])

    def _hide_flag(self, name, flag, symlink):
        staging = self._stage(name)
        target = staging / 'upgrade.py'
        if symlink:
            outside = self.root / (name + '-outside')
            outside.write_bytes(target.read_bytes())
            outside.chmod(0o600)
            target.unlink()
            target.symlink_to(outside)
        saved = getattr(os, flag)
        real_stat, real_open, real_path_open = os.stat, os.open, Path.open
        opened = []

        def wrapped_stat(path, *args, **kwargs):
            if _entry_matches(path, target, kwargs) and hasattr(os, flag):
                delattr(os, flag)
            return real_stat(path, *args, **kwargs)

        def os_open(path, *args, **kwargs):
            if _entry_matches(path, target, kwargs):
                opened.append('os')
            return real_open(path, *args, **kwargs)

        def path_open(path, *args, **kwargs):
            if _entry_matches(path, target, kwargs):
                opened.append('path')
            return real_path_open(path, *args, **kwargs)

        before = self._metadata(self.root)
        fds = self._fds()
        os.stat, os.open, Path.open = wrapped_stat, os_open, path_open
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            os.stat, os.open, Path.open = real_stat, real_open, real_path_open
            setattr(os, flag, saved)
            self.assertEqual(self._metadata(self.root), before)
            self.assertEqual(self._fds(), fds)
        self.assertEqual(caught.exception.code, 'member_type_rejected')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(str(caught.exception), 'member_type_rejected')
        self.assertEqual(opened, [])

    def _inject_open(self, number, expected):
        staging = self._stage('open-' + str(number))
        target = staging / 'upgrade.py'
        real_open = os.open
        state = {'done': False}

        def os_open(path, flags, *args, **kwargs):
            if _entry_matches(path, target, kwargs) and not state['done']:
                state['done'] = True
                raise OSError(number, 'injected seam')
            return real_open(path, flags, *args, **kwargs)

        os.open = os_open
        try:
            self._expect(staging, expected)
        finally:
            os.open = real_open
        self.assertTrue(state['done'])

    def _inject_member_io(self, where, expected):
        staging = self._stage(where + '-eio')
        target = staging / 'upgrade.py'
        real_open, real_read, real_fstat = os.open, os.read, os.fstat
        state = {'fds': set(), 'done': False}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs):
                state['fds'].add(fd)
            return fd

        def os_read(fd, size):
            if where == 'read' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EIO, 'injected seam')
            return real_read(fd, size)

        def os_fstat(fd):
            if where == 'fstat' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EIO, 'injected seam')
            return real_fstat(fd)

        os.open, os.read, os.fstat = os_open, os_read, os_fstat
        try:
            self._expect(staging, expected)
        finally:
            os.open, os.read, os.fstat = real_open, real_read, real_fstat
        self.assertTrue(state['done'])

    def _inject_programming_error(self):
        staging = self._stage('programming-error')
        target = staging / 'upgrade.py'
        real_open, real_fstat = os.open, os.fstat
        state = {'fds': set(), 'done': False}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs):
                state['fds'].add(fd)
            return fd

        def os_fstat(fd):
            if fd in state['fds'] and not state['done']:
                state['done'] = True
                raise RuntimeError('programming-error')
            return real_fstat(fd)

        os.open, os.fstat = os_open, os_fstat
        fds = self._fds()
        try:
            with self.assertRaises(RuntimeError) as caught:
                self.bundle.verify(staging)
        finally:
            os.open, os.fstat = real_open, real_fstat
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(str(caught.exception), 'programming-error')

    def _fifo_child(self, mode, staging):
        try:
            completed = subprocess.run(
                [sys.executable, '-B', '-c',
                 'import sys; sys.path.insert(0, sys.argv[1]); '
                 'import test_upgrade_bundle as tests; '
                 'raise SystemExit(tests._verify_fifo_member(sys.argv[2], sys.argv[3], sys.argv[4]))',
                 str(HERE), mode, str(staging), 'upgrade.py'],
                cwd=self.root, timeout=5, capture_output=True, text=True,
                env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        except subprocess.TimeoutExpired:
            self.fail('fifo member verify was still blocked after 5s')
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertNotIn('SYNTHETIC', completed.stdout)
        self.assertNotIn(str(self.root), completed.stdout)

    def _write_all(self, fd, data):
        view = memoryview(data)
        while view:
            wrote = os.write(fd, view)
            if wrote <= 0:
                self.fail('fixture write stalled')
            view = view[wrote:]


def _build_source_fifo(mode, source, staging, name):
    """Subprocess entry. A source fifo must be refused without waiting for a writer."""
    import upgrade_bundle as bundle
    source, staging = Path(source), Path(staging)
    target = source / name
    state = {'done': False}
    if mode == 'static':
        os.rename(target, source.parent / 'fifo-static-held')
        os.mkfifo(target, 0o600)
    elif mode == 'race':
        real_open, real_path_open = os.open, Path.open

        def swap(path):
            if state['done'] or isinstance(path, int):
                return
            if os.path.normpath(os.fspath(path)) != str(target):
                return
            state['done'] = True
            os.rename(target, source.parent / 'fifo-race-held')
            os.mkfifo(target, 0o600)

        def os_open(path, flags, *args, **kwargs):
            swap(path)
            return real_open(path, flags, *args, **kwargs)

        def path_open(path, *args, **kwargs):
            swap(path)
            return real_path_open(path, *args, **kwargs)

        os.open, Path.open = os_open, path_open
    code = None
    try:
        bundle.build(source, staging)
    except bundle.BundleError as error:
        code = error.code if error.exit_code == 69 and str(error) == error.code else 'bad-error'
    except OSError as error:
        code = type(error).__name__
    if mode == 'race' and not state['done']:
        sys.stdout.write('hook-not-used\n')
        return 4
    if code == 'member_missing' and not (staging / 'manifest.json').exists():
        return 0
    sys.stdout.write((code or 'accepted') + '\n')
    return 2


class BuilderSourceTests(unittest.TestCase):
    """Builder source descriptor contract. Fixtures are inert and never executed.

    Race and IO seams call the real open, read, and fstat. Injected errno values
    are error-mapping tests, not hardware faults.
    """

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        actual = Path(bundle.__file__).resolve()
        expected = (HERE / 'upgrade_bundle.py').resolve()
        print(f'upgrade_bundle={actual}', file=sys.stderr)
        self.assertEqual(actual, expected, 'refusing to test an imported module outside this tree')
        self.assertNotEqual(os.getuid(), 0)
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-source-')
        self.addCleanup(self.tmp.cleanup)
        self.addCleanup(self._relax_tree)
        self.root = Path(self.tmp.name).resolve()
        self.root.chmod(0o700)

    def test_recorded_builder_source_contract(self):
        """Eight source-window failures plus four controls. Hooks call the real open."""
        cases = (
            ('regular', 'none', None, False),
            ('source-executable', 'executable-source', None, False),
            ('static-symlink', 'static-symlink', 'member_missing', False),
            ('unreadable-source', 'unreadable', 'source_unreadable', False),
            ('symlink-before-read', 'symlink-before', 'member_missing', False),
            ('regular-before-read', 'regular-before', 'source_changed', False),
            ('path-after-open', 'path-after', 'source_changed', False),
            ('chmod-after-open', 'chmod-after', 'source_changed', False),
            ('truncate-after-open', 'truncate-after', 'source_changed', False),
            ('fallback-regular', 'none', None, True),
            ('fallback-symlink-before-read', 'symlink-before', 'member_missing', True),
            ('dangling-primary-with-fallback', 'dangling-primary', 'member_missing', False),
        )
        for label, action, expected, fallback in cases:
            with self.subTest(case=label):
                self._recorded(label, action, expected, fallback)

    def test_source_modes_hardlink_and_stable_manifest(self):
        payload = b'STABLE SOURCE BYTES\n'
        hashes = {}
        for mode in (0o600, 0o644, 0o700, 0o755):
            with self.subTest(mode=oct(mode)):
                source = self._fill('mode-' + oct(mode)[2:])
                target = source / self.bundle.REQUIRED[0]
                target.write_bytes(payload)
                target.chmod(mode)
                before = self._identity(target)
                staging = self.root / ('mode-staging-' + oct(mode)[2:])
                built = self._build_ok(source, staging)
                self.assertEqual(self._identity(target), before)
                self.assertEqual(stat.S_IMODE(target.lstat().st_mode), mode)
                staged = staging / self.bundle.REQUIRED[0]
                self.assertEqual(staged.read_bytes(), payload)
                self.assertEqual(stat.S_IMODE(staged.lstat().st_mode), 0o644)
                self.assertFalse(stat.S_IMODE(staged.lstat().st_mode) & 0o111)
                hashes[mode] = built['manifest_sha256']
        self.assertEqual(len(set(hashes.values())), 1)

        source = self._fill('hardlink')
        target = source / self.bundle.REQUIRED[0]
        payload = target.read_bytes()
        linked = self.root / 'hardlink-bytes'
        linked.write_bytes(payload)
        linked.chmod(0o644)
        target.unlink()
        os.link(linked, target)
        self.assertGreaterEqual(target.lstat().st_nlink, 2)
        before = self._identity(linked)
        staging = self.root / 'hardlink-staging'
        self._build_ok(source, staging)
        self.assertEqual((staging / self.bundle.REQUIRED[0]).read_bytes(), payload)
        self.assertEqual(stat.S_IMODE((staging / self.bundle.REQUIRED[0]).lstat().st_mode), 0o644)
        self.assertEqual(self._identity(linked), before)
        self.assertGreaterEqual(target.lstat().st_nlink, 2)

    def test_static_source_types_refuse_without_following(self):
        kinds = ('absolute', 'relative', 'internal', 'external', 'dangling', 'chain', 'directory')
        for kind in kinds:
            with self.subTest(kind=kind):
                source = self._fill('static-' + kind)
                target = source / self.bundle.REQUIRED[0]
                outside = self._outside('static-' + kind)
                raw = target.read_bytes()
                outside.write_bytes(raw)
                self._make_nonregular(target, kind, outside)
                opened = []
                real_open, real_path_open = os.open, Path.open

                def os_open(path, flags, *args, **kwargs):
                    if not isinstance(path, int):
                        opened.append(os.path.normpath(os.fspath(path)))
                    return real_open(path, flags, *args, **kwargs)

                def path_open(path, *args, **kwargs):
                    opened.append(os.path.normpath(os.fspath(path)))
                    return real_path_open(path, *args, **kwargs)

                before = self._identity(outside)
                os.open, Path.open = os_open, path_open
                try:
                    self._refuse(source, self.root / ('static-staging-' + kind), 'member_missing')
                finally:
                    os.open, Path.open = real_open, real_path_open
                self.assertNotIn(str(outside), opened)
                self.assertEqual(outside.read_bytes(), raw)
                self.assertEqual(self._identity(outside), before)

    def test_fifo_source_does_not_block(self):
        for mode in ('static', 'race'):
            with self.subTest(mode=mode):
                source = self._fill('fifo-' + mode)
                staging = self.root / ('fifo-staging-' + mode)
                name = self.bundle.REQUIRED[0]
                held_name = 'fifo-static-held' if mode == 'static' else 'fifo-race-held'
                original = (source / name).read_bytes()
                self._fifo_child(mode, source, staging, name)
                held = self.root / held_name
                self.assertEqual(held.read_bytes(), original)
                self.assertTrue(stat.S_ISFIFO((source / name).lstat().st_mode))
                self.assertFalse((staging / 'manifest.json').exists())

    def test_fallback_selection_rules(self):
        primary_bytes = b'PRIMARY RELEASE SOURCE\n'
        fallback_bytes = b'FALLBACK RELEASE SOURCE\n'
        source = self._fill('fallback-primary-wins')
        self._write_fallback(source, fallback_bytes)
        primary = source / 'release-source.json'
        primary.write_bytes(primary_bytes)
        primary.chmod(0o600)
        staging = self.root / 'fallback-primary-staging'
        self._build_ok(source, staging)
        self.assertEqual((staging / 'release-source.json').read_bytes(), primary_bytes)

        source = self._fill('fallback-missing-primary')
        primary = source / 'release-source.json'
        primary.unlink()
        fallback = self._write_fallback(source, fallback_bytes)
        before = self._identity(fallback)
        staging = self.root / 'fallback-missing-staging'
        self._build_ok(source, staging)
        self.assertEqual((staging / 'release-source.json').read_bytes(), fallback_bytes)
        self.assertEqual(self._identity(fallback), before)

        source = self._fill('fallback-dangling')
        self._write_fallback(source, fallback_bytes)
        primary = source / 'release-source.json'
        primary.unlink()
        primary.symlink_to('absent-primary-target')
        self._refuse(source, self.root / 'fallback-dangling-staging', 'member_missing')
        self.assertEqual(fallback.read_bytes(), fallback_bytes)

        source = self._fill('fallback-unreadable-primary')
        self._write_fallback(source, fallback_bytes)
        primary = source / 'release-source.json'
        primary.chmod(0)
        try:
            self._refuse(source, self.root / 'fallback-unreadable-staging', 'source_unreadable')
        finally:
            primary.chmod(0o600)
        self.assertEqual(fallback.read_bytes(), fallback_bytes)

        source = self._fill('fallback-directory-primary')
        self._write_fallback(source, fallback_bytes)
        primary = source / 'release-source.json'
        primary.unlink()
        primary.mkdir(mode=0o700)
        self._refuse(source, self.root / 'fallback-directory-staging', 'member_missing')

        source = self._fill('fallback-bad-type')
        primary = source / 'release-source.json'
        primary.unlink()
        fallback = self._write_fallback(source, fallback_bytes)
        fallback.unlink()
        fallback.symlink_to(self.root / 'absent-fallback')
        self._refuse(source, self.root / 'fallback-bad-type-staging', 'member_missing')

        source = self._fill('fallback-unreadable')
        primary = source / 'release-source.json'
        primary.unlink()
        fallback = self._write_fallback(source, fallback_bytes)
        fallback.chmod(0)
        try:
            self._refuse(source, self.root / 'fallback-unreadable-file-staging', 'source_unreadable')
        finally:
            fallback.chmod(0o600)

        source = self._fill('fallback-absent')
        primary = source / 'release-source.json'
        primary.unlink()
        stale = source.parent / 'fork' / 'release-source.json'
        if stale.is_symlink() or stale.exists():
            stale.unlink()
        self._refuse(source, self.root / 'fallback-absent-staging', 'member_missing')

        source = self._fill('selected-primary-disappears')
        fallback = self._write_fallback(source, fallback_bytes)
        self._replace_on_open(source, source / 'release-source.json', 'disappear', 'member_missing',
                              forbidden=fallback)
        source = self._fill('selected-fallback-disappears')
        primary = source / 'release-source.json'
        primary.unlink()
        fallback = self._write_fallback(source, fallback_bytes)
        self._replace_on_open(source, fallback, 'disappear', 'member_missing', forbidden=primary)

    def test_replacement_before_open_refuses_type_and_identity(self):
        for kind in ('absolute', 'relative', 'internal', 'external', 'dangling', 'chain', 'directory', 'same-bytes'):
            expected = 'source_changed' if kind == 'same-bytes' else 'member_missing'
            with self.subTest(kind=kind):
                source = self._fill('before-' + kind)
                target = source / self.bundle.REQUIRED[0]
                self._replace_on_open(source, target, kind, expected)

    def test_changes_after_open_and_during_read(self):
        source = self._fill('after-path')
        self._replace_on_open(source, source / self.bundle.REQUIRED[0], 'path-after', 'source_changed')
        source = self._fill('after-unlink')
        self._replace_on_open(source, source / self.bundle.REQUIRED[0], 'unlink-after', 'source_changed')
        for kind in ('rewrite', 'truncate', 'chmod', 'grow'):
            with self.subTest(kind=kind):
                source = self._fill('during-' + kind)
                self._during_read(source, kind)

    def test_streamed_copy_bounds_reads_and_matches_bytes(self):
        sizes = (0, 65535, 65536, 65537, 2 * 1024 * 1024)
        marker = bytes((0, 255, 10, 13))
        for size in sizes:
            with self.subTest(size=size):
                source = self._fill('stream-' + str(size))
                payload = (marker * ((size // len(marker)) + 1))[:size]
                self.assertEqual(len(payload), size)
                if size:
                    self.assertIn(0, payload)
                    self.assertIn(255, payload)
                target = source / self.bundle.REQUIRED[0]
                target.write_bytes(payload)
                target.chmod(0o600)
                before = self._identity(target)
                staging = self.root / ('stream-staging-' + str(size))
                observed = self._observe_source_io(source, staging, target, size)
                built = observed['outcome']
                self.assertEqual(self._identity(target), before)
                staged = staging / self.bundle.REQUIRED[0]
                self.assertEqual(staged.read_bytes(), payload)
                self.assertEqual(staged.stat().st_size, size)
                self.assertEqual(stat.S_IMODE(staged.lstat().st_mode), 0o644)
                manifest = json.loads((staging / 'manifest.json').read_bytes())
                self.assertEqual(manifest['files'][self.bundle.REQUIRED[0]], hashlib.sha256(payload).hexdigest())
                self._assert_verified(staging, built)
                self.assertFalse(observed['path_open'])
                self.assertEqual(observed['opens'], 1)
                self.assertGreaterEqual(observed['fstats'], 2)
                self.assertEqual(observed['flags'], os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
                self.assertTrue(observed['requests'])
                self.assertLessEqual(max(observed['requests']), self.bundle._MEMBER_CHUNK)
                self.assertLessEqual(observed['read'], size + 1)
                self.assertEqual(observed['read'], size)
                self.assertIn(1, observed['requests'])
                self._assert_closed(observed['closed'])

    def test_short_read_and_eintr_continue(self):
        source = self._fill('short-read')
        self._short_read(source)
        for where in ('open', 'read', 'fstat'):
            with self.subTest(where=where):
                source = self._fill('eintr-' + where)
                self._eintr(source, where)

    def test_missing_open_flags_do_not_follow(self):
        for flag, symlink in (('O_NOFOLLOW', True), ('O_NOFOLLOW', False), ('O_NONBLOCK', True), ('O_NONBLOCK', False)):
            with self.subTest(flag=flag, symlink=symlink):
                source = self._fill('flag-' + flag + '-' + str(symlink).lower())
                target = source / self.bundle.REQUIRED[0]
                outside = self._outside('flag-' + flag + '-' + str(symlink).lower())
                outside.write_bytes(b'OUTSIDE FLAG BYTES\n')
                if symlink:
                    target.unlink()
                    target.symlink_to(outside)
                self._hide_flag(source, target, outside, flag)

    def test_source_io_error_mapping_is_not_a_hardware_fault(self):
        """Injected errno values cover the mapping. They are not hardware failures."""
        self._inject_open(errno.ENOENT, 'member_missing')
        self._inject_open(errno.ENOTDIR, 'member_missing')
        self._inject_open(errno.ELOOP, 'member_missing')
        self._inject_open(errno.EISDIR, 'member_missing')
        self._inject_open(errno.ENXIO, 'member_missing')
        self._inject_open(errno.EACCES, 'source_unreadable')
        self._inject_open(errno.EIO, 'source_unreadable')
        self._inject_lstat(errno.ENOTDIR, 'member_missing', release=False)
        self._inject_lstat(errno.EACCES, 'source_unreadable', release=True)
        self._inject_lstat(errno.ENOENT, 'member_missing', release=False)
        self._inject_member_io('read', errno.EIO, 'source_unreadable')
        self._inject_member_io('read', errno.EACCES, 'source_unreadable')
        self._inject_member_io('fstat', errno.EIO, 'source_unreadable')

    def test_programming_error_closes_source_fd(self):
        source = self._fill('programming-error')
        target = source / self.bundle.REQUIRED[0]
        real_open, real_fstat, real_close = os.open, os.fstat, os.close
        state = {'fds': set(), 'done': False, 'closed': []}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
            return fd

        def os_fstat(fd):
            if fd in state['fds'] and not state['done']:
                state['done'] = True
                raise RuntimeError('programming-error')
            return real_fstat(fd)

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        os.open, os.fstat, os.close = os_open, os_fstat, os_close
        fds = self._fds()
        try:
            with self.assertRaises(RuntimeError) as caught:
                self.bundle.build(source, self.root / 'programming-staging')
        finally:
            os.open, os.fstat, os.close = real_open, real_fstat, real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(str(caught.exception), 'programming-error')
        self._assert_closed(state['closed'])
        self.assertFalse((self.root / 'programming-staging' / 'manifest.json').exists())

    def test_destination_write_failure_is_not_source_unreadable(self):
        """Destination seam only. This is not a source-device failure and must not change source selection."""
        source = self._fill('dest-write')
        primary = source / 'release-source.json'
        primary_bytes = b'PRIMARY DEST FAILURE\n'
        fallback_bytes = b'FALLBACK DEST FAILURE\n'
        primary.write_bytes(primary_bytes)
        primary.chmod(0o600)
        fallback = self._write_fallback(source, fallback_bytes)
        before = self._identity(primary)
        fallback_before = self._identity(fallback)
        dest = self.root / 'dest-write-staging' / 'release-source.json'
        real_open, real_write = os.open, os.write
        state = {'dest': set(), 'injected': False, 'opened': []}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int):
                state['opened'].append(os.path.normpath(os.fspath(path)))
                if self._same(path, dest, kwargs):
                    state['dest'].add(fd)
            return fd

        def os_write(fd, data):
            if fd in state['dest'] and not state['injected']:
                state['injected'] = True
                raise OSError(errno.EIO, 'injected destination seam')
            return real_write(fd, data)

        os.open, os.write = os_open, os_write
        fds = self._fds()
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, self.root / 'dest-write-staging')
        finally:
            os.open, os.write = real_open, real_write
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['injected'])
        self.assertEqual(caught.exception.errno, errno.EIO)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertNotEqual(getattr(caught.exception, 'code', None), 'source_unreadable')
        self.assertNotIn(str(fallback), state['opened'])
        self.assertFalse((self.root / 'dest-write-staging' / 'manifest.json').exists())
        self.assertEqual(self._identity(primary), before)
        self.assertEqual(self._identity(fallback), fallback_before)
        self.assertEqual(fallback.read_bytes(), fallback_bytes)

    def test_close_eio_after_real_write_is_not_published(self):
        """Close EIO after the bytes exist is not success. The errno is an injected mapping, not a device fault.

        A caller that is already handling another exception is not this copy's failure.
        Close EIO in that context must still refuse publication.
        """
        for role in ('destination', 'source'):
            for handled in (False, True):
                with self.subTest(role=role, handled=handled):
                    self._inject_delayed_close(role, handled=handled)

    def test_close_error_keeps_the_earlier_refusal(self):
        """A later close error must not replace the refusal that already happened, and must not close twice."""
        self._close_after_destination_write_error()
        self._close_after_programming_error()

    def test_refusal_does_not_publish_manifest_or_touch_sentinels(self):
        sentinel = self.root / 'sentinel'
        sentinel.write_bytes(b'SENTINEL\n')
        sentinel.chmod(0o600)
        outside = self._outside('sentinel-case')
        outside.write_bytes(b'OUTSIDE SENTINEL\n')
        before = (self._identity(sentinel), self._identity(outside))
        source = self._fill('refusal-partial')
        target = source / self.bundle.REQUIRED[1]
        raw = target.read_bytes()
        target.unlink()
        target.symlink_to('absent-second-member')
        source_before = self._tree_identity(source)
        staging = self.root / 'refusal-partial-staging'
        self._refuse(source, staging, 'member_missing')
        self.assertTrue((staging / self.bundle.REQUIRED[0]).is_file())
        self.assertFalse((staging / 'manifest.json').exists())
        with self.assertRaises(self.bundle.BundleError):
            self.bundle.verify(staging)
        self.assertEqual(self._tree_identity(source), source_before)
        self.assertEqual((self._identity(sentinel), self._identity(outside)), before)
        self.assertEqual(sentinel.read_bytes(), b'SENTINEL\n')
        self.assertEqual(outside.read_bytes(), b'OUTSIDE SENTINEL\n')
        self.assertNotEqual(raw, b'')

    def test_existing_staging_is_not_reused(self):
        source = self._fill('staging-exists')
        staging = self.root / 'staging-exists-dir'
        built = self._build_ok(source, staging)
        manifest = (staging / 'manifest.json').read_bytes()
        with self.assertRaises(self.bundle.BundleError) as caught:
            self.bundle.build(source, staging)
        self.assertEqual(caught.exception.code, 'staging_exists')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(str(caught.exception), 'staging_exists')
        self.assertEqual((staging / 'manifest.json').read_bytes(), manifest)
        self.assertEqual(hashlib.sha256(manifest).hexdigest(), built['manifest_sha256'])

    def _recorded(self, label, action, expected, fallback):
        root = self.root / label
        root.mkdir(mode=0o700)
        source = root / 'source'
        self._fill_at(source)
        name = 'release-source.json' if fallback or action == 'dangling-primary' else self.bundle.REQUIRED[0]
        primary = source / name
        target = primary
        original = primary.read_bytes()
        outside = root / 'synthetic-alternate'
        replacement = b'SYNTHETIC ALTERNATE BYTES\n'
        outside.write_bytes(replacement)
        outside.chmod(0o600)
        if fallback or action == 'dangling-primary':
            fork = root / 'fork'
            fork.mkdir(mode=0o700)
            target = fork / name
            target.write_bytes(original)
            target.chmod(0o600)
            primary.rename(root / 'held-primary')
            if action == 'dangling-primary':
                primary.symlink_to('absent-synthetic-target')
        if action == 'static-symlink':
            target.rename(root / 'held-static')
            target.symlink_to(outside)
        elif action == 'unreadable':
            target.chmod(0)
        elif action == 'executable-source':
            target.chmod(0o700)
        before = self._identity(target)
        outside_before = self._identity(outside)
        state = {'hook_count': 0, 'injected': None}
        real_path_open, real_open = Path.open, os.open
        raced = action in ('symlink-before', 'regular-before', 'path-after', 'chmod-after', 'truncate-after')

        def mutate():
            state['hook_count'] += 1
            if action == 'symlink-before':
                target.rename(root / 'held-before')
                target.symlink_to(outside)
            elif action in ('regular-before', 'path-after'):
                target.rename(root / 'held-replaced')
                target.write_bytes(original)
                target.chmod(0o600)
            elif action == 'chmod-after':
                target.chmod(0o700)
            elif action == 'truncate-after':
                with real_path_open(target, 'wb') as output:
                    output.write(replacement)
            else:
                raise AssertionError(action)
            state['injected'] = self._identity(target)

        def selected(path):
            return not isinstance(path, int) and os.path.normpath(os.fspath(path)) == str(target)

        def around(open_fn, path, *args, **kwargs):
            hit = selected(path) and state['hook_count'] == 0 and raced
            if hit and action.endswith('-before'):
                mutate()
            opened = open_fn(path, *args, **kwargs)
            if hit and action.endswith('-after'):
                mutate()
            return opened

        def path_open(path, *args, **kwargs):
            return around(real_path_open, path, *args, **kwargs)

        def os_open(path, *args, **kwargs):
            return around(real_open, path, *args, **kwargs)

        staging = root / 'staging'
        Path.open, os.open = path_open, os_open
        try:
            try:
                built = self.bundle.build(source, staging)
            except self.bundle.BundleError as exc:
                outcome = ('BundleError', exc.code, exc.exit_code, str(exc), exc.__cause__)
            except OSError as exc:
                outcome = (type(exc).__name__, getattr(exc, 'errno', None))
            else:
                verified = self.bundle.verify(staging)
                copied = (staging / name).read_bytes()
                outcome = ('success', verified['verified'], verified['installable'],
                           verified['manifest_sha256'] == built['manifest_sha256'],
                           copied == original, copied == replacement,
                           oct(stat.S_IMODE((staging / name).lstat().st_mode)))
            self.assertEqual(self._identity(target), state['injected'] or before)
            self.assertEqual(self._identity(outside), outside_before)
        finally:
            Path.open, os.open = real_path_open, real_open
            if action == 'unreadable':
                target.chmod(0o600)
        self.assertEqual(state['hook_count'], 1 if raced else 0)
        self.assertEqual(outside.read_bytes(), replacement)
        if expected is None:
            self.assertEqual(outcome, ('success', True, False, True, True, False, '0o644'), outcome)
        else:
            self.assertEqual(outcome, ('BundleError', expected, 69, expected, None), outcome)
            self.assertFalse((staging / 'manifest.json').exists())
            staged_member = staging / name
            if staged_member.exists() and action in ('symlink-before', 'truncate-after'):
                self.assertNotEqual(staged_member.read_bytes(), replacement)

    def _replace_on_open(self, source, target, kind, expected, forbidden=None):
        original = target.read_bytes() if target.is_symlink() is False and target.is_file() else b''
        if kind == 'disappear':
            original = target.read_bytes()
        outside = self._outside(source.name + '-' + kind)
        outside.write_bytes(b'EXTERNAL REPLACEMENT\n')
        state = {'done': False, 'opened': [], 'closed': [], 'fds': set()}
        real_open, real_close = os.open, os.close

        def mutate():
            state['done'] = True
            if kind == 'disappear':
                target.rename(self.root / (source.name + '-held'))
                return
            if kind == 'path-after':
                target.rename(self.root / (source.name + '-held'))
                target.write_bytes(original)
                target.chmod(0o600)
                return
            if kind == 'unlink-after':
                target.rename(self.root / (source.name + '-held'))
                return
            self._swap_nonregular(target, kind, outside, original)

        def os_open(path, flags, *args, **kwargs):
            hit = not state['done'] and not isinstance(path, int) and self._same(path, target)
            if hit and kind not in ('path-after', 'unlink-after'):
                mutate()
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int):
                state['opened'].append(os.path.normpath(os.fspath(path)))
                if self._same(path, target):
                    state['fds'].add(fd)
            if hit and kind in ('path-after', 'unlink-after'):
                mutate()
            return fd

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        os.open, os.close = os_open, os_close
        try:
            self._refuse(source, self.root / (source.name + '-staging'), expected)
        finally:
            os.open, os.close = real_open, real_close
        self.assertTrue(state['done'])
        if kind == 'same-bytes':
            held = self.root / (source.name + '-held')
            self.assertEqual(held.read_bytes(), target.read_bytes())
            self.assertNotEqual(held.lstat().st_ino, target.lstat().st_ino)
        if state['closed']:
            self._assert_closed(state['closed'])
        if forbidden is not None:
            self.assertNotIn(str(forbidden), state['opened'])

    def _during_read(self, source, kind):
        target = source / self.bundle.REQUIRED[0]
        original = target.read_bytes()
        inode = target.lstat().st_ino
        state = {'done': False, 'read': 0, 'requests': [], 'fds': set(), 'closed': [], 'flags': 0, 'fstats': 0}
        real_open, real_read, real_close, real_fstat = os.open, os.read, os.close, os.fstat

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
                state['flags'] = flags
            return fd

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
                state['fds'].discard(fd)
            return real_close(fd)

        def mutate():
            if kind == 'rewrite':
                writer = real_open(target, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    self._write_all(writer, b'X' * len(original))
                finally:
                    real_close(writer)
            elif kind == 'truncate':
                writer = real_open(target, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    os.ftruncate(writer, 0)
                finally:
                    real_close(writer)
            elif kind == 'chmod':
                os.chmod(target, 0o700)
            else:
                writer = real_open(target, os.O_RDWR | os.O_NOFOLLOW)
                try:
                    os.lseek(writer, 0, os.SEEK_END)
                    self._write_all(writer, b'Y' * (1024 * 1024))
                finally:
                    real_close(writer)

        def os_read(fd, size):
            data = real_read(fd, size)
            if fd in state['fds']:
                state['requests'].append(size)
                if not state['done']:
                    state['done'] = True
                    mutate()
                state['read'] += len(data)
            return data

        def os_fstat(fd):
            if fd in state['fds']:
                state['fstats'] += 1
            return real_fstat(fd)

        os.open, os.read, os.close, os.fstat = os_open, os_read, os_close, os_fstat
        fds = self._fds()
        try:
            self._refuse(source, self.root / (source.name + '-staging'), 'source_changed')
        finally:
            os.open, os.read, os.close, os.fstat = real_open, real_read, real_close, real_fstat
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertTrue(state['flags'] & os.O_NOFOLLOW)
        self.assertTrue(state['flags'] & os.O_NONBLOCK)
        self.assertTrue(state['requests'])
        self.assertLessEqual(max(state['requests']), self.bundle._MEMBER_CHUNK)
        self.assertLessEqual(state['read'], len(original) + 1)
        self.assertGreaterEqual(state['fstats'], 1)
        self._assert_closed(state['closed'])
        self.assertEqual(target.lstat().st_ino, inode)
        if kind == 'grow':
            self.assertEqual(state['read'], len(original) + 1)
            self.assertEqual(target.stat().st_size, len(original) + 1024 * 1024)
        elif kind == 'rewrite':
            self.assertEqual(target.read_bytes(), b'X' * len(original))
        elif kind == 'truncate':
            self.assertEqual(target.stat().st_size, 0)
        else:
            self.assertTrue(stat.S_IMODE(target.lstat().st_mode) & 0o111)

    def _observe_source_io(self, source, staging, target, size):
        state = {'read': 0, 'requests': [], 'flags': 0, 'path_open': False, 'fds': set(),
                 'closed': [], 'opens': 0, 'fstats': 0, 'outcome': None}
        real_open, real_read, real_close, real_fstat, real_path_open = os.open, os.read, os.close, os.fstat, Path.open

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
                state['flags'] = flags
                state['opens'] += 1
            return fd

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
                state['fds'].discard(fd)
            return real_close(fd)

        def os_read(fd, request):
            if fd in state['fds']:
                state['requests'].append(request)
            data = real_read(fd, request)
            if fd in state['fds']:
                state['read'] += len(data)
            return data

        def os_fstat(fd):
            if fd in state['fds']:
                state['fstats'] += 1
            return real_fstat(fd)

        def path_open(path, *args, **kwargs):
            if self._same(path, target):
                state['path_open'] = True
            return real_path_open(path, *args, **kwargs)

        os.open, os.read, os.close, os.fstat, Path.open = os_open, os_read, os_close, os_fstat, path_open
        fds = self._fds()
        try:
            try:
                state['outcome'] = self.bundle.build(source, staging)
            except self.bundle.BundleError as error:
                state['outcome'] = error
        finally:
            os.open, os.read, os.close, os.fstat, Path.open = real_open, real_read, real_close, real_fstat, real_path_open
            self.assertEqual(self._fds(), fds)
        return state

    def _short_read(self, source):
        target = source / self.bundle.REQUIRED[0]
        payload = target.read_bytes()
        state = {'fds': set(), 'short': False, 'closed': []}
        real_open, real_read, real_close = os.open, os.read, os.close

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
            return fd

        def os_read(fd, size):
            if fd in state['fds'] and not state['short'] and size > 1:
                data = real_read(fd, size)
                if len(data) > 1:
                    state['short'] = True
                    os.lseek(fd, 1 - len(data), os.SEEK_CUR)
                    return data[:1]
            return real_read(fd, size)

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        os.open, os.read, os.close = os_open, os_read, os_close
        try:
            staging = self.root / 'short-staging'
            built = self.bundle.build(source, staging)
        finally:
            os.open, os.read, os.close = real_open, real_read, real_close
        self.assertTrue(state['short'])
        self.assertEqual((staging / self.bundle.REQUIRED[0]).read_bytes(), payload)
        self._assert_verified(staging, built)
        self._assert_closed(state['closed'])

    def _eintr(self, source, where):
        target = source / self.bundle.REQUIRED[0]
        state = {'fds': set(), 'once': False, 'closed': []}
        real_open, real_read, real_fstat, real_close = os.open, os.read, os.fstat, os.close

        def os_open(path, flags, *args, **kwargs):
            if where == 'open' and self._same(path, target) and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted open seam')
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
            return fd

        def os_read(fd, size):
            if where == 'read' and fd in state['fds'] and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted read seam')
            return real_read(fd, size)

        def os_fstat(fd):
            if where == 'fstat' and fd in state['fds'] and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted fstat seam')
            return real_fstat(fd)

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        os.open, os.read, os.fstat, os.close = os_open, os_read, os_fstat, os_close
        try:
            staging = self.root / ('eintr-staging-' + where)
            built = self.bundle.build(source, staging)
        finally:
            os.open, os.read, os.fstat, os.close = real_open, real_read, real_fstat, real_close
        self.assertTrue(state['once'])
        self._assert_verified(staging, built)
        self._assert_closed(state['closed'])

    def _hide_flag(self, source, target, outside, flag):
        saved = getattr(os, flag)
        real_lstat, real_open, real_path_open = os.lstat, os.open, Path.open
        opened = []

        def wrapped_lstat(path, *args, **kwargs):
            if self._same(path, target) and hasattr(os, flag):
                delattr(os, flag)
            return real_lstat(path, *args, **kwargs)

        def os_open(path, *args, **kwargs):
            if not isinstance(path, int) and (self._same(path, target) or self._same(path, outside)):
                opened.append(os.path.normpath(os.fspath(path)))
            return real_open(path, *args, **kwargs)

        def path_open(path, *args, **kwargs):
            if self._same(path, target) or self._same(path, outside):
                opened.append('path:' + os.path.normpath(os.fspath(path)))
            return real_path_open(path, *args, **kwargs)

        before = outside.read_bytes()
        os.lstat, os.open, Path.open = wrapped_lstat, os_open, path_open
        try:
            self._refuse(source, self.root / (source.name + '-staging'), 'member_missing')
        finally:
            os.lstat, os.open, Path.open = real_lstat, real_open, real_path_open
            setattr(os, flag, saved)
        self.assertEqual(opened, [])
        self.assertEqual(outside.read_bytes(), before)

    def _inject_open(self, number, expected):
        source = self._fill('open-' + str(number))
        target = source / self.bundle.REQUIRED[0]
        real_open = os.open
        state = {'done': False}

        def os_open(path, flags, *args, **kwargs):
            if self._same(path, target) and not state['done']:
                state['done'] = True
                raise OSError(number, 'injected source open seam')
            return real_open(path, flags, *args, **kwargs)

        os.open = os_open
        try:
            self._refuse(source, self.root / ('open-staging-' + str(number)), expected)
        finally:
            os.open = real_open
        self.assertTrue(state['done'])

    def _inject_lstat(self, number, expected, *, release):
        source = self._fill('lstat-' + str(number) + '-' + str(release).lower())
        target = source / ('release-source.json' if release else self.bundle.REQUIRED[0])
        if release and number == errno.EACCES:
            fallback = self._write_fallback(source, b'FALLBACK MUST STAY UNREAD\n')
        else:
            fallback = None
        real_lstat = os.lstat
        state = {'done': False, 'opened': []}
        real_open = os.open

        def os_lstat(path, *args, **kwargs):
            if self._same(path, target) and not state['done']:
                state['done'] = True
                raise OSError(number, 'injected source lstat seam')
            return real_lstat(path, *args, **kwargs)

        def os_open(path, flags, *args, **kwargs):
            if not isinstance(path, int):
                state['opened'].append(os.path.normpath(os.fspath(path)))
            return real_open(path, flags, *args, **kwargs)

        os.lstat, os.open = os_lstat, os_open
        try:
            self._refuse(source, self.root / ('lstat-staging-' + str(number) + '-' + str(release).lower()), expected)
        finally:
            os.lstat, os.open = real_lstat, real_open
        self.assertTrue(state['done'])
        if fallback is not None:
            self.assertNotIn(str(fallback), state['opened'])

    def _inject_delayed_close(self, role, handled=False):
        label = 'close-' + role + ('-handled' if handled else '')
        source = self._fill(label)
        primary = source / 'release-source.json'
        primary_bytes = b'PRIMARY CLOSE FAILURE\n'
        fallback_bytes = b'FALLBACK CLOSE FAILURE\n'
        primary.write_bytes(primary_bytes)
        primary.chmod(0o600)
        fallback = self._write_fallback(source, fallback_bytes)
        before = self._identity(primary)
        fallback_before = self._identity(fallback)
        member = self.bundle.REQUIRED[0]
        watched = source / member if role == 'source' else self.root / (label + '-staging') / member
        original = (source / member).read_bytes()
        real_open, real_close, real_fstat = os.open, os.close, os.fstat
        state = {'fds': set(), 'hits': 0, 'real_closed': False, 'opened': [], 'closed': []}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int):
                state['opened'].append(os.path.normpath(os.fspath(path)))
                watched_kwargs = kwargs if role == 'destination' else None
                if self._same(path, watched, watched_kwargs):
                    state['fds'].add(fd)
            return fd

        def os_close(fd):
            if fd in state['fds'] and state['hits'] == 0:
                real_close(fd)
                state['hits'] += 1
                state['closed'].append(fd)
                try:
                    real_fstat(fd)
                except OSError as exc:
                    state['real_closed'] = exc.errno == errno.EBADF
                raise OSError(errno.EIO, 'injected delayed close failure')
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        os.open, os.close = os_open, os_close
        fds = self._fds()
        staging = self.root / (label + '-staging')
        try:
            if role == 'destination':
                with self.assertRaises(OSError) as caught:
                    self._build_while_handling(source, staging, handled)
            else:
                with self.assertRaises(self.bundle.BundleError) as caught:
                    self._build_while_handling(source, staging, handled)
        finally:
            os.open, os.close = real_open, real_close
            self.assertEqual(self._fds(), fds)
        self.assertEqual(state['hits'], 1)
        self.assertTrue(state['real_closed'])
        self._assert_closed(state['closed'])
        self.assertFalse((staging / 'manifest.json').exists())
        self.assertNotIn(str(fallback), state['opened'])
        self.assertEqual((staging / member).read_bytes(), original)
        self.assertEqual(self._identity(primary), before)
        self.assertEqual(self._identity(fallback), fallback_before)
        self.assertEqual(fallback.read_bytes(), fallback_bytes)
        if role == 'destination':
            self.assertEqual(caught.exception.errno, errno.EIO)
            self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
            self.assertNotEqual(getattr(caught.exception, 'code', None), 'source_unreadable')
        else:
            self.assertEqual(caught.exception.code, 'source_unreadable')
            self.assertEqual(caught.exception.exit_code, 69)
            self.assertEqual(str(caught.exception), 'source_unreadable')
            self.assertIsNone(caught.exception.__cause__)
        self.assertNotIn(str(self.root), str(caught.exception))
        self.assertNotIn('SYNTHETIC', str(caught.exception))
        with self.assertRaises(self.bundle.BundleError) as verified:
            self.bundle.verify(staging)
        self.assertEqual(verified.exception.code, 'manifest_missing')

    def _build_while_handling(self, source, staging, handled):
        if not handled:
            self.bundle.build(source, staging)
            return
        try:
            raise ValueError('already-handled-caller-error')
        except ValueError:
            self.bundle.build(source, staging)

    def _close_after_destination_write_error(self):
        source = self._fill('close-after-write')
        member = self.bundle.REQUIRED[0]
        watched = self.root / 'close-after-write-staging' / member
        real_open, real_write, real_close, real_fstat = os.open, os.write, os.close, os.fstat
        state = {'fds': set(), 'hits': 0, 'real_closed': False, 'wrote': False, 'closed': []}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, watched, kwargs):
                state['fds'].add(fd)
            return fd

        def os_write(fd, data):
            if fd in state['fds'] and not state['wrote']:
                state['wrote'] = True
                raise OSError(errno.EACCES, 'injected destination write')
            return real_write(fd, data)

        def os_close(fd):
            if fd in state['fds'] and state['hits'] == 0:
                real_close(fd)
                state['hits'] += 1
                state['closed'].append(fd)
                try:
                    real_fstat(fd)
                except OSError as exc:
                    state['real_closed'] = exc.errno == errno.EBADF
                raise OSError(errno.EIO, 'injected delayed close failure')
            return real_close(fd)

        os.open, os.write, os.close = os_open, os_write, os_close
        fds = self._fds()
        staging = self.root / 'close-after-write-staging'
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.write, os.close = real_open, real_write, real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['wrote'])
        self.assertEqual(state['hits'], 1)
        self.assertTrue(state['real_closed'])
        self._assert_closed(state['closed'])
        self.assertEqual(caught.exception.errno, errno.EACCES)
        self.assertIn('injected destination write', str(caught.exception))
        self.assertNotIn('injected delayed close failure', str(caught.exception))
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertFalse((staging / 'manifest.json').exists())

    def _close_after_programming_error(self):
        source = self._fill('close-after-programming')
        target = source / self.bundle.REQUIRED[0]
        real_open, real_fstat, real_close = os.open, os.fstat, os.close
        state = {'fds': set(), 'hits': 0, 'real_closed': False, 'done': False, 'closed': []}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
            return fd

        def os_fstat(fd):
            if fd in state['fds'] and not state['done']:
                state['done'] = True
                raise RuntimeError('programming-error')
            return real_fstat(fd)

        def os_close(fd):
            if fd in state['fds'] and state['hits'] == 0:
                real_close(fd)
                state['hits'] += 1
                state['closed'].append(fd)
                try:
                    real_fstat(fd)
                except OSError as exc:
                    state['real_closed'] = exc.errno == errno.EBADF
                raise OSError(errno.EIO, 'injected delayed close failure')
            return real_close(fd)

        os.open, os.fstat, os.close = os_open, os_fstat, os_close
        fds = self._fds()
        staging = self.root / 'close-after-programming-staging'
        try:
            with self.assertRaises(RuntimeError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.fstat, os.close = real_open, real_fstat, real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(state['hits'], 1)
        self.assertTrue(state['real_closed'])
        self._assert_closed(state['closed'])
        self.assertEqual(str(caught.exception), 'programming-error')
        self.assertFalse((staging / 'manifest.json').exists())

    def _inject_member_io(self, where, number, expected):
        source = self._fill(where + '-' + str(number))
        target = source / self.bundle.REQUIRED[0]
        real_open, real_read, real_fstat, real_close = os.open, os.read, os.fstat, os.close
        state = {'fds': set(), 'done': False, 'closed': []}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if self._same(path, target):
                state['fds'].add(fd)
            return fd

        def os_read(fd, size):
            if where == 'read' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(number, 'injected source read seam')
            return real_read(fd, size)

        def os_fstat(fd):
            if where == 'fstat' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(number, 'injected source fstat seam')
            return real_fstat(fd)

        def os_close(fd):
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        os.open, os.read, os.fstat, os.close = os_open, os_read, os_fstat, os_close
        fds = self._fds()
        try:
            self._refuse(source, self.root / (where + '-staging-' + str(number)), expected)
        finally:
            os.open, os.read, os.fstat, os.close = real_open, real_read, real_fstat, real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self._assert_closed(state['closed'])

    def _swap_nonregular(self, target, kind, outside, original):
        held = self.root / (target.parent.name + '-held')
        if kind == 'same-bytes':
            outside.write_bytes(original)
            outside.chmod(0o600)
        target.rename(held)
        if kind == 'absolute' or kind == 'external':
            target.symlink_to(outside)
        elif kind == 'relative':
            target.symlink_to(Path('..') / outside.name)
        elif kind == 'internal':
            target.symlink_to(self.bundle.REQUIRED[1])
        elif kind == 'dangling':
            target.symlink_to('missing-source-target')
        elif kind == 'chain':
            inner = self.root / (target.parent.name + '-inner')
            outer = self.root / (target.parent.name + '-outer')
            inner.write_bytes(original)
            inner.chmod(0o600)
            outer.symlink_to(inner)
            target.symlink_to(Path('..') / outer.name)
        elif kind == 'directory':
            target.mkdir(mode=0o700)
        elif kind == 'same-bytes':
            outside.rename(target)
        else:
            self.fail('unknown replacement ' + kind)

    def _make_nonregular(self, target, kind, outside):
        raw = target.read_bytes()
        target.unlink()
        if kind == 'absolute' or kind == 'external':
            target.symlink_to(outside if kind == 'absolute' else outside.resolve())
        elif kind == 'relative':
            target.symlink_to(Path('..') / outside.name)
        elif kind == 'internal':
            target.symlink_to(self.bundle.REQUIRED[1])
        elif kind == 'dangling':
            target.symlink_to('missing-source-target')
        elif kind == 'chain':
            inner = self.root / (target.parent.name + '-inner')
            outer = self.root / (target.parent.name + '-outer')
            inner.write_bytes(raw)
            inner.chmod(0o600)
            outer.symlink_to(inner)
            target.symlink_to(Path('..') / outer.name)
        elif kind == 'directory':
            target.mkdir(mode=0o700)
        else:
            self.fail('unknown static kind ' + kind)

    def _fifo_child(self, mode, source, staging, name):
        try:
            completed = subprocess.run(
                [sys.executable, '-B', '-c',
                 'import sys; sys.path.insert(0, sys.argv[1]); '
                 'import test_upgrade_bundle as tests; '
                 'raise SystemExit(tests._build_source_fifo(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]))',
                 str(HERE), mode, str(source), str(staging), name],
                cwd=self.root, timeout=5, capture_output=True, text=True,
                env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        except subprocess.TimeoutExpired as expired:
            self.fail('fifo source build was still blocked after 5s\n'
                      + (expired.stdout or '') + (expired.stderr or ''))
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertNotIn('SYNTHETIC', completed.stdout)
        self.assertNotIn(str(self.root), completed.stdout)

    def _fill(self, name):
        source = self.root / name
        self._fill_at(source)
        return source

    def _fill_at(self, source):
        source.mkdir(mode=0o700)
        for name in self.bundle.REQUIRED:
            path = source / name
            path.write_bytes(b'SYNTHETIC INERT SOURCE ' + name.encode() + b'\n')
            path.chmod(0o600)

    def _write_fallback(self, source, payload):
        fork = source.parent / 'fork'
        fork.mkdir(mode=0o700, exist_ok=True)
        path = fork / 'release-source.json'
        if path.is_symlink() or path.exists():
            path.unlink()
        path.write_bytes(payload)
        path.chmod(0o600)
        return path

    def _outside(self, name):
        path = self.root / (name + '-outside')
        path.touch(mode=0o600)
        return path

    def _build_ok(self, source, staging):
        sentinel = self.root / (source.name + '-sentinel')
        sentinel.write_bytes(b'SENTINEL\n')
        before = self._identity(sentinel)
        built = self.bundle.build(source, staging)
        self._assert_verified(staging, built)
        self.assertEqual(sentinel.read_bytes(), b'SENTINEL\n')
        self.assertEqual(self._identity(sentinel), before)
        return built

    def _assert_verified(self, staging, built):
        self.assertIsInstance(built, dict)
        verified = self.bundle.verify(staging)
        self.assertTrue(verified['verified'])
        self.assertFalse(verified['installable'])
        self.assertEqual(verified['manifest_sha256'], built['manifest_sha256'])
        raw = (staging / 'manifest.json').read_bytes()
        self.assertEqual(hashlib.sha256(raw).hexdigest(), built['manifest_sha256'])

    def _refuse(self, source, staging, code):
        fds = self._fds()
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.build(source, staging)
        finally:
            self.assertEqual(self._fds(), fds)
        self.assertEqual(caught.exception.code, code)
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(str(caught.exception), code)
        self.assertIsNone(caught.exception.__cause__)
        self.assertNotIn(str(self.root), str(caught.exception))
        self.assertNotIn('SYNTHETIC', str(caught.exception))
        self.assertFalse((staging / 'manifest.json').exists())
        return caught.exception

    def _assert_closed(self, closed):
        self.assertTrue(closed)
        for fd in closed:
            with self.assertRaises(OSError) as caught:
                os.fstat(fd)
            self.assertEqual(caught.exception.errno, errno.EBADF)

    def _identity(self, path):
        info = path.lstat()
        return (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
                info.st_mtime_ns, info.st_ctime_ns)

    def _tree_identity(self, root):
        rows = []
        for path in sorted(root.rglob('*'), key=lambda item: item.as_posix()):
            rows.append((str(path.relative_to(root)), self._identity(path)))
        return tuple(rows)

    def _same(self, path, target, open_kwargs=None):
        if open_kwargs is None:
            return not isinstance(path, int) and os.path.normpath(os.fspath(path)) == str(target)
        return _entry_matches(path, target, open_kwargs)

    def _fds(self):
        base = '/proc/self/fd' if os.path.isdir('/proc/self/fd') else '/dev/fd'
        return frozenset(os.listdir(base))

    def _relax_tree(self):
        if not self.root.exists():
            return
        for path in [self.root, *self.root.rglob('*')]:
            try:
                info = path.lstat()
            except OSError:
                continue
            if stat.S_ISLNK(info.st_mode):
                continue
            os.chmod(path, 0o700 if stat.S_ISDIR(info.st_mode) else 0o600)

    def _write_all(self, fd, data):
        view = memoryview(data)
        while view:
            wrote = os.write(fd, view)
            if wrote <= 0:
                self.fail('fixture write stalled')
            view = view[wrote:]



class ProgrammingMarker(RuntimeError):
    """Injected verifier fault. Not a bundle input error."""


class CancelMarker(BaseException):
    """Injected cancellation. Must not become BundleError."""


class VerifierCloseTests(unittest.TestCase):
    """Close failures after a real manifest or member read.

    Errno values are injected after the real close. They are not hardware
    faults. Hooks call the real open, read, fstat, and close. Fixtures are
    inert and are not executed or imported.
    """

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.module_path = Path(bundle.__file__).resolve()
        self.assertEqual(self.module_path, HERE / 'upgrade_bundle.py')
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-verifier-close-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        self.payloads = {}
        for name in bundle.REQUIRED:
            path = self.source / name
            payload = b'SYNTHETIC INERT CLOSE ' + name.encode() + b'\x00\xff\n'
            path.write_bytes(payload)
            path.chmod(0o600)
            self.payloads[name] = payload
        self.sentinel = self.root / 'sentinel'
        self.sentinel.write_bytes(b'SYNTHETIC SENTINEL\n')
        self.sentinel.chmod(0o600)

    def test_recorded_probe_cases(self):
        """Eight close failures must refuse. Twelve controls keep their result."""
        cases = []
        for role in ('manifest', 'member'):
            for number in (errno.EIO, errno.EINTR):
                for handled in (False, True):
                    cases.append((role, 'valid', number, handled))
            for mode in ('no_fault', 'prior_refusal', 'programming'):
                for handled in (False, True):
                    cases.append((role, mode, errno.EIO, handled))
        self.assertEqual(len(cases), 20)
        for role, mode, number, handled in cases:
            with self.subTest(role=role, mode=mode, errno=errno.errorcode[number], handled=handled):
                self._recorded(role, mode, number, handled)

    def test_close_errno_is_fixed_and_does_not_reuse_the_fd(self):
        """Any close OSError is the fixed code. A reused descriptor is not closed again."""
        numbers = (errno.EIO, errno.EINTR, errno.EBADF, errno.ENOENT, errno.EACCES)
        for role in ('manifest', 'member'):
            expected = 'manifest_unreadable' if role == 'manifest' else 'member_unreadable'
            for number in numbers:
                with self.subTest(role=role, errno=errno.errorcode[number], occupy=True):
                    self._close_case(role, number, expected, occupy=True)
            with self.subTest(role=role, errno='none', occupy=True):
                self._close_case(role, None, 'success', occupy=True)

    def test_executable_false_still_rejects_close_failure(self):
        """executable=False skips only the executable bit."""
        for role in ('manifest', 'member'):
            for injected in (False, True):
                with self.subTest(role=role, injected=injected):
                    staging, built, target = self._stage('exec-false-' + role + '-' + str(injected).lower(), role)
                    target.chmod(0o700)
                    number = errno.EIO if injected else None
                    expected = self._close_code(role) if injected else 'success'
                    self._close_case(role, number, expected, staging=staging, built=built, target=target,
                                     executable=False, occupy=injected)

    def test_earlier_failure_survives_close_failure(self):
        """A later close OSError must not replace the exception this read already raised."""
        kinds = ('read', 'fstat', 'identity', 'owner', 'executable', 'keyboard', 'cancel', 'programming')
        for role in ('manifest', 'member'):
            for kind in kinds:
                for handled in (False, True):
                    with self.subTest(role=role, kind=kind, handled=handled):
                        self._earlier(role, kind, handled)

    def test_manifest_close_stops_before_parser_and_members(self):
        follow = {}
        staging, built, target = self._stage('manifest-stop', 'manifest')
        seen, restore_io = self._hooks(target, close_errno=errno.EIO)
        restore_follow = self._watch_followups(follow)
        before = self._snapshot()
        fds = self._fds()
        try:
            _outcome, error, preserved = self._call(staging, handled=True)
        finally:
            restore_follow()
            restore_io()
        self.assertEqual(self._fds(), fds)
        self.assertEqual(self._snapshot(), before)
        self._assert_close_error(error, 'manifest_unreadable')
        self.assertTrue(preserved)
        self.assertEqual(follow, {'parser': 0, 'iterdir': 0})
        self.assertEqual(seen['opens_after'], 0)
        self.assertEqual(seen['target_opens'], 1)
        self.assertGreaterEqual(seen['target_reads'], 2)
        self.assertEqual(seen['read_bytes'], target.stat().st_size)
        self.assertGreaterEqual(seen['fstats'], 2)
        self.assertEqual(seen['target_close_calls'], 1)
        self.assertEqual(seen['double_close'], 0)
        self.assertEqual(seen['close_injections'], 1)
        self.assertEqual([name for name in seen['opened_names'] if name != 'manifest.json'], [])
        self.assertNotIn('installable', built)

    def test_member_close_stops_at_early_middle_and_last(self):
        staging, _built, _target = self._stage('member-order', 'member')
        order = list(json.loads((staging / 'manifest.json').read_bytes())['files'])
        self.assertEqual(order, sorted(self.bundle.REQUIRED))
        positions = {
            'early': 0,
            'middle': len(order) // 2,
            'last': len(order) - 1,
        }
        self.assertEqual(len(set(positions.values())), 3)
        for name, index in positions.items():
            with self.subTest(position=name):
                self._member_stop(name, order, index)

    def test_compatible_shapes_remain_readonly_and_uninstalled(self):
        """Current UID, modes, hard links, and empty or large members stay uninstalled."""
        for mode in (0o600, 0o644):
            for role in ('manifest', 'member'):
                with self.subTest(role=role, mode=oct(mode), injected=False):
                    self._shape(role, mode=mode, injected=False)
                with self.subTest(role=role, mode=oct(mode), injected=True):
                    self._shape(role, mode=mode, injected=True)
        for size in (0, self.bundle._MEMBER_CHUNK + 1):
            for injected in (False, True):
                with self.subTest(size=size, injected=injected):
                    self._shape('member', size=size, injected=injected)
        for injected in (False, True):
            with self.subTest(hardlink=True, injected=injected):
                self._shape('member', hardlink=True, injected=injected)

    def _recorded(self, role, mode, number, handled):
        label = 'recorded-%s-%s-%s-%s' % (role, mode, errno.errorcode[number], int(handled))
        staging, built, target = self._stage(label, role)
        if mode == 'prior_refusal':
            if role == 'manifest':
                raw = target.read_bytes()
                target.write_bytes(raw + b' ' * (65537 - len(raw)))
            else:
                target.write_bytes(b'SYNTHETIC TAMPER\n')
        marker = ProgrammingMarker('SYNTHETIC PROGRAMMING MARKER')

        def on_read(_raw):
            if mode == 'programming':
                raise marker

        close_errno = None if mode == 'no_fault' else number
        seen, restore = self._hooks(target, close_errno=close_errno, on_read=on_read if mode == 'programming' else None)
        before = self._snapshot()
        fds = self._fds()
        try:
            outcome, error, preserved = self._call(staging, handled=handled)
        finally:
            restore()
        expected = {
            'valid': self._close_code(role),
            'no_fault': 'success',
            'prior_refusal': 'manifest_budget_exceeded' if role == 'manifest' else 'digest_rejected',
            'programming': 'ProgrammingMarker',
        }[mode]
        detail = self._detail(expected, error, seen, preserved)
        self.assertEqual(self._fds(), fds, detail)
        self.assertEqual(self._snapshot(), before, detail)
        self.assertTrue(preserved, detail)
        self.assertEqual(seen['target_opens'], 1, detail)
        self.assertGreaterEqual(seen['target_reads'], 1, detail)
        self.assertGreater(seen['read_bytes'], 0, detail)
        self.assertEqual(seen['target_close_calls'], 1, detail)
        self.assertEqual(seen['double_close'], 0, detail)
        self.assertEqual(seen['real_fd_closed'], [True], detail)
        self.assertEqual(seen['close_injections'], 0 if mode == 'no_fault' else 1, detail)
        self.assertEqual(self._observed(error), expected, detail)
        if mode == 'programming':
            self.assertIs(error, marker, detail)
        elif mode == 'no_fault':
            self._assert_success(staging, built, outcome)
        else:
            self._assert_bundle_error(error, expected, close_generated=(mode == 'valid'))
        if mode != 'no_fault':
            self.assertEqual(seen['opens_after'], 0, detail)
        if mode in ('valid', 'no_fault'):
            self.assertGreaterEqual(seen['target_reads'], 2, detail)
            self.assertGreaterEqual(seen['fstats'], 2, detail)
            self.assertEqual(seen['read_bytes'], target.stat().st_size, detail)

    def _close_case(self, role, number, expected, *, occupy=False, staging=None, built=None,
                    target=None, executable=True):
        if staging is None:
            label = 'close-' + role + '-' + (errno.errorcode[number] if number is not None else 'ok')
            if not executable:
                label += '-noexec'
            staging, built, target = self._stage(label, role)
        seen, restore = self._hooks(target, close_errno=number, occupy=occupy)
        before = self._snapshot()
        fds = self._fds()
        try:
            outcome, error, preserved = self._call(staging, executable=executable, handled=True)
        finally:
            restore()
        detail = self._detail(expected, error, seen, preserved)
        self.assertEqual(self._fds(), fds, detail)
        self.assertEqual(self._snapshot(), before, detail)
        self.assertTrue(preserved, detail)
        self.assertEqual(seen['target_close_calls'], 1, detail)
        self.assertEqual(seen['double_close'], 0, detail)
        self.assertEqual(seen['real_fd_closed'], [True], detail)
        self.assertGreaterEqual(seen['target_opens'], 1, detail)
        self.assertGreaterEqual(seen['target_reads'], 1, detail)
        self.assertEqual(seen['close_injections'], 0 if number is None else 1, detail)
        if occupy:
            self.assertTrue(seen['occupied'], detail)
        if expected == 'success':
            self._assert_success(staging, built, outcome)
            self.assertIsNone(error, detail)
        else:
            self._assert_close_error(error, expected)
            self.assertEqual(seen['opens_after'], 0, detail)

    def _earlier(self, role, kind, handled):
        staging, built, target = self._stage('earlier-' + role + '-' + kind + '-' + str(handled), role)
        marker = ProgrammingMarker('SYNTHETIC PROGRAMMING MARKER')
        cancel = CancelMarker('SYNTHETIC CANCEL')
        keyboard = KeyboardInterrupt('SYNTHETIC INTERRUPT')
        once = {'done': False}

        def on_read(_raw):
            if once['done']:
                return
            once['done'] = True
            if kind == 'read':
                raise OSError(errno.EIO, 'SYNTHETIC READ ERROR', str(target))
            if kind == 'identity':
                current = stat.S_IMODE(target.stat().st_mode)
                updated = 0o640 if current != 0o640 else 0o604
                if updated & 0o111 or updated == current:
                    raise AssertionError('identity seam could not choose a non-executable mode')
                os.chmod(target, updated)
            elif kind == 'programming':
                raise marker
            elif kind == 'keyboard':
                raise keyboard
            elif kind == 'cancel':
                raise cancel

        def on_fstat(_info):
            if kind == 'fstat' and not once['done']:
                once['done'] = True
                raise OSError(errno.EIO, 'SYNTHETIC FSTAT ERROR', str(target))

        if kind == 'executable':
            target.chmod(0o700)
        created = []
        owner = self._patch_owner(role, os.getuid() + 1) if kind == 'owner' else None
        seen, restore = self._hooks(
            target, close_errno=errno.EIO,
            on_read=on_read if kind in ('read', 'identity', 'programming', 'keyboard', 'cancel') else None,
            on_fstat=on_fstat if kind == 'fstat' else None)
        self._mark_errors(created)
        before = self._snapshot()
        fds = self._fds()
        try:
            _outcome, error, preserved = self._call(staging, handled=handled)
        finally:
            restore()
            self._unmark_errors()
            if owner is not None:
                owner()
        if kind == 'identity':
            self._assert_chmod_only(before, target)
        else:
            self.assertEqual(self._snapshot(), before)
        self.assertEqual(self._fds(), fds)
        self.assertTrue(preserved)
        self.assertEqual(seen['target_close_calls'], 1)
        self.assertEqual(seen['close_injections'], 1)
        self.assertEqual(seen['double_close'], 0)
        self.assertEqual(seen['real_fd_closed'], [True])
        self.assertEqual(seen['opens_after'], 0)
        expected = {
            'read': self._close_code(role),
            'fstat': self._close_code(role),
            'identity': 'manifest_changed' if role == 'manifest' else 'member_changed',
            'owner': 'owner_rejected',
            'executable': 'unexpected_executable',
            'keyboard': 'KeyboardInterrupt',
            'cancel': 'CancelMarker',
            'programming': 'ProgrammingMarker',
        }[kind]
        self.assertEqual(self._observed(error), expected)
        if kind == 'programming':
            self.assertIs(error, marker)
        elif kind == 'keyboard':
            self.assertIs(error, keyboard)
        elif kind == 'cancel':
            self.assertIs(error, cancel)
        else:
            self._assert_bundle_error(error, expected, close_generated=False)
            self.assertEqual(created, [error])
        if kind in ('read', 'fstat', 'owner', 'executable'):
            self.assertGreaterEqual(seen['fstats'], 1)
        if kind in ('read', 'identity', 'programming', 'keyboard', 'cancel'):
            self.assertGreaterEqual(seen['target_reads'], 1)
        self.assertIsNone(built.get('installable'))

    def _member_stop(self, name, order, index):
        staging, _built, _target = self._stage('stop-' + name, 'member')
        target = staging / order[index]
        follow = {}
        seen, restore_io = self._hooks(target, close_errno=errno.EINTR, occupy=True)
        restore_follow = self._watch_followups(follow)
        before = self._snapshot()
        fds = self._fds()
        try:
            outcome, error, preserved = self._call(staging, handled=False)
        finally:
            restore_follow()
            restore_io()
        self.assertEqual(self._fds(), fds)
        self.assertEqual(self._snapshot(), before)
        self.assertTrue(preserved)
        self.assertIsNone(outcome)
        self._assert_close_error(error, 'member_unreadable')
        self.assertEqual(follow['parser'], 1)
        self.assertEqual(follow['iterdir'], 1)
        opened = [item for item in seen['opened_names'] if item in self.bundle.REQUIRED]
        self.assertEqual(opened, order[:index + 1])
        self.assertEqual(seen['opens_after'], 0)
        self.assertEqual(seen['target_close_calls'], 1)
        self.assertEqual(seen['double_close'], 0)
        self.assertTrue(seen['occupied'])
        self.assertGreaterEqual(seen['target_reads'], 2)
        self.assertEqual(seen['read_bytes'], target.stat().st_size)
        self.assertGreaterEqual(seen['fstats'], 2)

    def _shape(self, role, *, mode=None, size=None, hardlink=False, injected=False):
        member = 'upgrade.py'
        if size is not None:
            self._write_source(member, b'Z' * size)
        label = 'shape-' + role + '-' + str(mode) + '-' + str(size) + '-' + str(hardlink) + '-' + str(injected)
        staging, built, target = self._stage(label, role if size is None and not hardlink else 'member')
        if size is not None or hardlink:
            target = staging / member
        if mode is not None:
            target.chmod(mode)
        link = None
        if hardlink:
            link = self.root / (label + '-link')
            os.link(target, link)
        if role == 'manifest' and size is None and not hardlink:
            checked = 'manifest'
        else:
            checked = 'member'
        number = errno.EIO if injected else None
        expected = self._close_code(checked) if injected else 'success'
        try:
            self._close_case(checked, number, expected, staging=staging, built=built, target=target,
                             occupy=injected)
            if link is not None:
                self.assertEqual(link.lstat().st_ino, target.lstat().st_ino)
                self.assertGreaterEqual(target.lstat().st_nlink, 2)
        finally:
            if size is not None:
                self._write_source(member, self.payloads[member])

    def _stage(self, label, role):
        staging = self.root / label
        built = self.bundle.build(self.source, staging)
        target = staging / ('manifest.json' if role == 'manifest' else sorted(self.bundle.REQUIRED)[0])
        return staging, built, target

    def _write_source(self, name, payload):
        path = self.source / name
        path.write_bytes(payload)
        path.chmod(0o600)

    def _call(self, staging, *, executable=True, handled=False):
        preserved = True
        outcome = None
        error = None
        if not handled:
            try:
                outcome = self.bundle.verify(staging, executable=executable)
            except BaseException as exc:
                error = exc
            return outcome, error, preserved
        caller = LookupError('SYNTHETIC CALLER EXCEPTION')
        try:
            raise caller
        except LookupError:
            try:
                outcome = self.bundle.verify(staging, executable=executable)
            except BaseException as exc:
                error = exc
            preserved = sys.exception() is caller
        return outcome, error, preserved

    def _hooks(self, target, *, close_errno=None, on_read=None, on_fstat=None, occupy=False):
        real_open, real_read, real_close, real_fstat = os.open, os.read, os.close, os.fstat
        state = {
            'target_opens': 0, 'target_reads': 0, 'read_bytes': 0, 'target_close_calls': 0,
            'close_injections': 0, 'real_fd_closed': [], 'opens_after': 0, 'fstats': 0,
            'double_close': 0, 'opened_names': [], 'occupied': False,
        }
        live = {}
        finished = set()
        sentinels = set()
        closed = {'value': False}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            # A later open may reuse the number. That is not a second close.
            if fd in finished and fd not in sentinels:
                finished.discard(fd)
            if not isinstance(path, int):
                name = Path(os.fspath(path)).name
                if name == 'manifest.json' or name in self.bundle.REQUIRED:
                    state['opened_names'].append(name)
                if self._same(path, target, kwargs):
                    live[fd] = True
                    state['target_opens'] += 1
            if closed['value']:
                state['opens_after'] += 1
            return fd

        def os_read(fd, amount):
            raw = real_read(fd, amount)
            if live.get(fd):
                state['target_reads'] += 1
                state['read_bytes'] += len(raw)
                if on_read is not None:
                    on_read(raw)
            return raw

        def os_fstat(fd):
            info = real_fstat(fd)
            if live.get(fd):
                state['fstats'] += 1
                if on_fstat is not None:
                    on_fstat(info)
            return info

        def os_close(fd):
            if fd in sentinels or fd in finished:
                state['double_close'] += 1
                return None
            if not live.pop(fd, False):
                return real_close(fd)
            state['target_close_calls'] += 1
            real_close(fd)
            finished.add(fd)
            closed['value'] = True
            try:
                real_fstat(fd)
            except OSError as exc:
                state['real_fd_closed'].append(exc.errno == errno.EBADF)
            else:
                state['real_fd_closed'].append(False)
            if occupy:
                holder = real_open(os.devnull, os.O_RDONLY)
                if holder != fd:
                    os.dup2(holder, fd)
                    real_close(holder)
                sentinels.add(fd)
                state['occupied'] = True
            if close_errno is not None:
                state['close_injections'] += 1
                raise OSError(close_errno, 'SYNTHETIC CLOSE ERROR', str(target))
            return None

        os.open, os.read, os.close, os.fstat = os_open, os_read, os_close, os_fstat

        def restore():
            os.open, os.read, os.close, os.fstat = real_open, real_read, real_close, real_fstat
            for fd in list(sentinels):
                real_close(fd)
            sentinels.clear()

        return state, restore

    def _watch_followups(self, seen):
        seen['parser'] = 0
        seen['iterdir'] = 0
        real_load = self.bundle._load_manifest
        real_iterdir = Path.iterdir

        def load(raw):
            seen['parser'] += 1
            return real_load(raw)

        def iterdir(path):
            seen['iterdir'] += 1
            return real_iterdir(path)

        real_listdir = os.listdir

        def listdir(path, *args, **kwargs):
            if isinstance(path, int):
                seen['iterdir'] += 1
            return real_listdir(path, *args, **kwargs)

        self.bundle._load_manifest = load
        Path.iterdir = iterdir
        os.listdir = listdir

        def restore():
            self.bundle._load_manifest = real_load
            Path.iterdir = real_iterdir
            os.listdir = real_listdir

        return restore

    def _mark_errors(self, created):
        real = self.bundle.BundleError

        class Marked(real):
            def __init__(self, reason, exit_code=69):
                super().__init__(reason, exit_code)
                created.append(self)

        self._marked = (real, Marked)
        self.bundle.BundleError = Marked

    def _unmark_errors(self):
        if getattr(self, '_marked', None) is not None:
            self.bundle.BundleError = self._marked[0]
            self._marked = None

    def _patch_owner(self, role, uid):
        """Expected-UID seam only. This is not a foreign-owner or sudo proof."""
        self.assertNotEqual(os.getuid(), 0)
        name = '_manifest_owner_uid' if role == 'manifest' else '_member_owner_uid'
        original = getattr(self.bundle, name)
        setattr(self.bundle, name, lambda: uid)

        def restore():
            setattr(self.bundle, name, original)

        return restore

    def _assert_success(self, staging, built, outcome):
        raw = (staging / 'manifest.json').read_bytes()
        digest = hashlib.sha256(raw).hexdigest()
        self.assertEqual(outcome, {
            'verified': True, 'installable': False, 'manifest_sha256': digest,
        })
        self.assertIs(outcome['verified'], True)
        self.assertIs(outcome['installable'], False)
        self.assertEqual(outcome['manifest_sha256'], built['manifest_sha256'])
        self.assertEqual(digest, built['manifest_sha256'])

    def _assert_close_error(self, error, expected):
        self._assert_bundle_error(error, expected, close_generated=True)

    def _assert_bundle_error(self, error, expected, *, close_generated):
        self.assertIsInstance(error, self.bundle.BundleError)
        self.assertEqual(error.code, expected)
        self.assertEqual(error.exit_code, 69)
        self.assertEqual(str(error), expected)
        self.assertEqual(error.args, (expected,))
        self.assertIsNone(error.__cause__)
        self.assertNotIn(str(self.root), str(error))
        self.assertNotIn('SYNTHETIC', str(error))
        self.assertNotIn(str(self.root), repr(error.args))
        if close_generated:
            self.assertTrue(error.__suppress_context__)

    def _assert_chmod_only(self, before, target):
        after = self._snapshot()
        rel = str(target.relative_to(self.root))
        self.assertEqual(set(before), set(after))
        for key, old in before.items():
            new = after[key]
            if key == rel:
                self.assertEqual(old[7], new[7], key)
                self.assertEqual(old[:2], new[:2], key)
                self.assertEqual(old[3:6], new[3:6], key)
                self.assertNotEqual(old[2], new[2], key)
                continue
            self.assertEqual(old, new, key)

    def _observed(self, error):
        if error is None:
            return 'success'
        if isinstance(error, self.bundle.BundleError):
            return error.code
        return type(error).__name__

    def _close_code(self, role):
        return 'manifest_unreadable' if role == 'manifest' else 'member_unreadable'

    def _detail(self, expected, error, seen, preserved):
        return 'module=%s expected=%s observed=%s preserved=%s opens_after=%s closes=%s double=%s reads=%s bytes=%s' % (
            self.module_path, expected, self._observed(error), preserved, seen['opens_after'],
            seen['target_close_calls'], seen['double_close'], seen['target_reads'], seen['read_bytes'])

    def _snapshot(self):
        rows = {}
        for path in self.root.rglob('*'):
            info = path.lstat()
            digest = None
            if path.is_file() and not path.is_symlink():
                digest = hashlib.sha256(path.read_bytes()).hexdigest()
            rel = str(path.relative_to(self.root))
            rows[rel] = (info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
                         info.st_mtime_ns, info.st_ctime_ns, digest)
        return rows

    def _fds(self):
        base = '/proc/self/fd' if os.path.isdir('/proc/self/fd') else '/dev/fd'
        return frozenset(os.listdir(base))

    def _same(self, path, target, kwargs=None):
        return _entry_matches(path, target, kwargs)

def _occupy_destination(role, kind, source, staging, sentinel, outside):
    """Subprocess entry. An occupied fifo or dangling link must fail without waiting or following."""
    import upgrade_bundle as bundle
    source, staging = Path(source), Path(staging)
    sentinel, outside = Path(sentinel), Path(outside)
    name = 'manifest.json' if role == 'manifest' else bundle.REQUIRED[0]
    target = staging / name
    real_mkdir = os.mkdir

    def mkdir(path, mode=0o777, *args, **kwargs):
        real_mkdir(path, mode, *args, **kwargs)
        if Path(path) == staging:
            if kind == 'fifo':
                os.mkfifo(target, 0o600)
            elif kind == 'dangling':
                target.symlink_to(outside)
            else:
                raise AssertionError(kind)

    os.mkdir = mkdir
    before = sentinel.read_bytes()
    try:
        bundle.build(source, staging)
    except FileExistsError as exc:
        if exc.errno != errno.EEXIST:
            return 2
    else:
        return 3
    info = target.lstat()
    if sentinel.read_bytes() != before or outside.exists():
        return 4
    if kind == 'fifo' and not stat.S_ISFIFO(info.st_mode):
        return 5
    if kind == 'dangling' and not stat.S_ISLNK(info.st_mode):
        return 6
    if role == 'member' and (staging / 'manifest.json').exists():
        return 7
    if role == 'manifest' and stat.S_ISREG(info.st_mode):
        return 8
    return 0


class BuilderDestinationTests(unittest.TestCase):
    """Destination ownership for files this build creates.

    Hooks call the real open, write, and chmod. Fixtures are inert bytes and
    are never executed. The eight reproduced windows are exclusive-create or
    destination_changed refusals. The four controls keep the previous result.
    """

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        actual = Path(bundle.__file__).resolve()
        expected = (HERE / 'upgrade_bundle.py').resolve()
        self.assertEqual(actual, expected, 'refusing to test an imported module outside this tree')
        self.assertNotEqual(os.getuid(), 0)
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-dest-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.root.chmod(0o700)

    def test_recorded_probe_cases(self):
        """Eight destination windows plus four controls. One real build each."""
        cases = (
            ('manifest-regular', 'manifest', 'regular', 'exclusive'),
            ('manifest-hardlink', 'manifest', 'hardlink', 'exclusive'),
            ('manifest-absolute', 'manifest', 'absolute', 'exclusive'),
            ('manifest-relative', 'manifest', 'relative', 'exclusive'),
            ('member-regular', 'member', 'regular', 'destination_changed'),
            ('member-hardlink', 'member', 'hardlink', 'destination_changed'),
            ('member-absolute', 'member', 'absolute', 'destination_changed'),
            ('member-relative', 'member', 'relative', 'destination_changed'),
            ('regular-control', 'control', 'normal', 'success'),
            ('source-executable-control', 'control', 'source-executable', 'success'),
            ('member-collision-control', 'collision', 'regular', 'exclusive'),
            ('source-symlink-control', 'control', 'source-symlink', 'member_missing'),
        )
        self.assertEqual(len(cases), 12)
        for label, role, kind, expected in cases:
            with self.subTest(case=label):
                self._recorded(label, role, kind, expected)

    def _recorded(self, label, role, kind, expected):
        root = self.root / label
        root.mkdir(mode=0o700)
        source, staging = root / 'source', root / 'staging'
        source.mkdir(mode=0o700)
        for name in self.bundle.REQUIRED:
            path = source / name
            path.write_bytes(b'SYNTHETIC INERT\x00\xff ' + name.encode() + b'\n')
            path.chmod(0o600)
        member = self.bundle.REQUIRED[0]
        target = staging / ('manifest.json' if role == 'manifest' else member)
        sentinel = root / 'sentinel'
        sentinel.write_bytes((source / member).read_bytes() if role == 'member' else b'UNRELATED SYNTHETIC SENTINEL\n')
        sentinel.chmod(0o600)
        if kind == 'source-executable':
            (source / member).chmod(0o700)
        if kind == 'source-symlink':
            (source / member).rename(root / 'held-source')
            (source / member).symlink_to(sentinel)
        sources_before = {path.name: self._identity(path) for path in source.iterdir()}
        state = {'injections': 0, 'tracked_fd': None, 'writes': 0, 'close_calls': 0,
                 'chmod_calls': 0, 'protected_before': None, 'sentinel_before': self._identity(sentinel)}
        real_open, real_write, real_close = os.open, os.write, os.close
        real_path_open, real_chmod = Path.open, os.chmod

        def inject():
            self.assertEqual(state['injections'], 0, label)
            state['injections'] += 1
            if role == 'member':
                target.rename(root / 'held-output')
            if kind in ('absolute', 'relative'):
                target.symlink_to(sentinel if kind == 'absolute' else Path('..') / 'sentinel')
            elif kind == 'hardlink':
                os.link(sentinel, target)
            else:
                target.write_bytes(sentinel.read_bytes())
                real_chmod(target, 0o600)
            state['protected_before'] = self._identity(target)
            state['sentinel_before'] = self._identity(sentinel)

        def match(path, open_kwargs=None):
            return _entry_matches(path, target, open_kwargs)

        def opening(path, writable, open_kwargs=None):
            if match(path, open_kwargs) and writable and not state['injections'] and role in ('manifest', 'collision'):
                inject()

        def open_hook(path, flags, *args, **kwargs):
            opening(path, bool(flags & (os.O_WRONLY | os.O_RDWR)), kwargs)
            fd = real_open(path, flags, *args, **kwargs)
            if match(path, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['tracked_fd'] = fd
            return fd

        def path_open_hook(path, mode='r', *args, **kwargs):
            opening(path, any(mark in mode for mark in 'wax+'))
            return real_path_open(path, mode, *args, **kwargs)

        def write_hook(fd, data):
            written = real_write(fd, data)
            if fd == state['tracked_fd']:
                state['writes'] += 1
                if role == 'member' and not state['injections']:
                    inject()
            return written

        def close_hook(fd):
            if fd == state['tracked_fd']:
                state['close_calls'] += 1
                state['tracked_fd'] = None
            return real_close(fd)

        def chmod_hook(path, mode, *args, **kwargs):
            if match(path) and state['injections']:
                state['chmod_calls'] += 1
            return real_chmod(path, mode, *args, **kwargs)

        before_fds = self._fds()
        os.open, os.write, os.close = open_hook, write_hook, close_hook
        Path.open, os.chmod = path_open_hook, chmod_hook
        outcome = None
        try:
            try:
                outcome = self.bundle.build(source, staging)
            except FileExistsError as exc:
                outcome = exc
            except self.bundle.BundleError as exc:
                outcome = exc
            except OSError as exc:
                outcome = exc
        finally:
            os.open, os.write, os.close = real_open, real_write, real_close
            Path.open, os.chmod = real_path_open, real_chmod
        self.assertEqual(self._fds(), before_fds, label)
        self.assertEqual({path.name: self._identity(path) for path in source.iterdir()}, sources_before, label)
        if role in ('member', 'manifest', 'collision'):
            self.assertEqual(state['injections'], 1, label)
        if role == 'member':
            self.assertGreaterEqual(state['writes'], 1, label)
        unchanged = True
        if state['protected_before'] is not None:
            unchanged = (self._identity(sentinel) == state['sentinel_before'] and
                         self._identity(target) == state['protected_before'])
        else:
            unchanged = self._identity(sentinel) == state['sentinel_before']
        print('recorded case=%s outcome=%s unchanged=%s chmod_calls=%s writes=%s' % (
            label, self._outcome_name(outcome), unchanged, state['chmod_calls'], state['writes']),
            file=sys.stderr)
        if expected == 'success':
            self.assertIsInstance(outcome, dict, label)
            verified = self.bundle.verify(staging)
            self.assertIs(verified['verified'], True, label)
            self.assertIs(verified['installable'], False, label)
            self.assertEqual(verified['manifest_sha256'], outcome['manifest_sha256'], label)
            self.assertTrue(unchanged, label)
            if kind == 'source-executable':
                staged = staging / member
                self.assertEqual(staged.read_bytes(), (source / member).read_bytes(), label)
                self.assertEqual(stat.S_IMODE(staged.lstat().st_mode), 0o644, label)
                self.assertFalse(stat.S_IMODE(staged.lstat().st_mode) & 0o111, label)
                self.assertEqual(stat.S_IMODE((source / member).lstat().st_mode), 0o700, label)
                self.assertEqual(staged.read_bytes(), (source / member).read_bytes(), label)
            return
        self.assertTrue(unchanged, '%s changed an occupied object chmod_calls=%s' % (label, state['chmod_calls']))
        self.assertFalse((staging / 'manifest.json').exists() and role != 'manifest', label)
        if expected == 'exclusive':
            self.assertIsInstance(outcome, FileExistsError, '%s build=%s chmod_calls=%s' % (
                label, self._outcome_name(outcome), state['chmod_calls']))
            self.assertEqual(outcome.errno, errno.EEXIST, label)
            self.assertFalse(isinstance(outcome, self.bundle.BundleError), label)
            if role == 'collision':
                self.assertFalse((staging / 'manifest.json').exists(), label)
            return
        self.assertIsInstance(outcome, self.bundle.BundleError, '%s build=%s unchanged=%s chmod_calls=%s' % (
            label, self._outcome_name(outcome), unchanged, state['chmod_calls']))
        self.assertEqual(outcome.code, expected, label)
        self.assertEqual(outcome.exit_code, 69, label)
        self.assertEqual(str(outcome), expected, label)
        self.assertEqual(outcome.args, (expected,), label)
        self.assertIsNone(outcome.__cause__, label)
        self.assertNotIn(str(root), str(outcome), label)
        self.assertNotIn('SYNTHETIC', str(outcome), label)
        if expected == 'destination_changed':
            self.assertFalse((staging / 'manifest.json').exists(), label)
            self.assertFalse((staging / self.bundle.REQUIRED[1]).exists(), label)
            self.assertEqual(state['chmod_calls'], 0, label)

    def test_preexisting_objects_fail_exclusive_open(self):
        """An occupied final component is not truncated, followed, or reused."""
        kinds = ('regular', 'same-bytes', 'hardlink', 'absolute', 'relative', 'dangling', 'chain', 'directory')
        for role in ('member', 'manifest'):
            for kind in kinds:
                with self.subTest(role=role, kind=kind):
                    self._preexisting(role, kind)

    def test_fifo_and_dangling_do_not_block_or_follow(self):
        """A fifo must not wait for a writer, and a dangling link must not create its target."""
        for role in ('member', 'manifest'):
            for kind in ('fifo', 'dangling'):
                with self.subTest(role=role, kind=kind):
                    self._occupy_subprocess(role, kind)

    def test_replacement_after_create_write_and_mode(self):
        """A different inode, link, or removed path is destination_changed even when bytes match."""
        points = {
            'member': ('after-open', 'after-write', 'before-fchmod', 'after-fchmod'),
            'manifest': ('after-open', 'after-write', 'before-lstat'),
        }
        kinds = ('same-bytes', 'absolute', 'unlink')
        for role, selected in points.items():
            for point in selected:
                for kind in kinds:
                    with self.subTest(role=role, point=point, kind=kind):
                        self._replace(role, point, kind)

    def test_member_bytes_and_mode_use_the_created_fd(self):
        """Mode 0644 and the copied bytes belong to the exclusive descriptor, not a path chmod."""
        marker = bytes((0, 255, 10, 13))
        for size in (0, 65535, 65536, 65537, 2 * 1024 * 1024):
            with self.subTest(size=size):
                payload = (marker * ((size // len(marker)) + 1))[:size]
                self.assertEqual(len(payload), size)
                self._observe_member(size, payload)

    def test_short_write_eintr_and_rejected_counts(self):
        """A real short write and write EINTR continue. Zero and negative counts do not."""
        for role in ('member', 'manifest'):
            for mode in ('short', 'eintr', 'open-eintr', 'fstat-eintr'):
                with self.subTest(role=role, mode=mode):
                    self._write_seam(role, mode, success=True)
            if role == 'member':
                with self.subTest(role=role, mode='fchmod-eintr'):
                    self._write_seam(role, 'fchmod-eintr', success=True)
            for mode in ('zero', 'negative'):
                with self.subTest(role=role, mode=mode):
                    self._write_seam(role, mode, success=False)

    def test_output_errors_stay_destination_errors(self):
        """Injected write, fstat, fchmod, and close failures are seams, not device faults."""
        for role in ('member', 'manifest'):
            seams = ('write', 'fstat', 'close-eio', 'close-eintr')
            if role == 'member':
                seams = seams + ('fchmod',)
            for seam in seams:
                for handled in (False, True):
                    with self.subTest(role=role, seam=seam, handled=handled):
                        self._output_error(role, seam, handled)

    def test_one_close_keeps_the_earlier_failure(self):
        """Close EIO must not replace an earlier refusal, and it must not close a reused descriptor."""
        for role in ('member', 'manifest'):
            for kind in ('destination_changed', 'write', 'programming', 'cancel', 'source_changed'):
                if role == 'manifest' and kind == 'source_changed':
                    continue
                with self.subTest(role=role, kind=kind):
                    self._close_preserves(role, kind)

    def test_manifest_residue_is_not_success(self):
        """A manifest created before a later failure is not deleted and is not a successful build."""
        for seam in ('write', 'close'):
            with self.subTest(seam=seam):
                self._manifest_residue(seam)

    def test_umask_members_and_manifest_modes(self):
        """Members stay 0644. Manifest keeps 0666 masked by umask. The directory stays 0700."""
        observed = self._umask()
        self.assertEqual(observed, self._umask())
        for mask in (0o022, 0o077):
            with self.subTest(umask=oct(mask)):
                self._umask_once(mask)

    def test_missing_destination_flags_do_not_truncate_or_follow(self):
        for flag in ('O_NOFOLLOW', 'O_NONBLOCK', 'fchmod'):
            with self.subTest(flag=flag):
                self._hide_destination_flag(flag)

    def _fill(self, label):
        root = self.root / label
        root.mkdir(mode=0o700)
        source = root / 'source'
        source.mkdir(mode=0o700)
        payloads = {}
        for name in self.bundle.REQUIRED:
            path = source / name
            payload = b'SYNTHETIC INERT\x00\xff ' + name.encode() + b'\n'
            path.write_bytes(payload)
            path.chmod(0o600)
            payloads[name] = payload
        return root, source, payloads

    def _preexisting(self, role, kind):
        root, source, payloads = self._fill('pre-' + role + '-' + kind)
        sentinel = root / 'sentinel'
        member = self.bundle.REQUIRED[0]
        sentinel.write_bytes(payloads[member] if kind == 'same-bytes' and role == 'member' else b'UNRELATED SENTINEL\n')
        sentinel.chmod(0o600)
        staging = root / 'staging'
        target_name = 'manifest.json' if role == 'manifest' else member
        manifest_bytes = b''
        if role == 'manifest' and kind == 'same-bytes':
            clean = root / 'clean'
            self.bundle.build(source, clean)
            manifest_bytes = (clean / 'manifest.json').read_bytes()
        state = {'planted': None, 'flags': None, 'mode': None, 'opens': 0, 'followed': []}
        real_mkdir, real_open = os.mkdir, os.open

        def plant(target):
            absent = root / 'absent-target'
            if kind == 'regular':
                self._raw_write(real_open, target, b'OCCUPIED DESTINATION\n', 0o600)
            elif kind == 'same-bytes':
                raw = manifest_bytes if role == 'manifest' else sentinel.read_bytes()
                self._raw_write(real_open, target, raw, 0o600)
            elif kind == 'hardlink':
                os.link(sentinel, target)
            elif kind == 'absolute':
                target.symlink_to(sentinel)
            elif kind == 'relative':
                target.symlink_to(Path('..') / 'sentinel')
            elif kind == 'dangling':
                target.symlink_to(absent.name)
            elif kind == 'chain':
                inner = root / 'chain-inner'
                outer = root / 'chain-outer'
                inner.write_bytes(b'CHAIN INNER\n')
                inner.chmod(0o600)
                outer.symlink_to(inner.name)
                target.symlink_to(Path('..') / outer.name)
            elif kind == 'directory':
                target.mkdir(mode=0o700)
            else:
                self.fail(kind)
            state['planted'] = self._identity(target)
            state['sentinel'] = self._identity(sentinel)
            state['absent'] = absent

        def mkdir(path, mode=0o777, *args, **kwargs):
            real_mkdir(path, mode, *args, **kwargs)
            if Path(path) == staging:
                plant(staging / target_name)

        def os_open(path, flags, *args, **kwargs):
            if _entry_matches(path, staging / target_name, kwargs):
                state['opens'] += 1
                state['flags'] = flags
                state['mode'] = args[0] if args else kwargs.get('mode')
            if not isinstance(path, int) and Path(path) == sentinel:
                state['followed'].append(str(path))
            return real_open(path, flags, *args, **kwargs)

        os.mkdir, os.open = mkdir, os_open
        fds = self._fds()
        try:
            with self.assertRaises(FileExistsError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.mkdir, os.open = real_mkdir, real_open
            self.assertEqual(self._fds(), fds)
        self.assertEqual(caught.exception.errno, errno.EEXIST)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertEqual(state['opens'], 1)
        self.assertEqual(state['flags'] & (os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_NONBLOCK),
                         os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_NONBLOCK)
        self.assertFalse(state['flags'] & getattr(os, 'O_TRUNC', 0))
        self.assertEqual(state['mode'], 0o666 if role == 'manifest' else 0o644)
        self.assertEqual(state['followed'], [])
        target = staging / target_name
        self.assertEqual(self._identity(target), state['planted'])
        self.assertEqual(self._identity(sentinel), state['sentinel'])
        if kind == 'dangling':
            self.assertFalse(state['absent'].exists())
        if kind == 'chain':
            self.assertEqual((root / 'chain-inner').read_bytes(), b'CHAIN INNER\n')
        if role == 'member':
            self.assertFalse((staging / 'manifest.json').exists())
            self.assertFalse((staging / self.bundle.REQUIRED[1]).exists())

    def _occupy_subprocess(self, role, kind):
        root, source, _payloads = self._fill('sub-' + role + '-' + kind)
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'UNRELATED SENTINEL\n')
        sentinel.chmod(0o600)
        outside = root / 'outside-target'
        staging = root / 'staging'
        try:
            completed = subprocess.run(
                [sys.executable, '-B', '-c',
                 'import sys; sys.path.insert(0, sys.argv[1]); '
                 'import test_upgrade_bundle as tests; '
                 'raise SystemExit(tests._occupy_destination(*sys.argv[2:]))',
                 str(HERE), role, kind, str(source), str(staging), str(sentinel), str(outside)],
                cwd=root, timeout=5, capture_output=True, text=True,
                env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        except subprocess.TimeoutExpired as expired:
            self.fail('destination %s %s was still blocked after 5s\n%s%s' % (
                role, kind, expired.stdout or '', expired.stderr or ''))
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertNotIn(str(root), completed.stdout)
        self.assertEqual(sentinel.read_bytes(), b'UNRELATED SENTINEL\n')
        self.assertFalse(outside.exists())

    def _replace(self, role, point, kind):
        root, source, payloads = self._fill('repl-' + role + '-' + point + '-' + kind)
        member = self.bundle.REQUIRED[0]
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'UNRELATED SENTINEL\n')
        sentinel.chmod(0o600)
        sentinel_before = self._identity(sentinel)
        source_before = {path.name: self._identity(path) for path in source.iterdir()}
        staging = root / 'staging'
        target = staging / ('manifest.json' if role == 'manifest' else member)
        state = {'done': False, 'fd': None, 'opens': 0, 'flags': 0, 'mode': None,
                 'chmod': 0, 'fchmod': [], 'closed': [], 'ebadf': [], 'protected': None}
        real_open, real_write = os.open, os.write
        real_fchmod, real_lstat, real_close, real_chmod = os.fchmod, os.lstat, os.close, os.chmod
        real_stat = os.stat

        def replace():
            self.assertFalse(state['done'])
            state['done'] = True
            payload = b''
            if target.is_file() and not target.is_symlink():
                payload = target.read_bytes()
            if target.exists() or target.is_symlink():
                target.rename(root / 'held-output')
            if kind == 'unlink':
                state['protected'] = 'absent'
                return
            if kind == 'absolute':
                target.symlink_to(sentinel)
            elif kind == 'same-bytes':
                self._raw_write(real_open, target, payload, 0o600)
            else:
                self.fail(kind)
            state['protected'] = self._identity(target)

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['fd'] = fd
                state['opens'] += 1
                state['flags'] = flags
                state['mode'] = args[0] if args else kwargs.get('mode')
                if point == 'after-open':
                    replace()
            return fd

        def os_write(fd, data):
            written = real_write(fd, data)
            if fd == state['fd'] and point == 'after-write' and not state['done'] and written > 0:
                replace()
            return written

        def os_fchmod(fd, mode):
            if fd == state['fd'] and point == 'before-fchmod' and not state['done']:
                replace()
            result = real_fchmod(fd, mode)
            if fd == state['fd']:
                state['fchmod'].append(mode)
                if point == 'after-fchmod' and not state['done']:
                    replace()
            return result

        def os_lstat(path, *args, **kwargs):
            if point == 'before-lstat' and not state['done'] and _entry_matches(path, target, kwargs):
                replace()
            return real_lstat(path, *args, **kwargs)

        def os_stat(path, *args, **kwargs):
            if point == 'before-lstat' and not state['done'] and _entry_matches(path, target, kwargs):
                replace()
            return real_stat(path, *args, **kwargs)

        def os_chmod(path, mode, *args, **kwargs):
            if not isinstance(path, int) and Path(path) == target:
                state['chmod'] += 1
            return real_chmod(path, mode, *args, **kwargs)

        def os_close(fd):
            if fd == state['fd']:
                state['closed'].append(fd)
            result = real_close(fd)
            if fd == state['fd']:
                try:
                    os.fstat(fd)
                except OSError as exc:
                    state['ebadf'].append(exc.errno == errno.EBADF)
            return result

        os.open, os.write, os.fchmod = os_open, os_write, os_fchmod
        os.lstat, os.stat, os.chmod, os.close = os_lstat, os_stat, os_chmod, os_close
        fds = self._fds()
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.write, os.fchmod = real_open, real_write, real_fchmod
            os.lstat, os.stat, os.chmod, os.close = real_lstat, real_stat, real_chmod, real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(caught.exception.code, 'destination_changed')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(str(caught.exception), 'destination_changed')
        self.assertEqual(caught.exception.args, ('destination_changed',))
        self.assertIsNone(caught.exception.__cause__)
        self.assertNotIn(str(root), str(caught.exception))
        self.assertEqual(state['opens'], 1)
        self.assertFalse(state['flags'] & getattr(os, 'O_TRUNC', 0))
        self.assertEqual(state['chmod'], 0)
        self.assertEqual(state['closed'], [state['fd']])
        self.assertEqual(state['ebadf'], [True])
        self.assertEqual(self._identity(sentinel), sentinel_before)
        self.assertEqual({path.name: self._identity(path) for path in source.iterdir()}, source_before)
        if state['protected'] == 'absent':
            self.assertFalse(target.exists())
            self.assertFalse(target.is_symlink())
        else:
            self.assertEqual(self._identity(target), state['protected'])
        if role == 'member':
            self.assertFalse((staging / 'manifest.json').exists())
            self.assertFalse((staging / self.bundle.REQUIRED[1]).exists())
            self.assertEqual(state['fchmod'], [0o644] if point in ('after-open', 'after-write', 'before-fchmod', 'after-fchmod') else [])
        held = root / 'held-output'
        self.assertTrue(held.exists() or held.is_symlink())
        if state['fchmod']:
            self.assertEqual(stat.S_IMODE(held.lstat().st_mode), 0o644)

    def _observe_member(self, size, payload):
        root, source, _payloads = self._fill('bytes-' + str(size))
        member = self.bundle.REQUIRED[0]
        target = source / member
        target.write_bytes(payload)
        target.chmod(0o600)
        before = self._identity(target)
        staging = root / 'staging'
        dest = staging / member
        manifest_path = staging / 'manifest.json'
        state = {'source': set(), 'dest': None, 'manifest': None, 'reads': [], 'read_bytes': 0,
                 'requests': [], 'fchmod': [], 'chmod': [], 'chown': [], 'truncate': [],
                 'closed': [], 'ebadf': [], 'dest_flags': 0, 'dest_mode': None,
                 'manifest_flags': 0, 'manifest_mode': None, 'manifest_fchmod': []}
        real_open, real_read, real_close = os.open, os.read, os.close
        real_fchmod, real_chmod = os.fchmod, os.chmod
        real_chown = getattr(os, 'chown', None)
        real_truncate = getattr(os, 'truncate', None)

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if isinstance(path, int):
                return fd
            if not isinstance(path, int) and Path(path) == target:
                state['source'].add(fd)
            elif _entry_matches(path, dest, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['dest'] = fd
                state['dest_ino'] = os.fstat(fd).st_ino
                state['dest_flags'] = flags
                state['dest_mode'] = args[0] if args else kwargs.get('mode')
            elif _entry_matches(path, manifest_path, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['manifest'] = fd
                state['manifest_flags'] = flags
                state['manifest_mode'] = args[0] if args else kwargs.get('mode')
            return fd

        def os_read(fd, count):
            if fd in state['source']:
                state['requests'].append(count)
            data = real_read(fd, count)
            if fd in state['source']:
                state['read_bytes'] += len(data)
            return data

        def os_fchmod(fd, mode):
            state['fchmod'].append((os.fstat(fd).st_ino, mode))
            if fd == state['manifest']:
                state['manifest_fchmod'].append(mode)
            return real_fchmod(fd, mode)

        def os_chmod(path, mode, *args, **kwargs):
            state['chmod'].append(os.fspath(path))
            return real_chmod(path, mode, *args, **kwargs)

        def os_chown(path, *args, **kwargs):
            state['chown'].append(os.fspath(path))
            return real_chown(path, *args, **kwargs)

        def os_truncate(path, length):
            state['truncate'].append(os.fspath(path))
            return real_truncate(path, length)

        def os_close(fd):
            role = None
            if state['manifest'] is not None and fd == state['manifest']:
                role = 'manifest'
                state['manifest'] = None
            elif state['dest'] is not None and fd == state['dest']:
                role = 'dest'
                state['dest'] = None
            elif fd in state['source']:
                role = 'source'
                state['source'].discard(fd)
            if role:
                state['closed'].append(role)
            result = real_close(fd)
            if role:
                try:
                    os.fstat(fd)
                except OSError as exc:
                    state['ebadf'].append(exc.errno == errno.EBADF)
            return result

        os.open, os.read, os.close = os_open, os_read, os_close
        os.fchmod, os.chmod = os_fchmod, os_chmod
        if real_chown is not None:
            os.chown = os_chown
        if real_truncate is not None:
            os.truncate = os_truncate
        fds = self._fds()
        try:
            built = self.bundle.build(source, staging)
        finally:
            os.open, os.read, os.close = real_open, real_read, real_close
            os.fchmod, os.chmod = real_fchmod, real_chmod
            if real_chown is not None:
                os.chown = real_chown
            if real_truncate is not None:
                os.truncate = real_truncate
            self.assertEqual(self._fds(), fds)
        self.assertEqual(self._identity(target), before)
        self.assertEqual(dest.read_bytes(), payload)
        self.assertEqual(dest.lstat().st_ino, state['dest_ino'])
        self.assertEqual(stat.S_IMODE(dest.lstat().st_mode), 0o644)
        manifest = json.loads(manifest_path.read_bytes())
        self.assertEqual(manifest['files'][member], hashlib.sha256(payload).hexdigest())
        verified = self.bundle.verify(staging)
        self.assertIs(verified['verified'], True)
        self.assertIs(verified['installable'], False)
        self.assertEqual(verified['manifest_sha256'], built['manifest_sha256'])
        self.assertEqual(state['dest_flags'], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_NONBLOCK)
        self.assertEqual(state['dest_mode'], 0o644)
        self.assertEqual([mode for ino, mode in state['fchmod'] if ino == state['dest_ino']], [0o644])
        self.assertEqual(len(state['fchmod']), len(self.bundle.REQUIRED))
        self.assertTrue(all(mode == 0o644 for _ino, mode in state['fchmod']))
        self.assertEqual(state['manifest_flags'], os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_NONBLOCK)
        self.assertEqual(state['manifest_mode'], 0o666)
        self.assertEqual(state['manifest_fchmod'], [])
        self.assertEqual(state['chmod'], [])
        self.assertEqual(state['chown'], [])
        self.assertEqual(state['truncate'], [])
        self.assertEqual(state['source'], set())
        self.assertLessEqual(max(state['requests']), self.bundle._MEMBER_CHUNK)
        self.assertIn(1, state['requests'])
        self.assertEqual(state['read_bytes'], size)
        self.assertEqual(state['ebadf'], [True] * len(state['closed']))
        self.assertEqual(state['closed'].count('dest'), 1)
        self.assertEqual(state['closed'].count('manifest'), 1)
        self.assertEqual(state['closed'].count('source'), 1)

    def _write_seam(self, role, mode, success):
        root, source, payloads = self._fill('seam-' + role + '-' + mode)
        member = self.bundle.REQUIRED[0]
        if role == 'member':
            payload = b'ABCDEFGH'
            (source / member).write_bytes(payload)
            (source / member).chmod(0o600)
        else:
            payload = None
        staging = root / 'staging'
        target = staging / ('manifest.json' if role == 'manifest' else member)
        state = {'fds': set(), 'once': False, 'written': bytearray()}
        real_open, real_write, real_fstat, real_fchmod = os.open, os.write, os.fstat, os.fchmod

        def selected(fd):
            return fd in state['fds']

        def os_open(path, flags, *args, **kwargs):
            if mode == 'open-eintr' and not state['once'] and _entry_matches(path, target, kwargs):
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted destination open')
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['fds'].add(fd)
            return fd

        def os_write(fd, data):
            if selected(fd) and mode in ('short', 'eintr', 'zero', 'negative') and not state['once']:
                state['once'] = True
                if mode == 'eintr':
                    raise OSError(errno.EINTR, 'interrupted destination write')
                if mode == 'zero':
                    return 0
                if mode == 'negative':
                    return -1
                if len(data) > 1:
                    wrote = real_write(fd, data[:1])
                    state['written'].extend(data[:wrote])
                    return wrote
            wrote = real_write(fd, data)
            if selected(fd) and wrote > 0:
                state['written'].extend(data[:wrote])
            return wrote

        def os_fstat(fd):
            if selected(fd) and mode == 'fstat-eintr' and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted destination fstat')
            return real_fstat(fd)

        def os_fchmod(fd, bits):
            if selected(fd) and mode == 'fchmod-eintr' and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted destination fchmod')
            return real_fchmod(fd, bits)

        os.open, os.write, os.fstat, os.fchmod = os_open, os_write, os_fstat, os_fchmod
        fds = self._fds()
        try:
            if success:
                built = self.bundle.build(source, staging)
            else:
                with self.assertRaises(OSError) as caught:
                    self.bundle.build(source, staging)
        finally:
            os.open, os.write, os.fstat, os.fchmod = real_open, real_write, real_fstat, real_fchmod
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['once'])
        if success:
            self.assertIsInstance(built, dict)
            if role == 'member':
                self.assertEqual((staging / member).read_bytes(), payload)
                self.assertEqual(bytes(state['written'])[:len(payload)], payload)
            else:
                raw = target.read_bytes()
                self.assertEqual(bytes(state['written']), raw)
                self.assertGreater(len(raw), 1)
            verified = self.bundle.verify(staging)
            self.assertIs(verified['installable'], False)
            self.assertEqual(verified['manifest_sha256'], built['manifest_sha256'])
            return
        self.assertEqual(caught.exception.errno, errno.EIO)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertNotEqual(getattr(caught.exception, 'code', None), 'source_unreadable')
        self.assertIn('incomplete destination write', str(caught.exception))
        if role == 'member':
            self.assertFalse((staging / 'manifest.json').exists())
            self.assertNotEqual((staging / member).read_bytes() if (staging / member).is_file() else b'', payload)
        else:
            self.assertTrue(target.is_file())
            self.assertNotEqual(target.read_bytes(), bytes(state['written']) + payloads[member])

    def _output_error(self, role, seam, handled):
        root, source, _payloads = self._fill('err-' + role + '-' + seam + '-' + str(handled))
        member = self.bundle.REQUIRED[0]
        fallback_bytes = b'FALLBACK MUST STAY UNREAD\n'
        primary = source / 'release-source.json'
        fork = root / 'fork'
        fork.mkdir(mode=0o700)
        fallback = fork / 'release-source.json'
        fallback.write_bytes(fallback_bytes)
        fallback.chmod(0o600)
        fallback_before = self._identity(fallback)
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'SENTINEL\n')
        sentinel.chmod(0o600)
        sentinel_before = self._identity(sentinel)
        staging = root / 'staging'
        target = staging / ('manifest.json' if role == 'manifest' else member)
        number = errno.EINTR if seam == 'close-eintr' else errno.EACCES if seam == 'fchmod' else errno.EIO
        state = {'fds': set(), 'done': False, 'closed': [], 'double': 0, 'occupied': None,
                 'ebadf': False, 'opened': []}
        real_open, real_write, real_fstat = os.open, os.write, os.fstat
        real_fchmod, real_close = os.fchmod, os.close

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int):
                state['opened'].append(os.path.normpath(os.fspath(path)))
                if _entry_matches(path, target, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                    state['fds'].add(fd)
            return fd

        def os_write(fd, data):
            if seam == 'write' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EIO, 'injected destination write')
            return real_write(fd, data)

        def os_fstat(fd):
            if seam == 'fstat' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EIO, 'injected destination fstat')
            return real_fstat(fd)

        def os_fchmod(fd, mode):
            if seam == 'fchmod' and fd in state['fds'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EACCES, 'injected destination fchmod')
            return real_fchmod(fd, mode)

        def os_close(fd):
            if fd == state['occupied']:
                state['double'] += 1
                return real_close(fd)
            if fd in state['fds'] and seam.startswith('close') and not state['done']:
                real_close(fd)
                state['done'] = True
                state['closed'].append(fd)
                try:
                    real_fstat(fd)
                except OSError as exc:
                    state['ebadf'] = exc.errno == errno.EBADF
                state['occupied'] = real_open(sentinel, os.O_RDONLY)
                raise OSError(number, 'injected destination close')
            if fd in state['fds']:
                state['closed'].append(fd)
            return real_close(fd)

        def invoke():
            if not handled:
                self.bundle.build(source, staging)
                return
            try:
                raise ValueError('already-handled-caller-error')
            except ValueError:
                self.bundle.build(source, staging)

        os.open, os.write, os.fstat = os_open, os_write, os_fstat
        os.fchmod, os.close = os_fchmod, os_close
        fds = self._fds()
        try:
            with self.assertRaises(OSError) as caught:
                invoke()
        finally:
            os.open, os.write, os.fstat = real_open, real_write, real_fstat
            os.fchmod, os.close = real_fchmod, real_close
            if state['occupied'] is not None:
                try:
                    os.fstat(state['occupied'])
                except OSError:
                    self.fail('reused descriptor was closed')
                os.close(state['occupied'])
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(caught.exception.errno, number)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertNotEqual(getattr(caught.exception, 'code', None), 'source_unreadable')
        self.assertNotIn(str(fallback), state['opened'])
        self.assertEqual(self._identity(fallback), fallback_before)
        self.assertEqual(fallback.read_bytes(), fallback_bytes)
        self.assertEqual(sentinel.read_bytes(), b'SENTINEL\n')
        self.assertEqual(self._identity(sentinel), sentinel_before)
        self.assertEqual(state['double'], 0)
        self.assertEqual(len(state['closed']), 1)
        if seam.startswith('close'):
            self.assertTrue(state['ebadf'])
        if role == 'member':
            self.assertFalse((staging / 'manifest.json').exists())

    def _close_preserves(self, role, kind):
        root, source, payloads = self._fill('keep-' + role + '-' + kind)
        member = self.bundle.REQUIRED[0]
        (source / member).write_bytes(b'KEEP PRIOR ' + kind.encode())
        (source / member).chmod(0o600)
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'SENTINEL\n')
        sentinel.chmod(0o600)
        sentinel_before = self._identity(sentinel)
        staging = root / 'staging'
        target = staging / ('manifest.json' if role == 'manifest' else member)
        source_fd = {'value': None}
        marker = ProgrammingMarker('SYNTHETIC PROGRAMMING MARKER') if kind == 'programming' else CancelMarker('SYNTHETIC CANCEL')
        state = {'dest': None, 'done': False, 'closed': [], 'occupied': None, 'double': 0, 'ebadf': False,
                 'grew': False}
        real_open, real_read, real_write = os.open, os.read, os.write
        real_fstat, real_close, real_lstat, real_stat = os.fstat, os.close, os.lstat, os.stat
        append_fd = None

        def os_open(path, flags, *args, **kwargs):
            nonlocal append_fd
            fd = real_open(path, flags, *args, **kwargs)
            if isinstance(path, int):
                return fd
            if not isinstance(path, int) and Path(path) == source / member and not flags & (os.O_WRONLY | os.O_RDWR):
                source_fd['value'] = fd
                if kind == 'source_changed' and append_fd is None:
                    append_fd = real_open(path, os.O_WRONLY | os.O_APPEND)
            if _entry_matches(path, target, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['dest'] = fd
            return fd

        def os_read(fd, count):
            if kind == 'source_changed' and fd == source_fd['value'] and count == 1 and not state['grew']:
                state['grew'] = True
                real_write(append_fd, b'X')
            return real_read(fd, count)

        def os_write(fd, data):
            if kind == 'write' and fd == state['dest'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EACCES, 'injected destination write')
            return real_write(fd, data)

        def os_fstat(fd):
            if kind in ('programming', 'cancel') and fd == state['dest'] and not state['done']:
                state['done'] = True
                raise marker
            return real_fstat(fd)

        def replace_bound_name(path, stat_kwargs):
            if (kind == 'destination_changed' and state['dest'] is not None and not state['done']
                    and _entry_matches(path, target, stat_kwargs)):
                state['done'] = True
                os.rename(target, root / 'held-output')
                self._raw_write(real_open, target, b'SAME OR NOT\n', 0o600)

        def os_lstat(path, *args, **kwargs):
            replace_bound_name(path, kwargs)
            return real_lstat(path, *args, **kwargs)

        def os_stat(path, *args, **kwargs):
            replace_bound_name(path, kwargs)
            return real_stat(path, *args, **kwargs)

        def os_close(fd):
            if fd == state['occupied']:
                state['double'] += 1
                return real_close(fd)
            if fd == state['dest'] and fd not in state['closed']:
                real_close(fd)
                state['closed'].append(fd)
                try:
                    real_fstat(fd)
                except OSError as exc:
                    state['ebadf'] = exc.errno == errno.EBADF
                state['occupied'] = real_open(sentinel, os.O_RDONLY)
                raise OSError(errno.EIO, 'injected destination close')
            return real_close(fd)

        os.open, os.read, os.write = os_open, os_read, os_write
        os.fstat, os.lstat, os.stat, os.close = os_fstat, os_lstat, os_stat, os_close
        fds = self._fds()
        try:
            with self.assertRaises((self.bundle.BundleError, OSError, ProgrammingMarker, CancelMarker)) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.read, os.write = real_open, real_read, real_write
            os.fstat, os.lstat, os.stat, os.close = real_fstat, real_lstat, real_stat, real_close
            if append_fd is not None:
                os.close(append_fd)
            if state['occupied'] is not None:
                self.assertEqual(state['double'], 0)
                os.fstat(state['occupied'])
                os.close(state['occupied'])
            self.assertEqual(self._fds(), fds)
        self.assertEqual(state['closed'], [state['dest']])
        self.assertTrue(state['ebadf'])
        self.assertEqual(self._identity(sentinel), sentinel_before)
        error = caught.exception
        if kind == 'destination_changed':
            self.assertIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.code, 'destination_changed')
            self.assertEqual(str(error), 'destination_changed')
            self.assertEqual(self._identity(target)['sha256'], hashlib.sha256(b'SAME OR NOT\n').hexdigest())
            self.assertEqual(stat.S_IMODE(target.lstat().st_mode), 0o600)
        elif kind == 'write':
            self.assertIsInstance(error, OSError)
            self.assertEqual(error.errno, errno.EACCES)
            self.assertIn('injected destination write', str(error))
            self.assertNotIn('injected destination close', str(error))
        elif kind == 'programming':
            self.assertIs(error, marker)
        elif kind == 'cancel':
            self.assertIs(error, marker)
        else:
            self.assertIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.code, 'source_changed')
            self.assertTrue(state['grew'])
        if role == 'member':
            self.assertFalse((staging / 'manifest.json').exists())
        self.assertNotIn('approval', {path.name for path in staging.rglob('*')} if staging.exists() else set())
        self.assertNotEqual(payloads[member], b'')

    def _manifest_residue(self, seam):
        root, source, _payloads = self._fill('residue-' + seam)
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'SENTINEL\n')
        sentinel.chmod(0o600)
        before = self._identity(sentinel)
        source_before = {path.name: self._identity(path) for path in source.iterdir()}
        staging = root / 'staging'
        target = staging / 'manifest.json'
        state = {'opens': 0, 'done': False}
        real_open, real_write, real_close = os.open, os.write, os.close
        manifest_fd = {'value': None}

        def os_open2(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if _entry_matches(path, target, kwargs) and flags & (os.O_WRONLY | os.O_RDWR):
                state['opens'] += 1
                manifest_fd['value'] = fd
            return fd

        def os_write2(fd, data):
            if seam == 'write' and fd == manifest_fd['value'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EIO, 'injected manifest write')
            return real_write(fd, data)

        def os_close2(fd):
            if seam == 'close' and fd == manifest_fd['value'] and not state['done']:
                state['done'] = True
                real_close(fd)
                raise OSError(errno.EIO, 'injected manifest close')
            return real_close(fd)

        os.open, os.write, os.close = os_open2, os_write2, os_close2
        fds = self._fds()
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.write, os.close = real_open, real_write, real_close
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(state['opens'], 1)
        self.assertEqual(caught.exception.errno, errno.EIO)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertTrue(target.is_file())
        self.assertEqual(self._identity(sentinel), before)
        self.assertEqual({path.name: self._identity(path) for path in source.iterdir()}, source_before)
        names = {path.name for path in staging.iterdir()}
        self.assertEqual(names, set(self.bundle.REQUIRED) | {'manifest.json'})
        self.assertFalse((staging / 'approval.json').exists())
        if seam == 'close':
            raw = target.read_bytes()
            self.assertGreater(len(raw), 0)
            verified = self.bundle.verify(staging)
            self.assertIs(verified['installable'], False)
        else:
            self.assertEqual(target.read_bytes(), b'')

    def _umask(self):
        current = os.umask(0o022)
        os.umask(current)
        return current

    def _umask_once(self, mask):
        previous = self._umask()
        self.addCleanup(lambda: os.umask(previous))
        root, source, payloads = self._fill('umask-' + oct(mask)[2:])
        os.umask(mask)
        first = root / 'staging-a'
        second = root / 'staging-b'
        chmod_calls = []
        real_chmod = os.chmod

        def os_chmod(path, mode, *args, **kwargs):
            chmod_calls.append(os.fspath(path))
            return real_chmod(path, mode, *args, **kwargs)

        os.chmod = os_chmod
        try:
            built = self.bundle.build(source, first)
            again = self.bundle.build(source, second)
        finally:
            os.chmod = real_chmod
            os.umask(previous)
        self.assertEqual(chmod_calls, [])
        self.assertEqual(built['manifest_sha256'], again['manifest_sha256'])
        raw = (first / 'manifest.json').read_bytes()
        self.assertEqual((second / 'manifest.json').read_bytes(), raw)
        self.assertEqual(hashlib.sha256(raw).hexdigest(), built['manifest_sha256'])
        self.assertEqual(stat.S_IMODE(first.lstat().st_mode), 0o700)
        self.assertEqual(stat.S_IMODE((first / 'manifest.json').lstat().st_mode), 0o666 & ~mask)
        for name in self.bundle.REQUIRED:
            staged = first / name
            self.assertEqual(staged.read_bytes(), payloads[name])
            self.assertEqual(stat.S_IMODE(staged.lstat().st_mode), 0o644)
        verified = self.bundle.verify(first)
        self.assertIs(verified['verified'], True)
        self.assertIs(verified['installable'], False)
        self.assertEqual(self._umask(), previous)

    def _hide_destination_flag(self, flag):
        root, source, _payloads = self._fill('hide-' + flag)
        member = self.bundle.REQUIRED[0]
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'SENTINEL\n')
        sentinel.chmod(0o600)
        before = self._identity(sentinel)
        staging = root / 'staging'
        target = staging / member
        state = {'hidden': False, 'opens': []}
        real_mkdir, real_open, real_chmod = os.mkdir, os.open, os.chmod
        saved = os.fchmod if flag == 'fchmod' else getattr(os, flag)

        def mkdir(path, mode=0o777, *args, **kwargs):
            real_mkdir(path, mode, *args, **kwargs)
            if Path(path) == staging:
                (staging / member).symlink_to(sentinel)

        def os_open(path, flags, *args, **kwargs):
            if not isinstance(path, int) and Path(path) == source / member and not state['hidden']:
                state['hidden'] = True
                if flag == 'fchmod':
                    del os.fchmod
                else:
                    delattr(os, flag)
            if not isinstance(path, int) and Path(path) in (target, sentinel):
                state['opens'].append(os.path.normpath(os.fspath(path)))
            return real_open(path, flags, *args, **kwargs)

        def os_chmod(path, mode, *args, **kwargs):
            if not isinstance(path, int) and Path(path) in (target, sentinel):
                self.fail('path chmod used as a destination fallback')
            return real_chmod(path, mode, *args, **kwargs)

        os.mkdir, os.open, os.chmod = mkdir, os_open, os_chmod
        fds = self._fds()
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.mkdir, os.open, os.chmod = real_mkdir, real_open, real_chmod
            if flag == 'fchmod':
                os.fchmod = saved
            else:
                setattr(os, flag, saved)
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['hidden'])
        self.assertEqual(caught.exception.errno, errno.ENOTSUP)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertEqual(state['opens'], [])
        self.assertEqual(self._identity(sentinel), before)
        self.assertTrue(target.is_symlink())
        self.assertFalse((staging / 'manifest.json').exists())

    def _raw_write(self, opener, path, data, mode):
        fd = opener(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
        try:
            view = memoryview(data)
            while view:
                wrote = os.write(fd, view)
                if wrote <= 0:
                    self.fail('fixture write stalled')
                view = view[wrote:]
        finally:
            os.close(fd)

    def _outcome_name(self, outcome):
        if isinstance(outcome, dict):
            return 'success'
        if isinstance(outcome, self.bundle.BundleError):
            return outcome.code
        if isinstance(outcome, OSError):
            return '%s:%s' % (type(outcome).__name__, outcome.errno)
        return type(outcome).__name__

    def _identity(self, path):
        info = path.lstat()
        result = {'dev': info.st_dev, 'ino': info.st_ino, 'mode': info.st_mode, 'uid': info.st_uid,
                  'size': info.st_size, 'mtime_ns': info.st_mtime_ns, 'ctime_ns': info.st_ctime_ns}
        if stat.S_ISREG(info.st_mode) and not stat.S_ISLNK(info.st_mode):
            result['sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
        elif stat.S_ISLNK(info.st_mode):
            result['link'] = os.readlink(path)
        return result

    def _fds(self):
        base = '/proc/self/fd' if os.path.isdir('/proc/self/fd') else '/dev/fd'
        return frozenset(os.listdir(base))


class BuilderOutputDirectoryTests(unittest.TestCase):
    """Builder outputs stay on the staging directory descriptor held after mkdir.

    The twelve recorded replacements are red on the pre-fix builder because it
    returns success and writes the replacement. The five controls keep the
    previous results. A swap before the first directory lstat is not claimed
    to identify the mkdir inode.
    """

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        actual = Path(bundle.__file__).resolve()
        expected = (HERE / 'upgrade_bundle.py').resolve()
        self.assertEqual(actual, expected, 'refusing to test an imported module outside this tree')
        self.assertNotEqual(os.getuid(), 0)
        self.module_path = str(actual)
        self.module_sha256 = hashlib.sha256(actual.read_bytes()).hexdigest()
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-outdir-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.root.chmod(0o700)

    def test_recorded_replacement_refuses_without_writing_the_new_directory(self):
        cases = tuple(
            (phase, kind)
            for phase in ('first', 'middle', 'last', 'manifest')
            for kind in ('directory', 'absolute_symlink', 'relative_symlink')
        )
        self.assertEqual(len(cases), 12)
        for phase, kind in cases:
            with self.subTest(phase=phase, kind=kind):
                self._replacement(phase + '-' + kind, phase, kind)

    def test_recorded_controls_keep_previous_results(self):
        cases = (
            ('ordinary', 'success'),
            ('source_exec', 'success'),
            ('ancestor_symlink', 'success'),
            ('source_symlink', 'member_missing'),
            ('existing_staging', 'staging_exists'),
        )
        self.assertEqual(len(cases), 5)
        for control, expected in cases:
            with self.subTest(control=control):
                self._control(control, expected)

    def _replacement(self, label, phase, kind):
        state = self._run_case(label, phase=phase, kind=kind)
        detail = 'module=%s sha256=%s build=%s replacement_bytes=%s replacement_opens=%s replacement_chmods=%s injections=%s source_opens=%s' % (
            self.module_path, self.module_sha256, state['build'],
            state['output_write_bytes']['replacement'], state['replacement_opens'],
            state['output_chmods']['replacement'], state['injections'], state['source_opens'])
        self.assertEqual(state['build'], 'destination_changed', detail)
        self.assertEqual(state['exit_code'], 69, detail)
        self.assertEqual(state['message'], 'destination_changed', detail)
        self.assertIsNone(state['cause'], detail)
        self.assertEqual(state['injections'], 1, detail)
        self.assertGreaterEqual(state['source_opens'], 1, detail)
        self.assertEqual(state['output_write_bytes']['replacement'], 0, detail)
        self.assertEqual(state['output_chmods']['replacement'], 0, detail)
        self.assertEqual(state['replacement_opens'], 0, detail)
        self.assertEqual(state['replacement_snapshot'], state['replacement_before'], detail)
        self.assertNotEqual(state['cloned_prefix'], 'builder')

    def _control(self, control, expected):
        state = self._run_case('control-' + control, control=control)
        detail = 'module=%s sha256=%s control=%s build=%s' % (
            self.module_path, self.module_sha256, control, state['build'])
        self.assertEqual(state['build'], expected, detail)
        self.assertEqual(state['injections'], 0, detail)
        self.assertTrue(state['source_unchanged'], detail)
        self.assertTrue(state['sentinel_unchanged'], detail)
        self.assertTrue(state['fd_set_unchanged'], detail)
        if expected == 'success':
            self.assertIs(state['verify']['verified'], True, detail)
            self.assertIs(state['verify']['installable'], False, detail)
            self.assertEqual(state['verify']['manifest_sha256'], state['manifest_sha256'], detail)
            if control == 'source_exec':
                staged = state['staging'] / self.bundle.REQUIRED[0]
                self.assertEqual(stat.S_IMODE(staged.lstat().st_mode), 0o644, detail)
                self.assertEqual(stat.S_IMODE((state['source'] / self.bundle.REQUIRED[0]).lstat().st_mode), 0o700, detail)
        else:
            self.assertEqual(state['exit_code'], 69, detail)
            self.assertEqual(state['message'], expected, detail)

    def _run_case(self, label, phase=None, kind='directory', control=None):
        bundle = self.bundle
        root = self.root / label
        root.mkdir(mode=0o700)
        source = root / 'source'
        source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            path = source / name
            path.write_bytes(b'INERT\x00\xff fixture ' + name.encode() + b'\n')
            path.chmod(0o600)
        if control == 'source_exec':
            (source / bundle.REQUIRED[0]).chmod(0o700)
        staging = root / 'staging'
        held = root / 'held'
        alternate = root / 'alternate'
        alternate.mkdir(mode=0o700)
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'PRIVATE SYNTHETIC SENTINEL\x00')
        sentinel.chmod(0o600)
        if control == 'source_symlink':
            (source / bundle.REQUIRED[0]).rename(root / 'source-held')
            (source / bundle.REQUIRED[0]).symlink_to(sentinel)
        if control == 'existing_staging':
            staging.mkdir(mode=0o700)
        if control == 'ancestor_symlink':
            (root / 'parent').mkdir(mode=0o700)
            (root / 'alias').symlink_to(root / 'parent', target_is_directory=True)
            staging = root / 'alias' / 'staging'
        source_before = self._snapshot(source)
        sentinel_before = self._stable(sentinel)
        source_target = {'first': bundle.REQUIRED[0], 'middle': bundle.REQUIRED[len(bundle.REQUIRED) // 2],
                         'last': bundle.REQUIRED[-1]}.get(phase)
        state = {
            'injections': 0, 'output_write_bytes': {'original': 0, 'replacement': 0},
            'output_chmods': {'original': 0, 'replacement': 0}, 'replacement_opens': 0,
            'source_opens': 0, 'cloned_prefix': 0, 'real_source_reads': 0,
        }
        fd_role = {}
        root_ids = {}
        before_alternate = None

        def swap():
            nonlocal before_alternate
            self.assertEqual(state['injections'], 0, label)
            root_ids['original'] = self._ident(staging)
            for child in staging.iterdir():
                self.assertTrue(child.is_file(), label)
                self.assertFalse(child.is_symlink(), label)
                copied = alternate / child.name
                copied.write_bytes(child.read_bytes())
                copied.chmod(stat.S_IMODE(child.stat().st_mode))
                state['cloned_prefix'] += 1
            staging.rename(held)
            if kind == 'directory':
                alternate.rename(staging)
                replacement = staging
            else:
                link = alternate if kind == 'absolute_symlink' else Path('alternate')
                staging.symlink_to(link, target_is_directory=True)
                replacement = alternate
            root_ids['replacement'] = self._ident(replacement)
            before_alternate = self._snapshot(replacement)
            state['replacement'] = replacement
            state['injections'] += 1

        real_open, real_close = os.open, os.close
        real_write, real_read, real_fchmod = os.write, os.read, os.fchmod

        def open_hook(path, flags, mode=0o777, *, dir_fd=None):
            name = Path(path).name if not isinstance(path, int) else ''
            is_output = bool(flags & os.O_CREAT)
            if phase == 'manifest' and is_output and name == 'manifest.json' and not state['injections']:
                swap()
            fd = real_open(path, flags, mode, dir_fd=dir_fd)
            source_open = (not is_output and not isinstance(path, int) and Path(path).parent == source)
            if source_open:
                state['source_opens'] += 1
                if source_target and Path(path).name == source_target and not state['injections']:
                    swap()
            if is_output:
                if dir_fd is not None:
                    info = os.fstat(dir_fd)
                    parent = (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode))
                else:
                    parent = self._ident(Path(path).parent.resolve())
                role = 'replacement' if parent == root_ids.get('replacement') else 'original'
                fd_role[fd] = role
                if role == 'replacement':
                    state['replacement_opens'] += 1
            return fd

        def write_hook(fd, data):
            written = real_write(fd, data)
            if fd in fd_role:
                state['output_write_bytes'][fd_role[fd]] += written
            return written

        def chmod_hook(fd, mode):
            real_fchmod(fd, mode)
            if fd in fd_role:
                state['output_chmods'][fd_role[fd]] += 1

        def close_hook(fd):
            real_close(fd)
            fd_role.pop(fd, None)

        def read_hook(fd, size):
            raw = real_read(fd, size)
            state['real_source_reads'] += len(raw)
            return raw

        before_fds = self._open_fds()
        os.open, os.write, os.fchmod = open_hook, write_hook, chmod_hook
        os.close, os.read = close_hook, read_hook
        outcome = None
        try:
            try:
                outcome = bundle.build(source, staging)
                state['build'] = 'success'
                state['exit_code'] = None
                state['message'] = None
                state['cause'] = None
                state['manifest_sha256'] = outcome['manifest_sha256']
            except bundle.BundleError as exc:
                state['build'] = exc.code
                state['exit_code'] = exc.exit_code
                state['message'] = str(exc)
                state['cause'] = exc.__cause__
                state['manifest_sha256'] = None
            except OSError as exc:
                state['build'] = '%s:%s' % (type(exc).__name__, exc.errno)
                state['exit_code'] = None
                state['message'] = str(exc)
                state['cause'] = exc.__cause__
                state['manifest_sha256'] = None
        finally:
            os.open, os.write, os.fchmod = real_open, real_write, real_fchmod
            os.close, os.read = real_close, real_read
        after_fds = self._open_fds()
        state['fd_set_unchanged'] = before_fds == after_fds
        state['source_unchanged'] = self._snapshot(source) == source_before
        state['sentinel_unchanged'] = self._stable(sentinel) == sentinel_before
        state['source'] = source
        state['staging'] = staging
        state['verify'] = None
        if state['build'] == 'success':
            try:
                state['verify'] = bundle.verify(staging)
            except bundle.BundleError as exc:
                state['verify'] = {'error': exc.code, 'exit_code': exc.exit_code}
        if phase:
            replacement = state['replacement']
            state['replacement_snapshot'] = self._snapshot(replacement)
            state['replacement_before'] = before_alternate
        self.assertTrue(state['source_unchanged'], label)
        self.assertTrue(state['sentinel_unchanged'], label)
        self.assertTrue(state['fd_set_unchanged'], label)
        return state

    def _ident(self, path):
        info = os.stat(path, follow_symlinks=False)
        return (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode))

    def _stable(self, path):
        info = path.lstat()
        return (path.read_bytes(), info.st_ino, info.st_mode, info.st_mtime_ns, info.st_ctime_ns)

    def _snapshot(self, path):
        rows = {}
        found = [path] + sorted(path.rglob('*'))
        for item in found:
            info = item.lstat()
            row = [info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
                   info.st_mtime_ns, info.st_ctime_ns]
            if stat.S_ISREG(info.st_mode):
                row.append(hashlib.sha256(item.read_bytes()).hexdigest())
            elif stat.S_ISLNK(info.st_mode):
                row.append(os.readlink(item))
            rows[str(item.relative_to(path))] = tuple(row)
        return tuple(sorted(rows.items()))

    def _open_fds(self):
        return {fd for fd in range(3, 128) if self._fd_open(fd)}

    def _fd_open(self, fd):
        try:
            os.fstat(fd)
        except OSError:
            return False
        return True

    def test_anchor_matrix_writes_only_the_held_directory(self):
        points = ('after-lstat', 'after-open', 'after-first-close', 'after-middle-close',
                  'after-last-close', 'before-manifest', 'after-manifest-close')
        kinds = ('directory', 'absolute_symlink', 'relative_symlink', 'unlink', 'same-bytes')
        for point in points:
            for kind in kinds:
                with self.subTest(point=point, kind=kind):
                    self._anchor(point, kind)

    def test_open_and_stat_stay_on_the_held_descriptor(self):
        self._assert_bound_success('bound-success')
        self._swap_before_manifest_stat('stat-dirfd')

    def test_first_observation_rejects_unexpected_roots(self):
        for kind in ('symlink', 'file', 'missing'):
            with self.subTest(kind=kind):
                self._plant_root(kind)
        self._plant_root_subprocess('fifo')

    def test_missing_capability_does_not_fall_back_to_path_creates(self):
        cases = (
            ('O_DIRECTORY', None, None),
            ('O_NOFOLLOW', None, None),
            ('O_NONBLOCK', None, None),
            (None, 'supports_dir_fd', os.open),
            (None, 'supports_dir_fd', os.stat),
            (None, 'supports_follow_symlinks', os.stat),
        )
        for flag, attr, drop in cases:
            with self.subTest(flag=flag, attr=attr, drop=getattr(drop, '__name__', None)):
                self._hide_capability(flag, attr, drop)
        self._not_implemented_boundaries()

    def test_directory_errors_and_one_close(self):
        for where, number, code in (
                ('lstat', errno.EIO, 'oserror'),
                ('lstat', errno.ENOENT, 'destination_changed'),
                ('open', errno.EIO, 'oserror'),
                ('open', errno.ELOOP, 'destination_changed'),
                ('fstat', errno.EIO, 'oserror'),
                ('fstat-identity', None, 'destination_changed'),
                ('lstat-type', None, 'typeerror')):
            with self.subTest(where=where, code=code):
                self._directory_fault(where, number, code)
        for number in (errno.EIO, errno.EINTR, errno.EBADF):
            for handled in (False, True):
                with self.subTest(close=errno.errorcode[number], handled=handled):
                    self._directory_close(number, handled)
        for where in ('lstat', 'open', 'fstat'):
            with self.subTest(eintr=where):
                self._directory_eintr(where)
        for kind in ('source_unreadable', 'destination_oserror', 'destination_changed',
                     'runtime', 'cancel', 'source_changed'):
            with self.subTest(kept=kind):
                self._close_keeps(kind)

    def test_listing_capability_is_not_required_for_build(self):
        root, source = self._fill('no-list')
        staging = root / 'staging'
        saved = os.supports_fd
        os.supports_fd = set(saved) - {os.listdir}
        try:
            built = self.bundle.build(source, staging)
        finally:
            os.supports_fd = saved
        self._assert_verified(staging, built)

    def test_legal_paths_fallback_and_empty_member_use_one_directory(self):
        self._relative_and_absolute()
        self._hardlink_and_empty()
        self._fallback_uses_dirfd()

    def _fill(self, label):
        root = self.root / label
        root.mkdir(mode=0o700)
        source = root / 'source'
        source.mkdir(mode=0o700)
        for name in self.bundle.REQUIRED:
            path = source / name
            path.write_bytes(b'INERT\x00\xff ' + name.encode() + b'\n')
            path.chmod(0o600)
        return root, source

    def _assert_verified(self, staging, built):
        verified = self.bundle.verify(staging)
        self.assertIs(verified['verified'], True)
        self.assertIs(verified['installable'], False)
        self.assertEqual(verified['manifest_sha256'], built['manifest_sha256'])
        self.assertEqual(set(verified), {'verified', 'installable', 'manifest_sha256'})

    def _anchor(self, point, kind):
        root, source = self._fill('anchor-' + point + '-' + kind)
        staging = root / 'staging'
        held = root / 'held'
        alternate = root / 'alternate'
        alternate.mkdir(mode=0o700)
        watched_name = {
            'after-first-close': self.bundle.REQUIRED[0],
            'after-middle-close': self.bundle.REQUIRED[len(self.bundle.REQUIRED) // 2],
            'after-last-close': self.bundle.REQUIRED[-1],
        }.get(point)
        state = {'swapped': False, 'dir': None, 'original': None, 'replacement': None,
                 'before': None, 'output_dirs': [], 'watched': None}
        real_open, real_close = os.open, os.close
        real_lstat, real_mkdir = os.lstat, os.mkdir
        real_read = os.read

        def read_file(path):
            fd = real_open(path, os.O_RDONLY)
            try:
                chunks = []
                while True:
                    chunk = real_read(fd, 65536)
                    if not chunk:
                        return b''.join(chunks)
                    chunks.append(chunk)
            finally:
                real_close(fd)

        def snap(path):
            if path is None or not path.exists():
                return None
            rows = []
            for item in [path] + sorted(path.rglob('*')):
                info = item.lstat()
                row = [str(item.relative_to(path)), info.st_dev, info.st_ino, info.st_mode, info.st_size]
                if stat.S_ISREG(info.st_mode):
                    row.append(hashlib.sha256(item.read_bytes()).hexdigest())
                rows.append(tuple(row))
            return tuple(rows)

        def swap():
            self.assertFalse(state['swapped'])
            state['swapped'] = True
            if kind == 'same-bytes':
                for child in staging.iterdir():
                    raw = read_file(child)
                    fd = real_open(alternate / child.name, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o644)
                    try:
                        os.write(fd, raw)
                    finally:
                        real_close(fd)
            os.rename(staging, held)
            if kind == 'unlink':
                state['replacement'] = None
                state['before'] = None
                return
            if kind in ('directory', 'same-bytes'):
                os.rename(alternate, staging)
                state['replacement'] = staging
            else:
                link = alternate if kind == 'absolute_symlink' else Path('alternate')
                os.symlink(link, staging, target_is_directory=True)
                state['replacement'] = alternate
            state['before'] = snap(state['replacement'])

        def os_lstat(path, *args, **kwargs):
            info = real_lstat(path, *args, **kwargs)
            if point == 'after-lstat' and not state['swapped'] and not isinstance(path, int) and Path(path) == staging:
                state['original'] = (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode))
                swap()
            return info

        def os_open(path, flags, *args, **kwargs):
            directory_open = bool(flags & getattr(os, 'O_DIRECTORY', 0)) and not (flags & os.O_CREAT)
            if point == 'before-manifest' and not state['swapped'] and flags & os.O_CREAT and Path(path).name == 'manifest.json':
                swap()
            fd = real_open(path, flags, *args, **kwargs)
            if directory_open and state['dir'] is None:
                state['dir'] = fd
                if state['original'] is None:
                    info = os.fstat(fd)
                    state['original'] = (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode))
                if point == 'after-open':
                    swap()
            if flags & os.O_CREAT:
                dir_fd = kwargs.get('dir_fd')
                self.assertIsNotNone(dir_fd)
                info = os.fstat(dir_fd)
                parent = (info.st_dev, info.st_ino, stat.S_IFMT(info.st_mode))
                state['output_dirs'].append(parent)
                if watched_name and Path(path).name == watched_name:
                    state['watched'] = fd
            return fd

        def os_close(fd):
            watched = state['watched']
            real_close(fd)
            if point in ('after-first-close', 'after-middle-close', 'after-last-close', 'after-manifest-close'):
                manifest_close = point == 'after-manifest-close' and fd == state.get('manifest_fd')
                if not state['swapped'] and (fd == watched or manifest_close):
                    swap()

        def os_open_mark(path, flags, *args, **kwargs):
            fd = os_open(path, flags, *args, **kwargs)
            if flags & os.O_CREAT and Path(path).name == 'manifest.json':
                state['manifest_fd'] = fd
            return fd

        before_fds = self._open_fds()
        os.lstat, os.open, os.close = os_lstat, os_open_mark, os_close
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.lstat, os.open, os.close = real_lstat, real_open, real_close
        self.assertEqual(self._open_fds(), before_fds)
        self.assertTrue(state['swapped'], point)
        error = caught.exception
        self.assertEqual(error.code, 'destination_changed')
        self.assertEqual(error.exit_code, 69)
        self.assertEqual(str(error), 'destination_changed')
        self.assertEqual(error.args, ('destination_changed',))
        self.assertIsNone(error.__cause__)
        self.assertNotIn(str(root), str(error))
        if point in ('after-lstat', 'after-open'):
            self.assertEqual(state['output_dirs'], [])
        else:
            self.assertTrue(state['output_dirs'])
            self.assertTrue(all(parent == state['original'] for parent in state['output_dirs']))
        if state['replacement'] is not None:
            self.assertEqual(snap(state['replacement']), state['before'])
        else:
            self.assertFalse(staging.exists() or staging.is_symlink())

    def _assert_bound_success(self, label):
        root, source = self._fill(label)
        staging = root / 'staging'
        state = {'dir': None, 'stats': [], 'lstats': [], 'opens': [], 'chdir': []}
        real_open, real_stat, real_lstat, real_chdir = os.open, os.stat, os.lstat, os.chdir
        names = set(self.bundle.REQUIRED) | {'manifest.json'}

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            text = path if isinstance(path, int) else os.fspath(path)
            if not isinstance(path, int) and flags & os.O_DIRECTORY and not flags & os.O_CREAT and state['dir'] is None:
                state['dir'] = fd
            if not isinstance(path, int):
                self.assertNotIn('proc/self/fd', text)
                self.assertNotIn('/dev/fd/', text)
            if flags & os.O_CREAT:
                state['opens'].append((text, kwargs.get('dir_fd')))
            return fd

        def os_stat(path, *args, **kwargs):
            text = path if isinstance(path, int) else os.fspath(path)
            state['stats'].append((text, kwargs.get('dir_fd'), kwargs.get('follow_symlinks', True)))
            return real_stat(path, *args, **kwargs)

        def os_lstat(path, *args, **kwargs):
            if not isinstance(path, int):
                state['lstats'].append(os.fspath(path))
            return real_lstat(path, *args, **kwargs)

        def chdir(*args, **kwargs):
            state['chdir'].append(args)
            return real_chdir(*args, **kwargs)

        os.open, os.stat, os.lstat, os.chdir = os_open, os_stat, os_lstat, chdir
        try:
            built = self.bundle.build(source, staging)
        finally:
            os.open, os.stat, os.lstat, os.chdir = real_open, real_stat, real_lstat, real_chdir
        self._assert_verified(staging, built)
        self.assertIsNotNone(state['dir'])
        self.assertEqual(state['chdir'], [])
        self.assertEqual(sorted(text for text, _dir_fd in state['opens']), sorted(names))
        for text, dir_fd in state['opens']:
            self.assertEqual(dir_fd, state['dir'])
            self.assertEqual(text, Path(text).name)
        member_stats = [row for row in state['stats'] if row[0] in names]
        self.assertEqual(sorted(row[0] for row in member_stats), sorted(names))
        for text, dir_fd, follow in member_stats:
            self.assertEqual(dir_fd, state['dir'])
            self.assertIs(follow, False)
        joined = {str(staging / name) for name in names}
        self.assertFalse(joined & set(state['lstats']))

    def _swap_before_manifest_stat(self, label):
        root, source = self._fill(label)
        staging = root / 'staging'
        held = root / 'held'
        alternate = root / 'next'
        alternate.mkdir(mode=0o700)
        state = {'dir': None, 'swapped': False}
        real_open, real_stat = os.open, os.stat

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int) and flags & os.O_DIRECTORY and not flags & os.O_CREAT and state['dir'] is None:
                state['dir'] = fd
            return fd

        def os_stat(path, *args, **kwargs):
            if (not state['swapped'] and kwargs.get('dir_fd') is not None and not isinstance(path, int)
                    and Path(path).name == 'manifest.json'):
                self.assertIs(kwargs.get('follow_symlinks'), False)
                self.assertEqual(kwargs.get('dir_fd'), state['dir'])
                os.rename(staging, held)
                os.rename(alternate, staging)
                state['swapped'] = True
            return real_stat(path, *args, **kwargs)

        os.open, os.stat = os_open, os_stat
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.stat = real_open, real_stat
        self.assertTrue(state['swapped'])
        self.assertEqual(caught.exception.code, 'destination_changed')
        self.assertEqual(list(staging.iterdir()), [])
        self.assertTrue((held / 'manifest.json').is_file())

    def _plant_root(self, kind):
        root, source = self._fill('plant-' + kind)
        staging = root / 'staging'
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'UNTOUCHED SENTINEL\n')
        sentinel.chmod(0o600)
        before = sentinel.read_bytes()
        real_mkdir, real_open = os.mkdir, os.open
        opened = []

        def mkdir(path, mode=0o777, *args, **kwargs):
            real_mkdir(path, mode, *args, **kwargs)
            if Path(path) == staging:
                os.rmdir(path)
                if kind == 'file':
                    fd = real_open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                    os.close(fd)
                elif kind == 'symlink':
                    os.symlink(sentinel, path)
                elif kind != 'missing':
                    self.fail(kind)

        def os_open(path, flags, *args, **kwargs):
            if not isinstance(path, int) and Path(path) == sentinel:
                opened.append(os.fspath(path))
            return real_open(path, flags, *args, **kwargs)

        os.mkdir, os.open = mkdir, os_open
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.mkdir, os.open = real_mkdir, real_open
        self.assertEqual(caught.exception.code, 'destination_changed')
        self.assertEqual(caught.exception.exit_code, 69)
        self.assertEqual(str(caught.exception), 'destination_changed')
        self.assertIsNone(caught.exception.__cause__)
        self.assertEqual(opened, [])
        self.assertEqual(sentinel.read_bytes(), before)
        if kind == 'missing':
            self.assertFalse(staging.exists())
        elif kind == 'file':
            self.assertTrue(staging.is_file())
        else:
            self.assertTrue(staging.is_symlink())

    def _plant_root_subprocess(self, kind):
        root, source = self._fill('plant-sub-' + kind)
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'UNTOUCHED SENTINEL\n')
        sentinel.chmod(0o600)
        staging = root / 'staging'
        completed = subprocess.run(
            [sys.executable, '-B', '-c',
             'import sys; sys.path.insert(0, sys.argv[1]); '
             'import test_upgrade_bundle as tests; '
             'raise SystemExit(tests._refuse_replaced_staging_root(*sys.argv[2:]))',
             str(HERE), kind, str(source), str(staging), str(sentinel)],
            cwd=root, timeout=5, capture_output=True, text=True,
            env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertEqual(sentinel.read_bytes(), b'UNTOUCHED SENTINEL\n')
        self.assertTrue(stat.S_ISFIFO(staging.lstat().st_mode))

    def _hide_capability(self, flag, attr, drop):
        root, source = self._fill('cap-' + (flag or attr) + '-' + getattr(drop, '__name__', 'flag'))
        staging = root / 'staging'
        saved_flag = getattr(os, flag) if flag else None
        saved_attr = getattr(os, attr) if attr else None
        creats = []
        real_open = os.open

        def os_open(path, flags, *args, **kwargs):
            if flags & os.O_CREAT:
                creats.append(os.fspath(path) if not isinstance(path, int) else path)
            return real_open(path, flags, *args, **kwargs)

        if flag:
            delattr(os, flag)
        else:
            setattr(os, attr, set(saved_attr) - {drop})
        os.open = os_open
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open = real_open
            if flag:
                setattr(os, flag, saved_flag)
            else:
                setattr(os, attr, saved_attr)
        self.assertEqual(caught.exception.errno, errno.ENOTSUP)
        self.assertFalse(isinstance(caught.exception, self.bundle.BundleError))
        self.assertEqual(creats, [])
        self.assertEqual(list(staging.iterdir()), [])

    def _not_implemented_boundaries(self):
        root, source = self._fill('ni-open')
        staging = root / 'staging'
        real_open = os.open

        def os_open(path, flags, *args, **kwargs):
            if flags & getattr(os, 'O_DIRECTORY', 0) and not flags & os.O_CREAT:
                raise NotImplementedError('directory open seam')
            if kwargs.get('dir_fd') is not None:
                raise NotImplementedError('dir_fd open seam')
            return real_open(path, flags, *args, **kwargs)

        os.open = os_open
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open = real_open
        self.assertEqual(caught.exception.errno, errno.ENOTSUP)
        self.assertEqual(list(staging.iterdir()), [])
        root, source = self._fill('ni-stat')
        staging = root / 'staging'
        real_stat = os.stat
        fallback = []

        def os_stat(path, *args, **kwargs):
            if kwargs.get('dir_fd') is not None:
                raise NotImplementedError('dir_fd stat seam')
            if not isinstance(path, int) and Path(path).name in set(self.bundle.REQUIRED) | {'manifest.json'}:
                fallback.append(os.fspath(path))
            return real_stat(path, *args, **kwargs)

        os.stat = os_stat
        try:
            with self.assertRaises(OSError) as caught:
                self.bundle.build(source, staging)
        finally:
            os.stat = real_stat
        self.assertEqual(caught.exception.errno, errno.ENOTSUP)
        self.assertEqual(fallback, [])
        self.assertFalse((staging / 'manifest.json').exists())

    def _directory_fault(self, where, number, code):
        root, source = self._fill('fault-' + where + '-' + code)
        staging = root / 'staging'
        state = {'dir': None, 'closes': 0, 'once': False}
        real_open, real_lstat, real_fstat, real_close = os.open, os.lstat, os.fstat, os.close

        def os_lstat(path, *args, **kwargs):
            if where == 'lstat' and not state['once'] and not isinstance(path, int) and Path(path) == staging:
                state['once'] = True
                raise OSError(number, 'injected directory lstat')
            if where == 'lstat-type' and not isinstance(path, int) and Path(path) == staging:
                raise TypeError('programming-not-capability')
            return real_lstat(path, *args, **kwargs)

        def os_open(path, flags, *args, **kwargs):
            if where == 'open' and not state['once'] and not isinstance(path, int) and Path(path) == staging:
                state['once'] = True
                raise OSError(number, 'injected directory open')
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int) and flags & os.O_DIRECTORY and state['dir'] is None:
                state['dir'] = fd
            return fd

        def os_fstat(fd):
            if where == 'fstat' and fd == state['dir'] and not state['once']:
                state['once'] = True
                raise OSError(number, 'injected directory fstat')
            info = real_fstat(fd)
            if where == 'fstat-identity' and fd == state['dir'] and not state['once']:
                state['once'] = True
                class Fake:
                    st_mode = stat.S_IFDIR | 0o700
                    st_dev = info.st_dev
                    st_ino = info.st_ino + 1
                return Fake()
            return info

        def os_close(fd):
            if fd == state['dir']:
                state['closes'] += 1
            return real_close(fd)

        os.lstat, os.open, os.fstat, os.close = os_lstat, os_open, os_fstat, os_close
        fds = self._open_fds()
        try:
            with self.assertRaises((OSError, self.bundle.BundleError, TypeError)) as caught:
                self.bundle.build(source, staging)
        finally:
            os.lstat, os.open, os.fstat, os.close = real_lstat, real_open, real_fstat, real_close
        self.assertEqual(self._open_fds(), fds)
        error = caught.exception
        if code == 'typeerror':
            self.assertIsInstance(error, TypeError)
            self.assertEqual(str(error), 'programming-not-capability')
        elif code == 'destination_changed':
            self.assertIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.code, 'destination_changed')
            self.assertEqual(str(error), 'destination_changed')
            self.assertEqual(error.exit_code, 69)
        else:
            self.assertIsInstance(error, OSError)
            self.assertNotIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.errno, number)
            self.assertNotEqual(getattr(error, 'code', None), 'source_unreadable')
        if where in ('fstat', 'fstat-identity'):
            self.assertEqual(state['closes'], 1)
        self.assertEqual(list(staging.iterdir()) if staging.is_dir() else [], [])

    def _directory_close(self, number, handled):
        root, source = self._fill('dir-close-' + errno.errorcode[number] + '-' + str(handled))
        sentinel = root / 'sentinel'
        sentinel.write_bytes(b'SENTINEL\n')
        sentinel.chmod(0o600)
        staging = root / 'staging'
        state = {'dir': None, 'count': 0, 'occupied': None, 'double': 0}
        real_open, real_close = os.open, os.close

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if state['dir'] is None and not isinstance(path, int) and flags & os.O_DIRECTORY and not flags & os.O_CREAT:
                state['dir'] = fd
            return fd

        def os_close(fd):
            if fd == state['occupied']:
                state['double'] += 1
                return real_close(fd)
            if fd == state['dir'] and state['count'] == 0:
                real_close(fd)
                state['count'] += 1
                state['occupied'] = real_open(sentinel, os.O_RDONLY)
                raise OSError(number, 'injected directory close')
            return real_close(fd)

        def invoke():
            if not handled:
                self.bundle.build(source, staging)
                return
            try:
                raise ValueError('already-handled-caller-error')
            except ValueError:
                self.bundle.build(source, staging)

        os.open, os.close = os_open, os_close
        fds = self._open_fds()
        try:
            with self.assertRaises(OSError) as caught:
                invoke()
        finally:
            os.open, os.close = real_open, real_close
            if state['occupied'] is not None:
                os.fstat(state['occupied'])
                os.close(state['occupied'])
            self.assertEqual(self._open_fds(), fds)
        self.assertEqual(state['count'], 1)
        self.assertEqual(state['double'], 0)
        self.assertEqual(caught.exception.errno, number)
        self.assertNotIsInstance(caught.exception, self.bundle.BundleError)
        self.assertNotIn('already-handled-caller-error', str(caught.exception))

    def _directory_eintr(self, where):
        root, source = self._fill('dir-eintr-' + where)
        staging = root / 'staging'
        state = {'once': False, 'dir_opens': 0, 'dir': None, 'closes': 0}
        real_open, real_lstat, real_fstat, real_close = os.open, os.lstat, os.fstat, os.close

        def os_lstat(path, *args, **kwargs):
            if where == 'lstat' and not state['once'] and not isinstance(path, int) and Path(path) == staging:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted directory lstat')
            return real_lstat(path, *args, **kwargs)

        def os_open(path, flags, *args, **kwargs):
            if (where == 'open' and not state['once'] and not isinstance(path, int) and Path(path) == staging
                    and flags & os.O_DIRECTORY):
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted directory open')
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int) and flags & os.O_DIRECTORY and not flags & os.O_CREAT:
                state['dir_opens'] += 1
                state['dir'] = fd
            return fd

        def os_fstat(fd):
            if where == 'fstat' and fd == state['dir'] and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted directory fstat')
            return real_fstat(fd)

        def os_close(fd):
            if fd == state['dir']:
                state['closes'] += 1
            return real_close(fd)

        os.lstat, os.open, os.fstat, os.close = os_lstat, os_open, os_fstat, os_close
        try:
            built = self.bundle.build(source, staging)
        finally:
            os.lstat, os.open, os.fstat, os.close = real_lstat, real_open, real_fstat, real_close
        self.assertTrue(state['once'])
        self.assertEqual(state['dir_opens'], 1)
        self.assertEqual(state['closes'], 1)
        self._assert_verified(staging, built)

    def _close_keeps(self, kind):
        root, source = self._fill('keep-' + kind)
        member = self.bundle.REQUIRED[0]
        (source / member).write_bytes(b'KEEP\n')
        (source / member).chmod(0o600)
        staging = root / 'staging'
        marker = RuntimeError('runtime-marker') if kind == 'runtime' else CancelMarker('cancel-marker')
        state = {'dir': None, 'source': set(), 'dest': None, 'done': False, 'dir_closes': 0,
                 'occupied': None, 'double': 0}
        real_open, real_close = os.open, os.close
        real_read, real_write, real_fstat, real_stat = os.read, os.write, os.fstat, os.stat

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if isinstance(path, int):
                return fd
            if flags & os.O_DIRECTORY and not flags & os.O_CREAT and state['dir'] is None:
                state['dir'] = fd
            elif flags & os.O_CREAT and Path(path).name == member:
                state['dest'] = fd
            elif Path(path) == source / member:
                state['source'].add(fd)
            return fd

        def os_read(fd, size):
            if kind == 'source_unreadable' and fd in state['source'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EIO, 'injected source read')
            if kind == 'source_changed' and fd in state['source'] and size == 1 and not state['done']:
                state['done'] = True
                extra = real_open(source / member, os.O_WRONLY | os.O_APPEND)
                real_write(extra, b'X')
                real_close(extra)
            return real_read(fd, size)

        def os_write(fd, data):
            if kind == 'destination_oserror' and fd == state['dest'] and not state['done']:
                state['done'] = True
                raise OSError(errno.EACCES, 'injected destination write')
            return real_write(fd, data)

        def os_fstat(fd):
            if kind in ('runtime', 'cancel') and fd == state['dest'] and not state['done']:
                state['done'] = True
                raise marker
            return real_fstat(fd)

        def os_stat(path, *args, **kwargs):
            if (kind == 'destination_changed' and state['dest'] is not None and not state['done']
                    and kwargs.get('dir_fd') is not None and Path(path).name == member):
                state['done'] = True
                target = staging / member
                os.rename(target, root / 'held-output')
                fd = real_open(target, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                real_close(fd)
            return real_stat(path, *args, **kwargs)

        def os_close(fd):
            if fd == state['occupied']:
                state['double'] += 1
                return real_close(fd)
            if fd == state['dir']:
                real_close(fd)
                state['dir_closes'] += 1
                state['occupied'] = real_open(root / 'sentinel-fd', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                raise OSError(errno.EIO, 'injected directory close')
            return real_close(fd)

        os.open, os.read, os.write = os_open, os_read, os_write
        os.fstat, os.stat, os.close = os_fstat, os_stat, os_close
        fds = self._open_fds()
        try:
            with self.assertRaises((self.bundle.BundleError, OSError, RuntimeError, CancelMarker)) as caught:
                self.bundle.build(source, staging)
        finally:
            os.open, os.read, os.write = real_open, real_read, real_write
            os.fstat, os.stat, os.close = real_fstat, real_stat, real_close
            if state['occupied'] is not None:
                os.fstat(state['occupied'])
                os.close(state['occupied'])
            self.assertEqual(self._open_fds(), fds)
        self.assertTrue(state['done'])
        self.assertEqual(state['dir_closes'], 1)
        self.assertEqual(state['double'], 0)
        error = caught.exception
        self.assertNotIn('injected directory close', str(error))
        if kind == 'source_unreadable':
            self.assertIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.code, 'source_unreadable')
        elif kind == 'destination_oserror':
            self.assertIsInstance(error, OSError)
            self.assertEqual(error.errno, errno.EACCES)
            self.assertIn('injected destination write', str(error))
        elif kind == 'destination_changed':
            self.assertIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.code, 'destination_changed')
        elif kind == 'source_changed':
            self.assertIsInstance(error, self.bundle.BundleError)
            self.assertEqual(error.code, 'source_changed')
        else:
            self.assertIs(error, marker)

    def _relative_and_absolute(self):
        root, source = self._fill('paths')
        absolute = root / 'absolute-staging'
        built = self.bundle.build(source, absolute)
        self._assert_verified(absolute, built)
        relative_root = root / 'relative'
        relative_root.mkdir(mode=0o700)
        relative_source = relative_root / 'source'
        os.rename(source, relative_source)
        calls = []
        real_chdir = os.chdir

        def chdir(*args, **kwargs):
            calls.append(args)
            return real_chdir(*args, **kwargs)

        previous = os.getcwd()
        os.chdir(relative_root)
        os.chdir = chdir
        try:
            again = self.bundle.build(Path('source'), Path('staging'))
        finally:
            os.chdir = real_chdir
            os.chdir(previous)
        self.assertEqual(calls, [])
        self._assert_verified(relative_root / 'staging', again)
        self.assertEqual(again['manifest_sha256'], built['manifest_sha256'])

    def _hardlink_and_empty(self):
        root, source = self._fill('link-empty')
        member = source / self.bundle.REQUIRED[0]
        linked = root / 'linked-name'
        os.link(member, linked)
        member.write_bytes(b'')
        self.assertEqual(member.stat().st_ino, linked.stat().st_ino)
        before = self._stable(linked)
        staging = root / 'staging'
        built = self.bundle.build(source, staging)
        self._assert_verified(staging, built)
        staged = staging / self.bundle.REQUIRED[0]
        self.assertEqual(staged.read_bytes(), b'')
        self.assertEqual(stat.S_IMODE(staged.lstat().st_mode), 0o644)
        self.assertEqual(self._stable(linked), before)

    def _fallback_uses_dirfd(self):
        root, source = self._fill('fallback')
        primary = source / 'release-source.json'
        payload = b'FALLBACK BYTES\n'
        fork = root / 'fork'
        fork.mkdir(mode=0o700)
        fallback = fork / 'release-source.json'
        fallback.write_bytes(payload)
        fallback.chmod(0o600)
        primary.unlink()
        state = {'dir': None, 'fallback_opened': False}
        real_open = os.open

        def os_open(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            if not isinstance(path, int) and flags & os.O_DIRECTORY and state['dir'] is None:
                state['dir'] = fd
            if not isinstance(path, int) and Path(path) == fallback:
                state['fallback_opened'] = True
            if flags & os.O_CREAT and Path(path).name == 'release-source.json':
                self.assertEqual(kwargs.get('dir_fd'), state['dir'])
                self.assertEqual(os.fspath(path), 'release-source.json')
            return fd

        os.open = os_open
        try:
            built = self.bundle.build(source, root / 'staging')
        finally:
            os.open = real_open
        self.assertTrue(state['fallback_opened'])
        self.assertFalse(primary.exists())
        self.assertEqual((root / 'staging' / 'release-source.json').read_bytes(), payload)
        self._assert_verified(root / 'staging', built)


def _refuse_replaced_staging_root(kind, source, staging, sentinel):
    """Subprocess entry. A fifo planted at the new root must fail without a writer."""
    import upgrade_bundle as bundle
    source, staging, sentinel = Path(source), Path(staging), Path(sentinel)
    real_mkdir = os.mkdir

    def mkdir(path, mode=0o777, *args, **kwargs):
        real_mkdir(path, mode, *args, **kwargs)
        if Path(path) == staging:
            os.rmdir(path)
            if kind == 'fifo':
                os.mkfifo(path, 0o600)
            else:
                return

    os.mkdir = mkdir
    before = sentinel.read_bytes()
    try:
        bundle.build(source, staging)
    except bundle.BundleError as exc:
        if exc.code != 'destination_changed' or exc.exit_code != 69 or str(exc) != 'destination_changed':
            return 2
    else:
        return 3
    if sentinel.read_bytes() != before or not stat.S_ISFIFO(staging.lstat().st_mode):
        return 4
    return 0


def _verify_fifo_root(staging):
    """Subprocess entry. A fifo root must be refused without waiting for a writer."""
    import upgrade_bundle as bundle
    staging = Path(staging)
    real_open = os.open
    opened = []

    def wrapped(path, flags, *args, **kwargs):
        opened.append(path if isinstance(path, int) else os.fspath(path))
        return real_open(path, flags, *args, **kwargs)

    os.open = wrapped
    code = None
    try:
        bundle.verify(staging)
    except bundle.BundleError as error:
        code = error.code if error.exit_code == 69 and str(error) == error.code else 'bad-error'
    if opened:
        sys.stdout.write('opened\n')
        return 3
    if code == 'staging_type_rejected':
        return 0
    sys.stdout.write((code or 'accepted') + '\n')
    return 2


class StagingDirectoryTests(unittest.TestCase):
    """One staging directory descriptor for listing, manifest, and members.

    Fixtures are synthetic inert bytes. Nothing is executed, imported, or provisioned.
    """

    def setUp(self):
        import upgrade_bundle as bundle
        self.bundle = bundle
        self.module_path = Path(bundle.__file__).resolve()
        self.assertEqual(self.module_path, HERE / 'upgrade_bundle.py')
        self.tmp = tempfile.TemporaryDirectory(dir=Path.home(), prefix='bundle-staging-dir-')
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir(mode=0o700)
        for name in bundle.REQUIRED:
            path = self.source / name
            path.write_bytes(b'SYNTHETIC INERT\x00\xff ' + name.encode())
            path.chmod(0o600)

    def test_recorded_root_and_replacement_cases(self):
        """Seven root-identity refusals and five controls from the directory probe."""
        cases = (
            ('plain', None, None, True, None),
            ('mode0755', None, None, True, None),
            ('ancestor-symlink', None, None, True, None),
            ('member-symlink', None, None, True, 'member_type_rejected'),
            ('digest-tamper', None, None, True, 'digest_rejected'),
            ('root-absolute-link', None, None, True, 'staging_type_rejected'),
            ('root-relative-link', None, None, True, 'staging_type_rejected'),
            ('root-link-executable-false', None, None, False, 'staging_type_rejected'),
            ('manifest-close-root-directory', 'manifest', 'directory', True, 'staging_changed'),
            ('manifest-close-root-symlink', 'manifest', 'symlink', True, 'staging_changed'),
            ('member-close-root-directory', 'member', 'directory', True, 'staging_changed'),
            ('member-close-root-symlink', 'member', 'symlink', True, 'staging_changed'),
        )
        self.assertEqual(len(cases), 12)
        for label, phase, replacement, executable, expected in cases:
            with self.subTest(case=label):
                self._recorded(label, phase, replacement, executable, expected)

    def test_root_replacement_matrix_does_not_read_the_new_directory(self):
        """Swap the root at each held-directory checkpoint. Replacement bytes stay unread."""
        phases = (
            'before-open', 'after-open', 'manifest-close', 'after-list',
            'member-first', 'member-middle', 'member-last', 'before-final',
        )
        for phase in phases:
            for kind in ('directory', 'absolute-symlink', 'relative-symlink'):
                with self.subTest(phase=phase, kind=kind):
                    self._swap_case(phase, kind, invalid=True)
        for phase in ('manifest-close', 'member-last', 'before-final'):
            with self.subTest(phase=phase, kind='same-bytes'):
                self._swap_case(phase, 'directory', invalid=False)

    def test_static_root_types_have_bounded_results(self):
        root, _source, staging, _built = self._make('static-root')
        dangling = root / 'dangling'
        dangling.symlink_to('missing-staging-target')
        self._assert_unopened(dangling, 'staging_type_rejected')
        link = root / 'link'
        link.symlink_to(staging, target_is_directory=True)
        alias = root / 'alias'
        alias.symlink_to('link')
        self._assert_unopened(alias, 'staging_type_rejected')
        regular = root / 'regular'
        regular.write_bytes(b'SYNTHETIC NOT A DIRECTORY')
        self._assert_unopened(regular, 'staging_type_rejected')
        self._assert_code(root / 'missing', 'manifest_missing')
        parent = root / 'file-parent'
        parent.write_bytes(b'SYNTHETIC')
        self._assert_code(parent / 'staging', 'manifest_missing')
        blocked = root / 'fifo-root'
        os.mkfifo(blocked)
        self._fifo_root(blocked)
        opaque = root / 'opaque'
        opaque.mkdir(mode=0o700)
        opaque.chmod(0)
        try:
            self._assert_code(opaque, 'staging_unreadable')
        finally:
            opaque.chmod(0o700)

    def test_missing_directory_api_does_not_fall_back_to_path_reads(self):
        _root, _source, staging, _built = self._make('api')
        cases = (
            ('flag-directory', 'O_DIRECTORY', None),
            ('flag-nofollow', 'O_NOFOLLOW', None),
            ('flag-nonblock', 'O_NONBLOCK', None),
            ('dir-fd', None, 'supports_dir_fd'),
            ('listing', None, 'supports_fd'),
            ('nofollow-stat', None, 'supports_follow_symlinks'),
        )
        for name, flag, attr in cases:
            with self.subTest(case=name):
                self._hide_directory_api(staging, flag, attr)

    def test_file_flag_loss_after_directory_open_does_not_follow(self):
        for symlink in (False, True):
            with self.subTest(symlink=symlink):
                root, _source, staging, _built = self._make('file-flag-' + str(symlink).lower())
                manifest = staging / 'manifest.json'
                outside = root / 'outside.json'
                outside.write_bytes(manifest.read_bytes())
                outside.chmod(0o600)
                if symlink:
                    manifest.unlink()
                    manifest.symlink_to(outside)
                self._drop_flag_on_manifest_stat(staging, manifest)

    def test_directory_eintr_retries_once_at_the_call(self):
        for where in ('lstat', 'open', 'fstat', 'listdir'):
            with self.subTest(where=where):
                _root, _source, staging, built = self._make('eintr-' + where)
                self._directory_eintr(staging, built, where)

    def test_directory_close_is_one_attempt_and_keeps_the_earlier_error(self):
        cases = (
            ('success', errno.EIO, False, 'staging_unreadable'),
            ('success', errno.EINTR, True, 'staging_unreadable'),
            ('success', errno.EBADF, False, 'staging_unreadable'),
            ('success', None, True, 'success'),
            ('manifest-invalid', errno.EIO, True, 'manifest_invalid'),
            ('digest', errno.EIO, False, 'digest_rejected'),
            ('manifest-close', errno.EIO, True, 'manifest_unreadable'),
            ('programming', errno.EIO, False, 'ProgrammingMarker'),
            ('cancel', errno.EINTR, True, 'CancelMarker'),
        )
        for kind, number, handled, expected in cases:
            with self.subTest(kind=kind, errno=None if number is None else errno.errorcode[number], handled=handled):
                self._directory_close(kind, number, handled, expected)

    def test_entries_are_relative_to_the_held_directory(self):
        root, _source, staging, built = self._make('anchor')
        self._assert_anchor(staging, built)
        self.assertFalse(any(path.name in {'approval.json', 'host-profile.json', 'registration.json', 'pin.json'}
                             for path in root.rglob('*')))

    def test_compatible_directory_shapes_stay_uninstalled(self):
        root, _source, staging, built = self._make('compat')
        staging.chmod(0o755)
        for executable in (True, False):
            with self.subTest(executable=executable, shape='mode0755'):
                outcome = self.bundle.verify(staging, executable=executable)
                self.assertEqual(outcome, {
                    'verified': True, 'installable': False,
                    'manifest_sha256': built['manifest_sha256'],
                })
                self.assertEqual(stat.S_IMODE(staging.lstat().st_mode), 0o755)
        previous = os.getcwd()
        os.chdir(root)
        real_chdir = os.chdir
        calls = []

        def wrapped(*args, **kwargs):
            calls.append(args)
            return real_chdir(*args, **kwargs)

        os.chdir = wrapped
        try:
            relative = self.bundle.verify(Path('staging'))
        finally:
            os.chdir = real_chdir
            os.chdir(previous)
        self.assertEqual(calls, [])
        self.assertIs(relative['installable'], False)
        self.assertEqual(relative['manifest_sha256'], built['manifest_sha256'])
        alias = root / 'alias'
        alias.symlink_to('staging', target_is_directory=True)
        os.chdir(root)
        os.chdir = wrapped
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(Path('alias'))
        finally:
            os.chdir = real_chdir
            os.chdir(previous)
        self.assertEqual(calls, [])
        self.assertEqual(caught.exception.code, 'staging_type_rejected')
        self.assertEqual(caught.exception.exit_code, 69)

    def _recorded(self, label, phase, replacement, executable, expected):
        staging = self.root / label / 'staging'
        source = self.root / label / 'source'
        source.parent.mkdir(mode=0o700)
        source.mkdir(mode=0o700)
        for name in self.bundle.REQUIRED:
            path = source / name
            path.write_bytes(b'SYNTHETIC INERT\x00\xff ' + name.encode())
            path.chmod(0o600)
        built = self.bundle.build(source, staging)
        verify_path = staging
        if label == 'mode0755':
            staging.chmod(0o755)
        if label == 'ancestor-symlink':
            (source.parent / 'via').symlink_to(source.parent, target_is_directory=True)
            verify_path = source.parent / 'via' / 'staging'
        if label.startswith('root-'):
            target = 'staging' if label == 'root-relative-link' else staging
            (source.parent / 'alias').symlink_to(target, target_is_directory=True)
            verify_path = source.parent / 'alias'
        if label == 'member-symlink':
            member = staging / self.bundle.REQUIRED[0]
            member.rename(source.parent / 'member-held')
            member.symlink_to(source.parent / 'member-held')
        if label == 'digest-tamper':
            (staging / self.bundle.REQUIRED[0]).write_bytes(b'TAMPERED SYNTHETIC')
        alternate = source.parent / 'alternate'
        if phase:
            self.bundle.build(source, alternate)
            manifest = json.loads((alternate / 'manifest.json').read_bytes())
            manifest['schema'] = 'synthetic.invalid'
            (alternate / 'manifest.json').write_bytes(json.dumps(manifest).encode())
        origin_ids = self._file_ids(staging)
        alternate_ids = self._file_ids(alternate) if phase else set()
        initial = self._snapshot(source.parent)
        target_name = 'manifest.json' if phase == 'manifest' else (sorted(self.bundle.REQUIRED)[0] if phase else None)
        target_info = (staging / target_name).stat() if target_name else None
        state = {
            'hits': 0, 'target_reads': 0, 'target_closes': 0, 'fd': None,
            'opens_original': 0, 'opens_alternate': 0, 'baseline': None,
        }
        real_open, real_read, real_close, real_fstat = os.open, os.read, os.close, os.fstat

        def opened(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            info = real_fstat(fd)
            key = (info.st_dev, info.st_ino)
            if stat.S_ISREG(info.st_mode):
                state['opens_original'] += int(key in origin_ids)
                state['opens_alternate'] += int(key in alternate_ids)
            if phase and state['hits'] == 0 and target_info is not None and key == (target_info.st_dev, target_info.st_ino):
                state['fd'] = fd
            return fd

        def read(fd, size):
            data = real_read(fd, size)
            if phase and state['hits'] == 0 and fd == state['fd']:
                state['target_reads'] += 1
            return data

        def close(fd):
            is_target = phase and state['hits'] == 0 and fd == state['fd']
            real_close(fd)
            if not is_target:
                return None
            state['target_closes'] += 1
            try:
                real_fstat(fd)
            except OSError as exc:
                self.assertEqual(exc.errno, errno.EBADF)
            else:
                self.fail('target descriptor was still open after close')
            staging.rename(source.parent / 'held-staging')
            if replacement == 'directory':
                alternate.rename(staging)
            else:
                staging.symlink_to(alternate, target_is_directory=True)
            state['hits'] += 1
            state['baseline'] = self._snapshot(source.parent)
            return None

        before_fds = self._fds()
        os.open, os.read, os.close = opened, read, close
        try:
            try:
                outcome = self.bundle.verify(verify_path, executable=executable)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            os.open, os.read, os.close = real_open, real_read, real_close
        self.assertEqual(self._fds(), before_fds)
        self.assertEqual(self._snapshot(source.parent), state['baseline'] or initial)
        if phase:
            self.assertEqual(state['hits'], 1)
            self.assertGreater(state['target_reads'], 0)
            self.assertEqual(state['target_closes'], 1)
        code = outcome.code if isinstance(outcome, self.bundle.BundleError) else 'success'
        detail = 'case=%s code=%s original=%s alternate=%s result=%s' % (
            label, code, state['opens_original'], state['opens_alternate'],
            outcome if not isinstance(outcome, self.bundle.BundleError) else outcome.code)
        if expected is None:
            self.assertEqual(code, 'success', detail)
            self.assertEqual(outcome, {
                'verified': True, 'installable': False,
                'manifest_sha256': built['manifest_sha256'],
            }, detail)
            self.assertEqual(state['opens_alternate'], 0, detail)
            return
        self.assertIsInstance(outcome, self.bundle.BundleError, detail)
        self.assertEqual(outcome.code, expected, detail)
        self.assertEqual(outcome.exit_code, 69, detail)
        self.assertEqual(str(outcome), expected, detail)
        self.assertEqual(state['opens_alternate'], 0, detail)
        if phase and replacement == 'directory':
            fresh = self._fresh(verify_path, executable)
            self.assertEqual(fresh, 'manifest_invalid', detail)
        if phase and replacement == 'symlink':
            fresh = self._fresh(verify_path, executable)
            self.assertEqual(fresh, 'staging_type_rejected', detail)

    def _make(self, label):
        root = self.root / label
        source = root / 'source'
        source.mkdir(parents=True, mode=0o700)
        for name in self.bundle.REQUIRED:
            path = source / name
            path.write_bytes(b'SYNTHETIC INERT\x00\xff ' + name.encode())
            path.chmod(0o600)
        staging = root / 'staging'
        built = self.bundle.build(source, staging)
        return root, source, staging, built

    def _swap_case(self, phase, kind, *, invalid):
        label = '%s-%s-%s' % (phase, kind, str(invalid).lower())
        root, source, staging, built = self._make(label)
        alternate = root / 'alternate'
        self.bundle.build(source, alternate)
        if invalid:
            manifest = json.loads((alternate / 'manifest.json').read_bytes())
            manifest['schema'] = 'synthetic.invalid'
            (alternate / 'manifest.json').write_bytes(json.dumps(manifest).encode())
        origin_ids = self._file_ids(staging)
        alternate_ids = self._file_ids(alternate)
        directory_id = (staging.lstat().st_dev, staging.lstat().st_ino)
        order = sorted(self.bundle.REQUIRED)
        member_name = {
            'member-first': order[0],
            'member-middle': order[len(order) // 2],
            'member-last': order[-1],
        }.get(phase)
        target_name = 'manifest.json' if phase == 'manifest-close' else member_name
        target_id = None
        if target_name is not None:
            info = (staging / target_name).stat()
            target_id = (info.st_dev, info.st_ino)
        state = {
            'swapped': False, 'reads': 0, 'closes': 0, 'target_fd': None,
            'original': 0, 'alternate': 0, 'baseline': None, 'closed_origin': set(),
        }
        real_open, real_read, real_close, real_fstat = os.open, os.read, os.close, os.fstat
        real_lstat, real_list = os.lstat, os.listdir

        def swap():
            if state['swapped']:
                return
            state['swapped'] = True
            staging.rename(root / 'held-staging')
            if kind == 'directory':
                alternate.rename(staging)
            elif kind == 'absolute-symlink':
                staging.symlink_to(alternate, target_is_directory=True)
            else:
                staging.symlink_to(Path(os.path.relpath(alternate, staging.parent)),
                                   target_is_directory=True)
            state['baseline'] = self._snapshot(root)

        def opened(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            info = real_fstat(fd)
            key = (info.st_dev, info.st_ino)
            if stat.S_ISREG(info.st_mode):
                state['original'] += int(key in origin_ids)
                state['alternate'] += int(key in alternate_ids)
            if target_id is not None and key == target_id and state['target_fd'] is None:
                state['target_fd'] = fd
            if (phase == 'after-open' and not state['swapped'] and not isinstance(path, int)
                    and os.path.normpath(os.fspath(path)) == str(staging) and flags & os.O_DIRECTORY):
                swap()
            return fd

        def read(fd, size):
            data = real_read(fd, size)
            if fd == state['target_fd']:
                state['reads'] += 1
            return data

        def close(fd):
            info = None
            try:
                info = real_fstat(fd)
            except OSError:
                info = None
            is_target = target_id is not None and fd == state['target_fd'] and not state['swapped']
            real_close(fd)
            if info is not None and stat.S_ISREG(info.st_mode) and (info.st_dev, info.st_ino) in origin_ids:
                state['closed_origin'].add((info.st_dev, info.st_ino))
            if not is_target:
                return None
            state['closes'] += 1
            try:
                real_fstat(fd)
            except OSError as exc:
                self.assertEqual(exc.errno, errno.EBADF)
            else:
                self.fail('target descriptor was still open after close')
            if phase in ('manifest-close', 'member-first', 'member-middle', 'member-last'):
                swap()
            return None

        def lstat(path, *args, **kwargs):
            staging_hit = (not isinstance(path, int)
                           and os.path.normpath(os.fspath(path)) == str(staging))
            if phase == 'before-open' and staging_hit and not state['swapped']:
                info = real_lstat(path, *args, **kwargs)
                swap()
                return info
            if (phase == 'before-final' and staging_hit and not state['swapped']
                    and state['closed_origin'] == origin_ids):
                swap()
            return real_lstat(path, *args, **kwargs)

        def listed(path, *args, **kwargs):
            names = real_list(path, *args, **kwargs)
            if phase == 'after-list' and isinstance(path, int) and not state['swapped']:
                info = real_fstat(path)
                if (info.st_dev, info.st_ino) == directory_id and stat.S_ISDIR(info.st_mode):
                    swap()
            return names

        fds = self._fds()
        os.open, os.read, os.close = opened, read, close
        os.lstat, os.listdir = lstat, listed
        try:
            try:
                outcome = self.bundle.verify(staging)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            os.open, os.read, os.close = real_open, real_read, real_close
            os.lstat, os.listdir = real_lstat, real_list
        detail = 'phase=%s kind=%s invalid=%s original=%s alternate=%s' % (
            phase, kind, invalid, state['original'], state['alternate'])
        self.assertEqual(self._fds(), fds, detail)
        self.assertTrue(state['swapped'], detail)
        self.assertEqual(self._snapshot(root), state['baseline'], detail)
        self.assertEqual(state['alternate'], 0, detail)
        self.assertIsInstance(outcome, self.bundle.BundleError, detail)
        self.assertEqual(outcome.code, 'staging_changed', detail)
        self.assertEqual(outcome.exit_code, 69, detail)
        self.assertEqual(str(outcome), 'staging_changed', detail)
        if phase in ('manifest-close', 'member-first', 'member-middle', 'member-last'):
            self.assertEqual(state['closes'], 1, detail)
            self.assertGreater(state['reads'], 0, detail)
        if phase in ('before-open', 'after-open'):
            self.assertEqual(state['original'], 0, detail)
        if invalid and kind == 'directory':
            self.assertEqual(self._fresh(staging, True), 'manifest_invalid', detail)
        elif invalid:
            self.assertEqual(self._fresh(staging, True), 'staging_type_rejected', detail)
        else:
            fresh = self.bundle.verify(staging)
            self.assertEqual(fresh, {
                'verified': True, 'installable': False,
                'manifest_sha256': built['manifest_sha256'],
            }, detail)

    def _assert_unopened(self, path, expected):
        real_open = os.open
        opened = []

        def wrapped(candidate, flags, *args, **kwargs):
            opened.append(candidate if isinstance(candidate, int) else os.fspath(candidate))
            return real_open(candidate, flags, *args, **kwargs)

        before = self._snapshot(path.parent)
        fds = self._fds()
        os.open = wrapped
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(path)
        finally:
            os.open = real_open
        self.assertEqual(self._fds(), fds)
        self.assertEqual(self._snapshot(path.parent), before)
        self.assertEqual(opened, [])
        self._assert_bundle(caught.exception, expected)

    def _assert_code(self, path, expected):
        before = self._snapshot(path.parent if path.parent.exists() else self.root)
        fds = self._fds()
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(path)
        finally:
            self.assertEqual(self._fds(), fds)
        if path.parent.exists():
            self.assertEqual(self._snapshot(path.parent), before)
        self._assert_bundle(caught.exception, expected)

    def _assert_bundle(self, error, expected):
        self.assertEqual(error.code, expected)
        self.assertEqual(error.exit_code, 69)
        self.assertEqual(str(error), expected)
        self.assertEqual(error.args, (expected,))

    def _fifo_root(self, path):
        before = self._snapshot(path.parent)
        try:
            completed = subprocess.run(
                [sys.executable, '-B', '-c',
                 'import sys; sys.path.insert(0, sys.argv[1]); '
                 'import test_upgrade_bundle as tests; '
                 'raise SystemExit(tests._verify_fifo_root(sys.argv[2]))',
                 str(HERE), str(path)],
                cwd=self.root, timeout=5, capture_output=True, text=True,
                env={**os.environ, 'PYTHONDONTWRITEBYTECODE': '1'})
        except subprocess.TimeoutExpired:
            self.fail('fifo root verify was still blocked after 5s')
        self.assertEqual(completed.returncode, 0, completed.stdout + completed.stderr)
        self.assertEqual(self._snapshot(path.parent), before)
        self.assertTrue(stat.S_ISFIFO(path.lstat().st_mode))

    def _hide_directory_api(self, staging, flag, attr):
        real_open = os.open
        opened = []

        def wrapped(candidate, flags, *args, **kwargs):
            opened.append(candidate if isinstance(candidate, int) else os.fspath(candidate))
            return real_open(candidate, flags, *args, **kwargs)

        before = self._snapshot(staging.parent)
        fds = self._fds()
        saved_flag = getattr(os, flag) if flag else None
        saved_attr = getattr(os, attr) if attr else None
        os.open = wrapped
        try:
            if flag:
                delattr(os, flag)
            else:
                setattr(os, attr, frozenset())
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            os.open = real_open
            if flag:
                setattr(os, flag, saved_flag)
            else:
                setattr(os, attr, saved_attr)
            self.assertEqual(self._fds(), fds)
            self.assertEqual(self._snapshot(staging.parent), before)
        self.assertEqual(opened, [])
        self._assert_bundle(caught.exception, 'staging_unreadable')

    def _drop_flag_on_manifest_stat(self, staging, manifest):
        real_open, real_stat = os.open, os.stat
        opened = []
        saved = os.O_NOFOLLOW
        state = {'done': False}

        def stated(path, *args, **kwargs):
            if _entry_matches(path, manifest, kwargs) and not state['done']:
                state['done'] = True
                delattr(os, 'O_NOFOLLOW')
            return real_stat(path, *args, **kwargs)

        def opened_file(path, flags, *args, **kwargs):
            if _entry_matches(path, manifest, kwargs):
                opened.append(os.fspath(path))
            return real_open(path, flags, *args, **kwargs)

        before = self._snapshot(staging.parent)
        fds = self._fds()
        os.stat, os.open = stated, opened_file
        try:
            with self.assertRaises(self.bundle.BundleError) as caught:
                self.bundle.verify(staging)
        finally:
            os.stat, os.open = real_stat, real_open
            setattr(os, 'O_NOFOLLOW', saved)
            self.assertEqual(self._fds(), fds)
            self.assertEqual(self._snapshot(staging.parent), before)
        self.assertTrue(state['done'])
        self.assertEqual(opened, [])
        self._assert_bundle(caught.exception, 'manifest_type_rejected')

    def _directory_eintr(self, staging, built, where):
        real_open, real_lstat, real_fstat, real_list = os.open, os.lstat, os.fstat, os.listdir
        state = {'once': False, 'dir_fd': None}

        def lstat(path, *args, **kwargs):
            if (where == 'lstat' and not state['once'] and not isinstance(path, int)
                    and os.path.normpath(os.fspath(path)) == str(staging)):
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted')
            return real_lstat(path, *args, **kwargs)

        def opened(path, flags, *args, **kwargs):
            if (where == 'open' and not state['once'] and not isinstance(path, int)
                    and os.path.normpath(os.fspath(path)) == str(staging)
                    and flags & os.O_DIRECTORY):
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted')
            fd = real_open(path, flags, *args, **kwargs)
            if state['dir_fd'] is None and not isinstance(path, int) and flags & os.O_DIRECTORY:
                state['dir_fd'] = fd
            return fd

        def fstat(fd):
            if where == 'fstat' and fd == state['dir_fd'] and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted')
            return real_fstat(fd)

        def listed(path, *args, **kwargs):
            if where == 'listdir' and path == state['dir_fd'] and not state['once']:
                state['once'] = True
                raise OSError(errno.EINTR, 'interrupted')
            return real_list(path, *args, **kwargs)

        fds = self._fds()
        os.lstat, os.open, os.fstat, os.listdir = lstat, opened, fstat, listed
        try:
            outcome = self.bundle.verify(staging)
        finally:
            os.lstat, os.open, os.fstat, os.listdir = real_lstat, real_open, real_fstat, real_list
            self.assertEqual(self._fds(), fds)
        self.assertTrue(state['once'])
        self.assertIs(outcome['verified'], True)
        self.assertIs(outcome['installable'], False)
        self.assertEqual(outcome['manifest_sha256'], built['manifest_sha256'])

    def _directory_close(self, kind, number, handled, expected):
        root, _source, staging, built = self._make('dir-close-%s-%s-%s' % (kind, number, int(handled)))
        if kind == 'manifest-invalid':
            manifest = staging / 'manifest.json'
            manifest.write_bytes(manifest.read_bytes() + b'x')
        elif kind == 'digest':
            (staging / sorted(self.bundle.REQUIRED)[0]).write_bytes(b'TAMPERED SYNTHETIC')
        manifest_info = (staging / 'manifest.json').stat()
        manifest_id = (manifest_info.st_dev, manifest_info.st_ino)
        real_open, real_close, real_fstat, real_read = os.open, os.close, os.fstat, os.read
        state = {
            'dir_fd': None, 'dir_closes': 0, 'double': 0, 'sentinels': set(), 'manifest_fd': None,
        }
        marker = ProgrammingMarker('SYNTHETIC PROGRAMMING MARKER')
        cancel = CancelMarker('SYNTHETIC CANCEL')

        def opened(path, flags, *args, **kwargs):
            fd = real_open(path, flags, *args, **kwargs)
            info = real_fstat(fd)
            if state['dir_fd'] is None and not isinstance(path, int) and flags & os.O_DIRECTORY:
                state['dir_fd'] = fd
            if (info.st_dev, info.st_ino) == manifest_id and state['manifest_fd'] is None:
                state['manifest_fd'] = fd
            return fd

        def read(fd, size):
            if fd == state['manifest_fd'] and kind == 'programming':
                raise marker
            if fd == state['manifest_fd'] and kind == 'cancel':
                raise cancel
            return real_read(fd, size)

        def close(fd):
            if fd in state['sentinels']:
                state['double'] += 1
                return None
            if fd == state['dir_fd']:
                if state['dir_closes']:
                    state['double'] += 1
                    return None
                state['dir_closes'] += 1
                real_close(fd)
                try:
                    real_fstat(fd)
                except OSError as exc:
                    self.assertEqual(exc.errno, errno.EBADF)
                else:
                    self.fail('directory descriptor was still open after close')
                holder = real_open(os.devnull, os.O_RDONLY)
                if holder != fd:
                    os.dup2(holder, fd)
                    real_close(holder)
                state['sentinels'].add(fd)
                if number is not None:
                    raise OSError(number, 'SYNTHETIC DIRECTORY CLOSE')
                return None
            if fd == state['manifest_fd'] and kind == 'manifest-close':
                real_close(fd)
                raise OSError(errno.EIO, 'SYNTHETIC FILE CLOSE')
            return real_close(fd)

        fds = self._fds()
        before = self._snapshot(root)
        os.open, os.close, os.read = opened, close, read
        caller = LookupError('SYNTHETIC CALLER EXCEPTION')
        outcome = None
        error = None
        preserved = False
        try:
            if not handled:
                try:
                    outcome = self.bundle.verify(staging)
                except BaseException as exc:
                    error = exc
                preserved = True
            else:
                try:
                    raise caller
                except LookupError:
                    try:
                        outcome = self.bundle.verify(staging)
                    except BaseException as exc:
                        error = exc
                    preserved = sys.exception() is caller
        finally:
            os.open, os.close, os.read = real_open, real_close, real_read
            for fd in list(state['sentinels']):
                real_close(fd)
            state['sentinels'].clear()
        self.assertEqual(self._fds(), fds)
        self.assertEqual(self._snapshot(root), before)
        self.assertTrue(preserved)
        self.assertEqual(state['dir_closes'], 1)
        self.assertEqual(state['double'], 0)
        if error is None:
            observed = 'success'
        elif isinstance(error, self.bundle.BundleError):
            observed = error.code
        else:
            observed = type(error).__name__
        self.assertEqual(observed, expected)
        if expected == 'success':
            self.assertIs(outcome['verified'], True)
            self.assertIs(outcome['installable'], False)
            self.assertEqual(outcome['manifest_sha256'], built['manifest_sha256'])
            return
        if expected == 'ProgrammingMarker':
            self.assertIs(error, marker)
            return
        if expected == 'CancelMarker':
            self.assertIs(error, cancel)
            return
        self._assert_bundle(error, expected)
        self.assertIsNone(error.__cause__)

    def _assert_anchor(self, staging, built):
        real_open, real_stat, real_list = os.open, os.stat, os.listdir
        real_chdir, real_rename, real_unlink, real_chmod = os.chdir, os.rename, os.unlink, os.chmod
        real_chown = os.chown
        state = {'dir': None, 'opens': [], 'stats': [], 'lists': [], 'forbidden': []}

        def forbid(name):
            def wrapped(*args, **kwargs):
                state['forbidden'].append(name)
                raise AssertionError('verifier called ' + name)
            return wrapped

        def opened(path, flags, *args, **kwargs):
            text = path if isinstance(path, int) else os.fspath(path)
            if not isinstance(path, int) and ('/proc/self/fd' in text or text.startswith('/dev/fd/')):
                state['forbidden'].append(text)
            fd = real_open(path, flags, *args, **kwargs)
            if (state['dir'] is None and not isinstance(path, int) and flags & os.O_DIRECTORY
                    and os.path.normpath(text) == str(staging)):
                state['dir'] = fd
                self.assertTrue(flags & os.O_NOFOLLOW and flags & os.O_NONBLOCK)
                self.assertFalse(flags & (os.O_WRONLY | os.O_RDWR))
            else:
                state['opens'].append((text, kwargs.get('dir_fd'), flags))
            return fd

        def stated(path, *args, **kwargs):
            text = path if isinstance(path, int) else os.fspath(path)
            state['stats'].append((text, kwargs.get('dir_fd'), kwargs.get('follow_symlinks', True)))
            return real_stat(path, *args, **kwargs)

        def listed(path, *args, **kwargs):
            state['lists'].append(path if isinstance(path, int) else os.fspath(path))
            return real_list(path, *args, **kwargs)

        names = {'manifest.json'} | set(self.bundle.REQUIRED)
        before = self._snapshot(staging.parent)
        fds = self._fds()
        os.open, os.stat, os.listdir = opened, stated, listed
        os.chdir, os.rename, os.unlink, os.chmod = (
            forbid('chdir'), forbid('rename'), forbid('unlink'), forbid('chmod'))
        os.chown = forbid('chown')
        try:
            try:
                outcome = self.bundle.verify(staging)
            except self.bundle.BundleError as error:
                outcome = error
        finally:
            os.open, os.stat, os.listdir = real_open, real_stat, real_list
            os.chdir, os.rename, os.unlink, os.chmod = real_chdir, real_rename, real_unlink, real_chmod
            os.chown = real_chown
            self.assertEqual(self._fds(), fds)
            self.assertEqual(self._snapshot(staging.parent), before)
        self.assertEqual(state['forbidden'], [])
        self.assertEqual(outcome, {
            'verified': True, 'installable': False,
            'manifest_sha256': built['manifest_sha256'],
        })
        self.assertEqual(state['lists'], [state['dir']])
        self.assertEqual({item[0] for item in state['opens']}, names)
        self.assertEqual(len(state['opens']), len(names))
        for text, dir_fd, flags in state['opens']:
            self.assertEqual(dir_fd, state['dir'])
            self.assertNotIn('/', str(text))
            self.assertTrue(flags & os.O_NOFOLLOW and flags & os.O_NONBLOCK)
            self.assertFalse(flags & os.O_DIRECTORY)
        self.assertGreaterEqual(len(state['stats']), len(names) * 2)
        for text, dir_fd, follow in state['stats']:
            self.assertEqual(dir_fd, state['dir'])
            self.assertIs(follow, False)
            self.assertIn(text, names)
            self.assertNotIn('/', str(text))

    def _fresh(self, path, executable):
        try:
            self.bundle.verify(path, executable=executable)
        except self.bundle.BundleError as error:
            return error.code if error.exit_code == 69 and str(error) == error.code else 'bad-error'
        return 'success'

    def _file_ids(self, root):
        found = set()
        if not root.exists():
            return found
        for path in root.iterdir():
            info = path.lstat()
            if stat.S_ISREG(info.st_mode):
                found.add((info.st_dev, info.st_ino))
        return found

    def _snapshot(self, root):
        rows = []
        for path in sorted(root.rglob('*'), key=lambda item: item.as_posix()):
            info = path.lstat()
            row = [info.st_dev, info.st_ino, info.st_mode, info.st_uid, info.st_size,
                   info.st_mtime_ns, info.st_ctime_ns]
            if stat.S_ISREG(info.st_mode):
                row.append(hashlib.sha256(path.read_bytes()).hexdigest())
            elif stat.S_ISLNK(info.st_mode):
                row.append(os.readlink(path))
            rows.append((str(path.relative_to(root)), tuple(row)))
        return tuple(rows)

    def _fds(self):
        base = '/proc/self/fd' if os.path.isdir('/proc/self/fd') else '/dev/fd'
        return frozenset(os.listdir(base))


if __name__ == '__main__':
    unittest.main()
