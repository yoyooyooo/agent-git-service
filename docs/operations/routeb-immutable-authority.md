# Route B immutable authority archives

This is a versioned recovery contract for files that the runtime account may
read but cannot replace. It neither grants privilege nor provisions a host.
The ordinary v1 contract remains strict: it preserves each object's original
ownership, mode, ACL, xattrs and mtime. A v1 descriptor cannot add an authority
flag to avoid that requirement.

## Why an explicit second version exists

The running recovery program, fixed identity and manager authority must remain
outside an application's restore switch. The existing `tool`, `launcher`,
`manager` and `policy` roles already have `provision_separately=true`. That does
not make them optional. A normal non-root archive cannot reproduce root
ownership, and its own UID must never be reported as restored root metadata.

`ags.recovery-group.v2` allows a file in one of those four roles to carry an
`immutable_authority` descriptor. Roles do not change, and directory authority
is not supported in this slice. A user-owned launcher or manager file continues
through the ordinary copy and metadata path. Runtime environment, secrets,
SQLite, Git, Wiki, CAS, old binary and installation receipt cannot use this
exception.

## Independent evidence required before quiescence

`routeb_immutable_authority.verify` reads each authority without following
symlinks. Its complete ancestry must be root-owned and not group/other
writable, and the runtime account must lack write access to the file and all
ancestors. The latter check also rejects ACL permission to rename an otherwise
root-owned file from a writable directory.

Each descriptor binds these independent artifacts:

| Artifact | Required binding |
|---|---|
| Live authority | Exact absolute source, original UID/GID/mode/mtime/ACL/xattrs, size and content SHA-256 |
| Recoverable asset | A separate regular file in owner-private storage, same bytes, different device/inode pair, no symlink or hard link |
| Root approval | Closed `ags.immutable-authority-approval.v1`, human identity, authorization ID, host, tool manifest, installer digest, validity window and exact per-object records |
| Root provision receipt | Closed `ags.immutable-authority-provision.v1`, matching approval digest/ID, host, path, content and metadata, installer and in-window provision time |
| Executing tool bundle | Actual manifest digest and verified member bytes; a `tool` object must be that manifest or one of its bound members |

The host loader additionally requires every member of the executing bundle,
the manifest itself and the fixed host identity to appear in the group with
explicit authority evidence. An inventory containing only a representative
tool file is incomplete. Missing approval, receipt, independent asset or live
authority is a refusal before any stop or snapshot creation.

## Archive, independent restore and old start

The S1 producer records `ags.backup-manifest.v2` and
`ags.local-backup.v3`. It copies each recoverable asset into the private payload
and records the archive's **actual** UID/GID/mode/ACL/xattrs/mtime. The original
root metadata lives separately in `authority_evidence`, bound to the
descriptor, host and tool manifest. The producer rechecks live authority and
the full source fingerprint before sealing. It does not chown a private copy
or claim that a root-owned file was restored.

S2 creates an independent ordinary copy, checks the archived bytes and actual
archive metadata, and separately rereads the live immutable authorities and
provision records. Its `ags.restore-verification.v2` report includes
`immutable_authorities` and the digest of the exact authority evidence map.
V1 manifests, seals and reports cannot stand in for those newer schemas.

Full-group recovery still leaves separately provisioned roles to their
provisioner. `old_start` reloads the sealed group, inventory and restore report,
rechecks the current immutable authorities, and compares the entire current
source fingerprint with the original Tq fingerprint. A missing or changed
tool, user-owned launcher, manager or policy refuses old start until its
independent provisioner restores the approved state. An `applied=true` result
does not override that check.

The fresh old-start approval is consumed into an owner-private journal, whose
path is derived from the sealed group's backup storage path. A caller cannot
change that path to reuse the same approval in another private directory. The
file and directory are fsynced before invoking the launcher. Unknown launch
outcomes leave the consumed marker in place and are never retried. These
component APIs are not a production recovery shortcut; the host coordinator
and privileged provisioner retain their separate approval and identity gates.

## Evidence boundary

Component tests can inject root approval and receipt reads to exercise the
archive format with a real read-only root file. Such tests are fixtures, even
when ordinary copy and metadata comparisons use the real filesystem. Native
evidence requires actual independent provisioning, manager lifecycle,
recovery, once-only launch, business reads, at least 600 seconds of bound
observation, and verified cleanup. No schema or fixture result grants
`installable=true`.
