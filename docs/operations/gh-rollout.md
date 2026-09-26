# Personal-first rollout of official gh and CI backends

## Order and authority

The first live upgrade target is the maintainer's personal primary. A shared/team
primary stays on its accepted release until the personal rollout has survived
real development, current CI data and restarts. Source development can happen on
a separate machine; source acceptance does not switch a running service.

Keep the existing single root, data, repository IDs, users, credentials and Git
remotes. The new native program should serve the installed old client as well as
official gh with the optional companion. Never start a second owning primary on
the same SQLite file or Git store. No historical multi-version runtime is revived.

## Gates

1. Close source defects and run tests with the exact compiler in `.go-version`.
   Commit linearly, scan committed history and private operator literals, and run
   hosted full CI. A dirty-source local acceptance is useful development evidence,
   not the final release gate.
2. Exercise old-to-new SQLite migration from each deployed version. On a real
   consistent snapshot, compare existing rows, Git refs, identity and foreign-key
   checks. Test repository/token lifecycle after new run/CI rows exist; additive
   tables do not by themselves prove backward compatibility.
3. Publish an immutable explicit RC through the existing release entry. On the
   personal target, preserve a bounded pre-write recovery point, stop/drain the
   owner, replace only the installed program set, migrate required configuration,
   and restart. Do not replace the team target or independent Edge just to make
   versions look uniform.
4. Verify old-client reads/writes and an isolated canary first; then official gh
   origin routing, CI/projection and companion task identity. The companion uses
   separate configuration from the existing team's client during trial.
5. Prove real restart, original-user denial through warm Edge data, read/write
   identity and old-client compatibility. Keep one current rollout receipt with
   exact program/source IDs and any remaining limitations. Open a team rollout
   only after these personal-host gates and representative dogfood work pass.

## CI migration is explicit, not a product default

Repositories already using a provider keep that choice in `ci.repositories`.
Omission/default native is not an instruction to discard deployed provider CI.
Discover current branch protection and CI names; do not guess required checks,
remove review requirements, or reinterpret an unconfigured policy as known-empty.
CI backend selection must not select merge authority or change how code is stored.
Unsupported provider operations/large-history observations must be resolved or
explicitly excluded from rollout, not hidden behind successful fixture tests.

## Data recovery and bounded storage

Installation rollback selects exact immutable bytes but is not a database undo.
Before reopening writes, prove whether the prior binary can operate on the migrated
schema; otherwise a consistent old snapshot with its matching program/config is a
separate recovery operation. After new writes exist, never restore an old database
alone and silently lose or split facts. Prefer forward repair and exact readback.

Migration rehearsals use a consistent temporary snapshot and no live provider
effects. Retain only the recovery material required for the current cutover with
an explicit disposal condition. Do not accumulate release directories, permanent
shadow environments or whole-database copies after every routine program upgrade.
Private hostnames, task details, token paths and live receipts remain outside the
public source. Moving retired material to Trash is not physical disk reclamation.

## Companion adoption

The executable remains `agsx`, distinct from the installed `ags-cli`. Ordinary
Git/gh commands remain unwrapped. First trial host setup and read-only diagnosis,
then the generated environment at one task-launch boundary. A native Agent identity
must already exist; mat-only task launchers need an explicit source-identity intake
plan, not a Human fallback or recreation of per-command grant operations.
A client release, public repository, default-account change or team workflow switch
is not implied by server RC publication.
