# Collaboration pass-through + association sidecar

Status: implementation-in-progress  
Date: 2026-08-10  
Authority: Nowledge `ags-team` Current Home

## Decision

AGS remains Git/PR collaboration infrastructure plus execution-context association.
It is not a fine-grained permission gateway for daily Agent work.

- Ordinary Git/PR success must not depend on association completeness.
- Association status is observability only: `linked | provisional | unlinked | conflict`.
- Internal transport receipts may still exist, but they are not the product decision surface.
- `AGS_CONTEXT_IDENTITY=1` only enriches association receipts.
- `ags-cli context verify` judges association independently and must never gate push/PR create.

## Server changes (this branch)

`AccessGrantReceipt` now includes:

- `association_status`
- `identity_kind`
- `collaboration_mode=passthrough_transport`
- warning `collaboration_passthrough_transport`

Conflict/provisional association does not remove collaboration operations from the default envelope.

## CLI changes (agent-kit branch)

- `ags-cli context verify`
- `context-identity` helpers and unit tests
- Grant JSON may attach a `collaboration` block when `AGS_CONTEXT_IDENTITY=1`
- Skill/docs path prefers direct `git`/`pr` over Grant lifecycle

## Non-goals

- Rebuilding policy class / native grant as Agent-facing authorization
- Expanding Multica product surface first
- Using association failure as a machine-onboarding signal
