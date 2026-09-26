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

Missing capabilities return unsupported. In particular the currently targeted
Forgejo API does not expose standard cancel/rerun or a workflow catalogue; those
mutations are not routed through private web endpoints, SQL or another backend.
For `gh run list` name lookup, an explicitly labelled catalogue of already
observed workflow identities is available, with state `unknown`; this is not a
list of enabled workflows.

Forgejo jobs are selected by exact run number, workflow and head from its bounded
ActionTask inventory. The default work budget is 100 pages of 50 tasks, with an
explicit `task_page_limit` (1–1000) and an independent request deadline. The total
count must stay stable, page IDs must not overlap, and the unique observed count
must match the reported total. If completeness cannot be established within the
budget, the response is unavailable, never an empty/successful job set. An operator
can tune observation work without changing resource identity; very large/high-churn
repositories still benefit from a future run-specific API or typed indexed bridge.
Older jobs are not silently dropped to meet the budget.

## Logs

When Forgejo does not provide public logs, its adapter may use the explicit typed
provider-log bridge. The bridge must match repository, task ID, run number, job
name, commit, ref, event and provider PR (when applicable). It never receives an
ordinary AGS token. Push and workflow_dispatch are distinguished from actual
provider-PR runs, including when AGS PR numbers differ from provider numbers.

Run ZIPs contain actual whole-job logs where step metadata is unavailable; no
successful steps are fabricated. Signed download redirects require an exact
configured HTTPS hostname and receive no provider authorization, cookies or AGS
credentials. Only one explicit GET is allowed. Archives are checked for path
traversal, file type, entry count and total expanded bytes (8 MiB), not merely
compressed size. HTTP work is time-bounded and mutation requests are never retried.

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
as provider PR numbers. Duplicate check names, malformed statuses, cross-run jobs,
outages and incomplete pagination are errors, not empty success.

`HEAD /repos/...` preserves GET visibility for stock gh discovery. No-CI PRs still
return a commit connection, with empty check contexts rather than a fictitious
missing commit. Draft creation and standard merge expected-head semantics are
handled by their ordinary AGS owners, not by the CI adapter.

## Verification and scope

`node scripts/gh-native-acceptance.mjs` builds this checkout and uses a hash-pinned,
unmodified official gh. It tests origin-only routing, draft/HEAD, native-no-CI,
GitHub Actions then Forgejo protocol fixtures, required checks, logs, old ID denial,
unsupported mutation, standard merge, run identity and revocation. All resources
are disposable in a loopback-only user/network namespace. Backend fixtures use the
published API shapes; they are not evidence of running real hosted CI jobs.

This is a standard-client slice, not complete GitHub Actions API equivalence.
Unsupported external artifacts/caches/settings/check-run REST resources fail
explicitly rather than using unrelated native state. Provider provisioning and a
production TLS/credential/log-bridge rollout are separate explicit operations.
