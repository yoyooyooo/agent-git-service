# Service Layer — Component Reference

## Purpose

`internal/service` is the business-logic and persistence-orchestration layer.
It owns all domain rules, coordinates GORM-backed relational state with Git-backed repository state through `gitstore`, and translates persistence errors into stable sentinel errors for surfaces to map.

Surface packages (REST, GraphQL, OAuth, Git HTTP) call service methods and receive Go values and errors, never HTTP responses.

For the full system overview see [docs/architecture.md](../architecture.md).
Core invariant: `agent-git-service` is Git-backed; see [architecture.md § Purpose](../architecture.md#purpose).

## Scope

Owns:

- all business rules and domain validation
- GORM persistence reads and writes
- Git orchestration through `gitstore`
- cross-entity lifecycle changes (e.g., PR merge updates both Git history and DB state)
- execution-context intake persistence after a registered fixed-egress pull
- canonical runtime actor resolution, task/repository Access Grant lifecycle, invocation receipts, Human single-frontdoor provider operations, and exact provider-effect recovery
- domain side effects such as workflow sync and embedding follow-up
- sentinel error definitions consumed by surface layers

Does not own:

- HTTP routing, request decoding, or response formatting (belongs to surfaces)
- schema definition and migrations (belongs to `db`)
- raw Git operations (belongs to `gitstore`)

## Key Entry Points

### Service Struct

Defined in `repo.go`. Fields:

| Field | Type | Role |
|---|---|---|
| `DB` | `*gorm.DB` | Primary database connection (context-aware) |
| `Git` | `*gitstore.Store` | Git repository storage layer |
| `BaseURL` | `string` | HTTP base URL for generated links |
| `Embedder` | `embedding.Embedder` | Optional semantic search provider |
| `ExecutionContextPuller` | narrow `executioncontext.Pull` interface | Optional immutable startup registry used only for source fact acquisition |
| `ProviderIntegration` | narrow `ProviderPRIntegration` interface | Optional deployment-owned provider evidence/merge executor; callers never supply provider identity or credentials |
| `AllowAnyToken` | `bool` | Dev convenience: accept any token when no tokens exist in DB |

### Service Contract Shape

There is no authoritative `internal/service/iface.go` catalog in the current tree.
Production wiring passes the concrete `*Service` to REST, GraphQL, OAuth, and Git HTTP.
Narrower interfaces can still be introduced later where they remove real test or coupling pain, but the current contract source is the concrete service methods and the domain files listed below.

Milestone vs label semantics:
- Use a milestone for the single primary category/topic on an issue or conversation channel.
- Use labels for cross-cutting attributes and tags that can be multi-valued.

### Implementation Files by Domain

| Area | Files |
|---|---|
| Repository lifecycle | `repo.go`, `repo_fork.go`, `repo_query.go`, `branch.go` |
| Repository authorization | `permission.go`, `repo_access.go` |
| Issues | `issue.go` |
| Pull requests | `pr.go`, `pr_merge.go`, `pr_actor.go`, `provider_merge.go` |
| Execution context intake | `execution_context.go` |
| Canonical actor and Access Grants | `access_grant.go`, `access_grant_merge.go`, `access_grant_merge_runtime.go` |
| Principal-bound workload Sessions and durable operation authorization | `delegated_session.go`, `durable_authorize.go`, `principal_session_authority.go`, `delegation_policy.go` (migration inventory), `repo_access.go`, `audit.go` |
| Comments and timeline | `comment.go`, `timeline.go`, `review_comment.go` |
| Labels and milestones | `label.go`, `milestone.go` |
| Reviews | `review.go` |
| Reactions | `reaction.go` |
| Search | `search.go` |
| Releases | `release.go` |
| Workflows and actions | `workflow.go`, `workflow_dispatch.go`, `workflow_exec.go` |
| Users, orgs, teams, and collaboration governance | `user.go`, `team.go`, `org_membership.go`, `org_invitation.go`, `outside_collaborator.go` |
| Auth and keys | `auth.go`, `keys.go` |
| Projects | `project.go` |
| Wiki | `wiki.go`, `wiki_label.go`, `wiki_search.go`, `wiki_rewrite.go` |
| Other | `gist.go`, `star.go`, `dependabot.go`, `deployment.go`, `invitation.go`, `ruleset.go`, `webhook.go`, `webhook_push.go`, `actions.go`, `status.go` |
| Infrastructure | `errors.go`, `crud.go`, `preload.go`, `embedding_hook.go` |

## Main Flows

### Execution Context Intake

```text
IntakeExecutionContext(ctx, source selectors + locator + current source token)
  → call the injected executioncontext puller
  → registry resolves exactly one stable source and performs one fixed-egress GET
  → closed adapter verifies running state and exact workspace/Agent/Task locator
  → source token leaves scope and is never passed to persistence
  → persist ags.execution-context-snapshot.v1 with canonical source ref/context/digest
  → return ags.execution-context-intake.v1 credential-free receipt
```

The intake route is bootstrap fact acquisition, not authorization. It does not
create a User, UserIdentity, repository grant, delegated Session, operation, or
provider effect. `GetExecutionContextSnapshot` is an internal readback seam for
a later AGS-owned canonical actor/access-grant evaluator. The source snapshot is
immutable evidence and cannot be reinterpreted as source-issued AGS authority.
See [execution-context intake](execution-context-intake.md).

### Canonical Actor and Access Grant

```text
IssueAccessGrant(ctx, fresh source input + exact repo + optional intent)
  → call IntakeExecutionContext and persist a credential-free snapshot
  → resolve/JIT UserIdentity(execution_context, source/Agent)
  → resolve one unambiguous active source/workspace default policy binding
  → accept elevation only when the requested class is bound to that actor
  → intersect class operations with exact resource/operation policy
  → persist immutable actor/executor/task/repository authority facts
  → return a maximum-30-minute bearer once; persist only its SHA-256 digest
```

Invalid optional Agent/class/operation requests degrade to the legal default
with stable receipt warnings and cannot self-grant. Renewal requires a fresh
matching source observation plus exact actor, executor, task, repository and
authority continuity; it creates a new bearer and atomically revokes the old
one. Use time reloads actor identity, executor, repository, source snapshot and
the full policy revision before appending an invocation receipt.

Generic authorization is receipt-only and does not make the grant bearer a
generic Git or GitHub-compatible API credential. Exact `pr.merge` uses a separate effect method: it verifies
AGS/provider PR coordinates, head/base, configured method, exact-head CI and
protected provider authority. Definitive exact-fact drift before dispatch is
persisted and returned as a GET-readable terminal `conflict` receipt with
`provider_attempt=not_attempted`; otherwise the service persists a globally
unique `planned` intent, then CASes to `dispatching + outcome_unknown` before
the only Forgejo POST. Every later duplicate/restart/GET path reads the exact
provider PR and cannot repeat the write, and locator collisions after that
boundary never use HTTP 409. See [Canonical Actor and Access Grants](access-grants.md).

### Access Grant Transport Session Derivation

```text
IssueAccessGrantTransportSession(ctx, grantBearer, operation)
  → authenticate and reload the exact active parent Grant
  → require one exact non-merge operation already present in effective_operations
  → revalidate immutable source snapshot, canonical actor, selected executor, repository,
    policy/class/resource revisions, and the executor's live native grant
  → persist a hash-only access_grant_transport Session bound to the Grant ID/revision,
    exact operation constraints, actor/executor split, and workload.context.v1 provenance
  → append a not_attempted/not_applicable invocation receipt
  → return the transport bearer once with Cache-Control: no-store
```

The transport Session adapts existing Git and GitHub-compatible API surfaces; it is not a second
authority object. It cannot carry `pr.merge`, outlive or widen its parent Grant,
or fall back to assertion exchange, a durable profile, provider credentials, or
another principal. Grant renewal/revoke invalidates all dependent Sessions.
Dynamic Session status/revoke routes are absent; only durable-site-admin exact-ID
historical lifecycle readback remains. On the REST adapter, `repo.read` additionally
admits only exact `GET /api/v3/repos/{owner}/{repo}/branches/{safe-branch}/protection`;
the existing safe head-ref validation accepts slash refs and rejects protection
subresources, list/wildcard variants, unsafe or missing refs, writes, provider-direct
routes, and unrelated operations before handler dispatch. For numbered `pr.read`,
the middleware also admits only one bounded `PullRequestByNumber` GraphQL query whose
owner/repository/number variables match the Session repository and exact PR constraint;
mutations, alternate query shapes, extra resources, and fact mismatch fail before resolver dispatch.

### Durable Operation Authorization

```text
AuthorizeDurableOperation(ctx, authenticatedPrincipal, service, repository, operation, constraints)
  → service boundary accepts only operation-allowlisted safe scalar constraint keys
  → load the exact enabled repository
  → use operationcatalog for AGS-owned operation/risk semantics and sessionauthority for the unique service/repository policy
  → reject absent/inactive/target-ambiguous resource authority; exact disabled repo override blocks default fallback
  → compute the authenticated principal's current native repository permission
  → require the operation's shared read/write/admin level
  → return exact normalized allowlisted constraints plus native-grant and real resource-policy revisions
```

This path does not resolve an assertion binding because token authentication has
already selected the immutable principal. It does not maintain a second
operation-to-permission switch and does not use credential mode as a synthetic
policy revision. REST only decodes the request. Before any receipt, audit, or
error serialization, the service boundary rejects unknown, authority-shaped, or
secret-shaped keys, malformed values, and non-scalars; the receipt preserves
only the operation-specific normalized map exactly.

### Access Grant Transport PR Creation

```text
CreatePRWithResult(ctx, input)
  → authenticate one pr.create access_grant_transport Session
  → reload and revalidate its exact parent Grant, operation constraints, executor native grant,
    repository, canonical actor, and immutable execution-context snapshot
  → require the exact base_ref/head_ref pair stored in the Session
  → transactionally create the PR and agent_session_id provenance
  → attribute the PR to the canonical actor while recording the executor separately
  → derive authoritative Multica Issue/Task/Run linkage and human initiator/originator only from
    the immutable parent snapshot
```

The independent External PR link-token flow remains a correlation path for
external-provider callbacks; it does not authorize this transport PR creation or
select actor, executor, policy, operation, Grant, or provider credentials.

Durable and Access Grant authority share the AGS-owned `operationcatalog`, which
covers standard and privileged operations including `repo.read`, `git.read`,
`git.push`, `pr.create`, `pr.rebase`, `pr.read`, `pr.merge`, `ci.read`,
`review.read`, `review.submit`, `repo.admin`, and `repo.create` with shared
read/write/admin risk requirements. Configuration may bind a principal to a
class but cannot add an unknown operation or lower its risk. One constraint kernel is
used by durable authorization, Access Grant transport Sessions, and use-time
facts. Repository/Git operations, create refs, PR selectors, rebase coordinates,
review reads, CI variants, and exact merge coordinates retain their closed
operation-specific shapes; malformed, mixed, extra, secret-shaped, or
noncanonical values fail closed.

Access Grant transport adapters cover ordinary Git and GitHub-compatible API operations. They accept
no workload assertion and production use-time evaluation rejects every other
Session credential mode. Transport issuance explicitly rejects `repo.create`,
`repo.admin`, and `review.submit`; these high-risk control-plane operations may
only receive exact invocation admission and require dedicated effect ownership.
The supported `ags-expert` import workflow reauthorizes pinned `repo.create` and
`repo.admin` immediately before separately receipted AGS/provider effects. Exact
`pr.merge` is a separate Access Grant effect
adapter: it binds the provider mapping, head/base/method, CI, protected
authority, and a global effect key; it persists `dispatching + outcome_unknown`
before the only provider POST, then permits only provider-read recovery. The
legacy assertion exchange and delegated merge gateway are absent. Durable-profile
PR creation follows the existing path and leaves `agent_session_id` null.

`PullRequestAttributionFor` resolves that immutable session snapshot into two
presentation facts: the dynamic `AGSActor` and the stable `DelegatedBy`
principal/human chain. REST, GraphQL, and Forgejo projection consume this same
service result while owning their wire/body formatting. The method never
returns credential, assertion, assertion JTI/correlation, verifier/fingerprint,
or policy-snapshot material.

### Human Single-Frontdoor Provider Merge

```text
MergeProviderPRAsHuman(ctx, currentHuman, agsRepo, agsPR, expectedHead, method)
  → reject non-Human identities and authorize the live AGS repository/PR
  → resolve the exact provider PR and executor from AGS projection/config state
  → require expected/current head, current base ancestry, allowed method,
    independent current-head approval/status, provider CI/protection and executor eligibility
  → send at most one provider merge POST through the deployment-owned executor
  → return operation state and projection convergence as separate fields
```

The request has no provider owner/repository/PR or provider credential field.
Before dispatch, every authority/fact drift produces zero provider writes. If a
confirmed provider merge is retried while the webhook has not yet converged the
AGS PR, service reads the exact mapped provider PR and returns
`projection.status=pending` without sending another POST. Access Grant
`pr.merge` uses its own deterministic invocation locator but shares the
independent current-head review/status policy.

### PR Merge

```
MergePR(ctx, repoFullName, prNumber, method, message)
  → authenticate current user
  → load PR, validate open + not already merged
  → enforce merge policy:
      → require repository write access
      → load branch protection for the base branch
      → enforce required approvals unless the actor is in `bypass_pull_request_allowances.users`
      → enforce required status-check contexts (strict mode is rejected)
  → MergePRRecord():
      → resolve merge method (merge / squash / rebase)
      → call Git.Merge, Git.SquashMerge, or Git.Rebase
      → on success: single GORM `Updates` call to set merged=true, state=closed, merged_commit_sha, and clear any queued auto-merge request
      → reload PR with full preloads
```

This is one of the highest-risk flows because it crosses DB state and real Git history.

### Auto-Merge Queue

```
SetPRAutoMerge(ctx, prID, input)
  → authenticate current user
  → require repository write access
  → require repo.AllowAutoMerge when enabling
  → optionally validate expectedHeadOid against the current PR head SHA
  → persist merge method plus optional commit metadata and expected head SHA

CreateCommitStatus / completeRun
  → reevaluate open PRs whose head SHA matches the updated check/status SHA
  → reuse the same merge policy as manual merges
  → merge with the queued actor identity when policy passes
```

Current contract:
- `required_pull_request_reviews.bypass_pull_request_allowances.users` is enforced in the merge policy.
- `teams` and `apps` bypass actors are not supported.
- `required_status_checks.strict` is rejected rather than being silently ignored.

### Repository Creation with Fork

```
ForkRepo(ctx, srcFullName, targetOwner)
  → resolve source repo
  → CreateRepo under target owner (DB record + Git.Init)
  → Git.Fork (cp -a source bare repo)
  → DB transaction: set fork=true, parent_id
  → on any failure: compensating cleanup (delete DB record + Git directory)
```

### Issue/PR Number Allocation

```
CreateIssue(ctx, repoFullName, fields)
  → retry loop (max 5 attempts):
      → DB transaction:
          → lockRepoForNumbering (SELECT ... FOR UPDATE on repo row)
          → nextIssueOrPRNumberTx (MAX(number) across issues and PRs + 1)
          → INSERT issue with allocated number
      → on duplicate key: sleep(retryDelay) and retry
  → reload with preloads
```

PRs use the same number sequence to match GitHub behavior.

### Timeline Synthesis

```
GetIssueTimeline(ctx, repoFullName, number)
  → fetch issue or PR
  → load IssueComments → wrap as TimelineEvent
  → if PR: load PullRequestReviews → wrap as TimelineEvent
  → sort all events by CreatedAt
  → return chronological timeline
```

## Invariants and Design Constraints

- **Service is the only layer that coordinates both relational and Git state.** Surfaces should call service methods, not orchestrate GORM + gitstore themselves.
- **Wiki path and backlink rules live in service.** `service/wiki.go` owns the single writable slug grammar, prefix-collision checks, atomic move preconditions, markdown-aware inbound-link rewrites during page moves, link parsing, and the wiki-HEAD-keyed in-memory backlink cache so REST stays transport-thin.
- **Wiki catalog freshness lives in service.** `service/wiki_migrate.go` decides when catalog-backed reads are stale, schedules at most one background migration replay per repository, and keeps read handlers non-blocking while the catalog catches up to git-backed wiki pushes or historical imports.
- **Wiki catalog CAS GC is automatic housekeeping.** The server runtime starts an hourly background worker that calls `wikicatalog.GCRun` with one-hour pending/refcount TTLs, removing legacy inline-body ref metadata and reclaiming orphaned pending blobs and zero-refcount CAS blobs without exposing a REST or CLI trigger.
- **Wiki labels live in service.** `service/wiki_label.go` attaches the existing repo-scoped `labels` catalog to git-backed wiki slugs through `wiki_page_labels`, validates that the target page exists, keeps label links in sync across wiki delete/move/prefix move operations, and exposes label-filter helpers for list/search. Label mutations persist their lexical projection task in the same database transaction because they change search content without changing the page revision.
- **Wiki writes preserve one linear Git commit per API mutation.** REST page mutations capture the catalog head and touched-page conflict state in one preflight snapshot, validate that snapshot, and compute the exact Git commit SHA in memory. Git object persistence then overlaps the catalog transaction; a catalog pre-commit barrier waits for object durability and rolls the SQL transaction back if persistence fails. The original catalog transaction stores that durable Git SHA, so successful ref publication needs no second catalog marker update. Only after both durable operations succeed does the service publish the branch with a parent CAS while holding the repository write lock. For a single-page upsert, the preflight snapshot also carries prefix-directory state and live outbound-link targets, so the transaction can skip known directory rows and avoid resolving the same links twice. A changed head fails with `ErrCASLost`, forcing Git preparation and catalog validation to restart from one parent. This preserves synchronous failure semantics, the single-page CRUD API, and one linear Git commit per mutation while avoiding serial Git/catalog latency.
- **Wiki writes fail closed until catalog and Git agree.** Under the shared catalog/Git lock, every REST mutation compares the durable catalog SHA with Git HEAD: a catalog-ahead prepared commit is republished, while Git-ahead commits left by direct push are synchronously ingested. A new mutation cannot advance either head while an older projection remains unresolved. Read-path freshness checks are non-destructive for catalog-originated state because a lagging or missing Git ref can represent interrupted publication; explicit Git ingest and receive-pack remain authoritative for force-push rewrites and content-branch deletion. Before Git HTTP receive-pack can mutate a wiki ref, the service claims a `wiki_git_repair_obligations` row with the pre-receive-pack Git snapshot, an owner token, and an owner expiration; this claim is insert-only so a concurrent receive-pack cannot overwrite another active owner after both instances have reconciled. The Git HTTP handler refreshes that owner expiration while the receive-pack critical section is active. Rejected pushes, no-op pushes, and successful synchronous ingest can clear only the row owned by that receive-pack token. A later serialized writer must consume any remaining row before the healthy REST fast path: an unexpired in-progress owner makes the writer fail closed, an expired unchanged pre-receive-pack snapshot is cleared as abandoned, and an expired changed snapshot is honored as authoritative receive-pack state before any REST recovery path can republish catalog-ahead content. The supported mutation boundary is the REST API or Git HTTP receive-pack; direct filesystem edits to a server-side bare repository are an internal-storage violation and are not interpreted by ordinary reads.
- **Wiki post-commit work is ordered without extending the Git lock.** Repository lookup data and changed bodies travel in the changeset result. Git ref publication remains inside the critical section; issue-reference synchronization runs through a per-tenant, per-repository FIFO after the lock is released, and the API still waits for it synchronously. The existing `wiki_repo_heads` CAS update also advances a durable, coalescing reference-recovery cursor when a changeset may add or remove wiki issue references; normal completion clears it conditionally, and a runtime recovery worker rebuilds current references for any repository left pending by a process interruption. Plain new pages without issue-reference syntax leave the cursor unchanged and add no transaction query. Search mutations persist a coalescing TiDB outbox task after Git publication, so catalog/Git writes never wait for embedding.
- **Wiki search lifecycle also lives in service.** `service/wiki_search.go` owns repo-scoped candidate indexes, lexical recall, label-name/description boosting, semantic ranking, stale-row filtering, live result hydration, and explicit reindexing. `service/wiki_search_projection.go` owns durable put/move/delete/label projection: repository-bound tasks coalesce by repository, slug, and kind; lexical projection always lands before a separate embedding task; generation checks reject stale completions; leases permit safe multi-instance recovery; and startup repair recreates tasks for missing, stale, or deleted documents.
- **Service owns collaboration policy.** Org membership, org invitations, outside-collaborator reconciliation, and effective repository permission resolution all live in `service`, not in REST or GraphQL handlers.
- **Sentinel errors for surface mapping.** `errors.go` defines `ErrNotFound`, `ErrConflict`, `ErrInvalidState`, `ErrValidation`, `ErrUnauthorized`, `ErrDuplicate`, `ErrInvalidRequest`, and `ErrAlreadyCollaborator`. REST maps these to HTTP status codes via `respond.ServiceError`; GraphQL maps them to error payloads.
- **`wrapErr` normalizes GORM errors.** GORM's `ErrRecordNotFound` is converted to `ErrNotFound` for consistent HTTP 404 mapping.
- **Retry with backoff for concurrent number allocation.** Issue and PR creation retry up to 5 times on duplicate key errors, with exponential backoff via `retryDelay`.
- **Preload chains for consistent association loading.** `preload.go` defines reusable GORM preload helpers (`preloadIssue`, `preloadPRFull`, etc.) to prevent N+1 queries and ensure surfaces receive fully-loaded objects.
- **The concrete service is the primary wiring seam.** Production code passes `*Service`; introduce narrower interfaces only for a specific, tested boundary.

For the full dependency-boundary rules see [module-contracts.md § service](../module-contracts.md#service).

## Extension and Change Guidance

**Adding a new domain operation:**

1. Add the method to `Service` in the appropriate domain file.
2. Use the active request-scoped DB accessor for all GORM queries to respect request cancellation and context-scoped DB overrides. In today's code that is `s.DBForCtx(ctx)`.
3. Return sentinel errors for failures that surfaces need to distinguish (e.g., `ErrNotFound` for missing entities).
4. If the operation touches Git state, coordinate through `s.Git` and handle the case where `s.Git` is nil.
5. If the operation creates sequentially-numbered entities, follow the retry + `lockRepoForNumbering` pattern from `issue.go`.

**Common patterns:**

- Public methods accept `repoFullName` (e.g., `"owner/repo"`); internal helpers resolve to numeric IDs for efficiency.
- Upsert operations use `isDuplicateErr` for idempotency.
- Compensating cleanup on multi-step failures (see `repo_fork.go`).
- SQL injection prevention via `escapeLike` for LIKE queries.

## Related Tests

Test files in `internal/service/`:

| File | Coverage |
|---|---|
| `repo_lifecycle_test.go`, `repo_test.go`, `repo_fork_test.go` | Repository CRUD, fork, transfer |
| `pr_test.go`, `pr_lifecycle_test.go`, `pr_merge_test.go` | PR creation, merging, state transitions |
| `issue_test.go` | Issue creation, update, list |
| `comment_test.go` | Comment operations |
| `review_test.go` | Review creation and submission |
| `label_test.go` | Label CRUD and issue attachment |
| `milestone_test.go` | Milestone numbering and CRUD |
| `team_test.go` | Team management |
| `auth_test.go` | Token validation and device code exchange |
| `gist_test.go` | Gist CRUD |
| `release_test.go` | Release operations |
| `workflow_test.go` | Workflow dispatch and runs |
| `search_test.go`, `search_db_test.go` | Search qualifier parsing and DB queries |
| `service_test.go` | General service helpers |
| `numbering_concurrency_test.go` | Concurrent number allocation |

For the phased test roadmap see [docs/test-strategy.md](../test-strategy.md).

## Related Docs

- [docs/architecture.md](../architecture.md) — system overview
- [docs/module-contracts.md](../module-contracts.md) § service — dependency rules and ownership
- [Git Store](gitstore.md) — bare-repository operations that service orchestrates
- [REST API](rest.md) — primary consumer of service methods
- [GraphQL API](graphql.md) — primary consumer of service methods
