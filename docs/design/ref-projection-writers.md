# Ref operation writer inventory and adapter assessment

This is a **fencing coverage inventory**, not an allowlist of executable writers.
S0/S1 installs no gates and closes no legacy delete path. Each row is currently
**unproven/unfenced for strict ref operations**. New discovery must expand this
inventory, not weaken the gate. Scope includes refs and all facts used by a
signed baseline/review, not only `git push`.

## Source-side coverage domains

| Writer family | Current implementation / boundary to admit | Required revision and gate coverage |
| --- | --- | --- |
| Native Git protocol | `internal/githttp/handler.go` ReceivePack/serve, `internal/gitbackend` protocol execution, `gitstore/init.go` receive policy/hook dispatcher | Every accepted ref transaction, old/new OIDs, protected refs, repo epoch; gate held until child reap; hooks alone are not all-writer coverage |
| Low-level ref APIs | `internal/gitstore/refs.go`: CreateBranch/CreateBranchFromOid (go-git SetReference), CreatePRRef, UpdateRef/CAS/CASWithHead/Safe, CreateRef, DeleteRef, CreateTagIfNotExists | All names/OIDs including tombstones; local CAS has no mutation journal or ABA detection |
| Default HEAD/repo init/fork | `gitstore/init.go` Init/Fork/seedReadme/Delete, `githttp/handler.go` fixHEAD, `service/repo.go` CreateRepo/DeleteRepo/UpdateRepo | Immutable repo identity/lifecycle/default branch, symbolic HEAD, fork/bootstrap refs, rename/delete; no ID reuse |
| Contents and bulk commit | `gitstore/content.go` updateRef/updateRefCAS, WriteFile/WriteFileIfBranchHead/DeleteFileFromRepo/MoveFile(s); `commit_files.go` Persist/PublishPreparedCommit, CommitFiles/At/RootEmptyTreeAt | Publication, not merely object preparation; prevent a prepared commit bypassing head admission |
| REST refs/contents/tags | `internal/rest/handlers_git.go` Put/DeleteRepoContents, Create/Update/DeleteGitRef(Generic), DeleteGitTagRef, CreateTag; `handlers_misc.go` fork parent update | REST aliases must use same low-level gate; no separate handler-only mutex |
| Merge/rebase/revert/update-branch | `gitstore/merge.go`, `merge_checked.go` publishCheckedMerge; `service/pr_merge*.go`, `provider_merge_reconcile.go` | Multi-ref transaction, PR head/base facts, merge result/ancestry/history; temp-clone push cannot bypass |
| Provider-driven synchronization | `service/forgejo_webhook.go` syncForgejoMergedBranchToAGS and source cleanup; `forgejo_action*.go`/`forgejo_rebase*.go` intents/workers; `pr_projection_jobs.go` repair/retry | Remote effect attribution, exact source old/revisions, all pending dispatched attempts; a local claim change cannot revoke a sent write |
| PR membership and history | `service/pr.go` CreatePR/UpdatePR/retarget/reopen/UpdatePRCommitData/CreatePRRef; `pr_merge*.go`, `pr_integrations.go`, `pr_projection_jobs.go`, `forgejo_webhook.go`, action/intent/review/comment writers | All states, head AND base immutable repo identities, reverse head-domain updates for OTHER base repos/forks; historical roots append-only, completeness watermark |
| Wiki mutations | `internal/wikiv2/foundations.go`, `service/wiki_compact.go`, `wiki_projection_reconcile.go`, `wiki_catalog_post_commit.go`, wiki Git receive repair | Separate wiki identity/namespace remains out of executable selection; shared low-level gates must not overlook these writers or lock order |
| History/blob retirement | `internal/artifactretirement/{rewrite,publish,staging_roots}.go`, `service/artifact_retirement*.go`, `gitstore/retired_blob_policy.go` | Operator offline/staging and active publication are distinct; rewrite/replace/backup/root retirement affect history and generations; existing artifact-retirement is NOT new ref retirement |
| Maintenance/storage | `gitstore/maintenance.go` ref transactions/prune/pack, `service/git_maintenance.go` roots and retention | Admission for visible refs/retention, no removal of operation/archive pins; root errors stop GC. Provider-only roots stay outside AGS bare objects |
| Replica snapshots | `internal/snapshotstore/git.go` update-ref transaction, replication/edge ingestion | Replica storage is not AGS authoritative source; cannot supply source revision proof. Explicitly excluded domain, never a bypass into source |
| Mapping/integration policy | `internal/integrations/config.go`, `server/server.go` composition/reload, `forgejointegration` target resolution and policy clients | Non-secret mapping/policy/credential generations, target ownership uniqueness across instances, disabled reason retained; file reload and runtime replacement are writers too |
| Authentication/authorization | `service` users/org memberships/repo permissions, delegated/session/grant revocation, branch protection/settings; future retirement review/revoke | Current server-owned rights, native reviewer subject/issuer, disablement and revocation revisions serialized at commit; mapping/provenance cannot grant permission |
| Repo deletion/cascade | `service/repo.go` deleteRepoCascade plus new operation/review/archive lifecycle | Preserve immutable idempotency tombstones, journal/reviews/manifests independent of repo FK cascade; refuse in-flight deletion/retention handoff gaps |
| Pending work / admin bypass | Already-started native children, queued jobs, old versions/processes, imports/restores/operator offline commands | Drain/fence old epochs before enabling tracking; root/OS direct mutation is outside malicious-admin model but detection withdraws capability; do not initialize unknown history as revision0 complete |

Inventory method: enumerate low-level update-ref/SetReference/DeleteRef/commit
publication and work outward through service, REST, Git HTTP, wiki, retirement,
maintenance, provider callbacks and configuration/authority writers. Object-only
blob/tree/tag/commit construction (`git_database*.go`) is not head publication,
but published objects and required history roots still need retention/integrity
coverage. Process-local repo locks and snapshot barriers are not distributed
source/provider admission and cannot be repurposed around network calls.

## Provider and cross-domain coverage

| Writer family | Existing path or external source | Receiver proof required before capability=true |
| --- | --- | --- |
| Ordinary native projection | `forgejointegration/integration.go` HandlePushWithResult/pushAndVerify, GitPush/GitRemoteRefSHA; `service/native_ref_projection.go` and Forgejo/GitHub/GitLab dispatch | All create/update/delete/forced mirror effects must join provider repo/ref journal and native protection checks; legacy bare `:ref` cannot coexist as fallback in a strict domain |
| PR head repair/backup | RewriteBranchWithLease/rewritePullRequestHeadWithLease/createRemoteBackupTag; PR worker retry/repair | All backups and forced rewrites accounted, actual selected transport principal; PR repair must not become a general ref executor |
| Existing artifact rewrite | `artifact_retirement_policy.go`, service artifact retirement provider checkpoint/force window | Ref and protection changes both journaled; no temporary policy downgrade by the new protocol |
| HTTP PR/config/ACL | `client.go`, `attribution.go`, `authority_policy_http.go`, `managed_ssh.go` | Ref create/update/delete, merge/squash, protection/default branch, collaborators, user/key/token/permission/review revocation; Sudo/bot/ACL fallback is not new-operation authority |
| Provider-native Git | Users/bots/other AGS instances, deploy keys | Every Git writer at actual commit point, continuous old/new revisions/tombstones, operation ticket/epoch and native hooks/protection; not just AGS pusher instrumentation |
| Provider REST/UI merge/ref/delete | Native provider code paths outside AGS transport | Same admission+journal as Git; ordinary receive hooks alone cannot demonstrate REST coverage |
| Actions/automation/mirrors | Provider runners, scheduled jobs, remote mirrors/admin imports | Every mutation participates; detect old dispatched/in-flight writes and stale epochs across processes/restarts/partition |
| Cross-base/fork PR | Another base repo opens/reopens/changes head/base into this head repo | Immutable head reverse domain admission, membership and append-only history revision/watermark atomically updated; target base pulls pagination is insufficient |
| PR history/cache/refs | Internal PR refs, merge-result/old head, comments/reviews/attempt backups | Historical completeness and R, explicit pure cache derivations only; async/unattributed changes invalidate capability |
| Repo/config/authority lifecycle | Rename/delete/default branch/protection/user/key/credential/permission changes | Immutable instance/repo target identity, unique cross-deployment owner, authority/revocation current at commit; fail closed on inaccessible domains |
| Provider webhook/internal cleanup | `webhook.go` repo/branch-only event; `service/forgejo_webhook.go` handleForgejoDeletedBranch and cleanupMergedSourceBranchIfForgejoDeleted | S7 must suppress unattributed late events and internal synthetic equivalents, never delete rebuilt AGS head or propagate to other providers. HMAC alone lacks old SHA/effect/epoch |
| Guard/receiver recovery | Not implemented | Receiver commit+journal durability/continuity, source guard live, TTL/epoch/review revocation enforced under partition, crash boundaries and readonly recovery; no ordinary push fallback |

## Real adapter assessment (negative capability result)

The in-tree real-provider acceptance fixture uses `codeberg.org/forgejo/forgejo:12`
in `scripts/test-forgejo-projection.py`, reads `/api/v1/version`, and tests
ordinary native protected-branch rejection plus exact-head bot projection. The
major-version image tag is not an immutable patch/build/digest pin and that test
does **not** install a source admission coordinator or receiver journal. This
assessment inspects the repository adapter, not a live production Forgejo.
No mini/imile host, deployed provider or business database is contacted.

For **that current HTTP/native-Git adapter**, strict support is **false**:

| Required capability | Evidence now | Decision |
| --- | --- | --- |
| Native old-SHA CAS / protection | Existing lease push/readback and protected-branch fixtures | Useful primitive only; ABA remains possible |
| Atomic tag-create/head-delete | Git has an atomic transport option; no new narrow receiver implemented | Unknown -> false |
| Durable receiver journal + exact C prefix | No receiver implementation/store/commit integration | False |
| All Git/REST/UI/Actions/config writers | AGS controls its own requests only; native provider integration unproven | False |
| Reverse PR/history completeness | Current ListPullRequests queries one base repo/current snapshot | False |
| Cross-domain partition/epoch/revoke commit gate | Local beforeWrite, mutex and attempt CAS are not receiver gates | False |
| Exact Forgejo patch/build support | Major-version disposable fixture only; no strict adapter proof | Unknown -> false |

A future candidate must first pin exact Forgejo version/build and image digest,
identify the native Git and REST merge/delete/ref commit functions and every
Actions/config/authority/cross-PR writer, then implement a receiver integration
without bypassing native authorization/protection/hooks. A generic receive hook
or gateway covering only Git is insufficient. If upstream cannot carry all of
these gates, leave it plan-only or propose a separately reviewed provider change;
do not directly modify/read the provider business DB as an undocumented shortcut.

The required isolated acceptance suite must run real Git + REST merge/delete +
Actions + cross-base PR mutations + configuration/permission changes, two AGS
instances, stale dispatched workers, ABA, process kill/reap, journal durability/
rollback/gap, source/provider partitions and reviewer revoke-before/after-commit.
Every uncovered path, unknown version or missing history sets capability=false.
Fake receiver success tests may validate protocol models in S3, never substitute
for these real-adapter tests. Enabling any production domain also requires S7
legacy delete/cleanup bypass closure and explicit production approval.
