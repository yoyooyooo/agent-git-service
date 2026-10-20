# Independently selected CI backends

Git hosting, PR projection, CI execution and merge authority are separate concerns.
The default CI backend remains native AGS Actions; Forgejo is not an implicit
default. A repository may explicitly select `none` or a named backend. Selecting
CI never changes the repository's Git store, credentials, provider PR binding or
configured merge authority.

## Configuration

In the existing integrations file:

```yaml
ci:
  default_backend: native
  backends:
    build-farm:
      kind: forgejo
      url: https://forgejo.example.test
      token_file: private/ci-forgejo.token
      task_page_limit: 100
      log_bridge:
        url: https://logs.example.test
        token_file: private/ci-logs.token
    hosted-actions:
      kind: github-actions
      url: https://api.github.com
      token_file: private/ci-github.token
      # Exact hosts only, needed only when a log endpoint returns a signed URL.
      log_download_hosts: []
  repositories:
    team/service:
      backend: build-farm
      repository: ci/service
      required_checks: [unit, lint]
    team/another:
      backend: hosted-actions
      repository: ci/another
      required_checks: []
    team/documentation:
      backend: none
```

Token paths resolve relative to the integrations file. They must be bounded,
owner-only regular files. Ordinary AGS user credentials never leave AGS for a CI
provider or log bridge. HTTP requires explicit `allow_http: true`; production
should use verified TLS. CI fields are strictly decoded: a misspelled selector
cannot silently revert to another backend.

`required_checks: []` is a known empty set; omission is unknown for an external
backend. Current AGS branch protection requirements are unioned with configured
requirements and cannot be removed by changing CI backend. Unknown policy cannot
be reported as known-green `isRequired` evidence. `none` does not erase an already
configured branch protection requirement.

## Contract and implementations

`internal/cibackend` owns bounded Runs/Run/Jobs/Job/Logs/Workflows/Action contracts,
not Git storage or native AGS permissions. The service resolves a repository's
current backend once, authorizes access to the AGS repository, and translates its
observations to standard Actions routes and GraphQL checks.

GitHub Actions and Forgejo have distinct adapters. GitHub uses its published
workflow/run/job REST forms. Forgejo uses actual ActionRun and ActionTask shapes:
run ID, repository run number and task/job ID are distinct. It does not pretend
that `/actions/jobs` exists on a Forgejo API that does not provide it.

Some Forgejo versions expose the run collection without `GET actions/runs/{id}`.
On a 404/405/501 from that read, AGS locates the exact provider database ID in
bounded collection pages (the same `task_page_limit` budget). Repository run
numbers are never used as database IDs. Duplicate IDs, changed totals, incomplete
inventories and an exhausted budget return unavailable; authentication failures,
transport failures and other provider errors do not trigger this fallback.
Details, jobs and logs all use this lookup, so a successful `gh run list` does not
lead to a false missing-run error solely because the single-run route is absent.

Missing capabilities return unsupported. Forgejo cancel and workflow catalogue
management remain unsupported. Reruns use the public workflow dispatch API as
described below; no private web endpoints, SQL or another backend are used.
For `gh run list` name lookup, an explicitly labelled catalogue of already
observed workflow identities is available, with state `unknown`; this is not a
list of enabled workflows.

Forgejo runs are selected with the public `head_sha` filter. Forgejo jobs are then
selected by exact repository run number, workflow and head. The public task list
ignores `head_sha` and `run_number`, so the adapter reads that list from the newest
task. Forgejo 12 returns it `id DESC`, and repository run numbers increase with
those IDs, including retries of one run. After this run's tasks have been seen, the
scan stops at the first full page whose maximum run number is smaller. It does not
stop merely because the first page contained a match: a retry can place another
task of the same run on the next page.

The default work budget is 100 pages of 50 tasks, with an explicit
`task_page_limit` (1–1000) and an independent request deadline. While pages are
read, the total count must stay stable and page IDs must not overlap. Reaching the
end of the list still requires the unique observed count to match the reported
total. Stopping early because a lower run number was passed does not require
reading the older remainder, and does not report those older tasks as this run's
jobs. If neither the lower run number nor the end of the list is reached within
the budget, the response is unavailable, never an empty/successful job set. An
operator can tune observation work without changing resource identity. A repository
whose newest runs have more than the budget of newer tasks still needs a
run-specific task API or typed indexed bridge.

## Dispatching workflows with official gh

`POST /repos/{owner}/{repo}/actions/workflows/{workflow}/dispatches` forwards
an explicitly requested workflow dispatch to the selected external backend.
Repository write permission is required. A filename is accepted directly; numeric
IDs must resolve through the current repository/backend workflow identity map.
AGS resolves the requested branch, tag or commit to an immutable local source SHA
before one provider write. Missing refs return 422 and unknown/retired IDs return
404 without provider writes. A successful dispatch returns 204 and does not
create a native AGS workflow or invent a run ID. List runs at that exact SHA to
observe the provider result.

Forgejo accepts `.yml`/`.yaml` filenames and scalar inputs; boolean and numeric
inputs become strings as required by its public API. Nested inputs are rejected.
The provider validates workflow enablement and required inputs. It must expose
its public workflow dispatch API; otherwise the request remains unsupported.
An observed workflow identity is not proof that dispatch is enabled. Outages and
unknown write outcomes are errors, with no redirect following or automatic POST
replay. Read exact-head runs before deciding whether to retry.

```bash
gh workflow run ci.yml --ref "$SOURCE_REF" -f target=test
gh run list --commit "$HEAD_SHA" --json databaseId,headSha,event,status,conclusion
```

## Rerunning CI with official gh

The selected external backend supports these GitHub-shaped mutations:

- `POST /repos/{owner}/{repo}/actions/runs/{id}/rerun`
- `POST /repos/{owner}/{repo}/actions/runs/{id}/rerun-failed-jobs`
- `POST /repos/{owner}/{repo}/actions/jobs/{id}/rerun`

Successful requests return HTTP 201. Use the mapped AGS run/job ID reported by
`gh run list` or `gh run view`; unknown, foreign and retired IDs return 404.
Repository write permission is required (403). An unfinished run or a provider
state/input conflict returns 409. Provider denial remains 403; outages remain
errors. Mutations are sent once, without automatic replay or redirect following.
After an unknown result, inspect provider runs at the exact head before retrying.

GitHub Actions receives its native run, failed-jobs or job rerun request.
Forgejo 12 has no public retry route, but its public
`POST actions/workflows/{workflow}/dispatches` accepts a commit SHA. AGS dispatches
the original workflow filename with `ref` set to the original run's exact
`commit_sha`, never a moving branch. Forgejo creates a new run and job identity
with event `workflow_dispatch`; list runs again with `--commit SHA` to find it.
All three Forgejo rerun routes execute the **whole workflow**, including successful
jobs, and return `X-AGS-CI-Rerun-Scope: workflow`. Original trigger context and
custom dispatch inputs are not replayed; required inputs without defaults can
prevent dispatch. Unapproved or fork PR runs are denied rather than escalating
them to a trusted workflow dispatch. PR checks retain their existing preference
for actual PR events, so a successful dispatch does not erase a failed PR check.

```bash
gh run rerun "$RUN_ID"
gh run rerun "$RUN_ID" --failed
gh run rerun --job "$JOB_ID"
gh run list --commit "$HEAD_SHA" --json databaseId,headSha,event,status,conclusion
```

Native Actions keeps its separate native execution contract; these external
adapters never fall back to native state. See [command coverage](../operations/gh-command-parity.md).

## Logs

When Forgejo does not provide public logs, its adapter may use the explicit typed
provider-log bridge. The bridge must match repository, task ID, run number, job
name, commit, ref, event and provider PR (when applicable). It never receives an
ordinary AGS token. Push and workflow_dispatch are distinguished from actual
provider-PR runs, including when AGS PR numbers differ from provider numbers.
Forgejo stores a synchronized PR event as `pull_request_sync` while its public
Actions API reports `pull_request`. After matching the exact provider PR ref and
run/job/task commit SHAs, the bridge normalizes this known event alias in its
response. The CI adapter also normalizes `pull_request_sync` to `pull_request`
when the run has an exact provider PR identity, so standard Actions responses
and strict log receipt validation use the same event. Stored provenance is not rewritten; unknown `pull_request_*` events,
`pull_request_target`, wrong refs and wrong commits remain rejected. Upgrade the
host actually serving the log bridge as well as any affected CI adapter; updating
only the AGS client-facing primary cannot repair an older remote bridge.

Run ZIPs contain actual whole-job logs where step metadata is unavailable; no
successful steps are fabricated. Signed download redirects require an exact
configured HTTPS hostname and receive no provider authorization, cookies or AGS
credentials. Only one explicit GET is allowed. Archives are checked for path
traversal, file type, entry count and total expanded bytes (8 MiB), not merely
compressed size. HTTP work is time-bounded and mutation requests are never retried.

## Reading provider logs with official gh

Use the AGS checkout's ordinary GitHub CLI configuration. For PR runs, selecting
by the exact reviewed head avoids confusing a provider's `#PR` display branch
with the source branch:

```bash
HEAD_SHA="$(gh pr view "$PR" --json headRefOid --jq .headRefOid)"
gh run list --commit "$HEAD_SHA" --limit 20 \
  --json databaseId,number,headSha,status,conclusion
# RUN_ID is databaseId from the preceding AGS response, not the provider's
# display run number or global database ID. Select the intended attempt.
gh run view "$RUN_ID" --json databaseId,number,headSha,jobs
gh run view "$RUN_ID" --log-failed
gh run view "$RUN_ID" --log
# JOB_ID is a job databaseId from the same AGS run response.
gh run view --job "$JOB_ID" --log
```

No Forgejo login, provider token, alternate origin or additional PR client is
required. The server-side backend and log bridge must already be configured.
For direct REST reads, `gh api` has its own host selection. Use the authenticated
AGS API hostname explicitly when the shell's default host is GitHub:

```bash
gh api --hostname primary.example.test "repos/$REPO/actions/runs/$RUN_ID"
gh api --hostname primary.example.test "repos/$REPO/actions/runs/$RUN_ID/jobs"
gh api --hostname primary.example.test "repos/$REPO/actions/runs/$RUN_ID/logs" > logs.zip
```

A successful API read is not a successful CI result; keep the run head and
conclusion in the receipt. Missing provider step metadata may display as
`UNKNOWN STEP`; whole-job output remains the original log, not fabricated steps.

## Identity, completeness and switching

Public external run/job/workflow IDs use a stable `ci_resources` mapping above
2^52 and below 2^53, preserving exact JSON integers. Scope includes AGS repository,
backend name/kind/API origin, external repository and resource kind. Token rotation
does not change identity. Changing backend/origin/repository makes old IDs return
not-found rather than selecting a same-number object from the new provider.
The mapping is small metadata, not a log cache, and is deleted with its repository.

GraphQL checks select current PR head only and do not reuse a previous successful
attempt to hide a newer failure. Provider PR branches require a durable exact
projection with matching origin/repository/head; AGS PR numbers are never guessed
as provider PR numbers. The same job name in two workflow files is two checks, identified by workflow file plus job name. A repeated name inside one workflow, malformed statuses, cross-run jobs,
outages and incomplete pagination are errors, not empty success.

`HEAD /repos/...` preserves GET visibility for stock gh discovery. No-CI PRs still
return a commit connection, with empty check contexts rather than a fictitious
missing commit. Draft creation and standard merge expected-head semantics are
handled by their ordinary AGS owners, not by the CI adapter. Unset PR/check times
are JSON null, never an empty string or a fabricated year-one event.

Job-backed `GET check-runs/{id}` retains the exact repository/backend/job binding.
Structured check annotations are not currently projected by external adapters:
the optional annotations resource returns 404 with
`X-AGS-CI-Annotations: not-projected`, not a successful empty collection. Official
gh handles a missing optional annotation resource and still reports the actual
run outcome. Repository access failures and provider outages remain errors; they
are never converted to 404 for this purpose. Use the separately implemented job
logs for provider diagnostics. No synthetic file/line annotations are invented.

## Verification and scope

`node scripts/gh-native-acceptance.mjs` builds this checkout and uses a hash-pinned,
unmodified official gh. It tests origin-only routing, draft/HEAD, native-no-CI,
GitHub Actions then Forgejo protocol fixtures, required checks, logs, old ID denial,
rerun dispatch and unsupported cancel, standard merge, run identity and revocation. All resources
are disposable in a loopback-only user/network namespace. Backend fixtures use the
published API shapes; they are not evidence of running real hosted CI jobs.

This is a standard-client slice, not complete GitHub Actions API equivalence.
External adapters do not publish Actions artifacts. `GET` of the repository and
run artifact collections returns `{total_count:0, artifacts:[]}` so stock `gh run
view` can finish after jobs load. Artifact download stays unsupported. Commit
check-runs list the selected backend's jobs for that SHA, including a provider
branch label that is not the AGS head ref. Caches, settings and other unsupported
Actions operations still fail explicitly rather than using unrelated native state.
Provider provisioning and a production TLS/credential/log-bridge rollout are
separate explicit operations.
