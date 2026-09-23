# Documentation

Start here when you need the shape of the repo rather than a specific source
file.

## Run Locally

- [Quick Start](quickstart.md) - run `agent-git-service` locally with TiDB Zero.
- [Configuration Reference](../.env.example) - primary environment variable reference.
- [Edge Configuration](../.env.edge.example) - database-free local Git snapshots, fixed-primary gateway, explicit aliases, warming and diagnostics.

## Architecture

- [Architecture](architecture.md) - system overview and current implementation baseline.
- [Module Contracts](module-contracts.md) - ownership, dependency rules, and accepted couplings for `internal/*`.
- [GitHub API Compatibility Matrix](github-api-compatibility-matrix.md) - supported surfaces and known gaps.
- [Test Strategy](test-strategy.md) - test pyramid, CI layers, and execution commands.
- [Forgejo Integration](forgejo-integration.md) - optional AGS -> Forgejo branch/PR mirroring for Forgejo Actions.

Component and cross-cutting references live in [architecture/](architecture/):

- [REST API](architecture/rest.md)
- [GraphQL API](architecture/graphql.md)
- [Service Layer](architecture/service.md)
- [Git Store](architecture/gitstore.md)
- [Git Smart HTTP](architecture/git-http.md)
- [AGS Edge](architecture/ags-edge.md) - implemented local-read, incremental sync and gateway contracts; dated live receipts do not approve deploying a later rebased candidate.
- [OAuth](architecture/oauth.md)
- [Collaboration Framework](architecture/collaboration-framework.md)
- [Error Semantics](architecture/error-semantics.md)
- [Secrets Encryption](architecture/secrets-encryption.md)
- [Wiki Storage V2](architecture/wiki-storage-v2.md)

## Design Records

Design records live in [design/](design/). They may describe current behavior,
accepted direction, or incremental work that has not fully landed yet.

- [Agent Auth and Account Model](design/agent-auth.md)
- [Legacy Delegation Migration Input](design/delegation-policy.md)
- [Principal-Bound Operation-Scoped Session](design/delegated-agent-session.md)
- [Authorization Layer](design/authz-layer.md)
- [Wiki Storage Re-Architecture](design/wiki-storage-rearchitecture.md)

## Testing And Operations

- [Fork Governance](../fork/README.md) - GitHub source workflow, upstream generations, capability review and publication gates.
- [Production Deployment](production-deployment.md)
- [AGS Edge Host Gateway](operations/ags-edge-host-gateway.md) - global client routing, node-only prewarming, diagnostics and failure semantics.
- [Edge Client Routing](operations/ags-edge-client-routing.md) - exact per-user configuration ownership, drift checks and rollback.
- [AGS Edge Primary Listener](operations/ags-edge-primary-listener.md) - explicit mTLS startup, filesystem preflight, shutdown and remaining rollout gates.
- [AGS Edge Enrollment](operations/ags-edge-enrollment.md) - native-admin registration CLI, exact repository receipts and unapplied certificate-based peer plans.
- [CI](ci.md)
- [Wiki Storage V2 Cutover Checklist](operations/wiki-storage-v2-cutover.md)
- [Token Lifecycle Test Coverage](testing/token-lifecycle.md)
- [Dependency Licensing](governance/dependency-licensing.md)
- [Monitoring Assets](monitoring/README.md)
