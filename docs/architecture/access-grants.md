# Canonical Actor and Access Grants

## Status

Implemented by:

- `internal/service/access_grant*.go`;
- `internal/db/models_access_grant.go`;
- `internal/sessionauthority.ResolveRuntimePolicyAuthority`;
- `/api/v3/access-grants*` REST routes and their published OpenAPI contract.

This layer turns one fresh, task-token-bound execution-context observation into
an AGS-owned canonical actor and a short-lived task/repository authority grant.
It is independent of a durable `~/.ags-cli` profile and does not expose a
Forgejo credential.

For ordinary Git and GitHub-compatible API adapters, AGS can now derive a short-lived internal
transport Session directly from an already-authoritative Access Grant. This
path accepts no Multica Workload Assertion, preserves actor/executor separation,
and returns its bearer only to the invoking adapter. Assertion exchange is
retired: its route is absent and production use-time evaluation rejects every
Session credential mode except `access_grant_transport`. Provider observation receipts authenticated through that Session report `authentication.mode=access_grant_transport`; the historical `delegated_session` label is never emitted as current authentication.

The current routes are single-DB only. In control-plane mode a source locator is
not yet a tenant-routing credential, so the router does not register the Access
Grant family rather than accidentally falling back to the composition-root DB.

## Authority Model

```text
External Runtime
  owns: current source locator, current source bearer, optional intent selectors

Execution-context source
  owns: current workspace / Agent / Task / Run facts and source-token validity

AGS connector registry
  owns: stable source identity, fixed egress, adapter and workspace allowlist

AGS canonical actor
  owns: stable (source_instance_id, external_agent_id) identity

AGS policy registry
  owns: source/workspace/class -> immutable team binding,
        resource policy, operation registry and target

AGS Access Grant
  owns: one immutable actor/executor/task/repository/operation-envelope snapshot

AGS provider executor
  owns: server-only Forgejo integration credential and exact effect payload
```

Source facts and request environment variables are inputs, not grants. AGS is
the authority that intersects them with its current policy and repository
state.

## Canonical Actor

AGS resolves the actor through one `UserIdentity`:

```text
provider = execution_context
subject  = <source_instance_id>/<external_agent_id>
```

If that identity does not exist, AGS JIT-creates an active non-admin
`User(user_kind=agent)` and links it transactionally. JIT creation itself gives
the actor:

- no AGS token;
- no repository collaborator row;
- no team membership;
- no provider credential;
- no default repository permission.

A separately managed live native repository grant may later make that same
canonical actor eligible for an actor-bound elevated class for the independent
control-plane operations that use that model. The class binding alone is
insufficient, and the Runtime never receives a durable profile. Workload
`pr.merge` is intentionally simpler: `AGS_ACCESS_ROLE=maintainer` or `admin`
adds exactly `pr.merge` and does not consult an Agent allowlist, numeric
principal, or policy-class elevation.

The actor is stable across Tasks and Runs from the same source-local Agent. The
source can currently be Multica, but neither the identity key nor the grant
model embeds a Multica Workspace, Issue, or database as an AGS core object.
Those fields remain external provenance in the immutable context snapshot.

`ActorUserID` and `ExecutorUserID` are separate grant facts. The actor identifies
who performed the work. The executor identifies the accepted server-side policy
principal used for compatibility and audit. Forgejo mutation still uses only
the server-owned integration credential.

## Issuance

`POST /api/v3/access-grants` is unauthenticated at the durable AGS-token layer.
It accepts:

- the same fixed-connector selectors, locator and current `source_token` used by
  execution-context intake;
- one exact AGS `owner/repo`;
- optional `agent_id`, `policy_class`, and `operations` intent requests.

Issuance performs:

```text
fresh fixed-egress context pull
  -> immutable credential-free execution-context snapshot
  -> resolve/JIT canonical actor
  -> resolve one active default source/workspace policy binding
  -> evaluate optional selector/class/operation requests
  -> intersect current resource and operation policy
  -> persist immutable Access Grant authority facts
  -> return plaintext bearer once
```

Only `sha256(bearer)` and a non-authenticating display prefix are persisted.
The raw bearer is returned once as `ags_grant_*`, has a maximum lifetime of 30
minutes, and every response carries `Cache-Control: no-store`.

## Fail-Soft Intent, Fail-Closed Authority

The effective operation set is conceptually:

```text
requested = default-class operations
          ∪ accepted elevated-class operations
          ∪ valid low-risk additional requests

effective = requested
          ∩ active source/workspace binding
          ∩ canonical actor binding for elevation
          ∩ selected executor live native repository grant
          ∩ exact repository resource policy
          ∩ implemented operation schema
```

Optional selectors cannot self-grant:

- absent `agent_id` uses the canonical actor derived from the source;
- an `agent_id` equal to the current external Agent ID, canonical actor login,
  or canonical actor numeric ID is accepted;
- every other selector is ignored with `agent_selector_ignored_unbound`;
- absent policy class uses `multica.workspace.default.v1`;
- invalid policy classes fall back with `policy_class_ignored_invalid`;
- a valid elevated class is accepted only when its server-side team binding's
  principal is the already resolved canonical actor **and** that actor's live
  native repository grant covers the complete effective operation envelope;
- missing actor binding or native authority falls back with
  `policy_class_ignored_unbound`;
- unsupported, deferred, secret-shaped, or out-of-envelope operation requests
  are ignored or rejected without widening authority.

The receipt records requested/effective classes, requested/effective
operations, selector outcomes and stable warnings. AGS does not read process
environment variables directly. A Runtime or `ags-cli` may translate optional
`AGS_AGENT_ID`, `AGS_POLICY_CLASS`, and `AGS_OPERATIONS` values into these
request fields.

The default class remains the fixed bounded ordinary workflow envelope:
`repo.read`, `git.read`, `git.push`, `pr.create`, `pr.read`, `pr.rebase`,
`review.read`, and `ci.read`. It cannot contain merge, admin, repository-create,
or review-submit authority.

## Persistence and Lifecycle

`access_grants` stores immutable:

- actor and executor user IDs;
- execution-context snapshot ID;
- source/workspace/Agent/Task/Run and optional Issue/Runtime provenance;
- exact repository ID/name and target instance;
- selector/class outcomes and warnings;
- requested/effective policy classes and operations;
- binding, resource-policy and aggregate authority revisions;
- creation, expiry and renewal lineage.

Only lifecycle fields (`last_used_at`, revoke timestamp/reason) may change.
Update hooks reject authority-fact changes and delete hooks reject deletion.

Renewal requires:

- the still-active old bearer;
- a fresh source token;
- the same source, workspace, Agent, Task and Run;
- the same repository, actor, executor, target, operation envelope and authority
  revision.

It creates a new bearer and atomically revokes the old one plus every internal
transport Session derived from it. Policy drift cannot be accepted through
renewal. Explicit revoke is idempotent and transactionally revokes derived
transport Sessions as dependent authority. Status readback may show active,
revoked, or expired state without returning authenticating material.

## Invocation Receipts

`POST /api/v3/access-grants/authorize` validates one implemented operation's
exact constraint schema, revalidates the entire grant authority,
and appends `ags.access-grant-invocation.v1` with:

- grant, actor, executor and repository identity;
- exact normalized operation constraints;
- authority revision;
- authorization result;
- provider attempt/outcome facts.

For `repo.create`, `repo.admin`, and `review.submit`, the closed schemas are respectively pinned repository import, pinned Forgejo onboarding, and exact PR review action. These high-risk operations may be admitted only by an explicitly bound elevated class plus live native grant and resource policy; generic Access Grant transport Session issuance rejects all three. Admission is not provider execution: the supported `ags-expert` CLI reauthorizes `repo.create` directly before AGS import and `repo.admin` directly before target-owned operator onboarding, and separate nested receipts own effect claims.

This endpoint is an authority/receipt surface. It does not by itself turn the
bearer into Git Smart HTTP or general GitHub-compatible REST authentication.

`POST /api/v3/access-grants/transport-sessions` is the internal adapter bridge.
It repeats the same exact operation, constraint, repository, native-grant and
policy revalidation, then transactionally appends an invocation and a maximum
15-minute `access_grant_transport` Session. Default grants use the default
binding; an accepted actor-bound elevated grant uses that elevated binding while
the immutable Grant union remains the operation authority. The Session:

- authenticates existing Git and GitHub-compatible API adapters as the server-selected executor;
- emits the canonical workload subject `urn:multica:agent:<external-agent-id>`
  required by `workload.context.v1`;
- stores the canonical actor separately for commit and PR attribution;
- copies only credential-free provenance from the immutable execution-context
  snapshot;
- cannot represent `pr.merge`, which remains on the exact effect endpoint;
- for `repo.read`, admits the existing verification reads plus only exact `GET
  /api/v3/repos/{owner}/{repo}/branches/{safe-branch}/protection`; safe slash refs
  are accepted, while protection subresources, list/wildcard variants, unsafe or
  missing refs, non-GET methods, provider-direct routes, and other operations are denied;
- for numbered `pr.read`, additionally admits only the `PullRequestByNumber` GraphQL
  query emitted by `gh pr view`; a bounded parsed body must contain one query whose
  repository owner/name and positive PR number exactly match the Session repository
  and `pull_request_number` constraint, while mutations, subscriptions, fragments,
  alternate aliases, noncanonical variable definitions, extra variables/resources,
  malformed or oversized bodies, and fact mismatches are denied before resolver dispatch;
- expires no later than its parent Grant and is invalidated when that Grant is
  renewed, revoked, expired, or drifts at use time;
- is never cached or printed by the public CLI contract.

PR creation through this transport authorizes against the executor's current
repository grant but persists the canonical actor as the AGS PR author. Its
Multica projection link and optional initiator/originator presentation are
derived from the Grant's immutable snapshot; no second source assertion or JWT
is requested or embedded in the PR body. Missing non-authority linkage fields
degrade association only and do not widen or reject valid PR authority.

`GET /api/v3/access-grants/invocations/{id}` is restricted to the originating
grant or a verified renewed descendant in the same immutable Grant lineage. It
can read ordinary invocation receipts and perform GET-only recovery for an
already-dispatched provider effect even after the originating grant is expired
or revoked. Renewal never permits a second provider POST.

## Exact `pr.merge` Effect

`pr.merge` is not accepted by the generic authorize endpoint. It has one
server-owned effect adapter:

`POST /api/v3/access-grants/effects/pr.merge`

The supported client deterministically derives one canonical `invocation_id`
from repository, normalized AGS/provider PR numbers, expected head SHA, and
merge method；dry-run and apply therefore expose the same recovery locator.
Apply GETs that locator first: a matching receipt causes zero POSTs, only an
exact `403 grant_denied` permits at most one effect POST, and uncertain GET
transport/route results fail closed. The closed POST request contains that
locator plus AGS PR number, provider PR number, expected head SHA, and merge
method. AGS uses
the caller-owned UUID as the immutable invocation row ID and rejects reuse for
different facts. This makes GET-only recovery possible even when the provider
effect response is lost after server persistence. Before persistence and again
immediately before dispatch, AGS requires:

- active grant, actor, executor, source binding and exact authority revision;
- the selected executor's current native repository grant still covers the
  complete effective envelope;
- `pr.merge` in the effective envelope;
- exact enabled AGS repository and open non-draft PR;
- exact PR head plus the current AGS base ref; the current base may roll forward from creation-time metadata only when it remains an ancestor of the exact merge head;
- server-configured target, provider repository and merge method；base branch来自authoritative AGS PR，必须已纳入provider mirror policy；
- exact AGS-owned Forgejo projection and provider PR number;
- open, mergeable exact-head/exact-base provider PR；若provider暂报不可合并，只在open/head/base保持exact时有界GET重读，任何漂移或超时均在POST前失败；
- AGS-owned independent approval on the current head plus required current-head status policy;
- successful newest exact-head provider CI run per workflow;
- configured default base必须受保护、阻止direct/force push并显式授权integration bot；mirror-allowed非默认base不要求不存在的branch-protection whitelist，但仍要求server integration bot具备repository write collaborator authority。Provider-native protection仍是默认base的第二 enforcement boundary，不替代AGS policy。

AGS derives a global effect key from exact repository/AGS PR/provider PR/head
and method facts. The key is not scoped to a bearer, so renewal cannot repeat an
unknown effect. A definitive pre-dispatch exact-fact conflict is also persisted
under the caller-owned locator and returned as a terminal `conflict` receipt
with `provider_attempt=not_attempted`, `provider_outcome=not_attempted`, a
bounded `denial_code`, and `finished_at`. The same locator is immediately
GET-readable, so the supported client reports `not_merged` rather than an
unknown provider outcome. Locator reuse for different facts is invalid (422),
and a locator owned by another Grant is denied (403); the merge effect does not
use HTTP 409 after a provider dispatch may already exist.

Lifecycle:

```text
deterministic locator GET -> existing receipt (zero POST) | exact not-readable denial
  -> terminal pre-dispatch conflict + provider_attempt=not_attempted
  | planned
    -> dispatching + provider_attempt=attempted + provider_outcome=outcome_unknown
    -> exactly one server-owned provider POST
    -> completed | recovery_needed
```

`dispatching` is persisted before the provider call. After that boundary,
repeated POSTs, process recovery and invocation GETs perform exact provider
readback only. They never issue another merge POST. Only a merged provider PR
at the expected head with a canonical result SHA may become `confirmed` or
`reconciled_after_error`; drift becomes a terminal conflict receipt.

## Routes

| Route | Authentication | Effect |
|---|---|---|
| `POST /api/v3/access-grants` | current source token in closed body | issue bearer + receipt |
| `POST /api/v3/access-grants/renew` | grant bearer + fresh source token | rotate bearer, revoke old |
| `GET /api/v3/access-grants/current` | grant bearer | secret-free status |
| `POST /api/v3/access-grants/revoke` | grant bearer | revoke |
| `POST /api/v3/access-grants/authorize` | grant bearer | append low-risk invocation receipt |
| `POST /api/v3/access-grants/transport-sessions` | grant bearer | derive one exact internal Git / GitHub-compatible API Session + invocation |
| `POST /api/v3/access-grants/effects/pr.merge` | grant bearer | one exact server-owned effect attempt |
| `GET /api/v3/access-grants/invocations/{id}` | originating or renewed-lineage grant bearer | receipt / GET-only reconcile |

Every request body is closed and bounded. Errors are stable and secret-free.
A durable AGS profile token, provider token, source URL, or caller-selected
provider repository is not accepted on this surface.

## Availability Semantics

- invalid optional identity/class/operation requests degrade to the legal
  default and remain visible as warnings;
- unavailable AGS/source capability fails only the AGS operation, not the
  originating Runtime Task;
- source credential rejection creates no grant;
- actor, executor, native repository grant, repository, snapshot, policy or
  binding drift fails at use time;
- ordinary read/write authorization uses the bounded default envelope;
- merge/admin/destructive effects remain fail-closed and require explicit
  adapters;
- a provider outcome that cannot be proven remains `outcome_unknown`; it is
  never retried as a write.

## Proof Surface

Focused tests cover:

- trusted source/workspace policy resolution and ambiguity;
- JIT actor stability, actor/executor separation and absence of durable tokens
  or automatic repository grants;
- fail-soft selector/class/operation requests, native-grant-gated elevation and
  use-time denial after native grant revocation;
- canonical transport Context Envelope subject and default/elevated binding
  selection;
- bearer hash-only persistence, immutable authority rows and no-delete hooks;
- exact renewal, revocation, expiry and policy-drift rejection;
- closed AGS-token-free REST bootstrap, no-store responses, secret exclusion
  and OpenAPI coverage;
- generic invocation persistence/readback;
- Access Grant → internal transport Session issuance, executor authorization,
  canonical actor PR authorship, snapshot linkage/attribution, use-time drift,
  renewal/revoke invalidation, the exact operation-scoped `repo.read` branch-
  protection root-read allowlist with slash-ref and denial matrices, and the
  repository/PR-bound `PullRequestByNumber` GraphQL read with mutation and mismatch denials;
- exact merge head/base/mergeability/CI/protection gates;
- dispatch-state persistence before provider POST;
- one provider POST across duplicate calls, renewal, transport uncertainty and
  GET-only reconciliation.
