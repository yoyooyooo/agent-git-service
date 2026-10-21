# Permission recovery

## Sub-features

Below-write user gets write after a definitive Sudo denial. Missing account,
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

Do not pregrant the test user, create accounts, weaken branch protections, or
remove existing collaborators. Private Forgejo permission denial may be 404.
The new sandbox grant remains retained evidence, not an automatic cleanup target.
