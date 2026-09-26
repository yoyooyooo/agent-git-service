# Native gh, replaceable CI, and collaboration context

## Ownership

Official Git/gh owns daily repository/PR/CI interaction and origin selection.
AGS owns GitHub-compatible API semantics, authorization, merge authority and
backend identity. The independent companion owns explicit setup, diagnosis and
association—not a second repo/pr/run command language or a gh shim.

Git hosting/projection, CI observation/execution, and merge authority are separate
choices. Installing Forgejo projection does not implicitly select Forgejo CI.
Absent CI configuration uses the existing native Actions implementation. An
explicit `none` selection disables CI; named backends are selected by repository.
Never fall back to another backend because an observed request failed.

## CI contract

`internal/cibackend` owns backend-neutral run/job/status data and bounded adapter
I/O. `internal/service` authorizes against the AGS repository before backend I/O,
assigns durable API IDs, and binds each result to the configured backend/repository.
REST Actions and GraphQL checks consume this same service. A provider adapter
never sees a user's AGS bearer; provider credentials are fixed, server-owned files.

The first adapters are Forgejo Actions and GitHub Actions. Both use explicit
server configuration and immutable resource coordinates; native Actions remains
the zero-external-dependency backend. Adding another backend means implementing
the typed interface and constructor, not editing PR or gh command behavior.

Run/job API IDs are durable AGS mappings, not bare provider integers. Mapping
identity includes backend kind, API origin, namespace and external repository.
Changing selection cannot reinterpret an old run as a different provider run.
Token rotation does not change resource identity. Removing a binding makes its
previous observations unavailable; it does not silently query the new backend.

Required checks are explicit policy facts. An absent policy is unknown, not an
empty allowlist. Required checks that have no current-head result appear pending;
old-head success and unknown provider statuses cannot satisfy them. Unavailable
provider data produces an error, not an empty successful rollup.

## gh compatibility repairs

Preserve commits when no CI exists; honor draft on GraphQL creation; support HEAD
for repository discovery; and honor expectedHeadOid/REST sha at the owning merge
boundary. Native and configured provider merge paths must retain current server
permission/protection policy. No API silently substitutes a weaker merge path.

## Identity and association

Authentication/authorization and optional Task/Run association have different
failure semantics. A native authenticated actor may start a bounded run session
for standard Git/gh. Its token refers to that same actor plus immutable run
provenance; it does not grant extra roles or add collaborators. Missing metadata
is visible as unlinked/provisional, not an admission gate. Distinct concurrent
runs have distinct tokens, so source correlation never relies on 'latest task'.

The normal session uses ordinary server permissions and compatibility routes;
it is not a per-command grant. Trusted runtime intake remains responsible for
establishing a source identity when no native AGS credential exists. Association
claims alone never mint another actor's credential or override policy.

## Acceptance and non-goals

Real official gh + isolated AGS + loopback CI fixtures must exercise origin-only
selection, draft, no-CI checks, provider switching, current-head/required checks,
job logs, merge preconditions, and session identity separation. All data and
credentials are synthetic. Unit tests validate adapter boundaries and failure
semantics; green mocks alone are not a combined-client acceptance.

No production fleet switch, npm release, new public repository, CI-vendor plugin
marketplace, universal GitHub emulation or legacy command compatibility is implied.
The existing fork generation/root layout and upstream-sync process remain intact.
