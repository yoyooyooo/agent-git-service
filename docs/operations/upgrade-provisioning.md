# Upgrade bundle provisioning

`scripts/upgrade_bundle.py` builds a closed staging bundle. It copies only the
reviewed member list, writes a manifest that does not include its own digest,
and verifies that staging directory read-only. Verification success sets
`installable` false. It does not rename the staging directory onto
`/usr/local/libexec/ags-autodeploy` and does not change a host.

The member list includes the S6.01 identity module, S6.02 evidence and bootstrap
modules, both OS adapters, the metadata and trash clients, this builder, and
`ags-python.sh`. A three-file bundle fails the existing bundle digest check
with exit 69. A missing member, extra executable, tampered file, wrong owner,
or symlink also fails. An incomplete directory is not executed.

Root provision, helper installation, and the first launcher transition remain
separate approved operations. See
[local upgrade](local-upgrade.md#fixed-bundle-and-execution-identity) and
[user runtime](user-runtime.md#cold-window-startup-fence).

## S6.12 release inputs

Use the [offline pin preparation contract](releases.md#s612-offline-pin-preparation)
to check synthetic candidate bindings without generating an installable pin.
Do not place its non-installable envelope at the deployment pin path. Only a
separately accepted real immutable Release, both platform crypto/provenance,
exact-source CI and host-specific policy/bundle digests can supply that pin.
No provision, publication switch, release-source selection or host configuration
is changed by preparation. Policy and manifest remain separate from package and
pin so their hashes do not form a self-reference.
