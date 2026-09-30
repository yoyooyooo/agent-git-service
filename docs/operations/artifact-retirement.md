# Explicit artifact retirement at primary startup

**Status: implemented for an operator-approved, exact-blob migration.** This is
not ordinary Git GC and is never inferred from object age, size, path or a
closed PR. It exists for rare repository-history cleanup where the operator has
already decided that named blob objects must leave the authoritative store.

## Startup contract

Set:

```text
AGS_ARTIFACT_RETIREMENT_INTENT_FILE=/absolute/private/path.json
```

The primary consumes the intent after interrupted action-intent recovery but
**before** automatic maintenance workers, projection workers and HTTP listeners
start. Failure keeps the primary offline and the same intent is retried after
the service manager restarts it. A completed receipt makes later starts
idempotent.

The intent is an owner-private regular JSON file:

```json
{
  "schema": "ags.artifact-retirement.intent.v1",
  "operation_id": "bounded-operator-id",
  "repository": "owner/repo",
  "default_branch": "main",
  "expected_ancestor": "<40-char accepted commit>",
  "recovery_archive": "<operator-owned off-host locator>",
  "recovery_archive_sha256": "<64-char sha256>",
  "recovery_refs_sha256": "<64-char digest of verified archived refs>",
  "allow_signature_removal": true,
  "retired_blobs": [
    {"oid":"<40-char blob>","bytes":123,"sha256":"<64-char sha256>"}
  ]
}
```

The source verifies exact object type, byte size and SHA-256. The accepted
ancestor must still be in the current default-branch history and the current
default tree must no longer use a retired blob. Up to 32 explicit blobs are
accepted. There is no wildcard or "large object" mode.

## Prepare and publish

Preparation makes a local no-hardlink copy of the bare object store, restores
all authoritative ref namespaces, and protects available application-owned roots
in staging. This includes database-only commits that a network clone could omit.
It rewrites the commit graph itself; no Python/filter-repo runtime is required. Tree entries
are removed only when their exact blob OID is in the intent. Empty and merge
commits are retained. Author, committer, timestamps, messages and unaffected
trees are preserved. A changed signed commit/tag loses the invalid signature
only when the intent explicitly acknowledges signature removal.

All local namespaces are included, including heads, tags, PR refs, temporary
refs and AGS retention refs. `refs/ags/retention/<oid>` is **renamed** to the
cleaned identity instead of being left with a mismatched name/target pair.

Every changed commit receives:

```text
refs/replace/<old-commit> -> <cleaned-commit>
```

This keeps server-side historical API/PR reads by the original SHA possible
without retaining the old commit or blob object bytes. Read-only PR range and
merged-content checks resolve these representations explicitly; live ancestry,
exact-effect identity, CI and approval checks do not translate identities.
Ordinary maintenance pins the cleaned representation of a historical root rather
than treating an intentional replacement as lost data. It does **not** rename
historical database facts: old reviews, CI facts, completed authorization facts
and projection events keep their original identities. An old SHA therefore does
not become a new approval. Direct external Git clients should not assume that a
fresh clone fetches the server-owned replace namespace.

Before live publication the staging graph must pass full fsck, keep the exact
current default-branch tree and prove all retired blobs physically absent with
Git replacement disabled.

Live publication first imports the cleaned objects under a private temporary
namespace, then uses one `git update-ref --stdin` transaction with exact old
object IDs to update/create/delete the authoritative refs and create replacement
refs. Unexpected ref movement aborts. Import refs are removed, reflogs are
expired for this explicit migration, native GC runs with immediate prune, and
the retired blobs must then be physically absent. A restart after the atomic ref
transaction resumes finalization instead of attempting a second rewrite.

## Provider projections and active work

Only provider branches that are operationally required are rewritten: the
configured default branch, current projection-ref states and open PR
projections. Forgejo and GitHub use exact `force-with-lease=<ref>:<old-sha>`;
an absent or third-party-drifted required branch blocks startup. Unrelated old
provider branches are not force-rewritten merely because a similarly named AGS
branch exists. Repositories with another active provider on a changed ref fail
closed until that provider gains an explicit migration adapter.

Canonical Forgejo base protection keeps force-push disabled. For that one mapped
base branch only, the migration first verifies normal protected-base authority,
records a private no-secret force-window journal, fingerprints every observed
protection field except `enable_force_push`, and uses the separate policy
operator credential to toggle only that field. The integration-bot Git token
then performs the exact lease rewrite. The original force setting is restored
and normal authority is reverified before the journal is removed. Push failure
still restores the policy. If the process dies anywhere after the journal is
written, the next startup restores/verifies the policy before any rewrite,
worker, projection resume or HTTP listener is allowed to continue. A different
protection fingerprint fails closed rather than overwriting provider policy.

Nonterminal Forgejo projection jobs and active provider action intents block the
migration. Provider branches move before the local atomic ref publication so a
restart can safely recognize provider `old|new` state.

After local publication, only **active coordinates** are updated in the
application database:

- open PR head/base SHA;
- open PR projection `last_synced_sha` after its provider branch converged;
- current projection-ref state identities for a branch that converged.

Historical reviews, projection events, terminal jobs, CI and authorization
receipts remain on their original SHA.

## Durable state and recovery

A private, owner-checked sibling state directory stores the deterministic plan
and completed receipt, both bound to the SHA-256 of the complete canonical intent.
Reusing an operation ID with different blobs, ancestor or recovery evidence is
refused. Traversal components, symlinked paths and unsafe existing permissions
are rejected, not silently repaired.

A startup interlock in the Git root is durable before the first external write:
removing the intent configuration cannot start a half-migrated primary. All
periodic application workers are deferred until this startup gate succeeds.
A separate private Forgejo force-window journal exists only while a protected
base rewrite may need restoration; it contains no credential material.

Provider requirements are checkpointed before writes. Each required remote
branch is actually read back during both normal execution and crash recovery.
The plan records original/clean refs, the commit map, expected current tree,
signature removals and staging location. Rebuilding lost staging must reproduce
the complete planned ref maps, not just the default head and tree. After a crash,
startup verifies the replacement map, finishes GC/readback and resumes database
reconciliation without relabelling old audit evidence.

The intent requires an operator-owned off-host recovery locator, archive digest,
and reference-snapshot digest. The deployment owner must restore and verify the
archive, including available database-only objects, before provisioning the
intent. `ReferenceSnapshotSHA256` computes the reference digest as SHA-256 of the
canonical JSON reference-name/object-ID map. Startup requires the live source
map to match this verified recovery snapshot before preparing any rewrite.

AGS records the physical archive locator and checksum; it does not create or
upload the external archive or claim to have verified its bytes remotely. A
stale archive cannot authorize a changed source graph. Archive retention remains
owned by deployment operations, outside ordinary active-repository cloning.

## Acceptance

A completed migration is not established by a successful normal GC. For the
target repository verify separately:

1. startup receipt is `completed`;
2. the primary is ready on the exact released source;
3. every retired blob is physically absent with replacements disabled;
4. current default tree is unchanged;
5. old server-side commit/PR reads still resolve through the replacement map;
6. active open PR/provider coordinates point at cleaned identities while old
   review/CI/authorization facts keep old identities;
7. Forgejo/GitHub required branches have exact cleaned readback;
8. `git fsck --full` passes;
9. a fresh complete clone contains no retired blob;
10. measured active bare-repo disk use is lower;
11. ordinary automatic maintenance still accepts the renamed retention refs;
12. a stale checkout cannot push the retired blob history back, while a cleaned
    checkout can still push ordinary work.

The completed migration installs an exact retired-blob list consumed by the
server-owned Git receive guard. It does not authorize new changes to that list
based on file age, extension or size. Active cross-repository PRs require a
coordinated migration and are rejected by this single-repository executor.

The migration receipt never claims that a backup provider, snapshot filesystem
or off-host recovery archive has physically released its own storage.
