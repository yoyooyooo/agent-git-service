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
bytes are rechecked. Every maintenance install/resume also verifies the staged
file is private, owned and regular and matches the admitted binary hash **before**
delegating any candidate `--version` or installation code. This adapter does not
rerun a production smoke test.

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

`gate-drain` and every `resume-new` require
`normal_exit_no_timeout_no_sigkill`, `callers_paused`, `no_direct_file_writers`,
`autostart_suppressed` and an `offline_record` proof. The private JSON offline
record repeats the enclosing evidence's `run`, `host`, `mode`, `phase`, `nonce`,
`kind` and `observed_ns` exactly, and includes `runtime_sha256` for the current
private `state/runtime.json`. It may also contain inspection details or references
to them. Hash verification, normal exit (integer `exit_code: 0`, `stopped_at`),
record binding and quiescence are checked under both exclusion locks. After any
new start attempt, `stopped_at` must be a timezone-aware ISO timestamp no earlier
than the latest `start_attempted_ns` and no later than the evidence observation.
An old clean runtime receipt cannot authorize recovery from a later attempt.
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
- All Wiki lexical/embedding projection tasks, including active, expired and
  future leases, unknown task kinds, generations and revision/label inputs.
- All Wiki pages and search documents used by startup search repair, including
  matching/deleted controls and missing-page documents. Repair can create a new
  task even when the task table was empty before startup.
- Wiki repository heads, including every non-NULL reference-effects marker
  (zero is not NULL). Reference recovery rebuilds all live pages for that repo.

Each entry has a required `recovery_class`:

| Class | Meaning and source predicate |
| --- | --- |
| `startup_execute` | Startup recovery/its recurring worker can select this object, including local terminalization, repair and deferred retry/lease reclaim. It is not a promise that the first batch immediately executes an external write. |
| `audit_only` | No corresponding startup recovery selector executes this object in the pinned source, but its retained outcome, receipt, lease or repair input still requires review. This is not permission to discard it or skip the gate. |

Classification follows these source selectors, independently of whether a
particular deployment enabled an integration, dispatcher or embedding provider:

- `ResumePendingForgejoProjectionJobs` selects the ten listed nonterminal phases,
  heals only generic Forgejo `failed_terminal / provider_admission_denied`, and
  can terminalize closed generic jobs. Future retries and work beyond its first
  100 rows remain admitted. GitHub resume selects nonterminal
  `github_shadow_ensure` / `github_shadow_close` jobs. Other nonterminal jobs and
  terminal success-comment claims remain `audit_only`, not silently omitted.
- A reopen marker executes only for an open, unmerged PR. Other pending markers
  remain audit-only.
- `RecoverDurableActionIntents` selects only `action = pr.rebase` in `planned`,
  `dispatching`, `dispatched`, `accepted`, `running`, `recovery_needed`. It may
  deny expired/ambiguous intents rather than replay them; it also terminalizes
  nonterminal historical rebase jobs with no intent binding. Terminal intents,
  other actions/states, and Access Grant invocations are audit-only. Unknown or
  NULL provider outcomes and boundary receipt lineage retain the existing full
  review scope; unknown is not a startup replay instruction.
- Outbound `pending`, `retry_wait`, and `delivering / multica_external_pr` are
  worker candidates, including deferred retry/lease expiry. Other statuses and
  retained terminal leases are audit-only for delivery recovery.
- `server/wiki_gc.go` starts both Wiki recovery workers.
  `repairWikiSearchProjectionTasks` queues lexical repair for live pages with no
  document or SQL `docs.revision_sha <> pages.head_blob_sha`, and documents with
  no live page. SQL NULL comparison semantics are preserved, not replaced with
  Python inequality. Other page/document inputs remain audit-only.
  `claimWikiSearchProjectionTask` drains only `lexical` and `embedding`, with
  `lease_expires_at IS NULL OR lease_expires_at < now`; unexpired leases remain
  startup-executable deferred work, while unknown kinds are audit-only.
  `recoverPendingWikiReferenceEffects` selects heads with a non-NULL marker,
  joined to repositories; a missing repository blocks inventory. NULL-marker
  heads remain audit-only inputs, not fabricated reference work.

This is the durable recovery/admission set, not a complete list of every startup
side effect. Seed, migrations, retirement, catalog/outbound GC, Git maintenance,
provider watchers, configuration and Git/blob-store restore consistency retain
separate preflight/backup/observation requirements. In particular, `audit_only`
does not assert that an unrelated retention worker can never delete a row.

Required tables/fields must exist; unknown schema blocks. Old databases may
lack the three additive PR lifecycle columns. Linked PR facts and referenced
immutable boundary receipts are included in per-object fingerprints; missing
links block. Wiki fingerprints bind the repository, head, both sides of the
page/document join (including absence/deletion), page-label links and referenced
labels. Reference-marker fingerprints bind all pages/documents/labels in that
repository so body or membership drift cannot reuse its approval. Wiki inline
BLOB bodies are privately encoded as hexadecimal for hashing; no body bytes or
lease tokens are exported. Git-backed body hashes remain references: inventory
does not replace independent blob/Git restore validation. Whole rows are hashed
without exporting payloads, endpoints,
receipt bodies, labels or free-form errors. Historical SQLite TEXT is decoded
with UTF-8 `surrogateescape`, never replacement or ignored bytes. Its private
canonical JSON uses ASCII escapes (`ensure_ascii=True`); parsing that JSON and
encoding each text value with `encode('utf-8', 'surrogateescape')` recovers the
original TEXT bytes, including invalid UTF-8 and NUL. This representation is
not a database repair and must never be written back to production SQLite.
Valid UTF-8 fingerprints are unchanged; changing any retained byte in an
in-scope row or linked fact invalidates its review fingerprint. Terminal rows
are still scanned before filtering, even when they need no recovery admission.
No error text or reversible rendering is added to inventory output.
The output contains only object
keys, reason, recovery class, a facts digest and an unknown-result flag. It is
still private.
The source predicates are in `pr_projection_jobs.go`, `github_projection_jobs.go`,
`forgejo_action_intent.go`, `outbound_delivery.go`,
`wiki_search_projection.go` and `wiki_reference_recovery.go`; a scheduler change
requires a new adapter review.

Each convergence gate requires `recovery_review: {items: [...]}`. Empty inventory
requires an empty list. A nonempty list must match every current object key
exactly once, including audit-only rows and every Wiki category. Old six-table
reviews are insufficient. Each item binds the matching `recovery_class`,
`facts_sha256`, `reviewer`,
`decision: allow_exact_resume`, an `expected_effect_sha256`, integer
`unknown_external_results: 0`, and `readback` plus `exact_resume_test` proofs.
The reviewer must establish exact permission/generation/ref/PR, expected effect,
idempotency and external acceptance readback, including resolved unknown DB
flags. For audit-only rows the expected effect and exact-resume proof must
establish a no-op for that object, not authorize a new provider write. Class
changes, missing categories, extra keys and duplicate keys block; neither class
weakens the required proofs. Boolean risk acceptance alone does not pass.
Changed attempt, head,
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
set with fresh `resume` evidence and the bound offline record above. This entry
is allowed from `install-allowed`, `installing-new`, `installed-new`,
`start-attempted`, `new-ready` and `observing`; it never stops or starts a service.
It then requires a separate `start-new`. A still-running or ambiguous migration
must not be restarted; first use readback and inspect the manager.

If health or provider acceptance fails during `observing`, preserve the failed
scene and arrange a separately authorized normal stop and external reconciliation.
Only then use `resume-new`, `start-new`, `validate`, `observe`. Resume invalidates
the previous observation start before any install mutation. Fresh validation
starts the entire observation budget again; failed-window time is never reused.
Provider facts/attempts/receipts must be reviewed again at recovery and start.

A crash, nonzero/missing exit status, timeout or SIGKILL is **not** a normal stop.
This adapter deliberately has no abnormal-exit waiver, even when no process is
left. Such cases stay blocked pending a separate, independently reviewed abnormal
recovery protocol; neither a generic `resume` proof nor an edited runtime/phase
record can manufacture normal-exit evidence. A later candidate likewise needs a
separate compatibility review and adapter update.

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
