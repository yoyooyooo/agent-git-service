# Route B preparation runbook

This runbook updates the c980bb63 rehearsal notes to exact source
`3a0c7e4f0ac5150d774efc66168907671061bcc5`
(tree `a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`).
It describes preparation facts and the separate deployment window.
It does not authorize either window.

Host-specific pages:

- [mini](routeb-runbook-mini.md)
- [imile-win](routeb-runbook-imile-win.md)

Related contracts, which stay fail-closed:

- [baseline](routeb-baseline.md)
- [local upgrade](local-upgrade.md)
- [backup and recovery](backup-recovery.md)
- [maintenance window](maintenance-window.md)
- [external dependencies](routeb-external-dependencies.md)
- [build inputs](routeb-build.md)

`c980bb6385504c7a3e87d4b1e42f50909ee75d0e` is a historical rehearsal revision.
It was not deployed. It is not the current target and not the rollback target.

## 1. Identities

| Role | Revision |
|---|---|
| Candidate | `3a0c7e4f0ac5150d774efc66168907671061bcc5` |
| Running and rollback | `0013aa744c8c90637ec54562ffc248a1bf7c9a52` |

`--version` for a preparation candidate must show `version=source-3a0c7e4f0ac5150d774efc66168907671061bcc5`,
`revision=3a0c7e4f0ac5150d774efc66168907671061bcc5`, and
`tree=a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`, plus `goos`, `goarch` and
`go_version`. `development` is not an acceptable candidate version.
The build slice freezes the string. This page does not claim a Release.

## 2. What changed for operators

The first start of the candidate runs `MigratePRLifecycleColumns`.
On an existing `pull_requests` table it adds `lifecycle_generation`,
`forgejo_reopen_pending` and `forgejo_lifecycle_at` when each column is
absent. The migration is idempotent. There is no down migration.

Configuration files and launcher arguments did not change in this range.
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

Preparation may read git, update these documents, build in an isolated
directory, and run the transaction tool against an injected fake manager.
Preparation must not read or write a real `~/.ags`, stop or start a service,
edit launchd, systemd, Caddy or Tailscale, install, migrate, restore, tag,
publish a Release, or dispatch a workflow.

Before a later window, these items are still required and are not proven here:

1. Exact candidate manifest: revision, tree, version, GOOS, GOARCH, Go
   version, sha256, linkage. Linux must be a native build, not the Darwin binary.
2. Deployment id, transaction id, window approval, and binary/build manifest
   bound together.
3. External dependency contract accepted by each owner. See
   [external dependencies](routeb-external-dependencies.md).
4. A host package for the transaction tool. The checked-in tool refuses a
   real home `~/.ags` and refuses `execute` without an explicit approved
   window file. `scripts/upgrade` still returns `backup_contract_unimplemented`.
   `upgrade_adapter_launchd.py` and `upgrade_adapter_systemd.py` still raise
   `native_adapter_unavailable` from `create()`. Those source contracts are
   not the production adapter.

## 4. Backup and rollback

Rollback target is `0013aa744c8c90637ec54562ffc248a1bf7c9a52`.
Because the migration has no down migration, rollback is: stop the service,
restore the whole cold backup taken at quiescence, then start the old
binary. Swapping only the binary after the new process has started is not
rollback.

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

Its required order, once a future window is approved, is: manager
suppression, normal drain, cold backup, independent restore verification,
candidate readback, install, one start, first-start migration, ready plus
business and external readback, then observation of at least 600 seconds.
This repository copy defaults to an isolated root and a fake manager. It
does not perform that window.

Failure keeps the service stopped and fenced. Before `start_attempted`, and
only when Tq is unchanged, a newly approved pre-start restore of the old
binary and receipt is possible. After `start_attempted`, or after the
migration has run, recovery is the whole group or a forward repair.
Unknown manager or provider outcome stops.

## 6. Acceptance that this page does not perform

A later window still needs: local and service `/readyz` with the exact
revision, a separate Forgejo authority readback, Git and pull-request
readback, webhook 413/400 evidence, Edge evidence, and 600 seconds of
observation. Caddy and Tailscale are not restarted as a side effect of
replacing the AGS binary.

mini and imile-win are separate windows. A mini observation does not
authorize imile-win.
