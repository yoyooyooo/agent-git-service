# Disposable Linux continuation fixture

This manual job exercises the production Route B entry on a fresh GitHub-hosted
Ubuntu 24.04 VM. It does not access company providers, live AGS data, imile, or a
shared Forgejo runner. Source tests, catalog validation, and an unrun workflow
are not native admission evidence.

## Run the accepted source

Accept the source through the ordinary PR and Fork CI process first. The new
manual input is available only after the workflow is on the default branch.
Use the existing **Native adapter isolation** workflow with:

- `native_continuation=true`, `native_probe=false`;
- `source_ci_run` naming the successful default-branch push Fork CI run for the
  exact SHA being dispatched;
- `authorization_sha256` naming the SHA256 of the final transported bytes;
- `continuation_case=continue` for the complete positive sequence.

The operator supplies the already reviewed private authorization source through
the temporary repository secret `AGS_ROUTEB_NATIVE_FIXTURE_AUTHORIZATION` and
removes that secret after the terminal result, including failure. Do not put the
authorization text into a dispatch input, issue, log, or artifact. The workflow
does not create credentials or change deployment settings. Its token has only
repository contents and Actions read permissions.

Bind the digest to the bytes the CLI actually transports. GitHub CLI v2.102.0
removes all trailing CR/LF bytes from noninteractive `gh secret set` stdin
(`getBody` in `pkg/cmd/secret/set/set.go`); a raw-byte subprocess pipe does not
prevent this. Keep the reviewed source unchanged. If it ends in CR/LF, retain a
private transport copy with only those final bytes removed, check that its JSON
value is unchanged, and record both digests. Feed that copy through stdin and
use its SHA256 for `authorization_sha256`. Do not put private contents in argv.
The workflow must continue to reject any mismatch, and the operator must remove
the temporary secret after every terminal result.

The original injected-only job remains a separate default-off input. It cannot
stand in for this job. Both jobs use GitHub-hosted runners.

## Real sequence

The job builds the actual accepted gh-server through `build-exact-source.sh`, a
static stage0 with the same revision/tree, and the small fixture data tool. It
downloads the pinned official standalone Python release, inventories its real
ELF dependencies, and asks the production catalog verifier to read that sealed
closure. Failure to establish a closure stops the attempt. The real Linux
child uses catalog-bound `TZ=UTC0` and an authority-owned empty `TZDIR`. The
existing verifier checks that directory's identity and closed empty member set
before entry and on recheck. This removes the host localtime dependency; no
system tzdata permission is changed. The two locale variables remain fixed.

Provisioning uses the VM's actual non-root account, machine identity and home.
It creates one new private runtime root and fixed root-owned registry, bundle,
units and interpreter authority. The initial database comes from real
`db.Migrate`; it contains no business data. Providers are disabled and AGS binds
to loopback. Every approval uses the actual agent-delegation contract and binds
new files or observed effects. No GitHub account type is treated as human review.

PID1 runs bootstrap, initial serve, and the original transaction. A separate
fixture process marks only the original approval inode with fanotify. It allows
all opens except one open by the exact original Python child after both actual
journals report installed and the candidate start domain is still empty. The
kernel returns EPERM. The controller itself persists its failed history and
exits. The fixture reads Python/stage0 exit records and systemd properties
separately. It never creates a claim, journal, stop proof or manager completion.

Only then does the fixture bind a new continuation registration and approval to
the actual original registration, failed journals, generation, and sealed backup.
PID1 starts `continue-installed`. The production entry must reconstruct the
normal stop and failed-controller evidence, issue its own one-use start effect,
and complete its real 600-second native observation.

The continuation approval expires during that observation without being edited.
A fresh full-restore and separate old-start approval must still work through the
normal production recovery entry. The candidate is running at this point, so
restore must obtain its actual normal-stop proof. This does not use the prestart
original-stop branch. Finally a repeated continuation start must be
refused without changing claims, tickets, effects, or the restored service's
generation. Cleanup stops only the units created for this transaction and
retains their files. VM disposal handles removal.

`continuation_case=expired` is a separate fresh VM attempt. It creates the same
real installed failure, waits for the newly issued continuation approval to
expire, and requires refusal before a continuation claim or candidate effect.
It does not edit an old approval or reuse the positive attempt's state.

## Evidence and limits

The public artifact is a closed allowlist of effect records and hashes. It
excludes conversation text, scope grants, approval bodies, runtime environment,
database, backup payload, provider configuration, and service log contents.
Original failed and continued journals are distinct. Each result retains the
actual exit code and source binding. A terminal failure is UNVERIFIED, never an
empty or successful observation. Unsupported VM capability, tool closure or
manager behavior is recorded as a real failure for diagnosis, not bypassed.

A failed subprocess retains a fixed operation label, its actual return code,
and stdout/stderr byte lengths and SHA256 digests in `command_failure`. Native
tools can add only a matching fixed stage and an allowlisted reason. Unknown
text and extra fields stay opaque; argv, environment and output contents are
not exported. These fields describe a failure, not native admission. Digests
identify captured output and do not establish a root cause on their own.

Only after the real verifier returns `authority_file_invalid`, the failure can
include conditions from the digest-bound catalog input: a fixed file category,
entry index, field, numeric mode/size, and a path digest. At most eight conditions
are exported, with the total count and truncation flag. Paths, invalid strings,
and contents remain private. Missing diagnostic facts remain unavailable; no
result authorizes a file permission change or substitutes for catalog verification.
A source counterexample for writable external files does not identify which
condition occurred in a completed VM whose catalog was not exported.

Passing this job proves only the exercised disposable Linux cases for its exact
source and VM. It is not production activation, Darwin acceptance, a complete
native negative-case suite, or an Express release gate. Purpose/preimage/issuer
and other protocol negatives also have source regressions; those are not
relabeled as native observations. Native restoration after a continuation that
itself fails before a candidate effect remains a separate unexecuted case. A
successful post-start restore does not prove that path.

Local source checks require no privileged service operation:

```sh
python3 -B scripts/routeb-native-fixture/test_fixture.py -q
go test ./scripts/routeb-native-fixture/native-tools
```
