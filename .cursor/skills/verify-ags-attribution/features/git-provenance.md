# Git and GitHub provenance

## Sub-features

Bot-owned Smart HTTP projection, exact user commit preservation, shared GitHub
text mapping, generated merge trailers and durable async caller provenance.

## How to get to it (user POV)

Push to AGS and create the native PR. GitHub, when separately configured, receives
only a backup/shadow projection; it is never merge authority.

## Driving it with Python + official gh

The helper compares Git HEAD with Forgejo PR head and captures `push_ref` audit
with the AGS login and bot token owner. Direct Forgejo Actions push run readback
can identify `trigger_user`/webhook `pusher`. Do not infer Git identity from a PR
timeline `pull_push` entry. `TestGitHubShadowUsesSharedMappingAndDoesNotSudo` checks
both shadow-body and close-note text; `TestMarkerReplacementAndMergeTrailer`
checks replacement/trailers. `TestProjectionAttributionSurvivesEnqueueWithoutGrantingAuthority`
checks durable provenance and its separation from service authorization.

## Gotchas

No personal provider token, GitHub Sudo, user-to-server auth or rewritten user
commit is implemented. GitHub live writing is not part of the Forgejo sandbox
helper; report package tests separately from any future authorized live test.

The helper additionally uses the mapped account's existing primary email in its
isolated AGS/Git fixture, declares `commit_email` in enabled configuration, and
checks Forgejo author.login and committer.login plus raw commit emails/SHA. No
production user or commit is rewritten. The optional account evaluation helper
checks the real commit HTML: identical author/committer render as one account
link. It evaluates prohibit_login on newly created temporary accounts only;
normal validation does not create or disable provider users.
