# Startup dependency recovery

This proof covers selected startup failures in an isolated synthetic environment.
It does not authorize deployment, a database migration, a provider call, or
changes to a running service. A passing result is not a maintenance-window
acceptance record.

## What the tests prove

`server/startup_dependency_test.go` runs the real standalone startup path in a
child process. Its environment is allowlisted and contains only fixture values.
Both scenarios use a temporary SQLite database and an empty Git repository root:

- while the database parent path is an ordinary file, startup fails at database
  initialization and does not create the Git root;
- after that path is replaced by a directory, startup migrates, seeds once,
  answers `/readyz` and one authenticated read, then stops on `SIGTERM`;
- a second startup does not add users or tokens, leaves the listener released,
  and acquires no Git refs;
- a migration that commits its first table and then loses the process leaves no
  seed row. The next startup completes migration and passes the same checks.

`internal/service/startup_dependency_recovery_test.go` uses the existing
synthetic repository, PR, authority and Git fixture. Provider traffic stays on
a loopback test server. Each process incarnation reopens the same SQLite file
and uses the production recovery and pending-job entries:

- Forgejo label dispatch receives one `503` before any provider effect, or one
  timeout after exactly one effect. Startup records the dispatch as unknown
  rather than success. The next startup makes one request, reaches exactly one
  durable effect, and a further startup makes no request;
- one success comment is committed by the provider and then its response is
  lost. The durable claim remains incomplete. Startup recovery reads the exact
  marker back, writes no second comment, preserves the attempt count, and
  completes the exact intent and job. A second restart does not repeat either
  provider operation.

Both provider scenarios compare the PR row and `git show-ref` before and after
recovery. Neither changes.

## What the tests do not prove

These tests do not exercise the deployed database, its current schema migration,
production provider recovery, credentials, certificates, or a consistent
database-plus-Git snapshot. Those checks belong to a reviewed maintenance window
after writers are stopped. They also do not prove Forgejo authority readiness:
that diagnostic is warning-only and is covered separately from request recovery.

The suite does not cover every startup worker. In particular, it does not prove
Wiki recovery, generic projection repair, outbound delivery, or recovery of an
arbitrary historical provider result. An unknown external result remains unknown
until its exact readback is reviewed; closing a test fixture is not permission
to clear a production record.

Do not disable a worker, replace the candidate binary, or point the fixture at
a deployed database to reproduce this proof. Keep the temporary database, Git
root and provider server isolated, and retain the test process exit status.
