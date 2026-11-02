# imile-win preparation runbook

Common rules: [route B runbook](routeb-runbook.md).
Candidate `3a0c7e4f0ac5150d774efc66168907671061bcc5`,
tree `a6fc9f08d5ca4c8d8fe4860dc8e551263455426f`.
Rollback remains the whole cold backup of
`0013aa744c8c90637ec54562ffc248a1bf7c9a52`. There is no down migration.

This page replaces the c980bb63 imile rehearsal note. `c980bb63` is not the
target. A mini observation does not authorize this host.

## Platform

The candidate must be built on Linux x86_64 with `CGO_ENABLED=1` and Go
`go1.26.8`, then checked with `file` and `ldd`. Copying the mini Darwin/arm64
binary is forbidden. This macOS preparation task does not cross-compile that
binary and does not call the result native.

The 2026-11-02 read-only deploy check recorded imile-win `/readyz` as ready
on `0013aa744c8c90637ec54562ffc248a1bf7c9a52`, with
`forgejo_authority_policy` degraded and `diagnostic_only`. No SSH was used.
Live passwd, home, uid, gid, OS, glibc, unit scope, drop-ins, cgroup and
writers were not read. Unknown preflight is a stop, not a default.

`/readyz` is not a webhook proof.

## Service manager

The systemd scope is part of the host package. System and user scopes are
not mixed. This page does not contain a command that stops, starts, masks
or unmasks a unit. Do not pass a shell or a wildcard through sudo.

A future stop is closed only when the host package records stop exit,
`Result=success`, `MainPID=0`, an empty cgroup and a closed trigger.
Exit zero alone is not closure. `upgrade_adapter_systemd.py` still raises
`native_adapter_unavailable` from `create()` and is not that host package.

## Backup boundary

Forgejo on the same machine is not part of the AGS recovery group. Shared
Forgejo database, repositories, LFS and Actions are restored only under the
Forgejo owner contract. Restoring AGS must not restore those shared objects
as a side effect.

Linux ACL, xattr, mtime, owner and group need their own measurement. The
macOS rehearsal does not prove them. Non-root and root restore permissions
are a window check.

## Window

imile-win has its own candidate, cold backup, receipt and observation.
Disk size and stop duration are not known from mini. The earlier planning
note reserved 30–90 minutes and labeled that figure as planning, not a
measured SLA.
