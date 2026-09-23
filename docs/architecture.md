# Architecture

This document is the design baseline and single source of truth for `agent-git-service`.
Update it when behavior, package boundaries, or the local development workflow change.
Avoid putting transient status here such as exact passing test counts; the codebase and CI are the truth for fast-moving inventory.
This document records current implemented architecture and explicitly labelled source candidates. Canonical Actor and task/repository Access Grants are the AGS-owned profile-free authority surface; exact `pr.merge` is an Access Grant effect with one provider POST boundary and GET-only reconciliation. [Fork governance](../fork/README.md) owns this source repository's GitHub workflow, upstream generation and publication gates. Source acceptance is separate from deployed runtime acceptance.

## Purpose

`agent-git-service` is a self-hosted Git-backed server for standard GitHub-compatible API and Git transport workflows. The development binary is currently named `gh-server`.
The compatibility target is common GitHub API and Git behavior for automation clients, not strict endpoint-for-endpoint parity with GitHub.com.
It exposes four primary surfaces:

- GitHub REST API v3
- GitHub GraphQL API v4
- Git Smart HTTP
- OAuth device flow

It also exposes additive repo-specific endpoints such as OIDC-backed human-login
helpers under `/api/ext/v1/oidc/*` (with the fork's configurable `/api/v3` aliases), connected-login browser helpers under
`/auth/connected/*`, exact-ID historical Session audit and operation-boundary readback under
`/api/v3/agent-sessions/*`, AGS-token-free fixed-egress execution-context bootstrap at
`POST /api/v3/execution-context/intake`, canonical actor/task-repository grants under
`/api/v3/access-grants*`, append-only authority-boundary capture/readback
under `/api/v3/integrations/authority-boundary-receipts/*`, durable-profile operation authorization at
`POST /api/v3/operations/authorize`, authenticated exact PR-scoped provider projection/CI evidence under `/api/v3/repos/{owner}/{repo}/pulls/{number}/provider/*`, plus admin-only wiki maintenance endpoints such as
`/api/v3/admin/wiki/repos/{owner}/{repo}/repair-locks` for stale wiki ref-lock
recovery.

From a user-facing perspective, the main entry points are GitHub-compatible clients, including `gh` CLI, plus the REST discovery/auth endpoints `/api/v3/`, `/api/v3/meta`, and `/api/v3/rate_limit`. Git Smart HTTP is typically exercised after that setup path, when a Git client or credential helper crosses into clone, fetch, or push.

The key architectural invariant is that `agent-git-service` is Git-backed.
Repository content, history, refs, diffs, merges, rebases, and Git transport must preserve real Git semantics. Provider evidence endpoints never admit anonymous public-repo reads: delegated authority is revalidated again immediately before the server-owned provider request, exact provider PR numbers must match the AGS binding, and CI run pagination must complete or fail unavailable rather than report a partial empty result.

Authority is split by concern:

- Git is authoritative for Git-native repository state and behavior.
- The relational database is authoritative for higher-level metadata such as users, auth, issues, pull requests, reviews, labels, workflow records, and related product state.
- `service` coordinates flows that need both Git-backed and DB-backed state.

Current wiki contract:

- The sibling bare `*.wiki.git` repository is the durable authority for wiki page content, path layout, commit history, ref-pinned reads, rename semantics, and prefix moves.
- TiDB-backed wiki tables still serve some indexed metadata and current-page compatibility paths during the final cutover, but wiki lexical search now treats git as the primary authority and only falls back to the DB cache when git access is unavailable.
- Remaining wiki re-architecture work is tracked in [architecture/wiki-storage-v2.md](architecture/wiki-storage-v2.md) and the cutover runbook in [operations/wiki-storage-v2-cutover.md](operations/wiki-storage-v2-cutover.md), with the remaining goal of removing the last current-page and metadata transitional paths so every derived wiki index stays obviously rebuildable from git without reintroducing catalog-first writes.

This does not prohibit repository- or pull-request-related metadata in the database.
The rule is about authority: Git-native behavior stays Git-backed, while relational metadata stays DB-backed.

Deployment topology is intentionally not part of the architectural contract.
The system may run as a single local process for development or as stateless/distributed components with external services, as long as the Git-backed invariant and authority split above remain true.

The current reference setup stores relational state in one application database, normally TiDB through GORM, and Git objects in bare repositories under `GIT_REPO_DIR`. The fork retains explicit SQLite/PostgreSQL dialect selection for existing integrations; this is not a claim of identical search capabilities or full PostgreSQL runtime acceptance. TiDB remains the reference for multilingual full-text/vector search and the upstream test suite. A nonempty retired `CONTROL_PLANE_DSN` is rejected before database bootstrap.
The vendored `cli/` module is the gh CLI compatibility harness, not the product boundary.

## Repository Structure

| Path | Responsibility |
|---|---|
| `auth` | Public embedding identity types for external consumers |
| `cmd/gh-server` | CLI entrypoint, signal handling, `.env` loading, and logging init |
| `server` | Public startup/shutdown API, embeddable constructor/handlers, dependency wiring, TLS setup, and listeners |
| `config` | Environment-backed configuration exposed for external consumers |
| `internal/db` | GORM models, migrations, seed data, shared state constants |
| `internal/delegationpolicy` | Legacy delegation v1/v2 validation and non-executable migration inventory |
| `internal/sessionauthority` | Team-authority-v4 trusted issuers, immutable target-local issuer/workspace/team/class bindings, exact policy-class operations, target/service resource defaults, exact repository exception overrides, and legacy compatibility bindings |
| `internal/workloadidentity` | Secret-free `workload.context.v1` provenance shape retained by Access Grant transport Sessions |
| `internal/service` | Business logic over DB and Git storage (includes `Embedder` and `AllowAnyToken` fields) |
| `internal/tenant` | Legacy context marker used only to reject unsupported tenant contexts in fork authorization/replication; no DB or filesystem router |
| `internal/rest` | REST handlers plus response and transform helpers |
| `internal/graphql` | Query parsing, resolver dispatch, field filtering |
| `internal/gitstore` | Bare-repository operations on disk |
| `internal/githttp` | Primary Git authorization/storage/effect adapter using the shared native backend |
| `internal/gitbackend` | Already-authorized native Git CGI execution without business DB/bootstrap |
| `internal/edge` | Independent Edge foundations: fixed-primary forwarding and explicitly unready local-read boundary |
| `internal/edgeprotocol` | Credential-free read-plan/snapshot descriptor contracts, not permission minting |
| `internal/forgejointegration` | Optional post-push AGS -> Forgejo branch/PR mirror used to trigger Forgejo Actions while keeping AGS as agent-facing Git ingress |
| `internal/middleware` | Auth and request-size middleware |
| `internal/oauth` | OAuth device-flow endpoints |
| `internal/oidc` | Generic OIDC discovery, device flow, and ID token verification client |
| `internal/connectedlogin` | Configurable OAuth-style browser-login client for code exchange and userinfo |
| `internal/embedding` | Optional embedding-backed search support |
| `internal/executioncontext` | Fixed-egress connector registry, closed source adapter, and credential-free execution-context normalization |
| `internal/crypto` | NaCl-based encryption primitives for secrets |
| `internal/ratelimit` | GitHub-compatible rate-limit snapshot helpers |
| `internal/metrics` | Prometheus collectors and recorder facade |
| `internal/logging` | Structured logging and request-scoped log attributes |
| `internal/httputil` | Safe bounded helpers for outbound HTTP clients |
| `internal/randutil` | Shared random-hex generation utility |
| `internal/apperrors` | Sentinel application errors and error helpers |
| `internal/testharness` | Reusable real-router/service integration fixtures on isolated TiDB schemas; explicit SQLite fixtures separately cover fork runtime compatibility |
| `internal/router` | Chi router registration and middleware wiring |
| `cli/` | Vendored GitHub CLI plus compatibility tests |
| `docs/module-contracts.md` | Dependency boundaries, ownership, and coupling audit |
| `docs/architecture/execution-context-intake.md` | Connector resolution, source-credential boundary, immutable snapshot, and non-authority contract |
| `docs/architecture/access-grants.md` | Canonical actor, task/repository grant, invocation receipt, and exact effect boundary |
| `docs/test-strategy.md` | Testing roadmap and execution order |

## Startup and Runtime

The separate `cmd/ags-edge` executable does not call primary bootstrap or load
business metadata/workers. Stage B1 adds canonical manifests, self-contained
verified snapshots, registered-mTLS node export and a coalesced local Mirror.
Stage B2 adds persisted main-repository storage identities, a primary-owned
mutation/capture barrier and real original-user prepare/revalidate decisions.
`Server.NewReplicationEndpoint` constructs peer handlers inside the owning primary.
The optional `AGS_REPLICATION_CONFIG_FILE` now attaches a separate mTLS listener
there, with pre-provisioned identity checks, actual hardlink/headroom preflight,
all-or-none listener binding and handler-aware shutdown. Without explicit opt-in,
listeners are unchanged; peer routes are never added to the public API. Real DB,
mTLS, Access Grant/native-token and local Git integration tests cover this core.
Stage C1 wires packet-driven v0/v2 local reads through an explicit owner-only
AGS_EDGE_READ_CONFIG_FILE: normal canonical Git URLs drive fresh discovery,
bounded recent-view selection by want OIDs, synchronization, revalidation and
isolated local Git. Restricted unconfigured reads remain 503. The explicit
Operations component now drives readiness from live primary/peer/capacity checks
and expires stale results; without it the earlier operational gate remains.
Host-gateway mode routes unenrolled repositories to the primary before Git
negotiation, never as a response to failed authorization or corrupt cache.
Registered-node periodic warming requires no personal token. Separate loopback
diagnostics expose bounded request stages, transfer counters and certificate
expiry. Tests include real curloptResolve remotes and the standalone executable. E1 transfers exact missing objects and bounds published logical bytes,
view count and idle retention. E2a reserves private capture staging outside the
primary barrier, then observes refs and hardlinks immutable source objects under
it. Packing, verification and publication run after releasing the barrier. Both
primary retention and Edge imports reuse immutable pack/index files when every
base object remains reachable; shrinking object sets are rebuilt exactly. Views
own independent directory entries and retain no alternate-path dependency.
Same-filesystem capture, metadata/linking pauses, conservative per-view capacity
accounting, pack compaction and real operational rollout remain explicit gates.
E2b2 adds `AGS_REPLICATION_AUTHORITY_ID` and opt-in native-admin GET/POST
registration, enabling `ags-replication status/register` without a second local
DB owner. Exact prior row facts gate allocation; peer plans are certificate-
verified, unapplied fragments and cannot grant access themselves. Registration
and peer authority configuration must match. The read-only 2026-09-23 machine
audit found a writable Forgejo bind mount of one primary repository, which remains
excluded from initial replica canary approval.
See [AGS Edge](architecture/ags-edge.md) for exact implementation boundaries.

`cmd/gh-server` is the binary entrypoint and `server` is the composition root. External embedders can either keep using `server.Run` or construct a reusable instance with `server.New(config.Config, ...)`, mount `Handler()`, and manage listeners through `Start()` / `Shutdown(ctx)`. Embedded hosts may install `server.WithAuthenticator(...)` to inject a trusted request identity without minting AGS tokens first; AGS then owns the full identity-to-user mapping internally. The shared identity shape is exported from the top-level `auth` package. When that hook is absent, the single-database native/delegated token flow applies. The old protocol-specific embedding accessors and control-plane runtime are no longer part of this candidate.

The embedded-auth contract is:

- The host authenticator returns a trusted `auth.Identity` with non-empty `Provider`, `Subject`, and `Login`; `Name`, `Email`, `Groups`, and `SiteAdmin` are optional metadata that AGS will persist onto its internal user record.
- When the authenticator returns `ok=false`, AGS falls back to its historical token flow exactly as before.
- When the authenticator returns `ok=true`, embedded identity takes precedence over any `Authorization` header on the request. REST, GraphQL, Git Smart HTTP, OAuth device approval, discovery routes such as `/api/v3/rate_limit`, and optional-auth REST lookups such as `/api/v3/users/{username}/starred` all consume the same embedded-aware middleware path in single-DB mode.
- Retired control-plane configuration is rejected, including in embedded hosts. `WithAuthenticator` is not a tenant-routing or replication-node credential override.

A minimal host implementation looks like:

```go
import (
    "github.com/ngaut/agent-git-service/auth"
    "github.com/ngaut/agent-git-service/server"
)

srv, err := server.New(cfg, server.WithAuthenticator(myAuthenticator{}))
```

where `myAuthenticator.Authenticate(*http.Request)` returns a stable upstream subject such as:

```go
auth.Identity{
    Provider: "meshx",
    Subject:  "user-123",
    Login:    "alice",
    Name:     "Alice",
    Email:    "alice@example.com",
}
```

The startup sequence is:

1. Load `.env` for local development via `godotenv`.
2. Initialize structured logging via `internal/logging`.
3. Load typed configuration via `config.New()` from environment variables.
4. Initialize the main application database, run migrations, and seed default records.
5. Initialize embeddings if `EMBEDDING_API_KEY` is present.
6. Initialize the single-authority Git store rooted at `GIT_REPO_DIR`; no default-tenant fallback exists.
7. Build the shared `service.Service`, wiring DB, Git store, base URL, embeddings, generic OIDC, optional connected login, the fixed-egress execution-context registry, principal/session authority, legacy migration inventory, and local-dev auth conveniences.
8. Start explicitly configured integration and Wiki maintenance workers; validate the optional owning-process replication endpoint.
9. Initialize REST transforms, GraphQL server, REST deps, Git HTTP handler, OAuth handler, metrics, and readiness endpoints.
10. Register routes and start listeners.

### Listeners

Production mode starts one HTTP listener at `PORT`. Development mode starts the following listeners from the same handler tree; an explicitly configured replication listener is separate:

| Address | Protocol | Notes |
|---|---|---|
| `:443` | HTTPS | Conventional HTTPS endpoint |
| `:$PORT` | HTTPS | Configurable primary development port |
| `:8081` | HTTPS | Non-privileged HTTPS alternative |
| `:80` | HTTP | Conventional HTTP endpoint |
| `:4003` | HTTP | Non-privileged HTTP alternative |

TLS uses `cert.pem` and `key.pem`.
Shutdown is graceful with a 10-second timeout.

## Routing Model

Route wiring lives in `internal/router/router.go`.
That file is the executable truth for concrete endpoints.
This document records the stable structure around those routes.
GitHub-compatible REST uses `/api/v3`; upstream helper APIs use `/api/ext/v1`. The normal fork server also registers supported legacy helper aliases when `AGS_LEGACY_EXTENSION_ALIASES` is unset/true. Setting it false disables those aliases. Both names use the same handler and authorization, without redirecting or replaying a mutation. Fork-specific Access Grant/provider/replication routes retain their documented v3 names. The extension OpenAPI document is at `/api/ext/v1/openapi.json`.

### Request Families

- OAuth, OIDC helper, and connected-login endpoints are unauthenticated but service-backed.
- `POST /api/v3/execution-context/intake` accepts one request-scoped source token only to pull server-bound facts through fixed egress. The persisted snapshot is credential-free and grants no AGS authority.
- In single-DB mode, `/api/v3/access-grants*` is the profile-free authority family. Issue/renew observe fresh source context; authorize, transport, exact effect, and readback use only the Grant bearer and return `no-store`.
- `ags_sess_*` credentials are accepted only when the persisted Session has `credential_mode=access_grant_transport` and an exact live parent Grant. Legacy assertion-backed or pre-contract Session modes fail closed at use time.
- Git Smart HTTP is routed separately from REST/GraphQL but uses the same authentication middleware.
- Discovery endpoints use optional authentication; the authenticated API contains REST and GraphQL collaboration/governance surfaces.
- Assertion exchange, public dynamic Session status/revoke, delegation-policy inventory, and the delegated merge gateway are retired and return `404`.
- Unknown `/api/*` paths return GitHub-style JSON `404` responses.

### Discovery Endpoints

- `GET /api/v3/` advertises core REST URLs and password-auth capability to clients.
- `GET /api/v3/meta` is part of the `gh auth setup-git` and Git credential capability check path.
- `GET /api/v3/rate_limit` is a GitHub-compatible probe used by clients and smoke tests.

### Collaboration and Governance Endpoints

- Organization creation, membership, invitation, team, and outside-collaborator routes use explicit relational authority.
- `GET /api/v3/integrations/principal-sessions` and the monotonic epoch-floor route remain operator verification/governance surfaces; they do not mint Sessions.
- Authority-boundary receipts remain append-only, secret-safe local admission evidence. Delegated-effect receipts are scoped to the originating Access Grant transport Session or a site administrator and never prove provider success.
- `/api/v3/access-grants*` owns profile-free Runtime authority. It separates canonical actor attribution from executor authentication, revalidates source/repository/native-grant/policy facts at use time, and revokes dependent transport Sessions on renewal/revoke.
- Exact Access Grant `pr.merge` binds a caller-owned canonical invocation ID to AGS PR, provider PR, expected head, the live authoritative PR base, configured method, CI, and provider authority；non-default bases must be mirror-allowed and keep server-executor repository collaborator authority, while the configured default base retains full protection requirements. The PR creation-time `base_sha` is historical diff metadata. Merge admission requires the exact head to contain the current AGS base `A` and the mapped Forgejo PR base to equal the live Forgejo target ref `F`; `A` and `F` are not cross-compared. The supported CLI deterministically derives and GETs that locator before any effect POST；a matching receipt causes zero POSTs, only exact `403 grant_denied` permits at most one POST, and uncertain GET fails closed. Definitive pre-dispatch fact drift becomes a GET-readable terminal `provider_attempt=not_attempted` receipt; otherwise a global effect key and durable pre-provider `dispatching/outcome_unknown` boundary permit at most one provider merge POST. Once an effect may have dispatched, locator collisions use 422/403 rather than 409; lost responses/client crashes recover by re-deriving and GETting the locator, and every duplicate/recovery path is provider-read-only. Repo-local `fast_forward_ack` may complete AGS facts when `F` already equals the expected head without a provider merge POST and without closing the still-open Forgejo PR projection.
- `POST /api/v3/operations/authorize` remains the durable-principal path through the same normalized operation registry and native repository authority.
- Dynamic Session lifecycle status/revoke/replay routes are absent. Durable-site-admin exact-ID lifecycle readback remains for historical audit. The separate operation-specific `current/authority-boundary-receipts/{id}` read proves only a `pr.rebase` admission boundary for the originating transport Session; it is not Session lifecycle or authority renewal. Production use-time authorization rejects every credential mode except `access_grant_transport`.
- PR/Git/provider adapters revalidate the exact transport operation and constraints immediately before each read or effect. Access Grant PR creation derives Multica linkage and actor/initiator/originator facts from the immutable Grant snapshot; it never requests or accepts a workload assertion fallback.
- These retired paths are intentionally absent: `POST /api/v3/agent-sessions/exchange`, `GET /api/v3/agent-sessions/current`, both public Session revoke forms, `GET /api/v3/integrations/delegation-policies`, and `.../actions/pr.merge[/{intent_id}]`.

### Host Rewrite

Requests sent to `api.github.localhost` are rewritten before routing:

- `/graphql` becomes `/api/graphql`
- any non-`/api/*` path becomes `/api/v3/*`

This keeps the local server compatible with the way `gh`, `go-gh`, and other GitHub-style clients construct API URLs for enterprise hosts.

## Layer Boundaries

| Layer | Responsibility | Notes |
|---|---|---|
| `router` | Endpoint registration and host rewrite | Depends on REST, GraphQL, Git HTTP, OAuth, and middleware |
| `middleware` | Auth extraction and request guards | Uses concrete `*service.Service` with optional trusted embedded identity; no control-plane token resolver |
| `rest` | Decode request, call service, encode REST response | Handlers are expected to avoid direct DB access |
| `rest/respond` | Status code and JSON helpers | REST-only concern |
| `rest/transform` | Convert DB models into GitHub REST shapes | REST-only concern |
| `graphql` | Parse query, route resolvers, filter fields | Uses service methods directly |
| `service` | Business rules and orchestration | Owns DB and GitStore interaction |
| `db` | Relational schema and persistence | Auto-migrated on startup |
| `gitstore` | Bare-repository operations and Git command execution | Handles repo-level locking for writes |

### Important Current Constraint

The current service layer is wired as a concrete `*service.Service`; there is no generated or authoritative service-interface catalog.
That matters for future refactors and for test design: today the shortest reliable path is real-service integration testing, not handler-level mocks.

## Persistence Model

Persistence is split by authority rather than by deployment topology:

- one application `db` is authoritative for higher-level relational metadata and workflow/product records, including immutable credential-free `execution_context_snapshots`, canonical actor `UserIdentity` mappings, immutable task/repository `access_grants`, append-only `access_grant_invocations`, `delegated_agent_sessions` principal/team/class/epoch/resource/operation authority snapshots, `team_authority_epochs` monotonic floor/high-water revocation state, `principal_binding_revocations` append-only legacy tombstones, append-only `authority_boundary_receipts`, `workload.context.v1`, and expiry/revoke facts.
- `gitstore` is authoritative for Git-native repository state and operations.
- `service` coordinates flows that need both stores.

### Relational State

All GORM models live in `internal/db/models_*.go`.
The main data groups are:

| Area | Main Models |
|---|---|
| Auth and identity | `User` (including organization accounts and `DefaultRepositoryPermission`), `Token`, `DeviceCode`, `SSHKey`, `SSHSigningKey`, `GPGKey` |
| Execution context | `ExecutionContextSnapshot` (immutable canonical source ref/context/digest; no source bearer, authenticating hash, runtime alias, or egress endpoint) |
| Canonical runtime authority | `User` + `UserIdentity(provider=execution_context)` actor mapping, `AccessGrant` immutable task/repository authority snapshot with hash-only bearer, `AccessGrantInvocation` append-only authorization/provider-effect receipt and recovery locator, and derived `DelegatedAgentSession(credential_mode=access_grant_transport)` adapter rows invalidated with the parent Grant |
| Organization governance | `OrganizationMember`, `OrganizationInvitation`, `OutsideCollaborator` |
| Repositories and collaboration | `Repository`, `Collaborator`, `RepositoryInvitation`, `DeployKey`, `Ruleset`, `BranchProtection`, `Webhook`, `HookDelivery`, `Autolink`, `Star` |
| Issues and pull requests | `Issue`, `PullRequest` (including nullable delegated `AgentSessionID` provenance), `DelegatedAgentSession` (including principal/binding/resource/operation, `workload.context.v1`, trace quality, and lifecycle snapshots), `PrincipalBindingRevocation` (append-only revoked binding-ID tombstones), `PullRequestMulticaLink` (including assertion metadata/JTI), `IssueComment`, `Milestone`, `ReviewRequest`, `PullRequestReview`, `PRReviewComment`, `LinkedBranch`, `Reaction` |
| Projection and outbound | `PullRequestProjection`, `PullRequestProjectionJob`, `ProjectionEvent`, `ProjectionRefState` (including active generation), `OutboundDelivery` |
| Releases | `Release`, `ReleaseAsset` |
| Actions and workflows | `Workflow`, `WorkflowRun`, `WorkflowRunJob`, `Artifact`, `ActionCache`, `Secret`, `Variable` |
| Projects and teams | `Project`, `ProjectField`, `ProjectItem`, `ProjectRepoLink`, `Team`, `TeamMember`, `TeamRepository` |
| Other user content | `Gist`, `DependabotAlert`, `Deployment`, `DeploymentStatus`, `CommitStatus` |

### Durable Multica Terminal Projection

A verified external PR terminal fact is recorded by AGS and its typed Multica
handoff in one database transaction. The handoff is an `OutboundDelivery` row
with `event_type=multica_external_pr_terminal` and
`target_type=multica_external_pr`; it is not a Feishu notification and does not
accept a caller-selected URL. Its private durable payload carries a
secret-free `ExternalPRLinkRequest` plus typed target metadata, while the HTTP
body is exactly the existing closed request type. The canonical link identity
is `provider=ags` plus the AGS repository/PR; Forgejo facts remain in
`merge_provider`, `merge_repo`, `merge_number`, `merge_url`, `merged_sha`, and
the complete projection-facts group. Merged rows post only to the fixed
`/api/integrations/external-pr/complete-from-merge` path; closed-unmerged rows
post only to `/api/integrations/external-pr/links`.

The worker claims rows with a database-clock lease and CAS, retries only the
Multica projection handoff, reclaims expired leases only for this explicitly
idempotent typed target, and records bounded terminal failure as `dead_letter`.
The service token is read only from its owner-only secret file and exists only
in process memory and the Authorization header. A replay of the same row uses
the same idempotency key and must not be interpreted as a provider effect
retry or as Issue `done` evidence. AGS `pr.merge` keeps its independent
one-POST/GET-only effect ledger and all existing exact authority/base/head/CI
gates.

### Single Database and Explicit Context Overrides

The upstream control-plane implementation has been retired. `CONTROL_PLANE_DSN`
is retained solely as a startup rejection guard, never as an ignored setting.
`service.ContextWithDB` and `DBForCtx` remain for transaction/test-scoped overrides;
background work must preserve the relevant context. They do not choose tenants.
The small legacy tenant marker is only used to reject unsupported authorization
and replication contexts. A future multi-database product requires a separate,
explicit contract rather than reintroducing implicit root-database fallback.

### Current Collaboration Model

- Organizations are explicit `User{Type="Organization"}` accounts. `service.CreateOrg` is the current product entry point and records the creator as an org owner; `EnsureOrg` remains as a legacy/test helper.
- Organization membership is independent from team membership. `OrganizationMember` controls org-level membership and owner/member role, while `TeamMember` controls per-team member/maintainer role.
- Teams remain authorization-group objects. The legacy `privacy` field is retained for REST compatibility, but the service persists and serializes teams as `closed`.
- Organization invitations store GitHub-compatible invitation roles (`direct_member`, `admin`) plus invited team IDs. Accepting an invitation maps the invitation role to org membership (`direct_member` -> `member`, `admin` -> `owner`), joins the invited teams, and removes the pending invitation row.
- Outside collaborators are tracked explicitly for org-owned repos. The row is reconciled from direct repo collaborator state and removed when the user becomes an org member or loses all direct repo access in that org.
- Effective repository permission for REST, GraphQL, and viewer-repo listing is `max(org default permission, direct collaborator grant, team grant)` over the minimal runtime set `read`, `write`, `admin`. GitHub-style `triage` and `maintain` remain accepted as compatibility aliases for `read` and `write`.
- REST and Git Smart HTTP use `service.RequireRepoPermission`/`service.RequireRepoCapability` as shared effective authorization entries. Access Grant issuance resolves one canonical actor from trusted execution-context source facts, keeps actor and server policy executor separate, and intersects default plus actor-bound elevated classes with the selected executor's live native repository grant and exact resource/operation policy. Invalid optional selectors/classes/operations degrade with warnings; actor/executor/repository/snapshot/policy drift fails at use time. Generic authorization appends an invocation receipt; the internal transport adapter derives an exact short-lived Session whose executor authorizes existing Git and GitHub-compatible API surfaces while the canonical actor owns PR authorship and snapshot attribution. The adapter binds the parent Grant ID/revision, emits the canonical `urn:multica:agent:<id>` Context Envelope subject, and rechecks actor, executor, source coordinates, policy, repository, operation constraints, and native grant at use time. Grant renewal/revoke invalidates dependent Sessions; no public Session lifecycle route can extend or replace that authority. The exact merge adapter persists `dispatching + outcome_unknown` before the only provider POST and makes all later recovery GET-only; the Grant bearer itself is never generic Git or GitHub-compatible API authentication. Durable authorization starts from the already authenticated principal but reuses the same resource/operation evaluator and native-grant revision logic. Workspace display value, Agent, role, Issue, Task, Run, Trigger, Runtime, display name, and credential mode are provenance only; legacy delegation records remain non-executable migration inventory.
- Git HTTP receive-pack uses a server-owned pre-receive environment and hook. Any credential is denied when it attempts to update an AGS-protected branch, so the PR/merge path remains authoritative. A `git.push` Session is admitted only to `git-receive-pack` discovery/execution and is additionally scope-down constrained to branch create/advance: upload-pack, clone/fetch, branch deletion, direct default-branch update, and non-fast-forward replacement are rejected.
- Delegated PR presentation separates the stable authorization principal from the dynamic workload actor. REST/GraphQL and Forgejo body projection share `service.PullRequestAttributionFor`; identity fields are historical snapshots, session state is live, and authentication material is excluded.

### Git State

`internal/gitstore` manages bare repositories on disk.

Single-DB layout:

```text
GIT_REPO_DIR/{owner}/{repo}.git
```

There is no tenant-prefix layout in this runtime. One primary owns this Git root;
an Edge owns a distinct verified snapshot root and never becomes a second writer.

The package mixes:

- go-git for some native reference operations
- `git` CLI commands for merge, rebase, diff, archive, log, and content operations

Write-sensitive operations use a per-repository mutex.

### Other Stored Data

- Release asset binaries are stored in the database.
- Actions, Dependabot, and Codespaces secrets use the NaCl-based encryption primitives in `internal/crypto`.
- Semantic search data is optional and only activated when embeddings are configured and the active database supports the required vector-distance search capability.

## Authentication Model

### Token Auth

`internal/middleware/auth.go` implements two modes:

- `TokenAuth` for authenticated REST and GraphQL endpoints
- `OptionalTokenAuth` for discovery endpoints

Token validation goes through the single-database service layer. Native tokens,
Access Grant transport Sessions and optional trusted embedded identity keep their
separate use-time checks. The empty-token-table convenience is available only
when `AllowAnyToken` is explicitly enabled; it is not production authentication.

### Git Smart HTTP Auth

Git clone, fetch, and push are routed outside the REST/GraphQL API tree but still
pass through the standard auth middleware on their route group.
The shared auth extraction path accepts GitHub-style `token ...`,
`Bearer ...`, and HTTPS Basic auth (`username:token`).

Git routes preserve optional-auth behavior for genuinely public-repo reads;
private reads and all writes still require effective native authorization.
After auth, `githttp` enforces effective repo read/write permission through
`service.RequireRepoPermission(...)` before delegating to `git-http-backend`.
For delegated sessions that function requires the exact bound repository and
`repo:read` or `repo:write`, then reloads the Session and current principal grant.
Upload/receive CGI execution repeats that fresh check immediately before the Git
backend can read or update refs.

The handler still sets `REMOTE_USER=git` for CGI compatibility, but that value is
not treated as the authorization decision. A server-owned delegated marker and
protected-ref list carry the already-authorized request into the pre-receive
branch safety hook; no client-provided identity or author string controls it.

Delegated authorization remains single-database. Legacy tenant contexts are rejected; restoring multi-tenant routing requires a separately reviewed implementation.

### OIDC and Connected Login

When generic OIDC is configured, REST exposes these unauthenticated helper endpoints:

- `POST /api/v3/oidc/device/code`
- `POST /api/v3/oidc/session`
- `POST /api/v3/oidc/callback`
- `POST /api/v3/oidc/lookup`

These endpoints stay transport-thin: `internal/oidc` owns discovery, optional
device-authorization exchange, and ID token verification, while `service` owns
mapping verified external identities onto local application users and tokens.

When connected login is configured, REST also exposes:

- `GET /auth/connected/login`
- `GET /auth/connected/callback`

Some providers expose OAuth-like code exchange and userinfo endpoints without a
standard OIDC discovery document. `internal/connectedlogin` owns the configurable
browser login URL, token exchange path, userinfo path, and claim extraction.
`service` maps verified external userinfo into the same local identity/session
path as OIDC using the configured provider name and subject
`<subject_namespace>:<sub>` when a namespace claim is configured, otherwise
`<sub>`. The configured human and agent actor-type values map to human and agent
users. The callback URL is derived from `BASE_URL`, so there is no separate
`APP_ORIGIN` setting. On success, the browser callback mints a short-lived
one-time AGS authorization code plus a PKCE verifier. AGS stores the verifier in
an AGS-scoped `HttpOnly` cookie on `/login/oauth/access_token` and then redirects
the browser to `CONSOLE_BASE_URL` with the code plus non-secret identity metadata
in the query string. The console completes sign-in
by exchanging the code through the existing `/login/oauth/access_token` path
with browser credentials included, so a copied redirect URL is not sufficient
to mint a durable AGS bearer token. Callback JSON and console redirect metadata
always include `subject_namespace` when present, and also include the configured
namespace claim name as an alias when it is safe to expose, so providers can keep
their existing public metadata shape without code-level special cases.

The connected-login browser workflow is:

1. The browser requests `GET /auth/connected/login`.
2. AGS generates a CSRF `state`, stores it in the `connected_login_state`
   `HttpOnly` cookie, and redirects to
   `{CONNECTED_LOGIN_ORIGIN}{CONNECTED_LOGIN_LOGIN_PATH}` with `client_id`,
   the configured callback parameter (default `return_to`), and `state`.
3. The provider redirects back to
   `{BASE_URL}/auth/connected/callback?code=...&state=...`.
4. AGS validates `state`, exchanges `code` at
   `{CONNECTED_LOGIN_API_ORIGIN}{CONNECTED_LOGIN_TOKEN_PATH}` using client
   credentials, and receives an `access_token`.
5. AGS calls `{CONNECTED_LOGIN_API_ORIGIN}{CONNECTED_LOGIN_USERINFO_PATH}` with
   `Authorization: Bearer <access_token>`.
6. `internal/connectedlogin` extracts configured userinfo claims into subject,
   display-name, avatar, and actor-kind fields.
7. `service` converts the result into an `OIDCProfile` and reuses
   `oidcLoginWithProfile(...)` for local `UserIdentity`, `User`, and AGS token
   creation.

`CONNECTED_LOGIN_PROVIDER` controls the local identity provider key. The subject
is the configured subject claim (default `sub`), optionally prefixed with the
configured subject namespace claim. When a namespace claim is configured, the
provider must return it. The actor type claim (default `type`) is mapped by
`CONNECTED_LOGIN_HUMAN_TYPE_VALUE` and
`CONNECTED_LOGIN_AGENT_TYPE_VALUE` into local human and agent user kinds.

### OAuth Device Flow (Secured)

`internal/oauth` implements:

- `POST /login/device/code` — request device code (unauthenticated)
- `POST /login/device` — approve device code (requires authentication)
- `POST /login/oauth/access_token` — exchange device code for access token (unauthenticated)
- `GET /login/oauth/authorize` — authorization code flow with PKCE (requires state + PKCE S256)

Device codes require explicit authenticated user approval before token exchange succeeds. The `/login/device` endpoint is protected by `TokenAuth` middleware and rejects unauthenticated requests. The `/login/oauth/authorize` endpoint requires `state` and PKCE `code_challenge` parameters to prevent CSRF and authorization code interception attacks. Redirect validation is limited to same-origin or localhost targets.

## Canonical Request Flows

### Execution Context Intake

```text
Runtime locator + current task-scoped source token
  -> public AGS bootstrap route (strict closed body)
  -> executioncontext registry resolves exactly one stable source
  -> fixed configured egress GET; redirect disabled
  -> source validates current running task token
  -> closed provider-neutral source adapter + exact locator match
  -> service persists immutable source ref / canonical context / digest
  -> credential-free snapshot receipt (no AGS authority yet)
```

The canonical-actor/Access Grant flow consumes the snapshot ID. Intake itself cannot use runtime identity or operation-request environment variables to expand authority. Source or AGS failure affects only the AGS capability path; it does not cancel the originating Runtime task.

### Canonical Actor and Access Grant

```text
fresh source observation + exact AGS repo + optional identity/class/ops intent
  -> JIT resolve stable execution_context source/Agent UserIdentity
  -> select one trusted source/workspace default policy binding
  -> accept elevated class only when bound to that canonical actor
  -> intersect exact resource and implemented operation policy
  -> persist immutable actor/executor/task/repo authority + bearer digest
  -> low-risk invocation receipt, exact internal Git / GitHub-compatible API transport Session,
     or exact server-owned effect adapter
```

The issue route needs no durable AGS profile. Invalid optional intent falls back
to the legal default with warnings; it never widens authority. Use time reloads
actor, executor, source snapshot, repository and policy revisions. Ordinary
Git and GitHub-compatible API transport derives from this Grant without a Multica assertion and
keeps executor authorization separate from canonical actor attribution. Exact merge
persists an unknown-outcome dispatch boundary before one provider write, and
all later recovery is provider GET only. See [Canonical Actor and Access
Grants](architecture/access-grants.md).

### REST Request

```text
client
  -> router
  -> auth middleware
  -> service.ValidateAndResolveTokenDetailed(...) in the application database
  -> REST handler
  -> service
  -> service.DBForCtx(ctx) / gitstore
  -> transform/respond
```

Handlers are expected to stay thin:

1. parse params and request body
2. call service methods
3. transform model data into GitHub JSON shape
4. write HTTP response

### GraphQL Request

```text
client -> router -> auth middleware -> GraphQL handler -> parse query -> resolver -> service -> DBForCtx(ctx)/gitstore -> field filter -> response
```

GraphQL builds response objects directly and then prunes them against the requested selection set.

### Git Push

```text
git client -> router -> auth middleware -> githttp -> git-http-backend -> repository update -> post-push housekeeping
```

After push, the server performs follow-up work such as fixing `HEAD`, dispatching repository and pull-request webhook events, optionally mirroring eligible branch updates to Forgejo and GitLab, and syncing workflow definitions from the repository. Forgejo and GitLab post-push integrations are best-effort projection work: failures are logged and recorded as projection state, but they must not reject the original Git push. GitLab push projection is same-ref/same-SHA mirroring for configured repositories and branch policies; it does not make GitLab a merge authority. When Forgejo is configured as merge authority, the public Forgejo webhook endpoint accepts signed `pull_request closed+merged` callbacks. AGS PR creation first records the durable AGS-owned PR and then enqueues Forgejo/GitLab projection work. Projection mappings appear only after provider convergence; missing eligibility, transport failure, or delayed association remains observable and retryable without rejecting the PR creation response. A later Forgejo merge callback still requires an exact durable mapping before it may resolve back to AGS or advance the base. The callback reads and fetches the exact merged base object, rejects deletion or a non-fast-forward move relative to AGS, advances AGS base with compare-and-swap, marks the AGS PR merged/closed, comments and closes the GitLab shadow MR, and pushes the same Forgejo-authoritative SHA to the mapped GitLab backup branch. Signed Forgejo PR action-label callbacks are delivery adapters into one AGS-owned idempotent `pr.rebase` kernel. Automation first creates an exact intent through the shared durable/delegated policy evaluator. A human may instead add `ags/action-rebase`; the signed-webhook adapter may create the same intent only after resolving an explicit Forgejo actor → immutable numeric AGS principal binding, checking live Forgejo write permission, and passing the shared durable evaluator against current projection/head/base/full-label facts. The intent binds request source/actor/binding content revision, principal/team/class/epoch revisions, mapping, exact head/base and complete label transition before any rebase effect. The webhook consumes only one matching unexpired intent under its per-PR lease; duplicate delivery reads the same intent without repeating effects. Unmapped, unauthorized, expired, ambiguous, or fact-drifted labels are quarantined with no rebase and an idempotent blocked status; a stale delivery whose action label is already absent is ignored. The kernel then requires an exact consistent AGS/Forgejo head-and-base preflight, rebases the authoritative AGS PR head, and updates only the mapped non-base work branch with force-with-lease bound to the accepted old Forgejo SHA. The integration independently verifies the remote branch and exact mapped Forgejo PR head before the projection row advances. The service then re-reads the AGS branch, AGS PR, Forgejo branch, Forgejo PR, base heads, and projection row; only exact convergence enters the single success-comment branch and resolves active drift. A required Forgejo failure atomically becomes an append-only projection event, an active ref-state generation, and one idempotent `projection_drift` outbound intent per configured target. External Feishu delivery is attempted only after that transaction commits; retryable failures remain `retry_wait`, exhausted failures become `dead_letter`, and missing target/dispatcher configuration is an explicit alerting outcome rather than silent success. Repeated failures in one active generation reuse the same delivery. Exact convergence creates a resolved delivery only after that generation enqueued an active alert; a scan candidate that clears inside the watcher grace period resolves silently, and a later recurrence increments the generation with a fresh notification throttle. Paginated PR snapshots are discovery-only: mapped missing/lifecycle/head candidates are re-read through the exact Forgejo PR endpoint before they become durable drift. The durable pull-request projection job also owns each `forgejo_action_rebase` generation: it records its exact `action_intent_id`, preflight AGS/base/Forgejo heads, desired and observed heads, correlation identity, phase, attempts, and the single-success-comment claim. Every provider seam, phase write, completion, denial, and recovery matches that intent ID plus job generation and authority facts; historical nullable/unbound jobs fail terminal and never select a newer intent. Duplicate labels and startup resume continue the saved desired head through DB-CAS worker claims; they never create another rebase commit. Unknown Forgejo head changes become terminal drift, while a moved base becomes `needs_rebase`. The periodic projection watcher remains a fallback for scan-discovered or missed drift. Projection-drift label/comment presentation is best-effort, and optional GitLab shadow failure remains a separate provider outcome. `needs-rebase` is the conservative outcome when either base moves during the action window. Production readiness for signed Forgejo workflow actions also requires an initialized `projection_drift` target/dispatcher and live authority-policy convergence for every explicit mapping: native `allow_rebase_update` disabled, protected base push restricted to the configured integration bot, required labels present, and the AGS webhook active for pull-request, issue-label, and delete events. The idempotent `cmd/forgejo-authority` plan/apply/verify workflow owns this external configuration; it does not weaken or replace runtime lease/CAS and exact-SHA verification.

### Pull Request Merge

```text
REST or GraphQL merge request -> service merge logic -> gitstore merge/rebase/squash path -> DB state update
```

This is one of the highest-risk flows in the system because it crosses REST or GraphQL, DB state, and real Git history updates.

### Workflow Execution

```text
workflow dispatch -> service background runner -> Docker sandbox per `run:` step -> job logs/artifacts -> workflow completion
```

Workflow execution is fail-closed by default. The server only executes workflow steps when `ENABLE_WORKFLOW_EXEC=1` is set.

When enabled:

- each workflow `run:` step executes in an isolated Docker container rather than as a host shell process
- the container is launched with `--network none`, `--read-only`, `--cap-drop ALL`, `--security-opt no-new-privileges`, a bind-mounted temporary workspace, and a dedicated `tmpfs` at `/tmp`
- step processes receive only explicit workflow env vars plus a minimal runtime env (`HOME`, `PATH`, `CI`, `GITHUB_ACTIONS`); host `os.Environ()` is never inherited
- the service enforces workflow-wide timeout and container quotas for CPU, memory, process count, and file descriptors; timed-out containers are force-removed as a kill-switch
- workflow execution emits structured audit logs for start, each step, artifact handling, and final completion, all keyed by repo and workflow run ID

`actions/upload-artifact` remains a built-in service-side action over the sandbox workspace. Artifact collection skips symlinks and resolves real paths before reading files so workflow-created links cannot exfiltrate host files outside the workspace.

## Primary Product Flows

These flows should stay central in future work:

- server discovery and auth bootstrap through `/api/v3/`, `/api/v3/meta`, `/api/v3/rate_limit`, token login, and `gh auth setup-git` / Git credential setup
- explicit organization creation and governance through `/api/v3/user/orgs`, org invitations, team membership, and outside-collaborator inspection
- OIDC-backed human login and identity lookup through `/api/v3/oidc/*`
- repository creation, fork, transfer, delete
- repository sharing and effective permission resolution across org base permission, direct collaborators, and team grants
- issue creation, update, search, label and assignee management
- pull request creation, review, diff inspection, merge, revert；Human/provider effects authenticate only to AGS while AGS resolves provider coordinates and uses its deployment-owned integration executor
- release creation plus asset upload and download
- workflow discovery, dispatch, rerun, cancel, logs, and artifacts
- optional Forgejo and GitLab post-push mirroring for teams that use Forgejo PRs/Actions and GitLab backup visibility alongside AGS as Git ingress
- search across repositories, issues, pull requests, commits, and code

## Configuration

The canonical configuration reference is
[`../.env.example`](../.env.example). The top section contains the required
quick-start settings; later sections document optional runtime capabilities.

Configuration is loaded from environment variables in `config/config.go`
and a small number of subsystem-local environment reads for CORS, logging,
secret encryption, Git HTTP upload limits, and embedding concurrency.
`AGS_INTEGRATIONS_CONFIG` loads `team_authority` bindings、deployment-owned
Forgejo/GitLab/Multica mapping、provider executor references、notifications and
optional legacy `delegation` migration inventory. `principal_sessions` and
singular `principal_session` are rejected as retired live authority keys.
Operation names/risk are compiled into the AGS-owned
`internal/operationcatalog`; `team_authority` may bind trusted actors to the
maintainer class but cannot add an unknown operation. Provider credentials
remain service-side and never enter caller Context, Access Grant, or API
response.

## Local Development and Test Entry Points

The main developer commands are:

```bash
make setup
make run-bg
make test-unit
make test
make test-run SUITE=TestPullRequests
make test-script SUITE=TestPullRequests SCRIPT=pr-create-basic.txtar
```

`make setup` is for production or persistent development environments and
expects `DB_DSN` to point at TiDB Cloud Starter. Use `make test-setup` when a
local or CI test run needs the test-only `tiup playground` database.

To inspect the current acceptance inventory instead of hard-coding counts:

```bash
(
  cd cli
  go test -tags acceptance ./acceptance -list '^Test'
  find acceptance/testdata -name '*.txtar'
)
```

## Related Documents

- `docs/forgejo-integration.md` records the optional AGS -> Forgejo mirror used to trigger Forgejo PRs and Actions.
- `docs/design/delegation-policy.md` records legacy delegation as non-executable migration inventory.
- `docs/operations/team-authority-hard-cut.md` owns the atomic runtime-key/binary cutover and rollback contract from retired `principal_sessions` to `team_authority`.
- `docs/design/delegated-agent-session.md` records the principal binding, native-grant, operation-scoped Session, and `workload.context.v1` implementation.
- `docs/module-contracts.md` records layer ownership, dependency rules, and current technical debt around concrete couplings.
- `docs/architecture/tenant-db-correctness.md` records the `DBForCtx(ctx)` tenant-routing rules for service code.
- `docs/test-strategy.md` describes the phased test roadmap.
- `internal/router/router.go` is the concrete route inventory.
- `docs/module-contracts.md` is the current domain-contract reference.

### Component References

- [REST API](architecture/rest.md) — v3 surface handlers, transform, respond
- [GraphQL API](architecture/graphql.md) — v4 query/mutation dispatch and field filtering
- [Git Smart HTTP](architecture/git-http.md) — transport bridge to git-http-backend
- [OAuth](architecture/oauth.md) — device flow endpoints
- [Service Layer](architecture/service.md) — domain interfaces and persistence orchestration
- [Git Store](architecture/gitstore.md) — bare-repository operations

### Design Documents

- [Legacy Delegation Migration Input](design/delegation-policy.md) — compatibility inventory and migration claim limit
- [Principal-Bound Operation-Scoped Session](design/delegated-agent-session.md) — current target authority and trace contract
- [Fork Capability Review](../fork/CAPABILITIES.md) — retained contracts, reproduced defects and outstanding generation review
- [Wiki Storage Re-Architecture](design/wiki-storage-rearchitecture.md) — delivery plan for issue #1488
