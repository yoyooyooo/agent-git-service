<div align="center">

# agent-git-service

**A self-hosted, GitHub-compatible API server for agents, automation, and
developer workflows.**

[![CI](https://github.com/yoyooyooo/agent-git-service/actions/workflows/ci.yml/badge.svg)](https://github.com/yoyooyooo/agent-git-service/actions/workflows/ci.yml)
[![Upstream CI](https://github.com/ngaut/agent-git-service/actions/workflows/ci.yml/badge.svg)](https://github.com/ngaut/agent-git-service/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8.svg)](go.mod)

</div>

`agent-git-service` lets GitHub-speaking clients work with repositories you
own, and its agent-first design treats AI agents as first-class citizens with
durable identities, scoped tokens, default workspaces, and ownership/recovery
flows.

It exposes GitHub-style REST v3, GraphQL v4, OAuth device flow, and Git Smart
HTTP while storing repository data in real bare Git repositories and product
metadata in TiDB/MySQL-compatible storage.

The development binary is currently named `gh-server`.

## Why

Use `agent-git-service` when you want a GitHub-compatible service that can
run where your agents run:

- Keep repositories and product metadata under your control.
- Support existing GitHub clients instead of inventing new client protocols.
- Give agents durable accounts, tokens, optional human binding, and repository
  transfer flows.
- Preserve Git-native clone, fetch, push, refs, diffs, merges, and history in
  real bare Git repositories.

## agent-git-service vs GitHub

| Capability | agent-git-service | GitHub.com | How it differs |
|---|:---:|:---:|---|
| GitHub-style repositories, issues, labels, wiki, and Git history | Yes | Yes | Both support familiar GitHub-shaped collaboration workflows. agent-git-service keeps these workflows available on a self-hosted backend. |
| Git Smart HTTP, REST v3, OAuth device flow, and common `gh` workflows | Yes | Yes | Existing GitHub-speaking clients can work against agent-git-service without learning a new protocol. |
| First-class durable agent accounts | Yes | No | GitHub supports bots, Apps, PATs, and machine users, but agent-git-service gives agents their own durable accounts, tokens, and default workspaces. |
| Direct agent permissions across repos, orgs, and teams | Yes | Partial | In agent-git-service, agents can be granted collaborator, org, team, and team-repo access directly instead of relying on App or PAT indirection. |
| Human-agent binding and recovery | Yes | No | agent-git-service supports human-agent binding, connected login, switch sessions, and recovery flows as first-class workflows. |
| Self-hosted agent service | Yes | No | agent-git-service can run where your agents and data live, with local control over identity, storage, and rate-limit policy. |
| Local data, Git storage, and metadata ownership | Yes | No | agent-git-service stores repositories as real bare Git repos while keeping product metadata in TiDB/MySQL-compatible storage. |
| Full hosted GitHub product ecosystem | Partial | Yes | GitHub.com is broader across Actions, security products, marketplace, traffic/community features, and long-tail APIs. |
| Full GitHub GraphQL schema parity | Partial | Yes | agent-git-service provides GraphQL compatibility for selected workflows, not full GitHub GraphQL parity. |

## Enhancements

| Enhancement | What it adds |
|-------------|--------------|
| Agent identities and governance | Durable agent accounts, API tokens, human binding/recovery, switch sessions, default repos, repository transfer flows, and direct repo/org/team permission grants |
| GitHub-compatible core | REST v3, GraphQL compatibility, OAuth device flow, Git Smart HTTP, and `gh` acceptance coverage for common workflows |
| Issue collaboration | Issues, labels, comments, pinned comments, and reactions; retired typing/presence/attachment/read-state APIs are not advertised as supported |
| Wiki memory | Git-backed pages, history, search, labels, backlinks, page moves, reconcile, repair, and compact operations |
| Semantic search | Optional embedding-backed issue and pull request search |
| Self-hosted operations | Local data and Git storage, local rate-limit policy, Prometheus metrics, readiness checks, structured logs, and a Grafana dashboard |
| Optional Edge read replicas | Unchanged primary Git URLs, original-user authorization, verified local snapshots, incremental transfer, explicit forwarding, bounded retention and diagnostics; writes remain primary-owned |
| Fork authority and provider integration | Actor/executor separation, operation-scoped grants, exact effect recovery and opt-in provider projections; external metadata does not grant permission |

Known GitHub-compatibility gaps are tracked in
[`docs/github-api-compatibility-matrix.md`](docs/github-api-compatibility-matrix.md).

## Install or try AGS

- **Try the service from source.** Follow the [quick start](docs/quickstart.md)
  to run a local server with a disposable TiDB Zero database. The guide takes
  you through readiness, an authenticated `gh` request, and your first Git push.
- **Install an existing release.** Check the [published releases](https://github.com/yoyooyooo/agent-git-service/releases)
  and follow the [installation instructions for historical v1 bundles](docs/operations/releases.md#historical-v1-install-and-upgrade)
  if your release uses v1. The [v2 consumer](docs/operations/releases.md#v2-consumer-and-local-upgrade-source)
  can verify a v2 release from a complete reviewed checkout, but direct v2
  installation is closed. Installing binaries does not upgrade a running service.
- **Plan production use.** Read the [production deployment guide](docs/production-deployment.md)
  for prerequisites and configuration. Use the [release operations guide](docs/operations/releases.md)
  for provenance, upgrade, and rollback boundaries.

## Contribute

This repository is the maintained GitHub fork of
[`ngaut/agent-git-service`](https://github.com/ngaut/agent-git-service).
You do not need a running AGS, a private network, or a self-hosted runner to
contribute. Read the [contributing guide](CONTRIBUTING.md) for development
checks or the [documentation index](docs/README.md) for contracts, testing,
and fork governance.

## License

Licensed under the [Apache License 2.0](LICENSE).
