# Forgejo Integration

AGS can optionally mirror selected post-push branch updates into Forgejo so Forgejo Pull Requests and Forgejo Actions can act as a configured deployment's CI surface while AGS remains its agent-facing Git remote. This is an optional product integration, not this fork's source workflow: this source repository uses GitHub and hosted CI. The examples below are synthetic and are never applied automatically.

## Responsibility split

- AGS is the source for agent/human branch ingress and commit authorship.
- Forgejo is the PR, Actions, runner, CI-log, and merge-execution projection. An authenticated AGS Human decides merge timing; provider CI may continue asynchronously and is required before final release/acceptance claims rather than every Human merge.
- The integration is one-way for ordinary branch ingress: AGS pushes are mirrored to Forgejo.
- Forgejo is a projection/CI surface, not a second origin. Operators and agents must not repair routine CI issues by pushing a Forgejo remote directly.
- Ordinary PR verification reads the exact provider PR and exact-head Actions runs through authenticated AGS `/pulls/{number}/provider/projection` and `/provider/ci/runs`; AGS owns the provider credential, revalidates delegated authority immediately before provider I/O, requires the observed PR number to match the stored binding, exhausts bounded Actions pagination, and emits a secret-safe correlation receipt/audit. Anonymous requests, client-selected provider repo/number mismatches, incomplete pagination, and late authority drift fail closed before a successful observation.
- The integration is best-effort post-push work. Forgejo failures must not reject the original AGS git push, but AGS records structured projection status so wrappers can detect and repair drift.

## Trigger model

Forgejo Actions only trigger from Forgejo repository events. A push to AGS is not visible to Forgejo until AGS mirrors the branch.

```text
git push AGS refs/heads/agent/demo
  -> AGS post-push ref diff
  -> Forgejo Integration pushes refs/heads/agent/demo to Forgejo
  -> optional Forgejo PR agent/demo -> main
  -> Forgejo pull_request / synchronize Actions run
  -> Forgejo Runner executes jobs
```

## Configuration

Prefer one file-backed integration config:

```env
AGS_INTEGRATIONS_CONFIG="/path/to/integrations.yaml"
```

Example:

```yaml
outbound:
  targets:
    department_official:
      type: feishu_webhook
      enabled: true
      name: "部门正式群"
      description: "部门正式群飞书机器人；用于 PR merge 等对外消息投递"
      webhook_url_file: "/path/to/feishu-department-webhook"
      timeout: "5s"
    frontend_ci_debug:
      type: feishu_webhook
      enabled: true
      name: "前端CI测试"
      description: "前端 CI / debug 相关飞书机器人；用于 projection drift / incident 等调试通知"
      webhook_url_file: "/path/to/feishu-debug-webhook"
      timeout: "5s"
  events:
    pull_request_merged:
      enabled: true
      targets: [department_official]
    projection_drift:
      enabled: true
      targets: [frontend_ci_debug]
    multica_incident:
      enabled: true
      targets: [frontend_ci_debug]
      throttle_window: "15m"
# Legacy notifications.targets/events remains accepted during migration and is
# internally mapped to outbound when outbound is not configured.
projection_watch:
  enabled: true
  # Local DB notification dispatch; this performs no provider reads.
  poll_interval: "1m"
  # Bounded refs + open-PR reconciliation.
  scan_interval: "15m"
  # Complete provider PR-history integrity audit.
  full_audit_interval: "24h"
  # Keep restart/readiness recovery clear of the first full provider audit.
  startup_audit_delay: "5m"
  refs: "all"
  ref_policy: "ags-authoritative"
  notify:
    on: ["drift", "projection_failure"]
    throttle_window: "15m"
    grace_period: "2m"
  repair:
    auto: false
    safe_repairs: []
forgejo:
  enabled: true
  base_url: "https://forgejo.example.test"
  token_file: "/path/to/forgejo-token"
  webhook_secret_file: "/path/to/forgejo-webhook-secret"
  default_owner: "example-team"
  private_repos: true
  auto_create_repo: true
  auto_pr: true
  default_base_branch: "main"
  # Large binary branch/PR projection push tuning. Omit to use defaults.
  push_git_config: ["core.compression=0", "pack.window=0"]
  # Optional per-git-push subprocess timeout; unset/0 inherits the worker timeout.
  push_timeout: "0s"
  # Required before production Forgejo workflow actions may report ready.
  authority_policy:
    enabled: true
    webhook_url: "https://ags.example.com/api/v3/integrations/forgejo/webhook"
    integration_bot: "ags-bot"
    # Separate admin-read credential; projection pushes still use forgejo.token_file.
    operator_token_file: "/path/to/forgejo-operator-token"
    # Exact Forgejo actor login -> immutable AGS numeric principal ID. There is
    # no same-login, provider-role, or repository-owner fallback.
    action_principal_bindings:
      example-maintainer: 7

  # Empty include/exclude lists admit every branch regardless of naming.
  mirror:
    include: []
    exclude: []

  pull_request:
    include: []
    exclude: []

  repos:
    "example-team/project-one":
      owner: "example-team"
      repo: "project-one"
      base_branch: "main"
      auto_pr: true
      # Server-owned allowlisted method bound into delegation v2 facts.
      delegated_merge_method: "rebase"
      # Repo-local only. Default remains off; do not enable globally.
      # fast_forward_ack: false
    "example-team/project-two":
      owner: "example-team"
      repo: "project-two"
      base_branch: "main"
      auto_pr: true
      delegated_merge_method: "fast-forward-only"
      fast_forward_ack: true

gitlab:
  enabled: true
  base_url: "https://gitlab.company.local"
  token_file: "/path/to/gitlab-token"
  merge_authority: "forgejo"
  # Empty include admits every branch. Exclude skips GitLab backup/projection
  # for short-lived duty refs. Trailing "/" is a prefix. Do not list main or
  # long-lived release branches here.
  mirror:
    include: []
    exclude:
      - "sync/upstream-resolve/"
      - "sync/upstream-resolve/*"
      - "sync/upstream-*"
  repos:
    "example-team/project-one":
      project_id: "12345"
      project_path: "example-backups/project-one"
      target_branch: "main"
      env_branches:
        uat: "uat"
      repo_flow_evidence:
        enabled: true
        store:
          type: "jsonl"
          path: "/var/lib/agent-git-service/repo-flow/evidence/example-team/project-one/events.jsonl"
          max_records: 500
github:
  enabled: true
  base_url: "https://api.github.com"
  remote_url: "git@github.com:example-backups/project-one.git"
  token_file: "/path/to/github-token"
  merge_authority: "forgejo"
  # Empty include admits every branch. Exclude skips GitHub backup/projection
  # for short-lived duty refs. Trailing "/" is a prefix. Do not list main or
  # long-lived release branches here.
  mirror:
    include: []
    exclude:
      - "sync/upstream-resolve/"
      - "sync/upstream-resolve/*"
      - "sync/upstream-*"
  repos:
    "example-team/project-one":
      owner: "example-backups"
      repo: "project-one"
      remote_url: "git@github.com:example-backups/project-one.git"
      target_branch: "main"
    "example-team/project-two":
      owner: "example-backups"
      repo: "project-two"
      remote_url: "git@github.com:example-backups/project-two.git"
      target_branch: "main"
execution_context:
  enabled: true
  connectors:
    - source_instance_id: "task-service-primary"
      adapter: "multica_current_execution_context_v1"
      # Exact runtime-facing aliases only. A caller hint never becomes egress.
      accepted_runtime_endpoints:
        - "https://multica.example.com"
      # Fixed operator-owned source read; task tokens are never stored here.
      egress_endpoint: "https://multica.example.com/api/integrations/current-execution-context"
      timeout: "5s"
      workspace_mappings:
        "11111111-1111-4111-8111-111111111111": "example-workspace"
multica:
  enabled: true
  # Immutable AGS consumer identity projected to Multica. Callers cannot select it.
  target_instance: "primary-authority"
  command: "/usr/local/bin/multica"
  profile: "ags-multica-projection"
  # Legacy/default workspace shorthand. Keep for compatibility.
  workspace: "example-workspace"
  workspace_id: "11111111-1111-4111-8111-111111111111"
  # Preferred explicit defaults for multi-workspace projection.
  default_workspace: "example-workspace"
  default_workspace_id: "11111111-1111-4111-8111-111111111111"
  # Provider-neutral External PR Integration on Multica. AGS is the first provider.
  server_url: "https://multica.example.com"
  external_pr_provider: "ags"
  # Purpose-bound external PR link token. Workload assertions are retired.
  link_token_audience: "external-pr-link"
  link_token_secret: "${MULTICA_EXTERNAL_PR_LINK_TOKEN_SECRET}"
  # The typed terminal dispatcher reads service_token_file only.
  completion_on_merge:
    enabled: true
    mode: "leaf_child_only"
  # Durable typed AGS -> Multica terminal handoff. The dispatcher accepts
  # only this secret file; no inline token, payload URL, or raw path is accepted.
  external_pr_delivery:
    enabled: true
    timeout: "15s"
  service_token_file: /run/secrets/multica-service-token
  # Legacy projection comments are intentionally disabled/ignored. AGS PR/CI/merge
  # projection is bookkeeping and must not wake Multica assignees through ordinary
  # issue comments; Multica owns status changes and parent-stage notifications.
  comment: false
  set_status_on_merge: false
  workspaces:
    example-workspace:
      workspace_id: "11111111-1111-4111-8111-111111111111"
    secondary-workspace:
      workspace_id: "22222222-2222-4222-8222-222222222222"
  repos:
    "example-team/another-project":
      workspace: "secondary-workspace"
      workspace_id: "22222222-2222-4222-8222-222222222222"
  failure_watch:
    enabled: true
    poll_interval: "1m"
    rolling_window_days: 30
```

Legacy `FORGEJO_INTEGRATION_*` environment variables remain supported for local bootstrapping, but production/team use should prefer `AGS_INTEGRATIONS_CONFIG`. Prefer token/secret files over inline token values so raw secrets do not enter process listings or committed files. `projection_watch.poll_interval` only dispatches already-persisted drift notifications from AGS storage and does not contact Forgejo. `scan_interval` runs bounded operational reconciliation over refs and currently open PRs; missing or stale bulk candidates require an exact provider read before drift is recorded. `full_audit_interval` is the only periodic path allowed to enumerate `state=all` provider PR history and recheck historical merge/authority invariants. The first full audit waits for `startup_audit_delay`, so service restart and readiness recovery do not compete with historical provider pagination. Defaults are `1m`, `15m`, `24h`, and `5m` respectively. `execution_context` contains only stable source routing/adaptation facts: never add a Multica PAT, task token, AGS credential, or dynamic caller-controlled egress to it. Runtime `MULTICA_SERVER_URL` is only an exact hint matched against `accepted_runtime_endpoints`; every source bearer is request-scoped at standalone intake or Access Grant issue/renew, redirects fail, and the resulting snapshot alone grants no AGS authority. See [execution-context intake](architecture/execution-context-intake.md) and [Canonical Actor and Access Grants](architecture/access-grants.md). For Multica External PR Integration, `link_token_secret` matches `MULTICA_EXTERNAL_PR_LINK_TOKEN_SECRET`; the legacy direct projection client may still read its compatibility `service_token` value, but the durable typed `multica.external_pr_delivery` path is file-only and accepts only `service_token_file` with owner-only regular-file permissions (0400 or 0600). Keep real values in local ignored files or secret files, not in committed YAML. AGS accepts only the exact `external-pr-link` Task-bound token shape; assertion `kid`/purpose shapes fail closed.

Large push timeout and retry policy is owned by the async projection worker, not by the REST/GraphQL request context that created the AGS PR:

- `forgejo.push_git_config` / `FORGEJO_INTEGRATION_PUSH_GIT_CONFIG` is passed to `git push` as repeated `git -c key=value` options. The default is `core.compression=0` and `pack.window=0`, which avoids spending CPU repacking PNG-like or already-compressed binary payloads. In YAML, set `push_git_config: []` to disable per-command overrides.
- `forgejo.push_timeout` / `FORGEJO_INTEGRATION_PUSH_TIMEOUT` bounds only the `git push` subprocess. The default is unset/`0`, so push inherits the worker attempt deadline.
- `FORGEJO_PROJECTION_WORKER_TIMEOUT` bounds a whole projection attempt; default `10m`.
- `FORGEJO_PROJECTION_WORKER_MAX_ATTEMPTS` controls retry count for retryable phases; default `3`.
- `FORGEJO_PROJECTION_WORKER_RETRY_DELAY` controls retry backoff after retryable failures; default `250ms`.

Logs and persisted projection errors must expose only phase, duration, changed-file count, aggregate head blob bytes, stable error type, and redacted summaries. They must not include credential-bearing remote URLs or changed file paths.

AGS must not publish ordinary Multica issue comments for PR/CI/merge projection updates. Those comments are user-visible discussion events in Multica and can wake the issue assignee/agent. Projection writes PR/CI/linkage bookkeeping metadata, but it must not write or replace the Issue-owned `external_pr_completion_policy`; callers predeclare `leaf_child_only` or `record_only` before linkage. On a verified terminal fact AGS commits the fact and typed Multica delivery together; the restarted outbound worker sends the existing closed `ExternalPRLinkRequest` wire. Merged facts use only `/api/integrations/external-pr/complete-from-merge`; closed-unmerged facts use only `/api/integrations/external-pr/links`. Multica applies the current Issue policy when deciding whether to transition the leaf child or notify a parent Stage. A successful HTTP response only acknowledges the projection handoff, never proves Issue `done`. `multica.external_pr_delivery` is not an authority to override per-Issue policy. The historical `completion_on_merge` setting remains a compatibility bound for the older direct projection client and does not make Issue lifecycle authority move into AGS.

Outbound routing is explicit: `outbound.targets` defines named Feishu notification targets and `outbound.events` maps notification event names to them. Those notification targets remain `type: feishu_webhook`; the Multica terminal handoff is not configured as Feishu. Set `multica.external_pr_delivery.enabled: true` with the existing server-owned `multica.target_instance`, `multica.server_url`, and owner-only `multica.service_token_file` to register the fixed `multica_external_pr` target. It persists a secret-free typed row, sends the exact existing `ExternalPRLinkRequest` JSON, and uses its deterministic idempotency key for retries. Lease expiry, bounded retry, `dead_letter`, target configuration, and worker state are observable through `/readyz` and `/api/v3/outbound/deliveries`; an HTTP 2xx is delivery acceptance, not Issue completion. Legacy `notifications.targets` / `notifications.events` remains accepted during migration and is mapped to Feishu outbound only when `outbound` is not configured.

New message/webhook-style external calls should use outbound delivery by default so timeout handling, structured error classification, idempotency, retry, dead letter, and delivery observability are consistent. Authority-moving operations such as Git ref pushes, merge operations, and branch deletes may reuse outbound-adjacent client helpers, but must not be blindly replayed by the generic outbound retry worker without operation-specific idempotency and safety contracts.

### Production readiness and repository onboarding

When signed Forgejo workflow actions are enabled, `/readyz` requires both an initialized `projection_drift` outbound dispatcher with at least one enabled target and a live-converged authority policy for every enabled explicit Forgejo repository mapping. Mappings with `enabled: false` are excluded from authority verification and periodic projection scans. Missing alerting, `allow_rebase_update=true`, missing base protection, a push whitelist that omits `authority_policy.integration_bot`, missing workflow labels, or a missing/inactive AGS webhook makes the runtime `not_ready`. Development/test may independently use `AGS_ALLOW_MISSING_PROJECTION_ALERTING=true` and `AGS_ALLOW_MISSING_FORGEJO_AUTHORITY_POLICY=true`; `/readyz` reports the corresponding check as `degraded` with `explicit_opt_out`, so absence is never mistaken for production readiness. Give production readiness probes at least 30 seconds: live policy inspection is intentionally bounded, but a shorter caller deadline can cancel a healthy multi-repository check.

Use the idempotent onboarding command before enabling workflow actions:

```bash
go run ./cmd/forgejo-authority -config /path/to/integrations.yaml -mode plan
go run ./cmd/forgejo-authority -config /path/to/integrations.yaml -mode apply
go run ./cmd/forgejo-authority -config /path/to/integrations.yaml -mode verify
```

Forgejo restricts branch-protection reads and reads of another collaborator's effective permission to repository administrators. Keep `authority_policy.operator_token_file` separate from `forgejo.token_file`: readiness and label-admission permission inspection use the former only for live provider reads, while all projection, label, status, and comment writes continue to use the least-privileged integration-bot token. The operator read proves only the provider permission fact returned for the explicitly bound actor; it never selects the AGS principal or authorizes an effect. For one-shot onboarding, `-operator-token-file /path/to/forgejo-operator-token` overrides only the policy credential and does not rewrite `integrations.yaml`; never use the integration-bot token as proof of repository-administration authority.

The command refuses to run without an enabled `projection_drift` target. `apply` disables Forgejo-native `allow_rebase_update`, grants the named integration bot write collaborator permission when missing, creates or updates base protection so only that bot can push, creates the AGS workflow labels, and creates or updates the signed webhook for `pull_request`, `issues`, and `delete`. It then re-reads provider state; repeated apply on a converged repository creates no duplicate labels, webhook, or protection.

Authority verification is not ref convergence verification. Before promotion, also inspect AGS projection status and require exact AGS/Forgejo base SHA and every policy-managed head SHA to match. A branch count is not evidence of convergence. The runtime rebase path still enforces exact preflight, force-with-lease, PR-head verification, and final convergence regardless of onboarding state.

Upgrade order is: configure and verify the Feishu `projection_drift` target; configure explicit repo mappings, integration bot, webhook and authority policy; run plan/apply/verify; confirm ref convergence; then enable workflow-action traffic. Roll back action traffic first. Do not re-enable native Forgejo rebase/update or weaken the base protection as a rollback mechanism.

## Repository mapping

`FORGEJO_INTEGRATION_REPO_MAP` is optional. If absent, AGS repo `owner/repo` maps to Forgejo `{FORGEJO_INTEGRATION_DEFAULT_OWNER}/repo` when a default owner is set, otherwise to `owner/repo`.

Example:

```json
{
  "example-team/project-one": {
    "owner": "example-team",
    "repo": "project-one",
    "enabled": true,
    "base_branch": "main",
    "auto_pr": true
  },
  "example-owner/experimental": {
    "enabled": false
  }
}
```

Per-repo `base_branch` and `auto_pr` override global defaults. `enabled: false` is a hard opt-out: the repository is not mirrored, onboarded, authority-verified, or included in periodic head/PR drift scans.

## Branch policy

Mirror and PR policies are intentionally separate:

- `FORGEJO_INTEGRATION_MIRROR_BRANCH_*` controls which AGS branches are pushed to Forgejo.
- `FORGEJO_INTEGRATION_PR_BRANCH_*` controls which mirrored AGS PR head branches may get a Forgejo PR projection after an AGS PR exists.
- Tags are ignored.
- Deleted branches that match the mirror policy are deleted from Forgejo with a delete refspec.
- Exclude lists win over include lists.
- Branch globs are slash-aware: `sync/*/*` matches exactly two segments after `sync/`; use explicit deeper patterns when needed.
- Empty include list means every non-excluded branch is eligible.
- Empty mirror and PR include/exclude lists are the explicit all-branch policy. Branch pushes mirror refs; they do not create PRs by themselves, and otherwise-invalid PR shapes remain rejected by PR validation.
- With a restricted non-empty policy, include every PR base branch in mirror policy so workflow/base updates reach Forgejo, and exclude branches that should never be eligible as PR heads.
- Every branch eligible for PR projection must also be mirror-eligible; AGS refuses to create/update a projected Forgejo PR when it cannot mirror the head branch first.

Legacy `FORGEJO_INTEGRATION_BRANCH_INCLUDE` and `FORGEJO_INTEGRATION_BRANCH_EXCLUDE` remain supported as a combined fallback when split policies are unset.


## Multica failure incidents
When `multica.failure_watch` is enabled, AGS polls Multica issue run history and accepts only failed runs that map back to an AGS repository through `metadata.ags_repo`.

Multica issue identity is workspace-scoped. Prefer PR markers and CLI arguments that include the workspace slug, for example `Multica: example-workspace/EX-66` or `Multica: secondary-workspace/SECOND-12`. Legacy `Multica: EX-66` remains supported only through the configured default/repo workspace. When `workspace_id` is configured, AGS passes `--workspace-id <uuid>` to workspace-sensitive Multica CLI commands instead of mutating profile state with `multica workspace switch`.

- Accepted failures are durable `external_events`; generated repo issues are incident projections, not raw event logs.
- Incidents aggregate by repo, Multica issue, agent, and failure reason; infra reasons aggregate by repo, reason, and runtime provider.
- The incident body is a rolling 30-day window.
- A successful Feishu delivery closes the generated AGS incident issue with `COMPLETED`; a throttled event closes it as already covered by the previous delivery.
- Feishu delivery failures leave the AGS incident issue open so operators can notice the broken notification path.
- Closed incident issues remain available for periodic agent analysis; analysis should query `external_events`, `repo_incidents`, and `source/multica` issues with `state=all`.

## Projection drift monitoring

`projection_watch` is a fallback monitor for projection drift/failure state. It does not make Forgejo a second origin and it does not auto-repair by default.

- `poll_interval` controls how often AGS actively compares policy-eligible AGS heads against Forgejo heads, reads all paginated Forgejo PRs, and scans active `projection_ref_states` rows. Disabled mappings are skipped. Forgejo API reads have a 60-second client bound: live observation showed that a 30-second bound could still occasionally cancel a variably slow later PR page in a large repository.
- The paginated PR list is only a discovery snapshot. Before persisting a mapped PR as missing, lifecycle-drifted, or head-drifted, the scanner re-reads that exact Forgejo PR number. A failed exact read aborts that scan instead of converting an incomplete or torn provider snapshot into a drift fact.
- The PR scan records exact-confirmed missing AGS→Forgejo projection, AGS/Forgejo lifecycle drift, open-head SHA drift, merged AGS commits no longer reachable from their base, Forgejo-only merged PRs with no AGS authority mapping, and Forgejo merge commits no longer reachable from AGS base. An unresolvable base branch becomes a per-PR `merged_content_missing` failure; it does not abort checks for the remaining PRs.
- A required `ags/action-rebase` Forgejo projection failure writes its active drift generation and one `projection_drift` outbound intent per configured target in the same DB transaction, then immediately attempts Feishu delivery after commit. The payload includes both PR identities, actor, action phase, branch, old/new/expected/actual SHA, correlation ID, and a safe recovery hint.
- Idempotency is keyed by provider/repo/ref, active generation, state (`active` or `resolved`), and target. Repeated labels within one unresolved incident reuse the delivery. Exact convergence emits a resolved delivery only when that generation previously enqueued an active alert; a watcher candidate that clears during `notify.grace_period` resolves silently. Recurrence increments the generation and clears the previous generation's notification throttle marker.
- Retryable Feishu failures enter `retry_wait`; exhausted failures enter `dead_letter`. Missing target or dispatcher configuration is returned as an explicit alerting outcome while the durable drift remains recorded.
- Persisted summaries and payload URLs are sanitized before insert; authenticated URLs, tokens, webhook secrets, and query credentials must not be stored.
- Enabled `outbound.events.projection_drift` also supports compact fallback alerts from the periodic watcher. Legacy `notifications.events.projection_drift` is accepted during migration.
- `notify.grace_period` is the persistence confirmation window for watcher-discovered candidates, and `notify.throttle_window` limits repeats after an active alert is enqueued. Candidates that converge before the grace period never produce a Feishu message, including a standalone `resolved` message. Required action failures are already exact operation failures and do not wait for the polling interval.
- `repair.auto` should remain `false` unless a future explicit safe-repair policy is reviewed. Current repair path is the operator wrapper `ags-team converge-projection --authority ags --apply`.
- If a Forgejo-merged PR source branch has already been deleted on Forgejo but still exists on AGS, the scanner records `source_branch_cleanup_pending`. This is not a request to restore Forgejo. Delete the stale AGS source branch or let the signed Forgejo `delete` webhook clean it up.
- Historical loss may be suppressed only by an audited disposition: label the Forgejo PR `ags/integrity-superseded` and include `[AGS-INTEGRITY-SUPERSEDED]` (legacy `Merge-Integrity-Superseded-By:` is also accepted) in its body. A label or body marker alone is insufficient. For a mapped lifecycle mismatch, this suppression is accepted only when the AGS PR is already closed and unmerged; it cannot hide an open AGS PR.

Alerts are generated from structured projection state, not journal scraping. The expected operator loop is:

1. `GET /api/v3/repos/{owner}/{repo}/projection/status` or `ags-team verify-repo --repo <owner/repo>`.
2. Inspect `type`, `ags_sha`, `forgejo_sha`, and `error_summary`.
3. Dry-run `ags-team converge-projection --repo <owner/repo> --ref <ref> --authority ags`.
4. Apply only when the plan converges Forgejo to the authoritative AGS SHA and creates backup refs first.

## Runtime behavior

For each eligible AGS branch update, AGS performs:

1. Optional Forgejo repo creation.
2. `git push` from the AGS bare repository to the mapped Forgejo repository when the branch matches the mirror policy.
3. Structured projection failure recording when Forgejo rejects the mirror push, for example `non_fast_forward_blocked` when a Forgejo ref drifted away from the authoritative AGS SHA.
4. No Forgejo PR is created from a raw branch push. Before AGS persists a new PR, mapped auto-PR repositories require its head to satisfy both mirror and PR policies; Forgejo PR ensure then runs only from that AGS PR path.

After a local AGS merge, auto-merge, or signed Forgejo merged-PR callback records an AGS PR as merged/closed, enabled `outbound.events.pull_request_merged` creates an idempotent `outbound_deliveries` row and routes a compact Feishu text notification to its configured targets. The message includes AGS URL, Forgejo PR URL, and GitLab shadow MR URL when those projections exist. Delivery failure updates the outbound delivery state and must not fail or roll back the merge. Retryable external failures such as Feishu `11232` frequency limiting are scheduled for backoff retry by the outbound worker. Legacy `notifications.events.pull_request_merged` remains accepted during migration and is mapped into outbound when no `outbound` section exists.

Outbound deliveries can be inspected and recovered through REST:

```text
GET  /api/v3/outbound/deliveries?event=pull_request_merged&repo=<owner/repo>&status=retry_wait
POST /api/v3/outbound/deliveries/{delivery_id}/retry
POST /api/v3/repos/{owner}/{repo}/pulls/{pull_number}/outbound/replay-merge
POST /api/v3/repos/{owner}/{repo}/pulls/{pull_number}/outbound/replay-merge?force=true
```

Default replay is idempotent and will not re-send a delivered event. Forced replay creates a new manual replay delivery and marks the message payload as manual replay. The outbound worker also performs bounded GC for terminal rows: delivered rows are retained for 14 days, dead-letter rows for 30 days, and non-terminal rows (`pending`, `retry_wait`, `delivering`) are not garbage-collected by the default policy.

After a signed Forgejo merged-PR callback, AGS resolves the exact base ref from line-oriented `ls-remote` output, fetches the object without moving AGS base, proves the current AGS base is its ancestor, and then advances the AGS ref with an atomic compare-and-swap. Warnings or unrelated refs cannot be mistaken for the requested base. A missing, deleted, or non-fast-forward Forgejo base is rejected before AGS PR state changes. After that authoritative fast-forward, GitLab and GitHub shadow close plus backup-branch push are best-effort downstream projections: failures are logged and surfaced through their projection channels, but they must not roll back the accepted merge. After GitLab `PushRef` succeeds, AGS advances open GitLab PR projection `last_synced_sha` to that SHA. After GitHub `PushRef` succeeds, AGS advances open GitHub PR projection `last_synced_sha` to that SHA. After shadow-MR close succeeds — including when GitLab already marked the MR merged by ancestry — AGS upserts the GitLab projection row as `closed` with the Forgejo merged SHA and never treats GitLab as merge authority. After GitHub shadow-PR close succeeds — including when GitHub already marked the PR merged by ancestry — AGS upserts the GitHub projection row as `closed` with the Forgejo merged SHA and never treats GitHub as merge authority.

Human `POST /pulls/{number}/provider/merge` and Access Grant `pr.merge` preflight compare sides independently: AGS head against AGS refs, Forgejo PR head against the mapped provider head, and `Forgejo PR.base.sha` against the live mapped Forgejo target ref `F`. They do not require `F == A`. Creation-time `BaseSHA` is historical diff metadata. HTTP 409 / unknown merge outcomes are observed through exact GET (`GET /pulls/{number}/provider/merge?expected_head_sha=` or Access Grant invocation GET); they never authorize a second provider merge POST. When a repository mapping sets `fast_forward_ack: true` and the method is `fast-forward-only`, a live `F == expected head` may acknowledge the AGS PR through `UpdateRefCAS` without POSTing Forgejo merge and without claiming the Forgejo PR is merged. If AGS `main` has already fast-forwarded past that head, acknowledgement keeps the advanced ref and does not rewind. Protected Git HTTP push to `main` remains rejected.

Projection status is exposed at:

```text
GET /api/v3/repos/{owner}/{repo}/projection/status
```

A healthy projection must converge to the invariant `AGS ref SHA == Forgejo ref SHA` for every ref covered by the mirror policy. If the status reports drift, use a controlled wrapper such as `ags-team converge-projection --repo <owner/repo> --ref <ref> --authority ags --apply`; do not run `git push forgejo` manually.

Forced AGS work-branch updates observed by the generic Git Smart HTTP post-push mirror path are mirrored with a forced refspec. The synchronous `ags/action-rebase` path is stricter: it accepts only an existing mapped non-base PR work branch whose AGS branch, AGS PR, Forgejo branch, Forgejo PR, and durable `last_synced_sha` agree at preflight, then pushes with `--force-with-lease=<exact-ref>:<accepted-old-sha>`. It never uses a `+refspec`, naked `--force`, or a lease inferred from a later remote read. Before mutating AGS it creates a `forgejo_action_rebase` generation in `pull_request_projection_jobs`. The row persists preflight and desired/observed SHA facts through `preflight`, `rebasing`, `projection_resume`, push/ref/PR verification, recording, retryable/terminal failure, `needs_rebase`, and `projected` phases. Only the same bound job generation may resume its durably recorded desired SHA through DB-CAS startup/manual retry; duplicate labels and duplicate webhooks cannot select or regenerate that state. In-process mutexes remain only an optimization. If Forgejo is still at the saved old head, that same generation may reuse the exact saved lease; if it is already at the desired head, it independently verifies the PR and records convergence; any third head is terminal drift. A new signed label or intent must instead match its own persisted expected AGS/Forgejo head and base facts and may not borrow an operator-inspected, interrupted, or pre-job legacy head as recovery. Exact live convergence is recorded only by its owning generation; unknown or newer heads fail closed. AGS bare repositories reject deletion and non-fast-forward rewrites of their symbolic-HEAD default branch; ordinary work heads remain rewriteable for PR rebase flows. The managed `pre-receive` dispatcher keeps this guard in `hooks/pre-receive.d/50-gh-server-authority`; an existing repo-local hook is preserved as `10-local-preserved` rather than overwritten during hook refresh. Git HTTP repository resolution refreshes this layout for existing repositories and fails closed if the guard cannot be installed. AGS branch deletes are mirrored with a delete refspec so Forgejo does not retain extra projection refs after AGS has removed the authoritative head. Raw branch pushes never create Forgejo PRs; AGS PR create/update remains the metadata authority and the only path that may ensure a Forgejo PR projection.

RepoFlow environment projection is event-driven from AGS Git pushes. When a pushed ref matches `refs/heads/env/<env>`, AGS immediately projects that exact SHA to the configured GitLab branch from `gitlab.repos.<repo>.env_branches.<env>`; if no env-specific mapping exists, the env name is used as the target branch. This keeps GitLab/deployment credentials on the AGS runtime host instead of every teammate machine. No polling is required.

The latest result is stored in AGS DB. If `gitlab.repos.<repo>.repo_flow_evidence.enabled` is true, AGS also writes bounded typed evidence to the configured store. The first supported store type is `jsonl`; the JSONL path belongs to the AGS runtime integration config, not member repositories. AGS exposes:

```text
GET  /api/v3/repos/{owner}/{repo}/repo-flow/env/{env}/status
GET  /api/v3/repos/{owner}/{repo}/repo-flow/env/{env}/history?limit=100
POST /api/v3/repos/{owner}/{repo}/repo-flow/env/{env}/retry

POST /api/v3/repos/{owner}/{repo}/repo-flow/evidence
GET  /api/v3/repos/{owner}/{repo}/repo-flow/evidence/status
GET  /api/v3/repos/{owner}/{repo}/repo-flow/evidence/history?env=<env>&limit=100
```

`retry` replays projection from the current `refs/heads/env/<env>` SHA. The generic evidence endpoint is for AGS-owned evidence ingestion; deployment projection evidence is generated by the GitLab integration itself.

For the preferred review entrypoint, create the AGS PR first, for example with `gh pr create` against the AGS API/remote. AGS PR creation or metadata update then best-effort ensures:

1. the Forgejo head branch ref is force-mirrored from the AGS bare repository;
2. the Forgejo PR for merge authority and CI visibility exists and has current title/body/base metadata;
3. the Forgejo PR head SHA matches the AGS PR head SHA before AGS records the projection as current;
4. a GitLab shadow branch and shadow MR for backup visibility;
5. a durable AGS projection mapping row that links `AGS PR -> Forgejo PR -> GitLab MR`.

Creation availability is independent from provider convergence. Once repository permission, branch/base validity, and `pr.create` authority pass, AGS persists and returns the authoritative AGS PR even when the head is not yet covered by Forgejo mirror/PR include policy, the provider is unavailable, or Multica association is incomplete. Those conditions produce missing/pending/retryable/terminal projection evidence; they do not delete or reject the AGS PR. Forgejo-facing readiness and every merge path remain blocked until an exact projection mapping and current head/base/verification evidence exist.

The AGS PR creation entrypoints enqueue durable Forgejo projection work instead of running Forgejo branch push / PR ensure in the request context:

```text
REST POST /api/v3/repos/{owner}/{repo}/pulls
  -> internal/rest/handlers_pr.go CreatePR
  -> service.CreatePR
  -> service.EnqueuePullRequestCreatedIntegrations(r.Context(), pr)
  -> upsert pull_request_projection_jobs(provider=forgejo, phase=queued)
  -> in-process worker with server-lifecycle context
  -> phase=pushing_ref -> forgejointegration.EnsurePullRequest -> pushAndVerify
  -> phase=recording_projection -> pull_request_projections final row
  -> phase=projected

GraphQL createPullRequest
  -> internal/graphql/gql_mut_pr.go doCreatePR
  -> service.CreatePR
  -> service.EnqueuePullRequestCreatedIntegrations(ctx, pr)
  -> same durable Forgejo projection job path
```

`pull_request_projection_jobs` is the resumable queue/phase row. It records provider, AGS PR/repo identity, head/base refs, head SHA, phase, attempt count, safe last-error type/summary, external PR locator once known, and next retry time. For generic PR projection, `attempt` is the exact durable generation: claim and manual retry advance it atomically, every native or delegated provider `BeforeWrite` reloads the same job/attempt in an allowed active phase, delegated writes additionally reload Session authority, and the worker context deadline is bound to the persisted lease expiry so it is not later than takeover. After provider return, AGS rechecks the generation; projection mapping upsert and the final `projected` CAS then commit in one DB transaction, so a stale final CAS rolls back the mapping. This narrows external TOCTOU but does not claim a cross-system transaction between AGS and Forgejo. Startup calls `ResumePendingForgejoProjectionJobs` so queued, retryable, or interrupted phases can be reloaded from DB. The final `pull_request_projections` row is still written only after Forgejo PR projection is known. Synchronous refresh callers receive a structured result for each provider. Forgejo is required for the rebase action; GitLab remains an optional shadow. A required Forgejo failure is returned to the action handler, while an optional GitLab failure remains observable without changing the Forgejo action result.

Projection status is exposed at:

```text
GET /api/v3/repos/{owner}/{repo}/projection/status
```

The response includes three operator surfaces:

- `refs`: active or resolved ref-level projection drift/events from push reconciliation; when a Forgejo mapping and provider repository readback are available, each Forgejo ref is enriched with the exact secret-free repository identity (`target_repo`, `target_id`, `html_url`, and `clone_url`);
- `jobs`: durable Forgejo PR projection jobs with `phase`, coarse `status`, `attempt`, `attempts` history, `last_error_type`, redacted `last_error`, `remote_ref`, `remote_sha`, `external_number`, `external_url`, `last_synced_ags_head_sha`, `next_run_at`, and `next_repair_action`; later success or retry must not erase a prior attempt's error; action-rebase rows additionally bind one exact internal `action_intent_id` and generation (not exposed as authority to clients), while historical unbound rows fail terminal;
- `events`: recent bounded ref projection events.

Provider repository readback does not change ref authority or persistence. For the mapped default branch, the response reconciles the durable row against the current AGS Git head and Forgejo branch head on every read; equal live heads report `resolved`, while disagreement reports active `sha_drift`. A stale or missing historical default-branch row cannot override current Git/provider facts. If either live read is unavailable, the endpoint returns the durable ref facts without inventing repository identity, so callers keep the projection pending rather than claiming a linked repository.

The job `status` field intentionally maps implementation phases into operator states:

```text
queued           phase=queued
running          phase=pushing_ref|verifying_ref|ensuring_pr|recording_projection
projected        phase=projected
retryable_failed phase=failed_retryable
terminal_failed  phase=failed_terminal
```

The safe repair path for a retryable failed Forgejo PR projection is:

```text
POST /api/v3/repos/{owner}/{repo}/projection/forgejo/pulls/{ags_pr_number}/retry
```

This endpoint requeues/resumes AGS-owned durable projection work. It does not create Forgejo PRs directly, does not treat Forgejo as the source of truth, and refuses terminal failures until an operator has inspected drift/non-fast-forward evidence. For CLI/operator use, call the same endpoint through the AGS profile wrapper, for example:

```bash
ags-api GET  /api/v3/repos/example-team/project-one/projection/status | jq '.jobs[]'
ags-api POST /api/v3/repos/example-team/project-one/projection/forgejo/pulls/2/retry
```

Never repair this state by manually creating a Forgejo PR. AGS PR state, branch, head SHA, title/body, and projection jobs remain authoritative; Forgejo is the merge/CI projection surface.

The regression harness for large or slow PR projection uses deterministic fake pushers rather than credentials or real remotes. The pusher records a simulated remote branch update and then returns evidence such as `git push: signal: killed`, `unknown_projection_error refs/heads/...`, or `cannot lock ref ... reference already exists`. This captures the old partial state where Forgejo has the branch but AGS has not completed PR ensure/projection-row recording, and verifies the async worker no longer inherits request cancellation.

Focused reproduction and observability commands:

```bash
go test ./internal/forgejointegration -run 'Test(EnsurePullRequest(RetriesKilledPushAfterRemoteRefCreatedAtExpectedSHA|CanceledRequestContextLeavesPartialRemoteRefUnresumed|RejectsDifferentRemoteSHAAfterKilledPush|LeaseRewrite.*)|GitPush.*ForceWithLease.*)' -count=1 -v
go test ./internal/service -run 'Test(FindPullRequestByForgejoProjectionDoesNotAssumeMatchingNumbers|DispatchPullRequestIntegrationsReturnsErrorAndLeavesPartialStateAfterForgejoBranchPush|DispatchPullRequestIntegrationsKeepsOptionalGitLabFailureSeparate|ForgejoActionRebaseLabel(RebasesAGSPRAndRefreshesProjection|RejectsConcurrentRemoteChangeWithLease|MarksNeedsRebaseWhenBaseMovesAfterProjection)|ForgejoActionRebaseConflictLeavesAGSAndForgejoHeadsUnchanged|ForgejoActionProjectionPresentationFailurePreservesDurableFailure|EnqueuedForgejoProjectionUsesWorkerContextAfterRequestCancel|ForgejoProjectionJobCanResumeFromQueuedDBState|ForgejoProjectionJobClassifiesRetryableAndTerminalFailures|GetProjectionStatusIncludesForgejoProjectionJobs|RetryForgejoProjectionRequeuesFailedRetryableJob)' -count=1 -v
go test ./internal/rest -run 'Test(CreatePR(DispatchesForgejoProjectionFromRESTEntryPoint|EnqueuesForgejoProjectionWhenRESTWorkerDisabled)|ProjectionRetryEndpointRequeuesFailedRetryableJob)' -count=1 -v
go test ./internal/graphql -run 'TestCreatePR(DispatchesForgejoProjectionFromGraphQLEntryPoint|EnqueuesForgejoProjectionWhenGraphQLWorkerDisabled)' -count=1 -v
```

Large-payload end-to-end validation uses `e2e/forgejo-large-pr-projection.sh` against an AGS server with Forgejo projection enabled:

```bash
AGS_E2E_TOKEN=... \
E2E_BASE_URL=http://127.0.0.1:6666 \
AGS_E2E_PAYLOAD_MB=38 \
AGS_E2E_RUNNER_LABEL='ags-go-ci:docker://local/ags-go-ci:2026-07' \
AGS_E2E_RUNNER_IMAGE_REPO=/srv/example/runner-images \
./e2e/forgejo-large-pr-projection.sh
```

The script creates an AGS PR with PNG-like binaries under `ppt-images/`, waits for async Forgejo projection, verifies the projection job's `last_synced_ags_head_sha` and `remote_sha` match the AGS head SHA, verifies an external Forgejo PR URL is recorded, and calls the retry endpoint after success to prove the repair path is idempotent. The runner label/image are recorded as validation baseline only; projection failures should be diagnosed from AGS projection status/jobs, not conflated with runner image or dependency failures. Future missing CI dependencies belong in the `runner-images` repo/worktree (`/srv/example/runner-images`), not in the projection repair path.
`POST /api/v3/repos/{owner}/{repo}/pulls` and the exact numbered `GET` expose `external_projections` plus the current generic Forgejo `projection_job`. The job is a presentation receipt for bounded clients: it includes `job_id`, phase/status, `attempt`, retained `attempts` history, safe failure fields, external locator and observed SHA facts. Zero Forgejo mapping rows with a live job are pending, not missing mapping. Success does not clear prior attempt errors from `last_error` or `attempts`. The receipt does not move provider authority into the request path or make PR creation synchronous. Clients may poll the exact numbered GET for a bounded interval, must prefer the Forgejo `external_number` over branch-name inference, and must keep the already-created AGS PR when the projection remains pending or fails. Merge remains fail-closed until an exact Forgejo mapping exists.

The push actor in Forgejo/GitLab is the configured token owner. Commit authors are preserved from Git history.

For an AGS PR linked to a delegated session, the projected Forgejo PR body also
contains a server-generated `Delegated workload` section with the immutable
Multica workspace/Agent/task/optional run/Issue/session snapshot and the
`human -> stable AGS principal` delegation chain. The canonical section is
bounded by `ags-delegated-workload:start/end` HTML comments and placed before
the user-authored AGS PR body. Reconciliation regenerates it from AGS authority;
it does not treat user-authored text as provenance.
The configured Forgejo token owner remains Forgejo's API actor and is never
relabeled as the workload. The section excludes bearer, credential
hash/fingerprint, assertion/JTI, and policy-snapshot material. Durable-profile
PR bodies do not receive this section.

For Forgejo merge-authority callbacks, AGS exposes:

```text
POST /api/v3/integrations/forgejo/webhook
```

Configure a Forgejo repository webhook for `pull_request` and `delete` events, and set the webhook secret to match `forgejo.webhook_secret_file`. When a signed `pull_request` payload has `action=closed` and `pull_request.merged=true`, AGS resolves the Forgejo PR through the projection mapping, applies the fast-forward/CAS base update described above, and marks the AGS PR merged/closed with that synchronized SHA. If GitLab backup is enabled with `merge_authority: forgejo`, AGS comments and closes the mapped GitLab shadow MR before pushing the same exact SHA to the mapped GitLab `target_branch`. Closing before the backup push prevents GitLab from auto-marking the shadow MR as merged by ancestry. GitHub backup is the same class: when `github.enabled` and `merge_authority: forgejo`, AGS comments and closes the mapped GitHub shadow PR before pushing the same exact SHA to the mapped GitHub backup branch. A successful GitHub `PushRef` advances open GitHub PR projection `last_synced_sha`. A successful shadow-PR close — including an ancestry-merged GitHub PR with no remaining open PR — upserts the GitHub projection row as `closed` with the Forgejo merged SHA. AGS never merges GitHub PRs and never records GitHub as merge authority. A signed closed-but-unmerged callback closes the mapped AGS PR as unmerged; it does not invent a merge fact.

The legacy delegated merge gateway and Multica delegation introspection/consume/effect protocol are retired. `POST .../actions/pr.merge` and its intent GET are absent and return `404`; no feature flag can re-enable them.

The canonical Access Grant merge adapter does not call Multica introspect/consume/effect storage. A workload Grant receives `pr.merge` only when the source Agent custom environment contains `AGS_ACCESS_ROLE=maintainer` or `AGS_ACCESS_ROLE=admin`; those values are intentionally equivalent and do not authorize any other privileged operation. AGS derives provider repository/base/method from its own mapping, reads the live AGS base head, requires the exact PR head to contain that current base, and matches the exact provider PR head/base, mergeability, newest exact-head CI and protected integration-bot authority. The PR creation-time `base_sha` remains historical metadata rather than a permanent merge gate, so a reviewed candidate can roll forward with monotonic `main`. Definitive pre-dispatch exact-fact drift stores and returns a terminal `provider_attempt=not_attempted` receipt available through invocation GET; otherwise AGS stores a cross-grant effect key and `dispatching + outcome_unknown` before one provider POST. Duplicate calls, renewed grants and invocation readback cannot repeat the write; locator collisions after dispatch do not return 409, and only exact provider GET evidence completes recovery. This route still uses the same server-only Forgejo credential and does not make the grant bearer a provider credential.

The same signed `pull_request` webhook treats AGS workflow action labels as delivery events, never authority. Two request adapters feed one durable kernel. Automation may first obtain `pr.rebase` authority constrained by exactly `pull_request_number`, `forgejo_pull_request_number`, `expected_head_sha`, and `expected_base_sha`, then call `POST /api/v3/repos/{owner}/{repo}/pulls/{number}/actions/pr.rebase` with an immutable idempotency key, the same exact AGS/Forgejo PR mapping and canonical lowercase 40-hex expected head/base SHAs, plus complete pre/post label sets. For the human Forgejo surface, adding `ags/action-rebase` to an open mapped PR asks the signed-webhook adapter to resolve the exact `authority_policy.action_principal_bindings` actor login to an immutable numeric AGS principal, verify that actor's live Forgejo write permission through the read-only authority-policy client when configured, load the current AGS base SHA and complete live label set, and call the same durable `pr.rebase` evaluator. The adapter records request source, actor, deterministic binding content revision, principal, authority snapshot, exact head/base/label transition, and a delivery-derived idempotency key before entering the existing rebase kernel; duplicate deliveries read the same terminal/nonterminal intent and never repeat the Git/provider effect. Both SHA JSON values must be non-null strings whose original bytes already match that form: AGS does not trim or case-normalize them, rejects leading/trailing whitespace, uppercase, null, and non-40 values before intent/provider writes, and preserves the accepted bytes exactly through the durable intent, POST/GET receipt, and startup recovery. Old ref/`exact_head`, mixed, missing, unknown, or tampered Session constraints fail closed; provider refs remain independent coordinates. AGS authorizes `pr.rebase` through the shared durable/delegated principal-session evaluator (canonical repo ∩ live write grant ∩ active policy class ∩ target default/exact exception), persists a secret-safe durable intent, and CASes `planned -> dispatching` before its integration credential can add `ags/action-rebase`. `dispatching` is an outcome-unknown, restart-retryable state: the webhook may consume exactly one matching unexpired `dispatching` or `dispatched` intent under the same per-PR lease even while the provider call is still returning. Provider success CASes only `dispatching -> dispatched`; a zero-row CAS must reload and preserve a legal webhook-advanced state, while provider error keeps `dispatching` for idempotent startup recovery rather than returning to `planned`. The webhook revalidates full labels, AGS/Forgejo mapping, head and base before rebase. The label adapter fails closed when delivery identity, actor binding, live Forgejo write permission, mapped projection, active immutable principal, shared AGS authorization, head/base, or complete labels are missing or drifted. It removes the action label and converges the idempotent status projection to `ags/status-blocked` without creating an intent/job or executing Git; cleanup/status failure is returned for retry. An event whose live action label is already absent and has no matching delivery intent is stale and is ignored, preventing delayed webhooks from regressing a completed request. Binding changes invalidate admitted label-originated durable authority at later write/recovery seams. Existing-intent fact, label, expiry, or delegated-Session drift is terminally denied without treating the invalid Session as cleanup authority. Webhook signature and provider role prove event transport/provider facts only; same-login coincidence, profile/name, label ownership, repository ownership, and `repo.admin` are not action authority inputs. `GET /api/v3/repos/{owner}/{repo}/pulls/{number}/actions/pr.rebase/{intent_id}` returns only a secret-safe receipt. Normal preflight requires the mapped AGS branch, AGS PR head, Forgejo branch, Forgejo PR head, webhook head, and durable `last_synced_sha` to agree; the AGS and Forgejo base heads must also agree. Only a configured mirror/PR-eligible, non-base, non-default, same-repository work branch may continue.

After AGS rebase, the synchronous required Forgejo projection uses a force-with-lease bound to that preflight old head. A concurrent Forgejo update fails the lease and is preserved. AGS independently polls the remote ref and exact mapped Forgejo PR head after push; only then may the projection row advance. The success branch re-reads the AGS branch, AGS PR, Forgejo branch, Forgejo PR, base heads, and projection row and requires exact full-SHA equality before resolving active drift, clearing failure labels, and publishing one comment containing both PR numbers and the verified SHA. A base move yields `needs_rebase`; rebase conflict leaves the old AGS/Forgejo heads unchanged; any other incomplete convergence yields `projection_failed`.

Required Forgejo failure stops the success branch. Before best-effort labels or comments, AGS appends a projection event and upserts one active ref state containing the desired AGS SHA, independently observed Forgejo SHA when available, stable failure type, target repo/ref, and redacted summary. Label/comment delivery failure cannot erase the durable event/state. GitLab and GitHub shadow failure is optional and does not fabricate a Forgejo failure. Immediate durable outbound, restart-safe partial-action resume, and one resolved notification are implemented; status labels and comments remain presentation, not workflow authority.

When Forgejo emits a signed `pull_request.closed` payload with `merged=true` but no AGS PR projection row exists, AGS uses a strict fallback only for repos explicitly mapped in the Forgejo integration config, only when the Forgejo repo matches the mapped target, the base branch matches the configured target base, and the head branch is both mirror-eligible and PR-eligible. In that fallback AGS syncs the Forgejo base branch SHA back into the AGS bare repo but does not invent an AGS PR fact, close Multica work, or push backup remotes.

When Forgejo later emits a signed branch `delete` payload for the PR source branch, AGS treats it as cleanup only. The handler resolves the merged AGS PR through the Forgejo projection row and source branch, deletes the matching AGS `refs/heads/<branch>` if it still exists, optionally deletes the mirrored GitLab and GitHub source branches, preserves `refs/pull/<ags-pr>/head`, and projects `source_branch_state=deleted` back to Multica. Branch deletion never creates the merge/completion fact; the `pull_request.merged=true` event remains the authority for AGS merge state and Multica issue completion.

## Non-goals

- No bidirectional AGS/Forgejo reconciliation. The reverse directions are limited to signed Forgejo merge-authority callbacks and explicit AGS workflow action-label callbacks; both mutate AGS through AGS-owned service logic.
- GitLab PR sync is backup/visibility-only. Post-push same-ref GitLab mirroring honors `gitlab.mirror` include/exclude; trailing `/` is a prefix, so short-lived duty refs such as `sync/upstream-resolve/` can be skipped without excluding `main` or long-lived release branches. RepoFlow `env/*` projection may push deployment branches, but AGS still does not merge GitLab MRs.
- GitHub PR sync is the same backup/visibility class as GitLab. Post-push same-ref GitHub mirroring honors `github.mirror` include/exclude. Forgejo remains merge authority; AGS creates and closes GitHub shadow PRs and never merges them.
- External Forgejo/GitLab comments are not imported into AGS. AGS comments/reviews remain the authoritative discussion facts.
- No Forgejo Runner management inside AGS.
- No CI execution inside AGS.
- No automatic merge decisions.
- No direct dependency on Forgejo Actions internals.

## Operational notes

- Register all compute machines as Forgejo Runners against one Forgejo server instead of running multiple Forgejo control planes for the same repo.
- Keep polling/sync audit tooling as a fallback if missing a post-push event would be costly.
- Treat Forgejo token permissions as narrowly as possible: repository create/write and PR create/update for mapped repositories.
