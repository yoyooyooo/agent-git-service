# Local backup and recovery contract (S1)

S1 defines and validates recovery inventory and evidence. It does not copy a
backup, seal files, restore data, delete anything, stop/start a service or enable
upgrades. `upgrade.py`, the host adapter and transaction engine still reject
`apply` with `backup_contract_unimplemented` / 83 after authorization. Direct v2
install also remains closed with 83. The public deployment and migration policy
stays empty. See [local upgrade](local-upgrade.md) for the entry and gates.

## Recovery group

`scripts/upgrade_backup_contract.py` owns the closed JSON contracts;
`scripts/upgrade_inventory.py` supplies bounded read-only components, with no CLI
or production call from apply. A caller must supply an operator-reviewed exact
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
| `sqlite` | One persistent local business DB: every table, row, schema object, index, trigger, FK, intent, lease, audit and migration fact. The future snapshot producer must use the SQLite backup API, including committed WAL; copying only the main file is insufficient. |
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
links, no tracked checkout as destination, 0700 backup directories and 0600
sensitive files. S1 validates declarations; it creates no destination and makes
no claim that filesystem permissions/ACL/xattr restoration work on a host.

`capacity` has integer `backup_bytes`, `restore_copy_bytes`,
`candidate_scratch_bytes`, `migration_growth_bytes`, `runtime_reserve_bytes` and
`available_bytes`. Restore capacity must cover at least the full backup. The sum
of all five costs must fit the declared available bytes, or validation refuses
with `insufficient_space`. Inventory also rejects a backup-byte estimate below
measured source bytes. A future cold-window preflight must measure live free
space and recheck it before effects; declared space alone is not that proof.
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
S1 only defines and validates these values. It implements neither retention
selection nor backup/restore-copy deletion. Any later deletion uses the system
Trash, on macOS `/usr/bin/trash <absolute-path>` without `--`, never permanent
unlink. Insufficient space refuses the operation; it does not trigger cleanup.

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
ACL/xattr digests reference required metadata; S1 does not implement capture or
restoration. Every declared object must be covered. Future S2 must compare this
manifest against actual complete payload bytes and metadata, establish source
stability, fsync files/directories and publish the seal only after success.

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

Future restore verification must use an independent ordinary copy with no shared
inodes and no production target, verify every file, run SQLite integrity/FK and
source/backup/restore logical comparisons, Git fsck and DB/CAS dependencies, and
never start AGS with production credentials or contact providers. New-version
startup can migrate data and resume business; after `start_attempted`, a binary
only rollback is unsafe. Full DB+Git+CAS+configuration+old-program restoration
requires separate incident authorization and reconciliation of later facts.
Independent media/encryption is strongly recommended; same-disk copies cover a
narrower failure domain. SHA256/owner permissions do not authenticate a human or
protect against a malicious same-UID/root writer.

## Verification boundary

Run `scripts/ags-python.sh scripts/test_upgrade_inventory.py` with independent
HOME/TMPDIR/cwd and no inherited PYTHONPATH/PYTHONPYCACHEPREFIX/BASH_ENV or
PYTHONDONTWRITEBYTECODE (the shell entry establishes bytecode controls itself).
The suite uses real SQLite and bare Git with synthetic identities. It covers
empty DB/repo, optional Wiki, historical CAS, typed row bytes, WAL, DB-only and
cross-fork OIDs, external SHA classification, schema/source drift, dependency and
budget failures, seal tampering and the unchanged 83 gates.

This is source/component evidence. It does not establish a live host's writer
set, effective configuration graph, normal shutdown, cold-window durability,
complete backup/restore, real key decryption, native manager/ACL/xattr behavior,
production migration, business/provider health or disaster recovery. Later
slices must implement and verify those parts before apply can be connected.
