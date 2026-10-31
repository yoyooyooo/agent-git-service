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

`.github/workflows/release-control.yml` retains `workflow_dispatch`, with
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
bundle for seven days in separate target directories. Dispatch never reaches the
publish job. Candidate run [40876544913](https://github.com/yoyooyooo/agent-git-service/actions/runs/40876544913)
already built both platforms on 2026-10-29; that historical dispatch does not prove
the new tag push certificate policy. New real candidate runs require separate
approval. Adding this workflow does not enable deployment.

### Consumer policy and the legacy production-pin gate

`scripts/release_verify.py` reads expected runtime R/T from the reviewed
`fork/release-source.json` at control C. The pin or downloaded asset cannot choose
its own runtime expectation. `scripts/release_contract.py` shares archive/ABI
validation with the builder without importing the installer. The historical
standalone installer retains its self-contained v1 contract. V2 uses the separate
complete-checkout consumer below.
The verifier constructs the following policy from reviewed control provenance,
with no caller-supplied signer override:

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
`release_source_binding_unverified`, exit 65, before invoking `gh`. The distinct
v2 installer path validates tag-push evidence; failure cannot fall through to
the historical v1 attestation policy.
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

### Tag publication wiring (disabled by default)

A new `fork-*` tag selects `release-control.yml`. Tag C must be the current
`fork/main.20260924` tip at admission. `fork/release-source.json` at C contains
`ags.release-source.v1`, the exact version V, runtime R/T and runtime acceptance
PR. C differs from R; R must be an ancestor and its Git tree must equal T.
The example chooses `fork-20260924.5-rc5`, runtime
`07506714431912b32cf33cbe71d36d25dac12203`, tree
`5e61bb20efd72638c1ee07fa948328cd88ed469c`. The gate requires the recorded
runtime PR to have merged into the default branch with matching content and
green CI. It does not require a GitHub human approval on that runtime PR.

`AGS_RELEASE_PUBLICATION_ENABLED` must equal `true`; missing or any other value
fails the read-only gate before builds. The gate also reads the live variable,
including immediately before publication. No variable is set by this change.
It requires the expected public fork ID, parent and default branch, a new tag
creation (no deletion or forced update), the exact tag/checkout/workflow C,
and a merged default-branch PR for each of C and R. Each PR head tree must equal
the final merged/rebased C or R tree. The control PR still requires an independent
human `APPROVED` review at its current head: an associated owner, member or
collaborator who is not the author. `CHANGES_REQUESTED` rejects it. Publication
rereads and freezes that approval evidence. The runtime PR is the only exemption:
publication does not require a GitHub human approval for it and does not read its
reviews. Independent review before merge and exact-head CI carry that runtime
risk. A merged PR, bot review, tag message or missing control-review data does
not authorize publication.

For both C and R, the newest applicable exact-head `ci.yml` and `secret-scan.yml`
run and current attempt must succeed. All 12 Fork CI job names and the secret
job must be present exactly once and successful. Pending, skipped, failed or
incomplete inventories fail closed. The gate runs public audit/disclosure checks
without importing private operator policies. Published and unpublished draft
names must be absent. Later jobs retain the admitted C/R/T/V and run/attempt;
a default-branch advance is allowed only when C remains its ancestor. Tag object,
control approval, merged PR identity, CI, source, switch and repository facts are
reread before publication.

Native builds and signing have no contents-write permission. The publish job
alone receives contents-write, reads each platform artifact into its own directory,
and verifies the real bundle with all flags above. Verified certificate fields
must identify this push, exact tag ref and run/attempt; the signed statement
must bind exactly one archive subject and the reviewed R/T. Both manifests and
all binary identities must agree on V/R/T, toolchain and ABI.

The Release has exactly these nine assets:

- `ags-<V>-darwin_arm64.tar.gz` and `ags-<V>-linux_amd64.tar.gz`
- `build-info-darwin_arm64.json` and `build-info-linux_amd64.json`
- `attestation-darwin_arm64.sigstore.json` and `attestation-linux_amd64.sigstore.json`
- `release-source.json` (byte-for-byte from C), `install.sh`, and `SHA256SUMS`

Sorted `SHA256SUMS` covers the other eight files, excluding itself. Per-platform
predicate/checksum/build receipts remain intermediate files, not extra Release
assets. The publisher uses `gh release create --verify-tag --draft`, uploads
without `--clobber`, then reads back the exact Release ID, names, count, sizes,
digests and body before publishing. RCs remain prereleases; all versions use
`make_latest=false`. Stable Latest promotion needs separate approval.
Published Releases must be immutable and each of the nine assets must pass
`gh release verify-asset`.

Unknown create/upload/publish results cause exact readback, never a second POST.
Failed drafts and their assets remain intact; ordinary reruns refuse occupied
names. Explicit draft recovery is not enabled in this slice. Do not delete,
overwrite or rebuild a failed draft as a retry. A separately reviewed recovery
must bind its exact ID and all original asset digests.

`release.yml` has only a manual retired reject entry (exit 65), with no tag
trigger or build/sign/publish capability. Legacy Python `gate`, `build`,
`publish`, and `recover-draft` remain closed. `scripts/release.ts rc|stable`
still refuse before tag mutation; its workflow checks are advisory, while the
Python v2 gate owns live authorization. Historical commits keep their old
workflows. New source cannot fence tags targeting those commits: ruleset
activation and tag creator policy require separate operator approval.

Publication wiring does not activate rulesets, the publication variable,
candidate/tag runs, or any deployment. The complete-checkout v2 installer below
is a separate consumer; the legacy production-pin gate remains disabled. First tag
acceptance must prove the push certificate policy on both native platforms.
Publication does not deploy. Operators approve and perform each machine's
upgrade manually after installation compatibility is separately accepted.

To roll back wiring, submit a reviewed revert PR while keeping publication and
autodeploy disabled. Do not reset/force-push history, move tags or mutate an
immutable Release. If an approved future run has uncertain results, read its
Release/draft ID before any recovery; cancellation does not undo publication.

## Historical v1 install and upgrade

Previously published v1 Release archives retain their v1 verification path. V2
installation requires the complete reviewed checkout and the separate v2 consumer. The
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
`attestation verify` (the delivery path is exercised with 2.86.0). From a
trusted checkout, the guaranteed automation entry is the shell launcher
`scripts/install-release`. It exports `PYTHONDONTWRITEBYTECODE=1`, unsets
`PYTHONPYCACHEPREFIX` and execs `python3 -B` before the installer runs. The
default operation is a read-only plan:

```bash
scripts/install-release plan --version fork-20260924.1-rc1 \
  --allow-prerelease
```

`scripts/install-release.py` is the internal implementation. Running it as
`python3 scripts/install-release.py` is outside the read-only guarantee:
CPython can write standard-library bytecode before the first line of that file
if the caller points `PYTHONPYCACHEPREFIX` or `-X pycache_prefix` at a writable
directory. The file then executes `from __future__ import annotations` before
setting `sys.dont_write_bytecode`; those settings cannot prevent earlier writes.

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
scripts/install-release install --version fork-20260924.1-rc1 \
  --allow-prerelease
~/.ags/bin/gh-server --version
```

`scripts/install-release verify-only` is the guaranteed verify-only entry. It
forwards `--verify-only` to the same internal implementation.

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

## V2 consumer and local upgrade source

From a complete reviewed checkout, `scripts/install-release --version <tag>`
returns a read-only metadata plan. Its verification map explicitly marks asset
bytes, immutable release proof, attestation, archive and executable identity as
unverified. `--verify-only` downloads into private temporary scratch, verifies
all nine assets and both platform bundles, and never creates the installation
root or executes the candidate. A release is classified as v2 before any
installation effect when its asset names include `release-source.json` or either
platform attestation. Archive names are shared with historical v1, so they are
not the classifier. Named v2 verification and the path used when all three
names are absent share one scratch-parent selector. Both `--install` and
`--verify-only` select that parent before creating a directory or downloading,
including a named v2 release that downloads all nine assets. `TMPDIR`, then
`TEMP`, then `TMP` is used only when that value resolves through its final
symlink to an existing real directory which is neither the canonical
installation root nor its `state` directory, and is not inside either one.
Relative values resolve from the current directory, and identity uses
filesystem identity rather than text case. If every configured value is absent
or unsafe, `/tmp`, `/var/tmp` and `/usr/tmp` are tried in that order under the
same rule. No usable parent rejects with `release_artifact_unverified` and
exit 1 before any scratch or download. The selected parent then receives
private mode `0700` temporary scratch outside the installation root. A
successful verification uses that same parent and removes the scratch
afterwards; neither rejection nor success writes the installation root or its
`state` directory.

The Bash `install.sh` wrapper selects its bootstrap scratch parent under the
same rule before `mktemp`, installer download or installer invocation, for
plan, install and upgrade. That selection uses only the functions embedded in
the wrapper. It does not parse, import or execute the adjacent
`install-release.py`, so scratch selection does not depend on its contents.
Version identification and refusal still execute the selected trusted installer
after that choice. After a parent is selected, a regular adjacent
`install-release.py` is the installer that runs: it is the trusted source of
the release being installed, not an untrusted input isolated by this wrapper.
A copied standalone bootstrap has no adjacent source and fetches the exact
installer blob only after scratch selection. Regression tests compare the
embedded functions, syntax and results, with the Python selector. No suitable
parent exits 1 without creating scratch. Bootstrap scratch remains mode `0700`
and is removed on exit.

The guaranteed bytecode entries are `scripts/install.sh`, `scripts/install-release`
(`plan`, `install`, `verify-only`) and `scripts/upgrade`. Each sets
`PYTHONDONTWRITEBYTECODE=1`, unsets `PYTHONPYCACHEPREFIX` and starts `python3 -B`
before the internal implementation runs. The `.py` files are internal. Their
`from __future__ import annotations`, where present, executes before
`sys.dont_write_bytecode` runs, so those in-process settings do not cover
interpreter startup. Direct `python3 script.py` is outside this guarantee.
`-X pycache_prefix` is an interpreter option, not a launcher argument; these
launchers accept application arguments only. The internal helper
`scripts/ags-python.sh` also protects Python subprocess entry points used by
tests and CI. Its `-B` remains effective even with an explicit `-X` prefix.
Classification has three outcomes:

- V2: a readable manifest declares `ags.release.v2` or contains `abi`, or the
  readable archive contains `NOTICE` and does not pass the complete historical
  v1 checks. `--install` exits 83
  (`install_contracts_unimplemented`). `--verify-only` rejects the missing v2
  inventory with `release_artifact_unverified` and exit 1.
- Valid historical v1: the manifest declares `ags.release.v1` and has no `abi`.
  Historical packagers also copied `NOTICE` when present, so this member alone
  cannot exclude a valid v1 archive. The complete archive must pass the
  existing historical checks for members, limits, repository, source, platform,
  binary inventory, identities and SHA256 digests before the v1 path may create
  an installation root. Historical attestation verification still follows.
- Unknown or damaged: a missing, invalid or oversized manifest, non-tar content,
  or a manifest with no v2 clue that fails the historical checks rejects with
  `release_artifact_unverified` and exit 1. A manifest that cannot be read does
  not establish that the archive is v1, even if no v2 marker survives.

Classification reads archive members without extracting or executing them. The
private scratch is removed afterwards. Both refusal paths, and refusal of an
unsafe scratch parent, precede root creation, any download or temporary file
inside the installation root or its state directory, candidate execution,
installer calls, and lock, claim or receipt writes. A named v2 install stops at the same exit 83
gate before downloading. V2 `--install` remains closed. After v2 verification,
the consumer also refuses a host whose ABI is below the manifest minimum.
A host entry or a command that exits zero does not satisfy the missing backup,
Git/DB consistency, mini handoff or observation/finalize contracts. Valid
historical v1 archives retain their existing verification and replacement path.

V2 tags resolve to C. The consumer reads `fork/release-source.json` at C, verifies
its blob digest and R/T ancestry/tree, and requires the Release source asset to
match those exact bytes. It checks all asset IDs/sizes/SHA256 digests, sorted
SHA256SUMS, immutable Release proofs and real gh runtime-binding verification.
Only successful gh verificationResult fields count as signed evidence. Source
and signer digest are C, signer workflow is fixed, hosted runners are required,
and certificate ref/event/run/attempt must describe one tag push. Manifests and
binary digests bind R/T/platform. V2 failure never falls back to v1.

Receipts retain `source=R` and add `control_revision`, `runtime_tree`, Release ID,
tag, run ID/attempt and per-platform attestation evidence. A dispatch candidate
is useful for real cryptographic tests but cannot pass the production tag policy.
The standalone historical Bash bootstrap is unchanged; it does not distribute
the new Python dependency set. Use a complete reviewed checkout for v2.

The [local upgrade entry](local-upgrade.md) describes per-machine manual approval,
mini-before-imile order, Dagu approval-before-apply sequencing and current gaps.
No Dagu flow, host provisioning, installation, tag or workflow run is performed
by this source change. Its empty host/migration policy blocks live apply.

## Manual upgrade acceptance is not publication

S4 owns separate manual acceptance; S5 connects the S1–S4 transaction only with
a complete privately provisioned host package and current human window approval.
Default empty policy still refuses apply/backup/transaction with exit 83. Direct
v2 install is always closed with exit 83 and cannot borrow upgrade admission. It does not publish a Release, install a
bundle, provision policy, remove a startup fence, open traffic or start another
machine. The checked-in deployments and migration admissions remain empty.

Starting AGS can already restore business writes, migrations and provider effects.
`confirm` accepts the completed upgrade after human observation; it is not a
post-start traffic switch. The executor emits only pending requests. A human
records approval privately, then the consumer validates evidence and fresh health.
Same approval retries return one durable acceptance record, not another upgrade.

The manual mini-to-imile package requires the same repository/Release/runtime and
both platform asset/provenance identities, with distinct machine-bound pins allowed.
Hashes and actor text do not authenticate the carrier. imile still needs its own
local approval and recovery group. No transport, workflow dispatch, tag, Release
or deployment is part of this package. See [manual acceptance](local-upgrade.md#s4-manual-acceptance)
and [acceptance contracts](backup-recovery.md#s4-acceptance-file-contract).
