# Local v2 upgrade entry

`scripts/upgrade.py` is the fixed local entry. This source slice does not install
it, provision a host, create a Dagu flow, activate publication or deploy AGS.
The checked-in `upgrade_policy.json` admits no deployments and no migrations.
An operator must review and provision a root-owned tool bundle independently.

Each machine upgrade is manually initiated and separately approved. The order
is mini first, then imile-win. Each future machine flow calls this
entry; neither flow automatically starts the other. No such flow exists in this slice. No schedule, webhook, SSH
step or parent enqueue belongs in that flow. Dagu YAML and automation repository
changes are outside this slice.

## Approval before effects

A future local Dagu flow uses three distinct steps:

1. `authorize_switch` runs `upgrade.py plan`, then waits for the operator's
   decision. The command is read-only: no claim, backup, stop or installation.
2. `upgrade_local` depends on the accepted authorization and calls `apply`.
3. `observe_local` records an independent post-switch observation.

Dagu native approval waits after its command finishes. Attaching it to `apply`
would allow effects before approval and is prohibited. A long-lived deployment
switch, starting a Dagu run or a successful CI run is not per-attempt approval.
The transaction never writes `approved=true`. `awaiting_observation` is not
completion and exits 86. This slice has no observation-approval or finalize command.

The request parameters are `--deployment-id`, `--pin-sha256`, `--ci-run-id`,
`--ci-run-attempt` and `--authorization-id`. These identify one local attempt.
`plan`, `status` and `apply` use the same parameters. Plan reports unverified
stages; it does not download, smoke-test or prove installability.

## Fixed bundle and execution identity

The wrapper accepts only the operator-provisioned bundle at
`/usr/local/libexec/ags-autodeploy`. The release pin binds the raw SHA256 of
`manifest.json` through `verification_policy.tool_bundle_manifest_sha256`.
The manifest schema is `ags.upgrade-tool-bundle.v2`, its entry is
`upgrade_host.py`, and its `files` maps each adjacent file to an exact SHA256.
The bundle and ancestors must be root-owned and not group/world writable.
Required files include the wrapper, host adapter, transaction engine, policy,
installer, v2 consumer, verifier, shared archive contract and reviewed
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
or accept a mini handoff verifier. A command that exits zero, or a JSON digest,
cannot prove mini provenance, the current pin/release/deployment, fresh readyz
or independent observation approval. No arbitrary command arguments are accepted
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
at/after stop or install requires forward recovery. There is no automatic old
binary rollback and no automatic timeout-based claim release. Post-observation
finalization is deliberately unavailable here.

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
`--install` entry is separately closed with the same exit 83; see
[release operations](releases.md).

## Known incomplete acceptance

This implementation is a source preparation, not an accepted deployment path.
Linux smoke invokes a fixed unprivileged user/network namespace helper and
brings up loopback only; this Linux chain has not been executed on the current
macOS test machine. Disabled user namespaces fail closed. BASE_URL alone cannot
constrain the server's production listener. The schema comparator conservatively
rejects changes to indexes, foreign keys and triggers. Empty affected tables are
refused until a reviewed populated fixture is available. A future backup command must supply an owner-only `ags.local-backup.v2` receipt
binding the request, a consistent snapshot, file sizes/digests and SQLite
integrity/foreign-key checks. That receipt path is not implemented: current
backup only refuses the missing contract. Complete Git/config backup semantics
remain operator acceptance work. The pin loader rejects unknown fields, wrong types, missing fields, duplicate
JSON fields, and an unsafe ancestor directory. Mini handoff, Git/config backup
coverage, Git/database consistency, observation approval and finalize are
unimplemented and independently refused by apply. Journal-aware forward recovery
is also unimplemented. Empty policy is an additional default refusal, not proof
that every stage is implemented fail-closed. Do not remove these refusals just
to obtain an upgrade success.

Tests: `python3 scripts/test_upgrade.py`, `python3 scripts/test_upgrade_safety.py`, `python3 scripts/test_install_v2.py`, `python3 scripts/test_release_consumer.py`.
Real cryptographic acceptance is separately run with
`python3 scripts/test_release_crypto.py <local-evidence-directory>` against
pre-downloaded evidence. It never installs or executes candidate binaries.
A dispatch candidate does not establish tag-push certificate acceptance.
See [release operations](releases.md) and [single-root runtime](user-runtime.md).
