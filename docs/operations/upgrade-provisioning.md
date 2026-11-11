# Upgrade bundle provisioning

Route B host mode adds `routeb_host_transaction.py` and `routeb_observation.py`
to the closed bundle member list. `scripts/routeb_provision.py` is a read-only
plan: a missing native evidence file, a skipped native run, a wrong owner, or
an unreviewed interpreter closes the plan. A closed plan is not a pass and
does not install.

`scripts/upgrade_bundle.py` builds a closed staging bundle. It copies only the
reviewed member list, writes a manifest that does not include its own digest,
and verifies that staging directory read-only. Verification holds one
directory descriptor for the listing, the manifest, and the members, and
checks that the caller path still names that directory before success. See
[Offline staging directory](#offline-staging-directory). Verification success
sets `installable` false. Source bytes are copied from the same no-follow
descriptor that was checked; see
[Offline staging sources](#offline-staging-sources). Each output file is
created exclusively and checked on that descriptor; see
[Offline staging outputs](#offline-staging-outputs). The manifest must match
the fixed shape in
[Offline staging manifest](#offline-staging-manifest). It does not rename the
staging directory onto `/usr/local/libexec/ags-autodeploy` and does not change
a host.

The member list includes the S6.01 identity module, S6.02 evidence and bootstrap
modules, both OS adapters, the metadata and trash clients, this builder, and
`ags-python.sh`. A three-file bundle fails the existing bundle digest check
with exit 69. A missing member, extra executable, tampered file, wrong owner,
or symlink also fails. `manifest.json` itself must be a regular file owned by
the current user; verification does not follow a symlink in its final
component, and an executable bit is refused by default. An incomplete directory
is not executed. Ordinary members are hashed from the same no-follow
descriptor that was checked, not from a later path read. See
[Offline staging members](#offline-staging-members).

Root provision, helper installation, and the first launcher transition remain
separate approved operations. See
[local upgrade](local-upgrade.md#fixed-bundle-and-execution-identity) and
[user runtime](user-runtime.md#cold-window-startup-fence).

## Offline staging sources

`upgrade_bundle.build` checks each closed member before copying it. It
`lstat`s the source path and does not follow that final component. The only
alternate path is `release-source.json`. When the first `lstat` of the primary
path returns `ENOENT`, the builder reads `fork/release-source.json` beside the
source directory. A symlink, including a dangling link or a chain, a directory,
a fifo, or a permission error is not absence and does not select that fallback.
If both files exist, the primary file is used. After a path is selected, a
later absence does not switch to the other file.

The selected path is opened once, `O_RDONLY`, without following the final
component and without blocking. If the platform cannot open that way, the
builder refuses with `member_missing` and does not fall back to a following
read. The descriptor must be a regular file whose device, inode, mode, uid,
size, mtime, and ctime match the pre-open `lstat`. Atime is ignored. The
builder does not require the current UID, mode 0600, a link count of one, or a
clear executable bit. A stable hard link is accepted. Source modes 0600, 0644,
0700, and 0755 remain compatible. The staged copy is still mode 0644, set on
the destination descriptor; see
[Offline staging outputs](#offline-staging-outputs). A source executable bit
is not a staging executable bit.

Bytes and the SHA256 are taken from that same descriptor. Each read is at most
65536 bytes. The read stops at the size from the first `fstat`, plus one extra
byte so growth is noticed and is not copied. A short read continues. An
interrupted open, read, `fstat`, or `lstat` is retried only at that call. An
early end or a longer file is `source_changed`. The copy is the original bytes,
not decoded or normalized text. Neither the copy nor the digest reopens the
source path.

A static miss, a non-regular file, or a final symlink is `member_missing`,
including one substituted before the open. That is the builder's existing code,
not the verifier's `member_type_rejected`. An identity, length, mode, or
final-path change observed around the read is `source_changed`, including the
same bytes on a new inode. A source permission failure, or another source read
or `fstat` failure, is `source_unreadable`. It is not raised as a normal
permission exception. A destination write failure stays a destination
`OSError`: it is not `source_unreadable` and it does not select the fallback.
A destination path that no longer names the new file is `destination_changed`,
as defined in [Offline staging outputs](#offline-staging-outputs). The message
is only the fixed code. The exit code is 69.

The descriptor is closed on success, on refusal, and when a programming error
leaves the copy. The manifest is created exclusively only after every selected
member has been copied and still matches its opening identity. See
[Offline staging outputs](#offline-staging-outputs). A refusal may leave an
incomplete staging directory. That directory is not success, is not reused, and
is not deleted by the builder. An existing staging directory is still
`staging_exists`. Success is `verified=true` and `installable=false` only after
the checks in [Offline staging manifest](#offline-staging-manifest) and
[Offline staging members](#offline-staging-members). The builder does not prove
a Git revision or tree, does not admit production, and does not bind a host,
tool revision, or recovery asset. It does not snapshot every ancestor or bound
the whole bundle by total bytes or time.

## Offline staging outputs

`upgrade_bundle.build` creates every required member and `manifest.json` by
an exclusive open of that final component: `O_WRONLY|O_CREAT|O_EXCL`, without
following the component and without blocking. If the platform cannot open
that way, or cannot `fchmod` a descriptor, the builder refuses and does not
fall back to truncation or a following write. An existing final component is
not unlinked, renamed over, or reused. That includes a regular file, a hard
link, a symlink (dangling or chained), a directory, and a fifo. The open
fails with the destination `OSError`. A member that already exists still
surfaces as `FileExistsError` / `EEXIST`. The manifest uses that same
destination failure. These errors are not rewritten into one bundle code.

Member bytes and mode 0644 are applied to the descriptor just created. The
mode is `os.fchmod` on that descriptor, including when the process umask is
077. The builder does not `chmod`, `chown`, or truncate the destination path
after the descriptor is opened. The manifest is created with mode `0666`
masked by the current umask, the same creation rule as before. It is not
forced to 0644 or 0600. The staging directory remains mode 0700 under its
existing creation rule. The builder does not change the process umask.

The builder `fstat`s the new descriptor and requires a regular file. Device,
inode, type, and uid come from that first check. Size, mode, mtime, and ctime
change when the builder writes and, for a member, changes the mode, so those
fields are not compared with the creation snapshot. After the builder's own
writes finish, the size must equal the number of bytes that were supposed to
be written. Stable metadata from the descriptor is then compared with a
non-following `lstat` of the final path, ignoring atime, and a further
`fstat` must still agree. If the path is gone, is a link or another type,
names a different inode, or disagrees in length or identity, the result is
`destination_changed`, exit 69, and the message is only that code. The same
bytes on a replacement inode are still a refusal. Digest equality is not
enough.

The manifest is serialized only after every member copy, source check, and
destination check has succeeded. Its bytes use the same write loop as member
bytes. A short write continues. `EINTR` from `write` is retried only for that
call. A return of zero or a negative count is refused. The build returns
success only after the manifest length, identity check, and its one close all
succeed.

Each output descriptor is closed once, on success and on failure. `EINTR`
from `close` is not retried, and the descriptor is not closed again. Whether
the caller is already handling an exception does not change that close. A
`BundleError`, destination `OSError`, programming error, or cancellation that
already happened is kept. A `close`, `fchmod`, `fstat`, or `write` `OSError`
after the bytes were produced is not success, is not `source_unreadable`, and
does not select the fallback. A destination I/O error stays a destination
`OSError`. Only an observed identity or path mismatch is `destination_changed`.

Failure is not an atomic rollback. The builder does not return success, does
not reuse the staging directory, does not delete it, and does not write an
approval, pin, or registration. A member refusal does not continue on to the
manifest. A manifest collision does not modify the object already at that
path. If the manifest was created and a later write, identity check, or close
fails, a partial or even byte-complete `manifest.json` may remain. That
residue is not a successful build. Verification still checks only the files it
reads. Power-loss durability, `fsync`, and atomic publication are not part of
this check.

The check covers the life of that output descriptor. The builder does not
write an object that already occupied the path, and it does not change that
object's mode. If the path is replaced by another inode, later bytes and the
mode change still apply only to the original descriptor, and the path mismatch
is refused. The builder cannot stop the same user from linking that inode,
writing the inode directly, replacing the path again after the last check, or
moving an ancestor or the whole staging directory. It does not claim same-user
isolation or a consistent snapshot of the directory.

Success remains `verified=true` and `installable=false` only after
[Offline staging manifest](#offline-staging-manifest) and
[Offline staging members](#offline-staging-members). The source check is
[Offline staging sources](#offline-staging-sources). This output check does
not admit production and does not make the staging directory a host trust
root.

## Offline staging directory

`upgrade_bundle.verify` binds the caller's staging path before it reads a
manifest or member. It `lstat`s the final component and does not follow that
component. A symlink, including a dangling link or a link chain, is
`staging_type_rejected`. A regular file, fifo, or other non-directory uses
the same code. The fifo check does not wait for a writer. A missing path,
including a parent that is not a directory, stays `manifest_missing`.

The directory is opened once, `O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_NONBLOCK`.
`fstat` must show that same directory object. Listing uses that descriptor.
`manifest.json` and each required member are a single basename. Their
no-follow `stat` and `open` use that basename and the same descriptor. They
are not joined back onto the caller path. A slash, `..`, or another directory
component is not looked up. If the platform cannot open a directory without
following, cannot `stat` through a directory descriptor, or cannot list a
descriptor, verification returns `staging_unreadable` and does not fall back
to path reads.

Before success, `fstat` of the held descriptor and a no-follow `lstat` of the
original final component must still name that directory object: device,
inode, and type. Size and timestamps are not compared. A root that was
replaced, removed, or changed type is `staging_changed`, including when the
replacement directory has the same bytes. Verification may notice that change
before the last check. It does not read files from the replacement. A
directory `fstat`, listing, or close failure is `staging_unreadable`.

The directory descriptor is closed once. The close is not retried, including
after EINTR, and it is not closed again. Success requires that close. A close
OSError is `staging_unreadable`, exit 69, and the message is only that code.
If verification had already raised, including a manifest or member refusal, a
programming error, or cancellation, that earlier exception is kept. A
directory close OSError does not replace it. An exception the caller is
already handling is not this close's failure. One close attempt does not
prove that every OS released the descriptor after an unknown close error.

`executable=False` still skips only a file executable bit. It does not accept
a symlink root. A directory search bit is not `unexpected_executable`. The
check does not require mode `0700`, a directory owner rule, or an nlink rule.
An ancestor symlink remains allowed when the final component is an ordinary
directory. Mode `0755` remains acceptable.

The descriptor pins the directory used for lookup. It does not freeze
members, prove they were consistent at one instant, or catch every case where
the root is replaced and restored before the last check. It does not cover a
change after that check. It does not chmod, chown, rename, unlink, or execute
a member, and it does not write a marker, pin, approval, or registration. It
does not admit production. Success remains `verified=true` and
`installable=false`. Creating the directory is a separate output check; see
[Offline staging outputs](#offline-staging-outputs). The manifest and member
checks below run only after this directory is held.

These codes are `BundleError`, exit 69, and the message is only the code.

## Offline staging manifest

`upgrade_bundle.verify` accepts a staging manifest only when the manifest file
and every condition below hold. The file is the basename `manifest.json` in
the [held staging directory](#offline-staging-directory), not a path joined
back to the caller. The final path component is not followed. A
missing path is `manifest_missing`. A symlink, including a dangling link, a
link chain, or a link to a file inside or outside staging, is
`manifest_type_rejected`, as is a directory, fifo, or other non-regular file.
An unreadable file is `manifest_unreadable`. The opened descriptor must still
be that regular file and be owned by the current UID (`owner_rejected`). Any
executable bit is `unexpected_executable` unless the caller passes
`executable=False`. That flag skips only the executable-bit check. It does not
accept another file type, owner, or budget, and it does not make the bundle
installable. If the directory is already held and a manifest open would have to follow or
block, verification refuses with `manifest_type_rejected` instead of
following the path. A missing directory-open capability is
`staging_unreadable` and is not turned into a path read. The reader then
takes at most 65536 original
bytes plus one extra byte from that descriptor only. A metadata or path
identity change observed across the read is `manifest_changed`. None of these
codes echoes the path or the file body.

The opened descriptor is closed once, whether this read returns or raises.
The close is not retried, including when it returns EINTR, and verification
does not close that descriptor again. The read is finished only when the
close succeeds; only then are the original bytes parsed. A close OSError
after the read itself succeeded is `manifest_unreadable`, exit 69, and the
message is only that code. Parsing, the directory listing, and member reads
do not continue. EIO, EINTR, EBADF, and every other close errno use that same
code. The close does not select `manifest_missing` or `manifest_type_rejected`.
If this read had already raised, including a budget, identity, owner, or
executable refusal, a programming error, or cancellation, that earlier
exception is kept. A close OSError does not replace it. An exception the
caller is already handling is not this read's failure. `executable=False`
does not ignore a close failure. One close attempt does not prove that the
descriptor was released after an unknown close error. The failure does not
modify the manifest or write a marker. Success remains `verified=true` and
`installable=false`.

The original file is at most 65536 bytes, and the reader stops after one extra
byte. Those bytes are strict UTF-8. No JSON object contains a duplicate key at
any depth, even when both values are identical. The only top-level fields are
`schema`, `entry` and `files`. `schema` is `ags.upgrade-tool-bundle.v2` and
`entry` is `upgrade_host.py`. `files` maps the closed member list to SHA256
strings. A shape, encoding, duplicate-key, empty, truncated, or
nonstandard-constant failure is `manifest_invalid`. A larger original file is
`manifest_budget_exceeded`. Neither code echoes the input. No caller, flag, or
environment variable can raise the budget. That cap is the manifest document
only. It is not a limit on member size, and the member reader does not reuse
it as one. Member bytes are hashed in separate chunks of at most 65536 bytes.
Input, flags, and the environment cannot widen either rule.

Those manifest failures exit 69. A directory listing that does not match the
closed member set is still `unexpected_member`. Member type, owner, digest,
and executable bit keep `member_type_rejected`, `owner_rejected`,
`digest_rejected`, and `unexpected_executable`. A member that disappears
after that listing and before its first lookup is `member_missing`. A change
of identity or length, or a final-path mismatch, is `member_changed`.
Member permission and IO failures are `member_unreadable` and do not surface
as a normal permission exception. Manifest file type, readability, and
identity use the manifest codes above; manifest owner and executable bit use
`owner_rejected` and `unexpected_executable`. Success is still
`verified=true` and `installable=false`. `manifest_sha256` is the SHA256 of
the original bytes, not of a re-encoded document.

This check does not replace the root-owned runtime bundle check in
[local upgrade](local-upgrade.md#fixed-bundle-and-execution-identity). It does
not prove a native or M6 run and does not bind recovery assets. The runtime
parser is unchanged, including its handling of duplicate keys.

## Offline staging members

After the manifest is accepted, each closed member is checked on one
descriptor. The member name is a single basename in the
[held staging directory](#offline-staging-directory). Verification `stat`s
that name without following it, rejects a symlink or other
non-regular file, then opens it `O_RDONLY` without following the final
component and without blocking. It `fstat`s that descriptor and compares
device, inode, mode, uid, size, mtime, and ctime with the pre-open
no-follow stat. Atime is ignored because a read may change it. The current UID is checked
next. Unless the caller passes `executable=False`, any executable bit is
`unexpected_executable`. SHA256 is updated from that same descriptor in
chunks of at most 65536 bytes. The read stops at the size from the first
`fstat`, plus one extra byte so a growing file is noticed and is not followed
to its new end. A later `fstat` and a final no-follow stat of that same
basename must still name that file. The digest is compared only after those
checks. The descriptor is
then closed once. The close is not retried, including after EINTR, and it is
not closed again. This member is accepted only if that close succeeds. A
close OSError is `member_unreadable`, exit 69, and the message is only that
code; later members are not opened. Every close errno uses that code, not
`member_missing` or `member_type_rejected`. If this member had already
raised, that earlier exception is kept and a close OSError does not replace
it. An exception the caller is already handling is not this member's failure.
One close attempt does not prove that the descriptor was released after an
unknown close error. If the directory is already held and a member open would
have to follow or block, verification refuses with `member_type_rejected`
instead of following the path. No input, flag, or
environment variable selects a larger chunk or removes the extra-byte stop.
The chunk size is not the manifest's 65536-byte document budget. A member
may be larger than that budget, and an empty member is valid when its
digest matches.

`executable=False` skips only the executable-bit check. It does not accept
another type, owner, identity change, or digest, it does not ignore a close
failure, and it does not make the bundle installable. This current-UID rule is the offline staging rule, not
the root-owned runtime bundle rule.

A static symlink, including an internal, external, relative, absolute,
dangling, or chained link, is `member_type_rejected`, as is a directory,
fifo, or other non-regular file. Replacing the path with one of those before
the open uses the same code. An identity, length, or final-path change
observed around the read is `member_changed`. That includes a different
inode with the same bytes, a rewrite, truncation, or growth of the open
file, and a permission change after the open. A permission or other member
IO failure is `member_unreadable`. Disappearance after the directory listing
and before the first member `lstat` or open is `member_missing`. A listing
that already lacks a required name stays `unexpected_member`. These member
failures are `BundleError`, exit 69, and the message is only the fixed code.

The held directory descriptor fixes the directory object used for lookup.
That is not a snapshot of the whole staging directory, and it does not prove
every member was unchanged at the same instant. Replacing the root and
restoring it before the last check, or changing the tree after that check, is
outside this contract. The check does not trust every ancestor or freeze a
member after that member's own check returns. It does
not bound the bundle by total bytes or elapsed time. It does not change
production admission, and success remains `verified=true` /
`installable=false`. The source-file check that produced the staging directory
is separate and is described in
[Offline staging sources](#offline-staging-sources). The creation of those
files is described in
[Offline staging outputs](#offline-staging-outputs).

## S6.12 release inputs

Use the [offline pin preparation contract](releases.md#s612-offline-pin-preparation)
to check synthetic candidate bindings without generating an installable pin.
Do not place its non-installable envelope at the deployment pin path. Only a
separately accepted real immutable Release, both platform crypto/provenance,
exact-source CI and host-specific policy/bundle digests can supply that pin.
No provision, publication switch, release-source selection or host configuration
is changed by preparation. Policy and manifest remain separate from package and
pin so their hashes do not form a self-reference.

## S6.13 dependency-complete staging

The fixed member list now includes `upgrade`, `ags-runtime.py`, all S1–S6 local
imports, consumer dependencies, `upgrade_policy.json` and the frozen
`fork/release-source.json` bytes copied as `release-source.json`. No package,
private profile or pin is discovered from the checkout. The manifest excludes
itself and all private inputs. Every member is copied non-executable; this is a
staging archive, not an installation.

The AST import-closure regression checks actual staged source, not a synthetic
member list. `bundle_engine` independently requires the full exact-digest set.
This fixes the earlier staging builder's missing transitive dependencies but
does not supply a native adapter, privileged helper or bootstrap authority.
A manifest that passes the offline file and content check still supplies none of those
and does not bind recovery.
S6b.09 preserves this boundary: metadata and Trash operations require explicit provisioned callbacks and fail before claim when absent. A fixture readback cannot become ownership or deletion evidence.
No factory may return a no-op Trash or writer probe as production capability.
See [source integration](local-upgrade.md#s613-source-integration-boundary).
Route B exact-source admission (`scripts/routeb_host_admission.py`) is not a
provisioning step and does not install the bundle. The tool bundle now also
requires `routeb_candidate.py`, `routeb_host_admission.py` and
`upgrade_wireup_safety.py`. A bundle missing one of those members is refused.
Candidate build identity for the Route B preparation is in
[route B build](routeb-build.md); that page is not an installable pin.
