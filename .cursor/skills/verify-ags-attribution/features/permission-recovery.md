# Permission recovery

## Sub-features

Forgejo ACLs project AGS authority: only a currently AGS-authorized user/operation
gets minimum write after a definitive Sudo denial. Missing account,
failed grant, second denial and protected merge fall back without failure loops.

## How to get to it (user POV)

An admitted AGS user creates a PR projected to an explicitly mapped private
sandbox in which their existing Forgejo identity has no write permission.

## Driving it with Python + official gh

The helper checks initial `none`/read/404, then executes gh create. The evidence
must contain the initial denied create, a bot-owned `collaborator_grant` for only
write, one Sudo retry and a PR user equal to the mapping. Final API readback must
show write. `TestAttributionBoundedPermissionRecovery` covers failed grants,
second denials, private 404, no-grant merge 403/405 and non-replayed 409/500.
`TestAttributionMissingUserAndMissingMapping` covers fallback markers.

## Gotchas

Do not pregrant the test user, modify real accounts, weaken branch protections, or
remove existing collaborators. Private Forgejo permission denial may be 404.
The new sandbox grant remains retained evidence, not an automatic cleanup target.

Run enabled, `--mapping absent` and `--mapping empty`. The enabled verifier first
attempts an AGS-denied create and proves zero provider PR/grant, then restores only
the isolated fixture's AGS access and observes one write projection. Adapter tests
revoke admission before the grant; Service tests read fresh revoked grants and
suspended users. No AGS denial may become Sudo or bot fallback. Empty maps produce
no text mutation, comments, Sudo or ACL writes.
