---
name: verify-ags-attribution
description: Verify AGS outbound attribution using official gh, real Git and Forgejo API readback in an isolated SQLite AGS and newly created sandbox; use for Sudo, permission recovery and projection provenance changes.
---

# Verify AGS attribution

## Launch

Run from the repository root with Go from `go.mod`, Python 3, Git, OpenSSL and
official gh. Requires an explicitly authorized Forgejo test endpoint, a file with
the existing admin projection credential, and an explicitly authorized temporary non-operator Forgejo
login with no public keys. Do not print credentials or source a live runtime env file.

```bash
go build -buildvcs=true -o /tmp/ags-attribution-verify ./cmd/gh-server
python3 scripts/verify-attribution.py \
  --binary /tmp/ags-attribution-verify \
  --forgejo "$VERIFY_FORGEJO_URL" \
  --token-file "$VERIFY_FORGEJO_TOKEN_FILE" \
  --forgejo-login "$VERIFY_FORGEJO_LOGIN" \
  --ssh-url "$VERIFY_FORGEJO_SSH_URL" \
  --known-hosts-file "$VERIFY_FORGEJO_KNOWN_HOSTS" \
  --ags-login fixture \
  --output "$VERIFY_EVIDENCE_DIR"
```

The named inputs are explicit operator parameters, not product defaults. Enabled
mapping requires a trusted SSH endpoint and an independently verified known_hosts
file. The helper copies the operator token into its own private fixture directory;
managed keys never enter the installed AGS private directory.
`VERIFY_EVIDENCE_DIR` must be new. The command owns random loopback ports, fresh
SQLite/repos, an isolated gh home and TLS proxy. It runs foreground and tears
down in `finally`; do not use a background terminal or an installed AGS database.
The synthetic fixture bootstraps a repo, then drops its site-admin flag before
any Git/gh operation; the caller is a regular owner admitted by AGS. Use only the authorized temporary
provider account for this lifecycle verification; never edit real accounts. The AGS
login and Forgejo login intentionally need not match.

## Doctor

The script reads Forgejo `/api/v1/user` (must be the existing admin credential),
`/users/{login}` (must already exist) and the new sandbox's effective permission
(must be below write). Then `/readyz` and `/api/v3/user` must confirm the owned
AGS instance and synthetic caller. A doctor failure stops before gh writes.

## Drive

The executable helper is `scripts/verify-attribution.py`. It builds no hidden
client: real Git pushes go through AGS Smart HTTP; `gh pr create`, `gh pr comment`
and `gh pr close` use the isolated AGS TLS front. See [features](features/README.md)
for complete entry points and lower-level denial cases. Stop on three failures
without a new diagnostic; never repair the deployed service to make a test pass.

Also run the same helper with `--mapping absent` and `--mapping empty` using
new evidence directories. Both must retain bot PR/close actors, original body,
zero provider comments, no Sudo/grants, and no new attribution audit rows. The
formal `TestReviewPR46EmptyMappingIsBehaviorNoop` covers exact request counts.

In enabled mode the helper first removes AGS access in isolated fixture setup and
attempts official gh PR creation: AGS must deny it before the first Git projection, while Forgejo permission stays
none, no key or PR appears and no grant audit exists. Empty repositories can return
404 for the PR list before their first branch exists. After fixture access restoration,
the same gh path succeeds and Service projects only the required write ACL.
Forgejo permissions are derived AGS projection state, not extra user authority.
Unit tests separately revoke AGS write immediately before the ACL attempt and
assert unchanged denial/no bot fallback. No provenance/mapping-only grants.

`commit_email` is an optional existing-identity assertion. The fixture sets its
AGS email to the existing provider primary email and restarts only its own test
instance to validate that contract. Both author/committer account logins and
unchanged SHA must be read back. Never edit real user commits or email bindings.

For exact-source Forgejo CI, once the candidate is committed and pushed to its
public source fork, add `--source-sha <full-head-sha>` and
`--source-repo https://github.com/<owner>/<repo>.git`. The helper checks `go version -m` revision + dirty=false and records binary
SHA256; build with `-buildvcs=true` from that exact clean head. The sandbox workflow fetches
that exact SHA and runs the four attribution/integration package suites. This
supplements, not replaces, the source repository's complete hosted CI. Directly
read the sandbox's `/api/v1/repos/{owner}/{repo}/actions/runs` until the matching
head's runs finish; retain status, SHA, event and URL. Do not count a waiting or
wrong-head run as green. No production credentials are provided to workflow code.

## Evidence

`report.json` retains command results, caller, PR user, comment user, timeline
actors, exact head SHA, sandbox coordinates and cleanup. `server.log`,
`attribution.json` and `forgejo-ci-initial.json` retain bounded provenance and CI
observations. Preserve action + readback, not only the final PR URL. Unit tests
cover ambiguous mappings, read isolation, no-grant merge fallback, grant failure
and bounded retries. GitHub is text-only; never claim a native GitHub actor change. Enabled Forgejo verification strictly
requires both timeline push actors and exact-head Actions trigger_user to match
the mapped user. It checks repeated pushes reuse the same key id/private key,
recovers a deliberately deleted sandbox public key, then clears the fixture map
and restarts only the job-owned AGS to prove provider key revocation. This requires
a temporary sandbox account with no pre-existing public keys; never use a real
account for this lifecycle test.

## Cleanup

The helper terminates and waits only for the AGS PID it started and closes its
own TLS listener even after failure. It closes the successful sandbox PR but
retains the sandbox, grants, SQLite, checkout and evidence for review. A failed
run may leave a sandbox PR open; use its recorded URL for explicit follow-up,
not mass cleanup. Do not delete evidence, kill by process name or remove accounts.
Check that `report.json` still exists after cleanup.

## Helpers

`python3 scripts/verify-attribution.py --help` lists all arguments. Local targeted
checks are `go test ./internal/attribution ./internal/forgejointegration
./internal/githubintegration ./internal/integrations -count=1` and
`go test ./internal/service ./server -run 'Test(ProjectionAttribution|ForgejoAdminTokenWiring)' -count=1`.
Keep this procedure and its feature map current using `/maintain-verification-skill`
when the driving paths change.

## Optional temporary-account evaluation (separate authorization required)

```bash
python3 scripts/verify-projection-account.py \
  --forgejo "$VERIFY_FORGEJO_URL" --token-file "$VERIFY_FORGEJO_TOKEN_FILE" \
  --repo "$VERIFY_SANDBOX_REPO" --pr 1 --commit "$VERIFY_COMMIT_SHA" \
  --mapped-login "$VERIFY_FORGEJO_LOGIN" --output "$VERIFY_ACCOUNT_EVIDENCE_DIR"
```

Use only a previously created sandbox and a new evidence directory. This helper
creates a new temporary account, checks real commit-page links with its read-only
sandbox access, checks fresh web login then prohibit_login+admin Sudo, and retains
the account with prohibition enabled. This is NOT normal verification or product
provisioning; never pass an existing real user as a login-state mutation target.
No password/token is recorded. Record failures and test account cleanup status;
production prohibit-login policy is not implemented by this experiment.
