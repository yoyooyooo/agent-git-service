# Forgejo rebase execution lifetime

Status: **proposed; not implemented; requires operator approval**.

## Problem and current repair

A signed label request currently admits an exact action intent and runs rebase
and provider projection synchronously in a two-minute webhook processing context.
The existing durable projection job records the generation, expected provider
head and desired AGS head. This proposal does not replace that state model.

The accompanying corrective patch distinguishes unavailable reads from observed
fact/authority drift. When the synchronous attempt returns an unavailable error,
a separate five-second context can record **local** recovery state in the same
intent and job. It does not extend expiry, replay Git, push, change labels, or
schedule a new executor. PR-level projection presentation includes action jobs.
HTTP observation failures return 503 rather than an invalid-webhook 400.

These repairs preserve evidence and make explicit recovery safer. They do not
remove the two-minute execution window or guarantee a Forgejo status update while
that provider cannot be inspected. An interrupted action may still require the
existing explicit retry/startup recovery path, and expired authority still denies.

## Options

### A. Keep synchronous execution and the corrected failure boundary

No new runtime component or schema change. Suitable while these operations fit
inside the current request budget. A durable failure and a usable recovery hint
replace a false running state, but long operations can still run out of time.
Increasing the timeout alone only changes where the same failure recurs, so this
is not recommended as the long-term solution.

### B. Hand admitted work to the existing projection worker (recommended)

Reuse `pull_request_action_intents`, `pull_request_projection_jobs`, generation
binding and the existing worker. No external queue, new service, generic workflow
engine, per-command permission system or alternate CLI.

Proposed sequence:

1. The webhook verifies signature/delivery, actor mapping, current authorization
   and exact AGS/provider coordinates. It durably admits the intent and its action
   job before acknowledging acceptance. A response means **accepted**, not rebased.
2. The existing worker claims that exact job generation and revalidates authority
   and current head/base facts immediately before each effect. Capture/rebase,
   exact lease projection and confirmation move behind one execution owner.
3. Reuse the same saved old/desired SHA on retry. A duplicate delivery must locate
   that generation, never create another rebase. Base movement and unknown third
   heads remain explicit conflicts, not excuses to weaken the lease.
4. Ensure admission-to-scheduling interruption is covered: a committed job without
   an in-memory wake-up must be discoverable by existing startup/reconciliation.
   A persistent, bounded discovery cadence may be needed; choose its placement
   after approval rather than slipping a new polling daemon into a bug fix.
5. Outcome presentation follows durable state. Queued, executing, native-complete
   but projection-pending, failed and completed are distinct. Labels/comments use
   fresh authorized provider calls; a UI label alone is not a live executor lease.

This changes an externally visible contract and ownership boundaries even though
it reuses existing components. It is deliberately not part of the corrective patch.

## Decisions to approve

Approve **option B only**, with these default constraints:

- Keep the present identity, grant, expiry and exact SHA rules. Do not silently
  renew an old action or adopt a different principal. Expired historical actions
  require explicit authority reconciliation, not a new label that borrows history.
- A rebase yielding no remaining changes reports that outcome; do not automatically
  close/merge the PR or delete the branch. Automatic closing would be a separate
  product decision, not necessary to repair execution lifetime.
- Use the existing database and worker. Any schema extension must prove a missing
  invariant (for example exclusive owner/lease) rather than add another job ledger.
- Execution cancellation gets a bounded local finalizer, never an unlimited
  `WithoutCancel` path that continues external effects.

## Acceptance before deployment

Use isolated Git, SQLite plus existing TiDB regression, and an explicit provider
boundary. Cover: short webhook acknowledgement despite slow rebase; crash after
admission and before wake-up; duplicate label while executing; provider read
unavailable after native mutation; restart with a saved desired SHA; concurrent
replacement generation; expired/revoked original authority; a true third provider
head; empty rebase; one success comment after exact convergence.

Promote on the personal canary first. Do not replay a historical production action
just because source tests passed. Establish its current job, generation, principal,
expiry and both refs before proposing a concrete recovery operation.
