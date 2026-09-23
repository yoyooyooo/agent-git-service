# Repository guidelines

## Source workflow

This is a GitHub-maintained fork. Use ordinary Git and the official GitHub tooling against `origin`; `upstream` is the official source and must not receive pushes. Contributing and CI do not require an AGS deployment, provider account, private mesh or self-hosted runner. Optional integrations in the product do not change this repository's source authority.

Read [fork governance](fork/README.md) before changing generation history, version identity, CI or publication. Freeze upstream in `fork/UPSTREAM_BASELINE`; absence of an upstream release tag is explicit, not permission to invent a version. New generations reconstruct accepted capabilities from that baseline; older branches are private evidence, not an automatic replay queue. Keep feature changes linear and preserve upstream licenses and attribution.

`prepare/*` branches are working material. Neither a clean checkout nor a scanner pass authorizes public visibility, default-branch replacement, deployment, historical ref removal or mutation of running services. Follow [publication gates](fork/PUBLICATION.md). Never publish all local refs or private evidence as part of a normal push.

## Layout and ownership

- `cmd/gh-server` is the primary command entry; `server` owns composition and lifecycle.
- `cmd/ags-edge` is an independent optional read-replica process; it does not bootstrap business services or a business database.
- `internal/service` owns domain authorization, durable facts and orchestration. Transports remain thin.
- `internal/db` owns models and migrations; preserve deployed SQLite data and explicit database contracts while retaining upstream TiDB semantics.
- `internal/gitstore` owns native Git behavior. `internal/gitbackend` executes already-authorized protocol requests.
- `internal/edge`, `edgeprotocol`, `snapshotstore` and `replication` own the separated replica runtime and contracts.
- `cli/` and `cli/_go-gh-local` are independent Go modules for client compatibility.
- `docs/README.md` routes to architecture, module contracts, testing and operator guides. Private runtime observations do not become public product defaults.

## Verification entries

Use the Go version required by each `go.mod` and format changed Go files with `gofmt`.

```bash
go build ./...
go vet ./...
python3 fork/scripts/audit.py verify
python3 fork/scripts/audit.py inventory --details
PYTHONDONTWRITEBYTECODE=1 python3 fork/scripts/test_audit.py
PYTHONDONTWRITEBYTECODE=1 python3 scripts/test_edge_client_route.py
```

Full server tests require an isolated test database, never a deployed AGS database. GitHub-hosted CI provisions a job-owned loopback TiDB and checks all root packages exactly once. The operator-only publication policy remains outside the source tree. Full generation/PR CI includes root regression, Edge and authority race checks, and client contracts; preparation pushes deliberately run a smaller gate. A skipped suite is not a pass.

Use real Git, real isolated storage, original-user authorization and explicit dependency boundaries in integration tests. Preserve the reproduction before fixing a bug. Do not weaken permission checks, foreign keys, exact-effect identity, source attribution or result assertions to obtain a green test.

## API and safety contracts

Use `/api/v3` and `/api/graphql` for GitHub-shaped APIs and `/api/ext/v1` for new extensions. Existing fork compatibility routes are explicit exceptions documented in the router/OpenAPI, not a reason to add new platform APIs to `/api/v3`.

Source provenance, optional associations and display names do not grant authority. Peer replication permission is independent from the user's Git permission. Unknown provider writes use exact readback, not blind POST replay. A local cache cannot bypass revocation or substitute an unintended version.

Configuration is opt-in, bounded and credential-free where persisted as a receipt. Diagnostic output must not contain tokens, private keys or user request bodies. Examples and fixtures use synthetic identities and documentation hosts; real operator topology and deployment evidence stay private.

## Commit and review expectations

Keep changes scoped by capability and include their owning tests and contract updates. Inventory modifications to upstream paths rather than hiding them behind artificial wrappers. Report source tests, hosted CI, private publication checks and runtime acceptance separately. Do not deploy an exact-source build merely because its compilation succeeded.
