# Automatic repository storage maintenance

The primary owns automatic Git storage maintenance. It requires no admin
console, cron job or routine operator command. This is storage maintenance,
not permission to rewrite history or discard PR evidence.

## Default lifecycle

`server/git_maintenance.go` starts with the primary and stops with its server
context. An initial scan starts after 30 seconds. Subsequent scans visit a
bounded page of repositories in ID order, including inactive repositories.
Only one maintenance operation runs at a time in one primary.

Configuration:

| Variable | Default | Accepted range |
| --- | --- | --- |
| `AGS_GIT_MAINTENANCE_ENABLED` | `true` | Explicit boolean |
| `AGS_GIT_MAINTENANCE_INTERVAL` | `1h` | `1m` through `24h` |
| `AGS_GIT_MAINTENANCE_TIMEOUT` | `2m` | `1s` through `5m` |

The interval is between bounded scan pages, not a promise that every repository
is scanned once per interval. New non-empty stores are visited once even when
small. Recent successful operations are rate-limited; object/pack pressure and
an eventual weekly refresh make a repository eligible again. Failures are
retried by later scans. A yielded page gets at most two near-term retry scans
(one minute apart, or the configured interval if shorter), then normal paging
resumes so a busy repository cannot starve the rest. No workflow receives
authority from a maintenance receipt or a timestamp.

The worker logs a structured result and stores only the last receipt in the
repository's owner-only `ags-maintenance.json`. Successful runs are quiet apart
from the operational log. A changed missing-object condition emits a warning,
not a claim that the historical data was repaired. No token, user body or
credential is included. Failed receipts include the exact bounded maintenance
phase and, for application inventory errors, a finite reason plus a static
schema location. They never expose SQL values or constraint contents. A
cancelled database preflight remains `deferred`, not a persisted failure;
upgrades recheck older unclassified failures without manual receipt removal.
This version does not add external notification delivery.

## Protection and outcomes

`internal/service/git_maintenance.go` enumerates local object identities from
PRs, reviews, commit statuses, deployments, runs, projection facts and admitted
operation constraints. Every available application root is protected through
`refs/ags/retention/<oid>` before native Git can prune. These server-owned
protection refs are not automatically expired when an application row stops
appearing. A separate retention/migration decision must retire such a fact.

New features that keep Git identities outside refs must update this inventory
or own an equivalent explicit Git retention root. Query failures, malformed
schema and inventory budget overflow stop the operation; they are never
interpreted as an empty result. Wiki backing stores and snapshot stores retain
their existing owners and are not included in this primary-repository scan.

| Receipt status | Meaning |
| --- | --- |
| `completed` | Native maintenance finished; refs were checked before/after. `pruning_enabled` says whether pruning was permitted, not how many bytes were deleted. |
| `compacted_no_prune` | Some application identities were already missing. Native repacking keeps all unreachable packed objects and leaves loose objects intact. Missing data is not healed or hidden. |
| `deferred` | Foreground access, cancellation or a competing maintenance operation prevented completion. This is not success. |
| `failed` | Preflight, native Git, integrity or receipt persistence failed. |

With a complete available inventory, native GC uses a two-week object grace
and does not expire reflogs. It never runs `--prune=now`. With missing objects,
only `repack --keep-unreachable` is allowed. Normal branch/tag/PR refs are not
deleted, reset or translated by either mode. GC cannot remove an artifact that
is still reachable through any retained history.

Receipts report loose/packed KiB, pack/object counts, reference namespaces,
a ref digest and protection/missing counts. These are current storage
observations, not an estimate of a new clone's network transfer or a guaranteed
physical-disk saving. Snapshot hardlinks and external replicas may independently
retain old files after the primary's repack.

## Concurrency and failure boundaries

Known repository operations acquire maintenance admission for their scope.
Unknown/multi-repository requests conservatively acquire global admission.
Foreground work for the repository interrupts maintenance; unrelated scoped
Git-store work can continue. Health/metrics polling does not continuously cancel
idle work. Snapshot capture has its own established barrier and preempts
maintenance rather than holding a long pack operation inside a capture callback.

Native commands use a separate process group, receive TERM on cancellation and
have bounded escalation before the admission lease is released. Git HTTP and
managed local merge/fetch operations do not launch detached automatic GC behind
the worker's back. A file lock prevents two maintenance processes from packing
the same repository simultaneously. A file lock alone does not coordinate an
unmanaged writer or a second primary: deployments must still have one owning
primary and an audited writer topology.

The operation checks available space before packing and checks object
connectivity around the native maintenance step. Space is an observation, not a
reservation. It rejects alternate object stores, invalid storage boundaries and
unbounded inventories; it does not repair corruption, remove unknown lock files
or weaken repository permissions to finish. Inherited Git routing variables
cannot redirect the operation to another object store.

## Verification

```sh
go test ./internal/gitstore ./internal/gitbackend -count=1
go test -race ./internal/gitstore ./internal/gitbackend -count=1
go test ./internal/service -run '^TestGitMaintenance' -count=1
go test ./server -run 'Test(Maintenance|HealthPoll)' -count=1
```

The focused tests use real Git and isolated SQLite schemas. Full hosted CI
continues to cover TiDB, Git HTTP, the primary and replication. A source test
pass is not a deployed-runtime receipt.

## What requires a separate migration

A repository that previously committed native binaries retains those objects
in its historical commits even after a normal source change removes the files.
This worker deliberately preserves that history. Retiring such blobs needs an
explicit, replay-safe history migration covering all relevant refs, application
commit identities and configured external projections. A smaller pack after
normal GC is not acceptance evidence for that migration.

The `delete_branch_on_merge` API setting and source-branch lifecycle are not
implemented by this storage worker. It must not silently reinterpret a storage
maintenance opt-out or retention receipt as authorization to delete branches.
