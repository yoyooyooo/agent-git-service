# Execution Context Intake

## Status

Implemented by `internal/executioncontext`, `service.IntakeExecutionContext`, and
`POST /api/v3/execution-context/intake`.

This is the bootstrap fact-acquisition layer consumed by the AGS-owned
canonical actor and Access Grant evaluator. The standalone intake route does
**not** issue an AGS identity, credential, Session, operation, repository
permission, or provider effect. See [Canonical Actor and Access Grants](access-grants.md).

## Authority Split

```text
Runtime process
  owns: current source locator + current task-scoped source bearer

Execution-context source (currently Multica)
  owns: workspace / Agent / Task / Run / Issue / Squad / Runtime /
        Trigger / Attribution facts and current-running validity

AGS connector registry
  owns: stable source identity, accepted runtime aliases, fixed egress,
        workspace mapping, adapter, timeout

AGS service + DB
  owns: one immutable credential-free source snapshot, source ref, digest
```

The source describes execution facts. The Access Grant layer decides canonical
actor, repository scope, effective operations, grant lifetime, and effects.
Environment variables and source context never grant AGS authority by
themselves.

## Request Flow

```text
POST /api/v3/execution-context/intake
  -> strict 16 KiB closed JSON decode
  -> validate canonical workspace / Agent / Task UUID locator
  -> resolve exactly one configured connector
  -> create GET only to that connector's fixed egress_endpoint
  -> send current source bearer in one Authorization header
  -> reject redirects
  -> require HTTP 200 + JSON + Cache-Control: no-store
  -> decode the closed multica.current-execution-context.v1|v2 adapter
  -> require running Task (+ v1 Run status/attempt contract) and exact locator equality
  -> canonicalize context JSON and calculate sha256 digest
  -> discard request bearer
  -> persist immutable credential-free snapshot
  -> return snapshot id, source ref, digest, and normalized context
```

The route is unauthenticated at the AGS-token layer because it is the bootstrap
path for an ordinary Runtime that does not yet have an AGS credential. The
source bearer authorizes only the fixed source read; intake itself grants no AGS
capability.

## Connector Registry

The registry is file-backed under `execution_context` in
`AGS_INTEGRATIONS_CONFIG`:

```yaml
execution_context:
  enabled: true
  connectors:
    - source_instance_id: multica-mini
      adapter: multica_current_execution_context_v1
      accepted_runtime_endpoints:
        - http://primary.example.test:37134
        - http://127.0.0.1:37134
      egress_endpoint: http://multica-backend:8080/api/integrations/current-execution-context
      timeout: 5s
      workspace_mappings:
        11111111-1111-4111-8111-111111111111: mini
```

Contracts:

- `source_instance_id` is stable and unique. Runtime URL aliases do not change
  source identity.
- `runtime_endpoint_hint` is matched only against
  `accepted_runtime_endpoints`. It never becomes an outbound URL.
- `source_instance_id`, `runtime_endpoint_hint`, or both may select a
  connector. When both are present they must resolve the same connector.
- zero matches fail; multiple matches fail; the registry never picks the first
  candidate.
- accepted runtime endpoints are HTTP(S) origins only: no path, userinfo,
  query, or fragment.
- `egress_endpoint` is operator-controlled startup configuration and must be an
  HTTP(S) URL whose exact path is
  `/api/integrations/current-execution-context`; request data cannot alter it.
- only adapter `multica_current_execution_context_v1` is currently supported.
- `workspace_mappings` is required and is an allowlist. The returned workspace
  must be present; its mapped value is source-local routing metadata, not an AGS
  workspace object or grant.
- overlapping accepted aliases remain a runtime ambiguity and fail before any
  outbound request.

## Source Credential Boundary

The current Multica `mat_*` is request-scoped:

- accepted only as `source_token` in the standalone intake body or the Access Grant issue/renew bodies that invoke the same intake boundary;
- passed once in memory to the fixed connector egress as
  `Authorization: Bearer ...`;
- never written to DB, cache, filesystem, log, trace, receipt, or response;
- never hashed or fingerprinted for later authentication;
- never included in an error or upstream response snippet;
- never forwarded across a redirect or an ambient process HTTP proxy;
- required again for every later intake or grant renewal.

The implementation also rejects a normalized source response containing the
exact submitted bearer. Go cannot promise physical zeroization of every
short-lived string copy, so the enforced contract is bounded request lifetime
and zero durable/observable retention rather than a false memory-forensics
claim.

## Closed Source Adapter

The current adapter accepts schema
`multica.current-execution-context.v1` and dual-reads `multica.current-execution-context.v2`.
v1 keeps the richer display/run attempt contract; v2 is minimal and requires `claim.generation`
(with `run.id` dual-read alias). Unknown or trailing JSON fails. Shared requirements:

- exact workspace, Agent, and Task IDs equal to the caller locator;
- `task.status=running` and `task.attempt >= 1`;
- valid RFC3339Nano `observed_at`;
- non-empty attribution source;
- a mapped workspace when mappings are configured;
- canonical claim/run generation UUID and claim/run task consistency.

v1-only:

- `run.status=running` and `run.task_id=task.id`;
- coherent attempt/max-attempt facts on task and run;
- display enrichment fields may be present.

v2-only:

- `claim.generation` required on the wire;
- display enrichment (names, emails, titles, timelines, details_available, run status/attempt)
  is rejected so the dual-read struct union cannot re-admit v1-only fields;
- v1 responses may project `claim` onto the normalized snapshot as an additive dual-read field
  (canonical digest therefore includes `claim` after upgrade).

HTTP 401/403 becomes `source_credential_rejected`. A terminal or revoked
Multica Task therefore cannot renew after its task token stops being valid.
Network/non-200 failures are source-unavailable; redirect and schema/contract
failures are distinct secret-safe failures.

## Persistence

`execution_context_snapshots` stores:

- schema `ags.execution-context-snapshot.v1`;
- stable `source_instance_id`;
- canonical `ags.execution-context-source-ref.v1` JSON;
- source context schema;
- `sha256:<hex>` digest of canonical context JSON;
- canonical context JSON;
- indexed external workspace/Agent/Task locators plus Run/Issue/Runtime refs;
- source observation time and AGS creation time.

The model rejects GORM update and delete hooks. It contains no source bearer,
bearer hash, request header, runtime alias, or egress topology. An Access Grant
references the snapshot ID; it must not mutate or reinterpret the stored source
facts as source-issued AGS authority.

## Failure and Availability Semantics

- invalid/unmatched selector: no outbound request;
- ambiguous selector: no outbound request;
- redirect: no redirected request and no credential forwarding;
- source credential rejected or terminal: no snapshot;
- source unavailable or AGS unavailable: only the AGS capability path fails;
  the originating Runtime/Multica Run is not cancelled or blocked by this
  component;
- persistence failure: no intake receipt;
- intake success: proves only source-fact acquisition and immutable storage.

## Non-Goals

The standalone intake component does not:

- copy the Multica Task database;
- make Multica Workspace or Issue an AGS core object;
- read `AGS_AGENT_ID`, `AGS_POLICY_CLASS`, or `AGS_OPERATIONS` as authority;
- JIT-create canonical actors or calculate an operation envelope;
- issue/renew an Access Grant or delegated Session by itself (the Access Grant
  orchestrator calls intake and then owns those later steps);
- call Git, Forgejo, GitLab, merge, admin, or provider effect surfaces;
- replace existing compatibility Session paths before the later cutover gate.

## Proof Surface

Focused tests cover:

- fixed-egress request construction;
- unmatched, path-alias, selector-disagreement, and ambiguous resolution;
- redirect rejection with zero sink calls;
- credential and reflected-body exclusion from errors;
- terminal, locator drift, unmapped workspace, and unknown-field rejection;
- unsafe registry configuration;
- immutable DB persistence without the bearer or its authenticating hash;
- unauthenticated-at-AGS router intake, closed request body, OpenAPI coverage,
  and startup registry wiring.
