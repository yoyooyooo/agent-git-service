# mini preparation runbook

Common rules: [route B runbook](routeb-runbook.md).
Candidate `3a0c7e4f0ac5150d774efc66168907671061bcc5`,
tree `a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`.
Rollback remains `0013aa744c8c90637ec54562ffc248a1bf7c9a52` by restoring the
whole cold backup. There is no down migration.

This page replaces the c980bb63 mini rehearsal note. `c980bb63` is not the
target.

## Platform

- Host profile name: `mini`.
- Expected candidate platform: Darwin/arm64, `CGO_ENABLED=1`, Go `go1.26.8`.
- A Darwin binary must not be copied to imile-win.
- The preparation candidate is not a Release. Accept `--version` by the built
  checkout: `version=source-<built revision>`, `revision=<built revision>`,
  `tree=<built tree>`. The built revision is `git rev-parse HEAD` of the
  reviewed checkout passed as `GIT_SHA`. Content baseline
  `3a0c7e4f0ac5150d774efc66168907671061bcc5` (tree
  `a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`) is not that built identity.
  The copyable command is in [routeb-build.md](routeb-build.md).

The 2026-11-02 read-only deploy check recorded mini `/readyz` as ready on
`0013aa744c8c90637ec54562ffc248a1bf7c9a52`, with
`forgejo_authority_policy` degraded and `diagnostic_only`. That observation
is not a current process listing and is not webhook proof. `/readyz` is not
a webhook proof.

Historical rehearsal notes named `gui/$(id -u)`, label
`com.jackie.agent-git-service`, and
`~/Library/LaunchAgents/com.jackie.agent-git-service.plist`. Those strings
are inputs a future host package must read back. This document does not
contain a command that changes launchd. Do not disable, bootout, enable or
bootstrap the job from this page.

## Transaction

Call `scripts/ags-maintenance-transaction` with `--host mini`, the candidate
manifest, and a window file. The tool refuses a root whose real path is the
operator home `~/.ags`. The checked-in tests use an injected manager double
under an isolated directory.

The future window order is the common runbook order: suppress restart, drain,
cold backup, independent restore, candidate readback, install, one start,
first-start migration, ready and business readback, then at least 600 seconds.
Caddy and Tailscale stay unchanged during that window.

Before any start, and only while Tq is unchanged, a new approval can restore
the old binary and receipt. After `start_attempted`, restore the whole group
and reconcile. Do not automatically downgrade.

## Evidence this preparation does not replace

Darwin/arm64 linkage of the candidate built in this task, recorded outside
the repository. Live plist bytes, Python, Git, Multica, Caddy and Tailscale
versions. ACL, xattr and mtime of a real cold backup. Those remain deployment
window work or a separate Linux-native build for the other host.
