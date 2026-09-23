# Test Strategy

This document is the execution plan for increasing confidence in `agent-git-service`.
The current direction is bottom-up:

1. strengthen package-level and domain-level tests first
2. add real HTTP integration tests through router and middleware
3. keep gh CLI compatibility tests and shell E2E tests as the compatibility and end-to-end layer

The first goal is not broad coverage for every endpoint.
The first goal is confidence in the main user paths and the highest-risk behavior.

For the current dependency seams and acceptable concrete couplings, see `docs/module-contracts.md`.

## Upstream/Fork Reconciliation Gate

[Fork governance](../fork/README.md) separates upstream generation review,
source verification, publication and deployed releases. The main upstream
fixtures use isolated real TiDB schemas; fork SQLite runtime compatibility must
also be tested explicitly and cannot be inferred from a TiDB pass.

CI shards are checked by `scripts/check-ci-package-coverage.sh`: every root-module
package must appear exactly once, including fork integrations and Edge packages.
That inventory check is static coverage, not a claim that tests passed.

Preserve real foreign keys in normal fixtures. Intentionally orphaned historical
rows use `testdb.MutateWithoutForeignKeys` on one pinned test connection, with
restoration verified before service readback. Maximum-ID fixtures are discarded
from the pool so auto-ID high watermarks cannot contaminate later tests. Cleanup
all registered GORM failure callbacks. Timestamp assertions compare stored
precision, not the pre-insert Go clock value. Never weaken authorization checks
to accommodate invalid fixtures.

For concurrent idempotent operations, require one durable record, exact payload
agreement and no repeated provider effects under both SQLite and TiDB isolation.
A targeted pass after a failing full run is not a replacement for the final full
regression. Pin the complete run to one clean commit and retain its exit status;
source promotion is separate from deployment or shared protected-ref rewriting.
Upstream's retired interfaces require scoped consumer/migration checks before
upgrading affected consumers or changing a running server.

The fork's legacy SQLite upgrade gate must start from an actual old schema,
not only a fresh database. `scripts/upstream-sqlite-compat.py` exports two exact
source revisions, creates only synthetic credentials/core records with the old
version, and checks the new migration's identities/content hashes and integrity:

```bash
python3 scripts/upstream-sqlite-compat.py \
  --previous "${PREVIOUS_SOURCE_SHA:?select a retained prior release}" --candidate HEAD \
  --output "${RUN_EVIDENCE_DIR:?select a new private evidence directory}"
```

The output directory must not exist. The harness retains its evidence and does
not use live data. `TestForkSQLiteLegacyWikiColumnCleanupPreservesRowsAndIndexes`
separately protects child rows, unrelated indexes and exact slug/link values;
SQLite table recreation must not silently cascade-delete dependent rows.

Under concurrent merge intent creation and dispatch, lock grant, PR, projection
and invocation in the same order, recheck expiry after waiting, and assert one
provider POST even on lost responses. Database retries never wrap provider
side effects. Concurrent device-code exchanges must converge to one token and
must not rely on a stale repeatable-read snapshot after a conflicting upsert.

Hosted generation/PR CI runs the full root package matrix, client contracts and
selected race suites. Preparation pushes run a smaller explicit gate; skipped
matrix jobs do not count as passing tests. Public-source preparation also checks
complete reachable history and commit metadata, not only checked-out files.
The publication scanner and exact exception policy have disposable-history and
synthetic-credential regression tests under `fork/scripts/`.

Device-code exchange must revalidate the current approver before token writes.
Request spooling must stop on cancellation and remove its temporary file.
Client route planning must remain read-only, and rollback receipts may name only
the exact URL-scoped keys the tool owns. These repaired boundaries have direct
regressions and do not substitute for complete release verification.

## Priorities

The major paths to protect first are:

- discovery and auth bootstrap flows (`/api/v3/`, `/api/v3/meta`, `/api/v3/rate_limit`, token login, `gh auth login --with-token`, and `gh auth setup-git` / Git credential setup)
- token auth, embedded auth, and context-scoped DB correctness
- organization collaboration-governance flows (explicit org creation, org invitations, team-repo grants, outside collaborator lifecycle, effective permission precedence)
- repository create, clone, fork, transfer, and delete
- issue create, edit, comment, label, assignee, and search flows
- pull request create, view, diff, review, and merge flows
- release create plus asset upload and download
- workflow dispatch, rerun, cancel, logs, and artifact flows

These priorities are intentionally GitHub-compatible API-server centric. gh CLI compatibility is still a first-class compatibility target, and Git transport tests matter because repository and PR flows depend on real clone/fetch/push behavior.

## Principles

- prefer fast, deterministic tests close to the logic
- use real DB and Git behavior for merge, diff, and lifecycle flows
- test current architecture honestly instead of designing around an interface boundary that does not exist yet
- use gh CLI acceptance tests to validate CLI compatibility, not as the first line of defense for every edge case
- add breadth after core-path confidence is in place

## CI Layers

The repository now follows a layered merge gate adapted to what is stable today.

### Layer 1: Regression Gate

Run with:

```bash
bash scripts/regression_gate.sh
```

This gate is intentionally small and deterministic.
It is the merge-blocking pack for fast feedback, not the place to dump every existing suite.

Its responsibilities are:

- compile the backend tree with `go test ./... -run '^$'`
- compile the vendored `cli/` tree and `cli/_go-gh-local` without turning the known-flaky full CLI suite into a blocking behavior gate
- run the stable package tests listed in `scripts/regression_gate.list`
- run `scripts/wiki_regression_gate.sh`, which protects the stale wiki tree/search/backlink projection class without promoting the full service package into the fast gate

Promotion rule: when a bug fix or repaired area needs recurring protection, add a stable test to the package tree and then promote that package or script into `scripts/regression_gate.list`.

Remote wiki console checks are not part of the deterministic local gate. Use `scripts/wiki_remote_integrity.sh` during deploy verification to crawl tree-emitted page URLs and validate search/backlink page URLs against the running service.

### Layer 2: Integration Tests

Run with:

```bash
bash scripts/integration_tests.sh
```

This gate makes the stable real-router integration pack merge-blocking.
It is intentionally package-oriented: the same command should run locally and in CI without requiring a pre-started server.

Its responsibilities are:

- run `./internal/testharness`
- run `./internal/router`
- run `./internal/githttp/...`
- run focused `./internal/forgejointegration` exact force-with-lease, policy ceiling, remote-race, post-push verification, and exact-number PR read regressions
- run focused `./internal/service` provider-policy plus AGS-owned exact `pr.rebase` intent success/failure/base-race/conflict regressions backed by real AGS and local Forgejo bare repositories; cover complete-fact idempotency, PR-row-lock concurrent admission, duplicate-active fail-closed selection, direct-label denial, complete live AGS/Forgejo coordinate/head/base drift, expiry, canonical SHA request→durable→POST/GET receipt→restart-recovery byte-exact round trips, whitespace/uppercase/non-40/null rejection before intent/provider writes, same-generation restart recovery, and secret redaction
- run the mandatory Agent Kit producer bridge against the checked-in, checksummed accepted runtime transport evidence: verify the accepted AGS/Agent Kit source revisions, original issuance window, source/output SHA-256 values, and deterministic reversal of only documented client normalization before Agent Kit's production `normalizeReceipt` parser. This proves one historical production producer/consumer wire shape; it does not prove current deployment or live readiness.
- run the exact Access Grant REST hard-cut tests and validate their selectors exist before execution: real source-token bootstrap, immutable execution-context snapshot, canonical actor/executor split, closed/no-store/secret-safe Grant and transport receipts, exact operation/constraint OpenAPI variants, parent-Grant-bound transport use, and retired-route absence.
- run Access Grant `pr.merge` effect regressions: deterministic caller-owned canonical invocation locator shared by dry-run/apply；GET-first zero-POST recovery for an existing receipt and fail-closed uncertain GET；globally unique exact-effect admission; persisted GET-readable terminal `provider_attempt=not_attempted` receipt for pre-dispatch method/base/ancestry conflict; no HTTP-409-class conflict for locator collisions after dispatch; persisted `dispatching + outcome_unknown` before provider I/O; concurrent same-ID/exact-effect callers under both confirmed and unknown provider outcomes produce one provider POST；monotonic base roll-forward accepts a rebased head that contains the live current base without mutating the historical PR creation `base_sha`, while a stale head persists a GET-readable terminal conflict and produces zero provider writes；zero provider writes on authority/repo/PR/head/base/CI/protection drift; no second POST after timeout or unknown; GET-only reconciliation through the originating or renewed-lineage Grant; and exact confirmed/reconciled provider receipts only. These tests prove local source/DB-CAS behavior, not live TiDB lock timing, real network timeout state, multi-process fault injection, or Forgejo rollout.
- run focused projection-integrity/outbound regressions for exact rechecks of stale paginated PR candidates, same-transaction intent creation, active-generation dedupe, silent convergence before active notification, fresh throttling for recurrent generations, paired resolved alerts, explicit missing-config outcomes, retry/dead-letter transitions, and credential redaction
- run durable rebase-generation regressions covering exact `action_intent_id`/generation binding, old-job versus new-intent concurrency, terminal-denial protection against every late phase writer, duplicate-label/startup resume without a second rebase, automatic success-comment lease-expiry takeover without attempt consumption, interrupted rebasing/pushing/PR-verification phases, terminal unknown remote drift, base movement, DB-CAS claims, one success comment, status facts, and nullable historical jobs failing closed
- run Forgejo authority plan/apply/verify regressions for native-update, base protection, integration-bot whitelist, labels, webhook convergence, idempotent retrofit, required alert target validation, and `/readyz` failure/degraded states
- run the router-level rebase incident acceptance test through signed webhook, real database/service/AGS bare Git/local Forgejo bare Git, controlled Forgejo API, durable exact-intent job resume, retrying capture outbound, and resolved notification; new intents must match their own live AGS/Forgejo head/base facts and cannot adopt a pre-job legacy or newer head as recovery, while only the same bound generation may reuse its persisted accepted-old-SHA lease; also guard all-scheme credential URL redaction
- run focused `./internal/notifications` Feishu success/frequency-limit classification and typed Multica terminal-dispatch regressions: state-fixed `/complete-from-merge` versus `/links`, exact closed `ExternalPRLinkRequest` body, exact target type, origin-only config validation, idempotency header, and no token/payload leakage
- run AGS durable Multica terminal delivery regressions: merge/close fact plus OutboundDelivery transaction rollback, repeated terminal-fact row repair, merged-SHA conflict, deterministic duplicate key, database-clock lease CAS, typed-only expiry reclaim, bounded retry/dead-letter, secret-safe wrapper, and no raw URL/provider fallback
- run `./internal/executioncontext` fixed-egress, unmatched/ambiguous/alias, redirect, closed-adapter, terminal, workspace-map, timeout/config, and credential-leakage regressions; service/REST tests additionally prove immutable credential-free persistence and the AGS-token-free bootstrap route
- run the stable integration subset inside `./internal/rest`
- run the stable integration subset inside `./internal/graphql`

### Layer 3: Runtime Smoke Gates

Run with:

```bash
bash scripts/backend_smoke.sh
make test-migrate-tidb
```

These gates prove the built server still works as a real process, not just as isolated packages.

- `scripts/backend_smoke.sh` is the low-dependency startup smoke:
  - boot the server binary with a fresh TiDB playground database
  - verify `/readyz`, `/api/v3/`, `/api/v3/meta`, and `/api/v3/rate_limit`
  - create a repository through the REST API
  - verify authenticated Git Smart HTTP against the live server
- `make test-migrate-tidb` is the TiDB migration smoke:
  - start the test-only TiDB playground
  - create a fresh temporary TiDB database
  - boot the server against that TiDB DSN and wait for `/readyz`
  - fail early on TiDB-incompatible migration or bootstrap DDL

The smoke layer should stay small enough to run on every PR, but realistic enough to catch wiring and startup regressions the regression gate cannot see.

### Layer 4: Compatibility And E2E Gates

Run with:

```bash
make test
make test-e2e
```

These gates cover the higher-fidelity compatibility and shell E2E paths that are stable enough to be merge-blocking today.

- `make test` boots against a live TiDB-backed `github.localhost` server and runs the vendored gh CLI compatibility inventory
- CI mirrors the local acceptance path with the test-only TiDB playground: `make test-setup`, a Docker access verification step (`docker --version` plus `docker run --rm hello-world`), `ENABLE_WORKFLOW_EXEC=1 make run-bg`, `make test`, then `make test-clean-all`
- `make test-e2e` runs the full shell E2E inventory under `e2e/`
- the full E2E inventory includes `e2e/repo-transfer-lifecycle.sh`, so repo transfer remains merge-blocking without a separate standalone smoke job
- the full E2E inventory also includes `e2e/agent-auth-flow.sh`, which now asserts the agent-binding confirm contract for canonical agent tokens plus the human-token `403`, invalid-invite `422`, and consumed-invite `409` failure paths
- CI mirrors the local full E2E path too with the test-only TiDB playground: `make test-setup`, `ENABLE_WORKFLOW_EXEC=1 make run-bg`, `make test-e2e`, then `make test-clean-all`
- `make run-bg` waits up to `STARTUP_WAIT_SECONDS` seconds for `/readyz`; the default intentionally leaves enough room for first-start TiDB schema migration in the compatibility and E2E test lanes

#### OAuth Device Flow E2E

`e2e/oauth-device-flow.sh` is the merge-blocking shell regression for the live OAuth device bootstrap path.
It keeps the runtime contract executable across the full device flow:

- `POST /login/device/code` returns a device code, user code, verification URI, complete verification URI, and the expected `expires_in` and `interval` polling metadata
- authenticated device approval through both the headless console API and built-in `/login/device` fallback plus `POST /login/oauth/access_token` exchange yields a usable bearer token, with an optional `gh auth` verification step when the CLI is installed
- `GET /login/oauth/authorize` accepts same-origin and loopback redirect URIs while rejecting cross-origin callbacks
- malformed, missing, and form-encoded device-code exchange requests fail or recover in the expected way, including repeated polling after approval
- success and error payloads do not leak issued access tokens or device codes

### Layer 5: Documentation Gates

Run with:

```bash
python3 scripts/doc_lint.py
bash scripts/check-module-contracts.sh
```

The documentation gate is enforced in GitHub Actions and can also be run
locally. Its purpose is to stop process drift:

- doc-lint catches stale workflow inventories, broken internal links, and missing CI/test docs
- doc-lint also runs `scripts/check-module-contracts.sh` so every top-level `internal/*` package must have a contract entry in `docs/module-contracts.md`

### Workflow Execution Testing

Workflow execution is tested at multiple layers:

**Unit tests** (`internal/service/workflow_exec_internal_test.go`):
- `writeWorkflowSandboxFiles` - validates sandbox script and launcher creation
- `buildWorkflowLauncherScript` - validates environment variable injection and script generation
- `parseWorkflowEnv` - validates environment variable parsing and validation
- `shellQuote` - validates shell escaping for environment values

**Integration tests** (`internal/service/workflow_dispatch_exec_test.go`):
- Full workflow dispatch and execution flow
- Docker sandbox isolation verification
- Artifact creation from workflow steps
- Secret injection and environment resolution
- Timeout and cancellation behavior
- Artifact path traversal rejection (security hardening)

**Coverage tracking** (`internal/service/workflow_coverage_test.go`):
- Tracks coverage across workflow-related code paths
- Ensures workflow execution surface changes are visible in coverage reports
- Deadline-exceeded artifact collection handling

The workflow execution sandbox is **fail-closed by default** and requires `ENABLE_WORKFLOW_EXEC=1` to activate.
Tests verify both the disabled state (workflow runs fail immediately with audit logging) and the enabled state (steps execute in isolated containers).

## Current Reality

Today the repository already has useful tests in:

- `internal/service` (including `auth_test.go` for auth service flows)
- `internal/gitstore`
- `internal/graphql`
- `internal/middleware` (including `auth_test.go` for middleware auth)
- `internal/oauth`
- `internal/rest/respond`
- `internal/rest/transform`
- `internal/rest` (dedicated handler tests: `handlers_branch_test.go`, `handlers_dependabot_test.go`, `handlers_deployment_test.go`, `handlers_gist_test.go`, `handlers_webhook_test.go`, `handlers_webhook_delivery_test.go`, `pagination_test.go`)
- `internal/router` (router-level integration tests in `router_test.go`)
- `config`
- `internal/oidc`
- `internal/embedding` (including `embedder_test.go`)
- `internal/executioncontext` (fixed-egress connector registry and closed source adapter)
- `internal/apperrors`
- `internal/testharness` (reusable HTTP integration test harness with smoke tests)

The main gaps are:

- limited coverage for real Git merge, rebase, compare, and diff behavior
- real Docker-sandbox workflow execution still depends more on CI and smoke gates than on direct package tests
- too much reliance on end-to-end acceptance tests for HTTP-path confidence
- targeted collaboration-governance coverage now exists, but it needs to stay part of the routine regression path rather than living only in one-off branch work

Workflow execution sandbox coverage was added in 2026 with:
- unit tests for sandbox file writing, environment parsing, and shell escaping
- integration tests for full workflow dispatch and Docker-based step execution
- coverage tracking across workflow-related code paths
- security hardening tests for artifact path traversal and timeout handling

The repo also has focused shell end-to-end coverage under `e2e/`, including org collaboration governance and code search. Keep those flows in the normal regression toolbox when a change spans multiple endpoints and is awkward to express through one package test. The code search flow also creates repository contents through the GitHub-compatible Contents API, so it should keep asserting the GitHub create status contract while protecting repository-scoped search boundaries.

The main CI workflow runs the full backend inventory through `make test-unit`, with an explicit, overridable `GO_TEST_TIMEOUT` (default 20m). An authenticated Human may merge after sufficient local evidence while provider CI continues asynchronously. A complete exact-SHA CI result remains mandatory before release, deployment promotion, Ticket closure, or a final acceptance claim.
CI shards that inventory with `GO_TEST_PACKAGES` so each shard has an isolated TiDB playground instead of forcing the slowest database-backed packages to contend for one local TiDB instance.
The vendored `cli/` inventory remains outside the routine backend lane because it is slower and more environment-heavy.
The correct pattern is:

- run focused tests plus compile/vet locally before an early Human merge
- keep `make test-unit`, compile-all, and stable repaired packages in the asynchronous exact-SHA CI inventory
- keep a live smoke layer for real process validation
- require the complete inventory before final release/acceptance claims rather than serializing every small correction behind it

To inspect the current test inventory instead of hard-coding counts:

```bash
find internal -name '*_test.go' | sort
(
  cd cli
  go test -tags acceptance ./acceptance -list '^Test'
  find acceptance/testdata -name '*.txtar'
)
```

## Phase 1: Package and Domain Tests

This is the highest-priority phase.
It gives the fastest feedback and protects the most critical business rules.

### Existing Building Blocks

The repo already has useful helpers such as:

- `internal/testharness/service_fixture.go:NewService` — the canonical
  service-layer fixture. Returns a bare `*service.Service` wired to an
  isolated TiDB playground database migrated via `db.Migrate` (production parity)
  and an isolated gitstore. Accepts `ServiceConfig` with knobs for
  connection caps and a custom `Embedder`.
- `internal/service/service_test.go:setupTestService` — 1-line wrapper
  over `testharness.NewService{}`, kept for call-site compatibility across
  hundreds of existing service-layer tests.
- `internal/service/service_test.go:setupRepoForTest`
- `internal/gitstore/store_test.go` temp-repo setup patterns

New tests should call `testharness.NewService` directly or go through
`setupTestService`; avoid re-rolling TiDB+migration bootstrap inline.

### First Batch

#### Git and Repository Foundation

Start with the parts that every PR, release, and workflow flow depends on:

- `internal/gitstore`: `Merge`, `Rebase`, `Compare`, `DiffNameStatus`, `DiffNumStat`, `ReadFile`, `ListTags`, `LogBetweenTags`, `PRCommitsLog`
- `internal/service/repo_test.go`: duplicate repo names, transfer, fork behavior, delete cascade, repo emptiness, disk usage

#### Pull Request Lifecycle

Add direct service and Git coverage for:

- create PR from valid branches
- reject invalid same-branch or invalid-state requests
- list PR commits and files
- request reviewers and prevent duplicate review requests
- delegated actor attribution from immutable session snapshots, including live revoke/expiry state and durable-PR null compatibility
- merge with merge, squash, and rebase strategies
- conflict and already-merged paths

#### Issue Lifecycle

Add coverage for:

- create with labels, assignees, and milestone
- close and reopen behavior
- list and filter by state, label, and assignee
- search with multi-qualifier queries

#### Auth and OAuth

Add direct service tests for:

- dev-mode token behavior when the token table is empty
- valid and invalid token resolution
- device-code exchange paths
- user resolution by token
- generic OIDC discovery, device-code exchange, and local-user/token creation flows
- connected login code exchange, userinfo validation, local identity linking,
  and human versus agent user-kind mapping

#### Auth and Context-Scoped DB

Add direct package tests for:

- token resolution across missing, invalid, and valid token paths
- `service.DBForCtx(ctx)` request, transaction, and background propagation rules

#### Collaboration Governance and Authorization

Add direct service tests for:

- explicit org creation and org-owner bootstrap
- organization invitation create, accept, decline, revoke, and expiry paths with GitHub-compatible invitation roles (`direct_member`, `admin`)
- role-aware org membership and team membership behavior
- effective repository permission precedence across org base permission, direct collaborator grants, and team grants
- outside collaborator reconciliation after invite acceptance, collaborator removal, repo deletion, and repo transfer

#### Workflows and Releases

Protect the most common lifecycle paths:

- sync workflow definitions from repo contents
- dispatch, rerun, cancel, and list workflow runs
- create releases and upload or download assets

### Phase 1 Output

By the end of this phase, the service layer and Git layer should be trusted for the main CLI-facing behavior before any HTTP integration expansion.

## Phase 2: HTTP and Surface Integration Tests

This phase should use real service dependencies and exercise the real router and all primary HTTP-facing surfaces together:

- REST
- GraphQL
- OAuth
- Git Smart HTTP

### Why Real Integration First

REST handlers and auth middleware are currently wired to a concrete `*service.Service`.
That means mock-first handler tests are not the shortest or most honest path today.
Until REST and router are refactored to depend on injected interfaces, the recommended path is:

- isolated TiDB playground database
- temp gitstore
- real `service.Service`
- real `router.RegisterRoutes`
- `httptest` requests for REST, GraphQL, OAuth, and host-rewrite paths
- `httptest.NewServer` plus real `git` CLI calls for Git Smart HTTP paths

### Harness

The `internal/testharness` package provides a production-ready HTTP integration test harness. `testharness.New` builds on top of `testharness.NewService` (the shared service-layer fixture) and adds the full HTTP dispatch:

1. an isolated TiDB playground database migrated via `db.Migrate` (production parity)
2. a temp gitstore
3. a real `service.Service`
4. REST, GraphQL, Git HTTP, and OAuth handlers
5. the real router via `router.RegisterRoutes`

Service-only tests that do not need the HTTP surface should call
`testharness.NewService` directly.

The harness API is `testing.TB`-based rather than `*testing.T`-only, so the same wiring can be reused by package tests and benchmarks.
That allows hot-path benchmarks to exercise the real router with the same auth, DB, and gitstore setup used by the integration layer instead of maintaining a separate benchmark-only harness.

The harness supports both `httptest.NewRequest`-style tests and `httptest.NewServer` for real `git` CLI calls (clone, push, ls-remote) against an actual URL.
When a caller upgrades to `Server()`, cleanup is anchored to the root benchmark or test passed to `New()` so shared harness servers stay alive across sibling subtests or sub-benchmarks.

See `internal/testharness/smoke_test.go` for usage examples.

### First Integration Paths

Start with the highest-value flows for each primary surface.
Phase 2 is not complete until each surface has at least one core-path integration case.

#### REST

1. discovery endpoints with and without auth, explicitly covering `/api/v3/`, `/api/v3/meta`, and `/api/v3/rate_limit`
2. token auth failure and success paths
3. repository create and get
4. issue create, update, list, and comment
5. pull request create, view, list commits, list files, and merge
6. release create and asset upload or download
7. workflow dispatch and view
8. search basics
9. host rewrite behavior for `api.github.localhost`
10. explicit org creation through `/api/ext/v1/user/orgs` and GitHub-compatible org listing through `/api/v3/user/orgs`
11. organization invitation create/list/accept/decline/revoke flows, including pending-membership role rendering for `admin` invitations
12. outside collaborator listing and collaborator annotations on org-owned repos
13. team-repo permission alias compatibility, including canonical `read`/`write` decisions for `triage` and `maintain`
14. OIDC helper endpoints under `/api/ext/v1/oidc/*`
15. connected login helper endpoints under `/auth/connected/*`
16. embedded identity routing through middleware into service DB access
17. delegated PR create/get/list actor projection, stable-principal compatibility, historical snapshot stability, and authentication-material exclusion
18. execution-context intake without an AGS credential, proving strict request closure, fixed registered egress, no redirect forwarding, exact running locator match, immutable snapshot persistence, OpenAPI coverage, and source bearer/hash exclusion from DB/error/response

#### GraphQL

Add integration tests for the GraphQL paths that are important and currently under-protected:

1. authenticated repository or issue query through `/api/graphql`
2. pull request query that verifies review or diff-adjacent fields
3. `revertPullRequest` mutation
4. one ProjectV2 lifecycle path such as `createProjectV2` or updating a project item field
5. delegated PR `agsActor`/`delegatedBy` shape and `PullRequest` type introspection

These tests should hit the real GraphQL handler and field filtering path, not just resolver helpers.

#### OAuth

Add router-level integration tests for:

1. `POST /login/device/code`
2. `POST /login/oauth/access_token`
3. `GET /login/oauth/authorize` success and redirect validation failures

This ensures the device flow is covered at the integration layer rather than only through handler-local tests.

#### Git Smart HTTP

Add integration tests that use the real `git` CLI against a test server:

These tests should follow the real user path: discovery/auth setup first, then Git transport.

1. `info/refs` advertisement for an existing repository
2. clone or fetch via `git ls-remote` or `git clone`
3. push via `git push`, including post-push effects such as fixing `HEAD`, dispatching webhook deliveries, and syncing workflows
4. one real Access Grant-derived `access_grant_transport` credential across REST and Git, proving exact operation/repository/constraint binding, actor/executor separation, and denial outside its operation surface
5. parent Grant revoke/renew, Session expiry, and native-grant/policy drift replay against REST read, Git read, Git write, and PR create with the same generic authentication failure
6. transport Git writes append canonical, secret-safe audit rows for an active read-only denial, a successful branch update, and policy/no-ref-change denial
7. durable receive-pack branch safety: unprotected work-branch push succeeds while an AGS-protected branch rejects direct Git HTTP updates and points callers to the authoritative PR path
8. Access Grant transport receive-pack branch safety: ticket branch creation/fast-forward succeeds while upload-pack, default/protected branch update, branch deletion, and non-fast-forward replacement fail
9. durable-token clone/push and historical custom-ref behavior remain unchanged outside configured protected refs

REST branch-protection contract tests additionally verify that branch get/list responses project `protected=true` from the same exact protection rows consumed by Git HTTP, rather than returning a transport-local constant.

#### AGS Edge foundations and serving gates

Host-gateway extension adds real-Git unregistered clone/push/fetch without auto
registration, LFS/native passthrough, connection-refusal fallback but no HTTP
403 replay, periodic node-only warming and foreground authority checks. Loopback
diagnostics test redaction, bounded history, actual peer health and stale checks;
headroom tests reject producers and mid-stream writes at the configured threshold
without damaging old views (not an OS ENOSPC experiment). Run
`python3 scripts/test_edge_client_route.py` for reversible host-wide configuration,
rollback, URL matching and drift/unsafe-input tests. Existing restricted-mode,
force-update, revoke, corrupt-cache and uncertain-write gates remain unchanged.

Stages A/B1/B2 and C1's real packet-driven executable read path are implemented.
Operational readiness is still gated by primary-capture/total-disk/live rollout work,
not by an absent local reader. The tests prove these distinct boundaries:

- `internal/gitbackend`: explicit receive opt-in, query/service/method binding,
  CGI credential removal, protocol and primary policy environment preservation.
- `internal/githttp`: all existing regressions plus storage-free authorization
  and real native Git v0/v2 clone/push/fetch (`access_boundary_test.go`).
- `config/edge_test.go`: independent Edge environment/defaults and strict fixed
  origin validation; no required business DB or primary .env bootstrap.
- `internal/edge`: unwired upload-pack is blocked without touching the primary; writes
  and business requests preserve original credentials/results, reject ambiguous
  routing and identity assertions, detect loops, and never retry uncertain writes
  or follow upstream redirects. Liveness is not readiness. Cancellation drains
  an independently started listener with no reachable business database.
- `internal/edgeprotocol`: identity/phase/time validation plus bounded canonical
  manifests/export framing, full refs/HEAD content hashes, duplicate/conflicting
  refs and tamper rejection. Content hashes are not authority.
- `internal/snapshotstore`: actual packs, complete exact object closure, SHA-1/
  SHA-256, empty/detached HEAD, tags/PR refs/deletion, private staging/publication,
  source-independent retention, pinned eviction, restart/config/symlink checks.
- `internal/edge/replication_integration_test.go`: offline source capture, source
  deletion, real registered-mTLS export, Mirror import, then native v0/v2 shallow
  clone/unshallow against the local view. Multiple readers share one export.
  These are fixed admitted fixture views, not live ReadPlan authorization.
- `internal/edge/replication_failure_test.go`: real node certificate requirements,
  wrong key/store/authority/incarnation, bearer rejection, no redirect/proxy,
  bounded queue, independent waiter cancellation and blocked-stream shutdown.
- `cmd/ags-edge/main_test.go`: transitive dependency-graph gate forbids importing
  the primary bootstrap, business Service/DB, control plane or integration worker.
- `internal/gitstore/snapshot_barrier_test.go`: canceled real writes under capture,
  nested mutation lifetime/early outer release, queued capture, and fail-fast
  capture/mutation nesting. This is one owning process, not a distributed lock.
- `internal/edge/control_integration_test.go`: actual primary Service/DB, persisted
  identity, capture and fresh original-user authorization over real mTLS, then
  Mirror and native v0/v2 local clone. New discovery advances; exact retained fetch
  is not substituted. Rename preserves identity; delete/recreate cannot inherit
  cached data or an old peer grant. The test ingress still selects a fixed view.
- `internal/edge/control_failure_test.go`: token revocation after capture, native
  collaborator removal, disabled repository and effective global Git read policy.
- `internal/service/replication_read_test.go`: real Access Grant transport git.read,
  repo.read scope rejection, parent revoke, and policy-prefix binding even when an
  operator reuses the same label. Reordering identical prefixes is not drift.
- `server/replication_test.go`: the actual owning primary constructs its peer
  endpoint; public routes do not expose it, missing mTLS fails, and invalid
  construction releases the retained-store ownership lock.
- `internal/edgeprotocol/control_test.go`: typed discovery/fetch constraints and
  bounded, closed JSON with duplicate/case-folded keys and deep nesting denied.

C1 adds `read_integration_test.go`: actual Server/ReadRuntime, canonical unchanged
remotes using curloptResolve, real v0/v2 shallow clone/unshallow, push/immediate
fetch and an interleaved force-update with a second client's discovery. Git read
traffic must never hit the ordinary primary download route. Sync-time credential
revocation is denied before any Git data. Missing credentials trigger a working
native credential-helper challenge. `read_failure_test.go` covers unknown wants,
expired recent views, cached repository deletion and sync failure without fallback.
`git_request_test.go` covers framing, gzip/trailing data/size limits, unadvertised
commands and a fuzz target. Read-config tests load actual mTLS files and reject
unknown/duplicate fields or unsafe key permissions. `read_binary_test.go` builds
and starts the real executable, clones and terminates it, then reacquires the
cache to verify lifecycle cleanup.

Remaining live gates include broader Git workloads/capacity/restart scenarios,
live wiki identity/authority, reviewed operator/PKI rollout, topology
audit, and real two-site fault/performance acceptance.
E1 adds snapshotstore/incremental_test.go: exact missing objects, independent
publication after deleting the base, SHA-256, force-update/deletion/empty targets,
truncation/extra objects/base mismatch rejection. Retention tests use a controlled
clock to prove byte/count refusal, pinned/recent protection, idle collection and
restart grace; they do not claim OS-level disk quotas or power-loss testing.
edge/incremental_integration_test.go measures cold vs incremental pack bytes with
a real canonical-remote Git fetch and incompressible base data, tests restart base
reuse, explicit missing-base full transfer and substituted-base rejection. Config
tests prove retention cannot undercut the negotiation window or disable collection.
E2a adds snapshotstore/capture_test.go and pack_reuse_test.go: actual hardlinked
loose/packed SHA-1/SHA-256 objects, private capture non-exposure, source deletion,
unsafe source rejection/cancellation/lifetime, reused inode identity, base eviction
and restart, exact exclusion of deleted objects, combined pack caps, and no
snapshot-store mutex acquisition during source pinning. The pack sample measures
reused vs newly allocated pack contents, not network latency or an OS disk quota.
edge/capture_pause_test.go blocks the actual native pack-objects subprocess and
proves real primary writes/deletion finish before packing resumes. The selected
old view stays exact, later discovery sees new writes, and deletion fails final
authorization. Whole-history verification remains outside the barrier, while
metadata/ref enumeration and linking still pause writes; this is not a live pause
benchmark. No unmanaged concurrent filesystem writer is assumed safe.

The existing projection failure-classification fixture gives real Git/SQLite
preflight time to reach its injected error and asserts exactly one push call.
Its terminal-vs-retryable assertions are unchanged; a 50ms incidental worker
expiry must not replace the intended injected error under full-suite load.
Do not replace these gates with mock HTTP successes or treat notifications as freshness. Tests invoking zstd
require the installed executable in PATH (e.g. /opt/homebrew/bin on mini).
See [AGS Edge](architecture/ags-edge.md) for the dependency order and contracts.

E2b1 adds configured owning-primary listener acceptance. `server/replication_binary_test.go`
builds and launches gh-server with real certificate files, then exercises the normal
Edge and unchanged-remote Git clone/push/immediate read. SIGTERM and an occupied
peer port must release sockets and retained ownership. Runtime/config tests reject
stale store identities, impossible startup headroom, overlapping source/cache roots,
unsafe key/config permissions, certificate name mismatch and wildcard listeners.
Malformed file configuration must not create the business DB. Drain tests keep the
process lock while a handler remains alive after the shutdown deadline. Certificate
expiry tests cover a previously verified chain on a reused connection state.
`CheckCaptureFilesystem` tests actual hardlinks and probe cleanup; it is a startup
observation, not OS quota, continuous reservation or an ENOSPC simulation.

E2b2 adds native-admin enrollment and a DB-free operator executable. Actual
primary HTTP tests prove GET has no allocation effects, POST binds prior authority/
repository ID/creation time, exact repeats stay idempotent, rename survives and
same-name recreation cannot inherit stale enrollment. Tests cover disabled opt-in,
non-admin/delegated denial, duplicate/unknown JSON, closed OpenAPI and conflicting
primary authorities rejected before DB bootstrap. The command runs against the
real primary handler, while its transitive-dependency gate rejects DB/runtime
imports. Operator transport tests prove no redirect/retry or credential disclosure;
peer-plan tests verify certificate chain/time/clientAuth, key-not-name identity,
exact registered bindings and unapplied output. The topology audit is separate
point-in-time SSH/Docker evidence, not a test proving all live writers are managed.

#### Historical pre-hard-cut assertion/Session inventory

The numbered inventory below through item 23 records the removed assertion-exchange,
public Session lifecycle, and delegated-merge implementation. It is retained only to
explain historical evidence and must not be used as current test guidance; those routes
now return `404`, and the corresponding source tests were deleted.

1. two explicit issuer-instance/subject bindings can resolve N:1 to one immutable principal, while single-binding disable/revoke leaves the sibling binding usable
2. display name, role, workspace display value, Agent metadata, Issue, Task, Run, Trigger, Runtime, and credential mode never select principal or operation authority; canonical signed `workspace_id` is the sole workspace authority coordinate and must match its target-local team binding
3. the same subject/Task can reacquire `git.push` and `pr.create` Sessions without a policy change; each Session remains exact-resource, operation-scoped, and TTL-bounded
4. bad signature/audience/expiry/replay, unknown issuer/subject, inactive principal, missing native grant, insufficient grant, wrong target/repository, unsupported operation, and provider override fail closed without a partial Session
5. accepted Multica Session assertions require nested `workload.workload_context`, `workload.authority`, and top-level `scope`; after signature verification, the canonical top level and target/workload/actor/Context/authority/scope/resource/operation/constraint envelopes reject every unknown, legacy, wrong-typed, non-canonical, over-length, and secret-shaped field, including every present optional. Exact workload/Context subject, JTI correlation, provenance copies, resource/operation/capability links are verified; optional `requested_ttl` retains its original canonical positive single-unit form and is capped at 15 minutes. The exchange HTTP body rejects unsigned outer contract/resource/operation/capability/TTL repetitions and trailing JSON. `issuer_instance_id` selects the immutable trusted issuer record and its JWT `iss`/`kid` match is proven without treating `iss` as the ID. Optional `squad_id` is absent or a canonical lowercase hyphenated UUID, persists/read backs exactly, and cannot change team/principal/class/epoch/native-grant/operation authority
6. legacy delegation v1/v2 loads only as non-executable migration inventory; legacy mappings alone cannot renew or issue a new principal/session-v2 Session, while old active rows drain by existing TTL/revoke rules
7. `repo.read`, `pr.create`, and `git.push` remain operation-scoped across REST/Git; `repo.read` additionally admits only the exact branch-protection root GET for safe named or slash refs and denies protection subresources, trailing-slash/list/wildcard variants, unsafe or missing refs, non-GET methods, provider-direct routes, and unrelated operations; `git.push` admits only receive-pack discovery/execution and denies upload-pack, `ls-remote`, clone, and fetch; protected/default branches, deletes, non-fast-forward updates, repository permission, and exact operation constraints remain enforced. Missing/mismatched External PR association and provider projection eligibility do not reject an otherwise authorized AGS PR; no invalid association row is invented, and projection failure stays observable
8. exchange, operation, denial (including rejected delegated reads), expiry, and revoke audits carry principal/binding/issuer/context/resource/operation/lifecycle facts without assertion, bearer, verifier, fingerprint, signing, or cache material
9. one External PR JTI is idempotent only for the same PR request and cannot bind another PR
10. durable-profile PR creation remains compatible and leaves delegated Session provenance null; durable and delegated credentials for the same principal/operation share the same native-grant ceiling
11. delegated create/get/list and provider projections preserve the stable principal, expose only secret-safe workload/binding/session snapshots, and retain live Session state; a valid External PR assertion still provides JTI idempotency, while absent/invalid/mismatched association leaves the durable AGS PR linked only to its delegated Session provenance
12. active Session resolution requires the exact persisted binding revision; revoke → authority snapshot rollback/restart cannot resurrect the old bearer or reuse the revoked binding ID because the durable tombstone remains authoritative
13. canonical `workload.authority.v1` resolves two unknown Agent subjects and a new Squad member through one issuer/workspace/team/class binding without config mutation; wrong or conflicting signed workspace IDs deny, while Agent/Squad/role/name/prompt/skill and workspace display changes remain provenance only
14. canonical policy classes continue to govern the independent durable/delegated operations that use them; workload `pr.merge` is the deliberate single-role path selected only by `AGS_ACCESS_ROLE=maintainer|admin`, with both values adding exactly `pr.merge` and no other privileged operation
15. target/service `resource_defaults` derive an exact repository scope only after request normalization, never grant access without the principal's live native grant, reject unknown/disabled repositories, compute targetless effective candidates per target, let an exact row override only its same-target default, and fail closed for cross-target exact/default ambiguity
16. malformed/unknown authority v1, wrong signing key, wrong or conflicting workspace ID, disabled class, absent v1 without both explicit legacy configuration/assertion compatibility claims, stale/lower epochs, floor advance, replay, and restart/reload fail closed; issued Session lifetime never exceeds signed membership-proof expiry and receipts/audits remain secret-safe
17. canonical Session resolution checks the persisted team binding revision; a revision change invalidates old credentials, while `POST /api/v3/integrations/principal-sessions/epoch-floor` accepts only a site-admin's active issuer/team/class target, serializes with exchange/session persistence, and emits an auditable monotonic advance
18. secret-shaped (`mat_*`, `ags_sess_*`, JWT, private-key) authority configuration values are rejected before error formatting; malformed historical values are redacted from Session receipts and audits
19. credential resolution and every Git read/push, PR create/projection, `pr.rebase`, and exact `pr.merge` provider seam reload the Session/original durable principal/repository and recompute grant revision plus the full authority snapshot. Workload merge role admission is limited to `AGS_ACCESS_ROLE=maintainer|admin`; once admitted, the merge path still proves exact AGS/Forgejo numbering, canonical method/head, current AGS and Forgejo head/base convergence, protected branch, authorized integration bot, dry-run non-mutation, server-owned credential use, exact-head provider readback, and `outcome_unknown` recovery evidence without claiming success. The merge role never adds repo/admin/force/review operations. Durable action GET denies another principal and `pr.read`-only authority, while grant revoke or team/class/epoch/policy/revision drift at GET, startup recovery, label add/remove, comment, Git, push, and provider ensure seams terminally fails closed with zero denied provider writes. `pr.rebase` delegated and durable paths require exactly JSON-safe positive AGS/Forgejo PR numbers and canonical full expected head/base SHAs, rebuild them from the persisted action intent at each effect seam, and reject old ref/`exact_head`, mixed, missing, unknown, or tampered values. Creation/webhook/recovery serialize on the AGS PR row, reconcile expired rows, reject duplicate-active state, and compare all idempotent AGS/Forgejo/label/expiry/authority facts. Deterministic initial/recovery dispatch races must block the provider return after label acceptance, prove the webhook consumes durable `dispatching`, preserve any bound job and later state, and prove an outcome-unknown provider error remains `dispatching` for idempotent retry. Fresh AGS Git/PR, mapping, Forgejo ref/PR and base facts must match the exact intent before initial effects; after rebase only the same job generation's durably recorded exact head may converge. An empty delegated marker, revoke, write→read, admin→write, wrong repo/operation/capability/constraint, binding/policy/resource/trust/epoch drift, transaction retry drift, and drift between provider ensure/push/MR calls all deny before the affected provider write, persist terminal projection admission facts where applicable, and make zero denied provider calls, while unrelated Sessions remain usable and failed use does not advance `last_used_at`
20. `authority_boundary_receipts` migration is additive/repeatable; site-admin legacy capture accepts only the expected epoch, requires an exact build-injected source SHA, preserves historical closed v1 payload keys, keeps v1 read-only, and has the current producer always emit v2 with an explicit `resource_defaults` array, canonicalizes sorted secret-safe server facts deterministically, converges concurrent captures to one idempotent content ID, recomputes digest on GET, and rejects update/delete, wrong epoch, unknown source, payload tampering, or secret-shaped content
21. one shared constraint suite covers every registered operation across fresh Access Grant invocation matching and durable authorization: empty repo/Git vectors; exact create refs; exact rebase/merge/read/review/CI selectors; pinned `repo.create` source/import coordinates; pinned `repo.admin:forgejo_onboard`; and exact `review.submit` action. Positive and tampered vectors prove identical JSON-safe integer/ref/digest handling and reject unknown/mixed/secret-shaped forms. Generic transport tests separately prove `repo.create|repo.admin|review.submit` are denied even when a workload role Grant exists; `AGS_ACCESS_ROLE=maintainer|admin` never widens that envelope, and the `ags-expert` positive workflow remains limited to its separately owned import/onboarding receipts.
22. label-first `pr.rebase` covers signed delivery ID idempotency, exact normalized Forgejo actor → immutable numeric AGS principal binding, deterministic binding revision, live provider write permission, shared durable authorization, full projection/head/base/label admission, binding-change use-time denial, and one complete real-Git rebase/projection convergence. Duplicate delivery must preserve the same result with zero additional Git/provider/comment effects. Unmapped/unauthorized/head/base drift creates no intent/job, removes the request label and sets blocked status; an already-absent action label is a stale delivery and is ignored so delayed webhooks cannot regress a completed request.
23. provider evidence integration proves anonymous public-repo requests return 401 before any provider call, `pr.read` can access only the exact bound PR number, and `ci.read` revalidates the exact PR/head Session immediately before a completely paginated provider read. AGS uses its server-owned provider credential while clients supply none. Success returns and audits a correlation receipt linking principal/Session/Multica workload/AGS PR/provider binding. Provider transport failure remains `authorization=allowed + provider_attempt=attempted + provider_outcome=outcome_unknown + recovery_owner=ags_operator`, returns no internal error or credential value, and never becomes permission denial.
24. Human single-frontdoor provider merge accepts only an authenticated AGS Human、AGS PR、expected head and configured merge method; divergent provider PR mapping and executor come only from AGS. The binding derives a non-default base only from the authoritative AGS PR, requires that base in mirror policy, keeps full protection on the configured default base, and requires the server executor's repository collaborator authority on every base. Agent/Session callers、head/base/method drift、missing independent current-head approval and protection/executor drift all make zero provider writes. Pending or unavailable provider CI does not block this explicit Human timing decision. A transient provider `mergeable=false` is retried only by bounded exact GET while state/head/`F` remain unchanged；drift or timeout makes zero provider writes. Preflight compares Forgejo PR base to live Forgejo `F`, not AGS `A`. A confirmed merge returns provider success separately from webhook convergence；a retry while convergence is pending reads the exact mapped provider PR and never sends a second merge POST. `GET .../provider/merge` is observation-only. Repo-local `fast_forward_ack` covers `F==H` with zero merge POSTs, keeps an already-advanced AGS base, and leaves the Forgejo PR projection open.
#### Current execution-context and Access Grant integration

1. Execution-context intake resolves `source_instance_id` and/or `runtime_endpoint_hint` only through the immutable registry, sends the current `mat_*` once to a fixed egress, rejects zero/multiple matches and redirects before forwarding, requires the closed running source facts to equal the locator, stores only canonical source-ref/context/digest, refuses snapshot update/delete, and never emits or persists the bearer or its authenticating hash. A repeated/renewal intake must submit a currently valid bearer; source terminal/revocation fails without a snapshot, and no standalone intake result is an AGS authority grant.
2. Canonical Actor/Access Grant integration proves profile-free issue through the real router with a current source token, stable `(source_instance_id, external_agent_id)` JIT identity, no actor token, explicit actor/executor separation, hash-only bearer persistence, closed/no-store/secret-safe REST and OpenAPI, legal-default fail-soft behavior for absent/unbound/invalid `agent_id`/class/operations, actor-bound elevated-class acceptance only with the actor's live native repository grant, canonical high-risk invocation admission plus generic-transport denial, canonical transport Context Envelope subject, native-grant use-time revocation, exact Grant renewal/revoke/expiry/policy-drift behavior, immutable/no-delete grant and invocation rows, generic invocation readback, the table-driven positive/negative `repo.read` branch-protection root surface, and the `pr.read` numbered-PR GraphQL admission matrix covering exact owner/repository/number binding, body forwarding, and mutation/query-shape mismatch denial. Positive Git and GitHub-compatible API tests issue real Grants and production-derived transport Sessions; they never use historical Session rows as positive fixtures. Dynamic Session lifecycle status/revoke/replay routes must return `404`, while exact-ID lifecycle readback remains site-admin historical audit only; operation-specific `pr.rebase` authority-boundary receipt readback is a separate admission-evidence route, not lifecycle. Exact Access Grant `pr.merge` must require a caller-owned canonical invocation ID；the supported CLI deterministically derives it, GETs it before POST, emits zero POST for an existing receipt, and fails closed on uncertain GET. The service must make zero provider writes on repo/PR/head/base/mergeability/current-head review/CI/protection drift, persist that locator with attempted/outcome-unknown before the provider seam, make exactly one server-owned POST on success or transport uncertainty, block a renewed Grant from reclaiming the same effect key, and reconcile through GET only—with either the revoked originating bearer or a verified renewed descendant—without another provider write.

This is the integration layer for execution-context, Access Grant, transport, and provider-effect behavior; it should not be left exclusively to acceptance tests.

### Deferred Work

If we later refactor REST and router to consume narrow service interfaces, we can add smaller handler-unit tests on top.
That refactor is optional and should come after the real integration layer exists.

## Phase 3: Acceptance and End-to-End

The high-fidelity end-to-end layer is split across:

- `cli/acceptance/` for vendored gh CLI compatibility coverage
- `e2e/` shell flows for API and governance regressions that are easier to drive with `curl`, `git`, and `jq`

OIDC-specific end-to-end coverage should stay deterministic. Prefer the existing
mock-provider pattern and add provider-shaped discovery and ID token fixtures
under `e2e/cmd` rather than depending on a live third-party identity provider
in CI.

### Role of the End-to-End Layer

- verify CLI behavior against the running server
- protect cross-package flows that unit tests cannot express cleanly
- catch regressions in URL shape, auth behavior, host handling, and serialized responses
- keep organization-governance and collaboration-permission flows executable outside of ad hoc manual testing

### CI Coverage

For normal CI, run the full gh CLI compatibility inventory and a focused E2E subset around the major paths.

The acceptance lane should still prove the visible entry sequence around `gh`: `/api/v3/`, `/api/v3/meta`, `gh auth login --with-token`, then `gh auth setup-git`, while E2E scripts cover API flows that are easier to drive with `curl`, `git`, and `jq`.

- `TestAPI` with `basic-rest.txtar`
- `TestAPI` with `basic-graphql.txtar`
- `TestAuth` with `auth-setup-git.txtar`
- `TestRepo` with `repo-create-view.txtar`
- `TestRepo` with `repo-clone.txtar`
- `TestIssues` with `issue-create-basic.txtar`
- `TestPullRequests` with `pr-create-basic.txtar`
- `TestPullRequests` with `pr-merge-merge-strategy.txtar`
- `TestReleases` with `release-upload-download.txtar`
- `TestWorkflows` with `workflow-run.txtar`
- `TestSearches` with `search-issues.txtar`
- `TestSearches` with `search-code.txtar`
- `e2e/repo-rollback-compensation.sh`
- `e2e/push-postprocessing-consistency.sh`
- `e2e/oauth-device-flow.sh`

The current blocking merge gate includes:

- `make test-unit`
- `bash scripts/integration_tests.sh`
- `make test`
- `make test-e2e`
- `bash scripts/backend_smoke.sh`

Optional quick local sampling can still use `make test-e2e SCRIPT=...`, but the main gate now runs the full shell inventory.

### Full Compatibility Runs

Run the full acceptance suite in slower gates such as:

- nightly or scheduled validation
- pre-release validation
- large compatibility refactors
- changes touching router wiring, auth, serialization, or CLI patch behavior

## Recommended Execution Order

1. strengthen `gitstore` plus repo, PR, issue, and auth package tests
2. add workflow and release package tests for the main lifecycle paths
3. build a reusable HTTP integration harness on top of the real router
4. add API integration coverage for the main user paths
5. establish a small acceptance smoke gate
6. keep the full acceptance suite as the broader regression net

## Commands

Useful commands for this roadmap:

```bash
python3 scripts/doc_lint.py
bash scripts/check-module-contracts.sh
bash scripts/regression_gate.sh
bash scripts/integration_tests.sh
bash scripts/backend_smoke.sh
go test ./...
make test-unit
make test-integration
make test
make test-e2e
make test-e2e SCRIPT=org-collaboration-governance.sh
make test-run SUITE=TestPullRequests
make test-script SUITE=TestPullRequests SCRIPT=pr-create-basic.txtar
```

## Not the Current Focus

These are lower priority until the main path coverage exists:

- exhaustive handler-mock unit tests
- performance and load testing
- fuzzing every parser and endpoint
- full endpoint-by-endpoint API matrix coverage
