# Local backup, recovery and manual acceptance (S1–S5)

S1 owns inventory and evidence; S2 copies the full declared recovery group with
SQLite backup API and ordinary copies and verifies an independent restore-copy.
S3 owns the durable cold-window protocol; S4 consumes separate human acceptance.
S5 connects these existing owners through strict private-package admission.
The public deployments/migration policy remains empty: default apply and backup
still return `backup_contract_unimplemented` / 83. Direct v2 install always returns
`install_contracts_unimplemented` / 83 and cannot borrow the S5 admission.
No source acceptance provisions, publishes or upgrades a host. See
[local upgrade](local-upgrade.md#s5-two-layer-admission) for the exact package/adapter contract.

## Recovery group

`scripts/upgrade_backup_contract.py` owns the closed JSON contracts;
`scripts/upgrade_inventory.py` supplies bounded read-only components, with no standalone backup CLI; admitted S5 apply reuses them through S3. A caller must supply an operator-reviewed exact
SQLite schema digest and dependency graph. Computing a digest with
`schema_digest` discovers bytes; it does not admit or approve that schema.
Unknown tables, a changed schema, missing required semantic tables/columns,
unknown store paths, missing objects/dependencies, unsupported backends and
budget overruns are errors. No truncation is accepted as success.

The group schema `ags.recovery-group.v1` has exactly `schema`, `objects`,
`dependency_review_sha256`, `storage`, `retention` and `capacity`. Each object has
`id`, `role`, `source`, `restore_target`, `kind` and `depends_on`. IDs and edges
must be unique, dependencies must exist and be acyclic, and absolute source and
restore paths must match. Relocation is unsupported. Overlapping object roots,
symlinks, hard links and special files are refused; explicit link restoration is
reserved for a later reviewed contract. Empty directories are inventoried.

A complete group includes:

| Object | Required coverage |
| --- | --- |
| `sqlite` | One persistent local business DB: every table, row, schema object, index, trigger, FK, intent, lease, audit and migration fact. The snapshot producer uses the SQLite backup API, including committed WAL; copying only the main file is insufficient. |
| `git_root` | One complete Git root, with all bare main repositories and existing optional sibling Wiki repositories, objects/packs/indexes, refs/reflogs/HEAD/config/hooks and metadata. Unreferenced and DB-only objects remain included. Clone or `bundle --all` is insufficient. |
| Wiki and auxiliary data inside Git root | All existing `.wikiblobs/<aa>/<bb>/<sha>` CAS, including historical/unreferenced content, DB inline bodies and revisions; `.github-app-deliveries` spool; `.ags-artifact-retirement` only with a private, empty, non-busy `operation.lock`. Pending/force/unknown retirement state is refused. |
| `runtime_env`, `config`, `secret`, `policy` | Current environment, integration/replication/retention/authority configuration, all referenced keys/certificates/seeds/receipts and recoverable `SECRET_ENCRYPTION_KEY`. Never copy all of HOME. |
| `old_binary`, `installation`, `launcher`, `manager`, `tool` | Exact old gh-server bytes and identity, old installation receipt, launcher and its complete dependency chain, actual manager definition and tool/policy identity. S1 does not execute these files. |
| `external_authority` | Each additional configured authoritative upload/blob/LFS/artifact path and same-period recoverable version. An external service without a bounded recoverable contract is unsupported. |

The required roles are SQLite, Git root, runtime environment, old binary,
installation receipt, launcher, manager, policy and tool. A secret already in
runtime.env is included with that file; separate secret files must also appear
in the graph. SQLite, Git root, runtime environment, old binary and installation
are singleton roles. Recovery graph review must traverse nested integration YAML,
replication JSON, policies and manager definitions. S1 hashes these declared
files and requires `dependency_review_sha256`; it does **not** automatically
understand arbitrary nested configuration formats. The environment scanner
checks explicit file/directory variables and known integration-map references.
The review digest is a reference to required review, not proof of completeness,
a signature or approval. An undiscovered dependency cannot be claimed verified.

Paths follow the actual store: `<GitRepoDir>/<full_name>.git`, for example
`sample/project.git`, with optional `sample/project.wiki.git`. `GitStorageID`
is a replication identity, not the directory name. See [Git store](../architecture/gitstore.md).
Name collisions with another repository's optional Wiki are refused.

SQLite DSNs support explicit `sqlite:`, `sqlite://` and local `file:` forms with
absolute paths. Relative/bare DSNs, memory DBs, remote authorities, immutable,
custom VFS and unknown query options are refused. Only `mode=ro|rw|rwc`,
`cache=private` and enabled foreign-key options are recognized. The actual reader
always opens `mode=ro`, enables query-only and uses one read transaction; it does
not ignore WAL. A rollback journal is refused. This does not prove writer
quiescence and is not a backup implementation.

## Inventory and source mapping

The inventory records an exact schema digest (including user_version/application_id), complete typed logical row digests,
per-table counts, all Git-root paths (including empty directories), file digests,
Git refs/HEAD digests, local OIDs, external OIDs and legacy synthetic Wiki OIDs.
SQLite NULL, BLOB, non-UTF-8 TEXT and numeric storage classes remain distinct.
Limits are closed and bounded: at most 100,000 rows/paths/OIDs, 64 MiB encoded row
facts, 16 MiB individual SQLite values/JSON and 64 MiB individual CAS content.
Exceeding limits fails; a future larger supported deployment needs reviewed
limits and tests. Individual Git commands have a 120-second timeout.

The local commit-root queries match `Service.GitMaintenanceRoots`, including
base/head repository PR roots, reviews/comments, statuses, pages, workflow,
projection/intent/delegation constraints and exact deployment refs. Every local
OID must exist in the correct bare repository with the correct object type.
External provider SHA columns are classified separately and never treated as
local objects. Wiki changesets with format 0 and a known non-Git source are
legacy synthetic identities; format 1 or Git-originated changesets require a
real Wiki commit. Wiki projections/history/repair roots require real Git objects.
Inline/current/historical bodies use Git blob framing for digest checks. Legacy
inline `wiki_blob_refs` metadata does not require a CAS file, matching the
catalog GC contract. Non-inline referenced and pending CAS must be present.
Unknown Wiki source/format or malformed OID fails.

`deleted_at` classifies a Wiki page. A live row (`deleted_at` empty) must match
its head revision's blob, size and inline body exactly. `applyDelete` in
`internal/wikicatalog/applychange.go` is different: it inserts an `op=delete`
revision with an empty blob and zero size, then updates only `deleted_at`,
`updated_at`, `head_revision_id` and `head_changeset_id`. The page keeps the
body it had before deletion. Inventory accepts that tombstone only when the
head revision is that delete row and its changeset belongs to the page's repository.
The retained blob, size and inline body must exactly match an earlier content
revision of the same `page_id`, with `op` in `create`, `update`, `rename`,
`restore` or `compact`. That candidate's `revision_id` must be strictly less
than the delete head's `revision_id`, and its changeset's `repository_id` must
equal the page's `repository_id`. A later restore revision or another repository's
changeset cannot prove the retained body. A deleted row is not skipped.
A live row pointing at a delete revision, or a tombstone whose head is still a
content revision, is `wiki_head_revision_mismatch`. A
tombstone whose retained body has no candidate meeting all these conditions
is `wiki_tombstone_body_unproven`. Restore/recreate clears `deleted_at` and
writes a new content head; that row is live again and must match its head.
After another soft delete, the bound is the new delete head, so a restore
revision between the two deletes can prove the retained body.

Each bare repository runs `git fsck --full --strict`; Git uses a minimal isolated
environment, no global/system configuration, hooks, replace objects, automatic
maintenance, prompts or allowed network protocols. Alternates, shallow/promisor,
partial clones, include directives and unsupported repository extensions are
refused. Dangling objects are preserved, not pruned. No AGS process is started.

S1 conservatively fingerprints **all** rows of the recovery tables from
`ags_maintenance_window.TABLE_KEYS`, plus Wiki history/CAS/repair tables. It does
not import that helper's private executor, infer startup permission or authorize
provider replay. The output says `requires_separate_review`; future startup
classification/readback remains required. `test_upgrade_inventory.py` compares
all root query mappings, constraints keys, deployment refs and recovery-table
sets against source and pins the source control flow/storage semantics. Source
changes fail the test until the semantic mapping is reviewed and updated.

## Storage, capacity and retention

`storage` contains `backup_root`, `restore_root`, `private_policy_sha256` and
`copy_method=ordinary_copy`. Both roots must be distinct absolute paths outside
all source roots, outside each other, `/tmp` and `.git`. Future host provisioning
must additionally enforce private policy ownership, no writable ancestors or
links, no tracked checkout as destination, and 0700 backup roots. Payload modes
are preserved inside that private root; evidence files use 0600. S1 validates
declarations; it creates no destination and makes
no claim that filesystem permissions/ACL/xattr restoration work on a host.

`capacity` has integer `backup_bytes`, `restore_copy_bytes`,
`candidate_scratch_bytes`, `migration_growth_bytes`, `runtime_reserve_bytes` and
`available_bytes`. Restore capacity must cover at least the full backup. The sum
of all five costs must fit the declared available bytes, or validation refuses
with `insufficient_space`. Inventory also rejects a backup-byte estimate below
measured source bytes. S2 measures free space before creating a partial backup.
It checks both roots
when backup and restore storage are on different filesystems, and rechecks the
restore root before creating a restore copy. Declared space alone is not proof.
Moving material to Trash does not release usable space.

`retention` is a closed, fixed contract, not a deletion switch:

| Field | Required value |
| --- | --- |
| `scope` | `per_host` |
| `keep_verified_sealed` | `5` |
| `eligible` | `sealed_and_restore_verified` |
| `restore_copy` | `trash_after_verification` |
| `failed_or_incomplete` | `retain_for_manual_review` |
| `deletion` | `system_trash_only` |
| `insufficient_space` | `refuse` |
| `trash_counts_as_free_space` | `false` |

Each machine retains only its latest five sealed **and independently restore
verified** backups. After validation the restore-copy is removed through the
verified system Trash. Failed/incomplete backups or failed restore copies do not
count toward five and are not automatically deleted; they await manual review.
S2 counts a backup only after it validates the S1 seal, host identity and the
separate restore report's manifest binding. It sorts by the seal's `ended_at`,
not directory modification time. It accepts only safe direct children of the
backup root, never a symlink, the root itself, a traversal or an outside path.
Before moving an old backup, it reads and validates that evidence again.

The caller must inject a verified system Trash function. There is no default
implementation. On macOS the function must use `/usr/bin/trash <absolute-path>`
without `--`. Only a successful restore copy and eligible backups beyond the
latest five go to Trash. Failed partial backups and failed restore copies stay
in place for manual review and do not count toward five. Trash does not release
usable space. The preflight refuses instead of removing material to make room.

## Seal and independent restore evidence

Canonical JSON is ASCII JSON with sorted keys, compact separators, escaped
non-ASCII and one trailing LF. SHA256 covers those exact bytes. Duplicate keys
at every nesting level, unknown/missing fields, wrong types (including bool as
integer), non-finite JSON constants, unsafe paths and invalid digests are errors.

`ags.backup-manifest.v1` has exactly:

- `schema`, immutable `snapshot_id`, `binding` and `quiescence`;
- `inventory_sha256`, `recovery_group_sha256`, matching
  `source_before_sha256` / `source_after_sha256`;
- timezone-aware ordered `started_at` / `ended_at`, `retention`, `capacity`, `entries`.

The binding contains `host_instance`, `deployment_id`, `transaction_id`,
`attempt_id`, `authorization_id`, full `old`, `new`, `runtime_tree`,
`control_revision`, `control_tree`, `release_id`, `release_tag`, local `target`,
`asset_sha256`, `pin_sha256`, `tool_sha256`, `policy_sha256`,
`old_binary_sha256` and `new_binary_sha256`. It identifies this machine's asset;
mini and imile may have distinct pins/policies while sharing a release identity.
`quiescence` contains a nonce and stop/writers evidence digests. S1 checks their
shape; it does not produce or establish quiescence.

Every manifest entry contains `object_id`, relative payload `path`, `kind`,
`size`, `sha256` (null for a directory), `mode`, `uid`, `gid`, `mtime_ns`,
`acl_sha256`, `xattrs_sha256` and absolute `restore_target`. Payload paths start
with their object ID and map exactly to the declared restore root; duplicate
paths/targets, missing object roots/parents and a file used as a directory fail.
S1 keeps these fields unchanged. S2 captures and verifies mode, uid, gid and
nanosecond mtime. It preserves those values on both payload and restore copy.
Ownership that the current user cannot preserve is refused. macOS provenance
xattrs are captured, hashed, copied and verified. Other xattrs and macOS ACLs
are refused rather than recorded as absent. On Linux, any xattr, including a
POSIX ACL xattr, is refused. Other platforms are unsupported. These checks do
not establish native service-manager or full host recovery acceptance.

S2 checks the copied payload against the manifest before publication. It also
compares source content and metadata before and after the copy. SQLite uses the
backup API to include committed WAL data, then normalizes the backup to DELETE
journal mode. Offline reads therefore do not create unlisted WAL payload.
Unexpected payload paths are still errors; no suffix-based exclusion hides them.

The backup stores canonical `inventory.json` and `recovery-group.json` beside the
S1 evidence. The manifest's existing inventory and group digests bind these
files without adding S1 fields. Inventory supplies the admitted schema and
source logical digest for later recovery, even if the original source is gone.
Files are fsynced before their directories, and the partial directory is renamed
only after seal and payload validation. `publication.json` records `publishing`
until the rename and root sync complete, then `published`. A failure leaves the
partial or final directory and a failure marker when it can be written. A
missing, failed or publishing marker is not a successful publication. No failure
path deletes or moves that material automatically.

The separate `ags.local-backup.v2` receipt contains `schema`, `state=sealed`,
`snapshot_id`, raw `manifest_sha256`, `inventory_sha256` and canonical
`binding_sha256`. The manifest does not hash itself, the receipt or a later
restore report. A seal means sealed payload, not successful recovery.

`ags.restore-verification.v1` is separate: `schema`, `snapshot_id`,
`manifest_sha256`, `restore_copy_id`, `verified_at`, `checks`, `report_sha256`.
The report digest covers the canonical object excluding `report_sha256`.
Its closed checks are `independent_copy`, `manifest`, `sqlite_integrity`,
`sqlite_fk`, `sqlite_logical_digest`, `git_fsck`, `db_oids`, `wiki_cas` and
`dependencies`, all strict true booleans. S1 validates evidence structure and
binding, not whether someone actually performed those checks. A handwritten
success report cannot bypass apply's unconditional refusal.

S2 requires the reviewed group, DSN and admitted schema. A missing input,
missing inventory basis or invalid seal refuses verification. It copies every
object using the group's role-to-ID mapping and compares device/inode pairs
with the backup payload, not the original data paths. The copy runs SQLite
integrity and FK checks, Git fsck, DB-OID and Wiki CAS checks, dependency checks,
and sealed-source/backup/restore logical comparisons. Only completed checks are
written as true. It checks the final manifest and supported metadata before
publishing a separate report. A restore attempt ID stays inside the unchanged
96-character identifier rule. It embeds the snapshot ID when that fits; a longer
legal snapshot uses a prefix and snapshot digest with the same random suffix.
The report still binds the original snapshot ID and manifest digest. The report
file is fsynced before directory sync
and atomic publication. `restore-publication.json` stays `publishing` until the
report publication and directory sync finish. Retention requires `published`.
A publication error marks the outcome failed or unknown and retains the restore copy. Only successful verification calls injected Trash.

It never starts AGS or contacts providers. New-version startup can migrate data
and resume business. After `start_attempted`, a binary-only rollback is unsafe.
Full data, Git, CAS, configuration and old-program restoration requires separate
incident authorization and reconciliation of later facts.

Independent media/encryption is strongly recommended; same-disk copies cover a
narrower failure domain. SHA256/owner permissions do not authenticate a human or
protect against a malicious same-UID/root writer.

S6.05 keeps that ownership check. `upgrade_metadata.py` is a narrow client for a
registered staging role whose expected owner matches; it refuses live sources,
hardlinks, symlinks, wrong roles and paths outside staging. It does not gain
root and does not replace the S2 validator. A privileged helper run is a
separate provisioned binary and is unverified here. `upgrade_trash.py` allows
only `/usr/bin/trash <absolute-path>` with no flag. Success needs the source to
disappear and a trash entry to be readable. Unknown stays unknown. Insufficient
space refuses; nothing is permanently deleted or purged.

## S3 pre-start recovery

`upgrade_recovery.py` owns the private durable cold-window protocol described in
[local upgrade](local-upgrade.md#s3-private-cold-window-protocol). It does not
redefine the S1 recovery group, binding, seal, retention or restore attempt ID.
It revalidates S2 payloads, publication markers and the restore report's exact
`manifest_sha256` before installation and recovery. The injected Trash callback
must be callable before any restore-copy creation; `None` is refused early.
Retention stays five eligible backups per host; failure material is not eligible
and is not deleted. S3 deliberately does not invoke backup retirement during an
active transaction, so it cannot retire a recovery dependency. S2 remains the
owner of retention selection and Trash execution.

Pre-start recovery restores only old gh-server bytes and the raw old
`installation.json`, retained before installation intent and compared with the
sealed payload. It requires new hostops ownership, both exclusive locks, original
claim binding and data/config equal to Tq. The data fingerprint covers the
admitted logical DB, full Git/CAS tree, config and every other recovery object;
only the binary and receipt being replaced are excluded. S2's full fingerprint
still guards backup and install intent. Source collection is bounded by S1 limits.

A private owner-only `ags.prestart-recovery-approval.v1` file has exactly
`schema`, `approval_id`, `purpose=prestart-recover`, `decision=approve`, `binding`,
`generation`, `journal_sha256`, `issued_at`, `expires_at` and
`actor={type:human,id,source}`. Times must be timezone-aware and currently valid.
The approval ID must differ from the window authorization ID; its exact blocked
cold-journal digest and generation prevent stale reuse. This is a human
declaration, not a signature or proof of an independent identity. The component
never generates an approval.

Approval consumption and `recovery_authorized` are durable before either old
file is replaced. Only matching hashes after both durable replacements produce
`old_restored`. The cold marker remains and no process is started. Recovery
crashes, missing evidence, changed data, unknown phase, generation mismatch,
missing approval or any durable `start_attempted` refuse restoration/replay.
No timeout clears ownership facts. Restarting the old version requires a later
separately admitted protocol; S3 supplies no recovery CLI or old-start command.
The [S6.02 launcher transition](user-runtime.md#cold-window-startup-fence) described
under the cold-window fence is not
that CLI: it only admits a same-binary generation receipt and cannot start a
recovered service.

S6.08 `upgrade_recovery_cli.py` does not change the S3 return. `plan` lists the
old-byte effect and starts nothing. `old-start` requires a separate approval
whose id is not the pre-start recovery approval. Data that differs from Tq,
or a consumed token, refuses. A successful call returns `aborted_old_restored`
and a new generation. It does not spawn a process.

S6.09 `upgrade_forward_recovery.py` refuses an unknown spawn outcome with exit
82, including a missing PID. A closed incident and a new same-version approval
can mint another generation for the same binary. The function itself does not
spawn. Reusing the old start token, an open writer, or a different binary
refuses. A replacement candidate needs a separate release, pin, and backup.
Post-start full DB/Git/CAS/config restore is not implemented or authorized.

## Verification boundary

Run `scripts/ags-python.sh scripts/test_upgrade_inventory.py`,
`scripts/test_upgrade_backup.py BackupTests`,
`scripts/test_upgrade_restore.py RestoreTests`, and
`scripts/test_upgrade_backup_regressions.py`, `scripts/test_upgrade_quiescence.py`
and `scripts/test_upgrade_recovery.py` with independent HOME/TMPDIR/cwd
and no inherited PYTHONPATH/PYTHONPYCACHEPREFIX/BASH_ENV or
PYTHONDONTWRITEBYTECODE (the shell entry establishes bytecode controls itself).
The suites use real SQLite and bare Git with synthetic identities. They cover
empty DB/repo, optional Wiki, historical CAS, typed row bytes, WAL, DB-only and
cross-fork OIDs, external SHA classification, schema/source drift, dependency and
budget failures, seal tampering, source changes, hard links, missing space,
retained interrupted copies, invalid retention evidence and paths, metadata,
file fsync, source-independent role mapping, complete WAL round trips,
alternates/shallow repositories, unknown OIDs, tombstones
without an earlier same-repository body, and the unchanged 83 gates.

This is source/component evidence. It does not establish a live host's writer
set, effective configuration graph, native manager shutdown, host cold-window durability,
production backup/restore, real key decryption, native manager/ACL/xattr behavior,
production migration, business/provider health or disaster recovery. Those field claims need separate operator approval and evidence; synthetic wiring does not establish them.

## S4 acceptance file contract

S4 reuses S1 binding, group, seal and restore contracts and S3's original claim,
marker, cold journal and `verified_backup`. It does not define another backup
format. Acceptance reads the original private
`config/upgrade-authorization-<authorization_id>.json` and checks it against the
awaiting receipt. It validates the historical authorization window at issue time,
not as a new permission to run any upgrade effect.
Default public apply and backup remain `backup_contract_unimplemented` / 83;
S5 private-package admission is distinct from the S4 acceptance evidence chain.
Direct v2 install remains `install_contracts_unimplemented` / 83.

The closed `ags.manual-upgrade-approval.v1` accept declaration has these fields:

- `schema`, `approval_id`, `purpose=accept`, `decision=approve`.
- `actor={type:human,id,source}`, `recorded_at`, `authority_reference`. The reference
  must equal the original `authorization_id`. Actor identity is a human declaration.
- `issued_at`, `expires_at`, both timezone-aware, with recorded time in that window.
- `binding`, the entire original v2 request; `backup_binding`, the entire S1 binding,
  including host instance and transaction ID; `generation` and `evidence_id`.
- `evidence_packet_sha256`, the digest of the packet's original bytes.
- `confirmations`, exactly `start_restores_business=true`,
  `rollback_mode=forward-only`, `prestart_recovery_requires_separate_approval=true`,
  `manual_provenance_only=true` and `no_zero_loss_poststart_rollback=true`.
- `accepted_limitations`, a list of unique `{category,reason}` entries. Allowed
  categories are `native_platform_unverified`, `real_copy_start_unverified` and
  `long_tail_observation_unverified`. Unknown writers, bad FK, missing OIDs and
  unresolved provider results are not waivers.
- `independent_media={status,evidence,reason}`, with status `done` or `omitted`.
- `unresolved_blockers=[]`.

Other approval purposes keep their existing owners: the window uses
`ags.local-upgrade-authorization.v2`; pre-start recovery uses S3's
`ags.prestart-recovery-approval.v1`. The accept consumer does not reinterpret them
or authorize any recovery action.

`ags.manual-evidence.v1` fixes the original request, S1 binding, cold generation,
transaction and cold-journal digests, snapshot ID, seal/restore/inventory digests,
quiescence, installed receipt and binary digests. Its `running` object binds the
runtime file digest, generation, revision, start time and health digest. Observation
records start/end, that generation, business/Git/provider/recovery report digests
with `pass` or `not_applicable` status, and no unresolved blockers. These reports
remain human assertions about the referenced private evidence; S4 does not perform
business writes or provider reconciliation. The packet also binds `release_key`
and either a human handoff or null. Approval follows the observation end.

The `ags.manual-mini-handoff.v1` package contains `mini_pin`, `mini_pin_sha256`,
`imile_pin_sha256`, `mini_packet`, `mini_evidence_package_sha256`, `mini_completion`,
`mini_completed_receipt_sha256`, `mini_approval`, `mini_approval_sha256`, `release_key`,
`mini_generation`, `mini_health_observed_at`, `carried_by`, `source`, `received_at`,
`readback` and `artifacts`. Artifacts retain the original UTF-8 JSON bytes as
strings named `pin`, `packet`, `approval`, and `completion`. Received digests apply
to these bytes, not a re-serialized object. The mini packet cannot contain another
handoff. Readback contains exactly `confirmed=true`, `mini_generation`,
`mini_completed_receipt_sha256` and `mini_evidence_package_sha256`.

The mini completed record must bind its consumed approval and evidence. Mini's
release key must equal the imile target key, while each pin binds its own machine.
Human health observation must follow mini completion, precede receiver readback,
and be no older than five minutes at validation. Expired mini approval is allowed
only as evidence of an already-consumed historical acceptance; it cannot approve
imile. This unsigned protocol cannot prevent a human source error, same-UID
forgery, or mini changing after readback.

See [manual acceptance](local-upgrade.md#s4-manual-acceptance) for durable completion,
crash retry, query semantics and the retained startup fence. Component fixtures
are not production, native-manager or end-to-end upgrade acceptance.

## S5 wiring

[Local upgrade](local-upgrade.md#s5-two-layer-admission) owns the closed package and
root-pinned adapter contract. S5 does not redefine S1 seal, recovery graph, S2
restore report, S3 cold journal or S4 approval/consumption records. Capability
admission precedes claims; stage evidence is generated only in its stage and
revalidated before the next irreversible effect. Missing Git/CAS/config, altered
payload/seal/report or changed source cannot install. No prior snapshot can fill
in this attempt's quiescence nonce and immutable request.

`test_upgrade_wireup.py` uses real host dispatch, the real total gate, SQLite,
bare Git, locks and evidence validators. Release transport/smoke and manager/health
are explicitly synthetic. Real phase crashes and lost responses must refuse
blind replay; pre-start restore requires a new approval; post-start old restore
is forbidden. Two logical hosts use different pins and the same release key,
with completed mini evidence before imile stop and its own final approval.
`bash scripts/upgrade-regression.sh` runs S1–S5 and existing consumer/runtime
regressions in independent HOME/TMPDIR/cwd, retaining its sandbox for diagnosis.

The startup fence is retained after acceptance and pre-start recovery. Normal
manager restarts and old-version restart after recovery are not implemented by
S5 and must not be inferred from completed/old_restored. Native Darwin/Linux
full-chain, isolated VM and production-copy startup exercises remain outside
this source evidence. Publication, installation, service configuration, actual
windows, human decisions and recovery-material retirement require separate approval.

## S6.10 offline full-group plan

`upgrade_recovery_cli.plan` accepts `kind=full-restore` for an offline plan only.
It requires the original complete S1 group and seal, unchanged payload,
inventory/dependencies, and a published successful S2 restore report. A missing
tool, secret dependency or member refuses the entire plan. Audit claims,
authorizations, journals and runtime authority are not restore-group members.
The result has `effects=[]`, `started=false`, `live_switch_supported=false`.
There is no full-restore apply entry, live rename or manager capability.

Before any separately approved implementation can switch live data, preserve
the failed group including WAL/SHM, close every writer, materialize independent
staging and rerun S2. Explicitly approve Tq data loss and reconcile external
provider/Edge results. Journal every object switch; a mixed group remains fenced.
Never merge old WAL/SHM into the restored database. Tool/manager restoration
belongs to an independent frozen provisioner, never the running recovery tool.
Old-start and incident acceptance require separate approval. This source slice
provides neither those effects nor a claim that full live recovery was tested.
See [provisioning](upgrade-provisioning.md) and [local upgrade](local-upgrade.md).

## S6.11 long-window checkpoints

[Handoff v2](local-upgrade.md#s611-human-handoff-v2) separates immutable completion
from append-only fresh manual observations. A backup or observation longer than
five minutes requires another human readback; expiration never becomes success.
Refresh registers evidence only. It neither clears claims nor rewinds
`start_attempted`, and it does not enable blind apply replay. Existing S3 recovery
remains the only effect owner. Explicit long-transaction resume and changed-mini-
generation lifecycle proof are still blocked; do not use the pure S6.08/09
planning helpers as production start commands.
