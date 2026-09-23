# Fork generation main.20260924

**Status: source verified and ready for the maintainer's public-visibility decision.**

This is a source/disclosure checkpoint. It does not toggle repository visibility, deploy a primary or Edge, migrate a live database, or certify every historical client and platform. The documentation tip may follow the exact tested source below without changing application code.

## Exact identity

| Item | Accepted value |
| --- | --- |
| Formal branch | `fork/main.20260924` |
| Source repository | `yoyooyooo/agent-git-service-fork` on GitHub |
| Repository identity | `1383799420` |
| Frozen official upstream | `c7691f45cf2c02d6a32e4bf13a21eca40649aa24` |
| Official tag at selection | None; the fetched tag inventory was empty |
| Fully tested source | `2a2e19c4ae9b355db567f00d05ce878afc4c1522` |
| Fully tested source tree | `2d6e391bae8a165202f2f19a2926b6930ce390cf` |
| Fork lineage at that source | Eight commits, zero merge commits |

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

These groups form one atomically verified generation; an intermediate group is not advertised as an independently deployable release. Subsequent documentation-only acceptance records do not change the tested application source.

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
| Complete GitHub-hosted Fork CI | Run **35887767869**, source `2a2e19c`, **10 jobs successful, zero non-success jobs** |
| Root module behavior | Seven shards cover **all 60 packages exactly once** |
| Concurrency and failure boundaries | Selected Edge/snapshot/replication and authority/effect race suites passed |
| Client compatibility | Full vendored CLI compilation, core Git/auth/PR behavior and complete local API-client suite passed |
| Source checks | Build, vet, formatting, documentation and generation/audit regressions passed |
| Independent history secret scan | Run **35887767751** passed for the exact source; local Gitleaks scan also passed |
| Dependency licenses | Local check passed for **97 Go modules / six allowed license families** |
| Primary build attribution | Exact accepted Git archive built with the full source revision and tree receipt |
| Edge portability build | Linux amd64, CGO-disabled Edge command cross-build succeeded; not a deployment artifact acceptance |

Earlier failed trial runs remain private failure evidence. The failures caused by inconsistent anonymized request paths, YAML/JSON values, actor case variants and a fixed fixture checksum were reproduced and repaired without changing production policies or deleting the original assertions.

## Disclosure and repository boundary

The candidate was published only into a **fresh private GitHub repository** with a new repository identity. Earlier trial metadata, logs and commits remain in a distinct private review archive; the former source publisher targets a different private repository and has no configured mapping to this clean destination. No old repository was made public or deleted.

The accepted destination had one formal branch; the specifically probed old private-source and trial commits were unavailable through its commit API. It had no PRs, Issues, releases, artifacts, webhooks, configured Actions secrets or self-hosted runners. Wiki was disabled. New Actions history contains only this generation's verification. Workflows use read-only permissions and cannot approve reviews; checkout is pinned to the reviewed Node-24-compatible action revision.

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
