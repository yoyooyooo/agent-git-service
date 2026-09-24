# Fork generation main.20260924

**Status: native public GitHub fork active; exact source verified; first native-fork Release published.**

This is the source/disclosure and delivery checkpoint for the maintained fork. Runtime deployment, database migration and historical-client compatibility remain separate operator decisions. Documentation-only tips may follow the exact tested source below without changing application code.

## Exact identity

| Item | Accepted value |
| --- | --- |
| Formal branch | `fork/main.20260924` |
| Source repository | `yoyooyooo/agent-git-service`, native GitHub fork of `ngaut/agent-git-service` |
| Repository identity | `1384519373` |
| Frozen official upstream | `c7691f45cf2c02d6a32e4bf13a21eca40649aa24` |
| Official tag at selection | None; the fetched tag inventory was empty |
| Fully tested source | `d687bcfc6d05ebf399859c489149b04ae1a57e34` |
| Fully tested source tree | `ee25a1b355b6eaf83e4ce6fbfbc4485439dc8530` |
| Fork lineage at that source | Sixteen downstream commits, zero merge commits |
| First native-fork Release | `fork-20260924.1-rc5`, immutable pre-release from the fully tested source |

The generation name is a dated main snapshot, not an invented upstream release. Official upstream was re-read before final acceptance. The old private source and trial generation are not ancestors of this generation and were not pushed to this destination.

## Capability-scoped history

| Commit | Scope |
| --- | --- |
| `93bf493` | Storage, authority and verified native Git primitives |
| `ee1dd26` | Optional provider projections and credential-safe delivery |
| `a87a4c0` | Primary service integration, scoped actor/grant authority and exact effect recovery |
| `70c12f5` | Independent Edge reads, incremental replication and operational observation |
| `72a7e04` | Client API and credential compatibility |
| `af8595f` | Hosted build/test delivery and portable operating tools |
| `201e3c0` | Public product, capability and generation documentation |
| `2a2e19c` | Exact hashes for reviewed synthetic rejection fixtures |

These first eight commits reconstruct and validate the retained downstream capability stack. Later commits on the same linear generation add public Release delivery, exact toolchain pinning, runtime build identity, the one-command installer bootstrap, and native GitHub-fork repository identity. They do not delete or replay the original capability groups, and only an exact fully tested tip is eligible for a Release.

## Preservation evidence

The independent preservation tool compared this generation with the reviewed construction input and reported:

- **985 of 985 production Go files byte-for-byte identical**, including vendored client production code.
- **792 original Go test files and 4,125 original test/benchmark/fuzz entries preserved**; 108 files have changed narrative literals, with their declarations, imports and non-string token structure unchanged.
- All **174 production Go files originally added by the pre-upstream downstream** remain present.
- Outside Go, the reconstruction differences are governance/scanner code, workflow configuration, documentation and two synthetic JSON fixtures. Existing runtime shell/client-route scripts were not dropped or rewritten by reconstruction.
- Root and vendored license/notice files are unchanged.

The [capability matrix](../FEATURE_PARITY.md) binds retained behavior to its tests. Counts are source-preservation evidence, not proof that there are no latent bugs. The complete exact-source regression remains necessary to validate changed literal relationships.

A separate old-schema upgrade harness exported exact historical and candidate source archives, created only synthetic SQLite data, and ran the new migration. User/token/repository/Issue/PR fingerprints and the stable repository storage identity were preserved; SQLite integrity returned `ok`. No deployed database was copied or changed.

## Exact-source verification

| Gate | Evidence / result |
| --- | --- |
| Complete GitHub-hosted Fork CI | Run **35949890626**, source `d687bcf`, **10 jobs successful, zero non-success jobs** |
| Root module behavior | Seven shards cover **all 61 packages exactly once** |
| Concurrency and failure boundaries | Selected Edge/snapshot/replication and authority/effect race suites passed |
| Client compatibility | Full vendored CLI compilation, core Git/auth/PR behavior and complete local API-client suite passed |
| Source checks | Build, vet, formatting, documentation and generation/audit regressions passed |
| Independent history secret scan | Run **35949890621** passed for the exact native-fork source; local Gitleaks scan also passed |
| Dependency licenses | Run **35949890687** passed; the release source uses the repository's allowed dependency-license policy |
| Native Release | Run **35951047996** built macOS ARM64 and Linux amd64 bundles, verified provenance, and published immutable `fork-20260924.1-rc5` |
| Installer entry | The tagged Bash bootstrap delegates archive/digest/attestation verification to the exact tagged Python installer; fresh install and idempotent upgrade were accepted in an isolated prefix |

Earlier failed trial runs remain private failure evidence. The failures caused by inconsistent anonymized request paths, YAML/JSON values, actor case variants and a fixed fixture checksum were reproduced and repaired without changing production policies or deleting the original assertions.

## Disclosure and repository boundary

The maintained destination is a **native public GitHub fork** of `ngaut/agent-git-service` with its own repository identity. The accepted downstream generation is carried on `fork/main.20260924`; the inherited `main` remains the upstream branch lineage. Earlier trial metadata, logs and commits remain in distinct private archives, and the former AGS source publisher has no configured mapping to this maintained fork.

Before native-fork cutover, the clean standalone destination carried only the reviewed generation and no private-source/trial commits. The native fork adds the public upstream fork network and its inherited `main`, not the old private object inventory. Repository-specific Actions, releases and other writable surfaces are re-established only after the native fork's exact repository identity passes the same source/disclosure gates. Workflows use read-only permissions and cannot approve reviews; checkout is pinned to reviewed action revisions.

The complete new reachable-history scan, including commit metadata and case-insensitive operator-specific rules, has **zero unreviewed findings**. Fifteen synthetic URL/private-key-shape test examples are explicitly reviewed by exact path/blob/rule; changes invalidate their approvals. They are negative-test fixtures, not real credentials. The public maintainer identity uses GitHub noreply attribution.

This does not promise mathematical absence of all sensitive information. Public visibility is a separate explicit maintainer action. Never mirror the private local object/ref inventory into this repository, and never mistake removing a visible branch for deleting all external cached or cloned history.

## Compatibility limits retained

The upstream transition retired the former multi-tenant control-plane route and the old presence, typing, issue-attachment and read-state APIs. They are not advertised or counted as downstream parity. Consumers relying on those retired surfaces require a separate compatibility decision before runtime upgrade.

This checkpoint does not claim complete PostgreSQL functionality, all-platform behavior, every vendored CLI test, live third-party-provider acceptance or a production rollout. Edge retains its documented limits around periodic rather than event-driven prewarming, certificate renewal, transfer resume, durable audit and connection-only fallback. See the [Edge architecture](../../docs/architecture/ags-edge.md).

## Reproduce and maintain

```bash
python3 fork/scripts/audit.py verify
python3 fork/scripts/audit.py inventory --details
python3 fork/scripts/audit.py publication
PYTHONDONTWRITEBYTECODE=1 python3 fork/scripts/test_generation.py
PYTHONDONTWRITEBYTECODE=1 python3 fork/scripts/test_casefold_policy.py
(cd fork/tools && go test ./... -count=1)
```

The operator also supplies its owner-only private literal policy and uses `fork/tools/coverage.go` against the retained construction input. Neither private input refs nor the policy is part of the public generation. New code changes require new exact-source verification; a documentation-only acceptance tip records the unchanged runtime source explicitly.
