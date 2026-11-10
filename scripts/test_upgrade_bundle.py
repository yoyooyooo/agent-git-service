#!/usr/bin/env python3
"""S6.06 bundle builder. Staging fixtures only; nothing is provisioned."""
import hashlib
import json
import os
from pathlib import Path
import stat
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


if __name__ == '__main__':
    unittest.main()
