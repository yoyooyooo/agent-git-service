# Route B preparation runbook

This runbook describes the executable host-package source slice and the
remaining native and production gates. It does not grant a deployment or a
Release. Earlier `c980bb63` and `f63aa88e` rehearsal notes are historical inputs,
not current running or rollback identities.

Host-specific pages:

- [mini](routeb-runbook-mini.md)
- [imile-win](routeb-runbook-imile-win.md)

Related contracts, which stay fail-closed:

- [baseline](routeb-baseline.md)
- [local upgrade](local-upgrade.md)
- [backup and recovery](backup-recovery.md)
- [maintenance window](maintenance-window.md)
- [external dependencies](routeb-external-dependencies.md)
- [host package](routeb-host-package.md)
- [build inputs](routeb-build.md)

`c980bb6385504c7a3e87d4b1e42f50909ee75d0e` is a historical rehearsal revision.
It was not deployed. It is not the current target and not the rollback target.

## 1. Identities

| Role | Revision |
|---|---|
| Application baseline verified on 2026-11-05 | `4c963c9ff7ef03f283bf628ad213777713469532` |
| Host-package candidate | Explicit private candidate manifest; current execution policy requires the same application revision and tree as the installed baseline |
| Host tooling | Separately reviewed tool revision, tree and complete bundle manifest |
| Recovery target | Exact old binary and whole Tq group from that transaction's sealed backup |

`--version` must show `version=source-<built revision>`, `revision=<built revision>`,
and `tree=<built tree>`, plus `goos`, `goarch` and `go_version`.
`development` is not an acceptable candidate version. The builder stamps the
clean checkout it verified. The server's ready revision is a separate build
field, `server.gitSHA`, and must contain the same full revision. A `buildinfo`
version stamp alone does not populate `/readyz.version`. This page does not
claim a Release or select a mainline application upgrade.

## 2. What changed for operators

The current host-tooling slice refuses an application source/tree transition
and requires `allow_schema_change=false`. It does not bring in mainline's
`MigratePRLifecycleColumns` or change the deployed CI run-selection policy.
Any later application upgrade needs its own exact source diff, schema boundary
and recovery plan.

Native host provisioning installs a fixed trusted bundle and a user-owned
isolated Python launcher, with a one-shot manager definition. It does not
silently modify the existing production launcher or manager.
The recovery group still includes SQLite committed state and WAL, Git, Wiki,
CAS, runtime, config, secrets, the old binary and receipt, and the external
dependency contract. "No new environment variable" is not "no backup".

`/readyz` can be HTTP 200 while `forgejo_authority_policy` is
`diagnostic_only`. Ready is not Forgejo authority, Git, pull-request,
webhook or Edge health.

Webhook classification at this revision: a body already over 10 MiB is HTTP
413 even when that read also fails; a read error inside the cap is HTTP 400.
`/readyz` is not a webhook proof.

## 3. Preparation gates

Source preparation may read git, build in an isolated directory, and run
component tests. The private host `plan` and `status` commands are read-only.
Native registration requires its own bound inputs. Lifecycle activation,
`run --execute` and managed native startup require the live registered root
native supervisor. Direct Python, legacy profiles and file-only checks remain
closed by `interpreter_full_closure_required`; bootstrap plans without that
authority return this blocker and exit 78. Isolated Linux v2 has a conditional
native path with the [complete interpreter authority](../design/routeb-interpreter-authority.md).
Ordinary source tests do not perform or prove these native steps.
Production plans additionally retain `native_full_closure_required`.

Before a later window, these items are still required and are not proven here:

1. Exact candidate manifest: revision, tree, version, GOOS, GOARCH, Go
   version, sha256, linkage. Linux must be a native build, not the Darwin binary.
   `scripts/routeb_candidate.py` accepts only `ags.routeb-candidate-manifest.v1`
   and resolves a relative binary against the manifest parent. That check is
   not host admission and does not install.
2. Deployment id, transaction id, window approval, and binary/build manifest
   bound together.
3. External dependency contract accepted by each owner. See
   [external dependencies](routeb-external-dependencies.md).
4. A complete private host package, its fixed sidecars, registered native host
   identity, reviewed bundle, current installed receipt and bound approval.
   `routeb_host_native.py` provides fixed launchd/systemd capabilities; its
   constructor does not register a service or grant permissions. The old
   `upgrade_adapter_*.create()` stubs are not that execution path.
5. An existing active, host-bound lifecycle authority at epoch 1, no pending
   effect, a one-shot manager definition, real process and writer closure,
   usable System Trash, and every recovery dependency. The entry reports
   specific blockers before stopping when these prerequisites are missing.
6. Explicit v2 immutable-authority records for the complete root-owned bundle
   and fixed host identity. See [authority archives](routeb-immutable-authority.md).
   Missing privilege never permits a fictitious root-metadata copy.
7. A complete interpreter dependency authority, including standard-library
   and extension bytes, loader/shared libraries, and mapped OS resources,
   verified before that interpreter executes. The existing executable hash
   and SQLite capability check do not establish this authority. The current
   isolated Linux v2 entry must prove its registered files, kernel identities,
   fixed process tree and platform prerequisites before satisfying this gate.
   Source CI does not establish native or production acceptance.

## 4. Backup and rollback

The transaction's installed receipt and cold seal determine the rollback
target; an old revision written in a runbook is not authority. After a start
attempt, recovery is a whole cold-group restore followed by a separately
approved old start. Swapping only the binary after a new process has started
is not rollback.

The cold backup is one Tq group: SQLite committed state and WAL semantics,
Git, Wiki, CAS, config, secrets, old binary, old receipt, and the manager
definition hash recorded by the external contract. An online copy is not
this backup. ACL, xattr and mtime must be compared after an independent
restore. A tar exit code is not that comparison. Platform metadata that
cannot be read is a refusal, not a pass.

Failed partial backups and failed restores stay on disk. They are not
deleted to make room.

## 5. Transaction boundary

Do not assemble a production install from separate manager commands.
`launchctl` and `systemctl` forms in older rehearsal notes are not a
runbook step. The single entry is `scripts/ags-maintenance-transaction`.
`--mode host` selects the host coordinator and is not the default. A host
plan without a private package writes nothing and reports `package_missing`.
The default command remains the isolated rehearsal and still refuses a real
`~/.ags`. Host mode does not accept `--manager-dir` or `--fault-after`.

Its required order for an admitted window is: manager
suppression, normal drain, cold backup, independent restore verification,
candidate readback, install, one start, ready plus
business and external readback, then observation of at least 600 seconds.
The default command remains a fixture rehearsal. Explicit host mode contains
`routeb_host_execution.py` and fixed capabilities, but its native effects remain
blocked by the interpreter gate. A fixture result cannot establish native
acceptance. Production execution also requires the complete native closure.

Failure keeps the service stopped and fenced. Before `start_attempted`, and
only when Tq is unchanged, a newly approved pre-start restore of the old
binary and receipt is possible. After `start_attempted`, or after the
migration has run, recovery is the whole group or a forward repair.
Unknown manager or provider outcome stops. An isolated admitted chain
through `scripts/routeb_host_execution.py` seals a backup, runs independent
restore verification, and starts once. An unknown start is not replayed.
Stop and start each re-read the approval; an expired approval stops before
the manager call.

## 6. Acceptance that this page does not perform

A later window still needs: local and service `/readyz` with the exact
revision, a separate Forgejo authority readback, Git and pull-request
readback, webhook 413/400 evidence, Edge evidence, and 600 seconds of
observation. `scripts/routeb_observation.py` refuses 599 seconds, a lease
longer than the elapsed window, and a 200 readyz whose provider result is
unknown. Elapsed time starts at validation, not prepare. `readyz` remains
diagnostic_only and is not a webhook proof. Recovery inventory and the
review gate are the functions in `scripts/ags_maintenance_window.py`. Caddy and Tailscale are not restarted as a side effect of
replacing the AGS binary.

mini and imile-win require their own host identities, private packages and
receipts. A completed observation on one host is not evidence for the other.
Use the current session's explicit deployment authorization; these source
documents neither create it nor revoke an authorization already given.
