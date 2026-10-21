# Outbound projection attribution

[Forgejo integration](../forgejo-integration.md) owns projection behavior. This
page describes identity and permission projection, not a new identity issuer.
Humans and Agents use AGS only; Forgejo is backing storage/projection, hidden
behind AGS. Its projection accounts and repository ACLs derive from AGS authority,
not a separately designed user-facing permission model. The admin integration
credential remains unchanged.
The same file-backed table is consumed by both provider adapters:

```yaml
attribution:
  instance: primary-a
  principals:
    alice: {forgejo: alice.remote, github: alice-gh, commit_email: alice@example.test}
    bob: {forgejo: bob.remote}
```

Keys are AGS `users.login`, never token names. Columns are independent explicit
provider logins. Do not infer them from commit email, display names or webhook
`action_principal_bindings` (which is a separate authority contract). No accounts,
invitations, personal tokens or email bindings are created. AGS manages one
long-lived SSH key per mapped Forgejo identity for Git writes. Changes take effect
on the next separately authorized restart. Case-insensitive duplicate Forgejo
logins, invalid login/header characters and unknown attribution YAML keys reject
startup. When configured, startup reads `/user` and mapped `/users/{login}` with
the existing projection admin token. Missing configured accounts reject startup;
omit that target to select fallback. Users disappearing after startup fall back
per request. GitHub only resolves the credential owner; its column is a text label,
not a verified OAuth grant or a change to the REST actor.

## Forgejo writes

Only create/edit/retarget/close/reopen PR, merge, comment and issue-label mutations
send `Sudo: <mapped login>`. Every read and administrative operation omits Sudo.
The admin token stays unchanged; Forgejo checks the sudo user's repo permissions.

Before any attributed non-merge write, AGS Service checks the exact actor id/login,
repository, current account/repo state and current native write permission using
uncached, non-backfilling reads. Delegated operations additionally use the existing
operation/capability/constraint evaluator; recovered jobs preserve the session id
for this check without treating stored provenance as admission. A mapping or
nonzero actor id cannot supply the decision. The same check runs immediately
before an ACL projection and each retry. AGS denial is returned unchanged, with
no Sudo, bot replay, or permission grant. Merge retains its existing admission,
protection and fallback behavior.


- For a definitive 403 on non-merge writes, check the user's current effective
  permission as bot. Only after AGS admission, on an explicit enabled `forgejo.repos` target, project `write`
  if below write, log that mutation separately and retry Sudo once. Never downgrade
  maintain/admin/owner, pregrant all users, or grant on default-owner fallbacks.
- Forgejo can conceal a private repository denial with 404. After proving the
  mapped user still exists with an admin read, apply the same bounded recovery.
  A disappeared user is never provisioned. This is a deliberate extension of the
  403 rule to private-repository behavior.
- If the grant fails or the one Sudo retry is still denied, replay once as bot.
  Merge 403/405/404 falls back directly to bot, without any grant or protection
  change. Conflicts, server/transport errors and unknown write outcomes are not
  blindly replayed. Existing exact merge/PR readback remains in place.
- With a nonempty table, an unmapped identity means bot/text fallback, not projection rejection. Generated PR bodies and
  comments get one replaceable leading `AGS-Actor:` marker. Fallback state/label
  writes have no text field, so receive a best-effort generated attribution comment.
  User Git commits are untouched. Newly AGS-generated merge/squash commits get an
  `AGS-Actor: <login>` trailer; rebase/user commits are not rewritten for attribution.

The generic async PR projection job persists attribution user id/login/request id
and restores only provenance. It must not make the job actor an implicit
authorized user: Service freshly reloads and checks current AGS grants. For a
delegated check, only a freshly loaded principal can enter the existing evaluator.
Historical jobs without provenance remain system/bot-owned rather than guessing
that the PR author performed a later edit or push. Signed close/merge webhook
sender logins reverse-map to AGS logins; unknown senders remain `forgejo`.

To make native `gh pr comment` verifiable, attribution-enabled deployments also
best-effort project newly created native PR comments with an already established
exact Forgejo binding. This covers REST and GraphQL comment creation, not edits,
deletes, reviews or arbitrary issues. It requires the current author's existing
AGS write admission (and delegated revalidation where applicable). A failed
projection does not fail or duplicate the durable native comment. Comments made
before a binding exists are not backfilled. Clearing the map disables this path.

## GitHub and Git transport

GitHub.com has no Sudo. When the mapping is nonempty, shadow body and close comment carry `AGS-Actor:`
with the AGS identity and optional mapped GitHub login. Native opener/pusher stays
the configured token owner. User-to-server tokens are not implemented. GitHub App
manifest/webhook reception and GitLab projection are unchanged.

Mapped Forgejo Git pushes authenticate using an AGS-managed ed25519 user key,
so the native push actor is the mapped Forgejo user. GitHub Git pushes and
unmapped/empty-map Forgejo pushes retain the configured bot HTTP credential.
No personal provider token is required and user commit objects are unchanged.

Configure trusted routing explicitly alongside the identity table:

```yaml
forgejo:
  managed_ssh:
    url: ssh://git@forgejo.example.test:2222
    known_hosts_file: /etc/ags/private/forgejo_known_hosts
```

The known_hosts file must contain the independently verified provider host key
for that exact host/port. SSH uses strict host checking, no agent/password or
user SSH config, and never disables verification. A mapped push fails closed
if routing, trust, current AGS admission or SSH authentication fails; it never
replays as bot. Existing ref, forced work-branch and exact lease semantics are
preserved. Ref readback cannot turn a pre-dispatch authorization/key failure
into success merely because the remote already has the desired SHA.

Private keys have mode 0600 under `forgejo-managed-ssh/<provider-id>/` beside
`forgejo.authority_policy.operator_token_file`, or beside `forgejo.token_file`
when no operator file is configured. The directory is mode 0700. Each key is
registered by the existing admin API with title
`ags-managed:<attribution.instance>:<AGS login>`. The same private directory and
instance name must remain stable. Registration checks the actual public key
before each use, recovering a deleted provider key without rotating the private
key. A cross-process file lock serializes creation, registration, revocation
and push; a second process sharing the directory reuses the same key. Processes
with different private directories are separate instances and must use distinct
instance names. SSH user keys inherit that user's Git repository permissions;
they are neither repository-scoped nor expiring credentials.

AGS rechecks current authorization before key registration, a minimum write ACL
projection and the push itself. Only an explicit repository mapping can receive
an ACL projection. This does not weaken protected base branches: a mapped user
who cannot push a protected ref still receives a provider denial.

At startup, reconciliation revokes locally recorded public keys whose identity
mapping disappeared or changed, including when the principals table is cleared (retain the instance name). Other instance
names sharing the private directory are never reconciled by this instance. This is
the only empty-table lifecycle side effect when prior managed state exists;
a fresh absent/empty table makes zero key API requests. Configuration changes
still require the normal separately authorized restart. Reconciliation failure
rejects startup, and unknown key-creation outcomes are read back rather than
blindly replayed. Private files and credential-free metadata remain for review
and potential reuse; removing a mapping revokes the provider credential, not
historical commits, comments or collaborator ACLs.

## Diagnostics and rollback

Each outbound user write and Git push emits an info record with `ags_instance`,
`target`, `op`, `repo`, `target_repo`, `ags_pr`, `target_pr`, `ags_user_id`,
`ags_login`, `token_owner`, `sudo_as`, `attribution`, `grant_applied`, `status`,
`request_id`, and `fallback_reason`. `target_login` additionally names the exact
recipient of a collaborator grant, so rollback does not depend on today's map. Denied and retry attempts are separate rows;
`collaborator_grant` is a separate operation. Git status is 0/1. Request ids reuse
AGS request correlation when available, otherwise a UUID is created. The same
credential/body-free subset is stored as `projection.<op>` audit events; audit
failure is logged without changing an already-completed provider write.
No token, Authorization value, authenticated remote or user body belongs in logs.

Modes: `ssh` (Git; `target_login` is the authenticated mapped user), `sudo`, `fallback_unmapped`, `fallback_forbidden`, `text_only`, `bot_only`.
Reasons: `unmapped`, `sudo_404`, `forbidden`, `grant_failed`.

## Empty-table compatibility and rollback

Absent `attribution` or `principals: {}` is a complete opt-out. REST uses its
original payload and write count, no Sudo, grants, generated close comments, new
native comment projection, GitHub markers, merge trailers or attribution audits.
Even legacy operator-token precedence is preserved in this mode. Enabling a
nonempty map opts into projection-token separation and startup validation; clearing
the map and separately restarting restores the base credential choice too, after
revoking any previously managed public keys. Tokens
are not rotated or narrowed. Tests cover absent and explicit empty independently.

New defaulted provenance columns can remain when downgrading. Never remove or
downgrade provider collaborators automatically in an attribution rollback: they
are AGS-managed projection state, not an alternative authorization source. Review
`collaborator_grant` audits/current AGS grants before a separately authorized ACL
reconciliation. This change is on-demand write projection, not a full ACL revoke
controller. Existing commits/provider text are not mass-rewritten.

## Commit account linkage

Sudo changes REST actors, never a commit object. Forgejo associates commit author
and committer independently by their recorded email. Optional `commit_email`
asserts the intended pre-existing linkage: syntactically a plain email; Forgejo
primary email must match and, the AGS user must exist and AGS `users.email`
must match at startup. It is an assertion, not an instruction to edit identities,
create email bindings, rewrite pushed history or change a user's Git config.
A later user-specific Git author/committer email still determines that commit;
configuration alone cannot guarantee all user commits link to the mapped account.
Secondary provider emails may also link, but the optional assertion deliberately
checks primary email only. Omit it for existing deployments until linkage is
operator-verified rather than guessing from display names.

The verifier uses an isolated AGS fixture with the existing mapped provider email,
compares unchanged Git SHA/object identity, and reads both provider `author.login`
and `committer.login`. The optional account-evaluation helper reads the actual
commit page with a temporary sandbox viewer: identical author/committer may be
collapsed to one `/mapped-login` link by Forgejo.

## Prohibit-login evaluation boundary

`prohibit_login` is not applied by product code or normal verification. The
separate `verify-projection-account.py` helper is opt-in and creates only new
explicitly authorized temporary test accounts; it never changes existing users.
It verifies fresh web login before/after prohibition and separately calls admin
Sudo GET/comment, keeping credentials in memory. The result is an environment-
specific observation, not a promise across Forgejo versions. Retain the temporary
account with login prohibited and evidence for review; do not delete or modify
real users as cleanup. Any production account policy remains a separate decision.

## Verification

Use [verify-ags-attribution](../../.cursor/skills/verify-ags-attribution/SKILL.md)
and [the feature map](../../.cursor/skills/verify-ags-attribution/features/README.md).
It creates a new sandbox only, launches a job-owned AGS/TLS front in one foreground
process, exercises official gh, reads Forgejo APIs directly, and stops only owned
processes. Evidence survives cleanup. It never changes an installed AGS instance.
