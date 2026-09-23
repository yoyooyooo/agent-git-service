# Provider effects, projection recovery and workflow delivery

This is the reusable contract distilled from the fork's earlier projection and task-link designs. Machine inventories, incident transcripts, personal task IDs and deployment receipts are private operating evidence, not the specification. It describes the product's optional adapters; this source repository itself uses ordinary GitHub collaboration.

## One authoritative decision

AGS owns repository identity, native caller permission, PR/review facts, expected head/base and the authorization of an external effect. A provider owns the actual execution and its observed external result. Projection mappings bind exact repositories and PR numbers; equal numbers across systems are never assumed.

The canonical actor, execution principal, request provenance and provider executor are different facts. Human or workload identity is checked by AGS. Provider credentials are deployment-owned; clients do not supply a provider repository or account as an alternate authorization channel. Source provenance, a task association or a display name does not grant permission.

A local Git change, an HTTP 200, a posted comment and a terminal task state are not interchangeable evidence. Report separately: source write; required projection; optional backup; notification delivery; and task-system acknowledgement. Optional projection failure must not roll back an already successful source write, but a required projection failure must not be called successful convergence.

## Exact action and merge boundaries

An action intent binds the repository/PR, actor and current authority, action kind, mapping, head/base, expiry, accepted provider old head and a durable generation. Expected Git object IDs are validated on their original bytes; normalization must not quietly authorize a different input. Live facts and current permission are rechecked before dispatch, including after lock waits.

Concurrent intent creation and dispatch use a consistent grant -> PR -> projection -> invocation lock order. Unique durable effect identity and exact payload comparison fence duplicate requests. A provider POST occurs outside replayable database transactions. Unknown response outcome is reconciled through an exact read; a transport error does not authorize another POST.

For rebase projection, a non-fast-forward write is limited to an eligible work branch and uses an exact `force-with-lease` expectation. Default/base branches, tags, deleted refs, cross-repository heads and unrecognized remote drift do not enter that force-capable path. A concurrent provider update must survive a failed lease.

Only the same nonterminal, already-bound generation may resume its saved desired SHA and expected-old-SHA lease. A new signed label or intent cannot adopt an unrelated partial job, operator-inspected state or a newer head as recovery. Unknown mapping/head/base changes require a new decision rather than inferred force.

A successful provider ref update is followed by independent branch and provider-PR-head checks and a final source head/base check. Bounded polling may accommodate delayed provider reads; exhaustion is not success. Persist terminal verified state before emitting success presentation. Failed label/comment delivery never erases the durable action result.

## Durable projection and outbound ledgers

A projection job records the trigger and action generation, source/provider locators, expected/desired/observed refs, phase, attempt, correlation ID and safe failure classification. Phases distinguish preflight, local rebase, projection resume, push, branch verification, PR verification, recording and terminal/retryable outcomes. Durable ownership matters; an in-process mutex is not the cross-process correctness boundary.

Projection events preserve the audit sequence. Current ref state represents active/resolved drift. Outbound rows represent notification/task-delivery intent and receipt. These are distinct records with distinct authority.

Required projection failure must leave durable drift and an outbound intent, not only a log message. Repeated observations use a stable provider/repository/ref/incident-generation identity so attempts do not create unrelated duplicate incidents. Retry, backoff, leases, dead letter and resolution notifications remain observable.

A runtime enabling signed provider workflow actions must explicitly configure the necessary native repository policy, webhook, workflow labels and alert target. An explicit development opt-out is reported as degraded, not silently healthy. A configured provider policy is not a substitute for service-side permission, lease and exact-result verification.

Native provider Git I/O uses `internal/gittransport`: credential-free arguments, request-owned authentication, bounded output, cancellation cleanup and one-shot execution. HTTP(S) authentication is never grafted onto local or SSH URLs. Diagnostics cannot contain provider tokens, credential-bearing URLs or raw secret-bearing error responses.

## Task context and External PR linkage

Execution context is read through a registered connector and a fixed deployment-owned endpoint. A caller-provided endpoint is only an exact routing hint, never arbitrary egress. A current task token is request-scoped. The context snapshot does not by itself authorize a Git/PR effect.

External PR linkage accepts the purpose-bound task-link contract rather than inferring authority from prompt text, branch names, profile names or an issue-looking marker. Source PR creation and association have separate outcomes; a valid source operation can succeed even when optional linkage is unavailable.

For a verified merged terminal fact, use the typed `complete-from-merge` delivery; for a closed-unmerged fact, use the typed links path. The durable dispatcher uses its registered target and owner-only service-token file. It must not accept a caller-selected URL, secret path, provider credential or arbitrary payload.

Task-system policy owns whether a leaf child becomes done and whether a parent is notified. AGS does not overwrite that policy or wake assignees through ordinary PR/CI comments. HTTP acknowledgement proves delivery acceptance, not task completion. The same durable wire identity is reused for safe delivery retries.

## Retained verification surfaces

The generation retains the previous fork's real-Git, real-database and controlled-provider regression suites. The expected scenarios include:

- Signed router-to-service action success and denial, mapping mismatch, unauthorized actor and exact expected-SHA validation.
- Rebase conflict, known same-generation partial recovery, unknown drift, concurrent remote update, moving base, lease failure and restart.
- Required projection failure versus optional backup failure, independent branch/PR verification, stale provider read timeout and terminal presentation errors.
- Concurrent same-effect creation, lost provider responses, expiry/revocation after waits and no duplicate external effect.
- Immediate drift/outbound persistence, retry-wait, dead letter, duplicate delivery and resolved-state notification.
- Provider policy/readiness denial, socket-peer identity rather than spoofed forwarding headers, and bounded/decompressed log handling.
- Canonical actor presentation without replacing the executor, task-bound linkage, closed typed envelopes and terminal-delivery policy separation.

These are release obligations, not a claim that every environment or external provider version has been exercised. The coverage receipt, exact-head hosted test run and operator rollout evidence remain separate. Public generation construction verifies that production Go blobs and existing test structure survived; it does not replace the tests or certify absence of bugs.

Related: [capability decisions](../../fork/CAPABILITIES.md), [Access Grants](access-grants.md), [execution context](execution-context-intake.md), [Forgejo adapter](../forgejo-integration.md), and [Edge](ags-edge.md).
