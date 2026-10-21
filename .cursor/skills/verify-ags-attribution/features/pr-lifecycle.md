# PR lifecycle and comments

## Sub-features

Create/edit/retarget/close/reopen/merge, native PR comment creation, label writes,
and signed webhook reverse attribution.

## How to get to it (user POV)

Push a work branch to AGS. Use `gh pr create`, `gh pr comment`, `gh pr close` against
that AGS repository, not the provider URL. Merge is a separate authorization path.

## Driving it with Python + official gh

Run the skill helper. Its three gh commands and Forgejo PR/comments/timeline GETs
must show the existing mapped provider user, with the exact pushed head SHA.
`TestAttributionWritesAndReadIsolation` covers all listed REST write entry points;
`TestAttributionWebhookSenderRoundTrips` covers sender parsing and reverse lookup.
A lifecycle smoke alone is not merge execution acceptance.

## Gotchas

PR ids differ between AGS and Forgejo. Comments project only after an exact binding
exists and only with the existing AGS write admission; edits/deletes/reviews are
not backfilled. Never merge the implementation PR as part of verification.
