# Upstream modification inventory

The frozen official source is recorded in `UPSTREAM_BASELINE`. This inventory is a review map, not a claim that the fork is implemented entirely through plugins or that all changed behavior is safe.

## Initial retained-code census

The initial private preparation input contained 290 commits after the official baseline: 104 modified upstream paths and 385 additive paths, for 489 total changed paths. The complete private ledger records every commit, including six metadata-only commits. Categories can overlap; adding capability commit counts does not yield the total.

| Modified upstream path family | Initial count | Main review pressure |
| --- | ---: | --- |
| Primary platform and storage | 66 | Original-user authorization, schema migration, Git lifecycle, write barriers and process ownership |
| Client compatibility | 16 | API path/wire consistency, credential routing and provider facts |
| Documentation and governance | 12 | Public product contract versus historical private operating narrative |
| Build and operations | 9 | Removing private runners and self-hosting assumptions without silently dropping coverage |
| Provider and delivery | 1 | Transport presentation integration; many additive provider modules also require review |

The preparation adds further fixes and governance files, so these numbers are historical input measurements, not the final generation's live inventory. Regenerate the exact path list before acceptance:

```bash
python3 fork/scripts/audit.py inventory --details
```

`paths` provides each additive/modified/deleted path, and `commits` provides its history relationship. The unclassified input probe is private test evidence, not a public product feature. Every semantic status begins as `pending` and must be reconciled against [capability decisions](CAPABILITIES.md).

## Important integration seams

| Seam | Owned invariant | Change policy |
| --- | --- | --- |
| `config` / `server` | Explicit primary, replica and optional-provider configuration; single owning lifecycle | Keep product configuration generic and off by default; never transplant a private host setup |
| `internal/db` | Stable repository identity and data-preserving migration | Verify an actual prior schema and supported dialects, not only a newly created database |
| `internal/service` authorization | Canonical actor versus executor; live native permission and grant facts | Preserve use-time denial and exact-effect recovery across upstream service refactors |
| Git write entry points | Serialize snapshot capture with every supported primary writer | Recheck new upstream Git and Wiki write paths; do not assume one post-receive hook covers all writes |
| Router / REST / GraphQL | Shared policy and truthful wire presentation | Keep canonical extension paths and explicit compatibility aliases consistent |
| Native Git backend | Credential stripping, request limits, cancellation and request-scoped write policy | No implicit shell configuration or context-ignoring blocking I/O |
| Edge / snapshot store | Exact objects, pinned views, bounded resources and current authorization | No stale-read success, second write authority or automatic ambiguous effect replay |
| Provider projection | One authoritative mapping and one external effect decision | Keep side effects outside replayable database transactions; verify both current reads and lock order |
| Client compatibility modules | Stable primary URL and caller identity | Test fresh/noninteractive clones and exact route rollback, not only existing worktrees |

## Next-generation review

Fetch the official ref and tag inventory before selecting a target. Compare each generation to its own frozen baseline; compare overlap with later upstream changes using read-only Git operations. An empty merge-conflict list is only textual evidence.

Build a new generation from upstream and reconstruct the capabilities that remain necessary. Do not merge old generations into it, and do not add wrappers merely to make an intrusion count look smaller. Keep legacy private evidence, author metadata and runtime receipts outside the new public ancestry. Verify the final exact tree and update the release manifest before promoting a generation or changing repository visibility.
