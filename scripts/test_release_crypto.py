#!/usr/bin/env python3
"""Opt-in REAL gh/Sigstore acceptance using pre-downloaded candidate evidence.

AGS_CANDIDATE_EVIDENCE is an explicit local directory. Archives never enter Git.
Run: python3 scripts/test_release_crypto.py /path/to/evidence
The fixture is workflow_dispatch; it MUST NOT satisfy the tag-push install gate.
"""
import base64
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import release_verify as verifier

CONTROL = 'e2d1e21fdd14bee99b5ef32ad8dc35c814f8a381'
RUNTIME = '07506714431912b32cf33cbe71d36d25dac12203'
TREE = '5e61bb20efd72638c1ee07fa948328cd88ed469c'
TARGETS = ('darwin_arm64', 'linux_amd64')


def main():
    evidence = Path(sys.argv[1]).resolve()
    trusted = evidence / 'trusted-root.jsonl'
    source = {'schema': 'ags.release-source.v1', 'runtime': {'revision': RUNTIME, 'tree': TREE}}
    count = 0
    with tempfile.TemporaryDirectory(prefix='ags-real-crypto-') as scratch:
        scratch = Path(scratch).resolve()
        bundles = {}
        for target in TARGETS:
            local = scratch / (target + '.sigstore.json')
            shutil.copyfile(evidence / 'artifacts' / ('release-control-' + target) / 'attestation.sigstore.json', local)
            bundles[target] = local
        for target in TARGETS:
            archive = evidence / 'artifacts' / ('release-control-' + target) / ('ags-source-' + RUNTIME + '-' + target + '.tar.gz')
            pin = {'schema': verifier.contract.PIN_SCHEMA, 'repository': verifier.REPOSITORY,
                   'repository_id': verifier.REPOSITORY_ID, 'runtime': source['runtime'],
                   'targets': {name: {'manifest_schema': verifier.contract.MANIFEST_SCHEMA,
                                     'abi': verifier.contract.abi_for(name), 'provenance': {
                                         'control_revision': CONTROL, 'signer_digest': CONTROL,
                                         'signer_workflow': verifier.WORKFLOW}} for name in TARGETS}}
            command = verifier.verification_command(archive, pin, target, bundle=bundles[target], source=source)
            command += ['--custom-trusted-root', str(trusted), '--source-ref', 'refs/heads/fork/main.20260924']
            cases = [('positive', command, 0)]
            wrong_source = command.copy(); wrong_source[wrong_source.index('--source-digest') + 1] = RUNTIME
            cases.append(('wrong-source', wrong_source, 1))
            wrong_signer = command.copy(); wrong_signer[wrong_signer.index('--signer-workflow') + 1] = verifier.REPOSITORY + '/.github/workflows/release.yml'
            cases.append(('wrong-signer', wrong_signer, 1))
            wrong_tag = command.copy(); wrong_tag[wrong_tag.index('--source-ref') + 1] = 'refs/tags/fork-20260924.5-rc5'
            cases.append(('wrong-tag', wrong_tag, 1))
            cross = command.copy(); cross[cross.index('--bundle') + 1] = str(bundles[TARGETS[1 - TARGETS.index(target)]])
            cases.append(('cross-platform', cross, 1))
            # Signed DSSE predicate mutation: signature MUST fail cryptographically.
            raw = json.loads(bundles[target].read_bytes())
            predicate = json.loads(base64.b64decode(raw['dsseEnvelope']['payload']))
            predicate['predicate']['runtime_tree'] = '0' * 40
            raw['dsseEnvelope']['payload'] = base64.b64encode(json.dumps(predicate).encode()).decode()
            mutated_bundle = scratch / (target + '-tampered.json'); mutated_bundle.write_text(json.dumps(raw))
            mutated = command.copy(); mutated[mutated.index('--bundle') + 1] = str(mutated_bundle)
            cases.append(('tampered-predicate', mutated, 1))
            copied = scratch / archive.name; shutil.copyfile(archive, copied)
            with copied.open('r+b') as stream:
                stream.seek(copied.stat().st_size // 2); byte = stream.read(1)
                stream.seek(-1, 1); stream.write(bytes([byte[0] ^ 1]))
            changed = command.copy(); changed[3] = str(copied)
            cases.append(('tampered-archive', changed, 1))
            for label, args, expected in cases:
                result = subprocess.run(args, capture_output=True, timeout=120)
                print(json.dumps({'target': target, 'case': label, 'command': args,
                                  'exit': result.returncode, 'expected': expected}))
                if (result.returncode == 0) != (expected == 0):
                    raise AssertionError(label + ': ' + result.stderr.decode(errors='replace'))
                if expected == 0:
                    rows = json.loads(result.stdout)
                    verifier.check_verified_statement(rows, hashlib.sha256(archive.read_bytes()).hexdigest(), source=source, subject_name=archive.name)
                    info = verifier.contract.inspect_bundle(archive, 'source-' + RUNTIME, RUNTIME, target)
                    assert info['tree'] == TREE
                count += 1
    print('REAL_CRYPTO_PASS cases=' + str(count) + '; dispatch fixture is not tag-push installation acceptance')


if __name__ == '__main__':
    main()
