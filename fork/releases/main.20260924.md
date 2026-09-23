# Fork generation main.20260924

Status: candidate. This revision incorporates the reconstructed fixture relationship repairs; the final exact-source hosted run and clean GitHub destination inventory are required before marking it public-ready.

## Upstream identity

Frozen official source: `c7691f45cf2c02d6a32e4bf13a21eca40649aa24`.
The official tag inventory was empty when this generation was selected. The name uses a dated main snapshot rather than inventing a release tag.

## Capability scope

The generation retains the implemented downstream storage/dialect support, actor/executor separation, Access Grants, exact provider-effect recovery, optional Forgejo/GitLab/GitHub projections, task-context and terminal-delivery integration, notifications, provider-log bridge, client compatibility, and the independent AGS Edge.

The prior reviewed runtime is construction input, not a Git parent. Production Go blobs are retained exactly; narrative substitutions are restricted to tests, fixtures and documentation. Existing Go tests keep their declarations, imports and non-string token structure. Full hosted tests must still verify the changed literal relationships. Private deployment evidence is externalized, not treated as a product feature or as public test data.

All 174 production Go files originally added by the pre-upstream downstream are present. The complete reviewed construction input retains 985 production Go blobs and 4,125 existing test entries; the [preservation matrix](../FEATURE_PARITY.md) states the independent behavioral gates. The original downstream's added production files are preserved. The upstream transition intentionally retired multi-tenant control-plane routing and the old presence/typing/issue-attachment/read-state APIs. That retirement is not counted as new-fork functional parity. Existing SQLite data and stable repository storage identities have their own upgrade tests; source publication does not authorize a runtime upgrade or certify every older client.

## Required receipts

The final record identifies the exact source SHA and tree, capability commit sequence, production/test preservation check, current-tree and full-new-history publication scans, independent secret scan, exact-head hosted jobs, repository refs/surface inventory and separation from former automatic publication destinations.

Public readiness is a source/disclosure checkpoint, not a claim of zero bugs, complete PostgreSQL/platform coverage, production rollout, automatic PKI renewal, durable Edge audit or instant network failover. [Edge](../../docs/architecture/ags-edge.md) retains its precise runtime and operational limitations.

## Reproducible checks

```bash
python3 fork/scripts/audit.py verify
python3 fork/scripts/audit.py inventory --details
python3 fork/scripts/audit.py publication
PYTHONDONTWRITEBYTECODE=1 python3 fork/scripts/test_generation.py
(cd fork/tools && go test ./... -count=1)
```

The operator additionally supplies its private literal policy and compares the generation to the retained input using `go run fork/tools/coverage.go`. Those private input refs and policies are not pushed with the public generation. Source identity, tests and public-surface readiness are recorded separately before this status is promoted.
