# Official gh, origin routing and run association

The product path is unmodified official Git/gh, not an AGS command shim. GitHub
origin selects GitHub; an authenticated AGS hostname selects AGS. No per-command
GH_HOST/GH_REPO injection or wrapper PR command is needed.

## One-time host access

Provide a trusted HTTPS API at the hostname used in Git remotes. Git may retain
HTTP port 6666 (and its URL-scoped Edge route) while gh uses the same hostname's
standard HTTPS API. Distinct hostnames need their own valid certificate and gh
host authentication. A configured HTML URL is not an authentication router.

Official gh can keep multiple host accounts. Avoid global GH_HOST/GH_REPO and
broad token environment overrides when expecting repository-origin selection.
A repository with origin plus backup/upstream can explicitly choose once:

```bash
gh repo set-default origin
```

This is repository configuration, not a shell shim. The optional independent
companion can plan/verify an AGS credential import into one official gh host
entry and diagnose origin/overrides. It does not change GitHub accounts, remote
URLs, global PATH, or how ordinary gh commands are parsed.

The authenticated API supports HEAD repository discovery, draft creation, commit
connections with no CI, and current configured CI evidence. Not every GitHub
endpoint is fully replicated; unsupported external Actions management/check-run
REST capabilities are explicit rather than silently using unrelated native state.
See [CI backends](../architecture/ci-backends.md).

## Standard merge

Standard REST `sha` and GraphQL `expectedHeadOid` are enforced. For native same-repo
merge, the source and destination refs are frozen, normal AGS policy is checked,
objects are prepared, then one Git ref transaction verifies the source head and
CAS-updates the base. Repository receive hooks remain honored. Another Git process
changing either ref causes rejection without rolling its write back.

Native cross-repository atomic merge is explicitly unsupported in this change;
no same-repo condition is silently applied to an unrelated fork ref. A configured
provider merge authority continues through that exact authority, independently of
CI choice. A provider receipt is not an AGS merged fact; pending reconciliation is
reported for readback, not retried or reclassified as a local merge.

The ref transaction is not an atomic transaction across Git, SQL and external
providers. Existing reconciliation continues to own those boundaries.

## Task identity without command grants

A task launcher can use a preconfigured native AGS Agent account to create one
short-lived standard-client run credential:

```text
POST /api/ext/v1/client-runs
GET /api/ext/v1/client-runs/{uuid|current}
DELETE /api/ext/v1/client-runs/{uuid}
GET /api/ext/v1/repos/{owner}/{repo}/pulls/{number}/context
```

The companion applies its generated environment once while launching a task:
private GH_CONFIG_DIR plus standard URL-scoped Git credential helper settings.
PATH is unchanged, and official gh handles its normal sequence of GraphQL/REST
queries and mutations. The same helper can bind the HTTP Git origin and the HTTPS
API hostname without changing either transport.

The run credential inherits the native actor, parent-token validity and current
repository rights. It does not create users, collaborators, roles or task-specific
permission classes. Native parent revocation and user suspension take effect on
subsequent API and replication reads. Concurrent runs have independent revocation
and share an actor rate-limit bucket. Run credentials cannot list/mint durable
account tokens or install durable credential keys.

Context identifiers do not prove identity. Self-reported metadata is provisional;
empty context is unlinked. An optional fixed-egress execution-context source can
verify association, but its external Agent must already map to the authenticated
native actor. Mismatch is conflict, not permission to change actor. Source outage
or missing metadata does not revoke already-valid collaboration authorization.
PR association is written after business creation; failure is a warning/pending
fact and cannot cause another PR POST. The query includes durable provider links.

This does not automatically migrate a mat-token-only Multica task runner. The
launcher must establish a valid native AGS Agent identity first. No Human/admin
fallback or old per-operation transport ticket is silently inserted.

Run setup accepts an optional caller UUID so a lost POST response can be revoked
without replaying issuance. Raw bearer returns once with no-store; SQL stores a
hash. TTL is 60 seconds to 8 hours, bounded by the parent expiry, and unreferenced
expired setup records are collected. Associations already attached to PRs remain
business provenance rather than installation history. No automatic renewal daemon
or production launcher mutation is included.

## Validation and rollout boundary

```bash
go test ./internal/cibackend ./internal/integrations ./internal/gitstore
go test ./internal/service -run 'Test(ClientRun|CIBackend|CIChecks|CIPolicy|CICancel)'
node scripts/gh-native-acceptance.mjs
node scripts/gh-native-acceptance.mjs --companion /absolute/companion/.test-build/candidate
```

The official-gh script uses a fixed hash-verified release and this exact checkout,
with synthetic credentials, isolated HTTPS and CI protocol fixtures, and temporary
SQLite/Git data in a loopback-only namespace. The optional companion test exercises
its actual setup/session commands followed by system Git and official gh, not a
mock client.

This source work does not install certificates, change tailnet grants, switch live
mini/imile/CCS services, publish packages or move existing team credentials. Those
remain explicit rollout decisions after source review and target-specific CI/task
launcher compatibility checks.
