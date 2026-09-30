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

Preparation makes a no-hardlink mirror of the local bare repository and rewrites
its commit graph itself; no Python/filter-repo runtime is required. Tree entries
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
without retaining the old commit or blob object bytes. It does **not** rename
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

A private sibling state directory stores the deterministic plan and completed
receipt. Both are bound to the SHA-256 of the complete canonical intent; reusing
an `operation_id` with different blobs, ancestor or recovery evidence is refused.
The plan records original/clean refs, the commit map, expected current tree,
signature removals and staging location. If staging disappears before publication
it is rebuilt and must reproduce the same old/new default head and tree. If local
refs are already clean after a crash, startup re-reads required provider branches,
verifies the replacement map, finishes GC/readback and resumes DB reconciliation.

The intent requires an operator-owned off-host recovery locator and digest.
AGS records that evidence; it does not create or upload the external archive or
gain authority over its retention.

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
11. ordinary automatic maintenance still accepts the renamed retention refs.

The migration receipt never claims that a backup provider, snapshot filesystem
or off-host recovery archive has physically released its own storage.
