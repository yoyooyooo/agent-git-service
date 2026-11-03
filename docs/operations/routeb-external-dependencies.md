# Route B external dependency contract

Preparation states the owner, object set, version and metadata fields,
stop and restore order, and exact-effect readback for each dependency.
It does not back up or restore the live objects. A missing owner contract
makes `scripts/ags-maintenance-transaction plan` refuse the claim. The host-package page is [routeb-host-package](routeb-host-package.md).

Shared Forgejo and Multica state are not rolled back by restoring one AGS
data directory. Rollback of AGS remains the whole AGS cold backup to
`0013aa744c8c90637ec54562ffc248a1bf7c9a52` after stop. There is no down
migration for `lifecycle_generation`, `forgejo_reopen_pending` or
`forgejo_lifecycle_at`.

Candidate source is `f63aa88e0c82b1e17174cf07f8033552457185b1`.
`/readyz` does not prove these dependencies. `forgejo_authority_policy` is
`diagnostic_only`. `/readyz` is not a webhook proof.

`scripts/upgrade` still returns `backup_contract_unimplemented`.
The launchd and systemd adapter modules still raise
`native_adapter_unavailable` from `create()`.

## Forgejo

| Item | Contract |
|---|---|
| Owner | Forgejo operator, not the AGS binary owner |
| Objects | database, repositories, LFS, Actions logs and artifacts, app.ini, secrets and keys, runner and job state |
| AGS-local objects | token, webhook secret and operator secret files, recorded by mode, owner and content hash only |
| Stop | freeze projection, webhook and job writers before the AGS stop; stopping Forgejo itself is a separate approval |
| Restore | not with the AGS Tq; shared data stays on the Forgejo timeline |
| Readback | base SHA, policy-managed head, pull-request state and permission, webhook signature check, exact ref |
| Unknown | stop; do not replay a provider write whose outcome was not read back |

## Multica

| Item | Contract |
|---|---|
| Owner | Multica operator |
| Objects | Multica database, uploads, tasks, runs and completion state |
| AGS-local objects | `~/.multica/config.json`, profile config, link and service token files; owner-only |
| Stop | queue drain is a window action, not a preparation action |
| Restore | restoring local config does not roll back a remote task |
| Readback | task coordinates recorded before the window; no blind projection replay |
| Unknown | stop |

## Caddy and Funnel

| Item | Contract |
|---|---|
| Owner | host proxy operator |
| Objects | live config, install receipt and version, certificate and ACME state, loaded plist or unit and drop-ins |
| Excluded | admin socket and other runtime sockets |
| Metadata | content hash, byte length, mode, uid, gid, ACL digest, xattr digest, mtime_ns |
| Stop | AGS binary replacement does not restart Caddy |
| Restore | separate window; `caddy validate`, config hash, certificate and route, then public readback |
| Unknown | leave Caddy as it was; do not repair it inside the AGS transaction |

## Tailscale serve and service

| Item | Contract |
|---|---|
| Owner | tailnet operator |
| Objects | identity, keychain or daemon store, Serve and Funnel JSON, ACL, DNS, certificate, re-auth procedure, off-host archive |
| Recorded here | credential-free route and hash summary only; no token or private key |
| Not sufficient | `tailscale serve status` or funnel status text |
| Stop | AGS window does not change Tailscale; daemon or serve changes are another window |
| Unknown | stop |

## launchd plist and systemd unit

| Item | Contract |
|---|---|
| Owner | host service operator |
| Objects | loaded plist or unit, drop-ins, EnvironmentFile, ExecStart, User, uid, scope, Restart, Timeout, triggers |
| Store | copy bytes to a root-owned or owner-only audit directory outside the source checkout; hash the bytes |
| Forbidden | replacing the live definition with a repository template |
| mini readback | disabled state and job presence, recorded by the host package |
| imile readback | one scope, empty cgroup, closed trigger, readiness |
| This task | documents the fields; does not stat or hash a live plist or unit |

## Metadata

The cold-backup manifest records sha256, mtime_ns, mode, uid, gid, ACL digest
and xattr digest. Independent restore compares bytes and metadata, SQLite
integrity, foreign keys and a logical digest, `git fsck`, refs and OIDs, and
Wiki CAS to database body mapping. An unsupported metadata field is a refusal.
Tar success is not a pass. Online copy and the earlier rehearsal are not the
production Tq.

## Synthetic readback

`scripts/routeb_dependency_readback.py <contract.json>` checks one
caller-supplied contract for owner, object scope, version, ACL digest, mtime,
content hash, stop order, restore order, exact-effect readback and
`unknown=stop`. It does not open live configuration. Shared dependencies must
set `rolls_back_with_ags` false. A missing field or hash mismatch refuses the
claim. Real backup and restore remain a separate window.
