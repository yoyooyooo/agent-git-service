# Downstream capability preservation

This record separates source preservation, behavioral regression and runtime compatibility. A file-count or structural check is not a claim that every behavior is bug-free.

## Preservation baseline

The private pre-upstream fork introduced 174 production Go files relative to its own upstream merge base. Every one is present in this generation; none was removed as part of publication cleanup. The later reviewed source includes upstream reconciliation and the reproduced authorization, cancellation, routing and provider-credential fixes.

Against that reviewed construction input, `fork/tools/coverage.go` accounts for 985 production Go files, 792 existing Go test files and 4,125 test/benchmark/fuzz declarations across the server and vendored clients. Generation reconstruction preserves every production Go blob and the imports, declarations and non-string token structure of existing tests. Changed strings must still pass the complete final regression. This gate intentionally rejects deleting assertions or changing conditions to hide a fixture mismatch.

Retain the original source comparison and commit-to-capability ledger privately. It includes the prior 290-commit upstream-reconciliation line, historical runtime checkpoints, subsequent fixes and the externalized operator-only files. Those records are evidence, not parents of the public Git history.

## Capability matrix

| Capability | Retained contract | Verification boundary |
| --- | --- | --- |
| Native Git and repository lifecycle | Real bare Git data, stable identity, branch protection, ownership, rename/recreate distinction | GitStore, Git HTTP, primary Service and real-Git integration tests |
| SQLite runtime / database adapters | Existing rows and repository storage identity survive upgrade; explicit supported dialect behavior | Prior-schema migration harness, SQLite integrity tests and isolated TiDB matrix |
| Actor, executor and task context | Attribution cannot become authority; fixed source routes and purpose-bound intake | Execution-context, authority, middleware, REST and GraphQL tests |
| Access Grants and transport sessions | Exact operation/repository binding, use-time permission, revocation and expiry | Lifecycle, negative permission cases, race tests and transported Git/PR entry points |
| Merge/rebase and provider effects | Exact head/base/ref identities; one external mutation boundary; unknown outcomes use readback | Concurrent intent/dispatch, callback drift, force-with-lease and recovery tests |
| Forgejo projection | Optional source-owned mappings, PR/CI evidence, branch and merge convergence | Provider adapter, REST, Service and signed webhook regressions |
| GitHub/GitLab projection | Explicit optional secondary destinations; independent outcome bookkeeping | Native authenticated Git, shadow PR/MR and projection bookkeeping tests |
| Multica linkage and terminal delivery | Canonical closed payload, authoritative association and idempotent delivery; receiver owns task completion | Canonical fixture hash/roundtrip, lifecycle, dispatcher and error tests |
| Notifications and provider logs | Opt-in destinations, bounded output, secret-safe errors and real socket ownership | Notification, log-bridge, spoofed-header and decoder-cancellation tests |
| Edge foreground reads | Original user and node authority remain independent; exact locally published view | Git v0/v2, discovery/want races, revoke/recreate, mTLS and process tests |
| Edge replication/storage | Incremental transfer, immutable views, shared pack ownership, leases and bounded retention | Corruption, interrupted transfer, capacity, restart and lifetime tests |
| Edge gateway operations | Unchanged primary remote, explicit unbound forwarding, no ambiguous write replay | Fresh clone/push/readback, aliases, private/unbound paths and connection boundary tests |
| Edge observation and warming | Node-only periodic warming, current readiness and loopback diagnostics | Control/data separation, telemetry limits, expiry, error and queue tests |
| Client compatibility | Existing primary API/credential routing, PR and native Git workflows | Complete CLI compile, core behavior tests and complete local API-client suite |
| Official gh / configurable CI | Standard draft/HEAD/checks/run/log and expected-head merge; explicit native/none/Forgejo/GitHub Actions selection independent from merge authority | No shim or per-command routing overrides; unsupported provider actions remain explicit, no fallback to unrelated native facts |
| Native task-run association | Existing native actor with short-lived run credential, optional Task/Run metadata and durable PR/provider links | Association is not a permission grant; parent/user revocation and concurrent tasks verified; live task-launcher integration remains an explicit rollout |
| Source delivery | GitHub origin and hosted CI; no private runner or running AGS needed to contribute | Complete package inventory, exact-source build, disposable database ownership and publication tests |

The generation's complete hosted run must include all 60 root-module packages, the client gate and the selected race gate. A preparation push with skipped jobs is not full acceptance. Exact job receipts belong to the release manifest.

## Public-fixture reconstruction

Synthetic names must remain consistent across raw JSON/YAML, HTTP request paths, case-normalization examples, expected attribution and fixed fixture digests. Repair the input/expected relationship, not the production policy. The closed canonical payload and its reviewed SHA-256 remain mandatory; updating that digest does not relax its schema or roundtrip checks.

The publication scanner checks operator literal variants without case sensitivity. Its synthetic fixture exceptions are constrained to an exact test path, exact blob and exact URL/key-shape rule; operator names, network locations and home paths cannot be suppressed through that manifest.

## Explicit compatibility limits

The upstream transition retired the former multi-tenant control-plane route and old presence, typing, issue-attachment and read-state surfaces. This generation does not advertise those retired upstream features or silently count their removal as downstream parity. A running deployment or old client that relies on them needs a separate compatibility decision before upgrade. Existing runtime binaries and data are not replaced by publishing source.

Edge still does not imply multi-primary writes, durable event subscription, automatic certificate renewal, resumable partial transfer, permanent client fallback evidence or complete outage masking. Its current behavior and operational limits remain in the [Edge contract](../docs/architecture/ags-edge.md). The fork remains source-maintainable without deploying any of these optional services.
