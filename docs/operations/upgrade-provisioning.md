# Upgrade bundle provisioning

Route B host mode adds `routeb_host_transaction.py` and `routeb_observation.py`
to the closed bundle member list. `scripts/routeb_provision.py` is a read-only
plan: a missing native evidence file, a skipped native run, a wrong owner, or
an unreviewed interpreter closes the plan. A closed plan is not a pass and
does not install.

`scripts/upgrade_bundle.py` builds a closed staging bundle. It copies only the
reviewed member list, writes a manifest that does not include its own digest,
and verifies that staging directory read-only. Verification success sets
`installable` false. The manifest must match the fixed shape in
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
is not executed.

Root provision, helper installation, and the first launcher transition remain
separate approved operations. See
[local upgrade](local-upgrade.md#fixed-bundle-and-execution-identity) and
[user runtime](user-runtime.md#cold-window-startup-fence).

## Offline staging manifest

`upgrade_bundle.verify` accepts a staging manifest only when the manifest file
and every condition below hold. The final path component is not followed. A
missing path is `manifest_missing`. A symlink, including a dangling link, a
link chain, or a link to a file inside or outside staging, is
`manifest_type_rejected`, as is a directory, fifo, or other non-regular file.
An unreadable file is `manifest_unreadable`. The opened descriptor must still
be that regular file and be owned by the current UID (`owner_rejected`). Any
executable bit is `unexpected_executable` unless the caller passes
`executable=False`. That flag skips only the executable-bit check. It does not
accept another file type, owner, or budget, and it does not make the bundle
installable. If the platform cannot open without following a final symlink, or
cannot open nonblocking, verification refuses with `manifest_type_rejected`
instead of following the path. The reader then takes at most 65536 original
bytes plus one extra byte from that descriptor only. A metadata or path
identity change observed across the read is `manifest_changed`. None of these
codes echoes the path or the file body.

The original file is at most 65536 bytes, and the reader stops after one extra
byte. Those bytes are strict UTF-8. No JSON object contains a duplicate key at
any depth, even when both values are identical. The only top-level fields are
`schema`, `entry` and `files`. `schema` is `ags.upgrade-tool-bundle.v2` and
`entry` is `upgrade_host.py`. `files` maps the closed member list to SHA256
strings. A shape, encoding, duplicate-key, empty, truncated, or
nonstandard-constant failure is `manifest_invalid`. A larger original file is
`manifest_budget_exceeded`. Neither code echoes the input. No caller, flag, or
environment variable can raise the budget.

Those manifest failures exit 69. Refusals for the member set, an unexpected
member, member type, owner, digest, or executable bit keep their existing
codes. Manifest file type, readability, and identity use the codes above;
manifest owner and executable bit use `owner_rejected` and
`unexpected_executable`. Success is still `verified=true` and
`installable=false`. `manifest_sha256` is the SHA256 of the original bytes,
not of a re-encoded document.

This check does not replace the root-owned runtime bundle check in
[local upgrade](local-upgrade.md#fixed-bundle-and-execution-identity). It does
not prove a native or M6 run and does not bind recovery assets. The runtime
parser is unchanged, including its handling of duplicate keys.

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
