# Git Smart HTTP — Component Reference

## Purpose

`internal/githttp` implements the Git Smart HTTP transport surface.
It authorizes primary Git requests through `service.AuthorizeGitTransport`, delegates CGI execution to the database-free `internal/gitbackend`, and performs post-push follow-up work (HEAD fixing, webhook dispatch, and workflow discovery). The independent Edge reuses gitbackend, not this primary handler.
Within the product, this is a supporting surface for GitHub-compatible repository workflows rather than a separate standalone Git-hosting focus.
Users normally encounter it only after starting with an API client or the REST discovery/auth endpoints such as `/api/v3/` and `/api/v3/meta`.
This document records the current implementation.

For the full system overview see [docs/architecture.md](../architecture.md).
Core invariant: `agent-git-service` is Git-backed; see [architecture.md § Purpose](../architecture.md#purpose).

## Scope

Owns:

- Git Smart HTTP protocol handling (info/refs, upload-pack, receive-pack)
- delegation to `git-http-backend` via CGI
- transport-level repository existence bootstrap
- post-push follow-up triggering (HEAD repair, webhook dispatch, workflow sync)

Does not own:

- business rules such as PR merge policy or workflow execution semantics (belongs to `service`)
- bare-repository operations (belongs to `gitstore`)
- API token validation and current-user resolution (belongs to `middleware` and `service`)

## Key Entry Points

| File | Responsibility |
|---|---|
| `handler.go` | Primary `InfoRefs`, `UploadPack`, `ReceivePack`, branch-policy adapter, synchronous follow-up and HEAD repair |
| `wiki_context.go` | Resolve synthetic Wiki storage through the parent's current native authorization, without widening main-repo delegated scope |
| `internal/gitbackend/backend.go` | Native CGI discovery/execution, bounded request spooling, credential stripping and explicit primary write opt-in |

The `Handler` struct holds:

| Field | Type | Role |
|---|---|---|
| `store` | `*gitstore.Store` | Check repository existence, get repo paths |
| `Svc` | `*service.Service` | `GetRepo` for DB lookup, webhook dispatch helpers, `SyncWorkflowsFromRepo` for post-push |
| `repoRoot` | `string` | Root directory for Git repositories |

## Main Flows

### Clone / Fetch

```
git clone https://host/{owner}/{repo}.git
  → credentials pass through route auth middleware (`OptionalTokenAuth`)
  → GET /{owner}/{repo}.git/info/refs?service=git-upload-pack
      → InfoRefs handler
      → resolve repo from DB (including historical names via `repo_redirects`)
      → require a resolved viewer with at least read access via `Svc.HasRepoAccess`
      → map the canonical repo full name to the on-disk bare repo path
      → ensure repo exists in gitstore (init from DB metadata if needed)
      → serve() → CGI to git-http-backend (advertise refs)
  → POST /{owner}/{repo}.git/git-upload-pack
      → UploadPack handler
      → require read access via `Svc.HasRepoAccess`
      → serve() → CGI to git-http-backend (send packfile)
```

### Push with Post-Push Follow-Up

```
git push origin main
  → credentials pass through route auth middleware and are normalized into the viewer context
  → POST /{owner}/{repo}.git/git-receive-pack
      → ReceivePack handler
      → require write access via `Svc.HasRepoAccess`
      → enforce `GITHTTP_MAX_PUSH_BYTES` limit (default `2 GiB`) for the incoming pack
      → acquire the gitstore repository write lock
      → snapshot refs and serve() → CGI to git-http-backend
          (receive objects and update refs)
      → release the repository write lock
      → on success, complete synchronous follow-up:
          → fixHEAD(repoPath)
              → git symbolic-ref HEAD → check target branch exists
              → if dangling: git for-each-ref → find first branch → git symbolic-ref HEAD <branch>
          → Svc.SyncWorkflowsFromRepo(ctx, fullName)
              → scan .github/workflows/ for workflow definitions
              → sync to DB (30-second context timeout)
```

For a synthetic `{repo}.wiki.git` backing repository, receive-pack additionally
holds the parent Wiki catalog serialization lock. Before CGI runs, service
reconciliation republishes any pending REST commit and ingests an older
unhandled Git head. The CGI response is buffered while the newly updated ref is
ingested synchronously; it is released only after catalog catch-up succeeds.
If ingest fails, the client receives an HTTP failure and the advanced Git head
remains detectable for the next locked reconciliation attempt.

### CGI Delegation (`serve`)

```
serve(w, r, repoPath, serviceName, advertise)
  → set env: GIT_PROJECT_ROOT, GIT_HTTP_EXPORT_ALL, PATH_INFO, REMOTE_USER
  → create net/http/cgi.Handler pointing to git-http-backend binary
  → delegate full HTTP request/response to CGI
```

`gitbackend` locates the native executable using `git --exec-path` and known platform paths. Its zero-value request cannot write. Only the primary receive-pack adapter enables writes; only synthetic Wiki writes permit current-branch deletion through a request-local configuration override.

## Current Auth Behavior

The current route is authenticated and permission-aware:

- Git Smart HTTP routes are registered separately from the REST API paths, but they are wrapped in auth middleware at route registration time
- middleware accepts `token`, `Bearer`, and Git-helper `Basic` auth forms and injects the resolved viewer into request context
- auth remains optional at the route layer so unauthenticated requests can still reach the handler for public-repo reads
- `resolveRepoContext` requires `git.push` for new push Sessions and admits them only when the requested Git service is `git-receive-pack`; upload-pack advertisement/execution, clone, and fetch are rejected before CGI delegation. It then calls `Svc.RequireRepoPermission` for exact repository/read-write transport facets and the immutable principal's current native grant; pre-v2 rows with no operation retain only TTL-bounded compatibility
- `InfoRefs` derives the required permission and operation envelope from the `service` query parameter, so push discovery is checked before `git-receive-pack` while upload-pack discovery remains outside a `git.push` Session
- after the request reaches `resolveRepoContext`, missing viewer context, missing repo access, and missing repos intentionally collapse to GitHub-style hidden/challenge responses; an authenticated delegated-session scope or capability mismatch returns `403`
- CGI still receives a fixed `REMOTE_USER=git`, but that value is not the security decision point; delegated receive-pack is marked only by server-owned CGI environment, never by a client header
- the requested public `owner/repo` path is first normalized through DB lookup; historical repo URLs can resolve to the canonical repo before the handler computes the on-disk path

## Invariants and Design Constraints

- **Transport auth happens before CGI delegation.** Git Smart HTTP depends on middleware auth, exact `git.push` Session operation, exact `git-receive-pack` service, and `service.RequireRepoPermission`; `REMOTE_USER` in the CGI environment is not a security boundary.
- **InfoRefs must enforce intent-specific permissions.** `info/refs` serves both clone/fetch and push discovery, so the handler must derive required access from the `service` query parameter before delegating to CGI.
- **Git pushes bypass the generic API body cap.** The router keeps the normal 50 MB request cap for REST/API traffic, but `git-receive-pack` enforces its own `GITHTTP_MAX_PUSH_BYTES` limit so large repository pushes can behave more like GitHub. This avoids the previous bug where pushes just over 50 MB were rejected by the generic API limit and, behind AWS ALB, could present to clients as an edge `504`.
- **Receive-pack shares gitstore's repository write lock.** Ref negotiation and update are serialized with service merges and prepared Wiki ref publication. This prevents a push from advancing a Wiki branch between catalog commit preparation and the final parent-CAS publication.
- **Wiki receive-pack acknowledges only after catalog ingest.** The handler buffers the successful CGI response for synthetic Wiki backing repos, runs synchronous ingest under the shared catalog/Git locks, and discards that response if ingest fails. The next Wiki push or REST mutation reconciles any Git head left by such a failure before accepting another write.
- **Dual dependency on gitstore and service.** The handler depends on both `*gitstore.Store` (for repo paths and existence checks) and `*service.Service` (for DB repo lookup, webhook dispatch, and workflow sync). This is an accepted coupling documented as visible technical debt.
- **Protected branches require the authoritative PR path.** AGS refreshes the server-owned pre-receive hook before Git service. Any Git HTTP credential, including a durable maintainer token, is rejected when `receive-pack` attempts to update a branch with an AGS `BranchProtection` row; service-owned PR merge/projection synchronization does not traverse Git HTTP and remains the controlled base-update path. Delegated requests add a stricter ceiling: they also treat the repository default branch as protected even without a row, may update branch refs only, and may not delete a branch or replace branch history non-fast-forward. Main-repository protection/delegation is not implicitly applied to synthetic Wiki storage.
- **Delegated operation audit follows the transport result.** Upload-pack attempts by a `git.push` Session and receive-pack capability/body/backend failures record a secret-safe denial with the normalized Git service; ref comparison under the repository lock distinguishes success, no change and unknown snapshot errors. Canonical attribution comes from the session row, never author strings or client identity headers. Audit failure never changes the Git response.
- **Post-push follow-up stays synchronous after the repository lock.** HEAD fixing, push webhook fan-out, pull request synchronize webhook fan-out, and workflow sync run after `git-receive-pack` releases the concrete repository lock but before the HTTP request returns. Failures are logged, and request-scoped DB, user and delegated-session context stay valid for the full follow-up window.
- **HEAD repair handles branch naming mismatches.** When a client pushes to a branch that differs from the repository's HEAD target (e.g., pushing to `master` when HEAD points to `main`), `fixHEAD` corrects the dangling reference to the first available branch.
- **Current canonical repo path = physical path.** The request URL still starts as `/{owner}/{repo}.git`, but the handler first resolves the repo through `service.GetRepo` (including redirects) and then maps the canonical full name to `GIT_REPO_DIR/{owner}/{repo}.git`.

For the full dependency-boundary rules see [module-contracts.md § githttp](../module-contracts.md#githttp).

## Extension and Change Guidance

**Adding post-push follow-up work:**

1. Add the follow-up logic as a service method (githttp should not own business rules).
2. Call the new service method from the post-lock follow-up block in `ReceivePack`, alongside the existing webhook dispatch and workflow sync follow-up.
3. Use a context with timeout for the follow-up work.

**Common patterns:**

- Repository path extraction starts from the public URL path `/{owner}/{repo}.git/...`, resolves that name through `service.GetRepo`, and then maps the canonical `owner/repo` to storage under `GIT_REPO_DIR`.
- CGI environment setup and executable discovery are centralized in `gitbackend.Serve`; the primary adapter supplies only already-authorized storage and branch policy.
- `AuthorizeGitTransport` does not initialize a repository; primary storage preparation and Edge missing-snapshot synchronization remain separate.

## Snapshot coordination and Edge boundary

Primary writes acquire any repository/catalog locks before a `BeginMutation` lease.
Native receive-pack runs inside that order, including Wiki repair and ingest.
Prepared Wiki object persistence and reference publication participate separately;
pure commit computation does not hold the write barrier. HEAD repair likewise
acquires a repository lock before the mutation lease. Capture never takes these
repository locks and releases its exclusive barrier before expensive local packing.

The Edge uses explicit `ReadPlan`/snapshot authorization and verified local views.
It cannot invoke primary repository initialization, callbacks or receive-pack.
User credentials are stripped before CGI; peer credentials never substitute for
end-user permission. See [AGS Edge](ags-edge.md) for sync and deployment gates.

## Related Tests

- `internal/githttp/handler_test.go` — integration tests covering clone, fetch, push, `git.push` receive-pack-only scope (including upload-pack/ls-remote/clone/fetch denial), durable-credential denial on AGS-protected branches, exact-repository/native-grant enforcement, successful/denied secret-safe Git audit, expired/revoked replay, protected/default branch and non-fast-forward denial, HEAD repair, post-push webhooks, and workflow detection
- `internal/gitstore/init_nonff_test.go` — pre-receive policy tests for durable custom-ref CAS and delegated branch ceilings

For the phased test roadmap see [docs/test-strategy.md](../test-strategy.md).

## Related Docs

- [docs/architecture.md](../architecture.md) — system overview, canonical Git push flow
- [docs/module-contracts.md](../module-contracts.md) § githttp — dependency rules, accepted couplings
- [Git Store](gitstore.md) — bare-repository operations used for transport
- [Service Layer](service.md) — webhook dispatch, workflow sync, and repo lookup called post-push
