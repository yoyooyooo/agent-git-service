# `team_authority` Runtime Hard Cut

This runbook owns the one-time deployment boundary from the retired
`integrations.yaml.principal_sessions` key to `integrations.yaml.team_authority`.
It is an operator deployment transaction, not an Agent-facing command or a
second authorization control plane.

## Invariants

- The new binary accepts `team_authority` and rejects both
  `principal_sessions` and `principal_session`.
- The old and new keys must never coexist.
- The mapping payload is copied byte-for-byte at the YAML value level; this
  migration does not add a principal、binding、class、resource or operation.
- Operation names and risk classes come from `internal/operationcatalog`.
  Runtime configuration cannot add or downgrade them.
- Provider credentials remain deployment-owned and are not part of this file.
- Rollback restores the old binary **and** old configuration together.

## Preflight

Record secret-free evidence for:

1. accepted candidate source revision and candidate binary SHA-256;
2. current binary source revision and SHA-256;
3. owner-only backups of the current binary and complete integrations file;
4. one unambiguous config state: either exactly one retired
   `principal_sessions` key with no new/singular key, or an already-converged
   `team_authority` key with neither retired key;
5. current readiness and the configured AGS target;
6. a rollback command/path that restores both backed-up artifacts.

Stop if both keys exist, the singular key exists, all authority keys are
absent, YAML is not closed/valid, or the backup cannot be read back and hashed.
An already-converged `team_authority` config is a no-op config migration; do not
rewrite it.

## Apply Transaction

1. Stop the AGS service through its deployment service manager.
2. If and only if the retired plural key is present, rewrite only its
   top-level name to `team_authority`; preserve the complete nested value and
   owner-only mode. If `team_authority` is already the sole key, leave the file
   byte-for-byte unchanged.
3. Install the exact-source candidate binary with an atomic same-filesystem
   replace.
4. Start the service.
5. Require readiness and exact source-revision readback.
6. Verify fresh Access Grant issue/use and the required provider-observation or
   merge-denial/effect path for the deployment.
7. Probe retired assertion/session/delegated-merge routes and require `404`.
8. Record binary/config hashes and the fact that the retired keys are absent;
   never record tokens or provider credentials.

Do not use the retired Agent Kit `principal-session` planner/apply/rollback
surface. It is intentionally absent from current source.

## Failure and Rollback

If startup、readiness、source readback or any hard-cut probe fails:

1. stop the candidate service;
2. restore the complete old integrations file (thereby restoring
   `principal_sessions`);
3. restore the exact old binary;
4. start the old service and require its readiness/source readback;
5. retain the failed candidate/config hashes and bounded diagnostics as the
   rollback receipt.

Never roll back only one side of the binary/config pair.

## Completion Evidence

The cutover is complete only when all are true:

- deployed source revision equals the accepted commit;
- deployed binary hash equals the recorded candidate hash;
- runtime config contains `team_authority` and neither retired key;
- readiness succeeds after restart;
- fresh current-path evidence succeeds;
- retired routes return `404`;
- the old binary/config pair remains available for bounded rollback.
