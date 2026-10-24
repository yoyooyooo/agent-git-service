# Reviewed maintenance windows

`scripts/ags_maintenance_window.py` adds an explicit `maintenance-window` mode to
an independently reviewed, private staged deployment executor. The external
executor directory is required and its three executable Python inputs are
SHA-256 pinned before import. The repository does not distribute host bindings,
private configuration, binaries, deployment receipts or credentials. Source CI
and a merged PR never authorize a deployment.

The adapter defaults to `isolated`, which delegates to the pinned executor's
original CLI, evidence checks and recovery ledger. A run created in maintenance
mode cannot be continued in isolated mode, or vice versa. The one shared repair
is strict classification of the artifact-retirement startup lock described below.
The adapter supports the exact candidate revision/tree already admitted by that
executor. A different candidate requires another reviewed code change.

## Policy and sequence

A normal window pauses callers, converges recovery work, stops the primary
through its existing manager, proves it is offline, takes the final consistent
recovery group, installs the reviewed candidate and starts through that manager.
It changes no network or ACL rules. **Start restores business:** migration,
seed, durable intent recovery and provider activity can occur before a listener
or readiness response. Existing ingress resumes as soon as the service listens.
This mode does not promise an operator-only read interval after startup.

The explicit stage sequence is:

1. `prepare`: check the old identity/health, import the exact reviewed candidate
   into private staging, verify its hash before invoking `--version`, and persist
   its identity. No stop and no fences are required here.
2. `gate-prestop`: review caller pause and absence of direct file writers;
   inventory the full recovery set and all configuration-referenced startup
   dependencies. Preserve the dependency paths/hashes in the phase record.
3. `stop`: repeat recovery admission immediately before manager stop. Prepare
   old binary/identity/manager/config fingerprints and copy referenced startup
   files into the private backup. Preserve partial backup directories on retry.
4. `gate-drain`: require a normal exit without timeout/SIGKILL, manager/autostart
   suppression, dead runtime PIDs, no primary or replication listener, no data
   holders or Git writer, plus a reviewed offline inspection record. Repeat
   recovery admission under runtime/install locks. There is no post-stop drain
   worker and no queue mutation.
5. `snapshot`, `gate-backup`, `gate-git`: retain the executor's stopped SQLite
   online backup, integrity/FK checks, complete store hashes, independent restore
   copy, full DB repository-to-store coverage and native `git fsck --full --strict`.
   Startup dependency copies and live files must still match their admitted hashes.
6. `install`: repeat recovery admission, backup manifest verification, offline
   checks and complete store validation under both locks; use the original
   durable binary/installation/build-identity replacement implementation.
7. `start-new`: verify the installed hash/identity, offline facts, backup and
   recovery set again; durably write `start-attempted` **before** calling the
   manager. Release runtime/install locks before startup. Startup owns migration.
8. `read-new-start`: after an interrupted or uncertain start, only observe
   health/identity. It never calls start. `validate` then requires local and
   Service readiness, original schema validation, reviewed business/Git reads
   and provider readback, before entering `observing`.
9. `observe`: require at least ten minutes after validation, coverage of every
   actual in-flight lease/recovery duration, zero unknown external results,
   reviewed observation/provider records, healthy exact identity and a fresh
   admitted recovery set. Time alone never completes a window.

Run `--help` for the external CLI arguments; for the additional stages use:

```sh
python3 scripts/ags_maintenance_window.py \
  --executor-dir /private/reviewed-executor \
  --mode maintenance-window --help
```

The wrapper's plan mode loads only the pinned code and prints a plan. It does
not construct a production Host. `--execute` remains explicit. Runtime paths,
manager ownership and the explicit HTTPS Service URL contract stay owned by the
external executor. Never use a private deployment package as a CI dependency.

## Private evidence contracts

All JSON inputs, proof files and imported binaries must be regular files owned
by the executor user with no group/other permissions. Stage evidence binds
`run`, `host`, `mode`, phase, a freshly rotated nonce, kind and observation time
(after that phase and no more than five minutes old). It contains `operator`,
`passed: true`, `service_url`, `service_binding_verified: true`, and a
`route_record` proof. Every proof has `path`, `sha256`, `passed: true`; the file
must exist privately and match. Evidence is archived privately in the run.

Every stage requires the reviewed policy fields `start_restores_business: true`
and `default_forward: true`. No fence boolean is interpreted or manufactured in
maintenance mode. Evidence is an operator/reviewer attestation with checked
bindings; it is not a cryptographic review signature or a substitute for the
underlying inspections. Proof files must describe their claim, expected effects,
reviewer and limitations; hashes alone cannot establish those semantic facts.

A candidate review has schema `ags.maintenance-candidate.v1`, `host`, `reviewer`,
`artifact_kind: exact-source`, `os`, `arch`, `revision`, `tree`, `binary`,
`binary_sha256`, and `build`, `smoke`, `startup_configuration` proofs. Host,
native OS/architecture and the executor-admitted revision/tree must match. The
build/smoke reviewer must bind these exact binary bytes and reject overlays or
migration probes; a version label alone is insufficient. Copy and installed
bytes are rechecked. This adapter does not rerun a production smoke test.

`gate-prestop` requires `callers_paused`, `no_direct_file_writers`, a
`startup_dependencies` proof and `dependency_files: [{path, sha256}]`. The review
must inventory the unchanged effective configuration, seed, background
maintenance, retirement intent/normalized digest/completed receipt/retired
policy and every external path needed to validate those facts. An empty file
list is valid only when that review establishes no external dependencies. Copy
these dependencies privately; never place their contents in source or logs.
Pending retirement or an operation that would rewrite history is not admitted.
No configuration/provider disabling, SQL cleanup or token substitution is used
to manufacture a clean gate.

`gate-drain` additionally requires `normal_exit_no_timeout_no_sigkill`,
`no_direct_file_writers`, `autostart_suppressed` and an `offline_record` proof.
Inspect all configured listener paths, holders and manager jobs; built-in checks
cover the executor's primary port, replication port, runtime PIDs and data tree.
Unknown inspection results block. `validate` requires `business_reads` and
`provider_readback` proofs. `observe` requires `required_observation_seconds`
(integer, at least 600), `all_actual_leases_covered`, `unknown_external_results: 0`,
`observation_record` and `provider_readback` proofs.

## Recovery-set admission

The read-only inventory uses one SQLite connection and one `BEGIN` transaction,
with `mode=ro` and `query_only`. It scans all rows, without API pagination or the
startup scheduler's batch/time limits:

- Every nonterminal projection job, across providers and triggers, including
  retries scheduled in the future and interrupted phases. It conservatively
  includes jobs that startup may terminalize rather than run.
- Generic Forgejo `failed_terminal / provider_admission_denied` jobs that startup
  requeues, and outstanding success-comment claims. A terminal phase alone does
  not prove a job cannot run again.
- Reopen-pending PRs, all nonterminal action intents, unknown/not-recorded provider
  outcomes even on terminal intents, and intents with boundary receipt lineage. Nonterminal access-grant invocations
  and unknown invocation outcomes remain in scope even when marked completed.
- Every unfinished/unknown delivery status and every delivery lease, including a
  delivered row with a retained lease owner or expiry.

Required tables/fields must exist; unknown schema blocks. Old databases may
lack the three additive PR lifecycle columns. Linked PR facts and referenced
immutable boundary receipts are included in per-object fingerprints; missing
links block. Whole rows are hashed without exporting payloads, endpoints,
receipt bodies, labels or free-form errors. The output contains only object
keys, reason, a facts digest and an unknown-result flag. It is still private.
The source predicates are in `pr_projection_jobs.go`, `github_projection_jobs.go`
and `forgejo_action_intent.go`; a scheduler change requires a new adapter review.

Each convergence gate requires `recovery_review: {items: [...]}`. Empty inventory
requires an empty list. A nonempty list must match every current object key
exactly once. Each item binds `facts_sha256`, `reviewer`,
`decision: allow_exact_resume`, an `expected_effect_sha256`, integer
`unknown_external_results: 0`, and `readback` plus `exact_resume_test` proofs.
The reviewer must establish exact permission/generation/ref/PR, expected effect,
idempotency and external acceptance readback, including resolved unknown DB
flags. Boolean risk acceptance alone does not pass. Changed attempt, head,
linked PR, lease or receipt invalidates the fingerprint and requires review.
An object disposed by another authorized procedure disappears from the inventory;
do not retain a stale approval or use this tool to perform that disposition.

## Artifact-retirement auxiliary contract

The store root's `.ags-artifact-retirement` directory is defined by
`internal/artifactretirement/startup_gate.go` and `state_safety*.go`. Accept only a
private, owned directory containing exactly the private, owned, empty regular
`operation.lock` created by `LockState`. A nonblocking exclusive lock must succeed.
The file remains in the complete store hash and backup/restore copy. It is never
removed or silently skipped. Completed retirement receipts live in the separate
configured state directory; they belong in the startup dependency review/backup,
not in this startup-lock directory.

Pending markers, force journals, unknown entries, symlinks, FIFO/socket/device
nodes, unsafe permissions, busy locks, missing repositories and unexpected
repositories continue to block. The existing strict `.github-app-deliveries`
classification is retained. No hidden-directory wildcard exception exists.

## Failures and forward repair

An exception leaves a private failure record and preserves the phase, partial
backups and scene. No automatic stop, retry, downgrade or restore occurs.
`resume-new` repairs installation of the **same admitted candidate** only after
separately proving normal stop/quiescence and revalidating the backup/recovery
set. It then requires a separate `start-new`. A still-running or ambiguous
migration must not be restarted; first use readback and inspect the manager.
A later candidate needs a separate compatibility review and adapter update.

Maintenance mode refuses `restore-old`, `start-old-isolated`,
`abort-before-install` and successor staging. In particular, a start timeout
cannot lead to swapping only the old binary. Full old recovery requires its own
reviewed recovery operation, complete consistent recovery group and provider
reconciliation. The original isolated executor retains its R2
`read-old-start`/`resume-old-start` protocol for its own isolated runs. Never edit
a maintenance run's mode/phase or point isolated recovery at it to bypass this
boundary. Backup restoration does not undo external provider effects.

## Verification and limits

Run the portable suite without any deployment package:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_ags_maintenance_window.py
```

The optional `AGS_REVIEWED_EXECUTOR_DIR` test verifies the byte-pinned executor,
reproduces its original lock misclassification and runs its actual staged
backup/restore-copy/Git/installation code on synthetic files. Only its Host's
manager, health, identity and SQLite-location seams are replaced. CI does not
fetch or require this private input. Both variants use synthetic SQLite and
real temporary bare Git repositories; no AGS/provider server is started. Tests
retain their private temporary directories instead of permanently deleting them.

Passing these tests establishes adapter admission behavior. Native managers,
sudo/owner capabilities, production startup configuration, exact native binary
isolation/read acceptance, current provider dispositions and a real maintenance
window still require their own independent evidence and deployment authorization.
