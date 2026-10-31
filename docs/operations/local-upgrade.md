# Local v2 upgrade entry

`scripts/upgrade` is the guaranteed local entry. It starts the internal
`scripts/upgrade.py` with bytecode writes disabled before Python starts.
Direct `python3 scripts/upgrade.py` is not covered by that guarantee. This
source slice does not install the entry, provision a host, create a Dagu flow,
activate publication or deploy AGS.
The checked-in `upgrade_policy.json` admits no deployments and no migrations.
An operator must review and provision a root-owned tool bundle independently.

Each machine upgrade is manually initiated and separately approved. The order
is mini first, then imile-win. Each future machine flow calls this
entry; neither flow automatically starts the other. No such flow exists in this slice. No schedule, webhook, SSH
step or parent enqueue belongs in that flow. Dagu YAML and automation repository
changes are outside this slice.

## Approval before effects

A future local Dagu flow uses three distinct steps:

1. `authorize_switch` runs `scripts/upgrade plan`, then waits for the operator's
   decision. The command is read-only: no claim, backup, stop or installation.
2. `upgrade_local` depends on the accepted authorization and calls `apply`.
3. `observe_local` records an independent post-switch observation.

Dagu native approval waits after its command finishes. Attaching it to `apply`
would allow effects before approval and is prohibited. A long-lived deployment
switch, starting a Dagu run or a successful CI run is not per-attempt approval.
The transaction never writes `approved=true`. `awaiting_observation` is not
completion and exits 86. S4 adds manual `confirm` for an existing valid transaction;
it does not enable `apply`.

The request parameters are `--deployment-id`, `--pin-sha256`, `--ci-run-id`,
`--ci-run-attempt` and `--authorization-id`. These identify one local attempt.
`plan`, `status`, `reconcile`, `confirm` and `apply` use the same parameters.
`confirm` also accepts `--approval-file` and `--evidence-packet`, both private
absolute paths. Missing approval returns 86. Invalid approval or evidence returns
77. Neither query exit 0 nor plan exit 0 means the upgrade completed. Plan reports unverified
stages; it does not download, smoke-test or prove installability.

## Fixed bundle and execution identity

The wrapper accepts only the operator-provisioned bundle at
`/usr/local/libexec/ags-autodeploy`. The release pin binds the raw SHA256 of
`manifest.json` through `verification_policy.tool_bundle_manifest_sha256`.
The manifest schema is `ags.upgrade-tool-bundle.v2`, its entry is
`upgrade_host.py`, and its `files` maps each adjacent file to an exact SHA256.
The bundle and ancestors must be root-owned and not group/world writable.
Required files include the wrapper, host adapter, transaction engine, policy,
installer, v2 consumer, verifier, shared archive contract, `upgrade_smoke.py`,
`upgrade_backup_contract.py`, `upgrade_inventory.py`, `upgrade_backup.py`,
`upgrade_restore_verify.py`, `upgrade_recovery.py`, `upgrade_manual_approval.py` and reviewed
`release-source.json`. Missing or changed dependencies fail with
`dependency_unavailable`, exit 69; there is no PATH/source fallback.

The execution identity gate runs before business actions: only the supported
host and its designated yoyo UID are accepted. Wrong UID fails before claim.
The imile Dagu root process must use a previously verified absolute `runuser`
path and `env -i` to become yoyo before entering the wrapper. It must provide the
owner HOME and an owner-writable cwd, without root tokens, gh configuration or
Dagu credentials. mini already runs as its owner and needs no extra runuser.
The source does not discover or configure either machine's service commands.

An independently provisioned root policy may later bind exact backup, stop,
start, legacy-check and a local readyz endpoint. This slice does not implement
or accept a production mini handoff adapter. S4 validates human-carried evidence
for existing transactions. A command that exits zero, or a JSON digest, cannot
authenticate mini provenance or prove an independent observer's identity. No arbitrary command arguments are accepted
from the Dagu request or release pin.

## Admission and transaction

Authorization is a private owner-only file named by the authorization ID in the
configuration directory. Schema `ags.local-upgrade-authorization.v2` binds the
whole request identity, actor type/id/source, issue/expiry timestamps,
`decision=approve`, evidence digest and `rollback_mode=forward-only`.
It must exist before apply can create a claim or run backup/stop/install.

The engine owns the lock, claim, journal and receipt under
`state/ags-autodeploy/local-v2`. It records and fsyncs prepare/stop/install flags
before those effects. Interrupted attempts preserve the pending claim. An exact
successful reentry checks health and returns the stored receipt without replaying
effects; incomplete or mismatched attempts fail explicitly. A failure or crash
at/after stop retains the cold-window fence and requires explicit recovery. The
private S3 component permits separately approved pre-start program/receipt recovery
only while data still equals Tq; it never starts the old version. There is no automatic old
binary rollback and no automatic timeout-based claim release. S4 acceptance only
records the human decision for an already-running, fully evidenced transaction.

Migration admission matches all three columns exactly: old full SHA, new full
SHA and the entire sorted observed migration set. The checked-in registry is
empty. An unregistered pair, subset, extra migration or duplicate registration
is refused. Old, fresh new and populated old-to-new smoke runs use independent
scratch HOME, TMPDIR, SQLite and Git roots, never production configuration or DB.
They check readyz revision, clean exit, database integrity and preservation of
old rows/columns. Migration identifiers include structural digests.

## Unavailable apply contracts

`apply` rechecks authorization, then returns exit 83 before creating a claim or
running backup, stop or installation. The diagnostic names every missing contract:

- `backup_git_config_coverage_and_git_db_consistency`
- `independent_observation_approval_and_finalize`
- `trusted_mini_handoff_bound_to_pin_release_deployment` on imile-win

SQLite-only receipts and zero-exit commands are not implementations of these
contracts. `plan`, `status` and verify-only remain available. Exit 86 is reserved
for a future transaction that has stopped at observation; it is distinct from
exit 0, which requires `status=completed`.

A future apply, after the contracts above exist, rechecks full exact-head CI for
R and C, verifies the v2 immutable Release, then checks actual candidate identity
before smoke. That sequence is not reachable in this slice. Only gh-server would
be replaced; Edge, replication, credentials, data and service definitions are not
replaced. The host adapter does not run backup, stop or installation until those
contracts exist. A configured command cannot bypass that refusal. The direct v2
`--install` entry is separately closed with `install_contracts_unimplemented`, exit 83; see
[release operations](releases.md).

## S1 inventory and evidence contracts

[Backup and recovery](backup-recovery.md) owns the complete recovery group,
strict seal/restore evidence schemas, local SQLite/Git/Wiki inventory and fixed
per-host retention fields. The two S1 modules are required in the exact-digest
tool bundle. S2 adds a synthetic cold-backup producer and an offline
restore-copy checker. Neither is called by production apply, the host backup adapter or direct
install. S3 uses both only through its private cold-window component. Every machine retains only its
latest five sealed and restore-verified backups; restore copies go to system
Trash only after successful verification. Failed restore copies and partial
backups remain in place for manual review and
do not count toward five. Insufficient space refuses; Trash is not free space.
S1 validates the policy. S2 validates seal/report binding and host identity, then
selects the latest five by seal time. A caller must inject a verified system
Trash function; the component has no default Trash implementation. Recovery
requires the reviewed group, DSN, admitted schema and sealed inventory evidence,
but does not require the original data paths to exist. Apply and direct v2
install still exit 83.

## S3 private cold-window protocol

`upgrade_recovery.py` provides a private component harness, not a production
command. Its order is `stop → quiescent → backup → restore_verified →
install_started → installed → start_attempted`. It reuses S1 binding/group/seal
validation and S2 backup and independent restore verification without changing
those contracts. The public host backup adapter and both apply gates remain
unconditionally `backup_contract_unimplemented` / 83. Direct host install also
refuses without a private cold guard; direct v2 install remains
`install_contracts_unimplemented` / 83. There is no recovery CLI or new route
from `upgrade.py`, `upgrade_host.py` or `release_install_v2.py` to installation.

Before stopping, the owner fsyncs independent `state/upgrade-cold.json` and
`pending/cold-journal.json` under `state/ags-autodeploy/local-v2`. The marker is
not `installing.json` and is never expired, cleared on timeout or removed to
start. Missing journal/claim, unknown phase or generation mismatch blocks spawn.
The original claim binding remains the source of old identity after replacement;
current installation identity is checked separately.

Hostops ownership spans manager handoff. After normal supervisor and child exit,
the component takes nonblocking install EX then runtime EX locks and retains
them through snapshot, restore verification and durable installation. Runtime
continues to take runtime EX then install SH, also nonblocking; no lock waits
form a cycle. Install uses the same guard instead of reacquiring its own locks.
The fixed manager fixture suppresses restart, proves both exit statuses, no
forced kill, matching runtime generation and an empty process group. A successful
stop command alone is insufficient. The reviewed writer probe must close every
other writer; locks do not constrain unmanaged processes or malicious same-UID
file edits. Writer/lock/unknown failures retain the fence, with no automatic start.

Tq records a nonce plus S2's complete source fingerprint and a data/config
fingerprint. The latter excludes only the deliberately replaced binary and
installation receipt. Before installation intent, authorization, source, sealed
payload and manifest-bound restore report are revalidated under both locks.
Original binary and raw receipt bytes are retained. New hash, identity and receipt
must agree before `installed` can advance. Contract failures after stop keep exit
83 and record `service_state=stopped_or_unknown`, `recovery_required=true`; 83 no
longer implies that service remained online in a private harness.

Before releasing either lock, `start_attempted` persists a token and generation.
The fixed manager reads no arbitrary command/token arguments: it invokes the
fixed launcher, which reads the durable handoff, consumes it under runtime EX
and install SH, fsyncs consumption, then spawns. Manager invocation is separately
recorded before the call; a timeout/lost return cannot cause another call. A crash
after token consumption may mean no process started: only read-only diagnosis is
safe. A manager restart cannot consume the same token. S3 does not clear the
marker or restore normal restart policy after observation; that remains gated
completion work. Startup can migrate and resume business immediately.

`recover_prestart` requires hostops plus both locks, matching original binding,
known pre-start phase, unchanged data/config, valid sealed evidence and a fresh
owner-only `ags.prestart-recovery-approval.v1` file. It binds generation, exact
blocked cold-journal SHA256, time window, human declaration and a new approval ID.
The file is consumed before replacement. It restores only original binary and
raw installation receipt, retains marker and journal, and never starts a process.
Interrupted recovery is readback-only, not replay. After durable `start_attempted`
it always refuses old-version restoration, even if no PID exists. Full data
restore and post-start forward repair are separate incident work. See
[backup and recovery](backup-recovery.md#s3-pre-start-recovery) and
[runtime startup fencing](user-runtime.md#cold-window-startup-fence).

## Known incomplete acceptance

This implementation is a source preparation, not an accepted deployment path.
Linux smoke invokes a fixed unprivileged user/network namespace helper and
brings up loopback only; this Linux chain has not been executed on the current
macOS test machine. Disabled user namespaces fail closed. BASE_URL alone cannot
constrain the server's production listener. The schema comparator conservatively
rejects changes to indexes, foreign keys and triggers. Empty affected tables are
refused until a reviewed populated fixture is available. A future backup command must supply an owner-only `ags.local-backup.v2` receipt
binding the request, a consistent snapshot, file sizes/digests and SQLite
integrity/foreign-key checks. S1 defines the receipt/manifest and inventory
components. S2 produces a sealed synthetic snapshot and checks an offline
restore copy, but current backup, apply and direct install still only refuse the
missing contract. S3 exercises cold-window quiescence and pre-start recovery only through
private synthetic components. Production invocation and accepted host recovery
remain later work. The pin loader rejects unknown fields, wrong types, missing fields, duplicate
JSON fields, and an unsafe ancestor directory. Production admission for mini handoff, complete backup coverage, consistency,
production acceptance integration remains unimplemented and refused by apply.
S4's consumer tests do not constitute an end-to-end apply test. Journal-aware forward recovery
is also unimplemented. Empty policy is an additional default refusal, not proof
that every stage is implemented fail-closed. Do not remove these refusals just
to obtain an upgrade success.

Tests use `scripts/ags-python.sh` followed by `scripts/test_upgrade.py`,
`scripts/test_upgrade_safety.py`, `scripts/test_upgrade_inventory.py`,
`scripts/test_upgrade_backup.py`, `scripts/test_upgrade_restore.py`,
`scripts/test_upgrade_quiescence.py`, `scripts/test_upgrade_recovery.py`,
`scripts/test_upgrade_manual_approval.py`,
`scripts/test_install_v2.py` or `scripts/test_release_consumer.py`. Run with
independent HOME/TMPDIR/cwd and remove inherited Python injection variables.
Real cryptographic acceptance is separately run with
`scripts/ags-python.sh scripts/test_release_crypto.py <local-evidence-directory>` against
pre-downloaded evidence. It never installs or executes candidate binaries.
A dispatch candidate does not establish tag-push certificate acceptance.
See [release operations](releases.md) and [single-root runtime](user-runtime.md).

## S4 manual acceptance

`upgrade_manual_approval.py` consumes a human declaration, not a signature.
A file must be a single-link regular file with mode 0600, owned by the executing
UID. Its parent is private, and every ancestor must exclude group/world writes
and symlinks. Unknown or duplicate fields, an expired declaration, a wrong phase,
and mismatched request, process generation or packet digest all refuse completion.
Same-UID and root writers remain trusted. The actor fields cannot prove a human
identity, so the operator checks the private source out of band.

The executor can publish a fixed evidence packet with `write_packet` and a
`decision=pending` request with `write_request`. It never fills `approve`.
The operator reviews the actual backup and restore reports, installation/runtime
identity, business and Git reads, provider results, recovery items and limitations.
The operator records the decision outside the executor. The closed field contract
is in [backup and recovery](backup-recovery.md#s4-acceptance-file-contract).
A recommendation to observe for at least ten minutes does not authorize a timer
to approve. Hard failures cannot be listed as accepted limitations.

`confirm` holds the same nonblocking `hostops.lock` as S3. It reads the original
claim, retained window authorization, transaction journal, consumed startup token,
S1 seal, S2 inventory and payload, independent restore report and publication
record. It calls S3's evidence validator rather than trusting a success flag.
It checks installed bytes and receipt, current runtime generation, and both local
and service health before committing. Missing or unknown health refuses completion.
The private host policy must provide `readyz` and `service_readyz`; S4 adds no
host entries and does not provision either URL.

One atomic `completed-<request-fingerprint>.json` record contains both the completed
receipt and approval consumption. Its durable publication is the acceptance point.
`pending` then moves to `accepted-<generation>` on the same filesystem. If the
process crashes after publication, retry only finishes that archive and returns
the same record. A different approval cannot replace it. A staged record is not
accepted until fresh checks pass and its publication completes. No retry invokes
stop, install, start or a later host. Historical acceptance is not a promise that
the service remains healthy after completion.

`status` and `reconcile` only read facts under the existing hostops lock. They do
not create the lock, repair a journal, consume approval or replay any effect.
The cold marker stays in place even after acceptance. S4 grants no manager restart
authority and does not remove S3's one-shot startup fence. A later host integration
must separately define normal restart admission.

The human mini-to-imile package binds the completed mini record and raw evidence
bytes, both machine-specific pins, a shared release key, mini generation and a
fresh human health readback. The release key includes repository ID, Release ID
and tag, runtime revision/tree, and both platform asset digests and control
revisions. Pins need not be equal. The receiver validates the carried bytes,
source declaration and readback within five minutes, but hashes do not authenticate
transport or prove mini is still unchanged. imile needs its own approval and local
backup evidence. Nothing sends this package or initiates imile automatically.
