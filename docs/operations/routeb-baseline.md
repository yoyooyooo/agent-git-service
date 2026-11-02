# Route B baseline and impact matrix

This page freezes the preparation inputs for moving the two running hosts
from `0013aa744c8c90637ec54562ffc248a1bf7c9a52` to exact source
`f63aa88e0c82b1e17174cf07f8033552457185b1`. It is not a deployment approval
and not a release record.

## Frozen identities

| Fact | Value | How it was read |
|---|---|---|
| Source branch | `fork/main.20260924` | `git rev-parse origin/fork/main.20260924` after `git fetch origin` on 2026-11-03 |
| Candidate revision | `f63aa88e0c82b1e17174cf07f8033552457185b1` | same command; tip matched this value |
| Candidate tree | `8bf2c52ec3f7c7bc04850791b4e507bbc8261ab4` | `git rev-parse f63aa88e^{tree}` |
| Previous preparation revision | `3a0c7e4f0ac5150d774efc66168907671061bcc5` | content baseline of the superseded preparation; tree `a6fc9f08d5ca4c8d8fe4860dc8e551263455426f` |
| Running revision | `0013aa744c8c90637ec54562ffc248a1bf7c9a52` | read-only `/readyz` on mini and imile-win, recorded in the 2026-11-02 deploy check |
| Running tree | `0c2d1cbf7507dc715e5f65253236f6bdecd97afb` | `git rev-parse 0013aa74^{tree}` |
| Range size | 140 commits | `git rev-list --count 0013aa74..f63aa88e` |
| Rollback target | `0013aa74` | live revision; `c980bb63` was never deployed |

`c980bb63` remains a historical rehearsal revision only. Manuals must not
name it as the current target or as the rollback binary.

## Migration

`internal/db/migration_pr_lifecycle.go` is the only migration file added in
`0013aa74..f63aa88e` (`3a0c7e4f..f63aa88e` adds none). `db.go` calls `MigratePRLifecycleColumns` during startup.
The function adds three columns to `pull_requests` when the table exists and
the column is absent:

- `lifecycle_generation`
- `forgejo_reopen_pending`
- `forgejo_lifecycle_at`

The function is additive and idempotent. The repository has no down
migration for these columns. After the new binary has started once, rollback
is stop, then restore the whole cold backup, then start `0013aa74`. Replacing
only the binary is not a rollback.

`.env.example` and `internal/config` have no diff in this range. That does
not shrink the backup set: secrets, spool, replication cache and external
authority state still move with the recovery group.

## Health

`/readyz` checks the main database, typed Multica worker availability when
configured, outbound worker heartbeat and required projection alerting.
Those failures can return 503.

`forgejo_authority_policy` is written only as
`degraded` / `diagnostic_only`. The handler comment states that Forgejo
permissions and policy are not called from readiness. HTTP 200 therefore
does not prove Forgejo authority, Git, pull-request state, webhook delivery
or Edge.

## Webhook

`internal/githubappreceiver/receiver.go` classifies a body before a transport
error. `readBody` returns the oversized error when the buffered body is
already past `MaxBodyBytes` (10 MiB), even if the same `Read` also failed.
A read error that stays inside the cap is `payload read failed` with HTTP 400.
The oversized path is HTTP 413 and `payload exceeds 10 MiB`.

`5d1658b` and `3a0c7e4` change only this classification and its tests. They
add no environment variable, migration, launcher argument or manager step.
`/readyz` is not a webhook proof. Webhook acceptance uses the existing
receiver tests or a reviewed no-side-effect delivery, covering 10 MiB,
10 MiB+1 and an in-cap read error.

## Upgrade surface

`scripts/upgrade` still returns `backup_contract_unimplemented` / 83 for
default public apply and backup. `upgrade_adapter_launchd.py` and
`upgrade_adapter_systemd.py` raise `native_adapter_unavailable` from
`create()`. Those modules are source contracts and tests. They are not a
production manager adapter and must not be described as one.

`scripts/build-exact-source.sh` previously injected only `server.gitSHA`.
The preparation build slice injects `internal/buildinfo.Version`,
`Revision` and `Tree` as well. A candidate is a manual exact-source build.
It is not a Release and has no release provenance.

## What this page does not prove

Live process lists, disk size, OS, glibc, database paths, writers, loaded
launchd or systemd definitions, Linux native linkage, production cold-backup
stillness, ACL/mtime restore, migration duration on live data, and any
Forgejo, Multica, Caddy or Tailscale remote fact. Those stay outside this
source baseline. See the [route B runbooks](routeb-runbook.md).
