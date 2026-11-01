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
S5 connects `apply` only through a complete private host package.

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
`upgrade_restore_verify.py`, `upgrade_recovery.py`, `upgrade_manual_approval.py`,
`upgrade_wireup.py`, `upgrade_identity.py` and reviewed
`release-source.json`. `host-profile.json` is the fixed trust root beside the
bundle and is bound by the resolved profile digest, not by guessing a host.
Missing or changed dependencies fail with
`dependency_unavailable`, exit 69; there is no PATH/source fallback.

## S6.01 fixed host identity

`upgrade_identity.py` resolves identity for the wrapper, host engine,
private-package admission and cold-window runtime. It reads only
`/usr/local/libexec/ags-autodeploy/host-profile.json` before the owner pin.
A missing profile returns `dependency_unavailable` / 69 and does not fall back
to hostname truncation, HOME, a request host, or an environment variable.

The identity-only `ags.host-identity.v1` profile has exactly `schema`,
`logical_host`, `host_instance`, `uid`, `gid`, `user`, `home`, `root`,
`platform`, `arch`, `machine_id_kind`, `machine_id_sha256` and `aliases`.
`logical_host` is `mini` or `imile-win`. The retained contracts are yoyo UID
501 with Darwin arm64 for mini, and UID 1000 with Linux x86_64 for imile-win.
Real and effective UID, GID, passwd name and passwd home must match. `root` is
the canonical passwd home plus `/.ags`. These are source contracts, not a claim
about a deployed machine. No real machine digest is stored in source.

`aliases` is a nonempty explicit whitelist compared after ASCII case folding.
A short name and a `.local` name are separate entries; the resolver does not
truncate, strip a suffix, or call DNS. Duplicate aliases after normalization
refuse the profile. Tests use `primary-mini` and `primary-mini.local`.
Unknown aliases return `host_alias_unknown` / 64. Conflicting logical mappings
return `host_mapping_conflict` / 64. A copied profile whose machine digest,
platform, architecture or identifier kind does not match returns
`host_machine_mismatch` / 64. Wrong UID, eUID, GID, user or home returns
`wrong_execution_identity` / 77. Diagnostics do not echo the raw machine
identifier. The wrapper passes `--identity-sha256`; the child re-resolves and
refuses a changed parent before reading further effects.

The execution identity gate runs before business actions: only the supported
host and its designated yoyo UID are accepted. Wrong UID fails before claim.
The imile Dagu root process must use a previously verified absolute `runuser`
path and `env -i` to become yoyo before entering the wrapper. It must provide the
owner HOME and an owner-writable cwd, without root tokens, gh configuration or
Dagu credentials. mini already runs as its owner and needs no extra runuser.
The source does not discover or configure either machine's service commands.

An independently provisioned root policy selects the fixed private adapter and
local/service health routes. No production adapter is shipped. S5 consumes S4
human-carried mini evidence before imile stop. A command that exits zero, or a JSON digest, cannot
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

## S5 two-layer admission

The checked-in policy still has `deployments={}` and `migration_admissions=[]`.
With a valid pin/bundle and window authorization, that default `apply` returns
`backup_contract_unimplemented` / 83 before any claim or service effect. Earlier
pin/bundle/identity failures retain 64/69/77. No host is provisioned by source.
Direct v2 `--install` remains unconditionally `install_contracts_unimplemented` /
83, including when a private upgrade package exists. It is not a second path.

The capability layer runs before claim creation. A root-owned exact-digest
`ags.upgrade-policy.v2` entry has exactly `root`, `readyz`, `service_readyz`,
`legacy_check` (fixed absolute argv) and `host_package` (absolute private path).
The root-owned bundle must include `upgrade_wireup.py` and the package's adapter
module. Policy has no enabled, force, skip-backup or success-command option.
The empty migration registry must be replaced only by separately reviewed exact
old/new/observed migration admissions; normal preflight still checks Release,
R/C exact-head CI, ABI, binary identity and old/new/populated smoke.

`host_package` is a single-link owner-only JSON file, read with the S4 private
reader. Its closed `ags.private-host-package.v1` fields are:

- `schema`, `host`, `host_instance`, `deployment_id`, `transaction_id`,
  `authorization_id`, `pin_sha256` (this machine's exact request/window).
- `dsn` (local SQLite), `schema_sha256` (reviewed, not discovered approval),
  `recovery_group` (the existing S1 graph, capacity and storage contract).
- `adapter_module`, an `upgrade_adapter_<name>.py` exact-digest member of the
  root-owned bundle and an explicit `tool` in the recovery group.
- `mini_handoff`, null on mini; the S4 human-carried package on imile.

The adapter declares `SCHEMA=ags.upgrade-host-adapter.v1`. Its side-effect-free
`create(root, package)` returns exactly `manager`, `writer_probe`, `trash`.
The fixed manager implements `suppress_restart`, `stop`, `start`; stop and writer
probe results must satisfy S3, not merely exit zero. `trash` must be a verified
system Trash callback; the executor never supplies a delete fallback. Root-owned
adapter code is trusted provisioning, not an untrusted JSON command interpreter.
No production adapter is shipped here. A synthetic adapter is only a test.

The window authorization `evidence_sha256` equals the raw private-package SHA256.
Changing the package requires a new human authorization, not a runtime edit.
The capability layer reuses S1 inventory/schema, capacity and path validation,
S4 pin/request/release-key binding and imile handoff, and the retained original
window authorization. The package's transaction ID plus host/deployment/window
binding distinguishes deployments of the same CI build. Machine-specific pin,
policy, old identity and bundle digests need not equal those of the other host.

The stage layer then runs `stop → quiescent → backup → restore_verified →
install_started → installed → start_attempted → health → awaiting_observation`.
It never demands a future seal before stopping. S3 owns the durable marker,
writer evidence, dual locks, S1 seal/S2 restore revalidation and one-shot start.
Window authorization is reread before installation and immediately before each
manager stop or start call, after durable intent writes. These checks also validate
the package and imile handoff freshness. An unreadable, missing, malformed or
unsafe-permission stage authorization file is a contract failure, not an unknown
installation outcome. Stage contract failures retain 83 plus stopped/unknown
service state and recovery-required journal fields. A rejected start preserves
`start_attempted`, the cold marker and the no-replay boundary. Installation/start
outcomes that are unknown keep 81/82. Initial request, pin, bundle and execution
identity errors keep their existing codes. A stopped 83 is not proof that the old
service is up.

Successful apply stops at 86. Only a separate S4 human `confirm` can return
completed / 0 after validating its own full evidence and consumption chain.
Neither query success nor a timer is acceptance. Reentry never blindly repeats
an interrupted effect; awaiting reentry revalidates the full evidence chain and
running generation. Completed history remains an acceptance record, not live health.

## S1 inventory and evidence contracts

[Backup and recovery](backup-recovery.md) owns the complete recovery group,
strict seal/restore evidence schemas, local SQLite/Git/Wiki inventory and fixed
per-host retention fields. The two S1 modules are required in the exact-digest
tool bundle. S2 adds a synthetic cold-backup producer and an offline
restore-copy checker. S5 calls both only through S3's admitted cold-window component; direct v2 install cannot call them. Every machine retains only its
latest five sealed and restore-verified backups; restore copies go to system
Trash only after successful verification. Failed restore copies and partial
backups remain in place for manual review and
do not count toward five. Insufficient space refuses; Trash is not free space.
S1 validates the policy. S2 validates seal/report binding and host identity, then
selects the latest five by seal time. A caller must inject a verified system
Trash function; the component has no default Trash implementation. Recovery
requires the reviewed group, DSN, admitted schema and sealed inventory evidence,
but does not require the original data paths to exist. Default public apply and direct v2 install still exit 83.

## S3 private cold-window protocol

`upgrade_recovery.py` provides a private component harness, not a production
command. Its order is `stop → quiescent → backup → restore_verified →
install_started → installed → start_attempted`. It reuses S1 binding/group/seal
validation and S2 backup and independent restore verification without changing
those contracts. S5 connects the host backup adapter and apply only after private-package admission.
Without it, they retain `backup_contract_unimplemented` / 83. Direct host install also
refuses without a private cold guard; direct v2 install remains
`install_contracts_unimplemented` / 83. There is no recovery CLI or direct-install route around the admitted transaction.

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
stop command alone is insufficient. S6.02 `upgrade_host_evidence.py` refuses
that record when it still names a live writer before the existing S3 check.
S6.03 `upgrade_adapter_launchd.py` accepts only the fixed launchctl forms
for one reviewed domain and label. Disable must read back disabled with no pid.
Bootout timeout, supervisor failure, leftover descendants and boot mismatch do
not count as a normal stop. A consumed one-shot cannot start again. The native
isolated job sequence is unverified unless a permitted disposable probe runs;
this source test does not call a production launchd service.
S6.04 `upgrade_adapter_systemd.py` is an inferred Linux contract, not an imile
finding. System and user scopes cannot be mixed. A stop exit of zero with a
nonempty cgroup, timeout, wrong peer UID, argv injection, or drop-in drift is
not closure. No sudo shell or wildcard is accepted. No production systemctl is
called.
The reviewed writer probe must close every
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

## Verification limits

Source wiring is not host provisioning or production acceptance. Linux smoke
still requires its unprivileged user/network namespace and refuses when that
is unavailable. The schema comparator still rejects changed indexes, FKs and
triggers and empty affected populated-smoke tables. Real Release verification,
permissions, bytecode controls and provenance failures do not fall back to a
weaker path. Tests replace only synthetic manager/health and Release transport
boundaries, not the total gate, backup validators, manual validators or order.

Darwin/Linux native full chains, isolated VM execution and production-copy server
startup/migration remain unverified by this source suite. Same-UID/root can forge
files and declarations. Actual writer closure, native ACL/fsync/ABI, routing,
provider outcomes and long-tail business health need separately approved field
verification. No unknown writer, corrupt backup or provider uncertainty can be
accepted as a limitation. Starting AGS already resumes business writes.

S3 pre-start recovery restores only old binary/receipt and keeps the startup
fence; it does not start old AGS. S5 does not add old-start or normal automatic
manager restart authority. The one-shot fence remains after confirm. This is a
conservative retained stop, not automatic rollback or continuous availability.
Restoring normal manager lifecycle, post-start repair, full data restore and
retiring recovery materials require separate design/approval. No claim is TTL-cleared.

Tests use `scripts/ags-python.sh` followed by `scripts/test_upgrade_identity.py`,
`scripts/test_upgrade.py`,
`scripts/test_upgrade_safety.py`, `scripts/test_upgrade_inventory.py`,
`scripts/test_upgrade_backup.py`, `scripts/test_upgrade_restore.py`,
`scripts/test_upgrade_quiescence.py`, `scripts/test_upgrade_recovery.py`,
`scripts/test_upgrade_manual_approval.py`, `scripts/test_upgrade_wireup.py`,
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
The private host policy must provide `readyz` and `service_readyz`; source adds no
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
authority and does not remove S3's one-shot startup fence. Normal restart admission remains separately scoped; S5 does not remove that fence.

The human mini-to-imile package binds the completed mini record and raw evidence
bytes, both machine-specific pins, a shared release key, mini generation and a
fresh human health readback. The release key includes repository ID, Release ID
and tag, runtime revision/tree, and both platform asset digests and control
revisions. Pins need not be equal. The receiver validates the carried bytes,
source declaration and readback within five minutes, but hashes do not authenticate
transport or prove mini is still unchanged. imile needs its own approval and local
backup evidence. Nothing sends this package or initiates imile automatically.

`bash scripts/upgrade-regression.sh` is the shared isolated local/CI runner and is
listed in `scripts/regression_gate.list`. See [backup contracts](backup-recovery.md#s5-wiring)
and [CI scope](../ci.md).

Live health treats a consumed cold-window token as a restart refusal, not a
failure of the running generation. Every other periodic startup check must be
healthy; the current process, generation, installation and both health routes
are still revalidated. A blocked restart fence never grants another spawn.
