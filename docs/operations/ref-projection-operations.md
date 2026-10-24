# Ref projection operations: S0/S1 boundary

See the [design contract](../design/ref-projection-convergence.md) and
[writer/adapter inventory](../design/ref-projection-writers.md).

## What is available

Only immediate typed safe diagnostics for existing projection transports have
landed. There is **no ref plan/execute/retire API or CLI**, migration, receiver,
review store, archive builder or enablement flag. `RefCapabilities()` always
reports false. Do not use old PR retry/repair, artifact history retirement or
ordinary branch deletion as substitutes for this future protocol.

Existing push logs include a `diagnostic` object and the compatibility
`authenticated_login`/`status` fields. Identity is null when not proven for the
selected transport; configured bot/TokenOwner and actor are not substitutes.
Native HTTPS probes `/api/v1/user` once per push using the same selected token.
Managed SSH diagnostics read the already-selected public-key binding and owner;
they do not register keys or change ACLs. Legacy managed key/ACL provisioning
remains its own existing path, not something enabled by this diagnostic probe.

Use category/reason/evidence_source together. HTTP status is numeric only when
measured from HTTP; a Git stderr 403 pattern has null status. A generic hook
refusal does not prove branch protection. A missing/inaccessible repository is
not proven absent. Ref-lock contention is different from expected-old conflict.
`stderr_truncated=true` is explicit; fixed-pattern scanning continues after the
capture bound. Unknown retains no raw provider response.

A transport success is `transport_succeeded_unverified`, not an attestation that
all desired remote effects occurred. Failure effect status stays unknown. Retry
advice means inspect/replan; it is **not** permission to replay a push. Existing
job lifecycle still owns retries and exact readback under its prior contract;
new executor semantics are not silently grafted onto PR workers.

Do not persist full DTOs into ErrorSummary, overwrite historical attempts or
backfill guessed identity. Durable diagnostics and operation/ref/attempt query
surfaces require a separately reviewed S3 migration. Existing status and
notification shapes remain unchanged and get bounded safe typed summaries.
Never log transport requests/config/errors with raw `%+v`, bearer headers,
credential URLs, private key paths, PR bodies or raw stderr. Low-cardinality
metrics may use provider/category/reason/effect-status only, not ref/login/job.

## Local verification

Run with the repository-required Go version, no deployed DSN, no real-provider
opt-in environment and no host credentials in fixtures:

```sh
go test -race ./internal/gittransport ./internal/forgejointegration -count=1 -timeout=4m
go test -race ./internal/service -run 'TestProjectionDiagnostic|TestProjectionState|TestProjectionOutbound|TestProjectionJob' -count=1 -timeout=4m
go build ./...
go vet ./...
python3 fork/scripts/audit.py verify
python3 fork/scripts/audit.py inventory --details
PYTHONDONTWRITEBYTECODE=1 python3 fork/scripts/test_audit.py
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_edge_client_route.py
python3 scripts/doc_lint.py
```

Service tests must use their test-owned isolated fixtures. Full root CI provisions
a job-owned loopback TiDB; never point TEST_DB_DSN at a deployed database. The
real Forgejo test is separately opt-in; ordinary local hermetic tests skip it.
A skip is not a pass or real-adapter acceptance. GitHub CI may run its own
disposable Forgejo fixture, not a production provider.

## Future enablement and recovery

An exact-version receiver must cover **all** writers, journal commit atomically,
reject stale epochs/unknown history/identity, enforce live source/review gates
under partition, and demonstrate full R offline recovery before retirement.
S7 must close legacy non-CAS delete/unattributed webhook/internal cleanup bypasses.
No such evidence or production permission is supplied by S0/S1.

Future rollback stops admission/workers while retaining status, read-only
reconciliation, journal, idempotency tombstones, reviews and archives. No down
migration deletes evidence; no ref rollback or archive deletion to fake success;
no old binary auto-replays unresolved work. Already-sent unknown effects are
inspected, never blindly retried. Production operations require a new realtime
signed plan and separate explicit authorization, not historical counts or CI.
