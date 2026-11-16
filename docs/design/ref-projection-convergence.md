# Ref projection convergence: contract and safe diagnostics

Status: **S0/S1 only**. The contract is `ref_operations.v1`. No ref-operation
route, planner, signer, review store, receiver, executor, retirer, worker or
configuration enablement exists in this slice. `RefCapabilities()` returns
false for execute, retire, strict fence and reverse PR history on every call.
There is no environment variable or flag to turn them on. Existing PR retry,
repair, artifact-retirement and ordinary mirror paths are **not** this protocol
and have not become strict-fenced by this change. This is not a rollout permit.

- [Writer inventory and adapter assessment](ref-projection-writers.md)
- [Draft request schema](ref-projection-api.schema.json)
- Type ownership: `internal/forgejointegration/ref_operation_contract.go`
- [Diagnostic operating boundary](../operations/ref-projection-operations.md)

## Scope and invariants

Convergence is independent of PR projection jobs. v1 will accept ordinary
`refs/heads/*` commit heads only, bounded full ref names (512 bytes), full OIDs
validated by actual object format, and one immutable AGS-to-provider mapping.
SHA-256 width is not proof that the complete installed stack supports SHA-256;
unknown format/capability is blocked, not silently treated as SHA-1.

The only future converge effects are verify-only noop and strict FF of an
existing provider F to immutable source A, with F a **strict ancestor** of A.
No missing-head create, force/mirror/all, divergent rewrite, provider-ahead
rollback or provider-to-AGS import is authorized. A lease encodes old-SHA CAS;
it does not prove FF, detect ABA or retract already-dispatched writes.

Retire means removing a provider projection, never creating/deleting an AGS
head or propagating deletes to another provider. Source must be successfully
observed absent with a tombstone revision; 403/404/timeouts mean inaccessible
or unknown, never proven absence. A candidate is not permission to delete.
Protected/default/base/system/pull/wiki/archive refs cannot be retired.

## Future API (not registered)

All endpoints below are reserved under
`/api/ext/v1/repos/{owner}/{repo}/projection/forgejo/ref-operations`.
This does not add GitHub-shaped endpoints or alter existing retry/repair APIs.

| Method and suffix | Contract | Effects when implemented |
| --- | --- | --- |
| GET `/plan?mode=converge&ref=...` | Repeated refs or bounded complete union inventory | Read only |
| GET `/plan?mode=retire&ref=...&review_id=...` | Evidence plus existing trusted reviews, no implicit approval | Read only |
| POST `/retirement-reviews` | Strict action union: stage candidate ownership OR approve | Explicit AGS evidence/review/audit writes only |
| GET `/retirement-reviews/{id}` | Reauthorize access | Read only |
| POST `/retirement-reviews/{id}/revoke` | Independent reviewer/revoker authorization | Explicit review/audit revocation, revoke uncommitted tickets |
| POST `/execute` | Original signed envelope, digests, exact confirmations, Idempotency-Key | Future admission and audited effects only |
| GET `/{operation_id}` and `/{operation_id}/events` | Live authorization, bounded monotonic event pagination | Read only |

Plan/status require repository read plus server-owned diagnosis permission.
Execute requires native owner/admin plus `projection.ref.execute`; retire also
requires `projection.ref.retire`. Delegated repo write, display login, mapping,
provenance and provider credentials grant none of these permissions. Review is
separate, as below. The draft JSON schema documents request shapes; it is not a
handler, a canonicalizer, a signature validator or an authorization decision.
Future decoders must additionally reject duplicate JSON keys, unknown fields,
duplicate refs, inconsistent modes/formats and unbounded payloads.

GET has `Cache-Control: no-store`; no business DB writes, migrations, scans that
resolve drift, enqueue/retry/repair, repo/key/ACL provisioning, webhook, archive,
review registration or provider write. Diagnostic objects must stay outside the
AGS bare object store: no source refs/objects/FETCH_HEAD changes. Runtime CLI is
a future thin API client, not an offline provider-token executor.

A versioned canonical signed envelope binds schema/planner/source version,
random plan ID, stable logical instance ID, immutable source/provider repo
identities, object format, TTL, exact sorted selection/effect order, inventory,
selection and baseline digests, B, observations, both ancestry checks, PR and
retirement evidence/R, trusted review bindings, allowed effect/transition spec,
signing key ID and signature. Signing authenticates issuance, not approval.
No token, secret path, credential hash or PR body is signed or logged.

A consistent observation reads every revision/watermark before and after heads,
pages, graph/history evidence. Differences trigger a bounded whole-snapshot
retry or `blocked_snapshot_unstable`, not mixed-page signing. Missing tables or
tracking in an early slice mean `executable=false`/incomplete, never implicit
migration or optimistic revision zero. Disabled mapping may retain explicit
coordinates for diagnosis but cannot enable execution.

## Immutable B, authenticated journal and cursor C

`RefBaseline` B is immutable, signed and never refreshed. It binds source and
provider immutable identities, all source/provider repo/ref revisions (absence
has a tombstone), reverse PR domain/revision/completeness watermark, mapping,
policy, credential generation, authority/revocation, inventory and selection.

C0=B. `RefExecutionCursor` C is only a prefix of committed effects authenticated
by the receiver. A current ls-remote or a coincidentally matching increment is
not authority to advance C. `RefEffectReceipt` binds receiver identity, immutable
domains, operation/item/effect/epoch, plan digest, input C digest, receipt ID,
previous receipt ID, continuous journal sequence, exact command kind, full
before/after ref OIDs and revisions, policy/hook evidence, explicitly permitted
PR derivations and receiver authentication. Receiver commit+journal must be
atomic or have an independently proven unambiguous durable recovery protocol.

A successful ref transaction advances provider repo revision +1 and every
actually created/updated/deleted ref +1. Deletion retains a tombstone. Noop
advances nothing. Initial archive-tag create + head delete is **one** transaction
(repo +1, both ref revisions +1); there is no pre-created archive tag in v1.
Example: two FF items take provider repo 20 -> 21 -> 22 through receipts C1/C2.
The second item expects C1, not original repo20. Unknown interposed events,
sequence gaps/rollback, foreign effects, ABA, duplicate effect with different
receipt, wrong epoch/chain/plan or unauthenticated receipts stop remaining work.
Repeated identical receipts do not advance C twice. Receipt/event append and
cursor version/item-phase CAS must share one short AGS DB transaction.

Source absence/heads, membership/history/R, configuration and review domains
remain B except a signed allowlist of synchronous pure PR cache derivations
whose exact identity/fields/before/after are attributed in the same receipt.
Membership/state/head-base/history changes are not cache derivations. Async or
unknown derivations block. If an earlier item changes a later item's unique/PR/R
review subject, split the batch or block it; never re-sign the review silently.

Crash/partition/guard loss after intent/send/receiver commit/journal/AGS cursor
or finalization: only inspect journal/refs/artifact, attribute an already
committed prefix and persist that fact. Do not replay, resume remaining effects,
refresh B/A/F or extend TTL. Seeing A or an absent head without this operation's
receipt is `outcome_unknown`/`absent_unattributed`, not success. Already-valid
commits remain facts; later external changes produce changed_after_effect or
partial, not rollback. A new plan, key and explicit execute are required.

## Admission and fixed idempotency scope

Future source and receiver gates must cover every writer in the inventory,
including cross-base PR references and authority revocation. Keep guards through
provider commit, exact readback and durable outcome. DB TTL leases only schedule;
old epoch requests must be rejected at the **receiver commit point**. Network
partitions cannot let cached source TTL/approval authorize commits. Gate handoff
cannot precede subprocess reap. Lock order is short admission DB transaction ->
source -> provider -> canonical ref/domain order; never hold DB transactions over
network or wait synchronously for provider callbacks while holding source gates.

Unique reservation is exactly
`UNIQUE(instance_id, repo_id, idempotency_key)`. Mode, plan/selection/request
digests are immutable comparison fields, **not** unique-index members. Logical
instance survives restart and is shared by cluster nodes. Canonical request
digest includes original requester, signed envelope, confirmations, reviews and
all effect parameters, not bearer credentials. Same key/different payload is
409 `idempotency_payload_conflict` even across modes or after TTL/revocation or
terminal status; same payload returns the original authorized operation only.
Authentication, current minimum scope and bounded parsing precede lookup.

New reservations atomically insert operation/items/C0/write-ahead audit; unique
conflict rereads the winner. Recheck winner even when concurrent admission fails.
Terminal/unknown operations never release keys; retain immutable tombstones.
No process mutex or SELECT-before-INSERT without a unique constraint suffices.

## Reverse PR/history and required recovery roots R

A complete index is keyed by immutable **head** repository as well as base PR
repository/identity. It covers AGS and provider, all PR states, both head/base
references, cross-fork and cross-base creation/reopen/retarget, jobs, attempts,
merge/action intents, rewrite backups, history and relevant comment/review roots.
Each mutation must participate in the reverse domain revision/gate with a
complete watermark. Listing pulls on only the target base repository, adding
head/base IDs to a snapshot, or initializing a new watermark is not proof of
historical coverage. Unknown permissions/pages/events/history is blocked.

`RefHistoryCoverage` carries the full R and PR/ref->OID mapping digest: F plus
historical head/base/merge-base/merge/squash/result/PR-ref OIDs, promised backups
and other roots under retention obligations. Deduplicate objects, not mappings.
Missing expected roots or unknown historical applicability is blocked, never an
unexplained null. Even unique=0 or closed PRs require complete history/R.

The future reverse membership store must use immutable head domain, base PR
identity, side, full ref and mutation sequence; history entries need PR identity,
logical ref/type, OID/object type and evidence ID. Append-only historical coverage
and mutation watermarks are independent from current membership. These are
logical schema requirements only; physical indexes/migrations are deferred to
S3/S6, with tracking_incomplete by default. Do not infer old history by backfill.

Before any delete, archive **every R root's full closure** into independent
immutable durable storage with atomic manifest publication, digest/size/durability,
ACL intersection and retention pins (not repo cascade or automatic TTL GC).
Verify in an empty offline repo without alternates/promisor/shallow/replace/graft:
OID/type, parent/tree/blob/tag closure, fsck and every PR/ref mapping. A bundle of
F passing fsck is insufficient if historical H or squash M is unreachable from F.
The provider archive tag anchors F only; it does not preserve unreachable H/M.

The future receiver transaction is create archive tag iff absent + delete head
iff F/ref revision/C/epoch/source-absence/PR/review gates hold. Existing same-F tag
without this effect's receipt is a conflict; with a receipt, inspect only. Git
may omit up-to-date tags from push commands, so `--atomic` alone is not a verify.
Without receiver atomic/fence/protection support, preserve the head and report
archived_not_retired. Retirement never authorizes legacy source cleanup.

## Trusted review subject and lifecycle

`RefRetirementReviewSubject` binds immutable source/provider identities, mode,
full ref/F/format, source absence and provider/reverse revisions, mapping/policy/
credential generations, planner/source/schema versions, full inventory,
authoritative baseline, ownership evidence/source, unique exact set/count,
all PR state/history/impact, full R/history/map and archive ACL/retention/effect.
It intentionally excludes plan ID, issuance/plan TTL and review IDs to prevent
approval recursion. Subject stays B; only verified C prefixes can explain
allowed own-effect revisions. Multi-retire semantic dependencies require split.

Reviewer comes from native authenticated immutable subject/issuer, must be a
human with current repo admin/owner **and** independent server-owned
`projection.ref.retirement.review`, with read/diagnosis access to all evidence.
Self-review is allowed only with that additional permission (not a claim of
two-person review). Stage ownership evidence requires admin/diagnosis only and
records an explicitly untrusted candidate digest; it cannot return approved ID.
Approve is a separate strict action requiring trusted signed current evidence,
expected subject digest and explicit decisions: ownership attestation when
needed, closed/merged PR impact when present, exact unique set when nonzero,
and archive/recovery/retention always. Client reviewer strings grant nothing.

Review records are immutable, one exact ref subject, default expiry one hour,
maximum 24 hours (server policy may tighten), no editable renewal. Authorized
reviewer or independent revoker appends revocation/audit and advances its domain.
Current reviewer permissions, disablement, expiry, revocation, policy and exact
subject are checked at new plan, admission and actual receiver commit, not from
cached TTL. Revocation-before-commit refuses that effect; commit-before-revoke
preserves the committed fact and stops remaining items. Reviews never override
open PRs, missing objects/history, ownership conflicts, protected refs or fences.

## S1: immediate safe diagnostic behavior

`gittransport.TransportError` carries a fixed `Failure` (category, reason,
evidence source, measured nullable HTTP/exit codes, stderr truncation, retry
advice). Context cancellation/deadline identity survives wrapping. Raw response,
argv/URL/path-bearing causes and stderr never cross this transport error. A
bounded streaming pattern scanner retains evidence after output truncation.
Priority is typed HTTP/protocol evidence, specific CAS/protection/403/lock,
auth/non-FF, generic hook/network, unknown. Hook-declined alone is **not** branch
protection, and old-SHA mismatch/reference-exists is **not** lock contention.
Legacy non-FF API type is retained for CAS, with the distinct diagnostic category.

Provider HTTP failures carry numeric status/method/fixed route-kind, not body,
URL or headers. Existing methods retain their return signatures and deliberate
404 absence/compatibility behavior; these old reads cannot certify new plan
absence. HTTP 403 differs from stderr-pattern 403 (`http_status=null`).

`ProjectionDiagnostic` is the shared allowlisted immediate JSON/slog DTO:
request or job/attempt, phase, validated head ref, actor versus authenticated
principal ID/login, transport, failure, timestamp and conservative effect state.
Job correlation is attached before the attempt and does not mutate claim/phase
ownership. No new DB columns/tables/JSON-in-summary storage or persisted identity
backfill. Existing status/notification APIs continue receiving stable type and
safe text; durable queryable actual-principal/job/ref diagnostics belong to S3.

For native HTTPS push, a bounded read-only `/api/v1/user` using the exact selected
token proves ID/login; configured TokenOwner/IntegrationBot/actor do not. Proof
is push-scoped, uncached, and not reused after credential replacement. For
managed SSH, read the selected existing public-key binding/title and owner ID;
only the selected identity with IdentitiesOnly/IdentityAgent=none can claim this
proof. Never substitute the HTTP admin owner. Unknown, injected or arbitrary
local/SSH transports emit null identity. Diagnostics never create keys or ACLs.
Legacy provisioning/writes and startup readiness are not changed by the probe;
unknown principal does not silently break or elevate legacy projection. No new
execute exists to admit unknown identity. Retry advice does not schedule a write.

## Verification and remaining gates

Hermetic tests use fake Git processes, real local Git, httptest loopback and
synthetic identities/secret sentinels, no deployed DB/provider credentials.
S0 tests explicitly show SHA CAS accepts ABA and an old dispatched worker can
commit after local claim replacement; these are counterexamples, not receiver
success tests. T12 covers wrapping/context, mixed/classification signals,
truncation/controls, typed HTTP, actual versus configured identity, SSH key
mismatch/credential rotation/unknown, safe errors/JSON/JSONL logs and no extra
writes/replay. Legacy job/status compatibility remains regression-tested.

S2 (read-only planner) is not implemented. S3 owns additive persistence,
all-writer admission, journal and durable diagnostics. S4/S5 own FF runners and
idempotency/batch acceptance. S6 owns trustworthy review/R/archive libraries.
S7 owns atomic retirement plus suppression of old bare-delete and uncorrelated
webhook/internal cleanup bypasses. S8 requires full regression, independent
review, exact-version real adapter partition/crash/revocation proof and separate
production approval. None is implied by S0/S1, green fixtures or existing
ordinary projection tests.
