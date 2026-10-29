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
| `linux_amd64` | x86-64 Linux | Primary built natively on Ubuntu 22.04 with a glibc 2.35 runtime baseline; Edge and replication client are CGO-disabled. |

The primary requires native Git at runtime. Optional integrations may require
other explicit tools, such as `zstd` for compressed provider logs. Installing the
bundle does not install those dependencies, certificates or external providers.

## V2 contract and publication hold

New candidates use `ags.release.v2`, structured `ags.abi.v2`, and the
`ags.deploy-release-pin.v2` pin contract. V1 describes previously published
assets only. V1 is not an alias for v2, and existing Release pages, tags, assets,
and signatures are not changed. Ubuntu 24.04 and glibc 2.39 are retired for new
primary binaries, even if their symbol requirements happen to be below 2.35.
No `patchelf` conversion qualifies as an Ubuntu 22.04 build.

The Linux ABI object has exactly these fields:

```json
{
  "schema": "ags.abi.v2",
  "os": "linux",
  "arch": "amd64",
  "build_distribution": "ubuntu",
  "build_distribution_version": "22.04",
  "libc": "glibc",
  "minimum_libc": "2.35"
}
```

The builder checks the native distribution before compilation, then checks all
GLIBC version needs, the ELF interpreter, and `ldd` resolution. The primary uses
`CGO_ENABLED=1`; Edge and replication use `CGO_ENABLED=0`. Go is exactly 1.26.8,
with `GOTOOLCHAIN=local`, `GOENV=off`, and `GOWORK=off`.
Darwin candidates use native macOS 15, ARM64, deployment target 15.0, and system
dynamic libraries only. This does not establish notarization or a previously
reported macOS candidate's full SHA256.

### Manual control workflow

`.github/workflows/release-control.yml` accepts only `workflow_dispatch`, with
required `runtime_revision` and `runtime_tree` inputs. Both are full lowercase
40-hex Git object identities. The workflow runs the control commit's builder,
fetches and verifies the exact runtime commit and tree, and builds a Git archive
outside the checkout. It does not execute historical build scripts from that
archive. The candidate version is `source-<full-runtime-revision>`, not a release
tag. Inputs cannot allocate a version, tag, or Release ID.

The native runners are `ubuntu-22.04` and `macos-15`. The action signs under the
workflow invocation's **control SHA**, not the runtime SHA. Its custom signed
predicate type is
`https://github.com/yoyooyooo/agent-git-service/attestations/runtime-binding/v1`.
The predicate records `schema=ags.runtime-binding.v1`, `runtime_revision`, and
`runtime_tree`. The in-toto subject binds the archive digest. Unsigned build
metadata alone is not evidence of that binding.

Only the real attestation action can produce the retained Sigstore bundle.
Workflow artifacts retain the candidate archive, manifest, predicate, and real
bundle for seven days. There is no Release publish job and no content-write
permission. Adding this workflow does not authorize dispatch or enable deployment.

### Consumer policy remains disabled

`scripts/release_verify.py` implements evidence checks for the approved runtime
revision `0b2c036604911858c9dae05d81056af44d6eb7d8` and tree
`6a8548d9419387568d27a8cfcb43531e36c26c78`. It constructs the following policy
from the pin's provenance, with no caller-supplied signer override:

```text
gh attestation verify <archive> --repo yoyooyooo/agent-git-service \
  --source-digest <full-control-SHA> --signer-digest <full-control-SHA> \
  --signer-workflow yoyooyooo/agent-git-service/.github/workflows/release-control.yml \
  --deny-self-hosted-runners \
  --predicate-type https://github.com/yoyooyooo/agent-git-service/attestations/runtime-binding/v1 \
  --format json [--bundle <real-bundle>]
```

After successful cryptographic verification, the checker reads only
`verificationResult.statement` from that command's output. It compares the signed
runtime revision and tree with the approved pair, and the subject SHA256 with the
archive's actual SHA256. `--predicate-type` selects the custom signed predicate;
it does not replace any source, signer, or runner check.

`separated-control-v1` remains **disabled**. The production entry returns
`release_source_binding_unverified`, exit 65, before invoking `gh`. V2 installs
also fail closed and cannot fall through to the historical v1 attestation policy.
The binding-policy helper checks the pin's ABI and provenance fragment; it is not
a complete pin loader, installation authorization, or host transaction engine.
No deployable pin is generated by these scripts.

`fork/scripts/test_release_binding.py` uses explicitly synthetic policy fragments
and mocked verifier output. It exercises altered revision, tree, subject digest,
old or omitted source digest, the legacy `release.yml` signer, and failed runner
verification. These are policy tests, not real bundle or certificate acceptance.
Enabling the mode still requires a separately approved real workflow run for each
target, retained certificates and predicates, and all real positive and negative
checks. Removing `--source-digest` is not an equivalent verification policy.

### Legacy tag path is closed on this control line

`.github/workflows/release.yml` retains its tag trigger only to reject publication
with exit 65. It has no build, signing, or publication job. The Python `gate`,
legacy `build`, `publish`, and `recover-draft` entries also refuse before network
or publication effects. The old version-selection commands in `scripts/release.ts`
are not an enabled v2 publication path. Do not use them to create a tag.

A new control commit cannot change a workflow stored in a historical commit.
Tagging the old runtime SHA would still select its old Ubuntu 24.04 workflow.
The repository changes do not claim to fence historical tag creation at GitHub.
No old tag, Release, or asset is moved or overwritten.

Whether a future tag points to control or runtime remains undecided. Tag identity,
control ref, and runtime revision must eventually be pinned separately. Until that
decision and real bundle evidence are approved, neither tag publication nor the
separated-control production mode is enabled. Existing deployment handoff and
operator automation are outside this change and are not disabled by it.

## Historical v1 install and upgrade

Previously published v1 Release archives remain installation artifacts. The v2
workflow currently produces candidates only, not installable Releases. The
Bash bootstrap selects exact binary bytes and delegates bundle/digest/attestation
checks to the Python installer. A trusted adjacent installer is used when present;
a standalone bootstrap fetches the exact installer blob from immutable Latest
stable, independently of the selected binary version. An older target must not
revive an older multi-version installation policy. See [single-root runtime](user-runtime.md).

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
unexpected tag identities before downloading anything. It links `gh-server`,
`ags-edge` and `ags-replication` under `~/.local/bin` to the current programs in
`~/.ags/bin`. It refuses an unrelated file/symlink rather than replacing it.
`plan` does not install binaries. There is no retained staging/activation surface
and no implicit latest-prerelease selector. Legacy installation roots require an
explicit migration, not an implicit recursive cleanup.

The bootstrap is itself attached to the immutable Release and included in
`SHA256SUMS`; it is copied from the exact tagged source by the publish job.
The executable archive is then independently checked using GitHub's asset digest,
immutable Release verification and build provenance. Operators requiring a
separately reviewed bootstrap can download/inspect `install.sh` before execution
rather than piping it directly.

**Install/upgrade here replaces the single installed program set only.** It does
not restart launchd/systemd, migrate live data, rewrite service configuration or
rotate credentials/certificates. Runtime changes use scoped acceptance below;
ordinary program replacement does not retain a complete historical environment.

## Plan, verify and replace the single installation

Use Python 3.9+ and a GitHub CLI supporting `release verify-asset` and
`attestation verify` (the delivery path is exercised with 2.86.0). The Python
entry remains the lower-level interface for automation from a trusted checkout. The default operation is a read-only plan:

```bash
python3 scripts/install-release.py --version fork-20260924.1-rc1 \
  --allow-prerelease
```

Add `--install` to download into an owner-controlled local directory. It requires
an immutable, non-draft release; pre-releases require their explicit opt-in.
Before extracting or executing historical v1 binaries, the installer verifies GitHub's asset
digest, the signed immutable-release asset, build provenance from this
repository's `release.yml`, exact source digest, archive paths/member types,
embedded identities and binary hashes. Verification failure is not bypassed by a
checksum-only fallback.

The default root is `~/.ags`. Verified commands replace the current files in
`bin/`; there are no `releases/<version>` directories or `current` selectors.
An intact repeated installation is safe; unowned or drifted files are rejected.
Transaction downloads/candidates are cleaned on success or exceptions. A small
current receipt and interruption marker live in `state/`, not inside downloads.

```bash
python3 scripts/install-release.py --version fork-20260924.1-rc1 \
  --allow-prerelease --install
~/.ags/bin/gh-server --version
```

An interrupted replacement prevents startup until the same exact verified
installation completes. No unattended auto-updater is enabled. Installed and
running identities are separate facts; neither a receipt nor a successful
installation claims that an existing process was restarted.

## Runtime upgrade sequence

Scope acceptance to the change. A canary normally uses explicit settings,
a dedicated test repository and bounded diagnostics, not a permanent clone of
the deployment. Schema-changing work may require a consistent isolated data
rehearsal/backup with an explicit storage budget and disposal condition. Disable
external effects in such a fixture. Never run two primaries against one Git store.

Coordinate with the owning service, preserve approved persistent configuration,
repository/storage identities and live data, and replace only the intended
programs. Do not regenerate users, node identities or certificates merely because
a binary changed. Keep startup inputs under the durable root, never a dated
deployment or build directory. A database backup is a separate recovery decision,
not an automatic copy of every installation.

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
