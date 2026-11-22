# CI Documentation

This document describes the GitHub Actions workflows carried by the public
`agent-git-service` repository.

## Overview

[Fork CI](../.github/workflows/ci.yml) is the source gate for the maintained
GitHub fork. Use [the test strategy](test-strategy.md) for local regression
commands. Hosted source checks do not prove platform-native recovery or
approve publication or deployment. See the [Route B host package](operations/routeb-host-package.md)
for native acceptance requirements.

## Workflow Files

- `ci.yml` runs Fork CI.
- `cli-pr-check.yml` runs the full vendored CLI suite on manual dispatch. Fork
  CI runs the core client contracts automatically.
- `doc-lint.yml` is the manual documentation gate. Fork CI runs the same lint.
- `secret-scan.yml` scans the fork's reachable commit range for secrets.
- `license-check.yml` checks dependency licenses and third-party notices.

## Release candidate workflows

`release-control.yml` retains non-publishing dispatch candidates and adds
`fork-*` tag publication wiring, disabled unless `AGS_RELEASE_PUBLICATION_ENABLED`
is exactly `true`. Its read-only gate refuses closed tag mode before builds.
Native builders remain Ubuntu 22.04/macOS 15 and Go 1.26.8. Only the tag publish
job can write Release contents, after rechecking reviewed C/R/T/V, complete
exact-head CI and real signed evidence. Both control and runtime PRs must be
merged. Only the runtime PR no longer requires a GitHub human approval;
the control PR still requires one. Independent review before merge and CI carry
the runtime-PR risk. `release.yml` is a manual
retired reject entry. Neither workflow deploys. Historical tags still select historical workflows;
platform fencing requires separate approval.

Fork CI runs `fork/scripts/test_release.py`, `fork/scripts/test_release_binding.py`
and `bun test scripts/release.test.ts` for event, identity, merged-PR and CI
refusals, admission when the runtime PR has no human approval but the control PR does, ABI/archive checks, nine-asset publication, and uncertain-write readback.
Synthetic fixtures do not replace real cryptographic platform acceptance.
The legacy production-pin verifier stays disabled. The complete-checkout v2
consumer and S1–S6 source contracts run through `bash scripts/upgrade-regression.sh`,
shared by Fork CI and `scripts/regression_gate.list`. Each test gets independent
HOME/TMPDIR/cwd, removes inherited Python injection variables and uses
`scripts/ags-python.sh`. Public host/migration policy stays empty and default
apply/backup/transaction and direct install retain exit 83. Synthetic complete
apply tests do not mock the total gate or backup/manual validators; they do not
prove native host manager, VM or production-copy startup behavior. Real evidence
acceptance runs separately with `scripts/test_release_crypto.py` and local archives.
A real two-platform candidate already ran on 2026-10-29; no new candidate or tag
run is authorized by wiring. See [release operations](operations/releases.md).

## Main CI Workflow

Fork CI runs for PRs targeting `main` or `fork/**`, pushes to `main`, `fork/**`,
or `prepare/public-fork-*`, and manual dispatch. PR jobs check out the exact PR
head rather than GitHub's synthetic merge commit. See the workflow YAML for
package inventory, job conditions, and commands:

| Job | What it checks |
| --- | --- |
| Source and governance | Fork provenance, package inventory, focused regressions, documentation lint, build, vet, and formatting. |
| Root tests | Seven shards cover the root module. Each job owns a disposable loopback TiDB. Each root package is assigned to one shard. |
| Edge and authority race checks | Race-enabled Edge and service authority tests with a disposable TiDB. |
| Vendored client compatibility | Compile the vendored CLI, test core CLI contracts, and run the local `go-gh` module tests. |
| Official gh and CI backend acceptance | Run official `gh` against an isolated primary and CI fixtures. |
| Real Forgejo protected-branch projection | Exercise projection against a real Forgejo fixture with generated credentials. |

Preparation pushes omit the root matrix, race checks, and client acceptance
jobs. A skipped job does not count as a pass. Compare evidence with the exact
head and required job inventory. The legacy Regression Gate, Integration Tests,
Compatibility Tests, E2E Tests, and Backend Smoke commands remain in the
[test strategy](test-strategy.md); they are not separate jobs in Fork CI.

## Secret Scan Workflow

[`secret-scan.yml`](../.github/workflows/secret-scan.yml) scans the range from the
frozen upstream baseline to the checked-out head, including removed files. It
uses checksum-pinned gitleaks and checks the policy's narrow exceptions. Fork
generation refs and PRs targeting them also run the public disclosure preflight.
A clean scan is not authorization to publish private evidence.

## Dependency License Workflow

[`license-check.yml`](../.github/workflows/license-check.yml) runs on PRs to
`main` or `fork/**`, on matching pushes that change dependency or license inputs,
and on manual dispatch. The scanner checks the root Go module build list and
third-party notices; see [dependency licensing](governance/dependency-licensing.md)
for the reviewed allowlist and `NOTICE` policy.

## Documentation Workflow

Fork CI and the manual `doc-lint.yml` run `python3 scripts/doc_lint.py` to verify that:

- required CI/testing docs and workflow files exist
- relative links inside the core CI/test docs resolve
- `docs/ci.md` mentions the active core workflows
- `.github/pull_request_template.md` points contributors at current CI/test docs
- documented verification helper scripts still exist
- every top-level `internal/*` package has a contract entry in
  `docs/module-contracts.md` via `scripts/check-module-contracts.sh`

## Workflow Execution Sandbox

The repository supports sandboxed workflow execution for GitHub Actions-style
workflow runs. This capability is fail-closed by default and must be explicitly
enabled via the `ENABLE_WORKFLOW_EXEC` environment variable.

| Variable | Default | Description |
|----------|---------|-------------|
| `ENABLE_WORKFLOW_EXEC` | `false` | Enable workflow execution sandbox (set to `true` or `1`) |
| `WORKFLOW_EXEC_IMAGE` | `bash:5.2` | Docker image used for workflow step execution |
| `WORKFLOW_EXEC_TIMEOUT` | `2m` | Maximum duration for workflow execution |
| `WORKFLOW_EXEC_CPUS` | `1.0` | CPU limit for workflow containers |
| `WORKFLOW_EXEC_MEMORY` | `256m` | Memory limit for workflow containers |
| `WORKFLOW_EXEC_PIDS_LIMIT` | `128` | Maximum number of processes in workflow containers |
| `WORKFLOW_EXEC_NOFILE` | `1024` | Maximum number of open file descriptors |
| `WORKFLOW_EXEC_TMPFS_SIZE` | `64m` | Size of tmpfs mount for `/tmp` |

Workflow steps run in isolated Docker containers with no network access, a
read-only root filesystem, dropped Linux capabilities, no-new-privileges, and
resource limits. Tests cover both the disabled state and enabled execution path.

## Known Gaps

The following capabilities are intentionally outside the default public CI set:

- scheduled release automation
- image publication and deployment
- coverage reporting
- benchmark-history publication
- supply-chain extras such as SBOM generation, signing, or provenance
  attestations

As additional suites become reliable, promote the smallest stable regressions
into `scripts/regression_gate.list` first, then consider widening the required
smoke or compatibility lanes.


## S6 source verification

`bash scripts/upgrade-regression.sh` is the shared sequential Python gate for
all `test_upgrade_*` entries, including the S6b.01 boundary reproduction, install-v2, single-root, release-consumer and fork
release/binding/installer-shell tests. New full-restore, handoff, pin and integration suites are
included through `scripts/regression_gate.list` and the existing Fork CI step.
The Source and governance job has a 30-minute bound for the expanded sequential
source suites and owning Go checks. Each entry has isolated HOME/TMPDIR/cwd
under the runner's home. The runner
unsets PYTHONDONTWRITEBYTECODE, PYTHONPYCACHEPREFIX, PYTHONPATH and BASH_ENV before
`scripts/ags-python.sh` establishes bytecode-free execution.

S6 tests cover source contracts and real synthetic S1/S2/S4 integration. S6b-fix3 B2 covers the real local-v2 root, transaction conflicts, the startup fence, launcher and supervise durable spawn/exit, and old-start readback. Those tests do not prove a live host recovery or a two-upgrade chain. S6b portable inventory checks, pin evidence checks, and the injected native sequence run through the same sequential runner.

Native adapter isolation lives in [`.github/workflows/native-adapter.yml`](../.github/workflows/native-adapter.yml), not in Fork CI. It is not release-required evidence: `fork/scripts/release.py` and `scripts/upgrade_host.py` still require the reviewed 12 Fork CI jobs to succeed. A skipped job outside that inventory does not count as success and does not fail the inventory. The separate workflow stays off unless the run is `workflow_dispatch` with `native_probe=true`. It uses a one-shot label and the injected sequence only. It does not use a private runner or a secret, and it does not call `launchctl` or `systemctl`. Real launchd and systemd chains, privileged metadata helper, native Trash, isolated VM, production-copy startup, imile behavior and tag-push crypto remain UNVERIFIED. A skip is not a pass. No candidate or publication workflow is dispatched by this gate. See [source limits](operations/local-upgrade.md#s613-source-integration-boundary).
