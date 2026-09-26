# Capability decisions for the next generation

This is a contract inventory, not a statement that the entire historical fork has been reviewed. The machine-readable commit/path ledger is produced by `scripts/audit.py inventory --details`; its initial semantic status is deliberately `pending` for every commit.

| Capability | Target decision | Contract to preserve | Required review/verification |
| --- | --- | --- | --- |
| GitHub-shaped API, native Git, OAuth and Wiki | upstream | Reuse the current official implementation; do not resurrect retired upstream features by accident | Existing upstream tests, installed-client compatibility and source-schema upgrade |
| SQLite runtime and retained database adapters | keep | Existing data and stable repository storage identities survive migrations | Real previous-to-current SQLite migration; TiDB semantics; explicitly bound PostgreSQL coverage |
| Actor/executor separation and Access Grants | keep | Source provenance never grants authority; exact repository/operation/use-time checks; revoked parents cannot issue transport | Closed wire schemas, privilege denial, cancellation/revocation and effect-ledger concurrency |
| Forgejo and optional GitLab/GitHub projection | keep | Optional provider adapters; original user authorization; exact ref/PR binding; unknown effects are not replayed | Mapping drift, independent review/CI rules, failed/duplicate callbacks, provider unavailable |
| Multica execution-context and terminal delivery | keep | Public provider contract, fixed egress, no private workspace/host defaults; terminal handoff is not provider-effect success | Canonical schemas, idempotency, row ownership and retry boundaries |
| Notifications and provider log bridge | keep | Explicit opt-in; fixed target registration; bounded data and credential redaction | Secret-bearing errors, timeout, trust boundaries, Unix socket ownership and delivery consistency |
| AGS Edge | keep | Independent optional binary, one primary authority, real original-user authorization, exact local snapshots, incremental replication, bounded retention and no ambiguous write replay | Real Git v0/v2, stale discovery/want races, user/node revocation, corrupt/incomplete objects, space/cancellation/restart, proxy-loop and host validation |
| Official gh with independently selected CI | reimplement | Standard repository/PR operations remain official-gh compatible; native, none or named external CI feeds the same checks/run/log surface without selecting Git hosting or merge authority | Fixed official-gh acceptance, current-head and required-policy truth, cross-backend ID isolation and typed log bounds |
| Native client-run association | reimplement | A task-scoped native actor login carries optional source context through standard Git/gh; missing association does not grant or revoke ordinary permissions | Parent/user revocation, concurrent-task isolation, precise source binding, no durable credential escape and companion source-pair acceptance |
| Source hosting and CI | reimplement | GitHub origin; standard hosted runners; immutable source identity; no AGS service needed to contribute | Complete package inventory, fork branch triggers, least privilege, absence of private runners/deploy credentials |
| Operator deployment transcripts and workstation helpers | externalize | Keep private recovery evidence, not public product defaults | No operator hostname/IP/account path/token/certificate in public tree or reachable history |
| Historical delegation issuers and temporary compatibility | review before retire | Do not revive retired minting paths; retain only readback/installed-contract needs | Consumer census and explicit absence gates before deletion |
| Old issue attachments/presence/typing/read-state | upstream removal, compatibility gate | Do not silently claim old clients still work | Real consumer verification before a runtime release |

## Findings addressed during preparation

### AUTH-01: inactive approver could still exchange a device code

The exchange path checked the original approver's account type but did not reload its current status. A previously approved code could create a token, or update and return an existing token, after the account became banned, suspended or deleted.

The exchange now current-reads and locks the approver after the device-code lock and refuses inactive status before any token write. `internal/service/auth_device_revalidation_test.go` reproduces all six denial cases plus the two active controls with a real isolated SQLite database. This does not claim a broader authentication audit is complete or prove any production exploit occurred.

### GIT-01: request spooling ignored cancellation

`SpoolChunkedBody` accepted a context but an input read could continue indefinitely after cancellation. It could also return a spool for an already-cancelled request.

The adapter now closes the request body on cancellation, preserves the cancellation cause and releases the temporary file. `internal/gitbackend/spool_cancellation_test.go` exercises a blocked reader and the pre-cancelled entry. Existing Git backend tests remain part of the regression gate. This protects the shared primary/Edge transport component; it is not a claim that every network failure is tested.

### ROUTE-01: an unchecked rollback receipt could change unrelated Git settings

The client route tool trusted all keys in its private receipt. A malformed receipt could make removal restore a non-routing key such as `user.name`. The read path now validates the closed receipt schema, matching key sets and exact URL-scoped `curloptResolve` ownership before reading or modifying Git configuration. It does not treat a private JSON file as an unlimited configuration-edit capability.

The same audit found that the nominally read-only `plan` command created state directories. Planning now returns before provisioning state, and missing-status inspection does not create a directory. `scripts/test_edge_client_route.py` covers these regressions alongside idempotence, drift and exact rollback using documentation-only names and addresses. The default route name is now the generic `primary`; existing named receipts still require their explicit name. This source change is not an automatic migration of an installed operator tool.

### PROVIDER-01: authenticated Git URLs entered process arguments

The native GitHub/GitLab/Forgejo pushers, Forgejo ref readers and merged-commit fetch passed authenticated in-memory URLs directly into native Git. Error redaction did not remove those credentials from argv, and some merged-commit failure paths could retain raw remote output.

`internal/gittransport` now owns this shared process boundary: sanitize the exact remote argument, keep credentials out of argv/environment/repository config, apply request-owned mode-0600 HTTP configuration inside a mode-0700 directory, scope headers to the target URL, reject redirects and disable inherited credential helpers/tracing. Normal completion, cancellation and start failure remove the temporary directory. A host crash or SIGKILL cannot run cleanup; operators must use a private ephemeral filesystem and normal orphan-file cleanup. Root and same-OS-user access are not claimed to be isolated.

All identified native provider push, ls-remote and merged-object fetch paths use the runner. It executes once, retains exact force-with-lease arguments and bounds output. Errors expose only known fixed failure classes and exit/cancellation status, not raw provider response text. Real-Git tests cover authenticated push/read/fetch and redirect refusal; process fixtures verify argv/environment, cleanup and cancellation. Legacy provider APIs still construct authenticated URLs in memory for compatibility: those values remain sensitive and are not public diagnostics. This bounded repair is not a claim that every provider HTTP path has been audited or any live credential exposure occurred.

### PROVIDER-02: a short hash prefix could attest projected-head convergence

`verifyProjectedHeadSHA` previously accepted a prefix match in either direction. A shortened provider response could therefore pass an equality check intended to attest an exact projected Git object. The function now requires exact equality of the compared IDs. The regression in `internal/forgejointegration/exact_head_test.go` rejects short SHA-1 prefixes in both directions and mixed-length object IDs while preserving the exact-match control. The full provider package passes repeated race runs. User-facing abbreviated-history search is a different operation and was not globally rewritten.

### Documentation correction: action recovery must keep its original generation

The deployment guide still described a new signed request adopting a historical partial rebase. That advice contradicted the current exact-intent gate. The public [provider-effect contract](../docs/architecture/provider-effects.md) and deployment guide now require the same nonterminal bound generation, its saved desired SHA and exact accepted-old-SHA lease. Existing rejection tests remain authoritative; no recovery permission was widened to preserve an outdated narrative.

## Review order

Review the authority/effect graph and storage migrations before re-committing the accepted stack. Then review Edge data/control-plane boundaries, followed by provider adapters and client compatibility. Product-neutral documentation and build policy can be separated earlier. Do not flatten the old history merely to make a scanner pass: the private ledger must retain the relationship between old changes, accepted contracts, omitted material and replacement tests.
