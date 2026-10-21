# Outbound projection attribution

[Forgejo integration](../forgejo-integration.md) owns projection behavior. This
page describes attribution, not repository authorization or a new identity issuer.
The same file-backed table is consumed by both provider adapters:

```yaml
attribution:
  instance: primary-a
  principals:
    alice: {forgejo: alice.remote, github: alice-gh}
    bob: {forgejo: bob.remote}
```

Keys are AGS `users.login`, never token names. Columns are independent explicit
provider logins. Do not infer them from commit email, display names or webhook
`action_principal_bindings` (which is a separate authority contract). No accounts,
invitations, personal tokens or email bindings are created. Changes take effect
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

- For a definitive 403 on non-merge writes, check the user's current effective
  permission as bot. On an explicit enabled `forgejo.repos` target, grant `write`
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
- No mapping means bot fallback, not projection rejection. Generated PR bodies and
  comments get one replaceable leading `AGS-Actor:` marker. Fallback state/label
  writes have no text field, so receive a best-effort generated attribution comment.
  User Git commits are untouched. Newly AGS-generated merge/squash commits get an
  `AGS-Actor: <login>` trailer; rebase/user commits are not rewritten for attribution.

The generic async PR projection job persists attribution user id/login/request id
and restores only a provenance context. It must not call `ContextWithUser` with
that stored value: the standing executor's authorization model is unchanged.
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

GitHub.com has no Sudo. Shadow body and close comment always carry `AGS-Actor:`
with the AGS identity and optional mapped GitHub login. Native opener/pusher stays
the configured token owner. User-to-server tokens are not implemented. GitHub App
manifest/webhook reception and GitLab projection are unchanged.

Forgejo and GitHub Git pushes still use their configured bot/server credentials.
A PR timeline `pull_push` entry may name the PR creator, while a push webhook's
`pusher` and Actions push trigger still name the bot. Do not treat that display
entry as proof that the Git transport impersonated the user. Compare the exact
SHA and audit provenance. No personal token is inserted into a Git remote.

## Diagnostics and rollback

Each outbound user write and Git push emits an info record with `ags_instance`,
`target`, `op`, `repo`, `target_repo`, `ags_pr`, `target_pr`, `ags_user_id`,
`ags_login`, `token_owner`, `sudo_as`, `attribution`, `grant_applied`, `status`,
`request_id`, and `fallback_reason`. Denied and retry attempts are separate rows;
`collaborator_grant` is a separate operation. Git status is 0/1. Request ids reuse
AGS request correlation when available, otherwise a UUID is created. The same
credential/body-free subset is stored as `projection.<op>` audit events; audit
failure is logged without changing an already-completed provider write.
No token, Authorization value, authenticated remote or user body belongs in logs.

Modes: `sudo`, `fallback_unmapped`, `fallback_forbidden`, `text_only`, `bot_only`.
Reasons: `unmapped`, `sudo_404`, `forbidden`, `grant_failed`.

To stop impersonation, clear `attribution.principals` and perform a separately
authorized restart. The empty map uses bot/text fallback and disables native
comment projection. Credential separation remains active independently. Revert
that code slice separately if restoring the former operator override is actually
required; do not rotate tokens to roll back attribution. New nullable/defaulted
job provenance columns can remain when downgrading the binary. Never remove or
downgrade collaborators automatically: review `collaborator_grant` audits first.
Existing commits and provider texts are not mass-rewritten on rollback.

## Verification

Use [verify-ags-attribution](../../.cursor/skills/verify-ags-attribution/SKILL.md)
and [the feature map](../../.cursor/skills/verify-ags-attribution/features/README.md).
It creates a new sandbox only, launches a job-owned AGS/TLS front in one foreground
process, exercises official gh, reads Forgejo APIs directly, and stops only owned
processes. Evidence survives cleanup. It never changes an installed AGS instance.
