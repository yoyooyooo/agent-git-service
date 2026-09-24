# Release, installation and rollback

The maintained public source is `yoyooyooo/agent-git-service`. `origin` refers to
that repository; `upstream` remains `ngaut/agent-git-service`. The dated `fork/*`
branch identifies an accepted downstream generation, not a product-name suffix.

## Source and artifact identity

Every bundle contains `gh-server`, `ags-edge`, `ags-replication`, license material
and `build-info.json`. Each executable supports an exact standalone `--version`
(or `version`) request and returns the `ags.build.v1` JSON identity **before**
reading `.env`, starting a listener or accessing a database. Development binaries
report `development`/`unknown`; they are not release artifacts.

The release manifest binds the version, complete source SHA/tree, platform, Go
version, CGO choice, binary hashes and smoke-test results. No credentials, private
network inventory, runtime configuration, databases or deployment receipts are
packaged. Builds use a verified Git archive outside the checkout, `-trimpath`,
read-only module resolution and explicit linker identities.

The root `.go-version` is the exact supported compiler patch used by all hosted
CI, client checks and native Release builds. The `go` directive in each `go.mod`
remains the module's minimum language/toolchain requirement, not a release
compiler selector. Changing the pin requires a new exact-source CI run and a new
immutable release; published bundles are never rebuilt in place.

`python3 fork/scripts/release.py toolchain` checks the installed compiler against
the selected commit's pin. CI and builds use `GOTOOLCHAIN=local` and `GOENV=off`
to prevent an implicit toolchain switch or operator Go configuration from
changing it. Release construction checks before creating staging; publication
also checks all three binaries' reported Go versions against the same source
pin. A checksum-consistent bundle from a different compiler is rejected.

Use `.go-version` for local release work as well; it does not automatically
upgrade a workstation's Go installation or a running service. The initial
release pin is Go 1.26.8, selected from the official supported release feed.
Review Go security/maintenance releases regularly rather than leaving this
pin at a historical minimum or following an unbounded `latest` selector.

The first supported targets are:

| Bundle | Platform | Runtime constraints |
| --- | --- | --- |
| `darwin_arm64` | Apple Silicon, macOS 15 or later | Native CGO primary; only system dynamic libraries. No Apple notarization claim. |
| `linux_amd64` | x86-64 Linux | Primary built on Ubuntu 24.04, requiring compatible glibc (2.39 baseline); Edge and replication client are CGO-disabled. |

The primary requires native Git at runtime. Optional integrations may require
other explicit tools, such as `zstd` for compressed provider logs. Installing the
bundle does not install those dependencies, certificates or external providers.

## Controlled publication

Release publication is tag-driven. Maintainers do not manually dispatch the
build workflow or hand-type the next version. `scripts/release.ts` is the one
release decision entry and follows the same model as the other maintained
tag-driven projects:

```bash
bun scripts/release.ts status
bun scripts/release.ts rc
# after that RC is accepted without source changes:
bun scripts/release.ts stable
```

`rc` continues the current `fork-YYYYMMDD.N` line with the next `-rcN`.
Once a line has a stable release, the next `rc` starts
`fork-YYYYMMDD.(N+1)-rc1`. `stable` selects the latest unpromoted RC, or an
explicit `--from fork-YYYYMMDD.N-rcN`, and creates
`fork-YYYYMMDD.N` at **the exact same source SHA**. The RC remains immutable.
Use `--dry-run` to show and validate a plan without creating a tag,
`--no-push` for a local-only tag, and `--no-watch` only when another operator
will observe the resulting workflow.

The script requires a clean, current default generation and verifies the native
GitHub fork identity, exact remote branch state, successful exact-source full CI
and secret scan, existing RC publication state for promotion, and absence of tag
or Release collisions. Its only publication write is an exact tag ref. A push of
`fork-*` is the sole normal trigger for `.github/workflows/release.yml`.

The Release workflow treats the tag as immutable input; it **never creates or
moves a tag**. It verifies that the tag already resolves to `github.sha`, then
re-checks the exact-source CI/disclosure gates before building. Ordinary branch
pushes and pull requests cannot publish. For a stable version the final Release
is marked GitHub Latest; RCs remain prereleases and never replace Latest.

Native standard GitHub-hosted runners build both targets. Actual binaries are
checked for identity, primary SQLite startup/schema/integrity, Edge liveness,
loopback diagnostics matching the same compiled build identity, and clean shutdown.
The Edge `/status` response carries `source_revision` plus the credential-free
`build` record used by `--version`; release archives do not depend on Go's optional
VCS metadata to remain diagnosable. Build provenance is attested for each archive. Only the final
publish job has repository-content write permission. It verifies both bundles,
their provenance and uploaded digests before publishing the complete draft.
The repository's immutable-release setting locks the published tag and assets.

A failed draft/upload is retained for inspection. Re-running does not silently
clobber a tag or finish an ambiguous release. The exact Git tag predates the
workflow and is never created, replaced or moved by the publisher. Before final
publication, the complete draft identity, uploaded digests and the pre-existing
tag-to-source binding are checked again.

An operator may explicitly resume a fully uploaded draft using `recover-draft`
with its numeric `--draft-id`, exact `--sha`, version and locally downloaded
original build artifacts. This path revalidates the source's full CI, provenance,
embedded manifests and every remote upload; it never rebuilds, uploads, changes
versions or overwrites content. It refuses an already published release or a
mismatched tag. A tag-triggered run that fails before a complete upload requires
separate inspection, not a blind new tag or source rebuild. A successful source
test or release does not deploy anything.

## One-command install and upgrade

Release archives are installation artifacts, not CI-only attachments. The
public Bash bootstrap is intentionally thin: it resolves one explicit immutable
Release tag, fetches the exact `scripts/install-release.py` blob from that tag,
verifies its Git blob identity, and delegates all bundle/digest/attestation
checks to the Python installer. It does not duplicate release-verification
policy in shell.

The normal path follows GitHub Latest, which must be an immutable stable release:

```bash
curl -fsSL "https://github.com/yoyooyooo/agent-git-service/releases/latest/download/install.sh" |
  bash -s -- install
```

An existing installer-owned installation upgrades to Latest stable with:

```bash
curl -fsSL "https://github.com/yoyooyooo/agent-git-service/releases/latest/download/install.sh" |
  bash -s -- upgrade
```

Use `--version fork-YYYYMMDD.N` to pin or roll back to a specific stable
release. A prerelease is never selected implicitly; RC testing requires an
explicit `--version fork-YYYYMMDD.N-rcN --allow-prerelease`.

The bootstrap requires Bash, Python 3.9+ and a GitHub CLI with Release asset and
attestation verification support. Without `--version`, it resolves
`releases/latest` and rejects draft, prerelease, mutable, non-exact or
unexpected tag identities before downloading anything. It creates only
installer-owned selectors plus `gh-server`, `ags-edge` and
`ags-replication` symlinks under `~/.local/bin` by default. It refuses an
unrelated file/symlink rather than replacing it. `stage` downloads and verifies
without activation; `plan` does not install binaries. There is deliberately no
implicit “latest prerelease” selector.

The bootstrap is itself attached to the immutable Release and included in
`SHA256SUMS`; it is copied from the exact tagged source by the publish job.
The executable archive is then independently checked using GitHub's asset digest,
immutable Release verification and build provenance. Operators requiring a
separately reviewed bootstrap can download/inspect `install.sh` before execution
rather than piping it directly.

**Install/upgrade here means software version selection only.** It does not
restart launchd/systemd, migrate a live database, rewrite service configuration
or rotate credentials/certificates. Runtime upgrades continue to use the
separate rehearsal/backup/readback procedure below.

## Plan, verify and stage an installation

Use Python 3.9+ and a GitHub CLI supporting `release verify-asset` and
`attestation verify` (the delivery path is exercised with 2.86.0). The Python
entry remains the lower-level interface and is useful for explicit staging or
automation from a trusted checkout. The default operation is a read-only plan:

```bash
python3 scripts/install-release.py --version fork-20260924.1-rc1 \
  --allow-prerelease
```

Add `--install` to download into an owner-controlled local directory. It requires
an immutable, non-draft release; pre-releases require their explicit opt-in.
Before extracting or executing binaries, the installer verifies GitHub's asset
digest, the signed immutable-release asset, build provenance from this
repository's `release.yml`, exact source digest, archive paths/member types,
embedded identities and binary hashes. Verification failure is not bypassed by a
checksum-only fallback.

The default prefix is `~/.local/lib/agent-git-service`. A version is staged at
`releases/<version>/`. Reinstalling an intact identical version is idempotent;
content drift is an error, not permission to overwrite it. Downloads and receipts
remain private under the prefix for diagnosis. Add `--activate` to atomically
select the verified version using the owned `current` symlink:

```bash
python3 scripts/install-release.py --version fork-20260924.1-rc1 \
  --allow-prerelease --install --activate
~/.local/lib/agent-git-service/current/bin/gh-server --version
```

Activation does not stop/start a service, migrate a database, rewrite an existing
launchd/systemd unit or alter user credentials. Those remain an operator's
separately backed-up deployment decision. No unattended auto-updater is enabled.

## Runtime upgrade sequence

First rehearse using a consistent copy of the actual database and separate Git
storage/ports. Disable external writes, background deliveries and real provider
integrations in the isolated process. Do not run two primary processes against
one Git store: their in-process mutation barriers are not shared.

Before switching, fence old source publishers by repository identity, drain the
owning primary, and preserve the old binary, configuration, stable replication
identity, database and relevant Git/storage state. Start the release binary with
the existing approved configuration; do not regenerate repositories, users,
node identities or certificates just because the binary changed.

Verify the exact `/readyz` source, personal and automation access, normal Git
reads, an independent canary push/readback, the existing Edge replica path and
permission denial against warm data. Keep the canary distinct from ordinary user
branches. Check logs and pending effects before opening the write window.

## Rollback is a data decision

Selecting an older binary is not enough after a forward-only schema migration.
Before production writes resume, decide whether the old executable can safely
use the migrated schema. Otherwise restore the **consistent** pre-upgrade
snapshot with the old configuration/binary. Once post-upgrade user writes have
been accepted, restoring an older database alone can lose or split facts; stop,
retain both states and reconcile rather than silently reverting.

Keep source/release, installed version, running process and observed Git/replica
results as separate receipts. Operator-specific receipts belong outside the
public repository. The historical control-plane/presence/typing/attachment/
read-state API retirements still require a consumer compatibility decision; a
package checksum does not resolve them.
