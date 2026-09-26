# Module Contracts

This document records the current architectural contracts between the main server modules.
It is meant to reduce drift between the intended layering and the real dependency graph in code.

This is a contract document, not a "refactor everything to interfaces" proposal.
When the code intentionally violates or relaxes one of these contracts, update this document.
This document records implementation contracts. [Fork governance](../fork/README.md) separates source generations, capability review, publication and runtime acceptance; private deployment transcripts are not product authority.

## Status

The contracts below describe one application database with explicit transaction-scoped
DB overrides. Upstream retired the control-plane router; the fork rejects its old
configuration rather than silently falling back to the application database.
Some boundaries are already clean.
Some are still concrete couplings by design or by technical debt.
Both cases are documented here explicitly.

## Core Invariant and Authority Split

The most important architectural invariant in this repo is that `agent-git-service` is Git-backed.
Repository content, history, refs, diffs, merges, rebases, and Git transport semantics should stay grounded in real Git behavior.

Authority is split by concern:

- `gitstore` owns Git-native repository state and operations.
- `db` owns higher-level relational metadata such as users, auth, issues, pull requests, reviews, labels, workflow records, and similar product state.
- `service` is the coordination layer when a flow needs both Git-backed and DB-backed state.
- surface packages should respect that ownership instead of inventing parallel storage rules.

These contracts do not forbid repository- or pull-request-related metadata in the database.
The narrower rule is that Git-native behavior must stay Git-backed, while relational metadata stays DB-backed.

Deployment topology is intentionally out of scope for this document.
The same module contracts should hold for a local all-in-one setup or a split, stateless deployment with external services.

## Core Layers

The main runtime layers are:

- `router`
- `middleware`
- `rest`
- `graphql`
- `service`
- `db`
- `gitstore`

Supporting packages such as `config`, `oauth`, `githttp`, `oidc`,
`connectedlogin`, `rest/respond`, `rest/transform`, `tenant`, `ratelimit`,
`metrics`, `logging`, `httputil`, `testharness`,
`apperrors`, `crypto`, `embedding`, and `randutil` are included where they
materially affect the contracts.

The public import surface is intentionally small:

- `config` exposes environment-backed startup configuration.
- `server` exposes the embeddable composition-root APIs (`New`, `Run`, `RunWikiReindex`, `Start`, `Shutdown`, and mountable handlers).
- Everything else in the root module remains internal-only unless documented otherwise.

## Top-Level Internal Package Inventory

This is the top-level contract inventory for `internal/*`.
When a new top-level package is introduced, add it to this table and then
document the relevant contract below in the same change.

| Package | Primary responsibility |
|---|---|
| `apperrors` | shared sentinel error catalog and helpers |
| `crypto` | NaCl-based secret encryption helpers |
| `cibackend` | independently configured CI observation adapters, exact backend-local identities and bounded logs; no user authorization, Git hosting or merge authority |
| `db` | relational schema, migrations, seed data, and model types |
| `delegationpolicy` | legacy delegation v1/v2 validation and non-executable migration inventory |
| `edge` | database-free runtime, fixed-primary forwarding and explicitly configured packet-driven local reads with per-RPC authority, verified Mirror and bounded view selection |
| `edgeprotocol` | credential-free read plans, canonical snapshot manifests/identities and bounded export framing; content binding is not authority |
| `embedding` | outbound embedding-provider integration |
| `executioncontext` | operator-registered fixed-egress execution-context resolution, closed source adaptation, and credential-free normalization |
| `forgejointegration` | AGS -> Forgejo branch/PR projection, including exact lease-protected mapped PR-head rewrites |
| `gitlabintegration` | optional AGS -> GitLab post-push mirror plus Forgejo-authoritative shadow MR and backup sync |
| `githubintegration` | optional AGS -> GitHub post-push mirror plus Forgejo-authoritative shadow PR and backup sync |
| `buildinfo` | dependency-free release identity and version output; no configuration, business state or network access |
| `gitbackend` | already-authorized native Git CGI execution, isolated snapshot reads, bounded spooling and explicit receive opt-in |
| `gittransport` | one-shot native provider Git with request-owned credentials, exact clean remote, bounded output and cancellation; no business or effect authority |
| `snapshotstore` | immutable self-contained Git views, exact-root exports, staging/verification/publication, pinned leases and private process-owned storage |
| `replication` | peer mTLS/export and original-user read-control HTTP adapters; capture and authority stay in the primary Service |
| `replicationadmin` | database-free remote native-admin registration client and offline, unapplied certificate/peer/binding plans |
| `githttp` | primary Git Smart HTTP authorization/storage/effect adapter using gitbackend |
| `integrations` | file-backed integration config loading and translation for Forgejo, GitLab, GitHub, Multica, notifications, and projection watch |
| `gitstore` | on-disk bare repository operations |
| `graphql` | GitHub GraphQL API surface |
| `httputil` | bounded outbound HTTP helpers |
| `logging` | structured logging and request-scoped log attributes |
| `metrics` | Prometheus collectors and metric-recording helpers |
| `mentions` | GitHub-style mention token parsing helpers |
| `middleware` | auth, logging, rate-limit, and request guards |
| `multicafailures` | Multica failed-run polling and projection into AGS incident records |
| `multicaprojection` | Multica issue/PR metadata projection plus the secret-free typed durable wrapper whose HTTP body is the existing closed `ExternalPRLinkRequest` |
| `notifications` | Feishu notification dispatchers plus the explicitly typed Multica external-PR terminal dispatcher |
| `operationcatalog` | AGS-owned standard/privileged operation names, risk classes, canonical aliases, and maintainer intent vocabulary |
| `operationconstraints` | exact default-operation constraint schemas plus shared JSON-safe integer, branch-ref, and SHA normalization |
| `oidc` | generic OIDC discovery, device flow, and JWKS-backed ID token verification |
| `oauth` | OAuth device-flow HTTP endpoints |
| `projectionwatch` | periodic Forgejo projection drift scanner/notification loop |
| `providerlogbridge` | host-local, socket-peer-bound Forgejo Actions log bridge with exact DB/ref/SHA binding and bounded decoding |
| `providerlogprotocol` | shared encoded/decoded byte ceilings for provider log transport |
| `randutil` | shared random helper functions |
| `ratelimit` | GitHub-compatible rate-limit snapshot helpers |
| `rest` | GitHub REST API surface |
| `router` | route registration and host rewrite |
| `service` | business logic and cross-store orchestration |
| `sessionauthority` | team-authority-v4 registered-source/workspace/team/class bindings, exact policy-class operations, target/service resource defaults, exact repo exception overrides, and non-executable legacy compatibility inventory |
| `connectedlogin` | configurable OAuth-style browser-login code exchange and userinfo client |
| `tenant` | legacy context marker for explicit denial at fork authorization/replication boundaries; not a storage or DB router |
| `testharness` | production-wired service and router test fixtures |
| `wikicatalog` | legacy wiki catalog primitives, slug canonicalization, and transitional blob/CAS helpers |
| `wikiv2` | git-authoritative wiki write planning, derived index contracts, and reconcile primitives |
| `workloadidentity` | credential-free `workload.context.v1` provenance value type used by Access Grant snapshots and transport receipts; no assertion verification or authority selection |

## Dependency Rules

| Layer | Owns | May call directly | Must not call directly |
|---|---|---|---|
| `router` | route registration, host rewrite, top-level HTTP composition | `middleware`, `rest`, `graphql`, `githttp`, `oauth` | `db`, `gitstore`, GORM queries, business logic |
| `middleware` | auth extraction, request guards, context injection | `service` auth methods, `rest/respond`, `logging`, `metrics`, `ratelimit` | `db`, `gitstore`, REST handlers, GraphQL resolvers |
| `rest` | HTTP request decode, REST response codes, REST JSON shapes | `service`, `rest/respond`, `rest/transform`, `ratelimit`, `db` model types, `Svc.Git` via `*service.Service` | GORM queries, GraphQL helpers |
| `graphql` | GraphQL request parse, resolver dispatch, GraphQL response shapes, field filtering | `service`, `db` model types, `rest/respond` for HTTP JSON writeout, selected `Svc.Git` and `Svc.DB` access via `*service.Service` | `rest/transform` |
| `service` | business rules, persistence orchestration, Git orchestration, domain side effects | `db`, `gitstore`, `cibackend`, `sessionauthority`, `operationcatalog`, `operationconstraints`, `delegationpolicy`, `workloadidentity`, `executioncontext`, `embedding`, `oidc`, `connectedlogin`, `forgejointegration`, `edgeprotocol`, `snapshotstore` | `router`, `middleware`, `rest`, `graphql`, HTTP response helpers |
| `db` | schema, migrations, seed data, relational model types, shared state constants | GORM and standard library only | `service`, `rest`, `graphql`, `gitstore` |
| `gitstore` | Git-native repo lifecycle, refs, merge/rebase/diff/content/archive operations | system `git`, go-git, filesystem | `db`, `rest`, `graphql` |

## Layer Contracts

### `cibackend`

Owns typed provider run/job/workflow observations and bounded transport, not their public AGS IDs or permissions. `service` authorizes repository access, selects an explicitly configured backend, maps durable IDs and binds current-head evidence before REST or GraphQL renders it. Git hosting, CI choice and merge authority are independent. Missing provider capabilities or incomplete evidence never trigger a fallback to another backend. See [CI backends](architecture/ci-backends.md) and [official gh operations](operations/official-gh.md).

### `router`

Ownership:

- builds the HTTP tree in one place
- decides which endpoints are authenticated, optionally authenticated, or unauthenticated
- performs host rewrite for `api.github.localhost`
- registers the profile-free Access Grant family in the single-database runtime; nonempty `CONTROL_PLANE_DSN` is rejected before bootstrap. A generic DB-ready response is not feature readiness
- registers upstream helpers under `/api/ext/v1`; the fork server optionally registers supported old `/api/v3` helper aliases with exactly the same middleware/handlers, without redirects or repeated effects

Rules:

- `router` is composition only
- it may wire handlers together, but it should not implement business logic
- it should not inspect the database or Git state directly

Current state:

- clean boundary overall
- `router` depends on surface handlers and middleware, not on `db` or `gitstore`

### `middleware`

Ownership:

- parse auth headers
- validate durable tokens and single-database delegated-session credentials
- inject current user plus a delegated Session identifier/presentation snapshot into request context; only the identifier may seed later authority evaluation
- enforce operation-scoped delegated surfaces: `repo.read` verification reads plus only exact `GET /api/v3/repos/{owner}/{repo}/branches/{safe-branch}/protection` (including safe slash refs, excluding subresources, list/wildcard variants and writes); `pr.read` exact numbered-PR, named-head, or exact provider-projection reads plus the single `PullRequestByNumber` GraphQL query emitted by `gh pr view`, with GraphQL owner/repository/number bound to Session facts; `review.read` verification reads; exact PR/head-bound `ci.read` verification and provider-run reads; exact-ref `pr.create`; exact-intent `pr.rebase`; and `git.read`/`git.push` confined to upload-pack/receive-pack Git Smart HTTP. Keep durable audit for rejected delegated methods and TTL-bounded legacy compatibility for rows without an operation
- enforce request body limits
- preserve optional-auth behavior on discovery endpoints that `gh` uses for server discovery and auth bootstrap

Rules:

- middleware may reject requests before they reach surface handlers
- middleware owns API auth handling for REST and GraphQL routes
- discovery endpoints under `/api/v3`, `/api/v3/meta`, and `/api/v3/rate_limit` are intentionally optional-auth because clients probe them before or during auth setup
- middleware must not issue GORM queries or inspect Git repositories directly

Current state:

- `TokenAuth` and `OptionalTokenAuth` depend on a concrete `*service.Service`
- `ags_sess_*` resolution rejects legacy tenant contexts; retired control-plane startup is unavailable
- delegated REST and the exact numbered-PR GraphQL read use the normalized Session operation before admission; the GraphQL gate parses a bounded body, requires one query with the canonical repository/pullRequest AST and rejects mutations, fragments, alternate aliases, noncanonical variable definitions, extra variables/resources, and repository/PR mismatch before resolver dispatch; service still enforces exact repository, transport facets, principal native grant, and purpose-separated PR proof
- Git Smart HTTP requires `git.push` for new push Sessions, admits only `git-receive-pack` discovery/execution, rejects upload-pack/clone/fetch, then applies exact repository, read/write facets, native grant, and protected-ref policy; control-plane Session routing remains fail-closed
- this is a real coupling, not just documentation

Assessment:

- acceptable for now
- highest-value candidate for a narrow interface later because the required contract is small:
  - `ValidateToken`
  - `ResolveUserByToken`

### `rest`

Component reference: [architecture/rest.md](architecture/rest.md)

Ownership:

- decode REST path, query, and JSON body inputs
- perform transport-level validation such as required fields and type parsing
- call service methods
- transform service and DB objects into GitHub REST JSON shapes
- add secret-safe `ags_actor`/`delegated_by` extensions to delegated PR create/get/list responses while preserving the stable principal in `user`
- serialize collaboration-governance responses such as org invitations, team-repo permission maps, and org member versus outside-collaborator annotations
- map service errors to HTTP status codes through `rest/respond`
- expose exact PR-scoped provider-neutral projection and CI evidence returned by `service`; REST never reads provider credentials or calls Forgejo directly
- own the closed, bounded, no-store `/api/v3/access-grants*` wire surface: source bearer appears only in issue/renew bodies, Access Grant bearer appears only in one Authorization header, the one-time internal transport bearer is returned only by its exact adapter route, and all canonical actor/policy/effect decisions remain in `service`

Rules:

- REST handlers should stay thin
- REST may use `db` structs as wire types and input containers
- REST must not run GORM queries directly
- target boundary: REST should not call `gitstore` directly

Current state:

- the package is wired through `rest.Deps{Svc: *service.Service}`
- handlers consistently use `respond.ServiceError`, `respond.ValidationFailed`, and `transform.*`
- REST now owns the transport layer for explicit org creation, org invitations, team-repo grants, and outside-collaborator listing, while delegating the underlying policy and persistence to `service`
- many Git-centric handlers still call `d.Svc.Git.*` directly for branch, tag, diff, archive, search, and ref operations
- repo JSON shapes and collaborator lists now expose canonical `read`/`write`/`admin` authorization decisions through `service.HasRepoAccess`, while still serializing GitHub-compatible permission flags for transport compatibility
- branch get/list handlers obtain exact protection facts through `service.GetBranchProtection` / `service.ProtectedBranchNames` and pass the resulting boolean into `rest/transform`; transport shaping does not infer or hard-code branch authority state
- delegated PR actor fields are shaped from `service.PullRequestAttributionFor`; durable PRs receive explicit nulls and REST never reads session secrets directly
- REST does not run GORM queries directly today
- this is a real current coupling to Git infrastructure, not just a future concern

Assessment:

- acceptable for now
- the current package boundary is "thin transport plus direct Git access through `Svc`", not "service-only"
- interface extraction is optional, not mandatory
- the main reason to introduce narrower interfaces later would be easier handler-only tests, not runtime flexibility

### `graphql`

Component reference: [architecture/graphql.md](architecture/graphql.md)

Ownership:

- parse GraphQL requests
- route queries and mutations
- build GraphQL-specific response shapes
- filter response fields to the requested selection set

Rules:

- GraphQL owns GraphQL response assembly
- GraphQL should not reuse REST transform code because REST and GraphQL contracts differ
- target boundary: GraphQL should not bypass the service layer for persistence or Git logic

Current state:

- `graphql.Server` depends on a concrete `*service.Service`
- GraphQL writes HTTP JSON through `rest/respond`, but it builds its own GraphQL payloads
- GraphQL contains important mutation flows such as `revertPullRequest` and ProjectV2 operations
- delegated PR shapes expose camelCase `agsActor`/`delegatedBy` fields from the service attribution projection and advertise them through type introspection
- repository GraphQL shapes compute `viewerPermission` through `service.HasRepoAccess`, exposing `READ`, `TRIAGE`, `WRITE`, `MAINTAIN`, or `ADMIN`
- resolvers still reach into `s.Svc.Git` for bare read operations (HeadSHA, ListTags, CreateBranchFromOid); the business-rule paths — compare, mergeability, merge-simulation, branch update, revert — now flow through typed service methods

Assessment:

- acceptable for now because GraphQL has a large surface and a broad service dependency set
- GraphQL business-rule Git operations are now service-backed; the remaining `Svc.Git` calls are narrow Git reads, acceptable as an internal coupling
- worth formalizing by tests before attempting interface extraction
- if narrower interfaces are introduced here, they should be domain-grouped and incremental, not package-wide for its own sake

### `service`

Component references: [architecture/service.md](architecture/service.md) and [architecture/access-grants.md](architecture/access-grants.md)

Ownership:

- all business rules
- GORM persistence orchestration
- Git orchestration through `gitstore`
- cross-entity lifecycle changes
- authenticated exact PR-scoped provider evidence orchestration: resolve the AGS-owned projection binding, revalidate delegated authority immediately before the provider call, require exact observed PR-number correlation, exhaust bounded provider CI pagination, accept only runs bound to both the current AGS head SHA and either Forgejo's synthetic `#<pr>` ref or the current AGS PR head ref (so same-head `workflow_dispatch` reruns remain admissible), emit the exact provider PR number plus a compatibility `head_branch=#<pr>` binding while preserving the raw ref as `source_head_branch`, and return secret-free projection/CI observations with a correlation receipt that separates authorization, provider attempt/outcome, recovery ownership, authentication identity, delegated Session and workload provenance
- Human single-frontdoor provider merge: accept only AGS repo/PR、expected head and merge method from an authenticated `user_kind=human`, resolve divergent provider coordinates and executor entirely from server config/projection state, derive base only from the authoritative AGS PR and require it in provider mirror policy, enforce AGS native permission、independent current-head review/status policy、current AGS-base ancestry、default-base protection and provider executor collaborator authority；preflight compares Forgejo `PR.base.sha` to live Forgejo target ref `F`, not to AGS `A`；provider仅暂报`mergeable=false`时，只在state/head/base仍exact的前提下有界GET重读，任何漂移立即失败且不发POST；unknown/409 outcomes and `GET .../provider/merge` are observation-only；repo-local `fast_forward_ack` may acknowledge `F==H` without a merge POST and must not claim the Forgejo PR is merged or rewind an already-advanced AGS base；merge timing仍由Human决定，provider CI异步可观察； return operation/projection separately, and on retry after confirmed provider merge read the exact provider snapshot without a second POST
- secret-safe delegated PR attribution derived from immutable session/principal/human snapshots
- one DB-backed `RevalidateDelegatedSession` use-time evaluator shared by credential resolution, repository/Git effects, PR create/projection, and `pr.rebase` final provider admission; use-time native-grant reads are observational and never invoke compatibility backfill
- append-only content-addressed authority-boundary receipt construction/readback from loaded config, DB facts, and exact build source revision; for delegated exact `pr.rebase`, service exclusively owns the atomic intent/receipt lineage and Session-scoped integrity readback
- execution-context intake orchestration: invoke one registered fixed-egress puller, persist only the immutable canonical source ref/context/digest, and expose a credential-free receipt without creating actor, grant, Session, or operation authority
- Canonical Actor and Access Grant orchestration: resolve/JIT one credential-free `(source_instance_id, external_agent_id)` actor without auto-granting repository authority, keep actor/executor facts separate, intersect the selected executor with its live native repository grant, issue/renew/revoke one hash-only task/repository grant, evaluate optional selector/class/operation intent fail-soft against the fixed legal envelope, revalidate exact source/repository/native-grant/policy facts at use time, and append invocation receipts
- internal Access Grant transport adaptation: transactionally derive one exact operation-scoped Session without any assertion fallback, choose the default or accepted elevated binding that owns the Grant executor, emit the canonical `urn:multica:agent:<external-agent-id>` workload subject, retain executor authentication and canonical actor attribution as separate IDs, source PR linkage/initiator/originator only from the immutable snapshot, and revoke all dependent Sessions with Grant renewal/revoke
- exact Access Grant `pr.merge`: require one caller-owned canonical invocation ID bound to one global repo/AGS-PR/provider-PR/head/method effect key；the supported client deterministically derives and GETs it before any effect POST, emits zero POST for an existing receipt, and fails closed on uncertain GET；verify the live authoritative AGS PR base ref, require it in provider mirror policy and the exact PR head to contain that current base, match provider head/base/mergeability/CI、default-base protection or non-default server-executor collaborator authority, and enforce the same AGS independent current-head review/status policy；暂时不可合并只允许在open/head/base exact时有界GET重读，任何漂移或超时均在provider POST前失败；the PR creation-time `base_sha` remains historical diff metadata and cannot block a later monotonic base roll-forward；persist definitive pre-dispatch exact-fact drift as a GET-readable terminal `provider_attempt=not_attempted` conflict receipt, persist `dispatching + outcome_unknown` before the only server-owned provider POST, reserve HTTP 422/403 rather than 409 for locator collisions once an effect may have dispatched, and make lost-response/duplicate/recovery paths provider-read-only through that predeclared locator
- organization governance flows such as explicit org creation, org membership, org invitations, team-repo grants, and outside-collaborator reconciliation
- effective repository permission resolution across org base permission, direct collaborators, and team grants
- domain side effects such as workflow sync and embedding follow-up
- translating persistence errors into stable sentinel errors

Rules:

- `service` is the only layer that should coordinate both relational state and Git state
- `service` returns Go values and errors, not HTTP responses
- `service` may launch domain background work when that work belongs to domain consistency rather than transport

Current state:

- `service.Service` owns:
  - `*gorm.DB`
  - `*gitstore.Store`
  - `BaseURL`
  - `embedding.Embedder` (optional, activated when embedding API key is configured)
  - a narrow optional execution-context puller injected from the startup registry
  - immutable principal/session policy used by both delegated Sessions and Access Grants
  - `AllowAnyToken bool` (local-development convenience to accept any non-empty token)
- the concrete service now also owns the collaboration policy helpers that normalize permission vocabularies, resolve effective repo access, and reconcile org membership versus outside-collaborator state
- synchronous PR projection dispatch returns provider-scoped outcomes; the service action policy treats Forgejo as required and GitLab shadow as optional, carries the preflight-approved exact old Forgejo head into the lease rewrite, verifies all five head/projection surfaces before success, and persists required Forgejo failure before label/comment presentation
- no authoritative service-interface catalog exists in the current tree; concrete service methods are the implemented contract

Assessment:

- strong boundary conceptually
- future interfaces should be introduced only where a specific surface needs a narrower seam

### `db`

Ownership:

- schema and model definitions
- database initialization and migration
- seed data
- shared state constants such as issue and PR states

Rules:

- `db` should remain infrastructure-only
- it must not know about HTTP, handlers, or Git transport
- model types may be shared outward, but business rules must stay outside `db`

Current state:

- clean boundary overall
- models are imported widely as data types, which is acceptable
- `PullRequestProjection` stores AGS-owned links from an AGS PR to external Forgejo/GitLab PR/MR projections; it is a mapping/index table, not a second external fact source
- `execution_context_snapshots` stores immutable canonical source-ref/context JSON plus digest and source-local locators; update/delete hooks fail closed, and no source bearer, authenticating hash, request header, runtime alias, or egress endpoint is stored
- canonical runtime actors are ordinary non-admin `User(user_kind=agent)` rows linked by `UserIdentity(provider=execution_context, subject=source/Agent)`; JIT creation adds no token, collaborator grant, team membership, or provider credential
- `access_grants` stores immutable actor/executor/snapshot/task/repository/selector/class/operation/revision facts plus only a bearer digest/prefix and mutable lifecycle timestamps; `access_grant_invocations` stores immutable operation/effect coordinates and mutable provider outcome state. `delegated_agent_sessions` may additionally store `access_grant_transport` rows with parent Grant/revision and canonical actor IDs while retaining the executor as principal; application transactions revoke these rows when the parent Grant renews or revokes. Both Grant tables reject delete, and effect-key uniqueness spans grant renewal so unknown provider writes cannot repeat
- Access Grant transport Session rows snapshot parent Grant ID/revision, executor principal, canonical actor, team/class/epoch/resource/operation authority, `workload.context.v1`, trace quality, native/resource revisions, and optional AGS-owned human presentation facts. The implemented operation constraint maps are validated by one shared exact-schema kernel at transport issuance, persistence, durable authorization, and use time. Optional Squad/Agent/role values remain provenance only. Legacy/pre-contract rows are non-executable historical data; their state may be read only through the durable-site-admin exact-ID lifecycle projection
- `authority_boundary_receipts` is an additive append-only ledger for closed historical `legacy_authority_capture.v1`, current `legacy_authority_capture.v2`, and `delegated_effect.v1`; only legacy-capture v2 owns `resource_defaults`, v1 remains byte-shape compatible, GORM update/delete hooks fail closed, rollback never drops the table, and GET recomputes stored content digests
- `expiry_audited_at` is internal idempotency authority for the proactive expiry worker and is committed in the same transaction as the canonical audit row; it is never serialized in holder-facing Session receipts, but the durable site-admin-only `GET /api/v3/agent-sessions/{session_id}/lifecycle` surface may expose that timestamp together with a canonical-team-v4-only, closed credential-free lifecycle projection for owning expiry attribution. A later operator revoke may coexist with the prior expiry marker; the current state remains revoked while the earlier committed audit fact is retained. Lifecycle v1 validates but omits operation constraints and never treats team identity as an alias for workspace identity

### `gitstore`

Component reference: [architecture/gitstore.md](architecture/gitstore.md)

Ownership:

- repository existence, init, fork, delete
- refs and branches
- merge, rebase, compare, diff, archive, file-content operations
- write serialization per repository

Rules:

- `gitstore` is infrastructure, not business policy
- it should not make database decisions
- it should not shape HTTP responses

Current state:

- clean boundary overall
- `githttp` uses `gitstore` directly for repository transport work, which is appropriate

## Supporting Runtime Packages

### `oauth`

Component reference: [architecture/oauth.md](architecture/oauth.md)

Ownership:

- OAuth device-flow HTTP endpoints and HTTP-specific response format

Rules:

- may call service auth methods
- must not persist directly through GORM

Current state:

- depends on concrete `*service.Service`
- acceptable for now because the package is small and already directly testable

### `githttp`

Component reference: [architecture/git-http.md](architecture/git-http.md)

Ownership:

- Git Smart HTTP transport
- primary adaptation to the shared `gitbackend` executor
- transport-level repository existence bootstrap
- post-push follow-up triggering

Rules:

- may call `gitstore` directly for repository existence and transport support
- may trigger service follow-up work that belongs to repository consistency
- should not own business rules such as PR merge policy or workflow semantics
- must not treat `REMOTE_USER`, client headers, or commit author strings as identity/authorization evidence; server-owned CGI variables may carry an already-authorized delegated receive policy into the pre-receive hook

Current state:

- depends on both `*gitstore.Store` and concrete `*service.Service`
- after push it runs `fixHEAD()` and `Svc.SyncWorkflowsFromRepo(...)`
- `resolveRepoContext` calls `service.AuthorizeGitTransport`, which resolves
  canonical identity and enforces anonymous/native access plus exact delegated
  `git.read`/`git.push` operation and repository constraints. A push-scoped Session
  cannot read. Authorization performs no Git storage preparation. The handler
  retains immediate-before-CGI `RevalidateDelegatedGitTransport` after primary
  storage/hook preparation; invalid credentials never fall back to anonymous.
- receive-pack supplies server-owned protected-ref facts to the refreshed
  pre-receive hook; AGS-protected branches reject direct Git HTTP updates for
  durable and delegated credentials, while delegated sessions additionally
  protect the default branch and enforce branch-only/no-delete/fast-forward rules
- delegated receive-pack reports successful/denied transport outcomes through
  `service.LogCurrentDelegatedSessionAudit`; `githttp` detects the transport
  result but does not shape or persist its own audit authority
- still treats `owner/repo` as the logical repository identity, while
  `gitstore` may add tenant-scoped physical roots beneath that identity

Assessment:

- acceptable for now
- worth keeping visible as technical debt because one handler currently mixes:
  - Git transport
  - repo bootstrap
  - post-push follow-up
- product priority remains GitHub-compatible API and repo workflows, with gh CLI compatibility as a first-class compatibility target, so future hardening should treat this package as a support surface for repo workflows rather than as an independent feature area

### `gitbackend`

Shared native Git HTTP execution, extracted from the primary handler. Depends
only on the standard library and installed Git, never DB/service/router or
repository creation. Callers supply an already-authorized, trusted local view.
Receive-pack requires explicit opt-in. Service/method/query binding is checked
before CGI; original credentials are stripped from the child request. The
primary's body-limit environment remains supported. A handled response is not
proof of successful Git ref updates; effects and audits remain on the primary.

### `edgeprotocol`

Credential-free versioned ReadPlan, RepositorySnapshot, canonical Manifest and
bounded export framing. Owns structural/content bindings and manifest hashing,
not peer/user authentication, live capture, object verification or permission
minting. Typed prepare/revalidate envelopes reject unknown, duplicate and deeply
nested JSON; the separate primary peer handler accepts them only with both node
and original-user checks. Store incarnation identity is separate from public
repo name and must not be reused after delete/recreate. No default public route
or listener is added by these contracts.

### `edge`

Host-gateway extension: explicit `UnboundReads=primary` routes unenrolled reads
at ingress to the fixed primary; it never retries failed authorized cache reads.
`Prewarmer` periodically reconciles registered identities via peer-only warm RPCs,
without user credentials. `Operations` probes primary/peer/capacity, owns a separate
loopback-only diagnostic handler, and expires stale health. `Telemetry` retains
bounded credential-free active/completed request stages and closed route counters.
Client machine setup is a separate reversible script; no runtime DNS/Tailscale
administration is introduced. See `docs/operations/ags-edge-host-gateway.md`.

Independent runtime assembled by `cmd/ags-edge` using `config.EdgeConfig`.
Must not import primary `server`, `service`, `db`, control-plane or integration
workers; the binary dependency-graph test enforces this transitively. Proxies
writes/business traffic to one configured primary while retaining original
caller credentials, not replication credentials. C1 optionally assembles a real
ReadRuntime from closed owner-only file configuration. It parses bounded Git
v0/v1/v2 discovery/fetch requests, selects exact want-compatible snapshots from
bounded credential-free hints, and revalidates original users after sync before
isolated local CGI. Discovery always checks primary-current state. No IP/cookie
sessions, empty-repo creation, download fallback, stale substitution, mutation
retries or network-vendor configuration. Unwired reads stay closed; wired reads
work; when Operations is configured, /readyz reflects current dependency checks
rather than a permanent release gate. Mirror implements EnsureSnapshot
through snapshotstore with bounded/coalesced producers and pinned views; its
cache is never an authorization cache. PeerClient uses explicit HTTPS/mTLS trust,
no environment proxies or redirects. Original-user Authorization is accepted only
as a per-request argument for prepare/revalidate; export/background jobs never
carry it. Cold imports use complete packs; compatible retained bases use exact
v2 target-minus-base transfers. Both ends pin bases; missing primary bases can
explicitly return the same full target, while denial/corruption never downgrade.
Mirror publishes exact verified views and exposes process-local byte/transfer/
retention observations. If the target retains every base object, immutable base
pack/index files are hardlinked rather than repacked; otherwise exact local
reconstruction excludes old objects. Published byte/count/idle budgets protect
recent and pinned views; failure to admit capacity is an error, not stale success.

### `snapshotstore`

Depends only on standard library, edgeprotocol and native Git. Owns private,
process-exclusive storage; canonical manifest verification; exact object closure;
staging/fsync/publication; restart verification; pinned leases and explicit GC.
Published views never borrow source paths/config/hooks or keep alternates.
Incremental import temporarily borrows an exact pinned local base in staging;
received objects must equal target-minus-base. If every base object stays allowed,
publication hardlinks its immutable packs; deletion/force-update with excluded
objects instead triggers exact repacking. Each view owns its directory entries,
so base eviction is safe; in-place corruption of a shared inode is NOT isolated.
Capture reserves private staging outside the primary barrier and pins source
object files inside it without taking Store.mu. These pins may contain source-only
objects and are never SnapshotViews: only manifest-root exports are publishable.
The source and snapshot root require a hardlink-capable shared filesystem, with
no full-copy fallback. Retention counts logical bytes PER VIEW (conservative for
hardlinks), not physical allocation or bytes actually freed. Collection
never removes pinned/recent views, restart gives a grace window, and imports fail
when protected data prevents admission. ObserveManifest assumes
a caller-owned source capture/GC guard; it does not make current AGS write paths
coherent or infer export authority. Pinned ContainsObjects checks only the exact
self-contained object set; it does not widen native Git's want authorization.
Linux/macOS are supported for process locking.

### `replicationadmin`

Operator-only client assembled by `cmd/ags-replication`; depends on standard
library and `edgeprotocol`, never primary server/Service/DB/GitStore or Edge
runtime (transitive executable test). Uses fixed-origin HTTP(S), owner-only token
files and no environment proxy/redirect/mutation retry. Status is read-only;
registration sends one expected-authority/ID/creation-time-bound POST through
the running primary. A transport failure is not permission to retry with a new
observation. Peer planning verifies an existing explicit client-auth certificate
and emits credential-free unapplied matching fragments; no PKI issuance, private
key export, runtime config edit, grant mutation, restart or network changes.

Native registration authority is separately opt-in on the primary. REST decodes
closed envelopes and maps errors; Service checks fresh native repo-admin scope,
no delegated/tenant fallback, lifecycle binding and internal identity allocation
under the owning capture barrier. Reading status never allocates. Configured
registration/peer authority identities must match before bootstrap. This endpoint
is not a replication peer surface and does not widen Access Grant operations.

### `replication`

Owns thin peer HTTP adapters for retained-view export and primary read control.
Both require verified mTLS, registered key and exact store grants. V2 export binds
one exact compatible base and sends an object-set difference, not caller-supplied
arbitrary haves. Export rejects
user bearers and only reads retained views; prepare/revalidate require ORIGINAL
user authentication and delegate authority/capture to the primary service. The
prepare adapter repeats token resolution after capture and revalidation checks
fresh native/Grant facts. It does not widen delegated REST surfaces, initialize
repositories or perform business effects. The primary-owned optional runtime
factory assembles these handlers. The explicit AGS_REPLICATION_CONFIG_FILE startup
option attaches a separate TLS listener in that same process; absent the option,
default listeners remain unchanged. Public routes never expose the peer handlers.
Peer RPCs recheck certificate-chain validity after the original TLS handshake.
The server validates node configuration/certificates before DB bootstrap, checks
persisted identities and actual hardlink/free-space conditions before listening,
and drains active peer handlers before closing retained storage. Provisioning,
network sharing and certificate hot reload are not implicit side effects.

`service.PrimaryReadAuthority` may depend on `edgeprotocol` and `snapshotstore`
for this orchestration. `gitstore` owns the in-process mutation/capture barrier;
service lifecycle/native Git adapters participate. Replica provisioning is an
explicit native repo-admin operation protected from delete/recreate. Capture
holds the Store-wide barrier for authorization/policy/refs and private immutable
object pinning, not packing, fsck, publication, retained-store verification or WAN
transfer. File enumeration/linking still creates a bounded source pause. Source
mutation/GC during pinning must be coordinated; in-place object mutation is
unsupported. Effective export policy binds sorted prefixes as well as the label;
unsupported effective hidden-ref/custom-hook/disabled-read policy fails closed.
See [AGS Edge](architecture/ags-edge.md) for the supported main-repo/single-DB
boundary and remaining wiki, listener/key lifecycle and Stage C/E gates.

### `forgejointegration`

Component reference: [Forgejo Integration](forgejo-integration.md)

Ownership:

- optional AGS -> Forgejo post-push branch mirroring
- small Forgejo REST client for repo/PR ensure, exact live PR reads, PR-bound Actions run reads, PR labels, and PR comments
- `git push` orchestration from an AGS bare repo to a mapped Forgejo repo
- slash-aware branch include/exclude and repository mapping policy
- paginated live PR discovery snapshots plus exact-number PR reads used to confirm mapped integrity candidates
- signed Forgejo webhook parsing for merge, branch-delete cleanup, and AGS workflow action labels; webhook labels are delivery events only. They may consume a previously persisted exact action intent or ask the label-first adapter to create one through an explicit actor-to-immutable-principal binding, live Forgejo write-permission check, and the shared AGS evaluator. When configured, the read-only authority-policy client inspects another collaborator's effective provider permission because Forgejo denies that read to a write-only integration bot; all provider writes remain on the least-privileged integration client, and neither reader identity nor sender/provider role authorizes mutation
- idempotent authority-policy plan/apply/verify for mapped repositories, including native-update, base protection, bot whitelist, labels, and webhook state

Rules:

- AGS push handling must stay one-way from AGS to Forgejo
- mapped auto-PR repositories must persist the authorized AGS PR first; a head that cannot yet be mirrored or projected becomes observable durable projection work/failure and must not reject or roll back the AGS PR fact
- Forgejo merge callbacks may advance AGS base only by fast-forward compare-and-swap
- `pr.rebase` request/status endpoints authorize the original delegated Session or original durable principal through the shared full evaluator; durable status reads and every durable provider/recovery write seam reload principal, repository, native grant, policy/team/class/epoch and revisions, rebuild the exact persisted authority snapshot, and terminally deny drift before any provider write. Its Session/durable constraint schema is exactly JSON-safe positive `pull_request_number` + `forgejo_pull_request_number` and canonical lowercase 40-hex `expected_head_sha` + `expected_base_sha`, with old ref/`exact_head` or mixed schemas denied. Action request SHA values are validated without trimming or case normalization; null, surrounding whitespace, uppercase, and non-40 values fail before intent/provider writes, while accepted bytes round-trip unchanged through durable storage, POST/GET receipts, and recovery. The service persists the originating Session ID plus principal/team/class/epoch/revision and exact AGS/Forgejo/head/base/full-label/expiry facts before dispatch. Intent creation locks the authoritative AGS PR row before expiry reconciliation, complete-fact idempotency comparison, active count and insert; webhook and startup recovery use that same row lock, terminalize expired rows, and reject zero/ambiguous matches instead of selecting an unordered intent. Before the provider label write, the lock order is `PullRequest -> PullRequestProjection -> PullRequestActionIntent -> receipt insert`; one local tenant-DB transaction appends/links the content-addressed `delegated_effect.v1` receipt, moves `planned` to durable `dispatching`, and records `outcome_unknown`. Provider eligibility starts only after base-DB readback recomputes the exact receipt identity/schema/digest/Session/constraints; the external call is never inside that transaction. Webhooks may consume `dispatching` or `dispatched`, provider uncertainty remains retryable `dispatching`, and a late provider acknowledgement must reload rather than overwrite any webhook-advanced state. Historical delegated `dispatching+` without proven lineage and new-protocol missing/corrupt receipts enter `recovery_needed` without provider mutation; empty historical provider evidence serializes as `not_recorded`, never inferred as `not_attempted`. Every effect seam rebuilds the four authority constraints from that exact intent, reloads the exact AGS/Forgejo mapping and live head/base facts, and treats refs as non-authority provider coordinates; receipts remain secret-safe. Every action job generation stores its exact nullable-for-history `action_intent_id`, and completion/denial/recovery/provider writes must match that ID, generation, repository, PR, Session, and principal; unbound historical jobs fail terminal without mutating an intent. A bound job may converge only from its expected old head to its durably recorded exact new head; a new intent/webhook may not adopt a newer head as recovery. `repo.admin`, provider role, profile/name, and webhook sender are not action authority
- A `delegated_effect.v1` Boundary Receipt proves only fresh exact delegated admission and AGS readback. It never proves a provider acknowledgement, provider state, send success, or operation completion; `verified_completed` belongs only to the provider-convergence + job/intent completion transaction. This module has no provider outbox or cross-system transaction/exactly-once claim.
- Access Grant `pr.merge` is the sole profile-free merge path and has no Multica delegation storage/consume dependency: a workload Grant receives this one operation only when `AGS_ACCESS_ROLE=maintainer` or `AGS_ACCESS_ROLE=admin`; the two values are equivalent and cannot add any other privileged operation. The service still requires a caller-owned canonical invocation ID, exact AGS/provider PR + head + live AGS base `A` and live Forgejo base `F` + configured method + exact-head CI + provider protection, and a unique cross-grant effect key. `ExpectedBaseSHA` keeps AGS-base semantics. The supported client deterministically derives and GETs the locator first, with zero POST for an existing receipt and no POST after uncertain GET. The service persists definitive pre-dispatch fact drift under that locator as a terminal `provider_attempt=not_attempted` receipt; otherwise it persists the locator with `dispatching + outcome_unknown` before the only provider merge call. Repo-local `fast_forward_ack` may finish AGS facts without that POST when `F` already equals the expected head. Lost response, duplicate POST, renewal, timeout, restart, and invocation GET are exact provider readback only, and a locator collision after dispatch is 422/403 rather than 409. The originating bearer or a verified renewed descendant may read the predecessor invocation, but neither can dispatch another POST. The retired delegated-Session merge routes return `404`, and a Grant bearer never becomes a provider credential.
- Forgejo action labels may only deliver a matching unexpired AGS-owned intent to the shared leased rebase kernel. When no prior intent exists, a signed delivery with a stable delivery ID may create one only after an explicit normalized actor-to-numeric-principal binding, live provider write permission, active AGS principal, exact projection/head/base/full-label facts, and the shared durable `pr.rebase` evaluator all pass. The intent stores `forgejo_label`, the external actor, a deterministic content-addressed binding revision, exact authority/facts, and a delivery/fact-derived idempotency key; later durable writes and recovery revalidate both AGS authority and the same binding revision. Duplicate deliveries read the same intent without replaying effects. Admission denial creates no intent/job or Git effect, removes the action label, and idempotently projects `ags/status-blocked`; an already-absent live action label with no matching delivery intent is stale and ignored, so delayed webhooks cannot regress completion. Intent fact/Session/label drift is terminally denied without reusing invalid delegated authority, and status labels remain bot-owned facts
- rebase projection may rewrite only an exactly mapped, policy-eligible non-base PR head with `--force-with-lease=<ref>:<preflight-old-sha>`; naked force, `+refspec`, inferred leases, and unverified PR identity are forbidden
- the projection row may advance only after independent remote-ref and exact mapped-PR-head verification
- mapped PR missing/lifecycle/head drift from a paginated scan must be confirmed by an exact-number Forgejo read before durable recording; an exact-read failure aborts the scan rather than creating drift
- watcher candidates that resolve before any active alert intent was enqueued must resolve silently; recurrence must not inherit the prior generation's notification throttle
- action rebase job recovery may reuse only the same generation's durable desired head and exact accepted-old-head lease; a fresh webhook or intent whose live head/base no longer equal its persisted expected facts is terminally denied. DB-CAS job claims and the AGS PR row lock are correctness boundaries, while process mutexes are only an optimization
- generic PR projection treats durable `attempt` as its generation: claim and manual retry advance it atomically; every provider `BeforeWrite` reloads exact job/attempt/active phase and, when delegated, the Session; the worker deadline is no later than the persisted lease expiry; mapping upsert and final `projected` CAS commit in one AGS transaction so a stale generation leaves no mapping; each attempt is persisted in `pull_request_projection_job_attempts` and a later `projected` CAS must not erase a prior attempt error. These checks fence AGS generations but cannot make the final check and an external provider write a cross-system transaction
- interrupted action phases must be restart-safe, and unknown remote heads must terminate without force
- production readiness for workflow actions must fail when durable projection alerting or live authority-policy verification is absent; only an explicit visible development/test opt-out may degrade instead
- onboarding policy convergence must not replace server-side mapping, lease, CAS, remote-ref, PR-head, or final exact-SHA checks
- must not execute CI jobs or manage Forgejo Runner registration
- must not become a second PR/merge policy engine
- should be called as best-effort post-push work so Forgejo failures do not reject AGS pushes
- should preserve commit authorship and use the Forgejo token owner only as the push/PR actor
- should append delegated workload provenance from `service.PullRequestAttributionFor` to projected PR bodies without exposing authentication material or presenting the projection bot as the workload

Current state:

- wired through `service.Service.ForgejoIntegration`
- triggered from `githttp` after existing AGS webhook and PR-head post-push follow-up
- configured through `AGS_INTEGRATIONS_CONFIG`; legacy Forgejo environment variables remain supported for bootstrapping
- `cmd/forgejo-authority` exposes the external plan/apply/verify workflow and refuses onboarding without an enabled `projection_drift` target

Assessment:

- boundary is intentionally narrow and replaceable
- if retry queues or dashboards are added later, keep them behind this package instead of moving Forgejo-specific rules into `githttp`

### `gitlabintegration`

Component reference: [Forgejo Integration](forgejo-integration.md)

Ownership:

- optional AGS -> GitLab post-push branch mirroring for configured repositories and slash-aware `gitlab.mirror` include/exclude; a trailing `/` is a prefix so short-lived duty refs such as `sync/upstream-resolve/` can be skipped without excluding `main` or long-lived release branches
- optional shadow branch/MR creation after AGS PR creation
- optional backup push after a Forgejo-authoritative PR merge callback
- map AGS repo full names to GitLab backup project paths
- comment/close GitLab shadow MRs after Forgejo merge
- push the exact merged SHA from the AGS bare repo to a configured GitLab target branch

Rules:

- must not become merge authority
- must not merge GitLab MRs
- AGS branch push mirroring is a same-ref, same-SHA projection; it must not rewrite the target branch through PR or MR semantics
- should run as best-effort post-push work so GitLab failures do not reject AGS pushes
- should close shadow MRs before pushing backup main so GitLab does not auto-mark them as merged by ancestry
- must only run when `merge_authority: forgejo` or no conflicting authority is configured
- should keep tokens file-backed and redact token values from git command errors

Current state:

- wired through `service.Service.GitLabIntegration`
- invoked from Git Smart HTTP post-push follow-up to mirror configured branch refs to GitLab
- after a successful GitLab `PushRef`, open GitLab `pull_request_projections` for that head branch advance `last_synced_sha` to the confirmed SHA (Forgejo required projection rows are unchanged by this write-back)
- invoked by AGS PR creation to create shadow branches/MRs and record projection mapping rows
- invoked by the Forgejo merged-PR webhook after AGS main has been synced from Forgejo; a successful shadow-MR close (or GitLab already ancestry-merged with no open MR) upserts the GitLab projection row as `state=closed` with the merged SHA and never records GitLab as merge authority
- configured through `AGS_INTEGRATIONS_CONFIG`

Assessment:

- boundary is projection-only and replaceable
- shadow-MR close/comment behavior should stay here or in a sibling GitLab shadow package, not in Forgejo or Git HTTP code

### `githubintegration`

Component reference: [Forgejo Integration](forgejo-integration.md)

Ownership:

- optional AGS -> GitHub post-push branch mirroring for configured repositories and slash-aware `github.mirror` include/exclude; a trailing `/` is a prefix so short-lived duty refs such as `sync/upstream-resolve/` can be skipped without excluding `main` or long-lived release branches
- optional shadow branch/PR creation after AGS PR creation
- optional backup push after a Forgejo-authoritative PR merge callback
- map AGS repo full names to GitHub backup owner/repo remotes
- comment/close GitHub shadow PRs after Forgejo merge
- push the exact merged SHA from the AGS bare repo to a configured GitHub target branch

Rules:

- must not become merge authority
- must not merge GitHub PRs
- AGS branch push mirroring is a same-ref, same-SHA projection; it must not rewrite the target branch through PR or MR semantics
- should run as best-effort post-push work so GitHub failures do not reject AGS pushes
- should close shadow PRs before pushing backup main so GitHub does not auto-mark them as merged by ancestry
- must only run when `merge_authority: forgejo` or no conflicting authority is configured
- should keep tokens file-backed and redact token values from git command errors
- git mirror may use SSH without a token; shadow PR REST requires a token and must no-op quietly when the token is absent

Current state:

- wired through `service.Service.GitHubIntegration`
- invoked from Git Smart HTTP post-push follow-up to mirror configured branch refs to GitHub
- after a successful GitHub `PushRef`, open GitHub `pull_request_projections` for that head branch advance `last_synced_sha` to the confirmed SHA (Forgejo required and GitLab optional projection rows are unchanged by this write-back)
- invoked by AGS PR creation to create shadow branches/PRs and record projection mapping rows
- invoked by the Forgejo merged-PR webhook after AGS main has been synced from Forgejo; a successful shadow-PR close (or GitHub already ancestry-merged with no open PR) upserts the GitHub projection row as `state=closed` with the merged SHA and never records GitHub as merge authority
- configured through `AGS_INTEGRATIONS_CONFIG`

Assessment:

- boundary is projection-only and replaceable
- shadow-PR close/comment behavior should stay here, not in Forgejo or Git HTTP code


### `delegationpolicy`

Ownership:

- validate historical v1 workspace policies and v2 Agent/role/Task mappings;
- preserve enough source facts for a secret-free migration plan;
- mark every migration candidate non-executable and identify explicit target decisions.

Rules:

- must not import service, DB, transport, or credential packages;
- must not contain or return raw assertions, tokens, credentials, or signing material;
- legacy lookup is inventory tooling only and must never be called by Access Grant issue, transport derivation, or any other runtime authorization path.

Current state:

- loaded from optional top-level `delegation` in `integrations.yaml`;
- malformed, mixed-generation, secret-bearing, or ambiguous historical snapshots still fail startup;
- no delegation-policy HTTP endpoint exists; verification/migration inventory is startup or offline tooling only;
- no production service method invokes this package to issue a Grant or transport Session.

### `sessionauthority`

Ownership:

- validate and normalize the `team_authority` revision `2026-07-24.team-authority-v4`; the internal `PrincipalSessions` field name is compatibility plumbing, not a live YAML key;
- retain registered source-instance/trust metadata as configuration identity, while Access Grant issuance resolves the immutable workspace/team/class binding only from the server-pulled execution-context snapshot and never from a caller assertion;
- resolve one unambiguous active trusted-source/workspace/policy-class binding for execution-context-backed Access Grant issuance without accepting caller-selected team identity; apply exact policy-class operation sets from one defensive-copy registry before native-grant/resource evaluation; `multica.workspace.default.v1` has a fixed startup-validated ceiling of all read surfaces plus `git.push`, `pr.create`, and `pr.rebase`, while merge/review-submit/admin/create require a separately named, explicitly reviewed immutable-principal class; reject secret-shaped authority configuration values before formatting, and enforce configured epoch floors;
- retain legacy subject bindings only as migration/rollback inventory; canonical Access Grant and transport paths do not consult them and have no assertion compatibility fallback;
- resolve exact target/service/repository state and server-known operation requirement;
- expose no Agent, role, Issue, Task, Run, Trigger, Runtime, display-name, or credential-mode authorization input; only the canonical workspace ID is an authority coordinate, while workspace display remains provenance.

Rules:

- infrastructure-free, secret-free, and immutable after startup;
- bindings contain no repository/capability allowlist;
- resource entries contain no principal/capability mapping;
- canonical repository state and native grants remain service-layer authority; target/service `resource_defaults` only provide TTL/revision ceilings, while exact repo rows are exception overrides and never a second grant inventory.

Current state:

- loaded from top-level `team_authority` in `integrations.yaml`; residual `principal_sessions` fails startup before service composition;
- injected into `service.Service` for Access Grant issue/use-time evaluation, internal transport derivation/revalidation, durable authorization, exact operator explain, and auditable target-local epoch-floor advance;
- service startup persists revoked IDs before transport traffic and rejects later active reuse from rolled-back snapshots;
- missing target authority denies Grant issue/transport even when legacy mappings exist.

### `operationconstraints`

Ownership:

- own the exact constraint schemas for the implemented operations consumed by durable authorization, Access Grant invocations/effects, persisted transport Sessions, and fresh use-time checks;
- own canonical JSON-safe positive-integer parsing, branch-ref validation/equivalence, and lowercase full Git SHA validation;
- restore PR numbers and CI `run_id` as JSON numbers in durable receipts without changing canonical persisted Session strings.

Rules:

- `repo.read`, `git.read`, and `git.push` accept only `{}`; `pr.create`, `pr.rebase`, `pr.read`, `pr.merge`, and `review.read` use exact closed schemas; `ci.read` accepts only empty repository-wide, exact positive `run_id`, or exact PR-number shapes with optional canonical `head_sha`;
- `repo.create` requires exact target repository, base ref, source base SHA, source ref-manifest SHA-256, import mode, and visibility; `repo.admin` requires the same immutable source/repository coordinates plus the sole `forgejo_onboard` action; `review.submit` requires exact AGS/provider PR numbers plus one of `approve|request_changes|comment`;
- CI `event`, SHA-only, mixed, missing, extra, malformed, unsafe, old read-path `exact_head`, unknown admin/review actions, and secret-shaped vectors fail closed;
- operation constraints normalize admission facts only. Generic Access Grant transport rejects `review.submit`, `repo.admin`, and `repo.create`; dedicated effect/workflow owners must revalidate and receipt actual effects.

### `integrations`

Ownership:

- parse file-backed integration configuration
- translate startup `team_authority` into the internal authority set, reject retired `principal_sessions`, and load legacy migration inventory, Forgejo, GitLab, GitHub, Multica projection, execution-context connector registry, notification, and projection-watch runtime config structs
- read secret values from configured secret files without making service-layer decisions

Rules:

- startup/configuration helper only
- must not perform runtime projection, notification delivery, or persistence work
- must not log or expose raw token values

Current state:

- `main` loads `AGS_INTEGRATIONS_CONFIG` through this package and injects the translated configs into the relevant runtime components
- `execution_context` validation canonicalizes stable source IDs, HTTP(S)-origin runtime aliases, fixed adapter egress paths, bounded timeouts, and workspace mappings; it contains no source credential
- legacy environment variables are still supported outside this package for bootstrap compatibility

### `executioncontext`

Component reference: [architecture/execution-context-intake.md](architecture/execution-context-intake.md)

Ownership:

- resolve a runtime hint and/or stable source ID to exactly one operator-registered connector;
- construct a single GET to that connector's fixed egress endpoint;
- reject unmatched, ambiguous, path-bearing alias, selector-disagreement, and redirect cases before credential forwarding;
- decode and validate the closed `multica.current-execution-context.v1` source contract;
- return canonical credential-free context JSON, a stable source ref, and a SHA-256 digest.

Rules:

- request values never become an outbound URL;
- the current `mat_*` may exist only in request memory and one fixed-egress Authorization header, and must not enter DB/cache/file/log/error/trace/receipt or an authenticating hash;
- redirects are always rejected and receive no forwarded Authorization header;
- Task/Run must be running and workspace/Agent/Task must exactly match the caller locator;
- this package performs no GORM work and grants no AGS actor, operation, repository permission, Session, or provider effect.

Current state:

- startup injects an immutable registry into `service.Service` when top-level `execution_context.enabled` is true;
- only adapter `multica_current_execution_context_v1` exists;
- the AGS-token-free standalone bootstrap route is `POST /api/v3/execution-context/intake`; service persists the returned normalized facts in `execution_context_snapshots`;
- Access Grant issue/renew reuse this pull boundary, but canonical actor/grant/effect authority remains exclusively in `service`, not this package.

### `multicafailures`

Ownership:

- poll Multica issue/run data through the configured Multica CLI
- accept failed runs that map back to AGS repositories
- convert accepted failures into `service.MulticaTaskFailedInput` records

Rules:

- must use configured workspace/profile routing instead of mutating global CLI state
- must not store raw CLI tokens or long log bodies
- service owns incident persistence and notification fan-out

Current state:

- wired as a background watcher when Multica failure watch is enabled
- uses a small runner interface for deterministic tests

### `multicaprojection`

Ownership:

- resolve explicit Multica markers and issue URLs in AGS PR text
- enrich PR bodies with canonical Multica issue references
- project AGS/Forgejo/GitLab/CI/merge metadata to Multica
- construct the provider-neutral typed terminal wrapper and its deterministic idempotency key
- expose exactly two fixed Multica paths: merged `/complete-from-merge`, closed-unmerged `/links`; it never accepts a caller-selected raw URL or provider credential

Rules:

- Multica is a coordination/projection surface, not the Git or PR authority
- issue identity must be workspace-scoped when workspace information is available
- comments are optional; metadata/status writes are the default projection surface

Current state:

- `service` uses this package for AGS PR creation/update and merge-completion projection
- token verification and external PR registration are provider-neutral, even when the current provider is AGS

### `notifications`

Ownership:

- construct and send Feishu text notifications for configured AGS events
- render PR merge, projection drift, and Multica incident notification summaries

Rules:

- Feishu notification delivery is best-effort side-effect work, but required projection-failure outbound intents are durable business facts
- the typed Multica dispatcher only accepts `target_type=multica_external_pr` and a strict typed wrapper; it sends the exact existing `ExternalPRLinkRequest` JSON to the state-fixed path
- service owns the transaction that records terminal AGS facts and typed Multica outbound rows; `notifications` only dispatches persisted payloads
- retry/dead-letter/lease state must remain observable and must not roll back source facts
- must not own merge, projection, drift, Issue, or provider-effect state transitions
- webhook/service URLs and tokens remain config/secret-file sourced and must not be printed or persisted in payloads

Current state:

- `main` wires notification dispatchers from integration config
- service-level notifier interfaces keep business events decoupled from Feishu delivery details

### `providerlogprotocol`

Ownership:

- define the shared maximum encoded response size and decoded UTF-8 log size
- keep the provider client and host-local bridge on one bounded wire contract

Rules:

- contains no credential, repository, PR, run, filesystem, or transport authority
- size-limit changes must update both bridge and client tests in the same change

### `providerlogbridge`

Ownership:

- expose the host-local Forgejo Actions log bridge used only by the AGS server-owned provider adapter
- bind repository, provider PR, head ref, head SHA, task row, log path, socket peer CIDR, and bridge credential before reading bytes
- read only the database-selected regular non-symlink log file and enforce encoded/decoded protocol ceilings while reaping the decoder on timeout or cancellation

Rules:

- never accepts caller-selected provider coordinates as authority and never returns provider credentials
- forwarded IP headers cannot replace the captured socket peer
- does not own AGS PR/CI authorization, provider mapping, or public REST routing; those remain in `service`, `forgejointegration`, and `server`

### `projectionwatch`

Ownership:

- dispatch already-persisted projection-drift notifications on a local polling interval
- periodically reconcile bounded active Forgejo projection state through a service-backed source
- run complete historical provider integrity audits on a separate low-frequency schedule
- apply grace/throttle policy before projection-drift notifications
- mark notified drift records after successful notification attempts

Rules:

- watches and reports projection state; it must not repair refs or mutate PR authority facts
- required action failures enqueue immediately through `service`; the watcher is only a fallback for scan-discovered, missed, or restart-recovered drift
- notification polling must not perform provider I/O
- active reconciliation may inspect refs and open PRs; missing or stale candidates require exact provider confirmation before drift is recorded
- only the low-frequency full audit may enumerate complete provider PR history
- startup must delay the first provider history audit so readiness recovery is not coupled to full pagination
- service and integration packages own the underlying projection status records

Current state:

- `main` runs the watcher when projection watch config is enabled
- `poll_interval`, `scan_interval`, `full_audit_interval`, and `startup_audit_delay` independently own local notification, active reconciliation, historical audit, and startup isolation timing
- the package depends on narrow source/notifier interfaces for focused tests

### `rest/respond`

Ownership:

- GitHub-style HTTP JSON responses
- REST error mapping from service sentinel errors

Rules:

- surface packages may use it as an HTTP writer helper
- service code must not depend on it

Current state:

- used by REST, GraphQL, OAuth, and Git HTTP-adjacent paths for HTTP writing
- acceptable because the dependency points toward transport helpers, not back into business logic

### `rest/transform`

Ownership:

- REST-only JSON shape conversion

Rules:

- only REST should depend on this package
- GraphQL should keep building GraphQL-native shapes

Current state:

- this rule already holds and should be preserved

### `config`

Ownership:

- environment-backed process configuration
- validation of startup-only configuration invariants

Rules:

- `config` is a startup helper, not a runtime service locator
- it must not depend on `service`, transport packages, or persistence packages

Current state:

- `main` is the primary consumer
- the package owns OIDC, logging, replication and multi-listener flags; retired control-plane settings are rejection guards only

### Retired control-plane runtime

`internal/controlplane` and `internal/authn.TokenResolver` are absent. One primary
owns one application database; old control-plane configuration fails before
bootstrap. `DBForCtx` remains the transaction/test override entry, not a tenant
selector. No automatic DB migration or data merge is implied by this removal.

### `oidc`

Ownership:

- generic OIDC discovery document loading
- generic device-authorization and token exchange helpers
- JWKS-backed ID token verification and claim decoding for provider-neutral login

Rules:

- may perform outbound HTTP and JWT validation
- must stay transport-agnostic and must not persist application users or tokens directly
- owns low-level discovery and verification helpers, while provider-to-local-user mapping remains in `service`

Current state:

- `main` constructs the client and injects it into `service.Service.OIDC`
- REST handlers under `/api/ext/v1/oidc/*` (and explicitly enabled legacy aliases) call service methods, not the client directly

### `connectedlogin`

Ownership:

- configurable OAuth-style browser login URL generation
- authorization-code token exchange against the configured token path
- bearer-token userinfo lookup and claim extraction for non-OIDC providers

Rules:

- may perform outbound HTTP and provider response validation
- must stay transport-agnostic and must not persist application users or tokens directly
- must remain provider-neutral; provider-specific behavior belongs in deployment configuration such as endpoint paths and claim names

Current state:

- `main` constructs the client and injects it into `service.Service.ConnectedLogin`
- REST handlers under `/auth/connected/*` call service methods, not the client directly

### `tenant`

This dependency-light legacy marker lets fork authorization and replication
explicitly reject unsupported tenant contexts. It neither chooses a database nor
adds physical path prefixes. Retaining a denial marker does not restore the
retired multi-tenant runtime.

### `wikiv2`

Component reference: [architecture/wiki-storage-v2.md](architecture/wiki-storage-v2.md)

Ownership:

- git-authoritative wiki path and slug translation helpers
- durable ref compare-and-swap primitives for wiki writes
- derived index contracts for reconcile progress and live page projections
- manual reconcile request and result types shared by service orchestration

Rules:

- `wikiv2` defines storage and reconcile primitives, not HTTP handlers or route contracts
- it may depend on low-level git and wiki catalog validation helpers, but it must not issue GORM queries or shape transport responses
- service owns permission checks, orchestration, and lifecycle policy around these primitives

Current state:

- `service` uses `wikiv2` for slug/path parity, write-plan creation, and manual reconcile entrypoints
- `db` owns the concrete `wiki_page_index`, `wiki_index_state`, `wiki_backlinks`, and optional `wiki_page_history` tables, while `wikiv2` owns the domain contracts those tables implement
- the package is additive and does not yet replace the existing routed wiki handlers or all catalog-derived projections

### `ratelimit`

Ownership:

- GitHub-compatible rate-limit snapshots and headers

Rules:

- transport helper only
- must not own quota enforcement or persistence

Current state:

- REST uses it for `/api/v3/rate_limit`
- middleware and responders may reuse the same request-scoped snapshot so headers and JSON payloads stay consistent

### `metrics`

Ownership:

- Prometheus collector registration
- package-level recorder facade used by middleware and background jobs

Rules:

- instrumentation only
- must not own business logic or request routing decisions

Current state:

- `main` registers `/metrics`
- `middleware` records HTTP/request-operation metrics

### `mentions`

Ownership:

- exact GitHub-style `@login` token extraction and lookup helpers
- shared mention parsing semantics reused across notification and search flows

Rules:

- string parsing only
- must not depend on service, storage, or transport packages

Current state:

- `service/notification` uses it to expand user mentions into notification recipients
- `service/pr` and `service/search` use it to enforce mention-token boundaries instead of raw substring matches

### `logging`

Ownership:

- process-wide `slog` initialization
- request-scoped structured log attributes
- GORM logger integration

Rules:

- infrastructure-only package
- may enrich logs with context, but must not influence business decisions

Current state:

- `main` initializes logging before other startup work
- middleware and services add request attributes and clone them into background work

### `httputil`

Ownership:

- bounded helper utilities for outbound HTTP clients

Rules:

- only for server-to-server HTTP helpers
- must not become a generic transport abstraction layer

Current state:

- `embedding` uses it to cap error-body reads on upstream failures

### `testharness`

Ownership:

- production-wired service and router test fixtures
- reusable HTTP integration setup for package tests and benchmarks

Rules:

- testing-only package
- runtime packages must not depend on it

Current state:

- the integration strategy relies on `testharness.New(...)` and `testharness.NewService(...)` rather than mock-only seams

## Cross-Cutting Responsibility Contracts

### Authentication

Ownership split:

- `middleware`: extract API auth headers, reject malformed or missing credentials, and inject request-scoped auth context
- `server`: optional public embedding seam that can accept a trusted host authenticator, then adapt it into the shared middleware pipeline
- `auth`: public identity shape for embedded hosts using the server package
- `service`: validate API tokens and resolve user-by-token in single-DB mode; persist application users and tokens for OIDC-backed human login; map trusted embedded identities onto internal `db.User` + `UserIdentity` rows
- `oidc`: perform provider-neutral discovery, device-flow requests, and ID token verification
- `connectedlogin`: perform configurable OAuth-style code exchange and userinfo lookup for providers without standard OIDC discovery
- `githttp`: uses single-database `OptionalTokenAuth`, followed by exact repository/operation checks; optional authentication never permits private unauthenticated reads or writes
- `rest` and `graphql`: consume `GetCurrentUser(ctx)` and assume middleware has prepared the context

Rule:

- surface handlers must not parse auth headers themselves
- native and delegated validation share one application database; no control-plane token router exists
- embedded single-DB hosts may inject a trusted identity through `server.WithAuthenticator`; middleware must still be the single place that turns that identity into request context
- the trusted identity contract requires non-empty `Provider`, `Subject`, and `Login`; AGS owns the mapping from that tuple onto `db.User` + `db.UserIdentity`
- when embedded identity is present in single-DB mode, it takes precedence over `Authorization` headers and must flow through every REST/GraphQL/Git route family that already depends on optional or required auth context, including `/api/v3/rate_limit` and `/api/v3/users/{username}/starred`
- outbound identity-provider clients such as `oidc` and `connectedlogin` must not write application state directly
- nonempty control-plane configuration is rejected for every startup path, including embedded hosts

### Collaboration Authorization

Ownership split:

- `service`: normalize repository permission vocabularies, resolve effective repo access across org base permission, collaborator grants, and team grants, and reconcile org membership with outside-collaborator rows
- `rest` and `graphql`: call the shared service policy and expose transport-specific permission shapes and authz failures
- `githttp`: calls `service.RequireRepoPermission` for Git read/write authorization, supplies server-owned delegated receive policy to the CGI hook, and reports the resulting write outcome to the canonical service audit writer
- `rest`: Access Grant transport PR create delegates exact Session/constraint/native-grant revalidation to `service`; exact merge exists only under the Access Grant effect route；public current/operator Session revoke is retired, Grant lifecycle owns normal revocation, and only durable-site-admin exact-ID historical lifecycle readback remains
- `server`: owns the cancellable one-minute delegated-session expiry worker; `service` owns selection, idempotent claim, transaction, and audit payload

Rule:

- only `service` should combine collaborator, team, and org-membership state into an effective repository permission decision
- transport layers may map that decision into their own response shapes, but they must not fork the policy

Current state:

- REST and Git Smart HTTP share `service.RequireRepoPermission`/`service.RequireRepoCapability`; Access Grant transport Sessions first reproduce their live parent Grant, explicit binding + operation resolution, canonical repo state, target default/exact exception, and the executor decision from `service.HasRepoAccess`; a default is not a wildcard grant. Active rows are reloaded from DB and must reproduce the stored native-grant revision and authority snapshot at credential use and immediately before each external/repository effect
- workspace/Agent/role/Issue/Task/Run/Trigger/Runtime provenance never changes that decision; missing optional provenance is `trace_degraded`
- delegated PR creation authority comes only from the exact Access Grant transport Session plus the executor's live repository permission. The Session persists the separate canonical actor as PR author and derives authoritative Multica linkage plus initiator/originator provenance from the immutable parent snapshot; no assertion fallback exists
- Git transport remains separate; new push Sessions require `git.push`, only receive-pack discovery/execution is admitted, upload-pack/clone/fetch are denied, and receive-pack continues to enforce protected/default branch, ref-kind, delete, and non-fast-forward rules

### Request Validation

Ownership split:

- `rest` and `oauth`: syntactic and transport validation
- `graphql`: GraphQL request parsing and argument presence checks
- `service`: domain validation and state-machine validation

Rule:

- reject malformed transport input in the surface layer
- reject invalid domain transitions in `service`

### Response Transformation

Ownership split:

- REST: `rest/transform`
- GraphQL: GraphQL resolver and shape helpers
- OAuth and Git HTTP: package-local response formatting

Rule:

- service returns domain data, never GitHub REST or GraphQL payload maps

### Persistence

Ownership split:

- `db`: schema, migration, and application relational metadata
- `service`: GORM-backed reads/writes and explicit transaction-scoped overrides through `DBForCtx(ctx)`
- `gitstore`: all Git-backed reads and writes for repository content, history, refs, diffs, merges, rebases, and related Git-native state
- `tenant`: legacy context rejection marker only, never a persistence selector

Rule:

- target boundary: REST, GraphQL, middleware, OAuth, and router should not talk to GORM directly
- only `service` coordinates application GORM state and Git state together
- transaction-scoped `ContextWithDB` must survive service/background boundaries; it must not be replaced with the default DB during error recovery
- database-backed metadata is allowed even for repository or pull-request domains, but it must not replace Git as the authority for Git-native behavior
- current wiki rule: the sibling `*.wiki.git` repo is authoritative for wiki page content, path layout, commit history, and lexical search recall, while TiDB-backed wiki tables remain rebuildable derived indexes and transitional compatibility surfaces until the final `#1488` cleanup lands
- `wikicatalog` remains in the tree only as transitional logic that still backs some routed handlers and migration paths; it must not be treated as the long-term durable authority
- issue `#1488` tracks the remaining cleanup toward a fully git-authoritative wiki stack; see `docs/architecture/wiki-storage-v2.md` for the approved target design

Current state:

- the target rule holds for REST, middleware, OAuth, router, Git HTTP, and GraphQL
- `service.DBForCtx(ctx)` is the context-aware DB entrypoint; the runtime has no tenant DB router
- idempotent inserts followed by readback use a current read where repeatable-read isolation would otherwise hide a concurrently committed winner; this does not authorize repeating an external effect
- optional incident timestamps are nullable rather than invalid SQL zero dates; identifier quoting is delegated to the dialect

### Release identity and delivery

`internal/buildinfo` owns the closed, credential-free `ags.build.v1` identity.
The three binary entry points handle standalone version queries before any
configuration, database or network activity. It depends only on the standard
library; Edge must not acquire primary-service dependencies through versioning.
Release linker values identify the full source/tree and fork release version.
Development defaults remain explicitly unknown, not inferred from a machine path.

`fork/scripts/release.py` owns exact-source archive builds, platform smoke checks,
complete-CI admission, manifest/archive validation and complete draft publication.
`scripts/install-release.py` owns verification and replacement of one installed
program set under `~/.ags/bin`, not service lifetime or schema changes. There are
no retained version slots. Installed and running identities remain separate facts.
`scripts/ags-runtime.py` owns the optional single-root process launcher, startup
dependency observations and bounded stdout/stderr; the service manager owns
restart policy. It does not sweep authoritative data or live snapshot leases.
See [release operations](operations/releases.md) and
[single-root runtime](operations/user-runtime.md).

### Provider Git process credentials

`internal/gittransport.Run` owns the native provider Git subprocess boundary for
GitHub/GitLab/Forgejo pushes, Forgejo remote ref inspection and merged-object fetch.
Callers retain operation, exact-ref/lease and authorization decisions. The runner
replaces the exact authenticated remote argument, never writes credentials to
argv/environment or repository configuration, and uses only a short-lived private
HTTP include file. Redirects and inherited credential helpers/tracing are disabled;
normal process completion and cancellation release the private staging directory.
Do not claim crash-proof deletion or isolation from the same OS user/root.

It performs one subprocess execution, not a retry loop. Unknown push outcomes stay
unknown until caller-owned exact readback. Bounded stderr is mapped to fixed error
classes rather than returned verbatim. Legacy in-memory authenticated provider URLs
remain sensitive compatibility values; do not log or persist them. No credentials
are added to the Edge user/peer identity model by this outbound-provider adapter.

### Side Effects and Background Follow-Up Work

Ownership split:

- `main`: process lifecycle and listener goroutines
- `githttp`: transport-triggered post-push follow-up kickoff
- `service`: domain-owned async work such as embeddings and workflow sync implementation

Rule:

- background work should live where ownership is clear
- transport layers may trigger domain follow-up, but they should not silently absorb large amounts of business logic

### Error Mapping Across Boundaries

Ownership split:

- `service` and `internal/apperrors`: define stable sentinel errors such as `ErrNotFound`, `ErrConflict`, `ErrInvalidState`, `ErrValidation`, and prefer returning them for transport-visible failures
- REST: map sentinel errors to HTTP via `respond.ServiceError`
- GraphQL: return GraphQL error payloads
- OAuth: map service auth errors to OAuth-specific error responses
- Git HTTP: map missing repos to 404 and internal transport failures to 500

Rule:

- `service` owns semantic error categories, but current implementation is mixed between sentinel-based errors and plain wrapped errors
- each surface owns its own transport-specific rendering

Current state:

- many state and persistence paths already return sentinel-wrapped errors
- some transport-visible service failures are still plain `fmt.Errorf(...)` values, including merge-path failures in `service/pr_merge.go`
- REST only maps known sentinel categories specially; non-sentinel service errors can still collapse to HTTP 500

## Current Concrete Couplings Audit

| Coupling | Where | Status | Rationale |
|---|---|---|---|
| `router -> rest/graphql/githttp/oauth/middleware` | route composition | intended | router is the composition root for HTTP |
| `middleware -> *service.Service` | auth middleware | technical debt worth tracking | very small contract, likely worth narrowing later |
| `rest -> *service.Service` | `rest.Deps` | acceptable for now | broad service surface; real-router integration tests are higher value than mock seams right now |
| `rest -> Svc.Git` | Git handlers, PR diff, release archive, search, deployment helpers | accepted current coupling | multiple REST paths still reach Git operations through the concrete service dependency |
| `graphql -> *service.Service` | `graphql.Server` | acceptable for now | large resolver surface; stabilize tests first |
| `graphql -> Svc.Git` | repo detail reads (HeadSHA, ListTags, CreateBranchFromOid) | accepted current coupling | bare-read paths; compare/mergeability/merge-simulation/revert/update-branch now flow through `Svc` service methods (`ComparePR`, `CanMergePR`, `SimulatePRMerge`, `UpdatePRBranch`, `RevertPRMerge`) |
| `oauth -> *service.Service` | OAuth handler | acceptable for now | small package; current direct wiring is simple |
| `githttp -> *gitstore.Store` | Git transport | intended | transport handler needs direct repo access |
| `githttp -> *service.Service` | ensure repo exists, post-push follow-up | acceptable but visible debt | transport + follow-up logic are coupled in one package |
| `service -> oidc.Client` | generic human-login flows | acceptable for now | keeps provider-neutral OIDC protocol work outside business-state orchestration |
| `service -> connectedlogin.Client` | non-OIDC connected-login flows | acceptable for now | keeps configurable OAuth-style protocol work outside business-state orchestration |
| `service -> tenant` | fork delegated/replication admission | denial compatibility only | a supplied legacy marker is rejected; no root/tenant fallback |

## Refactors Worth Doing

These are the follow-ups most likely to improve maintainability or testability:

1. Introduce a tiny auth interface for middleware instead of depending on the full `*service.Service`.
2. Build router-level integration tests on the real handler tree before adding more mock-heavy handler tests.
3. Keep GraphQL on the concrete service for now, but group its future seams by domain if integration tests prove a specific split is valuable.
4. Consider isolating post-push follow-up from `githttp` if Git transport tests or workflow sync logic become harder to reason about.

## Refactors Not Worth Doing By Default

These should not be treated as mandatory:

- replacing every surface dependency with interfaces immediately
- abstracting `db` behind another repository layer
- abstracting `gitstore` behind a generic storage interface without a concrete testing need
- forcing REST and GraphQL to share the same response transformation package

## Testing Implications

The testing strategy should match the contracts above:

- package and domain tests should focus on `service`, `gitstore`, GraphQL and replication internals; upstream fixtures use isolated TiDB and fork SQLite compatibility has dedicated fixtures
- HTTP integration tests should exercise the real router plus middleware plus surface handlers
- `testharness` should remain the default production-wired integration fixture
- gh CLI compatibility tests should remain an end-to-end compatibility net within the broader GitHub-compatible API target

Practical consequence:

- because middleware, REST, GraphQL, OAuth, and parts of Git HTTP still depend on the concrete service, real integration tests are currently a better investment than a full mock-based surface test architecture

## Decision Rule For Future Changes

Before introducing a new interface or moving logic across packages, answer:

1. Does this change improve testability at the layer where bugs actually occur?
2. Does it clarify ownership, or only add indirection?
3. Does it remove a concrete coupling that is causing real friction today?

If the answer is "no" to all three, prefer documentation and tests over abstraction.
