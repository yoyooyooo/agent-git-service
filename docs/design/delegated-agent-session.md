# Access Grant Transport Session

A transport Session is a short-lived adapter credential derived from an already-authoritative
Access Grant. It is not an independent authority source and cannot be created from a Multica
assertion.

## Authority chain

```text
fresh execution-context snapshot
  -> canonical actor + selected executor
  -> live native repository grant
  -> Access Grant
  -> exact operation invocation
  -> access_grant_transport Session
  -> adapter read/write
```

`POST /api/v3/access-grants/transport-sessions` accepts only the Access Grant bearer and one
closed non-merge operation request. The raw `ags_sess_*` bearer is returned once; AGS stores
only its digest. Session lifetime is bounded by both the resource policy and parent Grant.

## Identity separation

- The **executor** owns the Session principal and authenticates the adapter.
- The **canonical actor** remains a separate immutable parent-Grant fact and owns PR attribution.
- The workload subject is `urn:multica:agent:<external-agent-id>`.
- Workspace, Agent, Task/Run, optional Issue/Squad/Runtime/Trigger, initiator, and originator are
  provenance only. None can add repository authority.

## Use-time checks

Every adapter call reloads the Session, parent Grant, actor, executor, repository, policy, and
native grant. It requires:

- `credential_mode=access_grant_transport`;
- exact parent Grant ID and authority revision;
- active, unexpired, unrevoked Session and Grant;
- exact repository, operation, capability, and operation constraints;
- current canonical actor/executor/source coordinates;
- sufficient live native repository permission and matching native-grant revision;
- no retired merge-delegation fields.

Any legacy/pre-contract/assertion-backed credential mode fails with
`credential_mode_invalid` internally and a generic credential denial externally. There is no
fallback to assertion exchange, durable profile, provider token, or another principal.

`pr.merge` is forbidden on transport Sessions. Exact merge authority and the one provider-effect
state machine live under `POST /api/v3/access-grants/effects/pr.merge` and Grant invocation
readback.

## Operation-scoped adapter surface

A `repo.read` transport Session retains the existing verification reads and additionally admits
only `GET /api/v3/repos/{owner}/{repo}/branches/{safe-branch}/protection`. The middleware reuses
the exact repository path shape and safe head-ref validator, so named and slash refs are accepted
while missing, wildcard, traversal-like, lock-suffixed, or otherwise unsafe refs are denied.
Branch lists, protection subresources, trailing-slash variants, non-GET methods, provider-direct
routes, and the same path under every other operation remain outside the Session surface. This is
only route admission: the service still revalidates the exact Session repository, parent Grant,
operation, policy, and native repository grant, while the existing branch-protection handler owns
production read semantics.

## Lifecycle and readback

Dynamic Session status and revoke surfaces are retired. The issuing adapter validates the closed
transport receipt and then proves the bearer only through its exact allowed Git or GitHub-compatible API operation;
normal lifecycle is owned by the parent Grant. `GET
/api/v3/agent-sessions/{session_id}/lifecycle` is durable-site-admin-only historical readback; it
exposes no bearer, hash, provider credential, assertion, operation constraints, or provider-effect
proof. Parent Grant renewal/revoke atomically revokes all derived Sessions.

Authority-boundary receipts prove only fresh local AGS admission and lineage. They do not prove
provider success or cross-system atomicity.

## Retired surfaces

The following are absent and must return `404`:

```text
POST /api/v3/agent-sessions/exchange
GET  /api/v3/agent-sessions/current
POST /api/v3/agent-sessions/current/revoke
POST /api/v3/agent-sessions/{session_id}/revoke
POST /api/v3/repos/{owner}/{repo}/pulls/{number}/actions/pr.merge
GET  /api/v3/repos/{owner}/{repo}/pulls/{number}/actions/pr.merge/{intent_id}
```

Workload assertion verifier configuration, assertion secrets, assertion replay state, signed
merge delegation, Multica delegation introspection/consume/effect calls, and legacy Session
operation fallback are not runtime capabilities.

## Source anchors

- `internal/service/access_grant_transport.go`
- `internal/service/delegated_session.go`
- `internal/service/delegated_session_use_time.go`
- `internal/service/access_grant_merge.go`
- `internal/service/access_grant_merge_runtime.go`
- `internal/middleware/auth.go`
- `internal/router/router.go`
